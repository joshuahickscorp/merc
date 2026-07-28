from __future__ import annotations

import base64
import hashlib
import json
import math
import struct
import tempfile
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.runner import BlenderRunner
from blender_vision.core.util import canonical_json, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.perception.browser import BrowserAdapter
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.perception.experience import BrowserExperienceAdapter
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore

_GRAPHICS_INSTRUMENTATION = r"""
(() => {
  const records = {
    contexts: [],
    drawCalls: [],
    bufferUploads: [],
    textureUploads: [],
    shaderSources: [],
    errors: [],
  };
  Object.defineProperty(globalThis, "__VISIONMCP_GRAPHICS_RECORDS__", {
    value: records,
    configurable: false,
    writable: false,
  });
  const contextNames = [
    ["webgl", globalThis.WebGLRenderingContext],
    ["webgl2", globalThis.WebGL2RenderingContext],
  ];
  const originalGetContext = HTMLCanvasElement.prototype.getContext;
  HTMLCanvasElement.prototype.getContext = function(type, ...args) {
    const result = originalGetContext.call(this, type, ...args);
    if (result && ["webgl", "experimental-webgl", "webgl2", "webgpu", "2d"].includes(type)) {
      this.__visionmcpContextKind = type;
      if (!records.contexts.some(
        item => item.canvasId === (this.id || null) && item.type === type
      )) {
        records.contexts.push({
          canvasId: this.id || null,
          type,
          width: this.width,
          height: this.height,
        });
      }
    }
    return result;
  };
  for (const [kind, Constructor] of contextNames) {
    if (!Constructor) continue;
    const prototype = Constructor.prototype;
    for (const method of [
      "drawArrays", "drawElements", "drawArraysInstanced", "drawElementsInstanced",
    ]) {
      if (!prototype[method]) continue;
      const original = prototype[method];
      prototype[method] = function(...args) {
        records.drawCalls.push({
          kind,
          method,
          args: args.map(value => typeof value === "number" ? value : String(value)),
        });
        return original.apply(this, args);
      };
    }
    if (prototype.bufferData) {
      const original = prototype.bufferData;
      prototype.bufferData = function(target, source, usage, ...rest) {
        const byteLength = typeof source === "number"
          ? source
          : Number(source && source.byteLength || 0);
        records.bufferUploads.push({kind, target, byteLength, usage});
        return original.call(this, target, source, usage, ...rest);
      };
    }
    if (prototype.texImage2D) {
      const original = prototype.texImage2D;
      prototype.texImage2D = function(...args) {
        const source = args[args.length - 1];
        records.textureUploads.push({
          kind,
          width: Number(source && source.width || args[3] || 0),
          height: Number(source && source.height || args[4] || 0),
          argumentCount: args.length,
        });
        return original.apply(this, args);
      };
    }
    if (prototype.shaderSource) {
      const original = prototype.shaderSource;
      prototype.shaderSource = function(shader, source) {
        records.shaderSources.push({
          kind,
          length: String(source).length,
          stage: this.getShaderParameter(shader, this.SHADER_TYPE),
        });
        return original.call(this, shader, source);
      };
    }
  }
})();
"""

