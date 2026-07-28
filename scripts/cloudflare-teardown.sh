#!/usr/bin/env bash
# Tear down retired Cloudflare estate: zones, Pages projects and R2 buckets.
#
# Deleting a zone removes its DNS but leaves the things that were hanging off it
# still provisioned and still billable -- a Pages project keeps serving on its
# .pages.dev name, an R2 bucket keeps storing objects. So this handles all three
# rather than leaving orphans nobody remembers to look for.
#
#   bash scripts/cloudflare-teardown.sh plan      # show everything, delete nothing
#   bash scripts/cloudflare-teardown.sh apply     # delete, with per-item confirmation
#   bash scripts/cloudflare-teardown.sh apply --yes   # no prompts (scripted use)
#
# KEEP below is the whitelist. Anything not on it is a deletion candidate, which
# is the safer default: a forgotten zone shows up as a candidate rather than
# silently surviving.

set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck disable=SC1091
[ -f "$ROOT/.merc-secrets.env" ] && { set -a; . "$ROOT/.merc-secrets.env"; set +a; }
: "${CLOUDFLARE_API_TOKEN:?CLOUDFLARE_API_TOKEN required (run scripts/merc-secrets.sh)}"

# Everything else is a candidate. Keep this list short and explicit.
KEEP_ZONES="mercmerc.net mercmerc.app kilongozilaw.ca"
BACKUP_DIR="$ROOT/.cloudflare-zone-backups"
MODE="${1:-plan}"
ASSUME_YES=0
[ "${2:-}" = "--yes" ] && ASSUME_YES=1

say()  { printf '%s\n' "$*"; }
ok()   { printf '  \033[32m%s\033[0m\n' "$*"; }
warn() { printf '  \033[33m%s\033[0m\n' "$*"; }
bad()  { printf '  \033[31m%s\033[0m\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cf() {
  local method="$1" path="$2"
  printf 'header = "Authorization: Bearer %s"\n' "$CLOUDFLARE_API_TOKEN" \
    | curl -sS --config - -H 'content-type: application/json' --max-time 45 \
        -X "$method" "https://api.cloudflare.com/client/v4$path"
}

kept() { case " $KEEP_ZONES " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

confirm() {
  [ "$ASSUME_YES" -eq 1 ] && return 0
  printf '    delete %s? type yes: ' "$1"
  local a; read -r a
  [ "$a" = "yes" ]
}

account_id() {
  if [ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ]; then printf '%s' "$CLOUDFLARE_ACCOUNT_ID"; return; fi
  cf GET /accounts | python3 -c '
import json,sys
d=json.load(sys.stdin); r=d.get("result") or []
print(r[0]["id"] if r else "")'
}

# ------------------------------------------------------------------- zones
do_zones() {
  say "zones"
  local list
  list=$(cf GET "/zones?per_page=50" | python3 -c '
import json,sys
d=json.load(sys.stdin)
if not d.get("success"): raise SystemExit
for z in d.get("result",[]): print(z["name"], z["id"])')
  [ -n "$list" ] || { bad "  cannot list zones"; return; }

  while read -r name id; do
    [ -z "$name" ] && continue
    if kept "$name"; then ok "  KEEP   $name"; continue; fi
    warn "  DELETE $name ($id)"
    [ "$MODE" = plan ] && continue
    # Records are saved before the destructive call: the zone cannot be
    # undeleted, but its DNS can be rebuilt from this.
    mkdir -p "$BACKUP_DIR"
    cf GET "/zones/$id/dns_records?per_page=200" > "$BACKUP_DIR/$name.records.json"
    chmod 600 "$BACKUP_DIR/$name.records.json"
    confirm "zone $name" || { warn "    skipped"; continue; }
    if cf DELETE "/zones/$id" | grep -q '"success":true'; then ok "    deleted $name"
    else bad "    FAILED (token likely lacks account-scope zone edit)"; fi
  done <<< "$list"
}

# ------------------------------------------------------------------- pages
do_pages() {
  local acct; acct=$(account_id)
  [ -n "$acct" ] || { warn "pages: no account id visible to this token, skipping"; return; }
  say "pages projects"
  local list
  list=$(cf GET "/accounts/$acct/pages/projects" | python3 -c '
import json,sys
d=json.load(sys.stdin)
if not d.get("success"): raise SystemExit
for p in d.get("result",[]): print(p["name"], p.get("subdomain",""))' 2>/dev/null)
  [ -n "$list" ] || { warn "  none visible"; return; }
  while read -r name sub; do
    [ -z "$name" ] && continue
    case "$name" in
      *merc*|*kilongozi*) ok "  KEEP   $name"; continue ;;
    esac
    warn "  DELETE $name ($sub)"
    [ "$MODE" = plan ] && continue
    confirm "pages project $name" || { warn "    skipped"; continue; }
    if cf DELETE "/accounts/$acct/pages/projects/$name" | grep -q '"success":true'; then ok "    deleted $name"
    else bad "    FAILED"; fi
  done <<< "$list"
}

# ---------------------------------------------------------------------- r2
do_r2() {
  local acct; acct=$(account_id)
  [ -n "$acct" ] || { warn "r2: no account id visible to this token, skipping"; return; }
  say "r2 buckets"
  local list
  list=$(cf GET "/accounts/$acct/r2/buckets" | python3 -c '
import json,sys
d=json.load(sys.stdin)
if not d.get("success"): raise SystemExit
for b in (d.get("result") or {}).get("buckets",[]): print(b["name"])' 2>/dev/null)
  [ -n "$list" ] || { warn "  none visible"; return; }
  for name in $list; do
    case "$name" in
      *merc*|*kilongozi*) ok "  KEEP   $name"; continue ;;
    esac
    warn "  DELETE $name"
    [ "$MODE" = plan ] && continue
    confirm "r2 bucket $name" || { warn "    skipped"; continue; }
    # A bucket with objects will refuse to delete; that refusal is useful
    # information, not an error to work around.
    if cf DELETE "/accounts/$acct/r2/buckets/$name" | grep -q '"success":true'; then ok "    deleted $name"
    else bad "    FAILED (non-empty buckets must be emptied first)"; fi
  done
}

case "$MODE" in
  plan)  say "PLAN ONLY - nothing will be deleted"; say ""; do_zones; say ""; do_pages; say ""; do_r2 ;;
  apply) do_zones; say ""; do_pages; say ""; do_r2 ;;
  *) die "usage: cloudflare-teardown.sh [plan|apply] [--yes]" ;;
esac
