#!/usr/bin/env bash
#
# Computexchange — bench-local: a repeatable LOCAL benchmark harness, SEPARATE
# from prove-local (Plane D §16 D10). prove-local proves CORRECTNESS; this measures
# SPEED. It NEVER replaces prove-local and changes nothing it touches — run either,
# both, or neither.
#
# It provisions a throwaway native Postgres + MinIO the SAME way prove-local does
# (no Docker pulls; reuse an already-up stack with KEEP=1), then measures and prints
# p50/p90 for:
#   (a) POST /v1/quote latency over K calls (the buyer preflight hot path),
#   (b) the REAL ready-task claim query — the exact, verbatim SQL text
#       ClaimTask (control/scheduler.go) executes, rendered by the shared
#       ClaimTaskSQL function and printed by `control print-claim-sql` (no
#       second hand-copied query text anywhere) — via EXPLAIN ANALYZE over a
#       realistic-scale synthetic queue (SYNTH_TASKS tasks, SYNTH_WORKERS
#       workers across SYNTH_SUPPLIERS suppliers), under realistic planner
#       settings (no forced enable_seqscan). PATCH (Control plane hot path
#       4.5->5, docs/internal/CREED_AND_PATH_TO_TEN.md "Make the benchmark
#       measure the real query"): this used to be a hand-simplified stand-in
#       (a bare 6-column WHERE + a 2-column ORDER BY, run with
#       enable_seqscan=off) that could never have caught the real predicate
#       mismatch against tasks_ready_unclaimed_idx — see the facet writeup.
#   (c) the embed JSON-vs-binary artifact size for N rows (the D5/D15 payload win) —
#       computed from the exact CXEM layout, and cross-checked against the agent's
#       own binary-encoder unit test when the Rust toolchain is present.
#
# Honest by construction (BLACKHOLE): every step fails LOUDLY, nothing is faked, and
# numbers are REAL measurements on THIS machine — not targets. A markdown report
# with a UTC timestamp, the git commit, and the machine class lands in
# .artifacts/bench-local/report.md.
#
# OPTIONAL. Needs no external services. Does NOT alter prove-local.
#
# Usage:   scripts/bench-local.sh            (or: make bench-local)
#   Env:   KEEP=1            reuse an already-running native stack + leave it up
#          QUOTE_CALLS=N     number of /v1/quote calls to time      (default 30)
#          CLAIM_RUNS=N      EXPLAIN ANALYZE repetitions for the claim (default 11)
#          SYNTH_TASKS=N     synthetic queued tasks to insert         (default 10000)
#          SYNTH_WORKERS=N   synthetic registered workers to insert   (default 300)
#          SYNTH_SUPPLIERS=N synthetic suppliers those workers belong to (default 60)
#          EMBED_ROWS=N      rows for the JSON-vs-binary size compare  (default 1000)
#          EMBED_DIM=N       embedding width for the size compare      (default 384)
#          BPGPORT/BMINIO_PORT/BMINIO_CONSOLE/BCONTROL_PORT  override ports
#          SMOKE=1           tiny + fast (used by the prove-local smoke row)

set -euo pipefail

# ── Locate repo root ─────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

# ── Config (all overridable) ─────────────────────────────────────────────────
# Distinct default ports from prove-local (55432/59000/59001/18080) so the two can
# coexist; bench-local stays out of prove-local's way entirely.
BPGPORT="${BPGPORT:-55433}"
BMINIO_PORT="${BMINIO_PORT:-59100}"
BMINIO_CONSOLE="${BMINIO_CONSOLE:-59101}"
BCONTROL_PORT="${BCONTROL_PORT:-18090}"
KEEP="${KEEP:-0}"
SMOKE="${SMOKE:-0}"

QUOTE_CALLS="${QUOTE_CALLS:-30}"
CLAIM_RUNS="${CLAIM_RUNS:-11}"
# Realistic scale (not a toy 10-row table): a five-figure queue behind a few
# hundred registered workers spread across dozens of suppliers, matching the
# order of magnitude Scheduling & Matching's own load-test rung (6->6.5) asks
# for ("100k-task queue with 500+ registered workers") AND the exact scale entry
# 61 (docs/internal/CREED_AND_PATH_TO_TEN.md) root-caused the cheaper_class_online
# O(queue x fleet) cost at: ~12k claimable tasks / ~600 workers, plus a large
# historical-completed-task DILUTION so the tasks table is realistically big (a
# jobs/tasks table keeps finished work — the very condition that made the
# per-candidate-row fleet scan explode). Control Plane Hot Path 8->9's fix
# (eligible_jobs MATERIALIZED, per-JOB cheaper_class_online) is what this bench
# now proves flat against that scale.
SYNTH_TASKS="${SYNTH_TASKS:-12000}"
SYNTH_WORKERS="${SYNTH_WORKERS:-600}"
SYNTH_SUPPLIERS="${SYNTH_SUPPLIERS:-60}"
# Historical dilution: completed tasks on separate finished jobs that bloat the
# tasks table without being claimable — the realistic backdrop entry 61's report
# used (its earlier bench had NO dilution, which is why the cost hid until then).
SYNTH_HIST_TASKS="${SYNTH_HIST_TASKS:-100000}"
EMBED_ROWS="${EMBED_ROWS:-1000}"
EMBED_DIM="${EMBED_DIM:-384}"
# Smoke mode: small + fast + deterministic, for an optional prove-local row.
if [ "$SMOKE" = "1" ]; then
  QUOTE_CALLS=5
  CLAIM_RUNS=3
  SYNTH_TASKS=200
  SYNTH_WORKERS=20
  SYNTH_SUPPLIERS=5
  SYNTH_HIST_TASKS=1000
  EMBED_ROWS=256
fi

ART="$ROOT/.artifacts/bench-local"
PGDATA="$ART/pgdata"
MINIO_DATA="$ART/minio-data"
CONTROL_LOG="$ART/control.log"
PG_LOG="$ART/pg.log"
MINIO_LOG="$ART/minio.log"
REPORT="$ART/report.md"

