from __future__ import annotations

import json
import math
import os
import platform
import shutil
import socket
import subprocess
import tempfile
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal
from urllib.request import urlopen

from pydantic import BaseModel, ConfigDict, Field

from blender_vision.benchmarks.nocturne import (
    NocturnePacketAuthority,
    load_nocturne_contract,
    nocturne_benchmark_root,
)
from blender_vision.benchmarks.nocturne_3d import (
    _material_class_checks,
    _silhouette_iou,
)
from blender_vision.benchmarks.nocturne_app import NocturneAppEvaluator
from blender_vision.core.config import discover_blender
from blender_vision.core.util import atomic_write_json, sha256_file
from blender_vision.geometry.gltf_validator import GlbValidator


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class LocalContractAssertion(_StrictModel):
    id: str
    surface: Literal["application", "api", "database", "browser", "blender", "glb"]
    expected: Any
    observed: Any
    passed: bool


class NocturneLocalContractGateReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-sealed-v1"]
    authority: Literal["VISIONMCP_TRUSTED_LOCAL_CONTRACT_GATE"]
    contract_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    packet_manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    status: Literal["LOCAL_PASS_EXTERNAL_UNMEASURED", "LOCAL_FAIL"]
    global_acceptance: Literal["EXTERNAL_UNMEASURED"]
    assertions: list[LocalContractAssertion]
    measured_surfaces: list[str]
    external_unmeasured_surfaces: list[str]
    fixed_public_camera_digest: str = Field(pattern=r"^[0-9a-f]{64}$")
    output_digests: dict[str, str]
    runtime: dict[str, Any]
    failure: str | None = None


def _assertion(
    identifier: str,
    surface: Literal["application", "api", "database", "browser", "blender", "glb"],
    expected: Any,
    observed: Any,
    passed: bool,
) -> LocalContractAssertion:
    return LocalContractAssertion(
        id=identifier,
        surface=surface,
        expected=expected,
        observed=observed,
        passed=passed,
    )


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def _canonical_digest(value: Any) -> str:
    import hashlib

    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


