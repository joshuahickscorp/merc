"""Ocular stream bus: open, buffer, and close calibrated frame sources.

Sources: video file, image sequence, screen capture, Blender render directory,
and webcam. Webcam is opt-in only. A missing camera returns BLOCKED — never a
fabricated frame.
"""

from __future__ import annotations

import hashlib
import threading
import time
from collections import deque
from collections.abc import Iterator
from contextlib import suppress
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

import cv2
import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import utc_now
from blender_vision.ocular.attestation import (
    ExecutionClass,
    RuntimeAttestation,
    attest_blocked,
)
from blender_vision.ocular.records import ColourSpace, OcularFrame, default_lineage
from blender_vision.ocular.sensors import (
    DEFAULT_REGISTRY,
    PrivacyState,
    RightsState,
    SensorDescriptor,
    SensorRegistry,
    SourceType,
    TimestampDomain,
    register_sensor,
)
from blender_vision.v2.authority import AuthorityClass, CoordinateFrame

IMAGE_EXTENSIONS = frozenset({".png", ".jpg", ".jpeg", ".bmp", ".tif", ".tiff", ".webp"})


class StreamState(StrEnum):
    CLOSED = "closed"
    OPEN = "open"
    EXHAUSTED = "exhausted"
    BLOCKED = "blocked"
    ERROR = "error"


@dataclass(slots=True)
class StreamStats:
    """Stream counters.

    Invariant once the buffer is drained:
    ``frames_emitted + frames_dropped == frames_offered``.

    While frames still sit in the ring,
    ``frames_emitted + frames_dropped + buffer_occupancy == frames_offered``.
    """

    frames_emitted: int = 0
    frames_dropped: int = 0
    frames_offered: int = 0
    buffer_capacity: int = 0
    buffer_occupancy: int = 0
    last_timestamp: float = 0.0
    opened_at: str = ""
    closed_at: str = ""


@dataclass(slots=True)
class StreamHandle:
    """Live stream handle. Frames sit in a bounded ring; overflow drops oldest.

    Drops count only when an *undelivered* buffered frame is overwritten.
    ``read_frame`` ingests then immediately pops, so a keep-up consumer never
    drops. A genuine overrun requires the producer to push faster than the
    consumer pops (multiple ``push_frame`` / source ingests without ``pop_frame``).
    """

    stream_id: str
    sensor: SensorDescriptor
    source_type: SourceType
    state: StreamState = StreamState.CLOSED
    stats: StreamStats = field(default_factory=StreamStats)
    execution_class: ExecutionClass = ExecutionClass.BLOCKED
    attestation: RuntimeAttestation | None = None
    calibration_receipt: str = ""
    coordinate_frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name="opencv-camera", up_axis="-Y", forward_axis="+Z"
        )
    )
    _buffer: deque[OcularFrame] = field(default_factory=lambda: deque(maxlen=32))
    _raw_buffer: deque[np.ndarray] = field(default_factory=lambda: deque(maxlen=32))
    _lock: threading.RLock = field(default_factory=threading.RLock)
    _capture: Any = None
    _paths: list[Path] = field(default_factory=list)
    _path_index: int = 0
    _monotonic_origin: float = 0.0
    _last_mono: float = 0.0
    _sequence_index: int = 0
    _intrinsics: dict[str, Any] = field(default_factory=dict)
    _closed: bool = True
    # Frames lost to overflow since the last delivered frame; stamped onto the
    # next pop as OcularFrame.dropped_before (per-frame gap, not cumulative).
    _pending_gap: int = 0

    @property
    def buffer_capacity(self) -> int:
        return int(self._buffer.maxlen or 0)

    def push_frame(self, frame: OcularFrame, image: np.ndarray | None = None) -> None:
        """Enqueue an undelivered frame. Overflow drops the oldest undelivered sample."""
        with self._lock:
            self.stats.frames_offered += 1
            if self._buffer.maxlen is not None and len(self._buffer) == self._buffer.maxlen:
                # Genuine loss: an undelivered frame is about to be overwritten.
                self.stats.frames_dropped += 1
                self._pending_gap += 1
            self._buffer.append(frame)
            # Keep the raw ring the same capacity/eviction policy as the frame ring.
            if image is not None:
                self._raw_buffer.append(image)
            else:
                # Placeholder so pop indices stay aligned if a caller omits pixels.
                self._raw_buffer.append(None)  # type: ignore[arg-type]
            self.stats.buffer_occupancy = len(self._buffer)
            self.stats.last_timestamp = frame.timestamp

    def pop_frame(self) -> tuple[OcularFrame, np.ndarray | None] | None:
        """Deliver the oldest buffered frame, stamping per-frame ``dropped_before``."""
        with self._lock:
            if not self._buffer:
                return None
            frame = self._buffer.popleft()
            image = self._raw_buffer.popleft() if self._raw_buffer else None
            gap = self._pending_gap
            self._pending_gap = 0
            frame = _stamp_dropped_before(frame, gap)
            self.stats.frames_emitted += 1
            self.stats.buffer_occupancy = len(self._buffer)
            self.stats.last_timestamp = frame.timestamp
            return frame, image

    def snapshot_state(self) -> dict[str, Any]:
        with self._lock:
            return {
                "stream_id": self.stream_id,
                "state": self.state.value,
                "source_type": self.source_type.value,
                "sensor_id": self.sensor.sensor_id,
                "execution_class": self.execution_class.value,
                "calibration_receipt": self.calibration_receipt,
                "coordinate_frame": self.coordinate_frame.to_dict(),
                "stats": {
                    "frames_emitted": self.stats.frames_emitted,
                    "frames_dropped": self.stats.frames_dropped,
                    "frames_offered": self.stats.frames_offered,
                    "buffer_capacity": self.buffer_capacity,
                    "buffer_occupancy": self.stats.buffer_occupancy,
                    "last_timestamp": self.stats.last_timestamp,
                    "opened_at": self.stats.opened_at,
                    "closed_at": self.stats.closed_at,
                },
                "attestation_id": self.attestation.id if self.attestation else "",
            }


