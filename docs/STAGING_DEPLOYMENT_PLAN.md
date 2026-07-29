# Staging deployment command plan

One pass, in order. Every manual value is in Part 0; everything after it is a
command with the check that proves it worked.

**Scope: Milestone A only** — the marketplace lanes that have real evidence
today (realtime and batch inference, embeddings, verification, billing, refund,
settlement, backup, alerting). Image generation, LoRA train/evaluate/deploy,
tensor-parallel multi-GPU, external CUDA execution and broad model onboarding
are **not** in this plan and are not claimed by it. Those are Milestone B and
need their own real-runtime and canary receipts.

Nothing here authorises live value movement. The control runtime is pinned to
`MERC_PAYMENT_MODE=test`; the rehearsal refuses anything but a test Stripe key.

---

## Part 0 — values only you can supply

Copy the template, fill every placeholder, lock it down. Do not commit it, do
not paste it into any agent transcript, and place it on the staging host through
a separate secure channel — `go-closure-deploy.sh` deliberately never transfers it.

```bash
cp ops/staging/env.go-closure.example .env.go-closure
chmod 600 .env.go-closure
```

All 45 placeholders live in that one file. Grouped by what they actually are:

**Host and TLS** — `STAGING_SSH_TARGET`, `STAGING_TLS_HOSTNAME`,
`STAGING_STORAGE_TLS_HOSTNAME`, `STAGING_BIND_ADDRESS` (one specific IPv4; a
wildcard bind is rejected), `STAGING_DEPLOYMENT_ROOT`, `ACME_EMAIL`.

**Immutable images and their commits** — `MERC_CANDIDATE_CONTROL_IMAGE`,
`MERC_CANDIDATE_COMMIT`, `MERC_PRIOR_CONTROL_IMAGE`, `MERC_PRIOR_COMMIT`,
`MERC_PROMETHEUS_IMAGE`, `MERC_ALERTMANAGER_IMAGE`, `MERC_GRAFANA_IMAGE`,
`MERC_NODE_EXPORTER_IMAGE`. Every one must be `registry/repo@sha256:<64 hex>`.
A floating tag is refused: receipts bind image identity, not a name.

**Off-host backup** — `MERC_BACKUP_OFFSITE` (a bucket on *different*
infrastructure from the deployment), `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY` (restricted to that bucket; do not reuse the artifact
store keys), `MERC_BACKUP_ENCRYPTION_RECIPIENT`,
`MERC_BACKUP_DECRYPTION_IDENTITY_FILE` (the `age` private identity — keep it off
the deployment host).

**Stripe, test mode only** — `STRIPE_SECRET_KEY` (`sk_test_*` / `rk_test_*`;
live prefixes are refused before any network call), `STRIPE_WEBHOOK_SECRET`,
`MERC_CONNECT_WEBHOOK_SECRET` (must differ from the billing one),
`MERC_CONNECT_CLIENT_ID`, `STRIPE_TEST_CONNECTED_ACCOUNT_ID`,
`STRIPE_BILLING_WEBHOOK_ENDPOINT_ID`, `STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID`.

**Pricing authority** — `MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE` (CAD per USD,
operator-approved), `MERC_PRICE_FX_REVISION` (immutable source identifier).
The control plane refuses to boot without both, rather than reusing USD numbers
as CAD.

**Alerting** — `ALERT_RECEIVER_WEBHOOK_URL` (HTTPS, and it must actually page a
human), `ALERT_RECEIVER_NAME`.

**Approved participants** — `MERC_CANARY_APPROVED_BUYER_EMAILS` (exactly two),
`MERC_CANARY_APPROVED_WORKER_IDS` (exactly two, **real v4 UUIDs** — the demo
seed IDs are version-nibble `0` and the receipt validator correctly refuses
them), `MERC_CANARY_APPROVED_AGENT_VERSIONS`, `MERC_CANARY_APPROVED_BUILD_HASHES`.

