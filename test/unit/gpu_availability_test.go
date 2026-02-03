package unit

import (
	"testing"
	"time"

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

// TestGPUDedicatedExclusionConstraint tests that GPU jobs have the gpu-dedicated exclusion constraint
func TestGPUDedicatedExclusionConstraint(t *testing.T) {
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

	// Check constraints on the job
	testJobSpec := testJobs[0]
	constraints := testJobSpec.TaskGroups[0].Constraints

	// Should have both gpu-capable and gpu-dedicated exclusion constraints
	var hasGPUCapable, hasGPUDedicatedExclusion bool

	for _, c := range constraints {
		if c.LTarget == "${meta.gpu-capable}" && c.RTarget == "true" && c.Operand == "=" {
			hasGPUCapable = true
		}
		if c.LTarget == "${meta.gpu-dedicated}" && c.RTarget == "true" && c.Operand == "!=" {
			hasGPUDedicatedExclusion = true
		}
	}

	if !hasGPUCapable {
		t.Error("Expected gpu-capable constraint to be present")
	}

	if !hasGPUDedicatedExclusion {
		t.Error("Expected gpu-dedicated exclusion constraint (${meta.gpu-dedicated} != 'true') to be present")
	}
}

// TestGPUNodePinConstraint tests that node pinning works instead of gpu-dedicated exclusion
func TestGPUNodePinConstraint(t *testing.T) {
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
			GPUNodePin:  "gpu005", // Pin to specific node
		},
	}

	job := &types.Job{
		ID:     "test-job-node-pin",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	// Check constraints on the job
	testJobSpec := testJobs[0]
	constraints := testJobSpec.TaskGroups[0].Constraints

	// Should have node pin constraint, NOT gpu-dedicated exclusion
	var hasNodePin, hasGPUDedicatedExclusion bool
	var nodePinValue string

	for _, c := range constraints {
		if c.LTarget == "${node.unique.name}" && c.Operand == "=" {
			hasNodePin = true
			nodePinValue = c.RTarget
		}
		if c.LTarget == "${meta.gpu-dedicated}" && c.Operand == "!=" {
			hasGPUDedicatedExclusion = true
		}
	}

	if !hasNodePin {
		t.Error("Expected node pin constraint (${node.unique.name} = 'gpu005') to be present")
	}

	if nodePinValue != "gpu005" {
		t.Errorf("Expected node pin value 'gpu005', got '%s'", nodePinValue)
	}

	if hasGPUDedicatedExclusion {
		t.Error("Expected gpu-dedicated exclusion constraint NOT to be present when node pin is set")
	}
}

// TestNoGPUCheckTaskForGPUJobs tests that GPU jobs do NOT have a prestart gpu-availability-check task
// (This is the new behavior - we use constraints instead)
func TestNoGPUCheckTaskForGPUJobs(t *testing.T) {
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
		ID:     "test-job-no-prestart",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	// Should only have the main task, no gpu-availability-check prestart task
	taskGroup := testJobs[0].TaskGroups[0]

	if len(taskGroup.Tasks) != 1 {
		t.Errorf("Expected exactly 1 task (main), got %d", len(taskGroup.Tasks))
	}

	if taskGroup.Tasks[0].Name != "main" {
		t.Errorf("Expected task name 'main', got '%s'", taskGroup.Tasks[0].Name)
	}

	// Verify no gpu-availability-check task exists
	for _, task := range taskGroup.Tasks {
		if task.Name == "gpu-availability-check" {
			t.Error("Expected no 'gpu-availability-check' task - we now use constraints instead")
		}
	}
}

// TestNonGPUJobHasNoGPUConstraints tests that non-GPU jobs don't have GPU constraints
func TestNonGPUJobHasNoGPUConstraints(t *testing.T) {
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
		ID:     "test-job-no-gpu",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	constraints := testJobs[0].TaskGroups[0].Constraints

	// Should have NO GPU-related constraints
	for _, c := range constraints {
		if c.LTarget == "${meta.gpu-capable}" {
			t.Error("Non-GPU jobs should not have gpu-capable constraint")
		}
		if c.LTarget == "${meta.gpu-dedicated}" {
			t.Error("Non-GPU jobs should not have gpu-dedicated constraint")
		}
	}
}

// TestGPUJobDefaultReschedulePolicy tests that GPU jobs have the default (no retry) reschedule policy
// (We no longer use an aggressive reschedule policy since we use constraints now)
func TestGPUJobDefaultReschedulePolicy(t *testing.T) {
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

	// GPU jobs should now have the default reschedule policy (no retries)
	// not the aggressive 10-attempt policy we used with prestart checks
	if reschedulePolicy != nil && reschedulePolicy.Attempts != nil && *reschedulePolicy.Attempts != 0 {
		t.Errorf("GPU jobs should have 0 reschedule attempts (default), got %d", *reschedulePolicy.Attempts)
	}
}

