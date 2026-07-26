from __future__ import annotations

import json
from copy import deepcopy
from pathlib import Path

from jsonschema import Draft202012Validator

from blender_vision.app_build import (
    ApplicationReferencePacket,
    ReferenceCompletenessAnalyzer,
)


def _source(
    source_id: str,
    kind: str,
    *,
    authority: str = "SPECIFIED",
) -> dict[str, object]:
    return {
        "id": source_id,
        "kind": kind,
        "authority": authority,
        "rights_state": "OWNED",
        "digest": (source_id.encode().hex() + "0" * 64)[:64],
        "locator": f"packet/{source_id}.json",
    }


def complete_packet_document() -> dict[str, object]:
    sources = [
        _source("brief", "product_brief"),
        _source("stories", "user_story"),
        _source("data", "data_schema"),
        _source("api", "api_contract"),
        _source("auth", "auth_policy"),
        _source("rules", "business_rules"),
        _source("errors", "error_matrix"),
        _source("deploy", "deployment_target"),
        _source("slo", "slo"),
        _source("performance", "performance_budget"),
        _source("accessibility", "accessibility_requirements"),
        _source("visual", "visual_reference", authority="OBSERVED"),
        _source("samples", "sample_payload"),
    ]
    return {
        "packet_id": "owned-crud-v1",
        "sources": sources,
        "product": {
            "id": "owned-crud",
            "name": "Owned CRUD",
            "version": "1.0.0",
            "summary": "Create and list governed items.",
            "actors": ["administrator"],
            "goals": ["manage items"],
            "non_goals": ["payments"],
            "routes": [
                {
                    "id": "items",
                    "path": "/items",
                    "title": "Items",
                    "purpose": "List and create items",
                    "required_states": ["loading", "empty", "ready", "error"],
                    "visual_reference_ids": ["visual"],
                }
            ],
            "authority_refs": ["brief"],
        },
        "journeys": {
            "journeys": [
                {
                    "id": "create-item",
                    "name": "Create item",
                    "actor": "administrator",
                    "preconditions": ["authenticated as administrator"],
                    "steps": [
                        {
                            "id": "open-items",
                            "actor": "administrator",
                            "route_id": "items",
                            "action": "open item management",
                            "expected_state": "ready",
                        },
                        {
                            "id": "submit-item",
                            "actor": "administrator",
                            "route_id": "items",
                            "action": "submit a valid item",
                            "expected_state": "item persisted",
                            "api_operation_id": "createItem",
                            "error_recovery": "retain fields and expose accessible error",
                        },
                    ],
                    "success_postconditions": ["item appears in the list"],
                    "denied_or_failure_paths": ["viewer receives 403"],
                }
            ],
            "authority_refs": ["stories"],
        },
        "data_model": {
            "database_engine": "sqlite",
            "entities": [
                {
                    "name": "Item",
                    "table_name": "items",
                    "fields": [
                        {
                            "name": "id",
                            "data_type": "uuid",
                            "primary_key": True,
                            "unique": True,
                        },
                        {
                            "name": "name",
                            "data_type": "string",
                            "validation": {"minimum_length": 1, "maximum_length": 120},
                        },
                        {
                            "name": "created_at",
                            "data_type": "datetime",
                        },
                    ],
                    "indexes": [["name"]],
                    "retention_policy": "retain until administrator deletion",
                }
            ],
            "relations": [],
            "seed_policy": "deterministic seed 41",
            "migration_policy": "numbered forward migrations in one transaction",
            "rollback_policy": "paired down migrations with backup",
            "authority_refs": ["data"],
        },
        "api_contract": {
            "protocol": "REST",
            "base_path": "/api",
            "version": "1",
            "endpoints": [
                {
                    "operation_id": "listItems",
                    "method": "GET",
                    "path": "/items",
                    "summary": "List items",
                    "request_fields": [],
                    "responses": [
                        {
                            "status": 200,
                            "description": "Item list",
                            "schema_ref": "Item[]",
                        }
                    ],
                    "entity_refs": ["Item"],
                    "authorization": "authenticated",
                    "rate_limit": "120/minute/actor",
                    "timeout_ms": 1000,
                },
                {
                    "operation_id": "createItem",
                    "method": "POST",
                    "path": "/items",
                    "summary": "Create item",
                    "request_fields": [
                        {
                            "name": "name",
                            "location": "body",
                            "data_type": "string",
                            "required": True,
                            "validation": {"minimum_length": 1, "maximum_length": 120},
                        }
                    ],
                    "responses": [
                        {
                            "status": 201,
                            "description": "Created item",
                            "schema_ref": "Item",
                        },
                        {
                            "status": 403,
                            "description": "Denied",
                        },
                    ],
                    "entity_refs": ["Item"],
                    "authorization": "permission",
                    "required_permissions": ["item.create"],
                    "rate_limit": "30/minute/actor",
                    "timeout_ms": 1000,
                },
            ],
            "error_envelope": {
                "code": "stable machine code",
                "message": "safe human message",
                "request_id": "trace identifier",
            },
            "sample_payload_refs": ["samples"],
            "authority_refs": ["api"],
        },
        "auth_policy": {
            "provider": "test_header",
            "identity_claim": "x-test-user",
            "roles": [
                {
                    "id": "administrator",
                    "description": "Manages items",
                    "permission_ids": ["item.create"],
                },
                {
                    "id": "viewer",
                    "description": "Reads items",
                    "permission_ids": [],
                },
            ],
            "permissions": [
                {
                    "id": "item.create",
                    "description": "Create items",
                    "resource": "Item",
                    "actions": ["create"],
                }
            ],
            "default_role": "viewer",
            "deny_by_default": True,
            "cross_tenant_policy": "no tenant context in this single-tenant benchmark",
            "credential_storage_policy": "test identities only; no production credentials",
            "authority_refs": ["auth"],
        },
        "business_rules": {
            "rules": [
                {
                    "id": "item-name-required",
                    "description": "Names are trimmed and non-empty.",
                    "inputs": ["name"],
                    "preconditions": ["administrator permission"],
                    "deterministic_effect": "persist exactly one item",
                    "invariants": ["empty names are never stored"],
                    "retry_behavior": "caller may retry after validation correction",
                    "duplicate_behavior": "duplicate names are allowed",
                    "failure_behavior": "transaction rolls back",
                    "acceptance_test_ids": ["accept-create-item"],
                }
            ],
            "edge_case_matrix": [
                {
                    "case": "empty name",
                    "expected": "422 and no row",
                }
            ],
            "analytics_events": [
                {
                    "event": "item_created",
                    "properties": "request_id only",
                }
            ],
            "authority_refs": ["rules", "errors"],
        },
        "deployment": {
            "target": "local_container",
            "nodes": [
                {
                    "id": "app",
                    "kind": "api",
                    "runtime": "node:20",
                    "artifact": "dist/server.js",
                    "health_check": "GET /healthz",
                    "resource_limits": {"memory": "256Mi", "cpu": "1"},
                },
                {
                    "id": "database",
                    "kind": "database",
                    "runtime": "sqlite3",
                    "artifact": "data/app.sqlite3",
                    "health_check": "PRAGMA quick_check",
                    "resource_limits": {"disk": "100Mi"},
                },
            ],
            "edges": [{"from": "app", "to": "database"}],
            "secret_names": [],
            "migration_command": "npm run db:migrate",
            "rollback_command": "npm run db:rollback",
            "fresh_clone_command": "npm ci && npm run verify",
            "release_command": "docker build .",
            "authority_refs": ["deploy"],
        },
        "observability": {
            "structured_log_fields": [
                "timestamp",
                "level",
                "request_id",
                "operation_id",
                "duration_ms",
                "status",
            ],
            "metrics": ["http_request_duration_ms", "http_errors_total"],
            "traces": ["request to database transaction"],
            "slos": [
                {
                    "id": "local-api-latency",
                    "indicator": "API p95",
                    "objective": "<=150ms",
                    "window": "benchmark run",
                    "alert": "fail acceptance",
                }
            ],
            "dashboards": ["local benchmark summary"],
            "alerts": ["API p95 budget failure"],
            "privacy_redactions": ["authorization", "cookie"],
            "authority_refs": ["slo"],
        },
        "acceptance": {
            "tests": [
                {
                    "id": "accept-create-item",
                    "level": "api",
                    "requirement": "Administrator creates one item and viewer is denied.",
                    "command": "npm run test:api",
                    "expected": "201 for administrator and 403 for viewer",
                    "route_ids": ["items"],
                    "journey_ids": ["create-item"],
                    "operation_ids": ["createItem"],
                },
                {
                    "id": "accept-empty-name",
                    "level": "api",
                    "requirement": "Empty item name is rejected.",
                    "command": "npm run test:api",
                    "expected": "422 and zero inserted rows",
                    "operation_ids": ["createItem"],
                },
            ],
            "global_regression_command": "npm run verify",
            "fresh_clone_command": "npm ci && npm run verify",
            "authority_refs": ["stories", "api", "rules"],
        },
        "error_cases": [
            {
                "id": "empty-item-name",
                "trigger": "name is empty after trimming",
                "expected_user_state": "form retains input and announces error",
                "expected_api_status": 422,
                "retry_policy": "retry after correction",
                "acceptance_test_id": "accept-empty-name",
            }
        ],
        "performance_budgets": [
            {
                "metric": "local_api_p95",
                "operation": "<=",
                "value": 150,
                "unit": "ms",
                "measurement_command": "npm run test:performance",
            }
        ],
        "accessibility_requirements": [
            {
                "id": "keyboard-create-item",
                "criterion": "Create journey works with keyboard alone",
                "level": "AA",
                "test_method": "Playwright keyboard journey",
            }
        ],
        "visual_reference_ids": ["visual"],
        "motion_reference_ids": [],
        "design_system_reference_ids": [],
    }


