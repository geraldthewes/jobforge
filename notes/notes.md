Based on my exploration of the codebase, I can provide you with a comprehensive explanation of how Vault secrets are configured and accessed in
  Nomad jobs. This will help you explain the pattern to the devpod-provider-nomad maintainer.

  How Vault Secrets Access Works

  Configuration Format

  The vault_secrets field uses this structure:

  vault_secrets:
    - path: "secret/data/aws/transcription"  # Vault KV v2 path
      fields:
        access_key_id: "AWS_ACCESS_KEY_ID"          # vault_field: "ENV_VAR_NAME"
        secret_access_key: "AWS_SECRET_ACCESS_KEY"  # vault_field: "ENV_VAR_NAME"
        region: "AWS_DEFAULT_REGION"

  Key points:
  - Left side (key): Field name in the Vault secret
  - Right side (value): Environment variable name in the container
  - Path format: Must be Vault KV v2 format (secret/data/...)

  How It Works Under the Hood

  1. Template Generation

  When the system processes vault_secrets, it generates Nomad template stanzas:

  // For each vault_secret entry, creates a template like:
  {{- with secret "secret/data/aws/transcription" -}}
  export AWS_ACCESS_KEY_ID="{{ .Data.data.access_key_id }}"
  export AWS_SECRET_ACCESS_KEY="{{ .Data.data.secret_access_key }}"
  export AWS_DEFAULT_REGION="{{ .Data.data.region }}"
  {{- end -}}

  This template is saved to secrets/vault-0.env (or vault-1.env, etc.)

  2. Nomad's Vault Integration

  The task is configured with:

  task.Vault = &Vault{
      Policies:   ["your-policy-name"],  // Required!
      Role:       "nomad-workloads",
      ChangeMode: "restart",
  }

  task.Templates = []*Template{
      {
          DestPath:     "secrets/vault-0.env",
          EmbeddedTmpl: "<template content from above>",
          Envvars:      true,  // Key setting: makes secrets available as env vars
      },
  }

  3. Runtime Execution

  When the Nomad task starts:

  1. Nomad authenticates to Vault using the configured role and policies
  2. Renders the template by fetching the secret from Vault
  3. Sources the file (source secrets/vault-0.env) because Envvars: true
  4. Container receives the environment variables automatically

  Required Components

  For vault_secrets to work, you MUST also specify:

  vault_policies:
    - "your-policy-name"  # Vault policy that grants access to the secret paths

  vault_secrets:
    - path: "secret/data/..."
      fields:
        vault_field: "ENV_VAR"

  Validation enforces:
  - If vault_secrets is specified, vault_policies MUST be provided
  - Each secret must have a non-empty path
  - Each secret must have at least one field mapping
  - Field names and env var names cannot be empty

  Multiple Secrets Example

  You can configure multiple secrets:

  vault_policies:
    - "aws-policy"
    - "ml-policy"

  vault_secrets:
    # AWS credentials
    - path: "secret/data/aws/transcription"
      fields:
        access_key_id: "AWS_ACCESS_KEY_ID"
        secret_access_key: "AWS_SECRET_ACCESS_KEY"

    # ML API tokens
    - path: "secret/data/ml/tokens"
      fields:
        hf_token: "HUGGING_FACE_HUB_TOKEN"
        openai_key: "OPENAI_API_KEY"

  Each creates a separate template file (vault-0.env, vault-1.env, etc.), all sourced as environment variables.

  For devpod-provider-nomad Implementation

  To implement this in the devpod provider:

  1. Accept configuration in the provider config for vault_secrets and vault_policies
  2. Generate Nomad templates for each vault secret entry
  3. Attach to task with Envvars: true to make them available as environment variables
  4. Configure task.Vault with the policies and role

  The secrets will be automatically available as environment variables in the container without any additional setup needed inside the container
  itself.

  Important: The Nomad cluster must have Vault integration already configured (which yours does, based on the existing usage in this build
  service).

  Is there any specific aspect of this integration you'd like me to clarify further?


