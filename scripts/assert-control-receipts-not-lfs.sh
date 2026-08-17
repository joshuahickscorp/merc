#!/usr/bin/env bash
# Refuse to build a control image whose cited catalogue receipts are Git LFS
# pointer files. Observed 2026-08-16: HEAD built from a sparse/unsmudged
# checkout crash-looped with
#   catalogue price authority unavailable: ... cited receipt is not JSON
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
fail=0
shopt -s nullglob
for f in "$ROOT"/evidence/perf/runtime-benchmarks/* "$ROOT"/evidence/benchmarks/*; do
  [ -f "$f" ] || continue
  case "$(head -c 20 "$f" 2>/dev/null || true)" in
    version\ https://git-l*)
      printf 'assert-control-receipts-not-lfs: POINTER %s\n' "${f#"$ROOT"/}" >&2
      fail=1
      ;;
  esac
done
if [ "$fail" -ne 0 ]; then
  printf 'assert-control-receipts-not-lfs: FAIL — git lfs pull --include=evidence/perf/runtime-benchmarks/**\n' >&2
  exit 1
fi
printf 'assert-control-receipts-not-lfs: PASS\n'
