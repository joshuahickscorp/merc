#!/usr/bin/env python3
"""Fail-closed readiness: domain scores are derived from machine receipts only.

Hand-typed `earned` fields in ops/readiness.json are advisory and ignored.
Where a named receipt is missing or fails its content check, that receipt
contributes zero points. Live money / public launch must stay NO_GO_PROHIBITED.

Machine-reachable ceiling: with every currently wired local receipt present
and passing, the derived total is 84/100. The offsite backup/restore pair
under evidence/external/offsite-*.json is a real provider copy of the live
droplet volumes (or an isolated rehearsal with the same schema) and
adds 3 when those content checks pass (87/100). The remaining 13 points
have receipt rows wired to other evidence/external/ paths; their content
checks refuse local or paper substitutes.
The 3-point qualifying soak stays on evidence/external/qualifying-soak-24h.json
(persistent staging + two Metal devices). Local deterministic coverage of
named time-dependent mechanisms is recorded at
evidence/autonomous/soak-requirement-derivation.json as a 0-point row; it
does not award those 3 points and must not claim a 24h wall-clock pass.
Operator steps for those points: docs/PROGRAMME.md § "Facet external action pack".
Do not loosen content checks to "make room".

Backend alpha is an additional decision axis, not a replacement. The 100-point
bar and the Level A/B/C decisions are derived exactly as before. A second
score is computed from receipts classified ALPHA_BLOCKER or ALPHA_CONTROL in
ops/backend-alpha-gates.json. Two claims are scored separately:
ALPHA_ENGINEERING_READY (synthetics permitted) and EXTERNAL_ALPHA_PROVEN
(synthetics structurally cannot satisfy). See docs/BACKEND_ALPHA_CONTRACT.md.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable, Iterable

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT / "scripts") not in sys.path:
    sys.path.insert(0, str(ROOT / "scripts"))
from lib.receipt_binding import bound_to, head_commit, receipt_commit  # noqa: E402

CANDIDATE_PATH = "ops/candidate.json"
# Filled by load_candidate() before any receipt is scored.
_CANDIDATE_COMMIT: str | None = None
_CANDIDATE_ORIGIN: str = ""
# HTTPS-observer sampling slack, matching scripts/soak/soak24.py.
_SOAK24_GAP_SLACK_S = 30

_SHA256 = re.compile(r"^[0-9a-f]{64}$")
_COMMIT = re.compile(r"^[0-9a-f]{40}$")
_BACKUP_ID = re.compile(r"^[0-9]{8}T[0-9]{6}Z$")
_CONTAINER_ID = re.compile(r"^[0-9a-f]{64}$")
_IMAGE_ID = re.compile(r"^sha256:[0-9a-f]{64}$")
_IMMUTABLE_IMAGE = re.compile(
    r"^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$"
)
_RFC3339_Z = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
_SECRET_SHAPED = re.compile(
    r"(sk_(?:test|live)_|rk_(?:test|live)_|pk_live_|whsec_|"
    r"AGE-SECRET-KEY-|AKIA[0-9A-Z]{12,})",
    re.IGNORECASE,
)
_PLACEHOLDER_TOKEN = re.compile(
    r"\b(example|placeholder|todo|tbd|n/?a|unknown|dummy|fake|sample|"
    r"lorem|test(?:er)?|self[- ]?approv|not executed|pending)\b",
    re.IGNORECASE,
)
_LOCAL_HOST_TOKEN = re.compile(
    r"(localhost|127\.0\.0\.1|0\.0\.0\.0|::1|example\.(com|org|net)|"
    r"invalid|harness|docker\.internal|nip\.io|sslip\.io|localtest)",
    re.IGNORECASE,
)


def fail(message: str) -> None:
    print(f"readiness: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def load_json(relative: str) -> Any:
    path = ROOT / relative
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def _git(args: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", "-C", str(ROOT), *args],
        capture_output=True,
        text=True,
        check=False,
    )


def load_candidate() -> str:
    """Declared candidate commit, or HEAD if ops/candidate.json is absent."""
    global _CANDIDATE_COMMIT, _CANDIDATE_ORIGIN
    path = ROOT / CANDIDATE_PATH
    if not path.is_file():
        commit = head_commit(str(ROOT)).strip().lower()
        if not _COMMIT.fullmatch(commit):
            fail("HEAD is not a 40-hex commit")
        _CANDIDATE_COMMIT = commit
        _CANDIDATE_ORIGIN = "ops/candidate.json absent; falling back to HEAD"
        return commit
    doc = load_json(CANDIDATE_PATH)
    if not isinstance(doc, dict):
        fail("ops/candidate.json is not an object")
    if doc.get("schema_version") != 1:
        fail("ops/candidate.json schema_version must be 1")
    commit = str(doc.get("commit") or "").strip().lower()
    if not _COMMIT.fullmatch(commit):
        fail("ops/candidate.json commit is not a 40-hex sha")
    if not _is_rfc3339_z(doc.get("declared_at")):
        fail("ops/candidate.json declared_at must be RFC3339 UTC Z")
    reason = doc.get("reason")
    if not isinstance(reason, str) or not reason.strip() or "\n" in reason:
        fail("ops/candidate.json reason must be one non-empty line")
    verified = _git(["rev-parse", "--verify", f"{commit}^{{commit}}"])
    if verified.returncode != 0:
        fail(f"ops/candidate.json commit {commit} is not a commit in this repo")
    _CANDIDATE_COMMIT = commit
    _CANDIDATE_ORIGIN = (
        f"declared in ops/candidate.json at {doc.get('declared_at')}: {reason.strip()}"
    )
    return commit


_DRIFT_EXCLUSIONS = (
    ":!evidence",
    ":!ops/authorization-matrix.json",
    ":!ops/readiness.json",
    ":!ops/go-no-go.json",
    ":!ops/candidate.json",
)


def assert_no_code_drift(candidate: str) -> None:
    """Fail if tracked code changed after the candidate.

    Four kinds of file legitimately move after the candidate is declared, and
    nothing else does:

      evidence/                      the receipts, which are regenerated at the
                                     candidate and committed after it
      ops/authorization-matrix.json  a scored receipt that happens to live here
      ops/readiness.json             declared-score mirrors, which move when the
      ops/go-no-go.json              receipts they mirror are regenerated
      ops/candidate.json             the declaration itself, necessarily
                                     committed in a commit after the one it names

    ops/backend-alpha-gates.json is deliberately NOT excluded. It is the bar, not
    a ledger of results, and moving the bar after freezing the candidate is
    exactly the change that must be loud rather than silent — otherwise a gate
    could be reclassified underneath receipts that were earned against the old
    classification, and the score would still read as honest.

    Compared as commits (HEAD vs candidate), not the working tree.
    """
    diff = _git(
        [
            "diff",
            "--quiet",
            candidate,
            "HEAD",
            "--",
            ".",
            *_DRIFT_EXCLUSIONS,
        ]
    )
    if diff.returncode == 0:
        return
    if diff.returncode == 1:
        named = _git(
            [
                "diff",
                "--name-only",
                candidate,
                "HEAD",
                "--",
                ".",
                *_DRIFT_EXCLUSIONS,
            ]
        )
        files = [line for line in named.stdout.splitlines() if line]
        preview = ", ".join(files[:30]) if files else "(names unavailable)"
        fail(f"code changed since candidate {candidate}: {preview}")
    err = (diff.stderr or diff.stdout or "").strip() or f"exit {diff.returncode}"
    fail(f"cannot diff candidate {candidate} against HEAD: {err}")


def receipt_unbound_reason(doc: Any, candidate: str) -> str | None:
    """Why this receipt is not bound to the candidate, or None if it is.

    Uses scripts/lib/receipt_binding.py. binding_status is not authority:
    a BOUND label on a different commit is still unbound for scoring.
    """
    claimed = receipt_commit(doc)
    if claimed is None:
        return "receipt names no 40-hex commit"
    if not bound_to(doc, candidate):
        return f"receipt names {claimed}, candidate is {candidate}"
    return None


def evaluate_receipt(
    relative: str,
    checker: Callable[[Any], bool],
    points: int,
    candidate: str,
) -> tuple[bool, int, str]:
    """Return (ok, earned, note) for one DOMAIN_RECEIPTS row.

    Unbound receipts score zero even when the content check would pass.
    """
    points = int(points)
    path = ROOT / relative
    if not path.is_file():
        return False, 0, f"{relative}: MISSING → 0/{points}"
    doc: Any = load_json(relative) if relative.endswith(".json") else True
    if relative.endswith(".json") and doc is None:
        return False, 0, f"{relative}: UNREADABLE → 0/{points}"
    if relative.endswith(".json"):
        unbound = receipt_unbound_reason(doc, candidate)
        if unbound:
            return False, 0, f"{relative}: UNBOUND → 0/{points} ({unbound})"
    if not checker(doc):
        if checker is stripe_sandbox_matrix_proven:
            return (
                False,
                0,
                f"{relative}: CHECK_FAILED → 0/{points} "
                f"({stripe_sandbox_matrix_failure_reason(doc)})",
            )
        return False, 0, f"{relative}: CHECK_FAILED → 0/{points}"
    return True, points, f"{relative}: OK → {points}/{points}"


def status_in(*allowed: str) -> Callable[[Any], bool]:
    allowed_set = {a.upper() for a in allowed}

    def check(doc: Any) -> bool:
        if not isinstance(doc, dict):
            return False
        status = str(doc.get("status", "")).upper()
        return status in allowed_set

    return check


def file_exists(_doc: Any) -> bool:
    return True


def auth_matrix_complete(doc: Any) -> bool:
    if not isinstance(doc, dict):
        return False
    routes = 0
    for route_class in doc.get("route_classes", []):
        routes += len(route_class.get("routes", []))
    # 126 after the P8 composition GETs (buyer /v1/ui/v1/buy through authBuyer,
    # worker /v1/ui/v1/earn through authWorker, worker /v1/worker/ledger) joined
    # the reviewed matrix under the existing buyer_owned / worker_owned
    # role_decisions. Default deny is unchanged, so this is a pin retarget after
    # review, not a loosened content check.
    # The count is a tripwire, not a fact about the world: it exists so that
    # adding a route forces someone to look at the matrix. It had gone stale at
    # 118 while two routes were already serving buyer traffic, and stale AGAIN at
    # 123 — each time silently costing this domain 11 readiness points and making
    # `make ci` red. Both were found by re-deriving the score rather than trusting
    # it. So if you are reading this because the number moved: update BOTH this
    # tripwire and scripts/validate-authorization-matrix.py, and check they agree.
    return routes == 126 and doc.get("policy", {}).get("default") == "deny"


def technical_break_glass(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    bg = doc.get("break_glass") if isinstance(doc, dict) else None
    return isinstance(bg, dict) and str(bg.get("status", "")).upper() == "PASS"


def technical_privacy(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    for section in ("dsar", "deletion", "tombstone"):
        block = doc.get(section)
        if not isinstance(block, dict) or not any(block.values()):
            return False
    return True


def technical_tabletops(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    for section in ("support_tabletop", "security_tabletop"):
        block = doc.get(section)
        if not isinstance(block, dict) or str(block.get("status", "")).upper() != "PASS":
            return False
    return True


def soak_derivation_recorded(doc: Any) -> bool:
    """Local soak derivation (0 pts). Does not award the 24 h gate.

    Records that named time-dependent mechanisms were inventoried and
    exercised deterministically. Refuses any claim that a 24 h wall-clock
    soak passed.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "soak_requirement_derivation":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("qualifies_for_24h_gate") is True:
        return False
    if str(doc.get("conclusion", "")) != "deterministic_coverage_supersedes_arbitrary_24h":
        return False
    mechanisms = doc.get("mechanisms")
    if not isinstance(mechanisms, list) or len(mechanisms) < 8:
        return False
    for item in mechanisms:
        if not isinstance(item, dict):
            return False
        name = str(item.get("name", "")).strip()
        period = str(item.get("production_period", "")).strip()
        exercise = str(item.get("exercise", "")).strip()
        if len(name) < 3 or len(period) < 2 or len(exercise) < 8:
            return False
        if item.get("requires_wall_clock") is True:
            return False
    conclusion = str(doc.get("conclusion_text", "")).strip()
    if len(conclusion) < 40:
        return False
    if _has_secret_shaped(doc):
        return False
    return True


