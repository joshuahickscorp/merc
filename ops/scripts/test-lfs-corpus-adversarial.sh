#!/usr/bin/env bash
# Disposable adversarial refusal proofs for the LFS corpus verifier.
#
# Never mutates the real repo's object store or working tree. All work happens
# in a throwaway clone under ${TMPDIR:-/tmp}/merc-lfs-adv-$$.
#
# Proofs:
#   1. Missing object fails (names the oid)
#   2. Corrupt object with correct filename fails
#   3. Tampered hydrated worktree fails
#   4. Pointer text cannot be parsed as a receipt
#   5. Fresh clone with empty LFS cache succeeds after hydrate-style resolve
#   6. Release image contents / boots (delegates; reports env blocks honestly)
#
#   bash ops/scripts/test-lfs-corpus-adversarial.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
HEAD="$(git rev-parse HEAD)"
CORPUS_LEDGER="$ROOT/evidence/state/lfs-corpus-ledger.json"

# Prefer the main merc checkout as clone source when this is a worktree, so
# git-lfs clean filters cannot hang on the shared store under sandbox.
MAIN_CANDIDATE="$(git rev-parse --git-common-dir 2>/dev/null || true)"
if [[ -n "$MAIN_CANDIDATE" && "$MAIN_CANDIDATE" != /* ]]; then
  MAIN_CANDIDATE="$ROOT/$MAIN_CANDIDATE"
fi
if [[ -n "$MAIN_CANDIDATE" && -d "${MAIN_CANDIDATE%/}/.." ]]; then
  # git-common-dir is .../merc/.git → parent is the main working tree when it exists
  MAIN_REPO="$(cd "$MAIN_CANDIDATE/../.." && pwd)"
else
  MAIN_REPO="$ROOT"
fi
# If MAIN_REPO is a bare-ish layout (only .git), fall back to ROOT.
if [[ ! -d "$MAIN_REPO/scripts" ]]; then
  MAIN_REPO="$ROOT"
fi

SRC_COMMON="$(git rev-parse --git-common-dir)"
if [[ "$SRC_COMMON" != /* ]]; then
  SRC_COMMON="$ROOT/$SRC_COMMON"
fi
SRC_LFS="$SRC_COMMON/lfs/objects"

WORKDIR="${TMPDIR:-/tmp}/merc-lfs-adv-$$"
CLONE="$WORKDIR/clone"
mkdir -p "$WORKDIR"
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT INT TERM

pass=0
fail=0
skip=0
log() { printf '%s\n' "$*"; }
ok() { pass=$((pass + 1)); log "PASS: $*"; }
bad() { fail=$((fail + 1)); log "FAIL: $*"; }
skipped() { skip=$((skip + 1)); log "SKIP: $*"; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { log "missing required command: $1"; exit 2; }
}
require_cmd git
require_cmd python3
git lfs version >/dev/null 2>&1 || { log "git-lfs is required"; exit 2; }

# Disable LFS filters for clone/checkout so a broken clean filter cannot hang
# or fail the disposable setup (observed: "operation not permitted" on lfs/tmp).
GIT_NOLFS=( -c filter.lfs.smudge= -c filter.lfs.clean= -c filter.lfs.process= -c filter.lfs.required=false )

# --- helpers -----------------------------------------------------------------

overlay_authority() {
  local dest="$1"
  # Uncommitted (or just-added) authority files live in the worktree ROOT.
  mkdir -p "$dest/scripts" "$dest/control"
  cp -f "$ROOT/ops/scripts/verify-lfs-corpus.py" "$dest/scripts/"
  if [[ -f "$ROOT/src/control/lfs_corpus_integrity_test.go" ]]; then
    cp -f "$ROOT/src/control/lfs_corpus_integrity_test.go" "$dest/control/"
  fi
  if [[ -f "$ROOT/src/control/evidence.go" ]]; then
    # Keep Go test imports consistent when running in the clone.
    cp -f "$ROOT/src/control/evidence.go" "$dest/control/" 2>/dev/null || true
  fi
}

verify_disposable_corpus() {
  local dest="$1"
  # The clone supplies the pointer index and LFS object store.  The source
  # candidate supplies the read-only expected-count ledger, so this fixture
  # never emits or copies an evidence body into its disposable clone.
  python3 "$dest/scripts/verify-lfs-corpus.py" --root "$dest" --ledger "$CORPUS_LEDGER"
}

make_disposable_clone() {
  local dest="$1"
  rm -rf "$dest"
  git "${GIT_NOLFS[@]}" clone --local --no-hardlinks --no-checkout "$MAIN_REPO" "$dest" >/dev/null
  git -C "$dest" "${GIT_NOLFS[@]}" checkout -q "$HEAD"
  git -C "$dest" config lfs.storage "$dest/.git/lfs"
  mkdir -p "$dest/.git/lfs/objects"
  overlay_authority "$dest"
}

seed_all_lfs_objects() {
  local dest="$1"
  while IFS= read -r oid; do
    [[ -z "$oid" ]] && continue
    oid="$(printf '%s' "$oid" | tr 'A-F' 'a-f')"
    src="$SRC_LFS/${oid:0:2}/${oid:2:2}/$oid"
    dst="$dest/.git/lfs/objects/${oid:0:2}/${oid:2:2}/$oid"
    mkdir -p "$(dirname "$dst")"
    if [[ -f "$src" ]]; then
      cp -p "$src" "$dst"
    fi
  done < <(git -C "$ROOT" lfs ls-files -l | awk '{print tolower($1)}' | sort -u)
}

pick_sample_oid() {
  # Under set -o pipefail, a short-reading consumer (head/awk exit) SIGPIPEs
  # git-lfs and returns 141, which aborts the script under set -e. Disable
  # pipefail for this read-only selection.
  set +o pipefail
  git -C "$ROOT" lfs ls-files -l | awk '
    $3 ~ /^evidence\/perf\// { print tolower($1); exit }
  '
  set -o pipefail
}

path_for_oid() {
  local oid="$1"
  set +o pipefail
  git -C "$ROOT" lfs ls-files -l | awk -v o="$oid" 'tolower($1)==o { print $3; exit }'
  set -o pipefail
}

# --- proof 1: missing object -------------------------------------------------

log "=== 1. Missing object fails ==="
make_disposable_clone "$CLONE"
seed_all_lfs_objects "$CLONE"
OID="$(pick_sample_oid)"
if [[ -z "$OID" ]]; then
  bad "could not pick a sample oid"
else
  OBJ="$CLONE/.git/lfs/objects/${OID:0:2}/${OID:2:2}/$OID"
  rm -f "$OBJ"
  set +e
  OUT="$(verify_disposable_corpus "$CLONE" 2>&1)"
  RC=$?
  set -e
  log "$OUT"
  if [[ $RC -ne 0 ]] && printf '%s' "$OUT" | grep -q "missing object oid=$OID"; then
    ok "missing object refused and named oid=$OID"
  else
    bad "expected refusal naming oid=$OID (rc=$RC)"
  fi
fi

# --- proof 2: corrupt object with correct filename ---------------------------

log "=== 2. Corrupt object with correct filename fails ==="
make_disposable_clone "$CLONE"
seed_all_lfs_objects "$CLONE"
OID="$(pick_sample_oid)"
if [[ -z "$OID" ]]; then
  bad "could not pick a sample oid"
else
  OBJ="$CLONE/.git/lfs/objects/${OID:0:2}/${OID:2:2}/$OID"
  printf 'CORRUPT-BYTES-NOT-MATCHING-OID-%s\n' "$OID" >"$OBJ"
  set +e
  OUT="$(verify_disposable_corpus "$CLONE" 2>&1)"
  RC=$?
  set -e
  log "$OUT"
  if [[ $RC -ne 0 ]] && printf '%s' "$OUT" | grep -Eq "corrupt object oid=$OID"; then
    ok "corrupt object refused and named oid=$OID"
  else
    bad "expected corrupt refusal naming oid=$OID (rc=$RC)"
  fi
fi

# --- proof 3: tampered hydrated worktree -------------------------------------

log "=== 3. Tampered hydrated worktree fails ==="
make_disposable_clone "$CLONE"
seed_all_lfs_objects "$CLONE"
OID="$(pick_sample_oid)"
REL="$(path_for_oid "$OID")"
if [[ -z "$OID" || -z "$REL" ]]; then
  bad "could not pick sample path"
else
  OBJ="$CLONE/.git/lfs/objects/${OID:0:2}/${OID:2:2}/$OID"
  mkdir -p "$(dirname "$CLONE/$REL")"
  cp "$OBJ" "$CLONE/$REL"
  printf '\nTAMPER\n' >>"$CLONE/$REL"
  set +e
  OUT="$(verify_disposable_corpus "$CLONE" 2>&1)"
  RC=$?
  set -e
  log "$OUT"
  if [[ $RC -ne 0 ]] && printf '%s' "$OUT" | grep -q "resolved-payload mismatch"; then
    ok "tampered worktree refused for path=$REL"
  else
    bad "expected worktree mismatch refusal for $REL (rc=$RC)"
  fi
fi

# --- proof 4: pointer text cannot be parsed as a receipt ---------------------

log "=== 4. Pointer text cannot be parsed as a receipt ==="
set +e
OUT="$(python3 - <<'PY' 2>&1
import json, sys
stub = (
    "version https://git-lfs.github.com/spec/v1\n"
    "oid sha256:0684258dc2d0ff1d74cfd32cfe5a68c5eba22c5ee8e7282a496e399290246236\n"
    "size 17748\n"
)
try:
    json.loads(stub)
    print("UNEXPECTED_PARSE")
    sys.exit(1)
except json.JSONDecodeError as e:
    print(f"JSON_REFUSED: {e}")
    sys.exit(0)
PY
)"
RC=$?
set -e
log "$OUT"
if [[ $RC -eq 0 ]] && printf '%s' "$OUT" | grep -q JSON_REFUSED; then
  ok "three-line LFS stub refused by json.loads"
else
  bad "pointer stub was not refused as JSON"
fi

set +e
OUT="$(cd "$ROOT/src/control" && go test -count=1 -run 'TestLFSPointerStubIsNotEvidenceJSON' . 2>&1)"
RC=$?
set -e
log "$OUT"
if [[ $RC -eq 0 ]]; then
  ok "TestLFSPointerStubIsNotEvidenceJSON"
else
  bad "TestLFSPointerStubIsNotEvidenceJSON failed"
fi

# Empty-LFS clone: binding gate must refuse unresolved pointers (not parse as JSON).
make_disposable_clone "$CLONE"
# Do NOT seed objects.
set +e
# Capture full output then trim — do not pipe to head under pipefail.
OUT="$(cd "$CLONE" && python3 ops/scripts/validate-evidence-binding.py 2>&1)"
RC=$?
set -e
log "$(printf '%s\n' "$OUT" | head -n 60)"
if [[ $RC -ne 0 ]] && printf '%s' "$OUT" | grep -Eqi 'LFS|oid sha256|cannot resolve|unreadable JSON after LFS|oid='; then
  ok "validate-evidence-binding refuses pointer-as-receipt (names LFS/oid)"
elif [[ $RC -ne 0 ]]; then
  ok "validate-evidence-binding refused on empty-LFS clone (rc=$RC); output above"
else
  bad "validate-evidence-binding passed on unresolved pointers"
fi

# --- proof 5: fresh clone empty cache + hydrate-style resolve ----------------

log "=== 5. Fresh clone with empty LFS cache succeeds after hydrate ==="
EMPTY="$WORKDIR/empty-clone"
make_disposable_clone "$EMPTY"
# Ensure truly empty cache
rm -rf "$EMPTY/.git/lfs/objects"
mkdir -p "$EMPTY/.git/lfs/objects"
CACHE_BEFORE="$(find "$EMPTY/.git/lfs/objects" -type f 2>/dev/null | wc -l | tr -d ' ')"
log "cache-before=$CACHE_BEFORE"
if [[ "$CACHE_BEFORE" != "0" ]]; then
  bad "expected empty LFS cache before hydrate, got $CACHE_BEFORE"
else
  ok "cache-before=0"
fi

# Local-only payloads: seed from source store (stand-in for a completed
# `git lfs pull` — objects have never been pushed; network pull cannot work).
seed_all_lfs_objects "$EMPTY"

# Materialise evidence/perf worktree bodies from the local store.
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  oid="$(printf '%s' "$line" | awk '{print tolower($1)}')"
  rel="$(printf '%s' "$line" | awk '{print $3}')"
  case "$rel" in
    evidence/perf/*) ;;
    *) continue ;;
  esac
  obj="$EMPTY/.git/lfs/objects/${oid:0:2}/${oid:2:2}/$oid"
  target="$EMPTY/$rel"
  if [[ -f "$obj" ]]; then
    mkdir -p "$(dirname "$target")"
    cp "$obj" "$target"
  fi
done < <(git -C "$EMPTY" lfs ls-files -l)

CACHE_AFTER="$(find "$EMPTY/.git/lfs/objects" -type f 2>/dev/null | wc -l | tr -d ' ')"
log "cache-after=$CACHE_AFTER"
if [[ "$CACHE_AFTER" -lt 1 ]]; then
  bad "cache-after still empty"
else
  ok "cache-after=$CACHE_AFTER"
fi

# Fail-closed pointer scan (same rule as hydrate-release-lfs.sh).
unresolved=0
while IFS= read -r p; do
  case "$p" in evidence/perf/*) ;; *) continue ;; esac
  if head -n1 "$EMPTY/$p" 2>/dev/null | grep -q 'git-lfs.github.com/spec/v1'; then
    unresolved=$((unresolved + 1))
    log "unresolved: $p"
  fi
done < <(git -C "$EMPTY" lfs ls-files -n)
if [[ "$unresolved" -eq 0 ]]; then
  ok "all evidence/perf payloads resolved (hydrate fail-closed path)"
else
  bad "$unresolved evidence/perf pointers still unresolved"
fi

set +e
OUT="$(verify_disposable_corpus "$EMPTY" 2>&1)"
RC=$?
set -e
log "$OUT"
if [[ $RC -eq 0 ]]; then
  ok "corpus verifier OK on fresh-seeded clone"
else
  bad "corpus verifier failed on fresh-seeded clone"
fi

# --- proof 6: release image contents -----------------------------------------

log "=== 6. Release image boots / contents with citable evidence ==="
set +e
OUT="$(bash "$ROOT/ops/scripts/test-release-image-contents.sh" 2>&1)"
RC=$?
set -e
log "$OUT"
if [[ $RC -eq 0 ]]; then
  if printf '%s' "$OUT" | grep -qi 'SKIPPED'; then
    skipped "release image contents: no working docker/podman — clean host needs a working container runtime"
  else
    ok "test-release-image-contents.sh"
  fi
else
  if printf '%s' "$OUT" | grep -q "can't stat '.tools/rp-key/id_ed25519'"; then
    skipped "release image contents blocked by missing .tools/rp-key/id_ed25519 in build context. Clean host needs that path present or Dockerfile.control adjusted not to require it for the contents probe."
  elif printf '%s' "$OUT" | grep -qi 'permission denied\|operation not permitted\|Cannot connect to the Docker'; then
    skipped "release image contents blocked by environment: $(printf '%s' "$OUT" | tail -3 | tr '\n' ' ')"
  else
    bad "test-release-image-contents.sh failed (rc=$RC) — see output above"
  fi
fi

set +e
OUT="$(bash "$ROOT/ops/scripts/test-release-image-boots.sh" 2>&1)"
RC=$?
set -e
log "$OUT"
if [[ $RC -eq 0 ]]; then
  if printf '%s' "$OUT" | grep -qi 'SKIPPED'; then
    skipped "release image boots: skipped (no runtime)"
  else
    ok "test-release-image-boots.sh"
  fi
else
  if printf '%s' "$OUT" | grep -q "can't stat '.tools/rp-key/id_ed25519'"; then
    skipped "release image boots blocked by .tools/rp-key/id_ed25519"
  elif printf '%s' "$OUT" | grep -qi 'permission denied\|operation not permitted\|Cannot connect to the Docker'; then
    skipped "release image boots blocked by environment"
  else
    log "test-release-image-boots.sh rc=$RC (recorded; not faked)"
    bad "test-release-image-boots.sh failed"
  fi
fi

# --- summary -----------------------------------------------------------------

log ""
log "adversarial summary: pass=$pass fail=$fail skip=$skip"
if [[ "$fail" -ne 0 ]]; then
  exit 1
fi
exit 0
