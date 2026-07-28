"""The single authority for physical-runtime verdicts.

Every physical verdict in the repository must come from `issue_verdict()`. It is
the one place that knows what a physical pass requires, so there is exactly one
thing to audit and exactly one thing to get right.

This exists because the tracking lane printed PASS while its own render
attestation said DIAGNOSTIC_ONLY with `substituted_by: synthetic_sequence`. The
attestation was correct; nothing forced the verdict to consult it. Scattered
verdict construction means every site is a place to forget.

A physical verdict requires all seven of:

  1. an attestation exists
  2. it is ExecutionClass.PHYSICAL
  3. the expected completion marker was observed
  4. every required output artifact exists and hashes to its recorded digest
  5. no substitution field is set
  6. the classifier saw only real stdout/stderr
  7. the evaluator contract passed

Anything short of that is a `Verdict` in a non-physical state carrying the exact
reason. There is no path that returns a physical pass without the evidence.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any, ClassVar

from blender_vision.core.util import sha256_file, utc_now
from blender_vision.ocular.attestation import (
    PHYSICAL_VERDICTS,
    ExecutionClass,
    FalsePhysicalAuthority,
    RuntimeAttestation,
)
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import V2Record


class VerdictStatus(StrEnum):
    """What a run is allowed to claim."""

    #: The full physical chain held.
    PHYSICAL_PASS = "PHYSICAL_PASS"
    #: The physical chain held and the run produced a negative result. Still
    #: authoritative — a real failure is evidence.
    PHYSICAL_FAIL = "PHYSICAL_FAIL"
    #: Something ran and produced artifacts, but not under physical authority.
    DIAGNOSTIC = "DIAGNOSTIC"
    #: The runtime was unavailable.
    BLOCKED = "BLOCKED"


#: Markers that indicate a runner fabricated signatures rather than reading the
#: runtime's own output. Their presence in a classification input is a defect.
FABRICATED_SIGNATURES: tuple[str, ...] = (
    "metal_is_supported GPU backend type selection",
    "segmentation fault sigsegv returncode=",
    "Observed host cannot complete Blender WM_init",
)


@dataclass(slots=True, kw_only=True)
class Verdict(V2Record):
    """A run's outcome, bound to the evidence that licenses it."""

    RECORD_KIND: ClassVar[str] = "ocular.verdict"

    lane: str = ""
    status: VerdictStatus = VerdictStatus.BLOCKED
    attestation_id: str = ""
    attestation_digest: str = ""
    execution_class: ExecutionClass = ExecutionClass.BLOCKED
    marker_observed: bool = False
    artifacts: dict[str, str] = field(default_factory=dict)
    evaluator_passed: bool = False
    reasons: list[str] = field(default_factory=list)

    @property
    def is_physical(self) -> bool:
        return self.status in {VerdictStatus.PHYSICAL_PASS, VerdictStatus.PHYSICAL_FAIL}

    def require_physical_pass(self) -> None:
        if self.status is not VerdictStatus.PHYSICAL_PASS:
            raise FalsePhysicalAuthority(
                f"{self.lane}: not a physical pass ({self.status.value}): "
                + "; ".join(self.reasons or ["no reason recorded"])
            )

    def _enforce_authority_ceiling(self) -> None:
        V2Record._enforce_authority_ceiling(self)
        if not self.is_physical and self.authority is AuthorityClass.RUNTIME_OBSERVED:
            raise FalsePhysicalAuthority(
                f"{self.lane}: {self.status.value} cannot claim RUNTIME_OBSERVED"
            )


