#!/usr/bin/env bash
# Parallel, disposable-worktree mutation campaign for a frozen candidate.
#
# The serial mutation runner intentionally edits its own source tree, so it
# must own that tree.  This orchestrator keeps the candidate tree read-only:
# every shard receives a detached worktree at the same exact commit and every
# shard receives its own temporary PostgreSQL cluster. Every mutant still gets
# a fresh disposable database inside that cluster. That gives the full 84-case
# campaign useful wall-clock parallelism without letting concurrent tests share
# source bytes, database state, or one server's WAL lock.
#
# The runner is designed for a frozen candidate worktree, not a developer's
# active tree. It keeps four cores free on the 28-core development host and
# uses up to sixteen single-runtime workers, a 15-minute standalone ceiling,
# source-specific observed contracts, and immutable shared Go caches. The run
# fails rather than quietly reducing coverage when any worker, mutation,
# restoration proof, or budget fails. Adjust only through explicit variables:
#
#   MERC_MUTATION_WORKERS=16              # 1..32; default min(16, CPUs-4)
#   MERC_MUTATION_WALLCLOCK_SECONDS=900   # default 15-minute certification ceiling
#   MERC_MUTATION_POSTGRES_PORT_BASE=0     # 0 finds an unused local range
#   MERC_MUTATION_TEST_STRATEGY=adaptive   # adaptive (default), contracts, or full
#   MERC_MUTATION_GOMAXPROCS=1             # per-worker runtime CPU ceiling
#   MERC_MUTATION_PARALLEL_CASE_IDS=1,25   # optional calibrated subset
#   MERC_MUTATION_KEEP_WORKDIR=1          # retain failed shard logs/worktrees
#   MERC_MUTATION_TIMINGS_OUT=/tmp/run.json # external, validated timing record
#
#   bash scripts/mutation-test-parallel.sh --plan
#   bash scripts/mutation-test-parallel.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

usage() {
  cat <<'EOF'
usage: bash scripts/mutation-test-parallel.sh [--plan]

Runs every declared mutation in disposable detached worktrees at one exact
candidate commit.  --plan prints the deterministic sharding without creating a
worktree, database, or lock.
EOF
}

mode="run"
case "${1:-}" in
  "") ;;
  --plan) mode="plan" ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

cpu_count="$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)"
if ! [[ "$cpu_count" =~ ^[1-9][0-9]*$ ]]; then
  cpu_count=4
fi
default_workers=$((cpu_count - 4))
if [ "$default_workers" -lt 1 ]; then
  default_workers=1
elif [ "$default_workers" -gt 16 ]; then
  default_workers=16
fi
workers="${MERC_MUTATION_WORKERS:-$default_workers}"
budget_seconds="${MERC_MUTATION_WALLCLOCK_SECONDS:-900}"
postgres_port_base="${MERC_MUTATION_POSTGRES_PORT_BASE:-0}"
test_strategy="${MERC_MUTATION_TEST_STRATEGY:-adaptive}"
go_max_procs="${MERC_MUTATION_GOMAXPROCS:-1}"
parallel_case_ids="${MERC_MUTATION_PARALLEL_CASE_IDS:-}"
timings_out="${MERC_MUTATION_TIMINGS_OUT:-}"
if ! [[ "$workers" =~ ^[1-9][0-9]*$ ]] || [ "$workers" -gt 32 ]; then
  echo "MERC_MUTATION_WORKERS must be an integer from 1 through 32" >&2
  exit 2
fi
if ! [[ "$budget_seconds" =~ ^[1-9][0-9]*$ ]]; then
  echo "MERC_MUTATION_WALLCLOCK_SECONDS must be a positive integer" >&2
  exit 2
fi
if ! [[ "$postgres_port_base" =~ ^[0-9]+$ ]]; then
  echo "MERC_MUTATION_POSTGRES_PORT_BASE must be zero or a TCP port" >&2
  exit 2
fi
case "$test_strategy" in
  adaptive|contracts|full) ;;
  *)
    echo "MERC_MUTATION_TEST_STRATEGY must be adaptive, contracts, or full" >&2
    exit 2
    ;;
esac
if ! [[ "$go_max_procs" =~ ^[1-9][0-9]*$ ]]; then
  echo "MERC_MUTATION_GOMAXPROCS must be a positive integer" >&2
  exit 2
