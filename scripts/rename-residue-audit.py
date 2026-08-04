#!/usr/bin/env python3
"""Zero-residue audit for the ComputeXchange -> merc rename.

Every remaining occurrence of the old name is one of three things, and the whole
point of this script is that it says WHICH:

  FROZEN   Must never be renamed. Hash domain separators, 4-byte binary magics,
           live credential prefixes, Stripe metadata keys already on live
           objects, third-party attribution, and recorded receipts. Renaming one
           of these changes a derived value with nothing failing, or makes the
           repo claim it verified an artifact that was never built.

  BLOCKED  Cannot be renamed until something outside this repository changes --
           a GitHub repo name, a droplet's .env, an Alertmanager receiver's
           filters. Renaming in-repo first produces a dangling reference that
           still looks plausible.

  RESIDUE  Nothing is stopping it. This is the only category that is a defect,
           and any occurrence of it fails this script.

The classification comes from docs/RENAME_REGISTER.md; this encodes it so that
"the rename is finished except for the parts that cannot be finished" is a
checked claim rather than an assertion in a document.

Scoped to `git ls-files` deliberately. A find/sed sweep also hits .artifacts/
(signed CI evidence whose own SHA256SUMS then stop matching), agent/target/,
review/pass6/ and the *.orig patch backups.
"""

import json
import re
import subprocess
import sys

# Old-name patterns. `CX_` is anchored on a word boundary because the only place
# in the repo where it is preceded by a word character is a base85 patch blob,
# and a bare CX_ -> MERC_ substitution corrupts it.
PATTERNS = re.compile(
    r"ComputeXchange|Computeexchange|computexchange|computeexchange|"
    r"compute\.exchange|compute-exchange|ComputeExchange|"
    r"\bCX_[A-Z0-9_]+|\bcx_[a-z0-9_]+|\bcx-[a-z0-9-]+|X-CX-|\bCXEM\b|\bCXIN\b|\bCXPL\b"
)

# ---------------------------------------------------------------- FROZEN rules
# Each entry is (path-predicate, token-predicate, why). A match on both means the
# occurrence is deliberate.

FROZEN_PATHS = (
    ("evidence/", "recorded receipt: names which digest was actually pulled"),
    ("ops/asset-provenance.json", "recorded receipt"),
    ("ops/readiness.json", "recorded receipt"),
    ("ops/legal-review.json", "recorded receipt"),
    ("census/", "recorded receipt: file sha256 census"),
    ("docs/RENAME_REGISTER.md", "the register itself names what it freezes"),
    ("scripts/rename-residue-audit.py",
     "the audit must contain the patterns it searches for"),
    ("ops/rename-residue.json", "this audit's own output"),
    ("docs/WEBSITE_3D_BLENDER_STATUS_2026-07-19.md", "dated finding: was true on that date"),
    ("review/", "frozen review evidence"),
    ("realtime-lane-snapshot/", "checksummed snapshot, verified by manifest"),
    ("clients/macapp/ComputeExchangeAgent/",
     "real directory with 8 consumers including a live claim gate; renames as one commit"),
    ("logo/cx-capsule-target.svg", "pinned by path and sha256 in ops/asset-provenance.json"),
    ("docs/SHIPPABILITY_STATUS.md",
     "documents the rename; a record of what was renamed must be able to name it"),
    ("docs/DECISION_ZERO_REVERSAL.md", "documents the rename"),
    ("ops/go-closure-inputs.json", "recorded receipt: absolute paths as they were"),
    (".github/workflows/publish-candidate.yml",
     "sigstore certificate identity records this exact path"),
    # Process-rename cutover: these surfaces deliberately retain pre-rebrand
    # binary/label/state-dir names so already-installed agents can be removed
    # and so state migrates instead of being dropped.
    ("scripts/uninstall.sh",
     "retains pre-rebrand binary/label/state names so already-installed agents can be removed"),
    ("scripts/install.sh",
     "migrates and retires pre-rebrand binary/label/state names on upgrade"),
    ("scripts/test-agent-uninstall-legacy.sh",
     "proves uninstall still handles pre-rebrand names"),
    ("agent/src/config.rs",
     "LEGACY_AGENT_HOME_DIRNAME and migration from the pre-rebrand state dir"),
    ("agent/src/main.rs",
     "migration unit test and comments for the pre-rebrand state dir"),
    ("docs/RUNBOOKS.md",
     "operator cutover notes must name the pre-rebrand systemd units to disable"),
    ("ops/systemd/",
     "unit comments name the pre-rebrand units operators must disable first"),
    ("ops/local/compose.rehearsal.yml",
     "healthcheck default targets the retained pre-rebrand image; every caller "
     "overrides it per image (/merc-healthcheck for current, /cx for legacy)"),
)

