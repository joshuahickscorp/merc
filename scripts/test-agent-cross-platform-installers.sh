#!/usr/bin/env bash
# Contract gate for the three supplier-agent install paths.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="$ROOT/.github/workflows/agent-release.yml"
SH_INSTALL="$ROOT/scripts/install.sh"
PS_INSTALL="$ROOT/scripts/install.ps1"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
require() {
  local file="$1" pattern="$2" claim="$3"
  grep -Fq -- "$pattern" "$file" || fail "$claim"
}

require "$WORKFLOW" 'build-windows-amd64:' 'release workflow has no Windows build'
require "$WORKFLOW" 'cargo build --release --no-default-features' 'CPU-only cross-platform build is not explicit'
require "$WORKFLOW" 'merc-agent_${version}_windows_amd64' 'Windows archive name is not version-bound'
require "$WORKFLOW" 'collected/windows/*.zip' 'Windows archive is not assembled for signing'
require "$WORKFLOW" 'for archive in *.tar.gz *.zip' 'Windows archive is not publisher-signed'
require "$WORKFLOW" 'MERC_AGENT_VERSION="${GITHUB_REF_NAME#agent-}"' 'tagged Unix archives do not use the installable tag version'

require "$PS_INSTALL" 'Get-FileHash -Algorithm SHA256' 'Windows installer does not verify the archive digest'
require "$PS_INSTALL" '--certificate-identity-regexp' 'Windows installer does not pin publisher identity'
require "$PS_INSTALL" '--certificate-oidc-issuer' 'Windows installer does not pin the OIDC issuer'
require "$PS_INSTALL" 'Register-ScheduledTask' 'Windows installer does not install a persistent per-user agent'
require "$PS_INSTALL" 'run --config' 'Windows scheduled task does not run from the protected config file'
require "$PS_INSTALL" 'agent.prefs.toml' 'Windows installer does not create live operator preferences'

require "$SH_INSTALL" 'merc-agent.service' 'Linux installer does not install a user service'
require "$SH_INSTALL" 'vllm --config' 'Linux installer does not launch the pinned CUDA/vLLM adapter'
require "$SH_INSTALL" 'vllm-runtime-profile.json' 'Linux installer does not install the pinned vLLM profile'
require "$SH_INSTALL" 'ProtectSystem=strict' 'Linux service lacks filesystem isolation'
require "$SH_INSTALL" 'NoNewPrivileges=true' 'Linux service can gain privileges'
require "$SH_INSTALL" 'systemctl --user enable --now' 'Linux one-command start is missing'
require "$SH_INSTALL" 'agent.prefs.toml' 'Unix installer does not create live operator preferences'

bash -n "$SH_INSTALL" "$ROOT/scripts/uninstall.sh"
MERC_PREFIX="$ROOT/.artifacts/install-contract-never-created" bash "$SH_INSTALL" --check >/dev/null
test ! -e "$ROOT/.artifacts/install-contract-never-created" || fail '--check mutated the install prefix'

printf 'agent cross-platform installer contracts: PASS\n'
