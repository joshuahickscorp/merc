#!/usr/bin/env python3
"""Rebuild the registry and supply-chain receipts from a real publish run.

Neither receipt had a producer. Both attested to a July GHCR run and could not be
re-earned, because re-earning them needs an image published and signed by GitHub
Actions OIDC — which cannot be reproduced on a laptop, since keyless signing
needs the Actions identity.

So the evidence comes from the workflow itself: `gh run download` yields the
digest, the cosign signature and attestation verifications, the provenance and
its verification, the SBOM, and the Trivy scan, for both the candidate image and
the retained prior image. This reads those artifacts and records what they
actually show.

It emits PASS only when every verification in the bundle passed. Nothing here
decides that a signature is good; cosign already did, in CI, and this refuses to
restate a verdict the bundle does not contain.

Usage:
  python3 ops/scripts/produce-registry-receipts.py --bundle <dir> --run-url <url> \
      --built-commit <sha>
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.receipt_binding import candidate_commit, stamp  # noqa: E402

PRODUCER = "ops/scripts/produce-registry-receipts.py"
REGISTRY_OUT = ROOT / "evidence" / "autonomous" / "registry-verification.json"
SUPPLY_OUT = ROOT / "evidence" / "autonomous" / "supply-chain.json"


def _read(bundle: Path, name: str):
    return json.loads((bundle / name).read_text(encoding="utf-8"))


def _digest(bundle: Path, name: str) -> str:
    return (bundle / name).read_text(encoding="utf-8").strip()


def _critical_vulns(doc) -> int:
    """Count CRITICAL findings Trivy reported. The workflow already fails the
    build on any, so a nonzero here means the bundle disagrees with the run."""
    total = 0
    for result in (doc or {}).get("Results", []) or []:
        for vuln in result.get("Vulnerabilities", []) or []:
            if str(vuln.get("Severity", "")).upper() == "CRITICAL":
                total += 1
    return total


def _code_delta(built: str, candidate: str) -> list[str]:
    """Tracked files differing between the built commit and the candidate.

    The image is built from GITHUB_SHA, which is the commit that carried the
    candidate declaration, so it is normally one ops-only commit ahead. Anything
    other than ops/candidate.json here means the image does not contain the
    candidate's code and the receipt must not claim it does.
    """
    if built == candidate:
        return []
    out = subprocess.run(
        ["git", "-C", str(ROOT), "diff", "--name-only", candidate, built],
        capture_output=True,
        text=True,
    )
    return [line for line in out.stdout.splitlines() if line.strip()]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--bundle", required=True)
    ap.add_argument("--run-url", required=True)
    ap.add_argument("--built-commit", required=True)
    args = ap.parse_args()

    bundle = Path(args.bundle)
    if not bundle.is_dir():
        print(f"{PRODUCER}: no such bundle {bundle}", file=sys.stderr)
        return 2

    candidate = candidate_commit(str(ROOT))
    built = args.built_commit.strip().lower()
    delta = _code_delta(built, candidate)
    code_delta = [p for p in delta if p != "ops/candidate.json"]

    cand_img = _digest(bundle, "candidate-image.txt")
    prior_img = _digest(bundle, "prior-image.txt")
    cand_sig = _read(bundle, "candidate-signature-verification.json")
    cand_att = _read(bundle, "candidate-attestation-verification.json")
    cand_prov = _read(bundle, "candidate-provenance-verification.json")
    cand_vulns = _read(bundle, "candidate-vulnerabilities.json")
    prior_sig = _read(bundle, "prior-signature-verification.json")
    prior_vulns = _read(bundle, "prior-vulnerabilities.json")
    sbom = _read(bundle, "candidate-sbom.spdx.json")

    checks = {
        "candidate_digest_pinned": cand_img.startswith("ghcr.io/") and "@sha256:" in cand_img,
        "prior_digest_pinned": prior_img.startswith("ghcr.io/") and "@sha256:" in prior_img,
        "candidate_signature_verified": bool(cand_sig),
        "candidate_attestation_verified": bool(cand_att),
        "candidate_provenance_verified": bool(cand_prov),
        "prior_signature_verified": bool(prior_sig),
        "candidate_sbom_present": bool(sbom.get("packages") or sbom.get("SPDXID")),
        "candidate_no_critical_vulnerabilities": _critical_vulns(cand_vulns) == 0,
        "prior_no_critical_vulnerabilities": _critical_vulns(prior_vulns) == 0,
        "image_contains_candidate_code": not code_delta,
    }
    status = "PASS" if all(checks.values()) else "FAIL"

    shared = {
        "status": status,
        "checks": checks,
        "candidate": {
            "image": cand_img,
            "built_from_commit": built,
            "declared_candidate": candidate,
            "delta_to_candidate": delta,
            "delta_is_ops_only": not code_delta,
        },
        "prior": {"image": prior_img},
        "workflow_run": args.run_url,
        "secret_values_recorded": False,
    }

    registry = {
        "schema_version": 1,
        "kind": "registry_verification",
        "label": "REGISTRY VERIFICATION",
        "evidence_source": "GitHub Actions publish-candidate artifact bundle",
        **shared,
    }
    supply = {
        "schema_version": 1,
        "kind": "registry_supply_chain_verification",
        "label": "SUPPLY CHAIN",
        "sbom_format": "SPDX JSON",
        "sbom_packages": len(sbom.get("packages", []) or []),
        "scanner": "trivy (CRITICAL, ignore-unfixed)",
        **shared,
    }

    for doc, out in ((registry, REGISTRY_OUT), (supply, SUPPLY_OUT)):
        stamp(doc, candidate, PRODUCER)
        out.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
        print(f"wrote {out} status={status} source_commit={candidate}")

    if status != "PASS":
        failed = [k for k, v in checks.items() if not v]
        print(f"{PRODUCER}: checks failed: {', '.join(failed)}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
