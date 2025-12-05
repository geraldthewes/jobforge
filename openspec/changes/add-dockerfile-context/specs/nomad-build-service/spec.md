## MODIFIED Requirements

### Requirement: Build Phase (FR2)

The build phase SHALL use Nomad batch jobs with the `quay.io/buildah/stable` image to clone Git repositories and execute `buildah bud`. The system SHALL support build caching via persistent host volume, temporary image tagging for test phase, and custom build context directory via `dockerfile_context` configuration.

#### Scenario: Build with default context (repository root)

- **WHEN** a job is submitted without `dockerfile_context` specified
- **THEN** the build command uses `.` (repository root) as the build context
- **AND** `COPY` commands in the Dockerfile resolve paths relative to the repository root

#### Scenario: Build with custom context directory

- **WHEN** a job is submitted with `dockerfile_context` set to a subdirectory (e.g., `nvidia-privacy-model-service/`)
- **THEN** the build command uses the specified directory as the build context
- **AND** `COPY` commands in the Dockerfile resolve paths relative to that subdirectory
- **AND** the Dockerfile path is still specified separately via `dockerfile_path`

#### Scenario: Subdirectory Dockerfile with relative COPY commands

- **WHEN** `dockerfile_path` is `subdir/Dockerfile` and `dockerfile_context` is `subdir/`
- **AND** the Dockerfile contains `COPY requirements.txt .`
- **THEN** the build successfully copies `subdir/requirements.txt` into the image
- **AND** the equivalent command is `buildah bud -f subdir/Dockerfile --tag <image> subdir/`
