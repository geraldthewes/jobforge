package nomad

import (
	"fmt"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	
	"nomad-mcp-builder/pkg/types"
)

// createBuildJobSpec creates a Nomad job specification for the build phase
func (nc *Client) createBuildJobSpec(job *types.Job) (*nomadapi.Job, error) {
	jobID := fmt.Sprintf("build-%s", job.ID)
	
	// Resource limits with defaults for build phase
	buildDefaults := types.PhaseResourceLimits{
		CPU:    "1000", // Default 1000 MHz
		Memory: "2048", // Default 2048 MB
		Disk:   "10240", // Default 10 GB
	}

	buildLimits := job.Config.ResourceLimits.GetBuildLimits(buildDefaults)

	var cpu, memory, disk int
	fmt.Sscanf(buildLimits.CPU, "%d", &cpu)
	fmt.Sscanf(buildLimits.Memory, "%d", &memory)
	fmt.Sscanf(buildLimits.Disk, "%d", &disk)
	
	// Determine if we should skip tests (no tests configured)
	// Python tests also require the temp image flow
	skipTests := job.Config.Test == nil || (len(job.Config.Test.Commands) == 0 && !job.Config.Test.EntryPoint && job.Config.Test.PythonFile == "")
	
	var buildImageNames []string
	var pushCommands []string
	
	if skipTests {
		// No tests - build directly with final image names and push to final registry
		baseImageName := fmt.Sprintf("%s/%s", job.Config.RegistryURL, job.Config.ImageName)
		
		// Build with final image names directly
		for _, tag := range job.Config.ImageTags {
			finalImageName := fmt.Sprintf("%s:%s", baseImageName, tag)
			buildImageNames = append(buildImageNames, finalImageName)
		}
		
		// Push commands for each final tag
		for _, imageName := range buildImageNames {
			pushCommands = append(pushCommands, 
				fmt.Sprintf("buildah push --tls-verify=true %s docker://%s", imageName, imageName))
		}
	} else {
		// Tests configured - use temporary image name and push to temp registry
		tempImageName := nc.generateTempImageName(job)
		buildImageNames = []string{tempImageName}
		pushCommands = []string{
			fmt.Sprintf("buildah push --tls-verify=true %s docker://%s", tempImageName, tempImageName),
		}
	}
	
	// Create isolated cache directory for this project to prevent tag contamination
	sanitizedImageName := strings.ToLower(strings.ReplaceAll(job.Config.ImageName, "/", "-"))
	sanitizedImageName = strings.ReplaceAll(sanitizedImageName, "_", "-")
	isolatedCacheDir := fmt.Sprintf("/var/lib/containers/%s", sanitizedImageName)
	
	// Build the task commands with layer caching and proper HTTPS handling
	buildCommands := []string{
		"#!/bin/bash",
		"set -euo pipefail",
		"",
		"# Cache isolation setup",
		"echo 'Setting up isolated build cache...'",
		fmt.Sprintf("mkdir -p %s %s/tmp", isolatedCacheDir, isolatedCacheDir),
		"",
	}
	
	// Add aggressive cache clearing if requested
	if job.Config.ClearCache {
		buildCommands = append(buildCommands,
			"# Clear cache requested - performing aggressive cleanup",
			"echo 'Clearing build cache as requested...'",
			"",
			"# Remove isolated cache directory completely", 
			fmt.Sprintf("rm -rf %s", isolatedCacheDir),
			fmt.Sprintf("mkdir -p %s %s/tmp", isolatedCacheDir, isolatedCacheDir),
			"",
		)
	} else {
		buildCommands = append(buildCommands,
			"# Standard cache management with isolation",
			"# Check isolated cache space usage",
			fmt.Sprintf("echo 'Current isolated cache usage for %s:'", sanitizedImageName),
			fmt.Sprintf("du -sh %s 2>/dev/null || echo 'Isolated cache directory not yet created'", isolatedCacheDir),
			"",
			"# Clean up old/unused images in isolated cache if getting full",
			fmt.Sprintf("STORAGE_SIZE=$(du -s %s 2>/dev/null | cut -f1 || echo '0')", isolatedCacheDir),
			"STORAGE_LIMIT=4000000  # 4GB in KB per project",
			"if [ \"$STORAGE_SIZE\" -gt \"$STORAGE_LIMIT\" ]; then",
			"  echo 'Isolated cache approaching limit, cleaning up old images...'",
			"  BUILDAH_ROOT=$(pwd) buildah rmi --prune --force 2>/dev/null || echo 'No old images to clean'",
			"  BUILDAH_ROOT=$(pwd) buildah system prune --force 2>/dev/null || echo 'No system cache to clean'",
			"else",
			"  echo 'Isolated cache usage within limits, keeping layers'",
			"fi",
			"",
		)
	}
	
	// Add repository cloning and setup
	buildCommands = append(buildCommands,
		"# Clone repository", 
		fmt.Sprintf("git clone %s /tmp/repo", job.Config.RepoURL),
		"cd /tmp/repo",
		fmt.Sprintf("git checkout %s", job.Config.GitRef),
		"",
		"# Configure Buildah for isolated layer caching",
		"export STORAGE_DRIVER=overlay",
		"export BUILDAH_LAYERS=true",  // Enable layer caching by default
		fmt.Sprintf("export BUILDAH_ROOT=%s", isolatedCacheDir), // Use isolated cache
		fmt.Sprintf("export TMPDIR=%s/tmp", isolatedCacheDir), // Use isolated tmp
		"",
	)
	
	// Determine build context (default to "." if not specified)
	buildContext := job.Config.DockerfileContext
	if buildContext == "" {
		buildContext = "."
	}

	// Add build commands for each image name
	if skipTests {
		buildCommands = append(buildCommands, "# Build directly with final image names (no tests configured)")
		for _, imageName := range buildImageNames {
			buildCommands = append(buildCommands,
				fmt.Sprintf("buildah bud --isolation=chroot --layers --file %s --tag %s %s",
					job.Config.DockerfilePath, imageName, buildContext))
		}
	} else {
		buildCommands = append(buildCommands, "# Build with temporary image name for testing")
		buildCommands = append(buildCommands,
			fmt.Sprintf("buildah bud --isolation=chroot --layers --file %s --tag %s %s",
				job.Config.DockerfilePath, buildImageNames[0], buildContext))
	}
	
	buildCommands = append(buildCommands,
		"",
		"# Authenticate with registry if credentials are provided and non-empty",
		"if [ -f /secrets/registry-creds ]; then",
		"  source /secrets/registry-creds",
		"  if [ -n \"$REGISTRY_USERNAME\" ] && [ -n \"$REGISTRY_PASSWORD\" ]; then",
		"    echo 'Logging in to registry with credentials...'",
		fmt.Sprintf("    buildah login --username \"$REGISTRY_USERNAME\" --password \"$REGISTRY_PASSWORD\" %s", registryHost(buildImageNames[0])),
		"  else",
		"    echo 'No registry credentials provided, attempting anonymous push...'",
		"  fi",
		"else",
		"  echo 'No registry credentials file, attempting anonymous push...'",
		"fi",
		"",
		"# Configure buildah to use TLS and mount certificates properly",
		"export BUILDAH_TLS_VERIFY=true",
		"",
	)
	
	// Add push commands
	if skipTests {
		buildCommands = append(buildCommands, "# Push directly to final image tags (no tests configured)")
	} else {
		buildCommands = append(buildCommands, "# Push temporary image for testing")
	}
	buildCommands = append(buildCommands, pushCommands...)
	
	// Post-build cache management
	buildCommands = append(buildCommands,
		"",
		"# Post-build cache management",
		"echo 'Managing isolated build cache after build...'",
		"",
		"# Clean up intermediate build containers (but keep layers for next builds)",
		"buildah rm --all 2>/dev/null || echo 'No build containers to clean'",
		"",
		"# Remove our specific build images from local storage to save space",
		"# (they're now in the registry, and we don't need them locally anymore)",
	)
	
	// Remove each built image from local storage
	for _, imageName := range buildImageNames {
		buildCommands = append(buildCommands,
			fmt.Sprintf("buildah rmi %s 2>/dev/null || echo 'Build image %s already removed'", imageName, imageName))
	}
	
	buildCommands = append(buildCommands,
		"",
		"# Clean up any dangling/unused images older than this build",
		"buildah image prune --filter \"until=1h\" --force 2>/dev/null || echo 'No old images to prune'",
		"",
		"# Show final isolated cache usage",
		fmt.Sprintf("echo 'Final isolated cache usage for %s:'", sanitizedImageName),
		fmt.Sprintf("du -sh %s 2>/dev/null || echo 'Isolated cache directory empty'", isolatedCacheDir),
		"",
		"# Force sync file system to ensure all writes are committed",
		"sync",
		"",
		"echo 'Build completed successfully'",
		"exit 0", // Explicitly exit to ensure container terminates
	)
	
	jobSpec := &nomadapi.Job{
		ID:          &jobID,
		Name:        &jobID,
		Type:        stringPtr("batch"),
		Namespace:   stringPtr(nc.config.Nomad.Namespace),
		Region:      stringPtr(nc.config.Nomad.Region),
		Datacenters: nc.config.Nomad.Datacenters,
		Meta: map[string]string{
			"build-service-job-id": job.ID,
			"phase":                "build",
		},
		TaskGroups: []*nomadapi.TaskGroup{
			{
				Name:        stringPtr("build"),
				Count:       intPtr(1),
				Constraints: []*nomadapi.Constraint{}, // Override automatic constraints
				RestartPolicy: &nomadapi.RestartPolicy{
					Attempts: intPtr(0), // No restart for build jobs
				},
				Tasks: []*nomadapi.Task{
					{
						Name:   "main",
						Driver: "docker",
						Config: map[string]interface{}{
							"image": "quay.io/buildah/stable:latest",
							"command": "/bin/bash",
							"args": []string{"-c", strings.Join(buildCommands, "\n")},
							"privileged": true, // Use privileged for simpler overlay without fuse-overlayfs
							"volumes": []string{
								"/opt/nomad/data/buildah-cache:/var/lib/containers:rw", // Persistent layer cache
								"/etc/docker/certs.d:/etc/containers/certs.d:ro",   // Registry certificates for buildah
								"/etc/docker/certs.d:/etc/docker/certs.d:ro",       // Registry certificates for Docker compat
								"/etc/ssl/certs:/etc/ssl/certs:ro",                 // System CA certificates
							},
						},
						Env: map[string]string{
							"BUILDAH_ISOLATION": "chroot",
							"STORAGE_DRIVER":    "overlay",
							"BUILDAH_LAYERS":    "true", // Enable layer caching for large images
							"BUILDAH_TLS_VERIFY": "true", // Enable TLS verification
						},
						Resources: &nomadapi.Resources{
							CPU:      intPtr(cpu),
							MemoryMB: intPtr(memory),
							DiskMB:   intPtr(disk),
						},
						KillTimeout: &nc.config.Build.KillTimeout,
					},
				},
				EphemeralDisk: &nomadapi.EphemeralDisk{
					SizeMB: intPtr(disk),
				},
			},
		},
		// Allow job to run (don't auto-stop)
		Stop:        boolPtr(false),
	}
	
	// KillTimeout is now set per task using configurable value
	
	// Add Vault integration and templates only if needed
	templates := buildTemplates(job)
	if len(templates) > 0 {
		mainTask := jobSpec.TaskGroups[0].Tasks[0]
		mainTask.Vault = &nomadapi.Vault{
			Policies:   []string{"nomad-build-service"},
			ChangeMode: stringPtr("restart"),
			Role:       "nomad-workloads",
		}
		mainTask.Templates = templates
	}
	
	return jobSpec, nil
}

