#!/usr/bin/env python3
"""Ask the pinned vLLM image whether these engine args would start, before renting a GPU.

Two of the four pods created for the CUDA embed arm died in startup on errors
the image could have answered on this laptop for nothing:

  --task embed        vLLM 0.23.0 removed --task; the flag is --runner.
                      argparse exits, the server never binds, and RunPod's proxy
                      answers 404 for the whole readiness window -- which looks
                      exactly like a hung engine.
  --max-model-len 512 this MiniLM revision declares max_position_embeddings=256,
                      so config validation rejects 512 and the server exits.

Neither needed a GPU to discover. Both cost $0.07 and nine minutes each, and
ops/scripts/runpod-create-payload.py already carried a warning not to iterate on
engine flags by renting hardware -- written after the last time this happened.

    python3 ops/scripts/vllm-args-preflight.py --image <image@sha256:...> -- \\
        --model sentence-transformers/all-MiniLM-L6-v2 --runner pooling ...

Exit 0 means argparse accepted the flags AND vLLM resolved a full engine config
(architecture, dtype, context length, pooling vs generation) against the real
model repository. It does NOT mean the model will fit the card or that the
kernels exist for it -- device selection is stubbed, because there is no GPU
here. It catches the class of failure that killed both pods, which is the class
worth catching for free.

Needs docker and the linux/amd64 image; on Apple silicon it runs under emulation
and is slow the first time, then cached.
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys

# Runs inside the container. Device inference is stubbed so the config can be
# built without CUDA; everything else is the real code path the pod runs.
PROBE = r"""
import json, sys
import vllm.config.device as dv
dv.DeviceConfig.__post_init__ = lambda self: setattr(self, "device_type", "cuda")
from vllm.utils.argparse_utils import FlexibleArgumentParser
from vllm.entrypoints.openai.cli_args import make_arg_parser
from vllm.engine.arg_utils import AsyncEngineArgs
import vllm

argv = json.loads(sys.argv[1])
parser = make_arg_parser(FlexibleArgumentParser())
try:
    args = parser.parse_args(argv)
except SystemExit as exc:
    print(json.dumps({"ok": False, "stage": "argparse", "vllm": vllm.__version__,
                      "detail": f"argparse rejected the flags (exit {exc.code})"}))
    raise SystemExit(0)
try:
    cfg = AsyncEngineArgs.from_cli_args(args).create_engine_config()
except Exception as exc:
    print(json.dumps({"ok": False, "stage": "engine_config", "vllm": vllm.__version__,
                      "detail": f"{type(exc).__name__}: {exc}"[:1500]}))
    raise SystemExit(0)
m = cfg.model_config
print(json.dumps({"ok": True, "stage": "engine_config", "vllm": vllm.__version__,
                  "architectures": list(m.architectures), "runner": m.runner_type,
                  "convert": m.convert_type, "max_model_len": m.max_model_len,
                  "dtype": str(m.dtype)}))
"""


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--image", required=True, help="pinned vLLM image (use @sha256:)")
    ap.add_argument("--platform", default="linux/amd64")
    ap.add_argument("engine_args", nargs=argparse.REMAINDER,
                    help="engine args after --, exactly as the pod would receive them")
    ns = ap.parse_args()

    argv = ns.engine_args
    if argv and argv[0] == "--":
        argv = argv[1:]
    if not argv:
        print("refusing: no engine args given after --", file=sys.stderr)
        return 2
    if "@sha256:" not in ns.image:
        print("refusing: preflight a pinned image digest, not a mutable tag; a tag "
              "can move between this check and the pod", file=sys.stderr)
        return 2
    if shutil.which("docker") is None:
        print("refusing: docker is required to ask the image", file=sys.stderr)
        return 2

    import json as _json

    proc = subprocess.run(
        ["docker", "run", "--rm", "--platform", ns.platform,
         "--entrypoint", "python3", ns.image, "-c", PROBE, _json.dumps(argv)],
        capture_output=True, text=True,
    )
    verdict = None
    for line in reversed(proc.stdout.splitlines()):
        line = line.strip()
        if line.startswith("{"):
            try:
                verdict = _json.loads(line)
                break
            except ValueError:
                continue
    if verdict is None:
        print("preflight could not run inside the image:", file=sys.stderr)
        print((proc.stderr or proc.stdout)[-1500:], file=sys.stderr)
        return 3

    print(_json.dumps(verdict, indent=2))
    if not verdict.get("ok"):
        print(f"\nREFUSED at {verdict['stage']}: these args would not start a pod.",
              file=sys.stderr)
        return 1
    print("\nthese args resolve a full engine config; the remaining risks are "
          "device-side (memory, kernels, capacity)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
