from __future__ import annotations

import json
import tempfile
import uuid
from pathlib import Path
from typing import Any

from PIL import Image, ImageOps

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore


class ReferenceMaskStore:
    """Hash-bind named, human-reviewed silhouette masks to immutable references."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def propose_automatic(
        self,
        reference_id: str,
        *,
        creator: str = "VisionMCP",
        backend: str = "compare_silhouettes_v3",
        intended_use: str = "silhouette_evaluation",
        visible_components: list[str] | None = None,
        excluded_components: list[str] | None = None,
        roi: dict[str, int] | None = None,
    ) -> dict[str, Any]:
        """Create a replayable machine proposal with explicitly zero review authority."""
        creator = creator.strip()
        backend = backend.strip()
        intended_use = intended_use.strip()
        if not creator or not backend or not intended_use:
            raise ValueError("mask proposal governance fields cannot be blank")
        if backend != "compare_silhouettes_v3":
            raise ValueError("automatic mask proposal backend is unsupported")
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT artifact_digest,media_type,acceptance_eligible FROM reference_items "
                "WHERE id=?",
                (reference_id,),
            ).fetchone()
            rows = connection.execute(
                "SELECT id,record_json FROM reference_mask_proposals "
                "WHERE reference_id=? AND status='PROPOSED' ORDER BY created_at,id",
                (reference_id,),
            ).fetchall()
        if reference is None:
            raise KeyError(f"unknown reference: {reference_id}")
        if not str(reference["media_type"]).startswith("image/"):
            raise ValueError("mask proposals require an image reference")
        if not bool(reference["acceptance_eligible"]):
            raise ValueError("mask proposals require acceptance-eligible evidence")
        reference_path = self.artifacts.path_for(reference["artifact_digest"])
        with Image.open(reference_path) as image:
            reference_size = ImageOps.exif_transpose(image).size
        governed_visible = self._normalize_components(
            visible_components, field="visible_components"
        )
        governed_excluded = self._normalize_components(
            excluded_components, field="excluded_components"
        )
        if set(governed_visible) & set(governed_excluded):
            raise ValueError("visible and excluded reference-mask components must be disjoint")
        governed_roi = self._normalize_roi(
            roi
            or {
                "x": 0,
                "y": 0,
                "width": reference_size[0],
                "height": reference_size[1],
            },
            reference_size,
        )
        for row in rows:
            record = json.loads(row["record_json"])
            if not self.verify_proposal(row["id"])["valid"]:
                raise ValueError("existing proposed reference-mask receipt is invalid")
            if (
                record.get("reference_artifact_digest") == reference["artifact_digest"]
                and record.get("backend") == backend
                and record.get("intended_use") == intended_use
                and record.get("creator") == creator
                and record.get("visible_components") == governed_visible
                and record.get("excluded_components") == governed_excluded
                and record.get("roi") == governed_roi
            ):
                return self.get(row["id"], verify=True)

        from blender_vision.comparison.metrics import (
            BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION,
            _reference_mask,
        )

        with Image.open(reference_path) as image:
            mask, method, confidence = _reference_mask(
                image,
                automatic_segmentation_maximum_dimension=(
                    BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
                ),
            )
        foreground = sum(mask.tobytes()) // 255
        total = mask.width * mask.height
        if foreground <= 0 or foreground >= total:
            raise ValueError("automatic mask proposal must contain foreground and background")
        proposal_id = str(uuid.uuid4())
        now = utc_now()
        with self.project.connection() as connection:
            revision = connection.execute(
                "SELECT COALESCE(MAX(json_extract(record_json,'$.revision')),0)+1 "
                "FROM reference_mask_proposals WHERE reference_id=?",
                (reference_id,),
            ).fetchone()[0]
        relative_mask = Path("references") / "masks" / f"proposal-{proposal_id}.png"
        mask_path = self.project.root / relative_mask
        mask_path.parent.mkdir(parents=True, exist_ok=True)
        mask.save(mask_path, format="PNG", optimize=True)
        mask_artifact = self.artifacts.ingest_file(mask_path, media_type="image/png")
        proposal = {
            "schema_version": 1,
            "receipt_type": "reference_mask_proposal",
            "id": proposal_id,
            "reference_id": reference_id,
            "reference_artifact_digest": reference["artifact_digest"],
            "mask_artifact_digest": mask_artifact.digest,
            "method": str(method),
            "creator": creator,
            "backend": backend,
            "revision": int(revision),
            "approval_state": "proposed",
            "confidence": str(confidence),
            "intended_use": intended_use,
            "visible_components": governed_visible,
            "excluded_components": governed_excluded,
            "roi": governed_roi,
            "automatic_segmentation_maximum_dimension": (
                BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
            ),
            "authority": "MACHINE_PROPOSAL_NO_REVIEW_OR_ACCEPTANCE_AUTHORITY",
            "created_at": now,
        }
        relative_receipt = Path("receipts") / f"reference-mask-proposal-{proposal_id}.json"
        atomic_write_json(self.project.root / relative_receipt, proposal)
        proposal_artifact = self.artifacts.ingest_file(
            self.project.root / relative_receipt,
            media_type="application/vnd.bvmcp.reference-mask-proposal+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO reference_mask_proposals(id,reference_id,mask_artifact_digest,"
                "proposal_digest,status,record_json,decision_digest,approved_mask_id,created_at,"
                "updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
                (
                    proposal_id,
                    reference_id,
                    mask_artifact.digest,
                    proposal_artifact.digest,
                    "PROPOSED",
                    json.dumps(proposal),
                    None,
                    None,
                    now,
                    now,
                ),
            )
        return self.get(proposal_id, verify=True)

    def review_proposal(
        self,
        proposal_id: str,
        *,
        accepted: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Accept or reject a machine mask through one receipt-backed DB transaction."""
        if not reviewer.strip() or not reason.strip():
            raise ValueError("mask proposal review requires a named reviewer and reason")
        record = self.get(proposal_id, verify=True)
        if record["status"] != "PROPOSED":
            raise ValueError("mask proposal has already been reviewed")
        proposal = record["proposal"]
        now = utc_now()
        mask_id = str(uuid.uuid4()) if accepted else None
        with self.project.connection() as connection:
            revision = connection.execute(
                "SELECT COALESCE(MAX(revision),0)+1 FROM reference_masks WHERE reference_id=?",
                (proposal["reference_id"],),
            ).fetchone()[0]
        approved_mask = (
            {
                "id": mask_id,
                "reference_id": proposal["reference_id"],
                "artifact_digest": proposal["mask_artifact_digest"],
                "source_artifact_digest": proposal["mask_artifact_digest"],
                "method": "human_reviewed_machine_proposal",
                "reviewer": reviewer.strip(),
                "reason": reason.strip(),
                "creator": proposal["creator"],
                "backend": proposal["backend"],
                "revision": int(revision),
                "approval_state": "approved",
                "confidence": "high",
                "intended_use": proposal["intended_use"],
                "visible_components": proposal["visible_components"],
                "excluded_components": proposal["excluded_components"],
                "roi": proposal["roi"],
                "created_at": now,
            }
            if accepted
            else None
        )
        decision = {
            "schema_version": 1,
            "receipt_type": "reference_mask_proposal_decision",
            "id": str(uuid.uuid4()),
            "proposal_id": proposal_id,
            "proposal_digest": record["proposal_digest"],
            "reference_id": proposal["reference_id"],
            "mask_artifact_digest": proposal["mask_artifact_digest"],
            "decision": "APPROVED" if accepted else "REJECTED",
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "approved_mask": approved_mask,
            "authority": "NAMED_MASK_REVIEW_DECISION",
            "created_at": now,
        }
        relative = Path("receipts") / f"reference-mask-decision-{decision['id']}.json"
        atomic_write_json(self.project.root / relative, decision)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.reference-mask-decision+json",
        )
        if approved_mask is not None:
            approved_mask["proposal_id"] = proposal_id
            approved_mask["decision_digest"] = artifact.digest
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            if approved_mask is not None:
                connection.execute(
                    "INSERT INTO reference_masks(id,reference_id,artifact_digest,"
                    "source_artifact_digest,method,reviewer,reason,creator,backend,revision,"
                    "approval_state,confidence,intended_use,visible_components_json,"
                    "excluded_components_json,roi_json,proposal_id,decision_digest,created_at) "
                    "VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        approved_mask["id"],
                        approved_mask["reference_id"],
                        approved_mask["artifact_digest"],
                        approved_mask["source_artifact_digest"],
                        approved_mask["method"],
                        approved_mask["reviewer"],
                        approved_mask["reason"],
                        approved_mask["creator"],
                        approved_mask["backend"],
                        approved_mask["revision"],
                        approved_mask["approval_state"],
                        approved_mask["confidence"],
                        approved_mask["intended_use"],
                        json.dumps(approved_mask["visible_components"]),
                        json.dumps(approved_mask["excluded_components"]),
                        json.dumps(approved_mask["roi"]),
                        proposal_id,
                        artifact.digest,
                        approved_mask["created_at"],
                    ),
                )
            updated = connection.execute(
                "UPDATE reference_mask_proposals SET status=?,decision_digest=?,"
                "approved_mask_id=?,updated_at=? WHERE id=? AND status='PROPOSED' "
                "AND decision_digest IS NULL",
                (
                    decision["decision"],
                    artifact.digest,
                    mask_id,
                    now,
                    proposal_id,
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("mask proposal changed during review")
        return self.get(proposal_id, verify=True)

    def get_proposal(self, proposal_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM reference_mask_proposals WHERE id=?", (proposal_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown mask proposal: {proposal_id}")
        value = dict(row)
        value["proposal"] = json.loads(value.pop("record_json"))
        return value

    def get(self, identifier: str, *, verify: bool = False) -> dict[str, Any]:
        """Return a proposal when present, otherwise preserve the legacy mask getter."""
        with self.project.connection() as connection:
            proposal = connection.execute(
                "SELECT 1 FROM reference_mask_proposals WHERE id=?", (identifier,)
            ).fetchone()
        if proposal is None:
            return self._get_mask(identifier)
        value = self.get_proposal(identifier)
        if verify:
            verification = self.verify_proposal(identifier)
            if not verification["valid"]:
                raise ValueError("reference-mask proposal is invalid")
            value["proposal_verification"] = verification
            if value["status"] != "PROPOSED":
                decision = self.verify_decision(identifier)
                if not decision["valid"]:
                    raise ValueError("reference-mask proposal decision is invalid")
                value["decision_verification"] = decision
        return value

    def list_proposals(self, reference_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id FROM reference_mask_proposals "
                + ("WHERE reference_id=? " if reference_id else "")
                + "ORDER BY created_at,id",
                (reference_id,) if reference_id else (),
            ).fetchall()
        return [self.get(row["id"]) for row in rows]

    def verify_proposal(self, proposal_id: str) -> dict[str, Any]:
        try:
            record = self.get_proposal(proposal_id)
            proposal = record["proposal"]
            required_fields = {
                "schema_version",
                "receipt_type",
                "id",
                "reference_id",
                "reference_artifact_digest",
                "mask_artifact_digest",
                "method",
                "creator",
                "backend",
                "revision",
                "approval_state",
                "confidence",
                "intended_use",
                "visible_components",
                "excluded_components",
                "roi",
                "automatic_segmentation_maximum_dimension",
                "authority",
                "created_at",
            }
            if (
                not required_fields.issubset(proposal)
                or proposal.get("schema_version") != 1
                or proposal.get("receipt_type") != "reference_mask_proposal"
                or proposal.get("id") != proposal_id
                or proposal.get("reference_id") != record["reference_id"]
                or proposal.get("mask_artifact_digest") != record["mask_artifact_digest"]
                or not str(proposal.get("method", "")).strip()
                or not str(proposal.get("creator", "")).strip()
                or proposal.get("backend") != "compare_silhouettes_v3"
                or isinstance(proposal.get("revision"), bool)
                or not isinstance(proposal.get("revision"), int)
                or proposal.get("revision", 0) < 1
                or proposal.get("authority")
                != "MACHINE_PROPOSAL_NO_REVIEW_OR_ACCEPTANCE_AUTHORITY"
                or proposal.get("approval_state") != "proposed"
                or proposal.get("confidence") not in {"low", "medium", "high"}
                or not str(proposal.get("intended_use", "")).strip()
                or not str(proposal.get("created_at", "")).strip()
                or record.get("status") not in {"PROPOSED", "APPROVED", "REJECTED"}
                or (
                    record.get("status") == "PROPOSED"
                    and (
                        record.get("decision_digest") is not None
                        or record.get("approved_mask_id") is not None
                    )
                )
                or (
                    record.get("status") == "APPROVED"
                    and (
                        not record.get("decision_digest")
                        or not record.get("approved_mask_id")
                    )
                )
                or (
                    record.get("status") == "REJECTED"
                    and (
                        not record.get("decision_digest")
                        or record.get("approved_mask_id") is not None
                    )
                )
            ):
                return {"valid": False, "receipt_valid": False, "replay_valid": False}
            visible = self._normalize_components(
                proposal["visible_components"], field="visible_components"
            )
            excluded = self._normalize_components(
                proposal["excluded_components"], field="excluded_components"
            )
            if (
                visible != proposal["visible_components"]
                or excluded != proposal["excluded_components"]
                or set(visible) & set(excluded)
            ):
                return {"valid": False, "receipt_valid": False, "replay_valid": False}
            receipt_path = self.artifacts.path_for(record["proposal_digest"])
            receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
            receipt_valid = canonical_json(receipt) == canonical_json(proposal)
            with self.project.connection() as connection:
                reference = connection.execute(
                    "SELECT artifact_digest FROM reference_items WHERE id=?",
                    (record["reference_id"],),
                ).fetchone()
            if (
                not receipt_valid
                or reference is None
                or reference["artifact_digest"] != proposal["reference_artifact_digest"]
                or sha256_file(self.artifacts.path_for(record["proposal_digest"]))[0]
                != record["proposal_digest"]
                or sha256_file(self.artifacts.path_for(record["mask_artifact_digest"]))[0]
                != record["mask_artifact_digest"]
            ):
                return {"valid": False, "receipt_valid": False, "replay_valid": False}
            from blender_vision.comparison.metrics import (
                BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION,
                _reference_mask,
            )

            if (
                proposal["automatic_segmentation_maximum_dimension"]
                != BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
            ):
                return {"valid": False, "receipt_valid": receipt_valid, "replay_valid": False}

            with Image.open(self.artifacts.path_for(reference["artifact_digest"])) as image:
                reference_size = ImageOps.exif_transpose(image).size
                if self._normalize_roi(proposal["roi"], reference_size) != proposal["roi"]:
                    return {
                        "valid": False,
                        "receipt_valid": receipt_valid,
                        "replay_valid": False,
                    }
                replayed, method, confidence = _reference_mask(
                    image,
                    automatic_segmentation_maximum_dimension=int(
                        proposal["automatic_segmentation_maximum_dimension"]
                    ),
                )
            with tempfile.TemporaryDirectory(prefix="bvmcp-mask-proposal-") as temporary:
                replay_path = Path(temporary) / "mask.png"
                replayed.save(replay_path, format="PNG", optimize=True)
                replay_digest = sha256_file(replay_path)[0]
            replay_valid = bool(
                replay_digest == record["mask_artifact_digest"]
                and str(method) == proposal["method"]
                and str(confidence) == proposal["confidence"]
            )
            return {
                "valid": bool(receipt_valid and replay_valid),
                "receipt_valid": receipt_valid,
                "replay_valid": replay_valid,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "receipt_valid": False, "replay_valid": False}

    def verify_decision(self, proposal_id: str) -> dict[str, Any]:
        try:
            record = self.get_proposal(proposal_id)
            proposal_verification = self.verify_proposal(proposal_id)
            digest = record.get("decision_digest")
            if (
                not proposal_verification["valid"]
                or not digest
                or sha256_file(self.artifacts.path_for(digest))[0] != digest
            ):
                return {"valid": False}
            decision = json.loads(
                self.artifacts.path_for(digest).read_text(encoding="utf-8")
            )
            valid = bool(
                decision.get("schema_version") == 1
                and decision.get("receipt_type") == "reference_mask_proposal_decision"
                and decision.get("proposal_id") == proposal_id
                and decision.get("proposal_digest") == record["proposal_digest"]
                and decision.get("reference_id") == record["reference_id"]
                and decision.get("mask_artifact_digest") == record["mask_artifact_digest"]
                and decision.get("decision") == record["status"]
                and str(decision.get("id", "")).strip()
                and str(decision.get("reviewer", "")).strip()
                and str(decision.get("reason", "")).strip()
                and str(decision.get("created_at", "")).strip()
                and decision.get("authority") == "NAMED_MASK_REVIEW_DECISION"
            )
            if record["status"] == "APPROVED":
                mask = self._get_mask(str(record["approved_mask_id"]))
                proposal = record["proposal"]
                approved = decision.get("approved_mask")
                approved_fields = {
                    "id",
                    "reference_id",
                    "artifact_digest",
                    "source_artifact_digest",
                    "method",
                    "reviewer",
                    "reason",
                    "creator",
                    "backend",
                    "revision",
                    "approval_state",
                    "confidence",
                    "intended_use",
                    "visible_components",
                    "excluded_components",
                    "roi",
                    "created_at",
                }
                valid = bool(
                    valid
                    and isinstance(approved, dict)
                    and set(approved) == approved_fields
                    and mask["proposal_id"] == proposal_id
                    and mask["decision_digest"] == digest
                    and canonical_json({key: mask[key] for key in approved_fields})
                    == canonical_json(approved)
                    and approved["id"] == record["approved_mask_id"]
                    and approved["reference_id"] == proposal["reference_id"]
                    and approved["artifact_digest"] == proposal["mask_artifact_digest"]
                    and approved["source_artifact_digest"]
                    == proposal["mask_artifact_digest"]
                    and approved["method"] == "human_reviewed_machine_proposal"
                    and approved["reviewer"] == decision["reviewer"]
                    and approved["reason"] == decision["reason"]
                    and approved["creator"] == proposal["creator"]
                    and approved["backend"] == proposal["backend"]
                    and approved["approval_state"] == "approved"
                    and approved["confidence"] == "high"
                    and approved["intended_use"] == proposal["intended_use"]
                    and approved["visible_components"] == proposal["visible_components"]
                    and approved["excluded_components"] == proposal["excluded_components"]
                    and approved["roi"] == proposal["roi"]
                    and approved["created_at"] == decision["created_at"]
                )
            elif record["status"] == "REJECTED":
                valid = bool(
                    valid
                    and record.get("approved_mask_id") is None
                    and decision.get("approved_mask") is None
                )
            else:
                valid = False
            return {"valid": valid, "decision": decision}
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False}

    def verify_approved_mask(self, mask: dict[str, Any]) -> bool:
        proposal_id = mask.get("proposal_id")
        if not proposal_id:
            return True
        decision = self.verify_decision(str(proposal_id))
        return bool(decision["valid"])

    @staticmethod
    def _normalize_components(values: list[str] | None, *, field: str) -> list[str]:
        if values is None:
            return []
        if not isinstance(values, list) or any(not isinstance(value, str) for value in values):
            raise ValueError(f"{field} must be a list of component IDs")
        normalized = sorted({value.strip() for value in values})
        if any(not value for value in normalized):
            raise ValueError(f"{field} cannot contain blank component IDs")
        return normalized

    @staticmethod
    def _normalize_roi(roi: dict[str, int], size: tuple[int, int]) -> dict[str, int]:
        keys = {"x", "y", "width", "height"}
        if not isinstance(roi, dict) or set(roi) != keys:
            raise ValueError("reference-mask ROI requires exactly x, y, width, and height")
        if any(isinstance(roi[key], bool) or not isinstance(roi[key], int) for key in keys):
            raise ValueError("reference-mask ROI values must be integers")
        values = {key: roi[key] for key in ("x", "y", "width", "height")}
        if any(value < 0 for value in values.values()):
            raise ValueError("reference-mask ROI requires non-negative x, y, width, and height")
        if (
            values["width"] <= 0
            or values["height"] <= 0
            or values["x"] + values["width"] > size[0]
            or values["y"] + values["height"] > size[1]
        ):
            raise ValueError("reference-mask ROI exceeds reference dimensions")
        return values

    @staticmethod
    def _validate_roi(roi: dict[str, int], size: tuple[int, int]) -> None:
        ReferenceMaskStore._normalize_roi(roi, size)

    def import_reviewed(
        self,
        reference_id: str,
        source: Path,
        *,
        reviewer: str,
        reason: str,
        creator: str | None = None,
        backend: str = "human_manual",
        confidence: str = "high",
        intended_use: str = "silhouette_evaluation",
        visible_components: list[str] | None = None,
        excluded_components: list[str] | None = None,
        roi: dict[str, int] | None = None,
    ) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("reference-mask approval requires a named reviewer and reason")
        creator = (creator or reviewer).strip()
        if not creator or not backend.strip() or not intended_use.strip():
            raise ValueError("reference-mask governance fields cannot be blank")
        if confidence not in {"low", "medium", "high"}:
            raise ValueError("reference-mask confidence must be low, medium, or high")
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT relative_path FROM reference_items WHERE id=?", (reference_id,)
            ).fetchone()
        if reference is None:
            raise KeyError(f"unknown reference: {reference_id}")
        source = source.expanduser().resolve()
        if not source.is_file():
            raise FileNotFoundError(source)
        with Image.open(self.project.root / reference["relative_path"]) as reference_image:
            reference_size = ImageOps.exif_transpose(reference_image).size
        with Image.open(source) as mask_image:
            normalized = ImageOps.exif_transpose(mask_image).convert("RGBA")
            alpha = normalized.getchannel("A")
            if alpha.getextrema()[0] < alpha.getextrema()[1]:
                channel = alpha
            else:
                channel = normalized.convert("L")
            if channel.size != reference_size:
                raise ValueError(
                    "reviewed reference mask dimensions must exactly match the reference image"
                )
            binary = channel.point(lambda value: 255 if value >= 128 else 0)
        foreground = sum(binary.tobytes()) // 255
        total = binary.width * binary.height
        if foreground <= 0 or foreground >= total:
            raise ValueError("reviewed reference mask must contain foreground and background")
        source_artifact = self.artifacts.ingest_file(source)
        mask_id = str(uuid.uuid4())
        destination = self.project.root / "references" / "masks" / f"{reference_id}-{mask_id}.png"
        destination.parent.mkdir(parents=True, exist_ok=True)
        binary.save(destination, format="PNG", optimize=True)
        artifact = self.artifacts.ingest_file(destination, media_type="image/png")
        created_at = utc_now()
        with self.project.connection() as connection:
            revision = connection.execute(
                "SELECT COALESCE(MAX(revision),0)+1 FROM reference_masks WHERE reference_id=?",
                (reference_id,),
            ).fetchone()[0]
            governed_roi = roi or {
                "x": 0,
                "y": 0,
                "width": reference_size[0],
                "height": reference_size[1],
            }
            if any(int(governed_roi.get(key, -1)) < 0 for key in ("x", "y", "width", "height")):
                raise ValueError("reference-mask ROI requires non-negative x, y, width, and height")
            connection.execute(
                "INSERT INTO reference_masks(id,reference_id,artifact_digest,"
                "source_artifact_digest,method,reviewer,reason,creator,backend,revision,"
                "approval_state,confidence,intended_use,visible_components_json,"
                "excluded_components_json,roi_json,created_at) "
                "VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    mask_id,
                    reference_id,
                    artifact.digest,
                    source_artifact.digest,
                    "reviewed_manual_mask",
                    reviewer.strip(),
                    reason.strip(),
                    creator,
                    backend.strip(),
                    revision,
                    "approved",
                    confidence,
                    intended_use.strip(),
                    json.dumps(sorted(set(visible_components or []))),
                    json.dumps(sorted(set(excluded_components or []))),
                    json.dumps(governed_roi),
                    created_at,
                ),
            )
        return self.get(mask_id)

    def _get_mask(self, mask_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM reference_masks WHERE id=?", (mask_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown reference mask: {mask_id}")
        value = dict(row)
        value["visible_components"] = json.loads(value.pop("visible_components_json"))
        value["excluded_components"] = json.loads(value.pop("excluded_components_json"))
        value["roi"] = json.loads(value.pop("roi_json"))
        value["status"] = value["approval_state"]
        return value

    def latest(self, reference_id: str) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT id FROM reference_masks WHERE reference_id=? "
                "ORDER BY created_at DESC,id DESC LIMIT 1",
                (reference_id,),
            ).fetchone()
        return self.get(row["id"]) if row else None

    def list(self, reference_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            if reference_id:
                rows = connection.execute(
                    "SELECT id FROM reference_masks WHERE reference_id=? ORDER BY created_at,id",
                    (reference_id,),
                ).fetchall()
            else:
                rows = connection.execute(
                    "SELECT id FROM reference_masks ORDER BY created_at,id"
                ).fetchall()
        return [self.get(row["id"]) for row in rows]
