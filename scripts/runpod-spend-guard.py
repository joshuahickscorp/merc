#!/usr/bin/env python3
"""Money safety for a paid RunPod experiment: the cap, the clock, the receipt.

    # How long may this pod live before it has spent the cap?
    runpod-spend-guard.py budget --cost-per-hr 1.19 --cap-usd 2.00

    # What did the run actually cost, and is the receipt admissible?
    runpod-spend-guard.py receipt --pod-id abc --gpu 'NVIDIA A100' \\
        --image vllm/vllm-openai:v0.26.0 --cost-per-hr 1.19 --cap-usd 2.00 \\
        --started-at 1750000000 --stopped-at 1750001800 \\
        --teardown-verified true --out evidence/runpod/spend-<id>.json

    runpod-spend-guard.py --self-test

    # Re-check every retained receipt under evidence/runpod/ against today's rules.
    # A receipt written before a rule existed is not grandfathered: it fails the
    # build with the rule and path named, and the artifact is left untouched.
    runpod-spend-guard.py revalidate

    # Offline / fixture-driven orphan reconcile (no API, no spend):
    runpod-spend-guard.py reconcile \\
        --live-pods-json '[{"id":"p1","name":"merc-canary-vllm"}]' \\
        --intent-dir /tmp/intents --receipts-dir evidence/runpod

    # Intent markers (written before a pod exists, bound after create):
    runpod-spend-guard.py intent-write --request-id r1 --purpose experiment \\
        --gpu 'NVIDIA A40' --name merc-canary-vllm --intent-dir .merc-runpod/intent
    runpod-spend-guard.py intent-bind --request-id r1 --pod-id abc \\
        --intent-dir .merc-runpod/intent
    runpod-spend-guard.py intent-complete --request-id r1 --intent-dir .merc-runpod/intent

The arithmetic lives here rather than in the shell for one reason: it is the part
that decides how long real money is allowed to burn, and shell cannot be unit
tested without spending it. `--self-test` runs the cases offline.

The guard is deliberately pessimistic. It bounds the pod's LIFETIME, not its
useful work, because RunPod bills for the pod and not for what the pod achieved:
a run that hangs at 3% of the way through still costs the full wall clock.

Orphan reconcile exists because trap-based teardown cannot catch SIGKILL, host
loss, or death between create and trap arming. A live pod with no living owner
(or only a bound intent and no completion) is an orphan: report loudly; never
terminate unless the operator asks.
"""

from __future__ import annotations

import argparse
import ctypes
import hashlib
import json
import math
import os
import re
import secrets
import subprocess
import sys
import time
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field
from typing import Any, Iterable, Optional

import fcntl

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# A floor on the cap. Below this the experiment cannot even reach readiness — an
# image pull plus a model download is minutes of billed time — so a smaller cap
# would guarantee a teardown before anything was learned, which is a way of
# spending money for nothing rather than a way of saving it.
MIN_CAP_USD = 0.25

# The share of the cap the pod's lifetime may consume. The remainder is headroom
# for RunPod's own billing granularity and for the seconds between the kill
# decision and the pod actually stopping — teardown is not instantaneous, and a
# budget computed to the last cent overspends by exactly that delay.
LIFETIME_SHARE_OF_CAP = 0.80

# Intent markers live outside evidence/ so a crash mid-run does not leave a
# committed artifact claiming money was spent. Use the Git common directory so
# sibling worktrees of one clone contend for the same local account lease.
def _default_intent_dir() -> str:
    configured = os.environ.get("MERC_RUNPOD_INTENT_DIR")
    if configured:
        return configured
    try:
        common = subprocess.check_output(
            ["git", "-C", ROOT, "rev-parse", "--git-common-dir"],
            text=True,
            stderr=subprocess.DEVNULL,
        ).strip()
        if common:
            if not os.path.isabs(common):
                common = os.path.abspath(os.path.join(ROOT, common))
            return os.path.join(common, "merc-runpod", "intent")
    except (OSError, subprocess.CalledProcessError):
        pass
    return os.path.join(ROOT, ".merc-runpod", "intent")


DEFAULT_INTENT_DIR = _default_intent_dir()
DEFAULT_RECEIPTS_DIR = os.path.join(ROOT, "evidence", "runpod")

# Precise meaning of receipt field orphan_pods. Written into every fresh receipt
# so a reader cannot confuse "this run's post-teardown account list was empty"
# with "no pod will ever bill again".
ORPHAN_PODS_SCOPE = "account_after_this_run_teardown"
ORPHAN_PODS_MEANING = (
    "Pod IDs still listed on the RunPod account after this run tore down its own "
    "pod. Empty means none remained at receipt-write time. It does NOT mean the "
    "account is permanently clean, that a later SIGKILLed process left nothing "
    "behind, or that no operator-kept pod exists outside this run. A governed "
    "experiment never sweeps unrelated account pods; those remain visible to "
    "reconcile until provider-level idempotency or a durable account coordinator "
    "exists."
)


def budget_seconds(cost_per_hr: float, cap_usd: float) -> int:
    """Seconds the pod may live before its billed lifetime reaches the cap."""
    if not math.isfinite(cost_per_hr) or cost_per_hr <= 0:
        raise ValueError(f"cost per hour must be finite and positive, got {cost_per_hr!r}")
    if not math.isfinite(cap_usd) or cap_usd < MIN_CAP_USD:
        raise ValueError(
            f"cap must be at least ${MIN_CAP_USD:.2f}; ${cap_usd} cannot reach readiness"
        )
    return int((cap_usd * LIFETIME_SHARE_OF_CAP) / cost_per_hr * 3600)


def spend_usd(cost_per_hr: float, seconds: float) -> float:
    """What a pod that lived `seconds` cost, rounded UP to the cent.

    Up, not nearest: a receipt that rounds spend down reports less money leaving
    the account than left it.
    """
    if seconds < 0:
        raise ValueError(f"a pod cannot live for {seconds} seconds")
    return math.ceil(cost_per_hr * seconds / 3600 * 100) / 100


def receipt_rule_refusals(
    *,
    image: str,
    cost_per_hr: float,
    cap_usd: float,
    seconds: float,
    spent: float,
    teardown_verified: bool,
    ready: bool,
    orphans,
) -> list:
    """Today's refusal rules. Shared by fresh receipts and re-validation of stored ones."""
    refusals = []
    allowed = budget_seconds(cost_per_hr, cap_usd)
    if not teardown_verified:
        refusals.append(
            "teardown was not verified, so the pod may still be billing; a receipt "
            "cannot report a final cost for a pod that might still be running"
        )
    if not ready:
        refusals.append(
            "vLLM never reached a verified ready state, so this is a failed "
            "startup receipt rather than usable CUDA-runtime evidence"
        )
    if seconds > allowed:
        refusals.append(
            f"pod lived {seconds}s against a budget of {allowed}s: the lifetime bound "
            "did not hold, which is the only thing standing between a hung run and "
            "the whole balance"
        )
    if spent > cap_usd:
        refusals.append(f"spend ${spent:.2f} exceeded the cap ${cap_usd:.2f}")
    if "@sha256:" not in image:
        refusals.append(
            "image is not an immutable OCI digest, so the runtime this receipt "
            "describes cannot be identified again"
        )
    if orphans:
        refusals.append(
            "pods still listed on the account after this run's teardown "
            f"(orphan_pods scope={ORPHAN_PODS_SCOPE}): {orphans}"
        )
    return refusals


def build_receipt(args) -> dict:
    seconds = args.stopped_at - args.started_at
    if seconds < 0:
        raise ValueError(
            f"stopped_at {args.stopped_at} precedes started_at {args.started_at}"
        )
    allowed = budget_seconds(args.cost_per_hr, args.cap_usd)
    spent = spend_usd(args.cost_per_hr, seconds)
    refusals = receipt_rule_refusals(
        image=args.image,
        cost_per_hr=args.cost_per_hr,
        cap_usd=args.cap_usd,
        seconds=seconds,
        spent=spent,
        teardown_verified=bool(args.teardown_verified),
        ready=bool(args.ready),
        orphans=args.orphans,
    )

    return {
        "schema_version": 1,
        "kind": "runpod_spend_receipt",
        "pod_id": args.pod_id,
        "gpu": args.gpu,
        "image": args.image,
        "model": args.model,
        "cost_per_hr_usd": args.cost_per_hr,
        "cap_usd": args.cap_usd,
        "lifetime_budget_secs": allowed,
        "lifetime_actual_secs": seconds,
        "spend_usd": spent,
        "cap_headroom_usd": round(args.cap_usd - spent, 4),
        "started_at_unix": args.started_at,
        "stopped_at_unix": args.stopped_at,
        "teardown_verified": bool(args.teardown_verified),
        # Narrow meaning: remaining account pods at receipt write, not a permanent
        # "nothing is billing" guarantee. See orphan_pods_meaning.
        "orphan_pods": args.orphans,
        "orphan_pods_scope": ORPHAN_PODS_SCOPE,
        "orphan_pods_meaning": ORPHAN_PODS_MEANING,
        "ready": bool(args.ready),
        "admissible": not refusals,
        "refusals": refusals,
        "limitations": [
            "Spend is derived from RunPod's advertised cost per hour and the pod's "
            "observed lifetime, not from an invoice. It is what Merc believes it "
            "spent; the provider's own billing is the authority.",
            "Storage and network egress are not included.",
            ORPHAN_PODS_MEANING,
        ],
    }


