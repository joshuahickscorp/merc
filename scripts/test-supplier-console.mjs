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

// Captured from GET /v1/worker/{earnings,connect/status,verification} with a
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
  "/v1/worker/verification": {
    honeypots_passed: 0, honeypots_failed: 0, verification_label: "unverified",
  },
};

/** Enough DOM for the page: element text, class, hidden, and one submit. */
function run(responses, token) {
  const els = new Map();
  const el = (id) => {
    // #console carries the `hidden` attribute in the markup; the stub must
    // start in the same state or "console stayed hidden" proves nothing.
    if (!els.has(id)) {
      els.set(id, { id, textContent: "", className: "", value: "",
                    hidden: /id="console"[^>]*\shidden/.test(html) && id === "console" });
    }
    return els.get(id);
  };
  el("token").value = token;

  let submit;
  el("auth-form").addEventListener = (_ev, fn) => { submit = fn; };

  const requests = [];
  const ctx = {
    document: { getElementById: el },
    fetch: async (path, init) => {
      requests.push({ path, init });
      const body = responses[path];
      if (body === undefined) return { ok: false, status: 404, text: async () => "{}" };
      return { ok: true, status: 200, text: async () => JSON.stringify(body) };
    },
    Number, JSON, Error, String, Object,
  };

  new Function(...Object.keys(ctx), script)(...Object.values(ctx));
  return { el, requests, submit: () => submit({ preventDefault() {} }) };
}

const t = run(RESPONSES, "dev-worker-token-0001");
t.submit();
await new Promise((r) => setImmediate(r));
await new Promise((r) => setImmediate(r));
await new Promise((r) => setImmediate(r));

// The credential is a worker token, not a bearer token.
for (const { init } of t.requests) {
  const h = init.headers;
  assert.equal(h["x-worker-token"], "dev-worker-token-0001", "worker token header");
  assert.equal(h.authorization, undefined, "must not send Authorization: worker auth rejects it");
}
assert.equal(t.requests.length, 3, "earnings, rail and verification are all fetched");

// Money is shown at ledger granularity, not rounded to cents: a supplier with
// $0.003701 accruing must not be shown "$0.00".
assert.equal(t.el("paid").textContent, "$0.010000");
assert.equal(t.el("lifetime").textContent, "$0.013701");
assert.equal(t.el("carried").textContent, "$0.003701");
assert.match(t.el("carry-note").textContent, /under \$0\.01/);

// An unconfigured deployment must not be described as having a connected account.
assert.match(t.el("connect-status").textContent, /not configured/);
assert.doesNotMatch(t.el("connect-status").textContent, /connected account present/);
assert.match(t.el("verification-status").textContent, /^unverified/);

// The raw panel is the supplier's reconciliation path; it must be the real bodies.
assert.deepEqual(JSON.parse(t.el("raw").textContent).earnings, RESPONSES["/v1/worker/earnings"]);
assert.equal(t.el("console").hidden, false);
assert.equal(t.el("signin").hidden, true);

// Each rail state says something different, and three of the four mean "not paid yet".
const rail = (status) => {
  const s = run({ ...RESPONSES, "/v1/worker/connect/status": status }, "tok");
  s.submit();
  return s;
};
const enabled = rail({ configured: true, connected: true, payouts_enabled: true });
const pending = rail({ configured: true, connected: true, payouts_enabled: false });
const unlinked = rail({ configured: true, connected: false, payouts_enabled: false });
await new Promise((r) => setImmediate(r));
await new Promise((r) => setImmediate(r));
await new Promise((r) => setImmediate(r));
assert.match(enabled.el("connect-status").textContent, /able to receive payouts/);
assert.match(pending.el("connect-status").textContent, /has not enabled payouts/);
assert.match(unlinked.el("connect-status").textContent, /no payout account is connected/);

// A bad token must say so rather than opening an empty console.
const denied = run({}, "wrong");
denied.submit();
await new Promise((r) => setImmediate(r));
assert.equal(denied.el("console").hidden, true, "console must not open when the token is refused");
assert.equal(denied.el("signin").hidden, false, "sign-in stays up when the token is refused");
assert.match(denied.el("auth-status").textContent, /could not load earnings|not accepted/);

console.log("supplier console: worker-token header, sub-cent money, 4 rail states and refusal path verified");
