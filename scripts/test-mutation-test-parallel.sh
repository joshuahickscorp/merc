#!/usr/bin/env bash
# Fast structural checks for the disposable-worktree mutation orchestrator.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

plan="$(MERC_MUTATION_WORKERS=16 MERC_MUTATION_WALLCLOCK_SECONDS=1500 \
  bash scripts/mutation-test-parallel.sh --plan)"
printf '%s\n' "$plan"

header="$(printf '%s\n' "$plan" | sed -n '1p')"
printf '%s\n' "$header" | grep -Eq 'cases=104 workers=16 budget=1500s db=isolated-clusters strategy=adaptive gomaxprocs=1 sharding=weighted-p90-pure-warmup' || {
  echo "parallel mutation plan has an unexpected header" >&2
  exit 1
}

ids="$(printf '%s\n' "$plan" | sed -n 's/^  worker [0-9][0-9]: \(.*\) (p90_load=.*$/\1/p' | tr ',' '\n' | sort -n)"
count="$(printf '%s\n' "$ids" | sed '/^$/d' | wc -l | tr -d ' ')"
unique="$(printf '%s\n' "$ids" | sed '/^$/d' | uniq | wc -l | tr -d ' ')"
first="$(printf '%s\n' "$ids" | sed -n '1p')"
last="$(printf '%s\n' "$ids" | sed -n '$p')"
if [ "$count" != 104 ] || [ "$unique" != 104 ] || [ "$first" != 1 ] || [ "$last" != 104 ]; then
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
if MERC_MUTATION_PARALLEL_CASE_IDS=105 bash scripts/mutation-test-parallel.sh --plan >/dev/null 2>&1; then
  echo "parallel mutation runner accepted an unknown subset case" >&2
  exit 1
fi

# The database aggregate is exactly partitioned, not sampled. Derive the same
# full source set as the runner and prove two calls produce the same pair of
# disjoint, exactly complete lexical round-robin selectors.
sources_file="$(mktemp "${TMPDIR:-/tmp}/merc-mutation-preflight-sources.XXXXXX")"
trap 'rm -f "$sources_file"' EXIT
MERC_MUTATION_LIST=1 MERC_MUTATION_LIST_DETAIL=1 bash scripts/mutation-test.sh |
  awk -F '\t' '{ print $2 }' | LC_ALL=C sort -u >"$sources_file"
whole_selector="$(python3 scripts/mutation-preflight-cache.py \
  --root . --sources "$sources_file" --selector)"
selector_shards_one="$(python3 scripts/mutation-preflight-cache.py \
  --root . --sources "$sources_file" --selector-shards 2)"
selector_shards_two="$(python3 scripts/mutation-preflight-cache.py \
  --root . --sources "$sources_file" --selector-shards 2)"
[ "$selector_shards_one" = "$selector_shards_two" ] || {
  echo "database preflight selector sharding is not deterministic" >&2
  exit 1
}
python3 - "$whole_selector" "$selector_shards_one" <<'PY'
import sys


def names(selector: str) -> list[str]:
    if not selector.startswith("^(") or not selector.endswith(")$"):
        raise SystemExit(f"selector is not exactly anchored: {selector!r}")
    result = selector[2:-2].split("|")
    if not result or any(not name for name in result):
        raise SystemExit("selector contains an empty invariant name")
    return result


whole = names(sys.argv[1])
lanes: dict[int, list[str]] = {}
for line in sys.argv[2].splitlines():
    raw_lane, selector = line.split("\t", 1)
    lane = int(raw_lane)
    if lane in lanes:
        raise SystemExit(f"selector output repeated lane {lane}")
    lanes[lane] = names(selector)
if sorted(lanes) != [1, 2]:
    raise SystemExit(f"selector output lanes are not 1 and 2: {sorted(lanes)}")
if lanes[1] != whole[0::2] or lanes[2] != whole[1::2]:
    raise SystemExit("selector lanes are not stable lexical round-robin")
flattened = lanes[1] + lanes[2]
if len(flattened) != len(set(flattened)) or set(flattened) != set(whole):
    raise SystemExit("selector lanes are not a disjoint complete union")
PY

# Template migration is candidate work, not per-mutant work. The runner must
# seed one exact-candidate template and restore that immutable snapshot into
# the remaining isolated worker clusters.
rg --fixed-strings 'pg_dump --host=127.0.0.1' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'pg_restore --host=127.0.0.1' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'prepare_seed_template' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'restore_template_snapshot' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'candidate_baseline_pid' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'preflight_db_lanes=2' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'proof-candidate-baseline' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'proof-aggregate-unit' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings -- '--selector-shards "$preflight_db_lanes"' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'preflight-db-$lane.json' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'merc_mutation_preflight_w${lane}' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'preflight_db_pids' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'terminate_pid_tree' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'stop_background_pids' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'git -C "$worktree" lfs checkout' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'mutation-preflight-cache.py' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'MERC_MUTATION_PREFLIGHT_CACHE' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'preflight-db.json' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'default_workers=16' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'weighted-p90-pure-warmup' scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'MERC_TEST_S3_' scripts/mutation-preflight-cache.py >/dev/null
rg --fixed-strings 'newArtifactHarness(' scripts/mutation-preflight-cache.py >/dev/null
rg --fixed-strings 'NewStorageFromEnv(' scripts/mutation-preflight-cache.py >/dev/null
rg --fixed-strings 'NewStorage(' scripts/mutation-preflight-cache.py >/dev/null

python3 - <<'PY'
from pathlib import Path

script = Path("scripts/mutation-test-parallel.sh").read_text(encoding="utf-8")


def before(first: str, second: str) -> None:
    left = script.find(first)
    right = script.find(second)
    if left < 0 or right < 0 or left >= right:
        raise SystemExit(f"required runner order is absent: {first!r} before {second!r}")


# Cleanup must reap every possible database user before stopping clusters.
before("stop_background_pids \\", 'pg_ctl -D "$cluster" -m fast')
# Lanes own separate files until every lane status has been observed.
before('wait "${preflight_db_pids[$((lane - 1))]}"', 'cat "$log" >>"$run_root/preflight-db.json"')
# The cache is made only from the completed stable merge.
before('cat "$log" >>"$run_root/preflight-db.json"', '--cache "$preflight_cache" --create')
# Exact-clean proofs precede source-mutating worker launch.
before('assert_exact_clean_worktree "$aggregate_unit_worktree"', 'export MERC_MUTATION_CASE_IDS="$shard_ids"')
PY

echo "test-mutation-test-parallel: PASS all 104 cases are uniquely sharded"
