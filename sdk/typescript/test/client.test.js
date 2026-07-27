import { test } from "node:test";
import assert from "node:assert/strict";

import {
  Client,
  MercAPIError,
  MercJobFailed,
  VERSION,
  isEmbeddingsBinary,
  decodeEmbeddingsBinary,
} from "../dist/index.js";

/** Build a merc binary embeddings blob: magic, count, dim, then float32 rows. */
function embedBlob(rows) {
  const count = rows.length;
  const dim = rows[0]?.length ?? 0;
  const buf = new ArrayBuffer(16 + count * dim * 4);
  const u8 = new Uint8Array(buf);
  const view = new DataView(buf);
  u8.set([0x43, 0x58, 0x45, 0x4d], 0); // CXEM
  view.setUint32(4, count, true);
  view.setUint32(8, dim, true);
  let off = 16;
  for (const row of rows) {
    for (const v of row) {
      view.setFloat32(off, v, true);
      off += 4;
    }
  }
  return u8;
}

/** A fetch stub that records what it was called with. */
function stubFetch(handler) {
  const calls = [];
  const f = async (url, init) => {
    calls.push({ url, init });
    return handler(url, init, calls.length);
  };
  f.calls = calls;
  return f;
}

const json = (body, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });

test("version matches the Python SDK", () => {
  assert.equal(VERSION, "0.1.0");
});

test("binary embeddings round trip", () => {
  const rows = [
    [1, -2.5, 0],
    [0.5, 0.25, 1e-3],
  ];
  const blob = embedBlob(rows);
  assert.ok(isEmbeddingsBinary(blob));

  const got = decodeEmbeddingsBinary(blob);
  assert.equal(got.length, 2);
  for (let i = 0; i < rows.length; i++) {
    for (let j = 0; j < rows[i].length; j++) {
      assert.ok(Math.abs(got[i][j] - rows[i][j]) < 1e-6, `row ${i} col ${j}`);
    }
  }
});

test("a non-merc payload is not decoded as embeddings", () => {
  const notOurs = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]);
  assert.equal(isEmbeddingsBinary(notOurs), false);
  assert.throws(() => decodeEmbeddingsBinary(notOurs), /not a merc binary embeddings blob/);
  // Too short to even carry the magic.
  assert.equal(isEmbeddingsBinary(new Uint8Array([0x43, 0x58])), false);
});

test("a truncated blob is refused rather than returning short rows", () => {
  const blob = embedBlob([[1, 2, 3]]);
  assert.throws(() => decodeEmbeddingsBinary(blob.slice(0, 20)), /truncated/);
});

test("base url trailing slashes do not produce a double slash", async () => {
  const f = stubFetch(() => json({ job_id: "j1", status: "queued" }));
  const c = new Client({ baseUrl: "http://host:8080///", apiKey: "k", fetch: f });
  await c.getJob("j1");
  assert.equal(f.calls[0].url, "http://host:8080/v1/jobs/j1");
});

test("api key is sent as a bearer token, and omitted when absent", async () => {
  const f = stubFetch(() => json({}));
  await new Client({ apiKey: "merc_test_x", fetch: f }).models();
  assert.equal(f.calls[0].init.headers.authorization, "Bearer merc_test_x");

  const g = stubFetch(() => json({}));
  await new Client({ fetch: g }).models();
  assert.equal(g.calls[0].init.headers.authorization, undefined);
});

test("job ids are encoded, so a hostile id cannot alter the path", async () => {
  const f = stubFetch(() => json({}));
  await new Client({ baseUrl: "http://h", fetch: f }).getJob("../../admin/workers");
  assert.ok(!f.calls[0].url.includes("/admin/workers"), f.calls[0].url);
  assert.ok(f.calls[0].url.includes("%2F"), f.calls[0].url);
});

test("a single input is normalised to a list, matching the Python SDK", async () => {
  const f = stubFetch(() => json({ job_id: "j", status: "queued" }));
  const c = new Client({ fetch: f });
  await c.submitJob({ model: "m", job_type: "embed", input: "one" });
  const sent = JSON.parse(f.calls[0].init.body);
  assert.deepEqual(sent.input, ["one"]);
  assert.equal(sent.tier, "batch");
});

test("an HTTP failure surfaces status and path rather than a bare throw", async () => {
  const f = stubFetch(() => json({ error: "nope" }, 402));
  const c = new Client({ fetch: f });
  await assert.rejects(
    () => c.models(),
    (err) => {
      assert.ok(err instanceof MercAPIError);
      assert.equal(err.status, 402);
      assert.equal(err.path, "/v1/models");
      assert.deepEqual(err.body, { error: "nope" });
      return true;
    },
  );
});

test("wait() throws on a failed job instead of returning it", async () => {
  const f = stubFetch(() => json({ job_id: "j", status: "failed" }));
  const c = new Client({ fetch: f });
  await assert.rejects(() => c.wait("j", { pollMs: 1 }), MercJobFailed);
});

test("wait() returns the completed job", async () => {
  const f = stubFetch((_u, _i, n) =>
    json({ job_id: "j", status: n < 3 ? "running" : "complete" }),
  );
  const c = new Client({ fetch: f });
  const job = await c.wait("j", { pollMs: 1 });
  assert.equal(job.status, "complete");
  assert.ok(f.calls.length >= 3);
});

test("wait() gives up rather than polling forever", async () => {
  const f = stubFetch(() => json({ job_id: "j", status: "running" }));
  const c = new Client({ fetch: f });
  await assert.rejects(() => c.wait("j", { pollMs: 1, timeoutMs: 20 }), /timed out/);
});

test("a non-callable fetch is rejected at construction, not at first request", () => {
  // globalThis.fetch exists on Node 18+, so `null` correctly falls through to
  // it. The guard that matters is refusing something that is not callable.
  assert.throws(() => new Client({ fetch: 123 }), /no fetch/);
  assert.throws(() => new Client({ fetch: "nope" }), /no fetch/);
  // And with no override the platform fetch is adopted silently.
  assert.doesNotThrow(() => new Client());
});
