from __future__ import annotations

import json
import math
import sys
from pathlib import Path
from typing import Any

import bmesh
import bpy
from mathutils import Vector


def _arguments() -> tuple[Path, Path, Path, Path, Path]:
    if "--" not in sys.argv:
        raise SystemExit(
            "usage: blender ... -- CANDIDATE.blend ORACLE_MANIFEST OUTPUT HERO.glb LOW.glb"
        )
    values = sys.argv[sys.argv.index("--") + 1 :]
    if len(values) != 5:
        raise SystemExit(
            "usage: blender ... -- CANDIDATE.blend ORACLE_MANIFEST OUTPUT HERO.glb LOW.glb"
        )
    return tuple(Path(value).resolve() for value in values)  # type: ignore[return-value]


def _semantic_objects() -> dict[str, bpy.types.Object]:
    result: dict[str, bpy.types.Object] = {}
    for obj in bpy.data.objects:
        semantic_id = str(obj.get("semantic_id") or obj.name)
        if semantic_id and semantic_id not in result:
            result[semantic_id] = obj
    return result


def _descendants(obj: bpy.types.Object) -> list[bpy.types.Object]:
    return [obj, *list(obj.children_recursive)]


def _world_bounds(objects: list[bpy.types.Object]) -> tuple[Vector, Vector] | None:
    points: list[Vector] = []
    depsgraph = bpy.context.evaluated_depsgraph_get()
    for obj in objects:
        if obj.type not in {"MESH", "CURVE", "SURFACE", "META", "FONT"}:
            continue
        evaluated = obj.evaluated_get(depsgraph)
        for corner in evaluated.bound_box:
            points.append(evaluated.matrix_world @ Vector(corner))
    if not points:
        return None
    return (
        Vector(tuple(min(point[index] for point in points) for index in range(3))),
        Vector(tuple(max(point[index] for point in points) for index in range(3))),
    )


def _snapshot(obj: bpy.types.Object) -> dict[str, Any]:
    bounds = _world_bounds(_descendants(obj))
    if bounds is None:
        center = Vector(obj.matrix_world.translation)
        dimensions = Vector((0.0, 0.0, 0.0))
    else:
        lower, upper = bounds
        center = (lower + upper) * 0.5
        dimensions = upper - lower
    evaluated_meshes = [
        child
        for child in _descendants(obj)
        if child.type in {"MESH", "CURVE", "SURFACE", "META", "FONT"}
    ]
    vertex_count = 0
    polygon_count = 0
    depsgraph = bpy.context.evaluated_depsgraph_get()
    for child in evaluated_meshes:
        evaluated = child.evaluated_get(depsgraph)
        mesh = evaluated.to_mesh()
        try:
            vertex_count += len(mesh.vertices)
            polygon_count += len(mesh.polygons)
        finally:
            evaluated.to_mesh_clear()
    return {
        "location": [round(float(value), 6) for value in center],
        "dimensions": [round(float(value), 6) for value in dimensions],
        "object_type": obj.type,
        "materials": sorted(
            {
                slot.material.name
                for child in _descendants(obj)
                for slot in child.material_slots
                if slot.material
            }
        ),
        "descendant_count": len(obj.children_recursive),
        "vertex_count": vertex_count,
        "polygon_count": polygon_count,
    }


def _mesh_quality() -> dict[str, Any]:
    missing_uv: list[str] = []
    non_manifold_edges: dict[str, int] = {}
    non_finite_normals: list[str] = []
    negative_scale: list[str] = []
    mesh_count = 0
    for obj in sorted(bpy.data.objects, key=lambda item: item.name):
        if obj.type != "MESH":
            continue
        mesh_count += 1
        if not obj.data.uv_layers:
            missing_uv.append(obj.name)
        if any(value < 0 for value in obj.scale):
            negative_scale.append(obj.name)
        if any(
            not all(math.isfinite(float(value)) for value in polygon.normal)
            for polygon in obj.data.polygons
        ):
            non_finite_normals.append(obj.name)
        bm = bmesh.new()
        try:
            bm.from_mesh(obj.data)
            count = sum(not edge.is_manifold for edge in bm.edges)
        finally:
            bm.free()
        if count:
            non_manifold_edges[obj.name] = count
    return {
        "mesh_count": mesh_count,
        "missing_uv_objects": missing_uv,
        "non_manifold_edges": non_manifold_edges,
        "non_finite_normal_objects": non_finite_normals,
        "negative_scale_objects": negative_scale,
    }


