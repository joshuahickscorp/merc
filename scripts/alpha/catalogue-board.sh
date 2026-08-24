#!/usr/bin/env bash
# Diagnose why GET /pricing/board.json 503s, and print the operator input
# that unblocks the live plane without loosening catalogue authority.
#
#   scripts/alpha/catalogue-board.sh --diagnose
#   scripts/alpha/catalogue-board.sh --print-runbook
#
# --diagnose is HTTPS + an optional read-only droplet query. It never writes
# activation policy or restarts control. A catalogue republish alone cannot
# un-quarantine a drifted document seed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=scripts/alpha/lib.sh
. "$ROOT/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/alpha/catalogue-board.sh --diagnose|--print-runbook
USAGE
  exit 2
}

print_runbook() {
  cat <<'EOF'
# Catalogue board 503 — cause and operator input
#
# GET /pricing/board.json → Server.handlePriceBoardData →
# loadCurrentPublicCatalogue (control/api.go). The 503 string is
# "current catalogue price authority unavailable".
#
# The staging catalogue IS published: models.llama-3.2-1b-instruct-q4
# points at schedule 86e62a72… citing
# evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r6.json
# engine_build_hash=f4210c0ef62e4490. The r7 receipt in the image is BOUND (r6 retired: outlier rate + unreproducible binary).
#
# loadCurrentPublicCatalogue walks activation.advertised, not that schedule.
# The revision-1 document seed for candle-metal-llama1-infer still names
# promotion_receipt=…/candle-metal-llama1-q4-r4.json. CapabilityDigest
# excludes benchmark_authority, so the digest still matches, the row is
# applied, storedRoutableEntryHasCurrentGlobalAuthority fails, and the
# previous reader quarantined the cell. Advertised set empty → 503.
# Do not flip lifecycle by hand. Do not loosen the citation gate.
#
# Operator input (exactly one of):
#
# 1. Deploy a control binary that drops a drifted document seed and falls
#    back to the current document (control/pricing_publication.go +
#    activationSnapshotFrom). Restart is enough; no SQL.
#
# 2. Without a new binary, write a NEW document-sourced activation revision
#    whose candle-metal-llama1-infer promotion_receipt equals the current
#    document (…/candle-metal-llama1-q4-r6.json), lifecycle=ACTIVE, digest
#    and profile_revision unchanged. source MUST stay 'document'. The next
#    GET /pricing/board.json refreshes from PostgreSQL and will serve the
#    already-published r6 schedule. Do not UPDATE the r1 row in place —
#    equal epochs keep the process cache.
#
# Catalogue republish (ApplyRepricing) is a no-op: the r6 schedule is
# already the current pointer. /readyz must stay 200 and payment_mode=test.
EOF
}

diagnose() {
  local host code body version ready
  alpha_require_command curl
  alpha_require_command jq
  host="$(alpha_staging_host)"
  version="$(curl -sS --proto '=https' --tlsv1.2 --max-time 20 "https://$host/version")"
  ready="$(curl -sS --proto '=https' --tlsv1.2 --max-time 20 "https://$host/readyz")"
  body="$(mktemp)"
  code="$(curl -sS --proto '=https' --tlsv1.2 --max-time 20 \
    -o "$body" -w '%{http_code}' "https://$host/pricing/board.json")"
  printf 'host=%s board_http=%s\n' "$host" "$code"
  jq -c '{commit,price_board_sha256,price_board_source,modified}' <<<"$version"
  jq -c '{status,payment_mode,live_value_movement}' <<<"$ready"
  if [ "$code" = 200 ]; then
    jq -c '{price_authority_status:.price_authority.status,schedule:.price_authority.schedule_sha256,n:(.price_authority.catalogue|length)}' "$body"
  else
    cat "$body"
    printf '\n'
  fi
  rm -f "$body"

  if [ -x "$ROOT/scripts/alpha/remote.py" ]; then
    python3 "$ROOT/scripts/alpha/remote.py" \
      "docker exec merc-postgres-1 psql -U cx -d cx -c \"
SELECT m.id, m.price_source, left(m.price_schedule_sha256,16) AS sched
  FROM models m ORDER BY id;
SELECT cell_id, lifecycle, routable, promotion_receipt
  FROM runtime_activation_policies
 WHERE runtime_profile_id='candle_metal'
 ORDER BY cell_id;\"" || true
  fi
}

case "${1:-}" in
  --print-runbook) print_runbook ;;
  --diagnose) diagnose ;;
  *) usage ;;
esac
