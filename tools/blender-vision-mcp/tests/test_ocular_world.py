"""Tests for the Ocular persistent world model and predictive loop."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.predict import (
    PredictionKind,
    evaluate_prediction,
    evaluate_prediction_detailed,
    list_surprises,
    make_prediction,
    predict_next,
    uncertainty_trajectory,
)
from blender_vision.ocular.world import (
    BeliefSlot,
    ChangeClass,
    RelationKind,
    SurfaceProvenance,
    beliefs_bytes,
    build_world_model,
    compare_worlds,
    explain_belief,
    list_uncertainties,
    load_world,
    promote_same_as,
    propose_candidate_same_as,
    query_world,
    same_as_from_candidate_alone,
    save_world,
    update_world_model,
)
from blender_vision.v2.authority import VisibilityState

REPO = Path(__file__).resolve().parents[1]


def _obs(
    frame_index: int,
    entities: list[dict],
    *,
    lighting: dict | None = None,
    absent: bool = False,
    track_source: str = "perception",
) -> dict:
    payload: dict = {
        "frame_index": frame_index,
        "entities": entities,
        "track_source": track_source,
        "absent": absent,
    }
    if lighting is not None:
        payload["lighting"] = lighting
    return payload


def _entity(
    entity_id: str,
    pose: list[float],
    *,
    class_label: str = "mug",
    visible: bool = True,
    appearance: dict | None = None,
) -> dict:
    row: dict = {
        "entity_id": entity_id,
        "track_id": entity_id,
        "class_label": class_label,
        "pose_m": pose,
        "visible": visible,
    }
    if appearance is not None:
        row["appearance"] = appearance
    return row


def test_contradicting_evidence_adds_competing_belief() -> None:
    eid = "trk-0001"
    world = build_world_model(
        [
            _obs(0, [_entity(eid, [0.0, 0.0, 0.1])]),
            _obs(1, [_entity(eid, [0.0, 0.0, 0.11])]),  # within tolerance
        ],
        scene_id="room-a",
    )
    entity = world.entities[eid]
    prior_pose_beliefs = len(entity.all_beliefs(BeliefSlot.POSE.value))
    prior_history = len(world.belief_history)

    # Large jump: competing belief, not overwrite.
    update_world_model(world, _obs(2, [_entity(eid, [1.5, 0.0, 0.1])]))

    beliefs = entity.all_beliefs(BeliefSlot.POSE.value)
    assert len(beliefs) > prior_pose_beliefs
    assert len(world.belief_history) > prior_history

    history = explain_belief(world, eid, BeliefSlot.POSE.value)
    contradictions = [item for item in history if item["contradiction"]]
    assert contradictions, "contradicting observation must record contradiction=True"

    # Prior pose must still be present in the belief set (not silently replaced).
    pose_values = [tuple(item["pose_m"][:3]) for item in beliefs if "pose_m" in item]
    assert (0.0, 0.0, 0.11) in pose_values or (0.0, 0.0, 0.1) in pose_values
    assert (1.5, 0.0, 0.1) in pose_values


def test_belief_history_is_append_only() -> None:
    eid = "trk-0001"
    world = build_world_model(
        [_obs(0, [_entity(eid, [0.0, 0.0, 0.0])]), _obs(1, [_entity(eid, [0.01, 0.0, 0.0])])],
        scene_id="append",
    )
    snapshot = [item.id for item in world.belief_history]
    n = len(snapshot)
    update_world_model(world, _obs(2, [_entity(eid, [0.02, 0.0, 0.0])]))
    assert len(world.belief_history) > n
    # Earlier entries untouched (ids and order preserved as a prefix).
    assert [item.id for item in world.belief_history[:n]] == snapshot


def test_world_reload_across_two_processes_byte_identical(tmp_path: Path) -> None:
    lamp = "trk-0001"
    book = "trk-0002"
    world_path = tmp_path / "world.json"
    world = build_world_model(
        [
            _obs(
                0,
                [
                    _entity(lamp, [0.2, 0.1, 0.5], class_label="lamp"),
                    _entity(book, [0.4, 0.1, 0.05], class_label="book"),
                ],
                lighting={"mean_luminance": 0.55},
            ),
            _obs(
                1,
                [
                    _entity(lamp, [0.2, 0.1, 0.5], class_label="lamp"),
                    _entity(book, [0.41, 0.1, 0.05], class_label="book"),
                ],
                lighting={"mean_luminance": 0.55},
            ),
        ],
        scene_id="persist",
        session_id="s1",
    )
    digest = save_world(world, world_path)
    original_belief_bytes = beliefs_bytes(world)

    # Separate process reloads and re-emits the canonical beliefs slice.
    script = f"""
