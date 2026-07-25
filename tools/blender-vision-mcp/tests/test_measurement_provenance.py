from __future__ import annotations

import json
from pathlib import Path

import pytest

from blender_vision.acceptance.receipts import evaluate_acceptance
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore


def _source(project: ProjectStore) -> dict:
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture Works", "model": "Governed Box"}
    )
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "https://fixture.invalid/governed-box/specifications",
            "publisher": "Fixture Works",
            "page_title": "Governed Box Specifications",
            "authority_class": "manufacturer_authoritative",
            "target_variant": {"model": "Governed Box"},
            "viewpoint": "official dimensions",
            "quality_score": 1.0,
            "access_policy": {
                "source_terms_review": "approved",
                "privacy_review": "not_applicable",
                "reviewed_by": "Fixture governance reviewer",
                "reviewed_at": "2026-07-21T00:00:00+00:00",
            },
        },
        rights={"status": "INTERNAL", "internal_use": True, "redistribution": False},
        reviewed_by="Fixture governance reviewer",
    )
    fixture = project.root / "governed-box-specifications.html"
    fixture.write_text("<p>Dimensions: 400 × 300 × 200 mm</p>", encoding="utf-8")
    return store.acquire_local(source["id"], fixture)


def test_manufacturer_measurement_provenance_is_receipted_and_tamper_evident(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Measurement provenance")
    source = _source(project)
    store = MeasurementStore(project)
    measurements = [
        store.add(
            "known_overall_dimension",
            {
                "axis": axis,
                "millimetres": millimetres,
                "source_url": source["source"]["origin"],
                "scope": "official rounded envelope",
            },
            evidence_class=EvidenceClass.MANUFACTURER_SPEC,
            uncertainty={"millimetres": 5.0, "classification": "rounded"},
            certainty="bounded",
        )
        for axis, millimetres in (("x", 400.0), ("y", 300.0), ("z", 200.0))
    ]
    before = evaluate_acceptance(project)
    assert (
        "L3+ manufacturer measurements require valid source provenance receipts"
        in before["blockers"]
    )

    receipts = [
        store.bind_source_provenance(
            measurement["id"],
            source_id=source["id"],
            claim_locator="Dimensions table: official rounded envelope",
        )
        for measurement in measurements
    ]
    assert all(receipt["numeric_value_changed"] is False for receipt in receipts)
    after = evaluate_acceptance(project)
    assert (
        "L3+ manufacturer measurements require valid source provenance receipts"
        not in after["blockers"]
    )
    assert after["metrics"]["measurement_provenance"]["invalid_measurement_ids"] == []

    with project.connection() as connection:
        row = connection.execute(
            "SELECT value_json FROM measurements WHERE id=?", (measurements[0]["id"],)
        ).fetchone()
        forged = json.loads(row["value_json"])
        forged["millimetres"] = 401.0
        connection.execute(
            "UPDATE measurements SET value_json=? WHERE id=?",
            (json.dumps(forged), measurements[0]["id"]),
        )
    tampered = evaluate_acceptance(project)
    assert measurements[0]["id"] in tampered["metrics"]["measurement_provenance"][
        "invalid_measurement_ids"
    ]


def test_measurement_provenance_update_is_transactional(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Provenance transaction")
    source = _source(project)
    measurement = MeasurementStore(project).add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 400.0},
        evidence_class=EvidenceClass.MANUFACTURER_SPEC,
        uncertainty={"millimetres": 5.0},
    )
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER reject_measurement_provenance BEFORE UPDATE OF provenance_digest "
            "ON measurements BEGIN SELECT RAISE(ABORT, 'simulated failure'); END"
        )
    with pytest.raises(Exception, match="simulated failure"):
        MeasurementStore(project).bind_source_provenance(
            measurement["id"],
            source_id=source["id"],
            claim_locator="Dimensions table",
        )
    assert MeasurementStore(project).get(measurement["id"])["provenance_digest"] is None


def test_legacy_measurement_source_field_must_match_governed_origin(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Legacy source provenance")
    source = _source(project)
    store = MeasurementStore(project)
    measurement = store.add(
        "known_overall_dimension",
        {
            "axis": "x",
            "millimetres": 400.0,
            "source": "https://unrelated.invalid/specifications",
        },
        evidence_class=EvidenceClass.MANUFACTURER_SPEC,
        uncertainty={"millimetres": 5.0},
    )

    with pytest.raises(ValueError, match="source URL disagrees"):
        store.bind_source_provenance(
            measurement["id"],
            source_id=source["id"],
            claim_locator="Dimensions table",
        )


def test_discovered_source_cannot_create_measurement_provenance(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Discovered source provenance")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture Works", "model": "Governed Box"}
    )
    source = EvidenceAcquisitionStore(project).register_source(
        target["id"],
        {
            "origin": "https://fixture.invalid/governed-box/specifications",
            "publisher": "Fixture Works",
            "page_title": "Governed Box Specifications",
            "authority_class": "manufacturer_authoritative",
            "target_variant": {"model": "Governed Box"},
            "viewpoint": "official dimensions",
            "quality_score": 1.0,
            "access_policy": {
                "source_terms_review": "approved",
                "privacy_review": "not_applicable",
                "reviewed_by": "Fixture governance reviewer",
                "reviewed_at": "2026-07-21T00:00:00+00:00",
            },
        },
        rights={"status": "INTERNAL", "internal_use": True, "redistribution": False},
        reviewed_by="Fixture governance reviewer",
    )
    measurement = MeasurementStore(project).add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 400.0, "source": source["source"]["origin"]},
        evidence_class=EvidenceClass.MANUFACTURER_SPEC,
        uncertainty={"millimetres": 5.0},
    )

    with pytest.raises(ValueError, match="acquired authoritative source bytes"):
        MeasurementStore(project).bind_source_provenance(
            measurement["id"], source_id=source["id"], claim_locator="Dimensions table"
        )
