package storage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/sirupsen/logrus"

	"nomad-mcp-builder/pkg/types"
)

// ConsulStorage implements job storage using Consul KV
type ConsulStorage struct {
	client    *consulapi.Client
	keyPrefix string
	logger    *logrus.Logger
}

// NewConsulStorage creates a new Consul-based storage backend
func NewConsulStorage(address, token, datacenter, keyPrefix string) (*ConsulStorage, error) {
	config := consulapi.DefaultConfig()
	config.Address = address
	config.Token = token
	config.Datacenter = datacenter
	
	client, err := consulapi.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Consul client: %w", err)
	}
	
	return &ConsulStorage{
		client:    client,
		keyPrefix: keyPrefix,
		logger:    logrus.New(),
	}, nil
}

// StoreJob stores a job in Consul KV
func (cs *ConsulStorage) StoreJob(job *types.Job) error {
	key := fmt.Sprintf("%s/jobs/%s", cs.keyPrefix, job.ID)
	
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}
	
	pair := &consulapi.KVPair{
		Key:   key,
		Value: data,
	}
	
	_, err = cs.client.KV().Put(pair, nil)
	if err != nil {
		return fmt.Errorf("failed to store job in Consul: %w", err)
	}
	
	cs.logger.WithField("job_id", job.ID).Debug("Job stored in Consul")
	return nil
}

// GetJob retrieves a job from Consul KV
func (cs *ConsulStorage) GetJob(jobID string) (*types.Job, error) {
	key := fmt.Sprintf("%s/jobs/%s", cs.keyPrefix, jobID)
	
	pair, _, err := cs.client.KV().Get(key, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get job from Consul: %w", err)
	}
	
	if pair == nil {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}
	
	var job types.Job
	if err := json.Unmarshal(pair.Value, &job); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job: %w", err)
	}
	
	return &job, nil
}

// UpdateJob updates an existing job in Consul KV
func (cs *ConsulStorage) UpdateJob(job *types.Job) error {
	job.UpdatedAt = time.Now()
	return cs.StoreJob(job)
}

// DeleteJob removes a job from Consul KV
func (cs *ConsulStorage) DeleteJob(jobID string) error {
	key := fmt.Sprintf("%s/jobs/%s", cs.keyPrefix, jobID)
	
	_, err := cs.client.KV().Delete(key, nil)
	if err != nil {
		return fmt.Errorf("failed to delete job from Consul: %w", err)
	}
	
	cs.logger.WithField("job_id", jobID).Debug("Job deleted from Consul")
	return nil
}

// ListJobs returns a list of all job IDs
func (cs *ConsulStorage) ListJobs() ([]string, error) {
	prefix := fmt.Sprintf("%s/jobs/", cs.keyPrefix)
	
	pairs, _, err := cs.client.KV().List(prefix, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs from Consul: %w", err)
	}
	
	var jobIDs []string
	for _, pair := range pairs {
		// Extract job ID from key (remove prefix)
		jobID := pair.Key[len(prefix):]
		jobIDs = append(jobIDs, jobID)
	}
	
	return jobIDs, nil
}

// StoreJobHistory stores job history for debugging purposes
func (cs *ConsulStorage) StoreJobHistory(history *types.JobHistory) error {
	key := fmt.Sprintf("%s/history/%s", cs.keyPrefix, history.ID)
	
	data, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("failed to marshal job history: %w", err)
	}
	
	pair := &consulapi.KVPair{
		Key:   key,
		Value: data,
	}
	
	_, err = cs.client.KV().Put(pair, nil)
	if err != nil {
		return fmt.Errorf("failed to store job history in Consul: %w", err)
	}
	
	cs.logger.WithField("job_id", history.ID).Debug("Job history stored in Consul")
	return nil
}

