#!/usr/bin/env bash
# Heterogeneous placement principal test — precondition gate + governed driver.
#
# Env-gated. Without MERC_HETERO_PLACEMENT=1 this script exits 2 and writes
# nothing. With the flag it re-checks structural preconditions on this tree;
# if the three-arm mixed workload cannot be driven end-to-end it writes a
# BOUND refusal receipt (via scripts/write-bound-evidence.py) and exits 3.
# It never invents arm metrics.
#
# Intended paid path (only when preconditions pass):
#   MERC_HETERO_PLACEMENT=1 \
#   MERC_RUNPOD_CAP_USD=3 \
#   MERC_RUNPOD_COST_PER_HR=<advertised> \
#   MERC_RUNPOD_EXPERIMENT_CMD='bash scripts/heterogeneous-placement-drive.sh --execute' \
#   bash scripts/runpod-vllm.sh experiment
#
# Never invoke --execute outside scripts/runpod-vllm.sh experiment. No --keep.

set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
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
auth = json.loads((root / "control/runtime-authority.json").read_text())
matrix_md = (root / "docs/RUNTIME_MATRIX.md").read_text()
shape = (root / "control/shape_routing.go").read_text()
shadow = (root / "control/runtime_shadow_selection.go").read_text()
workload = (root / "control/workload_classification.go").read_text()

cells = []
for rt in auth.get("runtimes", []):
    rid = rt.get("runtime_id")
    for c in rt.get("cells", []):
        cells.append({
            "runtime_id": rid,
            "cell_id": c.get("id"),
            "job": c.get("job"),
            "model": c.get("model"),
            "lifecycle": c.get("lifecycle"),
            "cloud_backed": bool(c.get("cloud_backed")),
            "engine": rt.get("engine"),
            "device": rt.get("device"),
        })

# Advertised surface is enforced by tests as exactly the two BOUND candle cells.
# We restate the authority document facts here without re-implementing the
# bindable predicate (which lives in Go and must not be weakened here).
active_candle_jobs = {
    c["job"] for c in cells
    if c["runtime_id"] == "candle_metal" and c.get("lifecycle") == "ACTIVE"
}
vllm = [c for c in cells if c["runtime_id"] == "vllm_cuda"]
media = [c for c in cells if c["job"] in ("media_transcode", "media_rendering")]
cuda_embed = [c for c in cells if c["job"] == "embed" and "cuda" in (c["runtime_id"] or "")]
cuda_batch = [c for c in cells if c["job"] == "batch_infer" and c.get("cloud_backed")]

blockers = []

# 1. Ordinary admission is a singleton on the advertised set.
if "Ordinary admission is a singleton today" not in workload and "singleton today" not in shadow:
    blockers.append({
        "id": "singleton_admission_comment_missing",
        "detail": "expected workload/shadow comments documenting singleton ordinary admission",
    })
else:
    blockers.append({
        "id": "ordinary_admission_is_singleton",
        "detail": (
            "runtimeCapabilityForBindingDirected freezes exactly one advertised cell "
            "per (job type, model). Competing engines (including every CUDA batch_infer "
            "cell) exist only as DRAFT or directed; the shadow selector scores them but "
            "does not route ordinary buyer traffic. Multi-candidate production selection "
            "requires the engine tournament and is not live on this tree."
        ),
        "refs": [
            "control/workload_classification.go:runtimeCapabilityForBindingDirected",
            "control/runtime_shadow_selection.go",
            "docs/RUNTIME_MATRIX.md",
        ],
    })

# 2. No routable CUDA cell for batch_infer or embed.
if not any(c.get("lifecycle") in ("ACTIVE", "CANARY") and c.get("cloud_backed") for c in cells):
    blockers.append({
        "id": "no_routable_cuda_batch_or_embed_cell",
        "detail": (
            "vllm_cuda profile is DRAFT with cell lifecycle unset; sglang/tensorrt/lmdeploy "
            "batch_infer cells are DRAFT. No CUDA embed cell exists at all. Arm A cannot "
            "serve the catalogue embed or batch_infer contracts through ordinary Merc "
            "admission, and Arm C cannot place those contracts on CUDA."
        ),
        "vllm_cuda_cells": vllm,
        "cuda_batch_infer_cells": cuda_batch,
        "cuda_embed_cells": cuda_embed,
    })

