#!/usr/bin/env bash
# Packaging contract: the darwin release tarball must ship merc-agent.sb next to
# the binary. Shipping the containment story without the profile is the defect
# that left every stock macOS supplier uncontained.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKG="$ROOT/scripts/package-agent-binary.sh"
PROFILE="$ROOT/clients/macapp/ComputeExchangeAgent/merc-agent.sb"
INSTALL="$ROOT/scripts/install.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[[ -x "$PKG" ]] || fail "package-agent-binary.sh missing or not executable"
[[ -f "$PROFILE" ]] || fail "seatbelt profile missing at $PROFILE"
[[ -f "$INSTALL" ]] || fail "install.sh missing"

# Static contract: packaging and install must both name the profile so they
# cannot drift from the sibling-resolution path in the agent.
grep -Fq 'merc-agent.sb' "$PKG" || fail "package-agent-binary.sh does not stage merc-agent.sb"
grep -Fq 'darwin' "$PKG" || fail "package-agent-binary.sh has no darwin branch"
grep -Fq 'install_darwin_seatbelt_profile' "$INSTALL" || fail "install.sh does not install the seatbelt profile"
grep -Fq 'merc-agent.sb' "$INSTALL" || fail "install.sh never mentions merc-agent.sb"

# Functional contract: build a darwin tarball from a fake binary and assert the
# archive contains merc-agent.sb at the staged path.
WORK="$(mktemp -d "${TMPDIR:-/tmp}/merc-agent-pkg-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

FAKE_BIN="$WORK/merc-agent"
printf '#!/bin/sh\necho fake-merc-agent\n' >"$FAKE_BIN"
chmod 0755 "$FAKE_BIN"

export MERC_AGENT_OUT="$WORK/out"
export MERC_AGENT_VERSION="test-seatbelt-1"
mkdir -p "$MERC_AGENT_OUT"

bash "$PKG" darwin arm64 "$FAKE_BIN" >/dev/null

ARCHIVE="$MERC_AGENT_OUT/merc-agent_test-seatbelt-1_darwin_arm64.tar.gz"
[[ -f "$ARCHIVE" ]] || fail "expected archive not written: $ARCHIVE"

LIST="$(tar -tzf "$ARCHIVE")"
printf '%s\n' "$LIST" | grep -Eq '(^|/)merc-agent$' \
  || fail "darwin tarball missing merc-agent binary; contents:\n$LIST"
printf '%s\n' "$LIST" | grep -Eq '(^|/)merc-agent\.sb$' \
  || fail "darwin tarball missing merc-agent.sb (containment drifted from packaging); contents:\n$LIST"

# Profile bytes in the archive must match the source of truth so a stale copy
# cannot be shipped by accident.
EXTRACT="$WORK/extract"
mkdir -p "$EXTRACT"
tar -C "$EXTRACT" -xzf "$ARCHIVE"
STAGED="$(find "$EXTRACT" -type f -name merc-agent.sb | head -n 1)"
[[ -n "$STAGED" ]] || fail "extracted archive has no merc-agent.sb"
cmp -s "$PROFILE" "$STAGED" || fail "staged merc-agent.sb differs from clients/macapp/ComputeExchangeAgent/merc-agent.sb"

# Linux packages must not be forced to carry the macOS seatbelt file (different
# containment story), but must still succeed.
bash "$PKG" linux amd64 "$FAKE_BIN" >/dev/null
LINUX_ARCHIVE="$MERC_AGENT_OUT/merc-agent_test-seatbelt-1_linux_amd64.tar.gz"
[[ -f "$LINUX_ARCHIVE" ]] || fail "linux archive not written"
LINUX_LIST="$(tar -tzf "$LINUX_ARCHIVE")"
printf '%s\n' "$LINUX_LIST" | grep -Eq 'merc-agent\.sb' \
  && fail "linux tarball unexpectedly contains merc-agent.sb" || true

printf 'agent package seatbelt contract: PASS\n'
