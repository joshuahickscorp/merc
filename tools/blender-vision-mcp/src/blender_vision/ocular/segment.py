"""Classical, honestly-labelled segmentation for the Ocular loop.

No learned weights. Methods are colour/texture region growing, watershed on
gradient, GrabCut from a box, connected components on motion residual, and
contour/part decomposition. Authority ceiling is SENSOR_DERIVED. Identity of
segments is stabilised across frames when a previous result is supplied —
labels do not silently renumber.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

import cv2
import numpy as np
from numpy.typing import NDArray

from blender_vision.core.errors import ValidationError
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import Lineage, V2Record

ArrayU8 = NDArray[np.uint8]
ArrayI32 = NDArray[np.int32]
ArrayF64 = NDArray[np.float64]

#: Hard ceiling for every classical method in this module.
SEGMENT_AUTHORITY_CEILING = AuthorityClass.SENSOR_DERIVED


class SegmentationMethod(StrEnum):
    REGION_GROW = "region_grow"
    WATERSHED = "watershed"
    GRABCUT = "grabcut"
    MOTION_COMPONENTS = "motion_components"
    CONTOUR_PARTS = "contour_parts"


class ConceptResolution(StrEnum):
    RESOLVED = "RESOLVED"
    UNRESOLVED = "UNRESOLVED"


# Explicit local concept table. Not open-vocabulary: unknown prompts refuse.
LOCAL_CONCEPT_TABLE: dict[str, dict[str, Any]] = {
    "red": {"kind": "colour", "hsv_low": (0, 80, 60), "hsv_high": (12, 255, 255)},
    "green": {"kind": "colour", "hsv_low": (40, 60, 40), "hsv_high": (85, 255, 255)},
    "blue": {"kind": "colour", "hsv_low": (95, 60, 40), "hsv_high": (130, 255, 255)},
    "yellow": {"kind": "colour", "hsv_low": (18, 80, 80), "hsv_high": (38, 255, 255)},
    "orange": {"kind": "colour", "hsv_low": (8, 100, 80), "hsv_high": (22, 255, 255)},
    "foreground": {"kind": "salient", "method": SegmentationMethod.WATERSHED},
    "moving": {"kind": "motion", "method": SegmentationMethod.MOTION_COMPONENTS},
    "object": {"kind": "salient", "method": SegmentationMethod.WATERSHED},
}


@dataclass(slots=True)
class SegmentInstance:
    """One labelled region with a stable identity when previous is supplied."""

    segment_id: str
    label: int
    area_px: int
    bbox_xywh: tuple[int, int, int, int]
    centroid_xy: tuple[float, float]
    mean_bgr: tuple[float, float, float]
    appearance_hist: list[float]
    method: SegmentationMethod
    authority: AuthorityClass = SEGMENT_AUTHORITY_CEILING
    part_of: str | None = None
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "segment_id": self.segment_id,
            "label": self.label,
            "area_px": self.area_px,
            "bbox_xywh": list(self.bbox_xywh),
            "centroid_xy": list(self.centroid_xy),
            "mean_bgr": list(self.mean_bgr),
            "appearance_hist": list(self.appearance_hist),
            "method": self.method.value,
            "authority": self.authority.value,
            "part_of": self.part_of,
            "notes": list(self.notes),
        }


@dataclass(slots=True, kw_only=True)
class SegmentationResult(V2Record):
    """Sealed record of a segmentation pass."""

    RECORD_KIND = "ocular.segmentation"

    method: SegmentationMethod = SegmentationMethod.WATERSHED
    width: int = 0
    height: int = 0
    label_map_digest: str = ""
    instances: list[SegmentInstance] = field(default_factory=list)
    concept_prompt: str | None = None
    concept_resolution: ConceptResolution | None = None
    previous_result_id: str | None = None
    identity_links: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        value = V2Record.payload(self)
        value["method"] = self.method.value if isinstance(self.method, StrEnum) else self.method
        value["instances"] = [
            item.to_dict() if isinstance(item, SegmentInstance) else item
            for item in self.instances
        ]
        if self.concept_resolution is not None:
            value["concept_resolution"] = (
                self.concept_resolution.value
                if isinstance(self.concept_resolution, StrEnum)
                else self.concept_resolution
            )
        value["digest"] = self.digest or self.compute_digest()
        return value


def _as_bgr(image: NDArray[Any]) -> ArrayU8:
    if image.ndim == 2:
        return cv2.cvtColor(image.astype(np.uint8), cv2.COLOR_GRAY2BGR)
    if image.ndim != 3 or image.shape[2] not in (3, 4):
        raise ValidationError(f"expected HxW or HxWx3/4 image, got shape {image.shape}")
    arr = image[:, :, :3]
    if arr.dtype != np.uint8:
        arr = np.clip(arr, 0, 255).astype(np.uint8)
    return np.ascontiguousarray(arr)


def _bbox_from_mask(mask: ArrayU8) -> tuple[int, int, int, int]:
    ys, xs = np.where(mask > 0)
    if len(xs) == 0:
        return (0, 0, 0, 0)
    x0, x1 = int(xs.min()), int(xs.max())
    y0, y1 = int(ys.min()), int(ys.max())
    return (x0, y0, x1 - x0 + 1, y1 - y0 + 1)


def _centroid(mask: ArrayU8) -> tuple[float, float]:
    ys, xs = np.where(mask > 0)
    if len(xs) == 0:
        return (0.0, 0.0)
    return (float(xs.mean()), float(ys.mean()))


def _mean_bgr(image: ArrayU8, mask: ArrayU8) -> tuple[float, float, float]:
    pixels = image[mask > 0]
    if len(pixels) == 0:
        return (0.0, 0.0, 0.0)
    mean = pixels.mean(axis=0)
    return (float(mean[0]), float(mean[1]), float(mean[2]))


def appearance_histogram(image: ArrayU8, mask: ArrayU8, bins: int = 16) -> list[float]:
    """Normalised BGR histogram over the mask. Used for track association."""
    hist_parts: list[ArrayF64] = []
    for channel in range(3):
        hist = cv2.calcHist([image], [channel], mask, [bins], [0, 256]).flatten()
        hist_parts.append(hist.astype(np.float64))
    combined = np.concatenate(hist_parts)
    total = float(combined.sum())
    if total <= 0.0:
        return [0.0] * (bins * 3)
    combined = combined / total
    return [float(v) for v in combined]


def histogram_correlation(a: list[float] | ArrayF64, b: list[float] | ArrayF64) -> float:
    """Bhattacharyya-style similarity in [0, 1]. 1 is identical."""
    va = np.asarray(a, dtype=np.float64)
    vb = np.asarray(b, dtype=np.float64)
    if va.shape != vb.shape or va.size == 0:
        return 0.0
    va = va / (va.sum() + 1e-12)
    vb = vb / (vb.sum() + 1e-12)
    return float(np.clip(np.sum(np.sqrt(va * vb)), 0.0, 1.0))


def _label_map_digest(labels: ArrayI32) -> str:
    import hashlib

    return hashlib.sha256(np.ascontiguousarray(labels).tobytes()).hexdigest()


def _instances_from_labels(
    image: ArrayU8,
    labels: ArrayI32,
    method: SegmentationMethod,
    *,
    min_area: int,
    id_prefix: str,
    background: int = 0,
) -> list[SegmentInstance]:
    instances: list[SegmentInstance] = []
    for label in sorted(int(v) for v in np.unique(labels) if int(v) != background):
        mask = (labels == label).astype(np.uint8)
        area = int(mask.sum())
        if area < min_area:
            continue
        instances.append(
            SegmentInstance(
                segment_id=f"{id_prefix}-{label}",
                label=label,
                area_px=area,
                bbox_xywh=_bbox_from_mask(mask),
                centroid_xy=_centroid(mask),
                mean_bgr=_mean_bgr(image, mask),
                appearance_hist=appearance_histogram(image, mask),
                method=method,
                authority=SEGMENT_AUTHORITY_CEILING,
            )
        )
    return instances


def _stabilise_identities(
    current: list[SegmentInstance],
    previous: SegmentationResult | None,
    labels: ArrayI32,
) -> tuple[list[SegmentInstance], dict[str, str], ArrayI32]:
    """Match current regions to previous by IoU so IDs do not renumber silently."""
    if previous is None or not previous.instances:
        return current, {}, labels

    links: dict[str, str] = {}
    used_prev: set[str] = set()
    remapped = labels.copy()
    next_label = int(labels.max()) + 1 if labels.size else 1
    label_remap: dict[int, int] = {}

    prev_by_label = {inst.label: inst for inst in previous.instances}
    # Rebuild previous label map proxies from bboxes (full maps are not stored).
    h, w = labels.shape
    prev_map = np.zeros((h, w), dtype=np.int32)
    for inst in previous.instances:
        x, y, bw, bh = inst.bbox_xywh
        x1, y1 = min(w, x + bw), min(h, y + bh)
        x0, y0 = max(0, x), max(0, y)
        prev_map[y0:y1, x0:x1] = inst.label

    stabilised: list[SegmentInstance] = []
    for inst in current:
        mask = (labels == inst.label).astype(np.uint8)
        best_id: str | None = None
        best_iou = 0.0
        best_prev_label = 0
        for prev in previous.instances:
            if prev.segment_id in used_prev:
                continue
            prev_mask = (prev_map == prev.label).astype(np.uint8)
            inter = int(np.logical_and(mask > 0, prev_mask > 0).sum())
            union = int(np.logical_or(mask > 0, prev_mask > 0).sum())
            iou = inter / union if union else 0.0
            # Appearance as a soft tie-break when IoU is comparable.
            app = histogram_correlation(inst.appearance_hist, prev.appearance_hist)
            score = 0.7 * iou + 0.3 * app
            if score > best_iou:
                best_iou = score
                best_id = prev.segment_id
                best_prev_label = prev.label

        if best_id is not None and best_iou >= 0.25:
            used_prev.add(best_id)
            links[inst.segment_id] = best_id
            # Keep the previous segment_id and prefer previous label index.
            if best_prev_label in prev_by_label and best_prev_label not in label_remap.values():
                label_remap[inst.label] = best_prev_label
                new_label = best_prev_label
            else:
                new_label = inst.label
            stabilised.append(
                SegmentInstance(
                    segment_id=best_id,
                    label=new_label,
                    area_px=inst.area_px,
                    bbox_xywh=inst.bbox_xywh,
                    centroid_xy=inst.centroid_xy,
                    mean_bgr=inst.mean_bgr,
                    appearance_hist=inst.appearance_hist,
                    method=inst.method,
                    authority=inst.authority,
                    part_of=inst.part_of,
                    notes=list(inst.notes) + [f"identity-linked:{best_id}"],
                )
            )
        else:
            # New region: allocate a fresh label if needed.
            new_label = inst.label
            if new_label in {s.label for s in stabilised}:
                new_label = next_label
                next_label += 1
                label_remap[inst.label] = new_label
            stabilised.append(
                SegmentInstance(
                    segment_id=inst.segment_id,
                    label=new_label,
                    area_px=inst.area_px,
                    bbox_xywh=inst.bbox_xywh,
                    centroid_xy=inst.centroid_xy,
                    mean_bgr=inst.mean_bgr,
                    appearance_hist=inst.appearance_hist,
                    method=inst.method,
                    authority=inst.authority,
                    part_of=inst.part_of,
                    notes=list(inst.notes),
                )
            )

    if label_remap:
        out = np.zeros_like(labels)
        for old, new in label_remap.items():
            out[labels == old] = new
        # Keep unmapped labels as-is.
        for old in np.unique(labels):
            old_i = int(old)
            if old_i == 0 or old_i in label_remap:
                continue
            out[labels == old_i] = old_i
        remapped = out

    return stabilised, links, remapped


def _region_grow(
    image: ArrayU8,
    seeds: list[tuple[int, int]],
    *,
    colour_radius: float,
    min_area: int,
) -> ArrayI32:
    if not seeds:
        raise ValidationError("region_grow requires at least one seed (x, y)")
    h, w = image.shape[:2]
    labels = np.zeros((h, w), dtype=np.int32)
    lab = cv2.cvtColor(image, cv2.COLOR_BGR2LAB).astype(np.float32)
    for idx, (sx, sy) in enumerate(seeds, start=1):
        if not (0 <= sx < w and 0 <= sy < h):
            raise ValidationError(f"seed {(sx, sy)} outside image {w}x{h}")
        if labels[sy, sx] != 0:
            continue
        target = lab[sy, sx]
        stack = [(sx, sy)]
        labels[sy, sx] = idx
        while stack:
            x, y = stack.pop()
            for nx, ny in ((x - 1, y), (x + 1, y), (x, y - 1), (x, y + 1)):
                if nx < 0 or ny < 0 or nx >= w or ny >= h:
                    continue
                if labels[ny, nx] != 0:
                    continue
                if float(np.linalg.norm(lab[ny, nx] - target)) <= colour_radius:
                    labels[ny, nx] = idx
                    stack.append((nx, ny))
    # Drop tiny regions.
    for label in np.unique(labels):
        if label == 0:
            continue
        if int((labels == label).sum()) < min_area:
            labels[labels == label] = 0
    return labels


def _watershed(image: ArrayU8, *, min_area: int, max_regions: int) -> ArrayI32:
    gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
    blur = cv2.GaussianBlur(gray, (5, 5), 0)
    # Gradient magnitude as topographic surface.
    gx = cv2.Sobel(blur, cv2.CV_32F, 1, 0, ksize=3)
    gy = cv2.Sobel(blur, cv2.CV_32F, 0, 1, ksize=3)
    grad = cv2.magnitude(gx, gy)
    grad_u8 = cv2.normalize(grad, None, 0, 255, cv2.NORM_MINMAX).astype(np.uint8)

    # Foreground markers from local minima of the gradient (inverted peaks).
    _, thresh = cv2.threshold(blur, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
    # Prefer bright objects on darker ground; invert if background is bright.
    if float(blur[thresh == 0].mean() if np.any(thresh == 0) else 0) > float(
        blur[thresh > 0].mean() if np.any(thresh > 0) else 0
    ):
        thresh = cv2.bitwise_not(thresh)
    dist = cv2.distanceTransform(thresh, cv2.DIST_L2, 5)
    if dist.max() <= 0:
        return np.zeros(image.shape[:2], dtype=np.int32)
    _, sure_fg = cv2.threshold(dist, 0.35 * dist.max(), 255, 0)
    sure_fg = sure_fg.astype(np.uint8)
    kernel = np.ones((3, 3), np.uint8)
    sure_bg = cv2.dilate(thresh, kernel, iterations=2)
    unknown = cv2.subtract(sure_bg, sure_fg)
    num_markers, markers = cv2.connectedComponents(sure_fg)
    markers = markers + 1
    markers[unknown > 0] = 0
    surface = cv2.cvtColor(grad_u8, cv2.COLOR_GRAY2BGR)
    cv2.watershed(surface, markers)
    labels = np.zeros(image.shape[:2], dtype=np.int32)
    out_label = 1
    # markers: 1 is usually background border; >1 are regions; -1 boundaries.
    for marker in range(2, num_markers + 2):
        mask = markers == marker
        area = int(mask.sum())
        if area < min_area:
            continue
        if out_label > max_regions:
            break
        labels[mask] = out_label
        out_label += 1
    return labels


def _grabcut(image: ArrayU8, box: tuple[int, int, int, int], *, min_area: int) -> ArrayI32:
    x, y, w, h = box
    if w <= 1 or h <= 1:
        raise ValidationError("grabcut box must have positive width and height")
    ih, iw = image.shape[:2]
    x = max(0, min(x, iw - 1))
    y = max(0, min(y, ih - 1))
    w = max(1, min(w, iw - x))
    h = max(1, min(h, ih - y))
    mask = np.zeros(image.shape[:2], dtype=np.uint8)
    bgd = np.zeros((1, 65), np.float64)
    fgd = np.zeros((1, 65), np.float64)
    rect = (x, y, w, h)
    cv2.grabCut(image, mask, rect, bgd, fgd, 5, cv2.GC_INIT_WITH_RECT)
    foreground = np.where((mask == cv2.GC_FGD) | (mask == cv2.GC_PR_FGD), 1, 0).astype(
        np.uint8
    )
    num, cc = cv2.connectedComponents(foreground)
    labels = np.zeros(image.shape[:2], dtype=np.int32)
    out = 1
    for label in range(1, num):
        area = int((cc == label).sum())
        if area < min_area:
            continue
        labels[cc == label] = out
        out += 1
    return labels


def _motion_components(
    image: ArrayU8,
    previous_image: ArrayU8,
    *,
    min_area: int,
    residual_threshold: int,
) -> ArrayI32:
    if previous_image.shape[:2] != image.shape[:2]:
        raise ValidationError("motion_components requires matching previous image size")
    gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
    prev = cv2.cvtColor(previous_image, cv2.COLOR_BGR2GRAY)
    residual = cv2.absdiff(gray, prev)
    _, binary = cv2.threshold(residual, residual_threshold, 255, cv2.THRESH_BINARY)
    binary = cv2.morphologyEx(binary, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
    binary = cv2.morphologyEx(binary, cv2.MORPH_CLOSE, np.ones((5, 5), np.uint8))
    num, cc = cv2.connectedComponents(binary)
    labels = np.zeros(image.shape[:2], dtype=np.int32)
    out = 1
    for label in range(1, num):
        area = int((cc == label).sum())
        if area < min_area:
            continue
        labels[cc == label] = out
        out += 1
    return labels


def list_parts(
    image: NDArray[Any],
    mask: NDArray[Any],
    *,
    min_area: int = 25,
) -> list[SegmentInstance]:
    """Contour / part decomposition of a single object mask.

    Each external contour and each significant hole becomes a part. Authority
    remains SENSOR_DERIVED.
    """
    bgr = _as_bgr(image)
    m = (np.asarray(mask) > 0).astype(np.uint8)
    if m.shape[:2] != bgr.shape[:2]:
        raise ValidationError("mask shape must match image")
    contours, hierarchy = cv2.findContours(m, cv2.RETR_CCOMP, cv2.CHAIN_APPROX_SIMPLE)
    if hierarchy is None:
        return []
    hierarchy = hierarchy[0]
    parts: list[SegmentInstance] = []
    part_idx = 1
    for i, contour in enumerate(contours):
        area = int(abs(cv2.contourArea(contour)))
        if area < min_area:
            continue
        part_mask = np.zeros(m.shape, dtype=np.uint8)
        # hierarchy: [next, prev, child, parent]; parent < 0 => external.
        thickness = -1
        cv2.drawContours(part_mask, contours, i, 1, thickness)
        parent = int(hierarchy[i][3])
        part_of = "exterior" if parent < 0 else f"hole-of-{parent}"
        parts.append(
            SegmentInstance(
                segment_id=f"part-{part_idx}",
                label=part_idx,
                area_px=area,
                bbox_xywh=_bbox_from_mask(part_mask),
                centroid_xy=_centroid(part_mask),
                mean_bgr=_mean_bgr(bgr, part_mask),
                appearance_hist=appearance_histogram(bgr, part_mask),
                method=SegmentationMethod.CONTOUR_PARTS,
                authority=SEGMENT_AUTHORITY_CEILING,
                part_of=part_of,
            )
        )
        part_idx += 1
    return parts


def segment(
    image: NDArray[Any],
    *,
    method: SegmentationMethod | str = SegmentationMethod.WATERSHED,
    seeds: list[tuple[int, int]] | None = None,
    box: tuple[int, int, int, int] | None = None,
    previous_image: NDArray[Any] | None = None,
    previous_result: SegmentationResult | None = None,
    min_area: int = 40,
    colour_radius: float = 18.0,
    residual_threshold: int = 18,
    max_regions: int = 32,
    result_id: str | None = None,
) -> tuple[SegmentationResult, ArrayI32]:
    """Run a classical segmentation method and optionally stabilise IDs.

    Returns the sealed ``SegmentationResult`` and the integer label map
    (0 = background). Never claims MODEL_DERIVED or open-vocabulary capability.
    """
    resolved = SegmentationMethod(method)
    bgr = _as_bgr(image)
    h, w = bgr.shape[:2]

    if resolved is SegmentationMethod.REGION_GROW:
        labels = _region_grow(
            bgr, list(seeds or []), colour_radius=colour_radius, min_area=min_area
        )
    elif resolved is SegmentationMethod.WATERSHED:
        labels = _watershed(bgr, min_area=min_area, max_regions=max_regions)
    elif resolved is SegmentationMethod.GRABCUT:
        if box is None:
            raise ValidationError("grabcut requires box=(x, y, w, h)")
        labels = _grabcut(bgr, box, min_area=min_area)
    elif resolved is SegmentationMethod.MOTION_COMPONENTS:
        if previous_image is None:
            raise ValidationError("motion_components requires previous_image")
        labels = _motion_components(
            bgr,
            _as_bgr(previous_image),
            min_area=min_area,
            residual_threshold=residual_threshold,
        )
    elif resolved is SegmentationMethod.CONTOUR_PARTS:
        # Whole-image contour decomposition on Otsu foreground.
        gray = cv2.cvtColor(bgr, cv2.COLOR_BGR2GRAY)
        _, thresh = cv2.threshold(gray, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)
        if float(gray[thresh == 0].mean() if np.any(thresh == 0) else 0) > float(
            gray[thresh > 0].mean() if np.any(thresh > 0) else 0
        ):
            thresh = cv2.bitwise_not(thresh)
        num, cc = cv2.connectedComponents((thresh > 0).astype(np.uint8))
        labels = cc.astype(np.int32)
        instances = _instances_from_labels(
            bgr, labels, resolved, min_area=min_area, id_prefix="seg"
        )
        instances, links, labels = _stabilise_identities(instances, previous_result, labels)
        return _seal_result(
            resolved,
            bgr,
            labels,
            instances,
            previous_result=previous_result,
            identity_links=links,
            result_id=result_id,
        )
    else:
        raise ValidationError(f"unsupported segmentation method {resolved!r}")

    instances = _instances_from_labels(
        bgr, labels, resolved, min_area=min_area, id_prefix="seg"
    )
    instances, links, labels = _stabilise_identities(instances, previous_result, labels)
    return _seal_result(
        resolved,
        bgr,
        labels,
        instances,
        previous_result=previous_result,
        identity_links=links,
        result_id=result_id,
    )


def _seal_result(
    method: SegmentationMethod,
    bgr: ArrayU8,
    labels: ArrayI32,
    instances: list[SegmentInstance],
    *,
    previous_result: SegmentationResult | None,
    identity_links: dict[str, str],
    result_id: str | None,
    concept_prompt: str | None = None,
    concept_resolution: ConceptResolution | None = None,
) -> tuple[SegmentationResult, ArrayI32]:
    h, w = bgr.shape[:2]
    # Lineage.authority_ceiling() calls derive(inputs) with proposed=INFERRED, so
    # any non-empty input_authorities would cap seal() at INFERRED. Provenance is
    # carried in inputs/parameters instead; authority is the honest ceiling.
    source_authorities = [AuthorityClass.OBSERVED.value]
    if previous_result is not None:
        source_authorities.append(previous_result.authority.value)
    authority = derive(source_authorities, proposed=SEGMENT_AUTHORITY_CEILING)
    from blender_vision.v2.authority import strength

    if strength(authority) > strength(SEGMENT_AUTHORITY_CEILING):
        authority = SEGMENT_AUTHORITY_CEILING

    result = SegmentationResult(
        id=result_id or f"seg-{method.value}-{_label_map_digest(labels)[:12]}",
        method=method,
        width=w,
        height=h,
        label_map_digest=_label_map_digest(labels),
        instances=instances,
        concept_prompt=concept_prompt,
        concept_resolution=concept_resolution,
        previous_result_id=previous_result.id if previous_result else None,
        identity_links=identity_links,
        authority=authority,
        lineage=Lineage(
            operation=f"ocular.segment.{method.value}",
            inputs=[previous_result.id] if previous_result else [],
            input_authorities=[],
            parameters={
                "method": method.value,
                "instance_count": len(instances),
                "authority_ceiling": SEGMENT_AUTHORITY_CEILING.value,
                "source_authorities": source_authorities,
                "derived_authority": authority.value,
            },
            limitations=[
                "classical only; no learned weights",
                "not open-vocabulary",
            ],
        ),
        uncertainty=Uncertainty(
            kind="segmentation-boundary",
            sigma=None,
            units=Units.PIXEL,
            basis="classical-heuristic",
            samples=len(instances),
        ),
    ).seal()
    return result, labels


def segment_concept(
    image: NDArray[Any],
    prompt: str,
    *,
    previous_image: NDArray[Any] | None = None,
    previous_result: SegmentationResult | None = None,
    min_area: int = 40,
    result_id: str | None = None,
) -> tuple[SegmentationResult, ArrayI32 | None]:
    """Resolve a text prompt through the local concept table only.

    Unknown prompts return UNRESOLVED with no guessed class. No vision-language
    model is claimed or invoked.
    """
    key = prompt.strip().lower()
    bgr = _as_bgr(image)
    h, w = bgr.shape[:2]
    if key not in LOCAL_CONCEPT_TABLE:
        empty = SegmentationResult(
            id=result_id or f"seg-concept-unresolved-{abs(hash(key)) % 10**8}",
            method=SegmentationMethod.WATERSHED,
            width=w,
            height=h,
            label_map_digest="",
            instances=[],
            concept_prompt=prompt,
            concept_resolution=ConceptResolution.UNRESOLVED,
            previous_result_id=previous_result.id if previous_result else None,
            authority=AuthorityClass.UNRESOLVED,
            lineage=Lineage(
                operation="ocular.segment_concept",
                input_authorities=[],
                parameters={
                    "prompt": prompt,
                    "concept_table": sorted(LOCAL_CONCEPT_TABLE),
                    "source_authorities": [AuthorityClass.OBSERVED.value],
                },
                limitations=[
                    "no vision-language model; local concept table only",
                    f"prompt {prompt!r} not in table",
                ],
            ),
            notes=[f"UNRESOLVED: {prompt!r} not in LOCAL_CONCEPT_TABLE"],
        ).seal()
        return empty, None

    concept = LOCAL_CONCEPT_TABLE[key]
    kind = concept["kind"]
    if kind == "colour":
        hsv = cv2.cvtColor(bgr, cv2.COLOR_BGR2HSV)
        low = np.array(concept["hsv_low"], dtype=np.uint8)
        high = np.array(concept["hsv_high"], dtype=np.uint8)
        mask = cv2.inRange(hsv, low, high)
        # Red wraps hue — local table entry covers low band only; extend for 'red'.
        if key == "red":
            high = np.array((180, 255, 255), dtype=np.uint8)
            mask2 = cv2.inRange(hsv, np.array((170, 80, 60), dtype=np.uint8), high)
            mask = cv2.bitwise_or(mask, mask2)
        num, cc = cv2.connectedComponents((mask > 0).astype(np.uint8))
        labels = cc.astype(np.int32)
        for label in range(1, num):
            if int((labels == label).sum()) < min_area:
                labels[labels == label] = 0
        # Relabel densely.
        instances = _instances_from_labels(
            bgr, labels, SegmentationMethod.REGION_GROW, min_area=min_area, id_prefix="concept"
        )
        instances, links, labels = _stabilise_identities(instances, previous_result, labels)
        result, labels = _seal_result(
            SegmentationMethod.REGION_GROW,
            bgr,
            labels,
            instances,
            previous_result=previous_result,
            identity_links=links,
            result_id=result_id,
            concept_prompt=prompt,
            concept_resolution=ConceptResolution.RESOLVED,
        )
        return result, labels

    if kind == "motion":
        if previous_image is None:
            empty = SegmentationResult(
                id=result_id or "seg-concept-motion-blocked",
                method=SegmentationMethod.MOTION_COMPONENTS,
                width=w,
                height=h,
                concept_prompt=prompt,
                concept_resolution=ConceptResolution.UNRESOLVED,
                authority=AuthorityClass.UNRESOLVED,
                lineage=Lineage(
                    operation="ocular.segment_concept",
                    parameters={"prompt": prompt},
                    limitations=["motion concept requires previous_image"],
                ),
                notes=["UNRESOLVED: motion concept needs previous_image"],
            ).seal()
            return empty, None
        result, labels = segment(
            bgr,
            method=SegmentationMethod.MOTION_COMPONENTS,
            previous_image=previous_image,
            previous_result=previous_result,
            min_area=min_area,
            result_id=result_id,
        )
        # Re-seal with concept metadata.
        result.concept_prompt = prompt
        result.concept_resolution = ConceptResolution.RESOLVED
        result.digest = ""
        result.seal()
        return result, labels

    # salient / object → watershed
    result, labels = segment(
        bgr,
        method=SegmentationMethod.WATERSHED,
        previous_result=previous_result,
        min_area=min_area,
        result_id=result_id,
    )
    result.concept_prompt = prompt
    result.concept_resolution = ConceptResolution.RESOLVED
    result.digest = ""
    result.seal()
    return result, labels
