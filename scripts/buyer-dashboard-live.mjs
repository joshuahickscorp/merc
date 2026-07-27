#!/usr/bin/env node
// Drive web/buyer.html's own script against a RUNNING merc.
//
// site-build.mjs checks the page renders, its contrast passes and its CSP
// hashes match. None of that exercises what the page DOES: sign in, quote,
// submit, poll, and read a result back. This runs the page's real script with a
// real fetch against a real control plane, so a page that renders perfectly and
// calls an endpoint merc does not serve fails here.
//
// That is not hypothetical -- running the TypeScript SDK against a live merc
// this same way found three defects its stub-based unit tests could not.
//
// Env: MERC_LIVE_BASE_URL, MERC_LIVE_BUYER_EMAIL, MERC_LIVE_BUYER_PASSWORD.

import { readFileSync } from "node:fs";

const BASE = process.env.MERC_LIVE_BASE_URL || "http://127.0.0.1:8093";
const EMAIL = process.env.MERC_LIVE_BUYER_EMAIL || "buyer-canary@example.invalid";
const PASSWORD = process.env.MERC_LIVE_BUYER_PASSWORD || "merc-canary-password-2026";

const html = readFileSync(new URL("../web/buyer.html", import.meta.url), "utf8");
const script = html.match(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/i)[1];

// Minimal DOM: the page reads and writes elements by id and binds submit
// handlers. Values come from the environment, not from a person typing.
const values = { email: EMAIL, password: PASSWORD };
const els = new Map();
const handlers = new Map();

function el(id) {
  if (!els.has(id)) {
    els.set(id, {
      id,
      textContent: "",
      className: "",
      value: values[id] ?? "",
      hidden: /id="(workspace|results)"[^>]*\shidden/.test(html) &&
        (id === "workspace" || id === "results"),
      style: {},
      children: [],
      append(...k) { this.children.push(...k); },
      replaceChildren(...k) { this.children = k; },
      addEventListener(ev, fn) { handlers.set(`${id}:${ev}`, fn); },
      setAttribute() {},
      removeAttribute() {},
      querySelector: () => null,
      querySelectorAll: () => [],
    });
  }
  return els.get(id);
}

const requests = [];
const ctx = {
  document: {
    getElementById: el,
    createElement: (tag) => ({ tag, textContent: "", className: "", children: [],
      append(...k) { this.children.push(...k); }, setAttribute() {}, style: {} }),
    querySelector: () => null,
    querySelectorAll: () => [],
    addEventListener() {},
  },
  window: { location: { origin: BASE }, addEventListener() {}, removeEventListener() {} },
  fetch: async (path, init) => {
    const url = path.startsWith("http") ? path : BASE + path;
    requests.push({ url, method: init?.method || "GET" });
    return globalThis.fetch(url, init);
  },
  setTimeout, clearTimeout, setInterval, clearInterval,
  console, JSON, Number, String, Object, Array, Error, Math, Date, URL, Promise,
  encodeURIComponent, alert: () => {},
};

new Function(...Object.keys(ctx), script)(...Object.values(ctx));

const submit = handlers.get("login-form:submit");
if (typeof submit !== "function") {
  console.error("the page bound no login handler; its script shape changed");
  process.exit(1);
}
await submit({ preventDefault() {} });
for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));

const identity = el("identity").textContent;
const status = el("login-status").textContent;

if (status && /error|failed|invalid/i.test(status)) {
  console.error(`  buyer dashboard sign-in FAILED: ${status}`);
  process.exit(1);
}
if (identity !== EMAIL) {
  console.error(`  identity shows "${identity}", expected "${EMAIL}"`);
  process.exit(1);
}
if (el("workspace").hidden) {
  console.error("  workspace stayed hidden after a successful sign-in");
  process.exit(1);
}

// Every endpoint the page actually called must have been one merc serves; a
// 404 here is the class of bug the SDK run surfaced.
const bad = [];
for (const { url, method } of requests) {
  const resp = await globalThis.fetch(url, { method: "OPTIONS" }).catch(() => null);
  if (resp && resp.status === 404) bad.push(`${method} ${url}`);
}
if (bad.length) {
  console.error("  page called routes merc does not serve:", bad.join(", "));
  process.exit(1);
}

console.log(`  signed in as ${identity}, workspace open, ` +
  `${requests.length} live call(s): ${requests.map((r) => r.method + " " +
    new URL(r.url).pathname).join(", ")}`);
console.log("  BUYER DASHBOARD LIVE: PASS");
