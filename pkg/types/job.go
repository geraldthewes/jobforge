package types

import "time"

// JobStatus represents the current status of a build job
type JobStatus string

const (
	StatusPending         JobStatus = "PENDING"
	StatusBuilding        JobStatus = "BUILDING"
	StatusTesting         JobStatus = "TESTING"
	StatusTestingExternal JobStatus = "TESTING_EXTERNAL" // Waiting for CLI to run external python tests
	StatusPublishing      JobStatus = "PUBLISHING"
	StatusSucceeded       JobStatus = "SUCCEEDED"
	StatusFailed          JobStatus = "FAILED"
)

// VaultSecret defines a Vault secret path and field mappings for environment variables
type VaultSecret struct {
	Path   string            `json:"path" yaml:"path"`       // Vault secret path (e.g., "secret/data/aws/transcription")
	Fields map[string]string `json:"fields" yaml:"fields"`   // Field mappings: {"vault_field": "ENV_VAR_NAME"}
}

// Constraint defines a Nomad job constraint for node selection
type Constraint struct {
	Attribute string `json:"attribute" yaml:"attribute"` // Node attribute to match (e.g., "${meta.gpu-capable}")
	Value     string `json:"value" yaml:"value"`         // Expected value
	Operand   string `json:"operand" yaml:"operand"`     // Comparison operator: "=", "!=", "regexp", etc.
}

// TestConfig represents test phase configuration
type TestConfig struct {
	Commands       []string               `json:"commands,omitempty" yaml:"commands,omitempty"`             // Custom test commands to run
	EntryPoint     bool                   `json:"entry_point,omitempty" yaml:"entry_point,omitempty"`       // Run the image's default entrypoint/CMD as a test
	Env            map[string]string      `json:"env,omitempty" yaml:"env,omitempty"`                       // Environment variables for test execution
	VaultSecrets   []VaultSecret          `json:"vault_secrets,omitempty" yaml:"vault_secrets,omitempty"`   // Vault secrets to inject as environment variables
	VaultPolicies  []string               `json:"vault_policies,omitempty" yaml:"vault_policies,omitempty"` // Vault policies required to access secrets
	ResourceLimits *PhaseResourceLimits   `json:"resource_limits,omitempty" yaml:"resource_limits,omitempty"` // Test phase resource limits
	Timeout        *time.Duration         `json:"timeout,omitempty" yaml:"timeout,omitempty"`               // Test phase timeout
	GPURequired    bool                   `json:"gpu_required,omitempty" yaml:"gpu_required,omitempty"`     // Enable GPU runtime (nvidia) for test containers
	GPUCount       int                    `json:"gpu_count,omitempty" yaml:"gpu_count,omitempty"`           // Number of GPUs to allocate (0 = all available)
	Constraints    []Constraint           `json:"constraints,omitempty" yaml:"constraints,omitempty"`       // Custom node constraints for test job placement

	// External Python test configuration (runs via python-executor on CLI machine)
	PythonCwd      string `json:"python_cwd,omitempty" yaml:"python_cwd,omitempty"`           // Working directory for python tests (relative to repo root)
	PythonCommand  string `json:"python_command,omitempty" yaml:"python_command,omitempty"`   // Command to run (e.g., "python-executor run --file test.py")
	HealthEndpoint string `json:"health_endpoint,omitempty" yaml:"health_endpoint,omitempty"` // Health check endpoint to poll before tests (default: /health)
	HealthTimeout  int    `json:"health_timeout,omitempty" yaml:"health_timeout,omitempty"`   // Seconds to wait for health check (default: 60)
	ContainerPort  int    `json:"container_port,omitempty" yaml:"container_port,omitempty"`   // Port the container exposes (default: 8080)
}

