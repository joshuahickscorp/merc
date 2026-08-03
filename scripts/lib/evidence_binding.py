"""Evidence producer-identity binding — Python side of the write path.

Mirrors control/receipt_identity.go. Scripts that write under evidence/ must
call write_bound_evidence() rather than open()/json.dump directly.

Rules:
  - every identity field is a value or an explicit na reason (never empty)
  - source_commit must be a real git object in this repo
  - build_digest value must equal sha256 of the supplied binary path
  - withdrawn paths cannot be overwritten with non-withdrawn content without
    a new authority_id (sticky withdrawal)
  - refusal is an exception / non-zero return, not a log line
"""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import tempfile
from pathlib import Path
from typing import Any

BINDING_BOUND = "BOUND"
BINDING_UNBOUND = "UNBOUND"
BINDING_SUPERSEDED = "SUPERSEDED"
BINDING_WITHDRAWN = "WITHDRAWN"

IDENTITY_FIELDS = (
    "source_commit",
    "build_digest",
    "model_artifact_digest",
    "image_digest",
    "harness_revision",
    "corpus_digest",
    "exact_config",
    "raw_samples",
)

# Legacy key aliases used when classifying already-written artifacts.
# Classification never invents digests; it only reports what is present.
LEGACY_FIELD_KEYS: dict[str, tuple[str, ...]] = {
    "source_commit": ("source_commit", "merc_source_commit", "head", "commit"),
    "build_digest": (
        "build_digest",
        "binary_digest",
        "binary_sha256",
        "control_digest",
        "agent_digest",
    ),
    "model_artifact_digest": (
        "model_digest",
        "model_artifact_sha256",
        "merc_model_artifact_sha256",
        "artifact_digest",
        "artifact_sha256",
    ),
    "image_digest": ("image_digest", "image_id", "container_digest", "image_sha256"),
    "harness_revision": ("harness_revision", "harness", "gate_version"),
    "corpus_digest": ("corpus_digest", "corpus_sha256", "input_fixture_sha256"),
    "exact_config": ("exact_config", "config", "scope"),
    "raw_samples": ("raw_samples", "raw_sample_count", "samples", "runs", "levels"),
}


class EvidenceBindingError(Exception):
    """Raised when an evidence write or validation is refused."""


def slot_value(value: str) -> dict[str, str]:
    v = (value or "").strip()
    if not v:
        raise EvidenceBindingError("identity slot value must be non-empty")
    return {"value": v}


def slot_na(reason: str) -> dict[str, str]:
    r = (reason or "").strip()
    if not r:
        raise EvidenceBindingError("identity slot na reason must be non-empty")
    return {"na": r}


def slot_complete(slot: Any) -> bool:
    if not isinstance(slot, dict):
        return False
    value = str(slot.get("value") or "").strip()
    na = str(slot.get("na") or "").strip()
    if value and na:
        return False
    return bool(value or na)


def incomplete_fields(identity: dict[str, Any]) -> list[str]:
    missing: list[str] = []
    for name in IDENTITY_FIELDS:
        if not slot_complete(identity.get(name)):
            missing.append(name)
    return missing


