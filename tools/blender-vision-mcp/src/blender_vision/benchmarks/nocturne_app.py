from __future__ import annotations

import json
import os
import platform
import shutil
import socket
import statistics
import subprocess
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.benchmarks.nocturne import (
    NocturnePacketAuthority,
    SealedBuilderReceipt,
    load_nocturne_contract,
)
from blender_vision.core.errors import SecurityError
from blender_vision.core.util import atomic_write_json, code_revision, sha256_file, utc_now


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class CandidateFile(_StrictModel):
    path: str
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    size: int = Field(ge=0)


class CandidateAttempt(_StrictModel):
    id: str
    status: Literal["FAILED", "ACCEPTED"]
    retained_path: str
    receipt_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")


class NocturneCandidateBuildReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-sealed-v1"]
    authority: Literal["VISIONMCP_BUILDER_OUTPUT"]
    contract_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    packet_manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    builder_condition: str
    generated_at: str
    files: list[CandidateFile]
    attempts: list[CandidateAttempt]
    reproduction_commands: list[str]
    manual_edits_outside_receipt_chain: Literal[False]
    oracle_source_access: Literal[False]

    @model_validator(mode="after")
    def exact_files_and_attempts(self) -> NocturneCandidateBuildReceipt:
        paths = [item.path for item in self.files]
        if len(paths) != len(set(paths)):
            raise ValueError("candidate build receipt file paths must be unique")
        if not self.attempts or self.attempts[-1].status != "ACCEPTED":
            raise ValueError("candidate build receipt requires a final accepted attempt")
        if len({item.id for item in self.attempts}) != len(self.attempts):
            raise ValueError("candidate attempt IDs must be unique")
        return self


class AppAssertion(_StrictModel):
    id: str
    expected: Any
    observed: Any
    passed: bool
    severity: Literal["P0", "P1", "P2"]


class CommandReceipt(_StrictModel):
    id: str
    command: list[str]
    exit_code: int
    elapsed_seconds: float = Field(ge=0)
    stdout_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    stderr_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    passed: bool


class NocturneAppReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-sealed-v1"]
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    contract_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    packet_manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    sealed_builder_receipt_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    candidate_build_receipt_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    elapsed_seconds: float = Field(ge=0)
    status: Literal["PASS", "FAIL"]
    functional_passed: bool
    assertions: list[AppAssertion]
    commands: list[CommandReceipt]
    api: dict[str, Any]
    browser: dict[str, Any]
    performance: dict[str, Any]
    accessibility: dict[str, Any]
    output_digests: dict[str, str]
    runtime: dict[str, Any]
    claim_boundary: list[str]
    workspace: str
    failure: str | None = None


class NocturneAppEvaluationError(ValueError):
    pass


def _assertion(
    identifier: str,
    expected: Any,
    observed: Any,
    passed: bool,
    severity: Literal["P0", "P1", "P2"] = "P1",
) -> AppAssertion:
    return AppAssertion(
        id=identifier,
        expected=expected,
        observed=observed,
        passed=passed,
        severity=severity,
    )


def _percentile(values: list[float], percentile: float) -> float:
    if not values:
        raise ValueError("percentile requires observations")
    ordered = sorted(float(value) for value in values)
    index = min(len(ordered) - 1, max(0, int(len(ordered) * percentile + 0.9999) - 1))
    return ordered[index]


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def _http_json(
    method: str,
    url: str,
    *,
    payload: dict[str, Any] | None = None,
    headers: dict[str, str] | None = None,
    timeout: float = 10,
) -> tuple[int, dict[str, Any], float]:
    body = None if payload is None else json.dumps(payload).encode("utf-8")
    request = Request(
        url,
        data=body,
        method=method,
        headers={
            "Accept": "application/json",
            **({"Content-Type": "application/json"} if body is not None else {}),
            **(headers or {}),
        },
    )
    started = time.perf_counter()
    try:
        with urlopen(request, timeout=timeout) as response:
            status = response.status
            raw = response.read()
    except HTTPError as error:
        status = error.code
        raw = error.read()
    elapsed_ms = (time.perf_counter() - started) * 1000
    try:
        value = json.loads(raw.decode("utf-8"))
    except (UnicodeError, json.JSONDecodeError):
        value = {"raw": raw.decode("utf-8", errors="replace")}
    return status, value, elapsed_ms


class NocturneCandidateAuthority:
    receipt_relative = Path(".visionmcp/nocturne-build.receipt.json")

    def __init__(self, contract_path: Path | None = None):
        self.contract, self.contract_path = load_nocturne_contract(contract_path)

    def verify(
        self,
        candidate_root: Path,
        *,
        packet_manifest_sha256: str,
    ) -> tuple[NocturneCandidateBuildReceipt, dict[str, Any]]:
        root = candidate_root.expanduser().resolve()
        receipt_path = root / self.receipt_relative
        if receipt_path.is_symlink() or not receipt_path.is_file():
            raise SecurityError("NOCTURNE/ONE candidate build receipt is missing")
        receipt = NocturneCandidateBuildReceipt.model_validate_json(
            receipt_path.read_text(encoding="utf-8")
        )
        contract_digest = sha256_file(self.contract_path)[0]
        if receipt.contract_sha256 != contract_digest:
            raise SecurityError("NOCTURNE/ONE candidate contract digest is stale")
        if receipt.packet_manifest_sha256 != packet_manifest_sha256:
            raise SecurityError("NOCTURNE/ONE candidate packet digest is stale")
        registered = {item.path: item for item in receipt.files}
        observed = {
            path.relative_to(root).as_posix()
            for path in root.rglob("*")
            if path.is_file()
            and not path.is_relative_to(root / "node_modules")
            and not path.is_relative_to(root / "dist")
            and not path.is_relative_to(root / "data")
            and path != receipt_path
        }
        if set(registered) != observed:
            raise SecurityError(
                "NOCTURNE/ONE candidate receipt does not bind the exact source tree"
            )
        for relative, item in registered.items():
            path = root / relative
            if path.is_symlink():
                raise SecurityError("NOCTURNE/ONE candidate cannot contain symlinks")
            digest, size = sha256_file(path)
            if digest != item.sha256 or size != item.size:
                raise SecurityError(
                    f"NOCTURNE/ONE candidate source was substituted: {relative}"
                )
        return receipt, {
            "valid": True,
            "file_count": len(registered),
            "receipt_sha256": sha256_file(receipt_path)[0],
            "attempt_count": len(receipt.attempts),
            "failed_attempt_count": sum(
                item.status == "FAILED" for item in receipt.attempts
            ),
        }


