# Job Specification

This document describes the complete job configuration schema for the Nomad Build Service.

## Overview

Job configurations can be provided in either **JSON** or **YAML** format. The CLI tool supports YAML with a two-file approach (global + per-build), while the JSON-RPC APIs accept either format.

## Required Fields

These fields **must** be provided in every job configuration:

| Field | Type | Description | Example |
|-------|------|-------------|---------|
| `owner` | string | Job owner/team identifier | `"myteam"` |
| `repo_url` | string | Git repository URL (HTTP/HTTPS) | `"https://github.com/org/repo.git"` |
| `image_name` | string | Base name for Docker image | `"myservice"` |
| `image_tags` | array[string] | Image tags to apply | `["v1.0.0", "latest"]` |
| `registry_url` | string | Target registry URL | `"registry.example.com:5000/myapp"` |

## Optional Fields with Defaults

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `git_ref` | string | `"main"` | Git branch, tag, or commit SHA |
| `dockerfile_path` | string | `"Dockerfile"` | Path to Dockerfile in repo |
| `dockerfile_context` | string | `"."` | Build context directory (see below) |
| `git_credentials_path` | string | - | Vault path for Git credentials |
| `registry_credentials_path` | string | - | Vault path for registry credentials |

### Dockerfile Context

The `dockerfile_context` field specifies the build context directory for Docker builds. This is essential when:
- Your Dockerfile is in a subdirectory
- The Dockerfile contains `COPY` or `ADD` commands that reference files relative to its own directory

**Example:** If your Dockerfile is at `myapp/Dockerfile` and contains `COPY requirements.txt .`, by default the build will fail because it looks for `requirements.txt` at the repository root. Setting `dockerfile_context: myapp/` fixes this by making the build context the `myapp/` directory.

```yaml
# Dockerfile in subdirectory with relative COPY commands
dockerfile_path: nvidia-privacy-model-service/Dockerfile.gpu
dockerfile_context: nvidia-privacy-model-service/
```

This generates the equivalent of:
```bash
docker build -f nvidia-privacy-model-service/Dockerfile.gpu -t image nvidia-privacy-model-service/
```

**Validation rules:**
- Must be a relative path (cannot start with `/`)
- Cannot contain `..` (path traversal is not allowed)
- Defaults to `.` (repository root) if not specified

## Optional Fields

### Testing Configuration

Test configuration is specified under the `test` key, which groups all test-related settings.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `test.commands` | array[string] | `[]` | Commands to run in test phase |
| `test.entry_point` | boolean | `false` | Test container's ENTRYPOINT |
| `test.env` | map[string]string | `{}` | Environment variables for test containers |
| `test.vault_policies` | array[string] | `[]` | Vault policies for secret access |
| `test.vault_secrets` | array[VaultSecret] | `[]` | Vault secrets to inject as env vars |
| `test.resource_limits` | ResourceLimits | - | Per-test resource overrides |
| `test.timeout` | duration | `15m` | Maximum test phase duration |

**Note**: If both `test.commands` and `test.entry_point` are empty/false, the test phase is **skipped**.

#### GPU and Node Constraint Configuration

For GPU-accelerated workloads or specific node targeting, additional test configuration options are available:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `test.gpu_required` | boolean | `false` | Enable NVIDIA GPU runtime and automatically constrain to GPU-capable nodes |
| `test.gpu_count` | integer | `0` | **Deprecated** - GPU allocation is controlled via `NVIDIA_VISIBLE_DEVICES` env var |
| `test.constraints` | array[Constraint] | `[]` | Custom Nomad node constraints for test job placement |

**Constraint Type**:

Each constraint specifies a Nomad node attribute to match:

| Field | Type | Description |
|-------|------|-------------|
| `attribute` | string | Node attribute to match (e.g., `"${meta.gpu-capable}"`, `"${node.datacenter}"`) |
| `value` | string | Expected attribute value |
| `operand` | string | Comparison operator: `"="`, `"!="`, `"regexp"`, `">="`, `"<="`, etc. |

