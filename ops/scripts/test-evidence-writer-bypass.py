#!/usr/bin/env python3
"""Guard: no evidence writer may emit an artifact without binding_status.

Detects the unbound write idioms that strip or omit producer-identity stamps
when drills and harnesses re-run:

  shell:  cp/mv/redirect into evidence/ or $EVIDENCE*
  python: write_text / json.dump / open(w|a) to an evidence destination
          without going through the bound writer helpers
  make:   stripe-simulate redirect into evidence/autonomous/

Bound write markers (any one is enough for a given write site only when the
unsafe idiom is absent):

  write_bound_evidence, emit_bound_json, write_bound_jsonl_sidecar,
  write-bound-evidence.py, merc_emit_bound_json, WriteBoundEvidenceJSON

Unsafe idioms are always violations, even if the same file also has a bound
call elsewhere (e.g. energy-authority path bound, JSONL append still open).

    python3 ops/scripts/test-evidence-writer-bypass.py
    python3 ops/scripts/test-evidence-writer-bypass.py --from-git HEAD --expect-fail
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

ALLOWLIST = {
    "ops/scripts/lib/evidence_binding.py",
    "ops/scripts/lib/write-bound-evidence.sh",
    "ops/scripts/write-bound-evidence.py",
    "ops/scripts/stamp-evidence-binding.py",
    "ops/scripts/validate-evidence-binding.py",
    "ops/scripts/test-evidence-writer-bypass.py",
    "src/control/receipt_identity.go",
    "src/control/receipt_identity_test.go",
    "src/control/evidence_binding_payload_guard_test.go",
}

BOUND_MARKERS = (
    "write_bound_evidence",
    "emit_bound_json",
    "write_bound_jsonl_sidecar",
    "write-bound-evidence.py",
    "merc_emit_bound_json",
    "WriteBoundEvidenceJSON",
)

# Shell: bytes land under evidence/ without the bound helper.
# Note: jq '...' evidence/foo.json >/dev/null is a READ (path is an input arg).
# We only match when the redirect *target* is evidence or an evidence variable.
#
# $receipt alone is not enough — many tests use receipt=$tmp/foo.json. Only
# flag $receipt / $RECEIPT when the file also assigns it under evidence/.
SHELL_UNSAFE = [
    (
        re.compile(
            r"""\bcp\s+\S+\s+["']?\$(?:EVIDENCE(?:_OUT|_DIR)?|CORRUPT_EVIDENCE_OUT)\b"""
        ),
        "cp into evidence destination variable",
    ),
    (
        re.compile(
            r"""\bmv\s+\S+\s+["']?\$(?:EVIDENCE(?:_OUT|_DIR)?|CORRUPT_EVIDENCE_OUT)\b"""
        ),
        "mv into evidence destination variable",
    ),
    (
        re.compile(
            r"""\b(?:cp|mv)\s+\S+\s+["']?[^"'\s]*evidence/[A-Za-z0-9_./@+-]+\.(?:json|jsonl|txt)"""
        ),
        "cp/mv into literal evidence/ path",
    ),
    (
        re.compile(
            r""">\s*["']?\$(?:EVIDENCE(?:_OUT|_DIR)?|CORRUPT_EVIDENCE_OUT)\b"""
        ),
        "shell redirect into evidence destination variable",
    ),
    (
        re.compile(
            r""">\s*["']?(?:\.\./)*evidence/[A-Za-z0-9_./@+-]+\.(?:json|jsonl|txt)(?:\.tmp)?"""
        ),
        "shell redirect into literal evidence/ path",
    ),
    (
        re.compile(
            r""">\s*["']?\$EVIDENCE_DIR/"""
        ),
        "shell redirect into $EVIDENCE_DIR/",
    ),
    (
        re.compile(
            r""">\s*["']?\$GC_EVIDENCE_(?:DIR|FILE)\b"""
        ),
        "shell redirect into $GC_EVIDENCE_*",
    ),
]

# $receipt assigned under evidence/ then written without bound helper.
RECEIPT_ASSIGN_EVIDENCE = re.compile(
    r"""\breceipt=["']?[^"'\n]*(?:evidence/|\$GC_EVIDENCE_DIR|\$EVIDENCE)"""
)
RECEIPT_WRITE = re.compile(
    r"""(?:\b(?:cp|mv)\s+\S+\s+["']?\$receipt\b|>\s*["']?\$receipt\b)"""
)

# Python: open/write to evidence without a bound helper in the file.
# (Once write_bound_jsonl_sidecar / emit_bound_json is present, these are OK.)
PY_UNSAFE_WHEN_UNBOUND = [
    (
        re.compile(r"""\.write_text\s*\("""),
        "Path.write_text without bound writer",
    ),
    (
        re.compile(r"""\bjson\.dump\s*\("""),
        "json.dump without bound writer",
    ),
    (
        re.compile(r"""\bwith\s+out\.open\(\s*["'][wa]"""),
        "out.open(w/a) without bound writer",
    ),
    (
        re.compile(r"""\bopen\(\s*args\.out\s*,\s*["']w"""),
        "open(args.out, w) without bound writer",
    ),
]

