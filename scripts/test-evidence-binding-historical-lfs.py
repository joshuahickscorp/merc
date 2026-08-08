#!/usr/bin/env python3
"""Hostile tests for immutable historical Git-LFS binding envelopes.

The envelope is intentionally a very narrow exception to in-body binding:
it may classify an old LFS JSON body as UNBOUND only while the body has no
binding_status and its path, indexed pointer oid+size, and hydrated SHA-256 all
match.  These fixtures prove each component is fail-closed without touching a
real historical receipt.
"""

from __future__ import annotations

import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
VALIDATOR = ROOT / "scripts" / "validate-evidence-binding.py"
LIBRARY = ROOT / "scripts" / "lib" / "evidence_binding.py"
TARGET_REL = "evidence/perf/legacy-latest.json"
MISSING = [
    "source_commit",
    "build_digest",
    "model_artifact_digest",
    "image_digest",
    "harness_revision",
    "corpus_digest",
    "exact_config",
]


def canonical_body(extra: dict | None = None) -> bytes:
    value = {
        "kind": "historical_lfs_fixture",
        "raw_samples": [{"retained": True}],
    }
    if extra:
        value.update(extra)
    return (json.dumps(value, sort_keys=True) + "\n").encode("utf-8")


def pointer_for(body: bytes) -> tuple[str, int, bytes]:
    oid = hashlib.sha256(body).hexdigest()
    pointer = (
        "version https://git-lfs.github.com/spec/v1\n"
        f"oid sha256:{oid}\n"
        f"size {len(body)}\n"
    ).encode("utf-8")
    return oid, len(body), pointer


def envelope(body: bytes, *, source_commit: str, target: str = TARGET_REL) -> dict:
    oid, size, _ = pointer_for(body)
    return {
        "schema_version": 1,
        "kind": "historical_lfs_binding_envelope",
        "append_only": True,
        "historical_source_commit": source_commit,
        "target_path": target,
        "indexed_lfs_oid_sha256": oid,
        "indexed_lfs_size_bytes": size,
        "hydrated_body_sha256": oid,
        "binding_status": "UNBOUND",
        "missing_identity_fields": MISSING,
        "reason": "fixture historical body is intentionally unbound",
    }


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def stage_pointer(root: Path, body: bytes) -> None:
    target = root / TARGET_REL
    target.parent.mkdir(parents=True, exist_ok=True)
    _, _, pointer = pointer_for(body)
    target.write_bytes(pointer)
    subprocess.run(["git", "-C", str(root), "add", TARGET_REL], check=True)
    target.write_bytes(body)  # hydrated working tree; index remains pointer.