**GPU Requirements**:
- GPU-capable Nomad nodes must have the `meta.gpu-capable = "true"` attribute set
- The Docker driver must support the NVIDIA runtime (`runtime = "nvidia"`)
- GPU tests automatically get the GPU capability constraint added
- **GPU allocation** is controlled via environment variables, NOT Nomad device plugins:
  - `NVIDIA_VISIBLE_DEVICES` defaults to `"all"` when `gpu_required: true`
  - You can override by setting `NVIDIA_VISIBLE_DEVICES` in `test.env` (e.g., `"0"`, `"0,1"`)
  - This approach works without requiring Nomad NVIDIA device plugins

**Examples**:

```yaml
# Run specific test commands
test:
  commands:
    - /app/run-tests.sh
    - /app/validate.sh

# Test the entrypoint
test:
  entry_point: true

# Test with environment variables
test:
  entry_point: true
  env:
    S3_TRANSCRIBER_BUCKET: "ai-storage"
    S3_TRANSCRIBER_PREFIX: "transcriber"
    OLLAMA_HOST: "http://10.0.1.12:11434"
    AWS_REGION: "us-east-1"
    NVIDIA_VISIBLE_DEVICES: "all"

# Complete test configuration
test:
  commands:
    - npm test
    - npm run lint
  env:
    NODE_ENV: "test"
    DATABASE_URL: "postgres://test:test@localhost/testdb"
  resource_limits:
    cpu: "2000"
    memory: "4096"
    disk: "10240"
  timeout: 20m

# Test with Vault secrets
test:
  entry_point: true
  vault_policies:
    - transcription-policy
  vault_secrets:
    - path: "secret/data/aws/transcription"
      fields:
        access_key_id: "AWS_ACCESS_KEY_ID"
        secret_access_key: "AWS_SECRET_ACCESS_KEY"
        region: "AWS_DEFAULT_REGION"
    - path: "secret/data/ml/tokens"
      fields:
        hf_token: "HUGGING_FACE_HUB_TOKEN"

# GPU-accelerated test (ML/AI workload)
test:
  entry_point: true
  gpu_required: true  # Enables nvidia runtime, constrains to GPU nodes, auto-sets NVIDIA_VISIBLE_DEVICES=all
  env:
    CUDA_VISIBLE_DEVICES: "0,1"    # Optional: CUDA-specific GPU selection
  vault_policies:
    - ml-secrets-policy
  vault_secrets:
    - path: "secret/data/ml/api-keys"
      fields:
        api_key: "ML_API_KEY"
  resource_limits:
    cpu: "8000"
    memory: "16384"
    disk: "20480"

# Test with custom node constraints
test:
  commands:
    - /app/run-tests.sh
  constraints:
    - attribute: "${node.datacenter}"
      value: "us-west-2"
      operand: "="
    - attribute: "${meta.storage-type}"
      value: "ssd"
      operand: "="
  env:
    TEST_REGION: "us-west-2"
```

#### External Python Tests Configuration

For web services where you need to test the API from outside the container, you can run external Python tests using `python-executor`:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `test.python_cwd` | string | - | Working directory for python tests (relative to repo root) |
| `test.python_command` | string | - | Command to run (e.g., `"python-executor run --file test.py"`) |
| `test.health_endpoint` | string | `"/health"` | Health check endpoint to poll before running tests |
| `test.health_timeout` | integer | `60` | Seconds to wait for health check |
| `test.container_port` | integer | `8080` | Port the container exposes for the web service |

**How It Works**:

1. The container is started as a long-running service (Nomad service job)
2. The CLI waits for the health endpoint to respond with 2xx status
3. The CLI runs the python-executor command with `SERVICE_HOST` and `SERVICE_PORT` environment variables
4. python-executor runs the tests against the running container
5. Test results are reported back to the server
6. If tests pass, the publish phase continues; otherwise, the job fails

**Requirements**:
- The `--watch` flag is **required** when using python tests
- `python-executor` must be installed on the machine running the CLI
- The test code must be in the git repository (referenced by `python_cwd`)
- The container must expose an HTTP server and respond to health checks
- Python tests are **exclusive** - cannot be combined with `commands` or `entry_point`

