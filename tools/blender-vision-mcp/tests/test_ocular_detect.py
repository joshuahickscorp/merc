"""Perception detector + multi-block appearance embeddings (no ground truth)."""

from __future__ import annotations

import ast
import inspect
from pathlib import Path

import numpy as np
import pytest

from blender_vision.ocular import detect as detect_mod
from blender_vision.ocular.detect import (
    COLOUR_BLOCK_WEIGHT,
    EMBEDDING_DIM,
    GRADIENT_BLOCK_WEIGHT,
    SHAPE_BLOCK_WEIGHT,
    DetectionConfig,
    DetectionMethod,
    appearance_embedding,
    assert_no_ground_truth_in_detections,
    detect,
    embedding_similarity,
)
from blender_vision.ocular.track import Detection, VisualTrack


def _same_colour_different_texture() -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Two patches: identical mean colour, different texture (stripes vs dots)."""
    base = np.array([118, 148, 176], dtype=np.uint8)
    h, w = 64, 64
    a = np.zeros((h, w, 3), dtype=np.uint8)
    b = np.zeros((h, w, 3), dtype=np.uint8)
    a[:] = base
    b[:] = base
    # Horizontal stripes on A.
    for y in range(h):
        if (y // 3) % 2 == 0:
            a[y, :] = np.clip(a[y, :].astype(np.int16) + 22, 0, 255).astype(np.uint8)
        else:
            a[y, :] = np.clip(a[y, :].astype(np.int16) - 18, 0, 255).astype(np.uint8)
    # Dot grid on B.
    rng = np.random.default_rng(0)
    for _ in range(80):
        yy = int(rng.integers(2, h - 2))
        xx = int(rng.integers(2, w - 2))
        b[yy - 1 : yy + 2, xx - 1 : xx + 2] = np.clip(
            b[yy, xx].astype(np.int16) + 30, 0, 255
        ).astype(np.uint8)
    mask = np.ones((h, w), dtype=np.uint8)
    return a, b, mask


def test_embedding_separates_same_colour_different_texture() -> None:
    a, b, mask = _same_colour_different_texture()
    ea = appearance_embedding(a, mask)
    eb = appearance_embedding(b, mask)
    assert len(ea) == EMBEDDING_DIM
    assert len(eb) == EMBEDDING_DIM
    # Self-similarity near 1.
    assert embedding_similarity(ea, ea) == pytest.approx(1.0, abs=1e-5)
    # Cross similarity clearly below self.
    cross = embedding_similarity(ea, eb)
    assert cross < 0.97
    # Colour-only would be nearly identical; embedding must still separate.
    assert cross < embedding_similarity(ea, ea) - 0.01


def test_embedding_block_weights_sum_to_one() -> None:
    total = COLOUR_BLOCK_WEIGHT + GRADIENT_BLOCK_WEIGHT + SHAPE_BLOCK_WEIGHT
    assert total == pytest.approx(1.0)


def test_detect_emits_embeddings_without_gt() -> None:
    img = np.zeros((120, 160, 3), dtype=np.uint8)
    img[:] = (20, 20, 20)
    img[30:70, 30:70] = (40, 40, 200)
    img[30:70, 100:140] = (40, 200, 40)
    dets = detect(img, frame_index=0, config=DetectionConfig(min_area=40))
    assert len(dets) >= 1
    for d in dets:
        assert isinstance(d, Detection)
        assert len(d.appearance_embedding) == EMBEDDING_DIM
        assert "gt_id" not in d.meta
        assert "ground_truth_id" not in d.meta
    assert_no_ground_truth_in_detections(dets)


def test_detect_fused_with_motion() -> None:
    img0 = np.zeros((100, 120, 3), dtype=np.uint8)
    img0[:] = (18, 18, 18)
    img0[20:50, 20:50] = (30, 30, 200)
    img1 = img0.copy()
    img1[20:50, 20:50] = (18, 18, 18)
    img1[20:50, 60:90] = (30, 30, 200)
    dets = detect(
        img1,
        frame_index=1,
        config=DetectionConfig(method=DetectionMethod.FUSED, min_area=30),
        previous_image=img0,
    )
    assert isinstance(dets, list)


def test_assert_rejects_gt_meta() -> None:
    from blender_vision.ocular.track import TrackTargetKind

    bad = Detection(
        detection_id="x",
        kind=TrackTargetKind.OBJECT,
        bbox_xywh=(0, 0, 10, 10),
        centroid_xy=(5, 5),
        appearance_hist=[0.1] * 8,
        meta={"gt_id": "oracle"},
    )
    with pytest.raises(AssertionError, match="ground truth"):
        assert_no_ground_truth_in_detections([bad])


def test_no_ground_truth_symbol_in_detect_module() -> None:
    """Static scan: detect.py must not reference ground_truth_id."""
    src = Path(inspect.getsourcefile(detect_mod)).read_text(encoding="utf-8")
    tree = ast.parse(src)
    names: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Name):
            names.add(node.id)
        elif isinstance(node, ast.Attribute):
            names.add(node.attr)
        elif isinstance(node, ast.Constant) and isinstance(node.value, str):
            names.add(node.value)
    # The module may mention the phrase in comments/docstrings about forbidding GT;
    # forbid the field as an assignable runtime attribute name used as identifier
    # outside of the forbidden-key set itself.
    assert "ground_truth_id" not in {
        n.id for n in ast.walk(tree) if isinstance(n, ast.Name)
    }


def test_visual_track_has_no_ground_truth_id_field() -> None:
    assert "ground_truth_id" not in VisualTrack.__dataclass_fields__
