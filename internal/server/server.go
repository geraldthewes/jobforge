package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"

	"nomad-mcp-builder/internal/config"
	"nomad-mcp-builder/internal/nomad"
	"nomad-mcp-builder/internal/storage"
	"nomad-mcp-builder/pkg/types"
)

// Server represents the API server
type Server struct {
	config     *config.Config
	nomadClient *nomad.Client
	storage    *storage.ConsulStorage
	logger     *logrus.Logger

	// WebSocket connections for log streaming
	wsConnections map[string][]*websocket.Conn
	wsMutex       sync.RWMutex

	// Job-level mutexes to prevent concurrent updates to the same job
	jobMutexes map[string]*sync.Mutex
	jobMutexLock sync.RWMutex

	// WebSocket upgrader
	upgrader websocket.Upgrader

	// Webhook client for sending notifications
	webhookClient *http.Client
}

// getJobMutex returns or creates a mutex for the given job ID
func (s *Server) getJobMutex(jobID string) *sync.Mutex {
	s.jobMutexLock.Lock()
	defer s.jobMutexLock.Unlock()
	
	if mutex, exists := s.jobMutexes[jobID]; exists {
		return mutex
	}
	
	mutex := &sync.Mutex{}
	s.jobMutexes[jobID] = mutex
	return mutex
}

// lockJob locks the job for exclusive access and returns an unlock function
func (s *Server) lockJob(jobID string) func() {
	mutex := s.getJobMutex(jobID)
	mutex.Lock()
	return mutex.Unlock
}

// extractPythonTestOutput extracts python-executor output from logs.
// This output is identified by the "=== Python Test Output" marker.
// Returns the extracted lines, or nil if no python output is found.
func extractPythonTestOutput(logs []string) []string {
	var pythonOutput []string
	inPythonSection := false
	for _, line := range logs {
		if strings.HasPrefix(line, "=== Python Test Output") {
			inPythonSection = true
		}
		if inPythonSection {
			pythonOutput = append(pythonOutput, line)
		}
	}
	return pythonOutput
}

// NewServer creates a new API server
func NewServer(cfg *config.Config, nomadClient *nomad.Client, storage *storage.ConsulStorage, logger *logrus.Logger) *Server {
	return &Server{
		config:        cfg,
		nomadClient:   nomadClient,
		storage:       storage,
		logger:        logger,
		wsConnections: make(map[string][]*websocket.Conn),
		jobMutexes:    make(map[string]*sync.Mutex),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for now
			},
		},
		webhookClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

// Start starts the API server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// RESTful API endpoints
	mux.HandleFunc("/json/submitJob", s.handleSubmitJob)
	mux.HandleFunc("/json/getStatus", s.handleGetStatus)
	mux.HandleFunc("/json/getLogs", s.handleGetLogs)
	mux.HandleFunc("/json/killJob", s.handleKillJob)
	mux.HandleFunc("/json/cleanup", s.handleCleanup)
	mux.HandleFunc("/json/getHistory", s.handleGetHistory)
	mux.HandleFunc("/json/job/", s.handleJobResource)

	// Lock management endpoints
	mux.HandleFunc("/json/locks", s.handleListLocks)
	mux.HandleFunc("/json/locks/force-unlock", s.handleForceUnlock)
	mux.HandleFunc("/json/active-jobs", s.handleListActiveJobs)

	// Storage management endpoints
	mux.HandleFunc("/json/prune-storage", s.handlePruneStorage)
	mux.HandleFunc("/json/prune-job/", s.handlePruneJobStatus)
	mux.HandleFunc("/json/cleanup-buildah-cache", s.handleCleanupBuildahCache)
	mux.HandleFunc("/json/cleanup-cache-job/", s.handleCleanupCacheJobStatus)

	// Health check endpoints
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", s.config.Server.Host, s.config.Server.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	s.logger.WithField("address", server.Addr).Info("Starting API server")

	// Start background cleanup routine
	go s.backgroundCleanup(ctx)

	// Start background job monitoring routine
	go s.backgroundJobMonitor(ctx)

	return server.ListenAndServe()
}

// handleSubmitJob handles job submission requests
func (s *Server) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	
	// Log incoming REST API request
	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "submitJob",
	}).Info("REST API request received")
	
	if r.Method != http.MethodPost {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"status":      http.StatusMethodNotAllowed,
			"duration_ms": duration.Milliseconds(),
		}).Warn("REST API request failed: method not allowed")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req types.SubmitJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"status":      http.StatusBadRequest,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
		}).Warn("REST API request failed: invalid body")
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	
	// Log full request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "submitJob",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Submit job request details")
	}
	
	// Validate required fields
	if err := validateJobConfig(&req.JobConfig); err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"status":      http.StatusBadRequest,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
		}).Warn("REST API request failed: validation error")
		s.writeErrorResponse(w, "Job configuration validation failed", http.StatusBadRequest, err.Error())
		return
	}

	// Check per-owner concurrent build limit
	if err := s.checkOwnerBuildLimit(req.JobConfig.Owner); err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"status":      http.StatusTooManyRequests,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
			"owner":       req.JobConfig.Owner,
		}).Warn("REST API request failed: owner limit exceeded")
		s.writeErrorResponse(w, "Too many concurrent builds", http.StatusTooManyRequests, err.Error())
		return
	}

	// Create new job
	job, err := s.nomadClient.CreateJob(&req.JobConfig)
	if err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"status":      http.StatusInternalServerError,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
		}).Error("REST API request failed: job creation error")
		s.writeErrorResponse(w, "Failed to create job", http.StatusInternalServerError, err.Error())
		return
	}
	
	// Store job in Consul
	if err := s.storage.StoreJob(job); err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"job_id":      job.ID,
			"status":      http.StatusInternalServerError,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
		}).Error("REST API request failed: storage error")
		s.writeErrorResponse(w, "Failed to store job", http.StatusInternalServerError, err.Error())
		return
	}
	
	response := types.SubmitJobResponse{
		JobID:  job.ID,
		Status: job.Status,
	}
	
	s.writeJSONResponse(w, response)
	
	duration := time.Since(startTime)
	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "submitJob",
		"job_id":      job.ID,
		"status":      http.StatusOK,
		"duration_ms": duration.Milliseconds(),
	}).Info("REST API request completed successfully")
}

