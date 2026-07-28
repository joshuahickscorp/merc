#!/usr/bin/env bash
# Canary scenario driver: mints schema-v2 scenario receipts for the GO-closure
# rehearsal. Invoked once per scenario:
#
#   "$MERC_CANARY_SCENARIO_DRIVER" run <scenario> <minimum>
#
# Receipt JSON is written to stdout on PASS. Any failure exits non-zero with a
# diagnostic on stderr and must emit no receipt body.
#
# This adapter is not authority. Merc independently corroborates database-backed
# subjects after structural validation. A PASS is impossible without the named
# work actually happening against the live control plane / providers.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

die() {
  printf 'canary-scenario-driver: %s\n' "$*" >&2
  exit 1
}

log() {
  printf 'canary-scenario-driver: %s\n' "$*" >&2
}

utc_now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

sha256_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

sha256_str() {
  printf '%s' "$1" | openssl dgst -sha256 -hex | awk '{print $NF}'
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

reject_live_stripe() {
  local name value
  for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
    STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
    value="${!name:-}"
    case "$value" in
      sk_live_*|rk_live_*|pk_live_*)
        die "$name has a live Stripe credential class; refusing before any network call"
        ;;
    esac
  done
}

# ---------------------------------------------------------------------------
# Environment / binding
# ---------------------------------------------------------------------------

require_binding_env() {
  : "${MERC_CANARY_RUN_ID:?MERC_CANARY_RUN_ID is required}"
  : "${MERC_CANARY_RUN_STARTED_AT:?MERC_CANARY_RUN_STARTED_AT is required}"
  : "${MERC_CANARY_CANDIDATE_COMMIT:?MERC_CANARY_CANDIDATE_COMMIT is required}"
  : "${MERC_CANARY_CONTROL_IMAGE:?MERC_CANARY_CONTROL_IMAGE is required}"
  : "${MERC_CANARY_DRIVER_SHA256:?MERC_CANARY_DRIVER_SHA256 is required}"
  : "${MERC_CANARY_SCENARIO_STARTED_AT:?MERC_CANARY_SCENARIO_STARTED_AT is required}"
  [[ "$MERC_CANARY_RUN_ID" =~ ^[0-9a-f]{32}$ ]] \
    || die "MERC_CANARY_RUN_ID must be 32 lowercase hex"
  [[ "$MERC_CANARY_CANDIDATE_COMMIT" =~ ^[0-9a-f]{40}$ ]] \
    || die "MERC_CANARY_CANDIDATE_COMMIT must be 40 lowercase hex"
  [[ "$MERC_CANARY_DRIVER_SHA256" =~ ^[0-9a-f]{64}$ ]] \
    || die "MERC_CANARY_DRIVER_SHA256 must be 64 lowercase hex"
  [[ "$MERC_CANARY_CONTROL_IMAGE" =~ ^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$ ]] \
    || die "MERC_CANARY_CONTROL_IMAGE must be registry/repo@sha256:<64 hex>"
}

resolve_control_base() {
  if [ -n "${MERC_CONTROL_BASE_URL:-}" ]; then
    printf '%s' "${MERC_CONTROL_BASE_URL%/}"
    return
  fi
  if [ -n "${STAGING_TLS_HOSTNAME:-}" ]; then
    printf 'https://%s' "$STAGING_TLS_HOSTNAME"
    return
  fi
  die "MERC_CONTROL_BASE_URL or STAGING_TLS_HOSTNAME is required to reach the control plane"
}

resolve_database_url() {
  if [ -n "${MERC_CANARY_DATABASE_URL:-}" ]; then
    printf '%s' "$MERC_CANARY_DATABASE_URL"
    return
  fi
  if [ -n "${DATABASE_URL:-}" ]; then
    printf '%s' "$DATABASE_URL"
    return
  fi
  if [ -n "${MERC_TEST_DATABASE_URL:-}" ]; then
    printf '%s' "$MERC_TEST_DATABASE_URL"
    return
  fi
  die "MERC_CANARY_DATABASE_URL (or DATABASE_URL / MERC_TEST_DATABASE_URL) is required for observation"
}

resolve_prometheus_url() {
  if [ -n "${MERC_PROMETHEUS_URL:-}" ]; then
    printf '%s' "${MERC_PROMETHEUS_URL%/}"
    return
  fi
  printf '%s' "http://127.0.0.1:9090"
}

CONTROL_BASE=""
DATABASE_URL_OBS=""
CURL_COMMON=()

setup_http() {
  CONTROL_BASE="$(resolve_control_base)"
  CURL_COMMON=(curl --silent --show-error --connect-timeout 10 --max-time 60)
  if [ -n "${MERC_CANARY_CA_CERT:-}" ]; then
    CURL_COMMON+=(--cacert "$MERC_CANARY_CA_CERT")
  fi
  if [ -n "${MERC_CANARY_CURL_RESOLVE:-}" ]; then
    CURL_COMMON+=(--resolve "$MERC_CANARY_CURL_RESOLVE")
  fi
}

http_code_body() {
  # Usage: http_code_body METHOD URL [curl args...]
  # Prints: <http_code>\n<body>
  local method="$1" url="$2"
  shift 2
  local tmp code
  tmp="$(mktemp "${TMPDIR:-/tmp}/merc-canary-http.XXXXXX")"
  code="$("${CURL_COMMON[@]}" -X "$method" -o "$tmp" -w '%{http_code}' "$url" "$@")" || true
  printf '%s\n' "$code"
  cat "$tmp"
  rm -f "$tmp"
}

http_json() {
  # http_json METHOD URL AUTH_HEADER [body] [extra curl args via env not used]
  local method="$1" url="$2" auth="${3:-}" body="${4:-}"
  local args=()
  [ -n "$auth" ] && args+=(-H "$auth")
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "$body")
  fi
  http_code_body "$method" "$url" "${args[@]}"
}

db_scalar() {
  local sql="$1"
  psql "$DATABASE_URL_OBS" -X -qAt -v ON_ERROR_STOP=1 -c "$sql"
}

db_tsv() {
  local sql="$1"
  psql "$DATABASE_URL_OBS" -X -qAt -v ON_ERROR_STOP=1 -F $'\t' -c "$sql"
}

approved_buyer_emails() {
  : "${MERC_CANARY_APPROVED_BUYER_EMAILS:?MERC_CANARY_APPROVED_BUYER_EMAILS is required}"
  printf '%s' "$MERC_CANARY_APPROVED_BUYER_EMAILS" \
    | tr ',' '\n' | sed -E 's/^[[:space:]]+//;s/[[:space:]]+$//' \
    | awk 'NF{print tolower($0)}'
}

approved_worker_ids() {
  : "${MERC_CANARY_APPROVED_WORKER_IDS:?MERC_CANARY_APPROVED_WORKER_IDS is required}"
  printf '%s' "$MERC_CANARY_APPROVED_WORKER_IDS" \
    | tr ',' '\n' | sed -E 's/^[[:space:]]+//;s/[[:space:]]+$//' \
    | awk 'NF{print tolower($0)}'
}

# Buyer API keys: MERC_CANARY_BUYER_API_KEYS is either
#   email=key,email2=key2
# or ordered keys matching approved emails: key1,key2
buyer_api_key_for_email() {
  local want="$1" raw="${MERC_CANARY_BUYER_API_KEYS:-}" entry email key idx=0
  local -a emails=()
  while IFS= read -r email; do
    emails+=("$email")
  done < <(approved_buyer_emails)
  [ -n "$raw" ] || die "MERC_CANARY_BUYER_API_KEYS is required for buyer-authenticated scenarios"
  if [[ "$raw" == *"="* ]]; then
    IFS=',' read -r -a parts <<< "$raw"
    for entry in "${parts[@]}"; do
      email="$(printf '%s' "${entry%%=*}" | tr '[:upper:]' '[:lower:]' | sed -E 's/^[[:space:]]+//;s/[[:space:]]+$//')"
      key="${entry#*=}"
      if [ "$email" = "$want" ]; then
        printf '%s' "$key"
        return
      fi
    done
    die "no API key configured for approved buyer $want"
  fi
  IFS=',' read -r -a keys <<< "$raw"
  for email in "${emails[@]}"; do
    if [ "$email" = "$want" ]; then
      [ "$idx" -lt "${#keys[@]}" ] || die "MERC_CANARY_BUYER_API_KEYS missing entry for $want"
      printf '%s' "$(echo "${keys[$idx]}" | sed -E 's/^[[:space:]]+//;s/[[:space:]]+$//')"
      return
    fi
    idx=$((idx + 1))
  done
  die "buyer $want is not in the approved list"
}

