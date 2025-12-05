package mcp

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

// Server represents the MCP server
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

	// MCP session management (session ID -> creation time)
	mcpSessions map[string]time.Time
	sessionMutex sync.RWMutex

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

// NewServer creates a new MCP server
func NewServer(cfg *config.Config, nomadClient *nomad.Client, storage *storage.ConsulStorage, logger *logrus.Logger) *Server {
	return &Server{
		config:        cfg,
		nomadClient:   nomadClient,
		storage:       storage,
		logger:        logger,
		wsConnections: make(map[string][]*websocket.Conn),
		jobMutexes:    make(map[string]*sync.Mutex),
		mcpSessions:   make(map[string]time.Time),
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

// Start starts the MCP server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// RESTful API endpoints (both /json and /mcp prefixes for compatibility)
	mux.HandleFunc("/json/submitJob", s.handleSubmitJob)
	mux.HandleFunc("/mcp/submitJob", s.handleSubmitJob)
	mux.HandleFunc("/json/getStatus", s.handleGetStatus)
	mux.HandleFunc("/mcp/getStatus", s.handleGetStatus)
	mux.HandleFunc("/json/getLogs", s.handleGetLogs)
	mux.HandleFunc("/mcp/getLogs", s.handleGetLogs)
	mux.HandleFunc("/json/killJob", s.handleKillJob)
	mux.HandleFunc("/mcp/killJob", s.handleKillJob)
	mux.HandleFunc("/json/cleanup", s.handleCleanup)
	mux.HandleFunc("/mcp/cleanup", s.handleCleanup)
	mux.HandleFunc("/json/getHistory", s.handleGetHistory)
	mux.HandleFunc("/mcp/getHistory", s.handleGetHistory)
	mux.HandleFunc("/json/job/", s.handleJobResource)
	mux.HandleFunc("/mcp/job/", s.handleJobResource)

	// MCP Protocol endpoints
	mux.HandleFunc("/mcp", s.handleMCPRequest)           // JSON-RPC over HTTP

	// Health check endpoints
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", s.config.Server.Host, s.config.Server.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	s.logger.WithField("address", server.Addr).Info("Starting MCP server")

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
	
	response := types.GetStatusResponse{
		JobID:   job.ID,
		Status:  job.Status,
		Config:  &job.Config, // Include config for debugging
		Metrics: job.Metrics,
		Error:   job.Error,
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
	
	// Get latest logs from Nomad
	logs, err := s.nomadClient.GetJobLogs(job)
	if err != nil {
		s.logger.WithError(err).Warn("Failed to get logs from Nomad")
		// Return cached logs
		logs = job.Logs
	} else {
		// Update job with latest logs
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
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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

// MCP Protocol Handlers

// handleMCPRequest processes standard MCP JSON-RPC requests
func (s *Server) handleMCPRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Log incoming request in web server format
	s.logger.WithFields(map[string]interface{}{
		"method":         r.Method,
		"uri":            r.RequestURI,
		"remote_addr":    r.RemoteAddr,
		"user_agent":     r.UserAgent(),
		"content_length": r.ContentLength,
		"content_type":   r.Header.Get("Content-Type"),
	}).Info("MCP request received")

	// Handle CORS preflight
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", s.config.Server.CORSOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, mcp-protocol-version")
		w.Header().Set("Access-Control-Max-Age", "3600")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Set CORS headers for actual request
	w.Header().Set("Access-Control-Allow-Origin", s.config.Server.CORSOrigin)

	if r.Method != http.MethodPost {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":       r.Method,
			"uri":          r.RequestURI,
			"remote_addr":  r.RemoteAddr,
			"status":       http.StatusMethodNotAllowed,
			"duration_ms":  duration.Milliseconds(),
			"error":        "Method not allowed",
		}).Info("MCP request completed")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read body for potential verbose logging
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":       r.Method,
			"uri":          r.RequestURI,
			"remote_addr":  r.RemoteAddr,
			"status":       http.StatusBadRequest,
			"duration_ms":  duration.Milliseconds(),
			"error":        "Failed to read body: " + err.Error(),
		}).Info("MCP request completed")
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var mcpReq MCPRequest
	if err := json.Unmarshal(bodyBytes, &mcpReq); err != nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":       r.Method,
			"uri":          r.RequestURI,
			"remote_addr":  r.RemoteAddr,
			"status":       http.StatusBadRequest,
			"duration_ms":  duration.Milliseconds(),
			"error":        "Parse error: " + err.Error(),
		}).Info("MCP request completed")
		response := NewMCPErrorResponse(nil, MCPErrorParseError, "Parse error", err.Error())
		s.writeMCPResponse(w, response)
		return
	}

	// Log the actual MCP method being called
	s.logger.WithFields(map[string]interface{}{
		"mcp_method":   mcpReq.Method,
		"mcp_id":       mcpReq.ID,
		"remote_addr":  r.RemoteAddr,
	}).Info("MCP method call")

	// Verbose logging: log full request and extract tool name for tools/call
	if s.config.Logging.LogLevel >= 1 {
		logFields := map[string]interface{}{
			"raw_request": string(bodyBytes),
			"mcp_method":  mcpReq.Method,
		}

		// Extract tool name if this is a tools/call
		if mcpReq.Method == "tools/call" {
			if params, ok := mcpReq.Params.(map[string]interface{}); ok {
				if toolName, ok := params["name"].(string); ok {
					logFields["tool_name"] = toolName
				}
			}
		}

		s.logger.WithFields(logFields).Info("MCP request detail (LOG_LEVEL=1)")
	}

	// Handle notifications (JSON-RPC requests without id field)
	// Notifications don't expect a response, just acknowledge with 200 OK
	if mcpReq.ID == nil {
		duration := time.Since(startTime)
		s.logger.WithFields(map[string]interface{}{
			"method":       r.Method,
			"uri":          r.RequestURI,
			"remote_addr":  r.RemoteAddr,
			"status":       http.StatusOK,
			"duration_ms":  duration.Milliseconds(),
			"mcp_method":   mcpReq.Method,
			"mcp_id":       mcpReq.ID,
			"notification": true,
		}).Info("MCP notification received")
		w.WriteHeader(http.StatusOK)
		return
	}

	var response MCPResponse
	switch mcpReq.Method {
	case "tools/list":
		response = s.handleMCPToolsList(mcpReq)
	case "tools/call":
		response = s.handleMCPToolsCall(mcpReq)
	case "initialize":
		response = s.handleMCPInitialize(mcpReq)
	case "notifications/initialized":
		// Client signals initialization complete - acknowledge with empty result
		response = NewMCPResponse(mcpReq.ID, map[string]interface{}{})
	default:
		response = NewMCPErrorResponse(mcpReq.ID, MCPErrorMethodNotFound, "Method not found", mcpReq.Method)
	}

	duration := time.Since(startTime)
	statusCode := http.StatusOK
	if response.Error != nil {
		statusCode = http.StatusBadRequest
	}

	s.logger.WithFields(map[string]interface{}{
		"method":       r.Method,
		"uri":          r.RequestURI,
		"remote_addr":  r.RemoteAddr,
		"status":       statusCode,
		"duration_ms":  duration.Milliseconds(),
		"mcp_method":   mcpReq.Method,
		"mcp_id":       mcpReq.ID,
		"mcp_success":  response.Error == nil,
	}).Info("MCP request completed")

	// Verbose logging: log full response
	if s.config.Logging.LogLevel >= 1 {
		if responseJSON, err := json.Marshal(response); err == nil {
			s.logger.WithFields(map[string]interface{}{
				"raw_response": string(responseJSON),
				"mcp_method":   mcpReq.Method,
				"mcp_id":       mcpReq.ID,
			}).Info("MCP response detail (LOG_LEVEL=1)")
		}
	}

	s.writeMCPResponse(w, response)
}

