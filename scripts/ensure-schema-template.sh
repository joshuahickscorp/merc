#!/usr/bin/env bash
# Build or reuse a schema-stamped PostgreSQL template for isolated tests.
#
# The template is named merc_schema_<first 16 hex chars of sha256(schema.sql)>.
# A schema.sql change produces a new name, so a stale template can never be
# selected. Incomplete builds are dropped; stdout is only the database name.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

: "${MERC_TEST_DATABASE_URL:?ensure-schema-template needs MERC_TEST_DATABASE_URL}"

for command in createdb dropdb psql python3; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "ensure-schema-template: missing required command $command" >&2
    exit 2
  }
done

schema_file="$ROOT/control/schema.sql"
[ -f "$schema_file" ] || {
  echo "ensure-schema-template: $schema_file is missing" >&2
  exit 2
}

sha="$(python3 - "$schema_file" <<'PY'
import hashlib, sys
print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
PY
)"
short="${sha:0:16}"
# ddl_ marks a schema.sql-only template (no catalog/activation rows).
# Isolated tests re-seed those from the process's current authority.
template_name="merc_schema_ddl_${short}"
if ! [[ "$template_name" =~ ^[a-z][a-z0-9_]{2,60}$ ]]; then
  echo "ensure-schema-template: invalid template name $template_name" >&2
  exit 2
fi

admin_url="$MERC_TEST_DATABASE_URL"

db_exists() {
  local name="$1"
  local found
  found="$(psql "$admin_url" -X -qAt -v ON_ERROR_STOP=1 \
    -c "SELECT 1 FROM pg_database WHERE datname = '$name'")"
  [ "$found" = "1" ]
}

rewrite_url() {
  python3 - "$admin_url" "$1" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit
source = urlsplit(sys.argv[1])
print(urlunsplit((source.scheme, source.netloc, "/" + sys.argv[2],
                  source.query, source.fragment)))
PY
}

drop_stale_schema_templates() {
  local old
  while IFS= read -r old; do
    [ -n "$old" ] || continue
    [ "$old" = "$template_name" ] && continue
    echo "ensure-schema-template: dropping stale template $old" >&2
    psql "$admin_url" -X -q -v ON_ERROR_STOP=1 \
      -c "UPDATE pg_database SET datistemplate = false WHERE datname = '$old'" \
      >/dev/null 2>&1 || true
    dropdb --if-exists --force --maintenance-db="$admin_url" "$old" \
      >/dev/null 2>&1 || true
  done < <(psql "$admin_url" -X -qAt -v ON_ERROR_STOP=1 \
    -c "SELECT datname FROM pg_database WHERE datname LIKE 'merc_schema_%' AND datname <> '$template_name'")
}

if db_exists "$template_name"; then
  drop_stale_schema_templates
  printf '%s\n' "$template_name"
  exit 0
fi

echo "ensure-schema-template: building $template_name from control/schema.sql ($short)" >&2

if ! createdb --maintenance-db="$admin_url" "$template_name" >/dev/null; then
  # Another process won the create race. Wait for a complete template.
  for _ in $(seq 1 120); do
    if db_exists "$template_name"; then
      drop_stale_schema_templates
      printf '%s\n' "$template_name"
      exit 0
    fi
    sleep 0.25
  done
  echo "ensure-schema-template: timed out waiting for $template_name" >&2
  exit 1
fi

template_url="$(rewrite_url "$template_name")"
incomplete=1
cleanup_incomplete() {
  if [ "$incomplete" -eq 1 ]; then
    echo "ensure-schema-template: dropping incomplete $template_name" >&2
    dropdb --if-exists --force --maintenance-db="$admin_url" "$template_name" \
      >/dev/null 2>&1 || true
  fi
}
trap cleanup_incomplete EXIT

# Schema only. Catalog/profile/activation rows are process-authority-dependent
# and must be seeded by each isolated test after the clone.
apply_log="$(mktemp "${TMPDIR:-/tmp}/merc-schema-apply.XXXXXX")"
if ! psql "$template_url" -X -q -v ON_ERROR_STOP=1 -f "$schema_file" \
      >"$apply_log" 2>&1; then
  cat "$apply_log" >&2
  rm -f "$apply_log"
  exit 1
fi
if ! psql "$template_url" -X -q -v ON_ERROR_STOP=1 -f "$schema_file" \
      >"$apply_log" 2>&1; then
  cat "$apply_log" >&2
  rm -f "$apply_log"
  exit 1
fi
rm -f "$apply_log"

# No sessions may remain on the template or CREATE DATABASE TEMPLATE fails.
psql "$admin_url" -X -q -v ON_ERROR_STOP=1 <<SQL
SELECT pg_terminate_backend(pid)
  FROM pg_stat_activity
 WHERE datname = '$template_name'
   AND pid <> pg_backend_pid();
ALTER DATABASE $template_name IS_TEMPLATE true;
ALTER DATABASE $template_name ALLOW_CONNECTIONS false;
SQL

drop_stale_schema_templates
incomplete=0
trap - EXIT
printf '%s\n' "$template_name"
