#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFIX="${CX_PREFIX:-$HOME/.local/bin}"
BIN="$PREFIX/cx-agent"
HOMEDIR="$HOME/.compute-exchange"
CONFIG="$HOMEDIR/agent.toml"
PLIST="$HOME/Library/LaunchAgents/dev.computeexchange.agent.plist"
LABEL="dev.computeexchange.agent"

say()  { printf '\033[36m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[install]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[install] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

MODE="install"
case "${1:-}" in
  --check)     MODE="check" ;;
  --start)     MODE="start" ;;
  --uninstall) exec "$ROOT/scripts/uninstall.sh" "${@:2}" ;;
  -h|--help)   grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
  "")          : ;;
  *)           die "unknown flag ${1} (try --help)" ;;
esac

[ "$(uname -s)" = "Darwin" ] || die "macOS only (Apple Silicon is the supported supply target)"
command -v cargo >/dev/null 2>&1 || die "Rust toolchain (cargo) required  -  install from https://rustup.rs"
ARCH="$(uname -m)"
[ "$ARCH" = "arm64" ] || warn "arch is $ARCH, not arm64  -  the agent runs but Metal acceleration needs Apple Silicon"

if [ "$MODE" = "check" ]; then
  say "dry run  -  prerequisites OK (macOS + cargo). This install would:"
  say "  1. build the release agent:  (cd $ROOT/agent && cargo build --release)"
  say "  2. install the binary to:    $BIN"
  say "  3. write a starter config:   $CONFIG  (CX_CONTROL_URL / CX_WORKER_TOKEN)"
  say "  4. install a LaunchAgent:     $PLIST  (runs the agent at login)"
  say "Run without --check to perform the install; --uninstall to remove it."
  exit 0
fi

say "building the release agent (first build downloads + compiles deps; a few minutes)…"
( cd "$ROOT/agent" && cargo build --release ) || die "agent build failed"
SRC_BIN="$ROOT/agent/target/release/cx-agent"
[ -x "$SRC_BIN" ] || die "built binary not found at $SRC_BIN"

mkdir -p "$PREFIX"
install -m 0755 "$SRC_BIN" "$BIN"
say "installed $BIN ($("$BIN" version 2>/dev/null || echo cx-agent))"

mkdir -p "$HOMEDIR"
if [ -f "$CONFIG" ]; then
  say "keeping existing config $CONFIG"
else
  umask 077
  cat >"$CONFIG" <<TOML
control_url = "${CX_CONTROL_URL:-http://localhost:8080}"
worker_token = "${CX_WORKER_TOKEN:-PASTE_WORKER_TOKEN_FROM_make_seed}"
supplier_id = "00000000-0000-0000-0000-000000000000"
max_cpu_pct = 80.0
power_only = true            # don't run on battery
min_payout_usd_per_hr = 0.05 # reservation price floor
data_dir = "${HOMEDIR}/data"
TOML
  say "wrote starter config $CONFIG"
  [ -n "${CX_WORKER_TOKEN:-}" ] || warn "set worker_token in $CONFIG (from 'make seed') before earning"
fi

cat >"$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array><string>${BIN}</string><string>run</string><string>--config</string><string>${CONFIG}</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${HOMEDIR}/agent.log</string>
  <key>StandardErrorPath</key><string>${HOMEDIR}/agent.log</string>
</dict></plist>
PLIST_EOF
say "installed LaunchAgent $PLIST"

if [ "$MODE" = "start" ]; then
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST" && say "agent started (launchctl). Logs: $HOMEDIR/agent.log"
else
  say "to start now:  launchctl load $PLIST   (or re-run with --start)"
fi
say "done. Menu-bar app: see macapp/ (build with: swift build --package-path macapp)."
