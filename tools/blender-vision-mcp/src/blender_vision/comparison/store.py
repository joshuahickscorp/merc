from __future__ import annotations

import hashlib
import json
import math
import tempfile
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore


class ComparisonStore:
    """Persist and independently replay immutable per-view comparison evidence."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self._automatic_reference_mask_cache: dict[str, tuple[Any, str, str]] = {}

    def record(
        self,
        comparison_id: str,
        *,
        reference_id: str,
        render_digest: str,
        residual_digest: str,
        metrics: dict[str, Any],
        engine: str,
        created_at: str | None = None,
    ) -> dict[str, Any]:
        prepared = self._prepare_record(
            comparison_id,
            reference_id=reference_id,
            render_digest=render_digest,
            residual_digest=residual_digest,
            metrics=metrics,
            engine=engine,
            created_at=created_at,
        )
        row = prepared["row"]
        with self.project.connection() as connection:
            self._insert_prepared(connection, prepared)
        return {
            **row,
            "receipt": prepared["receipt"],
            "receipt_artifact": prepared["receipt_artifact"],
            "path": prepared["path"],
        }

    def _prepare_record(
        self,
        comparison_id: str,
        *,
        reference_id: str,
        render_digest: str,
        residual_digest: str,
        metrics: dict[str, Any],
        engine: str,
        created_at: str | None = None,
    ) -> dict[str, Any]:
        """Build and verify comparison artifacts without creating database authority."""
        created = created_at or utc_now()
        row = {
            "id": comparison_id,
            "reference_id": reference_id,
            "render_digest": render_digest,
            "residual_digest": residual_digest,
            "metrics": metrics,
            "created_at": created,
        }
        receipt = self._receipt(row, engine=engine)
        for digest in (
            receipt["reference_artifact_digest"],
            receipt["reference_input_digest"],
            receipt.get("mask_input_digest"),
            render_digest,
            residual_digest,
        ):
            if digest is not None and not self._artifact_valid(digest):
                raise ValueError("comparison receipt references a missing or corrupt artifact")
        if not self._replay(row, engine=engine, receipt=receipt)["valid"]:
            raise ValueError("comparison metrics or residual do not reproduce from stored inputs")
        relative = Path("comparisons") / f"comparison-{comparison_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.residual-comparison+json",
        )
        return {
            "row": row,
            "receipt": receipt,
            "receipt_artifact": artifact.to_dict(),
            "path": str(relative),
        }

    @staticmethod
    def _insert_prepared(connection: Any, prepared: dict[str, Any]) -> None:
        row = prepared["row"]
        connection.execute(
            "INSERT INTO comparisons(id,reference_id,render_digest,residual_digest,"
            "metrics_json,receipt_digest,created_at) VALUES(?,?,?,?,?,?,?)",
            (
                row["id"],
                row["reference_id"],
                row["render_digest"],
                row["residual_digest"],
                json.dumps(row["metrics"]),
                prepared["receipt_artifact"]["digest"],
                row["created_at"],
            ),
        )

    def migrate_legacy(self, comparison_id: str) -> dict[str, Any]:
        row = self._row(comparison_id)
        if row["receipt_digest"]:
            return self.verify_record(row, replay=True)
        engine = self._legacy_engine(row["metrics"])
        replay = self._replay(row, engine=engine, receipt=None)
        if not replay["valid"]:
            raise ValueError("legacy comparison cannot be reproduced from immutable inputs")
        receipt = self._receipt(row, engine=engine)
        receipt["migration"] = {
            "authority": "DETERMINISTIC_LEGACY_COMPARISON_REPLAY",
            "migrated_at": utc_now(),
            "metrics_replayed": True,
            "residual_digest_reproduced": True,
        }
        relative = Path("comparisons") / f"comparison-{comparison_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.residual-comparison+json",
        )
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE comparisons SET receipt_digest=? WHERE id=? AND receipt_digest IS NULL "
                "AND reference_id=? AND render_digest=? AND residual_digest=? AND metrics_json=? "
                "AND created_at=?",
                (
                    artifact.digest,
                    row["id"],
                    row["reference_id"],
                    row["render_digest"],
                    row["residual_digest"],
                    row["metrics_json"],
                    row["created_at"],
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("legacy comparison changed during receipt migration")
        return self.verify(comparison_id, replay=True)

    def recompute_and_supersede(self, comparison_id: str) -> dict[str, Any]:
        """Replace unreproducible legacy metrics from the same immutable inputs."""
        row = self._row(comparison_id)
        if row.get("superseded_by_id") or row.get("supersession_digest"):
            raise ValueError("comparison has already been superseded")
        if self.verify_record(row, replay=True)["valid"]:
            raise ValueError("valid replayable comparison does not require recomputation")
        legacy_engine = self._legacy_engine(row["metrics"])
        engine = (
            "compare_silhouettes_v3"
            if legacy_engine == "compare_silhouettes_v2"
            else legacy_engine
        )
        provisional = self._receipt(row, engine=engine)
        reference_path = self.artifacts.path_for(provisional["reference_input_digest"])
        render_path = self.artifacts.path_for(row["render_digest"])
        mask_digest = provisional.get("mask_input_digest")
        mask_path = self.artifacts.path_for(mask_digest) if mask_digest else None
        with tempfile.TemporaryDirectory(prefix="bvmcp-comparison-recompute-") as temporary:
            residual_path = Path(temporary) / "residual.png"
            if engine in {"compare_silhouettes_v2", "compare_silhouettes_v3"}:
                from blender_vision.comparison.metrics import compare_silhouettes

                metrics = compare_silhouettes(
                    reference_path,
                    render_path,
                    residual_path,
                    reviewed_mask_path=mask_path,
                    reviewed_mask_record=row["metrics"].get("reference_mask"),
                    automatic_segmentation_maximum_dimension=(
                        self._automatic_segmentation_maximum_dimension(engine)
                    ),
                    prepared_automatic_reference_mask=(
                        self._cached_automatic_reference_mask(provisional, engine)
                        if mask_path is None
                        else None
                    ),
                )
                if "locality" in row["metrics"]:
                    metrics["locality"] = row["metrics"]["locality"]
            else:
                from blender_vision.comparison.images import compare_pair

                metrics = compare_pair(reference_path, render_path, residual_path)
            residual = self.artifacts.ingest_file(residual_path, media_type="image/png")
        replacement_id = str(uuid.uuid4())
        prepared_replacement = self._prepare_record(
            replacement_id,
            reference_id=row["reference_id"],
            render_digest=row["render_digest"],
            residual_digest=residual.digest,
            metrics=metrics,
            engine=engine,
        )
        replacement = {
            **prepared_replacement["row"],
            "receipt": prepared_replacement["receipt"],
            "receipt_artifact": prepared_replacement["receipt_artifact"],
            "path": prepared_replacement["path"],
        }
        now = utc_now()
        supersession_id = str(uuid.uuid4())
        receipt = {
            "schema_version": 1,
            "receipt_type": "comparison_supersession",
            "id": supersession_id,
            "comparison_id": comparison_id,
            "superseded_record_sha256": self._record_sha256(row),
            "superseded_by_id": replacement_id,
            "superseded_by_receipt_digest": replacement["receipt_artifact"]["digest"],
            "reference_id": row["reference_id"],
            "render_digest": row["render_digest"],
            "authority": (
                "DETERMINISTIC_RECOMPUTATION_REPLACES_UNREPRODUCIBLE_LEGACY_METRICS"
            ),
            "legacy_metrics_reproduced": False,
            "replacement_replayed": True,
            "camera_or_scene_authority_changed": False,
            "created_at": now,
        }
        relative = Path("comparisons") / f"comparison-supersession-{supersession_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.comparison-supersession+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            self._insert_prepared(connection, prepared_replacement)
            updated = connection.execute(
                "UPDATE comparisons SET superseded_by_id=?,supersession_digest=? "
                "WHERE id=? AND superseded_by_id IS NULL AND supersession_digest IS NULL",
                (replacement_id, artifact.digest, comparison_id),
            )
            if updated.rowcount != 1:
                raise RuntimeError("comparison supersession raced with another update")
        return {
            "replacement": replacement,
            "supersession": receipt,
            "supersession_digest": artifact.digest,
        }

    def verify(self, comparison_id: str, *, replay: bool = True) -> dict[str, Any]:
        return self.verify_record(self._row(comparison_id), replay=replay)

    def verify_record(self, raw: dict[str, Any] | Any, *, replay: bool = True) -> dict[str, Any]:
        try:
            row = self._normalized(raw)
            receipt_digest = row.get("receipt_digest")
            if not receipt_digest or not self._artifact_valid(receipt_digest):
                return {"valid": False, "receipt_valid": False, "replay_valid": False}
            receipt = json.loads(
                self.artifacts.path_for(str(receipt_digest)).read_text(encoding="utf-8")
            )
            expected = self._receipt(row, engine=str(receipt.get("engine", "")))
            migration = receipt.get("migration")
            if migration is not None:
                expected["migration"] = migration
            migration_valid = bool(
                migration is None
                or migration
                == {
                    "authority": "DETERMINISTIC_LEGACY_COMPARISON_REPLAY",
                    "migrated_at": migration.get("migrated_at"),
                    "metrics_replayed": True,
                    "residual_digest_reproduced": True,
                }
            )
            receipt_valid = bool(
                canonical_json(receipt) == canonical_json(expected)
                and migration_valid
                and receipt.get("receipt_type") == "residual_comparison"
                and self._artifact_valid(row["render_digest"])
                and self._artifact_valid(row["residual_digest"])
                and self._artifact_valid(receipt.get("reference_artifact_digest"))
                and self._artifact_valid(receipt.get("reference_input_digest"))
                and (
                    receipt.get("mask_input_digest") is None
                    or self._artifact_valid(receipt.get("mask_input_digest"))
                )
            )
            replay_result = (
                self._replay(row, engine=receipt["engine"], receipt=receipt)
                if receipt_valid and replay
                else {"valid": receipt_valid}
            )
            return {
                "valid": bool(receipt_valid and replay_result["valid"]),
                "receipt_valid": receipt_valid,
                "replay_valid": bool(replay_result["valid"]),
                "receipt": receipt,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "receipt_valid": False, "replay_valid": False}

    def verify_supersession(
        self,
        raw: dict[str, Any] | Any,
        *,
        source_verification: dict[str, Any] | None = None,
        replacement_verification: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        try:
            row = self._normalized(raw)
            replacement_id = row.get("superseded_by_id")
            digest = row.get("supersession_digest")
            if not replacement_id and not digest:
                return {"valid": False, "superseded": False}
            if not replacement_id or not digest or not self._artifact_valid(digest):
                return {"valid": False, "superseded": True}
            receipt = json.loads(self.artifacts.path_for(digest).read_text(encoding="utf-8"))
            replacement = self._row(str(replacement_id))
            replacement_verification = replacement_verification or self.verify_record(
                replacement, replay=True
            )
            common_valid = bool(
                receipt.get("schema_version") in {1, 2}
                and receipt.get("receipt_type") == "comparison_supersession"
                and receipt.get("comparison_id") == row["id"]
                and receipt.get("superseded_record_sha256") == self._record_sha256(row)
                and receipt.get("superseded_by_id") == replacement["id"]
                and receipt.get("superseded_by_receipt_digest")
                == replacement["receipt_digest"]
                and receipt.get("reference_id") == row["reference_id"]
                and receipt.get("render_digest") == row["render_digest"]
                and replacement["reference_id"] == row["reference_id"]
                and replacement["render_digest"] == row["render_digest"]
                and receipt.get("camera_or_scene_authority_changed") is False
                and replacement_verification["valid"]
            )
            authority = receipt.get("authority")
            if authority == (
                "DETERMINISTIC_RECOMPUTATION_REPLACES_UNREPRODUCIBLE_LEGACY_METRICS"
            ):
                authority_valid = bool(
                    receipt.get("schema_version") == 1
                    and receipt.get("legacy_metrics_reproduced") is False
                    and receipt.get("replacement_replayed") is True
                )
            elif authority == "DETERMINISTIC_DUPLICATE_COMPARISON_COLLAPSE":
                source_verification = source_verification or self.verify_record(
                    row, replay=True
                )
                authority_valid = bool(
                    receipt.get("schema_version") == 2
                    and source_verification["valid"]
                    and receipt.get("source_replayed") is True
                    and receipt.get("replacement_replayed") is True
                    and receipt.get("metrics_identical") is True
                    and receipt.get("residual_identical") is True
                    and row["metrics"] == replacement["metrics"]
                    and row["residual_digest"] == replacement["residual_digest"]
                    and source_verification["receipt"].get("engine")
                    == replacement_verification["receipt"].get("engine")
                )
            else:
                authority_valid = False
            valid = bool(common_valid and authority_valid)
            return {
                "valid": valid,
                "superseded": True,
                "receipt": receipt,
                "replacement_valid": replacement_verification["valid"],
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "superseded": True}

    def supersede_duplicate(
        self, comparison_id: str, *, canonical_comparison_id: str
    ) -> dict[str, Any]:
        """Collapse a replay-identical duplicate without deleting either record."""
        if comparison_id == canonical_comparison_id:
            raise ValueError("duplicate and canonical comparison must differ")
        duplicate = self._row(comparison_id)
        canonical = self._row(canonical_comparison_id)
        if duplicate.get("superseded_by_id") or duplicate.get("supersession_digest"):
            raise ValueError("comparison has already been superseded")
        duplicate_verification = self.verify_record(duplicate, replay=False)
        canonical_verification = self.verify_record(canonical, replay=True)
        if not duplicate_verification["valid"] or not canonical_verification["valid"]:
            raise ValueError("duplicate collapse requires two replayable comparisons")
        if (
            duplicate["reference_id"] != canonical["reference_id"]
            or duplicate["render_digest"] != canonical["render_digest"]
            or duplicate["residual_digest"] != canonical["residual_digest"]
            or duplicate["metrics"] != canonical["metrics"]
            or duplicate_verification["receipt"].get("engine")
            != canonical_verification["receipt"].get("engine")
        ):
            raise ValueError("comparisons are not deterministic duplicates")
        now = utc_now()
        supersession_id = str(uuid.uuid4())
        receipt = {
            "schema_version": 2,
            "receipt_type": "comparison_supersession",
            "id": supersession_id,
            "comparison_id": comparison_id,
            "superseded_record_sha256": self._record_sha256(duplicate),
            "superseded_by_id": canonical_comparison_id,
            "superseded_by_receipt_digest": canonical["receipt_digest"],
            "reference_id": duplicate["reference_id"],
            "render_digest": duplicate["render_digest"],
            "authority": "DETERMINISTIC_DUPLICATE_COMPARISON_COLLAPSE",
            "source_replayed": True,
            "replacement_replayed": True,
            "metrics_identical": True,
            "residual_identical": True,
            "camera_or_scene_authority_changed": False,
            "created_at": now,
        }
        relative = Path("comparisons") / f"comparison-supersession-{supersession_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.comparison-supersession+json",
        )
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE comparisons SET superseded_by_id=?,supersession_digest=? "
                "WHERE id=? AND superseded_by_id IS NULL AND supersession_digest IS NULL",
                (canonical_comparison_id, artifact.digest, comparison_id),
            )
            if updated.rowcount != 1:
                raise RuntimeError("duplicate comparison changed during supersession")
        return {
            "supersession": receipt,
            "supersession_digest": artifact.digest,
            "verification": self.verify_supersession(
                self._row(comparison_id),
                source_verification=duplicate_verification,
                replacement_verification=canonical_verification,
            ),
        }

    def _row(self, comparison_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM comparisons WHERE id=?", (comparison_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown comparison: {comparison_id}")
        return self._normalized(row)

    @staticmethod
    def _normalized(raw: dict[str, Any] | Any) -> dict[str, Any]:
        row = dict(raw)
        if "metrics" not in row:
            row["metrics"] = json.loads(row["metrics_json"])
        return row

    @staticmethod
    def _record_sha256(row: dict[str, Any]) -> str:
        snapshot = {
            "id": row["id"],
            "reference_id": row["reference_id"],
            "render_digest": row["render_digest"],
            "residual_digest": row["residual_digest"],
            "metrics": row["metrics"],
            "receipt_digest": row.get("receipt_digest"),
            "created_at": row["created_at"],
        }
        return hashlib.sha256(canonical_json(snapshot)).hexdigest()

    def _receipt(self, row: dict[str, Any], *, engine: str) -> dict[str, Any]:
        if engine not in {
            "compare_silhouettes_v2",
            "compare_silhouettes_v3",
            "compare_pair_v1",
        }:
            raise ValueError("comparison receipt uses an unsupported engine")
        metrics = row["metrics"]
        iou = float(metrics["silhouette_iou"])
        if not math.isfinite(iou) or not 0.0 <= iou <= 1.0:
            raise ValueError("comparison silhouette IoU must be finite and bounded")
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT artifact_digest FROM reference_items WHERE id=?",
                (row["reference_id"],),
            ).fetchone()
        if reference is None:
            raise ValueError("comparison references unknown evidence")
        locality = metrics.get("locality", {})
        crop_digests = locality.get("crop_artifact_digests", {})
        reference_input_digest = crop_digests.get("reference") or reference[
            "artifact_digest"
        ]
        mask_input_digest = crop_digests.get("reviewed_mask") or metrics.get(
            "reference_mask", {}
        ).get("artifact_digest")
        return {
            "schema_version": 1,
            "receipt_type": "residual_comparison",
            "id": row["id"],
            "reference_id": row["reference_id"],
            "reference_artifact_digest": reference["artifact_digest"],
            "reference_input_digest": reference_input_digest,
            "mask_input_digest": mask_input_digest,
            "render_digest": row["render_digest"],
            "residual_digest": row["residual_digest"],
            "metrics": metrics,
            "engine": engine,
            "created_at": row["created_at"],
        }

    def _replay(
        self,
        row: dict[str, Any],
        *,
        engine: str,
        receipt: dict[str, Any] | None,
    ) -> dict[str, Any]:
        metrics = row["metrics"]
        provisional = self._receipt(row, engine=engine) if receipt is None else receipt
        reference_path = self.artifacts.path_for(provisional["reference_input_digest"])
        render_path = self.artifacts.path_for(row["render_digest"])
        mask_digest = provisional.get("mask_input_digest")
        mask_path = self.artifacts.path_for(mask_digest) if mask_digest else None
        with tempfile.TemporaryDirectory(prefix="bvmcp-comparison-replay-") as temporary:
            residual_path = Path(temporary) / "residual.png"
            if engine in {"compare_silhouettes_v2", "compare_silhouettes_v3"}:
                from blender_vision.comparison.metrics import compare_silhouettes

                recomputed = compare_silhouettes(
                    reference_path,
                    render_path,
                    residual_path,
                    reviewed_mask_path=mask_path,
                    reviewed_mask_record=metrics.get("reference_mask"),
                    automatic_segmentation_maximum_dimension=(
                        self._automatic_segmentation_maximum_dimension(engine)
                    ),
                    prepared_automatic_reference_mask=(
                        self._cached_automatic_reference_mask(provisional, engine)
                        if mask_path is None
                        else None
                    ),
                )
                if "locality" in metrics:
                    recomputed["locality"] = metrics["locality"]
            elif engine == "compare_pair_v1":
                from blender_vision.comparison.images import compare_pair

                recomputed = compare_pair(reference_path, render_path, residual_path)
            else:
                raise ValueError("comparison replay uses an unsupported engine")
            residual_digest, _ = sha256_file(residual_path)
        return {
            "valid": bool(
                canonical_json(recomputed) == canonical_json(metrics)
                and residual_digest == row["residual_digest"]
            ),
            "metrics": recomputed,
            "residual_digest": residual_digest,
        }

    @staticmethod
    def _legacy_engine(metrics: dict[str, Any]) -> str:
        return (
            "compare_pair_v1"
            if metrics.get("segmentation_method") == "alpha_or_corner_background_difference"
            else "compare_silhouettes_v2"
        )

    @staticmethod
    def _automatic_segmentation_maximum_dimension(engine: str) -> int:
        from blender_vision.comparison.metrics import (
            BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION,
            LEGACY_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION,
        )

        return (
            BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
            if engine == "compare_silhouettes_v3"
            else LEGACY_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
        )

    def _cached_automatic_reference_mask(
        self, receipt: dict[str, Any], engine: str
    ) -> tuple[Any, str, str]:
        from PIL import Image

        from blender_vision.comparison.metrics import _reference_mask

        digest = str(receipt["reference_input_digest"])
        key = f"{engine}:{digest}"
        cached = self._automatic_reference_mask_cache.get(key)
        if cached is None:
            with Image.open(self.artifacts.path_for(digest)) as image:
                cached = _reference_mask(
                    image,
                    automatic_segmentation_maximum_dimension=(
                        self._automatic_segmentation_maximum_dimension(engine)
                    ),
                )
            self._automatic_reference_mask_cache[key] = cached
        return cached

    def _artifact_valid(self, digest: Any) -> bool:
        if not isinstance(digest, str):
            return False
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT size,relative_path FROM artifacts WHERE digest=?", (digest,)
            ).fetchone()
        if row is None:
            return False
        try:
            path = self.artifacts.path_for(digest)
            registered = (self.project.root / row["relative_path"]).resolve()
            if path.resolve() != registered or not path.is_file():
                return False
            actual_digest, actual_size = sha256_file(path)
            return actual_digest == digest and actual_size == int(row["size"])
        except (OSError, TypeError, ValueError):
            return False
