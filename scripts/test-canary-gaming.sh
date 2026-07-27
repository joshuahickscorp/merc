#!/usr/bin/env bash
# The private canary decides whether merc may make a public capability claim.
# So the failure that matters is not a lane failing -- it is the canary being
# talked into reporting a lane proven that never ran. These are the ways someone
# would try.
set -euo pipefail
cd "$(dirname "$0")/.."
fail() { echo "GAMING GATE FAILED: $1" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
out="$tmp/canary.json"

# 1. A fabricated GPU endpoint must not satisfy the capability. If merc cannot
#    reach and authenticate to a runtime, the lane is blocked -- pointing at a
#    dead host is exactly how a canary gets talked into passing.
MERC_GPU_ENDPOINT="http://127.0.0.1:9" RUNPOD_API_KEY="not-a-key" \
  python3 scripts/private-canary.py --out "$out" >/dev/null 2>&1 || true
python3 - "$out" <<'PY' || fail "an unreachable GPU endpoint satisfied the gpu_runtime capability"
import json,sys
d=json.load(open(sys.argv[1]))
gpu=d["capabilities"]["gpu_runtime"]
sys.exit(0 if not gpu["present"] else 1)
PY

# 2. No lane may report CANARY_PROVEN while a capability it needs is missing.
python3 - "$out" <<'PY' || fail "a lane reported CANARY_PROVEN with a missing capability"
import json,sys
d=json.load(open(sys.argv[1]))
absent={k for k,v in d["capabilities"].items() if not v["present"]}
for lane in d["lanes"]:
    if lane["status"]=="CANARY_PROVEN" and set(lane.get("missing_capabilities",[])) & absent:
        sys.exit(1)
sys.exit(0)
PY

# 3. public_capability_allowed must be false unless EVERY lane is proven.
python3 - "$out" <<'PY' || fail "public capability was allowed without every lane proven"
import json,sys
d=json.load(open(sys.argv[1]))
ok = d["lanes_canary_proven"] == d["lanes_total"]
sys.exit(0 if d["public_capability_allowed"] == ok else 1)
PY

# 4. A lane that only walks part of the chain must report TESTED, never
#    CANARY_PROVEN. This is the distinction the whole status vocabulary exists
#    for, and it is the easiest one to erode.
python3 - <<'PY' || fail "a partial-chain lane is declared CANARY_PROVEN-eligible"
import importlib.util,sys
spec=importlib.util.spec_from_file_location("canary","scripts/private-canary.py")
m=importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
caps={k:(True,"forced") for k in m.CAPABILITIES}
for lane in m.LANES:
    if lane["full_path"]:
        continue
    r=m.run_lane(lane, caps, 600)
    if r["status"]=="CANARY_PROVEN":
        print("partial lane promoted itself:", lane["id"]); sys.exit(1)
sys.exit(0)
PY

echo "canary-gaming: PASS (unreachable runtime refused, no lane self-promotes, public claim gated on every lane)"
