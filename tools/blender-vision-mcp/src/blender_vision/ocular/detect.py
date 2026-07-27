"""Perception-driven object detection for the Ocular tracking loop.

Detections are derived from image evidence alone — segmentation regions
carrying bbox, centroid, mask area, and a multi-block appearance embedding.
Ground-truth boxes never enter this module; identity must come from pixels.
"""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from enum import StrEnum
from typing import Any

import cv2
import numpy as np
from numpy.typing import NDArray

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.segment import (
    SEGMENT_AUTHORITY_CEILING,
    SegmentationMethod,
    segment,
)
from blender_vision.ocular.track import Detection, TrackTargetKind
from blender_vision.v2.authority import AuthorityClass

ArrayU8 = NDArray[np.uint8]
ArrayF64 = NDArray[np.float64]

# Embedding block weights (stated; each block is L2-normalised before weighting).
COLOUR_BLOCK_WEIGHT = 0.40
GRADIENT_BLOCK_WEIGHT = 0.35
SHAPE_BLOCK_WEIGHT = 0.25

# Colour histogram: 8 bins per LAB channel → 24 dims.
_LAB_BINS = 8
# Oriented-gradient descriptor: 8 orientation bins over a coarse 2x2 grid → 32 dims.
_OG_ORIENTATIONS = 8
_OG_GRID = 2
# Shape block: 7 Hu moments + aspect + extent + solidity → 10 dims.
_SHAPE_DIMS = 10

EMBEDDING_DIM = (
    _LAB_BINS * 3
    + _OG_ORIENTATIONS * _OG_GRID * _OG_GRID
    + _SHAPE_DIMS
)

DEFAULT_MIN_AREA = 40
DEFAULT_MAX_REGIONS = 24


class DetectionMethod(StrEnum):
    BACKGROUND_MODEL = "background_model"
    WATERSHED = "watershed"
    REGION_GROW = "region_grow"
    MOTION_COMPONENTS = "motion_components"
    FUSED = "fused"
    #: Multi-source proposal fusion (see ocular.proposals). Preferred path when
    #: a single segmentation method collapses multiple objects into one region.
    PROPOSAL_FUSION = "proposal_fusion"


@dataclass(slots=True)
class DetectionConfig:
    """Declared knobs for the pure-vision detector."""

    method: DetectionMethod = DetectionMethod.WATERSHED
    min_area: int = DEFAULT_MIN_AREA
    max_regions: int = DEFAULT_MAX_REGIONS
    residual_threshold: int = 12
    #: Per-channel absolute difference from the background model that counts as
    #: foreground. 18/255 separates the white spheres from the white table on
    #: the hard fixtures without picking up render noise.
    background_threshold: int = 18


def _as_bgr(image: NDArray[Any]) -> ArrayU8:
    if image.ndim == 2:
        return cv2.cvtColor(image.astype(np.uint8), cv2.COLOR_GRAY2BGR)
    if image.ndim != 3 or image.shape[2] not in (3, 4):
        raise ValueError(f"expected HxW or HxWx3/4 image, got shape {image.shape}")
    arr = image[:, :, :3]
    if arr.dtype != np.uint8:
        arr = np.clip(arr, 0, 255).astype(np.uint8)
    return np.ascontiguousarray(arr)


def _l2_normalise(vec: ArrayF64) -> ArrayF64:
    n = float(np.linalg.norm(vec))
    if n <= 1e-12:
        return vec
    return vec / n


def colour_histogram_lab(image_bgr: ArrayU8, mask: ArrayU8) -> ArrayF64:
    """Normalised LAB histogram — perceptually spaced colour block."""
    lab = cv2.cvtColor(image_bgr, cv2.COLOR_BGR2LAB)
    parts: list[ArrayF64] = []
    # OpenCV LAB: L in [0,255], a/b in [0,255] (128-centred).
    ranges = ([0, 256], [0, 256], [0, 256])
    for channel, rng in enumerate(ranges):
        hist = cv2.calcHist([lab], [channel], mask, [_LAB_BINS], rng).flatten()
        parts.append(hist.astype(np.float64))
    combined = np.concatenate(parts)
    total = float(combined.sum())
    if total <= 0.0:
        return np.zeros(_LAB_BINS * 3, dtype=np.float64)
    return combined / total


