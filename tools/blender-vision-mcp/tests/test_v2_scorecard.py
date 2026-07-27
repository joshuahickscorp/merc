from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCORECARD = ROOT / "artifacts" / "v2" / "final-scorecard.json"
RECEIPT = ROOT / "artifacts" / "v2" / "final-scorecard.receipt.json"
VERIFIER = ROOT / "scripts" / "verify-final-scorecard.py"
MARKDOWN = ROOT / "docs" / "v2" / "VISIONMCP_V2_FINAL_SCORECARD.md"

REQUIRED_IDS = [
    "perception",
    "calibration",
    "depth",
    "point_clouds",
    "geometry_reconstruction",
    "hidden_geometry",
    "procedural_generation",
    "topology",
    "uv",
    "textures",
    "materials",
    "lighting",
    "reflections",
    "organic_geometry",
    "anatomy",
    "fur",
    "scene_composition",
    "camera_paths",
    "web_compilation",
    "lod",
    "streaming",
    "performance",
    "accessibility",
    "provenance",
    "security",
    "repair",
    "perceptual_quality",
    "generalization",
]

KNOWN_OPEN = [
    "uv-packing-branching-forms",
    "parity-roughness-blindness",
    "colmap-dense-mvs-no-cuda",
    "no-authorized-real-animal-capture",
    "no-user-supplied-consumer-object-photographs",
    "mac-studio-owned-blend-absent",
    "flagship-film-not-art-directed",
]


def test_scorecard_file_exists() -> None:
    assert SCORECARD.is_file(), "artifacts/v2/final-scorecard.json must exist"
    assert MARKDOWN.is_file(), "docs/v2/VISIONMCP_V2_FINAL_SCORECARD.md must exist"
    assert VERIFIER.is_file(), "scripts/verify-final-scorecard.py must exist"


def test_scorecard_has_twenty_eight_facets_with_required_fields() -> None:
    data = json.loads(SCORECARD.read_text(encoding="utf-8"))
    facets = data["facets"]
    assert len(facets) == 28
    assert [facet["id"] for facet in facets] == REQUIRED_IDS
    for facet in facets:
        for field in (
            "baseline",
            "final",
            "implementation_evidence",
            "runtime_evidence",
            "external_heldout_evidence",
            "failed_attempts",
            "limitations",
            "reproduction_command",
            "remaining_blocker",
        ):
            assert field in facet, f"{facet['id']} missing {field}"
        assert 0 <= facet["baseline"] <= 110
        assert 0 <= facet["final"] <= 110
        assert facet["final"] < 100, f"{facet['id']} must not claim 100 without proof fields"
        assert facet["runtime_evidence"], f"{facet['id']} needs runtime evidence"
        assert facet["implementation_evidence"]


def test_known_open_failures_present() -> None:
    data = json.loads(SCORECARD.read_text(encoding="utf-8"))
    ids = {item["id"] for item in data["known_open_failures"]}
    for required in KNOWN_OPEN:
        assert required in ids


def test_verifier_passes_and_writes_receipt() -> None:
    completed = subprocess.run(
        [sys.executable, str(VERIFIER), "--scorecard", str(SCORECARD), "--receipt", str(RECEIPT)],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert completed.returncode == 0, completed.stdout + completed.stderr
    assert "VERIFY PASS" in completed.stdout
    assert RECEIPT.is_file()
    receipt = json.loads(RECEIPT.read_text(encoding="utf-8"))
    assert receipt["passed"] is True
    assert receipt["failure_count"] == 0
    assert receipt["scorecard_sha256"]


def test_verifier_fails_on_unbacked_numeric_claim(tmp_path: Path) -> None:
    data = json.loads(SCORECARD.read_text(encoding="utf-8"))
    # Poison one numeric claim so the verifier must reject the scorecard.
    data["facets"][0]["runtime_evidence"][0]["numeric_claims"] = [999999.123456789]
    poisoned = tmp_path / "poisoned-scorecard.json"
    poisoned.write_text(json.dumps(data), encoding="utf-8")
    receipt = tmp_path / "poisoned.receipt.json"
    completed = subprocess.run(
        [
            sys.executable,
            str(VERIFIER),
            "--root",
            str(ROOT),
            "--scorecard",
            str(poisoned),
            "--receipt",
            str(receipt),
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    combined = completed.stdout + completed.stderr
    assert completed.returncode != 0, combined
    assert "VERIFY FAIL" in combined
    assert "numeric claim" in combined


def test_verifier_fails_on_score_100_without_proof(tmp_path: Path) -> None:
    data = json.loads(SCORECARD.read_text(encoding="utf-8"))
    data["facets"][0]["final"] = 100
    data["summary"]["facets_at_or_above_100"] = 1
    data["facets"][0].pop("reference_class_proof", None)
    poisoned = tmp_path / "score-100.json"
    poisoned.write_text(json.dumps(data), encoding="utf-8")
    receipt = tmp_path / "score-100.receipt.json"
    completed = subprocess.run(
        [
            sys.executable,
            str(VERIFIER),
            "--root",
            str(ROOT),
            "--scorecard",
            str(poisoned),
            "--receipt",
            str(receipt),
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    combined = completed.stdout + completed.stderr
    assert completed.returncode != 0, combined
    assert "reference_class_proof" in combined


def test_blocker_requirement_strings_are_exact() -> None:
    data = json.loads(SCORECARD.read_text(encoding="utf-8"))
    for facet in data["facets"]:
        blocker = facet["remaining_blocker"]
        if blocker is None:
            continue
        assert isinstance(blocker["requirement"], str)
        assert blocker["requirement"].strip()
        assert blocker["id"]
    for blocker in data["external_blockers"]:
        assert blocker["requirement"].strip()


@pytest.mark.parametrize("doc_name", ["ORGANIC_AND_FUR.md", "DATACENTER_FILM_REPORT.md"])
def test_gap_docs_exist(doc_name: str) -> None:
    path = ROOT / "docs" / "v2" / doc_name
    assert path.is_file(), f"missing gap doc {doc_name}"
    assert path.stat().st_size > 200
