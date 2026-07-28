from __future__ import annotations

import hashlib
import json
from enum import StrEnum
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class AuthorityClass(StrEnum):
    OBSERVED = "OBSERVED"
    SPECIFIED = "SPECIFIED"
    DERIVED = "DERIVED"
    HYPOTHESIS = "HYPOTHESIS"


class ReferenceSource(StrictModel):
    id: str
    kind: Literal[
        "product_brief",
        "user_story",
        "api_contract",
        "data_schema",
        "auth_policy",
        "business_rules",
        "error_matrix",
        "analytics_requirements",
        "deployment_target",
        "slo",
        "performance_budget",
        "accessibility_requirements",
        "visual_reference",
        "motion_reference",
        "design_system_export",
        "source_code",
        "sample_payload",
    ]
    authority: AuthorityClass
    rights_state: Literal[
        "OWNED",
        "AUTHORIZED",
        "CC0",
        "PERMISSIVE",
        "INTERNAL",
        "UNKNOWN",
    ]
    digest: str = Field(pattern=r"^[0-9a-f]{64}$")
    locator: str
    notes: str = ""


class RouteSpec(StrictModel):
    id: str
    path: str
    title: str
    purpose: str
    required_states: list[str]
    visual_reference_ids: list[str] = Field(default_factory=list)


class ProductSpecIR(StrictModel):
    id: str
    name: str
    version: str
    summary: str
    actors: list[str]
    goals: list[str]
    non_goals: list[str]
    routes: list[RouteSpec]
    feature_flags: list[str] = Field(default_factory=list)
    authority_refs: list[str]


class JourneyStep(StrictModel):
    id: str
    actor: str
    route_id: str
    action: str
    expected_state: str
    api_operation_id: str | None = None
    error_recovery: str | None = None


class UserJourney(StrictModel):
    id: str
    name: str
    actor: str
    preconditions: list[str]
    steps: list[JourneyStep]
    success_postconditions: list[str]
    denied_or_failure_paths: list[str]


class UserJourneyGraph(StrictModel):
    journeys: list[UserJourney]
    authority_refs: list[str]


class DataField(StrictModel):
    name: str
    data_type: Literal[
        "string",
        "integer",
        "number",
        "boolean",
        "datetime",
        "json",
        "binary",
        "uuid",
    ]
    nullable: bool = False
    primary_key: bool = False
    unique: bool = False
    default: Any | None = None
    validation: dict[str, Any] = Field(default_factory=dict)


class DataEntity(StrictModel):
    name: str
    table_name: str
    fields: list[DataField]
    indexes: list[list[str]] = Field(default_factory=list)
    retention_policy: str
    contains_sensitive_data: bool = False

    @model_validator(mode="after")
    def one_primary_key(self) -> DataEntity:
        keys = [field.name for field in self.fields if field.primary_key]
        if len(keys) != 1:
            raise ValueError(f"entity {self.name} must declare exactly one primary key")
        field_names = {field.name for field in self.fields}
        for index in self.indexes:
            missing = set(index) - field_names
            if missing:
                raise ValueError(f"entity {self.name} index references missing fields: {missing}")
        return self


class DataRelation(StrictModel):
    id: str
    from_entity: str
    from_field: str
    to_entity: str
    to_field: str
    cardinality: Literal["one_to_one", "one_to_many", "many_to_many"]
    on_delete: Literal["restrict", "cascade", "set_null"]


class DataModelGraph(StrictModel):
    database_engine: Literal["sqlite", "postgresql"]
    entities: list[DataEntity]
    relations: list[DataRelation]
    seed_policy: str
    migration_policy: str
    rollback_policy: str
    authority_refs: list[str]


class APIField(StrictModel):
    name: str
    location: Literal["path", "query", "header", "body", "response"]
    data_type: str
    required: bool
    validation: dict[str, Any] = Field(default_factory=dict)


class APIResponse(StrictModel):
    status: int = Field(ge=100, le=599)
    description: str
    schema_ref: str | None = None


