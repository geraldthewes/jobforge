## Context
Jobforge is a build → test → publish state machine. Agent work needs a different job shape: one checkout, one allocation, and a git host at the end.

## Goals / Non-Goals
- Goals: one batch allocation; opencode headless; sourced-review loop; GitHub and Gitea pull requests; backward-compatible build jobs.
- Non-Goals: multi-allocation stages, parallel agents, issue queues, cost accounting, an external log store, devpod integration.

## Decisions
- Decision: `kind` defaults to `build`. Agent jobs validate `agent`, `review`, and `output` instead of image fields.
- Decision: the runner is an embedded shell script. Git host HTTP calls live in `internal/githost` and are mirrored by the script.
- Decision: opencode is `opencode run --standalone --auto`. Provider keys are environment variables from Vault.
- Decision: review verdicts come from the JSON document, not the process exit code.
- Decision: Consul log values keep a head and a tail under 256KB.
- Decision: the monitor kills build and agent jobs that pass `build_timeout`, `test_timeout`, or `agent.timeout`.

## Risks / Trade-offs
- OpenCode flags and the `@opencode/cli` package name follow the published docs and can move.
- `review.concurrency` and `review.max_output_tokens` are exported to the process and are not passed as CLI flags, because those flags were not part of the verified `sourced-review run` command.
- A follow-up build clones with git credentials when `/secrets/git-creds` exists. Public clones are unchanged.

## Migration Plan
Existing job specs omit `kind` and keep the current pipeline. No stored job rewrite is required.

## Open Questions
- None for the MVP. Multi-allocation fan-out stays deferred.
