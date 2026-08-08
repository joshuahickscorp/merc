#!/usr/bin/env python3
"""Heterogeneous placement readiness gate.

Reports every precondition for a *valid* Metal-vs-CUDA embed placement test as
SATISFIED, UNSATISFIED, or BLOCKED_ON_AUTHORIZED_SPEND, with the artifact or
code path that backs the verdict.

Ready means: the only thing standing between here and a valid measurement is
authorized spend. Ready does not mean the test was run. This script never
provisions a pod, never calls RunPod, and never spends money.

Exit codes:
  0  — no UNSATISFIED items (SATISFIED and BLOCKED_ON_AUTHORIZED_SPEND only).
       Summary line is READY or READY_PENDING_AUTHORIZED_SPEND.
  1  — one or more UNSATISFIED items. Summary line is NOT_READY.
  2  — gate itself could not evaluate (missing tree / unreadable inputs).

No environment variable can force a pass. MERC_PLACEMENT_READINESS_FORCE and
similar names are ignored and, if set, named as a refused bypass attempt.
"""

from __future__ import annotations

import json
import os
import re
import sys
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Optional

ROOT = Path(__file__).resolve().parents[1]

AUTHORITY = ROOT / "control" / "runtime-authority.json"
CONTRACT = ROOT / "ops" / "placement-readiness-contract.json"
EMBED_COMPARATOR = ROOT / "control" / "embedding_comparator.go"
RUNTIME_PROFILE = ROOT / "control" / "runtime-profiles" / "vllm-llama-3.2-1b-instruct-bf16.json"
SPEND_GUARD = ROOT / "scripts" / "runpod-spend-guard.py"
RUNPOD_VLLM = ROOT / "scripts" / "runpod-vllm.sh"
ORPHAN_TEST = ROOT / "scripts" / "test-runpod-orphan-reconcile.py"
WRITE_BOUND = ROOT / "scripts" / "write-bound-evidence.py"
VALIDATE_BINDING = ROOT / "scripts" / "validate-evidence-binding.py"
SHADOW_SELECTION = ROOT / "control" / "runtime_shadow_selection.go"
GOVERNED_COMPARISON = ROOT / "control" / "runtime_governed_comparison.go"
CELL_COST = ROOT / "control" / "runtime_cell_cost.go"
CELL_COST_TEST = ROOT / "control" / "runtime_cell_cost_test.go"
METAL_PARITY = ROOT / "evidence" / "perf" / "selector" / "engine-parity-metal-embed-latest.json"
WITHDRAWN_SPEND = ROOT / "evidence" / "runpod" / "spend-rr7b6uwmivaolh.json"

IMMUTABLE_IMAGE = re.compile(
    r"^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$"
)
SHA256 = re.compile(r"^[0-9a-f]{64}$")

# Names that look like a force-pass. Setting any of these must never green the gate.
REFUSED_BYPASS_ENVS = (
    "MERC_PLACEMENT_READINESS_FORCE",
    "MERC_PLACEMENT_READINESS_BYPASS",
    "MERC_PLACEMENT_READY",
    "MERC_FORCE_PLACEMENT_READY",
    "MERC_ALLOW_PLACEMENT_READINESS_SKIP",
    "MERC_SKIP_PLACEMENT_READINESS",
)

STATE_SATISFIED = "SATISFIED"
STATE_UNSATISFIED = "UNSATISFIED"
STATE_BLOCKED_SPEND = "BLOCKED_ON_AUTHORIZED_SPEND"


@dataclass
class Check:
    id: str
    state: str
    reason: str
    artifact_or_path: str
    detail: dict[str, Any] = field(default_factory=dict)


