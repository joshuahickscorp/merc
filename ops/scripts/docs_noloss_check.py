#!/usr/bin/env python3
"""No-loss checker for documentation merges (PLAN_300K L6 / REFACTOR_PLAN §6.3).

Extracts a multiset of atoms from source docs and from the merged set:
  - ATX headings (normalized whitespace)
  - GFM table rows (raw line text)
  - Absolute URLs and repo-relative path tokens
  - Fenced code blocks (fence body)
  - Shell-like command lines (lines starting with `$ `)
  - Numeric literals with units (t/s, LOC, %, plain numbers)

Applies an explicit de-duplication ledger for justified collapses. Exits
non-zero when any source atom is missing from merged ∪ ledger with a count
deficit. Stdlib only.

Usage:
  python3 ops/scripts/docs_noloss_check.py \\
    --sources a.md b.md ... \\
    --merged merged1.md merged2.md ... \\
    [--dedupe-ledger docs/_noloss_dedupe_ledger.json]
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from collections import Counter
from pathlib import Path
from typing import Iterable

ROOT = Path(__file__).resolve().parents[2]

HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$")
TABLE_ROW_RE = re.compile(r"^\|.+\|\s*$")
URL_RE = re.compile(r"https?://[^\s\)\]\>\"']+")
# Repo-ish path tokens (docs/..., src/control/..., evidence/..., ops/scripts/..., ops/...)
PATH_RE = re.compile(
    r"(?<![A-Za-z0-9_./-])((?:docs|control|evidence|scripts|ops|agent|clients|"
    r"web|pricing|monitoring)/[A-Za-z0-9_./@+-]+\.[A-Za-z0-9]+"
    r"|[A-Za-z0-9_.-]+\.md)(?![A-Za-z0-9_./-])"
)
COMMAND_RE = re.compile(r"^\$\s+(.+)$")
# Numbers with optional units; include t/s and LOC explicitly per plan.
NUMBER_RE = re.compile(
    r"(?<![A-Za-z_])("
    r"[-+]?\d[\d,]*(?:\.\d+)?%?"
    r"|\d+(?:\.\d+)?\s*t/s"
    r"|\d[\d,]*\s*LOC"
    r")(?![A-Za-z_])"
)
FENCE_RE = re.compile(r"^```")


def norm_ws(s: str) -> str:
    return re.sub(r"\s+", " ", s.strip())


def extract_atoms(text: str) -> Counter[str]:
    """Return multiset of atom keys of the form 'class:value'."""
    atoms: Counter[str] = Counter()
    lines = text.splitlines()
    i = 0
    in_fence = False
    fence_lang = ""
    fence_body: list[str] = []

    def flush_fence() -> None:
        nonlocal fence_body, fence_lang
        body = "\n".join(fence_body).rstrip("\n")
        atoms[f"fence:{fence_lang}\n{body}"] += 1
        fence_body = []
        fence_lang = ""

    while i < len(lines):
        line = lines[i]
        if FENCE_RE.match(line.strip() if line.strip().startswith("```") else line):
            # fence open/close on lines starting with ```
            stripped = line.lstrip()
            if stripped.startswith("```"):
                if not in_fence:
                    in_fence = True
                    fence_lang = stripped[3:].strip()
                    fence_body = []
                else:
                    in_fence = False
                    flush_fence()
                i += 1
                continue
        if in_fence:
            fence_body.append(line)
            i += 1
            continue

        m = HEADING_RE.match(line)
        if m:
            atoms[f"heading:{m.group(1)} {norm_ws(m.group(2))}"] += 1
            i += 1
            continue

        if TABLE_ROW_RE.match(line):
            # skip pure separator rows
            if not re.match(r"^\|[\s:|-]+\|\s*$", line):
                atoms[f"table:{norm_ws(line)}"] += 1
            i += 1
            continue

        cm = COMMAND_RE.match(line.strip())
        if cm:
            atoms[f"command:{norm_ws(cm.group(1))}"] += 1

        for url in URL_RE.findall(line):
            atoms[f"url:{url.rstrip('.,;:)')}"] += 1

        for path in PATH_RE.findall(line):
            atoms[f"path:{path}"] += 1

        for num in NUMBER_RE.findall(line):
            atoms[f"number:{norm_ws(num)}"] += 1

        i += 1

    if in_fence and fence_body:
        flush_fence()
    return atoms


def load_files(paths: Iterable[str | Path]) -> Counter[str]:
    total: Counter[str] = Counter()
    for p in paths:
        path = Path(p)
        if not path.is_file():
            raise SystemExit(f"docs_noloss_check: missing file {path}")
        text = path.read_text(encoding="utf-8", errors="replace")
        total.update(extract_atoms(text))
    return total


def load_dedupe_ledger(path: Path | None) -> Counter[str]:
    """Ledger entries subtract from the required source multiset.

    JSON shape:
      {
        "entries": [
          {"atom": "heading:## Foo", "count": 1, "reason": "...", "sources": ["a.md","b.md"]}
        ]
      }
    Or a flat map {atom: count}.
    """
    if path is None or not path.is_file():
        return Counter()
    data = json.loads(path.read_text(encoding="utf-8"))
    out: Counter[str] = Counter()
    if isinstance(data, dict) and "entries" in data:
        for ent in data["entries"]:
            atom = ent["atom"]
            count = int(ent.get("count", 1))
            if not str(ent.get("reason") or "").strip():
                raise SystemExit(
                    f"docs_noloss_check: dedupe entry for {atom!r} lacks reason"
                )
            out[atom] += count
    elif isinstance(data, dict):
        for atom, count in data.items():
            if atom.startswith("_"):
                continue
            out[str(atom)] += int(count)
    else:
        raise SystemExit("docs_noloss_check: ledger must be a JSON object")
    return out


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--sources",
        nargs="+",
        default=[],
        help="Source markdown files (pre-merge content)",
    )
    ap.add_argument(
        "--sources-list",
        type=Path,
        default=None,
        help="File with one source path per line (# comments ok)",
    )
    ap.add_argument(
        "--merged",
        nargs="+",
        required=True,
        help="Merged markdown files to check against sources",
    )
    ap.add_argument(
        "--dedupe-ledger",
        type=Path,
        default=None,
        help="JSON ledger of justified de-duplications",
    )
    ap.add_argument(
        "--json-out",
        type=Path,
        default=None,
        help="Optional path to write full deficit/surplus report as JSON",
    )
    args = ap.parse_args(argv)

    sources: list[str] = list(args.sources)
    if args.sources_list:
        for line in args.sources_list.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            sources.append(line)
    if not sources:
        print("docs_noloss_check: no sources provided", file=sys.stderr)
        return 2

    # Resolve relative to repo root when not absolute.
    def resolve(p: str) -> Path:
        path = Path(p)
        return path if path.is_absolute() else ROOT / path

    source_paths = [resolve(s) for s in sources]
    merged_paths = [resolve(m) for m in args.merged]
    ledger_path = resolve(str(args.dedupe_ledger)) if args.dedupe_ledger else None

    S = load_files(source_paths)
    M = load_files(merged_paths)
    D = load_dedupe_ledger(ledger_path)

    # Available pool = merged + justified dedupe credits
    available = M + D
    deficits: list[tuple[str, int, int, int]] = []
    for atom, need in sorted(S.items()):
        have = available.get(atom, 0)
        if have < need:
            deficits.append((atom, need, have, need - have))

    surpluses = []
    for atom, have in sorted(M.items()):
        need = S.get(atom, 0)
        if have > need:
            surpluses.append((atom, have - need))

    report = {
        "sources": [str(p.relative_to(ROOT)) if p.is_relative_to(ROOT) else str(p) for p in source_paths],
        "merged": [str(p.relative_to(ROOT)) if p.is_relative_to(ROOT) else str(p) for p in merged_paths],
        "source_atoms": sum(S.values()),
        "merged_atoms": sum(M.values()),
        "dedupe_credits": sum(D.values()),
        "deficits": [
            {"atom": a, "need": n, "have": h, "missing": m} for a, n, h, m in deficits
        ],
        "surplus_count": len(surpluses),
    }
    if args.json_out:
        args.json_out.parent.mkdir(parents=True, exist_ok=True)
        args.json_out.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")

    print(
        f"docs_noloss_check: sources={len(source_paths)} merged={len(merged_paths)} "
        f"source_atoms={report['source_atoms']} merged_atoms={report['merged_atoms']} "
        f"dedupe_credits={report['dedupe_credits']}"
    )
    if deficits:
        print(f"docs_noloss_check: FAIL ({len(deficits)} deficit atoms)", file=sys.stderr)
        for a, n, h, m in deficits[:50]:
            preview = a if len(a) < 120 else a[:117] + "..."
            print(f"  - missing {m}× (need {n}, have {h}): {preview}", file=sys.stderr)
        if len(deficits) > 50:
            print(f"  ... and {len(deficits) - 50} more", file=sys.stderr)
        return 1

    print("docs_noloss_check: PASS (no atom loss)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
