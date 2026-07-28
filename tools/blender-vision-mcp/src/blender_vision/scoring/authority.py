from __future__ import annotations

import json
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from blender_vision.scoring.catalog import (
    CapabilityCatalog,
    canonical_json_bytes,
    sha256_bytes,
    sha256_path,
)
from blender_vision.scoring.models import (
    CapabilityEvidence,
    CapabilityFacet,
    CapabilityReport,
    CapabilityStatus,
    FacetEvaluation,
)


def _compare_metric(actual: Any, rule: dict[str, Any]) -> bool:
    expected = rule["value"]
    operation = rule["op"]
    if operation == "==":
        return actual == expected
    if operation == "<=":
        return actual <= expected
    if operation == ">=":
        return actual >= expected
    if operation == "<":
        return actual < expected
    if operation == ">":
        return actual > expected
    raise ValueError(f"unsupported metric operation: {operation}")


class CapabilityAuthority:
    def __init__(self, authority_root: Path | None = None):
        self.catalog = CapabilityCatalog(authority_root)

    def load_evidence(self, path: Path) -> CapabilityEvidence:
        return CapabilityEvidence.model_validate_json(path.read_text(encoding="utf-8"))

    def _verify_record(self, record: Any, errors: list[str]) -> None:
        path = Path(record.artifact_path).expanduser()
        if not path.is_absolute():
            path = (self.catalog.root.parents[1] / path).resolve()
        if not path.is_file():
            errors.append(f"evidence artifact is missing: {record.artifact_path}")
            return
        actual = sha256_path(path)
        if actual != record.artifact_sha256:
            errors.append(
                f"stale or substituted artifact {record.id}: "
                f"expected {record.artifact_sha256}, observed {actual}"
            )
        if not record.executed:
            errors.append(f"unexecuted evidence cannot support a score: {record.id}")
        if record.adapter_only:
            errors.append(f"adapter presence is not successful execution: {record.id}")

    def _required_kinds(self, proposed_score: int) -> set[str]:
        registry = self.catalog.registry
        if proposed_score >= 110:
            return set(registry["levels"]["110"]["required_evidence_kinds"])
        if proposed_score >= 105:
            return set(registry["levels"]["105"]["required_evidence_kinds"])
        if proposed_score >= 100:
            return set(registry["levels"]["100"]["required_evidence_kinds"])
        return set(registry["minimum_score_increase_evidence_kinds"])

    def evaluate(
        self,
        facet: CapabilityFacet,
        evidence: CapabilityEvidence | None = None,
    ) -> FacetEvaluation:
        if evidence is None:
            return FacetEvaluation(
                facet_id=facet.id,
                domain=facet.domain,
                baseline_score=facet.baseline_score,
                requested_score=facet.current_score,
                accepted_score=facet.current_score,
                status=facet.status,
                accepted=True,
                errors=[],
                warnings=["no new evidence submitted; preserved registered score"],
                evidence_ids=list(facet.evidence),
                reproduction_commands=list(facet.reproduction_commands),
                external_blockers=[],
            )

        errors: list[str] = []
        warnings: list[str] = []
        current_head = self.catalog.git_head()
        if evidence.facet_id != facet.id:
            errors.append(
                f"evidence facet mismatch: expected {facet.id}, observed {evidence.facet_id}"
            )
        if evidence.proposed_score < facet.current_score:
            errors.append("a score receipt cannot silently lower the registered score")
        if evidence.git_head != current_head:
            errors.append(
                f"evidence Git head {evidence.git_head} does not match current head {current_head}"
            )
        if evidence.catalog_sha256 != self.catalog.catalog_sha256:
            errors.append("evidence was produced against a different facet catalog")
        if evidence.registry_sha256 != self.catalog.registry_sha256:
            errors.append("evidence was produced against different acceptance thresholds")
        if evidence.thresholds_changed_after_run:
            errors.append("acceptance thresholds changed after execution")
        if evidence.builder_identity == evidence.evaluator_identity:
            errors.append("builder and evaluator identities must be isolated")
        if evidence.evaluator_had_builder_access:
            errors.append("evaluator access by the builder process is forbidden")
        if not evidence.manual_edits_receipted:
            errors.append("hidden manual edits are outside the receipt chain")
        for severity in ("P0", "P1"):
            if evidence.unresolved_defects.get(severity, 0) != 0 and evidence.proposed_score >= 100:
                errors.append(f"{severity} defects must be zero for a 100+ score")

        for record in evidence.records:
            self._verify_record(record, errors)

        if evidence.proposed_status == "NOT_APPLICABLE":
            if not evidence.not_applicable_justification:
                errors.append("NOT_APPLICABLE requires a concrete justification")
            accepted = not errors
            return FacetEvaluation(
                facet_id=facet.id,
                domain=facet.domain,
                baseline_score=facet.baseline_score,
                requested_score=evidence.proposed_score,
                accepted_score=facet.current_score if not accepted else evidence.proposed_score,
                status="NOT_APPLICABLE" if accepted else facet.status,
                accepted=accepted,
                errors=errors,
                warnings=warnings,
                evidence_ids=[record.id for record in evidence.records],
                reproduction_commands=evidence.reproduction_commands,
                external_blockers=evidence.external_blockers,
            )

        is_increase = evidence.proposed_score > facet.current_score
        if is_increase:
            if not evidence.reproduction_commands:
                errors.append("score increases require executable reproduction commands")
            kinds = {record.kind for record in evidence.records}
            missing_kinds = self._required_kinds(evidence.proposed_score) - kinds
            if missing_kinds:
                errors.append(
                    "unsupported score increase; missing evidence kinds: "
                    + ", ".join(sorted(missing_kinds))
                )
            required_receipts = set(facet.required_receipts)
            record_ids = {record.id for record in evidence.records}
            missing_receipts = required_receipts - record_ids
            if missing_receipts:
                errors.append("missing facet receipts: " + ", ".join(sorted(missing_receipts)))

        real_records = [
            record
            for record in evidence.records
            if record.kind == "real_runtime" and record.executed and not record.adapter_only
        ]
        observed_runtimes = {record.runtime for record in real_records}
        if is_increase:
            missing_runtimes = set(facet.required_real_runtimes) - observed_runtimes
            if missing_runtimes:
                errors.append(
                    "missing required real runtimes: " + ", ".join(sorted(missing_runtimes))
                )

        if evidence.proposed_score >= 100:
            external_records = [
                record
                for record in evidence.records
                if record.kind == "external_holdout" and record.executed
            ]
            if not external_records:
                errors.append("100+ requires executed external or held-out evidence")
            for record in external_records:
                if record.fixture_only:
                    errors.append(
                        f"fixture-only evidence cannot satisfy an external requirement: {record.id}"
                    )
            expected_tests = set(facet.required_external_or_holdout_tests)
            observed_tests = {record.test_id for record in external_records}
            missing_tests = expected_tests - observed_tests
            if missing_tests:
                errors.append(
                    "missing required external/holdout tests: " + ", ".join(sorted(missing_tests))
                )
            if any(
                record.simulated_hardware
                for record in evidence.records
                if record.kind in {"real_runtime", "external_holdout"}
            ):
                errors.append("simulated hardware cannot satisfy a 100+ physical runtime claim")
            for metric_name, rule in facet.required_metrics.items():
                if metric_name not in evidence.metrics:
                    errors.append(f"missing required metric: {metric_name}")
                    continue
                if not _compare_metric(evidence.metrics[metric_name], rule):
                    errors.append(
                        f"metric {metric_name} failed: observed "
                        f"{evidence.metrics[metric_name]!r}, required {rule}"
                    )

        if evidence.proposed_score >= 105 and len(set(evidence.target_variants)) < 3:
            errors.append("105 requires at least three unseen target variants")
        if evidence.proposed_score >= 110:
            if evidence.metrics.get("global_regressions") != 0:
                errors.append("110 requires zero global regressions")
            required_drills = {
                "adversarial_recovery",
                "bounded_repair",
                "global_regression",
            }
            observed_drills = {record.kind for record in evidence.records}
            missing_drills = required_drills - observed_drills
            if missing_drills:
                errors.append(
                    "110 requires adversarial recovery and bounded repair evidence: "
                    + ", ".join(sorted(missing_drills))
                )

        if evidence.external_blockers and evidence.proposed_score >= 100:
            errors.append("unresolved external blockers are incompatible with a 100+ score")

        accepted = not errors
        accepted_score = evidence.proposed_score if accepted else facet.current_score
        status: CapabilityStatus
        if accepted_score >= 100:
            status = "PROVEN_100_PLUS"
        elif evidence.external_blockers:
            status = "BLOCKED_EXTERNAL"
        else:
            status = "PROVEN_BELOW_100"
        return FacetEvaluation(
            facet_id=facet.id,
            domain=facet.domain,
            baseline_score=facet.baseline_score,
            requested_score=evidence.proposed_score,
            accepted_score=accepted_score,
            status=status,
            accepted=accepted,
            errors=errors,
            warnings=warnings,
            evidence_ids=[record.id for record in evidence.records],
            reproduction_commands=evidence.reproduction_commands,
            external_blockers=evidence.external_blockers,
        )

    def evaluate_selector(
        self,
        selector: str,
        evidence_by_facet: dict[str, CapabilityEvidence] | None = None,
    ) -> list[FacetEvaluation]:
        evidence_by_facet = evidence_by_facet or {}
        return [
            self.evaluate(facet, evidence_by_facet.get(facet.id))
            for facet in self.catalog.select(selector)
        ]

    def _report_payload(
        self,
        evaluations: list[FacetEvaluation],
        evidence_sources: list[dict[str, str]],
    ) -> dict[str, Any]:
        scores = [item.accepted_score for item in evaluations]
        by_status: dict[str, int] = {}
        for item in evaluations:
            by_status[item.status] = by_status.get(item.status, 0) + 1
        return {
            "schema_version": "1",
            "generated_at": datetime.now(UTC).isoformat(),
            "git_head": self.catalog.git_head(),
            "catalog_sha256": self.catalog.catalog_sha256,
            "registry_sha256": self.catalog.registry_sha256,
            "evaluations": [item.model_dump(mode="json") for item in evaluations],
            "evidence_sources": evidence_sources,
            "summary": {
                "facet_count": len(evaluations),
                "minimum_score": min(scores) if scores else None,
                "maximum_score": max(scores) if scores else None,
                "mean_score": sum(scores) / len(scores) if scores else None,
                "facets_at_100_plus": sum(score >= 100 for score in scores),
                "facets_below_100": sum(score < 100 for score in scores),
                "rejected_evaluations": sum(not item.accepted for item in evaluations),
                "status_counts": dict(sorted(by_status.items())),
            },
        }

    def report(
        self,
        evidence_paths: list[Path] | None = None,
        *,
        output_path: Path | None = None,
    ) -> CapabilityReport:
        evidence_by_facet: dict[str, CapabilityEvidence] = {}
        evidence_sources: list[dict[str, str]] = []
        for path in evidence_paths or []:
            resolved = path.expanduser().resolve()
            evidence = self.load_evidence(resolved)
            if evidence.facet_id in evidence_by_facet:
                raise ValueError(f"duplicate evidence bundle for facet {evidence.facet_id}")
            evidence_by_facet[evidence.facet_id] = evidence
            evidence_sources.append({"path": str(resolved), "sha256": sha256_path(resolved)})
        evaluations = self.evaluate_selector("all", evidence_by_facet)
        payload = self._report_payload(evaluations, evidence_sources)
        payload["report_sha256"] = sha256_bytes(canonical_json_bytes(payload))
        report = CapabilityReport.model_validate(payload)
        if output_path:
            output = output_path.expanduser().resolve()
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(
                json.dumps(report.model_dump(mode="json"), indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
        return report

    def verify_report(self, path: Path) -> dict[str, Any]:
        resolved = path.expanduser().resolve()
        report = CapabilityReport.model_validate_json(resolved.read_text(encoding="utf-8"))
        payload = report.model_dump(mode="json")
        claimed_digest = payload.pop("report_sha256")
        observed_digest = sha256_bytes(canonical_json_bytes(payload))
        errors: list[str] = []
        evidence_by_facet: dict[str, CapabilityEvidence] = {}
        if claimed_digest != observed_digest:
            errors.append("report digest does not match canonical report content")
        if report.git_head != self.catalog.git_head():
            errors.append("report Git head does not match current checkout")
        if report.catalog_sha256 != self.catalog.catalog_sha256:
            errors.append("report facet catalog is stale or substituted")
        if report.registry_sha256 != self.catalog.registry_sha256:
            errors.append("report acceptance registry is stale or substituted")
        for source in report.evidence_sources:
            source_path = Path(source["path"])
            if not source_path.is_file():
                errors.append(f"evidence source is missing: {source_path}")
                continue
            if sha256_path(source_path) != source["sha256"]:
                errors.append(f"evidence source is stale or substituted: {source_path}")
                continue
            try:
                evidence = self.load_evidence(source_path)
            except (ValueError, json.JSONDecodeError) as error:
                errors.append(f"evidence source is invalid: {source_path}: {error}")
                continue
            if evidence.facet_id in evidence_by_facet:
                errors.append(f"duplicate evidence bundle for facet {evidence.facet_id}")
                continue
            evidence_by_facet[evidence.facet_id] = evidence
        expected_evaluations = self.evaluate_selector("all", evidence_by_facet)
        observed_evaluations = [
            evaluation.model_dump(mode="json") for evaluation in report.evaluations
        ]
        if [
            evaluation.model_dump(mode="json") for evaluation in expected_evaluations
        ] != observed_evaluations:
            errors.append("report evaluations do not reproduce from registered evidence")
        expected_summary = self._report_payload(report.evaluations, report.evidence_sources)[
            "summary"
        ]
        if expected_summary != report.summary:
            errors.append("report summary does not match its facet evaluations")
        return {
            "valid": not errors,
            "path": str(resolved),
            "report_sha256": claimed_digest,
            "errors": errors,
            "facet_count": len(report.evaluations),
            "facets_at_100_plus": report.summary["facets_at_100_plus"],
            "facets_below_100": report.summary["facets_below_100"],
        }
