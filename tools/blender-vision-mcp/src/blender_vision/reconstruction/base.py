"""Shared contracts for reconstruction backends."""

from __future__ import annotations

import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

import numpy as np

from blender_vision.core.models import BackendState
from blender_vision.v2.authority import (
    AuthorityClass,
    CoordinateFrame,
    Units,
)
from blender_vision.v2.records import ReconstructionCandidate


@dataclass(slots=True)
class BackendAvailability:
    """Whether a backend can execute in this environment, and why if not."""

    state: BackendState
    reason: str = ""
    details: dict[str, Any] = field(default_factory=dict)

    @property
    def available(self) -> bool:
        return self.state is BackendState.AVAILABLE

    def to_dict(self) -> dict[str, Any]:
        return {
            "state": self.state.value,
            "reason": self.reason,
            "details": dict(self.details),
        }


@dataclass(slots=True)
class CameraView:
    """Pinhole camera in a world frame: world_from_camera is 4x4 column-major rows."""

    name: str
    width: int
    height: int
    fx: float
    fy: float
    cx: float
    cy: float
    world_from_camera: np.ndarray
    near: float = 0.01
    far: float = 100.0

    def camera_from_world(self) -> np.ndarray:
        return np.linalg.inv(self.world_from_camera)


@dataclass(slots=True)
class DepthFrame:
    """Posed depth map. Depth is metres along the camera optical axis (+Z OpenCV)."""

    name: str
    depth: np.ndarray
    camera: CameraView
    colour: np.ndarray | None = None
    valid_mask: np.ndarray | None = None


@dataclass(slots=True)
class MeshGeometry:
    vertices: np.ndarray
    faces: np.ndarray
    normals: np.ndarray | None = None

    def is_empty(self) -> bool:
        return self.vertices.size == 0 or self.faces.size == 0


@dataclass(slots=True)
class PointCloud:
    positions: np.ndarray
    normals: np.ndarray | None = None
    colours: np.ndarray | None = None
    radii: np.ndarray | None = None
    confidence: np.ndarray | None = None


@dataclass(slots=True)
class ReconstructionInputs:
    """Inputs shared across backends. Unused fields are ignored by each backend."""

    target_id: str
    work_dir: Path
    frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name="blender-world",
            up_axis="+Z",
            forward_axis="-Y",
            units=Units.METRE,
            scale_authority=AuthorityClass.UNRESOLVED,
        )
    )
    image_dir: Path | None = None
    masks: list[np.ndarray] = field(default_factory=list)
    cameras: list[CameraView] = field(default_factory=list)
    bounds_min: np.ndarray | None = None
    bounds_max: np.ndarray | None = None
    depth_frames: list[DepthFrame] = field(default_factory=list)
    points: PointCloud | None = None
    primitive_kind: str | None = None
    library_dir: Path | None = None
    archetype_id: str | None = None
    adaptation_scale: tuple[float, float, float] | None = None
    landmarks_source: np.ndarray | None = None
    landmarks_target: np.ndarray | None = None
    browser_scene: dict[str, Any] | None = None
    metric_anchor_m: float | None = None
    licensing: str = "SYNTHETIC_OWNED"
    parameters: dict[str, Any] = field(default_factory=dict)
    evidence_refs: list[str] = field(default_factory=list)
    input_authorities: list[AuthorityClass] = field(default_factory=list)

    def ensure_work_dir(self) -> Path:
        self.work_dir.mkdir(parents=True, exist_ok=True)
        return self.work_dir


class ReconstructionBackend(Protocol):
    name: str

    def availability(self) -> BackendAvailability:
        """Report whether this backend can actually execute here."""
        ...

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        """Execute reconstruction. executed=True only after real work completes."""
        ...


def new_candidate_id(backend: str) -> str:
    return f"{backend}-{uuid.uuid4().hex[:12]}"


def unavailable_candidate(
    *,
    backend: str,
    reason: str,
    inputs: ReconstructionInputs,
    authority: AuthorityClass = AuthorityClass.UNRESOLVED,
) -> ReconstructionCandidate:
    """Portfolio entry for a backend that could not execute."""
    return ReconstructionCandidate(
        candidate_id=new_candidate_id(backend),
        backend=backend,
        inputs=list(inputs.evidence_refs),
        frame=inputs.frame,
        scale_state="unresolved",
        scale_authority=AuthorityClass.UNRESOLVED,
        coverage={"status": "unavailable", "reason": reason},
        topology_state={"status": "unavailable"},
        editability="none",
        hidden_surface_assumptions=[],
        material_state="none",
        licensing=inputs.licensing,
        runtime_cost={"seconds": 0.0, "executed": False},
        failure_modes=[reason],
        authority=authority,
        artifacts={},
        executed=False,
        execution_log=f"UNAVAILABLE: {reason}",
    )


def finalize_candidate(
    *,
    backend: str,
    inputs: ReconstructionInputs,
    authority: AuthorityClass,
    scale_authority: AuthorityClass,
    scale_state: str,
    coverage: dict[str, Any],
    topology_state: dict[str, Any],
    editability: str,
    hidden_surface_assumptions: list[str],
    artifacts: dict[str, str],
    runtime_seconds: float,
    execution_log: str,
    failure_modes: list[str] | None = None,
    licensing: str | None = None,
    material_state: str = "none",
    visual_score: float | None = None,
    dimensional_score: float | None = None,
    executed: bool,
) -> ReconstructionCandidate:
    """Build a candidate. executed=True is only legal when the caller asserts real work."""
    if executed and not artifacts and not topology_state:
        raise ValueError(
            f"{backend}: cannot claim executed=True without artifacts or topology evidence"
        )
    return ReconstructionCandidate(
        candidate_id=new_candidate_id(backend),
        backend=backend,
        inputs=list(inputs.evidence_refs),
        frame=inputs.frame,
        scale_state=scale_state,
        scale_authority=scale_authority,
        coverage=coverage,
        topology_state=topology_state,
        editability=editability,
        visual_score=visual_score,
        dimensional_score=dimensional_score,
        hidden_surface_assumptions=hidden_surface_assumptions,
        material_state=material_state,
        licensing=licensing or inputs.licensing,
        runtime_cost={
            "seconds": float(runtime_seconds),
            "executed": executed,
            "wall_clock_basis": "process_time",
        },
        failure_modes=list(failure_modes or []),
        authority=authority,
        artifacts=dict(artifacts),
        executed=executed,
        execution_log=execution_log,
    )


class TimedRun:
    """Context manager measuring wall time for runtime_cost."""

    def __init__(self) -> None:
        self.seconds = 0.0
        self._start = 0.0

    def __enter__(self) -> TimedRun:
        self._start = time.perf_counter()
        return self

    def __exit__(self, *_exc: object) -> None:
        self.seconds = time.perf_counter() - self._start
