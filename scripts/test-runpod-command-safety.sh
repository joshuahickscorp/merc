#!/usr/bin/env bash
# No-argument and help paths must remain non-billable, even without credentials.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"

for command in "" --help; do
  if [[ -z "$command" ]]; then
    output="$(env -u RUNPOD_API_KEY bash "$ROOT/scripts/runpod-vllm.sh")"
  else
    output="$(env -u RUNPOD_API_KEY bash "$ROOT/scripts/runpod-vllm.sh" "$command")"
  fi
  if ! grep -q '^usage: scripts/runpod-vllm.sh <command>$' <<<"$output"; then
    echo "runpod-command-safety: FAIL -- ${command:-no-argument} did not print usage" >&2
    exit 1
  fi
done

echo "runpod-command-safety: PASS -- help paths require no key and cannot provision"
