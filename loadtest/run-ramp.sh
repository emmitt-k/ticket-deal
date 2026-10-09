#!/usr/bin/env bash
# Run k6 ramp test in background, saving output to a timestamped file in logs/ramp/.
# PID is saved to logs/k6-ramp.pid so `make stop-loadtest` still works.
#
# Self-bootstraps: runs reset-state.sh and mint-jwts.sh first so you
# don't have to remember to do it manually. Override behavior with env
# vars — anything passed to reset-state.sh / mint-jwts.sh:
#
#   EVENT_ID=2 ./loadtest/run-ramp.sh        # different event
#   VUS=2000  ./loadtest/run-ramp.sh        # more VUs (= more JWTs)
#   JWT_FILE=/tmp/jwts.txt ./loadtest/run-ramp.sh
#   K6_REST_ADDR=:6565 ./loadtest/run-ramp.sh
#
# The first positional arg, if present, is the JWT count (passed to mint-jwts.sh).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# ── Self-bootstrap: reset state + mint JWTs ────────────────────────
#
# Each step is idempotent: re-running reset-state.sh drops existing
# rows + holds and re-seeds inventory; mint-jwts.sh truncates the
# JWT file before writing fresh ones. So calling them on every
# invocation is safe and removes the "did I forget to reset?" footgun.

# 1) Reset Postgres reservations + Redis hold keys + re-seed inventory
echo "─── bootstrap: reset state ───"
./loadtest/reset-state.sh

# 2) Mint VUS fresh JWTs (default 1000; override with $VUS or $1)
N_JWT="${1:-${VUS:-1000}}"
echo ""
echo "─── bootstrap: mint JWTs ───"
./loadtest/mint-jwts.sh "$N_JWT"

# ── Launch k6 in background ───────────────────────────────────────
LOG_DIR="$REPO_ROOT/logs/ramp"
PID_FILE="$REPO_ROOT/logs/k6-ramp.pid"
mkdir -p "$LOG_DIR"

# Already running?
if [ -f "$PID_FILE" ]; then
  EXISTING_PID=$(cat "$PID_FILE")
  if kill -0 "$EXISTING_PID" 2>/dev/null; then
    echo ""
    echo "k6-ramp already running (PID $EXISTING_PID) — not restarting."
    echo "  → to re-run: make stop-loadtest && make loadtest-ramp"
    exit 0
  fi
  rm -f "$PID_FILE"
fi

TS=$(date +%Y-%m-%dT%H-%M-%S)
LOG_FILE="$LOG_DIR/${TS}-ramp.log"

echo ""
echo "─── launching k6 ramp ───"
nohup k6 run --address "${K6_REST_ADDR:-localhost:6565}" --linger loadtest/ramp.js > "$LOG_FILE" 2>&1 &
PID=$!
disown 2>/dev/null || true
echo "$PID" > "$PID_FILE"

echo "k6-ramp started (PID $PID)"
echo "log: $LOG_FILE"
