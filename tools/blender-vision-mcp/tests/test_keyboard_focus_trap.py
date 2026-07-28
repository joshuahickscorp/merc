from __future__ import annotations

import os
import threading
from collections.abc import Iterator
from contextlib import contextmanager
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

from blender_vision.perception.browser import BrowserAdapter

_ENGINES = [
    item
    for item in os.environ.get(
        "BVMCP_CROSS_BROWSER_ENGINES", "chromium,firefox,webkit"
    ).split(",")
    if item
]

_LAUNCH: dict[str, dict[str, Any]] = {
    "chromium": {"channel": "chrome"},
    "firefox": {"firefox_user_prefs": {"accessibility.tabfocus": 7}},
    "webkit": {},
}


@contextmanager
def _served(directory: Path) -> Iterator[str]:
    class QuietHandler(SimpleHTTPRequestHandler):
        def log_message(self, format: str, *args: Any) -> None:
            del format, args

    server = ThreadingHTTPServer(
        ("127.0.0.1", 0),
        lambda *args, **kwargs: QuietHandler(*args, directory=str(directory), **kwargs),
    )
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_address[1]}/index.html"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def _journey(engine: str, directory: Path) -> dict[str, Any]:
    from playwright.sync_api import sync_playwright

    with _served(directory) as url, sync_playwright() as playwright:
        browser = getattr(playwright, engine).launch(headless=True, **_LAUNCH[engine])
        try:
            page = browser.new_page()
            page.goto(url)
            page.wait_for_load_state("networkidle")
            return BrowserAdapter._keyboard_journey(
                page, {"keyboard_step_limit": 32, "engine": engine}
            )
        finally:
            browser.close()


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_CROSS_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_CROSS_BROWSER_TESTS=1 to launch real browser engines",
)
@pytest.mark.parametrize("engine", _ENGINES)
def test_focus_trap_is_not_reported_as_a_completed_journey(engine: str) -> None:
    fixture = Path(__file__).parent / "fixtures" / "web" / "focus_trap"
    journey = _journey(engine, fixture)

    assert journey["status"] == "FOCUS_TRAPPED"
    assert set(journey["unreached_focusable_targets"]) == {
        "#unreachable-one",
        "#unreachable-two",
    }


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_CROSS_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_CROSS_BROWSER_TESTS=1 to launch real browser engines",
)
@pytest.mark.parametrize("engine", _ENGINES)
def test_reachable_document_completes_on_every_engine(engine: str) -> None:
    fixture = Path(__file__).parent / "fixtures" / "web" / "static"
    journey = _journey(engine, fixture)

    assert journey["status"] in {"COMPLETE_CYCLE", "COMPLETE_DOCUMENT"}
    assert journey["unreached_focusable_targets"] == []
