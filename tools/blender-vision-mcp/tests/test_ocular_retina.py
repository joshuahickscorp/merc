"""Unit and integration tests for the ocular stream bus and retina pipeline."""

from __future__ import annotations

import os
from pathlib import Path

import cv2
import numpy as np
import pytest

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.attestation import ExecutionClass, RuntimeAttestation
from blender_vision.ocular.calibration import calibrate_sensor
from blender_vision.ocular.gaze import GazeController
from blender_vision.ocular.records import (
    RETINAL_EVENT_TYPES,
    ColourSpace,
    FixationOutcome,
    OcularFrame,
    RetinalEvent,
)
from blender_vision.ocular.retina import (
    ProcessingLane,
    RetinaState,
    assert_reflex_cannot_relabel,
    build_pyramid,
    dense_optical_flow,
    process_frame,
    separate_camera_object_motion,
)
from blender_vision.ocular.sensors import SensorRegistry, SourceType
from blender_vision.ocular.stream import close_stream, open_stream, read_frame
from blender_vision.v2.authority import AuthorityClass

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def _solid(w: int, h: int, color: tuple[int, int, int] = (40, 40, 40)) -> np.ndarray:
    img = np.zeros((h, w, 3), dtype=np.uint8)
    img[:] = color
    return img


def _blob(
    base: np.ndarray,
    cx: int,
    cy: int,
    size: int = 24,
    color: tuple[int, int, int] = (220, 60, 40),
) -> np.ndarray:
    out = base.copy()
    half = size // 2
    y0, y1 = max(0, cy - half), min(base.shape[0], cy + half)
    x0, x1 = max(0, cx - half), min(base.shape[1], cx + half)
    out[y0:y1, x0:x1] = color
    return out


def _write_sequence(directory: Path, frames: list[np.ndarray]) -> Path:
    directory.mkdir(parents=True, exist_ok=True)
    for i, frame in enumerate(frames):
        path = directory / f"frame_{i:04d}.png"
        assert cv2.imwrite(str(path), frame)
    return directory


def _checkerboard(
    cols: int = 9,
    rows: int = 6,
    square: int = 40,
    noise: float = 0.0,
) -> np.ndarray:
    """Synthetic board with (cols, rows) inner corners → (cols+1) x (rows+1) squares."""
    w = (cols + 1) * square
    h = (rows + 1) * square
    img = np.zeros((h, w), dtype=np.uint8)
    for iy in range(rows + 1):
        for ix in range(cols + 1):
            if (ix + iy) % 2 == 0:
                img[iy * square : (iy + 1) * square, ix * square : (ix + 1) * square] = 240
            else:
                img[iy * square : (iy + 1) * square, ix * square : (ix + 1) * square] = 20
    if noise > 0:
        rng = np.random.default_rng(0)
        img = np.clip(img.astype(np.float32) + rng.normal(0, noise, img.shape), 0, 255)
        img = img.astype(np.uint8)
    return cv2.cvtColor(img, cv2.COLOR_GRAY2BGR)


# ---------------------------------------------------------------------------
# frame immutability
# ---------------------------------------------------------------------------


def test_ocular_frame_immutable_after_seal() -> None:
    frame = OcularFrame(
        id="f1",
        frame_id="f1",
        stream_id="s1",
        timestamp=0.0,
        image_digest="abc",
        resolution=[64, 64],
        colour_space=ColourSpace.BGR,
        authority=AuthorityClass.SENSOR_DERIVED,
    ).seal()
    assert frame.digest
    with pytest.raises(AttributeError, match="immutable"):
        frame.timestamp = 1.0
    with pytest.raises(AttributeError, match="immutable"):
        frame.image_digest = "mutated"


# ---------------------------------------------------------------------------
# pyramid
# ---------------------------------------------------------------------------


def test_pyramid_level_sizes() -> None:
    image = _solid(320, 240)
    pyramid = build_pyramid(image, levels=4)
    sizes = pyramid.level_sizes()
    assert len(sizes) >= 3
    assert sizes[0] == (320, 240)
    for prev, cur in zip(sizes, sizes[1:], strict=False):
        assert cur[0] <= prev[0]
        assert cur[1] <= prev[1]
        assert cur[0] >= prev[0] // 2 - 1
        assert cur[1] >= prev[1] // 2 - 1


# ---------------------------------------------------------------------------
# optical flow
# ---------------------------------------------------------------------------


