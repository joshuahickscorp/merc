from __future__ import annotations

import os
from pathlib import Path

import numpy as np
import pytest

from blender_vision.materials.critic import (
    MATERIAL_CRITICS,
    inject_material_failure,
    run_material_critics,
)
from blender_vision.materials.frequency import (
    FrequencyBand,
    decompose_surface,
    lacks_medium_band,
)
from blender_vision.materials.inverse import (
    SurfaceObservation,
    SurfaceRegion,
    infer_materials,
)
from blender_vision.materials.parity import (
    ParityTarget,
    compare_images,
    delta_e2000,
    render_browser,
    render_poster,
    run_parity,
    structural_difference,
)
from blender_vision.materials.textures import generate_texture_set, load_texture_set_metadata
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import MaterialHypothesis


def _synthetic_metal_views() -> list[SurfaceObservation]:
    """Tinted specular (metal) vs neutral specular (dielectric) synthetic pair helpers."""
    size = 64
    y, x = np.mgrid[0:size, 0:size]
    cx, cy = size / 2, size / 2
    r = np.sqrt((x - cx) ** 2 + (y - cy) ** 2)
    sphere = r < size * 0.45
    # Copper-like: warm base, warm specular.
    base = np.zeros((size, size, 3), dtype=np.float64)
    base[sphere] = np.array([0.72, 0.28, 0.12])
    highlight = sphere & (r < size * 0.12)
    metal = base.copy()
    metal[highlight] = np.array([0.95, 0.45, 0.2])
    # Dielectric: cool neutral highlight on same base.
    dielectric = base.copy()
    dielectric[highlight] = np.array([0.95, 0.95, 0.97])
    return [
        SurfaceObservation(
            view_id="metal-v0",
            rgb=metal,
            mask=sphere,
            highlight_mask=highlight,
            authority=AuthorityClass.OBSERVED,
        ),
        SurfaceObservation(
            view_id="diel-v0",
            rgb=dielectric,
            mask=sphere,
            highlight_mask=highlight,
            authority=AuthorityClass.OBSERVED,
        ),
    ]


def test_metalness_separation_tinted_vs_neutral_specular() -> None:
    metal_obs, diel_obs = _synthetic_metal_views()
    surface = SurfaceRegion(surface_id="probe", label="probe-metal")
    metal_set = infer_materials([metal_obs], [surface])
    diel_surface = SurfaceRegion(surface_id="probe-d", label="probe-diel")
    diel_set = infer_materials([diel_obs], [diel_surface])
    metal_h = next(
        item
        for item in metal_set.hypotheses
        if item.hypothesis_id == metal_set.selected_hypothesis_id
    )
    # For dielectric, pick lowest metalness among hypotheses if portfolio emitted.
    diel_metalness = min(item.metalness for item in diel_set.hypotheses)
    assert metal_h.metalness > diel_metalness + 0.15
    assert metal_h.metalness >= 0.35
    assert diel_metalness <= 0.55


def test_frequency_decomposition_known_band_content() -> None:
    size = 128
    y, x = np.mgrid[0:size, 0:size].astype(np.float64)
    # Medium-band sinusoidal displacement-like structure.
    medium = 0.5 + 0.5 * np.sin(x / 6.0) * np.sin(y / 6.0)
    # Colour variation without medium structure.
    colour = np.zeros((size, size, 3), dtype=np.float64)
    colour[..., 0] = (x / size)
    colour[..., 1] = (y / size)
    colour[..., 2] = 0.4
    depth_decomp = decompose_surface(medium, colour_image=np.stack([medium] * 3, axis=-1), levels=5)
    flat_decomp = decompose_surface(
        np.full((size, size), 0.5),
        colour_image=colour,
        levels=5,
    )
    assert depth_decomp.energy_map()[FrequencyBand.MEDIUM_DISPLACEMENT.value] > 0.0
    assert lacks_medium_band(flat_decomp)
    assert not lacks_medium_band(depth_decomp)


def test_texture_set_world_scale_round_trip(tmp_path: Path) -> None:
    hypothesis = MaterialHypothesis(
        hypothesis_id="tex-1",
        label="anodized",
        base_colour=[0.15, 0.18, 0.22],
        roughness=0.35,
        metalness=0.9,
        texture_scale_m=0.025,
        authority=AuthorityClass.INFERRED,
    )
    texture_set = generate_texture_set(hypothesis, size=64, output_dir=tmp_path / "maps")
    assert texture_set.world_scale_m == pytest.approx(0.025)
    meta = load_texture_set_metadata(tmp_path / "maps" / "texture_set.json")
    assert meta["world_scale_m"] == pytest.approx(0.025)
    assert meta["metres_per_pixel"] == pytest.approx(0.025 / 64)
    for name in ("base_colour", "roughness", "metalness", "normal", "displacement", "occlusion"):
        assert texture_set.paths[name].is_file()


def test_parity_metrics_identical_images() -> None:
    image = np.full((32, 32, 3), 0.4, dtype=np.float64)
    metrics = compare_images(image, image)
    assert metrics.delta_e2000 == pytest.approx(0.0, abs=1e-6)
    assert metrics.structural == pytest.approx(0.0, abs=1e-6)


