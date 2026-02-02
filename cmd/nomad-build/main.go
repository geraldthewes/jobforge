package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"nomad-mcp-builder/pkg/client"
	"nomad-mcp-builder/pkg/config"
	"nomad-mcp-builder/pkg/consul"
	"nomad-mcp-builder/pkg/git"
	"nomad-mcp-builder/pkg/history"
	"nomad-mcp-builder/pkg/types"

	pyexec "github.com/geraldthewes/python-executor/pkg/client"
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

  Lock Management:
    list-locks [options]              List all build locks
    force-unlock [options] <lock-key> Force release a build lock
    list-active-jobs --owner <owner>  List active jobs for an owner

  Storage Management:
    prune-storage [options]           Prune buildah storage cache to free disk space

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

  --no-git-check            Skip the unpushed commits safety check
                            By default, if running from a git repo that matches the
                            configured repo_url, warns if there are local commits
                            that haven't been pushed to the remote

  --fail-on-git-check       Fail immediately if unpushed commits are detected
                            (non-interactive mode for CI/CD pipelines)

GET-LOGS OPTIONS:
  --phase <phase>           Get logs for specific phase only (build, test, or publish)
                            If not specified, returns logs for all phases

PRUNE-STORAGE OPTIONS:
  <node-name>               Target a specific node by name (e.g., gpu005.cluster)
  --dry-run                 Preview what would be cleaned without deleting
  --project <name>          Only prune cache for a specific project/image
  --all                     Aggressive prune: remove all images and caches
  --all-nodes               Prune on all Nomad nodes (uses sysbatch)
  --force                   Skip safety checks (required with --all if builds active)
  -w, --watch               Watch prune job progress

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

  Lock Management:
    # List all build locks
    jobforge list-locks

    # List only stale locks (job completed but lock not released)
    jobforge list-locks --stale-only

    # List locks for a specific owner
    jobforge list-locks --owner gerald

    # List locks with full details (no truncation)
    jobforge list-locks --no-trunc

    # Force unlock a specific lock
    jobforge force-unlock image-registry.cluster-5000-myapp-main

    # Force unlock without confirmation
    jobforge force-unlock image-registry.cluster-5000-myapp-main --force

    # Clear all stale locks
    jobforge force-unlock --stale-only

    # Nuclear option: clear ALL locks (dangerous!)
    jobforge force-unlock --all --force

    # List active jobs for an owner
    jobforge list-active-jobs --owner gerald

ENVIRONMENT VARIABLES:
  JOB_SERVICE_URL           Service URL (overrides default, can be overridden by -u flag)

FILES:
  deploy/global.yaml        Global configuration (optional)
  build.yaml                Per-build configuration

DOCUMENTATION:
  docs/usage.md             Quick start and usage guide
  docs/JobSpec.md           Complete job configuration reference

