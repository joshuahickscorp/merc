"""Phase K — ocular remote loop tests."""

from __future__ import annotations

from pathlib import Path

import cv2
import numpy as np

from blender_vision.ocular.attestation import ExecutionClass
from blender_vision.ocular.remote_loop import (
    FIXTURE_CLAIM,
    ViewReport,
    capture_protocol_text,
    default_hidden_surface_ledger,
    evaluate_representations,
    process_view,
    run_remote_loop,
)
from blender_vision.ocular.track import TrackerState
from blender_vision.v2.authority import VisibilityState


def _write_train_images(directory: Path, n: int = 4) -> Path:
    directory.mkdir(parents=True, exist_ok=True)
    for i in range(n):
        img = np.zeros((96, 128, 3), dtype=np.uint8)
        img[:] = (30, 30, 30)
        # Moving coloured body so tracking has signal.
        x0 = 20 + i * 8
        img[30:70, x0 : x0 + 40] = (40, 40, 180)
        img[40:55, x0 + 10 : x0 + 20] = (40, 180, 40)
        cv2.imwrite(str(directory / f"view_{i:03d}.png"), img)
    return directory


def test_process_view_answers_observed_inferred_next(tmp_path: Path) -> None:
    train = _write_train_images(tmp_path / "train", n=3)
    paths = sorted(train.glob("*.png"))
    tracker = TrackerState()
    prev = None
    prev_seg = None
    reports: list[ViewReport] = []
    for fi, path in enumerate(paths):
        image = cv2.imread(str(path))
        assert image is not None
        report, tracker, prev_seg, _labels = process_view(
            image,
            view_id=path.stem,
            frame_index=fi,
            image_digest="abc",
            tracker=tracker,
            previous_image=prev,
            previous_seg=prev_seg,
        )
        reports.append(report)
        prev = image
        assert isinstance(report.observed, list)
        assert isinstance(report.inferred, list)
        assert report.next_view, "every view must propose next capture"
        assert "underside" in {n["request"] for n in report.next_view}
    assert len(reports) == 3
    assert any(r.segment_count >= 1 for r in reports)


def test_run_remote_loop_no_gt_and_claim_boundary(tmp_path: Path) -> None:
    train = _write_train_images(tmp_path / "train", n=4)
    out = tmp_path / "out"
    receipt = run_remote_loop(out, train_dir=train, max_views=4)
    assert receipt.train_image_count == 4
    assert len(receipt.views) == 4
    assert FIXTURE_CLAIM in receipt.claim
    assert "user" not in receipt.claim.lower() or "not" in receipt.claim.lower()
    assert receipt.world_summary.get("track_source") == "perception_derived"
    # No GT seeding: every observation is from segments.
    for view in receipt.views:
        for row in view.observed:
            assert row["authority"] in {"SENSOR_DERIVED", "sensor_derived", row["authority"]}
            assert "ground_truth" not in str(row).lower()
    assert (out / "remote_loop.receipt.json").is_file()
    assert (out / "per_view_reports.json").is_file()
    # Radiance blocked in portfolio.
    backends = [c["backend"] for c in receipt.geometry_portfolio["candidates"]]
    assert "gaussian_radiance" in backends
    rad = next(
        c
        for c in receipt.geometry_portfolio["candidates"]
        if c["backend"] == "gaussian_radiance"
    )
    assert rad["executed"] is False
    assert rad["execution_class"] == ExecutionClass.BLOCKED.value


def test_hidden_surface_ledger_never_observed() -> None:
    ledger = default_hidden_surface_ledger()
    never = [e for e in ledger if e.visibility is VisibilityState.NEVER_OBSERVED]
    assert len(never) >= 2
    for entry in never:
        assert entry.to_dict()["observed"] is False


def test_evaluate_representations_radiance_not_forced() -> None:
    candidates = [
        {"backend": "mesh_visual_hull_box", "executed": True},
        {"backend": "point_cloud", "executed": True},
        {"backend": "gaussian_radiance", "executed": False, "reason": "no weights"},
    ]
    result = evaluate_representations(candidates)
    assert result["radiance_blocked"] is True
    photo = result["photoreal_view_synthesis"]
    assert any(r["backend"] == "gaussian_radiance" and not r["suitable"] for r in photo)


def test_capture_protocol_mentions_user_remote() -> None:
    text = capture_protocol_text()
    assert "rights-cleared" in text.lower() or "you own" in text.lower()
    assert "holdout" in text.lower()
    assert "scale" in text.lower()


def test_missing_train_dir_blocks(tmp_path: Path) -> None:
    receipt = run_remote_loop(tmp_path / "out", train_dir=tmp_path / "missing")
    assert receipt.execution_class == ExecutionClass.BLOCKED.value
    assert receipt.blockers
