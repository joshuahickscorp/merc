#!/usr/bin/env bash
set -euo pipefail

package_root="$(cd "$(dirname "$0")/.." && pwd)"
repository_root="$(cd "$package_root/../.." && pwd)"
smoke_root="${BVMCP_SMOKE_ROOT:-$package_root/.smoke/mac-studio}"

if [[ -e "$smoke_root/project.json" || -e "$smoke_root/project.db" ]]; then
  echo "smoke project already exists: $smoke_root" >&2
  exit 2
fi

cd "$package_root"
uv run bvmcp project create mac-studio --root "$smoke_root" \
  --scene "$repository_root/models/mac_studio/final_packed.blend"
uv run bvmcp reference import "$repository_root/web/assets/site/mac-studio@3x.png" \
  --project "$smoke_root" --rights-state INTERNAL --viewpoint-label front
uv run bvmcp workflow audit-reference-fidelity --project "$smoke_root" --maximum-dimension 512
uv run bvmcp project verify --project "$smoke_root"
