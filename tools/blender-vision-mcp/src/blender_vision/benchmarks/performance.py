from __future__ import annotations

import hashlib
import math
import mimetypes
import platform
import shutil
import sqlite3
import struct
import threading
import time
import uuid
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from importlib import metadata, resources
from pathlib import Path
from typing import Any, Literal
from urllib.parse import unquote, urlparse

from PIL import Image, ImageChops
from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.core.util import atomic_write_json, canonical_json, code_revision, sha256_file
from blender_vision.geometry.gltf_validator import GlbValidator
from blender_vision.perception.browser import BrowserAdapter
from blender_vision.perception.graphics import graphics_instrumentation_script
from blender_vision.performance import (
    BoundedPerformanceRepair,
    PerformanceAssertion,
    PerformanceAuthority,
    PerformanceBudget,
    PerformanceMeasurement,
    PerformancePreservation,
    PerformanceRepairReceipt,
)

_OBSERVER_SCRIPT = r"""
(() => {
  const state = {longTasks: [], layoutShifts: []};
  Object.defineProperty(globalThis, "__VISIONMCP_PERFORMANCE_OBSERVER__", {
    value: state,
    configurable: false,
    writable: false,
  });
  try {
    new PerformanceObserver(list => {
      for (const entry of list.getEntries()) {
        state.longTasks.push({startTime: entry.startTime, duration: entry.duration});
      }
    }).observe({type: "longtask", buffered: true});
  } catch (_error) {}
  try {
    new PerformanceObserver(list => {
      for (const entry of list.getEntries()) {
        if (!entry.hadRecentInput) {
          state.layoutShifts.push({startTime: entry.startTime, value: entry.value});
        }
      }
    }).observe({type: "layout-shift", buffered: true});
  } catch (_error) {}
})();
"""


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class RepairRule(_StrictModel):
    before: str
    after: str