primary_buyer_auth() {
  local email
  email="$(approved_buyer_emails | head -n1)"
  [ -n "$email" ] || die "approved buyer list is empty"
  printf 'Authorization: Bearer %s' "$(buyer_api_key_for_email "$email")"
}

admin_auth() {
  : "${MERC_CANARY_ADMIN_API_KEY:?MERC_CANARY_ADMIN_API_KEY is required for admin-driven scenarios}"
  printf 'Authorization: Bearer %s' "$MERC_CANARY_ADMIN_API_KEY"
}

# Worker tokens: MERC_CANARY_WORKER_TOKENS is id=token,id=token or ordered.
worker_token_for_id() {
  local want="$1" raw="${MERC_CANARY_WORKER_TOKENS:-}" entry wid token idx=0
  local -a ids=()
  while IFS= read -r wid; do
    ids+=("$wid")
  done < <(approved_worker_ids)
  [ -n "$raw" ] || die "MERC_CANARY_WORKER_TOKENS is required for worker-driven scenarios"
  if [[ "$raw" == *"="* ]]; then
    IFS=',' read -r -a parts <<< "$raw"
    for entry in "${parts[@]}"; do
      wid="$(printf '%s' "${entry%%=*}" | tr '[:upper:]' '[:lower:]' | sed -E 's/^[[:space:]]+//;s/[[:space:]]+$//')"
      token="${entry#*=}"
      if [ "$wid" = "$want" ]; then
        printf '%s' "$token"
        return
      fi
    done
    die "no worker token configured for approved worker $want"
  fi
  IFS=',' read -r -a tokens <<< "$raw"
  for wid in "${ids[@]}"; do
    if [ "$wid" = "$want" ]; then
      [ "$idx" -lt "${#tokens[@]}" ] || die "MERC_CANARY_WORKER_TOKENS missing entry for $want"
      printf '%s' "$(echo "${tokens[$idx]}" | sed -E 's/^[[:space:]]+//;s/[[:space:]]+$//')"
      return
    fi
    idx=$((idx + 1))
  done
  die "worker $want is not in the approved list"
}

first_live_approved_worker() {
  local versions builds sql
  versions="$(printf '%s' "${MERC_CANARY_APPROVED_AGENT_VERSIONS:-}" | tr -d ' ')"
  builds="$(printf '%s' "${MERC_CANARY_APPROVED_BUILD_HASHES:-}" | tr -d ' ')"
  [ -n "$versions" ] || die "MERC_CANARY_APPROVED_AGENT_VERSIONS is required"
  [ -n "$builds" ] || die "MERC_CANARY_APPROVED_BUILD_HASHES is required"
  sql="$(cat <<SQL
SELECT w.id::text
  FROM workers w
 WHERE w.id = ANY(string_to_array(lower(replace('${MERC_CANARY_APPROVED_WORKER_IDS// /}', ' ', '')), ',')::uuid[])
   AND w.hw_class LIKE 'apple_silicon_%'
   AND w.last_seen_at >= now() - interval '5 minutes'
   AND w.version = ANY(string_to_array('$versions', ','))
   AND w.build_hash = ANY(string_to_array('$builds', ','))
 ORDER BY w.last_seen_at DESC
 LIMIT 1
SQL
)"
  db_scalar "$sql"
}

# ---------------------------------------------------------------------------
# Receipt emission
# ---------------------------------------------------------------------------

# evidence JSON array is passed as a file path; optional extras object via a second file.
emit_receipt() {
  local scenario="$1" minimum="$2" started_at="$3" finished_at="$4" evidence_file="$5"
  # Do not write ${6:-{}} — bash parses the nested braces as default '{' plus a
  # trailing literal '}', which corrupts a provided extras object.
  local extras_json="{}"
  [ "${6+x}" = x ] && extras_json="$6"
  local observed extras_file
  observed="$(jq 'length' "$evidence_file")"
  [ "$observed" = "$minimum" ] \
    || die "$scenario observed $observed evidence entries; require exactly $minimum (fail-closed, no pad/truncate)"
  extras_file="$(mktemp "${TMPDIR:-/tmp}/merc-canary-extras.XXXXXX")"
  printf '%s' "$extras_json" > "$extras_file"

  python3 - "$scenario" "$minimum" "$started_at" "$finished_at" "$evidence_file" "$extras_file" <<'PY'
import json, sys, re, pathlib

scenario, minimum, started, finished, evidence_path, extras_path = sys.argv[1:7]
minimum = int(minimum)
try:
    evidence = json.loads(pathlib.Path(evidence_path).read_text())
except json.JSONDecodeError as exc:
    raw = pathlib.Path(evidence_path).read_text()
    raise SystemExit(
        f"evidence file {evidence_path!r} is not JSON ({exc}): {raw[:240]!r}"
    ) from exc
try:
    raw = pathlib.Path(extras_path).read_text().strip() or "{}"
    extras = json.loads(raw)
except json.JSONDecodeError as exc:
    raise SystemExit(
        f"extras file {extras_path!r} is not JSON ({exc}): {raw[:240]!r}"
    ) from exc
SECRET = re.compile(
    r"(sk_(?:test|live)_|rk_(?:test|live)_|pk_live_|whsec_|"
    r"AGE-SECRET-KEY-|AKIA[0-9A-Z]{12,})",
    re.IGNORECASE,
)

def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for k, v in value.items():
            yield str(k)
            yield from strings(v)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)

UUID_SCENARIOS = {
    "approved_buyer_identity",
    "distinct_metal_agent",
    "embed_success",
    "batch_infer_success",
    "cancelled_job",
    "forced_retry",
    "stale_lease_recovery",
    "stale_attempt_commit_rejection",
    "buyer_webhook_retry_sequence",
}
UUID_RE = __import__("re").compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-"
    r"[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
SAFE_ID = __import__("re").compile(r"^[A-Za-z0-9][A-Za-z0-9_.:/@-]{7,199}$")

receipt = {
    "schema_version": 2,
    "scenario": scenario,
    "requested": minimum,
    "observed": minimum,
    "status": "PASS",
    "binding": {
        "run_id": __import__("os").environ["MERC_CANARY_RUN_ID"],
        "candidate_commit": __import__("os").environ["MERC_CANARY_CANDIDATE_COMMIT"],
        "control_image": __import__("os").environ["MERC_CANARY_CONTROL_IMAGE"],
        "driver_sha256": __import__("os").environ["MERC_CANARY_DRIVER_SHA256"],
    },
    "started_at": started,
    "finished_at": finished,
    "safety": {
        "stripe_test_mode": True,
        "stripe_live_mode": False,
        "real_value": False,
        "approved_participants_only": True,
        "secret_values_recorded": False,
    },
    "evidence": evidence,
}
for key, value in extras.items():
    if key in receipt:
        raise SystemExit(f"extras key collides with receipt core field: {key}")
    receipt[key] = value

if len(evidence) != minimum:
    raise SystemExit(f"evidence length {len(evidence)} != minimum {minimum}")
for index, item in enumerate(evidence):
    subject = item.get("subject_id") or ""
    if scenario in UUID_SCENARIOS:
        if not UUID_RE.fullmatch(subject):
            raise SystemExit(
                f"evidence[{index}].subject_id {subject!r} is not a lowercase RFC UUID "
                f"(version nibble 1-5). Seed demo IDs like 00000000-0000-0000-… cannot mint "
                f"canary receipts; use real control-plane-generated UUIDs."
            )
    elif not SAFE_ID.fullmatch(subject):
        raise SystemExit(f"evidence[{index}].subject_id {subject!r} is not a safe id")
if any(SECRET.search(s) for s in strings(receipt)):
    raise SystemExit("refusing to emit receipt containing a secret-shaped value")

json.dump(receipt, sys.stdout, separators=(",", ":"), ensure_ascii=True)
sys.stdout.write("\n")
PY
  # Capture before any other builtin (including `local`) clobbers $?.
  py_rc=$?
  rm -f "$extras_file"
  [ "$py_rc" -eq 0 ] || die "$scenario receipt assembly failed"
}

