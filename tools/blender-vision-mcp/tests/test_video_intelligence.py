from __future__ import annotations

import json
from pathlib import Path

from PIL import Image, ImageDraw

from blender_vision.capture.intelligence import VideoIntelligenceService
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.features.store import FeatureStore
from blender_vision.projects.store import ProjectStore


def test_video_keyframes_preserve_timecodes_and_feature_tracks(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Video intelligence")
    ingestor = ReferenceIngestor(project)
    video_path = tmp_path / "walkaround.mp4"
    video_path.write_bytes(b"fixture")
    video = ingestor.import_file(video_path, rights_state="SYNTHETIC_OWNED")
    frame_ids = []
    for index in range(3):
        path = tmp_path / f"frame-{index}.png"
        image = Image.new("RGB", (96, 72), (32 + index * 50, 40, 60))
        ImageDraw.Draw(image).rectangle(
            (10 + index * 8, 12, 55 + index * 8, 60), fill=(220, 220, 220)
        )
        image.save(path)
        frame = ingestor.import_file(
            path, rights_state="SYNTHETIC_OWNED", viewpoint_label=f"video angle {index}"
        )
        metadata = {
            **frame["metadata"],
            "video_source_reference_id": video["id"],
            "extraction_index": index + 1,
            "timecode_seconds": index * 0.5,
        }
        with project.connection() as connection:
            connection.execute(
                "UPDATE reference_items SET metadata_json=? WHERE id=?",
                (json.dumps(metadata), frame["id"]),
            )
        frame_ids.append(frame["id"])
    FeatureStore(project).add(
        "LED",
        reference_ids=frame_ids[:2],
        confidence=0.8,
        evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
    )

    report = VideoIntelligenceService(project).analyze(video["id"], maximum_selected=3)

    assert report["frame_count"] == 3
    assert [item["timecode_seconds"] for item in report["all_frames"]] == [0.0, 0.5, 1.0]
    assert report["source_timecodes_preserved"] is True
    assert report["shots"]
    assert report["best_reconstruction_shot_id"].startswith("shot-")
    assert all(item["categories"] for item in report["all_frames"])
    assert report["feature_tracks"][0]["semantic_label"] == "LED"
    assert report["unavailable_products"]["depth_sequence"]
    assert project.status()["counts"]["video_analysis_runs"] == 1
    assert project.status()["counts"]["feature_tracks"] == 1
    eligible = {
        item["id"]
        for item in ingestor.list()
        if item["media_type"].startswith("image/") and item["acceptance_eligible"]
    }
    assert eligible == set(report["acceptance_reference_ids"])
