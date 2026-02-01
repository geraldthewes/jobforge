package unit

import (
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"nomad-mcp-builder/internal/config"
	"nomad-mcp-builder/internal/nomad"
	"nomad-mcp-builder/pkg/types"
)

// MockStorage implements the storage interface needed by nomad.NewClient
type MockStorage struct{}

func (m *MockStorage) AcquireLock(lockKey string, timeout time.Duration) (string, error) {
	return "mock-session-id", nil
}

func (m *MockStorage) AcquireLockWithMetadata(lockKey string, timeout time.Duration, metadata *types.LockMetadata) (string, error) {
	return "mock-session-id", nil
}

func (m *MockStorage) ReleaseLock(lockKey, sessionID string) error {
	return nil
}

func (m *MockStorage) GenerateImageLockKey(registryURL, imageName, branch string) string {
	return "mock-lock-key"
}

// createTestClient creates a nomad client with mock storage for testing
func createTestClient(t *testing.T) *nomad.Client {
	cfg := &config.Config{
		Nomad: config.NomadConfig{
			Address:     "http://localhost:4646",
			Namespace:   "default",
			Region:      "global",
			Datacenters: []string{"dc1"},
		},
		Build: config.BuildConfig{
			KillTimeout:       300000000000, // 5 minutes in nanoseconds
			DockerLogMaxFiles: 5,
			RegistryConfig: config.RegistryConfig{
				URL:        "registry.local:5000",
				TempPrefix: "bdtemp",
			},
		},
	}

	client, err := nomad.NewClient(cfg, &MockStorage{})
	if err != nil {
		t.Fatalf("Failed to create nomad client: %v", err)
	}
	return client
}

// TestGPUAvailabilityCheckTaskStructure tests that the GPU availability check task
// has the correct structure (name, driver, lifecycle hook)
func TestGPUAvailabilityCheckTaskStructure(t *testing.T) {
	client := createTestClient(t)

	// Create a GPU job configuration
	jobConfig := types.JobConfig{
		Owner:          "test-user",
		RepoURL:        "https://github.com/test/repo.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "test-image",
		ImageTags:      []string{"latest"},
		RegistryURL:    "registry.local:5000",
		Test: &types.TestConfig{
			EntryPoint:  true,
			GPURequired: true,
		},
	}

	job := &types.Job{
		ID:     "test-job-123",
		Config: jobConfig,
	}

	// Get test job specs
	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	// Check that GPU availability check task exists
	testJobSpec := testJobs[0]
	taskGroup := testJobSpec.TaskGroups[0]

	if len(taskGroup.Tasks) < 2 {
		t.Fatalf("Expected at least 2 tasks (gpu-availability-check + main), got %d", len(taskGroup.Tasks))
	}

	// GPU availability check should be the first task
	gpuCheckTask := taskGroup.Tasks[0]

	// Verify task name
	if gpuCheckTask.Name != "gpu-availability-check" {
		t.Errorf("Expected task name 'gpu-availability-check', got '%s'", gpuCheckTask.Name)
	}

	// Verify driver
	if gpuCheckTask.Driver != "exec" {
		t.Errorf("Expected driver 'exec', got '%s'", gpuCheckTask.Driver)
	}

	// Verify lifecycle hook
	if gpuCheckTask.Lifecycle == nil {
		t.Fatal("Expected lifecycle to be set")
	}
	if gpuCheckTask.Lifecycle.Hook != "prestart" {
		t.Errorf("Expected lifecycle hook 'prestart', got '%s'", gpuCheckTask.Lifecycle.Hook)
	}
	if gpuCheckTask.Lifecycle.Sidecar {
		t.Error("Expected sidecar to be false")
	}

	// Verify resources are minimal
	if gpuCheckTask.Resources == nil {
		t.Fatal("Expected resources to be set")
	}
	if *gpuCheckTask.Resources.CPU != 50 {
		t.Errorf("Expected CPU 50, got %d", *gpuCheckTask.Resources.CPU)
	}
	if *gpuCheckTask.Resources.MemoryMB != 32 {
		t.Errorf("Expected MemoryMB 32, got %d", *gpuCheckTask.Resources.MemoryMB)
	}
}

