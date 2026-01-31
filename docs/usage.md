# JobForge Usage Guide

JobForge is a CLI tool for submitting Docker image build jobs to a remote build service. It handles building, testing, and publishing Docker images using Nomad as the backend infrastructure.

## Getting Help

The CLI has comprehensive built-in help:

```bash
jobforge --help
```

This displays all commands, options, and examples.

## Environment Setup

Set the service URL before using JobForge:

```bash
export JOB_SERVICE_URL=http://your-service-url:port
```

Or pass it directly:

```bash
jobforge -u http://your-service-url:port <command>
```

## Quick Examples

### Submit a Build Job

```bash
# Basic submission with YAML config
jobforge submit-job build.yaml

# With global configuration (for shared settings)
jobforge submit-job -global deploy/global.yaml build.yaml

# Watch job progress in real-time
jobforge submit-job build.yaml --watch
```

### Check Job Status

```bash
jobforge get-status <job-id>
```

### Get Job Logs

```bash
# All logs
jobforge get-logs <job-id>

# Phase-specific logs
jobforge get-logs --phase build <job-id>
jobforge get-logs --phase test <job-id>
jobforge get-logs --phase publish <job-id>
```

### Service Health

```bash
jobforge health
```

## Configuration Reference

For complete job configuration options (YAML schema, test configuration, resource limits, webhooks, etc.), see [JobSpec.md](JobSpec.md).

## Tips for Coding Agents

### JSON Output

All commands return JSON output suitable for programmatic parsing:

```bash
jobforge get-status <job-id> | jq '.status'
```

### Error Handling

- Non-zero exit codes indicate failure
- Error messages are written to stderr
- Use `--watch` for real-time progress and automatic failure detection

### Recommended Workflow

1. Submit job with `--watch` flag for immediate feedback
2. On failure, check logs for the failed phase
3. Fix issues and resubmit

### Exit Codes

- `0`: Success
- `1`: Error (check stderr for details)
