# Decision Zero reversal — `[KEEP-RT]` supersedes `[KILL-RT]`

**Status: EXECUTED 2026-07-27.** The MERC COMPLETE-SHIPPABILITY goal is the
owner directive that supersedes `[KILL-RT]`; the lane is restored and green.

`docs/DECISION_ZERO_OUTCOME.md` recorded `[KILL-RT]` on 2026-07-26: the
OpenAI-compatible realtime lane was removed from the product and preserved as a
checksummed snapshot. The MERC COMPLETE-SHIPPABILITY goal lists that lane first
and says "Do not narrow launch to batch only", which reverses it.

I have **not** restored the lane on that inference alone. This file records why
the reversal is probably correct, and exactly what it costs, so the decision is
made once and in writing rather than drifting.

## Why KILL-RT was decided

The question was: *within 90 days, can you get a Linux/CUDA supplier to register
and serve real traffic?* The answer was "assume not". The supporting facts:

- `control/types.go` admits only four `apple_silicon_*` hardware classes.
- `proto/manifest.schema.json` pins `engine` to `"candle"`.
- An NVIDIA worker is therefore **rejected at registration with HTTP 400**.

Selling a latency SLA on hardware the control plane refuses to register is not a
product.

## What changed

**RunPod is set up.** The shippability goal's second lane is "RunPod-backed
pinned vLLM workers through Merc routing, verification and money". That is
precisely the CUDA supply whose absence justified the kill. The premise is now
false, so the conclusion drawn from it no longer holds.

## What the reversal actually costs

Restoring the lane is not just re-adding files. The hardware admission that made
it useless is still in force:

| work | where |
|---|---|
| Admit CUDA hardware classes | `control/types.go` |
| Admit a non-candle engine | `proto/manifest.schema.json` |
| Restore 5 Go files + runtime profile | `realtime-lane-snapshot/` (verified, 8 files + manifest) |
| Migrate the restored store to the sole ledger writer | the snapshot predates `control/ledger_write.go`; its raw `INSERT INTO ledger_entries` violates `TestNoRawLedgerInsertsOutsideWriter` |
| Restore schema DDL | ~297 lines removed when KILL-RT was completed |
| Re-add 5 routes to the authorization matrix | count returns 72 → 77 |
| Re-add 3 alerts + metrics + dashboard panel | removed as dead when the lane went |

**Realtime and RunPod are one lane, not two.** Neither is shippable without the
hardware-admission change, and the hardware-admission change is what makes both
worth doing.

## Recommendation

Confirm `[KEEP-RT]`, and treat it as a single lane: *CUDA admission + realtime
surface + RunPod worker*, proven end to end through routing, verification and
settlement before any public claim.

Do not restore the realtime files on their own. A realtime surface with no
registrable hardware is what KILL-RT correctly deleted, and re-adding it without
the admission change recreates exactly that.

## Note for whoever reads this next

The realtime files were never committed to git. Their absence looks identical to
data loss, and I restored them once by mistake earlier in this session before
finding the `[KILL-RT]` marker in `control/authorization_matrix_test.go`. Grep
for `KILL-RT` before concluding anything is missing.

## What was actually done

- CUDA hardware classes (`nvidia_24gb/48gb/80gb/180gb`) and the `vllm` engine
  admitted, paired so an engine cannot run on hardware that cannot serve it.
- 5 Go files + runtime profile restored from the verified snapshot, renamed
  `CX_` to `MERC_` to match the rest of the tree.
- 289 lines of realtime DDL restored; schema applies clean.
- 5 routes, the admin refund handler, the recovery sweep, 7 counters and the
  operational gauge block restored. Authorization surface 72 -> 77.
- `scripts/realtime-parity-benchmark.py` **rewritten** — the original was
  untracked and destroyed in the KILL-RT cleanup. The rewrite reconstructs the
  contract from its call site and keeps the attestation gate: a run against an
  unattested upstream reports `UNATTESTED_HARNESS_RUN` and refuses
  `public_claim_allowed`.

**Resolved 2026-07-27.** `realtime-openai-python-conformance.py` and
`realtime-openai-node-conformance.mjs` were rewritten from their call sites in
`control/realtime_integration_test.go`, which specifies the full contract: seven
capability flags, all required, plus `status == "PASS"`. Both now pass against
the real `openai` Python 2.48.0 and JavaScript 6.49.0 clients.

`realtime-sdk-conformance.sh` is NOT reconstructed. Unlike the other two it left
no call site carrying assertions - only a Makefile target name - so rebuilding
it would mean inventing a contract and calling the invention a recovery. The
`realtime-sdk-conformance` target now runs the two harnesses that do have a
specified contract, and fails loudly rather than skipping when the SDKs are not
configured.

## Bug this uncovered

Completing KILL-RT had removed the `execution_contracts` term from
`BuyerFreeCreditRemaining` **and the comma separating the final `GREATEST`
argument**, leaving `GREATEST(a - b - c 0)` — invalid SQL that shipped with
`make ci` green because nothing executed the query. The realtime integration
test caught it. Repaired, and `TestBuyerFreeCreditRemainingIsValidSQL` now
executes the query so it cannot recur.
