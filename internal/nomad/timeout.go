package nomad

import (
	"time"

	"nomad-mcp-builder/pkg/types"
)

const (
	defaultAgentTimeout = 60 * time.Minute
	defaultStepTimeout  = 20 * time.Minute
)

// JobDeadline is the wall-clock time after which the monitor should kill a job.
// defaultBuild and defaultTest are the service-level build and test timeouts.
// The bool is false when the job is in a phase that has no timeout.
func JobDeadline(job *types.Job, defaultBuild, defaultTest time.Duration) (time.Time, string, bool) {
	if job == nil {
		return time.Time{}, "", false
	}

	start := job.CreatedAt
	if job.StartedAt != nil && !job.StartedAt.IsZero() {
		start = *job.StartedAt
	}

	if job.Config.IsAgent() {
		limit := defaultAgentTimeout
		if job.Config.Agent != nil && job.Config.Agent.Timeout.Std() > 0 {
			limit = job.Config.Agent.Timeout.Std()
		}
		return start.Add(limit), "agent", true
	}

	switch job.Status {
	case types.StatusPending, types.StatusBuilding:
		return phaseDeadline(start, job.Metrics.BuildStart, configuredOrDefault(job.Config.BuildTimeout, defaultBuild), "build")
	case types.StatusTesting, types.StatusTestingExternal:
		var configured *time.Duration
		if job.Config.Test != nil {
			configured = job.Config.Test.Timeout
		}
		return phaseDeadline(start, job.Metrics.TestStart, configuredOrDefault(configured, defaultTest), "test")
	case types.StatusPublishing:
		return phaseDeadline(start, job.Metrics.PublishStart, configuredOrDefault(job.Config.BuildTimeout, defaultBuild), "publish")
	default:
		return time.Time{}, "", false
	}
}

func phaseDeadline(fallback time.Time, phaseStart *time.Time, limit time.Duration, phase string) (time.Time, string, bool) {
	if limit <= 0 {
		return time.Time{}, "", false
	}
	start := fallback
	if phaseStart != nil && !phaseStart.IsZero() {
		start = *phaseStart
	}
	return start.Add(limit), phase, true
}

func configuredOrDefault(configured *time.Duration, fallback time.Duration) time.Duration {
	if configured != nil && *configured > 0 {
		return *configured
	}
	return fallback
}

// JobExceededTimeout reports whether now is past the job deadline.
func JobExceededTimeout(job *types.Job, now time.Time, defaultBuild, defaultTest time.Duration) (string, bool) {
	deadline, phase, ok := JobDeadline(job, defaultBuild, defaultTest)
	if !ok || !now.After(deadline) {
		return "", false
	}
	return phase, true
}
