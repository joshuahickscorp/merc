# Merc

Merc is a marketplace for batch AI work that runs on Apple Silicon Macs. A buyer
sends a job over an HTTP API, a Go control plane prices it and splits it into
tasks, and Rust agents running on other people's Macs execute those tasks and
earn credit for the work.

No real money has moved through it yet, and it is not approved to handle any.
Read [Status](#status) first.

## What it does today

Four job types are named. Ordinary buyer admission currently advertises one
cell. The authority is [control/runtime-authority.json](control/runtime-authority.json).

| Job type | Model | How results are checked | Ordinary admission |
|---|---|---|---|
| `embed` | `all-minilm-l6-v2` | cosine similarity | not advertised — the cell receipt is not bindable |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | byte-for-byte match | advertised, Candle on Metal |
| `media_transcode` | `ffmpeg-transcode-v1` | byte-for-byte match | CANARY, not advertised |
| `media_rendering` | `svg-scene-render-v1` | byte-for-byte match | CANARY, not advertised |

Unknown job types, models, engines, and devices are rejected. CPU execution is
a test fallback and is never advertised. The Python SDK and
`clients/proto/manifest.schema.json` still only name `embed` and `batch_infer`.

`media_rendering` is a closed JSON scene rasterised to a PPM by a builtin agent
runner. It is not Blender, and it is not image generation. A headless Cycles
baseline and a CPU/Metal placement rule were measured on this host; those
artifacts are not a job type and are not priced. Image generation returns 503:
the policy exists, the runtime does not.

A separate OpenAI-compatible realtime path is wired. Its last real-engine
receipt predates this candidate and is not ordinary-routable canary authority.

Results are spot-checked with known-answer decoy tasks, duplicate work sent to
more than one supplier, and majority votes. The check rate never drops below
0.25, so even a trusted supplier gets roughly one task in four audited.

## The pieces

- **`cx`** — one Go binary: control server, buyer CLI, and operator CLI.
- **`merc-agent`** — the Rust program a supplier runs on their Mac. It claims a
  task, runs it, uploads the result, and reports back. It ships a macOS sandbox
  profile blocking inbound connections, stray writes, and credential reads.
- **PostgreSQL** — the queue and the record of every job and balance. Tasks are
  claimed from an ordinary table with `FOR UPDATE SKIP LOCKED`. No Kafka or Redis.
- **S3-compatible storage** (MinIO locally) — holds inputs and results. Agents
  never get storage credentials, only short-lived URLs scoped to their one task.
- **`clients/sdk/python/`** — a buyer SDK using nothing outside the Python
  standard library. The package name is `merc`.
- **`GET /v1/ui/v1/buy`** and **`GET /v1/ui/v1/earn`** — versioned composition
  reads over existing handlers. They cannot quote, submit, charge, or pay.
  There is no TUI.

Go embeds the runtime authority and the agent binds the same bytes.

## Status

**Not approved for a pilot that moves real buyer or supplier money.**
[ops/go-no-go.json](ops/go-no-go.json) records Level A software GO, Level B
private canary NO-GO, and live money or public launch NO-GO and prohibited.
There are no open target-scope P0s. Eight P1 gates remain. None of them can be
closed by writing more local code.

The five workstation-impossible operational proofs in
[docs/PROGRAMME.md](docs/PROGRAMME.md) § "Near term" are still the live list for
a money-moving private pilot: offsite backup upload and restore, a TLS staging
host, a rollback rehearsal, the full Stripe test-mode money matrix, and a real
alert page to an on-call receiver. That list is not the whole Level B ledger.
The other three open P1s are a two-device canary rehearsal, an independent
repository reviewer, and qualified governance approvals.

A staging control process has been driven to the go-live line and held:
`/readyz` returns 503 `canary_policy_unconfigured`, and there is no public TLS.
The alpha boot receipt is bound to an earlier commit, not this HEAD.

The machine-reachable ceiling is still documented as 84/100. The scorer
currently derives **73/100**, because its authorization-matrix pin is 123
routes and the reviewed matrix is 126. That is a tripwire lag, not a missing
local receipt. `make ci` runs the scorer, so it fails at this HEAD. The
remaining 16 points still require external receipts under `evidence/external/`.
The detail is in [RELEASE_READINESS.md](RELEASE_READINESS.md).

What is and is not established:

- Billing, payouts, refunds, disputes, and Stripe Connect transfers exist as
  code. No real charge, payout, fee, refund, or reversal has ever happened.
  Configured payment code is not proof that money moved.
- Local tests and a historical two-agent proof show the code paths behave.
  They say nothing about fleet size, market demand, or production payment
  processing. The local proof script still submits both `embed` and
  `batch_infer`; only `batch_infer` is advertised today.
- Supplier hardware and engine identity are self-declared. Scheduling checks
  what a machine claims to be; it does not verify the physical machine.
- The macOS sandbox limits the direction and ports of the agent's traffic, but
  not which host it can reach.
- Do not check the agent out under `Downloads`, `Documents`, or `Desktop` — the
  sandbox denies reads there and sandboxed runs will fail.
- Buyer account recovery, data export, deletion, and self-serve activation are
  still missing. The website is deliberately minimal.
- Buyer `objective` (`CHEAPEST` / `BALANCED` / `FASTEST`) is persisted on the
  binding and changes the digest. Nothing prices or places on it yet.
- Multi-family placement v4 can persist a heterogeneous eligible set. Claim-time
  pin still requires exact v3, so a v4 job cannot yet bind a worker.
- The World Model refuses provenance laundering and cannot write authority.
  The application database role is superuser here, so role isolation is not
  the runtime posture.
- L1 PIXEL_EXACT fails between Cycles CPU and Metal, and Metal is not
  self-deterministic on the measured repeat. Device is part of that quality
  contract. It is not a sellable render lane.

[docs/SECURITY.md](docs/SECURITY.md) is the full register of boundaries and
known limitations. It is a register, not a certification.

## What's next

The eight Level B P1s, then the missing buyer account features. On the code
side: rebind embed and media authority if those lanes are to be advertised,
close the claim-time v4 pin, and stop treating the 123-route readiness pin as
84/100. See [docs/PROGRAMME.md](docs/PROGRAMME.md).

Also: the [runtime notes](docs/RUNTIME_AND_PERF.md),
[runbooks](docs/RUNBOOKS.md), and the
[frontend contract](docs/ARCHITECTURE.md).