def load_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def cells_by_id(authority: dict) -> dict[str, dict]:
    out: dict[str, dict] = {}
    for rt in authority.get("runtimes", []):
        for cell in rt.get("cells", []):
            cid = cell.get("id")
            if not cid:
                continue
            row = dict(cell)
            row["_runtime_id"] = rt.get("runtime_id")
            row["_runtime_lifecycle"] = rt.get("lifecycle")
            row["_device"] = rt.get("device")
            row["_engine"] = rt.get("engine")
            row["_platforms"] = list((rt.get("hardware") or {}).get("platforms") or [])
            out[cid] = row
    return out


def model_artifact_sha(authority: dict, model_id: str, wire_kind: str, path_suffix: str) -> Optional[str]:
    for model in authority.get("models", []):
        if model.get("id") != model_id:
            continue
        model_kind = model.get("wire_kind") or "hf"
        for art in model.get("artifacts", []):
            kind = art.get("wire_kind") or model_kind
            if kind != wire_kind:
                continue
            if str(art.get("path", "")).endswith(path_suffix) or art.get("path") == path_suffix:
                return art.get("sha256")
    return None


def file_contains(path: Path, *needles: str) -> bool:
    if not path.is_file():
        return False
    text = path.read_text(encoding="utf-8", errors="replace")
    return all(n in text for n in needles)


def check_matched_model_identity(authority: dict, contract: dict, cells: dict[str, dict]) -> Check:
    cid = "matched_model_identity"
    want = contract["matched_model_identity"]
    job = want["job_type"]
    model = want["model"]
    arms = want["arms"]
    missing = []
    mismatches = []
    for arm_name, arm in arms.items():
        cell = cells.get(arm["cell_id"])
        if cell is None:
            missing.append(arm["cell_id"])
            continue
        if cell.get("job") != job:
            mismatches.append(f"{arm['cell_id']}: job={cell.get('job')!r} want {job!r}")
        if cell.get("model") != model:
            mismatches.append(f"{arm['cell_id']}: model={cell.get('model')!r} want {model!r}")
        if cell.get("_runtime_id") != arm["runtime_id"]:
            mismatches.append(
                f"{arm['cell_id']}: runtime={cell.get('_runtime_id')!r} want {arm['runtime_id']!r}"
            )
        if arm_name == "vllm_cuda" and cell.get("_device") != "cuda":
            mismatches.append(f"{arm['cell_id']}: device={cell.get('_device')!r} want 'cuda'")
        if arm_name != "vllm_cuda" and cell.get("_device") != "metal":
            mismatches.append(f"{arm['cell_id']}: device={cell.get('_device')!r} want 'metal'")
    if missing or mismatches:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=(
                "matched embed arms incomplete or identity-mismatched: "
                + "; ".join(missing + mismatches)
            ),
            artifact_or_path="control/runtime-authority.json + ops/placement-readiness-contract.json",
            detail={"missing": missing, "mismatches": mismatches},
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            "candle-metal-minilm-embed, llama-cpp-metal-minilm-embed, and "
            "vllm-cuda-minilm-embed all declare job=embed model=all-minilm-l6-v2"
        ),
        artifact_or_path="control/runtime-authority.json cells; ops/placement-readiness-contract.json",
    )


def check_matched_verification(authority: dict, contract: dict, cells: dict[str, dict]) -> Check:
    cid = "matched_verification_contract"
    want = contract["verification_contract"]
    arms = contract["matched_model_identity"]["arms"]
    bad = []
    for arm in arms.values():
        cell = cells.get(arm["cell_id"])
        if not cell:
            bad.append(f"missing {arm['cell_id']}")
            continue
        if cell.get("verification") != want["verification"]:
            bad.append(
                f"{arm['cell_id']}: verification={cell.get('verification')!r} "
                f"want {want['verification']!r}"
            )
    if not EMBED_COMPARATOR.is_file():
        bad.append(f"missing {EMBED_COMPARATOR.relative_to(ROOT)}")
    else:
        text = EMBED_COMPARATOR.read_text(encoding="utf-8")
        if "embeddingMeanCosineThreshold = 0.999" not in text:
            bad.append("embeddingMeanCosineThreshold is not 0.999")
        if "embeddingRowCosineThreshold = 0.999" not in text:
            bad.append("embeddingRowCosineThreshold is not 0.999")
    if bad:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="verification contract not matched across arms or thresholds: " + "; ".join(bad),
            artifact_or_path="control/runtime-authority.json; control/embedding_comparator.go",
            detail={"problems": bad},
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason="all three arms declare verification=cosine; mean and row floors are 0.999",
        artifact_or_path=(
            "control/runtime-authority.json; "
            "control/embedding_comparator.go:embeddingMeanCosineThreshold/"
            "embeddingRowCosineThreshold"
        ),
    )


