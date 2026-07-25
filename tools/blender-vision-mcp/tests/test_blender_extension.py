import ast
import tomllib
from pathlib import Path


def test_blender_extension_is_thin_and_manifested() -> None:
    root = Path(__file__).resolve().parents[1] / "blender_extension"
    manifest = tomllib.loads((root / "blender_manifest.toml").read_text(encoding="utf-8"))
    source = (root / "__init__.py").read_text(encoding="utf-8")
    tree = ast.parse(source)

    assert manifest["id"] == "blender_vision_mcp"
    assert manifest["type"] == "add-on"
    assert manifest["blender_version_min"] == "4.2.0"
    assert "ThreadingHTTPServer" not in source
    imports = {node.names[0].name for node in ast.walk(tree) if isinstance(node, ast.Import)}
    assert "socket" not in imports
    assert "VIEW_3D" in source
    assert "bvmcp.cancel_job" in source
    assert "127.0.0.1" in source
