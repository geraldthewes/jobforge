package nomad

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/sirupsen/logrus"
	
	"nomad-mcp-builder/internal/config"
	"nomad-mcp-builder/pkg/types"
)

// Client wraps the Nomad API client with build service specific functionality
type Client struct {
	client  *nomadapi.Client
	config  *config.Config
	logger  *logrus.Logger
	storage interface {
		AcquireLock(lockKey string, timeout time.Duration) (string, error)
		ReleaseLock(lockKey, sessionID string) error
		GenerateImageLockKey(registryURL, imageName, branch string) string
	}
}

// NewClient creates a new Nomad client
func NewClient(cfg *config.Config, storage interface {
	AcquireLock(lockKey string, timeout time.Duration) (string, error)
	ReleaseLock(lockKey, sessionID string) error
	GenerateImageLockKey(registryURL, imageName, branch string) string
}) (*Client, error) {
	nomadConfig := nomadapi.DefaultConfig()
	nomadConfig.Address = cfg.Nomad.Address
	nomadConfig.SecretID = cfg.Nomad.Token
	nomadConfig.Region = cfg.Nomad.Region
	nomadConfig.Namespace = cfg.Nomad.Namespace
	
	client, err := nomadapi.NewClient(nomadConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Nomad client: %w", err)
	}
	
	logger := logrus.New()
	
	// Set log level from configuration
	level, err := logrus.ParseLevel(cfg.Logging.Level)
	if err != nil {
		logger.WithField("log_level", cfg.Logging.Level).Warn("Invalid log level in nomad client, using info")
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	
	return &Client{
		client:  client,
		config:  cfg,
		logger:  logger,
		storage: storage,
	}, nil
}

// CreateJob creates a new build job and starts the build phase
func (nc *Client) CreateJob(jobConfig *types.JobConfig) (*types.Job, error) {
	// Generate lock key for this image build
	lockKey := nc.storage.GenerateImageLockKey(jobConfig.RegistryURL, jobConfig.ImageName, jobConfig.GitRef)

	// Try to acquire lock for this image/branch combination
	sessionID, err := nc.storage.AcquireLock(lockKey, 30*time.Minute)
	if err != nil {
		nc.logger.WithFields(logrus.Fields{
			"lock_key":      lockKey,
			"registry_url":  jobConfig.RegistryURL,
			"image_name":    jobConfig.ImageName,
			"git_ref":       jobConfig.GitRef,
		}).Warn("Failed to acquire build lock - another build may be in progress")
		return nil, fmt.Errorf("cannot start build: %w", err)
	}

	nc.logger.WithFields(logrus.Fields{
		"lock_key":      lockKey,
		"session_id":    sessionID,
		"registry_url":  jobConfig.RegistryURL,
		"image_name":    jobConfig.ImageName,
		"git_ref":       jobConfig.GitRef,
	}).Info("Build lock acquired successfully")

	jobID := uuid.New().String()
	now := time.Now()

	// Always include job-id as a tag for traceability
	if len(jobConfig.ImageTags) == 0 {
		// No tags provided, use only job-id
		jobConfig.ImageTags = []string{jobID}
		nc.logger.WithFields(logrus.Fields{
			"job_id": jobID,
		}).Info("No image_tags provided, using job-id as tag")
	} else {
		// Tags provided, prepend job-id for traceability
		jobConfig.ImageTags = append([]string{jobID}, jobConfig.ImageTags...)
		nc.logger.WithFields(logrus.Fields{
			"job_id": jobID,
			"custom_tags": jobConfig.ImageTags[1:],
		}).Info("Prepending job-id to custom image tags for traceability")
	}

	job := &types.Job{
		ID:        jobID,
		Config:    *jobConfig,
		Status:    types.StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
		Logs:      types.JobLogs{},
		Metrics:   types.JobMetrics{
			JobStart: &now,
		},
		// Store lock information for cleanup
		LockKey:       lockKey,
		LockSessionID: sessionID,
	}
	
	// Create and submit the build job to Nomad
	buildJobSpec, err := nc.createBuildJobSpec(job)
	if err != nil {
		// Release lock if job spec creation fails
		if releaseErr := nc.storage.ReleaseLock(lockKey, sessionID); releaseErr != nil {
			nc.logger.WithError(releaseErr).Warn("Failed to release lock after job spec creation failure")
		}
		return nil, fmt.Errorf("failed to create build job spec: %w", err)
	}
	
	// Log the job specification for debugging
	nc.logJobSpec(buildJobSpec, "build")
	
	// Use proper WriteOptions and RegisterOpts to match CLI behavior
	registerOpts := &nomadapi.RegisterOptions{
		PolicyOverride: false,
		PreserveCounts: false,
	}
	writeOpts := &nomadapi.WriteOptions{
		Region:    nc.config.Nomad.Region,
		Namespace: nc.config.Nomad.Namespace,
	}
	
	evalID, _, err := nc.client.Jobs().RegisterOpts(buildJobSpec, registerOpts, writeOpts)
	if err != nil {
		// Release lock if job registration fails
		if releaseErr := nc.storage.ReleaseLock(lockKey, sessionID); releaseErr != nil {
			nc.logger.WithError(releaseErr).Warn("Failed to release lock after job registration failure")
		}

		// Check for specific Vault template errors and provide better feedback
		errorMsg := err.Error()
		if strings.Contains(errorMsg, "Template failed") && strings.Contains(errorMsg, "vault.read: invalid format") {
			return nil, fmt.Errorf("vault template error: empty or invalid secret path provided. Please check that GitCredentialsPath and RegistryCredentialsPath are valid Vault paths or leave them empty if not needed. Original error: %w", err)
		}
		if strings.Contains(errorMsg, "Template failed") && strings.Contains(errorMsg, "vault.read") {
			return nil, fmt.Errorf("vault template error: failed to read secret from Vault. Please verify the secret path exists and the service has proper permissions. Original error: %w", err)
		}
		return nil, fmt.Errorf("failed to submit build job to Nomad: %w", err)
	}
	
	job.BuildJobID = *buildJobSpec.ID
	job.Status = types.StatusBuilding
	job.StartedAt = &now
	job.Metrics.BuildStart = &now
	
	nc.logger.WithFields(logrus.Fields{
		"job_id":      jobID,
		"nomad_job":   *buildJobSpec.ID,
		"eval_id":     evalID,
	}).Info("Build job submitted to Nomad")
	
	return job, nil
}

// UpdateJobStatus updates the job status by querying Nomad
func (nc *Client) UpdateJobStatus(job *types.Job) (*types.Job, error) {
	// Check if tests are configured (including external python tests)
	skipTests := job.Config.Test == nil || (len(job.Config.Test.Commands) == 0 && !job.Config.Test.EntryPoint && job.Config.Test.PythonCommand == "")

	// Check if this is an external (python) test job
	isExternalTest := job.Config.Test != nil && job.Config.Test.PythonCommand != ""
	
	// Check build job status
	if job.BuildJobID != "" {
		buildStatus, err := nc.getJobStatus(job.BuildJobID)
		if err != nil {
			return job, fmt.Errorf("failed to get build job status: %w", err)
		}
		
		switch buildStatus {
		case "running":
			job.Status = types.StatusBuilding
			job.CurrentPhase = "build"
		case "complete":
			// Build completed, capture logs before they disappear
			if err := nc.capturePhaseLogs(job, "build"); err != nil {
				nc.logger.WithError(err).Warn("Failed to capture build logs")
			}
			
			// Mark build phase as complete (only set end time if not already set)
			now := time.Now()
			if job.Metrics.BuildEnd == nil {
				job.Metrics.BuildEnd = &now
				if job.Metrics.BuildStart != nil {
					job.Metrics.BuildDuration = now.Sub(*job.Metrics.BuildStart)
				}
			}
			
			if skipTests {
				// No tests configured - build pushed directly to final tags, mark as succeeded
				nc.logger.WithField("job_id", job.ID).Info("Build completed with no tests configured, marking job as succeeded")
				job.Status = types.StatusSucceeded
				job.CurrentPhase = "build"
				job.FinishedAt = &now
				job.Metrics.JobEnd = &now
				// Calculate total duration
				if job.Metrics.JobStart != nil {
					job.Metrics.TotalDuration = now.Sub(*job.Metrics.JobStart)
				}
				// Release build lock since job is complete
				nc.releaseBuildLock(job)

				// No temporary images to clean up since we built directly to final tags
			} else {
				// Start test phase with a small delay to avoid Docker layer race conditions
				if job.Status == types.StatusBuilding {
					// Add a brief delay to ensure Docker daemon has finished cleaning up build layers
					// This helps prevent "file exists" errors when test jobs try to pull the same image
					nc.logger.WithField("job_id", job.ID).Info("Build completed, waiting 3 seconds before starting tests to avoid Docker layer conflicts")
					time.Sleep(3 * time.Second)
					
					if err := nc.startTestPhase(job); err != nil {
						job.Status = types.StatusFailed
						job.CurrentPhase = "build"
						job.Error = fmt.Sprintf("Failed to start test phase: %v", err)
						job.FinishedAt = &now
						job.Metrics.JobEnd = &now
						// Release build lock since job failed
						nc.releaseBuildLock(job)
					} else {
						job.Status = types.StatusTesting
						job.CurrentPhase = "test"
						job.Metrics.TestStart = &now
					}
				}
			}
		case "failed":
			// Capture logs from failed build before they disappear
			if err := nc.capturePhaseLogs(job, "build"); err != nil {
				nc.logger.WithError(err).Warn("Failed to capture failed build logs")
			}

			job.Status = types.StatusFailed
			job.CurrentPhase = "build"
			job.FailedPhase = "build"
			// Get detailed error information
			errorMsg, err := nc.getJobErrorDetails(job.BuildJobID)
			if err != nil {
				job.Error = "Build phase failed - unable to get error details"
			} else {
				job.Error = fmt.Sprintf("Build phase failed: %s", errorMsg)
			}
			now := time.Now()
			job.FinishedAt = &now
			job.Metrics.BuildEnd = &now
			job.Metrics.JobEnd = &now
			if job.Metrics.BuildStart != nil {
				job.Metrics.BuildDuration = now.Sub(*job.Metrics.BuildStart)
			}
			// Release build lock since job failed
			nc.releaseBuildLock(job)

			// Cleanup temporary images from failed build if tests were configured
			if !skipTests {
				nc.logger.WithField("job_id", job.ID).Info("Cleaning up temporary images from failed build")
				if err := nc.cleanupTempImages(job); err != nil {
					nc.logger.WithError(err).Warn("Failed to cleanup temporary images after build failure")
				}
			}
		}
	}
	
	// Check test job status (now supporting multiple test jobs)
	if len(job.TestJobIDs) > 0 {
		allComplete := true
		anyRunning := false
		anyFailed := false
		var failedJobID string
		
		for _, testJobID := range job.TestJobIDs {
			testStatus, err := nc.getJobStatus(testJobID)
			if err != nil {
				return job, fmt.Errorf("failed to get test job status for %s: %w", testJobID, err)
			}
			
			switch testStatus {
			case "running":
				anyRunning = true
				allComplete = false
			case "complete":
				// This test job completed successfully
				continue
			case "failed":
				anyFailed = true
				allComplete = false
				failedJobID = testJobID
			default:
				allComplete = false
			}
		}
		
		// Update job status based on test job states
		if anyRunning {
			job.Status = types.StatusTesting
			job.CurrentPhase = "test"
		} else if anyFailed {
			// If any test failed, capture logs from all test jobs before they disappear
			if err := nc.capturePhaseLogs(job, "test"); err != nil {
				nc.logger.WithError(err).Warn("Failed to capture failed test logs")
			}

			job.Status = types.StatusFailed
			job.CurrentPhase = "test"
			job.FailedPhase = "test"
			// Get detailed error information from the failed job
			errorMsg, err := nc.getJobErrorDetails(failedJobID)
			if err != nil {
				job.Error = fmt.Sprintf("Test phase failed - job %s failed but unable to get error details", failedJobID)
			} else {
				job.Error = fmt.Sprintf("Test phase failed in job %s: %s", failedJobID, errorMsg)
			}
			now := time.Now()
			job.FinishedAt = &now
			job.Metrics.TestEnd = &now
			job.Metrics.JobEnd = &now
			if job.Metrics.TestStart != nil {
				job.Metrics.TestDuration = now.Sub(*job.Metrics.TestStart)
			}
			// Release build lock since job failed
			nc.releaseBuildLock(job)

			// Cleanup temporary images from failed tests
			nc.logger.WithField("job_id", job.ID).Info("Cleaning up temporary images from failed test")
			if err := nc.cleanupTempImages(job); err != nil {
				nc.logger.WithError(err).Warn("Failed to cleanup temporary images after test failure")
			}
		} else if allComplete {
			// All tests completed successfully, capture logs before they disappear
			if err := nc.capturePhaseLogs(job, "test"); err != nil {
				nc.logger.WithError(err).Warn("Failed to capture test logs")
			}
			
			// Mark test phase as complete (only set end time if not already set)
			now := time.Now()
			if job.Metrics.TestEnd == nil {
				job.Metrics.TestEnd = &now
				if job.Metrics.TestStart != nil {
					job.Metrics.TestDuration = now.Sub(*job.Metrics.TestStart)
				}
			}
			
			// Start publish phase
			if job.Status == types.StatusTesting {
				if err := nc.startPublishPhase(job); err != nil {
					job.Status = types.StatusFailed
					job.CurrentPhase = "test"
					job.Error = fmt.Sprintf("Failed to start publish phase: %v", err)
					job.FinishedAt = &now
					job.Metrics.JobEnd = &now
					// Release build lock since job failed
					nc.releaseBuildLock(job)
				} else {
					job.Status = types.StatusPublishing
					job.CurrentPhase = "publish"
					job.Metrics.PublishStart = &now
				}
			}
		}
	}

	// Handle external (python) test job status
	if isExternalTest && job.TestJobNomadID != "" {
		testStatus, err := nc.getJobStatus(job.TestJobNomadID)
		if err != nil {
			return job, fmt.Errorf("failed to get external test job status: %w", err)
		}

		switch testStatus {
		case "running":
			// Check if we can get the endpoint info
			if job.TestServiceHost == "" || job.TestServicePort == 0 {
				host, port, err := nc.GetExternalTestEndpoint(job)
				if err == nil && host != "" && port > 0 {
					job.TestServiceHost = host
					job.TestServicePort = port
					job.Status = types.StatusTestingExternal
					job.CurrentPhase = "test"
					nc.logger.WithFields(logrus.Fields{
						"job_id":       job.ID,
						"service_host": host,
						"service_port": port,
					}).Info("External test container endpoint discovered, waiting for CLI to run tests")
				} else {
					// Container still starting, endpoint not yet available
					job.Status = types.StatusTesting
					job.CurrentPhase = "test"
				}
			} else {
				// Endpoint already known, keep waiting for CLI to report results
				job.Status = types.StatusTestingExternal
				job.CurrentPhase = "test"
			}
		case "failed":
			// External test container failed to start or crashed
			if err := nc.capturePhaseLogs(job, "test"); err != nil {
				nc.logger.WithError(err).Warn("Failed to capture external test logs")
			}

			job.Status = types.StatusFailed
			job.CurrentPhase = "test"
			job.FailedPhase = "test"
			errorMsg, err := nc.getJobErrorDetails(job.TestJobNomadID)
			if err != nil {
				job.Error = "External test container failed - unable to get error details"
			} else {
				job.Error = fmt.Sprintf("External test container failed: %s", errorMsg)
			}
			now := time.Now()
			job.FinishedAt = &now
			job.Metrics.TestEnd = &now
			job.Metrics.JobEnd = &now
			if job.Metrics.TestStart != nil {
				job.Metrics.TestDuration = now.Sub(*job.Metrics.TestStart)
			}
			nc.releaseBuildLock(job)
			nc.cleanupTempImages(job)
		}
		// Note: "complete" status won't happen since it's a service job
		// The CLI reports test results via the API, which then stops the job
	}

	// Check publish job status
	if job.PublishJobID != "" {
		publishStatus, err := nc.getJobStatus(job.PublishJobID)
		if err != nil {
			return job, fmt.Errorf("failed to get publish job status: %w", err)
		}
		
		switch publishStatus {
		case "running":
			job.Status = types.StatusPublishing
			job.CurrentPhase = "publish"
		case "complete":
			// Publish completed, capture logs before they disappear
			if err := nc.capturePhaseLogs(job, "publish"); err != nil {
				nc.logger.WithError(err).Warn("Failed to capture publish logs")
			}

			job.Status = types.StatusSucceeded
			job.CurrentPhase = "publish"
			now := time.Now()
			job.FinishedAt = &now

			// Mark publish phase as complete (only set end time if not already set)
			if job.Metrics.PublishEnd == nil {
				job.Metrics.PublishEnd = &now
				if job.Metrics.PublishStart != nil {
					job.Metrics.PublishDuration = now.Sub(*job.Metrics.PublishStart)
				}
			}

			job.Metrics.JobEnd = &now
			// Calculate total duration
			if job.Metrics.JobStart != nil {
				job.Metrics.TotalDuration = now.Sub(*job.Metrics.JobStart)
			}
			// Release build lock since job is complete
			nc.releaseBuildLock(job)

			// Automatically cleanup temporary images after successful publish
			nc.logger.WithField("job_id", job.ID).Info("Starting automatic cleanup of temporary images after successful publish")
			if err := nc.cleanupTempImages(job); err != nil {
				nc.logger.WithError(err).Warn("Failed to automatically cleanup temporary images")
			}
		case "failed":
			// Capture logs from failed publish before they disappear
			if err := nc.capturePhaseLogs(job, "publish"); err != nil {
				nc.logger.WithError(err).Warn("Failed to capture failed publish logs")
			}

			job.Status = types.StatusFailed
			job.CurrentPhase = "publish"
			job.FailedPhase = "publish"
			// Get detailed error information
			errorMsg, err := nc.getJobErrorDetails(job.PublishJobID)
			if err != nil {
				job.Error = "Publish phase failed - unable to get error details"
			} else {
				job.Error = fmt.Sprintf("Publish phase failed: %s", errorMsg)
			}
			now := time.Now()
			job.FinishedAt = &now
			job.Metrics.PublishEnd = &now
			job.Metrics.JobEnd = &now
			if job.Metrics.PublishStart != nil {
				job.Metrics.PublishDuration = now.Sub(*job.Metrics.PublishStart)
			}
			// Calculate total duration
			if job.Metrics.JobStart != nil {
				job.Metrics.TotalDuration = now.Sub(*job.Metrics.JobStart)
			}
			// Release build lock since job failed
			nc.releaseBuildLock(job)

			// Cleanup temporary images from failed publish
			nc.logger.WithField("job_id", job.ID).Info("Cleaning up temporary images from failed publish")
			if err := nc.cleanupTempImages(job); err != nil {
				nc.logger.WithError(err).Warn("Failed to cleanup temporary images after publish failure")
			}
		}
	}
	
	job.UpdatedAt = time.Now()
	return job, nil
}

// discoverTestJobs finds test jobs by querying Nomad for jobs matching the expected naming pattern
func (nc *Client) discoverTestJobs(jobID string) ([]string, error) {
	jobs, _, err := nc.client.Jobs().List(&nomadapi.QueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}
	
	var testJobIDs []string
	testEntryPrefix := fmt.Sprintf("test-entry-%s", jobID)
	testCmdPrefix := fmt.Sprintf("test-cmd-%s", jobID)
	
	for _, job := range jobs {
		if strings.HasPrefix(job.ID, testEntryPrefix) || strings.HasPrefix(job.ID, testCmdPrefix) {
			testJobIDs = append(testJobIDs, job.ID)
		}
	}
	
	return testJobIDs, nil
}

// GetJobLogs retrieves logs from Nomad for all phases
func (nc *Client) GetJobLogs(job *types.Job) (types.JobLogs, error) {
	logs := types.JobLogs{
		Build:   []string{},
		Test:    []string{},
		Publish: []string{},
	}
	
	// Get build logs
	if job.BuildJobID != "" {
		buildLogs, err := nc.getJobLogs(job.BuildJobID)
		if err != nil {
			nc.logger.WithError(err).Warn("Failed to get build logs")
		} else {
			logs.Build = buildLogs
		}
	}
	
	// Get test logs from all test jobs
	
	if len(job.TestJobIDs) > 0 {
		var allTestLogs []string
		for i, testJobID := range job.TestJobIDs {
			testLogs, err := nc.getJobLogs(testJobID)
			if err != nil {
				nc.logger.WithError(err).WithField("test_job_id", testJobID).Warn("Failed to get test logs")
				allTestLogs = append(allTestLogs, fmt.Sprintf("Failed to get logs for test job %d (%s): %v", i, testJobID, err))
			} else {
				// Add header to distinguish between different test jobs
				allTestLogs = append(allTestLogs, fmt.Sprintf("=== Test Job %d (%s) ===", i, testJobID))
				allTestLogs = append(allTestLogs, testLogs...)
				allTestLogs = append(allTestLogs, "") // Empty line for separation
			}
		}
		logs.Test = allTestLogs
	} else {
		// Fallback: try to discover test jobs if TestJobIDs is empty
		nc.logger.WithField("job_id", job.ID).Debug("GetJobLogs: TestJobIDs empty, attempting discovery")
		discoveredTestJobs, err := nc.discoverTestJobs(job.ID)
		if err != nil {
			nc.logger.WithError(err).Warn("GetJobLogs: Failed to discover test jobs")
		} else if len(discoveredTestJobs) > 0 {
			nc.logger.WithFields(logrus.Fields{
				"job_id": job.ID,
				"discovered_test_jobs": discoveredTestJobs,
			}).Info("GetJobLogs: Found test jobs via discovery")
			
			var allTestLogs []string
			for i, testJobID := range discoveredTestJobs {
				testLogs, err := nc.getJobLogs(testJobID)
				if err != nil {
					nc.logger.WithError(err).WithField("test_job_id", testJobID).Error("GetJobLogs: Failed to get discovered test logs")
					allTestLogs = append(allTestLogs, fmt.Sprintf("Failed to get logs for test job %d (%s): %v", i, testJobID, err))
				} else {
					// Add header to distinguish between different test jobs
					allTestLogs = append(allTestLogs, fmt.Sprintf("=== Test Job %d (%s) ===", i, testJobID))
					allTestLogs = append(allTestLogs, testLogs...)
					allTestLogs = append(allTestLogs, "") // Empty line for separation
				}
			}
			logs.Test = allTestLogs
		} else {
			nc.logger.WithField("job_id", job.ID).Debug("GetJobLogs: No test jobs found via discovery")
		}
	}
	
	// Get publish logs
	if job.PublishJobID != "" {
		publishLogs, err := nc.getJobLogs(job.PublishJobID)
		if err != nil {
			nc.logger.WithError(err).Warn("Failed to get publish logs")
		} else {
			logs.Publish = publishLogs
		}
	}
	
	return logs, nil
}

// KillJob terminates a running job
func (nc *Client) KillJob(job *types.Job) error {
	var errors []string
	
	nc.logger.WithField("job_id", job.ID).Info("Starting graceful job termination")
	
	// First, try to gracefully stop jobs by setting them to stop (allows completion)
	// This is safer than hard deregistration during build operations
	
	// Handle build job
	if job.BuildJobID != "" {
		buildStatus, err := nc.getJobStatus(job.BuildJobID)
		if err == nil && buildStatus == "running" {
			nc.logger.WithField("build_job_id", job.BuildJobID).Info("Gracefully stopping build job")
			
			// Try graceful stop first (allows current operations to complete)
			if _, _, err := nc.client.Jobs().Deregister(job.BuildJobID, false, nil); err != nil {
				nc.logger.WithError(err).Warn("Graceful stop failed, forcing termination")
				// Force stop if graceful fails
				if _, _, err := nc.client.Jobs().Deregister(job.BuildJobID, true, nil); err != nil {
					errors = append(errors, fmt.Sprintf("build: %v", err))
				}
			} else {
				// Wait briefly for graceful shutdown
				time.Sleep(2 * time.Second)
			}
		}
	}
	
	// Handle test jobs
	for _, testJobID := range job.TestJobIDs {
		testStatus, err := nc.getJobStatus(testJobID)
		if err == nil && testStatus == "running" {
			nc.logger.WithField("test_job_id", testJobID).Info("Gracefully stopping test job")
			
			if _, _, err := nc.client.Jobs().Deregister(testJobID, false, nil); err != nil {
				nc.logger.WithError(err).Warn("Graceful stop failed, forcing termination")
				if _, _, err := nc.client.Jobs().Deregister(testJobID, true, nil); err != nil {
					errors = append(errors, fmt.Sprintf("test job %s: %v", testJobID, err))
				}
			}
		}
	}
	
	// Handle publish job
	if job.PublishJobID != "" {
		publishStatus, err := nc.getJobStatus(job.PublishJobID)
		if err == nil && publishStatus == "running" {
			nc.logger.WithField("publish_job_id", job.PublishJobID).Info("Gracefully stopping publish job")
			
			if _, _, err := nc.client.Jobs().Deregister(job.PublishJobID, false, nil); err != nil {
				nc.logger.WithError(err).Warn("Graceful stop failed, forcing termination")
				if _, _, err := nc.client.Jobs().Deregister(job.PublishJobID, true, nil); err != nil {
					errors = append(errors, fmt.Sprintf("publish: %v", err))
				}
			}
		}
	}
	
	// Release the build lock since job is being terminated
	nc.releaseBuildLock(job)

	// Update job status to FAILED (only if not already failed with a specific error)
	now := time.Now()
	if job.Status != types.StatusFailed {
		job.Status = types.StatusFailed
		job.Error = "Job was killed by user request"
	}
	if job.FinishedAt == nil {
		job.FinishedAt = &now
	}
	if job.Metrics.JobEnd == nil {
		job.Metrics.JobEnd = &now
	}
	
	if len(errors) > 0 {
		return fmt.Errorf("failed to kill some jobs: %s", strings.Join(errors, ", "))
	}
	
	nc.logger.WithField("job_id", job.ID).Info("Job termination completed")
	return nil
}

// CleanupJob cleans up resources for a job
func (nc *Client) CleanupJob(job *types.Job) error {
	// First kill the job if it's still running
	if err := nc.KillJob(job); err != nil {
		nc.logger.WithError(err).Warn("Failed to kill job during cleanup")
	}
	
	// Clean up any temporary registry images
	if err := nc.cleanupTempImages(job); err != nil {
		nc.logger.WithError(err).Warn("Failed to cleanup temporary images")
	}
	
	return nil
}

// CleanupFailedJobs removes dead/failed jobs from Nomad after capturing their details
func (nc *Client) CleanupFailedJobs(job *types.Job) error {
	var errors []string
	
	// Clean up build job if it's dead/failed
	if job.BuildJobID != "" {
		if status, err := nc.getJobStatus(job.BuildJobID); err == nil && (status == "failed" || status == "dead") {
			// Purge (force remove) the dead job from Nomad
			if _, _, err := nc.client.Jobs().Deregister(job.BuildJobID, true, &nomadapi.WriteOptions{}); err != nil {
				errors = append(errors, fmt.Sprintf("build job cleanup: %v", err))
			} else {
				nc.logger.WithField("job_id", job.BuildJobID).Info("Cleaned up failed build job from Nomad")
			}
		}
	}
	
	// Clean up test jobs if they're dead/failed
	for _, testJobID := range job.TestJobIDs {
		if status, err := nc.getJobStatus(testJobID); err == nil && (status == "failed" || status == "dead") {
			if _, _, err := nc.client.Jobs().Deregister(testJobID, true, &nomadapi.WriteOptions{}); err != nil {
				errors = append(errors, fmt.Sprintf("test job %s cleanup: %v", testJobID, err))
			} else {
				nc.logger.WithField("job_id", testJobID).Info("Cleaned up failed test job from Nomad")
			}
		}
	}
	
	// Clean up publish job if it's dead/failed  
	if job.PublishJobID != "" {
		if status, err := nc.getJobStatus(job.PublishJobID); err == nil && (status == "failed" || status == "dead") {
			if _, _, err := nc.client.Jobs().Deregister(job.PublishJobID, true, &nomadapi.WriteOptions{}); err != nil {
				errors = append(errors, fmt.Sprintf("publish job cleanup: %v", err))
			} else {
				nc.logger.WithField("job_id", job.PublishJobID).Info("Cleaned up failed publish job from Nomad")
			}
		}
	}
	
	if len(errors) > 0 {
		return fmt.Errorf("failed to cleanup some jobs: %s", strings.Join(errors, ", "))
	}
	
	return nil
}

// Health checks the health of the Nomad connection
func (nc *Client) Health() error {
	_, err := nc.client.Status().Leader()
	if err != nil {
		return fmt.Errorf("nomad health check failed: %w", err)
	}
	return nil
}

// releaseBuildLock releases the build lock for a job if it has one
func (nc *Client) releaseBuildLock(job *types.Job) {
	if job.LockKey != "" && job.LockSessionID != "" {
		if err := nc.storage.ReleaseLock(job.LockKey, job.LockSessionID); err != nil {
			nc.logger.WithError(err).WithFields(logrus.Fields{
				"job_id":     job.ID,
				"lock_key":   job.LockKey,
				"session_id": job.LockSessionID,
			}).Warn("Failed to release build lock")
		} else {
			nc.logger.WithFields(logrus.Fields{
				"job_id":     job.ID,
				"lock_key":   job.LockKey,
				"session_id": job.LockSessionID,
			}).Info("Build lock released successfully")
			// Clear lock information from job
			job.LockKey = ""
			job.LockSessionID = ""
		}
	}
}

// Private helper methods

func (nc *Client) getJobStatus(nomadJobID string) (string, error) {
	job, _, err := nc.client.Jobs().Info(nomadJobID, nil)
	if err != nil {
		return "", err
	}

	// For service jobs, check deployment status first
	// If deployment is still running, Nomad is actively trying to place the job
	if job.Type != nil && *job.Type == "service" {
		deploys, _, err := nc.client.Jobs().Deployments(nomadJobID, false, nil)
		if err == nil && len(deploys) > 0 {
			// Get most recent deployment
			latestDeploy := deploys[0]
			for _, d := range deploys {
				if d.CreateIndex > latestDeploy.CreateIndex {
					latestDeploy = d
				}
			}
			// If deployment is still running, job is not failed yet
			if latestDeploy.Status == "running" {
				nc.logger.WithFields(logrus.Fields{
					"job_id":           nomadJobID,
					"deployment_id":    latestDeploy.ID,
					"deployment_status": latestDeploy.Status,
				}).Debug("Deployment still running, waiting for Nomad to reschedule")
				return "pending", nil
			}
			// If deployment explicitly failed, job has failed
			if latestDeploy.Status == "failed" || latestDeploy.Status == "cancelled" {
				return "failed", nil
			}
		}
	}

	// Check allocations for more detailed status
	allocs, _, err := nc.client.Jobs().Allocations(nomadJobID, false, nil)
	if err != nil {
		// If we can't get allocations but job exists, something is wrong
		nc.logger.WithError(err).WithField("job_id", nomadJobID).Warn("Failed to get job allocations")
	}

	// Handle case where job is dead with no allocations (scheduling failure)
	if *job.Status == "dead" && (allocs == nil || len(allocs) == 0) {
		return "failed", nil
	}
	
	// Check for placement failures - jobs that are stuck pending
	// If job has been pending for a long time with no running allocations, it may be unplaceable
	if *job.Status == "pending" && len(allocs) > 0 {
		allPending := true
		for _, alloc := range allocs {
			if alloc.ClientStatus != "pending" {
				allPending = false
				break
			}
		}
		// If all allocations are stuck in pending, check evaluations for placement failures
		if allPending {
			evals, _, err := nc.client.Jobs().Evaluations(nomadJobID, nil)
			if err == nil && len(evals) > 0 {
				// Check the most recent evaluation
				for _, eval := range evals {
					if eval.FailedTGAllocs != nil && len(eval.FailedTGAllocs) > 0 {
						nc.logger.WithFields(logrus.Fields{
							"job_id": nomadJobID,
							"eval_id": eval.ID,
						}).Warn("Job cannot be placed - placement failures detected")
						return "failed", nil
					}
				}
			}
		}
	}
	
	if err == nil && len(allocs) > 0 {
		// First pass: collect allocation status counts
		// Don't immediately fail if some allocations failed - Nomad may be rescheduling
		hasRunning := false
		hasPending := false
		hasFailed := false
		allComplete := true

		for _, alloc := range allocs {
			switch alloc.ClientStatus {
			case "running":
				hasRunning = true
				allComplete = false
			case "pending":
				hasPending = true
				allComplete = false
			case "complete":
				// Keep checking others
			case "failed":
				hasFailed = true
				allComplete = false
			case "lost":
				// Lost allocations should be treated as failed
				hasFailed = true
				allComplete = false
			default:
				// For any other status (including "dead"), check exit codes
				// Docker containers exit normally with code 0, resulting in "dead" status
				if alloc.TaskStates != nil {
					allDead := true
					anyFailed := false
					for _, taskState := range alloc.TaskStates {
						if taskState.State != "dead" {
							allDead = false
						}
						// Check if task failed (non-zero exit code or Failed flag)
						if taskState.Failed {
							anyFailed = true
						}
						// Also check exit code from events
						if taskState.Events != nil {
							for _, event := range taskState.Events {
								if event.Type == "Terminated" && event.ExitCode != 0 {
									anyFailed = true
								}
							}
						}
					}
					// If all tasks are dead and none failed, treat as complete
					if allDead && !anyFailed {
						// This allocation completed successfully (exit code 0)
						continue
					} else if anyFailed {
						hasFailed = true
					}
				}
				allComplete = false
			}
		}

		// Determine status based on allocation states
		// Running allocations take priority - job is working
		if hasRunning {
			return "running", nil
		}
		// Pending allocations mean Nomad is still trying to schedule
		if hasPending {
			return "pending", nil
		}
		if allComplete {
			return "complete", nil
		}
		// Only report failed if there are no running/pending allocations
		// This handles the case where Nomad is rescheduling after initial failures
		if hasFailed && !hasRunning && !hasPending {
			return "failed", nil
		}
	}
	
	// Fall back to basic job status
	switch *job.Status {
	case "pending":
		return "pending", nil
	case "running":
		return "running", nil
	case "complete":
		return "complete", nil
	case "failed", "cancelled":
		return "failed", nil
	default:
		return "unknown", nil
	}
}

func (nc *Client) getJobLogs(nomadJobID string) ([]string, error) {
	// Get allocations for the job
	allocs, _, err := nc.client.Jobs().Allocations(nomadJobID, false, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get allocations for job %s: %w", nomadJobID, err)
	}
	
	if len(allocs) == 0 {
		nc.logger.WithField("job_id", nomadJobID).Warn("No allocations found for job")
		return []string{"No allocations found - job may have failed to schedule"}, nil
	}
	
	var allLogs []string
	
	// Process each allocation
	for _, alloc := range allocs {
		// Get logs from each task in the allocation
		for taskName := range alloc.TaskStates {
			// Get stdout logs
			stdoutLogs, err := nc.getTaskLogs(alloc.ID, taskName, "stdout")
			if err != nil {
				nc.logger.WithError(err).WithFields(logrus.Fields{
					"alloc_id": alloc.ID,
					"task": taskName,
				}).Warn("Failed to get stdout logs")
			} else {
				for _, line := range stdoutLogs {
					if strings.TrimSpace(line) != "" {
						allLogs = append(allLogs, fmt.Sprintf("[%s/stdout] %s", taskName, line))
					}
				}
			}
			
			// Get stderr logs
			stderrLogs, err := nc.getTaskLogs(alloc.ID, taskName, "stderr")
			if err != nil {
				nc.logger.WithError(err).WithFields(logrus.Fields{
					"alloc_id": alloc.ID,
					"task": taskName,
				}).Warn("Failed to get stderr logs")
			} else {
				for _, line := range stderrLogs {
					if strings.TrimSpace(line) != "" {
						allLogs = append(allLogs, fmt.Sprintf("[%s/stderr] %s", taskName, line))
					}
				}
			}
		}
	}
	
	if len(allLogs) == 0 {
		// If no logs found, try to get allocation failure information
		for _, alloc := range allocs {
			if alloc.ClientStatus == "failed" {
				allLogs = append(allLogs, fmt.Sprintf("Allocation %s failed", alloc.ID))
				
				// Add task state information
				for taskName, taskState := range alloc.TaskStates {
					if taskState.Failed {
						allLogs = append(allLogs, fmt.Sprintf("Task %s failed in state: %s", taskName, taskState.State))
						
						// Add task events
						for _, event := range taskState.Events {
							allLogs = append(allLogs, fmt.Sprintf("Task %s event: %s - %s", taskName, event.Type, event.DisplayMessage))
						}
					}
				}
			}
		}
	}
	
	return allLogs, nil
}

// getTaskLogs retrieves logs for a specific task in an allocation
func (nc *Client) getTaskLogs(allocID, taskName, logType string) ([]string, error) {
	// Get allocation info first
	alloc, _, err := nc.client.Allocations().Info(allocID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get allocation info for %s: %w", allocID, err)
	}

	// Create cancel channel for timeout handling
	cancelCh := make(chan struct{})

	// Use the correct Logs API signature
	logStreamChan, errChan := nc.client.AllocFS().Logs(
		alloc,     // *Allocation
		false,     // follow
		taskName,  // task name
		logType,   // log type (stdout/stderr)
		"start",   // origin
		0,         // offset
		cancelCh,  // cancel chan
		nil,       // query options
	)

	var logs []string

	// Use timeout for dead allocations - prevents hanging on unusual cases
	// 5 seconds is generous for completed allocations
	timeout := time.After(5 * time.Second)

	// Read from stream, error, and timeout channels
	for {
		select {
		case frame, ok := <-logStreamChan:
			if !ok {
				// Channel closed properly - we have all logs
				return logs, nil
			}
			if frame.Data != nil && len(frame.Data) > 0 {
				// Split the frame data into lines
				content := string(frame.Data)
				lines := strings.Split(content, "\n")
				for _, line := range lines {
					if strings.TrimSpace(line) != "" {
						logs = append(logs, line)
					}
				}
			}
			// Wait for channel to close properly instead of returning early on empty frames
			// This ensures we capture all logs from short-lived or failed tasks

		case err, ok := <-errChan:
			if !ok {
				// Error channel closed
				return logs, nil
			}
			if err != nil {
				return logs, fmt.Errorf("error reading logs: %w", err)
			}

		case <-timeout:
			// Timeout reached - return what we have and signal cancellation
			close(cancelCh)
			return logs, nil
		}
	}
}

func (nc *Client) getJobErrorDetails(nomadJobID string) (string, error) {
	// Check for scheduling failures
	allocs, _, err := nc.client.Jobs().Allocations(nomadJobID, false, nil)
	if err != nil {
		return "Failed to get job allocations", nil
	}
	
	// If no allocations, job failed to schedule
	if len(allocs) == 0 {
		// Get job evaluations to understand why it failed to schedule
		evals, _, err := nc.client.Jobs().Evaluations(nomadJobID, nil)
		if err == nil && len(evals) > 0 {
			for _, eval := range evals {
				if eval.Status == "blocked" || eval.Status == "failed" {
					return fmt.Sprintf("Job failed to schedule: %s", eval.StatusDescription), nil
				}
			}
		}
		return "Job failed to schedule - no allocations placed", nil
	}
	
	// Check allocation failures
	var errorMessages []string
	for _, alloc := range allocs {
		if alloc.ClientStatus == "failed" {
			// Get task states for detailed error info
			if alloc.TaskStates != nil {
				for taskName, taskState := range alloc.TaskStates {
					if taskState.Failed {
						// Get the most recent failed event
						if len(taskState.Events) > 0 {
							lastEvent := taskState.Events[len(taskState.Events)-1]
							errorMessages = append(errorMessages, 
								fmt.Sprintf("Task '%s': %s - %s", taskName, lastEvent.Type, lastEvent.DisplayMessage))
						} else {
							errorMessages = append(errorMessages, 
								fmt.Sprintf("Task '%s' failed", taskName))
						}
					}
				}
			}
		}
	}
	
	if len(errorMessages) > 0 {
		return fmt.Sprintf("%s", errorMessages[0]), nil // Return first error for brevity
	}
	
	return "Unknown failure", nil
}

// getBuildJobNodeID retrieves the node ID where the build job ran
func (nc *Client) getBuildJobNodeID(buildJobID string) (string, error) {
	if buildJobID == "" {
		return "", fmt.Errorf("build job ID is empty")
	}
	
	_, _, err := nc.client.Jobs().Info(buildJobID, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get job info for %s: %w", buildJobID, err)
	}
	
	// Get allocations for the build job
	allocs, _, err := nc.client.Jobs().Allocations(buildJobID, false, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get allocations for job %s: %w", buildJobID, err)
	}
	
	// Find a completed allocation to get the node ID
	for _, alloc := range allocs {
		if alloc.ClientStatus == "complete" && alloc.NodeID != "" {
			return alloc.NodeID, nil
		}
	}
	
	// If no completed allocation, try any allocation with a node ID
	for _, alloc := range allocs {
		if alloc.NodeID != "" {
			return alloc.NodeID, nil
		}
	}
	
	return "", fmt.Errorf("no allocations found with node ID for build job %s", buildJobID)
}

func (nc *Client) startTestPhase(job *types.Job) error {
	// Check if this is an external (python) test
	if job.Config.Test != nil && job.Config.Test.PythonCommand != "" {
		return nc.startExternalTestPhase(job)
	}

	if job.Config.Test == nil || (len(job.Config.Test.Commands) == 0 && !job.Config.Test.EntryPoint) {
		// No tests configured, skip to publish phase
		nc.logger.WithField("job_id", job.ID).Info("No tests configured, skipping test phase")
		return nc.startPublishPhase(job)
	}
	
	// Get the build job's node ID to avoid scheduling test jobs on the same node
	buildNodeID, err := nc.getBuildJobNodeID(job.BuildJobID)
	if err != nil {
		nc.logger.WithError(err).Warn("Failed to get build job node ID, proceeding without node constraints")
		buildNodeID = "" // Empty string will disable node constraints
	}
	
	nc.logger.WithFields(logrus.Fields{
		"job_id": job.ID,
		"build_job_id": job.BuildJobID, 
		"build_node_id": buildNodeID,
	}).Info("Starting test phase with node constraint to avoid Docker layer conflicts")
	
	testJobSpecs, err := nc.createTestJobSpecs(job, buildNodeID)
	if err != nil {
		return fmt.Errorf("failed to create test job specs: %w", err)
	}
	
	if len(testJobSpecs) == 0 {
		// No test jobs to run, skip to publish phase
		nc.logger.WithField("job_id", job.ID).Info("No test jobs created, skipping test phase")
		return nc.startPublishPhase(job)
	}
	
	// Use proper WriteOptions and RegisterOpts to match CLI behavior
	registerOpts := &nomadapi.RegisterOptions{
		PolicyOverride: false,
		PreserveCounts: false,
	}
	writeOpts := &nomadapi.WriteOptions{
		Region:    nc.config.Nomad.Region,
		Namespace: nc.config.Nomad.Namespace,
	}
	
	// Submit all test jobs
	var testJobIDs []string
	for i, testJobSpec := range testJobSpecs {
		// Log the job specification for debugging
		nc.logJobSpec(testJobSpec, fmt.Sprintf("test-%d", i))
		
		evalID, _, err := nc.client.Jobs().RegisterOpts(testJobSpec, registerOpts, writeOpts)
		if err != nil {
			// Check for specific Vault template errors and provide better feedback
			errorMsg := err.Error()
			if strings.Contains(errorMsg, "Template failed") && strings.Contains(errorMsg, "vault.read: invalid format") {
				return fmt.Errorf("vault template error in test job %s: empty or invalid secret path provided. Please check that RegistryCredentialsPath is a valid Vault path or leave it empty if not needed. Original error: %w", *testJobSpec.ID, err)
			}
			if strings.Contains(errorMsg, "Template failed") && strings.Contains(errorMsg, "vault.read") {
				return fmt.Errorf("vault template error in test job %s: failed to read secret from Vault. Please verify the secret path exists and the service has proper permissions. Original error: %w", *testJobSpec.ID, err)
			}
			return fmt.Errorf("failed to submit test job %s to Nomad: %w", *testJobSpec.ID, err)
		}
		
		testJobIDs = append(testJobIDs, *testJobSpec.ID)
		
		nc.logger.WithFields(logrus.Fields{
			"job_id":      job.ID,
			"nomad_job":   *testJobSpec.ID,
			"eval_id":     evalID,
			"test_index":  i,
		}).Info("Test job submitted to Nomad")
	}
	
	job.TestJobIDs = testJobIDs
	
	nc.logger.WithFields(logrus.Fields{
		"job_id":       job.ID,
		"test_jobs":    len(testJobIDs),
		"test_job_ids": testJobIDs,
	}).Info("All test jobs submitted to Nomad - TestJobIDs set in memory")
	
	// Note: TestJobIDs need to be persisted by the caller to ensure
	// they're available for subsequent status checks and log capture
	return nil
}

func (nc *Client) startPublishPhase(job *types.Job) error {
	publishJobSpec, err := nc.createPublishJobSpec(job)
	if err != nil {
		return fmt.Errorf("failed to create publish job spec: %w", err)
	}
	
	// Log the job specification for debugging
	nc.logJobSpec(publishJobSpec, "publish")
	
	// Use proper WriteOptions and RegisterOpts to match CLI behavior
	registerOpts := &nomadapi.RegisterOptions{
		PolicyOverride: false,
		PreserveCounts: false,
	}
	writeOpts := &nomadapi.WriteOptions{
		Region:    nc.config.Nomad.Region,
		Namespace: nc.config.Nomad.Namespace,
	}
	
	evalID, _, err := nc.client.Jobs().RegisterOpts(publishJobSpec, registerOpts, writeOpts)
	if err != nil {
		// Check for specific Vault template errors and provide better feedback
		errorMsg := err.Error()
		if strings.Contains(errorMsg, "Template failed") && strings.Contains(errorMsg, "vault.read: invalid format") {
			return fmt.Errorf("vault template error in publish job: empty or invalid secret path provided. Please check that RegistryCredentialsPath is a valid Vault path or leave it empty if not needed. Original error: %w", err)
		}
		if strings.Contains(errorMsg, "Template failed") && strings.Contains(errorMsg, "vault.read") {
			return fmt.Errorf("vault template error in publish job: failed to read secret from Vault. Please verify the secret path exists and the service has proper permissions. Original error: %w", err)
		}
		return fmt.Errorf("failed to submit publish job to Nomad: %w", err)
	}
	
	job.PublishJobID = *publishJobSpec.ID
	
	nc.logger.WithFields(logrus.Fields{
		"job_id":      job.ID,
		"nomad_job":   *publishJobSpec.ID,
		"eval_id":     evalID,
	}).Info("Publish job submitted to Nomad")

	return nil
}

// startExternalTestPhase starts a service job for external (python) tests
// The container stays running so the CLI can execute tests against it
func (nc *Client) startExternalTestPhase(job *types.Job) error {
	buildNodeID, err := nc.getBuildJobNodeID(job.BuildJobID)
	if err != nil {
		nc.logger.WithError(err).Warn("Failed to get build job node ID, proceeding without constraint")
		buildNodeID = ""
	}

	testJobSpec, err := nc.createExternalTestJobSpec(job, buildNodeID)
	if err != nil {
		return fmt.Errorf("failed to create external test job spec: %w", err)
	}

	nc.logJobSpec(testJobSpec, "test-external")

	registerOpts := &nomadapi.RegisterOptions{
		PolicyOverride: false,
		PreserveCounts: false,
	}
	writeOpts := &nomadapi.WriteOptions{
		Region:    nc.config.Nomad.Region,
		Namespace: nc.config.Nomad.Namespace,
	}

	evalID, _, err := nc.client.Jobs().RegisterOpts(testJobSpec, registerOpts, writeOpts)
	if err != nil {
		return fmt.Errorf("failed to submit external test job: %w", err)
	}

	job.TestJobNomadID = *testJobSpec.ID

	nc.logger.WithFields(logrus.Fields{
		"job_id":    job.ID,
		"nomad_job": *testJobSpec.ID,
		"eval_id":   evalID,
	}).Info("External test job submitted to Nomad")

	return nil
}

// GetExternalTestEndpoint discovers the IP and port of a running external test container
func (nc *Client) GetExternalTestEndpoint(job *types.Job) (string, int, error) {
	if job.TestJobNomadID == "" {
		return "", 0, fmt.Errorf("no external test job running")
	}

	queryOpts := &nomadapi.QueryOptions{
		Namespace: nc.config.Nomad.Namespace,
	}

	allocs, _, err := nc.client.Jobs().Allocations(job.TestJobNomadID, false, queryOpts)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get allocations: %w", err)
	}

	for _, alloc := range allocs {
		if alloc.ClientStatus == "running" {
			allocInfo, _, err := nc.client.Allocations().Info(alloc.ID, queryOpts)
			if err != nil {
				nc.logger.WithError(err).Warn("Failed to get allocation info")
				continue
			}

			// Get the dynamic port from the allocation's resources
			if allocInfo.AllocatedResources != nil {
				for _, network := range allocInfo.AllocatedResources.Shared.Networks {
					for _, port := range network.DynamicPorts {
						if port.Label == "http" {
							return network.IP, port.Value, nil
						}
					}
				}
			}

			// Fallback: try to get from task resources
			if allocInfo.Resources != nil && allocInfo.Resources.Networks != nil {
				for _, network := range allocInfo.Resources.Networks {
					for _, port := range network.DynamicPorts {
						if port.Label == "http" {
							return network.IP, port.Value, nil
						}
					}
				}
			}
		}
	}

	return "", 0, fmt.Errorf("no running allocation with port found")
}

