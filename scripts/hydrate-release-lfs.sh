#!/usr/bin/env bash
# Hydrate release-critical LFS payloads that .lfsconfig fetchexclude skips.
#
# actions/checkout with lfs:true runs a plain `git lfs pull`, which honours
# fetchexclude = evidence/perf/**. Measured: --include alone does NOT override
# an exclude; only --exclude="" does.
#
# Fails closed if any tracked evidence/perf/** object remains an unresolved
# pointer after the pull — a job that proceeds on pointers is the silent-pass
# failure this programme exists to prevent.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "${ROOT}" ]]; then
  echo "hydrate-release-lfs: not inside a git repository" >&2
  exit 2
fi
cd "${ROOT}"

if ! command -v git >/dev/null || ! git lfs version >/dev/null 2>&1; then
  echo "hydrate-release-lfs: git-lfs is required" >&2
  exit 2
fi

echo "hydrate-release-lfs: pulling evidence/perf/** with exclude cleared"
git lfs pull --exclude="" --include="evidence/perf/**"

fail=0
checked=0
while IFS= read -r path; do
  [[ -z "${path}" ]] && continue
  case "${path}" in
    evidence/perf/*) ;;
    *) continue ;;
  esac
  checked=$((checked + 1))
  if [[ ! -e "${path}" ]]; then
    echo "hydrate-release-lfs: FAIL missing path: ${path}" >&2
    fail=1
    continue
  fi
  # Unresolved pointer is a 3-line LFS stub, not the payload.
  if head -n 1 "${path}" 2>/dev/null | grep -q 'git-lfs.github.com/spec/v1'; then
    oid="$(awk '/^oid sha256:/{print $2}' "${path}" | head -n1)"
    echo "hydrate-release-lfs: FAIL unresolved LFS pointer: ${path} (${oid:-unknown oid})" >&2
    fail=1
    continue
  fi
done < <(git lfs ls-files -n)

if [[ "${checked}" -eq 0 ]]; then
  echo "hydrate-release-lfs: FAIL no evidence/perf/** LFS paths tracked" >&2
  exit 1
fi

if [[ "${fail}" -ne 0 ]]; then
  echo "hydrate-release-lfs: FAIL ${checked} evidence/perf LFS path(s) checked; unresolved or missing objects present" >&2
  exit 1
fi

echo "hydrate-release-lfs: OK ${checked} evidence/perf LFS payload(s) resolved"
exit 0
