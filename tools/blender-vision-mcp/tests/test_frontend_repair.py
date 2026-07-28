from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator

from blender_vision.core.util import canonical_json
from blender_vision.perception import (
    AdapterRegistry,
    CaptureBus,
    FrontendComparisonService,
    FrontendRepairService,
)
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.projects.store import ProjectStore


class FrontendCandidateAdapter:
    name = "test.frontend-candidate"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        return {"id": target["id"], "kind": "owned-frontend-fixture"}

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        return {"variant": config["variant"]}

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {"runtime": "fixture", "variant": config["variant"]}

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        del target
        variant = config["variant"]
        hero = {
            "bounds": {"x": 20, "y": 20, "width": 400, "height": 240},
            "styles": {
                "position": "relative",
                "backgroundColor": "rgb(18, 92, 210)",
                "color": "rgb(255, 255, 255)",
            },
        }
        footer = {
            "bounds": {"x": 0, "y": 700, "width": 800, "height": 100},
            "styles": {
                "position": "relative",
                "backgroundColor": "rgb(20, 30, 40)",
                "color": "rgb(255, 255, 255)",
            },
        }
        if variant == "degraded":
            hero["bounds"]["width"] = 300
            hero["styles"]["backgroundColor"] = "rgb(220, 40, 40)"
        elif variant == "local-regression":
            footer["bounds"]["x"] = 40
            footer["styles"]["backgroundColor"] = "rgb(220, 40, 40)"
        elif variant == "worse":
            hero["bounds"]["width"] = 260
            hero["bounds"]["height"] = 180
        graph = {
            "schema": "vision.layout-graph/v1",
            "graph_type": "LayoutGraph",
            "authority": "OBSERVED",
            "coordinate_space": "CSS viewport pixels",
            "capture": {"variant": variant},
            "nodes": [
                {
                    "id": "node:hero",
                    "selector": "#hero",
                    "tag": "section",
                    "role": "region",
                    "text": "Hero",
                    "accessibleName": "Hero",
                    "sourceBinding": {
                        "id": "hero",
                        "file": "workspace/app.css",
                    },
                    "interactive": False,
                    "surface": "DOM",
                    "depth": 2,
                    "assetUrls": [],
                    **hero,
                },
                {
                    "id": "node:footer",
                    "selector": "#footer",
                    "tag": "footer",
                    "role": "contentinfo",
                    "text": "Footer",
                    "accessibleName": "Footer",
                    "sourceBinding": {
                        "id": "footer",
                        "file": "workspace/app.css",
                    },
                    "interactive": False,
                    "surface": "DOM",
                    "depth": 2,
                    "assetUrls": [],
                    **footer,
                },
            ],
            "edges": [],
        }
        sink("layout.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={"variant": variant},
            graphs=[
                {
                    "graph_type": "LayoutGraph",
                    "role": "layout.graph",
                    "node_count": 2,
                    "edge_count": 0,
                }
            ],
        )


def make_candidates(
    tmp_path: Path,
) -> tuple[ProjectStore, CaptureBus, dict[str, dict[str, Any]]]:
    project = ProjectStore.create(tmp_path / "project", "Frontend repair")
    registry = AdapterRegistry()
    adapter = FrontendCandidateAdapter()
    registry.register(adapter)
    bus = CaptureBus(project, registry)
    captures = {
        variant: bus.observe(
            adapter.name,
            {"id": "owned-page"},
            {"variant": variant},
            rights_decision="SYNTHETIC_OWNED",
        )
        for variant in ("target", "degraded", "local-regression", "good", "worse")
    }
    return project, bus, captures


def test_frontend_comparison_reports_component_local_geometry_and_style_residuals(
    tmp_path: Path,
) -> None:
    project, _bus, captures = make_candidates(tmp_path)
    report = FrontendComparisonService(project).compare(
        captures["target"]["capture_id"],
        captures["degraded"]["capture_id"],
        selectors=["#hero"],
    )

    assert report["status"] == "FAIL"
    assert report["scope"]["local"] is True
    residual = report["residuals"][0]
    assert residual["geometry"]["absolute"]["width"] == 100
    assert residual["style"]["mismatches"] == [
        {
            "property": "backgroundColor",
            "target": "rgb(18, 92, 210)",
            "candidate": "rgb(220, 40, 40)",
        }
    ]
    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "frontend-comparison.schema.json").read_text()
    )
    Draft202012Validator(schema).validate(report)