// StopExternalTestJob stops the external test service job
func (nc *Client) StopExternalTestJob(job *types.Job) error {
	if job.TestJobNomadID == "" {
		return nil
	}

	writeOpts := &nomadapi.WriteOptions{
		Region:    nc.config.Nomad.Region,
		Namespace: nc.config.Nomad.Namespace,
	}

	_, _, err := nc.client.Jobs().Deregister(job.TestJobNomadID, false, writeOpts)
	if err != nil {
		return fmt.Errorf("failed to stop external test job: %w", err)
	}

	nc.logger.WithFields(logrus.Fields{
		"job_id":    job.ID,
		"nomad_job": job.TestJobNomadID,
	}).Info("External test job stopped")

	return nil
}

// StartPublishPhaseAfterExternalTest is called by the API after CLI reports test success
func (nc *Client) StartPublishPhaseAfterExternalTest(job *types.Job) error {
	return nc.startPublishPhase(job)
}

func (nc *Client) cleanupTempImages(job *types.Job) error {
	// Create a cleanup job to remove temporary images from registry
	cleanupJobSpec := nc.createCleanupJobSpec(job)
	
	// Log the job specification for debugging
	nc.logJobSpec(cleanupJobSpec, "cleanup")
	
	// Use proper WriteOptions and RegisterOpts to match CLI behavior
	registerOpts := &nomadapi.RegisterOptions{
		PolicyOverride: false,
		PreserveCounts: false,
	}
	writeOpts := &nomadapi.WriteOptions{
		Region:    nc.config.Nomad.Region,
		Namespace: nc.config.Nomad.Namespace,
	}
	
	evalID, _, err := nc.client.Jobs().RegisterOpts(cleanupJobSpec, registerOpts, writeOpts)
	if err != nil {
		return fmt.Errorf("failed to submit cleanup job to Nomad: %w", err)
	}
	
	nc.logger.WithFields(logrus.Fields{
		"job_id":      job.ID,
		"nomad_job":   *cleanupJobSpec.ID,
		"eval_id":     evalID,
	}).Info("Cleanup job submitted to Nomad")
	
	return nil
}