// handleGetStatus handles status requests
func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req types.GetStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	
	// Log request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "getStatus",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Get status request details")
	}
	
	// Get job from storage
	job, err := s.storage.GetJob(req.JobID)
	if err != nil {
		s.writeErrorResponse(w, "Job not found", http.StatusNotFound, err.Error())
		return
	}
	
	// Update job status from Nomad
	updatedJob, err := s.nomadClient.UpdateJobStatus(job)
	if err != nil {
		s.logger.WithError(err).Warn("Failed to update job status from Nomad")
		// Continue with cached status
	} else {
		job = updatedJob
		// Update storage with latest status
		if err := s.storage.UpdateJob(job); err != nil {
			s.logger.WithError(err).Warn("Failed to update job in storage")
		}
	}

	// Get allocation information for warnings
	allocations, err := s.nomadClient.GetJobAllocations(job)
	if err != nil {
		s.logger.WithError(err).Warn("Failed to get job allocations")
		// Continue without allocations - not a fatal error
	}

	response := types.GetStatusResponse{
		JobID:       job.ID,
		Status:      job.Status,
		Config:      &job.Config, // Include config for debugging
		Metrics:     job.Metrics,
		Error:       job.Error,
		Allocations: allocations,
	}

	s.writeJSONResponse(w, response)
}

// handleGetLogs handles log retrieval requests
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req types.GetLogsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	
	// Log request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "getLogs",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Get logs request details")
	}
	
	// Get job from storage
	job, err := s.storage.GetJob(req.JobID)
	if err != nil {
		s.writeErrorResponse(w, "Job not found", http.StatusNotFound, err.Error())
		return
	}
	
	// Preserve any python-executor output that was appended via ReportTestResult
	// before fetching fresh logs from Nomad (which would overwrite them)
	pythonOutput := extractPythonTestOutput(job.Logs.Test)

	// Get latest logs from Nomad
	logs, err := s.nomadClient.GetJobLogs(job)
	if err != nil {
		s.logger.WithError(err).Warn("Failed to get logs from Nomad")
		// Return cached logs
		logs = job.Logs
	} else {
		// Re-append python output if any was preserved
		if len(pythonOutput) > 0 {
			logs.Test = append(logs.Test, pythonOutput...)
		}

		// Update job with merged logs
		job.Logs = logs
		if err := s.storage.UpdateJob(job); err != nil {
			s.logger.WithError(err).Warn("Failed to update job logs in storage")
		}
	}

	// Filter logs by phase if specified
	var filteredLogs types.JobLogs
	if req.Phase != "" {
		switch req.Phase {
		case "build":
			filteredLogs = types.JobLogs{Build: logs.Build}
		case "test":
			filteredLogs = types.JobLogs{Test: logs.Test}
		case "publish":
			filteredLogs = types.JobLogs{Publish: logs.Publish}
		default:
			// Invalid phase, return all logs
			filteredLogs = logs
		}
	} else {
		// No phase filter, return all logs
		filteredLogs = logs
	}

	response := types.GetLogsResponse{
		JobID: job.ID,
		Logs:  filteredLogs,
	}

	s.writeJSONResponse(w, response)
}

// handleStreamLogs handles WebSocket log streaming
func (s *Server) handleStreamLogs(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id parameter required", http.StatusBadRequest)
		return
	}
	
	// Upgrade to WebSocket
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.WithError(err).Error("Failed to upgrade to WebSocket")
		return
	}
	defer conn.Close()
	
	// Add connection to tracking
	s.wsMutex.Lock()
	s.wsConnections[jobID] = append(s.wsConnections[jobID], conn)
	s.wsMutex.Unlock()
	
	// Remove connection when done
	defer func() {
		s.wsMutex.Lock()
		connections := s.wsConnections[jobID]
		for i, c := range connections {
			if c == conn {
				s.wsConnections[jobID] = append(connections[:i], connections[i+1:]...)
				break
			}
		}
		if len(s.wsConnections[jobID]) == 0 {
			delete(s.wsConnections, jobID)
		}
		s.wsMutex.Unlock()
	}()
	
	// Stream logs
	s.streamJobLogs(conn, jobID)
}

// handleKillJob handles job termination requests
func (s *Server) handleKillJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req types.KillJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	
	// Log request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "killJob",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Kill job request details")
	}
	
	// Get job from storage
	job, err := s.storage.GetJob(req.JobID)
	if err != nil {
		s.writeErrorResponse(w, "Job not found", http.StatusNotFound, err.Error())
		return
	}
	
	// Kill job in Nomad
	err = s.nomadClient.KillJob(job)
	success := err == nil
	
	var message string
	if success {
		message = "Job killed successfully"
		job.Status = types.StatusFailed
		job.Error = "Job killed by user"
		job.FinishedAt = &[]time.Time{time.Now()}[0]
		s.storage.UpdateJob(job)
	} else {
		message = fmt.Sprintf("Failed to kill job: %v", err)
	}
	
	response := types.KillJobResponse{
		JobID:   req.JobID,
		Success: success,
		Message: message,
	}
	
	s.writeJSONResponse(w, response)
}

// handleCleanup handles cleanup requests
func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req types.CleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	
	// Log request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "cleanup",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Cleanup request details")
	}
	
	var cleanedJobs []string
	var err error
	
	if req.All {
		cleanedJobs, err = s.cleanupZombieJobs()
	} else if req.JobID != "" {
		err = s.cleanupSingleJob(req.JobID)
		if err == nil {
			cleanedJobs = []string{req.JobID}
		}
	}
	
	success := err == nil
	message := "Cleanup completed"
	if !success {
		message = fmt.Sprintf("Cleanup failed: %v", err)
	}
	
	response := types.CleanupResponse{
		Success:     success,
		CleanedJobs: cleanedJobs,
		Message:     message,
	}
	
	s.writeJSONResponse(w, response)
}

// handleGetHistory handles job history requests
func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req types.GetHistoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}
	
	// Log request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "getHistory",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Get history request details")
	}
	
	// Set defaults
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if req.Offset < 0 {
		req.Offset = 0
	}
	
	jobs, total, err := s.storage.GetJobHistory(req.Limit, req.Offset)
	if err != nil {
		s.writeErrorResponse(w, "Failed to get job history", http.StatusInternalServerError, err.Error())
		return
	}
	
	response := types.GetHistoryResponse{
		Jobs:  jobs,
		Total: total,
	}
	
	s.writeJSONResponse(w, response)
}

// handleHealth handles health check requests
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	services := make(map[string]string)
	
	// Check Nomad connectivity
	if err := s.nomadClient.Health(); err != nil {
		services["nomad"] = "unhealthy"
	} else {
		services["nomad"] = "healthy"
	}
	
	// Check Consul connectivity
	if err := s.storage.Health(); err != nil {
		services["consul"] = "unhealthy"
	} else {
		services["consul"] = "healthy"
	}
	
	// Overall health status
	status := "healthy"
	for _, serviceStatus := range services {
		if serviceStatus != "healthy" {
			status = "unhealthy"
			break
		}
	}
	
	response := types.HealthResponse{
		Status:    status,
		Services:  services,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	
	if status != "healthy" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	
	s.writeJSONResponse(w, response)
}

