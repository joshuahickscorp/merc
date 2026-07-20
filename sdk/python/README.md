# computeexchange  -  Python buyer SDK

A thin, **dependency-free** client for the Computexchange buyer REST API. The
runtime uses only stdlib `urllib`; installing the package adds no third-party
runtime dependencies.

The package is not published on PyPI yet. Install it from a repository checkout:

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install ./sdk/python
python -c "from computeexchange import Client; print(Client.__module__)"
```

```python
from computeexchange import Client

cx = Client("http://localhost:8080", api_key="<your buyer api key>")

# Low-level: submit, wait, fetch the merged JSONL artifact.
job = cx.submit_job(model="all-minilm-l6-v2", job_type="embed",
                    input='{"text":"hello"}\n{"text":"world"}\n')
cx.wait(job["job_id"])
print(cx.results_text(job["job_id"]))

# OpenAI-shaped convenience: submit -> wait -> reshape in one call.
out = cx.embeddings("all-minilm-l6-v2", ["hello", "world"])
print(out["data"][0]["embedding"][:3], out["model"])
```

**Methods:** `submit_job(...)`, `get_job(id)`, `results(id)`, `results_text(id)`,
`results_records(id)`, `cancel_job(id)`, `wait(id, timeout)`, `models()`,
`estimate(model, units, tier)`, and `embeddings(model, input)`.

Every non-2xx response raises `APIError` carrying the HTTP status and the
server's error body  -  failures are surfaced, never swallowed.

**Verify the package from a clean environment:**

```bash
bash scripts/verify-python-sdk-package.sh
python3 sdk/python/example.py --smoke  # builds + prints a request; no server
```

The verification script installs into a throwaway virtual environment, changes
out of the checkout, imports the installed wheel, checks its metadata and public
surface, and removes the environment on exit. It does not use `PYTHONPATH` or an
editable install.

Supported job types are `embed` and `batch_infer`. Inference parameters
(`max_tokens=`, `temperature=`) are folded into the tagged job type only when
given. Any other workload identifier fails locally.
