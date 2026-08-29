#!/usr/bin/env bash
# Prove agent-review-notes validator rejects the "my cat / vibes / SHIP IT" attack.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/merc-agent-review-gaming.XXXXXX")"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

rsync -a \
  --exclude '.git' \
  --exclude 'src/agent/target' \
  --exclude 'src/control/merc' \
  --exclude '.artifacts' \
  --exclude 'tools' \
  --exclude 'review' \
  --exclude 'diag_images' \
  --exclude 'logo' \
  --exclude 'build' \
  --exclude 'models' \
  --exclude 'codex-handoff-3d-rebuild' \
  --exclude 'clients/web/assets' \
  --exclude 'node_modules' \
  --exclude '*.glb' \
  --exclude '*.zip' \
  --exclude '*.psd' \
"$ROOT/" "$TMP/repo/"
cd "$TMP/repo"

# The excludes above are for speed (the full tree carries multi-GB asset dirs).
# Assert the UNMUTATED copy still passes first: otherwise an over-broad exclude
# would make validate-independent-reviews.py fail on a missing file, and the
# mutation assertion below would pass for entirely the wrong reason.
if ! baseline="$(python3 ops/scripts/validate-independent-reviews.py 2>&1)"; then
  printf 'test-agent-review-gaming: FAIL: unmutated copy does not validate - the rsync excludes are too broad\n%s\n' "$baseline" >&2
  exit 1
fi

python3 - <<'PY'
import json
from pathlib import Path

path = Path("ops/agent-review-notes.json")
doc = json.loads(path.read_text())
for review in doc["reviews"]:
    review["reviewer_track"] = "my cat"
    review["outcome"] = "SHIP IT"
    review["method"] = "I made all of this up in a text editor."
    for finding in review["findings"]:
        finding["evidence"] = "vibes"
doc["method"] = "I made all of this up in a text editor."
path.write_text(json.dumps(doc, indent=2) + "\n")
PY

set +e
output="$(python3 ops/scripts/validate-independent-reviews.py 2>&1)"
status=$?
set -e

if [ "$status" -eq 0 ]; then
  printf 'test-agent-review-gaming: FAIL: validator accepted fabricated reviews\n%s\n' "$output" >&2
  exit 1
fi

printf 'test-agent-review-gaming: PASS (validator rejected my-cat/vibes/SHIP-IT attack, exit=%s)\n' "$status"
printf '%s\n' "$output" | head -n 10
