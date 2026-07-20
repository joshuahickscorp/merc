#!/usr/bin/env bash

set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
  echo "SKIP: seatbelt/sandbox-exec is macOS-only (uname=$(uname -s))"
  exit 0
fi
command -v sandbox-exec >/dev/null 2>&1 || { echo "FAIL: sandbox-exec not found"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE="$SCRIPT_DIR/cx-agent.sb"
[ -f "$PROFILE" ] || { echo "FAIL: profile not found at $PROFILE"; exit 1; }

FAKE="$(mktemp -d "$HOME/.cx-sandbox-test.XXXXXX")"
cleanup() { rm -rf "$FAKE"; }
trap cleanup EXIT INT TERM

H="$FAKE/home"
MODELCACHE="$H/.cache/huggingface"
DATADIR="$H/.compute-exchange"
mkdir -p "$H/.ssh" "$H/.gnupg" "$H/.aws" "$H/Library/Keychains" "$H/Library/LaunchAgents" \
         "$H/Documents" "$H/Desktop" "$H/Downloads" "$MODELCACHE" "$DATADIR"
echo "PRIVATE-SSH-KEY"        > "$H/.ssh/id_rsa"
echo "AWS_SECRET=deadbeef"    > "$H/.aws/credentials"
echo "login-keychain-secret"  > "$H/Library/Keychains/login.keychain-db"
echo "user tax return 2026"   > "$H/Documents/taxes.txt"
echo "MODEL WEIGHTS"          > "$MODELCACHE/model.gguf"
echo '{"state":"idle"}'       > "$DATADIR/status.json"

TMPDIR_REAL="${TMPDIR:-/private/var/folders}"

run() {
  sandbox-exec -f "$PROFILE" \
    -D HOME="$H" \
    -D MODELCACHE="$MODELCACHE" \
    -D DATADIR="$DATADIR" \
    -D TMPDIR="$TMPDIR_REAL" \
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

echo "sandbox-profile-test: proving cx-agent.sb against standalone binaries"
echo "  profile : $PROFILE"
echo "  fakeHOME: $H"
echo

expect_allow "process executes under the profile"           /bin/sh -c 'exit 0'

expect_allow "read the model cache (weights/tokenizer)"     /bin/cat "$MODELCACHE/model.gguf"
expect_allow "write the model cache (hf-hub download)"      /bin/sh -c "printf x > '$MODELCACHE/new-weight.bin'"
expect_allow "read the agent data dir"                      /bin/cat "$DATADIR/status.json"
expect_allow "write the agent data dir (status.json)"       /bin/sh -c "printf y > '$DATADIR/status.json.tmp'"
expect_allow "write system temp (inference scratch)"        /bin/sh -c "printf z > \"\${TMPDIR:-/private/tmp}/cx-scratch.\$\$\""

expect_deny  "plant a LaunchAgent (persistence)"            /bin/sh -c "printf evil > '$H/Library/LaunchAgents/com.evil.plist'"
expect_deny  "overwrite the operator's Documents"          /bin/sh -c "printf x > '$H/Documents/taxes.txt'"
expect_deny  "read ~/.ssh/id_rsa (SSH private key)"        /bin/cat "$H/.ssh/id_rsa"
expect_deny  "read ~/.aws/credentials (cloud secrets)"     /bin/cat "$H/.aws/credentials"
expect_deny  "read ~/Library/Keychains (login keychain)"   /bin/cat "$H/Library/Keychains/login.keychain-db"
expect_deny  "read ~/Documents (personal files)"           /bin/cat "$H/Documents/taxes.txt"
expect_deny  "write a new ~/.zshrc (rc injection)"         /bin/sh -c "printf pwn > '$H/.zshrc'"

PY="$(command -v python3 || true)"
if [ -z "$PY" ]; then
  printf '  \033[1;33m•\033[0m SKIP  network rows  -  python3 not found (socket-level probe unavailable)\n'
else
  echo
  echo "  network containment:"

  if run "$PY" -c 'import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_STREAM)
try:
    s.bind(("127.0.0.1",0)); s.listen(1); sys.exit(1)   # bind SUCCEEDED = containment breach
except OSError as e:
    sys.exit(0 if e.errno==1 else 2)                    # errno 1 = EPERM = sandbox denied it' >/dev/null 2>&1; then
    ok  "DENY   open a listening socket (no inbound backdoor)"
  else
    bad "DENY   open a listening socket  -  bind was ALLOWED (a payload could listen)"
  fi

  LPORT="$("$PY" -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()' 2>/dev/null || echo 0)"
  if [ "$LPORT" != "0" ]; then
    "$PY" -c "import socket
srv=socket.socket(); srv.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
srv.bind(('127.0.0.1',$LPORT)); srv.listen(1); srv.settimeout(8)
try:
    c,_=srv.accept(); c.close()
except OSError:
    pass
srv.close()" >/dev/null 2>&1 &
    LSRV=$!
    sleep 1
    if run "$PY" -c "import socket,sys
s=socket.socket(); s.settimeout(4)
try:
    s.connect(('127.0.0.1',$LPORT)); sys.exit(0)
except OSError:
    sys.exit(1)" >/dev/null 2>&1; then
      ok "ALLOW  outbound to loopback (dev control plane / local sidecar)"
    else
      bad "ALLOW  outbound to loopback  -  legitimate local egress was BLOCKED"
    fi
    kill "$LSRV" >/dev/null 2>&1 || true
    wait "$LSRV" 2>/dev/null || true
  fi

  REMOTE_IP="1.1.1.1"
  if "$PY" -c "import socket,sys
s=socket.socket(); s.settimeout(4)
try:
    s.connect(('$REMOTE_IP',443)); sys.exit(0)
except OSError:
    sys.exit(1)" >/dev/null 2>&1; then

    if run "$PY" -c "import socket,sys
s=socket.socket(); s.settimeout(6)
try:
    s.connect(('$REMOTE_IP',443)); sys.exit(0)
except OSError as e:
    sys.exit(1 if e.errno==1 else 0)   # only an EPERM is a failure here" >/dev/null 2>&1; then
      ok "ALLOW  outbound HTTPS :443 (control plane / storage / HuggingFace)"
    else
      bad "ALLOW  outbound :443  -  the sandbox blocked a port inference needs"
    fi

    if run "$PY" -c "import socket,sys,time
s=socket.socket(); s.settimeout(6); t=time.time()
try:
    s.connect(('$REMOTE_IP',6667)); sys.exit(1)          # connect SUCCEEDED = containment breach
except OSError as e:
    dt=time.time()-t
    sys.exit(0 if (e.errno==1 and dt<1.0) else 2)        # fast EPERM = in-kernel sandbox deny" >/dev/null 2>&1; then
      ok "DENY   outbound to an arbitrary port :6667 (no C2/exfil egress)"
    else
      bad "DENY   outbound :6667  -  a payload could phone home on an arbitrary port"
    fi
  else
    printf '  \033[1;33m•\033[0m SKIP  remote-port rows  -  no network to %s:443 (offline runner)\n' "$REMOTE_IP"
  fi
fi

echo
if [ "$FAIL" -gt 0 ]; then
  printf '\033[1;31mFAIL: %d containment row(s) regressed (%d ok)\033[0m\n' "$FAIL" "$PASS" >&2
  exit 1
fi
printf '\033[1;32mPASS: all %d containment rows held  -  cx-agent.sb contains the blast radius\033[0m\n' "$PASS"
