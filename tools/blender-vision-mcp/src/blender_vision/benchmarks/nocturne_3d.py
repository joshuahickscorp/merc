from __future__ import annotations

import json
import math
import platform
import subprocess
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal

import numpy as np
from PIL import Image
from pydantic import BaseModel, ConfigDict, Field

from blender_vision.benchmarks.nocturne import (
    NocturnePacketAuthority,
    SealedBuilderReceipt,
    load_nocturne_contract,
    nocturne_benchmark_root,
)
from blender_vision.core.config import discover_blender
from blender_vision.core.errors import SecurityError
from blender_vision.core.util import atomic_write_json, code_revision, sha256_file
from blender_vision.geometry.gltf_validator import GlbValidator
from blender_vision.security.adversarial import SealedBenchmarkBoundary


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class Nocturne3DAssertion(_StrictModel):
    id: str
    expected: Any
    observed: Any
    passed: bool
    severity: Literal["P0", "P1", "P2"]


class NocturneViewScore(_StrictModel):
    label: str
    visibility: Literal["public", "hidden"]
    oracle_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    candidate_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    silhouette_iou: float = Field(ge=0, le=1)
    threshold: float = Field(ge=0, le=1)
    passed: bool


class Nocturne3DReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-sealed-v1"]
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    contract_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    packet_manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    sealed_builder_receipt_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    oracle_manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    candidate_blend_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    candidate_hero_glb_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    candidate_low_glb_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    elapsed_seconds: float = Field(ge=0)
    status: Literal["PASS", "FAIL"]
    functional_passed: bool
    assertions: list[Nocturne3DAssertion]
    view_scores: list[NocturneViewScore]
    inspection: dict[str, Any] | None
    hero_glb_validation: dict[str, Any] | None
    low_glb_validation: dict[str, Any] | None
    leakage_boundary: dict[str, Any] | None
    output_digests: dict[str, str]
    runtime: dict[str, Any]
    claim_boundary: list[str]
    workspace: str
    failure: str | None = None


class Nocturne3DEvaluationError(ValueError):
    pass


def _assertion(
    identifier: str,
    expected: Any,
    observed: Any,
    passed: bool,
    severity: Literal["P0", "P1", "P2"] = "P1",
) -> Nocturne3DAssertion:
    return Nocturne3DAssertion(
        id=identifier,
        expected=expected,
        observed=observed,
        passed=passed,
        severity=severity,
    )


def _alpha_mask(path: Path) -> np.ndarray:
    image = Image.open(path).convert("RGBA")
    alpha = np.asarray(image, dtype=np.uint8)[:, :, 3]
    return alpha >= 12


def _silhouette_iou(reference: Path, candidate: Path) -> float:
    first = _alpha_mask(reference)
    second = _alpha_mask(candidate)
    if first.shape != second.shape:
        raise ValueError(
            f"silhouette dimensions differ: {first.shape} != {second.shape}"
        )
    union = np.logical_or(first, second)
    if not bool(union.any()):
        return 1.0
    return float(np.logical_and(first, second).sum() / union.sum())


def _material_class_checks(details: dict[str, Any]) -> dict[str, bool]:
    def item(name: str) -> dict[str, Any]:
        return details.get(name, {})

    shell = item("MAT_BLACK_ANODIZED_ALUMINUM")
    glass = item("MAT_FROSTED_TRANSLUCENT_GLASS")
    emissive = item("MAT_WARM_EMISSIVE_CERAMIC")
    membrane = item("MAT_GRAPHITE_TENSIONED_TEXTILE")
    control = item("MAT_MACHINED_ALUMINUM")
    return {
        "black_anodized_is_metallic": float(shell.get("Metallic", 0)) >= 0.65,
        "black_anodized_is_not_mirror": 0.12
        <= float(shell.get("Roughness", 1))
        <= 0.48,
        "glass_is_transmissive": float(glass.get("Transmission Weight", 0)) >= 0.45,
        "glass_is_frosted": float(glass.get("Roughness", 0)) >= 0.16,
        "eclipse_is_emissive": float(emissive.get("Emission Strength", 0)) >= 2.0,
        "membrane_is_high_roughness": float(membrane.get("Roughness", 0)) >= 0.7,
        "rotary_control_is_metallic": float(control.get("Metallic", 0)) >= 0.7,
        "glass_and_emissive_are_separate": bool(glass) and bool(emissive),
    }


