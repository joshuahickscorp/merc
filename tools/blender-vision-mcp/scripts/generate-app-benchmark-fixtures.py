from __future__ import annotations

import argparse
import hashlib
import json
from copy import deepcopy
from pathlib import Path
from typing import Any

from blender_vision.app_build import ApplicationReferencePacket

ROOT = Path(__file__).resolve().parents[1]
BENCHMARK_ROOT = ROOT / "benchmarks" / "100_plus" / "app_build"
TEMPLATE_PATH = BENCHMARK_ROOT / "reference-template.json"


def _json(document: object) -> str:
    return json.dumps(document, indent=2, sort_keys=True) + "\n"


def _field(name: str, location: str, *, required: bool = True) -> dict[str, Any]:
    return {
        "name": name,
        "location": location,
        "data_type": "string",
        "required": required,
        "validation": {},
    }


def _response(status: int, description: str, schema_ref: str | None = None) -> dict[str, Any]:
    return {
        "status": status,
        "description": description,
        "schema_ref": schema_ref,
    }


def _handler(kind: str, *, status_field: str | None = None) -> dict[str, Any]:
    return {
        "kind": kind,
        "entity_ref": "Item",
        "id_field": "id",
        "status_field": status_field,
        "initial_status": None,
        "storage_subdirectory": None,
        "field_bindings": {},
    }


def _endpoint(
    operation_id: str,
    method: str,
    path: str,
    kind: str,
    *,
    permission: str | None = None,
    request_fields: list[dict[str, Any]] | None = None,
    rule_ids: list[str] | None = None,
) -> dict[str, Any]:
    return {
        "operation_id": operation_id,
        "method": method,
        "path": path,
        "summary": operation_id,
        "request_fields": request_fields or [],
        "responses": [_response(200, "Success", "Item")],
        "entity_refs": ["Item"],
        "business_rule_ids": rule_ids or [],
        "handler": _handler(kind),
        "authorization": "permission" if permission else "authenticated",
        "required_permissions": [permission] if permission else [],
        "idempotency": None,
        "file_boundary": None,
        "rate_limit": "120/minute/actor",
        "timeout_ms": 1000,
    }


def _rule(rule_id: str, description: str, test_id: str) -> dict[str, Any]:
    return {
        "id": rule_id,
        "description": description,
        "inputs": ["declared request fields"],
        "preconditions": ["authorized actor"],
        "deterministic_effect": description,
        "invariants": ["transactional persistence"],
        "retry_behavior": "safe after declared correction",
        "duplicate_behavior": "follows operation contract",
        "failure_behavior": "transaction rolls back",
        "acceptance_test_ids": [test_id],
    }


def _acceptance(
    test_id: str,
    requirement: str,
    operation_ids: list[str],
) -> dict[str, Any]:
    return {
        "id": test_id,
        "level": "api",
        "requirement": requirement,
        "command": "npm run test:api",
        "expected": requirement,
        "route_ids": ["items"],
        "journey_ids": ["create-item"],
        "operation_ids": operation_ids,
    }


def _permission(permission_id: str, actions: list[str]) -> dict[str, Any]:
    return {
        "id": permission_id,
        "description": f"Allows {permission_id}",
        "resource": "items",
        "actions": actions,
    }


def _identify(document: dict[str, Any], case_id: str, summary: str) -> None:
    document["packet_id"] = f"{case_id}-v1"
    document["product"]["id"] = case_id
    document["product"]["name"] = case_id.replace("-", " ").title()
    document["product"]["summary"] = summary


