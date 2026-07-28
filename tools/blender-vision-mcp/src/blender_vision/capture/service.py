from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from PIL import Image, ImageOps

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.capture.video import extract_video_frames
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore


class CaptureService:
    """Project-confined video extraction and deterministic frame triage."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.references = ReferenceIngestor(project)
        self.artifacts = ArtifactStore(project)

    def extract_video(
        self,
        source: Path,
        *,
        rights_state: str,
        interval_seconds: float = 1.0,
        maximum_frames: int = 300,
    ) -> dict[str, Any]:
        if rights_state == "UNKNOWN":
            raise ValueError("video extraction requires reviewed source rights")
        video = self.references.import_file(
            source,
            rights_state=rights_state,
            viewpoint_label="source video",
            evidence_role="source_video",
            acceptance_eligible=False,
        )
        output = self.project.root / "references" / "frames" / str(uuid.uuid4())
        extraction = extract_video_frames(
            source,
            output,
            interval_seconds=interval_seconds,
            maximum_frames=maximum_frames,
        )
        if not extraction["frames"]:
            raise RuntimeError("video extraction produced no decodable frames")
        frames = []
        for frame_record in extraction["frames"]:
            frame = self.references.import_file(
                Path(frame_record["path"]),
                rights_state=rights_state,
                viewpoint_label=f"video frame {frame_record['extraction_index']:06d}",
                evidence_role="diagnostic_video_frame",
                acceptance_eligible=False,
            )
            metadata = {
                **frame["metadata"],
                "video_source_reference_id": video["id"],
                "extraction_index": frame_record["extraction_index"],
                "timecode_seconds": frame_record["timecode_seconds"],
                "frame_linkage": "fixed-cadence extraction from immutable source artifact",
            }
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE reference_items SET metadata_json=? WHERE id=?",
                    (json.dumps(metadata), frame["id"]),
                )
            frames.append({**frame, "metadata": metadata})
        selection = self.select_frames(
            [frame["id"] for frame in frames],
            maximum_selected=min(24, len(frames)),
        )
        contact_sheet = self._contact_sheet(selection["selected_reference_ids"])
        from blender_vision.capture.intelligence import VideoIntelligenceService

        intelligence = VideoIntelligenceService(self.project).analyze(
            video["id"], maximum_selected=min(36, len(frames))
        )
        return {
            "source_reference": video,
            "extraction": {
                **{key: value for key, value in extraction.items() if key != "source"},
                "frames": [
                    {
                        "reference_id": frame["id"],
                        "extraction_index": frame["metadata"]["extraction_index"],
                        "timecode_seconds": frame["metadata"]["timecode_seconds"],
                    }
                    for frame in frames
                ],
            },
            "frame_references": frames,
            "selection": {**selection, "contact_sheet": contact_sheet},
            "intelligence": intelligence,
        }

    def select_frames(
        self,
        reference_ids: list[str] | None = None,
        *,
        maximum_selected: int = 24,
    ) -> dict[str, Any]:
        if not 1 <= maximum_selected <= 1000:
            raise ValueError("maximum_selected must be between 1 and 1000")
        requested = set(reference_ids or [])
        references = [
            item
            for item in self.references.list()
            if item["media_type"].startswith("image/")
            and (not requested or item["id"] in requested)
        ]
        if requested - {item["id"] for item in references}:
            raise KeyError("frame selection includes unknown or non-image reference ids")
        analyses: list[dict[str, Any]] = []
        previous_signature: list[int] | None = None
        previous_reference_id: str | None = None
        for reference in references:
            signature = self._signature(reference)
            difference = (
                sum(
                    abs(left - right)
                    for left, right in zip(signature, previous_signature, strict=True)
                )
                / (255.0 * len(signature))
                if signature and previous_signature
                else None
            )
            analysis = self._score(reference)
            analysis["temporal"] = {
                "previous_reference_id": previous_reference_id,
                "mean_absolute_frame_difference": (
                    round(difference, 8) if difference is not None else None
                ),
                "scene_cut": bool(difference is not None and difference >= 0.2),
                "near_duplicate": bool(difference is not None and difference <= 0.01),
                "optical_flow_diversity": self._optical_flow_diversity(
                    reference, previous_reference_id
                ),
                "rolling_shutter_warning": (
                    "possible_high_motion_capture; inspect line skew"
                    if difference is not None and difference >= 0.35
                    else None
                ),
            }
            if analysis["temporal"]["near_duplicate"]:
                analysis["warnings"].append("near_duplicate")
            analyses.append(analysis)
            previous_signature = signature
            previous_reference_id = reference["id"]
        ranked = sorted(
            analyses,
            key=lambda item: (-item["score"], item["reference_id"]),
        )
        unique = [item for item in ranked if not item["temporal"]["near_duplicate"]]
        duplicates = [item for item in ranked if item["temporal"]["near_duplicate"]]
        selected = unique[:maximum_selected]
        rejected = unique[maximum_selected:] + duplicates
        selected_ids = [item["reference_id"] for item in selected]
        considered_ids = [item["reference_id"] for item in analyses]
        with self.project.connection() as connection:
            connection.executemany(
                "UPDATE reference_items SET evidence_role='diagnostic_rejected_frame',"
                "acceptance_eligible=0 WHERE id=?",
                [(reference_id,) for reference_id in considered_ids],
            )
            connection.executemany(
                "UPDATE reference_items SET evidence_role='selected_keyframe',"
                "acceptance_eligible=1 WHERE id=?",
                [(reference_id,) for reference_id in selected_ids],
            )
        return {
            "selected_reference_ids": selected_ids,
            "selected": selected,
            "rejected": rejected,
            "policy": {
                "deterministic": True,
                "signals": [
                    "decode",
                    "resolution",
                    "edge_variance",
                    "exposure",
                    "clipping",
                    "scene_cut",
                    "near_duplicate",
                    "optical_flow_diversity",
                    "rolling_shutter_warning",
                ],
                "human_review_required": True,
            },
        }

    def _signature(self, reference: dict[str, Any]) -> list[int]:
        path = self.project.root / reference["relative_path"]
        try:
            with Image.open(path) as image:
                return list(ImageOps.grayscale(image).resize((16, 16)).tobytes())
        except Exception:
            return []

    def _optical_flow_diversity(
        self, reference: dict[str, Any], previous_reference_id: str | None
    ) -> dict[str, Any]:
        if previous_reference_id is None:
            return {"method": "not_applicable", "mean_magnitude": None}
        by_id = {item["id"]: item for item in self.references.list()}
        previous = by_id.get(previous_reference_id)
        if previous is None:
            return {"method": "unavailable", "mean_magnitude": None}
        try:
            import cv2
            import numpy as np

            with (
                Image.open(self.project.root / previous["relative_path"]) as first_image,
                Image.open(self.project.root / reference["relative_path"]) as second_image,
            ):
                first = np.asarray(ImageOps.grayscale(first_image).resize((96, 96)))
                second = np.asarray(ImageOps.grayscale(second_image).resize((96, 96)))
            flow = cv2.calcOpticalFlowFarneback(first, second, None, 0.5, 3, 15, 3, 5, 1.2, 0)
            magnitude = np.sqrt(flow[..., 0] ** 2 + flow[..., 1] ** 2)
            return {
                "method": "farneback",
                "mean_magnitude": round(float(magnitude.mean()), 8),
            }
        except (ImportError, ValueError, OSError):
            first = self._signature(previous)
            second = self._signature(reference)
            difference = (
                sum(abs(left - right) for left, right in zip(first, second, strict=True))
                / (255.0 * len(first))
                if first and second
                else None
            )
            return {
                "method": "mean_absolute_frame_difference_proxy",
                "mean_magnitude": round(difference, 8) if difference is not None else None,
            }

    def _contact_sheet(self, reference_ids: list[str]) -> dict[str, Any] | None:
        if not reference_ids:
            return None
        by_id = {item["id"]: item for item in self.references.list()}
        cell_width, cell_height, columns = 240, 180, 4
        rows = math.ceil(len(reference_ids) / columns)
        sheet = Image.new("RGB", (cell_width * columns, cell_height * rows), (24, 27, 33))
        for index, reference_id in enumerate(reference_ids):
            reference = by_id[reference_id]
            with Image.open(self.project.root / reference["relative_path"]) as image:
                thumbnail = ImageOps.contain(image.convert("RGB"), (cell_width, cell_height))
            x = (index % columns) * cell_width + (cell_width - thumbnail.width) // 2
            y = (index // columns) * cell_height + (cell_height - thumbnail.height) // 2
            sheet.paste(thumbnail, (x, y))
        relative = Path("comparisons") / f"contact-sheet-{uuid.uuid4()}.jpg"
        destination = self.project.root / relative
        sheet.save(destination, quality=88, optimize=True)
        artifact = self.artifacts.ingest_file(destination, media_type="image/jpeg")
        return {"path": str(relative), "artifact": artifact.to_dict()}

    @staticmethod
    def _score(reference: dict[str, Any]) -> dict[str, Any]:
        metadata = reference.get("metadata") or {}
        quality = reference.get("quality") or {}
        decoded = bool(quality.get("decode_ok"))
        area = max(1, int(metadata.get("width", 0)) * int(metadata.get("height", 0)))
        edge_variance = max(0.0, float(quality.get("edge_variance", 0.0)))
        exposure = float(quality.get("exposure_mean", 0.5))
        clipped = float(quality.get("clipped_black_fraction", 0.0)) + float(
            quality.get("clipped_white_fraction", 0.0)
        )
        score = (
            (4.0 if decoded else -100.0)
            + math.log2(area) * 0.2
            + math.log1p(edge_variance) * 0.7
            - abs(exposure - 0.5) * 4.0
            - clipped * 8.0
        )
        warnings = [
            name
            for name, active in (
                ("decode_failed", not decoded),
                ("blur", bool(quality.get("blur_warning"))),
                ("exposure", bool(quality.get("exposure_warning"))),
                ("clipping", clipped > 0.1),
            )
            if active
        ]
        return {
            "reference_id": reference["id"],
            "score": round(score, 6),
            "warnings": warnings,
            "viewpoint_label": reference.get("viewpoint_label"),
        }