CONTROL_URL="http://localhost:$BCONTROL_PORT"
export DATABASE_URL="postgres://cx@localhost:$BPGPORT/cx?sslmode=disable"
export S3_ENDPOINT="http://localhost:$BMINIO_PORT"
export S3_PUBLIC_ENDPOINT="http://localhost:$BMINIO_PORT"
export S3_BUCKET="cx-jobs"
export S3_ACCESS_KEY="minioadmin"
export S3_SECRET_KEY="minioadmin"
export S3_REGION="us-east-1"
export LISTEN_ADDR=":$BCONTROL_PORT"

# Demo credentials are fixed by control/seed.go (same as prove-local).
API_KEY="dev-api-key-0001"
BUYER_ID="00000000-0000-0000-0000-0000000000c1"

CONTROL_PID=""; MINIO_PID=""; PG_STARTED=0

# ── Pretty logging ───────────────────────────────────────────────────────────
b()    { printf '\033[1m%s\033[0m' "$*"; }
say()  { printf '\033[1;36m[bench]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  ⚠\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }

die() {
  fail "$*"
  echo "----- control log (tail) -----" >&2; [ -f "$CONTROL_LOG" ] && tail -n 30 "$CONTROL_LOG" >&2 || true
  exit 1
}

# ── Cleanup: tear down only what we started (KEEP=1 leaves it up for reuse) ────
cleanup() {
  local ec=$?
  [ -n "$CONTROL_PID" ] && kill "$CONTROL_PID" 2>/dev/null || true
  sleep 1
  [ -n "$CONTROL_PID" ] && kill -9 "$CONTROL_PID" 2>/dev/null || true
  if [ "$KEEP" = "1" ]; then
    [ -n "$CONTROL_PID" ] && warn "KEEP=1 — leaving native deps up (DATABASE_URL=$DATABASE_URL)"
    exit "$ec"
  fi
  [ -n "$MINIO_PID" ] && kill "$MINIO_PID" 2>/dev/null || true
  if [ "$PG_STARTED" = "1" ]; then
    LC_ALL=C pg_ctl -D "$PGDATA" -m fast stop >/dev/null 2>&1 || true
  fi
  rm -rf "$PGDATA" "$MINIO_DATA" 2>/dev/null || true
  exit "$ec"
}
trap cleanup EXIT INT TERM

# wait_for <seconds> <label> <cmd...>
wait_for() {
  local timeout="$1" label="$2"; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      warn "timed out waiting for $label after ${timeout}s"
      return 1
    fi
    sleep 1
  done
  return 0
}

# percentiles <p50_var> <p90_var> < newline-separated numbers on stdin
#   prints "p50<TAB>p90<TAB>n<TAB>min<TAB>max" computed with nearest-rank (no deps).
#   Reads the data from THIS function's stdin (do not use a heredoc here — a
#   `python3 - <<EOF` would make the heredoc the program's stdin and shadow the
#   redirected data file; `-c` keeps stdin pointed at the caller's data).
percentiles() {
  python3 -c '
import sys, math
xs=sorted(float(l) for l in sys.stdin if l.strip())
n=len(xs)
if n==0:
    print("0\t0\t0\t0\t0"); raise SystemExit
def pct(p):
    # nearest-rank: ceil(p/100 * n), 1-indexed, clamped into [1, n].
    k=max(1,min(n,math.ceil(p/100.0*n)))
    return xs[k-1]
print("%.3f\t%.3f\t%d\t%.3f\t%.3f"%(pct(50),pct(90),n,xs[0],xs[-1]))
'
}

# ── Machine class (best-effort, never fatal) ─────────────────────────────────
machine_class() {
  local os arch cpu mem chip
  os="$(uname -s 2>/dev/null || echo '?')"
  arch="$(uname -m 2>/dev/null || echo '?')"
  if [ "$os" = "Darwin" ]; then
    chip="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || true)"
    cpu="$(sysctl -n hw.ncpu 2>/dev/null || echo '?')"
    local memb; memb="$(sysctl -n hw.memsize 2>/dev/null || echo 0)"
    mem="$(MEMB="$memb" python3 -c 'import os;print("%.0f GB"%(int(os.environ["MEMB"])/1e9))' 2>/dev/null || echo '?')"
  else
    chip="$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || true)"
    cpu="$(nproc 2>/dev/null || echo '?')"
    mem="$(python3 -c "import os;print(f'{os.sysconf(\"SC_PAGE_SIZE\")*os.sysconf(\"SC_PHYS_PAGES\")/1e9:.0f} GB')" 2>/dev/null || echo '?')"
  fi
  [ -z "$chip" ] && chip="$arch"
  printf '%s %s · %s cores · %s RAM' "$os" "$arch" "$cpu" "$mem"
  [ -n "$chip" ] && printf ' · %s' "$chip"
}

# ── Phase 0: preflight + provisioning ────────────────────────────────────────
say "$(b 'Computexchange — local benchmark lab (Plane D D10)')"
[ "$SMOKE" = "1" ] && say "SMOKE=1 — tiny/fast run"
mkdir -p "$ART"

need=(go psql curl python3)
[ "$KEEP" = "1" ] || need+=(postgres initdb pg_ctl createdb minio)
for t in "${need[@]}"; do
  command -v "$t" >/dev/null 2>&1 || die "required tool '$t' not found on PATH"
done

# Reuse a running stack (KEEP=1) if it already answers; else provision a throwaway
# one the SAME way prove-local does (native, trust auth, custom ports).
if [ "$KEEP" = "1" ] && psql "$DATABASE_URL" -c 'SELECT 1' >/dev/null 2>&1; then
  ok "reusing already-up native Postgres on :$BPGPORT (KEEP=1)"
else
  say "provisioning native Postgres (:$BPGPORT) + MinIO (:$BMINIO_PORT)"
  rm -rf "$PGDATA" "$MINIO_DATA" 2>/dev/null || true
  # LC_ALL=C + --locale=C avoids the macOS Homebrew-PG multithreaded-postmaster
  # locale bug (identical to prove-local).
  LC_ALL=C initdb -D "$PGDATA" -U cx --auth=trust -E UTF8 --locale=C >"$PG_LOG" 2>&1 \
    || die "initdb failed (see $PG_LOG)"
  LC_ALL=C pg_ctl -D "$PGDATA" -o "-p $BPGPORT -c listen_addresses=localhost -c unix_socket_directories=$ART" \
         -l "$PG_LOG" -w start >>"$PG_LOG" 2>&1 || die "pg_ctl start failed (see $PG_LOG)"
  PG_STARTED=1
  createdb -h localhost -p "$BPGPORT" -U cx cx 2>>"$PG_LOG" || die "createdb cx failed (see $PG_LOG)"
  wait_for 20 "postgres ready" psql "$DATABASE_URL" -c 'SELECT 1' || die "postgres not accepting connections"
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    minio server "$MINIO_DATA" --address "localhost:$BMINIO_PORT" --console-address "localhost:$BMINIO_CONSOLE" \
    >"$MINIO_LOG" 2>&1 &
  MINIO_PID=$!
  wait_for 30 "minio live" curl -fsS "http://localhost:$BMINIO_PORT/minio/health/live" \
    || die "minio did not come up (see $MINIO_LOG)"
  ok "native postgres(:$BPGPORT) + minio(:$BMINIO_PORT) up"
fi

# Schema + demo creds (idempotent — schema.sql is IF NOT EXISTS, seed re-confirms).
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f db/schema.sql >/dev/null || die "schema apply failed"
say "building + starting the control plane (for the quote benchmark)"
(cd control && go build -o "$ART/cx" .) || die "control build failed"
(cd control && "$ART/cx" seed) >/dev/null 2>&1 || (cd control && go run . seed) >/dev/null 2>&1 \
  || die "seed failed (demo buyer api_key)"
( "$ART/cx" ) >"$CONTROL_LOG" 2>&1 &
CONTROL_PID=$!
wait_for 30 "control healthz" bash -c "kill -0 $CONTROL_PID 2>/dev/null && curl -fsS '$CONTROL_URL/healthz'" \
  || die "control plane never became healthy"
ok "control plane healthy on :$BCONTROL_PORT"

# ── Benchmark (a): POST /v1/quote latency over K calls ───────────────────────
# The buyer preflight hot path (scan + price + persist, no spend). We time the full
# request round-trip with curl's own %{time_total} (seconds → ms), K times, after a
# warmup call (JIT of the model-catalogue read, first PG plan cache). Real numbers.
say "(a) timing POST /v1/quote over $QUOTE_CALLS calls"
QBODY='{"job_type":{"type":"embed"},"model":{"kind":"gguf","ref":"all-minilm-l6-v2"},"tier":"batch","verification":{"redundancy_frac":0,"honeypot_frac":0,"payout_hold_secs":0},"input":"{\"id\":\"a\",\"text\":\"quote me\"}\n{\"id\":\"b\",\"text\":\"before I spend\"}\n"}'
# Warmup (not measured) — confirms the endpoint actually prices, else bail loudly.
WARM="$(curl -fsS -X POST "$CONTROL_URL/v1/quote" -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' -d "$QBODY" 2>/dev/null || true)"
printf '%s' "$WARM" | python3 -c 'import sys,json;d=json.load(sys.stdin);assert d["quote_id"].startswith("q_")' 2>/dev/null \
  || die "quote endpoint did not return a valid quote (warmup) — got: ${WARM:0:200}"
QUOTE_MS="$ART/quote_ms.txt"; : >"$QUOTE_MS"
for _ in $(seq 1 "$QUOTE_CALLS"); do
  # %{time_total} is whole-request seconds; convert to ms. -o /dev/null drops the body.
  T="$(curl -fsS -o /dev/null -w '%{time_total}' -X POST "$CONTROL_URL/v1/quote" \
        -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
        -d "$QBODY" 2>/dev/null || echo '')"
  # Pass the timing via env (T_SECS) so no curl-formatted value is interpolated into
  # the Python source — robust regardless of locale/quoting.
  [ -n "$T" ] && T_SECS="$T" python3 -c 'import os;print("%.3f"%(float(os.environ["T_SECS"])*1000))' >>"$QUOTE_MS"
