"""Governed local archetype retrieval and anisotropic adaptation.

Every retrieved surface is tagged VisibilityState.RETRIEVED_MODEL and can never
be reported as observed. Unreviewed licensing is refused.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.core.models import BackendState
from blender_vision.reconstruction.base import (
    BackendAvailability,
    MeshGeometry,
    ReconstructionInputs,
    TimedRun,
    finalize_candidate,
    unavailable_candidate,
)
from blender_vision.reconstruction.mesh_ops import (
    load_mesh_artifact,
    topology_report,
    write_obj_mesh,
    write_ply_mesh,
)
from blender_vision.v2.authority import (
    AuthorityClass,
    VisibilityState,
    derive,
    visibility_authority_ceiling,
)
from blender_vision.v2.records import ReconstructionCandidate

REVIEWED_RIGHTS = {
    "CC0",
    "CC-BY",
    "LICENSED_REUSABLE",
    "PUBLIC_DOMAIN",
    "SYNTHETIC_OWNED",
    "USER_OWNED",
    "PROCEDURAL_OWNED",
}


class RetrievalBackend:
    name = "retrieval"

    def availability(self) -> BackendAvailability:
        return BackendAvailability(
            state=BackendState.AVAILABLE,
            reason="local archetype library retrieval available",
        )

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        if inputs.library_dir is None or not Path(inputs.library_dir).is_dir():
            return unavailable_candidate(
                backend=self.name,
                reason="retrieval requires library_dir with a local archetype library",
                inputs=inputs,
            )
        library = Path(inputs.library_dir)
        manifest_path = library / "manifest.json"
        if not manifest_path.is_file():
            return unavailable_candidate(
                backend=self.name,
                reason=f"archetype library missing manifest.json: {library}",
                inputs=inputs,
            )
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        archetypes = {
            item["id"]: item for item in manifest.get("archetypes", []) if "id" in item
        }
        archetype_id = inputs.archetype_id or inputs.parameters.get("archetype_id")
        if not archetype_id:
            # Simple nearest by declared category/target_id tags.
            archetype_id = _select_archetype(manifest, inputs.target_id)
        if archetype_id not in archetypes:
            return unavailable_candidate(
                backend=self.name,
                reason=f"archetype not found in library: {archetype_id}",
                inputs=inputs,
            )
        entry = archetypes[archetype_id]
        rights = str(entry.get("licensing") or entry.get("rights") or "unreviewed")
        if rights not in REVIEWED_RIGHTS:
            return unavailable_candidate(
                backend=self.name,
                reason=(
                    f"refusing retrieved archetype {archetype_id}: "
                    f"licensing {rights!r} is unreviewed"
                ),
                inputs=inputs,
            )

        asset_rel = entry.get("mesh") or entry.get("path")
        if not asset_rel:
            return unavailable_candidate(
                backend=self.name,
                reason=f"archetype {archetype_id} has no mesh path",
                inputs=inputs,
            )
        mesh_path = library / asset_rel
        mesh = load_mesh_artifact(mesh_path)
        if mesh is None or mesh.is_empty():
            return unavailable_candidate(
                backend=self.name,
                reason=f"could not load archetype mesh: {mesh_path}",
                inputs=inputs,
            )

        with TimedRun() as timer:
            adapted, adapt_log = adapt_mesh(
                mesh,
                scale=inputs.adaptation_scale
                or tuple(entry.get("default_scale", (1.0, 1.0, 1.0))),
                landmarks_source=inputs.landmarks_source,
                landmarks_target=inputs.landmarks_target,
            )
            work = inputs.ensure_work_dir()
            out_ply = write_ply_mesh(work / f"retrieved_{archetype_id}.ply", adapted)
            out_obj = write_obj_mesh(work / f"retrieved_{archetype_id}.obj", adapted)
            report = topology_report(adapted)
            report["visibility_state"] = VisibilityState.RETRIEVED_MODEL.value
            report["archetype_id"] = archetype_id
            report["adaptation"] = adapt_log

        # Authority capped by visibility: RETRIEVED_MODEL -> MODEL_DERIVED ceiling.
        proposed = visibility_authority_ceiling(VisibilityState.RETRIEVED_MODEL)
        authority = derive(
            inputs.input_authorities or [AuthorityClass.MODEL_DERIVED],
            proposed=proposed,
        )
        return finalize_candidate(
            backend=self.name,
            inputs=inputs,
            authority=authority,
            scale_authority=AuthorityClass.MODEL_DERIVED,
            scale_state="adapted-anisotropic-scale",
            coverage={
                "archetype_id": archetype_id,
                "library": str(library),
                "visibility_state": VisibilityState.RETRIEVED_MODEL.value,
                "source_mesh": str(mesh_path),
            },
            topology_state=report,
            editability="archetype-instance; parametric if source was parametric",
            hidden_surface_assumptions=[
                "entire surface is RETRIEVED_MODEL, not observed",
                "non-rigid adaptation limited to anisotropic scale + landmark affine",
            ],
            artifacts={"mesh_ply": str(out_ply), "mesh_obj": str(out_obj)},
            runtime_seconds=timer.seconds,
            execution_log=(
                f"retrieved {archetype_id} rights={rights}; {adapt_log['summary']}; "
                f"visibility={VisibilityState.RETRIEVED_MODEL.value}"
            ),
            failure_modes=[
                "retrieved geometry is prior, not evidence",
                "unreviewed rights are refused",
            ],
            licensing=rights,
            executed=True,
        )


def adapt_mesh(
    mesh: MeshGeometry,
    *,
    scale: tuple[float, float, float] | list[float],
    landmarks_source: np.ndarray | None,
    landmarks_target: np.ndarray | None,
) -> tuple[MeshGeometry, dict[str, Any]]:
    """Anisotropic scale, then optional landmark-aligned affine (no free warp)."""
    vertices = np.asarray(mesh.vertices, dtype=np.float64).copy()
    sx, sy, sz = (float(scale[0]), float(scale[1]), float(scale[2]))
    vertices *= np.array([sx, sy, sz], dtype=np.float64)
    log: dict[str, Any] = {
        "scale": [sx, sy, sz],
        "affine": None,
        "summary": f"anisotropic scale=({sx:.4g},{sy:.4g},{sz:.4g})",
    }
    if (
        landmarks_source is not None
        and landmarks_target is not None
        and len(landmarks_source) >= 3
        and len(landmarks_source) == len(landmarks_target)
    ):
        src = np.asarray(landmarks_source, dtype=np.float64) * np.array([sx, sy, sz])
        dst = np.asarray(landmarks_target, dtype=np.float64)
        affine = _affine_from_landmarks(src, dst)
        homo = np.concatenate([vertices, np.ones((len(vertices), 1))], axis=1)
        vertices = (affine @ homo.T).T[:, :3]
        log["affine"] = affine.tolist()
        log["summary"] += " + landmark affine"
    return MeshGeometry(vertices=vertices, faces=mesh.faces.copy()), log


def _affine_from_landmarks(source: np.ndarray, target: np.ndarray) -> np.ndarray:
    """Least-squares 3D affine (3x4) mapping source landmarks to target."""
    ones = np.ones((len(source), 1))
    A = np.hstack([source, ones])
    # Solve A @ M.T = target for M (3x4).
    M, *_ = np.linalg.lstsq(A, target, rcond=None)
    # M is 4x3; we want 3x4.
    return M.T


def _select_archetype(manifest: dict[str, Any], target_id: str) -> str | None:
    target = target_id.lower()
    for item in manifest.get("archetypes", []):
        tags = [str(t).lower() for t in item.get("tags", [])]
        if item.get("id", "").lower() in target or any(tag in target for tag in tags):
            return item["id"]
    archetypes = manifest.get("archetypes") or []
    return archetypes[0]["id"] if archetypes else None
