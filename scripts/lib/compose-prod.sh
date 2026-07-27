# Shared compose invocation for the production application + observability stack.
# Callers set `set -euo pipefail` and ROOT before sourcing.

cx_prod_compose() {
  if [ -n "${MERC_COMPOSE_FILE:-}" ]; then
    docker compose -f "$MERC_COMPOSE_FILE" "$@"
    return
  fi
  docker compose \
    -f "$ROOT/docker-compose.prod.yml" \
    -f "$ROOT/docker-compose.observability.yml" \
    "$@"
}

cx_require_alert_receiver_secret() {
  local path="${MERC_ALERT_RECEIVER_URL_FILE:-}"
  if [ -z "$path" ]; then
    echo "deploy: MERC_ALERT_RECEIVER_URL_FILE is required (absolute path to a file containing the HTTPS alert webhook URL). See monitoring/README.md." >&2
    return 1
  fi
  if [ ! -f "$path" ]; then
    echo "deploy: MERC_ALERT_RECEIVER_URL_FILE=$path does not exist. Refusing to start an alerting stack with no receiver." >&2
    return 1
  fi
  if [ ! -s "$path" ]; then
    echo "deploy: MERC_ALERT_RECEIVER_URL_FILE=$path is empty. Refusing to start an alerting stack with no receiver." >&2
    return 1
  fi
  local url
  url="$(tr -d '\r\n' <"$path")"
  case "$url" in
    https://*) ;;
    *)
      echo "deploy: alert receiver URL in $path must be an https:// webhook (got non-HTTPS or empty after trim)." >&2
      return 1
      ;;
  esac
}
