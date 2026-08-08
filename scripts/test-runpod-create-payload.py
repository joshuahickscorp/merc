#!/usr/bin/env python3
"""Offline contract test for the RunPod create payload key transport.

The test never contacts RunPod.  It verifies that the only supported command
shape transports the vLLM key on stdin, while the generated provider body still
receives the key needed by the remote vLLM process.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BUILDER = os.path.join(ROOT, "scripts", "runpod-create-payload.py")
SENTINEL = "cx_vllm_stdin_only_0123456789abcdef0123456789abcdef"


def main() -> int:
    command = [
        sys.executable,
        BUILDER,
        "--api-key-stdin",
        "NVIDIA RTX A5000",
        "vllm/vllm-openai@sha256:" + "a" * 64,
        "meta-llama/Llama-3.2-1B-Instruct",
        "merc-canary-vllm",
        "SECURE",
    ]
    if SENTINEL in " ".join(command):
        raise AssertionError("test command exposed the vLLM key in argv")
    result = subprocess.run(
        command,
        input=SENTINEL + "\n",
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise AssertionError(result.stderr)
    payload = json.loads(result.stdout)
    args = payload["dockerStartCmd"]
    key_index = args.index("--api-key")
    if args[key_index + 1] != SENTINEL:
        raise AssertionError("payload did not carry the stdin vLLM key")
    old_shape = subprocess.run(
        [
            sys.executable,
            BUILDER,
            "NVIDIA RTX A5000",
            "image",
            "model",
            SENTINEL,
            "merc-canary-vllm",
            "SECURE",
        ],
        capture_output=True,
        text=True,
        check=False,
    )
    if old_shape.returncode == 0:
        raise AssertionError("legacy argv API-key shape was accepted")
    print("test-runpod-create-payload: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
