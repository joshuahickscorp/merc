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

# Homebrew /usr/bin/python3 is a stub, and a Cellar interpreter is not on the
# profile's process-exec allowlist. Exec-failure used to be scored as "bind
# allowed" / "exfil allowed". Probe with a tiny binary compiled into DATADIR
# (the profile may exec that subpath) so the rows measure network policy.
echo
echo "  network containment:"
PROBE="$DATADIR/netprobe"
if ! command -v cc >/dev/null 2>&1; then
  printf '  \033[1;33m•\033[0m SKIP  network rows  -  cc not found (cannot build in-sandbox probe)\n'
else
  cat > "$FAKE/netprobe.c" <<'C'
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>
/* exit 0 = denied (EPERM/EACCES) or expected allow succeeded
   exit 1 = unexpected allow (breach) or unexpected deny
   exit 2 = harness/network error */
static int conn(const char *ip, int port) {
  int fd = socket(AF_INET, SOCK_STREAM, 0);
  if (fd < 0) return -errno;
  struct sockaddr_in a;
  memset(&a, 0, sizeof(a));
  a.sin_family = AF_INET;
  a.sin_port = htons((unsigned short)port);
  inet_pton(AF_INET, ip, &a.sin_addr);
  struct timeval tv = {3, 0};
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
  setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
  int rc = connect(fd, (struct sockaddr *)&a, sizeof(a));
  int e = errno;
  close(fd);
  return rc == 0 ? 0 : -e;
}
int main(int argc, char **argv) {
  if (argc < 2) return 2;
  if (strcmp(argv[1], "bind") == 0) {
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) return 2;
    struct sockaddr_in a;
    memset(&a, 0, sizeof(a));
    a.sin_family = AF_INET;
    a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (bind(fd, (struct sockaddr *)&a, sizeof(a)) == 0) { close(fd); return 1; }
    int e = errno;
    close(fd);
    return (e == EPERM || e == EACCES) ? 0 : 2;
  }
  if (strcmp(argv[1], "connect") == 0 && argc == 4) {
    int rc = conn(argv[2], atoi(argv[3]));
    if (rc == 0) return 0;
    if (rc == -EPERM || rc == -EACCES) return 1;
    return 2;
  }
  if (strcmp(argv[1], "connect-deny") == 0 && argc == 4) {
    int rc = conn(argv[2], atoi(argv[3]));
    if (rc == 0) return 1; /* connected = breach */
    if (rc == -EPERM || rc == -EACCES) return 0;
    return 2;
  }
  return 2;
}
C
  if ! cc -O1 -o "$PROBE" "$FAKE/netprobe.c" >/dev/null 2>&1; then
    printf '  \033[1;33m•\033[0m SKIP  network rows  -  failed to compile in-sandbox probe\n'
  else
    run_probe() {
      local err
      err="$(mktemp "$FAKE/sb.XXXXXX")"
      if run "$PROBE" "$@" 2>"$err"; then
        rm -f "$err"
        return 0
      fi
      local st=$?
      if grep -q 'execvp()\|Operation not permitted' "$err"; then
        rm -f "$err"
        return 3
      fi
      rm -f "$err"
      return "$st"
    }

    run_probe bind
    st=$?
    if [ "$st" -eq 0 ]; then
      ok "DENY   open a listening socket (no inbound backdoor)"
    elif [ "$st" -eq 3 ]; then
      printf '  \033[1;33m•\033[0m SKIP  bind row  -  probe not executable under the profile\n'
    else
      bad "DENY   open a listening socket  -  bind was ALLOWED (a payload could listen)"
    fi

    PY="$(command -v python3 || true)"
    LPORT=0
    if [ -n "$PY" ]; then
      LPORT="$("$PY" -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()' 2>/dev/null || echo 0)"
    fi
    if [ "$LPORT" != "0" ] && [ -n "$PY" ]; then
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
      run_probe connect 127.0.0.1 "$LPORT"
      st=$?
      if [ "$st" -eq 0 ]; then
        ok "ALLOW  outbound to loopback (dev control plane / local sidecar)"
      elif [ "$st" -eq 3 ]; then
        printf '  \033[1;33m•\033[0m SKIP  loopback row  -  probe not executable under the profile\n'
      else
        bad "ALLOW  outbound to loopback  -  legitimate local egress was BLOCKED"
      fi
      kill "$LSRV" >/dev/null 2>&1 || true
      wait "$LSRV" 2>/dev/null || true
    fi

    REMOTE_IP="1.1.1.1"
    if [ -n "$PY" ] && "$PY" -c "import socket,sys
s=socket.socket(); s.settimeout(4)
try:
    s.connect(('$REMOTE_IP',443)); sys.exit(0)
except OSError:
    sys.exit(1)" >/dev/null 2>&1; then
      run_probe connect-deny "$REMOTE_IP" 443
      st=$?
      if [ "$st" -eq 0 ]; then
        ok "DENY   outbound HTTPS to arbitrary host :443 (no open exfil egress)"
      elif [ "$st" -eq 3 ]; then
        printf '  \033[1;33m•\033[0m SKIP  remote :443 row  -  probe not executable under the profile\n'
      else
        bad "DENY   outbound arbitrary :443  -  buyer payload could exfiltrate to any host"
      fi
      run_probe connect-deny "$REMOTE_IP" 6667
      st=$?
      if [ "$st" -eq 0 ]; then
        ok "DENY   outbound to an arbitrary port :6667 (no C2/exfil egress)"
      elif [ "$st" -eq 3 ]; then
        printf '  \033[1;33m•\033[0m SKIP  remote :6667 row  -  probe not executable under the profile\n'
      else
        bad "DENY   outbound :6667  -  a payload could phone home on an arbitrary port"
      fi
    else
      printf '  \033[1;33m•\033[0m SKIP  remote-port rows  -  no network to %s:443 (offline runner)\n' "$REMOTE_IP"
    fi
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
