#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

for command in jq python3 shasum; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "go-closure soak authority test: missing $command" >&2
    exit 1
  }
done

fail() { echo "go-closure soak authority test: FAIL: $*" >&2; exit 1; }
tmp="$(mktemp -d "${TMPDIR:-/tmp}/merc-go-closure-soak-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
root="$tmp/root"
mkdir -p "$root/evidence/go-closure"

commit='1111111111111111111111111111111111111111'
image='registry.example.invalid/merc/control@sha256:2222222222222222222222222222222222222222222222222222222222222222'
container_id='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
image_id='sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
base_rel='evidence/go-closure/valid-samples.jsonl'
base_samples="$root/$base_rel"

timestamps=(
  '2026-07-28T10:00:00Z'
  '2026-07-28T10:00:15Z'
  '2026-07-28T10:00:30Z'
  '2026-07-28T10:00:45Z'
)
rss=(100 101 103 102)
disk=(1000 1001 1002 1001)
writable=(1000 1002 1004 1003)
connections=(2 2 3 2)

: > "$base_samples"
for index in 0 1 2 3; do
  jq -cn \
    --arg at "${timestamps[$index]}" \
    --argjson sequence "$((index + 1))" \
    --arg container_id "$container_id" \
    --arg configured_image "$image" \
    --arg image_id "$image_id" \
    --argjson rss "${rss[$index]}" \
    --argjson disk "${disk[$index]}" \
    --argjson writable "${writable[$index]}" \
    --argjson connections "${connections[$index]}" '
    {
      observed_at:$at,
      sequence:$sequence,
      control_container_id:$container_id,
      control_configured_image:$configured_image,
      control_image_id:$image_id,
      control_rss_kb:$rss,
      host_disk_used_kb:$disk,
      control_writable_layer_bytes:$writable,
      control_restart_count:0,
      active_workers:2,
      db_connections_total:$connections,
      db_connections_acquired:1,
      firing_page_alerts:0,
      webhook_dead_letters:0,
      database:{
        buyers:2,suppliers:2,workers:2,jobs:40,tasks:40,ledger_entries:120,
        ledger_sum_usd:"0.000000",terminal_jobs_with_open_tasks:0
      }
    }' >> "$base_samples"
done

sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
base_sha="$(sha256 "$base_samples")"
valid="$tmp/valid.json"
jq -n \
  --arg commit "$commit" --arg image "$image" \
  --arg container_id "$container_id" --arg image_id "$image_id" \
  --arg samples_path "$base_rel" --arg samples_sha "$base_sha" '
  {
    schema_version:2,
    kind:"go_closure_soak",
    status:"PASS",
    started_at:"2026-07-28T10:00:00Z",
    finished_at:"2026-07-28T10:01:00Z",
    mode:"iteration",
    control_image:$image,
    expected_commit:$commit,
    runtime:{
      container_id:$container_id,
      configured_image:$image,
      image_id:$image_id,
      restart_count:0
    },
    duration:{
      requested_seconds:60,
      actual_seconds:60,
      interval_seconds:15,
      samples:4
    },
    samples:{path:$samples_path,sha256:$samples_sha},
    bounds:{
      rss:{
        baseline_kb:100,max_kb:103,final_kb:102,
        observed_growth_bytes:3072,limit_growth_bytes:134217728
      },
      disk:{
        baseline_used_kb:1000,max_used_kb:1002,final_used_kb:1001,
        observed_growth_kb:2,limit_growth_kb:1048576
      },
      writable_layer:{
        baseline_bytes:1000,max_bytes:1004,final_bytes:1003,
        observed_growth_bytes:4,limit_growth_bytes:67108864
      },
      db_connections:{
        baseline:2,max:3,final:2,observed_growth:1,limit_growth:4
      }
    },
    assertions:{
      two_agents_continuously_present:true,
      no_page_alerts:true,
      no_webhook_dead_letters:true,
      no_control_restarts_or_recreates:true,
      no_stuck_terminal_jobs:true,
      bounded_resource_growth:true,
      raw_samples_independently_validated:true
    },
    qualification:{
      qualifies_for_24h_gate:false,
      reason:"short_iteration_only"
    },
    policy:{
      stripe_test_mode:true,
      stripe_live_mode:false,
      real_value:false,
      secret_values_recorded:false
    }
  }' > "$valid"