class FileBoundary(StrictModel):
    allowed_content_types: list[str]
    maximum_bytes: int = Field(gt=0)
    storage_policy: str
    malware_scan_policy: str


class IdempotencyContract(StrictModel):
    key_header: str
    scope: Literal["actor", "tenant", "global"]
    request_hash_required: bool = True
    replay_status: int = Field(ge=200, le=299)
    conflict_status: int = Field(ge=400, le=499)
    retention_seconds: int = Field(gt=0)


class HandlerBinding(StrictModel):
    kind: Literal[
        "list_entities",
        "get_entity",
        "create_entity",
        "update_entity",
        "delete_entity",
        "idempotent_create",
        "file_upload",
        "status_lookup",
    ]
    entity_ref: str
    id_field: str
    status_field: str | None = None
    initial_status: str | None = None
    storage_subdirectory: str | None = None
    field_bindings: dict[str, str] = Field(default_factory=dict)


class APIEndpoint(StrictModel):
    operation_id: str
    method: Literal["GET", "POST", "PUT", "PATCH", "DELETE"]
    path: str
    summary: str
    request_fields: list[APIField]
    responses: list[APIResponse]
    entity_refs: list[str]
    business_rule_ids: list[str] = Field(default_factory=list)
    handler: HandlerBinding
    authorization: Literal["public", "authenticated", "permission"]
    required_permissions: list[str] = Field(default_factory=list)
    idempotency: IdempotencyContract | None = None
    file_boundary: FileBoundary | None = None
    rate_limit: str
    timeout_ms: int = Field(gt=0)

    @model_validator(mode="after")
    def endpoint_contract_is_explicit(self) -> APIEndpoint:
        if self.authorization == "permission" and not self.required_permissions:
            raise ValueError(f"{self.operation_id} requires explicit permissions")
        if self.authorization != "permission" and self.required_permissions:
            raise ValueError(
                f"{self.operation_id} declares permissions without permission authorization"
            )
        if self.file_boundary and self.method not in {"POST", "PUT", "PATCH"}:
            raise ValueError(f"{self.operation_id} file boundary requires a mutating method")
        if self.handler.kind == "idempotent_create" and self.idempotency is None:
            raise ValueError(f"{self.operation_id} idempotent handler requires idempotency")
        if self.handler.kind == "file_upload" and self.file_boundary is None:
            raise ValueError(f"{self.operation_id} upload handler requires file boundaries")
        if self.handler.kind == "status_lookup" and self.method != "GET":
            raise ValueError(f"{self.operation_id} status lookup must use GET")
        return self


class APIContractGraph(StrictModel):
    protocol: Literal["REST", "GraphQL", "RPC"]
    base_path: str
    version: str
    endpoints: list[APIEndpoint]
    error_envelope: dict[str, str]
    sample_payload_refs: list[str]
    authority_refs: list[str]


class PermissionSpec(StrictModel):
    id: str
    description: str
    resource: str
    actions: list[str]


class RoleSpec(StrictModel):
    id: str
    description: str
    permission_ids: list[str]


class AuthPolicyGraph(StrictModel):
    provider: Literal[
        "none",
        "test_header",
        "oidc",
        "oauth2",
        "signed_session",
        "api_key",
    ]
    identity_claim: str
    tenant_claim: str | None = None
    session_lifetime_seconds: int | None = Field(default=None, gt=0)
    roles: list[RoleSpec]
    permissions: list[PermissionSpec]
    default_role: str | None
    deny_by_default: bool
    cross_tenant_policy: str
    credential_storage_policy: str
    authority_refs: list[str]


class BusinessRule(StrictModel):
    id: str
    description: str
    inputs: list[str]
    preconditions: list[str]
    deterministic_effect: str
    invariants: list[str]
    retry_behavior: str
    duplicate_behavior: str
    failure_behavior: str
    acceptance_test_ids: list[str]