// handleReady handles readiness probe requests
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// Simple readiness check - service is ready if it can respond
	response := map[string]string{
		"status": "ready",
		"timestamp": time.Now().Format(time.RFC3339),
	}
	s.writeJSONResponse(w, response)
}

// Helper methods

func (s *Server) writeJSONResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.WithError(err).Error("Failed to encode JSON response")
	}
}

func (s *Server) writeErrorResponse(w http.ResponseWriter, message string, code int, details string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	
	response := types.ErrorResponse{
		Error:   message,
		Code:    code,
		Details: details,
	}
	
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.WithError(err).Error("Failed to encode error response")
	}
}

func (s *Server) streamJobLogs(conn *websocket.Conn, jobID string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	
	var lastLogCount int
	
	for {
		select {
		case <-ticker.C:
			job, err := s.storage.GetJob(jobID)
			if err != nil {
				s.logger.WithError(err).Warn("Failed to get job for log streaming")
				continue
			}
			
			// Get updated logs from Nomad
			logs, err := s.nomadClient.GetJobLogs(job)
			if err != nil {
				continue
			}
			
			// Send new log entries
			totalLogs := len(logs.Build) + len(logs.Test) + len(logs.Publish)
			if totalLogs > lastLogCount {
				// Send build logs
				for i := lastLogCount; i < len(logs.Build) && i >= 0; i++ {
					msg := types.StreamLogsMessage{
						JobID:     jobID,
						Phase:     "build",
						Timestamp: time.Now().Format(time.RFC3339),
						Level:     "INFO",
						Message:   logs.Build[i],
					}
					if err := conn.WriteJSON(msg); err != nil {
						return
					}
				}
				
				// Send test logs
				buildLogCount := len(logs.Build)
				for i := max(0, lastLogCount-buildLogCount); i < len(logs.Test); i++ {
					msg := types.StreamLogsMessage{
						JobID:     jobID,
						Phase:     "test",
						Timestamp: time.Now().Format(time.RFC3339),
						Level:     "INFO",
						Message:   logs.Test[i],
					}
					if err := conn.WriteJSON(msg); err != nil {
						return
					}
				}
				
				// Send publish logs
				testLogStart := buildLogCount + len(logs.Test)
				for i := max(0, lastLogCount-testLogStart); i < len(logs.Publish); i++ {
					msg := types.StreamLogsMessage{
						JobID:     jobID,
						Phase:     "publish",
						Timestamp: time.Now().Format(time.RFC3339),
						Level:     "INFO",
						Message:   logs.Publish[i],
					}
					if err := conn.WriteJSON(msg); err != nil {
						return
					}
				}
				
				lastLogCount = totalLogs
			}
			
			// Stop streaming if job is finished
			if job.Status == types.StatusSucceeded || job.Status == types.StatusFailed {
				return
			}
		}
	}
}

func (s *Server) backgroundJobMonitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second) // Check job status every 5 seconds
	defer ticker.Stop()
	
	s.logger.Info("Starting background job monitoring")
	
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("Stopping background job monitoring")
			return
		case <-ticker.C:
			jobIDs, err := s.storage.ListJobs()
			if err != nil {
				s.logger.WithError(err).Warn("Failed to list jobs during monitoring")
				continue
			}
			
			activeJobs := 0
			
			for _, jobID := range jobIDs {
				// Get the job details
				job, err := s.storage.GetJob(jobID)
				if err != nil {
					s.logger.WithError(err).WithField("job_id", jobID).Warn("Failed to get job during monitoring")
					continue
				}
				
				// Only monitor jobs that are not in final states
				if job.Status != types.StatusSucceeded && job.Status != types.StatusFailed {
					activeJobs++
					
					// Lock the job to prevent concurrent updates
					unlock := s.lockJob(job.ID)
					
					// Re-fetch job after acquiring lock (it might have been updated)
					freshJob, err := s.storage.GetJob(job.ID)
					if err != nil {
						unlock()
						s.logger.WithError(err).WithField("job_id", job.ID).Warn("Failed to re-fetch job during monitoring")
						continue
					}
					
					oldStatus := freshJob.Status
					oldPhase := freshJob.CurrentPhase
					
					// Update job status which will trigger phase transitions
					updatedJob, err := s.nomadClient.UpdateJobStatus(freshJob)
					if err != nil {
						// Check if this is a 404 error indicating the job no longer exists in Nomad
						if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "job not found") {
							s.logger.WithField("job_id", job.ID).Info("Job no longer exists in Nomad, removing from storage")
							// Move job to history before deleting from active storage
							if history := s.convertJobToHistory(freshJob); history != nil {
								s.storage.StoreJobHistory(history)
							}
							s.storage.DeleteJob(job.ID)
							unlock()
							continue
						}
						unlock()
						s.logger.WithError(err).WithField("job_id", job.ID).Warn("Failed to update job status during monitoring")
						continue
					}
					
					// Save updated job state
					s.storage.UpdateJob(updatedJob)
					
					// Send webhooks for status/phase changes
					if updatedJob.Status != oldStatus || updatedJob.CurrentPhase != oldPhase {
						s.logger.WithFields(logrus.Fields{
							"job_id":    job.ID,
							"old_status": oldStatus,
							"new_status": updatedJob.Status,
							"old_phase":  oldPhase,
							"new_phase":  updatedJob.CurrentPhase,
						}).Info("Job status/phase changed")
						
						// Send appropriate webhook events
						s.handleJobStatusChange(updatedJob, oldStatus, oldPhase)
					}
					
					unlock()
				}
			}
			
			if activeJobs > 0 {
				s.logger.WithField("active_jobs", activeJobs).Debug("Monitoring active jobs")
			}
		}
	}
}

func (s *Server) backgroundCleanup(ctx context.Context) {
	historyTicker := time.NewTicker(1 * time.Hour)
	staleLockTicker := time.NewTicker(15 * time.Minute)
	defer historyTicker.Stop()
	defer staleLockTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-historyTicker.C:
			// Get configurable retention period, default to 7 days
			retentionDays := s.config.Build.LogRetentionDays
			if retentionDays <= 0 {
				retentionDays = 7 // Default to 7 days
			}

			// Only cleanup old job history automatically - normal cleanup should be done explicitly
			if err := s.storage.CleanupOldHistory(time.Duration(retentionDays) * 24 * time.Hour); err != nil {
				s.logger.WithError(err).Warn("Failed to cleanup old job history")
			}

			// Cleanup zombie jobs (jobs running longer than 24 hours without updates)
			if _, err := s.cleanupZombieJobs(); err != nil {
				s.logger.WithError(err).Warn("Failed to cleanup zombie jobs")
			}
		case <-staleLockTicker.C:
			// Cleanup stale locks (orphaned locks with no session or completed jobs)
			cleanedKeys, err := s.storage.CleanupStaleLocks()
			if err != nil {
				s.logger.WithError(err).Warn("Failed to cleanup stale locks")
			} else if len(cleanedKeys) > 0 {
				s.logger.WithFields(map[string]interface{}{
					"cleaned_count": len(cleanedKeys),
					"cleaned_keys":  cleanedKeys,
				}).Info("Automatic stale lock cleanup completed")
			}
		}
	}
}

