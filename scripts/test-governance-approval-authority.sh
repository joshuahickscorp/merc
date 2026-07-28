#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

for command in git go jq shasum; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "governance approval authority test: missing $command" >&2
    exit 1
  }
done

fail() { echo "governance approval authority test: FAIL: $*" >&2; exit 1; }
tmp="$(mktemp -d "${TMPDIR:-/tmp}/merc-governance-approval-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
commit="$(git rev-parse HEAD)"
valid="$tmp/valid.json"

jq -n --arg candidate "$commit" '
  def approval($domain; $at): {
    status:"APPROVED",
    approver:("Qualified reviewer for " + $domain),
    organization:"Independent Review Organization",
    reviewed_scope:"Exact supervised test-mode private-canary candidate",
    evidence_uri:("s3://governance-evidence/" + $domain + ".json"),
    approved_at:$at
  };
  def exercise($name): {
    status:"PASS",
    evidence_uri:("s3://governance-evidence/" + $name + ".json"),
    completed_at:"2026-07-28T00:00:00Z"
  };
  {
    schema_version:1,
    candidate_commit:$candidate,
    scope:"supervised_stripe_test_mode_private_canary",
    approvals:{
      security:approval("security";"2026-07-28T01:00:00Z"),
      privacy:approval("privacy";"2026-07-28T01:00:00Z"),
      legal:approval("legal";"2026-07-28T01:00:00Z"),
      licensing:approval("licensing";"2026-07-28T01:00:00Z"),
      payments:approval("payments";"2026-07-28T01:00:00Z"),
      operations:approval("operations";"2026-07-28T01:00:00Z"),
      supplier_policy:approval("supplier_policy";"2026-07-28T01:00:00Z"),
      release_approval:approval("release_approval";"2026-07-28T02:00:00Z")
    },
    exercises:{
      support_tabletop:exercise("support_tabletop"),
      security_tabletop:exercise("security_tabletop"),
      dsar_export_deletion:exercise("dsar_export_deletion"),
      backup_tombstone:exercise("backup_tombstone"),
      asset_and_model_provenance:exercise("asset_and_model_provenance")
    }
  }' > "$valid"

env -i "PATH=$PATH" MERC_RELEASE_DOCTOR_NO_ENV_FILE=1 \
  GOVERNANCE_APPROVAL_BUNDLE_PATH="$valid" \
  bash scripts/release-doctor.sh --check governance >/dev/null \
  || fail "release doctor rejected the canonical approval schema"

cli_receipt="$(cd control && go run . release approvals-check --bundle "$valid")"
expected_sha="$(shasum -a 256 "$valid" | awk '{print $1}')"
jq -e --arg commit "$commit" --arg sha "$expected_sha" '
  .schema_version == 1 and
  .kind == "merc_governance_approval_validation" and
  .status == "PASS" and
  .candidate_commit == $commit and
  .approval_bundle_sha256 == $sha and
  .candidate_bound == true and
  .human_approvals == 8 and
  .technical_exercises == 5 and
  .secret_values_printed == false
' <<< "$cli_receipt" >/dev/null \
  || fail "approval CLI omitted its exact sanitized authority binding"

expect_doctor_fail() {
  local name="$1" filter="$2"
  jq "$filter" "$valid" > "$tmp/$name.json"
  if env -i "PATH=$PATH" MERC_RELEASE_DOCTOR_NO_ENV_FILE=1 \
    GOVERNANCE_APPROVAL_BUNDLE_PATH="$tmp/$name.json" \
    bash scripts/release-doctor.sh --check governance >/dev/null 2>&1; then
    fail "release doctor accepted $name governance mutation"
  fi
}

expect_doctor_fail legacy_domain_set '
  .approvals.license=.approvals.licensing |
  del(.approvals.licensing)'
expect_doctor_fail missing_approver '.approvals.legal.approver=""'
expect_doctor_fail whitespace_approver '.approvals.legal.approver="   "'
expect_doctor_fail pending_domain '.approvals.payments.status="PENDING"'
expect_doctor_fail fractional_timestamp '
  .approvals.security.approved_at="2026-07-28T01:00:00.000Z"'
expect_doctor_fail premature_release '
  .approvals.release_approval.approved_at="2026-07-27T23:00:00Z"'
expect_doctor_fail exercise_after_release '
  .exercises.security_tabletop.completed_at="2026-07-28T03:00:00Z"'
expect_doctor_fail wrong_candidate '.candidate_commit=("b" * 40)'

echo "governance-approval-authority: PASS (canonical domains, complete identities, ordered release approval)"