fi
if [ -n "$parallel_case_ids" ] && ! [[ "$parallel_case_ids" =~ ^[1-9][0-9]*(,[1-9][0-9]*)*$ ]]; then
  echo "MERC_MUTATION_PARALLEL_CASE_IDS must be comma-separated positive case IDs" >&2
  exit 2
fi
if [ -n "$timings_out" ] && [ ! -d "$(dirname "$timings_out")" ]; then
  echo "MERC_MUTATION_TIMINGS_OUT parent directory does not exist" >&2
  exit 2
fi
if [ -n "$timings_out" ] && ! python3 - "$ROOT" "$timings_out" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1]).resolve()
target = Path(sys.argv[2]).resolve()
try:
    target.relative_to(root)
except ValueError:
    raise SystemExit(0)
raise SystemExit("MERC_MUTATION_TIMINGS_OUT must be outside the frozen candidate tree")
PY
then
  exit 2
fi

declare -a case_ids=()
declare -a case_descriptions=()
while IFS=$'\t' read -r case_id description; do
  [[ "$case_id" =~ ^[1-9][0-9]*$ ]] || {
    echo "mutation list emitted an invalid case id: $case_id" >&2
    exit 2
  }
  [[ -n "$description" ]] || {
    echo "mutation list emitted an empty description for case $case_id" >&2
    exit 2
  }
  case_ids+=("$case_id")
  case_descriptions+=("$description")
done < <(MERC_MUTATION_LIST=1 bash scripts/mutation-test.sh)

if [ -n "$parallel_case_ids" ]; then
  duplicate_ids="$(printf '%s\n' "$parallel_case_ids" | tr ',' '\n' | sort | uniq -d)"
  if [ -n "$duplicate_ids" ]; then
    echo "MERC_MUTATION_PARALLEL_CASE_IDS contains duplicate case IDs: $duplicate_ids" >&2
    exit 2
  fi
  requested_count="$(printf '%s\n' "$parallel_case_ids" | tr ',' '\n' | wc -l | tr -d ' ')"
  declare -a selected_case_ids=()
  declare -a selected_case_descriptions=()
  for index in "${!case_ids[@]}"; do
    case ",$parallel_case_ids," in
      *",${case_ids[$index]},"*)
        selected_case_ids+=("${case_ids[$index]}")
        selected_case_descriptions+=("${case_descriptions[$index]}")
        ;;
    esac
  done
  if [ "${#selected_case_ids[@]}" -ne "$requested_count" ]; then
    echo "MERC_MUTATION_PARALLEL_CASE_IDS names an unknown case" >&2
    exit 2
  fi
  case_ids=("${selected_case_ids[@]}")
  case_descriptions=("${selected_case_descriptions[@]}")
fi

case_count="${#case_ids[@]}"
if [ "$case_count" -eq 0 ]; then
  echo "parallel mutation test refused: no mutation cases were declared" >&2
  exit 2
fi
if [ "$workers" -gt "$case_count" ]; then
  workers="$case_count"
fi

if [ ! -f scripts/mutation-manifest.json ]; then
  echo "parallel mutation test requires scripts/mutation-manifest.json" >&2
  exit 2
fi
python3 scripts/mutation-manifest.py --root . --validate >/dev/null

# First-fit decreasing on measured p90s keeps a slow database contract from
# being stranded at the end of one shard. On the first calibrated run every
# unknown weight is identical, so this still produces a complete deterministic
# plan without inventing performance data.
case_weight_json="$(python3 - "${case_ids[@]}" <<'PY'
import json
import subprocess
import sys

selected = [int(value) for value in sys.argv[1:]]
result = subprocess.run(
    ["python3", "scripts/mutation-manifest.py", "--root", ".", "--weights"],
    text=True, capture_output=True, check=False,
)
if result.returncode:
    raise SystemExit(result.stderr)
weights = {}
for line in result.stdout.splitlines():
    case_id, weight = line.split("\t")
    weights[int(case_id)] = float(weight)
if set(selected) - set(weights):
    raise SystemExit("mutation manifest did not provide every selected weight")
print(json.dumps([[case_id, weights[case_id]] for case_id in selected], separators=(",", ":")))
PY
)"

