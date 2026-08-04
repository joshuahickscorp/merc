#!/usr/bin/env bash
# Package a built merc-agent into a versioned tarball + checksum fragment.
# Usage: package-agent-binary.sh <os> <arch> <path-to-merc-agent>
set -euo pipefail

OS="${1:?os required (darwin|linux)}"
ARCH="${2:?arch required (arm64|amd64)}"
BIN_SRC="${3:?binary path required}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[[ -x "$BIN_SRC" ]] || { echo "binary not executable: $BIN_SRC" >&2; exit 1; }

VERSION="${MERC_AGENT_VERSION:-}"
if [[ -z "$VERSION" ]]; then
  VERSION="sha-$(git -C "$ROOT" rev-parse --short=12 HEAD)"
fi
COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
OUT="${MERC_AGENT_OUT:-$ROOT/.artifacts/agent-release}"
NAME="merc-agent_${VERSION}_${OS}_${ARCH}"

mkdir -p "$OUT"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/merc-agent-pkg.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

STAGE="$WORK/$NAME"
mkdir -p "$STAGE"
install -m 0755 "$BIN_SRC" "$STAGE/merc-agent"
if [[ "$OS" == "darwin" ]]; then
  # Seatbelt profile is resolved as a sibling of the executable
  # (agent/src/main.rs resolve_sandbox_profile). A stock install without this
  # file runs buyer payload with no containment at all — ship it next to the
  # binary so sibling resolution finds it with no env override.
  SB_PROFILE="$ROOT/clients/macapp/ComputeExchangeAgent/merc-agent.sb"
  [[ -f "$SB_PROFILE" ]] || { echo "missing seatbelt profile: $SB_PROFILE" >&2; exit 1; }
  install -m 0644 "$SB_PROFILE" "$STAGE/merc-agent.sb"
fi
if [[ "$OS" == "linux" ]]; then
  # The CUDA supplier process is `merc-agent vllm`, not the Apple-oriented
  # generic runner. Ship the exact profile it loads so a prebuilt install never
  # points at a developer checkout or an unpinned profile URL.
  VLLM_PROFILE="$ROOT/control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json"
  [[ -f "$VLLM_PROFILE" ]] || { echo "missing pinned vLLM runtime profile: $VLLM_PROFILE" >&2; exit 1; }
  install -m 0644 "$VLLM_PROFILE" "$STAGE/vllm-runtime-profile.json"
fi
cat >"$STAGE/README.txt" <<EOF
merc-agent ${VERSION}
commit ${COMMIT}
target ${OS}/${ARCH}

Install with scripts/install.sh (preferred) or copy merc-agent onto PATH.

macOS/arm64 runs the Metal supplier path under the merc-agent.sb seatbelt
profile shipped beside this binary. Linux/amd64 packages the pinned
vLLM adapter and its exact runtime profile; activation requires a supported
NVIDIA GPU, Docker-compatible container runtime, an enrolled worker token,
and a TLS public endpoint. Installation alone never advertises supply.
EOF

tar -C "$WORK" -czf "$OUT/${NAME}.tar.gz" "$NAME"

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

SHA="$(checksum "$OUT/${NAME}.tar.gz")"
SIZE="$(wc -c <"$OUT/${NAME}.tar.gz" | tr -d ' ')"
printf '%s  %s\n' "$SHA" "${NAME}.tar.gz" >"$OUT/SHA256SUMS"
python3 -c "
import json
print(json.dumps({
  'schema_version': 1,
  'component': 'merc-agent',
  'version': '''${VERSION}''',
  'commit': '''${COMMIT}''',
  'artifacts': [{
    'name': '''${NAME}.tar.gz''',
    'os': '''${OS}''',
    'arch': '''${ARCH}''',
    'sha256': '''${SHA}''',
    'size_bytes': int('''${SIZE}'''),
  }],
}, indent=2))
" >"$OUT/manifest.json"

echo "packaged $OUT/${NAME}.tar.gz sha256=$SHA"