// logJobSpec logs the job specification based on configuration settings
func (nc *Client) logJobSpec(jobSpec *nomadapi.Job, phase string) {
	if !nc.config.Logging.LogJobSpecs {
		return
	}

	logEntry := nc.logger.WithFields(logrus.Fields{
		"nomad_job_id": *jobSpec.ID,
		"phase":        phase,
	})

	if nc.config.Logging.LogHCLFormat {
		// Convert job spec to HCL-style format for easier debugging
		hcl, err := nc.JobSpecToHCL(jobSpec)
		if err != nil {
			logEntry.WithError(err).Warn("Failed to convert job spec to HCL format")
		} else {
			// Encode as base64 to avoid escaping hell
			encoded := base64.StdEncoding.EncodeToString([]byte(hcl))
			logEntry.WithField("job_spec_hcl_b64", encoded).Debug("Generated Nomad job specification (HCL base64 encoded)")
			
			// Also log decode instructions
			logEntry.Info("To decode HCL: echo 'BASE64_STRING' | base64 -d > job.hcl")
		}
	} else {
		// Log as JSON for programmatic consumption
		jsonSpec, err := json.MarshalIndent(jobSpec, "", "  ")
		if err != nil {
			logEntry.WithError(err).Warn("Failed to marshal job spec to JSON")
		} else {
			// Encode JSON as base64 too for consistency
			encoded := base64.StdEncoding.EncodeToString(jsonSpec)
			logEntry.WithField("job_spec_json_b64", encoded).Debug("Generated Nomad job specification (JSON base64 encoded)")
			
			// Also log decode instructions  
			logEntry.Info("To decode JSON: echo 'BASE64_STRING' | base64 -d | jq")
		}
	}
}

