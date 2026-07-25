from __future__ import annotations

import hashlib
import importlib.metadata
import ipaddress
import platform
import re
import socket
import subprocess
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

from blender_vision.core.util import canonical_json
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome

_SECRET_KEYS = re.compile(
    r"(?:^|[_-])(authorization|cookie|password|passwd|secret|token|api[_-]?key|"
    r"signature|credential)(?:$|[_-])",
    re.IGNORECASE,
)
_BEARER = re.compile(r"\bBearer\s+[A-Za-z0-9._~+/=-]+", re.IGNORECASE)
_ALLOWED_SCHEMES = {"http", "https"}

_PAGE_SNAPSHOT_SCRIPT = r"""
() => {
  const cleanText = value => (value || "").replace(/\s+/g, " ").trim().slice(0, 4096);
  const cssEscape = value => {
    if (globalThis.CSS && CSS.escape) return CSS.escape(value);
    return value.replace(/[^a-zA-Z0-9_-]/g, ch => `\\${ch}`);
  };
  const selectorFor = element => {
    if (element.id) return `#${cssEscape(element.id)}`;
    const parts = [];
    let current = element;
    while (
      current &&
      current.nodeType === Node.ELEMENT_NODE &&
      current !== document.documentElement
    ) {
      let part = current.localName;
      if (!part) break;
      let index = 1;
      let sibling = current;
      while ((sibling = sibling.previousElementSibling)) {
        if (sibling.localName === current.localName) index += 1;
      }
      part += `:nth-of-type(${index})`;
      parts.unshift(part);
      current = current.parentElement;
    }
    return `html > ${parts.join(" > ")}`;
  };
  const roleFor = element => {
    const explicit = element.getAttribute("role");
    if (explicit) return explicit;
    const tag = element.localName;
    if (tag === "button") return "button";
    if (tag === "a" && element.hasAttribute("href")) return "link";
    if (/^h[1-6]$/.test(tag)) return "heading";
    if (tag === "img") return "img";
    if (tag === "input") {
      const type = (element.getAttribute("type") || "text").toLowerCase();
      if (type === "checkbox") return "checkbox";
      if (type === "radio") return "radio";
      if (["button", "submit", "reset"].includes(type)) return "button";
      return "textbox";
    }
    if (tag === "textarea") return "textbox";
    if (tag === "select") return "combobox";
    if (tag === "nav") return "navigation";
    if (tag === "main") return "main";
    return null;
  };
  const surfaceFor = element => {
    const tag = element.localName;
    if (tag === "canvas") {
      try {
        if (element.getContext("webgpu")) return "WebGPU";
      } catch {}
      try {
        if (element.getContext("webgl2") || element.getContext("webgl")) return "WebGL";
      } catch {}
      return "Canvas2D";
    }
    if (tag === "svg" || element.ownerSVGElement) return "SVG";
    if (tag === "img") return "Image";
    if (tag === "video") return "Video";
    if (tag === "iframe") return "Iframe";
    return "DOM";
  };
  const elements = [document.documentElement, ...document.querySelectorAll("body, body *")];
  const selectorIds = new Map();
  const nodes = elements.map((element, index) => {
    const selector = selectorFor(element);
    const id = `node:${index}:${selector}`;
    selectorIds.set(element, id);
    const bounds = element.getBoundingClientRect();
    const style = getComputedStyle(element);
    const zIndex = Number.parseFloat(style.zIndex);
    const attributes = Object.fromEntries(
      [...element.attributes].map(attribute => [attribute.name, attribute.value.slice(0, 4096)])
    );
    const assetUrls = [
      element.currentSrc,
      element.src,
      element.href,
      element.poster,
    ].filter(value => typeof value === "string" && value);
    const interactive = Boolean(
      element.matches("a[href],button,input,select,textarea,summary,[role],[tabindex]") ||
      element.onclick
    );
    let depth = 0;
    for (let current = element.parentElement; current; current = current.parentElement) depth += 1;
    return {
      id,
      selector,
      tag: element.localName,
      role: roleFor(element),
      text: cleanText(element.innerText || element.textContent),
      accessibleName: cleanText(
        element.getAttribute("aria-label") ||
        element.getAttribute("alt") ||
        element.getAttribute("title") ||
        ""
      ),
      bounds: {
        x: bounds.x,
        y: bounds.y,
        width: bounds.width,
        height: bounds.height,
        top: bounds.top,
        right: bounds.right,
        bottom: bounds.bottom,
        left: bounds.left,
      },
      styles: {
        display: style.display,
        visibility: style.visibility,
        position: style.position,
        zIndex: style.zIndex,
        zIndexNumeric: Number.isFinite(zIndex) ? zIndex : 0,
        opacity: style.opacity,
        transform: style.transform,
        color: style.color,
        backgroundColor: style.backgroundColor,
        backgroundImage: style.backgroundImage,
        fontFamily: style.fontFamily,
        fontSize: style.fontSize,
        fontWeight: style.fontWeight,
        lineHeight: style.lineHeight,
        borderRadius: style.borderRadius,
        overflow: style.overflow,
      },
      attributes,
      sourceBinding: {
        id: element.id || null,
        class: element.getAttribute("class"),
        dataAttributes: Object.fromEntries(
          Object.entries(element.dataset || {}).map(([key, value]) => [key, String(value)])
        ),
      },
      assetUrls: [...new Set(assetUrls)],
      surface: surfaceFor(element),
      interactive,
      depth,
    };
  });
  const edges = [];
  for (const element of elements) {
    const child = selectorIds.get(element);
    const parent = selectorIds.get(element.parentElement);
    if (child && parent) edges.push({source: parent, target: child, type: "CONTAINS"});
  }
  const stylesheets = [...document.styleSheets].map((sheet, index) => {
    const result = {
      index,
      href: sheet.href,
      media: sheet.media ? sheet.media.mediaText : "",
      disabled: sheet.disabled,
      rules: [],
      inaccessible: false,
    };
    try {
      result.rules = [...sheet.cssRules].map(rule => rule.cssText);
    } catch (error) {
      result.inaccessible = true;
      result.error = error.name;
    }
    return result;
  });
  const fonts = document.fonts ? [...document.fonts].map(font => ({
    family: font.family,
    style: font.style,
    weight: font.weight,
    stretch: font.stretch,
    status: font.status,
  })) : [];
  const assets = [...new Set(nodes.flatMap(node => node.assetUrls))].map(url => ({url}));
  const surfaces = nodes
    .filter(node => node.surface !== "DOM")
    .map(node => ({
      nodeId: node.id,
      selector: node.selector,
      surface: node.surface,
      bounds: node.bounds,
    }));
  return {
    document: {
      url: location.href,
      origin: location.origin,
      title: document.title,
      language: document.documentElement.lang || null,
      characterSet: document.characterSet,
      contentType: document.contentType,
      readyState: document.readyState,
      viewport: {
        width: innerWidth,
        height: innerHeight,
        scrollWidth: document.documentElement.scrollWidth,
        scrollHeight: document.documentElement.scrollHeight,
        devicePixelRatio,
      },
    },
    graph: {
      schema: "vision.layout-graph/v1",
      graph_type: "LayoutGraph",
      authority: "OBSERVED",
      coordinate_space: "CSS viewport pixels",
      nodes,
      edges,
    },
    stylesheets,
    fonts,
    assets,
    surfaces,
  };
}
"""