declare -a worker_shards=()
declare -a worker_loads=()
while IFS=$'\t' read -r planned_worker planned_ids planned_load; do
  worker_shards[$((planned_worker - 1))]="$planned_ids"
  worker_loads[$((planned_worker - 1))]="$planned_load"
done < <(python3 - "$workers" "$case_weight_json" <<'PY'
import json
import sys

workers = int(sys.argv[1])
items = [(int(case_id), float(weight)) for case_id, weight in json.loads(sys.argv[2])]
bins = [([], 0.0) for _ in range(workers)]
for case_id, weight in sorted(items, key=lambda value: (-value[1], value[0])):
    target = min(range(workers), key=lambda index: (bins[index][1], index))
    bins[target][0].append(case_id)
    bins[target] = (bins[target][0], bins[target][1] + weight)
for index, (cases, load) in enumerate(bins, 1):
    print(f"{index}\t{','.join(str(case_id) for case_id in sorted(cases))}\t{load:.6f}")
PY
)

print_plan() {
  local worker ids
  printf 'parallel-mutation-test: candidate=%s cases=%s workers=%s budget=%ss db=isolated-clusters strategy=%s gomaxprocs=%s sharding=weighted-p90\n' \
    "$(git rev-parse HEAD)" "$case_count" "$workers" "$budget_seconds" "$test_strategy" "$go_max_procs"
  worker=1
  while [ "$worker" -le "$workers" ]; do
    ids="${worker_shards[$((worker - 1))]}"
    printf '  worker %02d: %s (p90_load=%ss)\n' "$worker" "$ids" "${worker_loads[$((worker - 1))]}"
    worker=$((worker + 1))
  done
}

if [ "$mode" = "plan" ]; then
  print_plan
  exit 0
fi

: "${MERC_TEST_DATABASE_URL:?parallel mutation testing needs a database}"
for command in initdb pg_ctl createdb pg_dump pg_restore; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "parallel mutation testing needs $command for isolated PostgreSQL clusters" >&2
    exit 2
  }
done

candidate="$(git rev-parse HEAD)"
if [ -n "$(git status --porcelain)" ]; then
  echo "parallel mutation test requires a clean frozen candidate" >&2
  exit 2
fi
started="$(date +%s)"

# Share the serial runner's candidate-root lock.  A concurrent serial mutation
# would otherwise change the candidate while isolated shards were proving a
# different snapshot.
repo_lock_id="$(printf '%s' "$ROOT" | shasum -a 256 | cut -c1-16)"
candidate_lock="${TMPDIR:-/tmp}/merc-mutation-${repo_lock_id}.lock"
if ! mkdir "$candidate_lock" 2>/dev/null; then
  echo "another mutation campaign owns $candidate_lock; refusing overlap" >&2
  exit 2
fi

run_root="$(mktemp -d "${TMPDIR:-/tmp}/merc-mutation-parallel.XXXXXX")"
selected_case_csv="$(IFS=,; printf '%s' "${case_ids[*]}")"
preflight_sources_file="$run_root/preflight-sources"
MERC_MUTATION_LIST=1 MERC_MUTATION_LIST_DETAIL=1 bash scripts/mutation-test.sh |
  awk -F '\t' -v selected=",$selected_case_csv," 'index(selected, "," $1 ",") { print $2 }' |
  LC_ALL=C sort -u >"$preflight_sources_file"
[ -s "$preflight_sources_file" ] || {
  echo "parallel mutation test could not resolve selected source contracts" >&2
  exit 2
}
preflight_cache="$run_root/preflight-cache.json"
declare -a worktrees=()
declare -a worker_pids=()
declare -a worker_logs=()
declare -a cluster_dirs=()
declare -a cluster_ports=()
declare -a template_names=()
declare -a worker_timing_files=()
candidate_baseline_pid=""
lfs_verify_pid=""
run_ok=0