def oriented_gradient_descriptor(image_bgr: ArrayU8, mask: ArrayU8) -> ArrayF64:
    """Coarse oriented-gradient histogram over the masked region (HOG-like)."""
    gray = cv2.cvtColor(image_bgr, cv2.COLOR_BGR2GRAY)
    gx = cv2.Sobel(gray, cv2.CV_64F, 1, 0, ksize=3)
    gy = cv2.Sobel(gray, cv2.CV_64F, 0, 1, ksize=3)
    mag = np.hypot(gx, gy)
    ang = (np.arctan2(gy, gx) + np.pi) / (2.0 * np.pi)  # [0, 1)
    ys, xs = np.where(mask > 0)
    n_cells = _OG_GRID * _OG_GRID
    n_bins = _OG_ORIENTATIONS
    desc = np.zeros(n_cells * n_bins, dtype=np.float64)
    if len(xs) == 0:
        return desc
    x0, x1 = int(xs.min()), int(xs.max())
    y0, y1 = int(ys.min()), int(ys.max())
    bw = max(1, x1 - x0 + 1)
    bh = max(1, y1 - y0 + 1)
    for y, x in zip(ys, xs, strict=False):
        cell_x = min(_OG_GRID - 1, int((x - x0) / bw * _OG_GRID))
        cell_y = min(_OG_GRID - 1, int((y - y0) / bh * _OG_GRID))
        cell = cell_y * _OG_GRID + cell_x
        orient = min(n_bins - 1, int(ang[y, x] * n_bins))
        desc[cell * n_bins + orient] += mag[y, x]
    total = float(desc.sum())
    if total > 0.0:
        desc = desc / total
    return desc


