from __future__ import annotations

from copy import deepcopy
from pathlib import Path

from blender_vision.benchmarks.devices import (
    evaluate_device_dimensions,
    import_device_feature_candidates,
    load_device_manifest,
)
from blender_vision.projects.store import ProjectStore

REPOSITORY_ROOT = Path(__file__).resolve().parents[1]


def _mesh(
    name: str,
    minimum: tuple[float, float, float],
    maximum: tuple[float, float, float],
) -> dict[str, object]:
    return {
        "name": name,
        "type": "MESH",
        "world_bounds": {
            "minimum": list(minimum),
            "maximum": list(maximum),
            "dimensions": [maximum[index] - minimum[index] for index in range(3)],
        },
    }


def _inventory(*objects: dict[str, object]) -> dict[str, object]:
    return {
        "canonical_transform": {"scale_to_millimetres": 1000.0},
        "objects": list(objects),
    }


def test_device_manifests_define_distinct_governed_targets() -> None:
    dgx = load_device_manifest("dgx_spark", REPOSITORY_ROOT)
    rtx = load_device_manifest("rtx_5090_fe", REPOSITORY_ROOT)

    assert dgx["manufacturer_spec"]["dimensions_mm"] == {
        "x": 150.0,
        "y": 150.0,
        "z": 50.5,
    }
    assert rtx["manufacturer_spec"]["dimensions_mm"] == {
        "x": 137.0,
        "y": 40.0,
        "z": 304.0,
    }
    assert len(dgx["feature_candidates"]) >= 8
    assert len(rtx["feature_candidates"]) >= 12
    assert all("camera_hint" in item for item in dgx["references"])
    assert all("camera_hint" in item for item in rtx["references"])
    assert dgx["known_blockers"]
    assert rtx["known_blockers"]


def test_dgx_body_anchor_passes_exact_manufacturer_dimensions() -> None:
    manifest = load_device_manifest("dgx_spark", REPOSITORY_ROOT)
    result = evaluate_device_dimensions(
        manifest,
        _inventory(
            _mesh("dgx-spark", (-0.075, -0.075, -0.02525), (0.075, 0.075, 0.02525)),
            _mesh("dgx-spark-foam", (-0.08, -0.08, -0.03), (0.08, 0.08, 0.03)),
        ),
    )

    assert result["passed"] is True
    assert result["body_bounds"]["objects"] == ["dgx-spark"]
    assert all(check["passed"] for check in result["checks"].values())


def test_rtx_cooler_envelope_excludes_bracket_and_detects_depth_failure() -> None:
    manifest = load_device_manifest("rtx_5090_fe", REPOSITORY_ROOT)
    inventory = _inventory(
        _mesh("fe-shroud", (-0.0685, -0.0075, -0.152), (0.0685, 0.0075, 0.152)),
        _mesh("fan-front", (-0.06, -0.02, -0.07), (0.06, 0.02, 0.07)),
        _mesh("fe-brk-io", (-0.09, -0.03, -0.16), (0.09, 0.03, 0.16)),
    )

    passing = evaluate_device_dimensions(manifest, inventory)
    assert passing["passed"] is True
    assert "fe-brk-io" not in passing["body_bounds"]["objects"]

    failing_inventory = deepcopy(inventory)
    failing_inventory["objects"][1]["world_bounds"]["maximum"][1] = 0.021
    failing = evaluate_device_dimensions(manifest, failing_inventory)
    assert failing["passed"] is False
    assert failing["checks"]["y"]["actual_mm"] == 41.0


def test_feature_candidates_remain_grouped_and_unapproved(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "DGX feature import")
    manifest = load_device_manifest("dgx_spark", REPOSITORY_ROOT)
    manifest = {**manifest, "feature_candidates": [manifest["feature_candidates"][0]]}
    result = import_device_feature_candidates(
        project,
        manifest,
        _inventory(
            _mesh("dgx-spark", (-0.075, -0.075, -0.02525), (0.075, 0.075, 0.02525))
        ),
        scene_id="scene-1",
        scene_digest="sha256:scene",
        reference_by_label={},
    )

    assert result["feature_count"] == 1
    assert result["human_approval"] is False
    feature = result["features"][0]
    assert feature["id"] == "dgx_spark-body-shell"
    assert feature["human_approval"] is False
    assert feature["observations"][0]["objects"] == ["dgx-spark"]
