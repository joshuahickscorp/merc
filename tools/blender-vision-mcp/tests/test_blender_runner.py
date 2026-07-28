from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace

from blender_vision.blender.runner import BlenderRunner
from blender_vision.projects.store import ProjectStore


def test_blender_runner_drains_and_bounds_large_worker_output(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Runner Output")
    scene = project.root / "scene" / "fixture.blend"
    scene.write_bytes(b"fixture")
    executable = tmp_path / "fake-blender"
    executable.write_text(
        """#!/usr/bin/env python3
import json
import sys
from pathlib import Path

manifest = json.loads(Path(sys.argv[-1]).read_text(encoding="utf-8"))
sys.stdout.write("x" * 4_000_000 + manifest["project_root"])
sys.stdout.flush()
Path(manifest["result_path"]).write_text(json.dumps({"ok": True}), encoding="utf-8")
""",
        encoding="utf-8",
    )
    executable.chmod(0o755)
    monkeypatch.setattr(
        "blender_vision.blender.runner.discover_blender",
        lambda: SimpleNamespace(available=True, path=str(executable)),
    )

    result = BlenderRunner(project).run("inspect_scene", scene, {}, job_id="large-output")

    assert result["ok"] is True
    log = project.root / result["worker"]["log"]
    contents = log.read_text(encoding="utf-8")
    assert len(contents) <= 2_000_000
    assert str(project.root) not in contents
    assert "$PROJECT" in contents
