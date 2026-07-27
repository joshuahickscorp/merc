"""Renderer parity harness: same material, geometry, and lighting across targets.

Targets:
  (a) Blender Cycles (headless)
  (b) Browser three.js/WebGL via Playwright (one browser at a time)
  (c) Mobile LOD WebGL variant
  (d) Fixed poster path (offline analytic GGX sphere)

A material that passes offline but fails in the browser is rejected.
"""

from __future__ import annotations

import json
import math
import os
import subprocess
import tempfile
import threading
import uuid
from dataclasses import dataclass, field
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


def _ggx_sphere(
    size: int,
    *,
    base_colour: list[float],
    roughness: float,
    metalness: float,
    light_dir: tuple[float, float, float] = (0.45, 0.65, 0.6),
    lod_bias: float = 0.0,
) -> np.ndarray:
    """Offline analytic GGX-ish sphere used for poster / reference fallback."""
    y, x = np.mgrid[0:size, 0:size].astype(np.float64)
    nx = (x + 0.5) / size * 2.0 - 1.0
    ny = 1.0 - (y + 0.5) / size * 2.0
    r2 = nx * nx + ny * ny
    mask = r2 <= 1.0
    nz = np.sqrt(np.clip(1.0 - r2, 0.0, 1.0))
    normal = np.stack([nx, ny, nz], axis=-1)
    view = np.array([0.0, 0.0, 1.0])
    light = np.array(light_dir, dtype=np.float64)
    light = light / (np.linalg.norm(light) + 1e-8)
    n_dot_l = np.clip(normal @ light, 0.0, 1.0)
    half = light + view
    half = half / (np.linalg.norm(half) + 1e-8)
    n_dot_h = np.clip(normal @ half, 0.0, 1.0)
    alpha = max(0.02, min(1.0, roughness + lod_bias)) ** 2
    denom = (n_dot_h * n_dot_h * (alpha - 1.0) + 1.0) ** 2
    d = alpha / (math.pi * denom + 1e-8)
    base = np.array(base_colour[:3], dtype=np.float64)
    diffuse = base * (1.0 - metalness) / math.pi
    f0 = base * metalness + (1.0 - metalness) * 0.04
    specular = f0[None, None, :] * d[..., None]
    ambient = 0.08 * base
    colour = ambient + n_dot_l[..., None] * (diffuse + specular)
    colour = np.clip(colour, 0.0, 1.0)
    colour[~mask] = 0.12
    return colour


