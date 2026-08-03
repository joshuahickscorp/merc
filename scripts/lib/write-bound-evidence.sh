#!/usr/bin/env bash
# Shell front-end for scripts/write-bound-evidence.py.
#
# Scripts that land JSON under evidence/ must call merc_emit_bound_json rather
# than cp/mv/jq-redirect into the tree. Requires ROOT to be set to the repo root.
#
# Usage:
#   merc_emit_bound_json OUT_PATH HARNESS PAYLOAD_FILE [extra write-bound-evidence.py flags...]
#
# Extra flags are forwarded (e.g. --image-digest, --model-na, --exact-config).
# build_digest is derived from --build-binary (default: the harness script path
# when it is a readable file under ROOT, else write-bound-evidence.py).

merc_emit_bound_json() {
  local out="${1:?merc_emit_bound_json: out path required}"
  local harness="${2:?merc_emit_bound_json: harness required}"
  local payload="${3:?merc_emit_bound_json: payload file required}"
  shift 3

  if [ -z "${ROOT:-}" ]; then
    echo "merc_emit_bound_json: ROOT is not set" >&2
    return 2
  fi
  if [ ! -f "$payload" ]; then
    echo "merc_emit_bound_json: payload not found: $payload" >&2
    return 2
  fi

  local build_binary="${MERC_BOUND_BUILD_BINARY:-}"
  if [ -z "$build_binary" ]; then
    if [ -f "$ROOT/$harness" ]; then
      build_binary="$ROOT/$harness"
    elif [ -f "$harness" ]; then
      build_binary="$harness"
    else
      build_binary="$ROOT/scripts/write-bound-evidence.py"
    fi
  fi

  python3 "$ROOT/scripts/write-bound-evidence.py" \
    --out "$out" \
    --harness "$harness" \
    --payload-file "$payload" \
    --build-binary "$build_binary" \
    "$@"
}
