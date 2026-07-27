"""Blender-side builder for the V2 organic benchmark targets.

Runs inside Blender via `V2BlenderExecutor`. Builds four targets with known
construction parameters, so downstream reconstruction can be scored against a
real ground truth rather than against another guess:

  organic_sculpture  an irregular branching form (non-hard-surface topology)
  plant              a branching stem with leaf clusters
  draped_cloth       a real cloth simulation baked over a collider
  animal_bust        a quadruped-style bust: skull mass, muzzle, ears, eyes

Every target is exported as GLB plus a `.blend`, and its construction
parameters are written out as the ground-truth record.
"""

from __future__ import annotations

import json
import math
import random
import sys
from pathlib import Path

import bmesh
import bpy
from mathutils import Vector


def _reset() -> None:
    bpy.ops.wm.read_factory_settings(use_empty=True)
    bpy.context.scene.unit_settings.system = "METRIC"
    bpy.context.scene.unit_settings.scale_length = 1.0


def _link(mesh_name: str, bm: bmesh.types.BMesh) -> bpy.types.Object:
    mesh = bpy.data.meshes.new(mesh_name)
    bm.to_mesh(mesh)
    bm.free()
    mesh.validate()
    obj = bpy.data.objects.new(mesh_name, mesh)
    bpy.context.collection.objects.link(obj)
    return obj


def _measure(obj: bpy.types.Object) -> dict:
    # A freshly linked object is not in the depsgraph yet; without this the
    # evaluated copy comes back empty and every measurement is silently zero.
    bpy.context.view_layer.update()
    depsgraph = bpy.context.evaluated_depsgraph_get()
    evaluated = obj.evaluated_get(depsgraph)
    mesh = evaluated.to_mesh()
    bm = bmesh.new()
    bm.from_mesh(mesh)
    coords = [obj.matrix_world @ v.co for v in bm.verts]
    if not coords:
        raise RuntimeError(f"{obj.name} evaluated to zero vertices; the builder produced nothing")
    lo = [min(c[i] for c in coords) for i in range(3)]
    hi = [max(c[i] for c in coords) for i in range(3)]
    report = {
        "vertices": len(bm.verts),
        "faces": len(bm.faces),
        "triangles": sum(max(0, len(f.verts) - 2) for f in bm.faces),
        "surface_area_m2": sum(f.calc_area() for f in bm.faces),
        "bounds_m": [lo, hi],
        "dimensions_m": [hi[i] - lo[i] for i in range(3)],
        "boundary_edges": sum(1 for e in bm.edges if e.is_boundary),
        "non_manifold_edges": sum(1 for e in bm.edges if not e.is_manifold),
    }
    bm.free()
    evaluated.to_mesh_clear()
    return report


# --------------------------------------------------------------------------
# organic sculpture: a branching, irregular form
# --------------------------------------------------------------------------


def build_organic_sculpture(seed: int) -> tuple[bpy.types.Object, dict]:
    rng = random.Random(seed)
    bm = bmesh.new()

    segments = []

    def branch(origin: Vector, direction: Vector, radius: float, length: float, depth: int) -> None:
        if depth == 0 or radius < 0.004:
            return
        end = origin + direction * length
        steps = 6
        for step in range(steps):
            t0 = step / steps
            t1 = (step + 1) / steps
            r0 = radius * (1.0 - 0.55 * t0)
            r1 = radius * (1.0 - 0.55 * t1)
            p0 = origin.lerp(end, t0)
            p1 = origin.lerp(end, t1)
            _tube(bm, p0, p1, r0, r1, rng)
        segments.append({"origin": list(origin), "end": list(end), "radius": radius})
        children = rng.randint(2, 3)
        for _ in range(children):
            axis = Vector(
                (rng.uniform(-1, 1), rng.uniform(-1, 1), rng.uniform(0.25, 1.0))
            ).normalized()
            new_direction = direction.lerp(axis, rng.uniform(0.45, 0.8)).normalized()
            branch(end, new_direction, radius * rng.uniform(0.55, 0.72), length * 0.72, depth - 1)

    branch(Vector((0, 0, 0)), Vector((0, 0, 1)), 0.05, 0.22, 4)

    bmesh.ops.remove_doubles(bm, verts=bm.verts, dist=1e-4)
    obj = _link("organic_sculpture", bm)
    modifier = obj.modifiers.new("smooth", "SUBSURF")
    modifier.levels = 1
    modifier.render_levels = 2
    return obj, {"seed": seed, "branch_segments": len(segments), "segments": segments[:64]}


