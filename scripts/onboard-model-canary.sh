#!/usr/bin/env bash
# Canary check for external-model onboarding: the real path must ADMIT a good
# model and REFUSE each bad one. A gate that only ever says yes is not a gate,
# so the refusals are asserted, not just the admission.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

ENDPOINT="${MERC_REALTIME_UPSTREAM:?MERC_REALTIME_UPSTREAM is required}"
export MERC_ONBOARD_API_KEY="${MERC_REALTIME_UPSTREAM_KEY:?MERC_REALTIME_UPSTREAM_KEY is required}"
ALIAS="${MERC_ONBOARD_ALIAS:-cx-chat-1b}"
REV=aa8e72537993ba99e69dfaafa59ed015b17504d1
OUT=evidence/onboarding/canary.json
mkdir -p evidence/onboarding

# Also runs the boot-time policy tests, so the JSON-declared half and the
# live-runtime half are both covered by this one lane.
( cd control && go test -count=1 \
    -run 'TestShippedCatalogueSatisfiesOnboardingPolicy|TestCatalogueAttribution|TestOnboardingPolicyRefuses' . >/dev/null )

python3 scripts/onboard-model.py --endpoint "$ENDPOINT" --alias "$ALIAS" \
  --license Apache-2.0 --license-url https://huggingface.co/Qwen/Qwen2.5-3B-Instruct \
  --repo Qwen/Qwen2.5-3B-Instruct --revision "$REV" --samples 3 --out "$OUT" >/dev/null

python3 - "$OUT" <<'PY'
import json,sys
r=json.load(open(sys.argv[1]))
assert r["admitted"], "a compliant model was refused"
assert r["measured"]["decode_tokens_per_sec_median"] > 0, "benchmark produced no throughput"
assert all(s["passed"] for s in r["stages"].values()), "a stage failed"
PY

refuse() {
  local desc="$1"; shift
  if python3 scripts/onboard-model.py --endpoint "$ENDPOINT" \
       --license-url https://x --repo r/m --samples 1 \
       --out /tmp/merc-onboard-refuse.json "$@" >/dev/null 2>&1; then
    echo "onboarding accepted a model it must refuse: $desc" >&2; exit 1
  fi
}
refuse "non-commercial licence" --alias "$ALIAS" --license CC-BY-NC-4.0 --revision "$REV"
refuse "remote_code"            --alias "$ALIAS" --license Apache-2.0 --remote-code --revision "$REV"
refuse "alias not served"       --alias definitely-not-served --license Apache-2.0 --revision "$REV"
refuse "unpinned revision"      --alias "$ALIAS" --license Apache-2.0 --revision main

echo "external-model onboarding: 1 admitted with a measured benchmark, 4 refused"
