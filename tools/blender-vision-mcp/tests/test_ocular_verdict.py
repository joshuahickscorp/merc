"""Verdict authority guards, each proven against a reconstructed real failure.

A guard that never fires is the same mistake in a new place, so every test here
rebuilds the exact historical artifact that should have been rejected and
asserts the guard rejects it.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from blender_vision.ocular.attestation import (
    ExecutionClass,
    FalsePhysicalAuthority,
    RuntimeAttestation,
)
from blender_vision.ocular.verdict import (
    Verdict,
    VerdictStatus,
    assert_no_direct_physical_verdict,
    issue_verdict,
)
from blender_vision.v2.authority import AuthorityClass


def _physical(**overrides) -> RuntimeAttestation:
    payload = {
        "id": "attest-1",
        "runtime": "blender",
        "execution_class": ExecutionClass.PHYSICAL,
        "returncode": 0,
        "stdout_tail": "OCULAR_TABLETOP_COMPLETE\n",
        "stderr_tail": "",
    }
    payload.update(overrides)
    return RuntimeAttestation(**payload).seal()


def _artifact(tmp_path: Path, name: str = "out.png") -> Path:
    path = tmp_path / name
    path.write_bytes(b"rendered-bytes")
    return path


# ------------------------------------------------------------- the happy path


def test_a_full_physical_chain_produces_a_physical_pass(tmp_path: Path) -> None:
    verdict = issue_verdict(
        "tracking",
        _physical(),
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        evaluator_passed=True,
    )
    assert verdict.status is VerdictStatus.PHYSICAL_PASS
    assert verdict.reasons == []
    verdict.require_physical_pass()
    verdict.verify()


def test_a_real_run_that_measured_failure_is_physical_fail_not_diagnostic(
    tmp_path: Path,
) -> None:
    """A genuine negative result is authoritative evidence, not a downgrade."""
    verdict = issue_verdict(
        "tracking",
        _physical(),
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        evaluator_passed=True,
        result_ok=False,
    )
    assert verdict.status is VerdictStatus.PHYSICAL_FAIL
    assert verdict.is_physical
    with pytest.raises(FalsePhysicalAuthority):
        verdict.require_physical_pass()


# --------------------------------------------- reconstructed historical failures


def test_the_synthetic_substitute_run_cannot_pass(tmp_path: Path) -> None:
    """The exact tracking-lane artifact pair that reported PASS.

    render_attestation was DIAGNOSTIC_ONLY with substituted_by
    synthetic_sequence, and the lane still printed PASS with
    occlusion_survival_rate 1.0.
    """
    attestation = RuntimeAttestation(
        id="attest-hist",
        runtime="blender",
        execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
        substituted_by="synthetic_sequence",
        blocked_reason="completion marker 'OCULAR_TABLETOP_COMPLETE' absent",
        stdout_tail="",
    ).seal()
    verdict = issue_verdict(
        "tracking",
        attestation,
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        evaluator_passed=True,
    )
    assert verdict.status is VerdictStatus.DIAGNOSTIC
    assert any("DIAGNOSTIC_ONLY" in r for r in verdict.reasons)
    assert any("substituted by synthetic_sequence" in r for r in verdict.reasons)
    assert any("marker" in r for r in verdict.reasons)
    with pytest.raises(FalsePhysicalAuthority):
        verdict.require_physical_pass()


def test_fabricated_metal_signature_blocks_a_pass(tmp_path: Path) -> None:
    """run-ocular-tracking.py appended invented Metal/WM_init text before
    classification, turning a Python AttributeError into a hardware verdict."""
    attestation = _physical(
        stderr_tail=(
            "segmentation fault sigsegv returncode=-11\n"
            "metal_is_supported GPU backend type selection during WM_init"
        )
    )
    verdict = issue_verdict(
        "tracking",
        attestation,
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        evaluator_passed=True,
    )
    assert verdict.status is VerdictStatus.DIAGNOSTIC
    assert any("fabricated signature" in r for r in verdict.reasons)


def test_missing_completion_marker_blocks_a_pass(tmp_path: Path) -> None:
    """Blender exited 0 having rendered one frame, then the script raised."""
    verdict = issue_verdict(
        "tracking",
        _physical(stdout_tail="Saved: frame_0001.png\nBlender quit\n"),
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        evaluator_passed=True,
    )
    assert verdict.status is VerdictStatus.DIAGNOSTIC
    assert any("marker" in r for r in verdict.reasons)


def test_missing_artifact_blocks_a_pass(tmp_path: Path) -> None:
    verdict = issue_verdict(
        "tracking",
        _physical(),
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": tmp_path / "never-written.png"},
        evaluator_passed=True,
    )
    assert verdict.status is VerdictStatus.DIAGNOSTIC
    assert any("artifact missing" in r for r in verdict.reasons)


def test_digest_mismatch_blocks_a_pass(tmp_path: Path) -> None:
    verdict = issue_verdict(
        "tracking",
        _physical(),
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        expected_digests={"render": "0" * 64},
        evaluator_passed=True,
    )
    assert verdict.status is VerdictStatus.DIAGNOSTIC
    assert any("digest mismatch" in r for r in verdict.reasons)


def test_unevaluated_contract_blocks_a_pass(tmp_path: Path) -> None:
    verdict = issue_verdict(
        "tracking",
        _physical(),
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        evaluator_passed=None,
    )
    assert verdict.status is VerdictStatus.DIAGNOSTIC
    assert any("evaluator contract was not evaluated" in r for r in verdict.reasons)


def test_absent_attestation_is_blocked_not_passed() -> None:
    verdict = issue_verdict("tracking", None, evaluator_passed=True)
    assert verdict.status is VerdictStatus.BLOCKED
    assert any("no runtime attestation" in r for r in verdict.reasons)


# ------------------------------------------------------------- authority law


def test_a_non_physical_verdict_cannot_claim_runtime_observed() -> None:
    with pytest.raises(FalsePhysicalAuthority):
        Verdict(
            id="v",
            lane="tracking",
            status=VerdictStatus.DIAGNOSTIC,
            authority=AuthorityClass.RUNTIME_OBSERVED,
        ).seal()


def test_direct_physical_verdict_construction_is_rejected() -> None:
    source = 'result["status"] = "PHYSICAL_PASS"'
    with pytest.raises(FalsePhysicalAuthority, match="constructs a physical verdict"):
        assert_no_direct_physical_verdict(source, where="some_runner.py")


def test_ocular_runners_do_not_construct_physical_verdicts_directly() -> None:
    """Repository-wide: only verdict.py may name a physical verdict."""
    root = Path(__file__).resolve().parents[1]
    offenders: list[str] = []
    for script in sorted((root / "scripts").glob("run-ocular-*.py")):
        try:
            assert_no_direct_physical_verdict(
                script.read_text(encoding="utf-8"), where=script.name
            )
        except FalsePhysicalAuthority as error:
            offenders.append(str(error))
    assert not offenders, "; ".join(offenders)


def test_reasons_are_always_recorded_even_on_a_pass(tmp_path: Path) -> None:
    """A receipt must carry the reason list so a pass is auditable, not asserted."""
    from blender_vision.ocular.verdict import summarise

    verdict = issue_verdict(
        "tracking",
        _physical(),
        expected_marker="OCULAR_TABLETOP_COMPLETE",
        required_artifacts={"render": _artifact(tmp_path)},
        evaluator_passed=True,
    )
    payload = summarise(verdict)
    assert payload["status"] == "PHYSICAL_PASS"
    assert "reasons" in payload
    assert payload["artifacts"]["render"]
    assert payload["digest"]