_STREAMS: dict[str, StreamHandle] = {}
_STREAMS_LOCK = threading.RLock()


def _digest_image(image: np.ndarray) -> str:
    contiguous = np.ascontiguousarray(image)
    return hashlib.sha256(contiguous.tobytes()).hexdigest()


def _next_monotonic(handle: StreamHandle, preferred: float | None = None) -> float:
    """Guarantee strictly increasing timestamps even when the source stalls."""
    if preferred is not None and preferred > handle._last_mono:
        handle._last_mono = preferred
        return preferred
    candidate = time.monotonic() - handle._monotonic_origin
    if candidate <= handle._last_mono:
        candidate = handle._last_mono + 1e-6
    handle._last_mono = candidate
    return candidate


def _stamp_dropped_before(frame: OcularFrame, gap: int) -> OcularFrame:
    """Set per-frame gap and seal. Safe on unsealed or previously sealed frames."""
    gap = int(gap)
    if gap < 0:
        gap = 0
    if getattr(frame, "_locked", False):
        if int(frame.dropped_before) == gap and frame.digest:
            return frame
        object.__setattr__(frame, "_locked", False)
    object.__setattr__(frame, "dropped_before", gap)
    # Clear prior digest so seal recomputes over the updated payload.
    object.__setattr__(frame, "digest", "")
    return frame.seal()


def _make_frame(
    handle: StreamHandle,
    image: np.ndarray,
    *,
    timestamp: float | None = None,
    pose: dict[str, Any] | None = None,
) -> OcularFrame:
    """Build an unsealed frame. ``dropped_before`` is stamped at delivery (pop)."""
    h, w = image.shape[:2]
    ts = _next_monotonic(handle, timestamp)
    index = handle._sequence_index
    handle._sequence_index += 1
    return OcularFrame(
        id=f"{handle.stream_id}-f{index:06d}",
        frame_id=f"{handle.stream_id}-f{index:06d}",
        stream_id=handle.stream_id,
        timestamp=ts,
        sensor_state=handle.sensor.to_dict(),
        image_digest=_digest_image(image),
        resolution=[int(w), int(h)],
        colour_space=ColourSpace.BGR if image.ndim == 3 else ColourSpace.GRAY,
        exposure=1.0,
        camera_intrinsics=dict(handle._intrinsics),
        camera_pose_if_known=pose,
        depth_digest="",
        motion_digest="",
        privacy_mask_digest="",
        calibration_receipt=handle.calibration_receipt,
        coordinate_frame=handle.coordinate_frame,
        sequence_index=index,
        dropped_before=0,
        authority=AuthorityClass.SENSOR_DERIVED,
        lineage=default_lineage(
            "ocular.stream.emit",
            inputs=[handle.sensor.sensor_id, handle.stream_id],
        ),
    )


def _list_sequence(path: Path) -> list[Path]:
    if path.is_file():
        return [path]
    if not path.is_dir():
        raise ValidationError(f"image sequence path does not exist: {path}")
    files = sorted(
        p for p in path.iterdir() if p.is_file() and p.suffix.lower() in IMAGE_EXTENSIONS
    )
    if not files:
        raise ValidationError(f"no images found in sequence directory: {path}")
    return files


