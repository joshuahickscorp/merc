"""Strict JSON-Schema validation and tamper verification for V2 records."""

from __future__ import annotations

import json
from functools import cache
from pathlib import Path
from typing import Any

from blender_vision.core.errors import ValidationError
from blender_vision.v2.records import RECORD_TYPES, TamperError, V2Record, load_record


def schema_root() -> Path:
    """Locate `schemas/v2` for both the source tree and the installed wheel."""
    packaged = Path(__file__).resolve().parent.parent / "schemas" / "v2"
    if packaged.is_dir():
        return packaged
    source = Path(__file__).resolve().parents[3] / "schemas" / "v2"
    if source.is_dir():
        return source
    raise FileNotFoundError("schemas/v2 is not available in this installation")


@cache
def _schema_for(record_kind: str) -> dict[str, Any]:
    name = record_kind.removeprefix("v2.")
    path = schema_root() / f"{name}.schema.json"
    if not path.is_file():
        raise FileNotFoundError(f"no V2 schema for {record_kind}: {path}")
    return json.loads(path.read_text(encoding="utf-8"))


@cache
def _validator_for(record_kind: str) -> Any:
    from jsonschema import Draft202012Validator

    return Draft202012Validator(_schema_for(record_kind))


def validate_payload(payload: dict[str, Any]) -> None:
    """Validate a record dictionary against its versioned schema."""
    kind = payload.get("record_kind")
    if kind not in RECORD_TYPES:
        raise ValidationError(f"unknown V2 record kind: {kind!r}")
    errors = sorted(_validator_for(kind).iter_errors(payload), key=lambda item: list(item.path))
    if errors:
        detail = "; ".join(
            f"{'/'.join(str(part) for part in error.path) or '<root>'}: {error.message}"
            for error in errors[:8]
        )
        raise ValidationError(f"{kind} failed schema validation: {detail}")


def validate_record(record: V2Record) -> None:
    validate_payload(record.to_dict())


def verify_payload(payload: dict[str, Any]) -> V2Record:
    """Full gate: schema, digest, and authority ceiling.

    This is the only sanctioned way to accept a record that came from outside
    the current process.
    """
    validate_payload(payload)
    record = load_record(payload)
    stored = payload.get("digest", "")
    record.digest = stored
    record.verify()
    record._enforce_authority_ceiling()
    return record


def write_record(path: Path, record: V2Record) -> Path:
    from blender_vision.core.util import atomic_write_json

    record.seal()
    validate_record(record)
    path.parent.mkdir(parents=True, exist_ok=True)
    atomic_write_json(path, record.to_dict())
    return path


def read_record(path: Path) -> V2Record:
    payload = json.loads(path.read_text(encoding="utf-8"))
    try:
        return verify_payload(payload)
    except TamperError as error:
        raise TamperError(f"{path}: {error}") from error


def all_schema_kinds() -> list[str]:
    return sorted(RECORD_TYPES)