def test_optical_flow_recovers_known_translation() -> None:
    # Textured field so Farneback has gradients to lock onto.
    rng = np.random.default_rng(0)
    base = rng.integers(40, 200, size=(128, 128), dtype=np.uint8)
    shift = 4
    moved = np.zeros_like(base)
    moved[:, shift:] = base[:, :-shift]
    flow = dense_optical_flow(base, moved)
    # Interior region away from the zero-filled border.
    region = flow[20:100, 20:100, 0]
    mean_dx = float(np.mean(region))
    assert mean_dx > 2.0, f"expected ~{shift}px translation, got {mean_dx}"


# ---------------------------------------------------------------------------
# camera vs object motion
# ---------------------------------------------------------------------------


def test_camera_motion_separation_on_synthetic_global_motion() -> None:
    base = np.zeros((160, 160), dtype=np.uint8)
    rng = np.random.default_rng(1)
    base[:] = rng.integers(30, 90, size=base.shape, dtype=np.uint8)
    cv2.rectangle(base, (40, 40), (80, 80), 200, -1)
    # Global translation of the whole frame (camera pan).
    dx, dy = 6, 0
    M = np.float32([[1, 0, dx], [0, 1, dy]])
    shifted = cv2.warpAffine(base, M, (base.shape[1], base.shape[0]), borderMode=cv2.BORDER_REFLECT)
    flow = dense_optical_flow(base, shifted)
    result = separate_camera_object_motion(flow)
    assert result.camera_motion_score > result.object_motion_score
    assert result.camera_motion_score > 2.0
    assert result.global_matrix is not None


def test_object_motion_has_residual() -> None:
    base = _solid(160, 160, (50, 50, 50))
    a = _blob(base, 40, 80, 28)
    b = _blob(base, 70, 80, 28)
    flow = dense_optical_flow(_to_gray(a), _to_gray(b))
    result = separate_camera_object_motion(flow)
    assert result.object_motion_score > 0.5


def _to_gray(image: np.ndarray) -> np.ndarray:
    if image.ndim == 3:
        return cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
    return image


# ---------------------------------------------------------------------------
# retinal events: each type fires on constructed case, not on confounder
# ---------------------------------------------------------------------------


def _event_types(images: list[np.ndarray], **kwargs) -> set[str]:
    state = RetinaState()
    types: set[str] = set()
    for img in images:
        analysis = process_frame(img, state=state, **kwargs)
        types.update(e.event_type for e in analysis.events)
    return types


def test_object_entered_and_left() -> None:
    bg = _solid(200, 160, (30, 30, 30))
    frames = [bg, bg, _blob(bg, 100, 80), _blob(bg, 100, 80), bg, bg, bg]
    types = _event_types(frames)
    assert "OBJECT_ENTERED" in types
    assert "OBJECT_LEFT" in types
    # Confounder: static empty scene must not fire enter/leave.
    quiet = _event_types([bg, bg, bg, bg])
    assert "OBJECT_ENTERED" not in quiet
    assert "OBJECT_LEFT" not in quiet


def test_object_moved_not_on_static() -> None:
    bg = _solid(200, 160, (30, 30, 30))
    frames = [
        _blob(bg, 40, 80),
        _blob(bg, 55, 80),
        _blob(bg, 70, 80),
        _blob(bg, 85, 80),
    ]
    types = _event_types(frames)
    assert "OBJECT_MOVED" in types
    static = [_blob(bg, 60, 80) for _ in range(5)]
    quiet = _event_types(static)
    assert "OBJECT_MOVED" not in quiet


def test_object_occluded_and_reappeared() -> None:
    bg = _solid(220, 160, (30, 30, 30))
    full = _blob(bg, 100, 80, 40)
    # Smaller residual region simulates partial occlusion.
    small = _blob(bg, 100, 80, 14)
    gone = bg.copy()
    frames = [full, full, small, small, gone, gone, full, full]
    types = _event_types(frames)
    assert "OBJECT_OCCLUDED" in types
    assert "OBJECT_REAPPEARED" in types or "OBJECT_ENTERED" in types
    # Confounder: steady full blob.
    quiet = _event_types([full, full, full, full])
    assert "OBJECT_OCCLUDED" not in quiet


def test_light_changed_not_on_stable_exposure() -> None:
    dark = _solid(160, 120, (20, 20, 20))
    bright = _solid(160, 120, (180, 180, 180))
    types = _event_types([dark, dark, bright, bright])
    assert "LIGHT_CHANGED" in types
    quiet = _event_types([dark, dark, dark, dark])
    assert "LIGHT_CHANGED" not in quiet


