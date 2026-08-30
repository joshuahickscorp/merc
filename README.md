# Merc

Merc is a frozen engineering prototype for a distributed compute marketplace.
It coordinates heterogeneous providers through a Go control plane, a Rust
provider agent, independent result verification, metering, accounting, and
operator-controlled release gates.

This repository is reviewable and testable. It is not approved for live money,
unrestricted public access, or unsupervised production use.

## Current boundary

| Surface | Decision |
| --- | --- |
| Level A software candidate | `GO` |
| Backend alpha | `NO_GO` |
| Private canary | `NO_GO` |
| Live money / public launch | `NO_GO_PROHIBITED` |

The authoritative status and release checks are in
[docs/RELEASE_READINESS.md](docs/RELEASE_READINESS.md). Payment and settlement
code exists for bounded testing; its presence is not evidence that money moved.

## Runtime

```text
buyer or supplier client
          -> Go control plane -> provider agent -> bounded execution
                              -> receipt -> verification -> accounting
```

The control plane owns authentication, admission, scheduling, pricing,
ledger writes, settlement, evidence, and the public HTTP contract. The Rust
`merc-agent` owns the provider-side execution boundary and cannot set prices
or authorize settlement. PostgreSQL stores durable state; MinIO provides local
S3-compatible object storage.

## Repository map

- `src/control/` — Go control plane, durable schema, accounting, verification,
  runtime authorities, and the service entry point.
- `src/agent/` — Rust provider agent and its sandboxed execution boundary.
- `clients/` — protocol definitions, CLI, SDKs, desktop client, and web UI.
- `ops/` — the single deployment/configuration, release-gate, audit,
  benchmark, and operator surface; its map is in [`ops/README.md`](ops/README.md).
  Provider CLIs are intentionally external to this cross-platform source tree.
- `docs/` — current architecture, security, operations, governance, and legal
  contracts; historical material is isolated under `docs/archive/`.
- `evidence/` — retained receipts, manifests, measurements, and binding data;
  it is not a local scratch directory and does not authorize a release by
  itself.

Root files are limited to standard build/deployment entry points and project
metadata. Mutable credentials, model data, generated render output, local
runtime state, and test artifacts are ignored.

## Fast local loop

```bash
make test-qualification
make test-normal
```

Use `make dev-up` for PostgreSQL and MinIO. `make test-expensive` requires the
external services and a built agent. `make test-certification` is the complete
CI/release surface; unavailable infrastructure remains a blocked check rather
than being represented as a pass.

The machine-readable tier contract is [`ops/test-tiers.json`](ops/test-tiers.json).
Run the read-only credential audit before packaging:

```bash
python3 ops/scripts/secret-exposure-audit.py
```

## Navigation

- [Architecture](docs/ARCHITECTURE.md)
- [Release readiness](docs/RELEASE_READINESS.md)
- [Security boundary](docs/SECURITY.md)
- [Operational runbooks](docs/RUNBOOKS.md)
- [Backend-alpha contract](docs/BACKEND_ALPHA_CONTRACT.md)
- [Changelog](docs/CHANGELOG.md)
