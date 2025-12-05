<!-- OPENSPEC:START -->
# OpenSpec Instructions

These instructions are for AI assistants working in this project.

Always open `@/openspec/AGENTS.md` when the request:
- Mentions planning or proposals (words like proposal, spec, change, plan)
- Introduces new capabilities, breaking changes, architecture shifts, or big performance/security work
- Sounds ambiguous and you need the authoritative spec before coding

Use `@/openspec/AGENTS.md` to learn:
- How to create and apply change proposals
- Spec format and conventions
- Project structure and guidelines

Keep this managed block so 'openspec update' can refresh the instructions.

<!-- OPENSPEC:END -->

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the Nomad Build Service, consisting of:
1. **MCP Server**: Lightweight, stateless Go application providing JSON-RPC over HTTP interface
2. **CLI Tool**: Command-line client with YAML configuration support and version management

The service enables users and coding agents to submit Docker image build jobs remotely using Nomad as the backend infrastructure. It orchestrates builds, tests, and publishes using Buildah for daemonless image building.

**For complete project requirements and architecture details, see [PRD.md](PRD.md).**

**Current Status:** Fully implemented and operational. The service provides a complete build-test-publish pipeline with improved Docker-native test execution. MCP transport has been simplified to JSON-RPC over HTTP only.

## Architecture (Current Implementation)

The system consists of:

- **MCP Server**: Stateless Go application that translates MCP requests into Nomad API calls
- **Nomad Jobs**: Three phases of ephemeral batch jobs:
  1. **Build phase**: Uses Buildah to build Docker images from Git repos and push to temporary registry location
  2. **Test phase**: Creates separate Nomad jobs using Docker driver directly to run the built image (no buildah complexity)
  3. **Publish phase**: Uses Buildah to retag and push final images to target registries
- **Build Caching**: Persistent host volume for Buildah layer caching at `/opt/nomad/data/buildah-cache`

**Key Innovation**: Test phase now uses Nomad's native Docker driver instead of buildah commands, providing better isolation, cleaner logs, and parallel test execution capability.

## Key Technical Requirements

### Technology Stack (from PRD)
- **Go 1.22+**
- **Nomad 1.10+** with Vault integration
- **Buildah** (latest stable via `quay.io/buildah/stable`)
- **MCP Protocol** for agent communication

### MCP Specification Compliance
- **CRITICAL**: All MCP server implementations and changes MUST conform to the latest Model Context Protocol specification
- **Specification URL**: https://modelcontextprotocol.io/specification/latest
- **Current Protocol Version**: `2025-06-18` (as of latest update)
- **Supported Transport**: JSON-RPC over HTTP only (SSE and Streamable HTTP have been removed for simplicity)
- **Required Compliance Areas**:
  - Initialization sequence (initialize request/response, notifications/initialized)
  - Protocol version negotiation
  - JSON-RPC 2.0 message format
  - Notification vs request handling (ID field presence)
  - Tool definitions and invocation format
  - Error codes and error handling
- When implementing new features or fixing bugs, always verify against the latest spec
- Integration tests should validate spec compliance

### Key Libraries to Use
- `github.com/hashicorp/nomad/api` - Nomad API client
- `github.com/sirupsen/logrus` - Logging
- `gopkg.in/yaml.v3` - YAML configuration parsing
- Standard `net/http` - HTTP handling (WebSocket and SSE removed)

### Security Requirements
- Buildah must run in rootless mode
- All secrets handled via Nomad Vault integration - server never handles raw credentials
- Stateless design for horizontal scaling

### API Validation Requirements
- **CRITICAL**: Both the web interface (`/mcp/submitJob`) and the MCP interface (`tools/call` with `submitJob`) must have identical parameter validation
- Always call `validateJobConfig()` in both interfaces to ensure consistent validation
- Required parameters must match between both interfaces (owner, repo_url, git_ref, dockerfile_path, image_name, image_tags, registry_url)
- This prevents runtime errors like "invalid reference format" when Docker image names are malformed due to missing parameters

