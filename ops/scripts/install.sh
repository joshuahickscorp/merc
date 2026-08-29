#!/usr/bin/env bash
# Install the merc supplier agent from a signed prebuilt release.
#
# Supported fetch targets: darwin/arm64, linux/amd64.
# Windows uses ops/scripts/install.ps1 so Task Scheduler and ACLs are native.
# Override:
#   MERC_AGENT_VERSION   release tag without the "agent-" prefix (default: latest)
#   MERC_AGENT_BASE_URL  base URL that hosts archives + SHA256SUMS
#                      (default: GitHub releases for this repo)
#   MERC_AGENT_FROM_SOURCE=1  force local cargo build (escape hatch)
#   MERC_PREFIX          install directory for the binary (default: ~/.local/bin)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PREFIX="${MERC_PREFIX:-$HOME/.local/bin}"
BIN="$PREFIX/merc-agent"
LEGACY_BIN="$PREFIX/cx-agent"
HOMEDIR="$HOME/.merc"
LEGACY_HOMEDIR="$HOME/.compute-exchange"
CONFIG="$HOMEDIR/agent.toml"
VLLM_CONFIG="$HOMEDIR/vllm.toml"
VLLM_PROFILE="$HOMEDIR/vllm-runtime-profile.json"
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
  --uninstall) exec "$ROOT/ops/scripts/uninstall.sh" "${@:2}" ;;
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
  if [[ "$OS" == "darwin" ]]; then
    install_darwin_seatbelt_profile "$work"
  fi
  if [[ "$OS" == "linux" ]]; then
    install_linux_vllm_profile "$work"
  fi
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
  ( cd "$ROOT/src/agent" && cargo build --release "${features[@]}" ) || die "agent build failed"
  local src="$ROOT/src/agent/target/release/merc-agent"
  [[ -x "$src" ]] || die "built binary not found at $src"
  mkdir -p "$PREFIX"
  install -m 0755 "$src" "$BIN"
  if [[ "$OS" == "darwin" ]]; then
    install_darwin_seatbelt_profile "$ROOT"
  fi
  if [[ "$OS" == "linux" ]]; then
    install_linux_vllm_profile "$ROOT"
  fi
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

install_darwin_seatbelt_profile() {
  local source_root="$1" profile dest
  [[ "$OS" == "darwin" ]] || return 0
  # merc-agent resolves the seatbelt profile as a sibling of the executable
  # (then MERC_SANDBOX_PROFILE). Install it next to $BIN so a stock macOS
  # install is contained without any LaunchAgent env. Source builds read the
  # checkout profile; prebuilt archives stage merc-agent.sb next to the binary.
  profile="$(find "$source_root" -type f \( -name merc-agent.sb -o -path "*/clients/macapp/ComputeExchangeAgent/merc-agent.sb" \) | head -n 1)"
  [[ -n "$profile" && -f "$profile" ]] || die "seatbelt profile merc-agent.sb not found under $source_root (containment cannot be installed)"
  dest="$PREFIX/merc-agent.sb"
  install -m 0644 "$profile" "$dest"
  say "installed seatbelt profile $dest (sibling of $BIN)"
}

install_linux_vllm_profile() {
  local source_root="$1" profile
  [[ "$OS" == "linux" ]] || return 0
  # Prebuilt archives include this exact file next to the binary. The source
  # escape hatch reads the same authority from the checkout. Do not download a
  # profile independently of the signed archive that supplied the binary.
  profile="$(find "$source_root" -type f \( -name vllm-runtime-profile.json -o -path "$source_root/src/control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json" \) | head -n 1)"
  [[ -f "$profile" ]] || die "Linux vLLM archive omitted its pinned runtime profile"
  migrate_legacy_state
  mkdir -p "$HOMEDIR"
  if [[ -f "$VLLM_PROFILE" ]]; then
    say "keeping existing pinned vLLM profile $VLLM_PROFILE"
  else
    install -m 0644 "$profile" "$VLLM_PROFILE"
    say "installed pinned vLLM profile $VLLM_PROFILE"
  fi
}

detect_linux_vllm_topology() {
  command -v nvidia-smi >/dev/null 2>&1 || return 1
  local mem_lines memory_mib gpu_count lowest_mib gpu_class interconnect
  mem_lines="$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null)" || return 1
  gpu_count="$(printf '%s\n' "$mem_lines" | awk 'NF {n++} END {print n+0}')"
  [[ "$gpu_count" -ge 1 ]] || return 1
  lowest_mib="$(printf '%s\n' "$mem_lines" | awk 'NF {if (!seen || $1 < min) min=$1; seen=1} END {if (seen) print min}')"
  [[ "$lowest_mib" =~ ^[0-9]+$ ]] || return 1
  # `nvidia-smi` reports vendor MiB totals, which vary slightly among cards
  # sold as the same nominal capacity. Classify only downward from a conservative
  # threshold; a 48GB card may safely use the 24GB capability tier, but a
  # nearly-24GB card must never be rounded upward into an unavailable class.
  if (( lowest_mib >= 172 * 1024 )); then
    gpu_class="nvidia_180gb"
  elif (( lowest_mib >= 76 * 1024 )); then
    gpu_class="nvidia_80gb"
  elif (( lowest_mib >= 46 * 1024 )); then
    gpu_class="nvidia_48gb"
  elif (( lowest_mib >= 23 * 1024 )); then
    gpu_class="nvidia_24gb"
  else
    return 1
  fi
  interconnect=""
  if [[ "$gpu_count" -gt 1 ]]; then
    if nvidia-smi topo -m 2>/dev/null | grep -Eq 'NV[0-9]+'; then
      interconnect="nvlink"
    else
      interconnect="pcie"
    fi
  fi
  # Print only topology metadata, never a credential or an endpoint.
  printf '%s %s %.3f %s\n' "$gpu_class" "$gpu_count" "$(awk -v mib="$lowest_mib" 'BEGIN {printf "%.3f", mib/1024}')" "$interconnect"
}