_GRAPHICS_SNAPSHOT = r"""
async () => {
  const canvases = [...document.querySelectorAll("canvas")];
  const canvasRecords = canvases.map((canvas, index) => {
    const bounds = canvas.getBoundingClientRect();
    const kind = canvas.__visionmcpContextKind || "unknown";
    const record = {
      id: canvas.id || `canvas:${index}`,
      contextKind: kind,
      cssBounds: {
        x: bounds.x,
        y: bounds.y,
        width: bounds.width,
        height: bounds.height,
      },
      drawingBuffer: {width: canvas.width, height: canvas.height},
      contextAttributes: null,
      webgl: null,
    };
    if (["webgl", "experimental-webgl", "webgl2"].includes(kind)) {
      const gl = canvas.getContext(kind);
      const debug = gl.getExtension("WEBGL_debug_renderer_info");
      const parameters = {
        version: gl.getParameter(gl.VERSION),
        shadingLanguageVersion: gl.getParameter(gl.SHADING_LANGUAGE_VERSION),
        vendor: gl.getParameter(gl.VENDOR),
        renderer: gl.getParameter(gl.RENDERER),
        maxTextureSize: gl.getParameter(gl.MAX_TEXTURE_SIZE),
        maxVertexAttribs: gl.getParameter(gl.MAX_VERTEX_ATTRIBS),
      };
      if (debug) {
        parameters.unmaskedVendor = gl.getParameter(debug.UNMASKED_VENDOR_WEBGL);
        parameters.unmaskedRenderer = gl.getParameter(debug.UNMASKED_RENDERER_WEBGL);
      }
      record.contextAttributes = gl.getContextAttributes();
      record.webgl = {
        parameters,
        extensions: gl.getSupportedExtensions(),
        contextLost: gl.isContextLost(),
      };
    }
    return record;
  });
  let frameDurations = [];
  let previous = performance.now();
  for (let index = 0; index < 12; index += 1) {
    await new Promise(resolve => requestAnimationFrame(resolve));
    const now = performance.now();
    frameDurations.push(now - previous);
    previous = now;
  }
  const sorted = [...frameDurations].sort((left, right) => left - right);
  return {
    canvases: canvasRecords,
    webgpu: {
      navigatorAvailable: Boolean(navigator.gpu),
      configuredContextCount: canvasRecords.filter(item => item.contextKind === "webgpu").length,
    },
    instrumentation: globalThis.__VISIONMCP_GRAPHICS_RECORDS__ || null,
    performance: {
      sampleCount: frameDurations.length,
      frameDurationsMs: frameDurations,
      medianFrameMs: sorted[Math.floor(sorted.length / 2)] || null,
      p95FrameMs: sorted[Math.floor(sorted.length * 0.95)] || null,
    },
    runtimeSceneHook: globalThis.__VISIONMCP_SCENE__
      ? globalThis.__VISIONMCP_SCENE__.snapshot()
      : null,
  };
}
"""


def graphics_instrumentation_script() -> str:
    """Return the exact WebGL interception used by the governed graphics adapter."""
    return _GRAPHICS_INSTRUMENTATION