// JobSpecToHCL converts a job specification to HCL-like format for debugging
func (nc *Client) JobSpecToHCL(jobSpec *nomadapi.Job) (string, error) {
	var hcl strings.Builder
	
	hcl.WriteString(fmt.Sprintf("job \"%s\" {\n", *jobSpec.ID))
	hcl.WriteString(fmt.Sprintf("  name      = \"%s\"\n", *jobSpec.Name))
	hcl.WriteString(fmt.Sprintf("  type      = \"%s\"\n", *jobSpec.Type))
	hcl.WriteString(fmt.Sprintf("  namespace = \"%s\"\n", *jobSpec.Namespace))
	hcl.WriteString(fmt.Sprintf("  region    = \"%s\"\n", *jobSpec.Region))
	
	if len(jobSpec.Datacenters) > 0 {
		hcl.WriteString("  datacenters = [")
		for i, dc := range jobSpec.Datacenters {
			if i > 0 {
				hcl.WriteString(", ")
			}
			hcl.WriteString(fmt.Sprintf("\"%s\"", dc))
		}
		hcl.WriteString("]\n")
	}
	
	// Add meta fields
	if len(jobSpec.Meta) > 0 {
		hcl.WriteString("\n  meta {\n")
		for key, value := range jobSpec.Meta {
			hcl.WriteString(fmt.Sprintf("    %s = \"%s\"\n", key, value))
		}
		hcl.WriteString("  }\n")
	}
	
	// Add task groups
	for _, tg := range jobSpec.TaskGroups {
		hcl.WriteString(fmt.Sprintf("\n  group \"%s\" {\n", *tg.Name))
		hcl.WriteString(fmt.Sprintf("    count = %d\n", *tg.Count))
		
		// Add restart policy
		if tg.RestartPolicy != nil {
			hcl.WriteString("\n    restart {\n")
			hcl.WriteString(fmt.Sprintf("      attempts = %d\n", *tg.RestartPolicy.Attempts))
			hcl.WriteString("    }\n")
		}
		
		// Add network configuration
		if len(tg.Networks) > 0 {
			for _, network := range tg.Networks {
				hcl.WriteString("\n    network {\n")
				if network.Mode != "" {
					hcl.WriteString(fmt.Sprintf("      mode = \"%s\"\n", network.Mode))
				}
				hcl.WriteString("    }\n")
			}
		}
		
		// Add ephemeral disk
		if tg.EphemeralDisk != nil {
			hcl.WriteString("\n    ephemeral_disk {\n")
			hcl.WriteString(fmt.Sprintf("      size = %d\n", *tg.EphemeralDisk.SizeMB))
			hcl.WriteString("    }\n")
		}
		
		// Add tasks
		for _, task := range tg.Tasks {
			hcl.WriteString(fmt.Sprintf("\n    task \"%s\" {\n", task.Name))
			hcl.WriteString(fmt.Sprintf("      driver = \"%s\"\n", task.Driver))
			
			// Add task config
			if len(task.Config) > 0 {
				hcl.WriteString("\n      config {\n")
				for key, value := range task.Config {
					switch v := value.(type) {
					case string:
						// Handle multi-line strings properly with heredocs
						if strings.Contains(v, "\n") {
							hcl.WriteString(fmt.Sprintf("        %s = <<EOF\n%s\nEOF\n", key, v))
						} else {
							// Escape quotes in single-line strings
							escaped := strings.ReplaceAll(v, "\"", "\\\"")
							hcl.WriteString(fmt.Sprintf("        %s = \"%s\"\n", key, escaped))
						}
					case []string:
						hcl.WriteString(fmt.Sprintf("        %s = [\n", key))
						for i, item := range v {
							// Handle multi-line items in arrays with proper escaping
							if strings.Contains(item, "\n") {
								hcl.WriteString("          <<EOF\n")
								hcl.WriteString(item)
								hcl.WriteString("\nEOF")
								if i < len(v)-1 {
									hcl.WriteString(",")
								}
								hcl.WriteString("\n")
							} else {
								// Escape quotes for single-line items
								escaped := strings.ReplaceAll(item, "\"", "\\\"")
								hcl.WriteString(fmt.Sprintf("          \"%s\"", escaped))
								if i < len(v)-1 {
									hcl.WriteString(",")
								}
								hcl.WriteString("\n")
							}
						}
						hcl.WriteString("        ]\n")
					case bool:
						hcl.WriteString(fmt.Sprintf("        %s = %t\n", key, v))
					default:
						// Convert to JSON for complex types
						jsonVal, _ := json.Marshal(v)
						hcl.WriteString(fmt.Sprintf("        %s = %s\n", key, string(jsonVal)))
					}
				}
				hcl.WriteString("      }\n")
			}
			
			// Add environment variables
			if len(task.Env) > 0 {
				hcl.WriteString("\n      env {\n")
				for key, value := range task.Env {
					hcl.WriteString(fmt.Sprintf("        %s = \"%s\"\n", key, value))
				}
				hcl.WriteString("      }\n")
			}
			
			// Add resources
			if task.Resources != nil {
				hcl.WriteString("\n      resources {\n")
				if task.Resources.CPU != nil {
					hcl.WriteString(fmt.Sprintf("        cpu    = %d\n", *task.Resources.CPU))
				}
				if task.Resources.MemoryMB != nil {
					hcl.WriteString(fmt.Sprintf("        memory = %d\n", *task.Resources.MemoryMB))
				}
				if task.Resources.DiskMB != nil {
					hcl.WriteString(fmt.Sprintf("        disk   = %d\n", *task.Resources.DiskMB))
				}
				hcl.WriteString("      }\n")
			}
			
			// Add Vault configuration
			if task.Vault != nil {
				hcl.WriteString("\n      vault {\n")
				if len(task.Vault.Policies) > 0 {
					hcl.WriteString("        policies = [")
					for i, policy := range task.Vault.Policies {
						if i > 0 {
							hcl.WriteString(", ")
						}
						hcl.WriteString(fmt.Sprintf("\"%s\"", policy))
					}
					hcl.WriteString("]\n")
				}
				if task.Vault.ChangeMode != nil {
					hcl.WriteString(fmt.Sprintf("        change_mode = \"%s\"\n", *task.Vault.ChangeMode))
				}
				if task.Vault.Role != "" {
					hcl.WriteString(fmt.Sprintf("        role = \"%s\"\n", task.Vault.Role))
				}
				hcl.WriteString("      }\n")
			}
			
			// Add templates
			if len(task.Templates) > 0 {
				for _, template := range task.Templates {
					hcl.WriteString("\n      template {\n")
					if template.DestPath != nil {
						hcl.WriteString(fmt.Sprintf("        destination = \"%s\"\n", *template.DestPath))
					}
					if template.ChangeMode != nil {
						hcl.WriteString(fmt.Sprintf("        change_mode = \"%s\"\n", *template.ChangeMode))
					}
					if template.EmbeddedTmpl != nil {
						// Use heredoc for template data to handle multi-line content
						hcl.WriteString("        data = <<EOF\n")
						hcl.WriteString(*template.EmbeddedTmpl)
						hcl.WriteString("\nEOF\n")
					}
					hcl.WriteString("      }\n")
				}
			}
			
			hcl.WriteString("    }\n")
		}
		
		hcl.WriteString("  }\n")
	}
	
	hcl.WriteString("}\n")
	
	return hcl.String(), nil
}

