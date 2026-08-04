# PLAN_300K.md — 1,303,077 → under 300,000 tracked lines

Target: **≤300,000 tracked lines.** Hit by separating the **active source tree**
from the **evidence archive** — a boundary Merc does not have today. Zero product
code deleted. Zero tests deleted. Zero proofs deleted. Zero receipts rewritten.

---

## 1. The arithmetic

```
1,303,077   today

 −826,236   evidence/perf/*.json  →  84 git-lfs pointers (826,488 → 252 lines)
 −122,368   control/evidence/perf orphan  →  deleted (verified uncited)
 − 59,798   .tools/*  →  lfs + checksum-pinned bootstrap (59,807 → 9 lines)
─────────
  294,675   ✓ under 300,000
```

Three piles. One deletion. Nothing else required.

Remaining margin before touching a line of Go: **5,325**. Docs consolidation
(68 → 19, ≈ −8,000) and dead code (≈ −180) are then headroom, not necessity.

## 2. Why this is a refactor and not a diet

Today `evidence/` is 64.5% of the repository and lives in the same tree, in the
same form, under the same scans as executable authority. Every clone, every
`source-id`, every checkpoint digest, every Docker context and every full-tree
hash pays for the archive as if it were live code.

Measured: **true executable authority is ~15 files / ~2.7k lines.** The
gateway-parity monsters are **~96% `raw_samples`** — re-derivation substrate.

The refactor introduces the missing tier:

```
tier 1  in tree, git-tracked, human-diffable
        receipt identity, binding status, digests, gate verdicts, summaries
        → every receipt keeps a file and a path

tier 2  content-addressed archive (git-lfs)
        the sample bodies
        → authenticity is the LFS oid == sha256(content), native and cryptographic
```

Nothing is deleted. The bodies remain in the repository, retrievable by
`git lfs pull`, addressed by their own hash.

## 3. Why git-lfs and not an invented CAS

D4 required an archive to answer one question or be rejected: **how does a
verifier holding only the repo prove an archived payload is authentic?**

git-lfs answers it natively. The tracked pointer *is* the digest:

```
version https://git-lfs.github.com/spec/v1
oid sha256:9c1a…
size 4823914
```

- The oid is committed to git. Tampering with the body fails the oid check.
- `git lfs pull` retrieves it; `git lfs fsck` verifies it.
- No new format, no index file to trust, no bespoke verifier to maintain.
- GitHub supports LFS on private repos — the remote already exists.

Building a custom pack + INDEX would mean writing and maintaining a verifier for
a problem that has a standard, audited solution. That is the "hidden complexity"
the directive forbids.

## 4. The one real trade, decided

A fresh `git clone` without `git lfs pull` no longer contains the sample bodies.
Someone auditing offline must run one extra command.

**Accepted**, because:
- the digest is still committed, so the *claim* is still verifiable from the repo
- `.lfsconfig` sets `lfs.fetchinclude` so a normal clone pulls tier 1 and can opt
  into tier 2
- CI pulls LFS; the gates see exactly what they see today

This is stated as a design decision, not discovered later.

## 5. What must not move

Determined by citation, not by size:

| stays in tree | why |
|---|---|
| the 13 paths in `evidence-manifest.json` | embedded in the control binary |
| `evidence/benchmarks/*` | `Dockerfile.control` COPYs them into the image |
| receipts cited by `control/pricing.go` | live repricing authority |
| every `*.binding.json` sidecar | binding contract; payload may be LFS, sidecar stays plain |
| `ops/asset-provenance.json` targets | path + sha256 governance rows |
| anything under `evidence/checkpoint/` | already gitignored |

**Both sides of every `-latest`/timestamped pair stay.** They are byte-identical
and **independently cited** — `board-power-a40-latest` by 3 files, its twin by 1.
Byte-identity was never the test; citation is. No alias is collapsed.

`quiet.json` stays: it uniquely records `missing_identity_fields`, a negative
result.

## 6. `raw_samples` is inside `IDENTITY_FIELDS`

