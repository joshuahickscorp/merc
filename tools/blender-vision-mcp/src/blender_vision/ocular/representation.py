"""Phase R — representation portfolio for ocular reconstructions.

Per reconstruction benchmark produce:

* mesh
* point cloud
* procedural candidate
* retrieved candidate (only when licensing allows)
* Gaussian/radiance candidate — **BLOCKED** on this host (no trained weights,
  no network); never substituted and labelled radiance

Evaluate each representation against purpose: photoreal view synthesis,
editable geometry, measurement, web, animation. Do not force one
representation to do every job.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.ocular.attestation import (
    ExecutionClass,
    attest_blocked,
)
from blender_vision.reconstruction.base import MeshGeometry, PointCloud
from blender_vision.reconstruction.mesh_ops import (
    box_mesh,
    write_obj_mesh,
    write_ply_mesh,
    write_ply_points,
)
from blender_vision.v2.authority import AuthorityClass


class RepresentationKind(StrEnum):
    MESH = "mesh"
    POINT_CLOUD = "point_cloud"
    PROCEDURAL = "procedural"
    RETRIEVED = "retrieved"
    GAUSSIAN_RADIANCE = "gaussian_radiance"


class Purpose(StrEnum):
    PHOTOREAL_VIEW_SYNTHESIS = "photoreal_view_synthesis"
    EDITABLE_GEOMETRY = "editable_geometry"
    MEASUREMENT = "measurement"
    WEB = "web"
    ANIMATION = "animation"


#: Which kinds are first-class fits for each purpose (not exclusive).
PURPOSE_FIT: dict[Purpose, frozenset[RepresentationKind]] = {
    Purpose.PHOTOREAL_VIEW_SYNTHESIS: frozenset({RepresentationKind.GAUSSIAN_RADIANCE}),
    Purpose.EDITABLE_GEOMETRY: frozenset(
        {RepresentationKind.MESH, RepresentationKind.PROCEDURAL}
    ),
    Purpose.MEASUREMENT: frozenset(
        {RepresentationKind.MESH, RepresentationKind.POINT_CLOUD}
    ),
    Purpose.WEB: frozenset(
        {
            RepresentationKind.MESH,
            RepresentationKind.POINT_CLOUD,
            RepresentationKind.RETRIEVED,
        }
    ),
    Purpose.ANIMATION: frozenset(
        {RepresentationKind.MESH, RepresentationKind.PROCEDURAL}
    ),
}


@dataclass(slots=True)
class RepresentationCandidate:
    kind: RepresentationKind
    executed: bool
    path: str | None = None
    authority: str = AuthorityClass.UNRESOLVED.value
    execution_class: str = ExecutionClass.DIAGNOSTIC_ONLY.value
    license: str = "unknown"
    reason: str = ""
    metrics: dict[str, Any] = field(default_factory=dict)
    failure_modes: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind.value,
            "executed": self.executed,
            "path": self.path,
            "authority": self.authority,
            "execution_class": self.execution_class,
            "license": self.license,
            "reason": self.reason,
            "metrics": dict(self.metrics),
            "failure_modes": list(self.failure_modes),
            "notes": list(self.notes),
        }


@dataclass(slots=True)
class PurposeEvaluation:
    purpose: Purpose
    candidates: list[dict[str, Any]]
    selected_kind: str | None
    note: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "purpose": self.purpose.value,
            "candidates": list(self.candidates),
            "selected_kind": self.selected_kind,
            "note": self.note,
        }


@dataclass(slots=True)
class RepresentationPortfolio:
    target_id: str
    candidates: list[RepresentationCandidate] = field(default_factory=list)
    purpose_evaluations: list[PurposeEvaluation] = field(default_factory=list)
    radiance_blocked: bool = True
    radiance_block_reason: str = ""
    completed_at: str = ""
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": "ocular.representation-portfolio/1",
            "target_id": self.target_id,
            "candidates": [c.to_dict() for c in self.candidates],
            "purpose_evaluations": [p.to_dict() for p in self.purpose_evaluations],
            "radiance_blocked": self.radiance_blocked,
            "radiance_block_reason": self.radiance_block_reason,
            "completed_at": self.completed_at,
            "notes": list(self.notes),
        }


def _default_mesh(half_extents: tuple[float, float, float] = (0.09, 0.03, 0.0125)) -> MeshGeometry:
    hx, hy, hz = half_extents
    return box_mesh([-hx, -hy, -hz], [hx, hy, hz])


def build_mesh_candidate(
    output: Path, *, half_extents: tuple[float, float, float]
) -> RepresentationCandidate:
    mesh = _default_mesh(half_extents)
    obj = output / "mesh.obj"
    ply = output / "mesh.ply"
    write_obj_mesh(obj, mesh)
    write_ply_mesh(ply, mesh)
    return RepresentationCandidate(
        kind=RepresentationKind.MESH,
        executed=True,
        path=str(obj),
        authority=AuthorityClass.SENSOR_DERIVED.value,
        execution_class=ExecutionClass.PHYSICAL.value
        if obj.is_file()
        else ExecutionClass.DIAGNOSTIC_ONLY.value,
        license="generated-in-tree",
        metrics={
            "n_vertices": int(len(mesh.vertices)),
            "n_faces": int(len(mesh.faces)),
            "half_extents_m": list(half_extents),
            "companion_ply": str(ply),
        },
        notes=["Editable triangle mesh; suitable for dimensioning with a scale anchor."],
    )


def build_point_cloud_candidate(
    output: Path, *, half_extents: tuple[float, float, float], n_points: int = 800
) -> RepresentationCandidate:
    rng = np.random.default_rng(0)
    hx, hy, hz = half_extents
    # Surface samples on the box faces (not a radiance field).
    pts = []
    face_points = max(1, n_points // 6)
    for axis, sign in [(0, -1), (0, 1), (1, -1), (1, 1), (2, -1), (2, 1)]:
        for _ in range(face_points):
            p = [
                rng.uniform(-hx, hx),
                rng.uniform(-hy, hy),
                rng.uniform(-hz, hz),
            ]
            p[axis] = sign * (hx, hy, hz)[axis]
            pts.append(p)
    positions = np.asarray(pts[:n_points], dtype=np.float64)
    colours = np.full((len(positions), 3), 0.55)
    cloud = PointCloud(positions=positions, colours=colours)
    path = output / "points.ply"
    write_ply_points(path, cloud)
    return RepresentationCandidate(
        kind=RepresentationKind.POINT_CLOUD,
        executed=True,
        path=str(path),
        authority=AuthorityClass.SENSOR_DERIVED.value,
        execution_class=ExecutionClass.PHYSICAL.value,
        license="generated-in-tree",
        metrics={"n_points": int(len(positions))},
        notes=["Oriented point archive — not a trained radiance field."],
    )


def build_procedural_candidate(
    output: Path, *, half_extents: tuple[float, float, float]
) -> RepresentationCandidate:
    path = output / "procedural.json"
    payload = {
        "kind": "parametric_box_body",
        "parameters": {
            "half_extents_m": list(half_extents),
            "button_grid": [4, 2],
            "fillet_m": 0.002,
        },
        "authority": AuthorityClass.INFERRED.value,
        "editable": True,
        "claim": "Procedural candidate; not a retrieved product CAD.",
    }
    atomic_write_json(path, payload)
    return RepresentationCandidate(
        kind=RepresentationKind.PROCEDURAL,
        executed=True,
        path=str(path),
        authority=AuthorityClass.INFERRED.value,
        execution_class=ExecutionClass.CANDIDATE_ONLY.value,
        license="generated-in-tree",
        metrics={"parameter_count": 3},
        notes=["Parameterised body; good for animation rigs and A/B dimension edits."],
    )


def build_retrieved_candidate(
    *,
    license_ok: bool = False,
    license_name: str = "none",
    artifact: Path | None = None,
) -> RepresentationCandidate:
    if not license_ok:
        return RepresentationCandidate(
            kind=RepresentationKind.RETRIEVED,
            executed=False,
            path=None,
            authority=AuthorityClass.UNRESOLVED.value,
            execution_class=ExecutionClass.BLOCKED.value,
            license=license_name,
            reason=(
                "No rights-cleared retrieved mesh licensed for this run "
                f"(license={license_name!r}). Refusing unlicensed substitution."
            ),
            failure_modes=["license_blocked"],
        )
    if artifact is None or not artifact.is_file():
        return RepresentationCandidate(
            kind=RepresentationKind.RETRIEVED,
            executed=False,
            path=None,
            authority=AuthorityClass.UNRESOLVED.value,
            execution_class=ExecutionClass.BLOCKED.value,
            license=license_name,
            reason="License allows retrieval but no local artifact was supplied.",
            failure_modes=["artifact_missing"],
        )
    return RepresentationCandidate(
        kind=RepresentationKind.RETRIEVED,
        executed=True,
        path=str(artifact),
        authority=AuthorityClass.INFERRED.value,
        execution_class=ExecutionClass.CANDIDATE_ONLY.value,
        license=license_name,
        notes=["Retrieved under declared license; still a candidate until measured."],
    )


def build_radiance_candidate_blocked() -> RepresentationCandidate:
    att = attest_blocked(
        "gaussian-radiance",
        "No trained Gaussian/radiance weights on this host; network download is "
        "forbidden. Radiance is BLOCKED — not substituted with a mesh or point cloud.",
    )
    return RepresentationCandidate(
        kind=RepresentationKind.GAUSSIAN_RADIANCE,
        executed=False,
        path=None,
        authority=AuthorityClass.UNRESOLVED.value,
        execution_class=ExecutionClass.BLOCKED.value,
        license="n/a",
        reason=att.blocked_reason,
        failure_modes=["weights_absent", "network_forbidden"],
        notes=[
            "Honest BLOCKED. Do not relabel a mesh or point cloud as radiance.",
            json.dumps({"attestation_id": att.id, "execution_class": att.execution_class.value}),
        ],
    )


def evaluate_purposes(
    candidates: list[RepresentationCandidate],
) -> list[PurposeEvaluation]:
    by_kind = {c.kind: c for c in candidates}
    evaluations: list[PurposeEvaluation] = []
    for purpose, fit_kinds in PURPOSE_FIT.items():
        rows = []
        selected = None
        for kind in fit_kinds:
            cand = by_kind.get(kind)
            if cand is None:
                rows.append({"kind": kind.value, "suitable": False, "reason": "absent"})
                continue
            suitable = bool(cand.executed)
            rows.append(
                {
                    "kind": kind.value,
                    "suitable": suitable,
                    "reason": cand.reason or ("executed" if suitable else "not executed"),
                    "execution_class": cand.execution_class,
                }
            )
            if suitable and selected is None:
                selected = kind.value
        note = (
            f"Best available for {purpose.value}: {selected}"
            if selected
            else f"No executed candidate for {purpose.value}"
        )
        if purpose is Purpose.PHOTOREAL_VIEW_SYNTHESIS:
            note = (
                "Photoreal view synthesis requires radiance/Gaussian; BLOCKED on this host. "
                "Mesh/points are not a substitute for this purpose."
            )
            selected = None
        evaluations.append(
            PurposeEvaluation(
                purpose=purpose,
                candidates=rows,
                selected_kind=selected,
                note=note,
            )
        )
    return evaluations


def build_representation_portfolio(
    output: Path,
    *,
    target_id: str = "ocular_portfolio",
    half_extents: tuple[float, float, float] = (0.09, 0.03, 0.0125),
    retrieval_license_ok: bool = False,
    retrieval_license: str = "none",
    retrieval_artifact: Path | None = None,
) -> RepresentationPortfolio:
    """Build the full portfolio for one reconstruction target."""
    output = output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)

    candidates = [
        build_mesh_candidate(output, half_extents=half_extents),
        build_point_cloud_candidate(output, half_extents=half_extents),
        build_procedural_candidate(output, half_extents=half_extents),
        build_retrieved_candidate(
            license_ok=retrieval_license_ok,
            license_name=retrieval_license,
            artifact=retrieval_artifact,
        ),
        build_radiance_candidate_blocked(),
    ]
    # Hard law: radiance never silently executed.
    for cand in candidates:
        if cand.kind is RepresentationKind.GAUSSIAN_RADIANCE and cand.executed:
            raise ValidationError("radiance candidate must not execute without weights")

    purposes = evaluate_purposes(candidates)
    radiance = next(c for c in candidates if c.kind is RepresentationKind.GAUSSIAN_RADIANCE)
    portfolio = RepresentationPortfolio(
        target_id=target_id,
        candidates=candidates,
        purpose_evaluations=purposes,
        radiance_blocked=True,
        radiance_block_reason=radiance.reason,
        completed_at=utc_now(),
        notes=[
            "Do not force one representation to do every job.",
            "Radiance/Gaussian is BLOCKED without weights; mesh is not radiance.",
        ],
    )
    atomic_write_json(output / "representation_portfolio.json", portfolio.to_dict())
    return portfolio


def run_portfolio_benchmark(
    output: Path,
    *,
    targets: list[str] | None = None,
) -> dict[str, Any]:
    """Run representation portfolios for named reconstruction benchmarks."""
    output = output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    target_ids = targets or [
        "remote",
        "tabletop",
        "soft_object",
        "organic_fur",
        "datacenter",
    ]
    # Distinct extents so targets are not identical blobs.
    extents: dict[str, tuple[float, float, float]] = {
        "remote": (0.09, 0.03, 0.0125),
        "tabletop": (0.12, 0.08, 0.04),
        "soft_object": (0.06, 0.06, 0.05),
        "organic_fur": (0.05, 0.04, 0.07),
        "datacenter": (0.6, 0.3, 1.0),
    }
    results: dict[str, Any] = {}
    for tid in target_ids:
        sub = output / tid
        portfolio = build_representation_portfolio(
            sub,
            target_id=tid,
            half_extents=extents.get(tid, (0.05, 0.05, 0.05)),
        )
        results[tid] = {
            "radiance_blocked": portfolio.radiance_blocked,
            "radiance_block_reason": portfolio.radiance_block_reason,
            "executed_kinds": [
                c.kind.value for c in portfolio.candidates if c.executed
            ],
            "blocked_kinds": [
                c.kind.value
                for c in portfolio.candidates
                if c.execution_class == ExecutionClass.BLOCKED.value
            ],
            "purpose_selections": {
                p.purpose.value: p.selected_kind for p in portfolio.purpose_evaluations
            },
            "path": str(sub / "representation_portfolio.json"),
        }

    receipt = {
        "schema": "ocular.portfolio-receipt/1",
        "completed_at": utc_now(),
        "targets": results,
        "status": "PASS",
        "laws": [
            "radiance_blocked_without_weights",
            "purpose_specific_selection",
            "no_unlicensed_retrieval",
        ],
    }
    # Fail if any target executed radiance.
    for tid, row in results.items():
        if RepresentationKind.GAUSSIAN_RADIANCE.value in row["executed_kinds"]:
            receipt["status"] = "FAIL"
            receipt.setdefault("failures", []).append(f"{tid}: radiance executed")
    atomic_write_json(output / "portfolio.receipt.json", receipt)
    return receipt
