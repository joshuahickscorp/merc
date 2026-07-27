"""Bounded fusion that refuses incompatible candidates."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.reconstruction.base import MeshGeometry
from blender_vision.reconstruction.mesh_ops import (
    load_mesh_artifact,
    topology_report,
    write_ply_mesh,
)
from blender_vision.v2.authority import (
    AuthorityClass,
    CoordinateFrame,
    VisibilityState,
    derive,
)
from blender_vision.v2.records import ReconstructionCandidate


class FusionError(ValidationError):
    """Raised when fusion is refused due to a named incompatibility."""

    def __init__(self, kind: str, message: str) -> None:
        self.kind = kind
        super().__init__(f"fusion refused ({kind}): {message}")


@dataclass(slots=True)
class FusionRequest:
    left: ReconstructionCandidate
    right: ReconstructionCandidate
    target_id_left: str
    target_id_right: str
    mode: str
    work_dir: Path


@dataclass(slots=True)
class FusionResult:
    mode: str
    authority: AuthorityClass
    artifacts: dict[str, str]
    hidden_surface_ledger: list[dict[str, Any]]
    topology_state: dict[str, Any]
    notes: list[str]
    candidate_ids: list[str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "authority": self.authority.value,
            "artifacts": dict(self.artifacts),
            "hidden_surface_ledger": list(self.hidden_surface_ledger),
            "topology_state": dict(self.topology_state),
            "notes": list(self.notes),
            "candidate_ids": list(self.candidate_ids),
        }


PERMITTED_MODES = {
    "observed_plus_procedural_shell",
    "depth_plus_measured_dimensions",
    "retrieved_plus_observed_face",
}


def fuse_candidates(
    left: ReconstructionCandidate,
    right: ReconstructionCandidate,
    *,
    target_id_left: str,
    target_id_right: str,
    mode: str,
    work_dir: Path,
) -> FusionResult:
    """Fuse two candidates if and only if they are compatible.

    Refuses (typed FusionError) when coordinate frames, units, scale authority,
    or target identity are incompatible.
    """
    if mode not in PERMITTED_MODES:
        raise FusionError(
            "mode",
            f"mode {mode!r} is not permitted; allowed={sorted(PERMITTED_MODES)}",
        )
    _require_compatible_frames(left.frame, right.frame)
    _require_compatible_units(left.frame, right.frame)
    _require_compatible_scale_authority(left.scale_authority, right.scale_authority, mode=mode)
    _require_same_target(target_id_left, target_id_right)

    if not left.executed or not right.executed:
        raise FusionError(
            "execution",
            "both candidates must have executed=True before fusion",
        )

    mesh_l = _mesh(left)
    mesh_r = _mesh(right)
    work_dir.mkdir(parents=True, exist_ok=True)

    if mode == "observed_plus_procedural_shell":
        return _fuse_observed_plus_shell(left, right, mesh_l, mesh_r, work_dir)
    if mode == "depth_plus_measured_dimensions":
        return _fuse_depth_plus_dims(left, right, mesh_l, mesh_r, work_dir)
    if mode == "retrieved_plus_observed_face":
        return _fuse_retrieved_plus_face(left, right, mesh_l, mesh_r, work_dir)
    raise FusionError("mode", f"unhandled mode {mode!r}")


def _require_compatible_frames(left: CoordinateFrame, right: CoordinateFrame) -> None:
    if left.up_axis != right.up_axis or left.forward_axis != right.forward_axis:
        raise FusionError(
            "coordinate_frame",
            f"axis mismatch: {left.name}({left.up_axis}/{left.forward_axis}) vs "
            f"{right.name}({right.up_axis}/{right.forward_axis})",
        )
    if left.handedness != right.handedness:
        raise FusionError(
            "coordinate_frame",
            f"handedness mismatch: {left.handedness.value} vs {right.handedness.value}",
        )


def _require_compatible_units(left: CoordinateFrame, right: CoordinateFrame) -> None:
    if left.units != right.units:
        raise FusionError(
            "units",
            f"units mismatch: {left.units.value} vs {right.units.value}",
        )


def _require_compatible_scale_authority(
    left: AuthorityClass,
    right: AuthorityClass,
    *,
    mode: str,
) -> None:
    # Unresolved + unresolved is only allowed for pure model/retrieved modes that
    # do not claim metric scale. Metric-affecting fusions require at least one
    # non-unresolved scale authority, and never mix contradictory metric claims
    # without shared ancestry — here we refuse pure unresolved pairs for depth
    # and measured-dimension fusion, and refuse REJECTED always.
    if left is AuthorityClass.REJECTED or right is AuthorityClass.REJECTED:
        raise FusionError(
            "scale_authority",
            "REJECTED scale authority cannot be fused",
        )
    if (
        mode == "depth_plus_measured_dimensions"
        and left is AuthorityClass.UNRESOLVED
        and right is AuthorityClass.UNRESOLVED
    ):
        raise FusionError(
            "scale_authority",
            "depth_plus_measured_dimensions requires at least one resolved scale authority",
        )
    # Incompatible: MEASURED with pure HYPOTHETICAL without shared frame metric.
    incompatible_pairs = {
        frozenset({AuthorityClass.MEASURED, AuthorityClass.HYPOTHETICAL}),
        frozenset({AuthorityClass.MANUFACTURER_SPEC, AuthorityClass.HYPOTHETICAL}),
    }
    if frozenset({left, right}) in incompatible_pairs:
        raise FusionError(
            "scale_authority",
            f"scale authority pair incompatible: {left.value} + {right.value}",
        )


def _require_same_target(left: str, right: str) -> None:
    if not left or not right:
        raise FusionError("target_identity", "both target ids must be non-empty")
    if left != right:
        raise FusionError(
            "target_identity",
            f"target identity mismatch: {left!r} vs {right!r}",
        )


def _mesh(candidate: ReconstructionCandidate) -> MeshGeometry | None:
    for key in ("mesh_ply", "mesh_obj", "ply"):
        path = candidate.artifacts.get(key)
        if path and Path(path).is_file():
            return load_mesh_artifact(Path(path))
    return None


def _combine_meshes(a: MeshGeometry, b: MeshGeometry) -> MeshGeometry:
    if a is None or a.is_empty():
        return b
    if b is None or b.is_empty():
        return a
    offset = len(a.vertices)
    vertices = np.vstack([a.vertices, b.vertices])
    faces = np.vstack([a.faces, b.faces + offset])
    return MeshGeometry(vertices=vertices, faces=faces)


def _fuse_observed_plus_shell(
    left: ReconstructionCandidate,
    right: ReconstructionCandidate,
    mesh_l: MeshGeometry | None,
    mesh_r: MeshGeometry | None,
    work_dir: Path,
) -> FusionResult:
    # Identify which is procedural shell by backend/visibility assumptions.
    observed, shell = left, right
    left_is_prior = any(k in left.backend for k in ("procedural", "retrieval", "parametric"))
    right_is_prior = any(k in right.backend for k in ("procedural", "retrieval"))
    if left_is_prior and not right_is_prior:
        observed, shell = right, left
    mesh_o = mesh_l if observed is left else mesh_r
    mesh_s = mesh_r if shell is right else mesh_l
    if mesh_o is None or mesh_s is None:
        raise FusionError("geometry", "both observed and shell meshes are required")
    combined = _combine_meshes(mesh_o, mesh_s)
    path = write_ply_mesh(work_dir / "fusion_observed_shell.ply", combined)
    authority = derive([observed.authority, shell.authority], proposed=AuthorityClass.INFERRED)
    ledger = [
        {
            "region": "procedural_shell",
            "visibility_state": VisibilityState.NEVER_OBSERVED.value,
            "source_candidate": shell.candidate_id,
            "authority": shell.authority.value,
            "assumption": "hidden shell introduced by procedural/retrieved geometry",
        }
    ]
    for assumption in shell.hidden_surface_assumptions:
        ledger.append(
            {
                "region": "shell_assumption",
                "visibility_state": VisibilityState.INFERRED_SURFACE.value,
                "source_candidate": shell.candidate_id,
                "authority": shell.authority.value,
                "assumption": assumption,
            }
        )
    return FusionResult(
        mode="observed_plus_procedural_shell",
        authority=authority,
        artifacts={"mesh_ply": str(path)},
        hidden_surface_ledger=ledger,
        topology_state=topology_report(combined),
        notes=[
            f"observed={observed.candidate_id}",
            f"shell={shell.candidate_id}",
        ],
        candidate_ids=[left.candidate_id, right.candidate_id],
    )


def _fuse_depth_plus_dims(
    left: ReconstructionCandidate,
    right: ReconstructionCandidate,
    mesh_l: MeshGeometry | None,
    mesh_r: MeshGeometry | None,
    work_dir: Path,
) -> FusionResult:
    depth = left if "depth" in left.backend or "point" in left.backend else right
    measured = right if depth is left else left
    mesh = mesh_l if depth is left else mesh_r
    if mesh is None:
        # Allow dimensional candidate without mesh: scale the depth mesh.
        mesh = mesh_r if depth is left else mesh_l
    if mesh is None:
        raise FusionError("geometry", "depth geometry mesh required")
    # Measured dimensions may be in topology_state or coverage.
    dims = measured.coverage.get("dimensions") or measured.topology_state.get("dimensions")
    if not dims:
        # If the peer is parametric with size, use that.
        dims = measured.topology_state.get("parameters") or measured.coverage
    scale = _scale_from_dimensions(mesh, dims)
    scaled = MeshGeometry(vertices=mesh.vertices * scale, faces=mesh.faces.copy())
    path = write_ply_mesh(work_dir / "fusion_depth_measured.ply", scaled)
    authority = derive(
        [depth.authority, measured.authority],
        proposed=AuthorityClass.SENSOR_DERIVED,
    )
    ledger = [
        {
            "region": "scaled_by_measured_dimensions",
            "visibility_state": VisibilityState.DIRECTLY_VISIBLE.value,
            "source_candidate": measured.candidate_id,
            "authority": measured.authority.value,
            "assumption": f"uniform scale factor {scale:.6g} from measured dimensions",
        }
    ]
    return FusionResult(
        mode="depth_plus_measured_dimensions",
        authority=authority,
        artifacts={"mesh_ply": str(path)},
        hidden_surface_ledger=ledger,
        topology_state={**topology_report(scaled), "applied_scale": scale},
        notes=[
            f"depth={depth.candidate_id}",
            f"measured={measured.candidate_id}",
            f"scale={scale}",
        ],
        candidate_ids=[left.candidate_id, right.candidate_id],
    )


def _fuse_retrieved_plus_face(
    left: ReconstructionCandidate,
    right: ReconstructionCandidate,
    mesh_l: MeshGeometry | None,
    mesh_r: MeshGeometry | None,
    work_dir: Path,
) -> FusionResult:
    retrieved = left if left.backend == "retrieval" else right
    observed = right if retrieved is left else left
    retrieved_visibility = str(retrieved.topology_state.get("visibility_state", ""))
    is_retrieved_like = (
        retrieved.backend == "retrieval"
        or VisibilityState.RETRIEVED_MODEL.value in retrieved_visibility
    )
    if not is_retrieved_like and right.backend == "retrieval":
        retrieved, observed = right, left
    mesh_ret = mesh_l if retrieved is left else mesh_r
    mesh_obs = mesh_r if observed is right else mesh_l
    if mesh_ret is None:
        raise FusionError("geometry", "retrieved mesh required")
    # Observed face may be a sparse point cloud; still record ledger.
    combined = mesh_ret if mesh_obs is None else _combine_meshes(mesh_ret, mesh_obs)
    path = write_ply_mesh(work_dir / "fusion_retrieved_face.ply", combined)
    authority = derive(
        [retrieved.authority, observed.authority], proposed=AuthorityClass.MODEL_DERIVED
    )
    ledger = [
        {
            "region": "retrieved_body",
            "visibility_state": VisibilityState.RETRIEVED_MODEL.value,
            "source_candidate": retrieved.candidate_id,
            "authority": retrieved.authority.value,
            "assumption": "non-observed body from archetype library",
        },
        {
            "region": "observed_face",
            "visibility_state": VisibilityState.DIRECTLY_VISIBLE.value,
            "source_candidate": observed.candidate_id,
            "authority": observed.authority.value,
            "assumption": "observed face constrained by evidence",
        },
    ]
    return FusionResult(
        mode="retrieved_plus_observed_face",
        authority=authority,
        artifacts={"mesh_ply": str(path)},
        hidden_surface_ledger=ledger,
        topology_state=topology_report(combined),
        notes=[
            f"retrieved={retrieved.candidate_id}",
            f"observed_face={observed.candidate_id}",
        ],
        candidate_ids=[left.candidate_id, right.candidate_id],
    )


def _scale_from_dimensions(mesh: MeshGeometry, dims: dict[str, Any] | None) -> float:
    if not dims:
        return 1.0
    extent = mesh.vertices.max(axis=0) - mesh.vertices.min(axis=0)
    current = float(np.max(extent)) if np.any(extent > 0) else 1.0
    for key in ("max_extent", "height", "width", "depth", "size", "diameter"):
        if key in dims and isinstance(dims[key], (int, float)) and dims[key] > 0:
            return float(dims[key]) / max(current, 1e-12)
    if "size" in dims and isinstance(dims["size"], list) and dims["size"]:
        target = float(max(dims["size"]))
        return target / max(current, 1e-12)
    return 1.0
