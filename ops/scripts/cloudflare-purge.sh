#!/usr/bin/env bash
# Empty the things Cloudflare refuses to delete while they still hold content.
#
# A plain delete fails on all three retired resources for three different
# reasons, none of them permissions:
#
#   zone   1315     Zones using Cloudflare Registrar can't be deleted
#   pages  8000076  project has too many deployments to be deleted
#   r2     10008    bucket is not empty
#
# Pages and R2 are solvable: remove the contents, then the container. The zone
# is not -- a domain registered through Cloudflare Registrar keeps its zone for
# as long as Cloudflare is the registrar, so the zone cannot be removed at all.
# What CAN be done is strip its DNS so it resolves nowhere, which is the
# practical equivalent of retiring it.
#
#   bash ops/scripts/cloudflare-purge.sh pages <project>
#   bash ops/scripts/cloudflare-purge.sh r2    <bucket>
#   bash ops/scripts/cloudflare-purge.sh dns   <zone>

set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck disable=SC1091
[ -f "$ROOT/.merc-secrets.env" ] && { set -a; . "$ROOT/.merc-secrets.env"; set +a; }
# shellcheck source=ops/scripts/lib/cloudflare-auth.sh
. "$ROOT/ops/scripts/lib/cloudflare-auth.sh"
cf_auth_config >/dev/null 2>&1 || { echo "no Cloudflare credential" >&2; exit 1; }