Perfect! Now let's provide information about the cleanup script for the existing temporary images:

  Summary

  I've successfully identified and fixed the issue with temporary Docker registry images not being cleaned up. Here's what
  I found and implemented:

  Root Cause

  The system had a cleanup mechanism (cleanupTempImages function in internal/nomad/client.go:1184-1213), but it was only 
  triggered manually via the MCP cleanup tool. There was no automatic cleanup when jobs completed successfully or failed.

  Solution Implemented (v0.5.22)

  I've added automatic cleanup that triggers in the following scenarios:

  1. After successful publish (internal/nomad/client.go:406-410) - Cleans up temporary images after the job successfully
  publishes to final registry tags
  2. After failed builds (internal/nomad/client.go:274-280) - Cleans up temp images if the build phase fails
  3. After failed tests (internal/nomad/client.go:343-347) - Cleans up temp images if tests fail
  4. After failed publish (internal/nomad/client.go:455-459) - Cleans up temp images if publish fails

  The cleanup creates a Nomad batch job that:
  - Uses the Docker Registry v2 API to delete manifests by digest
  - Handles authentication for private registries
  - Logs comprehensive debugging information

  Changes Made

  Code changes (internal/nomad/client.go):
  - Added automatic cleanupTempImages() calls after each phase completes (success or failure)
  - Only cleans up temp images for jobs that had tests configured (since no-test builds go directly to final tags)

  Documentation updates (README.md):
  - Added section explaining automatic cleanup (v0.5.22+)
  - Updated CI/CD examples to reflect that manual cleanup is no longer needed
  - Clarified when temporary images are automatically removed

  Testing

  All tests passed successfully:
  ok  	nomad-mcp-builder/test/integration	134.757s
  ok  	nomad-mcp-builder/test/unit	0.552s

  Cleaning Up Existing Images

  For the 100+ existing temporary images in your registry, you have two options:

  Option 1: Use the provided Python cleanup script (recommended):
  cd scripts
  pip3 install requests
  python3 cleanup_registry.py --registry https://registry.cluster:5000 --pattern "bdtemp*" --dry-run  # Preview first
  python3 cleanup_registry.py --registry https://registry.cluster:5000 --pattern "bdtemp*"  # Actually delete

  Option 2: Manual cleanup via MCP (for individual images):
  jobforge cleanup <job-id>

  After running the cleanup script, you'll need to run registry garbage collection to actually free the disk space:
  # Find your registry container
  docker ps | grep registry
  # Run garbage collection
  docker exec <registry_container> /bin/registry garbage-collect /etc/docker/registry/config.yml

  Future Behavior

  Going forward (v0.5.22+), all new builds will automatically clean up their temporary images when they complete, whether
  successful or failed. You shouldn't see temporary image accumulation anymore!

● Perfect! The garbage collection successfully removed all the manifests and blobs. The repositories still appear in the catalog, but they're completely empty.
  This is expected behavior - the Docker registry catalog doesn't automatically clean up empty repository entries.

  Summary

  ✅ Successfully completed automatic cleanup implementation and registry cleanup!

  What was done:

  1. Implemented automatic cleanup (v0.5.22) - Temporary images are now automatically deleted after jobs complete (success or failure)
  2. Improved cleanup script - Enhanced scripts/cleanup_registry.py to:
    - Detect and report empty repositories
    - Show clear instructions for running garbage collection
    - Support multiple registry deployment types (Docker, Nomad, K8s)
  3. Ran registry garbage collection - Successfully cleaned up 100 empty bdtemp-* repositories:
    - All manifests deleted
    - All blobs marked for garbage collection and removed
    - Freed up registry storage space
  4. Updated documentation - README.md now explains the automatic cleanup feature

  Current state:

  - All temporary images have been removed from the registry
  - The 100 bdtemp-* catalog entries are now empty (no tags, no blobs)
  - Future builds will automatically clean up their temporary images

  Going forward:

  From now on with v0.5.22, temporary images will be automatically deleted when jobs finish. The empty catalog entries are harmless - they're just metadata. You
  can optionally run a periodic cron job to keep the registry pristine, but it's not necessary for functionality.

