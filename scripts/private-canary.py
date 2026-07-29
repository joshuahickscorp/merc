#!/usr/bin/env python3
"""merc capability inventory: exercise every lane and report what was observed.

The goal's CANARY_PROVEN bar is specific -- the exact candidate must produce a
receipt for buyer request, contract, scheduler, REAL worker or runtime, result,
verification, buyer debit, supplier payable, and positive merc contribution.
This legacy inventory does not produce that candidate-bound receipt. It can
therefore report TESTED, or validate retained evidence as REAL_RUNTIME_PROVEN,
but it can never mint CANARY_PROVEN from a capability probe plus a partial test.

Candidate canary authority belongs to scripts/go-closure-canary-rehearsal.sh,
whose receipt is bound to an immutable image and exact candidate commit. Keeping
that authority out of this script prevents a reachable endpoint or a boolean
named ``full_path`` from becoming a receipt for work the command never did.

Exit codes:
  0  every lane reached CANARY_PROVEN (currently impossible by design)
  1  a lane ran and FAILED -- a real defect
  2  one or more lanes lack candidate-bound canary proof (expected)

Usage:
  python3 scripts/private-canary.py --out evidence/canary/private-canary.json
"""

import argparse
import hashlib
import json
import math
import os
import re
import shutil
import socket
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO_PATH = Path(REPO).resolve()


# --------------------------------------------------------------- capabilities
# A capability is something the canary needs from the world. Each returns
# (present, detail). Detail names the missing thing precisely enough to act on.

def cap_database():
    url = os.environ.get("MERC_TEST_DATABASE_URL", "")
    if not url:
        return False, "MERC_TEST_DATABASE_URL is not set"
    return True, "database configured"


def cap_object_store():
    endpoint = os.environ.get("S3_ENDPOINT", "")
    if not endpoint:
        return False, "S3_ENDPOINT is not set"
    host, _, port = endpoint.partition(":")
    try:
        with socket.create_connection((host, int(port or 80)), timeout=3):
            return True, f"object store reachable at {endpoint}"
    except (OSError, ValueError) as exc:
        return False, f"object store unreachable at {endpoint}: {exc}"


def cap_real_inference_runtime():
    """A real OpenAI-compatible engine merc can route realtime work to.

    NOT the same as CUDA. llama.cpp on Metal is a real runtime by every
    definition the goal uses -- it loads real weights, generates real tokens and
    reports real usage -- and a realtime request served by it has genuinely
    walked contract, stream, verification, settlement and receipt. Treating
    "real runtime" and "CUDA" as one capability is what made me report the
    realtime lane blocked when it was not.
    """
    endpoint = os.environ.get("MERC_REALTIME_UPSTREAM", "")
    key = os.environ.get("MERC_REALTIME_UPSTREAM_KEY", "")
    if not endpoint or not key:
        return False, ("no real inference runtime: set MERC_REALTIME_UPSTREAM and "
                       "MERC_REALTIME_UPSTREAM_KEY (llama-server on Metal satisfies this)")
    try:
        req = urllib.request.Request(endpoint.rstrip("/") + "/models",
                                     headers={"authorization": f"Bearer {key}", "User-Agent": MERC_UA})
        with urllib.request.urlopen(req, timeout=10) as resp:
            if resp.status != 200:
                return False, f"inference runtime answered HTTP {resp.status}"
            return True, f"real inference runtime serving at {endpoint}"
    except (urllib.error.URLError, OSError) as exc:
        return False, f"inference runtime unreachable at {endpoint}: {exc}"


# RunPod's edge blocks default library User-Agents. Python's urllib sends
# "Python-urllib/3.x" and gets 403 from the pod proxy even with a valid key and
# URL -- identical request via curl returns 200. A probe that omits this reports
# "no CUDA runtime" while a healthy vLLM is serving.
MERC_UA = "merc-canary/1.0"

def _models_url(endpoint):
    """Model-list URL for an endpoint that may or may not already end in /v1.

    MERC_GPU_ENDPOINT is conventionally written with the /v1 suffix (that is what
    OpenAI clients take as base_url), so blindly appending it produced
    /v1/v1/models and a 404 that looked like an unreachable runtime.
    """
    base = endpoint.rstrip("/")
    return base + "/models" if base.endswith("/v1") else base + "/v1/models"

