#!/usr/bin/env python3
"""Stdlib AWS SigV4 presigned PUT for S3-compatible stores (Cloudflare R2).

Prints one URL and nothing else. Credentials come from the environment and
are never written to stdout. Used so the droplet can upload ciphertext
without receiving a long-lived access key.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import hmac
import os
import sys
import urllib.parse


def _sign(key: bytes, msg: str) -> bytes:
    return hmac.new(key, msg.encode("utf-8"), hashlib.sha256).digest()


def _signing_key(secret: str, datestamp: str, region: str, service: str) -> bytes:
    k_date = _sign(("AWS4" + secret).encode("utf-8"), datestamp)
    k_region = hmac.new(k_date, region.encode("utf-8"), hashlib.sha256).digest()
    k_service = hmac.new(k_region, service.encode("utf-8"), hashlib.sha256).digest()
    return hmac.new(k_service, b"aws4_request", hashlib.sha256).digest()


def presign_put(
    *,
    endpoint: str,
    bucket: str,
    key: str,
    access_key: str,
    secret_key: str,
    region: str,
    expires: int,
) -> str:
    endpoint = endpoint.rstrip("/")
    host = urllib.parse.urlparse(endpoint).netloc
    if not host:
        raise SystemExit("r2-presign-put: endpoint has no host")
    now = dt.datetime.now(dt.timezone.utc)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    datestamp = now.strftime("%Y%m%d")
    credential_scope = f"{datestamp}/{region}/s3/aws4_request"
    credential = f"{access_key}/{credential_scope}"
    canonical_uri = "/" + bucket + "/" + "/".join(
        urllib.parse.quote(part, safe="-._~") for part in key.split("/")
    )
    query = {
        "X-Amz-Algorithm": "AWS4-HMAC-SHA256",
        "X-Amz-Credential": credential,
        "X-Amz-Date": amz_date,
        "X-Amz-Expires": str(expires),
        "X-Amz-SignedHeaders": "host",
    }
    canonical_query = "&".join(
        f"{urllib.parse.quote(k, safe='-_.~')}={urllib.parse.quote(v, safe='-_.~')}"
        for k, v in sorted(query.items())
    )
    canonical_headers = f"host:{host}\n"
    canonical_request = "\n".join(
        [
            "PUT",
            canonical_uri,
            canonical_query,
            canonical_headers,
            "host",
            "UNSIGNED-PAYLOAD",
        ]
    )
    string_to_sign = "\n".join(
        [
            "AWS4-HMAC-SHA256",
            amz_date,
            credential_scope,
            hashlib.sha256(canonical_request.encode("utf-8")).hexdigest(),
        ]
    )
    signature = hmac.new(
        _signing_key(secret_key, datestamp, region, "s3"),
        string_to_sign.encode("utf-8"),
        hashlib.sha256,
    ).hexdigest()
    return f"{endpoint}{canonical_uri}?{canonical_query}&X-Amz-Signature={signature}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--bucket", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--region", default=os.environ.get("AWS_DEFAULT_REGION", "auto"))
    parser.add_argument("--expires", type=int, default=1800)
    args = parser.parse_args()
    access = os.environ.get("AWS_ACCESS_KEY_ID", "")
    secret = os.environ.get("AWS_SECRET_ACCESS_KEY", "")
    if not access or not secret:
        print("r2-presign-put: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY unset", file=sys.stderr)
        return 2
    if args.expires < 60 or args.expires > 3600:
        print("r2-presign-put: expires must be 60..3600", file=sys.stderr)
        return 2
    print(
        presign_put(
            endpoint=args.endpoint,
            bucket=args.bucket,
            key=args.key,
            access_key=access,
            secret_key=secret,
            region=args.region,
            expires=args.expires,
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
