#!/usr/bin/env bash
# Run the fast control suite and the agent-subprocess integration tier.
#
# Tiers:
#   fast         default `go test` (no build tag). Isolated DBs clone the
#                schema template; safe isolated tests call t.Parallel().
#   integration  `go test -tags=integration` of the real merc-agent loop
#                (TestFirstCompleteLoopThroughThePublicAPI).
#
# `make ci` runs both, concurrently, so the agent process overlaps the serial
# DB tail. Combined JSON is written to the path in $1 (default .ci-test.json)
# so assert-no-test-skips.sh still sees every test. Neither tier is dropped.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

: "${MERC_TEST_DATABASE_URL:?run-ci-control-tests needs MERC_TEST_DATABASE_URL}"

out="${1:-$ROOT/.ci-test.json}"
int_out="${out}.integration"
fast_err="${out}.fast.err"
int_err="${int_out}.err"

template="$(bash scripts/ensure-schema-template.sh)"
export MERC_ISOLATED_TEST_DB_TEMPLATE="$template"
echo "run-ci-control-tests: template=$template" >&2

# Integration regex is the agent-subprocess public-API loop. There is no
# TestProjectCompilerCADExecutionThroughPublicAPI in this tree; the CAD
# compiler tests are refusal-at-quote and stay in the fast suite.
integration_run='^TestFirstCompleteLoopThroughThePublicAPI$'

run_fast() {
  (
    cd "$ROOT/control"
    bash ../scripts/with-isolated-test-storage.sh \
      bash ../scripts/with-isolated-test-db.sh \
      go test -timeout 45m -parallel 16 -json ./...
  )
}

run_integration() {
  (
    cd "$ROOT/control"
    bash ../scripts/with-isolated-test-storage.sh \
      bash ../scripts/with-isolated-test-db.sh \
      go test -timeout 45m -tags=integration -parallel 16 -json \
        -run "$integration_run" ./...
  )
}

run_integration >"$int_out" 2>"$int_err" &
int_pid=$!
set +e
run_fast >"$out.fast" 2>"$fast_err"
fast_status=$?
wait "$int_pid"
int_status=$?
set -e

# Surface wrapper stderr (docker/minio) without polluting the JSON record.
if [ -s "$fast_err" ]; then
  cat "$fast_err" >&2
fi
if [ -s "$int_err" ]; then
  cat "$int_err" >&2
fi

# One record for summarize-go-test-json.py and assert-no-test-skips.sh.
cat "$out.fast" "$int_out" >"$out"

if [ "$fast_status" -ne 0 ] || [ "$int_status" -ne 0 ]; then
  echo "run-ci-control-tests: FAIL fast=$fast_status integration=$int_status" >&2
  if [ "$fast_status" -ne 0 ]; then
    exit "$fast_status"
  fi
  exit "$int_status"
fi
echo "run-ci-control-tests: PASS fast+integration (template=$template)" >&2
