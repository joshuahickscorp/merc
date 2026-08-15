#!/usr/bin/env bash
# P1-ALERT-DELIVERY: Alertmanager -> EXTERNAL staffed HTTPS sink.
# The existing scripts/test-alert-delivery.sh proves a local sink only.
# This wrapper requires a non-loopback https:// receiver.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=scripts/alpha/lib.sh
. "$ROOT/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/alpha/alert-sink.sh --print-runbook|--check|--execute|--record-pass

--check     require https external URL (not localhost / droplet loopback)
--execute   SUPERVISOR: fire + resolve via staging Alertmanager; wait for sink file
USAGE
  exit 2
}

print_runbook() {
  cat <<EOF
# P1-ALERT-DELIVERY — SUPERVISOR EXECUTES
# Exit: Preserve redacted external receiver delivery IDs and firing,
# acknowledgement, deduplication, and resolution timestamps.
#
# Receiver wiring (already in ops/monitoring/alertmanager.yml):
#   receivers.real-canary-receiver.webhook_configs[0].url_file
#     = /run/secrets/cx_alert_receiver_url
#   send_resolved: true
# Compose secret: MERC_ALERT_RECEIVER_URL_FILE -> file containing ONE https URL.
# That URL must page a human (PagerDuty / Opsgenie / staffed webhook), not
# 127.0.0.1 and not the droplet itself.
#
# Fire/resolve (on the droplet, Alertmanager is loopback-bound):
ssh \${STAGING_SSH_TARGET:-root@192.241.134.31} 'curl -fsS http://127.0.0.1:9093/-/ready'
# Then from this Mac (if you have a tunnel) or on the droplet:
scripts/alpha/alert-sink.sh --execute
#
# --execute posts a synthetic page to Alertmanager and waits for
# MERC_CANARY_ALERT_SINK_FILE to observe distinct firing + resolved event IDs
# from the EXTERNAL receiver. The supervisor pastes/redacts those IDs into
# the sink file (JSONL: {status,alertname,event_id,received_at}).
#
# Acknowledgement: record the staffed-sink ack timestamp in the same file
# as {"status":"acknowledged","event_id":"...","received_at":"...Z"}.
# Dedup: a second fire with the same labels must not mint a new page.
EOF
}

is_external_https() {
  local url="$1" host
  [[ "$url" == https://* ]] || return 1
  host="$(printf '%s' "$url" | sed -E 's#^https://([^/:]+).*#\1#')"
  case "$host" in
    localhost|127.0.0.1|::1|0.0.0.0|host.docker.internal)
      return 1 ;;
    192.241.134.31)
      return 1 ;;
  esac
  return 0
}

