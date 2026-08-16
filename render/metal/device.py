"""Pin Cycles to CPU or Metal GPU and REFUSE a silent fallback.

Must run inside Blender. Never assigns EEVEE (background abort, exit 134).
A GPU request that cannot enable a Metal device is a hard failure — Cycles
will otherwise fall back to CPU and the measurement would be a lie.
"""

from __future__ import annotations

from typing import Any

from render.lib.settings import COLOR_CONFIG, CYCLES_QUALITY, ENGINE_REFUSE, scene_bounces


def _require_bpy():
    try:
        import bpy  # type: ignore
    except ImportError as exc:
        raise RuntimeError("render.metal.device must run inside Blender") from exc
    return bpy


def refuse_eevee(engine: str | None) -> None:
    if engine in ENGINE_REFUSE or (engine or "").upper() == "EEVEE":
        raise RuntimeError("EEVEE is refused in background mode (process abort, exit 134)")


def list_cycles_devices(prefs) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for d in getattr(prefs, "devices", []):
        out.append(
            {
                "name": str(getattr(d, "name", "")),
                "type": str(getattr(d, "type", "")),
                "use": bool(getattr(d, "use", False)),
                "id": str(getattr(d, "id", "")),
            }
        )
    return out


def refresh_devices(prefs) -> None:
    if hasattr(prefs, "refresh_devices"):
        try:
            prefs.refresh_devices()
            return
        except Exception:
            pass
    if hasattr(prefs, "get_devices"):
        prefs.get_devices()


def cycles_prefs(bpy):
    addon = bpy.context.preferences.addons.get("cycles")
    if addon is None:
        bpy.ops.preferences.addon_enable(module="cycles")
        addon = bpy.context.preferences.addons.get("cycles")
    if addon is None:
        raise RuntimeError("Cycles add-on is not available")
    return addon.preferences


def inspect_metalrt(prefs) -> dict[str, Any]:
    out: dict[str, Any] = {"available": False, "property": None, "value": None, "items": []}
    for key in ("metalrt", "use_metalrt"):
        if not hasattr(prefs, key):
            continue
        out["available"] = True
        out["property"] = key
        out["value"] = getattr(prefs, key)
        try:
            prop = prefs.bl_rna.properties.get(key)
            if prop is not None and getattr(prop, "enum_items", None):
                out["items"] = [i.identifier for i in prop.enum_items]
        except Exception:
            pass
        break
    return out


def set_metalrt(prefs, value: str) -> dict[str, Any]:
    info = inspect_metalrt(prefs)
    if not info["available"]:
        return {**info, "requested": value, "applied": False, "reason": "no metalrt property on this Blender"}
    key = info["property"]
    items = info["items"] or ["OFF", "ON", "AUTO"]
    if key == "metalrt":
        if value not in items:
            raise RuntimeError(f"metalrt={value!r} not in {items}")
        setattr(prefs, key, value)
    else:
        setattr(prefs, key, value in ("ON", "AUTO", "1", "true", "True"))
    info["value"] = getattr(prefs, key)
    info["requested"] = value
    info["applied"] = True
    return info


def pin_cycles(
    scene,
    record: dict[str, Any],
    *,
    device: str,
    metal_rt: str = "OFF",
    adaptive: bool | None = None,
    persistent_data: bool = False,
) -> dict[str, Any]:
    """Pin CYCLES to CPU or Metal GPU. Returns an assertion payload.

    device is 'CPU' or 'GPU'. GPU requires compute_device_type=METAL and at
    least one enabled Metal device; otherwise this raises rather than
    falling back.
    """
    bpy = _require_bpy()
    device = device.upper()
    if device not in ("CPU", "GPU"):
        raise RuntimeError(f"device must be CPU or GPU, got {device!r}")
    refuse_eevee(record.get("engine"))
    # Factory-startup defaults to EEVEE. That is not a request — switch first,
    # then refuse if we somehow failed to leave EEVEE.
    scene.render.engine = "CYCLES"
    refuse_eevee(scene.render.engine)

    prefs = cycles_prefs(bpy)
    metalrt_info: dict[str, Any] = inspect_metalrt(prefs)

    if device == "CPU":
        if hasattr(prefs, "compute_device_type"):
            prefs.compute_device_type = "NONE"
        refresh_devices(prefs)
        for d in getattr(prefs, "devices", []):
            d.use = str(d.type) == "CPU"
        scene.cycles.device = "CPU"
    else:
        if not hasattr(prefs, "compute_device_type"):
            raise RuntimeError("REFUSE: cycles preferences have no compute_device_type")
        items = []
        try:
            prop = prefs.bl_rna.properties.get("compute_device_type")
            if prop is not None:
                items = [i.identifier for i in prop.enum_items]
        except Exception:
            items = []
        if items and "METAL" not in items:
            raise RuntimeError(f"REFUSE: METAL not in compute_device_type items {items}")
        prefs.compute_device_type = "METAL"
        refresh_devices(prefs)
        n_metal = 0
        for d in getattr(prefs, "devices", []):
            is_metal = str(d.type) == "METAL"
            d.use = is_metal
            if is_metal:
                n_metal += 1
        if n_metal == 0:
            raise RuntimeError(
                "REFUSE: compute_device_type=METAL but no Metal devices were enumerated; "
                "refusing silent CPU fallback"
            )
        metalrt_info = set_metalrt(prefs, metal_rt)
        scene.cycles.device = "GPU"

    apply_quality(scene, record, adaptive=adaptive, persistent_data=persistent_data)
    assertion = assert_device(scene, prefs, device)
    assertion["metalrt"] = metalrt_info
    assertion["compute_device_type_items"] = items if device == "GPU" else []
    return assertion


