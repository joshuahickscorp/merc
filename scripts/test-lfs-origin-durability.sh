#!/usr/bin/env bash
# Hermetic safety test for the remote-durability gate.
#
# It does not pretend to prove origin availability.  It proves the inverse:
# pointing the gate at local bytes is rejected before a clone or LFS fetch can
# make a local cache look like off-machine durability.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

LOCAL_REMOTE="file://$(git rev-parse --git-common-dir)"
set +e
OUT="$(bash scripts/verify-lfs-origin-durability.sh --remote-url "$LOCAL_REMOTE" 2>&1)"
RC=$?
set -e

printf '%s\n' "$OUT"
if [[ "$RC" -eq 0 ]]; then
  echo "lfs-origin-durability self-test: FAIL -- accepted a file:// remote" >&2
  exit 1
fi
if ! printf '%s' "$OUT" | grep -q 'refusing non-network or loopback remote'; then
  echo "lfs-origin-durability self-test: FAIL -- local remote refusal was not explicit" >&2
  exit 1
fi
echo "lfs-origin-durability self-test: PASS -- file:// remote cannot satisfy off-machine proof"

# A public HTTPS/SSH remote is still not durability authority merely because it
# is network-reachable.  The candidate must name the reviewed repository, not
# an attacker-controlled or unrelated public project with a healthy LFS store.
for remote in \
  "https://github.com/example/public-merc.git" \
  "git@github.com:example/public-merc.git"; do
  set +e
  OUT="$(bash scripts/verify-lfs-origin-durability.sh --remote-url "$remote" 2>&1)"
  RC=$?
  set -e
  printf '%s\n' "$OUT"
  if [[ "$RC" -eq 0 ]]; then
    echo "lfs-origin-durability self-test: FAIL -- accepted noncanonical public remote" >&2
    exit 1
  fi
  if ! printf '%s' "$OUT" | grep -q 'does not match reviewed repository authority'; then
    echo "lfs-origin-durability self-test: FAIL -- public remote refusal was not explicit" >&2
    exit 1
  fi
done

CANONICAL="$(python3 scripts/validate-lfs-origin-authority.py --url 'git@github.com:joshuahickscorp/merc.git')"
if [[ "$CANONICAL" != "github.com/joshuahickscorp/merc" ]]; then
  echo "lfs-origin-durability self-test: FAIL -- canonical SSH identity=$CANONICAL" >&2
  exit 1
fi
echo "lfs-origin-durability self-test: PASS -- only reviewed HTTPS/SSH repository identity is authorized"

EXPECTED_LFS="$(python3 scripts/validate-lfs-origin-authority.py --expected-lfs-endpoint)"
if ! python3 scripts/validate-lfs-origin-authority.py --lfs-endpoint "$EXPECTED_LFS" >/dev/null; then
  echo "lfs-origin-durability self-test: FAIL -- reviewed LFS endpoint was rejected" >&2
  exit 1
fi
if python3 scripts/validate-lfs-origin-authority.py \
  --lfs-endpoint 'https://example.invalid/joshuahickscorp/merc.git/info/lfs' >/dev/null 2>&1; then
  echo "lfs-origin-durability self-test: FAIL -- unreviewed LFS endpoint was accepted" >&2
  exit 1
fi

# A candidate .lfsconfig is deliberately visible to the fresh proof. It must
# resolve outside reviewed authority and be rejected before transfer; caller
# global configuration is disabled entirely.
TMPDIR_LFS="$(mktemp -d "${TMPDIR:-/tmp}/merc-lfs-authority.XXXXXX")"
cleanup_lfs_authority_test() { rm -rf -- "$TMPDIR_LFS"; }
trap cleanup_lfs_authority_test EXIT INT TERM
git init -q "$TMPDIR_LFS/repo"
git -C "$TMPDIR_LFS/repo" config --file .lfsconfig lfs.url 'https://example.invalid/redirect.git/info/lfs'
git config --file "$TMPDIR_LFS/global" lfs.url 'https://example.invalid/global.git/info/lfs'
RESOLVED="$(env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
  git -C "$TMPDIR_LFS/repo" lfs env \
  | sed -n 's/^Endpoint=\([^ ]*\).*/\1/p' | head -n 1)"
if [[ "$RESOLVED" = "$EXPECTED_LFS" ]]; then
  echo "lfs-origin-durability self-test: FAIL -- candidate LFS redirect was not observable" >&2
  exit 1
fi
if python3 scripts/validate-lfs-origin-authority.py --lfs-endpoint "$RESOLVED" >/dev/null 2>&1; then
  echo "lfs-origin-durability self-test: FAIL -- candidate LFS redirect was accepted" >&2
  exit 1
fi
echo "lfs-origin-durability self-test: PASS -- candidate/global LFS redirects are detected and rejected"
