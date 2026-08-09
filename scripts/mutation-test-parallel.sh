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
# The default deliberately preserves interactive headroom on a 28-core
# development host: up to 6 one-runtime workers, a 25-minute mutation budget,
# and explicit invariant-test contracts. The run fails rather than quietly
# reducing coverage when any worker, mutation, restoration proof, or budget
# fails. Adjust only through explicit environment variables:
#
#   MERC_MUTATION_WORKERS=6               # 1..32; default min(6, CPUs-4)
#   MERC_MUTATION_WALLCLOCK_SECONDS=1500  # default 25 minutes
#   MERC_MUTATION_POSTGRES_PORT_BASE=0     # 0 finds an unused local range
#   MERC_MUTATION_TEST_STRATEGY=adaptive   # adaptive (default), contracts, or full
#   MERC_MUTATION_GOMAXPROCS=1             # per-worker runtime CPU ceiling
#   MERC_MUTATION_KEEP_WORKDIR=1          # retain failed shard logs/worktrees
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
elif [ "$default_workers" -gt 6 ]; then
  default_workers=6
fi
workers="${MERC_MUTATION_WORKERS:-$default_workers}"
budget_seconds="${MERC_MUTATION_WALLCLOCK_SECONDS:-1500}"
postgres_port_base="${MERC_MUTATION_POSTGRES_PORT_BASE:-0}"
test_strategy="${MERC_MUTATION_TEST_STRATEGY:-adaptive}"
go_max_procs="${MERC_MUTATION_GOMAXPROCS:-1}"
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

case_count="${#case_ids[@]}"
if [ "$case_count" -eq 0 ]; then
  echo "parallel mutation test refused: no mutation cases were declared" >&2
  exit 2
fi
if [ "$workers" -gt "$case_count" ]; then
  workers="$case_count"
fi

print_plan() {
  local worker index separator ids
  printf 'parallel-mutation-test: candidate=%s cases=%s workers=%s budget=%ss db=isolated-clusters strategy=%s gomaxprocs=%s\n' \
    "$(git rev-parse HEAD)" "$case_count" "$workers" "$budget_seconds" "$test_strategy" "$go_max_procs"
  worker=1
  while [ "$worker" -le "$workers" ]; do
    index=$((worker - 1))
    ids=""
    separator=""
    while [ "$index" -lt "$case_count" ]; do
      ids="${ids}${separator}${case_ids[$index]}"
      separator=,
      index=$((index + workers))
    done
    printf '  worker %02d: %s\n' "$worker" "$ids"
    worker=$((worker + 1))
  done
}

if [ "$mode" = "plan" ]; then
  print_plan
  exit 0
fi

: "${MERC_TEST_DATABASE_URL:?parallel mutation testing needs a database}"
for command in initdb pg_ctl createdb; do
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
declare -a worktrees=()
declare -a worker_pids=()
declare -a worker_logs=()
declare -a cluster_dirs=()
declare -a cluster_ports=()
run_ok=0

cleanup() {
  local worktree cluster
  trap - EXIT INT TERM
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

cluster_port_base="$(find_cluster_port_base)"

for ((worker = 1; worker <= workers; worker++)); do
  shard_file="$run_root/shard-$worker.ids"
  index=$((worker - 1))
  while [ "$index" -lt "$case_count" ]; do
    printf '%s\n' "${case_ids[$index]}" >>"$shard_file"
    index=$((index + workers))
  done
  worktree="$run_root/worker-$worker"
  git worktree add --detach "$worktree" "$candidate" >/dev/null
  worktrees+=("$worktree")
  if [ "$(git -C "$worktree" rev-parse HEAD)" != "$candidate" ] ||
     [ -n "$(git -C "$worktree" status --porcelain)" ]; then
    echo "worker $worker did not start at a clean exact candidate" >&2
    exit 1
  fi
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

started="$(date +%s)"
for ((worker = 1; worker <= workers; worker++)); do
  worktree="${worktrees[$((worker - 1))]}"
  port="${cluster_ports[$((worker - 1))]}"
  shard_file="$run_root/shard-$worker.ids"
  shard_ids="$(paste -sd, "$shard_file")"
  log="$run_root/worker-$worker.log"
  worker_logs+=("$log")
  (
    cd "$worktree"
    export MERC_TEST_DATABASE_URL="postgres://cx@127.0.0.1:${port}/cx?sslmode=disable"
    export MERC_MUTATION_CASE_IDS="$shard_ids"
    export MERC_MUTATION_DB_PREFIX="merc_mutation_w${worker}"
    export MERC_MUTATION_TEST_STRATEGY="$test_strategy"
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
  caught="$(printf '%s\n' "$summary" | sed -n 's/^mutation-test: \([0-9][0-9]*\) caught, 0 survived, 0 stale$/\1/p')"
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

printf 'parallel-mutation-test: PASS %s caught, 0 survived, 0 stale; %s workers; %ss (budget %ss)\n' \
  "$case_count" "$workers" "$elapsed" "$budget_seconds"
run_ok=1