def check_image_digest(contract: dict) -> Check:
    cid = "pinned_immutable_image_digest"
    pin = contract["cuda_image_pin"]
    image = pin.get("image") or ""
    if not IMMUTABLE_IMAGE.match(image):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=f"contract image is not an immutable OCI digest: {image!r}",
            artifact_or_path="ops/placement-readiness-contract.json:cuda_image_pin",
        )
    if not RUNTIME_PROFILE.is_file():
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="runtime profile that pins the image is missing",
            artifact_or_path=str(RUNTIME_PROFILE.relative_to(ROOT)),
        )
    profile = load_json(RUNTIME_PROFILE)
    profile_image = profile.get("container_image") or ""
    if profile_image != image:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=(
                f"contract image {image!r} does not match runtime profile "
                f"container_image {profile_image!r}"
            ),
            artifact_or_path=str(RUNTIME_PROFILE.relative_to(ROOT)),
        )
    if not file_contains(RUNPOD_VLLM, "@sha256:", "must be an immutable OCI digest"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="runpod-vllm.sh does not refuse non-digest images",
            artifact_or_path="scripts/runpod-vllm.sh",
        )
    # spend-guard uses: if "@sha256:" not in image → refusal about immutable OCI digest
    if not file_contains(SPEND_GUARD, "@sha256:", "immutable OCI digest"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="spend-guard does not refuse mutable image tags on receipts",
            artifact_or_path="scripts/runpod-spend-guard.py:receipt_rule_refusals",
        )
    withdrawn_note = ""
    if WITHDRAWN_SPEND.is_file():
        receipt = load_json(WITHDRAWN_SPEND)
        if str(receipt.get("validity", "")).upper() == "WITHDRAWN":
            withdrawn_note = (
                f"; cites {WITHDRAWN_SPEND.relative_to(ROOT)} as WITHDRAWN for mutable tag"
            )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            f"CUDA arm image pinned to {image}; provisioner and spend-guard refuse "
            f"mutable tags{withdrawn_note}"
        ),
        artifact_or_path=(
            "ops/placement-readiness-contract.json; "
            "control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json; "
            "scripts/runpod-vllm.sh; scripts/runpod-spend-guard.py"
        ),
    )


