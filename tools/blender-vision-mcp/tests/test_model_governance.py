import hashlib
from pathlib import Path

import pytest

from blender_vision.models.store import ModelStore
from blender_vision.projects.store import ProjectStore


def test_model_checkpoint_requires_explicit_license_review_and_digest(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Models")
    checkpoint = tmp_path / "weights.bin"
    checkpoint.write_bytes(b"approved fixture weights")
    digest = hashlib.sha256(checkpoint.read_bytes()).hexdigest()
    store = ModelStore(project)
    approval = store.approve_source(
        "feature-detector",
        "https://models.example.invalid/feature-detector.bin",
        digest,
        license_record={"license": "Apache-2.0", "commercial_use": True},
        approved_by="Model License Reviewer",
        reason="License and source hash reviewed",
    )
    assert approval["status"] == "approved_for_manual_download"

    wrong = tmp_path / "wrong.bin"
    wrong.write_bytes(b"wrong")
    with pytest.raises(ValueError, match="SHA-256"):
        store.import_checkpoint(approval["id"], wrong, revision="v1")

    installed = store.import_checkpoint(approval["id"], checkpoint, revision="v1")
    assert installed["artifact"]["digest"] == digest
    assert installed["commercial_eligible"] is True
    listing = store.list()
    assert listing["policy"]["silent_downloads"] is False
    registry = {item["id"]: item for item in listing["registry"]["models"]}
    assert registry["depth-anything-v2-small"]["commercial_use"] is True
    assert registry["depth-anything-v2-base-large-giant"]["commercial_use"] is False
    assert registry["metric3d-v2"]["commercial_use"] is None