def _crud(template: dict[str, Any]) -> dict[str, Any]:
    document = deepcopy(template)
    _identify(document, "crud-relational", "Complete relational CRUD with rollback.")
    create = document["api_contract"]["endpoints"][1]
    create["business_rule_ids"] = ["item-create"]
    create["handler"]["kind"] = "create_entity"
    create["operation_id"] = "createItem"
    create["path"] = "/items"
    get_item = _endpoint(
        "getItem",
        "GET",
        "/items/{id}",
        "get_entity",
        request_fields=[_field("id", "path")],
    )
    update_item = _endpoint(
        "updateItem",
        "PATCH",
        "/items/{id}",
        "update_entity",
        permission="item.update",
        request_fields=[_field("id", "path"), _field("name", "body")],
        rule_ids=["item-update"],
    )
    update_item["handler"]["field_bindings"] = {"name": "name"}
    delete_item = _endpoint(
        "deleteItem",
        "DELETE",
        "/items/{id}",
        "delete_entity",
        permission="item.delete",
        request_fields=[_field("id", "path")],
        rule_ids=["item-delete"],
    )
    document["api_contract"]["endpoints"].extend([get_item, update_item, delete_item])
    document["auth_policy"]["permissions"].extend(
        [
            _permission("item.update", ["update"]),
            _permission("item.delete", ["delete"]),
        ]
    )
    document["auth_policy"]["roles"][0]["permission_ids"].extend(["item.update", "item.delete"])
    document["business_rules"]["rules"] = [
        _rule("item-create", "create exactly one valid item", "accept-crud"),
        _rule("item-update", "update only the addressed item", "accept-crud"),
        _rule("item-delete", "delete only the addressed item", "accept-crud"),
    ]
    document["acceptance"]["tests"] = [
        _acceptance(
            "accept-crud",
            "Create, list, get, update, delete, then observe a missing record.",
            ["createItem", "listItems", "getItem", "updateItem", "deleteItem"],
        ),
        _acceptance("accept-empty-name", "Reject an empty item name with 422.", ["createItem"]),
    ]
    return document


def _rbac(template: dict[str, Any]) -> dict[str, Any]:
    document = deepcopy(template)
    _identify(document, "rbac-denied-paths", "Deny-by-default role and permission enforcement.")
    document["api_contract"]["endpoints"][1]["business_rule_ids"] = ["item-name-required"]
    return document


def _idempotent(template: dict[str, Any]) -> dict[str, Any]:
    document = deepcopy(template)
    _identify(
        document,
        "idempotent-reservation",
        "Actor-scoped reservation with replay and conflict semantics.",
    )
    create = document["api_contract"]["endpoints"][1]
    create["operation_id"] = "createReservation"
    create["path"] = "/reservations"
    create["summary"] = "Create reservation exactly once"
    create["handler"]["kind"] = "idempotent_create"
    create["business_rule_ids"] = ["reservation-create"]
    create["idempotency"] = {
        "key_header": "idempotency-key",
        "scope": "actor",
        "request_hash_required": True,
        "replay_status": 200,
        "conflict_status": 409,
        "retention_seconds": 86400,
    }
    document["journeys"]["journeys"][0]["steps"][1]["api_operation_id"] = "createReservation"
    document["business_rules"]["rules"] = [
        _rule(
            "reservation-create",
            "persist one reservation for an actor/key/request hash",
            "accept-reservation-retry",
        )
    ]
    document["acceptance"]["tests"] = [
        _acceptance(
            "accept-reservation-retry",
            "Replay the same request and reject a changed request under the same key.",
            ["createReservation"],
        ),
        _acceptance(
            "accept-empty-name",
            "Reject an empty reservation name with 422.",
            ["createReservation"],
        ),
    ]
    return document