def test_local_candidate_selection_is_atomically_rejected_by_global_regression_gate(
    tmp_path: Path,
) -> None:
    project, _bus, captures = make_candidates(tmp_path)
    service = FrontendRepairService(project)
    portfolio = service.create_portfolio(
        captures["target"]["capture_id"],
        [
            {
                "capture_id": captures["local-regression"]["capture_id"],
                "parameters": {"hero_width": 400, "footer_offset": 40},
            },
            {
                "capture_id": captures["good"]["capture_id"],
                "parameters": {"hero_width": 400, "footer_offset": 0},
            },
            {
                "capture_id": captures["worse"]["capture_id"],
                "parameters": {"hero_width": 260},
            },
        ],
        locality_selectors=["#hero"],
    )

    assert portfolio["candidates"][0]["capture_id"] == captures["local-regression"][
        "capture_id"
    ]
    gate = service.run_global_gate(portfolio["id"])

    assert gate["status"] == "FAIL"
    assert gate["atomic_decision"] == "REJECT_CANDIDATE"
    footer = next(
        residual
        for residual in gate["comparison"]["residuals"]
        if residual["selector"] == "#footer"
    )
    assert footer["geometry"]["absolute"]["x"] == 40
    with project.connection() as connection:
        attempts = connection.execute(
            "SELECT status,COUNT(*) AS count FROM frontend_candidates "
            "GROUP BY status ORDER BY status"
        ).fetchall()
    assert {row["status"]: row["count"] for row in attempts} == {
        "REJECTED_GLOBAL": 1,
        "REJECTED_LOCAL": 2,
    }

    passing = service.create_portfolio(
        captures["target"]["capture_id"],
        [{"capture_id": captures["good"]["capture_id"], "parameters": {"hero_width": 400}}],
        locality_selectors=["#hero"],
    )
    passing_gate = service.run_global_gate(passing["id"])
    assert passing_gate["status"] == "PASS"
    assert passing_gate["atomic_decision"] == "ACCEPT_CANDIDATE"


def test_css_repair_requires_named_review_exact_base_and_preserves_backup(
    tmp_path: Path,
) -> None:
    project, _bus, captures = make_candidates(tmp_path)
    workspace = project.root / "workspace"
    workspace.mkdir()
    css = workspace / "app.css"
    css.write_text(
        "#hero { width: 300px; background-color: rgb(220, 40, 40); }\n",
        encoding="utf-8",
    )
    service = FrontendRepairService(project)
    proposal = service.propose_css_patch(
        captures["target"]["capture_id"],
        captures["degraded"]["capture_id"],
        target_file="workspace/app.css",
        selectors=["#hero"],
    )

    assert proposal["status"] == "PROPOSED"
    with pytest.raises(PermissionError, match="approved named decision"):
        service.apply_patch(proposal["id"])
    decision = service.review_patch(
        proposal["id"],
        accepted=True,
        reviewer="Frontend acceptance policy",
        reason="Bounded hero repair matches the owned target graph",
    )
    applied = service.apply_patch(proposal["id"])

    assert decision["status"] == "APPROVED"
    assert applied["status"] == "APPLIED"
    result = css.read_text(encoding="utf-8")
    assert "width: 400px;" in result
    assert "background-color: rgb(18, 92, 210);" in result
    assert applied["base_digest"] == applied["backup_digest"]
    assert service.artifacts.path_for(applied["receipt_digest"]).is_file()


def test_css_repair_refuses_changed_base_and_path_escape(tmp_path: Path) -> None:
    project, _bus, captures = make_candidates(tmp_path)
    workspace = project.root / "workspace"
    workspace.mkdir()
    css = workspace / "app.css"
    css.write_text("#hero { width: 300px; }\n", encoding="utf-8")
    service = FrontendRepairService(project)
    with pytest.raises(PermissionError, match="escapes"):
        service.propose_css_patch(
            captures["target"]["capture_id"],
            captures["degraded"]["capture_id"],
            target_file="../../outside.css",
            selectors=["#hero"],
        )
    proposal = service.propose_css_patch(
        captures["target"]["capture_id"],
        captures["degraded"]["capture_id"],
        target_file="workspace/app.css",
        selectors=["#hero"],
    )
    service.review_patch(
        proposal["id"],
        accepted=True,
        reviewer="Reviewer",
        reason="Approved before simulated concurrent edit",
    )
    css.write_text("#hero { width: 310px; }\n", encoding="utf-8")

    with pytest.raises(RuntimeError, match="changed after proposal"):
        service.apply_patch(proposal["id"])
