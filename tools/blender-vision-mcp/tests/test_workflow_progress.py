from __future__ import annotations

import hashlib
import json
import uuid
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.core.models import FidelityLevel
from blender_vision.core.util import canonical_json, utc_now
from blender_vision.mcp.server import create_server
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.autonomous import (
    reconstruct_from_public_evidence,
    reconstruct_from_user_capture,
)
from blender_vision.workflows.progress import WorkflowProgressReporter


def _public_project(tmp_path: Path) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(
        tmp_path / "project", "Progress fixture", target_fidelity=FidelityLevel.L3
    )
    launched = reconstruct_from_public_evidence(
        project,
        target="NVIDIA DGX Spark",
        requested_tier="L3",
        configuration="factory standard",
        evidence_policy="public_internal_use",
        resource_profile="compact",
    )
    return project, launched


def test_initial_progress_is_compact_truthful_and_digest_bound(tmp_path: Path) -> None:
    project, launched = _public_project(tmp_path)

    progress = launched["progress"]
    assert progress["report_type"] == "workflow_progress"
    assert progress["target"]["status"] == "RESOLVED"
    assert progress["evidence"]["candidate_source_count"] == 0
    assert progress["portfolio"]["hypothesis_count"] == 8
    assert progress["coverage"]["surface_coverage_fraction"] == 0.0
    assert progress["accepted_tier"] is None
    assert progress["support_envelope"]["accepted_scopes"] == []
    assert progress["next_action"]["action"] == "advance_evidence_acquisition"

    digest = progress.pop("report_sha256")
    assert hashlib.sha256(canonical_json(progress)).hexdigest() == digest


def test_user_capture_progress_counts_governed_source_without_claiming_acceptance(
    tmp_path: Path,
) -> None:
    image = tmp_path / "front.png"
    Image.new("RGB", (96, 64), "gray").save(image)
    project = ProjectStore.create(
        tmp_path / "capture", "Capture fixture", target_fidelity=FidelityLevel.L3
    )

    launched = reconstruct_from_user_capture(
        project,
        target="NVIDIA DGX Spark",
        reference_paths=[image],
        category="computer_hardware",
        resource_profile="compact",
    )

    progress = launched["progress"]
    assert progress["evidence"]["candidate_source_count"] == 1
    assert progress["evidence"]["eligible_acquired_source_count"] == 1
    assert progress["evidence"]["rights_review_complete_count"] == 1
    assert progress["evidence"]["internal_use_permitted_count"] == 1
    assert progress["support_envelope"]["accepted_scopes"] == []
    assert progress["acceptance"]["accepted"] is False


def test_evidence_ceiling_exposes_typed_requests_and_unsupported_scope(tmp_path: Path) -> None:
    project, launched = _public_project(tmp_path)
    target_id = launched["target_resolution"]["id"]
    campaign_id = launched["campaign"]["id"]
    now = utc_now()
    capture_id = str(uuid.uuid4())
    pursuit_id = str(uuid.uuid4())
    request = {
        "id": capture_id,
        "request_type": "directed_still",
        "direction": "front",
        "region": "fan",
        "instructions": "Provide an unobscured front fan image.",
        "requester": "VisionMCP Capture Planner",
        "reason": "No governed public source closed this gap.",
        "status": "requested",
        "created_at": now,
        "updated_at": now,
    }
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO capture_requests(id,request_json,status,created_at,updated_at) "
            "VALUES(?,?,?,?,?)",
            (capture_id, json.dumps(request), "requested", now, now),
        )
        connection.execute(
            "INSERT INTO evidence_pursuit_runs("
            "id,cache_key,target_id,provider_id,status,focus_terms_json,coverage_json,"
            "discovery_run_id,capture_request_ids_json,report_digest,lease_token,"
            "lease_expires_at,attempt_count,created_at,updated_at) "
            "VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                pursuit_id,
                f"progress-{pursuit_id}",
                target_id,
                None,
                "EVIDENCE_CEILING",
                json.dumps(["fan"]),
                json.dumps({}),
                None,
                json.dumps([capture_id]),
                None,
                None,
                None,
                1,
                now,
                now,
            ),
        )

    progress = WorkflowProgressReporter(project).report(campaign_id)

    assert progress["missing_evidence"]["status"] == "EVIDENCE_CEILING"
    assert progress["missing_evidence"]["capture_requests"][0]["id"] == capture_id
    assert progress["next_action"]["action"] == "provide_external_evidence"
    fan = next(
        region for region in progress["support_envelope"]["regions"] if region["region"] == "fan"
    )
    assert fan["support_status"] == "NOT_SUPPORTED"
    assert fan["capture_request_id"] == capture_id


def test_progress_ignores_campaign_authored_acceptance_claim_and_is_read_only(
    tmp_path: Path,
) -> None:
    project, launched = _public_project(tmp_path)
    campaign_id = launched["campaign"]["id"]
    CampaignStore(project).progress(
        campaign_id,
        message="untrusted optimistic text",
        details={"accepted": True, "accepted_tier": "L5", "delivery": "COMPLETE"},
    )
    with project.connection() as connection:
        table_names = [
            row[0]
            for row in connection.execute(
                "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
            )
        ]
        counts_before = {
            table: connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
            for table in table_names
        }
    files_before = sorted(
        str(path.relative_to(project.root)) for path in project.root.rglob("*") if path.is_file()
    )

    progress = WorkflowProgressReporter(project).report(campaign_id)

    with project.connection() as connection:
        counts_after = {
            table: connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
            for table in table_names
        }
    files_after = sorted(
        str(path.relative_to(project.root)) for path in project.root.rglob("*") if path.is_file()
    )
    assert counts_after == counts_before
    assert files_after == files_before
    assert progress["campaign"]["latest_progress"]["details"]["accepted"] is True
    assert progress["acceptance"]["accepted"] is False
    assert progress["accepted_tier"] is None
    delivery = next(stage for stage in progress["stages"] if stage["stage"] == "delivery")
    assert delivery["status"] == "PENDING"


@pytest.mark.asyncio
async def test_mcp_progress_and_continuation_return_authoritative_envelope(tmp_path: Path) -> None:
    project, launched = _public_project(tmp_path)
    campaign_id = launched["campaign"]["id"]
    server = create_server(tmp_path / "projects")

    _content, progress = await server.call_tool(
        "workflow.progress",
        {"project_path": str(project.root), "campaign_id": campaign_id},
    )
    assert progress["report_type"] == "workflow_progress"
    assert progress["acceptance"]["accepted"] is False

    _content, continued = await server.call_tool(
        "workflow.continue_autonomous",
        {"project_path": str(project.root), "campaign_id": campaign_id},
    )
    assert continued["workflow_state"] == "EVIDENCE_ACQUISITION_REQUIRED"
    assert continued["progress"]["campaign"]["status"] == "PAUSED"
    assert continued["progress"]["acceptance"]["accepted"] is False
