# ComputExchange — Condensation Status

<!-- CLAIM-SCOPE: internal-engineering-non-authoritative -->

_The real target (System Reduction Plan): GLOBAL_OWNED_LOC <= 100,000 by net
DELETION of maintained first-party source. Relocation gets zero credit._

## Wave 2 — reconstruct/event-horizon (off main `60e20df`)

Aggressive, behavior-changing capability deletion + structural reconstruction.
Measured by `cx audit codebase` (`hydrated_owned_LOC`).

| checkpoint | GLOBAL_OWNED_LOC | net | how |
|---|--:|--:|---|
| wave-2 base | ~212,701 | — | off main 60e20df |
| CP1a `4216fc0` | ~197,966 | — | delete macapp Swift app (keep seatbelt control); vLLM patch distill |
| CP1b `383b5c8` | 168,593 | -29.4k gross | delete speculative-decode + render-spec-preview capability whole |
| CP1c `8f5fb57` | 164,421 | -4,172 | delete render/ site-asset build pipeline (assets stay committed) |
| **CP1 `72f5f51`** | **156,457** | **-7,964** | collapse internal engineering narrative docs → documentation 3,607 |

**Checkpoint 1 (<= 160,000): DONE at 156,457.** Green at each: agent build /
clippy -D warnings / cargo test / `--no-default-features`; control go build/vet/test;
`cx prove`; `validate_claims` PASS; `prove-local` 558 pass / 0 fail (CP1b).

**Descent to 125k (structural reconstruction, in progress):** control Go lifecycle
+ worker unification, store transaction consolidation, Rust consolidation, test
densification — behavior-preserving, integration-matrix verified per change.

## Global descent (owned = tracked LOC minus unmodified vendored upstream)

| checkpoint | GLOBAL_OWNED_LOC | net removed | how |
|---|--:|--:|---|
| baseline `96a4890` | ~352,583 | - | starting owned |
| R1 | 256,593 | -95,990 | delete 163 spec-lab research/cloud drivers (condense to receipt) |
| R2 | 231,683 | -24,910 | delete historical doc + render narrative (non-claim-bound) |
| R3a | 225,121 | -6,562 | delete render one-offs + asset generators; untrack regenerable census dumps |
| R3b | 205,984 | -19,106 | delete render/panel loop verdicts + render research artifacts |
| R3c | 206,361 | +377 | restore cx_agent_render_preview_driver.py (live Rust dep the matrix caught) |
| E1-E2 | 206,385 | ~0 | merge cli+control -> ONE cx module + ONE binary (structural; matrix 573/0/0) |
| C-flip | 205,470 | -915 | port five-by-five -> cx prove; flip registry/CI/prove-local; delete 6 proof-python |
| **now** | **~205,464** | **-147,119 (-41.7%)** | all deletion + one cx binary + proof orchestration in Go |

Validated by the native prove-local integration matrix (throwaway Postgres + MinIO):
**559 Go checks PASS + source-stability PASS + `cargo test` 278/0**. Python 123,741
-> 24,225. documentation 35,387 -> ~12,000.

**Remaining to 100k (~106k) is product/test/proof reconstruction, not deletion:**
control Go 62k -> 25k, agent Rust 44k -> 30k, tests 41k -> 20k, proof-python ~9k -> 0
(port to Go + flip CI/registry), proof JSON fixtures. This needs the merged single
`cx` module (Phase E), test-density refactor preserving all security/payment/lifecycle
assertions, and iterative CI. The integration matrix runs natively here, so it is
scope-bound (multi-cycle reconstruction), not infra-blocked.

- **Base:** `origin/main` @ `96a4890` (local `main` d9bff10 is 6 commits behind; origin/main is authoritative)
- **Branch / worktree:** `condense/black-hole` in `computexchange-condense/` (isolated; the dirty `codex/spec-decode-inference` checkout with 139 WIP files is NOT touched)
- **No-touch boundary:** [`census/LIVE_BOUND_NO_TOUCH.json`](census/LIVE_BOUND_NO_TOUCH.json)
- **Headline measurement:** `make audit` → `cx audit codebase` (retires the old `make loc`)

## Phase A — census + no-touch boundary — DONE