// CreateTestJobSpecs creates Nomad job specifications for the test phase using Docker driver directly
func (nc *Client) CreateTestJobSpecs(job *types.Job, buildNodeID string) ([]*nomadapi.Job, error) {
	var testJobs []*nomadapi.Job

	// If no test configuration, return empty array
	if job.Config.Test == nil {
		return []*nomadapi.Job{}, nil
	}

	// Resource limits with defaults for test phase
	testDefaults := types.PhaseResourceLimits{
		CPU:    "500",  // Less than build phase since tests are simpler
		Memory: "1024", // 1024 MB
		Disk:   "2048", // Tests need minimal disk
	}

	// Use test-specific resource limits if provided, otherwise fall back to global resource limits
	var testLimits types.PhaseResourceLimits
	if job.Config.Test.ResourceLimits != nil {
		testLimits = *job.Config.Test.ResourceLimits
		// Fill in any missing values with defaults
		if testLimits.CPU == "" {
			testLimits.CPU = testDefaults.CPU
		}
		if testLimits.Memory == "" {
			testLimits.Memory = testDefaults.Memory
		}
		if testLimits.Disk == "" {
			testLimits.Disk = testDefaults.Disk
		}
	} else {
		testLimits = job.Config.ResourceLimits.GetTestLimits(testDefaults)
	}

	var cpu, memory, disk int
	fmt.Sscanf(testLimits.CPU, "%d", &cpu)
	fmt.Sscanf(testLimits.Memory, "%d", &memory)
	fmt.Sscanf(testLimits.Disk, "%d", &disk)

	// Create temporary image name - this is the image built in the build phase
	tempImageName := nc.generateTempImageName(job)

	// Build constraints for test jobs
	constraints := nc.buildTestConstraints(job, buildNodeID)

	// Mode 1: Create separate test jobs for each custom test command
	if len(job.Config.Test.Commands) > 0 {
		for i, testCmd := range job.Config.Test.Commands {
			jobID := fmt.Sprintf("test-cmd-%s-%d", job.ID, i)
			
			testJobSpec := &nomadapi.Job{
				ID:          &jobID,
				Name:        &jobID,
				Type:        stringPtr("batch"),
				Namespace:   stringPtr(nc.config.Nomad.Namespace),
				Region:      stringPtr(nc.config.Nomad.Region),
				Datacenters: nc.config.Nomad.Datacenters,
				Meta: map[string]string{
					"build-service-job-id": job.ID,
					"phase":                "test",
					"test-type":            "command",
					"test-index":           fmt.Sprintf("%d", i),
					"test-command":         testCmd,
				},
				TaskGroups: []*nomadapi.TaskGroup{
					{
						Name:        stringPtr("test"),
						Count:       intPtr(1),
						Constraints: constraints,
						RestartPolicy: &nomadapi.RestartPolicy{
							Attempts: intPtr(0), // No retries for test failures
						},
						ReschedulePolicy: &nomadapi.ReschedulePolicy{
							Attempts:  intPtr(0),
							Unlimited: boolPtr(false),
						},
						Tasks: []*nomadapi.Task{
							{
								Name:        "main",
								Driver:      "docker",
								Config:      nc.configureTestDockerConfig(job, tempImageName, testCmd),
								Env:         nc.buildTestEnv(job), // Add test environment variables with GPU defaults
								Resources:   nc.buildTestResources(job, cpu, memory, disk),
								KillTimeout: &nc.config.Build.KillTimeout,
								LogConfig: &nomadapi.LogConfig{
									MaxFiles:      intPtr(nc.config.Build.DockerLogMaxFiles),
									MaxFileSizeMB: intPtr(10),
								},
							},
						},
						EphemeralDisk: &nomadapi.EphemeralDisk{
							SizeMB: intPtr(disk),
						},
					},
				},
			}
			
			// Add Vault integration if vault secrets OR registry credentials are needed
			if (job.Config.Test != nil && len(job.Config.Test.VaultSecrets) > 0) || job.Config.RegistryCredentialsPath != "" {
				templates := testTemplates(job)
				if len(templates) > 0 {
					mainTask := testJobSpec.TaskGroups[0].Tasks[0]

					// Determine which policies to use
					var policies []string
					if job.Config.Test != nil && len(job.Config.Test.VaultPolicies) > 0 {
						policies = job.Config.Test.VaultPolicies
					} else {
						policies = []string{"nomad-build-service"} // Default fallback
					}

					mainTask.Vault = &nomadapi.Vault{
						Policies:   policies,
						ChangeMode: stringPtr("restart"),
						Role:       "nomad-workloads",
					}
					mainTask.Templates = templates
				}
			}

			testJobs = append(testJobs, testJobSpec)
		}
	}

	// Mode 2: Create a test job that runs the image's entry point/CMD
	if job.Config.Test.EntryPoint {
		jobID := fmt.Sprintf("test-entry-%s", job.ID)
		
		testJobSpec := &nomadapi.Job{
			ID:          &jobID,
			Name:        &jobID,
			Type:        stringPtr("batch"),
			Namespace:   stringPtr(nc.config.Nomad.Namespace),
			Region:      stringPtr(nc.config.Nomad.Region),
			Datacenters: nc.config.Nomad.Datacenters,
			Meta: map[string]string{
				"build-service-job-id": job.ID,
				"phase":                "test",
				"test-type":            "entrypoint",
			},
			TaskGroups: []*nomadapi.TaskGroup{
				{
					Name:        stringPtr("test"),
					Count:       intPtr(1),
					Constraints: constraints,
					RestartPolicy: &nomadapi.RestartPolicy{
						Attempts: intPtr(0), // No retries for test failures
					},
					ReschedulePolicy: &nomadapi.ReschedulePolicy{
						Attempts:  intPtr(0),
						Unlimited: boolPtr(false),
					},
					Tasks: []*nomadapi.Task{
						{
							Name:        "main",
							Driver:      "docker",
							Config:      nc.configureTestDockerConfig(job, tempImageName, ""), // Empty testCmd for entrypoint
							Env:         nc.buildTestEnv(job), // Add test environment variables with GPU defaults
							Resources:   nc.buildTestResources(job, cpu, memory, disk),
							KillTimeout: &nc.config.Build.KillTimeout,
							LogConfig: &nomadapi.LogConfig{
								MaxFiles:      intPtr(nc.config.Build.DockerLogMaxFiles),
								MaxFileSizeMB: intPtr(10),
							},
						},
					},
					EphemeralDisk: &nomadapi.EphemeralDisk{
						SizeMB: intPtr(disk),
					},
				},
			},
		}
		
		// Add Vault integration if vault secrets OR registry credentials are needed
		if (job.Config.Test != nil && len(job.Config.Test.VaultSecrets) > 0) || job.Config.RegistryCredentialsPath != "" {
			templates := testTemplates(job)
			if len(templates) > 0 {
				mainTask := testJobSpec.TaskGroups[0].Tasks[0]

				// Determine which policies to use
				var policies []string
				if job.Config.Test != nil && len(job.Config.Test.VaultPolicies) > 0 {
					policies = job.Config.Test.VaultPolicies
				} else {
					policies = []string{"nomad-build-service"} // Default fallback
				}

				mainTask.Vault = &nomadapi.Vault{
					Policies:   policies,
					ChangeMode: stringPtr("restart"),
					Role:       "nomad-workloads",
				}
				mainTask.Templates = templates
			}
		}

		testJobs = append(testJobs, testJobSpec)
	}

	// If no test configuration, return empty array (caller will skip test phase)
	if len(testJobs) == 0 {
		return []*nomadapi.Job{}, nil
	}
	
	return testJobs, nil
}

