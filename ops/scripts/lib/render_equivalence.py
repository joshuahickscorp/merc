#!/usr/bin/env python3
"""Merc Render output-equivalence contract (plan §3).

Reusable from other lanes. L0 is encoded-file equality after stripping PNG
metadata (eXIf/tEXt/zTXt/iTXt/tIME). L1 is decoded 8-bit RGB. L2 is the
MERCLIN1 scene-linear float dump (not 8-bit PNG). L3 is a named metric and
bound; it is never substituted for a failed exact level.

    python3 ops/scripts/lib/render_equivalence.py compare \\
        --png-a A.png --png-b B.png [--linear-a A.lin --linear-b B.lin] \\
        [--l3-metric max_abs_linear --l3-bound 0.01] \\
        [--required L1_PIXEL_EXACT]
"""

from __future__ import annotations

import argparse
import hashlib
import json
import struct
import sys
import zlib
from typing import Any


PNG_SIG = b"\x89PNG\r\n\x1a\n"
LINEAR_MAGIC = b"MERCLIN1"
METADATA_CHUNKS = {"tEXt", "zTXt", "iTXt", "eXIf", "tIME"}

L0 = "L0_ENCODED_FILE_EQUAL"
L1 = "L1_PIXEL_EXACT"
L2 = "L2_LINEAR_BUFFER_EXACT"
L3 = "L3_QUALITY_CONTRACT"


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def parse_png_chunks(raw: bytes) -> list[tuple[str, bytes]]:
    if len(raw) < 8 or raw[:8] != PNG_SIG:
        raise ValueError("not a PNG")
    chunks: list[tuple[str, bytes]] = []
    i = 8
    while i + 12 <= len(raw):
        n = struct.unpack(">I", raw[i : i + 4])[0]
        if i + 12 + n > len(raw):
            raise ValueError("truncated PNG chunk")
        typ = raw[i + 4 : i + 8].decode("latin1")
        data = raw[i + 8 : i + 8 + n]
        want = struct.unpack(">I", raw[i + 8 + n : i + 12 + n])[0]
        crc = zlib.crc32(raw[i + 4 : i + 8 + n]) & 0xFFFFFFFF
        if crc != want:
            raise ValueError(f"PNG chunk {typ} crc mismatch")
        chunks.append((typ, data))
        i += 12 + n
        if typ == "IEND":
            break
    if not chunks or chunks[0][0] != "IHDR":
        raise ValueError("PNG missing IHDR")
    if chunks[-1][0] != "IEND":
        raise ValueError("PNG missing IEND")
    return chunks


def serialize_png(chunks: list[tuple[str, bytes]]) -> bytes:
    out = bytearray(PNG_SIG)
    for typ, data in chunks:
        if len(typ) != 4:
            raise ValueError(f"bad chunk type {typ!r}")
        out.extend(struct.pack(">I", len(data)))
        tb = typ.encode("latin1")
        out.extend(tb)
        out.extend(data)
        out.extend(struct.pack(">I", zlib.crc32(tb + data) & 0xFFFFFFFF))
    return bytes(out)


def normalize_png(raw: bytes) -> tuple[bytes, list[str], list[str]]:
    chunks = parse_png_chunks(raw)
    kept: list[tuple[str, bytes]] = []
    stripped: list[str] = []
    kept_types: list[str] = []
    for typ, data in chunks:
        if typ in METADATA_CHUNKS:
            stripped.append(typ)
            continue
        kept.append((typ, data))
        kept_types.append(typ)
    return serialize_png(kept), stripped, kept_types


def _paeth(a: int, b: int, c: int) -> int:
    p = a + b - c
    pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
    if pa <= pb and pa <= pc:
        return a
    if pb <= pc:
        return b
    return c


