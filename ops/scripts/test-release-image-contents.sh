#!/usr/bin/env bash
# Inspect the BUILT control image for every page and asset the control plane
# reads at runtime. The source-tree site-build gate cannot catch this:
# .dockerignore excludes ops/scripts/, and production mounts no clients/web/ volume — pages
# come only from COPY clients/web/ /web/ and COPY ops/configs/pricing/board.json in Dockerfile.control.
#
# Commit 0abb578a shipped an image whose clients/web/ copy listed three pages and omitted
# ops/configs/pricing/board.json; the process could not start and nothing noticed. This gate
# asks the artifact, not the checkout.
#
#   bash ops/scripts/test-release-image-contents.sh
#
# Skips (exit 0) when no container runtime is available. Fails when a runtime IS
# available and a required path is missing from the image.
set -euo pipefail
cd "$(dirname "$0")/../.." || exit 1

RUNTIME=""
for candidate in docker podman; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" info >/dev/null 2>&1; then
    RUNTIME="$candidate"
    break
  fi
done
if [ -z "$RUNTIME" ]; then
  echo "release image contents: SKIPPED (no working docker or podman)"
  exit 0
fi

TAG="merc-control:contents-test-$$"
PROBE="merc-contents-probe-$$"
cleanup() {
  "$RUNTIME" rm -f "$PROBE" >/dev/null 2>&1 || true
  "$RUNTIME" rmi -f "$TAG" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# .lfsconfig fetchexclude skips evidence/perf/**. Dockerfile.control copies
# those receipts into the image; a pointer file crash-loops the control plane
# with "cited receipt is not JSON". Hydrate before the build so the check
# inspects real payloads, then still fail if any copied receipt is a pointer.
if ! bash ops/scripts/hydrate-release-lfs.sh; then
  echo "release image contents: FAIL -- could not hydrate evidence/perf LFS payloads" >&2
  exit 1
fi

echo "release image contents: building $TAG"
# Prefer BuildKit; fall back to classic builder when buildx activity files are
# unwritable (common under sandboxed CI agents / restricted Docker contexts).
if ! "$RUNTIME" build -f Dockerfile.control -t "$TAG" . >/tmp/merc-image-contents-build.log 2>&1; then
  if ! DOCKER_BUILDKIT=0 "$RUNTIME" build -f Dockerfile.control -t "$TAG" . \
      >/tmp/merc-image-contents-build.log 2>&1; then
    echo "release image contents: FAIL -- build" >&2
    tail -40 /tmp/merc-image-contents-build.log >&2
    exit 1
  fi
fi

"$RUNTIME" create --name "$PROBE" "$TAG" >/dev/null

# Paths the control plane reads at runtime (relative to WORKDIR / inside the image).
# serveHTML / handleSiteAsset / handleFavicon / handleSecurityTxt / resolvePriceBoard.
REQUIRED_PATHS=(
  /web/index.html
  /web/admin.html
  /web/buyer.html
  /web/prices.html
  /web/supplier.html
  /web/favicon.ico
  /web/.well-known/security.txt
  /etc/merc/pricing/board.json
)

# Every file under clients/web/ in the source tree must land at /web/... in the image.
# That is the claim COPY clients/web/ /web/ makes; a selective COPY list is how pages
# disappeared before.
# Prefer `command find` so a shell wrapper (e.g. bfs shim) cannot break the gate.
while IFS= read -r -d '' src; do
  rel="${src#./}"
  REQUIRED_PATHS+=("/web/${rel}")
done < <(cd clients/web && command find . -type f -print0)

# Pages and the price board must be non-empty: an empty HTML page or board is as
# broken as a missing one. Other tree files (e.g. presence markers) may be empty.
NON_EMPTY=(
  /web/index.html
  /web/admin.html
  /web/buyer.html
  /web/prices.html
  /web/supplier.html
  /web/favicon.ico
  /web/.well-known/security.txt
  /etc/merc/pricing/board.json
)

must_be_non_empty() {
  local want="$1" p
  for p in "${NON_EMPTY[@]}"; do
    if [ "$p" = "$want" ]; then
      return 0
    fi
  done
  return 1
}

rc=0
tmpdir="$(mktemp -d)"
seen=""
for path in "${REQUIRED_PATHS[@]}"; do
  case " $seen " in
    *" $path "*) continue ;;
  esac
  seen="$seen $path"
  dest="$tmpdir/$(echo "$path" | tr '/' '_')"
  if "$RUNTIME" cp "$PROBE:$path" "$dest" >/dev/null 2>&1; then
    if must_be_non_empty "$path" && [ ! -s "$dest" ]; then
      echo "  FAIL  $path is present but empty" >&2
      rc=1
    else
      echo "  ok    $path"
    fi
  else
    echo "  FAIL  missing from image: $path" >&2
    rc=1
  fi
done
# Catalogue receipts must be real JSON, not Git LFS pointer files. A sparse or
# un-smudged checkout ships 129-byte "version https://git-lfs..." stubs; the
# control plane then crash-loops with "cited receipt is not JSON". Observed
# 2026-08-16 on the staging droplet when HEAD was built without `git lfs pull`.
receipt_dir="$tmpdir/runtime-benchmarks"
if "$RUNTIME" cp "$PROBE:/etc/merc/evidence/perf/runtime-benchmarks" "$receipt_dir" >/dev/null 2>&1; then
  receipt_count=0
  while IFS= read -r -d '' receipt; do
    receipt_count=$((receipt_count + 1))
    case "$(head -c 20 "$receipt" 2>/dev/null || true)" in
      version\ https://git-l*)
        echo "  FAIL  LFS pointer in image: ${receipt#"$receipt_dir"/}" >&2
        rc=1
        ;;
    esac
    case "$receipt" in
      *.json)
        if ! python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$receipt" 2>/dev/null; then
          echo "  FAIL  not JSON in image: ${receipt#"$receipt_dir"/}" >&2
          rc=1
        fi
        ;;
    esac
  done < <(command find "$receipt_dir" -type f -print0)
  if [ "$receipt_count" -lt 8 ]; then
    echo "  FAIL  expected cited runtime-benchmark receipts in the image, found $receipt_count" >&2
    rc=1
  else
    echo "  ok    $receipt_count runtime-benchmark receipts (not LFS pointers)"
  fi
else
  echo "  FAIL  missing from image: /etc/merc/evidence/perf/runtime-benchmarks" >&2
  rc=1
fi
rm -rf "$tmpdir"

if [ "$rc" != "0" ]; then
  echo "release image contents: FAIL -- control plane would 404 or fail-start on missing files" >&2
  exit 1
fi
echo "release image contents: PASS -- every runtime page/asset is inside the built image"
