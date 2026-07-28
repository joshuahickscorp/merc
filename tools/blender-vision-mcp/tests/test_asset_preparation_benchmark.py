from __future__ import annotations

import os
from pathlib import Path

import pytest

from blender_vision.benchmarks.asset_preparation import (
    AssetPreparationBenchmarkRunner,
    load_asset_preparation_manifest,
)
from blender_vision.cli.main import build_parser


def test_asset_preparation_manifest_is_fixed_and_complete() -> None:
    manifest, manifest_path = load_asset_preparation_manifest()

    assert manifest_path.name == "manifest.json"
    assert manifest.benchmark_id == "asset-preparation-v1"
    assert set(manifest.acceptance.required_capabilities) == {
        "retopology",
        "uv_generation",
        "pbr_material_generation",
        "texture_projection_and_baking",
        "rigging",
        "object_animation",
        "character_lite_animation",
        "lod_generation",
        "collision_generation",
        "mesh_repair",
    }
    assert len(manifest.acceptance.required_source_objects) == 6


def test_asset_preparation_cli_requires_explicit_output() -> None:
    args = build_parser().parse_args(
        ["benchmark", "bootstrap-asset-preparation", "--output", "evidence"]
    )

    assert args.benchmark_command == "bootstrap-asset-preparation"
    assert args.output == "evidence"


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender asset preparation",
)
def test_real_asset_preparation_benchmark_passes_every_stage(tmp_path: Path) -> None:
    receipt = AssetPreparationBenchmarkRunner().run(tmp_path / "evidence")

    assert receipt.status == "PASS", receipt.failure
    assert receipt.functional_passed is True
    assert receipt.assertions
    assert all(item.passed for item in receipt.assertions)
    assert set(receipt.output_digests) == {
        "source_blend",
        "candidate_blend",
        "candidate_glb",
        "glb_reimport_blend",
    }
    assert receipt.runtime["network_used"] is False
