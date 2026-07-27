#!/usr/bin/env python3
"""Multi-source region proposal ensemble on hard ocular conditions.

Pipeline:
  render (Blender EEVEE or diagnostic synthetic)
  → six independent proposal sources (pixels only; no GT)
  → non-destructive evidence fusion (split/merge hypotheses preserved)
  → sealed evaluator scores proposal recall/precision per source and fused

Exit non-zero if:
  - the first-frame stationary case yields fewer than three proposals, or
  - fused recall on the scored set falls below the declared threshold, or
  - the leakage canary appears in builder inputs, or
  - frozen thresholds disagree with the split-manifest digest.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

import cv2
import numpy as np

_ROOT = Path(__file__).resolve().parents[1]
_SRC = _ROOT / "src"
if str(_SRC) not in sys.path:
    sys.path.insert(0, str(_SRC))
if str(_ROOT / "benchmarks") not in sys.path:
    sys.path.insert(0, str(_ROOT / "benchmarks"))

from blender_vision.ocular.attestation import (  # noqa: E402
    ExecutionClass,
    FailureKind,
    RuntimeAttestation,
    attest_blocked,
    attest_substitute,
    classify_failure,
    run_attested,
)
from blender_vision.ocular.detect import BackgroundModel  # noqa: E402
from blender_vision.ocular.proposals import (  # noqa: E402
    ALL_SOURCES,
    FROZEN_THRESHOLDS,
    FROZEN_THRESHOLDS_DIGEST,
    ProposalContext,
    ProposalStatus,
    assert_no_ground_truth_in_proposals,
    confidence_calibration,
    match_proposals_to_gt,
    propose,
    thresholds_digest,
)
from blender_vision.ocular.verdict import issue_verdict, summarise  # noqa: E402

# Declared up front — a gate that always passes is worthless.
FIRST_FRAME_MIN_PROPOSALS = int(FROZEN_THRESHOLDS["first_frame_min_proposals"])
FUSED_RECALL_MIN = float(FROZEN_THRESHOLDS["fused_recall_min"])
COMPLETION_MARKER = "OCULAR_PROPOSALS_COMPLETE"

HARD_CONDITIONS = [
    "visually_similar",
    "crossing_paths",
    "partial_occlusion",
    "full_occlusion",
    "lighting_change",
    "scale_change",
    "camera_motion",
    "leave_return",
    "distractor_replacement",
    "unknown_entering",
    "permanence",
]


def _blender() -> str | None:
    env = os.environ.get("BVMCP_BLENDER")
    if env and Path(env).is_file():
        return env
    candidate = Path("/Applications/Blender.app/Contents/MacOS/Blender")
    if candidate.is_file():
        return str(candidate)
    return None


def _write_diagnostic_hard(hard_out: Path) -> None:
    from ocular_hard.synthetic import write_all_conditions  # type: ignore[import-not-found]

    write_all_conditions(hard_out)


def render_hard_suite(
    output: Path,
) -> tuple[RuntimeAttestation, Path, RuntimeAttestation | None]:
    hard_out = output / "hard"
    hard_out.mkdir(parents=True, exist_ok=True)
    scene_script = _ROOT / "benchmarks" / "ocular_hard" / "create_scene.py"
    blender = _blender()
    if blender is None:
        _write_diagnostic_hard(hard_out)
        blocked = attest_blocked(
            "blender",
            "Blender executable not found on this host",
            substituted_by="synthetic_hard_sequence",
        )
        sub = attest_substitute(
            "blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            reason="Blender not installed; offline OpenCV hard suite",
            substitute="synthetic_hard_sequence",
            outputs={"marker": hard_out / "OCULAR_HARD_COMPLETE"},
        )
        return blocked, hard_out, sub

    attestation = run_attested(
        "blender",
        [
            blender,
            "--background",
            "--python",
            str(scene_script),
            "--",
            "--output",
            str(hard_out),
        ],
        cwd=_ROOT,
        timeout_seconds=2400,
        version_argv=["--version"],
        expect_marker="OCULAR_HARD_COMPLETE",
        outputs={"marker": hard_out / "OCULAR_HARD_COMPLETE"},
    )
    if attestation.is_physical and (hard_out / "OCULAR_HARD_COMPLETE").is_file():
        return attestation, hard_out, None

    hay_stdout = attestation.stdout_tail or ""
    hay_stderr = attestation.stderr_tail or ""
    if attestation.returncode not in (0, None):
        try:
            failure = classify_failure(
                hay_stdout, hay_stderr, int(attestation.returncode or 1)
            )
        except Exception:  # noqa: BLE001
            failure = FailureKind.UNCLASSIFIED
        crash_note = f"Blender exit {attestation.returncode} classified {failure.value}"
    else:
        crash_note = (
            attestation.blocked_reason
            or "Blender did not produce OCULAR_HARD_COMPLETE"
        )

    _write_diagnostic_hard(hard_out)
    sub = attest_substitute(
        "blender",
        execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
        reason=(
            f"{crash_note}. Synthetic OpenCV hard suite used for proposal "
            "diagnostics only — never promoted to PHYSICAL."
        ),
        substitute="synthetic_hard_sequence",
        outputs={"marker": hard_out / "OCULAR_HARD_COMPLETE"},
    )
    return attestation, hard_out, sub


def _load_split_manifest() -> dict[str, Any]:
    path = _ROOT / "benchmarks" / "ocular_splits" / "manifest.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _run_leakage_canaries() -> dict[str, Any]:
    """Injected secret in hidden-only data must be absent from builder inputs."""
    from ocular_splits import (  # type: ignore[import-not-found]
        assert_canary_absent_from_builder_inputs,
        load_canary,
    )

    canary = load_canary()
    assert_canary_absent_from_builder_inputs(canary)
    return {"canary_present_in_hidden": True, "canary_absent_from_builder": True}


def _assert_thresholds_match_manifest(manifest: dict[str, Any]) -> None:
    frozen = manifest.get("frozen_thresholds") or {}
    expected = manifest.get("frozen_thresholds_digest") or ""
    actual = thresholds_digest(frozen)
    code_digest = FROZEN_THRESHOLDS_DIGEST
    if actual != expected:
        raise AssertionError(
            f"manifest frozen_thresholds digest mismatch: {actual} != {expected}"
        )
    if code_digest != expected:
        raise AssertionError(
            f"code FROZEN_THRESHOLDS_DIGEST {code_digest} != manifest {expected}"
        )
    # Values must also match the in-code table (single authority).
    for key, value in FROZEN_THRESHOLDS.items():
        if key not in frozen:
            raise AssertionError(f"manifest missing frozen threshold {key!r}")
        if frozen[key] != value:
            raise AssertionError(
                f"manifest threshold {key}={frozen[key]!r} != code {value!r}"
            )


def _gt_boxes_for_frame(
    condition_dir: Path, frame_index: int
) -> list[tuple[float, float, float, float]]:
    """Sealed evaluator only — open GT boxes for scoring."""
    sealed = condition_dir / "sealed_gt" / f"frame_{frame_index + 1:04d}.json"
    if not sealed.is_file():
        return []
    payload = json.loads(sealed.read_text(encoding="utf-8"))
    boxes: list[tuple[float, float, float, float]] = []
    for obj in payload.get("objects") or []:
        if not obj.get("visible"):
            continue
        role = obj.get("role") or "primary"
        if role in {"occluder"}:
            continue
        box = obj.get("bbox_xywh_px") or [0, 0, 0, 0]
        if box[2] <= 0 or box[3] <= 0:
            continue
        boxes.append((float(box[0]), float(box[1]), float(box[2]), float(box[3])))
    return boxes


def run_proposals_on_condition(condition_dir: Path) -> dict[str, Any]:
    """Builder path: pixels only. Never opens sealed GT."""
    manifest_path = condition_dir / "sequence_manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    for frame_meta in manifest["frames"]:
        if "ground_truth" in frame_meta:
            raise AssertionError(
                "ground_truth path present in builder-visible sequence_manifest"
            )

    frames: list[np.ndarray] = []
    for frame_meta in manifest["frames"]:
        image = cv2.imread(str(condition_dir / frame_meta["image"]), cv2.IMREAD_COLOR)
        if image is None:
            raise RuntimeError(f"failed to read {frame_meta['image']}")
        frames.append(image)

    # Background model over the full sequence (static-camera temporal source).
    # One source among several — never the sole detector.
    bg_model: BackgroundModel | None = None
    if len(frames) >= 3:
        bg_model = BackgroundModel(
            frames, threshold=int(FROZEN_THRESHOLDS["background_threshold"])
        )

    per_frame: list[dict[str, Any]] = []
    prev: np.ndarray | None = None
    points = None
    # Known masks accumulate from fused proposals of prior frames for source F.
    known_masks: list[np.ndarray] = []
    known_embeddings: list[list[float]] = []

    for frame_meta, image in zip(manifest["frames"], frames, strict=True):
        fi = int(frame_meta["frame_index"])
        ctx = ProposalContext(
            previous_image=prev,
            background_model=bg_model,
            depth=None,  # hard fixtures do not always emit a Z-pass
            known_masks=list(known_masks),
            known_embeddings=list(known_embeddings),
            prev_points=points,
        )
        result = propose(image, frame_index=fi, timestamp=float(fi), context=ctx)
        graph = result.graph
        points = result.next_points
        assert_no_ground_truth_in_proposals(graph.proposals)

        source_counts = {
            src.value: len(
                [
                    p
                    for p in graph.proposals
                    if p.source is src
                    and p.status is ProposalStatus.ACTIVE
                    and p.area_px > 0
                ]
            )
            for src in ALL_SOURCES
        }
        per_frame.append(
            {
                "frame_index": fi,
                "n_raw": len(graph.active_proposals()),
                "n_fused": len(graph.fused),
                "n_split": len(graph.split_hypotheses),
                "n_merge": len(graph.merge_hypotheses),
                "source_counts": source_counts,
                "source_reports": [r.to_dict() for r in graph.source_reports],
                "fused": [p.to_dict() for p in graph.fused],
                "graph": graph,  # kept for sealed scoring; stripped on serialize
            }
        )
        # Update known set from fused proposals (perception-derived identities only).
        known_masks = [p.mask for p in graph.fused if p.mask is not None]
        known_embeddings = [list(p.appearance_embedding) for p in graph.fused]
        prev = image

    return {
        "condition": manifest.get("condition"),
        "source": manifest.get("source"),
        "n_frames": len(frames),
        "per_frame": per_frame,
    }


def sealed_score_condition(
    condition_dir: Path, builder_result: dict[str, Any]
) -> dict[str, Any]:
    """Open sealed GT only here. Per-source and fused metrics."""
    per_source: dict[str, list[dict[str, Any]]] = {s.value: [] for s in ALL_SOURCES}
    fused_rows: list[dict[str, Any]] = []
    first_frame: dict[str, Any] | None = None

    for row in builder_result["per_frame"]:
        fi = int(row["frame_index"])
        graph = row["graph"]
        gt_boxes = _gt_boxes_for_frame(condition_dir, fi)

        fused_metrics = match_proposals_to_gt(graph.fused, gt_boxes)
        fused_metrics["frame_index"] = fi
        fused_metrics["calibration"] = confidence_calibration(graph.fused, gt_boxes)
        fused_rows.append(fused_metrics)

        for src in ALL_SOURCES:
            props = [
                p
                for p in graph.proposals
                if p.source is src
                and p.status is ProposalStatus.ACTIVE
                and p.area_px > 0
            ]
            m = match_proposals_to_gt(props, gt_boxes)
            m["frame_index"] = fi
            per_source[src.value].append(m)

        if fi == 0:
            first_frame = {
                "n_fused": len(graph.fused),
                "n_gt": len(gt_boxes),
                "fused_centroids": [list(p.centroid_xy) for p in graph.fused],
                "metrics": fused_metrics,
            }

    def _mean(rows: list[dict[str, Any]], key: str) -> float:
        if not rows:
            return 0.0
        return float(np.mean([float(r[key]) for r in rows]))

    source_summary = {
        src: {
            "mean_recall": _mean(rows, "recall"),
            "mean_precision": _mean(rows, "precision"),
            "mean_count_error": _mean(rows, "object_count_error"),
            "mean_fpr": _mean(rows, "false_positive_rate"),
            "mean_n_proposals": _mean(rows, "n_proposals"),
        }
        for src, rows in per_source.items()
    }
    fused_summary = {
        "mean_recall": _mean(fused_rows, "recall"),
        "mean_precision": _mean(fused_rows, "precision"),
        "mean_count_error": _mean(fused_rows, "object_count_error"),
        "mean_fpr": _mean(fused_rows, "false_positive_rate"),
        "mean_n_proposals": _mean(fused_rows, "n_proposals"),
    }
    return {
        "condition": builder_result.get("condition"),
        "first_frame": first_frame,
        "per_source": source_summary,
        "fused": fused_summary,
        "per_frame_fused": [
            {k: v for k, v in r.items() if k != "pairs"} for r in fused_rows
        ],
    }


def _print_source_table(scores: list[dict[str, Any]]) -> None:
    """Visible per-source table — which source carries which condition."""
    headers = ["condition", "fused_R", "fused_P", "fused_CE"] + [
        f"{s.value[:4]}_R" for s in ALL_SOURCES
    ]
    print("\n=== Per-source proposal recall (R) / fused precision (P) / count err (CE) ===")
    print("  ".join(f"{h:>10}" for h in headers))
    for sc in scores:
        cond = str(sc.get("condition") or "?")[:10]
        fused = sc.get("fused") or {}
        row = [
            f"{cond:>10}",
            f"{fused.get('mean_recall', 0):>10.2f}",
            f"{fused.get('mean_precision', 0):>10.2f}",
            f"{fused.get('mean_count_error', 0):>10.2f}",
        ]
        for src in ALL_SOURCES:
            r = (sc.get("per_source") or {}).get(src.value, {}).get("mean_recall", 0.0)
            row.append(f"{r:>10.2f}")
        print("  ".join(row))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=_ROOT / "artifacts" / "ocular" / "proposals",
    )
    parser.add_argument(
        "--conditions",
        nargs="*",
        default=None,
        help="Subset of hard conditions (default: all).",
    )
    parser.add_argument(
        "--skip-render",
        action="store_true",
        help="Reuse existing hard/ under --output.",
    )
    parser.add_argument(
        "--synthetic-only",
        action="store_true",
        help="Force diagnostic OpenCV fixtures (never claims PHYSICAL).",
    )
    args = parser.parse_args(argv)
    output: Path = args.output
    output.mkdir(parents=True, exist_ok=True)

    # --- split discipline + canaries before any sealed evaluation ---
    split_manifest = _load_split_manifest()
    _assert_thresholds_match_manifest(split_manifest)
    canary_receipt = _run_leakage_canaries()

    if args.skip_render and (output / "hard" / "OCULAR_HARD_COMPLETE").is_file():
        hard_dir = output / "hard"
        # Diagnostic attestation when reusing fixtures without a fresh render.
        primary = attest_blocked(
            "blender",
            "reuse of existing hard fixtures via --skip-render",
            substituted_by="existing_hard_suite",
        )
        substitute = None
    elif args.synthetic_only:
        hard_dir = output / "hard"
        hard_dir.mkdir(parents=True, exist_ok=True)
        _write_diagnostic_hard(hard_dir)
        primary = attest_blocked(
            "blender",
            "synthetic-only flag; Blender not invoked",
            substituted_by="synthetic_hard_sequence",
        )
        substitute = attest_substitute(
            "blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            reason="--synthetic-only",
            substitute="synthetic_hard_sequence",
            outputs={"marker": hard_dir / "OCULAR_HARD_COMPLETE"},
        )
    else:
        primary, hard_dir, substitute = render_hard_suite(output)

    conditions = list(args.conditions or HARD_CONDITIONS)
    builder_results: dict[str, Any] = {}
    scores: list[dict[str, Any]] = []
    first_frame_ok = False
    first_frame_detail: dict[str, Any] = {}

    for cond in conditions:
        cond_dir = hard_dir / cond
        if not cond_dir.is_dir():
            print(f"skip missing condition dir {cond_dir}")
            continue
        print(f"proposals: {cond}")
        builder = run_proposals_on_condition(cond_dir)
        # Strip non-serialisable graph objects before writing builder receipt.
        serialisable_frames = []
        for row in builder["per_frame"]:
            serialisable_frames.append(
                {k: v for k, v in row.items() if k != "graph"}
            )
        builder_results[cond] = {
            "condition": builder["condition"],
            "source": builder["source"],
            "n_frames": builder["n_frames"],
            "per_frame": serialisable_frames,
        }
        scored = sealed_score_condition(cond_dir, builder)
        scores.append(scored)

        if cond == "visually_similar" and scored.get("first_frame"):
            ff = scored["first_frame"]
            first_frame_detail = ff
            first_frame_ok = int(ff.get("n_fused") or 0) >= FIRST_FRAME_MIN_PROPOSALS

    _print_source_table(scores)

    mean_fused_recall = float(
        np.mean([float((s.get("fused") or {}).get("mean_recall") or 0.0) for s in scores])
        if scores
        else 0.0
    )
    print(f"\nfirst-frame stationary (visually_similar): {first_frame_detail}")
    print(
        f"first_frame_ok={first_frame_ok} "
        f"(need >= {FIRST_FRAME_MIN_PROPOSALS} fused proposals)"
    )
    print(
        f"mean fused recall={mean_fused_recall:.3f} "
        f"(threshold {FUSED_RECALL_MIN:.3f})"
    )

    # Evaluator contract.
    evaluator_passed = first_frame_ok and mean_fused_recall >= FUSED_RECALL_MIN
    evaluator_detail = (
        f"first_frame_ok={first_frame_ok}, "
        f"mean_fused_recall={mean_fused_recall:.3f}, "
        f"threshold={FUSED_RECALL_MIN:.3f}"
    )

    report = {
        "lane": "ocular-proposals",
        "thresholds_digest": FROZEN_THRESHOLDS_DIGEST,
        "canary": canary_receipt,
        "first_frame_stationary": first_frame_detail,
        "first_frame_ok": first_frame_ok,
        "mean_fused_recall": mean_fused_recall,
        "fused_recall_min": FUSED_RECALL_MIN,
        "scores": scores,
        "builder_results": builder_results,
        "primary_attestation": primary.to_dict() if hasattr(primary, "to_dict") else {},
        "substitute_attestation": (
            substitute.to_dict()
            if substitute is not None and hasattr(substitute, "to_dict")
            else None
        ),
    }
    report_path = output / "proposals_report.json"
    report_path.write_text(json.dumps(report, indent=2, default=str), encoding="utf-8")

    # Completion marker observed by issue_verdict.
    marker_path = output / "OCULAR_PROPOSALS_COMPLETE"
    marker_path.write_text(
        f"{COMPLETION_MARKER} first_frame_ok={first_frame_ok} "
        f"mean_fused_recall={mean_fused_recall:.4f}\n",
        encoding="utf-8",
    )
    # Ensure the attestation used for the verdict saw the marker in stdout_tail
    # when physical; for diagnostic paths issue_verdict still records reasons.
    if primary.execution_class is ExecutionClass.PHYSICAL:
        # Re-issue is not possible; append marker to a local diagnostic attestation
        # is wrong. Physical path already has OCULAR_HARD_COMPLETE. Record proposal
        # completion separately via required_artifacts.
        att_for_verdict = primary
        expected_marker = "OCULAR_HARD_COMPLETE"
    else:
        # Diagnostic substitute carries the synthetic completion.
        att_for_verdict = substitute if substitute is not None else primary
        expected_marker = None

    verdict = issue_verdict(
        "ocular-proposals",
        att_for_verdict,
        expected_marker=expected_marker,
        required_artifacts={"report": report_path, "marker": marker_path},
        evaluator_passed=evaluator_passed,
        evaluator_detail=evaluator_detail,
        result_ok=evaluator_passed,
    )
    verdict_path = output / "verdict.json"
    verdict_path.write_text(
        json.dumps(summarise(verdict), indent=2), encoding="utf-8"
    )
    print(f"\nverdict: {verdict.status.value}")
    print(f"reasons: {verdict.reasons}")
    print(f"report: {report_path}")
    print(COMPLETION_MARKER)

    if not evaluator_passed:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
