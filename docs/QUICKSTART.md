# Development client walkthrough

This walkthrough assumes an operator has already provided a running control URL,
a buyer key, and eligible supply for the exact runtime/job/model tuple. There is no
public-availability or time-to-result claim here; package publication, self-serve
activation, physical supply, and stranger testing remain separate gates. The
reserved `cx.example.invalid` host below is intentionally non-routable: replace it
with the operator-provided URL and replace `cx_live_…` with the issued key.

Every example below submits a native job, then polls until it's done. There is no separate synchronous inference endpoint; `POST /v1/jobs` is the direct, simplest path.

## curl

```bash
# submit an embed job for three rows, then poll for the result.
JOB=$(curl -s https://cx.example.invalid/v1/jobs \
  -H "Authorization: Bearer cx_live_…" \
  -H "Content-Type: application/json" \
  -d '{
        "job_type": {"type": "embed"},
        "model": {"kind": "hf", "ref": "all-minilm-l6-v2"},
        "tier": "batch",
        "verification": {"redundancy_frac": 0, "honeypot_frac": 0, "payout_hold_secs": 0},
        "input": "{\"id\":\"a\",\"text\":\"first row\"}\n{\"id\":\"b\",\"text\":\"second row\"}\n{\"id\":\"c\",\"text\":\"third row\"}\n"
      }' | python3 -c 'import sys,json; print(json.load(sys.stdin)["job_id"])')

# poll until complete (a real job usually finishes in a few seconds on a warm worker)
curl -s "https://cx.example.invalid/v1/jobs/$JOB" \
  -H "Authorization: Bearer cx_live_…"

# once status is "complete", fetch the merged result
curl -s "https://cx.example.invalid/v1/jobs/$JOB/results" \
  -H "Authorization: Bearer cx_live_…"
```

### Audio transcription foundation

Audio does not use the generic JSON job endpoint. The control plane must derive
duration and pricing from the uploaded bytes, so quote and submit both use the
dedicated multipart boundary:

```bash
# Required WAV contract: PCM16 integer, mono, 16 kHz, nonempty, at most 30 s
# and at most 1 MiB. Convert other local formats before upload if necessary.
ffmpeg -i source.m4a -ac 1 -ar 16000 -c:a pcm_s16le clip.wav

QUOTE=$(curl -s -X POST https://cx.example.invalid/v1/audio/jobs/quote \
  -H "Authorization: Bearer cx_live_…" \
  -F "file=@clip.wav;type=audio/wav" \
  -F "model=whisper-tiny" \
  -F "tier=batch")

QUOTE_ID=$(printf '%s' "$QUOTE" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["quote_id"])')

JOB=$(curl -s -X POST https://cx.example.invalid/v1/audio/jobs \
  -H "Authorization: Bearer cx_live_…" \
  -F "file=@clip.wav;type=audio/wav" \
  -F "model=whisper-tiny" \
  -F "tier=batch" \
  -F "quote_id=$QUOTE_ID" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["job_id"])')
```

The server normalizes exactly one audio record, prices the server-derived audio
minutes, and fixes verification at one primary plus one same-class redundancy
task. The current runner is explicitly English/no-timestamps. Buyer-supplied
`language`, `timestamps`, and verification overrides are not accepted because
the current runner does not implement those controls honestly.

This surface is a hardened development foundation, not a production-retention
claim. Submission idempotency, durable job-level storage of the pricing authority,
and deletion/retention policy are still open. Do not automatically retry a submit:
the same request can currently create a second billable job, and the normalized
JSONL stored for the job contains the base64-encoded audio.

## Python

The SDK has no third-party runtime dependencies and is not published on PyPI
yet. From a repository checkout, install it into a virtual environment:

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install ./sdk/python
```

```python
from computeexchange import Client

# base_url defaults to http://localhost:8080 — pass the real host explicitly,
# or a bare Client(api_key=...) call will silently try to reach localhost.
cx = Client("https://cx.example.invalid", "cx_live_…")

job = cx.submit_job(
    model="all-minilm-l6-v2",
    job_type="embed",
    input='{"id":"a","text":"first row"}\n{"id":"b","text":"second row"}\n{"id":"c","text":"third row"}\n',
)
cx.wait(job["job_id"])
print(cx.results_text(job["job_id"]))

# plain dict (not an object) — index it like JSON, not attribute access.
out = cx.embeddings("all-minilm-l6-v2", ["first row", "second row", "third row"])
print(out["data"][0]["embedding"][:5])
```

## cx CLI

```bash
# The CLI is a single stdlib-only Go binary, not yet published to a tap — for
# now, build it from the repo: (cd cli && go build -o cx .)
export CX_API_URL=https://cx.example.invalid
export CX_API_KEY=cx_live_…

# --wait polls to completion and prints the merged result for you.
cx submit --model all-minilm-l6-v2 --type embed --input rows.jsonl --wait
```

Where `rows.jsonl` is:

```
{"id":"a","text":"first row"}
{"id":"b","text":"second row"}
{"id":"c","text":"third row"}
```

Without `--wait`, `cx submit` prints a job id you can follow up on yourself:

```bash
JOB=$(cx submit --model all-minilm-l6-v2 --type embed --input rows.jsonl)
cx status "$JOB"
cx results "$JOB"
```

Run `cx -h` (or see the header comment in `cli/main.go`) for the full command list — `submit`, `quote`, `status`, `results`, `invoice`, `events`, `failures`, `models`, `estimate`, `explain-scheduler`, `cancel`, `private-pool`.

### Private pool (a real premium, not just a sentence)

By default your job can run on any eligible supplier on the exchange. If you
need your data to stay on a fixed, named set of suppliers you have personally
vetted — never the shared pool — bind them to your own private pool first:

```bash
# add a supplier (you get their id from a prior job's worker_id, or from a
# direct relationship with that supplier)
cx private-pool add <supplier_id>

# see who's actually in your pool right now
cx private-pool list

# take someone back out
cx private-pool remove <supplier_id>
```

Then price and submit with `--private-pool`. The quote shows the real premium
(25% of the expected cost, already folded into the min/expected/max figures —
never a separate number you have to remember to add) and the exact written
guarantee of what "private" means:

```bash
cx quote  --model all-minilm-l6-v2 --type embed --input rows.jsonl --private-pool
cx submit --model all-minilm-l6-v2 --type embed --input rows.jsonl --private-pool --wait
```

The guarantee, verbatim (also returned as `private_pool_attestation` on the
quote): tasks are claimable ONLY by suppliers you've explicitly bound —
enforced by the control plane's dispatch filter, not merely a stated policy —
no other supplier on the exchange can ever claim a task from that job. It
guarantees WHO runs your work; it does not by itself claim encryption-at-rest
or network isolation beyond that supplier selection.

Submitting `--private-pool` with zero bound suppliers is refused at submit
time (400) — the job could never be claimed by anyone, so the platform tells
you that up front instead of leaving the job stuck at 0% with no explanation.
