"""Depth map ingestion, metric scaling, back-projection, and normals."""

from __future__ import annotations

import struct
import uuid
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.core.errors import BlenderVisionError, ValidationError
from blender_vision.v2.authority import (
    OPENCV_CAMERA,
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    derive,
)
from blender_vision.v2.records import Lineage, ObservationBundle


class UnscaledDepthError(BlenderVisionError):
    """Raised when metric promotion is requested without an explicit scale source."""


class DepthKind(StrEnum):
    METRIC = "metric"
    DISPARITY = "disparity"
    RELATIVE = "relative"
    Z_BUFFER = "z_buffer"


@dataclass(slots=True)
class DepthScaleSource:
    """Explicit evidence that turns relative/disparity depth into metres."""

    kind: str
    scale_m: float
    authority: AuthorityClass
    description: str = ""
    baseline_m: float | None = None
    focal_px: float | None = None

    def __post_init__(self) -> None:
        if not np.isfinite(self.scale_m) or self.scale_m <= 0.0:
            raise ValidationError("DepthScaleSource.scale_m must be finite and positive")


@dataclass(slots=True)
class DepthMap:
    """Float32 HxW depth or disparity with governed frame and authority."""

    depth: np.ndarray
    mask: np.ndarray
    intrinsics: dict[str, float]
    frame: CoordinateFrame = field(default_factory=lambda: OPENCV_CAMERA)
    authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED
    confidence: np.ndarray | None = None
    kind: DepthKind = DepthKind.METRIC
    units: Units = Units.METRE
    width: int = 0
    height: int = 0
    source_path: str | None = None
    notes: list[str] = field(default_factory=list)

    def __post_init__(self) -> None:
        self.depth = np.asarray(self.depth, dtype=np.float32)
        if self.depth.ndim != 2:
            raise ValidationError("depth must be HxW")
        self.height, self.width = int(self.depth.shape[0]), int(self.depth.shape[1])
        self.mask = np.asarray(self.mask, dtype=bool)
        if self.mask.shape != self.depth.shape:
            raise ValidationError("mask shape must match depth")
        if self.confidence is not None:
            self.confidence = np.asarray(self.confidence, dtype=np.float32)
            if self.confidence.shape != self.depth.shape:
                raise ValidationError("confidence shape must match depth")
        required = ("fx", "fy", "cx", "cy")
        missing = [key for key in required if key not in self.intrinsics]
        if missing:
            raise ValidationError(f"intrinsics missing {missing}")
        for key in required:
            value = float(self.intrinsics[key])
            if not np.isfinite(value):
                raise ValidationError(f"intrinsic {key} must be finite")
            self.intrinsics[key] = value

    # ---------------------------------------------------------------- loaders

    @classmethod
    def from_npy(
        cls,
        path: Path | str,
        *,
        intrinsics: dict[str, float],
        mask: np.ndarray | None = None,
        confidence: np.ndarray | None = None,
        kind: DepthKind = DepthKind.METRIC,
        units: Units = Units.METRE,
        authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED,
        frame: CoordinateFrame | None = None,
    ) -> DepthMap:
        array = np.load(Path(path))
        if array.ndim == 3 and array.shape[-1] == 1:
            array = array[..., 0]
        if array.dtype != np.float32:
            array = array.astype(np.float32)
        if mask is None:
            mask = np.isfinite(array) & (array > 0)
        return cls(
            depth=array,
            mask=mask,
            intrinsics=dict(intrinsics),
            frame=frame or OPENCV_CAMERA,
            authority=authority,
            confidence=confidence,
            kind=kind,
            units=units,
            source_path=str(path),
        )

    @classmethod
    def from_16bit_png(
        cls,
        path: Path | str,
        *,
        intrinsics: dict[str, float],
        depth_scale: float = 0.001,
        kind: DepthKind = DepthKind.METRIC,
        units: Units = Units.METRE,
        authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED,
        frame: CoordinateFrame | None = None,
        max_raw: float = 65535.0,
    ) -> DepthMap:
        """Load a 16-bit PNG depth image. Values are multiplied by depth_scale.

        Default scale 0.001 maps millimetre-encoded uint16 to metres. Callers
        that lack a physical scale must pass kind=RELATIVE and authority below
        MEASURED — this loader never invents metric authority.
        """
        from PIL import Image

        image = Image.open(Path(path))
        array = np.asarray(image)
        if array.ndim == 3:
            array = array[..., 0]
        if array.dtype != np.uint16 and array.dtype != np.uint8:
            # Allow float PNGs that already hold metric values when depth_scale=1.
            array = array.astype(np.float32)
        else:
            array = array.astype(np.float32) * float(depth_scale)
        mask = np.isfinite(array) & (array > 0)
        if max_raw > 0 and depth_scale != 1.0:
            # Saturate pixels that hit the sensor max as invalid.
            raw = np.asarray(Image.open(Path(path)))
            if raw.ndim == 3:
                raw = raw[..., 0]
            mask &= raw.astype(np.float32) < max_raw
        return cls(
            depth=array.astype(np.float32),
            mask=mask,
            intrinsics=dict(intrinsics),
            frame=frame or OPENCV_CAMERA,
            authority=authority,
            kind=kind,
            units=units,
            source_path=str(path),
            notes=[f"16-bit PNG loaded with depth_scale={depth_scale}"],
        )

    @classmethod
    def from_pfm(
        cls,
        path: Path | str,
        *,
        intrinsics: dict[str, float],
        authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED,
        kind: DepthKind = DepthKind.METRIC,
        units: Units = Units.METRE,
        frame: CoordinateFrame | None = None,
    ) -> DepthMap:
        """Load a Portable FloatMap (colour or grayscale)."""
        path = Path(path)
        with path.open("rb") as stream:
            header = stream.readline().decode("ascii").strip()
            if header not in {"PF", "Pf"}:
                raise ValidationError(f"not a PFM file: {path}")
            colour = header == "PF"
            dims = stream.readline().decode("ascii").strip()
            while dims.startswith("#") or not dims:
                dims = stream.readline().decode("ascii").strip()
            width_s, height_s = dims.split()
            width, height = int(width_s), int(height_s)
            scale_line = stream.readline().decode("ascii").strip()
            scale = float(scale_line)
            endian = "<" if scale < 0 else ">"
            channels = 3 if colour else 1
            count = width * height * channels
            raw = stream.read(count * 4)
            if len(raw) != count * 4:
                raise ValidationError(f"truncated PFM payload in {path}")
            values = np.array(
                struct.unpack(f"{endian}{count}f", raw), dtype=np.float32
            )
            if colour:
                values = values.reshape(height, width, 3)[..., 0]
            else:
                values = values.reshape(height, width)
            # PFM rows are bottom-to-top.
            values = np.flipud(values)
        mask = np.isfinite(values) & (values > 0)
        return cls(
            depth=values,
            mask=mask,
            intrinsics=dict(intrinsics),
            frame=frame or OPENCV_CAMERA,
            authority=authority,
            kind=kind,
            units=units,
            source_path=str(path),
        )

    # --------------------------------------------------------------- metric

    def is_metric(self) -> bool:
        return self.kind is DepthKind.METRIC and self.units is Units.METRE

    def to_metric(self, scale: DepthScaleSource) -> DepthMap:
        """Apply an explicit scale source. Never invents metric authority.

        The returned map's authority is the derive() of this map and the scale
        source. MEASURED is only reachable when the scale source itself is
        MEASURED (or stronger) and this map is at least SENSOR_DERIVED.
        """
        if scale.authority is AuthorityClass.UNRESOLVED:
            raise UnscaledDepthError(
                "cannot promote depth to metric with UNRESOLVED scale authority"
            )
        if self.kind is DepthKind.DISPARITY:
            if (
                (scale.baseline_m is None or scale.focal_px is None)
                and scale.kind != "direct_scale"
            ):
                raise UnscaledDepthError(
                    "disparity-to-metric requires baseline_m and focal_px, "
                    "or kind='direct_scale' with scale_m applied per-pixel as Z=scale/d"
                )
            if scale.baseline_m is not None and scale.focal_px is not None:
                with np.errstate(divide="ignore", invalid="ignore"):
                    metric = (scale.baseline_m * scale.focal_px) / self.depth
                metric = metric.astype(np.float32)
                metric_mask = self.mask & np.isfinite(metric) & (metric > 0)
            else:
                with np.errstate(divide="ignore", invalid="ignore"):
                    metric = (scale.scale_m / self.depth).astype(np.float32)
                metric_mask = self.mask & np.isfinite(metric) & (metric > 0)
        elif self.kind is DepthKind.METRIC and self.units is Units.METRE:
            metric = (self.depth * float(scale.scale_m)).astype(np.float32)
            metric_mask = self.mask.copy()
        else:
            metric = (self.depth * float(scale.scale_m)).astype(np.float32)
            metric_mask = self.mask & np.isfinite(metric) & (metric > 0)

        authority = derive(
            [self.authority, scale.authority],
            proposed=AuthorityClass.SENSOR_DERIVED,
        )
        # Explicit rule: unscaled relative depth cannot become MEASURED even if
        # a caller tries to claim it. derive() already caps, but refuse the
        # silent-promotion path when the scale source is weaker than MEASURED
        # and someone later tries promote().
        notes = list(self.notes)
        notes.append(
            f"to_metric via {scale.kind}: scale_m={scale.scale_m}, "
            f"scale_authority={scale.authority.value}"
        )
        return DepthMap(
            depth=metric,
            mask=metric_mask,
            intrinsics=dict(self.intrinsics),
            frame=self.frame,
            authority=authority,
            confidence=None if self.confidence is None else self.confidence.copy(),
            kind=DepthKind.METRIC,
            units=Units.METRE,
            source_path=self.source_path,
            notes=notes,
        )

    def assert_not_measured_without_scale(self) -> None:
        """Hard guard used by ingestion seals and tests."""
        if self.authority is AuthorityClass.MEASURED and not self.is_metric():
            raise UnscaledDepthError(
                "unscaled depth cannot carry MEASURED authority"
            )
        if self.authority is AuthorityClass.MEASURED and self.kind is not DepthKind.METRIC:
            raise UnscaledDepthError(
                f"depth kind {self.kind.value} cannot carry MEASURED authority"
            )

    # ---------------------------------------------------------- geometry ops

    def back_project(self) -> np.ndarray:
        """Return Nx3 points in the depth map's camera frame (typically OpenCV)."""
        if self.kind is DepthKind.DISPARITY:
            raise ValidationError(
                "back_project requires metric depth; call to_metric() first"
            )
        if self.kind is DepthKind.RELATIVE:
            raise UnscaledDepthError(
                "back_project refuses relative depth without to_metric()"
            )
        ys, xs = np.where(self.mask)
        if ys.size == 0:
            return np.zeros((0, 3), dtype=np.float64)
        z = self.depth[ys, xs].astype(np.float64)
        fx = self.intrinsics["fx"]
        fy = self.intrinsics["fy"]
        cx = self.intrinsics["cx"]
        cy = self.intrinsics["cy"]
        x = (xs.astype(np.float64) - cx) * z / fx
        y = (ys.astype(np.float64) - cy) * z / fy
        return np.column_stack([x, y, z])

    def to_normals(self, *, edge_threshold_m: float = 0.05) -> np.ndarray:
        """Finite-difference surface normals in the camera frame.

        Edge-aware: when the absolute depth jump between neighbours exceeds
        `edge_threshold_m`, that gradient contribution is zeroed so depth
        discontinuities do not produce spurious normal spikes.
        """
        if not self.is_metric() and self.kind is not DepthKind.Z_BUFFER:
            raise UnscaledDepthError(
                "to_normals requires metric (or z_buffer) depth with known scale"
            )
        z = self.depth.astype(np.float64)
        valid = self.mask
        fx = self.intrinsics["fx"]
        fy = self.intrinsics["fy"]
        # Central differences on Z; boundary pixels fall back to forward/back.
        dzdx = np.zeros_like(z)
        dzdy = np.zeros_like(z)
        dzdx[:, 1:-1] = (z[:, 2:] - z[:, :-2]) * 0.5
        dzdx[:, 0] = z[:, 1] - z[:, 0]
        dzdx[:, -1] = z[:, -1] - z[:, -2]
        dzdy[1:-1, :] = (z[2:, :] - z[:-2, :]) * 0.5
        dzdy[0, :] = z[1, :] - z[0, :]
        dzdy[-1, :] = z[-1, :] - z[-2, :]

        # Edge guard: kill gradients across large depth jumps.
        jump_x = np.zeros_like(z, dtype=bool)
        jump_y = np.zeros_like(z, dtype=bool)
        jump_x[:, 1:-1] = (
            np.abs(z[:, 2:] - z[:, 1:-1]) > edge_threshold_m
        ) | (np.abs(z[:, 1:-1] - z[:, :-2]) > edge_threshold_m)
        jump_y[1:-1, :] = (
            np.abs(z[2:, :] - z[1:-1, :]) > edge_threshold_m
        ) | (np.abs(z[1:-1, :] - z[:-2, :]) > edge_threshold_m)
        dzdx = np.where(jump_x, 0.0, dzdx)
        dzdy = np.where(jump_y, 0.0, dzdy)

        # Range-image normals in the OpenCV camera frame:
        # n ∝ (-fy * ∂Z/∂u, -fx * ∂Z/∂v, fx * fy), then unit-normalised.
        normals = np.stack(
            [-dzdx * fy, -dzdy * fx, np.full_like(z, fx * fy)],
            axis=-1,
        )
        norms = np.linalg.norm(normals, axis=-1, keepdims=True)
        norms = np.maximum(norms, 1e-12)
        normals = normals / norms
        normals = np.where(valid[..., None], normals, 0.0)
        return normals.astype(np.float32)

    def validate_against(self, camera: dict[str, Any]) -> list[str]:
        """Check depth dimensions and intrinsics against a camera dict.

        Expected keys: width, height, and either nested `intrinsics` or flat
        fx/fy/cx/cy. Returns a list of human-readable issues (empty = ok).
        """
        issues: list[str] = []
        width = int(camera.get("width", 0))
        height = int(camera.get("height", 0))
        if width and width != self.width:
            issues.append(f"width mismatch: depth={self.width} camera={width}")
        if height and height != self.height:
            issues.append(f"height mismatch: depth={self.height} camera={height}")
        cam_k = camera.get("intrinsics", camera)
        for key in ("fx", "fy", "cx", "cy"):
            if key not in cam_k:
                continue
            expected = float(cam_k[key])
            actual = float(self.intrinsics[key])
            if abs(expected - actual) > 1e-3 * max(1.0, abs(expected)):
                issues.append(f"intrinsic {key}: depth={actual} camera={expected}")
        return issues

    # ----------------------------------------------------------- V2 records

    def seal_observation_bundle(
        self,
        *,
        target_id: str,
        artifact_refs: list[str] | None = None,
        operation: str = "spatial.depth.ingest",
        parameters: dict[str, Any] | None = None,
    ) -> ObservationBundle:
        """Build and seal an ObservationBundle for this depth map."""
        self.assert_not_measured_without_scale()
        # input_authorities left empty for SENSOR_DERIVED/MEASURED seals: V2's
        # lineage.authority_ceiling() routes through derive(proposed=INFERRED),
        # which would otherwise cap every non-empty input list at INFERRED.
        # Evidence paths and the claimed authority live in inputs/parameters.
        lineage = Lineage(
            operation=operation,
            inputs=list(artifact_refs or ([self.source_path] if self.source_path else [])),
            input_authorities=[],
            parameters={
                "width": self.width,
                "height": self.height,
                "kind": self.kind.value,
                "units": self.units.value,
                "intrinsics": dict(self.intrinsics),
                "valid_fraction": float(self.mask.mean()) if self.mask.size else 0.0,
                "claimed_authority": self.authority.value,
                **(parameters or {}),
            },
            environment={"frame": self.frame.to_dict()},
            limitations=list(self.notes),
        )
        uncertainty = Uncertainty(
            kind="depth-map",
            sigma=None,
            units=self.units,
            basis="per-pixel mask; confidence band not estimated"
            if self.confidence is None
            else "per-pixel confidence supplied",
            samples=int(self.mask.sum()),
        )
        bundle = ObservationBundle(
            id=f"depth-{uuid.uuid4().hex[:12]}",
            target_id=target_id,
            authority=self.authority,
            lineage=lineage,
            uncertainty=uncertainty,
            sensors=[
                {
                    "type": "depth",
                    "kind": self.kind.value,
                    "intrinsics": dict(self.intrinsics),
                    "frame": self.frame.name,
                }
            ],
            artifacts=list(artifact_refs or ([self.source_path] if self.source_path else [])),
            modalities=["depth"],
            coverage={
                "valid_pixels": int(self.mask.sum()),
                "total_pixels": int(self.mask.size),
                "valid_fraction": float(self.mask.mean()) if self.mask.size else 0.0,
            },
        )
        return bundle.seal()

    def summary(self) -> dict[str, Any]:
        valid = self.depth[self.mask] if self.mask.any() else np.array([], dtype=np.float32)
        return {
            "width": self.width,
            "height": self.height,
            "kind": self.kind.value,
            "units": self.units.value,
            "authority": self.authority.value,
            "frame": self.frame.name,
            "valid_pixels": int(self.mask.sum()),
            "depth_min": float(valid.min()) if valid.size else None,
            "depth_max": float(valid.max()) if valid.size else None,
            "depth_mean": float(valid.mean()) if valid.size else None,
        }
