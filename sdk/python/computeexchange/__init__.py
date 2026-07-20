"""Dependency-free buyer client for the Computexchange native API."""

import json
import struct
import time
import urllib.error
import urllib.parse
import urllib.request

__version__ = "0.1.0"
__all__ = ["Client", "APIError", "JobError", "BudgetStoppedError", "BadInputError",
           "decode_embeddings_binary", "is_embeddings_binary"]

JOB_TYPES = ("embed", "batch_infer")
_EMBED_MAGIC = b"CXEM"


def is_embeddings_binary(data):
    return len(data) >= 4 and data[:4] == _EMBED_MAGIC


def decode_embeddings_binary(data):
    if len(data) < 16 or data[:4] != _EMBED_MAGIC:
        raise ValueError("binary embeddings: invalid header")
    version, dim, count = struct.unpack_from("<III", data, 4)
    if version != 1:
        raise ValueError(f"binary embeddings: unsupported version {version}")
    if len(data) != 16 + count * dim * 4:
        raise ValueError("binary embeddings: body does not match header")
    values = struct.unpack_from(f"<{count * dim}f", data, 16)
    return [list(values[i * dim:(i + 1) * dim]) for i in range(count)]


def _job_type(value):
    if value not in JOB_TYPES:
        raise ValueError(f"unsupported job_type {value!r}; supported: {JOB_TYPES}")


class APIError(Exception):
    def __init__(self, status, body, method="", path=""):
        self.status, self.body = status, body
        super().__init__(f"{method} {path} -> HTTP {status}: {body}".strip())


class JobError(Exception):
    def __init__(self, job_id, message, status=None):
        self.job_id, self.status = job_id, status or {}
        super().__init__(f"job {job_id}: {message}")


class BudgetStoppedError(JobError):
    pass


class BadInputError(JobError):
    def __init__(self, job_id, message, status=None, failures=None):
        self.failures = failures or []
        super().__init__(job_id, message, status)