func (s *Server) cleanupZombieJobs() ([]string, error) {
	// Implementation for cleaning up zombie/orphaned jobs
	// This would involve querying Nomad for running jobs and comparing with stored jobs
	return []string{}, nil // Placeholder
}

func (s *Server) cleanupSingleJob(jobID string) error {
	job, err := s.storage.GetJob(jobID)
	if err != nil {
		return err
	}
	
	return s.nomadClient.CleanupJob(job)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// convertJobToHistory converts a Job to JobHistory for archival
func (s *Server) convertJobToHistory(job *types.Job) *types.JobHistory {
	if job == nil {
		return nil
	}

	var duration time.Duration
	if job.FinishedAt != nil {
		duration = job.FinishedAt.Sub(job.CreatedAt)
	} else {
		duration = time.Since(job.CreatedAt)
	}

	history := &types.JobHistory{
		ID:        job.ID,
		Config:    job.Config,
		Status:    job.Status,
		CreatedAt: job.CreatedAt,
		Duration:  duration,
		Metrics:   job.Metrics,
	}

	return history
}
// handlePruneStorage handles storage prune requests
func (s *Server) handlePruneStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req types.PruneStorageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}

	// Log request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "pruneStorage",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Prune storage request details")
	}

	// Safety check: warn about active builds
	var warnings []string
	if !req.Force {
		activeBuilds, err := s.nomadClient.CheckActiveBuildJobs()
		if err != nil {
			s.logger.WithError(err).Warn("Failed to check for active build jobs")
		} else if len(activeBuilds) > 0 {
			warnings = append(warnings, fmt.Sprintf("Warning: %d active build job(s) detected. Use --force to proceed anyway.", len(activeBuilds)))
			if !req.DryRun {
				// For non-dry-run without force, reject if aggressive prune requested
				if req.All {
					s.writeErrorResponse(w, "Cannot perform aggressive prune with active builds", http.StatusConflict,
						fmt.Sprintf("%d active build(s) detected. Use --force to override.", len(activeBuilds)))
					return
				}
			}
		}
	}

	// Submit prune job
	pruneJobID, err := s.nomadClient.RunPruneStorage(&req)
	if err != nil {
		s.writeErrorResponse(w, "Failed to submit prune storage job", http.StatusInternalServerError, err.Error())
		return
	}

	response := types.PruneStorageResponse{
		Success:  true,
		JobID:    pruneJobID,
		Message:  "Prune storage job submitted successfully",
		Warnings: warnings,
	}

	if req.DryRun {
		response.Message = "Prune storage job submitted in dry-run mode (no actual deletions)"
	}

	s.writeJSONResponse(w, response)

	s.logger.WithFields(logrus.Fields{
		"prune_job_id": pruneJobID,
		"dry_run":      req.DryRun,
		"all":          req.All,
		"all_nodes":    req.AllNodes,
		"project":      req.Project,
	}).Info("Prune storage job submitted")
}

// handlePruneJobStatus handles GET /json/prune-job/{jobID}/status
// This queries Nomad directly for prune job status (not Consul storage)
func (s *Server) handlePruneJobStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse URL path: /json/prune-job/{jobID}/status
	path := strings.TrimPrefix(r.URL.Path, "/json/prune-job/")
	parts := strings.Split(path, "/")

	if len(parts) < 2 || parts[1] != "status" {
		http.Error(w, "Invalid path. Use /json/prune-job/{jobID}/status", http.StatusBadRequest)
		return
	}

	jobID := parts[0]
	if jobID == "" {
		http.Error(w, "Job ID is required", http.StatusBadRequest)
		return
	}

	status, logs, err := s.nomadClient.GetPruneJobStatus(jobID)
	if err != nil {
		s.writeErrorResponse(w, "Failed to get prune job status", http.StatusInternalServerError, err.Error())
		return
	}

	response := struct {
		Status string   `json:"status"`
		Logs   []string `json:"logs,omitempty"`
	}{
		Status: status,
		Logs:   logs,
	}

	s.writeJSONResponse(w, response)
}

// handleCleanupBuildahCache handles cleanup buildah cache requests
func (s *Server) handleCleanupBuildahCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req types.CleanupBuildahCacheRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}

	// Log request details at debug level
	if s.logger.Level >= logrus.DebugLevel {
		reqJSON, _ := json.MarshalIndent(req, "", "  ")
		s.logger.WithFields(map[string]interface{}{
			"endpoint":    "cleanupBuildahCache",
			"remote_addr": r.RemoteAddr,
			"request":     string(reqJSON),
		}).Debug("Cleanup buildah cache request details")
	}

	// Safety check: warn about active builds
	var warnings []string
	if !req.Force {
		activeBuilds, err := s.nomadClient.CheckActiveBuildJobs()
		if err != nil {
			s.logger.WithError(err).Warn("Failed to check for active build jobs")
		} else if len(activeBuilds) > 0 {
			warnings = append(warnings, fmt.Sprintf("Warning: %d active build job(s) detected. Use --force to proceed anyway.", len(activeBuilds)))
			if !req.DryRun {
				// For non-dry-run without force, reject if full reset requested
				if req.Full {
					s.writeErrorResponse(w, "Cannot perform full reset with active builds", http.StatusConflict,
						fmt.Sprintf("%d active build(s) detected. Use --force to override.", len(activeBuilds)))
					return
				}
			}
		}
	}

	// Submit cleanup job
	cleanupJobID, err := s.nomadClient.RunCleanupBuildahCache(&req)
	if err != nil {
		s.writeErrorResponse(w, "Failed to submit cleanup buildah cache job", http.StatusInternalServerError, err.Error())
		return
	}

	response := types.CleanupBuildahCacheResponse{
		Success:  true,
		JobID:    cleanupJobID,
		Message:  "Cleanup buildah cache job submitted successfully",
		Warnings: warnings,
	}

	if req.DryRun {
		response.Message = "Cleanup buildah cache job submitted in dry-run mode (no actual changes)"
	}

	s.writeJSONResponse(w, response)

	s.logger.WithFields(logrus.Fields{
		"cleanup_job_id": cleanupJobID,
		"dry_run":        req.DryRun,
		"full":           req.Full,
		"all_nodes":      req.AllNodes,
		"node_name":      req.NodeName,
	}).Info("Cleanup buildah cache job submitted")
}

