"""Renderer parity harness: same material, geometry, and lighting across targets.

Targets:
  (a) Blender Cycles (headless)
  (b) Browser raw WebGL via Playwright (one browser at a time)
  (c) Mobile LOD WebGL / offline variant
  (d) Fixed poster path (offline analytic GGX sphere)

All four consume a single ProbeRig so camera, light, background, exposure and
sphere parameters cannot drift. A material that passes offline but fails in the
browser is rejected.
"""

from __future__ import annotations

import json
import math
import os
import subprocess
import tempfile
import threading
import uuid
from dataclasses import dataclass, field, replace
from enum import StrEnum
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from blender_vision.core.errors import BackendUnavailable, BlenderVisionError
from blender_vision.v2.records import MaterialHypothesis

# Global lock: at most one browser instance may be alive.
_BROWSER_LOCK = threading.Lock()
_BROWSER_HELD = False


class ParityTarget(StrEnum):
    CYCLES = "cycles"
    BROWSER = "browser"
    MOBILE_LOD = "mobile_lod"
    POSTER = "poster"


@dataclass(slots=True)
class ProbeRig:
    """Shared probe constants for every parity target.

    No target may hardcode its own value for anything declared here.
    """

    resolution: int = 128
    sphere_radius: float = 1.0
    sphere_segments: int = 48
    sphere_rings: int = 24
    # Camera: perspective, look-at target, vertical FOV in degrees.
    camera_position: tuple[float, float, float] = (0.0, -3.2, 0.9)
    camera_target: tuple[float, float, float] = (0.0, 0.0, 0.0)
    camera_fov_y_deg: float = 35.0
    # Cycles AREA light (square). Analytic targets use the conversion below.
    light_position: tuple[float, float, float] = (2.5, 1.8, 3.2)
    light_energy: float = 250.0
    light_size: float = 1.2
    light_rotation_euler_deg: tuple[float, float, float] = (45.0, 0.0, 35.0)
    # World background (linear RGB) and Background-node strength.
    background_colour: tuple[float, float, float] = (0.12, 0.13, 0.15)
    background_strength: float = 0.4
    # Blender view_settings.exposure (EV). Analytic path multiplies linear by 2^exposure.
    # Both targets then apply the Standard linear→sRGB transfer (not Filmic/AgX).
    exposure: float = 0.0
    cycles_samples: int = 32

    def with_resolution(self, size: int) -> ProbeRig:
        if size == self.resolution:
            return self
        return replace(self, resolution=int(size))

    def exposure_gain(self) -> float:
        return float(2.0 ** self.exposure)

    def light_distance(self) -> float:
        x, y, z = self.light_position
        return float(math.sqrt(x * x + y * y + z * z))

    def light_direction_from_origin(self) -> tuple[float, float, float]:
        """Unit vector from sphere origin toward the area-light centre."""
        d = self.light_distance()
        x, y, z = self.light_position
        return (x / d, y / d, z / d)

    def direct_irradiance_scale(self) -> float:
        """Approximate irradiance scale for the AREA light at the sphere origin.

        Cycles AREA energy is total power (W). For a square Lambert emitter of
        side ``light_size`` the exitance is energy / (π · size²). At distance d
        from light centre to sphere origin, a facing surface sees solid angle
        ≈ size² · cosθ / d², so irradiance ≈ energy · cosθ / (π · d²).

        Analytic targets fold cosθ into N·L and use the facing scale:

            E0 = energy / (π · d²)

        Arithmetic for the default rig (energy=250, pos=(2.5,1.8,3.2)):
            d = sqrt(2.5²+1.8²+3.2²) ≈ 4.44185
            E0 = 250 / (π · d²) ≈ 4.0333
        """
        d = self.light_distance()
        return float(self.light_energy / (math.pi * d * d + 1e-12))

    def background_linear(self) -> tuple[float, float, float]:
        s = self.background_strength
        r, g, b = self.background_colour
        return (r * s, g * s, b * s)

    def camera_basis(self) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
        """Return (eye, right, up, forward) in world space.

        Blender camera looks along local -Z with local Y as up. ``forward`` is
        the unit vector from eye toward the target (world direction of -Z).
        """
        eye = np.asarray(self.camera_position, dtype=np.float64)
        target = np.asarray(self.camera_target, dtype=np.float64)
        forward = target - eye
        fl = float(np.linalg.norm(forward))
        if fl < 1e-12:
            raise ValueError("camera_position and camera_target must differ")
        forward = forward / fl
        world_up = np.array([0.0, 0.0, 1.0], dtype=np.float64)
        right = np.cross(forward, world_up)
        # Degenerate when looking along world up: fall back to +X as up hint.
        if float(np.linalg.norm(right)) < 1e-8:
            right = np.cross(forward, np.array([1.0, 0.0, 0.0], dtype=np.float64))
        right = right / (float(np.linalg.norm(right)) + 1e-12)
        # right = forward × up  ⇒  up = right × forward keeps a right-handed frame
        # with -Z = forward when the camera basis is (right, up, -forward).
        up = np.cross(right, forward)
        up = up / (float(np.linalg.norm(up)) + 1e-12)
        # Rebuild right so the triad stays orthonormal.
        right = np.cross(forward, up)
        right = right / (float(np.linalg.norm(right)) + 1e-12)
        return eye, right, up, forward

    def to_dict(self) -> dict[str, Any]:
        return {
            "resolution": self.resolution,
            "sphere_radius": self.sphere_radius,
            "sphere_segments": self.sphere_segments,
            "sphere_rings": self.sphere_rings,
            "camera_position": list(self.camera_position),
            "camera_target": list(self.camera_target),
            "camera_fov_y_deg": self.camera_fov_y_deg,
            "light_position": list(self.light_position),
            "light_energy": self.light_energy,
            "light_size": self.light_size,
            "light_rotation_euler_deg": list(self.light_rotation_euler_deg),
            "background_colour": list(self.background_colour),
            "background_strength": self.background_strength,
            "exposure": self.exposure,
            "cycles_samples": self.cycles_samples,
            "direct_irradiance_scale": self.direct_irradiance_scale(),
            "exposure_gain": self.exposure_gain(),
        }


