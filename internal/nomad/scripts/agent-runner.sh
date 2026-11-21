#!/bin/bash
# jobforge agent runner: clone, implement, review, push, open a pull request.
# URL and Authorization rules match internal/githost.Provider.
# Rendered into the task directory by a Nomad template with non-default delimiters.
set -euo pipefail

emitted=0
RESULT_STATUS="failed"
RESULT_BRANCH=""
RESULT_PR_URL=""
RESULT_VERDICT=""
RESULT_ITERATIONS=0
RESULT_FINDINGS=0
RESULT_DOCKERFILE="false"
WATCHDOG_PID=""
BRANCH=""
TOKEN=""
review_id=""

emit_result() {
  if [ "$emitted" = "1" ]; then
    return 0
  fi
  emitted=1
  jq -nc \
    --arg branch "$RESULT_BRANCH" \
    --arg pr_url "$RESULT_PR_URL" \
    --arg verdict "$RESULT_VERDICT" \
    --arg status "$RESULT_STATUS" \
    --argjson iterations "${RESULT_ITERATIONS:-0}" \
    --argjson findings_count "${RESULT_FINDINGS:-0}" \
    --argjson dockerfile_present "${RESULT_DOCKERFILE}" \
    '{branch:$branch,pr_url:$pr_url,verdict:$verdict,iterations:$iterations,findings_count:$findings_count,status:$status,dockerfile_present:$dockerfile_present}' \
    | while IFS= read -r line; do
        printf 'JOBFORGE_RESULT %s\n' "$line"
      done
}

on_exit() {
  local rc=$?
  if [ -n "${WATCHDOG_PID}" ]; then
    kill "$WATCHDOG_PID" 2>/dev/null || true
  fi
  if [ "$rc" -ne 0 ]; then
    RESULT_STATUS="failed"
  fi
  emit_result
  exit "$rc"
}
trap on_exit EXIT

STEP_TIMEOUT="${STEP_TIMEOUT:-20m}"
SR_MAX_ITERATIONS="${SR_MAX_ITERATIONS:-2}"
SR_CHECK="${SR_CHECK:-local}"
SR_ON_INSUFFICIENT="${SR_ON_INSUFFICIENT:-fail}"
OPEN_PR="${OPEN_PR:-true}"
FIX_DISPOSITIONS="${FIX_DISPOSITIONS:-[\"confirmed\"]}"
FIX_SEVERITIES="${FIX_SEVERITIES:-[\"critical\",\"high\"]}"
FIX_BLOCKS_MERGE="${FIX_BLOCKS_MERGE:-true}"
export XDG_DATA_HOME=/alloc/sr
export SOURCED_REVIEW_CONFIG=/alloc/sr/config.json
if [ -n "${SR_CONCURRENCY:-}" ]; then
  export SOURCED_REVIEW_CONCURRENCY="$SR_CONCURRENCY"
fi
if [ -n "${SR_MAX_OUTPUT_TOKENS:-}" ]; then
  export SOURCED_REVIEW_MAX_OUTPUT_TOKENS="$SR_MAX_OUTPUT_TOKENS"
fi

mkdir -p /alloc /work /alloc/sr
cd /work

if [ -n "${AGENT_TIMEOUT:-}" ]; then
  (
    sleep "$AGENT_TIMEOUT"
    echo "AGENT_TIMEOUT ${AGENT_TIMEOUT} exceeded"
    kill -TERM 0
  ) &
  WATCHDOG_PID=$!
fi

echo "JOBFORGE_PHASE AGENT_RUNNING"
echo "agent job ${JOB_ID} provider=${OUTPUT_PROVIDER} base=${BASE_BRANCH}"

if [ -f /secrets/git-creds ]; then
  set -a
  # shellcheck disable=SC1091
  . /secrets/git-creds
  set +a
fi

TOKEN="${!TOKEN_ENV:-}"
if [ -z "$TOKEN" ]; then
  TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-${GITEA_TOKEN:-${GIT_TOKEN:-${GIT_PASSWORD:-}}}}}"
