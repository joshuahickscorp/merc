from __future__ import annotations

import json
import math
import sys
from pathlib import Path
from typing import Any

import bpy
from mathutils import Vector


def _arguments() -> tuple[Path, Path]:
    if "--" not in sys.argv:
        raise SystemExit("usage: blender ... -- SPEC_JSON OUTPUT_ROOT")
    values = sys.argv[sys.argv.index("--") + 1 :]
    if len(values) != 2:
        raise SystemExit("usage: blender ... -- SPEC_JSON OUTPUT_ROOT")
    return Path(values[0]).resolve(), Path(values[1]).resolve()


def _reset() -> None:
    bpy.ops.wm.read_factory_settings(use_empty=True)
    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.length_unit = "MILLIMETERS"
    scene.unit_settings.scale_length = 0.001
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGBA"
    scene.render.film_transparent = True
    scene.render.image_settings.color_depth = "8"
    scene.render.image_settings.compression = 25
    scene.render.resolution_x = 512
    scene.render.resolution_y = 512
    scene.render.fps = 24
    scene.frame_start = 1
    scene.frame_end = 120
    scene.view_settings.look = "AgX - Medium High Contrast"
    scene.view_settings.exposure = 1.15
    if scene.world is None:
        scene.world = bpy.data.worlds.new("NOCTURNE_WORLD")
    scene.world.color = (0.004, 0.006, 0.012)


def _material(
    name: str,
    color: tuple[float, float, float, float],
    *,
    metallic: float = 0.0,
    roughness: float = 0.45,
    transmission: float = 0.0,
    alpha: float = 1.0,
    emission: tuple[float, float, float, float] | None = None,
    emission_strength: float = 0.0,
) -> bpy.types.Material:
    material = bpy.data.materials.new(name)
    material.use_nodes = True
    material.diffuse_color = color
    material.surface_render_method = "DITHERED" if alpha < 1.0 else "DITHERED"
    node = material.node_tree.nodes.get("Principled BSDF")
    node.inputs["Base Color"].default_value = color
    node.inputs["Metallic"].default_value = metallic
    node.inputs["Roughness"].default_value = roughness
    node.inputs["Alpha"].default_value = alpha
    if "Transmission Weight" in node.inputs:
        node.inputs["Transmission Weight"].default_value = transmission
    if emission is not None:
        node.inputs["Emission Color"].default_value = emission
        node.inputs["Emission Strength"].default_value = emission_strength
    return material


def _assign(
    obj: bpy.types.Object,
    material: bpy.types.Material,
    semantic_id: str,
    root: bpy.types.Object,
) -> bpy.types.Object:
    obj.name = semantic_id
    obj.data.name = f"{semantic_id}_mesh"
    obj.data.materials.append(material)
    obj["semantic_id"] = semantic_id
    obj["authority"] = "ORACLE_GROUND_TRUTH"
    obj.parent = root
    return obj


def _rounded_box(
    semantic_id: str,
    dimensions: list[float],
    center: list[float],
    radius: float,
    material: bpy.types.Material,
    root: bpy.types.Object,
) -> bpy.types.Object:
    bpy.ops.mesh.primitive_cube_add(location=center)
    obj = bpy.context.object
    obj.dimensions = dimensions
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    bevel = obj.modifiers.new("manufactured_edge_radius", "BEVEL")
    bevel.width = radius
    bevel.segments = 8
    bevel.limit_method = "ANGLE"
    bpy.context.view_layer.objects.active = obj
    bpy.ops.object.modifier_apply(modifier=bevel.name)
    return _assign(obj, material, semantic_id, root)


def _ellipsoid(
    semantic_id: str,
    center: list[float],
    radii: list[float],
    material: bpy.types.Material,
    root: bpy.types.Object,
    *,
    segments: int = 64,
    rings: int = 32,
) -> bpy.types.Object:
    bpy.ops.mesh.primitive_uv_sphere_add(
        segments=segments,
        ring_count=rings,
        location=center,
    )
    obj = bpy.context.object
    obj.scale = radii
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    bevel = obj.modifiers.new("organic_surface_smoothing", "BEVEL")
    bevel.width = 0.8
    bevel.segments = 2
    bpy.context.view_layer.objects.active = obj
    bpy.ops.object.modifier_apply(modifier=bevel.name)
    for polygon in obj.data.polygons:
        polygon.use_smooth = True
    return _assign(obj, material, semantic_id, root)


