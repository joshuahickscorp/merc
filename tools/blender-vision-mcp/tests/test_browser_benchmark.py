from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from blender_vision.perception.browser_benchmark import (
    BrowserBenchmarkError,
    BrowserBenchmarkRunner,
    load_browser_benchmark_manifest,
)


def test_fixed_browser_manifest_binds_fixture_and_acceptance() -> None:
    manifest, manifest_path, fixture_root = load_browser_benchmark_manifest()

    assert manifest_path.name == "manifest.json"
    assert fixture_root.name == "fixture"
    assert [engine.id for engine in manifest.engines] == [
        "chromium",
        "firefox",
        "webkit",
    ]
    assert manifest.acceptance.minimum_passed_engines == 2
    assert manifest.acceptance.minimum_additional_engines == 1
    assert {profile.id for profile in manifest.profiles} == {
        "mobile-portrait-dark-touch-reduced",
        "mobile-landscape-touch",
        "offline-loaded-application",
        "slow-network",
    }


def test_browser_manifest_rejects_fixture_tampering(tmp_path: Path) -> None:
    manifest, manifest_path, fixture_root = load_browser_benchmark_manifest()
    copied_root = tmp_path / "browser"
    copied_fixture = copied_root / "fixture"
    copied_fixture.mkdir(parents=True)
    for source in fixture_root.iterdir():
        (copied_fixture / source.name).write_bytes(source.read_bytes())
    (copied_fixture / "status.json").write_text('{"status":"tampered"}', encoding="utf-8")
    copied_manifest = copied_root / "manifest.json"
    copied_manifest.write_text(
        json.dumps(manifest.model_dump(mode="json"), indent=2),
        encoding="utf-8",
    )

    with pytest.raises(BrowserBenchmarkError, match="digest mismatch"):
        load_browser_benchmark_manifest(copied_manifest)


def test_browser_runtime_failure_classifier_is_bounded() -> None:
    classify = BrowserBenchmarkRunner._is_external_runtime_failure

    assert classify(RuntimeError("BrowserType.launch: Timeout 15000ms exceeded."))
    assert classify(RuntimeError("browser executable is unavailable"))
    assert not classify(RuntimeError("accessibility assertion failed"))


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BROWSER_BENCHMARK") != "1",
    reason="set BVMCP_RUN_BROWSER_BENCHMARK=1 to run the governed browser matrix",
)
def test_real_browser_benchmark_preserves_passes_and_exact_blockers(
    tmp_path: Path,
) -> None:
    output = tmp_path / "browser-benchmark"
    receipt = BrowserBenchmarkRunner().run(output)
    statuses = {result.id: result.status for result in receipt.engines}

    assert receipt.functional_passed is True
    assert statuses["chromium"] == "PASS"
    assert statuses["webkit"] == "PASS"
    assert statuses["firefox"] in {"PASS", "BLOCKED_EXTERNAL"}
    assert all(profile.status == "PASS" for profile in receipt.profiles)
    if statuses["firefox"] == "BLOCKED_EXTERNAL":
        assert "firefox" in receipt.external_blockers
    assert (output / "browser-benchmark.receipt.json").is_file()
    assert (output / "browser-benchmark.receipt.sha256.json").is_file()
