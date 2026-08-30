# Operations

This is Merc's single operational surface. Runtime deployment files, safe
configuration templates, release gates, and operator commands live here; real
credentials and mutable runtime state do not.

## Start here

- [`test-tiers.json`](test-tiers.json) — the machine-readable test contract.
- [`deploy/`](deploy/) — Compose, Caddy, container, recovery, and Cloudflare
  deployment manifests.
- [`configs/`](configs/) — checked-in templates and pricing inputs; never put a
  populated secret in this directory.
- [`monitoring/`](monitoring/) — Prometheus, Alertmanager, and Grafana config.
- [`staging/`](staging/) — supervised staging overlays and their contracts.
- [`systemd/`](systemd/) — host service/timer units.

## Commands

The top-level files in [`scripts/`](scripts/) are the supported command
surface. The subdirectories narrow the purpose:

- `alpha/` — supervised canary and staging adapters.
- `lib/` — sourced helpers and evidence writers; not standalone commands.
- `render/` — deterministic render and verification helpers.
- `soak/` — long-running local observation.
- top-level `validate-*`, `verify-*`, `test-*`, and `seal-*` commands —
  fail-closed audits, contract tests, and receipt producers.

Run the ordinary local qualification loop from the repository root:

```bash
make test-qualification
make test-normal
python3 ops/scripts/secret-exposure-audit.py --ci
```

External services, paid providers, staging access, and live-money actions are
never implied by a passing local check. The release boundary remains the
combination of [`../docs/RELEASE_READINESS.md`](../docs/RELEASE_READINESS.md),
the machine ledgers in this directory, and bound evidence under
[`../evidence/`](../evidence/).
