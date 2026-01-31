package unit

import (
	"testing"
	"time"

	"nomad-mcp-builder/pkg/types"
)

func TestLockMetadata(t *testing.T) {
	t.Run("LockMetadata basic structure", func(t *testing.T) {
		now := time.Now()
		metadata := types.LockMetadata{
			SessionID:   "session-123",
			JobID:       "job-456",
			Owner:       "gerald",
			RegistryURL: "registry.cluster:5000",
			ImageName:   "myapp",
			GitRef:      "main",
			AcquiredAt:  now,
		}

		if metadata.SessionID != "session-123" {
			t.Errorf("Expected session ID 'session-123', got %s", metadata.SessionID)
		}

		if metadata.JobID != "job-456" {
			t.Errorf("Expected job ID 'job-456', got %s", metadata.JobID)
		}

		if metadata.Owner != "gerald" {
			t.Errorf("Expected owner 'gerald', got %s", metadata.Owner)
		}

		if metadata.ImageName != "myapp" {
			t.Errorf("Expected image name 'myapp', got %s", metadata.ImageName)
		}

		if !metadata.AcquiredAt.Equal(now) {
			t.Errorf("Expected acquired at %v, got %v", now, metadata.AcquiredAt)
		}
	})
}

func TestLockInfo(t *testing.T) {
	t.Run("LockInfo with stale lock", func(t *testing.T) {
		acquiredAt := time.Now().Add(-2 * time.Hour)
		lockInfo := types.LockInfo{
			LockKey:     "image-registry.cluster-5000-myapp-main",
			SessionID:   "session-123",
			JobID:       "job-456",
			Owner:       "gerald",
			ImageName:   "myapp",
			GitRef:      "main",
			RegistryURL: "registry.cluster:5000",
			AcquiredAt:  acquiredAt,
			Age:         2 * time.Hour,
			IsStale:     true,
			StaleReason: "job completed successfully",
		}

		if !lockInfo.IsStale {
			t.Error("Expected lock to be stale")
		}

		if lockInfo.StaleReason != "job completed successfully" {
			t.Errorf("Expected stale reason 'job completed successfully', got %s", lockInfo.StaleReason)
		}

		if lockInfo.Age != 2*time.Hour {
			t.Errorf("Expected age 2h, got %v", lockInfo.Age)
		}
	})

	t.Run("LockInfo with active lock", func(t *testing.T) {
		acquiredAt := time.Now().Add(-5 * time.Minute)
		lockInfo := types.LockInfo{
			LockKey:     "image-registry.cluster-5000-myapp-feature",
			SessionID:   "session-789",
			JobID:       "job-789",
			Owner:       "team-a",
			ImageName:   "myapp",
			GitRef:      "feature",
			AcquiredAt:  acquiredAt,
			Age:         5 * time.Minute,
			IsStale:     false,
			StaleReason: "",
		}

		if lockInfo.IsStale {
			t.Error("Expected lock to not be stale")
		}

		if lockInfo.StaleReason != "" {
			t.Errorf("Expected empty stale reason, got %s", lockInfo.StaleReason)
		}
	})
}

func TestListLocksRequest(t *testing.T) {
	t.Run("ListLocksRequest with filters", func(t *testing.T) {
		req := types.ListLocksRequest{
			StaleOnly: true,
			Owner:     "gerald",
		}

		if !req.StaleOnly {
			t.Error("Expected StaleOnly to be true")
		}

		if req.Owner != "gerald" {
			t.Errorf("Expected owner 'gerald', got %s", req.Owner)
		}
	})

	t.Run("ListLocksRequest without filters", func(t *testing.T) {
		req := types.ListLocksRequest{}

		if req.StaleOnly {
			t.Error("Expected StaleOnly to be false by default")
		}

		if req.Owner != "" {
			t.Errorf("Expected empty owner, got %s", req.Owner)
		}
	})
}

func TestListLocksResponse(t *testing.T) {
	t.Run("ListLocksResponse with locks", func(t *testing.T) {
		locks := []types.LockInfo{
			{LockKey: "lock-1", JobID: "job-1", IsStale: false},
			{LockKey: "lock-2", JobID: "job-2", IsStale: true, StaleReason: "job failed"},
			{LockKey: "lock-3", JobID: "job-3", IsStale: true, StaleReason: "job completed"},
		}

		response := types.ListLocksResponse{
			Locks:      locks,
			TotalCount: 3,
			StaleCount: 2,
		}

		if len(response.Locks) != 3 {
			t.Errorf("Expected 3 locks, got %d", len(response.Locks))
		}

		if response.TotalCount != 3 {
			t.Errorf("Expected total count 3, got %d", response.TotalCount)
		}

		if response.StaleCount != 2 {
			t.Errorf("Expected stale count 2, got %d", response.StaleCount)
		}
	})

	t.Run("ListLocksResponse empty", func(t *testing.T) {
		response := types.ListLocksResponse{
			Locks:      []types.LockInfo{},
			TotalCount: 0,
			StaleCount: 0,
		}

		if len(response.Locks) != 0 {
			t.Errorf("Expected 0 locks, got %d", len(response.Locks))
		}
	})
}

