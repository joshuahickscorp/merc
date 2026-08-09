#!/usr/bin/env python3
"""Exercise manifest completeness, timing ingestion, and duplicate refusal."""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
TOOL = ROOT / "scripts" / "mutation-manifest.py"
MANIFEST = ROOT / "scripts" / "mutation-manifest.json"


def run(*args: str, expect: int = 0) -> subprocess.CompletedProcess[str]:
    result = subprocess.run([sys.executable, str(TOOL), "--root", str(ROOT), *args], text=True, capture_output=True, check=False)
    if result.returncode != expect:
        raise SystemExit(f"manifest command failed: {args}\nstdout={result.stdout}\nstderr={result.stderr}")
    return result


def main() -> int:
    run("--validate")
    payload = json.loads(MANIFEST.read_text(encoding="utf-8"))
    mutations = payload.get("mutations", [])
    if len(mutations) != 84 or [item["id"] for item in mutations] != list(range(1, 85)):
        raise SystemExit("manifest does not contain exactly the 84 declared mutations")
    if any(not item["required_invariant_contracts"] for item in mutations):
        raise SystemExit("manifest has a mutation without invariant contracts")

    candidate = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    with tempfile.TemporaryDirectory() as temporary:
        temporary_path = Path(temporary)
        copied = temporary_path / "manifest.json"
        shutil.copyfile(MANIFEST, copied)
        timing = temporary_path / "timing.jsonl"
        records = []
        for item in mutations:
            records.append({
                "case_id": item["id"],
                "source": item["source_target"].removeprefix("control/"),
                "description": item["description"],
                "candidate": candidate,
                "result": "caught",
                "pathway": "PURE" if item["id"] % 2 else "DB",
                "duration_seconds": float(item["id"]),
            })
        timing.write_text("\n".join(json.dumps(record) for record in records) + "\n", encoding="utf-8")
        run("--manifest", str(copied), "--ingest", str(timing), "--commit", candidate, "--require-complete", "--write")
        updated = json.loads(copied.read_text(encoding="utf-8"))
        if any(item["historical"]["p90_seconds"] is None or item["last_validated_commit"] != candidate for item in updated["mutations"]):
            raise SystemExit("manifest did not persist complete timing statistics")
        duplicate = temporary_path / "duplicate.jsonl"
        duplicate.write_text(timing.read_text(encoding="utf-8") + timing.read_text(encoding="utf-8"), encoding="utf-8")
        result = subprocess.run(
            [sys.executable, str(TOOL), "--root", str(ROOT), "--manifest", str(copied), "--ingest", str(duplicate), "--require-complete", "--write"],
            text=True, capture_output=True, check=False,
        )
        if result.returncode == 0:
            raise SystemExit("manifest accepted duplicate timing records")
    print("test-mutation-manifest: PASS 84 exact mutations and timing guards")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