fi
if [ -z "$TOKEN" ]; then
  echo "no git token in \$${TOKEN_ENV} or GH_TOKEN, GITHUB_TOKEN, GITEA_TOKEN, GIT_TOKEN, GIT_PASSWORD"
  exit 1
fi

AUTH_URL="$(
  JF_PROVIDER="$OUTPUT_PROVIDER" JF_URL="$REPO_URL" JF_TOKEN="$TOKEN" python3 - << 'PY'
import os, urllib.parse
provider = os.environ["JF_PROVIDER"]
raw = os.environ["JF_URL"]
token = os.environ["JF_TOKEN"]
parts = urllib.parse.urlsplit(raw)
if parts.scheme not in ("https", "http"):
    raise SystemExit("authenticated clone requires an http(s) repo_url")
if provider == "github":
    user = "x-access-token"
elif provider == "gitea":
    user = "oauth2"
else:
    raise SystemExit("unknown provider: " + provider)
host = parts.hostname or ""
if parts.port:
    host = "%s:%s" % (host, parts.port)
netloc = "%s:%s@%s" % (
    urllib.parse.quote(user, safe=""),
    urllib.parse.quote(token, safe=""),
    host,
)
print(urllib.parse.urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment)))
PY
)"

short_id="${JOB_ID//[^A-Za-z0-9._-]/-}"
short_id="${short_id:0:12}"
BRANCH="${BRANCH_PREFIX}jobforge-${short_id}"
RESULT_BRANCH="$BRANCH"

git config --global user.email "jobforge-agent@users.noreply.local"
git config --global user.name "jobforge-agent"
git config --global commit.gpgsign false
git config --global --add safe.directory /work/repo

echo "=== STEP clone ==="
timeout --signal=TERM "$STEP_TIMEOUT" git clone "$AUTH_URL" /work/repo
timeout --signal=TERM "$STEP_TIMEOUT" git -C /work/repo checkout "$GIT_REF"
timeout --signal=TERM "$STEP_TIMEOUT" git -C /work/repo fetch origin "$BASE_BRANCH"
timeout --signal=TERM "$STEP_TIMEOUT" git -C /work/repo checkout -b "$BRANCH"

mkdir -p /work/repo/.git/info
if ! grep -qxF 'knowledge/reviews/' /work/repo/.git/info/exclude 2>/dev/null; then
  echo 'knowledge/reviews/' >> /work/repo/.git/info/exclude
fi

if [ -n "${TASK_B64:-}" ]; then
  printf '%s' "$TASK_B64" | base64 -d > /alloc/task-body.md
fi

{
  echo "Implement the task below in this repository."
  echo "Run the project's tests yourself and fix failures before you finish."
  echo "Do not use Docker."
  echo
  if [ -n "${TASK_PATH:-}" ]; then
    cat "/work/repo/${TASK_PATH}"
  else
    cat /alloc/task-body.md
  fi
} > /alloc/task.md

echo "=== STEP review-setup ==="
configure_args=(
  configure
  --provider "$SR_PROVIDER"
  --model "$SR_MODEL"
  --credential-env "$SR_CREDENTIAL_ENV"
  --catalog-root "$SR_CATALOG"
)
if [ -n "${SR_BASE_URL:-}" ]; then
  configure_args+=(--base-url "$SR_BASE_URL")
fi
timeout --signal=TERM "$STEP_TIMEOUT" sourced-review "${configure_args[@]}"
timeout --signal=TERM "$STEP_TIMEOUT" sourced-review catalog trust --repo /work/repo

run_opencode() {
  local prompt_file="$1"
  local message="$2"
  if [ -n "${AGENT_COMMAND:-}" ] && [ "$prompt_file" = "/alloc/task.md" ]; then
    TASK_FILE="$prompt_file" REPO=/work/repo timeout --signal=TERM "$STEP_TIMEOUT" bash -c "$AGENT_COMMAND"
    return 0
  fi
  if [ -n "${AGENT_MODEL:-}" ]; then
    timeout --signal=TERM "$STEP_TIMEOUT" opencode run --standalone --auto --model "$AGENT_MODEL" --file "$prompt_file" "$message"
  else
    timeout --signal=TERM "$STEP_TIMEOUT" opencode run --standalone --auto --file "$prompt_file" "$message"
  fi
}

