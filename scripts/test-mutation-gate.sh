#!/usr/bin/env bash
# Fast structural validation of gate selection and refusal behavior.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

help="$(bash scripts/mutation-gate.sh --help)"
printf '%s\n' "$help" | rg --fixed-strings 'full       all 84 mutations exactly once' >/dev/null
rg --fixed-strings 'budget="${MERC_MUTATION_WALLCLOCK_SECONDS:-299}"' scripts/mutation-gate.sh >/dev/null
rg --fixed-strings 'default_workers=14' scripts/mutation-gate.sh >/dev/null
if bash scripts/mutation-gate.sh deep >/dev/null 2>&1; then
  echo "mutation deep gate did not require explicit asynchronous acknowledgement" >&2
  exit 1
fi

# An unrelated changed path produces no mutation work instead of silently
# expanding the fast lane. The temporary base is a real current commit, so this
# does not write or depend on a dirty candidate.
none="$(MERC_MUTATION_BASE=HEAD bash scripts/mutation-gate.sh fast)"
printf '%s\n' "$none" | rg --fixed-strings 'PASS no declared mutations affected' >/dev/null

echo "test-mutation-gate: PASS tier selection is explicit and fail-closed"