def _open_video(path: Path) -> cv2.VideoCapture:
    capture = cv2.VideoCapture(str(path))
    if not capture.isOpened():
        capture.release()
        raise ValidationError(f"could not open video file: {path}")
    return capture


def _screen_grab() -> np.ndarray | None:
    """Grab the primary display. Pillow ImageGrab on macOS; fail closed otherwise."""
    try:
        from PIL import ImageGrab
    except ImportError:
        return None
    try:
        shot = ImageGrab.grab()
    except Exception:
        return None
    array = np.asarray(shot)
    if array.ndim == 3 and array.shape[2] >= 3:
        # ImageGrab is RGB; stream frames are BGR to match OpenCV.
        return cv2.cvtColor(array[:, :, :3], cv2.COLOR_RGB2BGR)
    return array


def open_stream(
    source: str | Path,
    *,
    source_type: SourceType | str,
    stream_id: str | None = None,
    sensor_id: str | None = None,
    buffer_size: int = 32,
    allow_webcam: bool = False,
    webcam_index: int = 0,
    frame_rate: float = 30.0,
    calibration_receipt: str = "",
    coordinate_frame: CoordinateFrame | None = None,
    intrinsics: dict[str, Any] | None = None,
    registry: SensorRegistry | None = None,
    rights_state: RightsState = RightsState.OWNED,
    privacy_state: PrivacyState = PrivacyState.CLEARED,
) -> StreamHandle | RuntimeAttestation:
    """Open a stream. Webcam requires allow_webcam=True; missing hardware is BLOCKED."""
    kind = SourceType(source_type)
    if buffer_size < 1:
        raise ValidationError("buffer_size must be >= 1")

    sid = stream_id or f"stream-{kind.value}-{int(time.time() * 1000)}"
    sensor_key = sensor_id or f"sensor-{kind.value}-{sid}"
    frame = coordinate_frame or CoordinateFrame(
        name="opencv-camera", up_axis="-Y", forward_axis="+Z", origin_semantics="camera-centre"
    )

    if kind is SourceType.WEBCAM and not allow_webcam:
        return attest_blocked(
            "webcam",
            "webcam open refused: allow_webcam=True is required (opt-in only)",
        )

    reg = registry or DEFAULT_REGISTRY
    try:
        sensor = reg.get(sensor_key)
    except ValidationError:
        sensor = register_sensor(
            SensorDescriptor(
                sensor_id=sensor_key,
                source_type=kind,
                hardware=str(source),
                resolution=[0, 0],
                frame_rate=frame_rate,
                timestamp_domain=TimestampDomain.MONOTONIC,
                rights_state=rights_state,
                privacy_state=privacy_state,
                last_calibration=calibration_receipt,
                known_limitations=[],
                authority=(
                    AuthorityClass.RUNTIME_OBSERVED
                    if kind is SourceType.WEBCAM
                    else AuthorityClass.SENSOR_DERIVED
                ),
            ),
            registry=reg,
        )

    handle = StreamHandle(
        stream_id=sid,
        sensor=sensor,
        source_type=kind,
        state=StreamState.CLOSED,
        stats=StreamStats(buffer_capacity=buffer_size, opened_at=utc_now()),
        calibration_receipt=calibration_receipt,
        coordinate_frame=frame,
        _buffer=deque(maxlen=buffer_size),
        _raw_buffer=deque(maxlen=buffer_size),
        _monotonic_origin=time.monotonic(),
        _intrinsics=dict(intrinsics or {}),
    )

    try:
        if kind is SourceType.VIDEO_FILE:
            path = Path(source)
            if not path.is_file():
                return attest_blocked("video_file", f"video file not found: {path}")
            handle._capture = _open_video(path)
            handle.execution_class = ExecutionClass.PHYSICAL
            handle.state = StreamState.OPEN
            handle._closed = False
            w = int(handle._capture.get(cv2.CAP_PROP_FRAME_WIDTH) or 0)
            h = int(handle._capture.get(cv2.CAP_PROP_FRAME_HEIGHT) or 0)
            fps = float(handle._capture.get(cv2.CAP_PROP_FPS) or frame_rate)
            sensor.resolution = [w, h]
            sensor.frame_rate = fps

        elif kind in {SourceType.IMAGE_SEQUENCE, SourceType.BLENDER_RENDER}:
            paths = _list_sequence(Path(source))
            handle._paths = paths
            handle.execution_class = (
                ExecutionClass.PHYSICAL
                if kind is SourceType.BLENDER_RENDER
                else ExecutionClass.PHYSICAL
            )
            handle.state = StreamState.OPEN
            handle._closed = False
            probe = cv2.imread(str(paths[0]), cv2.IMREAD_COLOR)
            if probe is not None:
                sensor.resolution = [int(probe.shape[1]), int(probe.shape[0])]
            sensor.frame_rate = frame_rate
            if kind is SourceType.BLENDER_RENDER:
                sensor.rights_state = RightsState.SYNTHETIC
                sensor.privacy_state = PrivacyState.SYNTHETIC

        elif kind is SourceType.SCREEN_CAPTURE:
            probe = _screen_grab()
            if probe is None:
                return attest_blocked(
                    "screen_capture",
                    "screen capture unavailable (Pillow ImageGrab failed or refused)",
                )
            handle.execution_class = ExecutionClass.PHYSICAL
            handle.state = StreamState.OPEN
            handle._closed = False
            sensor.resolution = [int(probe.shape[1]), int(probe.shape[0])]
            sensor.frame_rate = frame_rate
            # Seed one frame so the stream is immediately readable.
            ocular = _make_frame(handle, probe)
            handle.push_frame(ocular, probe)

        elif kind is SourceType.WEBCAM:
            capture = cv2.VideoCapture(int(webcam_index))
            if not capture.isOpened():
                capture.release()
                return attest_blocked(
                    "webcam",
                    f"webcam index {webcam_index} could not be opened "
                    "(no camera present, permission denied, or device busy)",
                )
            handle._capture = capture
            handle.execution_class = ExecutionClass.PHYSICAL
            handle.state = StreamState.OPEN
            handle._closed = False
            w = int(capture.get(cv2.CAP_PROP_FRAME_WIDTH) or 0)
            h = int(capture.get(cv2.CAP_PROP_FRAME_HEIGHT) or 0)
            sensor.resolution = [w, h]
            sensor.frame_rate = float(capture.get(cv2.CAP_PROP_FPS) or frame_rate)
            sensor.known_limitations.append("live-camera; frames are ephemeral")

        else:
            return attest_blocked(kind.value, f"unsupported source_type: {kind.value}")

    except ValidationError as exc:
        return attest_blocked(kind.value, str(exc))

    with _STREAMS_LOCK:
        _STREAMS[sid] = handle
    return handle