class Nocturne3DEvaluator:
    def __init__(self, contract_path: Path | None = None):
        self.contract, self.contract_path = load_nocturne_contract(contract_path)

    def run(
        self,
        *,
        packet_root: Path,
        sealed_oracle_root: Path,
        candidate_root: Path,
        sealed_builder_receipt_path: Path,
        output_root: Path,
    ) -> Nocturne3DReceipt:
        packet = packet_root.expanduser().resolve()
        sealed = sealed_oracle_root.expanduser().resolve()
        candidate = candidate_root.expanduser().resolve()
        output = output_root.expanduser().resolve()
        if output.exists() and any(output.iterdir()):
            raise Nocturne3DEvaluationError("3D evaluator output must be new or empty")
        output.mkdir(parents=True, exist_ok=True)
        packet_receipt = NocturnePacketAuthority(self.contract_path).verify(packet)
        builder_receipt_path = sealed_builder_receipt_path.expanduser().resolve()
        builder_receipt = SealedBuilderReceipt.model_validate_json(
            builder_receipt_path.read_text(encoding="utf-8")
        )
        if builder_receipt.status != "PASS":
            raise SecurityError("3D evaluation requires a passing sealed builder receipt")
        contract_digest = sha256_file(self.contract_path)[0]
        if builder_receipt.contract_sha256 != contract_digest:
            raise SecurityError("sealed builder receipt is bound to another contract")
        oracle_manifest_path = sealed / "oracle.manifest.json"
        oracle_manifest = json.loads(
            oracle_manifest_path.read_text(encoding="utf-8")
        )
        canary = (sealed / "ORACLE_CANARY.txt").read_text(encoding="utf-8")
        blend = candidate / "3d" / "nocturne-one.blend"
        hero_glb = candidate / "public" / "assets" / "nocturne-one-hero.glb"
        low_glb = candidate / "public" / "assets" / "nocturne-one-low.glb"
        for path in (blend, hero_glb, low_glb):
            if path.is_symlink() or not path.is_file():
                raise Nocturne3DEvaluationError(
                    f"required candidate 3D artifact is missing or linked: {path}"
                )
        source_head = code_revision(Path(__file__).resolve().parents[3])
        if len(source_head) != 40:
            raise Nocturne3DEvaluationError(
                "3D evaluation requires a full Git source revision"
            )
        started_at = datetime.now(UTC).isoformat()
        started = time.monotonic()
        assertions: list[Nocturne3DAssertion] = []
        view_scores: list[NocturneViewScore] = []
        inspection: dict[str, Any] | None = None
        hero_validation: dict[str, Any] | None = None
        low_validation: dict[str, Any] | None = None
        leakage: dict[str, Any] | None = None
        output_digests: dict[str, str] = {}
        runtime: dict[str, Any] = {}
        failure: str | None = None
        try:
            leakage = SealedBenchmarkBoundary.verify(
                candidate,
                sealed,
                canaries=[canary],
                maximum_scan_bytes=512 * 1024 * 1024,
            )
            oracle_blend = sealed / "nocturne-one-oracle.blend"
            candidate_blend_digest = sha256_file(blend)[0]
            oracle_blend_digest = sha256_file(oracle_blend)[0]
            assertions.append(
                _assertion(
                    "candidate_is_not_oracle_blend",
                    "different SHA-256",
                    {
                        "candidate": candidate_blend_digest,
                        "oracle": oracle_blend_digest,
                    },
                    candidate_blend_digest != oracle_blend_digest,
                    "P0",
                )
            )
            blender = discover_blender()
            if not blender.available or not blender.path:
                raise RuntimeError("installed Blender is unavailable")
            inspect_script = (
                nocturne_benchmark_root()
                / "evaluator"
                / "inspect_candidate.py"
            )
            command = [
                blender.path,
                "--background",
                "--factory-startup",
                "--disable-autoexec",
                "--python-exit-code",
                "1",
                "--python",
                str(inspect_script),
                "--",
                str(blend),
                str(oracle_manifest_path),
                str(output),
                str(hero_glb),
                str(low_glb),
            ]
            completed = subprocess.run(
                command,
                capture_output=True,
                text=True,
                timeout=600,
                check=False,
            )
            (output / "blender.stdout.log").write_text(
                completed.stdout, encoding="utf-8"
            )
            (output / "blender.stderr.log").write_text(
                completed.stderr, encoding="utf-8"
            )
            if completed.returncode:
                raise RuntimeError(
                    "candidate inspection failed: " + completed.stderr[-4000:]
                )
            inspection_path = output / "candidate-inspection.json"
            inspection = json.loads(inspection_path.read_text(encoding="utf-8"))
            runtime = {
                "blender_version": blender.version,
                "blender_executable": blender.path,
                "blender_executable_sha256": sha256_file(Path(blender.path))[0],
                "inspection_script_sha256": sha256_file(inspect_script)[0],
                "host": platform.platform(),
            }
            required_parts = set(self.contract.required_parts)
            observed_parts = set(inspection["observed_parts"])
            assertions.append(
                _assertion(
                    "all_named_parts_present",
                    sorted(required_parts),
                    sorted(observed_parts),
                    required_parts == observed_parts,
                    "P0",
                )
            )
            specification = json.loads(
                (
                    nocturne_benchmark_root() / "governed_spec.json"
                ).read_text(encoding="utf-8")
            )
            expected_dimensions = specification["overall_dimensions_mm"]
            expected_vector = [
                expected_dimensions["width"],
                expected_dimensions["depth"],
                expected_dimensions["height"],
            ]
            observed_vector = inspection["primary_dimensions_mm"]
            dimension_errors = [
                abs(float(observed) - float(expected)) / float(expected)
                for observed, expected in zip(
                    observed_vector, expected_vector, strict=True
                )
            ]
            dimension_limit = float(
                self.contract.geometry_gates[
                    "overall_dimension_error_ratio_maximum"
                ]
            )
            assertions.append(
                _assertion(
                    "overall_dimensions",
                    {
                        "dimensions_mm": expected_vector,
                        "maximum_error_ratio": dimension_limit,
                    },
                    {
                        "dimensions_mm": observed_vector,
                        "error_ratios": dimension_errors,
                    },
                    max(dimension_errors) <= dimension_limit,
                    "P0",
                )
            )
            diagonal = math.sqrt(sum(value * value for value in expected_vector))
            placement_errors = {}
            for identifier in sorted(required_parts & observed_parts):
                oracle_location = oracle_manifest["objects"][identifier]["location"]
                candidate_location = inspection["parts"][identifier]["location"]
                distance = math.sqrt(
                    sum(
                        (float(first) - float(second)) ** 2
                        for first, second in zip(
                            oracle_location,
                            candidate_location,
                            strict=True,
                        )
                    )
                )
                placement_errors[identifier] = distance / diagonal
            placement_limit = float(
                self.contract.geometry_gates[
                    "part_placement_error_diagonal_ratio_maximum"
                ]
            )
            assertions.append(
                _assertion(
                    "part_placement",
                    {"maximum_diagonal_ratio": placement_limit},
                    placement_errors,
                    bool(placement_errors)
                    and max(placement_errors.values()) <= placement_limit,
                    "P0",
                )
            )
            exact_mesh_signatures = sorted(
                identifier
                for identifier in required_parts & observed_parts
                if (
                    inspection["parts"][identifier]["vertex_count"],
                    inspection["parts"][identifier]["polygon_count"],
                )
                == (
                    oracle_manifest["objects"][identifier]["vertex_count"],
                    oracle_manifest["objects"][identifier]["polygon_count"],
                )
            )
            assertions.append(
                _assertion(
                    "oracle_mesh_not_embedded",
                    "not every required part has the oracle mesh signature",
                    exact_mesh_signatures,
                    len(exact_mesh_signatures) < len(required_parts),
                    "P0",
                )
            )
            material_checks = _material_class_checks(
                inspection["material_details"]
            )
            assertions.append(
                _assertion(
                    "material_classes",
                    "all declared class checks pass",
                    material_checks,
                    all(material_checks.values()),
                    "P1",
                )
            )
            hierarchy = inspection["hierarchy"]
            assertions.append(
                _assertion(
                    "clean_scene_hierarchy",
                    {"root_present": True, "unparented_required_parts": []},
                    hierarchy,
                    hierarchy["root_present"]
                    and not hierarchy["unparented_required_parts"],
                )
            )
            assertions.append(
                _assertion(
                    "missing_textures",
                    [],
                    inspection["missing_textures"],
                    not inspection["missing_textures"],
                )
            )
            mesh_quality = inspection["mesh_quality"]
            mesh_passed = (
                mesh_quality["mesh_count"] > 0
                and not mesh_quality["missing_uv_objects"]
                and not mesh_quality["non_manifold_edges"]
                and not mesh_quality["non_finite_normal_objects"]
                and not mesh_quality["negative_scale_objects"]
            )
            assertions.append(
                _assertion(
                    "uv_topology_normals",
                    {
                        "mesh_count_minimum": 1,
                        "missing_uv_objects": [],
                        "non_manifold_edges": {},
                        "non_finite_normal_objects": [],
                        "negative_scale_objects": [],
                    },
                    mesh_quality,
                    mesh_passed,
                )
            )
            animation = inspection["animation"]
            assertions.append(
                _assertion(
                    "deterministic_exploded_animation",
                    {
                        "all_required_animated": True,
                        "frame_120_deterministic": True,
                    },
                    {
                        "all_required_animated": animation[
                            "all_required_animated"
                        ],
                        "frame_120_deterministic": animation[
                            "frame_120_deterministic"
                        ],
                    },
                    animation["all_required_animated"]
                    and animation["frame_120_deterministic"],
                )
            )
            hero_result = GlbValidator().validate(
                hero_glb,
                required_node_names=sorted(required_parts),
            )
            low_result = GlbValidator().validate(
                low_glb,
                required_node_names=sorted(required_parts),
            )
            hero_validation = hero_result.to_dict()
            low_validation = low_result.to_dict()
            assertions.append(
                _assertion(
                    "validated_glb_outputs",
                    {
                        "hero_valid": True,
                        "low_valid": True,
                        "invalid_reference_count": 0,
                    },
                    {
                        "hero_valid": hero_result.valid,
                        "hero_errors": hero_result.errors,
                        "low_valid": low_result.valid,
                        "low_errors": low_result.errors,
                    },
                    hero_result.valid and low_result.valid,
                    "P0",
                )
            )
            reimports = {
                "hero_object_count": inspection["hero_glb_reimport"][
                    "object_count"
                ],
                "low_object_count": inspection["low_glb_reimport"]["object_count"],
            }
            assertions.append(
                _assertion(
                    "glb_reimports",
                    {"hero_object_count_minimum": 1, "low_object_count_minimum": 1},
                    reimports,
                    all(value > 0 for value in reimports.values()),
                    "P0",
                )
            )
            hero_nodes = set(hero_result.named_identity["observed_nodes"])
            low_nodes = set(low_result.named_identity["observed_nodes"])
            assertions.append(
                _assertion(
                    "lod_named_identity",
                    sorted(required_parts),
                    {
                        "hero": sorted(required_parts & hero_nodes),
                        "low": sorted(required_parts & low_nodes),
                    },
                    required_parts <= hero_nodes and required_parts <= low_nodes,
                )
            )
            blend_reopened = inspection["blend_reopened"] is True
            assertions.append(
                _assertion(
                    "blend_reopens",
                    True,
                    inspection["blend_reopened"],
                    blend_reopened,
                    "P0",
                )
            )
            public_threshold = float(
                self.contract.geometry_gates["public_silhouette_iou_minimum"]
            )
            hidden_threshold = float(
                self.contract.geometry_gates["hidden_silhouette_iou_minimum"]
            )
            for label in self.contract.public_view_labels:
                oracle_path = packet / "references" / f"{label}.png"
                candidate_path = (
                    output / "silhouettes" / f"{label}.candidate.png"
                )
                score = _silhouette_iou(oracle_path, candidate_path)
                view_scores.append(
                    NocturneViewScore(
                        label=label,
                        visibility="public",
                        oracle_sha256=sha256_file(oracle_path)[0],
                        candidate_sha256=sha256_file(candidate_path)[0],
                        silhouette_iou=score,
                        threshold=public_threshold,
                        passed=score >= public_threshold,
                    )
                )
            for label in sorted(oracle_manifest["hidden_cameras"]):
                oracle_path = sealed / "holdouts" / f"{label}.png"
                candidate_path = (
                    output / "silhouettes" / f"{label}.candidate.png"
                )
                score = _silhouette_iou(oracle_path, candidate_path)
                view_scores.append(
                    NocturneViewScore(
                        label=label,
                        visibility="hidden",
                        oracle_sha256=sha256_file(oracle_path)[0],
                        candidate_sha256=sha256_file(candidate_path)[0],
                        silhouette_iou=score,
                        threshold=hidden_threshold,
                        passed=score >= hidden_threshold,
                    )
                )
            public_scores = [
                item.silhouette_iou
                for item in view_scores
                if item.visibility == "public"
            ]
            hidden_scores = [
                item.silhouette_iou
                for item in view_scores
                if item.visibility == "hidden"
            ]
            assertions.append(
                _assertion(
                    "public_silhouettes",
                    {"every_view_minimum": public_threshold},
                    public_scores,
                    len(public_scores) == len(self.contract.public_view_labels)
                    and min(public_scores) >= public_threshold,
                    "P0",
                )
            )
            assertions.append(
                _assertion(
                    "hidden_silhouettes",
                    {"every_view_minimum": hidden_threshold},
                    hidden_scores,
                    len(hidden_scores) == self.contract.hidden_holdout_count
                    and min(hidden_scores) >= hidden_threshold,
                    "P0",
                )
            )
            assertions.append(
                _assertion(
                    "fixed_evaluator_cameras",
                    {
                        "public_count": len(self.contract.public_view_labels),
                        "hidden_count": self.contract.hidden_holdout_count,
                        "camera_nudge": False,
                    },
                    {
                        "public_count": len(oracle_manifest["public_cameras"]),
                        "hidden_count": len(oracle_manifest["hidden_cameras"]),
                        "inspection_script_sha256": runtime[
                            "inspection_script_sha256"
                        ],
                    },
                    len(oracle_manifest["public_cameras"])
                    == len(self.contract.public_view_labels)
                    and len(oracle_manifest["hidden_cameras"])
                    == self.contract.hidden_holdout_count,
                    "P0",
                )
            )
            for path in sorted(output.rglob("*")):
                if path.is_file():
                    output_digests[path.relative_to(output).as_posix()] = (
                        sha256_file(path)[0]
                    )
        except Exception as error:
            failure = f"{type(error).__name__}: {error}"
            atomic_write_json(
                output / "nocturne-3d.failure.json",
                {
                    "schema_version": "1",
                    "failure": failure,
                    "completed_at": datetime.now(UTC).isoformat(),
                },
            )
        functional_passed = (
            bool(assertions)
            and all(item.passed for item in assertions)
            and len(view_scores)
            == len(self.contract.public_view_labels)
            + self.contract.hidden_holdout_count
            and all(item.passed for item in view_scores)
            and leakage is not None
            and failure is None
        )
        receipt = Nocturne3DReceipt(
            benchmark_id=self.contract.benchmark_id,
            source_git_head=source_head,
            contract_sha256=contract_digest,
            packet_manifest_sha256=packet_receipt["packet_manifest_sha256"],
            sealed_builder_receipt_sha256=sha256_file(builder_receipt_path)[0],
            oracle_manifest_sha256=sha256_file(oracle_manifest_path)[0],
            candidate_blend_sha256=sha256_file(blend)[0],
            candidate_hero_glb_sha256=sha256_file(hero_glb)[0],
            candidate_low_glb_sha256=sha256_file(low_glb)[0],
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            elapsed_seconds=time.monotonic() - started,
            status="PASS" if functional_passed else "FAIL",
            functional_passed=functional_passed,
            assertions=assertions,
            view_scores=view_scores,
            inspection=inspection,
            hero_glb_validation=hero_validation,
            low_glb_validation=low_validation,
            leakage_boundary=leakage,
            output_digests=output_digests,
            runtime=runtime,
            claim_boundary=[
                "Public and hidden silhouettes use fixed oracle camera states with no nudge.",
                "Part placement compares evaluator-only oracle bounds to independently built "
                "candidate bounds.",
                "Structural material checks do not prove universal photorealism.",
                "The result is specific to the fixed NOCTURNE/ONE contract and recorded host.",
            ],
            workspace=str(output),
            failure=failure,
        )
        atomic_write_json(
            output / "nocturne-3d.receipt.json",
            receipt.model_dump(mode="json"),
        )
        return receipt
