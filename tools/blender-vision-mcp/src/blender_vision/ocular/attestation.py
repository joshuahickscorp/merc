"""Physical-run authority (Ocular Bible section 22).

The permanent V2.1 law: a fallback may produce a diagnostic, a candidate, or a
BLOCKED status. It may never produce a physical-runtime PASS.

The V2 run violated this in a way worth remembering. A relative `--python` path
made Blender exit 1; the fallback caught it, ran an offline mesh path, and
published the failure as "Metal SIGSEGV during WM_init". Metal was fine. A path
bug was reported as a hardware fault, and the offline geometry stood in for a
physical run. Both halves of that are prohibited here: `RuntimeAttestation`
records what actually executed, and `ExecutionClass` makes the distinction
between real and substituted impossible to blur.
"""

from __future__ import annotations

import os
import platform
import shutil
import subprocess
import time
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any, ClassVar

from blender_vision.core.errors import SecurityError, ValidationError
from blender_vision.core.util import sha256_file, utc_now
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import V2Record


class ExecutionClass(StrEnum):
    """What actually ran. Only PHYSICAL may carry runtime authority."""

    #: A real external runtime executed and exited on its own.
    PHYSICAL = "PHYSICAL"
    #: A substitute produced an artifact. Useful, never authoritative.
    DIAGNOSTIC_ONLY = "DIAGNOSTIC_ONLY"
    #: A substitute produced something that may become a candidate after review.
    CANDIDATE_ONLY = "CANDIDATE_ONLY"
    #: The runtime is unavailable. This is a result, not a failure to hide.
    BLOCKED = "BLOCKED"


#: Verdicts that assert real hardware ran. Reserved to ExecutionClass.PHYSICAL.
PHYSICAL_VERDICTS = frozenset({"PHYSICAL_PASS", "HARDWARE_VERIFIED", "REAL_RUNTIME_VERIFIED"})


class FalsePhysicalAuthority(ValidationError):
    """Raised when a non-physical execution tries to claim a physical verdict."""


class FailureKind(StrEnum):
    """Why a runtime exited non-zero. Never guess between these."""

    SCRIPT_ERROR = "SCRIPT_ERROR"
    PATH_ERROR = "PATH_ERROR"
    DEPENDENCY_ERROR = "DEPENDENCY_ERROR"
    API_ERROR = "API_ERROR"
    HARDWARE_ERROR = "HARDWARE_ERROR"
    TIMEOUT = "TIMEOUT"
    NOT_INSTALLED = "NOT_INSTALLED"
    UNCLASSIFIED = "UNCLASSIFIED"


# Ordered most-specific first. Hardware is deliberately last: the V2 run reached
# for it as the default explanation, which is how a path bug became a Metal bug.
_FAILURE_SIGNATURES: tuple[tuple[FailureKind, tuple[str, ...]], ...] = (
    (
        FailureKind.PATH_ERROR,
        (
            "no such file or directory",
            "cannot find file",
            "unable to open",
            "could not open",
            "file not found",
        ),
    ),
    (
        FailureKind.DEPENDENCY_ERROR,
        ("modulenotfounderror", "importerror", "no module named", "command not found"),
    ),
    (
        FailureKind.API_ERROR,
        ("attributeerror", "typeerror", "keyerror", "valueerror", "has no attribute"),
    ),
    (FailureKind.SCRIPT_ERROR, ("traceback (most recent call last)", "syntaxerror")),
    (
        FailureKind.HARDWARE_ERROR,
        (
            "metal_is_supported",
            "gpu device not found",
            "no cuda-capable device",
            "failed to create opengl context",
            "segmentation fault",
            "sigsegv",
        ),
    ),
)


def classify_failure(stdout: str, stderr: str, returncode: int) -> FailureKind:
    """Classify a non-zero exit from what the runtime actually said.

    A SIGSEGV alone is not hardware: a script error can segfault an interpreter.
    Script, path, dependency and API signatures are therefore matched first, and
    a bare crash with no other signal stays UNCLASSIFIED rather than becoming a
    hardware accusation.
    """
    if returncode == 0:
        raise ValidationError("classify_failure called on a successful run")
    haystack = f"{stdout}\n{stderr}".lower()
    for kind, needles in _FAILURE_SIGNATURES:
        if any(needle in haystack for needle in needles):
            return kind
    return FailureKind.UNCLASSIFIED


