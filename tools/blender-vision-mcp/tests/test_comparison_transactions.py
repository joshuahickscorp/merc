from __future__ import annotations

import json
import sqlite3
import uuid
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import evaluate_acceptance
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.comparison.metrics import compare_silhouettes
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.util import utc_now
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore


def _comparison_inputs(tmp_path: Path) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(tmp_path / "project", "Comparison transaction")
    reference_path = tmp_path / "reference.png"
    image = Image.new("RGBA", (64, 64), (0, 0, 0, 0))
    ImageDraw.Draw(image).rectangle((12, 12, 51, 51), fill=(255, 255, 255, 255))
    image.save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )
    render_path = tmp_path / "render.png"
    image.save(render_path)
    residual_path = tmp_path / "residual.png"
    metrics = compare_silhouettes(reference_path, render_path, residual_path)
    artifacts = ArtifactStore(project)
    render = artifacts.ingest_file(render_path, media_type="image/png")
    residual = artifacts.ingest_file(residual_path, media_type="image/png")
    return project, {
        "reference": reference,
        "render": render,
        "residual": residual,
        "metrics": metrics,
    }


def _record(project: ProjectStore, inputs: dict, comparison_id: str) -> dict:
    return ComparisonStore(project).record(
        comparison_id,
        reference_id=inputs["reference"]["id"],
        render_digest=inputs["render"].digest,
        residual_digest=inputs["residual"].digest,
        metrics=inputs["metrics"],
        engine="compare_silhouettes_v2",
    )


def test_comparison_receipt_replays_and_metric_tampering_blocks_acceptance(
    tmp_path: Path,
) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    comparison_id = str(uuid.uuid4())
    recorded = _record(project, inputs, comparison_id)
    assert recorded["receipt_artifact"]["digest"]
    assert ComparisonStore(project).verify(comparison_id, replay=True)["valid"] is True

    with project.connection() as connection:
        row = connection.execute(
            "SELECT metrics_json FROM comparisons WHERE id=?", (comparison_id,)
        ).fetchone()
        metrics = json.loads(row["metrics_json"])
        metrics["silhouette_iou"] = 0.75
        connection.execute(
            "UPDATE comparisons SET metrics_json=? WHERE id=?",
            (json.dumps(metrics), comparison_id),
        )

    assert ComparisonStore(project).verify(comparison_id, replay=True)["valid"] is False
    acceptance = evaluate_acceptance(project)
    assert comparison_id in acceptance["metrics"]["comparison_selection"][
        "invalid_comparison_ids"
    ]
    assert "one or more residual comparisons lack a valid replayable receipt" in acceptance[
        "blockers"
    ]


def test_comparison_receipt_rejects_corrupt_render_artifact(tmp_path: Path) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    comparison_id = str(uuid.uuid4())
    _record(project, inputs, comparison_id)
    ArtifactStore(project).path_for(inputs["render"].digest).write_bytes(b"corrupt")

    verification = ComparisonStore(project).verify(comparison_id, replay=True)

    assert verification["valid"] is False


def test_legacy_comparison_migration_requires_exact_replay(tmp_path: Path) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    comparison_id = str(uuid.uuid4())
    created_at = utc_now()
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO comparisons(id,reference_id,render_digest,residual_digest,metrics_json,"
            "created_at) VALUES(?,?,?,?,?,?)",
            (
                comparison_id,
                inputs["reference"]["id"],
                inputs["render"].digest,
                inputs["residual"].digest,
                json.dumps(inputs["metrics"]),
                created_at,
            ),
        )
    store = ComparisonStore(project)
    assert store.verify(comparison_id, replay=True)["valid"] is False

    migrated = store.migrate_legacy(comparison_id)

    assert migrated["valid"] is True
    assert migrated["receipt"]["migration"]["authority"] == (
        "DETERMINISTIC_LEGACY_COMPARISON_REPLAY"
    )
    assert migrated["receipt"]["migration"]["metrics_replayed"] is True