def sha256_file(path: str | Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def validate_git_object(repo_root: str | Path, rev: str) -> None:
    rev = (rev or "").strip()
    if not rev or any(c in rev for c in " \t\n\r"):
        raise EvidenceBindingError(f"source_commit: {rev!r} is not a git object")
    try:
        subprocess.run(
            ["git", "-C", str(repo_root), "cat-file", "-e", f"{rev}^{{object}}"],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (subprocess.CalledProcessError, FileNotFoundError, OSError) as exc:
        raise EvidenceBindingError(
            f"source_commit: {rev!r} is not a git object in this repo"
        ) from exc


def resolve_source_commit(repo_root: str | Path) -> str:
    """HEAD only — never accept free-form MERC_SOURCE_COMMIT strings."""
    try:
        out = subprocess.check_output(
            ["git", "-C", str(repo_root), "rev-parse", "HEAD"],
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except (subprocess.CalledProcessError, FileNotFoundError, OSError) as exc:
        raise EvidenceBindingError("source_commit: cannot resolve HEAD") from exc
    head = out.strip()
    validate_git_object(repo_root, head)
    return head


def validate_identity(
    repo_root: str | Path,
    identity: dict[str, Any],
    build_binary_path: str | Path | None,
) -> None:
    missing = incomplete_fields(identity)
    errors: list[str] = [f"{f}: empty (need value or na reason)" for f in missing]

    sc = identity.get("source_commit") or {}
    if isinstance(sc, dict) and str(sc.get("value") or "").strip():
        try:
            validate_git_object(repo_root, str(sc["value"]))
        except EvidenceBindingError as exc:
            errors.append(str(exc))

    bd = identity.get("build_digest") or {}
    if isinstance(bd, dict) and str(bd.get("value") or "").strip():
        if not build_binary_path:
            errors.append(
                "build_digest: value present but no binary path was supplied "
                "for derivation check"
            )
        else:
            try:
                got = sha256_file(build_binary_path)
            except OSError as exc:
                errors.append(f"build_digest: hash binary: {exc}")
            else:
                want = str(bd["value"]).strip().lower()
                if got.lower() != want:
                    errors.append(
                        f"build_digest: value {bd['value']} does not match "
                        f"sha256 of {build_binary_path} ({got})"
                    )

    if errors:
        raise EvidenceBindingError(
            "receipt identity incomplete: " + "; ".join(errors)
        )


def path_holds_withdrawn(data: dict[str, Any] | None) -> bool:
    if not data:
        return False
    status = str(data.get("binding_status") or "").upper()
    if status == BINDING_WITHDRAWN:
        return True
    validity = str(data.get("validity") or "").upper()
    return "INVALIDATED" in validity or "WITHDRAWN" in validity


def _read_json_object(path: Path) -> dict[str, Any] | None:
    if not path.is_file():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise EvidenceBindingError(
            f"evidence write refused: cannot read existing {path}"
        )
    if not isinstance(data, dict):
        raise EvidenceBindingError(
            f"evidence write refused: existing {path} is not a JSON object"
        )
    return data


def _atomic_write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".merc-ev-", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(text)
            if not text.endswith("\n"):
                fh.write("\n")
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def write_bound_evidence(
    *,
    path: str | Path,
    payload: dict[str, Any],
    identity: dict[str, Any],
    repo_root: str | Path,
    build_binary_path: str | Path | None = None,
    authority_id: str = "",
    allow_withdrawn_write: bool = False,
) -> None:
    """Single write path. Refuses incomplete identity and sticky-withdrawal bypass."""
    dest = Path(path)
    if not payload:
        raise EvidenceBindingError("evidence write: empty payload")
    if is_job_contract_payload(payload):
        raise EvidenceBindingError(
            f"evidence write refused: {dest} looks like a job-result payload "
            "(closed job contract). Write the payload bytes unchanged and put "
            "binding_status in a sibling .binding.json sidecar — never inside "
            "the payload (DisallowUnknownFields would refuse it)."
        )
    validate_identity(repo_root, identity, build_binary_path)

    existing = _read_json_object(dest)
    if existing is not None and path_holds_withdrawn(existing):
        new_withdrawn = allow_withdrawn_write and path_holds_withdrawn(payload)
        if not new_withdrawn:
            if not (authority_id or "").strip():
                raise EvidenceBindingError(
                    f"evidence write refused: {dest} holds a withdrawn receipt; "
                    "refusing non-withdrawn overwrite without authority_id "
                    "(pass a new authority id to supersede deliberately)"
                )
            payload = dict(payload)
            payload["supersession_authority_id"] = authority_id.strip()

    out = dict(payload)
    out["producer_identity"] = identity
    if allow_withdrawn_write and path_holds_withdrawn(payload):
        out["binding_status"] = BINDING_WITHDRAWN
    else:
        out["binding_status"] = BINDING_BOUND
    out.pop("missing_identity_fields", None)

    text = json.dumps(out, indent=2, ensure_ascii=False) + "\n"
    _atomic_write(dest, text)


def default_bound_identity(
    repo_root: str | Path,
    *,
    harness_revision: str,
    build_binary_path: str | Path,
    exact_config: str = "embedded in receipt body",
    raw_samples: str = "embedded in receipt body",
    model_na: str = "no model weights in this measurement",
    image_na: str = "no container image in this measurement",
    corpus_na: str = "no external corpus in this measurement",
) -> dict[str, Any]:
    commit = resolve_source_commit(repo_root)
    digest = sha256_file(build_binary_path)
    if not (harness_revision or "").strip():
        raise EvidenceBindingError("harness_revision required")
    return {
        "source_commit": slot_value(commit),
        "build_digest": slot_value(digest),
        "model_artifact_digest": slot_na(model_na),
        "image_digest": slot_na(image_na),
        "harness_revision": slot_value(harness_revision),
        "corpus_digest": slot_na(corpus_na),
        "exact_config": slot_value(exact_config),
        "raw_samples": slot_value(raw_samples),
    }


def _lookup_nested(obj: dict[str, Any], key: str) -> Any:
    if key in obj and obj[key] not in (None, "", [], {}):
        return obj[key]
    for nest in ("scope", "config", "hardware", "input_fixture", "producer_identity"):
        nested = obj.get(nest)
        if isinstance(nested, dict) and key in nested and nested[key] not in (
            None,
            "",
            [],
            {},
        ):
            return nested[key]
    return None


def classify_existing_artifact(
    data: dict[str, Any],
    repo_root: str | Path,
) -> tuple[str, list[str]]:
    """Return (binding_status, missing_identity_fields) for a historical artifact.

    Does not invent digests. WITHDRAWN/SUPERSEDED take precedence over field checks.
    """
    existing_status = str(data.get("binding_status") or "").upper()
    if existing_status in {
        BINDING_BOUND,
        BINDING_UNBOUND,
        BINDING_SUPERSEDED,
        BINDING_WITHDRAWN,
    }:
        # Re-check BOUND claims so a forged status still fails later gates.
        if existing_status == BINDING_BOUND:
            missing = missing_fields_for_object(data, repo_root)
            if missing:
                return BINDING_UNBOUND, missing
        if existing_status == BINDING_UNBOUND:
            return BINDING_UNBOUND, list(data.get("missing_identity_fields") or missing_fields_for_object(data, repo_root))
        return existing_status, list(data.get("missing_identity_fields") or [])

    if path_holds_withdrawn(data):
        return BINDING_WITHDRAWN, []

    if data.get("superseded_by") or data.get("superseded") is True:
        return BINDING_SUPERSEDED, []

    missing = missing_fields_for_object(data, repo_root)
    if not missing:
        # Historical artifact that happens to carry all fields under legacy keys
        # is still not BOUND unless it has producer_identity — formal identity
        # is required. Treat as UNBOUND with empty missing only if formal block
        # is present and complete.
        pi = data.get("producer_identity")
        if isinstance(pi, dict) and not incomplete_fields(pi):
            # Still require source_commit git validity + note build_digest was
            # not re-derived at stamp time for historical files.
            return BINDING_BOUND, []
        # Has content under aliases but no formal block → still unbound.
        if missing_fields_for_object(data, repo_root, formal_only=False) == []:
            return BINDING_UNBOUND, ["producer_identity"]
    return BINDING_UNBOUND, missing


def missing_fields_for_object(
    data: dict[str, Any],
    repo_root: str | Path,
    *,
    formal_only: bool = False,
) -> list[str]:
    """List programme identity fields not satisfied by this object."""
    pi = data.get("producer_identity")
    if isinstance(pi, dict):
        missing = incomplete_fields(pi)
        sc = pi.get("source_commit") or {}
        if isinstance(sc, dict) and str(sc.get("value") or "").strip():
            try:
                validate_git_object(repo_root, str(sc["value"]))
            except EvidenceBindingError:
                if "source_commit" not in missing:
                    missing.append("source_commit")
        return missing

    if formal_only:
        return list(IDENTITY_FIELDS)

    missing: list[str] = []
    for field, keys in LEGACY_FIELD_KEYS.items():
        found = False
        for key in keys:
            val = _lookup_nested(data, key)
            if val is None:
                continue
            if field == "source_commit":
                try:
                    validate_git_object(repo_root, str(val))
                    found = True
                except EvidenceBindingError:
                    found = False
                break
            found = True
            break
        if not found:
            missing.append(field)
    return missing


# Metadata the binding layer owns. Never inject these into a job-result payload:
# those files are decoded with DisallowUnknownFields under their job contracts.
BINDING_META_KEYS = frozenset(
    {
        "binding_status",
        "missing_identity_fields",
        "producer_identity",
    }
)

# Closed worker-committed result envelopes. A receipt describes a measurement
# (kind/schema_version/assertions); a payload is the measured bytes themselves.
_JOB_RESULT_PAYLOAD_MARKERS: dict[str, frozenset[str]] = {
    "embed": frozenset({"job_type", "model", "dim", "count", "vectors"}),
    "batch_infer": frozenset({"job_type", "model", "completions"}),
}


def is_job_contract_payload(data: Any) -> bool:
    """True when this JSON object is a job-result payload, not an evidence receipt.

    Job-result payloads are governed by the control-plane result contract
    (decodeStrictJSON / DisallowUnknownFields). Stamping binding_status into
    them breaks decoding. Binding for these files lives in a sibling
    ``<name>.binding.json`` sidecar, same as for .jsonl/.txt.
    """
    if not isinstance(data, dict):
        return False
    # Explicit measurement receipts always declare kind and/or schema_version.
    if data.get("kind") is not None or data.get("schema_version") is not None:
        return False
    job = data.get("job_type")
    if not isinstance(job, str):
        return False
    required = _JOB_RESULT_PAYLOAD_MARKERS.get(job)
    if required is None:
        return False
    keys = set(data.keys()) - BINDING_META_KEYS
    if not required.issubset(keys):
        return False
    # Payload envelopes are closed: only the contract fields (plus any binding
    # meta that a bad stamp may have left behind, which we ignore above).
    return keys <= (required | {"inference_backend"})


def strip_binding_meta(data: dict[str, Any]) -> dict[str, Any]:
    """Return a shallow copy without binding-layer keys."""
    return {k: v for k, v in data.items() if k not in BINDING_META_KEYS}


def binding_sidecar_path(path: str | Path) -> Path:
    return Path(str(path) + ".binding.json")


def stamp_binding_fields(
    data: dict[str, Any],
    status: str,
    missing: list[str],
) -> dict[str, Any]:
    """Return a copy with binding_status (+ missing list) added; no other changes.

    Do not call this for job-contract payloads — use a sidecar instead.
    """
    if is_job_contract_payload(data):
        raise EvidenceBindingError(
            "job-contract payload must not receive in-object binding fields; "
            "write a .binding.json sidecar"
        )
    out = dict(data)
    out["binding_status"] = status
    if status == BINDING_UNBOUND:
        out["missing_identity_fields"] = list(missing)
    else:
        # Keep missing list only for UNBOUND; remove stale lists on other statuses.
        if "missing_identity_fields" in out and status != BINDING_UNBOUND:
            # WITHDRAWN/SUPERSEDED/BOUND do not carry a repair checklist.
            if status in (BINDING_BOUND, BINDING_WITHDRAWN, BINDING_SUPERSEDED):
                out.pop("missing_identity_fields", None)
    return out


def emit_bound_json(
    path: str | Path,
    payload: dict[str, Any],
    *,
    harness: str,
    repo_root: str | Path | None = None,
    build_binary_path: str | Path | None = None,
    authority_id: str = "",
    exact_config: str = "embedded in receipt body",
    raw_samples: str = "embedded in receipt body",
    model_digest: str = "",
    model_na: str = "no model weights in this measurement",
    image_digest: str = "",
    image_na: str = "no container image in this measurement",
    corpus_digest: str = "",
    corpus_na: str = "no external corpus in this measurement",
) -> None:
    """Convenience writer for Python harnesses. Refuses incomplete identity."""
    root = Path(repo_root) if repo_root else Path(__file__).resolve().parents[2]
    build = Path(build_binary_path) if build_binary_path else Path(harness)
    if not build.is_file():
        # Fall back to this library file so digests are still derived, not typed.
        build = Path(__file__).resolve()
    identity = default_bound_identity(
        root,
        harness_revision=harness,
        build_binary_path=build,
        exact_config=exact_config,
        raw_samples=raw_samples,
        model_na=model_na,
        image_na=image_na,
        corpus_na=corpus_na,
    )
    if model_digest:
        identity["model_artifact_digest"] = slot_value(model_digest)
    if image_digest:
        identity["image_digest"] = slot_value(image_digest)
    if corpus_digest:
        identity["corpus_digest"] = slot_value(corpus_digest)
    write_bound_evidence(
        path=path,
        payload=payload,
        identity=identity,
        repo_root=root,
        build_binary_path=build,
        authority_id=authority_id,
    )