def apply_quality(scene, record: dict[str, Any], *, adaptive: bool | None, persistent_data: bool) -> None:
    q = CYCLES_QUALITY
    res = record["resolution"]
    scene.render.resolution_x = int(res[0])
    scene.render.resolution_y = int(res[1])
    scene.render.resolution_percentage = 100
    scene.render.use_file_extension = True
    scene.render.image_settings.file_format = q["image_format"]
    scene.render.image_settings.color_mode = q["color_mode"]
    scene.render.image_settings.color_depth = q["color_depth"]
    scene.render.image_settings.compression = q["compression"]
    scene.render.film_transparent = False
    scene.render.filter_size = q["filter_width"]
    scene.render.threads_mode = "AUTO"
    scene.render.use_persistent_data = bool(persistent_data)
    scene.render.use_motion_blur = bool(record.get("use_motion_blur", False))
    scene.render.use_compositing = False
    scene.render.use_sequencer = False
    scene.render.use_border = False
    scene.render.use_crop_to_border = False

    cyc = scene.cycles
    cyc.samples = int(record["samples"])
    cyc.use_denoising = False
    if adaptive is None:
        adaptive = bool(q["use_adaptive_sampling"])
    cyc.use_adaptive_sampling = bool(adaptive)
    cyc.seed = int(record["seed"])
    cyc.use_animated_seed = False
    if hasattr(cyc, "use_persistent_data"):
        cyc.use_persistent_data = bool(persistent_data)
    if hasattr(cyc, "pixel_filter_type"):
        cyc.pixel_filter_type = "BLACKMAN_HARRIS"
    elif hasattr(cyc, "filter_type"):
        cyc.filter_type = "BLACKMAN_HARRIS"
    if hasattr(cyc, "filter_width"):
        cyc.filter_width = q["filter_width"]
    cyc.filter_glossy = q["filter_glossy"]
    if hasattr(cyc, "sample_clamp_direct"):
        cyc.sample_clamp_direct = q["sample_clamp_direct"]
    if hasattr(cyc, "sample_clamp_indirect"):
        cyc.sample_clamp_indirect = q["sample_clamp_indirect"]
    if hasattr(cyc, "caustics_reflective"):
        cyc.caustics_reflective = q["caustics_reflective"]
    if hasattr(cyc, "caustics_refractive"):
        cyc.caustics_refractive = q["caustics_refractive"]
    if hasattr(cyc, "use_fast_gi"):
        cyc.use_fast_gi = False
    bounces = scene_bounces(record)
    cyc.max_bounces = bounces["max"]
    cyc.diffuse_bounces = bounces["diffuse"]
    cyc.glossy_bounces = bounces["glossy"]
    cyc.transmission_bounces = bounces["transmission"]
    cyc.volume_bounces = bounces["volume"]
    cyc.transparent_max_bounces = bounces["transparent"]

    color = record.get("color_config") or COLOR_CONFIG
    scene.display_settings.display_device = color["display_device"]
    scene.view_settings.view_transform = color["view_transform"]
    scene.view_settings.look = color["look"]
    scene.view_settings.exposure = float(color["exposure"])
    scene.view_settings.gamma = float(color["gamma"])

    scene.frame_start = 1
    scene.frame_end = 1
    scene.frame_current = int(record.get("render_frame", 1))


def assert_device(scene, prefs, want: str) -> dict[str, Any]:
    devices = list_cycles_devices(prefs)
    compute = getattr(prefs, "compute_device_type", None)
    actual = getattr(scene.cycles, "device", None)
    engine = scene.render.engine
    refuse_eevee(engine)
    enabled_metal = [d for d in devices if d["type"] == "METAL" and d["use"]]
    enabled_cpu = [d for d in devices if d["type"] == "CPU" and d["use"]]
    payload = {
        "engine": engine,
        "cycles_device": actual,
        "compute_device_type": compute,
        "want": want,
        "devices": devices,
        "enabled_metal": enabled_metal,
        "enabled_cpu": enabled_cpu,
        "backend": None,
        "fallback": False,
        "has_active_device": bool(prefs.has_active_device()) if hasattr(prefs, "has_active_device") else None,
        "num_gpu_devices": int(prefs.get_num_gpu_devices()) if hasattr(prefs, "get_num_gpu_devices") else None,
        "kernel_optimization_level": getattr(prefs, "kernel_optimization_level", None),
    }
    if want == "CPU":
        if engine != "CYCLES" or actual != "CPU":
            raise RuntimeError(f"REFUSE: wanted CPU, engine={engine} device={actual}")
        payload["backend"] = "CPU"
        payload["asserted"] = True
        return payload
    # GPU / Metal
    if engine != "CYCLES":
        raise RuntimeError(f"REFUSE: wanted Metal GPU, engine={engine}")
    if actual != "GPU":
        raise RuntimeError(f"REFUSE: wanted GPU, cycles.device={actual!r} (silent CPU fallback)")
    if compute != "METAL":
        raise RuntimeError(
            f"REFUSE: wanted Metal backend, compute_device_type={compute!r} (not METAL)"
        )
    if not enabled_metal:
        raise RuntimeError("REFUSE: GPU/METAL pinned but no Metal device is enabled")
    if hasattr(prefs, "has_active_device") and not prefs.has_active_device():
        raise RuntimeError("REFUSE: GPU/METAL pinned but has_active_device() is false")
    payload["backend"] = "METAL"
    payload["asserted"] = True
    payload["metal_device_names"] = [d["name"] for d in enabled_metal]
    return payload
