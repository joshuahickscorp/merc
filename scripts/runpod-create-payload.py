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

Usage: runpod-create-payload.py --api-key-stdin <gpu> <image> <model> <name> <cloud>

The vLLM API key is read from standard input, never process arguments.  The
caller must provide exactly one non-empty line through a private pipe/FD.

MERC_VLLM_GGUF_REPO + MERC_VLLM_GGUF_FILE switch to GGUF mode, where <model>
becomes the tokenizer repo.
"""

import json
import os
import re
import sys

REVISION = re.compile(r"^[0-9a-f]{40}$")


def runtime_settings() -> dict:
    """Read the values exported from the governed runtime profile.

    The provisioner must not have a second set of model-serving defaults: an
    offer for ``cx-chat-1b`` that launches an engine serving ``merc-vllm`` or a
    mutable model revision is not a valid candidate, even if /models answers.
    """
    settings = {
        "served_model": os.environ.get("MERC_VLLM_SERVED_MODEL", "cx-chat-1b"),
        "model_revision": os.environ.get("MERC_VLLM_MODEL_REVISION", "5a8abab4a5d6f164389b1079fb721cfab8d7126c"),
        "tokenizer_revision": os.environ.get("MERC_VLLM_TOKENIZER_REVISION", "5a8abab4a5d6f164389b1079fb721cfab8d7126c"),
        "max_model_len": os.environ.get("MERC_VLLM_MAX_MODEL_LEN", "32768"),
        "gpu_memory_utilization": os.environ.get("MERC_VLLM_GPU_MEMORY_UTILIZATION", "0.9"),
        "max_num_seqs": os.environ.get("MERC_VLLM_MAX_NUM_SEQS", "128"),
        # "" (default) serves a generator; "embed" serves a pooling model
        # through /v1/embeddings. The distinction changes which vLLM flags are
        # even legal — see vllm_args.
        "task": os.environ.get("MERC_VLLM_TASK", ""),
    }
    if settings["task"] not in ("", "embed"):
        raise ValueError("MERC_VLLM_TASK must be empty (generation) or 'embed'")
    if not REVISION.fullmatch(settings["model_revision"]):
        raise ValueError("MERC_VLLM_MODEL_REVISION must be an exact 40-character revision")
    if not REVISION.fullmatch(settings["tokenizer_revision"]):
        raise ValueError("MERC_VLLM_TOKENIZER_REVISION must be an exact 40-character revision")
    if not re.fullmatch(r"[A-Za-z0-9._:-]+", settings["served_model"]):
        raise ValueError("MERC_VLLM_SERVED_MODEL has unsupported characters")
    for key in ("max_model_len", "max_num_seqs"):
        if not settings[key].isdigit() or int(settings[key]) < 1:
            raise ValueError(f"{key} must be a positive integer")
    try:
        utilization = float(settings["gpu_memory_utilization"])
    except ValueError as exc:
        raise ValueError("gpu_memory_utilization must be numeric") from exc
    if not 0 < utilization <= 1:
        raise ValueError("gpu_memory_utilization must be in (0,1]")
    return settings


def vllm_args(settings: dict) -> list[str]:
    args = [
        "--host", "0.0.0.0",
        "--port", "8000",
        "--max-model-len", settings["max_model_len"],
        "--served-model-name", settings["served_model"],
        "--gpu-memory-utilization", settings["gpu_memory_utilization"],
        "--max-num-seqs", settings["max_num_seqs"],
    ]
    if settings["task"] == "embed":
        # A pooling model is not a generator, and vLLM refuses the generation
        # tuning flags for one: prefix caching and chunked prefill both assume a
        # decode loop that an embedding runner does not have. Passing them anyway
        # fails at startup, which on a rented pod costs money to discover.
        #
        # This is also why the embed arm cannot inherit the generation profile's
        # 32768 context: MiniLM's position embeddings stop at 512, and asking for
        # more is refused before the first request.
        #
        # `--runner pooling`, NOT `--task embed`. The pinned image is vLLM
        # 0.23.0, which removed --task entirely; vllm/config/model.py says it in
        # so many words -- "Use `--runner pooling` or `--runner auto` for
        # embedding models". An unrecognised argument makes argparse exit before
        # the server binds, and RunPod's proxy then answers 404 with nothing
        # behind it for the whole readiness window. That is indistinguishable
        # from the GGUF failure recorded above, and it cost $0.07 and 9 minutes
        # on an A40 to see once. The flag set is verifiable for free against the
        # pinned image:
        #
        #   docker run --rm --platform linux/amd64 --entrypoint bash <image> \
        #     -lc "grep -n 'runner' .../vllm/engine/arg_utils.py"
        #
        # Do that before changing engine flags rather than after renting a GPU.
        return ["--runner", "pooling", *args]
    return [*args, "--enable-prefix-caching", "--enable-chunked-prefill"]


def gguf_start_command(repo: str, filename: str, tokenizer: str, key: str, settings: dict) -> str:
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
        *vllm_args(settings),
    ])
    return f"set -euo pipefail; {fetch}; {serve}"


def main() -> int:
    if len(sys.argv) != 7 or sys.argv[1] != "--api-key-stdin":
        sys.stderr.write(__doc__.strip().splitlines()[-1] + "\n")
        return 2
    key = sys.stdin.read().rstrip("\n")
    if not key or "\n" in key or "\r" in key:
        sys.stderr.write("RunPod vLLM API key stdin must contain one non-empty line\n")
        return 2
    gpu, image, model, name, cloud = sys.argv[2:7]
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
        # 8000 serves vLLM; 22/tcp is for in-experiment board-power sampling
        # (nvidia-smi under load). No permanent shell access is required beyond
        # that measurement window; the governed experiment still tears the pod
        # down on exit.
        "ports": ["8000/http", "22/tcp"],
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
        **_start(model, key, runtime_settings()),
        "env": {"HF_HUB_ENABLE_HF_TRANSFER": "1"},
    }))
    return 0


def _start(model: str, key: str, settings: dict) -> dict:
    repo = os.environ.get("MERC_VLLM_GGUF_REPO", "")
    filename = os.environ.get("MERC_VLLM_GGUF_FILE", "")
    if repo and filename:
        return {
            "dockerEntrypoint": ["/bin/bash", "-lc"],
            "dockerStartCmd": [gguf_start_command(repo, filename, model, key, settings)],
        }
    return {
        "dockerStartCmd": [
            "--model", model,
            "--revision", settings["model_revision"],
            "--tokenizer-revision", settings["tokenizer_revision"],
            "--api-key", key,
            *vllm_args(settings),
        ],
    }


if __name__ == "__main__":
    sys.exit(main())
