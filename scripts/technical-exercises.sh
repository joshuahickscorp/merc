#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
NAME="cx-technical-exercises-20260719"
PORT="${CX_TECHNICAL_EXERCISE_PG_PORT:-55441}"
EVIDENCE="$ROOT/evidence/autonomous/technical-exercises.json"
PG_IMAGE='postgres:17@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d'

for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY; do
  value="${!name:-}"
  case "$value" in sk_live_*|rk_live_*) echo "$name is live-class; refused before network access" >&2; exit 1 ;; esac
done
unset STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY STRIPE_WEBHOOK_SECRET

docker inspect "$NAME" >/dev/null 2>&1 && { echo "container $NAME already exists" >&2; exit 1; }
cleanup() { docker stop "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
docker run --rm -d --name "$NAME" -e POSTGRES_USER=cx -e POSTGRES_PASSWORD=cx \
  -e POSTGRES_DB=cx -p "127.0.0.1:$PORT:5432" "$PG_IMAGE" >/dev/null
for _ in {1..60}; do
  pg_isready -h 127.0.0.1 -p "$PORT" -U cx >/dev/null 2>&1 && break
  sleep 1
done
pg_isready -h 127.0.0.1 -p "$PORT" -U cx >/dev/null 2>&1 || { echo 'PostgreSQL not ready' >&2; exit 1; }

DATABASE="postgres://cx:cx@127.0.0.1:$PORT/cx?sslmode=disable"
(cd "$ROOT/control" && CX_TEST_DATABASE_URL="$DATABASE" go test ./... -count=1 -run \
  '^(TestDSARDeletionTombstoneAndRestoreReplay|TestSupportAndSecurityTechnicalTabletops|TestPrivilegedAdminMutationsHaveCompleteAtomicAudit|TestPrivilegedMutationIdempotentConcurrentReplay|TestConcurrentNamedOperatorsRetainIndependentAttribution|TestRevocationWinsRaceBeforePrivilegedMutation|TestAdminMutationRollsBackWhenAuditInsertFails)$')
python3 "$ROOT/scripts/validate-authorization-matrix.py" >/dev/null

mkdir -p "$(dirname "$EVIDENCE")"
temporary="$EVIDENCE.tmp.$$"
jq -n --arg completed "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
  {schema_version:1,status:"PASS",completed_at:$completed,
   dsar:{export_complete:true,foreign_data_absent:true,secrets_redacted:true,
     artifacts_in_manifest:true,payment_records:true,provenance:true},
   deletion:{deletable_data_removed:true,minimal_financial_retention:true,
     artifact_expiry_harness:true,authentication_revoked:true,foreign_keys_valid:true},
   tombstone:{authentication_denied:true,submission_denied:true,private_retrieval_denied:true,
     silent_recreation_denied:true,minimal_identity_retained:true,pre_deletion_restore_replayed:true},
   provenance:{request:true,input:true,quote:true,submission:true,tasks:true,agent_identity:true,
     model_revision:true,attempt:true,result:true,verification:true,finalization:true,
     ledger:true,webhook:true,retrieval:true},
   support_tabletop:{status:"PASS",settlement_mismatch_detected:true,reconciled_to_immutable_ledger:true,
     safe_correction_proposal:true,communication_draft:true,postmortem_required:true},
   security_tabletop:{status:"PASS",cross_tenant_access:false,intake_pause_procedure:true,
     token_revocation:true,leaked_url_issued:false,url_presign_denied_after_owner_check:true,
     canonical_artifact_reference_revoked:true,audit_export:true,
     containment:true,recovery_intake_resumed:true,severity_if_access_succeeded:"P0"},
   break_glass:{status:"PASS",named_operator:true,reason:true,incident_reference:true,
     before_after:true,immutable_audit:true,unauthorized_rejected:true,audit_failure_aborts:true,
     concurrent_operators:true,idempotent_retries:true},
   qualification:{technical_tabletop:"PASS",qualified_human_tabletop:"NOT EXECUTED",
     external_subprocessor_deletion:"NOT EXECUTED"}}' > "$temporary"
mv "$temporary" "$EVIDENCE"
printf 'PASS technical exercises receipt: %s\n' "$EVIDENCE"
