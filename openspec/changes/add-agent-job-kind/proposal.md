# Change: Add an agent job kind

## Why
Jobforge can build, test, and publish an image, but it cannot run a coding agent that implements a task, reviews the diff, and opens a pull request.

## What Changes
- Add `kind: agent` beside the existing build pipeline. Build jobs stay the default.
- Run clone, opencode, sourced-review, push, and pull-request creation in one unprivileged Nomad allocation.
- Record `JOBFORGE_RESULT`, enforce job timeouts, and emit `agent.completed` / `agent.failed`.
- Optionally submit the existing build/test job on the pushed branch when a Dockerfile is present.

## Impact
- Affected specs: nomad-orchestration, nomad-build-service
- Affected code: `pkg/types`, `internal/nomad`, `internal/server`, `deploy/agent`, `examples/agent-job.yaml`
