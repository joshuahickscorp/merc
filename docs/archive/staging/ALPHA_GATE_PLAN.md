# Alpha gate plan (supervised Stripe test-mode private canary)

Scaffold only. This document sequences the remaining Level B gates in
`ops/go-no-go.json` and names the scripts that close them. It does not
authorise live money, public launch, or a release tag.

Candidate HEAD this plan is written against: `b1f593b4`. Everything is
test-mode. Live Stripe (`sk_live_` / `rk_live_` / `pk_live_`) is refused
before any network call.

Boot is owned by the power-authority lane (`VENDOR_WALL_UPPER_BOUND`).
Assume that lane will write a green receipt. **No deploy or `--execute`
path in `scripts/alpha/` runs until that receipt is `BOUND` + `PASS`.**

---

## What's left to alpha

One screen. Living copy: `scripts/alpha/status.sh`.

| Gate | State now | Who |
|---|---|---|
| boot (`VENDOR_WALL_UPPER_BOUND`) | blocked — no `evidence/state/alpha-boot-green.json` | power-authority lane |
| **P1-STAGING** | blocked (boot) | **SUPERVISOR** (ssh / docker / TLS) |
| **P1-STRIPE-TEST** | blocked (staging) | **SUPERVISOR** (Stripe TEST API) |
| **P1-OFFSITE-RESTORE** | blocked (staging) | **SUPERVISOR** upload + **SELF-CONTAINED** restore |
| **P1-ALERT-DELIVERY** | blocked (staging) | **SUPERVISOR** (external staffed sink) |
| **P1-CANARY-REHEARSAL** | blocked (staging) | **SUPERVISOR** + 2 Metal devices |
| **P1-RECOVERY-SOAK** | blocked (RUN-LAST) | **SUPERVISOR** — do not start |
| **P1-GOVERNANCE** | needs-supervisor | operator (eight named approvals) |
| **P1-INDEPENDENT-APPROVAL** | `ops/go-no-go.json` | repository_owner (this plan does not decide) |

`P1-INDEPENDENT-APPROVAL` is whatever `ops/go-no-go.json` records.
`scripts/alpha/lib.sh` reads that ledger and does not independently drop
or pass the gate.

---

## Dependency graph

```
                    ┌──────────────────────────────┐
                    │ boot                         │
                    │ VENDOR_WALL_UPPER_BOUND      │
                    │ evidence/state/              │
                    │   alpha-boot-green.json      │
                    └──────────────┬───────────────┘
                                   │
                                   v
                    ┌──────────────────────────────┐
                    │ P1-STAGING                   │
                    │ deploy exact digest + TLS    │
                    │ + external probes            │
                    └──────────────┬───────────────┘
                                   │
          ┌───────────────┬────────┼────────┬───────────────┐
          v               v        v        v               v
   P1-STRIPE-TEST  P1-OFFSITE   P1-ALERT  P1-CANARY   P1-GOVERNANCE
   (TEST matrix)   RESTORE      DELIVERY  REHEARSAL   (operator
                   (R2/age)     (external  (2 buyers    paperwork,
                                 sink)     + 2 Metal)   parallel)
          └───────────────┴────────┬────────┴───────────────┘
                                   │
                                   v
                    ┌──────────────────────────────┐
                    │ P1-RECOVERY-SOAK   RUN-LAST  │
                    │ rollback/forward             │
                    │ restart-storm                │
                    │ 24h soak on 2 Metal devices  │
                    └──────────────────────────────┘
```

**Batch 0** (in flight, not this worktree): boot.

**Batch 1** (serial): P1-STAGING. Nothing else executes until probes PASS.

**Batch 2** (parallel, after staging): Stripe TEST, offsite restore,
external alerts, 2-device canary. Governance paperwork may run in this
window; it is not a soak prerequisite.

**Batch 3** (serial, last): recovery soak. Rollback/forward and the
restart-storm happen *inside* this gate, then the 24h window.

Real dependencies are serial. The four Batch-2 operational gates do not
depend on each other. Canary re-exercises restore / Stripe / alert as
counted scenarios; that is corroboration, not a reason to hold those
gates behind canary.

---

## Fail-closed rules (every `scripts/alpha/*` `--execute`)

1. **Boot not green** — missing or non-`BOUND`/`PASS`
   `evidence/state/alpha-boot-green.json` (override path:
   `MERC_ALPHA_BOOT_RECEIPT`), or a receipt whose `commit` is not
   `git rev-parse HEAD` (override: `MERC_CANDIDATE_COMMIT`). Historical
   `evidence/state/release-image-boot.json` is `UNBOUND` and does **not**
   count.