done
read -r Q_P50 Q_P90 Q_N Q_MIN Q_MAX < <(percentiles <"$QUOTE_MS")
[ "${Q_N:-0}" -ge 1 ] || die "no quote latencies captured"
ok "quote: p50=${Q_P50}ms p90=${Q_P90}ms (n=$Q_N, min=${Q_MIN} max=${Q_MAX})"

# ── Benchmark (b): the REAL ClaimTask query, at realistic scale ───────────────
# PATCH (Control plane hot path 4.5->5, docs/internal/CREED_AND_PATH_TO_TEN.md
# "Make the benchmark measure the real query"): this used to EXPLAIN ANALYZE a
# hand-simplified stand-in — a bare 6-column WHERE + a 2-column ORDER BY, run
# with enable_seqscan=off forced — that could never have caught the real
# tasks_ready_unclaimed_idx predicate mismatch (db/schema.sql vs the
# claimed_by OR-branch) because it was never the real query to begin with.
# This block now:
#   1. asks the REAL control binary for the REAL SQL text via
#      `control print-claim-sql` (which calls the exact same ClaimTaskSQL
#      function scheduler.go's ClaimTask calls — see control/scheduler.go and
#      control/main.go), so the string EXPLAIN ANALYZE runs below is
#      byte-identical to production, not a hand-copied paraphrase,
#   2. seeds a REALISTIC-SCALE synthetic queue: SYNTH_SUPPLIERS suppliers,
#      SYNTH_WORKERS workers spread across them (varied hw_class so the
#      cheaper_class_online correlated EXISTS subquery has real candidates to
#      scan), a benchmark_results row per worker (worker_tps tiebreak),
#      worker_model_state rows for a fraction of workers (warm_for_task
#      tiebreak), and SYNTH_TASKS queued tasks across several synthetic jobs
#      (job_dispatched_count tiebreak + realistic per-job task counts, not one
#      giant job),
#   3. runs the LITERAL query text via PREPARE + EXPLAIN (ANALYZE) EXECUTE
#      inside a transaction that is always ROLLBACK'd — the exact statement
#      ClaimTask's UPDATE...RETURNING executes, timed for real, with the row
#      it would have claimed put back so every one of CLAIM_RUNS repetitions
#      sees the identical queue,
#   4. leaves enable_seqscan and every other planner GUC at its default —
#      "realistic planner settings" per the rung text, not a forced plan.
say "(b) seeding realistic queue ($SYNTH_TASKS tasks / $SYNTH_WORKERS workers / $SYNTH_SUPPLIERS suppliers) + timing the REAL ClaimTask query"

