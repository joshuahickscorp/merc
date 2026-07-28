"""Thin adapter over perception.graphics runtime scene extraction."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.core.models import BackendState
from blender_vision.perception.graphics import RuntimeGltfCompiler
from blender_vision.reconstruction.base import (
    BackendAvailability,
    MeshGeometry,
    ReconstructionInputs,
    TimedRun,
    finalize_candidate,
    unavailable_candidate,
)
from blender_vision.reconstruction.mesh_ops import topology_report, write_ply_mesh
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import ReconstructionCandidate


class BrowserRuntimeBackend:
    """Produce a geometry candidate from an owned browser/WebGL scene hook."""

    name = "browser_runtime"

    def availability(self) -> BackendAvailability:
        return BackendAvailability(
            state=BackendState.AVAILABLE,
            reason="RuntimeGltfCompiler from perception.graphics is importable",
        )

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        scene = inputs.browser_scene
        if scene is None:
            scene_path = inputs.parameters.get("scene_path")
            if scene_path:
                scene = json.loads(Path(scene_path).read_text(encoding="utf-8"))
        if not scene:
            return unavailable_candidate(
                backend=self.name,
                reason="browser_runtime requires browser_scene or parameters.scene_path",
                inputs=inputs,
            )
        objects = scene.get("objects") or []
        if not objects:
            return unavailable_candidate(
                backend=self.name,
                reason="browser scene has no objects",
                inputs=inputs,
            )

        with TimedRun() as timer:
            # Normalize bounds-only objects into positions so RuntimeGltfCompiler
            # can materialize them without reimplementing glTF packing.
            normalized = _normalize_scene(scene)
            gltf = RuntimeGltfCompiler().compile(normalized)
            mesh = _mesh_from_runtime_scene(normalized)
            work = inputs.ensure_work_dir()
            mesh_path = write_ply_mesh(work / "browser_runtime.ply", mesh)
            gltf_path = work / "browser_runtime.gltf"
            gltf_path.write_text(json.dumps(gltf, indent=2), encoding="utf-8")
            report = topology_report(mesh)
            report["runtime_object_count"] = len(objects)
            report["source"] = "perception.graphics.RuntimeGltfCompiler"

        authority = derive(
            inputs.input_authorities or [AuthorityClass.RUNTIME_OBSERVED],
            proposed=AuthorityClass.RUNTIME_OBSERVED,
        )
        return finalize_candidate(
            backend=self.name,
            inputs=inputs,
            authority=authority,
            scale_authority=AuthorityClass.RUNTIME_OBSERVED,
            scale_state="runtime-scene-units",
            coverage={
                "object_count": len(objects),
                "has_camera": bool(scene.get("camera")),
                "revision": scene.get("revision"),
            },
            topology_state=report,
            editability="runtime-extracted-mesh",
            hidden_surface_assumptions=[
                "only geometry present in the runtime scene hook is recovered",
                "materials/lighting are structural only",
            ],
            artifacts={"mesh_ply": str(mesh_path), "gltf": str(gltf_path)},
            runtime_seconds=timer.seconds,
            execution_log=(
                f"compiled runtime scene with {len(objects)} objects via "
                f"RuntimeGltfCompiler; mesh V={report['vertex_count']}"
            ),
            failure_modes=[
                "requires explicit __VISIONMCP_SCENE__ style hook data",
                "no photogrammetric registration of browser pixels",
            ],
            licensing=str(scene.get("licensing", inputs.licensing)),
            executed=True,
        )


def _normalize_scene(scene: dict[str, Any]) -> dict[str, Any]:
    objects = []
    for item in scene.get("objects") or []:
        entry = dict(item)
        if not entry.get("positions"):
            bounds = entry.get("bounds")
            if bounds and "min" in bounds and "max" in bounds:
                local = _box_triangles(bounds["min"], bounds["max"])
                entry["positions"] = [c for p in local["positions"] for c in p]
                entry["indices"] = local["indices"]
                entry.setdefault("id", entry.get("name", f"object-{len(objects)}"))
        if entry.get("positions"):
            entry.setdefault("id", entry.get("name", f"object-{len(objects)}"))
            objects.append(entry)
    return {**scene, "objects": objects}


def _mesh_from_runtime_scene(scene: dict[str, Any]) -> MeshGeometry:
    vertices: list[list[float]] = []
    faces: list[list[int]] = []
    offset = 0
    for item in scene.get("objects") or []:
        positions = item.get("positions") or item.get("vertices")
        indices = item.get("indices") or item.get("faces")
        matrix = item.get("matrix")
        if not positions:
            bounds = item.get("bounds")
            if bounds and "min" in bounds and "max" in bounds:
                local = _box_triangles(bounds["min"], bounds["max"])
                positions = local["positions"]
                indices = local["indices"]
            else:
                continue
        pts = np.asarray(positions, dtype=np.float64).reshape(-1, 3)
        if indices is None:
            indices = list(range(len(pts)))
        if matrix and len(matrix) == 16:
            m = np.asarray(matrix, dtype=np.float64).reshape(4, 4)
            # glTF matrices are column-major in the runtime hook convention used
            # by RuntimeGltfCompiler (flat 16). Treat as column-major.
            if m.shape == (4, 4):
                homo = np.concatenate([pts, np.ones((len(pts), 1))], axis=1)
                pts = (m @ homo.T).T[:, :3]
        idx = np.asarray(indices, dtype=np.int64).reshape(-1)
        for i in range(0, len(idx), 3):
            if i + 2 >= len(idx):
                break
            faces.append(
                [int(idx[i]) + offset, int(idx[i + 1]) + offset, int(idx[i + 2]) + offset]
            )
        vertices.extend(pts.tolist())
        offset += len(pts)
    if not vertices:
        return MeshGeometry(
            vertices=np.zeros((0, 3)), faces=np.zeros((0, 3), dtype=np.int64)
        )
    return MeshGeometry(
        vertices=np.asarray(vertices, dtype=np.float64),
        faces=np.asarray(faces, dtype=np.int64),
    )


def _box_triangles(minimum: list[float], maximum: list[float]) -> dict[str, list]:
    x0, y0, z0 = minimum
    x1, y1, z1 = maximum
    positions = [
        [x0, y0, z0],
        [x1, y0, z0],
        [x1, y1, z0],
        [x0, y1, z0],
        [x0, y0, z1],
        [x1, y0, z1],
        [x1, y1, z1],
        [x0, y1, z1],
    ]
    indices = [
        0,
        1,
        2,
        0,
        2,
        3,
        4,
        6,
        5,
        4,
        7,
        6,
        0,
        4,
        5,
        0,
        5,
        1,
        1,
        5,
        6,
        1,
        6,
        2,
        2,
        6,
        7,
        2,
        7,
        3,
        3,
        7,
        4,
        3,
        4,
        0,
    ]
    return {"positions": positions, "indices": indices}