Authoritative census, deterministic per commit, produced by the new Go subcommand
`cx audit codebase` (in `cli/`). Regenerate anytime with `make audit`.

### Where the mass is (tracked, owned, at `96a4890`)

| measure | LOC | vs target |
|---|--:|---|
| total tracked (text) | 445,524 | — |
| **kernel** (owned prod runtime) | **89,657** | target 20–25k → **~3.6× over** |
| active product | 135,937 | target 30–40k → ~3.4× over |
| active repository | 184,345 | target 50–65k → ~3× over |
| hydrated owned (excl vendored) | 360,362 | — |
| vendored (candle + three.js) | 85,162 | account separately |
| historical / pack-eligible | 181,504 | relocate to packs |
| **python** | **123,741** | target production=0 |

### Python reclamation ([`census/PYTHON_RECLAMATION.json`](census/PYTHON_RECLAMATION.json)) — 0 unknown

| plan | files | LOC | destination |
|---|--:|--:|---|
| archive_research | 118 | 53,478 | render-lab / docs-archive packs |
| retain_blender_pack | 46 | 38,024 | `computexchange-render-lab` (bpy only) |
| rewrite_rust | 57 | 23,580 | spec-engine (speculative core) |
| rewrite_go | 22 | 7,902 | `cx prove` / `cx audit` (evidence authority) |
| retain_sdk | 3 | 757 | public Python SDK (first-class, kept) |

**No production/proof/speculative Python remains after reclamation.** Only the
dependency-free SDK (757 LOC) and Blender `bpy` scripts (in a pack) survive.

### Biggest physical targets (safe, non-runtime)

- `scripts/spec-lab/` — 177 Python files, ~106k LOC of render/spec research → `render-lab` pack
- `render/` + `renderer/` — 402 files, 56.4 MB (PDF rack manuals, `.onnx`/`.pt` denoiser, `*@3x.png` iteration renders) → `render-lab` pack
- `docs/` archive-class — 114 files, 21.9 MB → `docs-archive` pack
- `web/assets/site/vendor/three.module.js` — 53k LOC vendored (account separately)

## Phase B — non-runtime physical reduction — IN PROGRESS

Safe reductions only (no hot-path changes; Go build stays green; live-bound hashes
unchanged). See [`CONDENSATION_LEDGER.jsonl`](CONDENSATION_LEDGER.jsonl) for per-commit metrics
and [`PACK_MANIFESTS.json`](PACK_MANIFESTS.json) for pack definitions.

**B1 — render-lab binary artifacts → pack (done).** Relocated 67 force-added binary
files (preview/verify PNGs, reference rack PDFs/JPGs, the `.glb` master) — 50.0 MB —
into `computexchange-render-lab`. **Tracked checkout 108.0 MB → 58.3 MB (−46%).**
Verified: web ships `web/assets/site/*` independently; no build/CI/test reference to
the moved files; proof-bound `render/handoff/**` kept. Go build/test green. Also fixed
a `make audit --out` path bug. Files preserved in history + regenerable via builders.

**B2 — benchmark research outputs → pack (done).** Relocated 78 files (21.0 MB):
`cx_denoiser.{pt,onnx}` model artifacts + large spec-lab `.jsonl` ledgers under
`docs/speed-lane-reports/` and `docs/bench-local-reports/` into
`computexchange-benchmarks`. Verified not in the green Makefile/CI test path and
not proof/claim-bound. **Tracked checkout now 37.2 MB (108.0 → 37.2, −66% total).**

_Next B candidates (not yet done):_ docs-archive pack is constrained — most
top-level `docs/*.md` are **claim-bound** (proof evidence in `claim-policy.json`),
so only non-claim-bound archive docs can move. `scripts/spec-lab/` stays until
Phase D (live CI/test dependency).

## Phase C — Go proof/evidence authority — IN PROGRESS (Go authority built + proven)

The one Go evidence core (`cli/evidence.go`: canonical JSON, atomic writes, framed
hashing, source fingerprint) now backs new `cx` subcommands, each verified at parity
against the Python it replaces:

- **`cx source-id`** = `scripts/source_fingerprint.py`: full JSON + every field
  (`source_sha256`, `status_sha256`) byte-identical on the live tree.