def cap_cuda_runtime():
    """CUDA supply specifically, for the RunPod-backed pinned vLLM lane.

    Distinct from cap_real_inference_runtime: that lane is about NVIDIA hardware
    merc has admitted and a pinned, digest-addressed vLLM image, and no Apple
    Silicon engine substitutes for it.
    """
    # The ENDPOINT key wins over the RunPod account key. They are different
    # credentials: RUNPOD_API_KEY authenticates to RunPod's control API, while
    # the vLLM server has its own --api-key. Sending the account key to the
    # engine returns 403, which reads as "no CUDA runtime" when one is serving.
    key = os.environ.get("MERC_GPU_API_KEY", "") or os.environ.get("RUNPOD_API_KEY", "")
    endpoint = os.environ.get("MERC_GPU_ENDPOINT", "")
    if not key and not endpoint:
        return False, ("no GPU runtime: set MERC_GPU_ENDPOINT to a reachable pinned "
                       "runtime and RUNPOD_API_KEY (or MERC_GPU_API_KEY) to authenticate. "
                       "No RunPod credential exists on this machine")
    if not key:
        return False, "MERC_GPU_ENDPOINT is set but no API key is configured"
    if not endpoint:
        return False, "a GPU API key is configured but MERC_GPU_ENDPOINT names no runtime"
    try:
        request = urllib.request.Request(_models_url(endpoint),
                                         headers={"authorization": f"Bearer {key}", "User-Agent": MERC_UA})
        with urllib.request.urlopen(request, timeout=10) as resp:
            if resp.status != 200:
                return False, f"GPU runtime answered HTTP {resp.status}"
            return True, f"GPU runtime serving at {endpoint}"
    except (urllib.error.URLError, OSError) as exc:
        return False, f"GPU runtime unreachable at {endpoint}: {exc}"


def cap_stripe_sandbox():
    if not os.environ.get("STRIPE_SECRET_KEY", ""):
        return False, "STRIPE_SECRET_KEY is not set"
    return True, "Stripe sandbox credential configured"


def cap_openai_sdks():
    py = os.environ.get("MERC_TEST_OPENAI_PYTHON", "")
    node_module = os.environ.get("MERC_TEST_OPENAI_NODE_MODULE", "")
    if not py or not node_module:
        return False, ("official OpenAI SDKs not configured "
                       "(MERC_TEST_OPENAI_PYTHON, MERC_TEST_OPENAI_NODE_MODULE)")
    if not os.path.exists(py):
        return False, f"MERC_TEST_OPENAI_PYTHON points at a missing interpreter: {py}"
    return True, "official OpenAI SDKs available"


def cap_local_runtime():
    """A real merc worker on locally supported hardware.

    merc's original supply is Apple Silicon running candle on Metal, and this is
    a real runtime by every definition the goal uses -- the shipped merc-agent
    binary executing a catalogue model and committing a verified result. It is
    not a substitute for CUDA supply, but a lane served by it has genuinely met
    the buyer-request-to-receipt chain, and reporting it as blocked would
    under-report what merc can actually do.
    """
    import platform
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        return False, "not an Apple Silicon host"
    agent = os.path.join(REPO, "agent", "target", "release", "merc-agent")
    if not os.path.exists(agent):
        return False, "merc-agent is not built (cargo build --release --features metal)"
    if not os.environ.get("MERC_LOCAL_WORKER_RUNNING", ""):
        return False, ("a local worker is not running; start merc-agent against the control "
                       "plane and set MERC_LOCAL_WORKER_RUNNING=1")
    return True, "local Apple Silicon worker (candle/Metal) available"


CAPABILITIES = {
    "local_runtime": cap_local_runtime,
    "real_inference_runtime": cap_real_inference_runtime,
    "database": cap_database,
    "object_store": cap_object_store,
    "cuda_runtime": cap_cuda_runtime,
    "stripe_sandbox": cap_stripe_sandbox,
    "openai_sdks": cap_openai_sdks,
}


# --------------------------------------------------------- retained evidence
# Retained receipts can preserve a REAL_RUNTIME_PROVEN fact across runs, but
# they cannot prove that the current candidate image was exercised. The latter
# is intentionally reserved for the formal GO-closure rehearsal.

COMMON_CHAIN_FIELDS = (
    "buyer_request",
    "merc_contract",
    "scheduler",
    "real_worker_runtime",
    "verification",
    "buyer_debit",
    "supplier_payable",
    "positive_merc_contribution",
    "receipt",
)