def check_cuda_model_artifact(authority: dict, contract: dict, cells: dict[str, dict]) -> Check:
    cid = "pinned_cuda_model_artifact_digest"
    arm = contract["matched_model_identity"]["arms"]["vllm_cuda"]
    cell = cells.get(arm["cell_id"])
    if not cell:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=f"CUDA embed cell {arm['cell_id']} is not declared",
            artifact_or_path="control/runtime-authority.json",
        )
    wire = cell.get("wire_kind") or "hf"
    if wire != arm["wire_kind"]:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=f"cell wire_kind={wire!r} != contract {arm['wire_kind']!r}",
            artifact_or_path="control/runtime-authority.json",
        )
    pinned = arm["model_artifact_sha256"]
    if not SHA256.match(pinned):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=f"contract model digest is not sha256: {pinned!r}",
            artifact_or_path="ops/placement-readiness-contract.json",
        )
    auth_sha = model_artifact_sha(
        authority, arm_model_id(arm, contract), wire, arm["model_artifact_path"]
    )
    if auth_sha != pinned:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=(
                f"authority artifact sha256 {auth_sha!r} does not match contract pin {pinned!r}"
            ),
            artifact_or_path="control/runtime-authority.json models[all-minilm-l6-v2].artifacts",
        )
    # Explicit third-arm declaration
    weights = contract["matched_model_identity"].get("weights_comparability") or {}
    if weights.get("status") != "TRIPLE_ARM_IDENTITY_DECLARED":
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="weights_comparability does not declare TRIPLE_ARM_IDENTITY_DECLARED",
            artifact_or_path="ops/placement-readiness-contract.json",
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            f"CUDA embed arm pins {arm['model_artifact_path']} sha256={pinned} "
            f"(wire_kind={wire}); third arm on dual-wire Metal pair "
            f"(candle safetensors + llama F16 GGUF)"
        ),
        artifact_or_path=(
            "control/runtime-authority.json models + vllm-cuda-minilm-embed; "
            "ops/placement-readiness-contract.json; "
            "evidence/perf/selector/engine-parity-metal-embed-latest.json (metal dual-wire)"
        ),
        detail={
            "wire_kind": wire,
            "model_artifact_sha256": pinned,
            "third_arm_note": arm.get("note"),
            "metal_parity_evidence_present": METAL_PARITY.is_file(),
        },
    )


def arm_model_id(arm: dict, contract: dict) -> str:
    return contract["matched_model_identity"]["model"]


def check_cost_cap(contract: dict) -> Check:
    cid = "cost_cap"
    if not SPEND_GUARD.is_file() or not RUNPOD_VLLM.is_file():
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="spend-guard or runpod-vllm.sh missing",
            artifact_or_path="scripts/runpod-spend-guard.py; scripts/runpod-vllm.sh",
        )
    guard = SPEND_GUARD.read_text(encoding="utf-8")
    vllm = RUNPOD_VLLM.read_text(encoding="utf-8")
    if "MIN_CAP_USD" not in guard or "budget_seconds" not in guard:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="spend-guard does not define MIN_CAP_USD / budget_seconds",
            artifact_or_path="scripts/runpod-spend-guard.py",
        )
    if "MERC_RUNPOD_CAP_USD" not in vllm or "BUDGET_SECS" not in vllm:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="runpod-vllm.sh experiment path does not bind MERC_RUNPOD_CAP_USD to BUDGET_SECS",
            artifact_or_path="scripts/runpod-vllm.sh experiment",
        )
    bounds = contract["money_bounds"]
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            f"cost cap via {bounds['cost_cap_env']} (default {bounds['default_cap_usd']}, "
            f"floor {bounds['min_cap_usd']}); lifetime = {bounds['lifetime_share_of_cap']} of cap"
        ),
        artifact_or_path=(
            "scripts/runpod-spend-guard.py:MIN_CAP_USD,budget_seconds; "
            "scripts/runpod-vllm.sh experiment"
        ),
    )


def check_startup_deadline() -> Check:
    cid = "startup_deadline"
    if not RUNPOD_VLLM.is_file():
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="scripts/runpod-vllm.sh missing",
            artifact_or_path="scripts/runpod-vllm.sh",
        )
    text = RUNPOD_VLLM.read_text(encoding="utf-8")
    if "for i in $(seq 1 90)" not in text and "seq 1 90" not in text:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="startup readiness poll bound (seq 1 90) not found",
            artifact_or_path="scripts/runpod-vllm.sh",
        )
    if "vLLM never became ready" not in text:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="startup failure path does not abort when vLLM never becomes ready",
            artifact_or_path="scripts/runpod-vllm.sh",
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason="startup readiness bounded to 90 x 6s (~540s) with teardown on failure",
        artifact_or_path="scripts/runpod-vllm.sh READY loop (seq 1 90; sleep 6)",
    )


