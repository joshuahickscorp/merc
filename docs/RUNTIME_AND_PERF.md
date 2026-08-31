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
- The versioned `RequestIdentity` digest now streams the same JSON escaping
  through one reusable encoder instead of allocating a marshaled blob for every
  field. The representative benchmark moved from about 1.61 to 1.50
  microseconds, 681 to 488 bytes, and 24 to 13 allocations; escaped-value
  parity tests retain the prior digest contract.
- Request-identity digest finalization now writes SHA-256 output into fixed-size
  stack buffers before constructing the returned `req_` string. The same
  representative benchmark moved from about 1.50 to 1.35 microseconds, 488 to
  328 bytes, and 13 to 10 allocations; the byte-compatible golden digest tests
  remain green. The same finalization reduction also brings the raw-top-level
  realtime identity miss to about 5.1 microseconds, 1,956 bytes, and 47
  allocations.
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
- The same commitment path now reuses the JSON-escaped strings for the finite
  runtime-profile catalog instead of marshaling those two immutable values on
  every request. The helper moved from about 0.64 microseconds, 160 bytes, and
  4 allocations to about 0.43 microseconds with zero bytes and allocations;
  representative full request preparation also drops from 94 to 90 allocations.
- Identity-cache misses now project top-level members from the canonical
  prepared body as `json.RawMessage`, keeping nested prompt/tool/schema bytes
  intact instead of recursively decoding and re-marshaling them. Representative
  derivation moved from about 9.3 to 7.4 microseconds and from 148 to 73
  allocations, with compatibility coverage for numeric precision, legacy token
  fallback, ignored transport tags, and refusal of out-of-set fields.
- Canonical-body identity misses now parse scalar numbers directly from their
  bounded raw JSON instead of creating a decoder and interface value for each
  scalar. The representative raw-top-level derivation moved from about 6.7 to
  5.3 microseconds and from 7,024 to 2,116 bytes across 62 to 50 allocations;
  nested-decoder parity remains covered.
- Existing API-key and realtime-identity cache-hit paths remain allocation-free.
  Database ranking and settlement were not loosened or bypassed because they
  are durable authority paths.
- Positive API-key cache hits now take a shared read lock, while expired-entry
  cleanup rechecks under the write lock. The parallel cache benchmark moved
  from about 240 to 219 nanoseconds per lookup with zero allocations; same-
  process revocation and the 250 ms cross-instance TTL bound are unchanged.
- Stripe payout idempotency-key construction now reserves its bounded output
  once instead of concatenating intermediate strings. The representative local
  setup benchmark moved from about 118 to 84 nanoseconds, 181 to 149 bytes,
  and 4 to 3 allocations; the exact key-format tests remain unchanged.
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
