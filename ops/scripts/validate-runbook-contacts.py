#!/usr/bin/env python3
"""Fail readiness while support/incident runbook contacts are still placeholders.

Contacts cannot be invented. Until a human fills
docs/SUPPORT_AND_INCIDENT_RUNBOOK.md, this gate stays red so the gap is visible
rather than dormant.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
RUNBOOK = ROOT / "docs" / "SUPPORT_AND_INCIDENT_RUNBOOK.md"

# Exact placeholder tokens used in the runbook contact block (lines 9-14).
PLACEHOLDER_PATTERNS = [
    re.compile(r"\[CONTACT REQUIRED\]"),
    re.compile(r"\[CHANNEL REQUIRED\]"),
    re.compile(r"\[PRIMARY AND BACKUP REQUIRED\]"),
    # A role alias on a domain that does not exist reaches nobody, so it is a
    # placeholder in exactly the same way an empty field is.  Without this the
    # gate went green the moment the contacts table was formatted, which is the
    # failure mode this whole apparatus exists to prevent.
    re.compile(r"DOMAIN-PENDING-REBRAND"),
    re.compile(r"(?i)\byourdomain\b"),
    re.compile(r"(?i)example\.(com|test|invalid)"),
]


def main() -> int:
    if not RUNBOOK.is_file():
        print(f"runbook-contacts: FAIL: missing {RUNBOOK}", file=sys.stderr)
        return 1
    text = RUNBOOK.read_text(encoding="utf-8")
    hits: list[str] = []
    for line_no, line in enumerate(text.splitlines(), start=1):
        for pattern in PLACEHOLDER_PATTERNS:
            if pattern.search(line):
                hits.append(f"  L{line_no}: {line.strip()}")
                break
    if hits:
        print(
            "runbook-contacts: FAIL: support/incident runbook still has "
            f"{len(hits)} placeholder contact line(s). Populate real contacts "
            "in docs/SUPPORT_AND_INCIDENT_RUNBOOK.md before GO; do not invent them.",
            file=sys.stderr,
        )
        print("\n".join(hits), file=sys.stderr)
        return 1
    print("runbook-contacts: PASS (no placeholder contacts remain)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