// Session management helpers

// generateSessionID creates a new unique session ID
func (s *Server) generateSessionID() string {
	// Use timestamp + random component for uniqueness
	return fmt.Sprintf("mcp-session-%d-%d", time.Now().UnixNano(), time.Now().Unix()%10000)
}

// getOrCreateSession returns existing session ID from header, or creates new one
func (s *Server) getOrCreateSession(r *http.Request) string {
	// Check if client sent session ID
	if sessionID := r.Header.Get("Mcp-Session-Id"); sessionID != "" {
		s.sessionMutex.RLock()
		_, exists := s.mcpSessions[sessionID]
		s.sessionMutex.RUnlock()

		if exists {
			return sessionID
		}
	}

	// Create new session
	sessionID := s.generateSessionID()
	s.sessionMutex.Lock()
	s.mcpSessions[sessionID] = time.Now()
	s.sessionMutex.Unlock()

	s.logger.WithField("session_id", sessionID).Debug("Created new MCP session")
	return sessionID
}


// handleMCPInitialize handles MCP initialization
func (s *Server) handleMCPInitialize(req MCPRequest) MCPResponse {
	// Parse client's requested protocol version
	var clientVersion string
	if params, ok := req.Params.(map[string]interface{}); ok {
		if version, ok := params["protocolVersion"].(string); ok {
			clientVersion = version
			s.logger.WithFields(map[string]interface{}{
				"client_version": clientVersion,
			}).Info("Client requested MCP protocol version")
		}
	}

	// Respond with our supported version (2025-06-18)
	result := map[string]interface{}{
		"protocolVersion": "2025-06-18",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "nomad-build-service",
			"version": "2.0.0",
		},
	}
	return NewMCPResponse(req.ID, result)
}

