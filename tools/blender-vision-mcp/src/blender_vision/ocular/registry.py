"""Governed model registry for the Ocular Operating System.

Every learned model family the Bible names is registered here with exact
metadata. Entries ship in REVIEW_PENDING. No checkpoint is downloaded; an
entry without a local file is BackendState unavailable and can never be
selected for a physical claim.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import asdict, dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

from blender_vision.core.errors import ValidationError
from blender_vision.core.models import BackendState
from blender_vision.v2.authority import AuthorityClass


class ReviewState(StrEnum):
    """Human-governance state of a model entry. Independent of runtime readiness."""

    REVIEW_PENDING = "REVIEW_PENDING"
    ACCEPTED = "ACCEPTED"
    REJECTED = "REJECTED"


class ModelFamily(StrEnum):
    DENSE_FEATURES = "dense_features"
    PROMPTABLE_SEGMENTATION = "promptable_segmentation"
    GEOMETRY = "geometry"
    POINT_TRACKING = "point_tracking"
    RADIANCE = "radiance"
    PREDICTION = "prediction"
    CLASSICAL = "classical"


@dataclass(slots=True)
class ModelEntry:
    """One governed model. Fields match the intake contract in MODEL_INTAKE.md."""

    model_id: str
    family: ModelFamily
    display_name: str
    version: str
    source: str
    license: str
    checkpoint_digest: str
    allowed_use: list[str]
    hardware: list[str]
    dependencies: list[str]
    benchmark: str
    failure_profile: list[str]
    authority_ceiling: AuthorityClass
    privacy_implications: list[str]
    replacement_path: str
    review_state: ReviewState = ReviewState.REVIEW_PENDING
    checkpoint_path: str | None = None
    notes: list[str] = field(default_factory=list)

    def backend_state(self) -> BackendState:
        """Runtime readiness. REVIEW_PENDING alone never implies AVAILABLE."""
        if self.review_state is ReviewState.REJECTED:
            return BackendState.LICENSE_REVIEW_REQUIRED
        if not self.checkpoint_path:
            return BackendState.DOWNLOAD_REQUIRED
        if not Path(self.checkpoint_path).is_file():
            return BackendState.UNAVAILABLE
        if self.review_state is not ReviewState.ACCEPTED:
            return BackendState.LICENSE_REVIEW_REQUIRED
        return BackendState.AVAILABLE

    def selectable_for_physical(self) -> bool:
        """Physical claims require an accepted entry with a present local checkpoint."""
        return (
            self.review_state is ReviewState.ACCEPTED
            and self.backend_state() is BackendState.AVAILABLE
            and bool(self.checkpoint_path)
            and Path(self.checkpoint_path).is_file()
        )

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["family"] = self.family.value
        value["authority_ceiling"] = self.authority_ceiling.value
        value["review_state"] = self.review_state.value
        value["backend_state"] = self.backend_state().value
        value["selectable_for_physical"] = self.selectable_for_physical()
        return value


class PhysicalModelSelectionError(ValidationError):
    """Raised when a model is requested for a physical claim it cannot support."""


def _pending(
    model_id: str,
    family: ModelFamily,
    display_name: str,
    version: str,
    source: str,
    license: str,
    checkpoint_digest: str,
    allowed_use: list[str],
    hardware: list[str],
    dependencies: list[str],
    benchmark: str,
    failure_profile: list[str],
    authority_ceiling: AuthorityClass,
    privacy_implications: list[str],
    replacement_path: str,
    notes: list[str] | None = None,
) -> ModelEntry:
    return ModelEntry(
        model_id=model_id,
        family=family,
        display_name=display_name,
        version=version,
        source=source,
        license=license,
        checkpoint_digest=checkpoint_digest,
        allowed_use=list(allowed_use),
        hardware=list(hardware),
        dependencies=list(dependencies),
        benchmark=benchmark,
        failure_profile=list(failure_profile),
        authority_ceiling=authority_ceiling,
        privacy_implications=list(privacy_implications),
        replacement_path=replacement_path,
        review_state=ReviewState.REVIEW_PENDING,
        checkpoint_path=None,
        notes=list(notes or []),
    )


def default_catalog() -> list[ModelEntry]:
    """Bible-named families. All REVIEW_PENDING; no checkpoints bundled."""
    return [
        _pending(
            model_id="dino-v2-vitb14",
            family=ModelFamily.DENSE_FEATURES,
            display_name="DINOv2 ViT-B/14",
            version="dinov2_vitb14@2023-04",
            source="https://github.com/facebookresearch/dinov2",
            license="Apache-2.0",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["dense_descriptor", "correspondence_candidate", "diagnostic_only"],
            hardware=["cuda>=12", "metal-mps", "cpu-slow"],
            dependencies=["torch>=2.1", "torchvision"],
            benchmark="ocular-descriptor-transfer / not-run",
            failure_profile=[
                "domain shift on synthetic CAD textures",
                "specular collapse of descriptors",
                "no metric scale",
            ],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=[
                "descriptors can reconstruct approximate image content",
                "do not export descriptors from private captures without review",
            ],
            replacement_path="classical.colour_histogram + optical_flow",
            notes=["Download nothing until REVIEW_PENDING clears and a digest is pinned."],
        ),
        _pending(
            model_id="sam2-hiera-large",
            family=ModelFamily.PROMPTABLE_SEGMENTATION,
            display_name="SAM 2 Hiera-Large",
            version="sam2_hiera_large@2024-07",
            source="https://github.com/facebookresearch/sam2",
            license="Apache-2.0",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["promptable_mask", "video_masklet_candidate"],
            hardware=["cuda>=12", "metal-mps"],
            dependencies=["torch>=2.3", "sam2"],
            benchmark="ocular-tabletop-segmentation / not-run",
            failure_profile=[
                "prompt ambiguity on similar objects",
                "temporal flicker under heavy occlusion",
                "VRAM pressure on long masklets",
            ],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["masks reveal object presence in private scenes"],
            replacement_path="ocular.segment classical region_grow/watershed/grabcut",
        ),
        _pending(
            model_id="sam3-class-placeholder",
            family=ModelFamily.PROMPTABLE_SEGMENTATION,
            display_name="SAM 3 class (placeholder intake)",
            version="sam3-unreleased-intake",
            source="https://github.com/facebookresearch/sam2",
            license="unreviewed",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["promptable_mask_candidate"],
            hardware=["cuda>=12"],
            dependencies=["torch", "sam3-when-released"],
            benchmark="ocular-tabletop-segmentation / not-run",
            failure_profile=["unreleased; cannot execute"],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["same class as SAM2 once weights exist"],
            replacement_path="ocular.segment classical contour_parts",
            notes=["Registered so intake tracks the family; no weights exist here."],
        ),
        _pending(
            model_id="vggt-geometry",
            family=ModelFamily.GEOMETRY,
            display_name="VGGT geometry backbone",
            version="vggt@intake",
            source="https://github.com/facebookresearch/vggt",
            license="unreviewed",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["multiview_geometry_candidate"],
            hardware=["cuda>=12"],
            dependencies=["torch"],
            benchmark="ocular-geometry-ensemble / not-run",
            failure_profile=["scale ambiguity", "textureless failure", "license unreviewed"],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["reconstructs private scenes in 3D"],
            replacement_path="COLMAP sparse SfM (CPU) + visual hull",
        ),
        _pending(
            model_id="moge-geometry",
            family=ModelFamily.GEOMETRY,
            display_name="MoGe monocular geometry",
            version="moge@intake",
            source="https://github.com/microsoft/MoGe",
            license="unreviewed",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["monocular_depth_candidate"],
            hardware=["cuda>=11", "cpu-slow"],
            dependencies=["torch"],
            benchmark="ocular-geometry-ensemble / not-run",
            failure_profile=["metric scale unresolved", "specular bias"],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["depth of private spaces"],
            replacement_path="COLMAP sparse + classical depth_fusion when multi-view",
        ),
        _pending(
            model_id="mast3r-geometry",
            family=ModelFamily.GEOMETRY,
            display_name="MASt3R matching/geometry",
            version="mast3r@intake",
            source="https://github.com/naver/mast3r",
            license="CC-BY-NC-4.0",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["pair_matching_candidate", "research_only"],
            hardware=["cuda>=12"],
            dependencies=["torch", "dust3r"],
            benchmark="ocular-geometry-ensemble / not-run",
            failure_profile=["non-commercial license blocks product path", "pair-scale ambiguity"],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["dense matches from private imagery"],
            replacement_path="COLMAP feature matching",
        ),
        _pending(
            model_id="cotracker3-point",
            family=ModelFamily.POINT_TRACKING,
            display_name="CoTracker3 point tracking",
            version="cotracker3@intake",
            source="https://github.com/facebookresearch/co-tracker",
            license="CC-BY-NC-4.0",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["dense_point_track_candidate", "research_only"],
            hardware=["cuda>=12"],
            dependencies=["torch"],
            benchmark="ocular-tabletop-tracking / not-run",
            failure_profile=["occlusion long-horizon drift", "non-commercial license"],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["tracks motion of people/objects in private video"],
            replacement_path="ocular.track classical IoU+histogram+Kalman",
        ),
        _pending(
            model_id="gaussian-splatting",
            family=ModelFamily.RADIANCE,
            display_name="3D Gaussian Splatting",
            version="3dgs@intake",
            source="https://github.com/graphdeco-inria/gaussian-splatting",
            license="unreviewed",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["novel_view_candidate"],
            hardware=["cuda>=11"],
            dependencies=["torch", "diff-gaussian-rasterization"],
            benchmark="ocular-radiance / not-run",
            failure_profile=["editability poor", "metric scale external", "VRAM"],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["photoreal private scene reconstruction"],
            replacement_path="mesh + baked textures from reconstruction portfolio",
        ),
        _pending(
            model_id="vjepa-prediction",
            family=ModelFamily.PREDICTION,
            display_name="V-JEPA video prediction",
            version="vjepa@intake",
            source="https://github.com/facebookresearch/jepa",
            license="unreviewed",
            checkpoint_digest="sha256:pending-local-checkpoint-not-present",
            allowed_use=["future_latent_candidate", "surprise_diagnostic"],
            hardware=["cuda>=12"],
            dependencies=["torch"],
            benchmark="ocular-prediction / not-run",
            failure_profile=["latent not geometric", "no physical authority"],
            authority_ceiling=AuthorityClass.MODEL_DERIVED,
            privacy_implications=["video embeddings of private activity"],
            replacement_path="constant-velocity Kalman + occlusion permanence in ocular.track",
        ),
        # Classical built-ins are always present; still REVIEW_PENDING for the
        # *learned* families only. Classical methods are not model weights and
        # are selected through segment/track modules, not this physical gate.
        _pending(
            model_id="classical-segment-track",
            family=ModelFamily.CLASSICAL,
            display_name="Classical segment+track (local OpenCV)",
            version="ocular-classical-1",
            source="in-tree:blender_vision.ocular.segment/track",
            license="Apache-2.0",
            checkpoint_digest="sha256:none-classical-no-weights",
            allowed_use=[
                "sensor_derived_segmentation",
                "sensor_derived_tracking",
                "object_permanence",
            ],
            hardware=["cpu"],
            dependencies=["numpy", "opencv-python-headless", "scipy", "scikit-image"],
            benchmark="ocular-tabletop-tracking",
            failure_profile=[
                "colour-similar object confusion under pure appearance association",
                "no open-vocabulary concepts without local concept table",
                "GrabCut needs a box; region grow needs seeds",
            ],
            authority_ceiling=AuthorityClass.SENSOR_DERIVED,
            privacy_implications=["operates only on caller-supplied frames; no network"],
            replacement_path="learned families above once accepted with pinned digests",
            notes=[
                "Not a weight checkpoint. Exposed for intake completeness.",
                "Physical selection still requires review_state ACCEPTED and a real file "
                "when treated as a registry model; the segment/track modules run directly.",
            ],
        ),
    ]


class ModelRegistry:
    """In-memory governed registry. Never downloads. Never invents availability."""

    def __init__(self, entries: Iterable[ModelEntry] | None = None) -> None:
        catalog = list(entries) if entries is not None else default_catalog()
        self._by_id: dict[str, ModelEntry] = {}
        for entry in catalog:
            if entry.model_id in self._by_id:
                raise ValidationError(f"duplicate model_id {entry.model_id}")
            self._by_id[entry.model_id] = entry

    def list_entries(self) -> list[ModelEntry]:
        return [self._by_id[key] for key in sorted(self._by_id)]

    def get(self, model_id: str) -> ModelEntry:
        try:
            return self._by_id[model_id]
        except KeyError as exc:
            raise ValidationError(f"unknown model_id {model_id!r}") from exc

    def by_family(self, family: ModelFamily | str) -> list[ModelEntry]:
        resolved = ModelFamily(family)
        return [entry for entry in self.list_entries() if entry.family is resolved]

    def select_for_physical(self, model_id: str) -> ModelEntry:
        """Return an entry only when it may underwrite a physical claim.

        REVIEW_PENDING, missing checkpoints, and rejected licenses all refuse.
        """
        entry = self.get(model_id)
        if entry.selectable_for_physical():
            return entry
        state = entry.backend_state()
        raise PhysicalModelSelectionError(
            f"model {model_id!r} is not selectable for a physical claim: "
            f"review_state={entry.review_state.value} backend_state={state.value} "
            f"checkpoint_path={entry.checkpoint_path!r}"
        )

    def physical_candidates(self) -> list[ModelEntry]:
        return [entry for entry in self.list_entries() if entry.selectable_for_physical()]

    def intake_report(self) -> dict[str, Any]:
        """What would be adopted if review and digests cleared. Download nothing."""
        return {
            "policy": "no-download; REVIEW_PENDING until human acceptance and pinned digest",
            "entries": [entry.to_dict() for entry in self.list_entries()],
            "physical_candidates": [entry.model_id for entry in self.physical_candidates()],
            "families_covered": sorted({entry.family.value for entry in self.list_entries()}),
        }


def default_registry() -> ModelRegistry:
    return ModelRegistry()