say()  { printf '%s\n' "$*"; }
ok()   { printf '  \033[32m%s\033[0m\n' "$*"; }
warn() { printf '  \033[33m%s\033[0m\n' "$*"; }
bad()  { printf '  \033[31m%s\033[0m\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

account_id() {
  [ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ] && { printf '%s' "$CLOUDFLARE_ACCOUNT_ID"; return; }
  cf_request GET /accounts | python3 -c '
import json,sys
r=json.load(sys.stdin).get("result") or []
print(r[0]["id"] if r else "")'
}

# ------------------------------------------------------------------- pages
purge_pages() {
  local proj="$1" acct; acct=$(account_id)
  [ -n "$acct" ] || die "no account visible"
  say "pages project $proj"

  # Custom domains come off FIRST. The project delete refuses with 8000028
  # while any remain, and that refusal arrives only after the deployment purge,
  # so discovering it late costs a full pass.
  local doms
  doms=$(cf_request GET "/accounts/$acct/pages/projects/$proj/domains" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for x in (d.get("result") or []): print(x["name"])' 2>/dev/null)
  for dom in $doms; do
    [ -z "$dom" ] && continue
    cf_request DELETE "/accounts/$acct/pages/projects/$proj/domains/$dom" >/dev/null 2>&1 \
      && ok "  removed custom domain $dom"
  done
  local removed=0 page=1
  while :; do
    local ids
    ids=$(cf_request GET "/accounts/$acct/pages/projects/$proj/deployments?page=$page&per_page=25" \
      | python3 -c '
import json,sys
d=json.load(sys.stdin)
for x in (d.get("result") or []): print(x["id"])' 2>/dev/null)
    [ -z "$ids" ] && break
    for id in $ids; do
      # force=true clears ordinary deployments. The ACTIVE PRODUCTION one still
      # refuses with 8000034 and cannot be removed individually at all -- it goes
      # only when the project does, which is fine: the project delete is blocked
      # by deployment COUNT, not by that last one.
      if cf_request DELETE "/accounts/$acct/pages/projects/$proj/deployments/$id?force=true" \
           | grep -q '"success":true'; then
        removed=$((removed+1))
        [ $((removed % 10)) -eq 0 ] && say "    removed $removed deployments"
      fi
    done
  done
  ok "  removed $removed deployment(s)"
  if cf_request DELETE "/accounts/$acct/pages/projects/$proj" | grep -q '"success":true'; then
    ok "  deleted project $proj"
  else
    bad "  project delete still refused"
    cf_request DELETE "/accounts/$acct/pages/projects/$proj" | head -c 200; echo
  fi
}

# ---------------------------------------------------------------------- r2
purge_r2() {
  local bucket="$1" acct; acct=$(account_id)
  [ -n "$acct" ] || die "no account visible"
  : "${R2_ACCESS_KEY_ID:?R2_ACCESS_KEY_ID required to delete objects}"
  : "${R2_SECRET_ACCESS_KEY:?R2_SECRET_ACCESS_KEY required}"
  local endpoint="https://${acct}.r2.cloudflarestorage.com"
  say "r2 bucket $bucket"

  # Object deletion goes over the S3 API with sigv4; the Cloudflare REST API
  # cannot remove objects, only whole buckets, and only when already empty.
  local removed=0
  while :; do
    local keys
    keys=$(curl -sS --max-time 40 --aws-sigv4 "aws:amz:auto:s3" \
             --user "${R2_ACCESS_KEY_ID}:${R2_SECRET_ACCESS_KEY}" \
             "$endpoint/$bucket?list-type=2&max-keys=200" 2>/dev/null \
           | python3 -c '
import sys,re
x=sys.stdin.read()
for m in re.findall(r"<Key>([^<]+)</Key>", x): print(m)' 2>/dev/null)
    [ -z "$keys" ] && break
    while IFS= read -r k; do
      [ -z "$k" ] && continue
      curl -sS -o /dev/null --max-time 30 -X DELETE --aws-sigv4 "aws:amz:auto:s3" \
        --user "${R2_ACCESS_KEY_ID}:${R2_SECRET_ACCESS_KEY}" \
        "$endpoint/$bucket/$k" 2>/dev/null && removed=$((removed+1))
    done <<< "$keys"
    say "    removed $removed object(s)"
  done
  ok "  removed $removed object(s)"
  if cf_request DELETE "/accounts/$acct/r2/buckets/$bucket" | grep -q '"success":true'; then
    ok "  deleted bucket $bucket"
  else
    bad "  bucket delete still refused"
    cf_request DELETE "/accounts/$acct/r2/buckets/$bucket" | head -c 200; echo
  fi
}

# --------------------------------------------------------------------- dns
purge_dns() {
  local zone="$1"
  local id
  id=$(cf_request GET "/zones?name=$zone" | python3 -c '
import json,sys
r=json.load(sys.stdin).get("result") or []
print(r[0]["id"] if r else "")')
  [ -n "$id" ] || die "zone $zone not found"
  say "zone $zone ($id)"
  say "  the zone itself cannot be deleted while Cloudflare Registrar holds the"
  say "  domain (error 1315). Stripping DNS so it resolves nowhere instead."
  mkdir -p "$ROOT/.cloudflare-zone-backups"
  cf_request GET "/zones/$id/dns_records?per_page=200" \
    > "$ROOT/.cloudflare-zone-backups/$zone.records.json"
  chmod 600 "$ROOT/.cloudflare-zone-backups/$zone.records.json"
  local recs removed=0
  recs=$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for r in d.get("result",[]): print(r["id"])' "$ROOT/.cloudflare-zone-backups/$zone.records.json")
  for r in $recs; do
    cf_request DELETE "/zones/$id/dns_records/$r" | grep -q '"success":true' && removed=$((removed+1))
  done
  ok "  removed $removed DNS record(s); records saved to .cloudflare-zone-backups/"
}

case "${1:-}" in
  pages) [ -n "${2:-}" ] || die "usage: cloudflare-purge.sh pages <project>"; purge_pages "$2" ;;
  r2)    [ -n "${2:-}" ] || die "usage: cloudflare-purge.sh r2 <bucket>";     purge_r2 "$2" ;;
  dns)   [ -n "${2:-}" ] || die "usage: cloudflare-purge.sh dns <zone>";      purge_dns "$2" ;;
  *) die "usage: cloudflare-purge.sh [pages|r2|dns] <name>" ;;
esac
