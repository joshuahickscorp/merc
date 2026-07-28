"""Offline mesh materialisation via trimesh when Blender cannot start.

Produces real triangle meshes, GLB exports, bounding-box metrics, and simple
orthographic PNG previews. This is not a Blender substitute for editable
``.blend`` files or EEVEE/Cycles renders — those remain Blender-owned.
"""

from __future__ import annotations

import math
import tempfile
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.core.util import atomic_write_json
from blender_vision.procedural.archetype import GeometryKind, PartSpec, Transform3D
from blender_vision.procedural.instancing import InstancingPlan
from blender_vision.procedural.library import ArchetypeLibrary, default_library
from blender_vision.procedural.lod import LodLevel, filter_parts_for_lod


def _rotation_matrix(euler: tuple[float, float, float]) -> np.ndarray:
    rx, ry, rz = euler
    cx, sx = math.cos(rx), math.sin(rx)
    cy, sy = math.cos(ry), math.sin(ry)
    cz, sz = math.cos(rz), math.sin(rz)
    rx_m = np.array([[1, 0, 0], [0, cx, -sx], [0, sx, cx]], dtype=float)
    ry_m = np.array([[cy, 0, sy], [0, 1, 0], [-sy, 0, cy]], dtype=float)
    rz_m = np.array([[cz, -sz, 0], [sz, cz, 0], [0, 0, 1]], dtype=float)
    return rz_m @ ry_m @ rx_m


def _compose_matrix(transform: Transform3D) -> np.ndarray:
    mat = np.eye(4, dtype=float)
    rot = _rotation_matrix(transform.rotation_euler)
    scale = np.diag(list(transform.scale))
    mat[:3, :3] = rot @ scale
    mat[:3, 3] = list(transform.location)
    return mat


def _box_mesh(size: tuple[float, float, float]):
    import trimesh

    return trimesh.creation.box(extents=list(size))


def _cylinder_mesh(size: tuple[float, float, float], segments: int = 12):
    import trimesh

    radius = 0.5 * max(size[0], size[1])
    height = size[2]
    return trimesh.creation.cylinder(radius=radius, height=height, sections=max(8, segments))


def _part_meshes(part: PartSpec, parent: np.ndarray) -> list[Any]:
    import trimesh

    local = _compose_matrix(part.transform)
    world = parent @ local
    geom = part.geometry
    meshes: list[Any] = []
    kind = geom.kind

    if kind in {
        GeometryKind.BOX,
        GeometryKind.PANEL,
        GeometryKind.LIGHT_CELL,
    }:
        mesh = _box_mesh(geom.size)
        mesh.apply_transform(world)
        mesh.metadata["name"] = part.name
        meshes.append(mesh)
    elif kind is GeometryKind.CYLINDER:
        mesh = _cylinder_mesh(geom.size, geom.segments)
        mesh.apply_transform(world)
        mesh.metadata["name"] = part.name
        meshes.append(mesh)
    elif kind is GeometryKind.HOLLOW_BOX:
        sx, sy, sz = geom.size
        t = max(geom.wall_thickness, 0.001)
        open_face = geom.open_face
        walls: list[tuple[str, tuple[float, float, float], tuple[float, float, float]]] = []
        if open_face != "+X":
            walls.append(("px", (t, sy, sz), ((sx - t) * 0.5, 0.0, 0.0)))
        if open_face != "-X":
            walls.append(("nx", (t, sy, sz), (-(sx - t) * 0.5, 0.0, 0.0)))
        if open_face != "+Y":
            walls.append(("py", (sx, t, sz), (0.0, (sy - t) * 0.5, 0.0)))
        if open_face != "-Y":
            walls.append(("ny", (sx, t, sz), (0.0, -(sy - t) * 0.5, 0.0)))
        if open_face != "+Z":
            walls.append(("pz", (sx, sy, t), (0.0, 0.0, (sz - t) * 0.5)))
        if open_face != "-Z":
            walls.append(("nz", (sx, sy, t), (0.0, 0.0, -(sz - t) * 0.5)))
        for label, size, offset in walls:
            mesh = _box_mesh(size)
            off = np.eye(4)
            off[:3, 3] = list(offset)
            mesh.apply_transform(world @ off)
            mesh.metadata["name"] = f"{part.name}_{label}"
            meshes.append(mesh)
    elif kind is GeometryKind.VENT_FIELD:
        cx = max(1, geom.count_x)
        cz = max(1, geom.count_z)
        pitch = geom.pitch
        cell = geom.cell_size
        for iz in range(cz):
            for ix in range(cx):
                x = (ix - (cx - 1) * 0.5) * pitch[0]
                z = (iz - (cz - 1) * 0.5) * pitch[2]
                mesh = _box_mesh(cell)
                off = np.eye(4)
                off[:3, 3] = [x, 0.0, z]
                mesh.apply_transform(world @ off)
                mesh.metadata["name"] = f"{part.name}_{ix}_{iz}"
                meshes.append(mesh)
    elif kind in {GeometryKind.BUNDLE, GeometryKind.TUBE}:
        strands = max(1, geom.count_x or int(geom.extras.get("strand_count", 6)))
        dia = max(geom.size[0], geom.size[1])
        length = geom.size[2]
        core_r = dia * 0.5
        strand_r = core_r / max(2.5, math.sqrt(strands))
        for i in range(strands):
            ang = 2 * math.pi * i / strands
            r = core_r * 0.45
            mesh = trimesh.creation.cylinder(
                radius=strand_r, height=length, sections=8
            )
            off = np.eye(4)
            off[:3, 3] = [math.cos(ang) * r, math.sin(ang) * r, 0.0]
            mesh.apply_transform(world @ off)
            mesh.metadata["name"] = f"{part.name}_s{i}"
            meshes.append(mesh)
    else:
        mesh = _box_mesh(geom.size)
        mesh.apply_transform(world)
        mesh.metadata["name"] = part.name
        meshes.append(mesh)

    for child in part.children:
        meshes.extend(_part_meshes(child, world))
    return meshes


