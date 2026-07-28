"""Tests for the VisionMCP V2 cinematic compiler."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

from blender_vision.cinematic.emit import export_motion_table
from blender_vision.cinematic.path import (
    CameraPathCompositionError,
    SolidGeometry,
    compose_camera_path,
    compose_flagship_datacentre_path,
    path_geometry_intersections,
)
from blender_vision.cinematic.replay import replay_camera_state, replay_digest
from blender_vision.cinematic.spline import ArcLengthSpline, CatmullRomSpline, QuaternionCurve
from blender_vision.cinematic.textsafe import TextZone, evaluate_text_safe
from blender_vision.core.util import canonical_json
from blender_vision.v2.records import NarrativeBeat
from blender_vision.v2.validation import validate_record

REPO = Path(__file__).resolve().parents[1]


def test_arc_length_parameterisation_accuracy() -> None:
    points = [
        [0.0, 0.0, 0.0],
        [1.0, 0.0, 0.0],
        [2.0, 1.0, 0.0],
        [3.0, 1.0, 0.5],
        [5.0, 0.0, 0.5],
    ]
    arc = ArcLengthSpline(CatmullRomSpline(points), table_samples=8192)
    rel_err = arc.relative_arc_length_error(1000)
    assert rel_err < 1e-6, f"relative arc-length error {rel_err} exceeds 1e-6"
    # Equal scroll deltas map to nearly equal world distance.
    max_dev = arc.max_scroll_distance_deviation(1000)
    assert max_dev < 1e-3, f"scroll/distance deviation {max_dev} too large"


def test_quaternion_path_has_no_flips() -> None:
    # A continuous yaw turn; control quats span more than 90 degrees.
    angles = np.linspace(0.0, np.pi * 0.75, 8)
    quats = []
    for angle in angles:
        quats.append([float(np.cos(angle / 2)), 0.0, 0.0, float(np.sin(angle / 2))])
    curve = QuaternionCurve(quats)
    dots = curve.consecutive_dot_products(512)
    assert float(np.min(dots)) > 0.0


def test_beat_gap_detection_catches_gapped_path() -> None:
    with pytest.raises(CameraPathCompositionError, match="gaps"):
        compose_camera_path(
            path_id="gapped",
            control_points=[[0, 0, 0], [1, 0, 0], [2, 0, 0]],
            beats=[
                NarrativeBeat(beat_id="a", label="A", scroll_start=0.0, scroll_end=0.4),
                NarrativeBeat(beat_id="b", label="B", scroll_start=0.6, scroll_end=1.0),
            ],
        )


def test_beat_overlap_detection() -> None:
    with pytest.raises(CameraPathCompositionError, match="overlaps"):
        compose_camera_path(
            path_id="overlap",
            control_points=[[0, 0, 0], [1, 0, 0], [2, 0, 0]],
            beats=[
                NarrativeBeat(beat_id="a", label="A", scroll_start=0.0, scroll_end=0.6),
                NarrativeBeat(beat_id="b", label="B", scroll_start=0.5, scroll_end=1.0),
            ],
        )


def test_replay_determinism_same_process() -> None:
    graph = compose_flagship_datacentre_path()
    a = replay_camera_state(graph, 0.37).to_dict()
    b = replay_camera_state(graph, 0.37).to_dict()
    assert canonical_json(a) == canonical_json(b)
    assert replay_digest(graph, 0.37) == replay_digest(graph, 0.37)


def test_replay_determinism_across_processes(tmp_path: Path) -> None:
    graph = compose_flagship_datacentre_path()
    payload_path = tmp_path / "path.json"
    payload_path.write_text(json.dumps(graph.to_dict()), encoding="utf-8")
    script = f"""
import json, hashlib, sys
from blender_vision.core.util import canonical_json
from blender_vision.v2.records import CameraPathGraph
from blender_vision.cinematic.replay import replay_camera_state