// capturePhaseLogs captures logs from a completed phase and stores them persistently
func (nc *Client) capturePhaseLogs(job *types.Job, phase string) error {
	switch phase {
	case "build":
		if job.BuildJobID == "" {
			return nil // No job to capture logs from
		}
		
		logs, err := nc.getJobLogs(job.BuildJobID)
		if err != nil {
			return fmt.Errorf("failed to get logs for build phase: %w", err)
		}
		
		job.Logs.Build = logs
		
		nc.logger.WithFields(logrus.Fields{
			"job_id": job.ID,
			"phase":  phase,
			"log_lines": len(logs),
		}).Info("Captured build phase logs")
		
	case "test":
		var testJobIDs []string
		
		// If TestJobIDs are available in the job object, use them
		if len(job.TestJobIDs) > 0 {
			testJobIDs = job.TestJobIDs
		} else {
			// Fallback: discover test jobs by searching for jobs with the expected naming pattern
			nc.logger.WithFields(logrus.Fields{
				"job_id": job.ID,
				"phase": phase,
			}).Warn("TestJobIDs not found in job object, attempting to discover test jobs")
			
			// Query Nomad for jobs matching the test job naming pattern
			discoveredJobs, err := nc.discoverTestJobs(job.ID)
			if err != nil {
				nc.logger.WithError(err).Warn("Failed to discover test jobs for log capture")
				return nil
			}
			testJobIDs = discoveredJobs
			
			if len(testJobIDs) == 0 {
				nc.logger.WithField("job_id", job.ID).Warn("No test jobs found for log capture")
				return nil
			}
		}
		
		var allTestLogs []string
		totalLogLines := 0
		
		for i, testJobID := range testJobIDs {
			logs, err := nc.getJobLogs(testJobID)
			if err != nil {
				nc.logger.WithError(err).WithField("test_job_id", testJobID).Warn("Failed to capture logs for test job")
				allTestLogs = append(allTestLogs, fmt.Sprintf("Failed to get logs for test job %d (%s): %v", i, testJobID, err))
			} else {
				// Add header to distinguish between different test jobs
				allTestLogs = append(allTestLogs, fmt.Sprintf("=== Test Job %d (%s) ===", i, testJobID))
				allTestLogs = append(allTestLogs, logs...)
				allTestLogs = append(allTestLogs, "") // Empty line for separation
				totalLogLines += len(logs)
			}
		}
		
		job.Logs.Test = allTestLogs
		
		nc.logger.WithFields(logrus.Fields{
			"job_id": job.ID,
			"phase":  phase,
			"test_jobs": len(job.TestJobIDs),
			"log_lines": totalLogLines,
		}).Info("Captured test phase logs")
		
	case "publish":
		if job.PublishJobID == "" {
			return nil // No job to capture logs from
		}
		
		logs, err := nc.getJobLogs(job.PublishJobID)
		if err != nil {
			return fmt.Errorf("failed to get logs for publish phase: %w", err)
		}
		
		job.Logs.Publish = logs
		
		nc.logger.WithFields(logrus.Fields{
			"job_id": job.ID,
			"phase":  phase,
			"log_lines": len(logs),
		}).Info("Captured publish phase logs")
		
	default:
		return fmt.Errorf("unknown phase: %s", phase)
	}

	return nil
}