// JobConfig represents the configuration for a build job
type JobConfig struct {
	Owner                   string   `json:"owner" yaml:"owner"`
	RepoURL                 string   `json:"repo_url" yaml:"repo_url"`
	GitRef                  string   `json:"git_ref" yaml:"git_ref"`
	GitCredentialsPath      string   `json:"git_credentials_path" yaml:"git_credentials_path"`
	DockerfilePath          string   `json:"dockerfile_path" yaml:"dockerfile_path"`
	ImageName               string   `json:"image_name" yaml:"image_name"`
	ImageTags               []string `json:"image_tags" yaml:"image_tags"`
	RegistryURL             string   `json:"registry_url" yaml:"registry_url"`
	RegistryCredentialsPath string   `json:"registry_credentials_path" yaml:"registry_credentials_path"`
	Test                    *TestConfig     `json:"test,omitempty" yaml:"test,omitempty"`                     // Test phase configuration
	ResourceLimits          *ResourceLimits `json:"resource_limits,omitempty" yaml:"resource_limits,omitempty"`
	BuildTimeout            *time.Duration  `json:"build_timeout,omitempty" yaml:"build_timeout,omitempty"`
	ClearCache              bool     `json:"clear_cache,omitempty" yaml:"clear_cache,omitempty"`  // Clear build cache before building

	// Webhook configuration for build notifications
	WebhookURL              string   `json:"webhook_url,omitempty" yaml:"webhook_url,omitempty"`              // URL to call on build completion
	WebhookSecret           string   `json:"webhook_secret,omitempty" yaml:"webhook_secret,omitempty"`           // Optional secret for webhook authentication
	WebhookOnSuccess        bool     `json:"webhook_on_success,omitempty" yaml:"webhook_on_success,omitempty"`       // Send webhook on successful builds (default: true)
	WebhookOnFailure        bool     `json:"webhook_on_failure,omitempty" yaml:"webhook_on_failure,omitempty"`       // Send webhook on failed builds (default: true)
	WebhookHeaders          map[string]string `json:"webhook_headers,omitempty" yaml:"webhook_headers,omitempty"`   // Optional custom headers

	// Local build history configuration (CLI only)
	DeployDir               string   `json:"deploy_dir,omitempty" yaml:"deploy_dir,omitempty"`                 // Directory for build history (default: "./deploy")
}

// PhaseResourceLimits defines resource constraints for a single phase
type PhaseResourceLimits struct {
	CPU    string `json:"cpu" yaml:"cpu"`    // e.g., "1000" (MHz)
	Memory string `json:"memory" yaml:"memory"` // e.g., "2048" (MB)
	Disk   string `json:"disk" yaml:"disk"`   // e.g., "10240" (MB)
}

// ResourceLimits defines resource constraints for build jobs per phase
type ResourceLimits struct {
	// Legacy fields for backward compatibility
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`    // e.g., "1000" (MHz) - applies to all phases if per-phase not specified
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"` // e.g., "2048" (MB) - applies to all phases if per-phase not specified
	Disk   string `json:"disk,omitempty" yaml:"disk,omitempty"`   // e.g., "10240" (MB) - applies to all phases if per-phase not specified

	// Per-phase resource limits
	Build   *PhaseResourceLimits `json:"build,omitempty" yaml:"build,omitempty"`
	Test    *PhaseResourceLimits `json:"test,omitempty" yaml:"test,omitempty"`
	Publish *PhaseResourceLimits `json:"publish,omitempty" yaml:"publish,omitempty"`
}

// GetBuildLimits returns the resource limits for the build phase
func (rl *ResourceLimits) GetBuildLimits(defaults PhaseResourceLimits) PhaseResourceLimits {
	if rl == nil {
		return defaults
	}

	// If phase-specific limits are provided, use them
	if rl.Build != nil {
		result := PhaseResourceLimits{}
		if rl.Build.CPU != "" {
			result.CPU = rl.Build.CPU
		} else if rl.CPU != "" {
			result.CPU = rl.CPU // Fall back to legacy global
		} else {
			result.CPU = defaults.CPU
		}
		if rl.Build.Memory != "" {
			result.Memory = rl.Build.Memory
		} else if rl.Memory != "" {
			result.Memory = rl.Memory // Fall back to legacy global
		} else {
			result.Memory = defaults.Memory
		}
		if rl.Build.Disk != "" {
			result.Disk = rl.Build.Disk
		} else if rl.Disk != "" {
			result.Disk = rl.Disk // Fall back to legacy global
		} else {
			result.Disk = defaults.Disk
		}
		return result
	}

	// Fall back to legacy global limits if provided
	result := PhaseResourceLimits{}
	if rl.CPU != "" {
		result.CPU = rl.CPU
	} else {
		result.CPU = defaults.CPU
	}
	if rl.Memory != "" {
		result.Memory = rl.Memory
	} else {
		result.Memory = defaults.Memory
	}
	if rl.Disk != "" {
		result.Disk = rl.Disk
	} else {
		result.Disk = defaults.Disk
	}
	return result
}

