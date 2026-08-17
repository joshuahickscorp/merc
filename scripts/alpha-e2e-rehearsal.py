#!/usr/bin/env python3
"""Operator-controlled backend-alpha rehearsal against https://mercmerc.net.

Proves closeout conditions 4–7 (buyer execution, supplier execution,
verification accept+reject, ledger settlement) with synthetic /
operator-controlled participants only. Completing this script MUST NOT
be described as EXTERNAL_ALPHA_PROVEN.

Secrets are read from .artifacts/alpha-e2e/ (gitignored) or the
environment. Receipts written under evidence/ are redacted.
"""

from __future__ import annotations

import argparse
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

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_BASE = "https://mercmerc.net"
SESSION_PATH = ROOT / ".artifacts" / "alpha-e2e" / "session.json"
PARTICIPANTS_PATH = ROOT / "ops" / "staging" / "alpha-participants.json"
EVIDENCE_DIR = ROOT / "evidence" / "canary"
RECEIPT_PREFIX = "l12-p1-canary-rehearsal"

# Approved reserved worker from ops/staging/alpha-participants.json.
RESERVED_WORKER_ID = "7d2bb6c8-c45a-505e-ae39-6b9fc73989f5"
APPROVED_AGENT_VERSION = "0.1.0"
# Sealed r6 candle-metal-llama1-infer identity. The live canary allowlist
# still names the superseded r5 hash f4303a751ca2b2af; this script does not
# widen that list. Registering the sealed identity will be refused by the
# canary build-hash gate until an operator adds 7cc01c properly.
SEALED_INFER_BUILD_HASH = "7cc01c442c7f6dbe"
SEALED_INFER_BUILD_POLICY = "merc_agent_running_executable_sha256_v1"
SEALED_INFER_HARDWARE = (
    "apple_silicon_v1|brand=Apple M3 Ultra|model=Mac15,14|"
    "memory_bytes=103079215104|cpu_cores=28|gpu_cores=60"
)
APPROVED_BUILD_HASH = SEALED_INFER_BUILD_HASH

SECRET_MARKERS = (
    "cx_test_",
    "cx_sess_",
    "cxw_",
    "sk_test_",
    "sk_live_",
    "rk_test_",
    "rk_live_",
    "pk_test_",
    "pk_live_",
    "whsec_",
    "cxeb2_",
    "cxer2_",
    "cxe_",
)


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def die(msg: str) -> None:
    print(f"alpha-e2e: FAIL {msg}", file=sys.stderr)
    raise SystemExit(1)


def log(msg: str) -> None:
    print(f"alpha-e2e: {msg}", file=sys.stderr)