// GetJobAllocations retrieves detailed allocation information for all phases of a job
func (nc *Client) GetJobAllocations(job *types.Job) (*types.JobAllocations, error) {
	allocations := &types.JobAllocations{}

	// Get build phase allocations
	if job.BuildJobID != "" {
		phaseAllocs, err := nc.getPhaseAllocations(job.BuildJobID, "build")
		if err != nil {
			nc.logger.WithError(err).WithField("job_id", job.BuildJobID).Warn("Failed to get build allocations")
		} else {
			allocations.Build = phaseAllocs
			if phaseAllocs.HasWarning {
				allocations.HasAnyWarning = true
			}
		}
	}

	// Get test phase allocations (multiple test jobs)
	for _, testJobID := range job.TestJobIDs {
		phaseAllocs, err := nc.getPhaseAllocations(testJobID, "test")
		if err != nil {
			nc.logger.WithError(err).WithField("job_id", testJobID).Warn("Failed to get test allocations")
			continue
		}
		allocations.Test = append(allocations.Test, *phaseAllocs)
		if phaseAllocs.HasWarning {
			allocations.HasAnyWarning = true
		}
	}

	// Also check external test job if present
	if job.TestJobNomadID != "" {
		phaseAllocs, err := nc.getPhaseAllocations(job.TestJobNomadID, "test")
		if err != nil {
			nc.logger.WithError(err).WithField("job_id", job.TestJobNomadID).Warn("Failed to get external test allocations")
		} else {
			allocations.Test = append(allocations.Test, *phaseAllocs)
			if phaseAllocs.HasWarning {
				allocations.HasAnyWarning = true
			}
		}
	}

	// Get publish phase allocations
	if job.PublishJobID != "" {
		phaseAllocs, err := nc.getPhaseAllocations(job.PublishJobID, "publish")
		if err != nil {
			nc.logger.WithError(err).WithField("job_id", job.PublishJobID).Warn("Failed to get publish allocations")
		} else {
			allocations.Publish = phaseAllocs
			if phaseAllocs.HasWarning {
				allocations.HasAnyWarning = true
			}
		}
	}

	return allocations, nil
}

