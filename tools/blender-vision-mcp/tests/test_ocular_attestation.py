"""Phase B regression fixtures.

Every defect the V2 run shipped becomes a permanent test here. These are not
hypothetical failure modes; each one was observed, diagnosed, and cost real
time, and each is now cheap to detect.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.attestation import (
    ExecutionClass,
    FailureKind,
    FalsePhysicalAuthority,
    RuntimeAttestation,
    attest_blocked,
    attest_substitute,
    classify_failure,
    run_attested,
)
from blender_vision.v2.authority import AuthorityClass

BLENDER = "/Applications/Blender.app/Contents/MacOS/Blender"
BLENDER_GATE = pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender attestation",
)


# ------------------------------------------------------- no fallback passes


@pytest.mark.parametrize(
    "verdict", ["PHYSICAL_PASS", "HARDWARE_VERIFIED", "REAL_RUNTIME_VERIFIED"]
)
@pytest.mark.parametrize(
    "execution_class",
    [ExecutionClass.DIAGNOSTIC_ONLY, ExecutionClass.CANDIDATE_ONLY, ExecutionClass.BLOCKED],
)
def test_no_substitute_can_emit_a_physical_verdict(verdict, execution_class) -> None:
    attestation = RuntimeAttestation(
        id="a", runtime="blender", execution_class=execution_class
    ).seal()
    with pytest.raises(FalsePhysicalAuthority):
        attestation.verdict(verdict)


def test_a_substitute_cannot_be_attested_physical() -> None:
    with pytest.raises(FalsePhysicalAuthority):
        attest_substitute(
            "blender",
            execution_class=ExecutionClass.PHYSICAL,
            reason="offline mesh",
            substitute="trimesh",
        )


def test_a_substitute_cannot_claim_runtime_observed_authority() -> None:
    with pytest.raises(FalsePhysicalAuthority):
        RuntimeAttestation(
            id="a",
            runtime="blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            authority=AuthorityClass.RUNTIME_OBSERVED,
        ).seal()


def test_blocked_runtime_records_its_reason_rather_than_failing_silently() -> None:
    attestation = attest_blocked("colmap_dense", "COLMAP built without CUDA")
    assert attestation.execution_class is ExecutionClass.BLOCKED
    assert "CUDA" in attestation.blocked_reason
    assert not attestation.is_physical
    attestation.verify()


# ------------------------------------------------- no fabricated hardware blame


def test_relative_script_path_is_not_blamed_on_the_gpu() -> None:
    """The exact V2 defect: a relative --python path published as a Metal fault."""
    kind = classify_failure(
        "",
        "Error: Cannot open file /relative/emit_blender.py: No such file or directory",
        1,
    )
    assert kind is FailureKind.PATH_ERROR


def test_a_real_metal_crash_is_still_classified_as_hardware() -> None:
    kind = classify_failure("", "MTLBackend::metal_is_supported failed", 139)
    assert kind is FailureKind.HARDWARE_ERROR


def test_a_script_error_that_segfaults_is_not_hardware() -> None:
    kind = classify_failure(
        "",
        "Traceback (most recent call last):\n  File x\nAttributeError: no attribute\n"
        "Segmentation fault: 11",
        139,
    )
    assert kind is FailureKind.API_ERROR


def test_an_unexplained_crash_stays_unclassified() -> None:
    assert classify_failure("", "", 134) is FailureKind.UNCLASSIFIED


def test_missing_dependency_is_not_hardware() -> None:
    assert (
        classify_failure("", "ModuleNotFoundError: No module named 'bpy'", 1)
        is FailureKind.DEPENDENCY_ERROR
    )


def test_classify_refuses_a_successful_run() -> None:
    with pytest.raises(ValidationError):
        classify_failure("", "", 0)


# --------------------------------------------------------- real execution


@BLENDER_GATE
def test_real_blender_run_is_attested_physical() -> None:
    attestation = run_attested(
        "blender", [BLENDER, "--version"], version_argv=["--version"], timeout_seconds=120
    )
    assert attestation.execution_class is ExecutionClass.PHYSICAL
    assert attestation.returncode == 0
    assert "Blender" in attestation.version
    assert attestation.executable_sha256
    assert attestation.verdict("PHYSICAL_PASS") == "PHYSICAL_PASS"
    attestation.verify()


@BLENDER_GATE
def test_a_zero_exit_without_its_marker_is_not_a_pass() -> None:
    """A process can finish while its work does not."""
    attestation = run_attested(
        "blender",
        [BLENDER, "--version"],
        version_argv=["--version"],
        expect_marker="NEVER_PRINTED_MARKER",
        timeout_seconds=120,
    )
    assert attestation.returncode == 0
    assert attestation.execution_class is ExecutionClass.DIAGNOSTIC_ONLY
    assert attestation.failure_kind is FailureKind.SCRIPT_ERROR
    with pytest.raises(FalsePhysicalAuthority):
        attestation.verdict("PHYSICAL_PASS")


def test_a_missing_executable_is_blocked_not_crashed() -> None:
    attestation = run_attested("nonesuch", ["definitely-not-installed-xyz", "--version"])
    assert attestation.execution_class is ExecutionClass.BLOCKED
    assert "not installed" in attestation.blocked_reason


# ------------------------------------------------------- frame regressions


def test_blender_and_gltf_frames_are_declared_incompatible() -> None:
    """Z-up vs Y-up made a wall behave like a floor. It must never be implicit."""
    from blender_vision.v2.authority import BLENDER_WORLD, GLTF_WORLD

    assert not BLENDER_WORLD.compatible_with(GLTF_WORLD)
    with pytest.raises(ValidationError):
        BLENDER_WORLD.require_compatible(GLTF_WORLD)


def test_the_flagship_runtime_declares_its_frame_conversion() -> None:
    """The web runtime must state the conversion, not apply it silently."""
    runtime = (
        Path(__file__).resolve().parents[1]
        / "sandbox"
        / "datacenter-film"
        / "src"
        / "film.js"
    )
    if not runtime.is_file():
        pytest.skip("datacenter film runtime is not present in this checkout")
    source = runtime.read_text(encoding="utf-8")
    assert "BLENDER_FROM_GLTF_X_ROTATION" in source, "frame conversion is unnamed"
    assert "camera.up.set(0, 0, 1)" in source, "camera up is not set for a Z-up world"


def test_the_flagship_runtime_keeps_drawing_until_damping_converges() -> None:
    """Request-on-demand plus damping froze the camera part-way to its target."""
    runtime = (
        Path(__file__).resolve().parents[1]
        / "sandbox"
        / "datacenter-film"
        / "src"
        / "film.js"
    )
    if not runtime.is_file():
        pytest.skip("datacenter film runtime is not present in this checkout")
    source = runtime.read_text(encoding="utf-8")
    assert "distanceToSquared(pendingTarget)" in source, "convergence guard is missing"


# --------------------------------------------------------- browser process law


def test_the_browser_lock_harness_exists_and_reaps_on_interrupt() -> None:
    harness = Path(__file__).resolve().parents[1] / "scripts" / "with-one-browser.sh"
    assert harness.is_file(), "browser serialization harness is missing"
    source = harness.read_text(encoding="utf-8")
    # A trap on EXIT alone leaks when a run is interrupted, which is exactly how
    # ten engines survived the V2 session.
    assert "trap 'reap' EXIT INT TERM HUP" in source
    assert "BROWSER LEAK" in source, "the harness does not fail on a leak"