def receipt_withdrawal(receipt: dict):
    """Return the withdrawal reason for a retained receipt, or None.

    A receipt written before a rule existed is not grandfathered -- it fails.
    The only honest exits are to re-take it under today's rules, or to WITHDRAW
    it with a stated reason, which is what the parity receipt does with
    validity: INVALIDATED_PENDING_RERUN.

    Withdrawal is not a softer pass. A withdrawn receipt may never back a claim
    again, which is strictly stronger than one that quietly satisfies a rule it
    predates. The reason is mandatory: a reasonless withdrawal is indistinguishable
    from deleting an inconvenient result, so it still fails.
    """
    validity = str(receipt.get("validity", "")).upper()
    if validity not in {"WITHDRAWN", "INVALIDATED", "INVALIDATED_PENDING_RERUN"}:
        return None
    reason = receipt.get("withdrawn_reason") or receipt.get("superseded_reason")
    if isinstance(reason, list):
        reason = "; ".join(str(r) for r in reason if str(r).strip())
    reason = str(reason or "").strip()
    return reason or None


def revalidate_stored_receipt(path: str, receipt: dict) -> list:
    """Re-apply today's rules to a retained receipt. Does not rewrite the file."""
    if receipt.get("kind") != "runpod_spend_receipt":
        return [f"not a runpod_spend_receipt (kind={receipt.get('kind')!r})"]
    validity = str(receipt.get("validity", "")).upper()
    if validity in {"WITHDRAWN", "INVALIDATED", "INVALIDATED_PENDING_RERUN"}:
        if not receipt_withdrawal(receipt):
            return [
                "withdrawn without a stated reason: set withdrawn_reason, or "
                "re-take the receipt under today's rules"
            ]
        return []
    try:
        cost = float(receipt["cost_per_hr_usd"])
        cap = float(receipt["cap_usd"])
        seconds = float(receipt["lifetime_actual_secs"])
        spent = spend_usd(cost, seconds)
    except (KeyError, TypeError, ValueError) as exc:
        return [f"receipt fields unreadable under today's rules: {exc}"]
    return receipt_rule_refusals(
        image=str(receipt.get("image") or ""),
        cost_per_hr=cost,
        cap_usd=cap,
        seconds=seconds,
        spent=spent,
        teardown_verified=bool(receipt.get("teardown_verified")),
        ready=bool(receipt.get("ready")),
        orphans=list(receipt.get("orphan_pods") or []),
    )


def revalidate_retained_receipts() -> int:
    """Walk evidence/runpod/ and fail any retained receipt that fails today's rules."""
    root = os.path.join(ROOT, "evidence", "runpod")
    if not os.path.isdir(root):
        print("runpod-spend-guard revalidate: no evidence/runpod/ directory", file=sys.stderr)
        return 0
    paths = sorted(
        os.path.join(root, name)
        for name in os.listdir(root)
        if name.endswith(".json") and name.startswith("spend-")
    )
    if not paths:
        print("runpod-spend-guard revalidate: no spend-*.json receipts retained")
        return 0
    failed = 0
    withdrawn_count = 0
    for path in paths:
        rel = os.path.relpath(path, ROOT)
        try:
            with open(path, encoding="utf-8") as handle:
                receipt = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            print(f"FAIL {rel}: cannot read receipt: {exc}", file=sys.stderr)
            failed += 1
            continue
        refusals = revalidate_stored_receipt(path, receipt)
        if refusals:
            failed += 1
            for reason in refusals:
                # Name the rule and the receipt; leave the artifact untouched.
                print(f"FAIL {rel}: {reason}", file=sys.stderr)
        elif (withdrawn := receipt_withdrawal(receipt)) is not None:
            # Never print PASS for a withdrawn receipt. It did not satisfy the
            # rules; it was retired from evidence, and a reader skimming for
            # green must not mistake one for the other.
            withdrawn_count += 1
            print(f"WITHDRAWN {rel}: {withdrawn}")
        else:
            print(f"PASS {rel}")
    if failed:
        print(
            f"runpod-spend-guard revalidate: {failed}/{len(paths)} receipt(s) fail today's rules",
            file=sys.stderr,
        )
        return 1
    passing = len(paths) - withdrawn_count
    if withdrawn_count:
        print(
            f"runpod-spend-guard revalidate: {passing} receipt(s) PASS, "
            f"{withdrawn_count} WITHDRAWN and citable by nothing"
        )
    else:
        print(f"runpod-spend-guard revalidate: {len(paths)} receipt(s) PASS")
    return 0


# ---------------------------------------------------------------------------
# Intent markers + orphan reconcile
# ---------------------------------------------------------------------------

INTENT_KIND = "runpod_pod_intent"
INTENT_STATUSES = frozenset({"requested", "bound", "completed"})
INTENT_SCHEMA_VERSION = 2
LOCK_KIND = "runpod_provisioning_lock"
DEFAULT_LEASE_TTL_SECONDS = 90
MAX_LEASE_TTL_SECONDS = 90
DEFAULT_OPERATOR_KEEP_TTL_SECONDS = 90
MAX_OPERATOR_KEEP_TTL_SECONDS = 300
# A lease from the future is not an ownership claim.  We intentionally have no
# clock-skew grace here: on a disagreement, reconcile reports an orphan rather
# than letting a future-dated record conceal a billable pod.
MAX_LEASE_CLOCK_SKEW_SECONDS = 0


def _atomic_write_json(path: str, value: dict) -> None:
    """Write a private JSON record atomically within its existing directory."""
    directory = os.path.dirname(path)
    os.makedirs(directory, mode=0o700, exist_ok=True)
    try:
        os.chmod(directory, 0o700)
    except OSError as exc:
        raise ValueError(f"cannot secure intent directory {directory}: {exc}") from exc
    tmp = f"{path}.tmp-{os.getpid()}-{secrets.token_hex(6)}"
    try:
        with open(tmp, "w", encoding="utf-8") as handle:
            json.dump(value, handle, indent=2, sort_keys=True)
            handle.write("\n")
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    finally:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass


