# Historical programme digest

This file is a compact, content-addressed index of the retired programme
record. The full working plans were development material, not release
authority; their complete history remains available through Git. The active
decision is derived from `docs/RELEASE_READINESS.md`, `ops/readiness.json`,
`ops/go-no-go.json`, and the bound receipts under `evidence/`.

## Current boundary

Merc is a frozen engineering prototype. The Level A software candidate is
`GO`; backend alpha, private canary, live money, and public launch remain
`NO_GO` or `NO_GO_PROHIBITED`. Historical positive loops, local simulations,
and unbound provider measurements cannot promote a release or authorize
settlement.

## Preserved decision anchors

- **Throughput cancels.** Supplier-liability projection is
  `units / 1000 × price × share`; throughput is an observation and must not
  be used to manufacture a cost claim.
- **Prefix routing is not savings.** Warm-prefix routing may influence
  selection, but no production job is credited with prefix savings until a
  receipt-backed attribution exists. See the Step 25 shape note in
  `NETWORK_V2_EXECUTION_PLAN.md`.
- **RemainderCarry is not production settlement.** The type preserves exact
  fractional arithmetic in isolation; the live settlement path does not use
  it, so no exact-nanos-per-task conservation claim is made.
- **External evidence is bounded.** Provider, staging, performance, and
  governance records retain their binding status and may not be relabelled by
  prose.

## 8. Licensing and release truth

The generated license inventory, third-party register, model provenance, and
release ledgers are separate authorities. A blocked model or incomplete human
approval stays blocked; removing a catalogue price does not resolve its
licensing or governance obligation.

## Facet external action pack

The remaining time-dependent and externally witnessed facets require the
operator runbooks and fresh bound receipts. Local deterministic tests are
useful qualification evidence but do not substitute for the stated staging,
canary, or 24-hour requirements.