// getPhaseAllocations retrieves allocation details for a specific Nomad job
func (nc *Client) getPhaseAllocations(nomadJobID, phase string) (*types.PhaseAllocations, error) {
	allocs, _, err := nc.client.Jobs().Allocations(nomadJobID, false, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get allocations: %w", err)
	}

	phaseAllocs := &types.PhaseAllocations{
		Phase:           phase,
		NomadJobID:      nomadJobID,
		AllocationCount: len(allocs),
		Allocations:     make([]types.AllocationInfo, 0, len(allocs)),
	}

	for _, alloc := range allocs {
		info := types.AllocationInfo{
			ID:            alloc.ID[:8], // Short ID for display
			FullID:        alloc.ID,
			ClientStatus:  alloc.ClientStatus,
			DesiredStatus: alloc.DesiredStatus,
			NodeID:        alloc.NodeID,
			CreatedAt:     time.Unix(0, alloc.CreateTime),
			ModifiedAt:    time.Unix(0, alloc.ModifyTime),
		}

		// Get node name if possible
		if alloc.NodeID != "" {
			if node, _, err := nc.client.Nodes().Info(alloc.NodeID, nil); err == nil && node != nil {
				info.NodeName = node.Name
			}
		}

		// Get failure reason from task states
		if alloc.ClientStatus == "failed" && alloc.TaskStates != nil {
			info.TaskStates = make(map[string]types.TaskStateInfo)
			for taskName, taskState := range alloc.TaskStates {
				taskInfo := types.TaskStateInfo{
					State:  taskState.State,
					Failed: taskState.Failed,
				}
				if len(taskState.Events) > 0 {
					lastEvent := taskState.Events[len(taskState.Events)-1]
					taskInfo.LastEvent = lastEvent.Type
					taskInfo.LastMessage = lastEvent.DisplayMessage
					if taskState.Failed {
						info.FailedReason = fmt.Sprintf("%s: %s", lastEvent.Type, lastEvent.DisplayMessage)
					}
				}
				info.TaskStates[taskName] = taskInfo
			}
		}

		phaseAllocs.Allocations = append(phaseAllocs.Allocations, info)
	}

	// Set warning if multiple allocations
	if len(allocs) > 1 {
		phaseAllocs.HasWarning = true
		phaseAllocs.WarningMessage = fmt.Sprintf(
			"Multiple allocations detected for %s phase: %d allocations (expected 1). "+
				"This may indicate scheduling issues, node failures, or resource constraints.",
			phase, len(allocs))
	}

	return phaseAllocs, nil
}