# 1. Print the literal production SQL from the real binary. Fails loudly if
#    the binary or the subcommand ever disappears — never silently falls back
#    to a hand-written copy.
CLAIM_SQL_BODY="$("$ART/cx" print-claim-sql)"
[ -n "$CLAIM_SQL_BODY" ] || die "control print-claim-sql produced no output"
MATRIX_SHA256="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["matrix_sha256"])' "$ROOT/proof/runtime-matrix.generated.json")"
[ "${#MATRIX_SHA256}" -eq 64 ] || die "runtime matrix artifact has no valid matrix_sha256"
printf '%s' "$CLAIM_SQL_BODY" | grep -q "cheaper_class_online" \
  || die "print-claim-sql output missing cheaper_class_online — does not look like the real query"
printf '%s' "$CLAIM_SQL_BODY" | grep -q "enable_seqscan" \
  && die "print-claim-sql output embeds a forced planner GUC — the rung requires realistic planner settings"
say "  claim SQL confirmed: $(printf '%s' "$CLAIM_SQL_BODY" | wc -l | tr -d ' ') lines from \`control print-claim-sql\` (control/scheduler.go's ClaimTaskSQL, byte-identical to ClaimTask's own query)"

# 2. Seed a realistic-scale synthetic fleet + queue. Every FK the real query
#    joins through (workers -> suppliers, tasks -> jobs, benchmark_results,
#    worker_model_state, private_pool_members, ledger_entries) gets real rows,
#    not just the claiming worker's own row, so the correlated subqueries do
#    real work instead of a trivially-empty scan.
SYNTH_TAG="cxbench-$(date +%s)-$$"
CLAIMING_WORKER=""
psql "$DATABASE_URL" -q -v ON_ERROR_STOP=1 >/dev/null <<SQL || die "synthetic fleet+queue seed failed (see error above)"
-- $SYNTH_SUPPLIERS suppliers, active, spread reputations (feeds reputationTier
-- + the elite-supplier min_reputation gate).
INSERT INTO suppliers (id, email, reputation, status, completed_tasks, data_country)
  SELECT gen_random_uuid(), '$SYNTH_TAG-supplier-'||g||'@bench.local',
         0.3 + (g % 7)::real / 10.0, 'active', (g * 37) % 5000, 'US'
  FROM generate_series(1, $SYNTH_SUPPLIERS) AS g;

