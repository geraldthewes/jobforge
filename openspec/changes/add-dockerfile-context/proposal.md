# Change: Add dockerfile_context field for build context specification

## Why

When Dockerfiles are located in subdirectories and contain relative path commands (like `COPY requirements.txt .`), the build context must be set to that subdirectory for the COPY commands to work correctly. Currently, the build always uses the repository root (`.`) as the context, which breaks builds where Dockerfiles expect files relative to their own directory.

Example: With `dockerfile_path: nvidia-privacy-model-service/Dockerfile.gpu`, a `COPY requirements.txt .` in that Dockerfile expects `requirements.txt` to be in the same subdirectory, but the current implementation looks for it at the repo root.

## What Changes

- Add new optional `dockerfile_context` field to `JobConfig` type
- Modify buildah build command to use the specified context directory instead of `.`
- Default behavior: If `dockerfile_context` is not specified, continue using `.` (repo root) for backward compatibility
- When specified, the build command becomes: `buildah bud -f <dockerfile_path> --tag <image> <dockerfile_context>`

**Example usage:**
```yaml
dockerfile_path: nvidia-privacy-model-service/Dockerfile.gpu
dockerfile_context: nvidia-privacy-model-service/
```

Generates: `buildah bud -f nvidia-privacy-model-service/Dockerfile.gpu --tag image nvidia-privacy-model-service/`

## Impact

- Affected specs: `nomad-build-service-spec.md` (Build Phase FR2)
- Affected code:
  - `pkg/types/job.go` - Add `DockerfileContext` field to `JobConfig`
  - `internal/nomad/jobs.go` - Use context in buildah command
  - `resources/mcp/tools/submitJob.yaml` - Add field to MCP tool schema
  - `pkg/config/yaml.go` - Ensure YAML parsing handles new field
- Affected docs:
  - `docs/JobSpec.md` - Document new field
  - `README.md` - Update examples if relevant
