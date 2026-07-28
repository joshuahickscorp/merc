from __future__ import annotations

import os
import shutil
from pathlib import Path

import pytest

from blender_vision.benchmarks.performance import (
    PerformanceBenchmarkRunner,
    load_performance_benchmark_manifest,
)
from blender_vision.cli.main import build_parser
from blender_vision.performance import (
    BoundedPerformanceRepair,
    PerformanceAuthority,
    PerformanceMeasurement,
)


def _passing_measurement() -> PerformanceMeasurement:
    return PerformanceMeasurement(
        variant="repaired",
        browser_engine="chromium",
        browser_version="test",
        browser_executable="/test/chrome",
        browser_executable_sha256="0" * 64,
        initial_transfer_bytes=10,
        javascript_execution_ms=1,
        cdp_script_duration_ms=1,
        selected_glb="scene-low.glb",
        selected_glb_bytes=10,
        texture_memory_bytes=16,
        shader_compilation_ms=1,
        shader_source_count=2,
        draw_call_count=1,
        frame_sample_count=3,
        frame_p95_ms=16,
        dropped_frame_ratio=0,
        long_task_count=0,
        long_task_total_ms=0,
        cumulative_layout_shift=0,
        interaction_samples_ms=[10, 11, 12],
        interaction_p95_ms=12,
        javascript_heap_growth_bytes=0,
        retained_allocation_bytes=0,
        api_samples_ms=[2, 3, 4],
        api_p95_ms=4,
        database_query_samples_ms=[0.1, 0.2, 0.3],
        database_query_p95_ms=0.3,
        database_query_plan=["SEARCH items USING INDEX items_slug"],
        database_uses_index=True,
        initial_resource_paths=["/index.html", "/app.js", "/styles.css"],
        intent_resource_paths=["/scene-low.glb"],
        eager_3d_asset_on_initial_load=False,
        lazy_3d_asset_after_intent=True,
        lod_level="LOW",
        adaptive_dpr=True,
        effective_dpr=2,
        reduced_motion_honored=True,
        no_webgl_fallback=True,
        webgl_observed=True,
        screenshot_sha256="1" * 64,
        aria_sha256="2" * 64,
        behavior_sha256="3" * 64,
        api_contract_sha256="4" * 64,
        selected_glb_sha256="5" * 64,
        selected_glb_valid=True,
        selected_glb_node_names=["GovernedTriangle"],
        selected_glb_mesh_names=["GovernedTriangleMesh"],
        console_errors=[],
    )


def test_performance_manifest_is_fixed_and_complete() -> None:
    manifest, path, fixture = load_performance_benchmark_manifest()

    assert path.name == "manifest.json"
    assert fixture.name == "fixture"
    assert manifest.benchmark_id == "performance-repair-authority-v1"
    assert len(manifest.repair_rules) == 7
    assert set(manifest.required_negative_controls) == {
        "transfer_budget",
        "lazy_load",
        "glb_structure",
        "preservation_gate",
    }


def test_bounded_repair_is_digest_bound_and_changes_only_app_js(
    tmp_path: Path,
) -> None:
    manifest, _path, fixture = load_performance_benchmark_manifest()
    target = tmp_path / "fixture"
    shutil.copytree(fixture, target)
    repair = BoundedPerformanceRepair(
        relative_path=manifest.repair_path,
        expected_before_sha256=manifest.repair_preimage_sha256,
        replacements=[(item.before, item.after) for item in manifest.repair_rules],
    )

    receipt = repair.apply(target)

    assert receipt.changed_paths == ["app.js"]
    assert receipt.replacement_count == 7
    assert receipt.before_sha256 != receipt.after_sha256
    with pytest.raises(ValueError, match="preimage digest mismatch"):
        repair.apply(target)


def test_authority_rejects_eager_asset_and_budget_overrun() -> None:
    manifest, _path, _fixture = load_performance_benchmark_manifest()
    authority = PerformanceAuthority(manifest.budget)
    passing = _passing_measurement()
    assert all(item.passed for item in authority.evaluate(passing))
    degraded = passing.model_copy(
        update={
            "initial_transfer_bytes": manifest.budget.initial_transfer_bytes + 1,
            "eager_3d_asset_on_initial_load": True,
            "lazy_3d_asset_after_intent": False,
            "lod_level": "HIGH",
        }
    )

    failed = {item.id for item in authority.evaluate(degraded) if not item.passed}

    assert {
        "initial_transfer_bytes",
        "initial_3d_asset_is_lazy",
        "intent_loads_3d_asset",
        "adaptive_low_lod",
    } <= failed


def test_performance_cli_requires_explicit_output() -> None:
    args = build_parser().parse_args(
        ["benchmark", "bootstrap-performance", "--output", "evidence"]
    )

    assert args.benchmark_command == "bootstrap-performance"
    assert args.output == "evidence"


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_BROWSER_TESTS=1 for real Chromium performance repair",
)
def test_real_performance_benchmark_rejects_then_repairs(tmp_path: Path) -> None:
    receipt = PerformanceBenchmarkRunner().run(tmp_path / "evidence")

    assert receipt.status == "PASS", receipt.failure
    assert receipt.degraded_rejected is True
    assert receipt.repaired_accepted is True
    assert receipt.preservation is not None and receipt.preservation.passed
    assert receipt.repair is not None and receipt.repair.replacement_count == 7
    assert all(receipt.negative_controls.values())
