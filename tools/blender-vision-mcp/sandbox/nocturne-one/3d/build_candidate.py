"""Independent parametric clean-room builder for NOCTURNE/ONE.

Run with:
  Blender --background --factory-startup --python 3d/build_candidate.py

The dimensions below are transcribed from the governed synthetic owned packet.
No source mesh or external texture is consumed by this generator.
"""

from __future__ import annotations

import math
import os
from pathlib import Path

import bpy
from mathutils import Vector


ROOT = Path(__file__).resolve().parents[1]
BLEND_PATH = ROOT / "3d" / "nocturne-one.blend"
HERO_PATH = ROOT / "public" / "assets" / "nocturne-one-hero.glb"
LOW_PATH = ROOT / "public" / "assets" / "nocturne-one-low.glb"
POSTER_PATH = ROOT / "public" / "assets" / "nocturne-one-poster.webp"
CROWN_CAMBER = float(os.environ.get("NOCTURNE_CROWN_CAMBER", "2.0"))
SKIP_POSTER = os.environ.get("NOCTURNE_SKIP_POSTER") == "1"

REQUIRED_PARTS = (
    "base",
    "outer_shell",
    "glass_core",
    "eclipse_disk",
    "acoustic_membrane",
    "thermal_grille",
    "rotary_control",
    "braided_cable",
    "internal_frame",
    "logic_board",
    "left_driver",
    "right_driver",
)

EXPLODED_OFFSETS = {
    "outer_shell": (0.0, 0.0, 44.0),
    "glass_core": (0.0, -46.0, 0.0),
    "eclipse_disk": (0.0, 58.0, 0.0),
    "acoustic_membrane": (0.0, -82.0, 0.0),
    "internal_frame": (0.0, 28.0, 0.0),
    "logic_board": (0.0, 76.0, -18.0),
    "left_driver": (-58.0, -28.0, 0.0),
    "right_driver": (58.0, -28.0, 0.0),
}

SHELL_POINTS = (
    (-112.0, 0.0, 33.0),
    (-136.0, 1.0, 112.0),
    (-124.0, 3.0, 232.0),
    (-75.0, 5.0, 319.0),
    (10.0, 3.0, 356.0),
    (95.0, 0.0, 298.0),
    (136.0, -2.0, 201.0),
    (128.0, -3.0, 88.0),
    (94.0, -2.0, 33.0),
)

CABLE_POINTS = (
    (58.0, 86.0, 20.0),
    (106.0, 121.0, 12.0),
    (164.0, 136.0, 7.0),
    (220.0, 124.0, 5.0),
)


def reset_scene() -> None:
    bpy.ops.wm.read_factory_settings(use_empty=True)
    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.length_unit = "MILLIMETERS"
    scene.unit_settings.scale_length = 0.001
    scene.frame_start = 1
    scene.frame_end = 120
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.render.film_transparent = True
    scene.render.resolution_x = 1024
    scene.render.resolution_y = 1024
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "WEBP"
    scene.render.image_settings.color_mode = "RGBA"
    scene.render.image_settings.color_depth = "8"
    scene.render.image_settings.quality = 91
    scene.render.filepath = str(POSTER_PATH)
    scene.render.image_settings.color_management = "FOLLOW_SCENE"
    scene.view_settings.look = "AgX - Medium High Contrast"
    scene.view_settings.exposure = 0.2
    world = bpy.data.worlds.new("WORLD_NOCTURNE")
    world.color = (0.001, 0.002, 0.005)
    world.use_nodes = True
    background = world.node_tree.nodes.get("Background")
    background.inputs["Color"].default_value = (0.035, 0.055, 0.095, 1.0)
    background.inputs["Strength"].default_value = 0.12
    scene.world = world


def collection(name: str) -> bpy.types.Collection:
    created = bpy.data.collections.new(name)
    bpy.context.scene.collection.children.link(created)
    return created


def move_to_collection(obj: bpy.types.Object, target: bpy.types.Collection) -> None:
    for current in list(obj.users_collection):
        current.objects.unlink(obj)
    target.objects.link(obj)


