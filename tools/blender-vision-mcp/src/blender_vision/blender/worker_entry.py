"""Self-contained Blender worker entry point.

Only Python's standard library and Blender's bundled modules are imported so the worker can
run in Blender's isolated Python environment.
"""

from __future__ import annotations

import hashlib
import json
import sys
import traceback
from pathlib import Path

import bpy
from mathutils import Vector

ALLOWED_OPERATIONS = {"inspect_scene", "render_passes", "export_glb", "validate_scene"}


def canonical_json(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def confined(root: Path, value: str, *, must_exist: bool = False) -> Path:
    candidate = Path(value).expanduser().resolve()
    if not candidate.is_relative_to(root):
        raise ValueError(f"path escapes project root: {value}")
    if must_exist and not candidate.exists():
        raise ValueError(f"path does not exist: {value}")
    return candidate


def vector(value):
    return [round(float(component), 9) for component in value]


def scene_bounds():
    points = []
    for obj in bpy.context.scene.objects:
        if obj.type == "MESH" and not obj.hide_render:
            points.extend(obj.matrix_world @ Vector(corner) for corner in obj.bound_box)
    if not points:
        return {
            "minimum": [0.0, 0.0, 0.0],
            "maximum": [0.0, 0.0, 0.0],
            "dimensions": [0.0, 0.0, 0.0],
        }
    minimum = Vector(min(point[i] for point in points) for i in range(3))
    maximum = Vector(max(point[i] for point in points) for i in range(3))
    return {
        "minimum": vector(minimum),
        "maximum": vector(maximum),
        "dimensions": vector(maximum - minimum),
    }


def object_world_bounds(obj):
    points = [obj.matrix_world @ Vector(corner) for corner in obj.bound_box]
    minimum = Vector(min(point[i] for point in points) for i in range(3))
    maximum = Vector(max(point[i] for point in points) for i in range(3))
    return {
        "minimum": vector(minimum),
        "maximum": vector(maximum),
        "dimensions": vector(maximum - minimum),
    }


def inspect_scene(_root, _parameters):
    objects = []
    for obj in sorted(bpy.context.scene.objects, key=lambda item: item.name):
        record = {
            "name": obj.name,
            "type": obj.type,
            "location": vector(obj.location),
            "rotation_euler": vector(obj.rotation_euler),
            "scale": vector(obj.scale),
            "dimensions": vector(obj.dimensions),
            "hide_render": bool(obj.hide_render),
            "modifiers": [
                {"name": modifier.name, "type": modifier.type} for modifier in obj.modifiers
            ],
        }
        if obj.type == "MESH":
            record["world_bounds"] = object_world_bounds(obj)
            record["mesh"] = {
                "vertices": len(obj.data.vertices),
                "edges": len(obj.data.edges),
                "polygons": len(obj.data.polygons),
                "materials": [
                    slot.material.name if slot.material else None for slot in obj.material_slots
                ],
            }
        objects.append(record)
    unit = bpy.context.scene.unit_settings
    return {
        "operation": "inspect_scene",
        "blender_version": bpy.app.version_string,
        "scene": {
            "objects": objects,
            "object_count": len(objects),
            "mesh_count": len(bpy.data.meshes),
            "material_count": len(bpy.data.materials),
            "image_count": len(bpy.data.images),
            "camera": bpy.context.scene.camera.name if bpy.context.scene.camera else None,
            "bounds": scene_bounds(),
            "units": {
                "system": unit.system,
                "scale_length": unit.scale_length,
                "length_unit": unit.length_unit,
            },
        },
        "warnings": scene_warnings(),
    }


def scene_warnings():
    warnings = []
    for obj in bpy.context.scene.objects:
        if obj.type == "MESH" and any(abs(component - 1.0) > 1e-6 for component in obj.scale):
            warnings.append(
                {"code": "UNAPPLIED_SCALE", "object": obj.name, "scale": vector(obj.scale)}
            )
        if obj.type == "MESH" and any(slot.material is None for slot in obj.material_slots):
            warnings.append({"code": "EMPTY_MATERIAL_SLOT", "object": obj.name})
    for image in bpy.data.images:
        if (
            image.source == "FILE"
            and image.filepath
            and not Path(bpy.path.abspath(image.filepath)).exists()
        ):
            warnings.append({"code": "MISSING_TEXTURE", "image": image.name})
    return warnings


def camera_look_at(camera, target):
    direction = Vector(target) - camera.location
    camera.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()


def ensure_camera(specification, bounds):
    camera_data = bpy.data.cameras.new(specification.get("name", "BVMCP Camera"))
    camera = bpy.data.objects.new(camera_data.name, camera_data)
    bpy.context.scene.collection.objects.link(camera)
    camera.location = specification["position"]
    camera_look_at(camera, specification.get("target", [0.0, 0.0, 0.0]))
    camera_data.type = "PERSP"
    camera_data.lens = float(specification.get("lens_mm", 50.0))
    camera_data.sensor_width = float(specification.get("sensor_width_mm", 36.0))
    bpy.context.scene.camera = camera
    return camera


def render_passes(root, parameters):
    output_directory = confined(root, parameters["output_directory"])
    output_directory.mkdir(parents=True, exist_ok=True)
    scene = bpy.context.scene
    scene.render.engine = "BLENDER_WORKBENCH"
    scene.render.dither_intensity = 0.0
    scene.render.image_settings.file_format = "PNG"
    scene.render.film_transparent = True
    scene.display.shading.light = "STUDIO"
    scene.display.shading.color_type = "MATERIAL"
    scene.display.shading.show_shadows = False
    scene.display.shading.show_cavity = False
    rendered = []
    bounds = scene_bounds()
    for index, camera_specification in enumerate(parameters.get("cameras", [])):
        ensure_camera(camera_specification, bounds)
        scene.render.resolution_x = max(64, min(8192, int(camera_specification.get("width", 1024))))
        scene.render.resolution_y = max(
            64, min(8192, int(camera_specification.get("height", 1024)))
        )
        scene.render.resolution_percentage = 100
        filename = f"{index:03d}_{camera_specification.get('reference_id', 'view')}.png"
        destination = output_directory / filename
        scene.render.filepath = str(destination)
        bpy.ops.render.render(write_still=True)
        rendered.append(
            {
                "reference_id": camera_specification.get("reference_id"),
                "path": str(destination.relative_to(root)),
                "camera": bpy.context.scene.camera.name,
            }
        )
    return {"operation": "render_passes", "renders": rendered}


def validate_scene(root, parameters):
    report = inspect_scene(root, parameters)
    report["operation"] = "validate_scene"
    report["valid"] = not any(
        warning["code"] == "MISSING_TEXTURE" for warning in report["warnings"]
    )
    return report


def export_glb(root, parameters):
    output = confined(root, parameters["output"])
    output.parent.mkdir(parents=True, exist_ok=True)
    bpy.ops.export_scene.gltf(
        filepath=str(output),
        export_format="GLB",
        export_apply=True,
        use_renderable=True,
    )
    return {
        "operation": "export_glb",
        "output": str(output.relative_to(root)),
        "bytes": output.stat().st_size,
    }


OPERATIONS = {
    "inspect_scene": inspect_scene,
    "render_passes": render_passes,
    "validate_scene": validate_scene,
    "export_glb": export_glb,
}


def main():
    if "--" not in sys.argv:
        raise ValueError("worker manifest argument is required")
    arguments = sys.argv[sys.argv.index("--") + 1 :]
    if len(arguments) != 1:
        raise ValueError("expected exactly one manifest path")
    manifest_path = Path(arguments[0]).expanduser().resolve()
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    provided_hash = manifest.pop("manifest_hash", None)
    actual_hash = hashlib.sha256(canonical_json(manifest)).hexdigest()
    if provided_hash != actual_hash:
        raise ValueError("manifest hash mismatch")
    if manifest.get("schema_version") != 1 or manifest.get("safe_mode") is not True:
        raise ValueError("unsupported or unsafe worker manifest")
    operation = manifest.get("operation")
    if operation not in ALLOWED_OPERATIONS:
        raise ValueError(f"operation is not allowlisted: {operation}")
    root = Path(manifest["project_root"]).expanduser().resolve()
    confined(root, str(manifest_path), must_exist=True)
    confined(root, manifest["scene"], must_exist=True)
    output = confined(root, manifest["output"])
    output.parent.mkdir(parents=True, exist_ok=True)
    result = OPERATIONS[operation](root, manifest.get("parameters", {}))
    result.update({"ok": True, "job_id": manifest["job_id"], "manifest_hash": provided_hash})
    output.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8")


try:
    main()
except Exception:
    traceback.print_exc()
    raise
