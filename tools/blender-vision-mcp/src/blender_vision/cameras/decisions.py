from __future__ import annotations

import hashlib
import json
import uuid
from copy import deepcopy
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.state import (
    REQUIRED_CAMERA_STATE_FIELDS,
    complete_camera_state,
    validate_complete_camera_state,
)
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore


class CameraDecisionStore:
    """Persist and verify immutable named camera approval/rejection decisions."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def record(
        self,
        solution_id: str,
        *,
        document: dict[str, Any],
        state: str,
        reviewer: str,
        reason: str,
        _reviewed_at: str | None = None,
        _migration: dict[str, Any] | None = None,
        _expected_document: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        normalized_state = str(state).lower()
        if normalized_state not in {"approved", "rejected"}:
            raise ValueError("camera decision must be approved or rejected")
        reviewer_name = str(reviewer).strip()
        review_reason = str(reason).strip()
        if not reviewer_name or not review_reason:
            raise ValueError("camera decision requires a named reviewer and reason")
        cameras = document.get("cameras", [])
        if not isinstance(cameras, list) or not cameras:
            raise ValueError("camera decision requires a non-empty solution")
        for camera in cameras:
            validate_complete_camera_state(camera)
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json,decision_id,decision_digest FROM camera_solutions WHERE id=?",
                (solution_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        persisted = json.loads(row["solution_json"])
        expected_document = _expected_document or document
        if canonical_json(persisted) != canonical_json(expected_document):
            raise RuntimeError("camera solution changed before its decision was recorded")
        prior_approval = deepcopy(document.get("approval"))
        prior_approved = bool(document.get("approved"))
        decision_id = str(uuid.uuid4())
        reviewed_at = _reviewed_at or utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "camera_solution_review",
            "id": decision_id,
            "solution_id": solution_id,
            "solution_snapshot_sha256": hashlib.sha256(canonical_json(document)).hexdigest(),
            "camera_immutable_sha256s": [
                str(camera.get("immutable_sha256")) for camera in cameras
            ],
            "reference_ids": sorted(str(camera["reference_id"]) for camera in cameras),
            "registration_classes": sorted(
                {str(camera.get("registration_class")) for camera in cameras}
            ),
            "prior_approved": prior_approved,
            "prior_approval": prior_approval,
            "supersedes_decision_digest": row["decision_digest"],
            "decision": normalized_state,
            "reviewer": reviewer_name,
            "reason": review_reason,
            "acceptance_performed": False,
            "reviewed_at": reviewed_at,
        }
        if _migration:
            receipt["migration"] = _migration
        relative = Path("receipts") / f"camera-review-{decision_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.camera-solution-review+json",
        )
        updated_document = deepcopy(document)
        updated_document["approved"] = normalized_state == "approved"
        updated_document["approval"] = {
            "state": normalized_state,
            "reviewer": reviewer_name,
            "reason": review_reason,
            "reviewed_at": reviewed_at,
            "decision_id": decision_id,
            "decision_digest": artifact.digest,
        }
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT solution_json,decision_id,decision_digest FROM camera_solutions WHERE id=?",
                (solution_id,),
            ).fetchone()
            if (
                current is None
                or canonical_json(json.loads(current["solution_json"]))
                != canonical_json(expected_document)
                or current["decision_id"] != row["decision_id"]
                or current["decision_digest"] != row["decision_digest"]
            ):
                raise RuntimeError("camera decision raced with another update")
            connection.execute(
                "INSERT INTO camera_decisions(id,solution_id,state,decision_json,"
                "decision_digest,created_at) VALUES(?,?,?,?,?,?)",
                (
                    decision_id,
                    solution_id,
                    normalized_state,
                    json.dumps(receipt),
                    artifact.digest,
                    reviewed_at,
                ),
            )
            updated = connection.execute(
                "UPDATE camera_solutions SET solution_json=?,approved=?,decision_id=?,"
                "decision_digest=? WHERE id=?",
                (
                    json.dumps(updated_document),
                    int(normalized_state == "approved"),
                    decision_id,
                    artifact.digest,
                    solution_id,
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("camera decision failed to update its solution")
        destination = self.project.root / "cameras" / f"solution_{solution_id}.json"
        atomic_write_json(destination, updated_document)
        return {
            **updated_document,
            "path": str(destination.relative_to(self.project.root)),
            "decision": receipt,
            "decision_artifact": artifact.to_dict(),
        }

    def prepare_for_decision(self, solution_id: str) -> dict[str, Any]:
        """Complete an old pending snapshot without changing its reviewed geometry."""
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT backend,approved,solution_json,decision_id,decision_digest "
                "FROM camera_solutions WHERE id=?",
                (solution_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        document = json.loads(row["solution_json"])
        cameras = document.get("cameras", [])
        incomplete = any(REQUIRED_CAMERA_STATE_FIELDS - set(camera) for camera in cameras)
        if incomplete:
            approval = document.get("approval", {})
            if (
                bool(row["approved"])
                or row["decision_id"]
                or row["decision_digest"]
                or approval.get("state") != "pending"
            ):
                raise ValueError(
                    "only an undecided legacy camera snapshot can be completed before review"
                )
            legacy_document = deepcopy(document)
            completed_document, completion_records = self._complete_legacy_document(
                document, backend=str(row["backend"])
            )
            return {
                "document": completed_document,
                "expected_document": legacy_document,
                "migration": self._migration_record(
                    legacy_document,
                    completed_document,
                    completion_records,
                    authority="DETERMINISTIC_PENDING_SNAPSHOT_COMPLETION",
                    new_human_review_performed=True,
                ),
            }
        if not cameras:
            raise ValueError("camera decision requires a non-empty solution")
        for camera in cameras:
            validate_complete_camera_state(camera)
        return {"document": document, "expected_document": None, "migration": None}

    def migrate_legacy(self, solution_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT backend,solution_json,decision_digest FROM camera_solutions WHERE id=?",
                (solution_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        if row["decision_digest"]:
            return self.verify(solution_id)
        document = json.loads(row["solution_json"])
        approval = document.get("approval", {})
        state = approval.get("state")
        if state not in {"approved", "rejected"}:
            raise ValueError("only an existing named camera decision can be migrated")
        if not all(
            str(approval.get(field, "")).strip()
            for field in ("reviewer", "reason", "reviewed_at")
        ):
            raise ValueError("legacy camera decision lacks reviewer, reason, or timestamp")
        legacy_document = deepcopy(document)
        completed_document, completion_records = self._complete_legacy_document(
            document, backend=str(row["backend"])
        )
        return self.record(
            solution_id,
            document=completed_document,
            state=state,
            reviewer=str(approval["reviewer"]),
            reason=str(approval["reason"]),
            _reviewed_at=str(approval["reviewed_at"]),
            _migration=self._migration_record(
                legacy_document,
                completed_document,
                completion_records,
                authority="EXISTING_NAMED_DECISION_ONLY",
                new_human_review_performed=False,
            ),
            _expected_document=legacy_document,
        )

    @staticmethod
    def _migration_record(
        legacy_document: dict[str, Any],
        completed_document: dict[str, Any],
        completion_records: list[dict[str, Any]],
        *,
        authority: str,
        new_human_review_performed: bool,
    ) -> dict[str, Any]:
        return {
            "schema_version": 2,
            "authority": authority,
            "migrated_at": utc_now(),
            "new_human_review_performed": new_human_review_performed,
            "legacy_solution_snapshot": legacy_document,
            "legacy_solution_snapshot_sha256": hashlib.sha256(
                canonical_json(legacy_document)
            ).hexdigest(),
            "completed_solution_snapshot_sha256": hashlib.sha256(
                canonical_json(completed_document)
            ).hexdigest(),
            "camera_completion": completion_records,
        }

    def _complete_legacy_document(
        self, document: dict[str, Any], *, backend: str
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        with self.project.connection() as connection:
            references = {
                str(reference["id"]): dict(reference)
                for reference in connection.execute(
                    "SELECT id,artifact_digest,original_name FROM reference_items"
                ).fetchall()
            }
        completion_records: list[dict[str, Any]] = []
        completed_cameras: list[dict[str, Any]] = []
        for camera in document.get("cameras", []):
            reference_id = str(camera.get("reference_id", ""))
            if not reference_id or reference_id not in references:
                raise ValueError(
                    f"legacy camera references unknown source image: {reference_id or '<missing>'}"
                )
            original = deepcopy(camera)
            if REQUIRED_CAMERA_STATE_FIELDS - set(original):
                completed = complete_camera_state(
                    original,
                    backend=backend,
                    source=references[reference_id],
                )
                validate_complete_camera_state(completed)
            else:
                validate_complete_camera_state(original)
                completed = original
            invariants = (
                "reference_id",
                "model",
                "width",
                "height",
                "intrinsics",
                "world_from_camera",
                "registration_class",
            )
            changed_invariants = [
                name
                for name in invariants
                if canonical_json(original.get(name)) != canonical_json(completed.get(name))
            ]
            if changed_invariants:
                raise ValueError(
                    "legacy camera completion would alter approved geometry: "
                    + ", ".join(changed_invariants)
                )
            populated_fields = sorted(set(completed) - set(original))
            geometry_before = {
                "intrinsics": original["intrinsics"],
                "world_from_camera": original["world_from_camera"],
            }
            geometry_after = {
                "intrinsics": completed["intrinsics"],
                "world_from_camera": completed["world_from_camera"],
            }
            completion_records.append(
                {
                    "reference_id": reference_id,
                    "populated_fields": populated_fields,
                    "geometry_sha256_before": hashlib.sha256(
                        canonical_json(geometry_before)
                    ).hexdigest(),
                    "geometry_sha256_after": hashlib.sha256(
                        canonical_json(geometry_after)
                    ).hexdigest(),
                    "geometry_unchanged": True,
                }
            )
            completed_cameras.append(completed)
        if len(completed_cameras) != len(document.get("cameras", [])) or not completed_cameras:
            raise ValueError("legacy camera decision requires a non-empty solution")
        completed_document = deepcopy(document)
        completed_document["cameras"] = completed_cameras
        return completed_document, completion_records

    def _verify_migration_receipt(
        self,
        migration: Any,
        *,
        prior_document: dict[str, Any],
        backend: str,
    ) -> bool:
        if migration is None:
            return True
        if not isinstance(migration, dict):
            return False
        authority = migration.get("authority")
        expected_human_review = {
            "EXISTING_NAMED_DECISION_ONLY": False,
            "DETERMINISTIC_PENDING_SNAPSHOT_COMPLETION": True,
        }.get(authority)
        if (
            expected_human_review is None
            or migration.get("new_human_review_performed") is not expected_human_review
        ):
            return False
        # Schema-1 migration receipts were immutable but did not embed the source
        # snapshot. Keep them verifiable; schema 2 adds replayable completion proof.
        if migration.get("schema_version") is None:
            return True
        if migration.get("schema_version") != 2:
            return False
        legacy_document = migration.get("legacy_solution_snapshot")
        if not isinstance(legacy_document, dict):
            return False
        if migration.get("legacy_solution_snapshot_sha256") != hashlib.sha256(
            canonical_json(legacy_document)
        ).hexdigest():
            return False
        completed_document, completion_records = self._complete_legacy_document(
            legacy_document, backend=backend
        )
        return bool(
            canonical_json(completed_document) == canonical_json(prior_document)
            and migration.get("completed_solution_snapshot_sha256")
            == hashlib.sha256(canonical_json(completed_document)).hexdigest()
            and canonical_json(migration.get("camera_completion"))
            == canonical_json(completion_records)
        )

    def verify(self, solution_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        record = dict(row)
        record["solution"] = json.loads(record.pop("solution_json"))
        return self.verify_record(record)

    def verify_record(self, record: dict[str, Any]) -> dict[str, Any]:
        document = record.get("solution", {})
        approval = document.get("approval", {}) if isinstance(document, dict) else {}
        state = approval.get("state") if isinstance(approval, dict) else None
        decision_id = record.get("decision_id")
        decision_digest = record.get("decision_digest")
        if state == "pending" and not record.get("approved"):
            valid = decision_id is None and decision_digest is None
            return {"valid": valid, "state": state, "decision": None}
        if state not in {"approved", "rejected"} or not decision_id or not decision_digest:
            return {"valid": False, "state": state, "decision": None}
        try:
            path = self.artifacts.path_for(str(decision_digest))
            if not path.is_file() or sha256_file(path)[0] != decision_digest:
                raise ValueError("camera decision receipt is missing or corrupt")
            artifact_receipt = json.loads(path.read_text(encoding="utf-8"))
            with self.project.connection() as connection:
                decision_row = connection.execute(
                    "SELECT * FROM camera_decisions WHERE id=?", (decision_id,)
                ).fetchone()
            if decision_row is None:
                raise ValueError("camera decision ledger row is missing")
            ledger_receipt = json.loads(decision_row["decision_json"])
            prior = deepcopy(document)
            prior["approved"] = artifact_receipt.get("prior_approved")
            prior["approval"] = artifact_receipt.get("prior_approval")
            cameras = document.get("cameras", [])
            for camera in cameras:
                validate_complete_camera_state(camera)
            valid = bool(
                canonical_json(artifact_receipt) == canonical_json(ledger_receipt)
                and decision_row["solution_id"] == record.get("id")
                and decision_row["state"] == state
                and decision_row["decision_digest"] == decision_digest
                and artifact_receipt.get("receipt_type") == "camera_solution_review"
                and artifact_receipt.get("id") == decision_id
                and artifact_receipt.get("solution_id") == record.get("id")
                and artifact_receipt.get("decision") == state
                and artifact_receipt.get("reviewer") == approval.get("reviewer")
                and artifact_receipt.get("reason") == approval.get("reason")
                and artifact_receipt.get("reviewed_at") == approval.get("reviewed_at")
                and approval.get("decision_id") == decision_id
                and approval.get("decision_digest") == decision_digest
                and bool(record.get("approved")) == (state == "approved")
                and document.get("approved") == (state == "approved")
                and artifact_receipt.get("acceptance_performed") is False
                and artifact_receipt.get("camera_immutable_sha256s")
                == [str(camera.get("immutable_sha256")) for camera in cameras]
                and artifact_receipt.get("reference_ids")
                == sorted(str(camera["reference_id"]) for camera in cameras)
                and artifact_receipt.get("registration_classes")
                == sorted({str(camera.get("registration_class")) for camera in cameras})
                and artifact_receipt.get("solution_snapshot_sha256")
                == hashlib.sha256(canonical_json(prior)).hexdigest()
                and self._verify_migration_receipt(
                    artifact_receipt.get("migration"),
                    prior_document=prior,
                    backend=str(record.get("backend", "")),
                )
            )
            return {"valid": valid, "state": state, "decision": artifact_receipt}
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "state": state, "decision": None}
