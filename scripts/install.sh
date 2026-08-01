#!/usr/bin/env bash
# Install the merc supplier agent from a signed prebuilt release.
#
# Supported fetch targets: darwin/arm64, linux/amd64.
# Windows uses scripts/install.ps1 so Task Scheduler and ACLs are native.
# Override:
#   MERC_AGENT_VERSION   release tag without the "agent-" prefix (default: latest)
#   MERC_AGENT_BASE_URL  base URL that hosts archives + SHA256SUMS
#                      (default: GitHub releases for this repo)
#   MERC_AGENT_FROM_SOURCE=1  force local cargo build (escape hatch)
#   MERC_PREFIX          install directory for the binary (default: ~/.local/bin)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFIX="${MERC_PREFIX:-$HOME/.local/bin}"
BIN="$PREFIX/merc-agent"
LEGACY_BIN="$PREFIX/cx-agent"
HOMEDIR="$HOME/.merc"
LEGACY_HOMEDIR="$HOME/.compute-exchange"
CONFIG="$HOMEDIR/agent.toml"
PREFS="$HOMEDIR/agent.prefs.toml"
PLIST="$HOME/Library/LaunchAgents/dev.merc.agent.plist"
LEGACY_PLIST="$HOME/Library/LaunchAgents/dev.computeexchange.agent.plist"
LABEL="dev.merc.agent"
SYSTEMD_USER_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SYSTEMD_UNIT="$SYSTEMD_USER_DIR/merc-agent.service"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) ARCH="$ARCH_RAW" ;;
esac

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