// GetJobHistory retrieves job history with pagination
func (cs *ConsulStorage) GetJobHistory(limit, offset int) ([]types.JobHistory, int, error) {
	prefix := fmt.Sprintf("%s/history/", cs.keyPrefix)
	
	pairs, _, err := cs.client.KV().List(prefix, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get job history from Consul: %w", err)
	}
	
	// Parse and sort by creation time (newest first)
	var histories []types.JobHistory
	for _, pair := range pairs {
		var history types.JobHistory
		if err := json.Unmarshal(pair.Value, &history); err != nil {
			cs.logger.WithError(err).Warn("Failed to unmarshal job history")
			continue
		}
		histories = append(histories, history)
	}
	
	// Sort by creation time (newest first)
	sort.Slice(histories, func(i, j int) bool {
		return histories[i].CreatedAt.After(histories[j].CreatedAt)
	})
	
	total := len(histories)
	
	// Apply pagination
	if offset >= total {
		return []types.JobHistory{}, total, nil
	}
	
	end := offset + limit
	if end > total {
		end = total
	}
	
	return histories[offset:end], total, nil
}

// CleanupOldHistory removes job history older than the specified duration
func (cs *ConsulStorage) CleanupOldHistory(maxAge time.Duration) error {
	prefix := fmt.Sprintf("%s/history/", cs.keyPrefix)
	
	pairs, _, err := cs.client.KV().List(prefix, nil)
	if err != nil {
		return fmt.Errorf("failed to list job history from Consul: %w", err)
	}
	
	cutoff := time.Now().Add(-maxAge)
	var deletedCount int
	
	for _, pair := range pairs {
		var history types.JobHistory
		if err := json.Unmarshal(pair.Value, &history); err != nil {
			cs.logger.WithError(err).Warn("Failed to unmarshal job history during cleanup")
			continue
		}
		
		if history.CreatedAt.Before(cutoff) {
			if _, err := cs.client.KV().Delete(pair.Key, nil); err != nil {
				cs.logger.WithError(err).WithField("key", pair.Key).Warn("Failed to delete old job history")
				continue
			}
			deletedCount++
		}
	}
	
	cs.logger.WithFields(logrus.Fields{
		"deleted_count": deletedCount,
		"max_age":       maxAge,
	}).Info("Cleaned up old job history")
	
	return nil
}

// GetConfiguration retrieves a configuration value from Consul
func (cs *ConsulStorage) GetConfiguration(key string) (string, error) {
	fullKey := fmt.Sprintf("%s/config/%s", cs.keyPrefix, key)
	
	pair, _, err := cs.client.KV().Get(fullKey, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get configuration from Consul: %w", err)
	}
	
	if pair == nil {
		return "", fmt.Errorf("configuration key not found: %s", key)
	}
	
	return string(pair.Value), nil
}

// SetConfiguration stores a configuration value in Consul
func (cs *ConsulStorage) SetConfiguration(key, value string) error {
	fullKey := fmt.Sprintf("%s/config/%s", cs.keyPrefix, key)
	
	pair := &consulapi.KVPair{
		Key:   fullKey,
		Value: []byte(value),
	}
	
	_, err := cs.client.KV().Put(pair, nil)
	if err != nil {
		return fmt.Errorf("failed to set configuration in Consul: %w", err)
	}
	
	cs.logger.WithFields(logrus.Fields{
		"key":   key,
		"value": value,
	}).Debug("Configuration updated in Consul")
	
	return nil
}

// Health checks the health of the Consul connection
func (cs *ConsulStorage) Health() error {
	_, err := cs.client.Status().Leader()
	if err != nil {
		return fmt.Errorf("consul health check failed: %w", err)
	}
	return nil
}

