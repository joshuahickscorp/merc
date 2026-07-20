#!/usr/bin/env python3
"""Fail closed when the registered HTTP surface and reviewed auth matrix diverge."""

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
API = ROOT / "control" / "api.go"
MATRIX = ROOT / "ops" / "authorization-matrix.json"
ROUTE_RE = re.compile(r'mux\.Handle(?:Func)?\("((?:GET|POST|DELETE) [^"]+)"')
ROLES = {
    "anonymous",
    "buyer_owner",
    "different_buyer",
    "active_worker",
    "different_worker",
    "operator",
    "revoked_identity",
    "provider_hmac",
}


def fail(message: str) -> None:
    print(f"authorization matrix: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


document = json.loads(MATRIX.read_text())
if document.get("policy", {}).get("default") != "deny":
    fail("default policy must be deny")
if set(document.get("roles", [])) != ROLES:
    fail("role axis is incomplete or contains an unreviewed role")

source = API.read_text()
registered = ROUTE_RE.findall(source)
if len(registered) != len(set(registered)):
    fail("control/api.go registers a duplicate method/path")

reviewed: dict[str, str] = {}
for route_class in document.get("route_classes", []):
    class_id = route_class.get("id")
    wrapper = route_class.get("registration")
    if set(route_class.get("role_decisions", {})) != ROLES:
        fail(f"{class_id} does not decide every role")
    if not route_class.get("enforcement"):
        fail(f"{class_id} has no enforcement description")
    for route in route_class.get("routes", []):
        if route in reviewed:
            fail(f"{route} appears in both {reviewed[route]} and {class_id}")
        reviewed[route] = class_id
        escaped = re.escape(route)
        if wrapper == "HandleFunc":
            pattern = rf'mux\.HandleFunc\("{escaped}",'
        elif wrapper in {"authBuyer", "authWorker", "authAdmin"}:
            pattern = rf'mux\.Handle\("{escaped}", s\.{wrapper}\('
        else:
            fail(f"{class_id} uses unknown registration {wrapper!r}")
        if re.search(pattern, source) is None:
            fail(f"{route} is not registered through reviewed wrapper {wrapper}")

missing = sorted(set(registered) - set(reviewed))
stale = sorted(set(reviewed) - set(registered))
if missing or stale:
    fail(f"coverage mismatch missing={missing} stale={stale}")
if len(reviewed) != 70:
    fail(f"expected reviewed 70-route surface, found {len(reviewed)}")

print(f"authorization matrix: PASS ({len(reviewed)} routes, {len(ROLES)} roles, default deny)")
