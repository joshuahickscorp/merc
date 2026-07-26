from __future__ import annotations

import os
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.appearance import AppearanceAuthority, AppearanceThresholds
from blender_vision.benchmarks.appearance import (
    AppearanceBenchmarkRunner,
    load_appearance_benchmark_manifest,
)
from blender_vision.cli.main import build_parser


def _thresholds() -> AppearanceThresholds:
    return AppearanceThresholds(
        maximum_mean_absolute_channel_error=0.25,
        maximum_root_mean_square_channel_error=0.75,
        maximum_p95_absolute_channel_error=1.0,
        maximum_channel_error=3,
        maximum_alpha_mean_absolute_error=0.25,
        maximum_highlight_coverage_delta=0.002,
        material_parameter_tolerance=1e-5,
        lighting_parameter_tolerance=1e-5,
        camera_parameter_tolerance=1e-7,
    )


def test_pixel_comparison_enforces_fixed_thresholds(tmp_path: Path) -> None:
    reference = tmp_path / "reference.png"
    matching = tmp_path / "matching.png"
    changed = tmp_path / "changed.png"
    Image.new("RGBA", (16, 16), (24, 48, 96, 255)).save(reference)
    Image.new("RGBA", (16, 16), (24, 48, 96, 255)).save(matching)
    Image.new("RGBA", (16, 16), (48, 48, 96, 255)).save(changed)
    authority = AppearanceAuthority(_thresholds())

    assert authority.compare_images(reference, matching).passed is True
    rejected = authority.compare_images(reference, changed)
    assert rejected.passed is False
    assert rejected.maximum_channel_error == 24


def test_appearance_manifest_fixes_material_light_and_negative_controls() -> None:
    manifest, path = load_appearance_benchmark_manifest()

    assert path.name == "manifest.json"
    assert manifest.required_public_views == 3
    assert manifest.required_holdout_views == 1
    assert manifest.required_material_classes["Appearance_FrostedGlass"] == "translucent"
    assert set(manifest.required_negative_controls) == {
        "camera_nudge",
        "material_substitution",
        "lighting_substitution",
    }


def test_appearance_cli_requires_new_output() -> None:
    args = build_parser().parse_args(
        ["benchmark", "bootstrap-appearance", "--output", "evidence"]
    )

    assert args.benchmark_command == "bootstrap-appearance"
    assert args.output == "evidence"


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender appearance authority",
)
def test_real_appearance_benchmark_passes_and_rejects_controls(
    tmp_path: Path,
) -> None:
    receipt = AppearanceBenchmarkRunner().run(tmp_path / "evidence")

    assert receipt.status == "PASS", receipt.failure
    assert receipt.functional_passed is True
    assert len(receipt.views) == 4
    assert all(view.passed for view in receipt.views)
    assert all(item.passed for item in receipt.structure)
    assert all(receipt.negative_controls.values())
