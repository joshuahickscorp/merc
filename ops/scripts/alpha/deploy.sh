#!/usr/bin/env bash
# P1-STAGING deploy runner.
#
# This script never opens SSH, never starts containers on the droplet, and never
# writes a PASS staging receipt unless --record-pass is used by the supervisor
# AFTER external probes pass. Default action is --print-runbook.
#
#   ops/scripts/alpha/deploy.sh --print-runbook
#   ops/scripts/alpha/deploy.sh --check
#   ops/scripts/alpha/deploy.sh --record-pass   # supervisor only, after probes PASS
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
# shellcheck source=ops/scripts/alpha/lib.sh
. "$ROOT/ops/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: ops/scripts/alpha/deploy.sh --print-runbook|--check|--record-pass

--print-runbook   print the exact supervisor SSH/docker steps (no boot required)
--check           fail-closed readiness (boot, no live Stripe, local artifacts)
--record-pass     SUPERVISOR stamps P1-STAGING after probes PASS (no ssh)
USAGE
  exit 2
}

print_runbook() {
  local commit host storage droplet
  commit="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || printf 'UNKNOWN_COMMIT')"
  host="$(alpha_staging_host)"
  storage="$(alpha_storage_host)"
  droplet="${STAGING_SSH_TARGET:-root@192.241.134.31}"

  cat <<EOF
# P1-STAGING runbook — SUPERVISOR EXECUTES (this worktree cannot ssh)
# Candidate HEAD: $commit
# Public TLS host: $host
# Storage TLS host: $storage
# Droplet: $droplet  (192.241.134.31)
# Existing on droplet: merc-postgres-1 + merc-minio-1 UP (healthy).
# Control is DOWN. Nothing listens on 80/443.
# Payment mode MUST be test. Live Stripe is prohibited.
# Do not recreate postgres/minio. Do not docker compose down.

## 0. Preconditions the supervisor confirms
# - evidence/state/alpha-boot-green.json is BOUND PASS at commit $commit
# - tree used to build is this commit (or MERC_CANDIDATE_COMMIT)
# - .env on the droplet is chmod 600; MERC_TOKEN_KEY is the EXISTING value
# - MERC_PAYMENT_MODE=test  MERC_PAYMENT_PROVIDER=stripe  MERC_CANARY_MODE=true
# - MERC_PAYOUT_EXPORT is unset
# - Cloudflare DNS: $host and $storage A 192.241.134.31 (grey-cloud for Caddy HTTP-01)

## 1. SELF-CONTAINED — build the control image (amd64)
# On this Mac, OrbStack (colima/qemu will crash the Go compiler):
docker context use orbstack
cd $ROOT
git rev-parse HEAD   # must equal $commit
git status --porcelain | head   # must be empty or /version will lie
# Runtime-benchmark receipts are LFS. An unsudged checkout ships 129-byte
# pointer files; the image then crash-loops: cited receipt is not JSON.
git lfs pull --include='evidence/perf/runtime-benchmarks/**' --exclude=''
bash ops/scripts/assert-control-receipts-not-lfs.sh
docker build --platform linux/amd64 -f ops/deploy/Dockerfile.control \\
  --build-arg MERC_BUILD_VERSION=v0.1.0-merc-rc1 \\
  --build-arg MERC_BUILD_COMMIT=$commit \\
  --build-arg MERC_BUILD_DATE=\$(date -u +%Y-%m-%dT%H:%M:%SZ) \\
  -t merc/control:${commit:0:12} .
IMAGE_ID=\$(docker image inspect -f '{{.Id}}' merc/control:${commit:0:12})
printf 'built %s\\n' "\$IMAGE_ID"

## 2. SUPERVISOR — copy the image onto the droplet
docker save merc/control:${commit:0:12} | gzip | ssh $droplet 'gunzip | docker load'
ssh $droplet 'docker image inspect merc/control:${commit:0:12} --format {{.Id}} {{.Config.Labels}}'

## 3. SUPERVISOR — env on the droplet (do not regenerate secrets)
# Preferred compose for this 1-vCPU host: ops/deploy/docker-compose.smallhost.yml
# plus ops/deploy/docker-compose.canary.yml (MERC_ENV=staging so TEST payment mode is allowed).
# Overlay only control + caddy (+ prometheus/alertmanager for later gates).
# Existing data plane stays:
ssh $droplet 'docker ps --format "{{.Names}} {{.Status}}" | grep -E "postgres|minio|control|caddy"'
# Expect merc-postgres-1 and merc-minio-1 healthy. Control absent.
#
# In the droplet .env (chmod 600), required for the alpha canary:
#   MERC_ENV=staging
#   MERC_PAYMENT_MODE=test
#   MERC_PAYMENT_PROVIDER=stripe
#   MERC_CANARY_MODE=true
#   MERC_PUBLIC_CONTROL_ORIGIN=https://$host
#   SITE_HOST=$host
#   STORAGE_HOST=$storage
#   MERC_SETTLEMENT_CURRENCY=cad
#   MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE=<operator-approved>
#   MERC_PRICE_FX_REVISION=<immutable>
#   MERC_TOKEN_KEY=<EXISTING byte-identical value>
#   MERC_VERIFICATION_SAMPLE_SECRET=<EXISTING or minted once>
#   STRIPE_SECRET_KEY=sk_test_...          # never sk_live_
#   STRIPE_PUBLISHABLE_KEY=pk_test_...
#   STRIPE_WEBHOOK_SECRET=whsec_...
#   MERC_CONNECT_WEBHOOK_SECRET=whsec_...  # must differ
#   MERC_ALERT_RECEIVER_URL_FILE=/abs/path/to/https-webhook-url
#   MERC_CANARY_APPROVED_BUYER_EMAILS=<exactly two>
#   MERC_CANARY_APPROVED_WORKER_IDS=<exactly two real v4 UUIDs>
#   MERC_BUILD_COMMIT=$commit
# Copy STRIPE_* from a test-mode file. Live prefixes must not exist in the file.

## 4. SUPERVISOR — start control + Caddy against the existing data plane
ssh $droplet 'cd /opt/merc && \\
  docker compose -f ops/deploy/docker-compose.smallhost.yml -f ops/deploy/docker-compose.canary.yml up -d --no-deps --no-recreate postgres minio && \\
  docker compose -f ops/deploy/docker-compose.smallhost.yml -f ops/deploy/docker-compose.canary.yml up -d --no-deps --build=false control caddy prometheus alertmanager'
# If the compose file still has \`build:\` for control, override the image:
#   docker tag merc/control:${commit:0:12} computexchange/control:$commit
#   MERC_BUILD_COMMIT=$commit docker compose -f ops/deploy/docker-compose.smallhost.yml -f ops/deploy/docker-compose.canary.yml up -d --no-deps control caddy
#
# Confirm we did not replace the data plane:
ssh $droplet 'docker inspect -f "{{.Id}} {{.State.Status}} {{.State.StartedAt}}" merc-postgres-1 merc-minio-1'
# StartedAt must be the pre-deploy timestamp.

## 5. SUPERVISOR — Caddy / Cloudflare TLS
# Primary: grey-cloud A records so Caddy HTTP-01 can obtain a cert for $host
# and $storage. The canonical Caddyfile is under ops/deploy/.
# Optional orange-cloud front: Full (strict) + origin cert; then Caddy uses
# that cert instead of HTTP-01. Do not mix Flexible TLS with this origin.
ssh $droplet 'curl -fsS --max-time 5 http://127.0.0.1:8080/healthz || true'
# Wait for Caddy to obtain the cert, then:

## 6. SELF-CONTAINED — external probes (this Mac, after TLS answers)
ops/scripts/alpha/probes.sh --execute

## 7. SUPERVISOR — stamp the staging receipt only if probes PASS
ops/scripts/alpha/deploy.sh --record-pass

# Existing go-closure-deploy.sh is the isolated compose.go-closure.yml path.
# Do NOT run it against this droplet: it would start a second postgres/minio
# and fight merc-postgres-1 / merc-minio-1.
EOF
}

