package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"nomad-mcp-builder/pkg/client"
	"nomad-mcp-builder/pkg/config"
	"nomad-mcp-builder/pkg/consul"
	"nomad-mcp-builder/pkg/history"
	"nomad-mcp-builder/pkg/types"
)

const (
	defaultServiceURL = "http://localhost:8080"
	usage = `jobforge - CLI client for JobForge Build Service

USAGE:
  jobforge [global-flags] <command> [command-args...]

DESCRIPTION:
  A command-line interface for the JobForge Build Service. Supports YAML
  configuration files and simplified image tagging (defaults to job-id).

COMMANDS:
  Job Management:
    submit-job [options] [config-file]  Submit a new build job
    get-status <job-id>               Get status of a job
    get-logs [options] <job-id>       Get logs for a job
    kill-job <job-id>                 Kill a running job
    cleanup <job-id>                  Clean up resources for a job
    get-history [limit] [offset]      Get job history (default: 10 recent jobs)

  Service Management:
    health                            Check service health status

GLOBAL FLAGS:
  -h, --help                Show this help message
  -u, --url <url>           Service URL (default: http://localhost:8080)
                            Can also be set via JOB_SERVICE_URL environment variable

SUBMIT-JOB OPTIONS:
  -global <file>            Global YAML configuration file (optional)
                            Typically: deploy/global.yaml

  <config-file>             Configuration file path (positional argument)
                            If not provided, reads from stdin

  --image-tags <tags>       Image tags to use (comma-separated)
                            If not specified, defaults to job-id

  -w, --watch               Watch job progress in real-time using Consul KV
                            Displays status updates and exits when job completes
                            Requires Consul connection (default: localhost:8500)

  --history                 Record build history locally
                            Creates deploy/builds/<job-id>/ with metadata and logs
                            When used alone: Records submission only (no final status)
                            With --watch: Records complete build with logs and final status
                            Deploy directory configurable via 'deploy_dir' in YAML (default: ./deploy)

GET-LOGS OPTIONS:
  --phase <phase>           Get logs for specific phase only (build, test, or publish)
                            If not specified, returns logs for all phases

YAML CONFIGURATION:
  Global config (deploy/global.yaml) - Shared settings:
    owner: myteam
    repo_url: https://github.com/myorg/myservice.git
    git_credentials_path: secret/nomad/jobs/git-credentials
    image_name: myservice
    registry_url: registry.example.com:5000/myapp

  Per-build config (build.yaml) - Build-specific overrides:
    git_ref: feature/new-feature
    test_entry_point: true

  See docs/JobSpec.md for complete configuration reference.

EXAMPLES:
  Submit Jobs:
    # Simple: config file as positional argument (recommended)
    jobforge submit-job build.yaml

    # With global config (recommended for teams)
    jobforge submit-job -global deploy/global.yaml build.yaml

    # With custom image tags (defaults to job-id if not specified)
    jobforge submit-job build.yaml --image-tags "latest,stable"

    # Watch job progress in real-time (recommended for interactive use)
    jobforge submit-job build.yaml --watch

    # Record build history (submission only, no final status)
    jobforge submit-job build.yaml --history

    # Record complete build history with logs (recommended)
    jobforge submit-job build.yaml --history --watch

    # From stdin (YAML format)
    cat build.yaml | jobforge submit-job
    echo "owner: test..." | jobforge submit-job

  Query Jobs:
    # Get job status
    jobforge get-status abc123

    # Get all logs
    jobforge get-logs abc123

    # Get phase-specific logs (recommended for large builds)
    jobforge get-logs --phase build abc123
    jobforge get-logs --phase test abc123
    jobforge get-logs --phase publish abc123

    # Backward compatible syntax also works
    jobforge get-logs abc123 build
    jobforge get-logs abc123 test
    jobforge get-logs abc123 publish

    # Get job history (last 20 jobs)
    jobforge get-history 20

    # Get job history with pagination
    jobforge get-history 10 20  # limit=10, offset=20

  Manage Jobs:
    # Kill a running job
    jobforge kill-job abc123

    # Clean up job resources
    jobforge cleanup abc123

  Service Health:
    # Check service health
    jobforge health
    # Output:
    #   Service URL: http://10.0.1.16:21654
    #
    #   ✅ Overall Status: healthy
    #   Timestamp: 2025-10-08T12:34:56Z
    #
    #   Services:
    #     ✅ nomad: healthy
    #     ✅ consul: healthy

ENVIRONMENT VARIABLES:
  JOB_SERVICE_URL           Service URL (overrides default, can be overridden by -u flag)

FILES:
  deploy/global.yaml        Global configuration (optional)
  build.yaml                Per-build configuration

DOCUMENTATION:
  docs/JobSpec.md           Complete job configuration reference
  README.md                 Project overview and setup guide
  CLAUDE.md                 Development guidelines

For more information, visit: https://github.com/geraldthewes/nomad-mcp-builder
`
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	// Parse flags
	serviceURL := getServiceURL()
	var command string
	var commandArgs []string

	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "-h" || arg == "--help" {
			fmt.Print(usage)
			return nil
		} else if arg == "-u" || arg == "--url" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			serviceURL = args[i+1]
			i += 2
		} else if strings.HasPrefix(arg, "--url=") {
			serviceURL = strings.TrimPrefix(arg, "--url=")
			i++
		} else if !strings.HasPrefix(arg, "-") {
			command = arg
			commandArgs = args[i+1:]
			break
		} else {
			return fmt.Errorf("unknown flag: %s", arg)
		}
	}

	if command == "" {
		return fmt.Errorf("no command specified")
	}

	// Create client
	c := client.NewClient(serviceURL)

	// Execute command
	switch command {
	case "submit-job":
		// Parse submit-job specific flags from commandArgs
		return handleSubmitJob(c, commandArgs)
	case "get-status":
		return handleGetStatus(c, commandArgs)
	case "get-logs":
		return handleGetLogs(c, commandArgs)
	case "kill-job":
		return handleKillJob(c, commandArgs)
	case "cleanup":
		return handleCleanup(c, commandArgs)
	case "get-history":
		return handleGetHistory(c, commandArgs)
	case "health":
		return handleHealth(c, commandArgs)
	default:
		return fmt.Errorf("unknown command: %s", command)
	}
}

