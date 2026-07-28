from __future__ import annotations

import threading
from pathlib import Path
from typing import Any

from PIL import Image, ImageFilter, ImageOps

_OPENCV_GRABCUT_LOCK = threading.Lock()
LEGACY_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION = 1024
BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION = 512


def _opencv_diagram_outline_mask(
    image: Image.Image,
    *,
    maximum_dimension: int = LEGACY_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION,
) -> Image.Image | None:
    """Isolate a principal chassis outline from low-saturation annotated diagrams."""
    try:
        import cv2  # type: ignore[import-not-found]
        import numpy as np  # type: ignore[import-not-found]
    except ImportError:
        return None

    rgb = ImageOps.exif_transpose(image).convert("RGB")
    width, height = rgb.size
    scale = min(1.0, maximum_dimension / max(width, height))
    working = rgb.resize(
        (max(2, round(width * scale)), max(2, round(height * scale))),
        Image.Resampling.LANCZOS,
    )
    array = np.asarray(working)
    work_height, work_width = array.shape[:2]
    hsv = cv2.cvtColor(array, cv2.COLOR_RGB2HSV)
    grayscale = cv2.cvtColor(array, cv2.COLOR_RGB2GRAY)
    # Product diagrams are nearly achromatic and have a bright page background
    # with sparse dark linework.  These guards prevent this path from treating
    # normal product photography as a technical outline.
    if float(np.percentile(hsv[:, :, 1], 90)) > 28.0:
        return None
    corner = max(2, round(min(work_width, work_height) * 0.03))
    border_samples = np.concatenate(
        (
            grayscale[:corner, :].ravel(),
            grayscale[-corner:, :].ravel(),
            grayscale[:, :corner].ravel(),
            grayscale[:, -corner:].ravel(),
        )
    )
    if float(np.median(border_samples)) < 235.0:
        return None
    dark = (grayscale < 220).astype(np.uint8)
    dark_fraction = float(dark.mean())
    if not 0.005 <= dark_fraction <= 0.25:
        return None
    kernel_size = max(3, round(min(work_width, work_height) / 110))
    kernel_size += 1 - kernel_size % 2
    opened = cv2.morphologyEx(
        dark,
        cv2.MORPH_OPEN,
        cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (kernel_size, kernel_size)),
    )
    component_count, labels, stats, _centroids = cv2.connectedComponentsWithStats(
        opened, 8
    )
    candidates: list[tuple[int, int]] = []
    for index in range(1, component_count):
        x, y, box_width, box_height, area = (
            int(value) for value in stats[index]
        )
        box_area = box_width * box_height
        if (
            box_width >= round(work_width * 0.50)
            and box_height >= round(work_height * 0.15)
            and box_area >= round(work_width * work_height * 0.08)
            and area / max(1, box_area) <= 0.18
            and y + box_height <= round(work_height * 0.85)
        ):
            candidates.append((box_area, index))
    if not candidates:
        return None
    _box_area, selected = max(candidates)
    points_yx = np.column_stack(np.where(labels == selected))
    if len(points_yx) < 3:
        return None
    points_xy = points_yx[:, ::-1].astype(np.int32)
    hull = cv2.convexHull(points_xy)
    hull_area = float(cv2.contourArea(hull))
    image_area = float(work_width * work_height)
    if not image_area * 0.08 <= hull_area <= image_area * 0.75:
        return None
    foreground = np.zeros((work_height, work_width), dtype=np.uint8)
    cv2.fillConvexPoly(foreground, hull, 255)
    result = Image.fromarray(foreground, mode="L").resize(
        (width, height), Image.Resampling.NEAREST
    )
    result.info["bvmcp_segmentation_method"] = (
        "opencv_diagram_principal_outline_annotation_suppression"
    )
    return result


