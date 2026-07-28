"""Phase K — real-remote perception through the ocular loop.

Drive a governed self-captured fixture (or any rights-cleared image sequence)
through:

  calibrated frames → segmentation → dense appearance features →
  perception-derived identities → temporal association → world update →
  next-best-view

Ground truth never enters the builder path. The fixture is procedural
ground-truth geometry only; it is never described as the user's object.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

import cv2
import numpy as np
from numpy.typing import NDArray

from blender_vision.active_perception import (
    NextBestViewPlanner,
    PerceptionTarget,
    PlannerConfig,
    SurfaceCell,
    consumer_object_candidates,
)
from blender_vision.core.errors import ValidationError
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now
from blender_vision.ocular.attestation import (
    ExecutionClass,
    RuntimeAttestation,
    attest_blocked,
    attest_substitute,
)
from blender_vision.ocular.records import ColourSpace, OcularFrame, default_lineage
from blender_vision.ocular.segment import (
    SegmentationMethod,
    SegmentInstance,
    segment,
)
from blender_vision.ocular.sensors import PrivacyState, RightsState, SourceType
from blender_vision.ocular.stream import (
    close_stream,
    open_stream,
)
from blender_vision.ocular.track import Detection, TrackerState, TrackTargetKind, track
from blender_vision.ocular.world import (
    SurfaceProvenance,
    build_world_model,
)
from blender_vision.v2.authority import (
    AuthorityClass,
    VisibilityState,
)

ArrayU8 = NDArray[np.uint8]

#: Claim boundary: this fixture is not the user's remote.
FIXTURE_CLAIM = (
    "Governed self-captured procedural fixture. Not a claim about any physical "
    "remote control, including any remote the user may own."
)

DEFAULT_TRAIN_DIR = (
    Path(__file__).resolve().parents[3]
    / "artifacts"
    / "v2"
    / "object-benchmarks"
    / "remote"
    / "capture"
    / "images"
    / "train"
)


class EvidenceRole(StrEnum):
    OBSERVED = "observed"
    INFERRED = "inferred"
    NEXT_VIEW = "next_view"


@dataclass(slots=True)
class ViewReport:
    """What is observed, inferred, and requested next for one view."""

    view_id: str
    frame_index: int
    image_digest: str
    resolution: list[int]
    observed: list[dict[str, Any]] = field(default_factory=list)
    inferred: list[dict[str, Any]] = field(default_factory=list)
    next_view: list[dict[str, Any]] = field(default_factory=list)
    track_ids: list[str] = field(default_factory=list)
    segment_count: int = 0
    material_hypotheses: list[dict[str, Any]] = field(default_factory=list)
    lighting_separation: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "view_id": self.view_id,
            "frame_index": self.frame_index,
            "image_digest": self.image_digest,
            "resolution": list(self.resolution),
            "observed": list(self.observed),
            "inferred": list(self.inferred),
            "next_view": list(self.next_view),
            "track_ids": list(self.track_ids),
            "segment_count": self.segment_count,
            "material_hypotheses": list(self.material_hypotheses),
            "lighting_separation": dict(self.lighting_separation),
        }


@dataclass(slots=True)
class HiddenSurfaceLedgerEntry:
    region: str
    visibility: VisibilityState
    provenance: SurfaceProvenance
    authority_ceiling: AuthorityClass
    reason: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "region": self.region,
            "visibility": self.visibility.value,
            "provenance": self.provenance.value,
            "authority_ceiling": self.authority_ceiling.value,
            "reason": self.reason,
            "observed": False,
        }


@dataclass(slots=True)
class RemoteLoopReceipt:
    """Physical/diagnostic receipt for one remote-loop run."""

    schema: str = "ocular.remote-loop-receipt/1"
    target_id: str = "ocular_remote"
    claim: str = FIXTURE_CLAIM
    authority: str = AuthorityClass.SENSOR_DERIVED.value
    execution_class: str = ExecutionClass.DIAGNOSTIC_ONLY.value
    completed_at: str = ""
    train_image_count: int = 0
    views: list[ViewReport] = field(default_factory=list)
    world_summary: dict[str, Any] = field(default_factory=dict)
    geometry_portfolio: dict[str, Any] = field(default_factory=dict)
    hidden_surface_ledger: list[HiddenSurfaceLedgerEntry] = field(default_factory=list)
    next_view_plan: dict[str, Any] = field(default_factory=dict)
    measurements: dict[str, Any] = field(default_factory=dict)
    material_hypotheses: list[dict[str, Any]] = field(default_factory=list)
    lighting_separation: dict[str, Any] = field(default_factory=dict)
    delivery: dict[str, Any] = field(default_factory=dict)
    attestations: list[dict[str, Any]] = field(default_factory=list)
    blockers: list[dict[str, Any]] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    artifacts: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": self.schema,
            "target_id": self.target_id,
            "claim": self.claim,
            "authority": self.authority,
            "execution_class": self.execution_class,
            "completed_at": self.completed_at,
            "train_image_count": self.train_image_count,
            "views": [v.to_dict() for v in self.views],
            "world_summary": dict(self.world_summary),
            "geometry_portfolio": dict(self.geometry_portfolio),
            "hidden_surface_ledger": [h.to_dict() for h in self.hidden_surface_ledger],
            "next_view_plan": dict(self.next_view_plan),
            "measurements": dict(self.measurements),
            "material_hypotheses": list(self.material_hypotheses),
            "lighting_separation": dict(self.lighting_separation),
            "delivery": dict(self.delivery),
            "attestations": list(self.attestations),
            "blockers": list(self.blockers),
            "notes": list(self.notes),
            "artifacts": dict(self.artifacts),
        }


def resolve_train_dir(train_dir: Path | None = None) -> Path:
    """Locate train images. Prefers explicit path, then governed fixture."""
    if train_dir is not None:
        path = train_dir.expanduser().resolve()
        if not path.is_dir():
            raise ValidationError(f"train image directory missing: {path}")
        return path
    default = DEFAULT_TRAIN_DIR
    if default.is_dir() and any(default.glob("*.png")):
        return default.resolve()
    raise ValidationError(
        "no train images: supply --train-dir or place the governed fixture at "
        f"{DEFAULT_TRAIN_DIR}"
    )


def list_train_images(train_dir: Path) -> list[Path]:
    paths = sorted(
        p
        for p in train_dir.iterdir()
        if p.is_file() and p.suffix.lower() in {".png", ".jpg", ".jpeg", ".webp"}
    )
    if not paths:
        raise ValidationError(f"no images under {train_dir}")
    return paths


def _image_digest(path: Path) -> str:
    return sha256_file(path)[0]


def _dense_appearance_features(image: ArrayU8, instance: SegmentInstance) -> dict[str, Any]:
    """Classical dense appearance: colour hist + local gradient energy. No weights."""
    x, y, w, h = instance.bbox_xywh
    h_img, w_img = image.shape[:2]
    x0, y0 = max(0, x), max(0, y)
    x1, y1 = min(w_img, x + w), min(h_img, y + h)
    crop = image[y0:y1, x0:x1]
    if crop.size == 0:
        return {
            "hist": list(instance.appearance_hist),
            "gradient_energy": 0.0,
            "mean_bgr": list(instance.mean_bgr),
            "method": "classical_hist_gradient",
        }
    gray = cv2.cvtColor(crop, cv2.COLOR_BGR2GRAY) if crop.ndim == 3 else crop
    gx = cv2.Sobel(gray, cv2.CV_32F, 1, 0, ksize=3)
    gy = cv2.Sobel(gray, cv2.CV_32F, 0, 1, ksize=3)
    energy = float(np.mean(np.sqrt(gx * gx + gy * gy)))
    return {
        "hist": list(instance.appearance_hist),
        "gradient_energy": energy,
        "mean_bgr": list(instance.mean_bgr),
        "method": "classical_hist_gradient",
        "authority": AuthorityClass.SENSOR_DERIVED.value,
    }


def _material_hypothesis(instance: SegmentInstance, features: dict[str, Any]) -> dict[str, Any]:
    """Separate body-like dark plastic from high-chroma button-like regions."""
    b, g, r = instance.mean_bgr
    chroma = max(r, g, b) - min(r, g, b)
    luminance = 0.114 * b + 0.587 * g + 0.299 * r
    energy = float(features.get("gradient_energy", 0.0))
    if chroma > 35 and luminance > 40:
        label = "high_chroma_plastic_candidate"
        roughness = 0.45
    elif luminance < 50:
        label = "dark_body_plastic_candidate"
        roughness = 0.55
    else:
        label = "mid_tone_surface_candidate"
        roughness = 0.50
    return {
        "segment_id": instance.segment_id,
        "label": label,
        "roughness_hypothesis": roughness,
        "metallic_hypothesis": 0.05 if chroma < 40 else 0.0,
        "chroma": float(chroma),
        "luminance": float(luminance),
        "gradient_energy": energy,
        "authority": AuthorityClass.INFERRED.value,
        "provenance": SurfaceProvenance.PROCEDURALLY_INFERRED.value,
        "note": "Hypothesis from pixels only; not a measured BRDF.",
    }


def _lighting_separation(image: ArrayU8) -> dict[str, Any]:
    """Split low-frequency illumination from high-frequency reflectance proxy."""
    if image.ndim == 3:
        gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY).astype(np.float32)
    else:
        gray = image.astype(np.float32)
    low = cv2.GaussianBlur(gray, (0, 0), sigmaX=12.0)
    high = gray - low
    return {
        "method": "gaussian_low_high_split",
        "illumination_mean": float(np.mean(low)),
        "illumination_std": float(np.std(low)),
        "reflectance_proxy_std": float(np.std(high)),
        "authority": AuthorityClass.SENSOR_DERIVED.value,
        "note": "Classical frequency split; not inverse rendering.",
    }


def _observed_from_instance(
    instance: SegmentInstance, features: dict[str, Any]
) -> dict[str, Any]:
    return {
        "role": EvidenceRole.OBSERVED.value,
        "segment_id": instance.segment_id,
        "bbox_xywh": list(instance.bbox_xywh),
        "centroid_xy": list(instance.centroid_xy),
        "area_px": instance.area_px,
        "mean_bgr": list(instance.mean_bgr),
        "appearance": features,
        "authority": AuthorityClass.SENSOR_DERIVED.value,
        "provenance": SurfaceProvenance.DIRECTLY_OBSERVED.value,
    }


def _inferred_from_tracks(
    tracker: TrackerState, frame_index: int
) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for trk in tracker.tracks:
        if trk.state.value in {"OCCLUDED", "LOST"}:
            rows.append(
                {
                    "role": EvidenceRole.INFERRED.value,
                    "track_id": trk.track_id,
                    "state": trk.state.value,
                    "predicted_xy": list(trk.predicted_xy or trk.centroid_xy),
                    "identity_uncertainty": trk.identity_uncertainty,
                    "frames_since_seen": trk.frames_since_seen,
                    "authority": AuthorityClass.INFERRED.value,
                    "provenance": SurfaceProvenance.SENSOR_DERIVED.value,
                    "note": "Object permanence: identity retained without current pixels.",
                }
            )
        elif trk.frames_since_seen == 0 and frame_index > 0:
            # Pose continuity is partially inferred from Kalman even when visible.
            rows.append(
                {
                    "role": EvidenceRole.INFERRED.value,
                    "track_id": trk.track_id,
                    "state": trk.state.value,
                    "predicted_xy": list(trk.predicted_xy or trk.centroid_xy),
                    "identity_uncertainty": trk.identity_uncertainty,
                    "authority": AuthorityClass.SENSOR_DERIVED.value,
                    "note": "Temporal identity association from appearance+motion.",
                }
            )
    return rows


def default_hidden_surface_ledger() -> list[HiddenSurfaceLedgerEntry]:
    """Regions a consumer remote typically hides without dedicated capture."""
    never = VisibilityState.NEVER_OBSERVED
    inferred = SurfaceProvenance.PROCEDURALLY_INFERRED
    ceiling = AuthorityClass.INFERRED
    return [
        HiddenSurfaceLedgerEntry(
            region="underside",
            visibility=never,
            provenance=inferred,
            authority_ceiling=ceiling,
            reason="No underside train views in the governed fixture orbit.",
        ),
        HiddenSurfaceLedgerEntry(
            region="battery_hatch_interior",
            visibility=never,
            provenance=inferred,
            authority_ceiling=ceiling,
            reason="Compartment interior not imaged while hatch closed.",
        ),
        HiddenSurfaceLedgerEntry(
            region="internals",
            visibility=never,
            provenance=inferred,
            authority_ceiling=ceiling,
            reason="PCB and contacts require disassembly; never observed.",
        ),
        HiddenSurfaceLedgerEntry(
            region="button_sidewalls",
            visibility=VisibilityState.PARTIALLY_VISIBLE,
            provenance=SurfaceProvenance.MULTI_VIEW_DERIVED,
            authority_ceiling=AuthorityClass.SENSOR_DERIVED,
            reason="Sidewalls partially visible at grazing angles only.",
        ),
    ]


def _geometry_portfolio_from_masks(
    images: list[ArrayU8],
    masks: list[NDArray[np.int32]],
    output: Path,
) -> dict[str, Any]:
    """Build mesh + point-cloud candidates from multi-view silhouettes.

    No GT boxes. Dense radiance is recorded BLOCKED (no weights / no network).
    """
    output.mkdir(parents=True, exist_ok=True)
    candidates: list[dict[str, Any]] = []

    # Point cloud from mask centroids / silhouette samples (sensor-derived).
    points: list[list[float]] = []
    for mask in masks:
        ys, xs = np.where(mask > 0)
        if ys.size == 0:
            continue
        step = max(1, ys.size // 400)
        for y, x in zip(ys[::step], xs[::step], strict=True):
            # Place points on a unit image plane; scale is unknown without anchor.
            points.append([float(x) / mask.shape[1], float(y) / mask.shape[0], 0.0])

    ply_path = output / "remote_points.ply"
    if points:
        _write_ascii_ply_points(ply_path, points)
        candidates.append(
            {
                "backend": "point_cloud",
                "executed": True,
                "path": str(ply_path),
                "n_points": len(points),
                "authority": AuthorityClass.SENSOR_DERIVED.value,
                "purposes": ["measurement_sparse", "web_preview"],
            }
        )
    else:
        candidates.append(
            {
                "backend": "point_cloud",
                "executed": False,
                "reason": "no foreground masks",
                "authority": AuthorityClass.UNRESOLVED.value,
            }
        )

    # Visual-hull style box from union of 2D extents (editable mesh candidate).
    if masks:
        ys_parts = [np.where(m > 0)[0] for m in masks if np.any(m > 0)]
        xs_parts = [np.where(m > 0)[1] for m in masks if np.any(m > 0)]
        all_ys = np.concatenate(ys_parts) if ys_parts else np.array([0])
        all_xs = np.concatenate(xs_parts) if xs_parts else np.array([0])
        h, w = masks[0].shape[:2]
        # Normalised half-extents; metric scale requires an external anchor.
        hx = 0.5 * (float(all_xs.max() - all_xs.min()) / max(w, 1))
        hy = 0.5 * (float(all_ys.max() - all_ys.min()) / max(h, 1))
        hz = min(hx, hy) * 0.35
        mesh_path = output / "remote_mesh_box.obj"
        _write_box_obj(mesh_path, hx, hy, hz)
        candidates.append(
            {
                "backend": "mesh_visual_hull_box",
                "executed": True,
                "path": str(mesh_path),
                "half_extents_normalised": [hx, hy, hz],
                "authority": AuthorityClass.SENSOR_DERIVED.value,
                "purposes": ["editable_geometry", "measurement_relative", "animation_proxy"],
                "note": "Axis-aligned hull proxy from silhouettes; not metric without scale.",
            }
        )

    # Procedural candidate: parametric remote body from image statistics only.
    proc_path = output / "remote_procedural.json"
    proc = {
        "kind": "parametric_consumer_object",
        "parameters": {
            "aspect_xy": float(hx / max(hy, 1e-6)) if masks else 1.0,
            "button_grid_hypothesis": [4, 2],
            "source": "image_statistics_only",
        },
        "authority": AuthorityClass.PROCEDURALLY_INFERRED.value
        if hasattr(AuthorityClass, "PROCEDURALLY_INFERRED")
        else AuthorityClass.INFERRED.value,
        "claim": FIXTURE_CLAIM,
    }
    # Authority enum may not have PROCEDURALLY_INFERRED — use INFERRED.
    proc["authority"] = AuthorityClass.INFERRED.value
    atomic_write_json(proc_path, proc)
    candidates.append(
        {
            "backend": "procedural_parametric",
            "executed": True,
            "path": str(proc_path),
            "authority": AuthorityClass.INFERRED.value,
            "purposes": ["editable_geometry", "animation_proxy"],
        }
    )

    # Retrieved candidate: licensing allows only public domain / owned archetypes.
    candidates.append(
        {
            "backend": "retrieval",
            "executed": False,
            "reason": (
                "No rights-cleared retrieved archetype licensed for this host run; "
                "refusing to substitute a random mesh."
            ),
            "authority": AuthorityClass.UNRESOLVED.value,
            "execution_class": ExecutionClass.BLOCKED.value,
            "purposes": ["web_preview"],
        }
    )

    # Gaussian / radiance: honestly BLOCKED.
    radiance_block = attest_blocked(
        "gaussian-radiance",
        "No trained radiance/Gaussian weights on this host; network download forbidden. "
        "Radiance candidate is BLOCKED, not substituted.",
    )
    candidates.append(
        {
            "backend": "gaussian_radiance",
            "executed": False,
            "reason": radiance_block.blocked_reason,
            "authority": AuthorityClass.UNRESOLVED.value,
            "execution_class": ExecutionClass.BLOCKED.value,
            "purposes": ["photoreal_view_synthesis"],
            "attestation": radiance_block.to_dict(),
        }
    )

    purpose_eval = evaluate_representations(candidates)
    portfolio = {
        "target_id": "ocular_remote",
        "candidates": candidates,
        "purpose_evaluation": purpose_eval,
        "note": "Do not force one representation to do every job.",
    }
    atomic_write_json(output / "geometry_portfolio.json", portfolio)
    return portfolio


def evaluate_representations(candidates: list[dict[str, Any]]) -> dict[str, Any]:
    """Score each representation against purpose classes honestly."""
    purposes = [
        "photoreal_view_synthesis",
        "editable_geometry",
        "measurement",
        "web",
        "animation",
    ]
    purpose_map = {
        "photoreal_view_synthesis": {"gaussian_radiance"},
        "editable_geometry": {"mesh_visual_hull_box", "procedural_parametric"},
        "measurement": {"point_cloud", "mesh_visual_hull_box"},
        "web": {"point_cloud", "retrieval", "mesh_visual_hull_box"},
        "animation": {"mesh_visual_hull_box", "procedural_parametric"},
    }
    by_backend = {c["backend"]: c for c in candidates}
    result: dict[str, Any] = {}
    for purpose in purposes:
        fits = []
        for backend in purpose_map[purpose]:
            cand = by_backend.get(backend)
            if cand is None:
                continue
            fits.append(
                {
                    "backend": backend,
                    "suitable": bool(cand.get("executed")),
                    "reason": cand.get("reason")
                    or ("executed" if cand.get("executed") else "not executed"),
                }
            )
        result[purpose] = fits
    # Explicit: radiance never silent-pass.
    rad = by_backend.get("gaussian_radiance")
    if rad is not None and rad.get("executed"):
        raise ValidationError("radiance must not execute without weights")
    result["radiance_blocked"] = True
    result["radiance_reason"] = (rad or {}).get("reason", "unavailable")
    return result


def _write_ascii_ply_points(path: Path, points: list[list[float]]) -> None:
    lines = [
        "ply",
        "format ascii 1.0",
        "comment ocular remote loop silhouette points",
        f"element vertex {len(points)}",
        "property float x",
        "property float y",
        "property float z",
        "end_header",
    ]
    for x, y, z in points:
        lines.append(f"{x:.6f} {y:.6f} {z:.6f}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _write_box_obj(path: Path, hx: float, hy: float, hz: float) -> None:
    verts = [
        (-hx, -hy, -hz),
        (hx, -hy, -hz),
        (hx, hy, -hz),
        (-hx, hy, -hz),
        (-hx, -hy, hz),
        (hx, -hy, hz),
        (hx, hy, hz),
        (-hx, hy, hz),
    ]
    faces = [
        (1, 2, 3, 4),
        (5, 6, 7, 8),
        (1, 2, 6, 5),
        (2, 3, 7, 6),
        (3, 4, 8, 7),
        (4, 1, 5, 8),
    ]
    lines = ["# ocular remote loop hull proxy", "o remote_hull"]
    for x, y, z in verts:
        lines.append(f"v {x:.6f} {y:.6f} {z:.6f}")
    for a, b, c, d in faces:
        lines.append(f"f {a} {b} {c} {d}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _measurements_from_masks(masks: list[NDArray[np.int32]]) -> dict[str, Any]:
    if not masks:
        return {"status": "empty"}
    areas = [int(np.count_nonzero(m > 0)) for m in masks]
    # Relative aspect from median mask bounding box.
    boxes = []
    for m in masks:
        ys, xs = np.where(m > 0)
        if ys.size == 0:
            continue
        boxes.append((int(xs.max() - xs.min() + 1), int(ys.max() - ys.min() + 1)))
    if not boxes:
        return {"status": "no_foreground", "areas_px": areas}
    ws, hs = zip(*boxes, strict=True)
    med_w = float(np.median(ws))
    med_h = float(np.median(hs))
    return {
        "status": "relative_only",
        "median_bbox_w_px": med_w,
        "median_bbox_h_px": med_h,
        "aspect_w_over_h": med_w / max(med_h, 1.0),
        "mean_area_px": float(np.mean(areas)),
        "scale": "unknown_without_metric_anchor",
        "authority": AuthorityClass.SENSOR_DERIVED.value,
        "note": "Pixel measurements only; metric scale requires a ruler/credit-card anchor.",
    }


def _next_view_plan(uncovered: list[str] | None = None) -> dict[str, Any]:
    cells = [
        SurfaceCell(
            region=region,
            area_m2=0.01 if region != "button_sidewalls" else 0.002,
            covered=False,
            occlusion_fraction=1.0,
            resolution_px=0,
        )
        for region in (uncovered or ["underside", "battery_hatch_interior", "internals", "rear"])
    ]
    target = PerceptionTarget(
        target_id="ocular_remote",
        cells=cells,
        scale_authority=AuthorityClass.UNRESOLVED,
    )
    planner = NextBestViewPlanner(config=PlannerConfig(max_requests=6, gain_threshold=0.01))
    # planner.plan builds consumer_object_candidates internally.
    _ = consumer_object_candidates(target)
    result = planner.plan(target)
    return result.to_dict()


def process_view(
    image: ArrayU8,
    *,
    view_id: str,
    frame_index: int,
    image_digest: str,
    tracker: TrackerState,
    previous_image: ArrayU8 | None,
    previous_seg: Any | None,
) -> tuple[ViewReport, TrackerState, Any, NDArray[np.int32]]:
    """Segment + track one frame. No GT boxes."""
    result, labels = segment(
        image,
        method=SegmentationMethod.WATERSHED,
        previous_image=previous_image,
        previous_result=previous_seg,
        min_area=40,
        max_regions=24,
        result_id=f"seg-{view_id}",
    )
    detections = [
        Detection.from_segment(inst, frame_index=frame_index, kind=TrackTargetKind.OBJECT)
        for inst in result.instances
    ]
    tracker = track(detections, tracker, frame_index=frame_index)

    observed = []
    materials = []
    for inst in result.instances:
        features = _dense_appearance_features(image, inst)
        observed.append(_observed_from_instance(inst, features))
        materials.append(_material_hypothesis(inst, features))

    lighting = _lighting_separation(image)
    inferred = _inferred_from_tracks(tracker, frame_index)

    # Per-view next capture suggestions from current coverage gaps.
    active_area = float(np.count_nonzero(labels > 0)) / max(labels.size, 1)
    next_view: list[dict[str, Any]] = []
    if active_area < 0.05:
        next_view.append(
            {
                "role": EvidenceRole.NEXT_VIEW.value,
                "request": "closer_framing",
                "reason": "foreground occupies <5% of pixels; fill frame",
                "priority": 8,
            }
        )
    if frame_index == 0:
        next_view.append(
            {
                "role": EvidenceRole.NEXT_VIEW.value,
                "request": "orbit_continue",
                "reason": "first view only; need multi-view coverage",
                "priority": 6,
            }
        )
    # Underside never present in orbit-only fixture.
    next_view.append(
        {
            "role": EvidenceRole.NEXT_VIEW.value,
            "request": "underside",
            "reason": "underside not observed in current orbit",
            "priority": 9,
            "human_instructions": (
                "Photograph the underside with a scale reference (ruler or credit card)."
            ),
        }
    )

    report = ViewReport(
        view_id=view_id,
        frame_index=frame_index,
        image_digest=image_digest,
        resolution=[int(image.shape[1]), int(image.shape[0])],
        observed=observed,
        inferred=inferred,
        next_view=next_view,
        track_ids=[t.track_id for t in tracker.tracks if t.frames_since_seen == 0],
        segment_count=len(result.instances),
        material_hypotheses=materials,
        lighting_separation=lighting,
    )
    return report, tracker, result, labels


def run_remote_loop(
    output: Path,
    *,
    train_dir: Path | None = None,
    max_views: int | None = None,
) -> RemoteLoopReceipt:
    """Execute the ocular remote perception loop and write a receipt."""
    output = output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    receipt = RemoteLoopReceipt(completed_at=utc_now())
    receipt.notes.append(FIXTURE_CLAIM)

    try:
        images_dir = resolve_train_dir(train_dir)
    except ValidationError as exc:
        blocked = attest_blocked("remote-train-images", str(exc))
        receipt.attestations.append(blocked.to_dict())
        receipt.blockers.append({"id": "train_images", "reason": str(exc)})
        receipt.execution_class = ExecutionClass.BLOCKED.value
        atomic_write_json(output / "remote_loop.receipt.json", receipt.to_dict())
        return receipt

    paths = list_train_images(images_dir)
    if max_views is not None:
        paths = paths[: max(1, max_views)]
    receipt.train_image_count = len(paths)
    receipt.notes.append(f"train_dir={images_dir}")
    receipt.artifacts["train_dir"] = str(images_dir)

    # Stream attestation: real image sequence on disk is physical input.
    stream_result = open_stream(
        images_dir,
        source_type=SourceType.IMAGE_SEQUENCE,
        stream_id="ocular-remote-train",
        rights_state=RightsState.OWNED,
        privacy_state=PrivacyState.CLEARED,
    )
    if isinstance(stream_result, RuntimeAttestation):
        receipt.attestations.append(stream_result.to_dict())
        receipt.execution_class = stream_result.execution_class.value
        if stream_result.execution_class is ExecutionClass.BLOCKED:
            receipt.blockers.append(
                {"id": "stream", "reason": stream_result.blocked_reason}
            )
            atomic_write_json(output / "remote_loop.receipt.json", receipt.to_dict())
            return receipt
        handle = None
    else:
        handle = stream_result
        receipt.execution_class = handle.execution_class.value
        receipt.attestations.append(
            {
                "runtime": "image_sequence",
                "execution_class": handle.execution_class.value,
                "stream_id": handle.stream_id,
                "n_paths": len(paths),
            }
        )

    tracker = TrackerState()
    previous_image: ArrayU8 | None = None
    previous_seg = None
    images: list[ArrayU8] = []
    masks: list[NDArray[np.int32]] = []
    world_observations: list[dict[str, Any]] = []
    all_materials: list[dict[str, Any]] = []

    try:
        for frame_index, path in enumerate(paths):
            image = cv2.imread(str(path), cv2.IMREAD_COLOR)
            if image is None:
                receipt.notes.append(f"skip unreadable {path.name}")
                continue
            digest = _image_digest(path)
            # Seal an OcularFrame for lineage without mutating later.
            frame = OcularFrame(
                id=f"frame-{path.stem}",
                frame_id=path.stem,
                stream_id="ocular-remote-train",
                timestamp=float(frame_index),
                image_digest=digest,
                resolution=[int(image.shape[1]), int(image.shape[0])],
                colour_space=ColourSpace.BGR,
                sequence_index=frame_index,
                authority=AuthorityClass.SENSOR_DERIVED,
                lineage=default_lineage(
                    "ocular.remote_loop.frame",
                    inputs=[digest],
                ),
            ).seal()
            del frame  # sealed for digest discipline; pixels stay in `image`

            report, tracker, previous_seg, labels = process_view(
                image,
                view_id=path.stem,
                frame_index=frame_index,
                image_digest=digest,
                tracker=tracker,
                previous_image=previous_image,
                previous_seg=previous_seg,
            )
            receipt.views.append(report)
            images.append(image)
            masks.append(labels)
            all_materials.extend(report.material_hypotheses)
            previous_image = image

            entities = []
            for trk in tracker.tracks:
                if trk.frames_since_seen > 0:
                    continue
                entities.append(
                    {
                        "entity_id": trk.track_id,
                        "track_id": trk.track_id,
                        "class_label": "object",
                        "pose_m": [
                            trk.centroid_xy[0] / max(image.shape[1], 1),
                            trk.centroid_xy[1] / max(image.shape[0], 1),
                            0.0,
                            1.0,
                            0.0,
                            0.0,
                            0.0,
                        ],
                        "visible": True,
                        "appearance": {"hist": list(trk.appearance_hist)},
                    }
                )
            world_observations.append(
                {
                    "frame_index": frame_index,
                    "entities": entities,
                    "track_source": "perception_derived",
                    "lighting": report.lighting_separation,
                }
            )
    finally:
        if handle is not None:
            close_stream(handle)

    if not receipt.views:
        receipt.blockers.append({"id": "no_views", "reason": "no processable train images"})
        receipt.execution_class = ExecutionClass.BLOCKED.value
        atomic_write_json(output / "remote_loop.receipt.json", receipt.to_dict())
        return receipt

    # World model from perception-derived tracks only.
    world = build_world_model(
        world_observations,
        scene_id="ocular-remote",
        session_id="remote-loop-0",
    )
    receipt.world_summary = {
        "scene_id": world.scene_id,
        "entity_count": len(world.entities),
        "entity_ids": sorted(world.entities.keys()),
        "current_frame": world.current_frame,
        "track_source": world.meta.get("track_source"),
        "authority": world.authority.value
        if hasattr(world.authority, "value")
        else str(world.authority),
    }

    geom_dir = output / "geometry"
    receipt.geometry_portfolio = _geometry_portfolio_from_masks(images, masks, geom_dir)
    receipt.hidden_surface_ledger = default_hidden_surface_ledger()
    receipt.measurements = _measurements_from_masks(masks)
    receipt.material_hypotheses = all_materials[:48]
    receipt.lighting_separation = receipt.views[-1].lighting_separation
    receipt.next_view_plan = _next_view_plan(
        [h.region for h in receipt.hidden_surface_ledger]
    )

    # Delivery stubs: editable mesh + GLB placeholder path (mesh is the editable form).
    mesh_candidate = next(
        (
            c
            for c in receipt.geometry_portfolio.get("candidates", [])
            if c.get("backend") == "mesh_visual_hull_box" and c.get("executed")
        ),
        None,
    )
    receipt.delivery = {
        "editable_mesh": (mesh_candidate or {}).get("path"),
        "glb": None,
        "glb_note": (
            "GLB export requires Blender physical run; mesh OBJ is the editable delivery "
            "when Blender is not invoked in this loop."
        ),
        "point_cloud": next(
            (
                c.get("path")
                for c in receipt.geometry_portfolio.get("candidates", [])
                if c.get("backend") == "point_cloud" and c.get("executed")
            ),
            None,
        ),
    }

    # Optional Blender attestation for GLB — try, never fake.
    blender = Path("/Applications/Blender.app/Contents/MacOS/Blender")
    if mesh_candidate and blender.is_file():
        glb_path = geom_dir / "remote_editable.glb"
        # We do not silently claim GLB success without running Blender; record path intent.
        receipt.delivery["glb_intended_path"] = str(glb_path)
        receipt.attestations.append(
            attest_substitute(
                "blender-glb-export",
                execution_class=ExecutionClass.CANDIDATE_ONLY,
                reason=(
                    "OBJ mesh produced by classical hull; full Blender GLB export left as "
                    "optional physical follow-up (not required for per-view observed/inferred)."
                ),
                substitute="obj_mesh",
            ).to_dict()
        )
    elif not blender.is_file():
        receipt.attestations.append(
            attest_blocked(
                "blender",
                f"Blender executable not found at {blender}",
            ).to_dict()
        )

    # COLMAP dense blocked honestly.
    receipt.blockers.append(
        {
            "id": "colmap_dense_mvs_cuda",
            "reason": "Host COLMAP has no CUDA; dense MVS blocked. Sparse may run separately.",
            "execution_class": ExecutionClass.BLOCKED.value,
        }
    )
    receipt.blockers.append(
        {
            "id": "gaussian_radiance",
            "reason": "No trained weights; download forbidden; radiance BLOCKED.",
            "execution_class": ExecutionClass.BLOCKED.value,
        }
    )

    receipt.artifacts["receipt"] = str(output / "remote_loop.receipt.json")
    receipt.artifacts["geometry_portfolio"] = str(geom_dir / "geometry_portfolio.json")
    atomic_write_json(output / "remote_loop.receipt.json", receipt.to_dict())
    atomic_write_json(
        output / "per_view_reports.json",
        [v.to_dict() for v in receipt.views],
    )
    return receipt


def capture_protocol_text() -> str:
    """Ready protocol for the user's actual remote (not the fixture)."""
    return """# Capture protocol — user's physical remote

This protocol is for a **rights-cleared capture of a remote you own**.
It is separate from the governed self-captured procedural fixture used in CI.

## Requirements

1. **≥ 24 orbit views**, short side ≥ 1600 px, sharp focus, even diffuse light.
2. **Underside** — tip carefully or shoot through glass; required or leave NEVER_OBSERVED.
3. **Scale reference** — metric ruler or credit card (85.60 × 53.98 mm) in ≥ 2 views.
4. **Lens metadata** — focal length (mm) and sensor width or 35 mm equivalent.
5. **Optional** ChArUco / checkerboard for intrinsics.
6. **Holdout set** — reserve ~8 views for the evaluator only; builder must not read them.

## Layout after capture

```
artifacts/v2/object-benchmarks/remote/real_capture/
  source_packet.json
  images/train/
  images/holdout/
  scale_reference.json
  camera_notes.json
```

## Command

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-ocular-remote.py \\
  --train-dir artifacts/v2/object-benchmarks/remote/real_capture/images/train \\
  --output artifacts/ocular/remote
```

## Claims

- Until real photographs are supplied, scored physical-remote claims stay BLOCKED.
- Dense COLMAP MVS stays BLOCKED without CUDA.
- Never seed detections from ground-truth boxes.
"""