def test_complete_packet_is_promotable_and_digest_stable() -> None:
    packet = ApplicationReferencePacket.model_validate(complete_packet_document())
    report = ReferenceCompletenessAnalyzer().analyze(packet)
    second = ApplicationReferencePacket.model_validate(deepcopy(complete_packet_document()))

    assert report.compilable_as_draft is True
    assert report.promotable is True
    assert report.missing_authority == []
    assert report.hypotheses == []
    assert report.contradictions == []
    assert packet.canonical_digest() == second.canonical_digest()
    assert report.packet_sha256 == packet.canonical_digest()


def test_hypothesis_can_compile_only_as_unpromotable_draft() -> None:
    document = complete_packet_document()
    auth_source = next(item for item in document["sources"] if item["id"] == "auth")
    auth_source["authority"] = "HYPOTHESIS"
    packet = ApplicationReferencePacket.model_validate(document)

    report = ReferenceCompletenessAnalyzer().analyze(packet)

    assert report.compilable_as_draft is True
    assert report.promotable is False
    assert "auth_policy.authority_refs" in report.hypotheses
    assert any(
        "Replace hypothetical auth_policy" in item for item in report.exact_resumption_contracts
    )


def test_missing_graph_authority_blocks_draft_compilation() -> None:
    document = complete_packet_document()
    document["business_rules"]["authority_refs"] = []
    packet = ApplicationReferencePacket.model_validate(document)

    report = ReferenceCompletenessAnalyzer().analyze(packet)

    assert report.compilable_as_draft is False
    assert report.promotable is False
    assert "business_rules.authority_refs" in report.missing_authority


