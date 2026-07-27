#!/usr/bin/env python3
"""Drive the Python SDK against a RUNNING merc, end to end.

The SDK's unit tests use a stub fetch that accepts whatever it is sent, so
they cannot catch a client that speaks a shape merc rejects. This submits a
real job to a real control plane, waits for a real worker to finish it, and
fetches the real result.

Needs MERC_LIVE_BASE_URL and MERC_LIVE_API_KEY, or the local defaults below.
"""

import sys, time, json
sys.path.insert(0, "sdk/python")
from merc import Client
c = Client(base_url="http://127.0.0.1:8093", api_key="dev-api-key-0001")
print("  models:", [m.get("id") for m in c.models()][:2])
rows = "\n".join(json.dumps({"text": f"python sdk live proof row {i}"}) for i in range(50))
job = c.submit_job(model="all-minilm-l6-v2", model_kind="hf", job_type="embed",
                   input=rows, tier="batch", max_usd=1.0,
                   idempotency_key=f"merc-py-sdk-live-{int(time.time())}")
print("  submitted:", job["job_id"][:8], "tasks:", job.get("task_count"))
done = c.wait(job["job_id"], poll=2, timeout=240)
print("  status:", done.get("status"), "actual_usd:", done.get("actual_usd"),
      "verification:", done.get("verification", {}).get("label"))
raw = c.results_bytes(job["job_id"])
out = [r for r in raw.decode().split("\n") if r.strip()]
print("  rows fetched:", len(out), "dim:", len(json.loads(out[0])["vector"]))
assert done["status"] == "complete" and len(out) == 50
print("  PYTHON SDK LIVE: PASS")
