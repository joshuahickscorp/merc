#!/usr/bin/env bash
# Prove the isolated-test template is a byte-identical schema clone of a fresh
# apply-twice, and that a stale merc_schema_* name cannot be used.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

: "${MERC_TEST_DATABASE_URL:?test-schema-template needs MERC_TEST_DATABASE_URL}"

admin_url="$MERC_TEST_DATABASE_URL"
template_name="$(bash ops/scripts/ensure-schema-template.sh)"
echo "test-schema-template: template=$template_name" >&2

rewrite_url() {
  python3 - "$admin_url" "$1" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit
source = urlsplit(sys.argv[1])
print(urlunsplit((source.scheme, source.netloc, "/" + sys.argv[2],
                  source.query, source.fragment)))
PY
}

scratch="$(mktemp -d "${TMPDIR:-/tmp}/merc-schema-template-proof.XXXXXX")"
fresh_name="merc_tmpl_fresh_$$"
clone_name="merc_tmpl_clone_$$"
stale_name="merc_schema_0000000000000000"

cleanup() {
  dropdb --if-exists --force --maintenance-db="$admin_url" "$fresh_name" >/dev/null 2>&1 || true
  dropdb --if-exists --force --maintenance-db="$admin_url" "$clone_name" >/dev/null 2>&1 || true
  dropdb --if-exists --force --maintenance-db="$admin_url" "$stale_name" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT

createdb --maintenance-db="$admin_url" "$fresh_name"
fresh_url="$(rewrite_url "$fresh_name")"
psql "$fresh_url" -X -q -v ON_ERROR_STOP=1 -f "$ROOT/src/control/schema.sql" >/dev/null
psql "$fresh_url" -X -q -v ON_ERROR_STOP=1 -f "$ROOT/src/control/schema.sql" >/dev/null

# Clone must not require a session on the template (ALLOW_CONNECTIONS is false).
createdb --maintenance-db="$admin_url" --template="$template_name" "$clone_name"
clone_url="$(rewrite_url "$clone_name")"

dump_schema() {
  local url="$1" out="$2"
  pg_dump --schema-only --no-owner --no-privileges --no-publications --no-subscriptions \
    "$url" >"${out}.raw"
  python3 - "$out" <<'PY'
import re, sys
out = sys.argv[1]
text = open(out + ".raw", encoding="utf-8").read()
# Database name, dump clock, and version banner are not schema.
text = re.sub(r'^--.*\n', '', text, flags=re.M)
text = re.sub(r'^\\restrict .*\n', '', text, flags=re.M)
text = re.sub(r'^\\unrestrict .*\n', '', text, flags=re.M)
text = re.sub(r'^\\connect .*\n', '', text, flags=re.M)
open(out, 'w', encoding='utf-8').write(text)
PY
}

dump_schema "$fresh_url" "$scratch/fresh.sql"
dump_schema "$clone_url" "$scratch/clone.sql"
if ! diff -u "$scratch/fresh.sql" "$scratch/clone.sql" >"$scratch/schema.diff"; then
  echo "test-schema-template: FAIL clone schema != fresh apply-twice" >&2
  cat "$scratch/schema.diff" >&2
  exit 1
fi

# Stale-name guard: a merc_schema_* name whose sha is not today's schema.sql
# must be refused before any clone happens.
if ! createdb --maintenance-db="$admin_url" "$stale_name" >/dev/null; then
  echo "test-schema-template: could not create stale-name probe database" >&2
  exit 1
fi
set +e
stale_out="$(
  cd "$ROOT/src/control" &&
  MERC_TEST_DATABASE_URL="$admin_url" \
    MERC_ISOLATED_TEST_DB_TEMPLATE="$stale_name" \
    go test -count=1 -timeout 60s -run '^TestQuoteWithoutHoneypotReportsVerificationUnavailableBeforePricing$' . 2>&1
)"
stale_status=$?
set -e
if [ "$stale_status" -eq 0 ]; then
  echo "test-schema-template: FAIL stale template was accepted" >&2
  printf '%s\n' "$stale_out" >&2
  exit 1
fi
case "$stale_out" in
  *stale*) ;;
  *)
    echo "test-schema-template: FAIL stale refusal did not name staleness" >&2
    printf '%s\n' "$stale_out" >&2
    exit 1
    ;;
esac

echo "test-schema-template: PASS clone schema == apply-twice; stale template refused"
