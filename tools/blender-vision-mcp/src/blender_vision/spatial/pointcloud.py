"""Point-cloud container with PLY I/O, alignment, and geometric queries."""

from __future__ import annotations

import struct
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, BinaryIO

import numpy as np
from scipy.spatial import cKDTree

from blender_vision.core.errors import ValidationError
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    derive,
)
from blender_vision.v2.records import Lineage, ObservationBundle


@dataclass(slots=True)
class SimilarityTransform:
    """Umeyama similarity: p' = scale * R @ p + t."""

    rotation: np.ndarray
    translation: np.ndarray
    scale: float
    rmse: float

    def apply(self, points: np.ndarray) -> np.ndarray:
        array = np.asarray(points, dtype=np.float64)
        return (self.scale * (array @ self.rotation.T)) + self.translation

    def matrix(self) -> np.ndarray:
        result = np.eye(4, dtype=np.float64)
        result[:3, :3] = self.scale * self.rotation
        result[:3, 3] = self.translation
        return result

    def to_dict(self) -> dict[str, Any]:
        return {
            "rotation": self.rotation.tolist(),
            "translation": self.translation.tolist(),
            "scale": float(self.scale),
            "rmse": float(self.rmse),
        }


@dataclass(slots=True)
class PointCloud:
    """Nx3 positions with optional normals, colours, confidence, and authority."""

    positions: np.ndarray
    normals: np.ndarray | None = None
    colors: np.ndarray | None = None
    confidence: np.ndarray | None = None
    frame: CoordinateFrame = field(default_factory=lambda: BLENDER_WORLD)
    authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED
    source_path: str | None = None
    notes: list[str] = field(default_factory=list)

    def __post_init__(self) -> None:
        self.positions = np.asarray(self.positions, dtype=np.float64)
        if self.positions.ndim != 2 or self.positions.shape[1] != 3:
            raise ValidationError("positions must have shape (N, 3)")
        n = self.positions.shape[0]
        if self.normals is not None:
            self.normals = np.asarray(self.normals, dtype=np.float64)
            if self.normals.shape != (n, 3):
                raise ValidationError("normals must have shape (N, 3)")
        if self.colors is not None:
            self.colors = np.asarray(self.colors, dtype=np.float64)
            if self.colors.shape != (n, 3):
                raise ValidationError("colors must have shape (N, 3)")
        if self.confidence is not None:
            self.confidence = np.asarray(self.confidence, dtype=np.float64)
            if self.confidence.shape != (n,):
                raise ValidationError("confidence must have shape (N,)")

    def __len__(self) -> int:
        return int(self.positions.shape[0])

    # ----------------------------------------------------------------- PLY

    def write_ply(self, path: Path | str, *, binary: bool = True) -> Path:
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        n = len(self)
        has_normals = self.normals is not None
        has_colors = self.colors is not None
        header_lines = [
            "ply",
            f"format {'binary_little_endian' if binary else 'ascii'} 1.0",
            f"element vertex {n}",
            "property float x",
            "property float y",
            "property float z",
        ]
        if has_normals:
            header_lines.extend(
                [
                    "property float nx",
                    "property float ny",
                    "property float nz",
                ]
            )
        if has_colors:
            header_lines.extend(
                [
                    "property uchar red",
                    "property uchar green",
                    "property uchar blue",
                ]
            )
        header_lines.append("end_header\n")
        header = "\n".join(header_lines).encode("ascii")

        if binary:
            with path.open("wb") as stream:
                stream.write(header)
                self._write_binary_vertices(stream, has_normals, has_colors)
        else:
            with path.open("wb") as stream:
                stream.write(header)
                for index in range(n):
                    parts = [
                        f"{self.positions[index, 0]:.8f}",
                        f"{self.positions[index, 1]:.8f}",
                        f"{self.positions[index, 2]:.8f}",
                    ]
                    if has_normals:
                        assert self.normals is not None
                        parts.extend(
                            f"{self.normals[index, axis]:.8f}" for axis in range(3)
                        )
                    if has_colors:
                        assert self.colors is not None
                        rgb = np.clip(np.round(self.colors[index] * 255.0), 0, 255)
                        parts.extend(str(int(channel)) for channel in rgb)
                    stream.write((" ".join(parts) + "\n").encode("ascii"))
        return path

    def _write_binary_vertices(
        self, stream: BinaryIO, has_normals: bool, has_colors: bool
    ) -> None:
        n = len(self)
        for index in range(n):
            stream.write(struct.pack("<fff", *self.positions[index]))
            if has_normals:
                assert self.normals is not None
                stream.write(struct.pack("<fff", *self.normals[index]))
            if has_colors:
                assert self.colors is not None
                rgb = np.clip(np.round(self.colors[index] * 255.0), 0, 255).astype(
                    np.uint8
                )
                stream.write(struct.pack("<BBB", int(rgb[0]), int(rgb[1]), int(rgb[2])))

    @classmethod
    def read_ply(
        cls,
        path: Path | str,
        *,
        frame: CoordinateFrame | None = None,
        authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED,
    ) -> PointCloud:
        path = Path(path)
        with path.open("rb") as stream:
            header_lines: list[str] = []
            while True:
                line = stream.readline()
                if not line:
                    raise ValidationError(f"truncated PLY header in {path}")
                text = line.decode("ascii", errors="replace").strip()
                header_lines.append(text)
                if text == "end_header":
                    break
            meta = _parse_ply_header(header_lines)
            if meta["format"] == "ascii":
                positions, normals, colors = _read_ascii_vertices(stream, meta)
            elif meta["format"] in {"binary_little_endian", "binary_big_endian"}:
                positions, normals, colors = _read_binary_vertices(stream, meta)
            else:
                raise ValidationError(f"unsupported PLY format: {meta['format']}")
        return cls(
            positions=positions,
            normals=normals,
            colors=colors,
            frame=frame or BLENDER_WORLD,
            authority=authority,
            source_path=str(path),
        )

    # -------------------------------------------------------------- geometry

    def bounds(self) -> dict[str, list[float]]:
        if len(self) == 0:
            return {"min": [0.0, 0.0, 0.0], "max": [0.0, 0.0, 0.0]}
        lo = self.positions.min(axis=0)
        hi = self.positions.max(axis=0)
        return {"min": lo.tolist(), "max": hi.tolist()}

    def transform(self, matrix: np.ndarray) -> PointCloud:
        """Apply a 3x3 or 4x4 transform. Authority is preserved (same evidence)."""
        matrix = np.asarray(matrix, dtype=np.float64)
        positions = self.positions.copy()
        normals = None if self.normals is None else self.normals.copy()
        if matrix.shape == (4, 4):
            rotation = matrix[:3, :3]
            translation = matrix[:3, 3]
            positions = positions @ rotation.T + translation
            if normals is not None:
                # Normals transform by inverse-transpose; for pure rotation R^{-T}=R.
                normals = normals @ np.linalg.inv(rotation).T
                norms = np.linalg.norm(normals, axis=1, keepdims=True)
                normals = normals / np.maximum(norms, 1e-12)
        elif matrix.shape == (3, 3):
            positions = positions @ matrix.T
            if normals is not None:
                normals = normals @ np.linalg.inv(matrix).T
                norms = np.linalg.norm(normals, axis=1, keepdims=True)
                normals = normals / np.maximum(norms, 1e-12)
        else:
            raise ValidationError("transform matrix must be 3x3 or 4x4")
        return PointCloud(
            positions=positions,
            normals=normals,
            colors=None if self.colors is None else self.colors.copy(),
            confidence=None if self.confidence is None else self.confidence.copy(),
            frame=self.frame,
            authority=self.authority,
            source_path=self.source_path,
            notes=[*self.notes, "transformed"],
        )

    def voxel_downsample(self, voxel_size: float) -> PointCloud:
        if voxel_size <= 0:
            raise ValidationError("voxel_size must be positive")
        if len(self) == 0:
            return PointCloud(
                positions=self.positions.copy(),
                frame=self.frame,
                authority=self.authority,
            )
        quantized = np.floor(self.positions / voxel_size).astype(np.int64)
        # Keep first point in each voxel for determinism.
        _, indices = np.unique(quantized, axis=0, return_index=True)
        indices = np.sort(indices)
        return PointCloud(
            positions=self.positions[indices],
            normals=None if self.normals is None else self.normals[indices],
            colors=None if self.colors is None else self.colors[indices],
            confidence=None if self.confidence is None else self.confidence[indices],
            frame=self.frame,
            authority=self.authority,
            source_path=self.source_path,
            notes=[*self.notes, f"voxel_downsample={voxel_size}"],
        )

    def estimate_normals(self, *, k: int = 16) -> PointCloud:
        """PCA normals over k-NN via cKDTree. Oriented toward the centroid."""
        if len(self) < 3:
            raise ValidationError("estimate_normals needs at least 3 points")
        k = min(k, len(self))
        tree = cKDTree(self.positions)
        _, neighbours = tree.query(self.positions, k=k)
        if k == 1:
            neighbours = neighbours[:, None]
        normals = np.zeros_like(self.positions)
        centroid = self.positions.mean(axis=0)
        for index in range(len(self)):
            pts = self.positions[neighbours[index]]
            centered = pts - pts.mean(axis=0)
            cov = centered.T @ centered / max(1, len(pts) - 1)
            eigenvalues, eigenvectors = np.linalg.eigh(cov)
            normal = eigenvectors[:, int(np.argmin(eigenvalues))]
            # Orient toward the cloud centroid so neighbouring normals agree roughly.
            if np.dot(normal, centroid - self.positions[index]) < 0:
                normal = -normal
            normals[index] = normal
        return PointCloud(
            positions=self.positions.copy(),
            normals=normals,
            colors=None if self.colors is None else self.colors.copy(),
            confidence=None if self.confidence is None else self.confidence.copy(),
            frame=self.frame,
            authority=derive(
                [self.authority], proposed=AuthorityClass.SENSOR_DERIVED
            ),
            source_path=self.source_path,
            notes=[*self.notes, f"estimate_normals k={k}"],
        )

    def chamfer_distance(self, other: PointCloud) -> float:
        """Symmetric mean nearest-neighbour distance (metres in the shared frame)."""
        self.frame.require_compatible(other.frame)
        if len(self) == 0 or len(other) == 0:
            raise ValidationError("chamfer_distance requires non-empty clouds")
        tree_a = cKDTree(self.positions)
        tree_b = cKDTree(other.positions)
        dist_ab, _ = tree_b.query(self.positions, k=1)
        dist_ba, _ = tree_a.query(other.positions, k=1)
        return float(0.5 * (np.mean(dist_ab) + np.mean(dist_ba)))

    def umeyama_align(
        self, other: PointCloud, *, with_scale: bool = True
    ) -> SimilarityTransform:
        """Umeyama similarity aligning `self` onto `other` (paired by index).

        Requires equal cardinality and correspondence by row order. For
        unordered clouds, callers must establish correspondence first.
        """
        self.frame.require_compatible(other.frame)
        if len(self) != len(other):
            raise ValidationError(
                "umeyama_align requires equal point counts (correspondence by index)"
            )
        if len(self) < 3:
            raise ValidationError("umeyama_align needs at least 3 correspondences")
        return umeyama(self.positions, other.positions, with_scale=with_scale)

    def seal_observation_bundle(
        self,
        *,
        target_id: str,
        artifact_refs: list[str] | None = None,
        operation: str = "spatial.pointcloud.ingest",
    ) -> ObservationBundle:
        lineage = Lineage(
            operation=operation,
            inputs=list(artifact_refs or ([self.source_path] if self.source_path else [])),
            input_authorities=[],
            parameters={
                "point_count": len(self),
                "has_normals": self.normals is not None,
                "has_colors": self.colors is not None,
                "bounds": self.bounds(),
                "claimed_authority": self.authority.value,
            },
            environment={"frame": self.frame.to_dict()},
            limitations=list(self.notes),
        )
        uncertainty = Uncertainty(
            kind="point-cloud",
            units=Units.METRE,
            basis="positional sigma not estimated",
            samples=len(self),
        )
        return ObservationBundle(
            id=f"pcd-{uuid.uuid4().hex[:12]}",
            target_id=target_id,
            authority=self.authority,
            lineage=lineage,
            uncertainty=uncertainty,
            sensors=[{"type": "pointcloud", "frame": self.frame.name}],
            artifacts=list(
                artifact_refs or ([self.source_path] if self.source_path else [])
            ),
            modalities=["pointcloud"],
            coverage={"point_count": len(self), "bounds": self.bounds()},
        ).seal()