def principled_material(
    name: str,
    color: tuple[float, float, float, float],
    *,
    metallic: float,
    roughness: float,
    transmission: float = 0.0,
    emission: tuple[float, float, float, float] | None = None,
    emission_strength: float = 0.0,
) -> bpy.types.Material:
    material = bpy.data.materials.new(name)
    material.use_nodes = True
    node = material.node_tree.nodes.get("Principled BSDF")
    if node is None:
        raise RuntimeError(f"Principled BSDF missing for {name}")
    node.inputs["Base Color"].default_value = color
    node.inputs["Metallic"].default_value = metallic
    node.inputs["Roughness"].default_value = roughness
    if "Transmission Weight" in node.inputs:
        node.inputs["Transmission Weight"].default_value = transmission
    if emission is not None:
        node.inputs["Emission Color"].default_value = emission
        node.inputs["Emission Strength"].default_value = emission_strength
    if transmission:
        node.inputs["IOR"].default_value = 1.45
        node.inputs["Alpha"].default_value = 0.76
        material.diffuse_color = (*color[:3], 0.76)
        try:
            material.surface_render_method = "DITHERED"
        except (AttributeError, TypeError):
            pass
    else:
        material.diffuse_color = color
    return material


def materials() -> dict[str, bpy.types.Material]:
    return {
        "black": principled_material(
            "MAT_BLACK_ANODIZED_ALUMINUM",
            (0.006, 0.008, 0.013, 1.0),
            metallic=0.88,
            roughness=0.24,
        ),
        "glass": principled_material(
            "MAT_FROSTED_TRANSLUCENT_GLASS",
            (0.22, 0.31, 0.43, 1.0),
            metallic=0.0,
            roughness=0.28,
            transmission=0.72,
        ),
        "emissive": principled_material(
            "MAT_WARM_EMISSIVE_CERAMIC",
            (1.0, 0.27, 0.07, 1.0),
            metallic=0.0,
            roughness=0.32,
            emission=(1.0, 0.16, 0.035, 1.0),
            emission_strength=4.8,
        ),
        "membrane": principled_material(
            "MAT_GRAPHITE_TENSIONED_TEXTILE",
            (0.055, 0.075, 0.11, 1.0),
            metallic=0.02,
            roughness=0.86,
        ),
        "control": principled_material(
            "MAT_MACHINED_ALUMINUM",
            (0.3, 0.38, 0.49, 1.0),
            metallic=0.9,
            roughness=0.2,
        ),
        "braid": principled_material(
            "MAT_BRAIDED_GRAPHITE",
            (0.018, 0.021, 0.028, 1.0),
            metallic=0.12,
            roughness=0.74,
        ),
        "frame": principled_material(
            "MAT_INTERNAL_FRAME",
            (0.018, 0.022, 0.032, 1.0),
            metallic=0.68,
            roughness=0.38,
        ),
        "board": principled_material(
            "MAT_LOGIC_BOARD",
            (0.015, 0.08, 0.075, 1.0),
            metallic=0.1,
            roughness=0.58,
        ),
        "driver": principled_material(
            "MAT_DIRECTIONAL_DRIVER",
            (0.024, 0.03, 0.042, 1.0),
            metallic=0.28,
            roughness=0.62,
        ),
    }


def activate(obj: bpy.types.Object) -> None:
    bpy.ops.object.select_all(action="DESELECT")
    obj.select_set(True)
    bpy.context.view_layer.objects.active = obj


def apply_transforms(obj: bpy.types.Object) -> None:
    activate(obj)
    bpy.ops.object.transform_apply(location=False, rotation=True, scale=True)


def ensure_uv(obj: bpy.types.Object) -> None:
    if obj.type != "MESH":
        return
    activate(obj)
    bpy.ops.object.mode_set(mode="EDIT")
    bpy.ops.mesh.select_all(action="SELECT")
    bpy.ops.uv.smart_project(angle_limit=math.radians(66.0), island_margin=0.02)
    bpy.ops.mesh.normals_make_consistent(inside=False)
    bpy.ops.object.mode_set(mode="OBJECT")