def soak_clean(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    bounds = doc.get("observed_bounds") if isinstance(doc, dict) else None
    if not isinstance(bounds, dict):
        return False
    restarts = bounds.get("control_restart_count")
    if isinstance(restarts, dict):
        restart_max = restarts.get("max", 1)
    else:
        restart_max = restarts if isinstance(restarts, (int, float)) else 1
    oom = bounds.get("control_oom_samples", 1)
    return restart_max == 0 and oom == 0


def payment_simulated(doc: Any) -> bool:
    if not isinstance(doc, dict):
        return False
    status = str(doc.get("status", "")).upper()
    label = str(doc.get("evidence_label", "")).upper()
    return "SIMULATED" in status or label == "SIMULATED"


def alert_delivery_proven(doc: Any) -> bool:
    """Validate an observed Alertmanager fire/resolve delivery receipt.

    The receipt is deliberately stricter than the harness simulation: it must
    contain two observed sink payloads, matching alert fingerprints, and both
    Alertmanager states.  This earns the private technical-delivery point only;
    the external staffed paging receiver remains a separate release gate.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "alert_delivery" or str(doc.get("status", "")).upper() != "PASS":
        return False
    receiver = doc.get("receiver")
    delivery = doc.get("delivery")
    observations = doc.get("observations")
    if not isinstance(receiver, dict) or not isinstance(delivery, dict) or not isinstance(observations, dict):
        return False
    if receiver.get("transport") != "alertmanager_webhook" or receiver.get("secret_values_recorded") is not False:
        return False
    host = str(receiver.get("url_host", "")).strip().lower()
    if not host or "harness" in host or "example" in host:
        return False
    try:
        count = int(delivery.get("sink_event_count", 0))
    except (TypeError, ValueError):
        return False
    firing = observations.get("firing")
    resolved = observations.get("resolved")
    if not isinstance(firing, dict) or not isinstance(resolved, dict):
        return False
    firing_body = firing.get("body")
    resolved_body = resolved.get("body")
    if not isinstance(firing_body, dict) or not isinstance(resolved_body, dict):
        return False
    return (
        count >= 2
        and bool(str(delivery.get("firing_received_at", "")).strip())
        and bool(str(delivery.get("resolved_received_at", "")).strip())
        and str(delivery.get("firing_fingerprint", "")).strip()
        == str(delivery.get("resolved_fingerprint", "")).strip()
        and bool(str(delivery.get("firing_fingerprint", "")).strip())
        and str(firing_body.get("status", "")).lower() == "firing"
        and str(resolved_body.get("status", "")).lower() == "resolved"
    )


def _all_strings(value: Any) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield str(key)
            yield from _all_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from _all_strings(item)


def _has_secret_shaped(doc: Any) -> bool:
    return any(_SECRET_SHAPED.search(text) for text in _all_strings(doc))


def _is_rfc3339_z(value: Any) -> bool:
    if not isinstance(value, str) or not _RFC3339_Z.fullmatch(value):
        return False
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError:
        return False
    return parsed.utcoffset() == dt.timedelta(0)


def _parse_utc(value: str) -> dt.datetime | None:
    if not _is_rfc3339_z(value):
        return None
    try:
        return dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError:
        return None


def _nonempty_text(value: Any, minimum: int = 3) -> bool:
    if not isinstance(value, str):
        return False
    text = value.strip()
    # Reject leading/trailing whitespace and placeholder tokens.
    if len(text) < minimum or value != text:
        return False
    if _PLACEHOLDER_TOKEN.search(text):
        return False
    return True


def _is_public_staging_host(host: Any) -> bool:
    if not isinstance(host, str):
        return False
    host = host.strip().lower().rstrip(".")
    if not host or "." not in host or " " in host:
        return False
    if _LOCAL_HOST_TOKEN.search(host):
        return False
    if host.endswith(".local") or host.endswith(".internal"):
        return False
    labels = host.split(".")
    if any(not label or len(label) > 63 for label in labels):
        return False
    return True


def _is_s3_offsite_uri(value: Any) -> bool:
    if not isinstance(value, str) or not value.startswith("s3://"):
        return False
    if any(ch.isspace() for ch in value) or "?" in value or "#" in value:
        return False
    rest = value[len("s3://") :]
    if "/" not in rest:
        return False
    bucket, _, key = rest.partition("/")
    if not bucket or ".." in key.split("/"):
        return False
    if "@" in bucket or ":" in bucket:
        return False
    return bool(key)


def _truthy_map(block: Any, required: set[str]) -> bool:
    if not isinstance(block, dict):
        return False
    for key in required:
        if block.get(key) is not True:
            return False
    return True


def stripe_sandbox_matrix_failure_reason(doc: Any) -> str:
    """Why stripe_sandbox_matrix_proven rejected this receipt.

    The 6-point award still requires a Connect-complete PASS. An honest
    BLOCKED receipt after the non-Connect wall is a CHECK_FAILED, not a
    silent zero.
    """
    if not isinstance(doc, dict):
        return "receipt is not an object"
    if str(doc.get("kind", "")) != "stripe_sandbox_matrix":
        return "kind is not stripe_sandbox_matrix"
    if str(doc.get("status", "")).upper() != "PASS":
        status = str(doc.get("status", ""))
        blocker = doc.get("blocker") if isinstance(doc.get("blocker"), dict) else {}
        blocker_id = str(blocker.get("id") or "")
        if status.upper() == "BLOCKED" and blocker_id:
            return (
                f"status={status} blocker={blocker_id}; "
                "stripe_sandbox_matrix_proven requires Connect-complete "
                "PASS with tr_ transfer and payout hold/release/failure/reversal"
            )
        return (
            f"status={status or 'missing'}; "
            "Connect-complete PASS required (transfer tr_ + payouts)"
        )
    if str(doc.get("provider_mode", "")).lower() != "test":
        return "provider_mode is not test"
    if str(doc.get("live_mode", "")).upper() != "PROHIBITED":
        return "live_mode is not PROHIBITED"
    payment_objects = doc.get("payment_objects")
    if isinstance(payment_objects, dict) and payment_objects.get("transfer") is not True:
        return "payment_objects.transfer is not true (no synthesized tr_)"
    external = doc.get("external_scenarios")
    if isinstance(external, dict):
        if str(external.get("status", "")).upper() != "PASS":
            return "external_scenarios.status is not PASS"
        payout = external.get("payout")
        if isinstance(payout, dict) and not all(
            payout.get(key) is True for key in ("hold", "release", "failure", "reversal")
        ):
            return "external_scenarios.payout hold/release/failure/reversal incomplete"
        transfer = str(external.get("transfer", "")).strip()
        if not transfer.startswith("tr_"):
            return "external_scenarios.transfer is not a real tr_ id"
    return "content check failed"


def stripe_sandbox_matrix_proven(doc: Any) -> bool:
    """Stripe sandbox matrix with provider reconciliation (6 pts).

    Matches scripts/stripe-sandbox.sh matrix emission. Refuses simulated
    payment receipts, live mode, secret-shaped values, and incomplete scenario
    coverage. status:PASS alone is not enough.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "stripe_sandbox_matrix":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if str(doc.get("provider_mode", "")).lower() != "test":
        return False
    if str(doc.get("live_mode", "")).upper() != "PROHIBITED":
        return False
    if doc.get("secret_values_printed") is not False:
        return False
    if not _nonempty_text(doc.get("run_id"), minimum=8):
        return False
    currency = str(doc.get("settlement_currency", "")).strip().lower()
    if len(currency) != 3 or not currency.isalpha():
        return False

    payment_objects = doc.get("payment_objects")
    if not isinstance(payment_objects, dict):
        return False
    for key in (
        "authorization",
        "capture",
        "decline",
        "idempotency",
        "refunds",
        "transfer",
    ):
        if payment_objects.get(key) is not True:
            return False
    timeout = payment_objects.get("timeout")
    if not isinstance(timeout, dict):
        return False
    if timeout.get("client_deadline") is not True or timeout.get("idempotent_recovery") is not True:
        return False

    external = doc.get("external_scenarios")
    if not isinstance(external, dict):
        return False
    if str(external.get("status", "")).upper() != "PASS":
        return False
    if str(external.get("provider_mode", "")).lower() != "test":
        return False
    if external.get("secret_values_recorded") is not False:
        return False
    if str(external.get("live_mode", "")).upper() != "PROHIBITED":
        return False
    webhook = external.get("webhook")
    if not isinstance(webhook, dict):
        return False
    for key in (
        "endpoint_secrets_verified",
        "delivery",
        "staging_urls_exact",
        "application_outcomes_verified",
        "replay_idempotent",
        "out_of_order_safe",
        "distinct_endpoint_ids",
    ):
        if webhook.get(key) is not True:
            return False
    api_version = str(webhook.get("payload_api_version", "")).strip()
    if "2025-06-30" not in api_version:
        return False
    dispute = external.get("dispute")
    payout = external.get("payout")
    reconciliation = external.get("reconciliation")
    settlement = external.get("settlement")
    if not isinstance(dispute, dict) or not isinstance(payout, dict):
        return False
    if not isinstance(reconciliation, dict) or not isinstance(settlement, dict):
        return False
    if dispute.get("opened") is not True or dispute.get("resolved") is not True:
        return False
    for key in ("hold", "release", "failure", "reversal"):
        if payout.get(key) is not True:
            return False
    if reconciliation.get("clean") is not True:
        return False
    if str(settlement.get("currency", "")).strip().lower() != currency:
        return False
    # Real provider object identifiers from the matrix run (not inventable
    # as empty/true flags alone).
    for field, prefix in (
        ("payment_intent", "pi_"),
        ("charge", "ch_"),
        ("transfer", "tr_"),
        ("disputed_payment_intent", "pi_"),
        ("disputed_charge", "ch_"),
    ):
        value = str(external.get(field, "")).strip()
        if not value.startswith(prefix) or len(value) < len(prefix) + 8:
            return False
    if _has_secret_shaped(doc):
        return False
    # Refuse the local simulator shape even if someone renames its kind.
    if str(doc.get("evidence_label", "")).upper() == "SIMULATED":
        return False
    if "SIMULATED" in str(doc.get("status", "")).upper():
        return False
    return True


def _soak24_samples_corroborate(
    doc: dict[str, Any],
    *,
    expected_commit: str,
    host: str,
    interval: int,
) -> bool:
    """Re-derive the 24 h window from the HTTPS-observer JSONL.

    A hand-typed PASS without a corroborating ≥86400 s sample stream cannot
    survive this. Does not trust duration.* on the receipt.
    """
    samples = doc.get("samples")
    if not isinstance(samples, dict):
        return False
    samples_rel = str(samples.get("path", "")).strip()
    if not samples_rel or samples_rel.startswith("/") or ".." in Path(samples_rel).parts:
        return False
    samples_path = ROOT / samples_rel
    if not samples_path.is_file() or samples_path.is_symlink():
        return False
    samples_sha = str(samples.get("sha256", "")).strip()
    if samples_sha:
        if not _SHA256.fullmatch(samples_sha):
            return False
        digest = hashlib.sha256()
        try:
            with samples_path.open("rb") as handle:
                for chunk in iter(lambda: handle.read(1 << 20), b""):
                    digest.update(chunk)
        except OSError:
            return False
        if digest.hexdigest() != samples_sha:
            return False
    rows: list[dict[str, Any]] = []
    try:
        with samples_path.open(encoding="utf-8") as handle:
            for raw in handle:
                line = raw.strip()
                if not line:
                    return False
                row = json.loads(line)
                if not isinstance(row, dict):
                    return False
                rows.append(row)
    except (OSError, json.JSONDecodeError):
        return False
    if len(rows) < 2:
        return False
    times: list[dt.datetime] = []
    last_epoch: int | None = None
    max_gap = 0
    for row in rows:
        if row.get("ok") is not True:
            return False
        observed = _parse_utc(str(row.get("observed_at", "")))
        if observed is None:
            return False
        epoch = int(observed.timestamp())
        if last_epoch is not None:
            gap = epoch - last_epoch
            if gap > max_gap:
                max_gap = gap
        last_epoch = epoch
        times.append(observed)
        if str(row.get("host", "")).strip().lower().rstrip(".") != host:
            return False
        commit = str(row.get("commit") or "").strip().lower()
        if commit != expected_commit:
            return False
        if str(row.get("payment_mode", "")).strip().lower() != "test":
            return False
        if row.get("live_value_movement") is not False:
            return False
        http = row.get("http") if isinstance(row.get("http"), dict) else {}
        try:
            version_status = int(http.get("version_status", 0))
            readyz_status = int(http.get("readyz_status", 0))
        except (TypeError, ValueError):
            return False
        if version_status != 200 or readyz_status != 200:
            return False
        version = row.get("version")
        if isinstance(version, dict) and version.get("modified") is True:
            return False
    window = int((times[-1] - times[0]).total_seconds())
    if window < 86400:
        return False
    if times != sorted(times):
        return False
    if max_gap > interval + _SOAK24_GAP_SLACK_S:
        return False
    return True


def qualifying_24h_soak_proven(doc: Any) -> bool:
    """Qualifying ≥24 h soak on persistent staging (3 pts).

    The wired path evidence/external/qualifying-soak-24h.json is written by
    scripts/soak/soak24.py as qualifying_24h_https_observer v1 (the
    go_closure_soak producer writes under evidence/go-closure/ and is not
    this row). Demand that observer shape. Do not award on an in-progress
    window, a redeploy, a payment-envelope break, or a receipt that does
    not independently re-derive ≥86400 s from the JSONL sample stream.
    Local 60 s / 300 s soaks and iteration mode cannot pass.
    """
    if not isinstance(doc, dict):
        return False
    if doc.get("schema_version") != 1:
        return False
    if str(doc.get("kind", "")) != "qualifying_24h_https_observer":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if str(doc.get("mode", "")) != "qualifying":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False

    started = _parse_utc(str(doc.get("started_at", "")))
    finished = _parse_utc(str(doc.get("finished_at", "")))
    if started is None or finished is None or finished <= started:
        return False
    wall = int((finished - started).total_seconds())
    if wall < 86400:
        return False

    duration = doc.get("duration")
    if not isinstance(duration, dict):
        return False
    try:
        requested = int(duration["requested_seconds"])
        elapsed = int(duration["elapsed_seconds"])
        observed_window = int(duration["observed_window_seconds"])
        interval = int(duration["interval_seconds"])
        sample_count = int(duration["samples"])
        ok_samples = int(duration["ok_samples"])
        failed_samples = int(duration["failed_samples"])
        max_gap = int(duration["max_inter_sample_gap_seconds"])
    except (KeyError, TypeError, ValueError):
        return False
    if requested < 86400 or elapsed < 86400 or observed_window < 86400:
        return False
    if elapsed < requested:
        return False
    if interval < 15 or interval > 900:
        return False
    if sample_count < 2 or ok_samples < 2 or failed_samples != 0:
        return False
    if ok_samples + failed_samples != sample_count:
        return False
    if max_gap > interval + _SOAK24_GAP_SLACK_S:
        return False
    if wall < elapsed:
        return False

    qualification = doc.get("qualification")
    if not isinstance(qualification, dict):
        return False
    if qualification.get("qualifies_for_24h_gate") is not True:
        return False
    if qualification.get("reason") != "observed_at_least_86400_seconds":
        return False

    expected_commit = str(doc.get("expected_commit", "")).strip().lower()
    if not _COMMIT.fullmatch(expected_commit):
        return False

    host = str(doc.get("host", "")).strip().lower().rstrip(".")
    if not _is_public_staging_host(host):
        return False
    base_url = str(doc.get("base_url", "")).strip().lower()
    if not base_url.startswith("https://" + host):
        return False

    candidate = doc.get("candidate")
    if not isinstance(candidate, dict):
        return False
    if candidate.get("changed") is not False:
        return False
    if candidate.get("modified_seen") is not False:
        return False
    if str(candidate.get("continuity", "")) != "uninterrupted":
        return False
    if str(candidate.get("expected_commit", "")).strip().lower() != expected_commit:
        return False
    observed_commits = candidate.get("observed_commits")
    if not isinstance(observed_commits, list) or not observed_commits:
        return False
    if any(str(item).strip().lower() != expected_commit for item in observed_commits):
        return False

    payment = doc.get("payment")
    if not isinstance(payment, dict):
        return False
    if str(payment.get("required_payment_mode", "")).strip().lower() != "test":
        return False
    if payment.get("required_live_value_movement") is not False:
        return False
    if payment.get("left_test_envelope") is not False:
        return False

    observer = doc.get("observer")
    if not isinstance(observer, dict):
        return False
    if str(observer.get("kind", "")) != "https_public_tls":
        return False
    paths = observer.get("paths")
    if not isinstance(paths, list):
        return False
    if "/version" not in paths or "/readyz" not in paths:
        return False

    policy = doc.get("policy")
    if policy != {
        "stripe_test_mode": True,
        "stripe_live_mode": False,
        "real_value": False,
        "secret_values_recorded": False,
    }:
        return False

    last_readyz = doc.get("last_readyz")
    if not isinstance(last_readyz, dict):
        return False
    if str(last_readyz.get("payment_mode", "")).strip().lower() != "test":
        return False
    if last_readyz.get("live_value_movement") is not False:
        return False

    if _has_secret_shaped(doc):
        return False

    return _soak24_samples_corroborate(
        doc,
        expected_commit=expected_commit,
        host=host,
        interval=interval,
    )


def independent_offsite_backup_proven(doc: Any) -> bool:
    """Independent external offsite encrypted backup copy (2 pts).

    Matches scripts/backup.sh merc_offsite_backup_verification emission.
    Requires an s3:// offsite URI, independent re-download of both manifest
    and ciphertext, matching checksums, and encrypted-before-upload policy.
    Local-only restore receipts cannot pass.
    """
    if not isinstance(doc, dict):
        return False
    if doc.get("schema_version") != 1:
        return False
    if str(doc.get("kind", "")) != "merc_offsite_backup_verification":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    backup_id = str(doc.get("backup_id", ""))
    if not _BACKUP_ID.fullmatch(backup_id):
        return False
    offsite = doc.get("offsite_uri")
    if not _is_s3_offsite_uri(offsite):
        return False
    if not str(offsite).rstrip("/").endswith("/" + backup_id):
        return False
    if not _SHA256.fullmatch(str(doc.get("manifest_sha256", ""))):
        return False
    ciphertext = doc.get("ciphertext")
    if not isinstance(ciphertext, dict):
        return False
    manifest_sha = str(ciphertext.get("manifest_sha256", ""))
    downloaded_sha = str(ciphertext.get("downloaded_sha256", ""))
    if not _SHA256.fullmatch(manifest_sha) or not _SHA256.fullmatch(downloaded_sha):
        return False
    if manifest_sha != downloaded_sha:
        return False
    try:
        byte_count = int(ciphertext.get("bytes", 0))
    except (TypeError, ValueError):
        return False
    if byte_count <= 0:
        return False
    if not _is_rfc3339_z(doc.get("verified_at")):
        return False
    if not _truthy_map(
        doc.get("checks"),
        {
            "offsite_bundle_visible",
            "independent_manifest_download",
            "independent_ciphertext_download",
            "manifest_checksum_match",
            "ciphertext_checksum_match",
        },
    ):
        return False
    policy = doc.get("policy")
    if policy != {
        "encrypted_before_upload": True,
        "plaintext_uploaded": False,
        "secret_values_recorded": False,
    }:
        return False
    if _has_secret_shaped(doc):
        return False
    # Explicit refusal of local logical-restore stand-ins.
    if str(doc.get("external_offsite_restore", "")).upper() == "NOT EXECUTED":
        return False
    if str(doc.get("kind", "")) == "logical_independent_restore":
        return False
    return True


def external_offsite_restore_proven(doc: Any) -> bool:
    """External offsite independent restore (1 pt).

    Requires a restore from an independently downloaded s3:// offsite bundle
    into an isolated environment with observed integrity. The local
    logical-independent-restore receipt (external_offsite_restore:NOT EXECUTED)
    cannot pass.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "external_offsite_restore":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    backup_id = str(doc.get("backup_id", ""))
    if not _BACKUP_ID.fullmatch(backup_id):
        return False
    offsite = doc.get("offsite_uri")
    if not _is_s3_offsite_uri(offsite):
        return False
    if not str(offsite).rstrip("/").endswith("/" + backup_id):
        return False
    if not _SHA256.fullmatch(str(doc.get("ciphertext_sha256", ""))):
        return False
    if not _is_rfc3339_z(doc.get("completed_at")):
        return False

    independence = doc.get("independence") if isinstance(doc.get("independence"), dict) else {}
    live_source = (
        str(independence.get("source_kind", "")) == "live_droplet_volume"
        or independence.get("live_droplet_volume_is_source") is True
    )

    for flag in (
        "independent_download",
        "ciphertext_checksum_verified",
        "decrypt_isolated",
        "new_database_credentials",
        "new_object_credentials",
        "new_namespace",
    ):
        if doc.get(flag) is not True:
            return False
    if live_source:
        # The live droplet must still be serving. Destroying it would be the
        # harm this gate exists to survive.
        if doc.get("source_environment_destroyed") is True:
            return False
        if independence.get("live_volumes_untouched") is not True:
            return False
        if independence.get("producer_plaintext_destroyed") is not True:
            return False
        if independence.get("live_droplet_volume_not_the_source") is True:
            return False
    elif doc.get("source_environment_destroyed") is not True:
        return False

    integrity = doc.get("integrity")
    if not isinstance(integrity, dict):
        return False
    for flag in (
        "ciphertext_verified",
        "payload_checksums_verified",
        "corrupt_backup_rejected",
        "ledger_zero_sum",
        "artifact_sentinels_verified",
    ):
        if integrity.get(flag) is not True:
            return False
    semantics = integrity.get("database_semantics")
    if not isinstance(semantics, dict) or not semantics:
        return False
    try:
        object_count = int(integrity.get("object_count", -1))
    except (TypeError, ValueError):
        return False
    if object_count < 0:
        return False

    # Refuse any residual local-only marker if present.
    marker = str(doc.get("external_offsite_restore", "PASS")).upper()
    if marker in {"NOT EXECUTED", "NOT_EXECUTED", "LOCAL", "SIMULATED"}:
        return False
    if _has_secret_shaped(doc):
        return False
    # Cross-check the local restore ledger: it must no longer claim the
    # external offsite restore was skipped.
    local = load_json("evidence/autonomous/logical-independent-restore.json")
    if isinstance(local, dict):
        local_marker = str(local.get("external_offsite_restore", "")).upper()
        if local_marker in {"NOT EXECUTED", "NOT_EXECUTED", "PENDING", ""}:
            return False
        if local_marker not in {"PASS", "EXECUTED", "COMPLETE"}:
            return False
    return True


_DRIVEN_ATTACK_CLASSES = ("identity", "identity_webhook", "authority", "money", "tls", "containment")


def named_attack_reviewer_proven(doc: Any) -> bool:
    """Named human reviewer of the public-hostname rehearsal.

    PUBLIC_LAUNCH only. Empty name/organization is an honest unmet, not a
    pass. Placeholder and machine-shaped strings are refused. Does not
    award the alpha security point.
    """
    if not external_staging_attack_proven(doc):
        return False
    reviewer = doc.get("reviewer")
    if not isinstance(reviewer, dict):
        return False
    if not _nonempty_text(reviewer.get("name"), minimum=5):
        return False
    if not _nonempty_text(reviewer.get("organization"), minimum=3):
        return False
    return True


def external_staging_attack_proven(doc: Any) -> bool:
    """External staging attack rehearsal (1 pt).

    Hostile exercise against a real public TLS staging hostname — not the
    local technical security_tabletop already scored in technical-exercises.
    Awards on executed public-surface evidence only. A named human
    reviewer is a separate PUBLIC_LAUNCH obligation
    (named_reviewer:staging-attack-rehearsal).
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "external_staging_attack_rehearsal":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if str(doc.get("surface", "")) != "persistent_staging_tls":
        return False

    target = doc.get("target")
    if not isinstance(target, dict):
        return False
    host = target.get("hostname")
    if not _is_public_staging_host(host):
        return False
    if str(target.get("scheme", "")).lower() != "https":
        return False
    url = str(target.get("url", "")).strip().lower()
    if not url.startswith("https://" + str(host).strip().lower()):
        return False
    if _LOCAL_HOST_TOKEN.search(url):
        return False

    findings = doc.get("findings")
    if not isinstance(findings, dict):
        return False
    # Critical negatives must be observed false; break-glass exercise true.
    if findings.get("cross_tenant_access") is not False:
        return False
    if findings.get("authz_bypass") is not False:
        return False
    if findings.get("break_glass_under_staging") is not True:
        return False

    observations = doc.get("observations")
    if not isinstance(observations, dict):
        return False
    started = _parse_utc(str(observations.get("started_at", "")))
    finished = _parse_utc(str(observations.get("finished_at", "")))
    if started is None or finished is None or finished <= started:
        return False
    try:
        request_count = int(observations.get("request_count", 0))
        routes = int(observations.get("distinct_routes_exercised", 0))
        executed = int(observations.get("attacks_executed", 0))
    except (TypeError, ValueError):
        return False
    if request_count < 5 or routes < 3 or executed < 5:
        return False

    classes = doc.get("attack_classes")
    if not isinstance(classes, dict) or not classes:
        return False
    for row in classes.values():
        if not isinstance(row, dict):
            return False
        try:
            if int(row.get("finding", -1)) != 0:
                return False
        except (TypeError, ValueError):
            return False
    for name in _DRIVEN_ATTACK_CLASSES:
        row = classes.get(name)
        if not isinstance(row, dict):
            return False
        try:
            attempted = int(row.get("attempted", -1))
            blocked = int(row.get("blocked", -1))
            finding = int(row.get("finding", -1))
            class_executed = int(row.get("executed", -1))
        except (TypeError, ValueError):
            return False
        if attempted < 1 or blocked < 0 or finding != 0 or class_executed < 1:
            return False

    # Local technical tabletop markers must not be accepted as external.
    if str(doc.get("qualification", "")).upper() in {"TECHNICAL", "LOCAL", "SIMULATED"}:
        return False
    if doc.get("technical_only") is True:
        return False
    if _has_secret_shaped(doc):
        return False
    return True


def _approval_record_qualified(record: Any) -> bool:
    if not isinstance(record, dict):
        return False
    if str(record.get("status", "")).upper() != "APPROVED":
        return False
    if not _nonempty_text(record.get("approver"), minimum=5):
        return False
    if not _nonempty_text(record.get("organization"), minimum=3):
        return False
    if not _nonempty_text(record.get("reviewed_scope"), minimum=12):
        return False
    evidence_uri = str(record.get("evidence_uri", "")).strip()
    if len(evidence_uri) < 12 or _PLACEHOLDER_TOKEN.search(evidence_uri):
        return False
    # Evidence must sit outside the repo's local technical receipts.
    lowered = evidence_uri.lower()
    if lowered.startswith("evidence/autonomous/"):
        return False
    if "technical-exercises" in lowered:
        return False
    if not _is_rfc3339_z(record.get("approved_at")):
        return False
    return True


def _exercise_record_pass(record: Any) -> bool:
    if not isinstance(record, dict):
        return False
    if str(record.get("status", "")).upper() != "PASS":
        return False
    evidence_uri = str(record.get("evidence_uri", "")).strip()
    if len(evidence_uri) < 12 or _PLACEHOLDER_TOKEN.search(evidence_uri):
        return False
    if evidence_uri.lower().startswith("evidence/autonomous/"):
        return False
    if not _is_rfc3339_z(record.get("completed_at")):
        return False
    return True


def privacy_qualified_approval_proven(doc: Any) -> bool:
    """Qualified privacy approval + external subprocessor deletion (1 pt).

    Local technical DSAR/deletion/tombstone already scores 3/4. This demands
    a named privacy authority approval and an executed external subprocessor
    deletion exercise — not technical-exercises.json.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "privacy_qualified_approval":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if doc.get("schema_version") != 1:
        return False
    if str(doc.get("scope", "")) != "supervised_stripe_test_mode_private_canary":
        return False
    commit = str(doc.get("candidate_commit", ""))
    if not _COMMIT.fullmatch(commit):
        return False
    if not _approval_record_qualified(doc.get("approval")):
        return False
    if not _exercise_record_pass(doc.get("dsar_export_deletion")):
        return False
    sub = doc.get("external_subprocessor_deletion")
    if not isinstance(sub, dict):
        return False
    if str(sub.get("status", "")).upper() != "PASS":
        return False
    if sub.get("executed") is not True:
        return False
    if not _nonempty_text(sub.get("subprocessor"), minimum=3):
        return False
    if not _is_rfc3339_z(sub.get("completed_at")):
        return False
    evidence_uri = str(sub.get("evidence_uri", "")).strip()
    if len(evidence_uri) < 12 or _PLACEHOLDER_TOKEN.search(evidence_uri):
        return False
    # Refuse the technical-exercises marker.
    if str(sub.get("status", "")).upper() in {"NOT EXECUTED", "NOT_EXECUTED"}:
        return False
    # Cross-check the live technical qualification ledger. The external receipt
    # alone must not award the point while technical-exercises still records
    # external_subprocessor_deletion as NOT EXECUTED.
    technical = load_json("evidence/autonomous/technical-exercises.json")
    if not isinstance(technical, dict):
        return False
    qualification = technical.get("qualification")
    if not isinstance(qualification, dict):
        return False
    live_sub = str(qualification.get("external_subprocessor_deletion", "")).upper()
    if live_sub in {"", "NOT EXECUTED", "NOT_EXECUTED", "PENDING"}:
        return False
    if live_sub not in {"PASS", "EXECUTED", "COMPLETE"}:
        return False
    if _has_secret_shaped(doc):
        return False
    return True


def licensing_provenance_approval_proven(doc: Any) -> bool:
    """License and asset/model provenance approval (1 pt).

    Requires a named licensing authority approval and a completed
    asset_and_model_provenance exercise with zero remaining BLOCKED rows.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "licensing_provenance_approval":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if doc.get("schema_version") != 1:
        return False
    if str(doc.get("scope", "")) != "supervised_stripe_test_mode_private_canary":
        return False
    commit = str(doc.get("candidate_commit", ""))
    if not _COMMIT.fullmatch(commit):
        return False
    if not _approval_record_qualified(doc.get("approval")):
        return False
    if not _exercise_record_pass(doc.get("asset_and_model_provenance")):
        return False
    register = doc.get("provenance_register")
    if not isinstance(register, dict):
        return False
    if str(register.get("status", "")).upper() != "APPROVED":
        return False
    try:
        blocked = int(register.get("blocked_rows_remaining", -1))
    except (TypeError, ValueError):
        return False
    if blocked != 0:
        return False
    if not _nonempty_text(register.get("register_uri"), minimum=12):
        return False
    # Refuse residual BLOCKED markers inside the receipt register summary.
    status_blob = " ".join(
        str(value) for value in _all_strings(register) if isinstance(value, str)
    ).upper()
    if "BLOCKED" in status_blob:
        return False
    # Cross-check the live in-repo provenance register. Planting only the
    # external receipt while ops/asset-provenance.json remains BLOCKED_* must
    # not award the point.
    live = load_json("ops/asset-provenance.json")
    if not isinstance(live, dict):
        return False
    live_status = str(live.get("status", "")).upper()
    if not live_status or live_status.startswith("BLOCKED") or live_status in {
        "PENDING",
        "NOT_EXECUTED",
        "NOT EXECUTED",
    }:
        return False
    if live_status not in {"APPROVED", "PASS", "COMPLETE", "CLEARED"}:
        return False
    if _has_secret_shaped(doc):
        return False
    return True


def staffed_abuse_route_or_tabletop_proven(doc: Any) -> bool:
    """Staffed human abuse route or qualified multi-role tabletop (1 pt).

    Local technical tabletops already score 1/2. This requires named humans
    on a real escalation path (or a timed multi-role tabletop) with
    non-placeholder contacts — not technical-exercises.json.
    """
    if not isinstance(doc, dict):
        return False
    kind = str(doc.get("kind", ""))
    if kind not in {"staffed_abuse_route", "qualified_human_abuse_tabletop"}:
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if doc.get("technical_only") is True:
        return False
    if str(doc.get("qualification", "")).upper() in {
        "TECHNICAL",
        "LOCAL",
        "NOT EXECUTED",
        "NOT_EXECUTED",
    }:
        return False

    if kind == "staffed_abuse_route":
        route = doc.get("route")
        if not isinstance(route, dict):
            return False
        if not _nonempty_text(route.get("owner"), minimum=5):
            return False
        if not _nonempty_text(route.get("organization"), minimum=3):
            return False
        contact = str(route.get("contact", "")).strip()
        # Staffed contact: email-shaped, not a placeholder or example host.
        if "@" not in contact or contact.count("@") != 1:
            return False
        local, _, domain = contact.partition("@")
        if len(local) < 2 or not _is_public_staging_host(domain):
            return False
        if _PLACEHOLDER_TOKEN.search(contact):
            return False
        if not _nonempty_text(route.get("runbook_uri"), minimum=12):
            return False
        if not _is_rfc3339_z(route.get("staffed_since")):
            return False
        if route.get("human_on_call") is not True:
            return False
    else:
        tabletop = doc.get("tabletop")
        if not isinstance(tabletop, dict):
            return False
        started = _parse_utc(str(tabletop.get("started_at", "")))
        finished = _parse_utc(str(tabletop.get("finished_at", "")))
        if started is None or finished is None or finished <= started:
            return False
        # At least 30 minutes of wall clock — not a checkbox flip.
        if int((finished - started).total_seconds()) < 1800:
            return False
        roles = tabletop.get("roles")
        if not isinstance(roles, dict):
            return False
        required_roles = ("trust_and_safety", "support", "security")
        for role in required_roles:
            person = roles.get(role)
            if not isinstance(person, dict):
                return False
            if not _nonempty_text(person.get("name"), minimum=5):
                return False
            if not _nonempty_text(person.get("organization"), minimum=3):
                return False
        if tabletop.get("escalation_path_exercised") is not True:
            return False
        if tabletop.get("postmortem_required") is not True:
            return False
        if not _nonempty_text(tabletop.get("evidence_uri"), minimum=12):
            return False

    if _has_secret_shaped(doc):
        return False
    # Cross-check technical qualification ledger: a planted staffed-route
    # receipt must not score while technical-exercises still says the qualified
    # human tabletop was NOT EXECUTED.
    technical = load_json("evidence/autonomous/technical-exercises.json")
    if not isinstance(technical, dict):
        return False
    qualification = technical.get("qualification")
    if not isinstance(qualification, dict):
        return False
    live_human = str(qualification.get("qualified_human_tabletop", "")).upper()
    if live_human in {"", "NOT EXECUTED", "NOT_EXECUTED", "PENDING"}:
        return False
    if live_human not in {"PASS", "EXECUTED", "COMPLETE", "STAFFED"}:
        return False
    return True


ALLOWED_CLASSIFICATIONS = frozenset(
    {
        "ALPHA_BLOCKER",
        "ALPHA_CONTROL",
        "POST_ALPHA",
        "PUBLIC_LAUNCH",
        "ENTERPRISE",
        "OBSOLETE",
    }
)
ALPHA_SCORED = frozenset({"ALPHA_BLOCKER", "ALPHA_CONTROL"})
KNOWN_P1_IDS = frozenset(
    {
        "P1-STAGING",
        "P1-RECOVERY-SOAK",
        "P1-OFFSITE-RESTORE",
        "P1-STRIPE-TEST",
        "P1-ALERT-DELIVERY",
        "P1-CANARY-REHEARSAL",
        "P1-INDEPENDENT-APPROVAL",
        "P1-GOVERNANCE",
    }
)
P0_INDEPENDENT_SUPPLIER = "P0-INDEPENDENT-SUPPLIER"
# Rescoped obligations that are not DOMAIN_RECEIPTS / P1 / P0 / soak / claim.
# A rescope lands here so the requirement is not deleted.
NAMED_REVIEWER_GATE_ID = "named_reviewer:staging-attack-rehearsal"
NAMED_REVIEWER_RECEIPT = "evidence/external/staging-attack-rehearsal.json"
ALPHA_SOAK_RECEIPT = "evidence/external/qualifying-soak-alpha.json"
ALPHA_SOAK_MINIMUM_SECONDS = 3600
EXTERNAL_PARTICIPANTS_RECEIPT = "evidence/external/external-alpha-participants.json"
_SYNTHETIC_CLASS = frozenset(
    {
        "synthetic",
        "operator_synthetic",
        "operator_controlled",
        "operator_owned",
        "harness",
        "test",
        "test_fixture",
        "fixture",
        "disposable",
        "local_simulator",
        "simulator",
        "canary_synthetic",
    }
)
_SYNTHETIC_EMAIL = re.compile(
    r"(synthetic|canary-bot|noreply\+canary|invalid|example\.(com|org|net)|test\+)",
    re.IGNORECASE,
)


def _participant_is_synthetic(participant: Any) -> bool:
    """True if this identity is synthetic, disposable, or operator-side."""
    if not isinstance(participant, dict):
        return True
    if participant.get("synthetic") is True:
        return True
    if participant.get("controlled_by_operator") is True:
        return True
    if participant.get("operator_owned") is True:
        return True
    classification = str(participant.get("participant_class", "")).strip().lower()
    if classification in _SYNTHETIC_CLASS:
        return True
    kind = str(participant.get("identity_kind", "")).strip().lower()
    if kind in _SYNTHETIC_CLASS:
        return True
    email = str(participant.get("email", "")).strip()
    if _SYNTHETIC_EMAIL.search(email):
        return True
    identity = str(participant.get("id", "")).strip().lower()
    if identity.startswith("00000000-0000-") or identity in {
        "00000000-0000-0000-0000-000000000000",
        "",
    }:
        # Empty id is incomplete, not an independent external.
        if identity.startswith("00000000-0000-") or identity == "00000000-0000-0000-0000-000000000000":
            return True
    return False


CANARY_REHEARSAL_RECEIPT_GLOB = "evidence/canary/l11-p1-canary-rehearsal-*.json"


def refuse_canary_rehearsal_as_external() -> None:
    """L11 rehearsal receipts are operator-controlled. They must not
    be labelled as EXTERNAL_ALPHA_PROVEN or independent_external."""
    import glob

    paths = sorted(glob.glob(str(ROOT / CANARY_REHEARSAL_RECEIPT_GLOB)))
    for path in paths:
        rel = str(Path(path).relative_to(ROOT))
        doc = load_json(rel)
        if not isinstance(doc, dict):
            fail(f"{rel}: rehearsal receipt is not an object")
        if str(doc.get("does_not_satisfy", "")) != "EXTERNAL_ALPHA_PROVEN":
            fail(f"{rel}: must declare does_not_satisfy=EXTERNAL_ALPHA_PROVEN")
        if doc.get("external_alpha_proven") is not False:
            fail(f"{rel}: external_alpha_proven must be false")
        if doc.get("synthetic") is not True:
            fail(f"{rel}: synthetic must be true")
        if str(doc.get("participant_class", "")) != "operator_controlled":
            fail(f"{rel}: participant_class must be operator_controlled")
        if doc.get("controlled_by_operator") is not True:
            fail(f"{rel}: controlled_by_operator must be true")
        if str(doc.get("claim", "")) == "EXTERNAL_ALPHA_PROVEN":
            fail(f"{rel}: must not carry claim EXTERNAL_ALPHA_PROVEN")


def qualifying_alpha_soak_proven(doc: Any) -> bool:
    """Backend-alpha soak: ≥3600 s, derived from pgx MaxConnLifetime.

    Twice the 30-minute pool lifetime so a live recycle is observed with
    samples on both sides. Does not, and must not, satisfy the Level B/C
    24-hour qualifying soak. Local 60 s / 300 s / 900 s receipts cannot pass.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    kind = str(doc.get("kind", ""))
    if kind not in {"go_closure_soak", "local_resilience_soak", "backend_alpha_soak"}:
        return False

    started = _parse_utc(str(doc.get("started_at", "")))
    finished = _parse_utc(str(doc.get("finished_at", "")))
    if started is None or finished is None or finished <= started:
        return False

    duration = doc.get("duration")
    if not isinstance(duration, dict):
        return False
    try:
        requested = int(duration["requested_seconds"])
        actual = int(duration["actual_seconds"])
        interval = int(duration["interval_seconds"])
    except (KeyError, TypeError, ValueError):
        return False
    if requested < ALPHA_SOAK_MINIMUM_SECONDS or actual < ALPHA_SOAK_MINIMUM_SECONDS:
        return False
    if interval < 15 or interval > 900:
        return False
    if actual < requested:
        return False
    wall = int((finished - started).total_seconds())
    if wall < actual or wall > actual + 300:
        return False

    # Refuse a receipt that only exists to tick 24 h on this path, and refuse
    # a receipt that claims the 24 h gate without meeting it — this path is
    # the derived hour, not a back door onto the Level B soak.
    qualification = doc.get("qualification")
    if isinstance(qualification, dict):
        if qualification.get("qualifies_for_24h_gate") is True:
            return False

    if kind == "go_closure_soak":
        assertions = doc.get("assertions")
        if not isinstance(assertions, dict) or not assertions:
            return False
        for key in (
            "two_agents_continuously_present",
            "no_page_alerts",
            "no_webhook_dead_letters",
            "no_control_restarts_or_recreates",
            "no_stuck_terminal_jobs",
            "bounded_resource_growth",
        ):
            if assertions.get(key) is not True:
                return False
        policy = doc.get("policy")
        if isinstance(policy, dict):
            if policy.get("stripe_live_mode") is True or policy.get("real_value") is True:
                return False
    else:
        bounds = doc.get("observed_bounds")
        if isinstance(bounds, dict):
            restarts = bounds.get("control_restart_count")
            if isinstance(restarts, dict):
                restart_max = restarts.get("max", 1)
            else:
                restart_max = restarts if isinstance(restarts, (int, float)) else 1
            oom = bounds.get("control_oom_samples", 1)
            if restart_max != 0 or oom != 0:
                return False

    if _has_secret_shaped(doc):
        return False
    return True


def external_alpha_participants_proven(doc: Any) -> bool:
    """EXTERNAL_ALPHA_PROVEN receipt. Synthetics cannot pass. Ever.

    A synthetic, disposable, harness, operator-owned, or operator-controlled
    identity appearing anywhere in `participants` makes the receipt fail.
    Completing P1-CANARY-REHEARSAL (synthetic buyers, operator Metal) cannot
    produce a document that passes this checker.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "external_alpha_participants":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if str(doc.get("claim", "")) != "EXTERNAL_ALPHA_PROVEN":
        return False
    if doc.get("includes_synthetic_participants") is True:
        return False
    if doc.get("secret_values_recorded") is not False:
        return False

    participants = doc.get("participants")
    if not isinstance(participants, list) or len(participants) < 2:
        return False

    buyer = None
    supplier = None
    for participant in participants:
        if not isinstance(participant, dict):
            return False
        # Structural: a synthetic identity may not appear in this receipt
        # at all, even with a renamed class.
        if _participant_is_synthetic(participant):
            return False
        if str(participant.get("participant_class", "")) != "independent_external":
            return False
        role = str(participant.get("role", "")).strip().lower()
        if role == "buyer":
            if buyer is not None:
                return False
            buyer = participant
        elif role == "supplier":
            if supplier is not None:
                return False
            supplier = participant
        else:
            return False

    if buyer is None or supplier is None:
        return False

    buyer_id = str(buyer.get("id", "")).strip()
    supplier_id = str(supplier.get("id", "")).strip()
    if not buyer_id or not supplier_id or buyer_id == supplier_id:
        return False
    if str(buyer.get("email", "")).strip().lower() == str(supplier.get("email", "")).strip().lower():
        return False
    if str(buyer.get("organization", "")).strip().lower() == str(
        supplier.get("organization", "")
    ).strip().lower():
        return False

    for participant in (buyer, supplier):
        attestation = participant.get("attestation")
        if not isinstance(attestation, dict):
            return False
        if attestation.get("not_synthetic") is not True:
            return False
        if attestation.get("independent_of_operator") is not True:
            return False
        if attestation.get("not_operator_employee_acting_as_fixture") is not True:
            return False
        if not _nonempty_text(participant.get("organization"), minimum=3):
            return False
        email = str(participant.get("email", "")).strip()
        if not _nonempty_text(email, minimum=6) or "@" not in email:
            return False

    inventory = doc.get("operator_controlled_device_ids")
    if not isinstance(inventory, list):
        return False
    inventory_ids = {str(item) for item in inventory}
    supplier_device = str(
        supplier.get("device_id") or supplier.get("worker_id") or ""
    ).strip()
    if not supplier_device or supplier_device in inventory_ids:
        return False

    # P0 expansion fields on the external supplier: without them this is
    # still an independently owned device under the standing prohibition.
    expansion = supplier.get("p0_expansion")
    if not isinstance(expansion, dict):
        return False
    for key in (
        "contract_recorded",
        "location_verified",
        "destination_pinned_egress",
        "attestation_recorded",
    ):
        if expansion.get(key) is not True:
            return False

    if _has_secret_shaped(doc):
        return False
    return True


def _gate_ids(gates: list[Any]) -> list[str]:
    return [str(g.get("id")) for g in gates if isinstance(g, dict) and g.get("id")]


def load_backend_alpha_gates() -> dict[str, Any]:
    path = ROOT / "ops" / "backend-alpha-gates.json"
    if not path.is_file():
        fail("ops/backend-alpha-gates.json is required")
    try:
        doc = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot load ops/backend-alpha-gates.json: {exc}")
    if not isinstance(doc, dict) or not isinstance(doc.get("gates"), list):
        fail("backend-alpha-gates.json must be an object with a gates array")
    return doc


def _require_classification_record(gate: Any, where: str) -> dict[str, Any]:
    if not isinstance(gate, dict):
        fail(f"{where}: each gate must be an object")
    gid = str(gate.get("id") or "").strip()
    if not gid:
        fail(f"{where}: gate missing id")
    classification = str(gate.get("classification") or "").strip()
    if classification not in ALLOWED_CLASSIFICATIONS:
        fail(f"{gid}: classification {gate.get('classification')!r} is not allowed")
    for field in (
        "harm",
        "reachable_harm_statement",
    ):
        text = gate.get(field)
        if not isinstance(text, str) or text.strip() != text or len(text.strip()) < 40:
            fail(f"{gid}: {field} must name the reachable harm in ≥40 characters")
    for flag in (
        "harm_reachable_in_this_alpha",
        "necessary_before_first_controlled_alpha_transaction",
        "later_production_or_public_launch_requirement",
    ):
        if gate.get(flag) is not True and gate.get(flag) is not False:
            fail(f"{gid}: {flag} must be a boolean")
    if "smaller_control" not in gate:
        fail(f"{gid}: smaller_control is required (string or null)")
    if gate.get("smaller_control") is not None and not isinstance(
        gate.get("smaller_control"), str
    ):
        fail(f"{gid}: smaller_control must be a string or null")
    # A later-level classification cannot claim the harm is a start-gate.
    if classification in {"POST_ALPHA", "PUBLIC_LAUNCH", "ENTERPRISE", "OBSOLETE"}:
        if gate.get("necessary_before_first_controlled_alpha_transaction") is True:
            fail(
                f"{gid}: {classification} cannot be necessary before the "
                "first controlled alpha transaction"
            )
    if classification == "ALPHA_BLOCKER":
        if gate.get("harm_reachable_in_this_alpha") is not True:
            fail(f"{gid}: ALPHA_BLOCKER must have a harm reachable on this alpha")
        if gate.get("necessary_before_first_controlled_alpha_transaction") is not True:
            fail(f"{gid}: ALPHA_BLOCKER must be necessary before the first transaction")
    return gate


def require_complete_classification(
    spec: dict[str, Any],
) -> dict[str, dict[str, Any]]:
    """Every DOMAIN_RECEIPTS row, every known P1, the P0, the derived soak,
    the external-proven claim, and every rescoped named obligation must
    have a classification record."""
    records = [_require_classification_record(g, "backend-alpha-gates") for g in spec["gates"]]
    by_id = {str(g["id"]): g for g in records}
    if len(by_id) != len(records):
        fail("backend-alpha-gates.json has duplicate gate ids")

    expected: set[str] = set()
    for domain_id, domain in DOMAIN_RECEIPTS.items():
        for relative, _checker, _points in domain["receipts"]:
            expected.add(f"receipt:{domain_id}:{relative}")
    for p1 in KNOWN_P1_IDS:
        expected.add(f"p1:{p1}")
    expected.add(f"p0:{P0_INDEPENDENT_SUPPLIER}")
    expected.add("soak:alpha-derived")
    expected.add("claim:EXTERNAL_ALPHA_PROVEN")
    expected.add(NAMED_REVIEWER_GATE_ID)

    missing = expected - set(by_id)
    extra = set(by_id) - expected
    if missing or extra:
        fail(
            "backend-alpha gate classification drift "
            f"missing={sorted(missing)} extra={sorted(extra)}"
        )
    return by_id


def receipt_passes(relative: str, checker: Callable[[Any], bool]) -> bool:
    if not _CANDIDATE_COMMIT:
        fail("candidate commit is not loaded")
    ok, _earned, _note = evaluate_receipt(
        relative, checker, 0, _CANDIDATE_COMMIT
    )
    return ok


def derive_backend_alpha_score(
    by_id: dict[str, dict[str, Any]],
) -> tuple[int, int, list[str]]:
    """Score only ALPHA_BLOCKER and ALPHA_CONTROL receipt rows.

    Level B's 100-point bar is untouched: this function never changes
    derive_domain_score and never drops a receipt from DOMAIN_RECEIPTS.
    Unbound receipts score zero here too.
    """
    if not _CANDIDATE_COMMIT:
        fail("candidate commit is not loaded")
    earned = 0
    possible = 0
    notes: list[str] = []
    for domain_id, domain in DOMAIN_RECEIPTS.items():
        for relative, checker, points in domain["receipts"]:
            record = by_id[f"receipt:{domain_id}:{relative}"]
            if record["classification"] not in ALPHA_SCORED:
                continue
            points = int(points)
            possible += points
            _ok, got, note = evaluate_receipt(
                relative, checker, points, _CANDIDATE_COMMIT
            )
            earned += got
            notes.append(f"{note} ({record['classification']})")
    return earned, possible, notes


def backend_alpha_blocker_receipts_open(by_id: dict[str, dict[str, Any]]) -> list[str]:
    open_ids: list[str] = []
    for domain_id, domain in DOMAIN_RECEIPTS.items():
        for relative, checker, _points in domain["receipts"]:
            gid = f"receipt:{domain_id}:{relative}"
            record = by_id[gid]
            if record["classification"] != "ALPHA_BLOCKER":
                continue
            if not receipt_passes(relative, checker):
                open_ids.append(gid)
    soak = load_json(ALPHA_SOAK_RECEIPT)
    soak_unbound = (
        isinstance(soak, dict)
        and _CANDIDATE_COMMIT is not None
        and receipt_unbound_reason(soak, _CANDIDATE_COMMIT) is not None
    )
    if not qualifying_alpha_soak_proven(soak) or soak_unbound:
        open_ids.append("soak:alpha-derived")
    return open_ids


# Each domain's possible points are fixed. earned is the sum of points for
# receipts that exist and pass their content check. Missing/failed => 0.
# External rows under evidence/external/ are wired so real operator artifacts
# can earn the reserved points; they evaluate to zero until those files exist
# and pass the content checks above.
DOMAIN_RECEIPTS: dict[str, dict[str, Any]] = {
    "source_and_ci": {
        "possible": 10,
        "receipts": [
            ("evidence/autonomous/registry-verification.json", status_in("PASS"), 4),
            ("evidence/autonomous/supply-chain.json", status_in("PASS"), 3),
            ("ops/authorization-matrix.json", auth_matrix_complete, 3),
        ],
    },
    "security": {
        "possible": 15,
        "receipts": [
            ("ops/authorization-matrix.json", auth_matrix_complete, 8),
            ("evidence/autonomous/technical-exercises.json", technical_break_glass, 6),
            (
                "evidence/external/staging-attack-rehearsal.json",
                external_staging_attack_proven,
                1,
            ),
        ],
    },
    "money_and_reconciliation": {
        "possible": 15,
        "receipts": [
            ("evidence/autonomous/payment-simulator.json", payment_simulated, 9),
            (
                "evidence/external/stripe-sandbox-matrix.json",
                stripe_sandbox_matrix_proven,
                6,
            ),
        ],
    },
    "lifecycle_and_concurrency": {
        "possible": 10,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_break_glass, 5),
            ("evidence/autonomous/local-restart-storm.json", status_in("PASS"), 5),
        ],
    },
    "artifacts_and_storage": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/logical-independent-restore.json", status_in("PASS"), 6),
            (
                "evidence/external/offsite-backup-verification.json",
                independent_offsite_backup_proven,
                2,
            ),
        ],
    },
    "agent_and_sandbox": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/hardware-characterization.json", status_in("PASS"), 8),
        ],
    },
    "database_and_recovery": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/logical-independent-restore.json", status_in("PASS"), 4),
            ("evidence/autonomous/local-rollback.json", status_in("PASS"), 3),
            (
                "evidence/external/offsite-independent-restore.json",
                external_offsite_restore_proven,
                1,
            ),
        ],
    },
    "deployment_and_rollback": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/staging-validation.json", status_in("PASS"), 2),
            ("evidence/autonomous/local-rollback.json", status_in("PASS"), 2),
            ("evidence/autonomous/local-restart-storm.json", status_in("PASS"), 1),
            ("evidence/autonomous/local-soak-60s.json", soak_clean, 0),
            (
                "evidence/autonomous/soak-requirement-derivation.json",
                soak_derivation_recorded,
                0,
            ),
            (
                "evidence/external/qualifying-soak-24h.json",
                qualifying_24h_soak_proven,
                3,
            ),
        ],
    },
    "observability_and_alerting": {
        "possible": 6,
        "receipts": [
            ("evidence/autonomous/alert-pipeline-simulation.json", status_in("PASS"), 3),
            ("evidence/autonomous/alert-page-simulation.json", status_in("PASS"), 2),
            ("evidence/autonomous/alert-delivery-r1.json", alert_delivery_proven, 1),
        ],
    },
    "privacy_and_data_governance": {
        "possible": 4,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_privacy, 3),
            (
                "evidence/external/privacy-qualified-approval.json",
                privacy_qualified_approval_proven,
                1,
            ),
        ],
    },
    "licensing_and_supply_chain": {
        "possible": 3,
        "receipts": [
            ("evidence/autonomous/supply-chain.json", status_in("PASS"), 2),
            (
                "evidence/external/licensing-provenance-approval.json",
                licensing_provenance_approval_proven,
                1,
            ),
        ],
    },
    "abuse_and_trust": {
        "possible": 2,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_tabletops, 1),
            (
                "evidence/external/staffed-abuse-route-or-tabletop.json",
                staffed_abuse_route_or_tabletop_proven,
                1,
            ),
        ],
    },
    "support_and_incident_response": {
        "possible": 1,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_tabletops, 1),
        ],
    },
    "website_and_buyer_usability": {
        "possible": 2,
        "receipts": [
            ("evidence/autonomous/website-validation.json", status_in("PASS", "PASS_AUTOMATED_BROWSER"), 2),
        ],
    },
}


