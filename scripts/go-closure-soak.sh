#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/go-closure-common.sh
. "$ROOT/scripts/lib/go-closure-common.sh"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/go-closure-soak.sh --target local|ssh [--duration SECONDS] [--interval SECONDS] [--iteration] --execute

Durations below 86400 require --iteration and can never produce qualifying GO evidence.
USAGE
  exit 2
}

integer_between() {
  local value="$1" minimum="$2" maximum="$3" name="$4"
  [[ "$value" =~ ^[0-9]+$ ]] || gc_die "$name must be an integer"
  [ "$value" -ge "$minimum" ] && [ "$value" -le "$maximum" ] \
    || gc_die "$name must be between $minimum and $maximum"
}

host_soak() {
  local duration="$1" interval="$2" qualification="$3"
  integer_between "$duration" 60 604800 duration
  integer_between "$interval" 15 900 interval
  if [ "$qualification" = qualifying ]; then
    [ "$duration" -ge 86400 ] || gc_die "qualifying soak duration must be at least 86400 seconds"
  elif [ "$qualification" != iteration ]; then
    gc_die "qualification must be qualifying or iteration"
  fi

  gc_load_env
  gc_validate_host_config
  export CX_ACTIVE_CONTROL_IMAGE="$CX_CANDIDATE_CONTROL_IMAGE"
  gc_validate_compose_images
  gc_probe_release "$CX_CANDIDATE_COMMIT" >/dev/null

  local rss_limit="${CX_SOAK_MAX_RSS_GROWTH_BYTES:-134217728}"
  local disk_limit="${CX_SOAK_MAX_DISK_GROWTH_KB:-1048576}"
  local writable_limit="${CX_SOAK_MAX_WRITABLE_LAYER_GROWTH_BYTES:-67108864}"
  local connection_limit="${CX_SOAK_MAX_CONNECTION_GROWTH:-4}"
  integer_between "$rss_limit" 1 2147483648 CX_SOAK_MAX_RSS_GROWTH_BYTES
  integer_between "$disk_limit" 1 104857600 CX_SOAK_MAX_DISK_GROWTH_KB
  integer_between "$writable_limit" 1 1073741824 CX_SOAK_MAX_WRITABLE_LAYER_GROWTH_BYTES
  integer_between "$connection_limit" 0 100 CX_SOAK_MAX_CONNECTION_GROWTH

  gc_prepare_evidence soak-working
  local samples
  samples="$GC_EVIDENCE_DIR/$(date -u +%Y%m%dT%H%M%SZ)-soak-samples.jsonl"
  local cid pid started_epoch end_epoch now_epoch sleep_seconds sample_count=0
  local baseline_rss_kb baseline_disk_kb baseline_size_rw baseline_connections baseline_restarts
  local max_rss_kb max_disk_kb max_size_rw max_connections
  local rss_kb disk_kb size_rw connections acquired workers page_alerts dead_letters
  local restarts db_snapshot sample_time last_rss_kb last_disk_kb last_size_rw last_connections
  cid="$(gc_compose ps -q control)"
  [ -n "$cid" ] || gc_die "control container is not running"
  pid="$(docker inspect -f '{{.State.Pid}}' "$cid")"
  [ "$pid" -gt 0 ] || gc_die "control container has no live PID"
  baseline_rss_kb="$(ps -o rss= -p "$pid" | tr -d ' ')"
  baseline_disk_kb="$(df -Pk "$GC_ROOT" | awk 'NR==2 {print $3}')"
  baseline_size_rw="$(docker inspect --size -f '{{.SizeRw}}' "$cid")"
  baseline_connections="$(gc_prometheus_scalar 'cx_db_pool_connections{state="total"}')"
  baseline_restarts="$(docker inspect -f '{{.RestartCount}}' "$cid")"
  integer_between "$baseline_rss_kb" 1 2147483647 baseline_rss_kb
  integer_between "$baseline_disk_kb" 1 9223372036854775807 baseline_disk_kb
  integer_between "$baseline_size_rw" 0 9223372036854775807 baseline_size_rw
  max_rss_kb="$baseline_rss_kb"
  max_disk_kb="$baseline_disk_kb"
  max_size_rw="$baseline_size_rw"
  max_connections="$baseline_connections"
  last_rss_kb="$baseline_rss_kb"
  last_disk_kb="$baseline_disk_kb"
  last_size_rw="$baseline_size_rw"
  last_connections="$baseline_connections"
  started_epoch="$(date +%s)"
  end_epoch=$((started_epoch + duration))
  umask 077

  while [ "$(date +%s)" -lt "$end_epoch" ]; do
    sample_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    gc_probe_release "$CX_CANDIDATE_COMMIT" >/dev/null
    cid="$(gc_compose ps -q control)"
    pid="$(docker inspect -f '{{.State.Pid}}' "$cid")"
    rss_kb="$(ps -o rss= -p "$pid" | tr -d ' ')"
    disk_kb="$(df -Pk "$GC_ROOT" | awk 'NR==2 {print $3}')"
    size_rw="$(docker inspect --size -f '{{.SizeRw}}' "$cid")"
    restarts="$(docker inspect -f '{{.RestartCount}}' "$cid")"
    [ "$restarts" = "$baseline_restarts" ] || gc_die "control restart count changed during soak"
    workers="$(gc_prometheus_scalar 'cx_active_workers')"
    connections="$(gc_prometheus_scalar 'cx_db_pool_connections{state="total"}')"
    acquired="$(gc_prometheus_scalar 'cx_db_pool_connections{state="acquired"}')"
    page_alerts="$(gc_prometheus_scalar 'sum(ALERTS{alertstate="firing",severity="page"}) or vector(0)')"
    dead_letters="$(gc_prometheus_scalar 'sum(cx_webhook_backlog{state="dead_letter"}) or vector(0)')"
    awk -v n="$workers" 'BEGIN { exit !(n >= 2) }' || gc_die "active workers dropped below two"
    awk -v n="$page_alerts" 'BEGIN { exit !(n == 0) }' || gc_die "a page alert fired during soak"
    awk -v n="$dead_letters" 'BEGIN { exit !(n == 0) }' || gc_die "webhook dead letters appeared during soak"
    db_snapshot="$(gc_database_snapshot)"
    jq -e '.terminal_jobs_with_open_tasks == 0' <<< "$db_snapshot" >/dev/null \
      || gc_die "terminal jobs with open tasks detected during soak"

    [ "$rss_kb" -le "$max_rss_kb" ] || max_rss_kb="$rss_kb"
    [ "$disk_kb" -le "$max_disk_kb" ] || max_disk_kb="$disk_kb"
    [ "$size_rw" -le "$max_size_rw" ] || max_size_rw="$size_rw"
    awk -v n="$connections" -v m="$max_connections" 'BEGIN { exit !(n > m) }' \
      && max_connections="$connections" || true
    last_rss_kb="$rss_kb"
    last_disk_kb="$disk_kb"
    last_size_rw="$size_rw"
    last_connections="$connections"
    sample_count=$((sample_count + 1))

    jq -cn \
      --arg at "$sample_time" --argjson sequence "$sample_count" \
      --argjson rss_kb "$rss_kb" --argjson disk_used_kb "$disk_kb" \
      --argjson writable_bytes "$size_rw" --argjson restarts "$restarts" \
      --argjson workers "$workers" --argjson connections "$connections" \
      --argjson acquired "$acquired" --argjson pages "$page_alerts" \
      --argjson dead_letters "$dead_letters" --argjson database "$db_snapshot" \
      '{observed_at:$at,sequence:$sequence,control_rss_kb:$rss_kb,
        host_disk_used_kb:$disk_used_kb,control_writable_layer_bytes:$writable_bytes,
        control_restart_count:$restarts,active_workers:$workers,
        db_connections_total:$connections,db_connections_acquired:$acquired,
        firing_page_alerts:$pages,webhook_dead_letters:$dead_letters,database:$database}' \
      >> "$samples"

    now_epoch="$(date +%s)"
    [ "$now_epoch" -lt "$end_epoch" ] || break
    sleep_seconds="$interval"
    [ $((end_epoch - now_epoch)) -ge "$sleep_seconds" ] || sleep_seconds=$((end_epoch - now_epoch))
    sleep "$sleep_seconds"
  done
  chmod 600 "$samples"

  local finished_epoch actual_duration expected_samples minimum_samples rss_growth_bytes
  local disk_growth_kb writable_growth_bytes connection_growth qualifies samples_sha
  finished_epoch="$(date +%s)"
  actual_duration=$((finished_epoch - started_epoch))
  expected_samples=$((duration / interval))
  [ "$expected_samples" -ge 1 ] || expected_samples=1
  minimum_samples=$(((expected_samples * 95 + 99) / 100))
  [ "$sample_count" -ge "$minimum_samples" ] \
    || gc_die "captured $sample_count samples, below 95% minimum $minimum_samples"
  rss_growth_bytes=$(((max_rss_kb - baseline_rss_kb) * 1024))
  disk_growth_kb=$((max_disk_kb - baseline_disk_kb))
  writable_growth_bytes=$((max_size_rw - baseline_size_rw))
  connection_growth="$(awk -v max="$max_connections" -v base="$baseline_connections" 'BEGIN {print max-base}')"
  [ "$rss_growth_bytes" -le "$rss_limit" ] || gc_die "RSS growth exceeded the configured bound"
  [ "$disk_growth_kb" -le "$disk_limit" ] || gc_die "disk growth exceeded the configured bound"
  [ "$writable_growth_bytes" -le "$writable_limit" ] || gc_die "writable-layer growth exceeded the configured bound"
  awk -v growth="$connection_growth" -v limit="$connection_limit" 'BEGIN { exit !(growth <= limit) }' \
    || gc_die "database connection growth exceeded the configured bound"
  qualifies=false
  if [ "$qualification" = qualifying ] && [ "$actual_duration" -ge 86400 ]; then qualifies=true; fi
  samples_sha="$(gc_sha256 "$samples")"

  gc_prepare_evidence soak
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$(date -u -r "$started_epoch" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$started_epoch" +%Y-%m-%dT%H:%M:%SZ)" \
    --arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg image "$CX_CANDIDATE_CONTROL_IMAGE" --arg commit "$CX_CANDIDATE_COMMIT" \
    --arg mode "$qualification" --arg samples_file "${samples#"$GC_ROOT"/}" \
    --arg samples_sha "$samples_sha" --argjson requested "$duration" \
    --argjson actual "$actual_duration" --argjson interval "$interval" \
    --argjson sample_count "$sample_count" --argjson qualifies "$qualifies" \
    --argjson baseline_rss "$baseline_rss_kb" --argjson max_rss "$max_rss_kb" \
    --argjson final_rss "$last_rss_kb" --argjson rss_growth "$rss_growth_bytes" \
    --argjson baseline_disk "$baseline_disk_kb" --argjson max_disk "$max_disk_kb" \
    --argjson final_disk "$last_disk_kb" --argjson disk_growth "$disk_growth_kb" \
    --argjson baseline_writable "$baseline_size_rw" --argjson max_writable "$max_size_rw" \
    --argjson final_writable "$last_size_rw" --argjson writable_growth "$writable_growth_bytes" \
    --argjson baseline_connections "$baseline_connections" --argjson max_connections "$max_connections" \
    --argjson final_connections "$last_connections" --argjson connection_growth "$connection_growth" \
    '{schema_version:1,kind:"go_closure_soak",status:"PASS",
      started_at:$started,finished_at:$finished,mode:$mode,
      control_image:$image,expected_commit:$commit,
      duration:{requested_seconds:$requested,actual_seconds:$actual,interval_seconds:$interval,samples:$sample_count},
      samples:{path:$samples_file,sha256:$samples_sha},
      bounds:{rss:{baseline_kb:$baseline_rss,max_kb:$max_rss,final_kb:$final_rss,max_growth_bytes:$rss_growth},
              disk:{baseline_used_kb:$baseline_disk,max_used_kb:$max_disk,final_used_kb:$final_disk,max_growth_kb:$disk_growth},
              writable_layer:{baseline_bytes:$baseline_writable,max_bytes:$max_writable,final_bytes:$final_writable,max_growth_bytes:$writable_growth},
              db_connections:{baseline:$baseline_connections,max:$max_connections,final:$final_connections,max_growth:$connection_growth}},
      assertions:{two_agents_continuously_present:true,no_page_alerts:true,no_webhook_dead_letters:true,
                  no_control_restarts:true,no_stuck_terminal_jobs:true,bounded_resource_growth:true},
      qualification:{qualifies_for_24h_gate:$qualifies,
                     reason:(if $qualifies then "observed_at_least_86400_seconds" else "short_iteration_only" end)},
      policy:{stripe_test_mode:true,stripe_live_mode:false,real_value:false,secret_values_recorded:false}}'
  gc_log "PASS receipt: $GC_EVIDENCE_FILE"
}

if [ "${1:-}" = --host ]; then
  [ "$#" -eq 4 ] || usage
  host_soak "$2" "$3" "$4"
  exit
fi

target="" duration=86400 interval=60 qualification=qualifying execute=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) shift; target="${1:-}" ;;
    --duration) shift; duration="${1:-}" ;;
    --interval) shift; interval="${1:-}" ;;
    --iteration) qualification=iteration ;;
    --execute) execute=true ;;
    *) usage ;;
  esac
  shift
done
case "$target" in local|ssh) ;; *) usage ;; esac
[ "$execute" = true ] || usage
integer_between "$duration" 60 604800 duration
integer_between "$interval" 15 900 interval
if [ "$duration" -lt 86400 ] && [ "$qualification" != iteration ]; then
  gc_die "durations below 86400 require --iteration"
fi
gc_require_command jq
gc_load_env
gc_require_declared_inputs STAGING_DEPLOYMENT_ROOT
if [ "$target" = ssh ]; then gc_validate_ssh_target; fi
gc_run_on_target "$target" scripts/go-closure-soak.sh "$duration" "$interval" "$qualification"