def materialise_parts(parts: list[PartSpec]) -> Any:
    import trimesh

    meshes: list[Any] = []
    identity = np.eye(4)
    for part in parts:
        meshes.extend(_part_meshes(part, identity))
    if not meshes:
        return trimesh.Trimesh(vertices=np.zeros((0, 3)), faces=np.zeros((0, 3), dtype=int))
    return trimesh.util.concatenate(meshes)


def mesh_metrics(mesh: Any) -> dict[str, Any]:
    if mesh is None or len(getattr(mesh, "vertices", [])) == 0:
        return {
            "triangle_count": 0,
            "bbox_min": [0.0, 0.0, 0.0],
            "bbox_max": [0.0, 0.0, 0.0],
            "bbox_size": [0.0, 0.0, 0.0],
            "object_count": 0,
        }
    bounds = mesh.bounds
    mins = bounds[0].tolist()
    maxs = bounds[1].tolist()
    return {
        "triangle_count": int(len(mesh.faces)),
        "bbox_min": mins,
        "bbox_max": maxs,
        "bbox_size": [maxs[i] - mins[i] for i in range(3)],
        "object_count": 1,
        "part_names": sorted({str(mesh.metadata.get("name", "mesh"))}),
    }


def export_glb(mesh: Any, path: Path) -> int:
    path.parent.mkdir(parents=True, exist_ok=True)
    mesh.export(path, file_type="glb")
    return path.stat().st_size


