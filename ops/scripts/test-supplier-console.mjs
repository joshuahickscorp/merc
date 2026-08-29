#!/usr/bin/env node
// Runs clients/web/supplier.html's script against recorded control-plane responses.
//
// This exists because the console shipped sending `Authorization: Bearer`
// while src/control/api.go authWorker reads `X-Worker-Token` and rejects anything
// else: the page could never have authenticated, and nothing in the Go suite
// or site-build would have said so. The header assertion below is the point.
//
// The response bodies are verbatim captures from a local control plane, so a
// field rename on the Go side shows up here as changed console text rather
// than as a wrong number in front of a supplier.

import { readFileSync } from "node:fs";
import assert from "node:assert/strict";

const html = readFileSync(new URL("../../clients/web/supplier.html", import.meta.url), "utf8");
const script = html.match(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/i)[1];

// Captured from GET /v1/worker/{earnings,connect/status,viability,verification}
// with a real worker token, against a supplier holding a sub-cent carry.
// Ledger body shape matches src/control/types.go PayoutLedger (currency + entries);
// the rows are a structural fixture for the payout-ledger table, not a live
// capture — GET /v1/worker/ledger?limit=50 is the production path the console
// actually calls (handleWorkerLedger default/clamp is 50).
const LEDGER = {
  currency: "USD",
  entries: [
    {
      id: "7c2a1b90-4d3e-4f6a-9b1c-2e8f0a4d6c11",
      kind: "supplier_credit",
      amount_usd: 0.01,
      currency: "USD",
      payout_status: "held",
      task_id: "3a91c0e2-8b74-4d15-a6f0-1c9e5b2d8407",
      job_id: "f0e1d2c3-b4a5-9687-0f1e-2d3c4b5a6978",
      created_at: "2026-07-25T16:32:36Z",
    },
    {
      id: "0b19e4d7-2c8a-41f3-90d6-5e7a1c3b8f24",
      kind: "clawback",
      amount_usd: 0.003701,
      currency: "USD",
      payout_status: "ready",
      task_id: "3a91c0e2-8b74-4d15-a6f0-1c9e5b2d8407",
      job_id: "f0e1d2c3-b4a5-9687-0f1e-2d3c4b5a6978",
      created_at: "2026-07-25T16:40:00Z",
    },
  ],
};

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
  "/v1/worker/ledger?limit=50": LEDGER,
};

const PUBLIC_CONFIG = {
  settlement_currency: "USD",
  contacts: { configured: false, missing: ["support", "security"] },
};

/** Enough DOM for the page: element text, attributes, table rows and listeners. */
function node() {
  const n = {
    className: "", value: "", dataset: {}, children: [],
    _text: "",
    get textContent() { return this._text; },
    set textContent(v) { this._text = String(v); this.children = []; },
    append(...kids) { this.children.push(...kids); },
  };
  return n;
}

function run(responses, token) {
  const els = new Map();
  const listeners = new Map();
  const el = (id) => {
    // Console containers carry the `hidden` attribute in the markup; the stub
    // must start in the same state or an "opened" assertion proves nothing.
    if (!els.has(id)) {
      const n = node();
      n.id = id;
      n.hidden = new RegExp(`id="${id}"[^>]*\\shidden`).test(html);
      n.addEventListener = (event, listener) => listeners.set(`${id}:${event}`, listener);
      els.set(id, n);
    }
    return els.get(id);
  };
  el("worker-token").value = token;

  const requests = [];
  const ctx = {
    document: { getElementById: el, createElement: () => node() },
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
// (service leases) and then a sixth (payout ledger) while a stale list still
// read "5", so the drift was a failing build rather than a caught regression.
// Naming the paths means adding or removing one fails here with the path in
// the message. The ledger path includes the production ?limit=50 query.
assert.deepEqual(
  workerRequests.map(({ path }) => path).sort(),
  [
    "/v1/worker/connect/status",
    "/v1/worker/earnings",
    "/v1/worker/ledger?limit=50",
    "/v1/worker/service-leases/active",
    "/v1/worker/verification",
    "/v1/worker/viability",
  ],
  "earnings, rail, payout ledger, service leases, viability and verification are all fetched",
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
assert.deepEqual(raw.ledger, LEDGER, "raw panel carries the payout ledger body");
assert.equal(t.el("worker-console").hidden, false);

// Payout ledger table: newest-first trail at ledger granularity, both kinds.
assert.equal(t.el("ledger-status").textContent, "2 row(s)");
const ledgerRows = t.el("ledger-body").children;
assert.equal(ledgerRows.length, 2, "one table row per ledger entry");
assert.deepEqual(
  ledgerRows.map((row) => row.children.map((cell) => cell.textContent)),
  [
    ["2026-07-25T16:32:36Z", "supplier_credit", "0.010000 USD", "held", "3a91c0e2-8b74-4d15-a6f0-1c9e5b2d8407", "f0e1d2c3-b4a5-9687-0f1e-2d3c4b5a6978"],
    ["2026-07-25T16:40:00Z", "clawback", "0.003701 USD", "ready", "3a91c0e2-8b74-4d15-a6f0-1c9e5b2d8407", "f0e1d2c3-b4a5-9687-0f1e-2d3c4b5a6978"],
  ],
  "ledger cells are when/kind/amount/status/task/job at six-decimal money",
);

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

// One failing panel must not blank the others. The console fetched every
// worker panel with Promise.all and wrote every field afterwards, so a
// single erroring endpoint erased earnings, payout rail, ledger and
// verification too — a supplier whose lease panel broke could not see what
// they were owed.
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
assert.equal(partial.el("ledger-status").textContent, "2 row(s)", "payout ledger survives a failing sibling panel");

const ledgerDown = run(
  { "/v1/public/config": PUBLIC_CONFIG, ...RESPONSES, "/v1/worker/ledger?limit=50": { __status: 500, __body: { error: "ledger store unavailable" } } },
  "dev-worker-token-0001",
);
ledgerDown.trigger("worker-login", "submit");
await flush();
assert.equal(ledgerDown.el("worker-console").hidden, false, "console still opens when the ledger panel fails");
assert.equal(ledgerDown.el("paid").textContent, "0.010000 USD", "earnings survive a failing ledger panel");
assert.match(ledgerDown.el("ledger-status").textContent, /unavailable/, "the failing ledger panel says so");
assert.equal(ledgerDown.el("ledger-body").children.length, 0, "a failed ledger leaves the table empty");

const emptyLedger = run(
  { "/v1/public/config": PUBLIC_CONFIG, ...RESPONSES, "/v1/worker/ledger?limit=50": { currency: "USD", entries: [] } },
  "dev-worker-token-0001",
);
emptyLedger.trigger("worker-login", "submit");
await flush();
assert.equal(emptyLedger.el("worker-console").hidden, false, "console opens with an empty payable trail");
assert.equal(emptyLedger.el("ledger-status").textContent, "No payable rows yet.");
assert.equal(emptyLedger.el("ledger-body").children.length, 0);

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

console.log("supplier console: owner and worker scopes, sub-cent money, payout ledger, 4 rail states and refusal path verified");