// handleMCPPing handles ping requests for keep-alive
func (s *Server) handleMCPPing(req MCPRequest) MCPResponse {
	result := map[string]interface{}{
		"status": "pong",
	}
	return NewMCPResponse(req.ID, result)
}

// handleMCPToolsList handles tools/list requests
func (s *Server) handleMCPToolsList(req MCPRequest) MCPResponse {
	tools := GetTools()
	result := ToolListResult{Tools: tools}
	return NewMCPResponse(req.ID, result)
}

// handleMCPToolsCall handles tools/call requests by translating to internal API
func (s *Server) handleMCPToolsCall(req MCPRequest) MCPResponse {
	params, ok := req.Params.(map[string]interface{})
	if !ok {
		return NewMCPErrorResponse(req.ID, MCPErrorInvalidParams, "Invalid params", nil)
	}

	toolName, ok := params["name"].(string)
	if !ok {
		return NewMCPErrorResponse(req.ID, MCPErrorInvalidParams, "Tool name required", nil)
	}

	arguments, ok := params["arguments"].(map[string]interface{})
	if !ok {
		arguments = make(map[string]interface{})
	}

	// Log the tool call with arguments for debugging
	s.logger.WithFields(map[string]interface{}{
		"tool_name": toolName,
		"arguments": arguments,
	}).Debug("MCP tool call received")

	var response MCPResponse
	switch toolName {
	case "submitJob":
		response = s.mcpSubmitJob(req.ID, arguments)
	case "getStatus":
		response = s.mcpGetStatus(req.ID, arguments)
	case "getLogs":
		response = s.mcpGetLogs(req.ID, arguments)
	case "killJob":
		response = s.mcpKillJob(req.ID, arguments)
	case "cleanup":
		response = s.mcpCleanup(req.ID, arguments)
	case "getHistory":
		response = s.mcpGetHistory(req.ID, arguments)
	case "purgeFailedJob":
		response = s.mcpPurgeFailedJob(req.ID, arguments)
	default:
		response = NewMCPErrorResponse(req.ID, MCPErrorMethodNotFound, "Tool not found", toolName)
	}

	// Log the response for debugging
	if response.Error != nil {
		s.logger.WithFields(map[string]interface{}{
			"tool_name": toolName,
			"error":     response.Error,
		}).Warn("MCP tool call failed")
	} else {
		s.logger.WithFields(map[string]interface{}{
			"tool_name": toolName,
			"result":    response.Result,
		}).Debug("MCP tool call succeeded")
	}

	return response
}