For more information, visit: https://github.com/geraldthewes/jobforge
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
	case "list-locks":
		return handleListLocks(c, commandArgs)
	case "force-unlock":
		return handleForceUnlock(c, commandArgs)
	case "list-active-jobs":
		return handleListActiveJobs(c, commandArgs)
	case "prune-storage":
		return handlePruneStorage(c, commandArgs)
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
	var noGitCheck bool
	var failOnGitCheck bool

	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--watch" || arg == "-w" {
			watch = true
			i++
		} else if arg == "--history" {
			enableHistory = true
			i++
		} else if arg == "--no-git-check" {
			noGitCheck = true
			i++
		} else if arg == "--fail-on-git-check" {
			failOnGitCheck = true
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

	// Automatically enable --watch for python tests and inform the user
	if jobConfig.Test != nil && jobConfig.Test.PythonFile != "" && !watch {
		watch = true
		fmt.Println("Note: Python test file detected - watch mode enabled automatically")
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

	// Check for unpushed commits unless --no-git-check is specified
	if !noGitCheck && jobConfig.RepoURL != "" {
		if err := checkAndWarnUnpushedCommits(jobConfig, failOnGitCheck); err != nil {
			return err
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

// checkAndWarnUnpushedCommits checks for unpushed commits and prompts the user for confirmation.
// If failOnGitCheck is true, returns an error immediately without prompting (for CI/CD pipelines).
// Returns an error if the user aborts or failOnGitCheck is set, nil if they proceed or no warning is needed.
func checkAndWarnUnpushedCommits(jobConfig *types.JobConfig, failOnGitCheck bool) error {
	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		// Can't determine directory, skip check
		return nil
	}

	// Check for unpushed commits
	status, err := git.CheckUnpushedCommits(cwd, jobConfig.GitRef, jobConfig.RepoURL)
	if err != nil {
		// Error checking, skip (don't block the build for git errors)
		return nil
	}

	// Skip if not a git repo or different repo
	if !status.IsGitRepo || !status.RepoMatches {
		return nil
	}

	// No unpushed commits - proceed
	if !status.HasUnpushed {
		return nil
	}

	// Display warning
	fmt.Println()
	fmt.Println("WARNING: Unpushed commits detected!")
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("  Local branch: %s\n", status.LocalBranch)
	if status.ConfigGitRef != "" {
		fmt.Printf("  Git ref in config: %s\n", status.ConfigGitRef)
	}
	fmt.Printf("  Unpushed commits: %d\n", status.UnpushedCount)

	if len(status.CommitSummary) > 0 {
		fmt.Println()
		fmt.Println("  Recent unpushed commits:")
		for _, summary := range status.CommitSummary {
			fmt.Printf("    - %s\n", summary)
		}
	}

	fmt.Println()
	fmt.Println("The remote build will use code from the remote repository,")
	fmt.Println("which does not include your local changes.")

	// If --fail-on-git-check is set, fail immediately without prompting
	if failOnGitCheck {
		return fmt.Errorf("unpushed commits detected: %d commits ahead of remote (CI/CD mode: --fail-on-git-check)", status.UnpushedCount)
	}

	fmt.Println()
	fmt.Print("Proceed anyway? Type 'yes' to continue: ")

	// Read user confirmation
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	response = strings.TrimSpace(strings.ToLower(response))
	if response != "yes" {
		return fmt.Errorf("aborted: unpushed commits detected (use --no-git-check to skip this warning)")
	}

	return nil
}

// displayAllocationWarnings prints allocation warnings to the console
func displayAllocationWarnings(allocations *types.JobAllocations) {
	if allocations == nil || !allocations.HasAnyWarning {
		return
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("WARNING: Multiple allocations detected")
	fmt.Println(strings.Repeat("=", 60))

	displayPhaseAllocations := func(phase *types.PhaseAllocations) {
		if phase == nil || !phase.HasWarning {
			return
		}

		fmt.Printf("\n[%s Phase] %s\n", strings.ToUpper(phase.Phase), phase.WarningMessage)
		fmt.Printf("  Nomad Job: %s\n", phase.NomadJobID)
		fmt.Println("  Allocations:")

		for i, alloc := range phase.Allocations {
			fmt.Printf("    %d. ID: %s\n", i+1, alloc.ID)
			fmt.Printf("       Status: %s (desired: %s)\n", alloc.ClientStatus, alloc.DesiredStatus)
			if alloc.NodeName != "" {
				fmt.Printf("       Node: %s (%s)\n", alloc.NodeName, alloc.NodeID[:8])
			} else if alloc.NodeID != "" {
				fmt.Printf("       Node: %s\n", alloc.NodeID[:8])
			}
			fmt.Printf("       Created: %s\n", alloc.CreatedAt.Format(time.RFC3339))
			if alloc.FailedReason != "" {
				fmt.Printf("       Failed: %s\n", alloc.FailedReason)
			}
		}
	}

	displayPhaseAllocations(allocations.Build)
	for i := range allocations.Test {
		displayPhaseAllocations(&allocations.Test[i])
	}
	displayPhaseAllocations(allocations.Publish)

	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()
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

	// Display allocation warnings if present
	displayAllocationWarnings(response.Allocations)

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
	fmt.Printf("Service URL: %s\n", serviceURL)

	// Display the repository URL if available
	if jobConfig != nil && jobConfig.RepoURL != "" {
		fmt.Printf("Repository: %s\n", jobConfig.RepoURL)
	}

	// Display the Docker image being built if available
	if jobConfig != nil && jobConfig.ImageName != "" {
		imageName := jobConfig.ImageName
		if jobConfig.RegistryURL != "" {
			fmt.Printf("Building: %s/%s\n", jobConfig.RegistryURL, imageName)
		} else {
			fmt.Printf("Building: %s\n", imageName)
		}
	}
	fmt.Println()

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

				// Check for allocation warnings when phase changes
				if update.Phase != lastPhase {
					c := client.NewClient(serviceURL)
					if statusResp, err := c.GetStatus(jobID); err == nil && statusResp.Allocations != nil {
						displayAllocationWarnings(statusResp.Allocations)
					}
				}

				lastStatus = update.Status
				lastPhase = update.Phase
			}

			// Handle external python tests
			if update.Status == types.StatusTestingExternal && !pythonTestsTriggered {
				if jobConfig != nil && jobConfig.Test != nil && jobConfig.Test.PythonFile != "" {
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

				// Fetch final status to get image metadata including sizes
				c := client.NewClient(serviceURL)
				if statusResp, err := c.GetStatus(jobID); err == nil && len(statusResp.Metrics.PublishedImages) > 0 {
					fmt.Printf("\nPublished Images:\n")
					for _, img := range statusResp.Metrics.PublishedImages {
						fmt.Printf("  %s (%s)\n", img.Name, img.SizeHuman)
					}
				} else {
					// Fallback to existing behavior if metadata not available
					if jobConfig != nil && jobConfig.ImageName != "" {
						tags := jobConfig.ImageTags
						if len(tags) == 0 {
							tags = []string{jobID} // Default tag is job ID
						}
						for _, tag := range tags {
							if jobConfig.RegistryURL != "" {
								fmt.Printf("Published: %s/%s:%s\n", jobConfig.RegistryURL, jobConfig.ImageName, tag)
							} else {
								fmt.Printf("Published: %s:%s\n", jobConfig.ImageName, tag)
							}
						}
					}
				}

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

				// Attempt to fetch and display logs for the failed phase
				var failedPhase string
				if update.Phase != "" {
					failedPhase = update.Phase
				}

				// Display logs for the failed phase if available
				if failedPhase != "" {
					// Create a client for displaying logs
					client := client.NewClient(serviceURL)
					displayFailedJobLogs(client, jobID, failedPhase)
				}

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

	// 3. Run python-executor via Go client library
	fmt.Println("Running Python tests...")
	startTime := time.Now()

	if jobConfig.Test.PythonFile == "" {
		return reportTestFailure(httpClient, jobID, "Empty python_file", -1)
	}

	// Determine working directory
	workDir := "."
	if jobConfig.Test.PythonCwd != "" {
		workDir = jobConfig.Test.PythonCwd
	}

	// Read Python test file
	pythonFilePath := filepath.Join(workDir, jobConfig.Test.PythonFile)
	pythonContent, err := os.ReadFile(pythonFilePath)
	if err != nil {
		return reportTestFailure(httpClient, jobID, fmt.Sprintf("Failed to read python file %s: %v", pythonFilePath, err), -1)
	}

	// Build file map for tar
	files := map[string]string{
		jobConfig.Test.PythonFile: string(pythonContent),
	}

	// Read requirements.txt if provided
	var requirementsContent string
	if jobConfig.Test.PythonRequirements != "" {
		reqPath := filepath.Join(workDir, jobConfig.Test.PythonRequirements)
		reqBytes, err := os.ReadFile(reqPath)
		if err != nil {
			return reportTestFailure(httpClient, jobID, fmt.Sprintf("Failed to read requirements file %s: %v", reqPath, err), -1)
		}
		requirementsContent = string(reqBytes)
		files[jobConfig.Test.PythonRequirements] = requirementsContent
	}

	// Create tar from files
	tarData, err := pyexec.TarFromMap(files)
	if err != nil {
		return reportTestFailure(httpClient, jobID, fmt.Sprintf("Failed to create tar: %v", err), -1)
	}

	// Get pyexec server URL from environment or use default
	pyexecServer := os.Getenv("PYEXEC_SERVER")
	if pyexecServer == "" {
		pyexecServer = "http://pyexec.cluster:9999"
	}

	// Create python-executor client
	pyClient := pyexec.New(pyexecServer)

	// Build metadata with environment variables for python-executor
	envVars := []string{
		fmt.Sprintf("SERVICE_HOST=%s", endpoint.ServiceHost),
		fmt.Sprintf("SERVICE_PORT=%d", endpoint.ServicePort),
	}

	// Add user-defined python environment variables
	for key, value := range jobConfig.Test.PythonEnv {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}

	metadata := &pyexec.Metadata{
		Entrypoint:      jobConfig.Test.PythonFile,
		RequirementsTxt: requirementsContent,
		EnvVars:         envVars,
	}

	// Execute the Python tests
	ctx := context.Background()
	result, err := pyClient.ExecuteSync(ctx, tarData, metadata)
	duration := time.Since(startTime)

	// Print output to console
	if result != nil {
		if result.Stdout != "" {
			fmt.Print(result.Stdout)
		}
		if result.Stderr != "" {
			fmt.Fprint(os.Stderr, result.Stderr)
		}
	}

	// 4. Report results to server
	exitCode := 0
	success := true
	var stdout, stderr string

	if err != nil {
		success = false
		exitCode = -1
		stderr = fmt.Sprintf("Python executor error: %v", err)
		fmt.Fprintf(os.Stderr, "%s\n", stderr)
	} else if result != nil {
		exitCode = result.ExitCode
		stdout = result.Stdout
		stderr = result.Stderr
		if result.ExitCode != 0 {
			success = false
		}
	}

	testResult := &types.ReportTestResultRequest{
		JobID:    jobID,
		Success:  success,
		ExitCode: exitCode,
		Stdout:   stdout,
		Stderr:   stderr,
		Duration: duration.Milliseconds(),
	}

	_, err = httpClient.ReportTestResult(testResult)
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
	return fmt.Errorf("%s", message)
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

// displayFailedJobLogs displays logs for a failed job
func displayFailedJobLogs(c *client.Client, jobID string, failedPhase string) error {
	// Attempt to fetch and display logs for the failed phase
	if failedPhase != "" {
		logsResp, err := c.GetLogs(jobID, failedPhase)
		if err == nil {
			fmt.Printf("Failed phase: %s\n", failedPhase)
			fmt.Printf("%s logs:\n", strings.Title(failedPhase))

			// Display logs for the failed phase
			switch failedPhase {
			case "build":
				if len(logsResp.Logs.Build) > 0 {
					for _, logLine := range logsResp.Logs.Build {
						fmt.Printf("%s\n", logLine)
					}
				}
			case "test":
				if len(logsResp.Logs.Test) > 0 {
					for _, logLine := range logsResp.Logs.Test {
						fmt.Printf("%s\n", logLine)
					}
				}
			case "publish":
				if len(logsResp.Logs.Publish) > 0 {
					for _, logLine := range logsResp.Logs.Publish {
						fmt.Printf("%s\n", logLine)
					}
				}
			}
		}
	}
	return nil
}

// handleListLocks handles the list-locks command
func handleListLocks(c *client.Client, args []string) error {
	var staleOnly bool
	var owner string
	var noTrunc bool

	// Parse flags
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--stale-only" {
			staleOnly = true
			i++
		} else if arg == "--no-trunc" {
			noTrunc = true
			i++
		} else if arg == "--owner" {
			if i+1 >= len(args) {
				return fmt.Errorf("--owner flag requires a value")
			}
			owner = args[i+1]
			i += 2
		} else if strings.HasPrefix(arg, "--owner=") {
			owner = strings.TrimPrefix(arg, "--owner=")
			i++
		} else if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown flag: %s", arg)
		} else {
			i++
		}
	}

	response, err := c.ListLocks(staleOnly, owner)
	if err != nil {
		return fmt.Errorf("failed to list locks: %w", err)
	}

	// Print header
	fmt.Printf("Build Locks (Total: %d, Stale: %d)\n", response.TotalCount, response.StaleCount)

	if len(response.Locks) == 0 {
		fmt.Println(strings.Repeat("-", 130))
		fmt.Println("No locks found.")
		return nil
	}

	if noTrunc {
		// No truncation - print each lock with full details
		fmt.Println(strings.Repeat("-", 130))
		for i, lock := range response.Locks {
			if i > 0 {
				fmt.Println()
			}
			staleStr := "No"
			if lock.IsStale {
				staleStr = "Yes"
				if lock.StaleReason != "" {
					staleStr = fmt.Sprintf("Yes (%s)", lock.StaleReason)
				}
			}
			fmt.Printf("Lock Key:  %s\n", lock.LockKey)
			fmt.Printf("Job ID:    %s\n", lock.JobID)
			fmt.Printf("Owner:     %s\n", lock.Owner)
			fmt.Printf("Image:     %s\n", lock.ImageName)
			fmt.Printf("Git Ref:   %s\n", lock.GitRef)
			fmt.Printf("Registry:  %s\n", lock.RegistryURL)
			fmt.Printf("Phase:     %s\n", lock.Phase)
			fmt.Printf("Age:       %s\n", formatDuration(lock.Age))
			fmt.Printf("Stale:     %s\n", staleStr)
		}
		return nil
	}

	// Table format with truncation
	fmt.Println(strings.Repeat("-", 130))

	// Print table header
	fmt.Printf("%-60s %-12s %-10s %-15s %-10s %-10s %-5s\n",
		"LOCK KEY", "JOB ID", "OWNER", "IMAGE", "PHASE", "AGE", "STALE")
	fmt.Println(strings.Repeat("-", 130))

	for _, lock := range response.Locks {
		jobID := lock.JobID
		if len(jobID) > 12 {
			jobID = jobID[:12]
		}

		imageName := lock.ImageName
		if len(imageName) > 15 {
			imageName = imageName[:15]
		}

		// Show full lock key - this is critical for force-unlock command
		lockKey := lock.LockKey

		ownerStr := lock.Owner
		if len(ownerStr) > 10 {
			ownerStr = ownerStr[:10]
		}

		phaseStr := lock.Phase
		if len(phaseStr) > 10 {
			phaseStr = phaseStr[:10]
		}

		ageStr := formatDuration(lock.Age)

		staleStr := "No"
		if lock.IsStale {
			staleStr = "Yes"
			if lock.StaleReason != "" {
				staleStr = fmt.Sprintf("Yes (%s)", lock.StaleReason)
			}
		}

		fmt.Printf("%-60s %-12s %-10s %-15s %-10s %-10s %s\n",
			lockKey, jobID, ownerStr, imageName, phaseStr, ageStr, staleStr)
	}

	return nil
}

// handleForceUnlock handles the force-unlock command
func handleForceUnlock(c *client.Client, args []string) error {
	var lockKey string
	var staleOnly bool
	var all bool
	var force bool

	// Parse flags
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--stale-only" {
			staleOnly = true
			i++
		} else if arg == "--all" {
			all = true
			i++
		} else if arg == "--force" || arg == "-f" {
			force = true
			i++
		} else if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown flag: %s", arg)
		} else if lockKey == "" {
			lockKey = arg
			i++
		} else {
			i++
		}
	}

	// Validate
	if lockKey == "" && !staleOnly && !all {
		return fmt.Errorf("must specify lock-key, --stale-only, or --all")
	}

	if all && !force {
		fmt.Println("WARNING: This will clear ALL locks, including active ones!")
		fmt.Print("Are you sure? Type 'yes' to confirm: ")
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	if lockKey != "" && !force {
		fmt.Printf("This will force release the lock: %s\n", lockKey)
		fmt.Print("Are you sure? Type 'yes' to confirm: ")
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	req := &types.ForceUnlockRequest{
		LockKey:   lockKey,
		StaleOnly: staleOnly,
		All:       all,
		Force:     force || staleOnly, // --stale-only doesn't need --force
	}

	response, err := c.ForceUnlock(req)
	if err != nil {
		return fmt.Errorf("failed to force unlock: %w", err)
	}

	fmt.Printf("%s\n", response.Message)

	if len(response.UnlockedKeys) > 0 {
		fmt.Printf("\nUnlocked keys (%d):\n", len(response.UnlockedKeys))
		for _, key := range response.UnlockedKeys {
			fmt.Printf("  ✅ %s\n", key)
		}
	}

	if len(response.FailedKeys) > 0 {
		fmt.Printf("\nFailed to unlock (%d):\n", len(response.FailedKeys))
		for _, key := range response.FailedKeys {
			fmt.Printf("  ❌ %s\n", key)
		}
	}

	return nil
}

// handleListActiveJobs handles the list-active-jobs command
func handleListActiveJobs(c *client.Client, args []string) error {
	var owner string

	// Parse flags
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--owner" {
			if i+1 >= len(args) {
				return fmt.Errorf("--owner flag requires a value")
			}
			owner = args[i+1]
			i += 2
		} else if strings.HasPrefix(arg, "--owner=") {
			owner = strings.TrimPrefix(arg, "--owner=")
			i++
		} else if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown flag: %s", arg)
		} else if owner == "" {
			// Allow owner as positional argument
			owner = arg
			i++
		} else {
			i++
		}
	}

	if owner == "" {
		return fmt.Errorf("--owner is required")
	}

	response, err := c.ListActiveJobs(owner)
	if err != nil {
		return fmt.Errorf("failed to list active jobs: %w", err)
	}

	fmt.Printf("Active Jobs for Owner: %s\n", response.Owner)
	fmt.Printf("Active Count: %d / %d (limit)\n", response.ActiveCount, response.Limit)
	fmt.Println(strings.Repeat("-", 80))

	if len(response.ActiveJobs) == 0 {
		fmt.Println("No active jobs found.")
		return nil
	}

	// Print table header
	fmt.Printf("%-36s %-12s %-20s %-15s\n",
		"JOB ID", "STATUS", "IMAGE", "STARTED")
	fmt.Println(strings.Repeat("-", 80))

	for _, job := range response.ActiveJobs {
		imageName := job.ImageName
		if len(imageName) > 20 {
			imageName = imageName[:20]
		}

		started := job.StartedAt.Format("15:04:05")

		fmt.Printf("%-36s %-12s %-20s %-15s\n",
			job.JobID, job.Status, imageName, started)
	}

	return nil
}

