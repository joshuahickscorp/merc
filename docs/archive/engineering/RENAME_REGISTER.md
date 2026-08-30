# Historical Merc rename register

The repository rebrand from ComputeXchange to Merc is complete for the active
source tree. This compact register preserves the few names that are deliberate
compatibility or provenance boundaries; it is not a live rename queue.

## Active result

- The repository, Go module, Python package, TypeScript package, control-plane
  binary, supplier agent, web copy, deployment manifests, and public links use
  the Merc namespace.
- Historical receipt fields, hashes, external asset-pack names, and installed
  supplier paths remain unchanged when they are part of a replay or digest
  contract.
- The rename audit is scoped to tracked files and fails on new unclassified
  residue. The exception list in `ops/scripts/rename-residue-audit.py` is the
  executable classification; this document records why the exceptions exist.

No rename record changes a receipt, a hash input, a deployed data path, or a
wire-compatible legacy replay without an explicit migration.
