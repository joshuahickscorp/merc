#!/usr/bin/env node
// Runs web/supplier.html's script against recorded control-plane responses.
//
// This exists because the console shipped sending `Authorization: Bearer`
// while control/api.go authWorker reads `X-Worker-Token` and rejects anything
// else: the page could never have authenticated, and nothing in the Go suite
// or site-build would have said so. The header assertion below is the point.
//
// The response bodies are verbatim captures from a local control plane, so a
// field rename on the Go side shows up here as changed console text rather
// than as a wrong number in front of a supplier.

import { readFileSync } from "node:fs";
import assert from "node:assert/strict";

const html = readFileSync(new URL("../web/supplier.html", import.meta.url), "utf8");
const script = html.match(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/i)[1];

// Captured from GET /v1/worker/{earnings,connect/status,viability,verification} with a
// real worker token, against a supplier holding a sub-cent carry.
const RESPONSES = {
  "/v1/worker/earnings": {
    balance_usd: 0.01, lifetime_usd: 0.013701, carried_usd: 0.003701,
    last_payout_usd: 0.01, last_payout_at: 1785165156,
  },
  "/v1/worker/connect/status": {
    configured: false, connected: false, payouts_enabled: false,
    credential_id: "eb5ab425-43ca-44f8-8ba2-868de2f6898f", credential_version: 1,
  },
  "/v1/worker/viability": {
    eligible: true, reasons: [], minimum_earnings_usd: 0.01,
  },
  "/v1/worker/verification": {
    honeypots_passed: 0, honeypots_failed: 0, verification_label: "unverified",
  },
  "/v1/worker/service-leases/active": [],
};

const PUBLIC_CONFIG = {
  settlement_currency: "USD",
  contacts: { configured: false, missing: ["support", "security"] },
};

/** Enough DOM for the page: element text, attributes and event listeners. */
function run(responses, token) {
  const els = new Map();
  const listeners = new Map();
  const el = (id) => {
    // Console containers carry the `hidden` attribute in the markup; the stub
    // must start in the same state or an "opened" assertion proves nothing.
    if (!els.has(id)) {
      els.set(id, {
        id, textContent: "", className: "", value: "", dataset: {},
        hidden: new RegExp(`id="${id}"[^>]*\\shidden`).test(html),
        addEventListener: (event, listener) => listeners.set(`${id}:${event}`, listener),
      });
    }
    return els.get(id);
  };
  el("worker-token").value = token;

  const requests = [];
  const ctx = {
    document: { getElementById: el, createElement: () => ({ append() {}, dataset: {} }) },
    window: { addEventListener() {} },
    location: { assign() {} },
    fetch: async (path, init) => {
      requests.push({ path, init });
      const response = responses[path];
      if (response === undefined) return { ok: false, status: 404, text: async () => '{"error":"worker token not accepted"}' };
      const status = response.__status || 200;
      const body = response.__body || response;
      return { ok: status < 400, status, text: async () => JSON.stringify(body) };
    },
    Number, JSON, Error, String, Object,
  };

  new Function(...Object.keys(ctx), script)(...Object.values(ctx));
  return {
    el,
    requests,
    trigger: (id, event) => {
      const listener = listeners.get(`${id}:${event}`);
      assert.ok(listener, `${id} ${event} listener is registered`);
      return listener({ preventDefault() {} });
    },
  };
}

const flush = async () => {
  for (let i = 0; i < 4; i++) await new Promise((resolve) => setImmediate(resolve));
};

const t = run({ "/v1/public/config": PUBLIC_CONFIG, ...RESPONSES }, "dev-worker-token-0001");
t.trigger("worker-login", "submit");
await flush();

// The credential is a worker token, not a bearer token.
const workerRequests = t.requests.filter(({ path }) => path.startsWith("/v1/worker/"));
for (const { init } of workerRequests) {
  const h = init.headers;
  assert.equal(h["X-Worker-Token"], "dev-worker-token-0001", "worker token header");
  assert.equal(h.authorization, undefined, "must not send Authorization: worker auth rejects it");
}
// The SET, not the count. A bare count let the console grow a fifth fetch
// (service leases) while this assertion still read "4", so the drift was a
// failing build rather than a caught regression. Naming the paths means adding
// or removing one fails here with the path in the message.
assert.deepEqual(
  workerRequests.map(({ path }) => path).sort(),
  [
    "/v1/worker/connect/status",
    "/v1/worker/earnings",
    "/v1/worker/service-leases/active",
    "/v1/worker/verification",
    "/v1/worker/viability",
  ],
  "earnings, rail, service leases, viability and verification are all fetched",
);
const publicRequest = t.requests.find(({ path }) => path === "/v1/public/config");
assert.equal(publicRequest.init.headers["X-Worker-Token"], undefined, "public config has no worker credential");

// Money is shown at ledger granularity, not rounded to cents: a supplier with
// $0.003701 accruing must not be shown "$0.00".
assert.equal(t.el("paid").textContent, "0.010000 USD");
assert.equal(t.el("lifetime").textContent, "0.013701 USD");
assert.equal(t.el("carried").textContent, "0.003701 USD");
assert.match(t.el("carry-note").textContent, /below one minor unit \(0\.01 USD\)/);

