from __future__ import annotations

import hashlib
import json
import os
import shlex
import socket
import subprocess
import time
import urllib.request
from datetime import UTC, datetime
from importlib import resources
from pathlib import Path
from typing import Literal

from pydantic import Field, model_validator

from blender_vision.app_build.compiler import BoundedApplicationCompiler
from blender_vision.app_build.loader import ReferencePacketLoader
from blender_vision.app_build.specification import StrictModel
from blender_vision.core.util import atomic_write_json, code_revision, sha256_file


class ApplicationBenchmarkError(ValueError):
    pass


class ApplicationBenchmarkCase(StrictModel):
    id: str
    packet: str
    packet_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    required_handler_kinds: list[str]
    required_runtime_gates: list[str]


class ApplicationBenchmarkManifest(StrictModel):
    schema_version: Literal["1"] = "1"
    corpus_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    cases: list[ApplicationBenchmarkCase]

    @model_validator(mode="after")
    def unique_cases(self) -> ApplicationBenchmarkManifest:
        identifiers = [case.id for case in self.cases]
        if len(set(identifiers)) != len(identifiers):
            raise ValueError("application benchmark case IDs must be unique")
        return self


class ApplicationBenchmarkGateResult(StrictModel):
    id: str
    status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"]
    command: list[str]
    elapsed_seconds: float = Field(ge=0)
    return_code: int | None
    log_path: str
    log_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    exact_resumption_contract: str | None = None


class ApplicationBenchmarkCaseResult(StrictModel):
    id: str
    packet_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    candidate_receipt_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    functional_passed: bool
    complete: bool
    gates: list[ApplicationBenchmarkGateResult]


class ApplicationBenchmarkReceipt(StrictModel):
    schema_version: Literal["1"] = "1"
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    corpus_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    node_version: str
    npm_version: str
    functional_passed: bool
    complete: bool
    cases: list[ApplicationBenchmarkCaseResult]
    external_gates: list[ApplicationBenchmarkGateResult]
    external_boundaries: dict[str, str]


def _benchmark_root() -> Path:
    development = Path(__file__).resolve().parents[3] / "benchmarks" / "100_plus" / "app_build"
    if development.is_dir():
        return development
    installed = resources.files("blender_vision").joinpath(
        "benchmarks", "data", "100_plus", "app_build"
    )
    return Path(str(installed))


def load_application_benchmark_manifest(
    path: Path | None = None,
) -> tuple[ApplicationBenchmarkManifest, Path]:
    manifest_path = (path or (_benchmark_root() / "manifest.json")).expanduser().resolve()
    if not manifest_path.is_file():
        raise ApplicationBenchmarkError(
            f"application benchmark manifest is missing: {manifest_path}"
        )
    manifest = ApplicationBenchmarkManifest.model_validate_json(
        manifest_path.read_text(encoding="utf-8")
    )
    observed_corpus = hashlib.sha256(
        "\n".join(case.packet_sha256 for case in manifest.cases).encode()
    ).hexdigest()
    if observed_corpus != manifest.corpus_sha256:
        raise ApplicationBenchmarkError(
            "application benchmark corpus digest mismatch: "
            f"expected {manifest.corpus_sha256}, observed {observed_corpus}"
        )
    root = manifest_path.parent
    for case in manifest.cases:
        relative = Path(case.packet)
        if relative.is_absolute() or ".." in relative.parts:
            raise ApplicationBenchmarkError(f"benchmark packet escaped its root: {case.packet}")
        packet_path = root / relative
        loaded = ReferencePacketLoader().load(packet_path)
        if not loaded.packet_path.is_relative_to(root):
            raise ApplicationBenchmarkError(f"benchmark packet escaped its root: {case.packet}")
        observed = loaded.packet.canonical_digest()
        if observed != case.packet_sha256:
            raise ApplicationBenchmarkError(
                f"benchmark packet {case.id} digest mismatch: "
                f"expected {case.packet_sha256}, observed {observed}"
            )
        handlers = sorted(
            {endpoint.handler.kind for endpoint in loaded.packet.api_contract.endpoints}
        )
        if handlers != case.required_handler_kinds:
            raise ApplicationBenchmarkError(f"benchmark packet {case.id} handler registry mismatch")
    return manifest, manifest_path


