# Tasks: Add dockerfile_context field

## 1. Core Implementation

- [x] 1.1 Add `DockerfileContext` field to `JobConfig` struct in `pkg/types/job.go`
- [x] 1.2 Update buildah build command in `internal/nomad/jobs.go` to use context when specified
- [x] 1.3 Add `dockerfile_context` to MCP tool schema in `resources/mcp/tools/submitJob.yaml`

## 2. Validation

- [x] 2.1 Add validation for `dockerfile_context` (must be valid relative path, no path traversal)
- [x] 2.2 Ensure YAML config parsing handles new field correctly in `pkg/config/yaml.go`

## 3. Testing

- [x] 3.1 Add unit test for build command generation with dockerfile_context
- [x] 3.2 Add unit test for validation of dockerfile_context field
- [x] 3.3 Update existing tests that may be affected

## 4. Documentation

- [x] 4.1 Update `docs/JobSpec.md` with new field documentation and examples
- [x] 4.2 Add example YAML config showing dockerfile_context usage

## 5. Verification

- [x] 5.1 Run all unit tests: `go test ./...`
- [ ] 5.2 Deploy and run integration test with a real subdirectory Dockerfile build