// createExternalTestJobSpec creates a Nomad service job for external (python) tests
// Unlike regular test jobs, this creates a long-running service that the CLI connects to
func (nc *Client) createExternalTestJobSpec(job *types.Job, buildNodeID string) (*nomadapi.Job, error) {
	jobID := fmt.Sprintf("test-external-%s", job.ID)

	// Determine the container port (default 8080)
	containerPort := 8080
	if job.Config.Test != nil && job.Config.Test.ContainerPort > 0 {
		containerPort = job.Config.Test.ContainerPort
	}

	tempImageName := nc.generateTempImageName(job)
	constraints := nc.buildTestConstraints(job, buildNodeID)

	// Resource limits with defaults for test phase
	testDefaults := types.PhaseResourceLimits{
		CPU:    "500",
		Memory: "1024",
		Disk:   "2048",
	}

	var testLimits types.PhaseResourceLimits
	if job.Config.Test != nil && job.Config.Test.ResourceLimits != nil {
		testLimits = *job.Config.Test.ResourceLimits
		if testLimits.CPU == "" {
			testLimits.CPU = testDefaults.CPU
		}
		if testLimits.Memory == "" {
			testLimits.Memory = testDefaults.Memory
		}
		if testLimits.Disk == "" {
			testLimits.Disk = testDefaults.Disk
		}
	} else {
		testLimits = job.Config.ResourceLimits.GetTestLimits(testDefaults)
	}

	var cpu, memory, disk int
	fmt.Sscanf(testLimits.CPU, "%d", &cpu)
	fmt.Sscanf(testLimits.Memory, "%d", &memory)
	fmt.Sscanf(testLimits.Disk, "%d", &disk)

	// Docker config for external test container
	dockerConfig := map[string]interface{}{
		"image":      tempImageName,
		"force_pull": true,
		"ports":      []string{"http"},
		"volumes": []string{
			"/etc/docker/certs.d:/etc/docker/certs.d:ro",
		},
	}

	// Add GPU runtime if required
	if job.Config.Test != nil && job.Config.Test.GPURequired {
		dockerConfig["runtime"] = "nvidia"
	}

	// Add Docker logging configuration
	dockerConfig["logging"] = map[string]interface{}{
		"type": "json-file",
		"config": []map[string]interface{}{
			{
				"max-file": fmt.Sprintf("%d", nc.config.Build.DockerLogMaxFiles),
				"max-size": nc.config.Build.DockerLogMaxFileSize,
			},
		},
	}

	testJobSpec := &nomadapi.Job{
		ID:          &jobID,
		Name:        &jobID,
		Type:        stringPtr("service"), // SERVICE job, not batch - stays running
		Namespace:   stringPtr(nc.config.Nomad.Namespace),
		Region:      stringPtr(nc.config.Nomad.Region),
		Datacenters: nc.config.Nomad.Datacenters,
		Meta: map[string]string{
			"build-service-job-id": job.ID,
			"phase":                "test",
			"test-type":            "external",
		},
		TaskGroups: []*nomadapi.TaskGroup{
			{
				Name:        stringPtr("test"),
				Count:       intPtr(1),
				Constraints: constraints,
				Networks: []*nomadapi.NetworkResource{
					{
						Mode: "host",
						DynamicPorts: []nomadapi.Port{
							{
								Label: "http",
								To:    containerPort,
							},
						},
					},
				},
				Tasks: []*nomadapi.Task{
					{
						Name:      "main",
						Driver:    "docker",
						Config:    dockerConfig,
						Env:       nc.buildTestEnv(job),
						Resources: nc.buildTestResources(job, cpu, memory, disk),
						LogConfig: &nomadapi.LogConfig{
							MaxFiles:      intPtr(nc.config.Build.DockerLogMaxFiles),
							MaxFileSizeMB: intPtr(10),
						},
					},
				},
				EphemeralDisk: &nomadapi.EphemeralDisk{
					SizeMB: intPtr(disk),
				},
			},
		},
	}

	// Add Vault integration if vault secrets OR registry credentials are needed
	if (job.Config.Test != nil && len(job.Config.Test.VaultSecrets) > 0) || job.Config.RegistryCredentialsPath != "" {
		templates := testTemplates(job)
		if len(templates) > 0 {
			mainTask := testJobSpec.TaskGroups[0].Tasks[0]

			var policies []string
			if job.Config.Test != nil && len(job.Config.Test.VaultPolicies) > 0 {
				policies = job.Config.Test.VaultPolicies
			} else {
				policies = []string{"nomad-build-service"}
			}

			mainTask.Vault = &nomadapi.Vault{
				Policies:   policies,
				ChangeMode: stringPtr("restart"),
				Role:       "nomad-workloads",
			}
			mainTask.Templates = templates
		}
	}

	return testJobSpec, nil
}

