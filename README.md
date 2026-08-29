# MERC

MERC is a distributed compute marketplace and control-plane prototype
coordinating workloads across heterogeneous compute providers with metering,
verification, accounting, and operational controls. It is an engineering
prototype, not an approved live-money or public-launch service.

## System

- The Go control plane handles authenticated HTTP APIs, admission, scheduling,
  records, accounting, and operational gates.
- The Rust `merc-agent` runs on a provider machine, enrolls and heartbeats,
  claims work, executes an allowed workload, uploads results, and reports
  completion. macOS sandboxing is applied where the local runtime supports it.
- PostgreSQL stores queue and ledger records. MinIO provides S3-compatible local
  object storage for inputs and results.
- Known-answer, duplicate, and majority checks support result verification.
- Python and TypeScript SDKs, a CLI, web surfaces, deployment files, and
  operational scripts provide client and operator entry points.

The code contains payment and settlement paths, but configured payment code is
not evidence that money moved. Current release boundaries are recorded in
[RELEASE_READINESS.md](RELEASE_READINESS.md); live money movement remains
prohibited by the current readiness state.

## Architecture

```text
client -> control plane -> provider agent -> execution
                                      -> result upload -> verification -> accounting
```

The control plane and agent bind workload, runtime, and verification identity
before admission. Unknown workload types, models, engines, and devices are
rejected; unadvertised or unproven lanes are not automatically sellable.

## Canonical tree

The repository has five maintained top-level areas:

- `src/` — the Go control plane, Rust provider agent, render helpers, schemas,
  and runtime authority.
- `clients/` — SDKs, protocol definitions, desktop client, and web surfaces.
- `ops/` — deployment, release tooling, audits, test harnesses, and local
  operational configuration.
- `docs/` — active architecture/runbooks plus clearly separated historical
  material.
- `evidence/` — retained receipts, manifests, and independently verifiable
  release evidence.

Implementation code and scripts do not live as loose root files. Mutable local
runtime output remains ignored at its historical paths; committed evidence is
kept under `evidence/` and is not treated as a runtime scratch directory.

## Test tiers

The tiers are intentionally distinct. A fast green loop is not release
certification, and an unavailable external service is never represented as a
successful integration result.

| Tier | Target | Purpose |
| --- | --- | --- |
| 0 | `make test-qualification` | Seconds-scale core financial, admission, identity, evidence, and workload invariants. |
| 1 | `make test-normal` | Short Go, Rust, and SDK package suites. |
| 2 | `make test-expensive` | Real PostgreSQL/object storage and provider-agent integration. |
| 3 | `make test-certification` | Full CI, security, mutation, portability, evidence, and release gates. |

The machine-readable contract is [`ops/test-tiers.json`](ops/test-tiers.json).

## Development

Local infrastructure uses Docker Compose:

```bash
make dev-up
make build
make test-qualification
make test-normal
```

`make test-expensive` requires the local database/object store and a built
agent. `make test-certification` is the complete release surface. `make up`
starts the full local stack when a rebuilt control service is needed.

## Configuration and release boundary

Copy `.env.example` to an ignored local environment file when developing. The
checkout should contain templates only; credentials, provider tokens, and
operator keys belong in the host secret manager or another access-controlled
location. Run `python3 ops/scripts/secret-exposure-audit.py` before packaging a
release. Credentials that ever appeared in Git history must still be revoked or
rotated and removed through a reviewed history rewrite before this repository is
shared publicly.

The detailed operational contract stays in the repository’s readiness and
security documents; this README intentionally keeps business-sensitive
implementation detail out of the landing page.
