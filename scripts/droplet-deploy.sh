#!/usr/bin/env bash
# One blind run on the droplet: preflight, deploy, verify, and roll back on its
# own if verification fails.
#
# docs/DEPLOY_MERC_DROPLET.md is the long form. This is the same sequence with
# the judgement calls already made, so it can be pasted into an SSH session
# without reading anything first.
#
#   scp scripts/droplet-deploy.sh root@<droplet>:/opt/merc/
#   ssh root@<droplet> 'cd /opt/merc && bash droplet-deploy.sh'
#
# It refuses rather than guesses. Every refusal names the fix, and nothing is
# touched until every check has passed -- a deploy that fails halfway is worse
# than one that never starts, because the old container is already gone.
#
#   --dry-run   run every check, change nothing
#   --yes       skip the confirmation prompt

set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
DRY=0; YES=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    --yes) YES=1 ;;
    *) printf 'unknown argument: %s\n' "$arg" >&2; exit 2 ;;
  esac
done

ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
die()  { printf '\nSTOPPED: %s\n' "$*" >&2; exit 1; }
FAILED=0
need() { if "$@"; then :; else FAILED=1; fi; }

printf 'merc droplet deploy\n\n'
printf 'preflight\n'

# ---------------------------------------------------------------- the basics
for tool in docker git curl jq; do
  command -v "$tool" >/dev/null && ok "$tool present" || { bad "$tool is required"; FAILED=1; }
done
docker compose version >/dev/null 2>&1 && ok "docker compose v2" \
  || { bad "docker compose v2 is required"; FAILED=1; }

[ -f "$ROOT/.env" ] || die ".env is missing. Copy .env.example and fill it in; §1 of
       docs/DEPLOY_MERC_DROPLET.md lists every variable the control plane demands."
perms=$(stat -c '%a' "$ROOT/.env" 2>/dev/null || stat -f '%Lp' "$ROOT/.env")
[ "$perms" = "600" ] && ok ".env is 600" || warn ".env is $perms; should be 600 (chmod 600 .env)"

# The image records MERC_BUILD_COMMIT and reports modified:false regardless of
# whether the tree was dirty, so an image built from uncommitted work claims a
# provenance it does not have.
# 2>/dev/null on its own would turn a git FAILURE into an empty string, which
# reads as "clean". Check git works first, then check the tree.
if ! git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  bad "not a git repository (or git failed); cannot confirm what will be built"
  FAILED=1
elif [ -n "$(git -C "$ROOT" status --porcelain)" ]; then
  bad "worktree is dirty; commit first or /version lies about what is running"
  FAILED=1
else
  ok "worktree clean, /version will be true"
fi

# ------------------------------------------------------- the env cutover trap
# CX_TOKEN_KEY is the one that cannot be regenerated: control/crypto.go derives
# the AES key as sha256(value), so a new value makes every sealed OAuth token
# and webhook secret already in Postgres permanently undecryptable.
set -a; . "$ROOT/.env"; set +a

if [ -n "${CX_TOKEN_KEY:-}" ] && [ -n "${MERC_TOKEN_KEY:-}" ] \
   && [ "$CX_TOKEN_KEY" != "$MERC_TOKEN_KEY" ]; then
  die "CX_TOKEN_KEY and MERC_TOKEN_KEY are BOTH set and DIFFER.
       control/crypto.go derives the encryption key as sha256(value). Whichever
       one the binary reads, the other half of your sealed secrets becomes
       permanently undecryptable. Copy the OLD value byte-identically into the
       new name, then remove the old one."
fi
if [ -z "${MERC_TOKEN_KEY:-}" ] && [ -n "${CX_TOKEN_KEY:-}" ]; then
  warn "only CX_TOKEN_KEY is set. The binary reads MERC_TOKEN_KEY. Copy the value
        byte-identically: MERC_TOKEN_KEY=\"\$CX_TOKEN_KEY\""
fi
# ${#var:-0} is not valid bash -- you cannot combine length with a default --
# and under `set -e` it aborted the script before every later guard ran. The
# first version of this file did exactly that, so the Stripe, webhook and
# MERC_ENV checks below silently never executed.
_token_key="${MERC_TOKEN_KEY:-}"
if [ "${#_token_key}" -ge 32 ]; then
  ok "MERC_TOKEN_KEY is long enough"
