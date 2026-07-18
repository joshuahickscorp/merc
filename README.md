# Computexchange

A development-stage, centrally coordinated exchange implementation for task-priced
batch inference on Apple Silicon. The Go control plane prices and settles accepted
tasks; the Rust supplier agent executes supported jobs; Postgres and S3 hold the
queue, facts, and artifacts. Verification is coverage-labeled (secret sampling,
honeypots, within-class redundancy, and payout holds), not a blanket guarantee that
unchecked output or colluding model substitution is correct. Real supplier earnings,
production money movement, market liquidity, and a competitive buyer-price win are
separate unproven gates in [`proof/5x5-gates.json`](proof/5x5-gates.json).

## Architecture

```
                 ┌──────────────────────────── Control plane (Go, control/) ────────────────────────────┐
  Buyer  ──────▶ │  REST API → Scheduler/Matcher → Verification → Reputation → Payment/Ledger → Benchmark│
  (api_key)      └───────┬───────────────────────────────────┬──────────────────────────────────────────┘
                         │ enqueue tasks                      │ claim: SELECT ... FOR UPDATE SKIP LOCKED
                         ▼                                    ▼          (the queue IS Postgres, no NATS)
                   ┌───────────────┐                  ┌──────────────────────────────────────┐
                   │  S3 / MinIO   │  presigned       │              Postgres                 │
                   │  job in/out   │◀───── URLs ─────▶│  tasks · jobs · workers · ledger · …  │
                   └───────┬───────┘                  └──────────────────────────────────────┘
                           │ GET input / PUT result            ▲ register · poll · commit · heartbeat (HTTP)
                           ▼                                    │
              ┌──────────────────────── Supplier agents (Rust, agent/) ─────────────────────────┐
              │   Mac M4 Max          Mac M4 Pro          Mac Studio …   (poll → run job → commit)│
              └─────────────────────────────────────────────────────────────────────────────────┘
```

The control plane is a single Go binary; the supplier agent a single Rust binary. The job queue is a **Postgres table** (`tasks`), claimed with `FOR UPDATE SKIP LOCKED` gated on `(status, visible_at)`, retries just push `visible_at` forward. No message broker.