def check_idle_deadline() -> Check:
    cid = "idle_deadline"
    if not RUNPOD_VLLM.is_file() or not SPEND_GUARD.is_file():
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="runpod driver or spend-guard missing",
            artifact_or_path="scripts/runpod-vllm.sh; scripts/runpod-spend-guard.py",
        )
    vllm = RUNPOD_VLLM.read_text(encoding="utf-8")
    if "BUDGET_SECS" not in vllm or "ELAPSED" not in vllm:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="experiment path lacks lifetime/idle kill against BUDGET_SECS",
            artifact_or_path="scripts/runpod-vllm.sh experiment",
        )
    if "MERC_RUNPOD_HOLD_SECS" not in vllm:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="non-experiment hold bound MERC_RUNPOD_HOLD_SECS missing",
            artifact_or_path="scripts/runpod-vllm.sh",
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            "lifetime budget (BUDGET_SECS from cap) kills hung/idle experiments; "
            "MERC_RUNPOD_HOLD_SECS bounds non-experiment hold (default 60s)"
        ),
        artifact_or_path=(
            "scripts/runpod-vllm.sh experiment ELAPSED/BUDGET_SECS; "
            "scripts/runpod-spend-guard.py budget_seconds; MERC_RUNPOD_HOLD_SECS"
        ),
    )


def check_teardown_and_orphan() -> Check:
    cid = "teardown_and_orphan_sweep"
    missing = []
    for path in (RUNPOD_VLLM, SPEND_GUARD, ORPHAN_TEST):
        if not path.is_file():
            missing.append(str(path.relative_to(ROOT)))
    if missing:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="teardown/orphan paths missing: " + ", ".join(missing),
            artifact_or_path="scripts/runpod-vllm.sh; scripts/runpod-spend-guard.py",
        )
    vllm = RUNPOD_VLLM.read_text(encoding="utf-8")
    guard = SPEND_GUARD.read_text(encoding="utf-8")
    need_vllm = ("teardown_verified", "sweep_all", "entry_reconcile", "terminate")
    need_guard = ("reconcile", "orphan", "teardown_verified", "--self-test")
    bad = []
    for n in need_vllm:
        if n not in vllm:
            bad.append(f"runpod-vllm.sh missing {n}")
    for n in need_guard:
        if n not in guard:
            bad.append(f"spend-guard missing {n}")
    if "orphan" not in ORPHAN_TEST.read_text(encoding="utf-8"):
        bad.append("test-runpod-orphan-reconcile.py does not exercise orphan reconcile")
    if bad:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="; ".join(bad),
            artifact_or_path="scripts/runpod-vllm.sh; scripts/runpod-spend-guard.py",
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            "verified teardown + account orphan sweep exist and are exercised offline "
            "(self-test + test-runpod-orphan-reconcile.py in make ci)"
        ),
        artifact_or_path=(
            "scripts/runpod-vllm.sh (trap sweep_all, teardown_verified); "
            "scripts/runpod-spend-guard.py reconcile; "
            "scripts/test-runpod-orphan-reconcile.py; make ci"
        ),
    )


