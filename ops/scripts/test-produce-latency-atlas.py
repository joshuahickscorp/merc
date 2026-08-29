#!/usr/bin/env python3
"""Prove the latency-atlas producer refuses stale inheritance.

The obligation is not "an atlas exists". It is: an atlas built from a value
measured at a different commit fails, the error names the stage and both
commits, and a stage with no harness is UNMEASURED with a reason — never zero,
never a borrowed percentile.

    python3 ops/scripts/test-produce-latency-atlas.py
"""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PRODUCER_PATH = ROOT / "ops/scripts" / "produce-latency-atlas.py"

SPEC = importlib.util.spec_from_file_location("produce_latency_atlas", PRODUCER_PATH)
if SPEC is None or SPEC.loader is None:
    raise SystemExit(f"cannot load {PRODUCER_PATH}")
PRODUCER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PRODUCER)

sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.receipt_binding import bound_to, candidate_commit, receipt_commit  # noqa: E402

FAILURES: list[str] = []

# The UNBOUND census this producer exists to stop inheriting.
STALE_COMMIT = "74dea5e17dcc44c7790c4e6b41c48bbd59dd0f0d"


def check(condition: bool, message: str) -> None:
    if not condition:
        FAILURES.append(message)


def _fresh_cell(commit: str, **metrics: object) -> dict:
    cell: dict = {"status": "MEASURED", "source_commit": commit}
    cell.update(metrics)
    return cell


def test_unmeasured_atlas_binds_to_candidate() -> None:
    candidate = candidate_commit(str(ROOT))
    doc = PRODUCER.produce(repo=ROOT)
    check(doc.get("kind") == "merc_latency_atlas", f"kind={doc.get('kind')!r}")
    check(doc.get("schema_version") == 2, f"schema_version={doc.get('schema_version')!r}")
    check(doc.get("binding_status") == "BOUND", f"binding_status={doc.get('binding_status')!r}")
    check(bound_to(doc, candidate), f"atlas not bound_to candidate {candidate}: {receipt_commit(doc)}")
    check(receipt_commit(doc) == candidate, f"receipt_commit={receipt_commit(doc)} candidate={candidate}")
    check(doc.get("produced_by") == "ops/scripts/produce-latency-atlas.py", f"produced_by={doc.get('produced_by')!r}")
    check(doc.get("stage_count") == 12, f"stage_count={doc.get('stage_count')!r}")
    check(doc.get("unmeasured_count") == 12, f"unmeasured_count={doc.get('unmeasured_count')!r}")
    check(doc.get("measured_count") == 0, f"measured_count={doc.get('measured_count')!r}")
    check(doc.get("honesty", {}).get("inherited_fields") == 0, "honesty.inherited_fields must be 0")

    stages = doc.get("stages")
    check(isinstance(stages, list) and len(stages) == 12, f"stages len={0 if not isinstance(stages, list) else len(stages)}")
    got_ids = [s.get("stage_id") for s in stages] if isinstance(stages, list) else []
    check(got_ids == list(PRODUCER.STAGE_IDS), f"stage_ids={got_ids}")

    for stage in stages or []:
        sid = stage.get("stage_id")
        check(stage.get("status") == PRODUCER.UNMEASURED, f"{sid} status={stage.get('status')!r}")
        check(isinstance(stage.get("reason"), str) and stage["reason"].strip(), f"{sid} missing UNMEASURED reason")
        check(stage.get("source_commit") == PRODUCER.UNMEASURED, f"{sid} source_commit={stage.get('source_commit')!r}")
        for field in PRODUCER.METRIC_FIELDS:
            value = stage.get(field)
            check(
                value == PRODUCER.UNMEASURED,
                f"{sid}.{field}={value!r} (must be UNMEASURED, not a number or zero)",
            )
            check(value != 0, f"{sid}.{field} is the integer 0")
            check(value != 0.0, f"{sid}.{field} is the float 0.0")