**Inference is real.** The agent runs models on-device with [Candle](https://github.com/huggingface/candle): sentence embeddings (all-MiniLM-L6-v2, 384-dim), fixed-English/no-timestamps Whisper transcription, and quantized Llama generation, on **Metal** on Apple Silicon (CPU fallback elsewhere). Weights are pulled from HuggingFace on first use and cached (never re-fetched, never deleted).

**The result flow is presigned S3.** The control plane mints a presigned **GET** for each task's input and a presigned **PUT** for its result, signed against `S3_PUBLIC_ENDPOINT` (the address the *agent* reaches), while control-side reads/writes go through the internal `S3_ENDPOINT`. The agent uses the URLs verbatim, no S3 credentials ever leave the control plane.

## Repo layout

```
agent/             Rust, supplier agent binary (warm model pool + concurrency)
  src/{main,hardware,runners,pool,models,protocol,status,config,types}.rs
control/           Go, control plane, single binary (one flat package main)
  {main,api,openai,scheduler,store,storage,verification,reputation,payment,
   benchmark,workers,metrics,seed,types}.go
db/schema.sql      Single authoritative PostgreSQL schema (applied via `make migrate`)
proto/manifest.schema.json   Canonical wire contract (JSON Schema, draft 2020-12)
cli/               `cx`, standalone Go CLI (submit / status / results / cancel / estimate)
                   control/openai.go, OpenAI-shaped Batch API subset (files + batches)
sdk/python/        dep-free Python client (urllib) + OpenAI-shaped `embeddings()`
web/admin.html     Operator Control Room · passkey-gated console (served at `/admin`; `/` redirects here)
macapp/            SwiftUI menu-bar app, `swift build` via Package.swift (signing external)
docs/TURBO.md · docs/RUNBOOKS.md · docs/ALPHA_READINESS.md
                   Turbo scheduler pass · operator runbooks · alpha readiness (proven vs skeleton vs external)
scripts/prove-local.sh       the one-command local proof (native PG+MinIO, live Metal)
scripts/{install,uninstall,backup}.sh   one-command agent install/uninstall · DB + object-store backup
.github/workflows/ci.yml     CI: Go build/vet/fmt/unit+integration, Rust fmt/clippy/build/test, schema apply
RELEASE_CANDIDATE.md         historical capability inventory; canonical status is the 5/5 registry
Dockerfile.control · docker-compose.yml · Makefile · .env.example · .gitignore
```

**Turbo** (second pass): a hard-filter scheduler (a claim must pass the admitted
runtime tuple plus the worker's declared capability/resource filters; declarations
are not independent execution attestation), warm model pool + bounded concurrency, three new workloads
(`batch_classification` / `json_extraction` / `rerank`), 3-way verification
tiebreak, auto-quarantine, straggler hedging, one merged buyer-ready artifact per
job, an **OpenAI-shaped Batch API**, and a menu-bar **status surface**. These are
implementation capabilities, not proof of public distribution, full OpenAI sync or
streaming compatibility, physical fleet scale, or market outcomes. See
[docs/TURBO.md](docs/TURBO.md) and the canonical gate registry.

**Current advertised runtime projection: Candle/Metal on Apple Silicon only.** A
CPU fallback exists for CI but is not a supplier lane. CUDA, vLLM, Hawking, MLX,
container, and clustered cells are hardware-pending, soak-only, stub, or wire-only
as recorded in [`docs/RUNTIME_MATRIX.md`](docs/RUNTIME_MATRIX.md); none may be
presented as production supply. A local free-lane stack can run on one host after
dependencies and model weights are present, but that is not a TEE, operator-blind,
confidential-compute, data-sovereignty, or paid-market air-gap guarantee.

**Closed-alpha pass:** **dynamic supplier throttling** (the agent reads real
available memory and pauses claiming work before it would breach the operator's
reserved headroom or swap the box), a **scheduler safety contract** (the claim
filter refuses to dispatch to a throttled worker or one whose *effective* memory
is below the job's need), and the operator **Control Room** — a passkey-gated
console at `/admin` (`/` redirects there) showing the birds-eye money split, fleet,
runs, and the self-healing watchdog. The old role-tab skeleton at `/app` was
retired into this one console; the supplier surface + workflows notes moved to
[docs/SUPPLIER-SURFACE.md](docs/SUPPLIER-SURFACE.md). See
[docs/ALPHA_READINESS.md](docs/ALPHA_READINESS.md) (proven vs external) and
[docs/PRODUCT_SHAPE.md](docs/PRODUCT_SHAPE.md) (app topology).
Configure headroom with `memory_headroom_gb` / `max_memory_pct` in `agent.toml`.

There are no `utils/` or `helpers/` junk drawers; every file is load-bearing.

`proto/manifest.schema.json` is the single source of truth for the wire shape (the "horizon"). `agent/src/types.rs` (serde) and `control/types.go` mirror it exactly: snake_case fields, snake_case string enums, internally-tagged `JobType`. Keep all three in lockstep.

## Quickstart

Requires Docker (compose v2), the Postgres client (`psql`), Rust (`cargo`), and Go 1.26.

```bash
cp .env.example .env                      # defaults already match the dev stack
cp agent/agent.example.toml agent/agent.toml   # agent config (gitignored; holds the worker token)
make dev-up                   # start Postgres + MinIO + create the cx-jobs bucket (detached)
make migrate                  # apply db/schema.sql  (idempotent)
make seed                     # mint a demo api_key + worker_token (prints both)
#   → put the printed worker_token in agent/agent.toml (or export CX_WORKER_TOKEN)
make control                  # run the Go control plane on :8080 (foreground)
make agent-bench              # (other shell) benchmark local hardware
make agent-run                # (other shell) start the supplier polling loop
```

> **Evidence boundary.** The repository contains a real Candle/Metal execution path
> and a live-agent proof mode. Cite a live result only when the terminal
> `.artifacts/prove-local/proof-ledger.txt` passes
> `scripts/verify_proof_ledger.py` for the current source and required `full_local`
> rows. Contract-only integration tests do not prove physical inference. The Stripe
> Connect transfer code also does not prove a real charge, payout, reversal, fee, or
> reconciliation; those remain external money gates.

Auth is **not bypassable**: `make seed` inserts a *hashed* api_key into `api_keys` and a token into `worker_tokens`. The api_key is the buyer's `Authorization: Bearer` for `POST /v1/jobs`; the worker_token is the agent's `CX_WORKER_TOKEN`. Without seeded rows, every request is rejected.

## Run the full stack

The control plane is containerized; the agent is not (Metal is unavailable in Linux containers, see "Why no agent image"). `make up` builds and starts Postgres, MinIO, the bucket + schema one-shots, and the control plane, in dependency order:

```bash
make up                       # docker compose up -d --build  (control plane on :8080)
make seed                     # mint api_key + worker_token against the running stack
make down                     # tear the stack down  (add `-v` via compose to wipe volumes)
```

Then run the agent natively against it: `CX_CONTROL_URL=http://localhost:8080 CX_WORKER_TOKEN=<seed token> make agent-run`.

## Seed

`make seed` (or `cd control && go run . seed`) runs the control plane's `seed` subcommand: idempotent (stable demo UUIDs + `ON CONFLICT DO NOTHING`), it prints a buyer `api_key` and a `worker_token` and exits without starting the server. Re-running is a no-op.

## Run the local proof harness

`make prove-local` (→ `scripts/prove-local.sh`) provisions throwaway Postgres and
MinIO, applies the schema, runs the integration/unit suites, and in `full_local`
mode drives the Rust agent through physical Metal paths. `SKIP_LIVE=1` is explicitly
`contract_only`. The harness writes a terminal source-bound ledger and exits nonzero
on a failed check or source mutation. It proves only its named rows, never product
5/5, launch readiness, public distribution, real money, multiple physical machines,
or demand.

```bash
make prove-local            # physical local mode; models/toolchains may make this slow
#   SKIP_LIVE=1   matrix only (no model run)     KEEP=1   leave the stack up to inspect
#   PROVE_WHISPER=1   make whisper a required check (default best-effort)
```

Validate rather than trusting a pasted count:

```bash
cx verify \
  --ledger .artifacts/prove-local/proof-ledger.txt \
  --current-source --mode full_local
cx prove
```

The second command prints prerequisites separately from outcomes and gives the
concrete next action for every unproven gate.

This is a macOS step (the agent needs Metal); the models (MiniLM, Llama-3.2-1B, whisper-tiny) are cached after first use. Needs `go`, `cargo`, `psql`, native `postgres`/`minio` (or `USE_DOCKER=1`), `curl`, `python3` on PATH.

## Observability

- **`GET /healthz`**, liveness; the control plane fatals at startup if Postgres or the object store is unreachable, so a 200 here means the deps are wired.
- **`GET /metrics`**, Prometheus exposition (`make metrics` to scrape). Job/task counters, queue depth, payout state.
- **Background workers** run for the life of the process: **payout-release** (held credits → released once `release_at` passes), **stale-task requeue** (claims that outlive their deadline get `visible_at` pushed forward and are re-dispatched), plus independent **job-finalization** and leased, per-registration-signed **webhook delivery** loops.

## Deliberate simplifications

Two service-architecture simplifications, documented here so they can be revisited:

1. **Core services are Go + Rust.** The control plane and CLI are Go; the agent is Rust. Python is used for SDK and proof/benchmark tooling, not as a third long-running service.
2. **No NATS, Postgres queue.** The plan names NATS for the job queue; we use a Postgres queue instead (`SELECT ... FOR UPDATE SKIP LOCKED` on `tasks`, with `claimed_by`/`claimed_at`/`visible_at` columns and a `(status, visible_at)` index). This removes a dependency and a dev container, the dev stack is just Postgres + MinIO. At V1 scale Postgres is more than enough and keeps everything in one transactional store. *Revert:* introduce NATS, move dispatch off the `tasks` table, drop the queue columns/index.

## Why no agent image

There is deliberately **no Dockerfile for the agent**. It uses Candle + **Metal** for on-device inference, and Metal is unavailable inside Linux containers. The agent must run natively on macOS; only the control plane is containerized (`Dockerfile.control`, distroless static binary). `docker-compose.yml` and CI reflect this, the stack runs the control plane in a container and you point the native agent at it via `CX_CONTROL_URL`.

## Status

This is a substantial implementation with real local code paths and many proven
contracts, but it is not yet a 5/5 product or a demonstrated market. The canonical
status is generated from [`proof/5x5-gates.json`](proof/5x5-gates.json); harnesses,
generators, packages, and mock fleets are prerequisites rather than outcome credit.

**Real now**
- **Execution**, Candle inference on Metal: MiniLM embeddings (384-dim), Whisper transcription, quantized Llama generation.
- **Verification**, honeypots + within-class redundancy + payout holds; the hold→release state machine is real.
- **Storage**, real presigned S3 GET/PUT (internal vs public endpoint split), bucket auto-created at startup.
- **Queue**, the Postgres `tasks` queue (`FOR UPDATE SKIP LOCKED`, retry visibility).
- **Workers**, payout-release, stale-task requeue, webhook delivery, all running.
- **Observability**, `/healthz`, `/metrics`, structured startup that fatals on missing config.
- `db/schema.sql`, complete schema (domain, queue columns/index, auth/honeypot, webhooks, models catalogue).
- `proto/manifest.schema.json`, the full wire contract, matching `agent/src/types.rs` and `control/types.go`.

**Still unproven as product outcomes**
- Real buyer charge, Connect payout, reversal/refund, provider fee, and exact reconciliation.
- All-attempt margin safety for losing/retried/disputed work and a 30-day paid corpus.
- Collusion-resistant execution identity and quantified long-con detection bounds.
- Two physical Macs, production CUDA/container supply, lane promotion, and soak.
- Signed app/package distribution, stranger activation, liquidity, and repeat use.

## Build / verify

```bash
make build    # cargo build (agent) + go build ./... (control)
make test     # cargo test + go test ./...   (agent model-download tests are #[ignore]d)
make fmt      # cargo fmt + gofmt -w
make loc      # line counts vs the BLACKHOLE targets
make docker-build   # build the control-plane image standalone (cx-control)
```

CI (`.github/workflows/ci.yml`) gates every push/PR: the **control** job builds, vets, gofmt-checks, and runs both the unit tests **and the full integration matrix** (`go test -tags integration`) against live Postgres + MinIO service containers; the **agent** job runs fmt/clippy/build/test on macOS (for Candle/Metal); the **schema** job applies `db/schema.sql` into Postgres 17 and asserts a clean, re-runnable load. The deeper `make prove-local` (live Metal inference) is the local release-candidate gate, it needs Apple Silicon + cached models, so it runs on a developer machine, not cheap CI.

## Roadmap

Computexchange is pursuing a task-priced, coverage-labeled batch-compute exchange.
The work is sequenced by the canonical 5/5 registry, not by the historical labels
below.

### Current local implementation
- The full job lifecycle: submit, queue (Postgres FOR UPDATE SKIP LOCKED),
  claim, run real on-device inference on Metal, verify, and settle the ledger.
- Verification: honeypots, within-class redundancy, 3-way majority vote,
  reputation, and payout holds.
- An OpenAI-shaped Batch API, locally buildable CLI/Python package, and menu-bar app core; public distribution and sync/stream parity are not claimed.

### Next
- Attempt-complete margin safety and Stripe-test recovery.
- Runtime-matrix enforcement, one-time app enrollment exchange/revocation, and claim conformance.
- Cross-machine validation on a real Mac Studio and a second physical Mac
  (byte-identical output rates, sustained thermal load).

### Later
- Capped live-money canary and paid margin/supplier-value cohorts.
- A buyer-demanded CUDA/PyTorch-or-container lane promoted through physical fault and soak gates.
- Any multi-Mac clustered runner only after a real distributed forward pass and physical proof; two independent workers do not imply clustering.
