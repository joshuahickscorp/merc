#!/usr/bin/env bash
# Prove that the exact candidate's LFS corpus can be recovered from an
# off-machine origin with no inherited object cache.
#
# This is intentionally NOT a local-clone test.  A local clone plus copied
# objects only proves that this workstation still has the bytes.  Before merge,
# the candidate must be reachable from origin and every LFS body must be
# downloadable into a brand-new repository whose lfs.storage starts empty.
#
# The script is read-only with respect to the remote.  It never pushes or
# repairs an object.  Missing commits or payloads are failures that must be
# fixed by an authorized publisher before the candidate can merge.
#
# Usage:
#   bash scripts/verify-lfs-origin-durability.sh
#   bash scripts/verify-lfs-origin-durability.sh --commit <full-sha>
#   bash scripts/verify-lfs-origin-durability.sh --remote origin --json
#
# Optional environment overrides are useful for an explicitly authorized CI
# remote, but a file path, file:// URL, or loopback URL is always refused:
#   MERC_LFS_DURABILITY_COMMIT
#   MERC_LFS_DURABILITY_REMOTE
#   MERC_LFS_DURABILITY_REMOTE_URL
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$ROOT" ]]; then
  echo "lfs-origin-durability: FAIL -- not inside a git repository" >&2
  exit 2
fi
cd "$ROOT"

FORMAT_JSON=0
REMOTE_NAME="${MERC_LFS_DURABILITY_REMOTE:-origin}"
REMOTE_URL="${MERC_LFS_DURABILITY_REMOTE_URL:-}"
TARGET_INPUT="${MERC_LFS_DURABILITY_COMMIT:-HEAD}"
TARGET=""
SAFE_REMOTE_URL=""
WORKDIR=""
CLONE=""
VERIFY_JSON=""
CACHE_BEFORE=""
CACHE_AFTER=""
LFS_PATHS=""
FETCH_STATUS="not-run"
CHECKOUT_STATUS="not-run"
CANONICAL_REMOTE_ID=""
EXPECTED_LFS_ENDPOINT=""
RESOLVED_LFS_ENDPOINT=""

usage() {
  cat <<'USAGE'
usage: verify-lfs-origin-durability.sh [--commit SHA] [--remote NAME] [--remote-url URL] [--json]

Builds a fresh temporary repository from the named network remote, starts with
an empty private LFS object store, fetches every LFS object referenced by SHA
with lfs.fetchexclude explicitly cleared, hydrates every tracked path, and runs
the independent SHA-256/OID/size verifier from that exact commit.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --commit)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      TARGET_INPUT="$2"
      shift 2
      ;;
    --remote)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      REMOTE_NAME="$2"
      shift 2
      ;;
    --remote-url)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      REMOTE_URL="$2"
      shift 2
      ;;
    --json)
      FORMAT_JSON=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

note() {
  if [[ "$FORMAT_JSON" -eq 1 ]]; then
    printf '%s\n' "$*" >&2
  else
    printf '%s\n' "$*"
  fi
}

emit_json() {
  local status="$1" message="$2"
  python3 - "$status" "$message" "$TARGET" "$SAFE_REMOTE_URL" \
    "$CANONICAL_REMOTE_ID" "$CACHE_BEFORE" "$CACHE_AFTER" "$LFS_PATHS" \
    "$FETCH_STATUS" "$CHECKOUT_STATUS" "$VERIFY_JSON" "$EXPECTED_LFS_ENDPOINT" \
    "$RESOLVED_LFS_ENDPOINT" <<'PY'
import json
import pathlib
import sys

(
    status,
    message,
    target,
    remote,
    reviewed_repository,
    cache_before,
    cache_after,
    hydrated_paths,
    fetch_status,
    checkout_status,
    verifier_path,
    expected_lfs_endpoint,
    resolved_lfs_endpoint,
) = sys.argv[1:]

verification = None
if verifier_path:
    path = pathlib.Path(verifier_path)
    if path.is_file():
        try:
            verification = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            verification = {"parse_error": str(exc)}

def integer_or_none(value):
    try:
        return int(value)
    except (TypeError, ValueError):
        return None

print(json.dumps({
    "schema_version": 1,
    "kind": "lfs_origin_durability",
    "status": status,
    "message": message,
    "target_commit": target or None,
    "remote": remote or None,
    "reviewed_repository": reviewed_repository or None,
    "expected_lfs_endpoint": expected_lfs_endpoint or None,
    "resolved_lfs_endpoint": resolved_lfs_endpoint or None,
    "off_machine": True,
    "lfs_cache_before": integer_or_none(cache_before),
    "lfs_cache_after": integer_or_none(cache_after),
    "hydrated_lfs_paths": integer_or_none(hydrated_paths),
    "fetch_status": fetch_status,
    "checkout_status": checkout_status,
    "independent_verification": verification,
}, indent=2, sort_keys=True))
PY
}

fail() {
  local message="$*"
  note "lfs-origin-durability: FAIL -- $message"
  if [[ "$FORMAT_JSON" -eq 1 ]]; then
    emit_json "FAIL" "$message"
  fi
  exit 1
}