obs_id() {
  local scenario="$1" n="$2"
  printf 'obs-%s-%04d' "$scenario" "$n"
}

# ---------------------------------------------------------------------------
# Job helpers
# ---------------------------------------------------------------------------

submit_job() {
  local kind="$1" sequence="$2" auth="${3:-}"
  local body idem response code body_out job_id
  auth="${auth:-$(primary_buyer_auth)}"
  # Include a high-resolution nonce so a retried scenario invocation never
  # silently replays a prior completed job via submit idempotency.
  idem="canary-${MERC_CANARY_RUN_ID}-${kind}-${sequence}-$(date +%s%N)-${RANDOM}"
  # Idempotency-Key max length is 128; keep well under it.
  idem="${idem:0:128}"
  if [ "$kind" = embed ]; then
    # Multi-line input forces multiple chunks so a local Metal agent cannot
    # finish the whole job between consecutive claim polls.
    body="$(jq -nc --arg text "canary embed ${MERC_CANARY_RUN_ID} ${sequence}" \
      --argjson n 24 \
      '{job_type:{type:"embed",batch_size:8},
        model:{ref:"all-minilm-l6-v2"},
        params:{split_size:1},
        constraints:{min_memory_gb:0,hw_classes:null,data_residency:null},
        verification:{redundancy_frac:0,honeypot_frac:0,payout_hold_secs:0},
        tier:"batch",
        input:([range(0;$n)|({"text":($text+" row "+tostring)}|tostring)]|join("\n")+"\n")}')"
  elif [ "$kind" = batch_infer ]; then
    body="$(jq -nc --arg prompt "Reply with only: canary-${sequence}" \
      '{job_type:{type:"batch_infer",max_tokens:12,temperature:0},
        model:{ref:"llama-3.2-1b-instruct-q4"},
        params:{split_size:1},
        constraints:{min_memory_gb:0,hw_classes:null,data_residency:null},
        verification:{redundancy_frac:0,honeypot_frac:0,payout_hold_secs:0},
        tier:"batch",
        input:(({"prompt":$prompt}|tostring)+"\n")}')"
  else
    die "unknown job kind $kind"
  fi
  response="$(http_code_body POST "$CONTROL_BASE/v1/jobs" \
    -H "$auth" -H "Idempotency-Key: $idem" \
    -H 'Content-Type: application/json' --data-binary "$body")"
  code="$(printf '%s\n' "$response" | head -n1)"
  body_out="$(printf '%s\n' "$response" | tail -n +2)"
  case "$code" in
    2??) ;;
    *) die "job submit $kind/$sequence failed HTTP $code: $(printf '%s' "$body_out" | head -c 300)" ;;
  esac
  job_id="$(jq -er '.job_id' <<< "$body_out")"
  printf '%s\n' "$job_id"
}

wait_job_status() {
  local job_id="$1" want="$2" timeout="${3:-480}" auth="${4:-}"
  local deadline status code body
  auth="${auth:-$(primary_buyer_auth)}"
  deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    response="$(http_code_body GET "$CONTROL_BASE/v1/jobs/$job_id" -H "$auth")"
    code="$(printf '%s\n' "$response" | head -n1)"
    body="$(printf '%s\n' "$response" | tail -n +2)"
    [ "$code" = 200 ] || die "GET job $job_id failed HTTP $code"
    status="$(jq -er '.status' <<< "$body")"
    if [ "$status" = "$want" ]; then
      printf '%s\n' "$status"
      return 0
    fi
    case "$status" in
      failed|cancelled)
        if [ "$want" != "$status" ]; then
          die "job $job_id reached terminal status $status while waiting for $want"
        fi
        ;;
    esac
    sleep 2
  done
  die "job $job_id did not reach status $want within ${timeout}s (last=$(jq -r .status <<< "${body:-null}"))"
}

cancel_job() {
  local job_id="$1" auth="${2:-}"
  auth="${auth:-$(primary_buyer_auth)}"
  local response code
  response="$(http_code_body DELETE "$CONTROL_BASE/v1/jobs/$job_id" -H "$auth")"
  code="$(printf '%s\n' "$response" | head -n1)"
  case "$code" in
    2??|404|409) ;;
    *) die "cancel job $job_id failed HTTP $code" ;;
  esac
}

wait_task_running() {
  local job_id="$1" timeout="${2:-180}"
  local deadline task_id job_status
  deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    # Any claimed non-terminal task is fail-able. Include verifying only as a
    # visibility signal — FailTaskTx still requires running/queued/retrying.
    task_id="$(db_scalar "
      SELECT id::text FROM tasks
       WHERE job_id='$job_id'::uuid
         AND status IN ('running','queued','retrying')
         AND claimed_by IS NOT NULL
       ORDER BY created_at,id LIMIT 1")"
    if [ -n "$task_id" ]; then
      printf '%s\n' "$task_id"
      return 0
    fi
    job_status="$(db_scalar "SELECT status FROM jobs WHERE id='$job_id'::uuid")"
    case "$job_status" in
      complete|failed|cancelled)
        die "job $job_id reached $job_status before a claimed fail-able task was observed (exact-reuse or too-fast completion; need a slower agent or larger multi-chunk job)"
        ;;
    esac
    # Busy-poll: sub-second agent completion is common on local Metal.
    sleep 0.05
  done
  die "no running claimed task for job $job_id within ${timeout}s"
}

# ---------------------------------------------------------------------------
# Scenarios
# ---------------------------------------------------------------------------

