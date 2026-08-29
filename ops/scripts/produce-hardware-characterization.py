#!/usr/bin/env python3
"""Write evidence/autonomous/hardware-characterization.json from merc-agent.

`make agent-characterize` / `cargo run --release -- characterize` prints a
`cx_agent_device_characterization` object to stdout and does not write the
evidence path. Nothing in ops/scripts/ or the Makefile previously captured that
output into a receipt. This wrapper is the file producer: it runs that
command, stamps the commit with receipt_binding.stamp, and writes via the
bound evidence path. It does not copy values from an older receipt.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.evidence_binding import EvidenceBindingError, emit_bound_json  # noqa: E402
from lib.receipt_binding import candidate_commit, stamp  # noqa: E402

OUT = ROOT / "evidence" / "autonomous" / "hardware-characterization.json"
PRODUCER = "ops/scripts/produce-hardware-characterization.py"
AGENT_MAIN = ROOT / "src/agent" / "src" / "main.rs"


def extract_json_object(stdout: str) -> dict:
    text = stdout.strip()
    start = text.find("{")
    end = text.rfind("}")
    if start < 0 or end <= start:
        raise SystemExit(
            "produce-hardware-characterization: no JSON object on characterize stdout"
        )
    try:
        doc = json.loads(text[start : end + 1])
    except json.JSONDecodeError as exc:
        raise SystemExit(
            f"produce-hardware-characterization: stdout is not JSON: {exc}"
        ) from exc
    if not isinstance(doc, dict):
        raise SystemExit(
            "produce-hardware-characterization: stdout JSON is not an object"
        )
    return doc


def main() -> int:
    if not AGENT_MAIN.is_file():
        print(
            "produce-hardware-characterization: src/agent/src/main.rs is not on disk; "
            "cannot re-run merc-agent characterize",
            file=sys.stderr,
        )
        return 2

    completed = subprocess.run(
        ["cargo", "run", "--release", "--", "characterize"],
        cwd=ROOT / "src/agent",
        stdout=subprocess.PIPE,
        stderr=None,
        text=True,
        check=False,
    )
    if completed.returncode != 0:
        print(
            f"produce-hardware-characterization: characterize exited {completed.returncode}",
            file=sys.stderr,
        )
        return completed.returncode or 1

    doc = extract_json_object(completed.stdout)
    if str(doc.get("kind", "")) != "cx_agent_device_characterization":
        print(
            f"produce-hardware-characterization: unexpected kind {doc.get('kind')!r}",
            file=sys.stderr,
        )
        return 1

    stamp(doc, candidate_commit(str(ROOT)), PRODUCER)
    try:
        emit_bound_json(
            OUT,
            doc,
            harness=PRODUCER,
            repo_root=ROOT,
            build_binary_path=Path(__file__).resolve(),
            exact_config="cargo run --release -- characterize",
            raw_samples="embedded benchmarks and device fields in receipt body",
            model_na="model revisions recorded in receipt body; weights not inlined",
            image_na="no container image in this measurement",
            corpus_na="no external corpus; retained models are loaded by the agent",
        )
    except EvidenceBindingError as exc:
        print(f"produce-hardware-characterization: REFUSED: {exc}", file=sys.stderr)
        return 2

    summary = {
        "path": str(OUT.relative_to(ROOT)),
        "status": doc.get("status"),
        "source_commit": doc.get("source_commit"),
        "produced_by": doc.get("produced_by"),
        "binding_status": doc.get("binding_status"),
        "runtime_authority_sha256": doc.get("runtime_authority_sha256"),
        "device_model": doc.get("device_model"),
    }
    print(json.dumps(summary, indent=2))
    return 0 if str(doc.get("status", "")).upper() == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
