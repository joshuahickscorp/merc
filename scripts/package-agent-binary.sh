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
cat >"$STAGE/README.txt" <<EOF
merc-agent ${VERSION}
commit ${COMMIT}
target ${OS}/${ARCH}

Install with scripts/install.sh (preferred) or copy merc-agent onto PATH.

Linux/amd64 builds are CPU Candle only. control still rejects non-Apple
hw_class values for worker registration; installing this binary does not
unlock Linux/NVIDIA batch supply.
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
