from __future__ import annotations

import copy
import platform
import time
from datetime import UTC, datetime
from importlib import resources
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.appearance import (
    AppearanceAssertion,
    AppearanceAuthority,
    AppearanceThresholds,
    AppearanceViewEvaluation,
)
from blender_vision.blender.runner import BlenderRunner
from blender_vision.core.util import atomic_write_json, code_revision, sha256_file
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.service import ReconstructionService


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class AppearanceBenchmarkManifest(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: str
    generator_operation: Literal["generate_appearance_benchmark"]
    rights_state: Literal["SYNTHETIC_OWNED_CC0"]
    required_public_views: int = Field(ge=1)
    required_holdout_views: int = Field(ge=1)
    required_passes: list[str]
    required_material_classes: dict[str, str]
    required_lights: list[str]
    required_negative_controls: list[
        Literal["camera_nudge", "material_substitution", "lighting_substitution"]
    ]
    thresholds: AppearanceThresholds

    @model_validator(mode="after")
    def unique_contract_values(self) -> AppearanceBenchmarkManifest:
        for values, label in (
            (self.required_passes, "passes"),
            (self.required_lights, "lights"),
            (self.required_negative_controls, "negative controls"),
        ):
            if len(values) != len(set(values)):
                raise ValueError(f"appearance benchmark {label} must be unique")
        return self


class AppearanceBenchmarkReceipt(_StrictModel):
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
    assertions: list[AppearanceAssertion]
    views: list[AppearanceViewEvaluation]
    structure: list[AppearanceAssertion]
    negative_controls: dict[str, bool]
    output_digests: dict[str, str]
    workspace: str
    failure: str | None = None


class AppearanceBenchmarkError(ValueError):
    pass


def _benchmark_root() -> Path:
    development = (
        Path(__file__).resolve().parents[3]
        / "benchmarks"
        / "100_plus"
        / "appearance"
    )
    if development.is_dir():
        return development
    installed = resources.files("blender_vision").joinpath(
        "benchmarks", "data", "100_plus", "appearance"
    )
    return Path(str(installed))


def load_appearance_benchmark_manifest(
    path: Path | None = None,
) -> tuple[AppearanceBenchmarkManifest, Path]:
    manifest_path = (path or (_benchmark_root() / "manifest.json")).expanduser().resolve()
    if not manifest_path.is_file():
        raise AppearanceBenchmarkError(
            f"appearance benchmark manifest is missing: {manifest_path}"
        )
    return (
        AppearanceBenchmarkManifest.model_validate_json(
            manifest_path.read_text(encoding="utf-8")
        ),
        manifest_path,
    )


def _assertion(
    identifier: str, expected: Any, observed: Any, passed: bool
) -> AppearanceAssertion:
    return AppearanceAssertion(
        id=identifier,
        expected=expected,
        observed=observed,
        passed=passed,
    )


class AppearanceBenchmarkRunner:
    def __init__(self, manifest_path: Path | None = None):
        self.manifest, self.manifest_path = load_appearance_benchmark_manifest(
            manifest_path
        )
        self.authority = AppearanceAuthority(self.manifest.thresholds)

    def run(self, output_root: Path) -> AppearanceBenchmarkReceipt:
        output_root = output_root.expanduser().resolve()
        if output_root.exists() and any(output_root.iterdir()):
            raise AppearanceBenchmarkError(
                f"appearance benchmark output must be new or empty: {output_root}"
            )
        output_root.mkdir(parents=True, exist_ok=True)
        started_at = datetime.now(UTC).isoformat()
        started = time.monotonic()
        source_head = code_revision(Path(__file__).resolve().parents[3])
        if len(source_head) != 40:
            raise AppearanceBenchmarkError(
                "appearance benchmark requires a full Git source revision"
            )
        manifest_sha256 = sha256_file(self.manifest_path)[0]
        workspace = output_root / "workspace"
        assertions: list[AppearanceAssertion] = []
        views: list[AppearanceViewEvaluation] = []
        structure: list[AppearanceAssertion] = []
        negative_controls: dict[str, bool] = {}
        output_digests: dict[str, str] = {}
        runtime: dict[str, Any] = {}
        failure: str | None = None
        try:
            project = ProjectStore.create(
                workspace,
                "Appearance Authority Benchmark",
                metadata={
                    "benchmark": self.manifest.benchmark_id,
                    "rights_state": self.manifest.rights_state,
                },
            )
            scene_path = project.root / "scene" / "appearance-authority.blend"
            generated = BlenderRunner(project).run(
                self.manifest.generator_operation,
                project.root,
                {"output_path": str(scene_path)},
                job_id="appearance-fixture",
                timeout_seconds=1200,
            )
            scene = SceneStore(project).register_generated(
                scene_path,
                original_name="appearance-authority.blend",
            )
            audit = ReconstructionService(project).audit_scene(
                scene["id"], job_id="appearance-audit"
            )
            inventory = audit["audit"]["inventory"]
            structure = self.authority.evaluate_structure(
                inventory=inventory,
                expected_materials=generated["materials"],
                expected_lighting=generated["lighting"],
                required_separate_objects=generated["required_separate_objects"],
            )
            views, render_records = self._render_views(
                project=project,
                scene_path=scene_path,
                cameras=generated["cameras"],
            )
            assertions = self._acceptance_assertions(generated, views, structure)
            negative_controls = self._negative_controls(
                project_root=project.root,
                generated=generated,
                inventory=inventory,
                render_records=render_records,
            )
            assertions.append(
                _assertion(
                    "negative_controls",
                    {
                        identifier: True
                        for identifier in self.manifest.required_negative_controls
                    },
                    negative_controls,
                    all(
                        negative_controls.get(identifier) is True
                        for identifier in self.manifest.required_negative_controls
                    ),
                )
            )
            runtime = {
                "blender_executable": generated["worker"]["executable"],
                "blender_executable_sha256": sha256_file(
                    Path(generated["worker"]["executable"])
                )[0],
                "blender_version": inventory["blender_version"],
                "worker_entry_sha256": generated["worker"][
                    "worker_entry_sha256"
                ],
                "worker_runtime_revision": generated["worker"][
                    "runtime_revision"
                ],
                "fixture_duration_seconds": generated["worker"][
                    "duration_seconds"
                ],
                "audit_duration_seconds": audit["audit"]["worker"][
                    "duration_seconds"
                ],
                "render_process_count": len(render_records) * 2,
                "network_used": False,
            }
            output_digests["scene"] = sha256_file(scene_path)[0]
            for view_id, records in render_records.items():
                for lane, record in records.items():
                    for pass_name, relative in record["passes"].items():
                        if str(relative).lower().endswith(".png"):
                            output_digests[
                                f"{view_id}.{lane}.{pass_name}"
                            ] = sha256_file(project.root / relative)[0]
        except Exception as error:  # retained as a benchmark receipt
            failure = f"{type(error).__name__}: {error}"
            atomic_write_json(
                output_root / "appearance.failure.json",
                {
                    "schema_version": "1",
                    "source_git_head": source_head,
                    "manifest_sha256": manifest_sha256,
                    "failure": failure,
                    "completed_at": datetime.now(UTC).isoformat(),
                },
            )

        functional_passed = (
            bool(assertions)
            and all(item.passed for item in assertions)
            and bool(views)
            and all(item.passed for item in views)
            and bool(structure)
            and all(item.passed for item in structure)
            and failure is None
        )
        receipt = AppearanceBenchmarkReceipt(
            benchmark_id=self.manifest.benchmark_id,
            source_git_head=source_head,
            manifest_sha256=manifest_sha256,
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            elapsed_seconds=round(time.monotonic() - started, 6),
            status="PASS" if functional_passed else "FAIL",
            functional_passed=functional_passed,
            host={
                "platform": platform.platform(),
                "python": platform.python_version(),
                "machine": platform.machine(),
            },
            runtime=runtime,
            assertions=assertions,
            views=views,
            structure=structure,
            negative_controls=negative_controls,
            output_digests=output_digests,
            workspace=str(workspace.relative_to(output_root)),
            failure=failure,
        )
        receipt_path = output_root / "appearance.receipt.json"
        atomic_write_json(receipt_path, receipt.model_dump(mode="json"))
        atomic_write_json(
            output_root / "appearance.receipt.sha256.json",
            {
                "schema_version": "1",
                "receipt": receipt_path.name,
                "sha256": sha256_file(receipt_path)[0],
            },
        )
        return receipt

    def _render_views(
        self,
        *,
        project: ProjectStore,
        scene_path: Path,
        cameras: list[dict[str, Any]],
    ) -> tuple[
        list[AppearanceViewEvaluation],
        dict[str, dict[str, dict[str, Any]]],
    ]:
        views = []
        records: dict[str, dict[str, dict[str, Any]]] = {}
        for camera in cameras:
            view_id = camera["id"]
            state = camera["camera_state"]
            records[view_id] = {}
            for lane in ("reference", "candidate"):
                output = (
                    project.root
                    / "renders"
                    / "appearance"
                    / lane
                    / f"{view_id}.png"
                )
                records[view_id][lane] = BlenderRunner(project).run(
                    "render_passes",
                    scene_path,
                    {
                        "output_path": str(output),
                        "width": state["width"],
                        "height": state["height"],
                        "camera_state": state,
                        "requested_passes": self.manifest.required_passes,
                        "governed_validation": True,
                        "evidence_passes": False,
                    },
                    job_id=f"appearance-{lane}-{view_id}",
                    timeout_seconds=600,
                )
            views.append(
                self.authority.evaluate_view(
                    view_id=view_id,
                    visibility=camera["visibility"],
                    camera_state=state,
                    reference_render=records[view_id]["reference"],
                    candidate_render=records[view_id]["candidate"],
                    project_root=project.root,
                )
            )
        return views, records

    def _acceptance_assertions(
        self,
        generated: dict[str, Any],
        views: list[AppearanceViewEvaluation],
        structure: list[AppearanceAssertion],
    ) -> list[AppearanceAssertion]:
        public = [item for item in views if item.visibility == "public"]
        holdout = [item for item in views if item.visibility == "holdout"]
        generated_classes = {
            name: record["material_class"]
            for name, record in generated["materials"].items()
        }
        generated_lights = sorted(
            item["name"] for item in generated["lighting"]["lights"]
        )
        return [
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
                "public_views",
                self.manifest.required_public_views,
                len(public),
                len(public) >= self.manifest.required_public_views
                and all(item.passed for item in public),
            ),
            _assertion(
                "heldout_views",
                self.manifest.required_holdout_views,
                len(holdout),
                len(holdout) >= self.manifest.required_holdout_views
                and all(item.passed for item in holdout),
            ),
            _assertion(
                "material_classes",
                self.manifest.required_material_classes,
                generated_classes,
                generated_classes == self.manifest.required_material_classes,
            ),
            _assertion(
                "lighting_sources",
                sorted(self.manifest.required_lights),
                generated_lights,
                generated_lights == sorted(self.manifest.required_lights),
            ),
            _assertion(
                "scene_structure",
                True,
                {item.id: item.passed for item in structure},
                bool(structure) and all(item.passed for item in structure),
            ),
        ]

    def _negative_controls(
        self,
        *,
        project_root: Path,
        generated: dict[str, Any],
        inventory: dict[str, Any],
        render_records: dict[str, dict[str, dict[str, Any]]],
    ) -> dict[str, bool]:
        camera = generated["cameras"][0]
        reference = render_records[camera["id"]]["reference"]
        nudged = copy.deepcopy(render_records[camera["id"]]["candidate"])
        nudged["camera"]["world_from_camera"][0][3] += 1.0
        camera_rejected = not self.authority.evaluate_view(
            view_id="negative-camera-nudge",
            visibility="negative_control",
            camera_state=camera["camera_state"],
            reference_render=reference,
            candidate_render=nudged,
            project_root=project_root,
        ).passed

        substituted_material = copy.deepcopy(inventory)
        target_material = next(
            item
            for item in substituted_material["material_details"]
            if item["name"] == "Appearance_FrostedGlass"
        )
        target_material["material_class"] = "transparent"
        material_rejected = not all(
            item.passed
            for item in self.authority.evaluate_structure(
                inventory=substituted_material,
                expected_materials=generated["materials"],
                expected_lighting=generated["lighting"],
                required_separate_objects=generated[
                    "required_separate_objects"
                ],
            )
        )

        substituted_lighting = copy.deepcopy(inventory)
        substituted_lighting["light_details"][0]["energy"] += 25.0
        lighting_rejected = not all(
            item.passed
            for item in self.authority.evaluate_structure(
                inventory=substituted_lighting,
                expected_materials=generated["materials"],
                expected_lighting=generated["lighting"],
                required_separate_objects=generated[
                    "required_separate_objects"
                ],
            )
        )
        return {
            "camera_nudge": camera_rejected,
            "material_substitution": material_rejected,
            "lighting_substitution": lighting_rejected,
        }
