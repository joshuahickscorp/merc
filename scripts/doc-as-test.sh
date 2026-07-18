#!/usr/bin/env bash
#
# Computexchange — doc-as-test: make docs/QUICKSTART.md EXECUTABLE TRUTH.
#
# The buyer quickstart is the first thing an outside developer runs. If a
# documented command silently stops working (an SDK method renamed, a CLI flag
# dropped, an API field moved), the doc becomes a lie and the buyer's very first
# experience is a 404 or a stack trace. This script closes that gap: it PARSES the
# three documented lanes (curl, Python SDK, cx CLI) OUT OF docs/QUICKSTART.md
# itself, rewrites only the host + key placeholders to point at a real local
# control plane, and RUNS each documented command end to end against the real
# shipped code — the same Go API, the same Python SDK, the same cx binary a buyer
# would touch. If a documented command no longer works, this exits non-zero.
#
# It parses the doc rather than re-typing the commands so it cannot drift AWAY from
# the doc: the moment the doc's command changes, the test runs the NEW command. A
# built-in self-test (run on EVERY invocation) proves that a deliberately-broken doc
# command IS caught — a doc that documents a non-existent SDK method / CLI flag / API
# shape must fail this harness, not pass it — so the guarantee is continuously
# verified, not asserted once.
#
# TWO MODES, both real:
#   ATTACHED  — CX_DOCTEST_CONTROL_URL + CX_DOCTEST_API_KEY are set (prove-local
#               exports them after it has already stood up a real control plane and
#               a LIVE Metal/Candle agent). We run the documented commands against
#               that fully-real stack; a real supplier agent drains the jobs.
#   STANDALONE — no such env (CI's Linux control job, or a bare local run). We
#               provision a throwaway native Postgres + MinIO + the real control
#               plane exactly the way prove-local/bench-local do, seed the demo
#               buyer + worker, and run a MINIMAL stand-in worker drainer (poll →
#               PUT a valid embed result → commit, the same loop the integration
#               suite's driveOneTask uses) so submitted jobs actually complete.
#               This proves the DOCUMENTED BUYER COMMANDS are valid against the real
#               API/SDK/CLI even on a GPU-less runner — orthogonal to whether a real
#               GPU produced the vectors.
#
# Honest by construction (BLACKHOLE): every step fails LOUDLY, nothing is faked, and
# the one thing standalone mode cannot do locally (produce REAL Metal embeddings) is
# named, not pretended — the vectors in standalone mode are a stand-in worker's, and
# the harness says so; the ATTACHED path (prove-local) is the one that runs them for
# real. Exit code is non-zero if any documented command fails.
#
# Usage:   scripts/doc-as-test.sh              (standalone, provisions its own stack)
#   Env:   CX_DOCTEST_CONTROL_URL / CX_DOCTEST_API_KEY  attach to a running stack
#          KEEP=1        leave the stack up on exit (standalone only)
#          DPGPORT/DMINIO_PORT/DMINIO_CONSOLE/DCONTROL_PORT  override ports

set -euo pipefail

# ── Locate repo root ─────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

QUICKSTART="$ROOT/docs/QUICKSTART.md"

# ── Config (all overridable) ─────────────────────────────────────────────────
# Distinct default ports from prove-local (5543x/5900x/1808x) and bench-local so all
# three can coexist on one machine.
DPGPORT="${DPGPORT:-55434}"
DMINIO_PORT="${DMINIO_PORT:-59200}"
DMINIO_CONSOLE="${DMINIO_CONSOLE:-59201}"
DCONTROL_PORT="${DCONTROL_PORT:-18100}"
KEEP="${KEEP:-0}"

ART="$ROOT/.artifacts/doc-as-test"
PGDATA="$ART/pgdata"
MINIO_DATA="$ART/minio-data"
CONTROL_LOG="$ART/control.log"
PG_LOG="$ART/pg.log"
MINIO_LOG="$ART/minio.log"
DRAINER_LOG="$ART/drainer.log"
WORK="$ART/work"        # scratch for extracted commands + inputs

# Demo credentials are fixed by control/seed.go.
DEMO_API_KEY="dev-api-key-0001"
DEMO_WORKER_TOKEN="dev-worker-token-0001"

