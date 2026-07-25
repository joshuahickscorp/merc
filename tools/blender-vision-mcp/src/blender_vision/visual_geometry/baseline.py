from __future__ import annotations

import hashlib
import json
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore


class VisualBaselineStore:
    """Freeze and replay the complete visual-comparison state of one or more scenes."""

    SCHEMA_VERSION = 1
    _JSON_COLUMNS = {
        "scene_assets": {"inventory_json"},
        "scene_transitions": set(),
        "candidate_evaluations": {"gates_json", "metrics_json", "regressions_json"},
        "camera_solutions": {"solution_json", "diagnostics_json"},
        "camera_decisions": {"decision_json"},
        "reference_items": {"metadata_json"},
        "reference_mask_proposals": {"record_json"},
        "reference_masks": {
            "visible_components_json",
            "excluded_components_json",
            "roi_json",
        },
        "render_runs": {"config_json", "outputs_json"},
        "visual_geometry_rigs": {"config_json"},
        "visual_geometry_scorecards": {"scorecard_json"},
        "manufactured_form_audits": {"report_json"},
        "semantic_geometry_bindings": {"record_json"},
        "visual_component_packets": {"packet_json"},
        "visual_frequency_scorecards": {"scorecard_json"},
        "visual_defect_diagnoses": {"report_json"},
        "exports": {"config_json", "worker_json"},
        "beast_benchmark_runs": {"report_json"},
    }

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def freeze(
        self,
        *,
        label: str,
        scene_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        label = label.strip()
        if not label:
            raise ValueError("visual baseline freeze requires a label")
        selected_scene_ids = self._resolve_scene_ids(scene_ids)
        tables = self._capture_tables(selected_scene_ids)
        scene_states = [str(row["state"]) for row in tables["scene_assets"]]
        state = (
            "AUTHORITATIVE_ACCEPTED_BASELINE"
            if scene_states and all(value in {"ACCEPTED", "PROMOTED"} for value in scene_states)
            else "CURRENT_DIAGNOSTIC_BASELINE"
        )
        project_metadata = self.project.project()
        artifact_digests = self._artifact_digests(tables)
        artifact_manifest = self._artifact_manifest(artifact_digests)
        snapshot = {
            "schema_version": self.SCHEMA_VERSION,
            "project_id": project_metadata["id"],
            "project_metadata": project_metadata,
            "scene_ids": selected_scene_ids,
            "tables": tables,
            "artifact_manifest": artifact_manifest,
            "comparison_contract": self._comparison_contract(tables),
            "isolation_policy": {
                "candidate_scene_must_be_new": True,
                "captured_rows_are_immutable": True,
                "captured_artifacts_are_content_addressed": True,
                "candidate_must_use_captured_rig_when_available": True,
                "rollback_checkpoint_required": True,
            },
        }
        snapshot_digest = hashlib.sha256(canonical_json(snapshot)).hexdigest()
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT * FROM visual_baseline_freezes WHERE snapshot_digest=?",
                (snapshot_digest,),
            ).fetchone()
        if existing is not None:
            return self._normalize(existing)

        baseline_id = str(uuid.uuid4())
        created_at = utc_now()
        receipt = {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "visual_baseline_freeze",
            "id": baseline_id,
            "label": label,
            "state": state,
            "snapshot": snapshot,
            "snapshot_digest": snapshot_digest,
            "authority": (
                "IMMUTABLE_ACCEPTED_VISUAL_BASELINE"
                if state == "AUTHORITATIVE_ACCEPTED_BASELINE"
                else "IMMUTABLE_DIAGNOSTIC_VISUAL_BASELINE_NO_ACCEPTANCE_AUTHORITY"
            ),
            "created_at": created_at,
        }
        relative = Path("receipts") / f"visual-baseline-freeze-{baseline_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.visual-baseline-freeze+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO visual_baseline_freezes"
                "(id,label,state,snapshot_json,snapshot_digest,receipt_digest,created_at) "
                "VALUES(?,?,?,?,?,?,?)",
                (
                    baseline_id,
                    label,
                    state,
                    json.dumps(snapshot),
                    snapshot_digest,
                    artifact.digest,
                    created_at,
                ),
            )
        return {
            "id": baseline_id,
            "label": label,
            "state": state,
            "snapshot": snapshot,
            "snapshot_digest": snapshot_digest,
            "receipt_digest": artifact.digest,
            "created_at": created_at,
        }

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM visual_baseline_freezes ORDER BY created_at,id"
            ).fetchall()
        return [self._normalize(row) for row in rows]

    def verify(self, baseline_id: str) -> dict[str, Any]:
        try:
            baseline = self._get(baseline_id)
            snapshot = baseline["snapshot"]
            snapshot_digest = hashlib.sha256(canonical_json(snapshot)).hexdigest()
            receipt_path = self.artifacts.path_for(baseline["receipt_digest"])
            receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
            expected = {
                "schema_version": self.SCHEMA_VERSION,
                "receipt_type": "visual_baseline_freeze",
                "id": baseline["id"],
                "label": baseline["label"],
                "state": baseline["state"],
                "snapshot": snapshot,
                "snapshot_digest": snapshot_digest,
                "authority": (
                    "IMMUTABLE_ACCEPTED_VISUAL_BASELINE"
                    if baseline["state"] == "AUTHORITATIVE_ACCEPTED_BASELINE"
                    else "IMMUTABLE_DIAGNOSTIC_VISUAL_BASELINE_NO_ACCEPTANCE_AUTHORITY"
                ),
                "created_at": baseline["created_at"],
            }
            receipt_valid = bool(
                self._artifact_valid(baseline["receipt_digest"])
                and baseline["snapshot_digest"] == snapshot_digest
                and canonical_json(receipt) == canonical_json(expected)
            )
            artifact_valid = all(
                self._artifact_valid(digest) and self._artifact_record(digest) == record
                for digest, record in snapshot["artifact_manifest"].items()
            )
            rows_valid = all(
                self._captured_rows_still_match(table, rows)
                for table, rows in snapshot["tables"].items()
            )
            return {
                "valid": bool(receipt_valid and artifact_valid and rows_valid),
                "receipt_valid": receipt_valid,
                "artifact_valid": artifact_valid,
                "captured_rows_valid": rows_valid,
                "receipt": receipt,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {
                "valid": False,
                "receipt_valid": False,
                "artifact_valid": False,
                "captured_rows_valid": False,
            }

    def _resolve_scene_ids(self, scene_ids: list[str] | None) -> list[str]:
        with self.project.connection() as connection:
            known = {
                str(row["id"])
                for row in connection.execute("SELECT id FROM scene_assets").fetchall()
            }
        selected = sorted(set(scene_ids or known))
        if not selected:
            raise ValueError("visual baseline freeze requires at least one scene")
        unknown = sorted(set(selected) - known)
        if unknown:
            raise KeyError(f"unknown scene ids: {unknown}")
        return selected

    def _capture_tables(self, scene_ids: list[str]) -> dict[str, list[dict[str, Any]]]:
        placeholders = ",".join("?" for _ in scene_ids)
        tables: dict[str, list[dict[str, Any]]] = {}
        with self.project.connection() as connection:
            for table in (
                "scene_assets",
                "scene_transitions",
                "candidate_evaluations",
                "render_runs",
                "visual_geometry_rigs",
                "visual_geometry_scorecards",
                "manufactured_form_audits",
                "semantic_geometry_bindings",
                "visual_frequency_scorecards",
                "visual_defect_diagnoses",
                "exports",
            ):
                scene_column = "id" if table == "scene_assets" else "scene_id"
                rows = connection.execute(
                    f"SELECT * FROM {table} WHERE {scene_column} IN ({placeholders}) "
                    "ORDER BY created_at,id",
                    scene_ids,
                ).fetchall()
                tables[table] = [self._normalize_row(table, row) for row in rows]

            binding_ids = [
                str(row["id"]) for row in tables["semantic_geometry_bindings"]
            ]
            render_ids = [str(row["id"]) for row in tables["render_runs"]]
            packet_clauses = []
            packet_parameters: list[str] = []
            if binding_ids:
                packet_clauses.append(
                    "binding_id IN (" + ",".join("?" for _ in binding_ids) + ")"
                )
                packet_parameters.extend(binding_ids)
            if render_ids:
                packet_clauses.append(
                    "render_run_id IN (" + ",".join("?" for _ in render_ids) + ")"
                )
                packet_parameters.extend(render_ids)
            packet_rows = (
                connection.execute(
                    "SELECT * FROM visual_component_packets WHERE "
                    + " OR ".join(packet_clauses)
                    + " ORDER BY created_at,id",
                    packet_parameters,
                ).fetchall()
                if packet_clauses
                else []
            )
            tables["visual_component_packets"] = [
                self._normalize_row("visual_component_packets", row)
                for row in packet_rows
            ]

            camera_ids = sorted(
                {
                    str(row["camera_solution_id"])
                    for table in ("render_runs", "visual_geometry_rigs")
                    for row in tables[table]
                }
            )
            tables["camera_solutions"] = self._rows_by_ids(
                connection, "camera_solutions", camera_ids
            )
            tables["camera_decisions"] = self._rows_by_foreign_ids(
                connection, "camera_decisions", "solution_id", camera_ids
            )

            reference_ids = sorted(self._reference_ids(tables))
            tables["reference_items"] = self._rows_by_ids(
                connection, "reference_items", reference_ids
            )
            mask_ids, proposal_ids = self._mask_ids(tables["visual_geometry_scorecards"])
            tables["reference_masks"] = self._rows_by_ids(
                connection, "reference_masks", sorted(mask_ids)
            )
            tables["reference_mask_proposals"] = self._rows_by_ids(
                connection, "reference_mask_proposals", sorted(proposal_ids)
            )
            latest_benchmark = connection.execute(
                "SELECT * FROM beast_benchmark_runs ORDER BY created_at DESC,id DESC LIMIT 1"
            ).fetchall()
            tables["beast_benchmark_runs"] = [
                self._normalize_row("beast_benchmark_runs", row) for row in latest_benchmark
            ]
        return dict(sorted(tables.items()))

    def _rows_by_ids(self, connection: Any, table: str, ids: list[str]) -> list[dict[str, Any]]:
        if not ids:
            return []
        placeholders = ",".join("?" for _ in ids)
        rows = connection.execute(
            f"SELECT * FROM {table} WHERE id IN ({placeholders}) ORDER BY created_at,id", ids
        ).fetchall()
        return [self._normalize_row(table, row) for row in rows]

    def _rows_by_foreign_ids(
        self,
        connection: Any,
        table: str,
        column: str,
        ids: list[str],
    ) -> list[dict[str, Any]]:
        if not ids:
            return []
        placeholders = ",".join("?" for _ in ids)
        rows = connection.execute(
            f"SELECT * FROM {table} WHERE {column} IN ({placeholders}) ORDER BY created_at,id",
            ids,
        ).fetchall()
        return [self._normalize_row(table, row) for row in rows]

    def _normalize_row(self, table: str, raw: Any) -> dict[str, Any]:
        value = dict(raw)
        for column in self._JSON_COLUMNS.get(table, set()):
            if column in value and value[column] is not None:
                value[column] = json.loads(value[column])
        return value

    @staticmethod
    def _reference_ids(tables: dict[str, list[dict[str, Any]]]) -> set[str]:
        result: set[str] = set()
        for rig in tables["visual_geometry_rigs"]:
            result.update(str(value) for value in rig["config_json"].get("reference_ids", []))
        for run in tables["render_runs"]:
            result.update(
                str(output["reference_id"])
                for output in run["outputs_json"]
                if output.get("reference_id")
            )
        for scorecard in tables["visual_geometry_scorecards"]:
            result.add(str(scorecard["reference_id"]))
        return result

    @staticmethod
    def _mask_ids(scorecards: list[dict[str, Any]]) -> tuple[set[str], set[str]]:
        masks: set[str] = set()
        proposals: set[str] = set()
        for row in scorecards:
            inputs = row["scorecard_json"].get("inputs", {})
            mask_id = inputs.get("mask_id")
            if not mask_id:
                continue
            if inputs.get("mask_authority") == "REVIEWED_MASK":
                masks.add(str(mask_id))
            else:
                proposals.add(str(mask_id))
        return masks, proposals

    def _artifact_digests(self, tables: dict[str, list[dict[str, Any]]]) -> list[str]:
        with self.project.connection() as connection:
            known = {
                str(row["digest"])
                for row in connection.execute("SELECT digest FROM artifacts").fetchall()
            }
        found: set[str] = set()

        def visit(value: Any) -> None:
            if isinstance(value, dict):
                for item in value.values():
                    visit(item)
            elif isinstance(value, list):
                for item in value:
                    visit(item)
            elif isinstance(value, str) and value in known:
                found.add(value)

        visit(tables)
        return sorted(found)

    def _artifact_manifest(self, digests: list[str]) -> dict[str, dict[str, Any]]:
        return {digest: self._artifact_record(digest) for digest in digests}

    def _artifact_record(self, digest: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT digest,size,media_type,relative_path FROM artifacts WHERE digest=?",
                (digest,),
            ).fetchone()
        if row is None:
            raise KeyError(f"artifact is not registered: {digest}")
        value = dict(row)
        value["content_valid"] = self._artifact_valid(digest)
        return value

    @staticmethod
    def _comparison_contract(tables: dict[str, list[dict[str, Any]]]) -> dict[str, Any]:
        return {
            "scene_artifact_digests": {
                row["id"]: row["artifact_digest"] for row in tables["scene_assets"]
            },
            "camera_snapshot_sha256s": {
                row["id"]: hashlib.sha256(canonical_json(row["solution_json"])).hexdigest()
                for row in tables["camera_solutions"]
            },
            "rig_config_digests": {
                row["id"]: row["config_digest"] for row in tables["visual_geometry_rigs"]
            },
            "render_configuration_sha256s": {
                row["id"]: hashlib.sha256(canonical_json(row["config_json"])).hexdigest()
                for row in tables["render_runs"]
            },
            "lighting_state": "CAPTURED_IN_FIXED_RIG_OR_RENDER_CONFIGURATION",
        }

    def _captured_rows_still_match(self, table: str, expected_rows: list[dict[str, Any]]) -> bool:
        if not expected_rows:
            return True
        ids = [str(row["id"]) for row in expected_rows]
        with self.project.connection() as connection:
            actual_rows = self._rows_by_ids(connection, table, ids)
        return canonical_json(actual_rows) == canonical_json(expected_rows)

    def _artifact_valid(self, digest: str) -> bool:
        try:
            path = self.artifacts.path_for(digest)
            return path.is_file() and sha256_file(path)[0] == digest
        except (OSError, TypeError, ValueError):
            return False

    def _get(self, baseline_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_baseline_freezes WHERE id=?", (baseline_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown visual baseline freeze: {baseline_id}")
        return self._normalize(row)

    @staticmethod
    def _normalize(raw: Any) -> dict[str, Any]:
        value = dict(raw)
        value["snapshot"] = json.loads(value.pop("snapshot_json"))
        return value
