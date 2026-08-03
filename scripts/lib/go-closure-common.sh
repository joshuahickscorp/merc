#!/usr/bin/env bash

# Shared fail-closed primitives for the GO-closure staging scripts. Callers set
# `set -euo pipefail` before sourcing this file.

GC_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
GC_COMPOSE="$GC_ROOT/ops/staging/compose.go-closure.yml"
GC_ENV_FILE="${MERC_GO_CLOSURE_ENV_FILE:-$GC_ROOT/.env.go-closure}"
GC_INPUTS="$GC_ROOT/ops/go-closure-inputs.json"

gc_die() {
  printf 'go-closure: %s\n' "$*" >&2
  exit 1
}

gc_log() {
  printf 'go-closure: %s\n' "$*" >&2
}

gc_require_command() {
  command -v "$1" >/dev/null 2>&1 || gc_die "$1 is required"
}

gc_require_value() {
  local name="$1"
  [ -n "${!name:-}" ] || gc_die "$name is required in $GC_ENV_FILE"
  case "${!name}" in
    *REPLACE*|*example.invalid*) gc_die "$name still contains a template placeholder" ;;
  esac
}

gc_env_mode() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

gc_load_env() {
  [ -f "$GC_INPUTS" ] || gc_die "missing operator-input declaration: $GC_INPUTS"
  jq -e '.schema_version == 1 and (.inputs | type == "array")' "$GC_INPUTS" >/dev/null \
    || gc_die "invalid operator-input declaration: $GC_INPUTS"
  [ -f "$GC_ENV_FILE" ] || gc_die "missing ignored operator file: $GC_ENV_FILE"
  [ ! -L "$GC_ENV_FILE" ] || gc_die "$GC_ENV_FILE must not be a symlink"
  local mode
  mode="$(gc_env_mode "$GC_ENV_FILE")"
  [[ "$mode" =~ ^[0-7]*00$ ]] \
    || gc_die "$GC_ENV_FILE permissions are $mode; require no group/other access (chmod 600)"
  set -a
  # shellcheck disable=SC1090
  . "$GC_ENV_FILE"
  set +a
  gc_reject_live_stripe_environment
}

gc_reject_live_stripe_environment() {
  local name value
  for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
    STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
    value="${!name:-}"
    case "$value" in
      sk_live_*|rk_live_*|pk_live_*)
        gc_die "$name has a live Stripe credential class; closure commands refuse it before network access"
        ;;
    esac
  done
  [ "${MERC_PAYMENT_PROVIDER:-}" != simulator ] \
    || gc_die "the payment simulator is CLI/test-only and cannot be selected by staging"
}

gc_require_declared_inputs() {
  local name
  for name in "$@"; do
    jq -e --arg name "$name" '.inputs | any(.name == $name)' "$GC_INPUTS" >/dev/null \
      || gc_die "$name is not declared in ops/go-closure-inputs.json"
    gc_require_value "$name"
  done
}

gc_validate_image_ref() {
  local name="$1" value="${!1:-}"
  gc_require_value "$name"
  [[ "$value" =~ ^[a-zA-Z0-9._:-]+(/[a-zA-Z0-9._-]+)+@sha256:[0-9a-f]{64}$ ]] \
    || gc_die "$name must be an immutable registry/repository@sha256:<64 lowercase hex> reference"
}

gc_validate_commit() {
  local name="$1" value="${!1:-}"
  gc_require_value "$name"
  [[ "$value" =~ ^[0-9a-f]{40}$ ]] || gc_die "$name must be a full lowercase 40-character commit"
}

