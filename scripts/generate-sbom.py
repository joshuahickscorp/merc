#!/usr/bin/env python3
"""Generate a CycloneDX 1.5 SBOM for the merc distribution.

Deliberately dependency-free: it reads `go list -m -json all` and
`cargo metadata`, which are the toolchains this repo already builds with. An
SBOM generator that needs its own install is one more thing to trust, and the
build tools already know the exact graph they compiled.

What this records and what it does not:

  * Declared licenses only, taken from cargo metadata. Go modules do not
    declare a license in module metadata, so their `licenses` field is omitted
    rather than guessed -- an SBOM that invents a license is worse than one
    that admits it does not know, because a reviewer will act on it.
  * `purl` for every component so a scanner can resolve it upstream.
  * The Go module graph is the *build* graph, not the vendored subset; a module
    listed here may not be linked into the final binary.

Model weights are NOT components here. They are third-party artifacts with
their own obligations tracked in docs/THIRD_PARTY_LICENSES.md, and two of them
are BLOCKED. Folding them into a dependency SBOM would let a green SBOM imply
a license clearance that does not exist.

    python3 scripts/generate-sbom.py --out evidence/state/sbom.json
"""
import argparse, json, subprocess, sys, hashlib, os

def go_modules(root):
    out = subprocess.run(["go", "list", "-m", "-json", "all"], cwd=os.path.join(root, "control"),
                         capture_output=True, text=True)
    if out.returncode != 0:
        return []
    mods, dec, i, s = [], json.JSONDecoder(), 0, out.stdout
    while i < len(s):
        while i < len(s) and s[i].isspace():
            i += 1
        if i >= len(s):
            break
        o, i = dec.raw_decode(s, i)
        mods.append(o)
    comps = []
    for m in mods:
        if not m.get("Version"):
            continue  # the main module has no version; it is the subject, not a dependency
        comps.append({
            "type": "library", "name": m["Path"], "version": m["Version"],
            "purl": f"pkg:golang/{m['Path']}@{m['Version']}",
            "properties": [{"name": "merc:ecosystem", "value": "go"}],
        })
    return comps

def rust_packages(root):
    out = subprocess.run(["cargo", "metadata", "--format-version", "1", "--offline"],
                         cwd=os.path.join(root, "agent"), capture_output=True, text=True)
    if out.returncode != 0:
        return []
    comps = []
    for p in json.loads(out.stdout).get("packages", []):
        c = {"type": "library", "name": p["name"], "version": p["version"],
             "purl": f"pkg:cargo/{p['name']}@{p['version']}",
             "properties": [{"name": "merc:ecosystem", "value": "cargo"}]}
        if p.get("license"):
            c["licenses"] = [{"expression": p["license"]}]
        comps.append(c)
    return comps

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--root", default=os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    a = ap.parse_args()
    comps = go_modules(a.root) + rust_packages(a.root)
    head = subprocess.run(["git", "-C", a.root, "rev-parse", "HEAD"],
                          capture_output=True, text=True).stdout.strip()
    undeclared = [c["name"] for c in comps if "licenses" not in c]
    doc = {
        "bomFormat": "CycloneDX", "specVersion": "1.5", "version": 1,
        "metadata": {
            "component": {"type": "application", "name": "merc", "version": head or "unknown"},
            "properties": [
                {"name": "merc:source_commit", "value": head},
                {"name": "merc:generator", "value": "scripts/generate-sbom.py"},
            ],
        },
        "components": comps,
        "merc_honesty": {
            "declared_licenses_only": True,
            "components_without_declared_license": len(undeclared),
            "why": "Go module metadata carries no license field, so those components have no "
                   "licenses entry rather than a guessed one. Absence here means undeclared, "
                   "not permissive.",
            "model_weights_excluded": "Catalogue model weights are third-party artifacts with "
                   "their own obligations in docs/THIRD_PARTY_LICENSES.md, where two rows are "
                   "BLOCKED. They are excluded so a complete SBOM cannot be read as a license "
                   "clearance it does not grant.",
            "graph_scope": "go list -m all is the build graph, not the linked subset.",
        },
    }
    body = json.dumps(doc, indent=2, sort_keys=False)
    with open(a.out, "w") as f:
        f.write(body + "\n")
    print(f"sbom: {len(comps)} components -> {a.out}")
    print(f"sbom: {len(undeclared)} components without a declared license (recorded, not guessed)")
    print("sbom: sha256", hashlib.sha256(body.encode()).hexdigest())

if __name__ == "__main__":
    sys.exit(main())
