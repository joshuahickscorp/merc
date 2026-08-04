#!/usr/bin/env python3
"""Fail closed when a Python or TypeScript SDK path is not registered in control/api.go.

Client-server path drift (e.g. TypeScript calling GET /v1/estimate while the
control plane registers GET /v1/price-estimate) must not be able to ship again.
Reuses the same mux.Handle registration parse as validate-authorization-matrix.py.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
API = ROOT / "control" / "api.go"
PY_SDK = ROOT / "clients" / "sdk" / "python" / "merc" / "__init__.py"
TS_SDK = ROOT / "clients" / "sdk" / "typescript" / "src" / "index.ts"

ROUTE_RE = re.compile(r'mux\.Handle(?:Func)?\("((?:GET|POST|DELETE) [^"]+)"')
# Python: self._request("GET", "/v1/...") or f"/v1/jobs/{job_id}"
PY_CALL_RE = re.compile(
    r'_request\(\s*["\'](GET|POST|DELETE)["\']\s*,\s*(?:f)?["\']([^"\']+)["\']'
)
# TypeScript: this.#request("GET", `/v1/...` or "/v1/...")
TS_CALL_RE = re.compile(
    r'#request(?:<[^>]+>)?\(\s*["\'](GET|POST|DELETE)["\']\s*,\s*(?:`([^`]+)`|["\']([^"\']+)["\'])'
)


def fail(message: str) -> None:
    print(f"sdk routes: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def registered_routes() -> set[str]:
    source = API.read_text()
    routes = ROUTE_RE.findall(source)
    if not routes:
        fail("control/api.go has no mux.Handle registrations")
    if len(routes) != len(set(routes)):
        fail("control/api.go registers a duplicate method/path")
    return set(routes)


def normalize_sdk_path(path: str) -> str:
    """Turn SDK path templates into Go ServeMux patterns for comparison."""
    # Strip query string if any slipped in.
    path = path.split("?", 1)[0]
    # Python/TS interpolate with {job_id}, ${jobId}, etc. → {id} style segments
    # used by control/api.go ({id}, {path...}, {name}, ...).
    path = re.sub(r"\{[^}]+\}", "{id}", path)
    path = re.sub(r"\$\{[^}]+\}", "{id}", path)
    # encodeURIComponent(...) wrappers in template strings leave bare ${...}
    path = re.sub(r"\$\{encodeURIComponent\([^)]+\)\}", "{id}", path)
    return path


def route_matches(method: str, sdk_path: str, registered: set[str]) -> bool:
    candidate = f"{method} {normalize_sdk_path(sdk_path)}"
    if candidate in registered:
        return True
    # ServeMux wildcards: {path...} and multi-segment params.
    for route in registered:
        if not route.startswith(method + " "):
            continue
        pattern = route[len(method) + 1 :]
        # Convert {id} and {path...} into regex.
        rx = "^"
        i = 0
        while i < len(pattern):
            if pattern[i] == "{":
                end = pattern.find("}", i)
                if end < 0:
                    break
                name = pattern[i + 1 : end]
                if name.endswith("..."):
                    rx += ".+"
                else:
                    rx += "[^/]+"
                i = end + 1
            else:
                rx += re.escape(pattern[i])
                i += 1
        rx += "$"
        if re.match(rx, normalize_sdk_path(sdk_path)):
            return True
    return False


def collect_python(registered: set[str]) -> list[str]:
    text = PY_SDK.read_text()
    missing = []
    for method, path in PY_CALL_RE.findall(text):
        if not path.startswith("/"):
            continue
        if not route_matches(method, path, registered):
            missing.append(f"python {method} {path}")
    return missing


def collect_typescript(registered: set[str]) -> list[str]:
    text = TS_SDK.read_text()
    missing = []
    for method, tmpl, plain in TS_CALL_RE.findall(text):
        path = tmpl or plain
        if not path.startswith("/"):
            continue
        if not route_matches(method, path, registered):
            missing.append(f"typescript {method} {path}")
    return missing


def main() -> None:
    registered = registered_routes()
    missing = collect_python(registered) + collect_typescript(registered)
    if missing:
        fail(
            "SDK path(s) not registered in control/api.go:\n  - "
            + "\n  - ".join(missing)
        )
    # Explicit regression pin for the known drift that shipped.
    if not route_matches("GET", "/v1/price-estimate", registered):
        fail("control plane is missing GET /v1/price-estimate")
    ts = TS_SDK.read_text()
    if '"/v1/estimate"' in ts or "`/v1/estimate`" in ts:
        fail("TypeScript SDK still calls /v1/estimate; registered path is /v1/price-estimate")
    if "/v1/price-estimate" not in ts:
        fail("TypeScript SDK does not call /v1/price-estimate")
    py = PY_SDK.read_text()
    if "/v1/price-estimate" not in py:
        fail("Python SDK does not call /v1/price-estimate")
    print(
        f"sdk routes: PASS "
        f"({len(registered)} registered routes; python+typescript paths resolve)"
    )


if __name__ == "__main__":
    main()
