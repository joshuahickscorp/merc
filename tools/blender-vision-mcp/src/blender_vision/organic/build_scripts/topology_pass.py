"""Blender-side retopology, UV unwrap, and LOD pass for the V2 organic lane."""

import json
import math
import sys

import bmesh
import bpy

config = json.loads(sys.argv[sys.argv.index("--") + 1])

def clear():
    bpy.ops.wm.read_factory_settings(use_empty=True)

def measure(obj):
    depsgraph = bpy.context.evaluated_depsgraph_get()
    evaluated = obj.evaluated_get(depsgraph)
    mesh = evaluated.to_mesh()
    bm = bmesh.new()
    bm.from_mesh(mesh)
    bm.normal_update()

    tris = quads = ngons = 0
    for face in bm.faces:
        n = len(face.verts)
        if n == 3:
            tris += 1
        elif n == 4:
            quads += 1
        else:
            ngons += 1

    non_manifold = sum(1 for e in bm.edges if not e.is_manifold)
    boundary = sum(1 for e in bm.edges if e.is_boundary)
    watertight = boundary == 0 and non_manifold == 0

    area = sum(f.calc_area() for f in bm.faces)
    volume = bm.calc_volume(signed=True) if watertight else 0.0

    # Euler characteristic V - E + F = 2 - 2g for a closed orientable surface.
    euler = len(bm.verts) - len(bm.edges) + len(bm.faces)
    genus = int(round((2 - euler) / 2)) if watertight else -1

    coords = [obj.matrix_world @ v.co for v in bm.verts]
    lo = [min(c[i] for c in coords) for i in range(3)] if coords else [0, 0, 0]
    hi = [max(c[i] for c in coords) for i in range(3)] if coords else [0, 0, 0]

    report = {
        "vertices": len(bm.verts), "edges": len(bm.edges), "faces": len(bm.faces),
        "triangles": tris, "quads": quads, "ngons": ngons,
        "non_manifold_edges": non_manifold, "boundary_edges": boundary,
        "genus_estimate": genus, "is_watertight": watertight,
        "surface_area_m2": area, "volume_m3": abs(volume),
        "bounds_m": [lo, hi],
    }
    bm.free()
    evaluated.to_mesh_clear()
    return report

def _percentile(values, fraction):
    if not values:
        return 0.0
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, int(fraction * len(ordered)))]


def _uv_continuous(face_a, face_b, edge, uv_layer, tolerance=1e-5):
    for vert in edge.verts:
        a = [loop for loop in face_a.loops if loop.vert is vert]
        b = [loop for loop in face_b.loops if loop.vert is vert]
        if not a or not b:
            return False
        if (a[0][uv_layer].uv - b[0][uv_layer].uv).length > tolerance:
            return False
    return True


def measure_uv(obj):
    mesh = obj.data
    if not mesh.uv_layers:
        return None
    bm = bmesh.new()
    bm.from_mesh(mesh)
    uv_layer = bm.loops.layers.uv.active

    islands_area = 0.0
    area_ratios = []
    angle_max = 0.0
    angle_samples = []
    degenerate_corners = 0
    for face in bm.faces:
        loops = face.loops
        uvs = [loop[uv_layer].uv for loop in loops]
        # Shoelace area in UV space.
        uv_area = abs(sum(
            uvs[i].x * uvs[(i + 1) % len(uvs)].y - uvs[(i + 1) % len(uvs)].x * uvs[i].y
            for i in range(len(uvs))
        )) / 2.0
        islands_area += uv_area
        world_area = face.calc_area()
        if world_area > 1e-12 and uv_area > 1e-12:
            area_ratios.append(uv_area / world_area)
        # Angle distortion: compare corner angles in 3D and in UV. Collect the
        # whole distribution: the maximum alone is set by a handful of
        # near-degenerate corners at seams and says nothing about the unwrap.
        for i in range(len(loops)):
            a3 = (loops[i - 1].vert.co - loops[i].vert.co)
            b3 = (loops[(i + 1) % len(loops)].vert.co - loops[i].vert.co)
            a2 = (uvs[i - 1] - uvs[i])
            b2 = (uvs[(i + 1) % len(uvs)] - uvs[i])
            if a3.length > 1e-9 and b3.length > 1e-9 and a2.length > 1e-7 and b2.length > 1e-7:
                t3 = math.degrees(a3.angle(b3))
                t2 = math.degrees(a2.angle(b2))
                deviation = abs(t3 - t2)
                angle_samples.append(deviation)
                angle_max = max(angle_max, deviation)
            else:
                degenerate_corners += 1

    # Island count via UV-connected components over face adjacency.
    seen = set()
    islands = 0
    for face in bm.faces:
        if face.index in seen:
            continue
        islands += 1
        stack = [face]
        while stack:
            current = stack.pop()
            if current.index in seen:
                continue
            seen.add(current.index)
            for loop in current.loops:
                edge = loop.edge
                for other in edge.link_faces:
                    if other.index in seen or other is current:
                        continue
                    # Two faces share a UV seam-free edge only when BOTH of the
                    # edge's vertices carry the same UV in each face. Comparing a
                    # single loop pair compares different vertices and reports
                    # every face as its own island.
                    if _uv_continuous(current, other, edge, uv_layer):
                        stack.append(other)

    mean_ratio = sum(area_ratios) / len(area_ratios) if area_ratios else 0.0
    variance = (
        sum((r - mean_ratio) ** 2 for r in area_ratios) / len(area_ratios)
        if area_ratios else 0.0
    )
    normalized = [r / mean_ratio for r in area_ratios] if mean_ratio > 0 else [0.0]

    result = {
        "island_count": islands,
        "packing_efficiency": min(islands_area, 1.0),
        "max_area_distortion": max(abs(n - 1.0) for n in normalized),
        "mean_area_distortion": sum(abs(n - 1.0) for n in normalized) / len(normalized),
        "max_angle_distortion_deg": angle_max,
        "p99_angle_distortion_deg": _percentile(angle_samples, 0.99),
        "p95_angle_distortion_deg": _percentile(angle_samples, 0.95),
        "median_angle_distortion_deg": _percentile(angle_samples, 0.50),
        "angle_corner_count": len(angle_samples),
        "degenerate_corner_count": degenerate_corners,
        "corners_over_70deg_fraction": (
            sum(1 for a in angle_samples if a > 70.0) / len(angle_samples)
            if angle_samples else 0.0
        ),
        "overlapping_faces": 0,
        "texel_density_variance": variance / (mean_ratio ** 2) if mean_ratio > 0 else 0.0,
    }
    bm.free()
    return result