class GraphicsRuntimeAdapter(BrowserAdapter):
    name = "browser.graphics"
    version = "1"

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        normalized = super().normalize_config(target, config)
        timestamps = [int(value) for value in config.get("frame_timestamps_ms", [0, 500, 1000])]
        if not 1 <= len(timestamps) <= 32 or any(
            value < 0 or value > 60_000 for value in timestamps
        ):
            raise ValueError("frame_timestamps_ms must contain 1-32 values in [0, 60000]")
        normalized["frame_timestamps_ms"] = sorted(set(timestamps))
        normalized["require_runtime_scene_hook"] = bool(
            config.get("require_runtime_scene_hook", False)
        )
        normalized["materialize_gltf"] = bool(config.get("materialize_gltf", True))
        return normalized

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            **super().environment(config),
            "capture_mode": "graphics-runtime",
            "graphics_instrumentation_version": self.version,
            "frame_timestamps_ms": config["frame_timestamps_ms"],
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        from playwright.sync_api import sync_playwright

        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(
                **BrowserExperienceAdapter._launch_options(config)
            )
            context = browser.new_context(
                **BrowserExperienceAdapter._context_options(config, has_touch=False)
            )
            context.add_init_script(_GRAPHICS_INSTRUMENTATION)
            page = context.new_page()
            BrowserExperienceAdapter._govern_page(self, page, config)
            page.goto(
                target["url"],
                wait_until=config["wait_until"],
                timeout=config["timeout_ms"],
            )
            snapshot = page.evaluate(_GRAPHICS_SNAPSHOT)
            if config["require_runtime_scene_hook"] and not snapshot["runtimeSceneHook"]:
                raise RuntimeError("owned graphics capture requires __VISIONMCP_SCENE__")
            frame_records = []
            for index, timestamp in enumerate(config["frame_timestamps_ms"]):
                runtime = page.evaluate(
                    """async timestamp => {
                        if (globalThis.__VISIONMCP_SCENE__?.setTime) {
                          globalThis.__VISIONMCP_SCENE__.setTime(timestamp);
                        }
                        await new Promise(resolve =>
                          requestAnimationFrame(() => requestAnimationFrame(resolve))
                        );
                        return globalThis.__VISIONMCP_SCENE__
                          ? globalThis.__VISIONMCP_SCENE__.snapshot()
                          : null;
                    }""",
                    timestamp,
                )
                role = f"graphics.frame.{index:03d}"
                evidence = sink(
                    role,
                    page.screenshot(type="png", full_page=False),
                    "image/png",
                    {"timestamp_ms": timestamp},
                )
                frame_records.append(
                    {
                        "timestamp_ms": timestamp,
                        "artifact_digest": evidence["digest"],
                        "runtime_scene": runtime,
                    }
                )
            snapshot["instrumentation"] = page.evaluate(
                "globalThis.__VISIONMCP_GRAPHICS_RECORDS__ || null"
            )
            context.close()
            browser.close()

        runtime_record = sink(
            "graphics.runtime",
            canonical_json(self._redact(snapshot)),
            "application/json",
            None,
        )
        graph = self._compile_graph(snapshot, frame_records, runtime_record["digest"])
        gltf_record = None
        if snapshot["runtimeSceneHook"] and config["materialize_gltf"]:
            gltf = RuntimeGltfCompiler.compile(snapshot["runtimeSceneHook"])
            gltf_record = sink(
                "graphics.scene.gltf",
                canonical_json(gltf),
                "model/gltf+json",
                {"authority": "DERIVED", "source": "explicit-runtime-scene-hook"},
            )
            graph["materialized_gltf"] = {
                "artifact_digest": gltf_record["digest"],
                "authority": "DERIVED",
            }
        sink("graphics.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "canvas_count": len(snapshot["canvases"]),
                "webgl_canvas_count": sum(
                    canvas["contextKind"] in {"webgl", "experimental-webgl", "webgl2"}
                    for canvas in snapshot["canvases"]
                ),
                "webgpu_canvas_count": snapshot["webgpu"]["configuredContextCount"],
                "draw_call_count": len(
                    (snapshot.get("instrumentation") or {}).get("drawCalls", [])
                ),
                "runtime_scene_exposed": bool(snapshot["runtimeSceneHook"]),
                "frame_count": len(frame_records),
                "median_frame_ms": snapshot["performance"]["medianFrameMs"],
                "gltf_materialized": gltf_record is not None,
            },
            limitations=[
                "Opaque GPU resources are not promoted to geometry without an exposed "
                "runtime hook.",
                "Shader source content is not collected; only stage and source length "
                "are recorded.",
                "WebGPU adapter availability is distinct from an actually configured "
                "WebGPU canvas.",
            ],
            graphs=[
                {
                    "graph_type": "GraphicsFrameGraph",
                    "role": "graphics.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "OBSERVED",
                }
            ],
        )

    @staticmethod
    def _compile_graph(
        snapshot: dict[str, Any],
        frames: list[dict[str, Any]],
        runtime_digest: str,
    ) -> dict[str, Any]:
        evidence = [{"role": "graphics.runtime", "artifact_digest": runtime_digest}]
        nodes: list[dict[str, Any]] = []
        edges: list[dict[str, Any]] = []
        for canvas in snapshot["canvases"]:
            canvas_id = f"graphics:canvas:{canvas['id']}"
            nodes.append(
                {
                    "id": canvas_id,
                    "domain_type": "GraphicsSurface",
                    "spatial_bounds": canvas["cssBounds"],
                    "temporal_validity": "capture-session",
                    "evidence_references": evidence,
                    "authority": "OBSERVED",
                    "confidence": 1.0,
                    "source_restrictions": ["public-runtime-only"],
                    "uncertainty": [],
                    "revision_lineage": [],
                    **canvas,
                }
            )
        runtime = snapshot["runtimeSceneHook"]
        if runtime:
            camera = runtime.get("camera")
            if camera:
                camera_id = f"graphics:camera:{camera.get('id', 'active')}"
                nodes.append(
                    GraphicsRuntimeAdapter._runtime_node(
                        camera_id, "RuntimeCamera", camera, evidence
                    )
                )
            else:
                camera_id = None
            for item in runtime.get("objects", []):
                node_id = f"graphics:object:{item['id']}"
                nodes.append(
                    GraphicsRuntimeAdapter._runtime_node(
                        node_id, "RuntimeSceneObject", item, evidence
                    )
                )
                for canvas in snapshot["canvases"]:
                    edges.append(
                        {
                            "source": node_id,
                            "target": f"graphics:canvas:{canvas['id']}",
                            "type": "RENDERS_TO",
                            "authority": "OBSERVED",
                            "evidence_references": evidence,
                        }
                    )
                if camera_id:
                    edges.append(
                        {
                            "source": camera_id,
                            "target": node_id,
                            "type": "CAMERA_VIEWS",
                            "authority": "OBSERVED",
                            "evidence_references": evidence,
                        }
                    )
        return {
            "schema": "vision.graphics-frame-graph/v1",
            "graph_type": "GraphicsFrameGraph",
            "authority": "OBSERVED",
            "nodes": nodes,
            "edges": edges,
            "frames": frames,
            "runtime": snapshot,
            "surface_classification": [
                {
                    "canvas_id": canvas["id"],
                    "surface": {
                        "webgl": "WebGL",
                        "experimental-webgl": "WebGL",
                        "webgl2": "WebGL2",
                        "webgpu": "WebGPU",
                        "2d": "Canvas2D",
                    }.get(canvas["contextKind"], "CanvasUnknown"),
                }
                for canvas in snapshot["canvases"]
            ],
            "materialized_gltf": None,
        }

    @staticmethod
    def _runtime_node(
        node_id: str,
        domain_type: str,
        value: dict[str, Any],
        evidence: list[dict[str, str]],
    ) -> dict[str, Any]:
        return {
            "id": node_id,
            "domain_type": domain_type,
            "spatial_bounds": value.get("bounds"),
            "transform": value.get("matrix"),
            "temporal_validity": "runtime-scene-revision",
            "evidence_references": evidence,
            "authority": "OBSERVED",
            "confidence": 1.0,
            "source_restrictions": ["explicit-runtime-hook"],
            "uncertainty": [],
            "revision_lineage": [],
            **value,
        }