@dataclass(slots=True, kw_only=True)
class RuntimeAttestation(V2Record):
    """Proof that a specific external runtime executed. Bible section 22.3."""

    RECORD_KIND: ClassVar[str] = "ocular.runtime-attestation"

    runtime: str = ""
    execution_class: ExecutionClass = ExecutionClass.BLOCKED
    executable: str = ""
    executable_sha256: str = ""
    version: str = ""
    command: list[str] = field(default_factory=list)
    process_id: int = 0
    started_at: str = ""
    ended_at: str = ""
    elapsed_seconds: float = 0.0
    returncode: int | None = None
    failure_kind: FailureKind | None = None
    host: dict[str, Any] = field(default_factory=dict)
    environment: dict[str, str] = field(default_factory=dict)
    output_digests: dict[str, str] = field(default_factory=dict)
    stdout_tail: str = ""
    stderr_tail: str = ""
    blocked_reason: str = ""
    substituted_by: str = ""

    @property
    def is_physical(self) -> bool:
        return self.execution_class is ExecutionClass.PHYSICAL

    def require_physical(self, claim: str) -> None:
        """Gate a runtime claim. The only sanctioned way to assert hardware ran."""
        if not self.is_physical:
            raise FalsePhysicalAuthority(
                f"{claim!r} requires a physical run of {self.runtime or 'the runtime'}, "
                f"but execution_class is {self.execution_class.value}"
                + (f" ({self.blocked_reason})" if self.blocked_reason else "")
            )

    def verdict(self, verdict: str) -> str:
        """Return a verdict string, refusing physical verdicts for substitutes."""
        if verdict in PHYSICAL_VERDICTS:
            self.require_physical(verdict)
        return verdict

    def _enforce_authority_ceiling(self) -> None:
        # Explicit base call, not super(): slots=True dataclasses are rebuilt as
        # new classes, which invalidates the __class__ cell zero-arg super() uses.
        V2Record._enforce_authority_ceiling(self)
        # A substituted run cannot carry observed-runtime authority regardless of
        # what its lineage claims.
        if not self.is_physical and self.authority is AuthorityClass.RUNTIME_OBSERVED:
            raise FalsePhysicalAuthority(
                f"{self.execution_class.value} attestation cannot claim RUNTIME_OBSERVED"
            )


def _host() -> dict[str, Any]:
    return {
        "platform": platform.system().lower(),
        "machine": platform.machine(),
        "release": platform.release(),
        "python": platform.python_version(),
        "cpu_count": os.cpu_count(),
    }


def _digest_if_present(path: Path) -> str:
    try:
        return sha256_file(path)[0] if path.is_file() else ""
    except OSError:
        return ""


def attest_blocked(runtime: str, reason: str, *, substituted_by: str = "") -> RuntimeAttestation:
    """Record an unavailable runtime. A result in its own right, never a pass."""
    return RuntimeAttestation(
        id=f"attest-{runtime}-blocked-{int(time.time() * 1000)}",
        runtime=runtime,
        execution_class=ExecutionClass.BLOCKED,
        blocked_reason=reason,
        substituted_by=substituted_by,
        host=_host(),
        authority=AuthorityClass.UNRESOLVED,
    ).seal()


def attest_substitute(
    runtime: str,
    *,
    execution_class: ExecutionClass,
    reason: str,
    substitute: str,
    outputs: dict[str, Path] | None = None,
) -> RuntimeAttestation:
    """Record that something stood in for a runtime, and say so plainly."""
    if execution_class is ExecutionClass.PHYSICAL:
        raise FalsePhysicalAuthority("a substitute cannot be attested PHYSICAL")
    return RuntimeAttestation(
        id=f"attest-{runtime}-{substitute}-{int(time.time() * 1000)}",
        runtime=runtime,
        execution_class=execution_class,
        blocked_reason=reason,
        substituted_by=substitute,
        host=_host(),
        output_digests={
            name: _digest_if_present(path) for name, path in (outputs or {}).items()
        },
        authority=AuthorityClass.MODEL_DERIVED,
    ).seal()


