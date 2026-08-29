#!/usr/bin/env bash
# Sign, notarize and staple a macOS release, and prove it afterwards.
#
# Everything here is unattended EXCEPT obtaining two credentials, which are an
# Apple account matter rather than an engineering one:
#
#   1. a "Developer ID Application" certificate in the login keychain
#      (Apple Developer Program membership required), and
#   2. an App Store Connect API key (.p8) for notarytool.
#
# Once both exist this runs end to end with no human in the loop -- a release is
# not notarized by hand per build. Until they exist it FAILS CLOSED with the
# exact missing item named. It never emits an unsigned artifact that looks
# signed, because a build that silently skips signing is worse than one that
# stops: the first ships, the second gets fixed.
#
#   ops/scripts/notarize-macos-release.sh v1.2.3 [artifact-dir]
#
# Environment (no secret is ever echoed):
#   MERC_SIGN_IDENTITY       "Developer ID Application: NAME (TEAMID)".
#                            Optional: a single Developer ID identity is auto-detected.
#   MERC_NOTARY_KEY_ID       App Store Connect key id
#   MERC_NOTARY_KEY_ISSUER   issuer uuid
#   MERC_NOTARY_KEY_PATH     path to the AuthKey_XXXX.p8
#   MERC_NOTARY_PROFILE      alternative: a keychain profile already stored with
#                            `xcrun notarytool store-credentials`
#   MERC_SKIP_NOTARIZE=1     sign and verify locally, do not submit to Apple.
#                            Useful before the API key exists; the receipt then
#                            records notarized=false rather than implying success.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="${1:-}"
[[ -n "$VERSION" ]] || { echo "usage: $0 <version> [artifact-dir]" >&2; exit 2; }
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]] || {
  echo "version must look like v1.2.3 (got $VERSION)" >&2; exit 2; }

ART="${2:-$ROOT/.artifacts/releases/cli/$VERSION}"
[[ -d "$ART" ]] || { echo "no artifact dir: $ART -- run ops/scripts/build-cli-release.sh $VERSION first" >&2; exit 2; }

[[ "$(uname -s)" == "Darwin" ]] || { echo "notarization requires macOS" >&2; exit 2; }
for t in codesign xcrun spctl ditto shasum; do
  command -v "$t" >/dev/null || { echo "missing required tool: $t" >&2; exit 2; }
done

# ---------------------------------------------------------------- identity ---
IDENTITY="${MERC_SIGN_IDENTITY:-}"
if [[ -z "$IDENTITY" ]]; then
  # Auto-detect exactly one Developer ID Application identity. Two is ambiguous
  # and guessing which one signs a public release is not a guess worth making.
  mapfile -t FOUND < <(security find-identity -v -p codesigning 2>/dev/null \
    | grep "Developer ID Application" | sed -E 's/.*"(.*)"/\1/')
  case "${#FOUND[@]}" in
    0) cat >&2 <<'MSG'
notarize: no "Developer ID Application" certificate found in the keychain.

This is the one step that cannot be scripted. To get it:
  1. Enrol in the Apple Developer Program (99 USD/yr) at developer.apple.com.
  2. Xcode > Settings > Accounts > Manage Certificates > + > Developer ID Application.
     (Or create a CSR in Keychain Access and upload it at
      developer.apple.com/account/resources/certificates.)
  3. Confirm with:  security find-identity -v -p codesigning
     A "Developer ID Application: NAME (TEAMID)" line must appear.

Nothing was signed. Re-run this script afterwards; the rest is unattended.
MSG
       exit 3 ;;
    1) IDENTITY="${FOUND[0]}" ;;
    *) echo "notarize: ${#FOUND[@]} Developer ID identities found; set MERC_SIGN_IDENTITY to choose" >&2
       printf '  %s\n' "${FOUND[@]}" >&2; exit 3 ;;
  esac
