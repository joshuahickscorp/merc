#!/usr/bin/env python3
"""Checkable-before-dispatch GPU placement predicate.

In the spirit of even_split_license(N, K): the planner evaluates this
from properties known in advance (resolution, samples, triangle count,
instance count, texture bytes, project device contract, kernel warmth).
It never requires having rendered the frame.

Host this table was derived on: Mac Studio M3 Ultra, Blender 4.2.1 LTS
Cycles CPU vs Cycles Metal, same samples/seed/bounces/AgX/adaptive-off/
denoise-off on both arms. See evidence/perf/cycles-device-placement.json.

The speed rule moves per-worker speedup (GPU only where it wins). It
does not change the serial fraction. The parity rule is the constraint:
CPU and Metal are NOT L1 interchangeable, so device is part of the
quality contract and must not be mixed inside one project.
"""

from __future__ import annotations

from typing import Any

# ---------------------------------------------------------------------------
# Frozen corpus complexity. These are SPEC-derived, not render-derived.
# UV sphere faces = 2 * segments * ring_count (Blender primitive).
# Ico subdiv n = 20 * 4^n. Plane = 2. Cube = 12.
# ---------------------------------------------------------------------------

CORPUS_COMPLEXITY: dict[str, dict[str, Any]] = {
    "trivial": {
        "triangle_count": 1024 + 2,
        "instance_count": 2,
        "texture_bytes": 0,
        "unique_mesh_count": 2,
        "builder": "trivial",
    },
    "trivial_hires": {
        "triangle_count": 1024 + 2,
        "instance_count": 2,
        "texture_bytes": 0,
        "unique_mesh_count": 2,
        "builder": "trivial",
    },
    "dense_geometry": {
        # Blender ico_sphere subdivisions=N yields 20*4^(N-1) faces
        # (subdiv=1 is the 20-face base). subdiv=7 => 81920 tris, not 20*4^7.
        "triangle_count": 20 * (4**6) + 2,  # 81922
        "instance_count": 2,
        "texture_bytes": 0,
        "unique_mesh_count": 2,
        "builder": "dense_geometry",
    },
    "many_instances": {
        "triangle_count": 12,  # unique cube mesh
        "instance_count": 1024,
        "rendered_triangle_count": 12 * 1024 + 2,
        "texture_bytes": 0,
        "unique_mesh_count": 2,
        "builder": "many_instances",
    },
    "principled_graph": {
        "triangle_count": 48 * 24 * 2 + 2,  # 2306
        "instance_count": 2,
        "texture_bytes": 0,
        "unique_mesh_count": 2,
        "builder": "principled_graph",
        "shader_graph": "deep_principled",
    },
    "large_textures": {
        "triangle_count": 2,
        "instance_count": 1,
        "texture_bytes": 4096 * 4096 * 4,  # packed RGBA
        "unique_mesh_count": 1,
        "builder": "large_textures",
    },
}

# ---------------------------------------------------------------------------
# Published host curve (Mac Studio M3 Ultra / Blender 4.2.1 / 2026-08-16).
#
# pixel_samples = width * height * samples.
# A first-assignment GPU license requires pixel_samples >= the floor for
# the matching complexity class AND every published cell at or above that
# floor in the same class to have been a GPU win on COLD WALL.
#
# Floors are conservative: the unknown band between a measured lose and a
# measured win is REFUSED, not interpolated into a license. The bench
# tightens these constants from the live sweep; self-tests lock them.
#
# Priors from the GPU lane (256²/64 lose on every scene; 1024²/512 trivial
# wins 8.15x cold). This lane's sweep replaces the mid-band "unknown".
# ---------------------------------------------------------------------------

# Complexity class thresholds (known-in-advance properties).
# 50k sits below Blender ico subdiv=7 (81920 tris) and well above the
# light corpus (1026 / 2306). The GPU-lane "327680" figure used 20*4^7;
# Blender counts the base icosahedron as subdiv=1, so subdiv=7 is 20*4^6.
DENSE_TRI_FLOOR = 50_000
INSTANCE_FLOOR = 256
TEXTURE_BYTE_FLOOR = 1_000_000

