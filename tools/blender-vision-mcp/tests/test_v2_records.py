from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from blender_vision.core.errors import ValidationError
from blender_vision.v2.authority import (
    AuthorityClass,
    AuthorityPromotionError,
    CoordinateFrame,
    Uncertainty,
    Units,
    VisibilityState,
    derive,
    to_metres,
    visibility_authority_ceiling,
)
from blender_vision.v2.records import (
    RECORD_TYPES,
    CameraPathGraph,
    CriticFinding,
    DeliveryAsset,
    DeliveryManifest,
    LightingHypothesis,
    LightingHypothesisSet,
    Lineage,
    MaterialHypothesis,
    MaterialHypothesisSet,
    NarrativeBeat,
    NextViewRequest,
    ObservationBundle,
    PerceptualCritique,
    ProceduralSceneGraph,
    ReconstructionCandidate,
    ReconstructionPortfolio,
    SceneEvidenceGraph,
    TamperError,
    load_record,
)
from blender_vision.v2.validation import (
    read_record,
    schema_root,
    validate_payload,
    validate_record,
    verify_payload,
    write_record,
)

REPO = Path(__file__).resolve().parents[1]


def _one_of_each() -> list:
    return [
        ObservationBundle(id="ob-1", target_id="rack"),
        SceneEvidenceGraph(id="seg-1", nodes=[{"id": "n1", "authority": "INFERRED"}]),
        ReconstructionPortfolio(
            id="rp-1",
            target_id="rack",
            candidates=[ReconstructionCandidate(candidate_id="c1", backend="visual_hull")],
        ),
        MaterialHypothesisSet(
            id="mhs-1", hypotheses=[MaterialHypothesis(hypothesis_id="m1", label="alu")]
        ),
        LightingHypothesisSet(
            id="lhs-1", hypotheses=[LightingHypothesis(hypothesis_id="l1")]
        ),
        ProceduralSceneGraph(id="psg-1", scene_name="datacenter"),
        CameraPathGraph(
            id="cpg-1",
            control_points=[[0, 0, 0], [1, 0, 0]],
            beats=[
                NarrativeBeat(beat_id="b0", label="THRESHOLD", scroll_start=0.0, scroll_end=1.0)
            ],
        ),
        PerceptualCritique(
            id="pc-1",
            subject_id="scene",
            findings=[
                CriticFinding(
                    finding_id="f1",
                    critic_role="lighting artist",
                    diagnosis="flat corridor",
                    evidence=["sha256:abc"],
                )
            ],
        ),
        DeliveryManifest(
            id="dm-1",
            source_scene="datacenter",
            assets=[
                DeliveryAsset(
                    asset_id="a1", role="shell", path="shell.glb", digest="0" * 64, bytes=1024
                )
            ],
        ),
        NextViewRequest(id="nvr-1", target_id="remote", reason="underside unseen", priority=8),
    ]


def test_every_canonical_record_is_registered() -> None:
    assert len(RECORD_TYPES) == 10
    assert {type(item) for item in _one_of_each()} == set(RECORD_TYPES.values())


@pytest.mark.parametrize("record", _one_of_each(), ids=lambda item: item.RECORD_KIND)
def test_records_seal_validate_and_round_trip(record, tmp_path: Path) -> None:
    record.seal()
    validate_record(record)
    record.verify()

    payload = record.to_dict()
    restored = verify_payload(payload)
    assert restored.to_dict() == payload

    path = write_record(tmp_path / "record.json", record)
    assert read_record(path).digest == record.digest


@pytest.mark.parametrize("record", _one_of_each(), ids=lambda item: item.RECORD_KIND)
def test_tampering_any_field_is_detected(record) -> None:
    record.seal()
    payload = record.to_dict()
    payload["notes"] = ["injected"]
    with pytest.raises(TamperError):
        verify_payload(payload)


def test_unsealed_record_cannot_be_verified() -> None:
    with pytest.raises(TamperError):
        ObservationBundle(id="ob").verify()


def test_schemas_reject_unknown_properties() -> None:
    record = ObservationBundle(id="ob").seal()
    payload = record.to_dict()
    payload["smuggled"] = True
    with pytest.raises(ValidationError):
        validate_payload(payload)


def test_schemas_enforce_value_ranges() -> None:
    record = MaterialHypothesisSet(
        id="mhs", hypotheses=[MaterialHypothesis(hypothesis_id="m", roughness=4.0)]
    ).seal()
    with pytest.raises(ValidationError):
        validate_record(record)


def test_critic_finding_requires_evidence() -> None:
    with pytest.raises(ValidationError):
        CriticFinding(finding_id="f", critic_role="r", diagnosis="d", evidence=[])


def test_next_view_priority_is_bounded() -> None:
    with pytest.raises(ValidationError):
        NextViewRequest(id="n", target_id="t", reason="r", priority=99)


# ------------------------------------------------------------------ authority


def test_derivation_is_capped_by_the_weakest_input() -> None:
    assert derive(["MEASURED", "OBSERVED"], proposed="MEASURED") == AuthorityClass.OBSERVED
    assert derive(["OBSERVED", "INFERRED"], proposed="OBSERVED") == AuthorityClass.INFERRED
    assert derive([], proposed="MEASURED") == AuthorityClass.HYPOTHETICAL


