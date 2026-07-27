#!/bin/sh
# Serialize every Playwright run through one lock and reap helpers on any exit.
# The Ocular Bible caps browsers at three and targets one; a leak is a failure,
# so the trap covers INT/TERM/HUP as well as normal exit.
LOCK="${TMPDIR:-/tmp}/visionmcp-browser.lock"
DEADLINE=$(( $(date +%s) + 1800 ))
until mkdir "$LOCK" 2>/dev/null; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && { echo "browser lock timeout: $LOCK" >&2; exit 75; }
  sleep 2
done
reap() {
  pkill -f "ms-playwright/webkit-" 2>/dev/null
  pkill -f "ms-playwright/firefox-" 2>/dev/null
  pkill -f "chrome-headless-shell" 2>/dev/null
  rmdir "$LOCK" 2>/dev/null
}
trap 'reap' EXIT INT TERM HUP
"$@"
status=$?
live=$(ps ax | grep -c '[m]s-playwright')
if [ "$live" -gt 3 ]; then
  echo "BROWSER LEAK: $live playwright processes still alive (cap 3)" >&2
  status=1
fi
exit $status