def test_fresh_measurement_at_candidate_is_kept() -> None:
    candidate = candidate_commit(str(ROOT))
    doc = PRODUCER.produce(
        repo=ROOT,
        measurements={
            "prefill": _fresh_cell(
                candidate,
                p50_ms=12.5,
                p95_ms=18.0,
                p99_ms=21.25,
                throughput=410.0,
                samples=40,
            )
        },
    )
    check(bound_to(doc, candidate), "fresh atlas lost its bind")
    check(doc.get("measured_count") == 1, f"measured_count={doc.get('measured_count')!r}")
    check(doc.get("unmeasured_count") == 11, f"unmeasured_count={doc.get('unmeasured_count')!r}")
    by_id = {s["stage_id"]: s for s in doc["stages"]}
    prefill = by_id["prefill"]
    check(prefill["status"] == "MEASURED", f"prefill status={prefill['status']!r}")
    check(prefill["source_commit"] == candidate, f"prefill source_commit={prefill['source_commit']}")
    check(prefill["p50_ms"] == 12.5, f"prefill p50_ms={prefill['p50_ms']!r}")
    check(prefill["p95_ms"] == 18.0, f"prefill p95_ms={prefill['p95_ms']!r}")
    check(prefill["p99_ms"] == 21.25, f"prefill p99_ms={prefill['p99_ms']!r}")
    check(prefill["throughput"] == 410.0, f"prefill throughput={prefill['throughput']!r}")
    check(prefill["cpu"] == PRODUCER.UNMEASURED, "omitted metric must stay UNMEASURED, not 0")
    check(prefill.get("samples") == 40, f"prefill samples={prefill.get('samples')!r}")
    quote = by_id["quote"]
    check(quote["status"] == PRODUCER.UNMEASURED, "quote must remain UNMEASURED")
    check(quote["p50_ms"] == PRODUCER.UNMEASURED, "quote p50 must not borrow prefill")
    check(isinstance(quote.get("reason"), str) and quote["reason"].strip(), "quote missing reason")


def test_stale_commit_is_refused_and_names_stage_and_both_commits() -> None:
    candidate = candidate_commit(str(ROOT))
    check(STALE_COMMIT != candidate, "stale fixture accidentally equals candidate")
    measurements = {
        "quote": _fresh_cell(
            STALE_COMMIT,
            p50_ms=1.133,
            p95_ms=1.445,
            p99_ms=1.582,
        )
    }
    try:
        PRODUCER.build_atlas(measurements, candidate)
    except PRODUCER.StaleInheritanceError as exc:
        text = str(exc)
        check(exc.stage_id == "quote", f"stage_id={exc.stage_id!r}")
        check(exc.measured_at == STALE_COMMIT, f"measured_at={exc.measured_at!r}")
        check(exc.atlas_commit == candidate, f"atlas_commit={exc.atlas_commit!r}")
        check("quote" in text, f"error does not name stage: {text}")
        check(STALE_COMMIT in text, f"error does not name measured commit: {text}")
        check(candidate in text, f"error does not name atlas commit: {text}")
        check(text.startswith("refusing to inherit stage"), f"error is not a loud refusal: {text}")
    else:
        check(False, "stale quote cell was accepted; producer must refuse")


def test_cli_refuses_stale_inheritance() -> None:
    candidate = candidate_commit(str(ROOT))
    with tempfile.TemporaryDirectory(prefix="merc-atlas-stale-") as tmp:
        measurements_path = Path(tmp) / "cells.json"
        measurements_path.write_text(
            json.dumps(
                {
                    "quote": {
                        "source_commit": STALE_COMMIT,
                        "p50_ms": 1.133,
                        "p95_ms": 1.445625,
                        "p99_ms": 1.582,
                    }
                }
            )
            + "\n",
            encoding="utf-8",
        )
        proc = subprocess.run(
            [
                sys.executable,
                str(PRODUCER_PATH),
                "--repo",
                str(ROOT),
                "--measurements",
                str(measurements_path),
            ],
            cwd=str(ROOT),
            capture_output=True,
            text=True,
        )
        err = proc.stderr
        check(proc.returncode != 0, f"CLI accepted stale inheritance, rc={proc.returncode}")
        check(proc.returncode == 2, f"CLI rc={proc.returncode}, expected 2; stderr={err!r}")
        check("REFUSED" in err, f"CLI did not say REFUSED: {err}")
        check("quote" in err, f"CLI error does not name stage: {err}")
        check(STALE_COMMIT in err, f"CLI error does not name measured commit: {err}")
        check(candidate in err, f"CLI error does not name atlas commit: {err}")
        check(proc.stdout.strip() == "", f"CLI wrote atlas on refusal: {proc.stdout[:200]!r}")


