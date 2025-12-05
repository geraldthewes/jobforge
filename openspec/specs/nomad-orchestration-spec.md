# Nomad Job Orchestration Specification

## Overview

The Nomad Build Service orchestrates build, test, and publish operations using Nomad batch jobs. Each phase of the workflow is represented as a separate Nomad job.

## Build Phase

### Job Template
- **Image**: `quay.io/buildah/stable`
- **Execution**: 
  1. Clone Git repository using provided credentials
  2. Build Docker image using `buildah bud`
  3. Push temporary image to registry as `<registry>/temp/<job-id>:latest`

### Configuration
- **User Namespace Mappings**: `user = "build:10000:65536"`
- **Fuse Device Access**: `device "/dev/fuse"`
- **Storage Configuration**: Mount persistent volume to `/var/lib/containers`
- **Isolation Mode**: `BUILDAH_ISOLATION=chroot`
- **Privilege Escalation**: `allow_privilege_escalation = true` for rootless mode

### Build Caching
- **Host Volume**: `/opt/nomad/data/buildah-cache:/var/lib/containers`
- **Purpose**: Enable Buildah layer caching for accelerated builds

## Test Phase

### Architecture
- **Driver**: Nomad's native Docker driver
- **Execution**: Native Docker containers for test execution
- **Parallelization**: Multiple test commands run as separate Nomad jobs

### Modes
#### Custom Commands Mode
- For each test command in `test_commands`:
  - Creates separate Nomad batch job with Docker driver
  - Job config: `{"image": "<temp-image>", "command": "sh", "args": ["-c", "<test-command>"]}`
  - Runs test command inside built image

#### Entry Point Mode
- If `test_entry_point` is true:
  - Creates single Nomad batch job with Docker driver
  - Job config: `{"image": "<temp-image>"}` (no command - uses image's ENTRYPOINT/CMD)
  - Tests image's default execution behavior

### Benefits
- Eliminates buildah complexity in test phase
- Leverages Nomad's native Docker driver and log collection
- Supports parallel test execution
- Better resource isolation per test
- Cleaner, native log collection

## Publish Phase

### Job Template
- **Image**: `quay.io/buildah/stable`
- **Execution**:
  1. Pull temporary image from registry
  2. Retag image for each specified tag in `image_tags`
  3. Push final images to target registry using `buildah push`

## Job Lifecycle Management

### Atomicity
- Build-test-publish workflow treated as single atomic operation
- If any phase fails, entire job marked as failed
- Agent must resubmit entire job on failure

### Cleanup
- Nomad batch jobs configured with garbage collection policy
- Temporary images automatically cleaned up
- Service provides `cleanup` endpoint for manual cleanup

### Zombie Job Management
- On service startup, query Nomad for orphaned jobs
- Provide `cleanupZombies` endpoint to terminate abandoned jobs
- Automatic cleanup of jobs running longer than configured timeout

## Resource Management

### Default Resource Limits
- CPU/Memory limits configurable via Consul KV
- Build timeout (default: 30 minutes)
- Test timeout (default: 15 minutes)

### Configuration
- Service configuration via Consul KV store at `jobforge-service/config/`
- Secrets management via Vault at paths like `nomad/jobs/<service>-secrets`
- Environment injection via consul-template pattern