- **`cx verify`** = `scripts/verify_proof_ledger.py` (the primitive 14 registry gate
  commands depend on): accept-case semantic parity incl. the `ledger_sha256` byte
  hash; six adversarial reject cases byte-identical in FAIL message + exit code.

These are additive: the Python and CI are untouched, so the `proof-contracts` job
stays green. Go unit tests cover both (`evidence_test.go`, `prove_test.go`).

### Why the Python is not yet deleted (the flip is Phase-E-gated)

Removing the Python safely requires two things this branch cannot yet satisfy without
risking a red checkpoint:

1. An installed single `cx` binary on PATH. The registry (`proof/5x5-gates.json`, 14
   gates) and `scripts/prove-local.sh` invoke `python3 scripts/<x>.py` directly.
   Flipping them to `cx <sub>` needs one installed `cx`, which is the Phase E
   deliverable (merge `cli`+`control` into one `cx`).
2. Live verification. Those 14 gates run inside `make prove-local` (live Postgres +
   Metal) and the CI `proof-contracts` job (Ubuntu runner). Neither is reproducible in
   this worktree, so flipping them blind would break "green at every checkpoint".

Exact flip (do when `cx` is installed + against live prove-local/CI): rewrite the 14
`cx verify` gate commands and the `prove-local.sh`
source-fingerprint calls to `cx verify` / `cx source-id`; port `five-by-five.py` to
`cx prove` and `runtime_matrix.py`/`api_contract.py` (`--check`) to `cx runtime check`
/ `cx contract check`; swap the CI `proof-contracts` steps to `go test ./cli` + the
`cx` binary; delete the replaced Python + migrate remaining assertions to Go tests.

Remaining Python proof scripts to port before deletion: `five-by-five` (313),
`runtime_matrix` (948), `api_contract` (1419), `release_surface`, `performance_proof`
(854), `fleet_proof` (1360), `validate_claims` (223) + their tests.

## Remaining (D to H) — gates

- **D** Rust speculation authority: fold the 57 `rewrite_rust` spec-lab files into
  `spec-engine`; one receipt type; delete Python core. Gate: cross-language golden
  fixtures + full `cargo test` + the CI spec lane; `scripts/spec-lab/` is a live CI
  dep until then.
- **E** Go product collapse: **steps 1-2 DONE** (matrix-green). `cli/`+`control/` are
  now ONE Go module (`computeexchange/control`) producing ONE `cx` binary: `cx serve`
  = control plane; `cx submit/status/.../version` + `cx audit/source-id/verify` dispatch
  at the top of `main()` before the DB gate (verified to run without DATABASE_URL).
  Designed by a 6-agent map+design+adversarial-review workflow; validated by the native
  prove-local matrix (573 pass / 0 skip / 0 fail). _Remaining E:_ step 3 wire-type dedup
  (deferred: inert byte change for ~40 LOC), then one lifecycle engine + one worker
  supervisor (the large internal control dedup, 62k -> 25k, needs the integration matrix
  per change). Now unblocks the Phase C Python deletion (installed `cx` for the registry
  flip).
- **F** Rust agent collapse: one Cargo workspace (`agent` + `spec-engine` +
  `token-spec-poc` merged); kill shadow impls. Gate: full `cargo build/test` across
  the candle/Metal feature graph.
- **G** interface/product split; thin SDK; pack Swift/render.
- **H** 25k-kernel attempt (only after E/F green).

These phases are each gated on infrastructure absent from this environment (live
Postgres/MinIO/Metal/CUDA, a CI runner, an installed `cx`) or on large cross-language
parity efforts that cannot be adversarially verified here. Per the black-hole rule
"no checkpoint is green solely because LOC fell," they are left staged with exact
next-steps rather than pushed as unverified edits.

## Commands

```
make audit          # regenerate census/ (cx audit codebase)
make build test     # Go + Rust build & test
make prove-local    # source-bound local proof matrix (needs docker/native pg+minio)
```

## Rollback

Every checkpoint is an independent commit on `condense/black-hole`. Base release =
`origin/main` @ `96a4890`. To restore: `git reset --hard 96a4890` in the condense worktree.
Nothing is merged to `main` until the previous release is restorable per the above.
