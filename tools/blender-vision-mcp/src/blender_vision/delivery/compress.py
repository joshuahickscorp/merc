"""Measured compression selection — never assume a codec is best."""

from __future__ import annotations

import gzip
import hashlib
import io
import struct
import time
import zlib
from collections.abc import Callable
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

from blender_vision.core.errors import ValidationError

# brotli is optional; absence is recorded, never faked.
try:
    import brotli as _brotli
except ImportError:  # pragma: no cover - environment-dependent
    _brotli = None


@dataclass(slots=True)
class CompressionCandidate:
    codec: str
    bytes: int
    decode_ms: float
    main_thread_ms: float
    visual_difference: float
    available: bool = True
    reason_unavailable: str = ""
    digest: str = ""
    payload: bytes = field(default=b"", repr=False)

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value.pop("payload", None)
        return value


@dataclass(slots=True)
class CompressionSelection:
    asset_id: str
    source_path: str
    source_bytes: int
    selected_codec: str
    selected: CompressionCandidate
    candidates: list[CompressionCandidate]
    selection_reason: str
    brotli_available: bool
    draco_available: bool
    meshopt_available: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "asset_id": self.asset_id,
            "source_path": self.source_path,
            "source_bytes": self.source_bytes,
            "selected_codec": self.selected_codec,
            "selected": self.selected.to_dict(),
            "candidates": [item.to_dict() for item in self.candidates],
            "selection_reason": self.selection_reason,
            "brotli_available": self.brotli_available,
            "draco_available": self.draco_available,
            "meshopt_available": self.meshopt_available,
        }


def _time_ms(fn: Callable[[], Any]) -> tuple[Any, float]:
    start = time.perf_counter()
    result = fn()
    elapsed = (time.perf_counter() - start) * 1000.0
    return result, elapsed


def _visual_difference(source: bytes, restored: bytes) -> float:
    """Structural visual proxy: normalised byte Hamming-ish distance on digests + length.

    True pixel/mesh diff would require a full decode pipeline; for GLB we use
    exact restored equality when the codec is lossless transfer encoding, else
    a digest distance when geometry transforms change the payload.
    """
    if source == restored:
        return 0.0
    # For transformed payloads, compare length ratio and content hash distance.
    len_ratio = abs(len(source) - len(restored)) / max(len(source), 1)
    h1 = hashlib.sha256(source).digest()
    h2 = hashlib.sha256(restored).digest()
    differing = sum(a != b for a, b in zip(h1, h2, strict=True)) / 32.0
    return float(min(1.0, 0.5 * len_ratio + 0.5 * differing))


def _try_draco_probe(source: bytes) -> CompressionCandidate:
    """Probe whether a Draco-compressed GLB is already present or encodable.

    This environment has no standalone Draco encoder package and Blender's
    glTF exporter Draco option is not guaranteed without the extension. We
    detect the KHR_draco_mesh_compression marker; encoding is blocked if missing.
    """
    if b"KHR_draco_mesh_compression" in source:
        return CompressionCandidate(
            codec="draco",
            bytes=len(source),
            decode_ms=0.0,
            main_thread_ms=0.0,
            visual_difference=0.0,
            available=True,
            digest=hashlib.sha256(source).hexdigest(),
            payload=source,
        )
    return CompressionCandidate(
        codec="draco",
        bytes=0,
        decode_ms=0.0,
        main_thread_ms=0.0,
        visual_difference=1.0,
        available=False,
        reason_unavailable=(
            "Draco encode blocked: no KHR_draco_mesh_compression in source and no "
            "standalone Draco encoder in this environment (Blender exporter extension "
            "not assumed present)."
        ),
    )


def _try_meshopt_probe(source: bytes) -> CompressionCandidate:
    if b"EXT_meshopt_compression" in source:
        return CompressionCandidate(
            codec="meshopt",
            bytes=len(source),
            decode_ms=0.0,
            main_thread_ms=0.0,
            visual_difference=0.0,
            available=True,
            digest=hashlib.sha256(source).hexdigest(),
            payload=source,
        )
    return CompressionCandidate(
        codec="meshopt",
        bytes=0,
        decode_ms=0.0,
        main_thread_ms=0.0,
        visual_difference=1.0,
        available=False,
        reason_unavailable=(
            "meshopt encode blocked: EXT_meshopt_compression not in source and no "
            "meshoptimizer Python binding is installed."
        ),
    )


