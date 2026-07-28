"""Decode governed multilayer OpenEXR component crops with Blender's bundled OIIO."""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import numpy as np
import OpenImageIO as oiio


def confined(root: Path, raw: str, *, must_exist: bool = False) -> Path:
    candidate = Path(raw).expanduser().resolve()
    if candidate != root and root not in candidate.parents:
        raise ValueError(f"path escapes governed root: {raw}")
    if must_exist and not candidate.is_file():
        raise FileNotFoundError(candidate)
    return candidate


def write_image(path: Path, pixels: np.ndarray, pixel_type: oiio.TypeDesc) -> None:
    height, width = pixels.shape[:2]
    channels = 1 if pixels.ndim == 2 else pixels.shape[2]
    output = oiio.ImageOutput.create(str(path))
    if output is None:
        raise RuntimeError(f"OpenImageIO cannot create output: {path}")
    try:
        specification = oiio.ImageSpec(width, height, channels, pixel_type)
        if not output.open(str(path), specification):
            raise RuntimeError(output.geterror())
        if not output.write_image(pixels):
            raise RuntimeError(output.geterror())
    finally:
        output.close()


def channel_indexes(names: tuple[str, ...], suffixes: tuple[str, ...]) -> list[int]:
    result = []
    for suffix in suffixes:
        matches = [index for index, name in enumerate(names) if str(name).endswith(suffix)]
        if len(matches) != 1:
            raise RuntimeError(f"OpenEXR requires exactly one channel ending in {suffix}")
        result.append(matches[0])
    return result


def main() -> None:
    arguments = sys.argv[sys.argv.index("--") + 1 :] if "--" in sys.argv else sys.argv[1:]
    if len(arguments) != 1:
        raise ValueError("expected exactly one EXR crop manifest")
    manifest_path = Path(arguments[0]).expanduser().resolve()
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if manifest.get("schema_version") != 1:
        raise ValueError("unsupported EXR crop manifest")
    project_root = Path(manifest["project_root"]).expanduser().resolve()
    source = confined(project_root, manifest["source_path"], must_exist=True)
    output_root = manifest_path.parent
    result_path = confined(output_root, manifest["result_path"])
    requested_outputs = manifest.get("outputs")
    if not isinstance(requested_outputs, dict) or not requested_outputs:
        raise ValueError("EXR crop outputs must be a non-empty object")
    output_paths = {
        name: confined(output_root, raw)
        for name, raw in requested_outputs.items()
        if name in {"depth", "normal"}
    }
    if set(output_paths) != set(requested_outputs):
        raise ValueError("EXR crop supports only depth and normal products")
    if any(path.suffix.lower() != ".png" for path in output_paths.values()):
        raise ValueError("EXR diagnostic crops must use PNG output")

    image_input = oiio.ImageInput.open(str(source))
    if image_input is None:
        raise RuntimeError("OpenEXR source could not be opened")
    try:
        specification = image_input.spec()
        pixels = image_input.read_image(oiio.FLOAT)
        if pixels is None:
            raise RuntimeError("OpenEXR source pixels could not be decoded")
        width = int(specification.width)
        height = int(specification.height)
        names = tuple(str(value) for value in specification.channelnames)
    finally:
        image_input.close()

    box = manifest.get("crop_box_xyxy")
    if (
        not isinstance(box, list)
        or len(box) != 4
        or any(isinstance(value, bool) or not isinstance(value, int) for value in box)
    ):
        raise ValueError("EXR crop box must contain four integers")
    left, top, right, bottom = box
    if not (0 <= left < right <= width and 0 <= top < bottom <= height):
        raise ValueError("EXR crop box is outside the source image")
    crop = pixels[top:bottom, left:right]
    results: dict[str, object] = {}

    if "depth" in output_paths:
        depth_channel = channel_indexes(names, (".Depth.Z",))[0]
        depth = crop[:, :, depth_channel]
        valid = np.isfinite(depth) & (depth > 0.0) & (depth < 1e9)
        encoded = np.zeros(depth.shape, dtype=np.uint16)
        if np.any(valid):
            near = float(np.min(depth[valid]))
            far = float(np.max(depth[valid]))
            if math.isclose(near, far, rel_tol=0.0, abs_tol=1e-12):
                encoded[valid] = np.iinfo(np.uint16).max
            else:
                normalized = 1.0 - np.clip((depth - near) / (far - near), 0.0, 1.0)
                encoded[valid] = np.rint(
                    normalized[valid] * np.iinfo(np.uint16).max
                ).astype(np.uint16)
            depth_range: list[float] | None = [near, far]
        else:
            depth_range = None
        write_image(output_paths["depth"], encoded, oiio.UINT16)
        results["depth"] = {
            "channel_names": [names[depth_channel]],
            "encoding": "UINT16_NEAR_WHITE_COMPONENT_CROP",
            "valid_pixel_count": int(np.count_nonzero(valid)),
            "source_depth_range": depth_range,
        }

    if "normal" in output_paths:
        normal_channels = channel_indexes(names, (".Normal.X", ".Normal.Y", ".Normal.Z"))
        normal = crop[:, :, normal_channels]
        valid = np.isfinite(normal).all(axis=2) & (
            np.linalg.norm(normal, axis=2) > 1e-8
        )
        encoded = np.zeros(normal.shape, dtype=np.uint8)
        mapped = np.rint(np.clip(normal * 0.5 + 0.5, 0.0, 1.0) * 255.0).astype(
            np.uint8
        )
        encoded[valid] = mapped[valid]
        write_image(output_paths["normal"], encoded, oiio.UINT8)
        results["normal"] = {
            "channel_names": [names[index] for index in normal_channels],
            "encoding": "UINT8_RGB_SIGNED_NORMAL_MINUS1_PLUS1",
            "valid_pixel_count": int(np.count_nonzero(valid)),
        }

    result_path.write_text(
        json.dumps(
            {
                "schema_version": 1,
                "source_size": [width, height],
                "crop_box_xyxy": box,
                "outputs": results,
            },
            sort_keys=True,
        ),
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