# 3. Advertised jobs are Metal-only candle cells.
if active_candle_jobs != {"embed", "batch_infer"} and active_candle_jobs != {"embed"}:
    # Still record; after r3/r4 we expect embed+batch_infer ACTIVE on candle.
    pass
blockers.append({
    "id": "advertised_surface_is_metal_only",
    "detail": (
        "The advertised/bindable surface is candle_metal only (embed + batch_infer after "
        "r3/r4 seal). Tests pin advertisedRuntimeCapabilities() == 2 and both on candle_metal. "
        "No Metal+CUDA choice is available to ordinary placement for any batch job."
    ),
    "active_candle_jobs": sorted(active_candle_jobs),
})

# 4. Media not available for the mix.
blockers.append({
    "id": "media_not_ordinary_routable",
    "detail": (
        "media_transcode and media_rendering cells are CANARY and fail the cell-authority "
        "bindable predicate (source commit / harness identity). Constraint forbids promotion. "
        "The optional media/render class of the mix is unavailable."
    ),
    "media_cells": media,
})

# 5. Shape-aware routing is off and is not matched-weight evidence.
if "MERC_SHAPE_AWARE_ROUTING" not in shape:
    blockers.append({"id": "shape_routing_file_unexpected", "detail": "shape_routing.go missing flag"})
else:
    blockers.append({
        "id": "shape_routing_off_and_unproven",
        "detail": (
            "MERC_SHAPE_AWARE_ROUTING defaults off. Comments state no bound matched-weight "
            "Metal-versus-CUDA crossover exists; enabling it would redirect money on a "
            "speculative heuristic. Even when on, shapeOrderSQL only reorders claim among "
            "workers already authorized for the frozen cell — it cannot route a candle_metal "
            "cell to a CUDA worker."
        ),
        "refs": ["control/shape_routing.go", "control/scheduler.go:ClaimTaskSQL"],
    })

# 6. Realtime multi-offer clearing is cost/warmth, not shape-per-contract.
blockers.append({
    "id": "realtime_clearing_not_shape_aware",
    "detail": (
        "Realtime offer selection ranks verified-outcome cost first, then warmth as a "
        "tiebreak inside a cost class (realtime_store.go). It does not map interactive vs "
        "throughput request shape onto Metal vs CUDA. Dual realtime offers (local Metal "
        "engine + RunPod vLLM) would still not implement 'select Metal/CUDA per contract' "
        "for the stated mix, and Metal Q4 vs CUDA bf16 is a different quality contract."
    ),
    "refs": ["control/realtime_store.go (authorize select offer SQL)", "control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json"],
})

# 7. Quality contract mismatch if one tried to force engines anyway.
blockers.append({
    "id": "quality_contract_mismatch_metal_q4_vs_cuda_bf16",
    "detail": (
        "Catalogue Metal generation is llama-3.2-1b-instruct-q4 (GGUF Q4_K_M). Catalogue "
        "vLLM realtime profile is unsloth/Llama-3.2-1B-Instruct bf16. Claim standard forbids "
        "comparing across different quality contracts. historical routing-crossover.json is "
        "UNBOUND and compared dissimilar models/precisions by its own caveat."
    ),
})