def test_whole_atlas_at_other_commit_is_refused() -> None:
    candidate = candidate_commit(str(ROOT))
    try:
        PRODUCER.build_atlas(
            {
                "kind": "merc_latency_atlas",
                "source_commit": STALE_COMMIT,
                "stages": {
                    "quote": _fresh_cell(STALE_COMMIT, p50_ms=1.0),
                },
            },
            candidate,
        )
    except PRODUCER.StaleInheritanceError as exc:
        text = str(exc)
        check(STALE_COMMIT in text, f"whole-atlas error missing stale commit: {text}")
        check(candidate in text, f"whole-atlas error missing candidate: {text}")
    else:
        check(False, "whole atlas at a different commit was accepted")


def test_unattributed_number_is_refused() -> None:
    candidate = candidate_commit(str(ROOT))
    try:
        PRODUCER.build_atlas({"decode": {"p50_ms": 3.14}}, candidate)
    except PRODUCER.StaleInheritanceError as exc:
        text = str(exc)
        check(exc.stage_id == "decode", f"stage_id={exc.stage_id!r}")
        check("decode" in text, f"error does not name stage: {text}")
        check(candidate in text, f"error does not name atlas commit: {text}")
    else:
        check(False, "number with no source_commit was accepted")


def test_unknown_stage_is_refused() -> None:
    candidate = candidate_commit(str(ROOT))
    try:
        PRODUCER.build_atlas(
            {"read_body": _fresh_cell(candidate, p50_ms=0.004)},
            candidate,
        )
    except ValueError as exc:
        check("read_body" in str(exc), f"unknown-stage error: {exc}")
    else:
        check(False, "unknown stage id was accepted")


def test_write_does_not_touch_committed_atlas() -> None:
    committed = ROOT / "evidence" / "perf" / "latency-atlas.json"
    before = committed.exists()
    candidate = candidate_commit(str(ROOT))
    with tempfile.TemporaryDirectory(prefix="merc-atlas-out-") as tmp:
        out = Path(tmp) / "latency-atlas.json"
        doc = PRODUCER.produce(repo=ROOT)
        PRODUCER.write_atlas(doc, out, ROOT)
        written = json.loads(out.read_text(encoding="utf-8"))
        check(written.get("binding_status") == "BOUND", "written atlas is not BOUND")
        check(bound_to(written, candidate), "written atlas not bound to candidate")
        check(written.get("unmeasured_count") == 12, "written atlas invented measurements")
        identity = written.get("producer_identity")
        check(isinstance(identity, dict), "written atlas missing producer_identity")
        if isinstance(identity, dict):
            slot = identity.get("source_commit") or {}
            check(
                isinstance(slot, dict) and slot.get("value") == candidate,
                f"producer_identity.source_commit={slot!r}",
            )
    check(committed.exists() == before, "producer created or removed evidence/perf/latency-atlas.json")


def test_help_exits_zero() -> None:
    proc = subprocess.run(
        [sys.executable, str(PRODUCER_PATH), "--help"],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
    )
    check(proc.returncode == 0, f"--help rc={proc.returncode} stderr={proc.stderr}")
    check("UNMEASURED" in proc.stdout, "--help does not mention UNMEASURED")
    check("--measurements" in proc.stdout, "--help missing --measurements")


def main() -> int:
    test_unmeasured_atlas_binds_to_candidate()
    test_fresh_measurement_at_candidate_is_kept()
    test_stale_commit_is_refused_and_names_stage_and_both_commits()
    test_cli_refuses_stale_inheritance()
    test_whole_atlas_at_other_commit_is_refused()
    test_unattributed_number_is_refused()
    test_unknown_stage_is_refused()
    test_write_does_not_touch_committed_atlas()
    test_help_exits_zero()

    if FAILURES:
        print("test-produce-latency-atlas: FAIL")
        for item in FAILURES:
            print(f"  - {item}")
        return 1
    print("test-produce-latency-atlas: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
