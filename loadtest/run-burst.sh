#!/usr/bin/env bash
# Run k6 burst test in background, saving output to a timestamped file in logs/burst/.
# PID is saved to logs/k6-burst.pid so `make stop-loadtest` still works.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG_DIR="$REPO_ROOT/logs/burst"
PID_FILE="$REPO_ROOT/logs/k6-burst.pid"
mkdir -p "$LOG_DIR"

# Already running?
if [ -f "$PID_FILE" ]; then
  EXISTING_PID=$(cat "$PID_FILE")
  if kill -0 "$EXISTING_PID" 2>/dev/null; then
    echo "k6-burst already running (PID $EXISTING_PID)"
    exit 0
  fi
  rm -f "$PID_FILE"
fi

TS=$(date +%Y-%m-%dT%H-%M-%S)
LOG_FILE="$LOG_DIR/${TS}-burst.log"

cd "$REPO_ROOT"
nohup k6 run --address "${K6_REST_ADDR:-localhost:6565}" --linger loadtest/burst.js > "$LOG_FILE" 2>&1 &
PID=$!
disown 2>/dev/null || true
echo "$PID" > "$PID_FILE"

echo "k6-burst started (PID $PID)"
echo "log: $LOG_FILE"
