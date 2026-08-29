#!/usr/bin/env python3
"""Qualifying 24-hour HTTPS observer soak against persistent staging.

Samples https://mercmerc.net /version and /readyz on a fixed interval,
records the deployed commit and payment_mode on every sample, and keeps
an honest receipt at evidence/external/qualifying-soak-24h.json.

This harness never writes status=PASS and never sets
qualification.qualifies_for_24h_gate=true. The Level B/C 3-point gate
(ops/scripts/validate-readiness.py::qualifying_24h_soak_proven) requires a
schema-v2 go_closure_soak PASS after a real 86400 s window; an in-progress
or HTTPS-observer-complete receipt is refused, which is the correct
outcome until that bar is actually met.

A parallel-lane redeploy is recorded as candidate.changed=true. Continuity
is not pretended.

Usage (from repo root):

  python3 ops/scripts/soak/soak24.py start
  python3 ops/scripts/soak/soak24.py status
  python3 ops/scripts/soak/soak24.py stamp
  python3 ops/scripts/soak/soak24.py resume
  python3 ops/scripts/soak/soak24.py finish
"""

from __future__ import annotations

import datetime as dt
import errno
import fcntl
import hashlib
import json
import os
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[3]
HARNESS = "ops/scripts/soak/soak24.py"
HARNESS_PATH = ROOT / "ops/scripts" / "soak" / "soak24.py"
HOST = "mercmerc.net"
BASE_URL = f"https://{HOST}"
REQUESTED_SECONDS = 86400
INTERVAL_SECONDS = 60
KIND = "qualifying_24h_https_observer"
SCHEMA_VERSION = 1
TMUX_SESSION = "merc-qualifying-soak-24h"
USER_AGENT = "merc-qualifying-soak-24h"

RECEIPT_REL = "evidence/external/qualifying-soak-24h.json"
SAMPLES_REL = "evidence/external/qualifying-soak-24h-samples.jsonl"
RECEIPT_PATH = ROOT / RECEIPT_REL
SAMPLES_PATH = ROOT / SAMPLES_REL
SAMPLES_BINDING_PATH = Path(str(SAMPLES_PATH) + ".binding.json")
RUN_DIR = ROOT / "ops/scripts" / "soak" / "run"
STATE_PATH = RUN_DIR / "state.json"
PID_PATH = RUN_DIR / "soak24.pid"
LOG_PATH = RUN_DIR / "soak24.log"
LOCK_PATH = RUN_DIR / "soak24.lock"

PROBE_TIMEOUT_S = 20
MAX_GAP_SLACK_S = 30


def die(message: str, code: int = 1) -> None:
    print(f"soak24: FAIL: {message}", file=sys.stderr)
    raise SystemExit(code)


def utc_now(epoch: float | None = None) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(epoch))


def parse_utc(value: str) -> int | None:
    if not isinstance(value, str):
        return None
    try:
        parsed = dt.datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ")
    except (TypeError, ValueError):
        return None
    return int(parsed.replace(tzinfo=dt.timezone.utc).timestamp())


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 16), b""):
            digest.update(chunk)
    return digest.hexdigest()


