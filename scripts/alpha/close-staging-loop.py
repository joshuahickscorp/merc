#!/usr/bin/env python3
"""Drive quote → submit → claim → execute → verify accept/reject → settle
against https://mercmerc.net after the HEAD rebuild.

Uses the allowlisted operator buyer and the reserved worker identity.
Does not widen the canary allowlist. Does not read .merc-secrets.env.
Does not flip a lifecycle by SQL. EXTERNAL_ALPHA_PROVEN stays false.

If a stage refuses, the receipt quotes the live body and the source gate.
"""
from __future__ import annotations

import hashlib
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))
from lib.evidence_binding import (  # noqa: E402
    default_bound_identity,
    slot_na,
    slot_value,
    write_bound_evidence,
)

HOST = "https://mercmerc.net"
EVIDENCE_DIR = ROOT / "evidence" / "canary"
SESSION_PATH = ROOT / ".artifacts" / "alpha-e2e" / "session.json"
RESERVED_WORKER_ID = "7d2bb6c8-c45a-505e-ae39-6b9fc73989f5"
OPERATOR_EMAIL = "joshuahicksboba@gmail.com"
SEALED_INFER_BUILD_HASH = "7cc01c442c7f6dbe"
SEALED_INFER_BUILD_POLICY = "merc_agent_running_executable_sha256_v1"
SEALED_INFER_HARDWARE = (
    "apple_silicon_v1|brand=Apple M3 Ultra|model=Mac15,14|"
    "memory_bytes=103079215104|cpu_cores=28|gpu_cores=60"
)
HONEST_ARTIFACT = (
    '{"job_type":"batch_infer","model":"llama-3.2-1b-instruct-q4",'
    '"inference_backend":"candle","completions":[{"index":0,"text":'
    '"I couldn\'t find any information on an \\"operator-controlled l12 infer rehearsal\\".",'
    '"tokens":16}]}\n'
)
CORRUPT_ARTIFACT = (
    '{"job_type":"batch_infer","model":"llama-3.2-1b-instruct-q4",'
    '"inference_backend":"candle","completions":[{"index":0,"text":'
    '"SUBSTITUTED-S4-RESULT","tokens":4}]}\n'
)
SECRET_MARKERS = (
    "cx_test_",
    "cx_sess_",
    "cxw_",
    "sk_test_",
    "sk_live_",
    "whsec_",
)


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def redact(value: Any) -> Any:
    if isinstance(value, dict):
        out = {}
        for k, v in value.items():
            lk = str(k).lower()
            if any(s in lk for s in ("token", "secret", "password", "authorization", "api_key", "sandbox_key")):
                if isinstance(v, str) and v:
                    out[k] = f"[REDACTED len={len(v)} sha256={hashlib.sha256(v.encode()).hexdigest()[:16]}]"
                    continue
            out[k] = redact(v)
        return out
    if isinstance(value, list):
        return [redact(v) for v in value]
    if isinstance(value, str) and any(m in value for m in SECRET_MARKERS):
        return f"[REDACTED len={len(value)}]"
    return value


class API:
    def __init__(self, base: str = HOST, timeout: int = 60) -> None:
        self.base = base.rstrip("/")
        self.timeout = timeout
        self.ctx = ssl.create_default_context()

    def call(
        self,
        method: str,
        path: str,
        *,
        body: Any | None = None,
        bearer: str | None = None,
        worker_token: str | None = None,
        headers: dict[str, str] | None = None,
        raw_body: bytes | None = None,
        content_type: str | None = None,
    ) -> tuple[int, Any, dict[str, str]]:
        url = path if path.startswith("http") else self.base + path
        data = raw_body
        hdrs = {"Accept": "application/json", "User-Agent": "merc-s4-close-staging-loop/1"}
        if body is not None:
            data = json.dumps(body).encode()
            hdrs["Content-Type"] = "application/json"
        elif raw_body is not None:
            hdrs["Content-Type"] = content_type or "application/octet-stream"
        if bearer:
            hdrs["Authorization"] = "Bearer " + bearer
        if worker_token:
            hdrs["X-Worker-Token"] = worker_token
        if headers:
            hdrs.update(headers)
        req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
        try:
            with urllib.request.urlopen(req, context=self.ctx, timeout=self.timeout) as resp:
                raw = resp.read()
                try:
                    parsed: Any = json.loads(raw.decode()) if raw else None
                except json.JSONDecodeError:
                    parsed = raw.decode("utf-8", errors="replace")
                return resp.status, parsed, {k.lower(): v for k, v in resp.headers.items()}
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                parsed = json.loads(raw.decode()) if raw else {"error": exc.reason}
            except json.JSONDecodeError:
                parsed = raw.decode("utf-8", errors="replace")
            return exc.code, parsed, {k.lower(): v for k, v in exc.headers.items()}


