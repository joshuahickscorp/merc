from __future__ import annotations

import ipaddress
import json
import math
import os
import re
import uuid
from pathlib import Path
from typing import Any
from urllib import parse, request

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

PROVIDER_KINDS = {"json_items", "opensearch_suggestions"}
AUTHENTICATION_MODES = {"none", "bearer_env", "header_env"}
TECHNICAL_TERMS = {"dimensions", "manual", "parts", "technical drawing"}
DIRECT_MEDIA_SUFFIXES = {
    ".avif",
    ".gif",
    ".jpeg",
    ".jpg",
    ".m4v",
    ".mov",
    ".mp4",
    ".pdf",
    ".png",
    ".tif",
    ".tiff",
    ".webm",
    ".webp",
}


def _text(value: Any, label: str, *, maximum: int = 2000) -> str:
    normalized = str(value or "").strip()
    if (
        not normalized
        or len(normalized) > maximum
        or any(ord(character) < 32 for character in normalized)
    ):
        raise ValueError(f"{label} must be non-empty printable text")
    return normalized


class SearchProviderStore:
    """Execute explicitly reviewed search APIs without granting discovered source rights."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.acquisition = EvidenceAcquisitionStore(project)

    def register(
        self,
        *,
        name: str,
        configuration: dict[str, Any],
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        name = _text(name, "search provider name", maximum=200)
        reviewer = _text(reviewer, "search provider reviewer", maximum=200)
        reason = _text(reason, "search provider review reason")
        normalized = self._validated_configuration(configuration)
        provider_id = str(uuid.uuid4())
        review_id = str(uuid.uuid4())
        now = utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "search_provider_review",
            "id": review_id,
            "provider_id": provider_id,
            "name": name,
            "configuration": normalized,
            "reviewer": reviewer,
            "reason": reason,
            "created_at": now,
            "authority": "NAMED_SEARCH_PROVIDER_POLICY_REVIEW",
        }
        relative = Path("receipts") / f"search-provider-review-{review_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.search-provider-review+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute(
                "INSERT INTO search_providers(id,name,config_json,reviewer,reason,created_at,"
                "updated_at) VALUES(?,?,?,?,?,?,?)",
                (
                    provider_id,
                    name,
                    json.dumps(normalized),
                    reviewer,
                    reason,
                    now,
                    now,
                ),
            )
            connection.execute(
                "INSERT INTO search_provider_reviews(id,provider_id,receipt_digest,created_at) "
                "VALUES(?,?,?,?)",
                (review_id, provider_id, artifact.digest, now),
            )
        return self.get(provider_id)

    def _record(self, provider_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM search_providers WHERE id=?", (provider_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown search provider: {provider_id}")
        value = dict(row)
        value["configuration"] = json.loads(value.pop("config_json"))
        return value

    def get(self, provider_id: str, *, verify: bool = True) -> dict[str, Any]:
        value = self._record(provider_id)
        if verify:
            authority = self.authority_status(provider_id)
            if not authority["valid"]:
                raise ValueError(authority["error"] or "search provider authority is invalid")
            value["receipt_digest"] = authority["receipt_digest"]
            value["authority"] = authority["authority"]
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute(
                    "SELECT id FROM search_providers ORDER BY created_at,id"
                )
            ]
        return [self.get(provider_id) for provider_id in ids]

    def authority_status(self, provider_id: str) -> dict[str, Any]:
        try:
            record = self._record(provider_id)
        except (KeyError, TypeError, json.JSONDecodeError) as error:
            return {
                "provider_id": provider_id,
                "valid": False,
                "receipt_digest": None,
                "authority": None,
                "error": str(error),
            }
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM search_provider_reviews WHERE provider_id=?",
                (provider_id,),
            ).fetchone()
        result = {
            "provider_id": provider_id,
            "valid": False,
            "receipt_digest": row["receipt_digest"] if row else None,
            "authority": None,
            "error": None,
        }
        if row is None:
            result["error"] = "search provider has no authoritative named-review receipt"
            return result
        try:
            path = self.artifacts.path_for(row["receipt_digest"])
            if not path.is_file() or sha256_file(path)[0] != row["receipt_digest"]:
                raise ValueError("search provider review receipt is missing or corrupt")
            receipt = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(receipt, dict):
                raise ValueError("search provider review receipt must be an object")
            expected_fields = {
                "schema_version",
                "receipt_type",
                "id",
                "provider_id",
                "name",
                "configuration",
                "reviewer",
                "reason",
                "created_at",
                "authority",
            }
            schema_version = receipt.get("schema_version")
            expected_authority = "NAMED_SEARCH_PROVIDER_POLICY_REVIEW"
            migration_valid = schema_version == 1
            if schema_version == 2:
                expected_fields.add("migration")
                expected_authority = "MIGRATED_NAMED_SEARCH_PROVIDER_POLICY_REVIEW"
                migration = receipt.get("migration")
                migration_valid = bool(
                    isinstance(migration, dict)
                    and set(migration)
                    == {"kind", "legacy_provider_record", "new_review_performed"}
                    and migration.get("kind")
                    == "legacy_search_provider_review_receipt_migration"
                    and migration.get("new_review_performed") is False
                    and canonical_json(migration.get("legacy_provider_record"))
                    == canonical_json(record)
                )
            normalized = self._validated_configuration(record["configuration"])
            valid = bool(
                set(receipt) == expected_fields
                and schema_version in {1, 2}
                and receipt.get("receipt_type") == "search_provider_review"
                and receipt.get("id") == row["id"]
                and receipt.get("provider_id") == provider_id
                and receipt.get("name") == record["name"]
                and canonical_json(receipt.get("configuration"))
                == canonical_json(record["configuration"])
                and canonical_json(normalized) == canonical_json(record["configuration"])
                and receipt.get("reviewer") == record["reviewer"]
                and receipt.get("reason") == record["reason"]
                and receipt.get("created_at") == record["created_at"]
                and row["created_at"] == record["created_at"]
                and record["updated_at"] == record["created_at"]
                and receipt.get("authority") == expected_authority
                and migration_valid
                and _text(record["name"], "search provider name", maximum=200)
                == record["name"]
                and _text(record["reviewer"], "search provider reviewer", maximum=200)
                == record["reviewer"]
                and _text(record["reason"], "search provider review reason")
                == record["reason"]
            )
            result["valid"] = valid
            result["authority"] = receipt.get("authority")
            if not valid:
                result["error"] = "search provider review receipt semantics are inconsistent"
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as error:
            result["error"] = str(error)
        return result

    def migrate_legacy_authority(self, provider_id: str) -> dict[str, Any]:
        record = self._record(provider_id)
        if self.authority_status(provider_id)["receipt_digest"] is not None:
            raise ValueError("search provider already has an authority receipt")
        self._validated_configuration(record["configuration"])
        review_id = str(uuid.uuid4())
        migration = {
            "kind": "legacy_search_provider_review_receipt_migration",
            "legacy_provider_record": record,
            "new_review_performed": False,
        }
        receipt = {
            "schema_version": 2,
            "receipt_type": "search_provider_review",
            "id": review_id,
            "provider_id": provider_id,
            "name": record["name"],
            "configuration": record["configuration"],
            "reviewer": record["reviewer"],
            "reason": record["reason"],
            "created_at": record["created_at"],
            "authority": "MIGRATED_NAMED_SEARCH_PROVIDER_POLICY_REVIEW",
            "migration": migration,
        }
        relative = Path("receipts") / f"search-provider-review-migration-{review_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.search-provider-review+json",
        )
        expected = canonical_json(record)
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT * FROM search_providers WHERE id=?", (provider_id,)
            ).fetchone()
            current_record = dict(current) if current else None
            if current_record:
                current_record["configuration"] = json.loads(
                    current_record.pop("config_json")
                )
            if (
                current_record is None
                or canonical_json(current_record) != expected
                or connection.execute(
                    "SELECT 1 FROM search_provider_reviews WHERE provider_id=?",
                    (provider_id,),
                ).fetchone()
            ):
                raise RuntimeError("legacy search provider changed during authority migration")
            connection.execute(
                "INSERT INTO search_provider_reviews(id,provider_id,receipt_digest,created_at) "
                "VALUES(?,?,?,?)",
                (review_id, provider_id, artifact.digest, record["created_at"]),
            )
        return {
            "provider_id": provider_id,
            "receipt_digest": artifact.digest,
            "migration": migration,
            "authority": self.authority_status(provider_id),
        }

    def discovery_status(self, run_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM search_discovery_runs WHERE id=?", (run_id,)
            ).fetchone()
        result = {
            "run_id": run_id,
            "valid": False,
            "receipt_digest": row["artifact_digest"] if row else None,
            "error": None,
        }
        if row is None:
            result["error"] = "unknown search discovery run"
            return result
        try:
            provider = self.get(row["provider_id"])
            target = TargetResolver(self.project).get(row["target_id"])
            plan = json.loads(row["plan_json"])
            stored = json.loads(row["results_json"])
            path = self.artifacts.path_for(row["artifact_digest"])
            if not path.is_file() or sha256_file(path)[0] != row["artifact_digest"]:
                raise ValueError("search discovery receipt is missing or corrupt")
            report = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(plan, dict) or not isinstance(stored, dict):
                raise ValueError("search discovery database payload is malformed")
            if canonical_json(report) != canonical_json(stored):
                raise ValueError("search discovery receipt differs from its database payload")
            fields = {
                "schema_version",
                "receipt_type",
                "id",
                "provider",
                "target_id",
                "canonical_identity",
                "category",
                "status",
                "query_count",
                "successful_query_count",
                "registered_source_ids",
                "query_responses",
                "query_failures",
                "skipped_results",
                "policy",
                "created_at",
            }
            responses = report.get("query_responses")
            failures = report.get("query_failures")
            skipped = report.get("skipped_results")
            source_ids = report.get("registered_source_ids")
            collections = (responses, failures, skipped, source_ids)
            if not all(isinstance(value, list) for value in collections):
                raise ValueError("search discovery result collections are malformed")
            if len(source_ids) != len(set(source_ids)):
                raise ValueError("search discovery repeats a registered source identifier")
            response_valid = all(
                isinstance(item, dict)
                and set(item)
                == {
                    "query",
                    "request_url",
                    "final_url",
                    "http_status",
                    "response_bytes",
                    "result_count",
                }
                and item.get("query") in plan.get("queries", [])
                and isinstance(item.get("http_status"), int)
                and 100 <= item["http_status"] <= 599
                and isinstance(item.get("response_bytes"), int)
                and 0 < item["response_bytes"]
                <= provider["configuration"]["maximum_response_bytes"]
                and isinstance(item.get("result_count"), int)
                and 0 <= item["result_count"]
                <= provider["configuration"]["maximum_results_per_query"]
                and self._provider_url_allowed(
                    item.get("request_url"), provider["configuration"]
                )
                and self._provider_url_allowed(
                    item.get("final_url"), provider["configuration"]
                )
                for item in responses
            )
            with self.project.connection() as connection:
                source_rows = (
                    connection.execute(
                        "SELECT s.id,s.target_id,s.source_json "
                        "FROM evidence_sources s "
                        f"WHERE s.id IN ({','.join('?' for _ in source_ids)})",
                        source_ids,
                    ).fetchall()
                    if source_ids
                    else []
                )
            sources = {item["id"]: item for item in source_rows}
            sources_valid = len(sources) == len(source_ids)
            for source_id in source_ids:
                if not sources_valid:
                    break
                source_row = sources[source_id]
                source = json.loads(source_row["source_json"])
                provenance = source.get("search_provenance", {})
                sources_valid = bool(
                    source_row["target_id"] == row["target_id"]
                    and provenance.get("provider_id") == row["provider_id"]
                    and provenance.get("provider_name") == provider["name"]
                    and provenance.get("query") in plan.get("queries", [])
                    and isinstance(provenance.get("rank"), int)
                    and provenance["rank"] >= 0
                    and self._normalized_result_url(
                        source.get("origin"), provider["configuration"]
                    )
                    == source.get("origin")
                )
            expected_status = "COMPLETED" if responses else "FAILED"
            expected_provider = {
                "id": provider["id"],
                "name": provider["name"],
                "reviewer": provider["reviewer"],
                "reason": provider["reason"],
            }
            valid = bool(
                set(report) == fields
                and report.get("schema_version") == 1
                and report.get("receipt_type") == "governed_search_discovery"
                and report.get("id") == row["id"]
                and report.get("provider") == expected_provider
                and report.get("target_id") == target["id"] == row["target_id"]
                and report.get("canonical_identity") == plan.get("canonical_identity")
                and report.get("category") == plan.get("category")
                and report.get("status") == row["status"] == expected_status
                and report.get("query_count")
                == len(plan.get("ranked_tasks", [])[: report.get("query_count", 0)])
                and isinstance(report.get("query_count"), int)
                and 0 <= report["query_count"]
                <= provider["configuration"]["maximum_queries_per_run"]
                and report.get("successful_query_count") == len(responses)
                and len(responses) + len(failures) == report.get("query_count")
                and report.get("created_at") == row["created_at"]
                and report.get("policy")
                == {
                    "source_rights_auto_approved": False,
                    "source_download_performed": False,
                    "target_binding_required": True,
                    "duplicate_origins_registered": False,
                }
                and response_valid
                and sources_valid
            )
            result["valid"] = valid
            if not valid:
                result["error"] = "search discovery receipt semantics are inconsistent"
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError) as error:
            result["error"] = str(error)
        return result

    @classmethod
    def _provider_url_allowed(cls, value: Any, configuration: dict[str, Any]) -> bool:
        try:
            parsed = parse.urlsplit(str(value))
            return bool(
                parsed.scheme in {"http", "https"}
                and parsed.hostname
                and not parsed.username
                and not parsed.password
                and cls._domain_allowed(
                    parsed.hostname, configuration["allowed_provider_domains"]
                )
            )
        except (TypeError, ValueError):
            return False

    def discover(
        self,
        provider_id: str,
        *,
        target_id: str | None = None,
        category: str = "general_product",
        focus_terms: list[str] | None = None,
        maximum_queries: int | None = None,
        maximum_results_per_query: int | None = None,
        timeout_seconds: float = 20.0,
    ) -> dict[str, Any]:
        provider = self.get(provider_id)
        config = provider["configuration"]
        query_limit = int(
            config["maximum_queries_per_run"]
            if maximum_queries is None
            else maximum_queries
        )
        result_limit = int(
            config["maximum_results_per_query"]
            if maximum_results_per_query is None
            else maximum_results_per_query
        )
        if not 1 <= query_limit <= config["maximum_queries_per_run"]:
            raise ValueError("discovery query limit exceeds the reviewed provider policy")
        if not 1 <= result_limit <= config["maximum_results_per_query"]:
            raise ValueError("discovery result limit exceeds the reviewed provider policy")
        if (
            not isinstance(timeout_seconds, (int, float))
            or timeout_seconds <= 0
            or timeout_seconds > 120
        ):
            raise ValueError("search timeout must be between zero and 120 seconds")

        resolution = TargetResolver(self.project).get(target_id)
        plan = self.acquisition.plan_search(
            resolution["id"], category=category, focus_terms=focus_terms
        )
        tasks = plan["ranked_tasks"][:query_limit]
        existing_origins = {
            self._normalized_result_url(item["source"].get("origin"), config)
            for item in self.acquisition.list(resolution["id"])
            if item["source"].get("origin")
        }
        existing_origins.discard(None)
        seen = set(existing_origins)
        registered: list[dict[str, Any]] = []
        skipped: list[dict[str, Any]] = []
        query_failures: list[dict[str, str]] = []
        query_records: list[dict[str, Any]] = []
        for task in tasks:
            try:
                raw_results, response_record = self._query(
                    config,
                    task["query"],
                    result_limit,
                    timeout_seconds=float(timeout_seconds),
                )
            except Exception as error:
                query_failures.append(
                    {
                        "query": task["query"],
                        "error": f"{type(error).__name__}: {error}",
                    }
                )
                continue
            query_records.append(response_record)
            for rank, result in enumerate(raw_results[:result_limit]):
                normalized = self._normalized_result_url(result.get("url"), config)
                if normalized is None:
                    skipped.append(
                        {
                            "query": task["query"],
                            "rank": rank,
                            "reason": "unsafe_or_disallowed_result_url",
                        }
                    )
                    continue
                if normalized in seen:
                    skipped.append(
                        {
                            "query": task["query"],
                            "rank": rank,
                            "url": normalized,
                            "reason": "duplicate_origin",
                        }
                    )
                    continue
                seen.add(normalized)
                hostname = parse.urlsplit(normalized).hostname or "unknown"
                authority_class = self._authority_class(hostname, task["term"], config)
                provider_score = result.get("score")
                if isinstance(provider_score, (int, float)) and math.isfinite(
                    float(provider_score)
                ):
                    quality_score = max(0.0, min(1.0, float(provider_score)))
                else:
                    quality_score = max(0.1, 1.0 - rank * 0.08)
                source = self.acquisition.register_source(
                    resolution["id"],
                    {
                        "origin": normalized,
                        "publisher": str(result.get("publisher") or hostname).strip(),
                        "page_title": str(result.get("title") or normalized).strip(),
                        "authority_class": authority_class,
                        "target_variant": resolution["target"],
                        "viewpoint": task["term"],
                        "quality_score": round(quality_score, 6),
                        "direct_media": Path(parse.urlsplit(normalized).path).suffix.lower()
                        in DIRECT_MEDIA_SUFFIXES,
                        "included_evidence": [task["term"]],
                        "search_provenance": {
                            "provider_id": provider_id,
                            "provider_name": provider["name"],
                            "query": task["query"],
                            "rank": rank,
                            "snippet": str(result.get("snippet") or "")[:2000],
                        },
                        "access_policy": {
                            "robots_respected": True,
                            "robots_verification": "required_again_at_acquisition",
                            "authentication_boundary": "none",
                            "source_terms_review": "pending",
                            "privacy_review": "not_applicable",
                            "rate_limit_policy": "publisher_limits",
                        },
                    },
                    rights={
                        "status": "UNREVIEWED_DISCOVERY",
                        "internal_use": False,
                        "redistribution": False,
                        "basis": "search result requires independent source review",
                    },
                )
                registered.append(source)

        run_id = str(uuid.uuid4())
        created_at = utc_now()
        status = "COMPLETED" if query_records else "FAILED"
        report = {
            "schema_version": 1,
            "receipt_type": "governed_search_discovery",
            "id": run_id,
            "provider": {
                "id": provider_id,
                "name": provider["name"],
                "reviewer": provider["reviewer"],
                "reason": provider["reason"],
            },
            "target_id": resolution["id"],
            "canonical_identity": plan["canonical_identity"],
            "category": category,
            "status": status,
            "query_count": len(tasks),
            "successful_query_count": len(query_records),
            "registered_source_ids": [item["id"] for item in registered],
            "query_responses": query_records,
            "query_failures": query_failures,
            "skipped_results": skipped,
            "policy": {
                "source_rights_auto_approved": False,
                "source_download_performed": False,
                "target_binding_required": True,
                "duplicate_origins_registered": False,
            },
            "created_at": created_at,
        }
        relative = Path("receipts") / f"search-discovery-{run_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.search-discovery+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO search_discovery_runs(id,provider_id,target_id,status,plan_json,"
                "results_json,artifact_digest,created_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    run_id,
                    provider_id,
                    resolution["id"],
                    status,
                    json.dumps(plan),
                    json.dumps(report),
                    artifact.digest,
                    created_at,
                ),
            )
        authority = self.discovery_status(run_id)
        if not authority["valid"]:
            raise RuntimeError(
                authority["error"] or "search discovery receipt failed verification"
            )
        return {
            **report,
            "registered_sources": registered,
            "artifact": artifact.to_dict(),
            "path": str(relative),
            "authority": authority,
            "conflicts": self.acquisition.resolve_conflicts(resolution["id"]),
            "deduplication": self.acquisition.deduplicate(),
        }

    @staticmethod
    def _validated_configuration(configuration: dict[str, Any]) -> dict[str, Any]:
        if not isinstance(configuration, dict):
            raise ValueError("search provider configuration must be an object")
        endpoint = str(configuration.get("endpoint", "")).strip()
        parsed = parse.urlsplit(endpoint)
        access = dict(configuration.get("access_policy") or {})
        authentication = dict(configuration.get("authentication") or {"mode": "none"})
        kind = str(configuration.get("provider_kind", "json_items"))
        if kind not in PROVIDER_KINDS:
            raise ValueError("unsupported search provider kind")
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            raise ValueError("search provider endpoint must be absolute HTTP(S)")
        if parsed.username or parsed.password:
            raise ValueError("search provider credentials cannot be embedded in the endpoint")
        if any(
            re.search(r"key|token|secret|auth", key, re.I) and value
            for key, value in parse.parse_qsl(parsed.query, keep_blank_values=True)
        ):
            raise ValueError(
                "search provider endpoint secrets must use environment-only authentication"
            )
        private_authorized = bool(access.get("private_network_authorized")) and access.get(
            "authentication_boundary"
        ) == "user_authorized"
        if parsed.scheme != "https" and not private_authorized:
            raise ValueError("search provider endpoint requires HTTPS")
        if access.get("source_terms_review") not in {
            "approved",
            "not_applicable",
            "user_owned",
        }:
            raise ValueError("search provider requires approved source-terms review")
        if access.get("privacy_review") not in {"approved", "not_applicable", "user_owned"}:
            raise ValueError("search provider requires approved privacy review")
        mode = str(authentication.get("mode", "none"))
        if mode not in AUTHENTICATION_MODES:
            raise ValueError("unsupported search provider authentication mode")
        allowed_authentication_fields = {
            "none": {"mode"},
            "bearer_env": {"mode", "environment_variable"},
            "header_env": {"mode", "environment_variable", "header_name"},
        }[mode]
        if set(authentication) - allowed_authentication_fields:
            raise ValueError(
                "search provider authentication contains unsupported or secret fields"
            )
        if mode != "none":
            environment_variable = str(authentication.get("environment_variable", ""))
            if not re.fullmatch(r"[A-Z][A-Z0-9_]{1,63}", environment_variable):
                raise ValueError("search provider secret must use a named environment variable")
        if mode == "header_env" and not re.fullmatch(
            r"[A-Za-z][A-Za-z0-9-]{0,63}", str(authentication.get("header_name", ""))
        ):
            raise ValueError("search provider authentication header name is invalid")
        maximum_queries = int(configuration.get("maximum_queries_per_run", 5))
        maximum_results = int(configuration.get("maximum_results_per_query", 10))
        maximum_response = int(configuration.get("maximum_response_bytes", 8 * 1024 * 1024))
        if not 1 <= maximum_queries <= 100 or not 1 <= maximum_results <= 100:
            raise ValueError("search provider query and result limits must be between 1 and 100")
        if not 1024 <= maximum_response <= 16 * 1024 * 1024:
            raise ValueError("search provider response limit must be 1 KiB through 16 MiB")
        fields = {
            "items": "items",
            "url": "url",
            "title": "title",
            "snippet": "snippet",
            "publisher": "publisher",
            "score": "score",
            **dict(configuration.get("result_fields") or {}),
        }
        if any(not isinstance(value, str) or not value for value in fields.values()):
            raise ValueError("search provider result field mappings must be non-empty strings")
        return {
            "endpoint": endpoint,
            "provider_kind": kind,
            "query_parameter": str(configuration.get("query_parameter", "q")),
            "count_parameter": str(configuration.get("count_parameter", "count")),
            "result_fields": fields,
            "maximum_queries_per_run": maximum_queries,
            "maximum_results_per_query": maximum_results,
            "maximum_response_bytes": maximum_response,
            "allowed_provider_domains": SearchProviderStore._domains(
                configuration.get("allowed_provider_domains") or [parsed.hostname]
            ),
            "allowed_result_domains": SearchProviderStore._domains(
                configuration.get("allowed_result_domains") or []
            ),
            "official_domains": SearchProviderStore._domains(
                configuration.get("official_domains") or []
            ),
            "authentication": authentication,
            "access_policy": {
                "authentication_boundary": "none",
                "private_network_authorized": False,
                **access,
            },
        }

    def _query(
        self,
        config: dict[str, Any],
        query: str,
        count: int,
        *,
        timeout_seconds: float,
    ) -> tuple[list[dict[str, Any]], dict[str, Any]]:
        parsed = parse.urlsplit(config["endpoint"])
        allow_private = bool(config["access_policy"].get("private_network_authorized")) and (
            config["access_policy"].get("authentication_boundary") == "user_authorized"
        )
        self.acquisition._validate_remote_host(parsed.hostname or "", allow_private=allow_private)
        parameters = parse.parse_qsl(parsed.query, keep_blank_values=True)
        parameters.extend(
            [
                (config["query_parameter"], query),
                (config["count_parameter"], str(count)),
            ]
        )
        url = parse.urlunsplit(
            (parsed.scheme, parsed.netloc, parsed.path, parse.urlencode(parameters), "")
        )
        headers = {
            "Accept": "application/json",
            "User-Agent": "VisionMCP/1.0 (governed source discovery)",
        }
        authentication = config["authentication"]
        if authentication.get("mode") != "none":
            variable = authentication["environment_variable"]
            secret = os.environ.get(variable)
            if not secret:
                raise PermissionError(
                    f"search provider credential environment variable is unavailable: {variable}"
                )
            if authentication["mode"] == "bearer_env":
                headers["Authorization"] = f"Bearer {secret}"
            else:
                headers[authentication["header_name"]] = secret
        search_request = request.Request(url, headers=headers)
        with request.urlopen(search_request, timeout=timeout_seconds) as response:
            final_url = response.geturl()
            final_host = parse.urlsplit(final_url).hostname
            if not final_host or not self._domain_allowed(
                final_host, config["allowed_provider_domains"]
            ):
                raise PermissionError("search provider redirected to an unapproved domain")
            self.acquisition._validate_remote_host(final_host, allow_private=allow_private)
            body = response.read(config["maximum_response_bytes"] + 1)
            if len(body) > config["maximum_response_bytes"]:
                raise ValueError("search provider response exceeds its reviewed size limit")
            if not body:
                raise ValueError("search provider returned an empty response")
            status = int(getattr(response, "status", 200))
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise ValueError("search provider response is not valid UTF-8 JSON") from error
        results = self._parse_results(payload, config)
        return results, {
            "query": query,
            "request_url": self._redacted_url(url),
            "final_url": self._redacted_url(final_url),
            "http_status": status,
            "response_bytes": len(body),
            "result_count": len(results),
        }

    @staticmethod
    def _parse_results(payload: Any, config: dict[str, Any]) -> list[dict[str, Any]]:
        if config["provider_kind"] == "opensearch_suggestions":
            if not isinstance(payload, list) or len(payload) < 4:
                raise ValueError("OpenSearch suggestion response is malformed")
            titles, snippets, urls = payload[1], payload[2], payload[3]
            if not all(isinstance(value, list) for value in (titles, snippets, urls)):
                raise ValueError("OpenSearch suggestion response lists are malformed")
            return [
                {
                    "url": url,
                    "title": titles[index] if index < len(titles) else url,
                    "snippet": snippets[index] if index < len(snippets) else "",
                }
                for index, url in enumerate(urls)
            ]
        fields = config["result_fields"]
        items = payload.get(fields["items"]) if isinstance(payload, dict) else None
        if not isinstance(items, list):
            raise ValueError("search provider JSON items response is malformed")
        results = []
        for item in items:
            if not isinstance(item, dict):
                continue
            results.append(
                {
                    "url": item.get(fields["url"]),
                    "title": item.get(fields["title"]),
                    "snippet": item.get(fields["snippet"]),
                    "publisher": item.get(fields["publisher"]),
                    "score": item.get(fields["score"]),
                }
            )
        return results

    @staticmethod
    def _normalized_result_url(value: Any, config: dict[str, Any]) -> str | None:
        if not isinstance(value, str):
            return None
        try:
            parsed = parse.urlsplit(value.strip())
            port = parsed.port
        except ValueError:
            return None
        query_items = [
            (key, item)
            for key, item in parse.parse_qsl(parsed.query, keep_blank_values=True)
            if key.lower() not in {"fbclid", "gclid"}
            and not key.lower().startswith("utm_")
        ]
        if (
            parsed.scheme not in {"http", "https"}
            or not parsed.hostname
            or parsed.username
            or parsed.password
            or any(re.search(r"key|token|secret|auth", key, re.I) for key, _item in query_items)
        ):
            return None
        try:
            address = ipaddress.ip_address(parsed.hostname)
        except ValueError:
            address = None
        if address and (
            address.is_private
            or address.is_loopback
            or address.is_link_local
            or address.is_multicast
            or address.is_reserved
            or address.is_unspecified
        ):
            return None
        if config["allowed_result_domains"] and not SearchProviderStore._domain_allowed(
            parsed.hostname, config["allowed_result_domains"]
        ):
            return None
        netloc = parsed.hostname.lower()
        if port and not (
            (parsed.scheme == "https" and port == 443)
            or (parsed.scheme == "http" and port == 80)
        ):
            netloc = f"{netloc}:{port}"
        return parse.urlunsplit(
            (
                parsed.scheme.lower(),
                netloc,
                parsed.path or "/",
                parse.urlencode(sorted(query_items)),
                "",
            )
        )

    @staticmethod
    def _authority_class(hostname: str, term: str, config: dict[str, Any]) -> str:
        if SearchProviderStore._domain_allowed(hostname, config["official_domains"]):
            return "manufacturer_authoritative"
        if term in TECHNICAL_TERMS:
            return "public_factual_technical"
        return "diagnostic_third_party"

    @staticmethod
    def _domains(values: Any) -> list[str]:
        if not isinstance(values, list):
            raise ValueError("search provider domain allowlists must be lists")
        domains = []
        for value in values:
            domain = str(value).strip().lower().rstrip(".")
            if not re.fullmatch(r"[a-z0-9.-]+", domain) or ".." in domain:
                raise ValueError(f"invalid search provider domain: {value}")
            domains.append(domain)
        return sorted(set(domains))

    @staticmethod
    def _domain_allowed(hostname: str, domains: list[str]) -> bool:
        host = hostname.lower().rstrip(".")
        return any(host == domain or host.endswith(f".{domain}") for domain in domains)

    @staticmethod
    def _redacted_url(value: str) -> str:
        parsed = parse.urlsplit(value)
        parameters = [
            (key, "<redacted>" if re.search(r"key|token|secret|auth", key, re.I) else item)
            for key, item in parse.parse_qsl(parsed.query, keep_blank_values=True)
        ]
        return parse.urlunsplit(
            (parsed.scheme, parsed.netloc, parsed.path, parse.urlencode(parameters), "")
        )
