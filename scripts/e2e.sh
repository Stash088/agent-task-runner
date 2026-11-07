#!/usr/bin/env bash
#
# End-to-end check for agent-task-runner.
#
# Deploys the kustomization, port-forwards the service, drives an MCP session
# over streamable HTTP, runs one task, polls it to completion, verifies the
# worker log, and cancels it. Safe to run repeatedly.
#
# Required: kubectl, curl, a reachable cluster, and either:
#   - API_KEY + OPENCODE_GO_API_KEY in the environment (secret is upserted), or
#   - an existing Secret (default: agent-task-llm) with API_KEY.
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAMESPACE="${NAMESPACE:-agent-tasks}"
SERVICE="${SERVICE:-agent-task-runner}"
DEPLOYMENT="${DEPLOYMENT:-agent-task-runner}"
SECRET_NAME="${SECRET_NAME:-agent-task-llm}"
LOCAL_PORT="${LOCAL_PORT:-18080}"
POLL_INTERVAL="${POLL_INTERVAL:-5}"
TASK_TIMEOUT_SECONDS="${TASK_TIMEOUT_SECONDS:-900}"
PROMPT="${PROMPT:-Reply with exactly: E2E-OK}"
BASE_URL="http://127.0.0.1:${LOCAL_PORT}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}
need kubectl
need curl

TMP_E2E="$(mktemp -d)"
HEADERS_FILE="$TMP_E2E/headers"
PF_LOG="$TMP_E2E/port-forward.log"
PF_PID=""

log() { printf '[e2e] %s\n' "$*" >&2; }

cleanup() {
  if [[ -n "$PF_PID" ]]; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_E2E"
}
trap cleanup EXIT

# --- deploy -----------------------------------------------------------------

log "applying manifests"
kubectl apply -k "$REPO_ROOT/deploy" >/dev/null

# Upsert the secret only when the provider key is supplied. Nothing secret is
# ever read from the repository.
if [[ -n "${OPENCODE_GO_API_KEY:-}" ]]; then
  : "${API_KEY:?API_KEY must be set when OPENCODE_GO_API_KEY is provided}"
  log "upserting secret $SECRET_NAME"
  kubectl -n "$NAMESPACE" create secret generic "$SECRET_NAME" \
    --from-literal=API_KEY="$API_KEY" \
    --from-literal=OPENCODE_GO_API_KEY="$OPENCODE_GO_API_KEY" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi

if [[ -z "${API_KEY:-}" ]]; then
  log "reading API_KEY from secret $SECRET_NAME"
  API_KEY="$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" \
    -o jsonpath='{.data.API_KEY}' 2>/dev/null | base64 --decode 2>/dev/null || true)"
fi
if [[ -z "${API_KEY:-}" ]]; then
  echo "API_KEY is empty: export API_KEY and OPENCODE_GO_API_KEY, or create secret $SECRET_NAME" >&2
  exit 1
fi

log "waiting for deployment rollout"
kubectl -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout=180s >/dev/null

# --- port-forward -----------------------------------------------------------

log "port-forwarding $SERVICE to $BASE_URL"
kubectl -n "$NAMESPACE" port-forward --address 127.0.0.1 "svc/$SERVICE" \
  "${LOCAL_PORT}:8080" >"$PF_LOG" 2>&1 &
PF_PID=$!

ready=0
for _ in $(seq 1 30); do
  if curl -fsS --max-time 2 "$BASE_URL/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$PF_PID" 2>/dev/null; then
    cat "$PF_LOG" >&2
    exit 1
  fi
  sleep 1
done
if [[ "$ready" != 1 ]]; then
  cat "$PF_LOG" >&2
  echo "healthz did not become ready" >&2
  exit 1
fi

# --- MCP helpers ------------------------------------------------------------

SESSION_ID=""
PROTOCOL_VERSION=""
CALL_ID=0

status_of() {
  awk 'toupper($1) ~ /^HTTP\// {code=$2} END {print code}' "$HEADERS_FILE"
}

