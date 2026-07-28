from __future__ import annotations

import json
import math
import re
import statistics
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, utc_now
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore

VARIANT_FIELDS = (
    "manufacturer",
    "model",
    "generation",
    "model_year",
    "market",
    "regional_version",
    "trim",
    "body_style",
    "aero_package",
    "wheel_option",
)
BLOCKING_CATEGORIES = {
    "aftermarket_parts",
    "different_variant",
    "market_specific_components",
    "mirrored_image",
    "modified_product",
    "optional_package_mismatch",
    "partial_crop_without_scope",
    "prototype_vs_production",
    "severe_editing",
}
DECISIONS = {
    "CANONICAL_MATCH_CONFIRMED",
    "CONFIGURATION_BRANCH",
    "CONTROLLED_VIEW_VARIATION",
    "DIAGNOSTIC_ONLY",
    "EXCLUDE",
}
NEGATIVE_DECISIONS = {"CONFIGURATION_BRANCH", "DIAGNOSTIC_ONLY", "EXCLUDE"}
MODIFICATION_PATTERNS = {
    "modified_product": re.compile(
        r"\b(modified|custom(?:ized)?|tuned|restomod|conversion)\b", re.I
    ),
    "aftermarket_parts": re.compile(r"\b(aftermarket|body\s*kit|non[- ]oem|replica)\b", re.I),
    "prototype_vs_production": re.compile(
        r"\b(prototype|pre[- ]production|concept|test\s*mule|engineering\s*sample)\b", re.I
    ),
}
MIRRORED_ORIENTATIONS = {2, 4, 5, 7}


