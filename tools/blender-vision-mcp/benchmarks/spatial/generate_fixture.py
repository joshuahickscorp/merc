"""Blender headless fixture generator for the spatial evidence lane.

Creates a checkerboard of known square size, a simple metric target box,
renders >= 12 RGB views plus Z-depth from known poses, and writes a manifest
of ground-truth intrinsics, poses, and geometry.
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import bpy
import mathutils

BOARD_COLS = 8  # inner corners X
BOARD_ROWS = 6  # inner corners Y
SQUARE_M = 0.025  # 25 mm squares
IMAGE_W = 640
IMAGE_H = 480
FOCAL_MM = 35.0
SENSOR_W_MM = 36.0
N_VIEWS = 14


def clear_scene() -> None:
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for collection in (bpy.data.materials, bpy.data.cameras, bpy.data.lights, bpy.data.meshes):
        for item in list(collection):
            collection.remove(item)


def make_material(name: str, color: tuple[float, float, float, float]) -> bpy.types.Material:
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    nodes = mat.node_tree.nodes
    nodes.clear()
    out = nodes.new("ShaderNodeOutputMaterial")
    bsdf = nodes.new("ShaderNodeBsdfPrincipled")
    bsdf.inputs["Base Color"].default_value = color
    bsdf.inputs["Roughness"].default_value = 0.45
    mat.node_tree.links.new(bsdf.outputs["BSDF"], out.inputs["Surface"])
    return mat


def build_checkerboard() -> dict:
    """Build a flat checkerboard on the XY plane (Z up), squares of SQUARE_M.

    Board extends from origin: (cols+1) by (rows+1) squares of size SQUARE_M.
    Inner corners are at (i*SQUARE_M, j*SQUARE_M, 0) for i in 0..cols-1 etc.
    OpenCV expects columns x rows of *inner* corners.
    """
    n_sq_x = BOARD_COLS + 1
    n_sq_y = BOARD_ROWS + 1
    white = make_material("board-white", (0.92, 0.92, 0.92, 1.0))
    black = make_material("board-black", (0.05, 0.05, 0.05, 1.0))
    for iy in range(n_sq_y):
        for ix in range(n_sq_x):
            bpy.ops.mesh.primitive_plane_add(size=SQUARE_M)
            square = bpy.context.object
            square.name = f"sq-{ix}-{iy}"
            square.location = (
                (ix + 0.5) * SQUARE_M,
                (iy + 0.5) * SQUARE_M,
                0.0,
            )
            mat = black if (ix + iy) % 2 == 0 else white
            square.data.materials.append(mat)
    # Raised metric box for depth / coverage (sits above the board centre).
    board_w = n_sq_x * SQUARE_M
    board_h = n_sq_y * SQUARE_M
    bpy.ops.mesh.primitive_cube_add(size=1.0)
    box = bpy.context.object
    box.name = "metric-box"
    box.scale = (0.06, 0.04, 0.03)
    box.location = (board_w * 0.5, board_h * 0.5, 0.03)
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    box.data.materials.append(make_material("box-alu", (0.3, 0.35, 0.4, 1.0)))
    return {
        "board_cols": BOARD_COLS,
        "board_rows": BOARD_ROWS,
        "square_m": SQUARE_M,
        "board_width_m": board_w,
        "board_height_m": board_h,
        "box_size_m": [0.12, 0.08, 0.06],
        "box_center_m": [board_w * 0.5, board_h * 0.5, 0.03],
        "box_bounds_min": [
            board_w * 0.5 - 0.06,
            board_h * 0.5 - 0.04,
            0.0,
        ],
        "box_bounds_max": [
            board_w * 0.5 + 0.06,
            board_h * 0.5 + 0.04,
            0.06,
        ],
    }


def configure_render(scene: bpy.types.Scene) -> dict:
    # Prefer WORKBENCH: no Metal path-tracing, fewer headless GPU hazards.
    try:
        scene.render.engine = "BLENDER_WORKBENCH"
    except Exception:
        scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.render.resolution_x = IMAGE_W
    scene.render.resolution_y = IMAGE_H
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGB"
    scene.render.film_transparent = False
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.scale_length = 1.0
    scene.use_nodes = True
    scene.view_layers["ViewLayer"].use_pass_z = True
    fx = FOCAL_MM / SENSOR_W_MM * IMAGE_W
    fy = fx
    cx = IMAGE_W / 2.0
    cy = IMAGE_H / 2.0
    return {
        "width": IMAGE_W,
        "height": IMAGE_H,
        "focal_mm": FOCAL_MM,
        "sensor_width_mm": SENSOR_W_MM,
        "intrinsics": {"fx": fx, "fy": fy, "cx": cx, "cy": cy},
    }


def add_camera(name: str, location, target, sensor: dict) -> bpy.types.Object:
    cam_data = bpy.data.cameras.new(name)
    cam_data.lens = FOCAL_MM
    cam_data.sensor_width = SENSOR_W_MM
    cam_data.sensor_fit = "HORIZONTAL"
    cam_obj = bpy.data.objects.new(name, cam_data)
    bpy.context.scene.collection.objects.link(cam_obj)
    cam_obj.location = location
    direction = mathutils.Vector(target) - mathutils.Vector(location)
    cam_obj.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
    return cam_obj


def view_poses(board: dict) -> list[dict]:
    cx, cy, cz = board["box_center_m"]
    target = (cx, cy, cz)
    poses = []
    # Upper hemisphere orbit + a few lower/side views that still see the board.
    for index in range(N_VIEWS):
        theta = 2.0 * math.pi * index / N_VIEWS
        elev = 0.35 + 0.25 * math.sin(theta * 2)
        radius = 0.55
        x = cx + radius * math.cos(theta) * math.cos(elev)
        y = cy + radius * math.sin(theta) * math.cos(elev)
        z = cz + radius * math.sin(elev) + 0.12
        poses.append(
            {
                "label": f"view_{index:02d}",
                "location": [x, y, z],
                "target": list(target),
                "timestamp": float(index),
            }
        )
    return poses


def matrix_world_list(obj: bpy.types.Object) -> list[list[float]]:
    m = obj.matrix_world
    return [[float(m[row][col]) for col in range(4)] for row in range(4)]


def setup_compositor_depth(scene: bpy.types.Scene, depth_path: Path) -> None:
    tree = scene.node_tree
    tree.nodes.clear()
    rl = tree.nodes.new("CompositorNodeRLayers")
    depth_out = tree.nodes.new("CompositorNodeOutputFile")
    depth_out.base_path = str(depth_path.parent)
    depth_out.format.file_format = "OPEN_EXR"
    depth_out.format.color_mode = "BW"
    depth_out.file_slots[0].path = depth_path.stem + "_"
    tree.links.new(rl.outputs["Depth"], depth_out.inputs[0])
    # Also need composite for RGB.
    comp = tree.nodes.new("CompositorNodeComposite")
    tree.links.new(rl.outputs["Image"], comp.inputs["Image"])


def render_views(out_dir: Path, board: dict, sensor: dict) -> list[dict]:
    scene = bpy.context.scene
    poses = view_poses(board)
    records = []
    rgb_dir = out_dir / "rgb"
    depth_dir = out_dir / "depth"
    rgb_dir.mkdir(parents=True, exist_ok=True)
    depth_dir.mkdir(parents=True, exist_ok=True)

    # Light
    light_data = bpy.data.lights.new(name="key", type="AREA")
    light_data.energy = 80.0
    light_data.size = 1.0
    light = bpy.data.objects.new("key", light_data)
    scene.collection.objects.link(light)
    light.location = (board["board_width_m"] * 0.5, -0.4, 0.8)

    for pose in poses:
        cam = add_camera(pose["label"], pose["location"], pose["target"], sensor)
        scene.camera = cam
        rgb_path = rgb_dir / f"{pose['label']}.png"
        depth_exr = depth_dir / f"{pose['label']}_depth"
        setup_compositor_depth(scene, depth_exr)
        scene.render.filepath = str(rgb_path)
        bpy.ops.render.render(write_still=True)
        # Blender appends frame number to file output nodes.
        # Find the written EXR.
        candidates = sorted(depth_dir.glob(f"{pose['label']}_depth*"))
        exr_file = None
        for candidate in candidates:
            if candidate.suffix.lower() in {".exr", ".png"}:
                exr_file = candidate
                break
        records.append(
            {
                **pose,
                "rgb": str(rgb_path.relative_to(out_dir)),
                "depth_exr": str(exr_file.relative_to(out_dir)) if exr_file else None,
                "world_from_camera": matrix_world_list(cam),
                "intrinsics": sensor["intrinsics"],
                "width": sensor["width"],
                "height": sensor["height"],
            }
        )
        # Remove camera to keep scene tidy.
        bpy.data.objects.remove(cam, do_unlink=True)
        if cam.data:
            bpy.data.cameras.remove(cam.data)
    return records


def export_box_mesh(out_dir: Path) -> str:
    box = bpy.data.objects.get("metric-box")
    if box is None:
        return ""
    # Sample surface points of the box for chamfer GT (corners + face centres).
    pts = []
    dims = list(box.dimensions)
    cx, cy, cz = box.location
    hx, hy, hz = dims[0] / 2, dims[1] / 2, dims[2] / 2
    for sx in (-1, 0, 1):
        for sy in (-1, 0, 1):
            for sz in (-1, 0, 1):
                if sx == 0 and sy == 0 and sz == 0:
                    continue
                # Face/edge/corner samples on the surface.
                if abs(sx) + abs(sy) + abs(sz) >= 1:
                    pts.append([cx + sx * hx, cy + sy * hy, cz + sz * hz])
    # Dense face grid.
    for face_axis, sign in [(0, -1), (0, 1), (1, -1), (1, 1), (2, -1), (2, 1)]:
        for u in range(5):
            for v in range(5):
                p = [cx, cy, cz]
                others = [i for i in range(3) if i != face_axis]
                half = [hx, hy, hz]
                p[face_axis] = [cx, cy, cz][face_axis] + sign * half[face_axis]
                p[others[0]] = [cx, cy, cz][others[0]] + (u / 4 - 0.5) * 2 * half[others[0]]
                p[others[1]] = [cx, cy, cz][others[1]] + (v / 4 - 0.5) * 2 * half[others[1]]
                pts.append(p)
    path = out_dir / "box_surface_points.json"
    path.write_text(json.dumps(pts), encoding="utf-8")
    return str(path.relative_to(out_dir))


def main() -> None:
    out_dir = Path(sys.argv[sys.argv.index("--") + 1]).resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    clear_scene()
    board = build_checkerboard()
    sensor = configure_render(bpy.context.scene)
    views = render_views(out_dir, board, sensor)
    surface = export_box_mesh(out_dir)
    manifest = {
        "board": board,
        "sensor": sensor,
        "views": views,
        "box_surface_points": surface,
        "generator": "benchmarks/spatial/generate_fixture.py",
        "blender_version": bpy.app.version_string,
    }
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"fixture written to {out_dir} views={len(views)}")


if __name__ == "__main__":
    main()
