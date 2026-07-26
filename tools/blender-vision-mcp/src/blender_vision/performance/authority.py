from __future__ import annotations

import difflib
import hashlib
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.core.util import atomic_write_text, canonical_json, sha256_file


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class PerformanceBudget(_StrictModel):
    initial_transfer_bytes: int = Field(ge=1)
    javascript_execution_ms: float = Field(gt=0)
    selected_glb_bytes: int = Field(ge=1)
    texture_memory_bytes: int = Field(ge=1)
    shader_compilation_ms: float = Field(gt=0)
    frame_p95_ms: float = Field(gt=0)
    dropped_frame_ratio: float = Field(ge=0, le=1)
    long_task_count: int = Field(ge=0)
    cumulative_layout_shift: float = Field(ge=0)
    interaction_p95_ms: float = Field(gt=0)
    javascript_heap_growth_bytes: int = Field(ge=0)
    retained_allocation_bytes: int = Field(ge=0)
    api_p95_ms: float = Field(gt=0)
    database_query_p95_ms: float = Field(gt=0)
    maximum_effective_dpr: float = Field(gt=0)


class PerformanceMeasurement(_StrictModel):
    schema_version: Literal["1"] = "1"
    variant: Literal["degraded", "repaired"]
    browser_engine: Literal["chromium"]
    browser_version: str
    browser_executable: str
    browser_executable_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    initial_transfer_bytes: int = Field(ge=0)
    javascript_execution_ms: float = Field(ge=0)
    cdp_script_duration_ms: float = Field(ge=0)
    selected_glb: str
    selected_glb_bytes: int = Field(ge=0)
    texture_memory_bytes: int = Field(ge=0)
    shader_compilation_ms: float = Field(ge=0)
    shader_source_count: int = Field(ge=0)
    draw_call_count: int = Field(ge=0)
    frame_sample_count: int = Field(ge=1)
    frame_p95_ms: float = Field(ge=0)
    dropped_frame_ratio: float = Field(ge=0, le=1)
    long_task_count: int = Field(ge=0)
    long_task_total_ms: float = Field(ge=0)
    cumulative_layout_shift: float = Field(ge=0)
    interaction_samples_ms: list[float]
    interaction_p95_ms: float = Field(ge=0)
    javascript_heap_growth_bytes: int = Field(ge=0)
    retained_allocation_bytes: int = Field(ge=0)
    api_samples_ms: list[float]
    api_p95_ms: float = Field(ge=0)
    database_query_samples_ms: list[float]
    database_query_p95_ms: float = Field(ge=0)
    database_query_plan: list[str]
    database_uses_index: bool
    initial_resource_paths: list[str]
    intent_resource_paths: list[str]
    eager_3d_asset_on_initial_load: bool
    lazy_3d_asset_after_intent: bool
    lod_level: Literal["HIGH", "LOW", "NONE"]
    adaptive_dpr: bool
    effective_dpr: float = Field(gt=0)
    reduced_motion_honored: bool
    no_webgl_fallback: bool
    webgl_observed: bool
    screenshot_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    aria_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    behavior_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    api_contract_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    selected_glb_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    selected_glb_valid: bool
    selected_glb_node_names: list[str]
    selected_glb_mesh_names: list[str]
    console_errors: list[str]

    @model_validator(mode="after")
    def samples_are_present(self) -> PerformanceMeasurement:
        if not self.interaction_samples_ms:
            raise ValueError("interaction samples are required")
        if not self.api_samples_ms:
            raise ValueError("API samples are required")
        if not self.database_query_samples_ms:
            raise ValueError("database query samples are required")
        return self


