#!/usr/bin/env bash
# Pre-mint N unique JWTs and write them line-by-line to a file.
# The k6 burst script reads this file at startup and each VU uses
# a different JWT (so each VU has a unique user_id/sub and therefore
# a unique hold key in Redis).
#
# Usage:
#   ./loadtest/mint-jwts.sh           # default 1000 JWTs
#   ./loadtest/mint-jwts.sh 500       # 500 JWTs
#   JWT_FILE=/tmp/custom.txt ./loadtest/mint-jwts.sh 1000

set -euo pipefail

N=${1:-1000}
JWT_FILE=${JWT_FILE:-/tmp/k6_jwts.txt}
USER_PREFIX=${USER_PREFIX:-k6user}
EVENT_ID=${EVENT_ID:-1}
TTL_SECONDS=${TTL_SECONDS:-120}

# Make sure the binary exists
if [ ! -x bin/mintjwt ]; then
  echo "▶ Building cmd/mintjwt..."
  go build -o bin/mintjwt ./cmd/mintjwt
fi

echo "▶ Minting $N JWTs to $JWT_FILE (user prefix: $USER_PREFIX, event: $EVENT_ID)..."
> "$JWT_FILE"  # truncate
for i in $(seq 1 "$N"); do
  USER_ID="${USER_PREFIX}-${i}" \
  EVENT_ID="$EVENT_ID" \
  TTL_SECONDS="$TTL_SECONDS" \
    ./bin/mintjwt 2>/dev/null   # discard mintjwt's stderr status messages
  echo ""                        # newline separator so the file is line-oriented
done >> "$JWT_FILE"

echo "Wrote $N JWTs ($(wc -l < "$JWT_FILE" | tr -d ' ') lines, $(wc -c < "$JWT_FILE" | tr -d ' ') bytes)"
echo "   sample: $(head -c 80 "$JWT_FILE")..."
