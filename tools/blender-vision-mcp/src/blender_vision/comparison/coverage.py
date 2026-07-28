from __future__ import annotations

from typing import Any

from blender_vision.core.util import atomic_write_json
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore

DIRECTIONS = ("front", "rear", "left", "right", "top", "bottom")


def coverage_report(project: ProjectStore, camera_solution: dict[str, Any]) -> dict[str, Any]:
    references = {item["id"]: item for item in ReferenceIngestor(project).list()}
    observed: dict[str, list[str]] = {direction: [] for direction in DIRECTIONS}
    unclassified = []
    for camera in camera_solution.get("cameras", []):
        reference = references.get(camera["reference_id"], {})
        text = " ".join(
            filter(None, [reference.get("viewpoint_label"), reference.get("original_name")])
        ).lower()
        match = next((direction for direction in DIRECTIONS if direction in text), None)
        if match:
            observed[match].append(camera["reference_id"])
        else:
            unclassified.append(camera["reference_id"])
    covered = sum(bool(items) for items in observed.values())
    report = {
        "directions": observed,
        "unclassified": unclassified,
        "directional_coverage": round(covered / len(DIRECTIONS), 6),
        "missing_directions": [direction for direction, items in observed.items() if not items],
        "registered_reference_fraction": round(
            len(camera_solution.get("cameras", [])) / max(1, len(references)), 6
        ),
        "next_best_views": [
            {"direction": direction, "reason": "no approved reference covers this surface"}
            for direction, items in observed.items()
            if not items
        ],
    }
    atomic_write_json(project.root / "comparisons" / "coverage.json", report)
    return report
