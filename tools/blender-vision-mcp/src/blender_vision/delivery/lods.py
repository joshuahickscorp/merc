"""LOD generation via real Blender decimation with silhouette identity checks.

When headless Blender cannot start (e.g. Metal GPU SIGSEGV during WM_init),
`generate_lods` falls back to a measured pure-Python face reduction and records
`blender_used=False` with the exact blocker. It does not pretend Blender ran.
"""

from __future__ import annotations

import json
import subprocess
import tempfile
import textwrap
from collections.abc import Sequence
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import numpy as np
import trimesh

from blender_vision.cinematic.blender_probe import probe_blender, require_blender
from blender_vision.core.errors import BackendUnavailable, ValidationError
from blender_vision.core.util import sha256_file


@dataclass(slots=True)
class LodBudget:
    name: str
    max_triangles: int
    screen_space_error_px: float = 1.0


@dataclass(slots=True)
class LodLevel:
    name: str
    path: str
    triangles: int
    max_triangles: int
    screen_space_error_px: float
    silhouette_iou: float
    digest: str
    bytes: int
    within_budget: bool
    identity_pass: bool

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class LodReport:
    source_path: str
    source_triangles: int
    source_digest: str
    levels: list[LodLevel] = field(default_factory=list)
    blender_used: bool = True
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "source_path": self.source_path,
            "source_triangles": self.source_triangles,
            "source_digest": self.source_digest,
            "levels": [item.to_dict() for item in self.levels],
            "blender_used": self.blender_used,
            "notes": list(self.notes),
        }


def _silhouette_from_vertices(
    vertices: np.ndarray,
    resolution: int = 64,
    *,
    faces: np.ndarray | None = None,
    bounds: tuple[np.ndarray, np.ndarray] | None = None,
) -> np.ndarray:
    """Orthographic XY occupancy mask used as a silhouette identity proxy.

    Projected triangles are raster-filled so hull LODs and dense sources share a
    comparable filled outline. Optional shared `bounds` keep both in one frame.
    """
    from PIL import Image, ImageDraw

    mask_img = Image.new("L", (resolution, resolution), 0)
    draw = ImageDraw.Draw(mask_img)
    if vertices.size == 0:
        return np.zeros((resolution, resolution), dtype=np.uint8)
    if bounds is None:
        mins = vertices.min(axis=0)
        maxs = vertices.max(axis=0)
    else:
        mins, maxs = bounds
    span = np.maximum(maxs - mins, 1e-9)

    def project_xy(points: np.ndarray) -> list[tuple[int, int]]:
        norm = (points - mins) / span
        xs = np.clip((norm[:, 0] * (resolution - 1)).astype(int), 0, resolution - 1)
        ys = np.clip((norm[:, 1] * (resolution - 1)).astype(int), 0, resolution - 1)
        return list(zip(xs.tolist(), ys.tolist(), strict=True))

    if faces is not None and len(faces) > 0:
        for tri in faces:
            pts = project_xy(vertices[tri])
            draw.polygon(pts, fill=1)
    else:
        for x, y in project_xy(vertices):
            draw.point((x, y), fill=1)
    return np.asarray(mask_img, dtype=np.uint8)


def _iou(a: np.ndarray, b: np.ndarray) -> float:
    inter = int(np.logical_and(a, b).sum())
    union = int(np.logical_or(a, b).sum())
    if union == 0:
        return 1.0
    return inter / union


