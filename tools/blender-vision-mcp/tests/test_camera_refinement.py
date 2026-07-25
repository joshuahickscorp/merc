from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import export_receipt
from blender_vision.cameras.refinement import CameraRefiner, _phase_candidates
from blender_vision.cameras.solver import CameraSolver
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.service import ReconstructionService


def _camera(reference_id: str) -> dict[str, Any]:
    return {
        "reference_id": reference_id,
        "model": "PINHOLE",
        "width": 64,
        "height": 64,
        "intrinsics": {"fx": 80.0, "fy": 80.0, "cx": 32.0, "cy": 32.0},
        "world_from_camera": [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, 1.0, 0.0, -1.0],
            [0.0, 0.0, 1.0, 1.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        "confidence": 0.4,
        "registration_class": "approximate_visual_registration",
        "evidence_class": "SINGLE_VIEW_OBSERVED",
        "diagnostics": {"view_direction": [0.0, 1.0, -0.2]},
    }


class _FakeRunner:
    def __init__(self, project: ProjectStore):
        self.project = project

    def run(
        self,
        operation: str,
        _scene_path: Path,
        parameters: dict[str, Any],
        **_kwargs: Any,
    ) -> dict[str, Any]:
        assert operation == "evaluate_camera_candidates"
        output = Path(parameters["output_directory"])
        output.mkdir(parents=True, exist_ok=True)
        evaluations = []
        for index, candidate in enumerate(parameters["candidates"]):
            path = output / f"candidate-{index:03d}.png"
            image = Image.new("RGBA", (parameters["width"], parameters["height"]), (0, 0, 0, 0))
            ImageDraw.Draw(image).rectangle((16, 16, 48, 48), fill=(200, 200, 200, 255))
            image.save(path)
            fov = float(candidate["horizontal_fov_degrees"])
            focal = parameters["width"] / (2.0 * math.tan(math.radians(fov) / 2.0))
            evaluations.append(
                {
                    "index": index,
                    "candidate": candidate,
                    "render_path": str(path.relative_to(self.project.root)),
                    "camera": {
                        "world_from_camera": [
                            [1.0, 0.0, 0.0, 0.0],
                            [0.0, 1.0, 0.0, -1.0],
                            [0.0, 0.0, 1.0, 1.0],
                            [0.0, 0.0, 0.0, 1.0],
                        ],
                        "view_direction": candidate["view_direction"],
                        "horizontal_fov_degrees": fov,
                        "intrinsics": {
                            "fx": focal,
                            "fy": focal,
                            "cx": parameters["width"] / 2.0,
                            "cy": parameters["height"] / 2.0,
                        },
                    },
                }
            )
        return {
            "evaluations": evaluations,
            "worker": {
                "executable": "fake-blender",
                "safe_mode": True,
                "log": "jobs/logs/fake.log",
                "duration_seconds": 0.1,
            },
        }


def test_camera_refinement_is_bounded_and_stays_non_metric(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera Refinement")
    reference_path = tmp_path / "reference.png"
    reference_image = Image.new("RGBA", (64, 64), (0, 0, 0, 0))
    ImageDraw.Draw(reference_image).rectangle((16, 16, 48, 48), fill=(220, 220, 220, 255))
    reference_image.save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="INTERNAL", viewpoint_label="front"
    )
    blend = tmp_path / "candidate.blend"
    blend.write_bytes(b"test-only blend fixture")
    scene = SceneStore(project).import_blend(blend)
    source = CameraSolver(project).import_manual([_camera(reference["id"])])
    monkeypatch.setattr("blender_vision.cameras.refinement.BlenderRunner", _FakeRunner)

    result = CameraRefiner(project).refine(
        source_solution_id=source["id"],
        reference_id=reference["id"],
        scene_id=scene["id"],
        maximum_dimension=64,
        stages=1,
    )

    assert result["status"] == "completed"
    assert result["evaluation_count"] == 125
    assert result["best"]["metrics"]["silhouette_iou"] == 1.0
    camera = result["result_solution"]["cameras"][0]
    assert camera["registration_class"] == "approximate_visual_registration"
    assert camera["confidence"] <= 0.8
    framing = camera["diagnostics"]["render_framing"]
    assert camera["intrinsics"]["cx"] == 64 * (0.5 - framing["lens_shift_x"])
    assert camera["intrinsics"]["cy"] == 32 + 64 * framing["lens_shift_y"]
    assert result["result_solution"]["approved"] is False
    assert result["result_solution"]["approval"]["state"] == "pending"
    assert CameraRefiner(project).list()[0]["best_silhouette_iou"] == 1.0
    assert project.status()["counts"]["camera_refinement_runs"] == 1
    report = json.loads((project.root / result["report_path"]).read_text(encoding="utf-8"))
    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "camera-refinement.schema.json").read_text(
            encoding="utf-8"
        )
    )
    Draft202012Validator(schema).validate(report)
    receipt = export_receipt(project)
    envelope = json.loads((project.root / receipt["path"]).read_text(encoding="utf-8"))
    assert len(envelope["payload"]["evidence"]["camera_refinement_runs"]) == 1


