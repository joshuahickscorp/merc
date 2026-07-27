"""Real stdio callthrough for the nineteen ocular MCP tools.

Starts one MCP server subprocess and asserts every ocular tool either PASS
or is explicitly BLOCKED with a reason. Fails if a tool regresses to
unregistered or returns a hard error.
"""

from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT_PATH = ROOT / "scripts" / "verify-ocular-mcp-callthrough.py"


def _load_script():
    import sys

    name = "verify_ocular_mcp_callthrough"
    if name in sys.modules:
        return sys.modules[name]
    spec = importlib.util.spec_from_file_location(name, SCRIPT_PATH)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    # dataclasses need the module present in sys.modules during class body exec.
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.mark.asyncio
async def test_ocular_mcp_callthrough_stdio(tmp_path: Path) -> None:
    """One server process, all nineteen tools over the real stdio wire."""
    mod = _load_script()
    assert len(mod.OCULAR_TOOLS) == 19

    artifact = tmp_path / "mcp-callthrough.json"
    work = tmp_path / "work"
    work.mkdir()

    record = await mod.run_callthrough(
        artifact_path=artifact,
        work_dir=work,
    )

    assert artifact.is_file(), "callthrough did not write artifact"
    assert record["transport"] == "stdio"
    assert record["ocular_listed"] == 19, (
        f"ocular tools missing from tools/list: listed={record['ocular_listed']}"
    )
    assert record["tools_listed_total"] >= 286

    summary = record["summary"]
    assert summary["called"] == 19
    assert summary["failed"] == 0, (
        "failures:\n"
        + "\n".join(
            f"  {r['tool']}: {r.get('error')}"
            for r in record["results"]
            if r["status"] == "FAIL"
        )
    )
    assert summary["succeeded"] + summary["blocked"] == 19

    by_name = {r["tool"]: r for r in record["results"]}
    for name in mod.OCULAR_TOOLS:
        assert name in by_name, f"missing result row for {name}"
        row = by_name[name]
        assert row["status"] in {"PASS", "BLOCKED"}, (
            f"{name} status={row['status']!r} error={row.get('error')!r}"
        )
        if row["status"] == "BLOCKED":
            assert row.get("blocked_reason"), f"{name} BLOCKED without reason"

    # Unregistered regression: every ocular name must be executed, not only listed.
    executed = {r["tool"] for r in record["results"]}
    assert executed == set(mod.OCULAR_TOOLS)
