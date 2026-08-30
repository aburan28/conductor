#!/usr/bin/env bash
#
# One command from a cold checkout to a running, logged-in control plane:
# Postgres, the binaries, conductord in the background (log + pidfile under
# .conductor/runtime/), and a saved CLI login.
#
#   make up    → this script
#   make down  → stop the background server (Postgres keeps running)
#
# Overrides (same names the other make targets use):
#   ENDPOINT  control plane URL              (default http://localhost:8080)
#   ADDR      conductord --addr bind address (default 127.0.0.1:8080)
#   PROJECT   project to pin when re-using a saved token

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin"
RUNTIME="$ROOT/.conductor/runtime"
PIDFILE="$RUNTIME/conductord.pid"
LOGFILE="$RUNTIME/conductord.log"
TOKEN_FILE="$ROOT/.conductor/.bootstrap-token"
ENDPOINT="${ENDPOINT:-http://localhost:8080}"
export DATABASE_URL="${DATABASE_URL:-postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable}"

step() { printf '\033[1m==> %s\033[0m\n' "$*"; }

step "Postgres and binaries"
make -C "$ROOT" --no-print-directory db-up build
mkdir -p "$RUNTIME"

server_running() {
  [[ -s "$PIDFILE" ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

start_server() {
  if server_running; then
    echo "control plane already running (pid $(cat "$PIDFILE"))"
    return
  fi
  if curl -sf "$ENDPOINT/v1/health" >/dev/null 2>&1; then
    echo "control plane already serving at $ENDPOINT (not started by make up) — leaving it alone"
    return
  fi
  step "Starting conductord in the background"
  local args=()
  if [[ -n "${ADDR:-}" ]]; then
    args+=(--addr "$ADDR")
  fi
  nohup "$BIN/conductord" "${args[@]}" >"$LOGFILE" 2>&1 </dev/null &
  echo $! >"$PIDFILE"
  for _ in $(seq 1 60); do
    curl -sf "$ENDPOINT/v1/health" >/dev/null 2>&1 && return 0
    sleep 0.25
  done
  echo "control plane did not become healthy at $ENDPOINT — last log lines:" >&2
  tail -20 "$LOGFILE" >&2
  return 1
}

login_saved() {
  # A saved login counts only when it points at this endpoint and its token still
  # authenticates. Anything else falls through to a fresh login below.
  local creds="${HOME}/.conductor/credentials"
  [[ -s "$creds" ]] || return 1
  local endpoint_saved token
  endpoint_saved="$(sed -n 's/.*"endpoint": *"\([^"]*\)".*/\1/p' "$creds")"
  token="$(sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' "$creds")"
  if [[ "$endpoint_saved" != "$ENDPOINT" ]]; then
    echo "note: existing login points at $endpoint_saved — leaving it alone"
    return 0
  fi
  [[ -n "$token" ]] \
    && curl -sf -H "Authorization: Bearer $token" "$ENDPOINT/v1/whoami" >/dev/null 2>&1
}

ensure_login() {
  step "Login"
  if login_saved; then
    echo "already logged in ($HOME/.conductor/credentials)"
    return
  fi
  if [[ -s "$TOKEN_FILE" ]]; then
    # A previous bootstrap minted this token; reuse it instead of minting another.
    local project_args=()
    if [[ -n "${PROJECT:-}" ]]; then
      project_args+=(--project "$PROJECT")
    fi
    "$BIN/conductor" login --endpoint "$ENDPOINT" --token "$(cat "$TOKEN_FILE")" "${project_args[@]}"
    return
  fi
  # Cold start: create the org/project/principal, mint a token, and save the login.
  local out
  out="$("$BIN/conductord" bootstrap --repo "$ROOT" --endpoint "$ENDPOINT")"
  printf '%s\n' "$out"
  printf '%s' "$out" | grep -o 'cdt_[A-Za-z0-9_-]*' | head -1 >"$TOKEN_FILE" || true
}

start_server
ensure_login

step "Ready"
cat <<EOF
  endpoint    $ENDPOINT
  dashboard   $ENDPOINT/
  server log  $LOGFILE
  stop with   make down   (make db-down also stops Postgres)

  conductor status
  conductor wrap claude   # or: codex / opencode
EOF
