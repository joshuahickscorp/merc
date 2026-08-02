/**
 * Dependency-free TypeScript client for the merc buyer API.
 *
 * Mirrors sdk/python/merc so the two SDKs describe the same product. No runtime
 * dependencies: it uses the platform `fetch`, so it runs on Node 18+, Deno,
 * Bun and browsers without a bundler deciding anything for you.
 */

export const VERSION = "0.1.0";

/** 4-byte header on binary embedding payloads. Shared with the Go control
 *  plane and the Python SDK -- see control/api.go and agent/src/executor.rs.
 *  It is a stored-artifact header, so it is frozen and must not be renamed. */
const EMBED_MAGIC = new Uint8Array([0x43, 0x58, 0x45, 0x4d]); // "CXEM"

export type JobType = "embed" | "batch_infer";
export type Tier = "batch" | "priority" | "trusted";

export interface JobSpec {
  model: string;
  job_type: JobType;
  input: string | string[];
  tier?: Tier;
  split_size?: number;
  min_memory_gb?: number;
  max_duration_secs?: number;
  params?: Record<string, unknown>;
}

export interface Job {
  job_id: string;
  status: string;
  [k: string]: unknown;
}

/** An HTTP-level failure carrying the status and the path that produced it,
 *  so a caller can distinguish "merc said no" from "the network broke". */
export class MercAPIError extends Error {
  constructor(
    readonly status: number,
    readonly body: unknown,
    readonly method = "",
    readonly path = "",
  ) {
    super(`merc api ${method} ${path} failed with ${status}: ${JSON.stringify(body)}`);
    this.name = "MercAPIError";
  }
}

export class MercJobError extends Error {
  constructor(readonly jobId: string, message: string, readonly status?: string) {
    super(message);
    this.name = "MercJobError";
  }
}

export class MercJobFailed extends MercJobError {
  constructor(jobId: string, message: string, status?: string, readonly failures?: unknown[]) {
    super(jobId, message, status);
    this.name = "MercJobFailed";
  }
}

export function isEmbeddingsBinary(data: Uint8Array): boolean {
  if (data.length < EMBED_MAGIC.length) return false;
  return EMBED_MAGIC.every((b, i) => data[i] === b);
}

/**
 * Decode the binary embeddings payload.
 *
 * Layout: 4-byte magic, uint32 count, uint32 dim, then count*dim float32,
 * all little-endian. Matches decode_embeddings_binary in the Python SDK.
 */
export function decodeEmbeddingsBinary(data: Uint8Array): number[][] {
  if (!isEmbeddingsBinary(data)) {
    throw new Error("payload is not a merc binary embeddings blob");
  }
  const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
  const count = view.getUint32(4, true);
  const dim = view.getUint32(8, true);
  const expected = 16 + count * dim * 4;
  if (data.length < expected) {
    throw new Error(
      `truncated embeddings blob: ${data.length} bytes, expected at least ${expected}`,
    );
  }
  const out: number[][] = [];
  let off = 16;
  for (let i = 0; i < count; i++) {
    const row = new Array<number>(dim);
    for (let j = 0; j < dim; j++) {
      row[j] = view.getFloat32(off, true);
      off += 4;
    }
    out.push(row);
  }
  return out;
}

export interface ClientOptions {
  baseUrl?: string;
  apiKey?: string;
  timeoutMs?: number;
  fetch?: typeof globalThis.fetch;
}

