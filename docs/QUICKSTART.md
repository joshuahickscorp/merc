# Buyer quickstart

End-to-end path against a local control plane. Counted steps below are the
numbered buyer-facing actions after the stack is up.

## 0. Prerequisites (once per machine)

These are not buyer API steps; they stand up the local stack the rest of the
doc talks to.

```bash
# From the repository root.
cp -n .env.example .env

# Postgres must accept DATABASE_URL from .env (default
# postgres://cx:cx@localhost:5432/cx?sslmode=disable). Apply schema:
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql

# Object storage for job artifacts:
docker compose up -d minio createbuckets

# Export economic schedule (required for quotes and job intake):
set -a && source .env && set +a

# Seed demo buyer + worker credentials, then start control:
(cd control && go run . seed)
(cd control && go run .)
```

`cx seed` prints `api_key = dev-api-key-0001`. Keep that shell's control process
running for the steps below.

## Buyer steps

### 1. Export the control URL and buyer key

```bash
export MERC_URL=http://localhost:8080
export MERC_API_KEY=dev-api-key-0001
```

Every buyer request uses `Authorization: Bearer $MERC_API_KEY`.

### 2. Discover models

```bash
curl -fsS "$MERC_URL/v1/models" -H "Authorization: Bearer $MERC_API_KEY"
```

### 3. Quote an embeddings job

Quotes scan real input (JSONL string or object). They are source-bound, expire,
and do not reserve capacity or move money. With market-board catalogue prices,
use enough text that the microdollar estimate stays positive (two short lines
can round to $0 and return `409 conflict`).

```bash
curl -fsS "$MERC_URL/v1/quote" \
  -H "Authorization: Bearer $MERC_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "job_type":{"type":"embed","batch_size":8},
    "model":{"ref":"all-minilm-l6-v2"},
    "tier":"batch",
    "input":"{\"text\":\"the quick brown fox jumps over the lazy dog with enough tokens to clear the microdollar floor\"}\n{\"text\":\"another reasonably long sentence so base_compute_usd stays positive after rounding\"}\n"
  }'
```

### 4. Submit embeddings

```bash
JOB_ID=$(curl -fsS "$MERC_URL/v1/jobs" \
  -H "Authorization: Bearer $MERC_API_KEY" \
  -H "Idempotency-Key: quickstart-embed-001" \
  -H 'Content-Type: application/json' \
  -d '{
    "job_type":{"type":"embed","batch_size":8},
    "model":{"ref":"all-minilm-l6-v2"},
    "params":{"split_size":1},
    "tier":"batch",
    "input":"{\"text\":\"the quick brown fox jumps over the lazy dog with enough tokens to clear the microdollar floor\"}\n{\"text\":\"another reasonably long sentence so base_compute_usd stays positive after rounding\"}\n"
  }' | jq -r .job_id)
echo "$JOB_ID"
```

For batched text generation, use
`"job_type":{"type":"batch_infer","max_tokens":32,"temperature":0}`,
model `llama-3.2-1b-instruct-q4`, and JSONL records with a `prompt` field.

### 5. Inspect the job

```bash
curl -fsS "$MERC_URL/v1/jobs/$JOB_ID" \
  -H "Authorization: Bearer $MERC_API_KEY"
```

Without a live agent the job stays queued; that is expected on a control-only
stack.

### 6. Fetch results (after an agent completes the job)

```bash
curl -fsS "$MERC_URL/v1/jobs/$JOB_ID/results" \
  -H "Authorization: Bearer $MERC_API_KEY"
```

Result records preserve input order. Before completion this returns a conflict.

### 7. Cancel (idempotent; unsettled work only)

```bash
curl -fsS -X DELETE "$MERC_URL/v1/jobs/$JOB_ID" \
  -H "Authorization: Bearer $MERC_API_KEY"
```

## Python SDK

### 8. Install the package from the checkout

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install ./sdk/python
```

The package is **not** published to PyPI (this repository has no LICENSE, so
publishing is blocked). Install from the tree or a built wheel only.

### 9. Submit and wait with the SDK

Requires a worker agent online for `wait` to finish. Against control-only, use
`submit_job` + `get_job` instead of `wait`.

```python
from merc import Client

client = Client("http://localhost:8080", api_key="dev-api-key-0001")
job = client.submit_job(
    model="all-minilm-l6-v2",
    job_type="embed",
    input=(
        '{"text":"the quick brown fox jumps over the lazy dog with enough tokens to clear the microdollar floor"}\n'
        '{"text":"another reasonably long sentence so base_compute_usd stays positive after rounding"}\n'
    ),
)
print(client.get_job(job["job_id"])["status"])
# When an agent is running:
# done = client.wait(job["job_id"], timeout=300)
# rows = client.results_records(done["job_id"])
```

The SDK has no runtime dependency outside the Python standard library. Smoke
without a server:

```bash
python sdk/python/example.py --smoke
```

## Step count

**9 buyer-facing steps** after prerequisites (export → models → quote → submit →
inspect → results → cancel → install SDK → SDK submit). Prerequisites (schema,
MinIO, seed, control process, economic env) are separate setup.