def umeyama(
    source: np.ndarray,
    target: np.ndarray,
    *,
    with_scale: bool = True,
) -> SimilarityTransform:
    """Least-squares similarity transform mapping source → target (Umeyama 1991)."""
    src = np.asarray(source, dtype=np.float64)
    dst = np.asarray(target, dtype=np.float64)
    n = src.shape[0]
    mu_src = src.mean(axis=0)
    mu_dst = dst.mean(axis=0)
    src_c = src - mu_src
    dst_c = dst - mu_dst
    cov = (dst_c.T @ src_c) / n
    u, singular, vt = np.linalg.svd(cov)
    d = np.ones(3)
    if np.linalg.det(u) * np.linalg.det(vt) < 0:
        d[-1] = -1.0
    rotation = u @ np.diag(d) @ vt
    if with_scale:
        var_src = float(np.mean(np.sum(src_c**2, axis=1)))
        scale = float(np.sum(singular * d) / max(var_src, 1e-18))
    else:
        scale = 1.0
    translation = mu_dst - scale * rotation @ mu_src
    aligned = scale * (src @ rotation.T) + translation
    rmse = float(np.sqrt(np.mean(np.sum((aligned - dst) ** 2, axis=1))))
    return SimilarityTransform(
        rotation=rotation, translation=translation, scale=scale, rmse=rmse
    )