write_linux_vllm_config() {
  if [[ -f "$VLLM_CONFIG" ]]; then
    say "keeping existing vLLM configuration $VLLM_CONFIG"
    return
  fi
  local hw_class="REQUIRE_NVIDIA_DETECTION" gpu_count=0 memory_gb=0.0 interconnect=""
  local detected
  if detected="$(detect_linux_vllm_topology)"; then
    read -r hw_class gpu_count memory_gb interconnect <<<"$detected"
    say "detected CUDA topology: class=$hw_class gpus=$gpu_count memory_gb_per_gpu=$memory_gb"
  else
    warn "no supported NVIDIA topology was detected; the vLLM config will refuse activation until corrected on a supported CUDA host"
  fi
  local public_url="${MERC_VLLM_PUBLIC_BASE_URL:-https://REQUIRE_CONFIGURED_TLS_PUBLIC_ENDPOINT/v1}"
  if [[ "$public_url" == *'"'* || "$public_url" == *$'\n'* || "$public_url" == *$'\r'* ]]; then
    die "MERC_VLLM_PUBLIC_BASE_URL cannot contain quotes or newlines"
  fi
  umask 077
  cat >"$VLLM_CONFIG" <<TOML
control_url = "${MERC_CONTROL_URL:-http://localhost:8080}"
worker_token = "${MERC_WORKER_TOKEN:-PASTE_WORKER_TOKEN_FROM_make_seed}"
runtime_profile_path = "${VLLM_PROFILE}"
public_base_url = "${public_url}"
model_cache_dir = "${HOMEDIR}/models"
operator_prefs_path = "${PREFS}"
container_runtime = "docker"
listen_host = "127.0.0.1"
listen_port = 8000
max_active_sequences = 128
startup_timeout_secs = 900
hw_class = "${hw_class}"
gpu_count = ${gpu_count}
memory_gb_per_gpu = ${memory_gb}
memory_gb_in_use = 0.0
interconnect = "${interconnect}"
supplier_input_usd_per_million_tokens = 0.08
supplier_output_usd_per_million_tokens = 0.30
TOML
  say "wrote pinned CUDA/vLLM configuration $VLLM_CONFIG"
  [[ -n "${MERC_WORKER_TOKEN:-}" ]] || warn "set worker_token in $VLLM_CONFIG before earning"
  [[ -n "${MERC_VLLM_PUBLIC_BASE_URL:-}" ]] || warn "set public_base_url in $VLLM_CONFIG to the externally reachable TLS endpoint before activation"
  say "after configuration, verify without pulling or advertising: $BIN vllm-check --config $VLLM_CONFIG"
}

# Existing Linux installs predate the vLLM adapter's live policy binding. Add
# the one unambiguous sidecar path without rewriting supplier-provided runtime
# values; an explicit existing binding remains the supplier's authority.
bind_linux_vllm_prefs() {
  [[ "$OS" == "linux" && -f "$VLLM_CONFIG" ]] || return
  if grep -Eq '^[[:space:]]*operator_prefs_path[[:space:]]*=' "$VLLM_CONFIG"; then
    return
  fi
  umask 077
  printf '\noperator_prefs_path = "%s"\n' "$PREFS" >>"$VLLM_CONFIG"
  say "bound live supplier preferences to $VLLM_CONFIG"
}

write_config() {
  migrate_legacy_state
  mkdir -p "$HOMEDIR"
  if [[ "$OS" == "linux" ]]; then
    write_linux_vllm_config
    return
  fi
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
# The Linux vLLM adapter uses a public host-networked endpoint. A nonzero
# bandwidth cap is refused until the adapter has a real traffic shaper.
max_bandwidth_mbps = 0.0
max_cpu_pct = 80.0
thermal_limit = "serious"
power_only = true
quiet_hours = [22, 6]
# A vLLM supplier rate is not an hourly-earnings guarantee. A nonzero floor is
# refused until continuous service revenue is metered against it.
min_payout_usd_per_hr = 0.0
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
ExecStart=${BIN} vllm --config ${VLLM_CONFIG}
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
      if grep -Eq 'PASTE_WORKER_TOKEN|REQUIRE_CONFIGURED_TLS_PUBLIC_ENDPOINT|REQUIRE_NVIDIA_DETECTION' "$VLLM_CONFIG"; then
        die "vLLM activation requires a real worker token, TLS public endpoint, and detected supported NVIDIA topology in $VLLM_CONFIG"
      fi
      systemctl --user enable --now merc-agent.service \
        && say "agent enabled and started (systemd --user)" \
        || die "could not enable/start merc-agent.service"
    else
      say "to start now: systemctl --user enable --now merc-agent.service (or re-run with --start)"
    fi
  else
    warn "systemctl is unavailable; run $BIN vllm --config $VLLM_CONFIG under your service manager"
  fi
  say "Linux starts the pinned CUDA/vLLM adapter, not the generic Apple-oriented runner."
  say "It will refuse to advertise capacity until the pinned container is healthy and control accepts the configured runtime profile."
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
  if [[ "$OS" == "linux" ]]; then
    say "  would install the pinned vLLM profile: $VLLM_PROFILE (if missing)"
    say "  would write CUDA/vLLM config: $VLLM_CONFIG (if missing)"
  else
    say "  would write starter config: $CONFIG (if missing)"
  fi
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
bind_linux_vllm_prefs

case "$OS" in
  darwin) install_darwin_launchagent ;;
  linux)  install_linux_service ;;
esac