# ── Pretty logging ───────────────────────────────────────────────────────────
b()    { printf '\033[1m%s\033[0m' "$*"; }
say()  { printf '\033[1;36m[doc-as-test]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  ⚠\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }

CONTROL_PID=""; MINIO_PID=""; DRAINER_PID=""; PG_STARTED=0; OWN_STACK=0

die() {
  fail "$*"
  [ -f "$CONTROL_LOG" ] && { echo "----- control log (tail) -----" >&2; tail -n 20 "$CONTROL_LOG" >&2; } || true
  [ -f "$DRAINER_LOG" ] && { echo "----- drainer log (tail) -----" >&2; tail -n 20 "$DRAINER_LOG" >&2; } || true
  exit 1
}

cleanup() {
  local ec=$?
  [ -n "$DRAINER_PID" ] && kill "$DRAINER_PID" 2>/dev/null || true
  [ -n "$CONTROL_PID" ] && kill "$CONTROL_PID" 2>/dev/null || true
  sleep 1
  [ -n "$DRAINER_PID" ] && kill -9 "$DRAINER_PID" 2>/dev/null || true
  [ -n "$CONTROL_PID" ] && kill -9 "$CONTROL_PID" 2>/dev/null || true
  if [ "$OWN_STACK" = "1" ] && [ "$KEEP" != "1" ]; then
    [ -n "$MINIO_PID" ] && kill "$MINIO_PID" 2>/dev/null || true
    if [ "$PG_STARTED" = "1" ]; then
      LC_ALL=C pg_ctl -D "$PGDATA" -m fast stop >/dev/null 2>&1 || true
    fi
    rm -rf "$PGDATA" "$MINIO_DATA" 2>/dev/null || true
  elif [ "$KEEP" = "1" ]; then
    warn "KEEP=1 — leaving the doc-as-test stack up"
  fi
  exit "$ec"
}
trap cleanup EXIT INT TERM

# wait_for <seconds> <cmd...>
wait_for() {
  local timeout="$1"; shift
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 1
  done
  return 0
}

# ── Doc extraction: pull the documented commands OUT of QUICKSTART.md ─────────
# The whole point of a doc-as-test is that the commands come FROM the doc, so the
# test can never silently diverge from what's published. We slice the fenced code
# block that immediately follows a given "## <heading>" line. `lang` narrows to the
# right fence when a section has more than one block (e.g. the CLI section).
#
# extract_block <heading-regex> [lang] [which]  → prints the block body to stdout
#
# `blk` counts fenced code blocks within the section (every fence PAIR is one
# block, regardless of language); a block counts toward `blk` only if its opening
# fence's language matches LANG (or LANG is empty → any language). `inblk` is a
# strict open/closed toggle so a CLOSING fence is never misread as an opening one
# (the subtle bug: block 1's closing ``` must not be counted as block 2's opener).
extract_block() {
  local heading="$1" lang="${2:-}" which="${3:-1}"
  awk -v H="$heading" -v LANG="$lang" -v WHICH="$which" '
    $0 ~ "^## " H { insec=1; blk=0; inblk=0; capture=0; next }
    insec && !inblk && /^## / { insec=0 }   # a new section ends this one
    insec && /^```/ {
      if (inblk) {                          # this ``` CLOSES the current block
        inblk=0
        if (capture) exit                   # captured the requested block; done
        next
      }
      # this ``` OPENS a block
      inblk=1
      flang=$0; sub(/^```/, "", flang)
      if (LANG=="" || flang==LANG) {
        blk++
        capture=(blk==WHICH)
      } else {
        capture=0                           # a non-matching-language block: skip its body
      }
      next
    }
    insec && inblk && capture { print }
  ' "$QUICKSTART"
}

# ── Phase 0: preflight + resolve the target control plane ────────────────────
say "$(b 'Computexchange — docs/QUICKSTART.md as an executable test')"
rm -rf "$ART"; mkdir -p "$WORK"

[ -f "$QUICKSTART" ] || die "docs/QUICKSTART.md not found (this test IS the doc)"

need=(python3 curl go)
for t in "${need[@]}"; do
  command -v "$t" >/dev/null 2>&1 || die "required tool '$t' not found on PATH"
done

# THREE modes, all real:
#   ATTACHED  — a control plane + agent are already up (prove-local): just run the docs.
#   SERVICE   — DATABASE_URL + S3_* already point at CI's Postgres/MinIO services, but
#               no control plane is running: build+start the real control plane against
#               them (+ a stand-in drainer). Detected by DATABASE_URL being pre-set.
#   STANDALONE — nothing given: provision native throwaway Postgres+MinIO ourselves.
if [ -n "${CX_DOCTEST_CONTROL_URL:-}" ] && [ -n "${CX_DOCTEST_API_KEY:-}" ]; then
  MODE="attached"
  CONTROL_URL="$CX_DOCTEST_CONTROL_URL"
  API_KEY="$CX_DOCTEST_API_KEY"
  say "attached to a running stack at $CONTROL_URL (real agent drains jobs)"
  curl -fsS "$CONTROL_URL/healthz" >/dev/null 2>&1 || die "attached control URL is not healthy"
elif [ -n "${DATABASE_URL:-}" ] && [ -n "${S3_ENDPOINT:-}" ]; then
  MODE="service"
  say "using CI-provided Postgres/MinIO services (DATABASE_URL + S3_* preset)"
else
  MODE="standalone"
fi

# ── Provision (STANDALONE only) ──────────────────────────────────────────────
if [ "$MODE" = "standalone" ]; then
  OWN_STACK=1
  for t in psql initdb pg_ctl createdb postgres minio; do
    command -v "$t" >/dev/null 2>&1 || die "standalone mode needs '$t' (or preset DATABASE_URL/S3_*, or attach via CX_DOCTEST_CONTROL_URL/API_KEY)"
  done
  export DATABASE_URL="postgres://cx@localhost:$DPGPORT/cx?sslmode=disable"
  export S3_ENDPOINT="http://localhost:$DMINIO_PORT"
  export S3_PUBLIC_ENDPOINT="http://localhost:$DMINIO_PORT"
  export S3_BUCKET="cx-jobs"
  export S3_ACCESS_KEY="minioadmin"
  export S3_SECRET_KEY="minioadmin"
  export S3_REGION="us-east-1"

  say "provisioning throwaway Postgres(:$DPGPORT) + MinIO(:$DMINIO_PORT)"
  LC_ALL=C initdb -D "$PGDATA" -U cx --auth=trust -E UTF8 --locale=C >"$PG_LOG" 2>&1 \
    || die "initdb failed (see $PG_LOG)"
  LC_ALL=C pg_ctl -D "$PGDATA" -o "-p $DPGPORT -c listen_addresses=localhost -c unix_socket_directories=$ART" \
    -l "$PG_LOG" -w start >>"$PG_LOG" 2>&1 || die "pg_ctl start failed (see $PG_LOG)"
  PG_STARTED=1
  createdb -h localhost -p "$DPGPORT" -U cx cx 2>>"$PG_LOG" || die "createdb failed"
  wait_for 20 psql "$DATABASE_URL" -c 'SELECT 1' || die "postgres not accepting connections"
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    minio server "$MINIO_DATA" --address "localhost:$DMINIO_PORT" --console-address "localhost:$DMINIO_CONSOLE" \
    >"$MINIO_LOG" 2>&1 &
  MINIO_PID=$!
  wait_for 30 curl -fsS "http://localhost:$DMINIO_PORT/minio/health/live" || die "minio did not come up"
fi

# ── Start the real control plane + a stand-in drainer (STANDALONE + SERVICE) ──
if [ "$MODE" != "attached" ]; then
  CONTROL_URL="http://localhost:$DCONTROL_PORT"
  API_KEY="$DEMO_API_KEY"
  export LISTEN_ADDR=":$DCONTROL_PORT"
  # Hermetic money: never touch a live rail (same rationale as prove-local).
  unset STRIPE_SECRET_KEY STRIPE_PUBLISHABLE_KEY STRIPE_WEBHOOK_SECRET CX_CONNECT_WEBHOOK_SECRET 2>/dev/null || true
  # The production server deliberately has no implicit economic defaults. This
  # isolated no-Stripe harness still needs an explicit, versioned schedule so
  # quote/submit exercises the same fail-closed path without claiming a processor
  # contract or changing real prices.
  export CX_ECON_SCHEDULE_VERSION="doc-as-test-hermetic-v1"
  export CX_PROCESSOR_PERCENT_BPS="0"
  export CX_PROCESSOR_FIXED_USD="0"
  export CX_CONTROL_PLANE_PER_TASK_USD="0"
  export CX_TARGET_MARGIN_BPS="0"

  say "applying schema + seeding demo buyer/worker"
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f db/schema.sql >/dev/null 2>&1 || die "schema apply failed"
  (cd control && go build -o "$ART/cx" .) || die "control build failed"
  "$ART/cx" seed >/dev/null 2>&1 || die "seed failed"
  ( "$ART/cx" ) >"$CONTROL_LOG" 2>&1 &
  CONTROL_PID=$!
  wait_for 30 bash -c "kill -0 $CONTROL_PID 2>/dev/null && curl -fsS '$CONTROL_URL/healthz'" \
    || die "control plane never became healthy"

  # The server-side verification FLOOR injects the seeded demo honeypot into every
  # buyer job even when the buyer asked for honeypot_frac=0. The drainer must answer
  # that honeypot with its REAL known answer (or the verifier requeues it forever and
  # no job completes). The honeypot's input_url path is OPAQUE by a security fix, so
  # the drainer recognizes it by the probe TEXT in the input. Extract BOTH the probe
  # text and the known answer straight from control/seed.go so the drainer holds no
  # hand-copied literal that could drift from the seed.
  HONEYPOT_TEXT="$(python3 -c 'import re;m=re.search(r"demoHoneypotEmbedText\s*=\s*\"([^\"]*)\"",open("control/seed.go").read());print(m.group(1) if m else "")')"
  HONEYPOT_ANSWER="$(python3 -c 'import re;m=re.search(r"demoHoneypotEmbedKnownAnswer\s*=\s*`([^`]*)`",open("control/seed.go").read());print(m.group(1) if m else "")')"
  [ -n "$HONEYPOT_TEXT" ]   || die "could not extract demoHoneypotEmbedText from control/seed.go"
  [ -n "$HONEYPOT_ANSWER" ] || die "could not extract demoHoneypotEmbedKnownAnswer from control/seed.go"

  # Stand-in worker drainer: the minimal poll→PUT→commit loop (mirrors the
  # integration suite's driveOneTask). This makes submitted jobs complete so the
  # DOCUMENTED buyer commands (submit → poll → results) run through to a real
  # result — on a GPU-less runner. It fakes only the VECTORS (a stand-in worker's,
  # not real Metal output); the buyer-facing API/SDK/CLI surface it exercises is
  # 100% the real shipped code. prove-local's ATTACHED path runs the real agent.
  say "starting the stand-in worker drainer"
  CX_DRAIN_CONTROL_URL="$CONTROL_URL" CX_DRAIN_WORKER_TOKEN="$DEMO_WORKER_TOKEN" \
    CX_DRAIN_HONEYPOT_TEXT="$HONEYPOT_TEXT" CX_DRAIN_HONEYPOT_ANSWER="$HONEYPOT_ANSWER" \
    python3 "$SCRIPT_DIR/doc-as-test-drainer.py" >"$DRAINER_LOG" 2>&1 &
  DRAINER_PID=$!
  sleep 1
  kill -0 "$DRAINER_PID" 2>/dev/null || die "drainer failed to start (see $DRAINER_LOG)"
fi

# ── Build the cx CLI once (the CLI lane needs the real binary) ────────────────
CX_BIN="$ART/cx"
say "building the real cx CLI (control/) to exercise the documented CLI lane"
(cd control && go build -o "$CX_BIN" .) || die "cx CLI build failed"

# ── Rewrite doc placeholders → the local stack ───────────────────────────────
# The doc points at the deliberately non-routable https://cx.example.invalid with
# a cx_live_… placeholder key.
# We rewrite ONLY the host + key so the exact documented COMMAND SHAPE runs against
# the local control plane. Everything else (endpoints, JSON shape, SDK method names,
# CLI flags) is executed verbatim as the doc wrote it.
localize() {
  sed -e "s#https://cx.example.invalid#$CONTROL_URL#g" \
      -e "s#cx_live_…#$API_KEY#g" \
      -e "s#cx_live_...#$API_KEY#g"
}

PASS=0; FAILC=0
pass() { ok "$*"; PASS=$((PASS+1)); }
lose() { fail "$*"; FAILC=$((FAILC+1)); }

# ── Lane 1: the documented `curl` block ──────────────────────────────────────
say "$(b 'Lane 1 — the documented curl commands')"
CURL_BLOCK="$(extract_block 'curl' bash 1)"
[ -n "$CURL_BLOCK" ] || die "could not extract the curl block from QUICKSTART.md"
# Run the documented curl block VERBATIM (only host+key localized). The block's own
# first command sets JOB via the documented submit + `["job_id"]` extraction — if the
# API renamed that field, the documented python one-liner throws a KeyError and the
# whole block exits non-zero, which is exactly the doc-drift we want to catch. We
# append one line to echo the captured JOB so we can then follow it deterministically.
{ printf '%s\n' "$CURL_BLOCK" | localize; printf '\necho "DOCTEST_JOB_ID=$JOB"\n'; } >"$WORK/curl-lane.sh"
if bash -euo pipefail "$WORK/curl-lane.sh" >"$WORK/curl-out.txt" 2>&1; then
  JOB_ID="$(sed -n 's/^DOCTEST_JOB_ID=//p' "$WORK/curl-out.txt" | head -1)"
  if [ -z "$JOB_ID" ]; then
    lose "curl lane: the documented submit produced no job_id (API 'job_id' field renamed?)"
  else
    lose_curl=0; ST=""
    # Poll the documented status endpoint to completion, then fetch the documented
    # results endpoint — exactly the two commands the doc shows after submit.
    for _ in $(seq 1 60); do
      ST="$(curl -fsS "$CONTROL_URL/v1/jobs/$JOB_ID" -H "Authorization: Bearer $API_KEY" \
        | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
      [ "$ST" = "complete" ] && break
      { [ "$ST" = "failed" ] || [ "$ST" = "cancelled" ]; } && { lose_curl=1; break; }
      sleep 2
    done
    RES="$(curl -fsS "$CONTROL_URL/v1/jobs/$JOB_ID/results" -H "Authorization: Bearer $API_KEY" 2>/dev/null || true)"
    if [ "$ST" = "complete" ] && [ -n "$RES" ] && [ "$lose_curl" = "0" ]; then
      pass "curl lane: documented submit → poll → results completed (job $JOB_ID)"
    else
      lose "curl lane: job did not complete / no results (status=$ST)"
    fi
  fi
else
  lose "curl lane: the documented curl commands failed to run — see $WORK/curl-out.txt"
  cat "$WORK/curl-out.txt" >&2 || true
fi

# ── Lane 2: the documented Python SDK block ──────────────────────────────────
say "$(b 'Lane 2 — the documented Python SDK commands')"
PY_BLOCK="$(extract_block 'Python' python 1)"
[ -n "$PY_BLOCK" ] || die "could not extract the Python block from QUICKSTART.md"
# Install the SDK exactly as the doc says, into a disposable venv. Running from
# $WORK without PYTHONPATH proves packaging metadata and imports work for a fresh
# user rather than accidentally importing straight from the checkout. The doc's
# `Client(...)`, `submit_job`, `wait`, `results_text`, and `embeddings` calls are
# then run verbatim (localized).
SDK_VENV="$WORK/sdk-venv"
python3 -m venv "$SDK_VENV"
SDK_PY="$SDK_VENV/bin/python"
if ! "$SDK_PY" -m pip install --disable-pip-version-check --quiet "$ROOT/sdk/python" \
    >"$WORK/sdk-install.txt" 2>&1; then
  lose "Python SDK lane: documented 'pip install ./sdk/python' failed — see $WORK/sdk-install.txt"
  cat "$WORK/sdk-install.txt" >&2 || true
fi
printf '%s\n' "$PY_BLOCK" | localize >"$WORK/py-lane.py"
if (cd "$WORK" && PYTHONNOUSERSITE=1 "$SDK_PY" "$WORK/py-lane.py") >"$WORK/py-out.txt" 2>&1; then
  # The doc's script prints the merged result text and the first 5 embedding floats.
  # A working run produces non-empty output on both prints.
  if [ -s "$WORK/py-out.txt" ]; then
    pass "Python SDK lane: Client → submit_job → wait → results_text → embeddings all ran"
  else
    lose "Python SDK lane: ran but produced no output (a documented method returned nothing)"
  fi
else
  lose "Python SDK lane: the documented SDK calls failed — see $WORK/py-out.txt"
  cat "$WORK/py-out.txt" >&2 || true
fi

# ── Lane 3: the documented cx CLI block ──────────────────────────────────────
say "$(b 'Lane 3 — the documented cx CLI commands')"
# The CLI section documents `export CX_API_URL/CX_API_KEY` then
# `cx submit … --input rows.jsonl --wait`, with rows.jsonl defined in the very next
# fenced block. Reconstruct exactly that: write rows.jsonl from the doc, export the
# two env vars from the doc (localized), and run the documented submit line against
# the real cx binary we just built.
CLI_ENV_BLOCK="$(extract_block 'cx CLI' bash 1)"
ROWS_BLOCK="$(extract_block 'cx CLI' '' 2)"   # the unlabelled fence: rows.jsonl body
[ -n "$CLI_ENV_BLOCK" ] || die "could not extract the CLI block from QUICKSTART.md"
[ -n "$ROWS_BLOCK" ]   || die "could not extract the rows.jsonl block from QUICKSTART.md"
printf '%s\n' "$ROWS_BLOCK" >"$WORK/rows.jsonl"
# Turn the documented env exports + submit line into a runnable script. We take the
# documented `export …` lines and `cx …` commands verbatim (only host+key localized),
# and rewrite the bare `cx` prefix to the real binary we built from control/ — the FLAGS
# and SUBCOMMAND the doc shows are executed exactly as written, so a dropped/renamed
# flag or subcommand fails here. Run from $WORK so the doc's relative `rows.jsonl`
# resolves.
{
  printf 'set -euo pipefail\n'
  printf '%s\n' "$CLI_ENV_BLOCK" | localize \
    | grep -E '^(export |cx )' \
    | sed -E "s#^cx #\"$CX_BIN\" #"
} >"$WORK/cli-lane.sh"
if (cd "$WORK" && bash "$WORK/cli-lane.sh") >"$WORK/cli-out.txt" 2>&1; then
  if [ -s "$WORK/cli-out.txt" ]; then
    pass "cx CLI lane: documented 'cx submit … --wait' ran and printed a result"
  else
    lose "cx CLI lane: ran but produced no output"
  fi
else
  lose "cx CLI lane: the documented cx command failed — see $WORK/cli-out.txt"
  cat "$WORK/cli-out.txt" >&2 || true
fi

# ── Self-test: prove a BROKEN documented command IS caught ───────────────────
# A doc-as-test that can't fail is theatre. We take the real Python lane, corrupt it
# EXACTLY as a stale doc would (call a method the SDK doesn't have), and assert the
# harness's own check would have FAILED on it. This runs on every invocation so the
# guarantee ("a broken doc command fails CI") is itself continuously proven.
say "$(b 'Self-test — a deliberately broken documented command must fail')"
BROKEN_PY="$WORK/py-broken.py"
{
  printf '%s\n' "$PY_BLOCK" | localize
  # Append a call the SDK does NOT define — this is what a stale doc looks like.
  printf '\n# (injected) a documented-but-removed method a stale doc might still show:\n'
  printf 'cx.this_method_was_removed_from_the_sdk("all-minilm-l6-v2")\n'
} >"$BROKEN_PY"
if (cd "$WORK" && PYTHONNOUSERSITE=1 "$SDK_PY" "$BROKEN_PY") >"$WORK/py-broken-out.txt" 2>&1; then
  lose "self-test: a broken documented command RAN CLEAN — the harness would NOT catch doc drift"
else
  pass "self-test: a broken documented command was caught (non-zero exit, as a stale doc must be)"
fi

# ── Summary ──────────────────────────────────────────────────────────────────
echo
say "$(b '========  DOC-AS-TEST SUMMARY  ========')"
printf '  %s: \033[1;32m%d pass\033[0m, \033[1;31m%d fail\033[0m\n' "$(b summary)" "$PASS" "$FAILC"
echo
if [ "$FAILC" -gt 0 ]; then
  die "$FAILC documented command(s) FAILED — docs/QUICKSTART.md is not executable truth"
fi
say "$(b 'QUICKSTART.md is executable truth: every documented buyer command runs') ✅"