**Environment Variables Injected**:
- `SERVICE_HOST` - IP address of the running test container
- `SERVICE_PORT` - Dynamic port assigned by Nomad

**Example**:

```yaml
# External Python tests for a web service
test:
  python_cwd: "tests"
  python_command: "python-executor run --requirements requirements.txt --file test_api.py"
  health_endpoint: "/health"
  health_timeout: 120
  container_port: 8080
  env:
    LOG_LEVEL: "debug"
  vault_policies:
    - api-secrets-policy
  vault_secrets:
    - path: "secret/data/api/credentials"
      fields:
        api_key: "API_KEY"
```

**Usage**:

```bash
# Python tests require --watch flag
jobforge submit-job build.yaml --watch

# Output will show:
# [12:34:56] 🔨 Status: BUILDING | Phase: build
# [12:35:42] 🐍 Status: TESTING_EXTERNAL | Phase: test
#
# [12:35:45] 🐍 Starting external Python tests...
# Waiting for test container endpoint...
# Test container running at 10.0.1.15:21654
# Waiting for health check at http://10.0.1.15:21654/health (timeout: 120s)...
# Health check passed!
# Running Python tests...
# [python-executor output...]
# Python tests completed successfully in 15.3s
#
# [12:36:10] 📦 Status: PUBLISHING | Phase: publish
# [12:36:45] ✅ Status: SUCCEEDED | Phase: publish
#
# ✅ Job completed successfully
```

#### Vault Secrets Configuration

**VaultSecret Type**:

Each vault secret has two required fields:

| Field | Type | Description |
|-------|------|-------------|
| `path` | string | Vault secret path (KV v2 format: `secret/data/...`) |
| `fields` | map[string]string | Mapping of Vault field names to container environment variable names |

**Field Mapping Syntax**:

The `fields` map uses this format:
```yaml
fields:
  <vault_field_name>: "<CONTAINER_ENV_VAR_NAME>"
```

- **LEFT side** (key): The field name **stored in Vault**
- **RIGHT side** (value): The environment variable name **set in the Docker container**

**Example**:
```yaml
vault_secrets:
  - path: "secret/data/ml/api-keys"
    fields:
      openai_key: "OPENAI_API_KEY"        # Vault field "openai_key" → Container env var OPENAI_API_KEY
      anthropic_key: "ANTHROPIC_API_KEY"  # Vault field "anthropic_key" → Container env var ANTHROPIC_API_KEY
```

If you stored secrets in Vault like this:
```bash
vault kv put secret/ml/api-keys \
  openai_key="sk-proj-abc123..." \
  anthropic_key="sk-ant-xyz789..."
```

Then your test container will receive these environment variables:
- `OPENAI_API_KEY=sk-proj-abc123...`
- `ANTHROPIC_API_KEY=sk-ant-xyz789...`

**How It Works**:

1. The build service creates Vault templates for each secret source
2. During test execution, Vault automatically injects secrets as environment variables
3. Test containers receive the environment variables with the names you specified
4. Secrets are never logged or exposed in job configurations

**Requirements**:

- `vault_policies` is **required** when `vault_secrets` are specified
- Vault policies must grant read access to the specified secret paths
- Secret paths must use Vault KV v2 format (`secret/data/...`)
- The Nomad cluster must have Vault integration configured

**Common Use Cases**:

