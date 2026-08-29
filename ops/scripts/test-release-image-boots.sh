#!/usr/bin/env bash
# Build the FINAL image from a clean tree and prove it serves.
#
# This exists because the production image could not start and nothing noticed.
# Dockerfile.control shipped the binary and three HTML pages; the catalogue price
# authority is read from a file that was never copied, so the container reached
# log.Fatalf("catalogue price authority unavailable") on every boot. The live host
# was up only because it had been assembled by hand -- meaning the revenue host
# could not be rebuilt from the repository at all.
#
# A unit test on the COPY list catches an omission. It cannot catch a path that
# resolves differently inside distroless, a WORKDIR change, or a file that is
# present but unreadable by the nonroot user. Only booting the artifact does that.
#
#   bash ops/scripts/test-release-image-boots.sh
#
# Skips (exit 0) when no container runtime is available, and says so. Fails when a
# runtime IS available and the image does not serve.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

if ! git diff --quiet || ! git diff --cached --quiet ||
   [ -n "$(git ls-files --others --exclude-standard)" ]; then
  echo "release image boot: FAIL -- exact artifact requires a clean HEAD" >&2
  exit 1
fi
HEAD_SHA="$(git rev-parse HEAD)" || exit 1
BUILD_VERSION="git-${HEAD_SHA:0:12}"
BUILD_DATE="$(git show -s --format=%cI HEAD)" || exit 1

RUNTIME=""
for candidate in docker podman; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" info >/dev/null 2>&1; then
    RUNTIME="$candidate"
    break
  fi
done
if [ -z "$RUNTIME" ]; then
  echo "release image boot: SKIPPED (no working docker or podman)"
  echo "  This gate is the only thing that proves the shipped artifact starts."
  echo "  Run it before any release."
  exit 0
fi

TAG="merc-control:boot-test-$$"
NET="merc-boot-$$"
DB="merc-boot-db-$$"
S3="merc-boot-s3-$$"
APP="merc-boot-app-$$"

cleanup() {
  "$RUNTIME" rm -f "$APP" "$DB" "$S3" >/dev/null 2>&1 || true
  "$RUNTIME" network rm "$NET" >/dev/null 2>&1 || true
  "$RUNTIME" rmi -f "$TAG" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "release image boot: building $TAG from exact HEAD ${HEAD_SHA:0:12}"
if ! "$RUNTIME" build -f Dockerfile.control -t "$TAG" \
  --build-arg "MERC_BUILD_VERSION=$BUILD_VERSION" \
  --build-arg "MERC_BUILD_COMMIT=$HEAD_SHA" \
  --build-arg "MERC_BUILD_DATE=$BUILD_DATE" \
  . >/tmp/merc-image-build.log 2>&1; then
  echo "release image boot: FAIL -- build" >&2
  tail -30 /tmp/merc-image-build.log >&2
  exit 1
fi

# The artifact must contain the price board. Checked inside the image rather than
# inferred from the Dockerfile, because that is the claim being made.
if ! "$RUNTIME" run --rm --entrypoint /merc "$TAG" --help >/dev/null 2>&1; then
  : # the binary may not support --help; the boot below is the real check
fi

# The one binary also provides the operator CLI. Its identity uses separate Go
# variables from the HTTP server, so /version alone cannot prove it was stamped.
cli_version_json="$($RUNTIME run --rm --entrypoint /merc "$TAG" version --json 2>/dev/null || echo '{}')"
cli_version="$(printf '%s' "$cli_version_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("version",""))' 2>/dev/null)"
cli_commit="$(printf '%s' "$cli_version_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("commit",""))' 2>/dev/null)"
cli_build_date="$(printf '%s' "$cli_version_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("build_date",""))' 2>/dev/null)"
if [ "$cli_version" != "$BUILD_VERSION" ] || [ "$cli_commit" != "$HEAD_SHA" ] ||
   [ "$cli_build_date" != "$BUILD_DATE" ]; then
  echo "release image boot: FAIL -- CLI artifact identity does not match exact HEAD" >&2
  echo "  expected version=$BUILD_VERSION commit=${HEAD_SHA:0:12} build_date=$BUILD_DATE" >&2
  echo "  observed version=${cli_version:-missing} commit=${cli_commit:0:12} build_date=${cli_build_date:-missing}" >&2
  exit 1
fi
echo "  ok    operator CLI identifies exact HEAD ${HEAD_SHA:0:12}"

"$RUNTIME" network create "$NET" >/dev/null 2>&1 || true
"$RUNTIME" run -d --name "$DB" --network "$NET" \
  -e POSTGRES_PASSWORD=boot -e POSTGRES_USER=merc -e POSTGRES_DB=merc \
  postgres:16-alpine >/dev/null 2>&1 || {
    echo "release image boot: SKIPPED (could not start a postgres sidecar)"; exit 0; }

for _ in $(seq 1 60); do
  "$RUNTIME" exec "$DB" pg_isready -U merc >/dev/null 2>&1 && break
  sleep 1
done