func getServiceURL() string {
	if url := os.Getenv("JOB_SERVICE_URL"); url != "" {
		return url
	}
	return defaultServiceURL
}

func handleSubmitJob(c *client.Client, args []string) error {
	// Parse submit-job specific flags
	var additionalImageTags []string
	var globalConfigPath string
	var perBuildConfigPath string
	var configData string
	var watch bool
	var enableHistory bool

	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--watch" || arg == "-w" {
			watch = true
			i++
		} else if arg == "--history" {
			enableHistory = true
			i++
		} else if arg == "--image-tags" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			tagStr := args[i+1]
			additionalImageTags = strings.Split(tagStr, ",")
			// Trim spaces from tags
			for j, tag := range additionalImageTags {
				additionalImageTags[j] = strings.TrimSpace(tag)
			}
			i += 2
		} else if strings.HasPrefix(arg, "--image-tags=") {
			tagStr := strings.TrimPrefix(arg, "--image-tags=")
			additionalImageTags = strings.Split(tagStr, ",")
			// Trim spaces from tags
			for j, tag := range additionalImageTags {
				additionalImageTags[j] = strings.TrimSpace(tag)
			}
			i++
		} else if arg == "-global" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			globalConfigPath = args[i+1]
			i += 2
		} else if !strings.HasPrefix(arg, "-") {
			// This is a positional argument - check if it's a file path
			if _, err := os.Stat(arg); err == nil {
				// File exists, treat as config file path
				perBuildConfigPath = arg
			} else {
				// Not a file, treat as YAML data string
				configData = arg
			}
			i++
			break
		} else {
			return fmt.Errorf("unknown flag: %s", arg)
		}
	}

	var jobConfig *types.JobConfig
	var err error

	// Determine how to load configuration
	if perBuildConfigPath != "" {
		// Load from YAML files (with optional global config)
		jobConfig, err = config.LoadAndMergeJobConfigs(globalConfigPath, perBuildConfigPath)
		if err != nil {
			return fmt.Errorf("failed to load YAML config: %w", err)
		}
	} else if configData != "" {
		// Parse from command line argument (YAML)
		jobConfig, err = parseConfigData(configData)
		if err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
	} else {
		// Read from stdin
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read from stdin: %w", err)
		}
		configData = string(data)

		if strings.TrimSpace(configData) == "" {
			return fmt.Errorf("no job configuration provided")
		}

		jobConfig, err = parseConfigData(configData)
		if err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
	}

	// Validate that --watch is required for python tests
	if jobConfig.Test != nil && jobConfig.Test.PythonCommand != "" && !watch {
		return fmt.Errorf("--watch flag is required when using python tests (test.python_command is set)")
	}

	// Set image tags from --image-tags flag if provided
	// If not provided, server will default to using job-id as the tag
	if len(additionalImageTags) > 0 {
		// Filter out empty tags
		var validTags []string
		for _, tag := range additionalImageTags {
			if tag != "" {
				validTags = append(validTags, tag)
			}
		}
		if len(validTags) > 0 {
			jobConfig.ImageTags = validTags
		}
	}

	// Submit job
	submissionTime := time.Now()
	response, err := c.SubmitJob(jobConfig)
	if err != nil {
		return fmt.Errorf("failed to submit job: %w", err)
	}

	// Handle history recording if --history flag is set
	var historyMgr *history.Manager
	if enableHistory {
		// Get deploy directory from config (default to "./deploy")
		deployDir := jobConfig.DeployDir
		if deployDir == "" {
			deployDir = "./deploy"
		}

		// Create history manager
		historyMgr, err = history.NewManager(deployDir)
		if err != nil {
			return fmt.Errorf("failed to create history manager: %w", err)
		}

		// Create build directory
		if err := historyMgr.CreateBuildDirectory(response.JobID); err != nil {
			return fmt.Errorf("failed to create build directory: %w", err)
		}

		// Write initial metadata
		if err := historyMgr.WriteInitialMetadata(response.JobID, *jobConfig); err != nil {
			return fmt.Errorf("failed to write initial metadata: %w", err)
		}

		// Add initial entry to history.md
		entry := history.HistoryEntry{
			JobID:     response.JobID,
			Branch:    extractBranchFromConfig(*jobConfig),
			GitRef:    jobConfig.GitRef,
			Timestamp: submissionTime,
			Status:    types.StatusPending,
			ImageTags: jobConfig.ImageTags,
		}
		if err := historyMgr.UpdateHistoryFile(entry); err != nil {
			// Log warning but don't fail
			fmt.Fprintf(os.Stderr, "Warning: failed to update history file: %v\n", err)
		}

		fmt.Printf("History recorded to: %s\n", historyMgr.GetBuildDir(response.JobID))
	}

	// If --watch flag is set, watch the job progress instead of returning immediately
	if watch {
		return watchJobProgress(response.JobID, c.GetBaseURL(), historyMgr, submissionTime, jobConfig)
	}

	// Output response
	output, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format response: %w", err)
	}

	fmt.Println(string(output))
	return nil
}

