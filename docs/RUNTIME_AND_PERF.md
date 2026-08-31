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

## Latest bounded pass

The 2026-08-31 local pass reduced repeated representation work without
changing the request-identity encoding, payment authority, or settlement
boundary:

- Realtime sampling-fingerprint lookup moved from reparsing the prepared JSON
  body on every access (about 2.9 microseconds, 1,715 bytes, 53 allocations)
  to the preparation result (about 13 nanoseconds, zero bytes, zero
  allocations).
- Identity-cache-miss construction moved from about 2.46 microseconds and
  1,506 bytes across 28 allocations to about 1.61 microseconds and 681 bytes
  across 24 allocations. A golden identity digest confirms byte compatibility.
- Prepared realtime identity hits now reuse the canonical body digest computed
  during request preparation: the production-shaped hit measured about 151
  nanoseconds versus about 260 nanoseconds for the compatibility path that
  hashes the body on access, with zero allocations in both paths.
- Realtime `InputCommitment` now streams the already-canonical upstream body
  into the enclosing commitment hash instead of serializing the payload a
  second time. On the representative local payload, that path measured about
  0.62 microseconds, 160 bytes, and 4 allocations versus about 2.2 microseconds,
  1,683 bytes, and 22 allocations for the prior full-map marshal; a parity test
  covers escaped strings and large integers.
- Existing API-key and realtime-identity cache-hit paths remain allocation-free.
  Database ranking and settlement were not loosened or bypassed because they
  are durable authority paths.
- The flag-on liveness shadow fingerprint now uses the same FNV-1a bytes through
  a direct loop: about 18 nanoseconds became about 12 nanoseconds per lookup,
  with zero allocations and a compatibility test against the standard
  implementation.

These are local Go microbenchmarks, not end-to-end time-to-first-token or
payment-latency promises. The local pass did not have the external database,
provider engine, or live payment environment available.

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
remain under `evidence/perf/`; this page is the canonical runtime and
performance index.
The archived material is useful for provenance and comparison only.