else
  bad "MERC_TOKEN_KEY must be at least 32 bytes (it is ${#_token_key})"
  FAILED=1
fi

# MERC_ENV gates the production hardening refusal. If it resolves empty the
# refusal is skipped and control boots writing plain:-prefixed secrets.
case "${MERC_ENV:-}" in
  production) ok "MERC_ENV=production, hardening refusal is active" ;;
  "") bad "MERC_ENV is unset. control/main.go gates its production hardening
        refusal on this, so it will boot with a warning and write plain:-prefixed
        secrets. Set MERC_ENV=production."; FAILED=1 ;;
  *) warn "MERC_ENV=${MERC_ENV}; production hardening is NOT active" ;;
esac

# ------------------------------------------------------------------- secrets
[ -n "${STRIPE_SECRET_KEY:-}" ] || { bad "STRIPE_SECRET_KEY is unset"; FAILED=1; }
case "${STRIPE_SECRET_KEY:-}" in
  sk_live_*|rk_live_*) ok "Stripe key is live-mode, as production requires" ;;
  sk_test_*|rk_test_*) bad "STRIPE_SECRET_KEY is a TEST key. Production takes real
        money; a test key means every charge silently succeeds against nothing."
        FAILED=1 ;;
  "") ;;
  *) bad "STRIPE_SECRET_KEY is not a recognisable Stripe secret key"; FAILED=1 ;;
esac
[ -n "${STRIPE_WEBHOOK_SECRET:-}" ] && [ -n "${MERC_CONNECT_WEBHOOK_SECRET:-}" ] \
  && [ "$STRIPE_WEBHOOK_SECRET" != "$MERC_CONNECT_WEBHOOK_SECRET" ] \
  && ok "billing and Connect webhook secrets are distinct" \
  || { bad "STRIPE_WEBHOOK_SECRET and MERC_CONNECT_WEBHOOK_SECRET must both be set
        and must differ; one endpoint's signature must not validate the other's"
       FAILED=1; }

alert_file="${MERC_ALERT_RECEIVER_URL_FILE:-${CX_ALERT_RECEIVER_URL_FILE:-}}"
if [ -n "$alert_file" ] && [ -s "$alert_file" ]; then
  ok "alert receiver URL file present"
else
  bad "MERC_ALERT_RECEIVER_URL_FILE must point at a non-empty file containing the
        HTTPS alert webhook URL. Without it, alerts fire into nothing and the
        first outage is silent."
  FAILED=1
fi

