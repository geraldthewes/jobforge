package types

// MCP Request/Response types for the build service

// SubmitJobRequest represents an MCP request to submit a new job
type SubmitJobRequest struct {
	JobConfig JobConfig `json:"job_config"`
}

// SubmitJobResponse represents the response to a job submission
type SubmitJobResponse struct {
	JobID string `json:"job_id"`
	Status JobStatus `json:"status"`
}

// GetStatusRequest represents an MCP request to get job status
type GetStatusRequest struct {
	JobID string `json:"job_id"`
}

// GetStatusResponse represents the response to a status request
type GetStatusResponse struct {
	JobID   string     `json:"job_id"`
	Status  JobStatus  `json:"status"`
	Config  *JobConfig `json:"config,omitempty"` // Include config for debugging
	Metrics JobMetrics `json:"metrics"`
	Error   string     `json:"error,omitempty"`
}

// GetLogsRequest represents an MCP request to get job logs
type GetLogsRequest struct {
	JobID string `json:"job_id"`
	Phase string `json:"phase,omitempty"` // Optional: "build", "test", "publish"
}

// GetLogsResponse represents the response to a logs request
type GetLogsResponse struct {
	JobID string  `json:"job_id"`
	Logs  JobLogs `json:"logs"`
}

// KillJobRequest represents an MCP request to kill a job
type KillJobRequest struct {
	JobID string `json:"job_id"`
}

// KillJobResponse represents the response to a kill job request
type KillJobResponse struct {
	JobID   string `json:"job_id"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// CleanupRequest represents an MCP request to cleanup resources
type CleanupRequest struct {
	JobID string `json:"job_id,omitempty"` // Optional: cleanup specific job
	All   bool   `json:"all,omitempty"`    // Optional: cleanup all zombie jobs
}

// CleanupResponse represents the response to a cleanup request
type CleanupResponse struct {
	Success      bool     `json:"success"`
	CleanedJobs  []string `json:"cleaned_jobs"`
	Message      string   `json:"message"`
}

// GetHistoryRequest represents an MCP request to get job history
type GetHistoryRequest struct {
	Limit  int `json:"limit,omitempty"`  // Optional: number of records to return
	Offset int `json:"offset,omitempty"` // Optional: pagination offset
}

// GetHistoryResponse represents the response to a history request
type GetHistoryResponse struct {
	Jobs  []JobHistory `json:"jobs"`
	Total int          `json:"total"`
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status    string            `json:"status"`
	Services  map[string]string `json:"services"` // service -> status
	Timestamp string            `json:"timestamp"`
}

// StreamLogsMessage represents a WebSocket log streaming message
type StreamLogsMessage struct {
	JobID     string `json:"job_id"`
	Phase     string `json:"phase"`
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

// ErrorResponse represents an error response for MCP requests
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Details string `json:"details,omitempty"`
}

// GetTestEndpointResponse returns the external test container's endpoint information
// Used by CLI to discover where to run python tests against
type GetTestEndpointResponse struct {
	JobID          string    `json:"job_id"`
	ServiceHost    string    `json:"service_host"`              // IP address of test container
	ServicePort    int       `json:"service_port"`              // Dynamic port assigned by Nomad
	HealthEndpoint string    `json:"health_endpoint"`           // Endpoint to poll for readiness
	Status         JobStatus `json:"status"`                    // Current job status
}

// ReportTestResultRequest is sent by CLI after running external python tests
type ReportTestResultRequest struct {
	JobID    string `json:"job_id"`
	Success  bool   `json:"success"`            // Whether tests passed
	ExitCode int    `json:"exit_code"`          // Exit code from python-executor
	Stdout   string `json:"stdout,omitempty"`   // Captured stdout
	Stderr   string `json:"stderr,omitempty"`   // Captured stderr
	Duration int64  `json:"duration_ms"`        // Test execution duration in milliseconds
}

// ReportTestResultResponse acknowledges the test result from CLI
type ReportTestResultResponse struct {
	JobID   string    `json:"job_id"`
	Status  JobStatus `json:"status"`   // New job status after processing result
	Message string    `json:"message"`
}