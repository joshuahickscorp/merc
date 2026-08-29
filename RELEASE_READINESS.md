# Release readiness

MERC is a frozen engineering candidate. The machine-readable ledgers are the
authority; this page is the short operator-facing summary.

| Surface | Current decision | Boundary |
|---|---|---|
| Level A software candidate | `GO` | Buildable, locally tested, and governed by the checked-in evidence and release controls |
| Backend alpha | `NO_GO` | Controlled participants, persistent staging, Stripe test mode, and no value movement |
| Level B private canary | `NO_GO` | Exact candidate, approved participants, recovery, alerting, and test-mode money-path proof |
| Level C live money/public launch | `NO_GO_PROHIBITED` | No real charges, transfers, payouts, public signup, or independent suppliers |

The current derived software score is 83/100 against a 95-point threshold,
with four open target-scope P1 gates. Backend alpha is recorded as 81/92;
`ALPHA_ENGINEERING_READY` and `EXTERNAL_ALPHA_PROVEN` are both `NO_GO`.
These values are a snapshot for orientation, not a substitute for rerunning
the validators.

## Authority and checks

- `ops/go-no-go.json` — release decisions, open P0/P1 gates, and the exact
  machine input request.
- `ops/readiness.json` — derived score domains and the separate backend-alpha
  decision axis.
- `ops/backend-alpha-gates.json` — gate classifications and evidence rules.
- `docs/BACKEND_ALPHA_CONTRACT.md` — meaning and scope of the controlled alpha.
- `docs/SECURITY.md` — authorization, storage, sandbox, and residual-risk
  boundary.
- `docs/RUNBOOKS.md` — deployment, recovery, incident, and evidence handling.

Run the read-only candidate check from the repository root:

```bash
python3 scripts/validate-readiness.py
python3 scripts/secret-exposure-audit.py
```

The broader release check is `make ci`. It may require local PostgreSQL,
object storage, or operator-owned test infrastructure; an unavailable external
dependency is a blocked check, never a release pass.

## Non-negotiable safety boundary

The repository contains payment and settlement code, but the current decision
does not authorize money movement. Live Stripe credentials, provider secrets,
operator keys, participant data, and deployment environment files belong
outside Git in an access-controlled secret store. The checkout should contain
templates and test fixtures only. Any credential that appeared in history must
be revoked or rotated before external sharing; the audit must not print secret
values.

## Historical material

Completed planning, staging, closeout, and network-migration narratives are in
`docs/archive/engineering/` and `docs/archive/staging/`. They are preserved as
history and are not current launch instructions. The active documentation set
is intentionally limited to architecture, security, operations, legal/privacy,
distribution, the backend-alpha contract, and this status page.