// TestGPUComputeCapabilityConstraint tests that GPU compute capability constraint is added when specified
func TestGPUComputeCapabilityConstraint(t *testing.T) {
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
			EntryPoint:           true,
			GPURequired:          true,
			GPUComputeCapability: "7.5", // Turing or newer
		},
	}

	job := &types.Job{
		ID:     "test-job-compute-cap",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	// Check constraints on the job
	testJobSpec := testJobs[0]
	constraints := testJobSpec.TaskGroups[0].Constraints

	// Should have gpu-capable, gpu-dedicated exclusion, AND gpu_compute_capability constraints
	var hasGPUCapable, hasGPUDedicated, hasComputeCapability bool
	var computeCapValue, computeCapOperand string

	for _, c := range constraints {
		if c.LTarget == "${meta.gpu-capable}" && c.RTarget == "true" && c.Operand == "=" {
			hasGPUCapable = true
		}
		if c.LTarget == "${meta.gpu-dedicated}" && c.RTarget == "true" && c.Operand == "!=" {
			hasGPUDedicated = true
		}
		if c.LTarget == "${meta.gpu_compute_capability}" {
			hasComputeCapability = true
			computeCapValue = c.RTarget
			computeCapOperand = c.Operand
		}
	}

	if !hasGPUCapable {
		t.Error("Expected gpu-capable constraint to be present")
	}

	if !hasGPUDedicated {
		t.Error("Expected gpu-dedicated exclusion constraint to be present")
	}

	if !hasComputeCapability {
		t.Error("Expected gpu_compute_capability constraint to be present")
	}

	if computeCapValue != "7.5" {
		t.Errorf("Expected gpu_compute_capability value '7.5', got '%s'", computeCapValue)
	}

	if computeCapOperand != ">=" {
		t.Errorf("Expected gpu_compute_capability operand '>=', got '%s'", computeCapOperand)
	}
}

// TestGPUComputeCapabilityNotAddedWhenEmpty tests that no constraint is added when GPUComputeCapability is empty
func TestGPUComputeCapabilityNotAddedWhenEmpty(t *testing.T) {
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
			EntryPoint:           true,
			GPURequired:          true,
			GPUComputeCapability: "", // Empty - should not add constraint
		},
	}

	job := &types.Job{
		ID:     "test-job-no-compute-cap",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	// Check constraints on the job
	testJobSpec := testJobs[0]
	constraints := testJobSpec.TaskGroups[0].Constraints

	// Should have gpu-capable and gpu-dedicated exclusion but NOT gpu_compute_capability constraint
	var hasGPUCapable, hasGPUDedicated, hasComputeCapability bool

	for _, c := range constraints {
		if c.LTarget == "${meta.gpu-capable}" && c.RTarget == "true" && c.Operand == "=" {
			hasGPUCapable = true
		}
		if c.LTarget == "${meta.gpu-dedicated}" && c.RTarget == "true" && c.Operand == "!=" {
			hasGPUDedicated = true
		}
		if c.LTarget == "${meta.gpu_compute_capability}" {
			hasComputeCapability = true
		}
	}

	if !hasGPUCapable {
		t.Error("Expected gpu-capable constraint to be present")
	}

	if !hasGPUDedicated {
		t.Error("Expected gpu-dedicated exclusion constraint to be present")
	}

	if hasComputeCapability {
		t.Error("Expected gpu_compute_capability constraint NOT to be present when GPUComputeCapability is empty")
	}
}