def check_spend_receipt_producer_identity() -> Check:
    cid = "spend_receipt_producer_identity"
    if not SPEND_GUARD.is_file() or not WRITE_BOUND.is_file() or not VALIDATE_BINDING.is_file():
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="spend-guard or bound-evidence tooling missing",
            artifact_or_path="scripts/runpod-spend-guard.py; scripts/write-bound-evidence.py",
        )
    guard = SPEND_GUARD.read_text(encoding="utf-8")
    binding_lib = ROOT / "scripts" / "lib" / "evidence_binding.py"
    if not binding_lib.is_file():
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="scripts/lib/evidence_binding.py missing",
            artifact_or_path="scripts/lib/evidence_binding.py",
        )
    # Receipt writes go through emit_bound_json → write_bound_evidence (BOUND).
    # Detect the live writer path, not a free-form comment.
    wired = (
        "emit_bound_json" in guard
        and "from lib.evidence_binding import" in guard
        and file_contains(binding_lib, "def emit_bound_json", "write_bound_evidence", "producer_identity")
    )
    if not wired:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=(
                "spend-receipt writer does not bind producer_identity through "
                "lib.evidence_binding.emit_bound_json / write_bound_evidence"
            ),
            artifact_or_path=(
                "scripts/runpod-spend-guard.py receipt command; "
                "scripts/lib/evidence_binding.py"
            ),
        )
    if "image_digest" not in guard:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=(
                "spend-receipt writer calls emit_bound_json but does not lift the "
                "immutable image digest into producer_identity.image_digest"
            ),
            artifact_or_path="scripts/runpod-spend-guard.py receipt command",
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            "receipt --out writes via emit_bound_json → write_bound_evidence "
            "(BOUND producer_identity, image digest lifted from immutable image)"
        ),
        artifact_or_path=(
            "scripts/runpod-spend-guard.py receipt; "
            "scripts/lib/evidence_binding.py:emit_bound_json; "
            "scripts/write-bound-evidence.py; "
            "scripts/validate-evidence-binding.py"
        ),
    )


def check_selector_consumer() -> Check:
    cid = "selector_consumer"
    missing = [str(p.relative_to(ROOT)) for p in (SHADOW_SELECTION, GOVERNED_COMPARISON, CELL_COST) if not p.is_file()]
    if missing:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="selector consumer paths missing: " + ", ".join(missing),
            artifact_or_path="control/runtime_shadow_selection.go",
        )
    if not file_contains(SHADOW_SELECTION, "runtime_shadow_selections"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="shadow selection does not write runtime_shadow_selections",
            artifact_or_path="control/runtime_shadow_selection.go",
        )
    if not file_contains(CELL_COST, "runtime_shadow_selections"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="cell cost authority does not read runtime_shadow_selections",
            artifact_or_path="control/runtime_cell_cost.go",
        )
    if not file_contains(GOVERNED_COMPARISON, "GovernedShadowDecision", "ExcludedCells"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="governed comparison does not structure excluded/eligible cells for consumers",
            artifact_or_path="control/runtime_governed_comparison.go",
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            "selector consumers exist: shadow selection records, governed comparison "
            "decisions, and cell-cost regret over runtime_shadow_selections"
        ),
        artifact_or_path=(
            "control/runtime_shadow_selection.go; "
            "control/runtime_governed_comparison.go; "
            "control/runtime_cell_cost.go"
        ),
    )


def check_cross_hardware_cost_authority() -> Check:
    cid = "cross_hardware_cost_authority"
    if not CELL_COST.is_file() or not CELL_COST_TEST.is_file():
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="cell cost authority or its refusal test is missing",
            artifact_or_path="control/runtime_cell_cost.go",
        )
    if not file_contains(CELL_COST, "comparableHardwareFor"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="comparableHardwareFor refusal path missing",
            artifact_or_path="control/runtime_cell_cost.go",
        )
    if not file_contains(
        CELL_COST_TEST,
        "TestComparableHardwareRefusesToMixMachines",
        "compared across hardware classes",
    ):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason="cross-hardware cost refusal is not pinned by test",
            artifact_or_path="control/runtime_cell_cost_test.go",
        )
    # Explicit refusal satisfies the precondition (no invented cross-hw USD authority).
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            "no cross-hardware cost authority exists; comparableHardwareFor explicitly "
            "refuses to mix machines (pinned by test) — placement must not invent one"
        ),
        artifact_or_path=(
            "control/runtime_cell_cost.go:comparableHardwareFor; "
            "control/runtime_cell_cost_test.go:TestComparableHardwareRefusesToMixMachines"
        ),
    )


