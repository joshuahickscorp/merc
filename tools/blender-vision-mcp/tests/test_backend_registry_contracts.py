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
    external = capabilities["governed-external-3d-candidate"]
    assert external["state"] == "LICENSE_REVIEW_REQUIRED"
    assert external["quality_tier"] == "proposal-only"
    assert "retopology" in external["operations"]
    assert any(
        "does not provide an execution backend" in item
        for item in external["known_limitations"]
    )
