# Decision Zero — resolved: `[KILL-RT]`

**Decided by the owner, 2026-07-26.** The question was: *within 90 days, can you
get a Linux/CUDA supplier to register and serve real traffic?* The answer was
"assume not", which selects `[KILL-RT]`.

## What that means

The realtime OpenAI-compatible streaming lane is removed from the product. It was
never tracked in git, never in any CI run, and never executed against a GPU.

`control/types.go` admits only four `apple_silicon_*` hardware classes and the
`candle` engine, and `proto/manifest.schema.json` pins `engine` to `"candle"`. An
NVIDIA worker is rejected at registration with HTTP 400. Selling a latency SLA on
hardware the control plane refuses to register is not a product, and maintaining
a second lane for supply that will not arrive is the expensive failure mode.

## Recoverability

The lane is preserved outside the repository as a checksummed snapshot:

    realtime-lane-snapshot/            9 files, 3,680 lines
    realtime-lane-snapshot/MANIFEST.sha256

Verified with `shasum -a 256 -c MANIFEST.sha256` immediately before removal. This
is a reversible decision, which is why the snapshot was taken before the delete
rather than after.

## What survives, and why it was still worth building

The money and correctness work done on the realtime lane was not wasted, because
it was mostly done to shared machinery that the batch lane uses too:

- one integer-micro ledger writer, enforced by test
- Stripe transfer reversal and the global payout pause
- the atomic capacity reservation pattern
- the buyer-scoped balance indexes
- prepaid balances

## What this does not decide

`[KILL-RT]` says the realtime lane is not the product. It does not say the batch
AI lane is. Market research commissioned the same day found that no AI inference
lane pays a supplier above electricity at market prices, and that the best
non-AI lane — deadline-tolerant GPU rendering — is roughly three orders of
magnitude better per supplier-hour. See `docs/LANE_RESEARCH.md`. That is a
separate decision and it is still open.
