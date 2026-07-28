# Canary scenario driver

`scripts/canary-scenario-driver.sh` is the repository-bundled adapter that the
GO-closure canary rehearsal invokes to mint candidate-bound, schema-v2 scenario
receipts. It is **not** release authority. The rehearsal still:

1. SHA-pins the driver bytes to `MERC_CANARY_APPROVED_DRIVER_SHA256`
2. Validates every receipt with `scripts/validate-canary-scenario-receipt.py`
3. Independently re-queries PostgreSQL for database-backed subjects
4. Checks final Prometheus and ledger state

If the work did not happen, the driver exits non-zero and prints **no** receipt
on stdout.

## Invocation

```text
"$MERC_CANARY_SCENARIO_DRIVER" run <scenario> <minimum>
```

One JSON document on stdout, `status: "PASS"`, `observed == minimum == len(evidence)`.

### Environment the rehearsal provides

| Variable | Role |
| --- | --- |
| `MERC_CANARY_RUN_ID` | 32 lowercase hex run id |
| `MERC_CANARY_RUN_STARTED_AT` | UTC start of the whole canary run |
| `MERC_CANARY_CANDIDATE_COMMIT` | 40 lowercase hex candidate |
| `MERC_CANARY_CONTROL_IMAGE` | immutable `registry/repo@sha256:…` |
| `MERC_CANARY_DRIVER_SHA256` | SHA-256 of the driver bytes |
| `MERC_CANARY_SCENARIO` | scenario name (must match argv) |
| `MERC_CANARY_SCENARIO_MINIMUM` | exact observation count |
| `MERC_CANARY_SCENARIO_STARTED_AT` | UTC start of this scenario window |

### Operator / host environment

| Variable | Role |
| --- | --- |
| `MERC_CONTROL_BASE_URL` or `STAGING_TLS_HOSTNAME` | control plane base URL |
| `MERC_CANARY_DATABASE_URL` (or `DATABASE_URL` / `MERC_TEST_DATABASE_URL`) | **read-only** observation DB |
| `MERC_CANARY_APPROVED_BUYER_EMAILS` | exactly two distinct approved buyers |
| `MERC_CANARY_APPROVED_WORKER_IDS` | exactly two approved worker UUIDs |
| `MERC_CANARY_APPROVED_AGENT_VERSIONS` | reviewed agent semvers |
| `MERC_CANARY_APPROVED_BUILD_HASHES` | reviewed 16-hex build hashes |
| `MERC_CANARY_BUYER_API_KEYS` | `email=key,…` or ordered keys |
| `MERC_CANARY_WORKER_TOKENS` | `uuid=token,…` or ordered tokens |
| `MERC_CANARY_ADMIN_API_KEY` | admin routes when needed |
| Stripe test keys + webhook endpoint IDs | `stripe_test_matrix` |
| `MERC_BACKUP_OFFSITE`, age recipient/identity | `backup_independent_restore` |
| `ALERT_RECEIVER_*`, `MERC_CANARY_ALERT_SINK_FILE` | `real_alert_firing_resolution` |
| `MERC_CANARY_WEBHOOK_URL` | public HTTPS buyer webhook target |

Live Stripe credential classes (`sk_live_*`, `rk_live_*`, `pk_live_*`) are refused
before any network call.

## What each scenario proves

