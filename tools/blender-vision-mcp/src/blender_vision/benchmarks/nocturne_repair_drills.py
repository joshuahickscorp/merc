from __future__ import annotations

import copy
import hashlib
import json
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class RepairCandidate(_StrictModel):
    candidate_id: str
    strategy: Literal[
        "threshold_relaxation", "global_baseline_replacement", "exact_inverse_patch"
    ]
    mutated_paths: list[str]
    allowed: bool
    reason: str
    selected: bool


class RepairDrillResult(_StrictModel):
    drill_id: str
    domain: Literal["app", "3d"]
    failure_class: str
    assertion_id: str
    severity: Literal["P0", "P1"]
    baseline_receipt_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    baseline_canonical_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    mutation_path: str
    before: Any
    injected: Any
    detection_passed: bool
    candidates: list[RepairCandidate]
    selected_candidate_id: Literal["exact-inverse-patch"]
    repaired_canonical_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    exact_restore: bool
    global_regression_count: int = Field(ge=0)
    status: Literal["PASS", "FAIL"]

    @model_validator(mode="after")
    def fixed_candidate_portfolio(self) -> RepairDrillResult:
        if len(self.candidates) != 3:
            raise ValueError("each repair drill requires exactly three fixed candidates")
        selected = [candidate for candidate in self.candidates if candidate.selected]
        if len(selected) != 1 or selected[0].candidate_id != self.selected_candidate_id:
            raise ValueError("repair drill must select only the exact inverse patch")
        return self


class NocturneRepairDrillReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-repair-drills-v1"]
    authority: Literal["DETERMINISTIC_RECEIPT_REPLAY"]
    input_receipt_sha256: dict[str, str]
    input_source_git_heads: dict[str, str]
    drill_count: Literal[12]
    passed_count: int = Field(ge=0, le=12)
    failed_count: int = Field(ge=0, le=12)
    drills: list[RepairDrillResult]
    status: Literal["PASS", "FAIL"]
    claim_boundary: list[str]

    @model_validator(mode="after")
    def fixed_drill_corpus(self) -> NocturneRepairDrillReceipt:
        if len(self.drills) != 12:
            raise ValueError("NOCTURNE/ONE requires the fixed 12-class drill corpus")
        if self.passed_count + self.failed_count != 12:
            raise ValueError("repair drill counts must sum to 12")
        identifiers = [drill.drill_id for drill in self.drills]
        if len(identifiers) != len(set(identifiers)):
            raise ValueError("repair drill identifiers must be unique")
        expected_status = "PASS" if self.failed_count == 0 else "FAIL"
        if self.status != expected_status:
            raise ValueError("repair drill receipt status disagrees with its results")
        return self


Detector = Callable[[Any], bool]


@dataclass(frozen=True)
class _DrillSpec:
    drill_id: str
    domain: Literal["app", "3d"]
    failure_class: str
    assertion_id: str
    severity: Literal["P0", "P1"]
    observed_path: tuple[str | int, ...]
    injected: Any
    detector: Detector


def _dimensions_valid(observed: Any) -> bool:
    expected = (320.0, 180.0, 360.0)
    dimensions = observed["dimensions_mm"]
    return len(dimensions) == 3 and all(
        abs(float(actual) - target) / target <= 0.01
        for actual, target in zip(dimensions, expected, strict=True)
    )


def _material_valid(observed: Any) -> bool:
    return observed["glass_is_transmissive"] is True


def _cameras_valid(observed: Any) -> bool:
    return observed["public_count"] == 6 and observed["hidden_count"] == 4


def _animation_valid(observed: Any) -> bool:
    return (
        observed["all_required_animated"] is True
        and observed["frame_120_deterministic"] is True
    )


def _mobile_valid(observed: Any) -> bool:
    return (
        observed["step_count"] == 10
        and observed["final_route"] == "/receipt"
        and observed["final_state"] == "successful_reservation"
    )


def _reduced_motion_valid(observed: Any) -> bool:
    return observed["reduced"] is True and observed["animation"] is False


def _glb_valid(observed: Any) -> bool:
    return observed["hero"] <= 5_242_880 and observed["mobile_lod"] <= 1_572_864


def _frame_time_valid(observed: Any) -> bool:
    return observed["median_fps"] >= 55.0 and observed["p95_ms"] <= 24.0


def _idempotency_valid(observed: Any) -> bool:
    return (
        observed["first"] == 201
        and observed["repeated"] == 200
        and observed["conflict"] == 409
        and observed["same_id"] is True
    )


def _migration_valid(observed: Any) -> bool:
    return all(
        observed[key]["exit_code"] == 0
        for key in (
            "migration_first",
            "migration_reapply",
            "migration_rollback",
            "migration_second",
        )
    )