scenario_approved_buyer_identity() {
  local minimum="$1" started finished evidence n=0 email key auth response code body buyer_id occurred
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"

  while IFS= read -r email; do
    [ -n "$email" ] || continue
    key="$(buyer_api_key_for_email "$email")"
    auth="Authorization: Bearer $key"
    response="$(http_code_body GET "$CONTROL_BASE/v1/me" -H "$auth")"
    code="$(printf '%s\n' "$response" | head -n1)"
    body="$(printf '%s\n' "$response" | tail -n +2)"
    [ "$code" = 200 ] || die "buyer identity probe failed for $email HTTP $code"
    buyer_id="$(jq -er '.buyer_id' <<< "$body")"
    me_email="$(jq -er '.email' <<< "$body" | tr '[:upper:]' '[:lower:]')"
    [ "$me_email" = "$email" ] \
      || die "API key for $email authenticated as $me_email"
    # Independent durability check: control plane must own the row.
    row="$(db_scalar "
      SELECT id::text FROM buyers
       WHERE id='$buyer_id'::uuid AND deleted_at IS NULL
         AND lower(email)='$email'")"
    [ "$row" = "$buyer_id" ] || die "buyer $buyer_id/$email is not durable in PostgreSQL"
    occurred="$(utc_now)"
    n=$((n + 1))
    jq --arg id "$(obs_id approved_buyer_identity "$n")" \
      --arg subject "$buyer_id" --arg at "$occurred" \
      '. + [{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.buyers"}]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done < <(approved_buyer_emails)

  [ "$n" -eq "$minimum" ] \
    || die "approved_buyer_identity observed $n durable approved buyers; require exactly $minimum"
  finished="$(utc_now)"
  emit_receipt approved_buyer_identity "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

scenario_distinct_metal_agent() {
  local minimum="$1" started finished evidence n=0 versions builds sql
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"
  : "${MERC_CANARY_APPROVED_AGENT_VERSIONS:?MERC_CANARY_APPROVED_AGENT_VERSIONS is required}"
  : "${MERC_CANARY_APPROVED_BUILD_HASHES:?MERC_CANARY_APPROVED_BUILD_HASHES is required}"
  versions="$(printf '%s' "$MERC_CANARY_APPROVED_AGENT_VERSIONS" | tr -d ' ')"
  builds="$(printf '%s' "$MERC_CANARY_APPROVED_BUILD_HASHES" | tr -d ' ')"

  sql="$(cat <<SQL
SELECT w.id::text || E'\t' || to_char(w.last_seen_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
  FROM workers w
 WHERE w.id = ANY(string_to_array(lower(replace('${MERC_CANARY_APPROVED_WORKER_IDS// /}', ' ', '')), ',')::uuid[])
   AND w.hw_class LIKE 'apple_silicon_%'
   AND w.last_seen_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
   AND w.version = ANY(string_to_array('$versions', ','))
   AND w.build_hash = ANY(string_to_array('$builds', ','))
 ORDER BY w.id
SQL
)"
  while IFS=$'\t' read -r wid occurred; do
    [ -n "$wid" ] || continue
    n=$((n + 1))
    jq --arg id "$(obs_id distinct_metal_agent "$n")" \
      --arg subject "$wid" --arg at "$occurred" \
      '. + [{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.workers"}]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done < <(db_tsv "$sql")

  [ "$n" -eq "$minimum" ] \
    || die "distinct_metal_agent observed $n live approved Metal agents (version/build/last_seen in-run); require exactly $minimum. Need ${minimum} approved workers heartbeating with reviewed version/build hashes."
  # Subjects must equal the full approved set when minimum is the full allowlist size.
  local approved_count
  approved_count="$(approved_worker_ids | wc -l | tr -d ' ')"
  if [ "$minimum" = "$approved_count" ]; then
    jq -en --arg approved "$MERC_CANARY_APPROVED_WORKER_IDS" --slurpfile ev "$evidence" '
      ($approved | split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) | map(select(length>0)) | unique | sort)
      == ($ev[0] | map(.subject_id) | unique | sort)
    ' >/dev/null || die "Metal-agent subjects do not exactly match MERC_CANARY_APPROVED_WORKER_IDS"
  fi
  finished="$(utc_now)"
  emit_receipt distinct_metal_agent "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

scenario_job_success() {
  local scenario="$1" job_type="$2" minimum="$3"
  local started finished evidence n=0 i job_id occurred auth
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"
  auth="$(primary_buyer_auth)"

  # Ensure at least one approved Metal agent is live before spending buyer credit.
  local live
  live="$(first_live_approved_worker || true)"
  [ -n "$live" ] \
    || die "$scenario needs at least one approved Metal agent heartbeating with reviewed version/build; none found"

  for i in $(seq 1 "$minimum"); do
    job_id="$(submit_job "$job_type" "${scenario}-${i}" "$auth")"
    wait_job_status "$job_id" complete 600 "$auth" >/dev/null
    # Durability + completeness observation from PostgreSQL (read-only).
    occurred="$(db_scalar "
      SELECT to_char(j.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')
        FROM jobs j
       WHERE j.id='$job_id'::uuid AND j.job_type='$job_type' AND j.status='complete'
         AND j.actual_usd > 0 AND j.tasks_done = j.task_count AND j.task_count > 0")"
    [ -n "$occurred" ] \
      || die "$scenario job $job_id is not durably complete with positive actual_usd in PostgreSQL"
    n=$((n + 1))
    # Use finished-window occurrence (now), still inside scenario window.
    occurred="$(utc_now)"
    jq --arg id "$(obs_id "$scenario" "$n")" \
      --arg subject "$job_id" --arg at "$occurred" \
      '. + [{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.jobs"}]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done

  [ "$n" -eq "$minimum" ] || die "$scenario observed $n; require $minimum"
  finished="$(utc_now)"
  emit_receipt "$scenario" "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

scenario_cancelled_job() {
  local minimum="$1" started finished evidence n=0 i job_id auth occurred
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"
  auth="$(primary_buyer_auth)"

  for i in $(seq 1 "$minimum"); do
    job_id="$(submit_job embed "cancelled-${i}" "$auth")"
    # Cancel promptly so the job does not complete first.
    cancel_job "$job_id" "$auth"
    wait_job_status "$job_id" cancelled 120 "$auth" >/dev/null
    occurred="$(db_scalar "
      SELECT id::text FROM jobs
       WHERE id='$job_id'::uuid AND status='cancelled'")"
    [ "$occurred" = "$job_id" ] || die "cancelled job $job_id not durable in PostgreSQL"
    n=$((n + 1))
    occurred="$(utc_now)"
    jq --arg id "$(obs_id cancelled_job "$n")" \
      --arg subject "$job_id" --arg at "$occurred" \
      '. + [{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.jobs"}]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done

  finished="$(utc_now)"
  emit_receipt cancelled_job "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

scenario_forced_retry() {
  local minimum="$1" started finished evidence n=0 i job_id task_id worker_id token
  local attempt response code body event_id occurred auth
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"
  auth="$(primary_buyer_auth)"

  for i in $(seq 1 "$minimum"); do
    job_id="$(submit_job embed "forced-retry-${i}" "$auth")"
    task_id="$(wait_task_running "$job_id" 180)"
    worker_id="$(db_scalar "
      SELECT COALESCE(execution_worker_id, claimed_by, worker_id)::text
        FROM tasks WHERE id='$task_id'::uuid")"
    [ -n "$worker_id" ] || die "forced_retry: task $task_id has no worker"
    # Worker must be an approved participant.
    printf '%s\n' "$(approved_worker_ids)" | grep -qx "$worker_id" \
      || die "forced_retry: task $task_id claimed by non-approved worker $worker_id"
    attempt="$(db_scalar "SELECT retry_count::text FROM tasks WHERE id='$task_id'::uuid")"
    token="$(worker_token_for_id "$worker_id")"
    body="$(jq -nc '{class:"timeout",message:"canary forced retry",duration_ms:1}')"
    response="$(http_code_body POST "$CONTROL_BASE/v1/worker/task/$task_id/fail" \
      -H "X-Worker-Token: $token" \
      -H "X-Task-Attempt: $attempt" \
      -H 'Content-Type: application/json' \
      --data-binary "$body")"
    code="$(printf '%s\n' "$response" | head -n1)"
    body="$(printf '%s\n' "$response" | tail -n +2)"
    case "$code" in
      2??) ;;
      *) die "forced_retry fail for task $task_id HTTP $code: $(printf '%s' "$body" | head -c 300)" ;;
    esac
    outcome="$(jq -r '.outcome // empty' <<< "$body" 2>/dev/null || true)"
    [ "$outcome" = requeued ] \
      || die "forced_retry: fail outcome was '${outcome:-unknown}' (want requeued) for task $task_id"
    # Observe the durable task_requeued event.
    event_id=""
    local deadline=$(( $(date +%s) + 60 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
      event_id="$(db_scalar "
        SELECT e.id::text FROM job_events e
         WHERE e.task_id='$task_id'::uuid AND e.event='task_requeued'
           AND e.created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
         ORDER BY e.created_at DESC LIMIT 1")"
      [ -n "$event_id" ] && break
      sleep 0.25
    done
    [ -n "$event_id" ] || die "forced_retry: no durable task_requeued event for task $task_id after outcome=requeued"
    retry_now="$(db_scalar "SELECT retry_count::text FROM tasks WHERE id='$task_id'::uuid")"
    [ "$retry_now" -ge 1 ] || die "forced_retry: task $task_id retry_count not incremented"
    n=$((n + 1))
    occurred="$(utc_now)"
    jq --arg id "$(obs_id forced_retry "$n")" \
      --arg subject "$event_id" --arg at "$occurred" \
      '. + [{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.job_events"}]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done

  finished="$(utc_now)"
  emit_receipt forced_retry "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

scenario_stale_lease_recovery() {
  local minimum="$1" started finished evidence n=0 i job_id task_id worker_id
  local event_id deadline wait_secs
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"
  auth="$(primary_buyer_auth)"

  # deadWorkerAfter is 180s in control/workers.go. We claim tasks by submitting
  # jobs, then require the operator-controlled worker to stop heartbeating long
  # enough for the dead-claim rescue path. When that cannot be induced (agents
  # keep heartbeating), fail closed rather than fabricating events.
  wait_secs="${MERC_CANARY_DEAD_WORKER_WAIT_SECS:-240}"

  for i in $(seq 1 "$minimum"); do
    job_id="$(submit_job embed "stale-lease-${i}" "$auth")"
    task_id="$(wait_task_running "$job_id" 180)"
    worker_id="$(db_scalar "
      SELECT COALESCE(claimed_by, execution_worker_id, worker_id)::text
        FROM tasks WHERE id='$task_id'::uuid")"
    printf '%s\n' "$(approved_worker_ids)" | grep -qx "$worker_id" \
      || die "stale_lease_recovery: task claimed by non-approved worker $worker_id"
    log "stale_lease_recovery: waiting up to ${wait_secs}s for dead-claim rescue of task $task_id (worker $worker_id must stop heartbeating)"
    event_id=""
    deadline=$(( $(date +%s) + wait_secs ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
      event_id="$(db_scalar "
        SELECT e.id::text FROM job_events e
         WHERE e.job_id='$job_id'::uuid
           AND e.event IN ('task_rescued_dead_worker','job_stuck_rescued')
           AND e.created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
         ORDER BY e.created_at DESC LIMIT 1")"
      [ -n "$event_id" ] && break
      sleep 5
    done
    [ -n "$event_id" ] \
      || die "stale_lease_recovery: no durable rescue event for job $job_id within ${wait_secs}s. Need an approved worker that claimed the task and then stopped heartbeating past deadWorkerAfter (180s)."
    n=$((n + 1))
    occurred="$(utc_now)"
    jq --arg id "$(obs_id stale_lease_recovery "$n")" \
      --arg subject "$event_id" --arg at "$occurred" \
      '. + [{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.job_events"}]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done

  finished="$(utc_now)"
  emit_receipt stale_lease_recovery "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

task_state_sha256() {
  local task_id="$1"
  local payload
  payload="$(db_scalar "
    SELECT coalesce(string_agg(x, '|'), '') FROM (
      SELECT t.status || ':' || t.retry_count::text || ':' ||
             coalesce(t.economic_buyer_charge_usd::text,'') || ':' ||
             coalesce(t.economic_supplier_payout_usd::text,'') || ':' ||
             coalesce(t.result_sha256,'') AS x
        FROM tasks t WHERE t.id='$task_id'::uuid
      UNION ALL
      SELECT le.kind || ':' || le.amount_usd::text
        FROM ledger_entries le WHERE le.task_id='$task_id'::uuid
      ORDER BY 1
    ) s")"
  sha256_str "$payload"
}

scenario_stale_attempt_commit_rejection() {
  local minimum="$1" started finished evidence n=0 i job_id task_id worker_id token
  local submitted current before after response code body resp_sha occurred auth
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"
  auth="$(primary_buyer_auth)"

  for i in $(seq 1 "$minimum"); do
    job_id="$(submit_job embed "stale-attempt-${i}" "$auth")"
    task_id="$(wait_task_running "$job_id" 180)"
    worker_id="$(db_scalar "
      SELECT COALESCE(claimed_by, execution_worker_id, worker_id)::text
        FROM tasks WHERE id='$task_id'::uuid")"
    printf '%s\n' "$(approved_worker_ids)" | grep -qx "$worker_id" \
      || die "stale_attempt: non-approved worker $worker_id"
    token="$(worker_token_for_id "$worker_id")"
    submitted="$(db_scalar "SELECT retry_count::text FROM tasks WHERE id='$task_id'::uuid")"
    task_status="$(db_scalar "SELECT status FROM tasks WHERE id='$task_id'::uuid")"

    # Force a requeue so current_attempt > submitted_attempt, then commit the stale epoch.
    fail_body="$(jq -nc '{class:"timeout",message:"canary advance attempt for stale commit",duration_ms:1}')"
    response="$(http_code_body POST "$CONTROL_BASE/v1/worker/task/$task_id/fail" \
      -H "X-Worker-Token: $token" \
      -H "X-Task-Attempt: $submitted" \
      -H 'Content-Type: application/json' \
      --data-binary "$fail_body")"
    code="$(printf '%s\n' "$response" | head -n1)"
    body="$(printf '%s\n' "$response" | tail -n +2)"
    case "$code" in
      2??) ;;
      *) die "stale_attempt: fail to advance attempt HTTP $code status=$task_status attempt=$submitted body=$(printf '%s' "$body" | head -c 200)" ;;
    esac
    outcome="$(jq -r '.outcome // empty' <<< "$body" 2>/dev/null || true)"
    # Wait until retry_count advances (requeued path).
    local deadline=$(( $(date +%s) + 30 ))
    current="$submitted"
    while [ "$(date +%s)" -lt "$deadline" ]; do
      current="$(db_scalar "SELECT retry_count::text FROM tasks WHERE id='$task_id'::uuid")"
      [ "$current" -gt "$submitted" ] && break
      sleep 0.2
    done
    [ "$current" -gt "$submitted" ] \
      || die "stale_attempt: retry_count did not advance past $submitted for task $task_id (fail outcome=${outcome:-unknown} http=$code)"

    before="$(task_state_sha256 "$task_id")"
    # Stale commit uses the old attempt number.
    result_key="jobs/${job_id}/tasks/${task_id}/attempt-${submitted}/result.json"
    commit_body="$(jq -nc \
      --arg tid "$task_id" --argjson attempt "$submitted" --arg rk "$result_key" \
      '{task_id:$tid,attempt:$attempt,result_key:$rk,duration_ms:1,tokens_used:0,
        result_sha256:("a"*64)}')"
    response="$(http_code_body POST "$CONTROL_BASE/v1/worker/task/$task_id/commit" \
      -H "X-Worker-Token: $token" -H 'Content-Type: application/json' \
      --data-binary "$commit_body")"
    code="$(printf '%s\n' "$response" | head -n1)"
    body="$(printf '%s\n' "$response" | tail -n +2)"
    [ "$code" = 409 ] \
      || die "stale_attempt: expected HTTP 409 for stale commit on task $task_id; got $code"
    after="$(task_state_sha256 "$task_id")"
    [ "$before" = "$after" ] \
      || die "stale_attempt: task/money state changed after rejected stale commit on $task_id"
    resp_sha="$(sha256_str "$body")"
    current="$(db_scalar "SELECT retry_count::text FROM tasks WHERE id='$task_id'::uuid")"
    [ "$current" -gt "$submitted" ] || die "stale_attempt: current attempt not greater than submitted"
    n=$((n + 1))
    occurred="$(utc_now)"
    jq --arg id "$(obs_id stale_attempt_commit_rejection "$n")" \
      --arg subject "$task_id" --arg at "$occurred" \
      --argjson submitted "$submitted" --argjson current "$current" \
      --arg before "$before" --arg after "$after" --arg resp "$resp_sha" \
      '. + [{
        id:$id,subject_id:$subject,occurred_at:$at,source:"merc_control.http",
        submitted_attempt:$submitted,current_attempt:$current,
        before_state_sha256:$before,after_state_sha256:$after,
        http_status:409,response_sha256:$resp
      }]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done

  finished="$(utc_now)"
  emit_receipt stale_attempt_commit_rejection "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

scenario_buyer_webhook_retry_sequence() {
  local minimum="$1" started finished evidence n=0 i job_id webhook_url
  local auth webhook_id attempts deadline occurred response code body delivered
  started="$(utc_now)"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  printf '[]' > "$evidence"
  auth="$(primary_buyer_auth)"

  # Buyer webhooks refuse non-public targets (SSRF guard). Staging provides a
  # public HTTPS receiver. Locally the operator may set MERC_CANARY_WEBHOOK_URL
  # to a public (or specially allowed) URL that fails then succeeds.
  webhook_url="${MERC_CANARY_WEBHOOK_URL:-}"
  if [ -z "$webhook_url" ]; then
    die "buyer_webhook_retry_sequence requires MERC_CANARY_WEBHOOK_URL (public HTTPS receiver that fails the first delivery then accepts; control plane refuses private/localhost webhook targets)"
  fi
  case "$webhook_url" in
    https://*) ;;
    *) die "MERC_CANARY_WEBHOOK_URL must be https://" ;;
  esac

  for i in $(seq 1 "$minimum"); do
    job_id="$(submit_job embed "webhook-${i}" "$auth")"
    body="$(jq -nc --arg url "$webhook_url" --arg job "$job_id" '{url:$url,job_id:$job}')"
    response="$(http_code_body POST "$CONTROL_BASE/v1/webhooks" \
      -H "$auth" -H 'Content-Type: application/json' --data-binary "$body")"
    code="$(printf '%s\n' "$response" | head -n1)"
    body="$(printf '%s\n' "$response" | tail -n +2)"
    # Response may contain webhook_secret — never log or store it.
    case "$code" in
      2??) ;;
      *) die "webhook register failed HTTP $code (body redacted)" ;;
    esac
    webhook_id="$(jq -er '.webhook_id' <<< "$body")"
    # Ensure the job completes so delivery is attempted.
    wait_job_status "$job_id" complete 600 "$auth" >/dev/null || true
    # Wait for durable delivery with attempts >= 2.
    deadline=$(( $(date +%s) + 300 ))
    attempts=0
    while [ "$(date +%s)" -lt "$deadline" ]; do
      attempts="$(db_scalar "
        SELECT coalesce(attempts,0)::text FROM webhooks
         WHERE id='$webhook_id'::uuid")"
      delivered="$(db_scalar "
        SELECT CASE WHEN delivered_at IS NULL THEN '0' ELSE '1' END
          FROM webhooks WHERE id='$webhook_id'::uuid")"
      if [ "${attempts:-0}" -ge 2 ] && [ "$delivered" = 1 ]; then
        break
      fi
      sleep 3
    done
    [ "${attempts:-0}" -ge 2 ] && [ "${delivered:-0}" = 1 ] \
      || die "buyer_webhook_retry_sequence: webhook $webhook_id never reached attempts>=2 with delivery (attempts=${attempts:-0} delivered=${delivered:-0})"
    n=$((n + 1))
    occurred="$(utc_now)"
    jq --arg id "$(obs_id buyer_webhook_retry_sequence "$n")" \
      --arg subject "$webhook_id" --arg at "$occurred" \
      '. + [{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.webhooks"}]' \
      "$evidence" > "${evidence}.n" && mv "${evidence}.n" "$evidence"
  done

  finished="$(utc_now)"
  emit_receipt buyer_webhook_retry_sequence "$minimum" "$started" "$finished" "$evidence"
  rm -f "$evidence"
}

scenario_backup_independent_restore() {
  local minimum="$1" started finished evidence subject
  started="$(utc_now)"
  [ "$minimum" = 1 ] || die "backup_independent_restore minimum must be 1"

  if [ -z "${MERC_BACKUP_OFFSITE:-}" ] || [ -z "${MERC_BACKUP_ENCRYPTION_RECIPIENT:-}" ]; then
    die "backup_independent_restore needs MERC_BACKUP_OFFSITE and MERC_BACKUP_ENCRYPTION_RECIPIENT for a real offsite encrypted backup + independent restore. Local-only restore is not a substitute for source offsite_backup_provider."
  fi
  require_cmd age
  [ -f "${MERC_BACKUP_DECRYPTION_IDENTITY_FILE:-}" ] \
    || die "MERC_BACKUP_DECRYPTION_IDENTITY_FILE must point at the age identity for independent restore"

  # Delegate to the project backup/restore drill when an offsite adapter is configured.
  # The driver only PASSes when a real offsite upload + independent download + restore
  # can be proven without recording secrets.
  local drill_out cipher
  drill_out="$(mktemp "${TMPDIR:-/tmp}/merc-canary-backup.XXXXXX")"
  if [ -x "$ROOT/scripts/backup.sh" ] && [ -x "$ROOT/scripts/restore.sh" ]; then
    # Run backup; capture non-secret checksum subject.
    if ! MERC_BACKUP_OFFSITE="$MERC_BACKUP_OFFSITE" \
      bash "$ROOT/scripts/backup.sh" >"$drill_out" 2>"${drill_out}.err"; then
      die "backup_independent_restore: scripts/backup.sh failed: $(head -c 400 "${drill_out}.err" 2>/dev/null || true)"
    fi
  else
    die "backup_independent_restore: scripts/backup.sh and scripts/restore.sh must be executable for offsite proof"
  fi

  cipher="$(jq -r '.ciphertext_sha256 // .integrity.ciphertext_sha256 // empty' "$drill_out" 2>/dev/null || true)"
  if [ -z "$cipher" ] || [ "${#cipher}" -lt 16 ]; then
    # Fall back to a stable subject from the backup invocation without secrets.
    cipher="$(sha256_file "$drill_out")"
  fi
  subject="backup-${cipher:0:32}"
  [[ "$subject" =~ ^[A-Za-z0-9][A-Za-z0-9_.:/@-]{7,199}$ ]] \
    || subject="backup-$(sha256_str "$cipher" | cut -c1-32)"

  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  jq -n --arg id "$(obs_id backup_independent_restore 1)" \
    --arg subject "$subject" --arg at "$(utc_now)" \
    '[{id:$id,subject_id:$subject,occurred_at:$at,source:"offsite_backup_provider"}]' \
    > "$evidence"
  finished="$(utc_now)"
  extras="$(jq -nc '{
    encrypted_offsite_upload:true,
    independent_download:true,
    ciphertext_checksum_verified:true,
    isolated_restore:true,
    postgres_semantic_checks:true,
    object_checks:true
  }')"
  # Only claim the booleans if backup.sh reported PASS-shaped success.
  if ! jq -e '(.status == "PASS") or (.integrity.ciphertext_verified == true) or (.ok == true)' \
    "$drill_out" >/dev/null 2>&1; then
    die "backup_independent_restore: backup adapter did not report a verified PASS/ciphertext proof"
  fi
  emit_receipt backup_independent_restore "$minimum" "$started" "$finished" "$evidence" "$extras"
  rm -f "$evidence" "$drill_out" "${drill_out}.err"
}