def test_surface_and_text_changed() -> None:
    base = _solid(180, 140, (60, 60, 60))
    # Surface: broad low-frequency tint (not a compact blob).
    surface = base.copy()
    surface[:, :] = np.clip(surface.astype(np.int16) + (18, 12, 8), 0, 255).astype(np.uint8)
    # Keep mean shift modest so LIGHT_CHANGED may or may not fire; residual should.
    types_surface = _event_types([base, base, surface, surface])
    assert "SURFACE_CHANGED" in types_surface or "LIGHT_CHANGED" in types_surface

    # Text: high-frequency glyph change.
    text_a = base.copy()
    cv2.putText(text_a, "HELLO", (20, 80), cv2.FONT_HERSHEY_SIMPLEX, 1.2, (240, 240, 240), 2)
    text_b = base.copy()
    cv2.putText(text_b, "WORLD", (20, 80), cv2.FONT_HERSHEY_SIMPLEX, 1.2, (240, 240, 240), 2)
    types_text = _event_types([text_a, text_a, text_b, text_b])
    assert "TEXT_CHANGED" in types_text
    quiet = _event_types([text_a, text_a, text_a, text_a])
    assert "TEXT_CHANGED" not in quiet


def test_camera_moved_not_object_moved_on_global_pan() -> None:
    rng = np.random.default_rng(2)
    base = rng.integers(40, 100, size=(160, 200, 3), dtype=np.uint8)
    cv2.rectangle(base, (60, 50), (110, 100), (200, 40, 40), -1)
    frames = [base]
    for dx in (3, 6, 9, 12):
        M = np.float32([[1, 0, dx], [0, 1, 0]])
        frames.append(
            cv2.warpAffine(base, M, (base.shape[1], base.shape[0]), borderMode=cv2.BORDER_REFLECT)
        )
    types = _event_types(frames)
    assert "CAMERA_MOVED" in types
    # Pure global pan should not be reported as object motion.
    # (Allow transient residual noise but require camera event present.)
    state = RetinaState()
    cam_events = 0
    obj_events = 0
    for img in frames:
        analysis = process_frame(img, state=state)
        for e in analysis.events:
            if e.event_type == "CAMERA_MOVED":
                cam_events += 1
            if e.event_type == "OBJECT_MOVED":
                obj_events += 1
    assert cam_events >= 1
    assert obj_events == 0


def test_new_unknown_region_and_expected_missing() -> None:
    bg = _solid(160, 120, (25, 25, 25))
    frames = [bg, _blob(bg, 80, 60)]
    types = _event_types(frames)
    assert "NEW_UNKNOWN_REGION" in types

    state = RetinaState()
    expected = [
        {
            "id": "e1",
            "event_type": "OBJECT_ENTERED",
            "frame_index": 1,
            "region": [0, 0, 1, 1],
        }
    ]
    # Frame 0 only — expected enter on frame 1 will miss when we process only bg.
    analysis0 = process_frame(bg, state=state, expected_events=expected)
    analysis1 = process_frame(bg, state=state)  # no blob → enter missing
    types_m = {e.event_type for e in analysis0.events + analysis1.events}
    assert "EXPECTED_EVENT_MISSING" in types_m
    # Confounder: expected event that does occur.
    state2 = RetinaState()
    expected2 = [
        {
            "id": "e2",
            "event_type": "OBJECT_ENTERED",
            "frame_index": 1,
            "region": [0, 0, 1, 1],
        }
    ]
    process_frame(bg, state=state2, expected_events=expected2)
    analysis_hit = process_frame(_blob(bg, 80, 60), state=state2)
    assert "EXPECTED_EVENT_MISSING" not in {e.event_type for e in analysis_hit.events}
    assert "OBJECT_ENTERED" in {e.event_type for e in analysis_hit.events}


def test_all_eleven_event_types_defined() -> None:
    assert len(RETINAL_EVENT_TYPES) == 11


# ---------------------------------------------------------------------------
# inhibition of return
# ---------------------------------------------------------------------------


