from __future__ import annotations

import json
import uuid
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.capture.service import CaptureService
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.features.store import FeatureStore
from blender_vision.projects.store import ProjectStore


class VideoIntelligenceService:
    """Classify extracted frames and persist evidence-bound keyframes and tracks."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.references = ReferenceIngestor(project)
        self.artifacts = ArtifactStore(project)

    def analyze(self, source_reference_id: str, *, maximum_selected: int = 36) -> dict[str, Any]:
        references = self.references.list()
        source = next((item for item in references if item["id"] == source_reference_id), None)
        if source is None or not source["media_type"].startswith("video/"):
            raise ValueError("video intelligence requires an ingested video reference")
        frames = sorted(
            [
                item
                for item in references
                if item.get("metadata", {}).get("video_source_reference_id") == source_reference_id
            ],
            key=lambda item: item["metadata"].get("extraction_index", 0),
        )
        if not frames:
            raise ValueError("video source has no extracted frame references")
        selection = CaptureService(self.project).select_frames(
            [item["id"] for item in frames], maximum_selected=maximum_selected
        )
        analysis_by_id = {
            item["reference_id"]: item for item in [*selection["selected"], *selection["rejected"]]
        }
        frame_records = [self._frame_record(frame, analysis_by_id[frame["id"]]) for frame in frames]
        initially_selected_ids = set(selection["selected_reference_ids"])
        shots = self._shots(frame_records, initially_selected_ids)
        best_shot = max(shots, key=lambda item: item["reconstruction_score"])
        selected_ids = set(best_shot["selected_reference_ids"])
        selected = [item for item in frame_records if item["reference_id"] in selected_ids]
        with self.project.connection() as connection:
            connection.executemany(
                "UPDATE reference_items SET evidence_role='diagnostic_video_frame',"
                "acceptance_eligible=0 WHERE id=?",
                [(item["reference_id"],) for item in frame_records],
            )
            connection.executemany(
                "UPDATE reference_items SET evidence_role='acceptance_video_keyframe',"
                "acceptance_eligible=1 WHERE id=?",
                [(reference_id,) for reference_id in sorted(selected_ids)],
            )
        tracks = self._feature_tracks(source_reference_id, selected_ids)
        contact_sheet = CaptureService(self.project)._contact_sheet(
            selection["selected_reference_ids"]
        )
        run_id = str(uuid.uuid4())
        created_at = utc_now()
        report = {
            "schema_version": 1,
            "id": run_id,
            "source_reference_id": source_reference_id,
            "created_at": created_at,
            "frame_count": len(frames),
            "selected_keyframes": selected,
            "candidate_keyframe_ids": sorted(initially_selected_ids),
            "all_frames": frame_records,
            "shots": shots,
            "best_reconstruction_shot_id": best_shot["id"],
            "acceptance_reference_ids": sorted(selected_ids),
            "camera_trajectory": {
                "method": "ordered_optical_flow_motion_proxy",
                "metric_authority": False,
                "samples": [
                    {
                        "reference_id": item["reference_id"],
                        "timecode_seconds": item["timecode_seconds"],
                        "motion_magnitude": item["motion_magnitude"],
                    }
                    for item in selected
                ],
            },
            "feature_tracks": tracks,
            "object_masks": [],
            "depth_sequence": [],
            "normal_sequence": [],
            "unavailable_products": {
                "object_masks": "no approved segmentation backend output was supplied",
                "depth_sequence": "no approved depth backend output was supplied",
                "normal_sequence": "no approved normal backend output was supplied",
            },
            "view_coverage_gain": self._coverage_gain(selected),
            "contact_sheet": contact_sheet,
            "source_timecodes_preserved": True,
            "selection_policy": selection["policy"],
        }
        relative = f"comparisons/video-intelligence-{run_id}.json"
        destination = self.project.root / relative
        atomic_write_json(destination, report)
        artifact = self.artifacts.ingest_file(
            destination, media_type="application/vnd.bvmcp.video-intelligence+json"
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO video_analysis_runs(id,source_reference_id,report_json,"
                "report_digest,created_at) VALUES(?,?,?,?,?)",
                (run_id, source_reference_id, json.dumps(report), artifact.digest, created_at),
            )
        return {
            **report,
            "report": artifact.to_dict(),
            "path": relative,
        }

    @staticmethod
    def _shots(
        frames: list[dict[str, Any]], selected_ids: set[str]
    ) -> list[dict[str, Any]]:
        groups: list[list[dict[str, Any]]] = []
        for frame in frames:
            if not groups or "CUT" in frame["categories"]:
                groups.append([])
            groups[-1].append(frame)
        shots = []
        for index, group in enumerate(groups, start=1):
            selected = [item for item in group if item["reference_id"] in selected_ids]
            quality = sum(float(item["quality_score"]) for item in selected) / max(
                1, len(selected)
            )
            motion_samples = [
                float(item["motion_magnitude"])
                for item in selected
                if item.get("motion_magnitude") is not None
            ]
            motion = sum(motion_samples) / max(1, len(motion_samples))
            blur_fraction = sum("blur" in item["warnings"] for item in selected) / max(
                1, len(selected)
            )
            score = len(selected) * quality * (1.0 - 0.5 * blur_fraction)
            if motion_samples:
                score *= min(1.5, 1.0 + motion)
            shots.append(
                {
                    "id": f"shot-{index:04d}",
                    "start_timecode_seconds": group[0]["timecode_seconds"],
                    "end_timecode_seconds": group[-1]["timecode_seconds"],
                    "frame_reference_ids": [item["reference_id"] for item in group],
                    "selected_reference_ids": [item["reference_id"] for item in selected],
                    "frame_count": len(group),
                    "selected_count": len(selected),
                    "mean_quality": round(quality, 8),
                    "mean_motion": round(motion, 8),
                    "blur_fraction": round(blur_fraction, 8),
                    "reconstruction_score": round(score, 8),
                }
            )
        return shots

    @staticmethod
    def _frame_record(frame: dict[str, Any], analysis: dict[str, Any]) -> dict[str, Any]:
        temporal = analysis["temporal"]
        flow = temporal["optical_flow_diversity"].get("mean_magnitude")
        categories = []
        if temporal["scene_cut"]:
            categories.append("CUT")
        if "blur" in analysis["warnings"]:
            categories.append("MOTION_BLUR")
        if temporal["near_duplicate"]:
            categories.append("STATIC_OBJECT")
        elif flow is not None and flow > 0.02:
            categories.append("MOVING_CAMERA")
        else:
            categories.append("STATIC_OBJECT")
        return {
            "reference_id": frame["id"],
            "extraction_index": frame["metadata"]["extraction_index"],
            "timecode_seconds": frame["metadata"]["timecode_seconds"],
            "categories": categories,
            "quality_score": analysis["score"],
            "warnings": analysis["warnings"],
            "motion_magnitude": flow,
            "geometric_diversity_score": min(1.0, float(flow or 0.0)),
            "viewpoint_estimate": frame.get("viewpoint_label") or "unresolved",
        }

    def _feature_tracks(
        self, source_reference_id: str, selected_ids: set[str]
    ) -> list[dict[str, Any]]:
        tracks = []
        for feature in FeatureStore(self.project).list():
            observed = sorted(selected_ids & set(feature.get("reference_ids", [])))
            if not observed:
                continue
            track_id = str(uuid.uuid4())
            track = {
                "id": track_id,
                "semantic_label": feature["type"],
                "feature_id": feature["id"],
                "reference_ids": observed,
                "confidence": float(feature.get("confidence", 0.0)),
                "source": "approved_project_feature_observations",
            }
            with self.project.connection() as connection:
                connection.execute(
                    "INSERT INTO feature_tracks(id,source_reference_id,semantic_label,"
                    "observations_json,confidence,created_at) VALUES(?,?,?,?,?,?)",
                    (
                        track_id,
                        source_reference_id,
                        feature["type"],
                        json.dumps(track),
                        track["confidence"],
                        utc_now(),
                    ),
                )
            tracks.append(track)
        return tracks

    @staticmethod
    def _coverage_gain(selected: list[dict[str, Any]]) -> dict[str, Any]:
        viewpoints = sorted(
            {
                item["viewpoint_estimate"]
                for item in selected
                if item["viewpoint_estimate"] != "unresolved"
            }
        )
        return {
            "distinct_labeled_viewpoints": viewpoints,
            "selected_keyframe_count": len(selected),
            "requires_camera_solve_for_surface_percentage": True,
        }
