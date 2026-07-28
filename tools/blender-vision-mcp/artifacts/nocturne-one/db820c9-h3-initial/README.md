# NOCTURNE/ONE H3 initial sealed result

This directory preserves the unmodified first `H3-codex-sealed` candidate and
its evaluator evidence before any benchmark-driven repair.

## Frozen authorities

- Definition Git head: `db820c9dec75cfa73ffd9ff800874cc81c465c2c`
- Contract SHA-256:
  `b1ff7fd969075671ab59ffea65757a1fbcc825217fc456d002dd206cc7154fd6`
- Packet manifest SHA-256:
  `434a699dfd95e379c7c56d020fc5b0137f099c8c172ecb9690063df13b71d079`
- Oracle BLEND SHA-256:
  `947ba11ac4c05cfffeacb2b2655538644a918f9529a9c1b090fed8e973856102`

## Independent builder result

- Condition: `H3-codex-sealed`
- Wall clock: `3356.6659671660163` seconds
- Child exit: `0`
- OS denial preflight: `PASS`
- Oracle canary scan: `PASS`
- Candidate symlinks: `0`
- Failed attempts retained: `6`
- Accepted attempt: `attempt-007`
- Sealed-builder receipt SHA-256:
  `8e95cd6b2e6655372c874398fbb3200a7d963b1af9460e3db19fe5bdee19b4a6`
- Initial candidate receipt SHA-256:
  `bedd43987df7d1bcca04e084d88e2d5651de3e684ddb4f1da1b4033ba3982975`

The candidate source copied to `sandbox/nocturne-one/` was byte-identical to
the sealed builder output when this evidence directory was created.

## Frozen evaluator results

### 3D

- Status: `PASS`
- Assertions: all passed
- Public silhouette IoU: `0.9592975674` to `0.9781549393`
- Hidden silhouette IoU: `0.9565207319` to `0.9814884414`
- Receipt SHA-256:
  `f86d7f3e9b619317fc108ee9e07eeac695bd9b9fffbeae7be43a561ac712494f`

### Application

- Status: `FAIL`
- Assertions: `26 / 27` passed
- Sole failure: P0 `keyboard_journey`
- Observed focus targets after eight tabs: eight anchor elements; the
  `enter-3d` control was not reached inside the evaluator's fixed bound.
- Receipt SHA-256:
  `d8aad62ff1ded6dd927ab3beed0863bedaecf5c6efd844f5fb8225e6c5c5d01a`

The failed application receipt is intentionally retained. It is the causal
input to the bounded focus-order repair and must never be replaced by a later
passing receipt.