// handleCleanupCacheJobStatus handles GET /json/cleanup-cache-job/{jobID}/status
// This queries Nomad directly for cleanup job status (not Consul storage)
func (s *Server) handleCleanupCacheJobStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse URL path: /json/cleanup-cache-job/{jobID}/status
	path := strings.TrimPrefix(r.URL.Path, "/json/cleanup-cache-job/")
	parts := strings.Split(path, "/")

	if len(parts) < 2 || parts[1] != "status" {
		http.Error(w, "Invalid path. Use /json/cleanup-cache-job/{jobID}/status", http.StatusBadRequest)
		return
	}

	jobID := parts[0]
	if jobID == "" {
		http.Error(w, "Job ID is required", http.StatusBadRequest)
		return
	}

	status, logs, err := s.nomadClient.GetCleanupBuildahCacheJobStatus(jobID)
	if err != nil {
		s.writeErrorResponse(w, "Failed to get cleanup job status", http.StatusInternalServerError, err.Error())
		return
	}

	response := struct {
		Status string   `json:"status"`
		Logs   []string `json:"logs,omitempty"`
	}{
		Status: status,
		Logs:   logs,
	}

	s.writeJSONResponse(w, response)
}

// validateJobConfig validates the job configuration and returns an error if validation fails
func validateJobConfig(config *types.JobConfig) error {
	// Required fields
	if config.Owner == "" {
		return fmt.Errorf("owner is required")
	}
	if config.RepoURL == "" {
		return fmt.Errorf("repo_url is required")
	}
	if config.GitRef == "" {
		return fmt.Errorf("git_ref is required")
	}
	if config.DockerfilePath == "" {
		return fmt.Errorf("dockerfile_path is required")
	}
	// Validate dockerfile_context if provided (optional field)
	if config.DockerfileContext != "" {
		// Prevent path traversal attacks
		if strings.Contains(config.DockerfileContext, "..") {
			return fmt.Errorf("dockerfile_context cannot contain '..' (path traversal)")
		}
		// Must be a relative path (not absolute)
		if strings.HasPrefix(config.DockerfileContext, "/") {
			return fmt.Errorf("dockerfile_context must be a relative path")
		}
	}
	if config.ImageName == "" {
		return fmt.Errorf("image_name is required")
	}
	// image_tags is optional - will default to job-id if not provided
	if config.RegistryURL == "" {
		return fmt.Errorf("registry_url is required")
	}
	
	// Validate test configuration if provided
	if config.Test != nil {
		// Validate at least one testing mode is specified
		if len(config.Test.Commands) == 0 && !config.Test.EntryPoint {
			// This is allowed - empty test config means no testing
		}
		// Validate env is a valid map (Go already enforces this at unmarshal time)
		// No additional validation needed for env variables

		// Validate vault secrets configuration
		if len(config.Test.VaultSecrets) > 0 {
			// If vault secrets are provided, vault policies must be specified
			if len(config.Test.VaultPolicies) == 0 {
				return fmt.Errorf("vault_policies is required when vault_secrets are specified")
			}

			// Validate each vault secret
			for i, secret := range config.Test.VaultSecrets {
				if secret.Path == "" {
					return fmt.Errorf("vault_secrets[%d]: path is required", i)
				}
				if len(secret.Fields) == 0 {
					return fmt.Errorf("vault_secrets[%d]: fields map cannot be empty", i)
				}
				// Validate field mappings
				for vaultField, envVar := range secret.Fields {
					if vaultField == "" || envVar == "" {
						return fmt.Errorf("vault_secrets[%d]: invalid field mapping (empty field or env var)", i)
					}
				}
			}
		}
	}

	// Optional fields (git_credentials_path, registry_credentials_path, test, image_tags)
	// are allowed to be empty

	return nil
}

// handleJobResource handles RESTful job resource endpoints
// Routes: GET /json/job/{jobID}/status, GET /json/job/{jobID}/logs,
//         GET /json/job/{jobID}/test-endpoint, POST /json/job/{jobID}/test-result
func (s *Server) handleJobResource(w http.ResponseWriter, r *http.Request) {
	// Parse URL path: /json/job/{jobID}/{resource}
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/json/job/")
	parts := strings.Split(path, "/")

	if len(parts) != 2 {
		http.Error(w, "Invalid path format. Expected: /json/job/{jobID}/{status|logs|test-endpoint|test-result}", http.StatusBadRequest)
		return
	}

	jobID := parts[0]
	resource := parts[1]

	if jobID == "" {
		http.Error(w, "Job ID is required", http.StatusBadRequest)
		return
	}

	switch resource {
	case "status":
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleJobStatus(w, r, jobID)
	case "logs":
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleJobLogs(w, r, jobID)
	case "test-endpoint":
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleGetTestEndpoint(w, r, jobID)
	case "test-result":
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleReportTestResult(w, r, jobID)
	default:
		http.Error(w, "Invalid resource. Expected: status, logs, test-endpoint, or test-result", http.StatusBadRequest)
	}
}

// handleJobStatus handles GET /json/job/{jobID}/status
func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request, jobID string) {
	startTime := time.Now()
	
	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "getStatus",
		"job_id":      jobID,
	}).Info("REST API status request received")
	
	job, err := s.storage.GetJob(jobID)
	if err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"job_id":      jobID,
			"status":      http.StatusInternalServerError,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
		}).Error("REST API status request failed: storage error")
		s.writeErrorResponse(w, "Failed to get job", http.StatusInternalServerError, "")
		return
	}
	
	if job == nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"job_id":      jobID,
			"status":      http.StatusNotFound,
			"duration_ms": duration.Milliseconds(),
		}).Warn("REST API status request failed: job not found")
		s.writeErrorResponse(w, "Job not found", http.StatusNotFound, "")
		return
	}
	
	// Update job status before returning
	updatedJob, err := s.nomadClient.UpdateJobStatus(job)
	if err != nil {
		s.logger.WithError(err).WithField("job_id", jobID).Warn("Failed to update job status from Nomad")
		// Continue with existing job data rather than failing
		updatedJob = job
	}

	// Get allocation information for warnings
	allocations, err := s.nomadClient.GetJobAllocations(updatedJob)
	if err != nil {
		s.logger.WithError(err).WithField("job_id", jobID).Warn("Failed to get job allocations")
		// Continue without allocations - not a fatal error
	}

	response := types.GetStatusResponse{
		JobID:       updatedJob.ID,
		Status:      updatedJob.Status,
		Config:      &updatedJob.Config, // Include config for debugging
		Metrics:     updatedJob.Metrics,
		Error:       updatedJob.Error,
		Allocations: allocations,
	}
	
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.WithError(err).Error("Failed to encode status response")
		s.writeErrorResponse(w, "Failed to encode response", http.StatusInternalServerError, "")
		return
	}
	
	duration := time.Since(startTime)
	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "getStatus",
		"job_id":      jobID,
		"job_status":  updatedJob.Status,
		"status":      http.StatusOK,
		"duration_ms": duration.Milliseconds(),
	}).Info("REST API status request completed")
}