DEFAULT_PROBE_RIG = ProbeRig()


@dataclass(slots=True)
class ParityMetrics:
    delta_e2000: float
    structural: float
    mean_abs_error: float

    def to_dict(self) -> dict[str, float]:
        return {
            "delta_e2000": self.delta_e2000,
            "structural": self.structural,
            "mean_abs_error": self.mean_abs_error,
        }


@dataclass(slots=True)
class ParityTargetResult:
    target: ParityTarget
    image_path: Path | None
    metrics_vs_reference: ParityMetrics | None
    passed: bool
    blocked: bool = False
    block_reason: str = ""
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "target": self.target.value,
            "image_path": str(self.image_path) if self.image_path else None,
            "metrics_vs_reference": (
                self.metrics_vs_reference.to_dict() if self.metrics_vs_reference else None
            ),
            "passed": self.passed,
            "blocked": self.blocked,
            "block_reason": self.block_reason,
            "notes": list(self.notes),
        }


@dataclass(slots=True)
class ParityReport:
    hypothesis_id: str
    reference_target: ParityTarget
    results: list[ParityTargetResult]
    overall_passed: bool
    browser_gate_failed: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "hypothesis_id": self.hypothesis_id,
            "reference_target": self.reference_target.value,
            "results": [item.to_dict() for item in self.results],
            "overall_passed": self.overall_passed,
            "browser_gate_failed": self.browser_gate_failed,
        }


class BrowserBusyError(BlenderVisionError):
    """Raised when a second browser parity render is requested while one is live."""


def delta_e2000(image_a: np.ndarray, image_b: np.ndarray) -> float:
    """Mean CIEDE2000 between two RGB images in [0,1] or [0,255]."""
    from skimage.color import deltaE_ciede2000, rgb2lab

    a = np.asarray(image_a, dtype=np.float64)
    b = np.asarray(image_b, dtype=np.float64)
    if a.max() > 1.0 + 1e-6:
        a = a / 255.0
    if b.max() > 1.0 + 1e-6:
        b = b / 255.0
    a = np.clip(a[..., :3], 0.0, 1.0)
    b = np.clip(b[..., :3], 0.0, 1.0)
    if a.shape != b.shape:
        raise ValueError("images must share shape for ΔE2000")
    lab_a = rgb2lab(a)
    lab_b = rgb2lab(b)
    return float(np.mean(deltaE_ciede2000(lab_a, lab_b)))


def structural_difference(image_a: np.ndarray, image_b: np.ndarray) -> float:
    """1 - SSIM on luminance; 0 is identical structure."""
    from skimage.metrics import structural_similarity

    a = np.asarray(image_a, dtype=np.float64)
    b = np.asarray(image_b, dtype=np.float64)
    if a.max() > 1.0 + 1e-6:
        a = a / 255.0
    if b.max() > 1.0 + 1e-6:
        b = b / 255.0
    a = np.clip(a[..., :3], 0.0, 1.0)
    b = np.clip(b[..., :3], 0.0, 1.0)
    la = 0.2126 * a[..., 0] + 0.7152 * a[..., 1] + 0.0722 * a[..., 2]
    lb = 0.2126 * b[..., 0] + 0.7152 * b[..., 1] + 0.0722 * b[..., 2]
    ssim = float(structural_similarity(la, lb, data_range=1.0))
    return float(1.0 - ssim)


def compare_images(image_a: np.ndarray, image_b: np.ndarray) -> ParityMetrics:
    a = np.asarray(image_a, dtype=np.float64)
    b = np.asarray(image_b, dtype=np.float64)
    if a.max() > 1.0 + 1e-6:
        a = a / 255.0
    if b.max() > 1.0 + 1e-6:
        b = b / 255.0
    a = np.clip(a[..., :3], 0.0, 1.0)
    b = np.clip(b[..., :3], 0.0, 1.0)
    return ParityMetrics(
        delta_e2000=delta_e2000(a, b),
        structural=structural_difference(a, b),
        mean_abs_error=float(np.mean(np.abs(a - b))),
    )


def _linear_to_srgb(linear: np.ndarray) -> np.ndarray:
    """Standard IEC 61966-2-1 transfer, matching Blender view_transform='Standard'."""
    x = np.clip(np.asarray(linear, dtype=np.float64), 0.0, None)
    low = 12.92 * x
    high = 1.055 * np.power(np.clip(x, 1e-10, None), 1.0 / 2.4) - 0.055
    return np.where(x <= 0.0031308, low, high)


