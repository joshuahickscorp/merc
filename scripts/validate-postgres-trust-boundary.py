#!/usr/bin/env python3
"""CI check for the production Postgres TLS / trust-boundary decision.

Fails unless the active architecture page and docker-compose.prod.yml still
agree that sslmode=disable is only used because Postgres is unpublished on a
single-host Compose network.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DOC = ROOT / "docs" / "ARCHITECTURE.md"
COMPOSE = ROOT / "docker-compose.prod.yml"

REQUIRED_DOC_PHRASES = [
    "sslmode=disable",
    "no `ports:` mapping",
    "private project bridge",
    "single-host",
    "POSTGRES_PASSWORD",
    "the server must actually present a certificate",
]


def fail(msg: str) -> None:
    print(f"postgres-trust-boundary: {msg}", file=sys.stderr)
    raise SystemExit(1)


def postgres_service_block(compose: str) -> str:
    # Capture from "  postgres:" until the next top-level service or EOF.
    m = re.search(r"(?ms)^  postgres:\n(.*?)(?=^  [a-z].*:|\Z)", compose)
    if not m:
        fail("docker-compose.prod.yml has no postgres service")
    return m.group(1)


def main() -> None:
    if not DOC.is_file():
        fail(f"missing {DOC.relative_to(ROOT)}")
    doc = DOC.read_text(encoding="utf-8")
    for phrase in REQUIRED_DOC_PHRASES:
        if phrase not in doc:
            fail(f"{DOC.relative_to(ROOT)} missing required phrase: {phrase!r}")

    compose = COMPOSE.read_text(encoding="utf-8")
    block = postgres_service_block(compose)
    if re.search(r"(?m)^\s+ports:\s*", block):
        fail(
            "postgres service publishes ports:; either remove the publish or "
            "implement real Postgres TLS and update the trust-boundary doc"
        )

    # Control DSN must still match the documented exception (internal hostname,
    # sslmode=disable). Drift to a remote host without TLS is a failure.
    dsn_match = re.search(
        r"(?m)^\s+DATABASE_URL:\s+(.+)$",
        compose,
    )
    if not dsn_match:
        fail("control DATABASE_URL not found in docker-compose.prod.yml")
    dsn = dsn_match.group(1).strip()
    if "sslmode=disable" not in dsn:
        fail(
            "DATABASE_URL no longer uses sslmode=disable; update "
            "docs/ARCHITECTURE.md and this validator together"
        )
    if "@postgres:" not in dsn and "@postgres/" not in dsn:
        fail(
            "DATABASE_URL does not target the internal Compose hostname "
            "`postgres`; remote DSNs require TLS (see trust-boundary doc)"
        )

    print("postgres-trust-boundary: OK")


if __name__ == "__main__":
    main()
