#!/usr/bin/env node
// Drive the TypeScript SDK against a RUNNING merc, end to end.
//
// Running this the first time found three defects the stub-based unit tests
// could not: no Idempotency-Key (merc answers 400, so the client could not
// submit a job at all), input sent as an array when merc requires a JSONL
// string, and cancelJob calling a /cancel route merc does not serve.

import { Client } from "../sdk/typescript/dist/index.js";
const c = new Client({ baseUrl: "http://127.0.0.1:8093", apiKey: "dev-api-key-0001" });
const models = await c.models();
console.log("  models:", (models.data ?? models).slice(0, 2).map((m) => m.id));
const rows = Array.from({ length: 50 }, (_, i) =>
  JSON.stringify({ text: `ts sdk live proof row ${i}` })).join("\n");
const job = await c.submitJob({
  model: { kind: "hf", ref: "all-minilm-l6-v2" },
  job_type: { type: "embed" },
  input: rows, tier: "batch", max_usd: 1.0,
}, { idempotencyKey: `merc-ts-sdk-live-${Date.now()}` });
console.log("  submitted:", job.job_id?.slice(0, 8), "tasks:", job.task_count);
const done = await c.wait(job.job_id, { pollMs: 2000, timeoutMs: 240000 });
console.log("  status:", done.status, "actual_usd:", done.actual_usd,
            "verification:", done.verification?.label);
if (done.status !== "complete") throw new Error("job did not complete");
console.log("  TYPESCRIPT SDK LIVE: PASS");
