#!/usr/bin/env python3
"""Validate one encrypted offsite-backup verification receipt.

This binds the local encrypted bytes and manifest to the exact offsite URI and
fresh invocation window. It does not perform provider I/O; backup.sh performs
the upload and independent download, then this validator prevents a stale,
mixed, or self-asserted receipt from entering rollback authority.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit
_SCRIPTS = Path(__file__).resolve().parent
if str(_SCRIPTS) not in sys.path:
    sys.path.insert(0, str(_SCRIPTS))

from lib.receipt_json import (
    all_strings,
    exact_keys,
    fail,
    finite_numbers,
    object_without_duplicate_keys,
    parse_utc,
    reject_constant,
    sha256_file,
)


BACKUP_ID = re.compile(r"^[0-9]{8}T[0-9]{6}Z$")
SHA256 = re.compile(r"^[0-9a-f]{64}$")
SAFE_DATABASE = re.compile(r"^[A-Za-z_][A-Za-z0-9_-]{0,62}$")
SECRET = re.compile(
    r"(sk_(?:test|live)_|rk_(?:test|live)_|pk_live_|whsec_|"
    r"AGE-SECRET-KEY-|AKIA[0-9A-Z]{12,})",
    re.IGNORECASE,
)
MAX_JSON_BYTES = 1_048_576



def load_json(path: Path, label: str):
    if path.stat().st_size <= 0 or path.stat().st_size > MAX_JSON_BYTES:
        fail(f"{label} is empty or exceeds the 1 MiB bound")
    try:
        return json.loads(
            path.read_text(encoding="utf-8"),
            object_pairs_hook=object_without_duplicate_keys,
            parse_constant=reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(f"{label} is not strict UTF-8 JSON: {exc}")


def validate(manifest, receipt, ciphertext: Path, args) -> None:
    exact_keys(
        manifest,
        {
            "schema_version",
            "kind",
            "backup_id",
            "cipher",
            "ciphertext_sha256",
            "ciphertext_bytes",
            "created_at",
            "database",
            "objects_included",
            "offsite_uri",
        },
        "manifest",
    )
    if (
        type(manifest["schema_version"]) is not int
        or manifest["schema_version"] != 2
        or manifest["kind"] != "merc_encrypted_offsite_backup"
        or manifest["cipher"] != "age-x25519"
    ):
        fail("manifest identity or cipher is invalid")
    backup_id = manifest["backup_id"]
    if not isinstance(backup_id, str) or not BACKUP_ID.fullmatch(backup_id):
        fail("manifest.backup_id is invalid")
    if not SAFE_DATABASE.fullmatch(manifest["database"] or ""):
        fail("manifest.database is invalid")
    if type(manifest["objects_included"]) is not bool:
        fail("manifest.objects_included must be boolean")
    if not SHA256.fullmatch(manifest["ciphertext_sha256"] or ""):
        fail("manifest.ciphertext_sha256 is invalid")
    if (
        isinstance(manifest["ciphertext_bytes"], bool)
        or not isinstance(manifest["ciphertext_bytes"], int)
        or manifest["ciphertext_bytes"] <= 0
    ):
        fail("manifest.ciphertext_bytes must be positive")

    offsite = urlsplit(args.offsite_base)
    if (
        offsite.scheme != "s3"
        or not offsite.netloc
        or any(character.isspace() for character in args.offsite_base)
        or offsite.query
        or offsite.fragment
        or offsite.username
        or offsite.password
        or ".." in Path(offsite.path).parts
    ):
        fail("expected offsite base must be a credential-free s3:// URI")
    expected_uri = args.offsite_base.rstrip("/") + "/" + backup_id
    if manifest["offsite_uri"] != expected_uri:
        fail("manifest.offsite_uri is not the exact expected backup location")
    if ciphertext.stat().st_size != manifest["ciphertext_bytes"]:
        fail("local ciphertext size does not match the manifest")
    ciphertext_sha = sha256_file(ciphertext)
    if ciphertext_sha != manifest["ciphertext_sha256"]:
        fail("local ciphertext SHA-256 does not match the manifest")

    exact_keys(
        receipt,
        {
            "schema_version",
            "kind",
            "status",
            "backup_id",
            "offsite_uri",
            "manifest_sha256",
            "ciphertext",
            "verified_at",
            "checks",
            "policy",
        },
        "verification receipt",
    )
    if (
        type(receipt["schema_version"]) is not int
        or receipt["schema_version"] != 1
        or receipt["kind"] != "merc_offsite_backup_verification"
        or receipt["status"] != "PASS"
        or receipt["backup_id"] != backup_id
        or receipt["offsite_uri"] != expected_uri
    ):
        fail("verification receipt identity or binding is invalid")
    manifest_sha = sha256_file(Path(args.manifest))
    if (
        not SHA256.fullmatch(receipt["manifest_sha256"] or "")
        or receipt["manifest_sha256"] != manifest_sha
    ):
        fail("verification receipt does not bind the local manifest bytes")

    exact_keys(
        receipt["ciphertext"],
        {"manifest_sha256", "downloaded_sha256", "bytes"},
        "verification receipt ciphertext",
    )
    if (
        receipt["ciphertext"]["manifest_sha256"] != ciphertext_sha
        or receipt["ciphertext"]["downloaded_sha256"] != ciphertext_sha
        or receipt["ciphertext"]["bytes"] != manifest["ciphertext_bytes"]
    ):
        fail("verification receipt does not bind an identical downloaded ciphertext")

    expected_checks = {
        "offsite_bundle_visible": True,
        "independent_manifest_download": True,
        "independent_ciphertext_download": True,
        "manifest_checksum_match": True,
        "ciphertext_checksum_match": True,
    }
    if receipt["checks"] != expected_checks:
        fail("verification receipt checks are incomplete")
    expected_policy = {
        "encrypted_before_upload": True,
        "plaintext_uploaded": False,
        "secret_values_recorded": False,
    }
    if receipt["policy"] != expected_policy:
        fail("verification receipt policy is invalid")

    not_before = parse_utc(args.not_before, "expected not-before")
    checked_at = parse_utc(args.checked_at, "expected checked-at")
    created_at = parse_utc(manifest["created_at"], "manifest.created_at")
    verified_at = parse_utc(receipt["verified_at"], "receipt.verified_at")
    backup_started_at = dt.datetime.strptime(
        backup_id, "%Y%m%dT%H%M%SZ"
    ).replace(tzinfo=dt.timezone.utc)
    if (
        backup_started_at < not_before
        or created_at < backup_started_at
        or verified_at < created_at
        or verified_at > checked_at
        or checked_at < not_before
    ):
        fail("backup creation or verification falls outside this invocation window")

    if any(SECRET.search(value) for value in all_strings(manifest)):
        fail("manifest contains a secret-shaped value")
    if any(SECRET.search(value) for value in all_strings(receipt)):
        fail("verification receipt contains a secret-shaped value")
    if not finite_numbers(manifest) or not finite_numbers(receipt):
        fail("backup evidence contains a non-finite number")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest")
    parser.add_argument("receipt")
    parser.add_argument("--ciphertext", required=True)
    parser.add_argument("--offsite-base", required=True)
    parser.add_argument("--not-before", required=True)
    parser.add_argument("--checked-at", required=True)
    args = parser.parse_args()
    try:
        manifest_path = Path(args.manifest)
        receipt_path = Path(args.receipt)
        ciphertext = Path(args.ciphertext)
        for path, label in (
            (manifest_path, "manifest"),
            (receipt_path, "verification receipt"),
            (ciphertext, "ciphertext"),
        ):
            if not path.is_file() or path.is_symlink():
                fail(f"{label} must be a regular non-symlink file")
        manifest = load_json(manifest_path, "manifest")
        receipt = load_json(receipt_path, "verification receipt")
        validate(manifest, receipt, ciphertext, args)
    except (OSError, TypeError, ValueError) as exc:
        print(f"backup-verification-receipt: FAIL: {exc}", file=sys.stderr)
        return 1
    print(
        "backup-verification-receipt: PASS "
        "(fresh manifest, exact offsite URI, identical downloaded ciphertext)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