def render_poster(
    hypothesis: MaterialHypothesis,
    *,
    size: int = 128,
    output_path: Path | None = None,
    lod_bias: float = 0.0,
) -> Path:
    image = _ggx_sphere(
        size,
        base_colour=list(hypothesis.base_colour),
        roughness=hypothesis.roughness,
        metalness=hypothesis.metalness,
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


def render_cycles(
    hypothesis: MaterialHypothesis,
    *,
    size: int = 128,
    output_path: Path | None = None,
    samples: int = 32,
) -> Path:
    """Render a probe sphere with the hypothesis under fixed key light in Cycles."""
    available, reason = blender_probe()
    if not available:
        raise BackendUnavailable(reason)
    blender = _blender_executable()
    out = output_path or Path(tempfile.mkdtemp()) / f"cycles-{hypothesis.hypothesis_id}.png"
    out.parent.mkdir(parents=True, exist_ok=True)
    script = f"""
import bpy
import math

bpy.ops.wm.read_factory_settings(use_empty=True)
scene = bpy.context.scene
scene.render.engine = "CYCLES"
scene.cycles.samples = {int(samples)}
scene.cycles.use_denoising = False
scene.render.resolution_x = {int(size)}
scene.render.resolution_y = {int(size)}
scene.render.filepath = {json.dumps(str(out))}
scene.render.image_settings.file_format = "PNG"
scene.render.film_transparent = False
try:
    scene.cycles.device = "CPU"
except Exception:
    pass

bpy.ops.mesh.primitive_uv_sphere_add(segments=48, ring_count=24, radius=1.0)
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

bpy.ops.object.light_add(type="AREA", location=(2.5, 1.8, 3.2))
key = bpy.context.active_object
key.data.energy = 250.0
key.data.size = 1.2
key.rotation_euler = (math.radians(45), 0.0, math.radians(35))

world = bpy.data.worlds.new("World")
scene.world = world
world.use_nodes = True
bg = world.node_tree.nodes["Background"]
bg.inputs[0].default_value = (0.12, 0.13, 0.15, 1.0)
bg.inputs[1].default_value = 0.4

bpy.ops.object.camera_add(location=(0.0, -3.2, 0.9))
cam = bpy.context.active_object
cam.rotation_euler = (math.radians(74), 0.0, 0.0)
scene.camera = cam

bpy.ops.render.render(write_still=True)
"""
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


_WEBGL_HTML = """<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8"/>
<title>material-parity</title>
<style>
  html,body{{margin:0;background:#1e1f22;overflow:hidden}}
  canvas{{display:block;width:{size}px;height:{size}px}}
</style>
</head>
<body>
<canvas id="c" width="{size}" height="{size}"></canvas>
<script>
const baseColour = [{bc0}, {bc1}, {bc2}];
const roughness = {roughness};
const metalness = {metalness};
const lodBias = {lod_bias};
const size = {size};
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
void main() {{
  vec2 p = vUv * 2.0 - 1.0;
  float r2 = dot(p, p);
  if (r2 > 1.0) {{
    gl_FragColor = vec4(0.12, 0.12, 0.12, 1.0);
    return;
  }}
  vec3 n = normalize(vec3(p, sqrt(max(0.0, 1.0 - r2))));
  vec3 l = normalize(vec3(0.45, 0.65, 0.6));
  vec3 v = vec3(0.0, 0.0, 1.0);
  vec3 h = normalize(l + v);
  float ndl = max(dot(n, l), 0.0);
  float ndh = max(dot(n, h), 0.0);
  float a = max(0.02, min(1.0, uRough));
  a = a * a;
  float denom = (ndh * ndh * (a - 1.0) + 1.0);
  denom = 3.14159265 * denom * denom;
  float D = a / max(denom, 1e-4);
  vec3 diffuse = uBase * (1.0 - uMetal) / 3.14159265;
  vec3 f0 = mix(vec3(0.04), uBase, uMetal);
  vec3 specular = f0 * D;
  vec3 color = 0.08 * uBase + ndl * (diffuse + specular);
  gl_FragColor = vec4(clamp(color, 0.0, 1.0), 1.0);
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
gl.useProgram(prog);
const buf = gl.createBuffer();
gl.bindBuffer(gl.ARRAY_BUFFER, buf);
gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1,-1, 1,-1, -1,1, 1,1]), gl.STATIC_DRAW);
const loc = gl.getAttribLocation(prog, 'p');
gl.enableVertexAttribArray(loc);
gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);
gl.uniform3f(gl.getUniformLocation(prog, 'uBase'), baseColour[0], baseColour[1], baseColour[2]);
gl.uniform1f(gl.getUniformLocation(prog, 'uRough'), Math.min(1.0, roughness + lodBias));
gl.uniform1f(gl.getUniformLocation(prog, 'uMetal'), metalness);
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
    size: int = 128,
    output_path: Path | None = None,
    lod_bias: float = 0.0,
    force_wrong: bool = False,
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

    out = output_path or Path(tempfile.mkdtemp()) / f"browser-{hypothesis.hypothesis_id}.png"
    out.parent.mkdir(parents=True, exist_ok=True)
    # Optional deliberate browser-wrong path for gate tests.
    rough = min(1.0, hypothesis.roughness + (0.55 if force_wrong else 0.0))
    metal = 0.0 if force_wrong else hypothesis.metalness
    html = _WEBGL_HTML.format(
        size=int(size),
        bc0=float(hypothesis.base_colour[0]),
        bc1=float(hypothesis.base_colour[1]),
        bc2=float(hypothesis.base_colour[2]),
        roughness=float(rough),
        metalness=float(metal),
        lod_bias=float(lod_bias),
    )
    html_path = out.with_suffix(".html")
    html_path.write_text(html, encoding="utf-8")
    try:
        with sync_playwright() as playwright:
            browser = _launch_playwright_browser(playwright)
            try:
                page = browser.new_page(viewport={"width": size, "height": size})
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
) -> ParityReport:
    """Render across targets and enforce the browser gate."""
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    if run_cycles is None:
        run_cycles = os.environ.get("BVMCP_RUN_BLENDER_TESTS") == "1"
    if run_browser is None:
        run_browser = os.environ.get("BVMCP_RUN_BROWSER_TESTS") == "1"

    results: list[ParityTargetResult] = []
    images: dict[ParityTarget, np.ndarray] = {}

    poster_path = render_poster(
        hypothesis, size=size, output_path=output_dir / "poster.png"
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
            notes=["mobile LOD uses increased roughness bias"],
        )
    )

    if run_cycles:
        try:
            cycles_path = render_cycles(
                hypothesis, size=size, output_path=output_dir / "cycles.png"
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
            )
            images[ParityTarget.BROWSER] = _load_rgb(browser_path)
            # Prefer Cycles as offline reference when available; else poster.
            ref = images.get(ParityTarget.CYCLES, images[ParityTarget.POSTER])
            metrics = compare_images(ref, images[ParityTarget.BROWSER])
            # Browser gate: must stay within perceptual limits vs offline reference.
            browser_pass = (
                metrics.delta_e2000 <= delta_e_limit and metrics.structural <= structural_limit
            )
            if browser_force_wrong:
                browser_pass = False
            browser_gate_failed = not browser_pass
            results.append(
                ParityTargetResult(
                    target=ParityTarget.BROWSER,
                    image_path=browser_path,
                    metrics_vs_reference=metrics,
                    passed=browser_pass,
                    notes=["browser gate enforced against offline reference"],
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
    # When browser was requested and failed, overall must be false.
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
    return report
