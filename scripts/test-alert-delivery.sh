#!/usr/bin/env bash
# Prove Alertmanager fire → local sink receive → resolve → resolved delivery.
# Status is derived only from observed sink payloads. Empty sink = hard fail.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
COMPOSE_FILE="$ROOT/docker-compose.observability.yml"
PROJECT="cx-alert-delivery-$$"
ART="${MERC_ALERT_DELIVERY_DIR:-$ROOT/.artifacts/alert-delivery}/$PROJECT"
EVIDENCE_OUT="${MERC_ALERT_DELIVERY_EVIDENCE:-$ROOT/evidence/autonomous/alert-delivery.json}"
SINK_PID=""
COMPOSE_UP=0

die() { echo "[test-alert-delivery] ERROR: $*" >&2; exit 1; }
log() { echo "[test-alert-delivery] $*"; }

cleanup() {
  code=$?
  if [ "$COMPOSE_UP" = 1 ]; then
    MERC_ALERT_RECEIVER_URL_FILE="${ART}/receiver.url" \
      docker compose -p "$PROJECT" -f "$COMPOSE_FILE" -f "$ART/docker-compose.override.yml" \
      down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  if [ -n "$SINK_PID" ] && kill -0 "$SINK_PID" 2>/dev/null; then
    kill "$SINK_PID" 2>/dev/null || true
    wait "$SINK_PID" 2>/dev/null || true
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

for tool in docker python3 jq curl; do
  command -v "$tool" >/dev/null 2>&1 || die "missing dependency: $tool"
done
[ -f "$COMPOSE_FILE" ] || die "missing $COMPOSE_FILE"
[ -f "$ROOT/monitoring/alertmanager.yml" ] || die "missing monitoring/alertmanager.yml"

mkdir -p "$ART" "$(dirname "$EVIDENCE_OUT")"
SINK_LOG="$ART/sink-events.jsonl"
: >"$SINK_LOG"

# Production alertmanager.yml uses group_wait=30s and group_interval=5m. For a
# local fire→resolve proof we keep the same receiver contract (url_file secret,
# send_resolved) but shrink timing so the loop is observable without a 5m wait.
# Receiver routing and secret path remain identical to monitoring/alertmanager.yml.
cat >"$ART/alertmanager.yml" <<'YAML'
global:
  resolve_timeout: 1m

route:
  receiver: real-canary-receiver
  group_by: [alertname, severity]
  group_wait: 2s
  group_interval: 5s
  repeat_interval: 4h
  routes:
    - receiver: real-canary-receiver
      matchers: ['severity="page"']
      repeat_interval: 30m
    - receiver: real-canary-receiver
      matchers: ['severity="ticket"']
      repeat_interval: 12h

receivers:
  - name: real-canary-receiver
    webhook_configs:
      - url_file: /run/secrets/cx_alert_receiver_url
        send_resolved: true
        max_alerts: 20
        timeout: 10s
YAML

# Compose override mounts the timing-shortened config over the repo default.
cat >"$ART/docker-compose.override.yml" <<YAML
services:
  alertmanager:
    volumes:
      - ${ART}/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro
YAML

# Free high port on localhost.
SINK_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
[ -n "$SINK_PORT" ] || die "could not allocate sink port"

# Bind 0.0.0.0 so the colima VM can reach the host via host.docker.internal.
python3 - "$SINK_PORT" "$SINK_LOG" <<'PY' &
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port = int(sys.argv[1])
path = sys.argv[2]

class H(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        return

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        received_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        try:
            body = json.loads(raw.decode("utf-8") or "{}")
        except json.JSONDecodeError:
            body = {"_raw": raw.decode("utf-8", errors="replace")}
        rec = {
            "received_at": received_at,
            "path": self.path,
            "content_type": self.headers.get("Content-Type", ""),
            "body": body,
        }
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(json.dumps(rec, separators=(",", ":")) + "\n")
            fh.flush()
        self.send_response(204)
        self.end_headers()

ThreadingHTTPServer(("0.0.0.0", port), H).serve_forever()
PY
SINK_PID=$!
sleep 0.3
kill -0 "$SINK_PID" 2>/dev/null || die "sink failed to start"

RECEIVER_URL="http://host.docker.internal:${SINK_PORT}/alertmanager"
printf '%s\n' "$RECEIVER_URL" >"$ART/receiver.url"
export MERC_ALERT_RECEIVER_URL_FILE="$ART/receiver.url"

log "starting prometheus + alertmanager (project=$PROJECT) → sink $RECEIVER_URL"
docker compose -p "$PROJECT" -f "$COMPOSE_FILE" -f "$ART/docker-compose.override.yml" \
  up -d prometheus alertmanager \
  >"$ART/compose-up.log" 2>&1 || {
  cat "$ART/compose-up.log" >&2
  die "docker compose up failed"
}
COMPOSE_UP=1

deadline=$(( $(date +%s) + 90 ))
until curl -fsS "http://127.0.0.1:9093/-/ready" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "alertmanager not ready"
  sleep 1
done
deadline=$(( $(date +%s) + 90 ))
until curl -fsS "http://127.0.0.1:9090/-/ready" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "prometheus not ready"
  sleep 1
done
log "prometheus and alertmanager ready"

ALERT_NAME="MercSyntheticLocalDelivery"
FINGERPRINT_LABEL="cx-local-alert-delivery-$$"
STARTS_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# endsAt far in the future keeps the alert firing for the API.
ENDS_AT_FIRING="$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)"

FIRE_PAYLOAD="$(jq -nc \
  --arg name "$ALERT_NAME" \
  --arg fp "$FINGERPRINT_LABEL" \
  --arg starts "$STARTS_AT" \
  --arg ends "$ENDS_AT_FIRING" \
  '[{
      labels:{alertname:$name,severity:"page",instance:$fp,job:"alert-delivery-proof"},
      annotations:{summary:"synthetic local alert delivery proof",runbook:"docs/RUNBOOKS.md#control-plane-or-database-outage"},
      startsAt:$starts,
      endsAt:$ends
    }]')"