// MCP Tool implementations (translate to existing internal methods)

func (s *Server) mcpSubmitJob(id interface{}, args map[string]interface{}) MCPResponse {
	// Convert MCP arguments to internal job config
	var jobConfig types.JobConfig

	if owner, ok := args["owner"].(string); ok {
		jobConfig.Owner = owner
	}
	if repoURL, ok := args["repo_url"].(string); ok {
		jobConfig.RepoURL = repoURL
	}
	if gitRef, ok := args["git_ref"].(string); ok {
		jobConfig.GitRef = gitRef
	} else {
		jobConfig.GitRef = "main"
	}
	if gitCreds, ok := args["git_credentials_path"].(string); ok {
		jobConfig.GitCredentialsPath = gitCreds
	} else {
		jobConfig.GitCredentialsPath = "secret/nomad/jobs/git-credentials"
	}
	if dockerfile, ok := args["dockerfile_path"].(string); ok {
		jobConfig.DockerfilePath = dockerfile
	} else {
		jobConfig.DockerfilePath = "Dockerfile"
	}
	if imageName, ok := args["image_name"].(string); ok {
		jobConfig.ImageName = imageName
	}
	if regURL, ok := args["registry_url"].(string); ok {
		jobConfig.RegistryURL = regURL
	}
	if regCreds, ok := args["registry_credentials_path"].(string); ok {
		jobConfig.RegistryCredentialsPath = regCreds
	} else {
		jobConfig.RegistryCredentialsPath = "secret/nomad/jobs/registry-credentials"
	}

	// Convert image tags - handle multiple formats for compatibility
	if tagsValue, exists := args["image_tags"]; exists {
		// Try as array first (proper format)
		if tagsArray, ok := tagsValue.([]interface{}); ok {
			for _, tag := range tagsArray {
				if tagStr, ok := tag.(string); ok {
					jobConfig.ImageTags = append(jobConfig.ImageTags, tagStr)
				}
			}
		} else if tagsString, ok := tagsValue.(string); ok {
			// Handle string formats
			if strings.HasPrefix(tagsString, "[") {
				// Parse JSON array string like "[\"latest\", \"v1.0\"]"
				var parsedTags []string
				if err := json.Unmarshal([]byte(tagsString), &parsedTags); err == nil {
					jobConfig.ImageTags = parsedTags
				} else {
					s.logger.WithError(err).Warn("Failed to parse image_tags JSON string")
				}
			} else {
				// Treat as single tag
				jobConfig.ImageTags = []string{tagsString}
			}
		}
	}

	// Parse test configuration
	if testInterface, ok := args["test"]; ok {
		if testMap, ok := testInterface.(map[string]interface{}); ok {
			testConfig := &types.TestConfig{}

			// Parse test commands
			if cmdsInterface, ok := testMap["commands"].([]interface{}); ok {
				for _, cmd := range cmdsInterface {
					if cmdStr, ok := cmd.(string); ok {
						testConfig.Commands = append(testConfig.Commands, cmdStr)
					}
				}
			}

			// Parse test entry point
			if entryPoint, ok := testMap["entry_point"].(bool); ok {
				testConfig.EntryPoint = entryPoint
			}

			// Parse test environment variables
			if envInterface, ok := testMap["env"].(map[string]interface{}); ok {
				testConfig.Env = make(map[string]string)
				for key, value := range envInterface {
					if valueStr, ok := value.(string); ok {
						testConfig.Env[key] = valueStr
					}
				}
			}

			// Parse test resource limits
			if limitsInterface, ok := testMap["resource_limits"]; ok {
				if limitsMap, ok := limitsInterface.(map[string]interface{}); ok {
					limits := &types.PhaseResourceLimits{}
					if cpu, ok := limitsMap["cpu"].(string); ok {
						limits.CPU = cpu
					}
					if memory, ok := limitsMap["memory"].(string); ok {
						limits.Memory = memory
					}
					if disk, ok := limitsMap["disk"].(string); ok {
						limits.Disk = disk
					}
					testConfig.ResourceLimits = limits
				}
			}

			// Parse vault secrets
			if vaultSecretsInterface, ok := testMap["vault_secrets"].([]interface{}); ok {
				for _, secretInterface := range vaultSecretsInterface {
					if secretMap, ok := secretInterface.(map[string]interface{}); ok {
						vaultSecret := types.VaultSecret{}
						if path, ok := secretMap["path"].(string); ok {
							vaultSecret.Path = path
						}
						if fieldsInterface, ok := secretMap["fields"].(map[string]interface{}); ok {
							vaultSecret.Fields = make(map[string]string)
							for key, value := range fieldsInterface {
								if valueStr, ok := value.(string); ok {
									vaultSecret.Fields[key] = valueStr
								}
							}
						}
						testConfig.VaultSecrets = append(testConfig.VaultSecrets, vaultSecret)
					}
				}
			}

			// Parse vault policies
			if vaultPoliciesInterface, ok := testMap["vault_policies"].([]interface{}); ok {
				for _, policy := range vaultPoliciesInterface {
					if policyStr, ok := policy.(string); ok {
						testConfig.VaultPolicies = append(testConfig.VaultPolicies, policyStr)
					}
				}
			}

			jobConfig.Test = testConfig
		}
	}

	// Parse resource limits
	if resourceLimitsInterface, ok := args["resource_limits"]; ok {
		resourceLimits := parseResourceLimitsFromMCP(resourceLimitsInterface)
		jobConfig.ResourceLimits = resourceLimits
	}

	// Parse webhook configuration
	if webhookURL, ok := args["webhook_url"].(string); ok {
		jobConfig.WebhookURL = webhookURL
	}
	if webhookSecret, ok := args["webhook_secret"].(string); ok {
		jobConfig.WebhookSecret = webhookSecret
	}
	if webhookOnSuccess, ok := args["webhook_on_success"].(bool); ok {
		jobConfig.WebhookOnSuccess = webhookOnSuccess
	}
	if webhookOnFailure, ok := args["webhook_on_failure"].(bool); ok {
		jobConfig.WebhookOnFailure = webhookOnFailure
	}

	// Parse webhook headers
	if headersInterface, ok := args["webhook_headers"].(map[string]interface{}); ok {
		jobConfig.WebhookHeaders = make(map[string]string)
		for key, value := range headersInterface {
			if valueStr, ok := value.(string); ok {
				jobConfig.WebhookHeaders[key] = valueStr
			}
		}
	}

	// Validate job config using the same validation as web interface
	if err := validateJobConfig(&jobConfig); err != nil {
		return NewMCPErrorResponse(id, MCPErrorInvalidParams, "Job configuration validation failed", err.Error())
	}

	// Create job using existing logic
	job, err := s.nomadClient.CreateJob(&jobConfig)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Failed to create job", err.Error())
	}

	// Store job
	if err := s.storage.StoreJob(job); err != nil {
		s.logger.WithError(err).Warn("Failed to store job in storage")
	}

	result := ToolCallResult{
		Content: NewMCPJSONContent(map[string]interface{}{
			"job_id": job.ID,
			"status": job.Status,
		}),
	}
	return NewMCPResponse(id, result)
}