2. **Live Stripe** — any of `STRIPE_SECRET_KEY`,
   `STRIPE_LIVE_SECRET_KEY`, `STRIPE_RESTRICTED_KEY`,
   `STRIPE_PUBLISHABLE_KEY`, `NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY`
   classified `sk_live_` / `rk_live_` / `pk_live_`. Also
   `MERC_PAYMENT_MODE=live` and any `MERC_PAYOUT_EXPORT`.
3. **Out of order** — `--execute` requires PASS receipts for the
   prerequisites in `scripts/alpha/lib.sh`. Soak requires staging plus
   the four Batch-2 operational gates.

`--print-runbook` / `--print-start-command` never require boot. They are
how the supervisor reads the steps while Batch 0 is still open.

`--check` reports boot/order/live-Stripe and exits 1 if `--execute`
would be refused.

Progress receipts live under `.artifacts/alpha/` (gitignored). They are
an operational ledger, not Level B authority. The existing
`scripts/go-closure-*.sh` writers still produce the bound evidence
under `evidence/go-closure/` when the supervisor runs those paths.

---

## SUPERVISOR-EXECUTES vs SELF-CONTAINED

| Step | Who | Why |
|---|---|---|
| Write boot-green receipt | power-authority lane | other worktree |
| Build `Dockerfile.control` | SELF-CONTAINED (this Mac, OrbStack) | no ssh |
| `docker save \| ssh load`, compose up control/caddy | **SUPERVISOR** | droplet root SSH |
| External HTTPS probes | SELF-CONTAINED | `scripts/alpha/probes.sh --execute` |
| Stamp P1-STAGING | SUPERVISOR | after probes PASS |
| Stripe TEST matrix | **SUPERVISOR** | provider I/O |
| `backup.sh` dump+upload from the droplet | **SUPERVISOR** | data plane |
| Download / age decrypt / scratch PG restore | SELF-CONTAINED | independent credential |
| Fire Alertmanager → external sink | **SUPERVISOR** | pages a human |
| Enrol Mac Studio + MacBook agents | split: each device + supervisor approve | device-bound key never leaves the box |
| Counted canary matrix | **SUPERVISOR** | needs both workers + CP |
| Rollback / restart-storm / 24h soak | **SUPERVISOR** | RUN-LAST; this session does not start it |
| Governance bundle | operator | eight named approvals |

This worktree must not: ssh, `docker compose up` on the droplet, call
Stripe, upload a backup, or start the soak.

---

## Boot receipt (Batch 0)

The power-authority lane writes:

```json
{
  "schema_version": 1,
  "kind": "alpha_boot_green",
  "status": "PASS",
  "binding_status": "BOUND",
  "lane": "VENDOR_WALL_UPPER_BOUND",
  "commit": "<40 lowercase hex of the image that booted>",
  "finished_at": "<RFC3339 Z>"
}
```

Default path: `evidence/state/alpha-boot-green.json`.
`scripts/alpha/lib.sh` accepts `kind` `alpha_boot_green` or
`release_image_boot`, but only with `binding_status=BOUND`,
`status=PASS`, and `commit` equal to the candidate HEAD.

---

## P1-STAGING

**Blocker (quoted):** "No persistent private TLS staging endpoint or persistent data plane is available."

**Exit criterion (quoted):** "Deploy the exact candidate digest to the supplied host/DNS/TLS boundary and pass external health, source identity, auth, storage, lifecycle, and both workload probes."

**Host as given:** droplet `192.241.134.31`, DNS `mercmerc.net` → that
address. `merc-postgres-1` and `merc-minio-1` are already UP and
healthy. Control is DOWN. Nothing on 80/443. Cloudflare wrangler is
authed. Use the existing data plane; do not start a second Postgres or
MinIO.

**Scaffold:**

- `scripts/alpha/deploy.sh --print-runbook` — exact ssh/docker steps
- `scripts/alpha/deploy.sh --check` — local artifacts + boot + no live Stripe
- `scripts/alpha/probes.sh --execute` — HTTPS checklist
- `scripts/alpha/deploy.sh --record-pass` — after probes PASS

`deploy.sh` never invokes `ssh` or `rsync`. Do **not** run
`scripts/go-closure-deploy.sh --target ssh --execute` against this
droplet: that compose starts its own postgres/minio and will fight
`merc-postgres-1` / `merc-minio-1`.

**Supervisor steps (summary; full text is the runbook):**