gc_validate_hostname() {
  local name="$1" value="${!1:-}"
  gc_require_value "$name"
  [[ "$value" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] \
    || gc_die "$name is not a valid DNS hostname"
  [[ "$value" == *.* ]] || gc_die "$name must be a fully qualified DNS hostname"
}

gc_validate_absolute_path() {
  local name="$1" value="${!1:-}"
  gc_require_value "$name"
  [[ "$value" =~ ^/[A-Za-z0-9._/-]+$ ]] || gc_die "$name must be a shell-safe absolute path"
  [ "$value" != / ] || gc_die "$name must not be /"
  [ "${#value}" -ge 8 ] || gc_die "$name is too broad"
  [[ "$value" != */../* && "$value" != */.. && "$value" != *'/./'* ]] \
    || gc_die "$name must not contain dot path traversal"
}

gc_require_positive_at_most() {
  local name="$1" maximum="$2" value="${!1:-}"
  gc_require_value "$name"
  awk -v value="$value" -v maximum="$maximum" \
    'BEGIN { exit !(value ~ /^[0-9]+([.][0-9]+)?$/ && value > 0 && value <= maximum) }' \
    || gc_die "$name must be positive and at most $maximum"
}

gc_validate_ssh_target() {
  gc_require_declared_inputs STAGING_SSH_TARGET
  [[ "$STAGING_SSH_TARGET" =~ ^[A-Za-z0-9._@:-]+$ ]] \
    || gc_die "STAGING_SSH_TARGET contains unsupported characters"
}

gc_validate_host_config() {
  gc_require_command jq
  gc_require_command curl
  gc_require_command docker
  docker compose version >/dev/null 2>&1 || gc_die "Docker Compose v2 is required"
  gc_require_declared_inputs STAGING_TLS_HOSTNAME STAGING_DEPLOYMENT_ROOT \
    STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET MERC_CONNECT_WEBHOOK_SECRET \
    STRIPE_PUBLISHABLE_KEY MERC_CONNECT_CLIENT_ID \
    MERC_SUPPORT_EMAIL MERC_SECURITY_EMAIL MERC_STATUS_URL MERC_TERMS_URL MERC_PRIVACY_URL \
    ALERT_RECEIVER_WEBHOOK_URL ALERT_RECEIVER_NAME \
    STAGING_STORAGE_TLS_HOSTNAME STAGING_BIND_ADDRESS ACME_EMAIL \
    POSTGRES_PASSWORD MINIO_ROOT_USER MINIO_ROOT_PASSWORD \
    GF_SECURITY_ADMIN_PASSWORD MERC_TOKEN_KEY MERC_VERIFICATION_SAMPLE_SECRET \
    MERC_CANDIDATE_CONTROL_IMAGE MERC_CANDIDATE_COMMIT \
    MERC_PRIOR_CONTROL_IMAGE MERC_PRIOR_COMMIT \
    MERC_PROMETHEUS_IMAGE MERC_ALERTMANAGER_IMAGE MERC_GRAFANA_IMAGE MERC_NODE_EXPORTER_IMAGE
  gc_validate_hostname STAGING_TLS_HOSTNAME
  gc_validate_hostname STAGING_STORAGE_TLS_HOSTNAME
  gc_validate_absolute_path STAGING_DEPLOYMENT_ROOT
  gc_require_value STAGING_BIND_ADDRESS
  [[ "$STAGING_BIND_ADDRESS" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] \
    || gc_die "STAGING_BIND_ADDRESS must be a specific IPv4 address"
  awk -F. 'NF == 4 {for (i=1; i<=4; i++) if ($i < 0 || $i > 255) exit 1; exit 0} {exit 1}' \
    <<< "$STAGING_BIND_ADDRESS" || gc_die "STAGING_BIND_ADDRESS contains an invalid IPv4 octet"
  [ "$STAGING_BIND_ADDRESS" != 0.0.0.0 ] && [ "$STAGING_BIND_ADDRESS" != 127.0.0.1 ] \
    || gc_die "STAGING_BIND_ADDRESS must be non-wildcard and externally reachable"
  case "$STAGING_BIND_ADDRESS" in
    192.0.2.*|198.51.100.*|203.0.113.*) gc_die "STAGING_BIND_ADDRESS still uses a documentation-only subnet" ;;
  esac
  [ "$GC_ROOT" = "$(cd "$STAGING_DEPLOYMENT_ROOT" 2>/dev/null && pwd -P)" ] \
    || gc_die "script root $GC_ROOT does not match STAGING_DEPLOYMENT_ROOT $STAGING_DEPLOYMENT_ROOT"
  [[ "$STRIPE_SECRET_KEY" == sk_test_* || "$STRIPE_SECRET_KEY" == rk_test_* ]] \
    || gc_die "STRIPE_SECRET_KEY must be sk_test_* or a sufficiently scoped rk_test_*"
  [[ "$STRIPE_PUBLISHABLE_KEY" == pk_test_* ]] \
    || gc_die "STRIPE_PUBLISHABLE_KEY must be a pk_test_* browser key"
  [[ "$STRIPE_WEBHOOK_SECRET" == whsec_* ]] || gc_die "STRIPE_WEBHOOK_SECRET must be whsec_*"
  [[ "$MERC_CONNECT_WEBHOOK_SECRET" == whsec_* ]] || gc_die "MERC_CONNECT_WEBHOOK_SECRET must be whsec_*"
  [[ "$MERC_CONNECT_CLIENT_ID" == ca_* ]] || gc_die "MERC_CONNECT_CLIENT_ID must be a test-mode ca_* identifier"
  [ "$STRIPE_WEBHOOK_SECRET" != "$MERC_CONNECT_WEBHOOK_SECRET" ] \
    || gc_die "Stripe billing and Connect webhook secrets must be distinct"
  for name in MERC_SUPPORT_EMAIL MERC_SECURITY_EMAIL; do
    [[ "${!name}" =~ ^[^@[:space:]]+@[^@[:space:]]+[.][^@[:space:]]+$ ]] \
      || gc_die "$name must be a staffed email address"
  done
  for name in MERC_STATUS_URL MERC_TERMS_URL MERC_PRIVACY_URL; do
    [[ "${!name}" =~ ^https://[^[:space:]/]+(/[^[:space:]]*)?$ ]] \
      || gc_die "$name must be an absolute HTTPS URL"
  done
  [[ "$ALERT_RECEIVER_WEBHOOK_URL" == https://* ]] \
    || gc_die "ALERT_RECEIVER_WEBHOOK_URL must use HTTPS"
  gc_validate_image_ref MERC_CANDIDATE_CONTROL_IMAGE
  gc_validate_image_ref MERC_PRIOR_CONTROL_IMAGE
  gc_validate_image_ref MERC_PROMETHEUS_IMAGE
  gc_validate_image_ref MERC_ALERTMANAGER_IMAGE
  gc_validate_image_ref MERC_GRAFANA_IMAGE
  gc_validate_image_ref MERC_NODE_EXPORTER_IMAGE
  gc_validate_commit MERC_CANDIDATE_COMMIT
  gc_validate_commit MERC_PRIOR_COMMIT
  [ "$MERC_CANDIDATE_CONTROL_IMAGE" != "$MERC_PRIOR_CONTROL_IMAGE" ] \
    || gc_die "candidate and prior image digests must differ"
  [ "$MERC_CANDIDATE_COMMIT" != "$MERC_PRIOR_COMMIT" ] \
    || gc_die "candidate and prior commits must differ"
  [ "${#MERC_TOKEN_KEY}" -ge 32 ] || gc_die "MERC_TOKEN_KEY must contain at least 32 bytes"
  [ "${#MERC_VERIFICATION_SAMPLE_SECRET}" -ge 32 ] \
    || gc_die "MERC_VERIFICATION_SAMPLE_SECRET must contain at least 32 bytes"
  [ "$MERC_TOKEN_KEY" != "$MERC_VERIFICATION_SAMPLE_SECRET" ] \
    || gc_die "token and verification secrets must be distinct"
  [[ "$POSTGRES_PASSWORD" =~ ^[A-Za-z0-9._~-]{32,}$ ]] \
    || gc_die "POSTGRES_PASSWORD must be at least 32 URL-safe characters"
  [[ "$MINIO_ROOT_USER" =~ ^[A-Za-z0-9]{16,}$ ]] \
    || gc_die "MINIO_ROOT_USER must be at least 16 alphanumeric characters"
  [[ "$MINIO_ROOT_PASSWORD" =~ ^[A-Za-z0-9._~-]{32,}$ ]] \
    || gc_die "MINIO_ROOT_PASSWORD must be at least 32 URL-safe characters"
  [ "${#GF_SECURITY_ADMIN_PASSWORD}" -ge 32 ] \
    || gc_die "GF_SECURITY_ADMIN_PASSWORD must contain at least 32 bytes"
  [[ "$ACME_EMAIL" == *@*.* ]] || gc_die "ACME_EMAIL must be an email address"
  gc_require_declared_inputs MERC_CANARY_APPROVED_BUYER_EMAILS MERC_CANARY_APPROVED_WORKER_IDS \
    MERC_CANARY_APPROVED_AGENT_VERSIONS MERC_CANARY_APPROVED_BUILD_HASHES
  [ "$(jq -Rn --arg value "$MERC_CANARY_APPROVED_BUYER_EMAILS" \
      '$value | split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) | map(select(length > 0)) | unique | length')" -eq 2 ] \
    || gc_die "MERC_CANARY_APPROVED_BUYER_EMAILS must contain exactly two distinct buyers"
  jq -en --arg value "$MERC_CANARY_APPROVED_WORKER_IDS" '
    ($value | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) as $ids |
    ($ids | length) == 2 and ($ids | unique | length) == 2 and
    all($ids[]; test("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$"))
  ' >/dev/null || gc_die "MERC_CANARY_APPROVED_WORKER_IDS must contain exactly two distinct UUIDs"
  jq -en --arg value "$MERC_CANARY_APPROVED_AGENT_VERSIONS" '
    ($value | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) as $items |
    ($items | length) >= 1 and ($items | length) <= 4 and
    ($items | unique | length) == ($items | length) and
    all($items[]; test("^[0-9]+[.][0-9]+[.][0-9]+([+-][0-9A-Za-z.-]+)?$"))
  ' >/dev/null || gc_die "MERC_CANARY_APPROVED_AGENT_VERSIONS must contain one to four distinct reviewed semvers"
  jq -en --arg value "$MERC_CANARY_APPROVED_BUILD_HASHES" '
    ($value | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) as $items |
    ($items | length) >= 1 and ($items | length) <= 4 and
    ($items | unique | length) == ($items | length) and
    all($items[]; test("^[0-9a-f]{16}$"))
  ' >/dev/null || gc_die "MERC_CANARY_APPROVED_BUILD_HASHES must contain one to four distinct reviewed 16-hex hashes"
  [ -f "$GC_COMPOSE" ] || gc_die "missing compose manifest: $GC_COMPOSE"
}

gc_wait_http() {
  local url="$1" timeout="${2:-180}" deadline
  deadline=$(( $(date +%s) + timeout ))
  while ! curl --fail --silent --show-error --max-time 15 "$url" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || gc_die "$url did not become ready within ${timeout}s"
    sleep 3
  done
}

gc_materialize_alert_secret() {
  local secret_dir="$GC_ROOT/.secrets/go-closure"
  local secret_file="$secret_dir/cx_alert_receiver_url"
  mkdir -p "$secret_dir"
  chmod 700 "$secret_dir"
  umask 077
  printf '%s' "$ALERT_RECEIVER_WEBHOOK_URL" > "$secret_file"
  chmod 600 "$secret_file"
}

gc_compose() {
  docker compose --env-file "$GC_ENV_FILE" -f "$GC_COMPOSE" "$@"
}

gc_validate_compose_images() {
  local images image
  gc_compose config -q
  images="$(gc_compose config --images)"
  [ -n "$images" ] || gc_die "compose rendered no images"
  while IFS= read -r image; do
    [[ "$image" =~ @sha256:[0-9a-f]{64}$ ]] \
      || gc_die "compose rendered a mutable or invalid image reference: $image"
  done <<< "$images"
}

gc_pull_exact() {
  local image="$1" digest image_id
  digest="${image##*@}"
  docker pull "$image" >/dev/null
  image_id="$(docker image inspect "$image" --format '{{.Id}}')"
  [ -n "$image_id" ] || gc_die "could not inspect pulled image $image"
  docker image inspect "$image" --format '{{json .RepoDigests}}' \
    | jq -e --arg suffix "@$digest" 'any(.[]; endswith($suffix))' >/dev/null \
    || gc_die "pulled image metadata does not contain requested digest $digest"
}

gc_wait_service() {
  local service="$1" timeout="${2:-240}" deadline cid state
  deadline=$(( $(date +%s) + timeout ))
  while :; do
    cid="$(gc_compose ps -q "$service" 2>/dev/null || true)"
    if [ -n "$cid" ]; then
      state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || true)"
      case "$state" in
        healthy|running) return 0 ;;
        exited|dead) gc_die "$service entered $state" ;;
      esac
    fi
    [ "$(date +%s)" -lt "$deadline" ] || gc_die "$service did not become healthy within ${timeout}s"
    sleep 3
  done
}

gc_probe_release() {
  local expected_commit="$1" base ready version reported modified
  base="https://$STAGING_TLS_HOSTNAME"
  curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 30 "$base/healthz" >/dev/null
  ready="$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 30 "$base/readyz")"
  # Canary / go-closure rehearsal must never mint authority under live payment mode.
  jq -e '
    .status == "ready" and
    .stripe_api_version == "2025-06-30.basil" and
    (.payment_mode == "sealed" or .payment_mode == "test") and
    .live_value_movement == false
  ' <<< "$ready" >/dev/null \
    || gc_die "public /readyz does not bind the reviewed Stripe API contract (sealed|test, live_value_movement=false required)"
  version="$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 30 "$base/version")"
  reported="$(jq -er '.commit' <<< "$version")"
  modified="$(jq -er '.modified' <<< "$version")"
  [ "$reported" = "$expected_commit" ] \
    || gc_die "public /version commit $reported does not match $expected_commit"
  [ "$modified" = false ] || gc_die "public /version reports a modified build"
  printf '%s\n' "$version"
}

gc_tls_receipt() {
  gc_require_command openssl
  local pem
  pem="$(openssl s_client -connect "$STAGING_TLS_HOSTNAME:443" \
    -servername "$STAGING_TLS_HOSTNAME" -verify_return_error </dev/null 2>/dev/null)"
  [ -n "$pem" ] || gc_die "TLS handshake produced no certificate"
  openssl x509 -noout -checkhost "$STAGING_TLS_HOSTNAME" <<< "$pem" >/dev/null
  openssl x509 -noout -fingerprint -sha256 -enddate -issuer -subject <<< "$pem"
}

gc_prepare_evidence() {
  local kind="$1" timestamp
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  GC_EVIDENCE_DIR="$GC_ROOT/evidence/go-closure"
  mkdir -p "$GC_EVIDENCE_DIR"
  chmod 700 "$GC_EVIDENCE_DIR"
  GC_EVIDENCE_FILE="$GC_EVIDENCE_DIR/${timestamp}-${kind}.json"
  export GC_EVIDENCE_DIR GC_EVIDENCE_FILE
}

gc_atomic_json() {
  local destination="$1"; shift
  local temporary="${destination}.tmp.$$"
  umask 077
  jq "$@" > "$temporary"
  chmod 600 "$temporary"
  # Every go-closure receipt under evidence/go-closure/ must carry binding_status.
  # Route through the bound writer rather than mv-ing unstamped JSON into the tree.
  # shellcheck source=scripts/lib/write-bound-evidence.sh
  ROOT="${ROOT:-$GC_ROOT}"
  # shellcheck disable=SC1091
  . "$GC_ROOT/scripts/lib/write-bound-evidence.sh"
  merc_emit_bound_json "$destination" "scripts/lib/go-closure-common.sh:gc_atomic_json" \
    "$temporary" \
    --exact-config "go-closure receipt $(basename "$destination")" \
    --raw-samples "embedded in receipt body" \
    --model-na "go-closure receipt does not load model weights" \
    --image-na "image digests recorded in receipt body when applicable" \
    --corpus-na "no external corpus"
  rm -f -- "$temporary"
  chmod 600 "$destination" 2>/dev/null || true
}

gc_sha256() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

# Bind the content and mode of every releasable dirty file, not merely the set
# of paths reported by `git status`. Generated receipts are excluded because a
# receipt cannot include its own content without becoming self-referential.
gc_source_state_sha256() {
  local root="${1:-$GC_ROOT}" path digest mode
  {
    git -C "$root" diff --binary --no-ext-diff HEAD -- . \
      ':(exclude)evidence/autonomous/**'
    while IFS= read -r -d '' path; do
      case "$path" in evidence/autonomous/*) continue ;; esac
      mode=100644
      [ ! -x "$root/$path" ] || mode=100755
      if [ -L "$root/$path" ]; then
        mode=120000
        if command -v shasum >/dev/null 2>&1; then
          digest="$(readlink "$root/$path" | shasum -a 256 | awk '{print $1}')"
        else
          digest="$(readlink "$root/$path" | sha256sum | awk '{print $1}')"
        fi
      else
        digest="$(gc_sha256 "$root/$path")"
      fi
      printf 'untracked\0%s\0%s\0%s\0' "$mode" "$path" "$digest"
    done < <(git -C "$root" ls-files --others --exclude-standard -z)
  } | if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{print $1}'
  else
    sha256sum | awk '{print $1}'
  fi
}

gc_database_snapshot() {
  gc_compose exec -T postgres psql -X -qAt -U cx -d cx -c \
    "SELECT json_build_object(
       'buyers',(SELECT count(*) FROM buyers),
       'suppliers',(SELECT count(*) FROM suppliers),
       'workers',(SELECT count(*) FROM workers),
       'jobs',(SELECT count(*) FROM jobs),
       'tasks',(SELECT count(*) FROM tasks),
       'ledger_entries',(SELECT count(*) FROM ledger_entries),
       'ledger_sum_usd',(SELECT COALESCE(sum(amount_usd),0)::text FROM ledger_entries),
       'terminal_jobs_with_open_tasks',(
         SELECT count(*) FROM jobs j
          WHERE j.status IN ('complete','failed','cancelled')
            AND EXISTS (SELECT 1 FROM tasks t WHERE t.job_id=j.id AND t.status NOT IN ('complete','failed','cancelled'))
       )
     )::text;"
}

gc_prometheus_scalar() {
  local query="$1"
  curl --fail --silent --show-error --max-time 15 --get \
    --data-urlencode "query=$query" http://127.0.0.1:9090/api/v1/query \
    | jq -er 'if .status == "success" and (.data.result | length) == 1 then .data.result[0].value[1] else error("non-scalar query") end'
}

gc_validate_safe_arg() {
  [[ "$1" =~ ^[A-Za-z0-9._:/=-]+$ ]] || gc_die "unsafe target argument"
}

gc_run_on_target() {
  local target_mode="$1" script="$2"; shift 2
  gc_validate_absolute_path STAGING_DEPLOYMENT_ROOT
  [[ "$script" =~ ^scripts/go-closure-[a-z0-9-]+\.sh$ ]] || gc_die "unsafe target script"
  local arg
  for arg in "$@"; do gc_validate_safe_arg "$arg"; done
  if [ "$target_mode" = local ]; then
    (cd "$STAGING_DEPLOYMENT_ROOT" && exec bash "$script" --host "$@")
    return
  fi
  [ "$target_mode" = ssh ] || gc_die "target must be local or ssh"
  gc_validate_ssh_target
  local remote="cd -- '$STAGING_DEPLOYMENT_ROOT' && exec bash '$script' --host"
  for arg in "$@"; do remote="$remote '$arg'"; done
  ssh -- "$STAGING_SSH_TARGET" "$remote"
}

gc_sync_bundle() {
  local target_mode="$1" destination="$STAGING_DEPLOYMENT_ROOT"
  gc_require_command rsync
  gc_validate_absolute_path STAGING_DEPLOYMENT_ROOT
  local files=(
    Caddyfile
    monitoring
    ops/go-closure-inputs.json
    ops/staging/compose.go-closure.yml
    ops/staging/env.go-closure.example
    ops/staging/README.md
    scripts/backup.sh
    scripts/validate-backup-verification-receipt.py
    scripts/restore.sh
    scripts/lib/go-closure-common.sh
    scripts/go-closure-deploy.sh
    scripts/go-closure-release-identity.sh
    scripts/go-closure-evidence-proof.sh
    scripts/go-closure-destroy.sh
    scripts/go-closure-rollback-rehearsal.sh
    scripts/go-closure-restart-storm.sh
    scripts/validate-agent-restart-receipt.py
    scripts/go-closure-canary-rehearsal.sh
    scripts/validate-canary-scenario-receipt.py
    scripts/go-closure-soak.sh
    scripts/validate-go-closure-soak-receipt.py
    scripts/validate-go-closure-evidence-chain.py
    scripts/validate-go-closure-scaffold.sh
  )
  if [ "$target_mode" = local ]; then
    mkdir -p "$destination"
    if [ "$GC_ROOT" = "$(cd "$destination" && pwd -P)" ]; then
      return
    fi
    (cd "$GC_ROOT" && rsync -a --relative -- "${files[@]}" "$destination/")
    return
  fi
  [ "$target_mode" = ssh ] || gc_die "target must be local or ssh"
  gc_validate_ssh_target
  ssh -- "$STAGING_SSH_TARGET" "mkdir -p -- '$destination'"
  (cd "$GC_ROOT" && rsync -az --relative -- "${files[@]}" "$STAGING_SSH_TARGET:$destination/")
}