def _git(*args):
    return subprocess.run(["git", "-C", REPO, *args], capture_output=True, text=True)


def validate_retained_runtime_evidence(spec):
    """Validate a committed historical real-runtime receipt.

    A file supplied from /tmp, an untracked file, a dirty rewrite, a source
    commit outside the current history, or a receipt containing only a
    self-declared status is refused. This is provenance validation, not external
    attestation; the result is capped at REAL_RUNTIME_PROVEN.
    """
    configured = Path(spec["path"])
    candidate = configured if configured.is_absolute() else REPO_PATH / configured
    if candidate.is_symlink():
        return False, "receipt must not be a symlink", None
    try:
        path = candidate.resolve(strict=True)
        relative = path.relative_to(REPO_PATH)
    except (FileNotFoundError, ValueError):
        return False, "receipt is missing or outside the merc repository", None
    if not path.is_file():
        return False, "receipt is not a regular file", None

    rel = str(relative)
    if _git("ls-files", "--error-unmatch", "--", rel).returncode != 0:
        return False, "receipt is not tracked", None
    if _git("diff", "--quiet", "--", rel).returncode != 0:
        return False, "receipt has unstaged changes", None
    if _git("diff", "--cached", "--quiet", "--", rel).returncode != 0:
        return False, "receipt has staged changes", None

    try:
        raw = path.read_bytes()
        receipt = json.loads(raw)
    except (OSError, json.JSONDecodeError) as exc:
        return False, f"receipt cannot be parsed: {exc}", None
    if not isinstance(receipt, dict):
        return False, "receipt root must be an object", None

    commit = receipt.get("commit", "")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        return False, "receipt source commit is not a full lowercase SHA", None
    if _git("merge-base", "--is-ancestor", commit, "HEAD").returncode != 0:
        return False, "receipt source commit is not an ancestor of HEAD", None
    if (receipt.get("schema_version") != 1 or
            receipt.get("kind") != "real_runtime_end_to_end" or
            receipt.get("lane") != spec["lane"] or
            receipt.get("evidence_level") != "REAL_RUNTIME_PROVEN" or
            receipt.get("public_claim_allowed") is not False):
        return False, "receipt identity or evidence ceiling is invalid", None
    runtime = receipt.get("runtime", {})
    if runtime.get("kind") != "REAL" or not runtime.get("engine") or not runtime.get("hardware"):
        return False, "receipt lacks a concrete real runtime identity", None

    chain = receipt.get("chain", {})
    required = (*COMMON_CHAIN_FIELDS, spec["result_field"])
    if not all(chain.get(field) is True for field in required):
        return False, "receipt does not contain the complete observed chain", None

    try:
        if spec["money_shape"] == "batch":
            money = receipt.get("money_usd", {})
            buyer = -float(money.get("buyer_charge", 0))
            supplier = float(money.get("supplier_credit", 0))
            platform = float(money.get("platform_take", 0))
        else:
            money = receipt.get("receipt", {})
            buyer = float(money.get("buyer_charge_usd", 0))
            supplier = float(money.get("supplier_payable_usd", 0))
            platform = float(money.get("platform_margin_usd", 0))
    except (AttributeError, TypeError, ValueError):
        return False, "receipt money fields are not numeric", None
    if not all(math.isfinite(value) for value in (buyer, supplier, platform)):
        return False, "receipt money fields must be finite", None
    if buyer <= 0 or supplier <= 0 or platform <= 0:
        return False, "receipt does not prove positive buyer, supplier, and merc money", None
    if abs(buyer - supplier - platform) > 0.000001:
        return False, "receipt money does not conserve to micro-USD precision", None

    digest = hashlib.sha256(raw).hexdigest()
    detail = {
        "status": "REAL_RUNTIME_PROVEN",
        "path": rel,
        "sha256": digest,
        "source_commit": commit,
        "candidate_bound": False,
        "public_claim_allowed": False,
    }
    return True, "committed historical real-runtime receipt validated", detail


# ---------------------------------------------------------------------- lanes
# Each lane names the capabilities it needs and the command that exercises it.
# A passing command proves TESTED and nothing more. Selected lanes may also
# point to a committed historical receipt; strict validation can lift those
# lanes to REAL_RUNTIME_PROVEN. No field in this list can mint CANARY_PROVEN.

