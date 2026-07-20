# Third-Party License and Distribution Register

- Status: **INCOMPLETE / RELEASE BLOCKING**
- Review basis: `533788d69c0fa06863d8fbcf5b2fd793955c3bbd`

This register records what can be established from tracked source and primary
upstream declarations. It does not approve a license, infer ownership from a
Git commit, or claim that a mutable downloaded artifact is the artifact
reviewed here.

## Distribution-wide blockers

1. There is no owner-approved root project license. `agent/Cargo.toml` declares
   MIT, but no corresponding license text is tracked, and the Python package
   has no license metadata.
2. The review-basis commit did not enforce Llama or MiniLM revisions and file
   hashes. The closure worktree adds enforcement, but a final clean candidate,
   passing tests and release-bound receipt do not yet exist.
3. The full Llama 3.2 agreement, Apache-2.0 text for the reviewed MiniLM
   package, and SIL OFL text tied to the bundled font are not vendored.
4. CI-generated SBOMs must be reviewed for every shipped binary/image and
   extended to models, the site, Mac application and SDK. `NOASSERTION` is not
   an approved license conclusion.
5. Asset creator assignments, source receipts and reference-material review
   are absent from the candidate.

## Model register

| Component | Source selected by code | Upstream declaration | Known obligations | Current conclusion |
|---|---|---|---|---|
| Llama 3.2 1B Instruct GGUF | `unsloth/Llama-3.2-1B-Instruct-GGUF`, file `Llama-3.2-1B-Instruct-Q4_K_M.gguf` | Model page: `llama3.2`; Meta Llama 3.2 Community License and AUP | Agreement copy for applicable availability/distribution, “Built with Llama,” Notice attribution for distributed copies, AUP and applicable-law compliance | **BLOCKED**: worktree pin/hash enforcement is not final-candidate-bound; full agreement copy, reviewed acceptance receipt and policy approval remain absent |
| all-MiniLM-L6-v2 | `sentence-transformers/all-MiniLM-L6-v2`: config, tokenizer and safetensors | Model page: Apache-2.0 | Preserve required license/notices; review model-card/dataset and artifact provenance | **BLOCKED**: worktree pin/hash enforcement is not final-candidate-bound; artifact-bound notice and review remain absent |

Primary sources:

- <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/LICENSE>
- <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/USE_POLICY.md>
- <https://huggingface.co/unsloth/Llama-3.2-1B-Instruct-GGUF>
- <https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2>

## Font register

The tracked `logo/Geist-VariableFont_wght.ttf` and
`web/assets/site/fonts/geist-mono.woff2` identify as Geist-family assets. The
official upstream repository states SIL OFL 1.1 and copyright to Vercel in
collaboration with basement.studio. The WOFF2 appears in repository history as
a subset, but the exact upstream version, source binary, subsetting command and
license copy are not present in the candidate.

Source: <https://github.com/vercel/geist-font/blob/main/LICENSE.txt>

Conclusion: **BLOCKED_VERSION_LINKAGE**. Record the upstream commit and source
hash, reproduce the subset, verify naming restrictions, and ship the full
copyright/license notice before distribution.

## Software dependency process

For every release artifact:

1. Generate lockfile-bound SPDX or CycloneDX SBOMs for the Go control binary,
   Rust agent/Mac application and packaged Python SDK, plus the container image.
2. Produce a license report with declared and concluded licenses, source URL,
   version/checksum, copyright, notice obligations and policy decision.
3. Fail on missing/unknown license, unreviewed copyleft/network-copyleft,
   noncommercial/research-only/custom terms, yanked source, or package not
   represented in the artifact SBOM.
4. Collect required license and Notice files into the distribution and bind
   their hashes into its manifest.
5. Have an accountable owner and counsel approve the exact report. An automated
   scan alone is not approval.

## Visual asset register

`ops/asset-provenance.json` contains file hashes and repository-history facts.
History describes the device imagery as procedurally built from primitives and
without embedded Apple or NVIDIA marks. It also describes comparison to
reference photography. The source render files, reference list/license, creator
identity and IP assignment are not in the candidate. The images must not be
treated as cleared merely because they were committed.

Brand marks, icons, knobs and decorative images likewise require an owner or
creator declaration, source-file receipt, third-party-input declaration,
trademark review and approved license. Screenshots inherit the rights and
obligations of every visible component.

## Approval record required

For each component, record reviewer, organization, review date, exact version
or commit, artifact SHA-256, license identifier and hash, obligations,
distribution surfaces, territory/use restrictions, notice path, exceptions,
expiry/review date and decision. The matching entries in
`ops/legal-review.json` must remain `PENDING` or `BLOCKED` until that evidence
exists.