// extractBranchFromConfig extracts branch from job config
func extractBranchFromConfig(config types.JobConfig) string {
	if config.GitRef == "" {
		return "unknown"
	}
	// Handle common patterns
	if strings.HasPrefix(config.GitRef, "refs/heads/") {
		return strings.TrimPrefix(config.GitRef, "refs/heads/")
	}
	if strings.HasPrefix(config.GitRef, "refs/tags/") {
		return strings.TrimPrefix(config.GitRef, "refs/tags/")
	}
	return config.GitRef
}

// parseConfigData parses config data as YAML
func parseConfigData(data string) (*types.JobConfig, error) {
	var jobConfig types.JobConfig

	// Parse as YAML
	if err := yaml.Unmarshal([]byte(data), &jobConfig); err != nil {
		return nil, fmt.Errorf("failed to parse as YAML: %w", err)
	}

	return &jobConfig, nil
}

func handleGetStatus(c *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("job ID required")
	}

	jobID := args[0]
	response, err := c.GetStatus(jobID)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	// Output response
	output, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format response: %w", err)
	}

	fmt.Println(string(output))
	return nil
}

func handleGetLogs(c *client.Client, args []string) error {
	var phase string
	var jobID string

	// Parse all arguments - flags can come before or after job ID
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--phase" {
			if i+1 >= len(args) {
				return fmt.Errorf("--phase flag requires a value")
			}
			phase = args[i+1]
			// Validate phase value
			if phase != "build" && phase != "test" && phase != "publish" {
				return fmt.Errorf("invalid phase: %s (must be build, test, or publish)", phase)
			}
			i += 2
		} else if strings.HasPrefix(arg, "--phase=") {
			phase = strings.TrimPrefix(arg, "--phase=")
			// Validate phase value
			if phase != "build" && phase != "test" && phase != "publish" {
				return fmt.Errorf("invalid phase: %s (must be build, test, or publish)", phase)
			}
			i++
		} else if !strings.HasPrefix(arg, "-") {
			// Non-flag argument - could be job ID or positional phase
			if jobID == "" {
				jobID = arg
				i++
			} else if phase == "" {
				// Second non-flag argument could be positional phase (backward compatibility)
				if arg == "build" || arg == "test" || arg == "publish" {
					phase = arg
				}
				i++
			} else {
				// Extra argument - skip
				i++
			}
		} else {
			return fmt.Errorf("unknown flag: %s", arg)
		}
	}

	if jobID == "" {
		return fmt.Errorf("job ID required")
	}

	response, err := c.GetLogs(jobID, phase)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	// Output response
	output, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format response: %w", err)
	}

	fmt.Println(string(output))
	return nil
}