class PerformanceBenchmarkManifest(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: str
    fixture: str
    fixture_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    repair_path: str
    repair_preimage_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    repair_rules: list[RepairRule]
    frame_sample_count: int = Field(ge=8, le=120)
    interaction_sample_count: int = Field(ge=3, le=20)
    api_sample_count: int = Field(ge=3, le=50)
    database_sample_count: int = Field(ge=10, le=1000)
    generated_assets: dict[str, int]
    required_negative_controls: list[
        Literal[
            "transfer_budget",
            "lazy_load",
            "glb_structure",
            "preservation_gate",
        ]
    ]
    budget: PerformanceBudget

    @model_validator(mode="after")
    def fixed_contract(self) -> PerformanceBenchmarkManifest:
        if len(self.repair_rules) != len(
            {item.before for item in self.repair_rules}
        ):
            raise ValueError("performance repair preimages must be unique")
        required_assets = {
            "scene-high.glb",
            "scene-low.glb",
            "texture-high.bin",
            "texture-low.bin",
        }
        if set(self.generated_assets) != required_assets:
            raise ValueError("performance generated asset contract is incomplete")
        if len(self.required_negative_controls) != len(
            set(self.required_negative_controls)
        ):
            raise ValueError("performance negative controls must be unique")
        return self


class PerformanceBenchmarkReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: str
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    fixture_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    elapsed_seconds: float = Field(ge=0)
    status: Literal["PASS", "FAIL"]
    functional_passed: bool
    degraded_rejected: bool
    repaired_accepted: bool
    degraded: PerformanceMeasurement | None
    repaired: PerformanceMeasurement | None
    degraded_assertions: list[PerformanceAssertion]
    repaired_assertions: list[PerformanceAssertion]
    preservation: PerformancePreservation | None
    repair: PerformanceRepairReceipt | None
    negative_controls: dict[str, bool]
    host: dict[str, Any]
    runtime: dict[str, Any]
    output_digests: dict[str, str]
    claim_boundary: list[str]
    workspace: str
    failure: str | None = None


class PerformanceBenchmarkError(ValueError):
    pass


def _benchmark_root() -> Path:
    development = (
        Path(__file__).resolve().parents[3]
        / "benchmarks"
        / "100_plus"
        / "performance"
    )
    if development.is_dir():
        return development
    installed = resources.files("blender_vision").joinpath(
        "benchmarks", "data", "100_plus", "performance"
    )
    return Path(str(installed))


def _tree_digest(root: Path) -> str:
    entries = [
        {
            "path": path.relative_to(root).as_posix(),
            "sha256": sha256_file(path)[0],
        }
        for path in sorted(candidate for candidate in root.rglob("*") if candidate.is_file())
    ]
    return hashlib.sha256(canonical_json(entries)).hexdigest()


def load_performance_benchmark_manifest(
    path: Path | None = None,
) -> tuple[PerformanceBenchmarkManifest, Path, Path]:
    manifest_path = (path or (_benchmark_root() / "manifest.json")).expanduser().resolve()
    if not manifest_path.is_file():
        raise PerformanceBenchmarkError(
            f"performance benchmark manifest is missing: {manifest_path}"
        )
    manifest = PerformanceBenchmarkManifest.model_validate_json(
        manifest_path.read_text(encoding="utf-8")
    )
    fixture_relative = Path(manifest.fixture)
    if fixture_relative.is_absolute() or ".." in fixture_relative.parts:
        raise PerformanceBenchmarkError("performance fixture escaped the benchmark root")
    fixture_root = (manifest_path.parent / fixture_relative).resolve()
    if not fixture_root.is_dir() or not fixture_root.is_relative_to(
        manifest_path.parent
    ):
        raise PerformanceBenchmarkError("performance fixture is missing or escaped")
    observed = _tree_digest(fixture_root)
    if observed != manifest.fixture_sha256:
        raise PerformanceBenchmarkError(
            f"performance fixture digest mismatch: expected {manifest.fixture_sha256}, "
            f"observed {observed}"
        )
    repair_path = (fixture_root / manifest.repair_path).resolve()
    if (
        not repair_path.is_file()
        or not repair_path.is_relative_to(fixture_root)
        or sha256_file(repair_path)[0] != manifest.repair_preimage_sha256
    ):
        raise PerformanceBenchmarkError("performance repair preimage is stale")
    return manifest, manifest_path, fixture_root


def _percentile(values: list[float], percentile: float) -> float:
    if not values:
        raise ValueError("percentile requires observations")
    ordered = sorted(float(value) for value in values)
    index = max(0, min(len(ordered) - 1, math.ceil(percentile * len(ordered)) - 1))
    return ordered[index]


def _make_glb(path: Path, binary_bytes: int) -> None:
    if binary_bytes < 36 or binary_bytes % 4:
        raise ValueError("generated GLB binary bytes must be aligned and at least 36")
    triangle = struct.pack(
        "<9f",
        -0.62,
        -0.48,
        0.0,
        0.62,
        -0.48,
        0.0,
        0.0,
        0.62,
        0.0,
    )
    binary = triangle + bytes(binary_bytes - len(triangle))
    document = {
        "asset": {"version": "2.0", "generator": "VisionMCP performance fixture/v1"},
        "buffers": [{"byteLength": len(binary)}],
        "bufferViews": [{"buffer": 0, "byteOffset": 0, "byteLength": 36, "target": 34962}],
        "accessors": [
            {
                "bufferView": 0,
                "componentType": 5126,
                "count": 3,
                "type": "VEC3",
                "min": [-0.62, -0.48, 0.0],
                "max": [0.62, 0.62, 0.0],
            }
        ],
        "meshes": [
            {
                "name": "GovernedTriangleMesh",
                "primitives": [{"attributes": {"POSITION": 0}, "mode": 4}],
            }
        ],
        "nodes": [{"name": "GovernedTriangle", "mesh": 0}],
        "scenes": [{"name": "PerformanceScene", "nodes": [0]}],
        "scene": 0,
    }
    json_bytes = canonical_json(document)
    json_bytes += b" " * ((4 - len(json_bytes) % 4) % 4)
    payload = (
        struct.pack("<4sII", b"glTF", 2, 12 + 8 + len(json_bytes) + 8 + len(binary))
        + struct.pack("<II", len(json_bytes), 0x4E4F534A)
        + json_bytes
        + struct.pack("<II", len(binary), 0x004E4942)
        + binary
    )
    path.write_bytes(payload)


def _materialize_assets(root: Path, contract: dict[str, int]) -> None:
    _make_glb(root / "scene-high.glb", contract["scene-high.glb"])
    _make_glb(root / "scene-low.glb", contract["scene-low.glb"])
    (root / "texture-high.bin").write_bytes(
        bytes(contract["texture-high.bin"])
    )
    (root / "texture-low.bin").write_bytes(bytes(contract["texture-low.bin"]))


class _FixtureRuntime:
    def __init__(self, root: Path):
        self.root = root
        self.requests: list[dict[str, Any]] = []
        self.database = sqlite3.connect(":memory:", check_same_thread=False)
        self.database.execute(
            "CREATE TABLE items("
            "id INTEGER PRIMARY KEY, slug TEXT NOT NULL UNIQUE, title TEXT NOT NULL)"
        )
        self.database.executemany(
            "INSERT INTO items(slug,title) VALUES(?,?)",
            [
                ("front", "Front view"),
                ("profile", "Profile view"),
                ("detail", "Detail view"),
            ],
        )
        self.database.commit()
        self.lock = threading.Lock()

    def close(self) -> None:
        self.database.close()

    def api_payload(self) -> dict[str, Any]:
        with self.lock:
            rows = self.database.execute(
                "SELECT slug,title FROM items ORDER BY id"
            ).fetchall()
        return {
            "schema_version": "1",
            "items": [{"slug": row[0], "title": row[1]} for row in rows],
        }

    def database_samples(self, count: int) -> tuple[list[float], list[str]]:
        samples: list[float] = []
        with self.lock:
            plan = [
                str(row[3])
                for row in self.database.execute(
                    "EXPLAIN QUERY PLAN SELECT title FROM items WHERE slug=?",
                    ("detail",),
                ).fetchall()
            ]
            for _index in range(count):
                started = time.perf_counter()
                row = self.database.execute(
                    "SELECT title FROM items WHERE slug=?", ("detail",)
                ).fetchone()
                if row != ("Detail view",):
                    raise RuntimeError("database fixture returned the wrong row")
                samples.append((time.perf_counter() - started) * 1000)
        return samples, plan


@contextmanager
def _fixture_server(runtime: _FixtureRuntime) -> Iterator[str]:
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *args: Any) -> None:
            del format, args

        def do_GET(self) -> None:  # noqa: N802 - stdlib callback
            parsed = urlparse(self.path)
            path = unquote(parsed.path)
            if path == "/api/items":
                # The API runtime is identical before and after the frontend
                # repair. The benchmark may not manufacture an API improvement
                # by silently changing its server-side fixture.
                time.sleep(0.002)
                data = canonical_json(runtime.api_payload())
                media_type = "application/json"
                status = 200
            else:
                relative = Path(path.lstrip("/") or "index.html")
                candidate = (runtime.root / relative).resolve()
                if (
                    not candidate.is_relative_to(runtime.root)
                    or not candidate.is_file()
                    or candidate.is_symlink()
                ):
                    data = b"not found"
                    media_type = "text/plain"
                    status = 404
                else:
                    data = candidate.read_bytes()
                    media_type = (
                        mimetypes.guess_type(candidate.name)[0]
                        or "application/octet-stream"
                    )
                    status = 200
            runtime.requests.append(
                {
                    "path": path,
                    "status": status,
                    "bytes": len(data),
                    "query": parsed.query,
                }
            )
            self.send_response(status)
            self.send_header("content-type", media_type)
            self.send_header("content-length", str(len(data)))
            self.send_header("cache-control", "no-store")
            self.end_headers()
            self.wfile.write(data)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def _metric(metrics: list[dict[str, Any]], name: str) -> float:
    for item in metrics:
        if item.get("name") == name:
            return float(item.get("value", 0))
    return 0.0


