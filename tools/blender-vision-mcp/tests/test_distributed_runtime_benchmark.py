from __future__ import annotations

import os
from pathlib import Path

import pytest

from blender_vision.benchmarks.distributed_runtime import (
    DistributedRuntimeBenchmarkRunner,
    load_distributed_runtime_manifest,
)
from blender_vision.cli.main import build_parser


def test_distributed_runtime_manifest_fixes_real_and_external_boundaries() -> None:
    manifest, path = load_distributed_runtime_manifest()

    assert path.name == "manifest.json"
    assert manifest.benchmark_id == "distributed-physical-runtime-v1"
    assert manifest.lease_seconds == 15
    assert set(manifest.required_real_runtimes) == {
        "installed_chromium",
        "installed_blender",
        "additional_browser_engine",
        "two_isolated_worker_processes",
        "device_loss_restart_recovery",
    }
    assert set(manifest.external_requirements) == {
        "second_physical_host",
        "webgpu_adapter",
    }


def test_distributed_runtime_cli_requires_explicit_output() -> None:
    args = build_parser().parse_args(
        ["benchmark", "bootstrap-distributed-runtime", "--output", "evidence"]
    )

    assert args.benchmark_command == "bootstrap-distributed-runtime"
    assert args.output == "evidence"


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_PROCESS_TESTS") != "1",
    reason="set BVMCP_RUN_PROCESS_TESTS=1 for real process-loss recovery",
)
def test_real_process_loss_requeues_and_distinct_process_restarts(
    tmp_path: Path,
) -> None:
    receipt = DistributedRuntimeBenchmarkRunner().run(tmp_path / "evidence")

    assert receipt.functional_passed is True, receipt.failure
    assert receipt.status in {"PASS", "BLOCKED_EXTERNAL"}
    assert len(receipt.processes) == 2
    assert len({item.pid for item in receipt.processes}) == 2
    assert receipt.recovery["status_after_reap"] == "queued"
    assert receipt.recovery["final_status"] == "succeeded"
    assert receipt.recovery["final_attempt"] == 2
    assert all(item.passed for item in receipt.assertions)