// formatDuration formats a duration for display
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	} else if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	} else if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// handlePruneStorage handles the prune-storage command
func handlePruneStorage(c *client.Client, args []string) error {
	var nodeName string
	var dryRun bool
	var project string
	var all bool
	var allNodes bool
	var force bool
	var watch bool

	// Parse flags and positional argument (node name)
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--dry-run" {
			dryRun = true
			i++
		} else if arg == "--project" {
			if i+1 >= len(args) {
				return fmt.Errorf("--project flag requires a value")
			}
			project = args[i+1]
			i += 2
		} else if strings.HasPrefix(arg, "--project=") {
			project = strings.TrimPrefix(arg, "--project=")
			i++
		} else if arg == "--all" {
			all = true
			i++
		} else if arg == "--all-nodes" {
			allNodes = true
			i++
		} else if arg == "--force" || arg == "-f" {
			force = true
			i++
		} else if arg == "--watch" || arg == "-w" {
			watch = true
			i++
		} else if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown flag: %s", arg)
		} else {
			// Positional argument: node name
			nodeName = arg
			i++
		}
	}

	// Validate: --all-nodes and node name are mutually exclusive
	if allNodes && nodeName != "" {
		return fmt.Errorf("--all-nodes and node name (%s) are mutually exclusive", nodeName)
	}

	// Safety confirmation for aggressive prune without --force
	if all && !force && !dryRun {
		fmt.Println("WARNING: Aggressive prune will remove ALL cached images and build layers!")
		fmt.Println("This cannot be undone and will slow down subsequent builds.")
		fmt.Print("Are you sure? Type 'yes' to confirm: ")
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// Build request
	req := &types.PruneStorageRequest{
		NodeName: nodeName,
		Project:  project,
		All:      all,
		AllNodes: allNodes,
		DryRun:   dryRun,
		Force:    force,
	}

	// Submit prune job
	response, err := c.PruneStorage(req)
	if err != nil {
		return fmt.Errorf("failed to submit prune storage job: %w", err)
	}

	// Display response
	if response.Success {
		fmt.Printf("Prune storage job submitted: %s\n", response.JobID)
		fmt.Printf("Message: %s\n", response.Message)
	} else {
		return fmt.Errorf("failed to submit prune job: %s", response.Message)
	}

	// Display warnings
	for _, warning := range response.Warnings {
		fmt.Printf("Warning: %s\n", warning)
	}

	// Watch job progress if requested
	if watch && response.JobID != "" {
		fmt.Println()
		return watchPruneJob(c, response.JobID)
	}

	// Provide hint about watching
	if !watch && response.JobID != "" {
		fmt.Printf("\nTo watch progress: jobforge get-status %s\n", response.JobID)
		fmt.Printf("To see logs: jobforge get-logs %s\n", response.JobID)
	}

	return nil
}