def _cylinder_y(
    semantic_id: str,
    center: list[float],
    radius: float,
    depth: float,
    material: bpy.types.Material,
    root: bpy.types.Object,
    *,
    vertices: int = 64,
) -> bpy.types.Object:
    bpy.ops.mesh.primitive_cylinder_add(
        vertices=vertices,
        radius=radius,
        depth=depth,
        location=center,
        rotation=(math.pi / 2, 0.0, 0.0),
    )
    obj = bpy.context.object
    bevel = obj.modifiers.new("machined_edge", "BEVEL")
    bevel.width = min(2.0, depth * 0.15)
    bevel.segments = 4
    bpy.context.view_layer.objects.active = obj
    bpy.ops.object.modifier_apply(modifier=bevel.name)
    for polygon in obj.data.polygons:
        polygon.use_smooth = True
    return _assign(obj, material, semantic_id, root)


def _bezier_tube(
    semantic_id: str,
    points: list[list[float]],
    radius: float,
    material: bpy.types.Material,
    root: bpy.types.Object,
    *,
    resolution: int,
    bevel_resolution: int,
) -> bpy.types.Object:
    curve = bpy.data.curves.new(f"{semantic_id}_mesh", "CURVE")
    curve.dimensions = "3D"
    curve.resolution_u = resolution
    curve.bevel_depth = radius
    curve.bevel_resolution = bevel_resolution
    curve.resolution_u = resolution
    spline = curve.splines.new("BEZIER")
    spline.bezier_points.add(len(points) - 1)
    for point, coordinate in zip(spline.bezier_points, points, strict=True):
        point.co = coordinate
        point.handle_left_type = "AUTO"
        point.handle_right_type = "AUTO"
    obj = bpy.data.objects.new(semantic_id, curve)
    bpy.context.collection.objects.link(obj)
    obj.data.materials.append(material)
    obj["semantic_id"] = semantic_id
    obj["authority"] = "ORACLE_GROUND_TRUTH"
    obj.parent = root
    return obj