func handleKillJob(c *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("job ID required")
	}

	jobID := args[0]
	response, err := c.KillJob(jobID)
	if err != nil {
		return fmt.Errorf("failed to kill job: %w", err)
	}

	// Output response
	output, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format response: %w", err)
	}

	fmt.Println(string(output))
	return nil
}

func handleCleanup(c *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("job ID required")
	}

	jobID := args[0]
	response, err := c.Cleanup(jobID)
	if err != nil {
		return fmt.Errorf("failed to cleanup job: %w", err)
	}

	// Output response
	output, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format response: %w", err)
	}

	fmt.Println(string(output))
	return nil
}

func handleGetHistory(c *client.Client, args []string) error {
	limit := 10
	offset := 0

	if len(args) > 0 {
		var err error
		limit, err = strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid limit value: %s", args[0])
		}
	}
	if len(args) > 1 {
		var err error
		offset, err = strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid offset value: %s", args[1])
		}
	}

	response, err := c.GetHistory(limit, offset)
	if err != nil {
		return fmt.Errorf("failed to get history: %w", err)
	}

	// Output response
	output, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format response: %w", err)
	}

	fmt.Println(string(output))
	return nil
}

func handleHealth(c *client.Client, args []string) error {
	response, err := c.Health()
	if err != nil {
		return fmt.Errorf("failed to get health: %w", err)
	}

	// Display service URL
	fmt.Printf("Service URL: %s\n\n", c.GetBaseURL())

	// Display health status with color-coded output
	statusSymbol := "✅"
	if response.Status != "healthy" {
		statusSymbol = "❌"
	}

	fmt.Printf("%s Overall Status: %s\n", statusSymbol, response.Status)
	fmt.Printf("Timestamp: %s\n\n", response.Timestamp)

	fmt.Println("Services:")
	for service, status := range response.Services {
		serviceSymbol := "✅"
		if status != "healthy" {
			serviceSymbol = "❌"
		}
		fmt.Printf("  %s %s: %s\n", serviceSymbol, service, status)
	}

	return nil
}