# Object storage is mandatory -- main.go refuses to start without it, which is
# correct: a control plane that cannot store an artifact cannot verify one.
"$RUNTIME" run -d --name "$S3" --network "$NET" \
  -e MINIO_ROOT_USER=bootminio -e MINIO_ROOT_PASSWORD=bootminio123 \
  minio/minio server /data >/dev/null 2>&1 || {
    echo "release image boot: SKIPPED (could not start a minio sidecar)"; exit 0; }
sleep 4

# Production-SHAPED configuration: every variable docker-compose.prod.yml marks
# required, and nothing mounted from the host. If this needs a host file, the
# artifact is not self-contained and the gate has done its job.
"$RUNTIME" run -d --name "$APP" --network "$NET" -p 18080:8080 \
  -e DATABASE_URL="postgres://merc:boot@$DB:5432/merc?sslmode=disable" \
  -e MERC_ENV=production \
  -e MERC_PRICE_BOARD=/etc/merc/pricing/board.json \
  -e MERC_SETTLEMENT_CURRENCY=CAD \
  -e MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE=1.37 \
  -e MERC_PRICE_FX_REVISION=boot-test \
  -e MERC_TOKEN_KEY="release-image-boot-token-key-at-least-32-bytes" \
  -e MERC_VERIFICATION_SAMPLE_SECRET="release-image-boot-sampling-secret-32-bytes" \
  -e MERC_ECON_SCHEDULE_VERSION=boot-test \
  -e MERC_PROCESSOR_PERCENT_BPS=290 \
  -e MERC_PROCESSOR_FIXED_USD=0.30 \
  -e MERC_CONTROL_PLANE_PER_BATCH_USD=0.0001 \
  -e MERC_MIN_CONTRIBUTION_PER_BATCH_USD=0.000001 \
  -e MERC_TARGET_MARGIN_BPS=2000 \
  -e MERC_ADMIN_CIDRS=127.0.0.1/32 \
  -e MERC_PUBLIC_CONTROL_ORIGIN=https://merc.test \
  -e MERC_SUPPORT_EMAIL=support@merc.test \
  -e MERC_SECURITY_EMAIL=security@merc.test \
  -e MERC_STATUS_URL=https://status.merc.test \
  -e MERC_TERMS_URL=https://merc.test/terms \
  -e MERC_PRIVACY_URL=https://merc.test/privacy \
  -e S3_ENDPOINT="http://$S3:9000" \
  -e S3_BUCKET=cx-jobs \
  -e S3_ACCESS_KEY=bootminio \
  -e S3_SECRET_KEY=bootminio123 \
  -e MERC_RUN_WORKERS=false \
  "$TAG" >/dev/null 2>&1 || { echo "release image boot: FAIL -- container would not start" >&2; exit 1; }

probe() { curl -fsS -m 5 "http://127.0.0.1:18080$1" 2>/dev/null; }

ready=0
for _ in $(seq 1 60); do
  if probe /healthz >/dev/null; then ready=1; break; fi
  if [ "$("$RUNTIME" inspect -f '{{.State.Running}}' "$APP" 2>/dev/null)" != "true" ]; then
    echo "release image boot: FAIL -- the container exited" >&2
    "$RUNTIME" logs "$APP" 2>&1 | tail -30 >&2
    exit 1
  fi
  sleep 1
done
if [ "$ready" != "1" ]; then
  echo "release image boot: FAIL -- /healthz never answered" >&2
  "$RUNTIME" logs "$APP" 2>&1 | tail -30 >&2
  exit 1
fi

status=0
check() { # path, description
  if probe "$1" >/dev/null; then
    echo "  ok    $1"
  else
    echo "  FAIL  $1  ($2)" >&2
    status=1
  fi
}
echo "release image boot: probing the running artifact"
check /healthz    "liveness"
check /version    "build and price-board identity"
check /prices     "the price page the image did not used to ship"
check /pricing/board.json "the catalogue board data route"
check /.well-known/security.txt "security contact"

# The price board must have loaded FROM THE RELEASE ARTIFACT. A container that
# started by finding a board somewhere else is the failure this gate exists for.
version="$(probe /version || echo '{}')"
source="$(printf '%s' "$version" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("price_board_source",""))' 2>/dev/null)"
digest="$(printf '%s' "$version" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("price_board_sha256",""))' 2>/dev/null)"
runtime_commit="$(printf '%s' "$version" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("commit",""))' 2>/dev/null)"
image_commit="$("$RUNTIME" image inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$TAG" 2>/dev/null)"
case "$source" in
  release|env) echo "  ok    price board loaded from $source (${digest:0:12})" ;;
  *) echo "  FAIL  price board source is '$source', want release or env" >&2; status=1 ;;
esac
if [ "$runtime_commit" = "$HEAD_SHA" ] && [ "$image_commit" = "$HEAD_SHA" ]; then
  echo "  ok    runtime and image label identify exact HEAD ${HEAD_SHA:0:12}"
else
  echo "  FAIL  artifact identity runtime=${runtime_commit:-missing} label=${image_commit:-missing} want=$HEAD_SHA" >&2
  status=1
fi

if [ "$status" != "0" ]; then
  echo "release image boot: FAIL" >&2
  "$RUNTIME" logs "$APP" 2>&1 | tail -20 >&2
  exit 1
fi
echo "release image boot: PASS -- the shipped artifact starts and serves with no host files"
