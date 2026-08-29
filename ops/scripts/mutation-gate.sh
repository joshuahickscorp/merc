#!/usr/bin/env bash
# Select the normal mutation gate without ever changing the certification rules.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

usage() {
  cat <<'EOF'
usage: bash ops/scripts/mutation-gate.sh fast|authority|full|deep

fast       changed source targets only; hard budget 120 seconds
authority  every mutation in the changed money/evidence/runtime authority domain; hard budget 300 seconds
full       all 125 mutations exactly once; hard budget 299 seconds by default
deep       full-suite mutation redundancy for an isolated nightly/release candidate

Set MERC_MUTATION_BASE=<commit> to select a diff base for fast/authority.
Set MERC_MUTATION_AFFECTED_DOMAINS=a,b to override automatic domain selection.
deep additionally requires MERC_MUTATION_DEEP_ACK=1.
EOF
}

mode="${1:-}"
case "$mode" in
  fast|authority|full|deep) ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

[ -f ops/scripts/mutation-manifest.json ] || {
  echo "mutation gate requires ops/scripts/mutation-manifest.json" >&2
  exit 2
}
python3 ops/scripts/mutation-manifest.py --root . --validate >/dev/null

base="${MERC_MUTATION_BASE:-HEAD~1}"
if [ "$mode" = "fast" ] || [ "$mode" = "authority" ]; then
  git rev-parse --verify "${base}^{commit}" >/dev/null 2>&1 || {
    echo "mutation gate cannot resolve MERC_MUTATION_BASE=$base" >&2
    exit 2
  }
fi

selection="$(python3 - "$mode" "$base" "${MERC_MUTATION_AFFECTED_DOMAINS:-}" <<'PY'
import json
import subprocess
import sys
from pathlib import Path

mode, base, override = sys.argv[1:]
manifest = json.loads(Path("ops/scripts/mutation-manifest.json").read_text(encoding="utf-8"))
mutations = manifest["mutations"]
if mode in {"full", "deep"}:
    chosen = mutations
    reason = "all declared mutations"
else:
    changed = subprocess.check_output(["git", "diff", "--name-only", f"{base}...HEAD"], text=True).splitlines()
    framework_changed = any(
        path.startswith("ops/scripts/mutation-") or path in {"src/control/authority_callgraph_test.go", "src/control/money_authority_guard_test.go"}
        for path in changed
    )
    if override:
        domains = {value.strip() for value in override.split(",") if value.strip()}
    else:
        changed_targets = set(changed)
        domains = {item["authority_domain"] for item in mutations if item["source_target"] in changed_targets}
    if framework_changed:
        chosen = mutations
        reason = "mutation/authority framework changed"
    elif mode == "fast":
        targets = set(changed)
        chosen = [item for item in mutations if item["source_target"] in targets]
        reason = "changed mutation source targets"
    else:
        chosen = [item for item in mutations if item["authority_domain"] in domains]
        reason = "affected authority domains: " + (",".join(sorted(domains)) if domains else "none")
if len({item["id"] for item in chosen}) != len(chosen):
    raise SystemExit("mutation gate selected duplicate IDs")
print(",".join(str(item["id"]) for item in chosen))
print(reason)
PY
)"
case_ids="$(printf '%s\n' "$selection" | sed -n '1p')"
reason="$(printf '%s\n' "$selection" | sed -n '2p')"

if [ -z "$case_ids" ]; then
  printf 'mutation-%s: PASS no declared mutations affected (%s); not a full certification\n' "$mode" "$reason"
  exit 0
fi

case_count="$(printf '%s\n' "$case_ids" | tr ',' '\n' | wc -l | tr -d ' ')"
budget=""
strategy="adaptive"
default_workers=""
case "$mode" in
  fast)
    budget=120
    default_workers=4
    ;;
  authority)
    budget=300
    default_workers=6
    ;;
  full)
    # Measured full-campaign SLO: this remains overrideable for a deliberately
    # smaller host, but the normal calibrated path must fail rather than drift
    # past five minutes unnoticed.
    budget="${MERC_MUTATION_WALLCLOCK_SECONDS:-299}"
    default_workers=16
    ;;
  deep)
    [ "${MERC_MUTATION_DEEP_ACK:-}" = "1" ] || {
      echo "mutation-deep is intentionally asynchronous/expensive; set MERC_MUTATION_DEEP_ACK=1 in a dedicated validation worktree" >&2
      exit 2
    }
    budget="${MERC_MUTATION_WALLCLOCK_SECONDS:-14400}"
    # Oracle whole-suite strategy (not the "full" tier, which uses adaptive).
    # This is the independent serial reference against the contract path.
    strategy=oracle
    default_workers=6
    ;;
esac
if [ "$default_workers" -gt "$case_count" ]; then
  default_workers="$case_count"
fi

printf 'mutation-%s: candidate=%s cases=%s reason=%s budget=%ss\n' \
  "$mode" "$(git rev-parse HEAD)" "$case_count" "$reason" "$budget"
MERC_MUTATION_PARALLEL_CASE_IDS="$case_ids" \
  MERC_MUTATION_WALLCLOCK_SECONDS="$budget" \
  MERC_MUTATION_TEST_STRATEGY="$strategy" \
  MERC_MUTATION_WORKERS="${MERC_MUTATION_WORKERS:-$default_workers}" \
  bash ops/scripts/mutation-test-parallel.sh
