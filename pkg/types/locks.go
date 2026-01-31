package types

import "time"

// LockMetadata contains enhanced metadata stored in lock values
// This enables better debugging and stale lock detection
type LockMetadata struct {
	SessionID   string    `json:"session_id"`
	JobID       string    `json:"job_id"`
	Owner       string    `json:"owner"`
	RegistryURL string    `json:"registry_url"`
	ImageName   string    `json:"image_name"`
	GitRef      string    `json:"git_ref"`
	AcquiredAt  time.Time `json:"acquired_at"`
}

// LockInfo represents information about a single build lock
type LockInfo struct {
	LockKey     string        `json:"lock_key"`
	SessionID   string        `json:"session_id"`
	JobID       string        `json:"job_id"`
	Owner       string        `json:"owner"`
	ImageName   string        `json:"image_name"`
	GitRef      string        `json:"git_ref"`
	RegistryURL string        `json:"registry_url"`
	AcquiredAt  time.Time     `json:"acquired_at"`
	Age         time.Duration `json:"age"`          // How long the lock has been held
	IsStale     bool          `json:"is_stale"`     // True if the job is completed/missing
	StaleReason string        `json:"stale_reason"` // Why the lock is considered stale
}

// ListLocksRequest represents a request to list build locks
type ListLocksRequest struct {
	StaleOnly bool   `json:"stale_only,omitempty"` // Only return stale locks
	Owner     string `json:"owner,omitempty"`      // Filter by owner
}

// ListLocksResponse represents the response containing build locks
type ListLocksResponse struct {
	Locks      []LockInfo `json:"locks"`
	TotalCount int        `json:"total_count"`
	StaleCount int        `json:"stale_count"`
}

// ForceUnlockRequest represents a request to force unlock one or more locks
type ForceUnlockRequest struct {
	LockKey   string `json:"lock_key,omitempty"`   // Specific lock key to unlock
	StaleOnly bool   `json:"stale_only,omitempty"` // Only unlock stale locks
	All       bool   `json:"all,omitempty"`        // Unlock all locks (requires force)
	Force     bool   `json:"force,omitempty"`      // Skip confirmation (required for --all)
}

// ForceUnlockResponse represents the response to a force unlock request
type ForceUnlockResponse struct {
	Success       bool     `json:"success"`
	UnlockedKeys  []string `json:"unlocked_keys"`
	FailedKeys    []string `json:"failed_keys,omitempty"`
	Message       string   `json:"message"`
	UnlockedCount int      `json:"unlocked_count"`
}

// ActiveJobInfo represents information about an active job for a specific owner
type ActiveJobInfo struct {
	JobID     string    `json:"job_id"`
	Status    JobStatus `json:"status"`
	ImageName string    `json:"image_name"`
	GitRef    string    `json:"git_ref"`
	LockKey   string    `json:"lock_key,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// ListActiveJobsRequest represents a request to list active jobs for an owner
type ListActiveJobsRequest struct {
	Owner string `json:"owner"`
}

// ListActiveJobsResponse represents the response containing active jobs for an owner
type ListActiveJobsResponse struct {
	Owner       string          `json:"owner"`
	ActiveJobs  []ActiveJobInfo `json:"active_jobs"`
	ActiveCount int             `json:"active_count"`
	Limit       int             `json:"limit"` // The max concurrent builds per owner
}
