#!/usr/bin/env python3
"""Validate scope separation, weighted scoring, and fail-closed GO decision."""

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
readiness = json.loads((ROOT / "ops" / "readiness.json").read_text())
decision = json.loads((ROOT / "ops" / "go-no-go.json").read_text())


def require(condition: bool, message: str) -> None:
    if not condition:
        print(f"readiness: FAIL: {message}", file=sys.stderr)
        raise SystemExit(1)


score = readiness["weighted_score"]
earned = sum(item["earned"] for item in score["domains"])
possible = sum(item["possible"] for item in score["domains"])
require(earned == score["earned"] and possible == score["possible"] == 100,
        "domain scores do not sum to the declared 100-point score")
require(decision["readiness_score"] == earned, "decision and readiness scores differ")
open_p0 = decision["open_p0"]
open_p1 = decision["open_p1"]
require(readiness["severity"]["target_scope_open_p0"] == len(open_p0),
        "open target-scope P0 count differs")
require(readiness["severity"]["target_scope_open_p1"] == len(open_p1),
        "open target-scope P1 count differs")
level_b = decision["decisions"]["supervised_stripe_test_mode_private_canary"]
if earned < decision["go_threshold"] or open_p0 or open_p1:
    require(level_b == "NO_GO", "an under-threshold or blocked Level B must be NO_GO")
require(decision["decisions"]["live_money_or_public_launch"] == "NO_GO_PROHIBITED",
        "live money/public launch must remain explicitly prohibited")
require(decision["machine_input_request"] == "ops/go-closure-inputs.json",
        "decision must point to the single exact input request")

print(f"readiness: PASS ({earned}/100, P0={len(open_p0)}, P1={len(open_p1)}, Level B {level_b})")
