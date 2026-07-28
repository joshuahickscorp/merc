from __future__ import annotations

import math
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image
from pydantic import BaseModel, ConfigDict, Field

from blender_vision.core.util import sha256_file


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class AppearanceThresholds(_StrictModel):
    maximum_mean_absolute_channel_error: float = Field(ge=0.0)
    maximum_root_mean_square_channel_error: float = Field(ge=0.0)
    maximum_p95_absolute_channel_error: float = Field(ge=0.0)
    maximum_channel_error: int = Field(ge=0, le=255)
    maximum_alpha_mean_absolute_error: float = Field(ge=0.0)
    maximum_highlight_coverage_delta: float = Field(ge=0.0, le=1.0)
    material_parameter_tolerance: float = Field(ge=0.0)
    lighting_parameter_tolerance: float = Field(ge=0.0)
    camera_parameter_tolerance: float = Field(ge=0.0)


class AppearanceAssertion(_StrictModel):
    id: str
    passed: bool
    expected: Any
    observed: Any


class PixelComparison(_StrictModel):
    reference_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    candidate_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    width: int = Field(gt=0)
    height: int = Field(gt=0)
    mean_absolute_channel_error: float = Field(ge=0.0)
    root_mean_square_channel_error: float = Field(ge=0.0)
    p95_absolute_channel_error: float = Field(ge=0.0)
    maximum_channel_error: int = Field(ge=0, le=255)
    alpha_mean_absolute_error: float = Field(ge=0.0)
    reference_highlight_coverage: float = Field(ge=0.0, le=1.0)
    candidate_highlight_coverage: float = Field(ge=0.0, le=1.0)
    highlight_coverage_delta: float = Field(ge=0.0, le=1.0)
    passed: bool


class AppearanceViewEvaluation(_StrictModel):
    id: str
    visibility: str
    camera_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    camera_assertions: list[AppearanceAssertion]
    passes: dict[str, PixelComparison]
    passed: bool


def _assert(
    identifier: str, expected: Any, observed: Any, passed: bool
) -> AppearanceAssertion:
    return AppearanceAssertion(
        id=identifier,
        expected=expected,
        observed=observed,
        passed=passed,
    )


def _maximum_numeric_delta(left: Any, right: Any) -> float:
    if isinstance(left, dict) and isinstance(right, dict):
        if set(left) != set(right):
            return math.inf
        return max(
            (_maximum_numeric_delta(left[key], right[key]) for key in left),
            default=0.0,
        )
    if isinstance(left, list) and isinstance(right, list):
        if len(left) != len(right):
            return math.inf
        return max(
            (
                _maximum_numeric_delta(left_value, right_value)
                for left_value, right_value in zip(left, right, strict=True)
            ),
            default=0.0,
        )
    if isinstance(left, (int, float)) and isinstance(right, (int, float)):
        return abs(float(left) - float(right))
    return 0.0 if left == right else math.inf


