#!/usr/bin/env python3
"""Build the RunPod REST create-pod payload for a pinned vLLM worker.

Kept out of the shell script because the two things that make a pod actually
start are easy to get wrong and were both wrong for the first three attempts:

- No dockerEntrypoint by default. The vLLM image's own entrypoint IS the OpenAI
  api server. Overriding it also replaces RunPod's in-container agent, which is
  what populates pod.runtime -- so an override leaves runtime null forever and
  any readiness check that waits on runtime never fires.

  GGUF mode overrides it, and GGUF MODE DOES NOT WORK. Two attempts on
  2026-07-30 (A40 and L4) served HTTP 404 for 900s and were torn down without
  ever answering. Cost: $0.08 for nothing.

  The cause was not isolated. Either (a) the entrypoint override replaces
  RunPod's in-container agent and the HTTP proxy never wires to port 8000 --
  which is what the note above warns about and which I dismissed because the
  readiness check no longer polls pod.runtime -- or (b) the download-then-serve
  command itself failed inside the container. 404 rather than connection-refused
  says the proxy was reachable with nothing behind it, which leans toward (a).
  Distinguishing them needs container logs, which this script cannot fetch and
  which cost another pod to obtain.

  Do not re-enable without a way to read container logs first. The default
  (no entrypoint override, HF weights) is the proven path.

- volumeMountPath is the HuggingFace cache. Without a volume the model is
  re-downloaded on every restart, which is slow and billed.

Usage: runpod-create-payload.py <gpu> <image> <model> <api-key> <name> <cloud>

MERC_VLLM_GGUF_REPO + MERC_VLLM_GGUF_FILE switch to GGUF mode, where <model>
becomes the tokenizer repo.
"""

import json
import os
import sys

VLLM_ARGS = [
    "--host", "0.0.0.0",
    "--port", "8000",
    "--max-model-len", "8192",
    "--served-model-name", "merc-vllm",
    "--gpu-memory-utilization", "0.95",
    "--enable-prefix-caching",
    "--enable-chunked-prefill",
    "--max-num-seqs", "256",
]


def gguf_start_command(repo: str, filename: str, tokenizer: str, key: str) -> str:
    """Download the pinned GGUF, then exec vLLM against the local file.

    `exec` matters: without it vLLM runs as a child of bash and container stop
    signals reach the wrong process.
    """
    fetch = (
        "python3 -c \"from huggingface_hub import hf_hub_download as d; "
        f"print(d('{repo}','{filename}',local_dir='/model'))\""
    )
    serve = " ".join([
        "exec python3 -m vllm.entrypoints.openai.api_server",
        f"--model /model/{filename}",
        f"--tokenizer {tokenizer}",
        f"--api-key {key}",
        *VLLM_ARGS,
    ])
    return f"set -euo pipefail; {fetch}; {serve}"


def main() -> int:
    if len(sys.argv) != 7:
        sys.stderr.write(__doc__.strip().splitlines()[-1] + "\n")
        return 2
    gpu, image, model, key, name, cloud = sys.argv[1:7]
    print(json.dumps({
        "name": name,
        "imageName": image,
        "gpuTypeIds": [gpu],
        "gpuCount": 1,
        # Disk, not GPU, is what actually gets refused. A create asking 40GB
        # container + 40GB volume returned "no instances currently available"
        # for every GPU class while a 5GB probe on the SAME class succeeded --
        # the message names the GPU and means the machine cannot satisfy the
        # whole request. Keep the container lean and let the volume hold the
        # model cache.
        "containerDiskInGb": 20,
        "volumeInGb": 25,
        "volumeMountPath": "/root/.cache/huggingface",
        "ports": ["8000/http"],
        "cloudType": cloud,
        # Tuning, not defaults. The first sweep ran stock vLLM and left real
        # throughput unclaimed, which matters because throughput is the
        # SUPPLIER's margin: they pay the electricity either way, so tokens per
        # kilowatt-hour is what makes a rig worth attaching to merc.
        #
        #   enable-prefix-caching   reuses KV across requests sharing a prefix,
        #                           compounding with merc's warm-prefix routing
        #   enable-chunked-prefill  stops a long prompt stalling decode for
        #                           everyone else in the batch
        #   max-num-seqs            stock caps concurrency well below what the
        #                           card can hold
        #   gpu-memory-utilization  more KV cache means a deeper batch
        **_start(model, key),
        "env": {"HF_HUB_ENABLE_HF_TRANSFER": "1"},
    }))
    return 0


def _start(model: str, key: str) -> dict:
    repo = os.environ.get("MERC_VLLM_GGUF_REPO", "")
    filename = os.environ.get("MERC_VLLM_GGUF_FILE", "")
    if repo and filename:
        return {
            "dockerEntrypoint": ["/bin/bash", "-lc"],
            "dockerStartCmd": [gguf_start_command(repo, filename, model, key)],
        }
    return {
        "dockerStartCmd": ["--model", model, "--api-key", key, *VLLM_ARGS],
    }


if __name__ == "__main__":
    sys.exit(main())