# ------------------------------------------------------------------ PLY helpers


def _parse_ply_header(lines: list[str]) -> dict[str, Any]:
    if not lines or lines[0] != "ply":
        raise ValidationError("PLY header must start with 'ply'")
    fmt = "ascii"
    vertex_count = 0
    properties: list[tuple[str, str]] = []
    in_vertex = False
    for line in lines[1:]:
        if line.startswith("format "):
            fmt = line.split()[1]
        elif line.startswith("element vertex "):
            vertex_count = int(line.split()[2])
            in_vertex = True
        elif line.startswith("element "):
            in_vertex = False
        elif line.startswith("property ") and in_vertex:
            parts = line.split()
            # property <type> <name>  or  property list ...
            if parts[1] == "list":
                raise ValidationError("list properties are not supported in spatial PLY")
            properties.append((parts[1], parts[2]))
        elif line == "end_header":
            break
    if vertex_count < 0:
        raise ValidationError("invalid vertex count")
    return {"format": fmt, "vertex_count": vertex_count, "properties": properties}


_TYPE_STRUCT = {
    "char": "b",
    "uchar": "B",
    "int8": "b",
    "uint8": "B",
    "short": "h",
    "ushort": "H",
    "int16": "h",
    "uint16": "H",
    "int": "i",
    "uint": "I",
    "int32": "i",
    "uint32": "I",
    "float": "f",
    "float32": "f",
    "double": "d",
    "float64": "d",
}


