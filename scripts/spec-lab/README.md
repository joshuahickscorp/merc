<!-- CLAIM-SCOPE: internal-engineering-non-authoritative -->
# spec-lab (condensed)

The speculative-execution research is condensed to
[`docs/research/spec-lab-receipt.md`](../../docs/research/spec-lab-receipt.md);
the 163 research/cloud drivers were deleted (recoverable from git `96a4890`).

What remains here is the still-green contract surface exercised by CI and
`make spec-test`: the core (`cx_speculative_core`, `cx_render_spec_adapter`,
`cx_transcode_spec_adapter`, `cx_integrated_speculation`), the ladders
(`run_*`), `emit_current_spec_receipts`, and their five `test_*` gates. Per the
reduction plan this surface moves into the Rust `spec-engine` (`cx-core`), after
which the Python is deleted too.