class AppearanceAuthority:
    """Evaluate appearance without allowing camera or lighting adjustment."""

    def __init__(self, thresholds: AppearanceThresholds):
        self.thresholds = thresholds

    def compare_images(
        self, reference_path: Path, candidate_path: Path
    ) -> PixelComparison:
        reference_path = reference_path.expanduser().resolve()
        candidate_path = candidate_path.expanduser().resolve()
        with Image.open(reference_path) as source:
            reference = np.asarray(source.convert("RGBA"), dtype=np.int16)
        with Image.open(candidate_path) as source:
            candidate = np.asarray(source.convert("RGBA"), dtype=np.int16)
        if reference.shape != candidate.shape:
            raise ValueError(
                "appearance comparison requires identical image dimensions; "
                f"reference={reference.shape}, candidate={candidate.shape}"
            )
        rgb_difference = np.abs(reference[..., :3] - candidate[..., :3])
        alpha_difference = np.abs(reference[..., 3] - candidate[..., 3])
        mean_absolute = float(np.mean(rgb_difference))
        root_mean_square = float(
            np.sqrt(np.mean(np.square(rgb_difference.astype(np.float64))))
        )
        p95 = float(np.percentile(rgb_difference, 95))
        maximum = int(np.max(rgb_difference))
        alpha_mean = float(np.mean(alpha_difference))
        reference_highlights = np.max(reference[..., :3], axis=2) >= 245
        candidate_highlights = np.max(candidate[..., :3], axis=2) >= 245
        reference_highlight_coverage = float(np.mean(reference_highlights))
        candidate_highlight_coverage = float(np.mean(candidate_highlights))
        highlight_delta = abs(
            reference_highlight_coverage - candidate_highlight_coverage
        )
        thresholds = self.thresholds
        passed = (
            mean_absolute <= thresholds.maximum_mean_absolute_channel_error
            and root_mean_square
            <= thresholds.maximum_root_mean_square_channel_error
            and p95 <= thresholds.maximum_p95_absolute_channel_error
            and maximum <= thresholds.maximum_channel_error
            and alpha_mean <= thresholds.maximum_alpha_mean_absolute_error
            and highlight_delta <= thresholds.maximum_highlight_coverage_delta
        )
        return PixelComparison(
            reference_sha256=sha256_file(reference_path)[0],
            candidate_sha256=sha256_file(candidate_path)[0],
            width=int(reference.shape[1]),
            height=int(reference.shape[0]),
            mean_absolute_channel_error=mean_absolute,
            root_mean_square_channel_error=root_mean_square,
            p95_absolute_channel_error=p95,
            maximum_channel_error=maximum,
            alpha_mean_absolute_error=alpha_mean,
            reference_highlight_coverage=reference_highlight_coverage,
            candidate_highlight_coverage=candidate_highlight_coverage,
            highlight_coverage_delta=highlight_delta,
            passed=passed,
        )

    def evaluate_view(
        self,
        *,
        view_id: str,
        visibility: str,
        camera_state: dict[str, Any],
        reference_render: dict[str, Any],
        candidate_render: dict[str, Any],
        project_root: Path,
    ) -> AppearanceViewEvaluation:
        expected_matrix = camera_state["world_from_camera"]
        expected_intrinsics = camera_state["intrinsics"]
        expected_sha256 = camera_state["immutable_sha256"]
        candidate_camera = candidate_render["camera"]
        reference_camera = reference_render["camera"]
        tolerance = self.thresholds.camera_parameter_tolerance
        camera_assertions = [
            _assert(
                "camera.identity",
                expected_sha256,
                candidate_camera.get("source_camera_sha256"),
                candidate_camera.get("source_camera_sha256") == expected_sha256,
            ),
            _assert(
                "camera.framing_authority",
                "immutable_exact_camera_state",
                candidate_camera.get("framing_authority"),
                candidate_camera.get("framing_authority")
                == "immutable_exact_camera_state",
            ),
            _assert(
                "camera.world_from_camera",
                {"maximum_delta": tolerance},
                {
                    "candidate_delta": _maximum_numeric_delta(
                        expected_matrix,
                        candidate_camera.get("world_from_camera"),
                    ),
                    "reference_delta": _maximum_numeric_delta(
                        expected_matrix,
                        reference_camera.get("world_from_camera"),
                    ),
                },
                _maximum_numeric_delta(
                    expected_matrix, candidate_camera.get("world_from_camera")
                )
                <= tolerance
                and _maximum_numeric_delta(
                    expected_matrix, reference_camera.get("world_from_camera")
                )
                <= tolerance,
            ),
            _assert(
                "camera.intrinsics",
                {"maximum_delta": tolerance},
                {
                    "candidate_delta": _maximum_numeric_delta(
                        expected_intrinsics,
                        candidate_camera.get("intrinsics"),
                    ),
                    "reference_delta": _maximum_numeric_delta(
                        expected_intrinsics,
                        reference_camera.get("intrinsics"),
                    ),
                },
                _maximum_numeric_delta(
                    expected_intrinsics, candidate_camera.get("intrinsics")
                )
                <= tolerance
                and _maximum_numeric_delta(
                    expected_intrinsics, reference_camera.get("intrinsics")
                )
                <= tolerance,
            ),
            _assert(
                "camera.no_output_dither",
                0.0,
                candidate_camera.get("dither_intensity"),
                float(candidate_camera.get("dither_intensity", math.inf)) == 0.0,
            ),
            _assert(
                "lighting.fixed_scene_profile",
                reference_render.get("lighting"),
                candidate_render.get("lighting"),
                candidate_render.get("lighting") == reference_render.get("lighting"),
            ),
        ]
        reference_passes = reference_render["passes"]
        candidate_passes = candidate_render["passes"]
        common_passes = sorted(set(reference_passes) & set(candidate_passes))
        if set(reference_passes) != set(candidate_passes):
            raise ValueError("appearance render pass sets differ")
        comparisons = {
            pass_name: self.compare_images(
                project_root / reference_passes[pass_name],
                project_root / candidate_passes[pass_name],
            )
            for pass_name in common_passes
            if str(reference_passes[pass_name]).lower().endswith(".png")
        }
        passed = (
            bool(comparisons)
            and all(item.passed for item in comparisons.values())
            and all(item.passed for item in camera_assertions)
        )
        return AppearanceViewEvaluation(
            id=view_id,
            visibility=visibility,
            camera_sha256=expected_sha256,
            camera_assertions=camera_assertions,
            passes=comparisons,
            passed=passed,
        )

    def evaluate_structure(
        self,
        *,
        inventory: dict[str, Any],
        expected_materials: dict[str, dict[str, Any]],
        expected_lighting: dict[str, Any],
        required_separate_objects: list[str],
    ) -> list[AppearanceAssertion]:
        material_details = {
            item["name"]: item for item in inventory.get("material_details", [])
        }
        material_observations: dict[str, Any] = {}
        material_passed = True
        parameter_names = {
            "base_color",
            "metallic",
            "roughness",
            "ior",
            "alpha",
            "transmission",
            "emission_color",
            "emission_strength",
        }
        for name, expectation in expected_materials.items():
            observed = material_details.get(name)
            if observed is None:
                material_passed = False
                material_observations[name] = None
                continue
            expected_parameters = {
                key: value for key, value in expectation.items() if key in parameter_names
            }
            observed_parameters = {
                key: observed["principled"].get(key)
                for key in expected_parameters
            }
            maximum_delta = _maximum_numeric_delta(
                expected_parameters, observed_parameters
            )
            entry_passed = (
                observed["material_class"] == expectation["material_class"]
                and "ShaderNodeBsdfPrincipled" in observed["node_types"]
                and maximum_delta <= self.thresholds.material_parameter_tolerance
            )
            material_passed = material_passed and entry_passed
            material_observations[name] = {
                "material_class": observed["material_class"],
                "parameters": observed_parameters,
                "maximum_parameter_delta": maximum_delta,
                "structural_sha256": observed["structural_sha256"],
                "passed": entry_passed,
            }

        objects = {item["name"]: item for item in inventory.get("objects", [])}
        separate_observations = {
            name: (
                objects[name]["type"] == "MESH"
                and bool(objects[name].get("mesh", {}).get("materials"))
                if name in objects
                else False
            )
            for name in required_separate_objects
        }
        expected_lights = {
            item["name"]: item for item in expected_lighting.get("lights", [])
        }
        actual_lights = {
            item["name"]: item for item in inventory.get("light_details", [])
        }
        light_deltas = {
            name: (
                _maximum_numeric_delta(
                    {
                        key: expected[key]
                        for key in (
                            "type",
                            "world_from_light",
                            "color",
                            "energy",
                            "shape",
                            "size",
                        )
                    },
                    {
                        key: actual_lights.get(name, {}).get(key)
                        for key in (
                            "type",
                            "world_from_light",
                            "color",
                            "energy",
                            "shape",
                            "size",
                        )
                    },
                )
                if name in actual_lights
                else math.inf
            )
            for name, expected in expected_lights.items()
        }
        expected_environment = expected_lighting["environment"]
        actual_environment = inventory.get("environment", {})
        environment_delta = _maximum_numeric_delta(
            {
                "color": expected_environment["color"],
                "strength": expected_environment["strength"],
            },
            {
                "color": actual_environment.get("background_color"),
                "strength": actual_environment.get("background_strength"),
            },
        )
        return [
            _assert(
                "materials.structural_classes_and_parameters",
                {
                    "names": sorted(expected_materials),
                    "maximum_parameter_delta": (
                        self.thresholds.material_parameter_tolerance
                    ),
                },
                material_observations,
                material_passed
                and set(expected_materials).issubset(material_details),
            ),
            _assert(
                "materials.separate_editable_objects",
                {name: True for name in required_separate_objects},
                separate_observations,
                all(separate_observations.values()),
            ),
            _assert(
                "lighting.explicit_sources",
                {
                    "names": sorted(expected_lights),
                    "maximum_parameter_delta": (
                        self.thresholds.lighting_parameter_tolerance
                    ),
                },
                light_deltas,
                bool(expected_lights)
                and all(
                    delta <= self.thresholds.lighting_parameter_tolerance
                    for delta in light_deltas.values()
                ),
            ),
            _assert(
                "lighting.environment_hypothesis",
                {
                    "kind": expected_environment["kind"],
                    "hdr_supplied": expected_environment["hdr_supplied"],
                    "maximum_parameter_delta": (
                        self.thresholds.lighting_parameter_tolerance
                    ),
                },
                {
                    "background_color": actual_environment.get(
                        "background_color"
                    ),
                    "background_strength": actual_environment.get(
                        "background_strength"
                    ),
                    "environment_images": actual_environment.get(
                        "environment_images"
                    ),
                    "maximum_parameter_delta": environment_delta,
                },
                environment_delta <= self.thresholds.lighting_parameter_tolerance
                and (
                    bool(actual_environment.get("environment_images"))
                    == bool(expected_environment["hdr_supplied"])
                ),
            ),
            _assert(
                "images.no_missing_textures",
                [],
                inventory.get("missing_images", []),
                not inventory.get("missing_images"),
            ),
        ]
