#!/bin/sh
# Playwright leaks helper processes when a run is interrupted. Reap them between
# browser runs so at most one engine set is ever live on this machine.
pkill -f "ms-playwright/webkit-" 2>/dev/null
pkill -f "ms-playwright/firefox-" 2>/dev/null
pkill -f "chrome-headless-shell" 2>/dev/null
sleep 1
echo "live playwright processes: $(ps ax | grep -c '[m]s-playwright')"