1. Confirm boot-green.
2. On this Mac (OrbStack): build `Dockerfile.control` for
   `linux/amd64` with `MERC_BUILD_COMMIT=$(git rev-parse HEAD)`.
   Tree must be clean or `/version` lies.
3. `docker save | gzip | ssh root@192.241.134.31 'gunzip | docker load'`.
4. Droplet `.env` (`chmod 600`): `MERC_PAYMENT_MODE=test`,
   `MERC_PAYMENT_PROVIDER=stripe`, `MERC_CANARY_MODE=true`,
   `MERC_PAYOUT_EXPORT` unset, `MERC_TOKEN_KEY` **byte-identical** to
   the value already sealing the database.
5. `docker compose -f docker-compose.smallhost.yml -f docker-compose.canary.yml up -d --no-deps --no-recreate postgres minio`
   then `up -d --no-deps control caddy prometheus alertmanager`.
   Confirm postgres/minio `StartedAt` did not change.
6. TLS: grey-cloud A records so Caddy HTTP-01 can issue for
   `mercmerc.net` and `storage.mercmerc.net`. Orange-cloud is optional
   and requires Full (strict) + an origin cert; do not mix Flexible.
7. Probes from this Mac (see below).
8. `--record-pass`.

**Probes (`scripts/alpha/probes.sh`):**

| Probe | How |
|---|---|
| health | `GET /healthz` |
| source identity | `GET /version` — commit matches candidate, `modified=false`, price-board digest present |
| ready | `GET /readyz` — `status=ready`, `payment_mode=test`, `live_value_movement=false` |
| auth | anonymous `POST /v1/jobs` → 401 |
| storage | `GET https://storage.mercmerc.net/minio/health/live` |
| TLS | `openssl s_client` hostname check |
| lifecycle | `POST /v1/quote` + cancel |
| both workloads | completion of embed + batch_infer is corroborated by the canary counts (20+20) once the two Metal workers are enrolled; the staging probe refuses to stamp if quote/cancel cannot run |

---

## P1-STRIPE-TEST  (Batch 2, parallel)

**Blocker (quoted):** "The full Stripe test-mode payment, refund, dispute, Connect restriction, payout hold/release/failure, replay/out-of-order webhook, and provider reconciliation matrix has not run."

**Exit criterion (quoted):** "Execute every scenario with sk_test/whsec test credentials and distinct endpoint secrets; preserve redacted provider IDs and reconciliation receipts."

**Scaffold:** `scripts/alpha/stripe-test.sh` wraps
`scripts/stripe-sandbox.sh` (and therefore
`scripts/stripe-sandbox-scenarios.sh`). `--check` classifies keys
locally and never opens `api.stripe.com`. `--execute` is supervisor.

**Bind to the deployed candidate.** Webhook endpoints must already be
the staging URLs:

- `https://<STAGING_TLS_HOSTNAME>/v1/stripe/webhook`
- `https://<STAGING_TLS_HOSTNAME>/v1/stripe/connect-webhook`

Billing and Connect `whsec_` / `we_` values must differ. Settlement
currency is `cad`. Distinct test Connect account with payouts enabled.

**Supervisor:**

```bash
scripts/alpha/stripe-test.sh --check
scripts/alpha/stripe-test.sh --execute
# equivalent: make stripe-check && make stripe-matrix
```

---

## P1-OFFSITE-RESTORE  (Batch 2, parallel)

**Blocker (quoted):** "No encrypted backup has been uploaded across an independent provider/credential boundary and restored from an independently downloaded copy."

**Exit criterion (quoted):** "Upload only ciphertext, independently download/decrypt in isolation, restore database and objects, and match checksums plus application/ledger invariants."

**Scaffold:** `scripts/offsite-independent-restore.sh` (`make offsite-independent-restore`).
The older `scripts/alpha/offsite-restore.sh` runbook remains as a
supervisor/self-contained split around `backup.sh` / `restore.sh`.

Independent boundary: already-configured Cloudflare R2
(`.merc-secrets.env` `R2_*` keys), **not** the droplet MinIO. The
rehearsal mints a throwaway age identity on the verifying host, uploads
only ciphertext, destroys the isolated source, then independently
downloads and restores into a new isolated pair. It never targets
`merc-postgres-1` or `merc_pgdata`.

`--check` refuses a loopback/MinIO endpoint and does not dump.
`--execute` is the full isolated rehearsal and writes the two
`evidence/external/offsite-*.json` receipts.

---

## P1-ALERT-DELIVERY  (Batch 2, parallel)

**Blocker (quoted):** "A private Alertmanager→HTTP sink fire/resolve receipt now passes, but no external staffed paging receiver and acknowledgement route has been exercised."

