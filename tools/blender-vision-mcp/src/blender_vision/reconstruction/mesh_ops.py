"""Mesh and point-cloud utilities used by reconstruction backends."""

from __future__ import annotations

import struct
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.reconstruction.base import MeshGeometry, PointCloud


def write_ply_mesh(path: Path, mesh: MeshGeometry) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    vertices = np.asarray(mesh.vertices, dtype=np.float64)
    faces = np.asarray(mesh.faces, dtype=np.int64)
    lines = [
        "ply",
        "format ascii 1.0",
        "comment VisionMCP reconstruction mesh",
        f"element vertex {len(vertices)}",
        "property float x",
        "property float y",
        "property float z",
        f"element face {len(faces)}",
        "property list uchar int vertex_indices",
        "end_header",
    ]
    for x, y, z in vertices:
        lines.append(f"{x:.9g} {y:.9g} {z:.9g}")
    for a, b, c in faces:
        lines.append(f"3 {int(a)} {int(b)} {int(c)}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


def write_ply_points(path: Path, cloud: PointCloud) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    positions = np.asarray(cloud.positions, dtype=np.float64)
    colours = (
        np.asarray(cloud.colours, dtype=np.float64)
        if cloud.colours is not None
        else np.full((len(positions), 3), 0.7)
    )
    if colours.max() <= 1.0:
        colours = (colours * 255.0).clip(0, 255)
    lines = [
        "ply",
        "format ascii 1.0",
        "comment VisionMCP oriented point archive (not a trained radiance field)",
        f"element vertex {len(positions)}",
        "property float x",
        "property float y",
        "property float z",
        "property uchar red",
        "property uchar green",
        "property uchar blue",
        "end_header",
    ]
    for (x, y, z), (r, g, b) in zip(positions, colours, strict=True):
        lines.append(f"{x:.9g} {y:.9g} {z:.9g} {int(r)} {int(g)} {int(b)}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


def read_ply_mesh(path: Path) -> MeshGeometry:
    text = path.read_text(encoding="utf-8")
    lines = text.splitlines()
    header_end = lines.index("end_header")
    vertex_count = 0
    face_count = 0
    for line in lines[:header_end]:
        if line.startswith("element vertex"):
            vertex_count = int(line.split()[-1])
        elif line.startswith("element face"):
            face_count = int(line.split()[-1])
    body = lines[header_end + 1 :]
    vertices = np.array(
        [[float(v) for v in row.split()[:3]] for row in body[:vertex_count]],
        dtype=np.float64,
    )
    faces = []
    for row in body[vertex_count : vertex_count + face_count]:
        parts = row.split()
        count = int(parts[0])
        if count < 3:
            continue
        indices = [int(v) for v in parts[1 : 1 + count]]
        for i in range(1, count - 1):
            faces.append((indices[0], indices[i], indices[i + 1]))
    return MeshGeometry(vertices=vertices, faces=np.array(faces, dtype=np.int64))


def triangle_normals(mesh: MeshGeometry) -> np.ndarray:
    vertices = mesh.vertices
    faces = mesh.faces
    v0 = vertices[faces[:, 0]]
    v1 = vertices[faces[:, 1]]
    v2 = vertices[faces[:, 2]]
    normals = np.cross(v1 - v0, v2 - v0)
    lengths = np.linalg.norm(normals, axis=1, keepdims=True)
    lengths = np.maximum(lengths, 1e-12)
    return normals / lengths


def surface_area(mesh: MeshGeometry) -> float:
    if mesh.is_empty():
        return 0.0
    vertices = mesh.vertices
    faces = mesh.faces
    v0 = vertices[faces[:, 0]]
    v1 = vertices[faces[:, 1]]
    v2 = vertices[faces[:, 2]]
    cross = np.cross(v1 - v0, v2 - v0)
    return float(0.5 * np.linalg.norm(cross, axis=1).sum())


def volume(mesh: MeshGeometry) -> float:
    """Signed volume of a closed triangular mesh (divergence theorem)."""
    if mesh.is_empty():
        return 0.0
    vertices = mesh.vertices
    faces = mesh.faces
    v0 = vertices[faces[:, 0]]
    v1 = vertices[faces[:, 1]]
    v2 = vertices[faces[:, 2]]
    return float(np.abs(np.einsum("ij,ij->i", v0, np.cross(v1, v2)).sum()) / 6.0)


def bounding_box(mesh: MeshGeometry) -> tuple[np.ndarray, np.ndarray]:
    return mesh.vertices.min(axis=0), mesh.vertices.max(axis=0)


def sample_surface_points(mesh: MeshGeometry, count: int, *, seed: int = 0) -> np.ndarray:
    if mesh.is_empty() or count <= 0:
        return np.zeros((0, 3), dtype=np.float64)
    vertices = mesh.vertices
    faces = mesh.faces
    v0 = vertices[faces[:, 0]]
    v1 = vertices[faces[:, 1]]
    v2 = vertices[faces[:, 2]]
    areas = 0.5 * np.linalg.norm(np.cross(v1 - v0, v2 - v0), axis=1)
    total = float(areas.sum())
    if total <= 0:
        return vertices.copy()[: min(count, len(vertices))]
    probs = areas / total
    rng = np.random.default_rng(seed)
    face_idx = rng.choice(len(faces), size=count, p=probs)
    r1 = np.sqrt(rng.random(count))
    r2 = rng.random(count)
    a = 1.0 - r1
    b = r1 * (1.0 - r2)
    c = r1 * r2
    return a[:, None] * v0[face_idx] + b[:, None] * v1[face_idx] + c[:, None] * v2[face_idx]


def chamfer_distance(
    mesh_a: MeshGeometry,
    mesh_b: MeshGeometry,
    *,
    samples: int = 2000,
    seed: int = 0,
) -> dict[str, float]:
    """Symmetric mean nearest-neighbour distance between surface samples."""
    pts_a = sample_surface_points(mesh_a, samples, seed=seed)
    pts_b = sample_surface_points(mesh_b, samples, seed=seed + 1)
    if len(pts_a) == 0 or len(pts_b) == 0:
        return {"chamfer": float("inf"), "a_to_b": float("inf"), "b_to_a": float("inf")}
    d_ab = _mean_nn(pts_a, pts_b)
    d_ba = _mean_nn(pts_b, pts_a)
    return {"chamfer": 0.5 * (d_ab + d_ba), "a_to_b": d_ab, "b_to_a": d_ba}


def _mean_nn(source: np.ndarray, target: np.ndarray) -> float:
    # Chunked to keep memory bounded for larger sample sets.
    chunk = 512
    totals = []
    for start in range(0, len(source), chunk):
        block = source[start : start + chunk]
        diff = block[:, None, :] - target[None, :, :]
        dist = np.sqrt(np.maximum((diff * diff).sum(axis=2), 0.0))
        totals.append(dist.min(axis=1))
    return float(np.concatenate(totals).mean())


def topology_report(mesh: MeshGeometry) -> dict[str, Any]:
    """Estimate manifold/watertight properties without requiring external tools."""
    if mesh.is_empty():
        return {
            "vertex_count": 0,
            "face_count": 0,
            "edge_count": 0,
            "boundary_edge_count": 0,
            "non_manifold_edge_count": 0,
            "manifold": False,
            "watertight": False,
            "genus_estimate": None,
            "surface_area": 0.0,
            "volume": 0.0,
        }
    faces = np.asarray(mesh.faces, dtype=np.int64)
    edge_counts: dict[tuple[int, int], int] = {}
    for a, b, c in faces:
        for u, v in ((int(a), int(b)), (int(b), int(c)), (int(c), int(a))):
            key = (u, v) if u < v else (v, u)
            edge_counts[key] = edge_counts.get(key, 0) + 1
    boundary = sum(1 for count in edge_counts.values() if count == 1)
    non_manifold = sum(1 for count in edge_counts.values() if count > 2)
    edge_count = len(edge_counts)
    vertex_count = int(mesh.vertices.shape[0])
    face_count = int(faces.shape[0])
    # Euler characteristic for closed orientable surfaces: V - E + F = 2 - 2g
    chi = vertex_count - edge_count + face_count
    genus: int | None
    genus = max(0, (2 - chi) // 2) if boundary == 0 and non_manifold == 0 and chi % 2 == 0 else None
    manifold = non_manifold == 0
    watertight = manifold and boundary == 0
    return {
        "vertex_count": vertex_count,
        "face_count": face_count,
        "edge_count": edge_count,
        "boundary_edge_count": boundary,
        "non_manifold_edge_count": non_manifold,
        "manifold": manifold,
        "watertight": watertight,
        "genus_estimate": genus,
        "euler_characteristic": chi,
        "surface_area": surface_area(mesh),
        "volume": volume(mesh),
    }


def box_mesh(
    minimum: np.ndarray | list[float],
    maximum: np.ndarray | list[float],
) -> MeshGeometry:
    minimum = np.asarray(minimum, dtype=np.float64)
    maximum = np.asarray(maximum, dtype=np.float64)
    x0, y0, z0 = minimum
    x1, y1, z1 = maximum
    vertices = np.array(
        [
            [x0, y0, z0],
            [x1, y0, z0],
            [x1, y1, z0],
            [x0, y1, z0],
            [x0, y0, z1],
            [x1, y0, z1],
            [x1, y1, z1],
            [x0, y1, z1],
        ],
        dtype=np.float64,
    )
    faces = np.array(
        [
            [0, 1, 2],
            [0, 2, 3],
            [4, 6, 5],
            [4, 7, 6],
            [0, 4, 5],
            [0, 5, 1],
            [1, 5, 6],
            [1, 6, 2],
            [2, 6, 7],
            [2, 7, 3],
            [3, 7, 4],
            [3, 4, 0],
        ],
        dtype=np.int64,
    )
    return MeshGeometry(vertices=vertices, faces=faces)


def sphere_mesh(
    center: np.ndarray | list[float],
    radius: float,
    subdivisions: int = 2,
) -> MeshGeometry:
    """Unit icosphere scaled to radius (pure numpy, no external mesh deps required)."""
    # Start from regular icosahedron.
    t = (1.0 + np.sqrt(5.0)) / 2.0
    verts = np.array(
        [
            [-1, t, 0],
            [1, t, 0],
            [-1, -t, 0],
            [1, -t, 0],
            [0, -1, t],
            [0, 1, t],
            [0, -1, -t],
            [0, 1, -t],
            [t, 0, -1],
            [t, 0, 1],
            [-t, 0, -1],
            [-t, 0, 1],
        ],
        dtype=np.float64,
    )
    faces = np.array(
        [
            [0, 11, 5],
            [0, 5, 1],
            [0, 1, 7],
            [0, 7, 10],
            [0, 10, 11],
            [1, 5, 9],
            [5, 11, 4],
            [11, 10, 2],
            [10, 7, 6],
            [7, 1, 8],
            [3, 9, 4],
            [3, 4, 2],
            [3, 2, 6],
            [3, 6, 8],
            [3, 8, 9],
            [4, 9, 5],
            [2, 4, 11],
            [6, 2, 10],
            [8, 6, 7],
            [9, 8, 1],
        ],
        dtype=np.int64,
    )
    verts = verts / np.linalg.norm(verts, axis=1, keepdims=True)
    for _ in range(subdivisions):
        verts, faces = _subdivide_sphere(verts, faces)
    center = np.asarray(center, dtype=np.float64)
    verts = verts * float(radius) + center
    return MeshGeometry(vertices=verts, faces=faces)


def _subdivide_sphere(vertices: np.ndarray, faces: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    midpoint_cache: dict[tuple[int, int], int] = {}
    new_faces: list[list[int]] = []
    verts = list(vertices)

    def midpoint(i: int, j: int) -> int:
        key = (i, j) if i < j else (j, i)
        if key in midpoint_cache:
            return midpoint_cache[key]
        point = (np.asarray(verts[i]) + np.asarray(verts[j])) * 0.5
        point = point / np.linalg.norm(point)
        midpoint_cache[key] = len(verts)
        verts.append(point)
        return midpoint_cache[key]

    for a, b, c in faces:
        ab = midpoint(int(a), int(b))
        bc = midpoint(int(b), int(c))
        ca = midpoint(int(c), int(a))
        new_faces.extend(
            [
                [int(a), ab, ca],
                [int(b), bc, ab],
                [int(c), ca, bc],
                [ab, bc, ca],
            ]
        )
    return np.asarray(verts, dtype=np.float64), np.asarray(new_faces, dtype=np.int64)


def load_mesh_artifact(path: Path) -> MeshGeometry | None:
    if not path.is_file():
        return None
    suffix = path.suffix.lower()
    if suffix == ".ply":
        return read_ply_mesh(path)
    if suffix == ".obj":
        return read_wavefront_obj(path)
    return None


def read_wavefront_obj(path: Path) -> MeshGeometry:
    vertices: list[list[float]] = []
    faces: list[list[int]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        parts = line.split()
        if parts[0] == "v" and len(parts) >= 4:
            vertices.append([float(parts[1]), float(parts[2]), float(parts[3])])
        elif parts[0] == "f" and len(parts) >= 4:
            idx = [int(p.split("/")[0]) - 1 for p in parts[1:]]
            for i in range(1, len(idx) - 1):
                faces.append([idx[0], idx[i], idx[i + 1]])
    return MeshGeometry(
        vertices=np.asarray(vertices, dtype=np.float64),
        faces=np.asarray(faces, dtype=np.int64),
    )


def write_obj_mesh(path: Path, mesh: MeshGeometry) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    lines = ["# VisionMCP reconstruction mesh"]
    for x, y, z in mesh.vertices:
        lines.append(f"v {x:.9g} {y:.9g} {z:.9g}")
    for a, b, c in mesh.faces:
        lines.append(f"f {int(a) + 1} {int(b) + 1} {int(c) + 1}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


# Oriented-point binary archive: not a NeRF, not a trained Gaussian splat.
SPLAT_MAGIC = b"BVMCPSPLT"
SPLAT_VERSION = 1
SPLAT_DTYPE = np.dtype(
    [
        ("x", "<f4"),
        ("y", "<f4"),
        ("z", "<f4"),
        ("nx", "<f4"),
        ("ny", "<f4"),
        ("nz", "<f4"),
        ("r", "<f4"),
        ("g", "<f4"),
        ("b", "<f4"),
        ("radius", "<f4"),
        ("confidence", "<f4"),
    ]
)


def write_oriented_points(path: Path, cloud: PointCloud) -> Path:
    """Write the VisionMCP oriented-point archive (.osplat).

    This is a point-based representation with position, normal, colour, radius
    and confidence. It is explicitly not a trained radiance field or Gaussian
    splat model.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    n = len(cloud.positions)
    records = np.zeros(n, dtype=SPLAT_DTYPE)
    positions = np.asarray(cloud.positions, dtype=np.float32)
    records["x"] = positions[:, 0]
    records["y"] = positions[:, 1]
    records["z"] = positions[:, 2]
    if cloud.normals is not None:
        normals = np.asarray(cloud.normals, dtype=np.float32)
        records["nx"] = normals[:, 0]
        records["ny"] = normals[:, 1]
        records["nz"] = normals[:, 2]
    if cloud.colours is not None:
        colours = np.asarray(cloud.colours, dtype=np.float32)
        if colours.max() > 1.0:
            colours = colours / 255.0
        records["r"] = colours[:, 0]
        records["g"] = colours[:, 1]
        records["b"] = colours[:, 2]
    else:
        records["r"] = 0.7
        records["g"] = 0.7
        records["b"] = 0.7
    if cloud.radii is not None:
        records["radius"] = np.asarray(cloud.radii, dtype=np.float32)
    else:
        records["radius"] = 0.001
    if cloud.confidence is not None:
        records["confidence"] = np.asarray(cloud.confidence, dtype=np.float32)
    else:
        records["confidence"] = 1.0
    with path.open("wb") as stream:
        stream.write(SPLAT_MAGIC)
        stream.write(struct.pack("<I", SPLAT_VERSION))
        stream.write(struct.pack("<Q", n))
        # Representation contract: not NeRF / not trained Gaussian splat.
        note = b"oriented-point-archive;not-nerf;not-trained-gaussian-splat"
        stream.write(struct.pack("<H", len(note)))
        stream.write(note)
        stream.write(records.tobytes())
    return path


def read_oriented_points(path: Path) -> PointCloud:
    with path.open("rb") as stream:
        magic = stream.read(len(SPLAT_MAGIC))
        if magic != SPLAT_MAGIC:
            raise ValueError(f"not an oriented-point archive: {path}")
        (version,) = struct.unpack("<I", stream.read(4))
        if version != SPLAT_VERSION:
            raise ValueError(f"unsupported oriented-point version {version}")
        (count,) = struct.unpack("<Q", stream.read(8))
        (note_len,) = struct.unpack("<H", stream.read(2))
        stream.read(note_len)
        records = np.frombuffer(stream.read(count * SPLAT_DTYPE.itemsize), dtype=SPLAT_DTYPE)
    positions = np.stack([records["x"], records["y"], records["z"]], axis=1).astype(np.float64)
    normals = np.stack([records["nx"], records["ny"], records["nz"]], axis=1).astype(np.float64)
    colours = np.stack([records["r"], records["g"], records["b"]], axis=1).astype(np.float64)
    return PointCloud(
        positions=positions,
        normals=normals,
        colours=colours,
        radii=records["radius"].astype(np.float64),
        confidence=records["confidence"].astype(np.float64),
    )
