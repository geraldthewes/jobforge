package integration

import (
	"encoding/json"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"nomad-mcp-builder/pkg/consul"
	"nomad-mcp-builder/pkg/types"
)

// TestConsulJobWatcher tests the Consul KV job watching functionality
func TestConsulJobWatcher(t *testing.T) {
	// Skip if Consul is not available
	consulConfig := consulapi.DefaultConfig()
	consulClient, err := consulapi.NewClient(consulConfig)
	if err != nil {
		t.Skip("Consul not available, skipping test")
	}

	// Test connectivity
	_, err = consulClient.Status().Leader()
	if err != nil {
		t.Skip("Consul not reachable, skipping test")
	}

	// Create a test job ID
	testJobID := "test-watch-" + time.Now().Format("20060102-150405")
	keyPath := "nomad-build-service/jobs/" + testJobID

	// Clean up after test
	defer func() {
		consulClient.KV().Delete(keyPath, nil)
	}()

	// Create watcher client
	watcher, err := consul.NewClient("")
	if err != nil {
		t.Fatalf("Failed to create watcher client: %v", err)
	}

	// Channels for updates and errors
	updates := make(chan consul.JobUpdate, 10)
	errors := make(chan error, 10)

	// Start watching in background
	go watcher.WatchJob(testJobID, updates, errors)

	// Give watcher time to start
	time.Sleep(500 * time.Millisecond)

	// Simulate job lifecycle by updating KV
	stages := []struct {
		status types.JobStatus
		phase  string
	}{
		{types.StatusPending, ""},
		{types.StatusBuilding, "build"},
		{types.StatusTesting, "test"},
		{types.StatusPublishing, "publish"},
		{types.StatusSucceeded, ""},
	}

	receivedUpdates := []consul.JobUpdate{}

	// Simulate job progress
	for _, stage := range stages {
		// Create job object
		job := types.Job{
			ID:           testJobID,
			Status:       stage.status,
			CurrentPhase: stage.phase,
			UpdatedAt:    time.Now(),
			CreatedAt:    time.Now(),
		}

		// Marshal to JSON
		data, err := json.Marshal(job)
		if err != nil {
			t.Fatalf("Failed to marshal job: %v", err)
		}

		// Write to Consul KV
		pair := &consulapi.KVPair{
			Key:   keyPath,
			Value: data,
		}

		_, err = consulClient.KV().Put(pair, nil)
		if err != nil {
			t.Fatalf("Failed to write to Consul KV: %v", err)
		}

		// Wait for update to be received
		select {
		case update := <-updates:
			t.Logf("Received update: Status=%s, Phase=%s", update.Status, update.Phase)
			receivedUpdates = append(receivedUpdates, update)

			// Verify the update matches what we wrote
			if update.Status != stage.status {
				t.Errorf("Expected status %s, got %s", stage.status, update.Status)
			}
			if update.Phase != stage.phase {
				t.Errorf("Expected phase %s, got %s", stage.phase, update.Phase)
			}

		case err := <-errors:
			t.Fatalf("Received error from watcher: %v", err)

		case <-time.After(10 * time.Second):
			t.Fatalf("Timeout waiting for update for stage %s/%s", stage.status, stage.phase)
		}

		// Small delay between stages
		time.Sleep(100 * time.Millisecond)
	}

	// Verify we received all expected updates
	if len(receivedUpdates) != len(stages) {
		t.Errorf("Expected %d updates, got %d", len(stages), len(receivedUpdates))
	}

	// Verify watcher exits after job completes
	// Drain any buffered duplicate updates first (Consul blocking queries can return
	// spuriously when the global KV index changes or due to network hiccups)
	channelClosed := false
	drainTimeout := time.After(5 * time.Second)
	for !channelClosed {
		select {
		case update, ok := <-updates:
			if !ok {
				// Channel closed as expected
				channelClosed = true
			} else {
				// Got a buffered duplicate update, continue draining
				t.Logf("Draining buffered update: Status=%s, Phase=%s", update.Status, update.Phase)
			}
		case <-drainTimeout:
			t.Error("Watcher did not close updates channel after job completion")
			channelClosed = true
		}
	}
}

// TestConsulJobWatcher_Failure tests watching a job that fails
func TestConsulJobWatcher_Failure(t *testing.T) {
	// Skip if Consul is not available
	consulConfig := consulapi.DefaultConfig()
	consulClient, err := consulapi.NewClient(consulConfig)
	if err != nil {
		t.Skip("Consul not available, skipping test")
	}

	// Test connectivity
	_, err = consulClient.Status().Leader()
	if err != nil {
		t.Skip("Consul not reachable, skipping test")
	}

	// Create a test job ID
	testJobID := "test-watch-fail-" + time.Now().Format("20060102-150405")
	keyPath := "nomad-build-service/jobs/" + testJobID

	// Clean up after test
	defer func() {
		consulClient.KV().Delete(keyPath, nil)
	}()

	// Create watcher client
	watcher, err := consul.NewClient("")
	if err != nil {
		t.Fatalf("Failed to create watcher client: %v", err)
	}

	// Channels for updates and errors
	updates := make(chan consul.JobUpdate, 10)
	errors := make(chan error, 10)

	// Start watching in background
	go watcher.WatchJob(testJobID, updates, errors)

	// Give watcher time to start
	time.Sleep(500 * time.Millisecond)

	// Simulate job failure
	job := types.Job{
		ID:           testJobID,
		Status:       types.StatusFailed,
		CurrentPhase: "build",
		UpdatedAt:    time.Now(),
		CreatedAt:    time.Now(),
		Error:        "build failed: compilation error",
	}

	data, _ := json.Marshal(job)
	pair := &consulapi.KVPair{
		Key:   keyPath,
		Value: data,
	}

	consulClient.KV().Put(pair, nil)

	// Wait for update
	select {
	case update := <-updates:
		if update.Status != types.StatusFailed {
			t.Errorf("Expected status FAILED, got %s", update.Status)
		}
		if update.Error != "build failed: compilation error" {
			t.Errorf("Expected error message, got: %s", update.Error)
		}

	case err := <-errors:
		t.Fatalf("Received error from watcher: %v", err)

	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for failure update")
	}

	// Verify watcher exits after failure
	// Drain any buffered duplicate updates first (Consul blocking queries can return
	// spuriously when the global KV index changes or due to network hiccups)
	channelClosed := false
	drainTimeout := time.After(5 * time.Second)
	for !channelClosed {
		select {
		case update, ok := <-updates:
			if !ok {
				// Channel closed as expected
				channelClosed = true
			} else {
				// Got a buffered duplicate update, continue draining
				t.Logf("Draining buffered update: Status=%s, Phase=%s", update.Status, update.Phase)
			}
		case <-drainTimeout:
			t.Error("Watcher did not close updates channel after job failure")
			channelClosed = true
		}
	}
}