from pathlib import Path
import sys
from blender_vision.ocular.world import load_world, beliefs_bytes

world = load_world(Path({str(world_path)!r}))
assert world.beliefs_digest() == {digest!r}
sys.stdout.buffer.write(beliefs_bytes(world))
"""
    proc = subprocess.run(
        [sys.executable, "-c", script],
        cwd=str(REPO),
        capture_output=True,
        check=False,
    )
    assert proc.returncode == 0, proc.stderr.decode()
    assert proc.stdout == original_belief_bytes

    reloaded = load_world(world_path)
    assert reloaded.beliefs_digest() == digest
    assert beliefs_bytes(reloaded) == original_belief_bytes
    assert set(reloaded.entities) == set(world.entities)
    assert reloaded.entities[lamp].track_id == world.entities[lamp].track_id


def test_occluded_entity_persists_with_growing_uncertainty() -> None:
    ball = "trk-0001"
    box = "trk-0002"
    world = build_world_model(
        [
            _obs(0, [_entity(ball, [0.0, 0.0, 0.2]), _entity(box, [1.0, 0.0, 0.1])]),
            # ball occluded (missing), box still visible
            _obs(1, [_entity(box, [1.0, 0.0, 0.1])]),
            _obs(2, [_entity(box, [1.0, 0.0, 0.1])]),
            _obs(3, [_entity(box, [1.0, 0.0, 0.1])]),
        ],
        scene_id="occlusion",
    )
    assert ball in world.entities
    ball_ent = world.entities[ball]
    assert ball_ent.frames_since_seen >= 3
    assert ball_ent.confidence < 0.7
    assert (ball_ent.uncertainty.sigma or 0.0) > 0.02
    assert ball_ent.visibility in {
        VisibilityState.INFERRED_SURFACE,
        VisibilityState.PARTIALLY_VISIBLE,
    }
    # Pose retained (persistence through occlusion).
    assert ball_ent.pose_m[0] == pytest.approx(0.0)
    rows = list_uncertainties(world)
    ball_row = next(item for item in rows if item["entity_id"] == ball)
    box_row = next(item for item in rows if item["entity_id"] == box)
    assert ball_row["confidence"] < box_row["confidence"]


def test_absent_frame_grows_uncertainty_without_dropping_identity() -> None:
    vase = "trk-0001"
    world = build_world_model(
        [
            _obs(0, [_entity(vase, [0.3, 0.0, 0.2])]),
            _obs(1, [], absent=True),
            _obs(2, [], absent=True),
        ],
        scene_id="absent",
    )
    vase_ent = world.entities[vase]
    assert vase_ent.entity_id == vase
    assert vase_ent.frames_since_seen >= 2
    assert vase_ent.confidence < 0.7


def test_lighting_only_change_classified_as_appearance_not_geometry() -> None:
    table = "trk-0001"
    chair = "trk-0002"
    session1 = build_world_model(
        [
            _obs(
                0,
                [
                    _entity(table, [0.0, 0.0, 0.0], class_label="table"),
                    _entity(chair, [1.0, 0.0, 0.0], class_label="chair"),
                ],
                lighting={"mean_luminance": 0.4, "temperature_k": 5000},
            )
        ],
        scene_id="room-m",
        session_id="s1",
    )
    session2 = build_world_model(
        [
            _obs(
                0,
                [
                    _entity(table, [0.0, 0.0, 0.0], class_label="table"),
                    _entity(chair, [1.0, 0.0, 0.0], class_label="chair"),
                ],
                lighting={"mean_luminance": 0.85, "temperature_k": 3200},
            )
        ],
        scene_id="room-m",
        session_id="s2",
    )
    report = compare_worlds(session1, session2)
    lighting = report["change_classes"][ChangeClass.LIGHTING_ONLY.value]
    moved = report["change_classes"][ChangeClass.MOVED_OBJECT.value]
    assert lighting["detected"] is True
    assert lighting["geometry_change"] is False
    assert moved["detected"] is False
    assert report["geometry_change"] is False
    assert report["lighting_reported_as_geometry"] is False


def test_dynamic_room_five_change_classes() -> None:
    sofa = "trk-0001"
    lamp = "trk-0002"
    book = "trk-0003"
    plant = "trk-0004"
    s1 = build_world_model(
        [
            _obs(
                0,
                [
                    _entity(sofa, [0.0, 0.0, 0.0], class_label="sofa"),
                    _entity(lamp, [1.0, 0.0, 0.4], class_label="lamp"),
                    _entity(book, [0.5, 0.2, 0.1], class_label="book"),
                ],
                lighting={"mean_luminance": 0.5},
            )
        ],
        scene_id="dyn",
        session_id="s1",
    )
    # Move lamp, remove book, add plant, change lighting.
    s2 = build_world_model(
        [
            _obs(
                0,
                [
                    _entity(sofa, [0.0, 0.0, 0.0], class_label="sofa"),
                    _entity(lamp, [1.8, 0.3, 0.4], class_label="lamp"),
                    _entity(plant, [0.2, -0.5, 0.2], class_label="plant"),
                ],
                lighting={"mean_luminance": 0.9},
            )
        ],
        scene_id="dyn",
        session_id="s2",
    )
    report = compare_worlds(s1, s2)
    classes = report["change_classes"]
    assert classes[ChangeClass.SAME_SCENE.value]["detected"]
    assert classes[ChangeClass.MOVED_OBJECT.value]["detected"]
    assert classes[ChangeClass.REMOVED_OBJECT.value]["detected"]
    assert classes[ChangeClass.NEW_OBJECT.value]["detected"]
    assert classes[ChangeClass.LIGHTING_ONLY.value]["detected"]
    assert classes[ChangeClass.LIGHTING_ONLY.value]["geometry_change"] is False
    moved_ids = [item["entity_id"] for item in classes[ChangeClass.MOVED_OBJECT.value]["items"]]
    removed_ids = [item["entity_id"] for item in classes[ChangeClass.REMOVED_OBJECT.value]["items"]]
    new_ids = [item["entity_id"] for item in classes[ChangeClass.NEW_OBJECT.value]["items"]]
    assert lamp in moved_ids
    assert book in removed_ids
    assert plant in new_ids


def test_surprise_fires_outside_tolerance_not_inside() -> None:
    car = "trk-0001"
    world = build_world_model(
        [
            _obs(0, [_entity(car, [0.0, 0.0, 0.0])]),
            _obs(1, [_entity(car, [0.1, 0.0, 0.0])]),  # v = 0.1 m/frame
        ],
        scene_id="pred",
    )
    preds = predict_next(world, horizon=1, pose_tolerance_m=0.05)
    pose_preds = [p for p in preds if p.kind == PredictionKind.POSE.value and p.entity_id == car]
    assert pose_preds
    pose_pred = pose_preds[0]
    # Expected ≈ [0.2, 0, 0]
    expected = pose_pred.expected["pose_m"]
    assert expected[0] == pytest.approx(0.2, abs=1e-6)

    inside = evaluate_prediction(
        world,
        pose_pred,
        {"pose_m": [0.22, 0.0, 0.0]},
        update_uncertainty=False,
    )
    assert inside is None

    outside = evaluate_prediction(
        world,
        pose_pred,
        {"pose_m": [1.0, 0.0, 0.0]},
        update_uncertainty=False,
    )
    assert outside is not None
    assert outside.fired is True
    assert outside.magnitude > pose_pred.tolerance

    detailed_inside = evaluate_prediction_detailed(
        world,
        pose_pred,
        {"pose_m": [0.21, 0.0, 0.0]},
        update_uncertainty=False,
    )
    assert detailed_inside.fired is False


def test_uncertainty_rises_after_surprise() -> None:
    drone = "trk-0001"
    world = build_world_model(
        [
            _obs(0, [_entity(drone, [0.0, 0.0, 1.0])]),
            _obs(1, [_entity(drone, [0.05, 0.0, 1.0])]),
        ],
        scene_id="unc",
    )
    before = world.entities[drone].confidence
    pred = make_prediction(
        entity_id=drone,
        kind=PredictionKind.POSE.value,
        expected={"pose_m": [0.1, 0.0, 1.0, 1.0, 0.0, 0.0, 0.0]},
        tolerance=0.05,
        valid_from_frame=1,
        tolerance_units="m",
    )
    event = evaluate_prediction(
        world,
        pred,
        {"pose_m": [2.0, 0.0, 1.0]},
        frame_index=2,
        update_uncertainty=True,
    )
    assert event is not None
    after = world.entities[drone].confidence
    assert after < before
    traj = uncertainty_trajectory(world, drone)
    assert any(row["confidence_after"] < row["confidence_before"] for row in traj)
    assert list_surprises(world, entity_id=drone)


def test_uncertainty_falls_after_confirming_evidence() -> None:
    cup = "trk-0001"
    world = build_world_model(
        [_obs(0, [_entity(cup, [0.0, 0.0, 0.1])]), _obs(1, [_entity(cup, [0.0, 0.0, 0.1])])],
        scene_id="confirm",
    )
    # Induce a surprise first.
    pred = make_prediction(
        entity_id=cup,
        kind=PredictionKind.POSE.value,
        expected={"pose_m": [0.0, 0.0, 0.1, 1, 0, 0, 0]},
        tolerance=0.02,
        valid_from_frame=1,
    )
    evaluate_prediction(world, pred, {"pose_m": [1.0, 0.0, 0.1]}, frame_index=2)
    mid = world.entities[cup].confidence
    # Confirming prediction.
    pred2 = make_prediction(
        entity_id=cup,
        kind=PredictionKind.POSE.value,
        expected={"pose_m": [1.0, 0.0, 0.1, 1, 0, 0, 0]},
        tolerance=0.05,
        valid_from_frame=2,
    )
    event = evaluate_prediction_detailed(
        world, pred2, {"pose_m": [1.01, 0.0, 0.1]}, frame_index=3
    )
    assert event.fired is False
    assert world.entities[cup].confidence > mid


def test_same_as_never_inferred_from_candidate_without_evidence() -> None:
    a = "trk-0001"
    b = "trk-0002"
    world = build_world_model(
        [
            _obs(
                0,
                [
                    _entity(a, [0.0, 0.0, 0.0], class_label="mug"),
                    _entity(b, [0.01, 0.0, 0.0], class_label="mug"),
                ],
            )
        ],
        scene_id="id",
    )
    cand = propose_candidate_same_as(world, a, b, confidence=0.6)
    assert cand.kind is RelationKind.CANDIDATE_SAME_AS
    with pytest.raises(ValidationError, match="recorded evidence"):
        same_as_from_candidate_alone(world, cand)
    # Explicit evidence path works.
    rel = promote_same_as(
        world,
        a,
        b,
        evidence=["shared-serial-number-scan", "multi-view-reproj"],
        reviewer="human-reviewer",
    )
    assert rel.kind is RelationKind.SAME_AS
    assert rel.evidence_recorded is True


def test_surface_provenance_classifiable() -> None:
    block = "trk-0001"
    world = build_world_model(
        [
            _obs(
                0,
                [
                    {
                        "entity_id": block,
                        "class_label": "block",
                        "pose_m": [0.0, 0.0, 0.0],
                        "surfaces": [
                            {
                                "surface_id": "block-top",
                                "provenance": SurfaceProvenance.DIRECTLY_OBSERVED.value,
                                "visibility": VisibilityState.DIRECTLY_VISIBLE.value,
                                "centroid_m": [0.0, 0.0, 0.05],
                                "authority": "OBSERVED",
                            },
                            {
                                "surface_id": "block-bottom",
                                "provenance": SurfaceProvenance.SYMMETRY_INFERRED.value,
                                "visibility": VisibilityState.SYMMETRY_DERIVED.value,
                                "centroid_m": [0.0, 0.0, -0.05],
                                "authority": "INFERRED",
                            },
                        ],
                    }
                ],
            )
        ],
        scene_id="surf",
    )
    assert world.surfaces["block-top"].provenance is SurfaceProvenance.DIRECTLY_OBSERVED
    assert world.surfaces["block-bottom"].provenance is SurfaceProvenance.SYMMETRY_INFERRED
    assert "block-top" in world.entities[block].observed_surface_ids
    assert "block-bottom" in world.entities[block].inferred_surface_ids


def test_query_world_and_scene_summary() -> None:
    eid = "trk-0001"
    world = build_world_model(
        [_obs(0, [_entity(eid, [0.0, 0.0, 0.0], class_label="cube")])],
        scene_id="q",
    )
    summary = query_world(world, {"type": "scene_summary"})
    assert summary["n_entities"] == 1
    assert summary["beliefs_digest"]
    hit = query_world(world, {"type": "entity", "entity_id": eid})
    assert hit["found"] is True


def test_camera_motion_and_object_motion_survive() -> None:
    """World retains entities through camera motion and object motion frames."""
    mug = "trk-0001"
    plate = "trk-0002"
    frames = []
    for i in range(5):
        # Camera motion is external; objects shift slowly (object motion).
        frames.append(
            _obs(
                i,
                [
                    _entity(mug, [0.1 * i, 0.0, 0.1]),
                    _entity(plate, [0.5, 0.1 * i, 0.0]),
                ],
            )
        )
    world = build_world_model(frames, scene_id="motion")
    assert set(world.entities) == {mug, plate}
    assert world.entities[mug].pose_m[0] == pytest.approx(0.4)
    assert len(world.entities[mug].trajectory) == 5


def test_prediction_kinds_for_benchmarks() -> None:
    eid = "trk-0001"
    world = build_world_model([_obs(0, [_entity(eid, [0.0, 0.0, 0.0])])], scene_id="bench")
    cases = [
        (
            PredictionKind.BROWSER_ANIMATION.value,
            {"animation_phase": 0.0},
            {"animation_phase": 0.9},
            0.1,
        ),
        (
            PredictionKind.CAMERA_PATH.value,
            {"camera_position": [0.0, 0.0, 1.0]},
            {"camera_position": [0.0, 0.0, 2.5]},
            0.2,
        ),
        (
            PredictionKind.MATERIAL_RESPONSE.value,
            {"specular_peak": 0.2},
            {"specular_peak": 0.9},
            0.1,
        ),
        (
            PredictionKind.EXISTENCE.value,
            {"exists": True},
            {"exists": False},
            0.5,
        ),
    ]
    for kind, expected, observed, tol in cases:
        pred = make_prediction(entity_id=eid, kind=kind, expected=expected, tolerance=tol)
        event = evaluate_prediction(world, pred, observed, update_uncertainty=False)
        assert event is not None, f"{kind} should surprise"
        assert event.magnitude > tol


def test_world_seal_and_verify_round_trip(tmp_path: Path) -> None:
    world = build_world_model(
        [_obs(0, [_entity("trk-0001", [0.0, 0.0, 0.0])])], scene_id="seal", session_id="s"
    )
    world.verify()
    path = tmp_path / "w.json"
    save_world(world, path)
    loaded = load_world(path)
    loaded.verify()
    assert beliefs_bytes(loaded) == beliefs_bytes(world)


def test_same_as_requires_evidence_on_construction() -> None:
    from blender_vision.ocular.world import Relation

    with pytest.raises(ValidationError):
        Relation(
            id="bad",
            relation_id="bad",
            kind=RelationKind.SAME_AS,
            source_id="a",
            target_id="b",
            evidence=[],
            evidence_recorded=False,
        ).seal()


# ---------------------------------------------------------------------------
# Identity provenance gate
# ---------------------------------------------------------------------------


def test_build_world_model_rejects_ground_truth_track_source_by_default() -> None:
    with pytest.raises(ValueError, match="ground-truth identity is forbidden.*cup"):
        build_world_model(
            [
                {
                    "frame_index": 0,
                    "track_source": "ground_truth",
                    "entities": [
                        {
                            "entity_id": "cup",
                            "class_label": "cup",
                            "pose_m": [0.0, 0.0, 0.1, 1, 0, 0, 0],
                        }
                    ],
                }
            ],
            scene_id="gt-reject",
        )


def test_build_world_model_rejects_missing_track_source_by_default() -> None:
    with pytest.raises(ValueError, match="missing track_source"):
        build_world_model(
            [
                {
                    "frame_index": 0,
                    "entities": [
                        {
                            "entity_id": "sofa",
                            "class_label": "sofa",
                            "pose_m": [0.0, 0.0, 0.0, 1, 0, 0, 0],
                        }
                    ],
                }
            ],
            scene_id="silent-reject",
        )


def test_build_world_model_allow_ground_truth_records_provenance() -> None:
    world = build_world_model(
        [
            {
                "frame_index": 0,
                "track_source": "ground_truth",
                "entities": [
                    {
                        "entity_id": "lamp",
                        "class_label": "lamp",
                        "pose_m": [0.0, 0.0, 0.5, 1, 0, 0, 0],
                    }
                ],
            }
        ],
        scene_id="gt-allowed",
        allow_ground_truth=True,
    )
    assert world.meta["identity_provenance"] == "ground_truth"
    assert world.entities["lamp"].identity_provenance["track_source"] == "ground_truth"
    assert world.entities["lamp"].identity_provenance["minted_by"] == "caller"
    assert 0 in world.entities["lamp"].identity_provenance["source_observation_frames"]


def test_tracker_shaped_id_reports_minted_by_tracker() -> None:
    world = build_world_model(
        [_obs(0, [_entity("trk-0042", [0.1, 0.0, 0.0])])],
        scene_id="mint-tracker",
    )
    assert world.meta["identity_provenance"] == "perception"
    entity = world.entities["trk-0042"]
    assert entity.identity_provenance["minted_by"] == "tracker"
    assert entity.identity_provenance["track_source"] == "perception"
    assert entity.identity_provenance["source_observation_frames"] == [0]


def test_entity_belief_record_channels_populated() -> None:
    """Every belief channel is present and queryable without ground truth."""
    eid = "trk-0007"
    world = build_world_model(
        [
            _obs(
                0,
                [
                    {
                        "entity_id": eid,
                        "track_id": eid,
                        "class_label": "block",
                        "pose_m": [0.0, 0.0, 0.1],
                        "visible": True,
                        "detection_id": "det-0-a",
                        "appearance_embedding": [0.1, 0.2, 0.3, 0.4],
                        "bbox_xywh": [10.0, 20.0, 30.0, 40.0],
                        "area_px": 1200.0,
                        "identity_confidence": 0.9,
                        "surfaces": [
                            {
                                "surface_id": "front",
                                "provenance": SurfaceProvenance.DIRECTLY_OBSERVED.value,
                                "visibility": VisibilityState.DIRECTLY_VISIBLE.value,
                                "centroid_m": [0.0, 0.0, 0.1],
                                "authority": "OBSERVED",
                            },
                            {
                                "surface_id": "back",
                                "provenance": SurfaceProvenance.SYMMETRY_INFERRED.value,
                                "visibility": VisibilityState.SYMMETRY_DERIVED.value,
                                "centroid_m": [0.0, 0.0, -0.1],
                                "authority": "INFERRED",
                            },
                        ],
                    }
                ],
            ),
            # Occlusion frame: entity missing → occlusion_state + confidence drop.
            _obs(1, []),
            # Return with a competing pose so contradiction_history grows.
            _obs(
                2,
                [
                    {
                        "entity_id": eid,
                        "track_id": eid,
                        "class_label": "block",
                        "pose_m": [1.5, 0.0, 0.1],
                        "visible": True,
                        "detection_id": "det-2-a",
                        "appearance_embedding": [0.11, 0.19, 0.31, 0.39],
                        "bbox_xywh": [12.0, 22.0, 28.0, 38.0],
                        "area_px": 1064.0,
                        "identity_confidence": 0.8,
                    }
                ],
            ),
        ],
        scene_id="belief-channels",
    )
    entity = world.entities[eid]

    # Source observations name frames + detections/tracks.
    assert entity.source_observations
    assert entity.source_observations[0]["frame_index"] == 0
    assert entity.source_observations[0]["detection_id"] == "det-0-a"
    assert entity.source_observations[0]["track_id"] == eid

    # Appearance embedding is carried, not inventable from a class label.
    assert entity.appearance_embedding == [0.11, 0.19, 0.31, 0.39]

    # Geometry extent / mask summary.
    assert entity.geometry.get("bbox_xywh") == [12.0, 22.0, 28.0, 38.0]
    assert entity.geometry.get("area_px") == pytest.approx(1064.0)

    # Pose packages units + frame.
    assert entity.pose["units"] == "m"
    assert entity.pose["frame"]["name"]
    assert entity.pose["position_m"][0] == pytest.approx(1.5)

    # Trajectory is the observed history, not only the tip.
    assert len(entity.trajectory) >= 3

    # Identity confidence trajectory exists and moved under occlusion.
    assert entity.identity_confidence_history
    confs = [row["identity_confidence"] for row in entity.identity_confidence_history]
    assert confs[0] >= confs[1]  # dropped while unobserved

    # Contradiction on the large pose jump.
    assert entity.contradiction_history
    assert any(row["slot"] == BeliefSlot.POSE.value for row in entity.contradiction_history)

    # Occlusion state was held and then cleared on reappearance.
    assert entity.occlusion_state["state"] in {"visible", "partially_visible"}
    assert entity.frames_since_seen == 0

    # Observed vs inferred surfaces are separable.
    assert "front" in entity.observed_surface_ids
    assert "back" in entity.inferred_surface_ids
    surfaces = query_world(world, {"type": "surfaces", "entity_id": eid})
    assert surfaces["found"] is True
    assert surfaces["surfaces"]["n_observed"] >= 1
    assert surfaces["surfaces"]["n_inferred"] >= 1
    explained = explain_belief(world, eid, "surfaces")
    statuses = {row["status"] for row in explained}
    assert "observed" in statuses
    assert "inferred" in statuses
    belief = query_world(world, {"type": "belief_record", "entity_id": eid})
    assert belief["found"] is True
    assert belief["belief_record"]["appearance_embedding_dim"] == 4
