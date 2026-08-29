#!/usr/bin/env python3
"""Assert every model in repricingBenchmarks has an explicit register row.

The check is expected to FAIL while the two externally licensed catalogue
models remain legally blocked even though their engineering price paths exist.
Do not "fix" that by editing those blocked rows — it is a legal question, not
an engineering one. Merc-owned built-in contracts may be explicitly cleared
as internal contracts, but that does not clear any future third-party model.
"""

from __future__ import annotations

import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PRICING = ROOT / "control" / "pricing.go"
REGISTER = ROOT / "docs" / "THIRD_PARTY_LICENSES.md"
GENERATED = ROOT / "docs" / "generated" / "license-inventory.json"


def tracked_text(rel: str) -> str:
    path = ROOT / rel
    if path.is_file():
        return path.read_text(encoding="utf-8")
    completed = subprocess.run(
        ["git", "show", f"HEAD:{rel}"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if completed.returncode != 0:
        fail(f"missing {rel} on disk and in HEAD")
    return completed.stdout


def tracked_sha256(rel: str) -> str:
    path = ROOT / rel
    if path.is_file():
        return hashlib.sha256(path.read_bytes()).hexdigest()
    completed = subprocess.run(
        ["git", "show", f"HEAD:{rel}"],
        cwd=ROOT,
        capture_output=True,
        check=False,
    )
    if completed.returncode != 0:
        fail(f"missing {rel} on disk and in HEAD")
    return hashlib.sha256(completed.stdout).hexdigest()

# Map catalogue model ids to register row labels in THIRD_PARTY_LICENSES.md.
MODEL_REGISTER_LABELS = {
    "all-minilm-l6-v2": "all-MiniLM-L6-v2",
    "llama-3.2-1b-instruct-q4": "Llama 3.2 1B Instruct GGUF",
    "ffmpeg-transcode-v1": "Merc fixed media transcode contract",
    "svg-scene-render-v1": "Merc fixed scene rendering contract",
}


def fail(message: str) -> None:
    print(f"license-register: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def repricing_model_ids() -> list[str]:
    text = tracked_text("control/pricing.go")
    # Collect ModelID strings inside repricingBenchmarks.
    block = re.search(
        r"var repricingBenchmarks\s*=\s*\[\]measuredThroughput\{(.*?)\n\}",
        text,
        re.S,
    )
    if not block:
        fail("could not locate repricingBenchmarks in control/pricing.go")
    return re.findall(r'ModelID:\s+"([^"]+)"', block.group(1))


def register_status(label: str, text: str) -> str | None:
    # Table rows: | Label | ... | **BLOCKED**: ... | or similar conclusion cell.
    for line in text.splitlines():
        if label not in line:
            continue
        if not line.strip().startswith("|"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if not cells:
            continue
        # First cell is component name.
        if label not in cells[0]:
            continue
        conclusion = cells[-1]
        if "BLOCKED" in conclusion.upper():
            return "BLOCKED"
        if "ALLOWED" in conclusion.upper() or "OK" in conclusion.upper() or "APPROVED" in conclusion.upper():
            return "NON_BLOCKED"
        # Any conclusion without BLOCKED counts as non-blocked only if clearly free.
        if conclusion:
            return "OTHER:" + conclusion[:80]
    return None


def validate_generated_inventory() -> None:
    """The software-graph inventory must exist and be bound to the lockfiles.

    This is independent of the catalogue-model BLOCKED rows. A missing
    generated inventory is an engineering fail. BLOCKED Llama/MiniLM rows
    remain a legal fail on purpose.
    """
    if not GENERATED.is_file():
        fail("missing docs/generated/license-inventory.json; run scripts/generate-license-inventory.py")
    try:
        payload = json.loads(GENERATED.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"unreadable generated inventory: {exc}")
    if payload.get("kind") != "merc_generated_license_inventory":
        fail("generated inventory kind mismatch")
    if payload.get("status") != "GENERATED_DRAFT_NOT_APPROVAL":
        fail("generated inventory must remain GENERATED_DRAFT_NOT_APPROVAL")
    inputs = payload.get("inputs") or {}
    for rel in (
        "control/go.mod",
        "control/go.sum",
        "agent/Cargo.lock",
        "clients/sdk/python/pyproject.toml",
        "clients/sdk/typescript/package.json",
        "clients/sdk/typescript/package-lock.json",
    ):
        recorded = (inputs.get(rel) or {}).get("sha256")
        observed = tracked_sha256(rel)
        if recorded != observed:
            fail(
                f"generated inventory stale for {rel}: "
                f"recorded={recorded} observed={observed}"
            )
    inventory_md = ROOT / "docs" / "LICENSE_INVENTORY.md"
    if not inventory_md.is_file() or "GENERATED" not in inventory_md.read_text(encoding="utf-8"):
        fail("docs/LICENSE_INVENTORY.md missing or not marked GENERATED")


def main() -> None:
    validate_generated_inventory()
    # pricing.go may be absent from a sparse checkout; read it from HEAD.
    if not REGISTER.is_file():
        fail("missing docs/THIRD_PARTY_LICENSES.md")

    register_text = REGISTER.read_text(encoding="utf-8")
    models = repricing_model_ids()
    if not models:
        fail("repricingBenchmarks declares no models")

    errors: list[str] = []
    for model_id in models:
        label = MODEL_REGISTER_LABELS.get(model_id)
        if not label:
            errors.append(f"{model_id}: no register label mapping configured")
            continue
        status = register_status(label, register_text)
        if status is None:
            errors.append(f"{model_id}: no register row matching {label!r}")
        elif status == "BLOCKED":
            errors.append(
                f"{model_id}: priced in repricingBenchmarks but register row "
                f"{label!r} is BLOCKED"
            )
        elif status.startswith("OTHER:"):
            # Ambiguous conclusions are not accepted as non-BLOCKED clearance.
            errors.append(
                f"{model_id}: register row {label!r} has no explicit non-BLOCKED "
                f"clearance ({status[6:]})"
            )

    if errors:
        print("license-register: FAIL:", file=sys.stderr)
        for err in errors:
            print(f"  {err}", file=sys.stderr)
        print(
            "  (expected until legal clears the register; do not edit BLOCKED rows "
            "to silence this check)",
            file=sys.stderr,
        )
        raise SystemExit(1)

    print(
        f"license-register: PASS ({len(models)} repriced models have non-BLOCKED "
        f"register rows)"
    )


if __name__ == "__main__":
    main()