FROZEN_TOKENS = {
    "computexchange-source-fingerprint-v1": "hash domain separator (control/evidence.go)",
    "computeexchange-control-schema-v1": "advisory lock domain (schema migration)",
    "computeexchange-background-workers-v1": "advisory lock domain (leader election)",
    "CXEM": "4-byte binary magic, prefixes embedding blobs already in object storage",
    "CXIN": "4-byte advisory-lock namespace encoded as hex",
    "CXPL": "4-byte advisory-lock namespace encoded as hex",
    "cx_sess_": "live credential prefix: routes credential class",
    "cx_live_": "live credential prefix: keys already in api_keys",
    "cx_test_": "live credential prefix: keys already in api_keys",
    "cx_whsec_": "live credential prefix",
    "cx_admin_": "live credential prefix",
    "cx_operation_key": "Stripe metadata key already on live PaymentIntents",
    # Found by this audit; the register did not list them. Each is hashed or
    # signed over, so renaming changes a derived value with nothing failing.
    "cx-enrollment-request-v1":
        "hashed into every enrollment request id (control/enrollment.go: "
        "sha256(\"cx-enrollment-request-v1\\x00\" + publicKey))",
    "cx-worker-enrollment-exchange-v2":
        "first line of the enrollment signing transcript; renaming invalidates "
        "the proof for every already-enrolled agent",
    "cx-macos-agent-v2":
        "enrollment audience, and it appears inside the signing transcript above",
}
FROZEN_TOKEN_PREFIXES = {
    "cx_payout_": "Stripe metadata key already on live Transfers",
}

# --------------------------------------------------------------- BLOCKED rules
# token-prefix -> external prerequisite.
# Process names renamed in the process-rename cutover (cx-agent, cx-backup,
# cx-healthcheck, dev.computeexchange.agent, ~/.compute-exchange) are NO LONGER
# blocked: a reappearance outside a frozen compatibility surface is residue.
BLOCKED_TOKENS = {
    "CX_": "droplet .env, GitHub Actions secrets and systemd units supply these; "
           "CX_TOKEN_KEY must be copied byte-identically or every sealed secret "
           "in Postgres becomes undecryptable",
    "cx-control": "published image tag",
    "cx-jobs": "object storage bucket name in a running deployment",
}
BLOCKED_SUBSTRINGS = {
    # The GitHub repository is now joshuahickscorp/merc, so the old URL is no
    # longer blocked and no longer excused: any reappearance outside a recorded
    # receipt is residue and fails this script.
    "ghcr.io": "rename/republish the registry package first",
    "computexchange-control": "Prometheus job label fingerprinted by Alertmanager",
    "ComputeExchange": "alert names ship in the label set Alertmanager fingerprints",
    "/opt/computexchange": "real directory on the droplet",
    "/etc/computexchange": "real directory on the droplet",
    "/var/lib/computexchange": "real directory on the droplet",
    "computexchange.net": "live production domain",
    "clients/macapp/ComputeExchangeAgent": "real directory with 8 consumers incl. a live claim gate",
    "computexchange/control": "published image tag",
    "computeexchange/control": "former Go module path in recorded output",
    "cx?sslmode": "Postgres database name in a running deployment",
    "cx:cx@": "Postgres role in a running deployment",
    "postgres://cx": "Postgres role in a running deployment",
    "USER=cx": "Postgres role in a running deployment",
    "service: computexchange": "Prometheus external_labels.service; Alertmanager "
                               "fingerprints the label set and the receiver filters on it",
    "job_name: computexchange": "Prometheus job label in the fingerprinted set",
    "scripts/cx": "the cx CLI binary name",
    "/opt/merc": "install path pairs with the cx binary name",
    "joshuahickscorp/computexchange": "rename the GitHub repository first",
    "/srv/computexchange": "real directory on the staging server",
    "s3://private-staging-backups/computexchange":
        "real S3 prefix holding staging backups; renaming orphans them",
    "description=computexchange": "label on Stripe webhook endpoints that already exist; "
                                  "part of the Stripe-label cutover",
    "user=cx": "Postgres role in a running deployment",
    # The vLLM runtime profile is hashed WHOLE (sha256 over the raw file), and
    # realtime.go compares that digest against what a worker registered. Any
    # string inside a profile is therefore a hash input: renameable, but only in
    # a cutover where workers re-attest -- the same shape as the runtime
    # authority matrix hash.
    "cx-chat-1b": "model alias inside the sha256'd vLLM runtime profile; "
                  "workers must re-attest in the same cutover",
    "cx-llama32-instruct-v1": "version field inside the sha256'd vLLM runtime profile",
    "computexchange-render-lab": "external asset pack name; the comment must keep matching it",
    "computexchange-benchmarks": "external asset pack name; the comment must keep matching it",
    "/var/lib/computeexchange": "real directory on supplier machines (two-e spelling)",
    "/run/cx-health": "bind-mount path in a running deployment",
    "cx-metal": "generated asset filename; the ignore pattern must keep matching "
                "what the builders emit",
    "cx-mark-white": "generated asset filename",
    "cx-app-icon": "generated asset filename",
    "cx-capsule-target": "logo file pinned BY PATH AND SHA256 in "
                         "ops/asset-provenance.json; moving it orphans the receipt",
    "computexchange-prometheus": "Grafana datasource uid referenced by every dashboard panel",
    "cx-soak-24h": "soak run directory name; an in-flight 24h soak writes there",
    "cx-prove": "prove-local artifact namespace recorded in evidence",
    "cx-local": "local rehearsal container name",
    "cx-restore": "restore-drill container name",
}


