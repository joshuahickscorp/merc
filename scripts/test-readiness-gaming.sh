#!/usr/bin/env bash
# Prove validate-readiness.py fails closed when paperwork is maxed without receipts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cx-readiness-gaming.XXXXXX")"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# Copy the tree without .git / bulky caches so the mutation is isolated.
rsync -a \
  --exclude '.git' \
  --exclude 'agent/target' \
  --exclude 'control/cx' \
  --exclude '.artifacts' \
  --exclude 'tools' \
  --exclude 'review' \
  --exclude 'diag_images' \
  --exclude 'logo' \
  --exclude 'build' \
  --exclude 'models' \
  --exclude 'codex-handoff-3d-rebuild' \
  --exclude 'web/assets' \
  --exclude 'node_modules' \
  --exclude '*.glb' \
  --exclude '*.zip' \
  --exclude '*.psd' \
"$ROOT/" "$TMP/repo/"
cd "$TMP/repo"

# The excludes above are for speed (the full tree carries multi-GB asset dirs).
# Assert the UNMUTATED copy still passes first: otherwise an over-broad exclude
# would make validate-readiness.py fail on a missing file, and the
# mutation assertion below would pass for entirely the wrong reason.
if ! baseline="$(python3 scripts/validate-readiness.py 2>&1)"; then
  printf 'test-readiness-gaming: FAIL: unmutated copy does not validate - the rsync excludes are too broad\n%s\n' "$baseline" >&2
  exit 1
fi

python3 - <<'PY'
import json
from pathlib import Path

readiness = json.loads(Path("ops/readiness.json").read_text())
decision = json.loads(Path("ops/go-no-go.json").read_text())

# Classic gaming attack: type every domain to its maximum, clear blockers, claim GO.
for domain in readiness["weighted_score"]["domains"]:
    domain["earned"] = domain["possible"]
readiness["weighted_score"]["earned"] = 100
readiness["severity"]["target_scope_open_p0"] = 0
readiness["severity"]["target_scope_open_p1"] = 0
decision["readiness_score"] = 100
decision["open_p0"] = []
decision["open_p1"] = []
decision["decisions"]["supervised_stripe_test_mode_private_canary"] = "GO"
# Keep live money prohibited so only the score/receipt path is under test.

Path("ops/readiness.json").write_text(json.dumps(readiness, indent=2) + "\n")
Path("ops/go-no-go.json").write_text(json.dumps(decision, indent=2) + "\n")
PY

set +e
output="$(python3 scripts/validate-readiness.py 2>&1)"
status=$?
set -e

if [ "$status" -eq 0 ]; then
  printf 'test-readiness-gaming: FAIL: validator accepted maxed paperwork without receipts\n%s\n' "$output" >&2
  exit 1
fi

printf 'test-readiness-gaming: PASS (validator rejected maxed-domain GO attack, exit=%s)\n' "$status"
printf '%s\n' "$output" | head -n 20
