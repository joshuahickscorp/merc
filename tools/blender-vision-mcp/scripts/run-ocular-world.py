#!/usr/bin/env python3
"""Physical execution: persistent world, dynamic-room memory, prediction benchmarks.

Usage:
  .venv/bin/python scripts/run-ocular-world.py --output artifacts/ocular/world

Exit non-zero if lighting-only is reported as geometry, if a belief is
overwritten rather than competed, or if session restart loses identity.

Track source: ground-truth tracks (diagnostic only — not perception identity evidence).
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parents[1]
if str(REPO / "src") not in sys.path:
    sys.path.insert(0, str(REPO / "src"))

from blender_vision.core.util import atomic_write_json, utc_now  # noqa: E402
from blender_vision.ocular.attestation import (  # noqa: E402
    ExecutionClass,
    attest_substitute,
)
from blender_vision.ocular.predict import (  # noqa: E402
    PredictionKind,
    evaluate_prediction,
    evaluate_prediction_detailed,
    list_surprises,
    make_prediction,
    predict_next,
    uncertainty_trajectory,
)
from blender_vision.ocular.world import (  # noqa: E402
    BeliefSlot,
    ChangeClass,
    WorldState,
    beliefs_bytes,
    build_world_model as _build_world_model,
    compare_worlds,
    explain_belief,
    list_uncertainties,
    load_world,
    save_world,
    update_world_model as _update_world_model,
)


def build_world_model(*args: Any, **kwargs: Any) -> WorldState:
    """Diagnostic wrapper: always allow ground-truth identity for this script."""
    kwargs["allow_ground_truth"] = True
    return _build_world_model(*args, **kwargs)


def update_world_model(*args: Any, **kwargs: Any) -> WorldState:
    """Diagnostic wrapper: always allow ground-truth identity for this script."""
    kwargs["allow_ground_truth"] = True
    return _update_world_model(*args, **kwargs)


def _ok(msg: str) -> None:
    print(f"OK: {msg}")


def _fail(msg: str, code: int = 1) -> None:
    print(f"FAIL: {msg}", file=sys.stderr)
    raise SystemExit(code)


def _section(title: str) -> None:
    print()
    print("=" * 72)
    print(title)
    print("=" * 72)


def _entity(
    entity_id: str,
    pose: list[float],
    *,
    class_label: str = "object",
    visible: bool = True,
    appearance: dict[str, Any] | None = None,
) -> dict[str, Any]:
    row: dict[str, Any] = {
        "entity_id": entity_id,
        "track_id": entity_id,
        "class_label": class_label,
        "pose_m": pose if len(pose) >= 7 else list(pose[:3]) + [1.0, 0.0, 0.0, 0.0],
        "visible": visible,
    }
    if appearance is not None:
        row["appearance"] = appearance
    return row


def _obs(
    frame_index: int,
    entities: list[dict[str, Any]],
    *,
    lighting: dict[str, Any] | None = None,
    absent: bool = False,
    camera_position: list[float] | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "frame_index": frame_index,
        "entities": entities,
        "track_source": "ground_truth",
        "absent": absent,
    }
    if lighting is not None:
        payload["lighting"] = lighting
    if camera_position is not None:
        payload["camera_position"] = camera_position
    return payload


def load_or_build_tabletop_sequence(output: Path) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    """Consume benchmarks/ocular_tabletop if present; else procedural GT sequence.

    Reports track_source=ground_truth. Blender render is optional attestation.
    The dynamic-room fixture lives under benchmarks/ocular_room and is not used here.
    """
    tabletop = REPO / "benchmarks" / "ocular_tabletop"
    meta: dict[str, Any] = {"track_source": "ground_truth"}

    if (tabletop / "observations.json").is_file():
        observations = json.loads((tabletop / "observations.json").read_text(encoding="utf-8"))
        meta["source"] = str(tabletop / "observations.json")
        meta["execution"] = attest_substitute(
            "tabletop-sequence",
            execution_class=ExecutionClass.CANDIDATE_ONLY,
            reason="pre-authored tabletop observations on disk",
            substitute="benchmarks/ocular_tabletop",
        ).to_dict()
        return observations, meta

    # Synthetic tabletop: camera motion, object motion, occlusion, absent frame.
    meta["source"] = "procedural_tabletop_gt"
    meta["execution"] = attest_substitute(
        "blender-tabletop",
        execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
        reason=(
            "benchmarks/ocular_tabletop absent; driving world from ground-truth "
            "tracks (segmentation/tracking is a separate task)"
        ),
        substitute="ground_truth_tracks",
    ).to_dict()
    observations = _procedural_tabletop()
    atomic_write_json(output / "procedural_tabletop_observations.json", observations)
    return observations, meta


def _procedural_tabletop() -> list[dict[str, Any]]:
    frames: list[dict[str, Any]] = []
    # Frames 0-2: camera orbits, both objects visible, cup drifts slowly.
    for i in range(3):
        frames.append(
            _obs(
                i,
                [
                    _entity("cup", [0.05 * i, 0.0, 0.08], class_label="cup"),
                    _entity("bowl", [0.35, 0.1, 0.06], class_label="bowl"),
                ],
                lighting={"mean_luminance": 0.55},
                camera_position=[0.8 * (1 - 0.1 * i), -0.6, 0.5],
            )
        )
    # Frame 3: cup occluded (missing), bowl visible.
    frames.append(
        _obs(
            3,
            [_entity("bowl", [0.35, 0.1, 0.06], class_label="bowl")],
            lighting={"mean_luminance": 0.55},
            camera_position=[0.5, -0.6, 0.5],
        )
    )
    # Frame 4: absent frame (no sensor delivery).
    frames.append(_obs(4, [], absent=True))
    # Frame 5: both visible again; cup moved further (object motion).
    frames.append(
        _obs(
            5,
            [
                _entity("cup", [0.25, 0.05, 0.08], class_label="cup"),
                _entity("bowl", [0.36, 0.1, 0.06], class_label="bowl"),
            ],
            lighting={"mean_luminance": 0.55},
            camera_position=[0.4, -0.5, 0.55],
        )
    )
    return frames


def run_survival(observations: list[dict[str, Any]], output: Path) -> WorldState:
    _section("1. World survival: camera motion, object motion, occlusion, absent frame")
    world = build_world_model(observations, scene_id="tabletop", session_id="live")
    print(f"track_source: {world.meta.get('track_source', 'unknown')}")
    print(f"identity_provenance: {world.meta.get('identity_provenance', 'unknown')}")
    print(
        "DIAGNOSTIC: output is not evidence of perception identity "
        "(allow_ground_truth=True; pre-labelled tracks)."
    )
    print(f"entities: {sorted(world.entities)}")
    print(f"frames: 0..{world.current_frame}")
    for frame in range(world.current_frame + 1):
        print(f"  frame {frame}:")
        for eid, entity in sorted(world.entities.items()):
            last = next(
                (t for t in reversed(entity.trajectory) if t.get("frame_index") == frame),
                None,
            )
            if last is None:
                continue
            conf = last.get("confidence", entity.confidence)
            pose3 = [round(v, 3) for v in last["pose_m"][:3]]
            print(
                f"    {eid}: pose={pose3} visible={last.get('visible')} "
                f"conf={conf:.3f} reason={last.get('reason', '')}"
            )

    cup = world.entities.get("cup")
    if cup is None:
        _fail("expected cup entity from tabletop sequence")
    if cup.frames_since_seen < 0:
        _fail("cup identity lost")
    # After occlusion + absent, cup must still exist.
    if "cup" not in world.entities:
        _fail("occluded/absent cup was dropped from the world")
    _ok("world retained entities through motion, occlusion, and absent frame")

    # Competing belief check (must not overwrite).
    prior_n = len(world.entities["cup"].all_beliefs(BeliefSlot.POSE.value))
    prior_hist = [item.id for item in world.belief_history]
    update_world_model(
        world,
        _obs(world.current_frame + 1, [_entity("cup", [2.0, 0.0, 0.08], class_label="cup")]),
    )
    after_beliefs = world.entities["cup"].all_beliefs(BeliefSlot.POSE.value)
    if len(after_beliefs) <= prior_n:
        _fail("contradicting pose did not add a competing belief")
    if [item.id for item in world.belief_history[: len(prior_hist)]] != prior_hist:
        _fail("belief history was rewritten rather than appended")
    contradictions = explain_belief(world, "cup", BeliefSlot.POSE.value)
    if not any(item["contradiction"] for item in contradictions):
        _fail("contradiction flag missing on competing belief")
    _ok("competing belief appended; history not overwritten")

    atomic_write_json(output / "survival_world.json", world.to_dict())
    return world


def run_session_restart(world: WorldState, output: Path) -> None:
    _section("2. Session restart: write, exit process, reload, compare digests")
    path = output / "world_checkpoint.json"
    digest_a = save_world(world, path)
    print(f"process_a beliefs_digest: {digest_a}")

    script = f"""
