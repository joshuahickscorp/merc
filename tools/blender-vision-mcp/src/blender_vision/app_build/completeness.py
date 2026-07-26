from __future__ import annotations

from collections import Counter
from collections.abc import Iterable

from blender_vision.app_build.specification import (
    ApplicationReferencePacket,
    AuthorityClass,
    CompletenessFinding,
    ReferenceCompletenessReport,
)


class ReferenceCompletenessAnalyzer:
    REQUIRED_GRAPHS = (
        "product",
        "journeys",
        "data_model",
        "api_contract",
        "auth_policy",
        "business_rules",
        "deployment",
        "observability",
        "acceptance",
    )

    def _finding(
        self,
        findings: list[CompletenessFinding],
        *,
        finding_id: str,
        path: str,
        status: str,
        severity: str,
        message: str,
        authority_refs: Iterable[str] = (),
        resumption: str | None = None,
    ) -> None:
        findings.append(
            CompletenessFinding(
                id=finding_id,
                path=path,
                status=status,
                severity=severity,
                message=message,
                authority_refs=list(authority_refs),
                exact_resumption_contract=resumption,
            )
        )

    def analyze(self, packet: ApplicationReferencePacket) -> ReferenceCompletenessReport:
        findings: list[CompletenessFinding] = []
        sources = {source.id: source for source in packet.sources}
        if len(sources) != len(packet.sources):
            self._finding(
                findings,
                finding_id="duplicate-source-id",
                path="sources",
                status="CONTRADICTORY",
                severity="P0",
                message="Reference source IDs must be unique.",
                resumption="Rename duplicate source IDs and update every authority reference.",
            )

        self._validate_graph_authority(packet, sources, findings)
        self._validate_source_classes(packet, sources, findings)
        self._validate_routes_and_journeys(packet, findings)
        self._validate_data_and_api(packet, findings)
        self._validate_auth(packet, findings)
        self._validate_rules_and_tests(packet, findings)
        self._validate_operational_authority(packet, findings)

        blocking = [
            finding
            for finding in findings
            if finding.status in {"MISSING", "CONTRADICTORY"} and finding.severity in {"P0", "P1"}
        ]
        hypotheses = [finding.path for finding in findings if finding.status == "HYPOTHETICAL"]
        counts = Counter(finding.status for finding in findings)
        authoritative = counts["AUTHORITATIVE"]
        total = sum(counts.values())
        return ReferenceCompletenessReport(
            packet_id=packet.packet_id,
            packet_sha256=packet.canonical_digest(),
            compilable_as_draft=not any(
                finding.severity == "P0" and finding.status in {"MISSING", "CONTRADICTORY"}
                for finding in findings
            ),
            promotable=not blocking and not hypotheses,
            findings=findings,
            missing_authority=[finding.path for finding in findings if finding.status == "MISSING"],
            hypotheses=hypotheses,
            contradictions=[
                finding.path for finding in findings if finding.status == "CONTRADICTORY"
            ],
            exact_resumption_contracts=[
                finding.exact_resumption_contract
                for finding in findings
                if finding.exact_resumption_contract
            ],
            authority_coverage={
                "authoritative_findings": float(authoritative),
                "total_findings": float(total),
                "fraction": authoritative / total if total else 1.0,
            },
        )

    def _validate_graph_authority(
        self,
        packet: ApplicationReferencePacket,
        sources: dict[str, object],
        findings: list[CompletenessFinding],
    ) -> None:
        for graph_name in self.REQUIRED_GRAPHS:
            graph = getattr(packet, graph_name)
            refs = graph.authority_refs
            if not refs:
                self._finding(
                    findings,
                    finding_id=f"{graph_name}-authority-missing",
                    path=f"{graph_name}.authority_refs",
                    status="MISSING",
                    severity="P0",
                    message=f"{graph_name} has no declared authority.",
                    resumption=f"Supply an authoritative source for {graph_name}.",
                )
                continue
            missing = [reference for reference in refs if reference not in sources]
            if missing:
                self._finding(
                    findings,
                    finding_id=f"{graph_name}-authority-unknown",
                    path=f"{graph_name}.authority_refs",
                    status="MISSING",
                    severity="P0",
                    message=f"{graph_name} references unknown sources: {missing}",
                    authority_refs=refs,
                    resumption=(
                        f"Add source records for {', '.join(missing)} or remove the invalid refs."
                    ),
                )
                continue
            classes = [sources[reference].authority for reference in refs]
            if AuthorityClass.HYPOTHESIS in classes:
                status = "HYPOTHETICAL"
                severity = "P1"
                message = f"{graph_name} contains explicitly hypothetical authority."
                resumption = (
                    f"Replace hypothetical {graph_name} authority with a supplied specification."
                )
            elif all(
                authority in {AuthorityClass.OBSERVED, AuthorityClass.SPECIFIED}
                for authority in classes
            ):
                status = "AUTHORITATIVE"
                severity = "P2"
                message = f"{graph_name} is bound to supplied authority."
                resumption = None
            else:
                status = "HYPOTHETICAL"
                severity = "P1"
                message = f"{graph_name} is derived without direct specification."
                resumption = f"Review and specify the derived {graph_name} before promotion."
            self._finding(
                findings,
                finding_id=f"{graph_name}-authority",
                path=f"{graph_name}.authority_refs",
                status=status,
                severity=severity,
                message=message,
                authority_refs=refs,
                resumption=resumption,
            )

    def _validate_reference_list(
        self,
        *,
        ids: list[str],
        expected_kind: str,
        path: str,
        sources: dict[str, object],
        findings: list[CompletenessFinding],
        required: bool,
    ) -> None:
        if required and not ids:
            self._finding(
                findings,
                finding_id=f"{path}-missing",
                path=path,
                status="MISSING",
                severity="P1",
                message=f"{path} requires at least one supplied reference.",
                resumption=f"Supply and register at least one {expected_kind} reference.",
            )
            return
        for reference_id in ids:
            source = sources.get(reference_id)
            if source is None:
                self._finding(
                    findings,
                    finding_id=f"{path}-{reference_id}-unknown",
                    path=path,
                    status="MISSING",
                    severity="P1",
                    message=f"Unknown reference {reference_id}.",
                    resumption=f"Register source {reference_id} as {expected_kind}.",
                )
            elif source.kind != expected_kind:
                self._finding(
                    findings,
                    finding_id=f"{path}-{reference_id}-wrong-kind",
                    path=path,
                    status="CONTRADICTORY",
                    severity="P1",
                    message=(
                        f"Reference {reference_id} is {source.kind}, expected {expected_kind}."
                    ),
                    authority_refs=[reference_id],
                    resumption=f"Correct the source kind for {reference_id}.",
                )

    def _validate_source_classes(
        self,
        packet: ApplicationReferencePacket,
        sources: dict[str, object],
        findings: list[CompletenessFinding],
    ) -> None:
        self._validate_reference_list(
            ids=packet.visual_reference_ids,
            expected_kind="visual_reference",
            path="visual_reference_ids",
            sources=sources,
            findings=findings,
            required=True,
        )
        self._validate_reference_list(
            ids=packet.motion_reference_ids,
            expected_kind="motion_reference",
            path="motion_reference_ids",
            sources=sources,
            findings=findings,
            required=False,
        )
        self._validate_reference_list(
            ids=packet.design_system_reference_ids,
            expected_kind="design_system_export",
            path="design_system_reference_ids",
            sources=sources,
            findings=findings,
            required=False,
        )
        self._validate_reference_list(
            ids=packet.source_code_reference_ids,
            expected_kind="source_code",
            path="source_code_reference_ids",
            sources=sources,
            findings=findings,
            required=False,
        )

    def _validate_routes_and_journeys(
        self,
        packet: ApplicationReferencePacket,
        findings: list[CompletenessFinding],
    ) -> None:
        route_ids = {route.id for route in packet.product.routes}
        if len(route_ids) != len(packet.product.routes):
            self._finding(
                findings,
                finding_id="duplicate-route-id",
                path="product.routes",
                status="CONTRADICTORY",
                severity="P0",
                message="Route IDs must be unique.",
                resumption="Rename duplicate route IDs and update journey/test references.",
            )
        if not packet.journeys.journeys:
            self._finding(
                findings,
                finding_id="journeys-empty",
                path="journeys.journeys",
                status="MISSING",
                severity="P0",
                message="No user journeys are specified.",
                resumption="Supply at least one success journey and its denied/failure paths.",
            )
        for journey in packet.journeys.journeys:
            for step in journey.steps:
                if step.route_id not in route_ids:
                    self._finding(
                        findings,
                        finding_id=f"journey-{journey.id}-route-{step.id}",
                        path=f"journeys.{journey.id}.{step.id}.route_id",
                        status="CONTRADICTORY",
                        severity="P0",
                        message=f"Journey step references unknown route {step.route_id}.",
                        resumption=f"Add route {step.route_id} or correct journey {journey.id}.",
                    )

    def _validate_data_and_api(
        self,
        packet: ApplicationReferencePacket,
        findings: list[CompletenessFinding],
    ) -> None:
        entities = {entity.name: entity for entity in packet.data_model.entities}
        if len(entities) != len(packet.data_model.entities):
            self._finding(
                findings,
                finding_id="duplicate-data-entity",
                path="data_model.entities",
                status="CONTRADICTORY",
                severity="P0",
                message="Data entity names must be unique.",
                resumption="Rename duplicate entities and update every API/relation binding.",
            )
        table_names = [entity.table_name for entity in packet.data_model.entities]
        if len(set(table_names)) != len(table_names):
            self._finding(
                findings,
                finding_id="duplicate-data-table",
                path="data_model.entities.table_name",
                status="CONTRADICTORY",
                severity="P0",
                message="Data table names must be unique.",
                resumption="Assign one table name per entity.",
            )
        operations = {endpoint.operation_id: endpoint for endpoint in packet.api_contract.endpoints}
        if len(operations) != len(packet.api_contract.endpoints):
            self._finding(
                findings,
                finding_id="duplicate-api-operation",
                path="api_contract.endpoints",
                status="CONTRADICTORY",
                severity="P0",
                message="API operation IDs must be unique.",
                resumption="Assign one stable unique operation ID to every endpoint.",
            )
        for endpoint in packet.api_contract.endpoints:
            missing = set(endpoint.entity_refs) - entities.keys()
            if missing:
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-unknown-entity",
                    path=f"api_contract.{endpoint.operation_id}.entity_refs",
                    status="CONTRADICTORY",
                    severity="P0",
                    message=f"Endpoint references unknown entities: {sorted(missing)}.",
                    resumption="Add the entities to DataModelGraph or correct entity_refs.",
                )
            if endpoint.handler.entity_ref not in entities:
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-unknown-handler-entity",
                    path=f"api_contract.{endpoint.operation_id}.handler.entity_ref",
                    status="CONTRADICTORY",
                    severity="P0",
                    message=(f"Handler references unknown entity {endpoint.handler.entity_ref}."),
                    resumption="Bind the handler to a declared DataModelGraph entity.",
                )
            elif endpoint.handler.entity_ref not in endpoint.entity_refs:
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-unbound-handler-entity",
                    path=f"api_contract.{endpoint.operation_id}.handler.entity_ref",
                    status="CONTRADICTORY",
                    severity="P0",
                    message="Handler entity is absent from the endpoint entity_refs.",
                    resumption="Add the handler entity to entity_refs.",
                )
            else:
                entity = entities[endpoint.handler.entity_ref]
                field_names = {field.name for field in entity.fields}
                request_names = {field.name for field in endpoint.request_fields}
                if endpoint.handler.kind == "file_upload":
                    request_names.update({"id", "content_type", "size_bytes", "storage_path"})
                if endpoint.handler.id_field not in field_names:
                    self._finding(
                        findings,
                        finding_id=f"api-{endpoint.operation_id}-unknown-id-field",
                        path=f"api_contract.{endpoint.operation_id}.handler.id_field",
                        status="CONTRADICTORY",
                        severity="P0",
                        message="Handler ID field is absent from its entity.",
                        resumption="Bind id_field to a declared entity field.",
                    )
                missing_sources = set(endpoint.handler.field_bindings.keys()) - request_names
                missing_targets = set(endpoint.handler.field_bindings.values()) - field_names
                if missing_sources or missing_targets:
                    self._finding(
                        findings,
                        finding_id=f"api-{endpoint.operation_id}-invalid-field-binding",
                        path=f"api_contract.{endpoint.operation_id}.handler.field_bindings",
                        status="CONTRADICTORY",
                        severity="P0",
                        message=(
                            f"Handler bindings have unknown request fields "
                            f"{sorted(missing_sources)} or entity fields "
                            f"{sorted(missing_targets)}."
                        ),
                        resumption=("Bind only declared request fields to declared entity fields."),
                    )
                if (
                    endpoint.handler.status_field
                    and endpoint.handler.status_field not in field_names
                ):
                    self._finding(
                        findings,
                        finding_id=f"api-{endpoint.operation_id}-unknown-status-field",
                        path=f"api_contract.{endpoint.operation_id}.handler.status_field",
                        status="CONTRADICTORY",
                        severity="P0",
                        message="Handler status field is absent from its entity.",
                        resumption="Bind status_field to a declared entity field.",
                    )
            if (
                endpoint.method in {"POST", "PUT", "PATCH"}
                and "reservation" in endpoint.operation_id.lower()
                and endpoint.idempotency is None
            ):
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-idempotency",
                    path=f"api_contract.{endpoint.operation_id}.idempotency",
                    status="MISSING",
                    severity="P0",
                    message="Reservation/purchase mutation lacks an idempotency contract.",
                    resumption=(
                        f"Specify key, scope, request-hash, replay, conflict, and retention "
                        f"semantics for {endpoint.operation_id}."
                    ),
                )
        for journey in packet.journeys.journeys:
            for step in journey.steps:
                if step.api_operation_id and step.api_operation_id not in operations:
                    self._finding(
                        findings,
                        finding_id=f"journey-{journey.id}-api-{step.id}",
                        path=f"journeys.{journey.id}.{step.id}.api_operation_id",
                        status="CONTRADICTORY",
                        severity="P0",
                        message=(
                            f"Journey step references unknown operation {step.api_operation_id}."
                        ),
                        resumption="Add the API operation or correct the journey binding.",
                    )

    def _validate_auth(
        self,
        packet: ApplicationReferencePacket,
        findings: list[CompletenessFinding],
    ) -> None:
        permissions = {permission.id for permission in packet.auth_policy.permissions}
        roles = {role.id for role in packet.auth_policy.roles}
        for role in packet.auth_policy.roles:
            missing = set(role.permission_ids) - permissions
            if missing:
                self._finding(
                    findings,
                    finding_id=f"role-{role.id}-unknown-permission",
                    path=f"auth_policy.roles.{role.id}",
                    status="CONTRADICTORY",
                    severity="P0",
                    message=f"Role references unknown permissions: {sorted(missing)}.",
                    resumption="Define the permissions or remove them from the role.",
                )
        if packet.auth_policy.default_role and packet.auth_policy.default_role not in roles:
            self._finding(
                findings,
                finding_id="auth-default-role-unknown",
                path="auth_policy.default_role",
                status="CONTRADICTORY",
                severity="P0",
                message="Default role is not declared.",
                resumption="Declare the default role or set it to null.",
            )
        for endpoint in packet.api_contract.endpoints:
            missing = set(endpoint.required_permissions) - permissions
            if missing:
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-unknown-permission",
                    path=f"api_contract.{endpoint.operation_id}.required_permissions",
                    status="CONTRADICTORY",
                    severity="P0",
                    message=f"Endpoint references unknown permissions: {sorted(missing)}.",
                    resumption="Define the permissions in AuthPolicyGraph.",
                )
            if (
                endpoint.method in {"POST", "PUT", "PATCH", "DELETE"}
                and endpoint.authorization == "public"
                and packet.auth_policy.deny_by_default
            ):
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-public-mutation-review",
                    path=f"api_contract.{endpoint.operation_id}.authorization",
                    status="HYPOTHETICAL",
                    severity="P1",
                    message="A public mutation conflicts with deny-by-default policy.",
                    resumption=(
                        f"Explicitly authorize public mutation {endpoint.operation_id} in the "
                        "supplied auth policy or require a role/permission."
                    ),
                )

    def _validate_rules_and_tests(
        self,
        packet: ApplicationReferencePacket,
        findings: list[CompletenessFinding],
    ) -> None:
        tests = {test.id: test for test in packet.acceptance.tests}
        rule_ids = {rule.id for rule in packet.business_rules.rules}
        for endpoint in packet.api_contract.endpoints:
            missing_rules = set(endpoint.business_rule_ids) - rule_ids
            if missing_rules:
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-missing-rules",
                    path=f"api_contract.{endpoint.operation_id}.business_rule_ids",
                    status="MISSING",
                    severity="P0",
                    message=f"Endpoint references missing business rules: {sorted(missing_rules)}.",
                    resumption="Supply every referenced BusinessRuleGraph rule.",
                )
            if (
                endpoint.method in {"POST", "PUT", "PATCH", "DELETE"}
                and not endpoint.business_rule_ids
            ):
                self._finding(
                    findings,
                    finding_id=f"api-{endpoint.operation_id}-rules-absent",
                    path=f"api_contract.{endpoint.operation_id}.business_rule_ids",
                    status="MISSING",
                    severity="P0",
                    message="Mutating endpoint has no machine-bound business-rule authority.",
                    resumption=(
                        f"Bind {endpoint.operation_id} to at least one supplied business rule."
                    ),
                )
        journey_ids = {journey.id for journey in packet.journeys.journeys}
        covered_journeys = {
            journey_id for test in packet.acceptance.tests for journey_id in test.journey_ids
        }
        for journey_id in sorted(journey_ids - covered_journeys):
            self._finding(
                findings,
                finding_id=f"journey-{journey_id}-unaccepted",
                path=f"acceptance.journey.{journey_id}",
                status="MISSING",
                severity="P1",
                message=f"Journey {journey_id} has no acceptance test.",
                resumption=f"Add an executable acceptance test for journey {journey_id}.",
            )
        for rule in packet.business_rules.rules:
            missing = set(rule.acceptance_test_ids) - tests.keys()
            if missing:
                self._finding(
                    findings,
                    finding_id=f"rule-{rule.id}-missing-tests",
                    path=f"business_rules.{rule.id}.acceptance_test_ids",
                    status="MISSING",
                    severity="P0",
                    message=f"Business rule references missing tests: {sorted(missing)}.",
                    resumption="Supply executable acceptance tests for every business rule.",
                )
        for error_case in packet.error_cases:
            if error_case.acceptance_test_id not in tests:
                self._finding(
                    findings,
                    finding_id=f"error-{error_case.id}-missing-test",
                    path=f"error_cases.{error_case.id}.acceptance_test_id",
                    status="MISSING",
                    severity="P1",
                    message="Error case lacks its declared executable test.",
                    resumption=f"Add acceptance test {error_case.acceptance_test_id}.",
                )

    def _validate_operational_authority(
        self,
        packet: ApplicationReferencePacket,
        findings: list[CompletenessFinding],
    ) -> None:
        requirements = (
            (
                "error_cases",
                packet.error_cases,
                "Supply an error and edge-case matrix with retry and user-state expectations.",
            ),
            (
                "performance_budgets",
                packet.performance_budgets,
                "Supply numeric performance budgets and measurement commands.",
            ),
            (
                "accessibility_requirements",
                packet.accessibility_requirements,
                "Supply accessibility criteria and test methods.",
            ),
            (
                "observability.slos",
                packet.observability.slos,
                "Supply at least one measurable SLO and alert policy.",
            ),
            (
                "deployment.nodes",
                packet.deployment.nodes,
                "Supply the declared deployment nodes, runtime, and health checks.",
            ),
        )
        for path, value, contract in requirements:
            if not value:
                self._finding(
                    findings,
                    finding_id=f"{path}-missing",
                    path=path,
                    status="MISSING",
                    severity="P1",
                    message=f"{path} is required for production promotion.",
                    resumption=contract,
                )
