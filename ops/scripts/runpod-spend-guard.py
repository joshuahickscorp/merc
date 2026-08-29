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
import json
import math
import os
import sys
import time
from dataclasses import asdict, dataclass, field
from typing import Any, Iterable, Optional

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

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
# committed artifact claiming money was spent. Default relative to repo root.
DEFAULT_INTENT_DIR = os.path.join(ROOT, ".merc-runpod", "intent")
DEFAULT_RECEIPTS_DIR = os.path.join(ROOT, "evidence", "runpod")

# Precise meaning of receipt field orphan_pods. Written into every fresh receipt
# so a reader cannot confuse "this run's post-teardown account list was empty"
# with "no pod will ever bill again".
ORPHAN_PODS_SCOPE = "account_after_this_run_teardown"
ORPHAN_PODS_MEANING = (
    "Pod IDs still listed on the RunPod account after this run tore down its own "
    "pod (and, for governed experiment, after the exit sweep of every pod the "
    "process could see). Empty means none remained at receipt-write time. It does "
    "NOT mean the account is permanently clean, that a later SIGKILLed process "
    "left nothing behind, or that no operator-kept pod exists outside this run."
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
    gpu: str = "",
    name: str = "",
    now: Optional[int] = None,
) -> dict:
    """Record create *intent* before any billable pod exists."""
    os.makedirs(intent_dir, mode=0o700, exist_ok=True)
    ts = int(time.time() if now is None else now)
    record = {
        "schema_version": 1,
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
    }
    path = _intent_path(intent_dir, request_id)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(record, handle, indent=2, sort_keys=True)
        handle.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
    return record


def bind_intent(
    *,
    intent_dir: str,
    request_id: str,
    pod_id: str,
    now: Optional[int] = None,
) -> dict:
    """Attach the provider pod id as soon as create returns it."""
    if not pod_id:
        raise ValueError("pod_id is required to bind an intent")
    path = _intent_path(intent_dir, request_id)
    with open(path, encoding="utf-8") as handle:
        record = json.load(handle)
    if record.get("kind") != INTENT_KIND:
        raise ValueError(f"not an intent marker: {path}")
    if record.get("status") == "completed":
        raise ValueError(f"refusing to bind a completed intent: {request_id}")
    record["pod_id"] = pod_id
    record["status"] = "bound"
    record["pod_bound_at_unix"] = int(time.time() if now is None else now)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(record, handle, indent=2, sort_keys=True)
        handle.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
    return record


