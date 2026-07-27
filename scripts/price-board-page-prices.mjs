#!/usr/bin/env node
// Runs web/prices.html's own script against the real pricing/board.json and
// prints the price the PAGE would display for each class.
//
// The page tells visitors it recomputes the weighted median itself, so they can
// check merc's arithmetic rather than trust it. That promise is only worth
// anything if the page's answer matches the server's. This exists so a Go test
// can compare the two: two implementations of the same rule drift silently, and
// a price board that disagrees with the prices actually charged is worse than
// no board.
//
// Output: {"<class>": <usd per 1k>, ...} on stdout.

import { readFileSync } from "node:fs";

const html = readFileSync(new URL("../web/prices.html", import.meta.url), "utf8");
const script = html.match(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/i)[1];
// A board may be supplied on stdin. The shipped board has too few observations
// for the confidence weights to move the median, so a parity check against it
// alone cannot detect the two implementations disagreeing about weighting --
// the Go test feeds constructed boards where the weights decide the answer.
const stdin = readFileSync(0, "utf8").trim();
const board = stdin
  ? JSON.parse(stdin)
  : JSON.parse(readFileSync(new URL("../pricing/board.json", import.meta.url), "utf8"));

// Enough DOM for the page: it builds rows with createElement/append and reads
// three elements by id.
const rows = { "catalogue-rows": [], "observation-rows": [] };
let current = null;

function makeEl(tag) {
  const el = {
    tag,
    className: "",
    textContent: "",
    colSpan: 0,
    children: [],
    append(...kids) { this.children.push(...kids); },
    replaceChildren(...kids) { this.children = kids; },
  };
  return el;
}

const byId = {
  "catalogue-rows": makeEl("tbody"),
  "observation-rows": makeEl("tbody"),
  "board-meta": makeEl("p"),
};

const ctx = {
  document: {
    getElementById: (id) => byId[id],
    createElement: (tag) => makeEl(tag),
  },
  fetch: async (path) => {
    if (!path.includes("board.json")) throw new Error("unexpected fetch " + path);
    return { ok: true, status: 200, json: async () => board };
  },
  URL, Number, JSON, Error, String, Object, Math,
};

await new Function(...Object.keys(ctx), `return (async () => { ${script} })()`)(
  ...Object.values(ctx),
);
// The page's own IIFE is async; give its awaits a turn to settle.
for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));

const out = {};
for (const tr of byId["catalogue-rows"].children) {
  const cells = tr.children;
  if (cells.length < 5) continue;
  const name = cells[0].textContent;
  const priceText = cells[4].textContent;
  if (priceText === "not priced") { out[name] = null; continue; }
  out[name] = Number(priceText.replace(/^\$/, ""));
}
process.stdout.write(JSON.stringify(out) + "\n");
