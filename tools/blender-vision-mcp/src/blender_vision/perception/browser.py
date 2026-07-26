from __future__ import annotations

import hashlib
import importlib.metadata
import ipaddress
import os
import platform
import re
import socket
import subprocess
from pathlib import Path
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

from blender_vision.core.util import canonical_json, sha256_file
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome

_SECRET_KEYS = re.compile(
    r"(?:^|[_-])(authorization|cookie|password|passwd|secret|token|api[_-]?key|"
    r"signature|credential)(?:$|[_-])",
    re.IGNORECASE,
)
_BEARER = re.compile(r"\bBearer\s+[A-Za-z0-9._~+/=-]+", re.IGNORECASE)
_ALLOWED_SCHEMES = {"http", "https"}
_BROWSER_ENGINES = {"chromium", "firefox", "webkit"}
_NETWORK_PROFILES = {
    "online": None,
    "fast-3g": {
        "offline": False,
        "latency": 150,
        "downloadThroughput": 1_600_000 / 8,
        "uploadThroughput": 750_000 / 8,
        "connectionType": "cellular3g",
    },
    "slow-3g": {
        "offline": False,
        "latency": 400,
        "downloadThroughput": 400_000 / 8,
        "uploadThroughput": 400_000 / 8,
        "connectionType": "cellular3g",
    },
}

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

_SEMANTIC_ACCESSIBILITY_SCRIPT = r"""
() => {
  const clean = value => (value || "").replace(/\s+/g, " ").trim().slice(0, 1024);
  const escape = value => globalThis.CSS && CSS.escape
    ? CSS.escape(value)
    : value.replace(/[^a-zA-Z0-9_-]/g, ch => `\\${ch}`);
  const selectorFor = element => {
    if (element.id) return `#${escape(element.id)}`;
    const parts = [];
    for (
      let current = element;
      current && current !== document.documentElement;
      current = current.parentElement
    ) {
      let index = 1;
      for (
        let sibling = current.previousElementSibling;
        sibling;
        sibling = sibling.previousElementSibling
      ) {
        if (sibling.localName === current.localName) index += 1;
      }
      parts.unshift(`${current.localName}:nth-of-type(${index})`);
    }
    return `html > ${parts.join(" > ")}`;
  };
  const implicitRole = element => {
    const tag = element.localName;
    if (tag === "a" && element.hasAttribute("href")) return "link";
    if (tag === "button") return "button";
    if (/^h[1-6]$/.test(tag)) return "heading";
    if (tag === "img") return "img";
    if (tag === "nav") return "navigation";
    if (tag === "main") return "main";
    if (tag === "form") return "form";
    if (tag === "textarea") return "textbox";
    if (tag === "select") return "combobox";
    if (tag === "input") {
      const type = (element.type || "text").toLowerCase();
      if (type === "checkbox") return "checkbox";
      if (type === "radio") return "radio";
      if (["button", "submit", "reset"].includes(type)) return "button";
      if (type !== "hidden") return "textbox";
    }
    return null;
  };
  const nameFor = element => {
    const labelledBy = clean(element.getAttribute("aria-labelledby"));
    if (labelledBy) {
      const value = labelledBy
        .split(/\s+/)
        .map(id => document.getElementById(id))
        .filter(Boolean)
        .map(node => clean(node.innerText || node.textContent))
        .join(" ");
      if (value) return value;
    }
    const aria = clean(element.getAttribute("aria-label"));
    if (aria) return aria;
    if (element.id) {
      const label = document.querySelector(`label[for="${escape(element.id)}"]`);
      if (label) return clean(label.innerText || label.textContent);
    }
    const parentLabel = element.closest("label");
    if (parentLabel) return clean(parentLabel.innerText || parentLabel.textContent);
    return clean(
      element.getAttribute("alt") ||
      element.getAttribute("title") ||
      element.innerText ||
      element.value ||
      element.textContent
    );
  };
  const elements = [...document.querySelectorAll("body, body *")];
  const nodes = elements.map(element => {
    const bounds = element.getBoundingClientRect();
    const style = getComputedStyle(element);
    const hidden = (
      element.hidden ||
      element.getAttribute("aria-hidden") === "true" ||
      style.display === "none" ||
      style.visibility === "hidden"
    );
    const role = element.getAttribute("role") || implicitRole(element);
    const name = nameFor(element);
    const naturallyFocusable = element.matches(
      "a[href],button,input:not([type=hidden]),select,textarea,summary"
    );
    const focusable = !hidden && !element.disabled && (
      naturallyFocusable || element.tabIndex >= 0
    );
    return {
      selector: selectorFor(element),
      tag: element.localName,
      role,
      name,
      hidden,
      disabled: Boolean(element.disabled || element.getAttribute("aria-disabled") === "true"),
      focusable,
      tabIndex: element.tabIndex,
      language: element.getAttribute("lang"),
      aria: Object.fromEntries(
        [...element.attributes]
          .filter(attribute => attribute.name.startsWith("aria-"))
          .map(attribute => [attribute.name, attribute.value])
      ),
      bounds: {x: bounds.x, y: bounds.y, width: bounds.width, height: bounds.height},
    };
  });
  const issues = [];
  const add = (rule, impact, node, message) => issues.push({
    id: `${rule}:${node.selector}`,
    rule,
    impact,
    selector: node.selector,
    message,
  });
  for (const node of nodes) {
    if (node.hidden) continue;
    if (node.tag === "img" && !node.name) {
      add("image-alt", "serious", node, "Visible image has no accessible name.");
    }
    if (
      ["button", "link", "textbox", "combobox", "checkbox", "radio"].includes(node.role) &&
      !node.name
    ) {
      add("interactive-name", "serious", node, "Interactive control has no accessible name.");
    }
    if (node.tabIndex > 0) {
      add("positive-tabindex", "moderate", node, "Positive tabindex overrides document order.");
    }
  }
  const ids = new Map();
  for (const element of document.querySelectorAll("[id]")) {
    const current = ids.get(element.id) || [];
    current.push(selectorFor(element));
    ids.set(element.id, current);
  }
  for (const [id, selectors] of ids) {
    if (selectors.length > 1) {
      issues.push({
        id: `duplicate-id:${id}`,
        rule: "duplicate-id",
        impact: "serious",
        selector: selectors.join(","),
        message: `Document ID ${id} occurs ${selectors.length} times.`,
      });
    }
  }
  return {
    schema: "vision.accessibility-semantic-snapshot/v1",
    document: {
      title: document.title,
      language: document.documentElement.lang || null,
      url: location.href,
    },
    media: {
      colorSchemeDark: matchMedia("(prefers-color-scheme: dark)").matches,
      reducedMotion: matchMedia("(prefers-reduced-motion: reduce)").matches,
      forcedColors: matchMedia("(forced-colors: active)").matches,
      orientationPortrait: matchMedia("(orientation: portrait)").matches,
      pointerCoarse: matchMedia("(pointer: coarse)").matches,
      online: navigator.onLine,
      maxTouchPoints: navigator.maxTouchPoints,
      devicePixelRatio,
      viewport: {width: innerWidth, height: innerHeight},
    },
    nodes,
    issues,
  };
}
"""

