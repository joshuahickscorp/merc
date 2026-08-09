#!/usr/bin/env bash
# Assert template selection changes only the isolated clone command.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

scratch="$(mktemp -d "${TMPDIR:-/tmp}/merc-test-db-template.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin"

cat >"$scratch/bin/createdb" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$MERC_TEST_DB_TRACE"
EOF
cat >"$scratch/bin/dropdb" <<'EOF'
#!/usr/bin/env bash
printf 'drop %s\n' "$*" >>"$MERC_TEST_DB_TRACE"
EOF
chmod +x "$scratch/bin/createdb" "$scratch/bin/dropdb"

trace="$scratch/trace"
PATH="$scratch/bin:$PATH" MERC_TEST_DB_TRACE="$trace" \
  MERC_TEST_DATABASE_URL='postgres://cx@127.0.0.1:5432/cx?sslmode=disable' \
  MERC_ISOLATED_TEST_DB_PREFIX=merc_template_probe \
  MERC_ISOLATED_TEST_DB_TEMPLATE=merc_pristine \
  bash scripts/with-isolated-test-db.sh bash -c true
rg --fixed-strings -- '--template=merc_pristine' "$trace" >/dev/null || {
  echo "template database was not used for the disposable clone" >&2
  exit 1
}

: >"$trace"
PATH="$scratch/bin:$PATH" MERC_TEST_DB_TRACE="$trace" \
  MERC_TEST_DATABASE_URL='postgres://cx@127.0.0.1:5432/cx?sslmode=disable' \
  MERC_ISOLATED_TEST_DB_PREFIX=merc_template_probe \
  bash scripts/with-isolated-test-db.sh bash -c true
if rg --fixed-strings -- '--template=' "$trace" >/dev/null; then
  echo "blank isolated database unexpectedly selected a template" >&2
  exit 1
fi

echo "test-with-isolated-test-db: PASS template clones remain opt-in"
