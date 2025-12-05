# Project Context

## Purpose
The Nomad Build Service is a lightweight, stateless server written in Golang that enables coding agents to submit Docker image build jobs remotely. It orchestrates builds, tests, and publishes using Nomad as the backend infrastructure, ensuring all operations (server, builds, tests) run as Nomad jobs.

## Tech Stack
- Language: Go 1.22+
- Framework: Standard Go HTTP server
- Orchestration: HashiCorp Nomad
- Container Building: Buildah (daemonless)
- Configuration: Consul KV, Vault
- Metrics: Prometheus
- Logging: Logrus

## Project Conventions

### Code Style
- Follow Go best practices and idioms
- Use clear, descriptive variable and function names
- Maintain consistent indentation and formatting
- Include comprehensive documentation for public APIs

### Architecture Patterns
- Stateless server design
- Microservices-oriented architecture
- Clear separation of concerns between API, orchestration, and logging
- Event-driven approach for job lifecycle management

### Testing Strategy
- Unit tests for API handlers and business logic
- Integration tests with mocked Nomad API
- End-to-end tests for complete workflow validation
- Performance testing for concurrent job handling

### Git Workflow
- Feature branches for development
- Pull requests for code review
- Semantic versioning for releases
- Commit messages following conventional format

## Domain Context
The service is designed for agentic code development, offloading resource-intensive builds to remote Nomad clusters while empowering agents to debug failures autonomously through accessible logs. It implements graceful job termination to prevent corruption during Docker and registry operations.

## Important Constraints
- Never use direct Nomad job commands (`nomad job stop`, `nomad job deregister`) to terminate build service jobs
- Always use the service's `killJob` endpoint for graceful termination
- All secrets must be managed exclusively through Nomad's Vault integration
- Buildah must be run in rootless mode to minimize container privileges

## External Dependencies
- HashiCorp Nomad (1.10+)
- Buildah (latest stable version)
- HashiCorp Consul (for service discovery and configuration)
- HashiCorp Vault (for secret management)
- Prometheus (for metrics collection)