def load_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path: Path, doc: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(doc, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def _looks_like_secret_string(value: str) -> bool:
    return any(marker in value for marker in SECRET_MARKERS)


def redact(value: Any) -> Any:
    if isinstance(value, dict):
        out = {}
        for k, v in value.items():
            lk = str(k).lower()
            name_is_secret = any(
                s in lk
                for s in (
                    "token",
                    "secret",
                    "password",
                    "authorization",
                    "sandbox_key",
                    "api_key",
                    "worker_token",
                )
            )
            if name_is_secret and isinstance(v, str) and v:
                out[k] = (
                    f"[REDACTED len={len(v)} sha256={hashlib.sha256(v.encode()).hexdigest()[:16]}]"
                )
            else:
                out[k] = redact(v)
        return out
    if isinstance(value, list):
        return [redact(v) for v in value]
    if isinstance(value, str) and _looks_like_secret_string(value):
        return f"[REDACTED len={len(value)}]"
    return value


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class API:
    def __init__(self, base: str, timeout: int = 60) -> None:
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
        hdrs = {"Accept": "application/json", "User-Agent": "merc-alpha-e2e-rehearsal/1"}
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
                parsed: Any
                try:
                    parsed = json.loads(raw.decode()) if raw else None
                except json.JSONDecodeError:
                    parsed = raw.decode("utf-8", errors="replace")
                return resp.status, parsed, {k.lower(): v for k, v in resp.headers.items()}
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            parsed: Any
            try:
                parsed = json.loads(raw.decode()) if raw else {"error": exc.reason}
            except json.JSONDecodeError:
                parsed = raw.decode("utf-8", errors="replace")
            return exc.code, parsed, {k.lower(): v for k, v in exc.headers.items()}
        except urllib.error.URLError as exc:
            die(f"{method} {url}: {exc}")


def load_session() -> dict[str, Any]:
    if SESSION_PATH.is_file():
        doc = load_json(SESSION_PATH)
        if isinstance(doc, dict) and doc.get("sandbox_key"):
            return doc
    env_key = os.environ.get("MERC_CANARY_BUYER_API_KEY", "").strip()
    if env_key:
        return {
            "sandbox_key": env_key,
            "buyer_email": os.environ.get("MERC_CANARY_BUYER_EMAIL", ""),
            "base_url": os.environ.get("MERC_CONTROL_BASE_URL", DEFAULT_BASE),
        }
    die(f"no rehearsal session at {SESSION_PATH} and MERC_CANARY_BUYER_API_KEY unset")


def load_participants() -> dict[str, Any]:
    return load_json(PARTICIPANTS_PATH)


def public_plane(api: API) -> dict[str, Any]:
    code, health, _ = api.call("GET", "/healthz")
    if code != 200:
        die(f"/healthz HTTP {code}: {health}")
    code, ready, _ = api.call("GET", "/readyz")
    if code != 200:
        die(f"/readyz HTTP {code}: {ready}")
    code, version, _ = api.call("GET", "/version")
    if code != 200:
        die(f"/version HTTP {code}: {version}")
    if not isinstance(ready, dict) or ready.get("payment_mode") != "test":
        die(f"/readyz is not test-mode: {ready}")
    if ready.get("live_value_movement") is not False:
        die(f"/readyz live_value_movement is not false: {ready}")
    return {"health": health, "ready": ready, "version": version}


def probe_buyer(api: API, key: str) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for name, method, path, body in (
        ("me", "GET", "/v1/me", None),
        ("billing", "GET", "/v1/billing/status", None),
        ("keys", "GET", "/v1/keys", None),
        ("models", "GET", "/v1/models", None),
        ("jobs", "GET", "/v1/jobs", None),
        ("supplier_status", "GET", "/v1/supplier/status", None),
        ("worker_credentials", "GET", "/v1/supplier/worker-credentials", None),
        (
            "price_estimate",
            "GET",
            "/v1/price-estimate?model=all-minilm-l6-v2&units=1&tier=batch",
            None,
        ),
    ):
        code, payload, _ = api.call(method, path, bearer=key, body=body)
        out[name] = {"http": code, "body": payload}
        log(f"probe {name} -> {code}")
    return out


def unlisted_signup_refused(api: API) -> dict[str, Any]:
    code, payload, _ = api.call(
        "POST",
        "/v1/signup",
        body={
            "email": "canary-bot-unlisted@example.test",
            "password": "rehearsal-password-not-used-xxxx",
        },
    )
    return {"http": code, "body": payload, "refused": code == 403}


def embed_job_body(max_usd: float = 1.0) -> dict[str, Any]:
    # One JSONL record. Canary forces redundancy_frac=1 and honeypot_frac>=0.1.
    line = json.dumps({"id": "l11-0", "text": "operator-controlled canary rehearsal embed"})
    return {
        "job_type": {"type": "embed"},
        "model": {"ref": "all-minilm-l6-v2"},
        "tier": "batch",
        "input": line + "\n",
        "max_usd": max_usd,
        "constraints": {"min_memory_gb": 0, "max_duration_secs": 120},
        "verification": {"redundancy_frac": 1.0, "honeypot_frac": 0.1},
    }


def batch_infer_job_body(max_usd: float = 1.0) -> dict[str, Any]:
    line = json.dumps(
        {
            "id": "l11-0",
            "prompt": "operator-controlled canary rehearsal batch_infer",
        }
    )
    return {
        "job_type": {"type": "batch_infer", "max_tokens": 16, "temperature": 0},
        "model": {"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
        "tier": "batch",
        "input": line + "\n",
        "max_usd": max_usd,
        "constraints": {"min_memory_gb": 0, "max_duration_secs": 120},
        "verification": {"redundancy_frac": 1.0},
    }


def try_quote_and_submit(
    api: API, key: str, body: dict[str, Any], idem: str
) -> dict[str, Any]:
    q_code, q_body, _ = api.call("POST", "/v1/quote", bearer=key, body=body)
    s_code, s_body, s_hdrs = api.call(
        "POST",
        "/v1/jobs",
        bearer=key,
        body=body,
        headers={"Idempotency-Key": idem},
    )
    replay_code, replay_body, replay_hdrs = api.call(
        "POST",
        "/v1/jobs",
        bearer=key,
        body=body,
        headers={"Idempotency-Key": idem},
    )

    def slim(h: dict[str, str]) -> dict[str, str]:
        keep = ("idempotent-replayed", "x-request-id", "content-type")
        return {k: v for k, v in (h or {}).items() if k in keep}

    return {
        "quote": {"http": q_code, "body": q_body},
        "submit": {"http": s_code, "body": s_body, "headers": slim(s_hdrs)},
        "replay": {
            "http": replay_code,
            "body": replay_body,
            "headers": slim(replay_hdrs),
            "idempotent_replayed": (replay_hdrs or {}).get("idempotent-replayed"),
        },
    }


def mint_worker_token(api: API, key: str, worker_id: str) -> dict[str, Any]:
    code, body, _ = api.call(
        "POST",
        "/v1/supplier/worker-tokens",
        bearer=key,
        body={"worker_id": worker_id},
    )
    return {"http": code, "body": body}


def register_worker(api: API, worker_token: str, *, foreign: bool = False) -> dict[str, Any]:
    session = str(uuid.uuid4())
    now = int(time.time())
    cap = {
        "hw_class": "apple_silicon_ultra",
        "engine": "candle",
        "build_hash": SEALED_INFER_BUILD_HASH if not foreign else "deadbeefdeadbeef",
        "build_identity_policy": SEALED_INFER_BUILD_POLICY,
        "hardware_identity": SEALED_INFER_HARDWARE if not foreign else "Apple M1 Ultra",
        "memory_gb": 96,
        "memory_bw_gbps": 800,
        "supported_jobs": ["batch_infer"],
        "supported_models": ["llama-3.2-1b-instruct-q4"],
        "min_payout_usd_hr": 0.01,
        "benchmarks": []
        if foreign
        else [
            {
                "model_id": "llama-3.2-1b-instruct-q4",
                "job_type": "batch_infer",
                "tps": 304.2661,
                "eps": 0,
                "p99_ms": 20,
                "thermal_ok": True,
                "unit": "tokens",
                "unit_scope": "token_like_input_plus_max_output_tokens",
                "measured_unix": now,
            }
        ],
        "agent_version": APPROVED_AGENT_VERSION if not foreign else "9.9.9",
        "os_version": "macos",
        "sandboxed": True,
        "unsandboxed_opt_in": False,
        "agent_session_id": session,
    }
    code, body, _ = api.call(
        "POST", "/v1/worker/register", worker_token=worker_token, body=cap
    )
    return {"http": code, "body": body, "agent_session_id": session, "capability": cap}


def poll_worker(api: API, worker_token: str, wait_ms: int = 1000) -> dict[str, Any]:
    code, body, _ = api.call(
        "GET", f"/v1/worker/poll?wait_ms={wait_ms}", worker_token=worker_token
    )
    return {"http": code, "body": body}


def write_receipt(name: str, doc: dict[str, Any]) -> Path:
    path = EVIDENCE_DIR / f"{RECEIPT_PREFIX}-{name}.json"
    stamped = {
        "schema_version": 1,
        "kind": f"p1_canary_rehearsal_{name}",
        "gate": "P1-CANARY-REHEARSAL",
        "classification": "ALPHA_CONTROL",
        "does_not_satisfy": "EXTERNAL_ALPHA_PROVEN",
        "participant_class": "operator_controlled",
        "synthetic": True,
        "controlled_by_operator": True,
        "operator_owned": True,
        "external_alpha_proven": False,
        "observed_at": utc_now(),
        "plane": "https://mercmerc.net",
        **doc,
    }
    write_json(path, redact(stamped))
    log(f"wrote {path}")
    return path


def cmd_probe(args: argparse.Namespace) -> None:
    session = load_session()
    api = API(session.get("base_url") or DEFAULT_BASE)
    plane = public_plane(api)
    log(f"deployed commit {plane['version'].get('commit')}")
    unlisted = unlisted_signup_refused(api)
    buyer = probe_buyer(api, session["sandbox_key"])
    write_json(
        ROOT / ".artifacts" / "alpha-e2e" / "probe.json",
        redact(
            {
                "observed_at": utc_now(),
                "plane": plane,
                "unlisted_signup": unlisted,
                "buyer": buyer,
                "local_head": os.popen("git rev-parse HEAD").read().strip(),
            }
        ),
    )
    print(json.dumps(redact({"plane": plane, "unlisted_signup": unlisted, "buyer": buyer}), indent=2))


def cmd_buyer(args: argparse.Namespace) -> None:
    session = load_session()
    api = API(session.get("base_url") or DEFAULT_BASE)
    plane = public_plane(api)
    key = session["sandbox_key"]
    idem = "l12-rehearsal-" + uuid.uuid4().hex
    embed = try_quote_and_submit(api, key, embed_job_body(), idem)
    infer_idem = "l12-rehearsal-infer-" + uuid.uuid4().hex
    infer = try_quote_and_submit(api, key, batch_infer_job_body(), infer_idem)
    write_json(
        ROOT / ".artifacts" / "alpha-e2e" / "buyer-attempt.json",
        redact({"embed": embed, "batch_infer": infer, "plane": plane, "idem": idem}),
    )
    print(json.dumps(redact({"embed": embed, "batch_infer": infer}), indent=2))


def cmd_supplier(args: argparse.Namespace) -> None:
    session = load_session()
    api = API(session.get("base_url") or DEFAULT_BASE)
    key = session["sandbox_key"]
    reserved = mint_worker_token(api, key, RESERVED_WORKER_ID)
    foreign = mint_worker_token(api, key, str(uuid.uuid4()))
    result = {"reserved_mint": reserved, "foreign_mint": foreign}
    token = None
    if reserved["http"] in (200, 201) and isinstance(reserved["body"], dict):
        token = reserved["body"].get("worker_token")
        if token:
            result["register"] = register_worker(api, token)
            result["poll"] = poll_worker(api, token)
            result["heartbeat"] = {
                "http": api.call(
                    "POST",
                    "/v1/worker/heartbeat",
                    worker_token=token,
                    body={"available_memory_gb": 64},
                )[0]
            }
    write_json(ROOT / ".artifacts" / "alpha-e2e" / "supplier-attempt.json", redact(result))
    print(json.dumps(redact(result), indent=2))


def cmd_run(args: argparse.Namespace) -> None:
    session = load_session()
    participants = load_participants()
    api = API(session.get("base_url") or DEFAULT_BASE)
    key = session["sandbox_key"]
    plane = public_plane(api)
    deployed = (plane.get("version") or {}).get("commit", "")
    local_head = os.popen("git rev-parse HEAD").read().strip()

    unlisted = unlisted_signup_refused(api)
    me_code, me_body, _ = api.call("GET", "/v1/me", bearer=key)
    bill_code, bill_body, _ = api.call("GET", "/v1/billing/status", bearer=key)
    models_code, models_body, _ = api.call("GET", "/v1/models", bearer=key)

    anon_code, anon_body, _ = api.call("POST", "/v1/jobs", body=embed_job_body())
    missing_idem_code, missing_idem_body, _ = api.call(
        "POST", "/v1/jobs", bearer=key, body=embed_job_body()
    )

    idem = "l12-rehearsal-infer-" + uuid.uuid4().hex[:24]
    first = try_quote_and_submit(api, key, batch_infer_job_body(), idem)
    embed_idem = "l12-rehearsal-embed-" + uuid.uuid4().hex[:24]
    embed_attempt = try_quote_and_submit(api, key, embed_job_body(), embed_idem)
    infer_attempt = first
    jobs_code, jobs_body, _ = api.call("GET", "/v1/jobs", bearer=key)
    job_ids: list[str] = []
    if isinstance(jobs_body, dict):
        for row in jobs_body.get("jobs") or []:
            if isinstance(row, dict) and row.get("id"):
                job_ids.append(str(row["id"]))
            elif isinstance(row, dict) and row.get("job_id"):
                job_ids.append(str(row["job_id"]))

    reserved = mint_worker_token(api, key, RESERVED_WORKER_ID)
    foreign_id = str(uuid.uuid4())
    foreign = mint_worker_token(api, key, foreign_id)
    register = {}
    poll = {}
    token = None
    if reserved["http"] in (200, 201) and isinstance(reserved["body"], dict):
        token = reserved["body"].get("worker_token")
        if token:
            register = register_worker(api, token)
            poll = poll_worker(api, token)

    creds_code, creds_body, _ = api.call(
        "GET", "/v1/supplier/worker-credentials", bearer=key
    )
    revoked: list[dict[str, Any]] = []
    if isinstance(creds_body, dict):
        for cred in creds_body.get("credentials") or []:
            if not isinstance(cred, dict):
                continue
            cid = cred.get("credential_id") or cred.get("id")
            if not cid:
                continue
            r_code, r_body, _ = api.call(
                "DELETE", f"/v1/supplier/worker-credentials/{cid}", bearer=key
            )
            revoked.append({"credential_id": cid, "http": r_code, "body": r_body})

    poll_after = {}
    if token:
        poll_after = poll_worker(api, token)

    admin_from_operator = api.call("GET", "/admin/controls", bearer=key)
    ready_code, ready_body, _ = api.call("GET", "/readyz")

    buyer_submit_accepted = first["submit"]["http"] in (200, 201, 202)
    same_job_on_replay = False
    if buyer_submit_accepted:
        a = first["submit"]["body"] if isinstance(first["submit"]["body"], dict) else {}
        b = first["replay"]["body"] if isinstance(first["replay"]["body"], dict) else {}
        same_job_on_replay = a.get("job_id") == b.get("job_id") and bool(a.get("job_id"))

    buyer_receipt = {
        "status": "PARTIAL" if not buyer_submit_accepted else "PASS",
        "deployed_commit": deployed,
        "local_head": local_head,
        "commit_changed_under_lane": deployed != "8283ae583057d6265947a473023e4f05102704b4",
        "participant": {
            "role": "buyer",
            "participant_class": "operator_controlled",
            "email": session.get("buyer_email"),
            "buyer_id": session.get("buyer_id") or (me_body or {}).get("buyer_id")
            if isinstance(me_body, dict)
            else None,
            "origin": "approved canary email; signed up this rehearsal",
        },
        "plane": plane,
        "unlisted_signup_refused": unlisted,
        "me": {"http": me_code, "body": me_body},
        "billing": {"http": bill_code, "body": bill_body},
        "models": {"http": models_code, "body": models_body},
        "anonymous_submit": {"http": anon_code, "body": anon_body},
        "missing_idempotency_key": {
            "http": missing_idem_code,
            "body": missing_idem_body,
        },
        "authenticated_submit": first,
        "authenticated_submit_batch_infer": infer_attempt,
        "authenticated_submit_embed": embed_attempt,
        "jobs_after_submit": {"http": jobs_code, "ids": job_ids, "body": jobs_body},
        "proven": {
            "approved_buyer_authenticated": me_code == 200,
            "unlisted_email_refused": unlisted.get("refused") is True,
            "anonymous_submit_refused": anon_code == 401,
            "idempotency_key_required": missing_idem_code == 400,
            "job_accepted_priced_claimable": buyer_submit_accepted,
            "idempotent_replay_one_job": same_job_on_replay,
        },
        "blocked": None
        if buyer_submit_accepted
        else {
            "step": "POST /v1/jobs accepted into a claimable state",
            "reason": (
                "POST /v1/quote refuses in normalizeAndValidateJobSubmit via "
                "normalizeAdvertisedRuntimeModelRef → validateAdvertisedRuntimeJobModel "
                "when advertisedRuntimeCapabilities() has no (job, model) pair. "
                "That set is currentActivation().advertised = ACTIVE lifecycle AND "
                "cell.Routable = cellAuthorityBindable (BOUND + 16-hex "
                "engine_build_hash + exact Apple hardware_identity). Embed stays "
                "parked on an empty engine_build_hash. Infer is document-advertised "
                "under r6 (7cc01c442c7f6dbe) but a live overlay can QUARANTINE the "
                "ACTIVE row when storedRoutableEntryHasCurrentGlobalAuthority fails. "
                "A registered worker is not consulted at this 400. This script does "
                "not widen ops/staging/alpha-participants.json; the canary allowlist "
                "still names the superseded r5 hash f4303a751ca2b2af."
            ),
        },
    }
    write_receipt("buyer-execution", buyer_receipt)

    supplier_receipt = {
        "status": "PARTIAL",
        "deployed_commit": deployed,
        "participant": {
            "role": "supplier_worker",
            "participant_class": "operator_controlled",
            "worker_id": RESERVED_WORKER_ID,
            "origin": "allowlisted reserved UUID; unbound staging token mint",
        },
        "reserved_mint": reserved,
        "foreign_worker_mint": {
            "worker_id": foreign_id,
            **foreign,
            "refused": foreign["http"] == 403,
        },
        "register": register,
        "poll_before_revoke": poll,
        "credentials_list": {"http": creds_code, "body": creds_body},
        "revocation": revoked,
        "poll_after_revoke": poll_after,
        "proven": {
            "reserved_worker_token_minted": reserved["http"] in (200, 201),
            "foreign_worker_id_refused": foreign["http"] == 403,
            "identity_binding_enforced_at_canary_gate": foreign["http"] == 403,
            "worker_registered": (register or {}).get("http") == 200,
            "offer_claimed_and_executed": False,
            "revocation_attempted": bool(revoked),
        },
        "blocked": {
            "step": "register, claim, execute, return a result",
            "reason": (
                "POST /v1/worker/register is refused unless the worker offers the "
                "sealed r6 identity (hw_class/build_hash/hardware_identity/"
                "build_identity_policy plus a fresh bench above the conservative "
                "floor). projectWorkerRuntimeCapabilities (control/runtime_matrix.go) "
                "is the named gate. Live canary additionally requires the worker "
                "build hash to be in MERC_CANARY_APPROVED_BUILD_HASHES, which still "
                "lists only the superseded r5 hash f4303a751ca2b2af. This script "
                "registers 7cc01c442c7f6dbe and does not widen the allowlist."
            ),
        },
        "workers_exercised": 1,
        "buyers_exercised": 1,
        "matrix_wants": {"synthetic_buyers": 2, "operator_metal_workers": 2},
    }
    write_receipt("supplier-execution", supplier_receipt)

    verification_receipt = {
        "status": "BLOCKED",
        "deployed_commit": deployed,
        "accept": {
            "ran": False,
            "reason": "no job reached commit; verification accept cannot run",
        },
        "reject": {
            "ran": False,
            "reason": "no job reached commit; a corrupted result cannot be submitted",
        },
        "proven": {
            "good_result_accepted": False,
            "corrupted_result_rejected": False,
        },
        "blocked": {
            "step": "verification accept AND reject on a live result",
            "reason": (
                "Verification is inline on POST /v1/worker/task/{id}/commit. "
                "No task was claimed or committed because no job was accepted "
                "and no worker could register. A SQL-inserted task would not be "
                "the real authenticated path."
            ),
        },
    }
    write_receipt("verification", verification_receipt)

    settlement_receipt = {
        "status": "BLOCKED",
        "deployed_commit": deployed,
        "readyz": {"http": ready_code, "body": ready_body},
        "billing": bill_body,
        "jobs_created": job_ids,
        "live_payouts": False,
        "payout_export": os.environ.get("MERC_PAYOUT_EXPORT", "") == "",
        "proven": {
            "settled_exactly_once": False,
            "no_double_settlement_under_repeat": False,
            "amounts_reconcile": False,
            "test_mode_no_live_value": isinstance(ready_body, dict)
            and ready_body.get("payment_mode") == "test"
            and ready_body.get("live_value_movement") is False,
        },
        "blocked": {
            "step": "ledger settlement of an executed job",
            "reason": (
                "No job entered SubmitJobTx, so FinalizeJobTx / chargeForJob never "
                "ran. Ledger proof of single settlement requires an accepted, "
                "verified job. Test-mode and no live-value-movement are proven "
                "via /readyz."
            ),
        },
    }
    write_receipt("settlement", settlement_receipt)

    controls_receipt = {
        "status": "PARTIAL",
        "deployed_commit": deployed,
        "kill_switch": {
            "from_operator_mac": {
                "http": admin_from_operator[0],
                "body": admin_from_operator[1],
            },
            "note": (
                "MERC_ADMIN_CIDRS is 127.0.0.1/32,::1/128 on the control container. "
                "Anonymous/buyer calls from this Mac are refused. Loopback exercise "
                "is attempted separately from the droplet netns."
            ),
        },
        "revocation": revoked,
        "intake_pause_result_retrieval": {
            "ran": False,
            "reason": "no accepted job id and admin source is loopback-only from this Mac",
        },
        "no_payout_export": {
            "readyz_payment_mode": (ready_body or {}).get("payment_mode")
            if isinstance(ready_body, dict)
            else None,
            "live_value_movement": (ready_body or {}).get("live_value_movement")
            if isinstance(ready_body, dict)
            else None,
        },
        "reconciliation": {
            "jobs_on_buyer": job_ids,
            "note": "nothing to reconcile; no settled rehearsal job exists",
        },
    }
    write_receipt("fail-closed-controls", controls_receipt)

    summary = {
        "status": "PARTIAL",
        "closeout": {
            "4_buyer_execution": {
                "status": "PARTIAL",
                "receipt": f"evidence/canary/{RECEIPT_PREFIX}-buyer-execution.json",
            },
            "5_supplier_execution": {
                "status": "PARTIAL",
                "receipt": f"evidence/canary/{RECEIPT_PREFIX}-supplier-execution.json",
            },
            "6_verification": {
                "status": "BLOCKED",
                "receipt": f"evidence/canary/{RECEIPT_PREFIX}-verification.json",
            },
            "7_settlement_ledger": {
                "status": "BLOCKED",
                "receipt": f"evidence/canary/{RECEIPT_PREFIX}-settlement.json",
            },
        },
        "matrix": {
            "wanted_synthetic_buyers": 2,
            "wanted_operator_metal_workers": 2,
            "exercised_buyers": 1,
            "exercised_workers": 1,
            "allowlist_buyers": [
                b.get("email") for b in (participants.get("buyers") or [])
            ],
            "allowlist_workers": [
                w.get("id") for w in (participants.get("workers") or [])
            ],
        },
        "external_alpha_proven": False,
        "deployed_commit": deployed,
        "local_head": local_head,
    }
    write_receipt("summary", summary)
    print(json.dumps(redact(summary), indent=2))


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("probe", help="health + authenticated buyer surfaces")
    sub.add_parser("buyer", help="quote/submit/idempotency attempt")
    sub.add_parser("supplier", help="worker token mint + register + poll")
    sub.add_parser("run", help="full rehearsal (writes evidence receipts)")
    return p


def main() -> None:
    args = build_parser().parse_args()
    if args.cmd == "probe":
        cmd_probe(args)
    elif args.cmd == "buyer":
        cmd_buyer(args)
    elif args.cmd == "supplier":
        cmd_supplier(args)
    elif args.cmd == "run":
        cmd_run(args)
    else:
        die(f"unknown command {args.cmd}")


if __name__ == "__main__":
    main()
