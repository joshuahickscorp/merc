#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/go-closure-common.sh
. "$ROOT/ops/scripts/lib/go-closure-common.sh"

usage() {
  echo "usage: ops/scripts/go-closure-rollback-rehearsal.sh --target local|ssh --execute" >&2
  exit 2
}

activate_and_probe() {
  local image="$1" commit="$2" label="$3" start end cid configured
  export MERC_ACTIVE_CONTROL_IMAGE="$image"
  gc_validate_compose_images
  gc_pull_exact "$image"
  start="$(date +%s)"
  gc_compose up -d --no-deps control
  gc_wait_service control 300
  gc_probe_release "$commit" >/dev/null
  end="$(date +%s)"
  cid="$(gc_compose ps -q control)"
  configured="$(docker inspect -f '{{.Config.Image}}' "$cid")"
  [ "$configured" = "$image" ] || gc_die "$label activated $configured instead of $image"
  printf '%s\n' "$((end - start))"
}

host_rehearsal() {
  gc_load_env
  gc_validate_host_config
  gc_require_command age
  gc_require_command aws
  gc_require_command python3
  gc_require_declared_inputs MERC_BACKUP_OFFSITE AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY \
    MERC_BACKUP_ENCRYPTION_RECIPIENT MERC_BACKUP_DECRYPTION_IDENTITY_FILE
  [[ "$MERC_BACKUP_OFFSITE" == s3://* ]] || gc_die "MERC_BACKUP_OFFSITE must use s3://"
  [[ "$MERC_BACKUP_ENCRYPTION_RECIPIENT" == age1* ]] \
    || gc_die "MERC_BACKUP_ENCRYPTION_RECIPIENT must be an age recipient"
  [ -r "$MERC_BACKUP_DECRYPTION_IDENTITY_FILE" ] \
    || gc_die "MERC_BACKUP_DECRYPTION_IDENTITY_FILE is not readable"

  export MERC_ACTIVE_CONTROL_IMAGE="$MERC_CANDIDATE_CONTROL_IMAGE"
  gc_validate_compose_images
  gc_probe_release "$MERC_CANDIDATE_COMMIT" >/dev/null
  local initial_cid initial_image
  initial_cid="$(gc_compose ps -q control)"
  initial_image="$(docker inspect -f '{{.Config.Image}}' "$initial_cid")"
  [ "$initial_image" = "$MERC_CANDIDATE_CONTROL_IMAGE" ] \
    || gc_die "pre-rehearsal control image is $initial_image, not the candidate digest"

  local started_at snapshot_before snapshot_after snapshot_before_sha snapshot_after_sha
  local backup_log backup_result backup_base latest_manifest backup_verification backup_id
  local backup_ciphertext_sha backup_manifest_sha backup_checked_at
  local rollback_rto forward_rto finished_at
  started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  snapshot_before="$(gc_database_snapshot)"
  jq -e . <<< "$snapshot_before" >/dev/null || gc_die "pre-rehearsal database snapshot is invalid"

  gc_prepare_evidence rollback-working
  backup_log="$GC_EVIDENCE_DIR/$(date -u +%Y%m%dT%H%M%SZ)-pre-rollback-backup.log"
  backup_result="$GC_EVIDENCE_DIR/$(date -u +%Y%m%dT%H%M%SZ)-backup-invocation.json"
  export MERC_COMPOSE_FILE="$GC_COMPOSE"
  export MERC_BACKUP_STATUS_FILE="$GC_ROOT/.artifacts/go-closure-health/last-successful-offsite-backup.unixtime"
  export MERC_BACKUP_RESULT_FILE="$backup_result"
  mkdir -p "$(dirname "$MERC_BACKUP_STATUS_FILE")"
  chmod 700 "$(dirname "$MERC_BACKUP_STATUS_FILE")"
  if ! bash "$GC_ROOT/ops/scripts/backup.sh" >"$backup_log" 2>&1; then
    gc_log "backup failed; non-secret command log retained at $backup_log"
    exit 1
  fi
  chmod 600 "$backup_log"
  [ -f "$backup_result" ] || gc_die "backup succeeded but emitted no exact invocation result"
  backup_checked_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  jq -e --arg offsite "${MERC_BACKUP_OFFSITE%/}" \
    --arg started "$started_at" --arg checked "$backup_checked_at" '
    (keys == ["backup_id","ciphertext_sha256","completed_at","kind",
              "manifest_sha256","offsite_uri","schema_version","status",
              "verification_sha256"]) and
    .schema_version == 1 and .kind == "merc_backup_invocation_result" and
    .status == "PASS" and
    (.backup_id | test("^[0-9]{8}T[0-9]{6}Z$")) and
    .offsite_uri == ($offsite + "/" + .backup_id) and
    (.completed_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
    (.completed_at | fromdateiso8601) >= ($started | fromdateiso8601) and
    (.completed_at | fromdateiso8601) <= ($checked | fromdateiso8601) and
    (.manifest_sha256 | test("^[0-9a-f]{64}$")) and
    (.verification_sha256 | test("^[0-9a-f]{64}$")) and
    (.ciphertext_sha256 | test("^[0-9a-f]{64}$"))
  ' "$backup_result" >/dev/null \
    || gc_die "backup invocation result is malformed or offsite-mismatched"
  backup_id="$(jq -er '.backup_id' "$backup_result")"
  backup_base="${MERC_BACKUP_DIR:-$GC_ROOT/.artifacts/backups}"
  latest_manifest="$backup_base/$backup_id/manifest.json"
  [ -f "$latest_manifest" ] || gc_die "exact backup invocation manifest is missing"
  backup_verification="$(dirname "$latest_manifest")/verification.json"
  [ -f "$backup_verification" ] \
    || gc_die "backup succeeded but no verification receipt was found"
  backup_ciphertext_sha="$(jq -er '.ciphertext_sha256 | select(test("^[0-9a-f]{64}$"))' "$latest_manifest")"
  backup_manifest_sha="$(gc_sha256 "$latest_manifest")"
  [ "$backup_manifest_sha" = "$(jq -r '.manifest_sha256' "$backup_result")" ] \
    || gc_die "backup invocation result does not match its manifest bytes"
  [ "$(gc_sha256 "$backup_verification")" = "$(jq -r '.verification_sha256' "$backup_result")" ] \
    || gc_die "backup invocation result does not match its verification bytes"
  [ "$backup_ciphertext_sha" = "$(jq -r '.ciphertext_sha256' "$backup_result")" ] \
    || gc_die "backup invocation result does not match its ciphertext"
  python3 "$GC_ROOT/ops/scripts/validate-backup-verification-receipt.py" \
    "$latest_manifest" "$backup_verification" \
    --ciphertext "$(dirname "$latest_manifest")/backup.tar.age" \
    --offsite-base "$MERC_BACKUP_OFFSITE" \
    --not-before "$started_at" \
    --checked-at "$backup_checked_at" >/dev/null \
    || gc_die "pre-rollback backup lacks fresh exact offsite-download authority"

  rollback_rto="$(activate_and_probe "$MERC_PRIOR_CONTROL_IMAGE" "$MERC_PRIOR_COMMIT" prior)"
  forward_rto="$(activate_and_probe "$MERC_CANDIDATE_CONTROL_IMAGE" "$MERC_CANDIDATE_COMMIT" candidate)"

  snapshot_after="$(gc_database_snapshot)"
  jq -e . <<< "$snapshot_after" >/dev/null || gc_die "post-rehearsal database snapshot is invalid"
  [ "$snapshot_before" = "$snapshot_after" ] \
    || gc_die "database integrity snapshot changed during quiescent rollback/forward rehearsal"
  snapshot_before_sha="$(printf '%s' "$snapshot_before" | { if command -v shasum >/dev/null 2>&1; then shasum -a 256; else sha256sum; fi; } | awk '{print $1}')"
  snapshot_after_sha="$(printf '%s' "$snapshot_after" | { if command -v shasum >/dev/null 2>&1; then shasum -a 256; else sha256sum; fi; } | awk '{print $1}')"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  gc_prepare_evidence rollback-forward
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$started_at" --arg finished "$finished_at" \
    --arg candidate_image "$MERC_CANDIDATE_CONTROL_IMAGE" \
    --arg candidate_commit "$MERC_CANDIDATE_COMMIT" \
    --arg prior_image "$MERC_PRIOR_CONTROL_IMAGE" \
    --arg prior_commit "$MERC_PRIOR_COMMIT" \
    --arg backup_id "$backup_id" \
    --arg backup_ciphertext_sha "$backup_ciphertext_sha" \
    --arg backup_manifest_sha "$backup_manifest_sha" \
    --arg backup_manifest "${latest_manifest#"$GC_ROOT"/}" \
    --arg snapshot_before_sha "$snapshot_before_sha" \
    --arg snapshot_after_sha "$snapshot_after_sha" \
    --argjson snapshot "$snapshot_after" \
    --argjson rollback_rto "$rollback_rto" \
    --argjson forward_rto "$forward_rto" \
    --slurpfile backup_verification "$backup_verification" \
    --slurpfile backup_result "$backup_result" \
    '{schema_version:1,kind:"go_closure_rollback_forward",status:"PASS",
      started_at:$started,finished_at:$finished,
      candidate:{image:$candidate_image,commit:$candidate_commit,forward_recoveries:1,rto_seconds:$forward_rto},
      prior:{image:$prior_image,commit:$prior_commit,rollbacks:1,rto_seconds:$rollback_rto},
      pre_upgrade_backup:{backup_id:$backup_id,ciphertext_sha256:$backup_ciphertext_sha,
                          manifest_sha256:$backup_manifest_sha,local_manifest:$backup_manifest,
                          offsite_uri:$backup_verification[0].offsite_uri,
                          invocation_result:$backup_result[0],
                          verification_receipt:$backup_verification[0]},
      data_integrity:{unchanged:true,before_sha256:$snapshot_before_sha,
                      after_sha256:$snapshot_after_sha,snapshot:$snapshot},
      policy:{stripe_live_mode:false,real_value:false,secret_values_recorded:false}}'
  gc_log "PASS receipt: $GC_EVIDENCE_FILE"
}

if [ "${1:-}" = --host ]; then
  [ "$#" -eq 1 ] || usage
  host_rehearsal
  exit
fi

target="" execute=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) shift; target="${1:-}" ;;
    --execute) execute=true ;;
    *) usage ;;
  esac
  shift
done
case "$target" in local|ssh) ;; *) usage ;; esac
[ "$execute" = true ] || usage
gc_require_command jq
gc_load_env
gc_require_declared_inputs STAGING_DEPLOYMENT_ROOT
if [ "$target" = ssh ]; then gc_validate_ssh_target; fi
gc_run_on_target "$target" ops/scripts/go-closure-rollback-rehearsal.sh
