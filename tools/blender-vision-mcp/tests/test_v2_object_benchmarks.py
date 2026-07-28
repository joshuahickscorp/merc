"""Tests for Phase O/P object benchmarks (sealed consumer + soft/organic/fur)."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest

from blender_vision.benchmarks.objects import (
    KNOWN_OPEN_UV_FAILURES,
    MIN_UV_PACKING,
    SYNTHETIC_FUR_CLAIM,
    BenchmarkPhase,
    RemoteConstruction,
    StageStatus,
    assert_ledger_honest,
    build_remote_mesh,
    capture_remote_fixture,
    dimensional_errors,
    image_metrics,
    orbit_cameras,
    remote_hidden_surface_ledger,
    run_consumer_object_benchmark,
    run_object_benchmarks,
    run_organic_target_benchmark,
    run_soft_organic_fur_benchmarks,
)
from blender_vision.reconstruction.mesh_ops import box_mesh
from blender_vision.v2.authority import AuthorityClass, VisibilityState

REPO = Path(__file__).resolve().parents[1]


# ------------------------------------------------------------------ construction


def test_remote_construction_dimensions_are_metric() -> None:
    c = RemoteConstruction()
    assert c.body_dimensions_mm == pytest.approx((180.0, 60.0, 25.0))
    mesh = build_remote_mesh(c)
    assert mesh.vertices.shape[0] > 8
    assert mesh.faces.shape[0] > 12
    lo, hi = mesh.vertices.min(axis=0), mesh.vertices.max(axis=0)
    # Body + buttons + hatch span.
    assert hi[2] > lo[2]
    assert (hi[0] - lo[0]) == pytest.approx(0.180, abs=0.02)


def test_orbit_cameras_never_go_under_the_object() -> None:
    cameras = orbit_cameras(24, elevation=0.35, seed=1)
    for cam in cameras:
        z = float(cam.world_from_camera[2, 3])
        assert z > 0.0, f"{cam.name} has z={z}; underside would be visible"


# ------------------------------------------------------------------ hidden ledger


def test_remote_hidden_surfaces_are_never_observed() -> None:
    ledger = remote_hidden_surface_ledger(RemoteConstruction())
    regions = {e.region for e in ledger}
    assert "underside" in regions
    assert "battery_compartment_interior" in regions
    assert "battery_hatch_outer" in regions
    for entry in ledger:
        if entry.visibility is VisibilityState.NEVER_OBSERVED:
            assert entry.observed is False
            assert entry.authority_ceiling is AuthorityClass.INFERRED
    assert assert_ledger_honest(ledger) == []


def test_ledger_rejects_observed_never_observed() -> None:
    ledger = remote_hidden_surface_ledger(RemoteConstruction())
    for entry in ledger:
        if entry.region == "underside":
            entry.observed = True
            break
    violations = assert_ledger_honest(ledger)
    assert any("underside" in v for v in violations)


# ------------------------------------------------------------------ metrics


def test_image_metrics_identity_is_high() -> None:
    img = np.zeros((64, 64, 3), dtype=np.uint8)
    img[16:48, 16:48] = (180, 40, 40)
    metrics = image_metrics(img, img, view_id="id")
    assert metrics.psnr_db > 40.0 or metrics.psnr_db == float("inf")
    assert metrics.ssim > 0.99


def test_dimensional_errors_zero_on_truth_box() -> None:
    mesh = box_mesh([-0.09, -0.03, -0.0125], [0.09, 0.03, 0.0125])
    errors = dimensional_errors(mesh, (180.0, 60.0, 25.0))
    for item in errors:
        assert abs(item.error_mm) < 0.05


# ------------------------------------------------------------------ capture + phase O


def test_capture_remote_fixture_writes_train_and_holdout(tmp_path: Path) -> None:
    result = capture_remote_fixture(
        tmp_path / "capture",
        train_views=12,
        holdout_views=4,
        image_size=96,
        seed=3,
    )
    assert result["train_view_count"] >= 8
    assert len(result["holdout_view_ids"]) >= 3
    assert Path(result["truth_obj"]).is_file()
    assert Path(result["source_packet"]).is_file()
    packet = json.loads(Path(result["source_packet"]).read_text(encoding="utf-8"))
    assert packet["authority"] == AuthorityClass.PROCEDURAL_GROUND_TRUTH.value
    assert "Not a claim about any physical remote" in packet["construction"]["claim"]
    train_pngs = list(Path(result["train_image_dir"]).glob("*.png"))
    holdout_pngs = list(Path(result["holdout_image_dir"]).glob("*.png"))
    assert train_pngs
    assert holdout_pngs
    # Holdout names must not appear in the train directory.
    train_names = {p.name for p in train_pngs}
    holdout_names = {p.name for p in holdout_pngs}
    assert train_names.isdisjoint(holdout_names)


def test_consumer_object_benchmark_scorecard(tmp_path: Path) -> None:
    card = run_consumer_object_benchmark(
        tmp_path / "remote",
        train_views=12,
        holdout_views=4,
        seed=5,
    )
    assert card.target_id == "consumer_remote"
    assert card.phase is BenchmarkPhase.CONSUMER_OBJECT
    assert card.stages.get("capture") is StageStatus.PASSED
    assert card.stages.get("hidden_surface_ledger") is StageStatus.PASSED
    assert card.hidden_surface_ledger
    never = [
        e for e in card.hidden_surface_ledger if e.visibility is VisibilityState.NEVER_OBSERVED
    ]
    assert never
    assert all(not e.observed for e in never)
    assert card.backend_scores, "expected portfolio backend scores"
    # At least one backend should execute (parametric / visual hull / depth).
    assert any(b.executed for b in card.backend_scores)
    assert card.dimensional_errors_mm
    assert card.unseen_view_metrics, "holdout image metrics required"
    assert card.next_views.get("request_count", 0) >= 1
    # Underside must be among next-view requests.
    reasons = " ".join(r.get("reason", "") for r in card.next_views.get("requests", []))
    assert "underside" in reasons.lower() or any(
        "underside" in str(r).lower() for r in card.next_views.get("requests", [])
    )
    scorecard_path = tmp_path / "remote" / "scorecard.json"
    assert scorecard_path.is_file()
    payload = json.loads(scorecard_path.read_text(encoding="utf-8"))
    assert payload["hidden_surface_counts"]["incorrectly_marked_observed"] == 0
    assert "physical remote" not in json.dumps(payload).lower() or "not a claim" in json.dumps(
        payload
    ).lower()


# ------------------------------------------------------------------ phase P


def test_organic_targets_carry_known_uv_failures(tmp_path: Path) -> None:
    card = run_organic_target_benchmark("organic_sculpture", tmp_path / "sculp", seed=1)
    assert card.phase is BenchmarkPhase.ORGANIC
    uv_failures = [f for f in card.failures if f.get("gate") == "uv_packing"]
    assert uv_failures, "known-open UV packing failure must be carried forward"
    assert uv_failures[0]["threshold"] == MIN_UV_PACKING
    assert uv_failures[0]["value"] < MIN_UV_PACKING
    assert ("organic_sculpture", "uv_packing") in KNOWN_OPEN_UV_FAILURES


def test_plant_known_open_uv_not_relaxed(tmp_path: Path) -> None:
    card = run_organic_target_benchmark("plant", tmp_path / "plant", seed=2)
    assert any(f.get("gate") == "uv_packing" for f in card.failures)
    assert all(f.get("threshold", MIN_UV_PACKING) >= MIN_UV_PACKING for f in card.failures)


def test_fur_is_labelled_synthetic_everywhere(tmp_path: Path) -> None:
    card = run_organic_target_benchmark("animal_bust", tmp_path / "bust", seed=3)
    assert card.phase is BenchmarkPhase.FUR
    assert card.synthetic_claim == SYNTHETIC_FUR_CLAIM
    assert "not evidence about any real animal" in (card.synthetic_claim or "").lower()
    payload = card.to_dict()
    blob = json.dumps(payload).lower()
    assert "synthetic" in blob
    assert "real animal" in blob
    # Must not claim a live animal observation.
    assert "observed real animal" not in blob


def test_soft_cloth_scorecard(tmp_path: Path) -> None:
    card = run_organic_target_benchmark("draped_cloth", tmp_path / "cloth", seed=4)
    assert card.phase is BenchmarkPhase.SOFT
    assert card.backend_scores
    assert card.stages.get("hidden_surface_ledger") is StageStatus.PASSED


def test_phase_p_all_targets(tmp_path: Path) -> None:
    results = run_soft_organic_fur_benchmarks(tmp_path / "organic", seed=6)
    assert set(results) == {
        "draped_cloth",
        "organic_sculpture",
        "plant",
        "animal_bust",
    }
    assert results["animal_bust"].synthetic_claim == SYNTHETIC_FUR_CLAIM


# ------------------------------------------------------------------ end-to-end harness


def test_run_object_benchmarks_framework_exit(tmp_path: Path) -> None:
    receipt = run_object_benchmarks(
        tmp_path / "all",
        train_views=10,
        holdout_views=3,
        seed=9,
    )
    assert receipt["schema"] == "visionmcp.object-benchmarks-receipt/v1"
    assert "consumer_remote" in receipt["targets"]
    assert "animal_bust" in receipt["targets"]
    assert receipt["framework_errors"] == []
    # Dense MVS blocker is explicit.
    assert "CUDA" in receipt["dense_mvs_blocker"] or "cuda" in receipt["dense_mvs_blocker"].lower()
    receipt_path = tmp_path / "all" / "object_benchmarks_receipt.json"
    assert receipt_path.is_file()


# ------------------------------------------------------------------ contracts on disk


def test_capture_protocol_and_resumption_contracts_exist() -> None:
    protocol = REPO / "benchmarks" / "remote" / "capture_protocol.md"
    assert protocol.is_file()
    text = protocol.read_text(encoding="utf-8")
    assert "underside" in text.lower()
    assert "scale" in text.lower()

    for rel in (
        "benchmarks/remote/resumption_contract.json",
        "benchmarks/soft_object/resumption_contract.json",
        "benchmarks/organic/resumption_contract.json",
        "benchmarks/fur_animal/resumption_contract.json",
    ):
        path = REPO / rel
        assert path.is_file(), rel
        data = json.loads(path.read_text(encoding="utf-8"))
        assert data["schema"].startswith("visionmcp.object-benchmark-resumption")

    fur = json.loads(
        (REPO / "benchmarks" / "fur_animal" / "resumption_contract.json").read_text(
            encoding="utf-8"
        )
    )
    assert "not evidence about any real animal" in fur["synthetic_claim"].lower()
    assert "authorization" in fur["required_to_resume_real_animal"]