# Print the JSON body, unwrapping a single SSE data event if necessary.
extract_json() {
  local body
  body="$(cat)"
  if [[ "$body" == \{* ]]; then
    printf '%s' "$body"
    return
  fi
  printf '%s\n' "$body" |
    awk '/^data: / {sub(/^data: /, ""); last=$0} END {if (last != "") print last}'
}

mcp_post() {
  local payload="$1"
  local args=(
    -sS
    -D "$HEADERS_FILE"
    --connect-timeout 5
    --max-time "${MCP_TIMEOUT_SECONDS:-60}"
    -H "Content-Type: application/json"
    -H "Accept: application/json, text/event-stream"
    -H "X-API-Key: $API_KEY"
  )
  if [[ -n "$SESSION_ID" ]]; then
    args+=(-H "Mcp-Session-Id: $SESSION_ID")
  fi
  if [[ -n "$PROTOCOL_VERSION" ]]; then
    args+=(-H "MCP-Protocol-Version: $PROTOCOL_VERSION")
  fi

  local body http_status
  if ! body="$(curl "${args[@]}" --data-binary "$payload" "$BASE_URL/mcp")"; then
    echo "curl failed for MCP request" >&2
    return 1
  fi
  http_status="$(status_of)"
  if [[ -n "$http_status" && "$http_status" -ge 400 ]]; then
    echo "MCP HTTP $http_status: $body" >&2
    return 1
  fi
  printf '%s' "$body" | extract_json
}

mcp_request() {
  local method="$1"
  local params="${2:-}"
  CALL_ID=$((CALL_ID + 1))
  local payload
  if [[ -n "$params" ]]; then
    payload="{\"jsonrpc\":\"2.0\",\"id\":$CALL_ID,\"method\":\"$method\",\"params\":$params}"
  else
    payload="{\"jsonrpc\":\"2.0\",\"id\":$CALL_ID,\"method\":\"$method\"}"
  fi
  mcp_post "$payload"
}

mcp_tool() {
  local name="$1"
  local args="$2"
  mcp_request "tools/call" "{\"name\":\"$name\",\"arguments\":$args}"
}

json_string_field() {
  local body="$1"
  local field="$2"
  printf '%s' "$body" |
    grep -o "\"$field\":\"[^\"]*\"" |
    head -n 1 |
    sed 's/^[^:]*:"//; s/"$//' || true
}

is_error() { grep -Eq '"isError"[[:space:]]*:[[:space:]]*true'; }

# --- initialize -------------------------------------------------------------

log "initializing MCP session"
INIT_BODY="$(mcp_post '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1.0"}}}')"
SESSION_ID="$(grep -i '^Mcp-Session-Id:' "$HEADERS_FILE" |
  head -n 1 |
  sed 's/^[^:]*:[[:space:]]*//; s/\r$//' || true)"
PROTOCOL_VERSION="$(json_string_field "$INIT_BODY" protocolVersion)"
if [[ -z "$PROTOCOL_VERSION" ]]; then
  PROTOCOL_VERSION="2025-06-18"
fi
if [[ -z "$SESSION_ID" ]]; then
  echo "initialize did not return Mcp-Session-Id: $INIT_BODY" >&2
  exit 1
fi
log "session $SESSION_ID (protocol $PROTOCOL_VERSION)"

# The initialized notification has no id and returns 202 with an empty body.
mcp_post '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null

# --- task_run ---------------------------------------------------------------

log "calling task_run"
RUN_BODY="$(mcp_tool task_run "{\"prompt\":\"$PROMPT\"}")"
if printf '%s' "$RUN_BODY" | is_error; then
  echo "task_run failed: $RUN_BODY" >&2
  exit 1
fi
TASK_ID="$(json_string_field "$RUN_BODY" task_id)"
if [[ -z "$TASK_ID" ]]; then
  echo "task_run returned no task_id: $RUN_BODY" >&2
  exit 1
fi
log "task $TASK_ID started"

# --- poll status ------------------------------------------------------------

deadline=$((SECONDS + TASK_TIMEOUT_SECONDS))
state=""
while ((SECONDS < deadline)); do
  STATUS_BODY="$(mcp_tool task_status "{\"task_id\":\"$TASK_ID\"}")"
  if printf '%s' "$STATUS_BODY" | is_error; then
    echo "task_status failed: $STATUS_BODY" >&2
    exit 1
  fi
  state="$(json_string_field "$STATUS_BODY" state)"
  case "$state" in
    succeeded) break ;;
    failed)
      echo "task $TASK_ID failed: $STATUS_BODY" >&2
      exit 1
      ;;
    unknown)
      echo "task $TASK_ID became unknown before completing: $STATUS_BODY" >&2
      exit 1
      ;;
  esac
  sleep "$POLL_INTERVAL"
done
if [[ "$state" != "succeeded" ]]; then
  echo "task $TASK_ID did not succeed within ${TASK_TIMEOUT_SECONDS}s (last state: $state)" >&2
  exit 1
fi
log "task $TASK_ID succeeded"

# --- result -----------------------------------------------------------------

log "reading task_result"
RESULT_BODY="$(mcp_tool task_result "{\"task_id\":\"$TASK_ID\"}")"
if printf '%s' "$RESULT_BODY" | is_error; then
  echo "task_result failed: $RESULT_BODY" >&2
  exit 1
fi
if ! printf '%s' "$RESULT_BODY" | grep -q 'E2E-OK'; then
  echo "task_result does not contain E2E-OK: $RESULT_BODY" >&2
  exit 1
fi
log "task_result contains E2E-OK"

# --- cancel -----------------------------------------------------------------

log "cancelling task"
CANCEL_BODY="$(mcp_tool task_cancel "{\"task_id\":\"$TASK_ID\"}")"
if printf '%s' "$CANCEL_BODY" | is_error; then
  echo "task_cancel failed: $CANCEL_BODY" >&2
  exit 1
fi
if ! printf '%s' "$CANCEL_BODY" | grep -Eq '"deleted"[[:space:]]*:[[:space:]]*true'; then
  echo "task_cancel did not delete the job: $CANCEL_BODY" >&2
  exit 1
fi

for _ in $(seq 1 30); do
  if ! kubectl -n "$NAMESPACE" get job "$TASK_ID" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if kubectl -n "$NAMESPACE" get job "$TASK_ID" >/dev/null 2>&1; then
  echo "job $TASK_ID still exists after cancel" >&2
  exit 1
fi

log "E2E OK"
