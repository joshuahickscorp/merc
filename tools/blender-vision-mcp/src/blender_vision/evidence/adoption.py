from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore

ACCEPTED_GOVERNANCE_REVIEWS = {"approved", "not_applicable", "user_owned"}
DECISIONS = {"ADOPT", "EXCLUDE"}
PROPOSAL_LIMITATIONS = [
    "legacy rights_state is context only and is not a reviewed rights decision",
    "origin, publisher, authority, target compatibility, and redistribution are unknown",
    "proposal creation does not make the reference acceptance or coverage evidence",
]


def _text(value: Any, label: str, *, maximum: int = 500) -> str:
    normalized = str(value or "").strip()
    if (
        not normalized
        or len(normalized) > maximum
        or any(ord(character) < 32 for character in normalized)
    ):
        raise ValueError(f"{label} must be non-empty printable text")
    return normalized


class LegacyReferenceAdoptionStore:
    """Govern migration of existing reference bytes into the evidence source ledger."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def propose(self, target_id: str, reference_id: str) -> dict[str, Any]:
        target = TargetResolver(self.project).get(target_id)
        with self.project.connection() as connection:
            reference_row = connection.execute(
                "SELECT * FROM reference_items WHERE id=?", (reference_id,)
            ).fetchone()
            linked = connection.execute(
                "SELECT id FROM evidence_sources WHERE reference_id=?", (reference_id,)
            ).fetchone()
            existing = connection.execute(
                "SELECT id FROM reference_adoption_proposals WHERE target_id=? AND reference_id=?",
                (target_id, reference_id),
            ).fetchone()
        if reference_row is None:
            raise KeyError(f"unknown reference: {reference_id}")
        if linked is not None:
            raise ValueError("reference is already governed by an evidence source")
        if existing is not None:
            return self.get(existing["id"], verify=True)
        reference = dict(reference_row)
        artifact_path = self.artifacts.path_for(reference["artifact_digest"])
        materialized_path = self.project.root / reference["relative_path"]
        if (
            not artifact_path.is_file()
            or sha256_file(artifact_path)[0] != reference["artifact_digest"]
            or not materialized_path.is_file()
            or sha256_file(materialized_path)[0] != reference["artifact_digest"]
        ):
            raise ValueError("legacy reference artifact is missing or corrupt")
        metadata = json.loads(reference["metadata_json"])
        quality = json.loads(reference["quality_json"])
        proposal_id = str(uuid.uuid4())
        now = utc_now()
        proposal = {
            "schema_version": 1,
            "receipt_type": "legacy_reference_adoption_proposal",
            "id": proposal_id,
            "target_id": target_id,
            "canonical_target": target["target"],
            "reference": {
                "id": reference_id,
                "artifact_digest": reference["artifact_digest"],
                "original_name": reference["original_name"],
                "media_type": reference["media_type"],
                "relative_path": reference["relative_path"],
                "rights_state": reference["rights_state"],
                "viewpoint_label": reference["viewpoint_label"],
                "evidence_role": reference["evidence_role"],
                "acceptance_eligible": bool(reference["acceptance_eligible"]),
                "created_at": reference["created_at"],
                "metadata": metadata,
                "quality": quality,
            },
            "suggested_source": {
                "page_title": reference["original_name"],
                "viewpoint": reference["viewpoint_label"],
                "target_variant": {},
            },
            "authority": "MACHINE_MIGRATION_PROPOSAL_NOT_RIGHTS_REVIEWED",
            "known_limitations": PROPOSAL_LIMITATIONS,
            "created_at": now,
        }
        relative = Path("receipts") / f"legacy-reference-adoption-proposal-{proposal_id}.json"
        atomic_write_json(self.project.root / relative, proposal)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.legacy-reference-adoption-proposal+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO reference_adoption_proposals("
                "id,target_id,reference_id,status,proposal_json,proposal_digest,decision_json,"
                "decision_digest,source_id,created_at,updated_at) "
                "VALUES(?,?,?,'PROPOSED',?,?,NULL,NULL,NULL,?,?)",
                (
                    proposal_id,
                    target_id,
                    reference_id,
                    json.dumps(proposal),
                    artifact.digest,
                    now,
                    now,
                ),
            )
        return self.get(proposal_id, verify=True)

    def propose_orphans(
        self, target_id: str, reference_ids: list[str] | None = None
    ) -> dict[str, Any]:
        TargetResolver(self.project).get(target_id)
        with self.project.connection() as connection:
            if reference_ids is None:
                rows = connection.execute(
                    "SELECT r.id FROM reference_items r "
                    "LEFT JOIN evidence_sources s ON s.reference_id=r.id "
                    "WHERE s.id IS NULL AND r.evidence_role!='diagnostic_video_frame' "
                    "ORDER BY r.created_at,r.id"
                ).fetchall()
                selected = [row["id"] for row in rows]
            else:
                selected = list(dict.fromkeys(str(item) for item in reference_ids))
        proposals = [self.propose(target_id, reference_id) for reference_id in selected]
        return {
            "target_id": target_id,
            "proposal_count": len(proposals),
            "proposals": proposals,
            "authority": "PROPOSALS_ONLY_NO_RIGHTS_INFERRED",
        }

    def review(
        self,
        proposal_id: str,
        *,
        decision: str,
        reviewer: str,
        reason: str,
        source: dict[str, Any] | None = None,
        rights: dict[str, Any] | None = None,
        source_terms_review: str | None = None,
        privacy_review: str | None = None,
    ) -> dict[str, Any]:
        normalized_decision = str(decision).upper()
        if normalized_decision not in DECISIONS:
            raise ValueError("legacy reference decision must be ADOPT or EXCLUDE")
        reviewer_name = _text(reviewer, "reviewer", maximum=200)
        review_reason = _text(reason, "reason", maximum=2000)
        record = self.get(proposal_id, verify=True)
        if record["status"] != "PROPOSED":
            raise ValueError("legacy reference proposal has already been decided")
        now = utc_now()
        source_id = str(uuid.uuid4()) if normalized_decision == "ADOPT" else None
        normalized_source = None
        normalized_rights = None
        if normalized_decision == "ADOPT":
            normalized_source = self._source(
                dict(source or {}),
                proposal=record["proposal"],
                source_terms_review=source_terms_review,
                privacy_review=privacy_review,
                reviewer=reviewer_name,
                reviewed_at=now,
            )
            normalized_rights = self._rights(dict(rights or {}))
        elif any(
            value is not None
            for value in (source, rights, source_terms_review, privacy_review)
        ):
            raise ValueError("excluded legacy references cannot carry adoption metadata")
        review_id = str(uuid.uuid4())
        decision_receipt = {
            "schema_version": 1,
            "receipt_type": "legacy_reference_adoption_review",
            "id": review_id,
            "proposal_id": proposal_id,
            "proposal_digest": record["proposal_digest"],
            "target_id": record["target_id"],
            "reference_id": record["reference_id"],
            "reference_artifact_digest": record["proposal"]["reference"][
                "artifact_digest"
            ],
            "decision": normalized_decision,
            "source_id": source_id,
            "source": normalized_source,
            "rights": normalized_rights,
            "reviewer": reviewer_name,
            "reason": review_reason,
            "acceptance_performed": False,
            "reviewed_at": now,
        }
        relative = Path("receipts") / f"legacy-reference-adoption-review-{review_id}.json"
        atomic_write_json(self.project.root / relative, decision_receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.legacy-reference-adoption-review+json",
        )
        status = "ADOPTED" if normalized_decision == "ADOPT" else "EXCLUDED"
        persisted_source = None
        source_authority = None
        if source_id and normalized_source and normalized_rights:
            persisted_source = {
                **normalized_source,
                "adoption_proposal_id": proposal_id,
                "adoption_decision_digest": artifact.digest,
            }
            source_authority = EvidenceAcquisitionStore(
                self.project
            ).prepare_adoption_authority(
                source_id=source_id,
                target_id=record["target_id"],
                source=persisted_source,
                rights=normalized_rights,
                reviewer=reviewer_name,
                reviewed_at=now,
                reference_id=record["reference_id"],
            )
            persisted_source = source_authority["source"]
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            proposal_row = connection.execute(
                "SELECT status,reference_id,proposal_digest FROM reference_adoption_proposals "
                "WHERE id=?",
                (proposal_id,),
            ).fetchone()
            reference_row = connection.execute(
                "SELECT artifact_digest FROM reference_items WHERE id=?",
                (record["reference_id"],),
            ).fetchone()
            if (
                proposal_row is None
                or proposal_row["status"] != "PROPOSED"
                or proposal_row["proposal_digest"] != record["proposal_digest"]
                or reference_row is None
                or reference_row["artifact_digest"]
                != decision_receipt["reference_artifact_digest"]
            ):
                raise RuntimeError("legacy reference adoption raced or became stale")
            if (
                source_id
                and persisted_source
                and normalized_rights
                and source_authority
            ):
                existing = connection.execute(
                    "SELECT id FROM evidence_sources WHERE reference_id=?",
                    (record["reference_id"],),
                ).fetchone()
                if existing is not None:
                    raise RuntimeError("legacy reference was adopted by another decision")
                connection.execute(
                    "INSERT INTO evidence_sources(id,target_id,reference_id,source_json,status,"
                    "created_at,updated_at) VALUES(?,?,?,?, 'ACQUIRED',?,?)",
                    (
                        source_id,
                        record["target_id"],
                        record["reference_id"],
                        json.dumps(persisted_source),
                        now,
                        now,
                    ),
                )
                connection.execute(
                    "INSERT INTO rights_ledger(source_id,rights_json,reviewed_by,reviewed_at,"
                    "updated_at) VALUES(?,?,?,?,?)",
                    (source_id, json.dumps(normalized_rights), reviewer_name, now, now),
                )
                governance = source_authority["governance"]
                governance_receipt = governance["receipt"]
                connection.execute(
                    "INSERT INTO evidence_source_governance_reviews("
                    "id,source_id,reviewer,reviewer_type,source_json,rights_json,"
                    "receipt_digest,supersedes_receipt_digest,created_at) "
                    "VALUES(?,?,?,?,?,?,?,?,?)",
                    (
                        governance_receipt["id"],
                        source_id,
                        reviewer_name,
                        "human",
                        json.dumps(governance_receipt["source"]),
                        json.dumps(normalized_rights),
                        governance["digest"],
                        None,
                        now,
                    ),
                )
                acquisition = source_authority["acquisition"]
                acquisition_receipt = acquisition["receipt"]
                connection.execute(
                    "INSERT INTO evidence_source_acquisitions("
                    "id,source_id,reference_id,governance_receipt_digest,source_json,"
                    "reference_json,receipt_digest,supersedes_receipt_digest,created_at) "
                    "VALUES(?,?,?,?,?,?,?,?,?)",
                    (
                        acquisition_receipt["id"],
                        source_id,
                        record["reference_id"],
                        governance["digest"],
                        json.dumps(persisted_source),
                        json.dumps(acquisition_receipt["reference"]),
                        acquisition["digest"],
                        None,
                        now,
                    ),
                )
                connection.execute(
                    "UPDATE reference_items SET rights_state=? WHERE id=?",
                    (normalized_rights["status"], record["reference_id"]),
                )
            else:
                connection.execute(
                    "UPDATE reference_items SET acceptance_eligible=0 WHERE id=?",
                    (record["reference_id"],),
                )
            updated = connection.execute(
                "UPDATE reference_adoption_proposals SET status=?,decision_json=?,"
                "decision_digest=?,source_id=?,updated_at=? WHERE id=? AND status='PROPOSED'",
                (
                    status,
                    json.dumps(decision_receipt),
                    artifact.digest,
                    source_id,
                    now,
                    proposal_id,
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("legacy reference review raced with another reviewer")
        conflict_audit = None
        duplicate_audit = None
        if source_id:
            conflict_audit = EvidenceConflictStore(self.project).audit(
                record["target_id"], record=True
            )
            duplicate_audit = EvidenceDuplicateStore(self.project).audit(
                record["target_id"], record=True
            )
        return {
            **decision_receipt,
            "status": status,
            "decision_digest": artifact.digest,
            "conflict_audit": conflict_audit,
            "duplicate_audit": duplicate_audit,
        }

    def get(self, proposal_id: str, *, verify: bool = False) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM reference_adoption_proposals WHERE id=?", (proposal_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown legacy reference adoption proposal: {proposal_id}")
        value = dict(row)
        value["proposal"] = json.loads(value.pop("proposal_json"))
        decision_json = value.pop("decision_json")
        value["decision"] = json.loads(decision_json) if decision_json else None
        if verify:
            self._verify(value)
        return value

    def list(self, target_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id FROM reference_adoption_proposals "
                + ("WHERE target_id=? " if target_id else "")
                + "ORDER BY created_at,id",
                (target_id,) if target_id else (),
            ).fetchall()
        return [self.get(row["id"]) for row in rows]

    def _verify(self, record: dict[str, Any]) -> None:
        self._verify_document(record["proposal_digest"], record["proposal"])
        proposal = record["proposal"]
        if not isinstance(proposal, dict):
            raise ValueError("legacy reference adoption proposal must be an object")
        proposal_reference = proposal.get("reference")
        if not isinstance(proposal_reference, dict):
            raise ValueError("legacy reference adoption reference snapshot must be an object")
        proposal_fields = {
            "schema_version",
            "receipt_type",
            "id",
            "target_id",
            "canonical_target",
            "reference",
            "suggested_source",
            "authority",
            "known_limitations",
            "created_at",
        }
        if (
            set(proposal) != proposal_fields
            or proposal.get("schema_version") != 1
            or proposal.get("receipt_type") != "legacy_reference_adoption_proposal"
            or proposal.get("id") != record["id"]
            or proposal.get("target_id") != record["target_id"]
            or proposal_reference.get("id") != record["reference_id"]
            or proposal.get("suggested_source")
            != {
                "page_title": proposal_reference.get("original_name"),
                "viewpoint": proposal_reference.get("viewpoint_label"),
                "target_variant": {},
            }
            or proposal.get("authority")
            != "MACHINE_MIGRATION_PROPOSAL_NOT_RIGHTS_REVIEWED"
            or proposal.get("known_limitations") != PROPOSAL_LIMITATIONS
            or not str(proposal.get("created_at", "")).strip()
        ):
            raise ValueError("legacy reference adoption proposal semantics are invalid")
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT artifact_digest,original_name,media_type,relative_path,rights_state,"
                "viewpoint_label,evidence_role,acceptance_eligible,created_at,metadata_json,"
                "quality_json "
                "FROM reference_items WHERE id=?",
                (record["reference_id"],),
            ).fetchone()
        target = TargetResolver(self.project).get(record["target_id"])
        immutable_reference_snapshot = {
            "id": record["reference_id"],
            "artifact_digest": reference["artifact_digest"] if reference else None,
            "original_name": reference["original_name"] if reference else None,
            "media_type": reference["media_type"] if reference else None,
            "relative_path": reference["relative_path"] if reference else None,
            "viewpoint_label": reference["viewpoint_label"] if reference else None,
            "evidence_role": reference["evidence_role"] if reference else None,
            "created_at": reference["created_at"] if reference else None,
            "metadata": json.loads(reference["metadata_json"]) if reference else None,
            "quality": json.loads(reference["quality_json"]) if reference else None,
        }
        proposal_immutable_snapshot = {
            key: proposal_reference.get(key) for key in immutable_reference_snapshot
        }
        artifact_path = (
            self.artifacts.path_for(reference["artifact_digest"]) if reference else None
        )
        if (
            reference is None
            or proposal.get("canonical_target") != target["target"]
            or not isinstance(proposal_reference, dict)
            or set(proposal_reference)
            != {
                "id",
                "artifact_digest",
                "original_name",
                "media_type",
                "relative_path",
                "rights_state",
                "viewpoint_label",
                "evidence_role",
                "acceptance_eligible",
                "created_at",
                "metadata",
                "quality",
            }
            or proposal_immutable_snapshot != immutable_reference_snapshot
            or not isinstance(proposal_reference.get("acceptance_eligible"), bool)
            or not str(proposal_reference.get("rights_state", "")).strip()
            or artifact_path is None
            or not artifact_path.is_file()
            or sha256_file(artifact_path)[0] != reference["artifact_digest"]
            or not (self.project.root / reference["relative_path"]).is_file()
            or sha256_file(self.project.root / reference["relative_path"])[0]
            != reference["artifact_digest"]
        ):
            raise ValueError("legacy reference adoption proposal binding is stale")
        if record["status"] == "PROPOSED":
            if record["decision"] or record["decision_digest"] or record["source_id"]:
                raise ValueError("undecided legacy reference proposal has decision state")
            return
        decision = record["decision"]
        if not decision or not record["decision_digest"]:
            raise ValueError("decided legacy reference proposal lacks its review receipt")
        self._verify_document(record["decision_digest"], decision)
        if not isinstance(decision, dict):
            raise ValueError("legacy reference adoption review must be an object")
        decision_fields = {
            "schema_version",
            "receipt_type",
            "id",
            "proposal_id",
            "proposal_digest",
            "target_id",
            "reference_id",
            "reference_artifact_digest",
            "decision",
            "source_id",
            "source",
            "rights",
            "reviewer",
            "reason",
            "acceptance_performed",
            "reviewed_at",
        }
        if (
            set(decision) != decision_fields
            or decision.get("schema_version") != 1
            or decision.get("receipt_type") != "legacy_reference_adoption_review"
            or not str(decision.get("id", "")).strip()
            or decision.get("proposal_id") != record["id"]
            or decision.get("proposal_digest") != record["proposal_digest"]
            or decision.get("target_id") != record["target_id"]
            or decision.get("reference_id") != record["reference_id"]
            or decision.get("reference_artifact_digest") != reference["artifact_digest"]
            or _text(decision.get("reviewer"), "reviewer", maximum=200)
            != decision.get("reviewer")
            or _text(decision.get("reason"), "reason", maximum=2000)
            != decision.get("reason")
            or decision.get("acceptance_performed") is not False
            or not str(decision.get("reviewed_at", "")).strip()
        ):
            raise ValueError("legacy reference adoption review semantics are invalid")
        if record["status"] == "EXCLUDED":
            if (
                decision.get("decision") != "EXCLUDE"
                or decision.get("source_id") is not None
                or decision.get("source") is not None
                or decision.get("rights") is not None
                or record["source_id"] is not None
                or bool(reference["acceptance_eligible"])
            ):
                raise ValueError("excluded legacy reference retains source authority")
            return
        if record["status"] != "ADOPTED" or decision.get("decision") != "ADOPT":
            raise ValueError("legacy reference adoption status is invalid")
        if (
            not record["source_id"]
            or decision.get("source_id") != record["source_id"]
            or not isinstance(decision.get("source"), dict)
            or not isinstance(decision.get("rights"), dict)
        ):
            raise ValueError("legacy reference adoption source identity is invalid")
        self._verify_source_semantics(
            decision["source"],
            proposal=proposal,
            reviewer=decision["reviewer"],
            reviewed_at=decision["reviewed_at"],
        )
        self._verify_rights_semantics(decision["rights"])
        with self.project.connection() as connection:
            source_row = connection.execute(
                "SELECT s.target_id,s.reference_id,s.source_json,s.status,r.rights_json,"
                "r.reviewed_by,r.reviewed_at,i.rights_state FROM evidence_sources s "
                "JOIN rights_ledger r ON r.source_id=s.id "
                "JOIN reference_items i ON i.id=s.reference_id WHERE s.id=?",
                (record["source_id"],),
            ).fetchone()
        if source_row is None:
            raise ValueError("adopted legacy reference source is missing")
        source = json.loads(source_row["source_json"])
        expected_source = {
            **decision["source"],
            "adoption_proposal_id": record["id"],
            "adoption_decision_digest": record["decision_digest"],
        }
        if (
            source_row["target_id"] != record["target_id"]
            or source_row["reference_id"] != record["reference_id"]
            or source_row["status"] != "ACQUIRED"
            or canonical_json(source) != canonical_json(expected_source)
            or canonical_json(json.loads(source_row["rights_json"]))
            != canonical_json(decision["rights"])
            or source_row["reviewed_by"] != decision["reviewer"]
            or source_row["reviewed_at"] != decision["reviewed_at"]
            or source_row["rights_state"] != decision["rights"]["status"]
        ):
            raise ValueError("adopted legacy reference source ledger is inconsistent")

    def _verify_document(self, digest: str, document: dict[str, Any]) -> None:
        path = self.artifacts.path_for(digest)
        if not path.is_file() or sha256_file(path)[0] != digest:
            raise ValueError("legacy reference adoption receipt is missing or corrupt")
        artifact_document = json.loads(path.read_text(encoding="utf-8"))
        if canonical_json(artifact_document) != canonical_json(document):
            raise ValueError("legacy reference adoption record disagrees with its receipt")

    @staticmethod
    def _validate_target_variant(value: dict[str, Any], canonical: dict[str, Any]) -> None:
        if not isinstance(value, dict):
            raise ValueError("legacy reference target_variant must be an object")
        for field in ("manufacturer", "model"):
            expected = str(canonical.get(field) or "").strip()
            observed = str(value.get(field) or "").strip()
            if not expected or observed.casefold() != expected.casefold():
                raise ValueError(
                    "legacy reference target_variant must match the resolved manufacturer and model"
                )

    @staticmethod
    def _string_list(value: Any, label: str) -> list[str]:
        if not isinstance(value, list) or any(not isinstance(item, str) for item in value):
            raise ValueError(f"{label} must be a list of strings")
        normalized = [_text(item, label) for item in value]
        if len(normalized) != len(set(normalized)):
            raise ValueError(f"{label} cannot contain duplicates")
        return normalized

    @classmethod
    def _verify_source_semantics(
        cls,
        source: dict[str, Any],
        *,
        proposal: dict[str, Any],
        reviewer: str,
        reviewed_at: str,
    ) -> None:
        source_fields = {
            "origin",
            "publisher",
            "page_title",
            "authority_class",
            "target_variant",
            "viewpoint",
            "quality_score",
            "url",
            "retrieval_timestamp",
            "legacy_ingested_at",
            "content_hash",
            "media_hash",
            "editing_suspicion",
            "cropping",
            "known_scale",
            "included_evidence",
            "excluded_evidence",
            "access_policy",
            "provenance_limitations",
        }
        access_fields = {
            "robots_respected",
            "authentication_boundary",
            "source_terms_review",
            "privacy_review",
            "rate_limit_policy",
            "maximum_download_bytes",
            "reviewed_by",
            "reviewed_at",
            "reviewer_type",
            "network_acquisition_performed",
        }
        access = source.get("access_policy")
        quality = source.get("quality_score")
        reference = proposal["reference"]
        if (
            set(source) != source_fields
            or not isinstance(access, dict)
            or set(access) != access_fields
            or _text(source.get("origin"), "source origin", maximum=2000)
            != source.get("origin")
            or _text(source.get("publisher"), "source publisher")
            != source.get("publisher")
            or _text(source.get("page_title"), "source page title")
            != source.get("page_title")
            or _text(source.get("authority_class"), "authority class")
            != source.get("authority_class")
            or _text(source.get("viewpoint"), "viewpoint") != source.get("viewpoint")
            or _text(source.get("editing_suspicion"), "editing suspicion")
            != source.get("editing_suspicion")
            or isinstance(quality, bool)
            or not isinstance(quality, (int, float))
            or not math.isfinite(float(quality))
            or not 0.0 <= float(quality) <= 1.0
            or (
                source.get("url") is not None
                and _text(source.get("url"), "source URL", maximum=2000)
                != source.get("url")
            )
            or source.get("retrieval_timestamp") is not None
            or source.get("legacy_ingested_at") != reference["created_at"]
            or source.get("content_hash") != reference["artifact_digest"]
            or source.get("media_hash") != reference["artifact_digest"]
            or not isinstance(source.get("cropping"), dict)
            or access.get("robots_respected") is not True
            or access.get("authentication_boundary") != "none"
            or access.get("source_terms_review") not in ACCEPTED_GOVERNANCE_REVIEWS
            or access.get("privacy_review") not in ACCEPTED_GOVERNANCE_REVIEWS
            or access.get("rate_limit_policy") != "legacy_local_artifact_no_network"
            or access.get("maximum_download_bytes") != 512 * 1024 * 1024
            or access.get("reviewed_by") != reviewer
            or not str(access.get("reviewed_at", "")).strip()
            or str(access.get("reviewed_at")) > reviewed_at
            or access.get("network_acquisition_performed") is not False
            or source.get("provenance_limitations")
            != [
                "original retrieval timestamp was not retained by the legacy project",
                "adoption preserves existing bytes and does not re-download the source",
            ]
        ):
            raise ValueError("adopted legacy reference source semantics are invalid")
        cls._validate_target_variant(source["target_variant"], proposal["canonical_target"])
        if cls._string_list(source["included_evidence"], "included evidence") != source[
            "included_evidence"
        ] or cls._string_list(source["excluded_evidence"], "excluded evidence") != source[
            "excluded_evidence"
        ]:
            raise ValueError("legacy reference evidence lists are not canonical")

    @classmethod
    def _verify_rights_semantics(cls, rights: dict[str, Any]) -> None:
        if set(rights) != {"status", "internal_use", "redistribution", "notes"}:
            raise ValueError("legacy reference rights snapshot is incomplete")
        if canonical_json(cls._rights(rights)) != canonical_json(rights):
            raise ValueError("legacy reference rights semantics are invalid")

    @staticmethod
    def _source(
        value: dict[str, Any],
        *,
        proposal: dict[str, Any],
        source_terms_review: str | None,
        privacy_review: str | None,
        reviewer: str,
        reviewed_at: str,
    ) -> dict[str, Any]:
        terms = str(source_terms_review or "")
        privacy = str(privacy_review or "")
        if (
            terms not in ACCEPTED_GOVERNANCE_REVIEWS
            or privacy not in ACCEPTED_GOVERNANCE_REVIEWS
        ):
            raise ValueError("source terms and privacy review must be explicitly resolved")
        quality_score = value.get("quality_score")
        if (
            isinstance(quality_score, bool)
            or not isinstance(quality_score, (int, float))
            or not math.isfinite(float(quality_score))
            or not 0.0 <= float(quality_score) <= 1.0
        ):
            raise ValueError("legacy reference quality_score must be between zero and one")
        target_variant = value.get("target_variant")
        LegacyReferenceAdoptionStore._validate_target_variant(
            target_variant, proposal["canonical_target"]
        )
        included_evidence = LegacyReferenceAdoptionStore._string_list(
            value.get("included_evidence", []), "included evidence"
        )
        excluded_evidence = LegacyReferenceAdoptionStore._string_list(
            value.get("excluded_evidence", []), "excluded evidence"
        )
        source_url = value.get("url")
        if source_url is not None:
            source_url = _text(source_url, "source URL", maximum=2000)
        reference = proposal["reference"]
        return {
            "origin": _text(value.get("origin"), "source origin", maximum=2000),
            "publisher": _text(value.get("publisher"), "source publisher"),
            "page_title": _text(value.get("page_title"), "source page title"),
            "authority_class": _text(value.get("authority_class"), "authority class"),
            "target_variant": target_variant,
            "viewpoint": _text(value.get("viewpoint"), "viewpoint"),
            "quality_score": float(quality_score),
            "url": source_url,
            "retrieval_timestamp": None,
            "legacy_ingested_at": reference["created_at"],
            "content_hash": reference["artifact_digest"],
            "media_hash": reference["artifact_digest"],
            "editing_suspicion": _text(
                value.get("editing_suspicion", "unassessed"), "editing suspicion"
            ),
            "cropping": dict(value.get("cropping") or {}),
            "known_scale": value.get("known_scale"),
            "included_evidence": included_evidence,
            "excluded_evidence": excluded_evidence,
            "access_policy": {
                "robots_respected": True,
                "authentication_boundary": "none",
                "source_terms_review": terms,
                "privacy_review": privacy,
                "rate_limit_policy": "legacy_local_artifact_no_network",
                "maximum_download_bytes": 512 * 1024 * 1024,
                "reviewed_by": reviewer,
                "reviewed_at": reviewed_at,
                "reviewer_type": "human",
                "network_acquisition_performed": False,
            },
            "provenance_limitations": [
                "original retrieval timestamp was not retained by the legacy project",
                "adoption preserves existing bytes and does not re-download the source",
            ],
        }

    @staticmethod
    def _rights(value: dict[str, Any]) -> dict[str, Any]:
        if not {"status", "internal_use", "redistribution"}.issubset(value):
            raise ValueError("rights decision requires status, internal_use, and redistribution")
        status = _text(value["status"], "rights status")
        if not isinstance(value["internal_use"], bool) or not isinstance(
            value["redistribution"], bool
        ):
            raise ValueError("rights internal_use and redistribution must be booleans")
        if value["internal_use"] is not True:
            raise PermissionError("adoption requires explicit internal-use permission")
        notes = value.get("notes")
        normalized_notes = _text(notes, "rights notes", maximum=2000) if notes else None
        return {
            "status": status,
            "internal_use": value["internal_use"],
            "redistribution": value["redistribution"],
            "notes": normalized_notes,
        }