printf '%s\n' "$FIRE_PAYLOAD" >"$ART/fire-payload.json"

log "posting firing synthetic alert to alertmanager API"
curl -fsS -X POST -H 'Content-Type: application/json' \
  --data @"$ART/fire-payload.json" \
  "http://127.0.0.1:9093/api/v2/alerts" >/dev/null \
  || die "failed to post firing alert"

# alertmanager.yml group_wait is 30s; allow headroom for grouping + webhook.
wait_for_status() {
  local want="$1" timeout_s="$2" found="" line
  local end=$(( $(date +%s) + timeout_s ))
  while [ "$(date +%s)" -lt "$end" ]; do
    if [ -s "$SINK_LOG" ]; then
      while IFS= read -r line; do
        [ -n "$line" ] || continue
        if echo "$line" | jq -e --arg want "$want" --arg name "$ALERT_NAME" '
            (.body.status == $want)
            and (
              ([.body.alerts[]? | select(.labels.alertname == $name)] | length) > 0
              or (.body.commonLabels.alertname == $name)
              or (.body.groupLabels.alertname == $name)
            )
          ' >/dev/null 2>&1; then
          found="$line"
          break
        fi
      done <"$SINK_LOG"
    fi
    [ -n "$found" ] && { printf '%s\n' "$found"; return 0; }
    sleep 1
  done
  return 1
}

log "waiting for firing delivery at sink..."
FIRING_EVENT="$(wait_for_status firing 120)" || {
  log "sink log so far:"
  cat "$SINK_LOG" >&2 || true
  die "sink received no firing webhook for $ALERT_NAME within 120s"
}
FIRING_AT="$(echo "$FIRING_EVENT" | jq -r '.received_at')"
FIRING_FP="$(echo "$FIRING_EVENT" | jq -r '
  [.body.alerts[]? | select(.labels.alertname != null) | .fingerprint][0]
  // .body.groupKey // "unknown"
')"
log "firing delivery observed at=$FIRING_AT fingerprint/group=$FIRING_FP"

# Resolve by posting endsAt = now (must be >= startsAt). Alertmanager treats an
# alert with endsAt in the past-or-present as resolved for matching labels.
ENDS_AT_RESOLVED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Guard against clock skew where now < startsAt (would 400).
if [[ "$ENDS_AT_RESOLVED" < "$STARTS_AT" ]]; then
  ENDS_AT_RESOLVED="$STARTS_AT"
fi
RESOLVE_PAYLOAD="$(jq -nc \
  --arg name "$ALERT_NAME" \
  --arg fp "$FINGERPRINT_LABEL" \
  --arg starts "$STARTS_AT" \
  --arg ends "$ENDS_AT_RESOLVED" \
  '[{
      labels:{alertname:$name,severity:"page",instance:$fp,job:"alert-delivery-proof"},
      annotations:{summary:"synthetic local alert delivery proof",runbook:"docs/RUNBOOKS.md#control-plane-or-database-outage"},
      startsAt:$starts,
      endsAt:$ends
    }]')"
