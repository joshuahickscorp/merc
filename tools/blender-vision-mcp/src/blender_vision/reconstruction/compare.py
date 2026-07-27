"""Pairwise candidate comparison without a single scalar score."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.reconstruction.base import MeshGeometry
from blender_vision.reconstruction.mesh_ops import (
    chamfer_distance,
    load_mesh_artifact,
    surface_area,
    topology_report,
    volume,
)
from blender_vision.v2.records import ReconstructionCandidate


def compare_candidates(
    left: ReconstructionCandidate,
    right: ReconstructionCandidate,
    *,
    samples: int = 1500,
) -> dict[str, Any]:
    """Compare two candidates metric-by-metric with per-metric winners.

    Deliberately does not emit a single fused score that could hide disagreement.
    """
    mesh_l = _load_candidate_mesh(left)
    mesh_r = _load_candidate_mesh(right)
    metrics: dict[str, Any] = {
        "left_id": left.candidate_id,
        "right_id": right.candidate_id,
        "left_backend": left.backend,
        "right_backend": right.backend,
        "both_executed": left.executed and right.executed,
    }
    winners: dict[str, str] = {}

    have_meshes = (
        mesh_l is not None
        and mesh_r is not None
        and not mesh_l.is_empty()
        and not mesh_r.is_empty()
    )
    if have_meshes:
        chamfer = chamfer_distance(mesh_l, mesh_r, samples=samples)
        metrics["chamfer_distance"] = chamfer
        # Lower chamfer is better agreement, not a winner of quality alone.
        winners["chamfer_agreement"] = "tie"

        vol_l = volume(mesh_l)
        vol_r = volume(mesh_r)
        area_l = surface_area(mesh_l)
        area_r = surface_area(mesh_r)
        metrics["volume"] = {"left": vol_l, "right": vol_r}
        metrics["surface_area"] = {"left": area_l, "right": area_r}
        metrics["volume_ratio"] = _ratio(vol_l, vol_r)
        metrics["surface_area_ratio"] = _ratio(area_l, area_r)
        # For ratio metrics, closer to 1.0 is better agreement when comparing two
        # reconstructions of the same object; still report absolute values.
        winners["volume_closer_to_peer"] = "tie"
        winners["surface_area_closer_to_peer"] = "tie"

        topo_l = topology_report(mesh_l)
        topo_r = topology_report(mesh_r)
        metrics["topology"] = {
            "left": {
                "manifold": topo_l["manifold"],
                "watertight": topo_l["watertight"],
                "genus_estimate": topo_l["genus_estimate"],
                "non_manifold_edge_count": topo_l["non_manifold_edge_count"],
                "boundary_edge_count": topo_l["boundary_edge_count"],
            },
            "right": {
                "manifold": topo_r["manifold"],
                "watertight": topo_r["watertight"],
                "genus_estimate": topo_r["genus_estimate"],
                "non_manifold_edge_count": topo_r["non_manifold_edge_count"],
                "boundary_edge_count": topo_r["boundary_edge_count"],
            },
        }
        winners["watertight"] = _bool_winner(
            left.candidate_id, right.candidate_id, topo_l["watertight"], topo_r["watertight"]
        )
        winners["manifold"] = _bool_winner(
            left.candidate_id, right.candidate_id, topo_l["manifold"], topo_r["manifold"]
        )
        winners["fewer_non_manifold_edges"] = _lower_winner(
            left.candidate_id,
            right.candidate_id,
            topo_l["non_manifold_edge_count"],
            topo_r["non_manifold_edge_count"],
        )
    else:
        metrics["mesh_comparison"] = "unavailable"
        metrics["reason"] = "one or both candidates lack mesh artifacts"

    metrics["coverage_overlap"] = coverage_overlap(left.coverage, right.coverage)
    metrics["authority"] = {
        "left": left.authority.value,
        "right": right.authority.value,
    }
    metrics["frame_compatible"] = left.frame.compatible_with(right.frame)
    metrics["scale_authority"] = {
        "left": left.scale_authority.value,
        "right": right.scale_authority.value,
    }
    metrics["winners"] = winners
    metrics["disagreements"] = _disagreements(metrics, winners)
    return metrics


def compare_all(candidates: list[ReconstructionCandidate]) -> dict[str, Any]:
    executed = [c for c in candidates if c.executed]
    pairs = []
    for i, left in enumerate(executed):
        for right in executed[i + 1 :]:
            pairs.append(compare_candidates(left, right))
    return {
        "candidate_count": len(candidates),
        "executed_count": len(executed),
        "pair_count": len(pairs),
        "pairs": pairs,
        "note": "No single scalar score; inspect per-metric winners and disagreements.",
    }


def coverage_overlap(left: dict[str, Any], right: dict[str, Any]) -> dict[str, Any]:
    """Heuristic coverage overlap from declared coverage fields."""
    keys_l = set(left) if isinstance(left, dict) else set()
    keys_r = set(right) if isinstance(right, dict) else set()
    if not keys_l and not keys_r:
        return {"jaccard_keys": 0.0, "shared_keys": []}
    shared = sorted(keys_l & keys_r)
    union = keys_l | keys_r
    numeric_agree = {}
    for key in shared:
        lv, rv = left.get(key), right.get(key)
        if isinstance(lv, (int, float)) and isinstance(rv, (int, float)):
            if lv == 0 and rv == 0:
                numeric_agree[key] = 1.0
            else:
                numeric_agree[key] = float(min(lv, rv) / max(abs(lv), abs(rv), 1e-12))
    return {
        "jaccard_keys": len(shared) / max(len(union), 1),
        "shared_keys": shared,
        "numeric_agreement": numeric_agree,
    }


def _load_candidate_mesh(candidate: ReconstructionCandidate) -> MeshGeometry | None:
    for key in ("mesh_ply", "mesh_obj", "ply"):
        path = candidate.artifacts.get(key)
        if path and Path(path).is_file():
            return load_mesh_artifact(Path(path))
    return None


def _ratio(a: float, b: float) -> float:
    if a == 0 and b == 0:
        return 1.0
    if b == 0:
        return float("inf")
    return float(a / b)


def _bool_winner(left_id: str, right_id: str, left: bool, right: bool) -> str:
    if left == right:
        return "tie"
    return left_id if left and not right else right_id


def _lower_winner(left_id: str, right_id: str, left: float, right: float) -> str:
    if left == right:
        return "tie"
    return left_id if left < right else right_id


def _disagreements(metrics: dict[str, Any], winners: dict[str, str]) -> list[str]:
    notes: list[str] = []
    if "volume_ratio" in metrics:
        ratio = metrics["volume_ratio"]
        if np.isfinite(ratio) and (ratio < 0.7 or ratio > 1.3):
            notes.append(f"volume_ratio={ratio:.3f} indicates substantial volume disagreement")
    if "surface_area_ratio" in metrics:
        ratio = metrics["surface_area_ratio"]
        if np.isfinite(ratio) and (ratio < 0.7 or ratio > 1.3):
            notes.append(
                f"surface_area_ratio={ratio:.3f} indicates substantial area disagreement"
            )
    if metrics.get("chamfer_distance", {}).get("chamfer", 0) and metrics.get(
        "chamfer_distance", {}
    ).get("chamfer", 0) > 0.05:
        notes.append(
            f"chamfer={metrics['chamfer_distance']['chamfer']:.4g} exceeds 0.05 unit threshold"
        )
    topo = metrics.get("topology")
    if topo:
        if topo["left"]["watertight"] != topo["right"]["watertight"]:
            notes.append("watertight disagreement")
        if topo["left"]["manifold"] != topo["right"]["manifold"]:
            notes.append("manifold disagreement")
    if not metrics.get("frame_compatible", True):
        notes.append("coordinate frames are incompatible")
    distinct_winners = {w for w in winners.values() if w not in {"tie"}}
    if len(distinct_winners) > 1:
        notes.append("per-metric winners disagree")
    return notes
