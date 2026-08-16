"""Procedural scene records and builders for the resident bench.

Records are host-safe. Builders import bpy and must only run inside Blender.
Specs match the 2026-08-15 trivial sphere+plane class and then step up weight:
dense geometry, many instances, large textures, complex Principled graph.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any, Callable

from render.lib.settings import BOUNCES, COLOR_CONFIG

# --------------------------------------------------------------------------- records


def _base_record(
    scene_id: str,
    cls: str,
    builder: str,
    description: str,
    resolution: list[int],
    samples: int,
    spec: dict[str, Any],
    incremental: dict[str, Any],
    *,
    bounce_overrides: dict[str, int] | None = None,
) -> dict[str, Any]:
    bounces = dict(BOUNCES)
    if bounce_overrides:
        bounces.update(bounce_overrides)
    return {
        "id": scene_id,
        "class": cls,
        "builder": builder,
        "description": description,
        "engine": "CYCLES",
        "device": "CPU",
        "resolution": resolution,
        "samples": samples,
        "seed": 1,
        "denoise": False,
        "use_motion_blur": False,
        "bounces": bounces,
        "color_config": dict(COLOR_CONFIG),
        "spec": spec,
        "incremental": incremental,
    }


_TRIVIAL_SPEC = {
    "sphere_radius": 1.0,
    "sphere_location": [0.0, 0.0, 1.0],
    "segments": 32,
    "rings": 16,
    "plane_size": 6.0,
    "camera_location": [4.2, -4.2, 3.1],
    "camera_target": [0.0, 0.0, 0.8],
    "lens": 50.0,
    "area_light_location": [2.5, -1.5, 5.0],
    "area_light_energy": 400.0,
    "area_light_size": 2.0,
}

_TRIVIAL_INC = {
    "transform_object": "Sphere",
    "transform_location": [0.25, 0.10, 1.05],
    "material_object": "Sphere",
    "material_node": None,
    "material_input": "Base Color",
    "material_value": [0.10, 0.65, 0.20, 1.0],
    "camera_location": [4.55, -3.90, 3.35],
    "camera_target": [0.0, 0.0, 0.8],
}


SCENE_RECORDS: list[dict[str, Any]] = [
    _base_record(
        "trivial",
        "trivial",
        "trivial",
        "UV sphere + ground plane + one area light. Reproduces the 2026-08-15 64spp 256² class.",
        [256, 256],
        64,
        dict(_TRIVIAL_SPEC),
        dict(_TRIVIAL_INC),
    ),
    _base_record(
        "trivial_hires",
        "trivial",
        "trivial",
        "Same builder as trivial, frozen at 512spp 1024² to reproduce the second 2026-08-15 measurement.",
        [1024, 1024],
        512,
        dict(_TRIVIAL_SPEC),
        dict(_TRIVIAL_INC),
    ),
    _base_record(
        "dense_geometry",
        "dense_geometry",
        "dense_geometry",
        "Icosphere subdivisions=7 (81920 tris; Blender counts the 20-face base as subdiv=1) plus a ground plane. Stresses BVH build.",
        [256, 256],
        64,
        {
            "subdivisions": 7,
            "radius": 1.2,
            "location": [0.0, 0.0, 1.2],
            "plane_size": 8.0,
            "camera_location": [4.8, -4.8, 3.4],
            "camera_target": [0.0, 0.0, 1.0],
            "area_light_location": [3.0, -2.0, 6.0],
            "area_light_energy": 500.0,
            "area_light_size": 2.5,
        },
        {
            "transform_object": "DenseIco",
            "transform_location": [0.20, -0.15, 1.25],
            "material_object": "DenseIco",
            "material_node": None,
            "material_input": "Base Color",
            "material_value": [0.75, 0.20, 0.15, 1.0],
            "camera_location": [5.1, -4.5, 3.6],
            "camera_target": [0.0, 0.0, 1.0],
        },
    ),
    _base_record(
        "many_instances",
        "many_instances",
        "many_instances",
        "32×32 = 1024 objects sharing one cube mesh. Stresses object count / instance sync.",
        [256, 256],
        64,
        {
            "grid": 32,
            "spacing": 0.30,
            "cube_size": 0.18,
            "z": 0.12,
            "plane_size": 14.0,
            "camera_location": [8.0, -8.0, 7.0],
            "camera_target": [0.0, 0.0, 0.2],
            "area_light_location": [4.0, -4.0, 10.0],
            "area_light_energy": 800.0,
            "area_light_size": 4.0,
            "sun_energy": 2.0,
        },
        {
            "transform_object": "inst_000_000",
            "transform_location": [0.40, 0.40, 0.35],
            "material_object": "inst_016_016",
            "material_node": None,
            "material_input": "Base Color",
            "material_value": [0.15, 0.55, 0.85, 1.0],
            "camera_location": [8.4, -7.6, 7.3],
            "camera_target": [0.0, 0.0, 0.2],
        },
    ),
    _base_record(
        "large_textures",
        "large_textures",
        "large_textures",
        "4096² RGB texture generated procedurally, packed, Closest-filtered onto a plane.",
        [256, 256],
        64,
        {
            "texture_w": 4096,
            "texture_h": 4096,
            "texture_filename": "large_4096.png",
            "plane_size": 4.0,
            "camera_location": [0.0, -0.15, 5.2],
            "camera_target": [0.0, 0.0, 0.0],
            "area_light_location": [1.0, -1.0, 6.0],
            "area_light_energy": 350.0,
            "area_light_size": 2.0,
        },
        {
            "transform_object": "TexturedPlane",
            "transform_location": [0.15, -0.10, 0.05],
            "material_object": "TexturedPlane",
            "material_node": None,
            "material_input": "Roughness",
            "material_value": 0.15,
            "camera_location": [0.25, -0.20, 5.4],
            "camera_target": [0.0, 0.0, 0.0],
        },
    ),
    _base_record(
        "principled_graph",
        "principled_graph",
        "principled_graph",
        "Deep Principled graph: three noises, mix, ramp, bump, fresnel, layer-weight, dielectric+metal mix, emission add.",
        [256, 256],
        64,
        {
            "radius": 1.1,
            "location": [0.0, 0.0, 1.1],
            "plane_size": 6.0,
            "camera_location": [4.0, -4.0, 3.0],
            "camera_target": [0.0, 0.0, 1.0],
            "area_light_location": [2.5, -1.5, 5.5],
            "area_light_energy": 450.0,
            "area_light_size": 2.0,
        },
        {
            "transform_object": "GraphSphere",
            "transform_location": [0.18, 0.12, 1.15],
            "material_object": "GraphSphere",
            "material_node": "Dielectric",
            "material_input": "Roughness",
            "material_value": 0.85,
            "camera_location": [4.3, -3.7, 3.2],
            "camera_target": [0.0, 0.0, 1.0],
        },
    ),
]


SCENE_BY_ID = {r["id"]: r for r in SCENE_RECORDS}


def get_record(scene_id: str) -> dict[str, Any]:
    rec = SCENE_BY_ID.get(scene_id)
    if rec is None:
        raise KeyError(f"unknown scene_id {scene_id!r}; have {sorted(SCENE_BY_ID)}")
    return rec


def validate_records() -> list[str]:
    """Return a list of problems. Empty means the spec set is well-formed."""
    errs: list[str] = []
    seen: set[str] = set()
    required = (
        "id",
        "class",
        "builder",
        "resolution",
        "samples",
        "seed",
        "engine",
        "device",
        "spec",
        "incremental",
        "bounces",
        "color_config",
    )
    for rec in SCENE_RECORDS:
        sid = rec.get("id")
        if not isinstance(sid, str) or not sid:
            errs.append("record missing id")
            continue
        if sid in seen:
            errs.append(f"duplicate id {sid}")
        seen.add(sid)
        for key in required:
            if key not in rec:
                errs.append(f"{sid}: missing {key}")
        if rec.get("engine") != "CYCLES":
            errs.append(f"{sid}: engine {rec.get('engine')!r} is not CYCLES")
        if rec.get("device") != "CPU":
            errs.append(f"{sid}: device {rec.get('device')!r} is not CPU")
        if rec.get("denoise") is not False:
            errs.append(f"{sid}: denoise must be false")
        res = rec.get("resolution")
        if not (isinstance(res, list) and len(res) == 2 and all(isinstance(x, int) and x > 0 for x in res)):
            errs.append(f"{sid}: bad resolution {res!r}")
        if int(rec.get("samples", 0)) < 1:
            errs.append(f"{sid}: samples must be >= 1")
        inc = rec.get("incremental") or {}
        for k in (
            "transform_object",
            "transform_location",
            "material_object",
            "material_input",
            "camera_location",
            "camera_target",
        ):
            if k not in inc:
                errs.append(f"{sid}: incremental missing {k}")
        if rec["builder"] not in BUILDERS:
            errs.append(f"{sid}: builder {rec['builder']!r} not registered")
    return errs


# --------------------------------------------------------------------------- builders (bpy)


def _require_bpy():
    try:
        import bpy  # type: ignore
        from mathutils import Vector  # type: ignore
    except ImportError as exc:
        raise RuntimeError("scene builders run only inside Blender") from exc
    return bpy, Vector


def reset_scene() -> None:
    bpy, _Vector = _require_bpy()
    for obj in list(bpy.data.objects):
        bpy.data.objects.remove(obj, do_unlink=True)
    for coll in (
        bpy.data.meshes,
        bpy.data.cameras,
        bpy.data.lights,
        bpy.data.materials,
        bpy.data.images,
        bpy.data.curves,
        bpy.data.textures,
        bpy.data.particles,
        bpy.data.libraries,
        bpy.data.worlds,
    ):
        for item in list(coll):
            try:
                coll.remove(item)
            except Exception:
                pass
    scene = bpy.context.scene
    scene.frame_start = 1
    scene.frame_end = 1
    scene.frame_current = 1
    scene.render.use_motion_blur = False
    scene.render.use_border = False
    scene.render.use_crop_to_border = False


def force_cycles_cpu(scene, record: dict[str, Any], *, persistent_data: bool) -> None:
    """CPU pin kept for the resident builders. Metal lane uses pin_cycles."""
    from render.metal.device import pin_cycles

    pin_cycles(scene, record, device="CPU", metal_rt="OFF", persistent_data=persistent_data)


def set_world(rgb=(0.03, 0.03, 0.035), strength: float = 1.0) -> None:
    bpy, _Vector = _require_bpy()
    world = bpy.data.worlds.new("World")
    bpy.context.scene.world = world
    world.use_nodes = True
    bg = world.node_tree.nodes["Background"]
    bg.inputs[0].default_value = (float(rgb[0]), float(rgb[1]), float(rgb[2]), 1.0)
    bg.inputs[1].default_value = float(strength)


def add_camera(location, target, lens: float = 50.0):
    bpy, Vector = _require_bpy()
    cam_data = bpy.data.cameras.new("Camera")
    cam_data.lens = float(lens)
    cam_data.clip_start = 0.05
    cam_data.clip_end = 200.0
    cam = bpy.data.objects.new("Camera", cam_data)
    cam.location = Vector(location)
    direction = Vector(target) - cam.location
    cam.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
    bpy.context.collection.objects.link(cam)
    bpy.context.scene.camera = cam
    return cam


def add_area_light(name, location, energy, size, target=None):
    bpy, Vector = _require_bpy()
    data = bpy.data.lights.new(name, type="AREA")
    data.energy = float(energy)
    data.size = float(size)
    data.shape = "SQUARE"
    obj = bpy.data.objects.new(name, data)
    obj.location = Vector(location)
    if target is not None:
        direction = Vector(target) - obj.location
        obj.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
    bpy.context.collection.objects.link(obj)
    return obj


def add_sun(name, location, energy, rotation):
    bpy, Vector = _require_bpy()
    data = bpy.data.lights.new(name, type="SUN")
    data.energy = float(energy)
    obj = bpy.data.objects.new(name, data)
    obj.location = Vector(location)
    obj.rotation_euler = rotation
    bpy.context.collection.objects.link(obj)
    return obj


def new_principled(name: str, **inputs):
    bpy, _Vector = _require_bpy()
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    nt = mat.node_tree
    princ = nt.nodes.get("Principled BSDF")
    if princ is None:
        princ = nt.nodes.new("ShaderNodeBsdfPrincipled")
    for key, value in inputs.items():
        if key in princ.inputs:
            princ.inputs[key].default_value = value
    return mat, princ, nt


def assign(obj, mat) -> None:
    if obj.data.materials:
        obj.data.materials[0] = mat
    else:
        obj.data.materials.append(mat)


def add_plane(name, size, location=(0, 0, 0), rotation=(0, 0, 0)):
    bpy, _Vector = _require_bpy()
    bpy.ops.mesh.primitive_plane_add(size=float(size), location=location, rotation=rotation)
    obj = bpy.context.object
    obj.name = name
    return obj


def add_uv_sphere(name, radius, location, segments=32, rings=16):
    bpy, _Vector = _require_bpy()
    bpy.ops.mesh.primitive_uv_sphere_add(
        radius=float(radius),
        location=location,
        segments=int(segments),
        ring_count=int(rings),
    )
    obj = bpy.context.object
    obj.name = name
    bpy.ops.object.shade_smooth()
    return obj


def add_ico_sphere(name, radius, location, subdivisions):
    bpy, _Vector = _require_bpy()
    bpy.ops.mesh.primitive_ico_sphere_add(
        radius=float(radius), location=location, subdivisions=int(subdivisions)
    )
    obj = bpy.context.object
    obj.name = name
    return obj


def add_cube(name, size, location):
    bpy, _Vector = _require_bpy()
    bpy.ops.mesh.primitive_cube_add(size=float(size), location=location)
    obj = bpy.context.object
    obj.name = name
    return obj


def build_trivial(spec: dict[str, Any], _paths: dict[str, Path]) -> None:
    set_world()
    plane = add_plane("Ground", spec["plane_size"], (0, 0, 0))
    mat_g, _, _ = new_principled(
        "GroundMat",
        **{"Base Color": (0.55, 0.55, 0.58, 1.0), "Roughness": 0.65},
    )
    assign(plane, mat_g)
    sph = add_uv_sphere(
        "Sphere",
        spec["sphere_radius"],
        tuple(spec["sphere_location"]),
        spec.get("segments", 32),
        spec.get("rings", 16),
    )
    mat_s, _, _ = new_principled(
        "SphereMat",
        **{"Base Color": (0.8, 0.15, 0.12, 1.0), "Roughness": 0.35, "Metallic": 0.0},
    )
    assign(sph, mat_s)
    add_area_light(
        "Key",
        tuple(spec["area_light_location"]),
        spec["area_light_energy"],
        spec["area_light_size"],
        target=tuple(spec["camera_target"]),
    )
    add_camera(tuple(spec["camera_location"]), tuple(spec["camera_target"]), spec.get("lens", 50))


def build_dense_geometry(spec: dict[str, Any], _paths: dict[str, Path]) -> None:
    set_world()
    plane = add_plane("Ground", spec["plane_size"], (0, 0, 0))
    mat_g, _, _ = new_principled("GroundMat", **{"Base Color": (0.4, 0.4, 0.42, 1.0), "Roughness": 0.7})
    assign(plane, mat_g)
    ico = add_ico_sphere("DenseIco", spec["radius"], tuple(spec["location"]), spec["subdivisions"])
    mat, _, _ = new_principled(
        "DenseMat",
        **{"Base Color": (0.2, 0.45, 0.75, 1.0), "Roughness": 0.4, "Metallic": 0.15},
    )
    assign(ico, mat)
    add_area_light(
        "Key",
        tuple(spec["area_light_location"]),
        spec["area_light_energy"],
        spec["area_light_size"],
        target=tuple(spec["camera_target"]),
    )
    add_camera(tuple(spec["camera_location"]), tuple(spec["camera_target"]))


def build_many_instances(spec: dict[str, Any], _paths: dict[str, Path]) -> None:
    bpy, _Vector = _require_bpy()
    set_world()
    plane = add_plane("Ground", spec["plane_size"], (0, 0, 0))
    mat_g, _, _ = new_principled("GroundMat", **{"Base Color": (0.5, 0.5, 0.5, 1.0), "Roughness": 0.8})
    assign(plane, mat_g)
    src = add_cube("InstanceSource", spec["cube_size"], (0, 0, -10))
    mat, _, _ = new_principled(
        "InstMat",
        **{"Base Color": (0.85, 0.55, 0.15, 1.0), "Roughness": 0.3, "Metallic": 0.2},
    )
    assign(src, mat)
    src.hide_render = True
    src.hide_viewport = True
    n = int(spec["grid"])
    spacing = float(spec["spacing"])
    origin = -0.5 * (n - 1) * spacing
    mesh = src.data
    for i in range(n):
        for j in range(n):
            ob = bpy.data.objects.new(f"inst_{i:03d}_{j:03d}", mesh)
            ob.location = (origin + i * spacing, origin + j * spacing, float(spec["z"]))
            bpy.context.collection.objects.link(ob)
    add_area_light(
        "Key",
        tuple(spec["area_light_location"]),
        spec["area_light_energy"],
        spec["area_light_size"],
        target=(0, 0, spec["z"]),
    )
    add_sun("Fill", (0, 0, 8), spec.get("sun_energy", 2.0), (0.6, 0.2, 0.1))
    add_camera(tuple(spec["camera_location"]), tuple(spec["camera_target"]))


def build_large_textures(spec: dict[str, Any], paths: dict[str, Path]) -> None:
    bpy, _Vector = _require_bpy()
    from render.lib import pngutil

    set_world()
    tex_path = paths["generated"] / "textures" / spec["texture_filename"]
    if not tex_path.is_file():
        pngutil.write_procedural_png(tex_path, int(spec["texture_w"]), int(spec["texture_h"]), kind="large")
    img = bpy.data.images.load(str(tex_path))
    img.pack()
    plane = add_plane("TexturedPlane", spec["plane_size"], (0, 0, 0))
    mat, princ, nt = new_principled("LargeTexMat", **{"Roughness": 0.55})
    tex = nt.nodes.new("ShaderNodeTexImage")
    tex.image = img
    tex.interpolation = "Closest"
    tex.extension = "REPEAT"
    nt.links.new(tex.outputs["Color"], princ.inputs["Base Color"])
    assign(plane, mat)
    add_area_light(
        "Key",
        tuple(spec["area_light_location"]),
        spec["area_light_energy"],
        spec["area_light_size"],
        target=(0, 0, 0),
    )
    add_camera(tuple(spec["camera_location"]), tuple(spec["camera_target"]))


def build_principled_graph(spec: dict[str, Any], _paths: dict[str, Path]) -> None:
    bpy, _Vector = _require_bpy()
    set_world()
    plane = add_plane("Ground", spec["plane_size"], (0, 0, 0))
    mat_g, _, _ = new_principled("GroundMat", **{"Base Color": (0.35, 0.35, 0.36, 1.0), "Roughness": 0.75})
    assign(plane, mat_g)
    sph = add_uv_sphere("GraphSphere", spec["radius"], tuple(spec["location"]), 48, 24)
    mat = bpy.data.materials.new("ComplexPrincipled")
    mat.use_nodes = True
    nt = mat.node_tree
    for n in list(nt.nodes):
        nt.nodes.remove(n)
    out = nt.nodes.new("ShaderNodeOutputMaterial")
    out.location = (1200, 0)
    p_a = nt.nodes.new("ShaderNodeBsdfPrincipled")
    p_a.name = "Dielectric"
    p_a.location = (800, 200)
    p_a.inputs["Metallic"].default_value = 0.0
    p_a.inputs["Roughness"].default_value = 0.35
    p_b = nt.nodes.new("ShaderNodeBsdfPrincipled")
    p_b.name = "Metal"
    p_b.location = (800, -200)
    p_b.inputs["Metallic"].default_value = 1.0
    p_b.inputs["Roughness"].default_value = 0.15
    mix = nt.nodes.new("ShaderNodeMixShader")
    mix.location = (1050, 0)
    coord = nt.nodes.new("ShaderNodeTexCoord")
    coord.location = (-800, 0)
    mapping = nt.nodes.new("ShaderNodeMapping")
    mapping.location = (-600, 0)
    mapping.inputs["Scale"].default_value = (2.0, 2.0, 2.0)
    noises = []
    for i, scale in enumerate((2.5, 8.0, 18.0)):
        n = nt.nodes.new("ShaderNodeTexNoise")
        n.name = f"Noise{i}"
        n.location = (-400, 200 - i * 200)
        n.inputs["Scale"].default_value = scale
        n.inputs["Detail"].default_value = 6.0
        n.inputs["Roughness"].default_value = 0.55
        noises.append(n)
        nt.links.new(mapping.outputs["Vector"], n.inputs["Vector"])
    mix_ab = nt.nodes.new("ShaderNodeMixRGB")
    mix_ab.blend_type = "MIX"
    mix_ab.location = (-100, 100)
    mix_abc = nt.nodes.new("ShaderNodeMixRGB")
    mix_abc.blend_type = "MIX"
    mix_abc.location = (80, 0)
    ramp = nt.nodes.new("ShaderNodeValToRGB")
    ramp.location = (280, 80)
    ramp.color_ramp.elements[0].color = (0.05, 0.08, 0.2, 1)
    ramp.color_ramp.elements[1].color = (0.85, 0.35, 0.1, 1)
    bump = nt.nodes.new("ShaderNodeBump")
    bump.location = (280, -180)
    bump.inputs["Strength"].default_value = 0.35
    layer = nt.nodes.new("ShaderNodeLayerWeight")
    layer.location = (500, 40)
    layer.inputs["Blend"].default_value = 0.4
    emit = nt.nodes.new("ShaderNodeEmission")
    emit.location = (800, 400)
    emit.inputs["Color"].default_value = (0.2, 0.5, 1.0, 1)
    emit.inputs["Strength"].default_value = 0.15
    add_sh = nt.nodes.new("ShaderNodeAddShader")
    add_sh.location = (1150, 80)
    nt.links.new(coord.outputs["Object"], mapping.inputs["Vector"])
    nt.links.new(noises[0].outputs["Color"], mix_ab.inputs["Color1"])
    nt.links.new(noises[1].outputs["Color"], mix_ab.inputs["Color2"])
    mix_ab.inputs["Fac"].default_value = 0.5
    nt.links.new(mix_ab.outputs["Color"], mix_abc.inputs["Color1"])
    nt.links.new(noises[2].outputs["Color"], mix_abc.inputs["Color2"])
    mix_abc.inputs["Fac"].default_value = 0.35
    nt.links.new(mix_abc.outputs["Color"], ramp.inputs["Fac"])
    nt.links.new(noises[2].outputs["Fac"], bump.inputs["Height"])
    nt.links.new(ramp.outputs["Color"], p_a.inputs["Base Color"])
    nt.links.new(ramp.outputs["Color"], p_b.inputs["Base Color"])
    nt.links.new(bump.outputs["Normal"], p_a.inputs["Normal"])
    nt.links.new(bump.outputs["Normal"], p_b.inputs["Normal"])
    nt.links.new(layer.outputs["Facing"], mix.inputs["Fac"])
    nt.links.new(p_a.outputs["BSDF"], mix.inputs[1])
    nt.links.new(p_b.outputs["BSDF"], mix.inputs[2])
    nt.links.new(mix.outputs["Shader"], add_sh.inputs[0])
    nt.links.new(emit.outputs["Emission"], add_sh.inputs[1])
    nt.links.new(add_sh.outputs["Shader"], out.inputs["Surface"])
    assign(sph, mat)
    add_area_light(
        "Key",
        tuple(spec["area_light_location"]),
        spec["area_light_energy"],
        spec["area_light_size"],
        target=tuple(spec["location"]),
    )
    add_camera(tuple(spec["camera_location"]), tuple(spec["camera_target"]))


BUILDERS: dict[str, Callable[[dict[str, Any], dict[str, Path]], None]] = {
    "trivial": build_trivial,
    "dense_geometry": build_dense_geometry,
    "many_instances": build_many_instances,
    "large_textures": build_large_textures,
    "principled_graph": build_principled_graph,
}


def build(builder: str, spec: dict[str, Any], paths: dict[str, Path]) -> None:
    fn = BUILDERS.get(builder)
    if fn is None:
        raise KeyError(f"unknown builder {builder!r}")
    fn(spec, paths)


def look_at(obj, target) -> None:
    _bpy, Vector = _require_bpy()
    direction = Vector(target) - obj.location
    obj.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()


def apply_update(kind: str, payload: dict[str, Any]) -> dict[str, Any]:
    """Apply an incremental mutation. Returns a description of what changed."""
    bpy, Vector = _require_bpy()
    if kind == "camera":
        cam = bpy.context.scene.camera
        if cam is None:
            raise RuntimeError("no camera in scene")
        loc = payload.get("location")
        tgt = payload.get("target")
        if loc is not None:
            cam.location = Vector(loc)
        if tgt is not None:
            look_at(cam, tgt)
        return {
            "kind": "camera",
            "object": cam.name,
            "location": list(cam.location),
            "rotation_euler": list(cam.rotation_euler),
        }
    if kind == "transform":
        name = payload["object"]
        obj = bpy.data.objects.get(name)
        if obj is None:
            raise KeyError(f"no object {name!r}")
        loc = payload.get("location")
        if loc is not None:
            obj.location = Vector(loc)
        rot = payload.get("rotation_euler")
        if rot is not None:
            obj.rotation_euler = rot
        return {"kind": "transform", "object": name, "location": list(obj.location)}
    if kind == "material":
        name = payload["object"]
        obj = bpy.data.objects.get(name)
        if obj is None:
            raise KeyError(f"no object {name!r}")
        if not obj.data.materials:
            raise RuntimeError(f"object {name!r} has no material")
        mat = obj.data.materials[0]
        if not mat.use_nodes:
            raise RuntimeError(f"material on {name!r} has no nodes")
        node_name = payload.get("node")
        if node_name:
            node = mat.node_tree.nodes.get(node_name)
            if node is None:
                raise KeyError(f"no node {node_name!r} on {mat.name}")
        else:
            node = mat.node_tree.nodes.get("Principled BSDF")
            if node is None:
                for cand in mat.node_tree.nodes:
                    if cand.type == "BSDF_PRINCIPLED":
                        node = cand
                        break
            if node is None:
                raise RuntimeError(f"no Principled BSDF on {mat.name}")
        key = payload["input"]
        if key not in node.inputs:
            raise KeyError(f"node {node.name} has no input {key!r}")
        node.inputs[key].default_value = payload["value"]
        return {
            "kind": "material",
            "object": name,
            "material": mat.name,
            "node": node.name,
            "input": key,
        }
    raise ValueError(f"unknown UPDATE_SCENE kind {kind!r}")


def scene_identity() -> dict[str, Any]:
    bpy, _Vector = _require_bpy()
    objects = []
    n_verts = 0
    n_faces = 0
    for obj in bpy.data.objects:
        entry = {"name": obj.name, "type": obj.type, "location": list(obj.location)}
        if obj.type == "MESH" and obj.data is not None:
            entry["verts"] = len(obj.data.vertices)
            entry["polygons"] = len(obj.data.polygons)
            n_verts += len(obj.data.vertices)
            n_faces += len(obj.data.polygons)
        objects.append(entry)
    return {
        "object_count": len(objects),
        "mesh_vertex_count": n_verts,
        "mesh_polygon_count": n_faces,
        "material_count": len(bpy.data.materials),
        "image_count": len(bpy.data.images),
        "objects": objects[:32],
    }