func (s *Server) mcpGetStatus(id interface{}, args map[string]interface{}) MCPResponse {
	jobID, ok := args["job_id"].(string)
	if !ok {
		return NewMCPErrorResponse(id, MCPErrorInvalidParams, "job_id required", nil)
	}

	job, err := s.storage.GetJob(jobID)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Job not found", err.Error())
	}

	// Update status from Nomad
	if updatedJob, err := s.nomadClient.UpdateJobStatus(job); err == nil {
		job = updatedJob
		s.storage.UpdateJob(job) // Update storage
	}

	result := ToolCallResult{
		Content: NewMCPJSONContent(map[string]interface{}{
			"job_id": job.ID,
			"status": job.Status,
			"error":  job.Error,
			"phase":  job.FailedPhase,
		}),
	}
	return NewMCPResponse(id, result)
}

func (s *Server) mcpGetLogs(id interface{}, args map[string]interface{}) MCPResponse {
	jobID, ok := args["job_id"].(string)
	if !ok {
		return NewMCPErrorResponse(id, MCPErrorInvalidParams, "job_id required", nil)
	}

	job, err := s.storage.GetJob(jobID)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Job not found", err.Error())
	}

	logs, err := s.nomadClient.GetJobLogs(job)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Failed to get logs", err.Error())
	}

	phase, _ := args["phase"].(string)
	var result interface{}
	
	switch phase {
	case "build":
		result = logs.Build
	case "test":
		result = logs.Test
	case "publish":
		result = logs.Publish
	default:
		result = logs
	}

	toolResult := ToolCallResult{
		Content: NewMCPJSONContent(result),
	}
	return NewMCPResponse(id, toolResult)
}

