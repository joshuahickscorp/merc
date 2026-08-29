#!/usr/bin/env bash
# Heterogeneous placement principal test — precondition gate + governed driver.
#
# Env-gated. Without MERC_HETERO_PLACEMENT=1 this script exits 2 and writes
# nothing. With the flag it re-checks structural preconditions on this tree;
# if the three-arm mixed workload cannot be driven end-to-end it writes a
# BOUND refusal receipt (via ops/scripts/write-bound-evidence.py) and exits 3.
# It never invents arm metrics.
#
# Intended paid path (only when preconditions pass):
#   MERC_HETERO_PLACEMENT=1 \
#   MERC_RUNPOD_CAP_USD=3 \
#   MERC_RUNPOD_COST_PER_HR=<advertised> \
#   MERC_RUNPOD_EXPERIMENT_CMD='bash ops/scripts/heterogeneous-placement-drive.sh --execute' \
#   bash ops/scripts/runpod-vllm.sh experiment
#
# Never invoke --execute outside ops/scripts/runpod-vllm.sh experiment. No --keep.

set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$ROOT"

say() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

if [[ "${MERC_HETERO_PLACEMENT:-}" != "1" ]]; then
  say "refusing: set MERC_HETERO_PLACEMENT=1 to run the heterogeneous placement principal test" >&2
  exit 2
fi

MODE="${1:-check}"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
OUT_DIR="${MERC_HETERO_OUT_DIR:-evidence/perf}"
RECEIPT_REL="${MERC_HETERO_RECEIPT:-$OUT_DIR/heterogeneous-placement-principal-${TS}.json}"
RECEIPT_LATEST="$OUT_DIR/heterogeneous-placement-principal-latest.json"
mkdir -p "$OUT_DIR"

# ---------------------------------------------------------------------------
# Structural preconditions for the three-arm mix.
#
# A = fixed vLLM CUDA only
# B = fixed Metal only
# C = Merc selecting Metal/CUDA per contract
#
# Mix: latency-sensitive realtime, throughput batch_infer, embeddings,
# optional media/render. Zero intentional duplicates. Identical quality and
# deadline contracts across arms.
# ---------------------------------------------------------------------------