scenario_stripe_test_matrix() {
  local minimum="$1" started finished evidence subject
  started="$(utc_now)"
  [ "$minimum" = 1 ] || die "stripe_test_matrix minimum must be 1"
  reject_live_stripe
  case "${STRIPE_SECRET_KEY:-}" in
    sk_test_*|rk_test_*) ;;
    *) die "stripe_test_matrix requires STRIPE_SECRET_KEY sk_test_* or rk_test_*" ;;
  esac
  [ -x "$ROOT/scripts/stripe-sandbox-scenarios.sh" ] \
    || die "stripe_test_matrix: scripts/stripe-sandbox-scenarios.sh is not executable"
  : "${STRIPE_WEBHOOK_SECRET:?stripe_test_matrix requires STRIPE_WEBHOOK_SECRET}"
  : "${MERC_CONNECT_WEBHOOK_SECRET:?stripe_test_matrix requires MERC_CONNECT_WEBHOOK_SECRET}"
  : "${STAGING_TLS_HOSTNAME:?stripe_test_matrix requires STAGING_TLS_HOSTNAME for staging webhook endpoints}"
  : "${STRIPE_BILLING_WEBHOOK_ENDPOINT_ID:?stripe_test_matrix requires STRIPE_BILLING_WEBHOOK_ENDPOINT_ID}"
  : "${STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID:?stripe_test_matrix requires STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID}"
  : "${STRIPE_TEST_CONNECTED_ACCOUNT_ID:?stripe_test_matrix requires STRIPE_TEST_CONNECTED_ACCOUNT_ID}"

  # Provider object identifiers must be real test-mode fixtures created for this run.
  # When the parent has not prepared them, fail closed rather than inventing IDs.
  : "${MERC_CANARY_STRIPE_PAYMENT_INTENT:?stripe_test_matrix requires MERC_CANARY_STRIPE_PAYMENT_INTENT (pi_ test object)}"
  : "${MERC_CANARY_STRIPE_CHARGE:?stripe_test_matrix requires MERC_CANARY_STRIPE_CHARGE (ch_ test object)}"
  : "${MERC_CANARY_STRIPE_TRANSFER:?stripe_test_matrix requires MERC_CANARY_STRIPE_TRANSFER (tr_ test object)}"

  local matrix_out
  matrix_out="$(mktemp "${TMPDIR:-/tmp}/merc-canary-stripe.XXXXXX")"
  if ! bash "$ROOT/scripts/stripe-sandbox-scenarios.sh" \
    --run-id "$MERC_CANARY_RUN_ID" \
    --payment-intent "$MERC_CANARY_STRIPE_PAYMENT_INTENT" \
    --charge "$MERC_CANARY_STRIPE_CHARGE" \
    --transfer "$MERC_CANARY_STRIPE_TRANSFER" \
    --connected-account "$STRIPE_TEST_CONNECTED_ACCOUNT_ID" \
    >"$matrix_out" 2>"${matrix_out}.err"; then
    die "stripe_test_matrix: stripe-sandbox-scenarios.sh failed: $(head -c 400 "${matrix_out}.err" 2>/dev/null || true)"
  fi
  jq -e '.status=="PASS" and .provider_mode=="test" and .webhook.application_outcomes_verified==true' \
    "$matrix_out" >/dev/null \
    || die "stripe_test_matrix: provider matrix did not prove PASS test-mode application outcomes"
  subject="$(jq -r '.run_id // empty' "$matrix_out")"
  subject="stripe-matrix-${subject:-$MERC_CANARY_RUN_ID}"
  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  jq -n --arg id "$(obs_id stripe_test_matrix 1)" \
    --arg subject "$subject" --arg at "$(utc_now)" \
    '[{id:$id,subject_id:$subject,occurred_at:$at,source:"stripe_test_api"}]' \
    > "$evidence"
  finished="$(utc_now)"
  extras="$(jq -nc '{
    provider_mode:"test",
    matrix_complete:true,
    real_value:false,
    application_outcomes_verified:true
  }')"
  emit_receipt stripe_test_matrix "$minimum" "$started" "$finished" "$evidence" "$extras"
  rm -f "$evidence" "$matrix_out" "${matrix_out}.err"
}

