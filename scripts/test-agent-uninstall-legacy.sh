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

# ---------------------------------------------------------------------------
# Without --purge, secrets stay and the script must name them. With --purge,
# credentials, enrollment keys, prefs and logs are gone.
# ---------------------------------------------------------------------------
mkdir -p "$work/bin" \
  "$work/home/Library/LaunchAgents" \
  "$work/home/.merc/enrollment" \
  "$work/home/.merc/data"
printf '#!/bin/sh\necho modern\n' > "$work/bin/merc-agent"
chmod +x "$work/bin/merc-agent"
: >"$work/home/Library/LaunchAgents/dev.merc.agent.plist"
printf 'worker_token = "secret-token"\n' > "$work/home/.merc/agent.toml"
printf 'paused = false\n' > "$work/home/.merc/agent.prefs.toml"
printf 'ENCRYPTED_KEY\n' > "$work/home/.merc/enrollment/device.p256.pkcs8"
printf 'pending\n' > "$work/home/.merc/enrollment/pending_request.json"
printf 'log\n' > "$work/home/.merc/agent.log"
printf 'blob\n' > "$work/home/.merc/data/job.bin"

keep_out="$(HOME="$work/home" MERC_PREFIX="$work/bin" bash "$UNINSTALL" 2>&1)"
printf '%s\n' "$keep_out" | grep -Fq 'pass --purge to delete retained data' \
  || fail "uninstall without --purge did not tell the operator secrets remain"
printf '%s\n' "$keep_out" | grep -Fq 'agent.toml' \
  || fail "uninstall without --purge did not list retained agent.toml"
[[ -f "$work/home/.merc/agent.toml" ]] \
  || fail "uninstall without --purge deleted agent.toml"
[[ -f "$work/home/.merc/enrollment/device.p256.pkcs8" ]] \
  || fail "uninstall without --purge deleted enrollment key"
pass "uninstall without --purge retains secrets and names them"

HOME="$work/home" MERC_PREFIX="$work/bin" bash "$UNINSTALL" --purge >/dev/null
[[ ! -d "$work/home/.merc" ]] || fail "purge left ~/.merc behind"
[[ ! -e "$work/bin/merc-agent" ]] || fail "purge left binary behind"
pass "uninstall --purge deletes config, enrollment keys, logs and data"

printf '\nagent uninstall legacy compatibility: PASS\n'