// GetTestLimits returns the resource limits for the test phase
func (rl *ResourceLimits) GetTestLimits(defaults PhaseResourceLimits) PhaseResourceLimits {
	if rl == nil {
		return defaults
	}

	// If phase-specific limits are provided, use them
	if rl.Test != nil {
		result := PhaseResourceLimits{}
		if rl.Test.CPU != "" {
			result.CPU = rl.Test.CPU
		} else if rl.CPU != "" {
			result.CPU = rl.CPU // Fall back to legacy global
		} else {
			result.CPU = defaults.CPU
		}
		if rl.Test.Memory != "" {
			result.Memory = rl.Test.Memory
		} else if rl.Memory != "" {
			result.Memory = rl.Memory // Fall back to legacy global
		} else {
			result.Memory = defaults.Memory
		}
		if rl.Test.Disk != "" {
			result.Disk = rl.Test.Disk
		} else if rl.Disk != "" {
			result.Disk = rl.Disk // Fall back to legacy global
		} else {
			result.Disk = defaults.Disk
		}
		return result
	}

	// Fall back to legacy global limits if provided
	result := PhaseResourceLimits{}
	if rl.CPU != "" {
		result.CPU = rl.CPU
	} else {
		result.CPU = defaults.CPU
	}
	if rl.Memory != "" {
		result.Memory = rl.Memory
	} else {
		result.Memory = defaults.Memory
	}
	if rl.Disk != "" {
		result.Disk = rl.Disk
	} else {
		result.Disk = defaults.Disk
	}
	return result
}

// GetPublishLimits returns the resource limits for the publish phase
func (rl *ResourceLimits) GetPublishLimits(defaults PhaseResourceLimits) PhaseResourceLimits {
	if rl == nil {
		return defaults
	}

	// If phase-specific limits are provided, use them
	if rl.Publish != nil {
		result := PhaseResourceLimits{}
		if rl.Publish.CPU != "" {
			result.CPU = rl.Publish.CPU
		} else if rl.CPU != "" {
			result.CPU = rl.CPU // Fall back to legacy global
		} else {
			result.CPU = defaults.CPU
		}
		if rl.Publish.Memory != "" {
			result.Memory = rl.Publish.Memory
		} else if rl.Memory != "" {
			result.Memory = rl.Memory // Fall back to legacy global
		} else {
			result.Memory = defaults.Memory
		}
		if rl.Publish.Disk != "" {
			result.Disk = rl.Publish.Disk
		} else if rl.Disk != "" {
			result.Disk = rl.Disk // Fall back to legacy global
		} else {
			result.Disk = defaults.Disk
		}
		return result
	}

	// Fall back to legacy global limits if provided
	result := PhaseResourceLimits{}
	if rl.CPU != "" {
		result.CPU = rl.CPU
	} else {
		result.CPU = defaults.CPU
	}
	if rl.Memory != "" {
		result.Memory = rl.Memory
	} else {
		result.Memory = defaults.Memory
	}
	if rl.Disk != "" {
		result.Disk = rl.Disk
	} else {
		result.Disk = defaults.Disk
	}
	return result
}

