#!/usr/bin/env bash
# Run one command against a disposable, pinned MinIO sidecar. This keeps real
# object-storage paths in CI without sharing a bucket or a host port with an
# app, candidate, or another test run.
set -euo pipefail

[ "$#" -gt 0 ] || {
  echo "usage: with-isolated-test-storage.sh command [args...]" >&2
  exit 2
}

for command in docker curl python3; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "with-isolated-test-storage: missing required command $command" >&2
    exit 2
  }
done
docker info >/dev/null 2>&1 || {
  echo "with-isolated-test-storage: Docker daemon is unavailable" >&2
  exit 2
}

suffix="$(python3 - <<'PY'
import secrets
print(secrets.token_hex(12))
PY
)"
container="merc_test_s3_${suffix}"
bucket="merc-test-${suffix}"
minio_image='minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e'
mc_image='minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727'

cleanup() {
  # container is generated locally from a hex nonce and names only this run.
  docker rm -f "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker run -d --name "$container" -p '127.0.0.1::9000' \
  -e MINIO_ROOT_USER=merc_test_minio \
  -e MINIO_ROOT_PASSWORD=merc_test_minio_password \
  "$minio_image" server /data >/dev/null

endpoint=''
for _ in $(seq 1 120); do
  mapped="$(docker port "$container" 9000/tcp | head -n 1 || true)"
  if [[ "$mapped" =~ ^127\.0\.0\.1:[0-9]+$ ]] && \
    curl -fsS --max-time 1 "http://${mapped}/minio/health/live" >/dev/null 2>&1; then
    endpoint="http://${mapped}"
    break
  fi
  sleep 0.25
done
[ -n "$endpoint" ] || {
  echo "with-isolated-test-storage: MinIO did not become healthy" >&2
  exit 1
}

docker run --rm --network "container:${container}" --entrypoint /bin/sh "$mc_image" -c \
  "mc alias set local http://localhost:9000 merc_test_minio merc_test_minio_password >/dev/null && mc mb local/${bucket} >/dev/null"

MERC_TEST_S3_ENDPOINT="$endpoint" \
MERC_TEST_S3_PUBLIC_ENDPOINT="$endpoint" \
MERC_TEST_S3_BUCKET="$bucket" \
MERC_TEST_S3_ACCESS_KEY=merc_test_minio \
MERC_TEST_S3_SECRET_KEY=merc_test_minio_password \
"$@"