// AcquireLock acquires a distributed lock for the given key
// Returns a session ID that must be used to release the lock
func (cs *ConsulStorage) AcquireLock(lockKey string, timeout time.Duration) (string, error) {
	cs.logger.WithField("lock_key", lockKey).Debug("Attempting to acquire lock")
	
	// Create a session for the lock
	session := &consulapi.SessionEntry{
		TTL:      timeout.String(),
		Behavior: consulapi.SessionBehaviorRelease,
		Name:     fmt.Sprintf("build-lock-%s", lockKey),
	}
	
	sessionID, _, err := cs.client.Session().Create(session, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create session for lock: %w", err)
	}
	
	cs.logger.WithFields(logrus.Fields{
		"lock_key":   lockKey,
		"session_id": sessionID,
	}).Debug("Session created for lock")
	
	// Try to acquire the lock
	fullKey := fmt.Sprintf("%s/locks/%s", cs.keyPrefix, lockKey)
	pair := &consulapi.KVPair{
		Key:     fullKey,
		Value:   []byte(sessionID),
		Session: sessionID,
	}
	
	// Use the Acquire method which is atomic
	acquired, _, err := cs.client.KV().Acquire(pair, nil)
	if err != nil {
		// Clean up session if acquire failed
		cs.client.Session().Destroy(sessionID, nil)
		return "", fmt.Errorf("failed to acquire lock: %w", err)
	}
	
	if !acquired {
		// Lock is held by someone else, clean up session
		cs.client.Session().Destroy(sessionID, nil)
		return "", fmt.Errorf("lock is already held by another process")
	}
	
	cs.logger.WithFields(logrus.Fields{
		"lock_key":   lockKey,
		"session_id": sessionID,
	}).Info("Lock acquired successfully")
	
	return sessionID, nil
}

// ReleaseLock releases a distributed lock using the session ID
func (cs *ConsulStorage) ReleaseLock(lockKey, sessionID string) error {
	cs.logger.WithFields(logrus.Fields{
		"lock_key":   lockKey,
		"session_id": sessionID,
	}).Debug("Releasing lock")
	
	// First, release the key from the session
	fullKey := fmt.Sprintf("%s/locks/%s", cs.keyPrefix, lockKey)
	pair := &consulapi.KVPair{
		Key:     fullKey,
		Session: sessionID,
	}
	
	// Use the Release method which is atomic
	released, _, err := cs.client.KV().Release(pair, nil)
	if err != nil {
		cs.logger.WithError(err).Warn("Failed to release lock key")
	} else if !released {
		cs.logger.Warn("Lock key was not held by this session")
	}
	
	// Always destroy the session to clean up
	_, err = cs.client.Session().Destroy(sessionID, nil)
	if err != nil {
		cs.logger.WithError(err).Warn("Failed to destroy session")
		return fmt.Errorf("failed to destroy session: %w", err)
	}
	
	cs.logger.WithFields(logrus.Fields{
		"lock_key":   lockKey,
		"session_id": sessionID,
	}).Info("Lock released successfully")
	
	return nil
}

// GenerateImageLockKey generates a consistent lock key for image builds
func (cs *ConsulStorage) GenerateImageLockKey(registryURL, imageName, branch string) string {
	// Sanitize components for use in lock key
	sanitizedRegistry := strings.ToLower(strings.ReplaceAll(registryURL, "/", "-"))
	sanitizedImage := strings.ToLower(strings.ReplaceAll(imageName, "/", "-"))
	sanitizedBranch := strings.ToLower(strings.ReplaceAll(branch, "/", "-"))

	// Create a consistent lock key that includes registry, image, and branch
	// This allows different branches to build concurrently but prevents
	// concurrent builds of the same image on the same branch
	return fmt.Sprintf("image-%s-%s-%s", sanitizedRegistry, sanitizedImage, sanitizedBranch)
}

