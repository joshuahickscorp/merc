#!/usr/bin/env bash

set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
  echo "SKIP: seatbelt/sandbox-exec is macOS-only (uname=$(uname -s))"
  exit 0
fi
command -v sandbox-exec >/dev/null 2>&1 || { echo "FAIL: sandbox-exec not found"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE="$SCRIPT_DIR/merc-agent.sb"
[ -f "$PROFILE" ] || { echo "FAIL: profile not found at $PROFILE"; exit 1; }

FAKE="$(mktemp -d "$HOME/.merc-sandbox-test.XXXXXX")"
cleanup() { rm -rf "$FAKE"; }
trap cleanup EXIT INT TERM

H="$FAKE/home"
MODELCACHE="$H/.cache/huggingface"
DATADIR="$H/.merc"
mkdir -p "$H/.ssh" "$H/.gnupg" "$H/.aws" "$H/Library/Keychains" "$H/Library/LaunchAgents" \
         "$H/Documents" "$H/Desktop" "$H/Downloads" "$MODELCACHE" "$DATADIR"
echo "PRIVATE-SSH-KEY"        > "$H/.ssh/id_rsa"
echo "AWS_SECRET=deadbeef"    > "$H/.aws/credentials"
echo "login-keychain-secret"  > "$H/Library/Keychains/login.keychain-db"
echo "user tax return 2026"   > "$H/Documents/taxes.txt"
echo "MODEL WEIGHTS"          > "$MODELCACHE/model.gguf"
echo '{"state":"idle"}'       > "$DATADIR/status.json"

TMPDIR_REAL="${TMPDIR:-/private/var/folders}"
# Install prefix of the agent binary (process-exec/file-read of merc-agent itself).
BINDIR="${MERC_SANDBOX_BINDIR:-/usr/local/bin}"

run() {
  sandbox-exec -f "$PROFILE" \
    -D HOME="$H" \
    -D MODELCACHE="$MODELCACHE" \
    -D DATADIR="$DATADIR" \
    -D TMPDIR="$TMPDIR_REAL" \
    -D BINDIR="$BINDIR" \
    "$@"
}

PASS=0; FAIL=0
ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '  \033[1;31m✗ %s\033[0m\n' "$*" >&2; FAIL=$((FAIL+1)); }

expect_allow() {
  local label="$1"; shift
  if run "$@" >/dev/null 2>&1; then ok "ALLOW  $label"; else bad "ALLOW  $label  -  legitimate access was BLOCKED"; fi
}
expect_deny() {
  local label="$1"; shift
  if run "$@" >/dev/null 2>&1; then bad "DENY   $label  -  hostile access SUCCEEDED (containment breach)"; else ok "DENY   $label"; fi
}

echo "sandbox-profile-test: proving merc-agent.sb against standalone binaries"
echo "  profile : $PROFILE"
echo "  fakeHOME: $H"
echo

expect_allow "process executes under the profile"           /bin/sh -c 'exit 0'

expect_allow "read the model cache (weights/tokenizer)"     /bin/cat "$MODELCACHE/model.gguf"
expect_allow "write the model cache (hf-hub download)"      /bin/sh -c "printf x > '$MODELCACHE/new-weight.bin'"
expect_allow "read the agent data dir"                      /bin/cat "$DATADIR/status.json"
expect_allow "write the agent data dir (status.json)"       /bin/sh -c "printf y > '$DATADIR/status.json.tmp'"
expect_allow "write system temp (inference scratch)"        /bin/sh -c "printf z > \"\${TMPDIR:-/private/tmp}/merc-scratch.\$\$\""

expect_deny  "plant a LaunchAgent (persistence)"            /bin/sh -c "printf evil > '$H/Library/LaunchAgents/com.evil.plist'"
expect_deny  "overwrite the operator's Documents"          /bin/sh -c "printf x > '$H/Documents/taxes.txt'"
expect_deny  "read ~/.ssh/id_rsa (SSH private key)"        /bin/cat "$H/.ssh/id_rsa"
expect_deny  "read ~/.aws/credentials (cloud secrets)"     /bin/cat "$H/.aws/credentials"
expect_deny  "read ~/Library/Keychains (login keychain)"   /bin/cat "$H/Library/Keychains/login.keychain-db"
expect_deny  "read ~/Documents (personal files)"           /bin/cat "$H/Documents/taxes.txt"
expect_deny  "write a new ~/.zshrc (rc injection)"         /bin/sh -c "printf pwn > '$H/.zshrc'"

PERL="/usr/bin/perl"
if [ ! -x "$PERL" ]; then
  printf '  \033[1;33m•\033[0m SKIP  network rows  -  system Perl unavailable (socket-level probe unavailable)\n'
else
  echo
  echo "  network containment:"

  # Use the system Perl runtime, which the production profile permits under
  # /usr/bin. Homebrew Python is deliberately outside the executable allowlist;
  # using it here made a failed exec look like a network-containment failure.
  if run "$PERL" -MSocket=AF_INET,SOCK_STREAM,INADDR_LOOPBACK,sockaddr_in \
      -MErrno=EPERM -e '
