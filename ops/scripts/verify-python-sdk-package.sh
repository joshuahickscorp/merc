#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PYTHON_BIN="${PYTHON_BIN:-python3}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/merc-python-sdk.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

"$PYTHON_BIN" -m venv "$WORK/venv"
PY="$WORK/venv/bin/python"
cp -R "$ROOT/clients/sdk/python" "$WORK/sdk-python"
rm -rf \
  "$WORK/sdk-python/build" \
  "$WORK/sdk-python/dist" \
  "$WORK/sdk-python/merc.egg-info"
find "$WORK/sdk-python" -type d -name __pycache__ -prune -exec rm -rf {} +
find "$WORK/sdk-python" -type f \( -name '*.pyc' -o -name '*.pyo' \) -delete

"$PY" -m pip install --disable-pip-version-check --quiet "$WORK/sdk-python"
"$PY" -m pip check

cd "$WORK"
MERC_SDK_SOURCE_ROOTS="$ROOT/clients/sdk/python:$WORK/sdk-python" \
  PYTHONNOUSERSITE=1 \
  "$PY" -m unittest discover \
    -s "$ROOT/clients/sdk/python/tests" \
    -p 'test_*.py' \
    -v

"$PY" - <<'PY'
from merc import Client, __version__

client = Client("https://mercmerc.net", "cx_test_not_sent")
print(f"installed merc {__version__} from {client.__class__.__module__}")
PY
