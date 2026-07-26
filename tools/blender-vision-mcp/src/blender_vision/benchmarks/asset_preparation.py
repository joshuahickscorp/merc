from __future__ import annotations

import platform
import time
from datetime import UTC, datetime
from importlib import resources
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.blender.runner import BlenderRunner
from blender_vision.core.util import (
    atomic_write_json,
    code_revision,
    sha256_file,
)
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.service import ReconstructionService


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class AssetPreparationAcceptance(_StrictModel):
    required_capabilities: list[str]
    required_source_objects: list[str]
    maximum_glb_bytes: int = Field(gt=0)
    maximum_retopology_bounds_delta: float = Field(ge=0.0)
    minimum_lod_count: int = Field(ge=1)
    minimum_collision_count: int = Field(ge=1)
    minimum_baked_texture_bytes: int = Field(ge=1)
    required_rig_bones: list[str]


class AssetPreparationManifest(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: str
    generator_operation: Literal["generate_asset_preparation_benchmark"]
    preparation_operation: Literal["prepare_asset"]
    seed: int
    rights_state: Literal["SYNTHETIC_OWNED_CC0"]
    acceptance: AssetPreparationAcceptance

    @model_validator(mode="after")
    def unique_contract_values(self) -> AssetPreparationManifest:
        acceptance = self.acceptance
        if len(acceptance.required_capabilities) != len(
            set(acceptance.required_capabilities)
        ):
            raise ValueError("required capabilities must be unique")
        if len(acceptance.required_source_objects) != len(
            set(acceptance.required_source_objects)
        ):
            raise ValueError("required source objects must be unique")
        return self


class AssetPreparationAssertion(_StrictModel):
    id: str
    passed: bool
    expected: Any
    observed: Any


class AssetPreparationReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: str
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    elapsed_seconds: float = Field(ge=0.0)
    status: Literal["PASS", "FAIL"]
    functional_passed: bool
    host: dict[str, Any]
    runtime: dict[str, Any]
    assertions: list[AssetPreparationAssertion]
    output_digests: dict[str, str]
    workspace: str
    failure: str | None = None


class AssetPreparationBenchmarkError(ValueError):
    pass


def _benchmark_root() -> Path:
    development = (
        Path(__file__).resolve().parents[3]
        / "benchmarks"
        / "100_plus"
        / "geometry"
    )
    if development.is_dir():
        return development
    installed = resources.files("blender_vision").joinpath(
        "benchmarks", "data", "100_plus", "geometry"
    )
    return Path(str(installed))


def load_asset_preparation_manifest(
    path: Path | None = None,
) -> tuple[AssetPreparationManifest, Path]:
    manifest_path = (path or (_benchmark_root() / "manifest.json")).expanduser().resolve()
    if not manifest_path.is_file():
        raise AssetPreparationBenchmarkError(
            f"asset preparation benchmark manifest is missing: {manifest_path}"
        )
    return (
        AssetPreparationManifest.model_validate_json(
            manifest_path.read_text(encoding="utf-8")
        ),
        manifest_path,
    )


def _assertion(
    identifier: str, expected: Any, observed: Any, passed: bool
) -> AssetPreparationAssertion:
    return AssetPreparationAssertion(
        id=identifier,
        passed=passed,
        expected=expected,
        observed=observed,
    )


class AssetPreparationBenchmarkRunner:
    def __init__(self, manifest_path: Path | None = None):
        self.manifest, self.manifest_path = load_asset_preparation_manifest(
            manifest_path
        )

    def run(self, output_root: Path) -> AssetPreparationReceipt:
        output_root = output_root.expanduser().resolve()
        if output_root.exists() and any(output_root.iterdir()):
            raise AssetPreparationBenchmarkError(
                f"asset preparation benchmark output must be new or empty: {output_root}"
            )
        output_root.mkdir(parents=True, exist_ok=True)
        started_at = datetime.now(UTC).isoformat()
        started = time.monotonic()
        source_head = code_revision(Path(__file__).resolve().parents[3])
        if len(source_head) != 40:
            raise AssetPreparationBenchmarkError(
                "asset preparation benchmark requires a full Git source revision"
            )
        manifest_sha256, _manifest_size = sha256_file(self.manifest_path)
        workspace = output_root / "workspace"
        assertions: list[AssetPreparationAssertion] = []
        runtime: dict[str, Any] = {}
        output_digests: dict[str, str] = {}
        failure: str | None = None
        try:
            project = ProjectStore.create(
                workspace,
                "Asset Preparation Production Benchmark",
                metadata={
                    "benchmark": self.manifest.benchmark_id,
                    "seed": self.manifest.seed,
                    "rights_state": self.manifest.rights_state,
                },
            )
            fixture_path = project.root / "scene" / "asset-preparation-source.blend"
            generated = BlenderRunner(project).run(
                self.manifest.generator_operation,
                project.root,
                {"output_path": str(fixture_path)},
                job_id="asset-preparation-fixture",
                timeout_seconds=1200,
            )
            source_scene = SceneStore(project).register_generated(
                fixture_path,
                original_name="asset-preparation-source.blend",
            )
            result = ReconstructionService(project).prepare_asset(
                scene_id=source_scene["id"],
                targets=generated["targets"],
                job_id="asset-preparation-production",
            )
            assertions = self._evaluate(generated, result)
            runtime = self._runtime_record(generated, result)
            for key, path in {
                "source_blend": fixture_path,
                "candidate_blend": Path(
                    result["generated_scene"]["absolute_path"]
                ),
                "candidate_glb": project.root / result["glb"]["relative_path"],
                "glb_reimport_blend": Path(
                    result["glb_reimport"]["scene"]["absolute_path"]
                ),
            }.items():
                output_digests[key] = sha256_file(path)[0]
        except Exception as error:  # preserved as benchmark evidence
            failure = f"{type(error).__name__}: {error}"
            atomic_write_json(
                output_root / "asset-preparation.failure.json",
                {
                    "schema_version": "1",
                    "source_git_head": source_head,
                    "manifest_sha256": manifest_sha256,
                    "failure": failure,
                    "completed_at": datetime.now(UTC).isoformat(),
                },
            )

        functional_passed = bool(assertions) and all(
            assertion.passed for assertion in assertions
        )
        receipt = AssetPreparationReceipt(
            benchmark_id=self.manifest.benchmark_id,
            source_git_head=source_head,
            manifest_sha256=manifest_sha256,
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            elapsed_seconds=round(time.monotonic() - started, 6),
            status="PASS" if functional_passed and failure is None else "FAIL",
            functional_passed=functional_passed and failure is None,
            host={
                "platform": platform.platform(),
                "python": platform.python_version(),
                "machine": platform.machine(),
            },
            runtime=runtime,
            assertions=assertions,
            output_digests=output_digests,
            workspace=str(workspace.relative_to(output_root)),
            failure=failure,
        )
        receipt_path = output_root / "asset-preparation.receipt.json"
        atomic_write_json(receipt_path, receipt.model_dump(mode="json"))
        atomic_write_json(
            output_root / "asset-preparation.receipt.sha256.json",
            {
                "schema_version": "1",
                "receipt": receipt_path.name,
                "sha256": sha256_file(receipt_path)[0],
            },
        )
        return receipt

    def _evaluate(
        self, generated: dict[str, Any], result: dict[str, Any]
    ) -> list[AssetPreparationAssertion]:
        acceptance = self.manifest.acceptance
        worker = result["worker"]
        capability_receipts = worker["capability_receipts"]
        reports = {item["source"]: item for item in worker["targets"]}
        required_capabilities = set(acceptance.required_capabilities)
        source_objects = set(acceptance.required_source_objects)
        assertions = [
            _assertion(
                "fixture_identity",
                self.manifest.benchmark_id,
                generated["benchmark_id"],
                generated["benchmark_id"] == self.manifest.benchmark_id,
            ),
            _assertion(
                "rights_state",
                self.manifest.rights_state,
                generated["rights_state"],
                generated["rights_state"] == self.manifest.rights_state,
            ),
            _assertion(
                "source_objects",
                sorted(source_objects),
                sorted(reports),
                source_objects == set(reports),
            ),
            _assertion(
                "capability_coverage",
                sorted(required_capabilities),
                sorted(capability_receipts),
                required_capabilities <= set(capability_receipts),
            ),
            _assertion(
                "candidate_blend_audit",
                True,
                result["audit"]["audit"]["valid"],
                result["audit"]["audit"]["valid"] is True,
            ),
            _assertion(
                "glb_structural_validation",
                True,
                result["glb"]["validation"]["valid"],
                result["glb"]["validation"]["valid"] is True,
            ),
            _assertion(
                "glb_reimport_audit",
                True,
                result["glb_reimport"]["audit"]["audit"]["valid"],
                result["glb_reimport"]["audit"]["audit"]["valid"] is True,
            ),
            _assertion(
                "glb_size",
                {"maximum": acceptance.maximum_glb_bytes},
                worker["glb_size"],
                worker["glb_size"] <= acceptance.maximum_glb_bytes,
            ),
        ]

        retopology_reports = [
            item["stages"]["retopology"]
            for item in reports.values()
            if "retopology" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "retopology_reduces_or_preserves_polygons",
                {
                    "non_increasing": True,
                    "maximum_bounds_delta": (
                        acceptance.maximum_retopology_bounds_delta
                    ),
                },
                [
                    {
                        "before": item["before"]["polygons"],
                        "after": item["after"]["polygons"],
                        "maximum_bounds_delta": item["maximum_world_bounds_delta"],
                    }
                    for item in retopology_reports
                ],
                bool(retopology_reports)
                and all(
                    item["after"]["polygons"] <= item["before"]["polygons"]
                    and item["maximum_world_bounds_delta"]
                    <= acceptance.maximum_retopology_bounds_delta
                    for item in retopology_reports
                ),
            )
        )
        uv_reports = [
            item["stages"]["uv_generation"]
            for item in reports.values()
            if "uv_generation" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "uv_coordinates",
                {"finite": True, "within_unit_square": True},
                [
                    {
                        "object": item["prepared"],
                        "report": item["stages"].get("uv_generation"),
                    }
                    for item in reports.values()
                    if "uv_generation" in item["stages"]
                ],
                bool(uv_reports)
                and all(
                    item["finite"] and item["within_unit_square"] and item["loop_count"] > 0
                    for item in uv_reports
                ),
            )
        )
        material_reports = [
            item["stages"]["pbr_material_generation"]
            for item in reports.values()
            if "pbr_material_generation" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "principled_pbr_materials",
                "ShaderNodeBsdfPrincipled",
                [item["node_type"] for item in material_reports],
                bool(material_reports)
                and all(
                    item["node_type"] == "ShaderNodeBsdfPrincipled"
                    and item["material_class"] != "unspecified"
                    for item in material_reports
                ),
            )
        )
        bake_reports = [
            item["stages"]["texture_baking"]
            for item in reports.values()
            if "texture_baking" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "real_texture_bake",
                {
                    "minimum_bytes": acceptance.minimum_baked_texture_bytes,
                    "packed_in_blend": True,
                },
                bake_reports,
                bool(bake_reports)
                and all(
                    item["bytes"] >= acceptance.minimum_baked_texture_bytes
                    and item["packed_in_blend"]
                    and len(item["sha256"]) == 64
                    for item in bake_reports
                ),
            )
        )
        rig_reports = [
            item["stages"]["rigging"]
            for item in reports.values()
            if "rigging" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "character_lite_rig_and_animation",
                sorted(acceptance.required_rig_bones),
                rig_reports,
                len(rig_reports) == 1
                and set(acceptance.required_rig_bones)
                <= set(rig_reports[0]["bones"])
                and all(rig_reports[0]["weighted_vertices"].values())
                and rig_reports[0]["animation"]["action"] is not None,
            )
        )
        object_animations = [
            item["stages"]["object_animation"]
            for item in reports.values()
            if "object_animation" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "object_animation",
                {"minimum_actions": 1},
                object_animations,
                bool(object_animations)
                and all(item["action"] is not None for item in object_animations),
            )
        )
        lod_reports = [
            lod
            for item in reports.values()
            for lod in item["stages"].get("lod_generation", [])
        ]
        assertions.append(
            _assertion(
                "lod_generation",
                {"minimum_count": acceptance.minimum_lod_count},
                lod_reports,
                len(lod_reports) >= acceptance.minimum_lod_count
                and all(item["polygons"] > 0 for item in lod_reports),
            )
        )
        collision_reports = [
            item["stages"]["collision_generation"]
            for item in reports.values()
            if "collision_generation" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "collision_generation",
                {"minimum_count": acceptance.minimum_collision_count},
                collision_reports,
                len(collision_reports) >= acceptance.minimum_collision_count
                and all(
                    item["object"].startswith("UCX_") and item["polygons"] > 0
                    and item["maximum_world_bounds_delta"]
                    <= acceptance.maximum_retopology_bounds_delta
                    for item in collision_reports
                ),
            )
        )
        repair_reports = [
            item["stages"]["mesh_repair"]
            for item in reports.values()
            if "mesh_repair" in item["stages"]
        ]
        assertions.append(
            _assertion(
                "damaged_mesh_repair",
                {"degenerate_before_minimum": 1, "degenerate_after": 0},
                repair_reports,
                len(repair_reports) == 1
                and repair_reports[0]["degenerate_faces_before"] >= 1
                and repair_reports[0]["degenerate_faces_after"] == 0,
            )
        )
        return assertions

    @staticmethod
    def _runtime_record(
        generated: dict[str, Any], result: dict[str, Any]
    ) -> dict[str, Any]:
        executable = Path(result["worker"]["worker"]["executable"])
        return {
            "blender_executable": str(executable),
            "blender_executable_sha256": (
                sha256_file(executable)[0] if executable.is_file() else None
            ),
            "blender_version": result["audit"]["audit"]["inventory"][
                "blender_version"
            ],
            "worker_entry_sha256": result["worker"]["worker"][
                "worker_entry_sha256"
            ],
            "worker_runtime_revision": result["worker"]["worker"][
                "runtime_revision"
            ],
            "worker_manifest_hash": result["worker"]["worker"]["manifest_hash"],
            "fixture_duration_seconds": generated["worker"]["duration_seconds"],
            "preparation_duration_seconds": result["worker"]["worker"][
                "duration_seconds"
            ],
            "candidate_audit_duration_seconds": result["audit"]["audit"]["worker"][
                "duration_seconds"
            ],
            "glb_reimport_duration_seconds": result["glb_reimport"]["worker"][
                "worker"
            ]["duration_seconds"],
            "network_used": False,
        }
