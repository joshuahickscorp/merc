from __future__ import annotations

import os
from pathlib import Path

from blender_vision.core.errors import SecurityError


def safe_mode() -> bool:
    return os.environ.get("BVMCP_UNSAFE") != "1"


def confined_path(root: Path, candidate: Path, *, must_exist: bool = False) -> Path:
    resolved_root = root.expanduser().resolve()
    resolved = candidate.expanduser().resolve()
    if not resolved.is_relative_to(resolved_root):
        raise SecurityError(f"path escapes project root: {candidate}")
    if must_exist and not resolved.exists():
        raise SecurityError(f"path does not exist: {candidate}")
    return resolved


def safe_filename(name: str) -> str:
    cleaned = "".join(
        character if character.isalnum() or character in "._-" else "_" for character in name
    )
    cleaned = cleaned.strip("._")
    if not cleaned:
        raise SecurityError("filename is empty after sanitization")
    return cleaned[:180]