def _shade_ggx(
    normal: np.ndarray,
    view: np.ndarray,
    light_dir: np.ndarray,
    *,
    base_colour: np.ndarray,
    roughness: float,
    metalness: float,
    irradiance: float | np.ndarray,
    ambient_linear: np.ndarray,
) -> np.ndarray:
    """Cook-Torrance GGX with Lambert diffuse under a single directional sample."""
    n = normal / (np.linalg.norm(normal, axis=-1, keepdims=True) + 1e-8)
    v = view / (np.linalg.norm(view, axis=-1, keepdims=True) + 1e-8)
    light_n = light_dir / (np.linalg.norm(light_dir, axis=-1, keepdims=True) + 1e-8)
    h = v + light_n
    h = h / (np.linalg.norm(h, axis=-1, keepdims=True) + 1e-8)
    n_dot_l = np.clip(np.sum(n * light_n, axis=-1), 0.0, 1.0)
    n_dot_v = np.clip(np.sum(n * v, axis=-1), 0.0, 1.0)
    n_dot_h = np.clip(np.sum(n * h, axis=-1), 0.0, 1.0)
    v_dot_h = np.clip(np.sum(v * h, axis=-1), 0.0, 1.0)

    alpha = max(0.02, min(1.0, roughness)) ** 2
    a2 = alpha * alpha
    denom = n_dot_h * n_dot_h * (a2 - 1.0) + 1.0
    d = a2 / (math.pi * denom * denom + 1e-8)

    # Schlick-GGX geometry (Smith joint approximation).
    k = (roughness + 1.0) ** 2 / 8.0
    g_v = n_dot_v / (n_dot_v * (1.0 - k) + k + 1e-8)
    g_l = n_dot_l / (n_dot_l * (1.0 - k) + k + 1e-8)
    g = g_v * g_l

    base = np.asarray(base_colour[:3], dtype=np.float64)
    f0 = base * metalness + (1.0 - metalness) * 0.04
    fresnel = f0 + (1.0 - f0) * (1.0 - v_dot_h[..., None]) ** 5

    spec = (d * g)[..., None] * fresnel / (4.0 * n_dot_v * n_dot_l + 1e-4)[..., None]
    diffuse = base * (1.0 - metalness) / math.pi * (1.0 - fresnel)
    irr = np.asarray(irradiance, dtype=np.float64)
    if irr.ndim == 0:
        irr_term = float(irr) * n_dot_l[..., None]
    else:
        irr_term = irr[..., None] * n_dot_l[..., None]
    direct = irr_term * (diffuse + spec)

    # Constant environment: hemisphere of radiance ≈ ambient_linear.
    ambient = ambient_linear * (base * (1.0 - metalness) + f0 * metalness)
    return direct + ambient


def _render_probe_ggx(
    *,
    base_colour: list[float],
    roughness: float,
    metalness: float,
    rig: ProbeRig,
    lod_bias: float = 0.0,
) -> np.ndarray:
    """Perspective ray-sphere GGX probe used by poster and mobile-LOD offline."""
    size = int(rig.resolution)
    eye, right, up, forward = rig.camera_basis()
    tan_half = math.tan(math.radians(rig.camera_fov_y_deg) * 0.5)
    y_i, x_i = np.mgrid[0:size, 0:size].astype(np.float64)
    ndc_x = (x_i + 0.5) / size * 2.0 - 1.0
    ndc_y = 1.0 - (y_i + 0.5) / size * 2.0
    # Camera looks along -Z ≡ forward; pixel ray in camera space then world.
    ray_dir = (
        forward[None, None, :]
        + ndc_x[..., None] * tan_half * right[None, None, :]
        + ndc_y[..., None] * tan_half * up[None, None, :]
    )
    ray_dir = ray_dir / (np.linalg.norm(ray_dir, axis=-1, keepdims=True) + 1e-8)

    # Ray-sphere: |eye + t d|^2 = r^2, sphere at origin.
    radius = float(rig.sphere_radius)
    b = 2.0 * np.sum(eye[None, None, :] * ray_dir, axis=-1)
    c = float(np.dot(eye, eye)) - radius * radius
    disc = b * b - 4.0 * c
    hit = disc >= 0.0
    sqrt_disc = np.sqrt(np.clip(disc, 0.0, None))
    t0 = (-b - sqrt_disc) * 0.5
    t1 = (-b + sqrt_disc) * 0.5
    t = np.where(t0 > 1e-4, t0, t1)
    hit = hit & (t > 1e-4)

    pos = eye[None, None, :] + t[..., None] * ray_dir
    normal = pos / (radius + 1e-8)
    view = -ray_dir
    light_pos = np.asarray(rig.light_position, dtype=np.float64)
    to_light = light_pos[None, None, :] - pos
    light_dist = np.linalg.norm(to_light, axis=-1, keepdims=True) + 1e-8
    light_dir = to_light / light_dist
    # Per-point inverse-square relative to the origin-facing scale E0.
    d0 = rig.light_distance()
    irradiance = rig.direct_irradiance_scale() * (d0 * d0) / (light_dist[..., 0] ** 2)

    rough = float(min(1.0, max(0.0, roughness + lod_bias)))
    ambient = np.asarray(rig.background_linear(), dtype=np.float64)
    shaded = _shade_ggx(
        normal,
        view,
        light_dir,
        base_colour=np.asarray(base_colour[:3], dtype=np.float64),
        roughness=rough,
        metalness=float(metalness),
        irradiance=irradiance,
        ambient_linear=ambient,
    )
    gain = rig.exposure_gain()
    linear = shaded * gain
    bg = np.asarray(rig.background_linear(), dtype=np.float64) * gain
    linear = np.where(hit[..., None], linear, bg[None, None, :])
    srgb = _linear_to_srgb(np.clip(linear, 0.0, None))
    return np.clip(srgb, 0.0, 1.0)


