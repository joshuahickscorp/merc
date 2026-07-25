from __future__ import annotations

import hashlib
import json
import math
import re
import uuid
from collections import defaultdict
from pathlib import Path
from statistics import mean, pstdev
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore


class ManufacturedFormAuditor:
    """Detect objective mesh failures and manufactured-form review risks."""

    SCHEMA_VERSION = 1
    AUDIT_ENGINE_V1 = "manufactured_form_v1"
    AUDIT_ENGINE_V2 = "manufactured_form_v2_semantic_repetition"
    REPEATED_TOKENS_V1 = ("vent", "grille", "hole", "port", "screw", "foot", "slot")
    REPEATED_TOKENS_V2 = ("vent", "grille", "hole", "screw", "foot", "slot")
    DETAIL_TOKENS = ("port", "socket", "button", "led", "vent", "grille", "slot")

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def audit(self, scene_id: str) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        inventory = scene.get("inventory")
        if not isinstance(inventory, dict):
            raise ValueError("manufactured-form audit requires a stored Blender inventory")
        report = self._analyze(scene, inventory, engine=self.AUDIT_ENGINE_V2)
        audit_id = str(uuid.uuid4())
        created_at = utc_now()
        report.update(
            {
                "schema_version": self.SCHEMA_VERSION,
                "audit_engine": self.AUDIT_ENGINE_V2,
                "id": audit_id,
                "scene_id": scene_id,
                "scene_artifact_digest": scene["artifact_digest"],
                "inventory_sha256": hashlib.sha256(canonical_json(inventory)).hexdigest(),
                "created_at": created_at,
            }
        )
        receipt = self._receipt(report)
        relative = Path("receipts") / f"manufactured-form-audit-{audit_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.manufactured-form-audit+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO manufactured_form_audits"
                "(id,scene_id,status,report_json,receipt_digest,created_at) "
                "VALUES(?,?,?,?,?,?)",
                (
                    audit_id,
                    scene_id,
                    report["status"],
                    json.dumps(report),
                    artifact.digest,
                    created_at,
                ),
            )
        return {**report, "receipt": receipt, "receipt_digest": artifact.digest}

    def list(self, scene_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM manufactured_form_audits "
                + ("WHERE scene_id=? " if scene_id else "")
                + "ORDER BY created_at,id",
                (scene_id,) if scene_id else (),
            ).fetchall()
        return [self._normalize(row) for row in rows]

    def verify(self, audit_id: str) -> dict[str, Any]:
        try:
            row = self._get(audit_id)
            digest = row["receipt_digest"]
            path = self.artifacts.path_for(digest)
            if not path.is_file() or sha256_file(path)[0] != digest:
                return {"valid": False, "replay_valid": False}
            receipt = json.loads(path.read_text(encoding="utf-8"))
            scene = SceneStore(self.project).get(row["scene_id"])
            inventory = scene.get("inventory")
            if not isinstance(inventory, dict):
                return {"valid": False, "replay_valid": False}
            report = row["report"]
            engine = report.get("audit_engine", self.AUDIT_ENGINE_V1)
            replayed = self._analyze(scene, inventory, engine=engine)
            replay_valid = bool(
                report["scene_artifact_digest"] == scene["artifact_digest"]
                and report["inventory_sha256"]
                == hashlib.sha256(canonical_json(inventory)).hexdigest()
                and canonical_json(replayed)
                == canonical_json(
                    {
                        key: value
                        for key, value in report.items()
                        if key
                        not in {
                            "schema_version",
                            "audit_engine",
                            "id",
                            "scene_id",
                            "scene_artifact_digest",
                            "inventory_sha256",
                            "created_at",
                        }
                    }
                )
            )
            receipt_valid = canonical_json(receipt) == canonical_json(self._receipt(report))
            return {
                "valid": bool(receipt_valid and replay_valid),
                "receipt_valid": receipt_valid,
                "replay_valid": replay_valid,
                "receipt": receipt,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "receipt_valid": False, "replay_valid": False}

    def _analyze(
        self,
        scene: dict[str, Any],
        inventory: dict[str, Any],
        *,
        engine: str,
    ) -> dict[str, Any]:
        if engine not in {self.AUDIT_ENGINE_V1, self.AUDIT_ENGINE_V2}:
            raise ValueError(f"unsupported manufactured-form audit engine: {engine}")
        objects = [
            item
            for item in inventory.get("objects", [])
            if item.get("type") == "MESH" and not item.get("hidden_render", False)
        ]
        hard_failures: list[dict[str, Any]] = []
        warnings: list[dict[str, Any]] = []
        diagnostics: list[dict[str, Any]] = []

        for obj in objects:
            name = str(obj.get("name", "<unnamed>"))
            mesh = obj.get("mesh", {})
            topology = mesh.get("topology", {}) if isinstance(mesh, dict) else {}
            normal = mesh.get("normal_diagnostics", {}) if isinstance(mesh, dict) else {}
            exact = bool(mesh.get("audit_sampling", {}).get("exact", False))
            checks = (
                ("DEGENERATE_POLYGONS", mesh.get("degenerate_polygons")),
                ("DUPLICATE_FACES", mesh.get("duplicate_faces")),
                ("NON_MANIFOLD_GEOMETRY", topology.get("non_manifold_edges")),
                ("ZERO_LENGTH_NORMALS", normal.get("zero_length_polygon_normals")),
            )
            for code, count in checks:
                if isinstance(count, int) and count > 0:
                    hard_failures.append(
                        {
                            "code": code,
                            "object": name,
                            "count": count,
                            "exact": exact,
                            "cause": "GEOMETRY" if "NORMAL" not in code else "NORMALS",
                        }
                    )
            duplicate_vertices = mesh.get("duplicate_vertex_positions")
            if isinstance(duplicate_vertices, int) and duplicate_vertices > 0:
                warnings.append(
                    {
                        "code": "COINCIDENT_VERTEX_POSITIONS",
                        "object": name,
                        "count": duplicate_vertices,
                        "exact": exact,
                        "cause": "GEOMETRY",
                    }
                )
            loose = mesh.get("loose_vertices")
            if isinstance(loose, int) and loose > 0:
                hard_failures.append(
                    {
                        "code": "LOOSE_VERTICES",
                        "object": name,
                        "count": loose,
                        "exact": True,
                        "cause": "GEOMETRY",
                    }
                )
            if normal.get("mirrored_transform"):
                hard_failures.append(
                    {
                        "code": "MIRRORED_NORMAL_ORIENTATION_RISK",
                        "object": name,
                        "count": 1,
                        "exact": True,
                        "cause": "NORMALS",
                    }
                )
            if not exact:
                warnings.append(
                    {
                        "code": "BOUNDED_MESH_SAMPLING",
                        "object": name,
                        "cause": "UNKNOWN",
                        "detail": "absence of sampled defects is not an exact all-elements claim",
                    }
                )
            self._thin_detail_check(obj, inventory, warnings)

        correspondence = inventory.get("component_correspondence", {})
        unbound = sorted(str(value) for value in correspondence.get("unbound_mesh_objects", []))
        if unbound:
            warnings.append(
                {
                    "code": "UNBOUND_RENDERED_COMPONENTS",
                    "objects": unbound,
                    "count": len(unbound),
                    "cause": "GEOMETRY",
                    "detail": "component-level residual attribution is incomplete",
                }
            )

        inventory_findings = inventory.get("audit_findings", [])
        for finding in inventory_findings:
            if finding.get("code") == "CLOSED_SOLID_VENT_OR_GRILLE":
                hard_failures.append(
                    {
                        "code": "NON_PERFORATED_VENT_OR_GRILLE",
                        "object": finding.get("object"),
                        "count": 1,
                        "exact": True,
                        "cause": "GEOMETRY",
                    }
                )

        repeated = self._repetition_checks(objects, engine=engine)
        warnings.extend(repeated["warnings"])
        diagnostics.extend(repeated["diagnostics"])
        largest = self._largest_object(objects)
        if largest is not None and not any(
            modifier.get("type") == "BEVEL" for modifier in largest.get("modifiers", [])
        ):
            warnings.append(
                {
                    "code": "ENCLOSURE_EDGE_TREATMENT_REQUIRES_GRAZING_REVIEW",
                    "object": largest.get("name"),
                    "cause": "GEOMETRY",
                    "detail": (
                        "no live bevel modifier is present; applied bevels may still exist and "
                        "must be checked in grazing-light and curvature passes"
                    ),
                }
            )

        hard_failures = self._deduplicate(hard_failures)
        warnings = self._deduplicate(warnings)
        blind_spots = [
            {
                "code": "FACE_ASPECT_AND_PLANAR_WAVINESS_NOT_EXACTLY_AUDITED",
                "required_evidence": "curvature, wireframe, and grazing-light review",
            },
            {
                "code": "FLOATING_OR_INTERSECTING_PARTS_NOT_EXACTLY_AUDITED",
                "required_evidence": "reviewed component masks and contact/clearance checks",
            },
            {
                "code": "BEVEL_RADIUS_CONSISTENCY_NOT_METRICALLY_AUDITED",
                "required_evidence": "governed curvature reference or direct dimensions",
            },
        ]
        if hard_failures:
            status = "FAIL"
        elif warnings:
            status = "PASS_WITH_WARNINGS"
        else:
            status = "PASS"
        return {
            "status": status,
            "authority": "DETERMINISTIC_SCENE_INVENTORY_AUDIT",
            "summary": {
                "rendered_mesh_count": len(objects),
                "hard_failure_count": len(hard_failures),
                "warning_count": len(warnings),
                "blind_spot_count": len(blind_spots),
                "requires_human_visual_review": bool(warnings or blind_spots),
            },
            "hard_failures": hard_failures,
            "warnings": warnings,
            "diagnostics": diagnostics,
            "blind_spots": blind_spots,
            "manufactured_form_priors": {
                "authority": "DIAGNOSTIC_PRIORS_NOT_REFERENCE_EVIDENCE",
                "checks": [
                    "closed-manifold manufactured parts",
                    "non-degenerate detail thickness",
                    "repetition size and spacing consistency",
                    "component binding coverage",
                    "edge-treatment review readiness",
                ],
            },
            "cause_categories": [
                "GEOMETRY",
                "NORMALS",
                "MATERIAL",
                "LIGHTING",
                "CAMERA",
                "REFERENCE",
                "UNKNOWN",
            ],
            "scene_state": scene.get("state"),
        }

    def _thin_detail_check(
        self,
        obj: dict[str, Any],
        inventory: dict[str, Any],
        warnings: list[dict[str, Any]],
    ) -> None:
        name = str(obj.get("name", "")).lower()
        if not any(token in name for token in self.DETAIL_TOKENS):
            return
        bounds = obj.get("world_bounds", {})
        dimensions = bounds.get("dimensions") or self._bounds_dimensions(bounds)
        if not dimensions or len(dimensions) != 3:
            return
        scale = float(inventory.get("canonical_transform", {}).get("scale_to_millimetres", 1000.0))
        millimetres = [abs(float(value)) * scale for value in dimensions]
        if min(millimetres) < 0.05:
            warnings.append(
                {
                    "code": "NEAR_ZERO_DETAIL_THICKNESS",
                    "object": obj.get("name"),
                    "dimensions_mm": [round(value, 6) for value in millimetres],
                    "cause": "GEOMETRY",
                    "detail": "detail may be painted-on, floating, or dimensionally degenerate",
                }
            )

    def _repetition_checks(
        self, objects: list[dict[str, Any]], *, engine: str
    ) -> dict[str, Any]:
        repeated_tokens = (
            self.REPEATED_TOKENS_V1
            if engine == self.AUDIT_ENGINE_V1
            else self.REPEATED_TOKENS_V2
        )
        groups: dict[str, list[dict[str, Any]]] = defaultdict(list)
        for obj in objects:
            name = str(obj.get("name", "")).lower()
            if any(token in name for token in repeated_tokens):
                key = re.sub(r"[._ -]*\d+$", "", name)
                groups[key].append(obj)
        warnings: list[dict[str, Any]] = []
        diagnostics: list[dict[str, Any]] = []
        for key, values in sorted(groups.items()):
            if len(values) < 3:
                continue
            dimensions = [
                item.get("world_bounds", {}).get("dimensions")
                or self._bounds_dimensions(item.get("world_bounds", {}))
                for item in values
            ]
            dimensions = [value for value in dimensions if value and len(value) == 3]
            dimension_cv = [
                self._coefficient_of_variation([abs(float(item[axis])) for item in dimensions])
                for axis in range(3)
            ]
            centers = [self._center(item.get("world_bounds", {})) for item in values]
            centers = [value for value in centers if value is not None]
            spacing_cv = None
            if len(centers) >= 3:
                axis = max(
                    range(3),
                    key=lambda index: (
                        max(value[index] for value in centers)
                        - min(value[index] for value in centers)
                    ),
                )
                ordered = sorted(value[axis] for value in centers)
                spacing_cv = self._coefficient_of_variation(
                    [ordered[index + 1] - ordered[index] for index in range(len(ordered) - 1)]
                )
            diagnostic = {
                "group": key,
                "count": len(values),
                "dimension_cv": [round(value, 6) for value in dimension_cv],
                "spacing_cv": round(spacing_cv, 6) if spacing_cv is not None else None,
            }
            diagnostics.append(diagnostic)
            if max(dimension_cv, default=0.0) > 0.05 or (
                spacing_cv is not None and spacing_cv > 0.15
            ):
                warnings.append(
                    {
                        "code": "REPETITION_INCONSISTENCY",
                        **diagnostic,
                        "cause": "GEOMETRY",
                    }
                )
        return {"warnings": warnings, "diagnostics": diagnostics}

    @staticmethod
    def _coefficient_of_variation(values: list[float]) -> float:
        if len(values) < 2 or math.isclose(mean(values), 0.0, abs_tol=1e-12):
            return 0.0
        return abs(pstdev(values) / mean(values))

    @staticmethod
    def _bounds_dimensions(bounds: dict[str, Any]) -> list[float] | None:
        minimum = bounds.get("minimum")
        maximum = bounds.get("maximum")
        if not minimum or not maximum or len(minimum) != 3 or len(maximum) != 3:
            return None
        return [float(maximum[index]) - float(minimum[index]) for index in range(3)]

    @staticmethod
    def _center(bounds: dict[str, Any]) -> list[float] | None:
        minimum = bounds.get("minimum")
        maximum = bounds.get("maximum")
        if not minimum or not maximum or len(minimum) != 3 or len(maximum) != 3:
            return None
        return [(float(minimum[index]) + float(maximum[index])) / 2.0 for index in range(3)]

    @classmethod
    def _largest_object(cls, objects: list[dict[str, Any]]) -> dict[str, Any] | None:
        def volume(obj: dict[str, Any]) -> float:
            dimensions = obj.get("world_bounds", {}).get("dimensions") or cls._bounds_dimensions(
                obj.get("world_bounds", {})
            )
            if not dimensions:
                return 0.0
            return math.prod(abs(float(value)) for value in dimensions)

        return max(objects, key=volume) if objects else None

    @staticmethod
    def _deduplicate(findings: list[dict[str, Any]]) -> list[dict[str, Any]]:
        unique: dict[bytes, dict[str, Any]] = {}
        for finding in findings:
            unique[canonical_json(finding)] = finding
        return [unique[key] for key in sorted(unique)]

    def _receipt(self, report: dict[str, Any]) -> dict[str, Any]:
        return {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "manufactured_form_audit",
            "id": report["id"],
            "scene_id": report["scene_id"],
            "scene_artifact_digest": report["scene_artifact_digest"],
            "inventory_sha256": report["inventory_sha256"],
            "status": report["status"],
            "report": report,
            "report_sha256": hashlib.sha256(canonical_json(report)).hexdigest(),
            "authority": report["authority"],
            "created_at": report["created_at"],
        }

    def _get(self, audit_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM manufactured_form_audits WHERE id=?", (audit_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown manufactured-form audit: {audit_id}")
        return self._normalize(row)

    @staticmethod
    def _normalize(raw: Any) -> dict[str, Any]:
        value = dict(raw)
        value["report"] = json.loads(value.pop("report_json"))
        return value
