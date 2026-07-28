from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.cameras.landmark_matching import RenderLandmarkMatcher
from blender_vision.core.util import sha256_file

cv2 = pytest.importorskip("cv2")
numpy = pytest.importorskip("numpy")


def _fixture_manifest(tmp_path: Path) -> tuple[Path, Path, list[dict[str, object]], object]:
    render = numpy.zeros((320, 420), dtype=numpy.uint8)
    generator = numpy.random.default_rng(42)
    for index in range(90):
        x = int(generator.integers(15, 405))
        y = int(generator.integers(15, 305))
        radius = int(generator.integers(2, 8))
        shade = int(generator.integers(80, 256))
        cv2.circle(render, (x, y), radius, shade, -1)
        cv2.putText(
            render,
            str(index % 10),
            (max(0, x - 5), min(319, y + 5)),
            cv2.FONT_HERSHEY_SIMPLEX,
            0.25,
            255 - shade // 2,
            1,
            cv2.LINE_AA,
        )
    cv2.rectangle(render, (10, 10), (409, 309), 255, 3)
    render_path = tmp_path / "view.png"
    assert cv2.imwrite(str(render_path), render)
    source_model = tmp_path / "model.glb"
    source_model.write_bytes(b"hash-bound synthetic model fixture")
    anchor_source = tmp_path / "anchors.json"
    anchor_source.write_text('{"fixture": true}', encoding="utf-8")
    anchor_pixels = [
        (45.0, 45.0),
        (210.0, 42.0),
        (375.0, 48.0),
        (48.0, 160.0),
        (370.0, 165.0),
        (45.0, 275.0),
        (210.0, 278.0),
        (375.0, 272.0),
    ]
    anchors = [
        {
            "landmark_id": f"anchor-{index}",
            "world_model_units": [float(index % 2), float((index // 2) % 2), float(index // 4)],
            "render_px": list(pixel),
        }
        for index, pixel in enumerate(anchor_pixels)
    ]
    source_corners = numpy.float32([[0, 0], [419, 0], [419, 319], [0, 319]])
    target_corners = numpy.float32([[75, 55], [565, 35], [595, 430], [45, 455]])
    homography = cv2.getPerspectiveTransform(source_corners, target_corners)
    target = cv2.warpPerspective(render, homography, (640, 480))
    target_path = tmp_path / "target.png"
    assert cv2.imwrite(str(target_path), target)
    manifest = {
        "schema_version": 2,
        "source_model": str(source_model),
        "source_model_sha256": sha256_file(source_model)[0],
        "source_anchor_manifest": str(anchor_source),
        "source_anchor_manifest_sha256": sha256_file(anchor_source)[0],
        "authority": "SYNTHETIC_VIEW_FOR_LANDMARK_PROPOSAL_ONLY",
        "views": [
            {
                "id": "fixture-view",
                "image": render_path.name,
                "image_sha256": sha256_file(render_path)[0],
                "anchors": anchors,
            }
        ],
    }
    manifest_path = tmp_path / "render-manifest.json"
    manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
    return manifest_path, target_path, anchors, homography


def test_render_landmark_matcher_recovers_proposals_and_refuses_weak_evidence(
    tmp_path: Path,
) -> None:
    manifest_path, target_path, anchors, expected_homography = _fixture_manifest(tmp_path)
    matcher = RenderLandmarkMatcher()
    identity = [
        [100.0, 0.0, 0.0, 0.0],
        [0.0, 100.0, 0.0, 0.0],
        [0.0, 0.0, 100.0, 0.0],
        [0.0, 0.0, 0.0, 1.0],
    ]

    result = matcher.match(
        render_manifest_path=manifest_path,
        target_image_path=target_path,
        model_to_world_mm=identity,
    )

    assert result["status"] == "MATCH_PROPOSAL"
    assert result["authority"] == "MACHINE_PROPOSAL_NOT_REVIEWED"
    assert result["camera_acceptance_performed"] is False
    assert result["selected_view_id"] == "fixture-view"
    assert set(result["selected_matching_scale"]) == {
        "render_scale",
        "target_scale",
        "render_work_size",
        "target_work_size",
    }
    assert result["diagnostics"][0]["attempts"]
    assert len(result["correspondences"]) == len(anchors)
    expected_pixels = cv2.perspectiveTransform(
        numpy.float32([item["render_px"] for item in anchors]).reshape(-1, 1, 2),
        expected_homography,
    ).reshape(-1, 2)
    for correspondence, expected in zip(result["correspondences"], expected_pixels, strict=True):
        assert numpy.linalg.norm(numpy.array(correspondence["image_px"]) - expected) < 2.5
        assert 0.0 <= correspondence["confidence"] <= 0.95

    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    bottom_view = {**manifest["views"][0], "id": "bottom_fixture"}
    manifest["views"].append(bottom_view)
    manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
    constrained = matcher.match(
        render_manifest_path=manifest_path,
        target_image_path=target_path,
        model_to_world_mm=identity,
        config={"maximum_projected_anchors": 6},
        viewpoint_hint="inverted underbody",
    )
    assert constrained["selected_view_id"] == "bottom_fixture"
    assert constrained["viewpoint_policy"]["rule"] == "underbody"
    assert len(constrained["correspondences"]) == 6

    blank_path = tmp_path / "blank.png"
    Image.new("L", (640, 480), 127).save(blank_path)
    refused = matcher.match(
        render_manifest_path=manifest_path,
        target_image_path=blank_path,
        model_to_world_mm=identity,
    )
    assert refused["status"] == "REFUSED"
    assert refused["authority"] == "NO_LANDMARK_AUTHORITY"
    assert refused["correspondences"] == []

    Image.new("L", (420, 320), 0).save(tmp_path / "view.png")
    with pytest.raises(ValueError, match="digest verification"):
        matcher.match(
            render_manifest_path=manifest_path,
            target_image_path=target_path,
            model_to_world_mm=identity,
        )

    with pytest.raises(ValueError, match="pyramid_target_dimension"):
        matcher.match(
            render_manifest_path=manifest_path,
            target_image_path=target_path,
            model_to_world_mm=identity,
            config={"pyramid_target_dimension": 1801},
        )
