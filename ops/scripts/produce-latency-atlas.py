#!/usr/bin/env python3
"""Emit a twelve-stage latency atlas bound to the commit it was produced at.

The atlas is a census, not a copy. A stage with no harness, or a harness that
was not re-run at the bind commit, is written UNMEASURED with the reason.
UNMEASURED is an honest state and survives — this producer will not invent a
percentile, write a zero, or copy a number measured at a different HEAD.

The refusal is the point. Asked to emit a value whose source_commit is not the
bind commit, it fails and names the stage and both commits.

Bind uses the existing helpers: ops/scripts/lib/receipt_binding.py (candidate,
stamp) and ops/scripts/lib/evidence_binding.py (producer identity, bound write).

This producer does not run the twelve measurements. Default emission is twelve
UNMEASURED rows bound to the candidate. Pass --measurements only for cells
freshly measured at that same commit.

    python3 ops/scripts/produce-latency-atlas.py
    python3 ops/scripts/produce-latency-atlas.py --out /tmp/latency-atlas.json
    python3 ops/scripts/produce-latency-atlas.py --measurements cells.json --out /tmp/atlas.json
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.evidence_binding import (  # noqa: E402
    EvidenceBindingError,
    default_bound_identity,
    write_bound_evidence,
)
from lib.receipt_binding import (  # noqa: E402
    bound_to,
    candidate_commit,
    receipt_commit,
    stamp,
)

PRODUCER = "ops/scripts/produce-latency-atlas.py"
ATLAS_KIND = "merc_latency_atlas"
SCHEMA_VERSION = 2
UNMEASURED = "UNMEASURED"

# Rectangular metric set. A stage that does not measure a field still carries
# it, as UNMEASURED — never as 0, never as a borrowed number.
METRIC_FIELDS = (
    "p50_ms",
    "p95_ms",
    "p99_ms",
    "throughput",
    "cpu",
    "ram",
    "gpu_memory",
    "network",
    "db_time",
    "allocations",
    "failure_rate",
)

# Optional context copied only from a cell already proven fresh at the bind
# commit. Never inferred.
OPTIONAL_MEASURED_FIELDS = ("samples", "concurrency", "units")

# Aliases a harness might use for the percentile columns.
_PERCENTILE_ALIASES = {
    "p50": "p50_ms",
    "p50_ms": "p50_ms",
    "p95": "p95_ms",
    "p95_ms": "p95_ms",
    "p99": "p99_ms",
    "p99_ms": "p99_ms",
}

# Keys that wrap a measurements object; not stage ids.
_WRAPPER_KEYS = frozenset(
    {
        "schema_version",
        "kind",
        "binding_status",
        "head",
        "honesty",
        "stages",
        "source_commit",
        "expected_commit",
        "candidate_commit",
        "commit",
        "produced_by",
        "producer_identity",
        "missing_identity_fields",
        "generated_from",
        "scales",
        "instrumentation_gaps",
        "structural_risks",
    }
)

# stage_id, display name, harness note, reason this producer emits UNMEASURED
# unless a fresh same-commit cell is supplied.
STAGE_CATALOG: tuple[dict[str, str | None], ...] = (
    {
        "stage_id": "project_analysis",
        "name": "project analysis",
        "harness": None,
        "unmeasured_reason": (
            "no latency harness: src/control/topology_planner_test.go and "
            "src/control/plan_calibration.go are correctness tests, not a "
            "wall-clock of topology/compiler/plan"
        ),
    },
    {
        "stage_id": "quote",
        "name": "quote",
        "harness": None,
        "unmeasured_reason": (
            "no latency harness: quote.go emits buyer ETA p50_secs from "
            "planner/observed history, not control-plane wall-clock of "
            "quote_build_and_persist"
        ),
    },
    {
        "stage_id": "market_selection",
        "name": "market selection",
        "harness": "partial",
        "unmeasured_reason": (
            "no HEAD measurement: offer_claim receipts exist at older commits "
            "(offer_count=1) and market_clearing_receipt has no harness; this "
            "producer does not re-run them"
        ),
    },
    {
        "stage_id": "lease",
        "name": "lease",
        "harness": None,
        "unmeasured_reason": (
            "no e2e lease latency harness: src/control/service_lease_*_test.go are "
            "correctness; atlas sl_* stages have never been measured as a path"
        ),
    },
    {
        "stage_id": "runtime_startup",
        "name": "runtime startup",
        "harness": "partial",
        "unmeasured_reason": (
            "no control-plane startup clock at this commit: "
            "hardware-characterization records agent load_ms, not "
            "process-start-to-serving per engine/cell"
        ),
    },
    {
        "stage_id": "artifact_transfer",
        "name": "artifact transfer",
        "harness": None,
        "unmeasured_reason": (
            "no latency harness: src/control/artifact_harness_test.go is a "
            "real-storage correctness environment, not a timed presign/GET/PUT "
            "receipt"
        ),
    },
    {
        "stage_id": "model_load",
        "name": "model load",
        "harness": "partial",
        "unmeasured_reason": (
            "no per-cell weights-to-ready receipt at this commit: load_ms on "
            "hardware-characterization is not split out from throughput benches"
        ),
    },
    {
        "stage_id": "prefill",
        "name": "prefill",
        "harness": "ops/scripts/bench-harness.py",
        "unmeasured_reason": (
            "harness exists (ops/scripts/bench-harness.py, merc-agent bench-batch) "
            "but was not re-run at this commit; this producer does not copy an "
            "older prefill_s/ttft_ms"
        ),
    },
    {
        "stage_id": "decode",
        "name": "decode",
        "harness": "ops/scripts/bench-harness.py",
        "unmeasured_reason": (
            "harness exists (ops/scripts/bench-harness.py, merc-agent bench-batch) "
            "but was not re-run at this commit; this producer does not copy an "
            "older decode tok/s"
        ),
    },
    {
        "stage_id": "verification",
        "name": "verification",
        "harness": "partial",
        "unmeasured_reason": (
            "no job-verification wall-clock harness: "
            "render_verify_pipeline_bench_test.go covers Cycles-CPU L1 only "
            "and is not a HEAD measurement of verification_process"
        ),
    },
    {
        "stage_id": "settlement",
        "name": "settlement",
        "harness": "partial",
        "unmeasured_reason": (
            "no job-settlement latency harness at this commit: segment harness "
            "settlement_*_ms is a stub-upstream receipt at an older SHA; money "
            "tests do not emit p50"
        ),
    },
    {
        "stage_id": "control_plane_overhead",
        "name": "control-plane overhead",
        "harness": "src/control/merc_latency_gap_accounting_test.go",
        "unmeasured_reason": (
            "harnesses exist (gap-accounting, segment, hot-path, authorize "
            "tails, heartbeat ingest) but every committed receipt is bound to "
            "a SHA behind HEAD; this producer does not copy them"
        ),
    },
)

STAGE_IDS: tuple[str, ...] = tuple(row["stage_id"] for row in STAGE_CATALOG)  # type: ignore[misc]
_CATALOG_BY_ID: dict[str, dict[str, str | None]] = {
    row["stage_id"]: row for row in STAGE_CATALOG  # type: ignore[misc]
}

HONESTY = (
    "A stage with no measurement at the bind commit is UNMEASURED with a "
    "reason. No field is inherited from an older HEAD. Zero is not a "
    "substitute for UNMEASURED. This producer does not run harnesses."
)


class StaleInheritanceError(Exception):
    """Asked to emit a value measured at a different commit than the atlas.

    The message always names the stage and both commits so the refusal can be
    grepped without reading a stack trace.
    """

    def __init__(
        self,
        stage_id: str,
        measured_at: str,
        atlas_commit: str,
        detail: str = "",
    ) -> None:
        self.stage_id = stage_id
        self.measured_at = measured_at
        self.atlas_commit = atlas_commit
        msg = (
            f"refusing to inherit stage {stage_id!r} measured at {measured_at} "
            f"into atlas bound to {atlas_commit}"
        )
        if detail:
            msg = f"{msg}: {detail}"
        super().__init__(msg)


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def measurement_commit(cell: Any) -> str | None:
    """Commit a measurement cell claims, or None when it claims none.

    Uses receipt_commit so producers and the checker share one answer, then
    reads the legacy aliases the old atlas used (`head`) and a nested
    `measurement` block.
    """
    if not isinstance(cell, dict):
        return None
    found = receipt_commit(cell)
    if found:
        return found
    for alias in ("head", "measured_at_commit", "measured_commit"):
        found = receipt_commit({"commit": cell.get(alias)})
        if found:
            return found
    nested = cell.get("measurement")
    if isinstance(nested, dict):
        return measurement_commit(nested)
    identity = cell.get("producer_identity")
    if isinstance(identity, dict):
        found = receipt_commit({"producer_identity": identity})
        if found:
            return found
    return None


def _has_measured_value(cell: dict[str, Any]) -> bool:
    if str(cell.get("status") or "").strip().upper() == "MEASURED":
        return True
    for key, value in cell.items():
        field = _PERCENTILE_ALIASES.get(key, key)
        if field in METRIC_FIELDS and _is_number(value):
            return True
        if field in OPTIONAL_MEASURED_FIELDS and value not in (None, "", UNMEASURED):
            return True
    nested = cell.get("measurement")
    if isinstance(nested, dict):
        return _has_measured_value(nested)
    return False


def _metric_from_cell(cell: dict[str, Any], field: str) -> Any:
    aliases = [field] + [src for src, dest in _PERCENTILE_ALIASES.items() if dest == field]
    for key in aliases:
        if key in cell and cell[key] not in (None, ""):
            return cell[key]
    nested = cell.get("measurement")
    if isinstance(nested, dict):
        return _metric_from_cell(nested, field)
    return UNMEASURED


def refuse_stale(stage_id: str, measured_at: str, atlas_commit: str, detail: str = "") -> None:
    raise StaleInheritanceError(stage_id, measured_at, atlas_commit, detail)


def assert_fresh(stage_id: str, cell: dict[str, Any], candidate: str) -> None:
    """Fail if this cell reuses a value measured at a different commit.

    Built first, called on every supplied cell, including UNMEASURED-looking
    ones that still carry a number. The atlas is not emitted if this fires.
    """
    measured = measurement_commit(cell)
    claimed = str(cell.get("status") or "").strip().upper()
    has_value = _has_measured_value(cell)

    if claimed == "UNMEASURED" and has_value and measured != candidate:
        # Contradiction: labelled UNMEASURED but carrying a number from
        # somewhere else (or from nowhere).
        refuse_stale(
            stage_id,
            measured or "(no source_commit)",
            candidate,
            "UNMEASURED cell carries a measured field",
        )
    if claimed == "UNMEASURED" and has_value and measured == candidate:
        raise ValueError(
            f"stage {stage_id!r} is labelled UNMEASURED but carries a measured "
            f"field; a fresh cell at {candidate} must be status MEASURED"
        )

    if not has_value:
        if measured and measured != candidate:
            refuse_stale(stage_id, measured, candidate)
        return

    if not measured:
        refuse_stale(
            stage_id,
            "(no source_commit)",
            candidate,
            "numeric/MEASURED value has no source_commit so it cannot be proven fresh",
        )
    if measured != candidate:
        refuse_stale(stage_id, measured, candidate)


def _unmeasured_stage(row: dict[str, str | None]) -> dict[str, Any]:
    stage: dict[str, Any] = {
        "stage_id": row["stage_id"],
        "name": row["name"],
        "status": UNMEASURED,
        "source_commit": UNMEASURED,
        "harness": row["harness"] if row["harness"] is not None else UNMEASURED,
        "reason": row["unmeasured_reason"],
    }
    for field in METRIC_FIELDS:
        stage[field] = UNMEASURED
    return stage


def _measured_stage(
    row: dict[str, str | None],
    cell: dict[str, Any],
    candidate: str,
) -> dict[str, Any]:
    stage: dict[str, Any] = {
        "stage_id": row["stage_id"],
        "name": row["name"],
        "status": "MEASURED",
        "source_commit": candidate,
        "harness": cell.get("harness", row["harness"]),
        "reason": None,
    }
    any_number = False
    for field in METRIC_FIELDS:
        value = _metric_from_cell(cell, field)
        if value is None or value == "" or value == UNMEASURED:
            stage[field] = UNMEASURED
            continue
        if not _is_number(value):
            raise ValueError(
                f"stage {row['stage_id']!r} field {field} is {value!r}; "
                "use a number or UNMEASURED"
            )
        stage[field] = value
        any_number = True
    if not any_number:
        raise ValueError(
            f"stage {row['stage_id']!r} is MEASURED at {candidate} but every "
            "metric is UNMEASURED"
        )
    for field in OPTIONAL_MEASURED_FIELDS:
        if field in cell and cell[field] not in (None, "", UNMEASURED):
            stage[field] = cell[field]
    return stage


def normalize_measurements(raw: Any, candidate: str) -> dict[str, dict[str, Any]]:
    """Turn a measurements file (or in-memory dict) into {stage_id: cell}.

    A whole atlas bound to some other commit is refused before any stage is
    copied — that is how the old UNBOUND census would otherwise sneak in.
    """
    if raw is None:
        return {}
    if not isinstance(raw, dict):
        raise ValueError("measurements must be a JSON object")

    whole = measurement_commit(raw)
    if str(raw.get("kind") or "") == ATLAS_KIND and whole and whole != candidate:
        refuse_stale(
            "atlas",
            whole,
            candidate,
            "will not copy an atlas produced at a different commit",
        )

    body: Any = raw
    stages_field = raw.get("stages")
    if isinstance(stages_field, dict):
        body = stages_field
    elif isinstance(stages_field, list):
        converted: dict[str, Any] = {}
        for item in stages_field:
            if not isinstance(item, dict) or "stage_id" not in item:
                raise ValueError("stages list entries must be objects with stage_id")
            converted[str(item["stage_id"])] = item
        body = converted

    if not isinstance(body, dict):
        raise ValueError("measurements stages must be an object keyed by stage_id")

    out: dict[str, dict[str, Any]] = {}
    for key, cell in body.items():
        if key in _WRAPPER_KEYS:
            continue
        if key not in STAGE_IDS:
            raise ValueError(
                f"unknown stage id {key!r}; known: {', '.join(STAGE_IDS)}"
            )
        if not isinstance(cell, dict):
            raise ValueError(f"stage {key!r} measurement must be a JSON object")
        out[key] = cell
    return out


def build_atlas(
    measurements: dict[str, Any] | None,
    candidate: str,
) -> dict[str, Any]:
    """Assemble the twelve-stage atlas or refuse.

    `candidate` is the commit the atlas will claim. Every supplied measured
    value must already name that same commit. This function does not stamp
    binding; callers stamp after a successful build.
    """
    if not candidate or len(str(candidate).strip()) != 40:
        raise ValueError(f"atlas candidate is not a 40-hex commit: {candidate!r}")
    candidate = str(candidate).strip().lower()

    cells = normalize_measurements(measurements, candidate)
    stages: list[dict[str, Any]] = []
    for row in STAGE_CATALOG:
        stage_id = str(row["stage_id"])
        cell = cells.get(stage_id)
        if cell is None:
            stages.append(_unmeasured_stage(row))
            continue
        assert_fresh(stage_id, cell, candidate)
        if _has_measured_value(cell):
            stages.append(_measured_stage(row, cell, candidate))
        else:
            stages.append(_unmeasured_stage(row))

    measured_n = sum(1 for s in stages if s["status"] == "MEASURED")
    unmeasured_n = sum(1 for s in stages if s["status"] == UNMEASURED)
    doc: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "kind": ATLAS_KIND,
        "honesty": {
            "unmeasured_policy": HONESTY,
            "producer_does_not_run_harnesses": True,
            "inherited_fields": 0,
        },
        "stage_ids": list(STAGE_IDS),
        "stage_count": len(STAGE_IDS),
        "measured_count": measured_n,
        "unmeasured_count": unmeasured_n,
        "stages": stages,
    }
    _assert_atlas_fresh(doc, candidate)
    return doc


def _assert_atlas_fresh(doc: dict[str, Any], candidate: str) -> None:
    """Second pass: the document that would be written carries no stale number."""
    for stage in doc["stages"]:
        stage_id = str(stage.get("stage_id"))
        status = str(stage.get("status") or "")
        if status == "MEASURED":
            if stage.get("source_commit") != candidate:
                refuse_stale(
                    stage_id,
                    str(stage.get("source_commit")),
                    candidate,
                )
            continue
        if status != UNMEASURED:
            raise ValueError(f"stage {stage_id!r} has illegal status {status!r}")
        if not stage.get("reason"):
            raise ValueError(f"stage {stage_id!r} is UNMEASURED with no reason")
        if stage.get("source_commit") != UNMEASURED:
            refuse_stale(
                stage_id,
                str(stage.get("source_commit")),
                candidate,
                "UNMEASURED stage must not name a measurement commit",
            )
        for field in METRIC_FIELDS:
            value = stage.get(field)
            if value != UNMEASURED:
                refuse_stale(
                    stage_id,
                    measurement_commit(stage) or "(no source_commit)",
                    candidate,
                    f"UNMEASURED stage carries {field}={value!r}",
                )
            if value == 0 or value == 0.0:
                raise ValueError(
                    f"stage {stage_id!r} field {field} is zero; UNMEASURED "
                    "must not be filled with 0"
                )


def attach_binding(doc: dict[str, Any], repo: str | Path) -> dict[str, Any]:
    """Stamp the atlas with the declared candidate and a complete identity."""
    repo = Path(repo)
    candidate = candidate_commit(str(repo))
    stamp(doc, candidate, PRODUCER)
    identity = default_bound_identity(
        repo,
        harness_revision=PRODUCER,
        build_binary_path=Path(__file__).resolve(),
        exact_config=(
            "twelve-stage latency atlas; unmeasured stages remain UNMEASURED; "
            "no field is copied from an older HEAD"
        ),
        raw_samples="stage table in receipt body; this producer does not run harnesses",
        model_na="atlas is a census of stage clocks, not a model-weight measurement",
        image_na="atlas is not a container-image measurement",
        corpus_na="atlas does not ingest an external corpus",
    )
    identity_commit = str(identity["source_commit"]["value"]).strip().lower()
    if identity_commit != candidate:
        refuse_stale(
            "atlas",
            identity_commit,
            candidate,
            "receipt_binding candidate and evidence_binding source_commit disagree",
        )
    doc["producer_identity"] = identity
    if not bound_to(doc, candidate):
        raise ValueError(
            f"atlas stamp did not bind to candidate {candidate}"
        )
    return doc


def produce(
    *,
    repo: str | Path,
    measurements: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Build and bind an atlas for `repo`. Does not write a file."""
    candidate = candidate_commit(str(repo))
    doc = build_atlas(measurements, candidate)
    return attach_binding(doc, repo)