# pixel_samples floors. Two curves: process-cold (new Blender process,
# kernels already on disk) and resident (scene already in the worker).
# Unknown band between lose and win is REFUSED, not interpolated.
#
# Cold wall (this lane, 2026-08-16): every class loses at 256²/64
# (0.43-0.77x). Light wins at 1024²/512 (6.49x). Mid-band filled by
# the extra cold cells when present; until then the win floor stays
# at the measured heavy anchor.
LIGHT_GPU_LOSE_PIXEL_SAMPLES = 4_194_304  # 256² × 64 (0.43-0.77x)
LIGHT_GPU_WIN_PIXEL_SAMPLES = 67_108_864  # 512² × 256 / 1024² × 64 (≥2.97x)
DENSE_GPU_LOSE_PIXEL_SAMPLES = 4_194_304
DENSE_GPU_WIN_PIXEL_SAMPLES = 16_777_216  # 512² × 64 (1.56x)
INSTANCE_GPU_LOSE_PIXEL_SAMPLES = 4_194_304
INSTANCE_GPU_WIN_PIXEL_SAMPLES = 16_777_216  # 512² × 64 (1.88x)
TEXTURE_GPU_LOSE_PIXEL_SAMPLES = 4_194_304
TEXTURE_GPU_WIN_PIXEL_SAMPLES = 536_870_912  # no textured mid-band this lane

# Resident curve (same lane, same quality knobs). GPU wins much earlier
# once process+scene are warm: light/instanced at 256²/32 (2.10M),
# dense at 256²/64 (4.19M). 256²/16 still loses on trivial and dense.
LIGHT_RESIDENT_LOSE_PIXEL_SAMPLES = 1_048_576  # 256² × 16 / 128² × 64
LIGHT_RESIDENT_WIN_PIXEL_SAMPLES = 2_097_152  # 256² × 32
DENSE_RESIDENT_LOSE_PIXEL_SAMPLES = 2_097_152  # 256² × 32
DENSE_RESIDENT_WIN_PIXEL_SAMPLES = 4_194_304  # 256² × 64
INSTANCE_RESIDENT_LOSE_PIXEL_SAMPLES = 1_048_576
INSTANCE_RESIDENT_WIN_PIXEL_SAMPLES = 2_097_152
TEXTURE_RESIDENT_LOSE_PIXEL_SAMPLES = 4_194_304
TEXTURE_RESIDENT_WIN_PIXEL_SAMPLES = 536_870_912

# Required cold-wall CPU/GPU ratio to license a first GPU assignment.
# 1.15 means "GPU at least 15% faster". Do not flip on noise.
WIN_MARGIN = 1.15

# GPU process-startup / kernel-load constants. Measured on this host.
# The 0.40s CPU constant is process spawn. GPU equivalents:
CPU_PROCESS_CONSTANT_S = 0.40
GPU_PROCESS_CACHED_S = 0.45  # Metal pin, kernels already on disk
GPU_KERNEL_COMPILE_COLD_S = 32.75  # first Metal use in a cache-empty CWD
# A warm resident GPU worker has ~0 extra compile and ~0 extra process.

# L1 parity verdict (decoded pixels, never the PNG container).
# CPU vs Metal is NOT L1_PIXEL_EXACT on this host. Quantified in evidence.
L1_INTERCHANGEABLE = False
L1_WORST_SCENE = "many_instances"
L1_WORST_PIXELS_DIFFERING = 632590  # 1024²/64spp, this lane
L1_WORST_PIXELS_COMPARED = 1048576
L1_WORST_MAX_ABS = 53

DEVICES = ("CPU", "GPU")


def pixel_samples(width: int, height: int, samples: int) -> int:
    if width < 1 or height < 1 or samples < 1:
        raise ValueError("width, height, samples must be >= 1")
    return int(width) * int(height) * int(samples)