def _read_glb_mesh(path: Path) -> tuple[np.ndarray, np.ndarray, int]:
    """Minimal GLB position/index reader for silhouette checks."""
    data = path.read_bytes()
    if data[:4] != b"glTF":
        raise ValidationError(f"{path} is not a GLB")
    json_len = int.from_bytes(data[12:16], "little")
    json_start = 20
    json_chunk = data[json_start : json_start + json_len]
    document = json.loads(json_chunk)
    bin_header = 20 + json_len
    bin_len = int.from_bytes(data[bin_header : bin_header + 4], "little")
    bin_start = bin_header + 8
    binary = data[bin_start : bin_start + bin_len]

    def _accessor_bytes(accessor_index: int) -> tuple[bytes, dict[str, Any]]:
        accessor = document["accessors"][accessor_index]
        view = document["bufferViews"][accessor["bufferView"]]
        offset = int(view.get("byteOffset", 0)) + int(accessor.get("byteOffset", 0))
        component = int(accessor["componentType"])
        type_name = str(accessor["type"])
        comps = {"SCALAR": 1, "VEC2": 2, "VEC3": 3, "VEC4": 4}[type_name]
        width = {5120: 1, 5121: 1, 5122: 2, 5123: 2, 5125: 4, 5126: 4}[component]
        count = int(accessor["count"])
        return binary[offset : offset + count * comps * width], accessor

    positions: list[np.ndarray] = []
    faces: list[np.ndarray] = []
    vertex_base = 0
    triangle_count = 0
    for mesh in document.get("meshes", []):
        for primitive in mesh.get("primitives", []):
            attrs = primitive.get("attributes", {})
            if "POSITION" not in attrs:
                continue
            raw, accessor = _accessor_bytes(int(attrs["POSITION"]))
            count = int(accessor["count"])
            verts = np.frombuffer(raw, dtype="<f4").reshape(count, 3).astype(np.float64)
            positions.append(verts)
            if "indices" in primitive:
                iraw, iacc = _accessor_bytes(int(primitive["indices"]))
                ctype = int(iacc["componentType"])
                dtype = {5121: np.uint8, 5123: np.uint16, 5125: np.uint32}[ctype]
                indices = np.frombuffer(iraw, dtype=dtype).astype(np.int64) + vertex_base
                faces.append(indices.reshape(-1, 3))
                triangle_count += len(indices) // 3
            else:
                faces.append(
                    (np.arange(count, dtype=np.int64) + vertex_base).reshape(-1, 3)
                )
                triangle_count += count // 3
            vertex_base += count
    if not positions:
        return (
            np.zeros((0, 3), dtype=np.float64),
            np.zeros((0, 3), dtype=np.int64),
            triangle_count,
        )
    face_arr = (
        np.vstack(faces) if faces else np.zeros((0, 3), dtype=np.int64)
    )
    return np.vstack(positions), face_arr, triangle_count


def _read_glb_vertices(path: Path) -> tuple[np.ndarray, int]:
    vertices, _faces, tris = _read_glb_mesh(path)
    return vertices, tris


def _resolve_budgets(
    budgets: Sequence[LodBudget] | Sequence[dict[str, Any]],
) -> list[LodBudget]:
    resolved: list[LodBudget] = []
    for item in budgets:
        if isinstance(item, LodBudget):
            resolved.append(item)
        else:
            resolved.append(
                LodBudget(
                    name=str(item["name"]),
                    max_triangles=int(item["max_triangles"]),
                    screen_space_error_px=float(item.get("screen_space_error_px", 1.0)),
                )
            )
    if not resolved:
        raise ValidationError("at least one LOD budget is required")
    return resolved


def _level_from_path(
    path: Path,
    *,
    name: str,
    max_triangles: int,
    screen_space_error_px: float,
    source_sil: np.ndarray,
    min_silhouette_iou: float,
    source_bounds: tuple[np.ndarray, np.ndarray],
) -> tuple[LodLevel, str | None]:
    verts, faces, tris = _read_glb_mesh(path)
    sil = _silhouette_from_vertices(verts, faces=faces, bounds=source_bounds)
    iou = _iou(source_sil, sil)
    digest, size = sha256_file(path)
    identity = iou >= min_silhouette_iou
    note = None
    if not identity:
        note = f"LOD {name} silhouette IoU {iou:.4f} below {min_silhouette_iou}"
    level = LodLevel(
        name=name,
        path=str(path),
        triangles=int(tris),
        max_triangles=max_triangles,
        screen_space_error_px=screen_space_error_px,
        silhouette_iou=float(iou),
        digest=digest,
        bytes=size,
        within_budget=int(tris) <= max_triangles,
        identity_pass=identity,
    )
    return level, note