def render_poster(
    hypothesis: MaterialHypothesis,
    *,
    size: int | None = None,
    output_path: Path | None = None,
    lod_bias: float = 0.0,
    rig: ProbeRig | None = None,
) -> Path:
    base_rig = rig or DEFAULT_PROBE_RIG
    active = base_rig.with_resolution(size or base_rig.resolution)
    image = _render_probe_ggx(
        base_colour=list(hypothesis.base_colour),
        roughness=hypothesis.roughness,
        metalness=hypothesis.metalness,
        rig=active,
        lod_bias=lod_bias,
    )
    path = output_path or Path(tempfile.mkdtemp()) / f"poster-{hypothesis.hypothesis_id}.png"
    path.parent.mkdir(parents=True, exist_ok=True)
    Image.fromarray(np.clip(image * 255.0, 0, 255).astype(np.uint8), mode="RGB").save(path)
    return path


def _blender_executable() -> str:
    env = os.environ.get("BVMCP_BLENDER")
    if env and Path(env).is_file():
        return env
    candidate = Path("/Applications/Blender.app/Contents/MacOS/Blender")
    if candidate.is_file():
        return str(candidate)
    raise BackendUnavailable("Blender executable not found for Cycles parity render")


def blender_probe() -> tuple[bool, str]:
    """Return (available, reason). Truthful about Metal/headless blockers."""
    try:
        blender = _blender_executable()
    except BackendUnavailable as error:
        return False, str(error)
    try:
        completed = subprocess.run(
            [blender, "--background", "--python-expr", "print('BVMCP_BLENDER_OK')"],
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
    except (OSError, subprocess.SubprocessError) as error:
        return False, f"Blender probe failed to spawn: {error}"
    combined = (completed.stdout or "") + (completed.stderr or "")
    if completed.returncode == 0 and "BVMCP_BLENDER_OK" in combined:
        return True, "ok"
    if "blender.crash.txt" in combined or completed.returncode < 0:
        return (
            False,
            "Blender headless crash during GPU/Metal init "
            f"(returncode={completed.returncode}): {combined[-400:]}",
        )
    return False, f"Blender probe failed (returncode={completed.returncode}): {combined[-400:]}"


def build_cycles_script(
    hypothesis: MaterialHypothesis,
    output_path: Path,
    *,
    rig: ProbeRig,
    samples: int | None = None,
) -> str:
    """Generate the Cycles probe script from ProbeRig (no free-floating constants)."""
    cam = rig.camera_position
    tgt = rig.camera_target
    light = rig.light_position
    lrot = rig.light_rotation_euler_deg
    bg = rig.background_colour
    n_samples = int(samples if samples is not None else rig.cycles_samples)
    return f"""
import bpy
import math
from mathutils import Vector

bpy.ops.wm.read_factory_settings(use_empty=True)
scene = bpy.context.scene
scene.render.engine = "CYCLES"
scene.cycles.samples = {n_samples}
scene.cycles.use_denoising = False
scene.render.resolution_x = {int(rig.resolution)}
scene.render.resolution_y = {int(rig.resolution)}
scene.render.filepath = {json.dumps(str(output_path))}
scene.render.image_settings.file_format = "PNG"
scene.render.film_transparent = False
try:
    scene.cycles.device = "CPU"
except Exception:
    pass

# Standard (not Filmic/AgX): plain linear→sRGB, exposure in EV (gain = 2^exposure).
scene.view_settings.view_transform = "Standard"
scene.view_settings.look = "None"
scene.view_settings.exposure = {float(rig.exposure)}
scene.view_settings.gamma = 1.0
scene.display_settings.display_device = "sRGB"

bpy.ops.mesh.primitive_uv_sphere_add(
    segments={int(rig.sphere_segments)},
    ring_count={int(rig.sphere_rings)},
    radius={float(rig.sphere_radius)},
)
sphere = bpy.context.active_object
bpy.ops.object.shade_smooth()

mat = bpy.data.materials.new("Probe")
mat.use_nodes = True
nodes = mat.node_tree.nodes
links = mat.node_tree.links
nodes.clear()
out_n = nodes.new("ShaderNodeOutputMaterial")
bsdf = nodes.new("ShaderNodeBsdfPrincipled")
bsdf.inputs["Base Color"].default_value = (
    {float(hypothesis.base_colour[0])},
    {float(hypothesis.base_colour[1])},
    {float(hypothesis.base_colour[2])},
    1.0,
)
bsdf.inputs["Roughness"].default_value = {float(hypothesis.roughness)}
bsdf.inputs["Metallic"].default_value = {float(hypothesis.metalness)}
if "IOR" in bsdf.inputs:
    bsdf.inputs["IOR"].default_value = {float(hypothesis.specular_ior)}
if "Transmission Weight" in bsdf.inputs:
    bsdf.inputs["Transmission Weight"].default_value = {float(hypothesis.transmission)}
elif "Transmission" in bsdf.inputs:
    bsdf.inputs["Transmission"].default_value = {float(hypothesis.transmission)}
if "Subsurface Weight" in bsdf.inputs:
    bsdf.inputs["Subsurface Weight"].default_value = {float(hypothesis.subsurface)}
links.new(bsdf.outputs["BSDF"], out_n.inputs["Surface"])
sphere.data.materials.append(mat)

bpy.ops.object.light_add(type="AREA", location=({light[0]}, {light[1]}, {light[2]}))
key = bpy.context.active_object
key.data.energy = {float(rig.light_energy)}
key.data.size = {float(rig.light_size)}
key.rotation_euler = (
    math.radians({float(lrot[0])}),
    math.radians({float(lrot[1])}),
    math.radians({float(lrot[2])}),
)


world = bpy.data.worlds.new("World")
scene.world = world
world.use_nodes = True
bg_node = world.node_tree.nodes["Background"]
bg_node.inputs[0].default_value = ({bg[0]}, {bg[1]}, {bg[2]}, 1.0)
bg_node.inputs[1].default_value = {float(rig.background_strength)}

bpy.ops.object.camera_add(location=({cam[0]}, {cam[1]}, {cam[2]}))
cam = bpy.context.active_object
direction = Vector(({tgt[0]}, {tgt[1]}, {tgt[2]})) - cam.location
cam.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
cam.data.sensor_fit = "VERTICAL"
cam.data.angle = math.radians({float(rig.camera_fov_y_deg)})
scene.camera = cam

bpy.ops.render.render(write_still=True)
"""


def render_cycles(
    hypothesis: MaterialHypothesis,
    *,
    size: int | None = None,
    output_path: Path | None = None,
    samples: int | None = None,
    rig: ProbeRig | None = None,
) -> Path:
    """Render a probe sphere with the hypothesis under the shared ProbeRig in Cycles."""
    available, reason = blender_probe()
    if not available:
        raise BackendUnavailable(reason)
    blender = _blender_executable()
    base_rig = rig or DEFAULT_PROBE_RIG
    active = base_rig.with_resolution(size or base_rig.resolution)
    out = output_path or Path(tempfile.mkdtemp()) / f"cycles-{hypothesis.hypothesis_id}.png"
    out.parent.mkdir(parents=True, exist_ok=True)
    script = build_cycles_script(hypothesis, out, rig=active, samples=samples)
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as handle:
        handle.write(script)
        script_path = handle.name
    try:
        completed = subprocess.run(
            [
                blender,
                "--background",
                "--factory-startup",
                "--python-exit-code",
                "1",
                "--python",
                script_path,
            ],
            capture_output=True,
            text=True,
            timeout=180,
            check=False,
        )
        if completed.returncode != 0 or not out.is_file():
            raise BackendUnavailable(
                "Cycles parity render failed: "
                + (completed.stderr or completed.stdout or "no output")[:800]
            )
        return out
    finally:
        Path(script_path).unlink(missing_ok=True)


def _browser_material_params(
    hypothesis: MaterialHypothesis,
    *,
    lod_bias: float = 0.0,
    force_wrong: bool = False,
) -> tuple[list[float], float, float]:
    """Browser-side material. force_wrong perturbs only this path, not Cycles/poster."""
    base = [
        float(hypothesis.base_colour[0]),
        float(hypothesis.base_colour[1]),
        float(hypothesis.base_colour[2]),
    ]
    rough = float(min(1.0, max(0.0, hypothesis.roughness + lod_bias)))
    metal = float(hypothesis.metalness)
    if force_wrong:
        # Invert albedo, raise roughness, flip metalness so whole-image metrics
        # (including background dilution) still fire the published gate.
        base = [float(max(0.0, min(1.0, 1.0 - c))) for c in base]
        rough = float(min(1.0, rough + 0.4))
        metal = float(1.0 - metal)
    return base, rough, metal


def build_browser_html(
    hypothesis: MaterialHypothesis,
    *,
    rig: ProbeRig,
    lod_bias: float = 0.0,
    force_wrong: bool = False,
) -> str:
    """Build raw-WebGL HTML from ProbeRig (same camera/light/background/exposure)."""
    base, rough, metal = _browser_material_params(
        hypothesis, lod_bias=lod_bias, force_wrong=force_wrong
    )
    eye, right, up, forward = rig.camera_basis()
    bg = rig.background_linear()
    gain = rig.exposure_gain()
    e0 = rig.direct_irradiance_scale()
    d0 = rig.light_distance()
    return f"""<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8"/>
<title>material-parity</title>
<style>
  html,body{{margin:0;background:#000;overflow:hidden}}
  canvas{{display:block;width:{rig.resolution}px;height:{rig.resolution}px}}
</style>
</head>
<body>
<canvas id="c" width="{rig.resolution}" height="{rig.resolution}"></canvas>
<script>
// ProbeRig constants (must match Cycles script / offline poster).
const cameraPosition = [
  {float(eye[0])}, {float(eye[1])}, {float(eye[2])}];
const cameraTarget = [
  {float(rig.camera_target[0])},
  {float(rig.camera_target[1])},
  {float(rig.camera_target[2])}];
const cameraFovYDeg = {float(rig.camera_fov_y_deg)};
const lightPosition = [
  {float(rig.light_position[0])},
  {float(rig.light_position[1])},
  {float(rig.light_position[2])}];
const sphereRadius = {float(rig.sphere_radius)};
const baseColour = [{base[0]}, {base[1]}, {base[2]}];
const roughness = {float(rough)};
const metalness = {float(metal)};
const size = {int(rig.resolution)};
const camRight = [
  {float(right[0])}, {float(right[1])}, {float(right[2])}];
const camUp = [
  {float(up[0])}, {float(up[1])}, {float(up[2])}];
const camForward = [
  {float(forward[0])}, {float(forward[1])}, {float(forward[2])}];
const bgLinear = [
  {float(bg[0])}, {float(bg[1])}, {float(bg[2])}];
const exposureGain = {float(gain)};
const irradianceE0 = {float(e0)};
const lightDist0 = {float(d0)};
const canvas = document.getElementById('c');
const gl = canvas.getContext('webgl', {{antialias:true, preserveDrawingBuffer:true}});
if (!gl) throw new Error('WebGL unavailable');
const vs = `
attribute vec2 p;
varying vec2 vUv;
void main() {{
  vUv = p * 0.5 + 0.5;
  gl_Position = vec4(p, 0.0, 1.0);
}}`;
const fs = `
precision highp float;
varying vec2 vUv;
uniform vec3 uBase;
uniform float uRough;
uniform float uMetal;
uniform vec3 uEye;
uniform vec3 uRight;
uniform vec3 uUp;
uniform vec3 uForward;
uniform float uTanHalfFov;
uniform float uRadius;
uniform vec3 uLightPos;
uniform vec3 uBgLinear;
uniform float uExposureGain;
uniform float uIrradianceE0;
uniform float uLightDist0;

vec3 linearToSrgb(vec3 c) {{
  bvec3 cutoff = lessThanEqual(c, vec3(0.0031308));
  vec3 low = c * 12.92;
  vec3 high = 1.055 * pow(max(c, vec3(0.0)), vec3(1.0/2.4)) - 0.055;
  return mix(high, low, vec3(cutoff));
}}

void main() {{
  // WebGL vUv.y=1 is canvas top → ndc.y=+1 matches offline probe top rows.
  vec2 ndc = vUv * 2.0 - 1.0;
  vec3 rayDir = normalize(uForward + ndc.x * uTanHalfFov * uRight + ndc.y * uTanHalfFov * uUp);
  // Ray-sphere at origin.
  float b = 2.0 * dot(uEye, rayDir);
  float c = dot(uEye, uEye) - uRadius * uRadius;
  float disc = b * b - 4.0 * c;
  vec3 bg = linearToSrgb(uBgLinear * uExposureGain);
  if (disc < 0.0) {{
    gl_FragColor = vec4(bg, 1.0);
    return;
  }}
  float sdisc = sqrt(disc);
  float t0 = (-b - sdisc) * 0.5;
  float t1 = (-b + sdisc) * 0.5;
  float t = t0 > 1e-4 ? t0 : t1;
  if (t <= 1e-4) {{
    gl_FragColor = vec4(bg, 1.0);
    return;
  }}
  vec3 pos = uEye + t * rayDir;
  vec3 n = normalize(pos);
  vec3 v = normalize(-rayDir);
  vec3 toLight = uLightPos - pos;
  float ldist = length(toLight);
  vec3 l = toLight / max(ldist, 1e-6);
  float irradiance = uIrradianceE0 * (uLightDist0 * uLightDist0) / max(ldist * ldist, 1e-6);
  vec3 h = normalize(v + l);
  float ndl = max(dot(n, l), 0.0);
  float ndv = max(dot(n, v), 0.0);
  float ndh = max(dot(n, h), 0.0);
  float vdh = max(dot(v, h), 0.0);
  float alpha = max(0.02, min(1.0, uRough));
  alpha = alpha * alpha;
  float a2 = alpha * alpha;
  float denom = ndh * ndh * (a2 - 1.0) + 1.0;
  float D = a2 / max(3.14159265 * denom * denom, 1e-6);
  float k = (uRough + 1.0) * (uRough + 1.0) / 8.0;
  float Gv = ndv / max(ndv * (1.0 - k) + k, 1e-6);
  float Gl = ndl / max(ndl * (1.0 - k) + k, 1e-6);
  float G = Gv * Gl;
  vec3 f0 = mix(vec3(0.04), uBase, uMetal);
  vec3 F = f0 + (1.0 - f0) * pow(1.0 - vdh, 5.0);
  vec3 spec = (D * G) * F / max(4.0 * ndv * ndl, 1e-4);
  vec3 diffuse = uBase * (1.0 - uMetal) / 3.14159265 * (1.0 - F);
  vec3 direct = irradiance * ndl * (diffuse + spec);
  vec3 ambient = uBgLinear * (uBase * (1.0 - uMetal) + f0 * uMetal);
  vec3 linear = (direct + ambient) * uExposureGain;
  gl_FragColor = vec4(linearToSrgb(max(linear, vec3(0.0))), 1.0);
}}`;
function compile(type, src) {{
  const s = gl.createShader(type);
  gl.shaderSource(s, src);
  gl.compileShader(s);
  if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) throw new Error(gl.getShaderInfoLog(s));
  return s;
}}
const prog = gl.createProgram();
gl.attachShader(prog, compile(gl.VERTEX_SHADER, vs));
gl.attachShader(prog, compile(gl.FRAGMENT_SHADER, fs));
gl.linkProgram(prog);
if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) throw new Error(gl.getProgramInfoLog(prog));
gl.useProgram(prog);
const buf = gl.createBuffer();
gl.bindBuffer(gl.ARRAY_BUFFER, buf);
gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1,-1, 1,-1, -1,1, 1,1]), gl.STATIC_DRAW);
const loc = gl.getAttribLocation(prog, 'p');
gl.enableVertexAttribArray(loc);
gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);
function uni3(name, v) {{ gl.uniform3f(gl.getUniformLocation(prog, name), v[0], v[1], v[2]); }}
function uni1(name, v) {{ gl.uniform1f(gl.getUniformLocation(prog, name), v); }}
uni3('uBase', baseColour);
uni1('uRough', roughness);
uni1('uMetal', metalness);
uni3('uEye', cameraPosition);
uni3('uRight', camRight);
uni3('uUp', camUp);
uni3('uForward', camForward);
uni1('uTanHalfFov', Math.tan(cameraFovYDeg * Math.PI / 180.0 * 0.5));
uni1('uRadius', sphereRadius);
uni3('uLightPos', lightPosition);
uni3('uBgLinear', bgLinear);
uni1('uExposureGain', exposureGain);
uni1('uIrradianceE0', irradianceE0);
uni1('uLightDist0', lightDist0);
gl.viewport(0, 0, size, size);
gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
window.__parityReady = true;
</script>
</body>
</html>
"""


def render_browser(
    hypothesis: MaterialHypothesis,
    *,
    size: int | None = None,
    output_path: Path | None = None,
    lod_bias: float = 0.0,
    force_wrong: bool = False,
    rig: ProbeRig | None = None,
) -> Path:
    """Render the probe in a real browser WebGL context. Serialised globally."""
    global _BROWSER_HELD
    if not _BROWSER_LOCK.acquire(blocking=False):
        raise BrowserBusyError("another browser parity render is already alive")
    _BROWSER_HELD = True
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as error:
        _BROWSER_HELD = False
        _BROWSER_LOCK.release()
        raise BackendUnavailable("playwright is not installed") from error

    base_rig = rig or DEFAULT_PROBE_RIG
    active = base_rig.with_resolution(size or base_rig.resolution)
    out = output_path or Path(tempfile.mkdtemp()) / f"browser-{hypothesis.hypothesis_id}.png"
    out.parent.mkdir(parents=True, exist_ok=True)
    html = build_browser_html(
        hypothesis, rig=active, lod_bias=lod_bias, force_wrong=force_wrong
    )
    html_path = out.with_suffix(".html")
    html_path.write_text(html, encoding="utf-8")
    try:
        with sync_playwright() as playwright:
            browser = _launch_playwright_browser(playwright)
            try:
                page = browser.new_page(
                    viewport={"width": active.resolution, "height": active.resolution}
                )
                page.goto(html_path.resolve().as_uri(), wait_until="load")
                page.wait_for_function("() => window.__parityReady === true", timeout=10000)
                page.locator("canvas").screenshot(path=str(out), type="png")
            finally:
                browser.close()
        if not out.is_file():
            raise BackendUnavailable("browser parity render produced no image")
        return out
    except BackendUnavailable:
        raise
    except Exception as error:  # noqa: BLE001 — always re-raise as explicit blocker
        raise BackendUnavailable(f"browser parity render failed: {error}") from error
    finally:
        _BROWSER_HELD = False
        _BROWSER_LOCK.release()


def _launch_playwright_browser(playwright: Any) -> Any:
    """Launch a single Chromium browser. Never leave a second instance alive.

    Uses the installed Chrome channel when present (matches BrowserAdapter).
    Does not fan out to WebKit/Firefox: those engines can hang in restricted
    sandboxes and would violate the one-browser-at-a-time rule under retry.
    """
    chrome = Path("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
    launch_kwargs: dict[str, Any] = {"headless": True, "timeout": 20_000}
    if chrome.is_file():
        launch_kwargs["channel"] = "chrome"
    try:
        return playwright.chromium.launch(**launch_kwargs)
    except Exception as error:  # noqa: BLE001 — map to explicit blocker
        raise BackendUnavailable(
            "browser launch failed (Chrome/Chromium). "
            "Blocked environments often deny Crashpad/bootstrap access: "
            f"{error}"
        ) from error


def _load_rgb(path: Path) -> np.ndarray:
    return np.asarray(Image.open(path).convert("RGB"), dtype=np.float64) / 255.0


def run_parity(
    hypothesis: MaterialHypothesis,
    *,
    output_dir: Path,
    size: int = 128,
    delta_e_limit: float = 12.0,
    structural_limit: float = 0.25,
    run_cycles: bool | None = None,
    run_browser: bool | None = None,
    browser_force_wrong: bool = False,
    rig: ProbeRig | None = None,
) -> ParityReport:
    """Render across targets and enforce the browser gate."""
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    active = (rig or DEFAULT_PROBE_RIG).with_resolution(size)
    if run_cycles is None:
        run_cycles = os.environ.get("BVMCP_RUN_BLENDER_TESTS") == "1"
    if run_browser is None:
        run_browser = os.environ.get("BVMCP_RUN_BROWSER_TESTS") == "1"

    results: list[ParityTargetResult] = []
    images: dict[ParityTarget, np.ndarray] = {}

    poster_path = render_poster(
        hypothesis, size=size, output_path=output_dir / "poster.png", rig=active
    )
    images[ParityTarget.POSTER] = _load_rgb(poster_path)
    results.append(
        ParityTargetResult(
            target=ParityTarget.POSTER,
            image_path=poster_path,
            metrics_vs_reference=None,
            passed=True,
        )
    )

    mobile_path = render_poster(
        hypothesis,
        size=size,
        output_path=output_dir / "mobile_lod.png",
        lod_bias=0.18,
        rig=active,
    )
    images[ParityTarget.MOBILE_LOD] = _load_rgb(mobile_path)
    mobile_metrics = compare_images(images[ParityTarget.POSTER], images[ParityTarget.MOBILE_LOD])
    results.append(
        ParityTargetResult(
            target=ParityTarget.MOBILE_LOD,
            image_path=mobile_path,
            metrics_vs_reference=mobile_metrics,
            passed=mobile_metrics.delta_e2000 <= delta_e_limit * 1.5
            and mobile_metrics.structural <= structural_limit * 1.5,
            notes=["mobile LOD uses increased roughness bias on shared ProbeRig"],
        )
    )

    if run_cycles:
        try:
            cycles_path = render_cycles(
                hypothesis, size=size, output_path=output_dir / "cycles.png", rig=active
            )
            images[ParityTarget.CYCLES] = _load_rgb(cycles_path)
            metrics = compare_images(images[ParityTarget.POSTER], images[ParityTarget.CYCLES])
            results.append(
                ParityTargetResult(
                    target=ParityTarget.CYCLES,
                    image_path=cycles_path,
                    metrics_vs_reference=metrics,
                    passed=metrics.delta_e2000 <= delta_e_limit * 2.0,
                )
            )
        except BackendUnavailable as error:
            results.append(
                ParityTargetResult(
                    target=ParityTarget.CYCLES,
                    image_path=None,
                    metrics_vs_reference=None,
                    passed=False,
                    blocked=True,
                    block_reason=str(error),
                )
            )
    else:
        results.append(
            ParityTargetResult(
                target=ParityTarget.CYCLES,
                image_path=None,
                metrics_vs_reference=None,
                passed=False,
                blocked=True,
                block_reason="BVMCP_RUN_BLENDER_TESTS not set; Cycles not executed",
            )
        )

    browser_gate_failed = False
    if run_browser:
        try:
            browser_path = render_browser(
                hypothesis,
                size=size,
                output_path=output_dir / "browser.png",
                force_wrong=browser_force_wrong,
                rig=active,
            )
            images[ParityTarget.BROWSER] = _load_rgb(browser_path)
            # Prefer Cycles as offline reference when available; else poster.
            ref = images.get(ParityTarget.CYCLES, images[ParityTarget.POSTER])
            metrics = compare_images(ref, images[ParityTarget.BROWSER])
            # Gate is purely metric-driven. force_wrong only perturbs the browser
            # material; it must not short-circuit the comparison.
            browser_pass = (
                metrics.delta_e2000 <= delta_e_limit and metrics.structural <= structural_limit
            )
            browser_gate_failed = not browser_pass
            notes = ["browser gate enforced against offline reference via ProbeRig"]
            if browser_force_wrong:
                notes.append("browser material deliberately perturbed (roughness/metalness)")
            results.append(
                ParityTargetResult(
                    target=ParityTarget.BROWSER,
                    image_path=browser_path,
                    metrics_vs_reference=metrics,
                    passed=browser_pass,
                    notes=notes,
                )
            )
        except (BackendUnavailable, BrowserBusyError) as error:
            browser_gate_failed = True
            results.append(
                ParityTargetResult(
                    target=ParityTarget.BROWSER,
                    image_path=None,
                    metrics_vs_reference=None,
                    passed=False,
                    blocked=True,
                    block_reason=str(error),
                )
            )
    else:
        results.append(
            ParityTargetResult(
                target=ParityTarget.BROWSER,
                image_path=None,
                metrics_vs_reference=None,
                passed=False,
                blocked=True,
                block_reason="BVMCP_RUN_BROWSER_TESTS not set; browser not executed",
            )
        )

    # Pass rule: browser gate fails the material even if Cycles/poster look fine.
    offline_ok = any(
        item.target in {ParityTarget.POSTER, ParityTarget.CYCLES}
        and item.passed
        and not item.blocked
        for item in results
    )
    browser_result = next(item for item in results if item.target is ParityTarget.BROWSER)
    if not browser_result.blocked and not browser_result.passed:
        browser_gate_failed = True
    overall = offline_ok and not browser_gate_failed and (
        browser_result.blocked or browser_result.passed
    )
    if run_browser and browser_gate_failed:
        overall = False

    report = ParityReport(
        hypothesis_id=hypothesis.hypothesis_id or f"parity-{uuid.uuid4().hex[:8]}",
        reference_target=ParityTarget.CYCLES
        if any(item.target is ParityTarget.CYCLES and item.image_path for item in results)
        else ParityTarget.POSTER,
        results=results,
        overall_passed=overall,
        browser_gate_failed=browser_gate_failed,
    )
    (output_dir / "parity_report.json").write_text(
        json.dumps(report.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    (output_dir / "probe_rig.json").write_text(
        json.dumps(active.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return report
