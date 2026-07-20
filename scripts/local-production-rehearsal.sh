#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
mkdir -p "$ROOT/.artifacts"
exec > >(tee "$ROOT/.artifacts/local-production-rehearsal-last.log") 2>&1
COMPOSE="$ROOT/ops/local/compose.rehearsal.yml"
ART="${CX_LOCAL_ARTIFACT_DIR:-$ROOT/.artifacts/local-production-rehearsal}"
EVIDENCE="${CX_LOCAL_EVIDENCE_FILE:-$ROOT/evidence/autonomous/local-production-tls.json}"
PROJECT="${CX_LOCAL_PROJECT:-cx-local-production-rehearsal}"
CANDIDATE="${CX_LOCAL_CONTROL_IMAGE:-ghcr.io/joshuahickscorp/computexchange-control@sha256:f848a8048af250f7135f54b15d8bf4455bd24af6d42fd4d380dd99e0c1b91563}"
KEEP="${KEEP:-0}"
KEEP_AGENTS="${KEEP_AGENTS:-0}"
SOURCE_PROOF="${CX_LOCAL_SOURCE_PROOF:-0}"
AGENT_ONE=""
AGENT_TWO=""
STAGE="initialization"
MODEL_CACHE="${CX_MODEL_CACHE:-${HF_HOME:-$HOME/.cache/huggingface}}"

die() { printf 'local-production-rehearsal: %s\n' "$*" >&2; exit 1; }
compose() { docker compose -p "$PROJECT" -f "$COMPOSE" "$@"; }

for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
  value="${!name:-}"
  case "$value" in
    sk_live_*|rk_live_*|pk_live_*) die "$name is live-class; refusing before any network access" ;;
  esac
done
unset STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY STRIPE_WEBHOOK_SECRET \
  CX_CONNECT_WEBHOOK_SECRET CX_CONNECT_CLIENT_ID STRIPE_TEST_CONNECTED_ACCOUNT_ID

