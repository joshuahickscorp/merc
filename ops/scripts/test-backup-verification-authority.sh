#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

for command in jq python3 shasum; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "backup verification authority test: missing $command" >&2
    exit 1
  }
done

fail() { echo "backup verification authority test: FAIL: $*" >&2; exit 1; }
tmp="$(mktemp -d "${TMPDIR:-/tmp}/merc-backup-verification-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

backup_id='20260728T100001Z'
not_before='2026-07-28T10:00:00Z'
created_at='2026-07-28T10:00:02Z'
verified_at='2026-07-28T10:00:03Z'
offsite='s3://merc-backup-authority-test/private'
ciphertext="$tmp/backup.tar.age"
printf 'synthetic encrypted bytes for authority testing only\n' > "$ciphertext"
ciphertext_sha="$(shasum -a 256 "$ciphertext" | awk '{print $1}')"
ciphertext_bytes="$(wc -c < "$ciphertext" | tr -d ' ')"

manifest="$tmp/manifest.json"
jq -n \
  --arg id "$backup_id" --arg sha "$ciphertext_sha" \
  --arg created "$created_at" --arg offsite "$offsite/$backup_id" \
  --argjson bytes "$ciphertext_bytes" '
  {
    schema_version:2,
    kind:"merc_encrypted_offsite_backup",
    backup_id:$id,
    cipher:"age-x25519",
    ciphertext_sha256:$sha,
    ciphertext_bytes:$bytes,
    created_at:$created,
    database:"merc",
    objects_included:true,
    offsite_uri:$offsite
  }' > "$manifest"
manifest_sha="$(shasum -a 256 "$manifest" | awk '{print $1}')"

receipt="$tmp/verification.json"
jq -n \
  --arg id "$backup_id" --arg offsite "$offsite/$backup_id" \
  --arg manifest_sha "$manifest_sha" --arg ciphertext_sha "$ciphertext_sha" \
  --arg verified "$verified_at" --argjson bytes "$ciphertext_bytes" '
  {
    schema_version:1,
    kind:"merc_offsite_backup_verification",
    status:"PASS",
    backup_id:$id,
    offsite_uri:$offsite,
    manifest_sha256:$manifest_sha,
    ciphertext:{
      manifest_sha256:$ciphertext_sha,
      downloaded_sha256:$ciphertext_sha,
      bytes:$bytes
    },
    verified_at:$verified,
    checks:{
      offsite_bundle_visible:true,
      independent_manifest_download:true,
      independent_ciphertext_download:true,
      manifest_checksum_match:true,
      ciphertext_checksum_match:true
    },
    policy:{
      encrypted_before_upload:true,
      plaintext_uploaded:false,
      secret_values_recorded:false
    }
  }' > "$receipt"

validator=(
  python3 ops/scripts/validate-backup-verification-receipt.py
  "$manifest"
  "$receipt"
  --ciphertext "$ciphertext"
  --offsite-base "$offsite"
  --not-before "$not_before"
  --checked-at "$verified_at"
)
"${validator[@]}" >/dev/null \
  || fail "valid fresh offsite verification receipt was rejected"

expect_receipt_fail() {
  local name="$1" filter="$2"
  jq "$filter" "$receipt" > "$tmp/$name.json"
  if python3 ops/scripts/validate-backup-verification-receipt.py \
    "$manifest" "$tmp/$name.json" \
    --ciphertext "$ciphertext" --offsite-base "$offsite" \
    --not-before "$not_before" --checked-at "$verified_at" >/dev/null 2>&1; then
    fail "$name receipt mutation was accepted"
  fi
}

expect_receipt_fail wrong_backup_id '.backup_id="20260728T100002Z"'
expect_receipt_fail wrong_offsite '.offsite_uri="s3://other-bucket/private/20260728T100001Z"'
expect_receipt_fail wrong_manifest_sha '.manifest_sha256=("a" * 64)'
expect_receipt_fail forged_download_sha '.ciphertext.downloaded_sha256=("b" * 64)'
expect_receipt_fail missing_download_check '.checks.independent_ciphertext_download=false'
expect_receipt_fail plaintext_policy '.policy.plaintext_uploaded=true'
expect_receipt_fail verification_after_check '.verified_at="2026-07-28T10:00:04Z"'
expect_receipt_fail secret '.note="AGE-SECRET-KEY-NOT-REAL"'

mutated_manifest="$tmp/mutated-manifest.json"
jq '.offsite_uri="s3://other-bucket/private/20260728T100001Z"' \
  "$manifest" > "$mutated_manifest"
mutated_manifest_sha="$(shasum -a 256 "$mutated_manifest" | awk '{print $1}')"
jq --arg sha "$mutated_manifest_sha" '.manifest_sha256=$sha' \
  "$receipt" > "$tmp/mutated-manifest-receipt.json"
if python3 ops/scripts/validate-backup-verification-receipt.py \
  "$mutated_manifest" "$tmp/mutated-manifest-receipt.json" \
  --ciphertext "$ciphertext" --offsite-base "$offsite" \
  --not-before "$not_before" --checked-at "$verified_at" >/dev/null 2>&1; then
  fail "manifest bound to a different offsite URI was accepted"
fi

stale_manifest="$tmp/stale-manifest.json"
jq '.backup_id="20260728T095959Z" |
    .created_at="2026-07-28T09:59:59Z" |
    .offsite_uri="s3://merc-backup-authority-test/private/20260728T095959Z"' \
  "$manifest" > "$stale_manifest"
stale_manifest_sha="$(shasum -a 256 "$stale_manifest" | awk '{print $1}')"
jq --arg sha "$stale_manifest_sha" '
  .backup_id="20260728T095959Z" |
  .offsite_uri="s3://merc-backup-authority-test/private/20260728T095959Z" |
  .manifest_sha256=$sha' "$receipt" > "$tmp/stale-receipt.json"
if python3 ops/scripts/validate-backup-verification-receipt.py \
  "$stale_manifest" "$tmp/stale-receipt.json" \
  --ciphertext "$ciphertext" --offsite-base "$offsite" \
  --not-before "$not_before" --checked-at "$verified_at" >/dev/null 2>&1; then
  fail "backup created before this invocation was accepted"
fi

tampered_ciphertext="$tmp/tampered-backup.tar.age"
cp "$ciphertext" "$tampered_ciphertext"
printf 'tamper\n' >> "$tampered_ciphertext"
if python3 ops/scripts/validate-backup-verification-receipt.py \
  "$manifest" "$receipt" \
  --ciphertext "$tampered_ciphertext" --offsite-base "$offsite" \
  --not-before "$not_before" --checked-at "$verified_at" >/dev/null 2>&1; then
  fail "tampered local encrypted bytes were accepted"
fi

if python3 ops/scripts/validate-backup-verification-receipt.py \
  "$manifest" "$receipt" \
  --ciphertext "$ciphertext" --offsite-base 'https://not-s3.invalid/private' \
  --not-before "$not_before" --checked-at "$verified_at" >/dev/null 2>&1; then
  fail "non-S3 offsite authority was accepted"
fi

printf '{"schema_version":1,"schema_version":1}\n' > "$tmp/duplicate-key.json"
if python3 ops/scripts/validate-backup-verification-receipt.py \
  "$manifest" "$tmp/duplicate-key.json" \
  --ciphertext "$ciphertext" --offsite-base "$offsite" \
  --not-before "$not_before" --checked-at "$verified_at" >/dev/null 2>&1; then
  fail "duplicate JSON keys were accepted"
fi

echo "backup-verification-authority: PASS (fresh offsite manifest and downloaded ciphertext)"