cleanup() {
  local worktree cluster pid
  trap - EXIT INT TERM
  for pid in "$candidate_baseline_pid" "$lfs_verify_pid"; do
    [ -n "$pid" ] || continue
    kill -TERM "$pid" >/dev/null 2>&1 || true
    wait "$pid" >/dev/null 2>&1 || true
  done
  # Stop clusters before removing their worktree/log parent. Fast shutdown is
  # safe here: these clusters are test-only and every test database is already
  # disposable.
  for cluster in "${cluster_dirs[@]:-}"; do
    [[ -n "$cluster" ]] || continue
    case "$cluster" in
      "$run_root"/postgres-*)
        pg_ctl -D "$cluster" -m fast -w stop >/dev/null 2>&1 || true
        ;;
    esac
  done
  for worktree in "${worktrees[@]:-}"; do
    [[ -n "$worktree" ]] || continue
    case "$worktree" in
      "$run_root"/worker-*)
        git worktree remove --force "$worktree" >/dev/null 2>&1 || true
        ;;
    esac
  done
  rmdir "$candidate_lock" 2>/dev/null || true
  if [ "$run_ok" -eq 1 ] && [ "${MERC_MUTATION_KEEP_WORKDIR:-0}" != "1" ]; then
    case "$run_root" in
      "${TMPDIR:-/tmp}"/merc-mutation-parallel.*)
        rm -rf "$run_root"
        ;;
    esac
  elif [ "$run_ok" -ne 1 ]; then
    echo "parallel-mutation-test: retained diagnostics at $run_root" >&2
  fi
}

find_cluster_port_base() {
  python3 - "$workers" "$postgres_port_base" <<'PY'
import socket
import sys

workers = int(sys.argv[1])
requested = int(sys.argv[2])
if requested:
    candidates = [requested]
else:
    candidates = range(56000, 65000 - workers)

for base in candidates:
    if base < 1024 or base + workers - 1 > 65535:
        continue
    sockets = []
    try:
        for port in range(base, base + workers):
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.bind(("127.0.0.1", port))
            sockets.append(sock)
    except OSError:
        pass
    else:
        print(base)
        raise SystemExit(0)
    finally:
        for sock in sockets:
            sock.close()

raise SystemExit("no contiguous local PostgreSQL port range is available")
PY
}