redact_remote() {
  # Do not place an embedded HTTP credential into an artifact or CI log.
  printf '%s' "$1" | sed -E 's#(https?://)[^/@]+@#\1<redacted>@#'
}

is_off_machine_network_remote() {
  local url="$1"
  case "$url" in
    file://*|/*|./*|../*|~/*) return 1 ;;
    https://*|ssh://*|*@*:* ) ;;
    *) return 1 ;;
  esac
  case "$url" in
    *localhost*|*127.0.0.1*|*'[::1]'*) return 1 ;;
  esac
  return 0
}

count_lfs_objects() {
  local objects="$1"
  if [[ ! -d "$objects" ]]; then
    printf '0'
    return
  fi
  find "$objects" -type f -print | wc -l | tr -d '[:space:]'
}

if ! command -v git >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1 ||
   ! git lfs version >/dev/null 2>&1; then
  fail "git, git-lfs, and python3 are required"
fi

if ! TARGET="$(git rev-parse --verify "${TARGET_INPUT}^{commit}" 2>/dev/null)"; then
  fail "${TARGET_INPUT} is not a locally known commit"
fi

if [[ -z "$REMOTE_URL" ]]; then
  if ! REMOTE_URL="$(git remote get-url "$REMOTE_NAME" 2>/dev/null)"; then
    fail "remote ${REMOTE_NAME} has no URL"
  fi
fi
SAFE_REMOTE_URL="$(redact_remote "$REMOTE_URL")"
if ! is_off_machine_network_remote "$REMOTE_URL"; then
  fail "refusing non-network or loopback remote ${SAFE_REMOTE_URL:-<empty>}; local bytes are not an off-machine durability proof"
fi
if ! CANONICAL_REMOTE_ID="$(python3 "$ROOT/scripts/validate-lfs-origin-authority.py" --url "$REMOTE_URL" 2>&1)"; then
  CANONICAL_REMOTE_ID=""
  fail "remote ${SAFE_REMOTE_URL:-<empty>} does not match reviewed repository authority; origin configuration alone is not authorization"
fi
if ! EXPECTED_LFS_ENDPOINT="$(python3 "$ROOT/scripts/validate-lfs-origin-authority.py" --expected-lfs-endpoint 2>&1)"; then
  EXPECTED_LFS_ENDPOINT=""
  fail "reviewed LFS endpoint authority is unavailable"
fi

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/merc-lfs-origin-durability.XXXXXX")"
CLONE="$WORKDIR/fresh-remote"
VERIFY_JSON="$WORKDIR/verification.json"
cleanup() {
  [[ -z "$WORKDIR" ]] || rm -rf -- "$WORKDIR"
}
trap cleanup EXIT INT TERM

