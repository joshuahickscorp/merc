"""Single split authority for ocular proposal-fusion evaluation."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

SPLIT_ROOT = Path(__file__).resolve().parent
MANIFEST_PATH = SPLIT_ROOT / "manifest.json"
HIDDEN_CANARY_PATH = SPLIT_ROOT / "hidden" / "ORACLE_CANARY.txt"
BUILDER_INPUTS = SPLIT_ROOT / "builder_inputs"


def load_manifest() -> dict[str, Any]:
    return json.loads(MANIFEST_PATH.read_text(encoding="utf-8"))


def load_canary() -> str:
    return HIDDEN_CANARY_PATH.read_text(encoding="utf-8").strip()


def builder_visible_paths() -> list[Path]:
    """Every path the builder is allowed to read under this split tree."""
    paths: list[Path] = [MANIFEST_PATH]
    if BUILDER_INPUTS.is_dir():
        paths.extend(sorted(BUILDER_INPUTS.rglob("*")))
    return [p for p in paths if p.is_file()]


def assert_canary_absent_from_builder_inputs(canary: str | None = None) -> None:
    """Leakage canary: hidden marker must not appear in builder-visible files."""
    marker = canary if canary is not None else load_canary()
    if not marker:
        raise AssertionError("hidden canary is empty")
    for path in builder_visible_paths():
        text = path.read_text(encoding="utf-8", errors="replace")
        if marker in text:
            raise AssertionError(
                f"leakage canary present in builder-visible file {path}"
            )


def public_conditions() -> list[str]:
    manifest = load_manifest()
    cal = list(manifest["splits"]["calibration"]["conditions"])
    pub = list(manifest["splits"]["public_development"]["conditions"])
    return cal + pub


def hidden_conditions() -> list[str]:
    """Evaluator-only. Builder path must not call this to drive detection."""
    return list(load_manifest()["splits"]["sealed_hidden"]["conditions"])
