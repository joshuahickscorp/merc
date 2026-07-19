# Buyer quickstart

Set the control URL and a buyer API key printed by `cx seed`:

```bash
export CX_URL=http://localhost:8080
export CX_API_KEY=dev-api-key-0001
```

Every buyer request uses `Authorization: Bearer $CX_API_KEY`.

## Discover and quote

```bash
curl -fsS "$CX_URL/v1/models" -H "Authorization: Bearer $CX_API_KEY"

curl -fsS "$CX_URL/v1/quote" \
  -H "Authorization: Bearer $CX_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "job_type":{"type":"embed","batch_size":8},
    "model":{"ref":"all-minilm-l6-v2"},
    "records":2,
    "input_bytes":34,
    "tier":"batch"
  }'
```

Quotes are source-bound, expire, and do not reserve capacity or move money.

## Submit embeddings

```bash
JOB_ID=$(curl -fsS "$CX_URL/v1/jobs" \
  -H "Authorization: Bearer $CX_API_KEY" \
  -H "Idempotency-Key: quickstart-embed-001" \
  -H 'Content-Type: application/json' \
  -d '{
    "job_type":{"type":"embed","batch_size":8},
    "model":{"ref":"all-minilm-l6-v2"},
    "params":{"split_size":1},
    "tier":"batch",
    "input":"{\"text\":\"hello\"}\\n{\"text\":\"world\"}\\n"
  }' | jq -r .job_id)
```

For batched text generation, use
`"job_type":{"type":"batch_infer","max_tokens":32,"temperature":0}`,
model `llama-3.2-1b-instruct-q4`, and JSONL records with a `prompt` field.

## Inspect, retrieve, cancel

```bash
curl -fsS "$CX_URL/v1/jobs/$JOB_ID" \
  -H "Authorization: Bearer $CX_API_KEY"
curl -fsS "$CX_URL/v1/jobs/$JOB_ID/results" \
  -H "Authorization: Bearer $CX_API_KEY"
curl -fsS -X DELETE "$CX_URL/v1/jobs/$JOB_ID" \
  -H "Authorization: Bearer $CX_API_KEY"
```

Result records preserve input order. Cancellation is idempotent and only
unsettled work is eligible.

## Python SDK

```bash
python3 -m pip install ./sdk/python
```

```python
from computeexchange import Client

client = Client("http://localhost:8080", api_key="dev-api-key-0001")
job = client.submit_job(
    model="all-minilm-l6-v2",
    job_type="embed",
    input='{"text":"hello"}\n',
)
done = client.wait(job["job_id"], timeout=300)
rows = client.results_records(done["job_id"])
```

The SDK has no runtime dependency outside the Python standard library.