class RuntimeGltfCompiler:
    @staticmethod
    def compile(scene: dict[str, Any]) -> dict[str, Any]:
        objects = [item for item in scene.get("objects", []) if item.get("positions")]
        if not objects:
            raise ValueError("runtime scene exposes no materializable mesh geometry")
        binary = bytearray()
        buffer_views = []
        accessors = []
        meshes = []
        nodes = []
        materials = []
        material_lookup: dict[str, int] = {}

        def append_bytes(data: bytes, target: int | None) -> int:
            while len(binary) % 4:
                binary.append(0)
            offset = len(binary)
            binary.extend(data)
            index = len(buffer_views)
            view: dict[str, Any] = {
                "buffer": 0,
                "byteOffset": offset,
                "byteLength": len(data),
            }
            if target:
                view["target"] = target
            buffer_views.append(view)
            return index

        for item in objects:
            positions = [float(value) for value in item["positions"]]
            if len(positions) % 3 or len(positions) < 9:
                raise ValueError(f"invalid triangle positions for runtime object {item['id']}")
            indices = [int(value) for value in item.get("indices", range(len(positions) // 3))]
            position_view = append_bytes(
                struct.pack(f"<{len(positions)}f", *positions), 34962
            )
            position_accessor = len(accessors)
            triples = list(zip(positions[0::3], positions[1::3], positions[2::3], strict=True))
            accessors.append(
                {
                    "bufferView": position_view,
                    "componentType": 5126,
                    "count": len(triples),
                    "type": "VEC3",
                    "min": [min(values) for values in zip(*triples, strict=True)],
                    "max": [max(values) for values in zip(*triples, strict=True)],
                }
            )
            index_view = append_bytes(
                struct.pack(f"<{len(indices)}H", *indices), 34963
            )
            index_accessor = len(accessors)
            accessors.append(
                {
                    "bufferView": index_view,
                    "componentType": 5123,
                    "count": len(indices),
                    "type": "SCALAR",
                    "min": [min(indices)],
                    "max": [max(indices)],
                }
            )
            material = item.get("material") or {}
            material_key = json.dumps(material, sort_keys=True)
            if material_key not in material_lookup:
                material_lookup[material_key] = len(materials)
                color = material.get("baseColorFactor", [0.8, 0.8, 0.8, 1.0])
                materials.append(
                    {
                        "name": material.get("name", f"Material_{len(materials)}"),
                        "pbrMetallicRoughness": {
                            "baseColorFactor": color,
                            "metallicFactor": float(material.get("metallicFactor", 0)),
                            "roughnessFactor": float(material.get("roughnessFactor", 0.8)),
                        },
                        "doubleSided": bool(material.get("doubleSided", True)),
                    }
                )
            meshes.append(
                {
                    "name": item.get("name", item["id"]),
                    "primitives": [
                        {
                            "attributes": {"POSITION": position_accessor},
                            "indices": index_accessor,
                            "material": material_lookup[material_key],
                            "mode": 4,
                        }
                    ],
                }
            )
            matrix = item.get("matrix")
            node: dict[str, Any] = {
                "name": item.get("name", item["id"]),
                "mesh": len(meshes) - 1,
                "extras": {
                    "visionmcpRuntimeId": item["id"],
                    "authority": "DERIVED_FROM_EXPLICIT_RUNTIME_HOOK",
                },
            }
            if matrix and len(matrix) == 16:
                node["matrix"] = [float(value) for value in matrix]
            nodes.append(node)

        camera = scene.get("camera")
        if camera:
            perspective = camera.get("perspective") or {}
            camera_index = 0
            camera_node: dict[str, Any] = {
                "name": camera.get("name", "Runtime Camera"),
                "camera": camera_index,
            }
            if camera.get("matrix") and len(camera["matrix"]) == 16:
                camera_node["matrix"] = [float(value) for value in camera["matrix"]]
            nodes.append(camera_node)
            cameras = [
                {
                    "name": camera.get("name", "Runtime Camera"),
                    "type": "perspective",
                    "perspective": {
                        "yfov": float(perspective.get("yfov", math.radians(45))),
                        "znear": float(perspective.get("znear", 0.1)),
                        "zfar": float(perspective.get("zfar", 1000)),
                        "aspectRatio": float(perspective.get("aspectRatio", 1.0)),
                    },
                }
            ]
        else:
            cameras = []
        encoded = base64.b64encode(bytes(binary)).decode()
        result: dict[str, Any] = {
            "asset": {"version": "2.0", "generator": "VisionMCP RuntimeGltfCompiler/1"},
            "scene": 0,
            "scenes": [{"nodes": list(range(len(nodes)))}],
            "nodes": nodes,
            "meshes": meshes,
            "materials": materials,
            "buffers": [
                {
                    "byteLength": len(binary),
                    "uri": f"data:application/octet-stream;base64,{encoded}",
                }
            ],
            "bufferViews": buffer_views,
            "accessors": accessors,
            "extras": {
                "sourceRevision": scene.get("revision"),
                "authority": "DERIVED_FROM_EXPLICIT_RUNTIME_HOOK",
            },
        }
        if cameras:
            result["cameras"] = cameras
        return result


class GraphicsRoundTripService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.query = ObservationQueryService(project)

    def round_trip(self, capture_id: str) -> dict[str, Any]:
        graph = self.query.graph(capture_id, "GraphicsFrameGraph")
        source = graph.get("materialized_gltf")
        if not source or not source.get("artifact_digest"):
            raise ValueError("graphics capture has no materialized glTF evidence")
        source_digest = source["artifact_digest"]
        gltf_path = self.project.root / "geometry" / f"runtime-{capture_id[:12]}.gltf"
        self.artifacts.materialize(source_digest, gltf_path)
        blend_path = (
            self.project.root / "scene" / "checkpoints" / f"runtime-{capture_id[:12]}.blend"
        )
        runner = BlenderRunner(self.project)
        imported = runner.run(
            "import_asset",
            gltf_path,
            {"source_path": str(gltf_path), "output_path": str(blend_path)},
            timeout_seconds=180,
        )
        scene = SceneStore(self.project).register_generated(
            blend_path, original_name=blend_path.name
        )
        inventory = runner.run("inspect_scene", blend_path, {}, timeout_seconds=180)
        glb_path = self.project.root / "exports" / f"runtime-{capture_id[:12]}.glb"
        exported = runner.run(
            "export_glb",
            blend_path,
            {"output_path": str(glb_path)},
            timeout_seconds=180,
        )
        validation = self.validate_glb(glb_path)
        verification_blend = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"runtime-{capture_id[:12]}-reimport.blend"
        )
        reimported = runner.run(
            "import_asset",
            glb_path,
            {
                "source_path": str(glb_path),
                "output_path": str(verification_blend),
            },
            timeout_seconds=180,
        )
        blend_record = self.artifacts.ingest_file(
            blend_path, media_type="application/x-blender"
        )
        glb_record = self.artifacts.ingest_file(glb_path, media_type="model/gltf-binary")
        report_id = hashlib.sha256(
            canonical_json(
                {
                    "capture_id": capture_id,
                    "source_gltf_digest": source_digest,
                    "blend_digest": blend_record.digest,
                    "output_glb_digest": glb_record.digest,
                }
            )
        ).hexdigest()
        report = {
            "schema": "vision.graphics-roundtrip/v1",
            "id": report_id,
            "capture_id": capture_id,
            "authority": "DERIVED",
            "source_gltf_digest": source_digest,
            "blend_digest": blend_record.digest,
            "output_glb_digest": glb_record.digest,
            "scene_id": scene["id"],
            "import": imported,
            "inventory": inventory,
            "export": exported,
            "reimport": reimported,
            "validation": validation,
            "camera_comparison": {
                "status": "STRUCTURAL_ONLY",
                "runtime_camera": (
                    graph["runtime"].get("runtimeSceneHook") or {}
                ).get("camera"),
                "blocker": "fixed-camera browser-to-Blender render calibration not supplied",
            },
            "material_comparison": {
                "status": "STRUCTURAL_ONLY",
                "runtime_materials": [
                    item.get("material")
                    for item in (
                        graph["runtime"].get("runtimeSceneHook") or {}
                    ).get("objects", [])
                ],
                "blocker": "color-managed fixed-frame residual not evaluated",
            },
            "fixed_frame_residual": {
                "status": "NOT_EVALUATED",
                "blocker": "requires a calibrated Blender render camera and lighting contract",
            },
            "performance_comparison": {
                "browser_median_frame_ms": graph["runtime"]["performance"]["medianFrameMs"],
                "blender_import_seconds": imported["worker"]["duration_seconds"],
                "status": "NON_COMPARABLE_RUNTIMES",
            },
            "accepted": False,
            "acceptance_blockers": [
                "fixed camera transform equivalence is not calibrated",
                "fixed-frame browser/Blender visual residual is not measured",
                "material color-management equivalence is not measured",
            ],
            "created_at": utc_now(),
        }
        report_record = self._ingest_json(report)
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO graphics_roundtrips("
                "id,capture_id,source_gltf_digest,blend_digest,output_glb_digest,"
                "report_digest,validation_status,created_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    report_id,
                    capture_id,
                    source_digest,
                    blend_record.digest,
                    glb_record.digest,
                    report_record["digest"],
                    "PASS" if validation["valid"] else "FAIL",
                    report["created_at"],
                ),
            )
        return {**report, "report_digest": report_record["digest"]}

    @staticmethod
    def validate_glb(path: Path) -> dict[str, Any]:
        data = path.read_bytes()
        errors = []
        document: dict[str, Any] | None = None
        if len(data) < 20 or data[:4] != b"glTF":
            errors.append("missing GLB magic")
        else:
            version, declared_length = struct.unpack_from("<II", data, 4)
            if version != 2:
                errors.append(f"unsupported GLB version: {version}")
            if declared_length != len(data):
                errors.append("declared GLB length does not match file size")
            json_length, json_type = struct.unpack_from("<II", data, 12)
            if json_type != 0x4E4F534A:
                errors.append("first GLB chunk is not JSON")
            elif 20 + json_length > len(data):
                errors.append("JSON chunk exceeds GLB length")
            else:
                try:
                    document = json.loads(data[20 : 20 + json_length].rstrip(b" \x00"))
                except json.JSONDecodeError:
                    errors.append("GLB JSON chunk is invalid")
        if document:
            if document.get("asset", {}).get("version") != "2.0":
                errors.append("glTF asset.version is not 2.0")
            if not document.get("scenes") or not document.get("nodes"):
                errors.append("glTF has no scene nodes")
            if not document.get("meshes"):
                errors.append("glTF has no meshes")
            node_count = len(document.get("nodes", []))
            for scene in document.get("scenes", []):
                if any(index < 0 or index >= node_count for index in scene.get("nodes", [])):
                    errors.append("scene references an invalid node")
            accessor_count = len(document.get("accessors", []))
            for mesh in document.get("meshes", []):
                for primitive in mesh.get("primitives", []):
                    references = list(primitive.get("attributes", {}).values())
                    if "indices" in primitive:
                        references.append(primitive["indices"])
                    if any(
                        index < 0 or index >= accessor_count for index in references
                    ):
                        errors.append("mesh primitive references an invalid accessor")
        return {
            "validator": "VisionMCP structural GLB validator v1 plus Blender importer",
            "valid": not errors,
            "errors": errors,
            "asset": document.get("asset") if document else None,
            "mesh_count": len(document.get("meshes", [])) if document else 0,
            "node_count": len(document.get("nodes", [])) if document else 0,
        }

    def _ingest_json(self, value: dict[str, Any]) -> dict[str, Any]:
        staging = self.project.root / "observations" / ".staging"
        staging.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            prefix="graphics-roundtrip-", suffix=".json", dir=staging
        ) as file:
            file.write(canonical_json(value))
            file.flush()
            return self.artifacts.ingest_file(
                Path(file.name), media_type="application/json"
            ).to_dict()
