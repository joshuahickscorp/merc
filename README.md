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
private canary NO-GO, backend-alpha engineering-ready NO-GO, backend-alpha
external-proven NO-GO, and live money or public launch NO-GO and prohibited.
There are no open target-scope P0s. Five P1 gates remain. Recompute with
`python3 scripts/validate-readiness.py` (derived at this HEAD:
**Level B 87/100**, P0=0, P1=5, `NO_GO`; **backend alpha 85/91**,
`ALPHA_ENGINEERING_READY NO_GO`, `EXTERNAL_ALPHA_PROVEN NO_GO`). The only open
`ALPHA_BLOCKER` P1 is `P1-STRIPE-TEST`.

| P1 | Classification | State |
|---|---|---|
| `P1-STRIPE-TEST` | `ALPHA_BLOCKER` | open — Stripe Connect is not signed up on `acct_1TxbzMCwPLrR4vaY` |
| `P1-CANARY-REHEARSAL` | `ALPHA_CONTROL` | open — local L12 buyer/supplier/verify/settle receipts PASS; live staging BLOCKED; does not satisfy `EXTERNAL_ALPHA_PROVEN` |
| `P1-ALERT-DELIVERY` | `PUBLIC_LAUNCH` | open — local fire→sink→resolve is not a staffed paging receiver |
| `P1-INDEPENDENT-APPROVAL` | `PUBLIC_LAUNCH` | open |
| `P1-GOVERNANCE` | `PUBLIC_LAUNCH` | open — governance documents are unapproved drafts |

Three P1s closed on evidence, not by reclassification: `P1-STAGING`,
`P1-RECOVERY-SOAK` (alpha 3600 s exit; the 24-hour Level B/C clause stays
unearned), and `P1-OFFSITE-RESTORE`. See [docs/PROGRAMME.md](docs/PROGRAMME.md)
§ "Near term" and § "Facet external action pack".

Persistent staging is public TLS at `https://mercmerc.net`. Observed
2026-08-17: `GET /readyz` HTTP 200 with `payment_mode=test`,
`live_value_movement=false`, `provider_enabled=true`; `GET /version` HTTP 200
at commit `19fe0b23940c7e3d4da9b45d9cc5689c2c515d07` (`modified: false`,
`go_version: go1.26.6`). That commit is an ancestor of this HEAD and sits 20
commits behind it. The execution loop is **not** closed on that host
(`evidence/canary/l12-p1-canary-rehearsal-live-staging.json` is `BLOCKED`).

The derived score is **87/100** (threshold 95). Local receipts plus the
independent offsite backup/restore pair account for 87; the remaining 13
points are Stripe sandbox matrix (6), 24-hour qualifying soak (3), external
staging-attack rehearsal (1), qualified privacy approval (1), licensing
approval (1), and staffed abuse route (1). The authorization-matrix pin is
126 routes with default deny — it matches the reviewed matrix. The detail is
in [RELEASE_READINESS.md](RELEASE_READINESS.md). Backend-alpha meaning is
[docs/BACKEND_ALPHA_CONTRACT.md](docs/BACKEND_ALPHA_CONTRACT.md).

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

The five open Level B P1s — Connect signup first, then the counted canary
rehearsal, then the three `PUBLIC_LAUNCH` gates — then the missing buyer
account features. Live money stays `NO_GO_PROHIBITED` even if every Level B
P1 closes. On the code side: rebind embed (the candle embed cell is not
bindable: empty `engine_build_hash`) and media authority if those lanes are
to be advertised, and close the claim-time v4 pin. See
[docs/PROGRAMME.md](docs/PROGRAMME.md).

Also: the [runtime notes](docs/RUNTIME_AND_PERF.md),
[runbooks](docs/RUNBOOKS.md), and the
[frontend contract](docs/ARCHITECTURE.md).
