#!/usr/bin/env python3
"""CLI front-end for the single bound-evidence write path.

Used by standalone harnesses (gateway-parity-v2.go, gateway-concurrency-sweep.go)
that cannot import control/receipt_identity.go.

  python3 scripts/write-bound-evidence.py \\
      --out evidence/perf/example.json \\
      --harness 'scripts/foo.go' \\
      --payload-file /tmp/payload.json \\
      [--authority-id ID] \\
      [--model-digest HEX | --model-na REASON] \\
      [--image-na REASON] ...

Payload is a JSON object. producer_identity and binding_status are set by the
writer. Exit 2 on refusal.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from lib.evidence_binding import (  # noqa: E402
    EvidenceBindingError,
    default_bound_identity,
    slot_na,
    slot_value,
    write_bound_evidence,
)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", required=True)
    ap.add_argument("--harness", required=True, help="harness_revision value")
    ap.add_argument("--payload-file", required=True)
    ap.add_argument("--authority-id", default="")
    ap.add_argument("--exact-config", default="embedded in receipt body")
    ap.add_argument("--raw-samples", default="embedded in receipt body")
    ap.add_argument("--model-digest", default="")
    ap.add_argument("--model-na", default="no model weights in this measurement")
    ap.add_argument("--image-digest", default="")
    ap.add_argument("--image-na", default="no container image in this measurement")
    ap.add_argument("--corpus-digest", default="")
    ap.add_argument("--corpus-na", default="no external corpus in this measurement")
    ap.add_argument(
        "--build-binary",
        default="",
        help="file whose sha256 becomes build_digest (default: this CLI script)",
    )
    args = ap.parse_args()

    payload_path = Path(args.payload_file)
    try:
        payload = json.loads(payload_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(f"write-bound-evidence: bad payload: {exc}", file=sys.stderr)
        return 2
    if not isinstance(payload, dict):
        print("write-bound-evidence: payload must be a JSON object", file=sys.stderr)
        return 2

    build_path = Path(args.build_binary) if args.build_binary else Path(__file__).resolve()
    try:
        identity = default_bound_identity(
            ROOT,
            harness_revision=args.harness,
            build_binary_path=build_path,
            exact_config=args.exact_config,
            raw_samples=args.raw_samples,
            model_na=args.model_na,
            image_na=args.image_na,
            corpus_na=args.corpus_na,
        )
        if args.model_digest:
            identity["model_artifact_digest"] = slot_value(args.model_digest)
        if args.image_digest:
            identity["image_digest"] = slot_value(args.image_digest)
        if args.corpus_digest:
            identity["corpus_digest"] = slot_value(args.corpus_digest)
        # Ensure NA slots stay valid when digests not provided
        if not args.model_digest and not identity["model_artifact_digest"].get("na"):
            identity["model_artifact_digest"] = slot_na(args.model_na)
        write_bound_evidence(
            path=args.out,
            payload=payload,
            identity=identity,
            repo_root=ROOT,
            build_binary_path=build_path,
            authority_id=args.authority_id,
        )
    except EvidenceBindingError as exc:
        print(f"write-bound-evidence: REFUSED: {exc}", file=sys.stderr)
        return 2
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