def seal_nocturne_candidate(
    *,
    candidate_root: Path,
    packet_root: Path,
    builder_condition: str,
    attempts: list[tuple[str, Literal["FAILED", "ACCEPTED"], str]],
    contract_path: Path | None = None,
) -> NocturneCandidateBuildReceipt:
    contract, resolved_contract = load_nocturne_contract(contract_path)
    packet_verification = NocturnePacketAuthority(resolved_contract).verify(
        packet_root
    )
    root = candidate_root.expanduser().resolve()
    if not root.is_dir() or root.is_symlink():
        raise SecurityError("NOCTURNE/ONE candidate root must be a regular directory")
    receipt_path = root / NocturneCandidateAuthority.receipt_relative
    if receipt_path.exists():
        raise SecurityError("NOCTURNE/ONE candidate is already sealed")
    required = (
        root / "3d" / "nocturne-one.blend",
        root / "public" / "assets" / "nocturne-one-hero.glb",
        root / "public" / "assets" / "nocturne-one-low.glb",
        root / "package.json",
        root / "package-lock.json",
    )
    if any(path.is_symlink() or not path.is_file() for path in required):
        raise SecurityError("NOCTURNE/ONE candidate is missing required source artifacts")
    attempt_records: list[CandidateAttempt] = []
    for identifier, status, relative in attempts:
        relative_path = Path(relative)
        if relative_path.is_absolute() or ".." in relative_path.parts:
            raise SecurityError("NOCTURNE/ONE attempt receipt path escaped")
        path = root / relative_path
        if path.is_symlink() or not path.is_file():
            raise SecurityError(f"NOCTURNE/ONE attempt receipt is missing: {relative}")
        attempt_records.append(
            CandidateAttempt(
                id=identifier,
                status=status,
                retained_path=relative_path.as_posix(),
                receipt_sha256=sha256_file(path)[0],
            )
        )
    files = []
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            raise SecurityError("NOCTURNE/ONE candidate cannot contain symlinks")
        if not path.is_file():
            continue
        if (
            path.is_relative_to(root / "node_modules")
            or path.is_relative_to(root / "dist")
            or path.is_relative_to(root / "data")
            or path == receipt_path
        ):
            continue
        digest, size = sha256_file(path)
        files.append(
            CandidateFile(
                path=path.relative_to(root).as_posix(),
                sha256=digest,
                size=size,
            )
        )
    receipt = NocturneCandidateBuildReceipt(
        benchmark_id=contract.benchmark_id,
        authority="VISIONMCP_BUILDER_OUTPUT",
        contract_sha256=sha256_file(resolved_contract)[0],
        packet_manifest_sha256=packet_verification["packet_manifest_sha256"],
        builder_condition=builder_condition,
        generated_at=utc_now(),
        files=files,
        attempts=attempt_records,
        reproduction_commands=[
            "npm ci",
            "npm run verify",
            "npm run db:migrate",
            "npm run db:rollback",
            "npm start",
        ],
        manual_edits_outside_receipt_chain=False,
        oracle_source_access=False,
    )
    atomic_write_json(receipt_path, receipt.model_dump(mode="json"))
    return receipt