class BrowserAdapter:
    name = "browser.chromium"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        url = str(target.get("url", "")).strip()
        parsed = urlparse(url)
        if parsed.scheme not in _ALLOWED_SCHEMES or not parsed.hostname:
            raise ValueError("browser target must be an absolute http(s) URL")
        if parsed.username or parsed.password:
            raise ValueError("credentials are forbidden in target URLs")
        normalized = urlunparse(
            (
                parsed.scheme.lower(),
                parsed.netloc.lower(),
                parsed.path or "/",
                "",
                parsed.query,
                "",
            )
        )
        return {
            "id": str(
                target.get("id")
                or hashlib.sha256(normalized.encode("utf-8")).hexdigest()
            ),
            "url": normalized,
            "kind": "web",
        }

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        viewport = dict(config.get("viewport") or {})
        width = int(viewport.get("width", 1280))
        height = int(viewport.get("height", 720))
        if not 320 <= width <= 7680 or not 240 <= height <= 4320:
            raise ValueError("viewport is outside the supported range")
        dpr = float(config.get("device_scale_factor", 1.0))
        if not 0.5 <= dpr <= 4.0:
            raise ValueError("device_scale_factor must be between 0.5 and 4")
        color_scheme = str(config.get("color_scheme", "light"))
        if color_scheme not in {"light", "dark", "no-preference"}:
            raise ValueError("invalid color_scheme")
        reduced_motion = str(config.get("reduced_motion", "no-preference"))
        if reduced_motion not in {"reduce", "no-preference"}:
            raise ValueError("invalid reduced_motion")
        wait_until = str(config.get("wait_until", "networkidle"))
        if wait_until not in {"commit", "domcontentloaded", "load", "networkidle"}:
            raise ValueError("invalid wait_until")
        allowed_origins = sorted(
            {self._normalize_origin(str(origin)) for origin in config.get("allowed_origins", [])}
        )
        if not allowed_origins:
            raise PermissionError("an explicit allowed_origins list is required")
        target_origin = self._normalize_origin(target["url"])
        if target_origin not in allowed_origins:
            raise PermissionError("target origin is not present in allowed_origins")
        allow_private = bool(config.get("allow_private_network", False))
        self._enforce_network_policy(target["url"], allow_private=allow_private)
        executable_path = config.get("executable_path")
        if executable_path is not None:
            executable_path = str(executable_path)
            if not executable_path.startswith("/"):
                raise ValueError("executable_path must be absolute")
        return {
            "viewport": {"width": width, "height": height},
            "device_scale_factor": dpr,
            "locale": str(config.get("locale", "en-US")),
            "timezone_id": str(config.get("timezone_id", "UTC")),
            "color_scheme": color_scheme,
            "reduced_motion": reduced_motion,
            "wait_until": wait_until,
            "timeout_ms": max(1_000, min(int(config.get("timeout_ms", 30_000)), 120_000)),
            "allowed_origins": allowed_origins,
            "allow_private_network": allow_private,
            "channel": str(config.get("channel", "chrome")),
            "executable_path": executable_path,
            "headless": bool(config.get("headless", True)),
            "full_page": bool(config.get("full_page", True)),
            "ignore_https_errors": bool(config.get("ignore_https_errors", False)),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "playwright": importlib.metadata.version("playwright"),
            "browser_channel": config["channel"],
            "browser_executable": config["executable_path"],
            "browser_version": self._browser_version(config),
            "locale": config["locale"],
            "timezone_id": config["timezone_id"],
            "viewport": config["viewport"],
            "device_scale_factor": config["device_scale_factor"],
            "color_scheme": config["color_scheme"],
            "reduced_motion": config["reduced_motion"],
            "service_workers": "blocked",
            "cache": "disabled",
            "storage_state": "empty",
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        from playwright.sync_api import Error as PlaywrightError
        from playwright.sync_api import sync_playwright

        network: list[dict[str, Any]] = []
        console: list[dict[str, Any]] = []
        policy_violations: list[dict[str, str]] = []

        def emit_json(role: str, value: Any, metadata: dict[str, Any] | None = None) -> None:
            sink(role, canonical_json(self._redact(value)), "application/json", metadata)

        with sync_playwright() as playwright:
            launch: dict[str, Any] = {
                "headless": config["headless"],
                "args": [
                    "--disable-background-networking",
                    "--disable-component-update",
                    "--disable-default-apps",
                    "--disable-sync",
                    "--metrics-recording-only",
                    "--no-first-run",
                ],
            }
            if config["executable_path"]:
                launch["executable_path"] = config["executable_path"]
            else:
                launch["channel"] = config["channel"]
            browser = playwright.chromium.launch(**launch)
            context = browser.new_context(
                viewport=config["viewport"],
                device_scale_factor=config["device_scale_factor"],
                locale=config["locale"],
                timezone_id=config["timezone_id"],
                color_scheme=config["color_scheme"],
                reduced_motion=config["reduced_motion"],
                service_workers="block",
                ignore_https_errors=config["ignore_https_errors"],
            )
            page = context.new_page()
            cdp = context.new_cdp_session(page)
            cdp.send("Network.enable")
            cdp.send("Network.setCacheDisabled", {"cacheDisabled": True})
            cdp.send("Accessibility.enable")
            cdp.send("Performance.enable")

            def route_request(route: Any) -> None:
                url = route.request.url
                parsed = urlparse(url)
                if parsed.scheme in {"data", "blob", "about"}:
                    route.continue_()
                    return
                try:
                    origin = self._normalize_origin(url)
                    if origin not in config["allowed_origins"]:
                        raise PermissionError("origin is not allowlisted")
                    self._enforce_network_policy(
                        url, allow_private=config["allow_private_network"]
                    )
                except (PermissionError, ValueError) as error:
                    policy_violations.append({"url": self._redact_url(url), "reason": str(error)})
                    route.abort("blockedbyclient")
                    return
                route.continue_()

            page.route("**/*", route_request)
            page.on(
                "request",
                lambda request: network.append(
                    {
                        "event": "request",
                        "method": request.method,
                        "url": self._redact_url(request.url),
                        "resource_type": request.resource_type,
                        "headers": self._redact(dict(request.headers)),
                    }
                ),
            )
            page.on(
                "response",
                lambda response: network.append(
                    {
                        "event": "response",
                        "url": self._redact_url(response.url),
                        "status": response.status,
                        "ok": response.ok,
                        "from_service_worker": response.from_service_worker,
                        "headers": self._safe_response_headers(response),
                    }
                ),
            )
            page.on(
                "requestfailed",
                lambda request: network.append(
                    {
                        "event": "request_failed",
                        "url": self._redact_url(request.url),
                        "failure": request.failure,
                    }
                ),
            )
            page.on(
                "console",
                lambda message: console.append(
                    {
                        "type": message.type,
                        "text": self._redact_text(message.text),
                        "location": self._redact(message.location),
                    }
                ),
            )
            page.on(
                "pageerror",
                lambda error: console.append(
                    {"type": "pageerror", "text": self._redact_text(str(error))}
                ),
            )
            try:
                response = page.goto(
                    target["url"],
                    wait_until=config["wait_until"],
                    timeout=config["timeout_ms"],
                )
                if response is None:
                    raise RuntimeError("navigation returned no response")
                snapshot = page.evaluate(_PAGE_SNAPSHOT_SCRIPT)
                viewport_image = page.screenshot(type="png", full_page=False)
                full_image = (
                    page.screenshot(type="png", full_page=True)
                    if config["full_page"]
                    else viewport_image
                )
                dom_snapshot = cdp.send(
                    "DOMSnapshot.captureSnapshot",
                    {
                        "computedStyles": [
                            "display",
                            "position",
                            "z-index",
                            "opacity",
                            "transform",
                            "color",
                            "background-color",
                            "font-family",
                            "font-size",
                        ],
                        "includePaintOrder": True,
                        "includeDOMRects": True,
                        "includeBlendedBackgroundColors": True,
                        "includeTextColorOpacities": True,
                    },
                )
                accessibility = cdp.send("Accessibility.getFullAXTree")
                performance = cdp.send("Performance.getMetrics")
                html = page.content()
                final_url = page.url
                status = response.status
                browser_version = browser.version
            except PlaywrightError as error:
                raise RuntimeError(f"browser capture failed: {error}") from error
            finally:
                context.close()
                browser.close()

        graph = snapshot["graph"]
        graph["capture"] = {
            "target_url": self._redact_url(target["url"]),
            "final_url": self._redact_url(final_url),
            "viewport": config["viewport"],
            "device_scale_factor": config["device_scale_factor"],
        }
        sink(
            "screenshot.viewport",
            viewport_image,
            "image/png",
            {"coordinate_space": "device pixels", "viewport": config["viewport"]},
        )
        sink(
            "screenshot.full",
            full_image,
            "image/png",
            {"coordinate_space": "device pixels", "full_page": config["full_page"]},
        )
        sink("dom.html", html.encode("utf-8"), "text/html", {"final_url": final_url})
        emit_json("dom.snapshot", dom_snapshot)
        emit_json("accessibility.tree", accessibility)
        emit_json("layout.graph", graph)
        emit_json("stylesheets", snapshot["stylesheets"])
        emit_json("fonts", snapshot["fonts"])
        emit_json("assets", snapshot["assets"])
        emit_json("network", {"events": network, "policy_violations": policy_violations})
        emit_json("console", {"messages": console})
        emit_json("performance", performance)
        emit_json("document.metadata", snapshot["document"])
        emit_json("surfaces", snapshot["surfaces"])
        return CaptureOutcome(
            summary={
                "final_url": self._redact_url(final_url),
                "http_status": status,
                "browser_version": browser_version,
                "node_count": len(graph["nodes"]),
                "edge_count": len(graph["edges"]),
                "surface_count": len(snapshot["surfaces"]),
                "network_event_count": len(network),
                "console_message_count": len(console),
            },
            limitations=[
                "LayoutGraph covers the captured document and same-origin DOM exposed to the page.",
                "Closed shadow roots and cross-origin frame internals remain opaque.",
                "Network evidence records metadata but deliberately omits response bodies.",
            ],
            graphs=[
                {
                    "graph_type": "LayoutGraph",
                    "role": "layout.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "OBSERVED",
                }
            ],
        )

    @staticmethod
    def _normalize_origin(value: str) -> str:
        parsed = urlparse(value)
        if parsed.scheme not in _ALLOWED_SCHEMES or not parsed.hostname:
            raise ValueError(f"invalid http(s) origin: {value}")
        port = parsed.port
        default = (parsed.scheme == "http" and port == 80) or (
            parsed.scheme == "https" and port == 443
        )
        authority = (
            parsed.hostname.lower()
            if port is None or default
            else f"{parsed.hostname}:{port}"
        )
        return f"{parsed.scheme.lower()}://{authority}"

    @staticmethod
    def _enforce_network_policy(url: str, *, allow_private: bool) -> None:
        parsed = urlparse(url)
        if parsed.scheme not in _ALLOWED_SCHEMES or not parsed.hostname:
            raise ValueError("only absolute http(s) URLs are permitted")
        if allow_private:
            return
        try:
            addresses = {
                item[4][0]
                for item in socket.getaddrinfo(
                    parsed.hostname,
                    parsed.port or 443,
                    type=socket.SOCK_STREAM,
                )
            }
        except socket.gaierror as error:
            raise PermissionError(f"hostname could not be resolved: {parsed.hostname}") from error
        for address in addresses:
            ip = ipaddress.ip_address(address)
            if not ip.is_global:
                raise PermissionError(f"private or special-use address is blocked: {ip}")

    def _browser_version(self, config: dict[str, Any]) -> str:
        candidates = []
        if config["executable_path"]:
            candidates.append(config["executable_path"])
        if platform.system() == "Darwin" and config["channel"] == "chrome":
            candidates.append("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
        for executable in candidates:
            try:
                return subprocess.run(
                    [executable, "--version"],
                    check=True,
                    capture_output=True,
                    text=True,
                    timeout=5,
                ).stdout.strip()
            except (OSError, subprocess.SubprocessError):
                continue
        return f"{config['channel']}:resolved-at-launch"

    @staticmethod
    def _redact_text(value: str) -> str:
        return _BEARER.sub("Bearer [REDACTED]", value)

    @classmethod
    def _redact(cls, value: Any) -> Any:
        if isinstance(value, dict):
            return {
                key: (
                    "[REDACTED]"
                    if _SECRET_KEYS.search(str(key))
                    else cls._redact(item)
                )
                for key, item in value.items()
            }
        if isinstance(value, list):
            return [cls._redact(item) for item in value]
        if isinstance(value, str):
            return cls._redact_text(value)
        return value

    @classmethod
    def _redact_url(cls, url: str) -> str:
        parsed = urlparse(url)
        query = [
            (key, "[REDACTED]" if _SECRET_KEYS.search(key) else cls._redact_text(value))
            for key, value in parse_qsl(parsed.query, keep_blank_values=True)
        ]
        return urlunparse(
            (
                parsed.scheme,
                parsed.netloc,
                parsed.path,
                parsed.params,
                urlencode(query),
                "",
            )
        )

    @classmethod
    def _safe_response_headers(cls, response: Any) -> dict[str, Any]:
        try:
            return cls._redact(dict(response.headers))
        except Exception as error:
            return {"unavailable": type(error).__name__}
