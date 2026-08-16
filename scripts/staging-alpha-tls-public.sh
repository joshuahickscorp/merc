#!/usr/bin/env bash
# Flip mercmerc.net :443 from loopback to the public address.
# Lane L1 deliberately does not run this.
#
# On the droplet:
#   CADDY_TLS_BIND=0.0.0.0 docker compose \
#     -f /opt/merc/docker-compose.prod.yml \
#     -f /opt/merc/docker-compose.smallhost.yml \
#     -f /opt/merc/docker-compose.canary.yml \
#     up -d --no-deps caddy
set -euo pipefail
echo "refusing to publish :443 from this script; see the comment for the operator command" >&2
exit 2