def _create_scene(spec: dict[str, Any]) -> tuple[bpy.types.Object, dict[str, bpy.types.Object]]:
    root = bpy.data.objects.new("NOCTURNE_ONE_ROOT", None)
    root["benchmark_id"] = "nocturne-one-sealed-v1"
    root["oracle_seed"] = 2026072501
    bpy.context.collection.objects.link(root)
    materials = {
        "black_anodized_aluminum": _material(
            "MAT_BLACK_ANODIZED_ALUMINUM",
            (0.055, 0.067, 0.092, 1.0),
            metallic=0.88,
            roughness=0.24,
        ),
        "frosted_translucent_glass": _material(
            "MAT_FROSTED_TRANSLUCENT_GLASS",
            (0.62, 0.78, 0.82, 0.42),
            roughness=0.22,
            transmission=0.78,
            alpha=0.48,
        ),
        "warm_emissive_ceramic": _material(
            "MAT_WARM_EMISSIVE_CERAMIC",
            (0.18, 0.05, 0.015, 1.0),
            roughness=0.3,
            emission=(1.0, 0.19, 0.035, 1.0),
            emission_strength=7.5,
        ),
        "graphite_tensioned_textile": _material(
            "MAT_GRAPHITE_TENSIONED_TEXTILE",
            (0.075, 0.082, 0.102, 1.0),
            roughness=0.92,
        ),
        "machined_aluminum": _material(
            "MAT_MACHINED_ALUMINUM",
            (0.22, 0.25, 0.29, 1.0),
            metallic=0.95,
            roughness=0.18,
        ),
        "braided_graphite": _material(
            "MAT_BRAIDED_GRAPHITE",
            (0.015, 0.018, 0.024, 1.0),
            roughness=0.82,
        ),
        "internal": _material(
            "MAT_INTERNAL_DARK",
            (0.025, 0.031, 0.04, 1.0),
            metallic=0.42,
            roughness=0.5,
        ),
        "logic": _material(
            "MAT_LOGIC_BOARD",
            (0.025, 0.11, 0.085, 1.0),
            metallic=0.12,
            roughness=0.6,
        ),
    }
    objects: dict[str, bpy.types.Object] = {}
    base = spec["base"]
    objects["base"] = _rounded_box(
        "base",
        base["dimensions_mm"],
        base["center_mm"],
        base["corner_radius_mm"],
        materials["black_anodized_aluminum"],
        root,
    )
    shell = spec["outer_shell"]
    objects["outer_shell"] = _bezier_tube(
        "outer_shell",
        shell["profile_control_points_mm"],
        shell["tube_radius_mm"],
        materials["black_anodized_aluminum"],
        root,
        resolution=24,
        bevel_resolution=8,
    )
    core = spec["glass_core"]
    objects["glass_core"] = _ellipsoid(
        "glass_core",
        core["center_mm"],
        core["radii_mm"],
        materials["frosted_translucent_glass"],
        root,
    )
    disk = spec["eclipse_disk"]
    objects["eclipse_disk"] = _cylinder_y(
        "eclipse_disk",
        disk["center_mm"],
        disk["radius_mm"],
        disk["thickness_mm"],
        materials["warm_emissive_ceramic"],
        root,
    )
    membrane = spec["acoustic_membrane"]
    objects["acoustic_membrane"] = _ellipsoid(
        "acoustic_membrane",
        membrane["center_mm"],
        membrane["radii_mm"],
        materials["graphite_tensioned_textile"],
        root,
        segments=48,
        rings=24,
    )
    grille = spec["thermal_grille"]
    grille_parent = bpy.data.objects.new("thermal_grille", None)
    grille_parent["semantic_id"] = "thermal_grille"
    grille_parent["authority"] = "ORACLE_GROUND_TRUTH"
    grille_parent.parent = root
    bpy.context.collection.objects.link(grille_parent)
    slot_width, slot_depth, slot_height = grille["slot_dimensions_mm"]
    start = grille["center_mm"][2] - ((grille["count"] - 1) * grille["pitch_mm"] / 2)
    for index in range(grille["count"]):
        slot = _rounded_box(
            f"thermal_grille_slot_{index:02d}",
            [slot_width, slot_depth, slot_height],
            [
                grille["center_mm"][0],
                grille["center_mm"][1],
                start + index * grille["pitch_mm"],
            ],
            1.1,
            materials["black_anodized_aluminum"],
            grille_parent,
        )
        slot["component_of"] = "thermal_grille"
    objects["thermal_grille"] = grille_parent
    control = spec["rotary_control"]
    objects["rotary_control"] = _cylinder_y(
        "rotary_control",
        control["center_mm"],
        control["radius_mm"],
        control["depth_mm"],
        materials["machined_aluminum"],
        root,
        vertices=96,
    )
    cable = spec["braided_cable"]
    objects["braided_cable"] = _bezier_tube(
        "braided_cable",
        cable["path_control_points_mm"],
        cable["radius_mm"],
        materials["braided_graphite"],
        root,
        resolution=16,
        bevel_resolution=5,
    )
    internals = spec["internal_assembly"]["parts"]
    frame = internals["internal_frame"]
    objects["internal_frame"] = _rounded_box(
        "internal_frame",
        frame["dimensions_mm"],
        frame["center_mm"],
        8.0,
        materials["internal"],
        root,
    )
    logic = internals["logic_board"]
    objects["logic_board"] = _rounded_box(
        "logic_board",
        logic["dimensions_mm"],
        logic["center_mm"],
        2.0,
        materials["logic"],
        root,
    )
    for part_id in ("left_driver", "right_driver"):
        part = internals[part_id]
        objects[part_id] = _cylinder_y(
            part_id,
            part["center_mm"],
            part["radius_mm"],
            part["depth_mm"],
            materials["internal"],
            root,
            vertices=64,
        )
    for semantic_id, offset in spec["internal_assembly"]["exploded_offsets_mm"].items():
        obj = objects[semantic_id]
        obj["assembled_location"] = list(obj.location)
        obj["exploded_offset_mm"] = offset
        obj.keyframe_insert(data_path="location", frame=1)
        obj.location = Vector(obj.location) + Vector(offset)
        obj.keyframe_insert(data_path="location", frame=120)
        obj.location = Vector(obj["assembled_location"])
    return root, objects


def _look_at(camera: bpy.types.Object, target: tuple[float, float, float]) -> None:
    direction = Vector(target) - camera.location
    camera.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()


