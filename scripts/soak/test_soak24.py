#!/usr/bin/env python3
"""Offline checks for the 24h HTTPS observer. No network. No fabricated PASS."""

from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(Path(__file__).resolve().parent))

import soak24  # noqa: E402

_SPEC = importlib.util.spec_from_file_location(
    "validate_readiness", ROOT / "scripts" / "validate-readiness.py"
)
assert _SPEC is not None and _SPEC.loader is not None
validate_readiness = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(validate_readiness)


def sample(
    *,
    seq: int,
    at: str,
    commit: str,
    payment_mode: str = "test",
    live_value: bool = False,
    ok: bool = True,
) -> dict:
    row = {
        "observed_at": at,
        "sequence": seq,
        "ok": ok,
        "host": "mercmerc.net",
        "commit": commit if ok else None,
        "payment_mode": payment_mode if ok else None,
        "live_value_movement": live_value if ok else None,
        "http": {"version_status": 200 if ok else 0, "readyz_status": 200 if ok else 0},
    }
    if ok:
        row["version"] = {"commit": commit, "modified": False}
        row["readyz"] = {
            "status": "ready",
            "payment_mode": payment_mode,
            "live_value_movement": live_value,
        }
    else:
        row["error"] = "probe failed"
    return row


def state(started_at: str = "2026-08-17T15:00:00Z") -> dict:
    return {
        "started_at": started_at,
        "started_epoch": soak24.parse_utc(started_at),
        "requested_seconds": 86400,
        "interval_seconds": 60,
        "expected_commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "harness_source_commit": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        "harness_build_digest": "c" * 64,
    }