def test_unknown_entity_and_journey_operation_are_contradictions() -> None:
    document = complete_packet_document()
    document["api_contract"]["endpoints"][0]["entity_refs"] = ["MissingEntity"]
    document["journeys"]["journeys"][0]["steps"][1]["api_operation_id"] = "missingOperation"
    packet = ApplicationReferencePacket.model_validate(document)

    report = ReferenceCompletenessAnalyzer().analyze(packet)

    assert report.compilable_as_draft is False
    assert report.promotable is False
    assert any("entity_refs" in item for item in report.contradictions)
    assert any("api_operation_id" in item for item in report.contradictions)


def test_reservation_mutation_requires_explicit_idempotency_contract() -> None:
    document = complete_packet_document()
    document["api_contract"]["endpoints"][1]["operation_id"] = "createReservation"
    document["journeys"]["journeys"][0]["steps"][1]["api_operation_id"] = "createReservation"
    packet = ApplicationReferencePacket.model_validate(document)

    report = ReferenceCompletenessAnalyzer().analyze(packet)

    assert report.compilable_as_draft is False
    assert "api_contract.createReservation.idempotency" in report.missing_authority
    assert any("request-hash" in item for item in report.exact_resumption_contracts)


def test_public_mutation_under_deny_by_default_is_hypothetical() -> None:
    document = complete_packet_document()
    endpoint = document["api_contract"]["endpoints"][1]
    endpoint["authorization"] = "public"
    endpoint["required_permissions"] = []
    packet = ApplicationReferencePacket.model_validate(document)

    report = ReferenceCompletenessAnalyzer().analyze(packet)

    assert report.compilable_as_draft is True
    assert report.promotable is False
    assert "api_contract.createItem.authorization" in report.hypotheses


def test_packet_and_report_match_exported_schemas() -> None:
    packet = ApplicationReferencePacket.model_validate(complete_packet_document())
    report = ReferenceCompletenessAnalyzer().analyze(packet)
    schemas = Path(__file__).parents[1] / "schemas"

    packet_schema = json.loads(
        (schemas / "application-reference-packet.schema.json").read_text(encoding="utf-8")
    )
    report_schema = json.loads(
        (schemas / "reference-completeness-report.schema.json").read_text(encoding="utf-8")
    )
    Draft202012Validator(packet_schema).validate(packet.model_dump(mode="json"))
    Draft202012Validator(report_schema).validate(report.model_dump(mode="json"))