validator=(
  python3 scripts/validate-go-closure-soak-receipt.py
  --root "$root"
  --commit "$commit"
  --image "$image"
)
"${validator[@]}" "$valid" >/dev/null \
  || fail "valid sample-derived iteration receipt was rejected"

expect_receipt_fail() {
  local name="$1" filter="$2"
  jq "$filter" "$valid" > "$tmp/$name.json"
  if "${validator[@]}" "$tmp/$name.json" >/dev/null 2>&1; then
    fail "$name receipt mutation was accepted"
  fi
}

expect_receipt_fail mixed_commit '.expected_commit = ("3" * 40)'
expect_receipt_fail mixed_image '.control_image = "registry.example.invalid/merc/other@sha256:\("4" * 64)"'
expect_receipt_fail substituted_runtime '.runtime.configured_image = "registry.example.invalid/merc/other@sha256:\("5" * 64)"'
expect_receipt_fail short_qualifying '
  .mode="qualifying" |
  .qualification.qualifies_for_24h_gate=true |
  .qualification.reason="observed_at_least_86400_seconds"'
expect_receipt_fail iteration_promoted '.qualification.qualifies_for_24h_gate=true'
expect_receipt_fail forged_bound '.bounds.rss.max_kb=102'
expect_receipt_fail numeric_assertion '.assertions.no_page_alerts=1'
expect_receipt_fail floating_schema_version '.schema_version=2.0'
expect_receipt_fail missing_samples '.samples.path="evidence/go-closure/missing.jsonl"'
expect_receipt_fail traversal '.samples.path="../outside.jsonl"'
expect_receipt_fail secret '.runtime.container_id="whsec_not-a-real-secret"'

ln -s "$base_samples" "$root/evidence/go-closure/symlinked-samples.jsonl"
jq --arg path 'evidence/go-closure/symlinked-samples.jsonl' \
  '.samples.path=$path' "$valid" > "$tmp/symlinked-samples.json"
if "${validator[@]}" "$tmp/symlinked-samples.json" >/dev/null 2>&1; then
  fail "symlinked sample stream was accepted"
fi

tampered="$root/evidence/go-closure/tampered.jsonl"
cp "$base_samples" "$tampered"
printf ' \n' >> "$tampered"
jq --arg path 'evidence/go-closure/tampered.jsonl' \
  '.samples.path=$path' "$valid" > "$tmp/tampered-hash.json"
if "${validator[@]}" "$tmp/tampered-hash.json" >/dev/null 2>&1; then
  fail "sample stream with a mismatched SHA-256 was accepted"
fi

expect_sample_fail() {
  local name="$1" filter="$2" path rel digest
  rel="evidence/go-closure/$name.jsonl"
  path="$root/$rel"
  jq -sc "$filter | .[]" "$base_samples" > "$path"
  digest="$(sha256 "$path")"
  jq --arg path "$rel" --arg sha "$digest" \
    '.samples.path=$path | .samples.sha256=$sha' "$valid" > "$tmp/$name.json"
  if "${validator[@]}" "$tmp/$name.json" >/dev/null 2>&1; then
    fail "$name sample mutation was accepted"
  fi
}

expect_sample_fail recreated_container '.[1].control_container_id = ("c" * 64)'
expect_sample_fail one_agent '.[2].active_workers = 1'
expect_sample_fail page_alert '.[1].firing_page_alerts = 1'
expect_sample_fail dead_letter '.[3].webhook_dead_letters = 1'
expect_sample_fail terminal_open_task '.[0].database.terminal_jobs_with_open_tasks = 1'
expect_sample_fail sequence_gap '.[1].sequence = 8'
expect_sample_fail floating_sequence '.[1].sequence = 2.0'
expect_sample_fail forged_resource_samples '.[1].control_rss_kb = 999'

printf '{"schema_version":2,"schema_version":2}\n' > "$tmp/duplicate-key.json"
if "${validator[@]}" "$tmp/duplicate-key.json" >/dev/null 2>&1; then
  fail "duplicate JSON keys were accepted"
fi

echo "go-closure-soak-authority: PASS (candidate continuity and raw sample derivation)"