probe_preconditions() {
  python3 - <<'PY'
import json, sys
from pathlib import Path

root = Path(".").resolve()
auth = json.loads((root / "src/control/runtime-authority.json").read_text())
qc_path = root / "src/control/acceptable-quality-contracts.json"
if not qc_path.exists():
    qc_path = root / "ops/acceptable-quality-contracts.json"
qc = json.loads(qc_path.read_text()) if qc_path.exists() else {}
workload = (root / "src/control/workload_classification.go").read_text()
decision = (root / "src/control/runtime_decision.go").read_text()
promo = (root / "src/control/runtime_cell_promotion.go").read_text()

cells = []
for rt in auth.get("runtimes", []):
    rid = rt.get("runtime_id")
    for c in rt.get("cells", []):
        cells.append({
            "runtime_id": rid,
            "cell_id": c.get("id"),
            "job": c.get("job"),
            "model": c.get("model"),
            "lifecycle": c.get("lifecycle") or rt.get("lifecycle"),
            "cloud_backed": bool(c.get("cloud_backed")),
            "engine": rt.get("engine"),
            "device": rt.get("device"),
        })

active_candle_jobs = {
    c["job"] for c in cells
    if c["runtime_id"] == "candle_metal" and c.get("lifecycle") == "ACTIVE"
}
vllm = [c for c in cells if c["runtime_id"] == "vllm_cuda"]
cuda_embed = [c for c in cells if c["cell_id"] == "vllm-cuda-minilm-embed"]
cuda_batch = [c for c in cells if c["job"] == "batch_infer" and c.get("cloud_backed")]
routable_cuda = [
    c for c in cells
    if c.get("cloud_backed") and c.get("lifecycle") in ("ACTIVE", "CANARY")
]

blockers = []
software_ready = []

# 1. Multi-family admission path (G024 load-bearing change).
if "selectAdmissionCandidates" not in workload:
    blockers.append({
        "id": "multi_family_admission_missing",
        "detail": "selectAdmissionCandidates not present; Metal-only rank-and-freeze-1 still sole path",
        "refs": ["src/control/workload_classification.go"],
    })
else:
    software_ready.append({
        "id": "multi_family_admission_present",
        "detail": (
            "Named mechanism: rankAndFreezeAdmissionCell "
            "(src/control/workload_classification.go) was the Metal-only singleton freeze. "
            "selectAdmissionCandidates now freezes a multi-family eligible set when an "
            "ACTIVE AcceptableQualityContract covers multiple device families; same-family "
            "competition still rank-and-freezes to one. Placement v4 + claim identity pin "
            "relaxation let a second family claim."
        ),
        "refs": [
            "src/control/workload_classification.go:selectAdmissionCandidates",
            "src/control/workload_classification.go:rankAndFreezeAdmissionCell",
            "src/control/quote.go:placementRequirementVersionMultiFamily",
            "src/control/scheduler.go (placement version 4 identity pin skip)",
        ],
    })

# 2. Quality contracts.
contracts = {c.get("id"): c for c in qc.get("contracts", [])}
embed_c = contracts.get("embed-cosine-v2-all-minilm-l6-v2")
refused_gen = contracts.get("batch-infer-metal-q4-vs-cuda-bf16-REFUSED")
if not embed_c or not embed_c.get("multi_family_substitutable"):
    blockers.append({"id": "embed_quality_contract_missing", "detail": "embed multi-family contract absent"})
else:
    software_ready.append({
        "id": "embed_quality_contract",
        "detail": "AcceptableQualityContract embed-cosine-v2-all-minilm-l6-v2: mean/row cosine 0.999",
        "contract": embed_c.get("id"),
    })
if not refused_gen or refused_gen.get("status") != "REFUSED":
    blockers.append({
        "id": "generation_q4_bf16_refusal_missing",
        "detail": "Metal q4 vs CUDA bf16 must be an explicit REFUSED quality contract",
    })
else:
    software_ready.append({
        "id": "generation_q4_bf16_honest_refusal",
        "detail": refused_gen.get("refusal_reason") or refused_gen.get("how_routing_proves_met"),
        "contract": refused_gen.get("id"),
    })

if "HETEROGENEOUS_ELIGIBLE_SET" not in decision:
    blockers.append({
        "id": "runtime_decision_multi_family_basis_missing",
        "detail": "RuntimeDecision must seal HETEROGENEOUS_ELIGIBLE_SET and cite quality_contract_id",
    })
else:
    software_ready.append({
        "id": "runtime_decision_cites_quality_contract",
        "detail": "RuntimeDecision SelectionBasis HETEROGENEOUS_ELIGIBLE_SET + QualityContractID",
    })

# 3. CUDA cells still DRAFT — legitimate promotion gate, not status edit.
if not routable_cuda:
    blockers.append({
        "id": "no_routable_cuda_cell",
        "detail": (
            "vllm_cuda (and other CUDA profiles) remain DRAFT. Promotion is refused by "
            "promotionMatchedPairAuthorityRefusal and activation_policy scope/global-lifecycle "
            "(src/control/runtime_cell_promotion.go:78/:321; src/control/activation_policy.go:1326-1333). "
            "G024 does not force promotion. CUDA embed identity vllm-cuda-minilm-embed exists at "
            "parity (same model/artifact/cosine contract) but is non-routable."
        ),
        "vllm_cuda_cells": vllm,
        "cuda_embed_cells": cuda_embed,
        "cuda_batch_infer_cells": cuda_batch,
        "promotion_touches": [
            "promotionMatchedPairAuthorityRefusal — G024 reports, does not close",
            "activation_policy.go:1326-1333 scope/global — G024 reports, does not close",
        ],
    })

# 4. Advertised surface still Metal-only until promotion.
blockers.append({
    "id": "advertised_surface_is_metal_only_until_promotion",
    "detail": (
        "Ordinary advertised surface remains candle_metal only. Multi-family freeze "
        "activates when a second family is legitimately advertised under a quality "
        "contract — not by editing DRAFT to ACTIVE."
    ),
    "active_candle_jobs": sorted(active_candle_jobs),
})

# 5. Arm C directed-routing ban is code-enforced in RuntimeDecision note + harness.
if "must never be presented as selector proof for Arm C" not in decision:
    blockers.append({
        "id": "directed_arm_c_ban_missing",
        "detail": "RuntimeDecision directed note must forbid Arm C selector-proof claims",
    })
else:
    software_ready.append({
        "id": "directed_arm_c_ban",
        "detail": "Directed freeze explicitly forbidden as Arm C selector proof",
    })

# Directed-routing check for any execute path: env must not force a cell for Arm C.
directed_cell = ( __import__("os").environ.get("MERC_DIRECTED_CELL_ID")
                  or __import__("os").environ.get("MERC_HETERO_ARM_C_CELL")
                  or "" ).strip()
if directed_cell:
    blockers.append({
        "id": "arm_c_directed_cell_refused",
        "detail": (
            f"Arm C was directed onto cell {directed_cell!r}. Manually directing one cell "
            "and calling it selector proof is forbidden. Unset MERC_DIRECTED_CELL_ID / "
            "MERC_HETERO_ARM_C_CELL and let ordinary multi-family admission select."
        ),
    })

# Runnable only when software multi-family path exists, quality contracts exist,
# no directed Arm C, AND at least one CUDA cell is ordinary-routable under contract.
runnable = (
    "selectAdmissionCandidates" in workload
    and bool(embed_c)
    and bool(routable_cuda)
    and not directed_cell
    and "HETEROGENEOUS_ELIGIBLE_SET" in decision
)

payload = {
    "schema_version": 2,
    "kind": "heterogeneous_placement_principal",
    "status": "READY_PENDING_ROUTABLE_CUDA" if (
        "selectAdmissionCandidates" in workload and embed_c and not routable_cuda and not directed_cell
    ) else ("REFUSED_STRUCTURAL" if not runnable else "PRECONDITIONS_PASS"),
    "evidence_class": "REFUSAL" if not runnable else "PRECONDITION",
    "gate_passed": False,
    "comparable": False,
    "arms": {
        "A": "FIXED_CUDA — blocked until a CUDA cell is legitimately routable under quality contract",
        "B": "FIXED_METAL — catalogue candle embed/batch_infer",
        "C": "MERC_CHOOSES — multi-family ordinary admission under AcceptableQualityContract; directed freezes refused",
    },
    "requested_mix": [
        "embeddings under embed-cosine-v2-all-minilm-l6-v2",
        "batch_infer only if matched precision quality contract exists (q4-vs-bf16 REFUSED)",
    ],
    "intentional_duplicates": 0,
    "software_ready": software_ready,
    "blockers": blockers,
    "measured_arms": None,
    "aggregate": None,
    "deadline_failures": None,
    "power_analysis": "evidence/perf/heterogeneous-placement-power-analysis-latest.json",
    "verdict": {
        "c_beats_a_and_b_across_mix": None,
        "confidence": "n/a — experiment not run",
        "blunt": (
            "G024 software path: multi-family admission + quality contracts + RuntimeDecision "
            "citation + Arm C directed ban are in tree. Production still cannot run Arm C on "
            "CUDA because CUDA cells remain DRAFT and promotion is refused for missing matched "
            "pair authority and global-lifecycle coverage — not because admission still freezes "
            "a Metal-only singleton by design. Metal q4 vs CUDA bf16 generation remains REFUSED "
            "as a quality contract. Only hardware + legitimate promotion remain for embed; "
            "generation needs matched precision first. Authorise ~$3 A40 at $0.44/hr per power analysis."
        ),
    },
    "money": {
        "spent_usd_this_receipt": 0.0,
        "spend_receipts": [],
        "note": "No pod was created by this harness. Governed experiment was not entered.",
        "authorisation_ask_usd": 3.0,
        "instance": "RunPod NVIDIA A40 @ $0.44/hr",
    },
    "does_not_prove": [
        "that Merc heterogeneous placement beats or loses to fixed CUDA or Metal on a mixed workload",
        "that CUDA cells may be promoted (gate v4 still refuses)",
        "cross-hardware cost ranking",
        "that Metal q4 and CUDA bf16 generation are the same product",
    ],
    "limitations": [
        "Refusal/readiness is derived from repository authority and code paths on this commit.",
        "PromotionMatchedPairAuthorityRefusal and activation scope/global-lifecycle are untouched.",
    ],
    "what_would_unblock": [
        "Legitimate promotion of vllm-cuda-minilm-embed under embed-cosine-v2 (matched-pair + global coverage) — not a status edit",
        "For generation: CUDA cell loading the same Q4_K_M GGUF under byte_exact, or a new task-outcome contract",
        "Then: governed runpod-vllm.sh experiment with Metal + CUDA concurrent, Arm C undirected, power-analysis sample plan",
    ],
}
print(json.dumps({"runnable": runnable, "payload": payload}))
sys.exit(0)
PY
}