// handleJobLogs handles GET /json/job/{jobID}/logs
func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request, jobID string) {
	startTime := time.Now()
	
	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "getLogs",
		"job_id":      jobID,
	}).Info("REST API logs request received")
	
	// Lock the job to ensure consistent read during potential updates
	unlock := s.lockJob(jobID)
	defer unlock()
	
	job, err := s.storage.GetJob(jobID)
	if err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"job_id":      jobID,
			"status":      http.StatusInternalServerError,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
		}).Error("REST API logs request failed: storage error")
		s.writeErrorResponse(w, "Failed to get job", http.StatusInternalServerError, "")
		return
	}
	
	if job == nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"job_id":      jobID,
			"status":      http.StatusNotFound,
			"duration_ms": duration.Milliseconds(),
		}).Warn("REST API logs request failed: job not found")
		s.writeErrorResponse(w, "Job not found", http.StatusNotFound, "")
		return
	}
	
	logs, err := s.nomadClient.GetJobLogs(job)
	if err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":      r.Method,
			"uri":         r.RequestURI,
			"remote_addr": r.RemoteAddr,
			"interface":   "REST",
			"job_id":      jobID,
			"status":      http.StatusInternalServerError,
			"duration_ms": duration.Milliseconds(),
			"error":       err.Error(),
		}).Error("REST API logs request failed: failed to retrieve logs")
		s.writeErrorResponse(w, "Failed to get job logs", http.StatusInternalServerError, "")
		return
	}
	
	response := types.GetLogsResponse{
		JobID: job.ID,
		Logs:  logs,
	}
	
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.WithError(err).Error("Failed to encode logs response")
		s.writeErrorResponse(w, "Failed to encode response", http.StatusInternalServerError, "")
		return
	}
	
	duration := time.Since(startTime)
	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "getLogs",
		"job_id":      jobID,
		"status":      http.StatusOK,
		"duration_ms": duration.Milliseconds(),
	}).Info("REST API logs request completed")
}

// handleGetTestEndpoint handles GET /json/job/{jobID}/test-endpoint
// Returns the external test container's endpoint information for CLI to connect to
func (s *Server) handleGetTestEndpoint(w http.ResponseWriter, r *http.Request, jobID string) {
	startTime := time.Now()

	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "getTestEndpoint",
		"job_id":      jobID,
	}).Info("REST API test endpoint request received")

	job, err := s.storage.GetJob(jobID)
	if err != nil {
		s.writeErrorResponse(w, "Failed to get job", http.StatusInternalServerError, err.Error())
		return
	}

	if job == nil {
		s.writeErrorResponse(w, "Job not found", http.StatusNotFound, "")
		return
	}

	// Check if this is an external test job
	if job.Config.Test == nil || job.Config.Test.PythonFile == "" {
		s.writeErrorResponse(w, "Job is not configured for external python tests", http.StatusBadRequest, "")
		return
	}

	// If endpoint not yet discovered, try to get it
	if job.TestServiceHost == "" && job.TestJobNomadID != "" {
		host, port, err := s.nomadClient.GetExternalTestEndpoint(job)
		if err == nil && host != "" && port > 0 {
			oldStatus := job.Status
			oldPhase := job.CurrentPhase
			job.TestServiceHost = host
			job.TestServicePort = port
			job.Status = types.StatusTestingExternal
			s.storage.UpdateJob(job)
			s.handleJobStatusChange(job, oldStatus, oldPhase)
		}
	}

	healthEndpoint := "/health"
	if job.Config.Test != nil && job.Config.Test.HealthEndpoint != "" {
		healthEndpoint = job.Config.Test.HealthEndpoint
	}

	response := types.GetTestEndpointResponse{
		JobID:          job.ID,
		ServiceHost:    job.TestServiceHost,
		ServicePort:    job.TestServicePort,
		HealthEndpoint: healthEndpoint,
		Status:         job.Status,
	}

	duration := time.Since(startTime)
	s.logger.WithFields(map[string]interface{}{
		"method":       r.Method,
		"uri":          r.RequestURI,
		"remote_addr":  r.RemoteAddr,
		"interface":    "REST",
		"endpoint":     "getTestEndpoint",
		"job_id":       jobID,
		"service_host": job.TestServiceHost,
		"service_port": job.TestServicePort,
		"status":       http.StatusOK,
		"duration_ms":  duration.Milliseconds(),
	}).Info("REST API test endpoint request completed")

	s.writeJSONResponse(w, response)
}

// handleReportTestResult handles POST /json/job/{jobID}/test-result
// Receives test results from CLI after running external python tests
func (s *Server) handleReportTestResult(w http.ResponseWriter, r *http.Request, jobID string) {
	startTime := time.Now()

	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "reportTestResult",
		"job_id":      jobID,
	}).Info("REST API test result report received")

	var req types.ReportTestResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}

	// Lock the job for update
	unlock := s.lockJob(jobID)
	defer unlock()

	job, err := s.storage.GetJob(jobID)
	if err != nil {
		s.writeErrorResponse(w, "Failed to get job", http.StatusInternalServerError, err.Error())
		return
	}

	if job == nil {
		s.writeErrorResponse(w, "Job not found", http.StatusNotFound, "")
		return
	}

	// Capture old status for webhook handling
	oldStatus := job.Status
	oldPhase := job.CurrentPhase

	// Stop the external test container
	if err := s.nomadClient.StopExternalTestJob(job); err != nil {
		s.logger.WithError(err).Warn("Failed to stop external test job")
	}

	// Capture test container logs before stopping
	if logs, err := s.nomadClient.GetJobLogs(job); err == nil {
		job.Logs.Test = logs.Test
	}

	// Append CLI-reported output to logs
	if req.Stdout != "" {
		job.Logs.Test = append(job.Logs.Test, "=== Python Test Output (stdout) ===")
		job.Logs.Test = append(job.Logs.Test, strings.Split(req.Stdout, "\n")...)
	}
	if req.Stderr != "" {
		job.Logs.Test = append(job.Logs.Test, "=== Python Test Output (stderr) ===")
		job.Logs.Test = append(job.Logs.Test, strings.Split(req.Stderr, "\n")...)
	}

	// Update metrics
	now := time.Now()
	job.Metrics.TestEnd = &now
	if job.Metrics.TestStart != nil {
		job.Metrics.TestDuration = now.Sub(*job.Metrics.TestStart)
	}

	var responseMessage string
	if req.Success {
		// Proceed to publish phase
		if err := s.nomadClient.StartPublishPhaseAfterExternalTest(job); err != nil {
			job.Status = types.StatusFailed
			job.Error = fmt.Sprintf("Failed to start publish phase: %v", err)
			job.FinishedAt = &now
			job.Metrics.JobEnd = &now
			responseMessage = "Test passed but failed to start publish phase"
		} else {
			job.Status = types.StatusPublishing
			job.CurrentPhase = "publish"
			job.Metrics.PublishStart = &now
			responseMessage = "Test passed, publish phase started"
		}
	} else {
		job.Status = types.StatusFailed
		job.FailedPhase = "test"
		job.Error = fmt.Sprintf("Python tests failed with exit code %d", req.ExitCode)
		job.FinishedAt = &now
		job.Metrics.JobEnd = &now
		responseMessage = "Test failed"

		// Release build lock (already called above, but ensure cleanup)
		// Note: StopExternalTestJob already called above

		// Cleanup temp images
		s.nomadClient.CleanupJob(job)
	}

	// Persist job state
	if err := s.storage.UpdateJob(job); err != nil {
		s.logger.WithError(err).Error("Failed to update job after test result")
	}

	// Update Consul KV for watchers
	s.handleJobStatusChange(job, oldStatus, oldPhase)

	response := types.ReportTestResultResponse{
		JobID:   job.ID,
		Status:  job.Status,
		Message: responseMessage,
	}

	duration := time.Since(startTime)
	s.logger.WithFields(map[string]interface{}{
		"method":      r.Method,
		"uri":         r.RequestURI,
		"remote_addr": r.RemoteAddr,
		"interface":   "REST",
		"endpoint":    "reportTestResult",
		"job_id":      jobID,
		"success":     req.Success,
		"exit_code":   req.ExitCode,
		"new_status":  job.Status,
		"status":      http.StatusOK,
		"duration_ms": duration.Milliseconds(),
	}).Info("REST API test result report processed")

	s.writeJSONResponse(w, response)
}