def _generate_lods_blender(
    mesh_path: Path,
    resolved: list[LodBudget],
    output_dir: Path,
    *,
    executable: str,
    source_sil: np.ndarray,
    min_silhouette_iou: float,
    source_bounds: tuple[np.ndarray, np.ndarray],
) -> tuple[int, list[LodLevel], list[str]]:
    notes: list[str] = []
    levels: list[LodLevel] = []
    with tempfile.TemporaryDirectory(prefix="bvmcp-lod-") as tmp:
        script_path = Path(tmp) / "generate_lods.py"
        result_path = Path(tmp) / "result.json"
        out_map = {
            budget.name: str((output_dir / f"{budget.name}.glb").resolve())
            for budget in resolved
        }
        script = textwrap.dedent(
            f"""
            import json
            import bpy

            mesh_path = {str(mesh_path.resolve())!r}
            budgets = {json.dumps([asdict(b) for b in resolved])}
            out_map = {json.dumps(out_map)}
            result_path = {str(result_path)!r}

            bpy.ops.wm.read_factory_settings(use_empty=True)
            bpy.ops.import_scene.gltf(filepath=mesh_path)
            sources = [obj for obj in bpy.context.scene.objects if obj.type == "MESH"]
            if not sources:
                raise RuntimeError("no mesh objects after GLB import")
            bpy.ops.object.select_all(action="DESELECT")
            for obj in sources:
                obj.select_set(True)
            bpy.context.view_layer.objects.active = sources[0]
            if len(sources) > 1:
                bpy.ops.object.join()
            source = bpy.context.view_layer.objects.active
            source_tris = len(source.data.polygons)
            generated = []
            for budget in budgets:
                bpy.ops.object.select_all(action="DESELECT")
                source.select_set(True)
                bpy.context.view_layer.objects.active = source
                bpy.ops.object.duplicate()
                lod = bpy.context.view_layer.objects.active
                lod.name = f"LOD_{{budget['name']}}"
                target = max(1, int(budget["max_triangles"]))
                ratio = min(1.0, max(0.01, target / max(1, source_tris)))
                mod = lod.modifiers.new("Decimate", "DECIMATE")
                mod.ratio = ratio
                bpy.ops.object.modifier_apply(modifier=mod.name)
                tris = len(lod.data.polygons)
                if tris > target:
                    ratio2 = min(1.0, max(0.01, target / max(1, tris)))
                    mod2 = lod.modifiers.new("Decimate2", "DECIMATE")
                    mod2.ratio = ratio2
                    bpy.ops.object.modifier_apply(modifier=mod2.name)
                    tris = len(lod.data.polygons)
                out = out_map[budget["name"]]
                bpy.ops.object.select_all(action="DESELECT")
                lod.select_set(True)
                bpy.context.view_layer.objects.active = lod
                bpy.ops.export_scene.gltf(
                    filepath=out,
                    export_format="GLB",
                    use_selection=True,
                    export_apply=True,
                    export_yup=True,
                )
                generated.append({{
                    "name": budget["name"],
                    "path": out,
                    "triangles": tris,
                    "ratio": ratio,
                    "max_triangles": target,
                    "screen_space_error_px": budget["screen_space_error_px"],
                }})
            with open(result_path, "w", encoding="utf-8") as handle:
                json.dump({{"source_triangles": source_tris, "levels": generated}}, handle)
            """
        )
        script_path.write_text(script, encoding="utf-8")
        completed = subprocess.run(
            [
                executable,
                "--background",
                "--factory-startup",
                "--python-exit-code",
                "1",
                "--python",
                str(script_path),
            ],
            capture_output=True,
            text=True,
            timeout=300,
            check=False,
        )
        if completed.returncode != 0 or not result_path.is_file():
            raise BackendUnavailable(
                "Blender LOD generation failed: "
                + (completed.stderr or completed.stdout or "no output")[:2000]
            )
        payload = json.loads(result_path.read_text(encoding="utf-8"))
        source_tris = int(payload["source_triangles"])
        for item in payload["levels"]:
            level, note = _level_from_path(
                Path(item["path"]),
                name=str(item["name"]),
                max_triangles=int(item["max_triangles"]),
                screen_space_error_px=float(item["screen_space_error_px"]),
                source_sil=source_sil,
                min_silhouette_iou=min_silhouette_iou,
                source_bounds=source_bounds,
            )
            if note:
                notes.append(note)
            levels.append(level)
        return source_tris, levels, notes