def _accessibility_valid(observed: Any) -> bool:
    return observed["critical"] == 0 and observed["serious"] == 0


def _routes_valid(observed: Any) -> bool:
    return set(observed["routes"]) == {
        "/",
        "/technology",
        "/configurator",
        "/reserve",
        "/receipt",
    }


_DRILLS: tuple[_DrillSpec, ...] = (
    _DrillSpec(
        "geometry-dimension",
        "3d",
        "geometry dimension",
        "overall_dimensions",
        "P0",
        ("dimensions_mm", 0),
        336.0,
        _dimensions_valid,
    ),
    _DrillSpec(
        "incorrect-material-class",
        "3d",
        "incorrect material class",
        "material_classes",
        "P0",
        ("glass_is_transmissive",),
        False,
        _material_valid,
    ),
    _DrillSpec(
        "fixed-camera-mismatch",
        "3d",
        "fixed camera mismatch",
        "fixed_evaluator_cameras",
        "P0",
        ("public_count",),
        5,
        _cameras_valid,
    ),
    _DrillSpec(
        "broken-mobile-composition",
        "app",
        "broken mobile composition",
        "hidden_mobile_trace",
        "P0",
        ("final_route",),
        "/reserve",
        _mobile_valid,
    ),
    _DrillSpec(
        "animation-timing-drift",
        "3d",
        "animation timing drift",
        "deterministic_exploded_animation",
        "P0",
        ("frame_120_deterministic",),
        False,
        _animation_valid,
    ),
    _DrillSpec(
        "reduced-motion-regression",
        "app",
        "reduced motion regression",
        "reduced_motion",
        "P0",
        ("animation",),
        True,
        _reduced_motion_valid,
    ),
    _DrillSpec(
        "oversized-glb",
        "app",
        "oversized GLB",
        "glb_size_budgets",
        "P0",
        ("hero",),
        5_242_881,
        _glb_valid,
    ),
    _DrillSpec(
        "shader-frame-time-regression",
        "app",
        "shader/frame time regression",
        "desktop_3d_frames",
        "P1",
        ("p95_ms",),
        24.1,
        _frame_time_valid,
    ),
    _DrillSpec(
        "api-idempotency",
        "app",
        "API idempotency",
        "reservation_idempotency",
        "P0",
        ("same_id",),
        False,
        _idempotency_valid,
    ),
    _DrillSpec(
        "database-migration",
        "app",
        "DB migration",
        "fresh_clone_commands",
        "P0",
        ("migration_reapply", "exit_code"),
        1,
        _migration_valid,
    ),
    _DrillSpec(
        "accessibility",
        "app",
        "accessibility",
        "automated_accessibility",
        "P0",
        ("serious",),
        1,
        _accessibility_valid,
    ),
    _DrillSpec(
        "unrelated-route-regression",
        "app",
        "unrelated route regression",
        "required_routes_and_states",
        "P0",
        ("routes",),
        ["/", "/configurator", "/reserve", "/technology"],
        _routes_valid,
    ),
)


def nocturne_repair_drill_ids() -> tuple[str, ...]:
    return tuple(spec.drill_id for spec in _DRILLS)


def _canonical_digest(value: Any) -> str:
    return hashlib.sha256(canonical_json(value)).hexdigest()


def _find_assertion(receipt: dict[str, Any], assertion_id: str) -> dict[str, Any]:
    matches = [
        assertion
        for assertion in receipt.get("assertions", [])
        if assertion.get("id") == assertion_id
    ]
    if len(matches) != 1:
        raise ValueError(f"receipt must contain one assertion named {assertion_id}")
    return matches[0]


def _path_get(value: Any, path: tuple[str | int, ...]) -> Any:
    cursor = value
    for part in path:
        cursor = cursor[part]
    return copy.deepcopy(cursor)


def _path_set(value: Any, path: tuple[str | int, ...], replacement: Any) -> None:
    cursor = value
    for part in path[:-1]:
        cursor = cursor[part]
    cursor[path[-1]] = copy.deepcopy(replacement)


def _require_passing_receipt(receipt: dict[str, Any], label: str) -> None:
    if receipt.get("status") != "PASS" or receipt.get("functional_passed") is not True:
        raise ValueError(f"{label} baseline receipt must be a functional PASS")
    failed = [
        assertion.get("id")
        for assertion in receipt.get("assertions", [])
        if assertion.get("passed") is not True
    ]
    if failed:
        raise ValueError(f"{label} baseline contains failed assertions: {failed}")


