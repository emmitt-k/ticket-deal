#!/usr/bin/env bash
# Clear (truncate or delete) service log files in logs/*.log.
#
# Default behaviour: TRUNCATE — set each log file's size to 0 bytes
# without removing the file itself. This preserves the inode and any
# open file descriptors, so services that are still writing to the
# file keep doing so without missing a beat. POSIX-portable; works
# the same on macOS and Linux.
#
# Pass --delete to actually `rm` the files (next time the service
# starts, start-bg.sh will recreate them).
#
# What this does NOT touch:
#   - logs/burst/   (k6 burst test results, by timestamp)
#   - logs/ramp/    (k6 ramp test results, by timestamp)
#   - logs/*.pid    (PID files for liveness checks; managed by
#                    start-bg.sh / stop-bg.sh)
#
# Usage:
#   scripts/clear-logs.sh                # truncate all logs/*.log
#   scripts/clear-logs.sh api worker     # only those two
#   scripts/clear-logs.sh --delete       # rm instead of truncate
#   scripts/clear-logs.sh --delete api   # rm only api.log
#
# If services are still running, the script prints a warning but
# proceeds — truncation is safe in-flight. Stop services first with
# `make stop` if you want a clean snapshot.
#
# Companion Makefile target: `make clean-logs`.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG_DIR="$REPO_ROOT/logs"

# ── Argument parsing ────────────────────────────────────────────
MODE="truncate"   # or "delete"
TARGETS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --delete|-d)
      MODE="delete"
      shift
      ;;
    --truncate|-t)
      MODE="truncate"
      shift
      ;;
    --help|-h)
      sed -n '2,30p' "$0"   # print the comment header
      exit 0
      ;;
    -*)
      echo "unknown flag: $1" >&2
      echo "try --help" >&2
      exit 2
      ;;
    *)
      TARGETS+=("$1")
      shift
      ;;
  esac
done

# ── Pre-flight ─────────────────────────────────────────────────
if [ ! -d "$LOG_DIR" ]; then
  echo "no $LOG_DIR directory — nothing to clear"
  exit 0
fi

# Resolve targets. If none given, grab every logs/*.log
# (NOT the burst/ and ramp/ subdirs, NOT the *.pid files).
if [ ${#TARGETS[@]} -eq 0 ]; then
  shopt -s nullglob
  for f in "$LOG_DIR"/*.log; do
    TARGETS+=("$(basename "$f" .log)")
  done
  shopt -u nullglob
fi

# Build the absolute paths and filter out non-existent ones.
PATHS=()
MISSING=()
for name in "${TARGETS[@]}"; do
  path="$LOG_DIR/$name.log"
  if [ -f "$path" ]; then
    PATHS+=("$path")
  else
    MISSING+=("$path")
  fi
done

if [ ${#PATHS[@]} -eq 0 ]; then
  if [ ${#MISSING[@]} -gt 0 ]; then
    echo "no log files matched; missing:" >&2
    for m in "${MISSING[@]}"; do echo "  $m" >&2; done
  else
    echo "no logs/*.log files found"
  fi
  exit 0
fi

# ── Plan + warn ────────────────────────────────────────────────
echo "mode: $MODE"
echo "about to $MODE:"
total_bytes=0
for f in "${PATHS[@]}"; do
  size=$(wc -c < "$f" 2>/dev/null | tr -d ' ' || echo 0)
  total_bytes=$((total_bytes + size))
  printf "  %-50s %10d bytes\n" "$f" "$size"
done
echo "total: $total_bytes bytes across ${#PATHS[@]} file(s)"

# Warn if any of the services behind these logs are still running.
# (Truncation is safe in-flight, but the user may want a clean
# snapshot — let them ctrl-c and run `make stop` first.)
warned=0
shopt -s nullglob
for pidf in "$LOG_DIR"/*.pid; do
  pid=$(cat "$pidf" 2>/dev/null || echo "")
  [ -n "$pid" ] || continue
  if kill -0 "$pid" 2>/dev/null; then
    if [ $warned -eq 0 ]; then
      echo ""
      echo "⚠️  services still running (PID files in $LOG_DIR/):"
      warned=1
    fi
    printf "   PID %-6s  (%s)\n" "$pid" "$(basename "$pidf")"
  fi
done
shopt -u nullglob
if [ $warned -eq 1 ]; then
  echo "   (truncation in-flight is safe; pass --delete to remove instead)"
fi

if [ ${#MISSING[@]} -gt 0 ]; then
  echo ""
  echo "skipped (not found):"
  for m in "${MISSING[@]}"; do echo "  $m"; done
fi

# ── Do it ──────────────────────────────────────────────────────
echo ""
for f in "${PATHS[@]}"; do
  case "$MODE" in
    truncate)
      # `: > file` is the POSIX-portable way to truncate a file to
      # zero bytes. Preserves inode, file mode, and any open FDs.
      : > "$f"
      ;;
    delete)
      rm -f -- "$f"
      ;;
  esac
done

echo "done: $MODE ${#PATHS[@]} file(s)"
