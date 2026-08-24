#!/usr/bin/env python3
"""Regenerate evidence/autonomous/website-validation.json.

Runs the commands the receipt already named: `node scripts/site-build.mjs`,
then Playwright against a loopback static server using the installed
Google Chrome executable. Stamps source_commit as the last write.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import threading
from datetime import datetime, timezone
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from lib.receipt_binding import head_commit, stamp  # noqa: E402

OUT = ROOT / "evidence" / "autonomous" / "website-validation.json"
WEB = ROOT / "web"
CHROME = Path("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
BROWSER = ROOT / "scripts" / "website-validation-browser.mjs"
PRODUCER = "scripts/produce-website-validation.py"
PW_PREFIX = Path(os.environ.get("TMPDIR", "/tmp")) / "merc-website-validation-pw"

ROUTES = {
    "/": "index.html",
    "/buyer": "buyer.html",
    "/admin": "admin.html",
    "/prices": "prices.html",
    "/supplier": "supplier.html",
}

CLAIM_PHRASES = (
    ("embed", ("embed",)),
    ("batch_infer", ("batch_infer",)),
    ("Apple Silicon Metal", ("apple silicon", "metal")),
    ("approved private canary", ("private test canary", "approved")),
    ("native API", ("native api",)),
    ("test-mode payment flow", ("test-mode", "payment")),
)


class LoopbackHandler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(WEB), **kwargs)

    def log_message(self, *_args: object) -> None:
        return

    def do_GET(self) -> None:  # noqa: N802
        mapped = ROUTES.get(self.path.split("?", 1)[0])
        if mapped:
            self.path = "/" + mapped
        return super().do_GET()


def _contrast_from_site_build(stdout: str) -> float:
    match = re.search(r"AA contrast \(([0-9]+(?:\.[0-9]+)?):1\)", stdout)
    if not match:
        raise RuntimeError(f"site-build did not report contrast: {stdout!r}")
    return float(match.group(1))


def _supported_claims() -> list[str]:
    blob = " ".join(
        (WEB / name).read_text(encoding="utf-8").lower()
        for name in ("index.html", "buyer.html", "admin.html")
    )
    found = []
    for label, needles in CLAIM_PHRASES:
        if all(n.lower() in blob for n in needles):
            found.append(label)
    return found


def _ensure_playwright_core() -> Path:
    module_root = PW_PREFIX / "node_modules" / "playwright-core"
    if (module_root / "index.js").is_file():
        return PW_PREFIX
    PW_PREFIX.mkdir(parents=True, exist_ok=True)
    npm = subprocess.run(
        ["npm", "install", "--no-save", "--prefix", str(PW_PREFIX), "playwright-core@1.55.0"],
        check=False,
        capture_output=True,
        text=True,
    )
    if npm.returncode != 0 or not (module_root / "index.js").is_file():
        sys.stderr.write(npm.stdout)
        sys.stderr.write(npm.stderr)
        raise RuntimeError("playwright-core is not installed and npm install failed")
    return PW_PREFIX


def main() -> int:
    if not CHROME.is_file():
        print(f"{PRODUCER}: missing Chrome at {CHROME}", file=sys.stderr)
        return 2
    if not (WEB / "index.html").is_file():
        print(f"{PRODUCER}: missing {WEB / 'index.html'}", file=sys.stderr)
        return 2

    built = subprocess.run(
        ["node", str(ROOT / "scripts" / "site-build.mjs")],
        cwd=ROOT,
        check=False,
        capture_output=True,
        text=True,
    )
    sys.stdout.write(built.stdout)
    if built.returncode != 0:
        sys.stderr.write(built.stderr)
        print(f"{PRODUCER}: site-build.mjs exited {built.returncode}", file=sys.stderr)
        return built.returncode
    contrast = _contrast_from_site_build(built.stdout)

    pw_root = _ensure_playwright_core()
    server = ThreadingHTTPServer(("127.0.0.1", 0), LoopbackHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{server.server_port}"
    try:
        env = os.environ.copy()
        extra = str(pw_root / "node_modules")
        env["NODE_PATH"] = extra + os.pathsep + env.get("NODE_PATH", "")
        browser = subprocess.run(
            ["node", str(BROWSER), base, str(CHROME), str(pw_root)],
            cwd=ROOT,
            check=False,
            capture_output=True,
            text=True,
            env=env,
        )
        if browser.returncode not in (0, 1):
            sys.stderr.write(browser.stderr)
            print(f"{PRODUCER}: playwright helper exited {browser.returncode}", file=sys.stderr)
            return browser.returncode
        try:
            interactive = json.loads(browser.stdout)
        except json.JSONDecodeError:
            sys.stderr.write(browser.stdout)
            sys.stderr.write(browser.stderr)
            print(f"{PRODUCER}: playwright helper did not emit JSON", file=sys.stderr)
            return 2
    finally:
        server.shutdown()
        server.server_close()

    static_ok = built.returncode == 0
    browser_ok = bool(interactive.get("ok"))
    status = "PASS_AUTOMATED_BROWSER" if static_ok and browser_ok else "FAIL"
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    surfaces = interactive.get("surfaces") if isinstance(interactive.get("surfaces"), dict) else {}
    receipt = {
        "schema_version": 1,
        "kind": "website_contract_and_accessibility_validation",
        "status": status,
        "completed_at": now,
        "target": {
            "kind": "loopback_static_server",
            "url_not_tested": "https://mercmerc.net/buyer",
        },
        "honesty": {
            "record_class": "loopback_run",
            "event_date": now[:10],
            "surface_under_test": (
                "loopback static server after `node scripts/site-build.mjs`, "
                "driven by Playwright and the installed Google Chrome executable"
            ),
            "public_hostname_touched": False,
            "does_not_describe": (
                "the public /buyer workspace on mercmerc.net"
            ),
        },
        "surfaces": {
            "public": surfaces.get("public", "FAIL"),
            "buyer": surfaces.get("buyer", "FAIL"),
            "operator": surfaces.get("operator", "FAIL"),
        },
        "checks": {
            "frozen_api_contract": static_ok,
            "all_buyer_lifecycle_states": static_ok,
            "operator_release_and_control_surfaces": static_ok,
            "explicit_labels": static_ok,
            "visible_keyboard_focus": static_ok,
            "wcag_aa_contrast_ratio": contrast,
            "reduced_motion": static_ok,
            "responsive_breakpoint": static_ok,
            "hash_bound_csp": static_ok,
            "secure_headers": static_ok,
            "memory_only_credentials": static_ok,
            "no_deleted_feature_strings": static_ok,
            "no_absolute_local_paths": static_ok,
            "no_secret_prefixes": static_ok,
        },
        "supported_claims": _supported_claims(),
        "interactive_browser": {
            "status": "PASS_AUTOMATED" if browser_ok else "FAIL",
            "engine": "installed Google Chrome driven by Playwright",
            "viewports": interactive.get("viewports") or ["1440x900", "390x844"],
            "checks": interactive.get("checks") or {},
            "limitation": (
                "Automated desktop/mobile interaction coverage; "
                "no manual visual-review approval is claimed. "
                "Control-plane 404s on the static loopback server are expected "
                "and are not treated as page-load failures."
            ),
        },
        "verification_commands": [
            "node scripts/site-build.mjs",
            "Playwright against a loopback static server using the installed Google Chrome executable",
        ],
    }
    stamp(receipt, head_commit(str(ROOT)), PRODUCER)
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(receipt, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(
        f"wrote {OUT} status={status} source_commit={receipt['source_commit']} "
        f"binding_status={receipt['binding_status']}"
    )
    if browser.stderr:
        sys.stderr.write(browser.stderr)
    return 0 if status == "PASS_AUTOMATED_BROWSER" else 1


if __name__ == "__main__":
    raise SystemExit(main())