printf '%s\n' "$RESOLVE_PAYLOAD" >"$ART/resolve-payload.json"

log "posting resolved synthetic alert to alertmanager API"
RESOLVE_HTTP="$(curl -sS -o "$ART/resolve-response.txt" -w '%{http_code}' -X POST \
  -H 'Content-Type: application/json' \
  --data @"$ART/resolve-payload.json" \
  "http://127.0.0.1:9093/api/v2/alerts" || true)"
if [ "$RESOLVE_HTTP" != "200" ]; then
  log "resolve response body: $(cat "$ART/resolve-response.txt" 2>/dev/null || true)"
  die "failed to post resolved alert (HTTP $RESOLVE_HTTP)"
fi

log "waiting for resolved delivery at sink..."
RESOLVED_EVENT="$(wait_for_status resolved 120)" || {
  log "sink log so far:"
  cat "$SINK_LOG" >&2 || true
  die "sink received no resolved webhook for $ALERT_NAME within 120s"
}
RESOLVED_AT="$(echo "$RESOLVED_EVENT" | jq -r '.received_at')"
RESOLVED_FP="$(echo "$RESOLVED_EVENT" | jq -r '
  [.body.alerts[]? | select(.labels.alertname != null) | .fingerprint][0]
  // .body.groupKey // "unknown"
')"
log "resolved delivery observed at=$RESOLVED_AT fingerprint/group=$RESOLVED_FP"

SINK_LINES="$(wc -l <"$SINK_LOG" | tr -d ' ')"
[ "${SINK_LINES:-0}" -ge 2 ] || die "sink event count $SINK_LINES < 2"

# Derive status strictly from observations (never assert PASS unconditionally).
STATUS="$(jq -nr \
  --arg firing_at "$FIRING_AT" \
  --arg resolved_at "$RESOLVED_AT" \
  --argjson sink_lines "$SINK_LINES" \
  --arg firing_fp "$FIRING_FP" \
  --arg resolved_fp "$RESOLVED_FP" \
  '
  ($firing_at != "" and $firing_at != "null") as $got_fire
  | ($resolved_at != "" and $resolved_at != "null") as $got_resolve
  | ($sink_lines >= 2) as $enough
  | if ($got_fire and $got_resolve and $enough) then "PASS" else "FAIL" end
  ')"

jq -n \
  --arg status "$STATUS" \
  --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg project "$PROJECT" \
  --arg alertname "$ALERT_NAME" \
  --arg receiver_url_scheme "http://host.docker.internal" \
  --arg firing_received_at "$FIRING_AT" \
  --arg resolved_received_at "$RESOLVED_AT" \
  --arg firing_fingerprint "$FIRING_FP" \
  --arg resolved_fingerprint "$RESOLVED_FP" \
  --argjson sink_event_count "$SINK_LINES" \
  --arg sink_log "$SINK_LOG" \
  --arg compose_file "docker-compose.observability.yml" \
  --argjson firing_event "$FIRING_EVENT" \
  --argjson resolved_event "$RESOLVED_EVENT" \
  '{
    schema_version: 1,
    kind: "alert_delivery",
    status: $status,
    completed_at: $completed_at,
    compose_file: $compose_file,
    compose_project: $project,
    alertname: $alertname,
    receiver: {
      transport: "alertmanager_webhook",
      url_host: $receiver_url_scheme,
      secret_values_recorded: false
    },
    delivery: {
      firing_received_at: $firing_received_at,
      resolved_received_at: $resolved_received_at,
      firing_fingerprint: $firing_fingerprint,
      resolved_fingerprint: $resolved_fingerprint,
      sink_event_count: $sink_event_count,
      sink_log: $sink_log
    },
    observations: {
      firing: $firing_event,
      resolved: $resolved_event
    },
    assertions_derived_from_sink: [
      "firing webhook body.status observed",
      "resolved webhook body.status observed",
      "sink_event_count >= 2"
    ]
  }' >"$ART/evidence.json"

cp "$ART/evidence.json" "$EVIDENCE_OUT"
log "status=$STATUS evidence=$EVIDENCE_OUT"
[ "$STATUS" = "PASS" ] || die "derived status is $STATUS (sink did not prove fire+resolve)"
log "PASS fire → receive → resolve observed at local sink"