class EvidenceConflictStore:
    """Classify incompatible evidence and require explicit branch or exclusion decisions."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def audit(self, target_id: str | None = None, *, record: bool = True) -> dict[str, Any]:
        resolution = TargetResolver(self.project).get(target_id)
        sources, references, reviews = self._records(resolution["id"])
        findings = self._classify(resolution["target"], sources, references)
        latest_reviews = {
            (review["source_id"], review["category"]): review for review in reviews
        }
        eligibility = {
            source["id"]: {
                "acceptance_eligible": True,
                "coverage_eligible": source["status"] == "ACQUIRED",
                "configuration_branch_id": None,
                "reasons": [],
            }
            for source in sources
        }
        for finding in findings:
            review = latest_reviews.get((finding["source_id"], finding["category"]))
            if review and review["finding_sha256"] != self._finding_sha256(finding):
                review = None
            finding["review"] = review
            finding["status"] = "REVIEWED" if review else "UNRESOLVED"
            source_eligibility = eligibility[finding["source_id"]]
            if review and review["decision"] in NEGATIVE_DECISIONS:
                source_eligibility["acceptance_eligible"] = False
                source_eligibility["coverage_eligible"] = False
                source_eligibility["reasons"].append(review["decision"].lower())
                if review["decision"] == "CONFIGURATION_BRANCH":
                    source_eligibility["configuration_branch_id"] = review[
                        "configuration_model"
                    ]["id"]
            elif finding["blocking"] and review is None:
                source_eligibility["acceptance_eligible"] = False
                source_eligibility["coverage_eligible"] = False
                source_eligibility["reasons"].append(finding["category"])
            elif finding["category"] == "partial_crop_scoped":
                source_eligibility["coverage_eligible"] = False
                source_eligibility["reasons"].append("partial_crop_scoped")

        unresolved_blocking = [
            finding
            for finding in findings
            if finding["blocking"] and finding["status"] == "UNRESOLVED"
        ]
        unresolved_warnings = [
            finding
            for finding in findings
            if not finding["blocking"] and finding["status"] == "UNRESOLVED"
        ]
        report = {
            "schema_version": 1,
            "receipt_type": "evidence_conflict_audit",
            "target_id": resolution["id"],
            "canonical_target": resolution["target"],
            "source_count": len(sources),
            "finding_count": len(findings),
            "conflict_count": len(findings),
            "unresolved_blocking_count": len(unresolved_blocking),
            "unresolved_warning_count": len(unresolved_warnings),
            "canonical_merge_permitted": not unresolved_blocking,
            "merge_permitted": not unresolved_blocking,
            "workflow_may_continue": not unresolved_blocking,
            "findings": findings,
            "conflicts": findings,
            "source_eligibility": eligibility,
            "policy": {
                "unreviewed_blocking_sources_enter_canonical_model": False,
                "configuration_branches_are_merged": False,
                "warnings_establish_camera_or_geometry_authority": False,
            },
        }
        if not record:
            return report
        run_id = str(uuid.uuid4())
        created_at = utc_now()
        report.update(
            {
                "id": run_id,
                "status": "BLOCKED" if unresolved_blocking else "REVIEWED",
                "created_at": created_at,
            }
        )
        relative = Path("receipts") / f"evidence-conflicts-{run_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.evidence-conflicts+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO evidence_conflict_runs(id,target_id,status,report_json,"
                "artifact_digest,created_at) VALUES(?,?,?,?,?,?)",
                (
                    run_id,
                    resolution["id"],
                    report["status"],
                    json.dumps(report),
                    artifact.digest,
                    created_at,
                ),
            )
            for source_id, state in eligibility.items():
                if not state["acceptance_eligible"]:
                    connection.execute(
                        "UPDATE reference_items SET acceptance_eligible=0 "
                        "WHERE id=(SELECT reference_id FROM evidence_sources WHERE id=?)",
                        (source_id,),
                    )
        return {**report, "artifact": artifact.to_dict(), "path": str(relative)}

    def review(
        self,
        source_id: str,
        category: str,
        *,
        decision: str,
        reviewer: str,
        reason: str,
        configuration_model: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        if decision not in DECISIONS:
            raise ValueError("unsupported evidence-conflict decision")
        if not reviewer.strip() or not reason.strip():
            raise ValueError("evidence-conflict review requires a named reviewer and reason")
        with self.project.connection() as connection:
            source = connection.execute(
                "SELECT target_id FROM evidence_sources WHERE id=?", (source_id,)
            ).fetchone()
        if source is None:
            raise KeyError(f"unknown evidence source: {source_id}")
        current = self.audit(source["target_id"], record=False)
        finding = next(
            (
                item
                for item in current["findings"]
                if item["source_id"] == source_id and item["category"] == category
            ),
            None,
        )
        if finding is None:
            raise ValueError("conflict review must reference a current classified finding")
        if decision == "CONTROLLED_VIEW_VARIATION" and finding["blocking"]:
            raise ValueError("controlled-view variation cannot resolve a blocking conflict")
        if decision == "CONFIGURATION_BRANCH" and not finding["blocking"]:
            raise ValueError("configuration branches are only valid for blocking conflicts")
        model = dict(configuration_model or {})
        if decision == "CONFIGURATION_BRANCH":
            if (
                not str(model.get("id", "")).strip()
                or not str(model.get("description", "")).strip()
                or not isinstance(model.get("target_overrides"), dict)
                or not model["target_overrides"]
            ):
                raise ValueError(
                    "configuration-branch review requires id, description, and target overrides"
                )
        elif model:
            raise ValueError("configuration model is only valid for a configuration branch")
        review_id = str(uuid.uuid4())
        created_at = utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "evidence_conflict_review",
            "id": review_id,
            "target_id": source["target_id"],
            "source_id": source_id,
            "category": category,
            "finding_sha256": self._finding_sha256(finding),
            "decision": decision,
            "configuration_model": model,
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "created_at": created_at,
        }
        relative = Path("receipts") / f"evidence-conflict-review-{review_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.evidence-conflict-review+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO evidence_conflict_reviews(id,target_id,source_id,category,"
                "finding_sha256,decision,configuration_model_json,reviewer,reason,"
                "artifact_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
                (
                    review_id,
                    source["target_id"],
                    source_id,
                    category,
                    receipt["finding_sha256"],
                    decision,
                    json.dumps(model),
                    reviewer.strip(),
                    reason.strip(),
                    artifact.digest,
                    created_at,
                ),
            )
        updated = self.audit(source["target_id"], record=True)
        state = updated["source_eligibility"][source_id]
        duplicate_state = EvidenceDuplicateStore(self.project).audit(
            source["target_id"], record=False
        )["source_eligibility"].get(source_id, {})
        if state["acceptance_eligible"] and decision in {
            "CANONICAL_MATCH_CONFIRMED",
            "CONTROLLED_VIEW_VARIATION",
        } and duplicate_state.get("independent_evidence_eligible", True):
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE reference_items SET acceptance_eligible=1 "
                    "WHERE id=(SELECT reference_id FROM evidence_sources WHERE id=?) "
                    "AND evidence_role='acceptance_reference' "
                    "AND EXISTS(SELECT 1 FROM evidence_sources s "
                    "JOIN rights_ledger r ON r.source_id=s.id WHERE s.id=? "
                    "AND s.status='ACQUIRED' AND r.reviewed_by IS NOT NULL "
                    "AND r.reviewed_at IS NOT NULL "
                    "AND json_extract(r.rights_json,'$.internal_use')=1 "
                    "AND json_extract(s.source_json,'$.access_policy.source_terms_review') "
                    "IN ('approved','not_applicable','user_owned') "
                    "AND json_extract(s.source_json,'$.access_policy.privacy_review') "
                    "IN ('approved','not_applicable','user_owned'))",
                    (source_id, source_id),
                )
        return {
            **receipt,
            "artifact": artifact.to_dict(),
            "path": str(relative),
            "updated_conflict_audit": updated,
        }

    def _records(
        self, target_id: str
    ) -> tuple[list[dict[str, Any]], dict[str, dict[str, Any]], list[dict[str, Any]]]:
        with self.project.connection() as connection:
            sources = [
                {
                    **dict(row),
                    "source": json.loads(row["source_json"]),
                }
                for row in connection.execute(
                    "SELECT * FROM evidence_sources WHERE target_id=? ORDER BY created_at,id",
                    (target_id,),
                )
            ]
            references = {
                row["id"]: {
                    **dict(row),
                    "metadata": json.loads(row["metadata_json"]),
                    "quality": json.loads(row["quality_json"]),
                }
                for row in connection.execute("SELECT * FROM reference_items")
            }
            reviews = [
                {
                    **dict(row),
                    "configuration_model": json.loads(row["configuration_model_json"]),
                }
                for row in connection.execute(
                    "SELECT * FROM evidence_conflict_reviews WHERE target_id=? "
                    "ORDER BY created_at,id",
                    (target_id,),
                )
            ]
        return sources, references, reviews

    def _classify(
        self,
        canonical: dict[str, Any],
        sources: list[dict[str, Any]],
        references: dict[str, dict[str, Any]],
    ) -> list[dict[str, Any]]:
        findings: list[dict[str, Any]] = []
        for source in sources:
            record = source["source"]
            variant = record.get("target_variant") or {}
            mismatches = {
                key: {"canonical": canonical.get(key), "source": variant.get(key)}
                for key in VARIANT_FIELDS
                if canonical.get(key) is not None
                and variant.get(key) is not None
                and self._normalized(canonical[key]) != self._normalized(variant[key])
            }
            if mismatches:
                category = (
                    "market_specific_components"
                    if set(mismatches) <= {"market", "regional_version"}
                    else "optional_package_mismatch"
                    if set(mismatches) <= {"aero_package", "wheel_option"}
                    else "different_variant"
                )
                self._add(
                    findings,
                    source,
                    category,
                    "BLOCKING",
                    "source target fields conflict with the canonical identity",
                    {"mismatches": mismatches},
                )
            canonical_options = set(map(self._normalized, canonical.get("factory_options", [])))
            source_options = set(map(self._normalized, variant.get("factory_options", [])))
            if source_options and canonical_options != source_options:
                self._add(
                    findings,
                    source,
                    "optional_package_mismatch",
                    "BLOCKING",
                    "source factory options differ from the canonical configuration",
                    {
                        "canonical_options": sorted(canonical_options),
                        "source_options": sorted(source_options),
                    },
                )
            text = " ".join(
                str(value)
                for value in (
                    record.get("page_title"),
                    record.get("publisher"),
                    record.get("search_provenance", {}).get("snippet"),
                    record.get("modification_state"),
                    record.get("product_state"),
                )
                if value
            )
            for category, pattern in MODIFICATION_PATTERNS.items():
                match = pattern.search(text)
                if match:
                    self._add(
                        findings,
                        source,
                        category,
                        "BLOCKING",
                        f"source metadata indicates {category.replace('_', ' ')}",
                        {"matched_text": match.group(0)},
                    )
            if record.get("aftermarket_parts"):
                self._add(
                    findings,
                    source,
                    "aftermarket_parts",
                    "BLOCKING",
                    "source explicitly records aftermarket components",
                    {"aftermarket_parts": record["aftermarket_parts"]},
                )
            if record.get("mirrored") is True or record.get("image_transform") == "mirrored":
                self._add(
                    findings,
                    source,
                    "mirrored_image",
                    "BLOCKING",
                    "source explicitly records a mirrored image",
                    {"source_flag": True},
                )
            editing = str(record.get("editing_suspicion", "unassessed")).lower()
            if editing in {"confirmed", "high", "composite", "geometry_altered"}:
                self._add(
                    findings,
                    source,
                    "severe_editing",
                    "BLOCKING",
                    "source editing may alter target geometry",
                    {"editing_suspicion": editing},
                )
            elif editing not in {"", "none", "unassessed", "low"}:
                self._add(
                    findings,
                    source,
                    "edited_promotional_image",
                    "WARNING",
                    "source editing requires visual review",
                    {"editing_suspicion": editing},
                )
            cropping = record.get("cropping") or {}
            if cropping.get("partial_object") is True:
                scoped = bool(record.get("included_evidence")) and bool(
                    record.get("excluded_evidence")
                )
                self._add(
                    findings,
                    source,
                    "partial_crop_scoped" if scoped else "partial_crop_without_scope",
                    "WARNING" if scoped else "BLOCKING",
                    (
                        "partial crop has explicit included and excluded evidence scope"
                        if scoped
                        else "partial crop lacks explicit included and excluded evidence scope"
                    ),
                    {"cropping": cropping},
                )
            distortion = str(record.get("perspective_distortion", "")).lower()
            if distortion in {"high", "severe", "fisheye", "yes", "true"}:
                self._add(
                    findings,
                    source,
                    "perspective_distortion",
                    "WARNING",
                    "source reports perspective or lens distortion",
                    {"perspective_distortion": distortion},
                )
            reference = references.get(source.get("reference_id"))
            if reference:
                orientation = int(reference["metadata"].get("orientation", 1) or 1)
                if orientation in MIRRORED_ORIENTATIONS:
                    self._add(
                        findings,
                        source,
                        "mirrored_image",
                        "BLOCKING",
                        "EXIF orientation contains a reflected transform",
                        {"exif_orientation": orientation},
                    )
                focal = self._focal_35(reference)
                if focal is not None and focal < 20.0:
                    self._add(
                        findings,
                        source,
                        "perspective_distortion",
                        "WARNING",
                        "wide-angle focal metadata increases perspective-distortion risk",
                        {"focal_length_35mm": focal},
                    )
        self._cross_source_camera_warnings(findings, sources, references)
        findings.sort(key=lambda item: (item["source_id"], item["category"]))
        return findings

    def _cross_source_camera_warnings(
        self,
        findings: list[dict[str, Any]],
        sources: list[dict[str, Any]],
        references: dict[str, dict[str, Any]],
    ) -> None:
        groups: dict[str, list[tuple[dict[str, Any], dict[str, Any], float | None]]] = {}
        for source in sources:
            reference = references.get(source.get("reference_id"))
            if not reference:
                continue
            viewpoint = self._direction(source["source"].get("viewpoint"))
            if viewpoint:
                groups.setdefault(viewpoint, []).append(
                    (source, reference, self._focal_35(reference))
                )
        for viewpoint, group in groups.items():
            focals = [item[2] for item in group if item[2] is not None and item[2] > 0.0]
            if len(focals) >= 2 and max(focals) / min(focals) > 2.5:
                median = statistics.median(focals)
                for source, _reference, focal in group:
                    if focal is not None and (focal > median * 1.6 or focal < median / 1.6):
                        self._add(
                            findings,
                            source,
                            "lens_inconsistency",
                            "WARNING",
                            f"{viewpoint} evidence has an outlying 35mm-equivalent focal length",
                            {"focal_length_35mm": focal, "group_median": median},
                        )
            aspects = [
                reference["metadata"].get("width", 0)
                / max(1, reference["metadata"].get("height", 0))
                for _source, reference, _focal in group
            ]
            if len(aspects) >= 2 and max(aspects) - min(aspects) > 0.2:
                for (source, _reference, _focal), aspect in zip(group, aspects, strict=True):
                    self._add(
                        findings,
                        source,
                        "crop_inconsistency",
                        "WARNING",
                        f"{viewpoint} evidence uses inconsistent frame aspect or crop",
                        {
                            "aspect_ratio": round(aspect, 6),
                            "group_range": [min(aspects), max(aspects)],
                        },
                    )

    @staticmethod
    def _add(
        findings: list[dict[str, Any]],
        source: dict[str, Any],
        category: str,
        severity: str,
        summary: str,
        evidence: dict[str, Any],
    ) -> None:
        existing = next(
            (
                item
                for item in findings
                if item["source_id"] == source["id"] and item["category"] == category
            ),
            None,
        )
        if existing:
            existing["evidence"].update(evidence)
            if severity == "BLOCKING":
                existing["severity"] = severity
                existing["blocking"] = True
            return
        findings.append(
            {
                "source_id": source["id"],
                "reference_id": source.get("reference_id"),
                "category": category,
                "severity": severity,
                "blocking": category in BLOCKING_CATEGORIES or severity == "BLOCKING",
                "summary": summary,
                "evidence": evidence,
            }
        )

    @staticmethod
    def _normalized(value: Any) -> str:
        if isinstance(value, list):
            return "|".join(sorted(EvidenceConflictStore._normalized(item) for item in value))
        return re.sub(r"\s+", " ", str(value).strip().lower())

    @staticmethod
    def _direction(value: Any) -> str | None:
        text = str(value or "").lower()
        return next(
            (
                direction
                for direction in ("front", "rear", "left", "right", "top", "bottom", "underbody")
                if direction in text
            ),
            None,
        )

    @staticmethod
    def _focal_35(reference: dict[str, Any]) -> float | None:
        value = reference["metadata"].get("lens", {}).get("FocalLengthIn35mmFilm")
        if value is None:
            return None
        try:
            focal = float(value)
        except (TypeError, ValueError):
            return None
        return focal if math.isfinite(focal) and focal > 0.0 else None

    @staticmethod
    def _finding_sha256(finding: dict[str, Any]) -> str:
        stable = {
            key: value
            for key, value in finding.items()
            if key not in {"review", "status"}
        }
        return __import__("hashlib").sha256(canonical_json(stable)).hexdigest()
