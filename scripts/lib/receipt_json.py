"""Shared receipt-JSON helpers used by multiple validate-* gates.

Exact-duplicate bodies only. Stricter variants (e.g. parse_utc with a length
bound in the GO-closure evidence-chain validator) stay local to that gate.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import math
from pathlib import Path
from typing import Any, Iterable


def fail(message: str) -> None:
    raise ValueError(message)


def object_without_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            fail(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def reject_constant(value: str):
    fail(f"non-finite JSON number {value}")


def exact_keys(value, expected: set[str], field: str) -> None:
    if not isinstance(value, dict) or set(value) != expected:
        fail(f"{field} does not match its closed schema")


def parse_utc(value, field):
    """RFC3339 UTC timestamp ending in Z. No length bound (stricter variants stay local)."""
    if not isinstance(value, str) or not value.endswith("Z"):
        fail(f"{field} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as exc:
        fail(f"{field} is not a valid RFC3339 timestamp: {exc}")
    if parsed.utcoffset() != dt.timedelta(0):
        fail(f"{field} must be UTC")
    return parsed


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def all_strings(value) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield str(key)
            yield from all_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from all_strings(item)


# Historical name used by agent-restart / canary-scenario validators.
strings = all_strings


def finite_numbers(value) -> bool:
    if isinstance(value, float) and not math.isfinite(value):
        return False
    if isinstance(value, dict):
        return all(finite_numbers(item) for item in value.values())
    if isinstance(value, list):
        return all(finite_numbers(item) for item in value)
    return True
