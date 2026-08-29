"""PNG encode/decode and pixel hashing. Container metadata is never hashed.

Blender stamps eXIf/tEXt so PNG *file* bytes differ across runs of the same
pixels. Equivalence is L1 decoded pixels or L2 linear float buffers.
"""

from __future__ import annotations

import hashlib
import struct
import zlib
from pathlib import Path
from typing import Iterable


PNG_SIG = b"\x89PNG\r\n\x1a\n"


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str | Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def _chunk(tag: bytes, data: bytes) -> bytes:
    crc = zlib.crc32(tag + data) & 0xFFFFFFFF
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", crc)


def write_rgb8_png(path: str | Path, width: int, height: int, rows: Iterable[bytes]) -> None:
    """Write an 8-bit RGB PNG. Each row is width*3 bytes."""
    raw = bytearray()
    expected = width * 3
    n = 0
    for row in rows:
        if len(row) != expected:
            raise ValueError(f"row length {len(row)} != {expected}")
        raw.append(0)  # filter None
        raw.extend(row)
        n += 1
    if n != height:
        raise ValueError(f"row count {n} != height {height}")
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    blob = PNG_SIG + _chunk(b"IHDR", ihdr) + _chunk(b"IDAT", zlib.compress(bytes(raw), 9)) + _chunk(b"IEND", b"")
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_bytes(blob)


def _paeth(a: int, b: int, c: int) -> int:
    p = a + b - c
    pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
    if pa <= pb and pa <= pc:
        return a
    if pb <= pc:
        return b
    return c


def _unfilter(filter_type: int, row: bytearray, prev: bytes, bpp: int) -> bytes:
    n = len(row)
    if filter_type == 0:
        return bytes(row)
    out = bytearray(n)
    if filter_type == 1:
        for i in range(n):
            left = out[i - bpp] if i >= bpp else 0
            out[i] = (row[i] + left) & 255
    elif filter_type == 2:
        for i in range(n):
            up = prev[i] if prev else 0
            out[i] = (row[i] + up) & 255
    elif filter_type == 3:
        for i in range(n):
            left = out[i - bpp] if i >= bpp else 0
            up = prev[i] if prev else 0
            out[i] = (row[i] + ((left + up) >> 1)) & 255
    elif filter_type == 4:
        for i in range(n):
            left = out[i - bpp] if i >= bpp else 0
            up = prev[i] if prev else 0
            ul = prev[i - bpp] if prev and i >= bpp else 0
            out[i] = (row[i] + _paeth(left, up, ul)) & 255
    else:
        raise ValueError(f"unsupported PNG filter {filter_type}")
    return bytes(out)


def decode_png_rgb8(path: str | Path) -> tuple[int, int, bytes]:
    """Decode an 8-bit RGB or RGBA PNG to packed RGB8 (alpha dropped).

    Returns (width, height, rgb_bytes). Rejects 16-bit, palette, and interlaced.
    """
    data = Path(path).read_bytes()
    if not data.startswith(PNG_SIG):
        raise ValueError(f"{path} is not a PNG")
    pos = len(PNG_SIG)
    width = height = None
    bit_depth = color_type = interlace = None
    idat = bytearray()
    while pos + 12 <= len(data):
        length = struct.unpack(">I", data[pos : pos + 4])[0]
        tag = data[pos + 4 : pos + 8]
        chunk = data[pos + 8 : pos + 8 + length]
        pos += 12 + length
        if tag == b"IHDR":
            width, height, bit_depth, color_type, _comp, _filt, interlace = struct.unpack(
                ">IIBBBBB", chunk
            )
        elif tag == b"IDAT":
            idat.extend(chunk)
        elif tag == b"IEND":
            break
    if width is None or height is None:
        raise ValueError(f"{path}: missing IHDR")
    if bit_depth != 8 or interlace != 0 or color_type not in (2, 6):
        raise ValueError(
            f"{path}: need 8-bit non-interlaced RGB/RGBA, got depth={bit_depth} "
            f"type={color_type} interlace={interlace}"
        )
    bpp = 3 if color_type == 2 else 4
    raw = zlib.decompress(bytes(idat))
    stride = width * bpp
    expected = height * (1 + stride)
    if len(raw) != expected:
        raise ValueError(f"{path}: inflated {len(raw)} bytes, expected {expected}")
    prev = b""
    rgb = bytearray(width * height * 3)
    o = 0
    for y in range(height):
        start = y * (1 + stride)
        filt = raw[start]
        row = _unfilter(filt, bytearray(raw[start + 1 : start + 1 + stride]), prev, bpp)
        prev = row
        if bpp == 3:
            rgb[o : o + stride] = row
            o += stride
        else:
            for x in range(width):
                i = x * 4
                rgb[o] = row[i]
                rgb[o + 1] = row[i + 1]
                rgb[o + 2] = row[i + 2]
                o += 3
    return width, height, bytes(rgb)