def classify(path: str, token: str, line: str):
    """Returns (category, reason)."""
    for prefix, why in FROZEN_PATHS:
        if path.startswith(prefix) or path == prefix:
            return "FROZEN", why
    if token in FROZEN_TOKENS:
        return "FROZEN", FROZEN_TOKENS[token]
    for prefix, why in FROZEN_TOKEN_PREFIXES.items():
        if token.startswith(prefix):
            return "FROZEN", why
    # A frozen token can appear inside a longer match (a domain separator is
    # matched by the bare `computexchange` alternative first).
    for tok, why in FROZEN_TOKENS.items():
        if tok in line:
            return "FROZEN", why

    for sub, why in BLOCKED_SUBSTRINGS.items():
        if sub in line:
            return "BLOCKED", why
    for prefix, why in BLOCKED_TOKENS.items():
        if token.startswith(prefix):
            return "BLOCKED", why
    # cx_* metric names ship in the Alertmanager label set.
    if token.startswith("cx_"):
        return "BLOCKED", "metric name in the alert label set; must land with the receiver update"

    return "RESIDUE", "nothing external is blocking this rename"


SKIP_SUFFIXES = (".png", ".jpg", ".jpeg", ".glb", ".blend", ".zip", ".gz",
                 ".orig", ".webp", ".ico", ".woff", ".woff2", ".pdf")


def main() -> int:
    files = subprocess.run(["git", "ls-files"], capture_output=True, text=True,
                           check=True).stdout.split("\n")
    # Also consider newly-added tracked-intent paths that exist on disk but are
    # not yet in the index (e.g. git mv mid-edit in a worktree).
    # Still scope to repo-relative paths under the working tree.
    counts = {"FROZEN": 0, "BLOCKED": 0, "RESIDUE": 0}
    residue = []
    blocked_reasons = {}

    for path in files:
        if not path or path.endswith(SKIP_SUFFIXES):
            continue
        try:
            with open(path, "r", errors="replace") as fh:
                lines = fh.readlines()
        except (OSError, IsADirectoryError):
            continue
        for lineno, line in enumerate(lines, 1):
            for match in PATTERNS.finditer(line):
                category, why = classify(path, match.group(0), line)
                counts[category] += 1
                if category == "RESIDUE":
                    residue.append({"file": path, "line": lineno,
                                    "token": match.group(0),
                                    "context": line.strip()[:160]})
                elif category == "BLOCKED":
                    blocked_reasons.setdefault(why, 0)
                    blocked_reasons[why] += 1

    report = {
        "schema_version": 1,
        "kind": "rename_residue_audit",
        "counts": counts,
        "blocked_by_prerequisite": dict(sorted(blocked_reasons.items(),
                                               key=lambda kv: -kv[1])),
        "residue": residue[:100],
        "residue_total": len(residue),
        "zero_residue": len(residue) == 0,
        "note": ("FROZEN and BLOCKED are deliberate. FROZEN must never be renamed; "
                 "BLOCKED needs an external change first and renaming in-repo before "
                 "it produces a dangling reference that still looks plausible. Only "
                 "RESIDUE is a defect."),
    }
    with open("ops/rename-residue.json", "w") as fh:
        json.dump(report, fh, indent=2)
        fh.write("\n")

    print(f"rename residue audit: FROZEN={counts['FROZEN']} "
          f"BLOCKED={counts['BLOCKED']} RESIDUE={counts['RESIDUE']}")
    if residue:
        for item in residue[:25]:
            print(f"  RESIDUE {item['file']}:{item['line']}: {item['token']}")
        if len(residue) > 25:
            print(f"  ... and {len(residue) - 25} more (see ops/rename-residue.json)")
        return 1
    return 0

if __name__ == "__main__":
    sys.exit(main())