class Soak24Tests(unittest.TestCase):
    def test_parse_utc_roundtrip(self) -> None:
        epoch = soak24.parse_utc("2026-08-17T15:31:39Z")
        self.assertIsNotNone(epoch)
        self.assertEqual(soak24.utc_now(epoch), "2026-08-17T15:31:39Z")

    def test_in_progress_never_pass_or_qualify(self) -> None:
        started = "2026-08-17T15:00:00Z"
        now = soak24.parse_utc(started) + 120
        rows = [
            sample(seq=1, at="2026-08-17T15:00:00Z", commit="a" * 40),
            sample(seq=2, at="2026-08-17T15:01:00Z", commit="a" * 40),
            sample(seq=3, at="2026-08-17T15:02:00Z", commit="a" * 40),
        ]
        receipt = soak24.derive_receipt(state(started), rows, now_epoch=now)
        self.assertEqual(receipt["status"], "IN_PROGRESS")
        self.assertNotEqual(receipt["status"], "PASS")
        self.assertIs(receipt["qualification"]["qualifies_for_24h_gate"], False)
        self.assertEqual(receipt["duration"]["elapsed_seconds"], 120)
        self.assertEqual(receipt["duration"]["observed_window_seconds"], 120)
        self.assertEqual(receipt["duration"]["samples"], 3)
        self.assertNotIn("finished_at", receipt)
        self.assertFalse(validate_readiness.qualifying_24h_soak_proven(receipt))

    def test_elapsed_is_wall_clock_not_invented(self) -> None:
        started = "2026-08-17T00:00:00Z"
        now = soak24.parse_utc(started) + 3600
        rows = [sample(seq=1, at="2026-08-17T00:00:00Z", commit="a" * 40)]
        receipt = soak24.derive_receipt(state(started), rows, now_epoch=now)
        self.assertEqual(receipt["duration"]["elapsed_seconds"], 3600)
        self.assertLess(receipt["duration"]["elapsed_seconds"], 86400)
        self.assertEqual(receipt["status"], "IN_PROGRESS")

    def test_finish_refused_before_86400(self) -> None:
        started = "2026-08-17T15:00:00Z"
        now = soak24.parse_utc(started) + 100
        rows = [sample(seq=1, at=started, commit="a" * 40)]
        with self.assertRaises(SystemExit):
            soak24.derive_receipt(
                state(started), rows, now_epoch=now, force_finish=True
            )

    def test_candidate_change_breaks_continuity(self) -> None:
        started = "2026-08-17T15:00:00Z"
        now = soak24.parse_utc(started) + 180
        rows = [
            sample(seq=1, at="2026-08-17T15:00:00Z", commit="a" * 40),
            sample(seq=2, at="2026-08-17T15:01:00Z", commit="a" * 40),
            sample(seq=3, at="2026-08-17T15:02:00Z", commit="b" * 40),
        ]
        receipt = soak24.derive_receipt(state(started), rows, now_epoch=now)
        self.assertEqual(receipt["status"], "CANDIDATE_CHANGED")
        self.assertTrue(receipt["candidate"]["changed"])
        self.assertEqual(receipt["candidate"]["continuity"], "broken_redeploy")
        self.assertEqual(receipt["candidate"]["first_changed_at"], "2026-08-17T15:02:00Z")
        self.assertIs(receipt["qualification"]["qualifies_for_24h_gate"], False)
        self.assertFalse(validate_readiness.qualifying_24h_soak_proven(receipt))

    def test_policy_left_test(self) -> None:
        started = "2026-08-17T15:00:00Z"
        now = soak24.parse_utc(started) + 60
        rows = [
            sample(seq=1, at=started, commit="a" * 40),
            sample(
                seq=2,
                at="2026-08-17T15:01:00Z",
                commit="a" * 40,
                payment_mode="live",
            ),
        ]
        receipt = soak24.derive_receipt(state(started), rows, now_epoch=now)
        self.assertEqual(receipt["status"], "POLICY_LEFT_TEST")
        self.assertTrue(receipt["payment"]["left_test_envelope"])
        self.assertIs(receipt["qualification"]["qualifies_for_24h_gate"], False)

    def test_window_complete_still_refuses_gate(self) -> None:
        started = "2026-08-16T15:00:00Z"
        now = soak24.parse_utc(started) + 86400
        rows = [
            sample(seq=1, at="2026-08-16T15:00:00Z", commit="a" * 40),
            sample(seq=2, at="2026-08-17T15:00:00Z", commit="a" * 40),
        ]
        receipt = soak24.derive_receipt(state(started), rows, now_epoch=now)
        self.assertEqual(receipt["status"], "OBSERVED_WINDOW_COMPLETE")
        self.assertNotEqual(receipt["status"], "PASS")
        self.assertIs(receipt["qualification"]["qualifies_for_24h_gate"], False)
        self.assertEqual(
            receipt["qualification"]["reason"],
            "https_observer_window_complete_not_go_closure_schema",
        )
        self.assertEqual(receipt["duration"]["elapsed_seconds"], 86400)
        self.assertIn("finished_at", receipt)
        self.assertFalse(validate_readiness.qualifying_24h_soak_proven(receipt))

    def test_hand_edited_pass_still_refused_by_readiness(self) -> None:
        started = "2026-08-16T15:00:00Z"
        now = soak24.parse_utc(started) + 86400
        rows = [
            sample(seq=1, at="2026-08-16T15:00:00Z", commit="a" * 40),
            sample(seq=2, at="2026-08-17T15:00:00Z", commit="a" * 40),
        ]
        receipt = soak24.derive_receipt(state(started), rows, now_epoch=now)
        receipt["status"] = "PASS"
        receipt["qualification"]["qualifies_for_24h_gate"] = True
        self.assertFalse(validate_readiness.qualifying_24h_soak_proven(receipt))

    def test_write_receipt_refuses_pass(self) -> None:
        with self.assertRaises(SystemExit):
            soak24.write_receipt({"status": "PASS", "qualification": {"qualifies_for_24h_gate": False}})
        with self.assertRaises(SystemExit):
            soak24.write_receipt(
                {"status": "IN_PROGRESS", "qualification": {"qualifies_for_24h_gate": True}}
            )


if __name__ == "__main__":
    raise SystemExit(unittest.main(verbosity=2))
