#!/usr/bin/env python3
"""CI gate for evidence/perf/gateway-parity.json (and v2 receipts).

Validates the authoritative v2 harness receipt shape produced by
control/gateway_parity_harness.go (schema_version=2, gate_version=
gateway-parity-gate-v4). The withdrawn v1 shape (scripts/gateway-parity.py)
is refused as parity evidence — institutional green must not mean the old
harness's verdict.

Fails when the parity receipt is absent, undated, older than the revalidation
window (aligned with control/runtime_cell_performance.go
benchmarkRevalidationWindow = 180 days), carries a non-empty refusals list,
or claims gate_passed/comparable without Merc-bound body identity and the
required concurrency ladder {1,8,32}.

A receipt carrying validity != "VALID" is WITHDRAWN. That is a self-consistent
state, not a broken one -- the honest answer to "is there valid parity
evidence" is no -- so it reports NO VALID PARITY EVIDENCE and exits 0, printing
every recorded reason. None of the ordinary field checks run on it.

A withdrawal with an empty superseded_reason IS a failure.

schema_version != 2 (including the historical v1 file) is treated as withdrawn
with an automatic reason when validity is unset, so the committed v1 receipt
cannot greenwash a gate that only the v2 harness may pass.

    python3 scripts/test-gateway-parity-receipt.py
    python3 scripts/test-gateway-parity-receipt.py --self-test-only

No live engine. No network.
"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_RECEIPT = ROOT / "evidence" / "perf" / "gateway-parity.json"

# Keep aligned with control/runtime_cell_performance.go:benchmarkRevalidationWindow.
REVALIDATION_WINDOW = timedelta(days=180)

REQUIRED_LADDER = (1, 8, 32)
GATE_VERSION = "gateway-parity-gate-v4"
SCHEMA_VERSION = 2

FAILURES: list[str] = []
# Receipts explicitly withdrawn, with their recorded reasons. Not failures:
# an honest "there is no valid parity evidence" is a passing state.
WITHDRAWN: list[tuple] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)


def check(cond: bool, msg: str) -> None:
    if not cond:
        fail(msg)


def parse_measured_at(value: object) -> datetime | None:
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    try:
        if text.endswith("Z"):
            return datetime.fromisoformat(text[:-1] + "+00:00")
        return datetime.fromisoformat(text)
    except ValueError:
        return None


def base_receipt(**overrides: Any) -> dict[str, Any]:
    """Minimal v2 receipt that would pass structural checks if complete."""
    digest = "b" * 64
    receipt: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "kind": "gateway_parity",
        "harness": "merc-gateway-parity-v2",
        "gate_version": GATE_VERSION,
        "measured_at": "2026-07-28T11:10:09Z",
        "merc_source_commit": "a" * 40,
        "evidence_class": "PARITY_EVIDENCE",
        "comparable": True,
        "gate_passed": True,
        "refusals": [],
        "budget": {
            "ttft_shift_q95_ms": 15.0,
            "throughput_loss_fraction": 0.05,
            "require_measured_at_every_level": True,
            "primary_metric": "ttft_shift_q95_ms",
            "basis": "test",
        },
        "claimed_concurrency_levels": list(REQUIRED_LADDER),
        "body_identity": {
            "proven": True,
            "bodies_equal": True,
            "method": "X-Merc-Upstream-Body-SHA256 + X-Merc-Contract-ID on every OK merc sample",
            "harness_body_sha256": digest,
            "detail": "proven",
            "ok_samples_examined": 20,
            "contract_ids_observed": 20,
            "bare_sha_standin_refused": False,
        },
        "gate": {
            "version": GATE_VERSION,
            "gate_passed": True,
            "verdict": "PASS",
            "primary_metric": "ttft_shift_q95_ms",
            "claimed_concurrency_levels": list(REQUIRED_LADDER),
            "levels": [
                {
                    "concurrency": c,
                    "verdict": "PASS",
                    "passed": True,
                    "refusals": [],
                    "ttft_shift_q95_budget_ms": 15.0,
                    "minimum_detectable_effect_ms": 5.0,
                    "ttft_shift_q95_upper_bound_identified": True,
                    "wave_count": 72,
                }
                for c in REQUIRED_LADDER
            ],
        },
        "sampling_contract": {
            "model": "cx-chat-1b",
            "prompt": "test",
            "temperature": 0,
            "top_p": 0.95,
            "max_tokens": 32,
            "stream": True,
            "model_digest": digest,
        },
    }
    for key, value in overrides.items():
        if key == "body_identity" and isinstance(value, dict):
            merged = dict(receipt["body_identity"])
            merged.update(value)
            receipt["body_identity"] = merged
        elif key == "gate" and isinstance(value, dict):
            merged = dict(receipt["gate"])
            merged.update(value)
            receipt["gate"] = merged
        elif key == "budget" and isinstance(value, dict):
            merged = dict(receipt["budget"])
            merged.update(value)
            receipt["budget"] = merged
        else:
            receipt[key] = value
    return receipt


def validate_receipt(path: Path, *, now: datetime | None = None) -> list[str]:
    """Return a list of failure messages. Empty means the receipt clears the gate."""
    errors: list[str] = []
    if not path.exists():
        return [f"parity receipt absent: {path}"]

    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return [f"parity receipt unreadable: {path}: {exc}"]

    if not isinstance(data, dict):
        return [f"parity receipt must be a JSON object: {path}"]

    # Explicit withdrawal path.
    validity = str(data.get("validity", "")).strip()
    if validity and validity != "VALID":
        reasons = data.get("superseded_reason") or []
        if isinstance(reasons, str):
            reasons = [reasons]
        reasons = [r for r in reasons if str(r).strip()]
        if not reasons:
            return [
                f"{path}: validity={validity} but superseded_reason is empty. "
                f"A withdrawal without a recorded reason is not a withdrawal — it "
                f"is a gate silenced in one line. State what was wrong with the "
                f"measurement."
            ]
        WITHDRAWN.append((path, validity, reasons))
        return []

    # v1 / non-v2 receipts are not parity evidence. Treat as withdrawn when the
    # file has not already declared validity, so the committed historical v1
    # artifact does not greenwash this gate.
    schema = data.get("schema_version")
    if schema != SCHEMA_VERSION:
        reasons = [
            f"schema_version={schema!r} is not v2; v1 and other shapes are withdrawn "
            f"as parity evidence (authoritative harness is merc-gateway-parity-v2 / "
            f"{GATE_VERSION})"
        ]
        WITHDRAWN.append((path, "WITHDRAWN_V1_OR_FOREIGN_SCHEMA", reasons))
        return []

    gate_version = data.get("gate_version")
    if gate_version != GATE_VERSION:
        errors.append(
            f"{path}: gate_version={gate_version!r} want {GATE_VERSION!r}"
        )

    measured_at = parse_measured_at(data.get("measured_at"))
    if measured_at is None:
        errors.append(
            f"{path}: measured_at missing or not RFC3339 "
            f"(got {data.get('measured_at')!r}); undated receipts are treated "
            "as no measurement at all"
        )
    else:
        at = now or datetime.now(timezone.utc)
        if measured_at.tzinfo is None:
            measured_at = measured_at.replace(tzinfo=timezone.utc)
        age = at - measured_at
        if age > REVALIDATION_WINDOW:
            errors.append(
                f"{path}: measured_at {data.get('measured_at')} is "
                f"{age.days} days old, past the {REVALIDATION_WINDOW.days}-day "
                "revalidation window"
            )

    commit = data.get("merc_source_commit")
    if not isinstance(commit, str) or not commit.strip():
        errors.append(f"{path}: merc_source_commit missing or empty")

    evidence_class = data.get("evidence_class")
    if evidence_class != "PARITY_EVIDENCE":
        errors.append(
            f"{path}: evidence_class={evidence_class!r} is not PARITY_EVIDENCE "
            f"(HARNESS_SELF_TEST / INCOMPLETE_LADDER / others are not parity claims)"
        )

    refusals = data.get("refusals")
    if refusals is None:
        errors.append(
            f"{path}: refusals field missing "
            "(empty list required when budget is met)"
        )
    elif not isinstance(refusals, list):
        errors.append(f"{path}: refusals must be a list, got {type(refusals).__name__}")
    elif refusals:
        errors.append(
            f"{path}: refusals is non-empty ({len(refusals)}): "
            + "; ".join(str(r) for r in refusals)
        )

    if data.get("gate_passed") is not True:
        errors.append(f"{path}: gate_passed={data.get('gate_passed')!r} (required true)")
    if data.get("comparable") is not True:
        errors.append(f"{path}: comparable={data.get('comparable')!r} (required true)")

    budget = data.get("budget")
    if not isinstance(budget, dict):
        errors.append(f"{path}: budget block missing")
    else:
        for key in (
            "ttft_shift_q95_ms",
            "throughput_loss_fraction",
            "basis",
            "primary_metric",
        ):
            if key not in budget:
                errors.append(f"{path}: budget.{key} missing")

    claimed = data.get("claimed_concurrency_levels")
    if not isinstance(claimed, list):
        errors.append(f"{path}: claimed_concurrency_levels missing or not a list")
        claimed_set: set[int] = set()
    else:
        try:
            claimed_set = {int(c) for c in claimed}
        except (TypeError, ValueError):
            errors.append(f"{path}: claimed_concurrency_levels must be integers")
            claimed_set = set()
        for need in REQUIRED_LADDER:
            if need not in claimed_set:
                errors.append(
                    f"{path}: claimed_concurrency_levels missing required level {need} "
                    f"(ladder {list(REQUIRED_LADDER)}; got {claimed})"
                )

    body = data.get("body_identity")
    if not isinstance(body, dict):
        errors.append(f"{path}: body_identity block missing")
    else:
        if body.get("proven") is not True:
            errors.append(
                f"{path}: body_identity.proven={body.get('proven')!r} "
                "(PARITY_EVIDENCE requires Merc-bound proof)"
            )
        if body.get("bodies_equal") is not True:
            errors.append(
                f"{path}: body_identity.bodies_equal={body.get('bodies_equal')!r} "
                "(must be true only on evidence; default is false)"
            )
        if body.get("bare_sha_standin_refused") is True:
            errors.append(
                f"{path}: body_identity.bare_sha_standin_refused=true; "
                "bare-SHA stand-in is not parity evidence"
            )
        method = str(body.get("method") or "")
        if "Contract-ID" not in method and "contract" not in method.lower():
            errors.append(
                f"{path}: body_identity.method must name Contract-ID / contract proof "
                f"(got {method!r})"
            )

    gate = data.get("gate")
    if not isinstance(gate, dict):
        errors.append(f"{path}: gate block missing")
    else:
        if gate.get("version") != GATE_VERSION:
            errors.append(
                f"{path}: gate.version={gate.get('version')!r} want {GATE_VERSION!r}"
            )
        if gate.get("gate_passed") is not True:
            errors.append(
                f"{path}: gate.gate_passed={gate.get('gate_passed')!r} (required true)"
            )
        if gate.get("verdict") != "PASS":
            errors.append(f"{path}: gate.verdict={gate.get('verdict')!r} want PASS")
        # Nested flag must not disagree with the receipt (dual-pass collapse).
        if bool(gate.get("gate_passed")) != bool(data.get("gate_passed")):
            errors.append(
                f"{path}: nested gate.gate_passed={gate.get('gate_passed')!r} "
                f"disagrees with receipt gate_passed={data.get('gate_passed')!r}"
            )
        levels = gate.get("levels")
        if not isinstance(levels, list) or not levels:
            errors.append(f"{path}: gate.levels missing or empty")
        else:
            for i, lvl in enumerate(levels):
                if not isinstance(lvl, dict):
                    errors.append(f"{path}: gate.levels entry is not an object")
                    continue
                c = lvl.get("concurrency")
                path_lvl = f"{path}: gate.levels[{i}] (c={c})"
                # Required keys — never .get-optional: a rename that drops a key
                # must fail closed, not silently skip the under-powered check.
                required_lvl_keys = (
                    "verdict",
                    "minimum_detectable_effect_ms",
                    "ttft_shift_q95_budget_ms",
                )
                missing = [k for k in required_lvl_keys if k not in lvl]
                if missing:
                    errors.append(f"{path_lvl}: missing required keys {missing}")
                    continue
                verdict = lvl["verdict"]
                mde = lvl["minimum_detectable_effect_ms"]
                budget_ms = lvl["ttft_shift_q95_budget_ms"]
                if verdict != "PASS":
                    errors.append(
                        f"{path}: gate level c={c} verdict={verdict!r} want PASS"
                    )
                # Power gate: MDE must not exceed budget (required keys above).
                if not isinstance(mde, (int, float)):
                    errors.append(
                        f"{path_lvl}: minimum_detectable_effect_ms must be numeric, "
                        f"got {type(mde).__name__}"
                    )
                elif not isinstance(budget_ms, (int, float)):
                    errors.append(
                        f"{path_lvl}: ttft_shift_q95_budget_ms must be numeric, "
                        f"got {type(budget_ms).__name__}"
                    )
                elif budget_ms > 0 and mde > budget_ms:
                    errors.append(
                        f"{path}: gate level c={c} under-powered: "
                        f"MDE={mde} > budget={budget_ms}"
                    )

    return errors


def run_unit_tests() -> None:
    """Offline fixtures: structural refusals for the v2 schema."""
    with tempfile.TemporaryDirectory() as tmp:
        tmp_path = Path(tmp)

        def write(name: str, payload: dict[str, Any]) -> Path:
            path = tmp_path / name
            path.write_text(json.dumps(payload), encoding="utf-8")
            return path

        before = len(WITHDRAWN)
        withdrawn_payload = base_receipt()
        withdrawn_payload["validity"] = "INVALIDATED_PENDING_RERUN"
        withdrawn_payload["superseded_reason"] = ["one wave at c=32; n == c"]
        del withdrawn_payload["measured_at"]
        withdrawn = write("withdrawn.json", withdrawn_payload)
        errs = validate_receipt(withdrawn)
        check(errs == [], f"withdrawn receipt with reasons must not error, got {errs!r}")
        check(
            len(WITHDRAWN) == before + 1,
            "withdrawn receipt must be recorded so main() can announce it",
        )

        silent_payload = base_receipt()
        silent_payload["validity"] = "WITHDRAWN"
        silent = write("silent-withdrawal.json", silent_payload)
        errs = validate_receipt(silent)
        check(
            len(errs) == 1 and "superseded_reason is empty" in errs[0],
            f"reasonless withdrawal must fail, got {errs!r}",
        )

        # v1 schema is withdrawn, not a hard field-check failure.
        v1 = write(
            "v1.json",
            {
                "schema_version": 1,
                "kind": "gateway_parity",
                "gate_version": "gateway-parity-gate-v1",
                "measured_at": "2026-07-28T11:10:09Z",
                "merc_source_commit": "a" * 40,
                "refusals": [],
                "gate_passed": True,
                "comparable": True,
            },
        )
        before_v1 = len(WITHDRAWN)
        errs = validate_receipt(v1)
        check(errs == [], f"v1 must be withdrawn cleanly, got {errs!r}")
        check(len(WITHDRAWN) == before_v1 + 1, "v1 must be recorded as withdrawn")
        check(
            any("not v2" in r for r in WITHDRAWN[-1][2]),
            f"v1 withdrawal must name schema: {WITHDRAWN[-1]!r}",
        )

        undated_payload = base_receipt()
        del undated_payload["measured_at"]
        undated = write("undated.json", undated_payload)
        errs = validate_receipt(undated)
        check(any("measured_at" in e for e in errs), f"undated must fail, got {errs!r}")

        refused = write(
            "refused.json",
            base_receipt(refusals=["under-powered: MDE=21.000 ms exceeds TTFT budget 15.000 ms"]),
        )
        errs = validate_receipt(refused)
        check(
            any("refusals is non-empty" in e for e in errs),
            f"non-empty refusals must fail, got {errs!r}",
        )

        bare = write(
            "bare-sha.json",
            base_receipt(
                body_identity={
                    "proven": False,
                    "bodies_equal": False,
                    "bare_sha_standin_refused": True,
                    "method": "bare-SHA only",
                    "detail": "bare-SHA stand-in refused",
                },
                gate_passed=False,
                comparable=False,
                refusals=["bare-SHA stand-in refused"],
            ),
        )
        errs = validate_receipt(bare)
        check(len(errs) > 0, "bare-SHA receipt must fail")

        incomplete = write(
            "incomplete.json",
            base_receipt(
                claimed_concurrency_levels=[1],
                evidence_class="INCOMPLETE_LADDER",
                gate_passed=False,
                comparable=False,
                refusals=["incomplete concurrency ladder"],
            ),
        )
        errs = validate_receipt(incomplete)
        check(
            any("PARITY_EVIDENCE" in e or "claimed_concurrency" in e for e in errs),
            f"incomplete ladder must fail, got {errs!r}",
        )

        # Nested gate_passed true while receipt refused.
        dual = base_receipt(gate_passed=False, comparable=False, refusals=["x"])
        dual["gate"] = dict(dual["gate"])
        dual["gate"]["gate_passed"] = True
        dual_path = write("dual-flag.json", dual)
        errs = validate_receipt(dual_path)
        check(
            any("disagrees" in e or "gate_passed" in e for e in errs),
            f"dual pass flags must fail, got {errs!r}",
        )

        # Under-powered level recorded as PASS must fail validator.
        under = base_receipt()
        under["gate"] = dict(under["gate"])
        under["gate"]["levels"] = [
            {
                "concurrency": 1,
                "verdict": "PASS",
                "passed": True,
                "refusals": [],
                "ttft_shift_q95_budget_ms": 15.0,
                "minimum_detectable_effect_ms": 21.0,
            }
        ]
        under_path = write("underpowered.json", under)
        errs = validate_receipt(under_path)
        check(
            any("under-powered" in e or "MDE=" in e for e in errs),
            f"under-powered PASS level must fail validator, got {errs!r}",
        )

        # Renamed / required keys must fail closed — never .get-optional skip.
        no_mde = base_receipt()
        no_mde["gate"] = dict(no_mde["gate"])
        levels_no_mde = []
        for lvl in no_mde["gate"]["levels"]:
            stripped = dict(lvl)
            del stripped["minimum_detectable_effect_ms"]
            levels_no_mde.append(stripped)
        no_mde["gate"]["levels"] = levels_no_mde
        errs = validate_receipt(write("no-mde.json", no_mde))
        check(
            any("minimum_detectable_effect_ms" in e for e in errs),
            f"missing MDE key must fail closed, got {errs!r}",
        )
        no_budget_key = base_receipt()
        no_budget_key["gate"] = dict(no_budget_key["gate"])
        levels_no_b = []
        for lvl in no_budget_key["gate"]["levels"]:
            stripped = dict(lvl)
            del stripped["ttft_shift_q95_budget_ms"]
            levels_no_b.append(stripped)
        no_budget_key["gate"]["levels"] = levels_no_b
        errs = validate_receipt(write("no-shift-budget.json", no_budget_key))
        check(
            any("ttft_shift_q95_budget_ms" in e for e in errs),
            f"missing ttft_shift_q95_budget_ms must fail closed, got {errs!r}",
        )

        clean = write("clean.json", base_receipt())
        errs = validate_receipt(clean)
        check(errs == [], f"receipt-grade synthetic v2 must pass, got {errs!r}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--receipt",
        type=Path,
        default=DEFAULT_RECEIPT,
        help="path to the gateway parity receipt",
    )
    ap.add_argument(
        "--self-test-only",
        action="store_true",
        help="run offline unit tests only; skip the committed receipt",
    )
    args = ap.parse_args()

    run_unit_tests()
    WITHDRAWN.clear()

    if not args.self_test_only:
        for err in validate_receipt(args.receipt):
            fail(err)

    if FAILURES:
        print(f"gateway-parity-receipt: FAIL ({len(FAILURES)})")
        for f in FAILURES:
            print(f"  - {f}")
        return 1

    if WITHDRAWN:
        print("gateway-parity-receipt: NO VALID PARITY EVIDENCE")
        for path, validity, reasons in WITHDRAWN:
            print(f"  {path}: {validity}")
            for r in reasons:
                print(f"    * {r}")
        print(
            "  No parity or deficit claim may cite a withdrawn or non-v2 receipt. "
            "A replacement requires a new v2 measurement against a real Merc "
            "control plane (X-Merc-Contract-ID + upstream body SHA on every OK "
            "sample), not an edit to the file."
        )
        return 0

    print(
        "gateway-parity-receipt: PASS "
        "(v2/gate-v4, dated, empty refusals, Merc-bound body_identity, "
        "ladder {1,8,32}, nested gate_passed consistent, MDE ≤ budget)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