def _suppress_horizontal_shadow_wings(
    foreground: Any, *, edge_map: Any | None = None
) -> tuple[Any, bool]:
    """Bound studio-floor shadows when a tall, narrow foreground core is unambiguous."""
    import numpy as np  # type: ignore[import-not-found]

    height, width = foreground.shape
    column_counts = foreground.sum(axis=0)
    peak = int(column_counts.max(initial=0))
    if peak < round(height * 0.55):
        return foreground, False
    core_columns = np.flatnonzero(
        column_counts >= max(round(height * 0.45), round(peak * 0.70))
    )
    if core_columns.size == 0:
        return foreground, False
    breaks = np.flatnonzero(np.diff(core_columns) > 1) + 1
    runs = np.split(core_columns, breaks)
    core = max(runs, key=lambda run: int(run.size))
    if core.size < max(3, round(width * 0.02)):
        return foreground, False
    core_width = int(core[-1] - core[0] + 1)
    if core_width > round(width * 0.50):
        return foreground, False
    expansion = max(2, round(core_width * 0.02))
    left = max(0, int(core[0]) - expansion)
    right = min(width, int(core[-1]) + expansion + 1)
    bounded = foreground.copy()
    bounded[:, :left] = False
    bounded[:, right:] = False
    if edge_map is not None:
        row_edge_counts = edge_map[:, left:right].sum(axis=1)
        edge_rows = np.flatnonzero(
            row_edge_counts >= max(3, round((right - left) * 0.015))
        )
        if edge_rows.size:
            top = max(0, int(edge_rows[0]) - 2)
            bottom = min(height, int(edge_rows[-1]) + 3)
            if bottom - top >= round(height * 0.40):
                bounded[:top, :] = False
                bounded[bottom:, :] = False
    removed_fraction = float(np.logical_and(foreground, np.logical_not(bounded)).mean())
    if removed_fraction < 0.01 or float(bounded.mean()) < 0.005:
        return foreground, False
    return bounded, True


def _edge_validated_principal_grabcut_component(
    foreground: Any, suppressed_foreground: Any, *, edge_map: Any
) -> Any | None:
    """Restore a wide GrabCut component when a shadow heuristic crops it.

    A strong exterior edge component is used only as a cross-check, never as the
    silhouette itself: internal product edges can form plausible but false closed
    contours.  Returning only the largest original GrabCut component also excludes
    detached accessories that happen to share the studio background.
    """
    import cv2  # type: ignore[import-not-found]
    import numpy as np  # type: ignore[import-not-found]

    height, width = foreground.shape
    kernel_size = max(3, round(min(width, height) / 150))
    kernel_size += 1 - kernel_size % 2
    kernel = cv2.getStructuringElement(
        cv2.MORPH_ELLIPSE, (kernel_size, kernel_size)
    )
    connected_edges = cv2.morphologyEx(
        edge_map.astype(np.uint8), cv2.MORPH_CLOSE, kernel
    )
    connected_edges = cv2.dilate(
        connected_edges,
        cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (3, 3)),
        iterations=1,
    )

    component_count, labels, stats, _centroids = cv2.connectedComponentsWithStats(
        connected_edges, 8
    )
    edge_candidates: list[tuple[int, int]] = []
    for index in range(1, component_count):
        x, y, box_width, box_height, area = (
            int(value) for value in stats[index]
        )
        box_area = box_width * box_height
        if (
            box_width >= round(width * 0.55)
            and box_height >= round(height * 0.20)
            and box_width <= round(width * 0.98)
            and box_height <= round(height * 0.90)
            and box_area >= round(width * height * 0.12)
            and area >= round(width * height * 0.003)
            and y + box_height < height
        ):
            edge_candidates.append((box_area, index))
    if not edge_candidates:
        return None
    _box_area, edge_index = max(edge_candidates)
    edge_x, edge_y, edge_width, edge_height, _edge_area = (
        int(value) for value in stats[edge_index]
    )

    original_count, original_labels, original_stats, _original_centroids = (
        cv2.connectedComponentsWithStats(foreground.astype(np.uint8), 8)
    )
    suppressed_count, _suppressed_labels, suppressed_stats, _suppressed_centroids = (
        cv2.connectedComponentsWithStats(suppressed_foreground.astype(np.uint8), 8)
    )
    if original_count <= 1 or suppressed_count <= 1:
        return None
    original_index = max(
        range(1, original_count), key=lambda index: int(original_stats[index, 4])
    )
    suppressed_index = max(
        range(1, suppressed_count),
        key=lambda index: int(suppressed_stats[index, 4]),
    )
    original_x, original_y, original_width, original_height, original_area = (
        int(value) for value in original_stats[original_index]
    )
    suppressed_width = int(suppressed_stats[suppressed_index, 2])
    if suppressed_width >= round(edge_width * 0.75):
        return None
    if not round(edge_width * 0.75) <= original_width <= round(edge_width * 1.05):
        return None
    if not round(edge_height * 0.70) <= original_height <= round(edge_height * 1.10):
        return None
    intersection_width = max(
        0,
        min(original_x + original_width, edge_x + edge_width)
        - max(original_x, edge_x),
    )
    intersection_height = max(
        0,
        min(original_y + original_height, edge_y + edge_height)
        - max(original_y, edge_y),
    )
    intersection = intersection_width * intersection_height
    union = original_width * original_height + edge_width * edge_height - intersection
    if intersection / max(1, union) < 0.55:
        return None
    image_area = width * height
    if not round(image_area * 0.05) <= original_area <= round(image_area * 0.75):
        return None
    return original_labels == original_index