def encoded_pixels_sha256(path: str | Path) -> str:
    _w, _h, rgb = decode_png_rgb8(path)
    return sha256_bytes(rgb)


def container_sha256(path: str | Path) -> str:
    """PNG file bytes including metadata. Not an equivalence key."""
    return sha256_file(path)


def crop_rgb8(
    width: int, height: int, rgb: bytes, x0: int, y0: int, x1: int, y1: int
) -> tuple[int, int, bytes]:
    """Crop packed RGB8. Coordinates are half-open pixel bounds [x0,x1) x [y0,y1)."""
    if x0 < 0 or y0 < 0 or x1 > width or y1 > height or x1 <= x0 or y1 <= y0:
        raise ValueError(f"bad crop {(x0, y0, x1, y1)} of {width}x{height}")
    if len(rgb) != width * height * 3:
        raise ValueError(f"rgb length {len(rgb)} != {width * height * 3}")
    cw, ch = x1 - x0, y1 - y0
    out = bytearray(cw * ch * 3)
    for y in range(ch):
        src = ((y0 + y) * width + x0) * 3
        dst = y * cw * 3
        out[dst : dst + cw * 3] = rgb[src : src + cw * 3]
    return cw, ch, bytes(out)


def pixel_mismatch_count(a: bytes, b: bytes) -> int:
    """Count differing bytes (not pixels). Equal length required."""
    if len(a) != len(b):
        return max(len(a), len(b))
    return sum(1 for x, y in zip(a, b) if x != y)


def pixel_l1_stats(a: bytes, b: bytes, width: int, height: int) -> dict:
    """L1 decoded-pixel stats. 8-bit packed RGB. Never hashes the PNG container."""
    expected = width * height * 3
    if len(a) != expected or len(b) != expected:
        return {
            "pixel_exact": False,
            "comparable": False,
            "reason": f"buffer length a={len(a)} b={len(b)} expected={expected}",
            "pixels_compared": 0,
            "pixels_differing": None,
            "max_abs_error": None,
            "mean_abs_error": None,
            "first_diff_xy": None,
        }
    n = width * height
    differing = 0
    max_abs = 0
    sum_abs = 0
    first = None
    for i in range(n):
        o = i * 3
        dr = abs(a[o] - b[o])
        dg = abs(a[o + 1] - b[o + 1])
        db = abs(a[o + 2] - b[o + 2])
        mx = dr if dr >= dg and dr >= db else (dg if dg >= db else db)
        sum_abs += dr + dg + db
        if mx:
            differing += 1
            if first is None:
                first = [i % width, i // width]
            if mx > max_abs:
                max_abs = mx
    return {
        "pixel_exact": differing == 0,
        "comparable": True,
        "level": "L1_PIXEL_EXACT",
        "pixels_compared": n,
        "pixels_differing": differing,
        "differing_fraction": (differing / n) if n else 0.0,
        "max_abs_error": max_abs,
        "mean_abs_error": (sum_abs / (n * 3)) if n else 0.0,
        "first_diff_xy": first,
        "l1_sha256_a": sha256_bytes(a),
        "l1_sha256_b": sha256_bytes(b),
    }


def procedural_rgb_row(width: int, y: int, kind: str = "large") -> bytes:
    """Deterministic RGB row. Integer arithmetic only."""
    buf = bytearray(width * 3)
    if kind == "large":
        for x in range(width):
            buf[x * 3] = (x * 13 + y * 7) & 255
            buf[x * 3 + 1] = (x * 3 + y * 17) & 255
            buf[x * 3 + 2] = (x ^ y) & 255
    elif kind == "floor":
        for x in range(width):
            cell = ((x >> 6) ^ (y >> 6)) & 1
            g = (x + y) & 255
            if cell:
                buf[x * 3] = 40
                buf[x * 3 + 1] = 40
                buf[x * 3 + 2] = 48
            else:
                buf[x * 3] = 200
                buf[x * 3 + 1] = 198
                buf[x * 3 + 2] = min(255, 180 + (g >> 3))
    else:
        raise ValueError(f"unknown procedural kind {kind}")
    return bytes(buf)


def write_procedural_png(path: str | Path, width: int, height: int, kind: str = "large") -> str:
    write_rgb8_png(path, width, height, (procedural_rgb_row(width, y, kind) for y in range(height)))
    return sha256_file(path)
