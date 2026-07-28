from __future__ import annotations

import argparse
import ast
import hashlib
import json
import subprocess
import tempfile
import tomllib
from pathlib import Path

from blender_vision.projects.store import SCHEMA, ProjectStore

ROOT = Path(__file__).resolve().parents[1]


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def tracked_files() -> list[Path]:
    result = subprocess.run(
        ["git", "ls-files", "tools/blender-vision-mcp"],
        cwd=ROOT.parents[1],
        check=True,
        capture_output=True,
        text=True,
    )
    return [
        ROOT.parents[1] / value
        for value in result.stdout.splitlines()
        if value and (ROOT.parents[1] / value).is_file()
    ]


def source_tree_digest(paths: list[Path]) -> str:
    digest = hashlib.sha256()
    repository = ROOT.parents[1]
    for path in sorted(paths):
        relative = path.relative_to(repository).as_posix().encode()
        digest.update(len(relative).to_bytes(8, "big"))
        digest.update(relative)
        digest.update(bytes.fromhex(sha256(path)))
    return digest.hexdigest()


def test_count(paths: list[Path]) -> int:
    count = 0
    for path in paths:
        if path.parent.name != "tests" or path.suffix != ".py":
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        count += sum(
            1
            for node in ast.walk(tree)
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            and node.name.startswith("test_")
        )
    return count


def sqlite_tables() -> list[str]:
    with tempfile.TemporaryDirectory(prefix="visionmcp-census-") as temporary:
        project = ProjectStore.create(Path(temporary) / "project", "Core census")
        with project.connection() as connection:
            rows = connection.execute(
                "SELECT name FROM sqlite_master "
                "WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"
            ).fetchall()
    return [str(row["name"]) for row in rows]


def build_census() -> dict[str, object]:
    paths = tracked_files()
    python_paths = [path for path in paths if path.suffix == ".py"]
    schema_paths = sorted((ROOT / "schemas").glob("*.json"))
    manifest = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    tables = sqlite_tables()
    commit = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=ROOT.parents[1],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    bible = Path("/Users/scammermike/Downloads/VISIONMCP_ALL_SEEING_EYE_EXPANSION_BIBLE.md")
    return {
        "schema_version": 1,
        "authority": {
            "branch": "feature/blender-vision-mcp",
            "census_base_commit": commit,
            "expansion_bible_sha256": sha256(bible) if bible.is_file() else None,
        },
        "repository": {
            "tracked_project_files": len(paths),
            "python_files": len(python_paths),
            "python_lines": sum(
                len(path.read_text(encoding="utf-8").splitlines()) for path in python_paths
            ),
            "test_functions": test_count(paths),
            "json_schema_files": len(schema_paths),
            "source_tree_sha256": source_tree_digest(paths),
        },
        "dependencies": {
            "python_requires": manifest["project"]["requires-python"],
            "runtime": manifest["project"]["dependencies"],
            "optional": manifest["project"]["optional-dependencies"],
            "lockfile_sha256": sha256(ROOT / "uv.lock"),
        },
        "state": {
            "engine": "SQLite",
            "schema_sha256": hashlib.sha256(SCHEMA.encode()).hexdigest(),
            "table_count": len(tables),
            "tables": tables,
        },
        "artifact_store": {
            "identity": "sha256",
            "layout": "artifacts/sha256/{digest[0:2]}/{digest[2:4]}/{digest}",
            "registration_table": "artifacts",
            "immutable_source_bytes": True,
            "tamper_verification": True,
            "project_portable": True,
        },
    }


def markdown(census: dict[str, object]) -> str:
    authority = census["authority"]
    repository = census["repository"]
    dependencies = census["dependencies"]
    state = census["state"]
    artifacts = census["artifact_store"]
    return f"""# VisionMCP core census

This is the deterministic Wave 0 census of the recovered July 21 authority.
Capability claims remain bounded by executable tests and runtime receipts.

## Authority

- Branch: `{authority["branch"]}`
- Census base commit: `{authority["census_base_commit"]}`
- Expansion bible SHA-256: `{authority["expansion_bible_sha256"]}`
- Project source-tree SHA-256: `{repository["source_tree_sha256"]}`

## Repository

- Tracked project files: {repository["tracked_project_files"]}
- Python files: {repository["python_files"]}
- Python lines: {repository["python_lines"]}
- Collected test functions: {repository["test_functions"]}
- JSON Schema files: {repository["json_schema_files"]}

## Dependencies

- Python: `{dependencies["python_requires"]}`
- Runtime dependencies: {", ".join(f"`{item}`" for item in dependencies["runtime"])}
- Lockfile SHA-256: `{dependencies["lockfile_sha256"]}`

## State and artifacts

- State engine: {state["engine"]}
- SQLite tables: {state["table_count"]}
- Embedded schema SHA-256: `{state["schema_sha256"]}`
- Artifact identity: {artifacts["identity"]}
- Artifact layout: `{artifacts["layout"]}`
- Source bytes are immutable, content-addressed, portable, and tamper-verified.

The complete table and optional-dependency inventories are in
`docs/CORE_CENSUS.json`.
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--json", type=Path, default=ROOT / "docs" / "CORE_CENSUS.json")
    parser.add_argument("--markdown", type=Path, default=ROOT / "docs" / "CORE_CENSUS.md")
    args = parser.parse_args()
    census = build_census()
    args.json.write_text(json.dumps(census, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    args.markdown.write_text(markdown(census), encoding="utf-8")
    print(
        json.dumps(
            {
                "json": str(args.json),
                "markdown": str(args.markdown),
                "source_tree_sha256": census["repository"]["source_tree_sha256"],
            },
            sort_keys=True,
        )
    )


if __name__ == "__main__":
    main()
