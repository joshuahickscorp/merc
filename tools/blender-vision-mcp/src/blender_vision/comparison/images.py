from __future__ import annotations

import uuid
from pathlib import Path
from typing import Any

from PIL import Image, ImageChops

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore

MAX_COMPARE_EDGE = 2048


def _fit_for_comparison(image: Image.Image) -> Image.Image:
    image = image.copy()
    image.thumbnail((MAX_COMPARE_EDGE, MAX_COMPARE_EDGE), Image.Resampling.LANCZOS)
    return image


def silhouette_mask(image: Image.Image) -> Image.Image:
    rgba = _fit_for_comparison(image.convert("RGBA"))
    alpha = rgba.getchannel("A")
    extrema = alpha.getextrema()
    if extrema[0] != extrema[1]:
        return alpha.point(lambda value: 255 if value >= 16 else 0)
    width, height = rgba.size
    corner_size = max(1, min(width, height) // 50)
    samples = [
        rgba.crop((0, 0, corner_size, corner_size)),
        rgba.crop((width - corner_size, 0, width, corner_size)),
        rgba.crop((0, height - corner_size, corner_size, height)),
        rgba.crop((width - corner_size, height - corner_size, width, height)),
    ]
    total = [0, 0, 0]
    count = 0
    for sample in samples:
        pixels = sample.tobytes()
        for offset in range(0, len(pixels), 4):
            total[0] += pixels[offset]
            total[1] += pixels[offset + 1]
            total[2] += pixels[offset + 2]
        count += sample.width * sample.height
    background = tuple(round(value / max(1, count)) for value in total)
    flat = Image.new("RGB", rgba.size, background)
    difference = ImageChops.difference(rgba.convert("RGB"), flat).convert("L")
    return difference.point(lambda value: 255 if value >= 18 else 0)


def compare_pair(reference_path: Path, render_path: Path, residual_path: Path) -> dict[str, Any]:
    with Image.open(reference_path) as reference_image, Image.open(render_path) as render_image:
        reference = silhouette_mask(reference_image)
        render = silhouette_mask(render_image)
    render = render.resize(reference.size, Image.Resampling.NEAREST)
    reference_pixels = reference.load()
    render_pixels = render.load()
    overlap = union = reference_count = render_count = 0
    residual = Image.new("RGBA", reference.size, (0, 0, 0, 0))
    residual_pixels = residual.load()
    for y in range(reference.height):
        for x in range(reference.width):
            expected = reference_pixels[x, y] > 0
            observed = render_pixels[x, y] > 0
            reference_count += int(expected)
            render_count += int(observed)
            overlap += int(expected and observed)
            union += int(expected or observed)
            if expected and not observed:
                residual_pixels[x, y] = (255, 48, 48, 220)
            elif observed and not expected:
                residual_pixels[x, y] = (0, 190, 255, 220)
            elif expected and observed:
                residual_pixels[x, y] = (64, 255, 128, 64)
    residual_path.parent.mkdir(parents=True, exist_ok=True)
    residual.save(residual_path)
    intersection_over_union = overlap / union if union else 1.0
    precision = overlap / render_count if render_count else 0.0
    recall = overlap / reference_count if reference_count else 0.0
    return {
        "silhouette_iou": round(intersection_over_union, 6),
        "silhouette_precision": round(precision, 6),
        "silhouette_recall": round(recall, 6),
        "reference_pixels": reference_count,
        "render_pixels": render_count,
        "comparison_size": list(reference.size),
        "segmentation_method": "alpha_or_corner_background_difference",
    }


def compare_project_renders(project: ProjectStore, renders: list[dict[str, Any]]) -> dict[str, Any]:
    references = {item["id"]: item for item in ReferenceIngestor(project).list()}
    artifact_store = ArtifactStore(project)
    comparisons = []
    for render in renders:
        reference = references.get(render.get("reference_id"))
        if reference is None:
            continue
        reference_path = project.root / reference["relative_path"]
        render_path = project.root / render["path"]
        comparison_id = str(uuid.uuid4())
        residual_path = project.root / "comparisons" / f"residual_{comparison_id}.png"
        metrics = compare_pair(reference_path, render_path, residual_path)
        render_artifact = artifact_store.ingest_file(render_path, media_type="image/png")
        residual_artifact = artifact_store.ingest_file(residual_path, media_type="image/png")
        from blender_vision.comparison.store import ComparisonStore

        stored = ComparisonStore(project).record(
            comparison_id,
            reference_id=reference["id"],
            render_digest=render_artifact.digest,
            residual_digest=residual_artifact.digest,
            metrics=metrics,
            engine="compare_pair_v1",
        )
        comparisons.append(
            {
                "id": comparison_id,
                "reference_id": reference["id"],
                "reference": reference["relative_path"],
                "render": render["path"],
                "residual": str(residual_path.relative_to(project.root)),
                "render_digest": render_artifact.digest,
                "residual_digest": residual_artifact.digest,
                "metrics": metrics,
                "receipt": stored["receipt_artifact"],
            }
        )
    ious = [comparison["metrics"]["silhouette_iou"] for comparison in comparisons]
    return {
        "comparisons": comparisons,
        "summary": {
            "count": len(comparisons),
            "mean_silhouette_iou": round(sum(ious) / len(ious), 6) if ious else None,
            "minimum_silhouette_iou": min(ious) if ious else None,
        },
    }