from pathlib import Path
from blender_vision.ocular.world import load_world, beliefs_bytes
import sys
world = load_world(Path({str(path)!r}))
digest = world.beliefs_digest()
blob = beliefs_bytes(world)
sys.stdout.buffer.write(digest.encode("ascii"))
sys.stdout.buffer.write(b"\\n--BYTES--\\n")
sys.stdout.buffer.write(blob)
"""
    proc = subprocess.run(
        [sys.executable, "-c", script],
        cwd=str(REPO),
        capture_output=True,
        check=False,
        env={**os.environ, "PYTHONPATH": str(REPO / "src")},
    )
    if proc.returncode != 0:
        _fail(f"reload process failed: {proc.stderr.decode()}")
    stdout = proc.stdout
    text, sep, blob = stdout.partition(b"\n--BYTES--\n")
    if not sep:
        _fail(f"reload process produced no digest separator: {stdout[:200]!r}")
    digest_b = text.decode("ascii").strip()
    print(f"process_b beliefs_digest: {digest_b}")
    if digest_a != digest_b:
        _fail(f"session restart lost identity: {digest_a} != {digest_b}")
    if blob != beliefs_bytes(world):
        _fail("beliefs bytes not identical across processes")
    reloaded = load_world(path)
    if set(reloaded.entities) != set(world.entities):
        _fail("entity set changed across restart")
    for eid in world.entities:
        if reloaded.entities[eid].track_id != world.entities[eid].track_id:
            _fail(f"track identity changed for {eid}")
    _ok("session restart preserved byte-identical beliefs and identity")


def run_dynamic_room(output: Path) -> dict[str, Any]:
    _section("3. Phase M dynamic-room memory benchmark")
    bench = REPO / "benchmarks" / "ocular_room"
    s1_path = bench / "session1_observations.json"
    s2_path = bench / "session2_observations.json"
    if s1_path.is_file() and s2_path.is_file():
        s1_obs = json.loads(s1_path.read_text(encoding="utf-8"))
        s2_obs = json.loads(s2_path.read_text(encoding="utf-8"))
        source = "benchmarks/ocular_room"
    else:
        source = "inline_dynamic_room"
        s1_obs = [
            _obs(
                0,
                [
                    _entity("sofa", [0.0, 0.0, 0.0], class_label="sofa"),
                    _entity("lamp", [1.2, 0.0, 0.4], class_label="lamp"),
                    _entity("book", [0.4, 0.3, 0.1], class_label="book"),
                    _entity("table", [0.0, 0.8, 0.0], class_label="table"),
                ],
                lighting={"mean_luminance": 0.45, "temperature_k": 5500},
            )
        ]
        s2_obs = [
            _obs(
                0,
                [
                    _entity("sofa", [0.0, 0.0, 0.0], class_label="sofa"),
                    # moved
                    _entity("lamp", [2.0, 0.4, 0.4], class_label="lamp"),
                    # book removed
                    # plant added
                    _entity("plant", [-0.5, 0.2, 0.15], class_label="plant"),
                    _entity("table", [0.0, 0.8, 0.0], class_label="table"),
                ],
                lighting={"mean_luminance": 0.92, "temperature_k": 3000},
            )
        ]

    session1 = build_world_model(s1_obs, scene_id="dynamic-room", session_id="session-1")
    session2 = build_world_model(s2_obs, scene_id="dynamic-room", session_id="session-2")
    report = compare_worlds(session1, session2)
    report["source"] = source
    report["session1_digest"] = session1.beliefs_digest()
    report["session2_digest"] = session2.beliefs_digest()

    classes = report["change_classes"]
    required = [
        ChangeClass.SAME_SCENE.value,
        ChangeClass.MOVED_OBJECT.value,
        ChangeClass.REMOVED_OBJECT.value,
        ChangeClass.NEW_OBJECT.value,
        ChangeClass.LIGHTING_ONLY.value,
    ]
    for name in required:
        block = classes[name]
        status = "DETECTED" if block["detected"] else "MISSING"
        print(f"  {name}: {status} confidence={block.get('confidence', 0):.3f}")
        for ev in (block.get("evidence") or [])[:4]:
            print(f"    evidence: {ev}")
        if not block["detected"]:
            _fail(f"dynamic-room failed to report {name}")

    lighting = classes[ChangeClass.LIGHTING_ONLY.value]
    if lighting.get("geometry_change"):
        _fail("lighting-only change reported as geometry change")
    if report.get("lighting_reported_as_geometry"):
        _fail("lighting_reported_as_geometry flag set")
    _ok("all five change classes reported; lighting is not geometry")

    save_world(session1, output / "dynamic_session1.json")
    save_world(session2, output / "dynamic_session2.json")
    atomic_write_json(output / "dynamic_room_report.json", report)
    return report


def run_prediction_benchmarks(output: Path) -> list[dict[str, Any]]:
    _section("4. Six prediction benchmarks")
    results: list[dict[str, Any]] = []

    # 1. Expected motion — constant velocity, observation matches.
    world = build_world_model(
        [
            _obs(0, [_entity("slider", [0.0, 0.0, 0.0], class_label="slider")]),
            _obs(1, [_entity("slider", [0.1, 0.0, 0.0], class_label="slider")]),
        ],
        scene_id="pred-expected",
    )
    preds = predict_next(world, horizon=1, pose_tolerance_m=0.05)
    pose_pred = next(p for p in preds if p.kind == PredictionKind.POSE.value)
    obs_pose = pose_pred.expected["pose_m"]
    event = evaluate_prediction_detailed(
        world, pose_pred, {"pose_m": obs_pose}, frame_index=2, update_uncertainty=True
    )
    results.append(
        _pred_row(
            "expected_motion",
            pose_pred,
            {"pose_m": obs_pose},
            event,
            expect_surprise=False,
            conf_before=None,
            conf_after=world.entities["slider"].confidence,
        )
    )

    # 2. Unexpected moved object.
    world2 = build_world_model(
        [
            _obs(0, [_entity("box", [0.0, 0.0, 0.0])]),
            _obs(1, [_entity("box", [0.0, 0.0, 0.0])]),
        ],
        scene_id="pred-unexpected",
    )
    before = world2.entities["box"].confidence
    pred = make_prediction(
        entity_id="box",
        kind=PredictionKind.POSE.value,
        expected={"pose_m": [0.0, 0.0, 0.0, 1, 0, 0, 0]},
        tolerance=0.05,
        valid_from_frame=1,
        tolerance_units="m",
    )
    observed = {"pose_m": [1.2, 0.0, 0.0, 1, 0, 0, 0]}
    event = evaluate_prediction_detailed(world2, pred, observed, frame_index=2)
    results.append(
        _pred_row(
            "unexpected_moved_object",
            pred,
            observed,
            event,
            expect_surprise=True,
            conf_before=before,
            conf_after=world2.entities["box"].confidence,
        )
    )

    # 3. Missing object.
    world3 = build_world_model(
        [_obs(0, [_entity("key", [0.2, 0.0, 0.0])]), _obs(1, [_entity("key", [0.2, 0.0, 0.0])])],
        scene_id="pred-missing",
    )
    before = world3.entities["key"].confidence
    pred = make_prediction(
        entity_id="key",
        kind=PredictionKind.EXISTENCE.value,
        expected={"exists": True},
        tolerance=0.5,
        valid_from_frame=1,
    )
    observed = {"exists": False}
    event = evaluate_prediction_detailed(world3, pred, observed, frame_index=2)
    results.append(
        _pred_row(
            "missing_object",
            pred,
            observed,
            event,
            expect_surprise=True,
            conf_before=before,
            conf_after=world3.entities["key"].confidence,
        )
    )

    # 4. Wrong browser animation.
    world4 = build_world_model(
        [_obs(0, [_entity("hero", [0.0, 0.0, 0.0])])],
        scene_id="pred-browser",
    )
    before = world4.entities["hero"].confidence
    pred = make_prediction(
        entity_id="hero",
        kind=PredictionKind.BROWSER_ANIMATION.value,
        expected={"animation_phase": 0.25},
        tolerance=0.1,
        valid_from_frame=0,
        model="scroll_timeline",
    )
    observed = {"animation_phase": 0.95}
    event = evaluate_prediction_detailed(world4, pred, observed, frame_index=1)
    results.append(
        _pred_row(
            "wrong_browser_animation",
            pred,
            observed,
            event,
            expect_surprise=True,
            conf_before=before,
            conf_after=world4.entities["hero"].confidence,
        )
    )

    # 5. Camera-path mismatch.
    world5 = build_world_model([_obs(0, [_entity("scene", [0.0, 0.0, 0.0])])], scene_id="pred-cam")
    before = world5.entities["scene"].confidence
    pred = make_prediction(
        entity_id="scene",
        kind=PredictionKind.CAMERA_PATH.value,
        expected={"camera_position": [0.0, -1.0, 0.5]},
        tolerance=0.15,
        valid_from_frame=0,
        model="authored_path",
        tolerance_units="m",
    )
    observed = {"camera_position": [0.0, -1.0, 2.0]}
    event = evaluate_prediction_detailed(world5, pred, observed, frame_index=1)
    results.append(
        _pred_row(
            "camera_path_mismatch",
            pred,
            observed,
            event,
            expect_surprise=True,
            conf_before=before,
            conf_after=world5.entities["scene"].confidence,
        )
    )

    # 6. Material response mismatch.
    world6 = build_world_model(
        [
            _obs(
                0,
                [
                    _entity(
                        "metal",
                        [0.0, 0.0, 0.0],
                        appearance={"specular_peak": 0.3},
                    )
                ],
            )
        ],
        scene_id="pred-mat",
    )
    before = world6.entities["metal"].confidence
    pred = make_prediction(
        entity_id="metal",
        kind=PredictionKind.MATERIAL_RESPONSE.value,
        expected={"specular_peak": 0.3},
        tolerance=0.1,
        valid_from_frame=0,
        model="brdf_response",
    )
    observed = {"specular_peak": 0.95}
    event = evaluate_prediction_detailed(world6, pred, observed, frame_index=1)
    results.append(
        _pred_row(
            "material_response_mismatch",
            pred,
            observed,
            event,
            expect_surprise=True,
            conf_before=before,
            conf_after=world6.entities["metal"].confidence,
        )
    )

    for row in results:
        print(
            f"  {row['name']}: predicted={row['predicted']} observed={row['observed']} "
            f"magnitude={row['magnitude']:.4f} tolerance={row['tolerance']:.4f} "
            f"surprise={row['surprise']}"
        )
        if row["expect_surprise"] and not row["surprise"]:
            _fail(f"{row['name']} should have fired surprise")
        if not row["expect_surprise"] and row["surprise"]:
            _fail(f"{row['name']} should not have fired surprise")

    _ok("six prediction benchmarks completed")
    atomic_write_json(output / "prediction_benchmarks.json", results)
    return results


def _pred_row(
    name: str,
    pred: Any,
    observed: dict[str, Any],
    event: Any,
    *,
    expect_surprise: bool,
    conf_before: float | None,
    conf_after: float | None,
) -> dict[str, Any]:
    return {
        "name": name,
        "kind": pred.kind,
        "predicted": pred.expected,
        "observed": observed,
        "magnitude": event.magnitude,
        "tolerance": pred.tolerance,
        "surprise": event.fired,
        "expect_surprise": expect_surprise,
        "confidence_before": conf_before,
        "confidence_after": conf_after,
    }


def run_uncertainty_trajectory(output: Path) -> list[dict[str, Any]]:
    _section("5. Uncertainty trajectory: rise after surprise, fall after confirmation")
    world = build_world_model(
        [
            _obs(0, [_entity("probe", [0.0, 0.0, 0.0])]),
            _obs(1, [_entity("probe", [0.05, 0.0, 0.0])]),
        ],
        scene_id="unc-traj",
    )
    c0 = world.entities["probe"].confidence
    # Surprise
    pred = make_prediction(
        entity_id="probe",
        kind=PredictionKind.POSE.value,
        expected={"pose_m": [0.1, 0.0, 0.0, 1, 0, 0, 0]},
        tolerance=0.05,
        valid_from_frame=1,
        tolerance_units="m",
    )
    evaluate_prediction(world, pred, {"pose_m": [1.5, 0.0, 0.0]}, frame_index=2)
    c1 = world.entities["probe"].confidence
    # Confirmation
    pred2 = make_prediction(
        entity_id="probe",
        kind=PredictionKind.POSE.value,
        expected={"pose_m": [1.5, 0.0, 0.0, 1, 0, 0, 0]},
        tolerance=0.05,
        valid_from_frame=2,
        tolerance_units="m",
    )
    evaluate_prediction_detailed(
        world, pred2, {"pose_m": [1.51, 0.0, 0.0]}, frame_index=3
    )
    c2 = world.entities["probe"].confidence
    traj = uncertainty_trajectory(world, "probe")
    print(f"  confidence: start={c0:.3f} after_surprise={c1:.3f} after_confirm={c2:.3f}")
    for row in traj:
        print(
            f"    frame={row['frame_index']} model={row['model']} "
            f"{row['confidence_before']:.3f}->{row['confidence_after']:.3f} "
            f"contradiction={row['contradiction']}"
        )
    if not (c1 < c0):
        _fail("uncertainty did not rise (confidence drop) after surprise")
    if not (c2 > c1):
        _fail("uncertainty did not fall (confidence rise) after confirmation")
    _ok("uncertainty rose after surprise and fell after confirming evidence")
    atomic_write_json(
        output / "uncertainty_trajectory.json",
        {"start": c0, "after_surprise": c1, "after_confirm": c2, "trajectory": traj},
    )
    print("uncertainties:", json.dumps(list_uncertainties(world), indent=2))
    print("surprises:", json.dumps(list_surprises(world), indent=2)[:500])
    return traj


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=REPO / "artifacts" / "ocular" / "world",
    )
    args = parser.parse_args()
    output: Path = args.output
    output.mkdir(parents=True, exist_ok=True)

    print("Ocular world model + predictive loop")
    print(f"output: {output}")
    print(f"started: {utc_now()}")
    print("note: driving from ground-truth tracks (no live segmenter/tracker in this task)")
    print(
        "DIAGNOSTIC ONLY: this script's output is NOT evidence of perception identity. "
        "Worlds are built with allow_ground_truth=True; identity_provenance=ground_truth."
    )

    observations, meta = load_or_build_tabletop_sequence(output)
    atomic_write_json(output / "sequence_meta.json", meta)
    print(f"sequence source: {meta.get('source')}")
    print(f"execution_class: {meta.get('execution', {}).get('execution_class')}")

    world = run_survival(observations, output)
    run_session_restart(world, output)
    dynamic = run_dynamic_room(output)
    predictions = run_prediction_benchmarks(output)
    trajectory = run_uncertainty_trajectory(output)

    receipt = {
        "status": "PASS",
        "started_at": utc_now(),
        "track_source": "ground_truth",
        "identity_provenance": world.meta.get("identity_provenance", "ground_truth"),
        "diagnostic_only": True,
        "not_perception_identity_evidence": True,
        "sequence_meta": meta,
        "beliefs_digest": world.beliefs_digest(),
        "n_entities": len(world.entities),
        "n_belief_updates": len(world.belief_history),
        "dynamic_room": {
            name: {
                "detected": block["detected"],
                "confidence": block.get("confidence"),
                "geometry_change": block.get("geometry_change"),
            }
            for name, block in dynamic["change_classes"].items()
        },
        "prediction_benchmarks": [
            {
                "name": row["name"],
                "magnitude": row["magnitude"],
                "surprise": row["surprise"],
            }
            for row in predictions
        ],
        "uncertainty_trajectory_points": len(trajectory),
    }
    atomic_write_json(output / "receipt.json", receipt)

    _section("RECEIPT")
    print(json.dumps(receipt, indent=2, sort_keys=True))
    _ok("all ocular world gates passed")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except BrokenPipeError:
        raise SystemExit(1) from None
