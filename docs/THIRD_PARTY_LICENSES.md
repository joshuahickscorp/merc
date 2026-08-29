# Third-Party License and Distribution Register

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.

- Status: **INCOMPLETE / RELEASE BLOCKING**
- Review basis: regenerated against the current tree; model/font/asset rows
  remain blocked pending counsel. The complete software-graph inventory is
  generated in `docs/LICENSE_INVENTORY.md`.

This register records what can be established from tracked source and primary
upstream declarations. It does not approve a license, infer ownership from a
Git commit, or claim that a mutable downloaded artifact is the artifact
reviewed here.

The software dependency graph (Go, Cargo, Python SDK, TypeScript SDK) is
**generated**, not hand-written. Run `python3 ops/scripts/generate-license-inventory.py`.
A hand-edited list of crates is worthless. This file keeps the model, font,
and visual-asset register that the generator deliberately excludes.

## Distribution-wide blockers

1. The root `LICENSE` states a component-by-component split (Apache-2.0 agent
   and clients; proprietary control plane) and says the wording is pending
   counsel. That is a statement of intent, not an approved project license.
2. Llama and MiniLM pin/hash enforcement exists in the agent, but a final
   clean candidate, passing release-bound receipt, and counsel-approved
   license bundle do not yet exist.
3. The Llama 3.2 Community License and AUP were retrieved and hashed on
   2026-08-17 (see the Llama register note below). They are still not
   vendored into the release/notice bundle; `NOTICE` still says the full
   agreement is not vendored. Apache-2.0 text for the reviewed MiniLM
   package and SIL OFL text tied to the bundled font are also not vendored.
4. CI-generated SBOMs must be reviewed for every shipped binary/image and
   extended to models, the site, Mac application and SDK. `NOASSERTION` is not
   an approved license conclusion.
5. Asset creator assignments, source receipts and reference-material review
   are absent from the candidate.

## Model register

| Component | Source selected by code | Upstream declaration | Known obligations | Current conclusion |
|---|---|---|---|---|
| Llama 3.2 1B Instruct GGUF | `unsloth/Llama-3.2-1B-Instruct-GGUF` @ `b69aef112e9f895e6f98d7ae0949f72ff09aa401`, file `Llama-3.2-1B-Instruct-Q4_K_M.gguf` | Unsloth card YAML `license: llama3.2`; Meta Llama 3.2 Community License and AUP (hashed 2026-08-17; see note) | §1.b.i agreement copy + prominent “Built with Llama”; §1.b.iii Notice on distributed copies; §1.b.iv AUP (“use, or allow others to use”); §2 700M-MAU additional terms | **BLOCKED**: upstream grant and HF LFS artifact identity verified 2026-08-17; §1.b / AUP package, acceptance receipt, counsel review and final-candidate binding remain absent. Do not treat as cleared. |
| all-MiniLM-L6-v2 | `sentence-transformers/all-MiniLM-L6-v2`: config, tokenizer and safetensors | Model page: Apache-2.0 | Preserve required license/notices; review model-card/dataset and artifact provenance | **BLOCKED**: worktree pin/hash enforcement is not final-candidate-bound; artifact-bound notice and review remain absent |
| Merc fixed media transcode contract | `ffmpeg-transcode-v1`, fixed Merc-owned src/control/agent contract | `docs/ARCHITECTURE.md § "Merc media-transcode contract"`; FFmpeg/libx264 are invoked only through the pinned local binary contract | Contract bytes are Merc-owned; exact FFmpeg/libx264 binary notices and distribution terms remain separately tracked | **APPROVED_INTERNAL_CONTRACT**: no remote code or third-party model weights are fetched; public codec/legal activation remains a live authority residual |
| Merc fixed scene rendering contract | `svg-scene-render-v1`, fixed Merc-owned closed-scene CPU rasteriser | `docs/ARCHITECTURE.md § "Bounded media rendering contract"`; the runtime is the tracked deterministic rasteriser and accepts no remote model or prompt | Contract bytes and rasteriser are Merc-owned; any future prompt-to-image model requires a separate licence/provenance review | **APPROVED_INTERNAL_CONTRACT**: no remote code or third-party model weights are fetched; prompt-to-image activation is not enabled by this contract |

Primary sources:

- <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/LICENSE>
- <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/USE_POLICY.md>
- <https://huggingface.co/unsloth/Llama-3.2-1B-Instruct-GGUF>
- <https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2>

### Llama 3.2 1B Instruct GGUF — verified 2026-08-17, remains BLOCKED

This note records what was actually retrieved. It is not counsel review
and it does not clear the row. `make license-register` must keep failing
until a named licensing authority changes the conclusion cell, or until
the pricing owner removes the cell from `repricingBenchmarks`.