**Exit criterion (quoted):** "Preserve redacted external receiver delivery IDs and firing, acknowledgement, deduplication, and resolution timestamps."

**Scaffold:** `scripts/alpha/alert-sink.sh`.

`scripts/test-alert-delivery.sh` remains the *local* sink proof. It
does not close this gate. The production wiring is already
`ops/monitoring/alertmanager.yml` → `url_file` →
`MERC_ALERT_RECEIVER_URL_FILE`. That file must contain one `https://`
URL that pages a human (PagerDuty / Opsgenie / staffed webhook), not
`127.0.0.1` and not the droplet.

`--check` rejects localhost, `host.docker.internal`, and
`192.241.134.31`. `--execute` posts fire+resolve to Alertmanager
(loopback on the droplet — supervisor) and waits on
`MERC_CANARY_ALERT_SINK_FILE` JSONL for distinct external
`event_id`s plus an acknowledgement timestamp.

---

## P1-CANARY-REHEARSAL  (Batch 2, parallel)

**Blocker (quoted):** "Two approved synthetic buyers, two operator-controlled Metal workers, and the strict scenario/restart adapters are unavailable."

**Exit criterion (quoted):** "Exercise the complete counted scenario matrix within every fail-closed limit, including kill switches, revocations, restore, alert, reconciliation, result retrieval during intake pause, and no payout export."

**Two Metal devices (not RunPod):**

| Role | Hardware | How it stays up |
|---|---|---|
| `studio` | this Mac Studio, 28/60 M3 Ultra | `merc-agent run` locally, seatbelt on |
| `laptop` | operator MacBook | `merc-agent` headless, supervisor via screen-share |

Each device generates its own P-256 enrollment key under
`~/.merc/enrollment`. The private key never leaves that directory.
Containment: `MERC_SANDBOX_PROFILE=clients/macapp/ComputeExchangeAgent/merc-agent.sb`.
Do not set `MERC_ALLOW_UNSANDBOXED`. Worker IDs must be real v4 UUIDs
(version nibble 1–5). Demo `0000-…` IDs are refused.

```bash
scripts/alpha/enrol-worker.sh --device studio --print-runbook
scripts/alpha/enrol-worker.sh --device laptop --print-runbook
```

**Counted matrix** is still `scripts/canary-scenario-driver.sh` /
`scripts/go-closure-canary-rehearsal.sh` (2 / 2 / 20 / 20 / 5 / 5 / 3 /
3 / 3 / 1 / 1 / 1 / 1 / 1).

**Extra adapters** in `scripts/alpha/scenarios.sh` close the exit
criterion items the counted driver does not name:

| Adapter | What it proves |
|---|---|
| `kill_switch` | `POST /admin/controls/dispatch` pause then resume |
| `revocation` | `DELETE /v1/supplier/worker-credentials/{id}` |
| `intake_pause_result_retrieval` | pause intake, `GET /v1/jobs/{id}/results` still answers, resume |
| `no_payout_export` | `MERC_PAYOUT_EXPORT` unset and `/readyz` is test / no live value |

```bash
scripts/alpha/canary-rehearsal.sh --check
scripts/alpha/canary-rehearsal.sh --execute
```

`--execute` runs the extra adapters, then the counted matrix if
`MERC_CANARY_SCENARIO_DRIVER` is pinned to a reviewed SHA-256.

---

## P1-RECOVERY-SOAK  (Batch 3, RUN-LAST)

**Blocker (quoted):** "Local source-equivalent rollback/forward, seeded restart-storm, and continuous 15-minute/two-hour soak receipts pass, but they do not substitute for a qualifying 24-hour run against persistent staging state and two distinct external Metal devices."

**Exit criterion (quoted):** "Run the exact published candidate and retained prior digest against persistent staging, preserve source/state/restart receipts, and complete the configured 24-hour invariant and SLO window on two distinct external devices."

**Do not start it from this worktree.**

Inside this gate, in order:

1. **Rollback / forward** (supervisor): swap only the control image to
   the retained prior digest, probe, then forward to the candidate.
   Postgres and MinIO stay up.
2. **Restart-storm** (supervisor + both Metal devices): both
   `merc-agent` processes take a durable session transition.
   `scripts/go-closure-restart-storm.sh` if the isolated stack is in
   use; otherwise `kill -TERM` each agent and confirm
   `agent_session_id` changed.
3. **24h soak** against persistent staging, both devices heartbeating.

**Exact start command:**

