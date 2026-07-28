from __future__ import annotations

import hashlib
import itertools
import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.projects.store import ProjectStore


def _numeric_vector(value: Any, length: int, label: str) -> list[float]:
    if (
        not isinstance(value, list)
        or len(value) != length
        or not all(isinstance(item, (int, float)) for item in value)
    ):
        raise ValueError(f"{label} must contain {length} numeric values")
    result = [float(item) for item in value]
    if not all(math.isfinite(item) for item in result):
        raise ValueError(f"{label} contains a non-finite value")
    return result


def _noncoplanar(points: list[list[float]]) -> bool:
    if len(points) < 4:
        return False
    extent = max(
        max(point[axis] for point in points) - min(point[axis] for point in points)
        for axis in range(3)
    )
    threshold = max(1e-9, extent**3 * 1e-9)
    for origin, first, second, third in itertools.combinations(points, 4):
        left = [first[index] - origin[index] for index in range(3)]
        middle = [second[index] - origin[index] for index in range(3)]
        right = [third[index] - origin[index] for index in range(3)]
        cross = [
            middle[1] * right[2] - middle[2] * right[1],
            middle[2] * right[0] - middle[0] * right[2],
            middle[0] * right[1] - middle[1] * right[0],
        ]
        volume = abs(sum(left[index] * cross[index] for index in range(3)))
        if volume > threshold:
            return True
    return False