def finish_mesh(
    obj: bpy.types.Object,
    semantic_id: str,
    material: bpy.types.Material,
    product_collection: bpy.types.Collection,
    *,
    smooth: bool = True,
) -> bpy.types.Object:
    obj.name = semantic_id
    obj.data.name = f"MESH_{semantic_id}_LOD0"
    obj["semantic_id"] = semantic_id
    obj["lod_identity"] = "LOD0"
    obj["source_authority"] = "TEXTUAL_PARAMETRIC_GROUND_TRUTH"
    obj.data.materials.clear()
    obj.data.materials.append(material)
    move_to_collection(obj, product_collection)
    ensure_uv(obj)
    if smooth:
        for polygon in obj.data.polygons:
            polygon.use_smooth = True
    return obj


def rounded_box(
    name: str,
    center: tuple[float, float, float],
    dimensions: tuple[float, float, float],
    radius: float,
    material: bpy.types.Material,
    product_collection: bpy.types.Collection,
    *,
    segments: int = 6,
) -> bpy.types.Object:
    bpy.ops.mesh.primitive_cube_add(location=center)
    obj = bpy.context.object
    obj.dimensions = dimensions
    apply_transforms(obj)
    if radius > 0:
        bevel = obj.modifiers.new("Precision edge radii", "BEVEL")
        bevel.width = radius
        bevel.segments = segments
        bevel.limit_method = "ANGLE"
        activate(obj)
        bpy.ops.object.modifier_apply(modifier=bevel.name)
    return finish_mesh(obj, name, material, product_collection)


def ellipsoid(
    name: str,
    center: tuple[float, float, float],
    radii: tuple[float, float, float],
    material: bpy.types.Material,
    product_collection: bpy.types.Collection,
    *,
    segments: int,
    rings: int,
) -> bpy.types.Object:
    bpy.ops.mesh.primitive_uv_sphere_add(
        segments=segments, ring_count=rings, location=center
    )
    obj = bpy.context.object
    obj.scale = radii
    apply_transforms(obj)
    return finish_mesh(obj, name, material, product_collection)


def cylinder_y(
    name: str,
    center: tuple[float, float, float],
    radius: float,
    depth: float,
    material: bpy.types.Material,
    product_collection: bpy.types.Collection,
    *,
    vertices: int,
) -> bpy.types.Object:
    bpy.ops.mesh.primitive_cylinder_add(
        vertices=vertices,
        radius=radius,
        depth=depth,
        end_fill_type="NGON",
        location=center,
        rotation=(math.pi / 2.0, 0.0, 0.0),
    )
    obj = bpy.context.object
    apply_transforms(obj)
    return finish_mesh(obj, name, material, product_collection)


def catmull_rom(
    control_points: tuple[tuple[float, float, float], ...],
    samples_per_segment: int,
) -> list[Vector]:
    values = [Vector(point) for point in control_points]
    result: list[Vector] = []
    for index in range(len(values) - 1):
        p0 = values[max(0, index - 1)]
        p1 = values[index]
        p2 = values[index + 1]
        p3 = values[min(len(values) - 1, index + 2)]
        for sample in range(samples_per_segment):
            t = sample / samples_per_segment
            t2 = t * t
            t3 = t2 * t
            point = 0.5 * (
                2.0 * p1
                + (-p0 + p2) * t
                + (2.0 * p0 - 5.0 * p1 + 4.0 * p2 - p3) * t2
                + (-p0 + 3.0 * p1 - 3.0 * p2 + p3) * t3
            )
            result.append(point)
    result.append(values[-1])
    return result