// AcquireLockWithMetadata acquires a distributed lock with enhanced metadata
// Returns a session ID that must be used to release the lock
func (cs *ConsulStorage) AcquireLockWithMetadata(lockKey string, timeout time.Duration, metadata *types.LockMetadata) (string, error) {
	cs.logger.WithField("lock_key", lockKey).Debug("Attempting to acquire lock with metadata")

	// Create a session for the lock
	session := &consulapi.SessionEntry{
		TTL:      timeout.String(),
		Behavior: consulapi.SessionBehaviorRelease,
		Name:     fmt.Sprintf("build-lock-%s", lockKey),
	}

	sessionID, _, err := cs.client.Session().Create(session, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create session for lock: %w", err)
	}

	cs.logger.WithFields(logrus.Fields{
		"lock_key":   lockKey,
		"session_id": sessionID,
	}).Debug("Session created for lock")

	// Prepare lock value with metadata
	metadata.SessionID = sessionID
	metadata.AcquiredAt = time.Now()

	lockValue, err := json.Marshal(metadata)
	if err != nil {
		cs.client.Session().Destroy(sessionID, nil)
		return "", fmt.Errorf("failed to marshal lock metadata: %w", err)
	}

	// Try to acquire the lock
	fullKey := fmt.Sprintf("%s/locks/%s", cs.keyPrefix, lockKey)
	pair := &consulapi.KVPair{
		Key:     fullKey,
		Value:   lockValue,
		Session: sessionID,
	}

	// Use the Acquire method which is atomic
	acquired, _, err := cs.client.KV().Acquire(pair, nil)
	if err != nil {
		// Clean up session if acquire failed
		cs.client.Session().Destroy(sessionID, nil)
		return "", fmt.Errorf("failed to acquire lock: %w", err)
	}

	if !acquired {
		// Lock is held by someone else, clean up session
		cs.client.Session().Destroy(sessionID, nil)
		return "", fmt.Errorf("lock is already held by another process")
	}

	cs.logger.WithFields(logrus.Fields{
		"lock_key":   lockKey,
		"session_id": sessionID,
		"owner":      metadata.Owner,
		"job_id":     metadata.JobID,
	}).Info("Lock acquired successfully with metadata")

	return sessionID, nil
}

// ListLocks returns all current build locks with their metadata
func (cs *ConsulStorage) ListLocks() ([]types.LockInfo, error) {
	prefix := fmt.Sprintf("%s/locks/", cs.keyPrefix)

	pairs, _, err := cs.client.KV().List(prefix, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list locks from Consul: %w", err)
	}

	var locks []types.LockInfo
	now := time.Now()

	for _, pair := range pairs {
		lockKey := pair.Key[len(prefix):]

		lockInfo := types.LockInfo{
			LockKey: lockKey,
		}

		// Try to parse enhanced metadata
		var metadata types.LockMetadata
		if err := json.Unmarshal(pair.Value, &metadata); err == nil {
			lockInfo.SessionID = metadata.SessionID
			lockInfo.JobID = metadata.JobID
			lockInfo.Owner = metadata.Owner
			lockInfo.ImageName = metadata.ImageName
			lockInfo.GitRef = metadata.GitRef
			lockInfo.RegistryURL = metadata.RegistryURL
			lockInfo.AcquiredAt = metadata.AcquiredAt
			lockInfo.Age = now.Sub(metadata.AcquiredAt)
		} else {
			// Legacy lock format - value is just session ID
			lockInfo.SessionID = string(pair.Value)
			// Age unknown for legacy locks
		}

		// Check if the lock is stale and get the current phase
		if lockInfo.JobID != "" {
			isStale, reason, phase := cs.getLockStaleInfoAndPhase(lockInfo.JobID)
			lockInfo.IsStale = isStale
			lockInfo.StaleReason = reason
			lockInfo.Phase = phase
		} else if pair.Session == "" {
			// Lock has no session attached (orphaned)
			lockInfo.IsStale = true
			lockInfo.StaleReason = "no session attached"
		}

		locks = append(locks, lockInfo)
	}

	return locks, nil
}

// getPhaseFromStatus converts a job status to a human-readable phase string
func getPhaseFromStatus(status types.JobStatus) string {
	switch status {
	case types.StatusPending:
		return "pending"
	case types.StatusBuilding:
		return "build"
	case types.StatusTesting, types.StatusTestingExternal:
		return "test"
	case types.StatusPublishing:
		return "publish"
	case types.StatusSucceeded:
		return "completed"
	case types.StatusFailed:
		return "failed"
	default:
		return string(status)
	}
}

