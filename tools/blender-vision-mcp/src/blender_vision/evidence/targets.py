from __future__ import annotations

import json
import re
import uuid
from datetime import date
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import (
    atomic_write_json,
    canonical_json,
    sha256_file,
    utc_now,
)
from blender_vision.projects.store import ProjectStore

KNOWN_MANUFACTURERS = (
    "Porsche",
    "Apple",
    "NVIDIA",
    "AMD",
    "Intel",
    "Tesla",
    "BMW",
    "Mercedes-Benz",
    "Audi",
    "Ferrari",
    "Lamborghini",
)

REQUEST_CLASSES = {
    "GENERATIVE_DESIGN": "GENERATED DESIGN",
    "REFERENCE_RECONSTRUCTION": "REFERENCE RECONSTRUCTION",
    "AUTONOMOUS_PUBLIC_EVIDENCE": "AUTONOMOUS EVIDENCE-BASED RECONSTRUCTION",
}


def _text(value: Any, label: str, *, maximum: int) -> str:
    normalized = str(value or "").strip()
    if (
        not normalized
        or len(normalized) > maximum
        or any(ord(character) < 32 for character in normalized)
    ):
        raise ValueError(f"{label} must be non-empty printable text")
    return normalized


class TargetResolver:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def resolve(
        self,
        request: str | dict[str, Any],
        *,
        request_class: str = "AUTONOMOUS_PUBLIC_EVIDENCE",
        requested_tier: str = "L3",
        configuration: str = "factory standard unless evidence specifies otherwise",
        market: str = "unspecified",
        alternatives: list[dict[str, Any]] | None = None,
        evidence_cutoff_date: str | None = None,
    ) -> dict[str, Any]:
        if request_class not in REQUEST_CLASSES:
            raise ValueError("unsupported reconstruction request class")
        if requested_tier not in {f"L{index}" for index in range(6)}:
            raise ValueError("requested accuracy tier must be L0 through L5")
        if isinstance(request, dict):
            target = dict(request)
            request_text = _text(
                json.dumps(request, sort_keys=True), "target request", maximum=10000
            )
        else:
            request_text = _text(request, "target request", maximum=10000)
            target = self._parse(request_text)
        target.setdefault("manufacturer", None)
        target.setdefault("model", None)
        target.setdefault("product_family", target.get("model"))
        target.setdefault("generation", None)
        target.setdefault("model_year", None)
        target.setdefault("trim", None)
        target.setdefault("body_style", None)
        target.setdefault("market", market)
        target.setdefault("regional_version", market)
        target.setdefault("configuration", configuration)
        target.setdefault("desired_configuration", configuration)
        target.setdefault("factory_options", [])
        target.setdefault("wheel_option", None)
        target.setdefault("aero_package", None)
        target.setdefault("color_material_specification", None)
        with self.project.connection() as connection:
            previous_target_row = connection.execute(
                "SELECT id,target_json FROM target_resolutions ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            previous_event_row = connection.execute(
                "SELECT receipt_digest FROM target_resolution_events "
                "ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
        if previous_target_row is not None and previous_event_row is None:
            raise ValueError(
                "existing canonical target lacks a resolution receipt; migrate it first"
            )
        if previous_target_row is not None:
            previous_authority = self.authority_status(previous_target_row["id"])
            if not previous_authority["valid"]:
                raise ValueError(
                    previous_authority["error"]
                    or "existing canonical target resolution is invalid"
                )
        previous_revision = (
            int(json.loads(previous_target_row["target_json"]).get("revision", 0))
            if previous_target_row
            else 0
        )
        target["requested_tier"] = requested_tier
        target["output_classification"] = REQUEST_CLASSES[request_class]
        target["evidence_cutoff_date"] = evidence_cutoff_date or utc_now()[:10]
        target["revision"] = previous_revision + 1
        alternatives = alternatives or []
        material_alternatives = [item for item in alternatives if item.get("geometry_changes")]
        missing = [key for key in ("manufacturer", "model") if not target.get(key)]
        ambiguity = {
            "material": bool(material_alternatives),
            "missing_identity_fields": missing,
            "requires_clarification": bool(material_alternatives or missing),
            "question": (
                "Which materially different target variant should be reconstructed?"
                if material_alternatives
                else "What manufacturer and model should be reconstructed?"
                if missing
                else None
            ),
        }
        status = "NEEDS_CLARIFICATION" if ambiguity["requires_clarification"] else "RESOLVED"
        target_id = str(uuid.uuid4())
        created_at = utc_now()
        record = {
            "schema_version": 1,
            "id": target_id,
            "request_text": request_text,
            "target": target,
            "alternatives": alternatives,
            "ambiguity": ambiguity,
            "status": status,
            "created_at": created_at,
        }
        event_id = str(uuid.uuid4())
        previous_digest = (
            previous_event_row["receipt_digest"] if previous_event_row else None
        )
        receipt = {
            "schema_version": 1,
            "receipt_type": "target_resolution",
            "id": event_id,
            "target_id": target_id,
            "request_text": request_text,
            "target": target,
            "alternatives": alternatives,
            "ambiguity": ambiguity,
            "status": status,
            "created_at": created_at,
            "supersedes_receipt_digest": previous_digest,
            "authority": (
                "DETERMINISTIC_CANONICAL_TARGET_RESOLUTION"
                if status == "RESOLVED"
                else "TARGET_PROPOSAL_REQUIRES_CLARIFICATION"
            ),
        }
        relative = Path("receipts") / f"target-resolution-{event_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.target-resolution+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current_target = connection.execute(
                "SELECT id FROM target_resolutions ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            current_event = connection.execute(
                "SELECT receipt_digest FROM target_resolution_events "
                "ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            if (current_target["id"] if current_target else None) != (
                previous_target_row["id"] if previous_target_row else None
            ) or (current_event["receipt_digest"] if current_event else None) != previous_digest:
                raise RuntimeError("canonical target changed during resolution")
            connection.execute(
                "INSERT INTO target_resolutions(id,request_text,target_json,alternatives_json,"
                "ambiguity_json,status,created_at) VALUES(?,?,?,?,?,?,?)",
                (
                    target_id,
                    request_text,
                    json.dumps(target),
                    json.dumps(alternatives),
                    json.dumps(ambiguity),
                    status,
                    created_at,
                ),
            )
            connection.execute(
                "INSERT INTO target_resolution_events("
                "id,target_id,receipt_digest,supersedes_receipt_digest,created_at) "
                "VALUES(?,?,?,?,?)",
                (event_id, target_id, artifact.digest, previous_digest, created_at),
            )
        atomic_write_json(self.project.root / "target.json", record)
        project = self.project.project()
        project["metadata"] = {**project.get("metadata", {}), "canonical_target_id": target_id}
        project["updated_at"] = created_at
        atomic_write_json(self.project.project_file, project)
        return {
            **record,
            "receipt_digest": artifact.digest,
            "authority": receipt["authority"],
        }

    def _record(self, target_id: str | None = None) -> dict[str, Any]:
        with self.project.connection() as connection:
            if target_id:
                row = connection.execute(
                    "SELECT * FROM target_resolutions WHERE id=?", (target_id,)
                ).fetchone()
            else:
                row = connection.execute(
                    "SELECT * FROM target_resolutions ORDER BY rowid DESC LIMIT 1"
                ).fetchone()
        if row is None:
            raise FileNotFoundError("project has no resolved target")
        value = dict(row)
        value["target"] = json.loads(value.pop("target_json"))
        value["alternatives"] = json.loads(value.pop("alternatives_json"))
        value["ambiguity"] = json.loads(value.pop("ambiguity_json"))
        return value

    def get(
        self, target_id: str | None = None, *, verify: bool = True
    ) -> dict[str, Any]:
        value = self._record(target_id)
        if verify:
            authority = self.authority_status(value["id"])
            if not authority["valid"]:
                raise ValueError(
                    authority["error"] or "canonical target resolution is invalid"
                )
            value["receipt_digest"] = authority["receipt_digest"]
            value["authority"] = authority["authority"]
        return value

    @staticmethod
    def _semantics_error(record: dict[str, Any]) -> str | None:
        try:
            request_text = _text(record.get("request_text"), "target request", maximum=10000)
        except ValueError as error:
            return str(error)
        if request_text != record.get("request_text"):
            return "target request text is not canonical"
        target = record.get("target")
        alternatives = record.get("alternatives")
        ambiguity = record.get("ambiguity")
        if not isinstance(target, dict) or not isinstance(alternatives, list):
            return "target resolution payload is malformed"
        if any(not isinstance(item, dict) for item in alternatives):
            return "target alternatives must be objects"
        if not isinstance(ambiguity, dict) or set(ambiguity) != {
            "material",
            "missing_identity_fields",
            "requires_clarification",
            "question",
        }:
            return "target ambiguity schema is invalid"
        revision = target.get("revision")
        if isinstance(revision, bool) or not isinstance(revision, int) or revision < 1:
            return "target revision must be a positive integer"
        if target.get("requested_tier") not in {f"L{index}" for index in range(6)}:
            return "target requested tier is invalid"
        if target.get("output_classification") not in set(REQUEST_CLASSES.values()):
            return "target output classification is invalid"
        cutoff = target.get("evidence_cutoff_date")
        if not isinstance(cutoff, str) or re.fullmatch(r"\d{4}-\d{2}-\d{2}", cutoff) is None:
            return "target evidence cutoff date is invalid"
        try:
            date.fromisoformat(cutoff)
        except ValueError:
            return "target evidence cutoff date is invalid"
        missing = [
            field for field in ("manufacturer", "model") if not target.get(field)
        ]
        material = any(item.get("geometry_changes") is True for item in alternatives)
        requires = bool(material or missing)
        expected_ambiguity = {
            "material": material,
            "missing_identity_fields": missing,
            "requires_clarification": requires,
            "question": (
                "Which materially different target variant should be reconstructed?"
                if material
                else "What manufacturer and model should be reconstructed?"
                if missing
                else None
            ),
        }
        expected_status = "NEEDS_CLARIFICATION" if requires else "RESOLVED"
        if canonical_json(ambiguity) != canonical_json(expected_ambiguity):
            return "target ambiguity does not replay from identity and alternatives"
        if record.get("status") != expected_status:
            return "target resolution status is inconsistent with ambiguity"
        if not str(record.get("created_at", "")).strip():
            return "target resolution time is missing"
        return None

    def authority_status(self, target_id: str) -> dict[str, Any]:
        try:
            record = self._record(target_id)
        except (FileNotFoundError, json.JSONDecodeError, TypeError) as error:
            return {
                "target_id": target_id,
                "valid": False,
                "receipt_digest": None,
                "authority": None,
                "error": str(error),
            }
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT rowid AS ledger_sequence,* FROM target_resolution_events "
                "WHERE target_id=?",
                (target_id,),
            ).fetchone()
        result = {
            "target_id": target_id,
            "valid": False,
            "receipt_digest": row["receipt_digest"] if row else None,
            "authority": None,
            "error": None,
        }
        if row is None:
            result["error"] = "canonical target has no authoritative resolution receipt"
            return result
        try:
            path = self.artifacts.path_for(row["receipt_digest"])
            if not path.is_file() or sha256_file(path)[0] != row["receipt_digest"]:
                raise ValueError("target resolution receipt is missing or corrupt")
            receipt = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(receipt, dict):
                raise ValueError("target resolution receipt must be an object")
            base_fields = {
                "schema_version",
                "receipt_type",
                "id",
                "target_id",
                "request_text",
                "target",
                "alternatives",
                "ambiguity",
                "status",
                "created_at",
                "supersedes_receipt_digest",
                "authority",
            }
            schema_version = receipt.get("schema_version")
            migration_valid = schema_version == 1
            if schema_version == 2:
                base_fields.add("migration")
                migration = receipt.get("migration")
                migration_valid = bool(
                    isinstance(migration, dict)
                    and set(migration)
                    == {"kind", "legacy_target_record", "new_resolution_performed"}
                    and migration.get("kind")
                    == "legacy_canonical_target_receipt_migration"
                    and migration.get("new_resolution_performed") is False
                    and canonical_json(migration.get("legacy_target_record"))
                    == canonical_json(record)
                )
            semantics_error = self._semantics_error(record)
            if semantics_error:
                raise ValueError(semantics_error)
            with self.project.connection() as connection:
                previous = connection.execute(
                    "SELECT e.receipt_digest,t.target_json FROM target_resolution_events e "
                    "JOIN target_resolutions t ON t.id=e.target_id "
                    "WHERE e.rowid<? ORDER BY e.rowid DESC LIMIT 1",
                    (row["ledger_sequence"],),
                ).fetchone()
            expected_supersedes = previous["receipt_digest"] if previous else None
            revision_valid = True
            if schema_version == 1:
                previous_revision = (
                    int(json.loads(previous["target_json"]).get("revision", 0))
                    if previous
                    else 0
                )
                revision_valid = record["target"].get("revision") == previous_revision + 1
            expected_authority = (
                "MIGRATED_CANONICAL_TARGET_RESOLUTION"
                if schema_version == 2
                else "DETERMINISTIC_CANONICAL_TARGET_RESOLUTION"
                if record["status"] == "RESOLVED"
                else "TARGET_PROPOSAL_REQUIRES_CLARIFICATION"
            )
            valid = bool(
                set(receipt) == base_fields
                and schema_version in {1, 2}
                and receipt.get("receipt_type") == "target_resolution"
                and receipt.get("id") == row["id"]
                and receipt.get("target_id") == target_id
                and receipt.get("request_text") == record["request_text"]
                and canonical_json(receipt.get("target"))
                == canonical_json(record["target"])
                and canonical_json(receipt.get("alternatives"))
                == canonical_json(record["alternatives"])
                and canonical_json(receipt.get("ambiguity"))
                == canonical_json(record["ambiguity"])
                and receipt.get("status") == record["status"]
                and receipt.get("created_at") == record["created_at"]
                and row["created_at"] == record["created_at"]
                and receipt.get("supersedes_receipt_digest") == expected_supersedes
                and row["supersedes_receipt_digest"] == expected_supersedes
                and receipt.get("authority") == expected_authority
                and migration_valid
                and revision_valid
            )
            result["valid"] = valid
            result["authority"] = receipt.get("authority")
            if not valid:
                result["error"] = "target resolution receipt semantics are inconsistent"
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as error:
            result["error"] = str(error)
        return result

    def migrate_legacy_authority(self, target_id: str) -> dict[str, Any]:
        record = self._record(target_id)
        if self._semantics_error(record):
            raise ValueError(
                f"legacy target cannot be migrated: {self._semantics_error(record)}"
            )
        with self.project.connection() as connection:
            target_row = connection.execute(
                "SELECT rowid FROM target_resolutions WHERE id=?", (target_id,)
            ).fetchone()
            existing = connection.execute(
                "SELECT 1 FROM target_resolution_events WHERE target_id=?", (target_id,)
            ).fetchone()
            earlier_unmigrated = connection.execute(
                "SELECT 1 FROM target_resolutions t WHERE t.rowid<? AND NOT EXISTS("
                "SELECT 1 FROM target_resolution_events e WHERE e.target_id=t.id) LIMIT 1",
                (target_row["rowid"],),
            ).fetchone()
            previous = connection.execute(
                "SELECT receipt_digest FROM target_resolution_events "
                "ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
        if existing:
            raise ValueError("legacy target already has a resolution receipt")
        if earlier_unmigrated:
            raise ValueError("legacy targets must be migrated in creation order")
        previous_digest = previous["receipt_digest"] if previous else None
        event_id = str(uuid.uuid4())
        migration = {
            "kind": "legacy_canonical_target_receipt_migration",
            "legacy_target_record": record,
            "new_resolution_performed": False,
        }
        receipt = {
            "schema_version": 2,
            "receipt_type": "target_resolution",
            "id": event_id,
            "target_id": target_id,
            "request_text": record["request_text"],
            "target": record["target"],
            "alternatives": record["alternatives"],
            "ambiguity": record["ambiguity"],
            "status": record["status"],
            "created_at": record["created_at"],
            "supersedes_receipt_digest": previous_digest,
            "authority": "MIGRATED_CANONICAL_TARGET_RESOLUTION",
            "migration": migration,
        }
        relative = Path("receipts") / f"target-resolution-migration-{event_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.target-resolution+json",
        )
        expected = canonical_json(record)
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT * FROM target_resolutions WHERE id=?", (target_id,)
            ).fetchone()
            current_record = dict(current) if current else None
            if current_record:
                current_record["target"] = json.loads(current_record.pop("target_json"))
                current_record["alternatives"] = json.loads(
                    current_record.pop("alternatives_json")
                )
                current_record["ambiguity"] = json.loads(
                    current_record.pop("ambiguity_json")
                )
            current_previous = connection.execute(
                "SELECT receipt_digest FROM target_resolution_events "
                "ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            if (
                current_record is None
                or canonical_json(current_record) != expected
                or connection.execute(
                    "SELECT 1 FROM target_resolution_events WHERE target_id=?", (target_id,)
                ).fetchone()
                or (current_previous["receipt_digest"] if current_previous else None)
                != previous_digest
            ):
                raise RuntimeError("legacy target changed during authority migration")
            connection.execute(
                "INSERT INTO target_resolution_events("
                "id,target_id,receipt_digest,supersedes_receipt_digest,created_at) "
                "VALUES(?,?,?,?,?)",
                (event_id, target_id, artifact.digest, previous_digest, record["created_at"]),
            )
        return {
            "target_id": target_id,
            "receipt_digest": artifact.digest,
            "migration": migration,
            "authority": self.authority_status(target_id),
        }

    @staticmethod
    def _parse(request: str) -> dict[str, Any]:
        year_match = re.search(r"\b(19|20)\d{2}\b", request)
        manufacturer = next(
            (name for name in KNOWN_MANUFACTURERS if name.lower() in request.lower()), None
        )
        model = None
        if manufacturer:
            tail = re.split(re.escape(manufacturer), request, flags=re.IGNORECASE, maxsplit=1)[-1]
            tail = re.sub(r"\b(19|20)\d{2}\b", "", tail)
            tail = re.sub(
                r"\b(make|create|build|reconstruct|model|digital twin|of|an?|the|l[0-5])\b",
                " ",
                tail,
                flags=re.IGNORECASE,
            )
            model = " ".join(tail.strip(" .,-").split()) or None
        return {
            "manufacturer": manufacturer,
            "model": model,
            "model_year": int(year_match.group()) if year_match else None,
        }
