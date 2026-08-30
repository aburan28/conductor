#!/usr/bin/env bash
#
# Stop the background control plane started by `make up` (scripts/up.sh).
# conductord shuts down gracefully on SIGTERM; Postgres keeps running —
# `make db-down` stops (and removes) the database.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIDFILE="$ROOT/.conductor/runtime/conductord.pid"

if [[ ! -s "$PIDFILE" ]]; then
  echo "no running control plane recorded (no pidfile in .conductor/runtime/)"
  exit 0
fi

PID="$(cat "$PIDFILE")"
if ! kill -0 "$PID" 2>/dev/null; then
  echo "control plane (pid $PID) is not running"
  rm -f "$PIDFILE"
  exit 0
fi

echo "stopping conductord (pid $PID)"
kill "$PID"
for _ in $(seq 1 40); do
  kill -0 "$PID" 2>/dev/null || break
  sleep 0.25
done
if kill -0 "$PID" 2>/dev/null; then
  echo "did not exit; sending SIGKILL" >&2
  kill -9 "$PID" 2>/dev/null || true
fi
rm -f "$PIDFILE"
echo "stopped. Postgres is still running — make db-down also stops it."
