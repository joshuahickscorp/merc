#!/usr/bin/env python3
"""Fail if markdown claims a longer clean soak than any retained receipt proves.

Reads committed soak receipts under evidence/, finds the maximum wall time among
receipts with control_restart_count == 0 and no OOM, then scans repo markdown
for claims that a longer soak passed.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
EVIDENCE = ROOT / "evidence"

# Claims that a soak of duration D completed/passed.
CLAIM_PATTERNS = [
    # "24-hour soak have passed" / "15 minute soak passed"
    re.compile(
        r"(?i)(?P<n>\d+)\s*[- ]?\s*(?P<unit>hour|hr|h|minute|min|m|second|sec|s)s?"
        r"\s+soak\b[^.\n]{0,80}\b(?P<verb>passed|pass|completed|succeeded)\b"
    ),
    re.compile(
        r"(?i)\b(?P<verb>passed|completed|succeeded)\b[^.\n]{0,80}"
        r"(?P<n>\d+)\s*[- ]?\s*(?P<unit>hour|hr|h|minute|min|m|second|sec|s)s?"
        r"\s+soak\b"
    ),
    # "the required 24-hour soak have passed"
    re.compile(
        r"(?i)required\s+(?P<n>\d+)\s*[- ]?\s*(?P<unit>hour|hr|h|minute|min|m|second|sec|s)s?"
        r"\s+soak\b[^.\n]{0,40}\b(?P<verb>passed|pass|have passed|has passed)\b"
    ),
]

SKIP_DIRS = {
    ".git",
    "agent/target",
    "node_modules",
    ".artifacts",
    "census",
}


def fail(message: str) -> None:
    print(f"soak-claims: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


# Words that turn a duration into a precondition rather than a result.  Scanned
# backwards from the match within the same sentence, so "until the 24-hour soak
# has passed" is a plan and "the 24-hour soak passed" is a claim.
CONDITIONAL_MARKERS = (
    "until", "before", "once", "pending", "awaiting", "requires", "required",
    "must ", "would ", "when the", "not yet", "remains", "blocked on",
)


def is_conditional(text: str, start: int) -> bool:
    sentence_start = max(
        text.rfind(".", 0, start),
        text.rfind("\n\n", 0, start),
        text.rfind(";", 0, start),
    )
    window = text[sentence_start + 1 : start].lower()
    return any(marker in window for marker in CONDITIONAL_MARKERS)


def unit_to_seconds(n: int, unit: str) -> int:
    u = unit.lower()
    if u in {"s", "sec", "second", "seconds"}:
        return n
    if u in {"m", "min", "minute", "minutes"}:
        return n * 60
    if u in {"h", "hr", "hour", "hours"}:
        return n * 3600
    fail(f"unknown duration unit {unit!r}")
    return 0


def receipt_wall_seconds(doc: dict) -> int | None:
    if "wall_seconds" in doc and isinstance(doc["wall_seconds"], (int, float)):
        return int(doc["wall_seconds"])
    duration = doc.get("duration")
    if isinstance(duration, dict):
        for key in ("actual_seconds", "requested_seconds", "wall_seconds"):
            if isinstance(duration.get(key), (int, float)):
                return int(duration[key])
    return None


def receipt_is_clean(doc: dict) -> bool:
    status = str(doc.get("status", "")).upper()
    if status not in {"PASS", "OK"}:
        return False
    bounds = doc.get("observed_bounds") if isinstance(doc.get("observed_bounds"), dict) else {}
    assertions = doc.get("assertions") if isinstance(doc.get("assertions"), dict) else {}

    restarts = bounds.get("control_restart_count")
    if isinstance(restarts, dict):
        restart_max = restarts.get("max", None)
    else:
        restart_max = restarts
    if restart_max is None and "control_never_restarted" in assertions:
        restart_ok = bool(assertions.get("control_never_restarted"))
    else:
        restart_ok = restart_max == 0

    oom = bounds.get("control_oom_samples")
    if oom is None and "control_never_oom_killed" in assertions:
        oom_ok = bool(assertions.get("control_never_oom_killed"))
    else:
        oom_ok = (oom or 0) == 0

    return bool(restart_ok and oom_ok)


def formal_go_closure_soak_is_clean(path: Path, doc: dict) -> bool:
    if (
        doc.get("schema_version") != 2
        or doc.get("kind") != "go_closure_soak"
        or not isinstance(doc.get("expected_commit"), str)
        or not isinstance(doc.get("control_image"), str)
    ):
        return False
    result = subprocess.run(
        [
            sys.executable,
            str(ROOT / "scripts" / "validate-go-closure-soak-receipt.py"),
            str(path),
            "--root",
            str(ROOT),
            "--commit",
            doc["expected_commit"],
            "--image",
            doc["control_image"],
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    return result.returncode == 0


def max_clean_soak_seconds() -> tuple[int, list[str]]:
    best = 0
    sources: list[str] = []
    if not EVIDENCE.is_dir():
        return 0, sources
    for path in sorted(EVIDENCE.rglob("*.json")):
        try:
            doc = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(doc, dict):
            continue
        kind = str(doc.get("kind", ""))
        name = path.name
        if "soak" not in kind.lower() and "soak" not in name.lower():
            continue
        wall = receipt_wall_seconds(doc)
        if wall is None:
            continue
        if kind == "go_closure_soak":
            if not formal_go_closure_soak_is_clean(path, doc):
                continue
        elif not receipt_is_clean(doc):
            continue
        rel = path.relative_to(ROOT).as_posix()
        if wall > best:
            best = wall
            sources = [f"{rel} ({wall}s)"]
        elif wall == best:
            sources.append(f"{rel} ({wall}s)")
    return best, sources


def iter_markdown() -> list[Path]:
    files: list[Path] = []
    for path in ROOT.rglob("*.md"):
        rel_parts = path.relative_to(ROOT).parts
        if any(part in SKIP_DIRS or part.startswith(".") for part in rel_parts[:-1]):
            # allow top-level and docs; skip hidden and excluded trees
            if rel_parts[0] in SKIP_DIRS or rel_parts[0].startswith("."):
                continue
        if path.is_file():
            files.append(path)
    return files


def main() -> None:
    max_secs, sources = max_clean_soak_seconds()
    if max_secs <= 0:
        fail("no clean soak receipt found under evidence/")

    violations: list[str] = []
    for path in iter_markdown():
        text = path.read_text(encoding="utf-8", errors="replace")
        rel = path.relative_to(ROOT).as_posix()
        for pattern in CLAIM_PATTERNS:
            for match in pattern.finditer(text):
                n = int(match.group("n"))
                unit = match.group("unit")
                claimed = unit_to_seconds(n, unit)
                if is_conditional(text, match.start()):
                    # "remains absent until the 24-hour soak has passed" states a
                    # precondition, not a result.  Flagging it trains people to
                    # disable the gate, which costs more than the sentence saves.
                    continue
                if claimed > max_secs:
                    line = text.count("\n", 0, match.start()) + 1
                    excerpt = match.group(0).replace("\n", " ")
                    violations.append(
                        f"{rel}:{line}: claims {claimed}s soak passed "
                        f"(max clean receipt {max_secs}s): {excerpt!r}"
                    )

    if violations:
        print("soak-claims: FAIL:", file=sys.stderr)
        for item in violations:
            print(f"  {item}", file=sys.stderr)
        print(
            f"  retained clean max: {max_secs}s from {', '.join(sources)}",
            file=sys.stderr,
        )
        raise SystemExit(1)

    print(
        f"soak-claims: PASS (max clean retained soak {max_secs}s; "
        f"sources={', '.join(sources)}; no longer passed-claim in markdown)"
    )


if __name__ == "__main__":
    main()