// sendWebhook sends a webhook notification for job events
func (s *Server) sendWebhook(job *types.Job, event types.WebhookEvent) {
	if job.Config.WebhookURL == "" {
		return
	}
	
	// Check if we should send webhook for this event type
	shouldSend := false
	switch event {
	case types.WebhookEventJobCompleted:
		shouldSend = job.Config.WebhookOnSuccess || (job.Config.WebhookOnSuccess == false && job.Config.WebhookOnFailure == false) // Default to true
	case types.WebhookEventJobFailed, types.WebhookEventBuildFailed, types.WebhookEventTestFailed:
		shouldSend = job.Config.WebhookOnFailure || (job.Config.WebhookOnSuccess == false && job.Config.WebhookOnFailure == false) // Default to true
	default:
		shouldSend = true // Send all other events by default
	}
	
	if !shouldSend {
		return
	}
	
	// Create webhook payload
	payload := types.WebhookPayload{
		JobID:     job.ID,
		Status:    job.Status,
		Timestamp: time.Now(),
		Owner:     job.Config.Owner,
		RepoURL:   job.Config.RepoURL,
		GitRef:    job.Config.GitRef,
		ImageName: job.Config.ImageName,
		ImageTags: job.Config.ImageTags,
		Phase:     job.CurrentPhase,
	}
	
	// Calculate duration using job start/end times
	if job.StartedAt != nil && job.FinishedAt != nil {
		payload.Duration = job.FinishedAt.Sub(*job.StartedAt)
	} else if job.Metrics.JobStart != nil && job.Metrics.JobEnd != nil {
		payload.Duration = job.Metrics.JobEnd.Sub(*job.Metrics.JobStart)
	}
	
	if job.Status == types.StatusFailed && job.Error != "" {
		payload.Error = job.Error
	}
	
	// Include logs and metrics from the job struct
	payload.Logs = &job.Logs
	payload.Metrics = &job.Metrics
	
	// Send webhook asynchronously
	go s.sendWebhookAsync(job.Config.WebhookURL, job.Config.WebhookSecret, job.Config.WebhookHeaders, &payload)
}

// sendWebhookAsync sends webhook notification asynchronously with retries
func (s *Server) sendWebhookAsync(webhookURL, secret string, headers map[string]string, payload *types.WebhookPayload) {
	maxRetries := 3
	retryDelay := time.Second
	
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := s.sendWebhookRequest(webhookURL, secret, headers, payload); err != nil {
			s.logger.WithFields(logrus.Fields{
				"job_id":      payload.JobID,
				"webhook_url": webhookURL,
				"attempt":     attempt,
				"error":       err,
			}).Warn("Webhook delivery failed")
			
			if attempt < maxRetries {
				time.Sleep(retryDelay * time.Duration(attempt))
			}
		} else {
			s.logger.WithFields(logrus.Fields{
				"job_id":      payload.JobID,
				"webhook_url": webhookURL,
				"status":      payload.Status,
			}).Info("Webhook delivered successfully")
			return
		}
	}
	
	s.logger.WithFields(logrus.Fields{
		"job_id":      payload.JobID,
		"webhook_url": webhookURL,
	}).Error("Webhook delivery failed after all retries")
}

// sendWebhookRequest sends the actual HTTP request to the webhook URL
func (s *Server) sendWebhookRequest(webhookURL, secret string, headers map[string]string, payload *types.WebhookPayload) error {
	// Marshal payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook payload: %w", err)
	}
	
	// Create HTTP request
	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create webhook request: %w", err)
	}
	
	// Set default headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nomad-build-service/1.0")
	
	// Add custom headers
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	
	// Add HMAC signature if secret is provided
	if secret != "" {
		signature := s.generateWebhookSignature(jsonData, secret)
		req.Header.Set("X-Webhook-Signature", signature)
		payload.Signature = signature
	}
	
	// Set timeout for webhook requests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	
	// Send request
	resp, err := s.webhookClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send webhook request: %w", err)
	}
	defer resp.Body.Close()
	
	// Check response status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook returned status %d: %s", resp.StatusCode, string(body))
	}
	
	return nil
}

