#!/usr/bin/env python3
"""Machine verifier for the V2 final scorecard.

Does not trust the markdown. Loads ``artifacts/v2/final-scorecard.json`` and
re-checks every claim against receipt files on disk.

Exit non-zero on any unbacked claim; print what failed.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]

REQUIRED_FACET_FIELDS = (
    "id",
    "baseline",
    "final",
    "implementation_evidence",
    "runtime_evidence",
    "external_heldout_evidence",
    "failed_attempts",
    "limitations",
    "reproduction_command",
    "remaining_blocker",
)

REQUIRED_FACET_IDS = (
    "perception",
    "calibration",
    "depth",
    "point_clouds",
    "geometry_reconstruction",
    "hidden_geometry",
    "procedural_generation",
    "topology",
    "uv",
    "textures",
    "materials",
    "lighting",
    "reflections",
    "organic_geometry",
    "anatomy",
    "fur",
    "scene_composition",
    "camera_paths",
    "web_compilation",
    "lod",
    "streaming",
    "performance",
    "accessibility",
    "provenance",
    "security",
    "repair",
    "perceptual_quality",
    "generalization",
)

KNOWN_OPEN_REQUIRED_IDS = (
    "uv-packing-branching-forms",
    "parity-roughness-blindness",
    "colmap-dense-mvs-no-cuda",
    "no-authorized-real-animal-capture",
    "no-user-supplied-consumer-object-photographs",
    "mac-studio-owned-blend-absent",
    "flagship-film-not-art-directed",
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def collect_numbers(obj: Any, out: list[float | int]) -> None:
    if isinstance(obj, bool):
        return
    if isinstance(obj, int | float):
        out.append(obj)
    elif isinstance(obj, dict):
        for value in obj.values():
            collect_numbers(value, out)
    elif isinstance(obj, list):
        for value in obj:
            collect_numbers(value, out)


def number_in_payload(claim: Any, text: str, payload: Any | None) -> bool:
    """Return True if *claim* is present in the receipt text or parsed JSON."""
    if isinstance(claim, bool):
        token = "true" if claim else "false"
        return token in text or json.dumps(claim) in text

    if isinstance(claim, int) and not isinstance(claim, bool):
        if str(claim) in text or json.dumps(claim) in text:
            return True
        if payload is not None:
            numbers: list[float | int] = []
            collect_numbers(payload, numbers)
            return any(
                isinstance(number, int) and not isinstance(number, bool) and number == claim
                for number in numbers
            )
        return False

    if isinstance(claim, float):
        for form in (
            json.dumps(claim),
            format(claim, ".17g"),
            format(claim, ".15g"),
            format(claim, ".12g"),
            str(claim),
        ):
            if form in text:
                return True
        if payload is not None:
            numbers = []
            collect_numbers(payload, numbers)
            for number in numbers:
                if isinstance(number, bool):
                    continue
                if isinstance(number, (int, float)) and math.isclose(
                    float(number), float(claim), rel_tol=0.0, abs_tol=1e-9
                ):
                    return True
        return False

    return str(claim) in text


def load_receipt(path: Path) -> tuple[str, Any | None]:
    text = path.read_text(encoding="utf-8", errors="replace")
    payload: Any | None = None
    if path.suffix == ".json":
        try:
            payload = json.loads(text)
        except json.JSONDecodeError:
            payload = None
    return text, payload


def _display_path(path: Path, root: Path) -> str:
    try:
        return str(path.resolve().relative_to(root.resolve()))
    except ValueError:
        return str(path)


def verify_scorecard(scorecard_path: Path, root: Path) -> tuple[list[str], dict[str, Any]]:
    failures: list[str] = []
    details: dict[str, Any] = {
        "scorecard_path": _display_path(scorecard_path, root),
        "checked_receipts": [],
        "facets_checked": 0,
        "numeric_claims_checked": 0,
        "blockers_checked": 0,
    }

    if not scorecard_path.is_file():
        return [f"scorecard missing: {scorecard_path}"], details

    try:
        scorecard = json.loads(scorecard_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        return [f"scorecard is not valid JSON: {exc}"], details

    facets = scorecard.get("facets")
    if not isinstance(facets, list):
        failures.append("scorecard.facets must be a list")
        return failures, details

    facet_ids = [facet.get("id") for facet in facets if isinstance(facet, dict)]
    if len(facet_ids) != 28:
        failures.append(f"expected 28 facets, found {len(facet_ids)}")
    for required_id in REQUIRED_FACET_IDS:
        if required_id not in facet_ids:
            failures.append(f"missing required facet id: {required_id}")
    if len(set(facet_ids)) != len(facet_ids):
        failures.append("duplicate facet ids present")

    for facet in facets:
        if not isinstance(facet, dict):
            failures.append("facet entry is not an object")
            continue
        details["facets_checked"] += 1
        facet_id = facet.get("id", "<unknown>")
        for field in REQUIRED_FACET_FIELDS:
            if field not in facet:
                failures.append(f"{facet_id}: missing required field {field}")

        baseline = facet.get("baseline")
        final = facet.get("final")
        for label, value in (("baseline", baseline), ("final", final)):
            if not isinstance(value, int) or isinstance(value, bool):
                failures.append(f"{facet_id}: {label} must be an int")
            elif not 0 <= value <= 110:
                failures.append(f"{facet_id}: {label}={value} outside 0-110")

        if isinstance(final, int) and final >= 100:
            proof = facet.get("reference_class_proof")
            held_out = facet.get("held_out_targets") or facet.get("unseen_targets")
            if not proof:
                failures.append(
                    f"{facet_id}: score {final} >= 100 requires reference_class_proof"
                )
            if final >= 105 and (
                not isinstance(held_out, list) or len(held_out) < 3
            ):
                failures.append(
                    f"{facet_id}: score {final} >= 105 requires >= 3 held_out/unseen targets"
                )
            if final >= 110 and not facet.get("adversarial_full_runtime_repair"):
                failures.append(
                    f"{facet_id}: score {final} >= 110 requires "
                    "adversarial_full_runtime_repair proof"
                )

        impl = facet.get("implementation_evidence")
        if not isinstance(impl, list) or not impl:
            failures.append(f"{facet_id}: implementation_evidence must be a non-empty list")
        else:
            for rel in impl:
                path = root / str(rel)
                if not path.exists():
                    failures.append(f"{facet_id}: implementation path missing: {rel}")

        runtime = facet.get("runtime_evidence")
        if not isinstance(runtime, list) or not runtime:
            failures.append(f"{facet_id}: runtime_evidence must be a non-empty list")
        else:
            for entry in runtime:
                if not isinstance(entry, dict):
                    failures.append(f"{facet_id}: runtime_evidence entry is not an object")
                    continue
                rel = entry.get("path")
                expected_digest = entry.get("sha256")
                claims = entry.get("numeric_claims", [])
                if not rel or not expected_digest:
                    failures.append(
                        f"{facet_id}: runtime_evidence entry requires path and sha256"
                    )
                    continue
                path = root / str(rel)
                if not path.is_file():
                    failures.append(f"{facet_id}: runtime receipt missing: {rel}")
                    continue
                actual = sha256_file(path)
                details["checked_receipts"].append(
                    {"path": rel, "sha256": actual, "facet": facet_id}
                )
                if actual != expected_digest:
                    failures.append(
                        f"{facet_id}: digest mismatch for {rel}: "
                        f"expected {expected_digest}, got {actual}"
                    )
                text, payload = load_receipt(path)
                if not isinstance(claims, list):
                    failures.append(f"{facet_id}: numeric_claims must be a list for {rel}")
                    continue
                for claim in claims:
                    details["numeric_claims_checked"] += 1
                    if not number_in_payload(claim, text, payload):
                        failures.append(
                            f"{facet_id}: numeric claim {claim!r} not found in {rel}"
                        )

        for field in (
            "external_heldout_evidence",
            "limitations",
            "reproduction_command",
        ):
            value = facet.get(field)
            if value is None or value == "" or value == []:
                failures.append(f"{facet_id}: {field} must be non-empty")

        if not isinstance(facet.get("failed_attempts"), list):
            failures.append(f"{facet_id}: failed_attempts must be a list")

        blocker = facet.get("remaining_blocker")
        if blocker is not None:
            details["blockers_checked"] += 1
            if not isinstance(blocker, dict):
                failures.append(f"{facet_id}: remaining_blocker must be an object or null")
            else:
                requirement = blocker.get("requirement")
                if not isinstance(requirement, str) or not requirement.strip():
                    failures.append(
                        f"{facet_id}: remaining_blocker.requirement must be a non-empty string"
                    )
                if not blocker.get("id"):
                    failures.append(f"{facet_id}: remaining_blocker.id is required")

    known_open = scorecard.get("known_open_failures")
    if not isinstance(known_open, list) or len(known_open) < 7:
        failures.append(
            f"known_open_failures must list at least 7 entries, found "
            f"{0 if not isinstance(known_open, list) else len(known_open)}"
        )
    else:
        known_ids = {item.get("id") for item in known_open if isinstance(item, dict)}
        for required_id in KNOWN_OPEN_REQUIRED_IDS:
            if required_id not in known_ids:
                failures.append(f"known_open_failures missing required id: {required_id}")
        for item in known_open:
            if not isinstance(item, dict):
                continue
            for digest_entry in item.get("receipt_digests", []) or []:
                if not isinstance(digest_entry, dict):
                    continue
                rel = digest_entry.get("path")
                expected = digest_entry.get("sha256")
                if not rel or not expected:
                    continue
                path = root / str(rel)
                if not path.is_file():
                    failures.append(f"known_open receipt missing: {rel}")
                    continue
                actual = sha256_file(path)
                if actual != expected:
                    failures.append(
                        f"known_open digest mismatch for {rel}: "
                        f"expected {expected}, got {actual}"
                    )

    for blocker in scorecard.get("external_blockers", []) or []:
        if not isinstance(blocker, dict):
            failures.append("external_blockers entry is not an object")
            continue
        details["blockers_checked"] += 1
        if not isinstance(blocker.get("requirement"), str) or not blocker["requirement"].strip():
            failures.append(
                f"external_blocker {blocker.get('id')!r} missing exact requirement string"
            )

    summary = scorecard.get("summary") or {}
    if isinstance(summary, dict) and summary.get("facets_at_or_above_100", 0) not in (
        0,
        None,
    ):
        # Allow non-zero only if every such facet has proof (checked above).
        high = [
            f
            for f in facets
            if isinstance(f, dict)
            and isinstance(f.get("final"), int)
            and f["final"] >= 100
        ]
        if high and summary.get("facets_at_or_above_100") != len(high):
            failures.append("summary.facets_at_or_above_100 does not match facet finals")

    return failures, details


def write_receipt(
    receipt_path: Path,
    scorecard_path: Path,
    failures: list[str],
    details: dict[str, Any],
) -> None:
    receipt = {
        "schema": "v2.final-scorecard-receipt/1",
        "verified_at": datetime.now(UTC).isoformat(),
        "scorecard_path": details.get("scorecard_path"),
        "scorecard_sha256": sha256_file(scorecard_path) if scorecard_path.is_file() else None,
        "passed": not failures,
        "failure_count": len(failures),
        "failures": failures,
        "details": {
            "facets_checked": details.get("facets_checked"),
            "numeric_claims_checked": details.get("numeric_claims_checked"),
            "blockers_checked": details.get("blockers_checked"),
            "receipt_count": len(details.get("checked_receipts") or []),
        },
    }
    receipt_path.parent.mkdir(parents=True, exist_ok=True)
    receipt_path.write_text(
        json.dumps(receipt, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--scorecard",
        type=Path,
        default=ROOT / "artifacts" / "v2" / "final-scorecard.json",
        help="Path to final-scorecard.json",
    )
    parser.add_argument(
        "--receipt",
        type=Path,
        default=ROOT / "artifacts" / "v2" / "final-scorecard.receipt.json",
        help="Where to write the verifier receipt",
    )
    parser.add_argument(
        "--root",
        type=Path,
        default=ROOT,
        help="Repository root for resolving relative receipt paths",
    )
    args = parser.parse_args(argv)

    scorecard_path = args.scorecard if args.scorecard.is_absolute() else args.root / args.scorecard
    receipt_path = args.receipt if args.receipt.is_absolute() else args.root / args.receipt
    root = args.root.resolve()

    failures, details = verify_scorecard(scorecard_path, root)
    write_receipt(receipt_path, scorecard_path, failures, details)

    if failures:
        print(f"VERIFY FAIL: {len(failures)} issue(s)")
        for item in failures:
            print(f"  - {item}")
        print(f"receipt: {receipt_path}")
        return 1

    print("VERIFY PASS")
    print(f"  facets_checked: {details['facets_checked']}")
    print(f"  numeric_claims_checked: {details['numeric_claims_checked']}")
    print(f"  blockers_checked: {details['blockers_checked']}")
    print(f"  receipts_checked: {len(details.get('checked_receipts') or [])}")
    print(f"  receipt: {receipt_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
