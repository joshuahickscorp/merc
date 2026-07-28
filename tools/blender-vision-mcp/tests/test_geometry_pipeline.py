from __future__ import annotations

import hashlib
from contextlib import nullcontext
from pathlib import Path
from types import SimpleNamespace

import pytest
from PIL import Image, ImageDraw

from blender_vision.core.errors import BackendUnavailable
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.models.store import ModelStore
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.pipeline import GeometryPipeline
from blender_vision.vision.store import GeometryEvidenceStore
from blender_vision.vision.vggt import VGGTAdapter


def test_vggt_requires_an_explicit_governed_installation(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "VGGT governance")
    source = tmp_path / "reference.png"
    Image.new("RGB", (64, 64), "white").save(source)
    ReferenceIngestor(project).import_file(source, rights_state="INTERNAL")

    with pytest.raises(BackendUnavailable, match="model_installation_id"):
        GeometryPipeline(project).run("vggt-commercial")


def test_vggt_commercial_backend_rejects_research_checkpoint(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "VGGT license gate")
    source = tmp_path / "reference.png"
    Image.new("RGB", (64, 64), "white").save(source)
    ReferenceIngestor(project).import_file(source, rights_state="INTERNAL")
    checkpoint = tmp_path / "model.pt"
    checkpoint.write_bytes(b"not-loaded-because-license-gate-runs-first")
    digest = hashlib.sha256(checkpoint.read_bytes()).hexdigest()
    models = ModelStore(project)
    approval = models.approve_source(
        "VGGT-1B original",
        "https://example.test/vggt-original.pt",
        digest,
        license_record={
            "license": "upstream-research-only",
            "commercial_use": False,
            "research_only": True,
        },
        approved_by="Model governance reviewer",
        reason="Research comparison only",
    )
    installation = models.import_checkpoint(
        approval["id"], checkpoint, revision="test-research-revision"
    )

    with pytest.raises(BackendUnavailable, match="commercial authority"):
        GeometryPipeline(project).run(
            "vggt-commercial", {"model_installation_id": installation["id"]}
        )


def test_vggt_adapter_normalizes_local_predictions_without_network(
    tmp_path: Path, monkeypatch
) -> None:
    np = pytest.importorskip("numpy")
    project = ProjectStore.create(tmp_path / "project", "VGGT normalized contract")
    reference = tmp_path / "reference.png"
    Image.new("RGB", (64, 48), "white").save(reference)
    imported = ReferenceIngestor(project).import_file(reference, rights_state="INTERNAL")
    checkpoint = tmp_path / "commercial-model.pt"
    checkpoint.write_bytes(b"fake-safe-state-dict")
    digest = hashlib.sha256(checkpoint.read_bytes()).hexdigest()
    models = ModelStore(project)
    approval = models.approve_source(
        "VGGT-1B commercial",
        "https://example.test/vggt-commercial.pt",
        digest,
        license_record={
            "license": "VGGT-1B-Commercial-test",
            "commercial_use": True,
            "research_only": False,
        },
        approved_by="Model governance reviewer",
        reason="Commercial adapter contract fixture",
    )
    installation = models.import_checkpoint(
        approval["id"], checkpoint, revision="test-commercial-revision"
    )

    class Tensor:
        def __init__(self, value):
            self.value = np.asarray(value)

        @property
        def shape(self):
            return self.value.shape

        def to(self, _device):
            return self

        def detach(self):
            return self

        def float(self):
            return self

        def cpu(self):
            return self

        def numpy(self):
            return self.value

    class Model:
        def load_state_dict(self, _state):
            return None

        def eval(self):
            return self

        def to(self, _device):
            return self

        def __call__(self, _images):
            return {
                "images": Tensor(np.zeros((1, 1, 3, 12, 16), dtype=np.float32)),
                "pose_enc": Tensor(np.zeros((1, 1, 9), dtype=np.float32)),
                "depth": Tensor(np.ones((1, 1, 12, 16, 1), dtype=np.float32)),
                "depth_conf": Tensor(np.full((1, 1, 12, 16), 0.8, dtype=np.float32)),
                "world_points": Tensor(np.zeros((1, 1, 12, 16, 3), dtype=np.float32)),
                "world_points_conf": Tensor(
                    np.full((1, 1, 12, 16), 0.7, dtype=np.float32)
                ),
            }

    class Torch:
        cuda = SimpleNamespace(is_available=lambda: False)
        backends = SimpleNamespace(mps=SimpleNamespace(is_available=lambda: False))

        @staticmethod
        def load(_path, *, map_location, weights_only):
            assert map_location == "cpu" and weights_only is True
            return {}

        @staticmethod
        def inference_mode():
            return nullcontext()

    def load_images(_paths):
        return Tensor(np.zeros((1, 3, 12, 16), dtype=np.float32))

    def cameras(_pose, _shape):
        extrinsic = np.zeros((1, 1, 3, 4), dtype=np.float32)
        extrinsic[0, 0, :3, :3] = np.eye(3)
        intrinsic = np.asarray(
            [[[[20.0, 0.0, 8.0], [0.0, 20.0, 6.0], [0.0, 0.0, 1.0]]]],
            dtype=np.float32,
        )
        return Tensor(extrinsic), Tensor(intrinsic)

    module_root = tmp_path / "vggt-stub"
    module_root.mkdir()
    module_file = module_root / "__init__.py"
    module_file.write_text("__version__ = 'fixture'\n", encoding="utf-8")
    runtime = (
        np,
        Torch,
        SimpleNamespace(__file__=str(module_file), __version__="fixture"),
        Model,
        (load_images, cameras),
    )
    monkeypatch.setattr(VGGTAdapter, "_runtime", staticmethod(lambda: runtime))

    result = GeometryPipeline(project).run(
        "vggt-commercial", {"model_installation_id": installation["id"], "device": "cpu"}
    )

    assert result["commercial_eligible"] is True
    assert result["evidence_class"] == "SINGLE_VIEW_OBSERVED"
    assert result["evidence"]["camera_extrinsics"][0]["reference_id"] == imported["id"]
    assert (
        result["evidence"]["camera_extrinsics"][0]["registration_class"]
        == "approximate_visual_registration"
    )
    assert result["evidence"]["scale_factor"] is None
    assert result["evidence"]["diagnostics"]["network_used"] is False
    artifacts = VGGTAdapter(project).artifacts
    for field in ("depth_artifacts", "point_artifacts", "confidence_artifacts"):
        assert artifacts.path_for(result["evidence"][field][0]).is_file()


