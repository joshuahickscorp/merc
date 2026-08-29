#!/usr/bin/env bash
# List, inspect and delete Cloudflare zones.
#
# Deleting a zone is IRREVERSIBLE and takes every DNS record with it. Mail
# routing, verification TXT records, subdomains that nobody remembers -- all of
# it, immediately, with no undo. So this script is built to make the safe path
# easy and the destructive path deliberate:
#
#   - a protected list that cannot be deleted at all, whatever is typed
#   - a dump of every DNS record BEFORE deletion, saved to disk
#   - the zone name must be typed back exactly to confirm
#   - one zone per invocation
#
#   bash ops/scripts/cloudflare-zones.sh list
#   bash ops/scripts/cloudflare-zones.sh show   <zone>
#   bash ops/scripts/cloudflare-zones.sh backup <zone>
#   bash ops/scripts/cloudflare-zones.sh delete <zone>

set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck disable=SC1091
[ -f "$ROOT/.merc-secrets.env" ] && { set -a; . "$ROOT/.merc-secrets.env"; set +a; }
# shellcheck source=ops/scripts/lib/cloudflare-auth.sh
. "$ROOT/ops/scripts/lib/cloudflare-auth.sh"
cf_auth_config >/dev/null 2>&1 \
  || { echo "no Cloudflare credential: set CLOUDFLARE_API_TOKEN, or CLOUDFLARE_EMAIL +" >&2
       echo "CLOUDFLARE_GLOBAL_API_KEY. Run ops/scripts/merc-secrets.sh." >&2; exit 1; }

# Zones this script will never delete, whatever is typed. mercmerc.net is the
# live production site; deleting its zone takes merc off the internet.
PROTECTED="mercmerc.net"

BACKUP_DIR="$ROOT/.cloudflare-zone-backups"
say() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# Token goes via --config on stdin: never argv, never a log.
cf() { cf_request "$1" "$2" "${3:-}"; }

zone_id_for() {
  cf GET "/zones?name=$1" | python3 -c '
import json,sys
d=json.load(sys.stdin)
r=d.get("result") or []
print(r[0]["id"] if r else "")'
}

cmd_list() {
  cf GET "/zones?per_page=50" | python3 -c '
import json,sys
d=json.load(sys.stdin)
if not d.get("success"):
    print("  token cannot list zones:", json.dumps(d.get("errors"))[:200]); raise SystemExit(1)
rows=d.get("result",[])
if not rows: print("  no zones"); raise SystemExit
print("  %-28s %-10s %s" % ("zone","status","id"))
for z in rows:
    print("  %-28s %-10s %s" % (z["name"], z["status"], z["id"]))'
}

cmd_show() {
  local zone="$1" id
  id=$(zone_id_for "$zone"); [ -n "$id" ] || die "zone $zone not found"
  say "  zone $zone ($id)"
  cf GET "/zones/$id/dns_records?per_page=200" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for r in d.get("result",[]):
    print("    %-6s %-40s -> %s" % (r["type"], r["name"], str(r.get("content"))[:50]))'
}

cmd_backup() {
  local zone="$1" id
  id=$(zone_id_for "$zone"); [ -n "$id" ] || die "zone $zone not found"
  mkdir -p "$BACKUP_DIR"
  local out="$BACKUP_DIR/$zone.records.json"
  cf GET "/zones/$id/dns_records?per_page=200" > "$out"
  chmod 600 "$out"
  local n
  n=$(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1])).get("result",[])))' "$out")
  say "  backed up $n DNS record(s) to $out"
}

cmd_delete() {
  local zone="$1"
  for p in $PROTECTED; do
    [ "$zone" = "$p" ] && die "$zone is PROTECTED and will not be deleted by this script.
       It is the live production site; removing its zone takes merc off the
       internet along with every DNS record it holds."
  done
  local id
  id=$(zone_id_for "$zone"); [ -n "$id" ] || die "zone $zone not found"

  # Records are dumped BEFORE the destructive call, so there is something to
  # rebuild from even though the zone itself cannot be undeleted.
  cmd_backup "$zone"
  say ""
  say "  about to DELETE zone $zone ($id)"
  say "  this removes every DNS record above and cannot be undone"
  say ""
  printf '  Type the zone name exactly to confirm: '
  read -r typed
  [ "$typed" = "$zone" ] || die "confirmation did not match; nothing deleted"

  local resp
  resp=$(cf DELETE "/zones/$id")
  if printf '%s' "$resp" | grep -q '"success":true'; then
    say "  deleted $zone"
  else
    die "delete failed: $(printf '%s' "$resp" | head -c 300)"
  fi
}

case "${1:-list}" in
  list)   cmd_list ;;
  show)   [ -n "${2:-}" ] || die "usage: cloudflare-zones.sh show <zone>";   cmd_show "$2" ;;
  backup) [ -n "${2:-}" ] || die "usage: cloudflare-zones.sh backup <zone>"; cmd_backup "$2" ;;
  delete) [ -n "${2:-}" ] || die "usage: cloudflare-zones.sh delete <zone>"; cmd_delete "$2" ;;
  *) die "unknown command ${1:-}" ;;
esac
