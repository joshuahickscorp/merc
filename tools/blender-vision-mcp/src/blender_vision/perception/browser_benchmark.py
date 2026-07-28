from __future__ import annotations

import hashlib
import json
import platform
import threading
import time
import traceback
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from importlib import resources
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.core.util import atomic_write_json, canonical_json, code_revision, sha256_file
from blender_vision.perception.browser import BrowserAdapter
from blender_vision.perception.bus import AdapterRegistry, CaptureBus
from blender_vision.perception.experience import BrowserExperienceAdapter
from blender_vision.projects.store import ProjectStore


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class BrowserEngineSpec(_StrictModel):
    id: Literal["chromium", "firefox", "webkit"]
    channel: str | None
    installation_command: str


class BrowserProfileSpec(_StrictModel):
    id: str
    engine: Literal["chromium", "firefox", "webkit"]
    configuration: dict[str, Any]
    expected_environment_state: dict[str, Any]


class BrowserAcceptance(_StrictModel):
    required_primary_engine: str
    minimum_passed_engines: int = Field(ge=1)
    minimum_additional_engines: int = Field(ge=1)
    minimum_state_count: int = Field(ge=1)
    minimum_interaction_count: int = Field(ge=1)
    required_input_modes: list[str]
    responsive_observation_count: int = Field(ge=1)
    motion_timeline_count: int = Field(ge=1)
    maximum_critical_or_serious_accessibility_issues: int = Field(ge=0)
    accepted_keyboard_statuses: list[str]


class BrowserBenchmarkManifest(_StrictModel):
    schema_version: Literal["1"] = "1"
    fixture: str
    fixture_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    engines: list[BrowserEngineSpec]
    experience_configuration: dict[str, Any]
    profiles: list[BrowserProfileSpec]
    acceptance: BrowserAcceptance

    @model_validator(mode="after")
    def unique_identifiers(self) -> BrowserBenchmarkManifest:
        engine_ids = [engine.id for engine in self.engines]
        profile_ids = [profile.id for profile in self.profiles]
        if len(engine_ids) != len(set(engine_ids)):
            raise ValueError("browser engine IDs must be unique")
        if len(profile_ids) != len(set(profile_ids)):
            raise ValueError("browser profile IDs must be unique")
        if self.acceptance.required_primary_engine not in engine_ids:
            raise ValueError("required_primary_engine is not in the engine registry")
        if any(profile.engine not in engine_ids for profile in self.profiles):
            raise ValueError("browser profile references an unregistered engine")
        return self


class BrowserAssertion(_StrictModel):
    id: str
    passed: bool
    expected: Any
    observed: Any


class BrowserRuntimeResult(_StrictModel):
    id: str
    status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"]
    elapsed_seconds: float = Field(ge=0)
    capture_id: str | None
    browser_version: str | None
    executable_path: str | None
    executable_sha256: str | None
    assertions: list[BrowserAssertion]
    log_path: str
    log_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    exact_resumption_contract: str | None


class BrowserBenchmarkReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    fixture_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    host: dict[str, Any]
    functional_passed: bool
    complete: bool
    engines: list[BrowserRuntimeResult]
    profiles: list[BrowserRuntimeResult]
    external_blockers: dict[str, str]


class BrowserBenchmarkError(ValueError):
    pass


