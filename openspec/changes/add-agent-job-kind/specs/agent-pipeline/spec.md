## ADDED Requirements

### Requirement: Agent job kind
The system MUST accept a job whose kind is `agent` and MUST keep a job with an empty kind on the build pipeline.

#### Scenario: Build job without kind
- **WHEN** a job configuration omits `kind` and includes the build fields
- **THEN** validation uses the build rules and requires `dockerfile_path`, `image_name`, and `registry_url`

#### Scenario: Agent job
- **WHEN** a job configuration sets `kind` to `agent` with a task, image, review connection, and `output.provider` of `github` or `gitea`
- **THEN** validation does not require `dockerfile_path`, `image_name`, or `registry_url`

### Requirement: Single-allocation agent pipeline
The system MUST run an agent job as one unprivileged Nomad batch allocation that clones the repository, runs the coding agent, reviews the result, pushes a branch, and can open a pull request.

#### Scenario: Unprivileged container
- **WHEN** an agent job spec is generated
- **THEN** the Docker task sets `privileged` to false and does not mount a Docker socket

#### Scenario: Result line
- **WHEN** the runner finishes
- **THEN** it prints one `JOBFORGE_RESULT` JSON line containing the branch, pull request URL, verdict, iteration count, findings count, and status

### Requirement: Timeout enforcement
The system MUST kill a build or agent job that is still running after its configured timeout.

#### Scenario: Agent timeout
- **WHEN** an agent job stays in `AGENT_RUNNING` or `REVIEWING` past `agent.timeout`
- **THEN** the monitor marks the job failed and kills the Nomad allocation

#### Scenario: Build timeout
- **WHEN** a build job stays in `BUILDING` past `build_timeout` or in `TESTING` past `test_timeout`
- **THEN** the monitor marks the job failed and kills the Nomad allocation

### Requirement: Agent webhooks
The system MUST emit `agent.completed` when an agent job succeeds and `agent.failed` when it fails.

#### Scenario: Successful agent job
- **WHEN** an agent job transitions to `SUCCEEDED`
- **THEN** a webhook is sent with event `agent.completed` when a webhook URL is configured