commit_if_dirty() {
  local msg="$1"
  git -C /work/repo add -A
  if git -C /work/repo diff --cached --quiet; then
    return 1
  fi
  git -C /work/repo commit -m "$msg"
}

echo "=== STEP implement ==="
run_opencode /alloc/task.md "Implement the attached task. Run the project's tests yourself and fix failures. Do not use Docker."
if ! commit_if_dirty "jobforge agent: implement task"; then
  echo "agent produced no commits"
  exit 1
fi

normalize_verdict() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//'
}

echo "JOBFORGE_PHASE REVIEWING"

i=1
while [ "$i" -le "$SR_MAX_ITERATIONS" ]; do
  echo "JOBFORGE_PHASE REVIEWING"
  echo "=== STEP review-${i} ==="
  review_file="/alloc/review-${i}.json"
  set +e
  timeout --signal=TERM "$STEP_TIMEOUT" sourced-review run \
    --repo /work/repo \
    --branch "$BRANCH" \
    --base "origin/${BASE_BRANCH}" \
    --check "$SR_CHECK" \
    --no-tui \
    --allow-local-changes \
    --context-file /alloc/task.md \
    > "$review_file"
  review_rc=$?
  set -e
  if [ "$review_rc" -eq 124 ]; then
    echo "review timed out"
    exit 1
  fi
  if ! jq -e . "$review_file" >/dev/null 2>&1; then
    echo "sourced-review did not write JSON (exit ${review_rc})"
    cat "$review_file" || true
    exit 1
  fi

  verdict="$(jq -r '.verdict // empty' "$review_file")"
  review_id="$(jq -r '.id // empty' "$review_file")"
  RESULT_VERDICT="$verdict"
  RESULT_ITERATIONS="$i"
  norm="$(normalize_verdict "$verdict")"
  echo "review ${i} verdict: ${verdict} (exit ${review_rc})"

  case "$norm" in
    ready)
      RESULT_STATUS="succeeded"
      break
      ;;
    "ready with warnings"|ready_with_warnings)
      RESULT_STATUS="ready_with_warnings"
      break
      ;;
    "insufficient evidence"|insufficient_evidence)
      if [ "$SR_ON_INSUFFICIENT" = "warn" ]; then
        echo "insufficient evidence; continuing because on_insufficient_evidence=warn"
        RESULT_STATUS="insufficient_evidence"
        break
      fi
      echo "insufficient evidence"
      exit 1
      ;;
    "changes required"|changes_required)
      report="/work/repo/.git/sourced-review/${review_id}/report.json"
      if [ ! -f "$report" ]; then
        state_path="$(jq -r '.state // empty' "$review_file")"
        if [ -n "$state_path" ] && [ -f "$state_path" ]; then
          report="$state_path"
        fi
      fi
      if [ -f "$report" ]; then
        jq --argjson disps "$FIX_DISPOSITIONS" \
           --argjson sevs "$FIX_SEVERITIES" \
           --argjson blocks "$([ "$FIX_BLOCKS_MERGE" = "true" ] && echo true || echo false)" \
           '[ .state.synthesis.candidates[]?
              | select(.disposition as $d | $disps | index($d) != null)
              | select(
                  ((.severity as $s | $sevs | index($s) != null))
                  or ($blocks == true and .blocks_merge == true)
                )
            ]' "$report" > "/alloc/findings-${i}.json"
      else
        echo "review report.json not found for ${review_id}"
        echo '[]' > "/alloc/findings-${i}.json"
      fi
      count="$(jq 'length' "/alloc/findings-${i}.json")"
      RESULT_FINDINGS=$((RESULT_FINDINGS + count))
      if [ "$i" -ge "$SR_MAX_ITERATIONS" ] || [ "$count" -eq 0 ]; then
        echo "stopping review loop (iteration ${i}, matching findings ${count})"
        RESULT_STATUS="changes_required"
        break
      fi
      {
        echo "Review iteration ${i} returned Changes required."
        echo "Fix each finding, or explain why it should not change in fix-notes.md."
        echo "Run the project's tests yourself. Do not use Docker."
        echo
        cat "/alloc/findings-${i}.json"
      } > "/alloc/fix-prompt-${i}.md"
      echo "=== STEP fix-${i} ==="
      run_opencode "/alloc/fix-prompt-${i}.md" "Fix the findings in the attached file, or explain exceptions in fix-notes.md. Run tests. Do not use Docker."
      commit_if_dirty "jobforge agent: address review ${i}" || echo "fix iteration ${i} produced no commit"
      ;;
    *)
      echo "unrecognized review verdict: ${verdict}"
      exit 1
      ;;
  esac
  i=$((i + 1))