def _tube(
    bm: bmesh.types.BMesh,
    start: Vector,
    end: Vector,
    r0: float,
    r1: float,
    rng: random.Random,
    sides: int = 10,
) -> None:
    direction = (end - start)
    if direction.length < 1e-6:
        return
    direction = direction.normalized()
    reference = Vector((0, 0, 1)) if abs(direction.z) < 0.9 else Vector((1, 0, 0))
    tangent = direction.cross(reference).normalized()
    bitangent = direction.cross(tangent).normalized()

    ring_a, ring_b = [], []
    for index in range(sides):
        angle = 2 * math.pi * index / sides
        offset = tangent * math.cos(angle) + bitangent * math.sin(angle)
        jitter = 1.0 + rng.uniform(-0.08, 0.08)
        ring_a.append(bm.verts.new(start + offset * r0 * jitter))
        ring_b.append(bm.verts.new(end + offset * r1 * jitter))
    bm.verts.ensure_lookup_table()
    for index in range(sides):
        nxt = (index + 1) % sides
        bm.faces.new((ring_a[index], ring_a[nxt], ring_b[nxt], ring_b[index]))


# --------------------------------------------------------------------------
# plant
# --------------------------------------------------------------------------


def build_plant(seed: int) -> tuple[bpy.types.Object, dict]:
    rng = random.Random(seed)
    bm = bmesh.new()
    stem_height = 0.42
    for step in range(14):
        t0, t1 = step / 14, (step + 1) / 14
        sway = Vector((math.sin(t0 * 3.1) * 0.02, math.cos(t0 * 2.3) * 0.02, 0))
        sway2 = Vector((math.sin(t1 * 3.1) * 0.02, math.cos(t1 * 2.3) * 0.02, 0))
        _tube(
            bm,
            Vector((0, 0, t0 * stem_height)) + sway,
            Vector((0, 0, t1 * stem_height)) + sway2,
            0.006 * (1 - 0.6 * t0),
            0.006 * (1 - 0.6 * t1),
            rng,
            sides=8,
        )

    leaves = 0
    for index in range(9):
        height = 0.12 + 0.3 * (index / 9)
        angle = index * 2.399
        origin = Vector((math.cos(angle) * 0.01, math.sin(angle) * 0.01, height))
        direction = Vector((math.cos(angle), math.sin(angle), 0.35)).normalized()
        _leaf(bm, origin, direction, 0.075 * (1 - 0.4 * index / 9), rng)
        leaves += 1

    obj = _link("plant", bm)
    return obj, {"seed": seed, "stem_height_m": stem_height, "leaf_count": leaves}


def _leaf(
    bm: bmesh.types.BMesh, origin: Vector, direction: Vector, length: float, rng: random.Random
) -> None:
    reference = Vector((0, 0, 1)) if abs(direction.z) < 0.9 else Vector((1, 0, 0))
    side = direction.cross(reference).normalized()
    width = length * 0.34
    spine = [origin + direction * (length * t) for t in (0.0, 0.25, 0.5, 0.75, 1.0)]
    profile = (0.15, 1.0, 0.92, 0.55, 0.0)
    left, right = [], []
    for point, scale in zip(spine, profile, strict=True):
        curl = side * (width * scale) + Vector((0, 0, -width * scale * 0.25))
        left.append(bm.verts.new(point + curl))
        right.append(bm.verts.new(point - curl))
    bm.verts.ensure_lookup_table()
    for index in range(len(spine) - 1):
        bm.faces.new((left[index], left[index + 1], right[index + 1], right[index]))


# --------------------------------------------------------------------------
# draped cloth: a real simulation, baked
# --------------------------------------------------------------------------


def build_draped_cloth(frames: int) -> tuple[bpy.types.Object, dict]:
    bpy.ops.mesh.primitive_cylinder_add(radius=0.09, depth=0.30, location=(0, 0, 0.15))
    collider = bpy.context.active_object
    collider.name = "drape_collider"
    collider.modifiers.new("collision", "COLLISION")
    collider.collision.thickness_outer = 0.004

    bpy.ops.mesh.primitive_grid_add(x_subdivisions=64, y_subdivisions=64, size=0.52)
    cloth = bpy.context.active_object
    cloth.name = "draped_cloth"
    cloth.location = (0, 0, 0.36)

    modifier = cloth.modifiers.new("cloth", "CLOTH")
    settings = modifier.settings
    settings.quality = 8
    settings.mass = 0.25
    settings.tension_stiffness = 12
    settings.compression_stiffness = 12
    settings.shear_stiffness = 6
    settings.bending_stiffness = 0.4
    modifier.collision_settings.use_self_collision = True
    modifier.collision_settings.self_distance_min = 0.002

    scene = bpy.context.scene
    scene.frame_start = 1
    scene.frame_end = frames
    for frame in range(1, frames + 1):
        scene.frame_set(frame)

    # Freeze the simulated state into real geometry so the asset is editable and
    # reproducible without re-running the solver.
    bpy.context.view_layer.objects.active = cloth
    bpy.ops.object.modifier_apply(modifier="cloth")
    return cloth, {"simulated_frames": frames, "collider": "cylinder r=0.09 h=0.30"}


# --------------------------------------------------------------------------
# animal bust: the synthetic ground-truth fur target
# --------------------------------------------------------------------------