stop_workers() {
  local pid
  for pid in "${worker_pids[@]:-}"; do
    [[ -n "$pid" ]] && kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "${worker_pids[@]:-}"; do
    [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
  done
}

on_signal() {
  echo "parallel-mutation-test: signal received; stopping shards" >&2
  stop_workers
  exit 130
}

trap cleanup EXIT
trap on_signal INT TERM

print_plan

# The independent LFS proof and candidate-level unit baseline have no mutable
# shared state with disposable worktree/cluster creation. Start both now so
# their required work overlaps setup rather than extending the critical path.
# We still wait for both before a worker can exercise one mutation.
(
  cd "$ROOT/control" &&
    env -u MERC_TEST_DATABASE_URL MERC_ALLOW_SKIPPING_DB_TESTS=1 \
      go test -count=1 -timeout=2m ./...
) >"$run_root/candidate-unit-baseline.log" 2>&1 &
candidate_baseline_pid="$!"
python3 scripts/verify-lfs-corpus.py --root "$ROOT" >"$run_root/candidate-lfs-verify.log" 2>&1 &
lfs_verify_pid="$!"

cluster_port_base="$(find_cluster_port_base)"

for ((worker = 1; worker <= workers; worker++)); do
  shard_file="$run_root/shard-$worker.ids"
  printf '%s\n' "${worker_shards[$((worker - 1))]}" | tr ',' '\n' >"$shard_file"
  worktree="$run_root/worker-$worker"
  git worktree add --detach "$worktree" "$candidate" >/dev/null
  worktrees+=("$worktree")
  if [ "$(git -C "$worktree" rev-parse HEAD)" != "$candidate" ] ||
     [ -n "$(git -C "$worktree" status --porcelain)" ]; then
    echo "worker $worker did not start at a clean exact candidate" >&2
    exit 1
  fi
  if ! git -C "$worktree" lfs checkout >/dev/null; then
    echo "worker $worker could not hydrate its local LFS corpus" >&2
    exit 1
  fi
  # `git lfs checkout` does not fetch. Reject a leftover canonical pointer so
  # evidence readers cannot fail for missing bytes and be misclassified as a
  # mutation catch. The candidate corpus above has already verified OID, size,
  # and hydrated-body SHA-256 for these exact indexed objects.
  while IFS= read -r lfs_path; do
    [ -n "$lfs_path" ] || continue
    if [ "$(LC_ALL=C head -c 42 "$worktree/$lfs_path")" = "version https://git-lfs.github.com/spec/v1" ]; then
      echo "worker $worker retained an unhydrated LFS pointer: $lfs_path" >&2
      exit 1
    fi
  done < <(git -C "$worktree" lfs ls-files -n)
done

for ((worker = 1; worker <= workers; worker++)); do
  cluster="$run_root/postgres-$worker"
  port=$((cluster_port_base + worker - 1))
  initdb --no-locale --encoding=UTF8 --auth-host=trust --auth-local=trust \
    --username=cx -D "$cluster" >/dev/null
  cluster_dirs+=("$cluster")
  cluster_ports+=("$port")
  pg_ctl -D "$cluster" -l "$run_root/postgres-$worker.log" \
    -o "-h 127.0.0.1 -p $port" -w start >/dev/null
  createdb --host=127.0.0.1 --port="$port" --username=cx \
    --maintenance-db=postgres cx
done

# No worker may run a contract until both exact-candidate prerequisites passed.
if ! wait "$candidate_baseline_pid"; then
  echo "parallel-mutation-test: candidate unit baseline failed" >&2
  cat "$run_root/candidate-unit-baseline.log" >&2 || true
  exit 1
fi
candidate_baseline_pid=""
if ! wait "$lfs_verify_pid"; then
  echo "parallel mutation test requires an independently verified hydrated LFS corpus" >&2
  cat "$run_root/candidate-lfs-verify.log" >&2 || true
  exit 2
fi
lfs_verify_pid=""

# A seed cluster builds the one schema-only candidate template. The remaining
# isolated clusters restore an exact pg_dump snapshot of that template instead
# of independently re-running the same migration for every worker. Every contract
# still gets a fresh createdb clone in its own worker cluster.
prepare_seed_template() {
  local worktree port template template_url
  worktree="${worktrees[0]}"
  port="${cluster_ports[0]}"
  template="${template_names[0]}"
  template_url="postgres://cx@127.0.0.1:${port}/${template}?sslmode=disable"
  createdb --host=127.0.0.1 --port="$port" --username=cx \
    --maintenance-db=postgres "$template"
  (
    cd "$worktree/control" &&
      MERC_TEST_DATABASE_URL="$template_url" MERC_MUTATION_TEMPLATE_DB=1 \
        GOMAXPROCS="$go_max_procs" \
        go test -count=1 -timeout=2m -run '^TestMutationTemplateSchema$' .
  ) >"$run_root/template-seed.log" 2>&1
}

snapshot_template() {
  local port="${cluster_ports[0]}" template="${template_names[0]}"
  pg_dump --host=127.0.0.1 --port="$port" --username=cx \
    --format=custom --no-owner --no-privileges \
    --file="$run_root/template.snapshot" "$template" \
    >"$run_root/template-snapshot.log" 2>&1
  [ -s "$run_root/template.snapshot" ]
}

restore_template_snapshot() {
  local worker="$1" port template
  port="${cluster_ports[$((worker - 1))]}"
  template="${template_names[$((worker - 1))]}"
  createdb --host=127.0.0.1 --port="$port" --username=cx \
    --maintenance-db=postgres "$template"
  pg_restore --host=127.0.0.1 --port="$port" --username=cx \
    --dbname="$template" --no-owner --no-privileges --exit-on-error \
    "$run_root/template.snapshot" >"$run_root/template-$worker.log" 2>&1
}

for ((worker = 1; worker <= workers; worker++)); do
  template_names[$((worker - 1))]="merc_mutation_template_w${worker}"
done
if ! prepare_seed_template; then
  echo "parallel-mutation-test: seed worker could not prepare its schema template" >&2
  cat "$run_root/template-seed.log" >&2 || true
  exit 1
fi
if ! snapshot_template; then
  echo "parallel-mutation-test: could not snapshot its exact schema template" >&2
  cat "$run_root/template-snapshot.log" >&2 || true
  exit 1
fi

declare -a template_pids=()
for ((worker = 2; worker <= workers; worker++)); do
  restore_template_snapshot "$worker" &
  template_pids+=("$!")
done
for ((index = 0; index < ${#template_pids[@]}; index++)); do
  worker=$((index + 2))
  if ! wait "${template_pids[$index]}"; then
    echo "parallel-mutation-test: worker $worker could not prepare its schema template" >&2
    cat "$run_root/template-$worker.log" >&2 || true
    exit 1
  fi
done

# A clean preflight must prove every source's named invariants before any
# mutation starts. The aggregate run is not a shortcut: every worker later
# re-hashes and re-parses these logs for its own exact source subset. This
# removes only duplicate clean preflights, while every mutant still gets a
# fresh disposable database inside its isolated worker cluster.
run_aggregate_preflight() {
  local worktree port selector
  worktree="${worktrees[0]}"
  port="${cluster_ports[0]}"
  selector="$(cd "$worktree" && python3 scripts/mutation-preflight-cache.py \
    --root . --sources "$preflight_sources_file" --selector)" || return 1
  (
    cd "$worktree/control" &&
      env -u MERC_TEST_DATABASE_URL MERC_ALLOW_SKIPPING_DB_TESTS=1 \
        GOMAXPROCS="$go_max_procs" \
        go test -json -count=1 -timeout=2m -run "$selector" .
  ) >"$run_root/preflight-unit.json" 2>&1 || return 1
  (
    cd "$worktree" &&
      MERC_TEST_DATABASE_URL="postgres://cx@127.0.0.1:${port}/cx?sslmode=disable" \
      MERC_ISOLATED_TEST_DB_PREFIX="merc_mutation_preflight" \
      MERC_ISOLATED_TEST_DB_TEMPLATE="${template_names[0]}" \
      GOMAXPROCS="$go_max_procs" \
      bash scripts/with-isolated-test-db.sh \
        bash -c 'cd "$1" && go test -json -count=1 -timeout=2m -run "$2" .' \
        _ "$worktree/control" "$selector"
  ) >"$run_root/preflight-db.json" 2>&1 || return 1
  (
    cd "$worktree" &&
      python3 scripts/mutation-preflight-cache.py \
        --root . --sources "$preflight_sources_file" --cache "$preflight_cache" --create
  ) >"$run_root/preflight-cache.log" 2>&1 || return 1
  chmod 0444 "$run_root/preflight-unit.json" "$run_root/preflight-db.json" "$preflight_cache"
}

if ! run_aggregate_preflight; then
  echo "parallel-mutation-test: aggregate clean contract preflight failed" >&2
  cat "$run_root/preflight-unit.json" >&2 || true
  cat "$run_root/preflight-db.json" >&2 || true
  cat "$run_root/preflight-cache.log" >&2 || true
  exit 1
fi

for ((worker = 1; worker <= workers; worker++)); do
  worktree="${worktrees[$((worker - 1))]}"
  port="${cluster_ports[$((worker - 1))]}"
  shard_file="$run_root/shard-$worker.ids"
  shard_ids="$(paste -sd, "$shard_file")"
  log="$run_root/worker-$worker.log"
  worker_logs+=("$log")
  timing_file="$run_root/worker-$worker.timings.jsonl"
  worker_timing_files+=("$timing_file")
  (
    cd "$worktree"
    export MERC_TEST_DATABASE_URL="postgres://cx@127.0.0.1:${port}/cx?sslmode=disable"
    export MERC_MUTATION_CASE_IDS="$shard_ids"
    export MERC_MUTATION_DB_PREFIX="merc_mutation_w${worker}"
    export MERC_MUTATION_DB_TEMPLATE="${template_names[$((worker - 1))]}"
    export MERC_MUTATION_TIMINGS_FILE="$timing_file"
    export MERC_MUTATION_TEST_STRATEGY="$test_strategy"
    export MERC_MUTATION_PREFLIGHT_CACHE="$preflight_cache"
    export GOMAXPROCS="$go_max_procs"
    exec bash scripts/mutation-test.sh
  ) >"$log" 2>&1 &
  worker_pids+=("$!")
done

failed=0
for ((worker = 1; worker <= workers; worker++)); do
  if ! wait "${worker_pids[$((worker - 1))]}"; then
    echo "parallel-mutation-test: worker $worker failed; tail follows" >&2
    tail -n 60 "${worker_logs[$((worker - 1))]}" >&2 || true
    failed=1
  fi
done

elapsed=$(( $(date +%s) - started ))
for ((worker = 1; worker <= workers; worker++)); do
  worktree="${worktrees[$((worker - 1))]}"
  expected="$(wc -l <"$run_root/shard-$worker.ids" | tr -d ' ')"
  summary="$(grep '^mutation-test:' "${worker_logs[$((worker - 1))]}" | tail -n 1 || true)"
  caught="$(printf '%s\n' "$summary" | sed -n 's/^mutation-test: \([0-9][0-9]*\) caught, 0 survived, 0 stale, 0 infrastructure$/\1/p')"
  if [ "$caught" != "$expected" ]; then
    echo "parallel-mutation-test: worker $worker summary mismatch (expected $expected): $summary" >&2
    failed=1
  fi
  if [ "$(git -C "$worktree" rev-parse HEAD)" != "$candidate" ] ||
     ! git -C "$worktree" diff --quiet ||
     ! git -C "$worktree" diff --cached --quiet ||
     [ -n "$(git -C "$worktree" status --porcelain)" ]; then
    echo "parallel-mutation-test: worker $worker did not restore its exact clean source tree" >&2
    failed=1
  fi
done

if [ "$(git rev-parse HEAD)" != "$candidate" ] ||
   ! git diff --quiet ||
   ! git diff --cached --quiet ||
   [ -n "$(git status --porcelain)" ]; then
  echo "parallel-mutation-test: candidate tree changed while shards ran" >&2
  failed=1
fi
if [ "$elapsed" -gt "$budget_seconds" ]; then
  echo "parallel-mutation-test: wall clock ${elapsed}s exceeded ${budget_seconds}s budget" >&2
  failed=1
fi
if [ "$failed" -ne 0 ]; then
  exit 1
fi

# A timing report is certification input, not candidate evidence. It is written
# outside the frozen tree only after every worker has proven one legitimate
# catch per assigned mutation and restored its exact source bytes.
timing_report="$run_root/mutation-timings.json"
python3 - "$candidate" "$case_count" "$workers" "$elapsed" "$run_root" "$timing_report" "$timings_out" <<'PY'
import json
import os
import sys
from pathlib import Path

candidate, case_count, workers, elapsed, run_root, internal, requested = sys.argv[1:]
case_count = int(case_count)
workers = int(workers)
elapsed = int(elapsed)
root = Path(run_root)
expected = set()
for worker in range(1, workers + 1):
    expected.update(int(value) for value in (root / f"shard-{worker}.ids").read_text().split() if value)
records = []
seen = set()
for worker in range(1, workers + 1):
    path = root / f"worker-{worker}.timings.jsonl"
    if not path.is_file():
        raise SystemExit(f"missing worker timing file: {path}")
    for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        try:
            record = json.loads(line)
        except json.JSONDecodeError as exc:
            raise SystemExit(f"invalid timing record {path}:{line_number}: {exc}") from exc
        case_id = record.get("case_id")
        if case_id not in expected or case_id in seen:
            raise SystemExit(f"timing report has a missing or duplicate mutation case: {case_id!r}")
        if record.get("candidate") != candidate or record.get("result") != "caught" or record.get("pathway") not in {"PURE", "DB"}:
            raise SystemExit(f"timing report has a non-certifying mutation record: {case_id!r}")
        duration = record.get("duration_seconds")
        if not isinstance(duration, (int, float)) or duration < 0:
            raise SystemExit(f"timing report has an invalid duration: {case_id!r}")
        seen.add(case_id)
        records.append(record)
if expected != seen or len(expected) != case_count:
    raise SystemExit(f"timing report coverage mismatch: expected={len(expected)} seen={len(seen)}")
report = {
    "version": 1,
    "candidate": candidate,
    "workers": workers,
    "elapsed_seconds": elapsed,
    "mutations": sorted(records, key=lambda item: item["case_id"]),
}
payload = json.dumps(report, indent=2, sort_keys=True) + "\n"
Path(internal).write_text(payload, encoding="utf-8")
if requested:
    target = Path(requested).resolve()
    temporary = target.with_name(target.name + ".tmp")
    temporary.write_text(payload, encoding="utf-8")
    os.replace(temporary, target)
PY

printf 'parallel-mutation-test: PASS %s caught, 0 survived, 0 stale; %s workers; %ss (budget %ss)\n' \
  "$case_count" "$workers" "$elapsed" "$budget_seconds"
if [ -n "$timings_out" ]; then
  printf 'parallel-mutation-test: timing report %s\n' "$timings_out"
fi
run_ok=1