func (s *Server) mcpKillJob(id interface{}, args map[string]interface{}) MCPResponse {
	jobID, ok := args["job_id"].(string)
	if !ok {
		return NewMCPErrorResponse(id, MCPErrorInvalidParams, "job_id required", nil)
	}

	job, err := s.storage.GetJob(jobID)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Job not found", err.Error())
	}

	if err := s.nomadClient.KillJob(job); err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Failed to kill job", err.Error())
	}

	// Update job status
	job.Status = types.StatusFailed
	job.Error = "Job killed by user"
	s.storage.UpdateJob(job)

	result := ToolCallResult{
		Content: NewMCPTextContent("Job terminated successfully"),
	}
	return NewMCPResponse(id, result)
}

func (s *Server) mcpCleanup(id interface{}, args map[string]interface{}) MCPResponse {
	jobID, ok := args["job_id"].(string)
	if !ok {
		return NewMCPErrorResponse(id, MCPErrorInvalidParams, "job_id required", nil)
	}

	job, err := s.storage.GetJob(jobID)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Job not found", err.Error())
	}

	if err := s.nomadClient.CleanupJob(job); err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Failed to cleanup job", err.Error())
	}

	result := ToolCallResult{
		Content: NewMCPTextContent("Job resources cleaned up successfully"),
	}
	return NewMCPResponse(id, result)
}

func (s *Server) mcpGetHistory(id interface{}, args map[string]interface{}) MCPResponse {
	limit := 10
	if limitFloat, ok := args["limit"].(float64); ok {
		limit = int(limitFloat)
	}
	
	offset := 0 // Default offset for MCP interface

	history, total, err := s.storage.GetJobHistory(limit, offset)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Failed to get history", err.Error())
	}
	
	// Filter by owner if specified (post-filtering for simplicity)
	if ownerFilter, ok := args["owner"].(string); ok && ownerFilter != "" {
		var filteredHistory []types.JobHistory
		for _, job := range history {
			if job.Config.Owner == ownerFilter {
				filteredHistory = append(filteredHistory, job)
			}
		}
		history = filteredHistory
	}

	result := ToolCallResult{
		Content: NewMCPJSONContent(map[string]interface{}{
			"jobs":  history,
			"total": total,
		}),
	}
	return NewMCPResponse(id, result)
}

func (s *Server) mcpPurgeFailedJob(id interface{}, args map[string]interface{}) MCPResponse {
	jobID, ok := args["job_id"].(string)
	if !ok {
		return NewMCPErrorResponse(id, MCPErrorInvalidParams, "job_id required", nil)
	}

	job, err := s.storage.GetJob(jobID)
	if err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Job not found", err.Error())
	}

	// Use the new CleanupFailedJobs method to purge from Nomad
	if err := s.nomadClient.CleanupFailedJobs(job); err != nil {
		return NewMCPErrorResponse(id, MCPErrorInternalError, "Failed to purge job from Nomad", err.Error())
	}

	result := ToolCallResult{
		Content: NewMCPTextContent(fmt.Sprintf("Job %s purged successfully from Nomad", jobID)),
	}
	return NewMCPResponse(id, result)
}