_ACTIVE_ELEMENT_SCRIPT = r"""
() => {
  const element = document.activeElement;
  if (!element) return null;
  const escape = value => globalThis.CSS && CSS.escape
    ? CSS.escape(value)
    : value.replace(/[^a-zA-Z0-9_-]/g, ch => `\\${ch}`);
  const selectorFor = target => {
    if (target.id) return `#${escape(target.id)}`;
    const parts = [];
    for (
      let current = target;
      current && current !== document.documentElement;
      current = current.parentElement
    ) {
      let index = 1;
      for (
        let sibling = current.previousElementSibling;
        sibling;
        sibling = sibling.previousElementSibling
      ) {
        if (sibling.localName === current.localName) index += 1;
      }
      parts.unshift(`${current.localName}:nth-of-type(${index})`);
    }
    return `html > ${parts.join(" > ")}`;
  };
  return {
    selector: selectorFor(element),
    tag: element.localName,
    role: element.getAttribute("role"),
    name: (
      element.getAttribute("aria-label") ||
      element.innerText ||
      element.value ||
      element.getAttribute("title") ||
      ""
    ).replace(/\s+/g, " ").trim().slice(0, 512),
  };
}
"""


class BrowserAdapter:
    name = "browser.chromium"
    version = "2"

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
        forced_colors = str(config.get("forced_colors", "none"))
        if forced_colors not in {"active", "none"}:
            raise ValueError("invalid forced_colors")
        contrast = str(config.get("contrast", "no-preference"))
        if contrast not in {"more", "no-preference"}:
            raise ValueError("invalid contrast")
        engine = str(config.get("engine", "chromium")).lower()
        if engine not in _BROWSER_ENGINES:
            raise ValueError(f"browser engine must be one of {sorted(_BROWSER_ENGINES)}")
        channel_value = config.get("channel", "chrome" if engine == "chromium" else None)
        if engine == "chromium" and channel_value is None:
            channel_value = "chrome"
        channel = str(channel_value).strip() if channel_value else None
        if channel == "playwright":
            channel = None
        if engine != "chromium" and channel is not None:
            raise ValueError("browser channels are supported only by the chromium engine")
        is_mobile = bool(config.get("is_mobile", False))
        if engine == "firefox" and is_mobile:
            raise ValueError("Playwright does not support is_mobile for Firefox")
        has_touch = bool(config.get("has_touch", is_mobile))
        orientation = str(
            config.get("orientation", "portrait" if height >= width else "landscape")
        )
        if orientation not in {"portrait", "landscape"}:
            raise ValueError("orientation must be portrait or landscape")
        observed_orientation = "portrait" if height >= width else "landscape"
        if orientation != observed_orientation:
            raise ValueError(
                f"orientation {orientation} conflicts with {width}x{height} viewport"
            )
        offline = bool(config.get("offline", False))
        network_profile = str(config.get("network_profile", "offline" if offline else "online"))
        if network_profile not in {*_NETWORK_PROFILES, "offline"}:
            raise ValueError("network_profile must be online, offline, fast-3g, or slow-3g")
        if network_profile == "offline":
            offline = True
        elif offline:
            raise ValueError("offline=true conflicts with an online network_profile")
        if network_profile in {"fast-3g", "slow-3g"} and engine != "chromium":
            raise ValueError("bandwidth throttling requires the chromium CDP runtime")
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
            executable = Path(str(executable_path))
            if not executable.is_absolute():
                raise ValueError("executable_path must be absolute")
            if executable.is_symlink():
                raise ValueError("executable_path must not be a symlink")
            if not executable.is_file() or not os.access(executable, os.X_OK):
                raise ValueError("executable_path must be an executable regular file")
            executable_path = str(executable.resolve())
        resolved_executable = self._resolve_browser_executable(
            engine=engine,
            channel=channel,
            executable_path=executable_path,
        )
        executable_sha256 = None
        if resolved_executable and Path(resolved_executable).is_file():
            executable_sha256, _size = sha256_file(Path(resolved_executable))
        return {
            "engine": engine,
            "viewport": {"width": width, "height": height},
            "screen": {"width": width, "height": height},
            "device_scale_factor": dpr,
            "locale": str(config.get("locale", "en-US")),
            "timezone_id": str(config.get("timezone_id", "UTC")),
            "color_scheme": color_scheme,
            "reduced_motion": reduced_motion,
            "forced_colors": forced_colors,
            "contrast": contrast,
            "orientation": orientation,
            "is_mobile": is_mobile,
            "has_touch": has_touch,
            "offline": offline,
            "network_profile": network_profile,
            "wait_until": wait_until,
            "timeout_ms": max(1_000, min(int(config.get("timeout_ms", 30_000)), 120_000)),
            "launch_timeout_ms": max(
                1_000, min(int(config.get("launch_timeout_ms", 15_000)), 60_000)
            ),
            "keyboard_step_limit": max(
                1, min(int(config.get("keyboard_step_limit", 32)), 256)
            ),
            "allowed_origins": allowed_origins,
            "allow_private_network": allow_private,
            "channel": channel,
            "executable_path": executable_path,
            "resolved_executable_path": resolved_executable,
            "executable_sha256": executable_sha256,
            "headless": bool(config.get("headless", True)),
            "full_page": bool(config.get("full_page", True)),
            "ignore_https_errors": bool(config.get("ignore_https_errors", False)),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "playwright": importlib.metadata.version("playwright"),
            "browser_engine": config["engine"],
            "browser_channel": config["channel"],
            "browser_executable": config["resolved_executable_path"],
            "browser_executable_sha256": config["executable_sha256"],
            "browser_version": self._browser_version(config),
            "locale": config["locale"],
            "timezone_id": config["timezone_id"],
            "viewport": config["viewport"],
            "screen": config["screen"],
            "device_scale_factor": config["device_scale_factor"],
            "orientation": config["orientation"],
            "is_mobile": config["is_mobile"],
            "has_touch": config["has_touch"],
            "color_scheme": config["color_scheme"],
            "reduced_motion": config["reduced_motion"],
            "forced_colors": config["forced_colors"],
            "contrast": config["contrast"],
            "offline": config["offline"],
            "network_profile": config["network_profile"],
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
            self._verify_executable_binding(config)
            browser_type = getattr(playwright, config["engine"])
            browser = browser_type.launch(**self._launch_options(config))
            context = browser.new_context(**self._context_options(config))
            page = context.new_page()
            cdp = None
            if config["engine"] == "chromium":
                cdp = context.new_cdp_session(page)
                cdp.send("Network.enable")
                cdp.send("Network.setCacheDisabled", {"cacheDisabled": True})
                cdp.send("Accessibility.enable")
                cdp.send("Performance.enable")
                throttle = _NETWORK_PROFILES.get(config["network_profile"])
                if throttle:
                    cdp.send("Network.emulateNetworkConditions", throttle)

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
                if config["offline"]:
                    context.set_offline(True)
                    page.evaluate(
                        """() => {
                          dispatchEvent(new Event("offline"));
                          return {online: navigator.onLine};
                        }"""
                    )
                snapshot = page.evaluate(_PAGE_SNAPSHOT_SCRIPT)
                viewport_image = page.screenshot(type="png", full_page=False)
                full_image = (
                    page.screenshot(type="png", full_page=True)
                    if config["full_page"]
                    else viewport_image
                )
                aria_snapshot = page.locator("body").aria_snapshot(
                    timeout=config["timeout_ms"]
                )
                semantic_accessibility = page.evaluate(_SEMANTIC_ACCESSIBILITY_SCRIPT)
                keyboard_journey = self._keyboard_journey(page, config)
                if cdp is not None:
                    dom_snapshot = {
                        "format": "chromium-cdp-dom-snapshot/v1",
                        "engine": config["engine"],
                        "snapshot": cdp.send(
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
                        ),
                    }
                    accessibility = {
                        "format": "chromium-cdp-plus-playwright-aria/v1",
                        "engine": config["engine"],
                        "aria_snapshot": aria_snapshot,
                        "semantic_snapshot": semantic_accessibility,
                        "cdp_tree": cdp.send("Accessibility.getFullAXTree"),
                    }
                    performance = {
                        "format": "chromium-cdp-performance/v1",
                        "metrics": cdp.send("Performance.getMetrics"),
                    }
                else:
                    dom_snapshot = {
                        "format": "portable-layout-snapshot/v1",
                        "engine": config["engine"],
                        "document": snapshot["document"],
                        "graph": snapshot["graph"],
                    }
                    accessibility = {
                        "format": "playwright-aria-plus-dom-semantics/v1",
                        "engine": config["engine"],
                        "aria_snapshot": aria_snapshot,
                        "semantic_snapshot": semantic_accessibility,
                    }
                    performance = {
                        "format": "web-performance-api/v1",
                        "entries": page.evaluate(
                            """() => ({
                              navigation: performance.getEntriesByType("navigation")
                                .map(entry => entry.toJSON()),
                              paint: performance.getEntriesByType("paint")
                                .map(entry => entry.toJSON()),
                              resources: performance.getEntriesByType("resource")
                                .map(entry => entry.toJSON()),
                            })"""
                        ),
                    }
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
            "browser_engine": config["engine"],
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
        emit_json("accessibility.journey", keyboard_journey)
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
                "browser_engine": config["engine"],
                "browser_version": browser_version,
                "node_count": len(graph["nodes"]),
                "edge_count": len(graph["edges"]),
                "surface_count": len(snapshot["surfaces"]),
                "network_event_count": len(network),
                "console_message_count": len(console),
                "accessibility_issue_count": len(semantic_accessibility["issues"]),
                "accessibility_critical_or_serious_count": sum(
                    issue["impact"] in {"critical", "serious"}
                    for issue in semantic_accessibility["issues"]
                ),
                "keyboard_journey_status": keyboard_journey["status"],
                "environment_state": semantic_accessibility["media"],
                "network_profile": config["network_profile"],
            },
            limitations=[
                "LayoutGraph covers the captured document and same-origin DOM exposed to the page.",
                "Closed shadow roots and cross-origin frame internals remain opaque.",
                "Network evidence records metadata but deliberately omits response bodies.",
                "Playwright ARIA and DOM semantics are screen-reader-oriented evidence, not "
                "a claim of parity with a physical assistive-technology/browser pairing.",
                "CDP DOM, accessibility, performance, and bandwidth evidence is available "
                "only for Chromium; other engines emit portable, explicitly labeled forms.",
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
    def _launch_options(config: dict[str, Any]) -> dict[str, Any]:
        launch: dict[str, Any] = {
            "headless": config["headless"],
            "timeout": config["launch_timeout_ms"],
        }
        if config["engine"] == "chromium":
            launch["args"] = [
                "--disable-background-networking",
                "--disable-component-update",
                "--disable-default-apps",
                "--disable-sync",
                "--metrics-recording-only",
                "--no-first-run",
            ]
        if config["executable_path"]:
            launch["executable_path"] = config["executable_path"]
        elif config["channel"]:
            launch["channel"] = config["channel"]
        return launch

    @staticmethod
    def _context_options(
        config: dict[str, Any],
        *,
        reduced_motion: str | None = None,
        has_touch: bool | None = None,
        offline: bool | None = None,
    ) -> dict[str, Any]:
        options: dict[str, Any] = {
            "viewport": config["viewport"],
            "screen": config["screen"],
            "device_scale_factor": config["device_scale_factor"],
            "locale": config["locale"],
            "timezone_id": config["timezone_id"],
            "color_scheme": config["color_scheme"],
            "reduced_motion": reduced_motion or config["reduced_motion"],
            "forced_colors": config["forced_colors"],
            "contrast": config["contrast"],
            "service_workers": "block",
            "ignore_https_errors": config["ignore_https_errors"],
            "has_touch": config["has_touch"] if has_touch is None else has_touch,
            # Offline capture is an online bootstrap followed by a governed
            # transition so the already-loaded application can expose its
            # actual offline state.
            "offline": False if offline is None else offline,
        }
        if config["engine"] != "firefox":
            options["is_mobile"] = config["is_mobile"]
        return options

    @classmethod
    def _keyboard_journey(
        cls, page: Any, config: dict[str, Any]
    ) -> dict[str, Any]:
        page.evaluate("() => document.activeElement && document.activeElement.blur()")
        steps: list[dict[str, Any]] = []
        first_selector: str | None = None
        status = "BOUNDED"
        for index in range(config["keyboard_step_limit"]):
            page.keyboard.press("Tab")
            active = page.evaluate(_ACTIVE_ELEMENT_SCRIPT)
            if active is None:
                status = "FOCUS_UNAVAILABLE"
                break
            active["index"] = index
            selector = active["selector"]
            if first_selector is None:
                first_selector = selector
            elif selector == first_selector:
                status = "COMPLETE_CYCLE"
                break
            steps.append(active)
            if selector in {"body", "html > body:nth-of-type(1)"}:
                status = "COMPLETE_DOCUMENT" if len(steps) > 1 else "FOCUS_LEFT_DOCUMENT"
                break
        return {
            "schema": "vision.keyboard-journey/v1",
            "status": status,
            "step_limit": config["keyboard_step_limit"],
            "steps": steps,
            "unique_focus_targets": sorted({step["selector"] for step in steps}),
            "authority": "OBSERVED",
        }

    @staticmethod
    def _resolve_browser_executable(
        *,
        engine: str,
        channel: str | None,
        executable_path: str | None,
    ) -> str | None:
        if executable_path:
            return executable_path
        channel_paths = {
            ("chromium", "chrome"): (
                "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
            ),
            ("chromium", "msedge"): (
                "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"
            ),
        }
        channel_path = channel_paths.get((engine, channel))
        if channel_path and Path(channel_path).is_file():
            return str(Path(channel_path).resolve())
        try:
            from playwright.sync_api import sync_playwright

            with sync_playwright() as playwright:
                return str(Path(getattr(playwright, engine).executable_path))
        except Exception:
            return None

    @staticmethod
    def _verify_executable_binding(config: dict[str, Any]) -> None:
        path_value = config["resolved_executable_path"]
        expected = config["executable_sha256"]
        if not path_value or not expected:
            raise RuntimeError(
                f"{config['engine']} browser executable is unavailable; run "
                f"`playwright install {config['engine']}` or provide executable_path"
            )
        path = Path(path_value)
        if not path.is_file():
            raise RuntimeError(f"bound browser executable disappeared: {path}")
        observed, _size = sha256_file(path)
        if observed != expected:
            raise RuntimeError(
                f"bound browser executable digest changed: expected {expected}, "
                f"observed {observed}"
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
        if config["resolved_executable_path"]:
            candidates.append(config["resolved_executable_path"])
        for executable in candidates:
            try:
                process = subprocess.run(
                    [executable, "--version"],
                    check=False,
                    capture_output=True,
                    text=True,
                    timeout=5,
                )
                version = (process.stdout or process.stderr).strip()
                if process.returncode == 0 and version:
                    return version
            except (OSError, subprocess.SubprocessError):
                continue
        return f"{config['engine']}:{config['channel'] or 'managed'}:resolved-at-launch"

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