def go_test(pattern):
    return ["go", "test", "-count=1", "-run", pattern, "."]


LANES = [
    {"id": "batch_inference", "needs": ["database", "object_store", "local_runtime"],
     "cmd": go_test("TestJobTaskMoney|TestPayoutMoneyPath"), "cwd": "control",
     "retained_evidence": {
         "path": "evidence/canary/real-runtime-embed.json",
         "lane": "batch_embeddings",
         "result_field": "result",
         "money_shape": "batch",
     },
     "note": "proven end to end against a real Apple Silicon worker "
             "(evidence/canary/real-runtime-embed.json)"},
    {"id": "embeddings", "needs": ["database", "object_store", "local_runtime"],
     "cmd": go_test("TestBillingSchema|TestExactReuse"), "cwd": "control",
     "retained_evidence": {
         "path": "evidence/canary/real-runtime-embed.json",
         "lane": "batch_embeddings",
         "result_field": "result",
         "money_shape": "batch",
     },
     "note": "real 384-dim embeddings computed on Metal, honeypot-verified, settled"},
    {"id": "realtime", "needs": ["database", "real_inference_runtime"],
     "cmd": go_test("TestRealtimeStreamContractVerificationSettlementAndReceipt"),
     "cwd": "control",
     "retained_evidence": {
         "path": "evidence/canary/real-runtime-realtime.json",
         "lane": "openai_compatible_realtime",
         "result_field": "result_stream",
         "money_shape": "realtime",
     },
     "note": "proven against a real llama.cpp/Metal engine: contract, real completion, "
             "VERIFIED receipt, CAPTURED authorization, positive margin "
             "(evidence/canary/real-runtime-realtime.json)"},
    {"id": "runpod_vllm", "needs": ["database", "cuda_runtime"],
     "cmd": go_test("TestCUDAAdmission|TestEngineAdmissible"), "cwd": "control",
     "note": "NVIDIA hardware and a pinned digest-addressed vLLM image; no Apple "
             "Silicon engine substitutes for this; the retained provider receipt is "
             "direct-runtime evidence, not a merc request-to-settlement chain"},
    {"id": "openai_sdk_conformance", "needs": ["database", "openai_sdks"],
     "cmd": go_test("TestRealtimeStreamContractVerificationSettlementAndReceipt"),
     "cwd": "control",
     "note": ("official openai Python 2.48.0 and JS 6.49.0 against merc. Against a REAL "
              "engine the JS client passes all seven capabilities and Python passes six: "
              "parallel_tool_calls fails because llama.cpp cannot parse this model's tool "
              "schema. Against the httptest fake it passed -- the fake was masking a real "
              "incompatibility, which is the whole argument for real-runtime proof")},
    {"id": "object_storage", "needs": ["database", "object_store"],
     "cmd": go_test("TestJobObjectRetention|TestBuyerObjectDeletion|TestBuyerCannotReach"),
     "cwd": "control",
     "note": "retention, deletion, tenant isolation against a live store"},
    {"id": "image_generation", "needs": ["cuda_runtime"],
     "cmd": go_test("TestImage"), "cwd": "control",
     "note": "governance only; no image runtime exists"},
    {"id": "lora", "needs": ["cuda_runtime"],
     "cmd": go_test("TestLoRA"), "cwd": "control",
     "note": "settlement arithmetic only; no trainer, no evaluator dispatch"},
    {"id": "multi_gpu", "needs": ["cuda_runtime"],
     "cmd": go_test("TestAdmittedPlansAlwaysFit|TestHostTopologyFromRegistration"),
     "cwd": "control",
     "note": "admission only; no tensor-parallel runtime has served a request"},
    {"id": "external_model_onboarding", "needs": ["real_inference_runtime"],
     "cmd": ["bash", "scripts/onboard-model-canary.sh"], "cwd": ".",
     "note": ("a real external model taken through policy, identity, smoke, "
              "determinism and a measured benchmark against a live runtime, plus "
              "four refusal cases: non-commercial licence, remote_code, an alias "
              "the runtime does not serve, and an unpinned revision")},
    {"id": "refunds_disputes", "needs": ["database"],
     "cmd": go_test("TestResolveDispute|TestReversal"), "cwd": "control",
     "note": "dispute filing, freeze, resolution and payout control"},
    {"id": "payouts", "needs": ["database", "stripe_sandbox"],
     "cmd": go_test("TestWorkerEarnings|TestEarningsCarry|TestSupplierAccrual"),
     "cwd": "control",
     "note": "accrual and reconciliation; a real transfer needs the sandbox"},
    {"id": "failure_recovery", "needs": ["database"],
     "cmd": go_test("TestStuckRunningJobs|TestRescueStuckJob"), "cwd": "control",
     "note": "stuck-job rescue and cancellation"},
    {"id": "receipt_verification", "needs": ["database"],
     "cmd": go_test("TestMoneyCompleteness|TestLedgerWrite"), "cwd": "control",
     "note": "ledger conservation and sole-writer enforcement"},
    {"id": "backup_restore", "needs": [],
     "cmd": ["bash", "scripts/test-backup-schedule.sh"], "cwd": ".",
     "note": "backup scheduling and envelope"},
    {"id": "buyer_dashboard", "needs": ["database"],
     "cmd": ["node", "scripts/buyer-dashboard-live.mjs"], "cwd": ".",
     "note": "the page's own script signs in against a running merc and opens its "
             "workspace; every route it calls must be one merc serves"},
    {"id": "supplier_console", "needs": [],
     "cmd": ["node", "scripts/test-supplier-console.mjs"], "cwd": ".",
     "note": "worker-token auth, sub-cent money at ledger granularity, 4 payout-rail "
             "states and the refusal path, against recorded control-plane responses"},
    {"id": "price_board", "needs": ["database"],
     "cmd": go_test("TestPublicPriceBoardPage|TestPriceBoardObservations|TestPriceBoardWeighting"),
     "cwd": "control",
     "note": "the published page's own arithmetic must match the server's, including "
             "where the confidence weights decide the answer"},
    {"id": "python_sdk", "needs": ["database", "object_store", "local_runtime"],
     "cmd": ["python3", "scripts/sdk-live-python.py"], "cwd": ".",
     "note": "submits a real job to a running merc, waits for a real worker, fetches "
             "and validates the real 384-dim result"},
    {"id": "typescript_sdk", "needs": ["database", "object_store", "local_runtime"],
     "cmd": ["node", "scripts/sdk-live-typescript.mjs"], "cwd": ".",
     "note": "same end-to-end run; found three defects the stub tests could not "
             "(missing Idempotency-Key, array input, wrong cancel route)"},
    {"id": "alerts", "needs": [],
     "cmd": ["node", "scripts/site-build.mjs"], "cwd": ".",
     "note": "alert and dashboard validation only; no delivery to a receiver"},
]