def _version(executable: str) -> str:
    process = subprocess.run(
        [executable, "--version"],
        check=False,
        capture_output=True,
        text=True,
        timeout=10,
    )
    if process.returncode != 0:
        raise ApplicationBenchmarkError(f"{executable} is unavailable: {process.stderr.strip()}")
    return process.stdout.strip()


def _source_head() -> str:
    head = code_revision(Path(__file__).resolve().parents[3])
    if len(head) != 40 or any(character not in "0123456789abcdef" for character in head):
        raise ApplicationBenchmarkError("application benchmark requires a full Git source revision")
    return head


class ApplicationBenchmarkRunner:
    FUNCTIONAL_GATES = {
        "compile",
        "npm_ci",
        "typescript_and_api_tests",
        "migration",
        "repeat_migration",
        "restart_health",
        "rollback",
        "fresh_clone_install",
        "fresh_clone_reproduction",
    }

    def __init__(self, manifest_path: Path | None = None):
        self.manifest, self.manifest_path = load_application_benchmark_manifest(manifest_path)
        self.root = self.manifest_path.parent

    def _write_gate_log(
        self,
        output_root: Path,
        case_id: str,
        gate_id: str,
        text: str,
    ) -> tuple[str, str]:
        path = output_root / "logs" / case_id / f"{gate_id}.log"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
        digest, _size = sha256_file(path)
        return path.relative_to(output_root).as_posix(), digest

    def _command_gate(
        self,
        output_root: Path,
        case_id: str,
        gate_id: str,
        command: list[str],
        cwd: Path,
        *,
        timeout: int = 300,
    ) -> ApplicationBenchmarkGateResult:
        started = time.monotonic()
        environment = os.environ.copy()
        environment.update(
            {
                "CI": "1",
                "NPM_CONFIG_FUND": "false",
                "NPM_CONFIG_AUDIT": "false",
                "NPM_CONFIG_UPDATE_NOTIFIER": "false",
            }
        )
        try:
            process = subprocess.run(
                command,
                cwd=cwd,
                env=environment,
                check=False,
                capture_output=True,
                text=True,
                timeout=timeout,
            )
            output = (
                f"$ {shlex.join(command)}\n"
                f"cwd={cwd}\n"
                f"return_code={process.returncode}\n\n"
                f"STDOUT\n{process.stdout}\nSTDERR\n{process.stderr}"
            )
            status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"] = (
                "PASS" if process.returncode == 0 else "FAIL"
            )
            return_code = process.returncode
        except (OSError, subprocess.TimeoutExpired) as error:
            output = f"$ {shlex.join(command)}\ncwd={cwd}\nERROR\n{error}\n"
            status = "FAIL"
            return_code = None
        elapsed = time.monotonic() - started
        log_path, digest = self._write_gate_log(output_root, case_id, gate_id, output)
        return ApplicationBenchmarkGateResult(
            id=gate_id,
            status=status,
            command=command,
            elapsed_seconds=elapsed,
            return_code=return_code,
            log_path=log_path,
            log_sha256=digest,
        )

    def _blocked_gate(
        self,
        output_root: Path,
        case_id: str,
        gate_id: str,
        command: list[str],
        contract: str,
    ) -> ApplicationBenchmarkGateResult:
        log_path, digest = self._write_gate_log(
            output_root,
            case_id,
            gate_id,
            f"BLOCKED_EXTERNAL\n{contract}\n",
        )
        return ApplicationBenchmarkGateResult(
            id=gate_id,
            status="BLOCKED_EXTERNAL",
            command=command,
            elapsed_seconds=0,
            return_code=None,
            log_path=log_path,
            log_sha256=digest,
            exact_resumption_contract=contract,
        )

    def _restart_gate(
        self,
        output_root: Path,
        case_id: str,
        candidate: Path,
    ) -> ApplicationBenchmarkGateResult:
        started = time.monotonic()
        transcript: list[str] = []
        status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"] = "PASS"
        return_code: int | None = 0
        for iteration in range(2):
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            environment = os.environ.copy()
            environment["PORT"] = str(port)
            process = subprocess.Popen(
                ["node", "dist/src/server.js"],
                cwd=candidate,
                env=environment,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
            )
            healthy = False
            try:
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        break
                    try:
                        with urllib.request.urlopen(
                            f"http://127.0.0.1:{port}/healthz",
                            timeout=1,
                        ) as response:
                            healthy = response.status == 200 and json.loads(response.read()) == {
                                "ok": True
                            }
                        if healthy:
                            break
                    except (OSError, ValueError):
                        time.sleep(0.1)
                transcript.append(f"restart={iteration + 1} port={port} healthy={healthy}")
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
                if process.stdout:
                    transcript.append(process.stdout.read())
            if not healthy or process.returncode not in {0, -15}:
                status = "FAIL"
                return_code = process.returncode
                break
        log_path, digest = self._write_gate_log(
            output_root,
            case_id,
            "restart_health",
            "\n".join(transcript) + "\n",
        )
        return ApplicationBenchmarkGateResult(
            id="restart_health",
            status=status,
            command=["node", "dist/src/server.js", "# twice"],
            elapsed_seconds=time.monotonic() - started,
            return_code=return_code,
            log_path=log_path,
            log_sha256=digest,
        )

    def _docker_available(self) -> tuple[bool, str]:
        try:
            process = subprocess.run(
                ["docker", "info"],
                check=False,
                capture_output=True,
                text=True,
                timeout=20,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            return False, str(error)
        return process.returncode == 0, (process.stderr or process.stdout).strip()

    def run(
        self,
        output_root: Path,
        *,
        case_ids: set[str] | None = None,
    ) -> ApplicationBenchmarkReceipt:
        destination = output_root.expanduser().absolute()
        if destination.exists() and any(destination.iterdir()):
            raise ApplicationBenchmarkError(
                f"benchmark output must be absent or empty: {destination}"
            )
        destination.mkdir(parents=True, exist_ok=True)
        started_at = datetime.now(UTC).isoformat()
        node_version = _version("node")
        npm_version = _version("npm")
        selected = [case for case in self.manifest.cases if case_ids is None or case.id in case_ids]
        if case_ids and {case.id for case in selected} != case_ids:
            missing = sorted(case_ids - {case.id for case in selected})
            raise ApplicationBenchmarkError(f"unknown application benchmark cases: {missing}")
        docker_available, docker_detail = self._docker_available()
        results: list[ApplicationBenchmarkCaseResult] = []
        compiler = BoundedApplicationCompiler(destination / "candidates")
        for case in selected:
            loaded = ReferencePacketLoader().load(self.root / case.packet)
            compile_started = time.monotonic()
            compiler.compile(
                loaded.packet,
                candidate_id=case.id,
                mode="promotion_candidate",
                verified_source_ids=loaded.verified_source_ids,
            )
            candidate = destination / "candidates" / case.id
            clone = destination / "fresh-clones" / case.id
            compiler.copy_candidate(candidate, clone)
            receipt_path = candidate / ".visionmcp" / "candidate-receipt.json"
            receipt_digest, _size = sha256_file(receipt_path)
            compile_log, compile_digest = self._write_gate_log(
                destination,
                case.id,
                "compile",
                receipt_path.read_text(encoding="utf-8"),
            )
            gates = [
                ApplicationBenchmarkGateResult(
                    id="compile",
                    status="PASS",
                    command=["bvmcp", "app", "compile", case.packet],
                    elapsed_seconds=time.monotonic() - compile_started,
                    return_code=0,
                    log_path=compile_log,
                    log_sha256=compile_digest,
                ),
                self._command_gate(destination, case.id, "npm_ci", ["npm", "ci"], candidate),
            ]
            if gates[-1].status == "PASS":
                gates.append(
                    self._command_gate(
                        destination,
                        case.id,
                        "typescript_and_api_tests",
                        ["npm", "run", "verify"],
                        candidate,
                    )
                )
                gates.append(
                    self._command_gate(
                        destination,
                        case.id,
                        "migration",
                        ["npm", "run", "db:migrate"],
                        candidate,
                    )
                )
                if gates[-1].status == "PASS":
                    gates.append(
                        self._command_gate(
                            destination,
                            case.id,
                            "repeat_migration",
                            ["npm", "run", "db:migrate"],
                            candidate,
                        )
                    )
                if gates[-1].status == "PASS":
                    gates.append(self._restart_gate(destination, case.id, candidate))
                gates.append(
                    self._command_gate(
                        destination,
                        case.id,
                        "rollback",
                        ["npm", "run", "db:rollback"],
                        candidate,
                    )
                )
            gates.append(
                self._command_gate(
                    destination,
                    case.id,
                    "fresh_clone_install",
                    ["npm", "ci"],
                    clone,
                    timeout=420,
                )
            )
            if gates[-1].status == "PASS":
                gates.append(
                    self._command_gate(
                        destination,
                        case.id,
                        "fresh_clone_reproduction",
                        ["npm", "run", "verify"],
                        clone,
                        timeout=420,
                    )
                )
            if docker_available:
                gates.append(
                    self._command_gate(
                        destination,
                        case.id,
                        "local_container_build",
                        ["docker", "compose", "build"],
                        candidate,
                        timeout=600,
                    )
                )
            else:
                gates.append(
                    self._blocked_gate(
                        destination,
                        case.id,
                        "local_container_build",
                        ["docker", "compose", "build"],
                        "Start or provide an authorized Docker daemon, then rerun this exact "
                        f"benchmark. Preflight detail: {docker_detail}",
                    )
                )
            gate_by_id = {gate.id: gate for gate in gates}
            functional_passed = all(
                gate_by_id.get(gate_id) is not None and gate_by_id[gate_id].status == "PASS"
                for gate_id in self.FUNCTIONAL_GATES
            )
            complete = functional_passed and all(gate.status == "PASS" for gate in gates)
            results.append(
                ApplicationBenchmarkCaseResult(
                    id=case.id,
                    packet_sha256=case.packet_sha256,
                    candidate_receipt_sha256=receipt_digest,
                    functional_passed=functional_passed,
                    complete=complete,
                    gates=gates,
                )
            )
        remote_contract = (
            "Set BVMCP_AUTHORIZED_REMOTE_DEPLOY_JSON to a JSON argv array only after the user "
            "authorizes the target and credentials; no remote deployment was attempted."
        )
        remote_json = os.environ.get("BVMCP_AUTHORIZED_REMOTE_DEPLOY_JSON")
        external_gates: list[ApplicationBenchmarkGateResult]
        if remote_json:
            try:
                remote_command = json.loads(remote_json)
            except json.JSONDecodeError as error:
                raise ApplicationBenchmarkError(
                    "BVMCP_AUTHORIZED_REMOTE_DEPLOY_JSON is invalid JSON"
                ) from error
            if (
                not isinstance(remote_command, list)
                or not remote_command
                or not all(isinstance(item, str) and item for item in remote_command)
            ):
                raise ApplicationBenchmarkError(
                    "BVMCP_AUTHORIZED_REMOTE_DEPLOY_JSON must be a non-empty JSON argv array"
                )
            candidate_token = str(destination / "candidates" / selected[0].id) if selected else ""
            remote_command = [
                item.replace("{candidate}", candidate_token) for item in remote_command
            ]
            external_gates = [
                self._command_gate(
                    destination,
                    "_external",
                    "authorized_remote_deployment",
                    remote_command,
                    destination,
                    timeout=900,
                )
            ]
        else:
            external_gates = [
                self._blocked_gate(
                    destination,
                    "_external",
                    "authorized_remote_deployment",
                    [],
                    remote_contract,
                )
            ]
        external_boundaries = {
            "local_container": (
                "available" if docker_available else f"BLOCKED_EXTERNAL: {docker_detail}"
            ),
            "remote_deployment": (
                external_gates[0].status if remote_json else f"BLOCKED_EXTERNAL: {remote_contract}"
            ),
        }
        manifest_digest, _size = sha256_file(self.manifest_path)
        receipt = ApplicationBenchmarkReceipt(
            source_git_head=_source_head(),
            manifest_sha256=manifest_digest,
            corpus_sha256=self.manifest.corpus_sha256,
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            node_version=node_version,
            npm_version=npm_version,
            functional_passed=bool(results) and all(result.functional_passed for result in results),
            complete=bool(results)
            and all(result.complete for result in results)
            and (not remote_json or external_gates[0].status == "PASS"),
            cases=results,
            external_gates=external_gates,
            external_boundaries=external_boundaries,
        )
        atomic_write_json(
            destination / "application-benchmark-receipt.json",
            receipt.model_dump(mode="json"),
        )
        return receipt