// createPublishJobSpec creates a Nomad job specification for the publish phase
func (nc *Client) createPublishJobSpec(job *types.Job) (*nomadapi.Job, error) {
	jobID := fmt.Sprintf("publish-%s", job.ID)
	
	// Resource limits with defaults for publish phase
	publishDefaults := types.PhaseResourceLimits{
		CPU:    "500",  // Minimal for publish phase
		Memory: "1024", // 1024 MB
		Disk:   "2048", // 2048 MB
	}

	publishLimits := job.Config.ResourceLimits.GetPublishLimits(publishDefaults)

	var cpu, memory, disk int
	fmt.Sscanf(publishLimits.CPU, "%d", &cpu)
	fmt.Sscanf(publishLimits.Memory, "%d", &memory)
	fmt.Sscanf(publishLimits.Disk, "%d", &disk)
	
	// Create temporary and final image names
	tempImageName := nc.generateTempImageName(job)
	
	// Build publish commands for each tag
	publishCommands := []string{
		"#!/bin/bash",
		"set -euo pipefail",
		"",
		"# Pull the tested image",
		fmt.Sprintf("buildah pull docker://%s", tempImageName),
		"",
		"# Tag and push to final destinations",
	}
	
	for _, tag := range job.Config.ImageTags {
		finalImageName := fmt.Sprintf("%s/%s:%s", job.Config.RegistryURL, job.Config.ImageName, tag)
		publishCommands = append(publishCommands,
			fmt.Sprintf("buildah tag %s %s", tempImageName, finalImageName),
			"",
			"# Get image size before push using buildah images format",
			fmt.Sprintf("SIZE=$(buildah images --format '{{.Size}}' %s 2>/dev/null | head -1 || echo '0')", finalImageName),
			"# Convert human-readable size to bytes for consistent parsing",
			"# Parse sizes like '8.13 MB', '1.2 GB', '500 KB' into bytes",
			"SIZE_BYTES=$(echo \"$SIZE\" | awk '{",
			"  size=$1; unit=$2;",
			"  if (unit == \"B\" || unit == \"\") bytes = size;",
			"  else if (unit == \"KB\" || unit == \"kB\") bytes = size * 1024;",
			"  else if (unit == \"MB\") bytes = size * 1024 * 1024;",
			"  else if (unit == \"GB\") bytes = size * 1024 * 1024 * 1024;",
			"  else if (unit == \"TB\") bytes = size * 1024 * 1024 * 1024 * 1024;",
			"  else bytes = 0;",
			"  printf \"%.0f\", bytes",
			"}')",
			"",
			fmt.Sprintf("buildah push %s docker://%s", finalImageName, finalImageName),
			fmt.Sprintf("echo '===IMAGE_SIZE:%s:'$SIZE_BYTES'==='", finalImageName),
			fmt.Sprintf("echo 'Published image: %s (size: '$SIZE')'", finalImageName),
			"",
		)
	}
	
	publishCommands = append(publishCommands, 
		"echo 'All images published successfully'",
		"exit 0", // Explicitly exit to ensure container terminates
	)
	
	jobSpec := &nomadapi.Job{
		ID:          &jobID,
		Name:        &jobID,
		Type:        stringPtr("batch"),
		Namespace:   stringPtr(nc.config.Nomad.Namespace),
		Region:      stringPtr(nc.config.Nomad.Region),
		Datacenters: nc.config.Nomad.Datacenters,
		Meta: map[string]string{
			"build-service-job-id": job.ID,
			"phase":                "publish",
		},
		TaskGroups: []*nomadapi.TaskGroup{
			{
				Name:        stringPtr("publish"),
				Count:       intPtr(1),
				Constraints: []*nomadapi.Constraint{}, // Override automatic constraints
				RestartPolicy: &nomadapi.RestartPolicy{
					Attempts: intPtr(0), // No restart for publish jobs
				},
				Tasks: []*nomadapi.Task{
					{
						Name:   "main",
						Driver: "docker",
						Config: map[string]interface{}{
							"image":   "quay.io/buildah/stable:latest",
							"command": "/bin/bash",
							"args":    []string{"-c", strings.Join(publishCommands, "\n")},
							// Add capabilities required for Buildah user namespaces
							"cap_add": []string{
								"SYS_ADMIN",     // Required for mount operations
								"SETUID",        // Required for user namespace operations
								"SETGID",        // Required for user namespace operations
								"SYS_CHROOT",    // Required for chroot operations
							},
							// Enable user namespace mapping
							"userns_mode": "host",
							// Add security options for user namespace
							"security_opt": []string{
								"seccomp=unconfined",
								"apparmor=unconfined",
							},
							// Mount cgroup filesystem for container management
							"volumes": []string{
								"/sys/fs/cgroup:/sys/fs/cgroup:rw",
								"/tmp:/tmp:rw",  // Ensure tmp is writable
								"/etc/docker/certs.d:/etc/containers/certs.d:ro",   // Registry certificates
							},
							// Removed /dev/fuse device to avoid version constraints
						},
						Env: map[string]string{
							"BUILDAH_ISOLATION": "chroot",  // Use chroot isolation to avoid cgroup issues
							"STORAGE_DRIVER":    "vfs",     // Use VFS storage driver for better compatibility
							"BUILDAH_FORMAT":    "oci",     // Ensure OCI format
							"BUILDAH_LAYERS":    "false",   // Disable layer caching to simplify
							"_BUILDAH_STARTED_IN_USERNS": "1", // Tell buildah we're in a user namespace
							"BUILDAH_ROOTLESS":  "1",       // Enable rootless mode
							// Remove cgroup manager to avoid delegation issues
						},
						Resources: &nomadapi.Resources{
							CPU:      intPtr(cpu),
							MemoryMB: intPtr(memory),
							DiskMB:   intPtr(disk),
						},
						KillTimeout: &nc.config.Build.KillTimeout, // Configurable timeout
					},
				},
				EphemeralDisk: &nomadapi.EphemeralDisk{
					SizeMB: intPtr(disk),
				},
			},
		},
	}
	
	// Add Vault integration and templates only if needed
	templates := publishTemplates(job)
	if len(templates) > 0 {
		mainTask := jobSpec.TaskGroups[0].Tasks[0]
		mainTask.Vault = &nomadapi.Vault{
			Policies:   []string{"nomad-build-service"},
			ChangeMode: stringPtr("restart"),
			Role:       "nomad-workloads",
		}
		mainTask.Templates = templates
	}
	
	return jobSpec, nil
}