// generateWebhookSignature generates HMAC-SHA256 signature for webhook authentication
func (s *Server) generateWebhookSignature(payload []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

// handleJobStatusChange sends appropriate webhook events based on job status/phase changes
func (s *Server) handleJobStatusChange(job *types.Job, oldStatus types.JobStatus, oldPhase string) {
	newStatus := job.Status
	newPhase := job.CurrentPhase

	// Determine the appropriate webhook event based on status and phase changes
	var events []types.WebhookEvent

	// Phase-specific events (only for phase transitions)
	if newPhase != oldPhase {
		switch newPhase {
		case "build":
			if oldPhase != "build" {
				events = append(events, types.WebhookEventBuildStarted)
			}
		case "test":
			if oldPhase != "test" {
				events = append(events, types.WebhookEventTestStarted)
			}
		case "publish":
			if oldPhase != "publish" {
				events = append(events, types.WebhookEventPublishStarted)
			}
		}
	}

	// Status-specific events (job completion/failure always takes priority)
	if newStatus != oldStatus {
		switch newStatus {
		case types.StatusSucceeded:
			// Job completed successfully - always send job completion event
			events = append(events, types.WebhookEventJobCompleted)

			// Also send phase completion events based on current phase
			switch newPhase {
			case "build":
				events = append(events, types.WebhookEventBuildCompleted)
			case "test":
				events = append(events, types.WebhookEventTestCompleted)
			case "publish":
				events = append(events, types.WebhookEventPublishCompleted)
			}

		case types.StatusFailed:
			// Job failed - always send job failure event
			events = append(events, types.WebhookEventJobFailed)

			// Also send phase failure events based on current phase
			switch newPhase {
			case "build":
				events = append(events, types.WebhookEventBuildFailed)
			case "test":
				events = append(events, types.WebhookEventTestFailed)
			case "publish":
				events = append(events, types.WebhookEventPublishFailed)
			}
		}
	}

	// Send all applicable webhook events
	for _, event := range events {
		s.sendWebhook(job, event)
	}
}

// checkOwnerBuildLimit checks if the owner has reached their concurrent build limit
func (s *Server) checkOwnerBuildLimit(owner string) error {
	limit := s.config.Build.MaxConcurrentBuildsPerOwner
	if limit <= 0 {
		// No limit configured
		return nil
	}

	activeJobs, err := s.storage.GetActiveJobsForOwner(owner)
	if err != nil {
		s.logger.WithError(err).WithField("owner", owner).Warn("Failed to get active jobs for owner")
		// Don't block the build if we can't check the limit
		return nil
	}

	if len(activeJobs) >= limit {
		var activeJobIDs []string
		for _, job := range activeJobs {
			activeJobIDs = append(activeJobIDs, job.JobID)
		}
		return fmt.Errorf("owner '%s' has %d active builds (limit: %d). Complete or kill existing jobs first. Active jobs: %s",
			owner, len(activeJobs), limit, strings.Join(activeJobIDs, ", "))
	}

	return nil
}

// handleListLocks handles GET /json/locks - list all build locks
func (s *Server) handleListLocks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req types.ListLocksRequest
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
			return
		}
	} else {
		// Parse query parameters for GET
		req.StaleOnly = r.URL.Query().Get("stale_only") == "true"
		req.Owner = r.URL.Query().Get("owner")
	}

	locks, err := s.storage.ListLocks()
	if err != nil {
		s.writeErrorResponse(w, "Failed to list locks", http.StatusInternalServerError, err.Error())
		return
	}

	// Filter locks if requested
	var filteredLocks []types.LockInfo
	staleCount := 0
	for _, lock := range locks {
		if lock.IsStale {
			staleCount++
		}

		// Apply filters
		if req.StaleOnly && !lock.IsStale {
			continue
		}
		if req.Owner != "" && lock.Owner != req.Owner {
			continue
		}
		filteredLocks = append(filteredLocks, lock)
	}

	response := types.ListLocksResponse{
		Locks:      filteredLocks,
		TotalCount: len(locks),
		StaleCount: staleCount,
	}

	s.writeJSONResponse(w, response)
}

// handleForceUnlock handles POST /json/locks/force-unlock - forcefully release locks
func (s *Server) handleForceUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req types.ForceUnlockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
		return
	}

	// Validate request
	if req.All && !req.Force {
		s.writeErrorResponse(w, "The --all flag requires --force for safety", http.StatusBadRequest, "")
		return
	}

	if req.LockKey == "" && !req.StaleOnly && !req.All {
		s.writeErrorResponse(w, "Must specify lock_key, stale_only, or all", http.StatusBadRequest, "")
		return
	}

	var unlockedKeys []string
	var failedKeys []string

	if req.LockKey != "" {
		// Unlock a specific lock
		if err := s.storage.ForceReleaseLock(req.LockKey); err != nil {
			failedKeys = append(failedKeys, req.LockKey)
			s.logger.WithError(err).WithField("lock_key", req.LockKey).Warn("Failed to force unlock")
		} else {
			unlockedKeys = append(unlockedKeys, req.LockKey)
		}
	} else if req.StaleOnly {
		// Unlock all stale locks
		cleaned, err := s.storage.CleanupStaleLocks()
		if err != nil {
			s.writeErrorResponse(w, "Failed to cleanup stale locks", http.StatusInternalServerError, err.Error())
			return
		}
		unlockedKeys = cleaned
	} else if req.All {
		// Unlock ALL locks
		locks, err := s.storage.ListLocks()
		if err != nil {
			s.writeErrorResponse(w, "Failed to list locks", http.StatusInternalServerError, err.Error())
			return
		}

		for _, lock := range locks {
			if err := s.storage.ForceReleaseLock(lock.LockKey); err != nil {
				failedKeys = append(failedKeys, lock.LockKey)
				s.logger.WithError(err).WithField("lock_key", lock.LockKey).Warn("Failed to force unlock")
			} else {
				unlockedKeys = append(unlockedKeys, lock.LockKey)
			}
		}
	}

	response := types.ForceUnlockResponse{
		Success:       len(failedKeys) == 0,
		UnlockedKeys:  unlockedKeys,
		FailedKeys:    failedKeys,
		UnlockedCount: len(unlockedKeys),
		Message:       fmt.Sprintf("Unlocked %d lock(s)", len(unlockedKeys)),
	}

	if len(failedKeys) > 0 {
		response.Message = fmt.Sprintf("Unlocked %d lock(s), failed to unlock %d lock(s)", len(unlockedKeys), len(failedKeys))
	}

	s.writeJSONResponse(w, response)
}

// handleListActiveJobs handles GET/POST /json/active-jobs - list active jobs for an owner
func (s *Server) handleListActiveJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var owner string
	if r.Method == http.MethodPost {
		var req types.ListActiveJobsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeErrorResponse(w, "Invalid request body", http.StatusBadRequest, err.Error())
			return
		}
		owner = req.Owner
	} else {
		owner = r.URL.Query().Get("owner")
	}

	if owner == "" {
		s.writeErrorResponse(w, "owner parameter is required", http.StatusBadRequest, "")
		return
	}

	activeJobs, err := s.storage.GetActiveJobsForOwner(owner)
	if err != nil {
		s.writeErrorResponse(w, "Failed to get active jobs", http.StatusInternalServerError, err.Error())
		return
	}

	response := types.ListActiveJobsResponse{
		Owner:       owner,
		ActiveJobs:  activeJobs,
		ActiveCount: len(activeJobs),
		Limit:       s.config.Build.MaxConcurrentBuildsPerOwner,
	}

	s.writeJSONResponse(w, response)
}