case "${OS}/${ARCH}" in
  darwin/arm64|linux/amd64) : ;;
  darwin/*)
    die "prebuilt agent is published for darwin/arm64 only (this host is ${OS}/${ARCH}). Set MERC_AGENT_FROM_SOURCE=1 to build locally if you have cargo."
    ;;
  linux/*)
    die "prebuilt agent is published for linux/amd64 only (this host is ${OS}/${ARCH}). Set MERC_AGENT_FROM_SOURCE=1 to build locally if you have cargo."
    ;;
  *)
    die "unsupported platform ${OS}/${ARCH} (prebuilds: darwin/arm64, linux/amd64)"
    ;;
esac

REPO="${MERC_AGENT_REPO:-joshuahickscorp/merc}"
DEFAULT_BASE="https://github.com/${REPO}/releases/latest/download"
BASE_URL="${MERC_AGENT_BASE_URL:-$DEFAULT_BASE}"
VERSION="${MERC_AGENT_VERSION:-}"

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# The workflow allowed to publish supplier agents. cosign keyless verification
# without one of these is not "verify the publisher signed it", it is "verify
# somebody signed it" -- and cosign v2 refuses the call outright rather than
# doing the weaker thing, so an unpinned invocation aborts every install that
# has cosign on PATH.
IDENTITY_REGEXP="${MERC_AGENT_IDENTITY_REGEXP:-^https://github\.com/${REPO}/\.github/workflows/agent-release\.yml@refs/(tags/agent-v.*|heads/main)$}"
OIDC_ISSUER="${MERC_AGENT_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"

verify_signature() {
  local file="$1" url="$2" work="$3" label="$4" bundle="$3/${4}.cosign.bundle"

  if ! command -v cosign >/dev/null 2>&1; then
    # A digest list fetched from the same host as the archive authenticates
    # nothing on its own, so this is a real reduction in assurance and the
    # operator has to opt into it rather than get it by default.
    if [[ "${MERC_AGENT_ALLOW_UNVERIFIED:-0}" == "1" ]]; then
      warn "cosign not installed; $label verified by digest only (MERC_AGENT_ALLOW_UNVERIFIED=1)"
      return 0
    fi
    die "cosign is required to verify $label. Install cosign (https://docs.sigstore.dev/cosign/installation/), or set MERC_AGENT_ALLOW_UNVERIFIED=1 to accept digest-only verification."
  fi

  if ! curl -fsSL "${url}.cosign.bundle" -o "$bundle" 2>/dev/null; then
    if [[ "${MERC_AGENT_ALLOW_UNVERIFIED:-0}" == "1" ]]; then
      warn "no signature published for $label (MERC_AGENT_ALLOW_UNVERIFIED=1)"
      return 0
    fi
    die "no cosign bundle published at ${url}.cosign.bundle for $label. Set MERC_AGENT_ALLOW_UNVERIFIED=1 to install without a signature."
  fi

  cosign verify-blob \
    --bundle "$bundle" \
    --certificate-identity-regexp "$IDENTITY_REGEXP" \
    --certificate-oidc-issuer "$OIDC_ISSUER" \
    "$file" >/dev/null 2>&1 \
    || die "cosign could not verify $label against $IDENTITY_REGEXP (issuer $OIDC_ISSUER)"
  say "cosign verified $label"
}

fetch_prebuilt() {
  local work name sums_url archive_url expected actual
  work="$(mktemp -d "${TMPDIR:-/tmp}/merc-agent-install.XXXXXX")"
  # shellcheck disable=SC2064
  trap "rm -rf '$work'" RETURN

  if [[ -n "$VERSION" ]]; then
    name="merc-agent_${VERSION}_${OS}_${ARCH}.tar.gz"
    if [[ "$BASE_URL" == *"/latest/download" ]]; then
      archive_url="https://github.com/${REPO}/releases/download/agent-${VERSION}/${name}"
      sums_url="https://github.com/${REPO}/releases/download/agent-${VERSION}/SHA256SUMS"
    else
      archive_url="${BASE_URL%/}/${name}"
      sums_url="${BASE_URL%/}/SHA256SUMS"
    fi
  else
    # latest/download serves the newest release assets by name; version in the
    # filename still matters, so resolve via the GitHub API when possible.
    if command -v curl >/dev/null 2>&1 && [[ "$BASE_URL" == *"github.com"* ]]; then
      local api tag
      api="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")" || die "could not query latest agent release"
      tag="$(printf '%s' "$api" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tag_name",""))')"
      [[ "$tag" == agent-* ]] || die "latest release tag is not an agent release (got ${tag:-empty})"
      VERSION="${tag#agent-}"
      name="merc-agent_${VERSION}_${OS}_${ARCH}.tar.gz"
      archive_url="https://github.com/${REPO}/releases/download/${tag}/${name}"
      sums_url="https://github.com/${REPO}/releases/download/${tag}/SHA256SUMS"
    else
      die "set MERC_AGENT_VERSION or MERC_AGENT_BASE_URL to a directory that contains the archive and SHA256SUMS"
    fi
  fi

  say "fetching $archive_url"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$sums_url" -o "$work/SHA256SUMS" || die "download SHA256SUMS failed"
    curl -fsSL "$archive_url" -o "$work/$name" || die "download $name failed"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$work/SHA256SUMS" "$sums_url" || die "download SHA256SUMS failed"
    wget -q -O "$work/$name" "$archive_url" || die "download $name failed"
  else
    die "curl or wget required to fetch prebuilt agent"
  fi

  expected="$(awk -v f="$name" '$2 == f || $2 == ("./" f) {print $1; exit}' "$work/SHA256SUMS")"
  [[ -n "$expected" ]] || die "SHA256SUMS has no entry for $name"
  actual="$(checksum "$work/$name")"
  [[ "$actual" == "$expected" ]] || die "checksum mismatch for $name (got $actual want $expected)"

  # SHA256SUMS comes from the same host as the archive, so a matching digest
  # proves only that whoever served one served the other. Publisher identity
  # comes from the signature, and only if the certificate is pinned to the
  # workflow that is allowed to publish: cosign in keyless mode will otherwise
  # accept any Fulcio certificate. Verify the digest list itself, since that is
  # what every archive digest is checked against.
  verify_signature "$work/SHA256SUMS" "$sums_url" "$work" "SHA256SUMS"
  verify_signature "$work/$name" "$archive_url" "$work" "$name"

  tar -C "$work" -xzf "$work/$name"
  local extracted
  extracted="$(find "$work" -type f \( -name merc-agent -o -name cx-agent \) | head -n 1)"
  [[ -x "$extracted" ]] || die "archive did not contain an executable merc-agent"
  mkdir -p "$PREFIX"
  install -m 0755 "$extracted" "$BIN"
  # Drop the pre-rebrand binary name so PATH does not keep serving the old one.
  if [[ -e "$LEGACY_BIN" && "$LEGACY_BIN" != "$BIN" ]]; then
    rm -f "$LEGACY_BIN"
    say "removed legacy binary $LEGACY_BIN"
  fi
  say "installed $BIN ($("$BIN" version 2>/dev/null || echo merc-agent))"
}

build_from_source() {
  command -v cargo >/dev/null 2>&1 || die "Rust toolchain (cargo) required for MERC_AGENT_FROM_SOURCE=1  -  install from https://rustup.rs"
  say "building the release agent from source (MERC_AGENT_FROM_SOURCE=1)…"
  local features=()
  if [[ "$OS" == "linux" ]]; then
    features=(--no-default-features)
  fi
  ( cd "$ROOT/agent" && cargo build --release "${features[@]}" ) || die "agent build failed"
  local src="$ROOT/agent/target/release/merc-agent"
  [[ -x "$src" ]] || die "built binary not found at $src"
  mkdir -p "$PREFIX"
  install -m 0755 "$src" "$BIN"
  if [[ -e "$LEGACY_BIN" && "$LEGACY_BIN" != "$BIN" ]]; then
    rm -f "$LEGACY_BIN"
    say "removed legacy binary $LEGACY_BIN"
  fi
  say "installed $BIN ($("$BIN" version 2>/dev/null || echo merc-agent))"
}

migrate_legacy_state() {
  # Preserve bench cache, identity, config, and logs from a pre-rebrand install.
  if [[ -d "$HOMEDIR" ]]; then
    return 0
  fi
  if [[ -d "$LEGACY_HOMEDIR" ]]; then
    say "migrating agent state $LEGACY_HOMEDIR -> $HOMEDIR"
    mv "$LEGACY_HOMEDIR" "$HOMEDIR" || die "could not migrate $LEGACY_HOMEDIR to $HOMEDIR"
  fi
}

write_config() {
  migrate_legacy_state
  mkdir -p "$HOMEDIR"
  if [[ -f "$CONFIG" ]]; then
    say "keeping existing config $CONFIG"
    return
  fi
  # Config may still live under the legacy path if migrate could not run earlier.
  if [[ -f "$LEGACY_HOMEDIR/agent.toml" ]]; then
    migrate_legacy_state
    if [[ -f "$CONFIG" ]]; then
      say "keeping migrated config $CONFIG"
      return
    fi
  fi
  umask 077
  cat >"$CONFIG" <<TOML
control_url = "${MERC_CONTROL_URL:-http://localhost:8080}"
worker_token = "${MERC_WORKER_TOKEN:-PASTE_WORKER_TOKEN_FROM_make_seed}"
supplier_id = "00000000-0000-0000-0000-000000000000"
max_cpu_pct = 80.0
power_only = true            # don't run on battery (ignored on non-macOS)
min_payout_usd_per_hr = 0.05 # reservation price floor
data_dir = "${HOMEDIR}/data"
TOML
  say "wrote starter config $CONFIG"
  [[ -n "${MERC_WORKER_TOKEN:-}" ]] || warn "set worker_token in $CONFIG (from 'make seed') before earning"
}

write_prefs() {
  [[ -f "$PREFS" ]] && { say "keeping existing live preferences $PREFS"; return; }
  umask 077
  cat >"$PREFS" <<'TOML'
paused = false
allowed_weekdays = [0, 1, 2, 3, 4, 5, 6]
allowed_workload_classes = ["embed", "batch_infer"]
allow_model_downloads = true
max_model_cache_gb = 4.0
max_bandwidth_mbps = 25.0
power_only = true
quiet_hours = [22, 6]
min_payout_usd_per_hr = 0.05
memory_headroom_gb = 8.0
max_memory_pct = 85.0
TOML
  say "wrote live preferences $PREFS (reloaded before every claim)"
}

install_darwin_launchagent() {
  # Stop the pre-rebrand label so we do not leave two KeepAlive agents racing.
  if [[ -f "$LEGACY_PLIST" ]]; then
    launchctl unload "$LEGACY_PLIST" 2>/dev/null || true
    rm -f "$LEGACY_PLIST"
    say "removed legacy LaunchAgent $LEGACY_PLIST"
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
  if [[ "$MODE" == "start" ]]; then
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load "$PLIST" && say "agent started (launchctl). Logs: $HOMEDIR/agent.log"
  else
    say "to start now:  launchctl load $PLIST   (or re-run with --start)"
  fi
  say "done."
}

install_linux_service() {
  mkdir -p "$SYSTEMD_USER_DIR"
  cat >"$SYSTEMD_UNIT" <<UNIT_EOF
[Unit]
Description=Merc supplier agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN} run --config ${CONFIG}
Restart=on-failure
RestartSec=10
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=${HOMEDIR}
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=default.target
UNIT_EOF
  chmod 0644 "$SYSTEMD_UNIT"
  say "installed systemd user service $SYSTEMD_UNIT"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload || warn "systemctl --user daemon-reload failed; log in with a user systemd session before starting"
    if [[ "$MODE" == "start" ]]; then
      systemctl --user enable --now merc-agent.service \
        && say "agent enabled and started (systemd --user)" \
        || die "could not enable/start merc-agent.service"
    else
      say "to start now: systemctl --user enable --now merc-agent.service (or re-run with --start)"
    fi
  else
    warn "systemctl is unavailable; run $BIN run --config $CONFIG under your service manager"
  fi
  say "Note: control validHWClasses currently accepts only Apple Silicon classes,"
  say "so this host can install the agent but cannot register for batch work yet."
  say "done."
}

if [[ "$MODE" == "check" ]]; then
  say "dry run  -  platform ${OS}/${ARCH} is a supported prebuild target."
  if [[ "${MERC_AGENT_FROM_SOURCE:-}" == "1" ]]; then
    say "  would build from source with cargo (MERC_AGENT_FROM_SOURCE=1)"
  else
    say "  would fetch prebuilt archive from: $BASE_URL"
    say "  would verify SHA256SUMS (and cosign bundle when cosign is present)"
  fi
  say "  would install binary to: $BIN"
  say "  would write starter config: $CONFIG (if missing)"
  say "  would write live preferences: $PREFS (if missing)"
  if [[ -d "$LEGACY_HOMEDIR" && ! -d "$HOMEDIR" ]]; then
    say "  would migrate legacy state: $LEGACY_HOMEDIR -> $HOMEDIR"
  fi
  if [[ "$OS" == "darwin" ]]; then
    say "  would install LaunchAgent: $PLIST"
    if [[ -f "$LEGACY_PLIST" ]]; then
      say "  would remove legacy LaunchAgent: $LEGACY_PLIST"
    fi
  else
    say "  would install systemd user service: $SYSTEMD_UNIT"
  fi
  say "Run without --check to perform the install; --uninstall to remove it."
  exit 0
fi

if [[ "${MERC_AGENT_FROM_SOURCE:-}" == "1" ]]; then
  build_from_source
else
  fetch_prebuilt || die "prebuilt install failed (set MERC_AGENT_FROM_SOURCE=1 to compile instead)"
fi

write_config
write_prefs

case "$OS" in
  darwin) install_darwin_launchagent ;;
  linux)  install_linux_service ;;
esac