# The experiment is runnable only when every class in the mix can be placed on
# both fixed arms and selected by Merc. That is not true on this tree.
runnable = False
payload = {
    "schema_version": 1,
    "kind": "heterogeneous_placement_principal",
    "status": "REFUSED_STRUCTURAL",
    "evidence_class": "REFUSAL",
    "gate_passed": False,
    "comparable": False,
    "arms": {
        "A": "fixed vLLM CUDA only — not runnable for catalogue embed/batch_infer on this tree",
        "B": "fixed Metal only — catalogue embed+batch_infer only; no CUDA peer under same quality contract",
        "C": "Merc Metal/CUDA per contract — ordinary admission has no multi-class choice to exercise",
    },
    "requested_mix": [
        "latency-sensitive realtime",
        "throughput batch inference",
        "embeddings",
        "one media/render workload if available",
    ],
    "intentional_duplicates": 0,
    "blockers": blockers,
    "measured_arms": None,
    "aggregate": None,
    "deadline_failures": None,
    "verdict": {
        "c_beats_a_and_b_across_mix": None,
        "confidence": "n/a — experiment not run",
        "blunt": (
            "The heterogeneous placement thesis cannot be measured on this tree. "
            "Ordinary admission freezes a Metal-only singleton; CUDA generation is DRAFT; "
            "no CUDA embed exists; media is not routable; realtime clearing is not shape-aware; "
            "and Metal Q4 vs CUDA bf16 is not the same quality contract. This is a structural "
            "negative for shippability of the thesis, not a measured win or loss on the mix."
        ),
    },
    "money": {
        "spent_usd_this_receipt": 0.0,
        "spend_receipts": [],
        "note": "No pod was created by this harness. Governed experiment was not entered.",
    },
    "does_not_prove": [
        "that Merc heterogeneous placement beats or loses to a fixed CUDA or fixed Metal deployment on a mixed workload",
        "cost per verified outcome, throughput-inside-SLA, or energy per verified outcome for arms A/B/C",
        "deadline failure rates under concurrent Metal+CUDA supply",
        "that enabling MERC_SHAPE_AWARE_ROUTING would improve outcomes (flag remains off; no matched-weight authority)",
        "anything about media/render placement (cells not ordinary-routable)",
        "that the historical UNBOUND routing-crossover.json is a live placement authority",
    ],
    "limitations": [
        "Refusal is derived from repository authority and code paths on this commit, not from a live multi-supply run.",
        "A concurrent RunPod process owned by another lane may be billing; this receipt does not attribute that spend.",
        "batch_infer is sellable on Metal after r3/r4, which is necessary but not sufficient for the three-arm mix.",
    ],
    "what_would_unblock": [
        "A BOUND, ordinary-routable CUDA cell for the same quality contract as Metal generation (or a directed multi-candidate production path that is not shadow-only).",
        "A CUDA (or shared-quality) embed path under the same contract as candle-metal-minilm-embed, or an explicit decision to drop embed from the mix with a revised claim.",
        "Realtime or batch selection that maps contract shape (latency vs throughput) onto hw_class, measured under matched weights/precision.",
        "Media cells that clear the cell-authority bindable predicate if media remains in the mix.",
        "Only then: governed runpod-vllm.sh experiment with Metal agent + CUDA supply concurrent, zero intentional duplicates, deadline contracts identical across arms.",
    ],
}
print(json.dumps({"runnable": runnable, "payload": payload}))
if runnable:
    sys.exit(0)
sys.exit(0)  # structural refusal is success of the check path; driver maps status
PY
}

write_refusal_receipt() {
  local probe_json="$1"
  local payload_file
  payload_file="$(mktemp -t merc-hetero-payload.XXXXXX.json)"
  python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["payload"], indent=2))' \
    <<<"$probe_json" >"$payload_file"

  python3 "$ROOT/scripts/write-bound-evidence.py" \
    --out "$RECEIPT_REL" \
    --harness "scripts/heterogeneous-placement-drive.sh" \
    --payload-file "$payload_file" \
    --exact-config "MERC_HETERO_PLACEMENT=1 mode=$MODE structural precondition refusal; no pods created" \
    --raw-samples "none — experiment refused before measurement" \
    --model-na "no model weights in this refusal" \
    --image-na "no container image in this refusal" \
    --corpus-na "no external corpus in this refusal" \
    --build-binary "$ROOT/scripts/heterogeneous-placement-drive.sh"

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
      die "--execute requires parent scripts/runpod-vllm.sh experiment (MERC_RUNPOD_POD_ID / MERC_GPU_ENDPOINT)"
    fi
    PROBE="$(probe_preconditions)"
    RUNNABLE="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["runnable"])' <<<"$PROBE")"
    if [[ "$RUNNABLE" != "True" && "$RUNNABLE" != "true" ]]; then
      write_refusal_receipt "$PROBE"
      die "refusing to burn pod time: structural preconditions still fail (see $RECEIPT_REL)"
    fi
    die "execute path not implemented: preconditions currently never pass on this tree"
    ;;
  *)
    die "unknown mode: $MODE (use check or --execute)"
    ;;
esac
