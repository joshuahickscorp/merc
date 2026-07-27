#!/bin/sh
# Serialize every Playwright run on this machine behind one lock, and reap
# leaked engine processes before and after. At most one browser is ever alive.
# Usage: scripts/with-one-browser.sh <command...>
LOCK=/tmp/bvmcp-browser.lock
sh "$(dirname "$0")/reap-browsers.sh" >/dev/null 2>&1
# shellcheck disable=SC2064
trap "sh '$(dirname "$0")/reap-browsers.sh' >/dev/null 2>&1" EXIT INT TERM
if command -v shlock >/dev/null 2>&1 || ! command -v flock >/dev/null 2>&1; then
  # macOS has no flock(1); mkdir is the portable atomic lock.
  while ! mkdir "$LOCK" 2>/dev/null; do sleep 2; done
  trap "rmdir '$LOCK' 2>/dev/null; sh '$(dirname "$0")/reap-browsers.sh' >/dev/null 2>&1" EXIT INT TERM
fi
"$@"
status=$?
exit $status