// createCleanupJobSpec creates a Nomad job specification for cleanup operations
func (nc *Client) createCleanupJobSpec(job *types.Job) *nomadapi.Job {
	jobID := fmt.Sprintf("cleanup-%s", job.ID)
	
	// Create temporary image name for cleanup
	tempImageName := nc.generateTempImageName(job)
	
	// Extract registry host, image path, and tag for API calls
	registryHost := registryHost(tempImageName)
	imageWithTag := strings.TrimPrefix(tempImageName, registryHost+"/")
	parts := strings.Split(imageWithTag, ":")
	imagePath := parts[0] // Repository name
	imageTag := parts[1]  // Job ID as tag

	cleanupCommands := []string{
		"#!/bin/bash",
		"set -euo pipefail",
		"",
		"# Function to call registry API with proper authentication",
		"call_registry_api() {",
		"  local method=\"$1\"",
		"  local url=\"$2\"",
		"  local auth_header=\"\"",
		"  ",
		"  # Use registry credentials if available",
		"  if [ -f /secrets/registry-creds ]; then",
		"    source /secrets/registry-creds",
		"    if [ -n \"$REGISTRY_USERNAME\" ] && [ -n \"$REGISTRY_PASSWORD\" ]; then",
		"      local auth=$(echo -n \"$REGISTRY_USERNAME:$REGISTRY_PASSWORD\" | base64 -w 0)",
		"      auth_header=\"-H 'Authorization: Basic $auth'\"",
		"    fi",
		"  fi",
		"  ",
		"  eval \"curl -s -k -X $method $auth_header \\\"$url\\\"\"", // -k to ignore SSL cert issues
		"}",
		"",
		fmt.Sprintf("echo 'Cleaning up temporary image: %s'", tempImageName),
		fmt.Sprintf("REGISTRY_HOST='%s'", registryHost),
		fmt.Sprintf("IMAGE_PATH='%s'", imagePath),
		fmt.Sprintf("TAG='%s'", imageTag),
		"",
		"# Step 1: Check if image exists and get the manifest digest",
		"echo 'Checking if temporary image exists...'",
		"HTTP_STATUS=$(curl -s -k -I -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \"https://$REGISTRY_HOST/v2/$IMAGE_PATH/manifests/$TAG\" -w '%%{http_code}' -o /dev/null)",
		"echo \"Registry responded with HTTP status: $HTTP_STATUS\"",
		"",
		"if [ \"$HTTP_STATUS\" = \"200\" ]; then",
		"  echo 'Image exists, getting digest for deletion...'",
		"  # Extract digest from response headers",
		"  DIGEST=$(curl -s -k -I -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \"https://$REGISTRY_HOST/v2/$IMAGE_PATH/manifests/$TAG\" | grep -i docker-content-digest | cut -d' ' -f2 | tr -d '\\r')",
		"elif [ \"$HTTP_STATUS\" = \"404\" ]; then",
		"  echo 'Image not found (already deleted or never existed)'",
		"  echo 'Cleanup completed - nothing to delete'",
		"  exit 0",
		"else",
		"  echo \"Unexpected HTTP status: $HTTP_STATUS\"",
		"  echo 'Proceeding with cleanup attempt anyway...'",
		"  DIGEST=\"\"",
		"fi",
		"",
		"if [ -n \"$DIGEST\" ] && [ \"$DIGEST\" != \"\" ]; then",
		"  echo \"Found image digest: $DIGEST\"",
		"  ",
		"  # Step 2: Delete the manifest using the digest",
		"  echo 'Deleting image manifest...'",
		"  DELETE_RESPONSE=$(call_registry_api DELETE \"https://$REGISTRY_HOST/v2/$IMAGE_PATH/manifests/$DIGEST\")",
		"  ",
		"  if [ $? -eq 0 ]; then",
		"    echo 'Image manifest deleted successfully'",
		"  else",
		"    echo 'Warning: Failed to delete image manifest, but continuing...'",
		"  fi",
		"else",
		"  echo 'Warning: Could not find image digest, image may not exist or already be deleted'",
		"fi",
		"",
		"# Step 3: Trigger garbage collection (if supported by registry)",
		"echo 'Attempting to trigger registry garbage collection...'",
		"GC_RESPONSE=$(call_registry_api POST \"https://$REGISTRY_HOST/v2/_catalog\" 2>/dev/null || true)",
		"",
		"# Note: Most Docker registries require manual garbage collection",
		"# This is typically done via registry configuration or separate tools",
		"echo 'Registry cleanup commands completed'",
		"echo 'Note: Registry garbage collection may need to be run separately to free disk space'",
		"",
		"echo 'Cleanup completed successfully'",
	}
	
	jobSpec := &nomadapi.Job{
		ID:          &jobID,
		Name:        &jobID,
		Type:        stringPtr("batch"),
		Namespace:   stringPtr(nc.config.Nomad.Namespace),
		Region:      stringPtr(nc.config.Nomad.Region),
		Datacenters: nc.config.Nomad.Datacenters,
		Meta: map[string]string{
			"build-service-job-id": job.ID,
			"phase":                "cleanup",
		},
		TaskGroups: []*nomadapi.TaskGroup{
			{
				Name:  stringPtr("cleanup"),
				Count: intPtr(1),
				RestartPolicy: &nomadapi.RestartPolicy{
					Attempts: intPtr(0),
				},
				Tasks: []*nomadapi.Task{
					{
						Name:   "main",
						Driver: "docker",
						Config: map[string]interface{}{
							"image":   "curlimages/curl:latest", // Use curl image for registry API calls
							"command": "/bin/sh",
							"args":    []string{"-c", strings.Join(cleanupCommands, "\n")},
							"volumes": []string{
								"/etc/docker/certs.d:/etc/docker/certs.d:ro", // Registry certificates
								"/etc/ssl/certs:/etc/ssl/certs:ro",           // System CA certificates
							},
						},
						Resources: &nomadapi.Resources{
							CPU:      intPtr(100),
							MemoryMB: intPtr(256),
							DiskMB:   intPtr(512),
						},
						KillTimeout: &nc.config.Build.KillTimeout,
					},
				},
				EphemeralDisk: &nomadapi.EphemeralDisk{
					SizeMB: intPtr(512),
				},
			},
		},
	}

	// Add registry credentials if needed for private registries
	if job.Config.RegistryCredentialsPath != "" {
		templates := cleanupTemplates(job)
		if len(templates) > 0 {
			mainTask := jobSpec.TaskGroups[0].Tasks[0]
			mainTask.Vault = &nomadapi.Vault{
				Policies:   []string{"nomad-build-service"},
				ChangeMode: stringPtr("restart"),
				Role:       "nomad-workloads",
			}
			mainTask.Templates = templates
		}
	}

	return jobSpec
}

// Helper functions for creating Nomad API types
func stringPtr(s string) *string {
	return &s
}

func intPtr(i int) *int {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}

// generateTempImageName creates the temporary image name with branch-based isolation
// Format: registry.url/bdtemp-imagename:branch-job-id
// This ensures builds on different branches don't conflict, but same branch builds are prevented
func (nc *Client) generateTempImageName(job *types.Job) string {
	// Sanitize image name for use in registry path (remove special chars, make lowercase)
	sanitizedImageName := strings.ToLower(strings.ReplaceAll(job.Config.ImageName, "/", "-"))
	sanitizedImageName = strings.ReplaceAll(sanitizedImageName, "_", "-")

	// Sanitize branch name for use in image tag (remove special chars, limit length)
	sanitizedBranch := strings.ToLower(job.Config.GitRef)
	sanitizedBranch = strings.ReplaceAll(sanitizedBranch, "/", "-")
	sanitizedBranch = strings.ReplaceAll(sanitizedBranch, "_", "-")
	sanitizedBranch = strings.ReplaceAll(sanitizedBranch, ".", "-")
	// Limit branch name length to avoid registry tag limits (max 128 chars)
	if len(sanitizedBranch) > 50 {
		sanitizedBranch = sanitizedBranch[:50]
	}

	tempPrefix := fmt.Sprintf("%s-%s", nc.config.Build.RegistryConfig.TempPrefix, sanitizedImageName)

	return fmt.Sprintf("%s/%s:%s-%s",
		nc.config.Build.RegistryConfig.URL,
		tempPrefix,
		sanitizedBranch,
		job.ID)
}

func durationPtr(d string) *time.Duration {
	duration, _ := time.ParseDuration(d)
	return &duration
}

// buildTemplates creates Vault templates for a job, conditionally including registry credentials
func buildTemplates(job *types.Job) []*nomadapi.Template {
	var templates []*nomadapi.Template

	// Only add git credentials template if path is provided and not empty
	if job.Config.GitCredentialsPath != "" {
		gitTemplate := &nomadapi.Template{
			DestPath:   stringPtr("/secrets/git-creds"),
			ChangeMode: stringPtr("restart"),
			EmbeddedTmpl: stringPtr(fmt.Sprintf(`
{{- with secret "%s" -}}
export GIT_USERNAME="{{ .Data.data.username }}"
export GIT_PASSWORD="{{ .Data.data.password }}"
export GIT_SSH_KEY="{{ .Data.data.ssh_key }}"
{{- end -}}
`, job.Config.GitCredentialsPath)),
		}
		templates = append(templates, gitTemplate)
	}
	
	// Only add registry credentials template if path is provided and not empty
	if job.Config.RegistryCredentialsPath != "" {
		registryTemplate := &nomadapi.Template{
			DestPath:   stringPtr("/secrets/registry-creds"),
			ChangeMode: stringPtr("restart"),
			EmbeddedTmpl: stringPtr(fmt.Sprintf(`
{{- with secret "%s" -}}
export REGISTRY_USERNAME="{{ .Data.data.username }}"
export REGISTRY_PASSWORD="{{ .Data.data.password }}"
{{- end -}}
`, job.Config.RegistryCredentialsPath)),
		}
		templates = append(templates, registryTemplate)
	}
	
	return templates
}

// testTemplates creates Vault templates for the test phase (only registry credentials if needed)
func testTemplates(job *types.Job) []*nomadapi.Template {
	var templates []*nomadapi.Template

	// Only add registry credentials template if path is provided and not empty
	if job.Config.RegistryCredentialsPath != "" {
		registryTemplate := &nomadapi.Template{
			DestPath:   stringPtr("/secrets/registry-creds"),
			ChangeMode: stringPtr("restart"),
			EmbeddedTmpl: stringPtr(fmt.Sprintf(`
{{- with secret "%s" -}}
export REGISTRY_USERNAME="{{ .Data.data.username }}"
export REGISTRY_PASSWORD="{{ .Data.data.password }}"
{{- end -}}
`, job.Config.RegistryCredentialsPath)),
		}
		templates = append(templates, registryTemplate)
	}

	// Add custom vault secrets for test phase
	if job.Config.Test != nil && len(job.Config.Test.VaultSecrets) > 0 {
		for i, vaultSecret := range job.Config.Test.VaultSecrets {
			template := generateVaultSecretTemplate(vaultSecret, i)
			templates = append(templates, template)
		}
	}

	return templates
}