def write_atlas(doc: dict[str, Any], path: Path, repo: str | Path) -> None:
    """Write via the single bound-evidence path. Never open() to evidence/."""
    identity = doc.get("producer_identity")
    if not isinstance(identity, dict):
        raise ValueError("atlas has no producer_identity; attach_binding first")
    write_bound_evidence(
        path=path,
        payload=doc,
        identity=identity,
        repo_root=repo,
        build_binary_path=Path(__file__).resolve(),
    )


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError(f"cannot read {path}: {exc}") from exc


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument(
        "--out",
        default="",
        help="write a bound JSON receipt to this path (default: stdout)",
    )
    ap.add_argument(
        "--measurements",
        default="",
        help=(
            "JSON object of fresh stage cells keyed by stage_id. Each MEASURED "
            "cell must carry source_commit equal to the bind commit. A cell "
            "measured at any other commit is refused."
        ),
    )
    ap.add_argument(
        "--repo",
        default=str(ROOT),
        help="git root whose candidate/HEAD the atlas binds to",
    )
    args = ap.parse_args(argv)

    repo = Path(args.repo)
    measurements: dict[str, Any] | None = None
    if args.measurements:
        measurements = load_json(Path(args.measurements))

    try:
        doc = produce(repo=repo, measurements=measurements)
        if args.out:
            write_atlas(doc, Path(args.out), repo)
            print(
                f"wrote {args.out} source_commit={doc.get('source_commit')} "
                f"binding_status={doc.get('binding_status')} "
                f"measured={doc.get('measured_count')} "
                f"unmeasured={doc.get('unmeasured_count')}"
            )
        else:
            sys.stdout.write(json.dumps(doc, indent=2, ensure_ascii=False) + "\n")
    except StaleInheritanceError as exc:
        print(f"{PRODUCER}: REFUSED: {exc}", file=sys.stderr)
        return 2
    except (ValueError, EvidenceBindingError, OSError) as exc:
        print(f"{PRODUCER}: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