```yaml
# AWS credentials
# Vault storage: vault kv put secret/aws/s3-credentials access_key="AKIA..." secret_key="..."
test:
  vault_policies: ["aws-policy"]
  vault_secrets:
    - path: "secret/data/aws/s3-credentials"
      fields:
        access_key: "AWS_ACCESS_KEY_ID"        # LEFT=Vault field, RIGHT=Container env var
        secret_key: "AWS_SECRET_ACCESS_KEY"

# Machine Learning API tokens
# Vault storage: vault kv put secret/ml/api-keys openai_key="sk-..." anthropic_key="sk-ant-..."
test:
  vault_policies: ["ml-api-policy"]
  vault_secrets:
    - path: "secret/data/ml/api-keys"
      fields:
        openai_key: "OPENAI_API_KEY"           # LEFT=Vault field, RIGHT=Container env var
        anthropic_key: "ANTHROPIC_API_KEY"

# Database credentials
# Vault storage: vault kv put secret/postgres/test-db username="user" password="pass" host="db.example.com"
test:
  vault_policies: ["database-test-policy"]
  vault_secrets:
    - path: "secret/data/postgres/test-db"
      fields:
        username: "DB_USER"                    # LEFT=Vault field, RIGHT=Container env var
        password: "DB_PASSWORD"
        host: "DB_HOST"
```

### Resource Limits

Resource limits can be specified globally or per-phase. Per-phase limits override global limits.

#### Global Resource Limits (Legacy)

```yaml
resource_limits:
  cpu: "2000"      # MHz
  memory: "4096"   # MB
  disk: "20480"    # MB
```

Applies to all phases (build, test, publish) unless overridden.

#### Per-Phase Resource Limits (Recommended)

```yaml
resource_limits:
  build:
    cpu: "2000"
    memory: "4096"
    disk: "20480"
  test:
    cpu: "1000"
    memory: "2048"
    disk: "10240"
  publish:
    cpu: "1000"
    memory: "2048"
    disk: "10240"
```

**Default values** (if not specified):
- CPU: `1000` MHz
- Memory: `2048` MB
- Disk: `10240` MB

### Timeouts

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `build_timeout` | duration | `30m` | Maximum build phase duration |
| `test_timeout` | duration | `15m` | Maximum test phase duration |

**Duration format**: Use Go duration strings like `"30m"`, `"1h30m"`, `"45s"`.

### Build Cache

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clear_cache` | boolean | `false` | Clear Buildah layer cache before build |

### Webhooks

Configure HTTP webhooks for build notifications:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `webhook_url` | string | - | URL to POST notifications |
| `webhook_secret` | string | - | HMAC secret for signature verification |
| `webhook_on_success` | boolean | `true` | Send webhook on success |
| `webhook_on_failure` | boolean | `true` | Send webhook on failure |
| `webhook_headers` | map[string]string | - | Custom HTTP headers |

#### Webhook Payload Format

When a webhook is triggered, the service sends an HTTP POST request with the following characteristics:

**HTTP Headers:**
```
Content-Type: application/json
User-Agent: nomad-build-service/1.0
X-Webhook-Signature: sha256=<hmac-signature>  # If webhook_secret is configured
<custom-headers>  # Any headers specified in webhook_headers
```

**JSON Payload:**
```json
{
  "job_id": "abc123def456",
  "status": "SUCCEEDED",
  "timestamp": "2024-01-15T10:30:45Z",
  "duration": 125000000000,
  "owner": "myteam",
  "repo_url": "https://github.com/myorg/myservice.git",
  "git_ref": "main",
  "image_name": "myservice",
  "image_tags": ["v1.0.0", "latest"],
  "phase": "publish",
  "error": "",
  "logs": {
    "build": "...",
    "test": "...",
    "publish": "..."
  },
  "metrics": {
    "job_start": "2024-01-15T10:28:40Z",
    "job_end": "2024-01-15T10:30:45Z",
    "build_start": "2024-01-15T10:28:40Z",
    "build_end": "2024-01-15T10:29:30Z",
    "test_start": "2024-01-15T10:29:31Z",
    "test_end": "2024-01-15T10:30:15Z",
    "publish_start": "2024-01-15T10:30:16Z",
    "publish_end": "2024-01-15T10:30:45Z"
  }
}
```

**Payload Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `job_id` | string | Unique job identifier |
| `status` | string | Job status: `SUCCEEDED`, `FAILED`, `BUILDING`, `TESTING`, `PUBLISHING` |
| `timestamp` | string (RFC3339) | When the webhook was triggered |
| `duration` | number (nanoseconds) | Total job duration (if completed) |
| `owner` | string | Job owner from config |
| `repo_url` | string | Git repository URL |
| `git_ref` | string | Git branch/tag/commit |
| `image_name` | string | Docker image name |
| `image_tags` | array[string] | Applied image tags |
| `phase` | string | Current or failed phase: `build`, `test`, `publish` |
| `error` | string | Error message (only present on failure) |
| `logs` | object | Phase-specific logs (optional) |
| `metrics` | object | Timing metrics for each phase (optional) |

**HMAC Signature Verification:**

If `webhook_secret` is configured, verify the webhook authenticity:

```python
# Python example
import hmac
import hashlib