def test_camera_refinement_rejects_partial_object_crop(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Partial Crop")
    reference_path = tmp_path / "partial.png"
    reference_image = Image.new("RGBA", (100, 80), (0, 0, 0, 0))
    ImageDraw.Draw(reference_image).rectangle((20, 10, 99, 79), fill=(220, 220, 220, 255))
    reference_image.save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="INTERNAL", viewpoint_label="detail"
    )
    blend = tmp_path / "candidate.blend"
    blend.write_bytes(b"test-only blend fixture")
    scene = SceneStore(project).import_blend(blend)
    source = CameraSolver(project).import_manual([_camera(reference["id"])])
    monkeypatch.setattr("blender_vision.cameras.refinement.BlenderRunner", _FakeRunner)

    with pytest.raises(ValueError, match="complete-object silhouette"):
        CameraRefiner(project).refine(
            source_solution_id=source["id"],
            reference_id=reference["id"],
            scene_id=scene["id"],
            maximum_dimension=64,
            stages=1,
        )


def test_camera_refinement_candidate_phases_are_bounded() -> None:
    initial = {
        "view_direction": [0.0, 1.0, -0.2],
        "horizontal_fov_degrees": 50.0,
        "fit_margin": 1.25,
        "lens_shift_x": 0.0,
        "lens_shift_y": 0.0,
    }
    for phase in (1, 2, 3, 4):
        candidates = _phase_candidates(phase, initial)
        assert len(candidates) == 125
        assert all(0.25 <= item["fit_margin"] <= 8.0 for item in candidates)
        assert all(-1.0 <= item["lens_shift_x"] <= 1.0 for item in candidates)
        assert all(-1.0 <= item["lens_shift_y"] <= 1.0 for item in candidates)
    phase_one = _phase_candidates(1, initial)
    phase_two = _phase_candidates(2, initial)
    phase_four = _phase_candidates(4, initial)
    assert len({tuple(item["view_direction"]) for item in phase_one}) == 1
    assert {item["fit_margin"] for item in phase_one} == {
        0.75,
        1.0,
        1.25,
        1.5625,
        1.875,
    }
    assert {(item["lens_shift_x"], item["lens_shift_y"]) for item in phase_two} == {
        (0.0, 0.0)
    }
    assert len({tuple(item["view_direction"]) for item in phase_four}) == 1
    assert len({(item["lens_shift_x"], item["lens_shift_y"]) for item in phase_four}) == 25