**Reviewed drivers** — `MERC_CANARY_SCENARIO_DRIVER` and
`MERC_CANARY_APPROVED_DRIVER_SHA`, `MERC_AGENT_RESTART_DRIVER` and
`MERC_AGENT_RESTART_APPROVED_DRIVER_SHA`. Read the driver, then pin its digest;
the rehearsal re-checks the bytes before and after every scenario.

**Secrets that must be minted, never defaulted** — `POSTGRES_PASSWORD`,
`MINIO_ROOT_USER`, `MINIO_ROOT_PASSWORD`, `GF_SECURITY_ADMIN_PASSWORD`,
`MERC_VERIFICATION_SAMPLE_SECRET` (≥32 unpredictable bytes; a predictable value
tells a supplier when it is unobserved), and:

> **`MERC_TOKEN_KEY` — copy byte-identically, never regenerate.**
> `control/crypto.go` derives the AES key as `sha256(value)`. Against an
> existing database, a new value makes every sealed OAuth token and webhook
> secret permanently undecryptable. Back it up separately from the database,
> before step 3.

**Soak bound** — `MERC_SOAK_MAX_RSS_GROWTH_BYTES`.

---

## Part 1 — validate before touching the host

```bash
scripts/validate-go-closure-scaffold.sh
```
*Verifies:* compose pins every image by digest, no `build:` key, settlement bound
to `cad`, payment mode `test`, `.env.go-closure` gitignored, every scenario and
minimum present. Ends `PASS GO-closure staging scaffold`.

```bash
set -a; . ./.env.go-closure; set +a
bash scripts/stripe-sandbox.sh check
```
*Verifies:* `api_key: test`, both webhooks present and distinct, connect account
present, `staging_hostname_valid: true`, `live_mode: PROHIBITED`. The hostname
field is the one that is false today.

```bash
make release-doctor
make approvals-check
make credentials-check
```

**Gate:** all four PASS. Do not proceed on a partial.

---

## Part 2 — deploy the candidate

```bash
scripts/go-closure-deploy.sh --target ssh --activate candidate --check
scripts/go-closure-deploy.sh --target ssh --activate candidate --execute
```
*Verifies:* pulls and inspects both image digests before activation, syncs only
non-secret manifests, refuses if `.env.go-closure` is missing on the host.

```bash
curl -sf https://$STAGING_TLS_HOSTNAME/healthz
curl -s  https://$STAGING_TLS_HOSTNAME/readyz | jq '{status,payment_mode,live_value_movement}'
```
**Gate:** `status: ready`, `payment_mode: test`, `live_value_movement: false`.
If `readyz` is 503, the body still names the failing condition — read it rather
than retrying.

---

## Part 3 — prove the token key survived

Do this **before** any money flows, while the database is still disposable.

```bash
ssh $STAGING_SSH_TARGET 'cd '"$STAGING_DEPLOYMENT_ROOT"' && \
  docker compose -f compose.go-closure.yml exec -T postgres \
    psql -U cx -d cx -tAc \
    "SELECT count(*) FROM webhooks WHERE signing_secret_sealed NOT LIKE '"'"'enc:%'"'"'"'
```
*Verifies:* must be `0`. The schema constrains sealed webhook secrets to the
`enc:` form, and `sealToken` falls back to a `plain:` prefix when
`MERC_TOKEN_KEY` is absent — a non-zero count means secrets were written
unsealed.

Then confirm existing sealed values actually decrypt under the key now in use,
by re-registering a webhook for a job that already has one: `store.go` returns
the *existing* secret when it can be opened, and regenerates when it cannot.

