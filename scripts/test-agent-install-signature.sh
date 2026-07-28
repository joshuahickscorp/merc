#!/usr/bin/env bash
# Publisher-identity gate on the supplier agent install path.
#
# The agent runs buyer workloads on a supplier machine, so "who published this
# binary" is the whole question. Two failure modes are checked here, both of
# which the install path has actually had:
#
#   1. an unpinned `cosign verify-blob` -- cosign v2 refuses the call outright
#      ("--certificate-identity or --certificate-identity-regexp is required"),
#      so every install that had cosign on PATH aborted and the signature was
#      never verified by anyone;
#   2. silently falling back to SHA256SUMS, which is fetched from the same host
#      as the archive and therefore authenticates nothing on its own.
#
# No OIDC is available offline, so a real signature cannot be produced here.
# What is tested is the gating: unsigned artifacts must not install by default,
# and the pinned identity must reject a foreign publisher.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="$ROOT/scripts/install.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/merc-install-sig.XXXXXX")"
trap 'rm -rf "$work"; [[ -n "${server_pid:-}" ]] && kill "$server_pid" 2>/dev/null' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

# ---------------------------------------------------------------- fixture

release="$work/release"
mkdir -p "$release"
version="v0.0.0-test"
name="cx-agent_${version}_$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz"

mkdir -p "$work/payload"
printf '#!/bin/sh\necho cx-agent test fixture\n' > "$work/payload/cx-agent"
chmod +x "$work/payload/cx-agent"
tar -C "$work/payload" -czf "$release/$name" cx-agent

digest="$(cd "$release" && { command -v sha256sum >/dev/null 2>&1 && sha256sum "$name" || shasum -a 256 "$name"; })"
printf '%s\n' "$digest" > "$release/SHA256SUMS"

python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$release" >"$work/server.log" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
  port="$(sed -nE 's/.*port ([0-9]+).*/\1/p' "$work/server.log" | head -1)"
  [[ -n "$port" ]] && break
  sleep 0.1
done
[[ -n "${port:-}" ]] || fail "fixture server did not start"
base="http://127.0.0.1:$port"

run_install() {
  env MERC_AGENT_VERSION="$version" \
      MERC_AGENT_BASE_URL="$base" \
      MERC_PREFIX="$work/bin" \
      "$@" \
      bash "$INSTALL" 2>&1 || true
}

# ------------------------------------------------------------------ cases

if ! command -v cosign >/dev/null 2>&1; then
  printf 'SKIP: cosign is not installed; the identity gate cannot be exercised\n'
  exit 0
fi

# 1. No signature published. Digest alone came from the same host as the
#    archive, so this must not install.
output="$(run_install)"
grep -q "no cosign bundle published" <<<"$output" \
  || fail "unsigned artifact was not refused; got: $(tail -2 <<<"$output")"
[[ ! -x "$work/bin/cx-agent" ]] || fail "unsigned artifact was installed"
pass "unsigned artifact is refused by default"

# 2. The operator can accept digest-only verification, but only on purpose.
output="$(run_install MERC_AGENT_ALLOW_UNVERIFIED=1)"
grep -q "no signature published" <<<"$output" \
  || fail "explicit opt-out did not proceed; got: $(tail -2 <<<"$output")"
[[ -x "$work/bin/cx-agent" ]] || fail "explicit opt-out did not install"
pass "digest-only install requires MERC_AGENT_ALLOW_UNVERIFIED=1"

# 3. A bundle that exists but does not verify against the pinned publisher must
#    fail closed rather than counting as a signature.
rm -f "$work/bin/cx-agent"
printf '{"base64Signature":"","cert":"","rekorBundle":{}}' > "$release/${name}.cosign.bundle"
printf '{"base64Signature":"","cert":"","rekorBundle":{}}' > "$release/SHA256SUMS.cosign.bundle"
output="$(run_install)"
grep -q "cosign could not verify" <<<"$output" \
  || fail "a bundle from the wrong publisher was accepted; got: $(tail -2 <<<"$output")"
[[ ! -x "$work/bin/cx-agent" ]] || fail "unverifiable artifact was installed"
pass "a bundle that does not match the pinned identity is refused"

# 4. The pinned identity must name this repository's release workflow, not any
#    keyless signer.
grep -q 'certificate-identity-regexp' "$INSTALL" || fail "install.sh does not pin a certificate identity"
grep -q 'certificate-oidc-issuer' "$INSTALL" || fail "install.sh does not pin an OIDC issuer"
grep -q 'agent-release\\.yml' "$INSTALL" || fail "pinned identity does not name the release workflow"
pass "install.sh pins publisher identity and issuer"

printf '\nagent install signature gate: PASS\n'