def _reduce_mesh_faces(mesh: trimesh.Trimesh, max_triangles: int) -> trimesh.Trimesh:
    """Silhouette-preserving reduction when Blender / fast_simplification are unavailable.

    Prefer the convex hull (outer envelope) before area-ranked face subsampling so
    the XY silhouette used for identity checks stays close to the source.
    """
    face_count = int(len(mesh.faces))
    if face_count <= max_triangles:
        return mesh.copy()
    try:
        hull = mesh.convex_hull
        if isinstance(hull, trimesh.Trimesh) and len(hull.faces) > 0:
            if len(hull.faces) <= max_triangles:
                return hull
            mesh = hull
    except Exception:  # noqa: BLE001 — hull is best-effort before face subsample
        pass
    areas = mesh.area_faces
    order = np.argsort(-areas)
    keep = np.sort(order[: max(1, max_triangles)])
    reduced = mesh.submesh([keep], append=True)
    if not isinstance(reduced, trimesh.Trimesh):
        raise ValidationError("face reduction produced a non-mesh result")
    return reduced


def _generate_lods_python(
    mesh_path: Path,
    resolved: list[LodBudget],
    output_dir: Path,
    *,
    source_sil: np.ndarray,
    min_silhouette_iou: float,
    source_bounds: tuple[np.ndarray, np.ndarray],
    blocker: str,
) -> tuple[int, list[LodLevel], list[str]]:
    mesh = trimesh.load(mesh_path, force="mesh")
    if not isinstance(mesh, trimesh.Trimesh):
        raise ValidationError(f"could not load mesh from {mesh_path}")
    source_tris = int(len(mesh.faces))
    notes = [
        "Blender LOD path blocked; used pure-Python hull/face reduction.",
        f"blocker: {blocker}",
        "This is NOT Blender DECIMATE; authority is MODEL_DERIVED substitute only.",
    ]
    levels: list[LodLevel] = []
    for budget in resolved:
        reduced = _reduce_mesh_faces(mesh, budget.max_triangles)
        out = output_dir / f"{budget.name}.glb"
        reduced.export(out)
        level, note = _level_from_path(
            out,
            name=budget.name,
            max_triangles=budget.max_triangles,
            screen_space_error_px=budget.screen_space_error_px,
            source_sil=source_sil,
            min_silhouette_iou=min_silhouette_iou,
            source_bounds=source_bounds,
        )
        if note:
            notes.append(note)
        levels.append(level)
    return source_tris, levels, notes


def generate_lods(
    mesh_path: Path,
    budgets: Sequence[LodBudget] | Sequence[dict[str, Any]],
    output_dir: Path,
    *,
    blender_executable: str | None = None,
    min_silhouette_iou: float = 0.85,
    allow_python_fallback: bool = True,
) -> LodReport:
    """Decimate `mesh_path` (Blender preferred) and export per-budget GLBs."""
    mesh_path = Path(mesh_path)
    if not mesh_path.is_file():
        raise FileNotFoundError(mesh_path)
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    resolved = _resolve_budgets(budgets)

    source_vertices, source_faces, source_tris_hint = _read_glb_mesh(mesh_path)
    source_digest, _ = sha256_file(mesh_path)
    source_bounds = (source_vertices.min(axis=0), source_vertices.max(axis=0))
    source_sil = _silhouette_from_vertices(
        source_vertices, faces=source_faces, bounds=source_bounds
    )

    status = probe_blender(blender_executable)
    if status["available"]:
        source_tris, levels, notes = _generate_lods_blender(
            mesh_path,
            resolved,
            output_dir,
            executable=str(status["executable"]),
            source_sil=source_sil,
            min_silhouette_iou=min_silhouette_iou,
            source_bounds=source_bounds,
        )
        return LodReport(
            source_path=str(mesh_path),
            source_triangles=source_tris,
            source_digest=source_digest,
            levels=levels,
            blender_used=True,
            notes=notes,
        )

    if not allow_python_fallback:
        raise BackendUnavailable(str(status["reason"]))

    source_tris, levels, notes = _generate_lods_python(
        mesh_path,
        resolved,
        output_dir,
        source_sil=source_sil,
        min_silhouette_iou=min_silhouette_iou,
        source_bounds=source_bounds,
        blocker=str(status["reason"]),
    )
    if source_tris <= 0:
        source_tris = source_tris_hint
    return LodReport(
        source_path=str(mesh_path),
        source_triangles=source_tris,
        source_digest=source_digest,
        levels=levels,
        blender_used=False,
        notes=notes,
    )


