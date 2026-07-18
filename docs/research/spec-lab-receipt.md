# spec-lab research receipt (condensed)

Condensation of the `scripts/spec-lab/` speculative-execution research per the
System Reduction Plan (CONDENSE: keep the conclusion + reproducibility metadata,
delete the drivers; git history is the archive).

- **Source commit of the full lab:** `96a4890` (recover any driver with
  `git checkout 96a4890 -- scripts/spec-lab/<file>`).
- **Measurement ledgers:** the `.jsonl` result ledgers live in the
  `computexchange-benchmarks` pack (`PACK_MANIFESTS.json`) and in history.
- **Deleted:** 163 research drivers (96,348 LOC) - `run_*`, `screen_*`, `pod/*`
  RunPod cloud drivers, `verify_render_*`, per-experiment tests, one-off tuners.

## What it investigated

Speculative execution across three modalities, to decide whether a draft/verify
loop lowers cost without changing output:

- **Token speculation** (`run_token_spec_decode_ladder`) - draft-model token
  proposal + exact-match verify.
- **Render speculation** (`run_speculative_render_ladder`,
  `run_spec_render_token_end_to_end`) - cheap-preview proposal + tile-verify for
  Cycles/Blender renders.
- **Transcode speculation** (`cx_transcode_spec_adapter`) - fast-codec proposal +
  verify for media transcode.

## Conclusion (promoted mechanism)

The proven substrate is the **Rust `spec-engine`** (authoritative accept/verify +
receipt) plus the retained live adapters. The Python that remains under
`scripts/spec-lab/` is only the still-green contract surface (14 files) exercised
by CI and `make spec-test`:

- core: `cx_speculative_core`, `cx_render_spec_adapter`,
  `cx_transcode_spec_adapter`, `cx_integrated_speculation`
- agent driver: `cx_agent_render_preview_driver` (invoked as a subprocess by the
  Rust agent `render_preview.rs`; a live dependency, not research)
- ladders: `run_cx_native_speculation_ladder`, `run_spec_render_token_end_to_end`,
  `run_speculative_render_ladder`, `run_token_spec_decode_ladder`
- receipts: `emit_current_spec_receipts` (validated by `spec-engine` example)
- their five `test_*` gates

Per the plan's Phase D, this surface is itself slated to move fully into Rust
(`cx-core`) and the Python then deleted; until then it is the reproducible
contract, not research sprawl.