def _benchmark_root() -> Path:
    development = Path(__file__).resolve().parents[3] / "benchmarks" / "100_plus" / "browser"
    if development.is_dir():
        return development
    installed = resources.files("blender_vision").joinpath(
        "benchmarks", "data", "100_plus", "browser"
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


def load_browser_benchmark_manifest(
    path: Path | None = None,
) -> tuple[BrowserBenchmarkManifest, Path, Path]:
    manifest_path = (path or (_benchmark_root() / "manifest.json")).expanduser().resolve()
    if not manifest_path.is_file():
        raise BrowserBenchmarkError(f"browser benchmark manifest is missing: {manifest_path}")
    manifest = BrowserBenchmarkManifest.model_validate_json(
        manifest_path.read_text(encoding="utf-8")
    )
    fixture_relative = Path(manifest.fixture)
    if fixture_relative.is_absolute() or ".." in fixture_relative.parts:
        raise BrowserBenchmarkError("browser fixture escaped the benchmark root")
    fixture_root = (manifest_path.parent / fixture_relative).resolve()
    if not fixture_root.is_dir() or not fixture_root.is_relative_to(manifest_path.parent):
        raise BrowserBenchmarkError("browser fixture is missing or escaped the benchmark root")
    observed = _tree_digest(fixture_root)
    if observed != manifest.fixture_sha256:
        raise BrowserBenchmarkError(
            f"browser fixture digest mismatch: expected {manifest.fixture_sha256}, "
            f"observed {observed}"
        )
    return manifest, manifest_path, fixture_root


@contextmanager
def _fixture_server(root: Path) -> Iterator[str]:
    class QuietHandler(SimpleHTTPRequestHandler):
        def log_message(self, format: str, *args: Any) -> None:
            del format, args

    server = ThreadingHTTPServer(
        ("127.0.0.1", 0),
        lambda *args, **kwargs: QuietHandler(*args, directory=root, **kwargs),
    )
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


class BrowserBenchmarkRunner:
    def __init__(self, manifest_path: Path | None = None):
        self.manifest, self.manifest_path, self.fixture_root = (
            load_browser_benchmark_manifest(manifest_path)
        )

    def run(self, output_root: Path) -> BrowserBenchmarkReceipt:
        output_root = output_root.expanduser().resolve()
        if output_root.exists() and any(output_root.iterdir()):
            raise BrowserBenchmarkError(
                f"browser benchmark output must be new or empty: {output_root}"
            )
        output_root.mkdir(parents=True, exist_ok=True)
        started = datetime.now(UTC).isoformat()
        manifest_sha256, _size = sha256_file(self.manifest_path)
        engine_results: list[BrowserRuntimeResult] = []
        profile_results: list[BrowserRuntimeResult] = []

        with _fixture_server(self.fixture_root) as origin:
            target_url = f"{origin}/index.html"
            for engine in self.manifest.engines:
                engine_results.append(
                    self._run_engine(output_root, engine, target_url, origin)
                )
            for profile in self.manifest.profiles:
                profile_results.append(
                    self._run_profile(output_root, profile, target_url, origin)
                )

        passed_engines = {
            result.id for result in engine_results if result.status == "PASS"
        }
        acceptance = self.manifest.acceptance
        primary_passed = acceptance.required_primary_engine in passed_engines
        additional_count = len(
            passed_engines - {acceptance.required_primary_engine}
        )
        profiles_passed = all(result.status == "PASS" for result in profile_results)
        functional_passed = (
            primary_passed
            and len(passed_engines) >= acceptance.minimum_passed_engines
            and additional_count >= acceptance.minimum_additional_engines
            and profiles_passed
        )
        complete = functional_passed and all(
            result.status == "PASS" for result in engine_results
        )
        blockers = {
            result.id: result.exact_resumption_contract
            for result in [*engine_results, *profile_results]
            if result.status == "BLOCKED_EXTERNAL"
            and result.exact_resumption_contract is not None
        }
        source_head = code_revision(Path(__file__).resolve().parents[3])
        if len(source_head) != 40:
            raise BrowserBenchmarkError("browser benchmark requires a full Git source revision")
        receipt = BrowserBenchmarkReceipt(
            source_git_head=source_head,
            manifest_sha256=manifest_sha256,
            fixture_sha256=self.manifest.fixture_sha256,
            started_at=started,
            completed_at=datetime.now(UTC).isoformat(),
            host={
                "platform": platform.platform(),
                "python": platform.python_version(),
                "machine": platform.machine(),
            },
            functional_passed=functional_passed,
            complete=complete,
            engines=engine_results,
            profiles=profile_results,
            external_blockers=blockers,
        )
        atomic_write_json(
            output_root / "browser-benchmark.receipt.json",
            receipt.model_dump(mode="json"),
        )
        receipt_sha256, _size = sha256_file(
            output_root / "browser-benchmark.receipt.json"
        )
        atomic_write_json(
            output_root / "browser-benchmark.receipt.sha256.json",
            {
                "schema_version": "1",
                "receipt": "browser-benchmark.receipt.json",
                "sha256": receipt_sha256,
            },
        )
        return receipt

    def _run_engine(
        self,
        output_root: Path,
        engine: BrowserEngineSpec,
        target_url: str,
        origin: str,
    ) -> BrowserRuntimeResult:
        started = time.monotonic()
        adapter = BrowserExperienceAdapter()
        config = {
            **self.manifest.experience_configuration,
            "engine": engine.id,
            "channel": engine.channel,
            "allowed_origins": [origin],
            "allow_private_network": True,
        }
        project = ProjectStore.create(
            output_root / "workspaces" / f"engine-{engine.id}",
            f"Browser benchmark {engine.id}",
        )
        registry = AdapterRegistry()
        registry.register(adapter)
        bus = CaptureBus(project, registry)
        environment: dict[str, Any] = {}
        assertions: list[BrowserAssertion] = []
        try:
            normalized_target = adapter.normalize_target({"url": target_url})
            normalized_config = adapter.normalize_config(normalized_target, config)
            environment = adapter.environment(normalized_config)
            capture = bus.observe(
                adapter.name,
                {"url": target_url},
                config,
                rights_decision="SYNTHETIC_OWNED",
            )
            assertions = self._engine_assertions(capture, bus)
            status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"] = (
                "PASS" if all(item.passed for item in assertions) else "FAIL"
            )
            capture_id = capture["capture_id"]
            browser_version = str(capture["summary"].get("browser_version") or "") or None
            log_body = json.dumps(
                {
                    "capture_id": capture_id,
                    "environment": environment,
                    "summary": capture["summary"],
                    "assertions": [
                        item.model_dump(mode="json") for item in assertions
                    ],
                    "verification": bus.verify(capture_id),
                },
                indent=2,
                sort_keys=True,
            )
            resumption = None
        except Exception as error:
            capture_id = None
            browser_version = str(environment.get("browser_version") or "") or None
            external = self._is_external_runtime_failure(error)
            status = "BLOCKED_EXTERNAL" if external else "FAIL"
            resumption = (
                f"{engine.installation_command} Re-run: "
                "BVMCP_RUN_CROSS_BROWSER_TESTS=1 uv run pytest -q "
                f"tests/test_browser_perception.py tests/test_web_experience.py "
                f"-k {engine.id}"
                if external
                else None
            )
            log_body = (
                f"status={status}\n"
                f"engine={engine.id}\n"
                f"environment={json.dumps(environment, sort_keys=True)}\n"
                f"error_type={type(error).__name__}\n"
                f"error={error}\n\n"
                f"{traceback.format_exc()}"
            )
        log_path, log_sha256 = self._write_log(
            output_root, f"engine-{engine.id}", log_body
        )
        return BrowserRuntimeResult(
            id=engine.id,
            status=status,
            elapsed_seconds=time.monotonic() - started,
            capture_id=capture_id,
            browser_version=browser_version,
            executable_path=environment.get("browser_executable"),
            executable_sha256=environment.get("browser_executable_sha256"),
            assertions=assertions,
            log_path=log_path,
            log_sha256=log_sha256,
            exact_resumption_contract=resumption,
        )

    def _run_profile(
        self,
        output_root: Path,
        profile: BrowserProfileSpec,
        target_url: str,
        origin: str,
    ) -> BrowserRuntimeResult:
        started = time.monotonic()
        adapter = BrowserAdapter()
        engine = next(item for item in self.manifest.engines if item.id == profile.engine)
        config = {
            **profile.configuration,
            "engine": engine.id,
            "channel": engine.channel,
            "allowed_origins": [origin],
            "allow_private_network": True,
            "timeout_ms": 30_000,
            "launch_timeout_ms": 15_000,
        }
        project = ProjectStore.create(
            output_root / "workspaces" / f"profile-{profile.id}",
            f"Browser profile {profile.id}",
        )
        registry = AdapterRegistry()
        registry.register(adapter)
        bus = CaptureBus(project, registry)
        environment: dict[str, Any] = {}
        assertions: list[BrowserAssertion] = []
        try:
            normalized_target = adapter.normalize_target({"url": target_url})
            normalized_config = adapter.normalize_config(normalized_target, config)
            environment = adapter.environment(normalized_config)
            capture = bus.observe(
                adapter.name,
                {"url": target_url},
                config,
                rights_decision="SYNTHETIC_OWNED",
            )
            assertions = [
                BrowserAssertion(
                    id=f"environment.{key}",
                    passed=self._same_value(
                        capture["summary"]["environment_state"].get(key), expected
                    ),
                    expected=expected,
                    observed=capture["summary"]["environment_state"].get(key),
                )
                for key, expected in sorted(profile.expected_environment_state.items())
            ]
            assertions.extend(
                [
                    BrowserAssertion(
                        id="accessibility.critical_or_serious",
                        passed=(
                            capture["summary"][
                                "accessibility_critical_or_serious_count"
                            ]
                            <= self.manifest.acceptance
                            .maximum_critical_or_serious_accessibility_issues
                        ),
                        expected=(
                            self.manifest.acceptance
                            .maximum_critical_or_serious_accessibility_issues
                        ),
                        observed=capture["summary"][
                            "accessibility_critical_or_serious_count"
                        ],
                    ),
                    BrowserAssertion(
                        id="capture.verification",
                        passed=bus.verify(capture["capture_id"])["valid"],
                        expected=True,
                        observed=bus.verify(capture["capture_id"])["valid"],
                    ),
                ]
            )
            status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"] = (
                "PASS" if all(item.passed for item in assertions) else "FAIL"
            )
            capture_id = capture["capture_id"]
            browser_version = str(capture["summary"].get("browser_version") or "") or None
            log_body = json.dumps(
                {
                    "capture_id": capture_id,
                    "environment": environment,
                    "summary": capture["summary"],
                    "assertions": [
                        item.model_dump(mode="json") for item in assertions
                    ],
                },
                indent=2,
                sort_keys=True,
            )
            resumption = None
        except Exception as error:
            capture_id = None
            browser_version = str(environment.get("browser_version") or "") or None
            external = self._is_external_runtime_failure(error)
            status = "BLOCKED_EXTERNAL" if external else "FAIL"
            resumption = engine.installation_command if external else None
            log_body = (
                f"status={status}\n"
                f"profile={profile.id}\n"
                f"environment={json.dumps(environment, sort_keys=True)}\n"
                f"error_type={type(error).__name__}\n"
                f"error={error}\n\n"
                f"{traceback.format_exc()}"
            )
        log_path, log_sha256 = self._write_log(
            output_root, f"profile-{profile.id}", log_body
        )
        return BrowserRuntimeResult(
            id=profile.id,
            status=status,
            elapsed_seconds=time.monotonic() - started,
            capture_id=capture_id,
            browser_version=browser_version,
            executable_path=environment.get("browser_executable"),
            executable_sha256=environment.get("browser_executable_sha256"),
            assertions=assertions,
            log_path=log_path,
            log_sha256=log_sha256,
            exact_resumption_contract=resumption,
        )

    def _engine_assertions(
        self,
        capture: dict[str, Any],
        bus: CaptureBus,
    ) -> list[BrowserAssertion]:
        acceptance = self.manifest.acceptance
        interactions = self._artifact_json(capture, bus, "interaction.graph")
        modes = sorted(
            {
                edge["input"]["mode"]
                for edge in interactions["edges"]
                if edge.get("status") == "OBSERVED"
            }
        )
        verification = bus.verify(capture["capture_id"])
        values = [
            (
                "states",
                capture["summary"]["state_count"] >= acceptance.minimum_state_count,
                f">={acceptance.minimum_state_count}",
                capture["summary"]["state_count"],
            ),
            (
                "interactions",
                capture["summary"]["interaction_count"]
                >= acceptance.minimum_interaction_count,
                f">={acceptance.minimum_interaction_count}",
                capture["summary"]["interaction_count"],
            ),
            (
                "input_modes",
                set(acceptance.required_input_modes) <= set(modes),
                sorted(acceptance.required_input_modes),
                modes,
            ),
            (
                "responsive",
                capture["summary"]["responsive_observation_count"]
                == acceptance.responsive_observation_count,
                acceptance.responsive_observation_count,
                capture["summary"]["responsive_observation_count"],
            ),
            (
                "motion",
                capture["summary"]["motion_timeline_count"]
                == acceptance.motion_timeline_count,
                acceptance.motion_timeline_count,
                capture["summary"]["motion_timeline_count"],
            ),
            (
                "accessibility",
                capture["summary"]["accessibility_critical_or_serious_count"]
                <= acceptance.maximum_critical_or_serious_accessibility_issues,
                acceptance.maximum_critical_or_serious_accessibility_issues,
                capture["summary"]["accessibility_critical_or_serious_count"],
            ),
            (
                "keyboard_journey",
                capture["summary"]["keyboard_journey_status"]
                in acceptance.accepted_keyboard_statuses,
                acceptance.accepted_keyboard_statuses,
                capture["summary"]["keyboard_journey_status"],
            ),
            ("capture_verification", verification["valid"], True, verification["valid"]),
        ]
        return [
            BrowserAssertion(id=identifier, passed=passed, expected=expected, observed=observed)
            for identifier, passed, expected, observed in values
        ]

    @staticmethod
    def _artifact_json(
        capture: dict[str, Any],
        bus: CaptureBus,
        role: str,
    ) -> dict[str, Any]:
        artifact = next(item for item in capture["artifacts"] if item["role"] == role)
        return json.loads(bus.artifacts.path_for(artifact["digest"]).read_text(encoding="utf-8"))

    @staticmethod
    def _same_value(observed: Any, expected: Any) -> bool:
        if isinstance(expected, float | int) and isinstance(observed, float | int):
            return abs(float(observed) - float(expected)) <= 1e-6
        return observed == expected

    @staticmethod
    def _is_external_runtime_failure(error: Exception) -> bool:
        message = str(error).lower()
        return any(
            marker in message
            for marker in (
                "browser executable is unavailable",
                "executable doesn't exist",
                "browsertype.launch: timeout",
                "host system is missing dependencies",
                "rendercompositorswgl",
            )
        )

    @staticmethod
    def _write_log(
        output_root: Path,
        name: str,
        content: str,
    ) -> tuple[str, str]:
        path = output_root / "logs" / f"{name}.log"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
        digest, _size = sha256_file(path)
        return path.relative_to(output_root).as_posix(), digest