def verify_webhook(payload_bytes, signature_header, secret):
    expected = hmac.new(
        secret.encode('utf-8'),
        payload_bytes,
        hashlib.sha256
    ).hexdigest()
    expected_sig = f"sha256={expected}"
    return hmac.compare_digest(expected_sig, signature_header)

# Usage
signature = request.headers.get('X-Webhook-Signature')
is_valid = verify_webhook(request.body, signature, 'my-secret-key')
```

```bash
# Bash example
PAYLOAD='{"job_id":"abc123",...}'
SECRET="my-secret-key"
EXPECTED="sha256=$(echo -n "$PAYLOAD" | openssl dgst -sha256 -hmac "$SECRET" | cut -d' ' -f2)"
# Compare with X-Webhook-Signature header
```

**Webhook Events:**

Webhooks are sent for these events (configurable via `webhook_on_success`/`webhook_on_failure`):
- Job completed successfully (status: `SUCCEEDED`)
- Job failed (status: `FAILED`)
- Build phase failed
- Test phase failed

**Retry Behavior:**
- Failed webhooks are retried up to 3 times
- Exponential backoff between retries (1s, 2s, 3s)
- 30-second timeout per request

## Complete Examples

### Minimal Configuration (JSON)

```json
{
  "owner": "myteam",
  "repo_url": "https://github.com/myorg/myservice.git",
  "image_name": "myservice",
  "image_tags": ["v1.0.0"],
  "registry_url": "registry.example.com:5000/myapp"
}
```

### Minimal Configuration (YAML)

```yaml
owner: myteam
repo_url: https://github.com/myorg/myservice.git
image_name: myservice
image_tags:
  - v1.0.0
registry_url: registry.example.com:5000/myapp
```

### Complete Configuration with All Options (YAML)

```yaml
# Required fields
owner: myteam
repo_url: https://github.com/myorg/myservice.git
image_name: myservice
image_tags:
  - v1.2.3
  - latest
  - stable
registry_url: registry.example.com:5000/myapp

# Git configuration
git_ref: feature/new-feature
git_credentials_path: secret/nomad/jobs/git-credentials

# Build configuration
dockerfile_path: docker/Dockerfile
registry_credentials_path: secret/nomad/jobs/registry-credentials

# Testing
test:
  commands:
    - /app/run-tests.sh
    - /app/validate-config.sh
  env:
    NODE_ENV: "production"
    API_URL: "https://api.example.com"
  vault_policies:
    - app-secrets-policy
  vault_secrets:
    - path: "secret/data/app/credentials"
      fields:
        api_key: "APP_API_KEY"
        db_password: "DB_PASSWORD"

# Resource limits (per-phase)
resource_limits:
  build:
    cpu: "2000"
    memory: "4096"
    disk: "20480"
  test:
    cpu: "1000"
    memory: "2048"
    disk: "10240"
  publish:
    cpu: "1000"
    memory: "2048"
    disk: "10240"

# Timeouts
build_timeout: 45m
test_timeout: 20m

# Cache management
clear_cache: false

# Webhooks
webhook_url: https://my-webhook.example.com/notify
webhook_secret: my-secret-key
webhook_on_success: true
webhook_on_failure: true
webhook_headers:
  X-Custom-Header: my-value
  X-Team: myteam
```

### Global + Per-Build Configuration (CLI)

**deploy/global.yaml** (shared settings):

```yaml
owner: myteam
repo_url: https://github.com/myorg/myservice.git
git_credentials_path: secret/nomad/jobs/git-credentials
dockerfile_path: Dockerfile
image_name: myservice
registry_url: registry.example.com:5000/myapp
registry_credentials_path: secret/nomad/jobs/registry-credentials

