#!/usr/bin/env bash
set -euo pipefail

PREFIX="${CX_PREFIX:-$HOME/.local/bin}"
BIN="$PREFIX/cx-agent"
HOMEDIR="$HOME/.compute-exchange"
PLIST="$HOME/Library/LaunchAgents/dev.computeexchange.agent.plist"

say(){ printf '\033[36m[uninstall]\033[0m %s\n' "$*"; }

PURGE=0; CHECK=0
case "${1:-}" in
  --purge) PURGE=1 ;;
  --check) CHECK=1 ;;
  -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
esac

if [ "$CHECK" = "1" ]; then
  say "dry run  -  would remove:"
  say "  LaunchAgent: $PLIST"
  say "  binary:      $BIN"
  say "  data/config: $HOMEDIR   $( [ "$PURGE" = 1 ] && echo '(--purge)' || echo '(kept; pass --purge to remove)')"
  say "  (downloaded model weights in the HF cache are never touched)"
  exit 0
fi

if [ -f "$PLIST" ]; then
  launchctl unload "$PLIST" 2>/dev/null || true
  rm -f "$PLIST"
  say "removed LaunchAgent"
fi
if [ -e "$BIN" ]; then
  rm -f "$BIN"
  say "removed $BIN"
fi
if [ "$PURGE" = "1" ] && [ -d "$HOMEDIR" ]; then
  rm -rf "$HOMEDIR"
  say "purged $HOMEDIR"
else
  [ -d "$HOMEDIR" ] && say "kept $HOMEDIR (config + logs)  -  pass --purge to remove it"
fi
say "done."