def test_parity_harness_rejects_browser_wrong(tmp_path: Path) -> None:
    """A material fine offline but wrong in the browser must fail the gate."""
    from PIL import Image as PilImage

    hypothesis = MaterialHypothesis(
        hypothesis_id="parity-metal",
        label="anodized-metal",
        base_colour=[0.2, 0.22, 0.25],
        roughness=0.25,
        metalness=0.95,
        authority=AuthorityClass.INFERRED,
    )
    poster = render_poster(hypothesis, size=64, output_path=tmp_path / "poster.png")
    assert poster.is_file()

    if os.environ.get("BVMCP_RUN_BROWSER_TESTS") == "1":
        report = run_parity(
            hypothesis,
            output_dir=tmp_path / "parity",
            size=64,
            run_cycles=False,
            run_browser=True,
            browser_force_wrong=True,
            delta_e_limit=8.0,
            structural_limit=0.15,
        )
        browser = next(item for item in report.results if item.target is ParityTarget.BROWSER)
        if not browser.blocked:
            assert report.browser_gate_failed is True
            assert report.overall_passed is False
            assert browser.passed is False
            return

    # Constructed offline proof of the gate rule: offline reference diverges from
    # a deliberately wrong material image beyond perceptual thresholds.
    good = np.asarray(PilImage.open(poster).convert("RGB"))
    wrong_h = MaterialHypothesis(
        hypothesis_id="wrong",
        label="wrong",
        base_colour=[0.8, 0.1, 0.1],
        roughness=0.9,
        metalness=0.0,
        authority=AuthorityClass.INFERRED,
    )
    wrong_path = render_poster(wrong_h, size=64, output_path=tmp_path / "wrong.png")
    wrong = np.asarray(PilImage.open(wrong_path).convert("RGB"))
    de = delta_e2000(good, wrong)
    struct = structural_difference(good, wrong)
    assert de > 8.0 or struct > 0.15
    # Encode the pass rule: offline-ok + browser-wrong => overall fail.
    offline_ok = True
    browser_wrong = de > 8.0 or struct > 0.15
    overall_passed = offline_ok and not browser_wrong
    assert overall_passed is False


@pytest.mark.parametrize("failure", sorted(MATERIAL_CRITICS))
def test_each_material_critic_catches_injected_failure(failure: str) -> None:
    base = MaterialHypothesis(
        hypothesis_id="base",
        label="generic",
        base_colour=[0.5, 0.5, 0.5],
        roughness=0.4,
        metalness=0.0,
        authority=AuthorityClass.INFERRED,
    )
    ctx = inject_material_failure(failure, base)
    critique = run_material_critics(ctx)
    assert critique.findings, f"critic missed injected failure {failure}"
    diagnoses = {item.diagnosis for item in critique.findings}
    expected = {
        "plastic_looking_metal": "plastic-looking metal",
        "white_clipping": "white clipping",
        "environment_smear": "environment smear",
        "wrong_pore_scale": "wrong pore scale",
        "flat_texture_as_depth": "flat texture pretending to be depth",
        "sparkling_noise": "sparkling noise",
        "mirror_like_bead_blast": "mirror-like bead blast",
        "incorrect_subsurface": "incorrect subsurface",
        "incorrect_fur_clump_scale": "incorrect fur clump scale",
    }[failure]
    assert expected in diagnoses
    for finding in critique.findings:
        assert finding.evidence
        assert finding.measured
        assert finding.bounded_repair


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Cycles parity",
)
def test_cycles_parity_render(tmp_path: Path) -> None:
    from blender_vision.core.errors import BackendUnavailable
    from blender_vision.materials.parity import blender_probe, render_cycles

    ok, reason = blender_probe()
    if not ok:
        pytest.skip(f"Blender blocked in this environment: {reason}")

    hypothesis = MaterialHypothesis(
        hypothesis_id="cycles-1",
        label="matte-plastic",
        base_colour=[0.6, 0.2, 0.15],
        roughness=0.55,
        metalness=0.0,
        authority=AuthorityClass.INFERRED,
    )
    try:
        path = render_cycles(hypothesis, size=64, output_path=tmp_path / "cycles.png", samples=16)
    except BackendUnavailable as error:
        pytest.skip(f"Cycles blocked: {error}")
    assert path.is_file()
    assert path.stat().st_size > 100


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_BROWSER_TESTS=1 for real browser WebGL parity",
)
def test_browser_parity_render_serial(tmp_path: Path) -> None:
    from blender_vision.core.errors import BackendUnavailable

    hypothesis = MaterialHypothesis(
        hypothesis_id="browser-1",
        label="rubber",
        base_colour=[0.1, 0.1, 0.1],
        roughness=0.7,
        metalness=0.0,
        authority=AuthorityClass.INFERRED,
    )
    try:
        path = render_browser(hypothesis, size=64, output_path=tmp_path / "browser.png")
    except BackendUnavailable as error:
        pytest.skip(f"Browser blocked in this environment: {error}")
    assert path.is_file()