def _opencv_grabcut_mask(
    image: Image.Image,
    *,
    maximum_dimension: int = LEGACY_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION,
) -> Image.Image | None:
    """Return an automatic foreground proposal when the vision extra is installed.

    GrabCut is deliberately treated as medium-authority evidence: it is substantially
    more useful than a single border-colour threshold on studio gradients, but it is
    still an unreviewed segmentation and therefore cannot by itself establish L3.
    """
    try:
        import cv2  # type: ignore[import-not-found]
        import numpy as np  # type: ignore[import-not-found]
    except ImportError:
        return None

    rgb = ImageOps.exif_transpose(image).convert("RGB")
    width, height = rgb.size
    scale = min(1.0, maximum_dimension / max(width, height))
    working = rgb.resize(
        (max(2, round(width * scale)), max(2, round(height * scale))),
        Image.Resampling.LANCZOS,
    )
    array = np.asarray(working)
    work_height, work_width = array.shape[:2]
    margin = max(2, round(min(work_width, work_height) * 0.03))
    if work_width <= margin * 2 or work_height <= margin * 2:
        return None
    mask = np.zeros((work_height, work_width), np.uint8)
    background_model = np.zeros((1, 65), np.float64)
    foreground_model = np.zeros((1, 65), np.float64)
    try:
        with _OPENCV_GRABCUT_LOCK:
            cv2.setRNGSeed(0)
            cv2.grabCut(
                array,
                mask,
                (margin, margin, work_width - margin * 2, work_height - margin * 2),
                background_model,
                foreground_model,
                6,
                cv2.GC_INIT_WITH_RECT,
            )
    except cv2.error:
        return None
    original_foreground = np.logical_or(mask == cv2.GC_FGD, mask == cv2.GC_PR_FGD)
    grayscale = cv2.cvtColor(array, cv2.COLOR_RGB2GRAY)
    edge_map = cv2.Canny(grayscale, 40, 120) > 0
    foreground, shadow_wings_suppressed = _suppress_horizontal_shadow_wings(
        original_foreground, edge_map=edge_map
    )
    restored_foreground = _edge_validated_principal_grabcut_component(
        original_foreground, foreground, edge_map=edge_map
    )
    if restored_foreground is not None:
        foreground = restored_foreground
    foreground_fraction = float(foreground.mean())
    if not 0.005 <= foreground_fraction <= 0.9:
        return None
    binary = Image.fromarray((foreground.astype(np.uint8) * 255), mode="L")
    binary = binary.filter(ImageFilter.MaxFilter(3)).filter(ImageFilter.MinFilter(3))
    result = binary.resize((width, height), Image.Resampling.NEAREST)
    if restored_foreground is not None:
        result.info["bvmcp_segmentation_method"] = (
            "opencv_grabcut_principal_component_edge_validation"
        )
    elif shadow_wings_suppressed:
        result.info["bvmcp_segmentation_method"] = (
            "opencv_grabcut_rect_shadow_wing_suppression"
        )
    return result


