#!/usr/bin/env bash
# Start a binary in the background, save its PID, log to logs/<name>.log.
# Used by Makefile targets like `make api-bg`.
#
# Usage: scripts/start-bg.sh <name> <path-to-binary> [extra args...]
#   name:   short label used for log + pid files (e.g. "api", "worker")
#   binary: absolute or repo-relative path to the binary
#   args:   optional args passed to the binary
#
# Idempotent: if a process is already running under the saved PID, this is
# a no-op (prints the existing PID). If the PID file is stale (process
# gone), it cleans up the file and starts fresh.

set -euo pipefail

if [ $# -lt 2 ]; then
  echo "usage: $0 <name> <binary> [args...]" >&2
  exit 2
fi

NAME=$1
BINARY=$2
shift 2

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG_DIR="$REPO_ROOT/logs"
PID_FILE="$LOG_DIR/$NAME.pid"
LOG_FILE="$LOG_DIR/$NAME.log"

mkdir -p "$LOG_DIR"

# Already running?
if [ -f "$PID_FILE" ]; then
  EXISTING_PID=$(cat "$PID_FILE")
  if kill -0 "$EXISTING_PID" 2>/dev/null; then
    echo "$NAME already running (PID $EXISTING_PID), log: $LOG_FILE"
    exit 0
  else
    rm -f "$PID_FILE"
  fi
fi

# macOS doesn't ship `setsid`; nohup + & + disown is the standard pattern.
cd "$REPO_ROOT"
nohup "$BINARY" "$@" > "$LOG_FILE" 2>&1 &
PID=$!
disown 2>/dev/null || true
echo "$PID" > "$PID_FILE"

# Give it a beat to crash on a bad import / missing env, then verify.
sleep 0.3
if ! kill -0 "$PID" 2>/dev/null; then
  echo "$NAME crashed on startup; tail of log:" >&2
  tail -20 "$LOG_FILE" >&2
  rm -f "$PID_FILE"
  exit 1
fi

echo "$NAME started (PID $PID), log: $LOG_FILE"