// TestGPUComputeCapabilityWithoutGPURequired tests that compute capability constraint works independently
func TestGPUComputeCapabilityWithoutGPURequired(t *testing.T) {
	client := createTestClient(t)

	// Test case where only compute capability is set (not GPURequired)
	// This could be useful for jobs that need a specific architecture but don't need the nvidia runtime
	jobConfig := types.JobConfig{
		Owner:          "test-user",
		RepoURL:        "https://github.com/test/repo.git",
		GitRef:         "main",
		DockerfilePath: "Dockerfile",
		ImageName:      "test-image",
		ImageTags:      []string{"latest"},
		RegistryURL:    "registry.local:5000",
		Test: &types.TestConfig{
			EntryPoint:           true,
			GPURequired:          false, // Not requiring GPU runtime
			GPUComputeCapability: "8.6", // But specifying compute capability
		},
	}

	job := &types.Job{
		ID:     "test-job-compute-cap-only",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	// Check constraints on the job
	testJobSpec := testJobs[0]
	constraints := testJobSpec.TaskGroups[0].Constraints

	// Should have gpu_compute_capability but NOT gpu-capable or gpu-dedicated constraints
	var hasGPUCapable, hasGPUDedicated, hasComputeCapability bool
	var computeCapValue string

	for _, c := range constraints {
		if c.LTarget == "${meta.gpu-capable}" {
			hasGPUCapable = true
		}
		if c.LTarget == "${meta.gpu-dedicated}" {
			hasGPUDedicated = true
		}
		if c.LTarget == "${meta.gpu_compute_capability}" {
			hasComputeCapability = true
			computeCapValue = c.RTarget
		}
	}

	if hasGPUCapable {
		t.Error("Expected gpu-capable constraint NOT to be present when GPURequired is false")
	}

	if hasGPUDedicated {
		t.Error("Expected gpu-dedicated constraint NOT to be present when GPURequired is false")
	}

	if !hasComputeCapability {
		t.Error("Expected gpu_compute_capability constraint to be present")
	}

	if computeCapValue != "8.6" {
		t.Errorf("Expected gpu_compute_capability value '8.6', got '%s'", computeCapValue)
	}
}

// TestGPUCheckForCommandBasedTests tests GPU constraints are added for command-based tests
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

	// Each job should have GPU constraints but NO prestart task
	for i, testJob := range testJobs {
		taskGroup := testJob.TaskGroups[0]

		// Should only have 1 task (main), no prestart
		if len(taskGroup.Tasks) != 1 {
			t.Errorf("Job %d: Expected 1 task (main), got %d", i, len(taskGroup.Tasks))
		}

		// Should have GPU constraints
		var hasGPUCapable, hasGPUDedicated bool
		for _, c := range taskGroup.Constraints {
			if c.LTarget == "${meta.gpu-capable}" && c.RTarget == "true" {
				hasGPUCapable = true
			}
			if c.LTarget == "${meta.gpu-dedicated}" && c.RTarget == "true" && c.Operand == "!=" {
				hasGPUDedicated = true
			}
		}

		if !hasGPUCapable {
			t.Errorf("Job %d: Expected gpu-capable constraint", i)
		}
		if !hasGPUDedicated {
			t.Errorf("Job %d: Expected gpu-dedicated exclusion constraint", i)
		}
	}
}

// TestGPUNodePinWithComputeCapability tests that node pin works with compute capability
func TestGPUNodePinWithComputeCapability(t *testing.T) {
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
			EntryPoint:           true,
			GPURequired:          true,
			GPUNodePin:           "gpu002",
			GPUComputeCapability: "8.0", // Ampere
		},
	}

	job := &types.Job{
		ID:     "test-job-pin-with-compute",
		Config: jobConfig,
	}

	testJobs, err := client.CreateTestJobSpecs(job, "")
	if err != nil {
		t.Fatalf("Failed to create test job specs: %v", err)
	}

	if len(testJobs) == 0 {
		t.Fatal("Expected at least one test job")
	}

	constraints := testJobs[0].TaskGroups[0].Constraints

	// Should have: gpu-capable, node pin, AND compute capability (but NOT gpu-dedicated exclusion)
	var hasGPUCapable, hasNodePin, hasComputeCapability, hasGPUDedicated bool
	var nodePinValue, computeCapValue string

	for _, c := range constraints {
		if c.LTarget == "${meta.gpu-capable}" && c.RTarget == "true" && c.Operand == "=" {
			hasGPUCapable = true
		}
		if c.LTarget == "${node.unique.name}" && c.Operand == "=" {
			hasNodePin = true
			nodePinValue = c.RTarget
		}
		if c.LTarget == "${meta.gpu_compute_capability}" && c.Operand == ">=" {
			hasComputeCapability = true
			computeCapValue = c.RTarget
		}
		if c.LTarget == "${meta.gpu-dedicated}" {
			hasGPUDedicated = true
		}
	}

	if !hasGPUCapable {
		t.Error("Expected gpu-capable constraint")
	}

	if !hasNodePin {
		t.Error("Expected node pin constraint")
	}

	if nodePinValue != "gpu002" {
		t.Errorf("Expected node pin value 'gpu002', got '%s'", nodePinValue)
	}

	if !hasComputeCapability {
		t.Error("Expected compute capability constraint")
	}

	if computeCapValue != "8.0" {
		t.Errorf("Expected compute capability '8.0', got '%s'", computeCapValue)
	}

	if hasGPUDedicated {
		t.Error("Expected no gpu-dedicated constraint when node pin is used")
	}
}
