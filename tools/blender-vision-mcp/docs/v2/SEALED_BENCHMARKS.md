# V2 Sealed Benchmark Framework

Reusable isolation harness for VisionMCP V2 acceptance targets. Generalises the
NOCTURNE/ONE sealing pattern (`benchmarks/nocturne_one/`,
`blender_vision.benchmarks.nocturne`) without changing nocturne behaviour.

Package surface: `blender_vision.benchmarks.sealed`.

## Roles

| Role | Owns | Builder may see |
| --- | --- | --- |
| **Oracle** | Source scene, hidden cameras, hidden measurements, hidden materials, canary | Nothing under `oracle/hidden/` |
| **Builder** | Working directory + declared builder inputs only | Packet files listed in the manifest |
| **Evaluator** | Gates, thresholds, holdout scoring | Frozen tree digest in the contract |

A derived claim is never stronger than its weakest input (`derive()` on the
contract lineage). Inferred or never-observed structure is never labelled
`OBSERVED`. When evidence is underdetermined the target emits a portfolio /
hypothesis set requirement in its thresholds, not a single forced answer.

## Sealing discipline

1. **Freeze the evaluator** — `EvaluatorWorkspace.freeze()` records a tree
   digest and timestamp. The builder must not start before this.
2. **Seal the contract** — `SealedContract` binds:
   - `oracle_digest`
   - `evaluator_digest`
   - `builder_inputs_digest`
   - `acceptance_thresholds`
   - `blocked_requirements`
   - V2 `Lineage` (inputs + input authorities)
3. **Run the builder** inside `BuilderWorkspace`. Every path goes through
   `confined_path` (`security/paths.py`). Direct oracle reads, `..` escapes, and
   symlink walks into the oracle raise `LeakageBlocked`.
4. **Re-verify the evaluator** — any post-freeze swap invalidates the run.
5. **Preserve failures** under `failed-attempts/`; never delete an unsuccessful
   attempt.
6. **Emit a receipt** binding all four digests (`SealedReceipt`).

Contracts are content-addressed and verify themselves. They reuse V2 lineage
discipline but are **not** one of the ten canonical V2 record kinds (schemas
under `schemas/v2/**` are frozen).

## Leakage matrix (the point of Phase M)

| Probe | Expected |
| --- | --- |
| Builder reads an oracle file directly | Blocked |
| Builder path-escapes via `..` | Blocked |
| Builder plants a symlink into the oracle | Blocked |
| Evaluator swapped after freeze | Blocked |
| Hidden camera materialised into builder inputs | Blocked |
| Threshold edited after the contract is sealed | Blocked |

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-sealed-framework.py --output artifacts/v2/sealed
```

Exit is non-zero if any probe fails to block the cheat.

## Six fixed targets

Manifests and sealed contracts live under `benchmarks/<target_id>/`. Full scored
runs are later phases; Phase M only freezes the contracts and proves isolation.

| Target | Evidence | Notes |
| --- | --- | --- |
| `datacenter_film` | available | Procedural flagship film; delivery + cinematic gates |
| `remote` | **blocked** | Needs real multiview remote capture; dense MVS needs CUDA |
| `soft_object` | available | Soft/cloth multi-state reconstruction + material set |
| `organic` | available | Sculpture / plant / cloth topology lane |
| `fur_animal` | **blocked** | Real animal only; synthetic `animal_bust` is forbidden here |
| `browser_round_trip` | available | Browser → Blender → browser; max one Playwright browser |

### Blocked evidence (exact, never substituted)

**`remote`**

- `real_remote_multiview_capture` — no authorized physical multiview of a remote
  body. Synthetic stand-ins would launder `PROCEDURAL_GROUND_TRUTH` as
  `OBSERVED` and are refused.
- `colmap_dense_mvs_cuda` — host COLMAP 4.0.4 has no CUDA; sparse SfM can run
  when images exist, dense MVS cannot.

**`fur_animal`**

- `real_animal_capture` — no rights-cleared real-animal multiview. The synthetic
  organic-lane bust is a separate non-scored fixture.
- `measured_groom_reference` — no evaluator-grade real groom reference yet.

## Layout per target

```
benchmarks/<target_id>/
  manifest.json          # inputs, hidden evidence, evaluator, thresholds
  contract.json          # sealed SealedContract (four digests + thresholds)
  builder_inputs/        # declared public packet only
  oracle/hidden/         # cameras, measurements, materials, canary
  evaluator/             # frozen gates tree
```

## API sketch

```python
from blender_vision.benchmarks.sealed import (
    BuilderWorkspace,
    EvaluatorWorkspace,
    OracleWorkspace,
    freeze_contract,
    load_manifest,
    load_contract,
    run_sealed_benchmark,
    run_leakage_matrix,
)

manifest = load_manifest("datacenter_film")
contract = load_contract("datacenter_film")
contract.verify()
```

## Doctrine checklist

- [x] Builder cannot reach oracle answers
- [x] Evaluator frozen before builder; digest re-checked after
- [x] Thresholds digest-bound in the sealed contract
- [x] Failed attempts preserved
- [x] Blocked evidence declared exactly
- [x] No silent synthetic substitution for blocked targets
- [x] NOCTURNE/ONE behaviour unchanged
