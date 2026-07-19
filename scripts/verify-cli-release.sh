#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-v0.0.0-local}"
OUT="$(mktemp -d "${TMPDIR:-/tmp}/cx-cli-proof.XXXXXX")"
trap 'rm -rf "$OUT"' EXIT

"$ROOT/scripts/build-cli-release.sh" "$VERSION" "$OUT/release"

GOOS="$(go env GOOS)"
GOARCH="$(go env GOARCH)"
ARCHIVE="cx_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
grep "  $ARCHIVE$" "$OUT/release/SHA256SUMS" >/dev/null
(
  cd "$OUT/release"
  if command -v sha256sum >/dev/null 2>&1; then
    grep "  $ARCHIVE$" SHA256SUMS | sha256sum -c -
  else
    grep "  $ARCHIVE$" SHA256SUMS | shasum -a 256 -c -
  fi
)

mkdir -p "$OUT/install"
tar -C "$OUT/install" -xzf "$OUT/release/$ARCHIVE"
BIN="$OUT/install/cx_${VERSION}_${GOOS}_${GOARCH}/cx"
VERSION_JSON="$($BIN version --json)"
jq -e --arg version "$VERSION" '
  .version == $version and (.commit | length) == 40 and
  (.build_date | endswith("Z")) and (.platform | contains("/"))
' <<<"$VERSION_JSON" >/dev/null
"$BIN" help >/dev/null 2>&1
echo "PASS clean CLI archive install: $VERSION $GOOS/$GOARCH"