def _missing_textures() -> list[str]:
    missing = []
    for image in bpy.data.images:
        if image.source != "FILE" or not image.filepath:
            continue
        path = Path(bpy.path.abspath(image.filepath))
        if not path.is_file():
            missing.append(image.name)
    return sorted(missing)


def _material_details() -> dict[str, Any]:
    details: dict[str, Any] = {}
    for material in bpy.data.materials:
        node = (
            material.node_tree.nodes.get("Principled BSDF")
            if material.use_nodes and material.node_tree
            else None
        )
        if node is None:
            details[material.name] = {"uses_principled": False}
            continue
        item: dict[str, Any] = {"uses_principled": True}
        for key in (
            "Base Color",
            "Metallic",
            "Roughness",
            "Transmission Weight",
            "Alpha",
            "Emission Color",
            "Emission Strength",
        ):
            if key not in node.inputs:
                continue
            value = node.inputs[key].default_value
            item[key] = (
                [round(float(component), 6) for component in value]
                if hasattr(value, "__iter__")
                else round(float(value), 6)
            )
        details[material.name] = item
    return details


def _look_at(camera: bpy.types.Object, target: list[float]) -> None:
    direction = Vector(target) - camera.location
    camera.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()


def _render_silhouettes(
    cameras: dict[str, Any],
    output: Path,
) -> dict[str, str]:
    output.mkdir(parents=True, exist_ok=True)
    scene = bpy.context.scene
    scene.render.engine = "BLENDER_WORKBENCH"
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGBA"
    scene.render.image_settings.color_depth = "8"
    scene.render.film_transparent = True
    scene.display.shading.light = "FLAT"
    scene.display.shading.color_type = "SINGLE"
    scene.display.shading.single_color = (1.0, 1.0, 1.0)
    scene.display.shading.show_shadows = False
    scene.display.shading.show_cavity = False
    rendered: dict[str, str] = {}
    for label, record in sorted(cameras.items()):
        data = bpy.data.cameras.new(f"EVAL_CAM_{label}")
        data.lens = float(record["lens_mm"])
        data.sensor_width = float(record["sensor_width_mm"])
        data.clip_start = 1.0
        data.clip_end = 4000.0
        camera = bpy.data.objects.new(f"EVAL_CAM_{label}", data)
        bpy.context.collection.objects.link(camera)
        camera.location = record["location"]
        _look_at(camera, record["target"])
        scene.camera = camera
        width, height = record["resolution"]
        scene.render.resolution_x = int(width)
        scene.render.resolution_y = int(height)
        path = output / f"{label}.candidate.png"
        scene.render.filepath = str(path)
        bpy.ops.render.render(write_still=True)
        rendered[label] = str(path)
        bpy.data.objects.remove(camera, do_unlink=True)
        bpy.data.cameras.remove(data)
    return rendered


def _animation_snapshot(
    semantic: dict[str, bpy.types.Object], frame: int
) -> dict[str, list[float]]:
    bpy.context.scene.frame_set(frame)
    return {
        identifier: [
            round(float(value), 6)
            for value in semantic[identifier].matrix_world.translation
        ]
        for identifier in sorted(semantic)
    }


