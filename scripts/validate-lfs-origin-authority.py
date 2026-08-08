#!/usr/bin/env python3
"""Normalize a Git remote and require the reviewed LFS durability authority.

The remote name ``origin`` is not an authority boundary: a local config can
point it at any public repository.  This helper accepts only the repository
identity reviewed in .github/lfs-origin-authority.json, across conventional
HTTPS and SSH Git URL spellings.  It performs no network I/O.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[1]
POLICY = ROOT / ".github" / "lfs-origin-authority.json"
SCP_SSH_RE = re.compile(r"^(?:[^@/:\s]+@)?(?P<host>[^/:\s]+):(?P<path>[^\s]+)$")


class AuthorityError(ValueError):
    pass


def normalized_path(raw: str) -> tuple[str, str]:
    path = raw.strip().strip("/")
    if path.endswith(".git"):
        path = path[:-4]
    parts = path.split("/")
    if len(parts) != 2 or any(not p or p in {".", ".."} for p in parts):
        raise AuthorityError("repository path must be exactly owner/repository")
    return parts[0].lower(), parts[1].lower()


def normalize_remote(url: str) -> tuple[str, str]:
    """Return (transport, host/owner/repository) without trusting the name."""
    raw = (url or "").strip()
    if not raw or any(c in raw for c in "\r\n\t"):
        raise AuthorityError("remote URL is empty or malformed")

    # Do not let the colon after an ordinary URL scheme masquerade as the
    # host:path separator of scp-style SSH syntax.
    scp = SCP_SSH_RE.fullmatch(raw) if "://" not in raw else None
    if scp:
        owner, repo = normalized_path(scp.group("path"))
        return "ssh", f"{scp.group('host').lower()}/{owner}/{repo}"

    parsed = urlsplit(raw)
    scheme = parsed.scheme.lower()
    if scheme not in {"https", "ssh"}:
        raise AuthorityError("remote transport is not reviewed HTTPS or SSH")
    if not parsed.hostname or parsed.query or parsed.fragment:
        raise AuthorityError("remote URL has no canonical host/path")
    try:
        port = parsed.port
    except ValueError as exc:
        raise AuthorityError("remote URL has an invalid port") from exc
    default_port = 443 if scheme == "https" else 22
    if port is not None and port != default_port:
        raise AuthorityError("remote URL uses a noncanonical port")
    owner, repo = normalized_path(parsed.path)
    return scheme, f"{parsed.hostname.lower()}/{owner}/{repo}"


def normalize_lfs_endpoint(url: str) -> str:
    """Return a credential-free canonical HTTPS LFS endpoint identity."""
    raw = (url or "").strip()
    parsed = urlsplit(raw)
    if (
        parsed.scheme.lower() != "https"
        or not parsed.hostname
        or parsed.query
        or parsed.fragment
        or parsed.username
        or parsed.password
    ):
        raise AuthorityError("LFS endpoint is not a canonical credential-free HTTPS URL")
    try:
        port = parsed.port
    except ValueError as exc:
        raise AuthorityError("LFS endpoint has an invalid port") from exc
    if port is not None and port != 443:
        raise AuthorityError("LFS endpoint uses a noncanonical port")
    path = parsed.path.rstrip("/")
    if not path.endswith(".git/info/lfs"):
        raise AuthorityError("LFS endpoint path must end in .git/info/lfs")
    repo_path = path[: -len("/info/lfs")]
    owner, repo = normalized_path(repo_path)
    return f"https://{parsed.hostname.lower()}/{owner}/{repo}.git/info/lfs"


def load_policy() -> tuple[str, set[str], str]:
    try:
        data = json.loads(POLICY.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise AuthorityError("reviewed origin authority policy is unreadable") from exc
    if not isinstance(data, dict) or set(data) != {
        "schema_version", "kind", "canonical_repository", "canonical_lfs_endpoint", "allowed_transports"
    }:
        raise AuthorityError("reviewed origin authority policy has an invalid schema")
    if data.get("schema_version") != 1 or data.get("kind") != "lfs_origin_authority":
        raise AuthorityError("reviewed origin authority policy has an unsupported version")
    canonical = data.get("canonical_repository")
    lfs_endpoint = data.get("canonical_lfs_endpoint")
    transports = data.get("allowed_transports")
    if not isinstance(canonical, str) or not isinstance(lfs_endpoint, str) or not isinstance(transports, list):
        raise AuthorityError("reviewed origin authority policy has invalid values")
    policy_transport, normalized = normalize_remote("https://" + canonical)
    if policy_transport != "https":  # defensive: prefix above must make this true.
        raise AuthorityError("reviewed origin authority policy canonical identity is invalid")
    allowed = {str(t).lower() for t in transports}
    if not allowed or not allowed <= {"https", "ssh"}:
        raise AuthorityError("reviewed origin authority policy has invalid transports")
    endpoint = normalize_lfs_endpoint(lfs_endpoint)
    if endpoint.split("/", 3)[2] + "/" + "/".join(endpoint.split("/", 3)[3].split("/")[:2]).removesuffix(".git") != normalized:
        raise AuthorityError("reviewed LFS endpoint does not name the canonical repository")
    return normalized, allowed, endpoint


def require_authorized_remote(url: str) -> str:
    transport, identity = normalize_remote(url)
    canonical, transports, _ = load_policy()
    if transport not in transports or identity != canonical:
        raise AuthorityError("remote does not match reviewed repository authority")
    return identity


def expected_lfs_endpoint() -> str:
    _, _, endpoint = load_policy()
    return endpoint


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", help="candidate Git remote URL")
    parser.add_argument("--lfs-endpoint", help="candidate resolved LFS endpoint")
    parser.add_argument(
        "--expected-lfs-endpoint",
        action="store_true",
        help="print the reviewed canonical LFS endpoint",
    )
    args = parser.parse_args()
    if sum(bool(value) for value in (args.url, args.lfs_endpoint, args.expected_lfs_endpoint)) != 1:
        parser.error("provide exactly one of --url, --lfs-endpoint, or --expected-lfs-endpoint")
    try:
        if args.expected_lfs_endpoint:
            identity = expected_lfs_endpoint()
        elif args.url:
            identity = require_authorized_remote(args.url)
        else:
            candidate = normalize_lfs_endpoint(args.lfs_endpoint)
            expected = expected_lfs_endpoint()
            if candidate != expected:
                raise AuthorityError("LFS endpoint does not match reviewed repository authority")
            identity = expected
    except AuthorityError as exc:
        # Never echo a URL here: it can contain an HTTPS credential.
        print(f"lfs-origin-authority: FAIL -- {exc}", file=sys.stderr)
        return 1
    print(identity)
    return 0


if __name__ == "__main__":
    sys.exit(main())