def check_cuda_cell_not_advertised(cells: dict[str, dict], contract: dict) -> Check:
    """Offline structural check that the identity cell stays DRAFT / non-routable.

    Full advertised projection is enforced in Go tests (cellIsAdvertised). This
    gate re-checks the document facts the projection is built from.
    """
    cid = "cuda_embed_cell_non_routable_identity"
    cell_id = contract["cell_surface_policy"]["cuda_embed_cell_id"]
    cell = cells.get(cell_id)
    if not cell:
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=f"{cell_id} is not declared in runtime-authority.json",
            artifact_or_path="control/runtime-authority.json",
        )
    lifecycle = cell.get("lifecycle") or cell.get("_runtime_lifecycle")
    if lifecycle in ("ACTIVE", "CANARY"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=(
                f"{cell_id} effective lifecycle is {lifecycle}: that would make it "
                "routable/advertisable, which this identity work must not do"
            ),
            artifact_or_path="control/runtime-authority.json",
        )
    if cell.get("_runtime_lifecycle") in ("ACTIVE", "CANARY"):
        return Check(
            id=cid,
            state=STATE_UNSATISFIED,
            reason=f"parent profile lifecycle is {cell.get('_runtime_lifecycle')}",
            artifact_or_path="control/runtime-authority.json vllm_cuda",
        )
    return Check(
        id=cid,
        state=STATE_SATISFIED,
        reason=(
            f"{cell_id} is DRAFT under DRAFT vllm_cuda — identity only; not CANARY/ACTIVE "
            "(Go tests pin not advertised / not directed / not routable)"
        ),
        artifact_or_path=(
            "control/runtime-authority.json; "
            "control/placement_readiness_test.go; "
            "control/activation_policy_test.go:cellIsAdvertised"
        ),
    )


def evaluate() -> tuple[list[Check], list[str]]:
    """Return (checks, hard_errors). hard_errors mean the gate could not run."""
    hard: list[str] = []
    if not AUTHORITY.is_file():
        hard.append(f"missing {AUTHORITY.relative_to(ROOT)}")
    if not CONTRACT.is_file():
        hard.append(f"missing {CONTRACT.relative_to(ROOT)}")
    if hard:
        return [], hard

    authority = load_json(AUTHORITY)
    contract = load_json(CONTRACT)
    cells = cells_by_id(authority)

    checks = [
        check_matched_model_identity(authority, contract, cells),
        check_matched_verification(authority, contract, cells),
        check_image_digest(contract),
        check_cuda_model_artifact(authority, contract, cells),
        check_cost_cap(contract),
        check_startup_deadline(),
        check_idle_deadline(),
        check_teardown_and_orphan(),
        check_spend_receipt_producer_identity(),
        check_selector_consumer(),
        check_cross_hardware_cost_authority(),
        check_cuda_cell_not_advertised(cells, contract),
    ]
    return checks, []


# What the money buys, stated before it is spent.
#
# READY is a claim about preconditions, not about answers. An operator reading
# it should already know which questions the resulting measurement can settle,
# because the most expensive failure available here is a funded run that comes
# back refusing the question it was funded to answer.
YIELDS = [
    "per-hardware-class latency at p50/p95/p99 with an absolute delta, on one matched contract",
    "throughput per hardware class on that contract",
    "cosine outcome equivalence between the CUDA arm and the Metal pair, or a voided timing",
    "a bounded spend receipt with producer identity and verified teardown",
    "a shadow selector decision that records eligibility, exclusions, winners and regret",
]

CANNOT_YIELD = [
    "a cross-hardware cost comparison -- comparableHardwareFor refuses to mix machines, "
    "and the difference between an M3 Ultra and a rented A40 is the machine rather than the placement",
    "a promotion: the promotion gate wants production decisions, twenty samples on one "
    "hardware class, zero verification failures and a margin, and one experiment is none of that",
    "a fleet claim, a TP>1 claim, or a claim at any other batch, quantisation or model",
    "closure of any of the eight external readiness gates",
]


