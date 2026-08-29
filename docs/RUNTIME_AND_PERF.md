# Runtime and performance

This page is the active index for runtime behavior and measured performance.
Performance measurements describe a bounded environment; they do not create a
pricing authority, capacity promise, or release approval.

## Runtime contract

- The control service owns admission, verification, accounting, and settlement.
- Worker capabilities are admitted from signed or otherwise bound authorities;
  a heartbeat is not proof of useful capacity.
- Every quote and job path rechecks the current authority at its durable
  boundary and fails closed on drift, expiry, missing evidence, or ambiguous
  provider state.
- Historical performance receipts remain tied to their exact runtime, model,
  device, workload, and source digest. They are not silently reused for a new
  deployment.

## Measurement policy

Measured throughput, power, latency, and residency are reported with their
workload and hardware identity. They are evidence for a bounded comparison,
not a buyer-facing promise. Pricing uses the applicable catalogue and supplier
economics authority, not a copied benchmark number.

The hot path must preserve the free-admit and verification invariants: no
unverified work is admitted to settlement, no warm-prefix shortcut creates
unattributed savings, and no capacity estimate bypasses a durable recheck.

## Safe checks

From the repository root, use the read-only or local checks below:

```bash
python3 ops/scripts/secret-exposure-audit.py
python3 ops/scripts/validate-readiness.py
python3 ops/scripts/validate-claim-surfaces.py
cd src/control && go test ./... -run '^$' && go vet ./...
```

Database-backed financial tests require an explicitly supplied test database;
do not substitute production credentials or silently skip them. External
staging, Stripe, provider, and soak evidence stays under their own gates.

## Historical measurements

The detailed benchmark ledger, runtime census, and superseded decision records
remain in [the engineering archive](archive/engineering/RUNTIME_AND_PERF_2026-08-28.md).
The archived material is useful for provenance and comparison only.
