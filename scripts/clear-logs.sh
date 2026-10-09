#!/usr/bin/env bash
# Delete (or truncate) log files in logs/.
#
# Default behaviour: DELETE — actually `rm` each log file. Use this when
# you want a true clean slate (e.g. before a fresh load test, after a
# debugging session). The next time a service starts, start-bg.sh
# recreates the .log file; the .pid file is left alone (lives in the
# same dir but is a different file).
#
# Pass --truncate to set each file's size to 0 bytes instead, keeping
# the inode and any open file descriptors intact. Useful if you want
# to keep the file but reset its content while services are mid-flight
# (services keep writing to the same FD without missing a beat).
#
# What this targets by default (no args):
#   - logs/*.log            (api, worker, expiration-watcher, etc.)
#   - logs/burst/*          (k6 burst test result files)
#   - logs/ramp/*           (k6 ramp test result files)
#
# What this does NOT touch:
#   - logs/*.pid            (PID files for liveness checks; managed
#                            by start-bg.sh / stop-bg.sh)
#   - the burst/ and ramp/  DIRECTORIES themselves (only their
#     contents)
#
# Usage:
#   scripts/clear-logs.sh                  # delete all log files
#                                          #   (api.log + worker.log + ... + burst/* + ramp/*)
#   scripts/clear-logs.sh --truncate      # truncate instead of delete
#   scripts/clear-logs.sh api worker      # only those two service logs
#   scripts/clear-logs.sh burst           # only k6 burst results
#   scripts/clear-logs.sh api burst       # api log + all k6 burst results
#
# If services are still running, the script prints a warning but
# proceeds — both delete and truncate are safe in-flight (services
# keep their open FD on the deleted-then-recreated log, or on the
# truncated-to-zero log). Stop services with `make stop` first if
# you want a clean snapshot.
#
# Companion Makefile target: `make clean-logs`.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG_DIR="$REPO_ROOT/logs"

# ── Argument parsing ────────────────────────────────────────────
MODE="delete"   # default per Master: actually delete, not just truncate
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
      sed -n '2,38p' "$0"   # print the comment header
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

# Resolve targets → absolute paths.
# Each arg is either:
#   - a service name (e.g. "api")        → logs/api.log
#   - a directory under logs/ (e.g. "burst") → all files inside logs/burst/
PATHS=()
MISSING=()

if [ ${#TARGETS[@]} -eq 0 ]; then
  # No args: grab everything we manage.
  # - all logs/*.log (skip .pid files)
  shopt -s nullglob
  for f in "$LOG_DIR"/*.log; do
    PATHS+=("$f")
  done
  for d in "$LOG_DIR/burst" "$LOG_DIR/ramp"; do
    if [ -d "$d" ]; then
      for f in "$d"/*; do
        [ -f "$f" ] && PATHS+=("$f")
      done
    fi
  done
  shopt -u nullglob
else
  for name in "${TARGETS[@]}"; do
    # First try: it's a service log
    if [ -f "$LOG_DIR/$name.log" ]; then
      PATHS+=("$LOG_DIR/$name.log")
      continue
    fi
    # Second try: it's a directory under logs/
    if [ -d "$LOG_DIR/$name" ]; then
      found_any=0
      for f in "$LOG_DIR/$name"/*; do
        if [ -f "$f" ]; then
          PATHS+=("$f")
          found_any=1
        fi
      done
      if [ $found_any -eq 0 ]; then
        MISSING+=("$LOG_DIR/$name/ (exists but is empty)")
      fi
      continue
    fi
    # Neither
    MISSING+=("$name (no logs/$name.log or logs/$name/ directory found)")
  done
fi

# ── Plan + warn ────────────────────────────────────────────────
echo "mode: $MODE"
if [ ${#PATHS[@]} -eq 0 ]; then
  if [ ${#MISSING[@]} -gt 0 ]; then
    echo "no log files matched; missing:" >&2
    for m in "${MISSING[@]}"; do echo "  $m" >&2; done
  else
    echo "no log files found in $LOG_DIR/"
  fi
  exit 0
fi

echo "about to $MODE:"
total_bytes=0
for f in "${PATHS[@]}"; do
  size=$(wc -c < "$f" 2>/dev/null | tr -d ' ' || echo 0)
  total_bytes=$((total_bytes + size))
  printf "  %-70s %10d bytes\n" "$f" "$size"
done
echo "total: $total_bytes bytes across ${#PATHS[@]} file(s)"

# Warn if any of the services behind these logs are still running.
# (Both delete and truncate are safe in-flight — services keep their
# FD on the recreated / truncated file — but the user may want a
# clean snapshot. Let them ctrl-c and run `make stop` first.)
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
  echo "   (in-flight delete/truncate is safe; services keep their FD)"
fi

if [ ${#MISSING[@]} -gt 0 ]; then
  echo ""
  echo "skipped:"
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
      # rm -f so non-existent files (race with another process)
      # don't cause a non-zero exit. The `--` is a safety net: any
      # path starting with `-` would otherwise be interpreted as a
      # flag, not a file. (Unlikely in our case but cheap insurance.)
      rm -f -- "$f"
      ;;
  esac
done

echo "done: $MODE ${#PATHS[@]} file(s)"