class CameraLandmarkStore:
    """Govern machine-proposed image/model landmarks before metric PnP recovery."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def propose(
        self,
        *,
        target_id: str,
        model_source_id: str,
        intrinsics_solution_id: str,
        evidence_binding_ids: list[str],
        views: list[dict[str, Any]],
        backend_identity: dict[str, Any],
        known_limitations: list[str],
    ) -> dict[str, Any]:
        if not views:
            raise ValueError("landmark proposal requires at least one image view")
        if not isinstance(backend_identity, dict) or not str(
            backend_identity.get("name", "")
        ).strip():
            raise ValueError("landmark proposal requires a named backend identity")
        if not known_limitations or any(
            not str(item).strip() for item in known_limitations
        ):
            raise ValueError("landmark proposal requires explicit known limitations")
        model_source = EvidenceAcquisitionStore(self.project).get(model_source_id)
        self._validate_governed_source(model_source, target_id=target_id, model=True)
        with self.project.connection() as connection:
            solution_row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?",
                (intrinsics_solution_id,),
            ).fetchone()
            measurement_ids = {
                row[0] for row in connection.execute("SELECT id FROM measurements")
            }
        if solution_row is None:
            raise KeyError(f"unknown intrinsics solution: {intrinsics_solution_id}")
        if not evidence_binding_ids or not set(evidence_binding_ids).issubset(measurement_ids):
            raise ValueError("landmark proposal evidence bindings must reference measurements")
        solution = json.loads(solution_row[0])
        cameras = {item["reference_id"]: item for item in solution.get("cameras", [])}
        normalized_views = []
        seen_references: set[str] = set()
        for view in views:
            image_source_id = str(view.get("image_source_id", "")).strip()
            image_source = EvidenceAcquisitionStore(self.project).get(image_source_id)
            self._validate_governed_source(image_source, target_id=target_id, model=False)
            reference_id = str(image_source["reference_id"])
            if reference_id in seen_references:
                raise ValueError("landmark proposal contains a duplicate image reference")
            seen_references.add(reference_id)
            camera = cameras.get(reference_id)
            if camera is None:
                raise ValueError("intrinsics solution does not cover every proposed image")
            if camera.get("model") not in {"PINHOLE", "SIMPLE_PINHOLE"}:
                raise ValueError("landmark proposals require pinhole intrinsics")
            correspondences = view.get("correspondences")
            if not isinstance(correspondences, list) or len(correspondences) < 6:
                raise ValueError("each landmark proposal view requires at least six points")
            width, height = int(camera["width"]), int(camera["height"])
            normalized_points = []
            landmark_ids: set[str] = set()
            for point in correspondences:
                landmark_id = str(point.get("landmark_id", "")).strip()
                if not landmark_id or landmark_id in landmark_ids:
                    raise ValueError("proposed landmark ids must be unique and non-empty per view")
                landmark_ids.add(landmark_id)
                world = _numeric_vector(point.get("world_mm"), 3, "world_mm")
                image = _numeric_vector(point.get("image_px"), 2, "image_px")
                if not 0.0 <= image[0] < width or not 0.0 <= image[1] < height:
                    raise ValueError("proposed image landmark lies outside its image")
                confidence = float(point.get("confidence", -1.0))
                if not math.isfinite(confidence) or not 0.0 <= confidence <= 1.0:
                    raise ValueError("proposed landmark confidence must be between zero and one")
                method = str(point.get("method", "")).strip()
                if not method:
                    raise ValueError("proposed landmark requires a method")
                normalized_points.append(
                    {
                        "landmark_id": landmark_id,
                        "world_mm": world,
                        "image_px": image,
                        "confidence": confidence,
                        "method": method,
                    }
                )
            normalized_views.append(
                {
                    "image_source_id": image_source_id,
                    "reference_id": reference_id,
                    "image_artifact_digest": image_source["source"]["content_hash"],
                    "correspondences": normalized_points,
                }
            )
        proposal_id = str(uuid.uuid4())
        now = utc_now()
        proposal = {
            "schema_version": 1,
            "receipt_type": "camera_landmark_proposal",
            "id": proposal_id,
            "target_id": target_id,
            "model_source_id": model_source_id,
            "model_reference_id": model_source["reference_id"],
            "model_artifact_digest": model_source["source"]["content_hash"],
            "intrinsics_solution_id": intrinsics_solution_id,
            "intrinsics_snapshot_sha256": hashlib.sha256(canonical_json(solution)).hexdigest(),
            "evidence_binding_ids": sorted(set(evidence_binding_ids)),
            "backend_identity": backend_identity,
            "known_limitations": [str(item).strip() for item in known_limitations],
            "views": normalized_views,
            "authority": "MACHINE_PROPOSAL_NOT_REVIEWED",
            "camera_acceptance_performed": False,
            "created_at": now,
        }
        relative = Path("receipts") / f"camera-landmark-proposal-{proposal_id}.json"
        atomic_write_json(self.project.root / relative, proposal)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.camera-landmark-proposal+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO camera_landmark_proposals("
                "id,target_id,model_source_id,intrinsics_solution_id,status,proposal_json,"
                "proposal_digest,review_json,review_digest,created_at,updated_at) "
                "VALUES(?,?,?,?, 'PROPOSED',?,?,NULL,NULL,?,?)",
                (
                    proposal_id,
                    target_id,
                    model_source_id,
                    intrinsics_solution_id,
                    json.dumps(proposal),
                    artifact.digest,
                    now,
                    now,
                ),
            )
        return {**proposal, "status": "PROPOSED", "proposal_digest": artifact.digest}

    def review(
        self,
        proposal_id: str,
        *,
        reviewer: str,
        reason: str,
        decisions: list[dict[str, Any]],
    ) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("landmark review requires a named reviewer and reason")
        record = self.get(proposal_id, verify=True)
        if record["status"] != "PROPOSED":
            raise ValueError("landmark proposal has already been reviewed")
        proposed = {
            (view["reference_id"], point["landmark_id"]): (view, point)
            for view in record["proposal"]["views"]
            for point in view["correspondences"]
        }
        if len(decisions) != len(proposed):
            raise ValueError("landmark review must decide every proposed correspondence")
        normalized_decisions = []
        accepted_by_reference: dict[str, list[dict[str, Any]]] = {
            view["reference_id"]: [] for view in record["proposal"]["views"]
        }
        seen: set[tuple[str, str]] = set()
        for decision in decisions:
            key = (
                str(decision.get("reference_id", "")).strip(),
                str(decision.get("landmark_id", "")).strip(),
            )
            if key in seen or key not in proposed:
                raise ValueError("landmark review contains an unknown or duplicate decision")
            seen.add(key)
            outcome = str(decision.get("decision", "")).lower()
            if outcome not in {"accept", "reject", "correct"}:
                raise ValueError("landmark decision must be accept, reject, or correct")
            view, point = proposed[key]
            reviewed_point = {
                "landmark_id": key[1],
                "world_mm": list(point["world_mm"]),
                "image_px": list(point["image_px"]),
            }
            if outcome == "correct":
                reviewed_point["world_mm"] = _numeric_vector(
                    decision.get("world_mm"), 3, "corrected world_mm"
                )
                reviewed_point["image_px"] = _numeric_vector(
                    decision.get("image_px"), 2, "corrected image_px"
                )
                with self.project.connection() as connection:
                    camera_row = connection.execute(
                        "SELECT solution_json FROM camera_solutions WHERE id=?",
                        (record["intrinsics_solution_id"],),
                    ).fetchone()
                camera = next(
                    item
                    for item in json.loads(camera_row[0])["cameras"]
                    if item["reference_id"] == key[0]
                )
                x, y = reviewed_point["image_px"]
                if not 0.0 <= x < camera["width"] or not 0.0 <= y < camera["height"]:
                    raise ValueError("corrected image landmark lies outside its image")
            if outcome != "reject":
                accepted_by_reference[key[0]].append(reviewed_point)
            normalized_decisions.append(
                {
                    "reference_id": key[0],
                    "landmark_id": key[1],
                    "decision": outcome,
                    "reviewed_point": reviewed_point if outcome != "reject" else None,
                }
            )
        accepted_views = []
        insufficient = []
        for view in record["proposal"]["views"]:
            points = sorted(
                accepted_by_reference[view["reference_id"]],
                key=lambda item: item["landmark_id"],
            )
            if len(points) < 6 or not _noncoplanar([item["world_mm"] for item in points]):
                insufficient.append(view["reference_id"])
            accepted_views.append(
                {"reference_id": view["reference_id"], "correspondences": points}
            )
        status = "READY_FOR_PNP" if not insufficient else "REVIEWED_INSUFFICIENT"
        now = utc_now()
        review_id = str(uuid.uuid4())
        review = {
            "schema_version": 1,
            "receipt_type": "camera_landmark_review",
            "id": review_id,
            "proposal_id": proposal_id,
            "proposal_digest": record["proposal_digest"],
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "decisions": sorted(
                normalized_decisions,
                key=lambda item: (item["reference_id"], item["landmark_id"]),
            ),
            "accepted_views": accepted_views,
            "insufficient_reference_ids": insufficient,
            "status": status,
            "camera_acceptance_performed": False,
            "created_at": now,
        }
        relative = Path("receipts") / f"camera-landmark-review-{review_id}.json"
        atomic_write_json(self.project.root / relative, review)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.camera-landmark-review+json",
        )
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE camera_landmark_proposals SET status=?,review_json=?,review_digest=?,"
                "updated_at=? WHERE id=? AND status='PROPOSED'",
                (status, json.dumps(review), artifact.digest, now, proposal_id),
            )
        if updated.rowcount != 1:
            raise RuntimeError("landmark proposal review raced with another reviewer")
        return {**review, "review_digest": artifact.digest}

    def reviewed_pnp_input(self, proposal_id: str) -> dict[str, Any]:
        record = self.get(proposal_id, verify=True)
        if record["status"] != "READY_FOR_PNP" or not record["review"]:
            raise ValueError("landmark proposal lacks a sufficient immutable named review")
        self._verify_live_bindings(record)
        return {
            "intrinsics_solution_id": record["intrinsics_solution_id"],
            "views": record["review"]["accepted_views"],
            "evidence_binding_ids": record["proposal"]["evidence_binding_ids"],
            "reviewed_by": record["review"]["reviewer"],
            "review_id": record["review"]["id"],
            "review_digest": record["review_digest"],
        }

    def supersede_machine_proposal(
        self,
        proposal_id: str,
        *,
        replacement_id: str,
        reason: str,
    ) -> dict[str, Any]:
        """Replace an unreviewed machine proposal without creating review authority."""
        if proposal_id == replacement_id:
            raise ValueError("landmark proposal cannot supersede itself")
        if not reason.strip():
            raise ValueError("landmark proposal supersession requires a reason")
        record = self.get(proposal_id, verify=True)
        replacement = self.get(replacement_id, verify=True)
        if record["status"] != "PROPOSED" or replacement["status"] != "PROPOSED":
            raise ValueError("only unreviewed proposed landmarks may be superseded")
        if record["review"] is not None or replacement["review"] is not None:
            raise ValueError("reviewed landmark proposals cannot be machine-superseded")
        binding_fields = ("target_id", "model_source_id", "intrinsics_solution_id")
        if any(record[field] != replacement[field] for field in binding_fields):
            raise ValueError("landmark proposal replacement has different evidence bindings")

        supersession_id = str(uuid.uuid4())
        now = utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "camera_landmark_proposal_supersession",
            "id": supersession_id,
            "proposal_id": proposal_id,
            "proposal_digest": record["proposal_digest"],
            "superseded_by_id": replacement_id,
            "superseded_by_digest": replacement["proposal_digest"],
            "reason": reason.strip(),
            "authority": "MACHINE_PROPOSAL_REPLACEMENT_NO_REVIEW_AUTHORITY",
            "camera_acceptance_performed": False,
            "created_at": now,
        }
        relative = Path("receipts") / f"camera-landmark-supersession-{supersession_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.camera-landmark-supersession+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            updated = connection.execute(
                "UPDATE camera_landmark_proposals SET status='SUPERSEDED',"
                "superseded_by_id=?,supersession_digest=?,updated_at=? "
                "WHERE id=? AND status='PROPOSED' AND review_json IS NULL "
                "AND review_digest IS NULL AND superseded_by_id IS NULL "
                "AND supersession_digest IS NULL",
                (replacement_id, artifact.digest, now, proposal_id),
            )
            if updated.rowcount != 1:
                raise RuntimeError("landmark proposal supersession raced with another action")
        return {**receipt, "supersession_digest": artifact.digest}

    def get(self, proposal_id: str, *, verify: bool = False) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM camera_landmark_proposals WHERE id=?", (proposal_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown landmark proposal: {proposal_id}")
        value = dict(row)
        value["proposal"] = json.loads(value.pop("proposal_json"))
        review_json = value.pop("review_json")
        value["review"] = json.loads(review_json) if review_json else None
        value["supersession"] = None
        if verify:
            self._verify_artifact(value["proposal_digest"], value["proposal"])
            if value["review"]:
                self._verify_artifact(value["review_digest"], value["review"])
                if value["review"].get("proposal_digest") != value["proposal_digest"]:
                    raise ValueError("landmark review does not bind the current proposal receipt")
                self._verify_review_semantics(value)
            if value.get("supersession_digest"):
                supersession_path = self.artifacts.path_for(value["supersession_digest"])
                if not supersession_path.is_file():
                    raise ValueError("landmark supersession receipt is missing")
                supersession = json.loads(
                    supersession_path.read_text(encoding="utf-8")
                )
                self._verify_artifact(value["supersession_digest"], supersession)
                value["supersession"] = supersession
                self._verify_supersession_semantics(value)
            elif value["status"] == "SUPERSEDED" or value.get("superseded_by_id"):
                raise ValueError("superseded landmark proposal lacks an immutable receipt")
        return value

    def list(self, target_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            if target_id:
                rows = connection.execute(
                    "SELECT id FROM camera_landmark_proposals WHERE target_id=? "
                    "ORDER BY created_at,id",
                    (target_id,),
                ).fetchall()
            else:
                rows = connection.execute(
                    "SELECT id FROM camera_landmark_proposals ORDER BY created_at,id"
                ).fetchall()
        return [self.get(row["id"]) for row in rows]

    def _verify_artifact(self, digest: str | None, document: dict[str, Any]) -> None:
        if not digest:
            raise ValueError("landmark record is missing its artifact digest")
        path = self.artifacts.path_for(digest)
        if not path.is_file() or sha256_file(path)[0] != digest:
            raise ValueError("landmark receipt artifact is missing or corrupt")
        artifact_document = json.loads(path.read_text(encoding="utf-8"))
        if canonical_json(artifact_document) != canonical_json(document):
            raise ValueError("landmark database record disagrees with its immutable receipt")

    def _verify_live_bindings(self, record: dict[str, Any]) -> None:
        proposal = record["proposal"]
        with self.project.connection() as connection:
            solution_row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?",
                (record["intrinsics_solution_id"],),
            ).fetchone()
        if solution_row is None:
            raise ValueError("landmark proposal intrinsics solution no longer exists")
        solution = json.loads(solution_row[0])
        if hashlib.sha256(canonical_json(solution)).hexdigest() != proposal.get(
            "intrinsics_snapshot_sha256"
        ):
            raise ValueError("landmark proposal intrinsics snapshot is stale")
        model_source = EvidenceAcquisitionStore(self.project).get(record["model_source_id"])
        if model_source["source"].get("content_hash") != proposal.get(
            "model_artifact_digest"
        ):
            raise ValueError("landmark proposal model binding is stale")
        for view in proposal["views"]:
            image_source = EvidenceAcquisitionStore(self.project).get(view["image_source_id"])
            if (
                image_source.get("reference_id") != view["reference_id"]
                or image_source["source"].get("content_hash")
                != view["image_artifact_digest"]
            ):
                raise ValueError("landmark proposal image binding is stale")

    @staticmethod
    def _verify_review_semantics(record: dict[str, Any]) -> None:
        proposal = record["proposal"]
        review = record["review"]
        if (
            review.get("proposal_id") != record["id"]
            or not str(review.get("reviewer", "")).strip()
            or not str(review.get("reason", "")).strip()
            or review.get("camera_acceptance_performed") is not False
        ):
            raise ValueError("landmark review identity or authority boundary is invalid")
        proposed = {
            (view["reference_id"], point["landmark_id"]): point
            for view in proposal["views"]
            for point in view["correspondences"]
        }
        decisions = review.get("decisions")
        if not isinstance(decisions, list) or len(decisions) != len(proposed):
            raise ValueError("landmark review does not decide the complete proposal")
        accepted: dict[str, list[dict[str, Any]]] = {
            view["reference_id"]: [] for view in proposal["views"]
        }
        seen: set[tuple[str, str]] = set()
        for decision in decisions:
            key = (
                str(decision.get("reference_id", "")),
                str(decision.get("landmark_id", "")),
            )
            outcome = decision.get("decision")
            if key in seen or key not in proposed or outcome not in {
                "accept",
                "reject",
                "correct",
            }:
                raise ValueError("landmark review contains an invalid decision")
            seen.add(key)
            reviewed_point = decision.get("reviewed_point")
            if outcome == "reject":
                if reviewed_point is not None:
                    raise ValueError("rejected landmark unexpectedly retains a reviewed point")
                continue
            if not isinstance(reviewed_point, dict) or reviewed_point.get(
                "landmark_id"
            ) != key[1]:
                raise ValueError("accepted landmark is missing its reviewed point")
            point = {
                "landmark_id": key[1],
                "world_mm": _numeric_vector(
                    reviewed_point.get("world_mm"), 3, "reviewed world_mm"
                ),
                "image_px": _numeric_vector(
                    reviewed_point.get("image_px"), 2, "reviewed image_px"
                ),
            }
            if outcome == "accept" and (
                point["world_mm"] != proposed[key]["world_mm"]
                or point["image_px"] != proposed[key]["image_px"]
            ):
                raise ValueError("accepted landmark silently changes proposed coordinates")
            accepted[key[0]].append(point)
        accepted_views = [
            {
                "reference_id": view["reference_id"],
                "correspondences": sorted(
                    accepted[view["reference_id"]],
                    key=lambda item: item["landmark_id"],
                ),
            }
            for view in proposal["views"]
        ]
        insufficient = [
            item["reference_id"]
            for item in accepted_views
            if len(item["correspondences"]) < 6
            or not _noncoplanar(
                [point["world_mm"] for point in item["correspondences"]]
            )
        ]
        expected_status = "READY_FOR_PNP" if not insufficient else "REVIEWED_INSUFFICIENT"
        if (
            canonical_json(review.get("accepted_views")) != canonical_json(accepted_views)
            or review.get("insufficient_reference_ids") != insufficient
            or review.get("status") != expected_status
            or record["status"] != expected_status
        ):
            raise ValueError("landmark review outcome contradicts its decisions")

    def _verify_supersession_semantics(self, record: dict[str, Any]) -> None:
        receipt = record["supersession"]
        replacement_id = record.get("superseded_by_id")
        with self.project.connection() as connection:
            replacement = connection.execute(
                "SELECT id,target_id,model_source_id,intrinsics_solution_id,"
                "proposal_digest FROM camera_landmark_proposals WHERE id=?",
                (replacement_id,),
            ).fetchone()
        if replacement is None:
            raise ValueError("landmark supersession replacement no longer exists")
        if (
            record["status"] != "SUPERSEDED"
            or record["review"] is not None
            or record.get("review_digest") is not None
            or receipt.get("receipt_type")
            != "camera_landmark_proposal_supersession"
            or receipt.get("proposal_id") != record["id"]
            or receipt.get("proposal_digest") != record["proposal_digest"]
            or receipt.get("superseded_by_id") != replacement_id
            or receipt.get("superseded_by_digest") != replacement["proposal_digest"]
            or receipt.get("authority")
            != "MACHINE_PROPOSAL_REPLACEMENT_NO_REVIEW_AUTHORITY"
            or receipt.get("camera_acceptance_performed") is not False
            or not str(receipt.get("reason", "")).strip()
            or any(
                record[field] != replacement[field]
                for field in ("target_id", "model_source_id", "intrinsics_solution_id")
            )
        ):
            raise ValueError("landmark supersession receipt is semantically invalid")

    def _validate_governed_source(
        self, source: dict[str, Any], *, target_id: str, model: bool
    ) -> None:
        if source["target_id"] != target_id or source["status"] != "ACQUIRED":
            raise ValueError("landmark source must be acquired for the exact target")
        if not EvidenceAcquisitionStore(self.project).authority_status(source["id"])[
            "acquisition_valid"
        ]:
            raise ValueError("landmark source acquisition receipt is invalid")
        if not source.get("reviewed_by") or not source.get("reviewed_at"):
            raise ValueError("landmark source requires a named governance review")
        if not source["rights"].get("internal_use"):
            raise PermissionError("landmark source rights prohibit reconstruction use")
        reference_id = source.get("reference_id")
        if not reference_id:
            raise ValueError("landmark source is missing a reference")
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT media_type,acceptance_eligible FROM reference_items WHERE id=?",
                (reference_id,),
            ).fetchone()
        if reference is None:
            raise ValueError("landmark source reference does not exist")
        if model:
            if not (
                str(reference["media_type"]).startswith("model/")
                or reference["media_type"] == "application/x-blender"
            ):
                raise ValueError("model landmark source must be a governed 3D model")
        elif not str(reference["media_type"]).startswith("image/") or not bool(
            reference["acceptance_eligible"]
        ):
            raise ValueError("image landmark source must be acceptance-eligible image evidence")