def summary_line(checks: list[Check]) -> tuple[str, int]:
    unsat = [c for c in checks if c.state == STATE_UNSATISFIED]
    blocked = [c for c in checks if c.state == STATE_BLOCKED_SPEND]
    if unsat:
        return "NOT_READY", 1
    if blocked:
        return "READY_PENDING_AUTHORIZED_SPEND", 0
    return "READY", 0


def main(argv: list[str]) -> int:
    # Refuse env bypasses: presence is recorded, never honored.
    bypass_attempts = [name for name in REFUSED_BYPASS_ENVS if os.environ.get(name)]
    json_out = "--json" in argv

    checks, hard = evaluate()
    if hard:
        print("placement-readiness: FAIL: gate could not evaluate", file=sys.stderr)
        for h in hard:
            print(f"  - {h}", file=sys.stderr)
        return 2

    if bypass_attempts:
        checks.append(
            Check(
                id="env_bypass_refusal",
                state=STATE_UNSATISFIED,
                reason=(
                    "unsatisfied readiness gate cannot be bypassed by env var; refused "
                    + ", ".join(bypass_attempts)
                ),
                artifact_or_path="scripts/validate-placement-readiness.py (REFUSED_BYPASS_ENVS)",
                detail={"refused_env": bypass_attempts},
            )
        )

    status, code = summary_line(checks)
    counts = {
        STATE_SATISFIED: sum(1 for c in checks if c.state == STATE_SATISFIED),
        STATE_UNSATISFIED: sum(1 for c in checks if c.state == STATE_UNSATISFIED),
        STATE_BLOCKED_SPEND: sum(1 for c in checks if c.state == STATE_BLOCKED_SPEND),
    }

    report = {
        "schema_version": 1,
        "kind": "placement_readiness_gate",
        "summary": status,
        "counts": counts,
        "exit_code": code,
        "refused_bypass_envs": bypass_attempts,
        "preconditions": [asdict(c) for c in checks],
        "operator_note": (
            "SATISFIED items are checkable offline. UNSATISFIED items must be fixed "
            "before a valid placement measurement. BLOCKED_ON_AUTHORIZED_SPEND means "
            "the precondition can only complete with authorized spend (not used to "
            "hide identity gaps). Ready = no UNSATISFIED; only authorized spend remains."
        ),
        "a_valid_test_yields": YIELDS,
        "a_valid_test_cannot_yield": CANNOT_YIELD,
    }

    if json_out:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        print(f"placement-readiness: {status}")
        print(
            f"  SATISFIED={counts[STATE_SATISFIED]} "
            f"UNSATISFIED={counts[STATE_UNSATISFIED]} "
            f"BLOCKED_ON_AUTHORIZED_SPEND={counts[STATE_BLOCKED_SPEND]}"
        )
        width = max(len(c.id) for c in checks)
        for c in checks:
            print(f"  [{c.state:<28}] {c.id:<{width}}  {c.reason}")
            print(f"  {'':30} path: {c.artifact_or_path}")
        if status == "NOT_READY":
            print("placement-readiness: operator must resolve every UNSATISFIED item above")
        elif status == "READY_PENDING_AUTHORIZED_SPEND":
            print(
                "placement-readiness: identities and gates are offline-ready; "
                "only authorized spend remains (do not spend from this gate)"
            )
            print("  a valid test yields:")
            for y in YIELDS:
                print(f"    + {y}")
            print("  a valid test cannot yield:")
            for n in CANNOT_YIELD:
                print(f"    - {n}")
        else:
            print("placement-readiness: ready for a valid test pending authorized spend only")
            print("  a valid test yields:")
            for y in YIELDS:
                print(f"    + {y}")
            print("  a valid test cannot yield:")
            for n in CANNOT_YIELD:
                print(f"    - {n}")

    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