// watchJobProgress watches a job's progress in real-time using Consul KV
// If historyMgr is provided, logs and final status are written to local history
// If jobConfig is provided and has python_command, runs python tests when status is TESTING_EXTERNAL
func watchJobProgress(jobID string, serviceURL string, historyMgr *history.Manager, submissionTime time.Time, jobConfig *types.JobConfig) error {
	fmt.Printf("Watching job: %s\n", jobID)
	fmt.Printf("Service URL: %s\n\n", serviceURL)

	// Create Consul client
	consulClient, err := consul.NewClient("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to create Consul client: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to polling mode. Use 'jobforge get-status %s' to check status manually.\n", jobID)
		return fmt.Errorf("failed to create Consul client: %w", err)
	}

	// Create channels for updates and errors
	updates := make(chan consul.JobUpdate, 10)
	errors := make(chan error, 10)

	// Start watching in background
	go consulClient.WatchJob(jobID, updates, errors)

	var lastStatus types.JobStatus
	var lastPhase string
	pythonTestsTriggered := false // Flag to ensure we only run python tests once

	// Process updates until job completes
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				// Channel closed, job finished
				return nil
			}

			// Only print if status or phase changed
			if update.Status != lastStatus || update.Phase != lastPhase {
				timestamp := update.Timestamp.Format("15:04:05")

				statusSymbol := getStatusSymbol(update.Status)
				fmt.Printf("[%s] %s Status: %s", timestamp, statusSymbol, update.Status)

				if update.Phase != "" {
					fmt.Printf(" | Phase: %s", update.Phase)
				}

				if update.Error != "" {
					fmt.Printf(" | Error: %s", update.Error)
				}

				fmt.Println()

				lastStatus = update.Status
				lastPhase = update.Phase
			}

			// Handle external python tests
			if update.Status == types.StatusTestingExternal && !pythonTestsTriggered {
				if jobConfig != nil && jobConfig.Test != nil && jobConfig.Test.PythonCommand != "" {
					pythonTestsTriggered = true
					fmt.Printf("\n[%s] 🐍 Starting external Python tests...\n", time.Now().Format("15:04:05"))

					err := runPythonTests(jobID, serviceURL, jobConfig)
					if err != nil {
						fmt.Printf("\n❌ Python tests failed: %v\n", err)
						// Continue watching - server will update status based on our result report
					}
				}
			}

			// Check if job reached terminal state
			if update.Status == types.StatusSucceeded {
				fmt.Printf("\n✅ Job completed successfully\n")

				// Write complete history if history manager is available
				if historyMgr != nil {
					if err := writeCompleteHistory(jobID, serviceURL, historyMgr, submissionTime); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to write complete history: %v\n", err)
					} else {
						fmt.Printf("Complete history written to: %s\n", historyMgr.GetBuildDir(jobID))
					}
				}

				return nil
			} else if update.Status == types.StatusFailed {
				fmt.Printf("\n❌ Job failed")
				if update.Error != "" {
					fmt.Printf(": %s", update.Error)
				}
				fmt.Println()

				// Write complete history even on failure if history manager is available
				if historyMgr != nil {
					if err := writeCompleteHistory(jobID, serviceURL, historyMgr, submissionTime); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to write complete history: %v\n", err)
					} else {
						fmt.Printf("Complete history written to: %s\n", historyMgr.GetBuildDir(jobID))
					}
				}

				return fmt.Errorf("job failed")
			}

		case err, ok := <-errors:
			if !ok {
				// Error channel closed
				continue
			}

			// Log non-fatal errors but continue watching
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
	}
}