def _reference(path: Path, offset: int) -> None:
    image = Image.new("RGBA", (96, 72), (0, 0, 0, 0))
    draw = ImageDraw.Draw(image)
    draw.rounded_rectangle((18 + offset, 14, 78 + offset, 60), radius=8, fill=(180, 180, 180, 255))
    image.save(path)


def test_silhouette_geometry_runs_are_artifact_bound_and_compared(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Geometry evidence")
    ingestor = ReferenceIngestor(project)
    for index, label in enumerate(("front", "rear")):
        source = tmp_path / f"{label}.png"
        _reference(source, index)
        ingestor.import_file(source, rights_state="TEST_FIXTURE", viewpoint_label=label)
    pipeline = GeometryPipeline(project)
    first = pipeline.run("silhouette", {"solve_cameras": False, "pyramid_level": 1})
    second = pipeline.run("silhouette", {"solve_cameras": False, "pyramid_level": 2})
    assert first["evidence_class"] == "MULTI_VIEW_OBSERVED"
    assert first["commercial_eligible"] is True
    assert len(first["evidence"]["mask_artifacts"]) == 2
    for digest in first["evidence"]["mask_artifacts"]:
        assert pipeline.artifacts.path_for(digest).is_file()
    consensus = pipeline.compare([first["id"], second["id"]])
    assert consensus["report"]["averaging_performed"] is False
    assert consensus["report"]["selected_authority_run_id"] in {first["id"], second["id"]}
    assert consensus["report"]["pairwise"][0]["decision"] == "retain_separate_hypotheses"
    store = GeometryEvidenceStore(project)
    assert len(store.list()) == 2
    assert store.latest_consensus()["id"] == consensus["id"]
    assert project.status()["counts"]["geometry_runs"] == 2


def test_external_geometry_import_requires_registered_artifacts_and_license(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "External evidence")
    evidence = {"mask_artifacts": ["a" * 64]}
    with pytest.raises(ValueError, match="unknown artifact digests"):
        GeometryPipeline(project).import_external(
            backend="external-test",
            backend_version="1",
            evidence=evidence,
            evidence_class="SINGLE_VIEW_OBSERVED",
            license_record={"license": "Test-only", "commercial_use": False},
        )
    with pytest.raises(ValueError, match="license identifier"):
        GeometryPipeline(project).import_external(
            backend="external-test",
            backend_version="1",
            evidence={},
            evidence_class="SINGLE_VIEW_OBSERVED",
            license_record={},
        )