def test_a_rejected_input_poisons_the_derivation() -> None:
    assert derive(["MEASURED", "REJECTED"], proposed="MEASURED") == AuthorityClass.REJECTED


def test_a_record_cannot_claim_more_than_its_inputs_support() -> None:
    record = ReconstructionPortfolio(
        id="rp",
        authority=AuthorityClass.MEASURED,
        lineage=Lineage(input_authorities=["INFERRED"]),
    )
    with pytest.raises(ValidationError, match="only support INFERRED"):
        record.seal()


def test_promotion_requires_a_named_reviewer_and_reason() -> None:
    record = ObservationBundle(id="ob", authority=AuthorityClass.INFERRED).seal()
    with pytest.raises(AuthorityPromotionError):
        record.promote(AuthorityClass.MEASURED, reviewer="", reason="")
    with pytest.raises(AuthorityPromotionError):
        record.promote(AuthorityClass.MEASURED, reviewer="system", reason="looks right")

    record.promote(AuthorityClass.MEASURED, reviewer="joshua", reason="calipers, 3 samples")
    assert record.authority is AuthorityClass.MEASURED
    record.verify()


def test_downgrades_never_need_a_reviewer() -> None:
    record = ObservationBundle(id="ob", authority=AuthorityClass.OBSERVED).seal()
    record.promote(AuthorityClass.INFERRED, reviewer="", reason="")
    assert record.authority is AuthorityClass.INFERRED


def test_supersession_is_recorded_and_resealed() -> None:
    record = ObservationBundle(id="ob").seal()
    before = record.digest
    record.supersede("ob-2")
    assert record.superseded_by == "ob-2"
    assert record.digest != before
    record.verify()


def test_unobserved_surfaces_cannot_claim_observed_authority() -> None:
    assert visibility_authority_ceiling(VisibilityState.NEVER_OBSERVED) is AuthorityClass.INFERRED
    assert (
        visibility_authority_ceiling(VisibilityState.DIRECTLY_VISIBLE) is AuthorityClass.OBSERVED
    )

    graph = SceneEvidenceGraph(
        id="seg",
        nodes=[
            {"id": "front", "authority": "OBSERVED"},
            {"id": "back", "authority": "OBSERVED"},
        ],
        visibility={
            "front": VisibilityState.DIRECTLY_VISIBLE.value,
            "back": VisibilityState.NEVER_OBSERVED.value,
        },
    )
    assert graph.visibility_violations() == ["back"]


# ------------------------------------------------------------------ geometry


def test_coordinate_frames_refuse_silent_mixing() -> None:
    blender = CoordinateFrame(name="a", up_axis="+Z", forward_axis="-Y")
    gltf = CoordinateFrame(name="b", up_axis="+Y", forward_axis="-Z")
    assert not blender.compatible_with(gltf)
    with pytest.raises(ValidationError):
        blender.require_compatible(gltf)


def test_coordinate_frame_rejects_degenerate_axes() -> None:
    with pytest.raises(ValidationError):
        CoordinateFrame(name="bad", up_axis="+Z", forward_axis="-Z")


def test_unit_conversion_rejects_non_length_units() -> None:
    assert to_metres(2500, Units.MILLIMETRE) == pytest.approx(2.5)
    with pytest.raises(ValidationError):
        to_metres(1.0, Units.PIXEL)


def test_camera_path_reports_dead_scroll_distance() -> None:
    graph = CameraPathGraph(
        id="cpg",
        beats=[
            NarrativeBeat(beat_id="a", label="A", scroll_start=0.0, scroll_end=0.3),
            NarrativeBeat(beat_id="b", label="B", scroll_start=0.5, scroll_end=0.8),
        ],
    )
    assert graph.beat_coverage_gaps() == [(0.3, 0.5), (0.8, 1.0)]


def test_uncertainty_round_trips() -> None:
    value = Uncertainty(kind="dimensional", sigma=0.002, units=Units.METRE, samples=12)
    assert Uncertainty.from_dict(value.to_dict()) == value


# ------------------------------------------------------------------ contracts


def test_committed_schemas_match_the_generator() -> None:
    """The schemas are frozen artifacts; regenerating must be a no-op."""
    before = {
        path.name: path.read_text(encoding="utf-8") for path in sorted(schema_root().glob("*.json"))
    }
    assert len(before) == 10

    subprocess.run(
        [sys.executable, str(REPO / "scripts" / "generate-v2-schemas.py")],
        check=True,
        capture_output=True,
    )
    after = {
        path.name: path.read_text(encoding="utf-8") for path in sorted(schema_root().glob("*.json"))
    }
    assert after == before, "schemas/v2 is stale; re-run scripts/generate-v2-schemas.py"


def test_every_schema_is_valid_draft_2020_12() -> None:
    from jsonschema import Draft202012Validator

    for path in sorted(schema_root().glob("*.json")):
        Draft202012Validator.check_schema(json.loads(path.read_text(encoding="utf-8")))


def test_load_record_rejects_unknown_kinds() -> None:
    with pytest.raises(ValidationError):
        load_record({"record_kind": "v2.not-a-record", "id": "x"})
