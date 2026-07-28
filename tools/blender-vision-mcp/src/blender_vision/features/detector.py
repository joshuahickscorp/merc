from __future__ import annotations

import math
from pathlib import Path
from typing import Any

from PIL import Image

from blender_vision.core.models import EvidenceClass
from blender_vision.features.store import FeatureStore
from blender_vision.projects.store import ProjectStore


def detect_label_mask(path: Path, color_map: dict[str, list[int]]) -> list[dict[str, Any]]:
    """Recover perfect synthetic labels from a deterministic RGB feature-ID pass."""
    with Image.open(path) as image:
        rgb = image.convert("RGB")
        width, height = rgb.size
        pixels = list(rgb.get_flattened_data())
    detections = []
    for feature_type, color in color_map.items():
        if len(color) != 3:
            raise ValueError("synthetic label colors must be RGB triples")
        target = tuple(int(value) for value in color)
        coordinates = [
            (index % width, index // width) for index, pixel in enumerate(pixels) if pixel == target
        ]
        if not coordinates:
            continue
        xs = [point[0] for point in coordinates]
        ys = [point[1] for point in coordinates]
        detections.append(
            {
                "type": feature_type,
                "bounding_box_xyxy": [min(xs), min(ys), max(xs) + 1, max(ys) + 1],
                "centroid_xy": [sum(xs) / len(xs), sum(ys) / len(ys)],
                "pixel_count": len(coordinates),
                "confidence": 1.0,
            }
        )
    return detections


class FeatureDetectionImporter:
    def __init__(self, project: ProjectStore):
        self.project = project

    def import_detections(
        self,
        reference_id: str,
        detections: list[dict[str, Any]],
        *,
        model_revision: str,
        license_record: dict[str, Any],
    ) -> dict[str, Any]:
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT artifact_digest FROM reference_items WHERE id=?", (reference_id,)
            ).fetchone()
        if reference is None:
            raise ValueError("feature detections reference unknown image evidence")
        if not model_revision.strip() or not license_record.get("license"):
            raise ValueError("feature detections require model revision and license")
        commercial = bool(
            license_record.get("commercial_use") is True
            and not license_record.get("research_only", False)
        )
        store = FeatureStore(self.project)
        imported = []
        for detection in detections:
            confidence = float(detection["confidence"])
            if not math.isfinite(confidence) or not 0.0 <= confidence <= 1.0:
                raise ValueError("feature detection confidence must be between zero and one")
            evidence_class = (
                EvidenceClass.INFERRED_HIGH_CONFIDENCE
                if commercial and confidence >= 0.8
                else EvidenceClass.INFERRED_LOW_CONFIDENCE
            )
            imported.append(
                store.add(
                    detection["type"],
                    parent_component=detection.get("parent_component", "unassigned"),
                    observations=[
                        {
                            "kind": "model_detection",
                            "reference_id": reference_id,
                            "reference_artifact_digest": reference["artifact_digest"],
                            "bounding_box_xyxy": detection.get("bounding_box_xyxy"),
                            "mask_artifact_digest": detection.get("mask_artifact_digest"),
                            "keypoints": detection.get("keypoints", []),
                            "orientation": detection.get("orientation"),
                        }
                    ],
                    reference_ids=[reference_id],
                    confidence=confidence,
                    evidence_class=evidence_class,
                    uncertainty={
                        "model_generated": True,
                        "requires_human_review": True,
                        "commercial_eligible": commercial,
                    },
                    dimensions=detection.get("dimensions", {}),
                    model_revision=model_revision,
                    coverage_group=detection.get("coverage_group"),
                    hero_surface=bool(detection.get("hero_surface", False)),
                    provenance=[
                        {
                            "kind": "feature_model",
                            "model_revision": model_revision,
                            "license": license_record,
                        }
                    ],
                )
            )
        return {
            "reference_id": reference_id,
            "model_revision": model_revision,
            "commercial_eligible": commercial,
            "features": imported,
            "human_approval": False,
        }
