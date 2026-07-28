"""Build a sealed ReconstructionPortfolio from backend candidates."""

from __future__ import annotations

import uuid
from pathlib import Path
from typing import Any

from blender_vision.reconstruction.base import (
    ReconstructionBackend,
    ReconstructionInputs,
    unavailable_candidate,
)
from blender_vision.reconstruction.compare import compare_all
from blender_vision.reconstruction.fusion import FusionResult
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import (
    Lifecycle,
    Lineage,
    ReconstructionCandidate,
    ReconstructionPortfolio,
)


def build_portfolio(
    *,
    target_id: str,
    backends: list[ReconstructionBackend],
    inputs_for: dict[str, ReconstructionInputs] | ReconstructionInputs,
    fusion_results: list[FusionResult] | None = None,
    selected_candidate_id: str | None = None,
    notes: list[str] | None = None,
) -> ReconstructionPortfolio:
    """Run each backend (or record unavailable) and assemble a portfolio record.

    Coverage, topology, editability, hidden surfaces, licensing, runtime cost
    and failure modes are taken from real candidate fields — not defaults.
    """
    candidates: list[ReconstructionCandidate] = []
    input_authorities: list[str] = []
    evidence_inputs: list[str] = []

    for backend in backends:
        inputs = _inputs_for(backend.name, inputs_for)
        availability = backend.availability()
        if not availability.available:
            candidate = unavailable_candidate(
                backend=backend.name,
                reason=availability.reason,
                inputs=inputs,
            )
            candidates.append(candidate)
            continue
        try:
            candidate = backend.run(inputs)
        except Exception as error:  # noqa: BLE001 — portfolio must survive a single backend failure
            candidate = unavailable_candidate(
                backend=backend.name,
                reason=f"{type(error).__name__}: {error}",
                inputs=inputs,
            )
        # Enforce: executed=True only when the backend really claims success.
        if candidate.executed and not candidate.artifacts and not candidate.topology_state:
            candidate.executed = False
            candidate.failure_modes = [
                *candidate.failure_modes,
                "executed flag cleared: no artifacts/topology evidence",
            ]
            candidate.execution_log += " | executed cleared (no evidence)"
        candidates.append(candidate)
        input_authorities.append(candidate.authority.value)
        evidence_inputs.extend(candidate.inputs)

    comparison = compare_all(candidates)
    fusion_payload = [item.to_dict() for item in (fusion_results or [])]
    hidden_ledger: list[dict[str, Any]] = []
    for item in fusion_results or []:
        hidden_ledger.extend(item.hidden_surface_ledger)
    for candidate in candidates:
        for assumption in candidate.hidden_surface_assumptions:
            hidden_ledger.append(
                {
                    "region": f"candidate:{candidate.candidate_id}",
                    "source_candidate": candidate.candidate_id,
                    "backend": candidate.backend,
                    "assumption": assumption,
                    "authority": candidate.authority.value,
                    "executed": candidate.executed,
                }
            )

    portfolio_authority = derive(
        [AuthorityClass(a) for a in input_authorities] or [AuthorityClass.HYPOTHETICAL],
        proposed=AuthorityClass.INFERRED,
    )
    executed = [c for c in candidates if c.executed]
    uncertainty = Uncertainty(
        kind="ensemble-disagreement",
        sigma=None,
        interval=[],
        units=Units.UNITLESS,
        basis=(
            f"{len(executed)}/{len(candidates)} backends executed; "
            f"{comparison['pair_count']} pairwise comparisons"
        ),
        samples=len(executed),
    )
    portfolio = ReconstructionPortfolio(
        id=f"portfolio-{uuid.uuid4().hex[:12]}",
        target_id=target_id,
        authority=portfolio_authority,
        lifecycle=Lifecycle.CANDIDATE,
        lineage=Lineage(
            tool="blender-vision-mcp",
            tool_version="0.1.0",
            operation="reconstruction.ensemble",
            inputs=sorted(set(evidence_inputs)),
            input_authorities=input_authorities,
            parameters={
                "backends": [b.name for b in backends],
                "executed": [c.backend for c in executed],
                "unavailable": [c.backend for c in candidates if not c.executed],
            },
            limitations=[
                "portfolio is a set of hypotheses, not a single truth",
                "fusion is bounded and may be empty",
            ],
            rights_state="mixed" if candidates else "unreviewed",
        ),
        uncertainty=uncertainty,
        notes=list(notes or []),
        candidates=candidates,
        comparison=comparison,
        fusion=fusion_payload,
        selected_candidate_id=selected_candidate_id,
        hidden_surface_ledger=hidden_ledger,
    )
    return portfolio.seal()


def write_portfolio(path: Path, portfolio: ReconstructionPortfolio) -> Path:
    from blender_vision.v2.validation import write_record

    return write_record(path, portfolio)


def _inputs_for(
    backend_name: str,
    inputs_for: dict[str, ReconstructionInputs] | ReconstructionInputs,
) -> ReconstructionInputs:
    if isinstance(inputs_for, ReconstructionInputs):
        # Per-backend work subdirectory to avoid artifact clobbering.
        return ReconstructionInputs(
            target_id=inputs_for.target_id,
            work_dir=inputs_for.work_dir / backend_name,
            frame=inputs_for.frame,
            image_dir=inputs_for.image_dir,
            masks=list(inputs_for.masks),
            cameras=list(inputs_for.cameras),
            bounds_min=inputs_for.bounds_min,
            bounds_max=inputs_for.bounds_max,
            depth_frames=list(inputs_for.depth_frames),
            points=inputs_for.points,
            primitive_kind=inputs_for.primitive_kind,
            library_dir=inputs_for.library_dir,
            archetype_id=inputs_for.archetype_id,
            adaptation_scale=inputs_for.adaptation_scale,
            landmarks_source=inputs_for.landmarks_source,
            landmarks_target=inputs_for.landmarks_target,
            browser_scene=inputs_for.browser_scene,
            metric_anchor_m=inputs_for.metric_anchor_m,
            licensing=inputs_for.licensing,
            parameters=dict(inputs_for.parameters),
            evidence_refs=list(inputs_for.evidence_refs),
            input_authorities=list(inputs_for.input_authorities),
        )
    if backend_name in inputs_for:
        return inputs_for[backend_name]
    # Fallback: first inputs entry with work_dir specialized.
    if not inputs_for:
        raise ValueError("no reconstruction inputs provided")
    base = next(iter(inputs_for.values()))
    return _inputs_for(backend_name, base)