class Client:
    def __init__(self, base_url="http://localhost:8080", api_key="", timeout=60.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def _request(self, method, path, body=None, query=None):
        url = self.base_url + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        headers = {"Authorization": "Bearer " + self.api_key} if self.api_key else {}
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                raw = response.read()
        except urllib.error.HTTPError as error:
            detail = error.read().decode("utf-8", "replace").strip()
            raise APIError(error.code, detail, method, path) from None
        except urllib.error.URLError as error:
            raise APIError(0, str(error.reason), method, path) from None
        return json.loads(raw) if raw else {}

    @staticmethod
    def _input(value):
        if isinstance(value, str):
            return value
        return "".join(json.dumps(record) + "\n" for record in value)

    def _spec(self, model, job_type, input, tier, split_size, min_memory_gb,
              redundancy_frac, honeypot_frac, model_kind=None, **job_options):
        _job_type(job_type)
        tagged = {"type": job_type}
        for name in ("batch_size", "binary", "max_tokens", "temperature"):
            if job_options.get(name) is not None:
                tagged[name] = job_options[name]
        model_value = {"ref": model}
        if model_kind is not None:
            model_value["kind"] = model_kind
        return {
            "job_type": tagged,
            "model": model_value,
            "params": {"split_size": int(split_size)} if split_size else None,
            "constraints": {"min_memory_gb": float(min_memory_gb)},
            "verification": {
                "redundancy_frac": float(redundancy_frac),
                "honeypot_frac": float(honeypot_frac),
                "payout_hold_secs": int(job_options.get("payout_hold_secs", 0)),
            },
            "tier": tier,
            "input": self._input(input),
        }

    def submit_job(self, model, job_type, input, *, tier="batch", params=None,
                   max_tokens=None, temperature=None, batch_size=None, binary=None,
                   split_size=None, min_memory_gb=0.0, hw_classes=None,
                   data_residency=None, redundancy_frac=0.0, honeypot_frac=0.0,
                   payout_hold_secs=0, webhook_url=None, quote_id=None, max_usd=None,
                   s3_key=None, model_kind=None):
        body = self._spec(model, job_type, input, tier, split_size, min_memory_gb,
                          redundancy_frac, honeypot_frac, model_kind,
                          max_tokens=max_tokens, temperature=temperature,
                          batch_size=batch_size, binary=binary,
                          payout_hold_secs=payout_hold_secs)
        if params:
            body["params"] = dict(params)
            if split_size:
                body["params"]["split_size"] = int(split_size)
        if hw_classes:
            body["constraints"]["hw_classes"] = list(hw_classes)
        if data_residency:
            body["constraints"]["data_residency"] = list(data_residency)
        if s3_key:
            body["input"] = {"s3_key": s3_key}
        for key, value in (("webhook_url", webhook_url), ("quote_id", quote_id)):
            if value:
                body[key] = value
        if max_usd is not None and float(max_usd) > 0:
            body["max_usd"] = float(max_usd)
        return self._request("POST", "/v1/jobs", body)

    def quote(self, model, job_type, input, *, tier="batch", split_size=None,
              min_memory_gb=0.0, redundancy_frac=0.0, honeypot_frac=0.0,
              model_kind=None):
        body = self._spec(model, job_type, input, tier, split_size, min_memory_gb,
                          redundancy_frac, honeypot_frac, model_kind)
        return self._request("POST", "/v1/quote", body)

    def get_job(self, job_id):
        job = self._request("GET", f"/v1/jobs/{job_id}")
        state = job.get("budget_state")
        if state in ("paused_for_budget", "cancelled_by_budget"):
            raise BudgetStoppedError(job_id, f"budget_state={state}", job)
        if job.get("status") in ("failed", "cancelled"):
            try:
                buyer_faults = [row for row in self.failures(job_id) if row.get("buyer_fault")]
            except APIError:
                buyer_faults = []
            if buyer_faults:
                raise BadInputError(job_id, "buyer input failed", job, buyer_faults)
        return job

    def results(self, job_id):
        return self._request("GET", f"/v1/jobs/{job_id}/results")

    def cancel_job(self, job_id):
        return self._request("DELETE", f"/v1/jobs/{job_id}")

    def events(self, job_id):
        return self._request("GET", f"/v1/jobs/{job_id}/events")

    def failures(self, job_id):
        return self._request("GET", f"/v1/jobs/{job_id}/failures")

    def invoice(self, job_id):
        return self._request("GET", f"/v1/jobs/{job_id}/invoice")

    def models(self):
        return self._request("GET", "/v1/models")

    def estimate(self, model, units, tier="batch"):
        return self._request("GET", "/v1/price-estimate",
                             query={"model": model, "units": int(units), "tier": tier})

    def wait(self, job_id, timeout=1800.0, poll=3.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() <= deadline:
            job = self.get_job(job_id)
            if job.get("status") == "complete":
                return job
            if job.get("status") in ("failed", "cancelled"):
                raise JobError(job_id, f"status={job.get('status')}", job)
            time.sleep(poll)
        raise TimeoutError(f"job {job_id} not complete after {timeout}s")

    def results_bytes(self, job_id):
        result = self.results(job_id)
        urls = [result["results_url"]] if result.get("results_url") else result.get("result_urls", [])
        if not urls:
            raise APIError(0, f"job {job_id} has no results")
        output = bytearray()
        for url in urls:
            try:
                with urllib.request.urlopen(url, timeout=self.timeout) as response:
                    output.extend(response.read())
            except urllib.error.HTTPError as error:
                raise APIError(error.code, error.read().decode("utf-8", "replace"), "GET", url) from None
            except urllib.error.URLError as error:
                raise APIError(0, str(error.reason), "GET", url) from None
        return bytes(output)

    def results_text(self, job_id):
        return self.results_bytes(job_id).decode("utf-8", "replace")

    def results_records(self, job_id):
        return [json.loads(line) for line in self.results_text(job_id).splitlines() if line]

    def embeddings(self, model, input, *, binary=None, timeout=1800.0, poll=3.0,
                   **submit_options):
        texts = [input] if isinstance(input, str) else list(input)
        payload = "".join(json.dumps({"text": text}) + "\n" for text in texts)
        if binary is not None:
            submit_options["binary"] = binary
        job = self.submit_job(model, "embed", payload, **submit_options)
        self.wait(job["job_id"], timeout, poll)
        raw = self.results_bytes(job["job_id"])
        if is_embeddings_binary(raw):
            rows = enumerate(decode_embeddings_binary(raw))
        else:
            records = [json.loads(line) for line in raw.decode().splitlines() if line]
            rows = ((record.get("index", i), record.get("vector", record.get("embedding")))
                    for i, record in enumerate(records))
        data = [{"object": "embedding", "embedding": vector, "index": index}
                for index, vector in rows]
        if any(row["embedding"] is None for row in data):
            raise APIError(0, "embed result missing vector")
        data.sort(key=lambda row: row["index"])
        return {"object": "list", "data": data, "model": model,
                "usage": {"prompt_tokens": 0, "total_tokens": 0}}
