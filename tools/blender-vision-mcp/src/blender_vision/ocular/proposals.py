"""Multi-source region proposals and non-destructive evidence fusion.

The single-method pixel detector emits one region on scenes that contain three
objects. That is not a thresholding problem — it is a missing-source problem.
This module runs six independent proposal sources and fuses their evidence
without winner-takes-all NMS, so stationary objects, movers, unknown entrants,
and partially occluded objects are all discoverable before temporal association.

Sources (each independently measurable):
  A. appearance/objectness — single-frame masks (must work on frame 0)
  B. temporal change — background residual, flow residual, frame difference
  C. dense visual features — boundary regions + appearance embeddings
  D. geometry — depth/normal discontinuities (BLOCKED when no depth pass)
  E. point-track clusters — sparse LK points grouped by common motion
  F. unknown-region — coherent residual unexplained by known tracks/concepts

Ground truth never enters this module. Identity is derived from image evidence
alone. Fusion preserves split and merge hypotheses for association to resolve.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

import cv2
import numpy as np
from numpy.typing import NDArray

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.detect import (
    BackgroundModel,
    appearance_embedding,
    colour_histogram_lab,
    embedding_similarity,
)
from blender_vision.ocular.segment import (
    SEGMENT_AUTHORITY_CEILING,
    SegmentationMethod,
    segment,
)
from blender_vision.v2.authority import AuthorityClass

ArrayU8 = NDArray[np.uint8]
ArrayF32 = NDArray[np.float32]
ArrayF64 = NDArray[np.float64]
ArrayI32 = NDArray[np.int32]


# ---------------------------------------------------------------------------
# Frozen thresholds — single source of truth; digest-bound in the split manifest
# ---------------------------------------------------------------------------

FROZEN_THRESHOLDS: dict[str, float | int] = {
    # Geometry of a proposal
    "min_area": 40,
    "max_regions_per_source": 24,
    # Graph edges
    "overlap_iou_edge": 0.15,
    "appearance_agree_edge": 0.55,
    # Split: elongated / multi-peak silhouettes may be two touching objects
    "split_circularity_max": 0.72,
    "split_aspect_min": 1.55,
    "split_peak_min_sep_px": 8,
    # Merge: adjacent fragments may be one object
    "merge_centroid_dist_scale": 1.8,
    "merge_appearance_min": 0.70,
    "merge_gap_px": 12,
    # Temporal
    "background_threshold": 18,
    "residual_threshold": 12,
    "flow_mag_threshold": 0.8,
    # Point tracks
    "point_cluster_min_points": 3,
    "point_motion_eps": 0.6,
    "point_spatial_eps": 28.0,
    # Dense features
    "edge_dilate_px": 2,
    "local_contrast_percentile": 88.0,
    # Fusion acceptance (declared up front; not tuned on hidden)
    "fused_recall_min": 0.50,
    "first_frame_min_proposals": 3,
    # Confidence calibration bins
    "confidence_floor": 0.15,
    "multi_source_bonus": 0.12,
}


def thresholds_digest(thresholds: Mapping[str, float | int] | None = None) -> str:
    """Canonical SHA-256 of the frozen threshold table."""
    payload = thresholds if thresholds is not None else FROZEN_THRESHOLDS
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


FROZEN_THRESHOLDS_DIGEST = thresholds_digest()


class ProposalSource(StrEnum):
    APPEARANCE = "appearance"
    TEMPORAL_CHANGE = "temporal_change"
    DENSE_FEATURES = "dense_features"
    GEOMETRY = "geometry"
    POINT_TRACK_CLUSTERS = "point_track_clusters"
    UNKNOWN_REGION = "unknown_region"


class ProposalStatus(StrEnum):
    ACTIVE = "active"
    BLOCKED = "blocked"
    DIAGNOSTIC_ONLY = "diagnostic_only"


class HypothesisKind(StrEnum):
    """Structural alternatives that must survive fusion."""

    ATOMIC = "atomic"
    #: Two (or more) touching objects may have been merged into one mask.
    SPLIT = "split"
    #: One object may have been fragmented into several masks.
    MERGE = "merge"


class SourceAvailability(StrEnum):
    AVAILABLE = "available"
    BLOCKED = "blocked"
    EMPTY = "empty"


ALL_SOURCES: tuple[ProposalSource, ...] = (
    ProposalSource.APPEARANCE,
    ProposalSource.TEMPORAL_CHANGE,
    ProposalSource.DENSE_FEATURES,
    ProposalSource.GEOMETRY,
    ProposalSource.POINT_TRACK_CLUSTERS,
    ProposalSource.UNKNOWN_REGION,
)


# ---------------------------------------------------------------------------
# Records
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class RegionProposal:
    """One region hypothesis from one source. Never carries ground-truth IDs."""

    proposal_id: str
    source: ProposalSource
    frame_index: int
    timestamp: float
    bbox_xywh: tuple[float, float, float, float]
    centroid_xy: tuple[float, float]
    area_px: float
    confidence: float
    appearance_embedding: list[float] = field(default_factory=list)
    appearance_hist: list[float] = field(default_factory=list)
    # Compact mask storage: full HxW uint8 when present; fusion may share refs.
    mask: ArrayU8 | None = field(default=None, repr=False)
    geometry: dict[str, Any] = field(default_factory=dict)
    motion: dict[str, Any] = field(default_factory=dict)
    uncertainty: float = 0.5
    limitations: list[str] = field(default_factory=list)
    status: ProposalStatus = ProposalStatus.ACTIVE
    blocked_reason: str = ""
    hypothesis_kind: HypothesisKind = HypothesisKind.ATOMIC
    related_proposal_ids: list[str] = field(default_factory=list)
    supporting_sources: list[str] = field(default_factory=list)
    meta: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "proposal_id": self.proposal_id,
            "source": self.source.value,
            "frame_index": self.frame_index,
            "timestamp": self.timestamp,
            "bbox_xywh": list(self.bbox_xywh),
            "centroid_xy": list(self.centroid_xy),
            "area_px": self.area_px,
            "confidence": self.confidence,
            "appearance_embedding_dim": len(self.appearance_embedding),
            "geometry": dict(self.geometry),
            "motion": dict(self.motion),
            "uncertainty": self.uncertainty,
            "limitations": list(self.limitations),
            "status": self.status.value,
            "blocked_reason": self.blocked_reason,
            "hypothesis_kind": self.hypothesis_kind.value,
            "related_proposal_ids": list(self.related_proposal_ids),
            "supporting_sources": list(self.supporting_sources),
            "meta": {k: v for k, v in self.meta.items() if k != "mask"},
            # Hard contract: no GT may ride along.
            "gt_forbidden": True,
        }


@dataclass(slots=True)
class ProposalEdge:
    """Evidence link between two proposals — never a destructive suppression."""

    a_id: str
    b_id: str
    iou: float
    appearance_agreement: float
    centroid_distance_px: float
    relation: str  # "overlap" | "adjacent" | "appearance" | "split_candidate" | "merge_candidate"


@dataclass(slots=True)
class SourceReport:
    """Per-source measurability receipt for one frame."""

    source: ProposalSource
    availability: SourceAvailability
    n_proposals: int
    blocked_reason: str = ""
    limitations: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "source": self.source.value,
            "availability": self.availability.value,
            "n_proposals": self.n_proposals,
            "blocked_reason": self.blocked_reason,
            "limitations": list(self.limitations),
        }


@dataclass(slots=True)
class RegionProposalGraph:
    """Nodes = proposals; edges carry spatial overlap and appearance agreement.

    Fusion accumulates evidence. Competing split/merge hypotheses are retained
    as first-class nodes so association can resolve them with temporal context.
    """

    frame_index: int
    timestamp: float
    proposals: list[RegionProposal] = field(default_factory=list)
    edges: list[ProposalEdge] = field(default_factory=list)
    source_reports: list[SourceReport] = field(default_factory=list)
    fused: list[RegionProposal] = field(default_factory=list)
    split_hypotheses: list[RegionProposal] = field(default_factory=list)
    merge_hypotheses: list[RegionProposal] = field(default_factory=list)
    thresholds_digest: str = FROZEN_THRESHOLDS_DIGEST
    limitations: list[str] = field(default_factory=list)

    def active_proposals(self) -> list[RegionProposal]:
        return [p for p in self.proposals if p.status is ProposalStatus.ACTIVE]

    def proposals_for_source(self, source: ProposalSource) -> list[RegionProposal]:
        return [p for p in self.proposals if p.source is source]

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_index": self.frame_index,
            "timestamp": self.timestamp,
            "thresholds_digest": self.thresholds_digest,
            "n_raw_proposals": len(self.proposals),
            "n_fused": len(self.fused),
            "n_split_hypotheses": len(self.split_hypotheses),
            "n_merge_hypotheses": len(self.merge_hypotheses),
            "n_edges": len(self.edges),
            "source_reports": [r.to_dict() for r in self.source_reports],
            "proposals": [p.to_dict() for p in self.proposals],
            "fused": [p.to_dict() for p in self.fused],
            "split_hypotheses": [p.to_dict() for p in self.split_hypotheses],
            "merge_hypotheses": [p.to_dict() for p in self.merge_hypotheses],
            "limitations": list(self.limitations),
        }


@dataclass(slots=True)
class ProposalConfig:
    """Declared knobs; defaults match FROZEN_THRESHOLDS."""

    min_area: int = int(FROZEN_THRESHOLDS["min_area"])
    max_regions_per_source: int = int(FROZEN_THRESHOLDS["max_regions_per_source"])
    background_threshold: int = int(FROZEN_THRESHOLDS["background_threshold"])
    residual_threshold: int = int(FROZEN_THRESHOLDS["residual_threshold"])
    flow_mag_threshold: float = float(FROZEN_THRESHOLDS["flow_mag_threshold"])
    # When True, geometry invents nothing — BLOCKED without a depth map.
    require_depth_for_geometry: bool = True


# ---------------------------------------------------------------------------
# Image helpers
# ---------------------------------------------------------------------------


def _as_bgr(image: NDArray[Any]) -> ArrayU8:
    if image.ndim == 2:
        return cv2.cvtColor(image.astype(np.uint8), cv2.COLOR_GRAY2BGR)
    if image.ndim != 3 or image.shape[2] not in (3, 4):
        raise ValidationError(f"expected HxW or HxWx3/4 image, got shape {image.shape}")
    arr = image[:, :, :3]
    if arr.dtype != np.uint8:
        arr = np.clip(arr, 0, 255).astype(np.uint8)
    return np.ascontiguousarray(arr)


def _mask_bbox(mask: ArrayU8) -> tuple[float, float, float, float]:
    ys, xs = np.where(mask > 0)
    if len(xs) == 0:
        return (0.0, 0.0, 0.0, 0.0)
    x0, x1 = int(xs.min()), int(xs.max())
    y0, y1 = int(ys.min()), int(ys.max())
    return (float(x0), float(y0), float(x1 - x0 + 1), float(y1 - y0 + 1))


def _mask_centroid(mask: ArrayU8) -> tuple[float, float]:
    ys, xs = np.where(mask > 0)
    if len(xs) == 0:
        return (0.0, 0.0)
    return (float(xs.mean()), float(ys.mean()))


def _bbox_iou(
    a: tuple[float, float, float, float], b: tuple[float, float, float, float]
) -> float:
    ax, ay, aw, ah = a
    bx, by, bw, bh = b
    ax1, ay1 = ax + aw, ay + ah
    bx1, by1 = bx + bw, by + bh
    ix0, iy0 = max(ax, bx), max(ay, by)
    ix1, iy1 = min(ax1, bx1), min(ay1, by1)
    iw, ih = max(0.0, ix1 - ix0), max(0.0, iy1 - iy0)
    inter = iw * ih
    if inter <= 0.0:
        return 0.0
    union = aw * ah + bw * bh - inter
    return float(inter / union) if union > 0 else 0.0


def _mask_iou(a: ArrayU8, b: ArrayU8) -> float:
    inter = int(np.logical_and(a > 0, b > 0).sum())
    if inter <= 0:
        return 0.0
    union = int(np.logical_or(a > 0, b > 0).sum())
    return float(inter / union) if union > 0 else 0.0


def _circularity(mask: ArrayU8) -> float:
    area = float((mask > 0).sum())
    if area <= 0:
        return 0.0
    contours, _ = cv2.findContours(mask, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    if not contours:
        return 0.0
    peri = float(cv2.arcLength(contours[0], True))
    if peri <= 1e-6:
        return 0.0
    return float(4.0 * np.pi * area / (peri * peri))


def _aspect_ratio(bbox: tuple[float, float, float, float]) -> float:
    _, _, w, h = bbox
    if w <= 0 or h <= 0:
        return 1.0
    return float(max(w, h) / max(1e-6, min(w, h)))


def _components_from_binary(
    binary: ArrayU8,
    *,
    min_area: int,
    max_regions: int,
) -> list[ArrayU8]:
    m = (binary > 0).astype(np.uint8)
    if int(m.sum()) == 0:
        return []
    count, labels, stats, _ = cv2.connectedComponentsWithStats(m, 8)
    order = sorted(
        range(1, count), key=lambda i: int(stats[i, cv2.CC_STAT_AREA]), reverse=True
    )
    masks: list[ArrayU8] = []
    for label in order[:max_regions]:
        area = int(stats[label, cv2.CC_STAT_AREA])
        if area < min_area:
            continue
        masks.append((labels == label).astype(np.uint8))
    return masks


def _proposal_from_mask(
    image_bgr: ArrayU8,
    mask: ArrayU8,
    *,
    source: ProposalSource,
    frame_index: int,
    timestamp: float,
    proposal_id: str,
    confidence: float,
    limitations: Sequence[str] | None = None,
    motion: dict[str, Any] | None = None,
    geometry: dict[str, Any] | None = None,
    hypothesis_kind: HypothesisKind = HypothesisKind.ATOMIC,
    related: Sequence[str] | None = None,
    meta: dict[str, Any] | None = None,
) -> RegionProposal | None:
    m = (mask > 0).astype(np.uint8)
    area = int(m.sum())
    if area <= 0:
        return None
    emb = appearance_embedding(image_bgr, m)
    hist = [float(v) for v in colour_histogram_lab(image_bgr, m)]
    return RegionProposal(
        proposal_id=proposal_id,
        source=source,
        frame_index=frame_index,
        timestamp=timestamp,
        bbox_xywh=_mask_bbox(m),
        centroid_xy=_mask_centroid(m),
        area_px=float(area),
        confidence=float(np.clip(confidence, 0.0, 1.0)),
        appearance_embedding=emb,
        appearance_hist=hist,
        mask=m,
        geometry=dict(geometry or {}),
        motion=dict(motion or {}),
        uncertainty=float(np.clip(1.0 - confidence, 0.05, 1.0)),
        limitations=list(limitations or []),
        hypothesis_kind=hypothesis_kind,
        related_proposal_ids=list(related or []),
        supporting_sources=[source.value],
        meta=dict(meta or {}),
    )


# ---------------------------------------------------------------------------
# Source A — appearance / objectness (single-frame; must work on frame 0)
# ---------------------------------------------------------------------------


def _saliency_map(image_bgr: ArrayU8) -> ArrayF32:
    """Classical spectral residual saliency (Hou & Zhang) — no learned weights."""
    gray = cv2.cvtColor(image_bgr, cv2.COLOR_BGR2GRAY).astype(np.float32)
    # Spectral residual on log amplitude of the DFT.
    h, w = gray.shape
    # Prefer power-of-two padding for stable DFT; crop back.
    gh = cv2.getOptimalDFTSize(h)
    gw = cv2.getOptimalDFTSize(w)
    padded = cv2.copyMakeBorder(gray, 0, gh - h, 0, gw - w, cv2.BORDER_REPLICATE)
    dft = cv2.dft(padded, flags=cv2.DFT_COMPLEX_OUTPUT)
    mag, ang = cv2.cartToPolar(dft[:, :, 0], dft[:, :, 1])
    log_mag = cv2.log(mag + 1e-6)
    smooth = cv2.blur(log_mag, (3, 3))
    residual = log_mag - smooth
    real, imag = cv2.polarToCart(cv2.exp(residual), ang)
    back = cv2.merge([real, imag])
    inv = cv2.dft(back, flags=cv2.DFT_INVERSE | cv2.DFT_SCALE | cv2.DFT_REAL_OUTPUT)
    sal = inv[:h, :w]
    sal = cv2.GaussianBlur(sal * sal, (9, 9), 0)
    sal = cv2.normalize(sal, None, 0.0, 1.0, cv2.NORM_MINMAX).astype(np.float32)
    return sal


def _local_contrast_map(image_bgr: ArrayU8) -> ArrayF32:
    gray = cv2.cvtColor(image_bgr, cv2.COLOR_BGR2GRAY).astype(np.float32)
    blur = cv2.GaussianBlur(gray, (21, 21), 0)
    contrast = np.abs(gray - blur)
    return cv2.normalize(contrast, None, 0.0, 1.0, cv2.NORM_MINMAX).astype(np.float32)


def _dog_blob_markers(image_bgr: ArrayU8, *, min_area: int) -> list[ArrayU8]:
    """Difference-of-Gaussians peaks → blob masks for disk-like objects."""
    gray = cv2.cvtColor(image_bgr, cv2.COLOR_BGR2GRAY).astype(np.float32)
    g1 = cv2.GaussianBlur(gray, (0, 0), 1.5)
    g2 = cv2.GaussianBlur(gray, (0, 0), 4.0)
    dog = np.abs(g1 - g2)
    dog_u8 = cv2.normalize(dog, None, 0, 255, cv2.NORM_MINMAX).astype(np.uint8)
    thr = max(12, int(np.percentile(dog_u8, 92)))
    _, binary = cv2.threshold(dog_u8, thr, 255, cv2.THRESH_BINARY)
    binary = cv2.morphologyEx(binary, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
    return _components_from_binary(
        binary,
        min_area=min_area,
        max_regions=int(FROZEN_THRESHOLDS["max_regions_per_source"]),
    )


def _adaptive_fg_masks(image_bgr: ArrayU8, *, min_area: int) -> list[ArrayU8]:
    gray = cv2.cvtColor(image_bgr, cv2.COLOR_BGR2GRAY)
    blur = cv2.GaussianBlur(gray, (5, 5), 0)
    # Objects may be brighter or darker than the surround; try both polarities.
    masks: list[ArrayU8] = []
    for invert in (False, True):
        src = cv2.bitwise_not(blur) if invert else blur
        thr = cv2.adaptiveThreshold(
            src, 255, cv2.ADAPTIVE_THRESH_GAUSSIAN_C, cv2.THRESH_BINARY, 31, -4
        )
        thr = cv2.morphologyEx(thr, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
        thr = cv2.morphologyEx(thr, cv2.MORPH_CLOSE, np.ones((5, 5), np.uint8))
        masks.extend(
            _components_from_binary(
                thr,
                min_area=min_area,
                max_regions=int(FROZEN_THRESHOLDS["max_regions_per_source"]),
            )
        )
    # Otsu polarity-aware (works when object albedo differs from table).
    _, otsu = cv2.threshold(blur, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
    if float(blur[otsu == 0].mean() if np.any(otsu == 0) else 0) > float(
        blur[otsu > 0].mean() if np.any(otsu > 0) else 0
    ):
        otsu = cv2.bitwise_not(otsu)
    masks.extend(
        _components_from_binary(
            otsu,
            min_area=min_area,
            max_regions=int(FROZEN_THRESHOLDS["max_regions_per_source"]),
        )
    )
    return masks


def _watershed_masks(image_bgr: ArrayU8, *, min_area: int, max_regions: int) -> list[ArrayU8]:
    result, labels = segment(
        image_bgr,
        method=SegmentationMethod.WATERSHED,
        min_area=min_area,
        max_regions=max_regions,
    )
    if labels is None:
        return []
    masks: list[ArrayU8] = []
    for inst in result.instances:
        masks.append((labels == inst.label).astype(np.uint8))
    return masks


def _grabcut_from_saliency(
    image_bgr: ArrayU8, saliency: ArrayF32, *, min_area: int
) -> list[ArrayU8]:
    """Seed GrabCut boxes from saliency peaks; classical, no learned weights."""
    sal_u8 = (np.clip(saliency, 0, 1) * 255).astype(np.uint8)
    thr = max(30, int(np.percentile(sal_u8, 90)))
    _, binary = cv2.threshold(sal_u8, thr, 255, cv2.THRESH_BINARY)
    binary = cv2.morphologyEx(binary, cv2.MORPH_CLOSE, np.ones((7, 7), np.uint8))
    boxes = _components_from_binary(
        binary, min_area=max(min_area, 20), max_regions=3
    )
    out: list[ArrayU8] = []
    h, w = image_bgr.shape[:2]
    for seed_mask in boxes:
        x, y, bw, bh = [int(v) for v in _mask_bbox(seed_mask)]
        # Expand box slightly so GrabCut has background context.
        pad = 6
        x0 = max(0, x - pad)
        y0 = max(0, y - pad)
        x1 = min(w, x + bw + pad)
        y1 = min(h, y + bh + pad)
        if x1 - x0 < 4 or y1 - y0 < 4:
            continue
        try:
            result, labels = segment(
                image_bgr,
                method=SegmentationMethod.GRABCUT,
                box=(x0, y0, x1 - x0, y1 - y0),
                min_area=min_area,
            )
        except ValidationError:
            continue
        if labels is None:
            continue
        for inst in result.instances:
            out.append((labels == inst.label).astype(np.uint8))
    return out


def _dedupe_masks(masks: Sequence[ArrayU8], *, iou_thresh: float = 0.6) -> list[ArrayU8]:
    """Within-source dedupe only. Cross-source fusion never drops competitors."""
    kept: list[ArrayU8] = []
    for mask in sorted(masks, key=lambda m: int(m.sum()), reverse=True):
        if int(mask.sum()) == 0:
            continue
        duplicate = False
        for existing in kept:
            if _mask_iou(mask, existing) >= iou_thresh:
                duplicate = True
                break
        if not duplicate:
            kept.append(mask)
    return kept


def propose_appearance(
    image: NDArray[Any],
    *,
    frame_index: int = 0,
    timestamp: float = 0.0,
    config: ProposalConfig | None = None,
) -> tuple[list[RegionProposal], SourceReport]:
    """Source A: single-frame objectness. Must find stationary objects on frame 0.

    Sub-methods are cheap classical operators. GrabCut runs only when the
    primary set is still thin (low-contrast scenes), because it is expensive
    and would otherwise dominate the runtime budget.
    """
    cfg = config or ProposalConfig()
    bgr = _as_bgr(image)
    sal = _saliency_map(bgr)
    contrast = _local_contrast_map(bgr)

    raw_masks: list[ArrayU8] = []
    # Primary: watershed + polarity-aware Otsu + DoG blobs. These three cover
    # the textured synthetic fixtures and most EEVEE hard scenes.
    raw_masks.extend(
        _watershed_masks(
            bgr, min_area=cfg.min_area, max_regions=cfg.max_regions_per_source
        )
    )
    gray = cv2.cvtColor(bgr, cv2.COLOR_BGR2GRAY)
    blur = cv2.GaussianBlur(gray, (5, 5), 0)
    _, otsu = cv2.threshold(blur, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
    if float(blur[otsu == 0].mean() if np.any(otsu == 0) else 0) > float(
        blur[otsu > 0].mean() if np.any(otsu > 0) else 0
    ):
        otsu = cv2.bitwise_not(otsu)
    raw_masks.extend(
        _components_from_binary(
            otsu, min_area=cfg.min_area, max_regions=cfg.max_regions_per_source
        )
    )
    raw_masks.extend(_dog_blob_markers(bgr, min_area=cfg.min_area))

    # Adaptive FG only if primary is thin — white-on-white needs it.
    primary = _dedupe_masks(raw_masks, iou_thresh=0.5)
    if len(primary) < 3:
        raw_masks.extend(_adaptive_fg_masks(bgr, min_area=cfg.min_area))
        primary = _dedupe_masks(raw_masks, iou_thresh=0.5)

    # Contrast region-grow seeds (cheap) when still under-count.
    if len(primary) < 3:
        contrast_u8 = (contrast * 255).astype(np.uint8)
        thr = max(
            20,
            int(
                np.percentile(
                    contrast_u8, float(FROZEN_THRESHOLDS["local_contrast_percentile"])
                )
            ),
        )
        _, peaks = cv2.threshold(contrast_u8, thr, 255, cv2.THRESH_BINARY)
        peak_components = _components_from_binary(peaks, min_area=9, max_regions=8)
        seeds = [
            (int(round(cx)), int(round(cy)))
            for pm in peak_components
            for cx, cy in [_mask_centroid(pm)]
        ]
        if seeds:
            try:
                result, labels = segment(
                    bgr,
                    method=SegmentationMethod.REGION_GROW,
                    seeds=seeds[:8],
                    min_area=cfg.min_area,
                    colour_radius=18.0,
                )
                if labels is not None:
                    for inst in result.instances:
                        raw_masks.append((labels == inst.label).astype(np.uint8))
            except ValidationError:
                pass
        primary = _dedupe_masks(raw_masks, iou_thresh=0.5)

    # GrabCut last resort (expensive) — cap seeds at 3.
    if len(primary) < 3:
        raw_masks.extend(
            _grabcut_from_saliency(bgr, sal, min_area=cfg.min_area)[:3]
        )

    masks = _dedupe_masks(raw_masks, iou_thresh=0.5)[: cfg.max_regions_per_source]
    proposals: list[RegionProposal] = []
    for i, mask in enumerate(masks):
        m = mask > 0
        sal_score = float(sal[m].mean()) if np.any(m) else 0.0
        con_score = float(contrast[m].mean()) if np.any(m) else 0.0
        conf = float(np.clip(0.35 + 0.40 * sal_score + 0.25 * con_score, 0.15, 0.95))
        prop = _proposal_from_mask(
            bgr,
            mask,
            source=ProposalSource.APPEARANCE,
            frame_index=frame_index,
            timestamp=timestamp,
            proposal_id=f"A-{frame_index}-{i}",
            confidence=conf,
            limitations=["single-frame classical objectness; no temporal evidence"],
            meta={
                "submethods": [
                    "watershed",
                    "otsu",
                    "dog",
                    "adaptive?",
                    "region_grow?",
                    "grabcut?",
                ]
            },
        )
        if prop is not None:
            proposals.append(prop)

    report = SourceReport(
        source=ProposalSource.APPEARANCE,
        availability=(
            SourceAvailability.AVAILABLE if proposals else SourceAvailability.EMPTY
        ),
        n_proposals=len(proposals),
        limitations=["classical only; no learned objectness network"],
    )
    return proposals, report


# ---------------------------------------------------------------------------
# Source B — temporal change
# ---------------------------------------------------------------------------


def propose_temporal_change(
    image: NDArray[Any],
    *,
    previous_image: NDArray[Any] | None = None,
    background_model: BackgroundModel | None = None,
    frame_index: int = 0,
    timestamp: float = 0.0,
    config: ProposalConfig | None = None,
) -> tuple[list[RegionProposal], SourceReport]:
    """Source B: background residual, frame difference, flow residual."""
    cfg = config or ProposalConfig()
    bgr = _as_bgr(image)
    masks: list[ArrayU8] = []
    limitations: list[str] = []

    if background_model is not None:
        fg = background_model.foreground_mask(bgr)
        masks.extend(
            _components_from_binary(
                fg, min_area=cfg.min_area, max_regions=cfg.max_regions_per_source
            )
        )
    else:
        limitations.append("no BackgroundModel supplied")

    if previous_image is not None:
        prev = _as_bgr(previous_image)
        gray = cv2.cvtColor(bgr, cv2.COLOR_BGR2GRAY)
        prev_gray = cv2.cvtColor(prev, cv2.COLOR_BGR2GRAY)
        residual = cv2.absdiff(gray, prev_gray)
        _, binary = cv2.threshold(
            residual, cfg.residual_threshold, 255, cv2.THRESH_BINARY
        )
        binary = cv2.morphologyEx(binary, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
        binary = cv2.morphologyEx(binary, cv2.MORPH_CLOSE, np.ones((5, 5), np.uint8))
        masks.extend(
            _components_from_binary(
                binary, min_area=cfg.min_area, max_regions=cfg.max_regions_per_source
            )
        )
        # Dense flow residual after global affine removal (object motion).
        flow = cv2.calcOpticalFlowFarneback(
            prev_gray, gray, None, 0.5, 3, 15, 3, 5, 1.2, 0
        )
        mag = np.sqrt(flow[..., 0] ** 2 + flow[..., 1] ** 2)
        moving = (mag > cfg.flow_mag_threshold).astype(np.uint8)
        moving = cv2.morphologyEx(moving, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
        masks.extend(
            _components_from_binary(
                moving, min_area=cfg.min_area, max_regions=cfg.max_regions_per_source
            )
        )
    else:
        limitations.append("no previous_image; frame-difference and flow unavailable")

    if background_model is None and previous_image is None:
        report = SourceReport(
            source=ProposalSource.TEMPORAL_CHANGE,
            availability=SourceAvailability.BLOCKED,
            n_proposals=0,
            blocked_reason=(
                "temporal change requires previous_image or BackgroundModel; "
                "neither was supplied"
            ),
            limitations=limitations,
        )
        return [], report

    masks = _dedupe_masks(masks)[: cfg.max_regions_per_source]
    proposals: list[RegionProposal] = []
    for i, mask in enumerate(masks):
        prop = _proposal_from_mask(
            bgr,
            mask,
            source=ProposalSource.TEMPORAL_CHANGE,
            frame_index=frame_index,
            timestamp=timestamp,
            proposal_id=f"B-{frame_index}-{i}",
            confidence=0.70,
            limitations=list(limitations),
            motion={"kind": "temporal_residual"},
        )
        if prop is not None:
            proposals.append(prop)

    report = SourceReport(
        source=ProposalSource.TEMPORAL_CHANGE,
        availability=(
            SourceAvailability.AVAILABLE if proposals else SourceAvailability.EMPTY
        ),
        n_proposals=len(proposals),
        limitations=limitations,
    )
    return proposals, report


# ---------------------------------------------------------------------------
# Source C — dense visual features (boundaries + embeddings)
# ---------------------------------------------------------------------------


def propose_dense_features(
    image: NDArray[Any],
    *,
    frame_index: int = 0,
    timestamp: float = 0.0,
    config: ProposalConfig | None = None,
) -> tuple[list[RegionProposal], SourceReport]:
    """Source C: edge-enclosed regions with dense appearance embeddings."""
    cfg = config or ProposalConfig()
    bgr = _as_bgr(image)
    gray = cv2.cvtColor(bgr, cv2.COLOR_BGR2GRAY)
    edges = cv2.Canny(gray, 40, 120)
    dilate = int(FROZEN_THRESHOLDS["edge_dilate_px"])
    if dilate > 0:
        edges = cv2.dilate(edges, np.ones((dilate, dilate), np.uint8))
    # Invert: interior of edge-bounded regions.
    filled = cv2.bitwise_not(edges)
    # Drop the huge background component by removing border-connected regions.
    h, w = filled.shape
    flood = filled.copy()
    ff_mask = np.zeros((h + 2, w + 2), dtype=np.uint8)
    cv2.floodFill(flood, ff_mask, (0, 0), 0)
    masks = _components_from_binary(
        flood, min_area=cfg.min_area, max_regions=min(12, cfg.max_regions_per_source)
    )
    # Texture islands from gradient magnitude (capped — dense features support
    # embeddings for re-id, not exhaustive oversegmentation).
    gx = cv2.Sobel(gray, cv2.CV_32F, 1, 0, ksize=3)
    gy = cv2.Sobel(gray, cv2.CV_32F, 0, 1, ksize=3)
    mag = cv2.magnitude(gx, gy)
    mag_u8 = cv2.normalize(mag, None, 0, 255, cv2.NORM_MINMAX).astype(np.uint8)
    _, strong = cv2.threshold(
        mag_u8, max(20, int(np.percentile(mag_u8, 90))), 255, cv2.THRESH_BINARY
    )
    strong = cv2.morphologyEx(strong, cv2.MORPH_CLOSE, np.ones((5, 5), np.uint8))
    masks.extend(
        _components_from_binary(strong, min_area=cfg.min_area, max_regions=8)
    )

    masks = _dedupe_masks(masks, iou_thresh=0.5)[: min(12, cfg.max_regions_per_source)]
    proposals: list[RegionProposal] = []
    for i, mask in enumerate(masks):
        # Embedding is the load-bearing dense feature for re-id.
        prop = _proposal_from_mask(
            bgr,
            mask,
            source=ProposalSource.DENSE_FEATURES,
            frame_index=frame_index,
            timestamp=timestamp,
            proposal_id=f"C-{frame_index}-{i}",
            confidence=0.55,
            limitations=[
                "classical edges + Lab/HOG/shape embedding; no learned backbone"
            ],
            meta={"embedding_blocks": ["lab_hist", "oriented_gradients", "shape_moments"]},
        )
        if prop is not None:
            proposals.append(prop)

    report = SourceReport(
        source=ProposalSource.DENSE_FEATURES,
        availability=(
            SourceAvailability.AVAILABLE if proposals else SourceAvailability.EMPTY
        ),
        n_proposals=len(proposals),
        limitations=["dense features are classical multi-block embeddings only"],
    )
    return proposals, report


# ---------------------------------------------------------------------------
# Source D — geometry (depth / normals). BLOCKED when unavailable.
# ---------------------------------------------------------------------------


def propose_geometry(
    image: NDArray[Any],
    *,
    depth: NDArray[Any] | None = None,
    normals: NDArray[Any] | None = None,
    frame_index: int = 0,
    timestamp: float = 0.0,
    config: ProposalConfig | None = None,
) -> tuple[list[RegionProposal], SourceReport]:
    """Source D: depth and normal discontinuities.

    When no depth pass is available the source is BLOCKED — never invent depth.
    """
    cfg = config or ProposalConfig()
    bgr = _as_bgr(image)
    if depth is None and normals is None:
        report = SourceReport(
            source=ProposalSource.GEOMETRY,
            availability=SourceAvailability.BLOCKED,
            n_proposals=0,
            blocked_reason=(
                "geometry proposals require a depth or normal pass; none supplied. "
                "Depth is not invented from RGB."
            ),
            limitations=["no monocular depth estimator; physical Z-pass only"],
        )
        # Explicit blocked sentinel so callers can measure the source.
        blocked = RegionProposal(
            proposal_id=f"D-{frame_index}-blocked",
            source=ProposalSource.GEOMETRY,
            frame_index=frame_index,
            timestamp=timestamp,
            bbox_xywh=(0.0, 0.0, 0.0, 0.0),
            centroid_xy=(0.0, 0.0),
            area_px=0.0,
            confidence=0.0,
            status=ProposalStatus.BLOCKED,
            blocked_reason=report.blocked_reason,
            limitations=list(report.limitations),
            uncertainty=1.0,
            supporting_sources=[ProposalSource.GEOMETRY.value],
        )
        return [blocked], report

    masks: list[ArrayU8] = []
    limitations: list[str] = []
    if depth is not None:
        d = np.asarray(depth)
        if d.ndim == 3:
            d = d[..., 0]
        d = d.astype(np.float32)
        # Finite-difference depth edges.
        gx = cv2.Sobel(d, cv2.CV_32F, 1, 0, ksize=3)
        gy = cv2.Sobel(d, cv2.CV_32F, 0, 1, ksize=3)
        grad = cv2.magnitude(gx, gy)
        finite = grad[np.isfinite(grad)]
        thr = float(np.percentile(finite, 90)) if finite.size else 0.0
        edge = (grad > thr).astype(np.uint8)
        # Regions of relatively constant depth (inverse of edges).
        plateaus = cv2.bitwise_not(edge * 255)
        masks.extend(
            _components_from_binary(
                plateaus, min_area=cfg.min_area, max_regions=cfg.max_regions_per_source
            )
        )
    if normals is not None:
        n = np.asarray(normals)
        if n.ndim == 3 and n.shape[2] >= 3:
            # Angular discontinuity via finite differences of unit normals.
            nx = n[:, :, 0].astype(np.float32)
            ny = n[:, :, 1].astype(np.float32)
            nz = n[:, :, 2].astype(np.float32)
            dn = (
                np.abs(cv2.Sobel(nx, cv2.CV_32F, 1, 0, ksize=3))
                + np.abs(cv2.Sobel(ny, cv2.CV_32F, 1, 0, ksize=3))
                + np.abs(cv2.Sobel(nz, cv2.CV_32F, 1, 0, ksize=3))
            )
            thr = float(np.percentile(dn, 92))
            edge = (dn > thr).astype(np.uint8) * 255
            interior = cv2.bitwise_not(edge)
            masks.extend(
                _components_from_binary(
                    interior,
                    min_area=cfg.min_area,
                    max_regions=cfg.max_regions_per_source,
                )
            )
        else:
            limitations.append("normals array must be HxWx3")

    masks = _dedupe_masks(masks)[: cfg.max_regions_per_source]
    proposals: list[RegionProposal] = []
    for i, mask in enumerate(masks):
        geom: dict[str, Any] = {}
        if depth is not None:
            d = np.asarray(depth)
            if d.ndim == 3:
                d = d[..., 0]
            vals = d[mask > 0]
            vals = vals[np.isfinite(vals)]
            if vals.size:
                geom = {
                    "depth_mean": float(vals.mean()),
                    "depth_std": float(vals.std()),
                    "depth_min": float(vals.min()),
                    "depth_max": float(vals.max()),
                }
        prop = _proposal_from_mask(
            bgr,
            mask,
            source=ProposalSource.GEOMETRY,
            frame_index=frame_index,
            timestamp=timestamp,
            proposal_id=f"D-{frame_index}-{i}",
            confidence=0.75,
            geometry=geom,
            limitations=limitations or ["depth/normal discontinuity regions"],
        )
        if prop is not None:
            proposals.append(prop)

    report = SourceReport(
        source=ProposalSource.GEOMETRY,
        availability=(
            SourceAvailability.AVAILABLE if proposals else SourceAvailability.EMPTY
        ),
        n_proposals=len(proposals),
        limitations=limitations,
    )
    return proposals, report


# ---------------------------------------------------------------------------
# Source E — point-track clusters
# ---------------------------------------------------------------------------


def propose_point_track_clusters(
    image: NDArray[Any],
    *,
    previous_image: NDArray[Any] | None = None,
    prev_points: ArrayF32 | None = None,
    frame_index: int = 0,
    timestamp: float = 0.0,
    config: ProposalConfig | None = None,
) -> tuple[list[RegionProposal], SourceReport, ArrayF32 | None]:
    """Source E: sparse LK point tracks clustered by common motion.

    Returns (proposals, report, current_points) so the caller can carry points
    across frames for temporarily occluded objects that reappear.
    """
    cfg = config or ProposalConfig()
    bgr = _as_bgr(image)
    gray = cv2.cvtColor(bgr, cv2.COLOR_BGR2GRAY)

    if previous_image is None:
        # Seed points on the first frame so the next frame can form clusters.
        pts = cv2.goodFeaturesToTrack(
            gray, maxCorners=200, qualityLevel=0.01, minDistance=6, blockSize=5
        )
        report = SourceReport(
            source=ProposalSource.POINT_TRACK_CLUSTERS,
            availability=SourceAvailability.BLOCKED,
            n_proposals=0,
            blocked_reason=(
                "point-track clusters require a previous frame for Lucas-Kanade; "
                "seeded features only on this frame"
            ),
            limitations=["seeded goodFeaturesToTrack; motion clusters form on frame N>=1"],
        )
        current = pts.reshape(-1, 1, 2).astype(np.float32) if pts is not None else None
        blocked = RegionProposal(
            proposal_id=f"E-{frame_index}-blocked",
            source=ProposalSource.POINT_TRACK_CLUSTERS,
            frame_index=frame_index,
            timestamp=timestamp,
            bbox_xywh=(0.0, 0.0, 0.0, 0.0),
            centroid_xy=(0.0, 0.0),
            area_px=0.0,
            confidence=0.0,
            status=ProposalStatus.BLOCKED,
            blocked_reason=report.blocked_reason,
            limitations=list(report.limitations),
            uncertainty=1.0,
            supporting_sources=[ProposalSource.POINT_TRACK_CLUSTERS.value],
        )
        return [blocked], report, current

    prev_gray = cv2.cvtColor(_as_bgr(previous_image), cv2.COLOR_BGR2GRAY)
    if prev_points is None or len(prev_points) == 0:
        prev_points = cv2.goodFeaturesToTrack(
            prev_gray, maxCorners=200, qualityLevel=0.01, minDistance=6, blockSize=5
        )
    if prev_points is None or len(prev_points) == 0:
        report = SourceReport(
            source=ProposalSource.POINT_TRACK_CLUSTERS,
            availability=SourceAvailability.EMPTY,
            n_proposals=0,
            limitations=["no trackable corners found"],
        )
        return [], report, None

    prev_pts = prev_points.reshape(-1, 1, 2).astype(np.float32)
    nxt, status, _err = cv2.calcOpticalFlowPyrLK(
        prev_gray,
        gray,
        prev_pts,
        None,
        winSize=(21, 21),
        maxLevel=3,
        criteria=(cv2.TERM_CRITERIA_EPS | cv2.TERM_CRITERIA_COUNT, 30, 0.01),
    )
    if nxt is None or status is None:
        report = SourceReport(
            source=ProposalSource.POINT_TRACK_CLUSTERS,
            availability=SourceAvailability.EMPTY,
            n_proposals=0,
            limitations=["Lucas-Kanade returned no tracks"],
        )
        return [], report, None

    good_prev = prev_pts[status.ravel() == 1].reshape(-1, 2)
    good_next = nxt[status.ravel() == 1].reshape(-1, 2)
    if len(good_next) < int(FROZEN_THRESHOLDS["point_cluster_min_points"]):
        report = SourceReport(
            source=ProposalSource.POINT_TRACK_CLUSTERS,
            availability=SourceAvailability.EMPTY,
            n_proposals=0,
            limitations=["too few surviving tracks for clustering"],
        )
        return [], report, good_next.reshape(-1, 1, 2).astype(np.float32)

    motion = good_next - good_prev
    # Simple agglomerative clustering on (x, y, vx, vy) with fixed eps.
    spatial_eps = float(FROZEN_THRESHOLDS["point_spatial_eps"])
    motion_eps = float(FROZEN_THRESHOLDS["point_motion_eps"])
    min_pts = int(FROZEN_THRESHOLDS["point_cluster_min_points"])
    n = len(good_next)
    assigned = np.full(n, -1, dtype=np.int32)
    clusters: list[list[int]] = []
    for i in range(n):
        if assigned[i] >= 0:
            continue
        # Seed a new cluster.
        members = [i]
        assigned[i] = len(clusters)
        changed = True
        while changed:
            changed = False
            for j in range(n):
                if assigned[j] >= 0:
                    continue
                # Compare to cluster mean.
                idxs = np.asarray(members)
                mean_xy = good_next[idxs].mean(axis=0)
                mean_v = motion[idxs].mean(axis=0)
                if (
                    float(np.linalg.norm(good_next[j] - mean_xy)) <= spatial_eps
                    and float(np.linalg.norm(motion[j] - mean_v)) <= motion_eps
                ):
                    assigned[j] = assigned[i]
                    members.append(j)
                    changed = True
        clusters.append(members)

    proposals: list[RegionProposal] = []
    h, w = gray.shape
    for ci, members in enumerate(clusters):
        if len(members) < min_pts:
            continue
        pts = good_next[np.asarray(members)]
        vels = motion[np.asarray(members)]
        # Build a loose mask around the cluster convex hull.
        pts_i = pts.astype(np.int32).reshape(-1, 1, 2)
        mask = np.zeros((h, w), dtype=np.uint8)
        if len(pts_i) >= 3:
            hull = cv2.convexHull(pts_i)
            cv2.fillConvexPoly(mask, hull, 1)
        else:
            for x, y in pts:
                cv2.circle(mask, (int(x), int(y)), 8, 1, -1)
        if int(mask.sum()) < cfg.min_area:
            # Dilate sparse clusters so they clear min_area when possible.
            mask = cv2.dilate(mask, np.ones((9, 9), np.uint8))
        if int(mask.sum()) < cfg.min_area:
            continue
        mean_v = vels.mean(axis=0)
        prop = _proposal_from_mask(
            bgr,
            mask,
            source=ProposalSource.POINT_TRACK_CLUSTERS,
            frame_index=frame_index,
            timestamp=timestamp,
            proposal_id=f"E-{frame_index}-{ci}",
            confidence=float(np.clip(0.40 + 0.05 * len(members), 0.4, 0.9)),
            motion={
                "n_points": len(members),
                "mean_vx": float(mean_v[0]),
                "mean_vy": float(mean_v[1]),
                "speed": float(np.linalg.norm(mean_v)),
            },
            limitations=["sparse LK clusters; supports reappearing points after occlusion"],
        )
        if prop is not None:
            proposals.append(prop)

    # Refresh features for the next frame.
    refreshed = cv2.goodFeaturesToTrack(
        gray, maxCorners=200, qualityLevel=0.01, minDistance=6, blockSize=5
    )
    current_pts = (
        refreshed.reshape(-1, 1, 2).astype(np.float32) if refreshed is not None else None
    )

    report = SourceReport(
        source=ProposalSource.POINT_TRACK_CLUSTERS,
        availability=(
            SourceAvailability.AVAILABLE if proposals else SourceAvailability.EMPTY
        ),
        n_proposals=len(proposals),
        limitations=["classical goodFeaturesToTrack + Lucas-Kanade only"],
    )
    return proposals, report, current_pts


# ---------------------------------------------------------------------------
# Source F — unknown-region proposals
# ---------------------------------------------------------------------------


def propose_unknown_regions(
    image: NDArray[Any],
    *,
    candidate_masks: Sequence[ArrayU8] | None = None,
    known_masks: Sequence[ArrayU8] | None = None,
    known_embeddings: Sequence[Sequence[float]] | None = None,
    frame_index: int = 0,
    timestamp: float = 0.0,
    config: ProposalConfig | None = None,
) -> tuple[list[RegionProposal], SourceReport]:
    """Source F: coherent regions unexplained by current tracks or known concepts.

    This is the source that must produce NEW_UNKNOWN_REGION true positives.
    A region is unknown when it has low overlap with every known mask and its
    appearance embedding is dissimilar to every known embedding.
    """
    cfg = config or ProposalConfig()
    bgr = _as_bgr(image)
    h, w = bgr.shape[:2]

    # If no candidates supplied, derive from appearance quickly.
    if candidate_masks is None:
        app, _ = propose_appearance(
            bgr, frame_index=frame_index, timestamp=timestamp, config=cfg
        )
        candidates = [p.mask for p in app if p.mask is not None]
    else:
        candidates = list(candidate_masks)

    known = list(known_masks or [])
    known_embs = [list(e) for e in (known_embeddings or [])]

    # Union of known coverage so residual unexplained mass is well-defined.
    known_union: ArrayU8 | None = None
    if known:
        known_union = np.zeros((h, w), dtype=np.uint8)
        for km in known:
            if km is not None and km.shape == known_union.shape:
                known_union = np.where(km > 0, 1, known_union).astype(np.uint8)
        # Slight dilation so near-boundary leaks from the same object are explained.
        known_union = cv2.dilate(known_union, np.ones((5, 5), np.uint8))

    proposals: list[RegionProposal] = []
    for i, mask in enumerate(candidates):
        if mask is None or int(mask.sum()) < cfg.min_area:
            continue
        # Residual after subtracting known coverage — the actual unexplained mass.
        residual = mask.copy()
        if known_union is not None:
            residual = np.where(known_union > 0, 0, residual).astype(np.uint8)
            residual = cv2.morphologyEx(
                residual, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8)
            )
        if int(residual.sum()) < cfg.min_area:
            continue
        # Fraction of the original candidate explained by known tracks.
        if known_union is not None:
            explained_frac = 1.0 - (int(residual.sum()) / max(1, int(mask.sum())))
            if explained_frac > 0.50:
                continue

        emb = appearance_embedding(bgr, residual)
        explained_by_emb = False
        best_sim = 0.0
        for ke in known_embs:
            sim = embedding_similarity(emb, ke)
            best_sim = max(best_sim, sim)
            # High appearance agreement with a known concept → not unknown.
            if sim >= 0.90:
                explained_by_emb = True
                break
        if explained_by_emb:
            continue

        conf = float(np.clip(0.55 + 0.35 * (1.0 - best_sim), 0.2, 0.95))
        prop = _proposal_from_mask(
            bgr,
            residual,
            source=ProposalSource.UNKNOWN_REGION,
            frame_index=frame_index,
            timestamp=timestamp,
            proposal_id=f"F-{frame_index}-{i}",
            confidence=conf,
            limitations=["unexplained by known tracks/concepts at proposal time"],
            meta={
                "event": "NEW_UNKNOWN_REGION",
                "best_known_similarity": best_sim,
                "n_known_masks": len(known),
            },
        )
        if prop is not None:
            proposals.append(prop)

    # Deduplicate unknown residuals so the same entrant is not emitted thrice.
    if proposals:
        uniq_masks = _dedupe_masks(
            [p.mask for p in proposals if p.mask is not None], iou_thresh=0.5
        )
        kept: list[RegionProposal] = []
        for um in uniq_masks:
            for p in proposals:
                if p.mask is not None and _mask_iou(p.mask, um) >= 0.5:
                    kept.append(p)
                    break
        proposals = kept

    # If there are no known tracks, every coherent residual is unknown —
    # but only emit if we found something (avoids empty noise on blank frames).
    if not known and not known_embs and not proposals and candidates:
        # Frame with no prior knowledge: top appearance candidates are unknown.
        for i, mask in enumerate(candidates[: cfg.max_regions_per_source]):
            if mask is None or int(mask.sum()) < cfg.min_area:
                continue
            prop = _proposal_from_mask(
                bgr,
                mask,
                source=ProposalSource.UNKNOWN_REGION,
                frame_index=frame_index,
                timestamp=timestamp,
                proposal_id=f"F-{frame_index}-seed-{i}",
                confidence=0.50,
                limitations=["no known tracks yet; all regions are unknown"],
                meta={"event": "NEW_UNKNOWN_REGION", "seed": True},
            )
            if prop is not None:
                proposals.append(prop)

    proposals = proposals[: cfg.max_regions_per_source]
    report = SourceReport(
        source=ProposalSource.UNKNOWN_REGION,
        availability=(
            SourceAvailability.AVAILABLE if proposals else SourceAvailability.EMPTY
        ),
        n_proposals=len(proposals),
        limitations=["unknown = unexplained residual; no open-vocabulary classifier"],
    )
    return proposals, report


# ---------------------------------------------------------------------------
# Split / merge hypothesis construction
# ---------------------------------------------------------------------------


def _distance_peaks(mask: ArrayU8, *, min_sep: float) -> list[tuple[int, int]]:
    """Local maxima of the distance transform — candidate object centres."""
    dist = cv2.distanceTransform(mask, cv2.DIST_L2, 5)
    if dist.max() <= 0:
        return []
    # Non-maximum suppression on the distance map.
    kernel = int(max(3, min_sep))
    if kernel % 2 == 0:
        kernel += 1
    dilated = cv2.dilate(dist, np.ones((kernel, kernel), np.uint8))
    peaks = (dist == dilated) & (dist > 0.35 * dist.max())
    ys, xs = np.where(peaks)
    points = [(int(x), int(y)) for y, x in zip(ys, xs, strict=False)]
    # Greedy keep by distance value, enforcing min_sep.
    points.sort(key=lambda p: float(dist[p[1], p[0]]), reverse=True)
    kept: list[tuple[int, int]] = []
    for p in points:
        if all(np.hypot(p[0] - q[0], p[1] - q[1]) >= min_sep for q in kept):
            kept.append(p)
    return kept


def build_split_hypotheses(
    image_bgr: ArrayU8,
    proposals: Sequence[RegionProposal],
    *,
    frame_index: int,
    timestamp: float,
    config: ProposalConfig | None = None,
) -> list[RegionProposal]:
    """When one mask may cover two touching objects, keep both halves.

    The parent (merged) proposal is not deleted — the split children are extra
    hypotheses that association may prefer once temporal evidence arrives.
    Only the largest candidates are considered so runtime stays bounded.
    """
    cfg = config or ProposalConfig()
    circ_max = float(FROZEN_THRESHOLDS["split_circularity_max"])
    aspect_min = float(FROZEN_THRESHOLDS["split_aspect_min"])
    min_sep = float(FROZEN_THRESHOLDS["split_peak_min_sep_px"])
    out: list[RegionProposal] = []
    # Largest first; cap work.
    candidates = sorted(
        [
            p
            for p in proposals
            if p.status is ProposalStatus.ACTIVE
            and p.mask is not None
            and p.hypothesis_kind is HypothesisKind.ATOMIC
            and p.area_px >= cfg.min_area * 1.5
        ],
        key=lambda p: p.area_px,
        reverse=True,
    )[:12]
    for prop in candidates:
        mask = prop.mask
        assert mask is not None
        circ = _circularity(mask)
        aspect = _aspect_ratio(prop.bbox_xywh)
        # Skip compact blobs unless aspect already screams "two objects".
        if circ > circ_max and aspect < aspect_min:
            continue
        peaks = _distance_peaks(mask, min_sep=min_sep)
        if len(peaks) < 2:
            continue
        dist = cv2.distanceTransform(mask, cv2.DIST_L2, 5)
        surface = cv2.normalize(dist, None, 0, 255, cv2.NORM_MINMAX).astype(np.uint8)
        surface = cv2.cvtColor(surface, cv2.COLOR_GRAY2BGR)
        ws = np.zeros(mask.shape, dtype=np.int32)
        for i, (x, y) in enumerate(peaks[:2], start=1):
            ws[y, x] = i
        ws = cv2.dilate(ws.astype(np.uint8), np.ones((3, 3), np.uint8)).astype(np.int32)
        unknown = (mask > 0) & (ws == 0)
        ws[unknown] = 0
        ws[mask == 0] = 3
        try:
            cv2.watershed(surface, ws)
        except cv2.error:
            continue
        child_ids: list[str] = []
        for label in (1, 2):
            child_mask = ((ws == label) & (mask > 0)).astype(np.uint8)
            if int(child_mask.sum()) < cfg.min_area:
                continue
            cid = f"SPLIT-{prop.proposal_id}-{label}"
            child = _proposal_from_mask(
                image_bgr,
                child_mask,
                source=prop.source,
                frame_index=frame_index,
                timestamp=timestamp,
                proposal_id=cid,
                confidence=float(prop.confidence * 0.85),
                hypothesis_kind=HypothesisKind.SPLIT,
                related=[prop.proposal_id],
                limitations=[
                    "split hypothesis: parent may be two touching objects",
                    f"parent={prop.proposal_id}",
                ],
                meta={"parent_id": prop.proposal_id, "n_peaks": len(peaks)},
            )
            if child is not None:
                child.supporting_sources = list(prop.supporting_sources)
                out.append(child)
                child_ids.append(cid)
        if child_ids:
            prop.related_proposal_ids = list(
                dict.fromkeys([*prop.related_proposal_ids, *child_ids])
            )
            prop.meta = {
                **prop.meta,
                "split_children": child_ids,
                "split_peaks": len(peaks),
            }
    return out


def build_merge_hypotheses(
    image_bgr: ArrayU8,
    proposals: Sequence[RegionProposal],
    *,
    frame_index: int,
    timestamp: float,
    config: ProposalConfig | None = None,
) -> list[RegionProposal]:
    """When several fragments may be one object, keep a merged alternative.

    Children are not deleted — the merge parent is an extra hypothesis.
    """
    cfg = config or ProposalConfig()
    dist_scale = float(FROZEN_THRESHOLDS["merge_centroid_dist_scale"])
    app_min = float(FROZEN_THRESHOLDS["merge_appearance_min"])
    gap_px = float(FROZEN_THRESHOLDS["merge_gap_px"])
    active = sorted(
        [
            p
            for p in proposals
            if p.status is ProposalStatus.ACTIVE
            and p.mask is not None
            and p.hypothesis_kind is HypothesisKind.ATOMIC
        ],
        key=lambda p: p.area_px,
        reverse=True,
    )[:16]
    out: list[RegionProposal] = []
    used_pairs: set[tuple[str, str]] = set()
    for i, a in enumerate(active):
        for b in active[i + 1 :]:
            pair = tuple(sorted((a.proposal_id, b.proposal_id)))
            if pair in used_pairs:
                continue
            # Spatial proximity: centroids within scaled mean radius + gap.
            _, _, aw, ah = a.bbox_xywh
            _, _, bw, bh = b.bbox_xywh
            ra = 0.5 * max(aw, ah)
            rb = 0.5 * max(bw, bh)
            cx_a, cy_a = a.centroid_xy
            cx_b, cy_b = b.centroid_xy
            dist = float(np.hypot(cx_a - cx_b, cy_a - cy_b))
            if dist > dist_scale * (ra + rb) + gap_px:
                continue
            sim = embedding_similarity(a.appearance_embedding, b.appearance_embedding)
            if sim < app_min:
                continue
            if (
                a.mask is not None
                and b.mask is not None
                and _mask_iou(a.mask, b.mask) > 0.5
            ):
                continue
            used_pairs.add(pair)
            assert a.mask is not None and b.mask is not None
            merged_mask = np.zeros(a.mask.shape, dtype=np.uint8)
            merged_mask[a.mask > 0] = 1
            merged_mask[b.mask > 0] = 1
            merged_mask = cv2.morphologyEx(
                merged_mask, cv2.MORPH_CLOSE, np.ones((5, 5), np.uint8)
            )
            if int(merged_mask.sum()) < cfg.min_area:
                continue
            mid = f"MERGE-{a.proposal_id}+{b.proposal_id}"
            parent = _proposal_from_mask(
                image_bgr,
                merged_mask,
                source=a.source,
                frame_index=frame_index,
                timestamp=timestamp,
                proposal_id=mid,
                confidence=float(
                    0.5 * (a.confidence + b.confidence) * (0.7 + 0.3 * sim)
                ),
                hypothesis_kind=HypothesisKind.MERGE,
                related=[a.proposal_id, b.proposal_id],
                limitations=[
                    "merge hypothesis: children may be fragments of one object",
                ],
                meta={
                    "child_ids": [a.proposal_id, b.proposal_id],
                    "appearance_agreement": sim,
                    "centroid_distance_px": dist,
                },
            )
            if parent is not None:
                parent.supporting_sources = sorted(
                    set(a.supporting_sources) | set(b.supporting_sources)
                )
                out.append(parent)
                a.related_proposal_ids = list(
                    dict.fromkeys([*a.related_proposal_ids, mid])
                )
                b.related_proposal_ids = list(
                    dict.fromkeys([*b.related_proposal_ids, mid])
                )
            if len(out) >= 12:
                return out
    return out


# ---------------------------------------------------------------------------
# Fusion — evidence accumulation, not NMS
# ---------------------------------------------------------------------------


def _build_edges(proposals: Sequence[RegionProposal]) -> list[ProposalEdge]:
    iou_thr = float(FROZEN_THRESHOLDS["overlap_iou_edge"])
    app_thr = float(FROZEN_THRESHOLDS["appearance_agree_edge"])
    active = [p for p in proposals if p.status is ProposalStatus.ACTIVE and p.area_px > 0]
    edges: list[ProposalEdge] = []
    for i, a in enumerate(active):
        for b in active[i + 1 :]:
            if a.mask is not None and b.mask is not None:
                iou = _mask_iou(a.mask, b.mask)
            else:
                iou = _bbox_iou(a.bbox_xywh, b.bbox_xywh)
            app = embedding_similarity(a.appearance_embedding, b.appearance_embedding)
            cx = float(
                np.hypot(
                    a.centroid_xy[0] - b.centroid_xy[0],
                    a.centroid_xy[1] - b.centroid_xy[1],
                )
            )
            if iou < iou_thr and app < app_thr:
                continue
            if iou >= 0.5:
                relation = "overlap"
            elif iou >= iou_thr:
                relation = "adjacent"
            else:
                relation = "appearance"
            edges.append(
                ProposalEdge(
                    a_id=a.proposal_id,
                    b_id=b.proposal_id,
                    iou=iou,
                    appearance_agreement=app,
                    centroid_distance_px=cx,
                    relation=relation,
                )
            )
    return edges


def _union_find_clusters(
    proposal_ids: Sequence[str], edges: Sequence[ProposalEdge], *, min_iou: float
) -> list[list[str]]:
    parent = {pid: pid for pid in proposal_ids}

    def find(x: str) -> str:
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    def union(a: str, b: str) -> None:
        ra, rb = find(a), find(b)
        if ra != rb:
            parent[rb] = ra

    for e in edges:
        if e.a_id not in parent or e.b_id not in parent:
            continue
        # Fuse only when spatial evidence is strong; appearance alone does not
        # collapse two distant similar objects (visually_similar trap).
        if e.iou >= min_iou:
            union(e.a_id, e.b_id)

    groups: dict[str, list[str]] = {}
    for pid in proposal_ids:
        root = find(pid)
        groups.setdefault(root, []).append(pid)
    return list(groups.values())


def fuse_proposals(
    image: NDArray[Any],
    proposals: Sequence[RegionProposal],
    edges: Sequence[ProposalEdge],
    *,
    frame_index: int,
    timestamp: float,
) -> list[RegionProposal]:
    """Accumulate multi-source evidence into fused atomic proposals.

    Split and merge hypotheses are *not* collapsed here — the caller keeps them
    as parallel alternatives. Only ATOMIC active proposals participate.
    """
    bgr = _as_bgr(image)
    atomic = [
        p
        for p in proposals
        if p.status is ProposalStatus.ACTIVE
        and p.hypothesis_kind is HypothesisKind.ATOMIC
        and p.area_px > 0
        and p.mask is not None
    ]
    if not atomic:
        return []
    by_id = {p.proposal_id: p for p in atomic}
    clusters = _union_find_clusters(
        [p.proposal_id for p in atomic],
        edges,
        min_iou=0.35,
    )
    fused: list[RegionProposal] = []
    multi_bonus = float(FROZEN_THRESHOLDS["multi_source_bonus"])
    conf_floor = float(FROZEN_THRESHOLDS["confidence_floor"])
    for ci, members in enumerate(clusters):
        members_p = [by_id[m] for m in members if m in by_id]
        if not members_p:
            continue
        # Union mask — evidence accumulates; no source is dropped.
        mask = np.zeros(members_p[0].mask.shape, dtype=np.uint8)
        sources: set[str] = set()
        conf_acc = 0.0
        for p in members_p:
            mask = np.where(p.mask > 0, 1, mask).astype(np.uint8)
            sources.update(p.supporting_sources or [p.source.value])
            conf_acc = max(conf_acc, p.confidence)
        conf = float(
            np.clip(conf_acc + multi_bonus * max(0, len(sources) - 1), conf_floor, 0.99)
        )
        prop = _proposal_from_mask(
            bgr,
            mask,
            source=ProposalSource.APPEARANCE,  # fused retains multi-source list
            frame_index=frame_index,
            timestamp=timestamp,
            proposal_id=f"FUSED-{frame_index}-{ci}",
            confidence=conf,
            limitations=["fused by evidence accumulation; not NMS"],
            meta={
                "member_ids": [p.proposal_id for p in members_p],
                "n_sources": len(sources),
            },
        )
        if prop is None:
            continue
        prop.supporting_sources = sorted(sources)
        prop.source = ProposalSource.APPEARANCE  # label is multi; sources list is truth
        prop.meta["fused"] = True
        best = max(members_p, key=lambda p: p.confidence)
        prop.appearance_embedding = list(best.appearance_embedding)
        prop.appearance_hist = list(best.appearance_hist)
        fused.append(prop)
    # Prefer higher-confidence / larger fused nodes. Contained smaller fused
    # regions stay as raw proposals (not destroyed); they simply do not all
    # need to be re-listed as separate fused outputs.
    fused.sort(key=lambda p: (p.confidence, p.area_px), reverse=True)
    kept: list[RegionProposal] = []
    for prop in fused:
        if prop.mask is None:
            continue
        contained = False
        for larger in kept:
            if larger.mask is None:
                continue
            inter = int(np.logical_and(prop.mask > 0, larger.mask > 0).sum())
            if inter / max(1, int(prop.mask.sum())) > 0.7:
                contained = True
                break
        if not contained:
            kept.append(prop)
        if len(kept) >= int(FROZEN_THRESHOLDS["max_regions_per_source"]):
            break
    return kept


# ---------------------------------------------------------------------------
# Orchestrator
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class ProposalContext:
    """Optional temporal / geometric / track context for one frame."""

    previous_image: NDArray[Any] | None = None
    background_model: BackgroundModel | None = None
    depth: NDArray[Any] | None = None
    normals: NDArray[Any] | None = None
    known_masks: list[ArrayU8] = field(default_factory=list)
    known_embeddings: list[list[float]] = field(default_factory=list)
    prev_points: ArrayF32 | None = None


@dataclass(slots=True)
class ProposeResult:
    graph: RegionProposalGraph
    next_points: ArrayF32 | None = None


def propose(
    image: NDArray[Any],
    *,
    frame_index: int = 0,
    timestamp: float = 0.0,
    context: ProposalContext | None = None,
    config: ProposalConfig | None = None,
    sources: Sequence[ProposalSource] | None = None,
) -> ProposeResult:
    """Run selected sources and fuse. Default: all six sources."""
    cfg = config or ProposalConfig()
    ctx = context or ProposalContext()
    bgr = _as_bgr(image)
    wanted = set(sources) if sources is not None else set(ALL_SOURCES)

    all_props: list[RegionProposal] = []
    reports: list[SourceReport] = []
    next_points = ctx.prev_points

    if ProposalSource.APPEARANCE in wanted:
        props, rep = propose_appearance(
            bgr, frame_index=frame_index, timestamp=timestamp, config=cfg
        )
        all_props.extend(props)
        reports.append(rep)

    if ProposalSource.TEMPORAL_CHANGE in wanted:
        props, rep = propose_temporal_change(
            bgr,
            previous_image=ctx.previous_image,
            background_model=ctx.background_model,
            frame_index=frame_index,
            timestamp=timestamp,
            config=cfg,
        )
        all_props.extend(props)
        reports.append(rep)

    if ProposalSource.DENSE_FEATURES in wanted:
        props, rep = propose_dense_features(
            bgr, frame_index=frame_index, timestamp=timestamp, config=cfg
        )
        all_props.extend(props)
        reports.append(rep)

    if ProposalSource.GEOMETRY in wanted:
        props, rep = propose_geometry(
            bgr,
            depth=ctx.depth,
            normals=ctx.normals,
            frame_index=frame_index,
            timestamp=timestamp,
            config=cfg,
        )
        all_props.extend(props)
        reports.append(rep)

    if ProposalSource.POINT_TRACK_CLUSTERS in wanted:
        props, rep, next_points = propose_point_track_clusters(
            bgr,
            previous_image=ctx.previous_image,
            prev_points=ctx.prev_points,
            frame_index=frame_index,
            timestamp=timestamp,
            config=cfg,
        )
        all_props.extend(props)
        reports.append(rep)

    if ProposalSource.UNKNOWN_REGION in wanted:
        # Candidates from appearance + temporal active masks.
        candidate_masks = [
            p.mask
            for p in all_props
            if p.status is ProposalStatus.ACTIVE
            and p.mask is not None
            and p.source
            in {ProposalSource.APPEARANCE, ProposalSource.TEMPORAL_CHANGE}
        ]
        props, rep = propose_unknown_regions(
            bgr,
            candidate_masks=candidate_masks,
            known_masks=ctx.known_masks,
            known_embeddings=ctx.known_embeddings,
            frame_index=frame_index,
            timestamp=timestamp,
            config=cfg,
        )
        all_props.extend(props)
        reports.append(rep)

    # Structural hypotheses from atomic active proposals.
    atomic_for_hyp = [
        p
        for p in all_props
        if p.status is ProposalStatus.ACTIVE and p.hypothesis_kind is HypothesisKind.ATOMIC
    ]
    splits = build_split_hypotheses(
        bgr, atomic_for_hyp, frame_index=frame_index, timestamp=timestamp, config=cfg
    )
    merges = build_merge_hypotheses(
        bgr, atomic_for_hyp, frame_index=frame_index, timestamp=timestamp, config=cfg
    )
    all_props.extend(splits)
    all_props.extend(merges)

    edges = _build_edges(all_props)
    fused = fuse_proposals(
        bgr, all_props, edges, frame_index=frame_index, timestamp=timestamp
    )

    graph = RegionProposalGraph(
        frame_index=frame_index,
        timestamp=timestamp,
        proposals=all_props,
        edges=edges,
        source_reports=reports,
        fused=fused,
        split_hypotheses=splits,
        merge_hypotheses=merges,
        thresholds_digest=FROZEN_THRESHOLDS_DIGEST,
        limitations=[
            "no ground truth in builder path",
            "fusion preserves split/merge hypotheses",
            "geometry BLOCKED without depth pass",
        ],
    )
    return ProposeResult(graph=graph, next_points=next_points)


def assert_no_ground_truth_in_proposals(proposals: Sequence[RegionProposal]) -> None:
    """Contract guard: any GT symbol on a proposal is a hard failure."""
    forbidden = {
        "gt_id",
        "ground_truth_id",
        "ground_truth",
        "oracle_id",
        "gt_bbox",
        "bbox_xywh_px",
    }
    for prop in proposals:
        for key in forbidden:
            if key in prop.meta:
                raise AssertionError(
                    f"ground truth key {key!r} reached proposal {prop.proposal_id}"
                )
        if hasattr(prop, "ground_truth_id"):
            raise AssertionError("RegionProposal carries ground_truth_id")


def proposal_authority() -> AuthorityClass:
    return SEGMENT_AUTHORITY_CEILING


# ---------------------------------------------------------------------------
# Metrics (used by runner / sealed evaluator — scoring may use GT; builder must not)
# ---------------------------------------------------------------------------


def match_proposals_to_gt(
    proposals: Sequence[RegionProposal],
    gt_boxes: Sequence[tuple[float, float, float, float]],
    *,
    iou_threshold: float = 0.3,
) -> dict[str, Any]:
    """Greedy IoU match for proposal recall/precision. Evaluator-side only."""
    active = [
        p
        for p in proposals
        if p.status is ProposalStatus.ACTIVE and p.area_px > 0
    ]
    matched_gt: set[int] = set()
    matched_prop: set[int] = set()
    pairs: list[tuple[int, int, float]] = []
    for pi, prop in enumerate(active):
        best_j, best_iou = -1, 0.0
        for gi, gbox in enumerate(gt_boxes):
            if gi in matched_gt:
                continue
            iou = _bbox_iou(prop.bbox_xywh, gbox)
            if iou > best_iou:
                best_iou = iou
                best_j = gi
        if best_j >= 0 and best_iou >= iou_threshold:
            matched_gt.add(best_j)
            matched_prop.add(pi)
            pairs.append((pi, best_j, best_iou))
    n_gt = len(gt_boxes)
    n_prop = len(active)
    tp = len(matched_gt)
    recall = tp / n_gt if n_gt else 0.0
    precision = tp / n_prop if n_prop else 0.0
    fp = n_prop - len(matched_prop)
    fpr = fp / n_prop if n_prop else 0.0
    count_error = abs(n_prop - n_gt)
    return {
        "n_gt": n_gt,
        "n_proposals": n_prop,
        "true_positives": tp,
        "false_positives": fp,
        "recall": recall,
        "precision": precision,
        "false_positive_rate": fpr,
        "object_count_error": count_error,
        "pairs": pairs,
    }


def confidence_calibration(
    proposals: Sequence[RegionProposal],
    gt_boxes: Sequence[tuple[float, float, float, float]],
    *,
    iou_threshold: float = 0.3,
    n_bins: int = 5,
) -> dict[str, Any]:
    """Reliability-style bins: mean confidence vs empirical hit rate."""
    active = [
        p
        for p in proposals
        if p.status is ProposalStatus.ACTIVE and p.area_px > 0
    ]
    if not active:
        return {"bins": [], "ece": 0.0}
    hits: list[tuple[float, int]] = []
    for prop in active:
        hit = 0
        for gbox in gt_boxes:
            if _bbox_iou(prop.bbox_xywh, gbox) >= iou_threshold:
                hit = 1
                break
        hits.append((prop.confidence, hit))
    bins: list[dict[str, float]] = []
    ece = 0.0
    for b in range(n_bins):
        lo, hi = b / n_bins, (b + 1) / n_bins
        members = [(c, h) for c, h in hits if lo <= c < hi or (b == n_bins - 1 and c == 1.0)]
        if not members:
            bins.append({"lo": lo, "hi": hi, "n": 0, "mean_conf": 0.0, "hit_rate": 0.0})
            continue
        mean_conf = float(np.mean([c for c, _ in members]))
        hit_rate = float(np.mean([h for _, h in members]))
        bins.append(
            {
                "lo": lo,
                "hi": hi,
                "n": len(members),
                "mean_conf": mean_conf,
                "hit_rate": hit_rate,
            }
        )
        ece += (len(members) / len(hits)) * abs(mean_conf - hit_rate)
    return {"bins": bins, "ece": float(ece)}
