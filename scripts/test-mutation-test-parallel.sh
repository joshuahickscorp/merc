#!/usr/bin/env bash
# Fast structural checks for the disposable-worktree mutation orchestrator.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

plan="$(MERC_MUTATION_WORKERS=16 MERC_MUTATION_WALLCLOCK_SECONDS=1500 \
  bash scripts/mutation-test-parallel.sh --plan)"
printf '%s\n' "$plan"

header="$(printf '%s\n' "$plan" | sed -n '1p')"
printf '%s\n' "$header" | grep -Eq 'cases=84 workers=16 budget=1500s db=isolated-clusters strategy=adaptive gomaxprocs=1' || {
  echo "parallel mutation plan has an unexpected header" >&2
  exit 1
}

ids="$(printf '%s\n' "$plan" | sed -n 's/^  worker [0-9][0-9]: //p' | tr ',' '\n' | sort -n)"
count="$(printf '%s\n' "$ids" | sed '/^$/d' | wc -l | tr -d ' ')"
unique="$(printf '%s\n' "$ids" | sed '/^$/d' | uniq | wc -l | tr -d ' ')"
first="$(printf '%s\n' "$ids" | sed -n '1p')"
last="$(printf '%s\n' "$ids" | sed -n '$p')"
if [ "$count" != 84 ] || [ "$unique" != 84 ] || [ "$first" != 1 ] || [ "$last" != 84 ]; then
  echo "parallel mutation plan drops or duplicates declared cases" >&2
  exit 1
fi

if MERC_MUTATION_WORKERS=33 bash scripts/mutation-test-parallel.sh --plan >/dev/null 2>&1; then
  echo "parallel mutation runner accepted an unsafe worker count" >&2
  exit 1
fi

echo "test-mutation-test-parallel: PASS all 84 cases are uniquely sharded"
