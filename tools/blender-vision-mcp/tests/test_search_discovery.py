from __future__ import annotations

import json
from pathlib import Path
from urllib import parse

import pytest

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.evidence.discovery import SearchProviderStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.scheduling.distributed import operation_requirements
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def _project(tmp_path: Path) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(tmp_path / "project", "Provider discovery")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Acme", "model": "Widget", "model_year": 2026}
    )
    return project, target


def _provider(
    project: ProjectStore,
    *,
    authentication: dict | None = None,
    maximum_response_bytes: int = 8192,
) -> dict:
    return SearchProviderStore(project).register(
        name="Reviewed fixture search",
        configuration={
            "endpoint": "https://search.example.test/api",
            "provider_kind": "json_items",
            "maximum_queries_per_run": 2,
            "maximum_results_per_query": 10,
            "maximum_response_bytes": maximum_response_bytes,
            "allowed_provider_domains": ["search.example.test"],
            "allowed_result_domains": ["acme.example"],
            "official_domains": ["acme.example"],
            "authentication": authentication or {"mode": "none"},
            "access_policy": {
                "authentication_boundary": "none",
                "source_terms_review": "approved",
                "privacy_review": "not_applicable",
                "rate_limit_policy": "two queries per governed run",
            },
        },
        reviewer="Provider governance reviewer",
        reason="Fixture provider terms and privacy policy reviewed",
    )


class _Response:
    def __init__(self, payload: bytes, url: str = "https://search.example.test/api"):
        self.payload = payload
        self.url = url
        self.status = 200

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return None

    def geturl(self) -> str:
        return self.url

    def read(self, maximum: int) -> bytes:
        return self.payload[:maximum]


def _public_dns(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "blender_vision.evidence.acquisition.socket.getaddrinfo",
        lambda *_args, **_kwargs: [(2, 1, 6, "", ("93.184.216.34", 0))],
    )


