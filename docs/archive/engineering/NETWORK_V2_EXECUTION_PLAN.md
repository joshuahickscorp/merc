# Historical Network V2 execution digest

The former multi-thousand-line execution plan is retired. This compact record
keeps the decisions that still guard current code; the detailed plan is
available in Git history and is not an active specification.

## Step 25 shape note

Prefix locality is a routing feature, not a savings claim. `ClassPrefixReusedInput`
must not be attributed to a production job until the runtime path writes a
receipt-backed physical-work split. Any future attribution must bind the
candidate, input identity, upstream observation, and settlement result.

## Authority rules

1. A request, accepted decision, execution command, outcome, and receipt may
   be different lifecycle objects, but only one object may decide each fact.
2. Accepted objects are immutable, versioned, and digest-bound to their own
   snapshot; replay does not consult later catalogues or policies.
3. Compatibility conversion happens at one named ingress or egress edge.
4. Historical rows remain historical evidence and are never backfilled from
   current policy.
5. Shadow decisions observe only; they cannot reserve capacity or authorize
   money.

The active implementation and release truth live in `src/control/`,
`docs/ARCHITECTURE.md`, `docs/RELEASE_READINESS.md`, and the bound evidence
authorities. This file exists only to keep the Step 25 guard legible.
