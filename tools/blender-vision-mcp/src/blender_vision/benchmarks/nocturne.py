from __future__ import annotations

import hashlib
import os
import platform
import re
import subprocess
import time
from datetime import UTC, datetime
from importlib import resources
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.core.errors import SecurityError
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file
from blender_vision.security.adversarial import SealedBenchmarkBoundary

_PUBLIC_VIEWS = ("front", "rear", "left", "right", "top", "hero")
_ROUTES = ("/", "/technology", "/configurator", "/reserve", "/receipt")
_STATES = (
    "initial_loading",
    "poster_fallback",
    "3d_ready",
    "3d_unavailable",
    "reduced_motion",
    "keyboard_navigation",
    "touch_interaction",
    "slow_network",
    "offline_retry",
    "api_validation_error",
    "api_transient_error",
    "successful_reservation",
    "empty_configuration",
    "restored_saved_configuration",
)
_SHA256 = re.compile(r"^[0-9a-f]{64}$")


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class NocturneOneContract(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-sealed-v1"]
    product_id: Literal["nocturne-one"]
    product_name: Literal["NOCTURNE/ONE"]
    oracle_seed: int
    public_view_labels: list[str]
    hidden_holdout_count: int = Field(ge=4)
    hidden_mobile_trace_count: int = Field(ge=1)
    required_packet_files: list[str]
    required_parts: list[str]
    geometry_gates: dict[str, Any]
    application_routes: list[str]
    application_states: list[str]
    required_journeys: list[str]
    runtime_probe_contract: dict[str, Any]
    performance_budget: dict[str, Any]
    accessibility_gates: dict[str, Any]
    anti_cheat_rules: list[str]
    claim_boundary: list[str]

    @model_validator(mode="after")
    def fixed_public_contract(self) -> NocturneOneContract:
        if tuple(self.public_view_labels) != _PUBLIC_VIEWS:
            raise ValueError("NOCTURNE/ONE requires the fixed six public views")
        if tuple(self.application_routes) != _ROUTES:
            raise ValueError("NOCTURNE/ONE requires the fixed five routes")
        if tuple(self.application_states) != _STATES:
            raise ValueError("NOCTURNE/ONE requires the fixed application state corpus")
        for values, label in (
            (self.required_packet_files, "packet files"),
            (self.required_parts, "parts"),
            (self.required_journeys, "journeys"),
            (self.anti_cheat_rules, "anti-cheat rules"),
        ):
            if not values or len(values) != len(set(values)):
                raise ValueError(f"NOCTURNE/ONE {label} must be non-empty and unique")
        for relative in self.required_packet_files:
            candidate = Path(relative)
            if candidate.is_absolute() or ".." in candidate.parts:
                raise ValueError("NOCTURNE/ONE packet path escaped its root")
        return self


class PacketArtifact(_StrictModel):
    path: str
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    size: int = Field(ge=0)
    media_type: str


class NocturnePacketManifest(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-sealed-v1"]
    authority: Literal["GOVERNED_BUILDER_INPUT"]
    oracle_seed: int
    contract_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    governed_spec_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    generated_at: str
    artifacts: list[PacketArtifact]
    excluded_authority: list[str]
    rights_state: Literal["SYNTHETIC_OWNED"]

    @model_validator(mode="after")
    def unique_paths(self) -> NocturnePacketManifest:
        paths = [item.path for item in self.artifacts]
        if len(paths) != len(set(paths)):
            raise ValueError("NOCTURNE/ONE packet artifact paths must be unique")
        return self


class SealedBuilderReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: Literal["nocturne-one-sealed-v1"]
    contract_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    packet_manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    builder_root: str
    builder_root_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    oracle_root_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    profile_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    command: list[str]
    process_id: int = Field(gt=0)
    started_at: str
    completed_at: str
    elapsed_seconds: float = Field(ge=0)
    exit_code: int
    preflight_denied: bool
    oracle_canary_absent_from_builder: bool
    stdout_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    stderr_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    status: Literal["PASS", "FAIL"]
    host: dict[str, Any]
    claim_boundary: list[str]


class NocturneBenchmarkError(ValueError):
    pass


def _benchmark_root() -> Path:
    development = Path(__file__).resolve().parents[3] / "benchmarks" / "nocturne_one"
    if development.is_dir():
        return development
    installed = resources.files("blender_vision").joinpath(
        "benchmarks", "data", "nocturne_one"
    )
    return Path(str(installed))


def nocturne_benchmark_root() -> Path:
    return _benchmark_root()


def load_nocturne_contract(
    path: Path | None = None,
) -> tuple[NocturneOneContract, Path]:
    contract_path = (path or (_benchmark_root() / "contract.json")).expanduser().absolute()
    if contract_path.is_symlink() or not contract_path.is_file():
        raise NocturneBenchmarkError(
            f"NOCTURNE/ONE contract is missing or linked: {contract_path}"
        )
    return (
        NocturneOneContract.model_validate_json(
            contract_path.read_text(encoding="utf-8")
        ),
        contract_path.resolve(),
    )


def _tree_digest(root: Path) -> str:
    entries: list[dict[str, Any]] = []
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            raise SecurityError(f"sealed tree contains a symlink: {path}")
        if path.is_file():
            entries.append(
                {
                    "path": path.relative_to(root).as_posix(),
                    "sha256": sha256_file(path)[0],
                    "size": path.stat().st_size,
                }
            )
    return hashlib.sha256(canonical_json(entries)).hexdigest()


class NocturnePacketAuthority:
    def __init__(self, contract_path: Path | None = None):
        self.contract, self.contract_path = load_nocturne_contract(contract_path)

    def verify(self, packet_root: Path) -> dict[str, Any]:
        supplied = packet_root.expanduser().absolute()
        if supplied.is_symlink() or not supplied.is_dir():
            raise SecurityError("NOCTURNE/ONE packet root must be a regular directory")
        root = supplied.resolve()
        manifest_path = root / "packet.manifest.json"
        if manifest_path.is_symlink() or not manifest_path.is_file():
            raise SecurityError("NOCTURNE/ONE packet manifest is missing or linked")
        manifest = NocturnePacketManifest.model_validate_json(
            manifest_path.read_text(encoding="utf-8")
        )
        if manifest.oracle_seed != self.contract.oracle_seed:
            raise SecurityError("NOCTURNE/ONE packet seed does not match the contract")
        if manifest.contract_sha256 != sha256_file(self.contract_path)[0]:
            raise SecurityError("NOCTURNE/ONE packet contract digest is stale")
        governed_spec = _benchmark_root() / "governed_spec.json"
        if manifest.governed_spec_sha256 != sha256_file(governed_spec)[0]:
            raise SecurityError("NOCTURNE/ONE governed specification digest is stale")
        records = {item.path: item for item in manifest.artifacts}
        required = set(self.contract.required_packet_files) - {"packet.manifest.json"}
        if set(records) != required:
            raise SecurityError("NOCTURNE/ONE packet artifact set is not exact")
        observed_files = {
            path.relative_to(root).as_posix()
            for path in root.rglob("*")
            if path.is_file()
        }
        if observed_files != set(self.contract.required_packet_files):
            raise SecurityError("NOCTURNE/ONE packet contains undeclared files")
        verified: list[str] = []
        for relative, record in records.items():
            candidate = Path(relative)
            if candidate.is_absolute() or ".." in candidate.parts:
                raise SecurityError("NOCTURNE/ONE packet artifact path escaped")
            path = root / candidate
            if path.is_symlink() or not path.is_file():
                raise SecurityError(f"NOCTURNE/ONE packet artifact is missing: {relative}")
            digest, size = sha256_file(path)
            if digest != record.sha256 or size != record.size:
                raise SecurityError(
                    f"NOCTURNE/ONE packet artifact was substituted: {relative}"
                )
            verified.append(relative)
        return {
            "valid": True,
            "benchmark_id": self.contract.benchmark_id,
            "packet_manifest_sha256": sha256_file(manifest_path)[0],
            "packet_tree_sha256": _tree_digest(root),
            "verified_artifact_count": len(verified),
            "verified_artifacts": sorted(verified),
            "excluded_authority": manifest.excluded_authority,
        }


def _sandbox_profile(denied_roots: list[Path]) -> str:
    clauses = [
        "(version 1)",
        "(allow default)",
    ]
    for root in denied_roots:
        escaped = str(root.expanduser().resolve()).replace("\\", "\\\\").replace('"', '\\"')
        clauses.append(f'(deny file-read* (subpath "{escaped}"))')
        clauses.append(f'(deny file-write* (subpath "{escaped}"))')
    return "\n".join(clauses) + "\n"


class SealedBuilderRunner:
    """Run a builder under an OS policy that denies oracle and holdout reads."""

    def __init__(self, contract_path: Path | None = None):
        self.contract, self.contract_path = load_nocturne_contract(contract_path)

    def run(
        self,
        *,
        builder_root: Path,
        packet_root: Path,
        oracle_root: Path,
        oracle_source_root: Path,
        oracle_canary: str,
        command: list[str],
        output_root: Path,
        timeout_seconds: int = 3600,
    ) -> SealedBuilderReceipt:
        if not command:
            raise ValueError("sealed builder command cannot be empty")
        builder = builder_root.expanduser().resolve()
        packet = packet_root.expanduser().resolve()
        oracle = oracle_root.expanduser().resolve()
        oracle_source = oracle_source_root.expanduser().resolve()
        output = output_root.expanduser().resolve()
        if output.exists() and any(output.iterdir()):
            raise NocturneBenchmarkError("sealed builder output must be new or empty")
        output.mkdir(parents=True, exist_ok=True)
        if not builder.is_dir() or not packet.is_dir() or not oracle.is_dir():
            raise NocturneBenchmarkError("sealed builder roots must exist")
        if not oracle_source.is_dir():
            raise NocturneBenchmarkError("oracle source root must exist")
        packet_receipt = NocturnePacketAuthority(self.contract_path).verify(packet)
        profile_text = _sandbox_profile([oracle, oracle_source])
        profile_path = output / "builder.sb"
        profile_path.write_text(profile_text, encoding="utf-8")
        sandbox = Path("/usr/bin/sandbox-exec")
        if not sandbox.is_file():
            raise NocturneBenchmarkError("macOS sandbox-exec is required for this run")
        canary_path = oracle / "ORACLE_CANARY.txt"
        if not canary_path.is_file() or canary_path.read_text(encoding="utf-8") != oracle_canary:
            raise SecurityError("sealed oracle canary is missing or stale")
        preflight = subprocess.run(
            [str(sandbox), "-f", str(profile_path), "/bin/cat", str(canary_path)],
            cwd=builder,
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        preflight_denied = preflight.returncode != 0 and oracle_canary not in (
            preflight.stdout + preflight.stderr
        )
        if not preflight_denied:
            raise SecurityError("builder sandbox could read the sealed oracle canary")
        stdout_path = output / "builder.stdout.log"
        stderr_path = output / "builder.stderr.log"
        started_at = datetime.now(UTC).isoformat()
        started = time.monotonic()
        process = subprocess.Popen(
            [str(sandbox), "-f", str(profile_path), *command],
            cwd=builder,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env={
                **os.environ,
                "NOCTURNE_PACKET_ROOT": str(packet),
                "NOCTURNE_CONTRACT_SHA256": sha256_file(self.contract_path)[0],
                "NOCTURNE_BUILDER_ROOT": str(builder),
            },
        )
        try:
            stdout, stderr = process.communicate(timeout=timeout_seconds)
        except subprocess.TimeoutExpired:
            process.terminate()
            try:
                stdout, stderr = process.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                stdout, stderr = process.communicate()
            stderr = f"{stderr}\nSEALED_BUILDER_TIMEOUT after {timeout_seconds}s\n"
        stdout_path.write_text(stdout, encoding="utf-8")
        stderr_path.write_text(stderr, encoding="utf-8")
        separation = SealedBenchmarkBoundary.verify(
            builder,
            oracle,
            canaries=[oracle_canary],
            maximum_scan_bytes=512 * 1024 * 1024,
        )
        canary_absent = separation["leakage_detected"] is False
        passed = process.returncode == 0 and preflight_denied and canary_absent
        receipt = SealedBuilderReceipt(
            benchmark_id=self.contract.benchmark_id,
            contract_sha256=sha256_file(self.contract_path)[0],
            packet_manifest_sha256=packet_receipt["packet_manifest_sha256"],
            builder_root=str(builder),
            builder_root_sha256=_tree_digest(builder),
            oracle_root_sha256=_tree_digest(oracle),
            profile_sha256=sha256_file(profile_path)[0],
            command=command,
            process_id=process.pid,
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            elapsed_seconds=time.monotonic() - started,
            exit_code=int(process.returncode or 0),
            preflight_denied=preflight_denied,
            oracle_canary_absent_from_builder=canary_absent,
            stdout_sha256=sha256_file(stdout_path)[0],
            stderr_sha256=sha256_file(stderr_path)[0],
            status="PASS" if passed else "FAIL",
            host={
                "platform": platform.platform(),
                "sandbox": str(sandbox),
            },
            claim_boundary=[
                "The OS sandbox denied reads and writes under the sealed oracle and "
                "oracle-source roots.",
                "The builder tree was scanned for an evaluator-only canary after execution.",
                "This proves filesystem separation for the recorded process, not cognitive "
                "independence of the benchmark authors.",
            ],
        )
        atomic_write_json(
            output / "sealed-builder.receipt.json",
            receipt.model_dump(mode="json"),
        )
        return receipt


def canonical_payload_digest(value: Any) -> str:
    return hashlib.sha256(canonical_json(value)).hexdigest()


def validate_digest(value: str) -> str:
    if not _SHA256.fullmatch(value):
        raise ValueError("value is not a lowercase SHA-256 digest")
    return value