**Artifact (Hugging Face API, revision pinned by this repo).**
`GET https://huggingface.co/api/models/unsloth/Llama-3.2-1B-Instruct-GGUF`
returned `sha=b69aef112e9f895e6f98d7ae0949f72ff09aa401`, `gated=false`,
card `license: llama3.2`, `base_model: meta-llama/Llama-3.2-1B-Instruct`.
The tree at that revision lists `Llama-3.2-1B-Instruct-Q4_K_M.gguf` with
LFS `oid=3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1`
and `size=807694368`. Those two numbers match
`src/control/runtime-authority.json`, `src/agent/src/models.rs` (`INFER`),
`ops/model-provenance.json`, and the sole
`repricingBenchmarks` digest in `src/control/pricing.go`. The same tree has
no `LICENSE` file (siblings are GGUF files, `README.md`, `config.json`,
`imatrix_unsloth.dat`). The README at that revision hashes to
`sha256:0735a2a005c6ab7788f40951a29a807e439061d30ff4abe8e582ee469c00d8bb`
and declares `license: llama3.2`. Official
`meta-llama/Llama-3.2-1B-Instruct` is gated (contact-share clickwrap);
`GET …/raw/main/LICENSE` on that repo returned HTTP 401.

**License text (GitHub API, 2026-08-17).**
`GET https://api.github.com/repos/meta-llama/llama-models/contents/models/llama3_2/LICENSE`
decoded to 7669 bytes,
`git blob sha 83f13c3a5da0106959dd8cbee0eecfd4e6cef3d2`,
`sha256:8cc15535a8a34b41888f644b339a1a9eb428af793a4f5e24df58a3e5b1487d74`.
Last path commit `8d29d93fa5700a60532e0061a02ffa89d0acd3fc`
(2024-09-25T17:29:42Z). A parallel fetch of
`models/llama3_2/USE_POLICY.md` decoded to 6021 bytes,
`git blob sha ac3c5f21b9779e3da0677d6d3c587778fe3a331e`,
`sha256:40e2777d7faa6beaf98400654170f414d8ab29b921b5163ad4ea0a1d39894201`,
same path commit. These hashes are of the upstream blobs. They are not
an in-repo vendored copy.

Quoted grant and redistribution conditions (same LICENSE bytes):

> By clicking “I Accept” below or by using or distributing any portion
> or element of the Llama Materials, you agree to be bound by this
> Agreement.

> 1.a Grant of Rights. You are granted a non-exclusive, worldwide,
> non-transferable and royalty-free limited license under Meta’s
> intellectual property or other rights owned by Meta embodied in the
> Llama Materials to use, reproduce, distribute, copy, create derivative
> works of, and make modifications to the Llama Materials.

> 1.b.i If you distribute or make available the Llama Materials (or any
> derivative works thereof), or a product or service (including another
> AI model) that contains any of them, you shall (A) provide a copy of
> this Agreement with any such Llama Materials; and (B) prominently
> display “Built with Llama” on a related website, user interface,
> blogpost, about page, or product documentation.

> 1.b.iii You must retain in all copies of the Llama Materials that you
> distribute the following attribution notice within a “Notice” text
> file distributed as a part of such copies: “Llama 3.2 is licensed
> under the Llama 3.2 Community License, Copyright © Meta Platforms,
> Inc. All Rights Reserved.”