This is the one place the refactor touches a contract, and it is handled by
**supersession, never rewriting**.

The binding identity today hashes the inline `raw_samples` array. Under tier 2
the array is not inline.

**Historical receipts are not touched.** They move to LFS as whole files,
byte-identical, oid == sha256 of the exact bytes that exist today. Their binding
identity is unchanged because their bytes are unchanged.

**New receipts** written after this change use `raw_samples_sha256` in place of
the inline array, and the writer emits a superseding receipt recording the format
change. `scripts/lib/evidence_binding.py` gains one branch: if `raw_samples` is
absent and `raw_samples_sha256` is present, hash the digest. Both forms validate.

No old receipt is edited. No old digest changes. The format change is itself
evidence.

## 7. Phases

Each phase is one commit in `.worktrees/teardown`, independently revertible.

| # | Phase | Δlines | Gate |
|---|---|---:|---|
| **L1** | Install LFS substrate: `.gitattributes`, `.lfsconfig`, CI `lfs pull`. **Move nothing.** | 0 | `git lfs env` clean; every existing gate green |
| **L2** | Teach `validate-evidence-binding.py` to resolve LFS pointers; dual-form `raw_samples` / `raw_samples_sha256` in `evidence_binding.py` | 0 | binding census byte-identical to today |
| **L3** | Delete `control/evidence/perf/…runpod-vllm-20260803T175830Z.json` | **−122,368** | zero citations resolve to `control/evidence/` |
| **L4** | Migrate 84 `evidence/perf/*.json` to LFS | **−826,236** | every oid == sha256 of the pre-move bytes; binding census unchanged |
| **L5** | `.tools/runpodctl` → LFS; `rp-key` → untracked + checksum-pinned bootstrap | **−59,798** | bootstrap verifies pinned SHA-256 before exec |
| **L6** | Docs 68 → 19 with the no-loss checker | ≈ −8,000 | checker exits 0 |

**After L5: 294,675 lines. Target met.** L6 is margin.

### Per-phase verification

```
git ls-files | xargs wc -l | tail -1          # the metric
git lfs fsck                                   # every oid resolves and matches
python3 scripts/validate-evidence-binding.py   # census identical to baseline
python3 scripts/validate-claim-surfaces.py
python3 scripts/rename-residue-audit.py        # RESIDUE must stay 0
cd control && go build ./... && go vet ./... && \
  MERC_ALLOW_SKIPPING_DB_TESTS=1 go test ./...
bash scripts/test-release-image-contents.sh    # evidence/benchmarks still in image
```

L4 additionally: for all 84 files, `sha256` before the move == LFS oid after.
That is the proof nothing changed. Paste all 84.

## 8. Effect on the no-degradation gate

| metric | baseline | expected |
|---|---:|---|
| source fingerprint | 0.14 s / 53.9 MB | **~0.06 s** — byte-bound, and 61% of bytes leave |
| checkpoint digest | same shape | proportional improvement |
| docker build context | 9.9 MB | unchanged (already excludes `evidence/perf`) |
| image content | 11,252,151 B | **unchanged** — `evidence/benchmarks` stays plain |
| authorize p50 | 1.717 ms | **unchanged** — no product code touched |
| binaries, startup, microbenches | — | **unchanged** |
| unit tests | 19.70 s | unchanged |

The fingerprint improvement is the point D6 measured: it is byte-bound, and this
removes bytes rather than files.

## 9. What this plan does not claim

- It does not reduce `control` Go LOC. That floor stands at ~152–160k, measured.
- It does not reduce `control` file count. Packing is a separate question and is
  currently **blocked**: ~6,038 LOC of money logic sits in files the guard list
  does not name, so "free file" never meant "money-safe." Re-derive the money
  surface from domain membership before packing anything.
- It does not shrink clone size until the authorized history rewrite runs, which
  is sequenced last, after teardown is merged and tested.

The 300k target is a **tracked-lines** target and this plan hits it by moving
where evidence lives — which is the correct architectural answer to a repository
that is 64.5% archive.
