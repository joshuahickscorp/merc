"""Phase S — sealed ocular benchmarks and leakage canaries."""

from __future__ import annotations

from pathlib import Path

import pytest

from blender_vision.core.errors import SecurityError, ValidationError
from blender_vision.ocular.sealed import (
    TARGET_IDS,
    SplitAuthority,
    builder_read,
    default_manifests,
    make_split,
    materialise_all,
    run_leakage_canaries,
    run_sealed_ocular,
)


def test_eight_targets() -> None:
    assert len(TARGET_IDS) == 8
    assert "live_tabletop" in TARGET_IDS
    assert "remote" in TARGET_IDS
    assert "reflective_transparent" in TARGET_IDS
    assert "browser_page" in TARGET_IDS
    assert "data_centre" in TARGET_IDS
    assert "dynamic_room_memory" in TARGET_IDS
    assert "soft_object" in TARGET_IDS
    assert "organic_fur" in TARGET_IDS


def test_single_split_authority_no_overlap() -> None:
    split = make_split("remote", total_views=32, hidden_count=8, seed=7)
    assert split.sealed is True
    assert split.digest
    train = set(split.train_indices)
    hidden = set(split.hidden_indices)
    assert not (train & hidden)
    assert train | hidden == set(range(32))


def test_split_never_recomputed_after_seal() -> None:
    split = make_split("remote", total_views=32, hidden_count=8, seed=7)
    # Loading from dict re-verifies digest; mutation fails.
    payload = split.to_dict()
    payload["hidden_indices"] = list(payload["hidden_indices"]) + [0]
    # Overlap with train may or may not; digest will fail either way if sealed.
    with pytest.raises((SecurityError, ValidationError)):
        # Force sealed true with wrong digest
        payload["sealed"] = True
        SplitAuthority.from_dict(payload)


def test_builder_denied_hidden(tmp_path: Path) -> None:
    contracts = materialise_all(tmp_path, seed=1)
    assert set(contracts) == set(TARGET_IDS)
    target = tmp_path / "remote"
    with pytest.raises(SecurityError):
        builder_read(target, "oracle/hidden/canary.txt")


def test_canary_absent_from_builder(tmp_path: Path) -> None:
    materialise_all(tmp_path, seed=2)
    matrix = run_leakage_canaries(tmp_path)
    canary_rows = [r for r in matrix if r["probe"].endswith("canary_absent_from_builder")]
    assert canary_rows
    assert all(r["result"] == "PASS" for r in canary_rows)


def test_run_sealed_ocular_pass(tmp_path: Path) -> None:
    receipt = run_sealed_ocular(tmp_path, seed=3)
    assert receipt["status"] == "PASS"
    assert receipt["target_count"] == 8
    assert (tmp_path / "sealed.receipt.json").is_file()
    for tid in TARGET_IDS:
        assert (tmp_path / "benchmarks" / tid / "contract.json").is_file()
        assert (tmp_path / "benchmarks" / tid / "manifest.json").is_file()


def test_manifests_cover_all_targets() -> None:
    manifests = default_manifests()
    assert set(manifests) == set(TARGET_IDS)
    for manifest in manifests.values():
        assert manifest.split is not None
        assert manifest.split.sealed
