from __future__ import annotations

import hashlib
import json
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, utc_now
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.coverage_atlas import SurfaceCoverageAtlas
from blender_vision.evidence.discovery import SearchProviderStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore


class EvidencePursuitStore:
    """Persist one bounded attempt to resolve current coverage gaps without granting rights."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def pursue(
        self,
        target_id: str | None = None,
        *,
        category: str = "general_product",
        provider_id: str | None = None,
        required_terms: list[str] | None = None,
        maximum_queries: int = 5,
        maximum_results_per_query: int = 10,
        timeout_seconds: float = 20.0,
    ) -> dict[str, Any]:
        target = TargetResolver(self.project).get(target_id)
        if not 1 <= maximum_queries <= 100:
            raise ValueError("evidence pursuit query budget must be between one and 100")
        if not 1 <= maximum_results_per_query <= 100:
            raise ValueError("evidence pursuit result budget must be between one and 100")
        directional = EvidenceAcquisitionStore(self.project).analyze_coverage(target["id"])
        atlas = SurfaceCoverageAtlas(self.project).analyze(target["id"])
        focus_terms = self._focus_terms(directional, atlas)
        if required_terms:
            normalized_required = EvidenceAcquisitionStore(self.project).plan_search(
                target["id"], category=category, focus_terms=required_terms
            )["focus_terms"]
            focus_terms = list(dict.fromkeys([*normalized_required, *focus_terms]))[:32]
        providers = SearchProviderStore(self.project).list()
        selected_provider_id = provider_id or (providers[-1]["id"] if providers else None)
        if selected_provider_id and selected_provider_id not in {item["id"] for item in providers}:
            raise KeyError(f"unknown search provider: {selected_provider_id}")
        stable_coverage = {
            "missing_directions": directional["missing_directions"],
            "eligible_acquired_count": directional["eligible_acquired_count"],
            "unresolved_regions": atlas["unresolved_regions"],
            "observed_cell_count": atlas["observed_cell_count"],
            "cell_count": atlas["cell_count"],
        }
        key_record = {
            "target_id": target["id"],
            "category": category,
            "provider_id": selected_provider_id,
            "focus_terms": focus_terms,
            "coverage": stable_coverage,
            "maximum_queries": maximum_queries,
            "maximum_results_per_query": maximum_results_per_query,
        }
        cache_key = hashlib.sha256(canonical_json(key_record)).hexdigest()
        pursuit_id = str(uuid.uuid4())
        lease_token = str(uuid.uuid4())
        now = utc_now()
        lease_expires_at = (datetime.now(UTC) + timedelta(minutes=5)).isoformat()
        with self.project.connection() as connection:
            inserted = connection.execute(
                "INSERT OR IGNORE INTO evidence_pursuit_runs("
                "id,cache_key,target_id,provider_id,status,focus_terms_json,coverage_json,"
                "capture_request_ids_json,lease_token,lease_expires_at,attempt_count,"
                "created_at,updated_at) "
                "VALUES(?,?,?,?, 'RUNNING',?,?, '[]',?,?,1,?,?)",
                (
                    pursuit_id,
                    cache_key,
                    target["id"],
                    selected_provider_id,
                    json.dumps(focus_terms),
                    json.dumps(stable_coverage),
                    lease_token,
                    lease_expires_at,
                    now,
                    now,
                ),
            )
            if inserted.rowcount == 0:
                existing = connection.execute(
                    "SELECT id,status,lease_token,lease_expires_at "
                    "FROM evidence_pursuit_runs WHERE cache_key=?",
                    (cache_key,),
                ).fetchone()
                if existing["status"] != "RUNNING":
                    return self.get(existing["id"])
                if (
                    existing["lease_expires_at"]
                    and datetime.fromisoformat(existing["lease_expires_at"])
                    > datetime.now(UTC)
                ):
                    return self.get(existing["id"])
                reclaimed = connection.execute(
                    "UPDATE evidence_pursuit_runs SET lease_token=?,lease_expires_at=?,"
                    "attempt_count=attempt_count+1,updated_at=? "
                    "WHERE id=? AND status='RUNNING' AND lease_token IS ?",
                    (
                        lease_token,
                        lease_expires_at,
                        now,
                        existing["id"],
                        existing["lease_token"],
                    ),
                )
                if reclaimed.rowcount != 1:
                    return self.get(existing["id"])
                pursuit_id = existing["id"]

        discovery = None
        error = None
        if focus_terms and selected_provider_id:
            provider = next(
                item for item in providers if item["id"] == selected_provider_id
            )
            try:
                discovery = SearchProviderStore(self.project).discover(
                    selected_provider_id,
                    target_id=target["id"],
                    category=category,
                    focus_terms=focus_terms,
                    maximum_queries=min(
                        maximum_queries,
                        len(focus_terms),
                        int(provider["configuration"]["maximum_queries_per_run"]),
                    ),
                    maximum_results_per_query=min(
                        maximum_results_per_query,
                        int(provider["configuration"]["maximum_results_per_query"]),
                    ),
                    timeout_seconds=timeout_seconds,
                )
            except Exception as caught:
                error = f"{type(caught).__name__}: {caught}"
        registered_source_ids = (
            list(discovery.get("registered_source_ids", [])) if discovery else []
        )
        if not focus_terms:
            status = "COVERAGE_COMPLETE"
        elif registered_source_ids:
            status = "SOURCES_DISCOVERED"
        else:
            status = "EVIDENCE_CEILING"
        capture_requests = (
            []
            if status != "EVIDENCE_CEILING"
            else self._capture_requests(focus_terms, atlas)
        )
        report = {
            "schema_version": 1,
            "receipt_type": "missing_evidence_pursuit",
            "id": pursuit_id,
            "cache_key": cache_key,
            "target_id": target["id"],
            "category": category,
            "provider_id": selected_provider_id,
            "status": status,
            "focus_terms": focus_terms,
            "coverage_before": {
                "directional": directional,
                "surface_atlas": atlas,
                "stable_summary": stable_coverage,
            },
            "discovery_run_id": discovery.get("id") if discovery else None,
            "discovery_receipt_digest": (
                discovery.get("artifact", {}).get("digest") if discovery else None
            ),
            "registered_source_ids": registered_source_ids,
            "capture_requests": capture_requests,
            "error": error,
            "policy": {
                "source_rights_auto_approved": False,
                "source_download_performed": False,
                "capture_required_when_public_search_exhausted": True,
                "acceptance_performed": False,
            },
            "created_at": utc_now(),
        }
        relative = Path("receipts") / f"evidence-pursuit-{pursuit_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.evidence-pursuit+json",
        )
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE evidence_pursuit_runs SET status=?,discovery_run_id=?,"
                "capture_request_ids_json=?,report_digest=?,updated_at=?,"
                "lease_token=NULL,lease_expires_at=NULL "
                "WHERE id=? AND status='RUNNING' AND lease_token=?",
                (
                    status,
                    report["discovery_run_id"],
                    json.dumps([item["id"] for item in capture_requests]),
                    artifact.digest,
                    report["created_at"],
                    pursuit_id,
                    lease_token,
                ),
            )
        if updated.rowcount != 1:
            raise RuntimeError("missing-evidence pursuit finalization raced with another worker")
        return {**self.get(pursuit_id), "report": report, "path": str(relative)}

    def get(self, pursuit_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM evidence_pursuit_runs WHERE id=?", (pursuit_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown evidence pursuit: {pursuit_id}")
        value = dict(row)
        value["focus_terms"] = json.loads(value.pop("focus_terms_json"))
        value["coverage"] = json.loads(value.pop("coverage_json"))
        value["capture_request_ids"] = json.loads(value.pop("capture_request_ids_json"))
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute(
                    "SELECT id FROM evidence_pursuit_runs ORDER BY created_at,id"
                )
            ]
        return [self.get(item) for item in ids]

    @staticmethod
    def _focus_terms(
        directional: dict[str, Any], atlas: dict[str, Any]
    ) -> list[str]:
        return list(
            dict.fromkeys(
                [*directional["missing_directions"], *atlas["unresolved_regions"]]
            )
        )[:32]

    def _capture_requests(
        self, focus_terms: list[str], atlas: dict[str, Any]
    ) -> list[dict[str, Any]]:
        atlas_requests = {
            item["region"]: item["request"] for item in atlas["next_best_evidence"]
        }
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            existing_rows = connection.execute(
                "SELECT request_json FROM capture_requests "
                "WHERE status='requested' ORDER BY created_at,id"
            ).fetchall()
            existing_requests = [json.loads(row["request_json"]) for row in existing_rows]
            records = []
            for term in focus_terms:
                existing = next(
                    (
                        request
                        for request in existing_requests
                        if request.get("region") == term
                        and request.get("requester") == "VisionMCP Capture Planner"
                    ),
                    None,
                )
                if existing:
                    records.append(existing)
                    continue
                request_type, direction, instructions = self._request_spec(
                    term, atlas_requests
                )
                now = utc_now()
                record = {
                    "id": str(uuid.uuid4()),
                    "request_type": request_type,
                    "direction": direction,
                    "region": term,
                    "instructions": instructions,
                    "requester": "VisionMCP Capture Planner",
                    "reason": (
                        "Governed public discovery produced no new source for this "
                        "coverage or authority gap."
                    ),
                    "status": "requested",
                    "created_at": now,
                    "updated_at": now,
                }
                connection.execute(
                    "INSERT INTO capture_requests(id,request_json,status,created_at,updated_at) "
                    "VALUES(?,?,?,?,?)",
                    (record["id"], json.dumps(record), "requested", now, now),
                )
                existing_requests.append(record)
                records.append(record)
        return records

    @staticmethod
    def _request_spec(
        term: str, atlas_requests: dict[str, str]
    ) -> tuple[str, str, str]:
        if term in {
            "technical drawing",
            "official manual",
            "manufacturer specifications",
            "parts diagram",
        }:
            return (
                "document_upload",
                "document",
                f"Provide a legally usable {term} for the exact target variant, including "
                "publisher, revision, market, and page provenance.",
            )
        if term == "dimensions":
            return (
                "physical_measurement",
                "measurement",
                "Provide authoritative overall x/y/z dimensions or measured values with units, "
                "instrument identity, calibration state, and uncertainty.",
            )
        if term in {"calibration board", "reviewed camera landmarks"}:
            return (
                "calibrated_capture",
                "calibration",
                "Provide three or more sharp calibration-board views with known square size, or "
                "at least six reviewed non-coplanar 2D-to-3D landmarks per image.",
            )
        if term == "teardown":
            return (
                "teardown_capture",
                "detail",
                "Provide a rights-cleared teardown view with the target component unobscured, "
                "variant identity visible, and included/excluded component scope declared.",
            )
        direction = (
            term
            if term in {"front", "rear", "left", "right", "top", "bottom"}
            else "bottom"
            if term == "underbody"
            else "detail"
        )
        return (
            "photograph",
            direction,
            atlas_requests.get(
                term,
                f"Capture a sharp {term} view with the complete target region visible, at "
                "least 1600 pixels across, without blur or occlusion.",
            ),
        )
