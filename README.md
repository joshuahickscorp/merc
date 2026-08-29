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

## Development

Local infrastructure uses Docker Compose:

```bash
make dev-up
make build
make test-unit
(cd agent && cargo test)
bash scripts/verify-python-sdk-package.sh
```

`make test` runs the broader Go, Rust, and SDK checks and may require the local
database/object-store services. `make up` starts the full local stack when a
rebuilt control service is needed.

## Configuration and release boundary

Copy `.env.example` to an ignored local environment file when developing. The
checkout should contain templates only; credentials, provider tokens, and
operator keys belong in the host secret manager or another access-controlled
location. Run `python3 scripts/secret-exposure-audit.py` before packaging a
release. Credentials that ever appeared in Git history must still be revoked or
rotated and removed through a reviewed history rewrite before this repository is
shared publicly.

## Repository layout

- `control/` — Go control plane, schema, admission, accounting, and tests.
- `agent/` — Rust provider agent and execution runtime.
- `clients/` — Python/TypeScript SDKs, CLI, protocol, and desktop client.
- `web/` — web surfaces and assets.
- `deploy/`, `ops/`, and `scripts/` — deployment, evidence, validation, and
  operational tooling.
- `docs/` and `evidence/` — architecture, runbooks, readiness, and receipts;
  [archived planning and closeout material](docs/archive/README.md) is kept
  separate from the active surface.

The detailed operational contract stays in the repository’s readiness and
security documents; this README intentionally keeps business-sensitive
implementation detail out of the landing page.
