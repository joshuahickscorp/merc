from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest

from blender_vision.app_build import (
    ApplicationBenchmarkError,
    ApplicationBenchmarkRunner,
    ReferenceCompletenessAnalyzer,
    ReferencePacketLoader,
    load_application_benchmark_manifest,
)

EXPECTED_CASES = {
    "crud-relational",
    "file-upload-boundaries",
    "idempotent-reservation",
    "polling-status-recovery",
    "rbac-denied-paths",
}


def test_fixed_application_benchmark_corpus_is_digest_bound_and_promotable() -> None:
    manifest, manifest_path = load_application_benchmark_manifest()

    assert {case.id for case in manifest.cases} == EXPECTED_CASES
    assert len(manifest.corpus_sha256) == 64
    for case in manifest.cases:
        loaded = ReferencePacketLoader().load(manifest_path.parent / case.packet)
        report = ReferenceCompletenessAnalyzer().analyze(
            loaded.packet,
            verified_source_ids=loaded.verified_source_ids,
        )
        assert report.promotable is True
        assert loaded.packet.canonical_digest() == case.packet_sha256


def test_application_benchmark_fixture_generator_is_current() -> None:
    root = Path(__file__).resolve().parents[1]
    process = subprocess.run(
        ["uv", "run", "python", "scripts/generate-app-benchmark-fixtures.py", "--check"],
        cwd=root,
        check=False,
        capture_output=True,
        text=True,
        timeout=30,
    )

    assert process.returncode == 0, process.stderr


def test_manifest_rejects_corpus_digest_tampering(tmp_path: Path) -> None:
    _manifest, source = load_application_benchmark_manifest()
    document = json.loads(source.read_text(encoding="utf-8"))
    document["corpus_sha256"] = "0" * 64
    tampered = tmp_path / "manifest.json"
    tampered.write_text(json.dumps(document), encoding="utf-8")

    with pytest.raises(ApplicationBenchmarkError, match="corpus digest mismatch"):
        load_application_benchmark_manifest(tampered)


def test_benchmark_refuses_nonempty_output(tmp_path: Path) -> None:
    output = tmp_path / "existing"
    output.mkdir()
    (output / "owned.txt").write_text("preserve\n", encoding="utf-8")

    with pytest.raises(ApplicationBenchmarkError, match="absent or empty"):
        ApplicationBenchmarkRunner().run(output, case_ids={"crud-relational"})

    assert (output / "owned.txt").read_text(encoding="utf-8") == "preserve\n"


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_APP_BENCHMARKS") != "1",
    reason="set BVMCP_RUN_APP_BENCHMARKS=1 for real Node/SQLite application benchmarks",
)
def test_real_fixed_application_benchmark(tmp_path: Path) -> None:
    receipt = ApplicationBenchmarkRunner().run(
        tmp_path / "run",
        case_ids={"crud-relational"},
    )

    assert receipt.functional_passed is True