```bash
curl -sf -X POST https://$STAGING_TLS_HOSTNAME/v1/webhooks \
  -H "Authorization: Bearer $BUYER_API_KEY" -H 'Content-Type: application/json' \
  -d "{\"job_id\":\"$KNOWN_JOB_ID\",\"url\":\"https://$WEBHOOK_PROBE_HOST/probe\"}" \
  | jq -r '.webhook_secret' | head -c 8
```
*Verifies:* the same secret prefix comes back across two calls. A changed secret
means the key does not match the database — stop and restore the original key.
Recovering after real traffic means re-registering every webhook.

---

## Part 4 — off-host backup and a real restore

```bash
scripts/go-closure-rollback-rehearsal.sh --target ssh --execute
```
*Verifies:* runs `backup.sh` to the offsite bucket, independently re-downloads
both ciphertext and manifest, compares both hashes, restores into an isolated
database, and **starts a control plane against the restored data**. A backup that
cannot boot is not a backup.

**Gate:** a schema-v2 receipt under `evidence/` with the offsite URI and both
digests.

---

## Part 5 — paging that reaches a human

```bash
make alert-delivery-test
make alert-page
```
*Verifies:* fires a synthetic critical alert through Alertmanager to the real
receiver and requires the receiver to record both firing **and** resolution.

**Gate:** a phone actually buzzed, and the receipt shows the matched pair. A sink
that silently accepts the webhook proves nothing.

---

## Part 6 — enrol real participants

Two approved buyers, two workers with genuine v4 UUIDs. At least one worker must
be independent of this Mac — preferably RunPod/CUDA, which also starts the
Milestone B evidence.

```bash
ssh $STAGING_SSH_TARGET 'cd '"$STAGING_DEPLOYMENT_ROOT"' && \
  docker compose -f compose.go-closure.yml exec -T postgres \
    psql -U cx -d cx -tAc \
    "SELECT id, hw_class, agent_version, last_seen_at
       FROM workers WHERE last_seen_at > now() - interval '"'"'90 seconds'"'"'"'
```
**Gate:** exactly two workers, both heartbeating, both UUIDs version-nibble 1–5,
agent versions and build hashes matching the approved lists.

---

## Part 7 — the rehearsal

```bash
scripts/go-closure-canary-rehearsal.sh --target ssh --check
scripts/go-closure-canary-rehearsal.sh --target ssh --execute
```

Fourteen scenarios, each producing a schema-v2 receipt validated against
`scripts/validate-canary-scenario-receipt.py` and independently corroborated
against PostgreSQL. Minimums are 20 embed, 20 batch_infer, 5 cancel, 5 retry,
3 stale lease, 3 stale attempt, 3 webhook retry, and one each of backup, Stripe
matrix, real alert, invariant audit and retry-backoff audit.

Expect failures on the first run. Every diagnostic names exactly what it could
not observe — fix the cause, do not relax the scenario. A scenario that cannot
observe its work **must** fail closed; that is the property the last two review
rounds were spent establishing.

```bash
scripts/go-closure-restart-storm.sh --target ssh --execute
scripts/go-closure-soak.sh --target ssh --duration 86400 --execute
```

**Gate:** all fourteen scenarios PASS, restart storm PASS, 24-hour soak PASS with
RSS growth inside `MERC_SOAK_MAX_RSS_GROWTH_BYTES`, zero firing page alerts at the
end, and a post-run database with no open terminal tasks and a zero ledger sum.

---

## Part 8 — preserve the receipts

```bash
python3 scripts/validate-go-closure-evidence-chain.py
python3 scripts/private-canary.py
git add evidence/ && git commit -m "go-closure staging candidate receipts"
```
**Gate:** `lanes_canary_proven` moves off zero and the evidence chain verifies
against the exact candidate commit and image.

---

## After this

Return immediately to RunPod CUDA, vLLM, image generation, LoRA and
tensor-parallel. Those lanes are unproven, and Milestone A does not make them
true.

**Do not start the minimum-LOC refactor after this rehearsal.** Start it when
either the full intended product is canary-proven, or you consciously freeze a
smaller launch scope and remove the unproven lanes from what Merc promises.