# This is a new Git repository, not a worktree or --reference clone.  It has no
# objects, alternates, or LFS cache inherited from the candidate checkout.
git init -q "$CLONE"
git -C "$CLONE" remote add origin "$REMOTE_URL"
# GitHub Actions checkout stores its short-lived token as a scoped HTTP
# extraheader in the outer repository config. A fresh inner repository does
# not inherit it, so copy only GitHub's header locally for this disposable
# clone. Do not print, serialize, or copy any other credential configuration.
if [[ "$CANONICAL_REMOTE_ID" = github.com/* ]]; then
  while IFS= read -r github_header; do
    [[ -z "$github_header" ]] && continue
    git -C "$CLONE" config --local --add \
      "http.https://github.com/.extraheader" "$github_header"
  done < <(git config --get-all "http.https://github.com/.extraheader" 2>/dev/null || true)
fi
# Keep the LFS filter installation in the disposable repository. The proof
# deliberately ignores global/system configuration, so checkout must not rely
# on a machine-wide `git lfs install` entry.
git -C "$CLONE" lfs install --local >/dev/null
git -C "$CLONE" config lfs.storage "$CLONE/.git/lfs"
# Do not configure lfs.url here. For an SSH origin, Git LFS derives the
# GitHub credential flow from the remote; forcing an HTTPS endpoint can discard
# that authenticated transport. Candidate .lfsconfig is instead resolved and
# checked below before any payload fetch is allowed.
mkdir -p "$CLONE/.git/lfs/objects"
CACHE_BEFORE="$(count_lfs_objects "$CLONE/.git/lfs/objects")"
if [[ "$CACHE_BEFORE" != "0" ]]; then
  fail "fresh repository LFS cache started with ${CACHE_BEFORE} objects"
fi

note "lfs-origin-durability: fresh repository, empty LFS cache, fetching exact ${TARGET:0:12} from reviewed ${CANONICAL_REMOTE_ID}"
# Fetch the SHA directly.  A candidate that has not reached the remote cannot
# satisfy this check, which is precisely the pre-merge durability requirement.
if ! git -C "$CLONE" fetch --no-tags --depth=1 origin "$TARGET" >"$WORKDIR/git-fetch.log" 2>&1; then
  fail "origin does not expose exact commit ${TARGET}; push the candidate before requesting merge"
fi
REMOTE_HEAD="$(git -C "$CLONE" rev-parse FETCH_HEAD)"
if [[ "$REMOTE_HEAD" != "$TARGET" ]]; then
  fail "remote fetched ${REMOTE_HEAD}, not requested ${TARGET}"
fi

# Check out raw pointers first.  The explicit LFS fetch below is the only path
# permitted to hydrate bodies, so there is no accidental smudge/cache proof.
GIT_LFS_SKIP_SMUDGE=1 git -C "$CLONE" \
  -c filter.lfs.smudge= -c filter.lfs.process= -c filter.lfs.required=false \
  checkout --detach -q "$REMOTE_HEAD"

if [[ ! -f "$CLONE/scripts/verify-lfs-corpus.py" ]]; then
  fail "exact remote commit lacks scripts/verify-lfs-corpus.py; cannot establish independent corpus integrity"
fi

# Do not inherit caller global/system configuration. A candidate .lfsconfig is
# intentionally visible here: if it redirects the endpoint, the exact match
# check below refuses before fetching any payload from it.
RESOLVED_LFS_ENDPOINT="$(env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
  git -C "$CLONE" lfs env \
  | sed -n 's/^Endpoint=\([^ ]*\).*/\1/p' | head -n 1)"
if ! python3 "$ROOT/scripts/validate-lfs-origin-authority.py" \
  --lfs-endpoint "$RESOLVED_LFS_ENDPOINT" >/dev/null 2>&1; then
  fail "fresh clone resolved an LFS endpoint outside reviewed authority"
fi
if [[ "$RESOLVED_LFS_ENDPOINT" != "$EXPECTED_LFS_ENDPOINT" ]]; then
  fail "fresh clone resolved LFS endpoint differs from reviewed endpoint"
fi

# Clear both repository/global filters.  `--exclude=""` alone would still let
# a caller's lfs.fetchinclude narrow the corpus; `--include="*"` makes this
# explicitly every LFS path referenced by the exact commit, including
# .tools/runpodctl, not merely the receipts a particular CI job reads.
if env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
  git -C "$CLONE" -c lfs.fetchinclude= -c lfs.fetchexclude= \
  lfs fetch --include="*" --exclude="" origin "$REMOTE_HEAD" >"$WORKDIR/lfs-fetch.log" 2>&1; then
  FETCH_STATUS="OK"
else
  FETCH_STATUS="FAIL"
fi

# Materialise the fetched bodies and then make unresolved pointers a hard
# failure.  `git lfs fsck` is not consulted for this verdict.
if env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
  git -C "$CLONE" lfs checkout >"$WORKDIR/lfs-checkout.log" 2>&1; then
  CHECKOUT_STATUS="OK"
else
  CHECKOUT_STATUS="FAIL"
fi

LFS_PATHS=0
unresolved=0
while IFS= read -r path; do
  [[ -z "$path" ]] && continue
  LFS_PATHS=$((LFS_PATHS + 1))
  if [[ ! -f "$CLONE/$path" ]]; then
    note "lfs-origin-durability: unresolved missing path $path"
    unresolved=$((unresolved + 1))
    continue
  fi
  if head -c 64 "$CLONE/$path" 2>/dev/null | LC_ALL=C grep -Fq 'version https://git-lfs.github.com/spec/v1'; then
    note "lfs-origin-durability: unresolved pointer $path"
    unresolved=$((unresolved + 1))
  fi
done < <(git -C "$CLONE" lfs ls-files -n)
CACHE_AFTER="$(count_lfs_objects "$CLONE/.git/lfs/objects")"

# It derives the pointer/OID census from the exact remote tree, hashes every
# object body independently, checks pointer size, and checks hydrated worktree
# bytes.  Its supplementary fsck result is retained only as context.
VERIFY_STATUS=0
if ! python3 "$CLONE/scripts/verify-lfs-corpus.py" --root "$CLONE" --json >"$VERIFY_JSON"; then
  VERIFY_STATUS=1
fi

if [[ "$FETCH_STATUS" != "OK" ]]; then
  fail "git-lfs could not fetch every object from origin (independent verifier output identifies absent/corrupt OIDs)"
fi
if [[ "$CHECKOUT_STATUS" != "OK" ]]; then
  fail "git-lfs could not hydrate the origin-fetched corpus"
fi
if [[ "$LFS_PATHS" -eq 0 ]]; then
  fail "exact remote commit has no tracked LFS paths"
fi
if [[ "$unresolved" -ne 0 ]]; then
  fail "${unresolved} of ${LFS_PATHS} origin-fetched LFS paths remain unresolved"
fi
if [[ "$VERIFY_STATUS" -ne 0 ]]; then
  fail "independent OID, size, and hydrated-body verifier rejected the origin-fetched corpus"
fi

note "lfs-origin-durability: PASS -- remote ${SAFE_REMOTE_URL} supplied ${LFS_PATHS} hydrated paths with empty-cache-before=${CACHE_BEFORE}; independent verifier accepted ${TARGET:0:12}"
if [[ "$FORMAT_JSON" -eq 1 ]]; then
  emit_json "PASS" "exact candidate is remotely recoverable from an empty private LFS cache"
fi