done

if [ -n "${FOLLOWUP_DOCKERFILE:-}" ] && [ -f "/work/repo/${FOLLOWUP_DOCKERFILE}" ]; then
  RESULT_DOCKERFILE="true"
fi

echo "=== STEP push ==="
timeout --signal=TERM "$STEP_TIMEOUT" git -C /work/repo push -u origin "$BRANCH"

if [ "$OPEN_PR" = "true" ]; then
  echo "=== STEP pull-request ==="
  {
    echo "## Jobforge agent"
    echo
    echo "- Verdict: ${RESULT_VERDICT:-unknown}"
    echo "- Iterations: ${RESULT_ITERATIONS}"
    echo "- Findings: ${RESULT_FINDINGS}"
    echo "- Branch: ${BRANCH}"
    echo
    echo "## Review report"
    echo
    report_md="/work/repo/knowledge/reviews/${review_id}.md"
    if [ -n "$review_id" ] && [ -f "$report_md" ]; then
      head -c 40000 "$report_md"
      echo
    else
      echo "_No markdown report was found at knowledge/reviews/${review_id}.md._"
    fi
  } > /alloc/pr-body.md

  title_line="$(head -n 1 /alloc/task.md | tr -d '\r' | cut -c1-72)"
  pr_title="jobforge: ${title_line}"
  payload="$(jq -nc --arg title "$pr_title" --arg head "$BRANCH" --arg base "$BASE_BRANCH" --rawfile body /alloc/pr-body.md \
    '{title:$title,head:$head,base:$base,body:$body}')"

  case "$OUTPUT_PROVIDER" in
    github)
      api_base="${API_URL:-https://api.github.com}"
      api_base="${api_base%/}"
      pr_url="${api_base}/repos/${REPO_OWNER}/${REPO_NAME}/pulls"
      auth_header="Authorization: Bearer ${TOKEN}"
      accept_header="Accept: application/vnd.github+json"
      ;;
    gitea)
      api_base="${API_URL%/}"
      api_base="${api_base%/api/v1}"
      pr_url="${api_base}/api/v1/repos/${REPO_OWNER}/${REPO_NAME}/pulls"
      auth_header="Authorization: token ${TOKEN}"
      accept_header="Accept: application/json"
      ;;
    *)
      echo "unknown output provider ${OUTPUT_PROVIDER}"
      exit 1
      ;;
  esac

  http_code="$(timeout --signal=TERM "$STEP_TIMEOUT" curl -sS -o /alloc/pr-response.json -w '%{http_code}' \
    -X POST "$pr_url" \
    -H "$auth_header" \
    -H "$accept_header" \
    -H "Content-Type: application/json" \
    --data "$payload")"
  if [ "$http_code" -lt 200 ] || [ "$http_code" -ge 300 ]; then
    echo "pull request create failed: HTTP ${http_code}"
    cat /alloc/pr-response.json || true
    exit 1
  fi
  RESULT_PR_URL="$(jq -r '.html_url // .url // empty' /alloc/pr-response.json)"
  echo "opened pull request ${RESULT_PR_URL}"
fi

if [ "$RESULT_ITERATIONS" -eq 0 ] || [ "$RESULT_STATUS" = "failed" ]; then
  echo "agent pipeline finished without a review result"
  exit 1
fi
exit 0
