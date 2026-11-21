package nomad

import (
	"testing"
	"time"

	"nomad-mcp-builder/pkg/types"
)

func TestJobDeadlineAgentTimeout(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	limit := types.FlexibleDuration(30 * time.Minute)
	job := &types.Job{
		Status:    types.StatusAgentRunning,
		StartedAt: &start,
		Config: types.JobConfig{
			Kind:  types.JobKindAgent,
			Agent: &types.AgentConfig{Timeout: &limit},
		},
	}
	phase, overdue := JobExceededTimeout(job, time.Now(), time.Hour, 15*time.Minute)
	if !overdue || phase != "agent" {
		t.Fatalf("expected agent timeout, phase=%s overdue=%v", phase, overdue)
	}

	recent := time.Now().Add(-5 * time.Minute)
	job.StartedAt = &recent
	if _, overdue := JobExceededTimeout(job, time.Now(), time.Hour, 15*time.Minute); overdue {
		t.Fatal("agent job inside its timeout was marked overdue")
	}
}

func TestJobDeadlineAgentDefault(t *testing.T) {
	start := time.Now().Add(-61 * time.Minute)
	job := &types.Job{
		Status:    types.StatusReviewing,
		StartedAt: &start,
		Config:    types.JobConfig{Kind: types.JobKindAgent, Agent: &types.AgentConfig{}},
	}
	phase, overdue := JobExceededTimeout(job, time.Now(), time.Hour, 15*time.Minute)
	if !overdue || phase != "agent" {
		t.Fatalf("default 60m agent timeout did not fire: %s %v", phase, overdue)
	}
}

func TestJobDeadlineBuildAndTest(t *testing.T) {
	buildStart := time.Now().Add(-45 * time.Minute)
	buildLimit := 30 * time.Minute
	buildJob := &types.Job{
		Status: types.StatusBuilding,
		Metrics: types.JobMetrics{
			BuildStart: &buildStart,
		},
		Config: types.JobConfig{BuildTimeout: &buildLimit},
	}
	phase, overdue := JobExceededTimeout(buildJob, time.Now(), time.Hour, 15*time.Minute)
	if !overdue || phase != "build" {
		t.Fatalf("build timeout: phase=%s overdue=%v", phase, overdue)
	}

	testStart := time.Now().Add(-2 * time.Minute)
	testLimit := 15 * time.Minute
	testJob := &types.Job{
		Status: types.StatusTesting,
		Config: types.JobConfig{Test: &types.TestConfig{Timeout: &testLimit}},
		Metrics: types.JobMetrics{
			TestStart: &testStart,
		},
	}
	if _, overdue := JobExceededTimeout(testJob, time.Now(), time.Hour, 10*time.Minute); overdue {
		t.Fatal("test job inside test_timeout was marked overdue")
	}

	testStart = time.Now().Add(-20 * time.Minute)
	testJob.Metrics.TestStart = &testStart
	phase, overdue = JobExceededTimeout(testJob, time.Now(), time.Hour, 10*time.Minute)
	if !overdue || phase != "test" {
		t.Fatalf("test timeout: phase=%s overdue=%v", phase, overdue)
	}
}

func TestSucceededJobHasNoDeadline(t *testing.T) {
	job := &types.Job{Status: types.StatusSucceeded, CreatedAt: time.Now().Add(-24 * time.Hour)}
	if _, _, ok := JobDeadline(job, time.Hour, time.Hour); ok {
		t.Fatal("terminal jobs should not be timed out again")
	}
}
