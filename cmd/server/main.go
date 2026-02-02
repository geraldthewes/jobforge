package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"nomad-mcp-builder/internal/config"
	"nomad-mcp-builder/internal/metrics"
	"nomad-mcp-builder/internal/nomad"
	"nomad-mcp-builder/internal/server"
	"nomad-mcp-builder/internal/storage"
)

func main() {
	logger := logrus.New()
	logger.SetLevel(logrus.InfoLevel)
	
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		logger.WithError(err).Fatal("Failed to load configuration")
	}
	
	// Set log level from configuration
	level, err := logrus.ParseLevel(cfg.Logging.Level)
	if err != nil {
		logger.WithField("log_level", cfg.Logging.Level).Warn("Invalid log level, using info")
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	
	// Validate configuration
	if err := cfg.Validate(); err != nil {
		logger.WithError(err).Fatal("Invalid configuration")
	}
	
	logger.WithFields(logrus.Fields{
		"nomad_address":   cfg.Nomad.Address,
		"consul_address":  cfg.Consul.Address,
		"vault_address":   cfg.Vault.Address,
		"server_port":     cfg.Server.Port,
		"metrics_enabled": cfg.Monitoring.Enabled,
		"log_level":       level.String(),
	}).Info("Starting Nomad Build Service")
	
	// Initialize storage (Consul)
	consulStorage, err := storage.NewConsulStorage(
		cfg.Consul.Address,
		cfg.Consul.Token,
		cfg.Consul.Datacenter,
		cfg.Consul.KeyPrefix,
	)
	if err != nil {
		logger.WithError(err).Fatal("Failed to initialize Consul storage")
	}
	
	// Test Consul connectivity
	if err := consulStorage.Health(); err != nil {
		logger.WithError(err).Fatal("Failed to connect to Consul")
	}
	logger.Info("Connected to Consul successfully")
	
	// Initialize Nomad client
	nomadClient, err := nomad.NewClient(cfg, consulStorage)
	if err != nil {
		logger.WithError(err).Fatal("Failed to initialize Nomad client")
	}
	
	// Test Nomad connectivity
	if err := nomadClient.Health(); err != nil {
		logger.WithError(err).Fatal("Failed to connect to Nomad")
	}
	logger.Info("Connected to Nomad successfully")
	
	// Initialize metrics if enabled
	var metricsServer *metrics.Metrics
	if cfg.Monitoring.Enabled {
		metricsServer = metrics.NewMetrics()
		logger.WithField("metrics_port", cfg.Monitoring.MetricsPort).Info("Metrics collection enabled")
		
		// Start metrics server in background
		go func() {
			if err := metricsServer.StartMetricsServer(cfg.Monitoring.MetricsPort); err != nil {
				logger.WithError(err).Error("Metrics server failed")
			}
		}()
		
		// Start health monitoring
		go startHealthMonitoring(consulStorage, nomadClient, metricsServer, logger)
	}
	
	// Initialize API server with configured logger
	apiServer := server.NewServer(cfg, nomadClient, consulStorage, logger)

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	// Channel to listen for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	// Start servers
	var wg sync.WaitGroup
	
	// Start API server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := apiServer.Start(ctx); err != nil {
			logger.WithError(err).Error("API server failed")
		}
	}()
	
	logger.Info("All services started successfully")
	
	// Wait for shutdown signal
	<-sigChan
	logger.Info("Shutdown signal received, starting graceful shutdown...")
	
	// Cancel context to stop all services
	cancel()
	
	// Wait for all goroutines to finish with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	
	select {
	case <-done:
		logger.Info("Graceful shutdown completed")
	case <-time.After(30 * time.Second):
		logger.Warn("Graceful shutdown timeout, forcing exit")
	}
}

// startHealthMonitoring runs periodic health checks and updates metrics
func startHealthMonitoring(consulStorage *storage.ConsulStorage, nomadClient *nomad.Client, metricsServer *metrics.Metrics, logger *logrus.Logger) {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			services := make(map[string]bool)
			
			// Check Consul health
			if err := consulStorage.Health(); err != nil {
				services["consul"] = false
				logger.WithError(err).Warn("Consul health check failed")
			} else {
				services["consul"] = true
			}
			
			// Check Nomad health
			if err := nomadClient.Health(); err != nil {
				services["nomad"] = false
				logger.WithError(err).Warn("Nomad health check failed")
			} else {
				services["nomad"] = true
			}
			
			// Update health metrics
			metricsServer.UpdateAllHealthChecks(services)
			
			// Log overall health status
			allHealthy := true
			for _, healthy := range services {
				if !healthy {
					allHealthy = false
					break
				}
			}
			
			if allHealthy {
				logger.Debug("All services healthy")
			} else {
				logger.Warn("Some services are unhealthy")
			}
		}
	}
}