check_only() {
  alpha_require_command jq
  alpha_load_env_optional
  if ! alpha_check_ready P1-ALERT-DELIVERY; then
    alpha_die "P1-ALERT-DELIVERY is not execute-ready (boot/staging)"
  fi
  [ -n "${ALERT_RECEIVER_WEBHOOK_URL:-}" ] \
    || alpha_die "ALERT_RECEIVER_WEBHOOK_URL is required"
  [ -n "${ALERT_RECEIVER_NAME:-}" ] \
    || alpha_die "ALERT_RECEIVER_NAME is required"
  is_external_https "$ALERT_RECEIVER_WEBHOOK_URL" \
    || alpha_die "ALERT_RECEIVER_WEBHOOK_URL must be https:// to an external staffed sink (not localhost or the droplet)"
  [[ "$ALERT_RECEIVER_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9_.:/@-]{7,199}$ ]] \
    || alpha_die "ALERT_RECEIVER_NAME must be a non-secret SAFE_ID"
  alpha_log "CHECK ok: external https receiver named, boot+staging PASS (no fire)"
}

execute_fire_resolve() {
  alpha_require_command jq
  alpha_require_command curl
  alpha_load_env_optional
  alpha_require_execute_ready P1-ALERT-DELIVERY
  is_external_https "${ALERT_RECEIVER_WEBHOOK_URL:-}" \
    || alpha_die "receiver is not an external https URL"
  local am_url sink alert_name payload response code
  am_url="${MERC_ALERTMANAGER_URL:-http://127.0.0.1:9093}"
  sink="${MERC_CANARY_ALERT_SINK_FILE:-}"
  [ -n "$sink" ] || alpha_die "MERC_CANARY_ALERT_SINK_FILE must point at the JSONL the external receiver writes"
  alert_name="MercAlphaExternalSink_$(date -u +%Y%m%dT%H%M%SZ)"
  payload="$(jq -nc --arg name "$alert_name" --arg recv "$ALERT_RECEIVER_NAME" '
    [{labels:{alertname:$name,severity:"page",receiver:$recv,alpha_gate:"P1-ALERT-DELIVERY"},
      annotations:{summary:"alpha external sink fire/resolve"},
      startsAt:(now|strftime("%Y-%m-%dT%H:%M:%SZ"))}]')"
  response="$(curl --silent --show-error -o /tmp/merc-alpha-am-body.$$ -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' --data-binary "$payload" \
    "$am_url/api/v2/alerts" || true)"
  code="$response"
  rm -f /tmp/merc-alpha-am-body.$$
  case "$code" in
    2??) ;;
    *) alpha_die "Alertmanager fire failed HTTP $code (is MERC_ALERTMANAGER_URL reachable from here? supervisor may need to run this on the droplet)" ;;
  esac
  payload="$(jq -nc --arg name "$alert_name" --arg recv "$ALERT_RECEIVER_NAME" '
    [{labels:{alertname:$name,severity:"page",receiver:$recv,alpha_gate:"P1-ALERT-DELIVERY"},
      annotations:{summary:"alpha external sink fire/resolve"},
      startsAt:(now-120|strftime("%Y-%m-%dT%H:%M:%SZ")),
      endsAt:(now-60|strftime("%Y-%m-%dT%H:%M:%SZ"))}]')"
  code="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' --data-binary "$payload" \
    "$am_url/api/v2/alerts" || true)"
  case "$code" in
    2??) ;;
    *) alpha_die "Alertmanager resolve failed HTTP $code" ;;
  esac
  alpha_log "posted fire+resolve for $alert_name. waiting for external sink file $sink"
  local deadline firing_id resolved_id ack_at
  deadline=$(( $(date +%s) + 180 ))
  firing_id=""
  resolved_id=""
  ack_at=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if [ -f "$sink" ]; then
      firing_id="$(jq -r --arg n "$alert_name" \
        'select((.alertname // .labels.alertname) == $n) | select((.status // "") | test("fir")) | .event_id // .id // empty' \
        "$sink" 2>/dev/null | head -1 || true)"
      resolved_id="$(jq -r --arg n "$alert_name" \
        'select((.alertname // .labels.alertname) == $n) | select((.status // "") | test("resol")) | .event_id // .id // empty' \
        "$sink" 2>/dev/null | head -1 || true)"
      ack_at="$(jq -r --arg n "$alert_name" \
        'select((.alertname // .labels.alertname) == $n) | select((.status // "") | test("ack")) | .received_at // empty' \
        "$sink" 2>/dev/null | head -1 || true)"
    fi
    if [ -n "$firing_id" ] && [ -n "$resolved_id" ] && [ "$firing_id" != "$resolved_id" ]; then
      break
    fi
    sleep 2
  done
  [ -n "$firing_id" ] && [ -n "$resolved_id" ] && [ "$firing_id" != "$resolved_id" ] \
    || alpha_die "external sink did not observe distinct firing+resolved delivery IDs within 180s"
  mkdir -p "$ALPHA_RECEIPT_DIR"
  jq -n \
    --arg name "$alert_name" --arg recv "$ALERT_RECEIVER_NAME" \
    --arg fire "$firing_id" --arg res "$resolved_id" --arg ack "$ack_at" \
    --arg at "$(alpha_utc)" \
    '{schema_version:1,kind:"alpha_alert_delivery",status:"PASS",
      finished_at:$at,receiver_name:$recv,alertname:$name,
      delivery:{firing_id:$fire,resolved_id:$res,acknowledged_at:$ack,
                distinct:true,external:true},
      policy:{secret_values_recorded:false,local_sink:false}}' \
    > "$ALPHA_RECEIPT_DIR/P1-ALERT-DELIVERY.fire.json"
  chmod 600 "$ALPHA_RECEIPT_DIR/P1-ALERT-DELIVERY.fire.json"
  dest="$(alpha_write_receipt P1-ALERT-DELIVERY PASS alpha_alert_delivery)"
  alpha_log "PASS receipt: $dest"
}

record_pass() {
  alpha_load_env_optional
  alpha_require_execute_ready P1-ALERT-DELIVERY
  [ -f "$ALPHA_RECEIPT_DIR/P1-ALERT-DELIVERY.fire.json" ] \
    || alpha_die "missing fire receipt; run --execute first"
  dest="$(alpha_write_receipt P1-ALERT-DELIVERY PASS alpha_alert_delivery)"
  alpha_log "PASS receipt: $dest"
}

mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-runbook) mode=print ;;
    --check) mode=check ;;
    --execute) mode=execute ;;
    --record-pass) mode=record ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
[ -n "$mode" ] || mode=print

case "$mode" in
  print) print_runbook ;;
  check) check_only ;;
  execute) execute_fire_resolve ;;
  record) record_pass ;;
  *) usage ;;
esac