for tool in docker openssl curl jq cargo; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done
if [ "$SOURCE_PROOF" = 1 ]; then
  [[ "$CANDIDATE" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || die "local source proof image must use an immutable local image ID"
  CX_LOCAL_CONTROL_HEALTHCHECK="${CX_LOCAL_CONTROL_HEALTHCHECK:-/cx-healthcheck}"
else
  [[ "$CANDIDATE" =~ @sha256:[0-9a-f]{64}$ ]] \
    || die "candidate image must use an immutable registry digest"
  # The retained published images predate the dedicated probe. Keep the full
  # binary fallback narrowly scoped to immutable legacy registry images.
  CX_LOCAL_CONTROL_HEALTHCHECK="${CX_LOCAL_CONTROL_HEALTHCHECK:-/cx}"
fi

cleanup() {
  code=$?
  if [ "$code" -ne 0 ]; then
    printf 'local-production-rehearsal: failed during %s (exit %s)\n' "$STAGE" "$code" >&2
    compose logs --no-color --tail=200 > "$ART/failure-service-logs.txt" 2>&1 || true
  fi
  if [ "$KEEP_AGENTS" != 1 ]; then
    for pid in "$AGENT_ONE" "$AGENT_TWO"; do
      [ -z "$pid" ] || kill "$pid" >/dev/null 2>&1 || true
    done
  fi
  if [ "$KEEP" != 1 ]; then
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

rm -rf "$ART"
mkdir -p "$ART/tls" "$ART/agent1" "$ART/agent2" \
  "$ART/home/.compute-exchange/agent1" "$ART/home/.compute-exchange/agent2" "$(dirname "$EVIDENCE")"
chmod 700 "$ART" "$ART/tls"

openssl ecparam -name prime256v1 -genkey -noout -out "$ART/tls/ca.key"
openssl req -x509 -new -sha256 -key "$ART/tls/ca.key" -days 2 \
  -subj '/CN=ComputExchange local rehearsal CA' -out "$ART/tls/ca.crt"
openssl ecparam -name prime256v1 -genkey -noout -out "$ART/tls/server.key"
openssl req -new -sha256 -key "$ART/tls/server.key" -subj '/CN=cx.localhost' \
  -out "$ART/tls/server.csr"
printf '%s\n' \
  'subjectAltName=DNS:cx.localhost,DNS:storage.cx.localhost' \
  'keyUsage=digitalSignature,keyEncipherment' \
  'extendedKeyUsage=serverAuth' > "$ART/tls/server.ext"
openssl x509 -req -sha256 -in "$ART/tls/server.csr" -CA "$ART/tls/ca.crt" \
  -CAkey "$ART/tls/ca.key" -CAcreateserial -days 1 -extfile "$ART/tls/server.ext" \
  -out "$ART/tls/server.crt"
chmod 600 "$ART/tls/ca.key" "$ART/tls/server.key"

cat > "$ART/Caddyfile" <<'CADDY'
{
  admin off
}
https://cx.localhost {
  tls /certs/server.crt /certs/server.key
  encode zstd gzip
  header {
    Strict-Transport-Security "max-age=31536000"
    Content-Security-Policy "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; img-src 'self' https: data:; font-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; upgrade-insecure-requests"
    Permissions-Policy "camera=(), geolocation=(), microphone=(), payment=(), usb=()"
    X-Content-Type-Options "nosniff"
    X-Frame-Options "DENY"
    Referrer-Policy "no-referrer"
    -Server
  }
  @metrics path /metrics
  respond @metrics 404
  reverse_proxy control:8080
  log {
    output stdout
  }
}
https://storage.cx.localhost {
  tls /certs/server.crt /certs/server.key
  header {
    Strict-Transport-Security "max-age=31536000"
    X-Content-Type-Options "nosniff"
    Referrer-Policy "no-referrer"
    -Server
  }
  reverse_proxy minio:9000
  log {
    output stdout
  }
}
CADDY

cat > "$ART/prometheus.yml" <<'PROMETHEUS'
global:
  scrape_interval: 5s
  evaluation_interval: 5s
alerting:
  alertmanagers:
    - static_configs: [{targets: [alertmanager:9093]}]
scrape_configs:
  - job_name: computexchange-control
    static_configs: [{targets: [control:8080]}]
  - job_name: alertmanager
    static_configs: [{targets: [alertmanager:9093]}]
PROMETHEUS

cat > "$ART/alertmanager.yml" <<'ALERTMANAGER'
global:
  resolve_timeout: 30s
route:
  receiver: local-harness
  group_by: [alertname]
  group_wait: 1s
  group_interval: 2s
  repeat_interval: 30s
receivers:
  - name: local-harness
ALERTMANAGER

export CX_LOCAL_ASSET_DIR="$ART"
export CX_LOCAL_CONTROL_IMAGE="$CANDIDATE"
export CX_LOCAL_CONTROL_PLATFORM="${CX_LOCAL_CONTROL_PLATFORM:-linux/amd64}"
export CX_LOCAL_CONTROL_HEALTH_INTERVAL="${CX_LOCAL_CONTROL_HEALTH_INTERVAL:-5s}"
export CX_LOCAL_CONTROL_HEALTHCHECK
CX_LOCAL_POSTGRES_PASSWORD="local-pg-$(openssl rand -hex 24)"
CX_LOCAL_MINIO_USER="localminio$(openssl rand -hex 8)"
CX_LOCAL_MINIO_PASSWORD="local-minio-$(openssl rand -hex 24)"
CX_LOCAL_TOKEN_KEY="$(openssl rand -hex 32)"
CX_LOCAL_SAMPLE_SECRET="$(openssl rand -hex 32)"
export CX_LOCAL_POSTGRES_PASSWORD CX_LOCAL_MINIO_USER CX_LOCAL_MINIO_PASSWORD
export CX_LOCAL_TOKEN_KEY CX_LOCAL_SAMPLE_SECRET
umask 077
{
  printf 'export CX_LOCAL_ASSET_DIR=%q\n' "$CX_LOCAL_ASSET_DIR"
  printf 'export CX_LOCAL_CONTROL_IMAGE=%q\n' "$CX_LOCAL_CONTROL_IMAGE"
  printf 'export CX_LOCAL_CONTROL_PLATFORM=%q\n' "$CX_LOCAL_CONTROL_PLATFORM"
  printf 'export CX_LOCAL_CONTROL_HEALTHCHECK=%q\n' "$CX_LOCAL_CONTROL_HEALTHCHECK"
  printf 'export CX_LOCAL_CONTROL_HEALTH_INTERVAL=%q\n' "$CX_LOCAL_CONTROL_HEALTH_INTERVAL"
  printf 'export CX_LOCAL_POSTGRES_PASSWORD=%q\n' "$CX_LOCAL_POSTGRES_PASSWORD"
  printf 'export CX_LOCAL_MINIO_USER=%q\n' "$CX_LOCAL_MINIO_USER"
  printf 'export CX_LOCAL_MINIO_PASSWORD=%q\n' "$CX_LOCAL_MINIO_PASSWORD"
  printf 'export CX_LOCAL_TOKEN_KEY=%q\n' "$CX_LOCAL_TOKEN_KEY"
  printf 'export CX_LOCAL_SAMPLE_SECRET=%q\n' "$CX_LOCAL_SAMPLE_SECRET"
} > "$ART/runtime.env"
chmod 600 "$ART/runtime.env"

# A previous interrupted rehearsal must not leak credentials, database state,
# object bytes, or Caddy configuration into this run.
compose down --volumes --remove-orphans >/dev/null 2>&1 || true
while IFS= read -r image; do
  [[ "$image" =~ @sha256:[0-9a-f]{64}$ || "$image" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || die "mutable image rendered: $image"
done < <(compose config --images)

if [ "$SOURCE_PROOF" = 1 ]; then
  compose pull --quiet postgres minio createbuckets caddy prometheus alertmanager backup-worker
else
  compose pull --quiet
fi
if ! compose up -d --remove-orphans; then
  compose logs --no-color --tail=100 >&2 || true
  die "local topology failed during startup"
fi
for service in postgres minio control caddy prometheus alertmanager backup-worker; do
  deadline=$(( $(date +%s) + 240 ))
  while :; do
    cid="$(compose ps -q "$service")"
    observed_state=""
    if [ -n "$cid" ]; then
      observed_state="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || true)"
    fi
    case "$observed_state" in
      healthy) break ;;
      exited|dead) die "$service failed health readiness ($observed_state)" ;;
    esac
    [ "$(date +%s)" -lt "$deadline" ] || die "$service failed health readiness ($observed_state)"
    sleep 2
  done
done
docker exec "$(compose ps -q control)" /cx seed >/dev/null

STAGE="TLS and HTTP assertions"
CURL=(curl --noproxy '*' --silent --show-error --fail-with-body --cacert "$ART/tls/ca.crt" \
  --resolve cx.localhost:18443:127.0.0.1)
deadline=$(( $(date +%s) + 60 ))
until "${CURL[@]}" https://cx.localhost:18443/healthz >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "TLS reverse proxy did not accept connections"
  sleep 1
done
"${CURL[@]}" https://cx.localhost:18443/readyz >/dev/null
site_body="$("${CURL[@]}" https://cx.localhost:18443/)"
rg -qi 'computexchange' <<< "$site_body"
if curl --noproxy '*' --silent --fail --cacert "$ART/tls/ca.crt" \
  --resolve wrong.localhost:18443:127.0.0.1 https://wrong.localhost:18443/healthz >/dev/null 2>&1; then
  die "wrong-hostname TLS request succeeded"
fi
if curl --noproxy '*' --silent --fail --resolve cx.localhost:18443:127.0.0.1 \
  https://cx.localhost:18443/healthz >/dev/null 2>&1; then
  die "unknown-CA TLS request succeeded"
fi
future="$(( $(date +%s) + 172800 ))"
if openssl verify -CAfile "$ART/tls/ca.crt" -attime "$future" "$ART/tls/server.crt" >/dev/null 2>&1; then
  die "certificate remained valid past its expiry"
fi
headers="$(curl --noproxy '*' --silent --show-error --head --resolve cx.localhost:18080:127.0.0.1 \
  http://cx.localhost:18080/)"
rg -qi '^HTTP/[0-9.]+ 30[18]' <<< "$headers" || die "HTTP was not redirected"
tls_headers="$("${CURL[@]}" --head https://cx.localhost:18443/)"
rg -qi '^strict-transport-security: max-age=' <<< "$tls_headers" || die "HSTS missing"
rg -qi '^content-security-policy:' <<< "$tls_headers" || die "CSP missing"
status="$("${CURL[@]}" --output /dev/null --write-out '%{http_code}' \
  https://cx.localhost:18443/v1/jobs/00000000-0000-0000-0000-000000000001 || true)"
[ "$status" = 401 ] || die "buyer API did not require authentication ($status)"
status="$("${CURL[@]}" --output /dev/null --write-out '%{http_code}' \
  https://cx.localhost:18443/admin/workers || true)"
[ "$status" = 401 ] || die "operator route did not require authentication ($status)"

STAGE="agent build and registration"
export CARGO_TARGET_DIR="${CARGO_TARGET_DIR:-$ROOT/.artifacts/local-production-cargo-target}"
cargo build --release --manifest-path "$ROOT/agent/Cargo.toml" >/dev/null
AGENT_BIN="$CARGO_TARGET_DIR/release/cx-agent"
for n in 1 2; do
  printf 'control_url = "https://cx.localhost:18443"\nworker_token = "dev-worker-token-000%s"\n' "$n" > "$ART/agent$n/config.toml"
  printf 'supplier_id = "00000000-0000-0000-0000-0000000000a%s"\n' "$n" >> "$ART/agent$n/config.toml"
  printf 'max_cpu_pct = 100.0\npower_only = false\nmin_payout_usd_per_hr = 0.0\n' >> "$ART/agent$n/config.toml"
  printf 'memory_headroom_gb = 0.0\nmax_memory_pct = 0.0\ndata_dir = "%s/home/.compute-exchange/agent%s"\n' "$ART" "$n" >> "$ART/agent$n/config.toml"
done
start_agent() {
  n="$1"
  HOME="$ART/home" CX_MODEL_CACHE="$MODEL_CACHE" \
    CX_TLS_CA_FILE="$ART/tls/ca.crt" CX_REQUIRE_SANDBOX=1 \
    CX_SANDBOX_PROFILE="$ROOT/macapp/ComputeExchangeAgent/cx-agent.sb" \
    "$AGENT_BIN" run --config "$ART/agent$n/config.toml" > "$ART/agent$n.log" 2>&1 &
  STARTED_AGENT_PID=$!
}
wait_agents() {
  expected="$1"
  local workers worker_count
  deadline=$(( $(date +%s) + 360 ))
  while :; do
    # Caddy can briefly recycle while the host agent is loading its model
    # runtime on resource-constrained Docker Desktop/Colima hosts. Treat that
    # as a readiness transition, not as a failed registration assertion.
    workers="$("${CURL[@]}" https://cx.localhost:18443/admin/workers \
      -H 'Authorization: Bearer dev-admin-key-0001' 2>/dev/null || true)"
    worker_count="$(jq -r 'if type == "array" then [.[] | select(.version != "seed")] | length else 0 end' \
      <<< "$workers" 2>/dev/null || printf '0')"
    worker_count="${worker_count:-0}"
    [ "$worker_count" -ge "$expected" ] && return
    [ "$(date +%s)" -lt "$deadline" ] || die "$expected TLS-validating agent(s) did not register"
    sleep 3
  done
}
start_agent 1
AGENT_ONE="$STARTED_AGENT_PID"
wait_agents 1
start_agent 2
AGENT_TWO="$STARTED_AGENT_PID"
wait_agents 2
printf '%s\n' "$AGENT_ONE" > "$ART/agent1.pid"
printf '%s\n' "$AGENT_TWO" > "$ART/agent2.pid"
deadline=$(( $(date +%s) + 120 ))
until "${CURL[@]}" https://cx.localhost:18443/healthz >/dev/null 2>&1 && \
  "${CURL[@]}" https://cx.localhost:18443/readyz >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "TLS proxy did not remain ready after agent registration"
  sleep 2
done

STAGE="final invariants and evidence"
ledger="$(compose exec -T postgres psql -X -qAt -U cx -d cx -c \
  "SELECT json_build_object('sum',COALESCE(sum(amount_usd),0)::text,
  'duplicates',(SELECT count(*) FROM (SELECT task_id,kind,count(*) FROM ledger_entries
  WHERE task_id IS NOT NULL GROUP BY task_id,kind HAVING count(*)>1) d))::text FROM ledger_entries")"
jq -e '(.sum | tonumber) as $sum | $sum > -0.000001 and $sum < 0.000001 and .duplicates == 0' \
  <<< "$ledger" >/dev/null \
  || die "ledger invariants failed"
curl --silent --fail http://127.0.0.1:19090/-/ready >/dev/null
curl --silent --fail http://127.0.0.1:19093/-/ready >/dev/null
compose logs --no-color --tail=20 control caddy > "$ART/service-logs.txt"

version="$("${CURL[@]}" https://cx.localhost:18443/version)"
host_arch="$(docker info --format '{{.Architecture}}')"
image_arch="$(docker image inspect "$CANDIDATE" --format '{{.Architecture}}')"
finished="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
temporary="$EVIDENCE.tmp.$$"
jq -n --arg finished "$finished" --arg image "$CANDIDATE" --argjson version "$version" \
  --arg host_arch "$host_arch" --arg image_arch "$image_arch" --argjson ledger "$ledger" \
  --argjson source_proof "$SOURCE_PROOF" '
  {schema_version:1,status:"PASS",label:"LOCAL PRODUCTION-SHAPED TLS",
   completed_at:$finished,candidate_image:$image,reported_version:$version,
   topology:{tls_reverse_proxy:true,control_by_digest:true,postgresql:true,s3_compatible_storage:true,
     website:true,metrics:true,logs:true,alert_router:true,backup_worker:true,
     metal_agent_processes:2,distinct_physical_devices:1},
   tls:{valid_certificate:true,wrong_hostname_rejected:true,unknown_ca_rejected:true,
     expired_certificate_rejected:true,insecure_bypass_used:false,http_redirect:true,hsts:true,csp:true},
   authorization:{api_requires_auth:true,operator_routes_protected:true,worker_tls_validated:true},
   runtime_architecture:{host:$host_arch,candidate_image:$image_arch,
     cross_architecture_emulation:($host_arch != $image_arch),
     candidate_workload_execution:"NOT CLAIMED BY THIS TLS RECEIPT",
     proof_scope:(if $source_proof == 1 then "immutable local source-equivalent image" else "published registry candidate" end),
     note:(if $source_proof == 1 then
       "Native image is bound by immutable local image ID; workload execution is proved separately."
       else "The amd64 candidate transport was exercised on an arm64 Docker host; workload execution is proved separately by make prove-local." end)},
   ledger:$ledger,
   external:{persistent_external_tls:"NOT EXECUTED",stripe_sandbox:"NOT EXECUTED",
     stripe_live:"PROHIBITED"}}' > "$temporary"
mv "$temporary" "$EVIDENCE"
printf 'PASS local production-shaped TLS receipt: %s\n' "$EVIDENCE"