def test_render_falls_back_to_intrinsic_principal_point(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Intrinsic Framing")
    reference_path = tmp_path / "reference.png"
    Image.new("RGBA", (64, 64), (0, 0, 0, 0)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="INTERNAL", viewpoint_label="front"
    )
    blend = tmp_path / "candidate.blend"
    blend.write_bytes(b"test-only blend fixture")
    scene = SceneStore(project).import_blend(blend)
    camera = _camera(reference["id"])
    camera["intrinsics"]["cx"] = 28.0
    camera["intrinsics"]["cy"] = 36.0
    camera["diagnostics"]["camera_roll_degrees"] = 90.0
    source = CameraSolver(project).import_manual([camera])
    captured: dict[str, Any] = {}

    class RenderRunner:
        def __init__(self, selected_project: ProjectStore):
            self.project = selected_project

        def run(
            self,
            operation: str,
            _scene_path: Path,
            parameters: dict[str, Any],
            **_kwargs: Any,
        ) -> dict[str, Any]:
            assert operation == "render_passes"
            captured.update(parameters)
            output = Path(parameters["output_path"])
            Image.new("RGBA", (64, 64), (0, 0, 0, 0)).save(output)
            relative = str(output.relative_to(self.project.root))
            return {
                "render_path": relative,
                "passes": {"beauty": relative},
                "width": 64,
                "height": 64,
                "camera": {},
                "lighting": {},
                "bounds": {},
            }

    monkeypatch.setattr("blender_vision.workflows.service.BlenderRunner", RenderRunner)

    ReconstructionService(project).render_views(
        scene_id=scene["id"], solution_id=source["id"], maximum_dimension=64
    )

    assert captured["lens_shift_x"] == 0.0625
    assert captured["lens_shift_y"] == 0.0625
    assert captured["camera_roll_degrees"] == 90.0


def test_render_paths_are_unique_across_scenes_and_runs(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Unique Render Paths")
    reference_path = tmp_path / "reference.png"
    Image.new("RGBA", (64, 64), (0, 0, 0, 0)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="INTERNAL", viewpoint_label="front"
    )
    first_blend = tmp_path / "first.blend"
    first_blend.write_bytes(b"first test-only blend fixture")
    second_blend = tmp_path / "second.blend"
    second_blend.write_bytes(b"second test-only blend fixture")
    first_scene = SceneStore(project).import_blend(first_blend)
    second_scene = SceneStore(project).import_blend(second_blend)
    source = CameraSolver(project).import_manual([_camera(reference["id"])])

    class RenderRunner:
        def __init__(self, selected_project: ProjectStore):
            self.project = selected_project

        def run(
            self,
            operation: str,
            _scene_path: Path,
            parameters: dict[str, Any],
            **_kwargs: Any,
        ) -> dict[str, Any]:
            assert operation == "render_passes"
            output = Path(parameters["output_path"])
            Image.new("RGBA", (64, 64), (0, 0, 0, 0)).save(output)
            relative = str(output.relative_to(self.project.root))
            return {
                "render_path": relative,
                "passes": {"beauty": relative},
                "width": 64,
                "height": 64,
                "camera": {},
                "lighting": {},
                "bounds": {},
            }

    monkeypatch.setattr("blender_vision.workflows.service.BlenderRunner", RenderRunner)
    service = ReconstructionService(project)

    first = service.render_views(
        scene_id=first_scene["id"], solution_id=source["id"], maximum_dimension=64
    )
    second = service.render_views(
        scene_id=second_scene["id"], solution_id=source["id"], maximum_dimension=64
    )
    repeated = service.render_views(
        scene_id=first_scene["id"], solution_id=source["id"], maximum_dimension=64
    )

    paths = {
        first["renders"][0]["relative_path"],
        second["renders"][0]["relative_path"],
        repeated["renders"][0]["relative_path"],
    }
    assert len(paths) == 3
    assert first_scene["id"] in first["renders"][0]["relative_path"]
    assert second_scene["id"] in second["renders"][0]["relative_path"]
    assert all((project.root / path).is_file() for path in paths)