def run_attested(
    runtime: str,
    command: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    timeout_seconds: int = 1800,
    outputs: dict[str, Path] | None = None,
    version_argv: list[str] | None = None,
    expect_marker: str | None = None,
) -> RuntimeAttestation:
    """Execute an external runtime and attest exactly what happened.

    A zero exit with a missing completion marker is not a pass: the process
    finished, its work did not. That is recorded as a script error rather than
    quietly accepted.
    """
    if not command:
        raise ValidationError("run_attested requires a command")
    executable = command[0]
    resolved = shutil.which(executable) or executable
    if not Path(resolved).is_file():
        return attest_blocked(runtime, f"{executable} is not installed on this host")
    if Path(resolved).is_symlink() and not Path(resolved).resolve().is_file():
        raise SecurityError(f"{executable} resolves to a broken symlink")

    version = ""
    if version_argv:
        try:
            probe = subprocess.run(  # noqa: S603 - fixed argv
                [resolved, *version_argv], capture_output=True, text=True, timeout=120
            )
            version = (probe.stdout or probe.stderr).strip().splitlines()[0][:200]
        except (subprocess.SubprocessError, OSError, IndexError):
            version = ""

    environment = os.environ.copy()
    environment.update(env or {})
    started = time.monotonic()
    started_at = utc_now()
    try:
        completed = subprocess.run(  # noqa: S603 - caller-fixed argv
            command,
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            env=environment,
            cwd=str(cwd) if cwd else None,
        )
    except subprocess.TimeoutExpired:
        return RuntimeAttestation(
            id=f"attest-{runtime}-timeout-{int(time.time() * 1000)}",
            runtime=runtime,
            execution_class=ExecutionClass.BLOCKED,
            executable=resolved,
            version=version,
            command=list(command),
            started_at=started_at,
            ended_at=utc_now(),
            elapsed_seconds=time.monotonic() - started,
            failure_kind=FailureKind.TIMEOUT,
            blocked_reason=f"exceeded {timeout_seconds}s",
            host=_host(),
            authority=AuthorityClass.UNRESOLVED,
        ).seal()

    stdout, stderr = completed.stdout or "", completed.stderr or ""
    returncode = completed.returncode
    marker_missing = bool(expect_marker) and expect_marker not in stdout
    succeeded = returncode == 0 and not marker_missing

    failure_kind: FailureKind | None = None
    if returncode != 0:
        failure_kind = classify_failure(stdout, stderr, returncode)
    elif marker_missing:
        failure_kind = FailureKind.SCRIPT_ERROR

    return RuntimeAttestation(
        id=f"attest-{runtime}-{int(time.time() * 1000)}",
        runtime=runtime,
        # A run that really executed is PHYSICAL even when it failed: the
        # attestation proves the hardware path works and the work did not.
        execution_class=ExecutionClass.PHYSICAL if succeeded else ExecutionClass.DIAGNOSTIC_ONLY,
        executable=resolved,
        executable_sha256=_digest_if_present(Path(resolved)),
        version=version,
        command=list(command),
        process_id=os.getpid(),
        started_at=started_at,
        ended_at=utc_now(),
        elapsed_seconds=time.monotonic() - started,
        returncode=returncode,
        failure_kind=failure_kind,
        host=_host(),
        environment={
            key: environment.get(key, "")
            for key in ("BVMCP_NETWORK_DISABLED", "VIRTUAL_ENV", "PATH")
            if key in environment
        },
        output_digests={
            name: _digest_if_present(path) for name, path in (outputs or {}).items()
        },
        stdout_tail=stdout[-4000:],
        stderr_tail=stderr[-4000:],
        blocked_reason=(
            f"exit {returncode} classified {failure_kind.value}"
            if failure_kind and returncode != 0
            else (f"completion marker {expect_marker!r} absent" if marker_missing else "")
        ),
        authority=(
            AuthorityClass.RUNTIME_OBSERVED if succeeded else AuthorityClass.MODEL_DERIVED
        ),
    ).seal()