> 1.b.iv Your use of the Llama Materials must comply with applicable
> laws and regulations (including trade compliance laws and regulations)
> and adhere to the Acceptable Use Policy for the Llama Materials
> (available at https://www.llama.com/llama3_2/use-policy), which is
> hereby incorporated by reference into this Agreement.

> 2. Additional Commercial Terms. If, on the Llama 3.2 version release
> date, the monthly active users of the products or services made
> available by or for Licensee, or Licensee’s affiliates, is greater
> than 700 million monthly active users in the preceding calendar
> month, you must request a license from Meta…

Quoted AUP preface (same USE_POLICY.md bytes):

> You agree you will not use, or allow others to use, Llama 3.2 to:
> [prohibited categories 1–5, including unlawful content, CBRNE /
> weapons / critical infrastructure planning, deception, failure to
> disclose known dangers of the AI system, and interaction with tools
> designed to generate unlawful content].

The AUP’s EU restriction on **multimodal** Llama 3.2 models was read.
Meta’s model card for Llama 3.2 1B Instruct describes a **text-only**
1B instruct model. This note does not decide whether that restriction
attaches; counsel must.

**Redistribution position (facts, not a legal opinion).**

- Quantizing Meta weights to GGUF is a derivative-work path the
  Community License §1.a expressly grants. Unsloth’s card declares
  `license: llama3.2` and `base_model: meta-llama/Llama-3.2-1B-Instruct`.
- Merc’s agent, when downloads are enabled, fetches that GGUF from
  Hugging Face onto the supplier host (`huggingface.co:443`,
  `~/.cache/huggingface`). That is distribution of a derivative onto
  those machines, so §1.b.i and §1.b.iii are in scope for those copies.
- Offering `batch_infer` on that model is making available a product or
  service that contains Llama Materials, so §1.b.i is also in scope
  even if the weights never leave the supplier host.
- Binding occurs by use or distribution, not only by Meta’s gated
  clickwrap. This repository still has `acceptance_receipt: null`.
- A marketplace that runs buyer prompts is “allowing others to use”
  Llama 3.2. The in-repo AUP (`docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`)
  is `DRAFT_PENDING_COUNSEL_AND_TRUST_SAFETY`.
- “Built with Llama” appears in `NOTICE` and draft
  `docs/CANARY_TERMS.md`. Prominence on every related website / UI /
  about page / product documentation, and a Notice file shipped **with
  each distributed GGUF copy**, are not verified.
- Section 2 (700 million MAU on the 2024-09-25 release date) is a
  Meta additional-commercial-terms trigger. No in-repo measurement of
  Licensee MAU on that date exists here. Counsel must decide whether
  it applies. This note does not assert that it does or does not.

**Priced cell consequence.**

The only row in `src/control/pricing.go` `repricingBenchmarks` is:

| Field | Value |
|---|---|
| ModelID | `llama-3.2-1b-instruct-q4` |
| JobType | `batch_infer` |
| RuntimeCellID | `candle-metal-llama1-infer` |
| RuntimeProfileID / rev | `candle_metal` / `r9` |
| ModelArtifactDigest | `3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1` |
| SourceCitation | `evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r6.json#batch_infer` |

`all-minilm-l6-v2`, `ffmpeg-transcode-v1` and `svg-scene-render-v1` sit
in `unpricedThroughputUntilBound` and do not trip
`ops/scripts/validate-license-register.py`.

Must that price be withdrawn for **backend alpha**?

- **Not as an `ALPHA_BLOCKER`.** `ops/scripts/validate-readiness.py`
  `KNOWN_P1_IDS` does not include a license-register item. The Llama
  row staying BLOCKED does not, by itself, change Level B 87/100 or
  backend alpha 85/91.
- **Yes, before `make license-register` can pass** without a genuine
  clearance of this row. The check is: every `repricingBenchmarks`
  model has a non-BLOCKED register row. The Makefile says the fail is
  expected until counsel clears the register and must not be silenced
  by editing the register. `docs/archive/engineering/PROGRAMME.md` §8 says the same.
- **Unpricing while still serving the GGUF is not a license fix.**
  §1.b.i attaches to making the service available, not only to putting
  a number on the catalogue. Moving the cell to
  `unpricedThroughputUntilBound` would green the make target and leave
  the distribution/AUP duties in place.
- This lane cannot edit `src/control/pricing.go`. The pricing owner (not
  this register) is the only engineering path to a green
  `license-register` short of counsel changing **BLOCKED**.

**What a human must do to clear the row.**

Owner: product owner + license counsel (see `ops/legal-review.json`
`LICENSE-001` and `approvals.license`, both PENDING).

1. Name the legal entity that is Licensee. Record an acceptance /
   download receipt (who accepted, when, on behalf of which entity).
   Unsloth being ungated does not replace that receipt.
2. Vendor the Agreement and AUP (the hashed blobs above, or a later
   counsel-selected copy) into the release/notice bundle. Update
   `NOTICE`, which today still says the full agreement is not vendored.
3. Put a copy of the Agreement with every distributed GGUF (supplier
   cache / agent package) and ship the §1.b.iii Notice sentence as part
   of those copies.
4. Verify prominent “Built with Llama” on every related website, UI,
   about page and product documentation that makes the service
   available. `NOTICE` plus a draft canary-terms line is not that
   verification.
5. Implement and counsel-approve AUP enforcement for buyer prompts
   (“allow others to use”). Approve
   `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`.
6. Counsel decision on §2 (700M MAU as of 2024-09-25) and on the AUP
   EU multimodal clause as applied to this text-only 1B instruct GGUF.
7. Bind the pin/hash work to a final candidate receipt
   (`artifact_verification_status` is still
   `IMPLEMENTED_IN_CLOSURE_WORKTREE_PENDING_FINAL_CANDIDATE_TEST_AND_BINDING`).
8. Only then may `ops/legal-review.json` `approvals.license`,
   `ops/model-provenance.json` `review_status`, and this conclusion
   cell change. `ops/scripts/validate-governance.py` currently **requires**
   `review_status == BLOCKED` and provenance status
   `BLOCKED_LICENSE_AND_FINAL_CANDIDATE_BINDING`; those checks have to
   move in the same change as any clearance.

## Font register

The tracked `clients/web/logo/Geist-VariableFont_wght.ttf` and
`clients/web/assets/site/fonts/geist-mono.woff2` identify as Geist-family assets. The
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
`ops/scripts/generate-license-inventory.py` into:

- `docs/LICENSE_INVENTORY.md`
- `docs/generated/license-inventory.json`

That generator reads `src/control/go.mod` / `go.sum`, `src/agent/Cargo.lock` plus
declared crate licenses, and the Python/TypeScript SDK manifests. It does
not guess Go licenses: it reads the LICENSE file from the exact module zip
on `proxy.golang.org`. A generated row is not a clearance.

For every **release** artifact (public or live-money), still:

1. Generate lockfile-bound SPDX or CycloneDX SBOMs for the Go control binary,
   Rust src/agent/Mac application and packaged Python SDK, plus the container image.
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
