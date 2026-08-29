#!/usr/bin/env bash
# The capability inventory must never create candidate canary authority. Its
# commands and retained receipts can prove TESTED or REAL_RUNTIME_PROVEN only;
# exact-commit CANARY_PROVEN belongs to the GO-closure rehearsal.
set -euo pipefail
cd "$(dirname "$0")/../.."
fail() { echo "GAMING GATE FAILED: $1" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
out="$tmp/canary.json"

# 1. A fabricated GPU endpoint must not satisfy the capability. Also covers
#    real_inference_runtime, which is a separate capability -- llama.cpp on
#    Metal satisfies that one and no CUDA lane may borrow it. If merc cannot
#    reach and authenticate to a runtime, the lane is blocked -- pointing at a
#    dead host is exactly how a canary gets talked into passing.
MERC_GPU_ENDPOINT="http://127.0.0.1:9" RUNPOD_API_KEY="not-a-key" \
MERC_REALTIME_UPSTREAM="http://127.0.0.1:9/v1" MERC_REALTIME_UPSTREAM_KEY="not-a-key" \
  python3 ops/scripts/private-canary.py --out "$out" >/dev/null 2>&1 || true

python3 - "$out" <<'PY' || fail "inventory schema or unreachable runtime handling is invalid"
import json,sys
d=json.load(open(sys.argv[1]))
ok=(d["schema_version"]==2 and d["kind"]=="merc_capability_inventory" and
    d["candidate_canary_authority"]=="ops/scripts/go-closure-canary-rehearsal.sh" and
    not d["capabilities"]["real_inference_runtime"]["present"])
sys.exit(0 if ok else 1)
PY
python3 - "$out" <<'PY' || fail "an unreachable GPU endpoint satisfied the cuda_runtime capability"
import json,sys
d=json.load(open(sys.argv[1]))
gpu=d["capabilities"]["cuda_runtime"]
sys.exit(0 if not gpu["present"] else 1)
PY

# 2. This inventory has no CANARY_PROVEN minting path, even when partial
#    commands are replaced by /usr/bin/true and every capability is forced on.
python3 - <<'PY' || fail "a partial command minted CANARY_PROVEN"
import copy,importlib.util,sys
spec=importlib.util.spec_from_file_location("canary","ops/scripts/private-canary.py")
m=importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
caps={k:(True,"forced") for k in m.CAPABILITIES}
for original in m.LANES:
    lane=copy.deepcopy(original)
    lane["cmd"]=["/usr/bin/true"]
    lane["cwd"]="."
    r=m.run_lane(lane,caps,10)
    if r["status"]=="CANARY_PROVEN":
        print("lane self-promoted:",lane["id"]); sys.exit(1)
sys.exit(0)
PY

# 3. A fabricated file outside the tracked repository cannot promote even to
#    REAL_RUNTIME_PROVEN, regardless of how many booleans it sets.
python3 - <<'PY' || fail "untracked fabricated evidence was trusted"
import copy,importlib.util,json,tempfile
spec=importlib.util.spec_from_file_location("canary","ops/scripts/private-canary.py")
m=importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
lane=copy.deepcopy(m.LANES[0])
with tempfile.NamedTemporaryFile("w",suffix=".json") as f:
    json.dump({"schema_version":1,"kind":"real_runtime_end_to_end",
               "commit":"0"*40,"lane":"batch_embeddings",
               "evidence_level":"REAL_RUNTIME_PROVEN",
               "public_claim_allowed":False,
               "runtime":{"kind":"REAL","engine":"fake","hardware":"fake"},
               "chain":{k:True for k in (*m.COMMON_CHAIN_FIELDS,"result")},
               "money_usd":{"buyer_charge":-1,"supplier_credit":0.5,
                            "platform_take":0.5}},f)
    f.flush()
    lane["retained_evidence"]["path"]=f.name
    lane["cmd"]=["/usr/bin/true"]; lane["cwd"]="."
    caps={k:(True,"forced") for k in m.CAPABILITIES}
    r=m.run_lane(lane,caps,10)
    if r["status"]!="TESTED" or r["retained_evidence"]["status"]!="REJECTED":
        raise SystemExit(f"fabricated receipt promoted: {r}")
PY

# 4. The committed historical receipt may preserve REAL_RUNTIME_PROVEN, but its
#    detail must explicitly deny candidate binding and public-claim authority.
python3 - <<'PY' || fail "committed retained runtime evidence did not validate safely"
import copy,importlib.util
spec=importlib.util.spec_from_file_location("canary","ops/scripts/private-canary.py")
m=importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
lane=copy.deepcopy(m.LANES[0])
lane["cmd"]=["/usr/bin/true"]; lane["cwd"]="."
caps={k:(True,"forced") for k in m.CAPABILITIES}
r=m.run_lane(lane,caps,10)
e=r.get("retained_evidence",{})
if not (r["status"]=="REAL_RUNTIME_PROVEN" and
        e.get("candidate_bound") is False and
        e.get("public_claim_allowed") is False and
        len(e.get("sha256",""))==64):
    raise SystemExit(f"retained evidence boundary invalid: {r}")
PY

# 5. Missing current capabilities stay EXTERNALLY_BLOCKED even if historical
#    evidence validates.
python3 - "$out" <<'PY' || fail "a lane escaped a missing current capability"
import json,sys
d=json.load(open(sys.argv[1]))
absent={k for k,v in d["capabilities"].items() if not v["present"]}
for lane in d["lanes"]:
    if set(lane.get("missing_capabilities",[])) & absent and lane["status"]!="EXTERNALLY_BLOCKED":
        sys.exit(1)
sys.exit(0)
PY

# 6. Public capability is always false here; only the formal exact-candidate
#    rehearsal can be used by a release decision.
python3 - "$out" <<'PY' || fail "legacy inventory granted public capability"
import json,sys
d=json.load(open(sys.argv[1]))
ok=(d["lanes_canary_proven"]==0 and
    d["all_lanes_canary_proven"] is False and
    d["public_capability_allowed"] is False)
sys.exit(0 if ok else 1)
PY

# 7. The committed inventory must not preserve the superseded 15/21 claim.
jq -e '
  .schema_version == 2 and .kind == "merc_capability_inventory" and
  .lanes_canary_proven == 0 and .all_lanes_canary_proven == false and
  .public_capability_allowed == false and
  .retained_real_runtime_evidence.validated_lanes == 3 and
  .retained_real_runtime_evidence.unique_receipts == 2 and
  .retained_real_runtime_evidence.candidate_bound == false
' evidence/canary/private-canary.json >/dev/null \
  || fail "committed inventory retained an inflated or malformed claim"

echo "canary-gaming: PASS (partial commands capped, fabricated receipts refused, historical evidence capped, public authority absent)"
