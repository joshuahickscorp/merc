from __future__ import annotations

from blender_vision.backends import BackendRegistry


def test_every_backend_advertises_normalized_geometry_authority_contract() -> None:
    capabilities = BackendRegistry().as_dict()

    assert capabilities
    for capability in capabilities:
        assert "state" in capability
        assert "weight_hash" in capability
        assert "commercial_use" in capability
        assert capability["output_coordinate_frame"]
        assert capability["scale_authority"]
        assert capability["known_limitations"]
        assert capability["confidence_semantics"]


def test_registry_distinguishes_execution_from_proposal_governance() -> None:
    capabilities = {item["name"]: item for item in BackendRegistry().as_dict()}

    assert capabilities["gltf-structural-validator"]["state"] == "AVAILABLE"
    assert capabilities["gltf-structural-validator"]["operations"] == ["glb_validation"]
    assert capabilities["blender-hard-surface-parametric"]["operations"] == [
        "hard_surface_parametric_modeling",
        "component_generation",
    ]
    preparation = capabilities["blender-asset-preparation"]
    assert preparation["state"] in {"AVAILABLE", "UNAVAILABLE"}
    assert set(preparation["operations"]) >= {
        "retopology",
        "uv_generation",
        "pbr_material_generation",
        "texture_projection_and_baking",
        "rigging",
        "object_animation",
        "character_lite_animation",
        "lod_generation",
        "collision_generation",
        "mesh_repair",
        "blender_editability",
    }
    external = capabilities["governed-external-3d-candidate"]
    assert external["state"] == "LICENSE_REVIEW_REQUIRED"
    assert external["quality_tier"] == "proposal-only"
    assert "organic_reconstruction" in external["operations"]
    assert "retopology" not in external["operations"]
    appearance = capabilities["blender-appearance-authority"]
    assert appearance["state"] in {"AVAILABLE", "UNAVAILABLE"}
    assert set(appearance["operations"]) >= {
        "lens_distortion_governance",
        "illumination_hypothesis_validation",
        "transparent_translucent_validation",
        "heldout_camera_validation",
        "no_camera_nudge_acceptance",
    }
    assert any(
        "does not provide an execution backend" in item
        for item in external["known_limitations"]
    )