def derive_domain_score(domain_id: str) -> tuple[int, int, list[str]]:
    if not _CANDIDATE_COMMIT:
        fail("candidate commit is not loaded")
    spec = DOMAIN_RECEIPTS[domain_id]
    possible = int(spec["possible"])
    earned = 0
    notes: list[str] = []
    for relative, checker, points in spec["receipts"]:
        _ok, got, note = evaluate_receipt(
            relative, checker, points, _CANDIDATE_COMMIT
        )
        notes.append(note)
        earned += got
    if earned > possible:
        fail(f"{domain_id}: derived earned {earned} exceeds possible {possible}")
    return earned, possible, notes


def main() -> None:
    candidate = load_candidate()
    print(f"readiness: candidate {candidate} ({_CANDIDATE_ORIGIN})", flush=True)
    assert_no_code_drift(candidate)
    print("readiness: code-drift OK (no code changes since candidate)", flush=True)

    readiness_path = ROOT / "ops" / "readiness.json"
    decision_path = ROOT / "ops" / "go-no-go.json"
    try:
        readiness = json.loads(readiness_path.read_text(encoding="utf-8"))
        decision = json.loads(decision_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot load readiness ledgers: {exc}")

    score = readiness.get("weighted_score") or {}
    domains = score.get("domains")
    if not isinstance(domains, list) or not domains:
        fail("weighted_score.domains must be a non-empty array")

    declared_ids = [item.get("id") for item in domains]
    expected_ids = list(DOMAIN_RECEIPTS.keys())
    if declared_ids != expected_ids:
        fail(
            "domain id axis mismatch "
            f"missing={sorted(set(expected_ids) - set(declared_ids))} "
            f"extra={sorted(set(declared_ids) - set(expected_ids))} "
            f"order_or_dup=declared_ids!=expected"
        )

    derived_total = 0
    possible_total = 0
    per_domain: list[str] = []
    for item in domains:
        domain_id = item["id"]
        declared_possible = int(item.get("possible", -1))
        earned, possible, notes = derive_domain_score(domain_id)
        if declared_possible != possible:
            fail(f"{domain_id}: possible {declared_possible} != receipt schedule {possible}")
        # Hand-typed earned is advisory only — never authoritative.
        advisory = item.get("earned")
        if advisory is not None and int(advisory) != earned:
            per_domain.append(
                f"{domain_id}: derived={earned}/{possible} "
                f"(advisory earned={advisory} ignored)"
            )
        else:
            per_domain.append(f"{domain_id}: derived={earned}/{possible}")
        for note in notes:
            if (
                "MISSING" in note
                or "FAILED" in note
                or "UNREADABLE" in note
                or "UNBOUND" in note
            ):
                per_domain.append(f"  - {note}")
        derived_total += earned
        possible_total += possible

    if possible_total != 100:
        fail(f"domain possibles sum to {possible_total}, want 100")

    # Show the derived picture before ledger-agreement checks so a
    # go-no-go mismatch still names which receipts scored zero.
    classification_spec = load_backend_alpha_gates()
    by_id = require_complete_classification(classification_spec)
    alpha_earned, alpha_possible, alpha_notes = derive_backend_alpha_score(by_id)
    print(f"readiness: derived {derived_total}/100 (candidate {candidate})", flush=True)
    print(f"readiness: backend_alpha derived {alpha_earned}/{alpha_possible}", flush=True)
    for line in per_domain:
        print(f"  {line}", flush=True)
    for note in alpha_notes:
        if (
            "MISSING" in note
            or "FAILED" in note
            or "UNREADABLE" in note
            or "UNBOUND" in note
        ):
            print(f"  - {note}", flush=True)

    if decision.get("readiness_score") != derived_total:
        fail(
            f"decision readiness_score {decision.get('readiness_score')} "
            f"!= receipt-derived total {derived_total}"
        )

    open_p0 = decision.get("open_p0")
    open_p1 = decision.get("open_p1")
    if not isinstance(open_p0, list) or not isinstance(open_p1, list):
        fail("open_p0 and open_p1 must be arrays")

    # Scoping must not delete a P1. Every known id stays in open_p1 or dropped_p1.
    dropped = decision.get("dropped_p1") or []
    if not isinstance(dropped, list):
        fail("dropped_p1 must be an array when present")
    dropped_ids: set[str] = set()
    for item in dropped:
        if isinstance(item, str):
            dropped_ids.add(item)
        elif isinstance(item, dict) and item.get("id"):
            dropped_ids.add(str(item["id"]))
    present_p1 = set(_gate_ids(open_p1)) | dropped_ids
    missing_p1 = KNOWN_P1_IDS - present_p1
    if missing_p1:
        fail(
            "P1 gate(s) removed without an open_p1 or dropped_p1 entry "
            f"(scoping must not delete a gate): {sorted(missing_p1)}"
        )

    severity = readiness.get("severity") or {}
    if severity.get("target_scope_open_p0") != len(open_p0):
        fail("open target-scope P0 count differs")
    if severity.get("target_scope_open_p1") != len(open_p1):
        fail("open target-scope P1 count differs")

    level_b = decision.get("decisions", {}).get("supervised_stripe_test_mode_private_canary")
    go_threshold = int(decision.get("go_threshold", score.get("go_threshold", 95)))
    if derived_total < go_threshold or open_p0 or open_p1:
        if level_b != "NO_GO":
            fail("an under-threshold or blocked Level B must be NO_GO")

    if decision.get("decisions", {}).get("live_money_or_public_launch") != "NO_GO_PROHIBITED":
        fail("live money/public launch must remain explicitly prohibited")

    if decision.get("machine_input_request") != "ops/go-closure-inputs.json":
        fail("decision must point to the single exact input request")

    # ---- backend alpha (additive; does not change the 100-point bar) ----
    levels = readiness.get("release_levels") or {}
    if not isinstance(levels, dict) or "level_backend_alpha" not in levels:
        fail("readiness.release_levels.level_backend_alpha is required")
    alpha_meta = levels.get("level_backend_alpha") or {}
    if not isinstance(alpha_meta, dict):
        fail("release_levels.level_backend_alpha must be an object")
    if alpha_meta.get("live_money") is not False or alpha_meta.get("public_access") is not False:
        fail("backend alpha live_money and public_access must remain false")
    if alpha_meta.get("website_required") is not False:
        fail("backend alpha website_required must be false")
    level_c_meta = levels.get("level_c_live_money") or {}
    if level_c_meta.get("decision") != "NO_GO_PROHIBITED":
        fail("readiness release_levels.level_c_live_money.decision must remain NO_GO_PROHIBITED")
    if level_c_meta.get("live_money") is not False or level_c_meta.get("public_access") is not False:
        fail("Level C live_money and public_access must remain false")
    level_b_meta = levels.get("level_b_private_canary") or {}
    if level_b_meta.get("live_money") is not False or level_b_meta.get("public_access") is not False:
        fail("Level B live_money and public_access must remain false")

    # Every open P1 / out-of-scope P0 must carry the same classification as
    # the gate file. Rescoping lives in one place.
    out_of_scope_p0 = decision.get("out_of_scope_p0") or []
    if not isinstance(out_of_scope_p0, list):
        fail("out_of_scope_p0 must be an array")
    oos_ids = set(_gate_ids(out_of_scope_p0))
    if P0_INDEPENDENT_SUPPLIER not in oos_ids:
        fail(
            "P0-INDEPENDENT-SUPPLIER must remain in out_of_scope_p0 "
            "(containment, not a closeable backend-alpha PASS)"
        )

    def _declared_classification(gate: Any, gid: str) -> str:
        if not isinstance(gate, dict):
            fail(f"{gid}: not an object")
        declared = str(gate.get("classification") or "").strip()
        if declared not in ALLOWED_CLASSIFICATIONS:
            fail(f"{gid}: missing or invalid classification")
        return declared

    for gate in open_p1:
        gid = str(gate.get("id") or "")
        declared = _declared_classification(gate, gid)
        record = by_id.get(f"p1:{gid}")
        if record is None:
            fail(f"{gid}: not classified in backend-alpha-gates.json")
        if record["classification"] != declared:
            fail(
                f"{gid}: go-no-go classification {declared} != "
                f"{record['classification']}"
            )
    for gate in out_of_scope_p0:
        gid = str(gate.get("id") or "")
        declared = _declared_classification(gate, gid)
        record = by_id.get(f"p0:{gid}")
        if record is None:
            fail(f"{gid}: not classified in backend-alpha-gates.json")
        if record["classification"] != declared:
            fail(
                f"{gid}: go-no-go classification {declared} != "
                f"{record['classification']}"
            )

    alpha_blocker_p1 = [
        g for g in open_p1 if str(g.get("classification")) == "ALPHA_BLOCKER"
    ]
    alpha_control_p1 = [
        g for g in open_p1 if str(g.get("classification")) == "ALPHA_CONTROL"
    ]
    open_blocker_receipts = backend_alpha_blocker_receipts_open(by_id)

    engineering_ready = "GO"
    if alpha_blocker_p1 or open_blocker_receipts:
        engineering_ready = "NO_GO"
    if open_p0:
        engineering_ready = "NO_GO"

    external_doc = load_json(EXTERNAL_PARTICIPANTS_RECEIPT)
    external_proven_ok = external_alpha_participants_proven(external_doc)
    # P0-INDEPENDENT-SUPPLIER stays in out_of_scope_p0 as the general
    # prohibition (do not enroll a random GPU). A passing participants
    # receipt already carries per-supplier P0 expansion fields, so the
    # claim can become GO for that named pair without dissolving the
    # prohibition. Synthetics still cannot pass the checker.
    external_proven = "GO" if external_proven_ok else "NO_GO"
    refuse_canary_rehearsal_as_external()

    # A synthetic-looking receipt that someone marked PASS must not be able
    # to force the claim to GO by editing only go-no-go.json.
    declared_engineering = decision.get("decisions", {}).get(
        "backend_alpha_engineering_ready"
    )
    declared_external = decision.get("decisions", {}).get("backend_alpha_external_proven")
    if declared_engineering != engineering_ready:
        fail(
            "decisions.backend_alpha_engineering_ready "
            f"{declared_engineering!r} != derived {engineering_ready!r}"
        )
    if declared_external != external_proven:
        fail(
            "decisions.backend_alpha_external_proven "
            f"{declared_external!r} != derived {external_proven!r}"
        )
    # Nested copy in go-no-go.backend_alpha, when present, must agree.
    nested = decision.get("backend_alpha") or {}
    if isinstance(nested, dict):
        if nested.get("ALPHA_ENGINEERING_READY") not in {None, engineering_ready}:
            fail("backend_alpha.ALPHA_ENGINEERING_READY disagrees with derived claim")
        if nested.get("EXTERNAL_ALPHA_PROVEN") not in {None, external_proven}:
            fail("backend_alpha.EXTERNAL_ALPHA_PROVEN disagrees with derived claim")
        if nested.get("external_participants_receipt") not in {
            None,
            EXTERNAL_PARTICIPANTS_RECEIPT,
        }:
            fail("backend_alpha.external_participants_receipt path drift")

    alpha_claims = alpha_meta.get("claims") or {}
    if not isinstance(alpha_claims, dict):
        fail("release_levels.level_backend_alpha.claims must be an object")
    if alpha_claims.get("ALPHA_ENGINEERING_READY") != engineering_ready:
        fail(
            "release_levels.level_backend_alpha.claims.ALPHA_ENGINEERING_READY "
            f"!= derived {engineering_ready}"
        )
    if alpha_claims.get("EXTERNAL_ALPHA_PROVEN") != external_proven:
        fail(
            "release_levels.level_backend_alpha.claims.EXTERNAL_ALPHA_PROVEN "
            f"!= derived {external_proven}"
        )
    if engineering_ready != "GO" and alpha_meta.get("decision") != "NO_GO":
        fail("release_levels.level_backend_alpha.decision must be NO_GO while blocked")

    declared_alpha = readiness.get("backend_alpha_score") or {}
    if not isinstance(declared_alpha, dict):
        fail("readiness.backend_alpha_score must be an object")
    if (
        declared_alpha.get("earned") != alpha_earned
        or declared_alpha.get("possible") != alpha_possible
    ):
        fail(
            "backend_alpha_score "
            f"{declared_alpha.get('earned')}/{declared_alpha.get('possible')} "
            f"!= derived {alpha_earned}/{alpha_possible}"
        )
    if declared_alpha.get("open_alpha_blocker_p1") != len(alpha_blocker_p1):
        fail("backend_alpha_score.open_alpha_blocker_p1 disagrees with derived open blockers")
    if declared_alpha.get("open_alpha_control_p1") != len(alpha_control_p1):
        fail("backend_alpha_score.open_alpha_control_p1 disagrees with derived open controls")
    if declared_alpha.get("ALPHA_ENGINEERING_READY") != engineering_ready:
        fail("backend_alpha_score.ALPHA_ENGINEERING_READY disagrees with derived claim")
    if declared_alpha.get("EXTERNAL_ALPHA_PROVEN") != external_proven:
        fail("backend_alpha_score.EXTERNAL_ALPHA_PROVEN disagrees with derived claim")

    # Full-bar Level B number is 84/100 on local receipts alone, 87/100
    # once the independent offsite backup/restore pair also passes.
    # Refuse a ledger that silently retargets the 100-point possible.
    if possible_total != 100:
        fail(f"domain possibles sum to {possible_total}, want 100")

    print(
        f"readiness: PASS ({derived_total}/100 derived, P0={len(open_p0)}, "
        f"P1={len(open_p1)}, Level B {level_b})"
    )
    print(
        f"  level_b: {derived_total}/100 derived "
        f"(threshold {go_threshold}/100), "
        f"P0={len(open_p0)}, P1={len(open_p1)}, decision {level_b}"
    )
    print(
        f"  backend_alpha: {alpha_earned}/{alpha_possible} derived, "
        f"ALPHA_BLOCKER_P1={len(alpha_blocker_p1)}, "
        f"ALPHA_ENGINEERING_READY {engineering_ready}, "
        f"EXTERNAL_ALPHA_PROVEN {external_proven}"
    )
    print(
        "  backend_alpha open ALPHA_BLOCKER P1: "
        + ", ".join(_gate_ids(alpha_blocker_p1) or ["(none)"])
    )
    if open_blocker_receipts:
        print(
            "  backend_alpha open ALPHA_BLOCKER receipts/soaks: "
            + ", ".join(open_blocker_receipts)
        )
    reviewer_record = by_id.get(NAMED_REVIEWER_GATE_ID)
    if reviewer_record is None:
        fail(f"{NAMED_REVIEWER_GATE_ID}: missing from backend-alpha-gates.json")
    if reviewer_record["classification"] != "PUBLIC_LAUNCH":
        fail(
            f"{NAMED_REVIEWER_GATE_ID}: classification "
            f"{reviewer_record['classification']} != PUBLIC_LAUNCH"
        )
    if not named_attack_reviewer_proven(load_json(NAMED_REVIEWER_RECEIPT)):
        print(
            f"  public_launch open {NAMED_REVIEWER_GATE_ID}: "
            "reviewer.name/organization unmet (requirement kept; not an alpha point)"
        )


if __name__ == "__main__":
    main()