graph = CameraPathGraph.from_dict(json.loads(open({str(payload_path)!r}).read()))
state = replay_camera_state(graph, 0.42).to_dict()
print(hashlib.sha256(canonical_json(state)).hexdigest())
print(canonical_json(state).decode())
"""
    env = {**os.environ, "PYTHONPATH": str(REPO / "src")}
    runs = []
    for _ in range(2):
        completed = subprocess.run(
            [sys.executable, "-c", script],
            capture_output=True,
            text=True,
            check=True,
            env=env,
            cwd=str(REPO),
        )
        lines = completed.stdout.strip().splitlines()
        runs.append((lines[0], "\n".join(lines[1:])))
    assert runs[0][0] == runs[1][0]
    assert runs[0][1] == runs[1][1]


def test_text_safe_rejects_low_contrast_and_accepts_good() -> None:
    # Dark noisy background vs near-black text → fail.
    bad = np.zeros((120, 160, 3), dtype=np.float64)
    bad[:] = 0.05
    rng = np.random.default_rng(0)
    bad += rng.normal(0.0, 0.08, size=bad.shape)
    bad = np.clip(bad, 0.0, 1.0)
    bad_result = evaluate_text_safe(
        bad,
        zone=TextZone.CENTRE,
        text_luminance=0.0,
        min_contrast=4.5,
        max_background_variance=0.01,
    )
    assert bad_result.readable is False

    # Dark uniform background vs white text → pass.
    good = np.zeros((120, 160, 3), dtype=np.float64)
    good[:] = 0.08
    good_result = evaluate_text_safe(
        good,
        zone=TextZone.CENTRE,
        text_luminance=1.0,
        min_contrast=4.5,
        max_background_variance=0.02,
    )
    assert good_result.readable is True
    assert good_result.contrast_ratio >= 4.5


def test_camera_vs_geometry_intersection_detection() -> None:
    solid = SolidGeometry("block", (0.4, -0.5, -0.5), (0.6, 0.5, 0.5))
    hits = path_geometry_intersections(
        [[0.0, 0.0, 0.0], [1.0, 0.0, 0.0]],
        [solid],
        samples=64,
        margin=0.0,
    )
    assert hits, "expected intersection with solid block"

    with pytest.raises(CameraPathCompositionError, match="intersects solid"):
        compose_camera_path(
            path_id="through-wall",
            control_points=[[0.0, 0.0, 0.0], [1.0, 0.0, 0.0]],
            beats=[NarrativeBeat(beat_id="b", label="B", scroll_start=0.0, scroll_end=1.0)],
            solids=[solid],
        )


def test_flagship_path_nine_beats_and_schema() -> None:
    graph = compose_flagship_datacentre_path()
    assert len(graph.beats) == 9
    assert graph.beat_coverage_gaps() == []
    labels = [beat.label for beat in graph.beats]
    assert labels == [
        "THRESHOLD",
        "CAPACITY",
        "INFERENCE",
        "DISPATCH",
        "EXECUTION",
        "TURN",
        "VERIFY",
        "RECEIPT",
        "ACCESS",
    ]
    validate_record(graph)
    table = export_motion_table(
        graph,
        Path("/tmp/bvmcp-motion-test.json"),
        sample_rate_hz=10.0,
        duration_s=2.0,
    )
    assert table["sample_count"] >= 2
    assert table["bytes"] > 0


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender camera bake",
)
def test_blender_camera_bake(tmp_path: Path) -> None:
    from blender_vision.cinematic.blender_probe import probe_blender
    from blender_vision.cinematic.emit import bake_blender_camera
    from blender_vision.core.errors import BackendUnavailable

    status = probe_blender()
    if not status["available"]:
        pytest.skip(f"BLOCKED_EXTERNAL: {status['reason']}")

    graph = compose_flagship_datacentre_path()
    blend = tmp_path / "path.blend"
    try:
        result = bake_blender_camera(graph, blend, sample_count=24, frame_end=24)
    except BackendUnavailable as error:
        pytest.skip(f"BLOCKED_EXTERNAL: {error}")
    assert blend.is_file()
    assert result["bytes"] > 0
    assert result["key_count"] == 24