/** Idempotency keys must be 8-128 chars of [A-Za-z0-9._:-]. */
function randomIdempotencyKey(): string {
  const g = globalThis as { crypto?: { randomUUID?: () => string } };
  if (typeof g.crypto?.randomUUID === "function") return g.crypto.randomUUID();
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

export class Client {
  readonly baseUrl: string;
  readonly apiKey: string;
  readonly timeoutMs: number;
  readonly #fetch: typeof globalThis.fetch;

  constructor(opts: ClientOptions = {}) {
    // Trailing slashes are stripped so callers can pass either form without
    // producing a double slash that some proxies treat as a different path.
    this.baseUrl = (opts.baseUrl ?? "http://localhost:8080").replace(/\/+$/, "");
    this.apiKey = opts.apiKey ?? "";
    this.timeoutMs = opts.timeoutMs ?? 60_000;
    const f = opts.fetch ?? globalThis.fetch;
    if (typeof f !== "function") {
      throw new Error("no fetch available; pass one via ClientOptions.fetch");
    }
    this.#fetch = f;
  }

  async #request<T>(
    method: string,
    path: string,
    body?: unknown,
    query?: Record<string, string | number | undefined>,
    extraHeaders?: Record<string, string>,
  ): Promise<T> {
    const url = new URL(this.baseUrl + path);
    for (const [k, v] of Object.entries(query ?? {})) {
      if (v !== undefined) url.searchParams.set(k, String(v));
    }
    const headers: Record<string, string> = { accept: "application/json" };
    if (this.apiKey) headers.authorization = `Bearer ${this.apiKey}`;
    if (body !== undefined) headers["content-type"] = "application/json";
    Object.assign(headers, extraHeaders ?? {});

    // AbortController rather than a racing promise, so a timed-out request is
    // actually cancelled instead of left running.
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), this.timeoutMs);
    let resp: Response;
    try {
      // exactOptionalPropertyTypes: build the init object without an explicit
      // undefined body, rather than assigning one.
      const init: RequestInit = { method, headers, signal: ctl.signal };
      if (body !== undefined) init.body = JSON.stringify(body);
      resp = await this.#fetch(url.toString(), init);
    } finally {
      clearTimeout(timer);
    }

    const text = await resp.text();
    let parsed: unknown = text;
    if (text) {
      try {
        parsed = JSON.parse(text);
      } catch {
        /* non-JSON body is surfaced verbatim */
      }
    }
    if (!resp.ok) throw new MercAPIError(resp.status, parsed, method, path);
    return parsed as T;
  }

  #spec(spec: JobSpec): Record<string, unknown> {
    // merc's job input is a JSONL STRING, not an array: it rejects an array
    // with "input must be a JSONL string or an object with a non-empty
    // s3_key". The Python SDK has always serialised a list to JSONL; this
    // client wrapped a single value in an array instead and its unit test
    // pinned that as "matching the Python SDK". It never matched, and no test
    // caught it because the fetch stub accepted whatever it was sent.
    const input =
      typeof spec.input === "string"
        ? spec.input
        : (spec.input as unknown[]).map((r) => JSON.stringify(r)).join("\n") + "\n";
    const out: Record<string, unknown> = {
      model: spec.model,
      job_type: spec.job_type,
      input,
      tier: spec.tier ?? "batch",
    };
    for (const k of ["split_size", "min_memory_gb", "max_duration_secs"] as const) {
      if (spec[k] !== undefined) out[k] = spec[k];
    }
    if (spec.params) out.params = spec.params;
    return out;
  }

  /**
   * Submit a job.
   *
   * merc REQUIRES an Idempotency-Key on submission and rejects the request with
   * HTTP 400 without one, so this generates a key when the caller does not
   * supply one -- matching the Python SDK, which has always done so. Without
   * this the shipped client could not submit a job at all; every unit test
   * passed because the fetch stub never inspected headers.
   *
   * Pass your own key to make a retry idempotent: resubmitting with the same
   * key returns the original job instead of creating a second one.
   */
  submitJob(spec: JobSpec, options?: { idempotencyKey?: string }): Promise<Job> {
    const key = options?.idempotencyKey ?? `submit-${randomIdempotencyKey()}`;
    return this.#request<Job>("POST", "/v1/jobs", this.#spec(spec), undefined, {
      "idempotency-key": key,
    });
  }
  quote(spec: JobSpec): Promise<unknown> {
    return this.#request("POST", "/v1/quote", this.#spec(spec));
  }
  getJob(jobId: string): Promise<Job> {
    return this.#request<Job>("GET", `/v1/jobs/${encodeURIComponent(jobId)}`);
  }
  results(jobId: string): Promise<unknown> {
    return this.#request("GET", `/v1/jobs/${encodeURIComponent(jobId)}/results`);
  }
  /** Cancel a job. merc serves DELETE /v1/jobs/{id}; there is no /cancel route. */
  cancelJob(jobId: string): Promise<unknown> {
    return this.#request("DELETE", `/v1/jobs/${encodeURIComponent(jobId)}`);
  }
  events(jobId: string): Promise<unknown> {
    return this.#request("GET", `/v1/jobs/${encodeURIComponent(jobId)}/events`);
  }
  failures(jobId: string): Promise<unknown> {
    return this.#request("GET", `/v1/jobs/${encodeURIComponent(jobId)}/failures`);
  }
  invoice(jobId: string): Promise<unknown> {
    return this.#request("GET", `/v1/jobs/${encodeURIComponent(jobId)}/invoice`);
  }
  models(): Promise<unknown> {
    return this.#request("GET", "/v1/models");
  }
  estimate(model: string, units: number, tier: Tier = "batch"): Promise<unknown> {
    // Registered as GET /v1/price-estimate in control/api.go (Python SDK already
    // used the correct path; /v1/estimate was a client-only drift that 404'd).
    return this.#request("GET", "/v1/price-estimate", undefined, { model, units, tier });
  }

  /**
   * Poll until the job reaches a terminal state.
   *
   * Throws MercJobFailed on a failed job rather than returning it, so a caller
   * cannot mistake a failure for a result by forgetting to check `status`.
   */
  async wait(
    jobId: string,
    opts: { pollMs?: number; timeoutMs?: number } = {},
  ): Promise<Job> {
    const pollMs = opts.pollMs ?? 1000;
    const deadline = Date.now() + (opts.timeoutMs ?? 600_000);
    for (;;) {
      const job = await this.getJob(jobId);
      const status = String(job.status ?? "");
      if (status === "complete") return job;
      if (status === "failed" || status === "cancelled") {
        throw new MercJobFailed(jobId, `job ${jobId} ended as ${status}`, status);
      }
      if (Date.now() > deadline) {
        throw new MercJobError(jobId, `timed out waiting for job ${jobId}`, status);
      }
      await new Promise((r) => setTimeout(r, pollMs));
    }
  }
}

export default Client;