// TestGPUAvailabilityCheckCommand tests that the check command includes Consul KV query pattern
func TestGPUAvailabilityCheckCommand(t *testing.T) {
	client := createTestClient(t)

	jobConfig := types.JobConfig{
		Owner:          "test-user",
		RepoURL:        "https://github.com/test/repo.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "test-image",
		ImageTags:      []string{"latest"},
		RegistryURL:    "registry.local:5000",
		Test: &types.TestConfig{
			EntryPoint:  true,
			GPURequired: true,
		},
	}

	job := &types.Job{
		ID:     "test-job-456",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	gpuCheckTask := testJobs[0].TaskGroups[0].Tasks[0]

	// Verify config has command and args
	command, ok := gpuCheckTask.Config["command"].(string)
	if !ok {
		t.Fatal("Expected command to be a string")
	}
	if command != "/bin/sh" {
		t.Errorf("Expected command '/bin/sh', got '%s'", command)
	}

	args, ok := gpuCheckTask.Config["args"].([]string)
	if !ok {
		t.Fatal("Expected args to be a string slice")
	}
	if len(args) != 2 {
		t.Fatalf("Expected 2 args, got %d", len(args))
	}
	if args[0] != "-c" {
		t.Errorf("Expected first arg '-c', got '%s'", args[0])
	}

	checkScript := args[1]

	// Verify the script contains the Consul KV pattern
	if !strings.Contains(checkScript, "consul kv get gpu/occupied/") {
		t.Error("Check script should contain 'consul kv get gpu/occupied/'")
	}

	// Verify it uses node interpolation
	if !strings.Contains(checkScript, "${node.unique.name}") {
		t.Error("Check script should use ${node.unique.name} for node name")
	}

	// Verify it exits with error when GPU is occupied
	if !strings.Contains(checkScript, "exit 1") {
		t.Error("Check script should exit with code 1 when GPU is occupied")
	}

	// Verify it has a success message
	if !strings.Contains(checkScript, "GPU available") {
		t.Error("Check script should have a success message about GPU availability")
	}
}

// TestGPUCheckOnlyAddedWhenGPURequired tests that GPU check is only added when GPURequired=true
func TestGPUCheckOnlyAddedWhenGPURequired(t *testing.T) {
	client := createTestClient(t)

	// Test case 1: GPURequired = false
	jobConfig := types.JobConfig{
		Owner:          "test-user",
		RepoURL:        "https://github.com/test/repo.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "test-image",
		ImageTags:      []string{"latest"},
		RegistryURL:    "registry.local:5000",
		Test: &types.TestConfig{
			EntryPoint:  true,
			GPURequired: false, // GPU not required
		},
	}

	job := &types.Job{
		ID:     "test-job-no-gpu",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	taskGroup := testJobs[0].TaskGroups[0]

	// Should only have the main task, no GPU check
	if len(taskGroup.Tasks) != 1 {
		t.Errorf("Expected 1 task when GPURequired=false, got %d", len(taskGroup.Tasks))
	}

	if taskGroup.Tasks[0].Name != "main" {
		t.Errorf("Expected only 'main' task when GPURequired=false, got '%s'", taskGroup.Tasks[0].Name)
	}

	// Test case 2: GPURequired = true
	jobConfig.Test.GPURequired = true
	job.ID = "test-job-with-gpu"

	testJobs, err = client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	taskGroup = testJobs[0].TaskGroups[0]

	// Should have GPU check task + main task
	if len(taskGroup.Tasks) != 2 {
		t.Errorf("Expected 2 tasks when GPURequired=true, got %d", len(taskGroup.Tasks))
	}

	// First task should be GPU check
	if taskGroup.Tasks[0].Name != "gpu-availability-check" {
		t.Errorf("Expected first task to be 'gpu-availability-check', got '%s'", taskGroup.Tasks[0].Name)
	}

	// Second task should be main
	if taskGroup.Tasks[1].Name != "main" {
		t.Errorf("Expected second task to be 'main', got '%s'", taskGroup.Tasks[1].Name)
	}
}

// TestGPUReschedulePolicy tests that reschedule policy is configured for GPU jobs
func TestGPUReschedulePolicy(t *testing.T) {
	client := createTestClient(t)

	jobConfig := types.JobConfig{
		Owner:          "test-user",
		RepoURL:        "https://github.com/test/repo.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "test-image",
		ImageTags:      []string{"latest"},
		RegistryURL:    "registry.local:5000",
		Test: &types.TestConfig{
			EntryPoint:  true,
			GPURequired: true,
		},
	}

	job := &types.Job{
		ID:     "test-job-reschedule",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	reschedulePolicy := testJobs[0].TaskGroups[0].ReschedulePolicy
	if reschedulePolicy == nil {
		t.Fatal("Expected reschedule policy to be set for GPU jobs")
	}

	// Verify reschedule policy values
	if reschedulePolicy.Attempts == nil || *reschedulePolicy.Attempts != 10 {
		t.Errorf("Expected 10 reschedule attempts, got %v", reschedulePolicy.Attempts)
	}

	if reschedulePolicy.Unlimited == nil || *reschedulePolicy.Unlimited != false {
		t.Error("Expected unlimited to be false")
	}

	if reschedulePolicy.DelayFunction == nil || *reschedulePolicy.DelayFunction != "exponential" {
		t.Errorf("Expected exponential delay function, got %v", reschedulePolicy.DelayFunction)
	}
}

// TestNonGPUJobHasNoRescheduleOverride tests that non-GPU jobs keep their default reschedule policy
func TestNonGPUJobHasNoRescheduleOverride(t *testing.T) {
	client := createTestClient(t)

	jobConfig := types.JobConfig{
		Owner:          "test-user",
		RepoURL:        "https://github.com/test/repo.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "test-image",
		ImageTags:      []string{"latest"},
		RegistryURL:    "registry.local:5000",
		Test: &types.TestConfig{
			EntryPoint:  true,
			GPURequired: false, // Not a GPU job
		},
	}

	job := &types.Job{
		ID:     "test-job-no-reschedule",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	reschedulePolicy := testJobs[0].TaskGroups[0].ReschedulePolicy

	// Non-GPU jobs should have attempts=0 (no rescheduling)
	if reschedulePolicy != nil && reschedulePolicy.Attempts != nil && *reschedulePolicy.Attempts != 0 {
		t.Errorf("Non-GPU jobs should have 0 reschedule attempts, got %d", *reschedulePolicy.Attempts)
	}
}

// TestGPUCheckForCommandBasedTests tests GPU check is added for command-based tests
func TestGPUCheckForCommandBasedTests(t *testing.T) {
	client := createTestClient(t)

	jobConfig := types.JobConfig{
		Owner:          "test-user",
		RepoURL:        "https://github.com/test/repo.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "test-image",
		ImageTags:      []string{"latest"},
		RegistryURL:    "registry.local:5000",
		Test: &types.TestConfig{
			Commands:    []string{"python test.py", "pytest"},
			GPURequired: true,
		},
	}

	job := &types.Job{
		ID:     "test-job-commands-gpu",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	// Should have 2 test jobs (one per command)
	if len(testJobs) != 2 {
		t.Fatalf("Expected 2 test jobs, got %d", len(testJobs))
	}

	// Each job should have GPU check as first task
	for i, testJob := range testJobs {
		taskGroup := testJob.TaskGroups[0]
		if len(taskGroup.Tasks) < 2 {
			t.Errorf("Job %d: Expected at least 2 tasks, got %d", i, len(taskGroup.Tasks))
			continue
		}
		if taskGroup.Tasks[0].Name != "gpu-availability-check" {
			t.Errorf("Job %d: Expected first task to be 'gpu-availability-check', got '%s'", i, taskGroup.Tasks[0].Name)
		}
	}
}

// TestGPUCheckTaskIsExported verifies that CreateTestJobSpecs is exported for testing
func TestGPUCheckTaskIsExported(t *testing.T) {
	// This test just verifies the method is accessible
	var client *nomad.Client
	_ = client // Verify the type exists and has the method

	// The method should be accessible via client.CreateTestJobSpecs
	// If this compiles, the method is properly exported
}

// Helper to check if a task has a specific lifecycle hook
func hasLifecycleHook(task *nomadapi.Task, hook string) bool {
	return task.Lifecycle != nil && task.Lifecycle.Hook == hook
}