def _upload(template: dict[str, Any]) -> dict[str, Any]:
    document = deepcopy(template)
    _identify(
        document,
        "file-upload-boundaries",
        "Authorized upload with type, size, and confined storage boundaries.",
    )
    document["data_model"]["entities"] = [
        {
            "name": "Upload",
            "table_name": "uploads",
            "fields": [
                {
                    "name": "id",
                    "data_type": "uuid",
                    "nullable": False,
                    "primary_key": True,
                    "unique": True,
                    "default": None,
                    "validation": {},
                },
                {
                    "name": "content_type",
                    "data_type": "string",
                    "nullable": False,
                    "primary_key": False,
                    "unique": False,
                    "default": None,
                    "validation": {},
                },
                {
                    "name": "size_bytes",
                    "data_type": "integer",
                    "nullable": False,
                    "primary_key": False,
                    "unique": False,
                    "default": None,
                    "validation": {},
                },
                {
                    "name": "storage_path",
                    "data_type": "string",
                    "nullable": False,
                    "primary_key": False,
                    "unique": True,
                    "default": None,
                    "validation": {},
                },
                {
                    "name": "created_at",
                    "data_type": "datetime",
                    "nullable": False,
                    "primary_key": False,
                    "unique": False,
                    "default": None,
                    "validation": {},
                },
            ],
            "indexes": [["created_at"]],
            "retention_policy": "retain until explicit deletion",
        }
    ]
    upload = _endpoint(
        "uploadFile",
        "POST",
        "/uploads",
        "file_upload",
        permission="upload.create",
        rule_ids=["upload-boundary"],
    )
    upload["entity_refs"] = ["Upload"]
    upload["handler"] = {
        "kind": "file_upload",
        "entity_ref": "Upload",
        "id_field": "id",
        "status_field": None,
        "initial_status": None,
        "storage_subdirectory": "uploads",
        "field_bindings": {
            "id": "id",
            "content_type": "content_type",
            "size_bytes": "size_bytes",
            "storage_path": "storage_path",
        },
    }
    upload["responses"] = [
        _response(201, "Stored", "Upload"),
        _response(413, "Too large"),
        _response(415, "Unsupported content type"),
    ]
    upload["file_boundary"] = {
        "allowed_content_types": ["image/png"],
        "maximum_bytes": 64,
        "storage_policy": "confined generated application data directory",
        "malware_scan_policy": "benchmark fixture bytes only",
    }
    document["api_contract"]["endpoints"] = [upload]
    document["journeys"]["journeys"][0]["steps"][1]["api_operation_id"] = "uploadFile"
    document["auth_policy"]["permissions"] = [_permission("upload.create", ["create"])]
    document["auth_policy"]["roles"][0]["permission_ids"] = ["upload.create"]
    document["business_rules"]["rules"] = [
        _rule(
            "upload-boundary",
            "store only authorized in-boundary file bytes",
            "accept-upload-boundaries",
        )
    ]
    document["acceptance"]["tests"] = [
        _acceptance(
            "accept-upload-boundaries",
            "Accept PNG, reject wrong type, oversize payload, and unauthorized actor.",
            ["uploadFile"],
        )
    ]
    document["error_cases"] = [
        {
            "id": "upload-boundary-failure",
            "trigger": "payload exceeds size or type contract",
            "expected_user_state": "upload remains uncommitted and error is announced",
            "expected_api_status": 413,
            "retry_policy": "retry only with a compliant payload",
            "acceptance_test_id": "accept-upload-boundaries",
        }
    ]
    return document


