#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  echo "usage: scripts/build-cli-release.sh <version> [output-dir]" >&2
  exit 2
fi
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]]; then
  echo "version must look like v1.2.3 or v1.2.3-rc.1 (got $VERSION)" >&2
  exit 2
fi

OUT="${2:-$ROOT/.artifacts/releases/cli/$VERSION}"
COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.cliVersion=$VERSION -X main.cliCommit=$COMMIT -X main.cliBuildDate=$BUILD_DATE"
TARGETS=(darwin/arm64 darwin/amd64 linux/arm64 linux/amd64)

mkdir -p "$OUT"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/cx-cli-release.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

printf '{"schema_version":1,"version":"%s","commit":"%s","build_date":"%s","artifacts":[' \
  "$VERSION" "$COMMIT" "$BUILD_DATE" >"$OUT/manifest.json"
first=1
for target in "${TARGETS[@]}"; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  NAME="cx_${VERSION}_${GOOS}_${GOARCH}"
  STAGE="$WORK/$NAME"
  mkdir -p "$STAGE"
  (
    cd "$ROOT/control"
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
      go build -trimpath -ldflags "$LDFLAGS" -o "$STAGE/cx" .
  )
  cp "$ROOT/cli/README.md" "$STAGE/README.md"
  tar -C "$WORK" -czf "$OUT/$NAME.tar.gz" "$NAME"
  SHA="$(checksum "$OUT/$NAME.tar.gz")"
  SIZE="$(wc -c <"$OUT/$NAME.tar.gz" | tr -d ' ')"
  if [[ "$first" -eq 0 ]]; then printf ',' >>"$OUT/manifest.json"; fi
  first=0
  printf '{"name":"%s.tar.gz","goos":"%s","goarch":"%s","sha256":"%s","size_bytes":%s}' \
    "$NAME" "$GOOS" "$GOARCH" "$SHA" "$SIZE" >>"$OUT/manifest.json"
done
printf ']}\n' >>"$OUT/manifest.json"

(
  cd "$OUT"
  : >SHA256SUMS
  for archive in ./*.tar.gz; do
    printf '%s  %s\n' "$(checksum "$archive")" "${archive#./}" >>SHA256SUMS
  done
)

python3 -m json.tool "$OUT/manifest.json" >/dev/null
echo "CLI release artifacts: $OUT"
echo "Commit: $COMMIT"
echo "Checksums: $OUT/SHA256SUMS"