def _identity_digest(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _token_digest(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def add_owner_token_argument(parser: argparse.ArgumentParser) -> None:
    """Accept a local lease token without requiring it in process argv.

    --owner-token remains available for explicitly invoked operator commands,
    while provisioner calls use a private inherited FD.  The two shapes are
    mutually exclusive so a caller cannot accidentally trust a different token
    from the one it intended to pass.
    """
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--owner-token")
    group.add_argument("--owner-token-fd", type=int)


def owner_token_from_args(args: argparse.Namespace) -> str:
    token = getattr(args, "owner_token", None)
    fd = getattr(args, "owner_token_fd", None)
    if fd is not None:
        if not isinstance(fd, int) or fd < 3:
            raise ValueError("owner-token-fd must be an inherited private file descriptor")
        try:
            raw = os.read(fd, 4097)
        except OSError as exc:
            raise ValueError(f"cannot read owner token from fd {fd}: {exc}") from exc
        if len(raw) > 4096:
            raise ValueError("owner token fd is too large")
        try:
            token = raw.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("owner token fd is not UTF-8") from exc
        token = token.rstrip("\n")
    if not isinstance(token, str) or not token or "\n" in token or "\r" in token:
        raise ValueError("owner token must be one non-empty line")
    return token


def _darwin_boot_identity() -> str:
    """Return a stable Darwin boot identity or refuse ownership.

    A hostname is not a boot identity: it survives a reboot and would let a
    stale lease masquerade as the process that created it.  Keep this helper
    separate so its failure mode is testable without pretending Linux is macOS.
    """
    result = subprocess.run(
        ["sysctl", "-n", "kern.boottime"],
        capture_output=True,
        text=True,
        check=False,
    )
    match = re.search(r"\bsec\s*=\s*(\d+)\s*,\s*usec\s*=\s*(\d+)\b", result.stdout or "")
    if result.returncode != 0 or match is None:
        raise ValueError("cannot obtain Darwin kernel boot identity")
    return f"{match.group(1)}:{match.group(2)}"


def process_identity(pid: int) -> dict[str, Any]:
    """Return a PID-reuse-safe, non-secret process identity.

    Linux exposes boot + start ticks in procfs. macOS lacks that equivalent, so
    use hashes of `ps lstart` and `kern.boottime`; both are compared again before
    a lease is accepted. A PID alone is never ownership proof.
    """
    if not isinstance(pid, int) or pid <= 0:
        raise ValueError("owner pid must be a positive integer")
    proc_stat = f"/proc/{pid}/stat"
    if os.path.exists(proc_stat):
        try:
            with open(proc_stat, encoding="utf-8") as handle:
                raw = handle.read().strip()
            # comm may contain spaces/parentheses; field 22 follows the final ')'.
            tail = raw.rsplit(")", 1)[1].split()
            start = tail[19]
            with open("/proc/sys/kernel/random/boot_id", encoding="utf-8") as handle:
                boot = handle.read().strip()
        except (OSError, IndexError) as exc:
            raise ValueError(f"cannot read process identity for pid {pid}: {exc}") from exc
    elif os.uname().sysname == "Darwin":
        # proc_bsdinfo is the documented libproc record. Its start timeval has
        # microsecond precision; ps lstart is only second-granularity and would
        # leave a PID-reuse hole on a busy host.
        class ProcBSDInfo(ctypes.Structure):
            _fields_ = [
                ("pbi_flags", ctypes.c_uint32),
                ("pbi_status", ctypes.c_uint32),
                ("pbi_xstatus", ctypes.c_uint32),
                ("pbi_pid", ctypes.c_uint32),
                ("pbi_ppid", ctypes.c_uint32),
                ("pbi_uid", ctypes.c_uint32),
                ("pbi_gid", ctypes.c_uint32),
                ("pbi_ruid", ctypes.c_uint32),
                ("pbi_rgid", ctypes.c_uint32),
                ("pbi_svuid", ctypes.c_uint32),
                ("pbi_svgid", ctypes.c_uint32),
                ("rfu_1", ctypes.c_uint32),
                ("pbi_comm", ctypes.c_char * 16),
                ("pbi_name", ctypes.c_char * 32),
                ("pbi_nfiles", ctypes.c_uint32),
                ("pbi_pgid", ctypes.c_uint32),
                ("pbi_pjobc", ctypes.c_uint32),
                ("e_tdev", ctypes.c_uint32),
                ("e_tpgid", ctypes.c_uint32),
                ("pbi_nice", ctypes.c_int32),
                ("pbi_start_tvsec", ctypes.c_uint64),
                ("pbi_start_tvusec", ctypes.c_uint64),
            ]

        info = ProcBSDInfo()
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        proc_pidinfo = libproc.proc_pidinfo
        proc_pidinfo.argtypes = [
            ctypes.c_int,
            ctypes.c_int,
            ctypes.c_uint64,
            ctypes.c_void_p,
            ctypes.c_int,
        ]
        proc_pidinfo.restype = ctypes.c_int
        # PROC_PIDTBSDINFO is 3. Refuse ownership if the API is unavailable or
        # returns a partial record rather than falling back to coarse ps output.
        received = proc_pidinfo(
            pid,
            3,
            0,
            ctypes.byref(info),
            ctypes.sizeof(info),
        )
        if received != ctypes.sizeof(info) or info.pbi_pid != pid:
            raise ValueError(f"cannot obtain precise Darwin process identity for pid {pid}")
        start = f"{info.pbi_start_tvsec}:{info.pbi_start_tvusec}"
        boot = _darwin_boot_identity()
    else:
        # There is no supported high-resolution process birth identity here.
        # Do not turn a best-effort PID into a billing ownership claim.
        raise ValueError(f"unsupported platform for lease ownership: {os.uname().sysname}")
    return {
        "pid": pid,
        "process_start": _identity_digest(start),
        "boot_id": _identity_digest(boot),
    }


def process_identity_matches(
    *, pid: Any, process_start: Any, boot_id: Any
) -> bool:
    try:
        expected = process_identity(int(pid))
    except (TypeError, ValueError):
        return False
    return (
        isinstance(process_start, str)
        and isinstance(boot_id, str)
        and secrets.compare_digest(expected["process_start"], process_start)
        and secrets.compare_digest(expected["boot_id"], boot_id)
    )


def _positive_int(value: Any, *, field_name: str) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{field_name} must be an integer") from exc
    if parsed <= 0:
        raise ValueError(f"{field_name} must be positive")
    return parsed


def _bounded_ttl(value: Any, *, field_name: str, maximum: int) -> int:
    ttl = _positive_int(value, field_name=field_name)
    if ttl > maximum:
        raise ValueError(f"{field_name} must be at most {maximum}s")
    return ttl


def _require_owner(
    *,
    token: str,
    owner_pid: Any,
    owner_process_start: str,
    owner_boot_id: str,
) -> dict[str, Any]:
    if not isinstance(token, str) or len(token) < 24:
        raise ValueError("owner token must be at least 24 characters")
    pid = _positive_int(owner_pid, field_name="owner pid")
    if not all(
        isinstance(value, str) and len(value) == 64
        and all(char in "0123456789abcdef" for char in value)
        for value in (owner_process_start, owner_boot_id)
    ):
        raise ValueError("owner process start and boot identity must be SHA-256 digests")
    if not process_identity_matches(
        pid=pid, process_start=owner_process_start, boot_id=owner_boot_id
    ):
        raise ValueError("owner PID/start/boot identity is not live")
    return {
        "token": token,
        "owner_pid": pid,
        "owner_process_start": owner_process_start,
        "owner_boot_id": owner_boot_id,
    }


def _lease(
    *,
    owner: dict[str, Any],
    request_id: str,
    pod_id: Optional[str],
    purpose: str,
    state: str,
    now: int,
    ttl_seconds: int,
) -> dict[str, Any]:
    return {
        "token": owner["token"],
        "request_id": request_id,
        "pod_id": pod_id,
        "purpose": purpose,
        "state": state,
        "owner_pid": owner["owner_pid"],
        "owner_process_start": owner["owner_process_start"],
        "owner_boot_id": owner["owner_boot_id"],
        "heartbeat_at_unix": now,
        "expires_at_unix": now + ttl_seconds,
    }


def _lease_valid(
    lease: Any,
    *,
    request_id: str,
    pod_id: Optional[str],
    allowed_states: set[str],
    now: int,
    require_living_owner: bool,
    maximum_ttl: int,
    expected_token_sha256: Optional[str] = None,
) -> tuple[bool, str]:
    if not isinstance(lease, dict):
        return False, "missing lease"
    if lease.get("request_id") != request_id:
        return False, "lease request id does not match intent"
    if lease.get("pod_id") != pod_id:
        return False, "lease pod id does not match intent"
    if lease.get("state") not in allowed_states:
        return False, "lease state is not accepted here"
    if not isinstance(lease.get("token"), str) or len(lease["token"]) < 24:
        return False, "lease token is missing or malformed"
    if expected_token_sha256 is not None:
        if (
            not isinstance(expected_token_sha256, str)
            or len(expected_token_sha256) != 64
            or not secrets.compare_digest(_token_digest(lease["token"]), expected_token_sha256)
        ):
            return False, "lease token does not match intent binding"
    try:
        heartbeat = int(lease["heartbeat_at_unix"])
        expires = int(lease["expires_at_unix"])
    except (KeyError, TypeError, ValueError):
        return False, "lease heartbeat or expiry is malformed"
    if expires <= heartbeat or expires - heartbeat > maximum_ttl:
        return False, "lease TTL exceeds the hard bound"
    if heartbeat > now + MAX_LEASE_CLOCK_SKEW_SECONDS:
        return False, "lease heartbeat is in the future"
    if expires > now + maximum_ttl + MAX_LEASE_CLOCK_SKEW_SECONDS:
        return False, "lease expiry is in the future"
    if now >= expires:
        return False, "lease heartbeat expired"
    if require_living_owner and not process_identity_matches(
        pid=lease.get("owner_pid"),
        process_start=lease.get("owner_process_start"),
        boot_id=lease.get("owner_boot_id"),
    ):
        return False, "lease owner PID/start/boot identity is not live"
    return True, ""


def _owner_matches(lease: Any, owner: dict[str, Any]) -> bool:
    if not isinstance(lease, dict):
        return False
    return (
        secrets.compare_digest(str(lease.get("token", "")), owner["token"])
        and lease.get("owner_pid") == owner["owner_pid"]
        and secrets.compare_digest(
            str(lease.get("owner_process_start", "")), owner["owner_process_start"]
        )
        and secrets.compare_digest(
            str(lease.get("owner_boot_id", "")), owner["owner_boot_id"]
        )
    )


def _intent_token_binding(intent: dict, *, request_id: str, pod_id: Optional[str]) -> Optional[str]:
    binding = intent.get("lease_binding")
    if not isinstance(binding, dict):
        return None
    digest = binding.get("token_sha256")
    if (
        binding.get("request_id") != request_id
        or binding.get("pod_id") != pod_id
        or not isinstance(digest, str)
        or len(digest) != 64
        or any(char not in "0123456789abcdef" for char in digest)
    ):
        return None
    return digest


def _lock_path(intent_dir: str) -> str:
    return os.path.join(os.path.dirname(os.path.abspath(intent_dir)), "provisioning.lock.json")


def _lock_guard_path(intent_dir: str) -> str:
    return os.path.join(os.path.dirname(os.path.abspath(intent_dir)), "provisioning.lock.guard")


@contextmanager
def _locked_intents(intent_dir: str):
    """Serialize local lock reclamation so two creators cannot both win."""
    os.makedirs(os.path.dirname(os.path.abspath(intent_dir)), mode=0o700, exist_ok=True)
    os.chmod(os.path.dirname(os.path.abspath(intent_dir)), 0o700)
    path = _lock_guard_path(intent_dir)
    with open(path, "a+", encoding="utf-8") as handle:
        os.chmod(path, 0o600)
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def _read_json(path: str) -> Optional[dict]:
    try:
        with open(path, encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, json.JSONDecodeError):
        return None
    return value if isinstance(value, dict) else None


def _load_lock(intent_dir: str) -> Optional[dict]:
    record = _read_json(_lock_path(intent_dir))
    return record if record and record.get("kind") == LOCK_KIND else None


def _remove_own_lock(*, intent_dir: str, request_id: str, token: str) -> None:
    path = _lock_path(intent_dir)
    lock = _load_lock(intent_dir)
    if lock and lock.get("request_id") == request_id and secrets.compare_digest(
        str(lock.get("token", "")), token
    ):
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass


def _live_bound_intent_reason(intent: dict, now: int) -> Optional[str]:
    """Return a reason when an intent still owns a local provisioning lane.

    The persistent create lock is a fast path, not the sole ownership record.
    If a process dies after atomically binding a pod but before rewriting the
    lock, the bound schema-v2 lease must still prevent another local creator
    from racing ahead after the old create lock expires.
    """
    if (
        intent.get("schema_version") != INTENT_SCHEMA_VERSION
        or intent.get("kind") != INTENT_KIND
        or intent.get("status") != "bound"
        or not isinstance(intent.get("pod_id"), str)
        or not intent.get("pod_id")
    ):
        return None
    request_id = str(intent.get("request_id") or "")
    pod_id = str(intent["pod_id"])
    binding = _intent_token_binding(intent, request_id=request_id, pod_id=pod_id)
    if binding is None:
        return None
    lease = intent.get("active_lease")
    state = str(lease.get("state") or "") if isinstance(lease, dict) else ""
    if state == "provisioning":
        valid, _ = _lease_valid(
            lease,
            request_id=request_id,
            pod_id=pod_id,
            allowed_states={"provisioning"},
            now=now,
            require_living_owner=True,
            maximum_ttl=MAX_LEASE_TTL_SECONDS,
            expected_token_sha256=binding,
        )
    elif state == "operator_keep":
        valid, _ = _lease_valid(
            lease,
            request_id=request_id,
            pod_id=pod_id,
            allowed_states={"operator_keep"},
            now=now,
            require_living_owner=False,
            maximum_ttl=MAX_OPERATOR_KEEP_TTL_SECONDS,
            expected_token_sha256=binding,
        )
    else:
        return None
    if valid:
        return f"bound intent {request_id} owns pod {pod_id}"
    return None


@dataclass
class PodClassification:
    pod_id: str
    name: str
    desired_status: str
    cost_per_hr: Any
    classification: str
    owner: str
    orphan: bool
    detail: str


@dataclass
class ReconcileReport:
    live: list[PodClassification] = field(default_factory=list)
    orphans: list[PodClassification] = field(default_factory=list)
    owned: list[PodClassification] = field(default_factory=list)
    stale_intents: list[dict] = field(default_factory=list)
    unbound_intents: list[dict] = field(default_factory=list)

    @property
    def has_orphans(self) -> bool:
        return bool(self.orphans)

    def to_dict(self) -> dict:
        return {
            "live": [asdict(p) for p in self.live],
            "orphans": [asdict(p) for p in self.orphans],
            "owned": [asdict(p) for p in self.owned],
            "stale_intents": self.stale_intents,
            "unbound_intents": self.unbound_intents,
            "has_orphans": self.has_orphans,
            "orphan_pod_ids": [p.pod_id for p in self.orphans],
        }


def _intent_path(intent_dir: str, request_id: str) -> str:
    safe = "".join(c if c.isalnum() or c in "-_" else "_" for c in request_id)
    if not safe:
        raise ValueError("request_id is empty after sanitising")
    return os.path.join(intent_dir, f"{safe}.json")


def write_intent(
    *,
    intent_dir: str,
    request_id: str,
    purpose: str,
    owner_token: str,
    owner_pid: int,
    owner_process_start: str,
    owner_boot_id: str,
    gpu: str = "",
    name: str = "",
    lease_ttl_seconds: int = DEFAULT_LEASE_TTL_SECONDS,
    now: Optional[int] = None,
) -> dict:
    """Acquire the account create lease and record intent before any pod exists."""
    os.makedirs(intent_dir, mode=0o700, exist_ok=True)
    try:
        os.chmod(intent_dir, 0o700)
    except OSError as exc:
        raise ValueError(f"cannot secure intent directory {intent_dir}: {exc}") from exc
    ts = int(time.time() if now is None else now)
    owner = _require_owner(
        token=owner_token,
        owner_pid=owner_pid,
        owner_process_start=owner_process_start,
        owner_boot_id=owner_boot_id,
    )
    ttl = _bounded_ttl(
        lease_ttl_seconds,
        field_name="provisioning lease TTL",
        maximum=MAX_LEASE_TTL_SECONDS,
    )
    if not request_id:
        raise ValueError("request_id is required")
    path = _intent_path(intent_dir, request_id)
    create_lease = _lease(
        owner=owner,
        request_id=request_id,
        pod_id=None,
        purpose=purpose,
        state="creating",
        now=ts,
        ttl_seconds=ttl,
    )
    record = {
        "schema_version": INTENT_SCHEMA_VERSION,
        "kind": INTENT_KIND,
        "request_id": request_id,
        "pod_id": None,
        "purpose": purpose,
        "gpu": gpu,
        "name": name,
        "status": "requested",
        "created_at_unix": ts,
        "pod_bound_at_unix": None,
        "completed_at_unix": None,
        "active_lease": None,
        "lease_binding": None,
        "terminal": None,
    }
    with _locked_intents(intent_dir):
        if os.path.exists(path):
            raise ValueError(f"intent already exists: {request_id}")
        for existing in load_intents(intent_dir):
            if reason := _live_bound_intent_reason(existing, ts):
                raise ValueError(f"another live process holds the account provisioning lease: {reason}")
        old_lock = _load_lock(intent_dir)
        if old_lock is not None:
            valid, _ = _lease_valid(
                old_lock,
                request_id=str(old_lock.get("request_id") or ""),
                pod_id=old_lock.get("pod_id"),
                allowed_states={"creating", "provisioning"},
                now=ts,
                require_living_owner=True,
                maximum_ttl=MAX_LEASE_TTL_SECONDS,
            )
            if valid:
                raise ValueError(
                    "another live process holds the account provisioning lease "
                    f"for request {old_lock.get('request_id')}"
                )
            try:
                os.unlink(_lock_path(intent_dir))
            except FileNotFoundError:
                pass
        lock = {"schema_version": 1, "kind": LOCK_KIND, **create_lease}
        _atomic_write_json(_lock_path(intent_dir), lock)
        try:
            _atomic_write_json(path, record)
        except Exception:
            _remove_own_lock(intent_dir=intent_dir, request_id=request_id, token=owner_token)
            raise
    return record


def bind_intent(
    *,
    intent_dir: str,
    request_id: str,
    pod_id: str,
    owner_token: str,
    owner_pid: int,
    owner_process_start: str,
    owner_boot_id: str,
    lease_ttl_seconds: int = DEFAULT_LEASE_TTL_SECONDS,
    now: Optional[int] = None,
) -> dict:
    """Atomically attach pod id and active provisioning lease to an intent."""
    if not pod_id:
        raise ValueError("pod_id is required to bind an intent")
    owner = _require_owner(
        token=owner_token,
        owner_pid=owner_pid,
        owner_process_start=owner_process_start,
        owner_boot_id=owner_boot_id,
    )
    ttl = _bounded_ttl(
        lease_ttl_seconds,
        field_name="provisioning lease TTL",
        maximum=MAX_LEASE_TTL_SECONDS,
    )
    ts = int(time.time() if now is None else now)
    path = _intent_path(intent_dir, request_id)
    with _locked_intents(intent_dir):
        record = _read_json(path)
        if not record or record.get("kind") != INTENT_KIND:
            raise ValueError(f"not an intent marker: {path}")
        if record.get("schema_version") != INTENT_SCHEMA_VERSION:
            raise ValueError("only a schema-v2 intent can bind a provider pod")
        if record.get("status") != "requested" or record.get("pod_id") is not None:
            raise ValueError(f"refusing to bind non-requested intent: {request_id}")
        lock = _load_lock(intent_dir)
        valid, reason = _lease_valid(
            lock,
            request_id=request_id,
            pod_id=None,
            allowed_states={"creating"},
            now=ts,
            require_living_owner=True,
            maximum_ttl=MAX_LEASE_TTL_SECONDS,
        )
        if not valid or not _owner_matches(lock, owner):
            raise ValueError(f"cannot bind without the live matching create lease: {reason}")
        lease = _lease(
            owner=owner,
            request_id=request_id,
            pod_id=pod_id,
            purpose=str(record.get("purpose") or "up"),
            state="provisioning",
            now=ts,
            ttl_seconds=ttl,
        )
        # This replacement is the ownership commit point: a reconciler sees pod
        # id and the matching lease together, or neither, never an unowned gap.
        record["pod_id"] = pod_id
        record["status"] = "bound"
        record["pod_bound_at_unix"] = ts
        record["active_lease"] = lease
        record["lease_binding"] = {
            "request_id": request_id,
            "pod_id": pod_id,
            "token_sha256": _token_digest(owner_token),
        }
        _atomic_write_json(path, record)
        _atomic_write_json(_lock_path(intent_dir), {"schema_version": 1, "kind": LOCK_KIND, **lease})
    return record


def renew_create_lease(
    *,
    intent_dir: str,
    request_id: str,
    owner_token: str,
    owner_pid: int,
    owner_process_start: str,
    owner_boot_id: str,
    lease_ttl_seconds: int = DEFAULT_LEASE_TTL_SECONDS,
    now: Optional[int] = None,
) -> dict:
    """Renew the pre-create account lease around a bounded provider POST."""
    owner = _require_owner(
        token=owner_token,
        owner_pid=owner_pid,
        owner_process_start=owner_process_start,
        owner_boot_id=owner_boot_id,
    )
    ttl = _bounded_ttl(
        lease_ttl_seconds,
        field_name="provisioning lease TTL",
        maximum=MAX_LEASE_TTL_SECONDS,
    )
    ts = int(time.time() if now is None else now)
    path = _intent_path(intent_dir, request_id)
    with _locked_intents(intent_dir):
        record = _read_json(path)
        if not record or record.get("kind") != INTENT_KIND:
            raise ValueError(f"not an intent marker: {path}")
        if record.get("schema_version") != INTENT_SCHEMA_VERSION:
            raise ValueError("only a schema-v2 intent can renew a create lease")
        if record.get("status") != "requested" or record.get("pod_id") is not None:
            raise ValueError("only an unbound requested intent can renew the create lease")
        lock = _load_lock(intent_dir)
        valid, reason = _lease_valid(
            lock,
            request_id=request_id,
            pod_id=None,
            allowed_states={"creating"},
            now=ts,
            require_living_owner=True,
            maximum_ttl=MAX_LEASE_TTL_SECONDS,
        )
        if not valid or not _owner_matches(lock, owner):
            raise ValueError(f"refusing create lease renewal: {reason}")
        lock["heartbeat_at_unix"] = ts
        lock["expires_at_unix"] = ts + ttl
        _atomic_write_json(_lock_path(intent_dir), lock)
    return record


def complete_intent(
    *,
    intent_dir: str,
    request_id: str,
    owner_token: str,
    now: Optional[int] = None,
) -> dict:
    """Write a terminal tombstone after verified teardown; never revive it."""
    if not isinstance(owner_token, str) or len(owner_token) < 24:
        raise ValueError("owner token must be at least 24 characters")
    ts = int(time.time() if now is None else now)
    path = _intent_path(intent_dir, request_id)
    with _locked_intents(intent_dir):
        record = _read_json(path)
        if not record or record.get("kind") != INTENT_KIND:
            raise ValueError(f"not an intent marker: {path}")
        if record.get("status") == "completed":
            raise ValueError(f"intent is already terminal: {request_id}")
        lease = record.get("active_lease")
        if not isinstance(lease, dict) or not secrets.compare_digest(
            str(lease.get("token", "")), owner_token
        ):
            raise ValueError("refusing terminal transition without matching lease token")
        binding = _intent_token_binding(
            record, request_id=request_id, pod_id=record.get("pod_id")
        )
        if binding is None or not secrets.compare_digest(_token_digest(owner_token), binding):
            raise ValueError("refusing terminal transition without matching intent binding")
        record["status"] = "completed"
        record["completed_at_unix"] = ts
        record["terminal"] = {
            "state": "teardown_verified",
            "request_id": request_id,
            "pod_id": record.get("pod_id"),
            "token": owner_token,
            "completed_at_unix": ts,
        }
        record["active_lease"] = None
        _atomic_write_json(path, record)
        _remove_own_lock(intent_dir=intent_dir, request_id=request_id, token=owner_token)
    return record


def renew_intent_lease(
    *,
    intent_dir: str,
    request_id: str,
    owner_token: str,
    owner_pid: int,
    owner_process_start: str,
    owner_boot_id: str,
    lease_ttl_seconds: int = DEFAULT_LEASE_TTL_SECONDS,
    now: Optional[int] = None,
) -> dict:
    """Renew a live foreground provisioning lease; expired leases never revive."""
    owner = _require_owner(
        token=owner_token,
        owner_pid=owner_pid,
        owner_process_start=owner_process_start,
        owner_boot_id=owner_boot_id,
    )
    ttl = _bounded_ttl(
        lease_ttl_seconds,
        field_name="provisioning lease TTL",
        maximum=MAX_LEASE_TTL_SECONDS,
    )
    ts = int(time.time() if now is None else now)
    path = _intent_path(intent_dir, request_id)
    with _locked_intents(intent_dir):
        record = _read_json(path)
        if not record or record.get("kind") != INTENT_KIND:
            raise ValueError(f"not an intent marker: {path}")
        if record.get("schema_version") != INTENT_SCHEMA_VERSION:
            raise ValueError("only a schema-v2 intent can renew a provisioning lease")
        if record.get("status") != "bound":
            raise ValueError("only a bound intent can renew a provisioning lease")
        lease = record.get("active_lease")
        binding = _intent_token_binding(
            record, request_id=request_id, pod_id=record.get("pod_id")
        )
        if binding is None:
            raise ValueError("refusing lease renewal without a schema-v2 token binding")
        valid, reason = _lease_valid(
            lease,
            request_id=request_id,
            pod_id=record.get("pod_id"),
            allowed_states={"provisioning"},
            now=ts,
            require_living_owner=True,
            maximum_ttl=MAX_LEASE_TTL_SECONDS,
            expected_token_sha256=binding,
        )
        if not valid or not _owner_matches(lease, owner):
            raise ValueError(f"refusing lease renewal: {reason}")
        lease["heartbeat_at_unix"] = ts
        lease["expires_at_unix"] = ts + ttl
        record["active_lease"] = lease
        _atomic_write_json(path, record)
        _atomic_write_json(_lock_path(intent_dir), {"schema_version": 1, "kind": LOCK_KIND, **lease})
    return record


def promote_operator_keep(
    *,
    intent_dir: str,
    request_id: str,
    owner_token: str,
    keep_seconds: int = DEFAULT_OPERATOR_KEEP_TTL_SECONDS,
    now: Optional[int] = None,
) -> dict:
    """Turn a ready standalone --keep into a short, explicit operator claim."""
    if not isinstance(owner_token, str) or len(owner_token) < 24:
        raise ValueError("owner token must be at least 24 characters")
    ttl = _bounded_ttl(
        keep_seconds,
        field_name="operator keep TTL",
        maximum=MAX_OPERATOR_KEEP_TTL_SECONDS,
    )
    ts = int(time.time() if now is None else now)
    path = _intent_path(intent_dir, request_id)
    with _locked_intents(intent_dir):
        record = _read_json(path)
        if not record or record.get("kind") != INTENT_KIND:
            raise ValueError(f"not an intent marker: {path}")
        if record.get("schema_version") != INTENT_SCHEMA_VERSION:
            raise ValueError("only a schema-v2 intent can promote an operator keep")
        lease = record.get("active_lease")
        binding = _intent_token_binding(
            record, request_id=request_id, pod_id=record.get("pod_id")
        )
        if binding is None:
            raise ValueError("refusing operator keep promotion without a schema-v2 token binding")
        valid, reason = _lease_valid(
            lease,
            request_id=request_id,
            pod_id=record.get("pod_id"),
            allowed_states={"provisioning"},
            now=ts,
            require_living_owner=True,
            maximum_ttl=MAX_LEASE_TTL_SECONDS,
            expected_token_sha256=binding,
        )
        if not valid or not secrets.compare_digest(str(lease.get("token", "")), owner_token):
            raise ValueError(f"refusing operator keep promotion: {reason}")
        lease["state"] = "operator_keep"
        lease["heartbeat_at_unix"] = ts
        lease["expires_at_unix"] = ts + ttl
        record["active_lease"] = lease
        _atomic_write_json(path, record)
        _remove_own_lock(intent_dir=intent_dir, request_id=request_id, token=owner_token)
    return record


def renew_operator_keep(
    *,
    intent_dir: str,
    request_id: str,
    owner_token: str,
    keep_seconds: int = DEFAULT_OPERATOR_KEEP_TTL_SECONDS,
    now: Optional[int] = None,
) -> dict:
    """Explicitly extend an unexpired operator keep; it cannot be revived later."""
    if not isinstance(owner_token, str) or len(owner_token) < 24:
        raise ValueError("owner token must be at least 24 characters")
    ttl = _bounded_ttl(
        keep_seconds,
        field_name="operator keep TTL",
        maximum=MAX_OPERATOR_KEEP_TTL_SECONDS,
    )
    ts = int(time.time() if now is None else now)
    path = _intent_path(intent_dir, request_id)
    with _locked_intents(intent_dir):
        record = _read_json(path)
        if not record or record.get("kind") != INTENT_KIND:
            raise ValueError(f"not an intent marker: {path}")
        if record.get("schema_version") != INTENT_SCHEMA_VERSION:
            raise ValueError("only a schema-v2 intent can renew an operator keep")
        if record.get("status") != "bound":
            raise ValueError("only a bound intent can renew an operator keep")
        lease = record.get("active_lease")
        binding = _intent_token_binding(
            record, request_id=request_id, pod_id=record.get("pod_id")
        )
        if binding is None:
            raise ValueError("refusing operator keep renewal without a schema-v2 token binding")
        valid, reason = _lease_valid(
            lease,
            request_id=request_id,
            pod_id=record.get("pod_id"),
            allowed_states={"operator_keep"},
            now=ts,
            require_living_owner=False,
            maximum_ttl=MAX_OPERATOR_KEEP_TTL_SECONDS,
            expected_token_sha256=binding,
        )
        if not valid or not secrets.compare_digest(str(lease.get("token", "")), owner_token):
            raise ValueError(f"refusing operator keep renewal: {reason}")
        lease["heartbeat_at_unix"] = ts
        lease["expires_at_unix"] = ts + ttl
        record["active_lease"] = lease
        _atomic_write_json(path, record)
    return record


def load_intents(intent_dir: str) -> list[dict]:
    if not intent_dir or not os.path.isdir(intent_dir):
        return []
    out = []
    for name in sorted(os.listdir(intent_dir)):
        if not name.endswith(".json"):
            continue
        path = os.path.join(intent_dir, name)
        try:
            with open(path, encoding="utf-8") as handle:
                record = json.load(handle)
        except (OSError, json.JSONDecodeError):
            continue
        if record.get("kind") != INTENT_KIND:
            continue
        record["_path"] = path
        out.append(record)
    return out


def load_completed_pod_ids(receipts_dir: str) -> dict[str, dict]:
    """Map pod_id -> spend receipt for completed runs (teardown claimed or not)."""
    if not receipts_dir or not os.path.isdir(receipts_dir):
        return {}
    by_id: dict[str, dict] = {}
    for name in os.listdir(receipts_dir):
        if not (name.startswith("spend-") and name.endswith(".json")):
            continue
        path = os.path.join(receipts_dir, name)
        try:
            with open(path, encoding="utf-8") as handle:
                receipt = json.load(handle)
        except (OSError, json.JSONDecodeError):
            continue
        if receipt.get("kind") != "runpod_spend_receipt":
            continue
        pod_id = receipt.get("pod_id")
        if pod_id:
            by_id[str(pod_id)] = receipt
    return by_id


def classify_live_pods(
    live_pods: Iterable[dict],
    *,
    intents: Iterable[dict],
    active_pod_id: Optional[str] = None,
    completed_by_id: Optional[dict[str, dict]] = None,
    now: Optional[int] = None,
) -> ReconcileReport:
    """Reconcile live provider pods against local ownership trails.

    A live pod is an orphan unless a *living* owner claims it:
      - a fresh, PID/start/boot-validated provisioning lease in the matching intent;
      - a short explicit operator_keep lease made after standalone --keep readiness.

    `.merc-runpod.env`, pending files, and the deprecated active_pod_id argument
    are deliberately ignored: they can survive SIGKILL. Bound intents without an
    accepted lease are evidence of death, not owners. Completed/teardown-claimed
    records are never owners.
    """
    del active_pod_id
    ts = int(time.time() if now is None else now)
    completed_by_id = completed_by_id or {}
    intents = list(intents)
    bound_by_pod: dict[str, list[dict]] = {}
    terminal_by_pod: dict[str, list[dict]] = {}
    unbound: list[dict] = []
    for intent in intents:
        status = intent.get("status")
        pod_id = intent.get("pod_id")
        if status == "completed":
            if pod_id:
                terminal_by_pod.setdefault(str(pod_id), []).append(intent)
            continue
        if status == "requested" and not pod_id:
            unbound.append(intent)
            continue
        if pod_id:
            bound_by_pod.setdefault(str(pod_id), []).append(intent)

    report = ReconcileReport()
    live_ids: set[str] = set()
    for raw in live_pods:
        pod_id = str(raw.get("id") or raw.get("pod_id") or "")
        if not pod_id:
            continue
        live_ids.add(pod_id)
        name = str(raw.get("name") or "")
        desired = str(raw.get("desiredStatus") or raw.get("desired_status") or "")
        cost = raw.get("costPerHr", raw.get("cost_per_hr"))

        receipt = completed_by_id.get(pod_id)
        candidates = bound_by_pod.get(pod_id, [])
        terminals = terminal_by_pod.get(pod_id, [])

        # A receipt or terminal marker that claims teardown always wins over an
        # ownership claim: a still-live provider pod is billing.
        if receipt is not None and receipt.get("teardown_verified"):
            row = PodClassification(
                pod_id=pod_id,
                name=name,
                desired_status=desired,
                cost_per_hr=cost,
                classification="receipted_but_alive",
                owner=f"spend receipt for {pod_id} claims teardown_verified",
                orphan=True,
                detail=(
                    "a spend receipt claims this pod was torn down, but it is still "
                    "listed live — teardown verification failed or the pod returned"
                ),
            )
            report.live.append(row)
            report.orphans.append(row)
            continue

        if terminals:
            row = PodClassification(
                pod_id=pod_id,
                name=name,
                desired_status=desired,
                cost_per_hr=cost,
                classification="terminal_intent_alive",
                owner=(
                    f"terminal intent(s): {', '.join(str(i.get('request_id')) for i in terminals)}"
                ),
                orphan=True,
                detail=(
                    "a local intent reached a terminal teardown state, but the provider "
                    "still lists the pod live; an old heartbeat cannot revive it"
                ),
            )
            report.live.append(row)
            report.orphans.append(row)
            continue

        if receipt is not None and not receipt.get("teardown_verified"):
            row = PodClassification(
                pod_id=pod_id,
                name=name,
                desired_status=desired,
                cost_per_hr=cost,
                classification="receipted_unverified_alive",
                owner=f"spend receipt for {pod_id} (teardown not verified)",
                orphan=True,
                detail=(
                    "a spend receipt exists but did not verify teardown, and the pod "
                    "is still live"
                ),
            )
            report.live.append(row)
            report.orphans.append(row)
            continue

        if len(candidates) > 1:
            row = PodClassification(
                pod_id=pod_id,
                name=name,
                desired_status=desired,
                cost_per_hr=cost,
                classification="conflicting_open_intents",
                owner=(
                    f"open intents: {', '.join(str(i.get('request_id')) for i in candidates)}"
                ),
                orphan=True,
                detail="multiple open intents claim one pod; no single lease is trusted",
            )
            report.live.append(row)
            report.orphans.append(row)
            continue

        if candidates:
            intent = candidates[0]
            lease = intent.get("active_lease")
            state = lease.get("state") if isinstance(lease, dict) else None
            request_id = str(intent.get("request_id") or "")
            binding = _intent_token_binding(intent, request_id=request_id, pod_id=pod_id)
            if intent.get("schema_version") != INTENT_SCHEMA_VERSION:
                reason = "intent is not schema v2"
            elif binding is None:
                reason = "intent lease binding is missing or mismatched"
            elif state == "provisioning":
                valid, reason = _lease_valid(
                    lease,
                    request_id=request_id,
                    pod_id=pod_id,
                    allowed_states={"provisioning"},
                    now=ts,
                    require_living_owner=True,
                    maximum_ttl=MAX_LEASE_TTL_SECONDS,
                    expected_token_sha256=binding,
                )
                if valid:
                    row = PodClassification(
                        pod_id=pod_id,
                        name=name,
                        desired_status=desired,
                        cost_per_hr=cost,
                        classification="active_provisioning_lease",
                        owner=(
                            f"intent {intent.get('request_id')} foreground pid "
                            f"{lease.get('owner_pid')}"
                        ),
                        orphan=False,
                        detail=(
                            "fresh matching provisioning lease; owned but not yet "
                            "declared routable"
                        ),
                    )
                    report.live.append(row)
                    report.owned.append(row)
                    continue
            elif state == "operator_keep":
                valid, reason = _lease_valid(
                    lease,
                    request_id=request_id,
                    pod_id=pod_id,
                    allowed_states={"operator_keep"},
                    now=ts,
                    require_living_owner=False,
                    maximum_ttl=MAX_OPERATOR_KEEP_TTL_SECONDS,
                    expected_token_sha256=binding,
                )
                if valid:
                    row = PodClassification(
                        pod_id=pod_id,
                        name=name,
                        desired_status=desired,
                        cost_per_hr=cost,
                        classification="operator_keep",
                        owner=f"explicit keep intent {intent.get('request_id')}",
                        orphan=False,
                        detail=(
                            "short operator_keep lease is current; renew it explicitly "
                            "before expiry or it becomes an orphan"
                        ),
                    )
                    report.live.append(row)
                    report.owned.append(row)
                    continue
            else:
                reason = "missing active lease"

            row = PodClassification(
                pod_id=pod_id,
                name=name,
                desired_status=desired,
                cost_per_hr=cost,
                classification="abandoned_intent",
                owner=f"intent {intent.get('request_id')} status={intent.get('status')}",
                orphan=True,
                detail=(
                    "pod is bound to an open intent without a valid living lease "
                    f"({reason}); likely a killed or stalled run"
                ),
            )
            report.live.append(row)
            report.orphans.append(row)
            continue

        row = PodClassification(
            pod_id=pod_id,
            name=name,
            desired_status=desired,
            cost_per_hr=cost,
            classification="unknown",
            owner="none",
            orphan=True,
            detail=(
                "live pod with no accepted lease, bound open intent, or spend receipt "
                "— unrecognised billing"
            ),
        )
        report.live.append(row)
        report.orphans.append(row)

    # Bound intents whose pod is gone: stale trail, not currently billing.
    for pod_id, candidates in bound_by_pod.items():
        if pod_id not in live_ids:
            for intent in candidates:
                report.stale_intents.append(
                    {
                        "request_id": intent.get("request_id"),
                        "pod_id": pod_id,
                        "status": intent.get("status"),
                        "detail": "intent still open but pod is not live",
                    }
                )
    report.unbound_intents = [
        {
            "request_id": i.get("request_id"),
            "status": i.get("status"),
            "detail": "create intent never bound to a pod id (create may have failed "
            "before bind, or death between create response and bind)",
        }
        for i in unbound
    ]
    return report


def format_reconcile_human(report: ReconcileReport) -> str:
    lines = []
    if not report.live:
        lines.append("reconcile: no live pods")
    else:
        lines.append(f"reconcile: {len(report.live)} live pod(s)")
        for p in report.live:
            flag = "ORPHAN" if p.orphan else "owned"
            rate = f"${p.cost_per_hr}/hr" if p.cost_per_hr is not None else "?/hr"
            lines.append(
                f"  [{flag}] {p.pod_id}  {p.name or '-'}  {p.desired_status or '-'}  "
                f"{rate}  class={p.classification}"
            )
            lines.append(f"           owner: {p.owner}")
            lines.append(f"           {p.detail}")
    if report.orphans:
        lines.append(
            f"reconcile: {len(report.orphans)} ORPHAN pod(s) billing with no living owner"
        )
        lines.append(
            "  refuse quiet success. Terminate only with an explicit operator flag "
            "(e.g. MERC_RUNPOD_TERMINATE_ORPHANS=1 or: runpod-vllm.sh reconcile --terminate-orphans)."
        )
    for stale in report.stale_intents:
        lines.append(
            f"  stale intent: request={stale.get('request_id')} pod={stale.get('pod_id')} "
            f"({stale.get('detail')})"
        )
    for unbound in report.unbound_intents:
        lines.append(
            f"  unbound intent: request={unbound.get('request_id')} ({unbound.get('detail')})"
        )
    return "\n".join(lines)


def run_reconcile_from_args(args) -> int:
    if args.live_pods_json:
        live = json.loads(args.live_pods_json)
    elif args.live_pods_file:
        with open(args.live_pods_file, encoding="utf-8") as handle:
            live = json.load(handle)
    else:
        print(
            "reconcile requires --live-pods-json or --live-pods-file "
            "(shell fetches the API list; this command never calls RunPod)",
            file=sys.stderr,
        )
        return 2
    if not isinstance(live, list):
        print("live pods payload must be a JSON array", file=sys.stderr)
        return 2

    intent_dir = args.intent_dir or DEFAULT_INTENT_DIR
    receipts_dir = args.receipts_dir or DEFAULT_RECEIPTS_DIR
    intents = load_intents(intent_dir)
    completed = load_completed_pod_ids(receipts_dir)
    report = classify_live_pods(
        live,
        intents=intents,
        completed_by_id=completed,
    )
    if args.json:
        print(json.dumps(report.to_dict(), indent=2, sort_keys=True))
    else:
        print(format_reconcile_human(report))
    # Non-zero when any live pod has no living owner. Stale/unbound intents alone
    # do not fail the command — they do not bill.
    return 1 if report.has_orphans else 0


def self_test() -> int:
    # A cap buys a bounded lifetime, and the bound leaves headroom for teardown.
    assert budget_seconds(1.19, 2.00) == int(2.00 * 0.80 / 1.19 * 3600)
    assert budget_seconds(1.19, 2.00) < int(2.00 / 1.19 * 3600), "no teardown headroom"

    # A cap too small to reach readiness is refused rather than silently accepted.
    for bad in (0.0, 0.10, -1, float("nan")):
        try:
            budget_seconds(1.19, bad)
        except ValueError:
            pass
        else:
            raise AssertionError(f"cap {bad} was accepted")
    for bad in (0.0, -1, float("inf")):
        try:
            budget_seconds(bad, 2.00)
        except ValueError:
            pass
        else:
            raise AssertionError(f"cost per hour {bad} was accepted")

    # Spend rounds UP: 1 second on a $1.19/hr pod is a cent, not zero.
    assert spend_usd(1.19, 1) == 0.01, spend_usd(1.19, 1)
    assert spend_usd(1.19, 3600) == 1.19
    assert spend_usd(0.44, 1800) == 0.22

    class A:
        pod_id, gpu, model = "pod", "NVIDIA A100", "Qwen/Qwen2.5-1.5B-Instruct"
        image = "vllm/vllm-openai@sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0"
        cost_per_hr, cap_usd = 1.19, 2.00
        started_at, stopped_at = 0, 600
        teardown_verified, ready, orphans = True, True, []

    ok = build_receipt(A())
    assert ok["admissible"], ok["refusals"]
    assert ok["spend_usd"] == spend_usd(1.19, 600)
    assert ok["orphan_pods_scope"] == ORPHAN_PODS_SCOPE
    assert ok["orphan_pods_meaning"] == ORPHAN_PODS_MEANING
    assert ORPHAN_PODS_MEANING in ok["limitations"]

    # Each refusal fires on its own.
    class NoTeardown(A):
        teardown_verified = False

    assert not build_receipt(NoTeardown())["admissible"]

    class NotReady(A):
        ready = False

    assert not build_receipt(NotReady())["admissible"]

    class Overran(A):
        stopped_at = 99999

    over = build_receipt(Overran())
    assert not over["admissible"]
    assert any("lifetime bound did not hold" in r for r in over["refusals"]), over

    class Floating(A):
        image = "vllm/vllm-openai:v0.26.0"

    assert not build_receipt(Floating())["admissible"]

    class Orphaned(A):
        orphans = ["other-pod"]

    orphaned = build_receipt(Orphaned())
    assert not orphaned["admissible"]
    assert any(ORPHAN_PODS_SCOPE in r for r in orphaned["refusals"]), orphaned

    class Reversed(A):
        started_at, stopped_at = 600, 0

    try:
        build_receipt(Reversed())
    except ValueError:
        pass
    else:
        raise AssertionError("a receipt accepted a stop before its start")

    # --- Lease-backed orphan reconcile (all offline) ---
    import tempfile

    pod = "lnk2yta98ciwqv"
    live = [{"id": pod, "name": "merc-canary-vllm", "desiredStatus": "RUNNING"}]
    identity = process_identity(os.getpid())
    owner = {
        "owner_token": "a" * 48,
        "owner_pid": identity["pid"],
        "owner_process_start": identity["process_start"],
        "owner_boot_id": identity["boot_id"],
    }
    now = int(time.time())
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        write_intent(
            intent_dir=intent_dir,
            request_id="r-roundtrip",
            purpose="self-test",
            gpu="NVIDIA A40",
            name="merc-canary-vllm",
            now=now,
            **owner,
        )
        bind_intent(
            intent_dir=intent_dir,
            request_id="r-roundtrip",
            pod_id=pod,
            now=now + 1,
            **owner,
        )
        report = classify_live_pods(live, intents=load_intents(intent_dir), completed_by_id={}, now=now + 2)
        assert not report.has_orphans
        assert report.owned[0].classification == "active_provisioning_lease"
        promote_operator_keep(
            intent_dir=intent_dir,
            request_id="r-roundtrip",
            owner_token=owner["owner_token"],
            now=now + 3,
        )
        kept = classify_live_pods(live, intents=load_intents(intent_dir), completed_by_id={}, now=now + 4)
        assert not kept.has_orphans and kept.owned[0].classification == "operator_keep"
        complete_intent(
            intent_dir=intent_dir,
            request_id="r-roundtrip",
            owner_token=owner["owner_token"],
            now=now + 5,
        )
        terminal = classify_live_pods(live, intents=load_intents(intent_dir), completed_by_id={}, now=now + 6)
        assert terminal.has_orphans and terminal.orphans[0].classification == "terminal_intent_alive"

    print("runpod-spend-guard self-test: PASS")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    sub = parser.add_subparsers(dest="command")

    b = sub.add_parser("budget")
    b.add_argument("--cost-per-hr", type=float, required=True)
    b.add_argument("--cap-usd", type=float, required=True)

    r = sub.add_parser("receipt")
    r.add_argument("--pod-id", required=True)
    r.add_argument("--gpu", required=True)
    r.add_argument("--image", required=True)
    r.add_argument("--model", default="")
    r.add_argument("--cost-per-hr", type=float, required=True)
    r.add_argument("--cap-usd", type=float, required=True)
    r.add_argument("--started-at", type=int, required=True)
    r.add_argument("--stopped-at", type=int, required=True)
    r.add_argument("--teardown-verified", default="false")
    r.add_argument("--ready", default="false")
    r.add_argument("--orphans", default="")
    r.add_argument("--out", default="")

    sub.add_parser(
        "revalidate",
        help="re-check every retained evidence/runpod/spend-*.json under today's rules",
    )

    rec = sub.add_parser(
        "reconcile",
        help=(
            "classify live pods against local leases, intents, and receipts; "
            "exit 1 if any orphan (fixture-driven; no RunPod API calls)"
        ),
    )
    rec.add_argument("--live-pods-json", default="")
    rec.add_argument("--live-pods-file", default="")
    rec.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)
    rec.add_argument("--receipts-dir", default=DEFAULT_RECEIPTS_DIR)
    rec.add_argument(
        "--active-pod-id",
        default="",
        help="deprecated and ignored; .merc-runpod.env is not ownership proof",
    )
    rec.add_argument("--json", action="store_true")

    iw = sub.add_parser("intent-write", help="record create intent before the pod exists")
    iw.add_argument("--request-id", required=True)
    iw.add_argument("--purpose", required=True)
    iw.add_argument("--gpu", default="")
    iw.add_argument("--name", default="")
    add_owner_token_argument(iw)
    iw.add_argument("--owner-pid", type=int, required=True)
    iw.add_argument("--owner-process-start", required=True)
    iw.add_argument("--owner-boot-id", required=True)
    iw.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    ib = sub.add_parser(
        "intent-bind", help="atomically attach pod id and provisioning lease"
    )
    ib.add_argument("--request-id", required=True)
    ib.add_argument("--pod-id", required=True)
    add_owner_token_argument(ib)
    ib.add_argument("--owner-pid", type=int, required=True)
    ib.add_argument("--owner-process-start", required=True)
    ib.add_argument("--owner-boot-id", required=True)
    ib.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    icr = sub.add_parser(
        "intent-renew-create", help="renew an unbound pre-create account lease"
    )
    icr.add_argument("--request-id", required=True)
    add_owner_token_argument(icr)
    icr.add_argument("--owner-pid", type=int, required=True)
    icr.add_argument("--owner-process-start", required=True)
    icr.add_argument("--owner-boot-id", required=True)
    icr.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    ir = sub.add_parser("intent-renew", help="renew an unexpired foreground provisioning lease")
    ir.add_argument("--request-id", required=True)
    add_owner_token_argument(ir)
    ir.add_argument("--owner-pid", type=int, required=True)
    ir.add_argument("--owner-process-start", required=True)
    ir.add_argument("--owner-boot-id", required=True)
    ir.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    ik = sub.add_parser(
        "intent-promote-operator-keep",
        help="make a ready standalone --keep a short explicit operator lease",
    )
    ik.add_argument("--request-id", required=True)
    add_owner_token_argument(ik)
    ik.add_argument("--keep-seconds", type=int, default=DEFAULT_OPERATOR_KEEP_TTL_SECONDS)
    ik.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    ikr = sub.add_parser(
        "intent-renew-operator-keep",
        help="extend an unexpired explicit operator keep",
    )
    ikr.add_argument("--request-id", required=True)
    add_owner_token_argument(ikr)
    ikr.add_argument("--keep-seconds", type=int, default=DEFAULT_OPERATOR_KEEP_TTL_SECONDS)
    ikr.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    ic = sub.add_parser("intent-complete", help="write terminal teardown tombstone")
    ic.add_argument("--request-id", required=True)
    add_owner_token_argument(ic)
    ic.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    pi = sub.add_parser("process-identity", help="print PID/start/boot identity for a lease owner")
    pi.add_argument("--pid", type=int, required=True)

    args = parser.parse_args()
    if args.self_test:
        return self_test()
    if args.command in {
        "intent-write",
        "intent-bind",
        "intent-renew-create",
        "intent-renew",
        "intent-promote-operator-keep",
        "intent-renew-operator-keep",
        "intent-complete",
    }:
        try:
            args.owner_token = owner_token_from_args(args)
        except ValueError as exc:
            parser.error(str(exc))
    if args.command == "budget":
        print(budget_seconds(args.cost_per_hr, args.cap_usd))
        return 0
    if args.command == "receipt":
        args.teardown_verified = str(args.teardown_verified).lower() in ("1", "true", "yes")
        args.ready = str(args.ready).lower() in ("1", "true", "yes")
        args.orphans = [p for p in args.orphans.split(",") if p.strip()]
        receipt = build_receipt(args)
        if args.out:
            path = os.path.join(ROOT, args.out)
            _scripts = os.path.join(ROOT, "scripts")
            if _scripts not in sys.path:
                sys.path.insert(0, _scripts)
            from lib.evidence_binding import EvidenceBindingError, emit_bound_json
            # Bind producer identity through the single write path. Image digests
            # are lifted from the receipt's immutable image field so a BOUND
            # placement spend receipt can name what ran, not only that something
            # ran. Mutable tags never reach here admissible.
            image_digest = ""
            image_na = "no container image in this measurement"
            image = str(args.image or "")
            if "@sha256:" in image:
                digest_hex = image.rsplit("@sha256:", 1)[-1].strip().lower()
                if len(digest_hex) == 64 and all(c in "0123456789abcdef" for c in digest_hex):
                    image_digest = f"sha256:{digest_hex}"
                    image_na = ""
            model_na = (
                f"model field is a name/ref ({args.model}), not a weight digest; "
                "weight pins live on the placement contract / runtime authority"
                if args.model
                else "no model weights declared on this spend receipt"
            )
            try:
                emit_bound_json(
                    path,
                    receipt,
                    harness="scripts/runpod-spend-guard.py",
                    repo_root=ROOT,
                    build_binary_path=os.path.join(ROOT, "scripts", "runpod-spend-guard.py"),
                    exact_config=(
                        f"spend guard receipt: pod_id={args.pod_id} gpu={args.gpu} "
                        f"image={args.image} model={args.model} "
                        f"cap_usd={args.cap_usd} cost_per_hr={args.cost_per_hr}"
                    ),
                    raw_samples="spend fields embedded; no sample array",
                    image_digest=image_digest,
                    image_na=image_na or "no container image in this measurement",
                    model_na=model_na,
                )
            except EvidenceBindingError as exc:
                print(f"REFUSED evidence write: {exc}", file=sys.stderr)
                return 2
            print(f"spend receipt written to {args.out}", file=sys.stderr)
        else:
            print(json.dumps(receipt, indent=2))
        for refusal in receipt["refusals"]:
            print(f"REFUSED: {refusal}", file=sys.stderr)
        return 0 if receipt["admissible"] else 1
    if args.command == "revalidate":
        return revalidate_retained_receipts()
    if args.command == "reconcile":
        return run_reconcile_from_args(args)
    if args.command == "intent-write":
        record = write_intent(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            purpose=args.purpose,
            gpu=args.gpu,
            name=args.name,
            owner_token=args.owner_token,
            owner_pid=args.owner_pid,
            owner_process_start=args.owner_process_start,
            owner_boot_id=args.owner_boot_id,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-bind":
        record = bind_intent(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            pod_id=args.pod_id,
            owner_token=args.owner_token,
            owner_pid=args.owner_pid,
            owner_process_start=args.owner_process_start,
            owner_boot_id=args.owner_boot_id,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-renew-create":
        record = renew_create_lease(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            owner_token=args.owner_token,
            owner_pid=args.owner_pid,
            owner_process_start=args.owner_process_start,
            owner_boot_id=args.owner_boot_id,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-complete":
        record = complete_intent(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            owner_token=args.owner_token,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-renew":
        record = renew_intent_lease(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            owner_token=args.owner_token,
            owner_pid=args.owner_pid,
            owner_process_start=args.owner_process_start,
            owner_boot_id=args.owner_boot_id,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-promote-operator-keep":
        record = promote_operator_keep(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            owner_token=args.owner_token,
            keep_seconds=args.keep_seconds,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-renew-operator-keep":
        record = renew_operator_keep(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            owner_token=args.owner_token,
            keep_seconds=args.keep_seconds,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "process-identity":
        identity = process_identity(args.pid)
        print(f"{identity['pid']}|{identity['process_start']}|{identity['boot_id']}")
        return 0
    parser.print_help()
    return 2


if __name__ == "__main__":
    sys.exit(main())