```bash
export MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR=1
scripts/alpha/soak.sh --execute --duration 86400 --interval 60
```

`--execute` without that env var is refused. Duration `< 86400` cannot
qualify. The sampler is external HTTPS (`/version` + `/readyz` every
interval) from this Mac; the laptop's job is to stay enrolled. Isolated
alternative (not this droplet's existing data plane):

```bash
scripts/go-closure-soak.sh --target ssh --duration 86400 --interval 60 --execute
```

Print the command any time:

```bash
scripts/alpha/soak.sh --print-start-command
```

---

## P1-GOVERNANCE  (operator, parallel with Batch 2)

**Blocker (quoted):** "Qualified approvals for the bounded test canary and named human incident coverage are absent."

**Exit criterion (quoted):** "Supply an exact-candidate governance bundle conforming to ops/governance-approval-bundle.schema.json with all eight named approvals."

Operator *is* governance. The eight names in the schema are: security,
privacy, legal, licensing, payments, operations, supplier_policy,
release_approval. Plus the five exercises: support tabletop, security
tabletop, DSAR export/deletion, backup tombstone, asset/model
provenance.

Not a soak prerequisite. Required before claiming the Level B decision
is `GO`. `make approvals-check` once
`GOVERNANCE_APPROVAL_BUNDLE_PATH` points at the filled bundle bound to
the exact 40-hex candidate commit.

---

## P1-INDEPENDENT-APPROVAL

Status is whatever `ops/go-no-go.json` records. `scripts/alpha/lib.sh`
reads that file and does not independently drop or pass this gate. This
plan does not decide it.

**Exit criterion (quoted from the current ledger):** "Named eligible reviewer independently inspects evidence and grants the required exact-head approval with no unresolved thread."

If the ledger lists the id under `open_p1`, it is open. If the operator
later moves it to `dropped_p1`, the scripts follow. Do not fork a second
decision here or in lib.sh.

---

## Script index

| Script | Default | Mutates? |
|---|---|---|
| `scripts/alpha/status.sh` | one-screen checklist | no |
| `scripts/alpha/validate-scaffold.sh` | syntax + contract | no |
| `scripts/alpha/deploy.sh` | `--print-runbook` | no (never ssh) |
| `scripts/alpha/probes.sh` | `--print-runbook` | `--execute` is HTTPS only |
| `scripts/alpha/stripe-test.sh` | `--print-runbook` | `--execute` calls Stripe TEST |
| `scripts/alpha/offsite-restore.sh` | `--print-runbook` | `--execute-restore` downloads |
| `scripts/alpha/alert-sink.sh` | `--print-runbook` | `--execute` fires a page |
| `scripts/alpha/enrol-worker.sh` | `--print-runbook` | no |
| `scripts/alpha/canary-rehearsal.sh` | `--print-runbook` | `--execute` runs the matrix |
| `scripts/alpha/scenarios.sh` | extra adapters | only when invoked |
| `scripts/alpha/soak.sh` | `--print-start-command` | `--execute` is 24h and refused without the supervisor env |
| `scripts/alpha/lib.sh` | sourced | no |

Existing authority these wrap, not replace:
`scripts/stripe-sandbox.sh`, `scripts/backup.sh`, `scripts/restore.sh`,
`scripts/canary-scenario-driver.sh`, `scripts/go-closure-*.sh`,
`scripts/test-alert-delivery.sh` (local only).

---

## Supervisor batch cheat-sheet

```bash
# 0. Watch boot
scripts/alpha/status.sh

# 1. After boot-green
scripts/alpha/deploy.sh --check
scripts/alpha/deploy.sh --print-runbook    # then ssh as written
scripts/alpha/probes.sh --execute
scripts/alpha/deploy.sh --record-pass

# 2. Parallel
scripts/alpha/stripe-test.sh --check && scripts/alpha/stripe-test.sh --execute
scripts/alpha/offsite-restore.sh --check   # supervisor: backup.sh ; then:
scripts/alpha/offsite-restore.sh --execute-restore
scripts/alpha/offsite-restore.sh --record-pass
scripts/alpha/alert-sink.sh --check && scripts/alpha/alert-sink.sh --execute
scripts/alpha/enrol-worker.sh --device studio --print-runbook
scripts/alpha/enrol-worker.sh --device laptop --print-runbook
scripts/alpha/canary-rehearsal.sh --check && scripts/alpha/canary-rehearsal.sh --execute

# 3. LAST
scripts/alpha/soak.sh --print-start-command
# only then:
export MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR=1
scripts/alpha/soak.sh --execute --duration 86400 --interval 60
```