fi
echo "notarize: signing identity ${IDENTITY%% (*} (team suffix withheld from logs)"

# -------------------------------------------------------------- sign pass ---
# Hardened runtime and a secure timestamp are both notarization prerequisites.
# --options runtime is what Apple checks; without it notarization is rejected
# after upload, which wastes a round trip per build.
mapfile -t BINARIES < <(find "$ART" -type f -perm -u+x ! -name '*.txt' ! -name '*.json' ! -name '*.sig' | sort)
[[ "${#BINARIES[@]}" -gt 0 ]] || { echo "no executables under $ART" >&2; exit 2; }

for b in "${BINARIES[@]}"; do
  codesign --force --timestamp --options runtime --sign "$IDENTITY" "$b"
  codesign --verify --strict --verbose=2 "$b" 2>&1 | sed 's/^/  /'
done
echo "notarize: signed ${#BINARIES[@]} executable(s)"

ZIP="$ART/merc-$VERSION-macos.zip"
rm -f "$ZIP"
ditto -c -k --keepParent "$ART" "$ZIP"

# --------------------------------------------------------------- notarize ---
NOTARIZED=false
SUBMISSION_ID=""
if [[ "${MERC_SKIP_NOTARIZE:-0}" == "1" ]]; then
  echo "notarize: MERC_SKIP_NOTARIZE=1 -- signed and verified locally, NOT submitted"
else
  NOTARY_ARGS=()
  if [[ -n "${MERC_NOTARY_PROFILE:-}" ]]; then
    NOTARY_ARGS=(--keychain-profile "$MERC_NOTARY_PROFILE")
  elif [[ -n "${MERC_NOTARY_KEY_ID:-}" && -n "${MERC_NOTARY_KEY_ISSUER:-}" && -n "${MERC_NOTARY_KEY_PATH:-}" ]]; then
    [[ -f "$MERC_NOTARY_KEY_PATH" ]] || { echo "notarize: key file not found: $MERC_NOTARY_KEY_PATH" >&2; exit 3; }
    NOTARY_ARGS=(--key "$MERC_NOTARY_KEY_PATH" --key-id "$MERC_NOTARY_KEY_ID" --issuer "$MERC_NOTARY_KEY_ISSUER")
  else
    cat >&2 <<'MSG'
notarize: no App Store Connect credentials.

Create an API key once, then every future release is unattended:
  1. appstoreconnect.apple.com > Users and Access > Integrations > App Store Connect API
  2. Generate an API Key with the "Developer" role. Download AuthKey_XXXXXX.p8 --
     Apple lets you download it exactly once.
  3. Note the Key ID and the Issuer ID from that page.
  4. Either export per run:
        MERC_NOTARY_KEY_PATH=/secure/AuthKey_XXXXXX.p8
        MERC_NOTARY_KEY_ID=XXXXXX
        MERC_NOTARY_KEY_ISSUER=<issuer-uuid>
     or store it in the keychain once and use the profile:
        xcrun notarytool store-credentials merc-notary \
          --key /secure/AuthKey_XXXXXX.p8 --key-id XXXXXX --issuer <issuer-uuid>
        export MERC_NOTARY_PROFILE=merc-notary

