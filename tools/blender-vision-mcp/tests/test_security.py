from pathlib import Path

import pytest

from blender_vision.core.errors import SecurityError
from blender_vision.security.paths import confined_path, safe_filename


def test_confined_path_rejects_escape(tmp_path: Path) -> None:
    root = tmp_path / "root"
    root.mkdir()
    with pytest.raises(SecurityError):
        confined_path(root, tmp_path / "outside")


def test_safe_filename_removes_path_syntax() -> None:
    assert safe_filename("../../Mac Studio?.png") == "Mac_Studio_.png"
