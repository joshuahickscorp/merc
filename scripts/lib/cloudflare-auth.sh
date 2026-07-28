#!/usr/bin/env bash
# Shared Cloudflare auth. Two credential types, one interface.
#
#   CLOUDFLARE_API_TOKEN                     scoped token, Authorization: Bearer
#   CLOUDFLARE_EMAIL + CLOUDFLARE_GLOBAL_API_KEY   global key, X-Auth-* headers
#
# The Global API Key is UNSCOPED: it can do anything the account owner can do,
# on every zone and every product, and it cannot be narrowed or given a TTL.
# That is exactly why it solves a permission puzzle instantly, and exactly why
# it should be rolled once the job is done.
#
# The GLOBAL KEY WINS when both are present, which is the opposite of the usual
# least-privilege instinct and is deliberate here: the scoped token is the one
# that provably cannot do the job (it lists zones, then returns 9109 on delete
# and an empty list on /accounts). Preferring it would mean silently retrying
# the credential already known to fail. Set MERC_CF_PREFER_TOKEN=1 to invert
# this once a token with account-scope permissions exists.
#
# Either way the secret goes to curl through --config on stdin, never argv, so
# `ps` cannot read it.

cf_auth_config() {
  local have_global=0
  [ -n "${CLOUDFLARE_EMAIL:-}" ] && [ -n "${CLOUDFLARE_GLOBAL_API_KEY:-}" ] && have_global=1
  if [ "$have_global" -eq 1 ] && [ "${MERC_CF_PREFER_TOKEN:-0}" != "1" ]; then
    printf 'header = "X-Auth-Email: %s"\n' "$CLOUDFLARE_EMAIL"
    printf 'header = "X-Auth-Key: %s"\n' "$CLOUDFLARE_GLOBAL_API_KEY"
  elif [ -n "${CLOUDFLARE_API_TOKEN:-}" ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$CLOUDFLARE_API_TOKEN"
  elif [ "$have_global" -eq 1 ]; then
    printf 'header = "X-Auth-Email: %s"\n' "$CLOUDFLARE_EMAIL"
    printf 'header = "X-Auth-Key: %s"\n' "$CLOUDFLARE_GLOBAL_API_KEY"
  else
    return 1
  fi
}

cf_auth_kind() {
  local have_global=0
  [ -n "${CLOUDFLARE_EMAIL:-}" ] && [ -n "${CLOUDFLARE_GLOBAL_API_KEY:-}" ] && have_global=1
  if [ "$have_global" -eq 1 ] && [ "${MERC_CF_PREFER_TOKEN:-0}" != "1" ]; then printf 'GLOBAL API KEY (unscoped)'
  elif [ -n "${CLOUDFLARE_API_TOKEN:-}" ]; then printf 'scoped token'
  elif [ "$have_global" -eq 1 ]; then printf 'GLOBAL API KEY (unscoped)'
  else printf 'none'; fi
}

cf_request() {
  local method="$1" path="$2" body="${3:-}"
  local cfg; cfg=$(cf_auth_config) || return 1
  if [ -n "$body" ]; then
    printf '%s' "$cfg" | curl -sS --config - -H 'content-type: application/json' \
      --max-time 45 -X "$method" "https://api.cloudflare.com/client/v4$path" -d "$body"
  else
    printf '%s' "$cfg" | curl -sS --config - -H 'content-type: application/json' \
      --max-time 45 -X "$method" "https://api.cloudflare.com/client/v4$path"
  fi
}