// Helper method to write MCP responses
func (s *Server) writeMCPResponse(w http.ResponseWriter, response MCPResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.WithError(err).Error("Failed to encode MCP response")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
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

// parseResourceLimitsFromMCP converts the MCP resource_limits argument to internal types
func parseResourceLimitsFromMCP(limitsInterface interface{}) *types.ResourceLimits {
	limitsMap, ok := limitsInterface.(map[string]interface{})
	if !ok {
		return nil
	}

	resourceLimits := &types.ResourceLimits{}

	// Parse legacy global limits
	if cpu, ok := limitsMap["cpu"].(string); ok {
		resourceLimits.CPU = cpu
	}
	if memory, ok := limitsMap["memory"].(string); ok {
		resourceLimits.Memory = memory
	}
	if disk, ok := limitsMap["disk"].(string); ok {
		resourceLimits.Disk = disk
	}

	// Parse per-phase limits
	if buildInterface, ok := limitsMap["build"]; ok {
		if buildMap, ok := buildInterface.(map[string]interface{}); ok {
			build := &types.PhaseResourceLimits{}
			if cpu, ok := buildMap["cpu"].(string); ok {
				build.CPU = cpu
			}
			if memory, ok := buildMap["memory"].(string); ok {
				build.Memory = memory
			}
			if disk, ok := buildMap["disk"].(string); ok {
				build.Disk = disk
			}
			resourceLimits.Build = build
		}
	}

	if testInterface, ok := limitsMap["test"]; ok {
		if testMap, ok := testInterface.(map[string]interface{}); ok {
			test := &types.PhaseResourceLimits{}
			if cpu, ok := testMap["cpu"].(string); ok {
				test.CPU = cpu
			}
			if memory, ok := testMap["memory"].(string); ok {
				test.Memory = memory
			}
			if disk, ok := testMap["disk"].(string); ok {
				test.Disk = disk
			}
			resourceLimits.Test = test
		}
	}

	if publishInterface, ok := limitsMap["publish"]; ok {
		if publishMap, ok := publishInterface.(map[string]interface{}); ok {
			publish := &types.PhaseResourceLimits{}
			if cpu, ok := publishMap["cpu"].(string); ok {
				publish.CPU = cpu
			}
			if memory, ok := publishMap["memory"].(string); ok {
				publish.Memory = memory
			}
			if disk, ok := publishMap["disk"].(string); ok {
				publish.Disk = disk
			}
			resourceLimits.Publish = publish
		}
	}

	return resourceLimits
}

// handleJobResource handles RESTful job resource endpoints
// Routes: GET /json/job/{jobID}/status, GET /json/job/{jobID}/logs,
//         GET /json/job/{jobID}/test-endpoint, POST /json/job/{jobID}/test-result
func (s *Server) handleJobResource(w http.ResponseWriter, r *http.Request) {
	// Parse URL path: /json/job/{jobID}/{resource} or /mcp/job/{jobID}/{resource}
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/json/job/")
	path = strings.TrimPrefix(path, "/mcp/job/")
	parts := strings.Split(path, "/")

	if len(parts) != 2 {
		http.Error(w, "Invalid path format. Expected: /mcp/job/{jobID}/{status|logs|test-endpoint|test-result}", http.StatusBadRequest)
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

// handleJobStatus handles GET /mcp/job/{jobID}/status
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
	
	response := types.GetStatusResponse{
		JobID:   updatedJob.ID,
		Status:  updatedJob.Status,
		Config:  &updatedJob.Config, // Include config for debugging
		Metrics: updatedJob.Metrics,
		Error:   updatedJob.Error,
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

// handleJobLogs handles GET /mcp/job/{jobID}/logs
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

// handleGetTestEndpoint handles GET /mcp/job/{jobID}/test-endpoint
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
	if job.Config.Test == nil || job.Config.Test.PythonCommand == "" {
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

// handleReportTestResult handles POST /mcp/job/{jobID}/test-result
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