// generateVaultSecretTemplate creates a Vault template for a custom secret
func generateVaultSecretTemplate(secret types.VaultSecret, index int) *nomadapi.Template {
	var tmplData strings.Builder
	tmplData.WriteString(fmt.Sprintf("{{- with secret \"%s\" -}}\n", secret.Path))

	for vaultField, envVar := range secret.Fields {
		tmplData.WriteString(fmt.Sprintf("export %s=\"{{ .Data.data.%s }}\"\n", envVar, vaultField))
	}

	tmplData.WriteString("{{- end -}}")

	return &nomadapi.Template{
		DestPath:     stringPtr(fmt.Sprintf("secrets/vault-%d.env", index)),
		ChangeMode:   stringPtr("restart"),
		EmbeddedTmpl: stringPtr(tmplData.String()),
		Envvars:      boolPtr(true), // Load secrets as environment variables
	}
}

// publishTemplates creates Vault templates for the publish phase (only registry credentials if needed)
func publishTemplates(job *types.Job) []*nomadapi.Template {
	var templates []*nomadapi.Template
	
	// Only add registry credentials template if path is provided and not empty
	if job.Config.RegistryCredentialsPath != "" {
		registryTemplate := &nomadapi.Template{
			DestPath:   stringPtr("/secrets/registry-creds"),
			ChangeMode: stringPtr("restart"),
			EmbeddedTmpl: stringPtr(fmt.Sprintf(`
{{- with secret "%s" -}}
export REGISTRY_USERNAME="{{ .Data.data.username }}"
export REGISTRY_PASSWORD="{{ .Data.data.password }}"
{{- end -}}
`, job.Config.RegistryCredentialsPath)),
		}
		templates = append(templates, registryTemplate)
	}
	
	return templates
}

// cleanupTemplates creates Vault templates for the cleanup phase (only registry credentials if needed)
func cleanupTemplates(job *types.Job) []*nomadapi.Template {
	var templates []*nomadapi.Template
	
	// Only add registry credentials template if path is provided and not empty
	if job.Config.RegistryCredentialsPath != "" {
		registryTemplate := &nomadapi.Template{
			DestPath:   stringPtr("/secrets/registry-creds"),
			ChangeMode: stringPtr("restart"),
			EmbeddedTmpl: stringPtr(fmt.Sprintf(`
{{- with secret "%s" -}}
export REGISTRY_USERNAME="{{ .Data.data.username }}"
export REGISTRY_PASSWORD="{{ .Data.data.password }}"
{{- end -}}
`, job.Config.RegistryCredentialsPath)),
		}
		templates = append(templates, registryTemplate)
	}
	
	return templates
}

// registryHost extracts the registry host from an image name
// e.g., "registry.cluster:5000/bdtemp/image:tag" -> "registry.cluster:5000"
func registryHost(imageName string) string {
	parts := strings.Split(imageName, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ":") {
		return parts[0]
	}
	return "docker.io" // default to Docker Hub if no registry specified
}

// buildTestConstraints builds the constraint list for test jobs
// buildTestEnv builds the environment variable map for test tasks
// Automatically sets NVIDIA_VISIBLE_DEVICES="all" when gpu_required=true unless user specified it
func (nc *Client) buildTestEnv(job *types.Job) map[string]string {
	env := make(map[string]string)
	
	// Copy user-provided environment variables
	if job.Config.Test != nil && job.Config.Test.Env != nil {
		for k, v := range job.Config.Test.Env {
			env[k] = v
		}
	}
	
	// Auto-set NVIDIA_VISIBLE_DEVICES if GPU is required and user didn't specify it
	if job.Config.Test != nil && job.Config.Test.GPURequired {
		if _, exists := env["NVIDIA_VISIBLE_DEVICES"]; !exists {
			env["NVIDIA_VISIBLE_DEVICES"] = "all"
		}
	}
	
	return env
}

func (nc *Client) buildTestConstraints(job *types.Job, buildNodeID string) []*nomadapi.Constraint {
	var constraints []*nomadapi.Constraint

	// Add build node avoidance constraint if build node ID is provided
	if buildNodeID != "" {
		constraints = append(constraints, &nomadapi.Constraint{
			LTarget: "${node.unique.id}",
			RTarget: buildNodeID,
			Operand: "!=",
		})
	}

	// Add GPU capability constraint if GPU is required
	if job.Config.Test != nil && job.Config.Test.GPURequired {
		constraints = append(constraints, &nomadapi.Constraint{
			LTarget: "${meta.gpu-capable}",
			RTarget: "true",
			Operand: "=",
		})

		// If pinning to specific node, use that instead of gpu-dedicated exclusion
		if job.Config.Test.GPUNodePin != "" {
			constraints = append(constraints, &nomadapi.Constraint{
				LTarget: "${node.unique.name}",
				RTarget: job.Config.Test.GPUNodePin,
				Operand: "=",
			})
		} else {
			// Exclude dedicated GPU nodes (used by ollama, nemotron, llama-swap)
			// Jobs should schedule on shared GPU nodes instead
			constraints = append(constraints, &nomadapi.Constraint{
				LTarget: "${meta.gpu-dedicated}",
				RTarget: "true",
				Operand: "!=",
			})
		}
	}

	// Add GPU compute capability constraint if specified (e.g., "7.5" for Turing or newer)
	if job.Config.Test != nil && job.Config.Test.GPUComputeCapability != "" {
		constraints = append(constraints, &nomadapi.Constraint{
			LTarget: "${meta.gpu_compute_capability}",
			RTarget: job.Config.Test.GPUComputeCapability,
			Operand: ">=",
		})
	}

	// Add custom constraints from test configuration
	if job.Config.Test != nil && len(job.Config.Test.Constraints) > 0 {
		for _, c := range job.Config.Test.Constraints {
			constraints = append(constraints, &nomadapi.Constraint{
				LTarget: c.Attribute,
				RTarget: c.Value,
				Operand: c.Operand,
			})
		}
	}

	return constraints
}

// configureTestDockerConfig configures the Docker config for test tasks
func (nc *Client) configureTestDockerConfig(job *types.Job, tempImageName string, testCmd string) map[string]interface{} {
	config := map[string]interface{}{
		"image":      tempImageName,
		"force_pull": true, // Force Docker to always pull fresh image
		"volumes": []string{
			"/etc/docker/certs.d:/etc/docker/certs.d:ro",
		},
	}

	// Add command if provided
	// IMPORTANT: We must set "entrypoint" to override the image's default ENTRYPOINT.
	// If we only set "command" (which maps to Docker's CMD), the test command would be
	// ignored or passed as arguments to the ENTRYPOINT when the image has one defined.
	// By setting entrypoint to ["sh", "-c"] and args to [testCmd], we ensure the test
	// command actually runs instead of the image's default startup process.
	if testCmd != "" {
		config["entrypoint"] = []string{"sh", "-c"}
		config["args"] = []string{testCmd}
	}

	// Add GPU runtime if required
	if job.Config.Test != nil && job.Config.Test.GPURequired {
		config["runtime"] = "nvidia"
	}

	// Add Docker logging configuration
	config["logging"] = map[string]interface{}{
		"type": "json-file",
		"config": []map[string]interface{}{
			{
				"max-file": fmt.Sprintf("%d", nc.config.Build.DockerLogMaxFiles),
				"max-size": nc.config.Build.DockerLogMaxFileSize,
			},
		},
	}

	return config
}

// buildTestResources builds the Resources specification for test tasks including GPU devices
func (nc *Client) buildTestResources(job *types.Job, cpu, memory, disk int) *nomadapi.Resources {
	// Note: We don't use Nomad's device allocation for GPUs because it requires
	// the Nvidia device plugin to be configured on Nomad nodes. Instead, we rely on:
	// 1. Docker's nvidia runtime (set in configureTestDockerConfig)
	// 2. NVIDIA_VISIBLE_DEVICES env var (set by user in test.env)
	// This approach works without requiring Nomad device plugins.

	resources := &nomadapi.Resources{
		CPU:      intPtr(cpu),
		MemoryMB: intPtr(memory),
		// Note: DiskMB is deprecated in Nomad API - disk is specified at ephemeral_disk level
	}

	return resources
}