def commit_indexed_pointer(root: Path, message: str) -> str:
    subprocess.run(["git", "-C", str(root), "config", "user.email", "fixture@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(root), "config", "user.name", "Fixture"], check=True)
    subprocess.run(["git", "-C", str(root), "commit", "-qm", message], check=True)
    return subprocess.check_output(
        ["git", "-C", str(root), "rev-parse", "HEAD"], text=True
    ).strip()


def fixture(parent: Path) -> Path:
    root = parent / "repo"
    (root / "scripts" / "lib").mkdir(parents=True)
    shutil.copy2(VALIDATOR, root / "scripts" / VALIDATOR.name)
    shutil.copy2(LIBRARY, root / "scripts" / "lib" / LIBRARY.name)
    subprocess.run(["git", "init", "-q", str(root)], check=True)

    body = canonical_body()
    stage_pointer(root, body)
    source_commit = commit_indexed_pointer(root, "historical pointer")
    write_json(
        Path(str(root / TARGET_REL) + ".binding.json"),
        envelope(body, source_commit=source_commit),
    )
    return root


def run_validator(root: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "scripts/validate-evidence-binding.py"],
        cwd=root,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )


def update_sidecar(root: Path, mutate) -> None:
    path = Path(str(root / TARGET_REL) + ".binding.json")
    data = json.loads(path.read_text(encoding="utf-8"))
    mutate(data)
    write_json(path, data)


def expect_pass(root: Path, label: str) -> None:
    result = run_validator(root)
    if result.returncode != 0:
        raise AssertionError(f"{label}: expected PASS, got:\n{result.stdout}")
    print(f"historical-lfs-envelope: PASS {label}")


def expect_refusal(root: Path, label: str, fragment: str) -> None:
    result = run_validator(root)
    if result.returncode == 0:
        raise AssertionError(f"{label}: envelope forgery unexpectedly passed")
    if fragment not in result.stdout:
        raise AssertionError(
            f"{label}: refusal did not name {fragment!r}:\n{result.stdout}"
        )
    print(f"historical-lfs-envelope: PASS refused {label}")


def replace_body_and_index(root: Path, body: bytes) -> None:
    # Re-stage a new pointer as a candidate repoint would, then hydrate it.
    stage_pointer(root, body)


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="merc-evidence-envelope-") as raw:
        parent = Path(raw)

        root = fixture(parent / "baseline")
        expect_pass(root, "exact immutable UNBOUND envelope")

        root = fixture(parent / "wrong-path")
        update_sidecar(root, lambda d: d.__setitem__("target_path", "evidence/perf/other.json"))
        expect_refusal(root, "forged target path", "target_path must equal")

        root = fixture(parent / "wrong-oid")
        update_sidecar(root, lambda d: d.__setitem__("indexed_lfs_oid_sha256", "0" * 64))
        expect_refusal(root, "forged indexed oid", "indexed_lfs_oid_sha256")

        root = fixture(parent / "wrong-size")
        update_sidecar(root, lambda d: d.__setitem__("indexed_lfs_size_bytes", 1))
        expect_refusal(root, "forged indexed size", "indexed_lfs_size_bytes")

        root = fixture(parent / "tampered-body")
        (root / TARGET_REL).write_bytes(canonical_body({"tampered": True}))
        expect_refusal(root, "tampered hydrated body", "hydrated body")

        root = fixture(parent / "repointed-index")
        replace_body_and_index(root, canonical_body({"repointed": True}))
        expect_refusal(root, "repointed indexed pointer", "indexed_lfs_oid_sha256")

        root = fixture(parent / "repointed-with-sidecar")
        repointed = canonical_body({"repointed": True})
        replace_body_and_index(root, repointed)
        oid, size, _ = pointer_for(repointed)
        update_sidecar(root, lambda d: d.update({
            "indexed_lfs_oid_sha256": oid,
            "indexed_lfs_size_bytes": size,
            "hydrated_body_sha256": oid,
        }))
        expect_refusal(
            root,
            "coordinated pointer and sidecar repoint",
            "historical source pointer",
        )

        root = fixture(parent / "upgraded-sidecar")
        update_sidecar(root, lambda d: d.__setitem__("binding_status", "BOUND"))
        expect_refusal(root, "sidecar status upgrade", "may only declare binding_status=UNBOUND")

        root = fixture(parent / "inbody-wins")
        body = canonical_body({"binding_status": "BOUND"})
        replace_body_and_index(root, body)
        source_commit = commit_indexed_pointer(root, "in-body status pointer")
        write_json(
            Path(str(root / TARGET_REL) + ".binding.json"),
            envelope(body, source_commit=source_commit),
        )
        expect_refusal(root, "in-body status cannot be overridden", "producer_identity missing")

        root = fixture(parent / "claim-citation")
        claim = root / "docs" / "claim.md"
        claim.parent.mkdir(parents=True)
        claim.write_text(f"This claims {TARGET_REL}.\n", encoding="utf-8")
        expect_refusal(root, "UNBOUND historical receipt cited", "UNBOUND yet cited")

    print("historical-lfs-envelope: PASS all hostile cases")
    return 0


if __name__ == "__main__":
    sys.exit(main())