def decode_png_rgb(raw: bytes) -> dict[str, Any]:
    chunks = parse_png_chunks(raw)
    ihdr = chunks[0][1]
    if len(ihdr) != 13:
        raise ValueError("bad IHDR")
    width, height, bit_depth, color_type, compression, filt, interlace = struct.unpack(
        ">IIBBBBB", ihdr
    )
    if bit_depth != 8 or compression != 0 or filt != 0 or interlace != 0:
        raise ValueError(
            f"unsupported PNG ihdr bit={bit_depth} ctype={color_type} "
            f"comp={compression} filter={filt} interlace={interlace}"
        )
    if color_type == 2:
        ch = 3
    elif color_type == 6:
        ch = 4
    else:
        raise ValueError(f"unsupported PNG color type {color_type}")
    idat = b"".join(data for typ, data in chunks if typ == "IDAT")
    data = zlib.decompress(idat)
    stride = width * ch
    expect = height * (1 + stride)
    if len(data) != expect:
        raise ValueError(f"PNG inflate size {len(data)}, want {expect}")
    rows: list[bytearray] = []
    prev = bytearray(stride)
    off = 0
    for _ in range(height):
        ftype = data[off]
        scan = bytearray(data[off + 1 : off + 1 + stride])
        off += 1 + stride
        if ftype == 0:
            pass
        elif ftype == 1:
            for i in range(stride):
                left = scan[i - ch] if i >= ch else 0
                scan[i] = (scan[i] + left) & 255
        elif ftype == 2:
            for i in range(stride):
                scan[i] = (scan[i] + prev[i]) & 255
        elif ftype == 3:
            for i in range(stride):
                left = scan[i - ch] if i >= ch else 0
                scan[i] = (scan[i] + ((left + prev[i]) // 2)) & 255
        elif ftype == 4:
            for i in range(stride):
                left = scan[i - ch] if i >= ch else 0
                up = prev[i]
                ul = prev[i - ch] if i >= ch else 0
                scan[i] = (scan[i] + _paeth(left, up, ul)) & 255
        else:
            raise ValueError(f"unsupported PNG filter {ftype}")
        rows.append(scan)
        prev = scan
    rgb = bytearray(width * height * 3)
    di = 0
    for row in rows:
        for x in range(width):
            so = x * ch
            rgb[di] = row[so]
            rgb[di + 1] = row[so + 1]
            rgb[di + 2] = row[so + 2]
            di += 3
    return {
        "width": width,
        "height": height,
        "channels": 3,
        "pix": bytes(rgb),
    }


def compare_encoded_png(a: bytes, b: bytes) -> dict[str, Any]:
    out: dict[str, Any] = {
        "holds": False,
        "raw_bytes_equal": a == b,
        "normalized_bytes_equal": False,
        "raw_sha256_a": sha256_hex(a),
        "raw_sha256_b": sha256_hex(b),
        "normalized_sha256_a": "",
        "normalized_sha256_b": "",
        "stripped_chunks_a": [],
        "stripped_chunks_b": [],
        "kept_chunks_a": [],
        "kept_chunks_b": [],
    }
    try:
        na, sa, ka = normalize_png(a)
        nb, sb, kb = normalize_png(b)
    except ValueError as exc:
        out["error"] = str(exc)
        return out
    out["stripped_chunks_a"] = sa
    out["stripped_chunks_b"] = sb
    out["kept_chunks_a"] = ka
    out["kept_chunks_b"] = kb
    out["normalized_bytes_equal"] = na == nb
    out["normalized_sha256_a"] = sha256_hex(na)
    out["normalized_sha256_b"] = sha256_hex(nb)
    out["holds"] = bool(out["normalized_bytes_equal"])
    return out


L1_PATH_DIGEST = "decoded_pixel_digest"
L1_PATH_ARRAY = "pixel_array"


def digest_pixels(pix: bytes) -> str:
    return sha256_hex(pix)


def compare_l1(
    digest_a: str = "",
    digest_b: str = "",
    pix_a: dict[str, Any] | None = None,
    pix_b: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """L1 fast path: digest of decoded RGB. Walks pixels only on mismatch."""
    da = digest_a or (digest_pixels(pix_a["pix"]) if pix_a and pix_a.get("pix") else "")
    db = digest_b or (digest_pixels(pix_b["pix"]) if pix_b and pix_b.get("pix") else "")
    out: dict[str, Any] = {
        "holds": False,
        "pixel_sha256_a": da,
        "pixel_sha256_b": db,
        "path": L1_PATH_DIGEST,
        "decoded": False,
    }
    if not da or not db:
        out["error"] = "missing decoded-pixel digest"
        return out
    if da == db:
        out["holds"] = True
        if pix_a:
            out["width"] = pix_a.get("width", 0)
            out["height"] = pix_a.get("height", 0)
            out["channels"] = pix_a.get("channels", 0)
            out["pixels"] = int(out["width"]) * int(out["height"])
        return out
    if not pix_a or not pix_b:
        out["error"] = "digests differ; pixel buffers required for divergence diagnostics"
        return out
    walked = compare_pixels(pix_a, pix_b)
    walked["path"] = L1_PATH_ARRAY
    walked["decoded"] = False
    return walked


def compare_pixels(a: dict[str, Any], b: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {
        "holds": False,
        "width": a.get("width", 0),
        "height": a.get("height", 0),
        "channels": a.get("channels", 0),
        "pixels": int(a.get("width", 0)) * int(a.get("height", 0)),
        "differing_pixels": 0,
        "max_abs": 0,
        "mean_abs": 0.0,
        "pixel_sha256_a": sha256_hex(a.get("pix", b"")),
        "pixel_sha256_b": sha256_hex(b.get("pix", b"")),
    }
    if (a.get("width"), a.get("height"), a.get("channels")) != (
        b.get("width"),
        b.get("height"),
        b.get("channels"),
    ):
        out["error"] = "shape %dx%dx%d vs %dx%dx%d" % (
            a.get("width", 0),
            a.get("height", 0),
            a.get("channels", 0),
            b.get("width", 0),
            b.get("height", 0),
            b.get("channels", 0),
        )
        return out
    pa, pb = a["pix"], b["pix"]
    if len(pa) != len(pb):
        out["error"] = "pixel buffer length mismatch"
        return out
    ch = int(a["channels"])
    w, h = int(a["width"]), int(a["height"])
    total = 0
    first = None
    differing = 0
    max_abs = 0
    for y in range(h):
        for x in range(w):
            off = (y * w + x) * ch
            pixel_diff = False
            for c in range(ch):
                d = abs(pa[off + c] - pb[off + c])
                total += d
                if d > max_abs:
                    max_abs = d
                if d:
                    pixel_diff = True
            if pixel_diff:
                differing += 1
                if first is None:
                    first = [x, y]
    out["differing_pixels"] = differing
    out["max_abs"] = max_abs
    out["mean_abs"] = (total / len(pa)) if pa else 0.0
    if first is not None:
        out["first_diff_xy"] = first
    out["holds"] = differing == 0
    return out


def parse_linear(raw: bytes) -> dict[str, Any]:
    if len(raw) < 20:
        raise ValueError(f"linear buffer too short: {len(raw)} bytes")
    if raw[:8] != LINEAR_MAGIC:
        raise ValueError(f"linear buffer magic {raw[:8]!r}, want {LINEAR_MAGIC!r}")
    width, height, channels = struct.unpack_from("<III", raw, 8)
    if width < 1 or height < 1 or channels not in (3, 4):
        raise ValueError(f"linear buffer shape {width}x{height}x{channels} rejected")
    need = 20 + width * height * channels * 4
    if len(raw) != need:
        raise ValueError(f"linear buffer size {len(raw)}, want {need}")
    n = width * height * channels
    pix = list(struct.unpack_from("<" + "f" * n, raw, 20))
    return {"width": width, "height": height, "channels": channels, "pix": pix}


def marshal_linear(buf: dict[str, Any]) -> bytes:
    w, h, ch = int(buf["width"]), int(buf["height"]), int(buf["channels"])
    pix = buf["pix"]
    if len(pix) != w * h * ch:
        raise ValueError("linear buffer length mismatch")
    return LINEAR_MAGIC + struct.pack("<III", w, h, ch) + struct.pack("<" + "f" * len(pix), *pix)


def compare_linear(a: dict[str, Any], b: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {
        "holds": False,
        "bits_equal": False,
        "width": a.get("width", 0),
        "height": a.get("height", 0),
        "channels": a.get("channels", 0),
        "values": len(a.get("pix", [])),
        "differing_values": 0,
        "differing_pixels": 0,
        "max_abs": 0.0,
        "mean_abs": 0.0,
        "rmse": 0.0,
        "buffer_sha256_a": sha256_hex(marshal_linear(a)) if "pix" in a else "",
        "buffer_sha256_b": sha256_hex(marshal_linear(b)) if "pix" in b else "",
    }
    if (a.get("width"), a.get("height"), a.get("channels")) != (
        b.get("width"),
        b.get("height"),
        b.get("channels"),
    ):
        out["error"] = "shape mismatch"
        return out
    pa, pb = a["pix"], b["pix"]
    if len(pa) != len(pb):
        out["error"] = "linear buffer length mismatch"
        return out
    ch = int(a["channels"])
    w, h = int(a["width"]), int(a["height"])
    bits_equal = True
    differing_values = 0
    differing_pixels = 0
    max_abs = 0.0
    total = 0.0
    sum_sq = 0.0
    first = None
    for y in range(h):
        for x in range(w):
            off = (y * w + x) * ch
            pixel_diff = False
            for c in range(ch):
                av, bv = float(pa[off + c]), float(pb[off + c])
                if struct.pack("<f", av) != struct.pack("<f", bv):
                    bits_equal = False
                    differing_values += 1
                    pixel_diff = True
                d = abs(av - bv)
                total += d
                sum_sq += d * d
                if d > max_abs:
                    max_abs = d
            if pixel_diff:
                differing_pixels += 1
                if first is None:
                    first = [x, y]
    n = float(len(pa))
    out["bits_equal"] = bits_equal
    out["differing_values"] = differing_values
    out["differing_pixels"] = differing_pixels
    out["max_abs"] = max_abs
    out["mean_abs"] = (total / n) if n else 0.0
    out["rmse"] = (sum_sq / n) ** 0.5 if n else 0.0
    if first is not None:
        out["first_diff_xy"] = first
    out["holds"] = bits_equal
    return out


def evaluate_quality(
    lin_a: dict[str, Any] | None,
    lin_b: dict[str, Any] | None,
    pix_a: dict[str, Any] | None,
    pix_b: dict[str, Any] | None,
    metric: str,
    bound: float,
) -> dict[str, Any]:
    out: dict[str, Any] = {
        "evaluated": False,
        "holds": False,
        "metric": metric,
        "bound": bound,
        "note": "L3 is a bounded tolerance, not an exact level; it is never substituted for L0/L1/L2",
    }
    metric = (metric or "").strip()
    if not metric:
        out["error"] = "L3 refused: metric is empty (no implicit default)"
        return out
    if bound != bound or bound < 0:  # NaN or negative
        out["error"] = "L3 refused: bound must be a finite number >= 0"
        return out
    lin = compare_linear(lin_a, lin_b) if lin_a and lin_b else None
    pix = compare_pixels(pix_a, pix_b) if pix_a and pix_b else None
    if lin is not None:
        out["linear"] = lin
    if pix is not None:
        out["pixel"] = pix
    observed = None
    err = ""
    if metric == "max_abs_linear":
        observed, err = (lin or {}).get("max_abs"), (lin or {}).get("error", "") or (
            "" if lin else "linear buffers missing"
        )
    elif metric == "mean_abs_linear":
        observed, err = (lin or {}).get("mean_abs"), (lin or {}).get("error", "") or (
            "" if lin else "linear buffers missing"
        )
    elif metric == "rmse_linear":
        observed, err = (lin or {}).get("rmse"), (lin or {}).get("error", "") or (
            "" if lin else "linear buffers missing"
        )
    elif metric == "max_abs_u8":
        observed, err = (pix or {}).get("max_abs"), (pix or {}).get("error", "") or (
            "" if pix else "png bytes missing"
        )
    elif metric == "mean_abs_u8":
        observed, err = (pix or {}).get("mean_abs"), (pix or {}).get("error", "") or (
            "" if pix else "png bytes missing"
        )
    elif metric == "differing_pixels_u8":
        observed, err = (pix or {}).get("differing_pixels"), (pix or {}).get("error", "") or (
            "" if pix else "png bytes missing"
        )
    elif metric == "differing_pixels_linear":
        observed, err = (lin or {}).get("differing_pixels"), (lin or {}).get("error", "") or (
            "" if lin else "linear buffers missing"
        )
    else:
        out["error"] = f"L3 refused: unknown metric {metric!r}"
        return out
    if err:
        out["error"] = str(err)
        return out
    out["evaluated"] = True
    out["observed"] = float(observed)
    out["holds"] = float(observed) <= float(bound)
    return out


def highest_exact_holding(rep: dict[str, Any]) -> str:
    if rep.get("l2_linear_buffer_exact", {}).get("holds"):
        return L2
    if rep.get("l1_pixel_exact", {}).get("holds"):
        return L1
    if rep.get("l0_encoded_file_equal", {}).get("holds"):
        return L0
    return "NONE"


def meets_required(rep: dict[str, Any]) -> tuple[bool, bool]:
    req = (rep.get("required_level") or "").strip()
    if not req:
        return True, False
    holds = False
    if req == L0:
        holds = bool(rep["l0_encoded_file_equal"].get("holds"))
    elif req == L1:
        holds = bool(rep["l1_pixel_exact"].get("holds"))
    elif req == L2:
        holds = bool(rep["l2_linear_buffer_exact"].get("holds"))
    elif req == L3:
        l3 = rep["l3_quality_contract"]
        return bool(l3.get("evaluated") and l3.get("holds")), False
    else:
        return False, False
    if holds:
        return True, False
    l3 = rep.get("l3_quality_contract") or {}
    if l3.get("evaluated") and l3.get("holds"):
        return False, True
    return False, False


def compare_pair(
    png_a: bytes | None,
    png_b: bytes | None,
    lin_a: bytes | None,
    lin_b: bytes | None,
    metric: str = "",
    bound: float = 0.0,
    required: str = "",
) -> dict[str, Any]:
    rep: dict[str, Any] = {
        "required_level": required,
        "l0_encoded_file_equal": {},
        "l1_pixel_exact": {},
        "l2_linear_buffer_exact": {},
        "l3_quality_contract": {},
    }
    pix_a = pix_b = None
    if png_a and png_b:
        rep["l0_encoded_file_equal"] = compare_encoded_png(png_a, png_b)
        try:
            pix_a = decode_png_rgb(png_a)
            pix_b = decode_png_rgb(png_b)
            rep["l1_pixel_exact"] = compare_pixels(pix_a, pix_b)
        except ValueError as exc:
            rep["l1_pixel_exact"] = {"holds": False, "error": str(exc)}
    else:
        rep["l0_encoded_file_equal"] = {"holds": False, "error": "png bytes missing"}
        rep["l1_pixel_exact"] = {"holds": False, "error": "png bytes missing"}

    la = lb = None
    if lin_a and lin_b:
        try:
            la = parse_linear(lin_a)
            lb = parse_linear(lin_b)
            rep["l2_linear_buffer_exact"] = compare_linear(la, lb)
        except ValueError as exc:
            rep["l2_linear_buffer_exact"] = {"holds": False, "error": str(exc)}
    else:
        rep["l2_linear_buffer_exact"] = {"holds": False, "error": "linear buffers missing"}

    if metric.strip():
        rep["l3_quality_contract"] = evaluate_quality(la, lb, pix_a, pix_b, metric, bound)
    else:
        rep["l3_quality_contract"] = {
            "evaluated": False,
            "holds": False,
            "note": "L3 not evaluated: no metric/bound stated (exact levels preferred)",
        }

    rep["highest_exact_holding"] = highest_exact_holding(rep)
    meets, refused = meets_required(rep)
    rep["meets_required"] = meets
    rep["silent_downgrade_refused"] = refused
    return rep


def _read(path: str) -> bytes:
    with open(path, "rb") as fh:
        return fh.read()


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="render_equivalence")
    sub = p.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("compare")
    c.add_argument("--png-a", default="")
    c.add_argument("--png-b", default="")
    c.add_argument("--linear-a", default="")
    c.add_argument("--linear-b", default="")
    c.add_argument("--l3-metric", default="")
    c.add_argument("--l3-bound", type=float, default=0.0)
    c.add_argument("--required", default="")
    d = sub.add_parser("pixel-digest")
    d.add_argument("--png", required=True)
    args = p.parse_args(argv)
    if args.cmd == "pixel-digest":
        raw = _read(args.png)
        pix = decode_png_rgb(raw)
        sys.stdout.write(digest_pixels(pix["pix"]) + "\n")
        return 0
    if args.cmd == "compare":
        png_a = _read(args.png_a) if args.png_a else None
        png_b = _read(args.png_b) if args.png_b else None
        lin_a = _read(args.linear_a) if args.linear_a else None
        lin_b = _read(args.linear_b) if args.linear_b else None
        rep = compare_pair(
            png_a,
            png_b,
            lin_a,
            lin_b,
            metric=args.l3_metric,
            bound=args.l3_bound,
            required=args.required,
        )
        json.dump(rep, sys.stdout, separators=(",", ":"))
        sys.stdout.write("\n")
        return 0
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