# Go prints the panic FIRST and a goroutine dump after it, often hundreds of
# lines. Taking the last N lines therefore captures the dump's tail and discards
# the only part that says what broke -- every failed lane reported an identical,
# useless "testing.tRunner(...)" trace. Pull the diagnostic lines instead.
_FAILURE_MARKERS = (
    "panic:", "--- FAIL:", "FAIL\t", "fatal error:", "DATA RACE",
    "no such", "connection refused", "permission denied",
)


def failure_summary(output, limit=12):
    lines = output.strip().split("\n")
    keep = []
    for i, line in enumerate(lines):
        stripped = line.strip()
        if any(m in line for m in _FAILURE_MARKERS) or re.match(r"^\s+\S+\.go:\d+: ", line):
            keep.append(stripped)
            # A "--- FAIL" line is followed by the assertion that produced it.
            if "--- FAIL:" in line and i + 1 < len(lines):
                nxt = lines[i + 1].strip()
                if nxt and not nxt.startswith("---"):
                    keep.append(nxt)
        if len(keep) >= limit:
            break
    if not keep:
        keep = [l.strip() for l in lines[-6:] if l.strip()]
    seen, out = set(), []
    for k in keep:
        if k not in seen:
            seen.add(k)
            out.append(k)
    return "\n".join(out[:limit])