def git_head() -> str:
    try:
        out = subprocess.check_output(
            ["git", "-C", str(ROOT), "rev-parse", "HEAD"],
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except (subprocess.CalledProcessError, FileNotFoundError, OSError) as exc:
        die(f"cannot resolve HEAD: {exc}")
    head = out.strip()
    if len(head) != 40 or any(c not in "0123456789abcdef" for c in head):
        die(f"HEAD is not a 40-char hex commit: {head!r}")
    return head


def atomic_write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp.{os.getpid()}")
    tmp.write_text(text, encoding="utf-8")
    os.replace(tmp, path)


def load_json(path: Path) -> Any:
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def pid_is_this_harness(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except OSError:
        return False
    cmdline_path = Path(f"/proc/{pid}/cmdline")
    if cmdline_path.is_file():
        try:
            cmdline = cmdline_path.read_bytes().replace(b"\x00", b" ").decode(
                "utf-8", "replace"
            )
        except OSError:
            cmdline = ""
        return "soak24.py" in cmdline
    try:
        out = subprocess.check_output(
            ["ps", "-p", str(pid), "-o", "command="],
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        return False
    return "soak24.py" in out


def running_pid() -> int | None:
    if not PID_PATH.is_file():
        return None
    try:
        pid = int(PID_PATH.read_text(encoding="utf-8").strip())
    except (OSError, ValueError):
        return None
    if pid_is_this_harness(pid):
        return pid
    return None


def acquire_lock():
    RUN_DIR.mkdir(parents=True, exist_ok=True)
    handle = LOCK_PATH.open("a+")
    try:
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as exc:
        handle.close()
        if exc.errno in {errno.EACCES, errno.EAGAIN}:
            die("another soak24 process holds the lock")
        raise
    return handle


def fetch_json(path: str) -> tuple[int, dict[str, Any] | None, str | None]:
    req = urllib.request.Request(
        f"{BASE_URL}{path}",
        headers={"User-Agent": USER_AGENT},
        method="GET",
    )
    try:
        with urllib.request.urlopen(
            req, timeout=PROBE_TIMEOUT_S, context=ssl.create_default_context()
        ) as resp:
            status = int(resp.status)
            raw = resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        return int(exc.code), None, f"{path}: HTTP {exc.code}"
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return 0, None, f"{path}: {exc}"
    if status != 200:
        return status, None, f"{path}: HTTP {status}"
    try:
        body = json.loads(raw)
    except json.JSONDecodeError as exc:
        return status, None, f"{path}: invalid JSON ({exc})"
    if not isinstance(body, dict):
        return status, None, f"{path}: JSON is not an object"
    return status, body, None


def take_probe() -> dict[str, Any]:
    observed_at = utc_now()
    v_status, version, v_err = fetch_json("/version")
    r_status, ready, r_err = fetch_json("/readyz")
    errors = [e for e in (v_err, r_err) if e]
    ok = not errors and version is not None and ready is not None
    commit = None
    payment_mode = None
    live_value = None
    if isinstance(version, dict):
        c = version.get("commit")
        if isinstance(c, str) and c:
            commit = c
    if isinstance(ready, dict):
        pm = ready.get("payment_mode")
        if isinstance(pm, str) and pm:
            payment_mode = pm
        live_value = ready.get("live_value_movement")
    sample: dict[str, Any] = {
        "observed_at": observed_at,
        "ok": ok,
        "host": HOST,
        "commit": commit,
        "payment_mode": payment_mode,
        "live_value_movement": live_value,
        "http": {"version_status": v_status, "readyz_status": r_status},
    }
    if ok:
        sample["version"] = version
        sample["readyz"] = ready
    else:
        sample["error"] = "; ".join(errors)
    return sample


def load_samples() -> list[dict[str, Any]]:
    if not SAMPLES_PATH.is_file():
        return []
    rows: list[dict[str, Any]] = []
    with SAMPLES_PATH.open("rb") as handle:
        for line_number, raw in enumerate(handle, 1):
            if not raw.strip():
                die(f"{SAMPLES_REL}:{line_number}: blank line")
            try:
                row = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                die(f"{SAMPLES_REL}:{line_number}: {exc}")
            if not isinstance(row, dict):
                die(f"{SAMPLES_REL}:{line_number}: not an object")
            rows.append(row)
    return rows


def append_sample(sample: dict[str, Any]) -> dict[str, Any]:
    SAMPLES_PATH.parent.mkdir(parents=True, exist_ok=True)
    existing = load_samples()
    sample = dict(sample)
    sample["sequence"] = len(existing) + 1
    line = json.dumps(sample, separators=(",", ":"), ensure_ascii=False) + "\n"
    with SAMPLES_PATH.open("a", encoding="utf-8") as handle:
        handle.write(line)
    return sample


def identity(state: dict[str, Any]) -> dict[str, Any]:
    return {
        "source_commit": {"value": str(state["harness_source_commit"])},
        "build_digest": {"value": str(state["harness_build_digest"])},
        "model_artifact_digest": {"na": "soak does not load model weights"},
        "image_digest": {
            "na": "https observer does not inspect the control container image"
        },
        "harness_revision": {"value": HARNESS},
        "corpus_digest": {"na": "no external corpus"},
        "exact_config": {
            "value": (
                f"{KIND} requested={int(state['requested_seconds'])} "
                f"interval={int(state['interval_seconds'])} host={HOST}"
            )
        },
        "raw_samples": {"value": SAMPLES_REL},
    }


def write_samples_sidecar(state: dict[str, Any]) -> None:
    doc = {
        "schema_version": 1,
        "kind": "evidence_binding_sidecar",
        "target": SAMPLES_REL,
        "binding_status": "BOUND",
        "producer_identity": identity(state),
        "note": "sidecar for non-object evidence written via bound path",
    }
    atomic_write(
        SAMPLES_BINDING_PATH,
        json.dumps(doc, indent=2, ensure_ascii=False) + "\n",
    )


def classify(
    *,
    elapsed: int,
    requested: int,
    candidate_changed: bool,
    policy_left_test: bool,
    sample_count: int,
) -> tuple[str, str]:
    """Return (status, qualification_reason). Never PASS. Never qualifies."""
    if sample_count < 1:
        return "FAILED", "no_samples_recorded"
    if policy_left_test:
        return "POLICY_LEFT_TEST", "payment_envelope_left_test_mode"
    if candidate_changed:
        return "CANDIDATE_CHANGED", "deployed_commit_changed_mid_soak"
    if elapsed < requested:
        return "IN_PROGRESS", "in_progress_elapsed_below_86400"
    return (
        "OBSERVED_WINDOW_COMPLETE",
        "https_observer_window_complete_not_go_closure_schema",
    )


def derive_receipt(
    state: dict[str, Any],
    samples: list[dict[str, Any]],
    *,
    now_epoch: int | None = None,
    force_finish: bool = False,
) -> dict[str, Any]:
    now = int(now_epoch if now_epoch is not None else time.time())
    started_at = str(state["started_at"])
    started_epoch = parse_utc(started_at)
    if started_epoch is None:
        die("state.started_at is not RFC3339 Z")
    if now < started_epoch:
        die("clock is behind started_at; refusing to invent elapsed time")

    requested = int(state["requested_seconds"])
    interval = int(state["interval_seconds"])
    expected_commit = state.get("expected_commit")
    expected_commit = expected_commit if isinstance(expected_commit, str) else None

    observed_commits: list[str] = []
    first_changed_at = None
    policy_left_at = None
    last_ok = None
    first_ok = None
    ok_count = 0
    fail_count = 0
    last_epoch = started_epoch
    max_gap = 0
    modified_seen = False

    for sample in samples:
        observed = parse_utc(str(sample.get("observed_at", "")))
        if observed is None:
            continue
        gap = observed - last_epoch
        if gap > max_gap:
            max_gap = gap
        last_epoch = observed
        if sample.get("ok") is True:
            ok_count += 1
            if first_ok is None:
                first_ok = sample
            last_ok = sample
            commit = sample.get("commit")
            if isinstance(commit, str) and commit and commit not in observed_commits:
                observed_commits.append(commit)
            version = sample.get("version")
            if isinstance(version, dict) and version.get("modified") is True:
                modified_seen = True
            payment_mode = sample.get("payment_mode")
            live_value = sample.get("live_value_movement")
            left = payment_mode != "test" or live_value is not False
            if left and policy_left_at is None:
                policy_left_at = sample.get("observed_at")
        else:
            fail_count += 1

    if expected_commit is None and first_ok is not None:
        commit = first_ok.get("commit")
        if isinstance(commit, str) and commit:
            expected_commit = commit

    candidate_changed = bool(
        expected_commit
        and any(c != expected_commit for c in observed_commits)
    )
    first_changed_at = None
    if candidate_changed:
        for sample in samples:
            if sample.get("ok") is True and sample.get("commit") != expected_commit:
                first_changed_at = sample.get("observed_at")
                break

    policy_left_test = policy_left_at is not None
    elapsed = now - started_epoch
    # Never report more elapsed than the clock has actually advanced.
    if elapsed < 0:
        die("negative elapsed")

    first_obs = parse_utc(str(samples[0]["observed_at"])) if samples else None
    last_obs = parse_utc(str(samples[-1]["observed_at"])) if samples else None
    observed_window = 0
    if first_obs is not None and last_obs is not None and last_obs >= first_obs:
        observed_window = last_obs - first_obs

    status, reason = classify(
        elapsed=elapsed,
        requested=requested,
        candidate_changed=candidate_changed,
        policy_left_test=policy_left_test,
        sample_count=len(samples),
    )
    if force_finish and status == "IN_PROGRESS":
        # finish is explicit and still refuses a PASS before the window.
        die(
            f"finish refused: elapsed={elapsed}s < requested={requested}s; "
            "receipt stays IN_PROGRESS"
        )

    continuity = "uninterrupted"
    if candidate_changed:
        continuity = "broken_redeploy"
    elif policy_left_test:
        continuity = "broken_payment_envelope"
    elif max_gap > interval + MAX_GAP_SLACK_S:
        continuity = "sampling_gap"

    last_version = last_ok.get("version") if last_ok else None
    last_readyz = last_ok.get("readyz") if last_ok else None

    receipt: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "kind": KIND,
        "status": status,
        "mode": "qualifying",
        "started_at": started_at,
        "updated_at": utc_now(now),
        "host": HOST,
        "base_url": BASE_URL,
        "expected_commit": expected_commit,
        "duration": {
            "requested_seconds": requested,
            "elapsed_seconds": elapsed,
            "observed_window_seconds": observed_window,
            "interval_seconds": interval,
            "samples": len(samples),
            "ok_samples": ok_count,
            "failed_samples": fail_count,
            "max_inter_sample_gap_seconds": max_gap,
        },
        "qualification": {
            "qualifies_for_24h_gate": False,
            "reason": reason,
        },
        "candidate": {
            "expected_commit": expected_commit,
            "observed_commits": observed_commits,
            "changed": candidate_changed,
            "first_changed_at": first_changed_at,
            "modified_seen": modified_seen,
            "continuity": continuity,
        },
        "payment": {
            "required_payment_mode": "test",
            "required_live_value_movement": False,
            "left_test_envelope": policy_left_test,
            "first_left_at": policy_left_at,
        },
        "observer": {
            "kind": "https_public_tls",
            "paths": ["/version", "/readyz"],
            "harness": HARNESS,
            "tmux_session": TMUX_SESSION,
        },
        "commands": {
            "status": "python3 ops/scripts/soak/soak24.py status",
            "stamp": "python3 ops/scripts/soak/soak24.py stamp",
            "resume": "python3 ops/scripts/soak/soak24.py resume",
            "finish": "python3 ops/scripts/soak/soak24.py finish",
        },
        "samples": {
            "path": SAMPLES_REL,
        },
        "last_version": last_version,
        "last_readyz": last_readyz,
        "observed_bounds": {
            "control_restart_count": None,
            "control_oom_samples": None,
            "note": "https observer does not inspect docker restart or OOM counters",
        },
        "secret_values_recorded": False,
        "policy": {
            "stripe_test_mode": True,
            "stripe_live_mode": False,
            "real_value": False,
            "secret_values_recorded": False,
        },
        "producer_identity": identity(state),
        "binding_status": "BOUND",
    }
    # finished_at is omitted until the requested window has actually elapsed.
    # Never write a future or invented completion timestamp.
    if status == "OBSERVED_WINDOW_COMPLETE" and now >= started_epoch + requested:
        receipt["finished_at"] = utc_now(now)
    return receipt


def write_receipt(receipt: dict[str, Any]) -> None:
    if receipt.get("status") == "PASS":
        die("internal error: harness attempted to write status=PASS")
    qual = receipt.get("qualification")
    if isinstance(qual, dict) and qual.get("qualifies_for_24h_gate") is True:
        die("internal error: harness attempted to set qualifies_for_24h_gate=true")
    atomic_write(
        RECEIPT_PATH, json.dumps(receipt, indent=2, ensure_ascii=False) + "\n"
    )


def stamp(
    state: dict[str, Any],
    *,
    now_epoch: int | None = None,
    force_finish: bool = False,
) -> dict[str, Any]:
    samples = load_samples()
    receipt = derive_receipt(
        state, samples, now_epoch=now_epoch, force_finish=force_finish
    )
    write_receipt(receipt)
    write_samples_sidecar(state)
    return receipt


def default_state(*, requested: int, interval: int) -> dict[str, Any]:
    started_epoch = int(time.time())
    return {
        "schema_version": 1,
        "kind": "qualifying_24h_soak_state",
        "started_at": utc_now(started_epoch),
        "started_epoch": started_epoch,
        "requested_seconds": requested,
        "interval_seconds": interval,
        "host": HOST,
        "expected_commit": None,
        "harness_source_commit": git_head(),
        "harness_build_digest": sha256_file(HARNESS_PATH),
        "tmux_session": TMUX_SESSION,
    }


def state_from_receipt(receipt: dict[str, Any]) -> dict[str, Any]:
    started_at = str(receipt.get("started_at", ""))
    started_epoch = parse_utc(started_at)
    if started_epoch is None:
        die("receipt.started_at is not RFC3339 Z; cannot rebuild state")
    duration = receipt.get("duration") if isinstance(receipt.get("duration"), dict) else {}
    pi = receipt.get("producer_identity") if isinstance(receipt.get("producer_identity"), dict) else {}
    source = pi.get("source_commit") if isinstance(pi.get("source_commit"), dict) else {}
    return {
        "schema_version": 1,
        "kind": "qualifying_24h_soak_state",
        "started_at": started_at,
        "started_epoch": started_epoch,
        "requested_seconds": int(duration.get("requested_seconds") or REQUESTED_SECONDS),
        "interval_seconds": int(duration.get("interval_seconds") or INTERVAL_SECONDS),
        "host": HOST,
        "expected_commit": receipt.get("expected_commit"),
        "harness_source_commit": str(source.get("value") or git_head()),
        "harness_build_digest": sha256_file(HARNESS_PATH),
        "tmux_session": TMUX_SESSION,
        "rebuilt_from_receipt": True,
    }


def load_state() -> dict[str, Any] | None:
    doc = load_json(STATE_PATH)
    if isinstance(doc, dict) and doc.get("started_at"):
        return doc
    receipt = load_json(RECEIPT_PATH)
    if isinstance(receipt, dict) and receipt.get("started_at"):
        rebuilt = state_from_receipt(receipt)
        save_state(rebuilt)
        return rebuilt
    return None


def save_state(state: dict[str, Any]) -> None:
    atomic_write(STATE_PATH, json.dumps(state, indent=2, ensure_ascii=False) + "\n")


def ensure_expected_commit(state: dict[str, Any], sample: dict[str, Any]) -> None:
    if state.get("expected_commit"):
        return
    commit = sample.get("commit")
    if sample.get("ok") is True and isinstance(commit, str) and commit:
        state["expected_commit"] = commit
        save_state(state)


def record_sample(state: dict[str, Any]) -> dict[str, Any]:
    sample = take_probe()
    stored = append_sample(sample)
    ensure_expected_commit(state, stored)
    receipt = stamp(state)
    commit = stored.get("commit") or "-"
    payment = stored.get("payment_mode") or "-"
    flag = "ok" if stored.get("ok") else "FAIL"
    print(
        f"soak24: sample {stored['sequence']} {stored['observed_at']} "
        f"{flag} commit={commit} payment_mode={payment} "
        f"status={receipt['status']} elapsed={receipt['duration']['elapsed_seconds']}s",
        flush=True,
    )
    return receipt


def print_status(receipt: dict[str, Any] | None = None) -> int:
    state = load_state()
    if receipt is None and state is not None:
        receipt = stamp(state)
    elif receipt is None:
        receipt = load_json(RECEIPT_PATH) if RECEIPT_PATH.is_file() else None
        if not isinstance(receipt, dict):
            print("soak24: NOT_STARTED")
            return 1
    pid = running_pid()
    duration = receipt.get("duration") if isinstance(receipt.get("duration"), dict) else {}
    candidate = receipt.get("candidate") if isinstance(receipt.get("candidate"), dict) else {}
    qual = receipt.get("qualification") if isinstance(receipt.get("qualification"), dict) else {}
    print(
        json.dumps(
            {
                "status": receipt.get("status"),
                "started_at": receipt.get("started_at"),
                "updated_at": receipt.get("updated_at"),
                "elapsed_seconds": duration.get("elapsed_seconds"),
                "requested_seconds": duration.get("requested_seconds"),
                "interval_seconds": duration.get("interval_seconds"),
                "samples": duration.get("samples"),
                "ok_samples": duration.get("ok_samples"),
                "expected_commit": receipt.get("expected_commit"),
                "candidate_changed": candidate.get("changed"),
                "continuity": candidate.get("continuity"),
                "qualifies_for_24h_gate": qual.get("qualifies_for_24h_gate"),
                "qualification_reason": qual.get("reason"),
                "running_pid": pid,
                "receipt": RECEIPT_REL,
                "samples_path": SAMPLES_REL,
                "commands": receipt.get("commands"),
            },
            indent=2,
            ensure_ascii=False,
        )
    )
    return 0


def tmux_alive() -> bool:
    try:
        subprocess.check_call(
            ["tmux", "has-session", "-t", TMUX_SESSION],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        return False
    try:
        dead = subprocess.check_output(
            ["tmux", "list-panes", "-t", TMUX_SESSION, "-F", "#{pane_dead}"],
            stderr=subprocess.DEVNULL,
            text=True,
        ).strip().splitlines()
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        return False
    return bool(dead) and dead[0] == "0"


def launch_daemon() -> None:
    RUN_DIR.mkdir(parents=True, exist_ok=True)
    if running_pid() is not None or tmux_alive():
        return
    if shutil_which("tmux"):
        subprocess.check_call(
            ["tmux", "new-session", "-d", "-s", TMUX_SESSION],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        subprocess.check_call(
            [
                "tmux",
                "set-window-option",
                "-t",
                TMUX_SESSION,
                "remain-on-exit",
                "on",
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        cmd = f"cd {ROOT} && exec {sys.executable} {HARNESS} run"
        subprocess.check_call(
            ["tmux", "respawn-pane", "-k", "-t", f"{TMUX_SESSION}:0.0", cmd]
        )
        print(f"soak24: launched tmux session {TMUX_SESSION}", flush=True)
        return
    log = LOG_PATH.open("a", encoding="utf-8")
    proc = subprocess.Popen(
        [sys.executable, str(HARNESS_PATH), "run"],
        cwd=str(ROOT),
        stdout=log,
        stderr=log,
        start_new_session=True,
    )
    PID_PATH.write_text(str(proc.pid) + "\n", encoding="utf-8")
    print(f"soak24: launched detached pid {proc.pid}", flush=True)


def shutil_which(name: str) -> bool:
    for directory in os.environ.get("PATH", "").split(os.pathsep):
        candidate = Path(directory) / name
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return True
    return False


def cmd_run() -> int:
    state = load_state()
    if state is None:
        die("run requires an existing state; use start")
    PID_PATH.write_text(str(os.getpid()) + "\n", encoding="utf-8")
    requested = int(state["requested_seconds"])
    interval = int(state["interval_seconds"])
    started_epoch = int(state["started_epoch"])
    print(
        f"soak24: RUN started_at={state['started_at']} "
        f"requested={requested} interval={interval} host={HOST}",
        flush=True,
    )
    # Align to the next slot after existing samples. Never invent skipped rows.
    existing = len(load_samples())
    slot = existing
    # Do not burst-fill missed slots after a crash. One real sample at
    # resume, then return to the fixed cadence. Missed rows stay absent.
    now = int(time.time())
    current_slot = max(0, (now - started_epoch) // interval)
    if slot < current_slot:
        slot = current_slot
    while True:
        now = int(time.time())
        elapsed = now - started_epoch
        if elapsed >= requested and existing >= 1:
            receipt = stamp(state, now_epoch=now)
            print(
                f"soak24: window reached elapsed={elapsed}s status={receipt['status']}",
                flush=True,
            )
            return 0
        target = started_epoch + slot * interval
        if now < target:
            time.sleep(min(target - now, max(1, requested - elapsed)))
            continue
        receipt = record_sample(state)
        existing = int(receipt["duration"]["samples"])
        slot = existing
        if receipt["status"] in {
            "OBSERVED_WINDOW_COMPLETE",
        }:
            return 0


def cmd_start(resume_only: bool = False) -> int:
    lock = acquire_lock()
    try:
        state = load_state()
        if resume_only and state is None:
            die("resume refused: no existing soak state")
        if state is None:
            state = default_state(
                requested=REQUESTED_SECONDS, interval=INTERVAL_SECONDS
            )
            save_state(state)
            print(
                f"soak24: START {state['started_at']} "
                f"requested={REQUESTED_SECONDS} interval={INTERVAL_SECONDS} "
                f"host={HOST}",
                flush=True,
            )
        else:
            print(
                f"soak24: RESUME started_at={state['started_at']} "
                f"expected_commit={state.get('expected_commit')}",
                flush=True,
            )
        if not load_samples():
            record_sample(state)
        else:
            stamp(state)
        if running_pid() is None and not tmux_alive():
            launch_daemon()
        else:
            print("soak24: sampler already running", flush=True)
        return print_status()
    finally:
        lock.close()


def cmd_stamp() -> int:
    state = load_state()
    if state is None:
        die("no soak state; use start")
    stamp(state)
    return print_status()


def cmd_finish() -> int:
    state = load_state()
    if state is None:
        die("no soak state; use start")
    stamp(state, force_finish=True)
    return print_status()


def usage() -> None:
    print(
        "usage: python3 ops/scripts/soak/soak24.py "
        "start|status|stamp|resume|finish|run",
        file=sys.stderr,
    )


def main(argv: list[str] | None = None) -> int:
    args = list(sys.argv[1:] if argv is None else argv)
    if not args:
        usage()
        return print_status()
    cmd = args[0]
    if cmd in {"-h", "--help"}:
        print(__doc__)
        return 0
    if cmd == "start":
        return cmd_start(resume_only=False)
    if cmd == "resume":
        return cmd_start(resume_only=True)
    if cmd == "status":
        return print_status()
    if cmd == "stamp":
        return cmd_stamp()
    if cmd == "finish":
        return cmd_finish()
    if cmd == "run":
        return cmd_run()
    usage()
    return 2


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("soak24: interrupted", file=sys.stderr)
        raise SystemExit(130)
