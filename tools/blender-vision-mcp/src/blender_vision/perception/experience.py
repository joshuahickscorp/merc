from __future__ import annotations

import hashlib
from typing import Any

from blender_vision.core.util import canonical_json
from blender_vision.perception.browser import _PAGE_SNAPSHOT_SCRIPT, BrowserAdapter
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome

_ACTIONABLE_SCRIPT = r"""
() => {
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
  return [
    ...document.querySelectorAll(
      "a[href],button,input,select,textarea,summary,[role=tab],[role=button],[tabindex]"
    ),
  ].map(element => {
    const bounds = element.getBoundingClientRect();
    return {
      selector: selectorFor(element),
      tag: element.localName,
      type: element.getAttribute("type"),
      role: element.getAttribute("role"),
      text: (element.innerText || element.value || element.getAttribute("aria-label") || "")
        .replace(/\s+/g, " ").trim().slice(0, 512),
      disabled: Boolean(element.disabled || element.getAttribute("aria-disabled") === "true"),
      visible: (
        bounds.width > 0 &&
        bounds.height > 0 &&
        getComputedStyle(element).visibility !== "hidden"
      ),
      sourceBinding: {
        id: element.id || null,
        component: element.getAttribute("data-component"),
        source: element.getAttribute("data-source"),
      },
      bounds: {
        x: bounds.x,
        y: bounds.y,
        width: bounds.width,
        height: bounds.height,
      },
    };
  });
}
"""

_MOTION_SETUP_SCRIPT = r"""
() => {
  const selectorFor = element => element.id
    ? `#${CSS.escape(element.id)}`
    : element.localName;
  const animations = document.getAnimations({subtree: true});
  return animations.map((animation, index) => {
    animation.pause();
    const effect = animation.effect;
    const target = effect && effect.target;
    const timing = effect && effect.getTiming ? effect.getTiming() : {};
    const computed = effect && effect.getComputedTiming ? effect.getComputedTiming() : {};
    const keyframes = effect && effect.getKeyframes ? effect.getKeyframes() : [];
    return {
      id: `animation:${index}`,
      selector: target ? selectorFor(target) : null,
      animationName: animation.animationName || null,
      playState: animation.playState,
      timing,
      computedTiming: {
        delay: computed.delay,
        duration: computed.duration,
        endTime: computed.endTime,
        activeDuration: computed.activeDuration,
        iterations: computed.iterations,
      },
      keyframes,
    };
  });
}
"""

_MOTION_SAMPLE_SCRIPT = r"""
async ({timestamp, scrollProgress}) => {
  for (const animation of document.getAnimations({subtree: true})) {
    animation.pause();
    const timing = animation.effect && animation.effect.getComputedTiming
      ? animation.effect.getComputedTiming()
      : {};
    const end = Number(timing.endTime);
    animation.currentTime = Number.isFinite(end) ? Math.min(timestamp, end) : timestamp;
  }
  if (scrollProgress !== null) {
    const maximum = Math.max(0, document.documentElement.scrollHeight - innerHeight);
    scrollTo(0, maximum * scrollProgress);
  }
  await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  const elements = new Set(document.querySelectorAll("[data-motion]"));
  for (const animation of document.getAnimations({subtree: true})) {
    if (animation.effect && animation.effect.target) elements.add(animation.effect.target);
  }
  return {
    timestamp,
    requestAnimationFrameIndex: Math.round(timestamp / (1000 / 60)),
    scrollOffset: {x: scrollX, y: scrollY},
    scrollProgress,
    pointerState: "idle",
    elements: [...elements].map(element => {
      const bounds = element.getBoundingClientRect();
      const style = getComputedStyle(element);
      let matrix = null;
      try {
        const transform = new DOMMatrixReadOnly(style.transform);
        matrix = [
          transform.a, transform.b, transform.c, transform.d, transform.e, transform.f,
        ];
      } catch {}
      return {
        selector: element.id ? `#${CSS.escape(element.id)}` : element.localName,
        bounds: {
          x: bounds.x,
          y: bounds.y,
          width: bounds.width,
          height: bounds.height,
        },
        transform: style.transform,
        transformMatrix2d: matrix,
        opacity: Number.parseFloat(style.opacity),
        filter: style.filter,
        clipPath: style.clipPath,
        mask: style.mask,
        borderRadius: style.borderRadius,
        color: style.color,
        visibility: style.visibility,
        zIndex: style.zIndex,
        position: style.position,
        stickyOrFixed: style.position === "sticky" || style.position === "fixed",
      };
    }),
    animations: document.getAnimations({subtree: true}).map((animation, index) => ({
      id: `animation:${index}`,
      currentTime: animation.currentTime,
      playState: animation.playState,
    })),
  };
}
"""


