#!/usr/bin/env bash
# Create an event through the admin API and print its id.
#
# This is the missing piece that makes the load test usable with zero
# setup. There is no seeded event any more (item 9a deleted the seed),
# so a load test needs an event — and until now the only way to get
# one was to write SQL by hand.
#
# The event is created through the SAME admin API an organizer would
# use, which means:
#
#   - the inventory is seeded into Redis (the event is immediately
#     sellable)
#   - the price default applies ($25.00)
#   - the id is minted by the server
#
# Usage:
#   ./loadtest/create-event.sh                      # 100 seats, default name
#   ./loadtest/create-event.sh 250                  # 250 seats
#   ./loadtest/create-event.sh 100 "Midnight Echo"  # named
#
#   EVENT_ID=$(./loadtest/create-event.sh 100)
#
# The admin API needs an HMAC signature over the request, so this
# script signs it the same way the server verifies it:
#
#   signature = HMAC-SHA256(secret, method + "\n" + uri + "\n" + body + "\n" + timestamp)
#
# Env:
#   ADMIN_API_KEY   the admin secret (from .env.docker or .env)
#   API_BASE_URL    default http://localhost:8080

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

SEATS=${1:-100}
NAME=${2:-"Load Test Event $(date +%H:%M:%S)"}
API_BASE_URL=${API_BASE_URL:-http://localhost:8080}

# ── The admin key ──────────────────────────────────────────────────────
#
# It comes from the same env file the API reads, so the key and the
# server always agree. ADMIN_API_KEY wins if it is set.
if [ -z "${ADMIN_API_KEY:-}" ]; then
    for envfile in .env.docker .env; do
        if [ -f "$envfile" ]; then
            # grep the first ADMIN_API_KEYS= line and take the first
            # comma-separated value.
            ADMIN_API_KEY=$(grep -m1 '^ADMIN_API_KEYS=' "$envfile" | cut -d= -f2- | cut -d, -f1 | tr -d ' "'"'"'')
            if [ -n "$ADMIN_API_KEY" ]; then
                break
            fi
        fi
    done
fi

if [ -z "${ADMIN_API_KEY:-}" ]; then
    echo "ERROR: no admin key found." >&2
    echo "" >&2
    echo "  The event CRUD lives behind the admin API's HMAC signature." >&2
    echo "  Set ADMIN_API_KEY, or add ADMIN_API_KEYS=<secret> to .env.docker" >&2
    echo "  (the same file the API reads)." >&2
    exit 1
fi

# ── Sign and send ──────────────────────────────────────────────────────
#
# The signature covers the method, the FULL uri (query string
# included), the body and a timestamp — the server verifies exactly
# that, and a mismatch is refused.
URI="/admin/events"
BODY="{\"name\":\"$NAME\",\"initial_inventory\":$SEATS}"
TS=$(date +%s)
SIG=$(printf 'POST\n%s\n%s\n%s' "$URI" "$BODY" "$TS" \
    | openssl dgst -sha256 -hmac "$ADMIN_API_KEY" -hex | awk '{print $NF}')

RESP=$(curl -sS -X POST "$API_BASE_URL$URI" \
    -H "X-Admin-Key: $SIG" \
    -H "X-Admin-Timestamp: $TS" \
    -H "Content-Type: application/json" \
    -d "$BODY")

# The id is the only thing the caller needs. Print just that, so
# `EVENT_ID=$(./loadtest/create-event.sh)` works.
#
# The error case is handled first: an error body has an "error" key
# and no "event".
if echo "$RESP" | grep -q '"error"'; then
    echo "ERROR: the admin API refused the event creation:" >&2
    echo "$RESP" >&2
    exit 1
fi

EVENT_ID=$(echo "$RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
if [ -z "$EVENT_ID" ]; then
    echo "ERROR: could not read the event id out of the response:" >&2
    echo "$RESP" >&2
    exit 1
fi

echo "$EVENT_ID"
