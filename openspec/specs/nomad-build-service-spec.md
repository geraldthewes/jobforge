# Nomad Build Service Specification

## Overview

The Nomad Build Service is a lightweight, stateless server written in Golang that enables coding agents to submit Docker image build jobs remotely. It orchestrates builds, tests, and publishes using Nomad as the backend infrastructure, ensuring all operations (server, builds, tests) run as Nomad jobs.

## Key Features

- API server for job submission, status queries, and log retrieval
- Nomad job orchestration for repo cloning, image building (via Buildah), testing, and publishing
- Support for network access during test phase
- Robust error handling with phase-specific logs accessible via API
- Secure credential handling using Nomad Vault variables
- Buildah layer caching for accelerated builds

## Target Audience

- **AI Coding Agents**: Primary users interacting via API for secure, contextual, and automated build-test-deploy workflows
- **Developers**: Individuals building containerized applications who can leverage the service for standardized builds

## Business Goals

- Enable seamless, remote Docker image builds for agents without requiring local high-end compute resources
- Provide detailed, accessible logs to allow for autonomous self-correction by agents upon build or test failure
- Ensure integration with Nomad for orchestrated workloads
- Minimize dependencies by using daemonless build tools (Buildah)

## Core Functional Requirements

### Job Submission (FR1)
- Agent commits changes to a new build branch and publishes to Git repo
- Server validates inputs and generates a unique job ID
- Secrets handling via Nomad Vault references
- Git authentication via SSH keys and Personal Access Tokens

### Build Phase (FR2)
- Nomad batch job using `quay.io/buildah/stable` image
- Clone Git repository and execute `buildah bud`
- Build caching via persistent host volume
- Temporary image tagging for test phase

### Test Phase (FR3)
- Runs only if test is configured in job
- Separate Nomad batch jobs for each test command
- Native Docker container execution via Nomad's Docker driver
- Host networking mode for network access during tests
- Parallelization of multiple test commands

### Publish Phase (FR4)
- Runs only if test specified and succeeded
- Final Nomad batch job pushing image to registry
- Authentication handled by Nomad Vault injection

### Logging and Monitoring (FR5)
- Real-time status updates via polling `getStatus` endpoint
- Accessible logs for each phase via `getLogs` endpoint
- Raw Buildah output for error diagnosis
- Actionable error reporting

### Graceful Job Termination (FR6)
- `killJob` endpoint for safe job termination
- Prevents corruption during Docker and registry operations
- Graceful termination for all phases

### Query and Streaming Endpoints (FR7)
- `submitJob`, `getStatus`, `getLogs`, `streamLogs`, `killJob`, `cleanup`, `health`, `ready` endpoints

### Intermediate Image Handling (FR8)
- Private Docker registry for sharing images between phases
- Branch-based isolation for concurrent builds
- Distributed locking for registry protection

## Non-Functional Requirements

### Performance
- Optimized build times via layer caching
- Concurrent build submission handling

### Security
- Rootless Buildah execution
- Exclusive Nomad Vault secret management

### Reliability
- Atomic build-test-publish workflow
- Automatic cleanup of temporary artifacts
- Zombie job management

### Usability
- Simple JSON API with clear schemas
- Detailed error messages for agent debugging

### Scalability
- Stateless server deployment
- Horizontal scaling via Nomad replication

### Compatibility
- Go 1.22+
- Nomad 1.10+
- Buildah latest stable version

## Technical Stack

- Language: Golang
- Libraries: Nomad API, Consul API, Vault API, Prometheus client, Logrus
- Deployment: Dockerized application running as Nomad service job
- Testing: Unit tests for API handlers, integration tests with mocked Nomad API

## Architecture

### Components
- API Server: Stateless Go application acting as control plane
- Nomad Client: Communication with Nomad cluster API
- Build/Test/Push Jobs: Ephemeral Nomad batch jobs

### Data Flow
1. Agent submits `submitJob` request
2. Server validates and submits "build" job to Nomad
3. Upon successful build, submits "test" jobs (one per test command)
4. Upon successful test completion, submits "publish" job
5. Agent polls status or receives real-time updates and requests logs

### Job Atomicity
Build-test-publish workflow treated as single atomic operation. If any phase fails, entire job marked as failed.