def run_lane(lane, capabilities, timeout):
    evidence_ok, evidence_reason, evidence_detail = False, "", None
    if lane.get("retained_evidence"):
        evidence_ok, evidence_reason, evidence_detail = validate_retained_runtime_evidence(
            lane["retained_evidence"])

    missing = [c for c in lane["needs"] if not capabilities[c][0]]
    if missing:
        result = {
            "lane": lane["id"],
            "status": "EXTERNALLY_BLOCKED",
            "missing_capabilities": missing,
            "reason": "; ".join(capabilities[c][1] for c in missing),
            "note": lane["note"],
        }
        if lane.get("retained_evidence"):
            result["retained_evidence"] = evidence_detail or {
                "status": "REJECTED",
                "reason": evidence_reason,
            }
        return result

    try:
        proc = subprocess.run(lane["cmd"], cwd=os.path.join(REPO, lane["cwd"]),
                              capture_output=True, text=True, timeout=timeout)
    except (subprocess.TimeoutExpired, FileNotFoundError) as exc:
        return {"lane": lane["id"], "status": "FAILED",
                "reason": f"{type(exc).__name__}: {exc}", "note": lane["note"]}

    if proc.returncode != 0:
        return {"lane": lane["id"], "status": "FAILED",
                "reason": failure_summary(proc.stdout + proc.stderr),
                "note": lane["note"]}

    # The command passing proves TESTED. A clean, committed retained receipt can
    # preserve REAL_RUNTIME_PROVEN. Neither condition binds the current
    # candidate image, so this script has no path to CANARY_PROVEN.
    result = {
        "lane": lane["id"],
        "status": "REAL_RUNTIME_PROVEN" if evidence_ok else "TESTED",
        "reason": "passed",
        "note": lane["note"],
    }
    if lane.get("retained_evidence"):
        result["retained_evidence"] = evidence_detail or {
            "status": "REJECTED",
            "reason": evidence_reason,
        }
    return result


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="evidence/canary/private-canary.json")
    ap.add_argument("--timeout", type=int, default=1800)
    args = ap.parse_args()

    capabilities = {name: fn() for name, fn in CAPABILITIES.items()}
    results = [run_lane(lane, capabilities, args.timeout) for lane in LANES]

    by_status = {}
    for r in results:
        by_status.setdefault(r["status"], []).append(r["lane"])

    proven = len(by_status.get("CANARY_PROVEN", []))
    runtime_proven = len(by_status.get("REAL_RUNTIME_PROVEN", []))
    retained = [
        (r["lane"], r["retained_evidence"])
        for r in results
        if r.get("retained_evidence", {}).get("status") == "REAL_RUNTIME_PROVEN"
    ]
    report = {
        "schema_version": 2,
        "kind": "merc_capability_inventory",
        "candidate_canary_authority": "scripts/go-closure-canary-rehearsal.sh",
        "capabilities": {k: {"present": v[0], "detail": v[1]} for k, v in capabilities.items()},
        "lanes": results,
        "summary": {status: sorted(lanes) for status, lanes in sorted(by_status.items())},
        "lanes_total": len(LANES),
        "lanes_canary_proven": proven,
        "lanes_real_runtime_proven": runtime_proven,
        "retained_real_runtime_evidence": {
            "validated_lanes": len(retained),
            "unique_receipts": len({e["path"] for _, e in retained}),
            "lanes": sorted(lane for lane, _ in retained),
            "candidate_bound": False,
        },
        "all_lanes_canary_proven": proven == len(LANES),
        "public_capability_allowed": proven == len(LANES),
        "note": ("This inventory cannot mint CANARY_PROVEN. A passing command proves "
                 "TESTED; a strict committed historical receipt may preserve "
                 "REAL_RUNTIME_PROVEN. Only the formal immutable-image/exact-commit "
                 "GO-closure rehearsal can produce candidate-bound canary authority. "
                 "A missing current capability remains EXTERNALLY_BLOCKED even when "
                 "retained historical evidence validates."),
    }

    out = os.path.join(REPO, args.out)
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "w") as fh:
        json.dump(report, fh, indent=2)
        fh.write("\n")

    print(f"capability inventory: {proven}/{len(LANES)} candidate-bound lanes CANARY_PROVEN; "
          f"{len(retained)} lane(s) carry validated historical REAL_RUNTIME_PROVEN evidence")
    for status in sorted(by_status):
        print(f"  {status:20} {', '.join(sorted(by_status[status]))}")
    for name, (present, detail) in sorted(capabilities.items()):
        if not present:
            print(f"  MISSING CAPABILITY   {name}: {detail}")

    if by_status.get("FAILED"):
        return 1
    if proven != len(LANES):
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