def test_governed_provider_registers_deduplicated_unreviewed_source_leads(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    provider = _provider(project)
    _public_dns(monkeypatch)
    payload = json.dumps(
        {
            "items": [
                {
                    "url": (
                        "https://media.acme.example/widget-front.png?utm_source=search#hero"
                    ),
                    "title": "Official Widget front",
                    "snippet": "Manufacturer front view",
                    "publisher": "Acme",
                    "score": 0.93,
                },
                {
                    "url": "https://media.acme.example/widget-front.png",
                    "title": "Duplicate",
                },
                {"url": "http://127.0.0.1/private.png", "title": "Private"},
                {"url": "https://unapproved.example/widget.png", "title": "Wrong domain"},
                {"url": "https://media.acme.example:bad/widget.png", "title": "Bad port"},
                {
                    "url": "https://media.acme.example/secret.png?api_key=do-not-store",
                    "title": "Embedded secret",
                },
            ]
        }
    ).encode()
    observed_queries = []

    def urlopen(search_request, *, timeout):
        assert timeout == 5.0
        query = parse.parse_qs(parse.urlsplit(search_request.full_url).query)["q"][0]
        observed_queries.append(query)
        return _Response(payload)

    monkeypatch.setattr("blender_vision.evidence.discovery.request.urlopen", urlopen)
    result = SearchProviderStore(project).discover(
        provider["id"],
        target_id=target["id"],
        category="general_product",
        maximum_queries=1,
        maximum_results_per_query=10,
        timeout_seconds=5,
    )

    assert result["status"] == "COMPLETED"
    assert len(observed_queries) == 1
    assert result["policy"]["source_rights_auto_approved"] is False
    assert result["policy"]["source_download_performed"] is False
    assert len(result["registered_sources"]) == 1
    source = result["registered_sources"][0]
    assert source["source"]["origin"] == "https://media.acme.example/widget-front.png"
    assert source["source"]["authority_class"] == "manufacturer_authoritative"
    assert source["source"]["target_variant"]["model"] == "Widget"
    assert source["source"]["direct_media"] is True
    assert source["rights"] == {
        "status": "UNREVIEWED_DISCOVERY",
        "internal_use": False,
        "redistribution": False,
        "basis": "search result requires independent source review",
    }
    assert len(result["skipped_results"]) == 5
    assert project.status()["counts"]["search_providers"] == 1
    assert project.status()["counts"]["search_discovery_runs"] == 1

    receipt = export_receipt(project)
    assert receipt["acceptance"]["metrics"]["source_discovery"] == {
        "provider_count": 1,
        "invalid_provider_ids": [],
        "run_count": 1,
        "completed_run_count": 1,
        "registered_source_count": 1,
        "invalid_run_ids": [],
    }
    assert "evidence source rights/access governance is incomplete" in " ".join(
        receipt["acceptance"]["blockers"]
    )
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_provider_credentials_are_environment_only_and_fail_closed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    provider = _provider(
        project,
        authentication={
            "mode": "bearer_env",
            "environment_variable": "BVMCP_FIXTURE_SEARCH_TOKEN",
        },
    )
    _public_dns(monkeypatch)
    monkeypatch.delenv("BVMCP_FIXTURE_SEARCH_TOKEN", raising=False)
    result = SearchProviderStore(project).discover(
        provider["id"], target_id=target["id"], maximum_queries=1
    )

    assert result["status"] == "FAILED"
    assert result["registered_source_ids"] == []
    assert "BVMCP_FIXTURE_SEARCH_TOKEN" in result["query_failures"][0]["error"]
    stored = SearchProviderStore(project).get(provider["id"])
    assert "credential" not in json.dumps(stored).lower()
    with pytest.raises(ValueError, match="query limit"):
        SearchProviderStore(project).discover(
            provider["id"], target_id=target["id"], maximum_queries=0
        )


def test_provider_response_limit_and_redirect_domain_fail_closed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    provider = _provider(project, maximum_response_bytes=1024)
    _public_dns(monkeypatch)
    responses = iter(
        [
            _Response(b"x" * 1025),
            _Response(b"{}", url="https://redirected.example/api"),
        ]
    )
    monkeypatch.setattr(
        "blender_vision.evidence.discovery.request.urlopen",
        lambda *_args, **_kwargs: next(responses),
    )
    result = SearchProviderStore(project).discover(
        provider["id"], target_id=target["id"], maximum_queries=2
    )

    assert result["status"] == "FAILED"
    assert result["registered_source_ids"] == []
    assert "size limit" in result["query_failures"][0]["error"]
    assert "unapproved domain" in result["query_failures"][1]["error"]


def test_provider_registration_rejects_unreviewed_or_unsafe_endpoints(tmp_path: Path) -> None:
    project, _target = _project(tmp_path)
    store = SearchProviderStore(project)
    with pytest.raises(ValueError, match="HTTPS"):
        store.register(
            name="Unsafe",
            configuration={
                "endpoint": "http://127.0.0.1/search",
                "access_policy": {
                    "source_terms_review": "approved",
                    "privacy_review": "not_applicable",
                },
            },
            reviewer="Reviewer",
            reason="Must fail",
        )
    with pytest.raises(ValueError, match="source-terms"):
        store.register(
            name="Unreviewed",
            configuration={
                "endpoint": "https://search.example.test/api",
                "access_policy": {
                    "source_terms_review": "pending",
                    "privacy_review": "not_applicable",
                },
            },
            reviewer="Reviewer",
            reason="Must fail",
        )
    with pytest.raises(ValueError, match="environment-only"):
        store.register(
            name="Embedded secret",
            configuration={
                "endpoint": "https://search.example.test/api?api_key=secret",
                "access_policy": {
                    "source_terms_review": "approved",
                    "privacy_review": "not_applicable",
                },
            },
            reviewer="Reviewer",
            reason="Must fail",
        )
    with pytest.raises(ValueError, match="unsupported or secret fields"):
        store.register(
            name="Stored secret",
            configuration={
                "endpoint": "https://search.example.test/api",
                "authentication": {"mode": "none", "token": "secret"},
                "access_policy": {
                    "source_terms_review": "approved",
                    "privacy_review": "not_applicable",
                },
            },
            reviewer="Reviewer",
            reason="Must fail",
        )


def test_autonomous_campaign_discovers_then_pauses_for_source_rights_review(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    _provider(project)

    def query_fixture(_self, _config, query, _count, *, timeout_seconds):
        assert timeout_seconds == 20.0
        return (
            [
                {
                    "url": "https://media.acme.example/widget-front.png",
                    "title": "Widget front",
                    "snippet": query,
                    "publisher": "Acme",
                    "score": 0.9,
                }
            ],
            {
                "query": query,
                "request_url": "https://search.example.test/api?q=fixture",
                "final_url": "https://search.example.test/api",
                "http_status": 200,
                "response_bytes": 128,
                "result_count": 1,
            },
        )

    monkeypatch.setattr(SearchProviderStore, "_query", query_fixture)
    campaign = CampaignStore(project).start(
        "autonomous_public_evidence",
        configuration={"target_id": target["id"], "category": "general_product"},
        resource_profile="compact",
    )
    discovered = AutonomousWorkflowExecutor(project).continue_once(campaign["id"])

    assert discovered["workflow_state"] == "EVIDENCE_SOURCES_DISCOVERED"
    assert discovered["campaign"]["status"] == "RUNNING"
    assert discovered["evidence"]["discovery"]["registered_source_ids"]
    reviewed = AutonomousWorkflowExecutor(project).continue_once(campaign["id"])
    assert reviewed["workflow_state"] == "SOURCE_GOVERNANCE_REVIEW_REQUIRED"
    assert reviewed["campaign"]["status"] == "PAUSED"


def test_discovery_runs_through_coordinator_without_cache_or_secret_persistence(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    provider = _provider(project)

    monkeypatch.setattr(
        SearchProviderStore,
        "_query",
        lambda _self, _config, query, _count, **_kwargs: (
            [],
            {
                "query": query,
                "request_url": "https://search.example.test/api",
                "final_url": "https://search.example.test/api",
                "http_status": 200,
                "response_bytes": 2,
                "result_count": 0,
            },
        ),
    )
    job = Coordinator(project).run(
        "evidence.discover",
        {
            "provider_id": provider["id"],
            "target_id": target["id"],
            "maximum_queries": 1,
        },
    )

    assert job["status"] == "succeeded"
    assert job["cache_key"] is None
    assert job["result"]["status"] == "COMPLETED"
    requirements = operation_requirements("evidence.discover")
    assert requirements["worker_classes"] == ["vision"]
    assert requirements["preferred_hardware"][-1] == "cpu"