# File is an evidence producer if it names an evidence destination.
EVIDENCE_PRODUCER = re.compile(
    r"""
    (?:
        evidence/[A-Za-z0-9_./@+-]+\.(?:json|jsonl|txt)
      | \$(?:EVIDENCE(?:_OUT|_DIR)?|CORRUPT_EVIDENCE_OUT)\b
      | default=["']evidence/
      | --out\s+evidence/
      | evidence_path
    )
    """,
    re.VERBOSE,
)

SCAN_GLOBS = (
    "ops/scripts/**/*.sh",
    "ops/scripts/**/*.py",
    "ops/scripts/**/*.go",
    "Makefile",
)


def iter_scan_paths() -> list[Path]:
    out: list[Path] = []
    for pattern in SCAN_GLOBS:
        out.extend(ROOT.glob(pattern))
    seen: set[Path] = set()
    ordered: list[Path] = []
    for p in sorted(out, key=lambda x: x.as_posix()):
        if p in seen or not p.is_file():
            continue
        # Skip unit/gaming tests that only *read* committed evidence.
        rel = p.relative_to(ROOT).as_posix()
        if rel.startswith("ops/scripts/test-") and rel not in {
            # none of the test-* scripts are producers of evidence/
        }:
            # Still scan them for unsafe cp into evidence/ — rare but real.
            pass
        seen.add(p)
        ordered.append(p)
    return ordered


def rel_of(path: Path) -> str:
    return path.relative_to(ROOT).as_posix()


def read_text(path: Path, from_git: str | None) -> str | None:
    rel = rel_of(path)
    if from_git is None:
        try:
            return path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return None
    try:
        raw = subprocess.check_output(
            ["git", "-C", str(ROOT), "show", f"{from_git}:{rel}"],
            stderr=subprocess.DEVNULL,
        )
    except subprocess.CalledProcessError:
        return None
    return raw.decode("utf-8", errors="replace")


def uses_bound_writer(text: str) -> bool:
    return any(m in text for m in BOUND_MARKERS)


def line_no(text: str, pos: int) -> int:
    return text.count("\n", 0, pos) + 1


def known_non_evidence_json_handles(text: str) -> set[str]:
    """Return JSON file handles statically opened outside ``evidence/``.

    Reading committed evidence is not itself evidence production.  A report
    generator can therefore legitimately read a census from ``evidence/`` and
    write its report under ``ops/``.  Keep that distinction narrow: this only
    recognizes a literal, context-managed write handle whose path is visibly
    outside evidence.  Everything dynamic or evidence-addressed remains a
    fail-closed finding.
    """
    handles: set[str] = set()
    pattern = re.compile(
        r"""\bwith\s+open\(\s*[\"'](?P<path>[^\"']+)[\"']\s*,\s*[
            \"'](?:w|a)[\"']\s*\)\s+as\s+(?P<handle>[A-Za-z_]\w*)""",
        re.VERBOSE,
    )
    for match in pattern.finditer(text):
        path = match.group("path")
        if not path.startswith("evidence/"):
            handles.add(match.group("handle"))
    return handles


def json_dump_uses_known_non_evidence_handle(
    text: str, match: re.Match[str], handles: set[str]
) -> bool:
    """Return true only for a simple JSON dump to a proven ops-style handle."""
    line = text.splitlines()[line_no(text, match.start()) - 1]
    target = re.search(r"""\bjson\.dump\s*\([^,\n]+,\s*([A-Za-z_]\w*)""", line)
    return target is not None and target.group(1) in handles