// watchPruneJob watches a prune job until it completes
func watchPruneJob(c *client.Client, pruneJobID string) error {
	fmt.Printf("Watching prune job: %s\n", pruneJobID)

	// Poll for status updates
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeout := time.After(10 * time.Minute) // Max wait time for prune

	for {
		select {
		case <-ticker.C:
			// Get job status using the general status endpoint
			// The prune job is a regular Nomad batch job
			status, logs, err := c.GetPruneJobStatus(pruneJobID)
			if err != nil {
				// Job might not be found yet if just submitted
				continue
			}

			// Print status
			timestamp := time.Now().Format("15:04:05")
			switch status {
			case "running", "pending":
				fmt.Printf("[%s] ⏳ Status: %s\n", timestamp, status)
			case "complete":
				fmt.Printf("[%s] ✅ Prune completed successfully\n", timestamp)

				// Display logs
				if len(logs) > 0 {
					fmt.Println("\n=== Prune Job Logs ===")
					for _, line := range logs {
						fmt.Println(line)
					}
				}
				return nil
			case "failed":
				fmt.Printf("[%s] ❌ Prune job failed\n", timestamp)

				// Display logs
				if len(logs) > 0 {
					fmt.Println("\n=== Prune Job Logs ===")
					for _, line := range logs {
						fmt.Println(line)
					}
				}
				return fmt.Errorf("prune job failed")
			}

		case <-timeout:
			return fmt.Errorf("timed out waiting for prune job to complete")
		}
	}
}