// getStatusSymbol returns an emoji symbol for the given job status
func getStatusSymbol(status types.JobStatus) string {
	switch status {
	case types.StatusPending:
		return "⏳"
	case types.StatusBuilding:
		return "🔨"
	case types.StatusTesting:
		return "🧪"
	case types.StatusTestingExternal:
		return "🐍"
	case types.StatusPublishing:
		return "📦"
	case types.StatusSucceeded:
		return "✅"
	case types.StatusFailed:
		return "❌"
	default:
		return "❓"
	}
}

// runPythonTests runs external python tests against the test container
func runPythonTests(jobID, serviceURL string, jobConfig *types.JobConfig) error {
	httpClient := client.NewClient(serviceURL)

	// 1. Get test endpoint from server (with retries)
	var endpoint *types.GetTestEndpointResponse
	var err error

	fmt.Printf("Waiting for test container endpoint...\n")
	for i := 0; i < 30; i++ {
		endpoint, err = httpClient.GetTestEndpoint(jobID)
		if err == nil && endpoint.ServiceHost != "" && endpoint.ServicePort > 0 {
			break
		}
		fmt.Printf("  Waiting for test container to start (%d/30)...\n", i+1)
		time.Sleep(2 * time.Second)
	}

	if endpoint == nil || endpoint.ServiceHost == "" {
		return reportTestFailure(httpClient, jobID, "Failed to get test container endpoint", -1)
	}

	fmt.Printf("Test container running at %s:%d\n", endpoint.ServiceHost, endpoint.ServicePort)

	// 2. Wait for health check
	healthEndpoint := "/health"
	if jobConfig.Test.HealthEndpoint != "" {
		healthEndpoint = jobConfig.Test.HealthEndpoint
	}

	healthURL := fmt.Sprintf("http://%s:%d%s",
		endpoint.ServiceHost,
		endpoint.ServicePort,
		healthEndpoint)

	timeout := 60
	if jobConfig.Test.HealthTimeout > 0 {
		timeout = jobConfig.Test.HealthTimeout
	}

	fmt.Printf("Waiting for health check at %s (timeout: %ds)...\n", healthURL, timeout)

	healthy := false
	healthHTTPClient := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < timeout/2; i++ {
		resp, err := healthHTTPClient.Get(healthURL)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			healthy = true
			fmt.Println("Health check passed!")
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}

	if !healthy {
		return reportTestFailure(httpClient, jobID, fmt.Sprintf("Health check timeout after %ds", timeout), -1)
	}

	// 3. Run python-executor
	fmt.Println("Running Python tests...")
	startTime := time.Now()

	cmdParts := strings.Fields(jobConfig.Test.PythonCommand)
	if len(cmdParts) == 0 {
		return reportTestFailure(httpClient, jobID, "Empty python_command", -1)
	}

	cmd := exec.Command(cmdParts[0], cmdParts[1:]...)

	// Set working directory
	if jobConfig.Test.PythonCwd != "" {
		cmd.Dir = jobConfig.Test.PythonCwd
	}

	// Set environment variables
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, fmt.Sprintf("SERVICE_HOST=%s", endpoint.ServiceHost))
	cmd.Env = append(cmd.Env, fmt.Sprintf("SERVICE_PORT=%d", endpoint.ServicePort))

	// Capture output while also printing to console
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdout)
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	err = cmd.Run()
	duration := time.Since(startTime)

	// 4. Report results to server
	exitCode := 0
	success := true
	if err != nil {
		success = false
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	result := &types.ReportTestResultRequest{
		JobID:    jobID,
		Success:  success,
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration.Milliseconds(),
	}

	_, err = httpClient.ReportTestResult(result)
	if err != nil {
		return fmt.Errorf("failed to report test result: %w", err)
	}

	if !success {
		return fmt.Errorf("tests failed with exit code %d", exitCode)
	}

	fmt.Printf("Python tests completed successfully in %v\n", duration)
	return nil
}

