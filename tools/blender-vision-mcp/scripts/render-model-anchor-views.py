from __future__ import annotations

import argparse
import hashlib
import json
import math
import sys
from pathlib import Path

import bpy
from bpy_extras.object_utils import world_to_camera_view
from mathutils import Quaternion, Vector


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _arguments() -> argparse.Namespace:
    arguments = sys.argv[sys.argv.index("--") + 1 :] if "--" in sys.argv else []
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--anchors", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--resolution", type=int, default=1024)
    return parser.parse_args(arguments)


def _look_at(camera: bpy.types.Object, target: Vector, roll_degrees: float) -> None:
    direction = target - camera.location
    rotation = direction.to_track_quat("-Z", "Y")
    if roll_degrees:
        rotation = rotation @ Quaternion((0.0, 0.0, 1.0), math.radians(roll_degrees))
    camera.rotation_mode = "QUATERNION"
    camera.rotation_quaternion = rotation


def _add_area_light(
    name: str,
    location: Vector,
    target: Vector,
    energy: float,
    size: float,
) -> None:
    data = bpy.data.lights.new(name=name, type="AREA")
    data.energy = energy
    data.shape = "DISK"
    data.size = size
    light = bpy.data.objects.new(name, data)
    bpy.context.collection.objects.link(light)
    light.location = location
    _look_at(light, target, 0.0)


def main() -> None:
    args = _arguments()
    source = Path(args.input).expanduser().resolve()
    anchor_path = Path(args.anchors).expanduser().resolve()
    output_dir = Path(args.output_dir).expanduser().resolve()
    if args.resolution < 256 or args.resolution > 4096:
        raise ValueError("render resolution must be between 256 and 4096")
    anchor_document = json.loads(anchor_path.read_text(encoding="utf-8"))
    anchor_by_name = {
        item["object"]: item for item in anchor_document.get("anchors", [])
    }
    if not anchor_by_name:
        raise ValueError("anchor manifest contains no model objects")
    bpy.ops.wm.read_factory_settings(use_empty=True)
    bpy.ops.import_scene.gltf(filepath=str(source))
    scene = bpy.context.scene
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.render.resolution_x = args.resolution
    scene.render.resolution_y = args.resolution
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "PNG"
    scene.render.film_transparent = False
    scene.render.image_settings.color_mode = "RGB"
    scene.view_settings.look = "AgX - Medium High Contrast"
    if scene.world is None:
        scene.world = bpy.data.worlds.new("AnchorProposalWorld")
    scene.world.color = (0.025, 0.025, 0.025)
    camera_data = bpy.data.cameras.new("AnchorProposalCamera")
    camera_data.lens = 52.0
    camera_data.sensor_width = 36.0
    camera_data.clip_start = 0.01
    camera_data.clip_end = 1000.0
    camera = bpy.data.objects.new("AnchorProposalCamera", camera_data)
    bpy.context.collection.objects.link(camera)
    scene.camera = camera
    bounds = anchor_document["bounds_model_units"]
    minimum = Vector(bounds["minimum"])
    maximum = Vector(bounds["maximum"])
    center = (minimum + maximum) * 0.5
    radius = max(bounds["dimensions"]) * 1.45
    _add_area_light(
        "Key", center + Vector((-radius, -radius, radius)), center, 1800.0, radius
    )
    _add_area_light(
        "Fill",
        center + Vector((radius, -radius * 0.5, radius * 0.4)),
        center,
        1100.0,
        radius,
    )
    _add_area_light(
        "Rim", center + Vector((0.0, radius, radius)), center, 1400.0, radius
    )
    directions = {
        "front": (0.0, -1.0, 0.05),
        "front_high": (0.0, -1.0, 0.45),
        "front_low": (0.0, -1.0, -0.45),
        "rear": (0.0, 1.0, 0.05),
        "left": (-1.0, 0.0, 0.05),
        "right": (1.0, 0.0, 0.05),
        "top": (0.0, -0.1, 1.0),
        "bottom": (0.0, -0.1, -1.0),
        "bottom_front": (0.0, -0.7, -0.7),
        "bottom_rear": (0.0, 0.7, -0.7),
        "bottom_left": (-0.7, 0.0, -0.7),
        "bottom_right": (0.7, 0.0, -0.7),
        "front_left": (-0.7, -0.7, 0.2),
        "front_right": (0.7, -0.7, 0.2),
        "rear_left": (-0.7, 0.7, 0.2),
        "rear_right": (0.7, 0.7, 0.2),
    }
    output_dir.mkdir(parents=True, exist_ok=True)
    rendered_views = []
    for label, raw_direction in directions.items():
        direction = Vector(raw_direction).normalized()
        rolls = (0.0, 90.0, 180.0, 270.0) if label.startswith("bottom") else (0.0,)
        for roll in rolls:
            view_id = f"{label}_roll_{int(roll):03d}"
            camera.location = center + direction * radius
            _look_at(camera, center, roll)
            render_path = output_dir / f"{view_id}.png"
            scene.render.filepath = str(render_path)
            bpy.ops.render.render(write_still=True)
            projected = []
            for name, anchor in sorted(anchor_by_name.items()):
                world = Vector(anchor["center_model_units"])
                normalized = world_to_camera_view(scene, camera, world)
                if normalized.z <= 0.0:
                    continue
                pixel = [
                    float(normalized.x * args.resolution),
                    float((1.0 - normalized.y) * args.resolution),
                ]
                if not (
                    0.0 <= pixel[0] < args.resolution
                    and 0.0 <= pixel[1] < args.resolution
                ):
                    continue
                projected.append(
                    {
                        "landmark_id": name,
                        "world_model_units": list(anchor["center_model_units"]),
                        "render_px": pixel,
                    }
                )
            rendered_views.append(
                {
                    "id": view_id,
                    "image": f"{view_id}.png",
                    "image_sha256": _sha256(render_path),
                    "camera_location_model_units": list(camera.location),
                    "camera_world_matrix": [list(row) for row in camera.matrix_world],
                    "lens_mm": camera_data.lens,
                    "sensor_width_mm": camera_data.sensor_width,
                    "resolution": [args.resolution, args.resolution],
                    "anchors": projected,
                }
            )
    manifest = {
        "schema_version": 2,
        "source_model": str(source),
        "source_model_sha256": _sha256(source),
        "source_anchor_manifest": str(anchor_path),
        "source_anchor_manifest_sha256": _sha256(anchor_path),
        "authority": "SYNTHETIC_VIEW_FOR_LANDMARK_PROPOSAL_ONLY",
        "views": rendered_views,
    }
    manifest_path = output_dir / "render-manifest.json"
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True), encoding="utf-8"
    )
    print(f"VISIONMCP_ANCHOR_RENDER_MANIFEST={manifest_path}")


if __name__ == "__main__":
    main()
