#!/usr/bin/env python3
"""Retrofit binding_status onto existing evidence artifacts.

Does NOT invent producer identity for historical runs. Only adds:
  - binding_status: BOUND | UNBOUND | SUPERSEDED | WITHDRAWN
  - missing_identity_fields: [...]  (UNBOUND only)

Receipts (measurement descriptions) receive binding fields in-object.
Job-result payloads (embed vectors, batch_infer completions, …) and
non-object evidence (.jsonl/.txt) receive a sibling ``.binding.json``
sidecar so closed job contracts stay free of unknown fields.

Then writes a bound census summary at evidence/state/evidence-binding-census.json
via the single write path.

    python3 scripts/stamp-evidence-binding.py
    python3 scripts/stamp-evidence-binding.py --dry-run
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from lib.evidence_binding import (  # noqa: E402
    BINDING_BOUND,
    BINDING_SUPERSEDED,
    BINDING_UNBOUND,
    BINDING_WITHDRAWN,
    binding_sidecar_path,
    classify_existing_artifact,
    default_bound_identity,
    is_job_contract_payload,
    missing_fields_for_object,
    stamp_binding_fields,
    strip_binding_meta,
    write_bound_evidence,
    EvidenceBindingError,
)

EVIDENCE = ROOT / "evidence"
CENSUS_PATH = EVIDENCE / "state" / "evidence-binding-census.json"

# Keep in lockstep with scripts/validate-evidence-binding.py. These trees are
# not the binding corpus (policy/census/fixture/narrative).
EVIDENCE_SCAN_SKIP_PREFIXES = (
    "evidence/proof/",
    "evidence/census/",
    "evidence/artifacts/",
    "evidence/immutable-fixtures/",
    "evidence/workload-catalog/",
)
EVIDENCE_SCAN_SKIP_SUFFIXES = (".md",)


def iter_evidence_files() -> list[Path]:
    out: list[Path] = []
    for path in sorted(EVIDENCE.rglob("*")):
        if not path.is_file():
            continue
        if path.name.endswith(".binding.json"):
            continue
        if path.resolve() == CENSUS_PATH.resolve():
            continue
        rel = path.relative_to(ROOT).as_posix()
        if any(part.startswith(".") for part in path.relative_to(ROOT).parts):
            continue
        if any(rel.startswith(prefix) for prefix in EVIDENCE_SCAN_SKIP_PREFIXES):
            continue
        if path.suffix.lower() in EVIDENCE_SCAN_SKIP_SUFFIXES:
            continue
        out.append(path)
    return out


def stamp_json_object(path: Path, dry_run: bool) -> tuple[str, list[str]]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        return stamp_sidecar(path, dry_run, note="non-object JSON")
    # Job-result payloads (embed vectors, batch_infer completions, …) are
    # decoded with DisallowUnknownFields. Binding must never enter the body.
    if is_job_contract_payload(data):
        return stamp_payload_object(path, data, dry_run)
    status, missing = classify_existing_artifact(data, ROOT)
    if status == BINDING_UNBOUND and not missing:
        # A hand-written UNBOUND with an empty/omitted list is still UNBOUND;
        # recover the checklist so the validator does not treat it as malformed.
        missing = missing_fields_for_object(data, ROOT)
    stamped = stamp_binding_fields(data, status, missing)
    if not dry_run:
        text = json.dumps(stamped, indent=2, ensure_ascii=False) + "\n"
        path.write_text(text, encoding="utf-8")
    return status, missing


def stamp_payload_object(
    path: Path, data: dict, dry_run: bool
) -> tuple[str, list[str]]:
    """Sidecar binding for a job-result payload; body stays contract-clean.

    If a previous stamp left binding fields in the body, strip them back out
    (restoring a pretty-printed contract object) so the control plane can decode
    the artifact again. Prefer an existing sidecar's status over re-deriving.
    """
    side = binding_sidecar_path(path)
    payload = strip_binding_meta(data)
    # Prefer sidecar status when already present (re-stamp is idempotent).
    if side.is_file():
        try:
            existing = json.loads(side.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            existing = {}
        if isinstance(existing, dict) and existing.get("binding_status"):
            status = str(existing["binding_status"]).upper()
            missing = list(existing.get("missing_identity_fields") or [])
            if not dry_run and set(data.keys()) != set(payload.keys()):
                # Body still carries binding meta from a bad stamp — remove it.
                path.write_text(
                    json.dumps(payload, indent=2, ensure_ascii=False) + "\n",
                    encoding="utf-8",
                )
            return status, missing

    status, missing = classify_existing_artifact(payload, ROOT)
    # If the body still has binding meta (wrong prior stamp), use that status
    # before stripping so we do not lose WITHDRAWN/SUPERSEDED.
    if data.get("binding_status") and not side.is_file():
        status, missing = classify_existing_artifact(data, ROOT)

    doc = {
        "schema_version": 1,
        "kind": "evidence_binding_sidecar",
        "target": path.relative_to(ROOT).as_posix(),
        "binding_status": status,
        "missing_identity_fields": missing if status == BINDING_UNBOUND else [],
        "note": (
            "sidecar for job-result payload; body is a closed job contract "
            "and must not carry binding_status"
        ),
    }
    if status != BINDING_UNBOUND:
        doc.pop("missing_identity_fields", None)
    if not dry_run:
        side.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
        if set(data.keys()) != set(payload.keys()):
            path.write_text(
                json.dumps(payload, indent=2, ensure_ascii=False) + "\n",
                encoding="utf-8",
            )
    return status, missing if status == BINDING_UNBOUND else []


def stamp_sidecar(
    path: Path, dry_run: bool, note: str = ""
) -> tuple[str, list[str]]:
    """For jsonl/non-object files: write path+'.binding.json' only."""
    side = binding_sidecar_path(path)
    if side.is_file():
        try:
            existing = json.loads(side.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            existing = {}
        if isinstance(existing, dict) and existing.get("binding_status"):
            status = str(existing["binding_status"]).upper()
            missing = list(existing.get("missing_identity_fields") or [])
            return status, missing

    # Non-object historical files are UNBOUND: they carry no producer identity.
    status = BINDING_UNBOUND
    missing = [
        "source_commit",
        "build_digest",
        "model_artifact_digest",
        "image_digest",
        "harness_revision",
        "corpus_digest",
        "exact_config",
        "raw_samples",
    ]
    doc = {
        "schema_version": 1,
        "kind": "evidence_binding_sidecar",
        "target": path.relative_to(ROOT).as_posix(),
        "binding_status": status,
        "missing_identity_fields": missing,
        "note": note
        or "sidecar for non-object evidence; content left untouched",
    }
    if not dry_run:
        side.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
    return status, missing


def write_census(counts: dict[str, int], artifacts: list[dict], dry_run: bool) -> None:
    # Use this script file as the "binary" whose digest we bind — scripts have
    # no separate built binary; hashing the stamp driver is the honest answer.
    build_path = Path(__file__).resolve()
    identity = default_bound_identity(
        ROOT,
        harness_revision="scripts/stamp-evidence-binding.py",
        build_binary_path=build_path,
        exact_config="stamp of committed evidence tree at HEAD",
        raw_samples=f"artifact_count={sum(counts.values()) + 1}",
        model_na="census has no model weights",
        image_na="census has no container image",
        corpus_na="census summarises evidence/; not a corpus measurement",
    )
    # The census itself is BOUND; include it in the published counts.
    final_counts = {
        BINDING_BOUND: counts.get(BINDING_BOUND, 0) + 1,
        BINDING_UNBOUND: counts.get(BINDING_UNBOUND, 0),
        BINDING_SUPERSEDED: counts.get(BINDING_SUPERSEDED, 0),
        BINDING_WITHDRAWN: counts.get(BINDING_WITHDRAWN, 0),
    }
    census_rel = CENSUS_PATH.relative_to(ROOT).as_posix()
    final_artifacts = list(artifacts) + [
        {"path": census_rel, "binding_status": BINDING_BOUND}
    ]
    payload = {
        "schema_version": 1,
        "kind": "evidence_binding_census",
        "head": identity["source_commit"]["value"],
        "counts": final_counts,
        "total_artifacts": sum(final_counts.values()),
        "artifacts": final_artifacts,
        "note": (
            "Retrospective stamp only. UNBOUND means producer identity cannot "
            "be recovered for a historical run; digests were not invented. "
            "UNBOUND artifacts must not back programme claims. "
            "counts include this census (BOUND)."
        ),
    }
    if dry_run:
        print("census (dry-run):", json.dumps(payload["counts"], indent=2))
        return
    write_bound_evidence(
        path=CENSUS_PATH,
        payload=payload,
        identity=identity,
        repo_root=ROOT,
        build_binary_path=build_path,
    )
    print(f"wrote {CENSUS_PATH.relative_to(ROOT)}")
    print("final counts:", final_counts)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    counts = {
        BINDING_BOUND: 0,
        BINDING_UNBOUND: 0,
        BINDING_SUPERSEDED: 0,
        BINDING_WITHDRAWN: 0,
    }
    artifacts: list[dict] = []

    for path in iter_evidence_files():
        rel = path.relative_to(ROOT).as_posix()
        try:
            if path.suffix == ".json":
                # Peek: object vs array
                raw = path.read_text(encoding="utf-8")
                data = json.loads(raw)
                if isinstance(data, dict):
                    status, missing = stamp_json_object(path, args.dry_run)
                else:
                    status, missing = stamp_sidecar(
                        path, args.dry_run, note="non-object JSON"
                    )
            else:
                status, missing = stamp_sidecar(path, args.dry_run)
        except (OSError, json.JSONDecodeError) as exc:
            print(f"stamp: FAIL {rel}: {exc}", file=sys.stderr)
            return 1

        counts[status] = counts.get(status, 0) + 1
        entry: dict = {"path": rel, "binding_status": status}
        if status == BINDING_UNBOUND:
            entry["missing_identity_fields"] = missing
        artifacts.append(entry)
        print(f"  {status:10} {rel}" + (f"  missing={missing}" if missing else ""))

    print("counts:", counts)
    try:
        write_census(counts, artifacts, args.dry_run)
    except EvidenceBindingError as exc:
        print(f"stamp: census write refused: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
