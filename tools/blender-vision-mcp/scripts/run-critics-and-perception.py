#!/usr/bin/env python3
"""Real execution: catch matrix, false-positive check, repair, next-best-view loop."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT / "src") not in sys.path:
    sys.path.insert(0, str(ROOT / "src"))

from blender_vision.active_perception import (  # noqa: E402
    NextBestViewPlanner,
    PerceptionTarget,
    StopReason,
    SurfaceCell,
    quantify_uncertainty,
)
from blender_vision.core.util import atomic_write_json  # noqa: E402
from blender_vision.critics import (  # noqa: E402
    ALL_ROLES,
    BoundedRepairRunner,
    CriticRole,
    CriticWorkspace,
    default_critics,
    plan_from_finding,
)
from blender_vision.critics.fixtures import (  # noqa: E402
    evidence_for,
    load_control_subject,
    load_fault_subject,
)
from blender_vision.v2.authority import AuthorityClass  # noqa: E402
from blender_vision.v2.validation import write_record  # noqa: E402


def _partial_target() -> PerceptionTarget:
    return PerceptionTarget(
        target_id="consumer-object-partial",
        cells=[
            SurfaceCell(
                region="front",
                area_m2=1.0,
                covered=True,
                occlusion_fraction=0.0,
                resolution_px=1800,
                candidate_predictions=[0.2, 0.22],
            ),
            SurfaceCell(
                region="underside",
                area_m2=1.2,
                covered=False,
                candidate_predictions=[0.0, 0.9, 0.85],
            ),
            SurfaceCell(
                region="left",
                area_m2=0.9,
                covered=False,
                candidate_predictions=[0.1, 0.7],
            ),
            SurfaceCell(
                region="right",
                area_m2=0.9,
                covered=True,
                occlusion_fraction=0.0,
                resolution_px=1600,
            ),
            SurfaceCell(
                region="top",
                area_m2=0.7,
                covered=True,
                occlusion_fraction=0.1,
                resolution_px=1400,
            ),
        ],
        scale_authority=AuthorityClass.UNRESOLVED,
        material_confidences=[0.45, 0.4, 0.3],
        has_scale_reference=False,
        has_diffuse_light_view=False,
        has_grazing_light_view=False,
        has_lens_metadata=False,
        has_calibration_target=False,
    )


def _full_target() -> PerceptionTarget:
    return PerceptionTarget(
        target_id="consumer-object-full",
        cells=[
            SurfaceCell(
                region=name,
                area_m2=1.0,
                covered=True,
                occlusion_fraction=0.0,
                resolution_px=2000,
                candidate_predictions=[0.5, 0.5],
            )
            for name in ("front", "rear", "left", "right", "top", "underside")
        ],
        scale_authority=AuthorityClass.MEASURED,
        material_confidences=[0.97],
        has_scale_reference=True,
        has_diffuse_light_view=True,
        has_grazing_light_view=True,
        has_lens_metadata=True,
        has_calibration_target=True,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "v2" / "critics",
    )
    args = parser.parse_args()
    out: Path = args.output
    out.mkdir(parents=True, exist_ok=True)

    print("=== VisionMCP V2 critics + active perception demo ===\n")

    # 1) Catch matrix
    print("## Catch matrix (critic x fault fixture)")
    print(f"{'critic':36} {'result':8} measured_trigger")
    print("-" * 100)
    misses = 0
    matrix = []
    for role in ALL_ROLES:
        critic = next(c for c in default_critics() if c.role is role)
        subject = load_fault_subject(role)
        findings = critic.critique(subject, evidence_for(subject.subject_id))
        caught = bool(findings)
        if not caught:
            misses += 1
        trigger = {}
        if findings:
            trigger = dict(findings[0].measured)
        status = "caught" if caught else "MISSED"
        print(f"{role.value:36} {status:8} {trigger}")
        matrix.append(
            {
                "critic": role.value,
                "fixture": subject.subject_id,
                "result": status,
                "finding_count": len(findings),
                "measured": trigger,
                "diagnoses": [f.diagnosis for f in findings],
            }
        )
    print()

    # 2) False-positive check
    print("## False-positive check (control fixture)")
    false_positives = 0
    fp_details = []
    for role in ALL_ROLES:
        critic = next(c for c in default_critics() if c.role is role)
        subject = load_control_subject(role)
        findings = critic.critique(subject, evidence_for(subject.subject_id))
        if findings:
            false_positives += len(findings)
            fp_details.append({"critic": role.value, "findings": [f.finding_id for f in findings]})
            print(f"  FALSE POSITIVE {role.value}: {[f.finding_id for f in findings]}")
    print(f"false_positive_count = {false_positives}")
    print()

    # 3) Bounded repair on at least three findings
    print("## Bounded repair (three findings)")
    workspace = CriticWorkspace()
    repair_runner = BoundedRepairRunner(workspace)
    repair_roles = [
        CriticRole.MATERIAL_ARTIST,
        CriticRole.CINEMATOGRAPHER,
        CriticRole.LIGHTING_ARTIST,
    ]
    repair_reports = []
    control = load_control_subject(CriticRole.PRODUCT_PHOTOGRAPHER)
    for role in repair_roles:
        subject = load_fault_subject(role)
        evidence = evidence_for(subject.subject_id)
        before = workspace.run(subject, evidence)
        finding = before.blocking_findings()[0]
        plan = plan_from_finding(finding)
        result = repair_runner.apply(
            subject,
            evidence,
            plan,
            baseline=before,
            unrelated_subjects=[(control, evidence_for(control.subject_id))],
        )
        write_record(out / f"repair-before-{role.value}.json", result.before)
        write_record(out / f"repair-after-{role.value}.json", result.after)
        line = (
            f"  {role.value}: finding={finding.finding_id} "
            f"acceptance={'PASS' if result.acceptance_passed else 'FAIL'} "
            f"global_regression={result.global_regression}"
        )
        print(line)
        repair_reports.append(
            {
                "role": role.value,
                "finding_id": finding.finding_id,
                "acceptance_passed": result.acceptance_passed,
                "global_regression": result.global_regression,
                "notes": result.notes,
            }
        )
    print()

    # 4) Next-best-view planner loop
    print("## Next-best-view planner (partial coverage)")
    planner = NextBestViewPlanner()
    target = _partial_target()
    u_before = quantify_uncertainty(target)
    print(f"uncertainty_before = {u_before.total:.6f}")
    print(f"components = {json.dumps(u_before.components, sort_keys=True)}")
    plan = planner.plan(target)
    print(f"stop_reason = {plan.stop_reason.value}")
    print("ordered requests:")
    for index, request in enumerate(plan.requests):
        write_record(out / f"next-view-{index:02d}.json", request)
        print(
            f"  [{index}] priority={request.priority} gain={request.expected_reduction:.4f} "
            f"id={request.id} missing={request.missing_uncertainty}"
        )
        print(f"       reason={request.reason}")
        print(f"       human={request.human_instructions[:90]}...")

    if not plan.requests:
        print("ERROR: expected requests for partial target", file=sys.stderr)
        return 1

    top = plan.requests[0]
    print(f"\nsatisfying top request: {top.id}")
    planner.satisfy(target, top, observation_id=f"obs-{top.id}")
    u_after = quantify_uncertainty(target)
    print(f"uncertainty_after = {u_after.total:.6f}")
    print(f"uncertainty_delta = {u_before.total - u_after.total:.6f}")
    plan2 = planner.plan(target)
    still = [r.id for r in plan2.requests if r.id == top.id]
    print(f"top request still present after satisfy: {bool(still)}")
    print(f"remaining_request_count = {len(plan2.requests)}")
    print()

    # 5) Redundancy suppression
    print("## Redundancy suppression (fully covered target)")
    full = _full_target()
    full_plan = planner.plan(full)
    print(f"full_coverage_request_count = {len(full_plan.requests)}")
    print(f"full_coverage_stop_reason = {full_plan.stop_reason.value}")
    print()

    # Summary gates
    repair_ok = all(r["acceptance_passed"] and not r["global_regression"] for r in repair_reports)
    uncertainty_dropped = u_after.total < u_before.total
    top_gone = not still
    full_ok = (
        len(full_plan.requests) == 0 and full_plan.stop_reason is StopReason.GAIN_TOO_LOW
    )
    success = (
        misses == 0
        and false_positives == 0
        and repair_ok
        and uncertainty_dropped
        and top_gone
        and full_ok
    )

    summary = {
        "misses": misses,
        "false_positive_count": false_positives,
        "catch_matrix": matrix,
        "false_positive_details": fp_details,
        "repairs": repair_reports,
        "uncertainty_before": u_before.total,
        "uncertainty_after": u_after.total,
        "top_request_id": top.id,
        "top_request_resubmitted": bool(still),
        "full_coverage_requests": len(full_plan.requests),
        "full_coverage_stop_reason": full_plan.stop_reason.value,
        "success": success,
    }
    atomic_write_json(out / "summary.json", summary)
    print("## SUMMARY")
    print(json.dumps(summary, indent=2, sort_keys=True))
    print(f"\nartifacts written under {out}")
    return 0 if success else 1


if __name__ == "__main__":
    raise SystemExit(main())
