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

The arithmetic lives here rather than in the shell for one reason: it is the part
that decides how long real money is allowed to burn, and shell cannot be unit
tested without spending it. `--self-test` runs the cases offline.

The guard is deliberately pessimistic. It bounds the pod's LIFETIME, not its
useful work, because RunPod bills for the pod and not for what the pod achieved:
a run that hangs at 3% of the way through still costs the full wall clock.
"""

import argparse
import json
import math
import os
import sys

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
            f"pods left running that this experiment did not create: {orphans}"
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
        "orphan_pods": args.orphans,
        "ready": bool(args.ready),
        "admissible": not refusals,
        "refusals": refusals,
        "limitations": [
            "Spend is derived from RunPod's advertised cost per hour and the pod's "
            "observed lifetime, not from an invoice. It is what Merc believes it "
            "spent; the provider's own billing is the authority.",
            "Storage and network egress are not included.",
        ],
    }


def revalidate_stored_receipt(path: str, receipt: dict) -> list:
    """Re-apply today's rules to a retained receipt. Does not rewrite the file."""
    if receipt.get("kind") != "runpod_spend_receipt":
        return [f"not a runpod_spend_receipt (kind={receipt.get('kind')!r})"]
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
        else:
            print(f"PASS {rel}")
    if failed:
        print(
            f"runpod-spend-guard revalidate: {failed}/{len(paths)} receipt(s) fail today's rules",
            file=sys.stderr,
        )
        return 1
    print(f"runpod-spend-guard revalidate: {len(paths)} receipt(s) PASS")
    return 0


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

    assert not build_receipt(Orphaned())["admissible"]

    class Reversed(A):
        started_at, stopped_at = 600, 0

    try:
        build_receipt(Reversed())
    except ValueError:
        pass
    else:
        raise AssertionError("a receipt accepted a stop before its start")

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
            _scripts = os.path.join(ROOT, "scripts")
            if _scripts not in sys.path:
                sys.path.insert(0, _scripts)
            from lib.evidence_binding import EvidenceBindingError, emit_bound_json
            try:
                emit_bound_json(
                    path,
                    receipt,
                    harness="scripts/runpod-spend-guard.py",
                    repo_root=ROOT,
                    build_binary_path=os.path.join(ROOT, "scripts", "runpod-spend-guard.py"),
                    exact_config="spend guard args embedded in receipt",
                    raw_samples="spend fields embedded; no sample array",
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
    parser.print_help()
    return 2


if __name__ == "__main__":
    sys.exit(main())
