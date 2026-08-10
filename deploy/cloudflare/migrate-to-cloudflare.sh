#!/usr/bin/env bash
# Move merc off the droplet onto Cloudflare + managed Postgres, and PROVE the
# money semantics survived the move.
#
# The proof is not optional and not a formality: the control plane rests on 191
# FOR UPDATE, 45 SKIP LOCKED, 25 pg_advisory_* and 55 NUMERIC(12,6) columns. A
# provider that silently lacks one of those breaks money correctness in a way no
# smoke test would show. The isolated-DB suite is what catches it, so this script
# refuses to proceed past step 2 until that suite is green on the NEW database.
#
#   deploy/cloudflare/migrate-to-cloudflare.sh <postgres-url>
#
# Never point this at D1. D1 is SQLite: no row locks, no SKIP LOCKED, no exact
# decimal. The money path would fall back to float, which it explicitly refuses.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PG="${1:-}"
[[ -n "$PG" ]] || { echo "usage: $0 postgres://user:pass@host/db" >&2; exit 2; }
case "$PG" in postgres://*|postgresql://*) ;; *) echo "not a postgres url: refusing" >&2; exit 2;; esac

echo "== 1. preflight: does this endpoint have the primitives merc needs?"
psql "$PG" -v ON_ERROR_STOP=1 -qtA <<'SQL'
SELECT 'advisory_lock_ok=' || (pg_try_advisory_lock(hashtextextended('merc-preflight',0)))::text;
SELECT pg_advisory_unlock_all();
SELECT 'numeric_exact_ok=' || ((1.000001::NUMERIC(12,6) + 0.000001::NUMERIC(12,6)) = 1.000002::NUMERIC(12,6))::text;
SELECT 'version=' || split_part(version(),' ',2);
SQL

echo "== 2. apply schema"
psql "$PG" -v ON_ERROR_STOP=1 -q -f "$ROOT/control/schema.sql" >/dev/null
echo "   schema applied"

echo "== 3. THE GATE: full isolated-DB suite against the new endpoint"
# Not a subset. This suite is the reason the move can be trusted.
( cd "$ROOT" && MERC_TEST_DATABASE_URL="$PG" make test )
echo "   suite green on the new database"

echo "== 4. Hyperdrive"
echo "   wrangler hyperdrive create merc-pg --connection-string \"$PG\""
echo "   then put the returned id in deploy/cloudflare/wrangler.jsonc"

echo "== 5. deploy the control plane (needs Workers Paid for Containers)"
echo "   cd deploy/cloudflare && wrangler deploy"

echo
echo "Data migration is deliberately NOT automated here. Dumping and restoring a"
echo "LIVE money database is an operator decision with a maintenance window, and"
echo "the droplet currently runs live Stripe on 727-commit-stale code (G063)."
echo "Settle that first; a migration that carries the stale state forward just"
echo "moves the exposure to a new host."
