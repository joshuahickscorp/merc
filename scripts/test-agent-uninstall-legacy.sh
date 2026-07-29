#!/usr/bin/env bash
# Prove uninstall.sh still removes a pre-rebrand agent install.
#
# Already-installed suppliers may still have:
#   $PREFIX/cx-agent
#   ~/Library/LaunchAgents/dev.computeexchange.agent.plist
#   ~/.compute-exchange/
# After the process rename those names must not become orphans. This check
# fails if uninstall.sh stops handling any of them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNINSTALL="$ROOT/scripts/uninstall.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/merc-uninstall-legacy.XXXXXX")"
trap 'rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

# ---------------------------------------------------------------------------
# The script text itself must keep the legacy names (not just the new ones).
# ---------------------------------------------------------------------------
for token in 'cx-agent' 'dev.computeexchange.agent' '.compute-exchange' 'merc-agent' 'dev.merc.agent' '.merc'; do
  grep -Fq "$token" "$UNINSTALL" \
    || fail "uninstall.sh no longer mentions required name: $token"
done
pass "uninstall.sh retains both modern and legacy names"

# ---------------------------------------------------------------------------
# Dry-run check mode lists both trees.
# ---------------------------------------------------------------------------
check_out="$(HOME="$work/home" MERC_PREFIX="$work/bin" bash "$UNINSTALL" --check)"
printf '%s\n' "$check_out" | grep -Fq 'cx-agent' \
  || fail "--check did not list legacy binary path"
printf '%s\n' "$check_out" | grep -Fq 'dev.computeexchange.agent' \
  || fail "--check did not list legacy launchd label"
printf '%s\n' "$check_out" | grep -Fq '.compute-exchange' \
  || fail "--check did not list legacy state dir"
printf '%s\n' "$check_out" | grep -Fq 'merc-agent' \
  || fail "--check did not list modern binary path"
pass "uninstall --check lists modern and legacy paths"

# ---------------------------------------------------------------------------
# A fixture that only has the legacy layout is fully removed.
# ---------------------------------------------------------------------------
mkdir -p "$work/bin" \
  "$work/home/Library/LaunchAgents" \
  "$work/home/.compute-exchange/data"
printf '#!/bin/sh\necho legacy-agent\n' > "$work/bin/cx-agent"
chmod +x "$work/bin/cx-agent"
cat >"$work/home/Library/LaunchAgents/dev.computeexchange.agent.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.computeexchange.agent</string>
</dict></plist>
PLIST
printf 'legacy\n' > "$work/home/.compute-exchange/agent.toml"

HOME="$work/home" MERC_PREFIX="$work/bin" bash "$UNINSTALL" --purge

[[ ! -e "$work/bin/cx-agent" ]] \
  || fail "legacy binary still present after uninstall --purge"
[[ ! -e "$work/home/Library/LaunchAgents/dev.computeexchange.agent.plist" ]] \
  || fail "legacy LaunchAgent still present after uninstall --purge"
[[ ! -d "$work/home/.compute-exchange" ]] \
  || fail "legacy state dir still present after uninstall --purge"
pass "legacy-only install is fully removed by uninstall --purge"

# ---------------------------------------------------------------------------
# A mixed modern+legacy install removes both.
# ---------------------------------------------------------------------------
mkdir -p "$work/bin" \
  "$work/home/Library/LaunchAgents" \
  "$work/home/.merc" \
  "$work/home/.compute-exchange"
printf '#!/bin/sh\necho modern\n' > "$work/bin/merc-agent"
printf '#!/bin/sh\necho legacy\n' > "$work/bin/cx-agent"
chmod +x "$work/bin/merc-agent" "$work/bin/cx-agent"
: >"$work/home/Library/LaunchAgents/dev.merc.agent.plist"
: >"$work/home/Library/LaunchAgents/dev.computeexchange.agent.plist"
: >"$work/home/.merc/agent.toml"
: >"$work/home/.compute-exchange/agent.toml"

HOME="$work/home" MERC_PREFIX="$work/bin" bash "$UNINSTALL" --purge

for path in \
  "$work/bin/merc-agent" \
  "$work/bin/cx-agent" \
  "$work/home/Library/LaunchAgents/dev.merc.agent.plist" \
  "$work/home/Library/LaunchAgents/dev.computeexchange.agent.plist"
do
  [[ ! -e "$path" ]] || fail "still present after mixed uninstall: $path"
done
[[ ! -d "$work/home/.merc" ]] || fail "modern state dir still present"
[[ ! -d "$work/home/.compute-exchange" ]] || fail "legacy state dir still present"
pass "mixed modern+legacy install is fully removed"

printf '\nagent uninstall legacy compatibility: PASS\n'
