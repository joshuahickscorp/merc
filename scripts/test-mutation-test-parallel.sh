#!/usr/bin/env bash
# Fast structural checks for the disposable-worktree mutation orchestrator.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

plan="$(MERC_MUTATION_WORKERS=16 MERC_MUTATION_WALLCLOCK_SECONDS=1500 \
  bash scripts/mutation-test-parallel.sh --plan)"
printf '%s\n' "$plan"

header="$(printf '%s\n' "$plan" | sed -n '1p')"
printf '%s\n' "$header" | grep -Eq 'cases=84 workers=16 budget=1500s db=isolated-clusters strategy=adaptive gomaxprocs=1 sharding=weighted-p90-pure-warmup' || {
  echo "parallel mutation plan has an unexpected header" >&2
  exit 1
}

ids="$(printf '%s\n' "$plan" | sed -n 's/^  worker [0-9][0-9]: \(.*\) (p90_load=.*$/\1/p' | tr ',' '\n' | sort -n)"
count="$(printf '%s\n' "$ids" | sed '/^$/d' | wc -l | tr -d ' ')"
unique="$(printf '%s\n' "$ids" | sed '/^$/d' | uniq | wc -l | tr -d ' ')"
first="$(printf '%s\n' "$ids" | sed -n '1p')"
last="$(printf '%s\n' "$ids" | sed -n '$p')"
if [ "$count" != 84 ] || [ "$unique" != 84 ] || [ "$first" != 1 ] || [ "$last" != 84 ]; then
  echo "parallel mutation plan drops or duplicates declared cases" >&2
  exit 1
fi

# A cold database fallback is deliberately not allowed to lead a shard when
# that shard has a pure mutation available to warm its isolated checkout.
python3 - "$plan" <<'PY'
import json
import re
import sys

manifest = json.load(open("scripts/mutation-manifest.json", encoding="utf-8"))
classes = {item["id"]: item["class"] for item in manifest["mutations"]}
for line in sys.argv[1].splitlines():
    match = re.fullmatch(r"  worker \d+: ([0-9,]+) \(p90_load=.*", line)
    if not match:
        continue
    ids = [int(value) for value in match.group(1).split(",")]
    if any(classes[case_id] == "PURE" for case_id in ids) and classes[ids[0]] != "PURE":
        raise SystemExit(f"shard did not start with a pure warmup: {ids}")
PY

# The serial runner must execute the planner's supplied sequence, rather than
# treating it as a set and silently falling back to declaration order. This is
# what makes the pure warmup at the start of each worker shard real.
ordered_cases="$(MERC_TEST_DATABASE_URL='postgres://cx:cx@127.0.0.1:1/cx?sslmode=disable' \
  MERC_MUTATION_CASE_IDS=66,53,61,69 MERC_MUTATION_DRY_ORDER=1 \
  bash scripts/mutation-test.sh)"
[ "$ordered_cases" = $'66\n53\n61\n69' ] || {
  echo "mutation worker did not honor its planned case order: $ordered_cases" >&2
  exit 1
}

if MERC_MUTATION_WORKERS=33 bash scripts/mutation-test-parallel.sh --plan >/dev/null 2>&1; then
  echo "parallel mutation runner accepted an unsafe worker count" >&2
  exit 1
fi

subset="$(MERC_MUTATION_PARALLEL_CASE_IDS=1,25,49 MERC_MUTATION_WORKERS=16 \
  bash scripts/mutation-test-parallel.sh --plan)"
subset_header="$(printf '%s\n' "$subset" | sed -n '1p')"
printf '%s\n' "$subset_header" | grep -Eq 'cases=3 workers=3 ' || {
  echo "parallel mutation subset did not preserve exactly its requested cases" >&2
  exit 1
}
subset_ids="$(printf '%s\n' "$subset" | sed -n 's/^  worker [0-9][0-9]: \(.*\) (p90_load=.*$/\1/p' | tr ',' '\n' | sort -n | tr '\n' ',')"
[ "$subset_ids" = "1,25,49," ] || {
  echo "parallel mutation subset contains the wrong cases: $subset_ids" >&2
  exit 1
}
if MERC_MUTATION_PARALLEL_CASE_IDS=85 bash scripts/mutation-test-parallel.sh --plan >/dev/null 2>&1; then
  echo "parallel mutation runner accepted an unknown subset case" >&2
  exit 1
fi

# Template migration is candidate work, not per-mutant work. The runner must
# seed one exact-candidate template and restore that immutable snapshot into
# the remaining isolated worker clusters.
rg --fixed-strings 'pg_dump --host=127.0.0.1' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'pg_restore --host=127.0.0.1' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'prepare_seed_template' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'restore_template_snapshot' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'candidate_baseline_pid' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'cd "${worktrees[0]}/control"' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'git -C "$worktree" lfs checkout' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'mutation-preflight-cache.py' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'MERC_MUTATION_PREFLIGHT_CACHE' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'preflight-db.json' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'default_workers=16' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'weighted-p90-pure-warmup' scripts/mutation-test-parallel.sh >/dev/null

echo "test-mutation-test-parallel: PASS all 84 cases are uniquely sharded"