def _path_set(records: list[dict[str, Any]]) -> list[str]:
    return sorted(
        {
            str(record["path"])
            for record in records
            if record["status"] < 400 and record["path"] != "/favicon.ico"
        }
    )


class PerformanceBenchmarkRunner:
    def __init__(self, manifest_path: Path | None = None):
        self.manifest, self.manifest_path, self.fixture_root = (
            load_performance_benchmark_manifest(manifest_path)
        )
        self.authority = PerformanceAuthority(self.manifest.budget)

    def run(self, output_root: Path) -> PerformanceBenchmarkReceipt:
        output_root = output_root.expanduser().resolve()
        if output_root.exists() and any(output_root.iterdir()):
            raise PerformanceBenchmarkError(
                f"performance benchmark output must be new or empty: {output_root}"
            )
        output_root.mkdir(parents=True, exist_ok=True)
        started_at = datetime.now(UTC).isoformat()
        started = time.monotonic()
        source_head = code_revision(Path(__file__).resolve().parents[3])
        if len(source_head) != 40:
            raise PerformanceBenchmarkError(
                "performance benchmark requires a full Git source revision"
            )
        manifest_sha256 = sha256_file(self.manifest_path)[0]
        workspace = output_root / "workspace"
        degraded: PerformanceMeasurement | None = None
        repaired: PerformanceMeasurement | None = None
        repair: PerformanceRepairReceipt | None = None
        preservation: PerformancePreservation | None = None
        degraded_assertions: list[PerformanceAssertion] = []
        repaired_assertions: list[PerformanceAssertion] = []
        negative_controls: dict[str, bool] = {}
        output_digests: dict[str, str] = {}
        runtime_evidence: dict[str, Any] = {}
        failure: str | None = None
        try:
            degraded_root = workspace / "degraded"
            repaired_root = workspace / "repaired"
            shutil.copytree(self.fixture_root, degraded_root)
            shutil.copytree(self.fixture_root, repaired_root)
            _materialize_assets(degraded_root, self.manifest.generated_assets)
            _materialize_assets(repaired_root, self.manifest.generated_assets)
            repair = BoundedPerformanceRepair(
                relative_path=self.manifest.repair_path,
                expected_before_sha256=self.manifest.repair_preimage_sha256,
                replacements=[
                    (item.before, item.after) for item in self.manifest.repair_rules
                ],
            ).apply(repaired_root)
            degraded = self._measure(
                root=degraded_root,
                variant="degraded",
                output_root=output_root,
            )
            repaired = self._measure(
                root=repaired_root,
                variant="repaired",
                output_root=output_root,
            )
            preservation = self._preservation(
                degraded,
                repaired,
                output_root=output_root,
            )
            degraded_assertions = self.authority.evaluate(degraded)
            repaired_assertions = self.authority.evaluate(
                repaired, preservation=preservation
            )
            negative_controls = self._negative_controls(repaired, preservation)
            atomic_write_json(
                output_root / "repair.receipt.json",
                repair.model_dump(mode="json"),
            )
            atomic_write_json(
                output_root / "negative-controls.json",
                negative_controls,
            )
            runtime_evidence = {
                "playwright_version": metadata.version("playwright"),
                "browser_engine": repaired.browser_engine,
                "browser_version": repaired.browser_version,
                "browser_executable": repaired.browser_executable,
                "browser_executable_sha256": repaired.browser_executable_sha256,
                "graphics_instrumentation": "GraphicsRuntimeAdapter/v1",
                "glb_validator": "visionmcp-glb-validator/v1",
                "database": sqlite3.sqlite_version,
                "network_scope": "loopback-only",
                "external_network_used": False,
            }
            for name in (
                "degraded.measurement.json",
                "repaired.measurement.json",
                "repair.receipt.json",
                "preservation.json",
                "negative-controls.json",
                "degraded.png",
                "repaired.png",
            ):
                path = output_root / name
                if path.is_file():
                    output_digests[name] = sha256_file(path)[0]
        except Exception as error:  # keep a source-bound failure record
            failure = f"{type(error).__name__}: {error}"
            atomic_write_json(
                output_root / "performance.failure.json",
                {
                    "schema_version": "1",
                    "source_git_head": source_head,
                    "manifest_sha256": manifest_sha256,
                    "failure": failure,
                    "completed_at": datetime.now(UTC).isoformat(),
                },
            )

        degraded_rejected = bool(degraded_assertions) and any(
            not item.passed for item in degraded_assertions
        )
        repaired_accepted = bool(repaired_assertions) and all(
            item.passed for item in repaired_assertions
        )
        functional_passed = (
            degraded_rejected
            and repaired_accepted
            and preservation is not None
            and preservation.passed
            and all(
                negative_controls.get(identifier) is True
                for identifier in self.manifest.required_negative_controls
            )
            and failure is None
        )
        receipt = PerformanceBenchmarkReceipt(
            benchmark_id=self.manifest.benchmark_id,
            source_git_head=source_head,
            manifest_sha256=manifest_sha256,
            fixture_sha256=self.manifest.fixture_sha256,
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            elapsed_seconds=time.monotonic() - started,
            status="PASS" if functional_passed else "FAIL",
            functional_passed=functional_passed,
            degraded_rejected=degraded_rejected,
            repaired_accepted=repaired_accepted,
            degraded=degraded,
            repaired=repaired,
            degraded_assertions=degraded_assertions,
            repaired_assertions=repaired_assertions,
            preservation=preservation,
            repair=repair,
            negative_controls=negative_controls,
            host={
                "platform": platform.platform(),
                "python": platform.python_version(),
            },
            runtime=runtime_evidence,
            output_digests=output_digests,
            claim_boundary=[
                "Proves measured rejection and bounded repair on the owned fixed fixture.",
                "Does not prove universal performance on arbitrary applications, devices, "
                "networks, databases, or unseen 3D scenes.",
                "Browser measurements use installed Chromium on this host; no mobile "
                "physical-device runtime is claimed.",
                "GLB validation is structural and named-identity based, not a perceptual "
                "equivalence claim.",
            ],
            workspace=str(workspace),
            failure=failure,
        )
        atomic_write_json(
            output_root / "performance.receipt.json",
            receipt.model_dump(mode="json"),
        )
        return receipt

    def _measure(
        self,
        *,
        root: Path,
        variant: Literal["degraded", "repaired"],
        output_root: Path,
    ) -> PerformanceMeasurement:
        from playwright.sync_api import sync_playwright

        runtime = _FixtureRuntime(root)
        run_id = uuid.uuid4().hex
        try:
            with _fixture_server(runtime) as origin, sync_playwright() as playwright:
                adapter = BrowserAdapter()
                target = adapter.normalize_target(
                    {"url": f"{origin}/index.html?run={run_id}"}
                )
                config = adapter.normalize_config(
                    target,
                    {
                        "allowed_origins": [origin],
                        "allow_private_network": True,
                        "engine": "chromium",
                        "channel": "chrome",
                        "viewport": {"width": 1024, "height": 768},
                        "device_scale_factor": 2,
                        "color_scheme": "dark",
                        "wait_until": "networkidle",
                    },
                )
                launch = BrowserAdapter._launch_options(config)
                launch["args"] = [
                    *launch.get("args", []),
                    "--enable-precise-memory-info",
                    "--js-flags=--expose-gc",
                ]
                browser = playwright.chromium.launch(**launch)
                context = browser.new_context(
                    **BrowserAdapter._context_options(config)
                )
                context.add_init_script(graphics_instrumentation_script())
                context.add_init_script(_OBSERVER_SCRIPT)
                page = context.new_page()
                console_errors: list[str] = []
                page.on(
                    "console",
                    lambda message: (
                        console_errors.append(message.text)
                        if message.type == "error"
                        else None
                    ),
                )
                page.on("pageerror", lambda error: console_errors.append(str(error)))
                cdp = context.new_cdp_session(page)
                cdp.send("Performance.enable")
                before_metrics = cdp.send("Performance.getMetrics")["metrics"]
                request_start = len(runtime.requests)
                page.goto(
                    target["url"],
                    wait_until=config["wait_until"],
                    timeout=config["timeout_ms"],
                )
                page.wait_for_function(
                    "() => globalThis.__VISIONMCP_PERFORMANCE__?.state.ready === true"
                )
                initial_records = runtime.requests[request_start:]
                initial_end = len(runtime.requests)
                after_metrics = cdp.send("Performance.getMetrics")["metrics"]
                state = page.evaluate(
                    "() => ({...globalThis.__VISIONMCP_PERFORMANCE__.state})"
                )
                screenshot = page.screenshot(type="png")
                screenshot_path = output_root / f"{variant}.png"
                screenshot_path.write_bytes(screenshot)
                aria = page.locator("body").aria_snapshot()
                aria_sha256 = hashlib.sha256(aria.encode("utf-8")).hexdigest()
                frame_durations = page.evaluate(
                    "(count) => globalThis.__VISIONMCP_PERFORMANCE__.sampleFrames(count)",
                    self.manifest.frame_sample_count,
                )
                page.evaluate("() => globalThis.gc?.()")
                heap_before = max(
                    0, int(page.evaluate("() => performance.memory?.usedJSHeapSize || 0"))
                )
                interaction_samples: list[float] = []
                for index in range(self.manifest.interaction_sample_count):
                    interaction_samples.append(
                        float(
                            page.evaluate(
                                """async expected => {
                                  const started = performance.now();
                                  document.querySelector("#inspect").click();
                                  while (
                                    globalThis.__VISIONMCP_PERFORMANCE__.state.interactionCount
                                      < expected
                                  ) {
                                    await new Promise(resolve => setTimeout(resolve, 1));
                                  }
                                  await new Promise(resolve =>
                                    requestAnimationFrame(() => resolve())
                                  );
                                  return performance.now() - started;
                                }""",
                                index + 1,
                            )
                        )
                    )
                page.evaluate("() => globalThis.gc?.()")
                heap_after = max(
                    0, int(page.evaluate("() => performance.memory?.usedJSHeapSize || 0"))
                )
                state = page.evaluate(
                    "() => ({...globalThis.__VISIONMCP_PERFORMANCE__.state})"
                )
                behavior = {
                    "payload": state["behavior"],
                    "visible_output": page.locator("#result").evaluate(
                        "(element) => element.value"
                    ),
                }
                api_payload = state["behavior"]
                intent_records = runtime.requests[initial_end:]
                api_samples = page.evaluate(
                    """async ({count, runId}) => {
                      const samples = [];
                      for (let index = 0; index < count; index += 1) {
                        const started = performance.now();
                        const response = await fetch(
                          `api/items?run=${encodeURIComponent(runId)}&sample=${index}`
                        );
                        await response.json();
                        samples.push(performance.now() - started);
                      }
                      return samples;
                    }""",
                    {"count": self.manifest.api_sample_count, "runId": run_id},
                )
                observed = page.evaluate(
                    """() => ({
                      observer: globalThis.__VISIONMCP_PERFORMANCE_OBSERVER__,
                      graphics: globalThis.__VISIONMCP_GRAPHICS_RECORDS__,
                    })"""
                )
                browser_version = browser.version
                context.close()

                reduced = browser.new_context(
                    **BrowserAdapter._context_options(
                        config, reduced_motion="reduce"
                    )
                )
                reduced_page = reduced.new_page()
                reduced_page.goto(target["url"], wait_until="networkidle")
                reduced_page.wait_for_function(
                    "() => globalThis.__VISIONMCP_PERFORMANCE__?.state.ready === true"
                )
                reduced_motion_honored = bool(
                    reduced_page.evaluate(
                        """async () => {
                          await new Promise(resolve => setTimeout(resolve, 50));
                          const state = globalThis.__VISIONMCP_PERFORMANCE__.state;
                          return state.reducedMotionHonored
                            && !state.animationEnabled
                            && state.animationFrameCount === 0;
                        }"""
                    )
                )
                reduced.close()

                dpr_options = BrowserAdapter._context_options(config)
                dpr_options["device_scale_factor"] = 3
                dpr_context = browser.new_context(**dpr_options)
                dpr_page = dpr_context.new_page()
                dpr_page.goto(target["url"], wait_until="networkidle")
                dpr_page.wait_for_function(
                    "() => globalThis.__VISIONMCP_PERFORMANCE__?.state.ready === true"
                )
                adaptive_dpr = bool(
                    dpr_page.evaluate(
                        """() => {
                          const canvas = document.querySelector("#scene");
                          const bounds = canvas.getBoundingClientRect();
                          const state = globalThis.__VISIONMCP_PERFORMANCE__.state;
                          return devicePixelRatio === 3
                            && state.effectiveDpr === 2
                            && Math.abs(canvas.width / bounds.width - 2) < 0.001
                            && canvas.width === 1280
                            && canvas.height === 720;
                        }"""
                    )
                )
                dpr_context.close()

                fallback_context = browser.new_context(
                    **BrowserAdapter._context_options(config)
                )
                fallback_context.add_init_script(
                    """(() => {
                      const original = HTMLCanvasElement.prototype.getContext;
                      HTMLCanvasElement.prototype.getContext = function(type, ...args) {
                        if (String(type).startsWith("webgl")) return null;
                        return original.call(this, type, ...args);
                      };
                    })();"""
                )
                fallback_page = fallback_context.new_page()
                fallback_page.goto(target["url"], wait_until="networkidle")
                fallback_page.wait_for_function(
                    "() => globalThis.__VISIONMCP_PERFORMANCE__?.state.ready === true"
                )
                no_webgl_fallback = bool(
                    fallback_page.locator("#fallback").is_visible()
                    and fallback_page.locator("#scene").is_hidden()
                )
                fallback_context.close()
                browser.close()

            database_samples, database_plan = runtime.database_samples(
                self.manifest.database_sample_count
            )
            selected_name = (
                "scene-high.glb"
                if state["selectedGlb"] == "HIGH"
                else "scene-low.glb"
                if state["selectedGlb"] == "LOW"
                else ""
            )
            if not selected_name:
                raise RuntimeError("performance fixture did not select a GLB")
            glb = GlbValidator().validate(
                root / selected_name,
                required_node_names=["GovernedTriangle"],
                required_mesh_names=["GovernedTriangleMesh"],
            )
            dropped = [
                float(value)
                for value in frame_durations
                if float(value) > self.manifest.budget.frame_p95_ms
            ]
            long_tasks = observed["observer"]["longTasks"]
            layout_shifts = observed["observer"]["layoutShifts"]
            graphics = observed["graphics"] or {}
            texture_memory = sum(
                max(0, int(item.get("width", 0)))
                * max(0, int(item.get("height", 0)))
                * 4
                for item in graphics.get("textureUploads", [])
            )
            if texture_memory != int(state["selectedTextureBytes"]):
                raise RuntimeError(
                    "observed WebGL texture allocation disagrees with fetched texture bytes"
                )
            script_delta = max(
                0.0,
                (
                    _metric(after_metrics, "ScriptDuration")
                    - _metric(before_metrics, "ScriptDuration")
                )
                * 1000,
            )
            measurement = PerformanceMeasurement(
                variant=variant,
                browser_engine="chromium",
                browser_version=browser_version,
                browser_executable=str(config["resolved_executable_path"]),
                browser_executable_sha256=str(config["executable_sha256"]),
                initial_transfer_bytes=sum(
                    int(item["bytes"])
                    for item in initial_records
                    if item["status"] < 400
                ),
                javascript_execution_ms=max(
                    float(state["javascriptExecutionMs"]), script_delta
                ),
                cdp_script_duration_ms=script_delta,
                selected_glb=selected_name,
                selected_glb_bytes=glb.size,
                texture_memory_bytes=texture_memory,
                shader_compilation_ms=float(state["shaderCompilationMs"]),
                shader_source_count=len(graphics.get("shaderSources", [])),
                draw_call_count=len(graphics.get("drawCalls", [])),
                frame_sample_count=len(frame_durations),
                frame_p95_ms=_percentile(frame_durations, 0.95),
                dropped_frame_ratio=len(dropped) / len(frame_durations),
                long_task_count=len(long_tasks),
                long_task_total_ms=sum(float(item["duration"]) for item in long_tasks),
                cumulative_layout_shift=sum(
                    float(item["value"]) for item in layout_shifts
                ),
                interaction_samples_ms=interaction_samples,
                interaction_p95_ms=_percentile(interaction_samples, 0.95),
                javascript_heap_growth_bytes=max(0, heap_after - heap_before),
                retained_allocation_bytes=int(state["retainedAllocationBytes"]),
                api_samples_ms=[float(value) for value in api_samples],
                api_p95_ms=_percentile(api_samples, 0.95),
                database_query_samples_ms=database_samples,
                database_query_p95_ms=_percentile(database_samples, 0.95),
                database_query_plan=database_plan,
                database_uses_index=any(
                    "INDEX" in item.upper() for item in database_plan
                ),
                initial_resource_paths=_path_set(initial_records),
                intent_resource_paths=_path_set(intent_records),
                eager_3d_asset_on_initial_load=any(
                    item["path"].endswith(".glb") for item in initial_records
                ),
                lazy_3d_asset_after_intent=any(
                    item["path"].endswith(".glb") for item in intent_records
                ),
                lod_level=state["selectedGlb"],
                adaptive_dpr=adaptive_dpr,
                effective_dpr=float(state["effectiveDpr"]),
                reduced_motion_honored=reduced_motion_honored,
                no_webgl_fallback=no_webgl_fallback,
                webgl_observed=bool(graphics.get("contexts")),
                screenshot_sha256=hashlib.sha256(screenshot).hexdigest(),
                aria_sha256=aria_sha256,
                behavior_sha256=hashlib.sha256(canonical_json(behavior)).hexdigest(),
                api_contract_sha256=hashlib.sha256(
                    canonical_json(api_payload)
                ).hexdigest(),
                selected_glb_sha256=glb.sha256,
                selected_glb_valid=glb.valid,
                selected_glb_node_names=glb.named_identity["observed_nodes"],
                selected_glb_mesh_names=glb.named_identity["observed_meshes"],
                console_errors=console_errors,
            )
            atomic_write_json(
                output_root / f"{variant}.measurement.json",
                measurement.model_dump(mode="json"),
            )
            return measurement
        finally:
            runtime.close()

    @staticmethod
    def _preservation(
        degraded: PerformanceMeasurement,
        repaired: PerformanceMeasurement,
        *,
        output_root: Path,
    ) -> PerformancePreservation:
        degraded_image = Image.open(output_root / "degraded.png").convert("RGBA")
        repaired_image = Image.open(output_root / "repaired.png").convert("RGBA")
        if degraded_image.size != repaired_image.size:
            maximum_error = 255
        else:
            extrema = ImageChops.difference(degraded_image, repaired_image).getextrema()
            maximum_error = max(channel[1] for channel in extrema)
        preservation = PerformancePreservation(
            screenshot_equal=(
                degraded.screenshot_sha256 == repaired.screenshot_sha256
            ),
            screenshot_max_channel_error=maximum_error,
            aria_equal=degraded.aria_sha256 == repaired.aria_sha256,
            behavior_equal=degraded.behavior_sha256 == repaired.behavior_sha256,
            api_contract_equal=(
                degraded.api_contract_sha256 == repaired.api_contract_sha256
            ),
            glb_named_identity_equal=(
                degraded.selected_glb_node_names == repaired.selected_glb_node_names
                and degraded.selected_glb_mesh_names
                == repaired.selected_glb_mesh_names
            ),
            degraded_glb_valid=degraded.selected_glb_valid,
            repaired_glb_valid=repaired.selected_glb_valid,
        )
        atomic_write_json(
            output_root / "preservation.json",
            {
                **preservation.model_dump(mode="json"),
                "passed": preservation.passed,
            },
        )
        return preservation

    def _negative_controls(
        self,
        repaired: PerformanceMeasurement,
        preservation: PerformancePreservation,
    ) -> dict[str, bool]:
        controls: dict[str, bool] = {}
        transfer = repaired.model_copy(
            update={
                "initial_transfer_bytes": self.manifest.budget.initial_transfer_bytes
                + 1
            }
        )
        controls["transfer_budget"] = any(
            item.id == "initial_transfer_bytes" and not item.passed
            for item in self.authority.evaluate(transfer)
        )
        lazy = repaired.model_copy(
            update={
                "eager_3d_asset_on_initial_load": True,
                "lazy_3d_asset_after_intent": False,
            }
        )
        lazy_assertions = self.authority.evaluate(lazy)
        controls["lazy_load"] = all(
            any(item.id == identifier and not item.passed for item in lazy_assertions)
            for identifier in ("initial_3d_asset_is_lazy", "intent_loads_3d_asset")
        )
        corrupt_glb = repaired.model_copy(update={"selected_glb_valid": False})
        controls["glb_structure"] = any(
            item.id == "glb_structurally_valid" and not item.passed
            for item in self.authority.evaluate(corrupt_glb)
        )
        corrupt_preservation = preservation.model_copy(
            update={"screenshot_equal": False, "screenshot_max_channel_error": 1}
        )
        controls["preservation_gate"] = any(
            item.id == "all_preservation_gates" and not item.passed
            for item in self.authority.evaluate(
                repaired, preservation=corrupt_preservation
            )
        )
        return controls
