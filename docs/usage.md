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

### Lock Management

```bash
# List all build locks
jobforge list-locks

# List only stale locks (where holding job is completed/missing)
jobforge list-locks --stale-only

# List locks for a specific owner
jobforge list-locks --owner gerald

# Force unlock a specific lock (prompts for confirmation)
jobforge force-unlock <lock-key>

# Force unlock without confirmation
jobforge force-unlock <lock-key> --force

# Clear all stale locks
jobforge force-unlock --stale-only

# Clear ALL locks (requires --force)
jobforge force-unlock --all --force

# List active jobs for an owner
jobforge list-active-jobs --owner gerald
```

### Storage Management

The buildah storage cache at `/opt/nomad/data/buildah-cache` can accumulate over time and consume disk space. Use the prune-storage command to clean it up.

```bash
# Preview what would be cleaned (recommended first step)
jobforge prune-storage --dry-run

# Conservative prune (default) - removes dangling images and caches >24h old
jobforge prune-storage

# Watch prune job progress
jobforge prune-storage --watch

# Prune only a specific project's cache
jobforge prune-storage --project myapp

# Aggressive prune - removes ALL cached images and layers
# Warning: This will slow down subsequent builds significantly
jobforge prune-storage --all --force

# Prune on all Nomad nodes (uses sysbatch job)
jobforge prune-storage --all-nodes

# Aggressive prune across all nodes
jobforge prune-storage --all --all-nodes --force
```

**Important Notes:**
- Use `--dry-run` first to see what would be cleaned
- Conservative prune keeps recent layers for faster rebuilds
- Aggressive prune (`--all`) clears everything - use sparingly
- The `--force` flag is required for aggressive prune when active builds are detected
- Use `--all-nodes` when storage issues are cluster-wide

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

## Troubleshooting

### Lock Issues

Build locks prevent concurrent builds of the same image. Sometimes locks can become "stuck" if a build crashes without releasing its lock.

**Symptoms:**
- New builds fail with "failed to acquire lock" or similar errors
- Jobs stay in PENDING status indefinitely

**Investigation:**

```bash
# List all locks to see what's held
jobforge list-locks

# Example output:
LOCK KEY                                    JOB ID        OWNER      IMAGE              BRANCH   AGE      STALE
image-registry.cluster-5000-myapp-main      abc123def4    team-a     myapp              main     10m      No
image-registry.cluster-5000-readerlm-main   def456abc7    gerald     readerlm-litserve  main     2h       Yes (job completed)

# Check for stale locks specifically
jobforge list-locks --stale-only
```

**Resolution:**

```bash
# Clear a specific stale lock
jobforge force-unlock image-registry.cluster-5000-readerlm-main --force

# Clear all stale locks at once
jobforge force-unlock --stale-only
```

### Per-Owner Build Limits

The service enforces a limit on concurrent builds per owner (default: 3). This prevents runaway automation from consuming all build capacity.

**Symptoms:**
- Build submission fails with HTTP 429 error
- Error message: "Owner 'X' has N active builds (limit: N)"

**Investigation:**

```bash
# Check your active builds
jobforge list-active-jobs --owner <your-owner-name>

# Example output:
JOB ID        STATUS       IMAGE              STARTED      LOCK KEY
abc123def4    BUILDING     myapp              5m ago       image-registry.cluster-5000-myapp-main
def456abc7    TESTING      readerlm-litserve  15m ago      image-registry.cluster-5000-readerlm-main
```

**Resolution:**

1. Wait for existing builds to complete
2. Kill builds that are no longer needed: `jobforge kill-job <job-id>`
3. Check for stale locks that might be counting against your limit

### Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| "failed to acquire lock" | Another build is using the same image/branch combination | Wait for the other build to complete or use `list-locks` to investigate |
| "Owner has N active builds (limit: N)" | You've hit the concurrent build limit | Wait for builds to complete or kill unnecessary jobs |
| "lock not found" | Trying to unlock a lock that doesn't exist | Use `list-locks` to see current locks |
| "job not found" | The job associated with a lock no longer exists | The lock is stale; use `force-unlock --stale-only` |