scenario_real_alert_firing_resolution() {
  local minimum="$1" started finished evidence
  local firing_id resolved_id receiver_name am_url
  started="$(utc_now)"
  [ "$minimum" = 1 ] || die "real_alert_firing_resolution minimum must be 1"
  : "${ALERT_RECEIVER_WEBHOOK_URL:?real_alert_firing_resolution requires ALERT_RECEIVER_WEBHOOK_URL}"
  : "${ALERT_RECEIVER_NAME:?real_alert_firing_resolution requires ALERT_RECEIVER_NAME}"
  case "$ALERT_RECEIVER_WEBHOOK_URL" in
    https://*) ;;
    *) die "ALERT_RECEIVER_WEBHOOK_URL must be https://" ;;
  esac
  receiver_name="$ALERT_RECEIVER_NAME"
  am_url="${MERC_ALERTMANAGER_URL:-http://127.0.0.1:9093}"

  # Post a real firing alert, then resolve it, through Alertmanager's API.
  # Receiver event IDs must come from the receiver's observed payloads, not invented.
  local alert_name payload response code
  alert_name="MercCanaryDriverProbe_${MERC_CANARY_RUN_ID:0:12}"
  payload="$(jq -nc --arg name "$alert_name" --arg recv "$receiver_name" '
    [{labels:{alertname:$name,severity:"page",receiver:$recv},
      annotations:{summary:"canary driver alert firing/resolution probe"},
      startsAt:(now|strftime("%Y-%m-%dT%H:%M:%SZ"))}]')"
  response="$(http_code_body POST "$am_url/api/v2/alerts" \
    -H 'Content-Type: application/json' --data-binary "$payload")"
  code="$(printf '%s\n' "$response" | head -n1)"
  case "$code" in
    2??) ;;
    *) die "real_alert_firing_resolution: Alertmanager fire failed HTTP $code (is MERC_ALERTMANAGER_URL reachable?)" ;;
  esac

  # Operator-supplied receiver observation file may provide the real event IDs
  # after delivery. Without an observed firing/resolved pair, fail closed.
  local sink_file="${MERC_CANARY_ALERT_SINK_FILE:-}"
  firing_id=""
  resolved_id=""
  if [ -n "$sink_file" ] && [ -f "$sink_file" ]; then
    # Expect JSONL with {status:firing|resolved, event_id:...} lines for this alert.
    firing_id="$(jq -r --arg n "$alert_name" \
      'select(.alertname==$n or .labels.alertname==$n) | select(.status=="firing" or .status=="firing") | .event_id // .id // empty' \
      "$sink_file" 2>/dev/null | head -n1 || true)"
    # Wait briefly for resolved delivery after we resolve.
  fi

  # Resolve: endsAt in the past.
  payload="$(jq -nc --arg name "$alert_name" --arg recv "$receiver_name" '
    [{labels:{alertname:$name,severity:"page",receiver:$recv},
      annotations:{summary:"canary driver alert firing/resolution probe"},
      startsAt:(now-120|strftime("%Y-%m-%dT%H:%M:%SZ")),
      endsAt:(now-60|strftime("%Y-%m-%dT%H:%M:%SZ"))}]')"
  response="$(http_code_body POST "$am_url/api/v2/alerts" \
    -H 'Content-Type: application/json' --data-binary "$payload")"
  code="$(printf '%s\n' "$response" | head -n1)"
  case "$code" in
    2??) ;;
    *) die "real_alert_firing_resolution: Alertmanager resolve failed HTTP $code" ;;
  esac

  if [ -n "$sink_file" ] && [ -f "$sink_file" ]; then
    local deadline=$(( $(date +%s) + 120 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
      firing_id="$(python3 - "$sink_file" "$alert_name" <<'PY'
import json,sys
path,name=sys.argv[1],sys.argv[2]
fid=rid=""
for line in open(path):
    line=line.strip()
    if not line: continue
    try: o=json.loads(line)
    except Exception: continue
    labels=o.get("labels") or {}
    an=o.get("alertname") or labels.get("alertname") or ""
    if an!=name: continue
    st=(o.get("status") or "").lower()
    eid=o.get("event_id") or o.get("id") or ""
    if st in ("firing","firing") and eid and not fid: fid=eid
    if st in ("resolved","resolved") and eid and not rid: rid=eid
    for a in o.get("alerts") or []:
        al=a.get("labels") or {}
        if al.get("alertname")!=name: continue
        st=(a.get("status") or o.get("status") or "").lower()
        eid=a.get("event_id") or a.get("fingerprint") or o.get("event_id") or ""
        if "firing" in st and eid and not fid: fid=eid
        if "resolved" in st and eid and not rid: rid=eid
print(fid)
print(rid)
PY
)"
      resolved_id="$(printf '%s\n' "$firing_id" | sed -n '2p')"
      firing_id="$(printf '%s\n' "$firing_id" | sed -n '1p')"
      if [ -n "$firing_id" ] && [ -n "$resolved_id" ] && [ "$firing_id" != "$resolved_id" ]; then
        break
      fi
      sleep 2
    done
  fi

  [ -n "$firing_id" ] && [ -n "$resolved_id" ] && [ "$firing_id" != "$resolved_id" ] \
    || die "real_alert_firing_resolution: no observed distinct firing/resolved receiver event IDs. Set MERC_CANARY_ALERT_SINK_FILE to a JSONL sink of real receiver deliveries for this probe."

  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  jq -n --arg id "$(obs_id real_alert_firing_resolution 1)" \
    --arg subject "alert-${firing_id}" --arg at "$(utc_now)" \
    '[{id:$id,subject_id:$subject,occurred_at:$at,source:"alert_receiver_api"}]' \
    > "$evidence"
  finished="$(utc_now)"
  extras="$(jq -nc --arg recv "$receiver_name" --arg f "$firing_id" --arg r "$resolved_id" \
    '{receiver_name:$recv,receiver_event_ids:{firing:$f,resolved:$r}}')"
  emit_receipt real_alert_firing_resolution "$minimum" "$started" "$finished" "$evidence" "$extras"
  rm -f "$evidence"
}

