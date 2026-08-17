#!/usr/bin/env bash
# Load a control image that was built on the operator Mac onto the droplet.
# The 1-vCPU host cannot compile linux/amd64. Run from the operator machine:
#
#   docker save merc/control:<short> | gzip > /tmp/merc-control.tar.gz
#   python3 scripts/alpha/rebuild-redeploy.py --archive /tmp/merc-control.tar.gz
#
# rebuild-redeploy.py ships ops/staging/compose.alpha.yml onto the droplet
# before it recreates control. That overlay pins
# MERC_CANARY_APPROVED_BUILD_HASHES=7cc01c442c7f6dbe. The host .env still
# carries only the superseded r5 hash f4303a751ca2b2af; loading the new
# binary without the compose pin makes /readyz a 503 configError. That is
# the boot check working. Do not rewrite .env and do not disable the check.
#
# This helper is the droplet-side half if the archive is already on the host.
# It never touches merc_pgdata or merc_miniodata.
set -euo pipefail

ARCHIVE="${1:-/tmp/merc-control-candidate.tar.gz}"
COMMIT="${2:?usage: load-operator-image.sh <archive> <40-char-commit>}"
[[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "commit must be 40 hex" >&2; exit 2; }

if [[ "$ARCHIVE" == *pgdata* || "$ARCHIVE" == *minio* ]]; then
  echo "refusing an archive path that mentions a data volume" >&2
  exit 2
fi

gunzip -c "$ARCHIVE" | docker load
docker tag "merc/control:${COMMIT:0:12}" "computexchange/control:${COMMIT}"
docker image inspect "computexchange/control:${COMMIT}" --format '{{.Id}} {{.Os}}/{{.Architecture}} {{index .Config.Labels "org.opencontainers.image.revision"}}'
# Keep the previous candidate so rollback does not require a rebuild.
# 9e31c65b is the digest that was serving mercmerc.net before this HEAD rebuild.
# 19fe0b23 is the historic prior from the earlier 2026-08-17 swap.
docker image inspect sha256:e0b642220dcd195c84290466cdaf90c2083c8740a87e6c3166d5817683f59fd3 --format 'prior {{.Id}} still present'
docker image inspect sha256:245dc92a5fffc1b9ffefe2452277797a498dc9cfb779dd915ae2631802175768 --format 'historic-prior {{.Id}} still present'
