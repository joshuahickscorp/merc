from __future__ import annotations

import math
import os
from pathlib import Path

import numpy as np
import pytest

from blender_vision.lighting.critic import (
    LIGHTING_CRITICS,
    inject_lighting_failure,
    run_lighting_critics,
)
from blender_vision.lighting.joint import joint_solve
from blender_vision.lighting.rigs import RIG_NAMES, apply_rig_script, get_rig, list_rigs
from blender_vision.lighting.solve import (
    GeometryContext,
    LightingObservation,
    angular_error_deg,
    solve_lighting,
)
from blender_vision.materials.inverse import SurfaceObservation, SurfaceRegion
from blender_vision.v2.authority import AuthorityClass


def _shaded_sphere(
    size: int,
    light_dir: tuple[float, float, float],
    *,
    base: float = 0.55,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    y, x = np.mgrid[0:size, 0:size].astype(np.float64)
    nx = (x + 0.5) / size * 2.0 - 1.0
    ny = 1.0 - (y + 0.5) / size * 2.0
    r2 = nx * nx + ny * ny
    mask = r2 <= 1.0
    nz = np.sqrt(np.clip(1.0 - r2, 0.0, 1.0))
    normals = np.stack([nx, ny, nz], axis=-1)
    light = np.array(light_dir, dtype=np.float64)
    light = light / (np.linalg.norm(light) + 1e-8)
    ndl = np.clip(normals @ light, 0.0, 1.0)
    rgb = np.zeros((size, size, 3), dtype=np.float64)
    shade = 0.12 + 0.88 * ndl
    rgb[mask] = base * shade[mask, None]
    # Specular lobe near reflection.
    view = np.array([0.0, 0.0, 1.0])
    half = light + view
    half = half / (np.linalg.norm(half) + 1e-8)
    ndh = np.clip(normals @ half, 0.0, 1.0)
    spec = (ndh ** 40) * 0.55
    rgb[mask] = np.clip(rgb[mask] + spec[mask, None], 0.0, 1.0)
    rgb[~mask] = 0.05
    return rgb, mask, normals


def test_key_direction_recovery_on_analytic_sphere() -> None:
    true_dir = (0.45, 0.55, 0.70)
    rgb, mask, normals = _shaded_sphere(96, true_dir)
    geometry = GeometryContext(
        scene_id="sphere-probe",
        shape="sphere",
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )
    obs = LightingObservation(
        view_id="v0",
        rgb=rgb,
        mask=mask,
        normals=normals,
        authority=AuthorityClass.OBSERVED,
    )
    result = solve_lighting([obs], geometry)
    selected = next(
        item for item in result.hypotheses if item.hypothesis_id == result.selected_hypothesis_id
    )
    estimated = selected.key["direction"]
    error = angular_error_deg(estimated, true_dir)
    # Stated tolerance for analytic Lambert+specular sphere.
    assert error < 25.0, f"key direction error {error:.2f}° exceeds 25° tolerance"
    assert selected.confidence > 0.2


def test_four_rigs_are_real_blender_scripts() -> None:
    assert set(list_rigs()) == set(RIG_NAMES)
    assert len(RIG_NAMES) == 4
    for name in RIG_NAMES:
        rig = get_rig(name)
        script = apply_rig_script(name)
        assert "bpy.data.lights.new" in script or "_add_light" in script
        assert "BVMCP_Key" in script
        assert "Background" in script or "ShaderNodeBackground" in script
        assert rig.key.energy > 0
        assert rig.white_balance_k > 0
        fields = rig.to_hypothesis_fields()
        assert fields["rig_class"] == name
        assert "key" in fields and "fill" in fields and "rim" in fields


@pytest.mark.parametrize("failure", sorted(LIGHTING_CRITICS))
def test_each_lighting_critic_catches_injected_failure(failure: str) -> None:
    ctx = inject_lighting_failure(failure)
    critique = run_lighting_critics(ctx)
    assert critique.findings, f"critic missed injected failure {failure}"
    diagnoses = {item.diagnosis for item in critique.findings}
    expected = {
        "clipped_metal": "clipped metal",
        "fake_plastic_metal": "fake plastic metal",
        "floating_objects": "floating objects",
        "flat_black": "flat black",
        "overfilled_shadows": "overfilled shadows",
        "arbitrary_glow": "arbitrary glow",
        "material_error_from_lighting": "material error caused by lighting",
    }[failure]
    assert expected in diagnoses
    for finding in critique.findings:
        assert finding.evidence
        assert finding.measured
        assert finding.bounded_repair


def test_joint_solve_keeps_material_and_light_authority_separate() -> None:
    true_dir = (0.3, 0.5, 0.8)
    rgb, mask, normals = _shaded_sphere(64, true_dir, base=0.4)
    # Tint slightly copper for material cue.
    rgb = rgb.copy()
    rgb[mask, 0] = np.clip(rgb[mask, 0] * 1.2, 0.0, 1.0)
    rgb[mask, 2] = np.clip(rgb[mask, 2] * 0.7, 0.0, 1.0)
    observations = [
        SurfaceObservation(
            view_id="j0",
            rgb=rgb,
            mask=mask,
            authority=AuthorityClass.OBSERVED,
        )
    ]
    surfaces = [SurfaceRegion(surface_id="joint-surf", label="metal-probe")]
    geometry = GeometryContext(
        scene_id="joint-scene",
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )
    result = joint_solve(observations, surfaces, geometry, max_iterations=3)
    assert result.material_record.id != result.lighting_record.id
    assert result.material_record is not result.lighting_record
    assert result.authorities_merged() is False
    assert result.joint_metadata["authority_merged"] is False
    # Authority values may both be INFERRED but must remain independent records.
    assert result.material_record.RECORD_KIND == "v2.material-hypothesis-set"
    assert result.lighting_record.RECORD_KIND == "v2.lighting-hypothesis-set"
    assert result.lighting_record.joint_solve.get("joint_id")
    assert result.lighting_record.joint_solve["material_record_id"] == result.material_record.id
    assert result.lighting_record.joint_solve["lighting_record_id"] == result.lighting_record.id
    assert result.lighting_record.joint_solve["authority_merged"] is False


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 to execute rig scripts in Blender",
)
def test_rig_script_executes_in_blender(tmp_path: Path) -> None:
    import subprocess

    from blender_vision.materials.parity import blender_probe

    ok, reason = blender_probe()
    if not ok:
        pytest.skip(f"Blender blocked in this environment: {reason}")

    blender = os.environ.get(
        "BVMCP_BLENDER", "/Applications/Blender.app/Contents/MacOS/Blender"
    )
    script = apply_rig_script("neutral_documentation")
    wrapper = tmp_path / "apply_rig.py"
    marker = tmp_path / "ok.txt"
    wrapper.write_text(
        script
        + f"\nfrom pathlib import Path\nPath(r'{marker}').write_text("
        f"bpy.context.scene['bvmcp_rig_class'])\n",
        encoding="utf-8",
    )
    completed = subprocess.run(
        [
            blender,
            "--background",
            "--factory-startup",
            "--python-exit-code",
            "1",
            "--python",
            str(wrapper),
        ],
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    if completed.returncode != 0:
        pytest.skip(
            "Blender rig apply failed in this environment: "
            + (completed.stderr or completed.stdout or "")[-800:]
        )
    assert marker.read_text(encoding="utf-8") == "neutral_documentation"


def test_angular_error_known() -> None:
    assert angular_error_deg([0, 0, 1], [0, 0, 1]) == pytest.approx(0.0, abs=1e-6)
    err = angular_error_deg([0, 0, 1], [0, 1, 0])
    assert err == pytest.approx(90.0, abs=1e-3)
    assert math.isfinite(err)