def build_animal_bust(seed: int) -> tuple[bpy.types.Object, dict]:
    parts: list[dict] = []

    # A quadruped head mass built from overlapping ellipsoids, then fused with a
    # voxel remesh. Metaballs would express the same shape more directly but
    # their tessellation is unreliable in a headless factory-startup run, and a
    # target that sometimes evaluates to nothing is useless as ground truth.
    blobs = [
        ("cranium", (0.000, 0.000, 0.180), (0.075, 0.086, 0.068)),
        ("brow", (0.000, -0.055, 0.168), (0.062, 0.048, 0.038)),
        ("muzzle", (0.000, -0.105, 0.148), (0.044, 0.058, 0.036)),
        ("nose", (0.000, -0.140, 0.140), (0.026, 0.024, 0.022)),
        ("jaw", (0.000, -0.090, 0.122), (0.040, 0.054, 0.028)),
        ("neck", (0.000, 0.010, 0.098), (0.072, 0.078, 0.062)),
    ]
    for side in (-1, 1):
        tag = "L" if side < 0 else "R"
        blobs += [
            (f"cheek_{tag}", (side * 0.048, -0.052, 0.150), (0.032, 0.036, 0.030)),
            (f"ear_base_{tag}", (side * 0.052, 0.010, 0.216), (0.022, 0.030, 0.044)),
            (f"ear_tip_{tag}", (side * 0.062, 0.020, 0.252), (0.014, 0.020, 0.034)),
        ]

    pieces = []
    for label, location, radii in blobs:
        bpy.ops.mesh.primitive_uv_sphere_add(
            radius=1.0, segments=28, ring_count=18, location=location
        )
        piece = bpy.context.active_object
        piece.name = f"bust_{label}"
        piece.scale = radii
        bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
        pieces.append(piece)
        parts.append({"label": label, "location": list(location), "radii": list(radii)})

    bpy.ops.object.select_all(action="DESELECT")
    for piece in pieces:
        piece.select_set(True)
    bpy.context.view_layer.objects.active = pieces[0]
    bpy.ops.object.join()
    bust = bpy.context.active_object
    bust.name = "animal_bust"

    remesh = bust.modifiers.new("fuse", "REMESH")
    remesh.mode = "VOXEL"
    remesh.voxel_size = 0.006
    remesh.use_smooth_shade = True
    bpy.ops.object.modifier_apply(modifier="fuse")

    # Eyes are separate objects: they carry their own material and must survive
    # retopology and LOD generation as identifiable semantic parts.
    eyes = []
    for side in (-1, 1):
        bpy.ops.mesh.primitive_uv_sphere_add(
            radius=0.0125, segments=24, ring_count=16,
            location=(side * 0.036, -0.084, 0.163),
        )
        eye = bpy.context.active_object
        eye.name = f"animal_bust_eye_{'L' if side < 0 else 'R'}"
        eyes.append(eye.name)
        parts.append({"label": eye.name, "location": list(eye.location), "radius": 0.0125})

    bpy.context.view_layer.objects.active = bust
    bpy.ops.object.select_all(action="DESELECT")
    bust.select_set(True)
    bpy.ops.object.shade_smooth()

    return bust, {"seed": seed, "part_count": len(parts), "parts": parts, "eyes": eyes}


# --------------------------------------------------------------------------


def main() -> None:
    payload = json.loads(sys.argv[sys.argv.index("--") + 1])
    output = Path(payload["output_dir"])
    output.mkdir(parents=True, exist_ok=True)
    targets = payload.get("targets") or [
        "organic_sculpture",
        "plant",
        "draped_cloth",
        "animal_bust",
    ]
    seed = int(payload.get("seed", 20260726))

    ground_truth: dict = {"blender_version": bpy.app.version_string, "targets": {}}

    builders = {
        "organic_sculpture": lambda: build_organic_sculpture(seed),
        "plant": lambda: build_plant(seed + 1),
        "draped_cloth": lambda: build_draped_cloth(int(payload.get("cloth_frames", 60))),
        "animal_bust": lambda: build_animal_bust(seed + 2),
    }

    for name in targets:
        _reset()
        obj, construction = builders[name]()
        measured = _measure(obj)
        blend_path = output / f"{name}.blend"
        glb_path = output / f"{name}.glb"
        bpy.ops.wm.save_as_mainfile(filepath=str(blend_path))
        bpy.ops.object.select_all(action="SELECT")
        bpy.ops.export_scene.gltf(filepath=str(glb_path), export_format="GLB")
        ground_truth["targets"][name] = {
            "construction": construction,
            "measured": measured,
            "blend": str(blend_path),
            "glb": str(glb_path),
            "object": obj.name,
        }
        print(f"V2_ORGANIC_TARGET {name} tris={measured['triangles']} "
              f"dims={[round(d, 4) for d in measured['dimensions_m']]}")

    (output / "ground-truth.json").write_text(json.dumps(ground_truth, indent=2))
    print("V2_ORGANIC_BUILD_OK")


if __name__ == "__main__":
    main()