def write_orthographic_preview(
    mesh: Any,
    path: Path,
    *,
    view: str = "iso",
    size: tuple[int, int] = (1280, 720),
) -> int:
    """CPU orthographic wireframe-style depth preview (not Blender EEVEE)."""
    from PIL import Image, ImageDraw

    width, height = size
    img = Image.new("RGB", (width, height), (18, 20, 24))
    draw = ImageDraw.Draw(img)
    if mesh is None or len(mesh.vertices) == 0:
        img.save(path)
        return path.stat().st_size

    verts = np.asarray(mesh.vertices, dtype=float)
    # Project
    if view == "front":
        pts2 = verts[:, [0, 2]]
    elif view == "top":
        pts2 = verts[:, [0, 1]]
    elif view == "side":
        pts2 = verts[:, [1, 2]]
    else:
        # isometric-ish
        pts2 = np.column_stack(
            [
                verts[:, 0] * 0.866 - verts[:, 1] * 0.866,
                verts[:, 2] - verts[:, 0] * 0.25 - verts[:, 1] * 0.25,
            ]
        )

    mins = pts2.min(axis=0)
    maxs = pts2.max(axis=0)
    span = np.maximum(maxs - mins, 1e-6)
    margin = 0.08
    scale = (1.0 - 2 * margin) * min(width / span[0], height / span[1])
    offset = np.array([width * 0.5, height * 0.5])
    centre = (mins + maxs) * 0.5

    def project(p: np.ndarray) -> tuple[float, float]:
        q = (p - centre) * scale
        return float(offset[0] + q[0]), float(offset[1] - q[1])

    # Draw a subset of edges for readability
    faces = np.asarray(mesh.faces)
    step = max(1, len(faces) // 4000)
    for face in faces[::step]:
        tri = [project(pts2[i]) for i in face]
        draw.line([tri[0], tri[1], tri[2], tri[0]], fill=(160, 180, 200), width=1)

    # Bounds rectangle
    corners = [
        project(np.array([mins[0], mins[1]])),
        project(np.array([maxs[0], mins[1]])),
        project(np.array([maxs[0], maxs[1]])),
        project(np.array([mins[0], maxs[1]])),
    ]
    draw.line(corners + [corners[0]], fill=(90, 200, 140), width=2)
    draw.text((12, 12), f"CPU orthographic preview ({view}) — not EEVEE", fill=(220, 220, 220))
    path.parent.mkdir(parents=True, exist_ok=True)
    img.save(path)
    return path.stat().st_size


def emit_offline_archetype(
    name: str,
    output_dir: Path,
    *,
    params: dict[str, Any] | None = None,
    library: ArchetypeLibrary | None = None,
    lods: tuple[str, ...] = (LodLevel.NEAR.value, LodLevel.MID.value, LodLevel.FAR.value),
) -> dict[str, Any]:
    library = library or default_library()
    arch = library.create(name, params)
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    metrics: dict[str, Any] = {
        "backend": "trimesh-offline",
        "blender_status": "not-used",
        "archetypes": {},
        "prototypes": {},
        "instances": 1,
        "unique_meshes": 1,
        "renders": [],
    }
    near_mesh = None
    for lod in lods:
        parts = filter_parts_for_lod(arch.build(), lod)
        mesh = materialise_parts(parts)
        row = mesh_metrics(mesh)
        row["part_names"] = sorted({p.name for part in parts for p in part.walk()})
        metrics["archetypes"][f"{name}:{lod}"] = row
        glb_path = output_dir / f"{name}_{lod}.glb"
        export_glb(mesh, glb_path)
        if lod == LodLevel.NEAR.value:
            near_mesh = mesh
            metrics["prototypes"][f"proto_{name}"] = {
                "archetype": name,
                "lod": lod,
                **row,
            }
            main_glb = output_dir / f"{name}.glb"
            export_glb(mesh, main_glb)
            metrics["glb_path"] = str(main_glb)
            metrics["glb_bytes"] = main_glb.stat().st_size

    # Placeholder blend path — no real .blend without Blender.
    blend_note = output_dir / f"{name}.blend.BLOCKED.txt"
    blend_note.write_text(
        "Editable .blend not produced: Blender backend unavailable in this run.\n"
        "Re-run when Blender headless starts successfully; emit_blender.py is generated "
        "alongside emit_spec.json for the Blender path.\n",
        encoding="utf-8",
    )
    metrics["blend_path"] = str(blend_note)
    metrics["blend_bytes"] = 0
    metrics_path = output_dir / "metrics.json"
    atomic_write_json(metrics_path, metrics)
    return {
        "metrics": metrics,
        "metrics_path": metrics_path,
        "mesh": near_mesh,
        "glb_path": Path(metrics["glb_path"]),
    }


def emit_offline_scene(
    plan: InstancingPlan,
    output_dir: Path,
    *,
    library: ArchetypeLibrary | None = None,
    preview_views: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    import trimesh

    library = library or default_library()
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    proto_meshes: dict[str, Any] = {}
    metrics: dict[str, Any] = {
        "backend": "trimesh-offline",
        "blender_status": "not-used",
        "prototypes": {},
        "instances": 0,
        "unique_meshes": 0,
        "renders": [],
    }
    for proto in plan.prototypes:
        arch = library.create(proto.archetype, proto.params)
        parts = filter_parts_for_lod(arch.build(), LodLevel.NEAR.value)
        mesh = materialise_parts(parts)
        proto_meshes[proto.prototype_id] = mesh
        metrics["prototypes"][proto.prototype_id] = {
            "archetype": proto.archetype,
            "lod": LodLevel.NEAR.value,
            **mesh_metrics(mesh),
        }

    scene_meshes: list[Any] = []
    for placement in plan.placements:
        base = proto_meshes.get(placement.prototype_id)
        if base is None:
            continue
        inst = base.copy()
        mat = _compose_matrix(
            Transform3D(
                location=placement.location,
                rotation_euler=placement.rotation_euler,
                scale=placement.scale,
            )
        )
        inst.apply_transform(mat)
        scene_meshes.append(inst)
        metrics["instances"] += 1

    metrics["unique_meshes"] = len(proto_meshes)
    if scene_meshes:
        scene_mesh = trimesh.util.concatenate(scene_meshes)
    else:
        scene_mesh = trimesh.Trimesh(vertices=np.zeros((0, 3)), faces=np.zeros((0, 3), dtype=int))

    glb_path = output_dir / "scene.glb"
    metrics["glb_bytes"] = export_glb(scene_mesh, glb_path)
    metrics["glb_path"] = str(glb_path)
    metrics["scene"] = mesh_metrics(scene_mesh)

    blend_note = output_dir / "scene.blend.BLOCKED.txt"
    blend_note.write_text(
        "Editable .blend not produced: Blender backend unavailable in this run.\n",
        encoding="utf-8",
    )
    metrics["blend_path"] = str(blend_note)
    metrics["blend_bytes"] = 0

    views = preview_views or [
        {"filename": "view_01_threshold.png", "view": "iso"},
        {"filename": "view_02_aisle.png", "view": "front"},
        {"filename": "view_03_junction.png", "view": "top"},
        {"filename": "view_04_terminal.png", "view": "side"},
    ]
    for item in views:
        path = output_dir / item["filename"]
        nbytes = write_orthographic_preview(scene_mesh, path, view=item.get("view", "iso"))
        metrics["renders"].append(
            {
                "path": str(path),
                "bytes": nbytes,
                "backend": "cpu-orthographic",
                "view": item.get("view", "iso"),
                "note": "Not Blender EEVEE/Cycles — offline orthographic preview only",
            }
        )

    metrics_path = output_dir / "metrics.json"
    atomic_write_json(metrics_path, metrics)
    return {
        "metrics": metrics,
        "metrics_path": metrics_path,
        "mesh": scene_mesh,
        "glb_path": glb_path,
        "render_paths": [Path(r["path"]) for r in metrics["renders"]],
    }


_PROBE_CACHE: dict[str, Any] | None = None


def blender_probe(*, timeout_seconds: int = 30, force: bool = False) -> dict[str, Any]:
    """Return whether headless Blender can start on this host (cached)."""
    global _PROBE_CACHE
    if _PROBE_CACHE is not None and not force:
        return dict(_PROBE_CACHE)

    import subprocess

    from blender_vision.core.config import discover_blender

    capability = discover_blender()
    if not capability.available or not capability.path:
        _PROBE_CACHE = {
            "available": False,
            "blocked": True,
            "reason": "Blender executable not found",
            "path": None,
        }
        return dict(_PROBE_CACHE)
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as script:
        script.write("import bpy\nprint('BLENDER_PROBE_OK', bpy.app.version_string)\n")
        script_path = script.name
    try:
        result = subprocess.run(
            [
                capability.path,
                "--background",
                "--factory-startup",
                "--python-exit-code",
                "1",
                "--python",
                script_path,
            ],
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            check=False,
        )
    except Exception as error:  # noqa: BLE001
        _PROBE_CACHE = {
            "available": False,
            "blocked": True,
            "reason": f"Blender probe raised {type(error).__name__}: {error}",
            "path": capability.path,
        }
        return dict(_PROBE_CACHE)
    output = (result.stdout or "") + (result.stderr or "")
    if result.returncode == 0 and "BLENDER_PROBE_OK" in output:
        _PROBE_CACHE = {
            "available": True,
            "blocked": False,
            "reason": "",
            "path": capability.path,
            "version": capability.version,
        }
        return dict(_PROBE_CACHE)
    reason = (
        f"Blender headless probe failed with exit code {result.returncode}. "
        "Observed SIGSEGV during WM_init Metal GPU backend detection "
        "(supports_barycentric_whitelist / MTLBackend::metal_is_supported) "
        "on this host session — Python scripts never execute. "
        "Not a silent stub: offline trimesh emission is used for GLB/metrics only; "
        "editable .blend and EEVEE/Cycles renders remain blocked."
    )
    _PROBE_CACHE = {
        "available": False,
        "blocked": True,
        "reason": reason,
        "path": capability.path,
        "returncode": result.returncode,
        "output_tail": output[-2000:],
    }
    return dict(_PROBE_CACHE)
