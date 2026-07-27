#!/usr/bin/env bash
# Mint an offline admin operator credential into admin_credentials.
# This is the primary admin principal class (not a buyer API key).
#
# Migration from legacy is_admin API keys:
#   1. Set MERC_ADMIN_CIDRS to operator management networks.
#   2. Run this script; store the printed key in the operator secret store.
#   3. Verify GET /admin/controls with the new key from an allowlisted IP.
#   4. Revoke break-glass rows: UPDATE api_keys SET revoked=true WHERE is_admin=true;
set -euo pipefail
cd "$(dirname "$0")/.." || exit 1

COMPOSE="${MERC_COMPOSE_FILE:-docker-compose.prod.yml}"
LABEL="${MERC_ADMIN_KEY_LABEL:-operator admin}"
command -v openssl >/dev/null 2>&1 || { echo "openssl not found" >&2; exit 1; }

KEY="cx_admin_$(openssl rand -hex 24)"
if command -v sha256sum >/dev/null 2>&1; then
  HASH="$(printf '%s' "$KEY" | sha256sum | cut -d' ' -f1)"
else
  HASH="$(printf '%s' "$KEY" | shasum -a 256 | cut -d' ' -f1)"
fi

# Escape single quotes for SQL literal.
SQL_LABEL="${LABEL//\'/\'\'}"

docker compose -f "$COMPOSE" exec -T postgres \
  psql -U cx -d cx -v ON_ERROR_STOP=1 -c \
  "INSERT INTO admin_credentials (key_hash, label, revoked)
   VALUES ('$HASH', '$SQL_LABEL', false);" >/dev/null

printf '\nADMIN OPERATOR KEY (shown once; not a buyer API key):\n\n    %s\n\n' "$KEY"
printf "Label: %s\n" "$LABEL"
printf "Require MERC_ADMIN_CIDRS on the control plane before using this key.\n"
printf "Revoke with: UPDATE admin_credentials SET revoked=true WHERE key_hash='%s';\n" "$HASH"
printf "After cutover, revoke legacy break-glass: UPDATE api_keys SET revoked=true WHERE is_admin=true;\n"