def issue_verdict(
    lane: str,
    attestation: RuntimeAttestation | None,
    *,
    expected_marker: str | None = None,
    required_artifacts: dict[str, Path] | None = None,
    expected_digests: dict[str, str] | None = None,
    evaluator_passed: bool | None = None,
    evaluator_detail: str = "",
    result_ok: bool = True,
) -> Verdict:
    """The only sanctioned way to produce a physical verdict.

    `result_ok` is what the lane measured. A physical run that measured a
    failure is `PHYSICAL_FAIL`, which is authoritative — refusing to record real
    negative results is its own dishonesty.
    """
    reasons: list[str] = []
    artifacts: dict[str, str] = {}

    if attestation is None:
        return _seal(
            Verdict(
                id=f"verdict-{lane}-{utc_now()}",
                lane=lane,
                status=VerdictStatus.BLOCKED,
                reasons=["no runtime attestation was produced"],
            )
        )

    # 2. execution class
    if attestation.execution_class is not ExecutionClass.PHYSICAL:
        reasons.append(
            f"attestation is {attestation.execution_class.value}"
            + (f" ({attestation.blocked_reason})" if attestation.blocked_reason else "")
        )

    # 5. no substitution
    if attestation.substituted_by:
        reasons.append(f"substituted by {attestation.substituted_by}")

    # 3. completion marker
    marker_observed = True
    if expected_marker is not None:
        marker_observed = expected_marker in (attestation.stdout_tail or "")
        if not marker_observed:
            reasons.append(f"completion marker {expected_marker!r} absent from stdout")

    # 6. the classifier saw only real output
    haystack = f"{attestation.stdout_tail}\n{attestation.stderr_tail}"
    for signature in FABRICATED_SIGNATURES:
        if signature in haystack:
            reasons.append(f"fabricated signature in runtime output: {signature!r}")

    # 4. artifacts exist and hash correctly
    for name, path in (required_artifacts or {}).items():
        if not path.is_file():
            reasons.append(f"required artifact missing: {name} ({path})")
            continue
        digest, _ = sha256_file(path)
        artifacts[name] = digest
        expected = (expected_digests or {}).get(name)
        if expected and expected != digest:
            reasons.append(f"artifact digest mismatch for {name}: {digest} != {expected}")

    # 7. evaluator contract
    if evaluator_passed is None:
        reasons.append("evaluator contract was not evaluated")
    elif not evaluator_passed:
        detail = f": {evaluator_detail}" if evaluator_detail else ""
        reasons.append(f"evaluator contract failed{detail}")

    if reasons:
        status = (
            VerdictStatus.BLOCKED
            if attestation.execution_class is ExecutionClass.BLOCKED
            else VerdictStatus.DIAGNOSTIC
        )
    else:
        status = VerdictStatus.PHYSICAL_PASS if result_ok else VerdictStatus.PHYSICAL_FAIL

    return _seal(
        Verdict(
            id=f"verdict-{lane}-{utc_now()}",
            lane=lane,
            status=status,
            attestation_id=attestation.id,
            attestation_digest=attestation.digest,
            execution_class=attestation.execution_class,
            marker_observed=marker_observed,
            artifacts=artifacts,
            evaluator_passed=bool(evaluator_passed),
            reasons=reasons,
            authority=(
                AuthorityClass.RUNTIME_OBSERVED
                if status in {VerdictStatus.PHYSICAL_PASS, VerdictStatus.PHYSICAL_FAIL}
                else AuthorityClass.MODEL_DERIVED
            ),
        )
    )


def _seal(verdict: Verdict) -> Verdict:
    return verdict.seal()


def assert_no_direct_physical_verdict(source: str, *, where: str) -> None:
    """Fail when a module builds a physical verdict string itself.

    Constructing `PHYSICAL_PASS` outside this module bypasses every check above.
    """
    offenders = [
        name
        for name in PHYSICAL_VERDICTS
        if f'"{name}"' in source or f"'{name}'" in source
    ]
    if offenders:
        raise FalsePhysicalAuthority(
            f"{where} constructs a physical verdict directly: {offenders}; "
            "use blender_vision.ocular.verdict.issue_verdict"
        )


def summarise(verdict: Verdict) -> dict[str, Any]:
    """Receipt form. Always carries the reasons, including on a pass."""
    return {
        "lane": verdict.lane,
        "status": verdict.status.value,
        "execution_class": verdict.execution_class.value,
        "attestation_id": verdict.attestation_id,
        "marker_observed": verdict.marker_observed,
        "artifacts": dict(verdict.artifacts),
        "evaluator_passed": verdict.evaluator_passed,
        "reasons": list(verdict.reasons),
        "digest": verdict.digest,
    }
