#!/usr/bin/env python3
"""Pin the placement-readiness gate's refusals.

1. Gate reports every precondition and exits non-zero while any is UNSATISFIED
   (current tree may honestly be NOT_READY — that is a successful outcome of the
   gate programme, not a test failure).
2. Mutating away the CUDA embed cell makes matched_model_identity UNSATISFIED.
3. Env vars cannot force a pass.
4. Summary line and exit code distinguish NOT_READY from READY /
   READY_PENDING_AUTHORIZED_SPEND.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
GATE = ROOT / "ops/scripts" / "validate-placement-readiness.py"


def run_gate(cwd: Path, env: dict | None = None) -> tuple[int, str]:
    full_env = os.environ.copy()
    # Clear refuse-bypass names so the baseline is clean.
    for name in (
        "MERC_PLACEMENT_READINESS_FORCE",
        "MERC_PLACEMENT_READINESS_BYPASS",
        "MERC_PLACEMENT_READY",
        "MERC_FORCE_PLACEMENT_READY",
        "MERC_ALLOW_PLACEMENT_READINESS_SKIP",
        "MERC_SKIP_PLACEMENT_READINESS",
    ):
        full_env.pop(name, None)
    if env:
        full_env.update(env)
    proc = subprocess.run(
        [
            sys.executable,
            str(GATE if cwd == ROOT else cwd / "ops/scripts" / "validate-placement-readiness.py"),
            "--json",
        ],
        cwd=str(cwd),
        env=full_env,
        capture_output=True,
        text=True,
    )
    out = proc.stdout + proc.stderr
    return proc.returncode, out


def load_report(stdout: str) -> dict:
    # --json prints only JSON on stdout when clean; tolerate noise.
    text = stdout.strip()
    # Find the outermost JSON object.
    start = text.find("{")
    end = text.rfind("}")
    if start < 0 or end < 0:
        raise AssertionError(f"gate produced no JSON:\n{stdout}")
    return json.loads(text[start : end + 1])


def main() -> int:
    failures: list[str] = []

    # --- 1. Gate is executable and self-consistent on this tree ---
    code, out = run_gate(ROOT)
    try:
        report = load_report(out)
    except AssertionError as exc:
        failures.append(str(exc))
        print("test-placement-readiness: FAIL")
        for f in failures:
            print(f"  - {f}")
        return 1

    if report.get("kind") != "placement_readiness_gate":
        failures.append(f"unexpected kind {report.get('kind')!r}")
    pre = report.get("preconditions") or []
    if len(pre) < 8:
        failures.append(f"expected at least 8 preconditions, got {len(pre)}")
    states = {p["id"]: p["state"] for p in pre}
    for p in pre:
        if p["state"] not in (
            "SATISFIED",
            "UNSATISFIED",
            "BLOCKED_ON_AUTHORIZED_SPEND",
        ):
            failures.append(f"{p['id']} has unknown state {p['state']!r}")
        if not p.get("reason") or not p.get("artifact_or_path"):
            failures.append(f"{p['id']} missing reason or artifact_or_path")

    unsat = [p for p in pre if p["state"] == "UNSATISFIED"]
    summary = report.get("summary")
    if unsat:
        if code == 0:
            failures.append("gate exited 0 while preconditions are UNSATISFIED")
        if summary != "NOT_READY":
            failures.append(f"summary={summary!r} with UNSATISFIED items, want NOT_READY")
    else:
        if code != 0:
            failures.append(f"gate exited {code} with no UNSATISFIED items")
        if summary not in ("READY", "READY_PENDING_AUTHORIZED_SPEND"):
            failures.append(f"summary={summary!r} without UNSATISFIED, want READY*")

    # CUDA cell must exist and stay non-routable on the document facts.
    if states.get("cuda_embed_cell_non_routable_identity") != "SATISFIED":
        failures.append(
            "cuda_embed_cell_non_routable_identity is not SATISFIED: "
            + str(states.get("cuda_embed_cell_non_routable_identity"))
        )
    if states.get("matched_model_identity") != "SATISFIED":
        failures.append(
            "matched_model_identity is not SATISFIED after adding the CUDA embed cell"
        )

    # --- 2. Removing the CUDA embed cell makes the gate refuse ---
    with tempfile.TemporaryDirectory(prefix="merc-placement-ready-") as tmp:
        tmp_path = Path(tmp)
        # Minimal tree: authority, contract, scripts gate depends on, control files.
        for rel in (
            "src/control/runtime-authority.json",
            "ops/placement-readiness-contract.json",
            "ops/scripts/validate-placement-readiness.py",
            "ops/scripts/runpod-spend-guard.py",
            "ops/scripts/runpod-vllm.sh",
            "ops/scripts/test-runpod-orphan-reconcile.py",
            "ops/scripts/write-bound-evidence.py",
            "ops/scripts/validate-evidence-binding.py",
            "src/control/embedding_comparator.go",
            "src/control/runtime_shadow_selection.go",
            "src/control/runtime_governed_comparison.go",
            "src/control/runtime_cell_cost.go",
            "src/control/runtime_cell_cost_test.go",
            "src/control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json",
            "evidence/runpod/spend-rr7b6uwmivaolh.json",
            "evidence/perf/selector/engine-parity-metal-embed-latest.json",
        ):
            src = ROOT / rel
            if not src.is_file():
                continue
            dest = tmp_path / rel
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, dest)

        auth_path = tmp_path / "src/control" / "runtime-authority.json"
        auth = json.loads(auth_path.read_text(encoding="utf-8"))
        for rt in auth.get("runtimes", []):
            if rt.get("runtime_id") != "vllm_cuda":
                continue
            rt["cells"] = [c for c in rt.get("cells", []) if c.get("id") != "vllm-cuda-minilm-embed"]
        auth_path.write_text(json.dumps(auth, indent=2) + "\n", encoding="utf-8")

        code2, out2 = run_gate(tmp_path)
        try:
            report2 = load_report(out2)
        except AssertionError as exc:
            failures.append(f"mutated tree: {exc}")
            report2 = {}
        else:
            if code2 == 0:
                failures.append("gate exited 0 after CUDA embed cell was removed")
            if report2.get("summary") != "NOT_READY":
                failures.append(
                    f"after cell removal summary={report2.get('summary')!r}, want NOT_READY"
                )
            states2 = {p["id"]: p["state"] for p in report2.get("preconditions") or []}
            if states2.get("matched_model_identity") != "UNSATISFIED":
                failures.append(
                    "removing vllm-cuda-minilm-embed did not UNSATISFY matched_model_identity"
                )
            if states2.get("cuda_embed_cell_non_routable_identity") != "UNSATISFIED":
                failures.append(
                    "removing vllm-cuda-minilm-embed did not UNSATISFY "
                    "cuda_embed_cell_non_routable_identity"
                )

    # --- 3. Env var cannot bypass an unsatisfied gate ---
    # Force UNSATISFIED by pointing at a tree missing the contract, with FORCE set.
    with tempfile.TemporaryDirectory(prefix="merc-placement-bypass-") as tmp:
        tmp_path = Path(tmp)
        # Copy gate + authority but omit the contract → hard fail or unsatisfied.
        for rel in (
            "src/control/runtime-authority.json",
            "ops/scripts/validate-placement-readiness.py",
        ):
            dest = tmp_path / rel
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / rel, dest)
        code3, out3 = run_gate(
            tmp_path,
            env={"MERC_PLACEMENT_READINESS_FORCE": "1", "MERC_PLACEMENT_READY": "1"},
        )
        if code3 == 0:
            failures.append(
                "MERC_PLACEMENT_READINESS_FORCE=1 forced a pass on a broken tree"
            )
        # On a complete tree, FORCE still cannot convert UNSATISFIED to pass.
        if unsat:
            code4, out4 = run_gate(
                ROOT,
                env={"MERC_PLACEMENT_READINESS_FORCE": "1"},
            )
            try:
                report4 = load_report(out4)
            except AssertionError as exc:
                failures.append(f"force-on-unsat: {exc}")
            else:
                if code4 == 0:
                    failures.append(
                        "MERC_PLACEMENT_READINESS_FORCE=1 bypassed UNSATISFIED preconditions"
                    )
                if report4.get("summary") != "NOT_READY":
                    failures.append(
                        "FORCE env changed summary away from NOT_READY while unsatisfied"
                    )
                if not report4.get("refused_bypass_envs"):
                    failures.append("FORCE env was not recorded as refused_bypass_envs")
                bypass_state = {
                    p["id"]: p["state"] for p in report4.get("preconditions") or []
                }.get("env_bypass_refusal")
                if bypass_state != "UNSATISFIED":
                    failures.append(
                        f"env_bypass_refusal state={bypass_state!r}, want UNSATISFIED"
                    )

    if failures:
        print("test-placement-readiness: FAIL")
        for f in failures:
            print(f"  - {f}")
        print("--- gate report (baseline) ---")
        print(json.dumps(report, indent=2)[:4000])
        return 1

    print("test-placement-readiness: PASS")
    print(f"  baseline summary={summary} exit={code}")
    print(
        f"  SATISFIED={report['counts'].get('SATISFIED')} "
        f"UNSATISFIED={report['counts'].get('UNSATISFIED')} "
        f"BLOCKED_ON_AUTHORIZED_SPEND={report['counts'].get('BLOCKED_ON_AUTHORIZED_SPEND')}"
    )
    for p in pre:
        print(f"  [{p['state']}] {p['id']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