def complexity_class(
    triangle_count: int,
    instance_count: int,
    texture_bytes: int,
) -> str:
    """Bucket known-in-advance scene properties. Order is exclusive.

    textured wins over dense/instanced because the texture-generation
    cell is a different cost model (upload, not tracing). dense wins
    over instanced when both trip (a 327k-tri instanced mesh is dense).
    """
    if int(texture_bytes) >= TEXTURE_BYTE_FLOOR:
        return "textured"
    if int(triangle_count) >= DENSE_TRI_FLOOR:
        return "dense"
    if int(instance_count) >= INSTANCE_FLOOR:
        return "instanced"
    return "light"


def class_floors(cls: str, *, resident: bool = False) -> tuple[int, int]:
    """(lose_at_or_below, win_at_or_above) pixel_samples for a class."""
    cold = {
        "light": (LIGHT_GPU_LOSE_PIXEL_SAMPLES, LIGHT_GPU_WIN_PIXEL_SAMPLES),
        "dense": (DENSE_GPU_LOSE_PIXEL_SAMPLES, DENSE_GPU_WIN_PIXEL_SAMPLES),
        "instanced": (INSTANCE_GPU_LOSE_PIXEL_SAMPLES, INSTANCE_GPU_WIN_PIXEL_SAMPLES),
        "textured": (TEXTURE_GPU_LOSE_PIXEL_SAMPLES, TEXTURE_GPU_WIN_PIXEL_SAMPLES),
    }
    warm = {
        "light": (LIGHT_RESIDENT_LOSE_PIXEL_SAMPLES, LIGHT_RESIDENT_WIN_PIXEL_SAMPLES),
        "dense": (DENSE_RESIDENT_LOSE_PIXEL_SAMPLES, DENSE_RESIDENT_WIN_PIXEL_SAMPLES),
        "instanced": (INSTANCE_RESIDENT_LOSE_PIXEL_SAMPLES, INSTANCE_RESIDENT_WIN_PIXEL_SAMPLES),
        "textured": (TEXTURE_RESIDENT_LOSE_PIXEL_SAMPLES, TEXTURE_RESIDENT_WIN_PIXEL_SAMPLES),
    }
    table = warm if resident else cold
    if cls not in table:
        raise ValueError("unknown complexity class %r" % cls)
    return table[cls]


def predicted_gpu_faster(
    width: int,
    height: int,
    samples: int,
    triangle_count: int = 0,
    instance_count: int = 1,
    texture_bytes: int = 0,
    *,
    kernels_warm: bool = True,
    resident: bool = False,
) -> dict[str, Any]:
    """Speed half of the predicate. Does not look at the quality contract.

    Returns gpu_faster=True only when the published curve licenses a
    first-assignment GPU win. The unknown band is not a win.
    kernels_warm=False is compile-cold (~33s) and refuses every
    single-frame first assignment. resident=True uses the warm-worker
    curve (GPU wins at much lower pixel_samples).
    """
    ps = pixel_samples(width, height, samples)
    cls = complexity_class(triangle_count, instance_count, texture_bytes)
    lose_at, win_at = class_floors(cls, resident=resident)
    out: dict[str, Any] = {
        "gpu_faster": False,
        "band": "unknown",
        "complexity_class": cls,
        "pixel_samples": ps,
        "lose_at_or_below": lose_at,
        "win_at_or_above": win_at,
        "kernels_warm": bool(kernels_warm),
        "resident": bool(resident),
        "reason": "",
    }
    if not kernels_warm:
        # First Metal compile on this host is ~33s (66 kernels). The
        # heaviest published cell is 26s CPU / 3s GPU tracing; 3+33 > 26,
        # so a single compile-cold frame loses even there. A warm GPU
        # worker is the GPU equivalent of the 0.40s CPU startup constant.
        out["band"] = "compile_cold"
        out["reason"] = (
            "REFUSE speed: kernels are not warm. First Metal compile on this "
            "host is ~%.2fs (66 kernels). No single published cell amortises "
            "that (1024²/512 tracing is ~3s GPU vs ~26s CPU; 3+33 > 26). "
            "Warm a GPU worker, then re-evaluate."
            % GPU_KERNEL_COMPILE_COLD_S
        )
        return out
    if ps <= lose_at:
        out["band"] = "lose"
        out["reason"] = (
            "GPU loses on this host at <= %d pixel-samples in class %s "
            "(measured 256²/64 and the sweep lose-band). Dispatching GPU "
            "here makes the frame slower."
            % (lose_at, cls)
        )
        return out
    if ps >= win_at:
        out["gpu_faster"] = True
        out["band"] = "win"
        out["reason"] = (
            "GPU wins on this host at >= %d pixel-samples in class %s "
            "(margin %.2f, cold wall). This raises per-worker speedup; "
            "it does not change the serial fraction."
            % (win_at, cls, WIN_MARGIN)
        )
        return out
    out["band"] = "unknown"
    out["reason"] = (
        "REFUSE speed: %d pixel-samples sits between the measured lose "
        "floor (%d) and win floor (%d) for class %s. The planner does not "
        "interpolate a license through an unmeasured band."
        % (ps, lose_at, win_at, cls)
    )
    return out


