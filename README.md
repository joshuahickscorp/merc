# Computexchange

Computexchange is a task-priced batch-compute exchange for Apple Silicon. Its
production surface is intentionally narrow:

- workloads: `embed` and `batch_infer`
- models: `all-minilm-l6-v2` and `llama-3.2-1b-instruct-q4`
- execution: Candle on Metal
- programs: one Go `cx` control command and one Rust `cx-agent`

The Go control plane owns admission, quotes, job lifecycle, verification,
settlement, and the PostgreSQL task queue. The Rust agent claims tasks, fetches
presigned inputs, runs bounded native inference, uploads results, and commits a
source-bound receipt. S3-compatible storage holds artifacts.

## Layout

```text
control/                  control plane, cx command, embedded schema
agent/                    supplier agent and Candle executor
proto/                    runtime matrix and manifest schema
sdk/python/               dependency-free buyer SDK
web/                      public alpha site
scripts/prove-local.sh    repeatable two-agent proof
docs/                     operator and security notes
```

The queue is the PostgreSQL `tasks` table and claims use
`FOR UPDATE SKIP LOCKED`. Storage credentials stay in the control plane; agents
receive task-scoped presigned URLs.

## Develop

Install Go, Rust, Docker Compose, `psql`, Node, and Python. Then:

```bash
cp .env.example .env
cp agent/agent.example.toml agent/agent.toml
make dev-up
make migrate
make seed
make control
```

Put the printed worker token in `agent/agent.toml` or `CX_WORKER_TOKEN`, then run
`make agent-run` in another shell. The agent is native because Metal is not
available inside the Linux control stack.

## Buyer flow

The native HTTP flow is quote, submit, inspect, fetch results, and optionally
cancel:

```text
POST   /v1/quote
POST   /v1/jobs
GET    /v1/jobs/{id}
GET    /v1/jobs/{id}/results
DELETE /v1/jobs/{id}
```

Job submission requires an `Idempotency-Key` header (8-128 safe ASCII
characters). Reusing the key with identical JSON returns the original job;
reusing it with a different request returns `409 Conflict`. The CLI and Python
SDK generate a key unless the caller supplies one for an uncertain retry.

The Python SDK wraps that API:

```python
from computeexchange import Client

cx = Client("http://localhost:8080", api_key="<buyer key>")
job = cx.submit_job(
    model="all-minilm-l6-v2",
    job_type="embed",
    input='{"text":"hello"}\n',
)
cx.wait(job["job_id"])
print(cx.results_records(job["job_id"]))
```

See [docs/QUICKSTART.md](docs/QUICKSTART.md) for request examples.

## Validate

```bash
make build
make test
make ci
make prove-local
make audit
```

`make prove-local` provisions disposable PostgreSQL and MinIO services, applies
the schema twice, runs local gates, starts two agents, and completes both retained
workloads. Set `SKIP_LIVE=1` for contract-only validation. The proof emits a JSONL
ledger under `.artifacts/prove-local/` and never treats a skipped physical run as
proof.

The schema has one authority: `control/schema.sql`, embedded into `cx` and usable
directly with `make migrate`. `cx audit codebase` writes the deterministic census
under `census/`.

Health and metrics are exposed at `/healthz`, `/readyz`, and `/metrics`.
Authenticated operator actions remain native JSON endpoints under `/admin/*`.
Security boundaries and named limitations are in
[docs/SECURITY.md](docs/SECURITY.md); backup, restore, and rollback procedures are
in [docs/RUNBOOKS.md](docs/RUNBOOKS.md).