write_refusal_receipt() {
  local probe_json="$1"
  local payload_file
  payload_file="$(mktemp -t merc-hetero-payload.XXXXXX.json)"
  python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["payload"], indent=2))' \
    <<<"$probe_json" >"$payload_file"

  python3 "$ROOT/ops/scripts/write-bound-evidence.py" \
    --out "$RECEIPT_REL" \
    --harness "ops/scripts/heterogeneous-placement-drive.sh" \
    --payload-file "$payload_file" \
    --exact-config "MERC_HETERO_PLACEMENT=1 mode=$MODE structural precondition refusal; no pods created" \
    --raw-samples "none — experiment refused before measurement" \
    --model-na "no model weights in this refusal" \
    --image-na "no container image in this refusal" \
    --corpus-na "no external corpus in this refusal" \
    --build-binary "$ROOT/ops/scripts/heterogeneous-placement-drive.sh"

  cp "$RECEIPT_REL" "$RECEIPT_LATEST"
  rm -f "$payload_file"
  say "wrote $RECEIPT_REL"
  say "wrote $RECEIPT_LATEST"
}

case "$MODE" in
  check|--check|"")
    PROBE="$(probe_preconditions)"
    RUNNABLE="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["runnable"])' <<<"$PROBE")"
    write_refusal_receipt "$PROBE"
    if [[ "$RUNNABLE" == "True" || "$RUNNABLE" == "true" ]]; then
      say "preconditions pass; re-invoke with --execute under runpod-vllm.sh experiment"
      exit 0
    fi
    say "REFUSED: mixed three-arm workload cannot be driven end-to-end on this tree"
    exit 3
    ;;
  --execute)
    # Paid path. Only legal under MERC_RUNPOD_EXPERIMENT_CMD.
    if [[ -z "${MERC_RUNPOD_POD_ID:-}" || -z "${MERC_GPU_ENDPOINT:-}" ]]; then
      die "--execute requires parent ops/scripts/runpod-vllm.sh experiment (MERC_RUNPOD_POD_ID / MERC_GPU_ENDPOINT)"
    fi
    # Arm C must select, not be directed. Refuse before any burn.
    if [[ -n "${MERC_DIRECTED_CELL_ID:-}" || -n "${MERC_HETERO_ARM_C_CELL:-}" ]]; then
      die "refusing Arm C: MERC_DIRECTED_CELL_ID/MERC_HETERO_ARM_C_CELL is set — directed placement is not selector proof"
    fi
    PROBE="$(probe_preconditions)"
    RUNNABLE="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["runnable"])' <<<"$PROBE")"
    if [[ "$RUNNABLE" != "True" && "$RUNNABLE" != "true" ]]; then
      write_refusal_receipt "$PROBE"
      die "refusing to burn pod time: structural preconditions still fail (see $RECEIPT_REL)"
    fi
    die "execute path not implemented on this tree: preconditions pass only when a CUDA cell is ordinary-routable under quality contract (promotion still blocked)"
    ;;
  *)
    die "unknown mode: $MODE (use check or --execute)"
    ;;
esac
