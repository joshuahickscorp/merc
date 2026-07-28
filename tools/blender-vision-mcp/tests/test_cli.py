from __future__ import annotations

import json

import pytest

from blender_vision.cli.main import _json_argument


def test_json_argument_accepts_inline_json_longer_than_filename_limit() -> None:
    payload = {
        "base_color": [0.055, 0.038, 0.022, 1.0],
        "procedural_texture": {
            "kind": "layered_open_cell_metal_foam",
            "description": "x" * 300,
        },
    }

    assert _json_argument(json.dumps(payload)) == payload


def test_json_argument_reports_invalid_long_inline_data_as_json_error() -> None:
    with pytest.raises(json.JSONDecodeError):
        _json_argument("x" * 300)