def complete_intent(
    *,
    intent_dir: str,
    request_id: str,
    now: Optional[int] = None,
) -> dict:
    """Mark intent completed after verified teardown + receipt."""
    path = _intent_path(intent_dir, request_id)
    with open(path, encoding="utf-8") as handle:
        record = json.load(handle)
    if record.get("kind") != INTENT_KIND:
        raise ValueError(f"not an intent marker: {path}")
    record["status"] = "completed"
    record["completed_at_unix"] = int(time.time() if now is None else now)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(record, handle, indent=2, sort_keys=True)
        handle.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
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
) -> ReconcileReport:
    """Reconcile live provider pods against local ownership trails.

    A live pod is an orphan unless a *living* owner claims it:
      - active_pod_id from .merc-runpod.env (operator --keep or in-progress ready run)

    Bound intents without completion are *evidence of death*, not living owners:
    a SIGKILLed run leaves exactly that trail. Completed receipts that claim
    teardown_verified while the pod is still live are also orphans.
    """
    completed_by_id = completed_by_id or {}
    intents = list(intents)
    bound_by_pod: dict[str, dict] = {}
    unbound: list[dict] = []
    for intent in intents:
        status = intent.get("status")
        pod_id = intent.get("pod_id")
        if status == "completed":
            continue
        if status == "requested" and not pod_id:
            unbound.append(intent)
            continue
        if pod_id:
            bound_by_pod[str(pod_id)] = intent

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

        if active_pod_id and pod_id == str(active_pod_id):
            row = PodClassification(
                pod_id=pod_id,
                name=name,
                desired_status=desired,
                cost_per_hr=cost,
                classification="active_owner",
                owner=f"active env pod {active_pod_id}",
                orphan=False,
                detail=(
                    "claimed by .merc-runpod.env / active process; not an orphan "
                    "(operator may have left it with --keep)"
                ),
            )
            report.live.append(row)
            report.owned.append(row)
            continue

        intent = bound_by_pod.get(pod_id)
        receipt = completed_by_id.get(pod_id)

        if intent is not None and (receipt is None or not receipt.get("teardown_verified")):
            # Bound intent, no verified completion: classic SIGKILL / host-death leak.
            row = PodClassification(
                pod_id=pod_id,
                name=name,
                desired_status=desired,
                cost_per_hr=cost,
                classification="abandoned_intent",
                owner=f"intent {intent.get('request_id')} status={intent.get('status')}",
                orphan=True,
                detail=(
                    "pod is bound to a local intent with no verified completion — "
                    "likely a killed run; billing with nobody watching"
                ),
            )
            report.live.append(row)
            report.orphans.append(row)
            continue

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

        row = PodClassification(
            pod_id=pod_id,
            name=name,
            desired_status=desired,
            cost_per_hr=cost,
            classification="unknown",
            owner="none",
            orphan=True,
            detail=(
                "live pod with no active env, no bound open intent, and no spend "
                "receipt — unrecognised billing"
            ),
        )
        report.live.append(row)
        report.orphans.append(row)

    # Bound intents whose pod is gone: stale trail, not currently billing.
    for pod_id, intent in bound_by_pod.items():
        if pod_id not in live_ids:
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
        active_pod_id=args.active_pod_id or None,
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

    # --- Orphan reconcile: killed-run simulation (must detect) ---
    # A board-power sampler was SIGKILLed during startup. Pending intent is bound
    # to the pod; no completion receipt exists. Reconcile must fail (exit 1).
    killed_pod = "lnk2yta98ciwqv"
    live = [
        {
            "id": killed_pod,
            "name": "merc-canary-vllm",
            "desiredStatus": "RUNNING",
            "costPerHr": 0.44,
        }
    ]
    intents = [
        {
            "schema_version": 1,
            "kind": INTENT_KIND,
            "request_id": "req-killed-sampler",
            "pod_id": killed_pod,
            "purpose": "board_power_remeasure",
            "status": "bound",
            "created_at_unix": 1,
            "pod_bound_at_unix": 2,
            "completed_at_unix": None,
        }
    ]
    killed = classify_live_pods(live, intents=intents, active_pod_id=None, completed_by_id={})
    assert killed.has_orphans, "killed run with pending intent must be an orphan"
    assert killed.orphans[0].pod_id == killed_pod
    assert killed.orphans[0].classification == "abandoned_intent"
    assert len(killed.owned) == 0

    # Same live pod with no trail at all (death before bind): still an orphan.
    unknown = classify_live_pods(
        live, intents=[], active_pod_id=None, completed_by_id={}
    )
    assert unknown.has_orphans
    assert unknown.orphans[0].classification == "unknown"

    # Operator deliberately kept a pod: active env owns it — not an orphan.
    kept = classify_live_pods(
        live, intents=[], active_pod_id=killed_pod, completed_by_id={}
    )
    assert not kept.has_orphans, "deliberate --keep must not be classified as orphan"
    assert kept.owned[0].classification == "active_owner"

    # Clean account.
    clean = classify_live_pods([], intents=intents, active_pod_id=None, completed_by_id={})
    assert not clean.has_orphans
    assert clean.stale_intents and clean.stale_intents[0]["pod_id"] == killed_pod

    # Receipt claimed teardown but pod still live.
    receipted = classify_live_pods(
        live,
        intents=[],
        active_pod_id=None,
        completed_by_id={
            killed_pod: {
                "kind": "runpod_spend_receipt",
                "pod_id": killed_pod,
                "teardown_verified": True,
            }
        },
    )
    assert receipted.has_orphans
    assert receipted.orphans[0].classification == "receipted_but_alive"

    # Intent write/bind/complete round-trip on a temp dir (no network).
    import tempfile

    with tempfile.TemporaryDirectory() as tmp:
        write_intent(
            intent_dir=tmp,
            request_id="r-roundtrip",
            purpose="self-test",
            gpu="NVIDIA A40",
            name="merc-canary-vllm",
            now=100,
        )
        loaded = load_intents(tmp)
        assert len(loaded) == 1 and loaded[0]["status"] == "requested"
        bind_intent(intent_dir=tmp, request_id="r-roundtrip", pod_id="podxyz", now=101)
        loaded = load_intents(tmp)
        assert loaded[0]["status"] == "bound" and loaded[0]["pod_id"] == "podxyz"
        # Killed-run fixture on disk: reconcile via CLI must exit 1.
        live_json = json.dumps(
            [{"id": "podxyz", "name": "merc-canary-vllm", "desiredStatus": "RUNNING"}]
        )
        # Inline call equivalent to CLI.
        report = classify_live_pods(
            json.loads(live_json),
            intents=load_intents(tmp),
            active_pod_id=None,
            completed_by_id={},
        )
        assert report.has_orphans
        complete_intent(intent_dir=tmp, request_id="r-roundtrip", now=102)
        loaded = load_intents(tmp)
        assert loaded[0]["status"] == "completed"
        # Completed intent is not a living owner; without active env the still-live
        # pod is unknown (or receipted if we had one). Here: unknown.
        after = classify_live_pods(
            json.loads(live_json),
            intents=load_intents(tmp),
            active_pod_id=None,
            completed_by_id={},
        )
        assert after.has_orphans
        assert after.orphans[0].classification == "unknown"

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
            "classify live pods against local intents/receipts/active env; "
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
        help="pod id claimed by .merc-runpod.env (deliberate keep / current run)",
    )
    rec.add_argument("--json", action="store_true")

    iw = sub.add_parser("intent-write", help="record create intent before the pod exists")
    iw.add_argument("--request-id", required=True)
    iw.add_argument("--purpose", required=True)
    iw.add_argument("--gpu", default="")
    iw.add_argument("--name", default="")
    iw.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    ib = sub.add_parser("intent-bind", help="attach pod id immediately after create returns")
    ib.add_argument("--request-id", required=True)
    ib.add_argument("--pod-id", required=True)
    ib.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    ic = sub.add_parser("intent-complete", help="mark intent completed after verified teardown")
    ic.add_argument("--request-id", required=True)
    ic.add_argument("--intent-dir", default=DEFAULT_INTENT_DIR)

    args = parser.parse_args()
    if args.self_test:
        return self_test()
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
            _scripts = os.path.join(ROOT, "ops/scripts")
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
                    harness="ops/scripts/runpod-spend-guard.py",
                    repo_root=ROOT,
                    build_binary_path=os.path.join(ROOT, "ops/scripts", "runpod-spend-guard.py"),
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
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-bind":
        record = bind_intent(
            intent_dir=args.intent_dir,
            request_id=args.request_id,
            pod_id=args.pod_id,
        )
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    if args.command == "intent-complete":
        record = complete_intent(intent_dir=args.intent_dir, request_id=args.request_id)
        print(json.dumps(record, indent=2, sort_keys=True))
        return 0
    parser.print_help()
    return 2


if __name__ == "__main__":
    sys.exit(main())