class PerformancePreservation(_StrictModel):
    screenshot_equal: bool
    screenshot_max_channel_error: int = Field(ge=0, le=255)
    aria_equal: bool
    behavior_equal: bool
    api_contract_equal: bool
    glb_named_identity_equal: bool
    degraded_glb_valid: bool
    repaired_glb_valid: bool

    @property
    def passed(self) -> bool:
        return (
            self.screenshot_equal
            and self.screenshot_max_channel_error == 0
            and self.aria_equal
            and self.behavior_equal
            and self.api_contract_equal
            and self.glb_named_identity_equal
            and self.degraded_glb_valid
            and self.repaired_glb_valid
        )


class PerformanceAssertion(_StrictModel):
    id: str
    expected: Any
    observed: Any
    passed: bool


class PerformanceRepairReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    repair_id: str
    source_relative_path: str
    before_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    after_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    allowed_replacements: list[dict[str, str]]
    replacement_count: int = Field(ge=1)
    changed_paths: list[str]
    unified_diff_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    unified_diff: str


class PerformanceAuthority:
    """Evaluate measured performance without converting estimates into observations."""

    def __init__(self, budget: PerformanceBudget):
        self.budget = budget

    @staticmethod
    def _maximum(identifier: str, expected: Any, observed: Any) -> PerformanceAssertion:
        return PerformanceAssertion(
            id=identifier,
            expected={"maximum": expected},
            observed=observed,
            passed=observed <= expected,
        )

    @staticmethod
    def _required(identifier: str, observed: bool) -> PerformanceAssertion:
        return PerformanceAssertion(
            id=identifier,
            expected=True,
            observed=observed,
            passed=observed is True,
        )

    def evaluate(
        self,
        measurement: PerformanceMeasurement,
        *,
        preservation: PerformancePreservation | None = None,
    ) -> list[PerformanceAssertion]:
        budget = self.budget
        assertions = [
            self._maximum(
                "initial_transfer_bytes",
                budget.initial_transfer_bytes,
                measurement.initial_transfer_bytes,
            ),
            self._maximum(
                "javascript_execution_ms",
                budget.javascript_execution_ms,
                measurement.javascript_execution_ms,
            ),
            self._maximum(
                "selected_glb_bytes",
                budget.selected_glb_bytes,
                measurement.selected_glb_bytes,
            ),
            self._maximum(
                "texture_memory_bytes",
                budget.texture_memory_bytes,
                measurement.texture_memory_bytes,
            ),
            self._maximum(
                "shader_compilation_ms",
                budget.shader_compilation_ms,
                measurement.shader_compilation_ms,
            ),
            self._maximum("frame_p95_ms", budget.frame_p95_ms, measurement.frame_p95_ms),
            self._maximum(
                "dropped_frame_ratio",
                budget.dropped_frame_ratio,
                measurement.dropped_frame_ratio,
            ),
            self._maximum(
                "long_task_count",
                budget.long_task_count,
                measurement.long_task_count,
            ),
            self._maximum(
                "cumulative_layout_shift",
                budget.cumulative_layout_shift,
                measurement.cumulative_layout_shift,
            ),
            self._maximum(
                "interaction_p95_ms",
                budget.interaction_p95_ms,
                measurement.interaction_p95_ms,
            ),
            self._maximum(
                "javascript_heap_growth_bytes",
                budget.javascript_heap_growth_bytes,
                measurement.javascript_heap_growth_bytes,
            ),
            self._maximum(
                "retained_allocation_bytes",
                budget.retained_allocation_bytes,
                measurement.retained_allocation_bytes,
            ),
            self._maximum("api_p95_ms", budget.api_p95_ms, measurement.api_p95_ms),
            self._maximum(
                "database_query_p95_ms",
                budget.database_query_p95_ms,
                measurement.database_query_p95_ms,
            ),
            self._maximum(
                "effective_dpr",
                budget.maximum_effective_dpr,
                measurement.effective_dpr,
            ),
            self._required("database_uses_index", measurement.database_uses_index),
            self._required(
                "initial_3d_asset_is_lazy",
                not measurement.eager_3d_asset_on_initial_load,
            ),
            self._required(
                "intent_loads_3d_asset",
                measurement.lazy_3d_asset_after_intent,
            ),
            self._required("adaptive_low_lod", measurement.lod_level == "LOW"),
            self._required("adaptive_dpr", measurement.adaptive_dpr),
            self._required("reduced_motion_honored", measurement.reduced_motion_honored),
            self._required("no_webgl_fallback", measurement.no_webgl_fallback),
            self._required("webgl_observed", measurement.webgl_observed),
            self._required("glb_structurally_valid", measurement.selected_glb_valid),
            self._required("console_is_clean", not measurement.console_errors),
        ]
        if preservation is not None:
            assertions.extend(
                [
                    self._required("visual_gate_preserved", preservation.screenshot_equal),
                    self._required("aria_gate_preserved", preservation.aria_equal),
                    self._required("behavior_gate_preserved", preservation.behavior_equal),
                    self._required(
                        "api_contract_preserved", preservation.api_contract_equal
                    ),
                    self._required(
                        "glb_identity_preserved",
                        preservation.glb_named_identity_equal,
                    ),
                    self._required("all_preservation_gates", preservation.passed),
                ]
            )
        return assertions