# Default resource limits
resource_limits:
  build:
    cpu: "2000"
    memory: "4096"
    disk: "20480"
  test:
    cpu: "1000"
    memory: "2048"
    disk: "10240"
  publish:
    cpu: "1000"
    memory: "2048"
    disk: "10240"
```

**build.yaml** (per-build overrides):

```yaml
git_ref: feature/auth-fix
image_tags:
  - test
  - dev
test:
  entry_point: true
  env:
    TEST_MODE: "integration"
webhook_url: https://hooks.slack.com/services/T00/B00/XXX
```

**Usage**:
```bash
jobforge submit-job -global deploy/global.yaml -config build.yaml
```

**Result**: The CLI merges these configs, with `build.yaml` values overriding `global.yaml`, and automatically adds a branch-aware version tag.

## Vault Credentials Format

### Git Credentials

Store at the path specified in `git_credentials_path`:

```bash
vault kv put secret/nomad/jobs/git-credentials \
  username="git-user" \
  password="ghp_token123..." \
  ssh_key="$(cat ~/.ssh/id_rsa)"  # Optional
```

### Registry Credentials

Store at the path specified in `registry_credentials_path`:

```bash
vault kv put secret/nomad/jobs/registry-credentials \
  username="registry-user" \
  password="registry-password"
```

**Note**: Registry credentials are only needed for **private registries**. Public registries (like Docker Hub for public images) don't require credentials.

### Test Phase Secrets

Store secrets for test containers at paths referenced in `test.vault_secrets`.

**IMPORTANT**: The field names you use in `vault kv put` (left side) must match the field names in your YAML configuration's `fields` mapping (left side of the mapping).

```bash
# AWS credentials for test phase
# Field names: access_key_id, secret_access_key, region
vault kv put secret/aws/transcription \
  access_key_id="AKIA..." \
  secret_access_key="..." \
  region="us-east-1"

# ML API tokens
# Field names: hf_token, openai_key
vault kv put secret/ml/tokens \
  hf_token="hf_..." \
  openai_key="sk-..."

# Database credentials
# Field names: username, password, host
vault kv put secret/postgres/test-db \
  username="testuser" \
  password="testpass" \
  host="postgres.example.com"
```

**Example YAML configuration using these secrets**:
```yaml
test:
  vault_policies:
    - transcription-policy
  vault_secrets:
    - path: "secret/data/aws/transcription"
      fields:
        access_key_id: "AWS_ACCESS_KEY_ID"        # Vault field → Container env var
        secret_access_key: "AWS_SECRET_ACCESS_KEY"
        region: "AWS_DEFAULT_REGION"
    - path: "secret/data/ml/tokens"
      fields:
        hf_token: "HUGGING_FACE_HUB_TOKEN"
        openai_key: "OPENAI_API_KEY"
```

**Important**: Create corresponding Vault policies that grant read access to these paths, and specify those policy names in `test.vault_policies`.

**Example Vault Policy**:

```hcl
# transcription-policy.hcl
path "secret/data/aws/transcription" {
  capabilities = ["read"]
}