// createPruneStorageJobSpec creates a Nomad job specification for pruning buildah storage cache
func (nc *Client) createPruneStorageJobSpec(req *types.PruneStorageRequest, pruneJobID string) *nomadapi.Job {
	// Determine job type: sysbatch for all nodes, batch for single node
	jobType := "batch"
	if req.AllNodes {
		jobType = "sysbatch"
	}

	// Build the prune commands
	pruneCommands := []string{
		"#!/bin/bash",
		"set -euo pipefail",
		"",
		"echo '=== Buildah Storage Prune Started ==='",
		"echo \"Node: $(cat /proc/sys/kernel/hostname 2>/dev/null || echo 'unknown')\"",
		"echo \"Time: $(date -Iseconds)\"",
		"echo ''",
		"",
		"# Storage location",
		"STORAGE_ROOT='/var/lib/containers'",
		"",
		"# Show current storage usage",
		"echo '=== Current Storage Usage ==='",
		"du -sh $STORAGE_ROOT 2>/dev/null || echo 'Storage directory not found'",
		"echo ''",
		"",
	}

	if req.Project != "" {
		// Project-specific prune
		sanitizedProject := req.Project
		pruneCommands = append(pruneCommands,
			fmt.Sprintf("echo '=== Pruning cache for project: %s ==='", sanitizedProject),
			fmt.Sprintf("PROJECT_DIR=\"$STORAGE_ROOT/%s\"", sanitizedProject),
			"",
			"if [ -d \"$PROJECT_DIR\" ]; then",
			"  echo \"Project cache directory: $PROJECT_DIR\"",
			"  du -sh \"$PROJECT_DIR\" 2>/dev/null || echo 'Unable to determine size'",
		)

		if req.DryRun {
			pruneCommands = append(pruneCommands,
				"  echo ''",
				"  echo '[DRY RUN] Would remove project cache directory'",
				"  echo \"[DRY RUN] Would free: $(du -sh \"$PROJECT_DIR\" 2>/dev/null | cut -f1)\"",
			)
		} else {
			pruneCommands = append(pruneCommands,
				"  echo ''",
				"  echo 'Removing project cache...'",
				"  rm -rf \"$PROJECT_DIR\"",
				"  echo 'Project cache removed successfully'",
			)
		}

		pruneCommands = append(pruneCommands,
			"else",
			"  echo 'Project cache directory not found'",
			"fi",
			"",
		)
	} else if req.All {
		// Aggressive full prune
		pruneCommands = append(pruneCommands,
			"echo '=== Aggressive Full Prune ==='",
			"echo 'WARNING: This will remove ALL cached images and build layers'",
			"echo ''",
		)

		if req.DryRun {
			pruneCommands = append(pruneCommands,
				"echo '[DRY RUN] Would execute:'",
				"echo '  - buildah rm --all'",
				"echo '  - buildah rmi --all --force'",
				"echo '  - rm -rf $STORAGE_ROOT/*'",
				"echo ''",
				"echo '[DRY RUN] Current usage that would be freed:'",
				"du -sh $STORAGE_ROOT/* 2>/dev/null || echo 'No subdirectories found'",
			)
		} else {
			pruneCommands = append(pruneCommands,
				"# Remove all build containers",
				"echo 'Removing all build containers...'",
				"buildah rm --all 2>/dev/null || echo 'No containers to remove'",
				"",
				"# Remove all images",
				"echo 'Removing all images...'",
				"buildah rmi --all --force 2>/dev/null || echo 'No images to remove'",
				"",
				"# Clean up storage directories",
				"echo 'Cleaning up storage directories...'",
				"rm -rf $STORAGE_ROOT/* 2>/dev/null || echo 'Storage already clean'",
				"",
				"echo 'Aggressive prune completed'",
			)
		}
	} else {
		// Conservative prune (default)
		pruneCommands = append(pruneCommands,
			"echo '=== Conservative Prune ==='",
			"echo 'Removing dangling images and caches older than 24 hours'",
			"echo ''",
		)

		if req.DryRun {
			pruneCommands = append(pruneCommands,
				"echo '[DRY RUN] Would execute:'",
				"echo '  - buildah rm --all (remove build containers)'",
				"echo '  - buildah rmi --prune (remove dangling images)'",
				"echo '  - buildah system prune (remove unused build cache)'",
				"echo ''",
				"echo '[DRY RUN] Current storage breakdown:'",
				"du -sh $STORAGE_ROOT/* 2>/dev/null || echo 'No subdirectories found'",
				"echo ''",
				"echo '[DRY RUN] Dangling images:'",
				"buildah images --filter dangling=true 2>/dev/null || echo 'Unable to list dangling images'",
			)
		} else {
			pruneCommands = append(pruneCommands,
				"# Remove build containers",
				"echo 'Removing build containers...'",
				"buildah rm --all 2>/dev/null || echo 'No containers to remove'",
				"",
				"# Remove dangling/unused images",
				"echo 'Removing dangling images...'",
				"buildah rmi --prune 2>/dev/null || echo 'No dangling images to prune'",
				"",
				"# Prune unused build cache",
				"echo 'Pruning unused build cache...'",
				"buildah system prune 2>/dev/null || echo 'No unused data to prune'",
				"",
				"echo 'Conservative prune completed'",
			)
		}
	}

	// Add final storage usage report
	pruneCommands = append(pruneCommands,
		"",
		"echo ''",
		"echo '=== Final Storage Usage ==='",
		"du -sh $STORAGE_ROOT 2>/dev/null || echo 'Storage directory empty or not found'",
		"echo ''",
		"echo '=== Prune Completed ==='",
	)

	// Build constraints for node targeting
	var constraints []*nomadapi.Constraint
	if req.NodeName != "" {
		constraints = append(constraints, &nomadapi.Constraint{
			LTarget: "${node.unique.name}",
			RTarget: req.NodeName,
			Operand: "=",
		})
	}

	jobSpec := &nomadapi.Job{
		ID:          &pruneJobID,
		Name:        &pruneJobID,
		Type:        stringPtr(jobType),
		Namespace:   stringPtr(nc.config.Nomad.Namespace),
		Region:      stringPtr(nc.config.Nomad.Region),
		Datacenters: nc.config.Nomad.Datacenters,
		Meta: map[string]string{
			"operation":  "prune-storage",
			"dry_run":    fmt.Sprintf("%t", req.DryRun),
			"all":        fmt.Sprintf("%t", req.All),
			"all_nodes":  fmt.Sprintf("%t", req.AllNodes),
		},
		TaskGroups: []*nomadapi.TaskGroup{
			{
				Name:        stringPtr("prune"),
				Count:       intPtr(1),
				Constraints: constraints,
				RestartPolicy: &nomadapi.RestartPolicy{
					Attempts: intPtr(0), // No restart for prune jobs
				},
				Tasks: []*nomadapi.Task{
					{
						Name:   "main",
						Driver: "docker",
						Config: map[string]interface{}{
							"image":      "quay.io/buildah/stable:latest",
							"command":    "/bin/bash",
							"args":       []string{"-c", strings.Join(pruneCommands, "\n")},
							"privileged": true, // Required for storage access
							"volumes": []string{
								"/opt/nomad/data/buildah-cache:/var/lib/containers:rw", // Persistent layer cache
							},
						},
						Env: map[string]string{
							"BUILDAH_ISOLATION": "chroot",
							"STORAGE_DRIVER":    "overlay",
						},
						Resources: &nomadapi.Resources{
							CPU:      intPtr(200),
							MemoryMB: intPtr(512),
						},
						KillTimeout: durationPtr("2m"),
						LogConfig: &nomadapi.LogConfig{
							MaxFiles:      intPtr(3),
							MaxFileSizeMB: intPtr(5),
						},
					},
				},
				EphemeralDisk: &nomadapi.EphemeralDisk{
					SizeMB: intPtr(100),
				},
			},
		},
	}

	// Add project metadata if specified
	if req.Project != "" {
		jobSpec.Meta["project"] = req.Project
	}

	// Add node_name metadata if specified
	if req.NodeName != "" {
		jobSpec.Meta["node_name"] = req.NodeName
	}

	return jobSpec
}

