from __future__ import annotations

import hashlib
import json
import math
import uuid
from enum import StrEnum
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import EvidenceClass
from blender_vision.core.util import (
    atomic_write_json,
    canonical_json,
    sha256_file,
    utc_now,
)
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore


class MeasurementType(StrEnum):
    KNOWN_OVERALL_DIMENSION = "known_overall_dimension"
    POINT = "point"
    LINE = "line"
    ANGLE = "angle"
    CIRCLE = "circle"
    ELLIPSE = "ellipse"
    BOUNDING_BOX = "bounding_box"
    SURFACE_BOUNDARY = "surface_boundary"
    SYMMETRY_AXIS = "symmetry_axis"
    ARRAY_PITCH = "array_pitch"
    DEPTH_OFFSET = "depth_offset"
    CROSS_VIEW_CORRESPONDENCE = "cross_view_correspondence"
    APRILTAG_BOARD = "apriltag_board"
    CALIBRATION_BOARD = "calibration_board"
    RULER_SCALE_BAR = "ruler_scale_bar"
    STRUCTURED_LIGHT_SCAN = "structured_light_scan"
    LIDAR_SCAN = "lidar_scan"
    PHOTOGRAMMETRY_TARGET = "photogrammetry_target"
    TURNTABLE_ANGLE = "turntable_angle"
    DEPTH_CAMERA = "depth_camera"
    MANUAL_CALIPER = "manual_caliper"
    MANUFACTURER_DRAWING = "manufacturer_drawing"


class MeasurementCertainty(StrEnum):
    EXACT = "exact"
    BOUNDED = "bounded"
    ESTIMATED = "estimated"
    DERIVED = "derived"


class MeasurementStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def add(
        self,
        measurement_type: str | MeasurementType,
        value: dict[str, Any],
        *,
        evidence_class: EvidenceClass,
        uncertainty: dict[str, Any] | None = None,
        qualifier: str | MeasurementCertainty | None = None,
        certainty: str | MeasurementCertainty | None = None,
        reference_ids: list[str] | None = None,
        coordinate_frame: str = "canonical_mm",
    ) -> dict[str, Any]:
        measurement_type = MeasurementType(measurement_type)
        selected_certainty = MeasurementCertainty(certainty or qualifier or "estimated")
        if measurement_type == MeasurementType.KNOWN_OVERALL_DIMENSION:
            if value.get("axis") not in {"x", "y", "z"}:
                raise ValueError("known_overall_dimension requires axis x, y, or z")
            if not isinstance(value.get("millimetres"), (int, float)) or value["millimetres"] <= 0:
                raise ValueError("known_overall_dimension requires positive millimetres")
        reference_ids = reference_ids or []
        if reference_ids:
            with self.project.connection() as connection:
                known_references = {
                    row[0]
                    for row in connection.execute("SELECT id FROM reference_items").fetchall()
                }
            if not set(reference_ids).issubset(known_references):
                raise ValueError("measurement references unknown evidence")
        measurement_id = str(uuid.uuid4())
        created_at = utc_now()
        stored_value = {
            **value,
            "qualifier": selected_certainty.value,
            "certainty": selected_certainty.value,
            "reference_ids": reference_ids,
            "coordinate_frame": coordinate_frame,
        }
        stored_uncertainty = uncertainty or {"millimetres": None, "classification": "unknown"}
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO measurements"
                "(id,type,value_json,evidence_class,uncertainty_json,created_at) "
                "VALUES(?,?,?,?,?,?)",
                (
                    measurement_id,
                    measurement_type.value,
                    json.dumps(stored_value),
                    evidence_class.value,
                    json.dumps(stored_uncertainty),
                    created_at,
                ),
            )
        return {
            "id": measurement_id,
            "type": measurement_type.value,
            "value": stored_value,
            "coordinate_frame": coordinate_frame,
            "certainty": selected_certainty.value,
            "reference_ids": reference_ids,
            "evidence_class": evidence_class.value,
            "uncertainty": stored_uncertainty,
            "created_at": created_at,
        }

    def add_physical(
        self,
        source_kind: str | MeasurementType,
        value: dict[str, Any],
        *,
        evidence_class: EvidenceClass,
        uncertainty: dict[str, Any],
        calibration_state: dict[str, Any],
        reference_ids: list[str] | None = None,
        coordinate_frame: str = "canonical_mm",
        certainty: str | MeasurementCertainty = MeasurementCertainty.BOUNDED,
    ) -> dict[str, Any]:
        """Register calibrated physical evidence without discarding device uncertainty."""
        source_kind = MeasurementType(source_kind)
        physical = {
            MeasurementType.APRILTAG_BOARD,
            MeasurementType.CALIBRATION_BOARD,
            MeasurementType.RULER_SCALE_BAR,
            MeasurementType.STRUCTURED_LIGHT_SCAN,
            MeasurementType.LIDAR_SCAN,
            MeasurementType.PHOTOGRAMMETRY_TARGET,
            MeasurementType.TURNTABLE_ANGLE,
            MeasurementType.DEPTH_CAMERA,
            MeasurementType.MANUAL_CALIPER,
            MeasurementType.MANUFACTURER_DRAWING,
        }
        if source_kind not in physical:
            raise ValueError("physical measurement requires a supported calibrated source kind")
        if not isinstance(uncertainty, dict) or not uncertainty:
            raise ValueError("physical measurement requires a non-empty uncertainty record")
        state = str(calibration_state.get("state", "")).strip().lower()
        if state not in {"calibrated", "manufacturer_calibrated", "uncalibrated", "unknown"}:
            raise ValueError("physical measurement calibration state is invalid")
        if state in {"calibrated", "manufacturer_calibrated"} and not (
            calibration_state.get("calibration_id")
            or calibration_state.get("certificate")
            or calibration_state.get("method")
        ):
            raise ValueError("calibrated physical evidence requires calibration provenance")
        return self.add(
            source_kind,
            {**value, "physical_source": source_kind.value, "calibration": calibration_state},
            evidence_class=evidence_class,
            uncertainty=uncertainty,
            certainty=certainty,
            reference_ids=reference_ids,
            coordinate_frame=coordinate_frame,
        )

    def get(self, measurement_id: str, *, verify: bool = False) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM measurements WHERE id=?", (measurement_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown measurement: {measurement_id}")
        value = json.loads(row["value_json"])
        record = {
            "id": row["id"],
            "type": row["type"],
            "value": value,
            "coordinate_frame": value.get("coordinate_frame", "canonical_mm"),
            "certainty": value.get("certainty", value.get("qualifier", "estimated")),
            "reference_ids": value.get("reference_ids", []),
            "evidence_class": row["evidence_class"],
            "uncertainty": json.loads(row["uncertainty_json"]),
            "provenance_digest": row["provenance_digest"],
            "provenance": None,
            "created_at": row["created_at"],
        }
        if verify and record["provenance_digest"]:
            path = self.artifacts.path_for(record["provenance_digest"])
            if not path.is_file() or sha256_file(path)[0] != record["provenance_digest"]:
                raise ValueError("measurement provenance receipt artifact is missing or corrupt")
            receipt = json.loads(path.read_text(encoding="utf-8"))
            self._verify_provenance_semantics(record, receipt)
            record["provenance"] = receipt
        return record

    def list(self, *, verify: bool = False) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id FROM measurements ORDER BY created_at,id"
            ).fetchall()
        return [self.get(row["id"], verify=verify) for row in rows]

    def bind_source_provenance(
        self,
        measurement_id: str,
        *,
        source_id: str,
        claim_locator: str,
    ) -> dict[str, Any]:
        """Bind a manufacturer claim to governed source provenance without reviewing its value."""
        if not claim_locator.strip():
            raise ValueError("measurement provenance requires a claim locator")
        measurement = self.get(measurement_id)
        if measurement["evidence_class"] != EvidenceClass.MANUFACTURER_SPEC.value:
            raise ValueError("source provenance binding requires a manufacturer specification")
        if measurement["provenance_digest"]:
            raise ValueError("measurement already has a provenance receipt")
        acquisition = EvidenceAcquisitionStore(self.project)
        source = acquisition.get(source_id)
        try:
            target = TargetResolver(self.project).get()
        except FileNotFoundError as error:
            raise ValueError("measurement provenance requires a canonical target") from error
        if source["target_id"] != target["id"]:
            raise ValueError("measurement provenance source is not bound to the current target")
        access = source["source"].get("access_policy", {})
        accepted_reviews = {"approved", "not_applicable", "user_owned"}
        if (
            source["source"].get("authority_class") != "manufacturer_authoritative"
            or not source.get("reviewed_by")
            or not source.get("reviewed_at")
            or source["rights"].get("internal_use") is not True
            or access.get("robots_respected") is not True
            or access.get("source_terms_review") not in accepted_reviews
            or access.get("privacy_review") not in accepted_reviews
        ):
            raise ValueError("measurement provenance requires a governed authoritative source")
        content_hash = str(source["source"].get("content_hash", "")).strip()
        if source.get("status") != "ACQUIRED" or not source.get("reference_id") or not content_hash:
            raise ValueError("measurement provenance requires acquired authoritative source bytes")
        if not acquisition.authority_status(source_id)["acquisition_valid"]:
            raise ValueError(
                "measurement provenance requires receipt-verified authoritative source bytes"
            )
        with self.project.connection() as connection:
            source_reference = connection.execute(
                "SELECT artifact_digest FROM reference_items WHERE id=?",
                (source["reference_id"],),
            ).fetchone()
        if source_reference is None or source_reference["artifact_digest"] != content_hash:
            raise ValueError("measurement provenance source bytes disagree with its reference")
        try:
            content_path = self.artifacts.path_for(content_hash)
            if sha256_file(content_path)[0] != content_hash:
                raise ValueError("measurement provenance source artifact is corrupt")
        except (KeyError, OSError) as error:
            raise ValueError("measurement provenance source artifact is unavailable") from error
        origin = str(source["source"].get("origin", "")).strip()
        retrieved_at = str(source["source"].get("retrieval_timestamp", "")).strip()
        if not origin or not retrieved_at:
            raise ValueError("measurement provenance source lacks origin or retrieval time")
        declared_sources = {
            str(measurement["value"].get(key, "")).strip()
            for key in ("source_url", "source")
            if str(measurement["value"].get(key, "")).strip()
        }
        if len(declared_sources) > 1 or (
            declared_sources and declared_sources != {origin}
        ):
            raise ValueError("measurement source URL disagrees with the governed source")

        receipt_id = str(uuid.uuid4())
        now = utc_now()
        source_snapshot = self._source_snapshot(source, schema_version=2)
        receipt = {
            "schema_version": 2,
            "receipt_type": "measurement_source_provenance",
            "id": receipt_id,
            "measurement_id": measurement_id,
            "numeric_claim": self._numeric_claim(measurement),
            "source_id": source_id,
            "source_snapshot": source_snapshot,
            "source_snapshot_sha256": hashlib.sha256(
                canonical_json(source_snapshot)
            ).hexdigest(),
            "claim_locator": claim_locator.strip(),
            "retrieved_at": retrieved_at,
            "authority": "PROVENANCE_LINK_ONLY_NO_NUMERIC_REVIEW",
            "numeric_value_changed": False,
            "created_at": now,
        }
        relative = Path("receipts") / f"measurement-provenance-{receipt_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.measurement-provenance+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            updated = connection.execute(
                "UPDATE measurements SET provenance_digest=? "
                "WHERE id=? AND provenance_digest IS NULL AND value_json=? "
                "AND uncertainty_json=? AND evidence_class=?",
                (
                    artifact.digest,
                    measurement_id,
                    json.dumps(measurement["value"]),
                    json.dumps(measurement["uncertainty"]),
                    measurement["evidence_class"],
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("measurement provenance raced with another update")
        return {**receipt, "provenance_digest": artifact.digest}

    @staticmethod
    def _numeric_claim(measurement: dict[str, Any]) -> dict[str, Any]:
        value = measurement["value"]
        return {
            "measurement_type": measurement["type"],
            "evidence_class": measurement["evidence_class"],
            "axis": value.get("axis"),
            "millimetres": value.get("millimetres"),
            "scope": value.get("scope"),
            "certainty": measurement["certainty"],
            "coordinate_frame": measurement["coordinate_frame"],
            "uncertainty": measurement["uncertainty"],
        }

    @staticmethod
    def _source_snapshot(
        source: dict[str, Any], *, schema_version: int = 1
    ) -> dict[str, Any]:
        source_record = source["source"]
        access = source_record.get("access_policy", {})
        snapshot = {
            "target_id": source["target_id"],
            "origin": source_record.get("origin"),
            "publisher": source_record.get("publisher"),
            "page_title": source_record.get("page_title"),
            "authority_class": source_record.get("authority_class"),
            "target_variant": source_record.get("target_variant"),
            "retrieval_timestamp": source_record.get("retrieval_timestamp"),
            "access_governance": {
                "robots_respected": access.get("robots_respected"),
                "authentication_boundary": access.get("authentication_boundary"),
                "source_terms_review": access.get("source_terms_review"),
                "privacy_review": access.get("privacy_review"),
                "reviewed_by": access.get("reviewed_by"),
                "reviewed_at": access.get("reviewed_at"),
            },
            "rights": source["rights"],
            "rights_reviewed_by": source.get("reviewed_by"),
            "rights_reviewed_at": source.get("reviewed_at"),
        }
        if access.get("reviewer_type") is not None:
            snapshot["access_governance"]["reviewer_type"] = access.get("reviewer_type")
        if access.get("review_basis") is not None:
            snapshot["access_governance"]["review_basis"] = access.get("review_basis")
        if schema_version >= 2:
            snapshot["acquisition"] = {
                "status": source.get("status"),
                "reference_id": source.get("reference_id"),
                "content_hash": source_record.get("content_hash"),
                "media_hash": source_record.get("media_hash"),
                "retrieval": source_record.get("retrieval"),
            }
        return snapshot

    def _verify_provenance_semantics(
        self, measurement: dict[str, Any], receipt: dict[str, Any]
    ) -> None:
        try:
            source = EvidenceAcquisitionStore(self.project).get(receipt.get("source_id", ""))
        except KeyError as error:
            raise ValueError("measurement provenance source no longer exists") from error
        schema_version = receipt.get("schema_version")
        if schema_version not in {1, 2}:
            raise ValueError("measurement provenance receipt uses an unsupported schema")
        source_snapshot = self._source_snapshot(source, schema_version=schema_version)
        expected_hash = hashlib.sha256(canonical_json(source_snapshot)).hexdigest()
        if (
            receipt.get("receipt_type") != "measurement_source_provenance"
            or receipt.get("measurement_id") != measurement["id"]
            or canonical_json(receipt.get("numeric_claim"))
            != canonical_json(self._numeric_claim(measurement))
            or canonical_json(receipt.get("source_snapshot"))
            != canonical_json(source_snapshot)
            or receipt.get("source_snapshot_sha256") != expected_hash
            or receipt.get("retrieved_at")
            != source["source"].get("retrieval_timestamp")
            or not str(receipt.get("claim_locator", "")).strip()
            or receipt.get("authority") != "PROVENANCE_LINK_ONLY_NO_NUMERIC_REVIEW"
            or receipt.get("numeric_value_changed") is not False
        ):
            raise ValueError("measurement provenance receipt is semantically invalid")

    def correct(
        self,
        measurement_id: str,
        value: dict[str, Any],
        *,
        uncertainty: dict[str, Any],
        corrected_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Correct a measurement while retaining its complete prior record and named rationale."""
        if not corrected_by.strip() or not reason.strip():
            raise ValueError("measurement correction requires a named reviewer and reason")
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM measurements WHERE id=?", (measurement_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown measurement: {measurement_id}")
        previous = json.loads(row["value_json"])
        previous_uncertainty = json.loads(row["uncertainty_json"])
        replacement = {
            **value,
            "qualifier": previous.get("qualifier", "estimated"),
            "certainty": previous.get("certainty", previous.get("qualifier", "estimated")),
            "reference_ids": previous.get("reference_ids", []),
            "coordinate_frame": previous.get("coordinate_frame", "canonical_mm"),
            "correction_history": [
                *previous.get("correction_history", []),
                {
                    "prior_value": {
                        key: item
                        for key, item in previous.items()
                        if key != "correction_history"
                    },
                    "prior_uncertainty": previous_uncertainty,
                    "corrected_by": corrected_by.strip(),
                    "reason": reason.strip(),
                    "corrected_at": utc_now(),
                },
            ],
        }
        if row["type"] == MeasurementType.KNOWN_OVERALL_DIMENSION.value:
            if replacement.get("axis") not in {"x", "y", "z"}:
                raise ValueError("known_overall_dimension requires axis x, y, or z")
            if (
                not isinstance(replacement.get("millimetres"), (int, float))
                or replacement["millimetres"] <= 0
            ):
                raise ValueError("known_overall_dimension requires positive millimetres")
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE measurements SET value_json=?,uncertainty_json=? WHERE id=?",
                (json.dumps(replacement), json.dumps(uncertainty), measurement_id),
            )
        return next(item for item in self.list() if item["id"] == measurement_id)

    def link(
        self,
        measurement_id: str,
        reference_ids: list[str],
        *,
        linked_by: str,
        reason: str,
    ) -> dict[str, Any]:
        if not reference_ids or not linked_by.strip() or not reason.strip():
            raise ValueError("measurement linking requires references, a named actor, and reason")
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM measurements WHERE id=?", (measurement_id,)
            ).fetchone()
            known_references = {
                item[0] for item in connection.execute("SELECT id FROM reference_items").fetchall()
            }
        if row is None:
            raise KeyError(f"unknown measurement: {measurement_id}")
        if not set(reference_ids).issubset(known_references):
            raise ValueError("measurement link references unknown evidence")
        value = json.loads(row["value_json"])
        value["reference_ids"] = sorted(set(value.get("reference_ids", [])) | set(reference_ids))
        value.setdefault("link_history", []).append(
            {
                "linked_by": linked_by.strip(),
                "reason": reason.strip(),
                "reference_ids": sorted(set(reference_ids)),
                "linked_at": utc_now(),
            }
        )
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE measurements SET value_json=? WHERE id=?",
                (json.dumps(value), measurement_id),
            )
        return next(item for item in self.list() if item["id"] == measurement_id)


class MeasurementGridStore:
    """Evidence-bound perspective grids used for rulers, calibration, and camera initialization."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def create(
        self,
        reference_id: str,
        definition: dict[str, Any],
        *,
        created_by: str,
        uncertainty: dict[str, Any] | None = None,
        scale_measurement_id: str | None = None,
    ) -> dict[str, Any]:
        if not created_by.strip():
            raise ValueError("measurement grid requires a named creator")
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT metadata_json FROM reference_items WHERE id=? "
                "AND media_type LIKE 'image/%'",
                (reference_id,),
            ).fetchone()
            measurement = (
                connection.execute(
                    "SELECT id FROM measurements WHERE id=?", (scale_measurement_id,)
                ).fetchone()
                if scale_measurement_id
                else None
            )
        if reference is None:
            raise ValueError("measurement grid requires an image reference")
        if scale_measurement_id and measurement is None:
            raise ValueError("grid scale measurement does not exist")
        normalized = bool(definition.get("normalized_coordinates", True))
        if not normalized:
            raise ValueError("grid definitions must use normalized image coordinates")
        vanishing_points = dict(definition.get("vanishing_points") or {})
        for axis, point in vanishing_points.items():
            if axis not in {"x", "y", "z"}:
                raise ValueError(f"unsupported grid axis: {axis}")
            self._point(point, label=f"vanishing point {axis}")
        for collection_name in ("vanishing_lines", "rulers", "symmetry_axes"):
            for index, line in enumerate(definition.get(collection_name, [])):
                if not isinstance(line, dict):
                    raise ValueError(f"{collection_name}[{index}] must be an object")
                self._point(line.get("start"), label=f"{collection_name}[{index}].start")
                self._point(line.get("end"), label=f"{collection_name}[{index}].end")
        for index, target in enumerate(definition.get("calibration_targets", [])):
            if not isinstance(target, dict) or not target.get("kind"):
                raise ValueError(f"calibration_targets[{index}] requires a kind")
        now = utc_now()
        grid_id = str(uuid.uuid4())
        metadata = json.loads(reference["metadata_json"])
        record = {
            "id": grid_id,
            "reference_id": reference_id,
            "image_size": {
                "width": int(metadata.get("width", 0)),
                "height": int(metadata.get("height", 0)),
            },
            "coordinate_space": "normalized_image",
            "definition": {
                "vanishing_points": vanishing_points,
                "vanishing_lines": list(definition.get("vanishing_lines", [])),
                "rulers": list(definition.get("rulers", [])),
                "calibration_targets": list(definition.get("calibration_targets", [])),
                "symmetry_axes": list(definition.get("symmetry_axes", [])),
                "snap": dict(definition.get("snap") or {}),
                "multi_view_links": list(definition.get("multi_view_links", [])),
            },
            "scale_measurement_id": scale_measurement_id,
            "millimetre_conversion": dict(definition.get("millimetre_conversion") or {}),
            "uncertainty": uncertainty or {"classification": "unknown"},
            "created_by": created_by.strip(),
            "created_at": now,
            "updated_at": now,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO measurement_grids(id,reference_id,record_json,created_at,updated_at) "
                "VALUES(?,?,?,?,?)",
                (grid_id, reference_id, json.dumps(record), now, now),
            )
        return record

    @staticmethod
    def _point(value: Any, *, label: str) -> tuple[float, float]:
        if not isinstance(value, (list, tuple)) or len(value) != 2:
            raise ValueError(f"{label} must contain x and y")
        point = (float(value[0]), float(value[1]))
        if not all(math.isfinite(item) for item in point):
            raise ValueError(f"{label} must be finite")
        return point

    def get(self, grid_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json FROM measurement_grids WHERE id=?", (grid_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown measurement grid: {grid_id}")
        return json.loads(row["record_json"])

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT record_json FROM measurement_grids ORDER BY created_at"
            ).fetchall()
        return [json.loads(row["record_json"]) for row in rows]