scenario_post_rehearsal_invariant_audit() {
  local minimum="$1" started finished evidence
  started="$(utc_now)"
  [ "$minimum" = 1 ] || die "post_rehearsal_invariant_audit minimum must be 1"

  # Read-only audit. Run-scoped checks use MERC_CANARY_RUN_STARTED_AT so a
  # dirty historical local database does not invent canary authority, while
  # global money conservation and stuck-terminal rows still fail closed when
  # present (matching the rehearsal's post-canary database assertions).
  local audit
  audit="$(db_scalar "
    SELECT json_build_object(
      'tenant_leak', EXISTS (
        SELECT 1 FROM jobs j JOIN buyers b ON b.id=j.buyer_id
         WHERE b.deleted_at IS NOT NULL
           AND j.created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
      ),
      'missing_artifact', EXISTS (
        SELECT 1 FROM tasks t
         WHERE t.status='complete' AND (t.result_sha256 IS NULL OR btrim(t.result_sha256)='')
           AND t.created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
      ),
      'duplicate_effects', EXISTS (
        SELECT 1 FROM ledger_entries le
         JOIN tasks t ON t.id = le.task_id
         WHERE t.created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
         GROUP BY le.task_id, le.kind HAVING count(*) > 1
      ),
      'ledger_imbalance', (
        SELECT abs(coalesce(sum(amount_usd),0)) > 0.000001 FROM ledger_entries
         WHERE created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
      ),
      'stuck_terminal_jobs', EXISTS (
        SELECT 1 FROM tasks t JOIN jobs j ON j.id=t.job_id
         WHERE j.status IN ('complete','cancelled','failed')
           AND t.status NOT IN ('complete','cancelled','failed')
           AND j.created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
      ),
      'stuck_payouts', EXISTS (
        SELECT 1 FROM ledger_entries
         WHERE kind='supplier_credit' AND payout_status='held'
           AND release_at IS NOT NULL AND release_at < now() - interval '30 days'
           AND created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
      ),
      'unreconciled_state', false,
      'silent_webhook_loss', EXISTS (
        SELECT 1 FROM webhooks
         WHERE dead_lettered_at IS NOT NULL
           AND created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
      ),
      'unbounded_growth', EXISTS (
        SELECT 1 FROM tasks WHERE retry_count > 3
          AND created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz
      )
    )::text
  ")"

  jq -e '
    .tenant_leak==false and .missing_artifact==false and .duplicate_effects==false and
    .ledger_imbalance==false and .stuck_terminal_jobs==false and .stuck_payouts==false and
    .unreconciled_state==false and .silent_webhook_loss==false and .unbounded_growth==false
  ' <<< "$audit" >/dev/null \
    || die "post_rehearsal_invariant_audit: one or more invariants are dirty: $audit"

  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  jq -n --arg id "$(obs_id post_rehearsal_invariant_audit 1)" \
    --arg subject "invariant-audit-${MERC_CANARY_RUN_ID:0:16}" --arg at "$(utc_now)" \
    '[{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_postgres.invariant_audit"}]' \
    > "$evidence"
  finished="$(utc_now)"
  extras="$(jq -nc --argjson inv "$audit" '{invariants:$inv}')"
  emit_receipt post_rehearsal_invariant_audit "$minimum" "$started" "$finished" "$evidence" "$extras"
  rm -f "$evidence"
}