// createCleanupBuildahCacheJobSpec creates a Nomad job specification for cleaning up corrupted buildah storage
// This fixes "identifier is not a container" errors that occur when builds are interrupted
func (nc *Client) createCleanupBuildahCacheJobSpec(req *types.CleanupBuildahCacheRequest, cleanupJobID string) *nomadapi.Job {
	// Determine job type: sysbatch for all nodes, batch for single node
	jobType := "batch"
	if req.AllNodes {
		jobType = "sysbatch"
	}

	// Build the cleanup commands
	cleanupCommands := []string{
		"#!/bin/bash",
		"set -euo pipefail",
		"",
		"echo '=== Buildah Cache Cleanup Started ==='",
		"echo \"Node: $(cat /proc/sys/kernel/hostname 2>/dev/null || echo 'unknown')\"",
		"echo \"Time: $(date -Iseconds)\"",
		"echo ''",
		"",
		"# Storage location",
		"STORAGE_ROOT='/var/lib/containers'",
		"",
		"# Show current storage status",
		"echo '=== Current Storage Status ==='",
		"du -sh $STORAGE_ROOT 2>/dev/null || echo 'Storage directory not found'",
		"echo ''",
		"",
		"# Check for corrupted state",
		"echo '=== Checking for Corrupted Buildah State ==='",
		"echo 'Listing containers (may show errors if corrupted):'",
		"buildah containers 2>&1 || echo 'Container list command failed - database may be corrupted'",
		"echo ''",
		"",
	}

	if req.Full {
		// Full reset mode - clear all storage
		cleanupCommands = append(cleanupCommands,
			"echo '=== Full Reset Mode ==='",
			"echo 'WARNING: This will remove ALL cached images and build layers'",
			"echo ''",
		)

		if req.DryRun {
			cleanupCommands = append(cleanupCommands,
				"echo '[DRY RUN] Would execute:'",
				"echo '  - buildah rm --all'",
				"echo '  - buildah rmi --all --force'",
				"echo '  - rm -rf $STORAGE_ROOT/storage/overlay-containers/*'",
				"echo '  - rm -rf $STORAGE_ROOT/storage/overlay-images/*'",
				"echo '  - rm -rf $STORAGE_ROOT/storage/overlay-layers/*'",
				"echo ''",
				"echo '[DRY RUN] Current storage breakdown:'",
				"du -sh $STORAGE_ROOT/storage/* 2>/dev/null || echo 'No storage subdirectories found'",
				"echo ''",
				"echo '[DRY RUN] Orphaned lock files:'",
				"find $STORAGE_ROOT -name '*.lock' 2>/dev/null || echo 'No lock files found'",
			)
		} else {
			cleanupCommands = append(cleanupCommands,
				"# Step 1: Remove all container entries from database",
				"echo 'Removing all container entries...'",
				"buildah rm --all 2>/dev/null || echo 'buildah rm failed (expected if database corrupted)'",
				"",
				"# Step 2: Remove all images",
				"echo 'Removing all images...'",
				"buildah rmi --all --force 2>/dev/null || echo 'buildah rmi failed (database may be corrupted)'",
				"",
				"# Step 3: Clean up storage directories directly",
				"echo 'Cleaning up overlay storage directories...'",
				"rm -rf $STORAGE_ROOT/storage/overlay-containers/* 2>/dev/null || echo 'overlay-containers already clean'",
				"rm -rf $STORAGE_ROOT/storage/overlay-images/* 2>/dev/null || echo 'overlay-images already clean'",
				"rm -rf $STORAGE_ROOT/storage/overlay-layers/* 2>/dev/null || echo 'overlay-layers already clean'",
				"",
				"# Step 4: Remove orphaned lock files",
				"echo 'Removing orphaned lock files...'",
				"find $STORAGE_ROOT -name '*.lock' -delete 2>/dev/null || echo 'No lock files to remove'",
				"",
				"# Step 5: Reset the container database",
				"echo 'Resetting container database...'",
				"rm -f $STORAGE_ROOT/storage/libpod/bolt_state.db 2>/dev/null || echo 'No libpod database found'",
				"rm -rf $STORAGE_ROOT/storage/overlay-containers 2>/dev/null",
				"mkdir -p $STORAGE_ROOT/storage/overlay-containers",
				"",
				"echo 'Full reset completed'",
			)
		}
	} else {
		// Standard cleanup mode - fix corrupted entries without removing all data
		cleanupCommands = append(cleanupCommands,
			"echo '=== Standard Cleanup Mode ==='",
			"echo 'Fixing corrupted container database entries'",
			"echo ''",
		)

		if req.DryRun {
			cleanupCommands = append(cleanupCommands,
				"echo '[DRY RUN] Would execute:'",
				"echo '  - buildah rm --all (remove container entries from database)'",
				"echo '  - find $STORAGE_ROOT -name *.lock -delete (remove orphaned locks)'",
				"echo ''",
				"echo '[DRY RUN] Current containers in database:'",
				"buildah containers 2>&1 || echo 'Unable to list containers (database may be corrupted)'",
				"echo ''",
				"echo '[DRY RUN] Orphaned lock files:'",
				"find $STORAGE_ROOT -name '*.lock' -type f 2>/dev/null | wc -l | xargs -I{} echo '{} lock files found'",
				"find $STORAGE_ROOT -name '*.lock' -type f 2>/dev/null | head -10",
			)
		} else {
			cleanupCommands = append(cleanupCommands,
				"# Step 1: Remove all container entries from the database",
				"# This clears the \"identifier is not a container\" errors",
				"echo 'Removing container entries from database...'",
				"buildah rm --all 2>&1 || echo 'buildah rm encountered errors (expected if corrupted)'",
				"",
				"# Step 2: Remove orphaned lock files",
				"# These can prevent new builds from starting",
				"echo 'Removing orphaned lock files...'",
				"LOCK_COUNT=$(find $STORAGE_ROOT -name '*.lock' -type f 2>/dev/null | wc -l)",
				"echo \"Found $LOCK_COUNT lock files\"",
				"find $STORAGE_ROOT -name '*.lock' -type f -delete 2>/dev/null || echo 'No lock files to remove'",
				"echo \"Removed $LOCK_COUNT lock files\"",
				"",
				"# Step 3: Verify the fix",
				"echo ''",
				"echo '=== Verification ==='",
				"echo 'Listing containers after cleanup:'",
				"buildah containers 2>&1 && echo 'Container list successful - database is healthy' || echo 'Container list still failing'",
				"",
				"echo 'Standard cleanup completed'",
			)
		}
	}

	// Add final storage usage report
	cleanupCommands = append(cleanupCommands,
		"",
		"echo ''",
		"echo '=== Final Storage Status ==='",
		"du -sh $STORAGE_ROOT 2>/dev/null || echo 'Storage directory empty or not found'",
		"echo ''",
		"echo '=== Cleanup Completed ==='",
	)

	// Build constraints for node targeting
	var constraints []*nomadapi.Constraint
	if req.NodeName != "" {
		constraints = append(constraints, &nomadapi.Constraint{
			LTarget: "${node.unique.name}",
			RTarget: req.NodeName,
			Operand: "=",
		})
	}

	jobSpec := &nomadapi.Job{
		ID:          &cleanupJobID,
		Name:        &cleanupJobID,
		Type:        stringPtr(jobType),
		Namespace:   stringPtr(nc.config.Nomad.Namespace),
		Region:      stringPtr(nc.config.Nomad.Region),
		Datacenters: nc.config.Nomad.Datacenters,
		Meta: map[string]string{
			"operation": "cleanup-buildah-cache",
			"dry_run":   fmt.Sprintf("%t", req.DryRun),
			"full":      fmt.Sprintf("%t", req.Full),
			"all_nodes": fmt.Sprintf("%t", req.AllNodes),
		},
		TaskGroups: []*nomadapi.TaskGroup{
			{
				Name:        stringPtr("cleanup"),
				Count:       intPtr(1),
				Constraints: constraints,
				RestartPolicy: &nomadapi.RestartPolicy{
					Attempts: intPtr(0), // No restart for cleanup jobs
				},
				Tasks: []*nomadapi.Task{
					{
						Name:   "main",
						Driver: "docker",
						Config: map[string]interface{}{
							"image":      "quay.io/buildah/stable:latest",
							"command":    "/bin/bash",
							"args":       []string{"-c", strings.Join(cleanupCommands, "\n")},
							"privileged": true, // Required for storage access
							"volumes": []string{
								"/opt/nomad/data/buildah-cache:/var/lib/containers:rw", // Persistent layer cache
							},
						},
						Env: map[string]string{
							"BUILDAH_ISOLATION": "chroot",
							"STORAGE_DRIVER":    "overlay",
						},
						Resources: &nomadapi.Resources{
							CPU:      intPtr(200),
							MemoryMB: intPtr(512),
						},
						KillTimeout: durationPtr("2m"),
						LogConfig: &nomadapi.LogConfig{
							MaxFiles:      intPtr(3),
							MaxFileSizeMB: intPtr(5),
						},
					},
				},
				EphemeralDisk: &nomadapi.EphemeralDisk{
					SizeMB: intPtr(100),
				},
			},
		},
	}

	// Add node_name metadata if specified
	if req.NodeName != "" {
		jobSpec.Meta["node_name"] = req.NodeName
	}

	return jobSpec
}