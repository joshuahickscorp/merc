# Merc architecture

Merc is a frozen engineering candidate for a distributed compute marketplace.
The current checkout is software-reviewable, but backend alpha and live-money
release remain blocked by the machine authorities in `ops/`.

## System boundary

```text
buyer / supplier clients
          |
          v
      control (Go) <-> Postgres + MinIO
          |
          v
      agent (Rust) -> bounded execution -> receipts
```

- `src/control/` owns authentication, capability admission, quotes, job state,
  verification, accounting, settlement, and the public HTTP contract.
- `src/agent/` owns the worker-side execution boundary and reports receipts; it
  does not decide prices or move money.
- `clients/`, `clients/web/`, `ops/configs/pricing/`, `ops/deploy/`, and `ops/scripts/` are entry points,
  policy tooling, or operational wrappers. They do not replace control-plane
  authorities.

## Request and money path

The public sequence is model discovery, quote, idempotent job submission,
capacity assignment, execution, independent verification, and settlement.
Quotes are not orders, permissions, or proof of execution. A durable job is
accepted only with the applicable pricing, verification, worker, and funding
authorities; ledger projection is separate from modeled economics.

Verification checks the declared attempt, workload, receipt, answer, and
honeypot contract before a supplier outcome can affect settlement. Refunds,
reversals, payout holds, and webhook replay are reconciled from provider
events, never inferred from a client response.

## Release and security boundary

`activation` remains disabled in the source and evidence contracts. Live Stripe
mode is prohibited by the current readiness state. Test-mode credentials belong
in operator-controlled secret storage only; they must not appear in receipts,
logs, command arguments, or documentation.

The release decision is the intersection of `RELEASE_READINESS.md`,
`ops/go-no-go.json`, `ops/readiness.json`, backend-alpha gates, and the
governance/legal records. A green unit test or a local route does not promote a
claim to staging, private canary, or public production proof.

## Active technical contracts

- `src/control/schema.sql` is the durable storage contract; migrations and ledger
  invariants are reviewed as accounting changes.
- `src/control/pricing.go` is the pricing authority and keeps historical replay
  separate from current catalogue state.
- `src/control/verification.go` and `src/control/verification_artifact.go` own the
  verification and bounded-artifact policies.
- `ops/scripts/secret-exposure-audit.py`, the claim-surface validator, and the
  governance validators are read-only gates over the release surface.

## Media contracts

### Merc media-transcode contract

`ffmpeg-transcode-v1` is a Merc-owned, fixed local contract. FFmpeg/libx264
execution is permitted only through the pinned binary and provenance path;
future remote code or model weights require a separate license and evidence
review.

### Bounded media rendering contract

`svg-scene-render-v1` is a deterministic Merc-owned CPU rasteriser. It accepts
closed scene input and does not activate a prompt-to-image model or fetch
remote assets.

## Postgres trust boundary

The production Compose exception uses `sslmode=disable` only on a private project bridge with no `ports:` mapping on a single-host deployment. The
`POSTGRES_PASSWORD` never belongs in an external URL or process argument. Any
remote Postgres connection requires TLS, and the server must actually present a certificate that the client verifies.

## Detailed history

The former long-form architecture and embedded decision records are preserved
in [the engineering archive](archive/engineering/ARCHITECTURE_2026-08-28.md).
They are provenance, not a competing active specification.