def _read_source(handle: StreamHandle) -> tuple[np.ndarray, float | None] | None:
    """Decode the next sample from the underlying source without buffering it."""
    if handle.state not in {StreamState.OPEN, StreamState.EXHAUSTED}:
        return None
    if handle._closed:
        return None

    if handle.source_type is SourceType.VIDEO_FILE and handle._capture is not None:
        ok, array = handle._capture.read()
        if not ok or array is None:
            handle.state = StreamState.EXHAUSTED
            return None
        pos_ms = float(handle._capture.get(cv2.CAP_PROP_POS_MSEC) or 0.0)
        media_ts = pos_ms / 1000.0 if pos_ms > 0 else None
        return array, media_ts

    if handle.source_type in {SourceType.IMAGE_SEQUENCE, SourceType.BLENDER_RENDER}:
        if handle._path_index >= len(handle._paths):
            handle.state = StreamState.EXHAUSTED
            return None
        path = handle._paths[handle._path_index]
        handle._path_index += 1
        array = cv2.imread(str(path), cv2.IMREAD_COLOR)
        if array is None:
            raise ValidationError(f"failed to read image: {path}")
        # Frame-index timebase so sequences are deterministic across hosts.
        media_ts = (handle._path_index - 1) / max(handle.sensor.frame_rate, 1e-6)
        return array, media_ts

    if handle.source_type is SourceType.SCREEN_CAPTURE:
        array = _screen_grab()
        if array is None:
            handle.state = StreamState.ERROR
            return None
        return array, None

    if handle.source_type is SourceType.WEBCAM and handle._capture is not None:
        ok, array = handle._capture.read()
        if not ok or array is None:
            handle.state = StreamState.ERROR
            return None
        return array, None

    return None


def ingest_frame(handle: StreamHandle) -> bool:
    """Pull one sample from the source into the ring without delivering it.

    Returns True when a frame was enqueued. Used to create genuine buffer
    overrun (producer ahead of consumer). Keep-up callers should use
    ``read_frame`` instead, which ingests then immediately pops.
    """
    item = _read_source(handle)
    if item is None:
        return False
    image, media_ts = item
    frame = _make_frame(handle, image, timestamp=media_ts)
    handle.push_frame(frame, image)
    return True