// An unconfigured deployment must not be described as having a connected account.
assert.match(t.el("connect-status").textContent, /not configured/);
assert.doesNotMatch(t.el("connect-status").textContent, /connected account present/);
assert.match(t.el("verification-status").textContent, /^unverified/);

// The raw panel is the supplier's reconciliation path; it must be the real bodies.
const raw = JSON.parse(t.el("worker-raw").textContent);
assert.deepEqual(raw.earnings, RESPONSES["/v1/worker/earnings"]);
assert.deepEqual(raw.viability, RESPONSES["/v1/worker/viability"]);
assert.equal(t.el("worker-console").hidden, false);

// Each rail state says something different, and three of the four mean "not paid yet".
const rail = (status) => {
  const s = run({ "/v1/public/config": PUBLIC_CONFIG, ...RESPONSES, "/v1/worker/connect/status": status }, "tok");
  s.trigger("worker-login", "submit");
  return s;
};
const enabled = rail({ configured: true, connected: true, payouts_enabled: true });
const pending = rail({ configured: true, connected: true, payouts_enabled: false });
const unlinked = rail({ configured: true, connected: false, payouts_enabled: false });
await flush();
assert.match(enabled.el("connect-status").textContent, /payouts enabled/);
assert.match(pending.el("connect-status").textContent, /payouts remain disabled/);
assert.match(unlinked.el("connect-status").textContent, /no payout account is connected/i);

// A bad token must say so rather than opening an empty console.
const denied = run({ "/v1/public/config": PUBLIC_CONFIG }, "wrong");
denied.trigger("worker-login", "submit");
await flush();
assert.equal(denied.el("worker-console").hidden, true, "console must not open when the token is refused");
assert.match(denied.el("worker-status").textContent, /not accepted/);

// One failing panel must not blank the others. The console fetched five
// endpoints with Promise.all and wrote every field afterwards, so a single
// erroring endpoint erased earnings, payout rail and verification too — a
// supplier whose lease panel broke could not see what they were owed.
const partial = run(
  { "/v1/public/config": PUBLIC_CONFIG, ...RESPONSES, "/v1/worker/service-leases/active": { __status: 500, __body: { error: "lease store unavailable" } } },
  "dev-worker-token-0001",
);
partial.trigger("worker-login", "submit");
await flush();
assert.equal(partial.el("worker-console").hidden, false, "console still opens when one panel fails");
assert.equal(partial.el("paid").textContent, "0.010000 USD", "earnings survive a failing sibling panel");
assert.match(partial.el("connect-status").textContent, /not configured/, "payout rail survives a failing sibling panel");
assert.match(partial.el("service-assignments-status").textContent, /unavailable/, "the failing panel says so");

// Earnings failing is still fatal: a console that cannot show what is owed has
// nothing to show, and must not present an authoritative-looking empty page.
const noEarnings = run(
  { "/v1/public/config": PUBLIC_CONFIG, ...RESPONSES, "/v1/worker/earnings": { __status: 500, __body: { error: "ledger unavailable" } } },
  "dev-worker-token-0001",
);
noEarnings.trigger("worker-login", "submit");
await flush();
assert.equal(noEarnings.el("worker-console").hidden, true, "console must not open without earnings");

// Supplier ownership is a separate buyer-authenticated surface. It must never
// reuse a device token or accidentally attach worker scope to owner operations.
const owner = run({
  "/v1/public/config": PUBLIC_CONFIG,
  "/v1/login": { token: "owner-session-token" },
  "/v1/me": { email: "supplier-owner@example.test" },
  "/v1/supplier/status": { connect_status: "pending", payouts_enabled: false },
  "/v1/supplier/worker-credentials": { credentials: [] },
  "/v1/supplier/credential-audit": { events: [] },
}, "unrelated-worker-token");
owner.el("owner-email").value = "supplier-owner@example.test";
owner.el("owner-password").value = "not-a-real-password";
owner.trigger("owner-login", "submit");
await flush();
assert.equal(owner.el("owner-console").hidden, false, "owner console opens after authenticated owner login");
assert.equal(owner.el("owner-identity").textContent, "supplier-owner@example.test");
assert.match(owner.el("owner-status").textContent, /owner controls ready/);
assert.match(owner.el("connect-owner-status").textContent, /Stripe Connect: pending/);
const ownerRequests = owner.requests.filter(({ path }) =>
  path === "/v1/me" || path.startsWith("/v1/supplier/"));
assert.equal(ownerRequests.length, 4, "owner loads identity, payout status, credentials and audit");
for (const { init } of ownerRequests) {
  assert.equal(init.headers.Authorization, "Bearer owner-session-token", "owner bearer scope");
  assert.equal(init.headers["X-Worker-Token"], undefined, "owner calls never carry device scope");
}

console.log("supplier console: owner and worker scopes, sub-cent money, 4 rail states and refusal path verified");