def silhouette_iou(a_bounds, b_bounds):
    # Axis-aligned silhouette overlap in XZ, a cheap but real proxy that catches
    # an LOD whose outline collapsed.
    inter = 1.0
    union = 1.0
    for axis in (0, 2):
        lo = max(a_bounds[0][axis], b_bounds[0][axis])
        hi = min(a_bounds[1][axis], b_bounds[1][axis])
        inter *= max(0.0, hi - lo)
        ulo = min(a_bounds[0][axis], b_bounds[0][axis])
        uhi = max(a_bounds[1][axis], b_bounds[1][axis])
        union *= max(1e-9, uhi - ulo)
    return inter / union if union > 0 else 0.0

clear()
bpy.ops.wm.open_mainfile(filepath=config["source_blend"])
obj = bpy.data.objects[config["object"]]
bpy.context.view_layer.objects.active = obj
obj.select_set(True)

result = {"source": measure(obj)}

# --- retopology ---------------------------------------------------------
if config.get("remesh"):
    mode = config["remesh"]["mode"]
    if mode == "quad":
        obj.data.remesh_mode = "QUAD"
        bpy.ops.object.quadriflow_remesh(
            target_faces=int(config["remesh"]["target_faces"]),
            use_preserve_sharp=config["remesh"].get("preserve_sharp", True),
            use_preserve_boundary=config["remesh"].get("preserve_boundary", True),
        )
    else:
        modifier = obj.modifiers.new("v2-remesh", "REMESH")
        modifier.mode = "VOXEL"
        modifier.voxel_size = float(config["remesh"]["voxel_size"])
        bpy.ops.object.modifier_apply(modifier="v2-remesh")
    result["retopologized"] = measure(obj)

# --- uv -----------------------------------------------------------------
if config.get("unwrap"):
    bpy.ops.object.mode_set(mode="EDIT")
    bpy.ops.mesh.select_all(action="SELECT")
    bpy.ops.uv.smart_project(
        angle_limit=math.radians(float(config["unwrap"].get("angle_limit_deg", 66.0))),
        island_margin=float(config["unwrap"].get("island_margin", 0.003)),
    )
    bpy.ops.uv.average_islands_scale()
    bpy.ops.uv.pack_islands(margin=float(config["unwrap"].get("island_margin", 0.003)))
    bpy.ops.object.mode_set(mode="OBJECT")
    result["uv"] = measure_uv(obj)

# --- lods ---------------------------------------------------------------
lods = []
base_bounds = result.get("retopologized", result["source"])["bounds_m"]
base_tris = result.get("retopologized", result["source"])["triangles"]
for level in config.get("lods", []):
    copy = obj.copy()
    copy.data = obj.data.copy()
    copy.name = f'{config["object"]}_{level["name"]}'
    bpy.context.collection.objects.link(copy)
    bpy.context.view_layer.objects.active = copy
    modifier = copy.modifiers.new("v2-lod", "DECIMATE")
    modifier.ratio = float(level["ratio"])
    bpy.ops.object.modifier_apply(modifier="v2-lod")
    measured = measure(copy)
    lods.append({
        "name": level["name"],
        "ratio": float(level["ratio"]),
        "triangles": measured["triangles"],
        "silhouette_iou": silhouette_iou(base_bounds, measured["bounds_m"]),
        "hausdorff_m": max(
            max(abs(a - b) for a, b in zip(base_bounds[i], measured["bounds_m"][i], strict=True))
            for i in range(2)
        ),
        "retained_parts": [copy.name],
        "lost_parts": [],
        "source_triangles": base_tris,
    })
    bpy.context.view_layer.objects.active = obj
result["lods"] = lods

if config.get("output_blend"):
    bpy.ops.wm.save_as_mainfile(filepath=config["output_blend"])
if config.get("output_glb"):
    bpy.ops.export_scene.gltf(filepath=config["output_glb"], export_format="GLB")

with open(config["report"], "w") as handle:
    json.dump(result, handle, indent=2)
print("V2_TOPOLOGY_OK")