def gpu_placement_license(
    width: int,
    height: int,
    samples: int,
    *,
    triangle_count: int = 0,
    instance_count: int = 1,
    texture_bytes: int = 0,
    project_device: str | None = None,
    metal_available: bool = True,
    kernels_warm: bool = True,
    resident: bool = False,
    adaptive: bool = False,
    denoise: bool = False,
) -> dict[str, Any]:
    """Planner-callable placement predicate.

    licensed=True means: the planner may dispatch this frame to Metal.

    Quality outranks speed. If the project has already contracted CPU,
    GPU is refused even when it would be faster. If the project has
    already contracted GPU, stay on GPU even when this frame is light.
    First assignment (project_device is None) follows the speed curve.

    adaptive/denoise are recorded, not used to manufacture a speedup.
    Both arms of any comparison must pin them identically; the
    predicate does not flip them.
    """
    out: dict[str, Any] = {
        "licensed": False,
        "gpu_faster": False,
        "band": "",
        "same_product_as_cpu": L1_INTERCHANGEABLE,
        "mix_refused": True,
        "device_is_quality_contract": True,
        "project_device": project_device,
        "candidate_device": "GPU",
        "width": int(width),
        "height": int(height),
        "samples": int(samples),
        "triangle_count": int(triangle_count),
        "instance_count": int(instance_count),
        "texture_bytes": int(texture_bytes),
        "pixel_samples": 0,
        "complexity_class": "",
        "controls": {
            "adaptive_sampling": bool(adaptive),
            "denoising": bool(denoise),
            "metal_available": bool(metal_available),
            "kernels_warm": bool(kernels_warm),
            "resident": bool(resident),
        },
        "parity": {
            "l1_pixel_exact": L1_INTERCHANGEABLE,
            "worst_scene": L1_WORST_SCENE,
            "worst_pixels_differing": L1_WORST_PIXELS_DIFFERING,
            "worst_pixels_compared": L1_WORST_PIXELS_COMPARED,
            "worst_max_abs": L1_WORST_MAX_ABS,
            "consequence": (
                "Merc must not silently mix CPU and Metal frames in one "
                "project. Device is part of the quality contract."
            ),
        },
        "reason": "",
        "rule": (
            "DISPATCH license (checkable before render): Metal available + "
            "project device contract allows GPU + (first assignment only if "
            "the published host curve says GPU wins). "
            "resident=True uses the warm-worker curve (wins at ~2-4M "
            "pixel-samples); process-cold uses the cold-wall curve (wins "
            "at 1024²/512). CPU and Metal are different products at L1. "
            "Mixing is refused. Floors are classed by triangle_count / "
            "instance_count / texture_bytes. The unknown band is not a "
            "license. kernels_warm=False refuses a single-frame first "
            "assignment (compile ~33s). This axis moves per-worker "
            "speedup, not Amdahl."
        ),
    }
    try:
        ps = pixel_samples(width, height, samples)
    except ValueError as exc:
        out["reason"] = "REFUSE: %s" % exc
        return out
    out["pixel_samples"] = ps
    cls = complexity_class(triangle_count, instance_count, texture_bytes)
    out["complexity_class"] = cls

    if project_device is not None and project_device not in DEVICES:
        out["reason"] = "REFUSE: project_device must be None, 'CPU', or 'GPU'"
        return out
    if not metal_available:
        out["reason"] = "REFUSE: no Metal device asserted; silent CPU fallback is not a GPU placement"
        return out

    speed = predicted_gpu_faster(
        width,
        height,
        samples,
        triangle_count,
        instance_count,
        texture_bytes,
        kernels_warm=kernels_warm,
        resident=resident,
    )
    out["gpu_faster"] = bool(speed["gpu_faster"])
    out["band"] = speed["band"]
    out["lose_at_or_below"] = speed["lose_at_or_below"]
    out["win_at_or_above"] = speed["win_at_or_above"]

    if project_device == "CPU":
        out["licensed"] = False
        out["reason"] = (
            "REFUSE mix: project already contracted CPU. CPU and Metal "
            "fail L1 PIXEL_EXACT (worst %s: %d/%d px, max_abs=%d). "
            "Device is part of the quality contract; do not silently switch."
            % (L1_WORST_SCENE, L1_WORST_PIXELS_DIFFERING, L1_WORST_PIXELS_COMPARED, L1_WORST_MAX_ABS)
        )
        return out

    if project_device == "GPU":
        # Stay. A light GPU frame is slower than CPU but mixing is a
        # different image, which is worse than a slow frame.
        out["licensed"] = True
        out["reason"] = (
            "LICENSED stay: project already contracted GPU. Speed band is "
            "%s (%s). Do not migrate this frame to CPU to chase a light-work "
            "win — that would mix products."
            % (speed["band"], speed["reason"])
        )
        return out

    # First assignment: speed decides.
    if speed["gpu_faster"]:
        out["licensed"] = True
        out["reason"] = (
            "LICENSED first-assignment GPU: %s Same samples/seed/bounces/"
            "AgX/adaptive/denoise as the CPU arm would have used. Device "
            "becomes the project quality contract; later frames must stay."
            % speed["reason"]
        )
        return out

    out["licensed"] = False
    out["reason"] = (
        "REFUSE first-assignment GPU: %s Assign CPU. Device then becomes "
        "the project quality contract."
        % speed["reason"]
    )
    return out