def _camera(
    label: str,
    location: tuple[float, float, float],
    target: tuple[float, float, float],
    lens: float,
) -> bpy.types.Object:
    data = bpy.data.cameras.new(f"CAM_{label}")
    data.lens = lens
    data.sensor_width = 36.0
    data.clip_start = 1.0
    data.clip_end = 4000.0
    camera = bpy.data.objects.new(f"CAM_{label}", data)
    bpy.context.collection.objects.link(camera)
    camera.location = location
    _look_at(camera, target)
    return camera


def _lights() -> None:
    world = bpy.context.scene.world
    world.use_nodes = True
    background = world.node_tree.nodes.get("Background")
    background.inputs["Color"].default_value = (0.004, 0.006, 0.015, 1.0)
    background.inputs["Strength"].default_value = 0.12
    for name, location, color, energy, size in (
        ("KEY", (-420.0, -480.0, 590.0), (0.58, 0.72, 1.0), 850000.0, 360.0),
        ("RIM", (420.0, 220.0, 430.0), (1.0, 0.2, 0.07), 620000.0, 260.0),
        ("FILL", (0.0, -180.0, 70.0), (0.22, 0.32, 0.52), 360000.0, 220.0),
    ):
        data = bpy.data.lights.new(name, "AREA")
        data.energy = energy
        data.color = color
        data.shape = "DISK"
        data.size = size
        obj = bpy.data.objects.new(name, data)
        bpy.context.collection.objects.link(obj)
        obj.location = location
        _look_at(obj, (0.0, 0.0, 170.0))
    sun_data = bpy.data.lights.new("ORACLE_SUN", "SUN")
    sun_data.energy = 2.5
    sun_data.color = (0.58, 0.68, 1.0)
    sun = bpy.data.objects.new("ORACLE_SUN", sun_data)
    bpy.context.collection.objects.link(sun)
    sun.rotation_euler = (math.radians(28), math.radians(-18), math.radians(-32))


def _render(camera: bpy.types.Object, path: Path, resolution: int) -> None:
    scene = bpy.context.scene
    scene.camera = camera
    scene.render.resolution_x = resolution
    scene.render.resolution_y = resolution
    scene.render.filepath = str(path)
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.render.film_transparent = True
    bpy.ops.render.render(write_still=True)


def _render_motion(
    root: bpy.types.Object,
    camera: bpy.types.Object,
    directory: Path,
    *,
    kind: str,
) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    scene = bpy.context.scene
    scene.camera = camera
    scene.render.resolution_x = 384
    scene.render.resolution_y = 384
    scene.render.film_transparent = True
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    count = 36 if kind == "turntable" else 24
    root_rotation = root.rotation_euler.z
    for index in range(count):
        if kind == "turntable":
            scene.frame_set(1)
            root.rotation_euler.z = root_rotation + math.tau * index / count
        else:
            scene.frame_set(1 + round(119 * index / max(1, count - 1)))
        scene.render.filepath = str(directory / f"{index:04d}.png")
        bpy.ops.render.render(write_still=True)
    root.rotation_euler.z = root_rotation
    scene.frame_set(1)


def _object_snapshot(objects: dict[str, bpy.types.Object]) -> dict[str, Any]:
    snapshot: dict[str, Any] = {}
    for semantic_id, obj in sorted(objects.items()):
        evaluated_objects = [
            candidate
            for candidate in [obj, *list(obj.children_recursive)]
            if candidate.type in {"MESH", "CURVE", "SURFACE", "META", "FONT"}
        ]
        points = [
            evaluated.matrix_world @ Vector(corner)
            for candidate in evaluated_objects
            for evaluated in [
                candidate.evaluated_get(bpy.context.evaluated_depsgraph_get())
            ]
            for corner in evaluated.bound_box
        ]
        if points:
            lower = Vector(
                tuple(min(point[index] for point in points) for index in range(3))
            )
            upper = Vector(
                tuple(max(point[index] for point in points) for index in range(3))
            )
            location = (lower + upper) * 0.5
            dimensions = upper - lower
        else:
            location = obj.matrix_world.translation
            dimensions = obj.dimensions
        if obj.type == "EMPTY":
            children = list(obj.children_recursive)
            vertices = sum(
                len(child.data.vertices)
                for child in children
                if child.type == "MESH"
            )
            polygons = sum(
                len(child.data.polygons)
                for child in children
                if child.type == "MESH"
            )
        elif obj.type == "MESH":
            vertices = len(obj.data.vertices)
            polygons = len(obj.data.polygons)
        else:
            evaluated = obj.evaluated_get(bpy.context.evaluated_depsgraph_get())
            mesh = evaluated.to_mesh()
            try:
                vertices = len(mesh.vertices)
                polygons = len(mesh.polygons)
            finally:
                evaluated.to_mesh_clear()
        snapshot[semantic_id] = {
            "location": [round(float(value), 6) for value in location],
            "dimensions": [round(float(value), 6) for value in dimensions],
            "object_type": obj.type,
            "vertex_count": vertices,
            "polygon_count": polygons,
            "materials": [
                slot.material.name for slot in obj.material_slots if slot.material
            ],
        }
    return snapshot