## MCP API Endpoints

The service exposes these MCP endpoints via JSON-RPC over HTTP:
- `submitJob`: Submit build request with Git repo, credentials refs, test configuration (commands, entry point, environment variables)
- `getStatus`: Poll job status (`PENDING`, `BUILDING`, `TESTING`, `PUBLISHING`, `SUCCEEDED`, `FAILED`)
- `getLogs`: Retrieve phase-specific logs for debugging
- `killJob`: Terminate running jobs
- `cleanup`: Resource cleanup

## CLI Tool

The `jobforge` CLI tool provides a user-friendly interface to the build service:

**Key Features:**
- **YAML Configuration**: Support for both single-file and split-file (global + per-build) configurations
- **Simplified Image Tagging**: Uses job-id as default tag, or specify custom tags with `--image-tags`
- **Real-time Job Watching**: Watch job progress in real-time using Consul KV (push-based, no polling)
- **Webhook Support**: Service-level webhooks for external integrations (CI/CD, Slack, etc.)

**CLI Commands:**
```bash
# Submit a build job (simple)
jobforge submit-job build.yaml
jobforge submit-job -global deploy/global.yaml build.yaml
jobforge submit-job build.yaml --image-tags "v1.0.0,latest"

# Submit and watch progress in real-time (recommended for interactive use)
jobforge submit-job build.yaml --watch
# Output example:
#   Watching job: abc123def456
#   [12:34:56] 🔨 Status: BUILDING | Phase: build
#   [12:35:42] 🧪 Status: TESTING | Phase: test
#   [12:36:15] 📦 Status: PUBLISHING | Phase: publish
#   ✅ Job completed successfully

# Query job status and logs (polling mode)
jobforge get-status <job-id>
jobforge get-logs <job-id> [phase]

# Job management
jobforge kill-job <job-id>
jobforge cleanup <job-id>
jobforge get-history [limit] [offset]

# Service health
jobforge health
```

**YAML Configuration:**
The CLI supports YAML job configurations with deep merge capability:
- **Global config** (`deploy/global.yaml`): Shared settings across all builds
- **Per-build config** (e.g., `build.yaml`): Build-specific overrides
- Per-build values override global values for any non-zero field

**Test Configuration:**
Test phase settings are grouped under the `test` key:
- `test.commands`: List of test commands to execute
- `test.entry_point`: Test the container's ENTRYPOINT
- `test.env`: Environment variables for test containers (e.g., S3 credentials, API URLs, GPU settings)
- `test.resource_limits`: Per-test resource overrides
- `test.timeout`: Maximum test phase duration

**Job Progress Monitoring:**
Two approaches for monitoring job progress:
1. **Consul KV Watching (CLI)**: Use `--watch` flag for real-time push-based updates via Consul blocking queries
   - Efficient, no polling overhead
   - Displays live status updates with timestamps and emojis
   - Exits automatically when job completes or fails
   - Requires Consul connection (default: localhost:8500)

2. **Webhooks (External Integrations)**: Configure webhooks in YAML for external systems
   - Supports phase-level events (build/test/publish started/completed/failed)
   - HMAC-SHA256 signature authentication
   - Retry logic with 3 attempts
   - Custom headers and success/failure filtering

## Development Workflow

**CRITICAL: Service Discovery**
- The service runs in Nomad, NOT locally
- NEVER use `localhost:8080` for testing
- Always discover the service address using Consul:
  ```bash
  # Get service address and port
  consul catalog services
  consul catalog service jobforge-service
  ```
- Use the discovered address for curl commands, e.g.:
  ```bash
  curl -X POST http://10.0.1.12:31183/mcp/submitJob \
    -H "Content-Type: application/json" \
    -d '{"jobConfig": {...}}'
  ```

**Development Steps:**
1. Build: `make build`
2. Deploy: `REGISTRY_URL=registry.cluster:5000 make nomad-restart`
3. Discover service: `consul catalog service jobforge-service`
4. Test with discovered address: `curl -X POST http://<discovered-ip>:<discovered-port>/mcp/submitJob`