def tube_mesh(
    name: str,
    path: list[Vector],
    radius: float,
    depth_scale: float,
    radial_segments: int,
    material: bpy.types.Material,
    product_collection: bpy.types.Collection,
    *,
    fit_shell_bounds: bool = False,
) -> bpy.types.Object:
    vertices: list[tuple[float, float, float]] = []
    for index, center in enumerate(path):
        before = path[max(0, index - 1)]
        after = path[min(len(path) - 1, index + 1)]
        tangent = (after - before).normalized()
        depth_axis = Vector((0.0, 1.0, 0.0))
        profile_axis = tangent.cross(depth_axis).normalized()
        for segment in range(radial_segments):
            angle = math.tau * segment / radial_segments
            point = (
                center
                + profile_axis * (math.cos(angle) * radius)
                + depth_axis * (math.sin(angle) * radius * depth_scale)
            )
            vertices.append(tuple(point))
    faces: list[tuple[int, ...]] = []
    for ring in range(len(path) - 1):
        first = ring * radial_segments
        second = (ring + 1) * radial_segments
        for segment in range(radial_segments):
            following = (segment + 1) % radial_segments
            faces.append(
                (
                    first + segment,
                    first + following,
                    second + following,
                    second + segment,
                )
            )
    faces.append(tuple(reversed(range(radial_segments))))
    last = (len(path) - 1) * radial_segments
    faces.append(tuple(last + index for index in range(radial_segments)))
    if fit_shell_bounds:
        minimum_x = min(value[0] for value in vertices)
        maximum_x = max(value[0] for value in vertices)
        midpoint_x = (minimum_x + maximum_x) * 0.5
        scale_x = 320.0 / (maximum_x - minimum_x)
        vertices = [
            (
                (value[0] - midpoint_x) * scale_x,
                value[1]
                + (
                    (1.0 if math.sin(math.tau * (index % radial_segments) / radial_segments) >= 0 else -1.0)
                    * max(0.0, value[2] - 360.0)
                    * CROWN_CAMBER
                ),
                min(value[2], 360.0),
            )
            for index, value in enumerate(vertices)
        ]
    mesh = bpy.data.meshes.new(f"MESH_{name}_LOD0")
    mesh.from_pydata(vertices, [], faces)
    mesh.update(calc_edges=True)
    uv = mesh.uv_layers.new(name="UVMap")
    path_divisor = max(1, len(path) - 1)
    for polygon in mesh.polygons:
        for loop_index in polygon.loop_indices:
            vertex_index = mesh.loops[loop_index].vertex_index
            ring = min(len(path) - 1, vertex_index // radial_segments)
            segment = vertex_index % radial_segments
            uv.data[loop_index].uv = (
                segment / radial_segments,
                ring / path_divisor,
            )
    obj = bpy.data.objects.new(name, mesh)
    product_collection.objects.link(obj)
    return finish_mesh(obj, name, material, product_collection)


def disconnected_boxes(
    name: str,
    centers: list[tuple[float, float, float]],
    dimensions: tuple[float, float, float],
    material: bpy.types.Material,
    product_collection: bpy.types.Collection,
) -> bpy.types.Object:
    vertices: list[tuple[float, float, float]] = []
    faces: list[tuple[int, ...]] = []
    dx, dy, dz = (value * 0.5 for value in dimensions)
    local_vertices = (
        (-dx, -dy, -dz),
        (dx, -dy, -dz),
        (dx, dy, -dz),
        (-dx, dy, -dz),
        (-dx, -dy, dz),
        (dx, -dy, dz),
        (dx, dy, dz),
        (-dx, dy, dz),
    )
    local_faces = (
        (0, 3, 2, 1),
        (4, 5, 6, 7),
        (0, 1, 5, 4),
        (1, 2, 6, 5),
        (2, 3, 7, 6),
        (3, 0, 4, 7),
    )
    for center in centers:
        offset = len(vertices)
        vertices.extend(
            (
                center[0] + point[0],
                center[1] + point[1],
                center[2] + point[2],
            )
            for point in local_vertices
        )
        faces.extend(tuple(offset + index for index in face) for face in local_faces)
    mesh = bpy.data.meshes.new(f"MESH_{name}_LOD0")
    mesh.from_pydata(vertices, [], faces)
    mesh.update(calc_edges=True)
    obj = bpy.data.objects.new(name, mesh)
    product_collection.objects.link(obj)
    return finish_mesh(obj, name, material, product_collection, smooth=False)


def look_at(obj: bpy.types.Object, target: tuple[float, float, float]) -> None:
    obj.rotation_euler = (Vector(target) - obj.location).to_track_quat("-Z", "Y").to_euler()


def create_lighting(
    lighting_collection: bpy.types.Collection,
    camera_collection: bpy.types.Collection,
) -> None:
    camera_data = bpy.data.cameras.new("CAM_HERO_67MM")
    camera_data.lens = 67.0
    camera_data.sensor_width = 36.0
    camera_data.clip_start = 1.0
    camera_data.clip_end = 4000.0
    camera = bpy.data.objects.new("CAM_HERO", camera_data)
    camera_collection.objects.link(camera)
    camera.location = (470.0, -650.0, 390.0)
    look_at(camera, (0.0, 0.0, 168.0))
    bpy.context.scene.camera = camera

    def area(
        name: str,
        location: tuple[float, float, float],
        energy: float,
        color: tuple[float, float, float],
        size: float,
    ) -> None:
        data = bpy.data.lights.new(name, "AREA")
        data.energy = energy
        data.color = color
        data.shape = "DISK"
        data.size = size
        light = bpy.data.objects.new(name, data)
        lighting_collection.objects.link(light)
        light.location = location
        look_at(light, (0.0, 0.0, 170.0))

    area("KEY_COOL", (-390.0, -520.0, 610.0), 18_000_000.0, (0.4, 0.62, 1.0), 310.0)
    area("FILL_FRONT", (260.0, -430.0, 250.0), 7_500_000.0, (0.3, 0.46, 0.75), 240.0)
    area("RIM_WARM", (350.0, 240.0, 390.0), 14_000_000.0, (1.0, 0.13, 0.025), 180.0)
    area("TOP_SOFT", (-40.0, 30.0, 680.0), 9_000_000.0, (0.48, 0.58, 0.8), 260.0)


def animate_parts(objects: dict[str, bpy.types.Object]) -> None:
    for name, offset in EXPLODED_OFFSETS.items():
        obj = objects[name]
        assembled = obj.location.copy()
        obj.location = assembled
        obj.keyframe_insert(data_path="location", frame=1, group="Exploded assembly")
        obj.location = assembled + Vector(offset)
        obj.keyframe_insert(data_path="location", frame=120, group="Exploded assembly")
        action = obj.animation_data.action if obj.animation_data else None
        if action:
            for curve in action.fcurves:
                for point in curve.keyframe_points:
                    point.interpolation = "SINE"
    bpy.context.scene.frame_set(1)


def export_glb(path: Path, objects: dict[str, bpy.types.Object], root: bpy.types.Object) -> None:
    bpy.ops.object.select_all(action="DESELECT")
    root.select_set(True)
    for obj in objects.values():
        obj.select_set(True)
    bpy.context.view_layer.objects.active = root
    bpy.ops.export_scene.gltf(
        filepath=str(path),
        export_format="GLB",
        use_selection=True,
        export_extras=True,
        export_animations=True,
        export_frame_range=True,
        export_skins=False,
        export_morph=False,
        export_cameras=False,
        export_lights=False,
        export_apply=False,
        export_yup=True,
    )


def make_low_lod(objects: dict[str, bpy.types.Object]) -> None:
    for name, obj in objects.items():
        if obj.type != "MESH":
            continue
        obj["lod_identity"] = "LOD1"
        obj.data.name = f"MESH_{name}_LOD1"
        polygon_count = len(obj.data.polygons)
        if polygon_count < 80 or name == "thermal_grille":
            continue
        ratio = 0.34 if name in {"outer_shell", "glass_core", "acoustic_membrane"} else 0.5
        modifier = obj.modifiers.new("LOD1 deterministic reduction", "DECIMATE")
        modifier.ratio = ratio
        modifier.use_collapse_triangulate = True
        activate(obj)
        bpy.ops.object.modifier_apply(modifier=modifier.name)


def build() -> None:
    reset_scene()
    product_collection = collection("NOCTURNE_ONE_PRODUCT")
    lighting_collection = collection("NOCTURNE_ONE_LIGHTING")
    camera_collection = collection("NOCTURNE_ONE_CAMERAS")
    mats = materials()

    root = bpy.data.objects.new("NOCTURNE_ONE_ROOT", None)
    product_collection.objects.link(root)
    root["product_id"] = "nocturne-one"
    root["units"] = "millimetres"
    root["front_axis"] = "-Y"
    root["clean_room_candidate"] = True

    objects: dict[str, bpy.types.Object] = {}
    objects["base"] = rounded_box(
        "base", (0.0, 0.0, 17.0), (320.0, 180.0, 34.0), 18.0, mats["black"], product_collection
    )
    objects["outer_shell"] = tube_mesh(
        "outer_shell",
        catmull_rom(SHELL_POINTS, 14),
        24.0,
        1.22,
        20,
        mats["black"],
        product_collection,
        fit_shell_bounds=True,
    )
    objects["glass_core"] = ellipsoid(
        "glass_core",
        (-8.0, -4.0, 178.0),
        (61.0, 48.0, 126.0),
        mats["glass"],
        product_collection,
        segments=48,
        rings=32,
    )
    objects["eclipse_disk"] = cylinder_y(
        "eclipse_disk",
        (-8.0, 39.0, 181.0),
        54.0,
        8.0,
        mats["emissive"],
        product_collection,
        vertices=64,
    )
    objects["acoustic_membrane"] = ellipsoid(
        "acoustic_membrane",
        (-8.0, -48.0, 176.0),
        (54.0, 4.0, 111.0),
        mats["membrane"],
        product_collection,
        segments=48,
        rings=32,
    )
    grille_centers = [
        (0.0, 87.0, 75.0 + (index - 11) * 6.2) for index in range(23)
    ]
    objects["thermal_grille"] = disconnected_boxes(
        "thermal_grille",
        grille_centers,
        (220.0, 2.4, 3.0),
        mats["black"],
        product_collection,
    )
    objects["rotary_control"] = cylinder_y(
        "rotary_control",
        (104.0, -76.0, 41.0),
        17.0,
        13.0,
        mats["control"],
        product_collection,
        vertices=48,
    )
    objects["braided_cable"] = tube_mesh(
        "braided_cable",
        catmull_rom(CABLE_POINTS, 12),
        3.2,
        1.0,
        10,
        mats["braid"],
        product_collection,
    )
    objects["internal_frame"] = rounded_box(
        "internal_frame",
        (-8.0, 3.0, 178.0),
        (104.0, 68.0, 236.0),
        8.0,
        mats["frame"],
        product_collection,
        segments=4,
    )
    objects["logic_board"] = rounded_box(
        "logic_board",
        (-8.0, 12.0, 76.0),
        (94.0, 4.0, 48.0),
        2.0,
        mats["board"],
        product_collection,
        segments=3,
    )
    objects["left_driver"] = cylinder_y(
        "left_driver",
        (-76.0, -8.0, 178.0),
        29.0,
        18.0,
        mats["driver"],
        product_collection,
        vertices=48,
    )
    objects["right_driver"] = cylinder_y(
        "right_driver",
        (65.0, -8.0, 178.0),
        29.0,
        18.0,
        mats["driver"],
        product_collection,
        vertices=48,
    )

    if set(objects) != set(REQUIRED_PARTS):
        raise RuntimeError("Required semantic parts drifted")
    for obj in objects.values():
        obj.parent = root

    animate_parts(objects)
    create_lighting(lighting_collection, camera_collection)
    bpy.context.scene.frame_set(1)
    if not SKIP_POSTER:
        bpy.context.scene.render.filepath = str(POSTER_PATH)
        bpy.ops.render.render(write_still=True)

    bpy.ops.wm.save_as_mainfile(filepath=str(BLEND_PATH), check_existing=False)
    export_glb(HERO_PATH, objects, root)
    make_low_lod(objects)
    export_glb(LOW_PATH, objects, root)
    print(
        {
            "blend": str(BLEND_PATH),
            "hero_glb": str(HERO_PATH),
            "low_glb": str(LOW_PATH),
            "poster": str(POSTER_PATH),
            "parts": sorted(objects),
        }
    )


if __name__ == "__main__":
    build()
