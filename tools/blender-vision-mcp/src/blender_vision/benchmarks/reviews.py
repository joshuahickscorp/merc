from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore


def _text(value: Any, label: str, *, maximum: int) -> str:
    normalized = str(value or "").strip()
    if (
        not normalized
        or len(normalized) > maximum
        or any(ord(character) < 32 for character in normalized)
    ):
        raise ValueError(f"{label} must be non-empty printable text")
    return normalized


class BenchmarkReviewStore:
    """Persist named benchmark policy decisions as content-addressed review receipts."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def approve_dgx_foam_lod(
        self,
        *,
        strategy: dict[str, Any],
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        metadata = self.project.project().get("metadata", {})
        if metadata.get("benchmark") != "dgx_spark":
            raise ValueError("foam LOD approval is only valid for a DGX Spark benchmark")
        reviewer_name = _text(reviewer, "foam LOD reviewer", maximum=200)
        review_reason = _text(reason, "foam LOD review reason", maximum=2000)
        normalized = self._validate_foam_strategy(strategy)
        required_views = self._required_validation_views()
        missing_views = sorted(required_views - set(normalized["validation_views"]))
        if missing_views:
            raise ValueError(
                "foam LOD validation views omit acceptance references: "
                + ", ".join(missing_views)
            )
        with self.project.connection() as connection:
            previous_row = connection.execute(
                "SELECT receipt_digest FROM benchmark_policy_reviews "
                "WHERE benchmark='dgx_spark' AND review_kind='dgx_foam_lod_strategy' "
                "ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
        previous_digest = previous_row["receipt_digest"] if previous_row else None
        review_id = str(uuid.uuid4())
        now = utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "benchmark_named_review",
            "id": review_id,
            "benchmark": "dgx_spark",
            "review_kind": "dgx_foam_lod_strategy",
            "state": "approved",
            "reviewer": reviewer_name,
            "reason": review_reason,
            "strategy": normalized,
            "supersedes_receipt_digest": previous_digest,
            "authority": "NAMED_BENCHMARK_POLICY_REVIEW",
            "reviewed_at": now,
        }
        relative = Path("receipts") / f"benchmark-review-{review_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.benchmark-review+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT receipt_digest FROM benchmark_policy_reviews "
                "WHERE benchmark='dgx_spark' AND review_kind='dgx_foam_lod_strategy' "
                "ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            current_digest = current["receipt_digest"] if current else None
            if current_digest != previous_digest:
                raise RuntimeError("foam LOD policy changed during named review")
            connection.execute(
                "INSERT INTO benchmark_policy_reviews(id,benchmark,review_kind,state,reviewer,"
                "reason,strategy_json,receipt_digest,supersedes_receipt_digest,created_at) "
                "VALUES(?,?,?,?,?,?,?,?,?,?)",
                (
                    review_id,
                    "dgx_spark",
                    "dgx_foam_lod_strategy",
                    "approved",
                    reviewer_name,
                    review_reason,
                    json.dumps(normalized),
                    artifact.digest,
                    previous_digest,
                    now,
                ),
            )
        approval = {
            "id": review_id,
            "benchmark": "dgx_spark",
            "state": "approved",
            "review_kind": receipt["review_kind"],
            "reviewer": receipt["reviewer"],
            "reason": receipt["reason"],
            "strategy": normalized,
            "reviewed_at": now,
            "receipt_digest": artifact.digest,
            "receipt_path": str(relative),
            "supersedes_receipt_digest": previous_digest,
            "authority": "NAMED_BENCHMARK_POLICY_REVIEW",
        }
        project = self.project.project()
        project["metadata"] = {**project.get("metadata", {}), "foam_lod_approval": approval}
        project["updated_at"] = now
        atomic_write_json(self.project.project_file, project)
        return {**approval, "artifact": artifact.to_dict(), "receipt": receipt}

    def dgx_foam_lod_status(self) -> dict[str, Any]:
        metadata = self.project.project().get("metadata", {})
        required = metadata.get("benchmark") == "dgx_spark"
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT rowid AS ledger_sequence,* FROM benchmark_policy_reviews "
                "WHERE benchmark='dgx_spark' AND review_kind='dgx_foam_lod_strategy' "
                "ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
        ledger_backed = row is not None
        approval = (
            {
                "id": row["id"],
                "benchmark": row["benchmark"],
                "state": row["state"],
                "review_kind": row["review_kind"],
                "reviewer": row["reviewer"],
                "reason": row["reason"],
                "strategy": json.loads(row["strategy_json"]),
                "reviewed_at": row["created_at"],
                "receipt_digest": row["receipt_digest"],
                "supersedes_receipt_digest": row["supersedes_receipt_digest"],
                "authority": "NAMED_BENCHMARK_POLICY_REVIEW",
            }
            if row is not None
            else metadata.get("foam_lod_approval")
        )
        result = {
            "required": required,
            "valid": False,
            "approval": approval if isinstance(approval, dict) else None,
            "error": None,
        }
        if not required or not isinstance(approval, dict):
            return result
        if not ledger_backed:
            result["error"] = (
                "legacy foam LOD metadata has no authoritative policy-ledger row; "
                "named review is required"
            )
            return result
        try:
            digest = str(approval.get("receipt_digest", ""))
            path = self.artifacts.path_for(digest)
            if not path.is_file() or sha256_file(path)[0] != digest:
                raise ValueError("foam LOD review receipt is missing or corrupt")
            receipt = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(receipt, dict):
                raise ValueError("foam LOD review receipt must be an object")
            normalized = self._validate_foam_strategy(dict(receipt.get("strategy") or {}))
            required_views = self._required_validation_views()
            if not required_views.issubset(normalized["validation_views"]):
                raise ValueError("foam LOD review no longer covers every acceptance view")
            receipt_fields = {
                "schema_version",
                "receipt_type",
                "id",
                "benchmark",
                "review_kind",
                "state",
                "reviewer",
                "reason",
                "strategy",
                "supersedes_receipt_digest",
                "authority",
                "reviewed_at",
            }
            with self.project.connection() as connection:
                previous = connection.execute(
                    "SELECT receipt_digest FROM benchmark_policy_reviews "
                    "WHERE benchmark='dgx_spark' "
                    "AND review_kind='dgx_foam_lod_strategy' AND rowid<? "
                    "ORDER BY rowid DESC LIMIT 1",
                    (row["ledger_sequence"],),
                ).fetchone()
            expected_supersedes = previous["receipt_digest"] if previous else None
            supersession_valid = (
                approval.get("supersedes_receipt_digest") == expected_supersedes
            )
            valid = bool(
                set(receipt) == receipt_fields
                and receipt.get("schema_version") == 1
                and receipt.get("id") == approval.get("id")
                and receipt.get("benchmark") == approval.get("benchmark")
                and receipt.get("review_kind") == approval.get("review_kind")
                and receipt.get("state") == approval.get("state")
                and _text(receipt.get("reviewer"), "foam LOD reviewer", maximum=200)
                == approval.get("reviewer")
                and _text(receipt.get("reason"), "foam LOD review reason", maximum=2000)
                == approval.get("reason")
                and canonical_json(receipt.get("strategy")) == canonical_json(normalized)
                and canonical_json(approval.get("strategy")) == canonical_json(normalized)
                and receipt.get("supersedes_receipt_digest")
                == approval.get("supersedes_receipt_digest")
                and receipt.get("authority") == "NAMED_BENCHMARK_POLICY_REVIEW"
                and receipt.get("reviewed_at") == approval.get("reviewed_at")
                and receipt.get("receipt_type") == "benchmark_named_review"
                and supersession_valid
            )
            result["valid"] = valid
            if not valid:
                result["error"] = "foam LOD review receipt semantics are inconsistent"
        except (OSError, ValueError, TypeError) as error:
            result["error"] = str(error)
        return result

    def _required_validation_views(self) -> set[str]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT viewpoint_label FROM reference_items "
                "WHERE acceptance_eligible=1 AND media_type LIKE 'image/%' "
                "AND viewpoint_label IS NOT NULL"
            ).fetchall()
        return {str(row["viewpoint_label"]).strip() for row in rows if row["viewpoint_label"]}

    @staticmethod
    def _validate_foam_strategy(strategy: dict[str, Any]) -> dict[str, Any]:
        if not isinstance(strategy, dict):
            raise ValueError("foam LOD strategy must be an object")
        if strategy.get("switch_policy") != "screen_space_pixel_footprint":
            raise ValueError("foam LOD requires a screen-space pixel-footprint switch policy")
        tiers = strategy.get("tiers")
        if (
            not isinstance(tiers, list)
            or len(tiers) != 3
            or any(not isinstance(item, dict) for item in tiers)
        ):
            raise ValueError("foam LOD requires hero, mid, and background tiers")
        by_name = {str(item.get("name", "")): dict(item) for item in tiers}
        if set(by_name) != {"hero", "mid", "background"}:
            raise ValueError("foam LOD tiers must be hero, mid, and background")
        allowed = {
            "physical_geometry",
            "geometry_nodes",
            "proxy_geometry",
            "normal_map",
            "procedural_texture",
        }
        thresholds: dict[str, float] = {}
        for name, tier in by_name.items():
            if set(tier) != {"name", "representation", "minimum_screen_diameter_px"}:
                raise ValueError(f"foam LOD tier {name} has an invalid schema")
            representation = tier.get("representation")
            if representation not in allowed:
                raise ValueError(f"unsupported foam representation for {name}")
            value = tier.get("minimum_screen_diameter_px")
            if (
                isinstance(value, bool)
                or not isinstance(value, (int, float))
                or not math.isfinite(float(value))
                or float(value) < 0.0
            ):
                raise ValueError("foam LOD thresholds must be non-negative numbers")
            thresholds[name] = float(value)
            tier["minimum_screen_diameter_px"] = float(value)
        if by_name["hero"]["representation"] != "physical_geometry":
            raise ValueError("hero foam LOD must use physical geometry")
        if by_name["background"]["representation"] not in {
            "normal_map",
            "procedural_texture",
        }:
            raise ValueError("background foam LOD must use a non-geometric representation")
        if not thresholds["hero"] > thresholds["mid"] > thresholds["background"]:
            raise ValueError("foam LOD screen-space thresholds must decrease by tier")
        validation_views = strategy.get("validation_views")
        if (
            not isinstance(validation_views, list)
            or not validation_views
            or any(not isinstance(item, str) or not item.strip() for item in validation_views)
        ):
            raise ValueError("foam LOD approval requires named validation views")
        crossfade = strategy.get("crossfade", False)
        if not isinstance(crossfade, bool):
            raise ValueError("foam LOD crossfade must be a boolean")
        return {
            "switch_policy": "screen_space_pixel_footprint",
            "tiers": [by_name[name] for name in ("hero", "mid", "background")],
            "validation_views": sorted({str(item).strip() for item in validation_views}),
            "crossfade": crossfade,
        }
