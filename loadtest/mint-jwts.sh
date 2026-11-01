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
#
# ── EVENT_ID is REQUIRED ──────────────────────────────────────────────
#
# The event id is a UUID minted by the application, and no event is
# seeded any more — so there is no id to default to. A default would
# mint a thousand tokens scoped to an event that does not exist, and
# every one of them would fail at the reserve step with a confusing
# "event_not_found".
#
# Get the id from the database:
#
#   docker exec -i ticket-postgres psql -U tickets -d tickets \
#     -c "SELECT id FROM events LIMIT 1"

set -euo pipefail

N=${1:-1000}
JWT_FILE=${JWT_FILE:-/tmp/k6_jwts.txt}
USER_PREFIX=${USER_PREFIX:-k6user}
EVENT_ID=${EVENT_ID:-}

if [ -z "$EVENT_ID" ]; then
    echo "ERROR: EVENT_ID is required — it is the UUID of the event to load-test." >&2
    echo "" >&2
    echo "  Get it with:" >&2
    echo "    docker exec -i ticket-postgres psql -U tickets -d tickets -c \"SELECT id FROM events LIMIT 1\"" >&2
    exit 1
fi
# 600s matches the reserve-hold TTL — long enough for the 90s ramp test plus
# repeated runs without re-minting. Override with TTL_SECONDS=...
TTL_SECONDS=${TTL_SECONDS:-600}

# Make sure the binary exists AND is current.
#
# The `if [ ! -x ... ]` check alone is a trap: a STALE binary from
# before the event id became a UUID parses EVENT_ID as an integer,
# fails, and writes an EMPTY jwt file — which the k6 script then
# reports as "no JWTs found". The failure is real but the message
# points at the wrong thing.
#
# So always rebuild. It takes a second and it means the binary can
# never disagree with the source.
echo "▶ Building cmd/mintjwt..."
go build -o bin/mintjwt ./cmd/mintjwt

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
