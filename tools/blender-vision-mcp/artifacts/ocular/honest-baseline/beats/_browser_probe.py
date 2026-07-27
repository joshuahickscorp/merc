
from __future__ import annotations
import json
import sys
from pathlib import Path
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread

from playwright.sync_api import sync_playwright

sandbox = Path('/Users/scammermike/Downloads/visionmcp-authority-worktrees/visionmcp-v2-ocular/tools/blender-vision-mcp/sandbox/datacenter-film')
result = {"beats_reached": [], "ok": False, "error": ""}

class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *a, **k):
        super().__init__(*a, directory=str(sandbox), **k)
    def log_message(self, *args):
        pass

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port = server.server_address[1]
Thread(target=server.serve_forever, daemon=True).start()
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        page = browser.new_page(viewport={"width": 1280, "height": 720})
        page.goto(f"http://127.0.0.1:{port}/index.html", wait_until="networkidle", timeout=60000)
        # Native scroll through each beat midpoint; no hijacking.
        total = page.evaluate("() => document.documentElement.scrollHeight - window.innerHeight")
        for beat_id, frac in [
            ("00", 0.04), ("01", 0.13), ("02", 0.24), ("03", 0.36), ("04", 0.48),
            ("05", 0.61), ("06", 0.74), ("07", 0.85), ("08", 0.95),
        ]:
            y = max(0, int(total * frac))
            page.evaluate(f"window.scrollTo(0, {y})")
            page.wait_for_timeout(120)
            result["beats_reached"].append(beat_id)
        result["ok"] = len(result["beats_reached"]) == 9
        browser.close()
except Exception as exc:
    result["error"] = str(exc)
finally:
    server.shutdown()
print(json.dumps(result))
