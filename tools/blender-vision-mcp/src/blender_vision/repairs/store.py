from __future__ import annotations

import json
import uuid
from typing import Any

from blender_vision.core.errors import ProjectError
from blender_vision.core.util import utc_now
from blender_vision.projects.store import ProjectStore


class RepairStore:
    def __init__(self, project: ProjectStore):
        self.project = project

    def propose(
        self,
        kind: str,
        config: dict[str, Any],
        *,
        evidence_bindings: list[dict[str, Any]],
        expected_improvement: dict[str, Any],
    ) -> dict[str, Any]:
        proposal_id = str(uuid.uuid4())
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO repair_proposals("
                "id,kind,status,config_json,evidence_json,expected_json,created_at,updated_at"
                ") VALUES(?,?,?,?,?,?,?,?)",
                (
                    proposal_id,
                    kind,
                    "proposed",
                    json.dumps(config),
                    json.dumps(evidence_bindings),
                    json.dumps(expected_improvement),
                    now,
                    now,
                ),
            )
        return self.get(proposal_id)

    def approve(self, proposal_id: str, approved_by: str) -> dict[str, Any]:
        """Authorize an immutable checkpoint evaluation, not final geometry acceptance."""
        if not approved_by.strip():
            raise ValueError("approved_by must identify the approving actor")
        now = utc_now()
        with self.project.connection() as connection:
            result = connection.execute(
                "UPDATE repair_proposals SET status='approved',approved_by=?,approved_at=?,"
                "updated_at=? WHERE id=? AND status='proposed'",
                (approved_by, now, now, proposal_id),
            )
            if result.rowcount != 1:
                raise ProjectError(f"repair proposal cannot be approved: {proposal_id}")
        return self.get(proposal_id)

    def reject_proposed(self, proposal_id: str, *, reviewer: str, reason: str) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("repair rejection requires a named reviewer and reason")
        now = utc_now()
        result = {
            "acceptance": {
                "accepted": False,
                "state": "rejected_before_evaluation",
                "reviewer": reviewer.strip(),
                "reason": reason.strip(),
                "reviewed_at": now,
            }
        }
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE repair_proposals SET status='rejected',result_json=?,updated_at=? "
                "WHERE id=? AND status='proposed'",
                (json.dumps(result), now, proposal_id),
            )
            if updated.rowcount != 1:
                raise ProjectError(f"repair proposal cannot be rejected: {proposal_id}")
        return self.get(proposal_id)

    def review_applied(
        self,
        proposal_id: str,
        *,
        accepted: bool,
        reviewer: str,
        reason: str,
        receipt_id: str | None = None,
        evidence: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Record the final named decision for already-generated checkpoint geometry.

        Higher-level services are responsible for validating an acceptance receipt before
        calling this method with ``accepted=True``. The store preserves that validation evidence
        in the proposal result so receipts can distinguish evaluation authorization from final
        geometry acceptance.
        """
        if not reviewer.strip():
            raise ValueError("reviewer must identify the reviewing actor")
        if not reason.strip():
            raise ValueError("reason is required for a repair review")
        if accepted and not receipt_id:
            raise ValueError("accepted repair reviews require an acceptance receipt id")
        proposal = self.get(proposal_id)
        if proposal["status"] != "applied":
            raise ProjectError(f"repair proposal is not awaiting review: {proposal_id}")
        result = proposal.get("result")
        if not isinstance(result, dict):
            raise ProjectError(f"repair proposal has no applied result: {proposal_id}")
        now = utc_now()
        result["acceptance"] = {
            "accepted": accepted,
            "state": "accepted" if accepted else "rejected",
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "reviewed_at": now,
            "receipt_id": receipt_id,
            "evidence": evidence or {},
        }
        status = "accepted" if accepted else "rejected"
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE repair_proposals SET status=?,result_json=?,updated_at=? "
                "WHERE id=? AND status='applied'",
                (status, json.dumps(result), now, proposal_id),
            )
            if updated.rowcount != 1:
                raise ProjectError(
                    f"repair proposal review raced with another decision: {proposal_id}"
                )
        return self.get(proposal_id)

    def mark_applied(self, proposal_id: str, result: dict[str, Any]) -> dict[str, Any]:
        now = utc_now()
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE repair_proposals SET status='applied',result_json=?,updated_at=? "
                "WHERE id=? AND status='approved'",
                (json.dumps(result), now, proposal_id),
            )
            if updated.rowcount != 1:
                raise ProjectError(f"repair proposal is not approved: {proposal_id}")
        return self.get(proposal_id)

    def get(self, proposal_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM repair_proposals WHERE id=?", (proposal_id,)
            ).fetchone()
        if row is None:
            raise ProjectError(f"unknown repair proposal: {proposal_id}")
        value = dict(row)
        for key in ("config_json", "evidence_json", "expected_json", "result_json"):
            raw = value.pop(key)
            value[key.removesuffix("_json")] = json.loads(raw) if raw else None
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row["id"]
                for row in connection.execute(
                    "SELECT id FROM repair_proposals ORDER BY created_at"
                ).fetchall()
            ]
        return [self.get(proposal_id) for proposal_id in ids]