**Do not assume your changes work without testing them immediately!**

## Critical Design Considerations

- **Atomicity**: Build-test-publish is treated as single atomic operation
- **Logging**: Must provide detailed, accessible logs for agent self-correction
- **Networking**: Test phase needs network access for external services
- **Caching**: Implement Buildah layer caching for performance
- **Cleanup**: Automatic garbage collection of jobs and temporary images

## Testing Strategy

Plan for:
- Unit tests for MCP handlers
- Integration tests using mocked Nomad API
- End-to-end test with actual hello-world Docker image build

## Development and Deployment Workflow

### Versioning Strategy

The project uses **semantic versioning** (MAJOR.MINOR.PATCH) with automatic patch incrementing:

- **Patch versions (0.0.X)**: Automatically incremented on each deployment
- **Minor versions (0.X.0)**: Manual increment when you specify
- **Major versions (X.0.0)**: Manual increment when you specify

Please insure each build increases the Patch level


**Version Commands:**
```bash
# Show current version info
make version-info

# Manual version bumps (when requested)
make version-major MAJOR=1          # Creates v1.0.0
make version-minor MINOR_VER=2      # Creates v0.2.0
```

### Making Code Changes and Testing Fixes

When making fixes to the codebase, follow this workflow to build, deploy, and test changes:

1. **Make your code changes**
2. **Build and push the Docker image (auto-increments patch version):**
   ```bash
   REGISTRY_URL=registry.cluster:5000 make docker-push
   ```
   This will:
   - Build the Docker image with the next patch version (e.g., 0.0.5)
   - Push it to `registry.cluster:5000/jobforge-service:0.0.5`
   - Create and push git tag `v0.0.5`
   - Also tag as `latest` for compatibility

3. **Deploy the updated service:**
   ```bash
   make nomad-restart
   ```
   This restarts the Nomad job to pull and run the latest image

4. **Verify the deployment worked:**
   ```bash
   nomad job status jobforge-service
   nomad alloc logs -stderr <alloc-id>  # Check for any startup errors
   ```

### Job Management

**IMPORTANT**: The Nomad job for this service is managed by Terraform in a separate repository, NOT by this codebase.

The service can be restarted to pull new images using:
```bash
make nomad-restart
# or directly: nomad job restart -yes jobforge-service
```

### Testing Your Changes

**CRITICAL**: After deploying via the above workflow, you MUST immediately test that your changes work using the proper testing flow.

#### Required Testing Flow

**ALWAYS follow this exact sequence after making changes:**

1. **Check cluster health and prerequisites:**
   ```bash
   # Verify cluster is healthy
   nomad status                    # Nomad cluster running
   consul members                  # Consul cluster healthy
   vault status                    # Vault unsealed
   ```

2. **Deploy the latest version:**
   ```bash
   # Build and push to cluster registry
   REGISTRY_URL=registry.cluster:5000 make docker-push

   # Restart service to pull latest image
   make nomad-restart
   ```

3. **Run integration tests (PREFERRED METHOD):**
   ```bash
   # Run all tests including integration test
   go test ./...
   ```

   **Expected Results:**
   - All unit tests pass (17 tests)
   - Integration test passes (~15-25 seconds duration)
   - Integration test validates complete build-test-publish pipeline
   - Tests run against the newly deployed service

#### Alternative Manual Testing (Only if integration tests fail)

If integration tests fail and you need to debug manually:

```bash
# Find service URL
curl -s http://10.0.1.12:8500/v1/catalog/service/jobforge-service | jq -r '.[0] | "\(.ServiceAddress):\(.ServicePort)"'

# Submit manual test job
SERVICE_URL=<discovered-url>
curl -X POST http://${SERVICE_URL}/mcp/submitJob \
  -H "Content-Type: application/json" \
  -d '{
    "job_config": {
      "owner": "test",
      "repo_url": "https://github.com/geraldthewes/docker-build-hello-world.git",
      "git_ref": "main",
      "dockerfile_path": "Dockerfile",
      "image_tags": ["hello-world-test"],
      "registry_url": "registry.cluster:5000/helloworld",
      "test": {
        "entry_point": true,
        "env": {
          "NODE_ENV": "test"
        }
      }
    }
  }'
```