// reportTestFailure is a helper to report test failures to the server
func reportTestFailure(c *client.Client, jobID, message string, exitCode int) error {
	result := &types.ReportTestResultRequest{
		JobID:    jobID,
		Success:  false,
		ExitCode: exitCode,
		Stderr:   message,
	}
	c.ReportTestResult(result)
	return fmt.Errorf(message)
}

// writeCompleteHistory fetches job details and logs from server and writes complete history
func writeCompleteHistory(jobID string, serviceURL string, historyMgr *history.Manager, submissionTime time.Time) error {
	// Create a client to fetch job details
	c := client.NewClient(serviceURL)

	// Fetch job status
	statusResp, err := c.GetStatus(jobID)
	if err != nil {
		return fmt.Errorf("failed to fetch job status: %w", err)
	}

	// Fetch logs for all phases
	logsResp, err := c.GetLogs(jobID, "")
	if err != nil {
		return fmt.Errorf("failed to fetch job logs: %w", err)
	}

	// Construct a Job object for history writing
	job := &types.Job{
		ID:      jobID,
		Status:  statusResp.Status,
		Metrics: statusResp.Metrics,
		Error:   statusResp.Error,
		Logs:    logsResp.Logs,
	}

	// Set timing fields from metrics
	job.StartedAt = statusResp.Metrics.JobStart
	job.FinishedAt = statusResp.Metrics.JobEnd

	// We need to fetch the job config - it's not in the status response
	// For now, we'll create a minimal reconstruction
	// In a real implementation, we might need to add an endpoint to fetch full job details
	// For now, use empty config or try to reconstruct from available info
	job.Config = types.JobConfig{}

	// Write phase logs
	if len(logsResp.Logs.Build) > 0 {
		if err := historyMgr.WritePhaseLogs(jobID, "build", logsResp.Logs.Build); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write build logs: %v\n", err)
		}
	}
	if len(logsResp.Logs.Test) > 0 {
		if err := historyMgr.WritePhaseLogs(jobID, "test", logsResp.Logs.Test); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write test logs: %v\n", err)
		}
	}
	if len(logsResp.Logs.Publish) > 0 {
		if err := historyMgr.WritePhaseLogs(jobID, "publish", logsResp.Logs.Publish); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write publish logs: %v\n", err)
		}
	}

	// Read the initial metadata to get the full job config
	// This was written during submission
	initialMetadata, err := readInitialMetadata(historyMgr.GetBuildDir(jobID))
	if err == nil {
		job.Config = initialMetadata.JobConfig
	}

	// Write complete metadata
	if err := historyMgr.WriteCompleteMetadata(jobID, job, submissionTime); err != nil {
		return fmt.Errorf("failed to write complete metadata: %w", err)
	}

	// Write status file
	if err := historyMgr.WriteStatusFile(jobID, job); err != nil {
		return fmt.Errorf("failed to write status file: %w", err)
	}

	// Update history.md with final entry
	var duration time.Duration
	if job.FinishedAt != nil && job.StartedAt != nil {
		duration = job.FinishedAt.Sub(*job.StartedAt)
	}

	entry := history.HistoryEntry{
		JobID:     jobID,
		Branch:    extractBranchFromConfig(job.Config),
		GitRef:    job.Config.GitRef,
		Timestamp: submissionTime,
		Status:    job.Status,
		Duration:  duration,
		ImageTags: job.Config.ImageTags,
		Error:     job.Error,
	}

	if err := historyMgr.UpdateHistoryFile(entry); err != nil {
		return fmt.Errorf("failed to update history file: %w", err)
	}

	return nil
}

// readInitialMetadata reads the initial metadata from the build directory
func readInitialMetadata(buildDir string) (*history.InitialMetadata, error) {
	metadataPath := filepath.Join(buildDir, "metadata.yaml")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}

	var metadata history.InitialMetadata
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}