def corpus_license(scene_id: str, width: int, height: int, samples: int, **kwargs: Any) -> dict[str, Any]:
    """Convenience: look up frozen-corpus complexity, then license."""
    spec = CORPUS_COMPLEXITY.get(scene_id)
    if spec is None:
        raise KeyError("unknown corpus scene %r; have %s" % (scene_id, sorted(CORPUS_COMPLEXITY)))
    return gpu_placement_license(
        width,
        height,
        samples,
        triangle_count=int(spec["triangle_count"]),
        instance_count=int(spec["instance_count"]),
        texture_bytes=int(spec["texture_bytes"]),
        **kwargs,
    )


def self_test() -> None:
    # Complexity buckets.
    if complexity_class(1026, 2, 0) != "light":
        raise SystemExit("self-test: trivial must be light")
    if complexity_class(81922, 2, 0) != "dense":
        raise SystemExit("self-test: ico-7 (81922 tris) must be dense")
    if complexity_class(12, 1024, 0) != "instanced":
        raise SystemExit("self-test: 1024 instances must be instanced")
    if complexity_class(2, 1, 4096 * 4096 * 4) != "textured":
        raise SystemExit("self-test: 4096² must be textured")
    if complexity_class(81922, 1024, 0) != "dense":
        raise SystemExit("self-test: dense outranks instanced")
    if complexity_class(81922, 1024, 8_000_000) != "textured":
        raise SystemExit("self-test: textured outranks dense")

    # Corpus spec arithmetic.
    if CORPUS_COMPLEXITY["dense_geometry"]["triangle_count"] != 81922:
        raise SystemExit("self-test: ico-7 face count (Blender subdiv=7 is 20*4^6)")
    if CORPUS_COMPLEXITY["many_instances"]["instance_count"] != 1024:
        raise SystemExit("self-test: 32x32 instances")
    if CORPUS_COMPLEXITY["trivial"]["triangle_count"] != 1026:
        raise SystemExit("self-test: trivial face count")

    # Known lose: 256²/64 on every class (GPU lane + this lane).
    lose = gpu_placement_license(256, 256, 64, triangle_count=1026, instance_count=2)
    if lose["licensed"] or lose["gpu_faster"] or lose["band"] != "lose":
        raise SystemExit("self-test: 256²/64 light must refuse first-assignment: %s" % lose)
    dense_lose = gpu_placement_license(256, 256, 64, triangle_count=81922, instance_count=2)
    if dense_lose["licensed"] or dense_lose["gpu_faster"]:
        raise SystemExit("self-test: 256²/64 dense must refuse")
    inst_lose = gpu_placement_license(256, 256, 64, triangle_count=12, instance_count=1024)
    if inst_lose["licensed"] or inst_lose["gpu_faster"]:
        raise SystemExit("self-test: 256²/64 instanced must refuse")

    # Known cold-wall wins: light at 1024²/64 (67M, 2.97x) and 1024²/512.
    win = gpu_placement_license(1024, 1024, 64, triangle_count=1026, instance_count=2)
    if not win["licensed"] or not win["gpu_faster"] or win["band"] != "win":
        raise SystemExit("self-test: 1024²/64 light must license first-assignment: %s" % win)
    win_h = gpu_placement_license(1024, 1024, 512, triangle_count=1026, instance_count=2)
    if not win_h["licensed"] or win_h["band"] != "win":
        raise SystemExit("self-test: 1024²/512 light must license: %s" % win_h)
    # 512²/64 light is 1.10x — below WIN_MARGIN, so unknown not licensed.
    weak = gpu_placement_license(512, 512, 64, triangle_count=1026, instance_count=2)
    if weak["licensed"] or weak["band"] != "unknown":
        raise SystemExit("self-test: 512²/64 light is below margin and must refuse: %s" % weak)
    # 512²/64 dense/instanced clear the margin.
    dense_win = gpu_placement_license(512, 512, 64, triangle_count=81922, instance_count=2)
    if not dense_win["licensed"] or dense_win["band"] != "win":
        raise SystemExit("self-test: 512²/64 dense must license: %s" % dense_win)
    inst_win = gpu_placement_license(512, 512, 64, triangle_count=12, instance_count=1024)
    if not inst_win["licensed"] or inst_win["band"] != "win":
        raise SystemExit("self-test: 512²/64 instanced must license: %s" % inst_win)

    # Quality contract: do not mix, even when GPU would win.
    mix = gpu_placement_license(
        1024, 1024, 512, triangle_count=1026, instance_count=2, project_device="CPU"
    )
    if mix["licensed"]:
        raise SystemExit("self-test: must refuse GPU on a CPU-contracted project")
    if "mix" not in mix["reason"].lower() and "contract" not in mix["reason"].lower():
        raise SystemExit("self-test: mix refusal must name the contract: %s" % mix["reason"])

    # Quality contract: stay on GPU even when this frame is light.
    stay = gpu_placement_license(
        256, 256, 64, triangle_count=1026, instance_count=2, project_device="GPU"
    )
    if not stay["licensed"]:
        raise SystemExit("self-test: must stay on GPU for a GPU-contracted project")
    if stay["gpu_faster"]:
        raise SystemExit("self-test: stay must not claim a light frame is faster")

    # No Metal: refuse.
    no_metal = gpu_placement_license(1024, 1024, 512, metal_available=False)
    if no_metal["licensed"]:
        raise SystemExit("self-test: no Metal must refuse")

    # Compile-cold refuses every first-assignment (33s cannot amortise
    # even 1024²/512: 3s trace + 33s compile > 26s CPU).
    cold = gpu_placement_license(1024, 1024, 512, triangle_count=1026, kernels_warm=False)
    if cold["licensed"] or cold["band"] != "compile_cold":
        raise SystemExit("self-test: compile-cold must refuse even the heavy cell: %s" % cold)
    # A GPU-contracted project stays, even compile-cold (first frame of
    # that project pays the compile; mixing would change the image).
    cold_stay = gpu_placement_license(
        1024, 1024, 512, triangle_count=1026, kernels_warm=False, project_device="GPU"
    )
    if not cold_stay["licensed"]:
        raise SystemExit("self-test: compile-cold stay on a GPU project must still license")

    # Unknown band is not a license (between lose and win floors).
    mid_ps = (LIGHT_GPU_LOSE_PIXEL_SAMPLES + LIGHT_GPU_WIN_PIXEL_SAMPLES) // 2
    # pick a 256-wide frame whose samples land in the band
    mid_samples = max(1, mid_ps // (256 * 256))
    mid = gpu_placement_license(256, 256, mid_samples, triangle_count=1026)
    if mid["pixel_samples"] > LIGHT_GPU_LOSE_PIXEL_SAMPLES and mid["pixel_samples"] < LIGHT_GPU_WIN_PIXEL_SAMPLES:
        if mid["licensed"] or mid["band"] != "unknown":
            raise SystemExit("self-test: unknown band must refuse: %s" % mid)

    # Bad project_device.
    bad = gpu_placement_license(256, 256, 64, project_device="METAL")
    if bad["licensed"]:
        raise SystemExit("self-test: METAL is not a project_device token")

    # corpus_license wires spec complexity.
    via = corpus_license("trivial", 1024, 1024, 512)
    if not via["licensed"] or via["triangle_count"] != 1026:
        raise SystemExit("self-test: corpus_license trivial hires")
    via_dense = corpus_license("dense_geometry", 256, 256, 64)
    if via_dense["licensed"] or via_dense["complexity_class"] != "dense":
        raise SystemExit("self-test: corpus_license dense light")

    # Resident curve: 256²/32 light wins; 256²/16 still loses.
    res_win = gpu_placement_license(256, 256, 32, triangle_count=1026, resident=True)
    if not res_win["licensed"] or res_win["band"] != "win":
        raise SystemExit("self-test: resident 256²/32 light must license: %s" % res_win)
    res_lose = gpu_placement_license(256, 256, 16, triangle_count=1026, resident=True)
    if res_lose["licensed"] or res_lose["band"] != "lose":
        raise SystemExit("self-test: resident 256²/16 light must refuse: %s" % res_lose)
    # Same cell, process-cold, is unknown or lose — not a first-assignment win.
    cold_mid = gpu_placement_license(256, 256, 32, triangle_count=1026, resident=False)
    if cold_mid["licensed"]:
        raise SystemExit("self-test: process-cold 256²/32 must not license: %s" % cold_mid)

    # Parity fields are load-bearing: a future edit must not silently
    # claim L1 interchangeability.
    if via["same_product_as_cpu"] is not False:
        raise SystemExit("self-test: CPU/GPU must not be marked interchangeable")
    if via["parity"]["l1_pixel_exact"] is not False:
        raise SystemExit("self-test: L1 verdict must stay FAIL")

    # pixel_samples helper.
    if pixel_samples(256, 256, 64) != 4_194_304:
        raise SystemExit("self-test: 256²/64 pixel_samples")
    if pixel_samples(1024, 1024, 512) != 536_870_912:
        raise SystemExit("self-test: 1024²/512 pixel_samples")


if __name__ == "__main__":
    self_test()
    print("device_placement self-test ok")