socket(my $s, AF_INET, SOCK_STREAM, 0) or exit 2;
bind($s, sockaddr_in(0, INADDR_LOOPBACK)) and exit 1; # bind succeeded = breach
exit($! == EPERM ? 0 : 2)' >/dev/null 2>&1; then
    ok  "DENY   open a listening socket (no inbound backdoor)"
  else
    bad "DENY   open a listening socket  -  bind was allowed or probe was not denied with EPERM"
  fi

  PORT_FILE="$FAKE/loopback-port"
  "$PERL" -MSocket=AF_INET,SOCK_STREAM,INADDR_LOOPBACK,sockaddr_in -e '
socket(my $s, AF_INET, SOCK_STREAM, 0) or exit 1;
setsockopt($s, SOL_SOCKET, SO_REUSEADDR, 1);
bind($s, sockaddr_in(0, INADDR_LOOPBACK)) or exit 1;
listen($s, 1) or exit 1;
my ($port) = sockaddr_in(getsockname($s));
open my $out, ">", $ARGV[0] or exit 1;
print {$out} $port;
close $out;
alarm 8;
accept(my $client, $s);
' "$PORT_FILE" >/dev/null 2>&1 &
    LSRV=$!
  for _ in $(seq 1 20); do
    [ -s "$PORT_FILE" ] && break
    sleep 0.1
  done
  LPORT=""
  if [ -s "$PORT_FILE" ]; then
    LPORT="$(<"$PORT_FILE")"
  fi
  if [ -n "$LPORT" ]; then
    if run "$PERL" -MSocket=AF_INET,SOCK_STREAM,inet_aton,sockaddr_in -e '
socket(my $s, AF_INET, SOCK_STREAM, 0) or exit 1;
connect($s, sockaddr_in($ARGV[1], inet_aton($ARGV[0]))) or exit 1;
exit 0' 127.0.0.1 "$LPORT" >/dev/null 2>&1; then
      ok "ALLOW  outbound to loopback (dev control plane / local sidecar)"
    else
      bad "ALLOW  outbound to loopback  -  legitimate local egress was BLOCKED"
    fi
  fi
  kill "$LSRV" >/dev/null 2>&1 || true
  wait "$LSRV" 2>/dev/null || true

  REMOTE_IP="1.1.1.1"
  if "$PERL" -MSocket=AF_INET,SOCK_STREAM,inet_aton,sockaddr_in -e '
socket(my $s, AF_INET, SOCK_STREAM, 0) or exit 1;
connect($s, sockaddr_in($ARGV[1], inet_aton($ARGV[0]))) or exit 1;
exit 0' "$REMOTE_IP" 443 >/dev/null 2>&1; then

    # Deny-default profile: arbitrary remote :443 must be DENIED. Only hosts
    # declared via CONTROL_HOST / ARTIFACT_HOST / MODEL_HOST are reachable.
    if run "$PERL" -MSocket=AF_INET,SOCK_STREAM,inet_aton,sockaddr_in \
        -MErrno=EPERM -e '
socket(my $s, AF_INET, SOCK_STREAM, 0) or exit 2;
connect($s, sockaddr_in($ARGV[1], inet_aton($ARGV[0]))) and exit 1;
exit($! == EPERM ? 0 : 2)' "$REMOTE_IP" 443 >/dev/null 2>&1; then
      ok "DENY   outbound HTTPS to arbitrary host :443 (no open exfil egress)"
    else
      bad "DENY   outbound arbitrary :443  -  buyer payload could exfiltrate to any host"
    fi

    if run "$PERL" -MSocket=AF_INET,SOCK_STREAM,inet_aton,sockaddr_in \
        -MErrno=EPERM -e '
socket(my $s, AF_INET, SOCK_STREAM, 0) or exit 2;
connect($s, sockaddr_in($ARGV[1], inet_aton($ARGV[0]))) and exit 1;
exit($! == EPERM ? 0 : 2)' "$REMOTE_IP" 6667 >/dev/null 2>&1; then
      ok "DENY   outbound to an arbitrary port :6667 (no C2/exfil egress)"
    else
      bad "DENY   outbound :6667  -  a payload could phone home on an arbitrary port"
    fi
  else
    printf '  \033[1;33m•\033[0m SKIP  remote-port rows  -  no network to %s:443 (offline runner)\n' "$REMOTE_IP"
  fi
fi

# Static profile-shape assertions (same gates as TestSeatbeltProfileIsDenyDefault…).
# Strip ;; comments so explanatory prose about the old (allow default) / *:443
# rules cannot trip the gates.
PROFILE_LIVE="$(sed 's/;;.*//' "$PROFILE")"
if printf '%s\n' "$PROFILE_LIVE" | grep -q '(deny default)' \
   && ! printf '%s\n' "$PROFILE_LIVE" | grep -q '(allow default)'; then
  ok "profile is deny-default (no allow default)"
else
  bad "profile is not deny-default"
fi
if printf '%s\n' "$PROFILE_LIVE" | grep -EFq '(remote ip "*:443")' \
   || printf '%s\n' "$PROFILE_LIVE" | grep -EFq '(remote ip "*:80")'; then
  bad "profile still has wildcard host egress on 80/443"
else
  ok "profile has no wildcard *:443 / *:80 egress"
fi

echo
if [ "$FAIL" -gt 0 ]; then
  printf '\033[1;31mFAIL: %d containment row(s) regressed (%d ok)\033[0m\n' "$FAIL" "$PASS" >&2
  exit 1
fi
printf '\033[1;32mPASS: all %d containment rows held  -  merc-agent.sb contains the blast radius\033[0m\n' "$PASS"