def read_frame(handle: StreamHandle) -> tuple[OcularFrame, np.ndarray] | None:
    """Pull the next frame from the source, buffer it, and deliver it (FIFO).

    Keep-up path: ingest + immediate pop, so the ring never holds undelivered
    frames and ``frames_dropped`` stays 0. For deliberate overrun accounting,
    call ``ingest_frame`` repeatedly without ``pop_frame``.
    """
    if not ingest_frame(handle):
        return None
    delivered = handle.pop_frame()
    if delivered is None:
        return None
    frame, image = delivered
    if image is None:
        raise ValidationError(f"stream {handle.stream_id} delivered a frame without pixels")
    return frame, image


def iter_frames(handle: StreamHandle) -> Iterator[tuple[OcularFrame, np.ndarray]]:
    while True:
        item = read_frame(handle)
        if item is None:
            break
        yield item


def close_stream(handle: StreamHandle | str) -> dict[str, Any]:
    if isinstance(handle, str):
        with _STREAMS_LOCK:
            if handle not in _STREAMS:
                raise ValidationError(f"unknown stream_id: {handle}")
            handle = _STREAMS[handle]
    with handle._lock:
        if handle._capture is not None:
            with suppress(Exception):
                handle._capture.release()
            handle._capture = None
        handle._closed = True
        handle.state = StreamState.CLOSED
        handle.stats.closed_at = utc_now()
        state = handle.snapshot_state()
    with _STREAMS_LOCK:
        _STREAMS.pop(handle.stream_id, None)
    return state


def get_stream(stream_id: str) -> StreamHandle:
    """Return the live handle for a registered stream_id."""
    with _STREAMS_LOCK:
        if stream_id not in _STREAMS:
            raise ValidationError(f"unknown stream_id: {stream_id}")
        return _STREAMS[stream_id]


def get_stream_state(stream_id: str) -> dict[str, Any]:
    with _STREAMS_LOCK:
        if stream_id not in _STREAMS:
            raise ValidationError(f"unknown stream_id: {stream_id}")
        return _STREAMS[stream_id].snapshot_state()


def list_open_streams() -> list[str]:
    with _STREAMS_LOCK:
        return sorted(_STREAMS)


def attest_webcam_blocked(
    *,
    webcam_index: int = 0,
    reason: str | None = None,
) -> RuntimeAttestation:
    """Honest webcam BLOCKED attestation. Never fabricates a live frame.

    This host (and any host without a working camera) must surface BLOCKED
    rather than inventing pixels. Callers that need a live stream use the
    resumption protocol in docs/ocular/WEBCAM_RESUMPTION.md.
    """
    detail = reason or (
        f"webcam index {webcam_index} unavailable on this host "
        "(no camera present, permission denied, or device busy); "
        "live frames are never fabricated"
    )
    return attest_blocked("webcam", detail)


def open_stream_or_attest(
    source: str | Path,
    *,
    source_type: SourceType | str,
    stream_id: str | None = None,
    sensor_id: str | None = None,
    buffer_size: int = 32,
    allow_webcam: bool = False,
    webcam_index: int = 0,
    frame_rate: float = 30.0,
    calibration_receipt: str = "",
    coordinate_frame: CoordinateFrame | None = None,
    intrinsics: dict[str, Any] | None = None,
    registry: SensorRegistry | None = None,
    rights_state: RightsState = RightsState.OWNED,
    privacy_state: PrivacyState = PrivacyState.CLEARED,
) -> tuple[StreamHandle | None, RuntimeAttestation | None, dict[str, Any]]:
    """Open a stream and always return a JSON-serialisable status payload.

    Returns (handle|None, attestation|None, status_dict). A blocked open yields
    handle=None with an attestation whose execution_class is BLOCKED.
    """
    result = open_stream(
        source,
        source_type=source_type,
        stream_id=stream_id,
        sensor_id=sensor_id,
        buffer_size=buffer_size,
        allow_webcam=allow_webcam,
        webcam_index=webcam_index,
        frame_rate=frame_rate,
        calibration_receipt=calibration_receipt,
        coordinate_frame=coordinate_frame,
        intrinsics=intrinsics,
        registry=registry,
        rights_state=rights_state,
        privacy_state=privacy_state,
    )
    if isinstance(result, RuntimeAttestation):
        return None, result, {
            "status": "blocked",
            "execution_class": result.execution_class.value,
            "blocked_reason": result.blocked_reason,
            "runtime": result.runtime,
            "attestation": result.to_dict(),
        }
    return result, result.attestation, {
        "status": "open",
        "execution_class": result.execution_class.value,
        **result.snapshot_state(),
    }
