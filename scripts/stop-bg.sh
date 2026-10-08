#!/usr/bin/env bash
# Stop a background process started by scripts/start-bg.sh.
#
# Usage: scripts/stop-bg.sh <name>
# Sends SIGINT, waits up to 5s, escalates to SIGKILL if still alive.
# Removes the PID file on success.
# No-op (with a friendly message) if the service isn't running.

set -euo pipefail

if [ $# -lt 1 ]; then
  echo "usage: $0 <name>" >&2
  exit 2
fi

NAME=$1
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PID_FILE="$REPO_ROOT/logs/$NAME.pid"

if [ ! -f "$PID_FILE" ]; then
  echo "$NAME not running (no PID file)"
  exit 0
fi

PID=$(cat "$PID_FILE")
if ! kill -0 "$PID" 2>/dev/null; then
  echo "$NAME not running (stale PID $PID, removing)"
  rm -f "$PID_FILE"
  exit 0
fi

# Polite shutdown first
kill -INT "$PID" 2>/dev/null || true
for _ in 1 2 3 4 5; do
  if ! kill -0 "$PID" 2>/dev/null; then break; fi
  sleep 1
done

# Escalate if still alive
if kill -0 "$PID" 2>/dev/null; then
  echo "$NAME did not stop on SIGINT; sending SIGKILL"
  kill -KILL "$PID" 2>/dev/null || true
  sleep 0.3
fi

rm -f "$PID_FILE"
echo "$NAME stopped (was PID $PID)"
