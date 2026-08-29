#!/usr/bin/env bash
# Ensure .tools/runpodctl is present, matches the pinned SHA-256, then exec it.
# Never run an unverified binary. Pin lives in ops/scripts/runpodctl.sha256.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BIN="$ROOT/.tools/runpodctl"
PIN_FILE="$ROOT/ops/scripts/runpodctl.sha256"

if [[ ! -f "$PIN_FILE" ]]; then
  echo "bootstrap-runpodctl: missing pin file $PIN_FILE" >&2
  exit 2
fi

# Resolve pin (first 64-hex token on a non-comment line).
PIN="$(
  awk '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    {
      if (match($0, /[0-9a-fA-F]{64}/)) {
        print substr($0, RSTART, RLENGTH)
        exit
      }
    }
  ' "$PIN_FILE" | tr '[:upper:]' '[:lower:]'
)"
if [[ ! "$PIN" =~ ^[0-9a-f]{64}$ ]]; then
  echo "bootstrap-runpodctl: could not parse pin from $PIN_FILE" >&2
  exit 2
fi

is_pointer() {
  [[ -f "$1" ]] || return 1
  head -1 "$1" 2>/dev/null | grep -q '^version https://git-lfs.github.com/spec/v1$'
}

if [[ ! -f "$BIN" ]] || is_pointer "$BIN"; then
  if command -v git >/dev/null 2>&1; then
    # Pull only this object; ignore fetchexclude for this path.
    git -C "$ROOT" lfs pull --include=".tools/runpodctl" --exclude="" >/dev/null 2>&1 || true
  fi
fi

if [[ ! -f "$BIN" ]]; then
  echo "bootstrap-runpodctl: $BIN missing after lfs pull" >&2
  echo "  clone with git-lfs, or place a binary at $BIN matching pin $PIN" >&2
  exit 1
fi

if is_pointer "$BIN"; then
  echo "bootstrap-runpodctl: $BIN is still an LFS pointer; cannot verify/exec" >&2
  echo "  run: git -C \"$ROOT\" lfs pull --include=.tools/runpodctl" >&2
  exit 1
fi

if command -v shasum >/dev/null 2>&1; then
  GOT="$(shasum -a 256 "$BIN" | awk '{print tolower($1)}')"
elif command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "$BIN" | awk '{print tolower($1)}')"
else
  GOT="$(python3 -c "import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],'rb').read()).hexdigest())" "$BIN")"
fi

if [[ "$GOT" != "$PIN" ]]; then
  echo "bootstrap-runpodctl: SHA-256 mismatch for $BIN" >&2
  echo "  expected: $PIN" >&2
  echo "  got:      $GOT" >&2
  exit 1
fi

chmod +x "$BIN" 2>/dev/null || true
exec "$BIN" "$@"
