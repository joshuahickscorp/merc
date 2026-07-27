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
        "containerDiskInGb": 40,
        "volumeInGb": 40,
        "volumeMountPath": "/root/.cache/huggingface",
        "ports": ["8000/http"],
        "cloudType": cloud,
        "dockerStartCmd": [
            "--model", model,
            "--host", "0.0.0.0",
            "--port", "8000",
            "--api-key", key,
            "--max-model-len", "8192",
            "--served-model-name", "merc-vllm",
            "--gpu-memory-utilization", "0.90",
        ],
        "env": {"HF_HUB_ENABLE_HF_TRANSFER": "1"},
    }))
    return 0


if __name__ == "__main__":
    sys.exit(main())
