from __future__ import annotations

import hashlib
import json
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import (
    atomic_write_json,
    canonical_json,
    sha256_file,
    utc_now,
)
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore


class ReferenceDerivationStore:
    """Govern deterministic reference transforms without adding evidence authority."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def register_undistortion(self, derived_reference_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT id FROM reference_derivations WHERE derived_reference_id=?",
                (derived_reference_id,),
            ).fetchone()
        if existing:
            record = self.get(existing["id"], verify=True)
            if record.get("governed_source_id"):
                return record
            source = self._reference(record["source_reference_id"])
            governed_source = self._governed_source(source)
            if governed_source:
                return self._upgrade_governed_lineage(record, governed_source)
            return record

        derived = self._reference(derived_reference_id)
        metadata = derived["metadata"]
        source_reference_id = str(metadata.get("derived_from_reference_id", ""))
        source_solution_id = str(metadata.get("source_camera_solution_id", ""))
        if (
            derived["evidence_role"] != "acceptance_undistorted_reference"
            or not derived["acceptance_eligible"]
            or metadata.get("derivation")
            != "OpenCV cv2.undistort from immutable stored camera intrinsics"
            or not source_reference_id
            or not source_solution_id
        ):
            raise ValueError("reference is not a governed undistortion output")
        source = self._reference(source_reference_id)
        if metadata.get("derived_from_artifact_digest") != source["artifact_digest"]:
            raise ValueError("undistorted reference has stale source artifact lineage")
        camera_solution = self._camera_solution(source_solution_id, source_reference_id)
        governed_source = self._governed_source(source)
        source_snapshot = self._source_snapshot(governed_source)

        receipt_id = str(uuid.uuid4())
        now = utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "reference_derivation",
            "id": receipt_id,
            "operation": "opencv_undistort",
            "source_reference_id": source_reference_id,
            "source_artifact_digest": source["artifact_digest"],
            "derived_reference_id": derived_reference_id,
            "derived_artifact_digest": derived["artifact_digest"],
            "source_camera_solution_id": source_solution_id,
            "source_camera_solution_sha256": hashlib.sha256(
                canonical_json(camera_solution)
            ).hexdigest(),
            "parameters": {"roi": metadata.get("undistortion_roi")},
            "governed_source_id": governed_source["id"] if governed_source else None,
            "governed_source_snapshot_sha256": (
                hashlib.sha256(canonical_json(source_snapshot)).hexdigest()
                if source_snapshot
                else None
            ),
            "authority": "DERIVED_RASTER_INHERITS_SOURCE_GOVERNANCE_NO_NEW_EVIDENCE",
            "evidence_observation_added": False,
            "created_at": now,
        }
        relative = Path("receipts") / f"reference-derivation-{receipt_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.reference-derivation+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute(
                "INSERT INTO reference_derivations("
                "id,derived_reference_id,source_reference_id,governed_source_id,operation,"
                "receipt_digest,created_at) VALUES(?,?,?,?,?,?,?)",
                (
                    receipt_id,
                    derived_reference_id,
                    source_reference_id,
                    governed_source["id"] if governed_source else None,
                    "opencv_undistort",
                    artifact.digest,
                    now,
                ),
            )
        return {**receipt, "receipt_digest": artifact.digest}

    def _upgrade_governed_lineage(
        self, record: dict[str, Any], governed_source: dict[str, Any]
    ) -> dict[str, Any]:
        """Supersede an ungoverned derivation receipt when identical-byte lineage is found."""
        prior = record["receipt"]
        receipt_id = str(uuid.uuid4())
        now = utc_now()
        source_snapshot = self._source_snapshot(governed_source)
        receipt = {
            **prior,
            "id": receipt_id,
            "governed_source_id": governed_source["id"],
            "governed_source_snapshot_sha256": hashlib.sha256(
                canonical_json(source_snapshot)
            ).hexdigest(),
            "supersedes_derivation_id": record["id"],
            "supersedes_receipt_digest": record["receipt_digest"],
            "lineage_resolution": "content_addressed_reference_equivalence",
            "created_at": now,
        }
        relative = Path("receipts") / f"reference-derivation-{receipt_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.reference-derivation+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            updated = connection.execute(
                "UPDATE reference_derivations SET id=?,governed_source_id=?,"
                "receipt_digest=?,created_at=? WHERE id=? AND governed_source_id IS NULL "
                "AND receipt_digest=?",
                (
                    receipt_id,
                    governed_source["id"],
                    artifact.digest,
                    now,
                    record["id"],
                    record["receipt_digest"],
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("reference derivation lineage upgrade raced with another update")
        return self.get(receipt_id, verify=True)

    def get(self, derivation_id: str, *, verify: bool = False) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM reference_derivations WHERE id=?", (derivation_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown reference derivation: {derivation_id}")
        record = dict(row)
        path = self.artifacts.path_for(record["receipt_digest"])
        if not path.is_file() or sha256_file(path)[0] != record["receipt_digest"]:
            if verify:
                raise ValueError("reference derivation receipt is missing or corrupt")
            record["receipt"] = None
            return record
        receipt = json.loads(path.read_text(encoding="utf-8"))
        record["receipt"] = receipt
        if verify:
            self._verify_semantics(record, receipt)
        return record

    def audit(self) -> dict[str, Any]:
        with self.project.connection() as connection:
            ids = [
                row["id"]
                for row in connection.execute(
                    "SELECT id FROM reference_derivations ORDER BY created_at,id"
                )
            ]
            derived_ids = {
                row["id"]
                for row in connection.execute(
                    "SELECT id FROM reference_items WHERE acceptance_eligible=1 "
                    "AND evidence_role='acceptance_undistorted_reference'"
                )
            }
            recorded_ids = {
                row["derived_reference_id"]
                for row in connection.execute(
                    "SELECT derived_reference_id FROM reference_derivations"
                )
            }
        valid = []
        invalid = []
        for derivation_id in ids:
            try:
                record = self.get(derivation_id, verify=True)
                if record.get("governed_source_id"):
                    valid.append(record["derived_reference_id"])
            except (KeyError, OSError, ValueError) as error:
                invalid.append({"id": derivation_id, "error": str(error)})
        return {
            "valid_governed_reference_ids": sorted(valid),
            "missing_receipt_reference_ids": sorted(derived_ids - recorded_ids),
            "invalid_derivations": invalid,
        }

    def _verify_semantics(self, record: dict[str, Any], receipt: dict[str, Any]) -> None:
        derived = self._reference(record["derived_reference_id"])
        source = self._reference(record["source_reference_id"])
        metadata = derived["metadata"]
        camera_solution = self._camera_solution(
            receipt["source_camera_solution_id"], source["id"]
        )
        governed_source = (
            EvidenceAcquisitionStore(self.project).get(record["governed_source_id"])
            if record.get("governed_source_id")
            else None
        )
        source_snapshot = self._source_snapshot(governed_source)
        expected_source_hash = (
            hashlib.sha256(canonical_json(source_snapshot)).hexdigest()
            if source_snapshot
            else None
        )
        if (
            receipt.get("schema_version") != 1
            or receipt.get("receipt_type") != "reference_derivation"
            or receipt.get("id") != record["id"]
            or receipt.get("operation") != record["operation"]
            or receipt.get("operation") != "opencv_undistort"
            or receipt.get("source_reference_id") != source["id"]
            or receipt.get("source_artifact_digest") != source["artifact_digest"]
            or receipt.get("derived_reference_id") != derived["id"]
            or receipt.get("derived_artifact_digest") != derived["artifact_digest"]
            or metadata.get("derived_from_reference_id") != source["id"]
            or metadata.get("derived_from_artifact_digest") != source["artifact_digest"]
            or receipt.get("source_camera_solution_sha256")
            != hashlib.sha256(canonical_json(camera_solution)).hexdigest()
            or receipt.get("parameters", {}).get("roi")
            != metadata.get("undistortion_roi")
            or receipt.get("governed_source_id") != record.get("governed_source_id")
            or receipt.get("governed_source_snapshot_sha256") != expected_source_hash
            or receipt.get("authority")
            != "DERIVED_RASTER_INHERITS_SOURCE_GOVERNANCE_NO_NEW_EVIDENCE"
            or receipt.get("evidence_observation_added") is not False
            or not derived["acceptance_eligible"]
            or derived["evidence_role"] != "acceptance_undistorted_reference"
        ):
            raise ValueError("reference derivation receipt is semantically invalid")

    def _reference(self, reference_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT id,artifact_digest,metadata_json,evidence_role,acceptance_eligible "
                "FROM reference_items WHERE id=?",
                (reference_id,),
            ).fetchone()
        if row is None:
            raise ValueError("reference derivation points to a missing reference")
        record = dict(row)
        record["metadata"] = json.loads(record.pop("metadata_json"))
        record["acceptance_eligible"] = bool(record["acceptance_eligible"])
        artifact_path = self.artifacts.path_for(record["artifact_digest"])
        if (
            not artifact_path.is_file()
            or sha256_file(artifact_path)[0] != record["artifact_digest"]
        ):
            raise ValueError("reference derivation artifact is missing or corrupt")
        return record

    def _camera_solution(self, solution_id: str, reference_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
        if row is None:
            raise ValueError("reference derivation source camera no longer exists")
        document = json.loads(row["solution_json"])
        if not any(
            item.get("reference_id") == reference_id
            for item in document.get("cameras", [])
        ):
            raise ValueError("reference derivation source camera does not cover its source image")
        return document

    def _governed_source(self, source_reference: dict[str, Any]) -> dict[str, Any] | None:
        parent_video_id = source_reference["metadata"].get("video_source_reference_id")
        candidate_ids = [source_reference["id"]]
        if parent_video_id:
            candidate_ids.append(str(parent_video_id))
        placeholders = ",".join("?" for _ in candidate_ids)
        with self.project.connection() as connection:
            candidate_digests = [
                row["artifact_digest"]
                for row in connection.execute(
                    f"SELECT DISTINCT artifact_digest FROM reference_items "
                    f"WHERE id IN ({placeholders})",
                    candidate_ids,
                )
            ]
            digest_placeholders = ",".join("?" for _ in candidate_digests) or "NULL"
            row = connection.execute(
                "SELECT es.id FROM evidence_sources es "
                "JOIN reference_items ri ON ri.id=es.reference_id "
                "WHERE es.status='ACQUIRED' AND ("
                f"es.reference_id IN ({placeholders}) OR "
                f"ri.artifact_digest IN ({digest_placeholders})) "
                "ORDER BY CASE es.reference_id WHEN ? THEN 0 ELSE 1 END,es.id LIMIT 1",
                (*candidate_ids, *candidate_digests, source_reference["id"]),
            ).fetchone()
        if row is None:
            return None
        source = EvidenceAcquisitionStore(self.project).get(row["id"])
        if not EvidenceAcquisitionStore(self.project).authority_status(row["id"])[
            "acquisition_valid"
        ]:
            return None
        access = source["source"].get("access_policy", {})
        accepted = {"approved", "not_applicable", "user_owned"}
        try:
            target = TargetResolver(self.project).get()
        except (FileNotFoundError, ValueError):
            return None
        if (
            source["target_id"] != target["id"]
            or not source.get("reviewed_by")
            or not source.get("reviewed_at")
            or source["rights"].get("internal_use") is not True
            or access.get("robots_respected") is not True
            or access.get("source_terms_review") not in accepted
            or access.get("privacy_review") not in accepted
        ):
            return None
        return source

    @staticmethod
    def _source_snapshot(source: dict[str, Any] | None) -> dict[str, Any] | None:
        if source is None:
            return None
        return {
            "id": source["id"],
            "target_id": source["target_id"],
            "reference_id": source["reference_id"],
            "status": source["status"],
            "source": source["source"],
            "rights": source["rights"],
            "reviewed_by": source.get("reviewed_by"),
            "reviewed_at": source.get("reviewed_at"),
        }