def shape_moments(mask: ArrayU8) -> ArrayF64:
    """Hu moments plus aspect, extent, solidity — geometry of the silhouette."""
    out = np.zeros(_SHAPE_DIMS, dtype=np.float64)
    area = float((mask > 0).sum())
    if area <= 0.0:
        return out
    # Hu moments are log-scaled for dynamic-range control.
    moments = cv2.moments(mask, binaryImage=True)
    hu = cv2.HuMoments(moments).flatten()
    for i in range(7):
        v = float(hu[i])
        out[i] = -np.sign(v) * np.log10(abs(v) + 1e-12) if v != 0.0 else 0.0
    x, y, w, h = cv2.boundingRect(mask)
    out[7] = float(w) / float(h) if h > 0 else 1.0  # aspect
    bbox_area = float(max(1, w * h))
    out[8] = area / bbox_area  # extent
    contours, _ = cv2.findContours(mask, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    if contours:
        hull = cv2.convexHull(contours[0])
        hull_area = float(cv2.contourArea(hull))
        out[9] = area / hull_area if hull_area > 0 else 1.0  # solidity
    else:
        out[9] = 1.0
    return out


def appearance_embedding(image: NDArray[Any], mask: NDArray[Any]) -> list[float]:
    """Multi-block appearance embedding for association / re-identification.

    Blocks (weights stated):
      colour LAB histogram · 0.40
      oriented-gradient descriptor · 0.35
      shape moments (Hu + aspect + extent + solidity) · 0.25

    Each block is L2-normalised before weighting so one block cannot dominate.
    """
    bgr = _as_bgr(image)
    m = (mask > 0).astype(np.uint8)
    if m.ndim != 2:
        raise ValueError(f"mask must be HxW, got shape {m.shape}")
    if m.shape[:2] != bgr.shape[:2]:
        raise ValueError("mask spatial size must match image")

    colour = _l2_normalise(colour_histogram_lab(bgr, m)) * COLOUR_BLOCK_WEIGHT
    gradient = _l2_normalise(oriented_gradient_descriptor(bgr, m)) * GRADIENT_BLOCK_WEIGHT
    shape = _l2_normalise(shape_moments(m)) * SHAPE_BLOCK_WEIGHT
    emb = np.concatenate([colour, gradient, shape])
    # Final L2 so cosine distance is well-defined.
    emb = _l2_normalise(emb)
    return [float(v) for v in emb]


def embedding_similarity(
    a: list[float] | ArrayF64, b: list[float] | ArrayF64
) -> float:
    """Cosine similarity mapped to [0, 1]. 1 is identical."""
    va = np.asarray(a, dtype=np.float64).ravel()
    vb = np.asarray(b, dtype=np.float64).ravel()
    if va.size == 0 or vb.size == 0 or va.shape != vb.shape:
        return 0.0
    na = float(np.linalg.norm(va))
    nb = float(np.linalg.norm(vb))
    if na <= 1e-12 or nb <= 1e-12:
        return 0.0
    cos = float(np.dot(va, vb) / (na * nb))
    # Map [-1, 1] → [0, 1].
    return float(np.clip(0.5 * (cos + 1.0), 0.0, 1.0))


def embedding_distance(
    a: list[float] | ArrayF64, b: list[float] | ArrayF64
) -> float:
    """1 − similarity. 0 is identical."""
    return 1.0 - embedding_similarity(a, b)


def _mask_from_bbox(
    shape: tuple[int, int], bbox_xywh: tuple[float, float, float, float]
) -> ArrayU8:
    h, w = shape
    x, y, bw, bh = bbox_xywh
    mask = np.zeros((h, w), dtype=np.uint8)
    x0 = int(max(0, min(w - 1, x)))
    y0 = int(max(0, min(h - 1, y)))
    x1 = int(max(x0 + 1, min(w, x + bw)))
    y1 = int(max(y0 + 1, min(h, y + bh)))
    mask[y0:y1, x0:x1] = 1
    return mask


def detection_from_mask(
    image: NDArray[Any],
    mask: NDArray[Any],
    *,
    detection_id: str,
    frame_index: int,
    conf: float = 1.0,
    method: DetectionMethod = DetectionMethod.WATERSHED,
) -> Detection | None:
    """Build one Detection from a binary mask. Returns None if empty."""
    bgr = _as_bgr(image)
    m = (mask > 0).astype(np.uint8)
    area = int(m.sum())
    if area <= 0:
        return None
    ys, xs = np.where(m > 0)
    x0, x1 = int(xs.min()), int(xs.max())
    y0, y1 = int(ys.min()), int(ys.max())
    bw = x1 - x0 + 1
    bh = y1 - y0 + 1
    cx = float(xs.mean())
    cy = float(ys.mean())
    emb = appearance_embedding(bgr, m)
    # Colour block alone also stored as appearance_hist for hist-only callers.
    colour = colour_histogram_lab(bgr, m)
    return Detection(
        detection_id=detection_id,
        kind=TrackTargetKind.OBJECT,
        bbox_xywh=(float(x0), float(y0), float(bw), float(bh)),
        centroid_xy=(cx, cy),
        appearance_hist=[float(v) for v in colour],
        appearance_embedding=emb,
        area_px=float(area),
        frame_index=frame_index,
        conf=float(conf),
        meta={
            "method": method.value,
            "authority": SEGMENT_AUTHORITY_CEILING.value,
            # Hard contract: no ground-truth identity may ride along.
            "gt_forbidden": True,
        },
    )


class BackgroundModel:
    """Temporal median background for a static camera.

    Single-frame intensity segmentation cannot separate a white sphere from a
    white table: the detector found one region on fixtures containing three
    objects, which is why perception-driven tracking collapsed. The camera is
    static in these sequences, so the per-pixel median over time is the empty
    scene, and everything that differs from it is an object. Measured on the
    crossing_paths fixture: 2 components where watershed found 1, correctly
    merging to 1 at the frame where the objects actually overlap.

    Not usable when the camera moves — the retina's coverage signal reports
    that, and the caller must fall back to a single-frame method.
    """

    __slots__ = ("_background", "_frames", "_threshold")

    def __init__(self, frames: Sequence[NDArray[Any]], *, threshold: int = 18) -> None:
        if not frames:
            raise ValidationError("a background model needs at least one frame")
        stack = np.stack([_as_bgr(f).astype(np.float32) for f in frames])
        self._background = np.median(stack, axis=0).astype(np.uint8)
        self._frames = len(frames)
        self._threshold = int(threshold)

    @property
    def background(self) -> ArrayU8:
        return self._background

    def foreground_mask(self, image: NDArray[Any]) -> ArrayU8:
        difference = cv2.absdiff(_as_bgr(image), self._background).max(axis=2)
        mask = (difference > self._threshold).astype(np.uint8)
        mask = cv2.morphologyEx(mask, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
        return cv2.morphologyEx(mask, cv2.MORPH_CLOSE, np.ones((7, 7), np.uint8))


def detect_with_background(
    image: NDArray[Any],
    model: BackgroundModel,
    *,
    frame_index: int = 0,
    config: DetectionConfig | None = None,
) -> list[Detection]:
    """Detections from foreground components against a temporal background."""
    cfg = config or DetectionConfig()
    bgr = _as_bgr(image)
    mask = model.foreground_mask(bgr)
    count, labels, stats, _ = cv2.connectedComponentsWithStats(mask, 8)
    detections: list[Detection] = []
    order = sorted(
        range(1, count), key=lambda i: int(stats[i, cv2.CC_STAT_AREA]), reverse=True
    )
    for label in order[: cfg.max_regions]:
        if int(stats[label, cv2.CC_STAT_AREA]) < cfg.min_area:
            continue
        component = (labels == label).astype(np.uint8)
        detections.append(
            detection_from_mask(
                bgr,
                component,
                frame_index=frame_index,
                detection_id=f"bg-{frame_index}-{label}",
            )
        )
    assert_no_ground_truth_in_detections(detections)
    return detections


def detections_from_proposals(
    image: NDArray[Any],
    *,
    frame_index: int = 0,
    previous_image: NDArray[Any] | None = None,
    background_model: BackgroundModel | None = None,
    depth: NDArray[Any] | None = None,
    config: DetectionConfig | None = None,
) -> list[Detection]:
    """Build detections from the multi-source proposal ensemble.

    Prefer fused atomic proposals; fall back to raw active proposals when fusion
    is empty. Ground truth never enters this path.
    """
    # Local import avoids a circular import at module load (proposals → detect).
    from blender_vision.ocular.proposals import (
        ProposalContext,
        ProposalStatus,
        assert_no_ground_truth_in_proposals,
        propose,
    )

    cfg = config or DetectionConfig()
    result = propose(
        image,
        frame_index=frame_index,
        context=ProposalContext(
            previous_image=previous_image,
            background_model=background_model,
            depth=depth,
        ),
    )
    graph = result.graph
    assert_no_ground_truth_in_proposals(graph.proposals)
    pool = list(graph.fused) if graph.fused else [
        p
        for p in graph.proposals
        if p.status is ProposalStatus.ACTIVE and p.area_px > 0 and p.mask is not None
    ]
    detections: list[Detection] = []
    for i, prop in enumerate(pool[: cfg.max_regions]):
        if prop.mask is None or int(prop.mask.sum()) < cfg.min_area:
            continue
        det = detection_from_mask(
            image,
            prop.mask,
            detection_id=prop.proposal_id or f"prop-{frame_index}-{i}",
            frame_index=frame_index,
            conf=float(prop.confidence),
            method=DetectionMethod.PROPOSAL_FUSION,
        )
        if det is not None:
            det.meta["supporting_sources"] = list(prop.supporting_sources)
            det.meta["hypothesis_kind"] = prop.hypothesis_kind.value
            detections.append(det)
    assert_no_ground_truth_in_detections(detections)
    return detections


def detect(
    image: NDArray[Any],
    *,
    frame_index: int = 0,
    config: DetectionConfig | None = None,
    previous_image: NDArray[Any] | None = None,
    background_model: BackgroundModel | None = None,
) -> list[Detection]:
    """Segment the image and emit one Detection per region with embeddings.

    Pure vision: no ground-truth boxes, no oracle IDs. Authority ceiling is
    SENSOR_DERIVED via classical segmentation.
    """
    cfg = config or DetectionConfig()
    bgr = _as_bgr(image)
    if cfg.method is DetectionMethod.PROPOSAL_FUSION:
        return detections_from_proposals(
            bgr,
            frame_index=frame_index,
            previous_image=previous_image,
            background_model=background_model,
            config=cfg,
        )
    if cfg.method is DetectionMethod.BACKGROUND_MODEL:
        if background_model is None:
            raise ValidationError(
                "DetectionMethod.BACKGROUND_MODEL requires background_model="
            )
        return detect_with_background(
            bgr, background_model, frame_index=frame_index, config=cfg
        )
    method_map = {
        DetectionMethod.WATERSHED: SegmentationMethod.WATERSHED,
        DetectionMethod.REGION_GROW: SegmentationMethod.REGION_GROW,
        DetectionMethod.MOTION_COMPONENTS: SegmentationMethod.MOTION_COMPONENTS,
        DetectionMethod.FUSED: SegmentationMethod.WATERSHED,
    }
    seg_method = method_map[cfg.method]

    if cfg.method is DetectionMethod.MOTION_COMPONENTS:
        if previous_image is None:
            # Motion without a prior frame is unavailable; fall back to watershed
            # but keep the method label honest in meta via fused note.
            result, labels = segment(
                bgr,
                method=SegmentationMethod.WATERSHED,
                min_area=cfg.min_area,
                max_regions=cfg.max_regions,
            )
            det_method = DetectionMethod.WATERSHED
        else:
            result, labels = segment(
                bgr,
                method=SegmentationMethod.MOTION_COMPONENTS,
                previous_image=previous_image,
                min_area=cfg.min_area,
                residual_threshold=cfg.residual_threshold,
            )
            det_method = DetectionMethod.MOTION_COMPONENTS
    elif cfg.method is DetectionMethod.FUSED:
        # Watershed regions, supplemented by motion residual components when
        # a previous frame is available — still pure vision.
        result, labels = segment(
            bgr,
            method=SegmentationMethod.WATERSHED,
            min_area=cfg.min_area,
            max_regions=cfg.max_regions,
        )
        det_method = DetectionMethod.FUSED
        if previous_image is not None:
            motion, mlabels = segment(
                bgr,
                method=SegmentationMethod.MOTION_COMPONENTS,
                previous_image=previous_image,
                min_area=cfg.min_area,
                residual_threshold=cfg.residual_threshold,
            )
            # Merge motion labels that do not heavily overlap existing ones.
            dets = _instances_to_detections(
                bgr, result, labels, frame_index=frame_index, method=det_method
            )
            existing_masks = []
            for inst in result.instances:
                x, y, w, h = inst.bbox_xywh
                existing_masks.append(_mask_from_bbox(bgr.shape[:2], (x, y, w, h)))
            for inst in motion.instances:
                x, y, w, h = inst.bbox_xywh
                mm = (mlabels == inst.label).astype(np.uint8)
                if int(mm.sum()) < cfg.min_area:
                    continue
                overlap = False
                for em in existing_masks:
                    inter = int(np.logical_and(mm > 0, em > 0).sum())
                    if inter / max(1, int(mm.sum())) > 0.5:
                        overlap = True
                        break
                if not overlap:
                    d = detection_from_mask(
                        bgr,
                        mm,
                        detection_id=f"{inst.segment_id}-f{frame_index}",
                        frame_index=frame_index,
                        conf=0.8,
                        method=DetectionMethod.MOTION_COMPONENTS,
                    )
                    if d is not None:
                        dets.append(d)
            return dets[: cfg.max_regions]
    else:
        result, labels = segment(
            bgr,
            method=seg_method,
            min_area=cfg.min_area,
            max_regions=cfg.max_regions,
        )
        det_method = cfg.method

    return _instances_to_detections(
        bgr, result, labels, frame_index=frame_index, method=det_method
    )


def _instances_to_detections(
    bgr: ArrayU8,
    result: Any,
    labels: NDArray[Any] | None,
    *,
    frame_index: int,
    method: DetectionMethod,
) -> list[Detection]:
    dets: list[Detection] = []
    if labels is None:
        return dets
    for inst in result.instances:
        mask = (labels == inst.label).astype(np.uint8)
        d = detection_from_mask(
            bgr,
            mask,
            detection_id=f"{inst.segment_id}-f{frame_index}",
            frame_index=frame_index,
            conf=1.0,
            method=method,
        )
        if d is not None:
            dets.append(d)
    return dets


def assert_no_ground_truth_in_detections(detections: list[Detection]) -> None:
    """Contract guard: any GT symbol on a detection is a hard failure."""
    forbidden_keys = {
        "gt_id",
        "ground_truth_id",
        "ground_truth",
        "oracle_id",
        "gt_bbox",
    }
    for det in detections:
        for key in forbidden_keys:
            if key in det.meta:
                raise AssertionError(
                    f"ground truth key {key!r} reached Detection {det.detection_id}"
                )
        # Field must not exist on the runtime type either.
        if hasattr(det, "ground_truth_id"):
            raise AssertionError("Detection carries ground_truth_id")


def perception_authority() -> AuthorityClass:
    """Detections inherit the classical segmentation ceiling."""
    return SEGMENT_AUTHORITY_CEILING