def test_inhibition_of_return_suppresses_redundant_fixation() -> None:
    gaze = GazeController(stream_id="g1", ior_radius_px=30.0)
    region = [40.0, 40.0, 32.0, 32.0]
    first = gaze.fixate(region, uncertainty=0.1, expected_information=0.8)
    assert first.outcome is FixationOutcome.OBSERVED
    second = gaze.fixate(region, uncertainty=0.1, expected_information=0.8)
    assert second.outcome is FixationOutcome.SUPPRESSED_IOR
    # High uncertainty overrides IOR.
    third = gaze.fixate(region, uncertainty=0.9, expected_information=0.8)
    assert third.outcome is FixationOutcome.OBSERVED
    # Critic request overrides IOR.
    gaze.fixate([200.0, 200.0, 20.0, 20.0], uncertainty=0.0)
    fourth = gaze.fixate([200.0, 200.0, 20.0, 20.0], uncertainty=0.0, critic_requested=True)
    assert fourth.outcome is FixationOutcome.OBSERVED


# ---------------------------------------------------------------------------
# reflex lane never alters correctness label
# ---------------------------------------------------------------------------


def test_reflex_lane_never_alters_correctness_label() -> None:
    event = RetinalEvent(
        id="e1",
        event_type="OBJECT_ENTERED",
        correctness_label="observed",
        written_by_lane=ProcessingLane.ATTENTIVE.value,
        authority=AuthorityClass.SENSOR_DERIVED,
    ).seal()
    with pytest.raises(ValidationError, match="reflex lane may not change"):
        assert_reflex_cannot_relabel(event, "false_positive")
    # Same label is allowed.
    assert assert_reflex_cannot_relabel(event, "observed") is event

    # Full pipeline: reflex resolution is attached, label stays attentive.
    bg = _solid(200, 160)
    state = RetinaState()
    process_frame(bg, state=state)
    analysis = process_frame(_blob(bg, 90, 70), state=state)
    for event in analysis.events:
        assert event.written_by_lane == ProcessingLane.ATTENTIVE.value
        assert event.correctness_label == "observed"
        assert event.reflex_resolution is not None
        assert event.reflex_resolution[0] <= 200


# ---------------------------------------------------------------------------
# calibration authority
# ---------------------------------------------------------------------------


def test_calibration_refuses_measured_without_physical_scale(tmp_path: Path) -> None:
    board = _checkerboard()
    paths = []
    for i in range(5):
        # Slight synthetic viewpoint jitter via warp.
        M = np.float32([[1, 0, i * 0.5], [0, 1, i * 0.3]])
        warped = cv2.warpAffine(board, M, (board.shape[1], board.shape[0]))
        path = tmp_path / f"board_{i}.png"
        cv2.imwrite(str(path), warped)
        paths.append(path)

    no_scale = calibrate_sensor(paths, sensor_id="cam-a", board_size=(9, 6), square_m=None)
    assert no_scale.authority is not AuthorityClass.MEASURED
    assert no_scale.authority in {
        AuthorityClass.SENSOR_DERIVED,
        AuthorityClass.INFERRED,
    }
    assert any("physical scale" in item for item in no_scale.limitations)

    with_scale = calibrate_sensor(
        paths, sensor_id="cam-b", board_size=(9, 6), square_m=0.025
    )
    # MEASURED only when board was detected; otherwise SENSOR_DERIVED/INFERRED.
    if with_scale.samples_used >= 1:
        assert with_scale.authority is AuthorityClass.MEASURED
    assert with_scale.frame.scale_authority in {
        AuthorityClass.MEASURED,
        AuthorityClass.UNRESOLVED,
    }


# ---------------------------------------------------------------------------
# stream: image sequence, video, blocked webcam
# ---------------------------------------------------------------------------


def test_image_sequence_stream(tmp_path: Path) -> None:
    frames = [_solid(64, 48, (i * 20, 30, 40)) for i in range(5)]
    seq = _write_sequence(tmp_path / "seq", frames)
    registry = SensorRegistry()
    handle = open_stream(
        seq,
        source_type=SourceType.IMAGE_SEQUENCE,
        stream_id="seq-1",
        registry=registry,
        buffer_size=4,
    )
    assert not isinstance(handle, RuntimeAttestation)
    assert handle.execution_class is ExecutionClass.PHYSICAL
    collected = []
    while True:
        item = read_frame(handle)
        if item is None:
            break
        frame, image = item
        collected.append(frame)
        assert frame.digest
        assert image is not None
    assert len(collected) == 5
    # Timestamps must be strictly monotonic.
    ts = [f.timestamp for f in collected]
    assert all(ts[i] < ts[i + 1] for i in range(len(ts) - 1))
    state = close_stream(handle)
    assert state["stats"]["frames_emitted"] == 5