func TestForceUnlockRequest(t *testing.T) {
	t.Run("ForceUnlockRequest for specific lock", func(t *testing.T) {
		req := types.ForceUnlockRequest{
			LockKey: "image-registry.cluster-5000-myapp-main",
			Force:   true,
		}

		if req.LockKey != "image-registry.cluster-5000-myapp-main" {
			t.Errorf("Expected lock key, got %s", req.LockKey)
		}

		if !req.Force {
			t.Error("Expected Force to be true")
		}
	})

	t.Run("ForceUnlockRequest for stale only", func(t *testing.T) {
		req := types.ForceUnlockRequest{
			StaleOnly: true,
		}

		if !req.StaleOnly {
			t.Error("Expected StaleOnly to be true")
		}

		if req.LockKey != "" {
			t.Errorf("Expected empty lock key, got %s", req.LockKey)
		}
	})

	t.Run("ForceUnlockRequest for all locks", func(t *testing.T) {
		req := types.ForceUnlockRequest{
			All:   true,
			Force: true,
		}

		if !req.All {
			t.Error("Expected All to be true")
		}

		if !req.Force {
			t.Error("Expected Force to be true (required for All)")
		}
	})
}

func TestForceUnlockResponse(t *testing.T) {
	t.Run("ForceUnlockResponse success", func(t *testing.T) {
		response := types.ForceUnlockResponse{
			Success:       true,
			UnlockedKeys:  []string{"lock-1", "lock-2"},
			FailedKeys:    nil,
			Message:       "Unlocked 2 lock(s)",
			UnlockedCount: 2,
		}

		if !response.Success {
			t.Error("Expected Success to be true")
		}

		if len(response.UnlockedKeys) != 2 {
			t.Errorf("Expected 2 unlocked keys, got %d", len(response.UnlockedKeys))
		}

		if response.UnlockedCount != 2 {
			t.Errorf("Expected unlocked count 2, got %d", response.UnlockedCount)
		}
	})

	t.Run("ForceUnlockResponse partial failure", func(t *testing.T) {
		response := types.ForceUnlockResponse{
			Success:       false,
			UnlockedKeys:  []string{"lock-1"},
			FailedKeys:    []string{"lock-2"},
			Message:       "Unlocked 1 lock(s), failed to unlock 1 lock(s)",
			UnlockedCount: 1,
		}

		if response.Success {
			t.Error("Expected Success to be false")
		}

		if len(response.FailedKeys) != 1 {
			t.Errorf("Expected 1 failed key, got %d", len(response.FailedKeys))
		}
	})
}

func TestActiveJobInfo(t *testing.T) {
	t.Run("ActiveJobInfo basic structure", func(t *testing.T) {
		now := time.Now()
		job := types.ActiveJobInfo{
			JobID:     "job-123",
			Status:    types.StatusBuilding,
			ImageName: "myapp",
			GitRef:    "main",
			LockKey:   "image-registry.cluster-5000-myapp-main",
			StartedAt: now,
		}

		if job.JobID != "job-123" {
			t.Errorf("Expected job ID 'job-123', got %s", job.JobID)
		}

		if job.Status != types.StatusBuilding {
			t.Errorf("Expected status BUILDING, got %s", job.Status)
		}

		if job.ImageName != "myapp" {
			t.Errorf("Expected image name 'myapp', got %s", job.ImageName)
		}
	})
}

func TestListActiveJobsRequest(t *testing.T) {
	t.Run("ListActiveJobsRequest with owner", func(t *testing.T) {
		req := types.ListActiveJobsRequest{
			Owner: "gerald",
		}

		if req.Owner != "gerald" {
			t.Errorf("Expected owner 'gerald', got %s", req.Owner)
		}
	})
}

func TestListActiveJobsResponse(t *testing.T) {
	t.Run("ListActiveJobsResponse with active jobs", func(t *testing.T) {
		now := time.Now()
		jobs := []types.ActiveJobInfo{
			{JobID: "job-1", Status: types.StatusBuilding, StartedAt: now},
			{JobID: "job-2", Status: types.StatusTesting, StartedAt: now},
		}

		response := types.ListActiveJobsResponse{
			Owner:       "gerald",
			ActiveJobs:  jobs,
			ActiveCount: 2,
			Limit:       3,
		}

		if response.Owner != "gerald" {
			t.Errorf("Expected owner 'gerald', got %s", response.Owner)
		}

		if len(response.ActiveJobs) != 2 {
			t.Errorf("Expected 2 active jobs, got %d", len(response.ActiveJobs))
		}

		if response.ActiveCount != 2 {
			t.Errorf("Expected active count 2, got %d", response.ActiveCount)
		}

		if response.Limit != 3 {
			t.Errorf("Expected limit 3, got %d", response.Limit)
		}
	})

	t.Run("ListActiveJobsResponse at limit", func(t *testing.T) {
		now := time.Now()
		jobs := []types.ActiveJobInfo{
			{JobID: "job-1", Status: types.StatusBuilding, StartedAt: now},
			{JobID: "job-2", Status: types.StatusTesting, StartedAt: now},
			{JobID: "job-3", Status: types.StatusPublishing, StartedAt: now},
		}

		response := types.ListActiveJobsResponse{
			Owner:       "gerald",
			ActiveJobs:  jobs,
			ActiveCount: 3,
			Limit:       3,
		}

		// Check if at limit
		if response.ActiveCount < response.Limit {
			t.Error("Expected to be at limit")
		}
	})

	t.Run("ListActiveJobsResponse empty", func(t *testing.T) {
		response := types.ListActiveJobsResponse{
			Owner:       "gerald",
			ActiveJobs:  []types.ActiveJobInfo{},
			ActiveCount: 0,
			Limit:       3,
		}

		if len(response.ActiveJobs) != 0 {
			t.Errorf("Expected 0 active jobs, got %d", len(response.ActiveJobs))
		}

		if response.ActiveCount != 0 {
			t.Errorf("Expected active count 0, got %d", response.ActiveCount)
		}
	})
}
