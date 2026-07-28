"""Blender-side fur grooming for the V2 organic lane.

Builds a two-layer groom (undercoat + guard hair) from guide curves driven by
painted length / density / direction attributes, then emits three delivery
forms:

  offline   real hair curves for Cycles
  shells    concentric extruded shells for the web runtime
  cards     camera-facing textured cards for the mobile LOD

The clump, frizz and density parameters are the actual values used, echoed back
in the report, so a critic can check clump scale against body scale instead of
taking the groom's word for it.
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


def _surface_samples(obj: bpy.types.Object, count: int, seed: int) -> list[tuple[Vector, Vector]]:
    """Area-weighted point sampling over the mesh, with interpolated normals."""
    rng = random.Random(seed)
    mesh = obj.data
    bm = bmesh.new()
    bm.from_mesh(mesh)
    bmesh.ops.triangulate(bm, faces=bm.faces)
    bm.faces.ensure_lookup_table()

    areas = [face.calc_area() for face in bm.faces]
    total = sum(areas)
    cumulative: list[float] = []
    running = 0.0
    for area in areas:
        running += area
        cumulative.append(running / total if total else 0.0)

    samples: list[tuple[Vector, Vector]] = []
    for _ in range(count):
        target = rng.random()
        low, high = 0, len(cumulative) - 1
        while low < high:
            middle = (low + high) // 2
            if cumulative[middle] < target:
                low = middle + 1
            else:
                high = middle
        face = bm.faces[low]
        a, b, c = (vert.co for vert in face.verts)
        u, v = rng.random(), rng.random()
        if u + v > 1.0:
            u, v = 1.0 - u, 1.0 - v
        point = a + (b - a) * u + (c - a) * v
        samples.append((obj.matrix_world @ point, face.normal.copy()))
    bm.free()
    return samples


def _length_at(point: Vector, bounds: tuple[Vector, Vector], config: dict) -> float:
    """Length map: longer along the back and crown, shorter at the muzzle."""
    lo, hi = bounds
    height = (point.z - lo.z) / max(1e-6, hi.z - lo.z)
    depth = (point.y - lo.y) / max(1e-6, hi.y - lo.y)
    base = config["length_m"]
    return base * (0.45 + 0.75 * height) * (0.55 + 0.6 * depth)


def _density_at(point: Vector, bounds: tuple[Vector, Vector], config: dict) -> float:
    lo, hi = bounds
    height = (point.z - lo.z) / max(1e-6, hi.z - lo.z)
    return max(0.15, min(1.0, 0.35 + 0.85 * height))


def _direction_at(normal: Vector, point: Vector, config: dict) -> Vector:
    """Direction map: normal, combed backwards and downwards by gravity."""
    comb = Vector(config["comb_direction"]).normalized()
    droop = Vector((0, 0, -1)) * config["gravity"]
    return (normal * 1.0 + comb * config["comb_strength"] + droop).normalized()


def _build_guides(
    source: bpy.types.Object, config: dict, seed: int
) -> tuple[list[list[Vector]], dict]:
    samples = _surface_samples(source, config["guide_count"], seed)
    coords = [source.matrix_world @ v.co for v in source.data.vertices]
    lo = Vector((min(c.x for c in coords), min(c.y for c in coords), min(c.z for c in coords)))
    hi = Vector((max(c.x for c in coords), max(c.y for c in coords), max(c.z for c in coords)))
    body_scale = max(hi - lo)

    rng = random.Random(seed + 7)
    guides: list[list[Vector]] = []
    lengths: list[float] = []
    for point, normal in samples:
        if rng.random() > _density_at(point, (lo, hi), config):
            continue
        length = _length_at(point, (lo, hi), config)
        lengths.append(length)
        direction = _direction_at(normal, point, config)
        strand = [point]
        segments = config["segments"]
        for index in range(1, segments + 1):
            t = index / segments
            # Gravity accumulates along the strand rather than being applied
            # uniformly, which is what makes the tip fall while the root stays
            # on the surface normal.
            bend = Vector((0, 0, -config["gravity"] * t * t * 0.9))
            step = (direction + bend).normalized() * (length / segments)
            strand.append(strand[-1] + step)
        guides.append(strand)

    return guides, {
        "body_scale_m": body_scale,
        "guide_count": len(guides),
        "mean_length_m": sum(lengths) / len(lengths) if lengths else 0.0,
        "max_length_m": max(lengths) if lengths else 0.0,
        "bounds": [list(lo), list(hi)],
    }


def _interpolate_children(
    guides: list[list[Vector]], config: dict, seed: int
) -> list[list[Vector]]:
    """Children clumped around their parent guide, with per-strand frizz."""
    rng = random.Random(seed + 11)
    children: list[list[Vector]] = []
    clump = config["clump"]
    frizz = config["frizz"]
    per_guide = config["children_per_guide"]

    for strand in guides:
        for _ in range(per_guide):
            offset = Vector(
                (rng.gauss(0, config["root_scatter_m"]), rng.gauss(0, config["root_scatter_m"]), 0)
            )
            child = []
            phase = rng.uniform(0, math.tau)
            for index, point in enumerate(strand):
                t = index / max(1, len(strand) - 1)
                # Clumping pulls the child back toward the guide along its
                # length, so roots scatter and tips gather.
                scatter = offset * (1.0 - clump * t)
                wobble = Vector(
                    (
                        math.sin(phase + t * 9.0) * frizz * t,
                        math.cos(phase + t * 7.5) * frizz * t,
                        math.sin(phase * 1.7 + t * 11.0) * frizz * t * 0.5,
                    )
                )
                child.append(point + scatter + wobble)
            children.append(child)
    return children


def _curves_object(name: str, strands: list[list[Vector]], radius: float) -> bpy.types.Object:
    curve = bpy.data.curves.new(name, type="CURVE")
    curve.dimensions = "3D"
    curve.bevel_depth = radius
    curve.bevel_resolution = 1
    curve.resolution_u = 2
    for strand in strands:
        spline = curve.splines.new("POLY")
        spline.points.add(len(strand) - 1)
        for index, point in enumerate(strand):
            spline.points[index].co = (point.x, point.y, point.z, 1.0)
        spline.points[0].radius = 1.0
        spline.points[-1].radius = 0.15
    obj = bpy.data.objects.new(name, curve)
    bpy.context.collection.objects.link(obj)
    return obj


def _shell_object(
    source: bpy.types.Object, name: str, layers: int, thickness: float
) -> bpy.types.Object:
    """Concentric offset shells: the standard web-runtime fur approximation."""
    bm = bmesh.new()
    for layer in range(1, layers + 1):
        offset = thickness * (layer / layers)
        shell = bmesh.new()
        shell.from_mesh(source.data)
        shell.normal_update()
        for vert in shell.verts:
            vert.co = vert.co + vert.normal * offset
        temporary = bpy.data.meshes.new(f"{name}_layer_{layer}")
        shell.to_mesh(temporary)
        shell.free()
        bm.from_mesh(temporary)
        bpy.data.meshes.remove(temporary)

    mesh = bpy.data.meshes.new(name)
    bm.to_mesh(mesh)
    bm.free()
    obj = bpy.data.objects.new(name, mesh)
    obj.matrix_world = source.matrix_world.copy()
    bpy.context.collection.objects.link(obj)
    return obj


def _card_object(strands: list[list[Vector]], name: str, width: float) -> bpy.types.Object:
    """Camera-facing quad strips: the mobile LOD."""
    bm = bmesh.new()
    for strand in strands:
        if len(strand) < 2:
            continue
        left, right = [], []
        for index, point in enumerate(strand):
            t = index / max(1, len(strand) - 1)
            direction = (strand[min(index + 1, len(strand) - 1)] - strand[max(index - 1, 0)])
            if direction.length < 1e-9:
                direction = Vector((0, 0, 1))
            side = direction.normalized().cross(Vector((0, 1, 0)))
            if side.length < 1e-6:
                side = Vector((1, 0, 0))
            side = side.normalized() * (width * (1.0 - 0.7 * t))
            left.append(bm.verts.new(point + side))
            right.append(bm.verts.new(point - side))
        bm.verts.ensure_lookup_table()
        for index in range(len(strand) - 1):
            bm.faces.new((left[index], left[index + 1], right[index + 1], right[index]))

    mesh = bpy.data.meshes.new(name)
    bm.to_mesh(mesh)
    bm.free()
    obj = bpy.data.objects.new(name, mesh)
    bpy.context.collection.objects.link(obj)
    return obj


def _count(obj: bpy.types.Object) -> int:
    if obj.type == "MESH":
        return sum(max(0, len(face.vertices) - 2) for face in obj.data.polygons)
    return sum(len(spline.points) for spline in obj.data.splines)


def main() -> None:
    payload = json.loads(sys.argv[sys.argv.index("--") + 1])
    config = payload["groom"]
    output = Path(payload["output_dir"])
    output.mkdir(parents=True, exist_ok=True)
    seed = int(payload.get("seed", 20260726))

    bpy.ops.wm.open_mainfile(filepath=payload["source_blend"])
    source = bpy.data.objects[payload["object"]]

    guides, stats = _build_guides(source, config, seed)
    if not guides:
        raise SystemExit("groom produced zero guide curves; density map is degenerate")

    undercoat_config = dict(config)
    undercoat_config.update(
        {
            "children_per_guide": config["undercoat_children_per_guide"],
            "clump": config["undercoat_clump"],
            "frizz": config["frizz"] * 1.6,
        }
    )
    undercoat_guides = [
        [point * 1.0 for point in strand[: max(2, len(strand) // 2)]] for strand in guides
    ]

    guard = _interpolate_children(guides, config, seed)
    undercoat = _interpolate_children(undercoat_guides, undercoat_config, seed + 3)

    guide_object = _curves_object("fur_guides", guides, config["guide_radius_m"])
    guard_object = _curves_object("fur_guard", guard, config["strand_radius_m"])
    under_object = _curves_object("fur_undercoat", undercoat, config["strand_radius_m"] * 0.6)
    shells = _shell_object(source, "fur_shells", config["shell_layers"], stats["mean_length_m"])
    cards = _card_object(guard[:: max(1, config["card_stride"])], "fur_cards",
                         config["strand_radius_m"] * 6)

    report = {
        "blender_version": bpy.app.version_string,
        "config": config,
        "guides": stats,
        "guard_strands": len(guard),
        "undercoat_strands": len(undercoat),
        "clump_scale_m": stats["mean_length_m"] * config["clump"],
        "clump_to_body_ratio": (
            stats["mean_length_m"] * config["clump"] / stats["body_scale_m"]
            if stats["body_scale_m"] else 0.0
        ),
        "density_per_m2": 0.0,
        "counts": {
            "guides": _count(guide_object),
            "guard": _count(guard_object),
            "undercoat": _count(under_object),
            "shells_triangles": _count(shells),
            "cards_triangles": _count(cards),
        },
    }

    area = sum(face.area for face in source.data.polygons)
    report["density_per_m2"] = (len(guard) + len(undercoat)) / area if area else 0.0
    report["surface_area_m2"] = area

    offline = output / "fur-offline.blend"
    bpy.ops.wm.save_as_mainfile(filepath=str(offline))
    report["offline_blend"] = str(offline)

    # Web forms export without the offline curves, which do not survive glTF.
    for obj in (guide_object, guard_object, under_object):
        obj.hide_viewport = obj.hide_render = True
    bpy.ops.object.select_all(action="DESELECT")
    shells.select_set(True)
    cards.select_set(True)
    source.select_set(True)
    web = output / "fur-web.glb"
    bpy.ops.export_scene.gltf(
        filepath=str(web), export_format="GLB", use_selection=True
    )
    report["web_glb"] = str(web)

    (output / "groom-report.json").write_text(json.dumps(report, indent=2))
    print(
        f"V2_FUR guides={len(guides)} guard={len(guard)} under={len(undercoat)} "
        f"clump_ratio={report['clump_to_body_ratio']:.4f} "
        f"density={report['density_per_m2']:.1f}/m2"
    )
    print("V2_FUR_GROOM_OK")


if __name__ == "__main__":
    main()