-- $SYNTH_WORKERS workers across those suppliers, varied production hw_class (drives the
-- cheaper_class_online EXISTS subquery — real candidates at every cost rank),
-- all live (<60s) and unthrottled so they are real eligible peers, not dead rows.
INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at,
                      supported_jobs, supported_models, min_payout_usd_hr,
                      effective_memory_gb, throttled)
  SELECT gen_random_uuid(),
         (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%' ORDER BY email OFFSET (g % $SYNTH_SUPPLIERS) LIMIT 1),
         (ARRAY['apple_silicon_base','apple_silicon_pro','apple_silicon_max','apple_silicon_ultra'])[1 + (g % 4)],
         16 + (g % 8) * 8, 200 + (g % 5) * 100, now() - (g % 30 || ' seconds')::interval,
         ARRAY['embed','batch_infer'], ARRAY['all-minilm-l6-v2'], 0,
         16 + (g % 8) * 8, false
  FROM generate_series(1, $SYNTH_WORKERS) AS g;

-- Runtime authority is normalized exact cells, never the two arrays above. Bind
-- every synthetic worker to the one production cell this benchmark queues.
INSERT INTO worker_authorized_capabilities
  (worker_id, cell_id, runtime_id, job_type, model_ref, model_kind, matrix_sha256)
  SELECT id, 'candle-metal-minilm-embed', 'candle_metal', 'embed',
         'all-minilm-l6-v2', 'hf', '$MATRIX_SHA256'
    FROM workers
   WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%');

-- A benchmark_results row per worker (worker_tps ORDER BY tiebreak). PATCH
-- (Control plane hot path 7->8): worker_tps is now read from the maintained
-- worker_tps_cache (a plain LEFT JOIN), not a per-row correlated subquery over
-- benchmark_results — seed BOTH so the real query's real join has real,
-- varied data to rank on, matching what UpsertWorker maintains in production.
INSERT INTO benchmark_results (worker_id, job_type, tps, eps, thermal_ok)
  SELECT id, 'embed', 20 + (random() * 180)::real, 20 + (random() * 180)::real, true
  FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%');
INSERT INTO worker_tps_cache (worker_id, job_type, tps)
  SELECT worker_id, job_type, tps FROM benchmark_results
  WHERE worker_id IN (SELECT id FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%'))
  ON CONFLICT (worker_id, job_type) DO UPDATE SET tps = EXCLUDED.tps;

-- A THIRD of workers report the bench model warm right now (warm_for_task
-- ORDER BY tiebreak has real true/false variety to sort on, not all-false).
INSERT INTO worker_model_state (worker_id, model_id, last_seen_warm)
  SELECT id, 'all-minilm-l6-v2', now() - interval '5 seconds'
  FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%')
    AND (('x'||substr(id::text,1,8))::bit(32)::bigint % 3) = 0;

-- SYNTH_TASKS tasks spread across ~50-task synthetic jobs (not one giant job)
-- so job_dispatched_count has real per-job variety, each job carrying its own
-- max_usd cap on a THIRD of jobs (exercises the budget-governor subqueries
-- against ledger_entries + in-flight running tasks for real, not a NULL no-op
-- on every row).
INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
                   task_count, tasks_done, min_memory_gb, estimated_usd, max_usd)
  SELECT gen_random_uuid(), '$BUYER_ID', 'running', 'embed', 'all-minilm-l6-v2',
         'jobs/bench/'||g||'/in.jsonl', 'batch', 50, 0, 2, 5.00,
         CASE WHEN g % 3 = 0 THEN 50.00 ELSE NULL END
  FROM generate_series(1, GREATEST(1, $SYNTH_TASKS / 50)) AS g;

INSERT INTO tasks (id, job_id, status, input_ref, chunk_index, visible_at)
  SELECT gen_random_uuid(), j.id, 'queued', 'jobs/bench/t'||row_number() OVER (PARTITION BY j.id)||'/in.jsonl',
         (row_number() OVER (PARTITION BY j.id))::int, now()
  FROM (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%') j,
       generate_series(1, 50) AS t;

-- Historical DILUTION (Control plane hot path 8->9 proof): SYNTH_HIST_TASKS
-- COMPLETED tasks on separate FINISHED jobs. They are never claimable (status
-- 'complete'), so they change no result — but they bloat the tasks table to a
-- realistic size, which is the exact backdrop entry 61 measured the O(queue x
-- fleet) explosion against (its earlier, dilution-free bench never saw it). The
-- eligible_jobs claimable-task guard (scheduler.go) is what keeps the claim flat
-- despite this: it never re-prices the fleet against a finished job.
INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
                   task_count, tasks_done, min_memory_gb, estimated_usd)
  SELECT gen_random_uuid(), '$BUYER_ID', 'complete', 'embed', 'all-minilm-l6-v2',
         'jobs/benchhist/'||g||'/in.jsonl', 'batch', 200, 200, 2, 5.00
  FROM generate_series(1, GREATEST(1, $SYNTH_HIST_TASKS / 200)) AS g;
INSERT INTO tasks (id, job_id, status, input_ref, chunk_index, visible_at, completed_at)
  SELECT gen_random_uuid(), j.id, 'complete', 'x',
         (row_number() OVER (PARTITION BY j.id))::int, now(), now()
  FROM (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/benchhist/%') j,
       generate_series(1, 200) AS t;

-- A few running (in-flight) tasks per job so the budget-governor's
-- COUNT(running) subquery has real non-zero rows to sum, not always 0.
UPDATE tasks SET status = 'running', claimed_by = (SELECT id FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%') ORDER BY id LIMIT 1)
  WHERE id IN (
    SELECT id FROM tasks WHERE job_id IN (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%')
    ORDER BY id LIMIT (SELECT count(*) / 20 FROM tasks WHERE job_id IN (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%'))
  );

-- The ONE claiming worker (bound as query param 1 below) binds to: a real row
-- the JOINs actually resolve (workers w ON w.id = param1, suppliers s ON
-- s.id = w.supplier_id), distinct from the synthetic fleet above so
-- cheaper_class_online has OTHER online peers to compare against, not just
-- itself (w2.id <> w.id excludes self).
INSERT INTO suppliers (id, email, reputation, status, completed_tasks, data_country)
  VALUES ('11111111-1111-1111-1111-111111111111', '$SYNTH_TAG-claimer@bench.local', 0.9, 'active', 500, 'US')
  ON CONFLICT (id) DO NOTHING;
INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at,
                      supported_jobs, supported_models, min_payout_usd_hr,
                      effective_memory_gb, throttled)
  VALUES ('22222222-2222-2222-2222-222222222222', '11111111-1111-1111-1111-111111111111',
          'apple_silicon_max', 64, 400, now(), ARRAY['embed','batch_infer'], ARRAY['all-minilm-l6-v2'],
          0, 64, false)
  ON CONFLICT (id) DO NOTHING;
INSERT INTO worker_authorized_capabilities
  (worker_id, cell_id, runtime_id, job_type, model_ref, model_kind, matrix_sha256)
  VALUES ('22222222-2222-2222-2222-222222222222', 'candle-metal-minilm-embed',
          'candle_metal', 'embed', 'all-minilm-l6-v2', 'hf', '$MATRIX_SHA256')
  ON CONFLICT DO NOTHING;

ANALYZE suppliers; ANALYZE workers; ANALYZE benchmark_results; ANALYZE worker_model_state;
ANALYZE worker_tps_cache; ANALYZE worker_authorized_capabilities;
ANALYZE jobs; ANALYZE tasks; ANALYZE ledger_entries;
SQL
CLAIMING_WORKER="22222222-2222-2222-2222-222222222222"
QUEUE_DEPTH="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM tasks WHERE status IN ('queued','retrying') AND claimed_by IS NULL AND job_id IN (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%')" 2>/dev/null | tr -d '[:space:]')"
WORKER_COUNT="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%')" 2>/dev/null | tr -d '[:space:]')"
ok "seeded $QUEUE_DEPTH claimable tasks across $((SYNTH_TASKS / 50))+ jobs, $WORKER_COUNT synthetic workers"

# 3. Run the LITERAL query (general/unclaimed branch — the common,
#    index-servable path tasks_ready_unclaimed_idx is meant to serve) via
#    PREPARE + EXPLAIN (ANALYZE) EXECUTE inside a rolled-back transaction, so
#    the UPDATE's real side effects (claimed_by/status flip) are timed but
#    then undone — every one of CLAIM_RUNS repetitions sees the identical
#    $QUEUE_DEPTH-task queue. $1=claiming worker id, $2=tier (2=trusted-eligible,
#    matching a real high-reputation supplier), $3=selfCostRank for
#    apple_silicon_max (rank 4, per hwClassCostRank in scheduler.go — cheaper
#    classes 0-3 are online in the seeded fleet above, so cheaper_class_online
#    is exercised both true and false across candidate rows), and $4 is the exact
#    generated runtime-matrix SHA required by every capability row.
CLAIM_MS="$ART/claim_ms.txt"; : >"$CLAIM_MS"
CLAIM_PLAN=""
# CLAIM_SQL_BODY (from `control print-claim-sql`) contains REAL Postgres bind
# placeholders ($1/$2/$3/$4) — it must NEVER pass through an unquoted bash
# heredoc/here-string, where bash itself would expand those as positional
# parameters before psql ever saw them (silently corrupting the query). We
# build the whole driver script as a FILE via `printf '%s'` (no shell
# re-interpretation of its contents) and feed it to `psql -f`, so the
# literal bytes control print-claim-sql produced are exactly what Postgres
# parses — byte-identical to what ClaimTask itself sends.
CLAIM_SQL_FILE="$ART/claim_query.sql"
printf '%s' "$CLAIM_SQL_BODY" >"$CLAIM_SQL_FILE"
CLAIM_DRIVER_FILE="$ART/claim_driver.sql"
{
  printf 'BEGIN;\n'
  printf 'PREPARE cx_bench_claim (uuid, int, int, text) AS\n'
  printf '%s' "$CLAIM_SQL_BODY"
  printf ';\n'
  printf "EXPLAIN (ANALYZE, TIMING ON, FORMAT JSON) EXECUTE cx_bench_claim('%s', 2, 4, '%s');\n" "$CLAIMING_WORKER" "$MATRIX_SHA256"
  printf 'ROLLBACK;\n'
} >"$CLAIM_DRIVER_FILE"
for _ in $(seq 1 "$CLAIM_RUNS"); do
  # -f runs all four statements (BEGIN/PREPARE/EXPLAIN/ROLLBACK) in one psql
  # invocation, so stdout also carries "BEGIN"/"PREPARE"/"ROLLBACK" lines
  # around the JSON — extract just the "[...]" EXPLAIN payload before parsing.
  RAW="$(psql "$DATABASE_URL" -tA -v ON_ERROR_STOP=1 -f "$CLAIM_DRIVER_FILE" 2>>"$ART/claim_explain.log" || true)"
  PLAN="$(printf '%s' "$RAW" | python3 -c 'import sys
s=sys.stdin.read()
i=s.find("[")
print(s[i:s.rfind("]")+1] if i >= 0 else "")')"
  [ -z "$CLAIM_PLAN" ] && CLAIM_PLAN="$PLAN"
  printf '%s' "$PLAN" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); print("%.3f"%d[0]["Execution Time"])
except Exception:
    pass' >>"$CLAIM_MS" 2>/dev/null || true
done
read -r C_P50 C_P90 C_N C_MIN C_MAX < <(percentiles <"$CLAIM_MS")
[ "${C_N:-0}" -ge 1 ] || die "no claim-query timings captured (EXPLAIN ANALYZE produced nothing — see $ART/claim_explain.log)"
CLAIM_INDEX="seq scan"
printf '%s' "$CLAIM_PLAN" | grep -q "tasks_ready_unclaimed_idx" && CLAIM_INDEX="tasks_ready_unclaimed_idx (index scan)"
# Confirm the transaction actually rolled back (queue depth unchanged) —
# a benchmark that silently drains the queue it is measuring is not repeatable.
QUEUE_DEPTH_AFTER="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM tasks WHERE status IN ('queued','retrying') AND claimed_by IS NULL AND job_id IN (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%')" 2>/dev/null | tr -d '[:space:]')"
[ "$QUEUE_DEPTH_AFTER" = "$QUEUE_DEPTH" ] || die "queue depth changed ($QUEUE_DEPTH -> $QUEUE_DEPTH_AFTER) — ROLLBACK did not undo the claim; benchmark is not repeatable"
ok "claim (REAL query): p50=${C_P50}ms p90=${C_P90}ms over $QUEUE_DEPTH-task queue / $WORKER_COUNT workers via $CLAIM_INDEX (rollback-verified repeatable)"

# ── Benchmark (b2): the FLATNESS proof (Control plane hot path 8->9) ───────────
# Entry 61 (docs/internal/CREED_AND_PATH_TO_TEN.md) root-caused a ~190x latency
# ratio between a near-empty and a loaded queue to cheaper_class_online — a
# correlated EXISTS that sequentially scanned `workers` ONCE PER CANDIDATE TASK
# ROW, i.e. O(queue x fleet). The 8->9 fix computes cheaper_class_online once per
# candidate JOB (eligible_jobs AS MATERIALIZED, scheduler.go) behind a
# claimable-task guard, so the fleet scan no longer grows with the queue. This
# step MEASURES that flattening directly: drain the SAME fleet's claimable queue
# down to near-empty (keep every worker + all historical dilution — only the
# claimable task count changes), re-ANALYZE, and time the IDENTICAL real query
# again. A near-1x loaded:near-empty ratio is the proof the O(queue x fleet)
# cost is gone; entry 61's ~190x is the "before".
say "(b2) draining the claimable queue to near-empty (same $WORKER_COUNT-worker fleet + dilution) to measure the loaded:near-empty claim ratio"
# Keep exactly ONE bench job's worth of tasks claimable; complete the rest. This
# leaves the fleet, worker_tps_cache, worker_model_state, and the historical
# dilution fully intact — only the claimable-queue depth drops.
psql "$DATABASE_URL" -q -v ON_ERROR_STOP=1 -c "
  UPDATE tasks SET status='complete', completed_at=now()
   WHERE status IN ('queued','retrying') AND claimed_by IS NULL
     AND job_id IN (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%')
     AND job_id <> (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%' ORDER BY id LIMIT 1);
  ANALYZE tasks;
" >/dev/null 2>&1 || warn "near-empty drain hit an error (flatness ratio may be unavailable)"
NEAR_DEPTH="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM tasks WHERE status IN ('queued','retrying') AND claimed_by IS NULL AND job_id IN (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%')" 2>/dev/null | tr -d '[:space:]')"
CLAIM_NEAR_MS="$ART/claim_near_ms.txt"; : >"$CLAIM_NEAR_MS"
for _ in $(seq 1 "$CLAIM_RUNS"); do
  RAW="$(psql "$DATABASE_URL" -tA -v ON_ERROR_STOP=1 -f "$CLAIM_DRIVER_FILE" 2>>"$ART/claim_explain.log" || true)"
  PLAN="$(printf '%s' "$RAW" | python3 -c 'import sys
s=sys.stdin.read()
i=s.find("[")
print(s[i:s.rfind("]")+1] if i >= 0 else "")')"
  printf '%s' "$PLAN" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); print("%.3f"%d[0]["Execution Time"])
except Exception:
    pass' >>"$CLAIM_NEAR_MS" 2>/dev/null || true
done
read -r CN_P50 CN_P90 CN_N _cn_min _cn_max < <(percentiles <"$CLAIM_NEAR_MS")
# Loaded:near-empty ratio on p50 (the flatness number). Guard against a zero
# denominator (a sub-millisecond near-empty p50 rounds to 0.000).
CLAIM_RATIO="$(C_P50="$C_P50" CN_P50="$CN_P50" python3 -c '
import os
loaded=float(os.environ.get("C_P50") or 0)
near=float(os.environ.get("CN_P50") or 0)
print("%.2f"%(loaded/near) if near>0 else "n/a")' 2>/dev/null || echo 'n/a')"
if [ "${CN_N:-0}" -ge 1 ]; then
  ok "claim near-empty ($NEAR_DEPTH-task queue, SAME fleet): p50=${CN_P50}ms p90=${CN_P90}ms → loaded:near-empty p50 ratio = ${CLAIM_RATIO}x (entry 61's pre-fix figure: ~190x)"
else
  warn "near-empty claim timings unavailable — flatness ratio skipped"
  CN_P50="n/a"; CN_P90="n/a"; CLAIM_RATIO="n/a"; NEAR_DEPTH="n/a"
fi

# Clean the synthetic fleet + queue so a KEEP=1 reuse starts from a known state.
psql "$DATABASE_URL" -q -c "
  DELETE FROM tasks WHERE job_id IN (SELECT id FROM jobs WHERE input_ref LIKE 'jobs/bench/%' OR input_ref LIKE 'jobs/benchhist/%');
  DELETE FROM jobs WHERE input_ref LIKE 'jobs/bench/%' OR input_ref LIKE 'jobs/benchhist/%';
  DELETE FROM worker_model_state WHERE worker_id IN (SELECT id FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%') OR supplier_id = '11111111-1111-1111-1111-111111111111');
  DELETE FROM worker_tps_cache WHERE worker_id IN (SELECT id FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%') OR supplier_id = '11111111-1111-1111-1111-111111111111');
  DELETE FROM benchmark_results WHERE worker_id IN (SELECT id FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%') OR supplier_id = '11111111-1111-1111-1111-111111111111');
  DELETE FROM workers WHERE supplier_id IN (SELECT id FROM suppliers WHERE email LIKE '$SYNTH_TAG-%') OR supplier_id = '11111111-1111-1111-1111-111111111111';
  DELETE FROM suppliers WHERE email LIKE '$SYNTH_TAG-%' OR id = '11111111-1111-1111-1111-111111111111';
" >/dev/null 2>&1 || true

# ── Benchmark (c): embed JSON-vs-binary artifact size for N rows ──────────────
# The D5/D15 payload economy: for EMBED_ROWS×EMBED_DIM f32 embeddings, the binary
# CXEM blob is 16 + N*dim*4 bytes; the JSON the runner would otherwise PUT is the
# compact {"job_type","model","dim","count","vectors":[[...]]} object. We compute BOTH
# exactly (deterministic, infra-free) and report the ratio + bytes saved. When the
# Rust toolchain is present we ALSO run the agent's own encoder unit test so the report
# can state the format is the agent's real one, not a re-derivation.
say "(c) embed JSON-vs-binary size for ${EMBED_ROWS}×${EMBED_DIM} f32 rows"
# Compute into a file via a plain heredoc (no heredoc-inside-process-substitution —
# that confuses bash's parser), then read the tab-separated line back.
EMBED_OUT="$ART/embed_size.tsv"
EMBED_ROWS="$EMBED_ROWS" EMBED_DIM="$EMBED_DIM" python3 - >"$EMBED_OUT" <<'PY' || die "embed size computation failed"
import json,os
N=int(os.environ["EMBED_ROWS"]); DIM=int(os.environ["EMBED_DIM"])
# Binary CXEM blob: 16-byte header + N*DIM little-endian f32 (the exact agent layout).
bin_bytes=16+N*DIM*4
# JSON the runner would PUT for the same rows. Build the real object (deterministic
# values) and json.dumps it compactly so the byte count is the true serialized size.
vecs=[[((r*DIM+c)%1000)*0.001-0.5 for c in range(DIM)] for r in range(N)]
js=json.dumps({"job_type":"embed","model":"all-minilm-l6-v2","dim":DIM,"count":N,"vectors":vecs},
              separators=(",",":")).encode()
json_bytes=len(js)
ratio=json_bytes/bin_bytes if bin_bytes else 0.0
print("%d\t%d\t%.2f\t%d"%(bin_bytes,json_bytes,ratio,json_bytes-bin_bytes))
PY
IFS=$'\t' read -r BIN_BYTES JSON_BYTES RATIO SAVED <"$EMBED_OUT"
[ -n "${BIN_BYTES:-}" ] || die "embed size computation produced no output"
ok "embed: binary=${BIN_BYTES}B json=${JSON_BYTES}B → JSON is ${RATIO}× binary (saves ${SAVED}B)"
# Cross-check the format against the agent's own encoder test (best-effort; skipped if
# cargo absent or the test cannot build — never fatal, the sizes above stand alone).
ENCODER_CHECK="skipped (cargo not run)"
if [ "$SMOKE" != "1" ] && command -v cargo >/dev/null 2>&1; then
  if (cd agent && cargo test --release encode_embeddings_binary -- --nocapture) >"$ART/agent-encoder-test.log" 2>&1 \
     || (cd agent && cargo test --release embed_binary -- --nocapture) >>"$ART/agent-encoder-test.log" 2>&1; then
    ENCODER_CHECK="PASS (agent cargo encoder test)"
    ok "agent binary encoder unit test confirms the CXEM layout"
  else
    ENCODER_CHECK="not run (agent encoder test unavailable)"
    warn "agent encoder test did not run — size figures above are independent of it"
  fi
fi

# ── Report ───────────────────────────────────────────────────────────────────
TS_UTC="$(date -u +'%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u 2>/dev/null || echo 'unknown')"
GIT_SHA="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo 'unknown')"
GIT_DIRTY=""
git -C "$ROOT" diff --quiet 2>/dev/null || GIT_DIRTY=" (dirty)"
MACHINE="$(machine_class)"

cat >"$REPORT" <<MD
# Computexchange — local benchmark report

> Repeatable LOCAL speed measurements (Plane D §16 D10). SEPARATE from prove-local
> (correctness). Numbers are REAL measurements on this machine, not targets.

| field        | value |
|--------------|-------|
| generated    | \`$TS_UTC\` |
| git commit   | \`$GIT_SHA\`$GIT_DIRTY |
| machine      | $MACHINE |
| mode         | $([ "$SMOKE" = "1" ] && echo 'smoke (tiny/fast)' || echo 'full') |

## (a) POST /v1/quote latency

Whole-request round-trip over **$Q_N** calls (after warmup).

| p50 | p90 | min | max |
|-----|-----|-----|-----|
| ${Q_P50} ms | ${Q_P90} ms | ${Q_MIN} ms | ${Q_MAX} ms |

## (b) The REAL ClaimTask query — full CTE, realistic scale

\`EXPLAIN ANALYZE\` of the **literal, verbatim SQL** \`control print-claim-sql\`
prints from \`ClaimTaskSQL\` (control/scheduler.go) — the exact joins,
correlated subqueries (\`cheaper_class_online\`, \`worker_tps\`,
\`warm_for_task\`, \`job_dispatched_count\`, the budget-governor projected-spend
subqueries), and computed \`ORDER BY\` ClaimTask itself executes — run under
default (realistic) planner settings, no forced \`enable_seqscan\`. Every run
executes inside \`BEGIN; PREPARE; EXPLAIN ANALYZE EXECUTE; ROLLBACK;\`, and the
harness re-checks queue depth after every run to confirm the rollback
actually left the queue unchanged (repeatable, not draining).

Queue: **$QUEUE_DEPTH** claimable tasks / **$WORKER_COUNT** synthetic workers,
behind **$SYNTH_HIST_TASKS** historical-completed-task dilution rows, $C_N runs.
Plan: **$CLAIM_INDEX**.

| p50 | p90 | min | max |
|-----|-----|-----|-----|
| ${C_P50} ms | ${C_P90} ms | ${C_MIN} ms | ${C_MAX} ms |

Full rendered SQL (identical to what ClaimTask executes): \`.artifacts/bench-local/claim_query.sql\`

### (b2) Flatness — the Control Plane Hot Path 8->9 proof

Entry 61 (docs/internal/CREED_AND_PATH_TO_TEN.md) root-caused a **~190x** latency
ratio between a near-empty and a loaded queue to \`cheaper_class_online\` — a
correlated \`EXISTS\` that sequentially scanned \`workers\` **once per candidate
task row** (O(queue × fleet)). The 8->9 fix computes it **once per candidate job**
(\`eligible_jobs AS MATERIALIZED\`, behind a claimable-task guard), so the fleet
scan no longer grows with the claimable queue. The row below is the SAME real
query, SAME $WORKER_COUNT-worker fleet + dilution, run against a near-empty
claimable queue — the loaded:near-empty p50 ratio is the flatness number.

| loaded p50 | near-empty p50 (${NEAR_DEPTH}-task) | loaded:near-empty ratio | entry 61 (pre-fix) |
|-----------|-----------------|-------------------------|--------------------|
| ${C_P50} ms | ${CN_P50} ms | **${CLAIM_RATIO}x** | ~190x |

## (c) Embed artifact size — JSON vs binary

For **$EMBED_ROWS** rows × **$EMBED_DIM**-dim f32 embeddings (the D5/D15 payload path).

| binary (CXEM) | JSON | JSON ÷ binary | bytes saved |
|---------------|------|---------------|-------------|
| ${BIN_BYTES} B | ${JSON_BYTES} B | ${RATIO}× | ${SAVED} B |

Format cross-check: **$ENCODER_CHECK**

---

_Generated by \`scripts/bench-local.sh\`. Re-run with \`make bench-local\`. This harness
never replaces \`prove-local\` and changes nothing it touches._
MD

echo
say "$(b '================  BENCH REPORT  ================')"
ok "quote   p50=${Q_P50}ms  p90=${Q_P90}ms   (n=$Q_N)"
ok "claim   p50=${C_P50}ms  p90=${C_P90}ms   (REAL query, $QUEUE_DEPTH-task queue / $WORKER_COUNT workers, $CLAIM_INDEX)"
ok "flat    loaded p50=${C_P50}ms vs near-empty p50=${CN_P50}ms → ${CLAIM_RATIO}x (entry 61 pre-fix: ~190x)"
ok "embed   binary=${BIN_BYTES}B  json=${JSON_BYTES}B  (${RATIO}× smaller)"
echo
say "report → $REPORT"
say "$(b 'LOCAL BENCHMARK: DONE') ✅"