class NocturneAppEvaluator:
    def __init__(self, contract_path: Path | None = None):
        self.contract, self.contract_path = load_nocturne_contract(contract_path)

    def run(
        self,
        *,
        packet_root: Path,
        candidate_root: Path,
        sealed_builder_receipt_path: Path,
        hidden_mobile_trace_path: Path,
        output_root: Path,
    ) -> NocturneAppReceipt:
        packet = packet_root.expanduser().resolve()
        candidate = candidate_root.expanduser().resolve()
        output = output_root.expanduser().resolve()
        if output.exists() and any(output.iterdir()):
            raise NocturneAppEvaluationError("app evaluator output must be new or empty")
        output.mkdir(parents=True, exist_ok=True)
        packet_verification = NocturnePacketAuthority(self.contract_path).verify(packet)
        sealed_receipt_path = sealed_builder_receipt_path.expanduser().resolve()
        sealed_receipt = SealedBuilderReceipt.model_validate_json(
            sealed_receipt_path.read_text(encoding="utf-8")
        )
        contract_digest = sha256_file(self.contract_path)[0]
        if (
            sealed_receipt.status != "PASS"
            or sealed_receipt.contract_sha256 != contract_digest
        ):
            raise SecurityError("app evaluation requires a matching sealed builder receipt")
        build_receipt, candidate_verification = NocturneCandidateAuthority(
            self.contract_path
        ).verify(
            candidate,
            packet_manifest_sha256=packet_verification["packet_manifest_sha256"],
        )
        source_head = code_revision(Path(__file__).resolve().parents[3])
        if len(source_head) != 40:
            raise NocturneAppEvaluationError(
                "app evaluation requires a full Git source revision"
            )
        started_at = datetime.now(UTC).isoformat()
        started = time.monotonic()
        assertions: list[AppAssertion] = []
        commands: list[CommandReceipt] = []
        api: dict[str, Any] = {}
        browser: dict[str, Any] = {}
        performance: dict[str, Any] = {}
        accessibility: dict[str, Any] = {}
        runtime: dict[str, Any] = {}
        output_digests: dict[str, str] = {}
        failure: str | None = None
        server: subprocess.Popen[str] | None = None
        try:
            fresh = output / "fresh-clone"
            shutil.copytree(
                candidate,
                fresh,
                ignore=shutil.ignore_patterns(
                    "node_modules", "dist", "data", ".DS_Store"
                ),
            )
            commands.append(
                self._command(
                    "npm_ci",
                    ["npm", "ci"],
                    cwd=fresh,
                    output=output,
                    timeout=600,
                )
            )
            commands.append(
                self._command(
                    "npm_verify",
                    ["npm", "run", "verify"],
                    cwd=fresh,
                    output=output,
                    timeout=600,
                )
            )
            database = output / "application.sqlite3"
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
                    self._command(
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
                    "all install/build/test/migration/rollback commands pass",
                    {
                        item.id: {
                            "exit_code": item.exit_code,
                            "elapsed_seconds": item.elapsed_seconds,
                        }
                        for item in commands
                    },
                    all(item.passed for item in commands),
                    "P0",
                )
            )
            port = _free_port()
            origin = f"http://127.0.0.1:{port}"
            server_stdout = (output / "server.stdout.log").open("w", encoding="utf-8")
            server_stderr = (output / "server.stderr.log").open("w", encoding="utf-8")
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
            health_status = 0
            health_body: dict[str, Any] = {}
            while time.monotonic() < deadline:
                try:
                    health_status, health_body, _elapsed = _http_json(
                        "GET", f"{origin}/api/health", timeout=1
                    )
                    if health_status == 200:
                        break
                except OSError:
                    time.sleep(0.1)
            if health_status != 200:
                raise RuntimeError("NOCTURNE/ONE application did not become healthy")
            runtime["server_process_id"] = server.pid
            runtime["origin"] = origin
            runtime["host"] = platform.platform()
            runtime["health"] = health_body
            api = self._api_evaluation(origin)
            assertions.extend(self._api_assertions(api))
            hidden_trace = json.loads(
                hidden_mobile_trace_path.expanduser()
                .resolve()
                .read_text(encoding="utf-8")
            )
            browser, performance, accessibility = self._browser_evaluation(
                origin=origin,
                output=output,
                hidden_trace=hidden_trace,
            )
            performance["hero_glb_bytes"] = (
                candidate
                / "public"
                / "assets"
                / "nocturne-one-hero.glb"
            ).stat().st_size
            performance["mobile_lod_glb_bytes"] = (
                candidate
                / "public"
                / "assets"
                / "nocturne-one-low.glb"
            ).stat().st_size
            assertions.extend(
                self._browser_assertions(browser, performance, accessibility)
            )
            assertions.append(
                _assertion(
                    "release_receipt_verifies",
                    {
                        "valid": True,
                        "manual_edits_outside_receipt_chain": False,
                        "oracle_source_access": False,
                    },
                    {
                        **candidate_verification,
                        "manual_edits_outside_receipt_chain": (
                            build_receipt.manual_edits_outside_receipt_chain
                        ),
                        "oracle_source_access": build_receipt.oracle_source_access,
                    },
                    candidate_verification["valid"]
                    and not build_receipt.manual_edits_outside_receipt_chain
                    and not build_receipt.oracle_source_access,
                    "P0",
                )
            )
            assertions.append(
                _assertion(
                    "failed_variants_preserved",
                    "every declared attempt has a retained digest-bound receipt",
                    [item.model_dump(mode="json") for item in build_receipt.attempts],
                    all(
                        (candidate / item.retained_path).is_file()
                        and sha256_file(candidate / item.retained_path)[0]
                        == item.receipt_sha256
                        for item in build_receipt.attempts
                    ),
                )
            )
            for path in sorted(output.rglob("*")):
                if path.is_file() and not path.is_relative_to(fresh / "node_modules"):
                    output_digests[path.relative_to(output).as_posix()] = (
                        sha256_file(path)[0]
                    )
        except Exception as error:
            failure = f"{type(error).__name__}: {error}"
            atomic_write_json(
                output / "nocturne-app.failure.json",
                {
                    "schema_version": "1",
                    "failure": failure,
                    "completed_at": datetime.now(UTC).isoformat(),
                },
            )
        finally:
            if server is not None:
                server.terminate()
                try:
                    server.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait(timeout=5)
        functional_passed = (
            bool(assertions)
            and all(item.passed for item in assertions)
            and all(item.passed for item in commands)
            and failure is None
        )
        receipt = NocturneAppReceipt(
            benchmark_id=self.contract.benchmark_id,
            source_git_head=source_head,
            contract_sha256=contract_digest,
            packet_manifest_sha256=packet_verification["packet_manifest_sha256"],
            sealed_builder_receipt_sha256=sha256_file(sealed_receipt_path)[0],
            candidate_build_receipt_sha256=candidate_verification["receipt_sha256"],
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            elapsed_seconds=time.monotonic() - started,
            status="PASS" if functional_passed else "FAIL",
            functional_passed=functional_passed,
            assertions=assertions,
            commands=commands,
            api=api,
            browser=browser,
            performance=performance,
            accessibility=accessibility,
            output_digests=output_digests,
            runtime=runtime,
            claim_boundary=[
                "The evaluator installs from the lockfile in a fresh copied source tree.",
                "Browser, API, database, migration, accessibility, fallback, and performance "
                "gates execute on the real local application path.",
                "The selected accessibility scanner is deterministic but does not replace "
                "manual assistive-technology review.",
                "Performance results are specific to the recorded browser and host.",
            ],
            workspace=str(output),
            failure=failure,
        )
        atomic_write_json(
            output / "nocturne-app.receipt.json",
            receipt.model_dump(mode="json"),
        )
        return receipt

    @staticmethod
    def _command(
        identifier: str,
        command: list[str],
        *,
        cwd: Path,
        output: Path,
        timeout: int,
        env: dict[str, str] | None = None,
    ) -> CommandReceipt:
        started = time.monotonic()
        completed = subprocess.run(
            command,
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=timeout,
            env=env,
            check=False,
        )
        stdout = output / f"{identifier}.stdout.log"
        stderr = output / f"{identifier}.stderr.log"
        stdout.write_text(completed.stdout, encoding="utf-8")
        stderr.write_text(completed.stderr, encoding="utf-8")
        return CommandReceipt(
            id=identifier,
            command=command,
            exit_code=completed.returncode,
            elapsed_seconds=time.monotonic() - started,
            stdout_sha256=sha256_file(stdout)[0],
            stderr_sha256=sha256_file(stderr)[0],
            passed=completed.returncode == 0,
        )

    def _api_evaluation(self, origin: str) -> dict[str, Any]:
        configuration = {
            "variant": "ember",
            "light_intensity": 72,
            "orientation": 18,
            "accessory": "braided-cable",
        }
        valid = {
            "configuration": configuration,
            "email": "builder@example.invalid",
        }
        auth = {
            "X-NOCTURNE-ACTOR": "benchmark-actor",
            "X-NOCTURNE-PERMISSIONS": "reservation:create",
            "Idempotency-Key": "nocturne-fixed-idempotency-key",
        }
        unauthorized = _http_json(
            "POST", f"{origin}/api/reservations", payload=valid
        )
        missing_permission = _http_json(
            "POST",
            f"{origin}/api/reservations",
            payload=valid,
            headers={"X-NOCTURNE-ACTOR": "benchmark-actor"},
        )
        invalid = _http_json(
            "POST",
            f"{origin}/api/reservations",
            payload={**valid, "email": "invalid"},
            headers=auth,
        )
        transient = _http_json(
            "POST",
            f"{origin}/api/reservations",
            payload=valid,
            headers={**auth, "X-NOCTURNE-SIMULATE": "transient"},
        )
        first = _http_json(
            "POST",
            f"{origin}/api/reservations",
            payload=valid,
            headers=auth,
        )
        repeated = _http_json(
            "POST",
            f"{origin}/api/reservations",
            payload=valid,
            headers=auth,
        )
        conflict = _http_json(
            "POST",
            f"{origin}/api/reservations",
            payload={**valid, "email": "different@example.invalid"},
            headers=auth,
        )
        reservation_id = str(first[1].get("id", ""))
        own_lookup = _http_json(
            "GET",
            f"{origin}/api/reservations/{reservation_id}",
            headers={"X-NOCTURNE-ACTOR": "benchmark-actor"},
        )
        cross_actor = _http_json(
            "GET",
            f"{origin}/api/reservations/{reservation_id}",
            headers={"X-NOCTURNE-ACTOR": "other-actor"},
        )
        samples = [
            _http_json("GET", f"{origin}/api/health")[2]
            for _index in range(
                int(self.contract.performance_budget["api_sample_count"])
            )
        ]
        return {
            "unauthorized": {"status": unauthorized[0], "body": unauthorized[1]},
            "missing_permission": {
                "status": missing_permission[0],
                "body": missing_permission[1],
            },
            "invalid": {"status": invalid[0], "body": invalid[1]},
            "transient": {"status": transient[0], "body": transient[1]},
            "first": {"status": first[0], "body": first[1]},
            "repeated": {"status": repeated[0], "body": repeated[1]},
            "conflict": {"status": conflict[0], "body": conflict[1]},
            "own_lookup": {"status": own_lookup[0], "body": own_lookup[1]},
            "cross_actor": {"status": cross_actor[0], "body": cross_actor[1]},
            "latency_samples_ms": samples,
            "p95_ms": _percentile(samples, 0.95),
        }

    def _api_assertions(self, api: dict[str, Any]) -> list[AppAssertion]:
        first_id = api["first"]["body"].get("id")
        repeated_id = api["repeated"]["body"].get("id")
        return [
            _assertion(
                "api_auth_policy",
                {"unauthenticated": 401, "missing_permission": 403},
                {
                    "unauthenticated": api["unauthorized"]["status"],
                    "missing_permission": api["missing_permission"]["status"],
                },
                api["unauthorized"]["status"] == 401
                and api["missing_permission"]["status"] == 403,
                "P0",
            ),
            _assertion(
                "api_validation_and_transient_errors",
                {"validation": 400, "transient": 503},
                {
                    "validation": api["invalid"]["status"],
                    "transient": api["transient"]["status"],
                },
                api["invalid"]["status"] == 400
                and api["transient"]["status"] == 503,
                "P0",
            ),
            _assertion(
                "reservation_idempotency",
                {
                    "first": 201,
                    "repeated": 200,
                    "same_id": True,
                    "conflict": 409,
                },
                {
                    "first": api["first"]["status"],
                    "repeated": api["repeated"]["status"],
                    "same_id": bool(first_id) and first_id == repeated_id,
                    "conflict": api["conflict"]["status"],
                },
                api["first"]["status"] == 201
                and api["repeated"]["status"] == 200
                and bool(first_id)
                and first_id == repeated_id
                and api["conflict"]["status"] == 409,
                "P0",
            ),
            _assertion(
                "reservation_actor_scope",
                {"own_lookup": 200, "cross_actor": [403, 404]},
                {
                    "own_lookup": api["own_lookup"]["status"],
                    "cross_actor": api["cross_actor"]["status"],
                },
                api["own_lookup"]["status"] == 200
                and api["cross_actor"]["status"] in {403, 404},
                "P0",
            ),
            _assertion(
                "api_latency",
                {
                    "p95_ms_maximum": self.contract.performance_budget[
                        "api_p95_ms_maximum"
                    ]
                },
                {
                    "p95_ms": api["p95_ms"],
                    "sample_count": len(api["latency_samples_ms"]),
                },
                api["p95_ms"]
                <= float(
                    self.contract.performance_budget["api_p95_ms_maximum"]
                ),
            ),
        ]

    def _browser_evaluation(
        self,
        *,
        origin: str,
        output: Path,
        hidden_trace: dict[str, Any],
    ) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
        from playwright.sync_api import sync_playwright

        route_records: dict[str, Any] = {}
        accessibility_records: dict[str, Any] = {}
        observed_states: set[str] = set()
        screenshots = output / "screenshots"
        screenshots.mkdir()
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(
                channel="chrome",
                headless=True,
                args=[
                    "--disable-background-networking",
                    "--enable-precise-memory-info",
                    "--js-flags=--expose-gc",
                ],
            )
            context = browser.new_context(
                viewport={"width": 1280, "height": 800},
                color_scheme="dark",
                reduced_motion="no-preference",
            )
            context.add_init_script(
                """(() => {
                  const state = {cls: 0};
                  globalThis.__NOCTURNE_EVALUATOR__ = state;
                  try {
                    new PerformanceObserver(list => {
                      for (const entry of list.getEntries()) {
                        if (!entry.hadRecentInput) state.cls += entry.value;
                      }
                    }).observe({type: "layout-shift", buffered: true});
                  } catch (_error) {}
                })();"""
            )
            page = context.new_page()
            response_paths: list[str] = []
            page.on(
                "response",
                lambda response: response_paths.append(
                    response.url.split(origin, 1)[-1]
                    if response.url.startswith(origin)
                    else response.url
                ),
            )
            all_control_ids: set[str] = set()
            declared_states: list[str] = []
            for route in self.contract.application_routes:
                page.goto(f"{origin}{route}", wait_until="networkidle")
                page.wait_for_function("() => Boolean(globalThis.__NOCTURNE__)")
                snapshot = page.evaluate(
                    """() => {
                      const probe = globalThis.__NOCTURNE__;
                      return {
                        route: probe.route,
                        state: probe.state,
                        stateHistory: probe.stateHistory,
                        declaredStates: probe.declaredStates,
                        posterVisible: probe.posterVisible,
                        glbRequested: probe.glbRequested,
                        reducedMotion: probe.reducedMotion,
                        animationEnabled: probe.animationEnabled,
                        webglAvailable: probe.webglAvailable,
                        controls: [...document.querySelectorAll("[id]")]
                          .map(element => element.id),
                        h1Count: document.querySelectorAll("h1").length,
                        mainCount: document.querySelectorAll("main").length,
                        horizontalOverflow:
                          document.documentElement.scrollWidth
                            > document.documentElement.clientWidth + 1,
                        methods: {
                          enter3D: typeof probe.enter3D === "function",
                          sampleFrames: typeof probe.sampleFrames === "function",
                          selectPart: typeof probe.selectPart === "function",
                          setConfiguration: typeof probe.setConfiguration === "function",
                          setTestCondition: typeof probe.setTestCondition === "function",
                        },
                      };
                    }"""
                )
                declared_states = list(snapshot["declaredStates"])
                observed_states.update(snapshot["stateHistory"])
                all_control_ids.update(snapshot["controls"])
                route_records[route] = snapshot
                accessibility_records[route] = self._accessibility_scan(page)
                page.screenshot(
                    path=str(
                        screenshots
                        / f"{'home' if route == '/' else route.strip('/').replace('/', '-')}.png"
                    ),
                    full_page=True,
                )
            page.goto(f"{origin}/", wait_until="networkidle")
            page.wait_for_function("() => Boolean(globalThis.__NOCTURNE__)")
            poster_before = page.evaluate(
                "() => ({poster: __NOCTURNE__.posterVisible, glb: __NOCTURNE__.glbRequested})"
            )
            initial_paths = list(response_paths)
            page.locator("#enter-3d").click()
            page.wait_for_function(
                "() => ['3d_ready','3d_unavailable'].includes(__NOCTURNE__.state)"
            )
            ready = page.evaluate(
                """() => ({
                  state: __NOCTURNE__.state,
                  stateHistory: __NOCTURNE__.stateHistory,
                  glbRequested: __NOCTURNE__.glbRequested,
                  webglAvailable: __NOCTURNE__.webglAvailable,
                  posterVisible: __NOCTURNE__.posterVisible,
                })"""
            )
            observed_states.update(ready["stateHistory"])
            frame_durations = [
                float(value)
                for value in page.evaluate(
                    "(count) => __NOCTURNE__.sampleFrames(count)",
                    int(self.contract.performance_budget["frame_sample_count"]),
                )
            ]
            desktop_median_fps = 1000 / statistics.median(frame_durations)
            desktop_p95 = _percentile(frame_durations, 0.95)
            resources = page.evaluate(
                """() => performance.getEntriesByType("resource").map(entry => ({
                  name: new URL(entry.name).pathname,
                  initiatorType: entry.initiatorType,
                  transferSize: entry.transferSize,
                  encodedBodySize: entry.encodedBodySize,
                }))"""
            )
            cls = float(
                page.evaluate("() => globalThis.__NOCTURNE_EVALUATOR__.cls")
            )
            page.goto(f"{origin}/configurator", wait_until="networkidle")
            observed_states.update(
                page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            page.select_option("#variant", "ember")
            page.locator("#light").fill("72")
            page.locator("#light").dispatch_event("input")
            page.locator("#orientation").fill("18")
            page.locator("#orientation").dispatch_event("input")
            page.select_option("#accessory", "braided-cable")
            configured = page.evaluate(
                "() => ({config: __NOCTURNE__.config, scene: __NOCTURNE__.sceneConfig})"
            )
            page.reload(wait_until="networkidle")
            restored = page.evaluate("() => __NOCTURNE__.config")
            observed_states.update(
                page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            page.goto(f"{origin}/technology", wait_until="networkidle")
            page.select_option("#part-selector", "glass_core")
            selected_part = page.evaluate("() => __NOCTURNE__.selectedPart")
            keyboard_targets: list[str] = []
            page.goto(f"{origin}/", wait_until="networkidle")
            for _index in range(8):
                page.keyboard.press("Tab")
                keyboard_targets.append(
                    str(
                        page.evaluate(
                            "() => document.activeElement?.id || document.activeElement?.tagName"
                        )
                    )
                )
            observed_states.update(
                page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            reduced_context = browser.new_context(
                viewport={"width": 1280, "height": 800},
                reduced_motion="reduce",
                color_scheme="dark",
            )
            reduced_page = reduced_context.new_page()
            reduced_page.goto(f"{origin}/", wait_until="networkidle")
            reduced_page.wait_for_function("() => Boolean(__NOCTURNE__)")
            reduced = reduced_page.evaluate(
                """() => ({
                  reduced: __NOCTURNE__.reducedMotion,
                  animation: __NOCTURNE__.animationEnabled,
                  stateHistory: __NOCTURNE__.stateHistory,
                })"""
            )
            observed_states.update(reduced["stateHistory"])
            reduced_context.close()
            fallback_context = browser.new_context(
                viewport={"width": 1280, "height": 800},
                color_scheme="dark",
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
            fallback_routes: dict[str, Any] = {}
            for route in self.contract.application_routes:
                fallback_page.goto(f"{origin}{route}", wait_until="networkidle")
                fallback_page.wait_for_function("() => Boolean(__NOCTURNE__)")
                fallback_routes[route] = fallback_page.evaluate(
                    """() => ({
                      webgl: __NOCTURNE__.webglAvailable,
                      main: Boolean(document.querySelector("main")),
                      h1: document.querySelectorAll("h1").length,
                    })"""
                )
            fallback_page.goto(f"{origin}/", wait_until="networkidle")
            fallback_page.locator("#enter-3d").click()
            fallback_page.wait_for_function(
                "() => __NOCTURNE__.state === '3d_unavailable'"
            )
            fallback_state = fallback_page.evaluate("() => __NOCTURNE__.state")
            observed_states.update(
                fallback_page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            fallback_context.close()
            mobile_context = browser.new_context(
                viewport={
                    "width": int(hidden_trace["viewport"][0]),
                    "height": int(hidden_trace["viewport"][1]),
                },
                device_scale_factor=int(hidden_trace["device_scale_factor"]),
                is_mobile=True,
                has_touch=True,
                color_scheme="dark",
            )
            mobile_page = mobile_context.new_page()
            mobile_page.goto(f"{origin}/", wait_until="networkidle")
            mobile_page.wait_for_function("() => Boolean(__NOCTURNE__)")
            mobile_page.locator("#enter-3d").tap()
            mobile_page.wait_for_function(
                "() => ['3d_ready','3d_unavailable'].includes(__NOCTURNE__.state)"
            )
            mobile_frames = [
                float(value)
                for value in mobile_page.evaluate(
                    "(count) => __NOCTURNE__.sampleFrames(count)",
                    int(self.contract.performance_budget["frame_sample_count"]),
                )
            ]
            mobile_median_fps = 1000 / statistics.median(mobile_frames)
            mobile_p95 = _percentile(mobile_frames, 0.95)
            hidden_trace_result = self._execute_hidden_trace(
                mobile_page, origin, hidden_trace
            )
            observed_states.update(
                mobile_page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            mobile_overflow = bool(
                mobile_page.evaluate(
                    """() => document.documentElement.scrollWidth
                      > document.documentElement.clientWidth + 1"""
                )
            )
            mobile_context.close()
            scenario_context = browser.new_context(
                viewport={"width": 900, "height": 760},
                color_scheme="dark",
            )
            scenario_page = scenario_context.new_page()
            scenario_page.goto(f"{origin}/", wait_until="networkidle")
            scenario_page.evaluate(
                "() => __NOCTURNE__.setTestCondition('slow_network')"
            )
            scenario_page.locator("#enter-3d").click()
            scenario_page.wait_for_function(
                "() => __NOCTURNE__.state === 'slow_network'"
            )
            observed_states.update(
                scenario_page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            scenario_page.goto(f"{origin}/reserve", wait_until="networkidle")
            scenario_page.locator("#reserve-email").fill("invalid")
            scenario_page.locator("#reserve-submit").click()
            scenario_page.wait_for_function(
                "() => __NOCTURNE__.state === 'api_validation_error'"
            )
            scenario_page.evaluate(
                "() => __NOCTURNE__.setTestCondition('api_transient_error')"
            )
            scenario_page.locator("#reserve-email").fill(
                "transient@example.invalid"
            )
            scenario_page.locator("#reserve-submit").click()
            scenario_page.wait_for_function(
                "() => __NOCTURNE__.state === 'api_transient_error'"
            )
            scenario_page.evaluate("() => __NOCTURNE__.setTestCondition(null)")
            scenario_context.set_offline(True)
            scenario_page.locator("#reserve-email").fill("offline@example.invalid")
            scenario_page.locator("#reserve-submit").click()
            scenario_page.wait_for_function(
                "() => __NOCTURNE__.state === 'offline_retry'"
            )
            observed_states.update(
                scenario_page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            scenario_context.set_offline(False)
            scenario_page.locator("#reserve-submit").click()
            scenario_page.wait_for_function(
                "() => __NOCTURNE__.state === 'successful_reservation'"
            )
            observed_states.update(
                scenario_page.evaluate("() => __NOCTURNE__.stateHistory")
            )
            scenario_context.close()
            memory_samples: list[int] = []
            memory_started = time.monotonic()
            memory_duration = int(
                self.contract.performance_budget["interaction_loop_seconds"]
            )
            interval = int(
                self.contract.performance_budget[
                    "interaction_loop_sample_interval_seconds"
                ]
            )
            page.goto(f"{origin}/", wait_until="networkidle")
            page.locator("#enter-3d").click()
            page.wait_for_function("() => __NOCTURNE__.state === '3d_ready'")
            while time.monotonic() - memory_started < memory_duration:
                page.evaluate(
                    """() => {
                      const next = (__NOCTURNE__.config.orientation + 1) % 45;
                      __NOCTURNE__.setConfiguration({orientation: next});
                      globalThis.gc?.();
                    }"""
                )
                memory_samples.append(
                    int(
                        page.evaluate(
                            "() => performance.memory?.usedJSHeapSize || 0"
                        )
                    )
                )
                page.wait_for_timeout(interval * 1000)
            browser_version = browser.version
            browser.close()
        html_css_transfer = sum(
            int(item["transferSize"] or item["encodedBodySize"] or 0)
            for item in resources
            if item["initiatorType"] in {"css"} or item["name"] in {"/", "/index.html"}
        )
        javascript_transfer = sum(
            int(item["transferSize"] or item["encodedBodySize"] or 0)
            for item in resources
            if item["initiatorType"] == "script"
        )
        glb_paths = [
            item["name"] for item in resources if item["name"].endswith(".glb")
        ]
        memory_growth = (
            max(0, memory_samples[-1] - memory_samples[0])
            if len(memory_samples) >= 2
            else 0
        )
        browser_record = {
            "engine": "chromium",
            "version": browser_version,
            "routes": route_records,
            "declared_states": declared_states,
            "observed_states": sorted(observed_states),
            "observed_control_ids": sorted(all_control_ids),
            "poster_before_3d": poster_before,
            "ready_state": ready,
            "configuration": configured,
            "restored_configuration": restored,
            "selected_part": selected_part,
            "keyboard_targets": keyboard_targets,
            "reduced_motion": reduced,
            "fallback_routes": fallback_routes,
            "fallback_state": fallback_state,
            "hidden_mobile_trace": hidden_trace_result,
            "mobile_horizontal_overflow": mobile_overflow,
            "initial_request_paths": initial_paths,
            "glb_resource_paths": glb_paths,
        }
        performance_record = {
            "critical_html_css_transfer_bytes": html_css_transfer,
            "initial_javascript_transfer_bytes": javascript_transfer,
            "cumulative_layout_shift": cls,
            "desktop_frame_sample_count": len(frame_durations),
            "desktop_median_fps": desktop_median_fps,
            "desktop_frame_p95_ms": desktop_p95,
            "mobile_frame_sample_count": len(mobile_frames),
            "mobile_median_fps": mobile_median_fps,
            "mobile_frame_p95_ms": mobile_p95,
            "memory_duration_seconds": memory_duration,
            "memory_samples_bytes": memory_samples,
            "memory_growth_bytes": memory_growth,
            "resource_entries": resources,
        }
        critical = sum(
            finding["severity"] == "critical"
            for findings in accessibility_records.values()
            for finding in findings
        )
        serious = sum(
            finding["severity"] == "serious"
            for findings in accessibility_records.values()
            for finding in findings
        )
        accessibility_record = {
            "scanner": "NocturneAccessibilityScanner/v1",
            "routes": accessibility_records,
            "critical_violation_count": critical,
            "serious_violation_count": serious,
        }
        return browser_record, performance_record, accessibility_record

    @staticmethod
    def _accessibility_scan(page: Any) -> list[dict[str, str]]:
        return page.evaluate(
            """() => {
              const findings = [];
              const add = (id, severity, message) =>
                findings.push({id, severity, message});
              if (!document.documentElement.lang) {
                add("html-lang", "serious", "Document language is missing.");
              }
              if (document.querySelectorAll("main").length !== 1) {
                add("main-landmark", "critical", "Route requires exactly one main.");
              }
              if (document.querySelectorAll("h1").length !== 1) {
                add("h1", "serious", "Route requires exactly one level-one heading.");
              }
              for (const element of document.querySelectorAll(
                "button,a[href],input,select,textarea"
              )) {
                const name = (
                  element.getAttribute("aria-label")
                  || element.getAttribute("title")
                  || element.textContent
                  || element.id && document.querySelector(`label[for="${element.id}"]`)?.textContent
                  || ""
                ).trim();
                if (!name) {
                  add("accessible-name", "critical", `${element.tagName} has no name.`);
                }
              }
              for (const image of document.querySelectorAll("img")) {
                if (!image.hasAttribute("alt")) {
                  add("image-alt", "serious", "Image is missing alt.");
                }
              }
              if (!document.querySelector('a[href="#main"]')) {
                add("skip-link", "serious", "Skip link is missing.");
              }
              return findings;
            }"""
        )

    @staticmethod
    def _execute_hidden_trace(
        page: Any,
        origin: str,
        trace: dict[str, Any],
    ) -> dict[str, Any]:
        completed: list[str] = []
        for index, step in enumerate(trace["steps"]):
            action = step["action"]
            if action == "goto":
                page.goto(f"{origin}{step['path']}", wait_until="networkidle")
                page.wait_for_function("() => Boolean(__NOCTURNE__)")
            elif action == "wait_for":
                state = step["state"]
                page.wait_for_function(
                    "(expected) => __NOCTURNE__.state === expected",
                    arg=state,
                )
            elif action == "tap":
                page.locator(f"#{step['target']}").tap()
            elif action == "swipe":
                box = page.locator(f"#{step['target']}").bounding_box()
                if box is None:
                    raise RuntimeError("hidden trace target has no bounds")
                start_x = box["x"] + box["width"] / 2
                start_y = box["y"] + box["height"] / 2
                page.touchscreen.tap(start_x, start_y)
                page.dispatch_event(
                    f"#{step['target']}",
                    "pointermove",
                    {
                        "pointerType": "touch",
                        "clientX": start_x + step["delta"][0],
                        "clientY": start_y + step["delta"][1],
                    },
                )
            elif action == "select":
                page.select_option(f"#{step['target']}", step["value"])
            elif action == "set":
                page.locator(f"#{step['target']}").fill(str(step["value"]))
                page.locator(f"#{step['target']}").dispatch_event("input")
            elif action == "submit":
                page.locator("#reserve-email").fill("mobile@example.invalid")
                page.locator("#reserve-submit").tap()
                page.wait_for_function(
                    "() => __NOCTURNE__.state === 'successful_reservation'"
                )
            else:
                raise RuntimeError(f"unsupported hidden trace action: {action}")
            completed.append(f"{index}:{action}")
        return {
            "completed_steps": completed,
            "step_count": len(completed),
            "final_route": page.evaluate("() => __NOCTURNE__.route"),
            "final_state": page.evaluate("() => __NOCTURNE__.state"),
        }

    def _browser_assertions(
        self,
        browser: dict[str, Any],
        performance: dict[str, Any],
        accessibility: dict[str, Any],
    ) -> list[AppAssertion]:
        budget = self.contract.performance_budget
        routes = browser["routes"]
        expected_controls = set(
            self.contract.runtime_probe_contract["required_control_ids"]
        )
        observed_controls = set(browser["observed_control_ids"])
        route_passed = (
            set(routes) == set(self.contract.application_routes)
            and all(item["mainCount"] == 1 for item in routes.values())
            and all(item["h1Count"] == 1 for item in routes.values())
            and all(not item["horizontalOverflow"] for item in routes.values())
        )
        no_webgl_passed = (
            browser["fallback_state"] == "3d_unavailable"
            and all(
                not item["webgl"] and item["main"] and item["h1"] == 1
                for item in browser["fallback_routes"].values()
            )
        )
        configuration_passed = (
            browser["configuration"]["config"]
            == browser["configuration"]["scene"]
            == browser["restored_configuration"]
        )
        return [
            _assertion(
                "required_routes_and_states",
                {
                    "routes": self.contract.application_routes,
                "states": self.contract.application_states,
                },
                {
                    "routes": sorted(routes),
                    "states": browser["declared_states"],
                    "observed_states": browser["observed_states"],
                },
                set(routes) == set(self.contract.application_routes)
                and browser["declared_states"] == self.contract.application_states
                and set(self.contract.application_states)
                <= set(browser["observed_states"]),
                "P0",
            ),
            _assertion(
                "runtime_probe_methods",
                self.contract.runtime_probe_contract["required_methods"],
                routes["/"]["methods"],
                all(routes["/"]["methods"].values()),
                "P0",
            ),
            _assertion(
                "runtime_probe_controls",
                sorted(expected_controls),
                sorted(expected_controls & observed_controls),
                expected_controls <= observed_controls,
                "P0",
            ),
            _assertion(
                "responsive_route_structure",
                "every route has one main, one h1, and no horizontal overflow",
                routes,
                route_passed and not browser["mobile_horizontal_overflow"],
            ),
            _assertion(
                "poster_first_lazy_glb",
                {
                    "poster_before_3d": True,
                    "glb_before_intent": False,
                    "ready_after_intent": True,
                },
                {
                    "poster_before_3d": browser["poster_before_3d"],
                    "ready_state": browser["ready_state"],
                    "initial_glb_paths": [
                        path
                        for path in browser["initial_request_paths"]
                        if path.endswith(".glb")
                    ],
                },
                browser["poster_before_3d"]
                == {"poster": True, "glb": False}
                and not any(
                    path.endswith(".glb")
                    for path in browser["initial_request_paths"]
                )
                and browser["ready_state"]["state"] == "3d_ready"
                and browser["ready_state"]["glbRequested"],
                "P0",
            ),
            _assertion(
                "configuration_3d_state_persistence",
                "configuration equals scene state before and after reload",
                {
                    "configured": browser["configuration"],
                    "restored": browser["restored_configuration"],
                },
                configuration_passed,
                "P0",
            ),
            _assertion(
                "semantic_part_selection",
                "glass_core",
                browser["selected_part"],
                browser["selected_part"] == "glass_core",
            ),
            _assertion(
                "keyboard_journey",
                "focus reaches enter-3d and navigation",
                browser["keyboard_targets"],
                "enter-3d" in browser["keyboard_targets"]
                and len(set(browser["keyboard_targets"])) >= 3,
                "P0",
            ),
            _assertion(
                "reduced_motion",
                {"reduced": True, "animation": False},
                browser["reduced_motion"],
                browser["reduced_motion"]["reduced"] is True
                and browser["reduced_motion"]["animation"] is False,
                "P0",
            ),
            _assertion(
                "no_webgl_non_3d_journeys",
                "fallback state and every route remain usable",
                {
                    "state": browser["fallback_state"],
                    "routes": browser["fallback_routes"],
                },
                no_webgl_passed,
                "P0",
            ),
            _assertion(
                "hidden_mobile_trace",
                len(browser["hidden_mobile_trace"]["completed_steps"]),
                browser["hidden_mobile_trace"],
                browser["hidden_mobile_trace"]["step_count"] > 0
                and browser["hidden_mobile_trace"]["final_route"] == "/receipt",
                "P0",
            ),
            _assertion(
                "critical_transfer_budget",
                budget["critical_html_css_transfer_bytes_maximum"],
                performance["critical_html_css_transfer_bytes"],
                performance["critical_html_css_transfer_bytes"]
                <= int(budget["critical_html_css_transfer_bytes_maximum"]),
            ),
            _assertion(
                "javascript_transfer_budget",
                budget["initial_javascript_compressed_bytes_maximum"],
                performance["initial_javascript_transfer_bytes"],
                performance["initial_javascript_transfer_bytes"]
                <= int(budget["initial_javascript_compressed_bytes_maximum"]),
            ),
            _assertion(
                "glb_size_budgets",
                {
                    "hero_maximum": budget["hero_glb_bytes_maximum"],
                    "mobile_lod_maximum": budget["mobile_lod_glb_bytes_maximum"],
                },
                {
                    "hero": performance["hero_glb_bytes"],
                    "mobile_lod": performance["mobile_lod_glb_bytes"],
                },
                performance["hero_glb_bytes"]
                <= int(budget["hero_glb_bytes_maximum"])
                and performance["mobile_lod_glb_bytes"]
                <= int(budget["mobile_lod_glb_bytes_maximum"]),
                "P0",
            ),
            _assertion(
                "layout_shift_budget",
                budget["cumulative_layout_shift_maximum"],
                performance["cumulative_layout_shift"],
                performance["cumulative_layout_shift"]
                <= float(budget["cumulative_layout_shift_maximum"]),
            ),
            _assertion(
                "desktop_3d_frames",
                {
                    "median_fps_minimum": budget["desktop_median_fps_minimum"],
                    "p95_ms_maximum": budget["desktop_frame_p95_ms_maximum"],
                },
                {
                    "median_fps": performance["desktop_median_fps"],
                    "p95_ms": performance["desktop_frame_p95_ms"],
                },
                performance["desktop_median_fps"]
                >= float(budget["desktop_median_fps_minimum"])
                and performance["desktop_frame_p95_ms"]
                <= float(budget["desktop_frame_p95_ms_maximum"]),
            ),
            _assertion(
                "mobile_3d_frames",
                {
                    "median_fps_minimum": budget["mobile_median_fps_minimum"],
                    "p95_ms_maximum": budget["mobile_frame_p95_ms_maximum"],
                },
                {
                    "median_fps": performance["mobile_median_fps"],
                    "p95_ms": performance["mobile_frame_p95_ms"],
                },
                performance["mobile_median_fps"]
                >= float(budget["mobile_median_fps_minimum"])
                and performance["mobile_frame_p95_ms"]
                <= float(budget["mobile_frame_p95_ms_maximum"]),
            ),
            _assertion(
                "bounded_memory_growth",
                {
                    "duration_seconds": budget["interaction_loop_seconds"],
                    "growth_bytes_maximum": budget["memory_growth_bytes_maximum"],
                },
                {
                    "duration_seconds": performance["memory_duration_seconds"],
                    "growth_bytes": performance["memory_growth_bytes"],
                    "sample_count": len(performance["memory_samples_bytes"]),
                },
                performance["memory_duration_seconds"]
                == int(budget["interaction_loop_seconds"])
                and performance["memory_growth_bytes"]
                <= int(budget["memory_growth_bytes_maximum"]),
            ),
            _assertion(
                "automated_accessibility",
                {"critical": 0, "serious": 0},
                {
                    "critical": accessibility["critical_violation_count"],
                    "serious": accessibility["serious_violation_count"],
                    "scanner": accessibility["scanner"],
                },
                accessibility["critical_violation_count"] == 0
                and accessibility["serious_violation_count"] == 0,
                "P0",
            ),
        ]
