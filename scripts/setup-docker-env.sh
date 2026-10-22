#!/usr/bin/env bash
# Create .env.docker from .env.docker.example + your host .env (if any).
#
# Why this exists:
#   .env (host)  is gitignored — holds JWT_SECRET + dev creds.
#   .env.docker (the thing compose reads via --env-file) is also gitignored.
#   But .env.docker.example IS committed and ships sane defaults.
#
#   This script does the one-time setup so the dockerised stack just works.
#   Run it once per clone, or after wiping .env.docker.
#
# Strategy:
#   1. If .env.docker already exists → exit (don't clobber).
#   2. Copy .env.docker.example → .env.docker.
#   3. Replace the placeholder JWT_SECRET with one of (in order):
#        a. The JWT_SECRET from your host .env (so host mintjwt tokens
#           verify against the dockerised API).
#        b. A fresh random secret via `openssl rand -hex 32`.
#
#   Either way the JWT_SECRET in .env.docker is now ≥ 32 bytes and the
#   API will start cleanly. Idempotent.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

EXAMPLE=.env.docker.example
TARGET=.env.docker

if [ ! -f "$EXAMPLE" ]; then
  echo "✗ $EXAMPLE is missing — clone looks broken." >&2
  exit 1
fi

if [ -f "$TARGET" ]; then
  echo "✓ $TARGET already exists — leaving it alone (delete it first if you want a fresh one)."
  exit 0
fi

cp "$EXAMPLE" "$TARGET"

# Pick a JWT_SECRET. Priority:
#   1. From host .env (same secret across both runtimes = tokens minted by
#      `make mintjwt` on the host verify against the dockerised API).
#   2. Generated fresh (dockerised-only workflow; host tools won't share tokens).
JWT_SECRET=""
if [ -f .env ]; then
  # Grab only the JWT_SECRET line — don't pull every var from .env into .env.docker,
  # because .env may have host-only values that don't apply inside containers.
  JWT_SECRET=$(awk -F= '/^JWT_SECRET=/ {sub(/^JWT_SECRET=/, "", $2); print $2; exit}' .env || true)
fi

if [ -z "$JWT_SECRET" ] || [ "${#JWT_SECRET}" -lt 32 ]; then
  JWT_SECRET=$(openssl rand -hex 32)
  echo "→ generated fresh JWT_SECRET (no usable one in host .env)"
fi

# Replace the placeholder line in-place. sed -i.bak is portable enough for macOS/Linux.
sed -i.bak "s|^JWT_SECRET=.*|JWT_SECRET=${JWT_SECRET}|" "$TARGET"
rm -f "$TARGET.bak"

chmod 600 "$TARGET"

echo "✓ $TARGET created — JWT_SECRET set. Next:"
echo "  docker compose --env-file $TARGET up -d"
