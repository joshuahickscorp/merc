from __future__ import annotations

import json
import os
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.receipts import verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.benchmarks.external import bootstrap_external_benchmark
from blender_vision.blender.passes import (
    GOVERNED_RENDER_PASSES,
    MAXIMAL_VISUAL_RENDER_PASSES,
)
from blender_vision.blender.runner import BlenderRunner
from blender_vision.core.models import EvidenceClass
from blender_vision.datasets.store import DatasetStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.visual_geometry.packets import ComponentTaskPacketStore
from blender_vision.workflows.service import ReconstructionService


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender integration",
)
def test_mac_studio_vertical_slice(tmp_path: Path) -> None:
    repository = Path(__file__).resolve().parents[3]
    scene = repository / "models" / "mac_studio" / "final_packed.blend"
    reference = repository / "web" / "assets" / "site" / "mac-studio@3x.png"
    assert scene.is_file()
    assert reference.is_file()
    project = ProjectStore.create(tmp_path / "mac-studio", "Mac Studio")
    result = Coordinator(project).run(
        "workflow.audit_reference_fidelity",
        {
            "scene": str(scene),
            "references": [
                {"source": str(reference), "rights_state": "INTERNAL", "viewpoint_label": "front"}
            ],
            "backend": "heuristic-pinhole",
            "maximum_dimension": 512,
        },
    )
    assert result["status"] == "succeeded", result["error"]
    receipt = result["result"]["stages"][-1]["result"]
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True
    assert project.status()["counts"]["comparisons"] == 1
    with project.connection() as connection:
        render_row = connection.execute(
            "SELECT outputs_json FROM render_runs ORDER BY created_at DESC LIMIT 1"
        ).fetchone()
    render_output = json.loads(render_row["outputs_json"])[0]
    pass_digests = render_output["pass_artifact_digests"]
    assert GOVERNED_RENDER_PASSES.issubset(pass_digests)
    assert MAXIMAL_VISUAL_RENDER_PASSES.issubset(pass_digests)
    # This heuristic-camera fixture can reduce the model to a near-symmetric one-pixel
    # projection. The live fixed Mac rig separately asserts all three directional products.
    assert (
        len(
            {
                pass_digests["grazing_left"],
                pass_digests["grazing_right"],
                pass_digests["grazing_top"],
            }
        )
        >= 2
    )
    assert pass_digests["wireframe"] != pass_digests["beauty"]
    assert pass_digests["zebra"] != pass_digests["beauty"]
    assert pass_digests["reflected_line"] != pass_digests["beauty"]
    assert pass_digests["zebra"] != pass_digests["reflected_line"]
    assert pass_digests["normal_discontinuity"] != pass_digests["beauty"]
    assert pass_digests["highlight_flow"] != pass_digests["beauty"]
    assert pass_digests["normal_discontinuity"] != pass_digests["highlight_flow"]
    normal_diagnostics = render_output["render_diagnostics"]["normal_discontinuity"]
    assert normal_diagnostics["engine"] == "screen_space_normal_discontinuity_v1"
    assert normal_diagnostics["valid_pixel_count"] >= 0
    if normal_diagnostics["valid_pixel_count"]:
        assert normal_diagnostics["maximum_degrees"] is not None
    assert (
        len(
            {
                pass_digests["neutral_grey_background"],
                pass_digests["white_background"],
                pass_digests["black_background"],
            }
        )
        == 3
    )
    assert pass_digests["world_normal"] != pass_digests["beauty"]
    assert pass_digests["geometric_normal"] != pass_digests["beauty"]
    exr_crop_root = tmp_path / "decoded-exr-crops"
    exr_crop_root.mkdir()
    decoded_exr = ComponentTaskPacketStore(project)._crop_exr_diagnostics(
        digest=pass_digests["depth"],
        pass_names=["depth", "normal"],
        crop_box=(0, 0, int(render_output["width"]), int(render_output["height"])),
        temporary_root=exr_crop_root,
    )
    assert decoded_exr["depth"]["status"] == "AVAILABLE"
    assert decoded_exr["normal"]["status"] == "AVAILABLE"
    assert decoded_exr["depth"]["derivation"]["encoding"] == (
        "UINT16_NEAR_WHITE_COMPONENT_CROP"
    )
    assert decoded_exr["normal"]["derivation"]["encoding"] == (
        "UINT8_RGB_SIGNED_NORMAL_MINUS1_PLUS1"
    )
    assert decoded_exr["depth"]["derivation"]["valid_pixel_count"] > 0
    assert decoded_exr["normal"]["derivation"]["valid_pixel_count"] > 0
    with Image.open(ArtifactStore(project).path_for(pass_digests["beauty"])) as beauty:
        assert max(channel[1] for channel in beauty.convert("RGB").getextrema()) > 16
    with Image.open(ArtifactStore(project).path_for(pass_digests["object_id"])) as object_id:
        rendered_colors = {
            color for _count, color in object_id.convert("RGB").getcolors(2_000_000)
        }
        visible_id_pixels = sum(
            count for count, alpha in object_id.getchannel("A").getcolors(2_000_000) if alpha > 0
        )
    expected_id_colors = {
        tuple(record["rgb"]) for record in render_output["object_ids"].values()
    }
    assert render_output["id_pass_policy"]["identity_assignment"] == (
        "CYCLES_INTEGER_OBJECT_INDEX"
    )
    assert rendered_colors - {(0, 0, 0)} <= expected_id_colors
    # This fixture's heuristic camera can reduce the governed subject below one
    # integer ID sample. Empty is safer than assigning a subpixel to the wrong object.
    if visible_id_pixels:
        assert rendered_colors & expected_id_colors
    for pass_name in (
        "neutral_grey_background",
        "white_background",
        "black_background",
    ):
        with Image.open(ArtifactStore(project).path_for(pass_digests[pass_name])) as background:
            assert background.convert("RGBA").getchannel("A").getextrema() == (255, 255)
    audit = Coordinator(project).run("project.audit", {})
    assert audit["status"] == "succeeded", audit["error"]
    inventory = audit["result"]["audit"]["inventory"]
    assert inventory["canonical_transform"]["scale_to_millimetres"] == 1000.0
    assert 196.0 < inventory["canonical_bounds_mm"]["dimensions"][0] < 199.0
    assert audit["result"]["audit"]["valid"] is True
    vent = next(item for item in inventory["objects"] if item["name"] == "mac-vent-mesh")
    assert vent["mesh"]["topology"]["closed_surface_genus"] == 0
    assert "duplicate_faces" in vent["mesh"]
    assert "normal_diagnostics" in vent["mesh"]
    assert vent["mesh"]["audit_sampling"]["exact"] is True
    assert "component_correspondence" in inventory
    assert any(
        finding["code"] == "CLOSED_SOLID_VENT_OR_GRILLE" and finding["object"] == "mac-vent-mesh"
        for finding in inventory["audit_findings"]
    )
    exported = Coordinator(project).run("blender.export", {"output_name": "mac-studio.glb"})
    assert exported["status"] == "succeeded", exported["error"]
    assert (project.root / exported["result"]["export_path"]).is_file()

    measurements = MeasurementStore(project)
    for measurement_type, role, millimetres in (
        ("line", "rear_hero_grille_width", 171.5),
        ("line", "rear_hero_grille_height", 48.5),
        ("point", "rear_hero_grille_z_center", 66.3),
        ("array_pitch", "rear_hero_grille_pitch", 1.934),
        ("circle", "rear_hero_grille_diameter", 0.918),
    ):
        measurements.add(
            measurement_type,
            {"role": role, "millimetres": millimetres},
            evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
            certainty="derived",
            uncertainty={"classification": "integration_fixture"},
        )
    proposal = Coordinator(project).run("repair.propose_mac_studio_grille", {})
    assert proposal["status"] == "succeeded", proposal["error"]
    proposal_id = proposal["result"]["id"]
    approval = Coordinator(project).run(
        "repair.approve",
        {"proposal_id": proposal_id, "approved_by": "automated-integration-validation"},
    )
    assert approval["status"] == "succeeded", approval["error"]
    repair = Coordinator(project).run("repair.apply", {"proposal_id": proposal_id})
    assert repair["status"] == "succeeded", repair["error"]
    repaired = repair["result"]
    worker = repaired["worker"]
    assert worker["generated_hole_count"] == 2349
    assert worker["topology"]["connected_components"] == 1
    assert worker["topology"]["closed_surface_genus"] == 2349
    assert worker["topology"]["non_manifold_edges"] == 0
    assert worker["ray_validation"]["open_fraction"] == 1.0
    assert all(worker["dimensional_checks"].values())
    assert worker["replaced_object"] == "mac-vent-mesh"
    assert repaired["audit"]["audit"]["valid"] is True
    assert repaired["rear_render"]["pixel_evidence"]["distinct_values"] >= 32
    assert repaired["rear_render"]["pixel_evidence"]["clipped_white_fraction"] < 0.25
    assert repaired["acceptance"]["accepted"] is False
    assert (project.root / worker["checkpoint_path"]).is_file()
    repaired_receipt_job = Coordinator(project).run("receipt.export", {})
    assert repaired_receipt_job["status"] == "succeeded", repaired_receipt_job["error"]
    repaired_receipt = repaired_receipt_job["result"]
    assert repaired_receipt["acceptance"]["accepted"] is False
    assert (
        "L3+ applied repairs still require acceptance evidence"
        in repaired_receipt["acceptance"]["blockers"]
    )
    assert repaired_receipt["acceptance"]["metrics"]["repairs"]["status_counts"]["applied"] == 1
    assert verify_receipt(project.root / repaired_receipt["path"], project=project)["valid"] is True

    components = ComponentStore(project)
    components.create(
        ComponentSpec(
            id="integration-panel",
            type=ComponentType.PANEL,
            parameters={
                "width_mm": 10.0,
                "depth_mm": 1.0,
                "height_mm": 10.0,
                "location_mm": [0.0, 0.0, 50.0],
            },
        )
    )
    components.create(
        ComponentSpec(
            id="integration-holes",
            type=ComponentType.HOLE_ARRAY,
            parameters={
                "count_x": 3,
                "count_y": 2,
                "pitch_x_mm": 3.0,
                "pitch_y_mm": 3.0,
                "diameter_mm": 1.0,
                "depth_mm": 1.0,
                "location_mm": [0.0, 0.0, 50.0],
            },
        )
    )
    components.create(
        ComponentSpec(
            id="integration-loft",
            type=ComponentType.LOFTED_SURFACE,
            parameters={
                "dimensions_mm": [18.0, 8.0, 6.0],
                "location_mm": [20.0, 0.0, 50.0],
            },
        )
    )
    components.create(
        ComponentSpec(
            id="integration-bezier",
            type=ComponentType.BEZIER,
            parameters={
                "dimensions_mm": [15.0, 5.0, 5.0],
                "control_points_mm": [
                    [-7.5, 0.0, 0.0],
                    [0.0, 3.0, 2.0],
                    [7.5, 0.0, 0.0],
                ],
                "location_mm": [-20.0, 0.0, 50.0],
            },
        )
    )
    components.create(
        ComponentSpec(
            id="integration-tire",
            type=ComponentType.TIRE_PROFILE,
            parameters={
                "radius_mm": 6.0,
                "section_radius_mm": 1.5,
                "location_mm": [0.0, 18.0, 50.0],
            },
        )
    )
    component_ids = [
        "integration-panel",
        "integration-holes",
        "integration-loft",
        "integration-bezier",
        "integration-tire",
    ]
    generated = Coordinator(project).run(
        "component.generate",
        {"component_ids": component_ids},
    )
    assert generated["status"] == "succeeded", generated["error"]
    generation = generated["result"]
    assert generation["worker"]["component_count"] == 5
    assert generation["worker"]["object_count"] == 5
    hole_array = next(
        item
        for item in generation["worker"]["generated"]
        if item["component_id"] == "integration-holes"
    )
    assert hole_array["geometry_nodes"] == ["BVMCP_integration-holes_Array"]
    assert generation["audit"]["audit"]["valid"] is True

    dataset = DatasetStore(project).plan_synthetic(
        "integration-synthetic",
        sample_count=1,
        seed=23,
        scene_id=generation["generated_scene"]["id"],
        component_ids=component_ids,
    )
    dataset_job = Coordinator(project).run("dataset.generate", {"dataset_id": dataset["id"]})
    assert dataset_job["status"] == "succeeded", dataset_job["error"]
    generated_dataset = dataset_job["result"]
    assert generated_dataset["worker"]["sample_count"] == 1
    assert set(generated_dataset["worker"]["outputs"]) == {
        "beauty",
        "instance_mask",
        "feature_mask",
        "depth",
        "normals",
        "keypoints",
        "dimensions_mm",
        "feature_ids",
        "materials",
        "lighting",
        "occlusion",
        "pose",
        "orientation",
        "visible_fraction",
        "cross_view_identity",
    }
    assert generated_dataset["dataset"]["status"] == "generated"
    assert len(generated_dataset["artifacts"]) == 6
    metadata_file = next(
        item["path"]
        for item in generated_dataset["worker"]["files"]
        if item["path"].endswith("-metadata.json")
    )
    metadata = json.loads((project.root / metadata_file).read_text(encoding="utf-8"))
    assert metadata["image_degradation"]["applied_in_compositor"] is True
    assert metadata["lighting"]["temperature_kelvin"] >= 3200
    assert "integration-holes" in {item["feature_id"] for item in metadata["feature_ids"].values()}
    assert all("dimensions_mm" in item for item in metadata["keypoints"])
    assert len(metadata["manufacturing_variation_fraction_xyz"]) == 3


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender integration",
)
def test_fresh_parametric_seed_matches_governed_envelope(tmp_path: Path) -> None:
    result = bootstrap_external_benchmark(
        tmp_path / "perseverance",
        Path(__file__).parents[1],
        reviewed_by="Integration evidence reviewer",
    )
    project = ProjectStore.open(Path(result["project"]))

    seed = ReconstructionService(project).generate_parametric_seed(
        portfolio_id=result["portfolio"]["id"]
    )

    assert seed["accepted"] is False
    assert seed["worker"]["private_starting_model"] is False
    assert seed["worker"]["component_count"] == 18
    inventory = seed["audit"]["audit"]["inventory"]
    warning_codes = {item["code"] for item in inventory["audit_findings"]}
    assert "UNAPPLIED_SCALE" not in warning_codes
    assert "NON_MANIFOLD_GEOMETRY" not in warning_codes
    actual = inventory["canonical_bounds_mm"]["dimensions"]
    assert all(
        abs(value - expected) <= 1.0
        for value, expected in zip(actual, (3000.0, 2700.0, 2200.0), strict=True)
    )
    rendered = BlenderRunner(project).run(
        "render_passes",
        Path(seed["generated_scene"]["absolute_path"]),
        {
            "output_path": str(project.root / "renders" / "seed-index-check.png"),
            "width": 320,
            "height": 320,
            "view_direction": [-1.0, -1.0, -0.65],
            "evidence_passes": True,
            "governed_validation": True,
            "requested_passes": ["beauty", "object_id", "silhouette"],
        },
        job_id="perseverance-index-check",
    )
    assert rendered["id_pass_policy"]["identity_assignment"] == (
        "CYCLES_INTEGER_OBJECT_INDEX"
    )
    index_path = project.root / rendered["passes"]["object_id"]
    with Image.open(index_path) as object_id:
        colors = {
            color for _count, color in object_id.convert("RGB").getcolors(2_000_000)
        }
        visible = sum(
            count for count, alpha in object_id.getchannel("A").getcolors(2_000_000) if alpha > 0
        )
    expected_colors = {
        tuple(record["rgb"]) for record in rendered["object_ids"].values()
    }
    assert visible > 100
    assert len(colors - {(0, 0, 0)}) >= 2
    assert colors - {(0, 0, 0)} <= expected_colors