#### Monitor Job Progress (Manual Testing Only)

```bash
# Get the job ID from the response and monitor it
nomad job status build-<job-id>
nomad alloc logs -f <alloc-id>  # Follow build progress and check for errors

# Check final status (RESTful endpoint)
curl http://${SERVICE_URL}/mcp/job/<job-id>/status

# Get logs (RESTful endpoint)  
curl http://${SERVICE_URL}/mcp/job/<job-id>/logs
```

**Do not assume your changes work without testing them immediately!**

## Running Tests

**CRITICAL**: After implementing any changes or completing tasks, you MUST run tests to verify functionality.

**IMPORTANT**: Integration tests require the service to be deployed first. Always follow the complete workflow:

1. **Deploy first** (see "Testing Your Changes" section above)
2. **Then run tests** (below)

### Quick Test Command

From the **repository root directory**, run all tests:

```bash
go test ./...
```

This command will:
- Run all unit tests (17 tests)
- Run integration tests (1 end-to-end test against deployed service)
- Verify MCP tool loading from YAML resources
- Test the complete build-test-publish pipeline

### Test Structure

- **Unit Tests**: `test/unit/` - Fast tests for individual components
- **Integration Tests**: `test/integration/` - End-to-end tests requiring running services

### Expected Results

**All Passing Example:**
```
?       nomad-mcp-builder/cmd/server    [no test files]
?       nomad-mcp-builder/internal/config       [no test files]
?       nomad-mcp-builder/internal/mcp  [no test files]
...
ok      nomad-mcp-builder/test/integration      15.138s
ok      nomad-mcp-builder/test/unit     0.014s
```

**Integration Test Success Indicators:**
- ✅ Service discovery via Consul works
- ✅ Complete build-test-publish workflow (15-20s duration)
- ✅ All phases succeed (build → test → publish)
- ✅ Job logs and metrics captured
- ✅ MCP tools load correctly from YAML resources

### When to Run Tests

1. **After implementing new features**
2. **Before committing changes**
3. **When you think all tasks are complete**
4. **After fixing bugs**
5. **When modifying MCP tool definitions**

### Troubleshooting Test Failures

- **"tools directory not found"**: Resource loading path issue (should be auto-resolved)
- **Integration test skips**: Consul/Nomad services not running
- **Build failures**: Check that the service can connect to Nomad cluster

### Verbose Test Output

For detailed test information:
```bash
# Unit tests only
go test -v ./test/unit

# Integration tests only
go test -v ./test/integration -timeout 30s
```

## Dependencies

Requires:
- Running Nomad cluster with Docker driver
- Nomad-Vault integration for secrets
- Persistent volume access for build caching
- For GPU builds: GPU drivers and device plugins on Nomad clients

## Documentation Maintenance

**CRITICAL**: Whenever you make changes to the codebase (code, configuration, APIs, endpoints, etc.), you MUST update README.md to reflect those changes. The README.md is the primary user-facing documentation and must always be accurate and current. This includes:
- API endpoint changes (ports, paths, parameters)
- Configuration variable changes
- Service behavior changes
- New features or removed features
- Deployment or testing procedure changes

**CRITICAL**: Whenever you make changes to the job specification (JobConfig, TestConfig, or any related types in pkg/types/job.go), you MUST update docs/JobSpec.md to reflect those changes. The JobSpec.md is the comprehensive job configuration reference and must accurately document:
- New configuration fields or parameters
- Changes to existing fields (types, defaults, behavior)
- New validation rules or constraints
- New examples demonstrating the features
- GPU configuration and hardware requirements
- Node constraint options

**Before committing any changes, always verify that both README.md and docs/JobSpec.md accurately reflect the current state of the codebase.**