class NocturneLocalContractGate:
    """Trusted public preflight aligned to frozen NOCTURNE evaluator surfaces.

    This gate can reject a locally invalid candidate, but it deliberately cannot
    issue a global acceptance result because hidden silhouettes, the hidden
    mobile trace, and the full external performance run are evaluator-only.
    """

    external_surfaces = [
        "hidden silhouette cameras",
        "hidden mobile interaction trace",
        "external 300-second memory evaluation",
        "frozen evaluator global decision",
    ]

    def __init__(self, contract_path: Path | None = None):
        self.contract, self.contract_path = load_nocturne_contract(contract_path)

    def run(
        self,
        *,
        packet_root: Path,
        candidate_root: Path,
        output_root: Path,
    ) -> NocturneLocalContractGateReceipt:
        packet = packet_root.expanduser().resolve()
        candidate = candidate_root.expanduser().resolve()
        output = output_root.expanduser().resolve()
        if output.exists() and any(output.iterdir()):
            raise ValueError("local contract gate output must be new or empty")
        output.mkdir(parents=True, exist_ok=True)
        packet_verification = NocturnePacketAuthority(self.contract_path).verify(packet)
        contract_digest = sha256_file(self.contract_path)[0]
        started_at = datetime.now(UTC).isoformat()
        assertions: list[LocalContractAssertion] = []
        runtime: dict[str, Any] = {"host": platform.platform()}
        failure: str | None = None
        server: subprocess.Popen[str] | None = None
        server_stdout = None
        server_stderr = None
        fresh: Path | None = None
        try:
            with tempfile.TemporaryDirectory(prefix="nocturne-local-contract-gate-") as temp:
                fresh = Path(temp) / "fresh-clone"
                shutil.copytree(
                    candidate,
                    fresh,
                    ignore=shutil.ignore_patterns(
                        "node_modules",
                        "dist",
                        "data",
                        ".DS_Store",
                        "local-contract-gates",
                    ),
                )
                app_evaluator = NocturneAppEvaluator(self.contract_path)
                commands = [
                    app_evaluator._command(
                        "npm_ci",
                        ["npm", "ci"],
                        cwd=fresh,
                        output=output,
                        timeout=600,
                    ),
                    app_evaluator._command(
                        "npm_verify",
                        ["npm", "run", "verify"],
                        cwd=fresh,
                        output=output,
                        timeout=600,
                    ),
                ]
                database = output / "local-gate.sqlite3"
                environment = {
                    **os.environ,
                    "DATABASE_PATH": str(database),
                    "NODE_ENV": "test",
                }
                for identifier, command in (
                    ("migration_first", ["npm", "run", "db:migrate"]),
                    ("migration_second", ["npm", "run", "db:migrate"]),
                    ("migration_rollback", ["npm", "run", "db:rollback"]),
                    ("migration_reapply", ["npm", "run", "db:migrate"]),
                ):
                    commands.append(
                        app_evaluator._command(
                            identifier,
                            command,
                            cwd=fresh,
                            output=output,
                            timeout=120,
                            env=environment,
                        )
                    )
                assertions.append(
                    _assertion(
                        "fresh_clone_commands",
                        "application",
                        "npm ci, verify, migrate twice, rollback, and reapply all pass",
                        {
                            item.id: {
                                "exit_code": item.exit_code,
                                "elapsed_seconds": item.elapsed_seconds,
                            }
                            for item in commands
                        },
                        all(item.passed for item in commands),
                    )
                )
                port = _free_port()
                origin = f"http://127.0.0.1:{port}"
                server_stdout = (output / "server.stdout.log").open(
                    "w", encoding="utf-8"
                )
                server_stderr = (output / "server.stderr.log").open(
                    "w", encoding="utf-8"
                )
                server = subprocess.Popen(
                    ["npm", "start"],
                    cwd=fresh,
                    stdout=server_stdout,
                    stderr=server_stderr,
                    text=True,
                    env={
                        **environment,
                        "PORT": str(port),
                        "HOST": "127.0.0.1",
                    },
                )
                deadline = time.monotonic() + 30
                healthy = False
                while time.monotonic() < deadline:
                    try:
                        with urlopen(f"{origin}/api/health", timeout=1) as response:
                            healthy = response.status == 200
                        if healthy:
                            break
                    except OSError:
                        time.sleep(0.1)
                assertions.append(
                    _assertion(
                        "live_server_health",
                        "application",
                        200,
                        200 if healthy else None,
                        healthy,
                    )
                )
                runtime.update({"origin": origin, "server_process_id": server.pid})
                if healthy:
                    api = app_evaluator._api_evaluation(origin)
                    for item in app_evaluator._api_assertions(api):
                        assertions.append(
                            _assertion(
                                item.id,
                                "api",
                                item.expected,
                                item.observed,
                                item.passed,
                            )
                        )
                    assertions.extend(self._browser_probe(origin, output))
                assertions.extend(self._blender_probe(packet, candidate, output))
        except Exception as error:
            failure = f"{type(error).__name__}: {error}"
        finally:
            if server is not None:
                server.terminate()
                try:
                    server.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait(timeout=10)
            if server_stdout is not None:
                server_stdout.close()
            if server_stderr is not None:
                server_stderr.close()
            for suffix in ("", "-shm", "-wal"):
                database_path = output / f"local-gate.sqlite3{suffix}"
                if database_path.exists():
                    database_path.unlink()

        passed = failure is None and bool(assertions) and all(
            item.passed for item in assertions
        )
        output_digests = {
            path.relative_to(output).as_posix(): sha256_file(path)[0]
            for path in sorted(output.rglob("*"))
            if path.is_file()
            and path.name != "local-contract-gate.receipt.json"
            and path.suffix not in {".sqlite3", ".sqlite3-shm", ".sqlite3-wal"}
        }
        receipt = NocturneLocalContractGateReceipt(
            benchmark_id=self.contract.benchmark_id,
            authority="VISIONMCP_TRUSTED_LOCAL_CONTRACT_GATE",
            contract_sha256=contract_digest,
            packet_manifest_sha256=packet_verification["packet_manifest_sha256"],
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            status=(
                "LOCAL_PASS_EXTERNAL_UNMEASURED" if passed else "LOCAL_FAIL"
            ),
            global_acceptance="EXTERNAL_UNMEASURED",
            assertions=assertions,
            measured_surfaces=[
                "fresh clone install/build/tests",
                "database migration lifecycle",
                "exact API status semantics",
                "public routes, element types, selectors, and responsive structure",
                "real WebGL GLB load, reduced motion, and no-WebGL fallback",
                "exported/reopened Blender dimensions and hierarchy",
                "fixed public-camera silhouette IoU",
                "GLB validation and reimport",
            ],
            external_unmeasured_surfaces=self.external_surfaces,
            fixed_public_camera_digest=_canonical_digest(
                json.loads(
                    (packet / "dimension-sheet.json").read_text(encoding="utf-8")
                )["public_cameras"]
            ),
            output_digests=output_digests,
            runtime=runtime,
            failure=failure,
        )
        atomic_write_json(
            output / "local-contract-gate.receipt.json",
            receipt.model_dump(mode="json"),
        )
        return receipt

    def _browser_probe(
        self, origin: str, output: Path
    ) -> list[LocalContractAssertion]:
        from playwright.sync_api import sync_playwright

        assertions: list[LocalContractAssertion] = []
        expected_types = {
            "enter-3d": "BUTTON",
            "product-canvas": "CANVAS",
            "part-selector": "SELECT",
            "variant": "SELECT",
            "light": "INPUT",
            "orientation": "INPUT",
            "accessory": "SELECT",
            "reserve-email": "INPUT",
            "reserve-submit": "BUTTON",
        }
        routes: dict[str, Any] = {}
        observed_types: dict[str, str] = {}
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(
                channel="chrome",
                headless=True,
                args=["--disable-background-networking"],
            )
            desktop = browser.new_context(
                viewport={"width": 1440, "height": 900},
                device_scale_factor=2,
                color_scheme="dark",
            )
            page = desktop.new_page()
            for route in self.contract.application_routes:
                page.goto(f"{origin}{route}", wait_until="networkidle")
                page.wait_for_function("() => Boolean(globalThis.__NOCTURNE__)")
                routes[route] = page.evaluate(
                    """() => ({
                      route: __NOCTURNE__.route,
                      mainCount: document.querySelectorAll("main").length,
                      h1Count: document.querySelectorAll("h1").length,
                      overflow: document.documentElement.scrollWidth >
                        document.documentElement.clientWidth + 1,
                      declaredStates: __NOCTURNE__.declaredStates,
                    })"""
                )
                for identifier in self.contract.runtime_probe_contract[
                    "required_control_ids"
                ]:
                    locator = page.locator(f"#{identifier}")
                    if locator.count():
                        observed_types[identifier] = locator.evaluate(
                            "(element) => element.tagName"
                        )
            assertions.append(
                _assertion(
                    "public_routes_and_structure",
                    "browser",
                    {
                        "routes": self.contract.application_routes,
                        "main": 1,
                        "h1": 1,
                        "overflow": False,
                    },
                    routes,
                    set(routes) == set(self.contract.application_routes)
                    and all(
                        record["route"] == route
                        and record["mainCount"] == 1
                        and record["h1Count"] == 1
                        and not record["overflow"]
                        and record["declaredStates"]
                        == self.contract.application_states
                        for route, record in routes.items()
                    ),
                )
            )
            assertions.append(
                _assertion(
                    "public_element_types_and_selectors",
                    "browser",
                    expected_types,
                    {key: observed_types.get(key) for key in expected_types},
                    all(
                        observed_types.get(identifier) == tag
                        for identifier, tag in expected_types.items()
                    ),
                )
            )
            page.goto(f"{origin}/technology", wait_until="networkidle")
            part_selection_passed = True
            try:
                page.select_option("#part-selector", "glass_core")
                part_selection_passed = (
                    page.evaluate("() => __NOCTURNE__.selectedPart") == "glass_core"
                )
            except Exception:
                part_selection_passed = False
            assertions.append(
                _assertion(
                    "public_part_selector_semantics",
                    "browser",
                    {"element": "SELECT", "selected_part": "glass_core"},
                    {
                        "element": observed_types.get("part-selector"),
                        "selected_part": (
                            page.evaluate("() => __NOCTURNE__.selectedPart")
                            if part_selection_passed
                            else None
                        ),
                    },
                    part_selection_passed,
                )
            )
            page.goto(f"{origin}/", wait_until="networkidle")
            before = page.evaluate(
                "() => ({poster: __NOCTURNE__.posterVisible, glb: __NOCTURNE__.glbRequested})"
            )
            page.locator("#enter-3d").click()
            page.wait_for_function(
                "() => ['3d_ready','3d_unavailable'].includes(__NOCTURNE__.state)",
                timeout=20_000,
            )
            loaded = page.evaluate(
                "() => ({state: __NOCTURNE__.state, glb: __NOCTURNE__.glbRequested})"
            )
            assertions.append(
                _assertion(
                    "real_glb_poster_first",
                    "browser",
                    {
                        "before": {"poster": True, "glb": False},
                        "after": {"state": "3d_ready", "glb": True},
                    },
                    {"before": before, "after": loaded},
                    before == {"poster": True, "glb": False}
                    and loaded == {"state": "3d_ready", "glb": True},
                )
            )
            reduced = browser.new_context(
                viewport={"width": 390, "height": 844},
                device_scale_factor=3,
                is_mobile=True,
                has_touch=True,
                reduced_motion="reduce",
            )
            reduced_page = reduced.new_page()
            reduced_page.goto(f"{origin}/", wait_until="networkidle")
            reduced_value = reduced_page.evaluate(
                """() => ({
                  reduced: __NOCTURNE__.reducedMotion,
                  animation: __NOCTURNE__.animationEnabled,
                })"""
            )
            assertions.append(
                _assertion(
                    "reduced_motion_runtime",
                    "browser",
                    {"reduced": True, "animation": False},
                    reduced_value,
                    reduced_value == {"reduced": True, "animation": False},
                )
            )
            reduced.close()
            fallback = browser.new_context(viewport={"width": 390, "height": 844})
            fallback.add_init_script(
                """(() => {
                  const original = HTMLCanvasElement.prototype.getContext;
                  HTMLCanvasElement.prototype.getContext = function(type, ...args) {
                    if (String(type).startsWith("webgl")) return null;
                    return original.call(this, type, ...args);
                  };
                })();"""
            )
            fallback_page = fallback.new_page()
            fallback_page.goto(f"{origin}/", wait_until="networkidle")
            fallback_page.locator("#enter-3d").click()
            fallback_page.wait_for_function(
                "() => __NOCTURNE__.state === '3d_unavailable'", timeout=20_000
            )
            fallback_routes = {}
            for route in self.contract.application_routes:
                fallback_page.goto(f"{origin}{route}", wait_until="networkidle")
                fallback_routes[route] = {
                    "main": fallback_page.locator("main").count(),
                    "h1": fallback_page.locator("h1").count(),
                }
            assertions.append(
                _assertion(
                    "no_webgl_routes_remain_usable",
                    "browser",
                    "all routes retain one main and one h1",
                    fallback_routes,
                    all(
                        value == {"main": 1, "h1": 1}
                        for value in fallback_routes.values()
                    ),
                )
            )
            fallback.close()
            desktop.close()
            browser.close()
        return assertions

    def _blender_probe(
        self, packet: Path, candidate: Path, output: Path
    ) -> list[LocalContractAssertion]:
        assertions: list[LocalContractAssertion] = []
        blender = discover_blender()
        if not blender.available or not blender.path:
            return [
                _assertion(
                    "installed_blender",
                    "blender",
                    True,
                    False,
                    False,
                )
            ]
        dimensions = json.loads(
            (packet / "dimension-sheet.json").read_text(encoding="utf-8")
        )
        parts_spec = json.loads(
            (packet / "part-material-specification.json").read_text(encoding="utf-8")
        )
        public_manifest = {
            "objects": {name: {} for name in self.contract.required_parts},
            "public_cameras": dimensions["public_cameras"],
            "hidden_cameras": {},
        }
        manifest_path = output / "trusted-public-inspection-manifest.json"
        atomic_write_json(manifest_path, public_manifest)
        inspection_root = output / "trusted-public-inspection"
        inspection_script = nocturne_benchmark_root() / "evaluator/inspect_candidate.py"
        command = [
            blender.path,
            "--background",
            "--factory-startup",
            "--disable-autoexec",
            "--python-exit-code",
            "1",
            "--python",
            str(inspection_script),
            "--",
            str(candidate / "3d/nocturne-one.blend"),
            str(manifest_path),
            str(inspection_root),
            str(candidate / "public/assets/nocturne-one-hero.glb"),
            str(candidate / "public/assets/nocturne-one-low.glb"),
        ]
        completed = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=600,
            check=False,
        )
        (output / "blender.stdout.log").write_text(completed.stdout, encoding="utf-8")
        (output / "blender.stderr.log").write_text(completed.stderr, encoding="utf-8")
        assertions.append(
            _assertion(
                "blend_reopen_and_public_inspection",
                "blender",
                0,
                completed.returncode,
                completed.returncode == 0,
            )
        )
        if completed.returncode:
            return assertions
        inspection = json.loads(
            (inspection_root / "candidate-inspection.json").read_text(encoding="utf-8")
        )
        expected_dimensions = [
            float(dimensions["overall_dimensions_mm"][key])
            for key in ("width", "depth", "height")
        ]
        observed_dimensions = [
            float(value) for value in inspection["primary_dimensions_mm"]
        ]
        dimension_errors = [
            abs(observed - expected) / expected
            for observed, expected in zip(
                observed_dimensions, expected_dimensions, strict=True
            )
        ]
        assertions.append(
            _assertion(
                "exported_reopened_dimensions",
                "blender",
                {
                    "dimensions_mm": expected_dimensions,
                    "maximum_error_ratio": self.contract.geometry_gates[
                        "overall_dimension_error_ratio_maximum"
                    ],
                },
                {
                    "dimensions_mm": observed_dimensions,
                    "error_ratios": dimension_errors,
                },
                max(dimension_errors)
                <= float(
                    self.contract.geometry_gates[
                        "overall_dimension_error_ratio_maximum"
                    ]
                ),
            )
        )
        hierarchy = inspection["hierarchy"]
        assertions.append(
            _assertion(
                "root_and_hierarchy",
                "blender",
                {"root_present": True, "unparented_required_parts": []},
                hierarchy,
                hierarchy["root_present"]
                and not hierarchy["unparented_required_parts"],
            )
        )
        expected_centers = {
            name: record["center_mm"]
            for name, record in dimensions.items()
            if isinstance(record, dict) and "center_mm" in record
        }
        expected_centers.update(
            {
                name: record["center_mm"]
                for name, record in parts_spec["internal_assembly"]["parts"].items()
            }
        )
        diagonal = math.sqrt(sum(value * value for value in expected_dimensions))
        placement_errors = {
            name: math.sqrt(
                sum(
                    (float(observed) - float(expected)) ** 2
                    for observed, expected in zip(
                        inspection["parts"][name]["location"],
                        center,
                        strict=True,
                    )
                )
            )
            / diagonal
            for name, center in expected_centers.items()
            if name in inspection["parts"]
        }
        assertions.append(
            _assertion(
                "public_textual_part_placement",
                "blender",
                {
                    "maximum_diagonal_ratio": self.contract.geometry_gates[
                        "part_placement_error_diagonal_ratio_maximum"
                    ]
                },
                placement_errors,
                set(expected_centers) <= set(placement_errors)
                and max(placement_errors.values(), default=math.inf)
                <= float(
                    self.contract.geometry_gates[
                        "part_placement_error_diagonal_ratio_maximum"
                    ]
                ),
            )
        )
        material_checks = _material_class_checks(inspection["material_details"])
        assertions.append(
            _assertion(
                "material_classes",
                "blender",
                "all frozen material class checks pass",
                material_checks,
                all(material_checks.values()),
            )
        )
        animation = inspection["animation"]
        mesh = inspection["mesh_quality"]
        assertions.append(
            _assertion(
                "animation_mesh_and_texture_contract",
                "blender",
                {
                    "animation": True,
                    "mesh_issues": False,
                    "missing_textures": [],
                },
                {
                    "animation": animation,
                    "mesh_quality": mesh,
                    "missing_textures": inspection["missing_textures"],
                },
                animation["all_required_animated"]
                and animation["frame_120_deterministic"]
                and not mesh["missing_uv_objects"]
                and not mesh["non_manifold_edges"]
                and not mesh["non_finite_normal_objects"]
                and not mesh["negative_scale_objects"]
                and not inspection["missing_textures"],
            )
        )
        threshold = float(
            self.contract.geometry_gates["public_silhouette_iou_minimum"]
        )
        scores = {
            label: _silhouette_iou(
                packet / "references" / f"{label}.png",
                inspection_root / "silhouettes" / f"{label}.candidate.png",
            )
            for label in self.contract.public_view_labels
        }
        assertions.append(
            _assertion(
                "fixed_public_camera_silhouettes",
                "blender",
                {"every_view_minimum": threshold},
                scores,
                all(score >= threshold for score in scores.values()),
            )
        )
        required = set(self.contract.required_parts)
        glb_results = {}
        for label, relative in (
            ("hero", "public/assets/nocturne-one-hero.glb"),
            ("low", "public/assets/nocturne-one-low.glb"),
        ):
            result = GlbValidator().validate(
                candidate / relative,
                required_node_names=sorted(required),
            )
            glb_results[label] = {
                "valid": result.valid,
                "errors": result.errors,
                "required_nodes_present": required
                <= set(result.named_identity["observed_nodes"]),
                "reimport_object_count": inspection[f"{label}_glb_reimport"][
                    "object_count"
                ],
            }
        assertions.append(
            _assertion(
                "validated_reimported_glbs",
                "glb",
                "both GLBs valid, named, and reimportable",
                glb_results,
                all(
                    value["valid"]
                    and value["required_nodes_present"]
                    and value["reimport_object_count"] > 0
                    for value in glb_results.values()
                ),
            )
        )
        return assertions


def next_local_contract_gate_root(candidate_root: Path) -> Path:
    parent = candidate_root / ".visionmcp" / "local-contract-gates"
    parent.mkdir(parents=True, exist_ok=True)
    used = {
        int(path.name.split("-", 1)[1])
        for path in parent.iterdir()
        if path.is_dir()
        and path.name.startswith("gate-")
        and path.name.split("-", 1)[1].isdigit()
    }
    index = 1
    while index in used:
        index += 1
    return parent / f"gate-{index:03d}"
