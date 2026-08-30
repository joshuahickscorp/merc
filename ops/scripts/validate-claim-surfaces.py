#!/usr/bin/env python3
"""Fail-closed claim-surface gate for capability claims (paths, prices, models).

Restored after the 2026-07-19 deletion of proof/claims/claim-policy.json.
Asserts that capability claims on active surfaces resolve against the tree and
against the catalogue price the binary applies at boot.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
POLICY = ROOT / "evidence" / "proof" / "claims" / "claim-policy.json"

# Defaults matching src/control/pricing.go + src/control/payment.go (3% platform take).
TARGET_SUPPLIER_USD_HR = 2.0
DEFAULT_ELECTRICITY_USD_PER_KWH = 0.15
DEFAULT_SUPPLIER_SHARE = 0.97  # 1 - 3% platform take
WATTS_BY_CLASS = {
    "apple_silicon_base": 20.0,
    "apple_silicon_pro": 30.0,
    "apple_silicon_max": 45.0,
    "apple_silicon_ultra": 65.0,
    "cpu": 25.0,
}


# A repo-relative docs/*.md citation, in prose, a markdown link, or a JSON string.
DOC_PATH_RE = re.compile(r"(?<![A-Za-z0-9_./-])(docs/[A-Za-z0-9_./-]+\.md)(?![A-Za-z0-9_./-])")

# Text files git tracks, minus binaries. Scoped to the index deliberately: a
# filesystem walk also hits src/agent/target/ and .artifacts/ build output.
TEXT_SUFFIXES = (".md", ".json", ".py", ".sh", ".go", ".yml", ".yaml", ".html", ".mjs", ".js")

# Documents that were never absorbed anywhere and are cited from code comments
# and ops JSON. Enumerated as dangling in docs/archive/engineering/PROGRAMME.md's L6 contradiction
# ledger under "Three dangling references (do not create files)" — writing a
# file to satisfy a citation would be inventing a source. Exempt by name so the
# gate stays green on a known, recorded absence and red on a new one.
# Files whose citations are frozen at the moment they were written. Rewriting a
# path inside one either falsifies a record or changes a value the tree pins:
#   evidence/                      recorded receipts, name the tree as it was
#   src/control/runtime-authority.json content-addressed capability manifest; its
#                                  per-profile digests are pinned in
#                                  runtime_authority_v2_test.go, so editing a
#                                  doc string here shifts every profile digest
#                                  and forces agents to re-enrol
FROZEN_CITERS = ("evidence/", "src/control/runtime-authority.json")

KNOWN_DANGLING = frozenset({
    "docs/REBRAND.md",
    "docs/GPU_CAPABILITY.md",
    "docs/CREED_AND_PATH_TO_TEN.md",
    "docs/internal/CREED_AND_PATH_TO_TEN.md",
})


def tracked_text_files() -> list[str]:
    out = subprocess.run(
        ["git", "ls-files"], cwd=ROOT, capture_output=True, text=True, check=True
    ).stdout
    return [p for p in out.split("\n") if p.endswith(TEXT_SUFFIXES) and (ROOT / p).is_file()]


def fail(message: str) -> None:
    print(f"claim-surfaces: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def boot_reprice(model_id: str) -> float:
    """Mirror repriceFromSupplierEconomics for repricingBenchmarks entries."""
    pricing = (ROOT / "src/control" / "pricing.go").read_text(encoding="utf-8")
    # Parse the measuredThroughput block for the model.
    block_re = re.compile(
        r"ModelID:\s+\"" + re.escape(model_id) + r"\",\s*"
        r"JobType:\s+\"[^\"]+\",\s*"
        r"UnitsPerSec:\s+([0-9.]+),\s*"
        r"HWClass:\s+\"([^\"]+)\"",
        re.M,
    )
    match = block_re.search(pricing)
    if not match:
        fail(f"model {model_id} not found in src/control/pricing.go repricingBenchmarks")
    units_per_sec = float(match.group(1))
    hw_class = match.group(2)
    watts = WATTS_BY_CLASS.get(hw_class, 30.0)
    electricity_usd_hr = watts / 1000.0 * DEFAULT_ELECTRICITY_USD_PER_KWH
    units_per_hr = units_per_sec * 3600.0
    denom = units_per_hr / 1000.0 * DEFAULT_SUPPLIER_SHARE
    if denom <= 0:
        fail(f"non-positive reprice denominator for {model_id}")
    return (TARGET_SUPPLIER_USD_HR + electricity_usd_hr) / denom


def main() -> None:
    if not POLICY.is_file():
        fail(f"missing {POLICY.relative_to(ROOT)}")
    policy = json.loads(POLICY.read_text(encoding="utf-8"))
    errors: list[str] = []

    for relative in policy.get("active_surfaces", []):
        path = ROOT / relative
        if not path.is_file():
            errors.append(f"active surface missing: {relative}")

    checks = policy.get("capability_checks") or {}

    # macOS menu bar / Package.swift
    mac = checks.get("macos_menu_bar_requires_package_swift") or {}
    required = mac.get("required_path", "clients/macapp/MercAgent/Package.swift")
    has_package = (ROOT / required).is_file()
    for relative in mac.get("surfaces", ["clients/web/index.html"]):
        path = ROOT / relative
        if not path.is_file():
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        for pattern in mac.get("claim_patterns", []):
            regex = re.compile(pattern)
            for match in regex.finditer(text):
                if not has_package:
                    line = text.count("\n", 0, match.start()) + 1
                    errors.append(
                        f"{relative}:{line}: claims {match.group(0)!r} but "
                        f"{required} is absent"
                    )

    # Embeddings price vs boot reprice
    price_check = checks.get("embeddings_price_matches_boot_reprice") or {}
    surface = price_check.get("surface", "clients/web/index.html")
    model_id = price_check.get("model_id", "all-minilm-l6-v2")
    path = ROOT / surface
    if path.is_file():
        text = path.read_text(encoding="utf-8", errors="replace")
        # Monument block: embeddings ... $N.NNN
        price_re = re.compile(
            r'class="cap-top label">embeddings</span>\s*'
            r'<p class="fig"><span class="dollar">\$</span>'
            r'<span class="price">([0-9]+(?:\.[0-9]+)?)</span>',
            re.I,
        )
        match = price_re.search(text)
        if match:
            advertised = float(match.group(1))
            expected = boot_reprice(model_id)
            # Allow half-cent display rounding only if the advertised value is
            # within 5% of the boot reprice — seed 0.001 vs ~0.00029 must fail.
            if expected <= 0 or abs(advertised - expected) / expected > 0.05:
                line = text.count("\n", 0, match.start()) + 1
                errors.append(
                    f"{surface}:{line}: embeddings price ${advertised} does not match "
                    f"boot reprice ${expected:.8f} for {model_id} "
                    f"({price_check.get('source', 'src/control/pricing.go')})"
                )

    # Model ids mentioned as advertised workloads must exist in runtime authority.
    authority = json.loads((ROOT / "src/control" / "runtime-authority.json").read_text())
    catalog_ids = {m["id"] for m in authority.get("models", [])}
    for relative in policy.get("active_surfaces", []):
        path = ROOT / relative
        if not path.is_file() or not relative.endswith((".html", ".md")):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        for model_id in re.findall(r"\b(all-minilm-l6-v2|llama-3\.2-1b-instruct-q4)\b", text):
            if model_id not in catalog_ids:
                errors.append(f"{relative}: model id {model_id} not in runtime catalogue")

    # Cited doc paths must resolve. The L6 consolidation (68 markdown -> 19)
    # absorbed four operator docs and left every inbound reference pointing at
    # the deleted path, so the facet pack told an operator to open a 404 and
    # ops/governance-review-packets.json listed a missing file as required
    # evidence. The rename-residue audit reported RESIDUE=0 throughout: it
    # classifies the old *name*, and has nothing to say about a path that no
    # longer exists. This is the check that would have caught it.
    cited_docs = 0
    for relative in tracked_text_files():
        if relative.startswith("docs/archive/"):
            continue  # archived prose retains the paths true when it was written
        if relative.startswith(FROZEN_CITERS):
            continue
        text = (ROOT / relative).read_text(encoding="utf-8", errors="replace")
        historical = False
        for lineno, line in enumerate(text.split("\n"), 1):
            if "historical-doc-names:begin" in line:
                historical = True
                continue
            if "historical-doc-names:end" in line:
                historical = False
                continue
            if historical or "<!-- source:" in line:
                continue  # names a former file on purpose, not a live link
            for cited in DOC_PATH_RE.findall(line):
                cited_docs += 1
                if cited in KNOWN_DANGLING or (ROOT / cited).is_file():
                    continue
                errors.append(f"{relative}:{lineno}: cites {cited}, which does not exist")

    if errors:
        print("claim-surfaces: FAIL:", file=sys.stderr)
        for err in errors:
            print(f"  {err}", file=sys.stderr)
        raise SystemExit(1)

    print(
        f"claim-surfaces: PASS "
        f"({len(policy.get('active_surfaces', []))} surfaces; "
        f"capability path/price/model checks clean; "
        f"{cited_docs} cited doc paths resolve)"
    )


if __name__ == "__main__":
    main()
