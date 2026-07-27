#!/usr/bin/env python3
"""Build the RunPod REST create-pod payload for a pinned vLLM worker.

Kept out of the shell script because the two things that make a pod actually
start are easy to get wrong and were both wrong for the first three attempts:

- No dockerEntrypoint. The vLLM image's own entrypoint IS the OpenAI api server.
  Overriding it also replaces RunPod's in-container agent, which is what
  populates pod.runtime -- so an override leaves runtime null forever and any
  readiness check that waits on runtime never fires.

- volumeMountPath is the HuggingFace cache. Without a volume the model is
  re-downloaded on every restart, which is slow and billed.

Usage: runpod-create-payload.py <gpu> <image> <model> <api-key> <name> <cloud>
"""

import json
import sys


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
        "dockerStartCmd": [
            "--model", model,
            "--host", "0.0.0.0",
            "--port", "8000",
            "--api-key", key,
            "--max-model-len", "8192",
            "--served-model-name", "merc-vllm",
            "--gpu-memory-utilization", "0.95",
            "--enable-prefix-caching",
            "--enable-chunked-prefill",
            "--max-num-seqs", "256",
        ],
        "env": {"HF_HUB_ENABLE_HF_TRANSFER": "1"},
    }))
    return 0


if __name__ == "__main__":
    sys.exit(main())
