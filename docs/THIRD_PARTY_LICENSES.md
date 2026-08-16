# Third-Party License and Distribution Register

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.

- Status: **INCOMPLETE / RELEASE BLOCKING**
- Review basis: regenerated against the current tree; model/font/asset rows
  remain blocked pending counsel. Software-graph licenses are generated
  separately in `docs/LICENSE_INVENTORY.md` and
  `docs/DEPENDENCY_LICENSE_REPORT.md`.

This register records what can be established from tracked source and primary
upstream declarations. It does not approve a license, infer ownership from a
Git commit, or claim that a mutable downloaded artifact is the artifact
reviewed here.

The software dependency graph (Go, Cargo, Python SDK, TypeScript SDK) is
**generated**, not hand-written. Run `python3 scripts/generate-license-inventory.py`.
A hand-edited list of crates is worthless. This file keeps the model, font,
and visual-asset register that the generator deliberately excludes.

## Distribution-wide blockers

1. The root `LICENSE` states a component-by-component split (Apache-2.0 agent
   and clients; proprietary control plane) and says the wording is pending
   counsel. That is a statement of intent, not an approved project license.
2. Llama and MiniLM pin/hash enforcement exists in the agent, but a final
   clean candidate, passing release-bound receipt, and counsel-approved
   license bundle do not yet exist.
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
| Merc fixed media transcode contract | `ffmpeg-transcode-v1`, fixed Merc-owned control/agent contract | `docs/ARCHITECTURE.md § "Merc media-transcode contract"`; FFmpeg/libx264 are invoked only through the pinned local binary contract | Contract bytes are Merc-owned; exact FFmpeg/libx264 binary notices and distribution terms remain separately tracked | **APPROVED_INTERNAL_CONTRACT**: no remote code or third-party model weights are fetched; public codec/legal activation remains a live authority residual |
| Merc fixed scene rendering contract | `svg-scene-render-v1`, fixed Merc-owned closed-scene CPU rasteriser | `docs/ARCHITECTURE.md § "Bounded media rendering contract"`; the runtime is the tracked deterministic rasteriser and accepts no remote model or prompt | Contract bytes and rasteriser are Merc-owned; any future prompt-to-image model requires a separate licence/provenance review | **APPROVED_INTERNAL_CONTRACT**: no remote code or third-party model weights are fetched; prompt-to-image activation is not enabled by this contract |

Primary sources:

- <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/LICENSE>
- <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/USE_POLICY.md>
- <https://huggingface.co/unsloth/Llama-3.2-1B-Instruct-GGUF>
- <https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2>

## Font register

The tracked `web/logo/Geist-VariableFont_wght.ttf` and
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

The backend-alpha software graph is generated by
`scripts/generate-license-inventory.py` into:

- `docs/LICENSE_INVENTORY.md`
- `docs/DEPENDENCY_LICENSE_REPORT.md`
- `docs/generated/license-inventory.json`

That generator reads `control/go.mod` / `go.sum`, `agent/Cargo.lock` plus
declared crate licenses, and the Python/TypeScript SDK manifests. It does
not guess Go licenses: it reads the LICENSE file from the exact module zip
on `proxy.golang.org`. A generated row is not a clearance.

For every **release** artifact (public or live-money), still:

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