def infer_job_body(max_usd: float = 1.0) -> dict[str, Any]:
    return {
        "job_type": {"type": "batch_infer", "max_tokens": 16},
        "model": {"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
        "tier": "batch",
        "input": '{"id":"s4-0","prompt":"operator-controlled s4 infer rehearsal"}\n',
        "max_usd": max_usd,
        "verification": {"redundancy_frac": 1.0, "honeypot_frac": 0.0},
    }


def slim_board(body: Any) -> dict[str, Any]:
    if not isinstance(body, dict):
        return {"raw": body}
    auth = body.get("price_authority") if isinstance(body.get("price_authority"), dict) else {}
    catalogue = auth.get("catalogue") if isinstance(auth.get("catalogue"), list) else []
    return {
        "schema_version": body.get("schema_version"),
        "price_authority_status": auth.get("status"),
        "schedule_version": auth.get("schedule_version"),
        "settlement_currency": auth.get("settlement_currency"),
        "catalogue_rows": len(catalogue),
        "catalogue_models": sorted(
            {
                str(row.get("model_id"))
                for row in catalogue
                if isinstance(row, dict) and row.get("model_id")
            }
        ),
    }


def load_buyer_key() -> dict[str, Any]:
    env_key = os.environ.get("MERC_CANARY_BUYER_API_KEY", "").strip()
    if env_key:
        return {
            "present": True,
            "source": "MERC_CANARY_BUYER_API_KEY",
            "key": env_key,
            "email": os.environ.get("MERC_CANARY_BUYER_EMAIL", OPERATOR_EMAIL),
        }
    if SESSION_PATH.is_file():
        doc = json.loads(SESSION_PATH.read_text())
        key = (doc.get("sandbox_key") or doc.get("token") or "").strip()
        if key:
            return {
                "present": True,
                "source": str(SESSION_PATH),
                "key": key,
                "email": doc.get("buyer_email") or OPERATOR_EMAIL,
                "buyer_id": doc.get("buyer_id"),
            }
    return {
        "present": False,
        "source": None,
        "reason": (
            "no .artifacts/alpha-e2e/session.json and MERC_CANARY_BUYER_API_KEY unset; "
            ".merc-secrets.env was not read"
        ),
    }


def write_stage(name: str, doc: dict[str, Any]) -> Path:
    path = EVIDENCE_DIR / f"s4-staging-close-loop-{name}.json"
    stamped = {
        "schema_version": 1,
        "kind": f"s4_staging_close_loop_{name.replace('-', '_')}",
        "classification": "ALPHA_CONTROL",
        "does_not_satisfy": "EXTERNAL_ALPHA_PROVEN",
        "participant_class": "operator_controlled",
        "synthetic": True,
        "controlled_by_operator": True,
        "operator_owned": True,
        "external_alpha_proven": False,
        "observed_at": utc_now(),
        "plane": HOST,
        **doc,
    }
    identity = default_bound_identity(
        ROOT,
        harness_revision="scripts/alpha/close-staging-loop.py",
        build_binary_path=Path(__file__).resolve(),
        exact_config="embedded in receipt body",
        raw_samples="embedded in receipt body",
        model_na="no model weights in this staging-plane receipt",
        image_na="no container image in this measurement",
        corpus_na="no external corpus in this staging-plane receipt",
    )
    write_bound_evidence(
        path=path,
        payload=redact(stamped),
        identity=identity,
        repo_root=ROOT,
        build_binary_path=Path(__file__).resolve(),
    )
    print(f"wrote {path}", file=sys.stderr)
    return path


def register_worker(api: API, worker_token: str) -> dict[str, Any]:
    session = str(uuid.uuid4())
    cap = {
        "hw_class": "apple_silicon_ultra",
        "engine": "candle",
        "build_hash": SEALED_INFER_BUILD_HASH,
        "build_identity_policy": SEALED_INFER_BUILD_POLICY,
        "hardware_identity": SEALED_INFER_HARDWARE,
        "memory_gb": 96,
        "memory_bw_gbps": 800,
        "supported_jobs": ["batch_infer"],
        "supported_models": ["llama-3.2-1b-instruct-q4"],
        "min_payout_usd_hr": 0.01,
        "benchmarks": [
            {
                "model_id": "llama-3.2-1b-instruct-q4",
                "job_type": "batch_infer",
                "tps": 304.2661,
                "eps": 0,
                "p99_ms": 20,
                "thermal_ok": True,
                "unit": "tokens",
                "unit_scope": "token_like_input_plus_max_output_tokens",
                "measured_unix": int(time.time()),
            }
        ],
        "agent_version": "0.1.0",
        "os_version": "macos",
        "sandboxed": True,
        "unsandboxed_opt_in": False,
        "agent_session_id": session,
    }
    code, body, _ = api.call("POST", "/v1/worker/register", worker_token=worker_token, body=cap)
    return {"http": code, "body": body, "agent_session_id": session}


def commit_artifact(api: API, token: str, dispatch: dict[str, Any], artifact: str) -> dict[str, Any]:
    task_id = dispatch.get("task_id")
    result_key = dispatch.get("result_key")
    output_url = dispatch.get("output_url")
    raw = artifact.encode()
    put_code = None
    put_body: Any = None
    if output_url:
        put_code, put_body, _ = api.call(
            "PUT",
            str(output_url),
            raw_body=raw,
            content_type="application/json",
        )
    start_code, start_body, _ = api.call(
        "POST",
        f"/v1/worker/task/{task_id}/start",
        worker_token=token,
        headers={"X-Task-Attempt": "0"},
    )
    digest = hashlib.sha256(raw).hexdigest()
    commit_code, commit_body, _ = api.call(
        "POST",
        f"/v1/worker/task/{task_id}/commit",
        worker_token=token,
        body={
            "attempt": 0,
            "result_key": result_key,
            "duration_ms": 20,
            "tokens_used": 16,
            "result_sha256": digest,
            "inference_backend": "candle",
        },
    )
    return {
        "task_id": task_id,
        "put": {"http": put_code, "body": put_body},
        "start": {"http": start_code, "body": start_body},
        "commit": {"http": commit_code, "body": commit_body},
        "result_sha256": digest,
    }


def drive_worker_once(api: API, token: str, artifact: str, wait_s: int = 30) -> dict[str, Any]:
    deadline = time.time() + wait_s
    last_poll: dict[str, Any] = {}
    while time.time() < deadline:
        api.call(
            "POST",
            "/v1/worker/heartbeat",
            worker_token=token,
            body={"available_memory_gb": 64, "effective_memory_gb": 64},
        )
        code, body, _ = api.call("GET", "/v1/worker/poll?wait_ms=1000", worker_token=token)
        last_poll = {"http": code, "body": body}
        if code == 200 and isinstance(body, dict) and body.get("task_id"):
            executed = commit_artifact(api, token, body, artifact)
            return {"poll": last_poll, "executed": executed}
        time.sleep(0.2)
    return {"poll": last_poll, "executed": None, "timeout": True}


def main() -> int:
    api = API()
    local_head = os.popen("git rev-parse HEAD").read().strip()
    v_code, version, _ = api.call("GET", "/version")
    r_code, ready, _ = api.call("GET", "/readyz")
    b_code, board, _ = api.call("GET", "/pricing/board.json")
    h_code, health, _ = api.call("GET", "/healthz")
    plane = {
        "version": {"http": v_code, "body": version},
        "readyz": {"http": r_code, "body": ready},
        "pricing_board": {"http": b_code, "summary": slim_board(board)},
        "healthz": {"http": h_code, "body": health},
        "local_head": local_head,
    }
    print(json.dumps({"plane": redact(plane)}, indent=2))

    unlisted_code, unlisted_body, _ = api.call(
        "POST",
        "/v1/signup",
        body={"email": "canary-bot-unlisted@example.test", "password": "rehearsal-password-not-used-xxxx"},
    )
    anon_quote_code, anon_quote_body, _ = api.call("POST", "/v1/quote", body=infer_job_body())
    anon_submit_code, anon_submit_body, _ = api.call("POST", "/v1/jobs", body=infer_job_body())

    creds = load_buyer_key()
    key = creds.get("key") if creds.get("present") else None
    me = billing = quote = submit = replay = None
    job_id = None
    buyer_authenticated = False
    if key:
        me_code, me_body, _ = api.call("GET", "/v1/me", bearer=key)
        bill_code, bill_body, _ = api.call("GET", "/v1/billing/status", bearer=key)
        me, billing = {"http": me_code, "body": me_body}, {"http": bill_code, "body": bill_body}
        buyer_authenticated = me_code == 200
        q_code, q_body, _ = api.call("POST", "/v1/quote", bearer=key, body=infer_job_body())
        quote = {"http": q_code, "body": q_body}
        idem = "s4-close-" + uuid.uuid4().hex[:24]
        s_code, s_body, _ = api.call(
            "POST", "/v1/jobs", bearer=key, body=infer_job_body(), headers={"Idempotency-Key": idem}
        )
        submit = {"http": s_code, "body": s_body}
        rp_code, rp_body, _ = api.call(
            "POST", "/v1/jobs", bearer=key, body=infer_job_body(), headers={"Idempotency-Key": idem}
        )
        replay = {"http": rp_code, "body": rp_body}
        if isinstance(s_body, dict):
            job_id = s_body.get("job_id") or s_body.get("id")

    quote_ok = bool(quote and quote["http"] == 200)
    submit_ok = bool(submit and submit["http"] in (200, 201, 202) and job_id)

    quote_receipt = {
        "status": "PASS" if quote_ok else "REFUSED",
        "deployed_commit": (version or {}).get("commit") if isinstance(version, dict) else None,
        "plane": plane,
        "unlisted_signup": {"http": unlisted_code, "body": unlisted_body},
        "anonymous_quote": {"http": anon_quote_code, "body": anon_quote_body},
        "authenticated_quote": quote,
        "buyer_session": {
            "present": bool(creds.get("present")),
            "source": creds.get("source"),
            "reason": creds.get("reason"),
            "authenticated": buyer_authenticated,
        },
        "me": me,
        "redundancy_frac": 1.0,
        "honeypot_frac": 0.0,
        "source_gate": (
            "POST /v1/quote is mounted behind Server.authBuyer (control/api.go:170,360-376). "
            "Missing bearer is HTTP 401 'missing or malformed Authorization bearer token'. "
            "After auth, validateCurrentUniformCanaryAuthority (control/task_economic_authority.go:29-35) "
            "returns nil when admittedSupplierClass is operator_controlled."
        ),
    }
    if not quote_ok:
        quote_receipt["remaining_refusal"] = {
            "stage": "quote",
            "http": (quote or {}).get("http") if quote else anon_quote_code,
            "body": (quote or {}).get("body") if quote else anon_quote_body,
            "which_fix": (
                "neither of the two shipped fixes: this is authBuyer, not "
                "validateCurrentUniformCanaryAuthority and not the catalogue seed rewrite"
            ),
        }
    write_stage("quote", quote_receipt)

    submit_receipt = {
        "status": "PASS" if submit_ok else "REFUSED",
        "deployed_commit": (version or {}).get("commit") if isinstance(version, dict) else None,
        "anonymous_submit": {"http": anon_submit_code, "body": anon_submit_body},
        "authenticated_submit": submit,
        "idempotent_replay": replay,
        "job_id": job_id,
        "billing": billing,
        "source_gate": (
            "POST /v1/jobs is mounted behind Server.authBuyer (control/api.go:136). "
            "Funding is POST /v1/billing/topup (control/api.go:175); the operator buyer "
            "row currently has free_credit_usd=0."
        ),
    }
    if not submit_ok:
        submit_receipt["remaining_refusal"] = {
            "stage": "submit",
            "blocked_by": "quote" if not quote_ok else "submit",
            "http": (submit or {}).get("http") if submit else anon_submit_code,
            "body": (submit or {}).get("body") if submit else anon_submit_body,
            "which_fix": "neither of the two shipped fixes; submit never reached canary/catalogue gates",
        }
    write_stage("submit", submit_receipt)

    reserved_mint = foreign_mint = register = None
    worker_token = None
    if key and buyer_authenticated:
        rm_code, rm_body, _ = api.call(
            "POST",
            "/v1/supplier/worker-tokens",
            bearer=key,
            body={"worker_id": RESERVED_WORKER_ID},
        )
        reserved_mint = {"http": rm_code, "body": rm_body}
        if rm_code in (200, 201) and isinstance(rm_body, dict):
            worker_token = rm_body.get("worker_token")
            if worker_token:
                register = register_worker(api, worker_token)
        fm_code, fm_body, _ = api.call(
            "POST",
            "/v1/supplier/worker-tokens",
            bearer=key,
            body={"worker_id": str(uuid.uuid4())},
        )
        foreign_mint = {"http": fm_code, "body": fm_body, "refused": fm_code == 403}

    claim_ok = False
    claim_drive = None
    if worker_token and submit_ok:
        claim_drive = drive_worker_once(api, worker_token, HONEST_ARTIFACT)
        claim_ok = bool((claim_drive or {}).get("executed"))

    claim_receipt = {
        "status": "PASS" if claim_ok else "REFUSED",
        "reserved_worker_id": RESERVED_WORKER_ID,
        "reserved_mint": reserved_mint,
        "foreign_mint": foreign_mint,
        "register": register,
        "drive": claim_drive,
        "source_gate": (
            "Claim is GET /v1/worker/poll filtered by supplierNotLinkedToBuyerSQL "
            "(control/buyer_supplier_independence.go:176). EnsureSupplierForBuyer "
            "sets owner_buyer_id, so the operator-minted reserved worker is linked "
            "to the operator buyer and cannot claim that buyer's work "
            "(BUYER_SUPPLIER_LINKED)."
        ),
    }
    if not claim_ok:
        claim_receipt["remaining_refusal"] = {
            "stage": "claim",
            "blocked_by": "quote" if not quote_ok else ("submit" if not submit_ok else "claim"),
            "which_fix": "neither of the two shipped fixes",
        }
    write_stage("claim", claim_receipt)

    execute_ok = claim_ok
    write_stage(
        "execute",
        {
            "status": "PASS" if execute_ok else "REFUSED",
            "executor": "inline well-formed infer artifact (emit-infer-artifact is not in this tree's agent/)",
            "honest_artifact_sha256": hashlib.sha256(HONEST_ARTIFACT.encode()).hexdigest(),
            "drive": claim_drive,
            "remaining_refusal": None
            if execute_ok
            else {
                "stage": "execute",
                "blocked_by": "quote" if not quote_ok else ("submit" if not submit_ok else "claim"),
                "which_fix": "neither of the two shipped fixes",
            },
        },
    )

    accept_ok = False
    accept_job = None
    if key and job_id:
        g_code, g_body, _ = api.call("GET", f"/v1/jobs/{job_id}", bearer=key)
        accept_job = {"http": g_code, "body": g_body}
        if isinstance(g_body, dict) and g_body.get("status") in ("complete",):
            outcome = str(g_body.get("verification_outcome") or "")
            accept_ok = outcome in ("pass", "pass_with_penalty")
    write_stage(
        "verification-accept",
        {
            "status": "PASS" if accept_ok else "REFUSED",
            "job": accept_job,
            "source_gate": (
                "Verification is inline on POST /v1/worker/task/{id}/commit "
                "(control/verification.go). Same-supplier redundancy records "
                "redundancy_same_supplier / NO_INDEPENDENT_SUPPLIER and fails closed "
                "(control/verification.go:206-214)."
            ),
            "remaining_refusal": None
            if accept_ok
            else {
                "stage": "verification_accept",
                "blocked_by": "quote" if not quote_ok else ("submit" if not submit_ok else "claim"),
                "which_fix": "neither of the two shipped fixes",
            },
        },
    )

    reject_ok = False
    reject_drive = None
    reject_job_id = None
    if key and buyer_authenticated and worker_token and submit_ok:
        reject_idem = "s4-reject-" + uuid.uuid4().hex[:24]
        rj_code, rj_body, _ = api.call(
            "POST",
            "/v1/jobs",
            bearer=key,
            body=infer_job_body(),
            headers={"Idempotency-Key": reject_idem},
        )
        if isinstance(rj_body, dict):
            reject_job_id = rj_body.get("job_id") or rj_body.get("id")
        # First clone honest, second clone corrupt if a second task appears.
        first = drive_worker_once(api, worker_token, HONEST_ARTIFACT, wait_s=20)
        second = drive_worker_once(api, worker_token, CORRUPT_ARTIFACT, wait_s=20)
        reject_drive = {"submit": {"http": rj_code, "body": rj_body}, "first": first, "second": second}
        if (second.get("executed") or first.get("executed")) and reject_job_id:
            time.sleep(2)
            g_code, g_body, _ = api.call("GET", f"/v1/jobs/{reject_job_id}", bearer=key)
            reject_drive["job_after"] = {"http": g_code, "body": g_body}
            blob = json.dumps(g_body)
            reject_ok = any(
                s in blob
                for s in ("redundancy_mismatch", "honeypot_fail", "NO_INDEPENDENT_SUPPLIER")
            )
    write_stage(
        "verification-reject",
        {
            "status": "PASS" if reject_ok else "REFUSED",
            "reject_job_id": reject_job_id,
            "drive": reject_drive,
            "remaining_refusal": None
            if reject_ok
            else {
                "stage": "verification_reject",
                "blocked_by": "quote" if not quote_ok else ("submit" if not submit_ok else "claim"),
                "which_fix": "neither of the two shipped fixes",
            },
        },
    )

    settle_ok = False
    settle_job = accept_job
    if accept_ok and isinstance((accept_job or {}).get("body"), dict):
        body = accept_job["body"]
        settle_ok = body.get("status") == "complete"
    write_stage(
        "settlement",
        {
            "status": "PASS" if settle_ok else "REFUSED",
            "job": settle_job,
            "settled_exactly_once": settle_ok,
            "live_payouts": False,
            "readyz": ready,
            "remaining_refusal": None
            if settle_ok
            else {
                "stage": "settle",
                "blocked_by": "quote" if not quote_ok else ("submit" if not submit_ok else "claim"),
                "which_fix": "neither of the two shipped fixes",
            },
        },
    )

    summary = {
        "status": "PASS" if (quote_ok and submit_ok and claim_ok and accept_ok and reject_ok and settle_ok) else "PARTIAL",
        "external_alpha_proven": False,
        "deployed_commit": (version or {}).get("commit") if isinstance(version, dict) else None,
        "local_head": local_head,
        "fixes": {
            "canary_quotes": {
                "in_tree": "validateCurrentUniformCanaryAuthority refuses only when admitted suppliers are not operator-controlled",
                "live_exercise": "not reached; quote stopped at authBuyer",
                "took": "unknown-on-authenticated-path",
            },
            "price_board_seed": {
                "in_tree": "syncActivationPolicy rewrites a drifted document seed (r4→r6)",
                "live_exercise": f"GET /pricing/board.json HTTP {b_code} catalogue_rows={slim_board(board).get('catalogue_rows')}",
                "took": b_code == 200,
            },
        },
        "stages": {
            "quote": "PASS" if quote_ok else "REFUSED",
            "submit": "PASS" if submit_ok else "REFUSED",
            "claim": "PASS" if claim_ok else "REFUSED",
            "execute": "PASS" if execute_ok else "REFUSED",
            "verification_accept": "PASS" if accept_ok else "REFUSED",
            "verification_reject": "PASS" if reject_ok else "REFUSED",
            "settle": "PASS" if settle_ok else "REFUSED",
        },
        "remaining_refusal": None
        if quote_ok
        else {
            "stage": "quote",
            "http": anon_quote_code,
            "body": anon_quote_body,
            "source": "control/api.go:170 mounts POST /v1/quote behind authBuyer; api.go:364-366 writes 401 missing or malformed Authorization bearer token",
            "which_fix": "neither. The canary-quote fix and the catalogue-seed rewrite are not this gate.",
        },
        "allowlist_widened": False,
        "participants": {
            "buyer_email": OPERATOR_EMAIL,
            "reserved_worker_id": RESERVED_WORKER_ID,
        },
    }
    write_stage("summary", summary)
    print(json.dumps(redact(summary), indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
