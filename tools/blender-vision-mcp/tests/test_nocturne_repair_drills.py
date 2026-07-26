from __future__ import annotations

import json
from pathlib import Path

import pytest

from blender_vision.benchmarks.nocturne_repair_drills import (
    NocturneRepairDrillRunner,
    nocturne_repair_drill_ids,
)
from blender_vision.cli.main import build_parser
from blender_vision.core.util import atomic_write_json, sha256_file


def _assertion(identifier: str, observed: object) -> dict[str, object]:
    return {
        "id": identifier,
        "severity": "P0",
        "expected": None,
        "observed": observed,
        "passed": True,
    }


def _receipts(tmp_path: Path) -> tuple[Path, Path]:
    app = {
        "schema_version": "1",
        "source_git_head": "a" * 40,
        "status": "PASS",
        "functional_passed": True,
        "assertions": [
            _assertion(
                "hidden_mobile_trace",
                {
                    "step_count": 10,
                    "final_route": "/receipt",
                    "final_state": "successful_reservation",
                },
            ),
            _assertion("reduced_motion", {"reduced": True, "animation": False}),
            _assertion(
                "glb_size_budgets", {"hero": 393_040, "mobile_lod": 201_484}
            ),
            _assertion(
                "desktop_3d_frames", {"median_fps": 60.0, "p95_ms": 16.0}
            ),
            _assertion(
                "reservation_idempotency",
                {
                    "first": 201,
                    "repeated": 200,
                    "conflict": 409,
                    "same_id": True,
                },
            ),
            _assertion(
                "fresh_clone_commands",
                {
                    name: {"exit_code": 0}
                    for name in (
                        "migration_first",
                        "migration_reapply",
                        "migration_rollback",
                        "migration_second",
                    )
                },
            ),
            _assertion(
                "automated_accessibility",
                {"critical": 0, "serious": 0},
            ),
            _assertion(
                "required_routes_and_states",
                {
                    "routes": [
                        "/",
                        "/technology",
                        "/configurator",
                        "/reserve",
                        "/receipt",
                    ]
                },
            ),
        ],
    }
    three_d = {
        "schema_version": "1",
        "source_git_head": "b" * 40,
        "status": "PASS",
        "functional_passed": True,
        "assertions": [
            _assertion(
                "overall_dimensions",
                {"dimensions_mm": [320.0, 180.0, 360.0]},
            ),
            _assertion(
                "material_classes",
                {"glass_is_transmissive": True},
            ),
            _assertion(
                "fixed_evaluator_cameras",
                {"public_count": 6, "hidden_count": 4},
            ),
            _assertion(
                "deterministic_exploded_animation",
                {
                    "all_required_animated": True,
                    "frame_120_deterministic": True,
                },
            ),
        ],
    }
    app_path = tmp_path / "app.json"
    three_d_path = tmp_path / "3d.json"
    atomic_write_json(app_path, app)
    atomic_write_json(three_d_path, three_d)
    return app_path, three_d_path


def test_fixed_repair_drill_registry_has_all_required_classes() -> None:
    assert nocturne_repair_drill_ids() == (
        "geometry-dimension",
        "incorrect-material-class",
        "fixed-camera-mismatch",
        "broken-mobile-composition",
        "animation-timing-drift",
        "reduced-motion-regression",
        "oversized-glb",
        "shader-frame-time-regression",
        "api-idempotency",
        "database-migration",
        "accessibility",
        "unrelated-route-regression",
    )


def test_repair_drills_detect_rank_and_exactly_restore_all_failures(
    tmp_path: Path,
) -> None:
    app, three_d = _receipts(tmp_path)
    output = tmp_path / "receipt.json"

    receipt = NocturneRepairDrillRunner().run(
        app_receipt_path=app,
        three_d_receipt_path=three_d,
        output_path=output,
    )

    assert receipt.status == "PASS"
    assert receipt.passed_count == 12
    assert receipt.failed_count == 0
    assert output.is_file()
    assert all(drill.detection_passed for drill in receipt.drills)
    assert all(drill.exact_restore for drill in receipt.drills)
    assert all(drill.global_regression_count == 0 for drill in receipt.drills)
    assert all(len(drill.candidates) == 3 for drill in receipt.drills)
    assert all(
        [candidate.candidate_id for candidate in drill.candidates if candidate.selected]
        == ["exact-inverse-patch"]
        for drill in receipt.drills
    )
    assert {
        drill.baseline_receipt_sha256 for drill in receipt.drills if drill.domain == "app"
    } == {sha256_file(app)[0]}


def test_repair_drills_reject_nonpassing_baseline(tmp_path: Path) -> None:
    app, three_d = _receipts(tmp_path)
    payload = json.loads(app.read_text(encoding="utf-8"))
    payload["status"] = "FAIL"
    atomic_write_json(app, payload)

    with pytest.raises(ValueError, match="baseline receipt must be a functional PASS"):
        NocturneRepairDrillRunner().run(
            app_receipt_path=app,
            three_d_receipt_path=three_d,
        )


def test_repair_drill_cli_requires_both_receipts_and_output() -> None:
    args = build_parser().parse_args(
        [
            "benchmark",
            "run-nocturne-repair-drills",
            "--app-receipt",
            "app.json",
            "--three-d-receipt",
            "3d.json",
            "--output",
            "drills.json",
        ]
    )

    assert args.benchmark_command == "run-nocturne-repair-drills"
    assert args.app_receipt == "app.json"
    assert args.three_d_receipt == "3d.json"