path "secret/data/ml/tokens" {
  capabilities = ["read"]
}
```

Apply the policy:
```bash
vault policy write transcription-policy transcription-policy.hcl
```

## Field Validation Rules

### String Fields
- **owner**: Must be non-empty
- **repo_url**: Must be non-empty, should be valid Git URL
- **image_name**: Must be non-empty
- **registry_url**: Must be non-empty

### Array Fields
- **image_tags**: Must have at least one tag
- **test_commands**: Can be empty (skips test phase)

### Path Fields
- **dockerfile_path**: Defaults to `"Dockerfile"` if empty
- **git_ref**: Defaults to `"main"` if empty

### Resource Limits
- **cpu**: Positive integer as string (MHz)
- **memory**: Positive integer as string (MB)
- **disk**: Positive integer as string (MB)

### Timeouts
- **build_timeout**: Valid Go duration string (e.g., `"30m"`, `"1h"`)
- **test_timeout**: Valid Go duration string

### Vault Secrets
- **vault_policies**: Required when `vault_secrets` are specified
- **vault_secrets[].path**: Must be non-empty Vault secret path (KV v2 format)
- **vault_secrets[].fields**: Must have at least one field mapping
- **vault_secrets[].fields keys/values**: Both must be non-empty strings

## CLI Version Management

When using the CLI tool, additional behavior applies:

### Automatic Version Tags

The CLI automatically:
1. **Increments** patch version in `deploy/version.yaml`
2. **Detects** current Git branch
3. **Generates** branch-aware tag (e.g., `feature-auth-v0.1.5`)
4. **Appends** the tag to `image_tags`

**Example**:
```bash
# deploy/version.yaml shows: version: { major: 0, minor: 1, patch: 4 }
# Current branch: feature-auth
# Your build.yaml specifies: image_tags: ["test"]

jobforge submit-job -config build.yaml

# Actual tags sent to server:
# - "test" (from build.yaml)
# - "feature-auth-v0.1.5" (auto-generated)
# Version file updated to: version: { major: 0, minor: 1, patch: 5 }
```

### Additional Tags via CLI

You can add more tags using `--image-tags`:

```bash
jobforge submit-job -config build.yaml --image-tags "latest,stable"

# Final tags:
# - Tags from build.yaml
# - Auto-generated version tag
# - "latest", "stable" (from --image-tags)
```

## API Submission Formats

### MCP Protocol (JSON-RPC)

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "submitJob",
    "arguments": {
      "job_config": {
        "owner": "myteam",
        "repo_url": "https://github.com/myorg/myservice.git",
        "git_ref": "main",
        "dockerfile_path": "Dockerfile",
        "image_name": "myservice",
        "image_tags": ["v1.0.0"],
        "registry_url": "registry.example.com:5000/myapp",
        "test": {
          "entry_point": true,
          "env": {
            "NODE_ENV": "test",
            "API_URL": "https://api-test.example.com"
          }
        }
      }
    }
  }
}
```

### Direct JSON-RPC API

```bash
curl -X POST http://localhost:8080/json/submitJob \
  -H "Content-Type: application/json" \
  -d '{
    "job_config": {
      "owner": "myteam",
      "repo_url": "https://github.com/myorg/myservice.git",
      "git_ref": "main",
      "image_name": "myservice",
      "image_tags": ["v1.0.0"],
      "registry_url": "registry.example.com:5000/myapp",
      "test": {
        "commands": ["npm test"],
        "env": {
          "NODE_ENV": "test"
        }
      }
    }
  }'
```

### CLI Tool (YAML)

```bash
# Create build.yaml with the configuration
jobforge submit-job -config build.yaml
```

## Build Phases

Understanding the three-phase workflow:

### 1. Build Phase
- Clones Git repository
- Builds Docker image using Buildah
- Pushes to temporary registry location: `{registry_url}/temp/{job_id}:build`
- Uses resource limits from `resource_limits.build` or global `resource_limits`
- Timeout: `build_timeout` (default: 30m)

### 2. Test Phase
- **Skipped if**: No `test.commands` and `test.entry_point` is `false`
- Runs Docker container with built image
- Executes test commands OR tests entrypoint
- Environment variables from `test.env` are applied to test containers
- Vault secrets from `test.vault_secrets` are injected as environment variables
- Multiple test jobs run in parallel if multiple test commands specified
- Uses resource limits from `test.resource_limits` or `resource_limits.test` or global `resource_limits`
- Timeout: `test.timeout` or global `test_timeout` (default: 15m)

### 3. Publish Phase
- Pulls image from temporary location
- Re-tags with all specified `image_tags`
- Pushes final images to `{registry_url}/{image_name}:{tag}`
- Cleans up temporary image
- Uses resource limits from `resource_limits.publish` or global `resource_limits`

## Common Patterns