def test_unreproducible_legacy_comparison_can_only_be_receipt_superseded(
    tmp_path: Path,
) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    comparison_id = str(uuid.uuid4())
    forged_metrics = {**inputs["metrics"], "silhouette_iou": 0.25}
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO comparisons(id,reference_id,render_digest,residual_digest,metrics_json,"
            "created_at) VALUES(?,?,?,?,?,?)",
            (
                comparison_id,
                inputs["reference"]["id"],
                inputs["render"].digest,
                inputs["residual"].digest,
                json.dumps(forged_metrics),
                utc_now(),
            ),
        )
    store = ComparisonStore(project)
    with pytest.raises(ValueError, match="cannot be reproduced"):
        store.migrate_legacy(comparison_id)

    result = store.recompute_and_supersede(comparison_id)
    assert result["replacement"]["receipt"]["engine"] == "compare_silhouettes_v3"
    with project.connection() as connection:
        old = dict(
            connection.execute(
                "SELECT * FROM comparisons WHERE id=?", (comparison_id,)
            ).fetchone()
        )
    assert store.verify_supersession(old)["valid"] is True
    assert store.verify(result["replacement"]["id"], replay=True)["valid"] is True
    acceptance = evaluate_acceptance(project)
    assert comparison_id not in acceptance["metrics"]["comparison_selection"][
        "invalid_comparison_ids"
    ]
    assert comparison_id in acceptance["metrics"]["comparison_selection"][
        "validly_superseded_comparison_ids"
    ]

    with project.connection() as connection:
        connection.execute(
            "UPDATE comparisons SET superseded_by_id=? WHERE id=?",
            (comparison_id, comparison_id),
        )
    tampered = evaluate_acceptance(project)
    assert comparison_id in tampered["metrics"]["comparison_selection"][
        "invalid_comparison_ids"
    ]


def test_comparison_insert_failure_leaves_no_authoritative_row(tmp_path: Path) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_comparison_insert BEFORE INSERT ON comparisons "
            "BEGIN SELECT RAISE(ABORT, 'simulated comparison failure'); END"
        )

    with pytest.raises(sqlite3.IntegrityError, match="simulated comparison failure"):
        _record(project, inputs, str(uuid.uuid4()))

    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM comparisons").fetchone()[0] == 0


def test_interrupted_comparison_supersession_rolls_back_replacement_row(
    tmp_path: Path,
) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    comparison_id = str(uuid.uuid4())
    forged_metrics = {**inputs["metrics"], "silhouette_iou": 0.25}
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO comparisons(id,reference_id,render_digest,residual_digest,metrics_json,"
            "created_at) VALUES(?,?,?,?,?,?)",
            (
                comparison_id,
                inputs["reference"]["id"],
                inputs["render"].digest,
                inputs["residual"].digest,
                json.dumps(forged_metrics),
                utc_now(),
            ),
        )
        connection.execute(
            "CREATE TRIGGER fail_comparison_supersession "
            "BEFORE UPDATE OF superseded_by_id ON comparisons "
            "WHEN OLD.id='" + comparison_id + "' "
            "BEGIN SELECT RAISE(ABORT, 'simulated supersession failure'); END"
        )

    with pytest.raises(sqlite3.IntegrityError, match="simulated supersession failure"):
        ComparisonStore(project).recompute_and_supersede(comparison_id)

    with project.connection() as connection:
        rows = connection.execute("SELECT id FROM comparisons").fetchall()
    assert [row["id"] for row in rows] == [comparison_id]


def test_replay_identical_duplicate_is_receipt_collapsed_without_deletion(
    tmp_path: Path,
) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    canonical_id = str(uuid.uuid4())
    duplicate_id = str(uuid.uuid4())
    _record(project, inputs, canonical_id)
    _record(project, inputs, duplicate_id)

    result = ComparisonStore(project).supersede_duplicate(
        duplicate_id, canonical_comparison_id=canonical_id
    )

    assert result["verification"]["valid"] is True
    acceptance = evaluate_acceptance(project)
    selection = acceptance["metrics"]["comparison_selection"]
    assert duplicate_id in selection["validly_superseded_comparison_ids"]
    assert selection["active_comparison_ids"] == [canonical_id]
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM comparisons").fetchone()[0] == 2


def test_comparison_verification_reuses_immutable_reference_segmentation(
    tmp_path: Path, monkeypatch
) -> None:
    from blender_vision.comparison import metrics as comparison_metrics

    project, inputs = _comparison_inputs(tmp_path)
    first_id = str(uuid.uuid4())
    second_id = str(uuid.uuid4())
    _record(project, inputs, first_id)
    _record(project, inputs, second_id)
    original = comparison_metrics._reference_mask
    calls = 0

    def counted(*args, **kwargs):
        nonlocal calls
        calls += 1
        return original(*args, **kwargs)

    monkeypatch.setattr(comparison_metrics, "_reference_mask", counted)
    store = ComparisonStore(project)

    assert store.verify(first_id, replay=True)["valid"] is True
    assert store.verify(second_id, replay=True)["valid"] is True
    assert calls == 1


def test_comparison_store_refuses_forged_but_bounded_metric(tmp_path: Path) -> None:
    project, inputs = _comparison_inputs(tmp_path)
    forged = dict(inputs["metrics"])
    forged["silhouette_iou"] = 0.5

    with pytest.raises(ValueError, match="do not reproduce"):
        ComparisonStore(project).record(
            str(uuid.uuid4()),
            reference_id=inputs["reference"]["id"],
            render_digest=inputs["render"].digest,
            residual_digest=inputs["residual"].digest,
            metrics=forged,
            engine="compare_silhouettes_v2",
        )

    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM comparisons").fetchone()[0] == 0