def _read_binary_vertices(
    stream: BinaryIO, meta: dict[str, Any]
) -> tuple[np.ndarray, np.ndarray | None, np.ndarray | None]:
    endian = "<" if meta["format"] == "binary_little_endian" else ">"
    props = meta["properties"]
    fmt = endian + "".join(_TYPE_STRUCT[ptype] for ptype, _ in props)
    size = struct.calcsize(fmt)
    names = [name for _, name in props]
    rows: list[tuple[Any, ...]] = []
    for _ in range(meta["vertex_count"]):
        raw = stream.read(size)
        if len(raw) != size:
            raise ValidationError("truncated PLY vertex payload")
        rows.append(struct.unpack(fmt, raw))
    return _rows_to_arrays(rows, names)


def _read_ascii_vertices(
    stream: BinaryIO, meta: dict[str, Any]
) -> tuple[np.ndarray, np.ndarray | None, np.ndarray | None]:
    names = [name for _, name in meta["properties"]]
    rows: list[tuple[Any, ...]] = []
    for _ in range(meta["vertex_count"]):
        line = stream.readline()
        if not line:
            raise ValidationError("truncated PLY ascii vertex payload")
        parts = line.decode("ascii", errors="replace").split()
        values: list[Any] = []
        for index, (ptype, _) in enumerate(meta["properties"]):
            token = parts[index]
            if ptype in {"float", "float32", "double", "float64"}:
                values.append(float(token))
            else:
                values.append(int(token))
        rows.append(tuple(values))
    return _rows_to_arrays(rows, names)


def _rows_to_arrays(
    rows: list[tuple[Any, ...]], names: list[str]
) -> tuple[np.ndarray, np.ndarray | None, np.ndarray | None]:
    if not rows:
        return np.zeros((0, 3), dtype=np.float64), None, None
    by_name = {
        name: np.array([row[i] for row in rows], dtype=np.float64)
        for i, name in enumerate(names)
    }
    for axis in ("x", "y", "z"):
        if axis not in by_name:
            raise ValidationError(f"PLY missing property {axis}")
    positions = np.column_stack([by_name["x"], by_name["y"], by_name["z"]])
    normals = None
    if all(name in by_name for name in ("nx", "ny", "nz")):
        normals = np.column_stack([by_name["nx"], by_name["ny"], by_name["nz"]])
    colors = None
    if all(name in by_name for name in ("red", "green", "blue")):
        colors = np.column_stack(
            [by_name["red"], by_name["green"], by_name["blue"]]
        ) / 255.0
    return positions, normals, colors
