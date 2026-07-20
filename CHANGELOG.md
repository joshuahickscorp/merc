# Changelog

## v0.1.0-rc.1 — pending

Release scope: supervised, approved-participant, synthetic-data,
Stripe-test-mode private canary only. This release is not approved for live
money or unrestricted public access.

### Hardened control plane

- Durable, attempt-scoped artifact authority rejects stale late writes.
- Actor-bound, reason-required, transactional audit records cover privileged
  worker, task, reputation, payout, and operational-control mutations.
- Durable intake, dispatch, payments, and webhook pause controls fail closed.
- Private-canary allowlists and bounded job, resource, retry, and shadow-value
  admission are enforced by the server.
- Supplier payout release requires explicit operator approval in canary mode.
- Canonical billing-customer schema and migration/repricing persistence tests
  cover fresh databases.

### Agent and supply chain

- Model repositories, immutable revisions, artifact sizes, and SHA-256 values
  are source-bound to the runtime authority matrix and verified before use.
- Hardware build identity includes runtime authority and hardware class.
- One fail-closed deadline spans task acknowledgment through final commit.
- Candidate and prior control images are built, SBOM-attested, keylessly signed,
  pulled, and verified by immutable GHCR digest in the release workflow.

### Operations and governance

- Backups are encrypted with `age` before independently scoped offsite upload;
  restore verifies ciphertext and inner payload checksums.
- Metrics, alerts, runbooks, dashboard provisioning, staging deployment,
  rollback, restart-storm, rehearsal, and soak evidence harnesses are included.
- Private-canary terms, privacy/data-governance, DSAR, acceptable-use, support,
  incident, license, model, asset, and economics review artifacts are included.

The tag remains intentionally absent until all external Level-B evidence and
the required 24-hour soak have passed.