| Scenario | Real work | Observes | Needs running |
| --- | --- | --- | --- |
| `approved_buyer_identity` | `GET /v1/me` for each approved buyer | durable buyer UUIDs | control + DB + two approved buyers with API keys |
| `distinct_metal_agent` | read workers heartbeating in-run | approved Metal worker UUIDs | two reviewed Metal agents online with approved version/build |
| `embed_success` | submit embed jobs via public API; wait complete | job UUIDs with positive `actual_usd` | control + DB + Metal agents + buyer credit |
| `batch_infer_success` | submit batch_infer jobs; wait complete | job UUIDs | same + batch_infer model path |
| `cancelled_job` | submit then `DELETE /v1/jobs/{id}` | cancelled job UUIDs | control + DB + buyer key |
| `forced_retry` | claim task, `POST …/fail` with retryable class | `task_requeued` job_event UUIDs | control + live approved worker + worker token |
| `stale_lease_recovery` | submit, wait for dead-claim / stuck rescue | rescue job_event UUIDs | worker that stops heartbeating past `deadWorkerAfter` (~180s) |
| `stale_attempt_commit_rejection` | advance attempt, commit stale epoch | HTTP 409 + unchanged state digests | control + worker token + DB |
| `buyer_webhook_retry_sequence` | register webhook, complete job, wait retries | webhook UUIDs with `attempts>=2` + delivery | public HTTPS webhook URL (SSRF guard blocks localhost) |
| `backup_independent_restore` | offsite encrypted backup + independent restore | offsite provider subject | `MERC_BACKUP_OFFSITE`, age keys, backup/restore scripts |
| `stripe_test_matrix` | bundled Stripe sandbox matrix | Stripe test-mode subject | staging TLS hostname, test keys, endpoint IDs, fixture `pi_`/`ch_`/`tr_` |
| `real_alert_firing_resolution` | fire + resolve via Alertmanager | distinct receiver event IDs | Alertmanager + real receiver sink file |
| `post_rehearsal_invariant_audit` | read-only SQL invariant map | clean invariant map | control DB |
| `bounded_retry_backoff_audit` | max `retry_count` ≤ policy ceiling | policy flags | control DB (Prometheus optional) |

## Operator review and SHA pin

1. Read this document and the driver source.
2. Place a **physical, non-symlink** copy of the reviewed executable on the
   staging host in a non-group/world-writable directory.
3. `sha256sum` (or `shasum -a 256`) the bytes; set
   `MERC_CANARY_APPROVED_DRIVER_SHA256` to that 64 lowercase hex digest in
   `.env.go-closure`.
4. Point `MERC_CANARY_SCENARIO_DRIVER` at the absolute physical path.
5. `scripts/go-closure-canary-rehearsal.sh --target local|ssh --check` admits the
   pin without running scenarios; `--execute` runs all fourteen.

The rehearsal re-hashes the driver before every scenario and after the run. Any
byte change fails closed.

## Local test

```bash
# control plane + DB + at least one Metal agent required for workload scenarios
bash scripts/test-canary-scenario-driver.sh
```

The test:

- refuses live Stripe prefixes and missing control base (empty stdout)
- refuses `backup_independent_restore` without offsite config
- runs each scenario against the local control plane
- validates any PASS receipt with `validate-canary-scenario-receipt.py`
- requires identity + invariant audits to PASS; workload scenarios PASS when
  agents are live and fail closed (honestly) when not

## What this driver does **not** prove

- **Not release authority.** Only the full GO-closure canary rehearsal receipt
  (plus restart storm, rollback, and 24h soak) can support a GO decision.
- **Not live money.** Stripe stays test-mode; `real_value` is always false.
- **Not public canary or self-serve suppliers.** Only approved buyer emails and
  worker UUIDs participate.
- **Not a substitute for database corroboration.** Evidence that exists only in
  driver memory is a defect; Merc re-queries PostgreSQL.
- **Not coverage of RunPod / CUDA lanes.** Missing staging capabilities fail
  closed with a precise diagnostic; they are not papered over.
- **Not offsite backup** when only a local Docker restore is available. The
  observation source is `offsite_backup_provider`.
- **Not alert receiver proof** without observed firing/resolved event IDs from
  the real receiver sink.
- **Not secret safety if you paste secrets into env diagnostics.** The receipt
  path refuses secret-shaped strings; operator logs are still your responsibility.
- **Not multi-scenario subject uniqueness enforcement across process restarts.**
  Each scenario creates fresh subjects; do not hand-craft overlapping IDs.

## Threat model (summary)

1. A PASS is impossible without the work actually happening.
2. `observed == minimum == len(evidence)` exactly; no pad, truncate, or dedupe.
3. Every evidence entry must be re-findable by the control plane / provider.
4. No secret values in any receipt field.
5. Stripe test-mode only; live key prefixes refused first.
6. Only approved participants.
7. Timestamps bracket real work inside the invocation window.
8. Subject IDs unique within a receipt; keep them unique across the run too.