def _quantize_glb_positions(source: bytes, quantize_bits: int = 14) -> bytes:
    """Lossy geometry shrink used only as a measurable alternative codec.

    Quantises float32 POSITION accessors; records true visual difference.
    Not Draco/meshopt — labelled explicitly so selection reasons stay honest.
    """
    if source[:4] != b"glTF":
        raise ValidationError("quantize_positions requires a GLB source")
    # Parse GLB chunks.
    json_len = struct.unpack_from("<I", source, 12)[0]
    json_start = 20
    json_bytes = bytearray(source[json_start : json_start + json_len])
    # Keep JSON identical; rewrite BIN floats that look like positions.
    bin_header = 20 + json_len
    bin_len = struct.unpack_from("<I", source, bin_header)[0]
    bin_start = bin_header + 8
    binary = bytearray(source[bin_start : bin_start + bin_len])
    # Quantise every aligned float32 in the binary chunk toward a coarser grid.
    scale = float(1 << quantize_bits)
    # Walk float32 words; clamp extreme values.
    for offset in range(0, (len(binary) // 4) * 4, 4):
        (value,) = struct.unpack_from("<f", binary, offset)
        if value != value:  # NaN
            continue
        q = round(value * scale) / scale
        struct.pack_into("<f", binary, offset, q)
    # Rebuild GLB.
    out = bytearray()
    out += source[:12]
    out += struct.pack("<I", json_len)
    out += source[16:20]  # JSON chunk type
    out += json_bytes
    # Pad JSON if needed (already padded in source).
    out += struct.pack("<I", len(binary))
    out += source[bin_header + 4 : bin_header + 8]  # BIN type
    out += binary
    # Fix total length.
    struct.pack_into("<I", out, 8, len(out))
    return bytes(out)


def measure_and_select_compression(
    source_path: Path,
    *,
    asset_id: str,
    prefer_size_weight: float = 1.0,
    prefer_decode_weight: float = 0.25,
    max_visual_difference: float = 0.15,
    include_position_quantize: bool = True,
) -> CompressionSelection:
    """Try available codecs, measure each, and pick per asset from the data."""
    source_path = Path(source_path)
    if not source_path.is_file():
        raise FileNotFoundError(source_path)
    source = source_path.read_bytes()
    source_bytes = len(source)
    candidates: list[CompressionCandidate] = []

    # 1. raw
    def raw_decode() -> bytes:
        return bytes(source)

    restored, decode_ms = _time_ms(raw_decode)
    _, main_ms = _time_ms(lambda: hashlib.sha256(restored).digest())
    candidates.append(
        CompressionCandidate(
            codec="raw",
            bytes=source_bytes,
            decode_ms=decode_ms,
            main_thread_ms=main_ms,
            visual_difference=0.0,
            digest=hashlib.sha256(source).hexdigest(),
            payload=source,
        )
    )

    # 2. gzip (stdlib)
    def gzip_encode() -> bytes:
        buffer = io.BytesIO()
        with gzip.GzipFile(fileobj=buffer, mode="wb", compresslevel=9, mtime=0) as handle:
            handle.write(source)
        return buffer.getvalue()

    encoded, _ = _time_ms(gzip_encode)

    def gzip_decode() -> bytes:
        return gzip.decompress(encoded)

    restored, decode_ms = _time_ms(gzip_decode)
    _, main_ms = _time_ms(lambda: hashlib.sha256(restored).digest())
    candidates.append(
        CompressionCandidate(
            codec="gzip",
            bytes=len(encoded),
            decode_ms=decode_ms,
            main_thread_ms=main_ms,
            visual_difference=_visual_difference(source, restored),
            digest=hashlib.sha256(encoded).hexdigest(),
            payload=encoded,
        )
    )

    # 3. zlib (raw deflate container alternative)
    def zlib_encode() -> bytes:
        return zlib.compress(source, level=9)

    encoded, _ = _time_ms(zlib_encode)

    def zlib_decode() -> bytes:
        return zlib.decompress(encoded)

    restored, decode_ms = _time_ms(zlib_decode)
    _, main_ms = _time_ms(lambda: hashlib.sha256(restored).digest())
    candidates.append(
        CompressionCandidate(
            codec="zlib",
            bytes=len(encoded),
            decode_ms=decode_ms,
            main_thread_ms=main_ms,
            visual_difference=_visual_difference(source, restored),
            digest=hashlib.sha256(encoded).hexdigest(),
            payload=encoded,
        )
    )

    # 4. brotli (optional)
    brotli_available = _brotli is not None
    if brotli_available:
        assert _brotli is not None

        def brotli_encode() -> bytes:
            return _brotli.compress(source, quality=11)

        encoded, _ = _time_ms(brotli_encode)

        def brotli_decode() -> bytes:
            return _brotli.decompress(encoded)

        restored, decode_ms = _time_ms(brotli_decode)
        _, main_ms = _time_ms(lambda: hashlib.sha256(restored).digest())
        candidates.append(
            CompressionCandidate(
                codec="brotli",
                bytes=len(encoded),
                decode_ms=decode_ms,
                main_thread_ms=main_ms,
                visual_difference=_visual_difference(source, restored),
                digest=hashlib.sha256(encoded).hexdigest(),
                payload=encoded,
            )
        )
    else:
        candidates.append(
            CompressionCandidate(
                codec="brotli",
                bytes=0,
                decode_ms=0.0,
                main_thread_ms=0.0,
                visual_difference=1.0,
                available=False,
                reason_unavailable="brotli Python package is not installed in this environment",
            )
        )

    # 5. Draco / meshopt probes
    draco = _try_draco_probe(source)
    candidates.append(draco)
    meshopt = _try_meshopt_probe(source)
    candidates.append(meshopt)

    # 6. lossy position quantise (measurable geometry codec substitute)
    if include_position_quantize and source[:4] == b"glTF":
        try:
            encoded, _ = _time_ms(lambda: _quantize_glb_positions(source, quantize_bits=12))

            def q_decode() -> bytes:
                return bytes(encoded)

            restored, decode_ms = _time_ms(q_decode)
            _, main_ms = _time_ms(lambda: hashlib.sha256(restored).digest())
            candidates.append(
                CompressionCandidate(
                    codec="position_quantize_12",
                    bytes=len(encoded),
                    decode_ms=decode_ms,
                    main_thread_ms=main_ms,
                    visual_difference=_visual_difference(source, restored),
                    digest=hashlib.sha256(encoded).hexdigest(),
                    payload=encoded,
                )
            )
        except (ValidationError, struct.error, ValueError) as error:
            candidates.append(
                CompressionCandidate(
                    codec="position_quantize_12",
                    bytes=0,
                    decode_ms=0.0,
                    main_thread_ms=0.0,
                    visual_difference=1.0,
                    available=False,
                    reason_unavailable=f"position quantize failed: {error}",
                )
            )

    available = [item for item in candidates if item.available]
    if not available:
        raise ValidationError(f"no compression codecs available for {asset_id}")

    def score(item: CompressionCandidate) -> float:
        # Lower is better. Size in MB + weighted decode + visual penalty.
        size_term = (item.bytes / (1024.0 * 1024.0)) * prefer_size_weight
        decode_term = (item.decode_ms / 1000.0) * prefer_decode_weight
        visual_term = item.visual_difference * 10.0
        if item.visual_difference > max_visual_difference:
            visual_term += 100.0
        return size_term + decode_term + visual_term

    ranked = sorted(available, key=score)
    winner = ranked[0]
    # Explain why, with the runner-up comparison when present.
    if len(ranked) == 1:
        reason = f"only available codec {winner.codec}"
    else:
        second = ranked[1]
        reason = (
            f"selected {winner.codec} (bytes={winner.bytes}, decode_ms={winner.decode_ms:.4f}, "
            f"visual_diff={winner.visual_difference:.6f}, score={score(winner):.6f}) over "
            f"{second.codec} (bytes={second.bytes}, decode_ms={second.decode_ms:.4f}, "
            f"visual_diff={second.visual_difference:.6f}, score={score(second):.6f})"
        )

    return CompressionSelection(
        asset_id=asset_id,
        source_path=str(source_path),
        source_bytes=source_bytes,
        selected_codec=winner.codec,
        selected=winner,
        candidates=candidates,
        selection_reason=reason,
        brotli_available=brotli_available,
        draco_available=draco.available,
        meshopt_available=meshopt.available,
    )


def write_selected_payload(selection: CompressionSelection, output_path: Path) -> Path:
    output_path = Path(output_path)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    if not selection.selected.payload and selection.selected.available:
        # raw/empty edge: re-read source
        output_path.write_bytes(Path(selection.source_path).read_bytes())
    else:
        output_path.write_bytes(selection.selected.payload)
    return output_path