Here is Claude's plan:
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Plan: Add Python External Test Support for Web Services

 Summary

 Add a new test mode that launches a container as a running service and executes Python tests against it externally via python-executor. This is appropriate for
 web services where API testing needs to happen from outside the container.

 User Requirements (Confirmed)

 - Test code location: In the Git repo (e.g., tests/ folder)
 - Execution: python-executor CLI runs on user's machine, talks to remote python-executor server
 - Service discovery: Environment variables (SERVICE_HOST, SERVICE_PORT)
 - Watch mode: Required (--watch flag mandatory for python tests)
 - Health check: Required - poll endpoint before tests start
 - Port exposure: Dynamic host port assigned by Nomad

 New Configuration Options

 test:
   python_cwd: "tests"                                    # Subfolder in repo with test code
   python_command: "python-executor run --requirements requirements.txt --file test.py"
   health_endpoint: "/health"                             # Endpoint to poll (default: /health)
   health_timeout: 60                                     # Seconds to wait (default: 60)
   container_port: 8080                                   # Port container exposes (default: 8080)

 Architecture Flow

 1. User: jobforge submit-job build.yaml --watch (with python test config)
 2. Server: Build phase (existing)
 3. Server: Start test container as Nomad SERVICE job with dynamic port
 4. Server: Status -> TESTING_EXTERNAL with endpoint metadata
 5. CLI: Discovers SERVICE_HOST:SERVICE_PORT from job status
 6. CLI: Polls health_endpoint until healthy
 7. CLI: Runs python-executor with SERVICE_HOST/SERVICE_PORT env vars
 8. CLI: Captures stdout/stderr/exit_code
 9. CLI: POSTs test results to server (new endpoint)
 10. Server: Proceeds to publish if successful, or marks as failed
 11. Server: Cleans up test container

 Files to Modify

 1. pkg/types/job.go (lines 30-42)

 Add to TestConfig struct:
 PythonCwd       string `json:"python_cwd,omitempty" yaml:"python_cwd,omitempty"`
 PythonCommand   string `json:"python_command,omitempty" yaml:"python_command,omitempty"`
 HealthEndpoint  string `json:"health_endpoint,omitempty" yaml:"health_endpoint,omitempty"`
 HealthTimeout   int    `json:"health_timeout,omitempty" yaml:"health_timeout,omitempty"`
 ContainerPort   int    `json:"container_port,omitempty" yaml:"container_port,omitempty"`

 Add new status constant (line 8):
 StatusTestingExternal JobStatus = "TESTING_EXTERNAL"

 Add to Job struct (~line 185):
 TestServiceHost string `json:"test_service_host,omitempty"`
 TestServicePort int    `json:"test_service_port,omitempty"`
 TestJobNomadID  string `json:"test_job_nomad_id,omitempty"`

 2. pkg/types/mcp.go

 Add new request/response types:
 - GetTestEndpointResponse - returns SERVICE_HOST, SERVICE_PORT, status
 - ReportTestResultRequest - CLI sends success/exitCode/stdout/stderr
 - ReportTestResultResponse - acknowledges result

 3. internal/nomad/jobs.go (after line 482)

 Add new method:
 - createExternalTestJobSpec() - creates Nomad "service" job (not batch) with dynamic port allocation

 4. internal/nomad/client.go

 Modify startTestPhase() - check for python test mode, delegate to new method

 Add new methods:
 - startExternalTestPhase() - submits service job to Nomad
 - GetExternalTestEndpoint() - discovers allocated IP:port from running allocation
 - StopExternalTestJob() - deregisters the service job

 Modify UpdateJobStatus() - handle external test status, populate endpoint info when available

 5. internal/mcp/server.go

 Add new endpoints:
 - GET /mcp/job/{job_id}/test-endpoint - returns container endpoint info
 - POST /mcp/job/{job_id}/test-result - receives test results from CLI

 Add handlers:
 - handleGetTestEndpoint() - returns ServiceHost/ServicePort
 - handleReportTestResult() - processes result, triggers publish or fail

 6. pkg/client/client.go

 Add new client methods:
 - GetTestEndpoint() - calls test-endpoint API
 - ReportTestResult() - calls test-result API

 7. cmd/nomad-build/main.go

 Validate --watch required when python_command is set

 Modify watchJobProgress():
 - Accept jobConfig parameter
 - When status becomes TESTING_EXTERNAL, call runPythonTests()

 Add new functions:
 - runPythonTests() - orchestrates the external test flow:
   a. Get endpoint from server (with retries)
   b. Poll health endpoint until healthy
   c. Run python-executor command with SERVICE_HOST/PORT env vars
   d. Capture stdout/stderr
   e. Report results to server
 - reportTestFailure() - helper to report failures

 8. docs/JobSpec.md

 Document new configuration options and requirements

 Design Decisions

 1. Service job type: Use Nomad "service" (not batch) so container stays running
 2. Exclusive mode: Python tests cannot combine with commands/entry_point
 3. New status: TESTING_EXTERNAL distinguishes from internal tests
 4. CLI controls lifecycle: CLI runs tests and reports results, server responds

 Error Handling

 - Container startup timeout: 60s (30 retries x 2s)
 - Health check timeout: Configurable (default 60s)
 - Python test timeout: Controlled by python-executor
 - On any failure: Stop test container, cleanup temp images, release build lock

 Testing Strategy

 1. Unit tests for new type validations
 2. Integration test with mock python-executor
 3. End-to-end test with simple HTTP server container