[ -n "${SITE_HOST:-}" ] && ok "SITE_HOST=$SITE_HOST" \
  || { bad "SITE_HOST is unset. Every verification URL below interpolates it, and
        under 'set -u' the script would abort mid-deploy."; FAILED=1; }

# --------------------------------------------------------------- compose sane
docker compose -f "$ROOT/docker-compose.prod.yml" -f "$ROOT/docker-compose.observability.yml" \
  config -q 2>/dev/null && ok "compose files validate" \
  || { bad "compose config is invalid; run it yourself to see why:
        docker compose -f docker-compose.prod.yml -f docker-compose.observability.yml config"
       FAILED=1; }

[ "$FAILED" -eq 0 ] || die "preflight failed. Nothing was changed. Fix the items above."
printf '\n  preflight passed\n\n'
[ "$DRY" -eq 1 ] && { printf 'dry run: stopping before any change.\n'; exit 0; }

# ------------------------------------------------------------------- confirm
# The commit currently serving. scripts/rollback.sh takes a COMMIT, not an
# image id, so this is the value a rollback actually needs -- capturing an image
# id instead is why the first version of this script could not roll back at all.
previous_commit=$(curl -fsS --max-time 5 "https://${SITE_HOST}/version" 2>/dev/null \
          | jq -r '.commit // empty' 2>/dev/null || printf '')
current="${previous_commit:-unreachable}"
printf 'about to deploy\n'
printf '  host:        %s\n' "${SITE_HOST:-unset}"
printf '  from commit: %s\n' "$(git -C "$ROOT" rev-parse --short HEAD)"
printf '  live now:    %s\n\n' "$current"
if [ "$YES" -eq 0 ]; then
  printf 'Type yes to deploy: '; read -r answer
  [ "$answer" = yes ] || die "cancelled by operator"
fi

# --------------------------------------------------------------------- backup
# Before, not after. A backup taken after a bad deploy is a backup of the
# problem.
printf '\nbacking up first\n'
if bash "$ROOT/scripts/backup.sh"; then ok "backup complete"
else die "backup FAILED. Not deploying: without a good backup a bad deploy has no
       way back."; fi

# --------------------------------------------------------------------- deploy
printf '\ndeploying\n'
if bash "$ROOT/scripts/deploy.sh"; then ok "containers up"
else die "deploy failed. Nothing was rolled back because nothing replaced the
       running container yet; run scripts/deploy.sh by hand to see the error."; fi

# --------------------------------------------------------------------- verify
printf '\nverifying\n'
verify_failed=0
want=$(git -C "$ROOT" rev-parse HEAD)

# /healthz, not /health. control/api.go serves GET /healthz and GET /readyz;
# there is no /health route, so the first version of this check failed on EVERY
# deploy and took the rollback branch on deploys that had actually succeeded.
health_ok=0
for _ in $(seq 1 30); do
  sleep 4
  if curl -fsS --max-time 10 "https://${SITE_HOST}/healthz" >/dev/null 2>&1; then
    health_ok=1; break
  fi
done
[ "$health_ok" -eq 1 ] && ok "/healthz responds over TLS" \
  || { bad "/healthz did not respond within 120s"; verify_failed=1; }

# The commit check must REQUIRE a commit. `${want:0:${#got}}` with an empty got
# expands to the empty string and matches anything, so a /version answering {}
# -- including an OLD container still serving -- passed as "the commit just
# deployed". That was the sole condition gating rollback.
commit_ok=0
for _ in $(seq 1 15); do
  body=$(curl -fsS --max-time 10 "https://${SITE_HOST}/version" 2>/dev/null) || { sleep 4; continue; }
  got=$(printf '%s' "$body" | jq -r '.commit // empty' 2>/dev/null || printf '')
  if [ "${#got}" -ge 7 ] && [ "$got" = "${want:0:${#got}}" ]; then
    ok "/version reports ${got}, the commit just deployed"
    commit_ok=1; break
  fi
  sleep 4
done
if [ "$commit_ok" -ne 1 ]; then
  bad "/version never reported the deployed commit (${want:0:12}); last saw '${got:-nothing}'"
  verify_failed=1
fi

if [ "$verify_failed" -ne 0 ]; then
  printf '\nverification FAILED\n'
  if [ -z "$previous_commit" ]; then
    die "cannot roll back: the commit serving before this deploy was never
       readable from https://${SITE_HOST}/version, so there is no target. Roll
       back by hand:  bash scripts/rollback.sh <previous-40-char-commit>"
  fi
  # A REAL rollback. Re-running deploy.sh would rebuild the same commit from the
  # same worktree and redeploy the identical broken build while reporting that
  # it had rolled back.
  printf 'rolling back to %s\n' "${previous_commit:0:12}"
  if bash "$ROOT/scripts/rollback.sh" "$previous_commit"; then
    if curl -fsS --max-time 10 "https://${SITE_HOST}/healthz" >/dev/null 2>&1; then
      warn "rolled back to ${previous_commit:0:12} and it is serving; the new build is NOT live"
    else
      die "rolled back to ${previous_commit:0:12} but /healthz is still not
       responding. THE SITE MAY BE DOWN. Investigate now."
    fi
  else
    die "rollback FAILED. THE SITE MAY BE DOWN, running a build that did not
       verify. Roll back by hand:  bash scripts/rollback.sh $previous_commit"
  fi
  exit 1
fi

printf '\ndeployed and verified.\n\n'
printf 'still yours to do, and nothing above did them:\n'
printf '  - re-point both Stripe webhook endpoints at https://%s (§6 of the doc)\n' "${SITE_HOST}"
printf '  - update the Alertmanager receiver filters BEFORE renaming any metric\n'
printf '  - confirm a real alert reaches a human: make alert-delivery-test\n'
