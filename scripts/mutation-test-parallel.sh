#!/usr/bin/env bash
# Parallel, disposable-worktree mutation campaign for a frozen candidate.
#
# The serial mutation runner intentionally edits its own source tree, so it
# must own that tree.  This orchestrator keeps the candidate tree read-only:
# every shard receives a detached worktree at the same exact commit and every
# mutant still receives a fresh disposable PostgreSQL database.  That gives the
# full 84-case campaign useful wall-clock parallelism without letting concurrent
# tests share source bytes or database state.
#
# The default is deliberately aggressive enough for a 28-core development host:
# 16 workers, a 25-minute mutation budget.  The run fails rather than quietly
# reducing coverage when any worker, mutation, restoration proof, or budget
# fails.  Adjust only through explicit environment variables:
#
#   MERC_MUTATION_WORKERS=16              # 1..32; default 16
#   MERC_MUTATION_WALLCLOCK_SECONDS=1500  # default 25 minutes
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

workers="${MERC_MUTATION_WORKERS:-16}"
budget_seconds="${MERC_MUTATION_WALLCLOCK_SECONDS:-1500}"
if ! [[ "$workers" =~ ^[1-9][0-9]*$ ]] || [ "$workers" -gt 32 ]; then
  echo "MERC_MUTATION_WORKERS must be an integer from 1 through 32" >&2
  exit 2
fi
if ! [[ "$budget_seconds" =~ ^[1-9][0-9]*$ ]]; then
  echo "MERC_MUTATION_WALLCLOCK_SECONDS must be a positive integer" >&2
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
  printf 'parallel-mutation-test: candidate=%s cases=%s workers=%s budget=%ss\n' \
    "$(git rev-parse HEAD)" "$case_count" "$workers" "$budget_seconds"
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
run_ok=0

cleanup() {
  local worktree
  trap - EXIT INT TERM
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

started="$(date +%s)"
for ((worker = 1; worker <= workers; worker++)); do
  worktree="${worktrees[$((worker - 1))]}"
  shard_file="$run_root/shard-$worker.ids"
  shard_ids="$(paste -sd, "$shard_file")"
  log="$run_root/worker-$worker.log"
  worker_logs+=("$log")
  (
    cd "$worktree"
    export MERC_TEST_DATABASE_URL
    export MERC_MUTATION_CASE_IDS="$shard_ids"
    export MERC_MUTATION_DB_PREFIX="merc_mutation_w${worker}"
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
