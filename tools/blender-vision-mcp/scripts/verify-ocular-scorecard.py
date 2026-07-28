#!/usr/bin/env python3
"""Machine verifier for the V2.1 ocular final scorecard.

Does not trust the markdown. Loads ``artifacts/ocular/final-scorecard.json`` and
re-checks every claim against receipt files on disk.

Exit non-zero on any unbacked claim; print what failed.

Same shape as ``scripts/verify-final-scorecard.py`` (V2, 28 facets) but for the
Bible §27 ocular facet set (35 facets). Scoring ceilings:

- no 100 without ``reference_class_proof``
- no 105 without >= 3 held-out/unseen targets
- no 110 without ``adversarial_full_runtime_repair``
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

# Bible §27 — Score at minimum (order preserved).
REQUIRED_FACET_IDS = (
    "sensor_calibration",
    "live_streaming",
    "foveation",
    "attention",
    "segmentation",
    "tracking",
    "reidentification",
    "object_permanence",
    "dense_features",
    "depth",
    "camera",
    "geometry",
    "temporal_prediction",
    "world_memory",
    "active_perception",
    "next_best_view_efficiency",
    "material_decomposition",
    "lighting_separation",
    "reflection_transparency",
    "hard_surface",
    "soft_object",
    "organic",
    "hair_fur",
    "browser_perception",
    "source_to_pixel",
    "cinematic_composition",
    "aesthetic_criticism",
    "real_time_latency",
    "physical_run_authority",
    "leakage_resistance",
    "metric_sensitivity",
    "editability",
    "web_delivery",
    "uncertainty",
    "generalization",
)

# Deliberate open failures / external blockers that must appear on the ledger.
KNOWN_OPEN_REQUIRED_IDS = (
    "detection-precision-low",
    "tracking-suite-fails-three-gates",
    "permanence-one-of-four",
    "surprise-is-noise",
    "retina-confounder-fires",
    "beats-eight-of-nine-empty",
    "no-physical-webcam",
    "deliberate-failing-tests-8",
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
        "unknown_or_blocked_facets": [],
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
    if len(facet_ids) != len(REQUIRED_FACET_IDS):
        failures.append(
            f"expected {len(REQUIRED_FACET_IDS)} facets, found {len(facet_ids)}"
        )
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

        score_state = facet.get("score_state", "scored")
        if score_state not in ("scored", "unknown", "blocked"):
            failures.append(
                f"{facet_id}: score_state must be scored|unknown|blocked, got {score_state!r}"
            )
        if score_state in ("unknown", "blocked"):
            details["unknown_or_blocked_facets"].append(
                {"id": facet_id, "score_state": score_state}
            )

        baseline = facet.get("baseline")
        final = facet.get("final")
        for label, value in (("baseline", baseline), ("final", final)):
            if score_state in ("unknown", "blocked") and value is None:
                continue
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
        if score_state == "unknown":
            # Unknown facets may have empty runtime evidence; if present, still check digests.
            if runtime is None:
                runtime = []
            if not isinstance(runtime, list):
                failures.append(f"{facet_id}: runtime_evidence must be a list")
                runtime = []
        elif not isinstance(runtime, list) or not runtime:
            failures.append(f"{facet_id}: runtime_evidence must be a non-empty list")
            runtime = []

        if isinstance(runtime, list):
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
        if score_state in ("unknown", "blocked") and blocker is None:
            failures.append(
                f"{facet_id}: score_state={score_state} requires remaining_blocker"
            )
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
    if not isinstance(known_open, list) or len(known_open) < len(KNOWN_OPEN_REQUIRED_IDS):
        failures.append(
            f"known_open_failures must list at least {len(KNOWN_OPEN_REQUIRED_IDS)} "
            f"entries, found "
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
        high = [
            f
            for f in facets
            if isinstance(f, dict)
            and isinstance(f.get("final"), int)
            and f["final"] >= 100
        ]
        if high and summary.get("facets_at_or_above_100") != len(high):
            failures.append("summary.facets_at_or_above_100 does not match facet finals")

    quarantine = scorecard.get("quarantine")
    if not isinstance(quarantine, dict) or quarantine.get("state") != "EXPERIMENTAL":
        failures.append(
            "scorecard.quarantine.state must be EXPERIMENTAL "
            "(ocular is not a launch document)"
        )

    return failures, details


def write_receipt(
    receipt_path: Path,
    scorecard_path: Path,
    failures: list[str],
    details: dict[str, Any],
) -> None:
    receipt = {
        "schema": "ocular.final-scorecard-receipt/1",
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
            "unknown_or_blocked_facets": details.get("unknown_or_blocked_facets"),
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
        default=ROOT / "artifacts" / "ocular" / "final-scorecard.json",
        help="Path to ocular final-scorecard.json",
    )
    parser.add_argument(
        "--receipt",
        type=Path,
        default=ROOT / "artifacts" / "ocular" / "final-scorecard.receipt.json",
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
    print(
        f"  unknown_or_blocked: "
        f"{len(details.get('unknown_or_blocked_facets') or [])}"
    )
    print(f"  receipt: {receipt_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
