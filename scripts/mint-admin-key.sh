#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.." || exit 1

COMPOSE="${CX_COMPOSE_FILE:-docker-compose.prod.yml}"
command -v openssl >/dev/null 2>&1 || { echo "openssl not found" >&2; exit 1; }

KEY="cx_admin_$(openssl rand -hex 24)"
if command -v sha256sum >/dev/null 2>&1; then
  HASH="$(printf '%s' "$KEY" | sha256sum | cut -d' ' -f1)"
else
  HASH="$(printf '%s' "$KEY" | shasum -a 256 | cut -d' ' -f1)"
fi

docker compose -f "$COMPOSE" exec -T postgres \
  psql -U cx -d cx -v ON_ERROR_STOP=1 -c \
  "INSERT INTO api_keys (buyer_id, key_hash, is_admin, revoked, name)
   VALUES (gen_random_uuid(), '$HASH', true, false, 'operator admin');" >/dev/null

printf '\nADMIN KEY (shown once):\n\n    %s\n\n' "$KEY"
printf "Revoke with: UPDATE api_keys SET revoked=true WHERE name='operator admin';\n"