def _portfolio(mutation_path: str) -> list[RepairCandidate]:
    return [
        RepairCandidate(
            candidate_id="relax-evaluator-threshold",
            strategy="threshold_relaxation",
            mutated_paths=["evaluator.threshold"],
            allowed=False,
            reason="Rejected: changes frozen acceptance authority instead of product evidence.",
            selected=False,
        ),
        RepairCandidate(
            candidate_id="replace-global-baseline",
            strategy="global_baseline_replacement",
            mutated_paths=["receipt"],
            allowed=False,
            reason="Rejected: overbroad replacement exceeds the one-fact causal surface.",
            selected=False,
        ),
        RepairCandidate(
            candidate_id="exact-inverse-patch",
            strategy="exact_inverse_patch",
            mutated_paths=[mutation_path],
            allowed=True,
            reason="Selected: restores only the injected observed fact and fixed pass metadata.",
            selected=True,
        ),
    ]


class NocturneRepairDrillRunner:
    """Replay the fixed repair corpus against frozen real evaluator receipts."""

    def run(
        self,
        *,
        app_receipt_path: Path,
        three_d_receipt_path: Path,
        output_path: Path | None = None,
    ) -> NocturneRepairDrillReceipt:
        paths = {
            "app": app_receipt_path.expanduser().resolve(),
            "3d": three_d_receipt_path.expanduser().resolve(),
        }
        receipts = {
            label: json.loads(path.read_text(encoding="utf-8"))
            for label, path in paths.items()
        }
        for label, receipt in receipts.items():
            _require_passing_receipt(receipt, label)

        input_sha = {label: sha256_file(path)[0] for label, path in paths.items()}
        canonical_sha = {
            label: _canonical_digest(receipt) for label, receipt in receipts.items()
        }
        results: list[RepairDrillResult] = []
        for spec in _DRILLS:
            baseline = receipts[spec.domain]
            mutated = copy.deepcopy(baseline)
            assertion = _find_assertion(mutated, spec.assertion_id)
            before = _path_get(assertion["observed"], spec.observed_path)
            _path_set(assertion["observed"], spec.observed_path, spec.injected)
            assertion["passed"] = False
            mutated["status"] = "FAIL"
            mutated["functional_passed"] = False
            detection_passed = not spec.detector(assertion["observed"])

            mutation_path = (
                f"assertions[{spec.assertion_id}].observed."
                + ".".join(str(part) for part in spec.observed_path)
            )
            repaired = copy.deepcopy(mutated)
            repaired_assertion = _find_assertion(repaired, spec.assertion_id)
            _path_set(repaired_assertion["observed"], spec.observed_path, before)
            repaired_assertion["passed"] = True
            repaired["status"] = baseline["status"]
            repaired["functional_passed"] = baseline["functional_passed"]
            repaired_sha = _canonical_digest(repaired)
            exact_restore = repaired_sha == canonical_sha[spec.domain]
            regression_count = sum(
                assertion_item.get("passed") is not True
                for assertion_item in repaired["assertions"]
            )
            passed = detection_passed and exact_restore and regression_count == 0
            results.append(
                RepairDrillResult(
                    drill_id=spec.drill_id,
                    domain=spec.domain,
                    failure_class=spec.failure_class,
                    assertion_id=spec.assertion_id,
                    severity=spec.severity,
                    baseline_receipt_sha256=input_sha[spec.domain],
                    baseline_canonical_sha256=canonical_sha[spec.domain],
                    mutation_path=mutation_path,
                    before=before,
                    injected=spec.injected,
                    detection_passed=detection_passed,
                    candidates=_portfolio(mutation_path),
                    selected_candidate_id="exact-inverse-patch",
                    repaired_canonical_sha256=repaired_sha,
                    exact_restore=exact_restore,
                    global_regression_count=regression_count,
                    status="PASS" if passed else "FAIL",
                )
            )

        passed_count = sum(result.status == "PASS" for result in results)
        receipt = NocturneRepairDrillReceipt(
            benchmark_id="nocturne-one-repair-drills-v1",
            authority="DETERMINISTIC_RECEIPT_REPLAY",
            input_receipt_sha256=input_sha,
            input_source_git_heads={
                label: str(receipt_value.get("source_git_head", "unknown"))
                for label, receipt_value in receipts.items()
            },
            drill_count=12,
            passed_count=passed_count,
            failed_count=12 - passed_count,
            drills=results,
            status="PASS" if passed_count == 12 else "FAIL",
            claim_boundary=[
                "Each drill mutates one fixed observation in a frozen real runtime "
                "acceptance receipt, proves deterministic detection, evaluates the same "
                "three repair strategies, and verifies canonical whole-receipt restoration.",
                "Threshold relaxation and whole-baseline replacement are prohibited; only "
                "the exact inverse of the injected fact may be selected.",
                "This is executable receipt-level repair replay. It is not twelve fresh "
                "Blender, browser, API, database, accessibility, or performance reruns.",
            ],
        )
        if output_path is not None:
            atomic_write_json(output_path.expanduser().resolve(), receipt.model_dump(mode="json"))
        return receipt
