#!/usr/bin/env node
// Runs clients/web/prices.html's own script against a supplied public catalogue
// publication envelope and prints the schedule-derived prices the PAGE renders.
//
// The browser intentionally has no market-board pricing implementation. This
// harness proves it reads only price_authority.catalogue and cannot turn raw
// observations under market_evidence into an independently calculated price.

import { readFileSync } from "node:fs";

const html = readFileSync(new URL("../../clients/web/prices.html", import.meta.url), "utf8");
const script = html.match(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/i)[1];
const stdin = readFileSync(0, "utf8").trim();
if (!stdin) throw new Error("a public catalogue publication envelope is required on stdin");
const publication = JSON.parse(stdin);

function makeEl(tag) {
  return {
    tag,
    className: "",
    textContent: "",
    colSpan: 0,
    children: [],
    append(...kids) { this.children.push(...kids); },
    replaceChildren(...kids) { this.children = kids; },
  };
}

const byId = {
  "catalogue-rows": makeEl("tbody"),
  "observation-rows": makeEl("tbody"),
  "board-meta": makeEl("p"),
};
let fetchedPath = "";
const ctx = {
  document: {
    getElementById: (id) => byId[id],
    createElement: (tag) => makeEl(tag),
  },
  fetch: async (path) => {
    fetchedPath = path;
    if (path !== "/pricing/board.json") throw new Error("unexpected fetch " + path);
    return { ok: true, status: 200, json: async () => publication };
  },
  URL, Number, JSON, Error, String, Object, Math,
};

await new Function(...Object.keys(ctx), `return (async () => { ${script} })()`)(
  ...Object.values(ctx),
);
for (let i = 0; i < 5; i++) await new Promise((resolve) => setImmediate(resolve));

const prices = {};
for (const tr of byId["catalogue-rows"].children) {
  const cells = tr.children;
  if (cells.length < 5) continue;
  const price = Number(String(cells[4].textContent).split(" ")[0]);
  if (Number.isFinite(price)) prices[cells[0].textContent] = price;
}
process.stdout.write(JSON.stringify({
  fetched_path: fetchedPath,
  prices,
  meta: byId["board-meta"].textContent,
  catalogue_rows: byId["catalogue-rows"].children.map((tr) =>
    tr.children.map((td) => td.textContent)),
}) + "\n");