// Job represents a build job with its current state
type Job struct {
	ID         string     `json:"id"`
	Config     JobConfig  `json:"config"`
	Status     JobStatus  `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	
	// Timing fields for webhook payloads
	StartTime    *time.Time `json:"start_time,omitempty"`
	EndTime      *time.Time `json:"end_time,omitempty"`
	CurrentPhase string     `json:"current_phase,omitempty"`
	
	// Nomad job IDs for each phase
	BuildJobID   string   `json:"build_job_id,omitempty"`
	TestJobIDs   []string `json:"test_job_ids,omitempty"`  // Multiple test jobs
	PublishJobID string   `json:"publish_job_id,omitempty"`
	
	// Logs for each phase
	Logs JobLogs `json:"logs"`
	
	// Metrics
	Metrics JobMetrics `json:"metrics"`
	
	// Error information
	Error string `json:"error,omitempty"`
	FailedPhase string `json:"failed_phase,omitempty"`

	// Distributed locking information
	LockKey       string `json:"lock_key,omitempty"`
	LockSessionID string `json:"lock_session_id,omitempty"`

	// External test container endpoint info (for python tests)
	TestServiceHost string `json:"test_service_host,omitempty"` // IP address of running test container
	TestServicePort int    `json:"test_service_port,omitempty"` // Dynamic port assigned by Nomad
	TestJobNomadID  string `json:"test_job_nomad_id,omitempty"` // Nomad job ID for external test container
}

// JobLogs contains logs for each phase of the build
type JobLogs struct {
	Build   []string `json:"build"`
	Test    []string `json:"test"`
	Publish []string `json:"publish"`
}

// JobMetrics contains performance metrics for the job
type JobMetrics struct {
	// Phase timings
	JobStart      *time.Time    `json:"job_start,omitempty"`
	BuildStart    *time.Time    `json:"build_start,omitempty"`
	BuildEnd      *time.Time    `json:"build_end,omitempty"`
	TestStart     *time.Time    `json:"test_start,omitempty"`
	TestEnd       *time.Time    `json:"test_end,omitempty"`
	PublishStart  *time.Time    `json:"publish_start,omitempty"`
	PublishEnd    *time.Time    `json:"publish_end,omitempty"`
	JobEnd        *time.Time    `json:"job_end,omitempty"`
	
	// Phase durations
	BuildDuration   time.Duration `json:"build_duration"`
	TestDuration    time.Duration `json:"test_duration"`
	PublishDuration time.Duration `json:"publish_duration"`
	TotalDuration   time.Duration `json:"total_duration"`
	ResourceUsage   ResourceUsage `json:"resource_usage"`
}

// ResourceUsage tracks actual resource consumption
type ResourceUsage struct {
	MaxCPU    float64 `json:"max_cpu"`    // CPU usage percentage
	MaxMemory float64 `json:"max_memory"` // Memory usage in MB
	DiskUsed  float64 `json:"disk_used"`  // Disk usage in MB
}

// JobHistory represents a historical job record
type JobHistory struct {
	ID        string        `json:"id"`
	Config    JobConfig     `json:"config"`
	Status    JobStatus     `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	Duration  time.Duration `json:"duration"`
	Metrics   JobMetrics    `json:"metrics"`
}

// WebhookPayload represents the payload sent to webhook URLs
type WebhookPayload struct {
	JobID       string                 `json:"job_id"`
	Status      JobStatus              `json:"status"`
	Timestamp   time.Time             `json:"timestamp"`
	Duration    time.Duration         `json:"duration,omitempty"`    // Total build duration
	Owner       string                `json:"owner"`
	RepoURL     string                `json:"repo_url"`
	GitRef      string                `json:"git_ref"`
	ImageName   string                `json:"image_name"`
	ImageTags   []string              `json:"image_tags"`
	Error       string                `json:"error,omitempty"`       // Error message if failed
	Logs        *JobLogs              `json:"logs,omitempty"`        // Optional: include logs
	Metrics     *JobMetrics           `json:"metrics,omitempty"`     // Optional: include metrics
	Phase       string                `json:"phase,omitempty"`       // Current/failed phase
	Signature   string                `json:"signature,omitempty"`   // HMAC signature for webhook authentication
}

// WebhookEvent represents different types of webhook events
type WebhookEvent string

const (
	WebhookEventBuildStarted   WebhookEvent = "build.started"
	WebhookEventBuildCompleted WebhookEvent = "build.completed"
	WebhookEventBuildFailed    WebhookEvent = "build.failed"
	WebhookEventTestStarted    WebhookEvent = "test.started"
	WebhookEventTestCompleted  WebhookEvent = "test.completed"
	WebhookEventTestFailed     WebhookEvent = "test.failed"
	WebhookEventPublishStarted WebhookEvent = "publish.started"
	WebhookEventPublishCompleted WebhookEvent = "publish.completed"
	WebhookEventPublishFailed  WebhookEvent = "publish.failed"
	WebhookEventJobCompleted   WebhookEvent = "job.completed"
	WebhookEventJobFailed      WebhookEvent = "job.failed"
)
