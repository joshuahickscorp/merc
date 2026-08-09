#!/usr/bin/env bash
# Run one command against a disposable database derived from
# MERC_TEST_DATABASE_URL. The base database is only the administrative
# connection point; the child never writes to it.
set -euo pipefail

: "${MERC_TEST_DATABASE_URL:?with-isolated-test-db needs MERC_TEST_DATABASE_URL}"
[ "$#" -gt 0 ] || {
  echo "usage: with-isolated-test-db.sh command [args...]" >&2
  exit 2
}

for command in createdb dropdb python3; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "with-isolated-test-db: missing required command $command" >&2
    exit 2
  }
done

base_database_url="$MERC_TEST_DATABASE_URL"
prefix="${MERC_ISOLATED_TEST_DB_PREFIX:-merc_test_run}"
if ! [[ "$prefix" =~ ^[a-z][a-z0-9_]{2,40}$ ]]; then
  echo "with-isolated-test-db: invalid database prefix" >&2
  exit 2
fi
template_database="${MERC_ISOLATED_TEST_DB_TEMPLATE:-}"
if [ -n "$template_database" ] && ! [[ "$template_database" =~ ^[a-z][a-z0-9_]{2,60}$ ]]; then
  echo "with-isolated-test-db: invalid template database name" >&2
  exit 2
fi

database_name="${prefix}_$(python3 - <<'PY'
import secrets
print(secrets.token_hex(12))
PY
)"

cleanup() {
  # database_name is generated locally from a validated prefix and hex, so this
  # can only remove the database created by this invocation.
  dropdb --if-exists --maintenance-db="$base_database_url" "$database_name" \
    >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

if [ -n "$template_database" ]; then
  # The caller owns the template cluster and must prove it is a schema-only,
  # exact-candidate fixture before a contract uses it. PostgreSQL copies the
  # template atomically, so every mutant still starts with a separate database.
  createdb --maintenance-db="$base_database_url" --template="$template_database" "$database_name"
else
  createdb --maintenance-db="$base_database_url" "$database_name"
fi
test_database_url="$(MERC_SOURCE_DATABASE_URL="$base_database_url" \
  MERC_TARGET_DATABASE_NAME="$database_name" python3 - <<'PY'
import os
from urllib.parse import urlsplit, urlunsplit

source = urlsplit(os.environ["MERC_SOURCE_DATABASE_URL"])
print(urlunsplit((source.scheme, source.netloc,
                  "/" + os.environ["MERC_TARGET_DATABASE_NAME"],
                  source.query, source.fragment)))
PY
)"

MERC_TEST_DATABASE_URL="$test_database_url" "$@"
