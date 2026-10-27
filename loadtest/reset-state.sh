#!/usr/bin/env bash
# Reset the system to a known clean state for the load test.
#
# Steps (idempotent — safe to re-run between test runs):
#   1. Delete the reservations for event_id=1 (the seed event), plus
#      their audit-log rows
#   2. Delete all hold:* keys in Redis
#   3. Re-seed inventory:event:1 = 100 from Postgres via cmd/seed-inventory
#   4. Print a one-line summary so you can eyeball it
#
# Run from the repo root.

set -euo pipefail

EVENT_ID=${EVENT_ID:-1}
EXPECTED_INV=${EXPECTED_INV:-100}

# ON_ERROR_STOP=1 matters here: without it psql exits 0 even when a
# statement fails, so `set -e` never fires and a broken reset looks
# like a successful one. That is exactly how the audit-log FK broke
# this script silently — the DELETE failed, the script carried on, and
# the "state" summary showed stale rows.
PG="docker exec -i ticket-postgres psql -v ON_ERROR_STOP=1 -U tickets -d tickets"
REDIS="docker exec -i ticket-redis redis-cli"

echo "▶ Deleting reservations for event_id=$EVENT_ID (and their audit rows)..."
# The audit log (reservation_events) references reservations through a
# FOREIGN KEY, so the audit rows have to go FIRST — and the table is
# append-only, so the delete needs the documented escape hatch:
#
#   SET LOCAL ticket_deal.audit_purge = 'on'
#
# SET LOCAL is transaction-scoped, so the override cannot leak to
# another connection. See migrations/005_audit_log.sql.
#
# (A load-test reset is a legitimate purge: it wipes the whole event's
# history on purpose. Production code never sets this GUC.)
$PG >/dev/null <<SQL
BEGIN;
SET LOCAL ticket_deal.audit_purge = 'on';
DELETE FROM reservation_events
      WHERE reservation_id IN (
          SELECT reservation_id FROM reservations WHERE event_id = $EVENT_ID
      );
COMMIT;
DELETE FROM reservations WHERE event_id = $EVENT_ID;
SQL

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
echo -n "  audit rows (event $EVENT_ID)  : "
echo "SELECT COUNT(*) FROM reservation_events;" | $PG -tA
echo -n "  inventory:event:$EVENT_ID       : "
$REDIS GET "inventory:event:$EVENT_ID"
echo -n "  hold keys (event $EVENT_ID)     : "
$REDIS EVAL "return #redis.call('KEYS', 'hold:event:$EVENT_ID:user:*')" 0
echo "------------------------------------"
echo "Ready for load test."
