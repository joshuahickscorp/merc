#!/usr/bin/env bash
# Run the technical tabletop / privacy / break-glass suite and emit a receipt
# whose booleans are derived from go test -json results — never hardcoded true.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
NAME="merc-technical-exercises-20260719"
PORT="${MERC_TECHNICAL_EXERCISE_PG_PORT:-55441}"
EVIDENCE="$ROOT/evidence/autonomous/technical-exercises.json"
PG_IMAGE='postgres:17@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d'
TEST_JSON="$ROOT/.artifacts/technical-exercises-go-test.jsonl"

REQUIRED_TESTS=(
  TestDSARDeletionTombstoneAndRestoreReplay
  TestSupportAndSecurityTechnicalTabletops
  TestPrivilegedAdminMutationsHaveCompleteAtomicAudit
  TestPrivilegedMutationIdempotentConcurrentReplay
  TestConcurrentNamedOperatorsRetainIndependentAttribution
  TestRevocationWinsRaceBeforePrivilegedMutation
  TestAdminMutationRollsBackWhenAuditInsertFails
  # Object erasure: the DSAR receipt's deletable_data_removed is derived from
  # this test's result, not asserted.  It needs MinIO as well as Postgres.
  TestBuyerObjectDeletionQueueAndSweep
)

for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY; do
  value="${!name:-}"
  case "$value" in sk_live_*|rk_live_*) echo "$name is live-class; refused before network access" >&2; exit 1 ;; esac
done
unset STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY STRIPE_WEBHOOK_SECRET

# TestBuyerObjectDeletionQueueAndSweep skips unless S3_* is set. Re-exec under
# the pinned MinIO sidecar so deletable_data_removed can be re-earned rather
# than silently skipped. If the caller already provided object storage, keep it.
if [ -z "${S3_ENDPOINT:-}" ]; then
  exec bash "$ROOT/scripts/with-isolated-test-storage.sh" bash "$ROOT/scripts/technical-exercises.sh" "$@"
fi

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
mkdir -p "$(dirname "$TEST_JSON")" "$(dirname "$EVIDENCE")"
RUN_RE="$(IFS='|'; echo "${REQUIRED_TESTS[*]}")"

set +e
(
  cd "$ROOT/control"
  MERC_TEST_DATABASE_URL="$DATABASE" go test ./... -count=1 -json \
    -run "^(${RUN_RE})$"
) >"$TEST_JSON"
test_status=$?
set -e

python3 "$ROOT/scripts/validate-authorization-matrix.py" >/dev/null

# Derive every receipt boolean from go test -json. No field may be true unless
# its owning test event reports Action=pass for that Test name.
python3 "$ROOT/scripts/derive-technical-exercises-receipt.py" \
  "$TEST_JSON" "$EVIDENCE" "$test_status"

printf 'PASS technical exercises receipt: %s\n' "$EVIDENCE"