def _status(template: dict[str, Any]) -> dict[str, Any]:
    document = deepcopy(template)
    _identify(
        document,
        "polling-status-recovery",
        "Create work and poll explicit status with missing-record recovery.",
    )
    status_field = {
        "name": "status",
        "data_type": "string",
        "nullable": False,
        "primary_key": False,
        "unique": False,
        "default": "queued",
        "validation": {},
    }
    document["data_model"]["entities"][0]["fields"].append(status_field)
    create = document["api_contract"]["endpoints"][1]
    create["operation_id"] = "createStatusJob"
    create["path"] = "/jobs"
    create["business_rule_ids"] = ["job-create"]
    create["handler"]["initial_status"] = "queued"
    status = _endpoint(
        "getJobStatus",
        "GET",
        "/jobs/{id}/status",
        "status_lookup",
        request_fields=[_field("id", "path")],
    )
    status["handler"]["status_field"] = "status"
    status["responses"] = [
        _response(200, "Current status", "Item"),
        _response(404, "Unknown job"),
    ]
    document["api_contract"]["endpoints"].append(status)
    document["journeys"]["journeys"][0]["steps"][1]["api_operation_id"] = "createStatusJob"
    document["journeys"]["journeys"][0]["steps"].append(
        {
            "id": "poll-status",
            "actor": "administrator",
            "route_id": "items",
            "action": "poll job status",
            "expected_state": "queued",
            "api_operation_id": "getJobStatus",
            "error_recovery": "show an explicit missing-job recovery action",
        }
    )
    document["business_rules"]["rules"] = [
        _rule("job-create", "create a job in queued status", "accept-status-recovery")
    ]
    document["acceptance"]["tests"] = [
        _acceptance(
            "accept-status-recovery",
            "Create a queued job, poll it, and recover explicitly from a missing ID.",
            ["createStatusJob", "getJobStatus"],
        ),
        _acceptance(
            "accept-empty-name",
            "Reject an empty job name with 422.",
            ["createStatusJob"],
        ),
    ]
    return document


def _materialize_sources(document: dict[str, Any], case_id: str) -> dict[str, bytes]:
    files: dict[str, bytes] = {}
    for source in document["sources"]:
        locator = Path("sources") / f"{source['id']}.json"
        payload = _json(
            {
                "authority": source["authority"],
                "benchmark_case": case_id,
                "kind": source["kind"],
                "rights_state": source["rights_state"],
                "source_id": source["id"],
            }
        ).encode()
        source["locator"] = locator.as_posix()
        source["digest"] = hashlib.sha256(payload).hexdigest()
        files[locator.as_posix()] = payload
    return files


def build_expected() -> dict[Path, bytes]:
    template = json.loads(TEMPLATE_PATH.read_text(encoding="utf-8"))
    builders = {
        "crud-relational": _crud,
        "rbac-denied-paths": _rbac,
        "idempotent-reservation": _idempotent,
        "file-upload-boundaries": _upload,
        "polling-status-recovery": _status,
    }
    files: dict[Path, bytes] = {}
    cases: list[dict[str, Any]] = []
    for case_id, builder in builders.items():
        document = builder(template)
        source_files = _materialize_sources(document, case_id)
        packet = ApplicationReferencePacket.model_validate(document)
        case_root = Path("cases") / case_id
        files[case_root / "packet.json"] = _json(document).encode()
        for relative, payload in source_files.items():
            files[case_root / relative] = payload
        cases.append(
            {
                "id": case_id,
                "packet": (case_root / "packet.json").as_posix(),
                "packet_sha256": packet.canonical_digest(),
                "required_handler_kinds": sorted(
                    {item.handler.kind for item in packet.api_contract.endpoints}
                ),
                "required_runtime_gates": [
                    "source_digest_verification",
                    "typescript_build",
                    "generated_api_tests",
                    "repeat_migration",
                    "rollback",
                    "restart_health",
                    "fresh_clone_reproduction",
                    "local_container_build",
                ],
            }
        )
    corpus_digest = hashlib.sha256(
        "\n".join(case["packet_sha256"] for case in cases).encode()
    ).hexdigest()
    manifest = {
        "schema_version": "1",
        "corpus_sha256": corpus_digest,
        "cases": cases,
    }
    files[Path("manifest.json")] = _json(manifest).encode()
    return files


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    expected = build_expected()
    failures: list[str] = []
    for relative, payload in expected.items():
        destination = BENCHMARK_ROOT / relative
        if args.check:
            if not destination.is_file() or destination.read_bytes() != payload:
                failures.append(relative.as_posix())
            continue
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(payload)
    if failures:
        raise SystemExit("stale application benchmark fixtures: " + ", ".join(failures))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