class BrowserExperienceAdapter(BrowserAdapter):
    name = "browser.experience"
    version = "1"

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        normalized = super().normalize_config(target, config)
        viewports = config.get(
            "responsive_viewports",
            [
                {"width": 360, "height": 800},
                {"width": 768, "height": 800},
                {"width": 1280, "height": 800},
            ],
        )
        if not isinstance(viewports, list) or not 2 <= len(viewports) <= 8:
            raise ValueError("responsive_viewports must contain between 2 and 8 entries")
        normalized_viewports = []
        for viewport in viewports:
            width, height = int(viewport["width"]), int(viewport["height"])
            if not 320 <= width <= 7680 or not 240 <= height <= 4320:
                raise ValueError("responsive viewport is outside the supported range")
            normalized_viewports.append({"width": width, "height": height})
        normalized["responsive_viewports"] = sorted(
            normalized_viewports, key=lambda item: (item["width"], item["height"])
        )
        normalized["action_limit"] = max(1, min(int(config.get("action_limit", 24)), 64))
        normalized["timeline_duration_ms"] = max(
            100, min(int(config.get("timeline_duration_ms", 1200)), 10_000)
        )
        normalized["timeline_step_ms"] = max(
            16, min(int(config.get("timeline_step_ms", 100)), 1000)
        )
        normalized["scroll_steps"] = max(2, min(int(config.get("scroll_steps", 5)), 21))
        normalized["input_modes"] = [
            mode
            for mode in ("pointer", "keyboard", "touch")
            if mode in set(config.get("input_modes", ["pointer", "keyboard", "touch"]))
        ]
        if not normalized["input_modes"]:
            raise ValueError("at least one supported input mode is required")
        normalized["capture_reduced_motion_variant"] = bool(
            config.get("capture_reduced_motion_variant", True)
        )
        return normalized

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            **super().environment(config),
            "capture_mode": "experience",
            "experience_script_version": self.version,
            "input_modes": config["input_modes"],
            "responsive_viewports": config["responsive_viewports"],
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        from playwright.sync_api import sync_playwright

        def emit_json(
            role: str,
            value: Any,
            metadata: dict[str, Any] | None = None,
        ) -> dict[str, Any]:
            return sink(role, canonical_json(self._redact(value)), "application/json", metadata)

        with sync_playwright() as playwright:
            launch = self._launch_options(config)
            browser = playwright.chromium.launch(**launch)
            context = browser.new_context(**self._context_options(config, has_touch=True))
            page = context.new_page()
            self._govern_page(page, config)
            page.goto(
                target["url"],
                wait_until=config["wait_until"],
                timeout=config["timeout_ms"],
            )
            self._settle_static(page)
            state_graph, interaction_graph = self._discover_states(
                page, target, config, sink, emit_json
            )
            responsive_graph = self._capture_responsive(page, target, config, sink)
            motion_graph = self._capture_motion(page, target, config, sink, browser)
            context.close()
            browser.close()

        action_inventory = state_graph.pop("_action_inventory")
        state_graph["action_inventory_role"] = "actionable.inventory"
        emit_json("state.graph", state_graph)
        emit_json("interaction.graph", interaction_graph)
        emit_json("responsive.graph", responsive_graph)
        emit_json("motion.graph", motion_graph)
        emit_json("actionable.inventory", action_inventory)
        return CaptureOutcome(
            summary={
                "state_count": len(state_graph["nodes"]),
                "state_transition_count": len(state_graph["edges"]),
                "interaction_count": len(interaction_graph["edges"]),
                "responsive_observation_count": len(responsive_graph["nodes"]),
                "responsive_transition_count": len(responsive_graph["edges"]),
                "motion_timeline_count": len(motion_graph["timelines"]),
                "motion_track_count": len(motion_graph["nodes"]),
            },
            limitations=[
                "Exploration is bounded and never proves that undiscovered states do not exist.",
                "Only actually rendered post-input states receive OBSERVED state authority.",
                "External handler source symbols remain unknown unless the page exposes bindings.",
            ],
            graphs=[
                self._graph_descriptor("StateGraph", "state.graph", state_graph),
                self._graph_descriptor(
                    "InteractionGraph", "interaction.graph", interaction_graph
                ),
                self._graph_descriptor("ResponsiveGraph", "responsive.graph", responsive_graph),
                self._graph_descriptor("MotionGraph", "motion.graph", motion_graph),
            ],
        )

    def _discover_states(
        self,
        page: Any,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
        emit_json: Any,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        state_nodes: dict[str, dict[str, Any]] = {}
        state_edges: list[dict[str, Any]] = []
        interaction_edges: list[dict[str, Any]] = []
        action_inventory = [
            action
            for action in page.evaluate(_ACTIONABLE_SCRIPT)
            if action["visible"] and not action["disabled"]
        ][: config["action_limit"]]
        activatable = [
            action
            for action in action_inventory
            if action["tag"] in {"button", "a", "summary"}
            or action["role"] in {"button", "tab"}
            or action["type"] in {"button", "submit", "checkbox", "radio"}
        ]

        self._observe_state(
            page, state_nodes, sink, "state.000.baseline", cause={"type": "initial"}
        )
        sequence = 1
        for mode in config["input_modes"]:
            for action in activatable:
                page.goto(
                    target["url"],
                    wait_until=config["wait_until"],
                    timeout=config["timeout_ms"],
                )
                self._settle_static(page)
                before_id = self._state_identity(page.evaluate(_PAGE_SNAPSHOT_SCRIPT))
                locator = page.locator(action["selector"]).first
                try:
                    if mode == "pointer":
                        locator.hover(timeout=config["timeout_ms"])
                        hover_id = self._observe_state(
                            page,
                            state_nodes,
                            sink,
                            f"state.{sequence:03d}.pointer-hover",
                            cause={"type": "hover", "target": action["selector"]},
                        )
                        sequence += 1
                        self._transition(
                            state_edges,
                            interaction_edges,
                            before_id,
                            hover_id,
                            action,
                            mode,
                            "hover",
                            state_nodes,
                        )
                        bounds = locator.bounding_box()
                        if bounds is None:
                            continue
                        page.mouse.move(
                            bounds["x"] + bounds["width"] / 2,
                            bounds["y"] + bounds["height"] / 2,
                        )
                        page.mouse.down()
                        pressed_id = self._observe_state(
                            page,
                            state_nodes,
                            sink,
                            f"state.{sequence:03d}.pointer-pressed",
                            cause={"type": "pressed", "target": action["selector"]},
                        )
                        sequence += 1
                        self._transition(
                            state_edges,
                            interaction_edges,
                            hover_id,
                            pressed_id,
                            action,
                            mode,
                            "pressed",
                            state_nodes,
                        )
                        page.mouse.up()
                        start_id = pressed_id
                        event = "click"
                    elif mode == "keyboard":
                        locator.focus(timeout=config["timeout_ms"])
                        focus_id = self._observe_state(
                            page,
                            state_nodes,
                            sink,
                            f"state.{sequence:03d}.keyboard-focus",
                            cause={"type": "focus", "target": action["selector"]},
                        )
                        sequence += 1
                        self._transition(
                            state_edges,
                            interaction_edges,
                            before_id,
                            focus_id,
                            action,
                            mode,
                            "focus",
                            state_nodes,
                        )
                        locator.press("Enter")
                        start_id = focus_id
                        event = "Enter"
                    else:
                        bounds = locator.bounding_box()
                        if bounds is None:
                            continue
                        page.touchscreen.tap(
                            bounds["x"] + bounds["width"] / 2,
                            bounds["y"] + bounds["height"] / 2,
                        )
                        start_id = before_id
                        event = "tap"
                    page.wait_for_timeout(25)
                    after_id = self._observe_state(
                        page,
                        state_nodes,
                        sink,
                        f"state.{sequence:03d}.{mode}-activated",
                        cause={"type": event, "target": action["selector"]},
                    )
                    sequence += 1
                    self._transition(
                        state_edges,
                        interaction_edges,
                        start_id,
                        after_id,
                        action,
                        mode,
                        event,
                        state_nodes,
                    )
                except Exception as error:
                    interaction_edges.append(
                        {
                            "id": f"interaction:{len(interaction_edges)}",
                            "type": "TRIGGERS",
                            "input": {"mode": mode, "event": "activation"},
                            "event_target": action["selector"],
                            "status": "FAILED",
                            "error": {"type": type(error).__name__, "message": str(error)},
                            "authority": "OBSERVED",
                            "confidence": 1.0,
                            "evidence_references": [],
                        }
                    )

        return (
            {
                "schema": "vision.state-graph/v1",
                "graph_type": "StateGraph",
                "authority": "OBSERVED",
                "target": target,
                "nodes": list(state_nodes.values()),
                "edges": state_edges,
                "_action_inventory": action_inventory,
            },
            {
                "schema": "vision.interaction-graph/v1",
                "graph_type": "InteractionGraph",
                "authority": "OBSERVED",
                "target": target,
                "nodes": [
                    {
                        "id": action["selector"],
                        "domain_type": "ActionableElement",
                        "spatial_bounds": action["bounds"],
                        "temporal_validity": "baseline",
                        "evidence_references": ["actionable.inventory"],
                        "authority": "OBSERVED",
                        "confidence": 1.0,
                        "source_restrictions": ["public-runtime-only"],
                        "uncertainty": [],
                        "revision_lineage": [],
                        **action,
                    }
                    for action in action_inventory
                ],
                "edges": interaction_edges,
            },
        )

    def _observe_state(
        self,
        page: Any,
        state_nodes: dict[str, dict[str, Any]],
        sink: ArtifactSink,
        role: str,
        *,
        cause: dict[str, Any],
    ) -> str:
        snapshot = page.evaluate(_PAGE_SNAPSHOT_SCRIPT)
        identity = self._state_identity(snapshot)
        image = page.screenshot(type="png", full_page=False)
        evidence = sink(
            role,
            image,
            "image/png",
            {"state_id": identity, "cause": cause, "url": self._redact_url(page.url)},
        )
        visible = self._visible_projection(snapshot)
        if identity not in state_nodes:
            state_nodes[identity] = {
                "id": identity,
                "domain_type": "WebState",
                "spatial_bounds": {
                    "x": 0,
                    "y": 0,
                    "width": snapshot["document"]["viewport"]["width"],
                    "height": snapshot["document"]["viewport"]["height"],
                },
                "temporal_validity": "captured-interaction-session",
                "evidence_references": [
                    {"role": role, "artifact_digest": evidence["digest"]}
                ],
                "authority": "OBSERVED",
                "confidence": 1.0,
                "source_restrictions": ["runtime-observation"],
                "uncertainty": [],
                "revision_lineage": [],
                "url": self._redact_url(snapshot["document"]["url"]),
                "visible_elements": visible,
                "observation_causes": [cause],
            }
        else:
            state_nodes[identity]["evidence_references"].append(
                {"role": role, "artifact_digest": evidence["digest"]}
            )
            state_nodes[identity]["observation_causes"].append(cause)
        return identity

    def _transition(
        self,
        state_edges: list[dict[str, Any]],
        interaction_edges: list[dict[str, Any]],
        source: str,
        target: str,
        action: dict[str, Any],
        mode: str,
        event: str,
        nodes: dict[str, dict[str, Any]],
    ) -> None:
        before = nodes.get(source, {}).get("visible_elements", [])
        after = nodes.get(target, {}).get("visible_elements", [])
        delta = self._state_delta(before, after)
        observed = source != target or bool(delta["changed"])
        edge = {
            "id": f"transition:{len(state_edges)}",
            "source": source,
            "target": target,
            "type": "TRANSITIONS_TO",
            "input": {"mode": mode, "event": event},
            "event_target": action["selector"],
            "observed_visual_change": observed,
            "delta": delta,
            "evidence_references": nodes.get(target, {}).get("evidence_references", [])[-1:],
            "authority": "OBSERVED",
            "confidence": 1.0,
        }
        state_edges.append(edge)
        interaction_edges.append(
            {
                "id": f"interaction:{len(interaction_edges)}",
                "source": action["selector"],
                "target": target,
                "type": "TRIGGERS",
                "input": {"mode": mode, "event": event},
                "event_target": action["selector"],
                "handler_or_state_transition": {
                    "source_binding": action["sourceBinding"],
                    "runtime_handler": "not_exposed",
                },
                "dom_or_scene_mutation": delta,
                "visual_effect": {"observed": observed, "state_id": target},
                "network_or_storage_side_effect": "not_instrumented",
                "status": "OBSERVED",
                "authority": "OBSERVED",
                "confidence": 1.0,
                "evidence_references": edge["evidence_references"],
            }
        )

    def _capture_responsive(
        self,
        page: Any,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> dict[str, Any]:
        nodes = []
        for index, viewport in enumerate(config["responsive_viewports"]):
            page.set_viewport_size(viewport)
            page.goto(
                target["url"],
                wait_until=config["wait_until"],
                timeout=config["timeout_ms"],
            )
            self._settle_static(page)
            snapshot = page.evaluate(_PAGE_SNAPSHOT_SCRIPT)
            role = f"responsive.{index:02d}.{viewport['width']}x{viewport['height']}"
            evidence = sink(
                role,
                page.screenshot(type="png", full_page=False),
                "image/png",
                {"viewport": viewport},
            )
            nodes.append(
                {
                    "id": f"viewport:{viewport['width']}x{viewport['height']}",
                    "domain_type": "ResponsiveObservation",
                    "spatial_bounds": {"x": 0, "y": 0, **viewport},
                    "temporal_validity": "static",
                    "evidence_references": [
                        {"role": role, "artifact_digest": evidence["digest"]}
                    ],
                    "authority": "OBSERVED",
                    "confidence": 1.0,
                    "source_restrictions": ["runtime-observation"],
                    "uncertainty": [],
                    "revision_lineage": [],
                    "viewport": viewport,
                    "elements": self._visible_projection(snapshot),
                }
            )
        edges = []
        for left, right in zip(nodes, nodes[1:], strict=False):
            delta = self._state_delta(left["elements"], right["elements"])
            edges.append(
                {
                    "id": f"responsive-transition:{len(edges)}",
                    "source": left["id"],
                    "target": right["id"],
                    "type": "TRANSITIONS_TO",
                    "width_interval": [
                        left["viewport"]["width"],
                        right["viewport"]["width"],
                    ],
                    "reflow": delta,
                    "authority": "OBSERVED",
                    "confidence": 1.0,
                    "evidence_references": right["evidence_references"],
                }
            )
        return {
            "schema": "vision.responsive-graph/v1",
            "graph_type": "ResponsiveGraph",
            "authority": "OBSERVED",
            "target": target,
            "nodes": nodes,
            "edges": edges,
            "input_mode_variants": {
                "pointer": "observed",
                "keyboard": "observed",
                "touch": "observed" if "touch" in config["input_modes"] else "not_captured",
            },
        }

    def _capture_motion(
        self,
        page: Any,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
        browser: Any,
    ) -> dict[str, Any]:
        page.set_viewport_size(config["viewport"])
        page.goto(
            target["url"],
            wait_until=config["wait_until"],
            timeout=config["timeout_ms"],
        )
        animations = page.evaluate(_MOTION_SETUP_SCRIPT)
        duration = config["timeline_duration_ms"]
        step = config["timeline_step_ms"]
        timestamps = list(range(0, duration + 1, step))
        if timestamps[-1] != duration:
            timestamps.append(duration)
        animation_samples = []
        for index, timestamp in enumerate(timestamps):
            sample = page.evaluate(
                _MOTION_SAMPLE_SCRIPT,
                {"timestamp": timestamp, "scrollProgress": None},
            )
            role = f"motion.animation.{index:03d}"
            evidence = sink(
                role,
                page.screenshot(type="png", full_page=False),
                "image/png",
                {"timestamp_ms": timestamp, "timeline": "animation"},
            )
            sample["frame_artifact"] = evidence["digest"]
            animation_samples.append(sample)

        scroll_samples = []
        for index in range(config["scroll_steps"]):
            progress = index / (config["scroll_steps"] - 1)
            sample = page.evaluate(
                _MOTION_SAMPLE_SCRIPT,
                {"timestamp": 0, "scrollProgress": progress},
            )
            role = f"motion.scroll.{index:03d}"
            evidence = sink(
                role,
                page.screenshot(type="png", full_page=False),
                "image/png",
                {"scroll_progress": progress, "timeline": "scroll"},
            )
            sample["frame_artifact"] = evidence["digest"]
            scroll_samples.append(sample)

        reduced_variant: dict[str, Any] | None = None
        if config["capture_reduced_motion_variant"]:
            reduced_context = browser.new_context(
                **self._context_options(config, reduced_motion="reduce", has_touch=False)
            )
            reduced_page = reduced_context.new_page()
            self._govern_page(reduced_page, config)
            reduced_page.goto(
                target["url"],
                wait_until=config["wait_until"],
                timeout=config["timeout_ms"],
            )
            reduced_animations = reduced_page.evaluate(_MOTION_SETUP_SCRIPT)
            role = "motion.reduced"
            evidence = sink(
                role,
                reduced_page.screenshot(type="png", full_page=False),
                "image/png",
                {"reduced_motion": "reduce"},
            )
            reduced_variant = {
                "animations": reduced_animations,
                "frame_artifact": evidence["digest"],
                "authority": "OBSERVED",
            }
            reduced_context.close()

        tracks = self._motion_tracks(animation_samples, scroll_samples, animations)
        return {
            "schema": "vision.motion-graph/v1",
            "graph_type": "MotionGraph",
            "authority": "OBSERVED",
            "target": target,
            "nodes": tracks,
            "edges": self._motion_edges(tracks),
            "timelines": [
                {
                    "id": "timeline:animation",
                    "domain_type": "DeterministicAnimationTimeline",
                    "timestamps_ms": timestamps,
                    "samples": animation_samples,
                    "authority": "OBSERVED",
                },
                {
                    "id": "timeline:scroll",
                    "domain_type": "ScrollTimeline",
                    "progress": [
                        index / (config["scroll_steps"] - 1)
                        for index in range(config["scroll_steps"])
                    ],
                    "samples": scroll_samples,
                    "authority": "OBSERVED",
                },
            ],
            "animations": animations,
            "inference": self._infer_motion(animation_samples, scroll_samples, animations),
            "reduced_motion_variant": reduced_variant,
            "replay_contract": {
                "interpolation": "linear-between-observed-samples",
                "extrapolation": "forbidden",
                "coordinate_space": "CSS viewport pixels and 2D transform matrices",
            },
        }

    @staticmethod
    def _launch_options(config: dict[str, Any]) -> dict[str, Any]:
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
        return launch

    @staticmethod
    def _context_options(
        config: dict[str, Any],
        *,
        reduced_motion: str | None = None,
        has_touch: bool,
    ) -> dict[str, Any]:
        return {
            "viewport": config["viewport"],
            "device_scale_factor": config["device_scale_factor"],
            "locale": config["locale"],
            "timezone_id": config["timezone_id"],
            "color_scheme": config["color_scheme"],
            "reduced_motion": reduced_motion or config["reduced_motion"],
            "service_workers": "block",
            "ignore_https_errors": config["ignore_https_errors"],
            "has_touch": has_touch,
        }

    def _govern_page(self, page: Any, config: dict[str, Any]) -> None:
        def route_request(route: Any) -> None:
            parsed_url = route.request.url
            if parsed_url.startswith(("data:", "blob:", "about:")):
                route.continue_()
                return
            try:
                if self._normalize_origin(parsed_url) not in config["allowed_origins"]:
                    raise PermissionError("origin is not allowlisted")
                self._enforce_network_policy(
                    parsed_url, allow_private=config["allow_private_network"]
                )
            except (PermissionError, ValueError):
                route.abort("blockedbyclient")
                return
            route.continue_()

        page.route("**/*", route_request)

    @staticmethod
    def _settle_static(page: Any) -> None:
        page.evaluate(
            """() => document.getAnimations({subtree: true}).forEach(animation => {
                animation.pause();
                animation.currentTime = 0;
            })"""
        )

    @staticmethod
    def _visible_projection(snapshot: dict[str, Any]) -> list[dict[str, Any]]:
        return [
            {
                "selector": node["selector"],
                "role": node["role"],
                "text": node["text"],
                "bounds": node["bounds"],
                "styles": node["styles"],
                "attributes": {
                    key: value
                    for key, value in node["attributes"].items()
                    if key.startswith("aria-")
                    or key in {"class", "hidden", "open", "disabled", "checked", "value"}
                },
                "sourceBinding": node["sourceBinding"],
            }
            for node in snapshot["graph"]["nodes"]
            if node["bounds"]["width"] > 0
            and node["bounds"]["height"] > 0
            and node["styles"]["display"] != "none"
            and node["styles"]["visibility"] != "hidden"
        ]

    @classmethod
    def _state_identity(cls, snapshot: dict[str, Any]) -> str:
        projection = {
            "url": cls._redact_url(snapshot["document"]["url"]),
            "visible": cls._visible_projection(snapshot),
        }
        return f"state:{hashlib.sha256(canonical_json(projection)).hexdigest()}"

    @staticmethod
    def _state_delta(
        before: list[dict[str, Any]], after: list[dict[str, Any]]
    ) -> dict[str, Any]:
        left = {item["selector"]: item for item in before}
        right = {item["selector"]: item for item in after}
        changed = [
            selector
            for selector in sorted(left.keys() & right.keys())
            if left[selector] != right[selector]
        ]
        return {
            "added": sorted(right.keys() - left.keys()),
            "removed": sorted(left.keys() - right.keys()),
            "changed": changed,
        }

    @staticmethod
    def _motion_tracks(
        animation_samples: list[dict[str, Any]],
        scroll_samples: list[dict[str, Any]],
        animations: list[dict[str, Any]],
    ) -> list[dict[str, Any]]:
        selectors = sorted(
            {
                element["selector"]
                for sample in animation_samples + scroll_samples
                for element in sample["elements"]
            }
        )
        animation_map = {
            animation["selector"]: animation
            for animation in animations
            if animation.get("selector")
        }
        tracks = []
        for selector in selectors:
            tracks.append(
                {
                    "id": f"motion:{selector}",
                    "domain_type": "ElementMotionTrack",
                    "spatial_bounds": "per-sample",
                    "temporal_validity": "captured-timelines",
                    "evidence_references": [
                        sample["frame_artifact"]
                        for sample in animation_samples + scroll_samples
                    ],
                    "authority": "OBSERVED",
                    "confidence": 1.0,
                    "source_restrictions": ["runtime-observation"],
                    "uncertainty": [],
                    "revision_lineage": [],
                    "selector": selector,
                    "animation": animation_map.get(selector),
                    "animation_samples": [
                        {
                            "timestamp": sample["timestamp"],
                            **next(
                                element
                                for element in sample["elements"]
                                if element["selector"] == selector
                            ),
                        }
                        for sample in animation_samples
                        if any(
                            element["selector"] == selector
                            for element in sample["elements"]
                        )
                    ],
                    "scroll_samples": [
                        {
                            "progress": sample["scrollProgress"],
                            "scrollOffset": sample["scrollOffset"],
                            **next(
                                element
                                for element in sample["elements"]
                                if element["selector"] == selector
                            ),
                        }
                        for sample in scroll_samples
                        if any(
                            element["selector"] == selector
                            for element in sample["elements"]
                        )
                    ],
                }
            )
        return tracks

    @staticmethod
    def _motion_edges(tracks: list[dict[str, Any]]) -> list[dict[str, Any]]:
        edges = []
        for track in tracks:
            if track["animation"]:
                edges.append(
                    {
                        "source": track["animation"]["id"],
                        "target": track["id"],
                        "type": "DRIVEN_BY",
                        "driver": "css-or-waapi-animation",
                        "authority": "OBSERVED",
                    }
                )
            if track["scroll_samples"]:
                edges.append(
                    {
                        "source": "timeline:scroll",
                        "target": track["id"],
                        "type": "FOLLOWS_SCROLL",
                        "authority": "OBSERVED",
                    }
                )
        return edges

    @staticmethod
    def _infer_motion(
        animation_samples: list[dict[str, Any]],
        scroll_samples: list[dict[str, Any]],
        animations: list[dict[str, Any]],
    ) -> dict[str, Any]:
        parallax = []
        if len(scroll_samples) >= 2:
            first, last = scroll_samples[0], scroll_samples[-1]
            scroll_delta = last["scrollOffset"]["y"] - first["scrollOffset"]["y"]
            first_map = {item["selector"]: item for item in first["elements"]}
            last_map = {item["selector"]: item for item in last["elements"]}
            if scroll_delta:
                for selector in sorted(first_map.keys() & last_map.keys()):
                    start_matrix = first_map[selector].get("transformMatrix2d")
                    end_matrix = last_map[selector].get("transformMatrix2d")
                    transform_delta = (
                        end_matrix[5] - start_matrix[5]
                        if start_matrix and end_matrix
                        else 0.0
                    )
                    parallax.append(
                        {
                            "selector": selector,
                            "transform_y_per_scroll_y": transform_delta / scroll_delta,
                            "viewport_y_per_scroll_y": (
                                last_map[selector]["bounds"]["y"]
                                - first_map[selector]["bounds"]["y"]
                            )
                            / scroll_delta,
                        }
                    )
        delays = [
            {
                "selector": animation.get("selector"),
                "delay": animation.get("timing", {}).get("delay", 0),
            }
            for animation in animations
        ]
        sticky = []
        for sample in scroll_samples:
            for element in sample["elements"]:
                if element["stickyOrFixed"]:
                    sticky.append(
                        {
                            "selector": element["selector"],
                            "progress": sample["scrollProgress"],
                            "y": element["bounds"]["y"],
                            "position": element["position"],
                        }
                    )
        return {
            "keyframes": [
                {
                    "selector": animation.get("selector"),
                    "keyframes": animation.get("keyframes", []),
                }
                for animation in animations
            ],
            "timings": [
                {
                    "selector": animation.get("selector"),
                    "timing": animation.get("timing", {}),
                    "computedTiming": animation.get("computedTiming", {}),
                }
                for animation in animations
            ],
            "stagger_order": sorted(delays, key=lambda item: item["delay"]),
            "parallax": parallax,
            "sticky_or_pinned_samples": sticky,
            "movement_classification": {
                "DOM_tracks": len(
                    {
                        element["selector"]
                        for sample in animation_samples + scroll_samples
                        for element in sample["elements"]
                    }
                ),
                "camera_motion": "not_observed",
                "object_motion": "not_applicable_to_DOM",
                "viewport_or_scroll_motion": bool(scroll_samples),
            },
        }

    @staticmethod
    def _graph_descriptor(
        graph_type: str, role: str, graph: dict[str, Any]
    ) -> dict[str, Any]:
        return {
            "graph_type": graph_type,
            "role": role,
            "node_count": len(graph.get("nodes", [])),
            "edge_count": len(graph.get("edges", [])),
            "authority": "OBSERVED",
        }


class MotionGraphReplay:
    """Independent, bounded interpolation over observed MotionGraph samples."""

    def __init__(self, graph: dict[str, Any]):
        if graph.get("graph_type") != "MotionGraph":
            raise ValueError("MotionGraphReplay requires a MotionGraph")
        self.graph = graph

    def sample(self, selector: str, timestamp_ms: float) -> dict[str, Any]:
        track = next(
            (node for node in self.graph["nodes"] if node["selector"] == selector),
            None,
        )
        if track is None:
            raise KeyError(selector)
        samples = track["animation_samples"]
        if not samples:
            raise ValueError(f"{selector} has no animation samples")
        if timestamp_ms < samples[0]["timestamp"] or timestamp_ms > samples[-1]["timestamp"]:
            raise ValueError("MotionGraph replay forbids extrapolation")
        for left, right in zip(samples, samples[1:], strict=False):
            if left["timestamp"] <= timestamp_ms <= right["timestamp"]:
                span = right["timestamp"] - left["timestamp"]
                ratio = 0.0 if span == 0 else (timestamp_ms - left["timestamp"]) / span
                return {
                    "selector": selector,
                    "timestamp": timestamp_ms,
                    "opacity": self._lerp(left["opacity"], right["opacity"], ratio),
                    "transformMatrix2d": [
                        self._lerp(start, end, ratio)
                        for start, end in zip(
                            left["transformMatrix2d"],
                            right["transformMatrix2d"],
                            strict=True,
                        )
                    ]
                    if left["transformMatrix2d"] and right["transformMatrix2d"]
                    else None,
                    "bounds": {
                        key: self._lerp(left["bounds"][key], right["bounds"][key], ratio)
                        for key in ("x", "y", "width", "height")
                    },
                    "authority": "DERIVED",
                    "evidence_interval": [left["timestamp"], right["timestamp"]],
                }
        return samples[-1]

    @staticmethod
    def _lerp(left: float, right: float, ratio: float) -> float:
        return left + (right - left) * ratio