check_local() {
  alpha_require_command jq
  alpha_require_command git
  alpha_load_env_optional
  [ -f "$ROOT/ops/deploy/Dockerfile.control" ] || alpha_die "missing ops/deploy/Dockerfile.control"
  [ -f "$ROOT/ops/deploy/docker-compose.smallhost.yml" ] || alpha_die "missing ops/deploy/docker-compose.smallhost.yml"
  [ -f "$ROOT/ops/deploy/docker-compose.canary.yml" ] || alpha_die "missing ops/deploy/docker-compose.canary.yml"
  grep -q 'MERC_ENV: staging' "$ROOT/ops/deploy/docker-compose.canary.yml" \
    || alpha_die "docker-compose.canary.yml must override MERC_ENV=staging"
  [ -f "$ROOT/ops/deploy/Caddyfile" ] || alpha_die "missing ops/deploy/Caddyfile"
  [ -f "$ROOT/ops/configs/pricing/board.json" ] || alpha_die "missing ops/configs/pricing/board.json"
  [ -f "$ROOT/ops/smallhost/postgresql.conf" ] || alpha_die "missing ops/smallhost/postgresql.conf"
  [ -f "$ROOT/ops/smallhost/Caddyfile.local" ] || alpha_die "missing ops/smallhost/Caddyfile.local"
  grep -q 'FROM gcr.io/distroless/static:nonroot@sha256:' "$ROOT/ops/deploy/Dockerfile.control" \
    || alpha_die "Dockerfile.control is not digest-pinned distroless"
  if [ -x "$ROOT/ops/scripts/assert-control-receipts-not-lfs.sh" ]; then
    bash "$ROOT/ops/scripts/assert-control-receipts-not-lfs.sh" \
      || alpha_die "control image receipts are Git LFS pointers; git lfs pull first"
  fi
  if grep -n 'sk_live_\|rk_live_\|pk_live_' "$ROOT/ops/deploy/docker-compose.smallhost.yml" \
    "$ROOT/ops/deploy/docker-compose.prod.yml" "$ROOT/ops/deploy/docker-compose.canary.yml" >/dev/null 2>&1; then
    alpha_die "compose files must not contain live Stripe prefixes"
  fi
  alpha_log "local artifacts present (ops/deploy Dockerfile, compose, Caddyfile)"
  if ! alpha_check_ready P1-STAGING; then
    alpha_die "P1-STAGING is not execute-ready (boot and/or order)"
  fi
  alpha_log "CHECK ok: boot green, no live Stripe, local deploy artifacts present"
}

record_pass() {
  alpha_load_env_optional
  alpha_require_execute_ready P1-STAGING
  [ -f "$ALPHA_RECEIPT_DIR/P1-STAGING.probes.json" ] \
    || alpha_die "missing probe receipt $ALPHA_RECEIPT_DIR/P1-STAGING.probes.json; run ops/scripts/alpha/probes.sh --execute first"
  jq -e '.status == "PASS"' "$ALPHA_RECEIPT_DIR/P1-STAGING.probes.json" >/dev/null \
    || alpha_die "probe receipt is not PASS"
  dest="$(alpha_write_receipt P1-STAGING PASS alpha_staging)"
  alpha_log "PASS receipt: $dest"
}

mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-runbook) mode=print ;;
    --check) mode=check ;;
    --record-pass) mode=record ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
[ -n "$mode" ] || mode=print

case "$mode" in
  print) print_runbook ;;
  check) check_local ;;
  record) record_pass ;;
  *) usage ;;
esac