// getLockStaleInfoAndPhase checks if a lock is stale and returns the current phase
func (cs *ConsulStorage) getLockStaleInfoAndPhase(jobID string) (isStale bool, staleReason string, phase string) {
	job, err := cs.GetJob(jobID)
	if err != nil {
		// Job not found in storage - likely completed and cleaned up
		return true, "job not found in storage", ""
	}

	// Get phase from job status
	phase = getPhaseFromStatus(job.Status)

	// Check if job is in a terminal state
	switch job.Status {
	case types.StatusSucceeded:
		return true, "job completed successfully", phase
	case types.StatusFailed:
		return true, "job failed", phase
	}

	return false, "", phase
}

// GetActiveJobsForOwner returns the count and list of active jobs for a specific owner
func (cs *ConsulStorage) GetActiveJobsForOwner(owner string) ([]types.ActiveJobInfo, error) {
	jobIDs, err := cs.ListJobs()
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}

	var activeJobs []types.ActiveJobInfo

	for _, jobID := range jobIDs {
		job, err := cs.GetJob(jobID)
		if err != nil {
			cs.logger.WithError(err).WithField("job_id", jobID).Warn("Failed to get job during active jobs query")
			continue
		}

		// Check if job belongs to the owner and is active
		if job.Config.Owner != owner {
			continue
		}

		// Check if job is in an active (non-terminal) state
		switch job.Status {
		case types.StatusPending, types.StatusBuilding, types.StatusTesting, types.StatusTestingExternal, types.StatusPublishing:
			activeJobInfo := types.ActiveJobInfo{
				JobID:     job.ID,
				Status:    job.Status,
				ImageName: job.Config.ImageName,
				GitRef:    job.Config.GitRef,
				LockKey:   job.LockKey,
			}
			if job.StartedAt != nil {
				activeJobInfo.StartedAt = *job.StartedAt
			} else {
				activeJobInfo.StartedAt = job.CreatedAt
			}
			activeJobs = append(activeJobs, activeJobInfo)
		}
	}

	return activeJobs, nil
}

// ForceReleaseLock forcefully releases a lock by destroying its session and deleting the key
func (cs *ConsulStorage) ForceReleaseLock(lockKey string) error {
	fullKey := fmt.Sprintf("%s/locks/%s", cs.keyPrefix, lockKey)

	// Get the lock to find its session
	pair, _, err := cs.client.KV().Get(fullKey, nil)
	if err != nil {
		return fmt.Errorf("failed to get lock: %w", err)
	}

	if pair == nil {
		return fmt.Errorf("lock not found: %s", lockKey)
	}

	// Try to destroy the session if present
	if pair.Session != "" {
		_, err = cs.client.Session().Destroy(pair.Session, nil)
		if err != nil {
			cs.logger.WithError(err).WithField("session_id", pair.Session).Warn("Failed to destroy session during force unlock")
		}
	}

	// Delete the lock key
	_, err = cs.client.KV().Delete(fullKey, nil)
	if err != nil {
		return fmt.Errorf("failed to delete lock key: %w", err)
	}

	cs.logger.WithField("lock_key", lockKey).Info("Lock forcefully released")
	return nil
}

// CleanupStaleLocks removes all locks where the associated job is completed or missing
func (cs *ConsulStorage) CleanupStaleLocks() ([]string, error) {
	locks, err := cs.ListLocks()
	if err != nil {
		return nil, fmt.Errorf("failed to list locks: %w", err)
	}

	var cleanedKeys []string

	for _, lock := range locks {
		if lock.IsStale {
			if err := cs.ForceReleaseLock(lock.LockKey); err != nil {
				cs.logger.WithError(err).WithField("lock_key", lock.LockKey).Warn("Failed to cleanup stale lock")
				continue
			}
			cleanedKeys = append(cleanedKeys, lock.LockKey)
		}
	}

	cs.logger.WithField("cleaned_count", len(cleanedKeys)).Info("Cleaned up stale locks")
	return cleanedKeys, nil
}