def find_violations(rel: str, text: str) -> list[str]:
    hits: list[str] = []

    # Fixture/unit tests under ops/scripts/test-* write into $tmp, not the repo's
    # evidence/ tree. Still flag literal evidence/ destinations.
    is_test = rel.startswith("ops/scripts/test-")

    # 1) Unsafe shell/make idioms — always a violation.
    for cre, label in SHELL_UNSAFE:
        for m in cre.finditer(text):
            ln = line_no(text, m.start())
            snippet = text.splitlines()[ln - 1].strip()[:140]
            # test fixtures may cp within $tmp; only literal evidence/ paths matter.
            if is_test and "evidence/" not in snippet and "EVIDENCE" not in snippet:
                continue
            hits.append(f"L{ln}: {label}: {snippet}")

    # receipt=$GC_EVIDENCE_DIR/... or receipt=...evidence/... then > "$receipt"
    if RECEIPT_ASSIGN_EVIDENCE.search(text) and not uses_bound_writer(text):
        for m in RECEIPT_WRITE.finditer(text):
            ln = line_no(text, m.start())
            snippet = text.splitlines()[ln - 1].strip()[:140]
            hits.append(f"L{ln}: write to $receipt assigned under evidence/: {snippet}")

    # gc_atomic_json used to mv unstamped JSON; require bound path when present.
    if (
        re.search(r"""\bgc_atomic_json\b""", text)
        and "merc_emit_bound_json" not in text
        and "write-bound-evidence.py" not in text
        and rel.endswith("go-closure-common.sh")
    ):
        # Definition site must call the bound writer.
        if "mv -f" in text and "destination" in text:
            hits.append(
                "gc_atomic_json still mv's into evidence without merc_emit_bound_json"
            )

    # 2) Python producers that write without any bound helper.
    #    Skip test harnesses that only fabricate tmp fixtures.
    is_py = rel.endswith(".py")
    if (
        is_py
        and not is_test
        and EVIDENCE_PRODUCER.search(text)
        and not uses_bound_writer(text)
    ):
        non_evidence_handles = known_non_evidence_json_handles(text)
        for cre, label in PY_UNSAFE_WHEN_UNBOUND:
            for m in cre.finditer(text):
                if (
                    label == "json.dump without bound writer"
                    and json_dump_uses_known_non_evidence_handle(
                        text, m, non_evidence_handles
                    )
                ):
                    continue
                ln = line_no(text, m.start())
                snippet = text.splitlines()[ln - 1].strip()[:140]
                hits.append(f"L{ln}: {label}: {snippet}")
        for m in re.finditer(r"""temporary\.replace\(\s*evidence_path""", text):
            ln = line_no(text, m.start())
            hits.append(f"L{ln}: atomic replace into evidence_path without bound writer")

    # 3) Partial bypass: JSONL under evidence/ without write_bound_jsonl_sidecar.
    if is_py and not is_test and EVIDENCE_PRODUCER.search(text):
        if re.search(r"""out\.open\(\s*["'][wa]""", text) and (
            "write_bound_jsonl_sidecar" not in text
            and "write-bound-evidence.py" not in text
        ):
            if "default=" in text and "evidence/" in text and re.search(
                r"""\.(jsonl)""", text
            ):
                for m in re.finditer(r"""out\.open\(\s*["'][wa]""", text):
                    ln = line_no(text, m.start())
                    snippet = text.splitlines()[ln - 1].strip()[:140]
                    hits.append(
                        f"L{ln}: JSONL open(w/a) under evidence/ without "
                        f"write_bound_jsonl_sidecar: {snippet}"
                    )

    return hits


def run_self_tests() -> list[str]:
    """Keep the read-vs-write distinction narrow and mechanically covered."""
    failures: list[str] = []
    ops_reporter = '''\
with open("evidence/census/input.json") as source:
    payload = json.load(source)
with open("ops/report.json", "w") as report:
    json.dump(payload, report)
'''
    if find_violations("ops/scripts/ops-reporter.py", ops_reporter):
        failures.append("a known ops-only JSON handle was treated as evidence output")

    unbound_evidence_writer = '''\
with open("evidence/state/receipt.json", "w") as receipt:
    json.dump({}, receipt)
'''
    if not find_violations("ops/scripts/unbound-evidence-writer.py", unbound_evidence_writer):
        failures.append("an unbound literal evidence JSON writer was not detected")
    return failures


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--from-git",
        default=None,
        help="scan file contents at this git ref instead of the working tree",
    )
    ap.add_argument(
        "--expect-fail",
        action="store_true",
        help="exit 0 only when at least one bypass is found (prove pre-fix tree)",
    )
    args = ap.parse_args()

    self_test_failures = run_self_tests()
    if self_test_failures:
        print("test-evidence-writer-bypass: FAIL self-test", file=sys.stderr)
        for failure in self_test_failures:
            print(f"  - {failure}", file=sys.stderr)
        return 1

    violations: list[tuple[str, list[str]]] = []
    scanned = 0
    for path in iter_scan_paths():
        rel = rel_of(path)
        if rel in ALLOWLIST:
            continue
        text = read_text(path, args.from_git)
        if text is None:
            continue
        scanned += 1
        hits = find_violations(rel, text)
        if hits:
            violations.append((rel, hits))

    if scanned == 0:
        print("test-evidence-writer-bypass: FAIL: scanned zero files", file=sys.stderr)
        return 1

    if args.expect_fail:
        if violations:
            print(
                f"test-evidence-writer-bypass: expected FAIL confirmed "
                f"({len(violations)} bypass writers at "
                f"{args.from_git or 'working tree'}):"
            )
            for rel, hits in violations:
                print(f"  - {rel}")
                for h in hits[:4]:
                    print(f"      {h}")
            return 0
        print(
            "test-evidence-writer-bypass: FAIL: --expect-fail but no bypass found",
            file=sys.stderr,
        )
        return 1

    if violations:
        print(
            f"test-evidence-writer-bypass: FAIL: {len(violations)} writer(s) "
            f"can emit evidence without binding_status:",
            file=sys.stderr,
        )
        for rel, hits in violations:
            print(f"  {rel}", file=sys.stderr)
            for h in hits:
                print(f"    {h}", file=sys.stderr)
        print(
            "\nRoute each through ops/scripts/write-bound-evidence.py, "
            "lib.evidence_binding.emit_bound_json / write_bound_jsonl_sidecar, "
            "or merc_emit_bound_json.",
            file=sys.stderr,
        )
        return 1

    print(
        f"test-evidence-writer-bypass: PASS "
        f"(scanned {scanned} files; no unbound evidence writers)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