class BoundedPerformanceRepair:
    """Apply an exact, digest-bound policy repair to one owned JavaScript fixture."""

    def __init__(
        self,
        *,
        relative_path: str,
        expected_before_sha256: str,
        replacements: list[tuple[str, str]],
    ):
        candidate = Path(relative_path)
        if (
            candidate.is_absolute()
            or ".." in candidate.parts
            or candidate.as_posix() != relative_path
        ):
            raise ValueError("repair path must be a normalized relative path")
        if not replacements or len({before for before, _after in replacements}) != len(
            replacements
        ):
            raise ValueError("repair replacements must have unique non-empty preimages")
        if any(not before or before == after for before, after in replacements):
            raise ValueError("repair replacements must change non-empty exact preimages")
        self.relative_path = relative_path
        self.expected_before_sha256 = expected_before_sha256
        self.replacements = replacements

    def apply(self, root: Path) -> PerformanceRepairReceipt:
        root = root.expanduser().resolve()
        target = (root / self.relative_path).resolve()
        if not target.is_relative_to(root) or not target.is_file() or target.is_symlink():
            raise ValueError("repair target is missing, linked, or escaped its root")
        before_sha256, _ = sha256_file(target)
        if before_sha256 != self.expected_before_sha256:
            raise ValueError(
                "repair preimage digest mismatch: "
                f"expected {self.expected_before_sha256}, observed {before_sha256}"
            )
        before_text = target.read_text(encoding="utf-8")
        after_text = before_text
        for before, after in self.replacements:
            count = after_text.count(before)
            if count != 1:
                raise ValueError(
                    f"repair preimage must occur exactly once, observed {count}: {before!r}"
                )
            after_text = after_text.replace(before, after, 1)
        diff = "".join(
            difflib.unified_diff(
                before_text.splitlines(keepends=True),
                after_text.splitlines(keepends=True),
                fromfile=f"a/{self.relative_path}",
                tofile=f"b/{self.relative_path}",
            )
        )
        if not diff:
            raise ValueError("bounded repair produced no diff")
        atomic_write_text(target, after_text)
        after_sha256, _ = sha256_file(target)
        return PerformanceRepairReceipt(
            repair_id=hashlib.sha256(
                canonical_json(
                    {
                        "path": self.relative_path,
                        "before": before_sha256,
                        "after": after_sha256,
                        "replacements": self.replacements,
                    }
                )
            ).hexdigest(),
            source_relative_path=self.relative_path,
            before_sha256=before_sha256,
            after_sha256=after_sha256,
            allowed_replacements=[
                {"before": before, "after": after}
                for before, after in self.replacements
            ],
            replacement_count=len(self.replacements),
            changed_paths=[self.relative_path],
            unified_diff_sha256=hashlib.sha256(diff.encode("utf-8")).hexdigest(),
            unified_diff=diff,
        )