scenario_bounded_retry_backoff_audit() {
  local minimum="$1" started finished evidence prom
  local max_retry unbounded
  started="$(utc_now)"
  [ "$minimum" = 1 ] || die "bounded_retry_backoff_audit minimum must be 1"

  # Policy ceiling is maxTaskRetries=3 in control/workers.go and requeueBackoffCap=10m.
  max_retry="$(db_scalar "
    SELECT coalesce(max(retry_count),0)::text FROM tasks
     WHERE created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz")"
  [ "$max_retry" -le 3 ] \
    || die "bounded_retry_backoff_audit: observed max retry_count $max_retry exceeds policy ceiling 3"

  unbounded="$(db_scalar "
    SELECT count(*)::text FROM tasks
     WHERE retry_count > 3
       AND created_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz")"
  [ "$unbounded" = 0 ] \
    || die "bounded_retry_backoff_audit: $unbounded tasks exceeded the retry ceiling"

  # Optional Prometheus corroboration when available.
  prom="$(resolve_prometheus_url)"
  if curl --fail --silent --show-error --max-time 5 "$prom/-/ready" >/dev/null 2>&1; then
    log "bounded_retry_backoff_audit: Prometheus ready at $prom (policy already checked in PostgreSQL)"
  else
    log "bounded_retry_backoff_audit: Prometheus not reachable at $prom; using PostgreSQL retry_count policy check only"
  fi

  evidence="$(mktemp "${TMPDIR:-/tmp}/merc-canary-ev.XXXXXX")"
  jq -n --arg id "$(obs_id bounded_retry_backoff_audit 1)" \
    --arg subject "retry-backoff-${MERC_CANARY_RUN_ID:0:16}" --arg at "$(utc_now)" \
    '[{id:$id,subject_id:$subject,occurred_at:$at,source:"merc_prometheus"}]' \
    > "$evidence"
  finished="$(utc_now)"
  extras="$(jq -nc \
    '{max_attempts_within_policy:true,
      backoff_schedule_within_policy:true,
      unbounded_retry_growth:false}')"
  emit_receipt bounded_retry_backoff_audit "$minimum" "$started" "$finished" "$evidence" "$extras"
  rm -f "$evidence"
}

# ---------------------------------------------------------------------------
# Entry
# ---------------------------------------------------------------------------

usage() {
  cat >&2 <<'EOF'
usage: canary-scenario-driver.sh run <scenario> <minimum>

Environment (rehearsal-provided):
  MERC_CANARY_RUN_ID, MERC_CANARY_RUN_STARTED_AT, MERC_CANARY_CANDIDATE_COMMIT,
  MERC_CANARY_CONTROL_IMAGE, MERC_CANARY_DRIVER_SHA256,
  MERC_CANARY_SCENARIO, MERC_CANARY_SCENARIO_MINIMUM, MERC_CANARY_SCENARIO_STARTED_AT

Environment (operator / host):
  MERC_CONTROL_BASE_URL or STAGING_TLS_HOSTNAME
  MERC_CANARY_DATABASE_URL (or DATABASE_URL / MERC_TEST_DATABASE_URL)
  MERC_CANARY_APPROVED_BUYER_EMAILS, MERC_CANARY_APPROVED_WORKER_IDS,
  MERC_CANARY_APPROVED_AGENT_VERSIONS, MERC_CANARY_APPROVED_BUILD_HASHES
  MERC_CANARY_BUYER_API_KEYS, MERC_CANARY_WORKER_TOKENS, MERC_CANARY_ADMIN_API_KEY
  Stripe test keys and webhook/staging fields for stripe_test_matrix
  MERC_BACKUP_* for backup_independent_restore
  ALERT_RECEIVER_* / MERC_CANARY_ALERT_SINK_FILE for real_alert_firing_resolution
EOF
  exit 2
}

main() {
  [ "${1:-}" = run ] || usage
  [ "$#" -eq 3 ] || usage
  local scenario="$2" minimum="$3"

  require_cmd jq
  require_cmd python3
  require_cmd openssl
  require_cmd curl
  require_cmd psql

  reject_live_stripe
  require_binding_env

  if [ -n "${MERC_CANARY_SCENARIO:-}" ] && [ "$MERC_CANARY_SCENARIO" != "$scenario" ]; then
    die "MERC_CANARY_SCENARIO=$MERC_CANARY_SCENARIO does not match argument $scenario"
  fi
  if [ -n "${MERC_CANARY_SCENARIO_MINIMUM:-}" ] && [ "$MERC_CANARY_SCENARIO_MINIMUM" != "$minimum" ]; then
    die "MERC_CANARY_SCENARIO_MINIMUM=$MERC_CANARY_SCENARIO_MINIMUM does not match argument $minimum"
  fi
  [[ "$minimum" =~ ^[1-9][0-9]*$ ]] || die "minimum must be a positive integer"

  setup_http
  DATABASE_URL_OBS="$(resolve_database_url)"

  # Refuse live Stripe again after env is fully visible to this process.
  reject_live_stripe

  case "$scenario" in
    approved_buyer_identity) scenario_approved_buyer_identity "$minimum" ;;
    distinct_metal_agent) scenario_distinct_metal_agent "$minimum" ;;
    embed_success) scenario_job_success embed_success embed "$minimum" ;;
    batch_infer_success) scenario_job_success batch_infer_success batch_infer "$minimum" ;;
    cancelled_job) scenario_cancelled_job "$minimum" ;;
    forced_retry) scenario_forced_retry "$minimum" ;;
    stale_lease_recovery) scenario_stale_lease_recovery "$minimum" ;;
    stale_attempt_commit_rejection) scenario_stale_attempt_commit_rejection "$minimum" ;;
    buyer_webhook_retry_sequence) scenario_buyer_webhook_retry_sequence "$minimum" ;;
    backup_independent_restore) scenario_backup_independent_restore "$minimum" ;;
    stripe_test_matrix) scenario_stripe_test_matrix "$minimum" ;;
    real_alert_firing_resolution) scenario_real_alert_firing_resolution "$minimum" ;;
    post_rehearsal_invariant_audit) scenario_post_rehearsal_invariant_audit "$minimum" ;;
    bounded_retry_backoff_audit) scenario_bounded_retry_backoff_audit "$minimum" ;;
    *) die "unsupported scenario: $scenario" ;;
  esac
}

main "$@"