### Public Repository, Public Registry
```yaml
owner: myteam
repo_url: https://github.com/myorg/public-repo.git
git_ref: main
dockerfile_path: Dockerfile
image_name: myapp
image_tags: [v1.0.0]
registry_url: docker.io/myorg
```

### Private Repository, Private Registry
```yaml
owner: myteam
repo_url: https://github.com/myorg/private-repo.git
git_credentials_path: secret/nomad/jobs/git-credentials
git_ref: main
dockerfile_path: Dockerfile
image_name: myapp
image_tags: [v1.0.0]
registry_url: registry.example.com:5000/myapp
registry_credentials_path: secret/nomad/jobs/registry-credentials
```

### With Tests and Custom Resources
```yaml
owner: myteam
repo_url: https://github.com/myorg/myservice.git
git_ref: main
dockerfile_path: Dockerfile
image_name: myservice
image_tags: [v1.0.0]
registry_url: registry.example.com:5000/myapp

test:
  commands:
    - npm test
    - npm run lint
  env:
    NODE_ENV: "test"
    CI: "true"

resource_limits:
  build:
    cpu: "4000"
    memory: "8192"
    disk: "40960"
  test:
    cpu: "2000"
    memory: "4096"
    disk: "20480"
```

### With Webhooks for CI/CD Integration
```yaml
owner: myteam
repo_url: https://github.com/myorg/myservice.git
git_ref: main
dockerfile_path: Dockerfile
image_name: myservice
image_tags: [v1.0.0]
registry_url: registry.example.com:5000/myapp

webhook_url: https://ci.example.com/webhook/build-complete
webhook_secret: super-secret-key
webhook_on_success: true
webhook_on_failure: true
webhook_headers:
  X-CI-Project: myservice
  X-CI-Pipeline: production
```

### With Vault Secrets for Test Phase
```yaml
owner: myteam
repo_url: https://github.com/myorg/ml-service.git
git_ref: main
dockerfile_path: Dockerfile
image_name: ml-service
image_tags: [v1.0.0]
registry_url: registry.example.com:5000/myapp

test:
  entry_point: true
  env:
    S3_TRANSCRIBER_BUCKET: "ai-storage"
    S3_TRANSCRIBER_PREFIX: "transcriber"
    OLLAMA_HOST: "http://10.0.1.12:11434"
  vault_policies:
    - transcription-policy
    - ml-tokens-policy
  vault_secrets:
    - path: "secret/data/aws/transcription"
      fields:
        access_key_id: "AWS_ACCESS_KEY_ID"
        secret_access_key: "AWS_SECRET_ACCESS_KEY"
        region: "AWS_DEFAULT_REGION"
    - path: "secret/data/ml/tokens"
      fields:
        hf_token: "HUGGING_FACE_HUB_TOKEN"
        openai_key: "OPENAI_API_KEY"
```

### GPU-Accelerated ML Workload
```yaml
owner: ai-team
repo_url: https://github.com/myorg/video-transcription.git
git_ref: main
dockerfile_path: Dockerfile
image_name: video-transcription-batch
image_tags: [latest]
registry_url: registry.cluster:5000

test:
  entry_point: true
  gpu_required: true  # Enables nvidia runtime + GPU node constraint (auto-sets NVIDIA_VISIBLE_DEVICES=all)
  env:
    # Application-specific environment variables
    S3_TRANSCRIBER_BUCKET: "ai-storage"
    S3_TRANSCRIBER_PREFIX: "transcriber"
    S3_JOB_ID: "test-job-id"
    OLLAMA_HOST: "http://10.0.1.12:11434"
    AWS_REGION: "us-east-1"
    S3_ENDPOINT: "http://cluster00.cluster"
  vault_policies:
    - transcription-policy
  vault_secrets:
    - path: "secret/data/aws/transcription"
      fields:
        access_key: "AWS_ACCESS_KEY_ID"
        secret_key: "AWS_SECRET_ACCESS_KEY"
    - path: "secret/data/hf/transcription"
      fields:
        token: "HF_TOKEN"
  resource_limits:
    test:
      cpu: "8000"
      memory: "16384"
      disk: "20480"
```