class BusinessRuleGraph(StrictModel):
    rules: list[BusinessRule]
    edge_case_matrix: list[dict[str, str]]
    analytics_events: list[dict[str, str]]
    authority_refs: list[str]


class DeploymentNode(StrictModel):
    id: str
    kind: Literal["frontend", "api", "database", "worker", "object_store", "proxy"]
    runtime: str
    artifact: str
    health_check: str
    resource_limits: dict[str, str]


class DeploymentGraph(StrictModel):
    target: Literal[
        "local_process",
        "local_container",
        "kubernetes",
        "cloud_run",
        "user_authorized_remote",
    ]
    nodes: list[DeploymentNode]
    edges: list[dict[str, str]]
    secret_names: list[str]
    migration_command: str
    rollback_command: str
    fresh_clone_command: str
    release_command: str
    authority_refs: list[str]


class SLOSpec(StrictModel):
    id: str
    indicator: str
    objective: str
    window: str
    alert: str


class ObservabilityGraph(StrictModel):
    structured_log_fields: list[str]
    metrics: list[str]
    traces: list[str]
    slos: list[SLOSpec]
    dashboards: list[str]
    alerts: list[str]
    privacy_redactions: list[str]
    authority_refs: list[str]


class AcceptanceTest(StrictModel):
    id: str
    level: Literal[
        "unit",
        "integration",
        "api",
        "browser",
        "accessibility",
        "visual",
        "performance",
        "security",
        "migration",
        "rollback",
    ]
    requirement: str
    command: str
    expected: str
    route_ids: list[str] = Field(default_factory=list)
    journey_ids: list[str] = Field(default_factory=list)
    operation_ids: list[str] = Field(default_factory=list)


class AcceptanceTestGraph(StrictModel):
    tests: list[AcceptanceTest]
    global_regression_command: str
    fresh_clone_command: str
    authority_refs: list[str]


class ErrorCase(StrictModel):
    id: str
    trigger: str
    expected_user_state: str
    expected_api_status: int | None = Field(default=None, ge=100, le=599)
    retry_policy: str
    acceptance_test_id: str


class PerformanceBudget(StrictModel):
    metric: str
    operation: Literal["<=", ">=", "=="]
    value: float
    unit: str
    measurement_command: str


class AccessibilityRequirement(StrictModel):
    id: str
    criterion: str
    level: Literal["A", "AA", "AAA", "benchmark"]
    test_method: str


class ApplicationReferencePacket(StrictModel):
    schema_version: Literal["1"] = "1"
    packet_id: str
    sources: list[ReferenceSource]
    product: ProductSpecIR
    journeys: UserJourneyGraph
    data_model: DataModelGraph
    api_contract: APIContractGraph
    auth_policy: AuthPolicyGraph
    business_rules: BusinessRuleGraph
    deployment: DeploymentGraph
    observability: ObservabilityGraph
    acceptance: AcceptanceTestGraph
    error_cases: list[ErrorCase]
    performance_budgets: list[PerformanceBudget]
    accessibility_requirements: list[AccessibilityRequirement]
    visual_reference_ids: list[str]
    motion_reference_ids: list[str]
    design_system_reference_ids: list[str]
    source_code_reference_ids: list[str] = Field(default_factory=list)

    def canonical_digest(self) -> str:
        encoded = json.dumps(
            self.model_dump(mode="json"),
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()


class CompletenessFinding(StrictModel):
    id: str
    path: str
    status: Literal["AUTHORITATIVE", "HYPOTHETICAL", "MISSING", "CONTRADICTORY"]
    severity: Literal["P0", "P1", "P2"]
    message: str
    authority_refs: list[str]
    exact_resumption_contract: str | None = None


class ReferenceCompletenessReport(StrictModel):
    schema_version: Literal["1"] = "1"
    packet_id: str
    packet_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    compilable_as_draft: bool
    promotable: bool
    findings: list[CompletenessFinding]
    missing_authority: list[str]
    hypotheses: list[str]
    contradictions: list[str]
    exact_resumption_contracts: list[str]
    authority_coverage: dict[str, float]