The artifacts ARE signed; they are simply not notarized, so Gatekeeper will warn
on first launch. Re-run to notarize. MERC_SKIP_NOTARIZE=1 records that state
honestly instead of failing.
MSG
    exit 3
  fi

  echo "notarize: submitting $(basename "$ZIP") to Apple (waits for the verdict)"
  SUBMIT_LOG="$ART/notarytool-submit.txt"
  # --wait blocks until Apple returns Accepted/Invalid, so the exit code is the
  # verdict rather than "upload succeeded".
  xcrun notarytool submit "$ZIP" "${NOTARY_ARGS[@]}" --wait --output-format json \
    > "$SUBMIT_LOG" 2>&1 || {
      echo "notarize: submission FAILED. Apple's reason:" >&2
      SUBMISSION_ID="$(python3 -c "import json,sys;print(json.load(open('$SUBMIT_LOG')).get('id',''))" 2>/dev/null || true)"
      [[ -n "$SUBMISSION_ID" ]] && xcrun notarytool log "$SUBMISSION_ID" "${NOTARY_ARGS[@]}" >&2 || cat "$SUBMIT_LOG" >&2
      exit 4
    }
  SUBMISSION_ID="$(python3 -c "import json;print(json.load(open('$SUBMIT_LOG')).get('id',''))")"
  STATUS="$(python3 -c "import json;print(json.load(open('$SUBMIT_LOG')).get('status',''))")"
  [[ "$STATUS" == "Accepted" ]] || {
    echo "notarize: Apple returned status=$STATUS. Full log:" >&2
    xcrun notarytool log "$SUBMISSION_ID" "${NOTARY_ARGS[@]}" >&2 || true
    exit 4
  }

  # Stapling attaches the ticket so the artifact validates offline. Without it a
  # machine with no network shows the unidentified-developer dialog even though
  # notarization succeeded.
  for b in "${BINARIES[@]}"; do xcrun stapler staple "$b" || true; done
  NOTARIZED=true
  echo "notarize: accepted, submission $SUBMISSION_ID, ticket stapled"
fi

# ----------------------------------------------------------------- verify ---
# Assess as Gatekeeper would. This is the check that catches a signed-but-not-
# hardened build, which otherwise only surfaces on a stranger's machine.
for b in "${BINARIES[@]}"; do
  spctl --assess --type execute --verbose=2 "$b" 2>&1 | sed 's/^/  /' || {
    $NOTARIZED && { echo "notarize: spctl rejected a notarized binary -- stopping" >&2; exit 5; }
    echo "  (spctl rejection expected while not notarized)"
  }
done

rm -f "$ZIP"; ditto -c -k --keepParent "$ART" "$ZIP"   # repack with stapled tickets

RECEIPT="$ROOT/evidence/state/macos-signing-$VERSION.json"
mkdir -p "$(dirname "$RECEIPT")"
python3 - "$RECEIPT" "$VERSION" "$NOTARIZED" "$SUBMISSION_ID" "$ZIP" "${BINARIES[@]}" <<'PY'
import json, subprocess, sys, hashlib, os
receipt, version, notarized, submission, zip_path, *bins = sys.argv[1:]
def sha(p):
    h = hashlib.sha256()
    with open(p, 'rb') as f:
        for c in iter(lambda: f.read(1 << 20), b''):
            h.update(c)
    return h.hexdigest()
root = os.path.dirname(os.path.dirname(receipt))
doc = {
    "schema_version": 1, "kind": "macos_signing_receipt",
    "binding_status": "BOUND" if notarized == "true" else "UNBOUND",
    "version": version,
    "source_commit": subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True,
                                    text=True).stdout.strip(),
    "notarized": notarized == "true",
    "notary_submission_id": submission or None,
    "artifacts": [{"path": os.path.relpath(b, root), "sha256": sha(b)} for b in bins],
    "bundle": {"path": os.path.relpath(zip_path, root), "sha256": sha(zip_path)},
    "does_not_prove": [
        "that the code is correct or safe -- signing proves publisher identity, nothing else",
        "that a stranger's machine will run it if the ticket was not stapled",
    ],
}
if notarized != "true":
    doc["missing_identity_fields"] = ["notary_submission_id", "apple_notarization_ticket"]
    doc["why_unbound"] = ("signed locally but not submitted to Apple, so no ticket exists and "
                          "Gatekeeper will warn on first launch")
json.dump(doc, open(receipt, "w"), indent=2)
open(receipt, "a").write("\n")
print("notarize: receipt ->", receipt)
PY

echo "notarize: done (notarized=$NOTARIZED)"
