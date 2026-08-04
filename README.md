# Merc

Merc is a marketplace for batch AI work that runs on Apple Silicon Macs. A buyer
sends a job over an HTTP API, a Go control plane prices it and splits it into
tasks, and Rust agents running on other people's Macs execute those tasks and
earn credit for the work.

No real money has moved through it yet, and it is not approved to handle any.
Read [Status](#status) first.

## What it does today

Exactly two kinds of work, on exactly two models:

| Job type | Model | How results are checked |
|---|---|---|
| `embed` | `all-minilm-l6-v2` | cosine similarity |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | byte-for-byte match |

```text
control/                  control plane, cx command, embedded schema
agent/                    Candle batch agent and CUDA/vLLM realtime adapter
clients/proto/            runtime matrix and manifest schema
clients/sdk/python/               dependency-free buyer SDK
web/                      public alpha site
scripts/prove-local.sh    repeatable two-agent proof
docs/                     operator and security notes
```

Any other job type, model, or device is rejected before anything runs. Inference
goes through Candle on Apple's Metal GPU; CPU execution is a test fallback and is
never offered to buyers. Results are spot-checked with known-answer decoy tasks,
duplicate work sent to more than one supplier, and majority votes — the check
rate never drops below 0.25, so even a trusted supplier gets roughly one task in
four audited.

## The pieces

- **`cx`** - one Go binary: control server, buyer CLI, and operator CLI.
- **`merc-agent`** - the Rust program a supplier runs on their Mac. It claims a
  task, runs it, uploads the result, and reports back. It ships a macOS sandbox
  profile blocking inbound connections, stray writes, and credential reads.
- **PostgreSQL** - the queue and the record of every job and balance. Tasks are
  claimed from an ordinary table with `FOR UPDATE SKIP LOCKED`. No Kafka or Redis.
- **S3-compatible storage** (MinIO locally) - holds inputs and results. Agents
  never get storage credentials, only short-lived URLs scoped to their one task.
- **`clients/sdk/python/`** - a buyer SDK using nothing outside the Python standard library.

## Run it locally

Needs Go, Rust, Docker Compose, `psql`, Node, and Python 3.

```bash
cp .env.example .env
cp agent/agent.example.toml agent/agent.toml
make dev-up      # PostgreSQL + MinIO in Docker
make migrate     # applies control/schema.sql
make seed        # prints dev credentials, incl. a worker token and an API key
make control     # control server on http://localhost:8080
```

Copy the printed `worker_token` into `agent/agent.toml` or set `CX_WORKER_TOKEN`,
then run `make agent-run` in a second shell. The agent runs natively, not in a
container, because Metal is not available inside the Linux stack.

## Buyer API

The pinned CUDA realtime adapter is a separate Linux command:

```bash
cp agent/vllm.example.toml agent/vllm.toml
cd agent && cargo run --release -- vllm --config vllm.toml
```

Its config must declare the host's CUDA capability class, GPU count, per-GPU
memory, committed memory, and multi-GPU interconnect. The control plane freezes
that placement into every physical realtime contract and receipt.

## Buyer flow

The native HTTP flow is quote, submit, inspect, fetch results, and optionally
cancel:

```text
POST   /v1/quote              price a job; no charge, no reserved capacity
POST   /v1/jobs               submit; requires an Idempotency-Key header
GET    /v1/jobs/{id}          status
GET    /v1/jobs/{id}/results  fetch results
DELETE /v1/jobs/{id}          cancel
```

Reusing an idempotency key with the same body returns the original job; reusing
it with a different body returns `409`. Other endpoints cover accounts, invoices,
webhooks, supplier onboarding, and billing. `/healthz`, `/readyz`, `/version`,
and `/metrics` are for operations; operator actions live under `/admin/*`.

The `cx` command drives all of this from a shell — `cx quote`, `cx submit`,
`cx status`, `cx results`, `cx cancel`, `cx models`, `cx invoice`. It reads
`CX_API_URL` (default `http://localhost:8080`) and `CX_API_KEY`; `cx help` lists
the rest.

The Python SDK wraps the same API. Its package name is `merc`, and it is not on
PyPI — install it from the checkout with
`python3 -m pip install ./clients/sdk/python`.

The media transcode private canary uses the same quote/submit identity. A small
local file may be passed through the CLI (bounded base64); larger objects should
be uploaded to the buyer-owned object namespace and referenced by `--s3-key`:

```bash
cx quote --model ffmpeg-transcode-v1 --type media_transcode \
  --input clip.mp4 --input-format mp4 --max-width 1920 --max-height 1080
cx submit --model ffmpeg-transcode-v1 --type media_transcode \
  --input clip.mp4 --input-format mp4 --max-width 1920 --max-height 1080 \
  --quote-id q_… --max-usd 0.05 --wait
```

Media requests are bounded, byte-exact verified, and remain private-canary only
until the live legal/payment activation authority is separately approved.

```python
from merc import Client

cx = Client("http://localhost:8080", api_key="dev-api-key-0001")
job = cx.submit_job(model="all-minilm-l6-v2", job_type="embed",
                    input='{"text":"hello"}\n')
cx.wait(job["job_id"])
print(cx.results_records(job["job_id"]))
```

Full request examples: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Check the build

```bash
make build         # go build + cargo build
make test          # go test, cargo test, Python SDK package check
make ci            # the above plus formatting, vet, clippy, site build
make prove-local   # full end-to-end proof, described below
make audit         # regenerate the code census under census/
```

`make prove-local` starts throwaway PostgreSQL and MinIO, applies the schema
twice, launches two agents, and runs both job types end to end, writing a receipt
to `.artifacts/prove-local/ledger.jsonl` tied to a fingerprint of the source it
ran against. A skipped step is recorded as a skip, never as a pass. `SKIP_LIVE=1`
checks contracts without real inference; `KEEP=1` leaves the services running.

## Status

**Not approved for a pilot that moves real buyer or supplier money.** There are
no known open P0 defects and the whole customer path passes locally, but five
proofs cannot be produced from a development workstation and none are done: an
offsite backup upload and restore, a deployment to a real TLS staging host, a
rollback rehearsal, the full Stripe test-mode money matrix, and a real alert page
delivered to an on-call receiver. The machine-derived readiness facet is
**84/100** — that is the **machine-reachable maximum** without staging, offsite
storage, or human approvers, not a local shortfall. The remaining 16 points and
the exact operator steps that close them are in
[docs/FACET_EXTERNAL_ACTION_PACK.md](docs/FACET_EXTERNAL_ACTION_PACK.md). The
decision is in [ops/go-no-go.json](ops/go-no-go.json), the detail in
[RELEASE_READINESS.md](RELEASE_READINESS.md).

What is and is not established:

- Billing, payouts, refunds, disputes, and Stripe Connect transfers exist as
  code. No real charge, payout, fee, refund, or reversal has ever happened.
  Configured payment code is not proof that money moved.
- Local proofs show the code paths behave. They say nothing about fleet size,
  market demand, or production payment processing.
- On one Apple Silicon Mac an `embed` job finished in about 3.2 seconds warm, but
  a cold run in the same series took 34 seconds. One machine, one set of runs —
  not a latency or capacity guarantee.
- Supplier hardware and engine identity are self-declared. Scheduling checks what
  a machine claims to be; it does not verify the physical machine.
- The macOS sandbox limits the direction and ports of the agent's traffic, but
  not which host it can reach.
- Do not check the agent out under `Downloads`, `Documents`, or `Desktop` — the
  sandbox denies reads there and sandboxed runs will fail.
- Buyer account recovery, data export, deletion, and self-serve activation are
  still missing. The website is deliberately minimal.

[docs/SECURITY.md](docs/SECURITY.md) is the full register of boundaries and known
limitations. It is a register, not a certification.

## What's next

Finishing the five external proofs above, then soak testing and the missing buyer
account features. See [docs/PROGRAMME.md](docs/PROGRAMME.md).

Also: [runtime matrix](docs/RUNTIME_MATRIX.md) (what may run),
[runbooks](docs/RUNBOOKS.md) (backup, restore, rollback, alerts), and the
[frontend contract](docs/FRONTEND_CONTRACT.md) (what a UI may claim).