def _animation_quality(semantic: dict[str, bpy.types.Object]) -> dict[str, Any]:
    required = {
        "outer_shell",
        "glass_core",
        "eclipse_disk",
        "acoustic_membrane",
        "internal_frame",
        "logic_board",
        "left_driver",
        "right_driver",
    }
    animated = []
    for identifier in sorted(required & semantic.keys()):
        obj = semantic[identifier]
        action = obj.animation_data.action if obj.animation_data else None
        curves = list(action.fcurves) if action and hasattr(action, "fcurves") else []
        if action and not curves and hasattr(action, "layers"):
            curves = [
                curve
                for layer in action.layers
                for strip in layer.strips
                for channelbag in strip.channelbags
                for curve in channelbag.fcurves
            ]
        if any(curve.data_path == "location" for curve in curves):
            animated.append(identifier)
    first = _animation_snapshot(semantic, 120)
    second = _animation_snapshot(semantic, 120)
    assembled = _animation_snapshot(semantic, 1)
    return {
        "required_animated_parts": sorted(required),
        "observed_animated_parts": animated,
        "all_required_animated": set(animated) == required,
        "frame_120_deterministic": first == second,
        "assembled_frame_sha256_payload": assembled,
        "exploded_frame_sha256_payload": first,
    }


def _root_quality(
    semantic: dict[str, bpy.types.Object],
    required_parts: list[str],
) -> dict[str, Any]:
    root = bpy.data.objects.get("NOCTURNE_ONE_ROOT")
    if root is None:
        return {"root_present": False, "unparented_required_parts": required_parts}
    unparented = []
    for identifier in required_parts:
        obj = semantic.get(identifier)
        if obj is None:
            continue
        ancestors = set()
        cursor = obj.parent
        while cursor is not None:
            ancestors.add(cursor.name)
            cursor = cursor.parent
        if root.name not in ancestors and obj != root:
            unparented.append(identifier)
    return {
        "root_present": True,
        "root_name": root.name,
        "unparented_required_parts": sorted(unparented),
    }


def _glb_reimport(path: Path) -> dict[str, Any]:
    bpy.ops.wm.read_factory_settings(use_empty=True)
    bpy.ops.import_scene.gltf(filepath=str(path))
    names = sorted(obj.name for obj in bpy.data.objects)
    semantic = sorted(
        {
            str(obj.get("semantic_id") or obj.name).split(".")[0]
            for obj in bpy.data.objects
        }
    )
    return {
        "path": str(path),
        "object_names": names,
        "semantic_names": semantic,
        "object_count": len(names),
    }


def main() -> None:
    blend, oracle_manifest_path, output, hero_glb, low_glb = _arguments()
    output.mkdir(parents=True, exist_ok=True)
    oracle = json.loads(oracle_manifest_path.read_text(encoding="utf-8"))
    required_parts = sorted(oracle["objects"])
    bpy.ops.wm.open_mainfile(filepath=str(blend))
    semantic = _semantic_objects()
    parts = {
        identifier: _snapshot(semantic[identifier])
        for identifier in required_parts
        if identifier in semantic
    }
    primary = [
        semantic[identifier]
        for identifier in (
            "base",
            "outer_shell",
            "glass_core",
            "eclipse_disk",
            "acoustic_membrane",
            "thermal_grille",
            "rotary_control",
        )
        if identifier in semantic
    ]
    primary_bounds = _world_bounds(primary)
    dimensions = (
        [0.0, 0.0, 0.0]
        if primary_bounds is None
        else [
            round(float(value), 6)
            for value in (primary_bounds[1] - primary_bounds[0])
        ]
    )
    cameras = {
        **oracle["public_cameras"],
        **oracle["hidden_cameras"],
    }
    silhouettes = _render_silhouettes(cameras, output / "silhouettes")
    report = {
        "schema_version": "1",
        "blend_reopened": True,
        "required_parts": required_parts,
        "observed_parts": sorted(set(required_parts) & semantic.keys()),
        "missing_parts": sorted(set(required_parts) - semantic.keys()),
        "parts": parts,
        "primary_dimensions_mm": dimensions,
        "mesh_quality": _mesh_quality(),
        "missing_textures": _missing_textures(),
        "hierarchy": _root_quality(semantic, required_parts),
        "animation": _animation_quality(semantic),
        "materials": sorted(material.name for material in bpy.data.materials),
        "material_details": _material_details(),
        "silhouettes": silhouettes,
        "hero_glb_reimport": _glb_reimport(hero_glb),
        "low_glb_reimport": _glb_reimport(low_glb),
    }
    (output / "candidate-inspection.json").write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
