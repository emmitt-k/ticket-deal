#!/usr/bin/env bash
# Reset the system to a known clean state for the load test.
#
# Steps (idempotent — safe to re-run between test runs):
#   1. Truncate reservations for event_id=1 (the seed event)
#   2. Delete all hold:* keys in Redis
#   3. Re-seed inventory:event:1 = 100 from Postgres via cmd/seed-inventory
#   4. Print a one-line summary so you can eyeball it
#
# Run from the repo root.

set -euo pipefail

EVENT_ID=${EVENT_ID:-1}
EXPECTED_INV=${EXPECTED_INV:-100}

PG="docker exec -i ticket-postgres psql -U tickets -d tickets"
REDIS="docker exec -i ticket-redis redis-cli"

echo "▶ Truncating reservations for event_id=$EVENT_ID..."
echo "DELETE FROM reservations WHERE event_id = $EVENT_ID;" | $PG >/dev/null

echo "▶ Deleting all hold: keys..."
# SCAN to avoid blocking Redis on large keyspaces; COUNT=500 keeps it snappy.
$REDIS --no-raw EVAL "
  local keys = redis.call('KEYS', 'hold:event:$EVENT_ID:user:*')
  if #keys > 0 then redis.call('DEL', unpack(keys)) end
  return #keys
" 0

echo "▶ Re-seeding inventory from Postgres..."
go run ./cmd/seed-inventory

echo ""
echo "--------------- STATE ---------------"
echo -n "  reservations (event $EVENT_ID): "
echo "SELECT COUNT(*) FROM reservations WHERE event_id = $EVENT_ID;" | $PG -tA
echo -n "  inventory:event:$EVENT_ID       : "
$REDIS GET "inventory:event:$EVENT_ID"
echo -n "  hold keys (event $EVENT_ID)     : "
$REDIS EVAL "return #redis.call('KEYS', 'hold:event:$EVENT_ID:user:*')" 0
echo "------------------------------------"
echo "Ready for load test."
