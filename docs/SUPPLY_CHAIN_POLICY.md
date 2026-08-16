# Supply-chain release policy

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.

Candidate and rollback control images are built from exact commits, published
by digest, scanned, assigned SPDX SBOMs, signed with GitHub OIDC, and attested
with the same SBOM plus a signed SLSA provenance predicate identifying the
repository, ref, exact commit, Dockerfile, builder workflow, and invocation.
The publication workflow removes and re-pulls the candidate before verifying
the signature, SBOM attestation, and provenance attestation. Registry evidence
retains the candidate and prior digest together.

The image vulnerability gate fails for a fixable **CRITICAL** vulnerability.
HIGH and CRITICAL findings, including unfixed findings, remain in the uploaded
JSON reports for review. A security reviewer may tighten this policy but may
not waive it by modifying a release run after the image is built.

Release tags are not created by the candidate workflow. Active GitHub ruleset
`19184161` covers `v*` and `rc*` tags and restricts tag creation, update, and
deletion to the repository-owner release authority. A tag must point to the
reviewed commit and its digest verification receipt. The ruleset is rechecked
through the authenticated GitHub API before a release decision; this candidate
workflow does not create an RC tag.

The retained Rust `paste` dependency is documented in
`ops/rust-paste-audit.json`. It has no known vulnerability in the recorded
RustSec snapshot, but it is unmaintained and remains a tracked removal item.