def _corner_patch_median(
    rgb: Image.Image, x_start: int, y_start: int, size: int
) -> tuple[int, ...]:
    crop = rgb.crop((x_start, y_start, x_start + size, y_start + size))
    channels = len(crop.getbands())
    raw = crop.tobytes()
    pixels = [tuple(raw[index : index + channels]) for index in range(0, len(raw), channels)]
    pixels.sort(key=lambda pixel: sum(pixel))
    return pixels[len(pixels) // 2]


def _bilinear_background_mask(image: Image.Image) -> Image.Image:
    """Estimate a smooth studio background from robust corner patches."""
    rgb = ImageOps.exif_transpose(image).convert("RGB")
    width, height = rgb.size
    patch = max(1, min(32, width // 12, height // 12))
    corners = (
        _corner_patch_median(rgb, 0, 0, patch),
        _corner_patch_median(rgb, width - patch, 0, patch),
        _corner_patch_median(rgb, 0, height - patch, patch),
        _corner_patch_median(rgb, width - patch, height - patch, patch),
    )
    source = rgb.load()
    mask = Image.new("L", rgb.size)
    target = mask.load()
    x_denominator = max(1, width - 1)
    y_denominator = max(1, height - 1)
    threshold = 32.0
    for y in range(height):
        vertical = y / y_denominator
        for x in range(width):
            horizontal = x / x_denominator
            expected = tuple(
                (1.0 - vertical)
                * ((1.0 - horizontal) * corners[0][channel] + horizontal * corners[1][channel])
                + vertical
                * ((1.0 - horizontal) * corners[2][channel] + horizontal * corners[3][channel])
                for channel in range(3)
            )
            pixel = source[x, y]
            distance = sum(
                (pixel[channel] - expected[channel]) ** 2 for channel in range(3)
            ) ** 0.5
            target[x, y] = 255 if distance >= threshold else 0
    return mask.filter(ImageFilter.MaxFilter(3)).filter(ImageFilter.MinFilter(3))


def _reference_mask(
    image: Image.Image,
    *,
    automatic_segmentation_maximum_dimension: int = (
        LEGACY_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
    ),
) -> tuple[Image.Image, str, str]:
    rgba = ImageOps.exif_transpose(image).convert("RGBA")
    alpha = rgba.getchannel("A")
    minimum, maximum = alpha.getextrema()
    if minimum < maximum:
        return (
            alpha.point(lambda value: 255 if value >= 128 else 0),
            "embedded_alpha",
            "high",
        )
    diagram = _opencv_diagram_outline_mask(
        rgba, maximum_dimension=automatic_segmentation_maximum_dimension
    )
    if diagram is not None:
        method = diagram.info.get(
            "bvmcp_segmentation_method", "opencv_diagram_principal_outline"
        )
        return diagram, str(method), "medium"
    grabcut = _opencv_grabcut_mask(
        rgba, maximum_dimension=automatic_segmentation_maximum_dimension
    )
    if grabcut is not None:
        method = grabcut.info.get("bvmcp_segmentation_method", "opencv_grabcut_rect")
        return grabcut, str(method), "medium"
    return _bilinear_background_mask(rgba), "bilinear_corner_background", "low"


def _render_mask(image: Image.Image) -> Image.Image:
    rgba = image.convert("RGBA")
    alpha = rgba.getchannel("A")
    if alpha.getextrema()[0] < 255:
        return alpha.point(lambda value: 255 if value >= 8 else 0)
    gray = rgba.convert("L")
    return gray.point(lambda value: 255 if value > 2 else 0)


def _mask_contacts_adjacent_borders(
    mask: Image.Image, *, tolerance_fraction: float = 0.035
) -> bool:
    """Return whether a foreground mask is clipped near adjacent image edges."""
    bbox = mask.getbbox()
    if bbox is None:
        return False
    width, height = mask.size
    left, top, right, bottom = bbox
    horizontal_contact = (
        left <= round(width * tolerance_fraction)
        or width - right <= round(width * tolerance_fraction)
    )
    vertical_contact = (
        top <= round(height * tolerance_fraction)
        or height - bottom <= round(height * tolerance_fraction)
    )
    return horizontal_contact and vertical_contact


def compare_silhouettes(
    reference_path: Path,
    render_path: Path,
    residual_path: Path,
    *,
    reviewed_mask_path: Path | None = None,
    reviewed_mask_record: dict[str, Any] | None = None,
    automatic_segmentation_maximum_dimension: int = (
        LEGACY_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
    ),
    prepared_automatic_reference_mask: tuple[Image.Image, str, str] | None = None,
) -> dict[str, Any]:
    if automatic_segmentation_maximum_dimension < 32:
        raise ValueError("automatic segmentation maximum dimension must be at least 32")
    with Image.open(reference_path) as reference_image, Image.open(render_path) as render_image:
        reference_size = ImageOps.exif_transpose(reference_image).size
        if reviewed_mask_path is not None:
            if prepared_automatic_reference_mask is not None:
                raise ValueError("reviewed and prepared automatic masks are mutually exclusive")
            with Image.open(reviewed_mask_path) as reviewed_image:
                reference_mask = ImageOps.exif_transpose(reviewed_image).convert("L")
                if reference_mask.size != reference_size:
                    raise ValueError("reviewed reference mask dimensions do not match reference")
                reference_mask = reference_mask.point(lambda value: 255 if value >= 128 else 0)
            segmentation_method = (
                str(reviewed_mask_record["method"])
                if reviewed_mask_record is not None
                else "reviewed_manual_mask"
            )
            segmentation_confidence = "high"
        else:
            if prepared_automatic_reference_mask is None:
                reference_mask, segmentation_method, segmentation_confidence = (
                    _reference_mask(
                        reference_image,
                        automatic_segmentation_maximum_dimension=(
                            automatic_segmentation_maximum_dimension
                        ),
                    )
                )
            else:
                reference_mask, segmentation_method, segmentation_confidence = (
                    prepared_automatic_reference_mask
                )
                if reference_mask.size != reference_size:
                    raise ValueError("prepared automatic mask dimensions do not match reference")
                reference_mask = reference_mask.copy()
        partial_object_crop = _mask_contacts_adjacent_borders(reference_mask)
        render_mask = _render_mask(render_image).resize(
            reference_mask.size, Image.Resampling.NEAREST
        )
    reference_values = reference_mask.tobytes()
    render_values = render_mask.tobytes()
    true_positive = false_positive = false_negative = true_negative = 0
    residual = Image.new("RGBA", reference_mask.size, (0, 0, 0, 0))
    residual_pixels = residual.load()
    width, height = reference_mask.size
    for index, (observed, predicted) in enumerate(
        zip(reference_values, render_values, strict=True)
    ):
        reference_on = observed >= 128
        render_on = predicted >= 128
        if reference_on and render_on:
            true_positive += 1
            color = (0, 255, 80, 32)
        elif render_on:
            false_positive += 1
            color = (255, 48, 32, 220)
        elif reference_on:
            false_negative += 1
            color = (48, 96, 255, 220)
        else:
            true_negative += 1
            color = (0, 0, 0, 0)
        residual_pixels[index % width, index // width] = color
    union = true_positive + false_positive + false_negative
    total = max(1, width * height)
    iou = true_positive / union if union else 1.0
    dice = 2 * true_positive / max(1, 2 * true_positive + false_positive + false_negative)
    residual_path.parent.mkdir(parents=True, exist_ok=True)
    residual.save(residual_path, format="PNG")
    result = {
        "silhouette_iou": round(iou, 8),
        "silhouette_dice": round(dice, 8),
        "pixel_accuracy": round((true_positive + true_negative) / total, 8),
        "false_positive_fraction": round(false_positive / total, 8),
        "false_negative_fraction": round(false_negative / total, 8),
        "reference_segmentation": segmentation_method,
        "reference_segmentation_confidence": segmentation_confidence,
        "reference_partial_object_crop": partial_object_crop,
        "counts": {
            "true_positive": true_positive,
            "false_positive": false_positive,
            "false_negative": false_negative,
            "true_negative": true_negative,
        },
    }
    if reviewed_mask_record is not None:
        result["reference_mask"] = {
            key: reviewed_mask_record[key]
            for key in (
                "id",
                "reference_id",
                "artifact_digest",
                "source_artifact_digest",
                "method",
                "reviewer",
                "reason",
                "created_at",
            )
        }
    return result