def test_video_file_stream(tmp_path: Path) -> None:
    path = tmp_path / "clip.mp4"
    writer = cv2.VideoWriter(
        str(path),
        cv2.VideoWriter_fourcc(*"mp4v"),
        10.0,
        (80, 60),
    )
    assert writer.isOpened()
    for i in range(8):
        writer.write(_solid(80, 60, (i * 10, 50, 50)))
    writer.release()

    handle = open_stream(path, source_type=SourceType.VIDEO_FILE, stream_id="vid-1")
    assert not isinstance(handle, RuntimeAttestation)
    count = 0
    while read_frame(handle) is not None:
        count += 1
    close_stream(handle)
    assert count >= 4


def test_blocked_webcam_returns_blocked_not_frame() -> None:
    # Opt-in required.
    blocked = open_stream(0, source_type=SourceType.WEBCAM, allow_webcam=False)
    assert isinstance(blocked, RuntimeAttestation)
    assert blocked.execution_class is ExecutionClass.BLOCKED
    assert "allow_webcam" in blocked.blocked_reason

    # Opt-in with an absurd device index: still BLOCKED, never a fake frame.
    blocked2 = open_stream(
        0,
        source_type=SourceType.WEBCAM,
        allow_webcam=True,
        webcam_index=99_999,
        stream_id="webcam-missing",
    )
    assert isinstance(blocked2, RuntimeAttestation)
    assert blocked2.execution_class is ExecutionClass.BLOCKED
    assert blocked2.blocked_reason


def test_blender_render_source_type(tmp_path: Path) -> None:
    frames = [_blob(_solid(96, 72), 40 + i * 5, 36) for i in range(6)]
    seq = _write_sequence(tmp_path / "blend_seq", frames)
    handle = open_stream(seq, source_type=SourceType.BLENDER_RENDER, stream_id="bl-1")
    assert not isinstance(handle, RuntimeAttestation)
    n = 0
    state = RetinaState()
    events: list[str] = []
    while True:
        item = read_frame(handle)
        if item is None:
            break
        frame, image = item
        analysis = process_frame(image, frame=frame, state=state)
        events.extend(e.event_type for e in analysis.events)
        n += 1
    close_stream(handle)
    assert n == 6
    assert "OBJECT_ENTERED" in events or "OBJECT_MOVED" in events


# ---------------------------------------------------------------------------
# Blender-gated physical render (optional)
# ---------------------------------------------------------------------------


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender retina fixture",
)
def test_blender_retina_fixture_renders(tmp_path: Path) -> None:
    """Attempt a real Blender render. PHYSICAL when it works; honest fail otherwise.

    On hosts where Blender SIGSEGVs in MTLBackend::metal_is_supported during
    WM_init, the process never reaches Python. That is DIAGNOSTIC_ONLY / BLOCKED
    with a classified failure — never a fabricated PHYSICAL pass.
    """
    from blender_vision.ocular.attestation import FailureKind, run_attested

    repo = Path(__file__).resolve().parents[1]
    script = repo / "benchmarks" / "ocular_retina" / "generate_fixture.py"
    blender = os.environ.get(
        "BVMCP_BLENDER",
        "/Applications/Blender.app/Contents/MacOS/Blender",
    )
    out = tmp_path / "blender_seq"
    out.mkdir()
    attestation = run_attested(
        "blender",
        [
            blender,
            "--background",
            "--factory-startup",
            "--python-exit-code",
            "1",
            "--python",
            str(script),
            "--",
            str(out),
            "object_motion",
        ],
        cwd=repo,
        timeout_seconds=300,
        version_argv=["--version"],
        expect_marker="OCULAR_FIXTURE_OK",
        outputs={"manifest": out / "manifest.json"},
    )
    if attestation.execution_class is ExecutionClass.PHYSICAL:
        assert (out / "manifest.json").is_file()
        frames_dir = out / "frames"
        assert frames_dir.is_dir()
        assert any(frames_dir.glob("*.png"))
        return

    # Honest non-physical: no frames may be claimed PHYSICAL.
    assert attestation.execution_class in {
        ExecutionClass.BLOCKED,
        ExecutionClass.DIAGNOSTIC_ONLY,
    }
    assert attestation.blocked_reason or attestation.failure_kind is not None
    # A pure path bug must not be labelled hardware; SIGSEGV in metal_is_supported
    # during WM_init is a known host fault and may be HARDWARE_ERROR or UNCLASSIFIED.
    if attestation.failure_kind is FailureKind.PATH_ERROR:
        pytest.fail(f"Blender path error misrouted: {attestation.blocked_reason}")
