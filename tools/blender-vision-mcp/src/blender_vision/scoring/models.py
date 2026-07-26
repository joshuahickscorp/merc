from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

CapabilityDomain = Literal["app", "3d", "system"]
CapabilityStatus = Literal[
    "PROVEN_100_PLUS",
    "PROVEN_BELOW_100",
    "BLOCKED_EXTERNAL",
    "NOT_APPLICABLE",
]
EvidenceKind = Literal[
    "implementation",
    "real_runtime",
    "external_holdout",
    "receipt",
    "adversarial_recovery",
    "bounded_repair",
    "global_regression",
]


class CapabilityFacet(BaseModel):
    model_config = ConfigDict(extra="forbid")

    id: str
    domain: CapabilityDomain
    name: str
    baseline_score: int = Field(ge=0, le=110)
    baseline_grade: str
    baseline_assessment: str
    required_reference_class: list[str]
    required_real_runtimes: list[str]
    required_external_or_holdout_tests: list[str]
    required_metrics: dict[str, dict[str, Any]]
    required_receipts: list[str]
    known_blockers: list[str]
    current_score: int = Field(ge=0, le=110)
    status: CapabilityStatus
    evidence: list[str]
    reproduction_commands: list[str]

    @model_validator(mode="after")
    def baseline_is_not_rewritten(self) -> CapabilityFacet:
        if self.current_score < self.baseline_score:
            raise ValueError("current_score cannot be lower than the preserved baseline score")
        if self.current_score >= 100 and self.status != "PROVEN_100_PLUS":
            raise ValueError("scores at or above 100 must use PROVEN_100_PLUS")
        if self.current_score < 100 and self.status == "PROVEN_100_PLUS":
            raise ValueError("PROVEN_100_PLUS requires a score at or above 100")
        return self


class EvidenceRecord(BaseModel):
    model_config = ConfigDict(extra="forbid")

    id: str
    kind: EvidenceKind
    artifact_path: str
    artifact_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    executed: bool = True
    fixture_only: bool = False
    adapter_only: bool = False
    simulated_hardware: bool = False
    runtime: str | None = None
    test_id: str | None = None
    target_id: str | None = None
    target_class: str | None = None
    notes: str = ""


class ExternalBlocker(BaseModel):
    model_config = ConfigDict(extra="forbid")

    id: str
    reason: str
    exact_resumption_contract: str
    credential_class: str | None = None
    device_class: str | None = None
    command: str | None = None


class CapabilityEvidence(BaseModel):
    model_config = ConfigDict(extra="forbid")

    schema_version: Literal["1"] = "1"
    facet_id: str
    proposed_score: int = Field(ge=0, le=110)
    proposed_status: CapabilityStatus | None = None
    git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    catalog_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    registry_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    builder_identity: str
    evaluator_identity: str
    evaluator_had_builder_access: bool = False
    manual_edits_receipted: bool = True
    thresholds_changed_after_run: bool = False
    metrics: dict[str, Any] = Field(default_factory=dict)
    records: list[EvidenceRecord] = Field(default_factory=list)
    external_blockers: list[ExternalBlocker] = Field(default_factory=list)
    target_variants: list[str] = Field(default_factory=list)
    reproduction_commands: list[str] = Field(default_factory=list)
    unresolved_defects: dict[str, int] = Field(default_factory=lambda: {"P0": 0, "P1": 0})
    not_applicable_justification: str | None = None


class FacetEvaluation(BaseModel):
    model_config = ConfigDict(extra="forbid")

    facet_id: str
    domain: CapabilityDomain
    baseline_score: int
    requested_score: int
    accepted_score: int
    status: CapabilityStatus
    accepted: bool
    errors: list[str]
    warnings: list[str]
    evidence_ids: list[str]
    reproduction_commands: list[str]
    external_blockers: list[ExternalBlocker]


class CapabilityReport(BaseModel):
    model_config = ConfigDict(extra="forbid")

    schema_version: Literal["1"] = "1"
    generated_at: str
    git_head: str
    catalog_sha256: str
    registry_sha256: str
    evaluations: list[FacetEvaluation]
    evidence_sources: list[dict[str, str]]
    summary: dict[str, Any]
    report_sha256: str
