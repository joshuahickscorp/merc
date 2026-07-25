from __future__ import annotations

import html
import ipaddress
import json
import re
import secrets
import threading
import webbrowser
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from blender_vision.projects.store import ProjectStore
from blender_vision.review.service import ReviewService

MAX_REQUEST_BYTES = 1024 * 1024
SAFE_MEDIA_TYPE = re.compile(r"^[A-Za-z0-9!#$&^_.+\-/]+(?:;[ A-Za-z0-9=._\-]+)?$")


@dataclass(frozen=True)
class ReviewServer:
    httpd: ThreadingHTTPServer
    url: str
    token: str

    def serve_forever(self) -> None:
        try:
            self.httpd.serve_forever(poll_interval=0.25)
        finally:
            self.httpd.server_close()

    def shutdown(self) -> None:
        self.httpd.shutdown()


def _static_root() -> Path:
    packaged = Path(__file__).with_name("static")
    source = Path(__file__).resolve().parents[3] / "review_app" / "frontend"
    return packaged if packaged.is_dir() else source


def _handler(service: ReviewService, token: str, static_root: Path):
    class Handler(BaseHTTPRequestHandler):
        server_version = "BlenderVisionReview/1"

        def _headers(self, status: int, media_type: str, length: int) -> None:
            self.send_response(status)
            self.send_header("Content-Type", media_type)
            self.send_header("Content-Length", str(length))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Referrer-Policy", "no-referrer")
            self.send_header("X-Frame-Options", "DENY")
            self.send_header(
                "Content-Security-Policy",
                "default-src 'self'; img-src 'self' data:; script-src 'self'; "
                "style-src 'self'; connect-src 'self'; frame-ancestors 'none'",
            )
            self.end_headers()

        def _json(self, value: Any, status: int = 200) -> None:
            encoded = json.dumps(value, sort_keys=True).encode()
            self._headers(status, "application/json; charset=utf-8", len(encoded))
            self.wfile.write(encoded)

        def _file(self, path: Path, media_type: str) -> None:
            if not SAFE_MEDIA_TYPE.fullmatch(media_type):
                media_type = "application/octet-stream"
            data = path.read_bytes()
            self._headers(200, media_type, len(data))
            self.wfile.write(data)

        def do_GET(self) -> None:  # noqa: N802
            try:
                path = urlparse(self.path).path
                if path == "/api/snapshot":
                    self._json(service.snapshot())
                    return
                if path == "/api/queue":
                    self._json({"review_queue": service.review_queue()})
                    return
                if path.startswith("/artifact/"):
                    digest = path.removeprefix("/artifact/")
                    artifact, media_type = service.artifact(digest)
                    self._file(artifact, media_type)
                    return
                if path == "/":
                    index = (static_root / "index.html").read_text(encoding="utf-8")
                    index = index.replace("__BVMCP_TOKEN__", html.escape(token, quote=True))
                    encoded = index.encode()
                    self._headers(200, "text/html; charset=utf-8", len(encoded))
                    self.wfile.write(encoded)
                    return
                if path.startswith("/static/"):
                    candidate = (static_root / path.removeprefix("/static/")).resolve()
                    if (
                        not candidate.is_relative_to(static_root.resolve())
                        or not candidate.is_file()
                    ):
                        raise FileNotFoundError(path)
                    media_type = {
                        ".css": "text/css; charset=utf-8",
                        ".js": "text/javascript; charset=utf-8",
                    }.get(candidate.suffix.lower(), "application/octet-stream")
                    self._file(candidate, media_type)
                    return
                self._json({"error": "not found"}, 404)
            except (FileNotFoundError, KeyError) as error:
                self._json({"error": str(error)}, 404)
            except Exception as error:
                self._json({"error": f"{type(error).__name__}: {error}"}, 500)

        def do_POST(self) -> None:  # noqa: N802
            if not secrets.compare_digest(self.headers.get("X-BVMCP-Review-Token", ""), token):
                self._json({"error": "invalid review token"}, 403)
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if length < 0 or length > MAX_REQUEST_BYTES:
                    self._json({"error": "request body is too large"}, 413)
                    return
                payload = json.loads(self.rfile.read(length) or b"{}")
                if not isinstance(payload, dict):
                    raise ValueError("review payload must be a JSON object")
                path = urlparse(self.path).path
                if not path.startswith("/api/action/"):
                    self._json({"error": "not found"}, 404)
                    return
                result = service.action(path.removeprefix("/api/action/"), payload)
                self._json({"ok": True, "result": result})
            except (KeyError, ValueError, FileNotFoundError) as error:
                self._json({"error": f"{type(error).__name__}: {error}"}, 400)
            except Exception as error:
                self._json({"error": f"{type(error).__name__}: {error}"}, 409)

        def log_message(self, format: str, *args: Any) -> None:
            return

    return Handler


def create_review_server(
    project: ProjectStore,
    *,
    host: str = "127.0.0.1",
    port: int = 8787,
) -> ReviewServer:
    normalized_host = "127.0.0.1" if host == "localhost" else host
    try:
        is_loopback = ipaddress.ip_address(normalized_host).is_loopback
    except ValueError as error:
        raise ValueError("review host must be a loopback IP address or localhost") from error
    if not is_loopback:
        raise ValueError("review server only binds to loopback addresses")
    if not 0 <= port <= 65535:
        raise ValueError("review port must be between 0 and 65535")
    static_root = _static_root()
    if not (static_root / "index.html").is_file():
        raise FileNotFoundError(f"review frontend is missing: {static_root}")
    token = secrets.token_urlsafe(32)
    server = ThreadingHTTPServer(
        (normalized_host, port), _handler(ReviewService(project), token, static_root)
    )
    actual_port = server.server_address[1]
    url = f"http://{normalized_host}:{actual_port}/"
    return ReviewServer(httpd=server, url=url, token=token)


def serve_review(
    project: ProjectStore,
    *,
    host: str = "127.0.0.1",
    port: int = 8787,
    open_browser: bool = False,
) -> None:
    server = create_review_server(project, host=host, port=port)
    print(json.dumps({"review_url": server.url, "project": str(project.root)}), flush=True)
    if open_browser:
        webbrowser.open(server.url)
    server.serve_forever()


def run_review_server_in_thread(server: ReviewServer) -> threading.Thread:
    """Start a pre-created review server for embedding and integration tests."""

    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return thread
