"""Build and seal DeliveryManifest records with honest budget evaluation."""

from __future__ import annotations

from collections.abc import Sequence
from typing import Any

from blender_vision.core.util import utc_now
from blender_vision.delivery.compress import CompressionSelection
from blender_vision.delivery.stream import StreamingPlan
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import DeliveryAsset, DeliveryManifest, Lineage
from blender_vision.v2.validation import validate_record

# Frozen budgets from the V2 goal. Do not relax these to hide violations.
FROZEN_BUDGETS: dict[str, Any] = {
    "initial_js_compressed_bytes": 300 * 1024,
    "shell_glb_bytes": int(1.5 * 1024 * 1024),
    "mobile_shell_bytes": 650 * 1024,
    "poster_before_glb": True,
    "detail_chapter_gated": True,
}


def evaluate_budgets(
    assets: Sequence[DeliveryAsset],
    *,
    budgets: dict[str, Any] | None = None,
    streaming_plan: StreamingPlan | None = None,
    initial_js_compressed_bytes: int | None = None,
) -> list[dict[str, Any]]:
    """Return budget violations. Never suppresses a breach by raising the budget."""
    resolved = dict(budgets or FROZEN_BUDGETS)
    violations: list[dict[str, Any]] = []

    shell_bytes = sum(item.bytes for item in assets if item.role == "shell")
    shell_budget = int(resolved["shell_glb_bytes"])
    if shell_bytes > shell_budget:
        violations.append(
            {
                "budget": "shell_glb_bytes",
                "limit": shell_budget,
                "actual": shell_bytes,
                "unit": "bytes",
                "message": f"shell GLB {shell_bytes} exceeds budget {shell_budget}",
            }
        )

    mobile_bytes = sum(item.bytes for item in assets if item.role == "mobile")
    mobile_budget = int(resolved["mobile_shell_bytes"])
    if mobile_bytes > mobile_budget:
        violations.append(
            {
                "budget": "mobile_shell_bytes",
                "limit": mobile_budget,
                "actual": mobile_bytes,
                "unit": "bytes",
                "message": f"mobile shell {mobile_bytes} exceeds budget {mobile_budget}",
            }
        )

    if initial_js_compressed_bytes is not None:
        js_budget = int(resolved["initial_js_compressed_bytes"])
        if initial_js_compressed_bytes > js_budget:
            violations.append(
                {
                    "budget": "initial_js_compressed_bytes",
                    "limit": js_budget,
                    "actual": initial_js_compressed_bytes,
                    "unit": "bytes",
                    "message": (
                        f"initial JS compressed {initial_js_compressed_bytes} "
                        f"exceeds budget {js_budget}"
                    ),
                }
            )

    if resolved.get("poster_before_glb") and streaming_plan is not None:
        roles_order = [stage.role for stage in sorted(streaming_plan.stages, key=lambda s: s.order)]
        if (
            "poster" in roles_order
            and "shell" in roles_order
            and roles_order.index("poster") > roles_order.index("shell")
        ):
            violations.append(
                {
                    "budget": "poster_before_glb",
                    "limit": True,
                    "actual": False,
                    "message": "shell stage ordered before poster",
                }
            )
        poster_assets = [item for item in assets if item.role == "poster"]
        if not poster_assets:
            violations.append(
                {
                    "budget": "poster_before_glb",
                    "limit": True,
                    "actual": False,
                    "message": "poster asset missing while poster_before_glb is required",
                }
            )

    if resolved.get("detail_chapter_gated") and streaming_plan is not None:
        detail_stages = [stage for stage in streaming_plan.stages if stage.role == "detail"]
        for stage in detail_stages:
            if stage.scroll_trigger <= 0.0 and stage.chapter in {None, "THRESHOLD"}:
                violations.append(
                    {
                        "budget": "detail_chapter_gated",
                        "limit": True,
                        "actual": False,
                        "message": (
                            f"detail stage {stage.stage_id} is not chapter-gated "
                            f"(scroll_trigger={stage.scroll_trigger}, chapter={stage.chapter})"
                        ),
                    }
                )

    return violations


def build_delivery_manifest(
    *,
    manifest_id: str,
    source_scene: str,
    assets: Sequence[DeliveryAsset],
    compression_selections: Sequence[CompressionSelection] | None = None,
    streaming_plan: StreamingPlan | None = None,
    budgets: dict[str, Any] | None = None,
    initial_js_compressed_bytes: int | None = None,
    receipts: Sequence[str] | None = None,
    input_authorities: Sequence[str] | None = None,
) -> DeliveryManifest:
    """Seal a DeliveryManifest with measured metrics and explicit violations."""
    resolved_budgets = dict(budgets or FROZEN_BUDGETS)
    asset_list = list(assets)
    violations = evaluate_budgets(
        asset_list,
        budgets=resolved_budgets,
        streaming_plan=streaming_plan,
        initial_js_compressed_bytes=initial_js_compressed_bytes,
    )

    measured: dict[str, Any] = {
        "asset_count": len(asset_list),
        "total_bytes": sum(item.bytes for item in asset_list),
        "by_role": {},
        "compression": {},
        "evaluated_at": utc_now(),
    }
    for role in sorted({item.role for item in asset_list}):
        measured["by_role"][role] = {
            "count": sum(1 for item in asset_list if item.role == role),
            "bytes": sum(item.bytes for item in asset_list if item.role == role),
        }
    for selection in compression_selections or []:
        measured["compression"][selection.asset_id] = selection.to_dict()
    if streaming_plan is not None:
        measured["streaming_plan"] = streaming_plan.to_dict()
    if initial_js_compressed_bytes is not None:
        measured["initial_js_compressed_bytes"] = initial_js_compressed_bytes

    authorities = list(input_authorities or [AuthorityClass.PROCEDURAL_GROUND_TRUTH.value])
    # Measurements are RUNTIME_OBSERVED when we actually measured; still capped by inputs.
    authority = derive(authorities, proposed=AuthorityClass.INFERRED)

    notes = []
    if violations:
        notes.append(f"{len(violations)} budget violation(s) recorded (not suppressed)")
    else:
        notes.append("all declared budgets satisfied")

    manifest = DeliveryManifest(
        id=manifest_id,
        authority=authority,
        lineage=Lineage(
            operation="build_delivery_manifest",
            inputs=[source_scene],
            input_authorities=[str(item) for item in authorities],
            parameters={"budgets": resolved_budgets},
            limitations=[
                "Byte counts measured in-process; network transfer variance is not modelled."
            ],
        ),
        uncertainty=Uncertainty(
            kind="delivery_bytes",
            sigma=None,
            units=Units.UNITLESS,
            basis="filesystem stat of selected compressed payloads",
            samples=len(asset_list),
        ),
        source_scene=source_scene,
        assets=asset_list,
        budgets=resolved_budgets,
        measured=measured,
        budget_violations=violations,
        receipts=list(receipts or []),
        notes=notes,
    )
    manifest.seal()
    validate_record(manifest)
    return manifest