def main() -> None:
    spec_path, output_root = _arguments()
    output_root.mkdir(parents=True, exist_ok=True)
    public_root = output_root / "input-packet"
    sealed_root = output_root / "sealed-evaluator"
    for root in (public_root / "references", sealed_root / "holdouts"):
        root.mkdir(parents=True, exist_ok=True)
    spec = json.loads(spec_path.read_text(encoding="utf-8"))
    _reset()
    root, objects = _create_scene(spec)
    _lights()
    public_cameras = {
        "front": ((0.0, -760.0, 184.0), (0.0, 0.0, 176.0), 58.0),
        "rear": ((0.0, 760.0, 184.0), (0.0, 0.0, 176.0), 58.0),
        "left": ((-650.0, -300.0, 190.0), (0.0, 0.0, 176.0), 62.0),
        "right": ((650.0, -300.0, 190.0), (0.0, 0.0, 176.0), 62.0),
        "top": ((0.0, -30.0, 820.0), (0.0, 0.0, 145.0), 64.0),
        "hero": ((470.0, -650.0, 390.0), (0.0, 0.0, 168.0), 67.0),
    }
    hidden_cameras = {
        "holdout-a": ((-430.0, -610.0, 276.0), (0.0, 0.0, 173.0), 63.0),
        "holdout-b": ((440.0, 570.0, 255.0), (0.0, 0.0, 172.0), 61.0),
        "holdout-c": ((-520.0, 350.0, 430.0), (0.0, 0.0, 170.0), 69.0),
        "holdout-d": ((250.0, -420.0, 650.0), (0.0, 0.0, 150.0), 72.0),
    }
    camera_records: dict[str, Any] = {}
    public_camera_objects: dict[str, bpy.types.Object] = {}
    for label, (location, target, lens) in public_cameras.items():
        camera = _camera(label, location, target, lens)
        public_camera_objects[label] = camera
        _render(camera, public_root / "references" / f"{label}.png", 512)
        camera_records[label] = {
            "location": location,
            "target": target,
            "lens_mm": lens,
            "sensor_width_mm": 36.0,
            "resolution": [512, 512],
        }
    hidden_records: dict[str, Any] = {}
    for label, (location, target, lens) in hidden_cameras.items():
        camera = _camera(label, location, target, lens)
        _render(camera, sealed_root / "holdouts" / f"{label}.png", 640)
        hidden_records[label] = {
            "location": location,
            "target": target,
            "lens_mm": lens,
            "sensor_width_mm": 36.0,
            "resolution": [640, 640],
        }
    _render_motion(
        root,
        public_camera_objects["hero"],
        output_root / "turntable-frames",
        kind="turntable",
    )
    _render_motion(
        root,
        public_camera_objects["hero"],
        output_root / "exploded-frames",
        kind="exploded",
    )
    scene = bpy.context.scene
    scene.frame_set(1)
    scene.camera = public_camera_objects["hero"]
    blend_path = sealed_root / "nocturne-one-oracle.blend"
    bpy.ops.wm.save_as_mainfile(filepath=str(blend_path), compress=True)
    manifest = {
        "schema_version": "1",
        "benchmark_id": "nocturne-one-sealed-v1",
        "oracle_seed": 2026072501,
        "objects": _object_snapshot(objects),
        "public_cameras": camera_records,
        "hidden_cameras": hidden_records,
        "blend_path": str(blend_path),
        "frame_range": [1, 120],
        "render_engine": "BLENDER_EEVEE_NEXT",
    }
    (sealed_root / "oracle.manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