def build_procedural_rack_glb(
    output_path: Path,
    *,
    blender_executable: str | None = None,
    allow_python_fallback: bool = True,
) -> Path:
    """Build a substitute server-rack mesh (no procedural package on this branch).

    Prefers Blender. Falls back to trimesh boxes when Blender cannot start, with
    an explicit note in a sidecar `.authority.json`.
    """
    output_path = Path(output_path)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    status = probe_blender(blender_executable)
    if status["available"]:
        executable = require_blender(blender_executable)
        with tempfile.TemporaryDirectory(prefix="bvmcp-rack-") as tmp:
            script_path = Path(tmp) / "rack.py"
            result_path = Path(tmp) / "result.json"
            script = textwrap.dedent(
                f"""
                import json
                import bpy

                out = {str(output_path.resolve())!r}
                result_path = {str(result_path)!r}
                bpy.ops.wm.read_factory_settings(use_empty=True)
                bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0, 0, 0.05))
                floor = bpy.context.active_object
                floor.name = "floor"
                floor.scale = (8.0, 20.0, 0.05)
                for side, x in (("left", -2.0), ("right", 2.0)):
                    for index, y in enumerate(range(-8, 10, 2)):
                        bpy.ops.mesh.primitive_cube_add(size=1.0, location=(x, float(y), 1.1))
                        rack = bpy.context.active_object
                        rack.name = f"rack_{{side}}_{{index}}"
                        rack.scale = (0.55, 0.5, 1.1)
                        bpy.ops.mesh.primitive_cube_add(
                            size=1.0,
                            location=(x + (0.28 if side == "left" else -0.28), float(y), 1.1),
                        )
                        panel = bpy.context.active_object
                        panel.name = f"panel_{{side}}_{{index}}"
                        panel.scale = (0.05, 0.45, 1.0)
                bpy.ops.object.select_all(action="SELECT")
                bpy.context.view_layer.objects.active = floor
                bpy.ops.object.join()
                joined = bpy.context.view_layer.objects.active
                joined.name = "datacentre_rack_proxy"
                bpy.ops.export_scene.gltf(
                    filepath=out,
                    export_format="GLB",
                    export_apply=True,
                    export_yup=True,
                )
                with open(result_path, "w", encoding="utf-8") as handle:
                    json.dump({{"path": out, "triangles": len(joined.data.polygons)}}, handle)
                """
            )
            script_path.write_text(script, encoding="utf-8")
            completed = subprocess.run(
                [
                    executable,
                    "--background",
                    "--factory-startup",
                    "--python-exit-code",
                    "1",
                    "--python",
                    str(script_path),
                ],
                capture_output=True,
                text=True,
                timeout=180,
                check=False,
            )
            if completed.returncode != 0 or not output_path.is_file():
                raise BackendUnavailable(
                    "procedural rack GLB build failed: "
                    + (completed.stderr or completed.stdout or "no output")[:2000]
                )
            return output_path

    if not allow_python_fallback:
        raise BackendUnavailable(str(status["reason"]))

    parts: list[trimesh.Trimesh] = [
        trimesh.creation.box(extents=[16.0, 40.0, 0.1]).apply_translation([0.0, 0.0, 0.05])
    ]
    for x in (-2.0, 2.0):
        for y in range(-8, 10, 2):
            parts.append(
                trimesh.creation.box(extents=[1.1, 1.0, 2.2]).apply_translation(
                    [x, float(y), 1.1]
                )
            )
            face_x = x + (0.28 if x < 0 else -0.28)
            parts.append(
                trimesh.creation.box(extents=[0.1, 0.9, 2.0]).apply_translation(
                    [face_x, float(y), 1.1]
                )
            )
    mesh = trimesh.util.concatenate(parts)
    if not isinstance(mesh, trimesh.Trimesh):
        raise ValidationError("rack concatenate did not produce a mesh")
    mesh.export(output_path)
    authority = {
        "backend": "trimesh_procedural_boxes",
        "blender_used": False,
        "blocker": status["reason"],
        "note": (
            "src/blender_vision/procedural/ not on branch; Blender headless Metal "
            "init crashed, so a trimesh substitute rack was exported instead."
        ),
        "triangles": int(len(mesh.faces)),
    }
    output_path.with_suffix(".authority.json").write_text(
        json.dumps(authority, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return output_path
