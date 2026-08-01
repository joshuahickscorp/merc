#!/usr/bin/env bash
# Remove a merc supplier agent install.
#
# Already-installed agents may still use the pre-rebrand names:
#   binary:  $PREFIX/cx-agent
#   launchd: dev.computeexchange.agent
#   state:   ~/.compute-exchange
# This script removes both the current and the legacy names so operators are
# not left with an orphan they cannot uninstall after the process rename.
set -euo pipefail

PREFIX="${MERC_PREFIX:-$HOME/.local/bin}"
BIN="$PREFIX/merc-agent"
LEGACY_BIN="$PREFIX/cx-agent"
HOMEDIR="$HOME/.merc"
LEGACY_HOMEDIR="$HOME/.compute-exchange"
PLIST="$HOME/Library/LaunchAgents/dev.merc.agent.plist"
LEGACY_PLIST="$HOME/Library/LaunchAgents/dev.computeexchange.agent.plist"
SYSTEMD_UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/merc-agent.service"

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
  say "  LaunchAgent (legacy): $LEGACY_PLIST"
  say "  binary:      $BIN"
  say "  binary (legacy): $LEGACY_BIN"
  say "  systemd user service: $SYSTEMD_UNIT"
  say "  data/config: $HOMEDIR   $( [ "$PURGE" = 1 ] && echo '(--purge)' || echo '(kept; pass --purge to remove)')"
  say "  data/config (legacy): $LEGACY_HOMEDIR   $( [ "$PURGE" = 1 ] && echo '(--purge)' || echo '(kept; pass --purge to remove)')"
  say "  (downloaded model weights in the HF cache are never touched)"
  exit 0
fi

remove_launchagent() {
  local plist="$1"
  if [ -f "$plist" ]; then
    launchctl unload "$plist" 2>/dev/null || true
    rm -f "$plist"
    say "removed LaunchAgent $plist"
  fi
}

remove_binary() {
  local bin="$1"
  if [ -e "$bin" ]; then
    rm -f "$bin"
    say "removed $bin"
  fi
}

remove_launchagent "$PLIST"
remove_launchagent "$LEGACY_PLIST"
if [ -f "$SYSTEMD_UNIT" ]; then
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user disable --now merc-agent.service >/dev/null 2>&1 || true
  fi
  rm -f "$SYSTEMD_UNIT"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload >/dev/null 2>&1 || true
  fi
  say "removed systemd user service $SYSTEMD_UNIT"
fi
remove_binary "$BIN"
remove_binary "$LEGACY_BIN"

if [ "$PURGE" = "1" ]; then
  if [ -d "$HOMEDIR" ]; then
    rm -rf "$HOMEDIR"
    say "purged $HOMEDIR"
  fi
  if [ -d "$LEGACY_HOMEDIR" ]; then
    rm -rf "$LEGACY_HOMEDIR"
    say "purged $LEGACY_HOMEDIR"
  fi
else
  [ -d "$HOMEDIR" ] && say "kept $HOMEDIR (config + logs)  -  pass --purge to remove it"
  [ -d "$LEGACY_HOMEDIR" ] && say "kept $LEGACY_HOMEDIR (legacy config + logs)  -  pass --purge to remove it"
fi
say "done."
