# Operator runbooks

All examples assume an explicit environment file, an explicit database URL, and
a current backup. Never operate on a database whose target you have not resolved.

## Deploy

1. Verify `make ci` and `make prove-local` at the exact source revision.
2. Back up PostgreSQL and the artifact bucket.
3. Build the control image and native signed agent from that revision.
4. Apply `control/schema.sql` in one transaction. Applying it twice must succeed.
5. Roll out the control plane, check `/readyz`, then roll out agents gradually.
6. Inspect queue age, task failures, verification, ledger drift, and payout holds.

The control binary also applies its embedded schema under an advisory migration
lock. The checked-in SQL file and embedded bytes are the same authority.

## Backup and restore

Create a database dump with `make backup` or an equivalent `pg_dump` invocation.
Record the source revision, schema hash, bucket version, and encryption-key version
beside the backup. Do not store secrets in the repository.

Restore into a new database first:

```bash
createdb cx_restore
pg_restore --exit-on-error --clean --if-exists \
  --dbname=postgres://cx@localhost/cx_restore backup.dump
psql postgres://cx@localhost/cx_restore -v ON_ERROR_STOP=1 \
  -f control/schema.sql
```

Start one control instance against the restored target, check readiness and
ledger reconciliation, then switch traffic. Artifact objects must be restored or
version-rewound to the matching backup boundary.

## Roll back

Schema changes are additive and idempotent. To roll back application behavior:

1. stop new submissions or put the service in maintenance mode;
2. allow active commits to drain or expire leases;
3. deploy the previous control and agent artifacts built from the recorded source;
4. leave additive schema objects in place unless a separately reviewed data
   migration proves removal safe;
5. re-run readiness, quote, submit, cancellation, verification, and ledger checks.

Do not reverse settled ledger rows. Use compensating entries with stable
idempotency keys. Do not delete artifacts referenced by completed jobs.

## Queue recovery

- A dead agent is recovered by lease expiry and transactional requeue.
- Before manual requeue, inspect task state, lease owner, attempt count, result
  object, commit record, and ledger effects.
- Use the authenticated requeue endpoint once. Repeated action is audited and must
  remain idempotent.
- Suspend a suspect worker before releasing its work. Reinstatement is a separate
  audited operator action.

## Money incidents

Freeze payout release before investigation. Compare job charge, supplier earning,
platform fee, subsidy, refund, and dispute effects by their source IDs. Search for
duplicate `(task_id, kind)` effects and verify the global ledger sum. Repair with a
new compensating entry; never mutate history. Resume payouts only after the drift
query is clean and the incident source is bounded.

## Secret rotation

- Buyer/admin keys and worker credentials: mint a replacement, verify it, revoke
  the old credential, then inspect the credential audit.
- Worker enrollment: revoke unused codes and active credentials independently.
- Webhook secrets: exact re-registration creates a new sealed secret.
- Token-encryption key: deploy dual-read/single-write rotation before removing the
  prior key.
- Verification-sampling secret: rotate only at a documented task boundary because
  it changes deterministic sampling decisions.

## Storage or database outage

On database loss, stop admission because lifecycle and money authority are
unavailable. On storage loss, keep tasks from being claimed and do not fabricate
completion from database state alone. After recovery, reconcile missing input and
result objects against task rows before resuming dispatch.

## Evidence

`scripts/prove-local.sh` writes source-bound JSONL receipts to
`.artifacts/prove-local/ledger.jsonl`. Preserve the ledger, source revision,
binary hashes, census, and service logs together. `KEEP=1` retains disposable
services for inspection; shut them down explicitly after investigation.

## Control plane or database outage

Stop admission at the proxy if `/readyz` fails. Record `/version`, control logs,
PostgreSQL health and saturation, disk availability, and the first failed ticker;
do not restart repeatedly before preserving evidence. If PostgreSQL is unhealthy,
keep control stopped until the database is consistent. Restore into a new target
with `scripts/restore.sh <timestamp> --to cx_restore_YYYYMMDD`, compare table and
ledger counts, then move traffic. Never accept jobs while lifecycle authority is
ambiguous.

## Agent offline or task stall

Check `merc_active_workers`, typed task failures, thermal/memory throttle state, and
the device's `status.json`. An active task renews its lease with both task id and
attempt; a stale attempt cannot renew or commit. Let automatic dead-claim and
stale-task recovery act first. If manual action is required, suspend the worker,
inspect the task attempt and artifact state, then use the authenticated requeue
endpoint once. Reinstatement is a separate audited action.

## Queue stall and safe requeue

Break queue depth down by tier and workload, inspect `/admin/scheduler/explain`,
and verify an exact runtime/model/hardware match exists. Do not requeue a running
task merely because it is slow: confirm its worker heartbeat/lease is absent or
the execution deadline has passed. The requeue action increments the attempt
epoch, so delayed start/fail/commit calls from the previous execution are inert.

## Verification failure or bad-result dispute

Freeze payout release for the affected task and preserve primary, redundancy,
honeypot, tiebreak, result-hash, and object metadata. Confirm output cardinality,
shape, checksum, and runtime provenance before blaming the supplier. Use the
buyer dispute endpoint once; allow the verification worker to resolve it. Ban or
reputation changes require an attributed admin action. Repair buyer money only
with an append-only compensating effect.

For a recovery stall, compare `merc_verification_backlog`,
`merc_verification_expired_leases`,
`merc_verification_oldest_open_age_seconds`, and the
`verification-recovery` progress ticker. Check PostgreSQL pool saturation before
restarting anything. Cancellation returns owned leases to `pending`; confirm
that transition and preserve the original error before allowing another
recovery leader to claim them. Never edit a verification lease or terminal
verdict directly.

## Money incident or payout hold

Disable payout release, preserve Stripe event ids and idempotency keys, and query
the admin drift/payout views. Distinguish provider `outcome_unknown` from a failed
operation: resolve it by exact provider id before retrying. Compare buyer charge,
supplier liability, platform take, processor fee, subsidy, refund, and cash-moved
records. Never edit or delete settled ledger rows. Apply a reviewed compensating
entry with a stable source id, re-run reconciliation, then unfreeze only the
bounded affected scope.

## Webhook failure

Confirm the registered URL is still public HTTPS, DNS has not moved to a private
address, and the signing secret matches. Inspect attempts, lease expiry, response
class, redirect rejection, and dead-letter time. Re-registering the same job/URL
is idempotent and returns the same encrypted secret. Do not bypass SSRF controls;
have the buyer provide a compliant endpoint and replay from the durable outbox.

## Object-store failure or artifact corruption

Pause claims while inputs or result PUTs are unavailable. Compare database keys,
object size, content SHA-256, expected record count, and ownership prefix. Restore
objects from the backup at the same boundary as PostgreSQL. Never mark a task
complete based on a database row when the authoritative result object is absent
or fails verification.

## Insufficient capacity

Check exact runtime cell, model kind/reference, memory headroom, hardware class,
minimum reputation, geographic constraint, reservation price, and worker age.
Return a capacity error or leave work queued; do not silently route to an
unsupported runtime or weaken redundancy. Invite a compatible supplier or ask
the buyer to select a supported model/tier.

## Emergency secret rotation

Treat any committed or logged production secret as compromised. Revoke at the
provider first, rotate endpoint-specific webhook secrets independently, mint new
buyer/admin/worker credentials, and verify the new credential before revoking the
old one. Encryption-key rotation requires a dual-read/single-write rollout.
Verification-sampling key rotation requires a documented task boundary. Re-run
the history secret scan after remediation without printing recovered values.

## Backup and restore drill

`make restore-drill` creates isolated PostgreSQL and MinIO instances, loads a
production-shaped dataset (≥10k jobs, ≥1k ledger rows, ≥1k objects), takes a
checksummed custom-format dump and object mirror, restores both, compares
content hashes, and records a measured RTO in the receipt. The drill fails if
row counts fall below those minima so it cannot quietly regress to a toy
fixture. This proves the mechanism locally; production readiness additionally
requires a successful `scripts/backup.sh` upload to independent offsite storage
followed by `scripts/restore.sh` from that uploaded copy.

Daily backups are scheduled by `ops/systemd/merc-backup.timer` (install the unit
pair on the host, point `EnvironmentFile` at offsite creds and
`MERC_BACKUP_STATUS_FILE`). Hosts that previously ran the pre-rebrand units must
`systemctl disable --now cx-backup.timer` (and the matching `.service`) before
enabling `merc-backup.timer`, or the old timer keeps firing the old script path.
After a verified offsite upload, `scripts/backup.sh` atomically updates that
status file; control exports `merc_backup_age_seconds` for the 26h stale-backup
page.

## Rollback rehearsal

Keep at least the current and prior content-addressed control images on the
staging/production host. Run `scripts/rollback.sh <full-commit>` in staging, verify
public `/version`, `/readyz`, quote, idempotent submit, cancel, both workloads, and
ledger reconciliation, then redeploy the candidate. Schema rollback is forbidden;
all schema changes are additive and old binaries must be compatibility-tested.

## Alert mapping

`ops/monitoring/alerts.yml` maps each alert to these sections. Before release, load
the rules in the actual monitoring system, route `severity=page` to the on-call
receiver, fire one synthetic alert, acknowledge it, and record the delivery and
resolution timestamps. A checked-in rule that has never paged is not a proven
alert.

---

# Absorbed operator runbooks

Sections below were previously separate files. Headings and anchors in the original docs/RUNBOOKS.md body above are unchanged.


<!-- source: RUNBOOK_ARTIFACTS.md -->

# Artifact runbook

Status as of 2026-07-21: the batch lane has S3-compatible presigned input/output
transfers, checksums, bounded retries, partial-result support, and governed model
pins. A complete buyer-facing artifact control plane and a real vLLM cache load
are not proven.

## Batch artifact incident checks

1. Identify the project, job, task, object key, expected SHA-256, byte bound,
   media type, and presigned-URL expiry without logging URL credentials.
2. Check object-store health and whether the failure is upload, download,
   expiry, range-resume, checksum, size, or authorization related.
3. Keep failed verification-staging objects isolated. Do not promote an object
   whose exact bytes, size, and digest do not match the fenced work record.
4. Reissue only task-scoped, short-lived credentials. Never grant a worker
   bucket-wide credentials or a cross-project object prefix.
5. Preserve the authoritative content-addressed record and receipt evidence
   before retention or deletion actions.

## Realtime model-cache checks

The vLLM supervisor mounts the operator-selected cache directory into the
digest-pinned container and passes exact model and tokenizer revisions. On a
real worker, verify the host architecture, container manifest, downloaded file
digests, cache ownership and permissions, available capacity, load duration,
and post-load health before advertising the offer as ready. A successful
software command-construction test is not proof that model acquisition or CUDA
loading succeeded.

## Required next proof

Implement project-namespaced initiate/multipart/complete/abort upload, presigned
download, metadata, retention, deletion, and artifact references without
proxying large bytes through the control API. Exercise cache miss, corruption,
interrupted download, resume, eviction, expiry, and tenant-isolation failures.

| Layer | Status |
|---|---|
| Implemented | Batch presigned transfer path; vLLM cache mount contract |
| Tested | Existing batch S3 retry/range/checksum tests; supervisor command test |
| Real-runtime proven | Batch evidence only; realtime model cache no |
| Private-canary proven | No |
| Production proven | No |
| Externally blocked | Object-store deployment for new API and Linux CUDA worker |


<!-- source: RUNBOOK_WORKER_FAILURE.md -->

# Realtime worker failure runbook

When a vLLM worker exits or a stream ends without final usage:

1. The request context cancels the upstream operation.
2. The contract moves from `EXECUTING` to `FAILED` or `CANCELLED`.
3. A failed `realtime_executions` row records the bounded failure code.
4. Reserved sequence capacity is released.
5. No buyer charge, supplier credit, or platform take is written.
6. The buyer retrieves the receipt and may retry with a new request or the
   original idempotency key for diagnosis; the same key cannot double-settle.

Quarantine repeated engine failures at the offer/worker layer. Do not manually
insert success ledger entries or pay a supplier from worker self-report.

If control exits after admission but before finalization, the contract remains
`EXECUTING` until its deadline plus the recovery grace. The leader-elected
`realtime-contract-recovery` sweep then locks it, records
`control_recovery_timeout`, restores capacity, and leaves money untouched.
Concurrent sweep replicas use `SKIP LOCKED`; only one may write the terminal
outcome. Alert on an executing age over 180 seconds or any finalization error.


<!-- source: docs/CUDA_CHAIN_RUNBOOK.md -->

# The CUDA chain: what is left, and exactly how to run it

Step 15's harness once ran with real money — a governed pod provisioned, served,
tore down verifiably for $0.01 — but the spend receipt
(`evidence/runpod/spend-rr7b6uwmivaolh.json`) is now **WITHDRAWN** (mutable image
tag; runtime unidentifiable) and citable by nothing. The paid experiment happened;
the receipt backs no cost or performance claim. What has never run is the **Merc
chain** through CUDA: a buyer request that produces a quote, a routed execution on
an NVIDIA host, verification, a buyer charge, a supplier payable, a positive Merc
contribution and a receipt.

This file exists because working that out took a session's worth of reading, and
none of it should have to be worked out twice.

## The insight that makes it tractable

**No agent binary is needed on the pod.** The realtime lane does not poll; Merc
calls OUT to a registered worker offer's `upstream_base_url`. So the pod runs
nothing but vLLM, and a plain HTTP registration puts it in the fleet. That removes
the two hard dependencies an earlier plan assumed: cross-compiling `merc-agent` for
`x86_64-unknown-linux-gnu`, and a publicly reachable control plane for the pod to
call back into. Merc reaches RunPod's proxy URL; RunPod never reaches Merc.

## What the pod must serve

`control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json` pins all of it, and
the offer is refused if the pod serves anything else:

```text
image      vllm/vllm-openai@sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0
model      unsloth/Llama-3.2-1B-Instruct
revision   5a8abab4a5d6f164389b1079fb721cfab8d7126c
alias      cx-chat-1b
dtype      bfloat16, tensor_parallel_size 1, max_model_len 32768
profile id vllm-llama-3.2-1b-instruct-bf16-tp1
```

This digest corresponds to **v0.23.0**. The provisioner defaults to this admitted
profile; an override must also be an immutable OCI digest. The first paid run used
a mutable v0.26.0 tag and Qwen, which is why it proved the harness and not the
lane.

## The registration contract

`POST /v1/worker/realtime/register`, worker-token auth, strict JSON — an unknown
field is a 400, not a warning (`RealtimeOfferRegistration` in
`control/realtime_store.go:24`):

```json
{
  "runtime_profile_id": "vllm-llama-3.2-1b-instruct-bf16-tp1",
  "runtime_profile_sha256": "<the profile digest the control plane computed>",
  "hw_class": "nvidia_24gb",
  "gpu_count": 1,
  "memory_gb_per_gpu": 24.0,
  "memory_gb_in_use": 0.0,
  "upstream_base_url": "https://<pod>-8000.proxy.runpod.net/v1",
  "upstream_token": "<MERC_GPU_API_KEY>",
  "warmth": "HOT",
  "max_active_sequences": 128,
  "available_sequences": 128,
  "supplier_input_usd_per_million_tokens": 0.08,
  "supplier_output_usd_per_million_tokens": 0.30
}
```

Supplier rates must sit under the profile's buyer rates (0.12 / 0.45) or the offer
is underwater and admission should refuse it — which is itself worth asserting.

`interconnect` stays absent at `gpu_count: 1`. Merc refuses to guess an
interconnect and refuses a multi-GPU offer that does not declare one.

Heartbeat is `POST /v1/worker/realtime/heartbeat` with
`{runtime_profile_id, warmth, available_sequences, status}` — `status` one of
ACTIVE / DRAINING / FAILED / QUARANTINED, `warmth` one of HOT / WARM / CACHED /
COLD. An offer that stops heartbeating drains.

## The run

1. **Provision, governed.** The cap is the money bound and the only thing between a
   hung run and the balance:

   ```bash
   # First query the current secure-cloud rate; the script refuses unless this
   # value matches RunPod exactly before and immediately after creation.
   MERC_RUNPOD_GPU="NVIDIA RTX A5000" MERC_RUNPOD_CLOUD=SECURE \
   MERC_RUNPOD_COST_PER_HR=<current-provider-rate> MERC_RUNPOD_CAP_USD=2.00 \
   MERC_VLLM_IMAGE="vllm/vllm-openai@sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0" \
   MERC_VLLM_MODEL="unsloth/Llama-3.2-1B-Instruct" \
   MERC_RUNPOD_EXPERIMENT_CMD="bash scripts/cuda-chain-drive.sh" \
   bash scripts/runpod-vllm.sh experiment
   ```

   The driver runs INSIDE the experiment so the pod is torn down and the spend
   receipt written however the driver exits.

2. **Control plane.** Boot it against a scratch database, as
   `scratchpad/stageenv.sh` did: `DATABASE_URL` to a fresh database with
   `control/schema.sql` applied, `MERC_PAYMENT_MODE=test`,
   `MERC_SETTLEMENT_CURRENCY=cad`, `MERC_CANARY_MODE=false` with a decision ref,
   `MERC_VERIFICATION_SAMPLE_SECRET` and `MERC_TOKEN_KEY` set to 32+ bytes, and —
   because the board is a USD reference against CAD settlement —
   `MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE` with `MERC_PRICE_FX_REVISION`. Neither
   the application nor a script may invent that rate; it is an operator input and
   the revision string should say so.

3. **Identities through the real routes, not through the database.** A chain proof
   that seeds its own worker row proves nothing about enrolment. Buyer signup →
   API key → funding; supplier enrolment code → worker credential → worker token.

4. **Drive it.** `POST /v1/chat/completions` with `Idempotency-Key` and
   `X-Merc-Max-USD`, model `cx-chat-1b`.

5. **Assert the whole chain, from the ledger and the receipt** — not from the HTTP
   response:

   * one contract, `CAPTURED`, charge ≤ the accepted ceiling;
   * exactly one supplier credit, to the offer's supplier;
   * buyer debit − supplier credit = platform take, and that take is **positive**;
   * the receipt names the runtime profile, the model revision and the
     `stream_root_sha256` / `output_commitment`;
   * usage reconciles against what vLLM reported in the final chunk, and
     `realtime.go` forces `stream_options.include_usage` precisely so it can.

## What this will and will not prove

It proves the CUDA lane end to end for ONE profile on ONE host, which is what
promotes `vllm_cuda` off `DRAFT` and gives the RuntimeSelector a second hardware
class to compare on — currently every measurement in
`evidence/perf/selector/paired-cohort-embed.json` (unbound cohort receipt) is
`apple_silicon_ultra`, and the cost model refuses to compare across hardware
classes for good reason.

It does not prove TP>1, does not prove a fleet, and does not close `P1-STRIPE-TEST`
or any of the other seven external gates.

If startup never reaches `/models` HTTP 200, the governed runner still sweeps
the pod and writes a receipt. That receipt has `ready=false` and is deliberately
inadmissible: it accounts for the bounded failed spend and teardown, but cannot
be promoted into CUDA-runtime evidence.

## Do not

* Do not run the driver outside `runpod-vllm.sh experiment`. The cap, the lifetime
  bound, the pre-flight refusal, the sweep-on-any-exit and the spend receipt all
  live there, and a pod nobody remembers to stop is the failure that costs money.
* Do not register an offer whose supplier rates exceed the profile's buyer rates
  and then read a negative contribution as a pricing defect.
* Do not promote the cell from this run alone. The promotion gate wants production
  decisions, twenty samples on one hardware class, zero verification failures and a
  margin — `control/runtime_cell_promotion.go` refuses anything less, and it should.


<!-- source: docs/DEPLOY_MERC_DROPLET.md -->

# Deploying merc to the droplet

Target: `192.241.134.31`, serving `mercmerc.net`. The droplet currently runs an
older build on `computexchange.net`; `/version` 404s there, so what is live
predates the economics fix, the honeypot fail-closed fix, and KILL-RT.

I have no SSH key to that host, so every step below is yours to run. Everything
that could be prepared without it has been.

---

## 0. Before anything: commit

The image records `MERC_BUILD_COMMIT` from a build argument and reports
`modified: false` regardless of whether the tree was dirty. An image built from
uncommitted work therefore claims a provenance it does not have. Commit first,
then build, so `/version` is true.

```bash
git status --porcelain | head
```

## 1. The environment the control plane demands

This list is not from the docs — it is what a `MERC_ENV=production` boot actually
refused to start without, in the order it refused. Each line below was a
separate `refusing to start`:

| Variable | Note |
|---|---|
| `MERC_ENV=production` | Turns on every check in this table. **Must be spelled `production` or `prod`.** |
| `MERC_ADMIN_CIDRS` | Comma-separated operator management CIDRs. |
| `MERC_TRUSTED_PROXY_CIDRS` | Caddy's network, so client IPs are attributed correctly. |
| `MERC_PUBLIC_CONTROL_ORIGIN` | `https://mercmerc.net` |
| `SITE_HOST` | `mercmerc.net` — also required to validate Stripe Connect return origins. |
| `STORAGE_HOST` | `storage.mercmerc.net` |
| `MERC_TOKEN_KEY` | **Copy the existing value byte-for-byte.** `control/crypto.go` derives the AES key as `sha256(value)`; regenerating it makes every sealed OAuth token and webhook secret already in Postgres permanently undecryptable. |
| `MERC_VERIFICATION_SAMPLE_SECRET` | Without it, `control/verification.go` silently substitutes a **published** default sampling secret. |
| `MERC_ECON_SCHEDULE_VERSION` | `2026-07-19` |
| `MERC_PROCESSOR_PERCENT_BPS` | `290` |
| `MERC_PROCESSOR_FIXED_USD` | `0.30` |
| `MERC_CONTROL_PLANE_PER_BATCH_USD` | `0.0001` — declared account/invoice overhead allocated across the collector's economic charge batch. |
| `MERC_MIN_CONTRIBUTION_PER_BATCH_USD` | `0.000001` — positive absolute contribution floor for micro-jobs. |
| `MERC_TARGET_MARGIN_BPS` | `1000` |
| `MERC_PAYMENT_MODE` | `sealed` until the candidate-bound LIVE activation package exists. |
| `MERC_PAYMENT_PROVIDER` | `none` in SEALED; `stripe` in LIVE. |
| `STRIPE_*`, `MERC_CONNECT_*` | **Unset in SEALED.** Required only by the separately approved LIVE activation procedure. |
| `DATABASE_URL`, `S3_*` | As already configured on the host. |

**The rename has not touched these names.** Tier 1 of the rebrand was
repo-internal only, so the droplet's existing `CX_*` variables still apply
unchanged. See `docs/RENAME_REGISTER.md` before renaming any of them.

## 1a. Environment variable cutover — `CX_*` is now `MERC_*`

Every variable in this runbook was renamed from `CX_` to `MERC_`. The droplet
currently has the **old** names set, so the rename is a coordinated cutover, not
a drop-in deploy:

```bash
# On the droplet, rewrite the env file in place. Keeps every VALUE byte-identical.
cp /path/to/.env /path/to/.env.bak.$(date -u +%Y%m%dT%H%M%SZ)
sed -i -E 's/^CX_([A-Z0-9_]+)=/MERC_\1=/' /path/to/.env
grep -c '^MERC_' /path/to/.env   # sanity: should match the old CX_ count
```

**Copy the values, do not regenerate them.** `MERC_TOKEN_KEY` in particular
derives the AES key as `sha256(value)`; a new value makes every sealed OAuth
token and webhook secret already in Postgres permanently undecryptable.

A half-applied cutover is the dangerous state: the binary reads `MERC_ENV`,
finds nothing, and skips the production-hardening refusal — booting with a
warning while writing `plain:`-prefixed secrets. Rename the whole file at once.

GitHub Actions secrets need the same rename.

## 2. Deploy SEALED until live payments are independently activated

Production now defaults to `MERC_PAYMENT_MODE=sealed`. SEALED is structural:
Stripe credentials, webhook secrets, payout export, provider network calls,
charges, refunds, reversals and payouts are refused. An administrator cannot
turn them on by changing the database payment kill switch. `/readyz` remains
green for the rest of the platform and reports:

```json
{"status":"ready","payment_mode":"sealed","provider_enabled":false,"live_value_movement":false,"stripe_api_version":"2025-06-30.basil"}
```

This is the correct mode while live Stripe, payment/legal approvals, and
provider-side aggregate limits are unavailable. The ledger currency is explicit
through `MERC_SETTLEMENT_CURRENCY`; the current sandbox authority is CAD, not a
hardcoded USD assumption. The catalogue market board remains a USD reference;
non-USD settlement additionally requires
`MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE` (settlement major units per USD) and
`MERC_PRICE_FX_REVISION` (an immutable operator-reviewed source/revision).
Startup refuses to publish the catalogue when either is missing.

Every control-plane and operator-script Stripe request pins
`Stripe-Version: 2025-06-30.basil`. Charges, setup, reads, Connect onboarding,
payouts, refunds, reversals, settlement probes, and webhook management therefore
cannot inherit a different account-default API contract after deployment.
Changing this version is a reviewed candidate change, never a Stripe Dashboard
side effect.

LIVE later requires all of the following at once:

- a clean 40-character deployed candidate commit;
- `MERC_PAYMENT_MODE=live` plus a permission-restricted
  `STRIPE_SECRET_KEY_SOURCE` file containing a live or restricted-live Stripe
  credential (never an inline production environment value);
- distinct billing and Connect webhook secrets;
- a 72-hour-or-shorter activation and bounded recovery window;
- payments, security, and release-manager approvals;
- per-operation caps and an independently configured aggregate-cap reference;
- a read-only activation file whose digest and HMAC both verify;
- a separate 0600/0640 HMAC-key file readable by the container's UID/GID.

When the short value-movement window expires, MERC remains healthy only during
the signed recovery window. New charges, payouts, and provider setup are
refused; provider reads, signed webhooks, refunds, and transfer reversals remain
available so reconciliation can finish. Once recovery expires, readiness fails
closed until a new candidate-bound activation is installed.

The canonical schema is `ops/live-payment-activation.schema.json`. Sign with the
candidate binary:

```bash
MERC_SETTLEMENT_CURRENCY=cad \
MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_FILE=/absolute/private/live-payment-activation-hmac-key \
scripts/cx release payment-activation-sign \
  --input /absolute/private/unsigned-live-activation.json \
  --output /absolute/private/live-payment-activation.json
```

The command refuses dirty/different/expired candidates and will not overwrite an
existing reviewed activation. It may pre-stage a future activation at most seven
days before `valid_from`; runtime authorization still uses the real clock.
Deployment uses
`MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_SOURCE` for that same file and mounts it
read-only. The live Stripe key uses the same pattern through
`STRIPE_SECRET_KEY_SOURCE`; neither secret value appears in `docker inspect`.

## 3. Build the image — use CI, not your laptop

`.github/workflows/publish-candidate.yml` already builds on `ubuntu-latest`
(native amd64) and cosign-signs the result with an SPDX SBOM and SLSA
provenance, keyless via GitHub OIDC. It triggers on push to
`release/rc1-go-closure`, which is the branch this work is on. A hand-built
image carries none of that, and the receipts under `evidence/` are written
against signed images — so pushing is both easier and more correct:

```bash
git push origin release/rc1-go-closure
```

**Cross-building amd64 on the Mac works — but only under OrbStack.** Under
colima/qemu the Go compiler dies with `fatal error: found pointer to free
object` (the Go GC misbehaving under qemu-user, not a code fault). OrbStack uses
Rosetta instead and builds cleanly:

```bash
docker context use orbstack
docker build --platform linux/amd64 -f Dockerfile.control \
  --build-arg MERC_BUILD_VERSION=v0.1.0-merc-rc1 \
  --build-arg MERC_BUILD_COMMIT="$(git rev-parse HEAD)" \
  --build-arg MERC_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -t merc/control:amd64-rc1 .
```

Built and smoke-tested that exact image here — 6.7 MB, and it serves:

```
/healthz -> 200
/version {"version":"v0.1.0-merc-rc1","platform":"linux/amd64","modified":false}
```

Ship it directly if you prefer not to wait on CI:

```bash
docker save merc/control:amd64-rc1 | gzip | ssh root@192.241.134.31 'gunzip | docker load'
```

A locally built image carries **no cosign signature, SBOM or SLSA provenance**,
and the receipts under `evidence/` are written against signed images. Prefer the
CI path for anything you intend to keep.

If you would rather not use CI, build natively **on the droplet** — it is
x86_64, so no emulation is involved.

`docker-compose.prod.yml` references `computexchange/control`, and the published
image is `ghcr.io/<owner>/computexchange-control`. Both are **EXTERNAL** renames
per `docs/RENAME_REGISTER.md`: rename the registry package first, then the
compose reference. Never rewrite the digests recorded under `evidence/` — they
attest to builds that really happened under the old name.

## 4. Cutover

`mercmerc.net` and `storage.mercmerc.net` already resolve to the droplet as
proxied=false A records, which is what lets Caddy terminate TLS and obtain a
certificate over HTTP-01. Nothing else in DNS needs to change.

Deploying with `SITE_HOST=mercmerc.net` moves the live site: Caddy will obtain a
certificate for the new host and stop serving `computexchange.net` unless you
keep a block for it. Decide deliberately whether the old hostname should redirect
or 404 — I have not changed its DNS, and it is serving 200 right now.

## 5. Verify from off-box

```bash
curl -s https://mercmerc.net/version
curl -s -o /dev/null -w '%{http_code}\n' https://mercmerc.net/healthz
curl -s -o /dev/null -w '%{http_code}\n' https://mercmerc.net/readyz
```

`/version` should report `v0.1.0-merc-rc1` and the commit you built. If it 404s,
the old build is still serving.

## 6. Re-point the Stripe webhooks only during approved LIVE activation

The two endpoints currently point at a dead `trycloudflare.com` quick tunnel.
Do not recreate them during a SEALED deployment. Once LIVE activation is
approved, recreate them through the supervised Stripe procedure:

```bash
stripe_secret="$(tr -d '\r\n' < "$STRIPE_SECRET_KEY_SOURCE")"
curl -s https://api.stripe.com/v1/webhook_endpoints -u "$stripe_secret:" \
  -H 'Stripe-Version: 2025-06-30.basil' \
  -d url=https://mercmerc.net/v1/stripe/webhook \
  -d api_version=2025-06-30.basil \
  -d 'enabled_events[]=payment_intent.succeeded' \
  -d 'enabled_events[]=setup_intent.succeeded' \
  -d 'enabled_events[]=payment_method.attached' \
  -d 'enabled_events[]=charge.refunded' \
  -d 'enabled_events[]=charge.dispute.created' \
  -d 'enabled_events[]=charge.dispute.closed'
unset stripe_secret
```

```bash
stripe_secret="$(tr -d '\r\n' < "$STRIPE_SECRET_KEY_SOURCE")"
curl -s https://api.stripe.com/v1/webhook_endpoints -u "$stripe_secret:" \
  -H 'Stripe-Version: 2025-06-30.basil' \
  -d url=https://mercmerc.net/v1/stripe/connect-webhook \
  -d connect=true \
  -d api_version=2025-06-30.basil \
  -d 'enabled_events[]=account.updated' \
  -d 'enabled_events[]=payout.created' \
  -d 'enabled_events[]=payout.paid' \
  -d 'enabled_events[]=payout.failed'
unset stripe_secret
```

Put each response's `secret` into `STRIPE_WEBHOOK_SECRET` and
`MERC_CONNECT_WEBHOOK_SECRET`, and each `id` into
`STRIPE_BILLING_WEBHOOK_ENDPOINT_ID` and `STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID`.
The two secrets **must differ** — that check stops a leaked billing secret being
used to forge a Connect "payout succeeded" event. Then delete the two old
`we_…` endpoints pointing at the dead tunnel.

Prefer `scripts/stripe-webhooks.sh`, which performs these calls and refuses an
existing endpoint whose `api_version` is null or different. Stripe does not
permit changing that field in place: create a replacement, rotate the signing
secret, verify signed delivery, and only then disable the old endpoint.

## 7. Then

```bash
make stripe-check && make stripe-matrix
```

`stripe-check` and `stripe-matrix` remain test-mode authority. They do not arm
LIVE payments or upgrade legal/governance readiness by themselves.


<!-- source: docs/STAGING_DEPLOYMENT_PLAN.md -->

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

> **`MERC_VERIFICATION_SAMPLE_SECRET` — also immutable against an existing
> database.** Verification sampling decisions are HMAC-derived from it and are
> recorded per attempt. Change it against a populated database and every stored
> decision conflicts with the recomputed one: `verification-recovery` fails on
> every pass, its ticker goes stale, and `/readyz` drops to 503 with
> `stale_tickers: ["verification-recovery"]`. Observed exactly that way on a
> local stack after the secret was changed between runs — 341 rows carrying
> decisions from the previous secret. The refusal is correct: a sampling
> decision must be immutable, or a supplier could be re-rolled out of being
> observed. Treat it with the same care as the token key below.

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
make offsite-independent-restore-check
make offsite-independent-restore
```
*Verifies:* encrypts an isolated source, uploads **only ciphertext** to the
already-configured Cloudflare R2 prefix, independently re-downloads both
manifest and ciphertext, compares both SHA-256 values computed on the
verifying side, decrypts in isolation, restores into a new Postgres/MinIO
with new credentials, and matches ledger/object/application invariants.
Does not touch `merc-postgres-1` or `merc_pgdata`.

**Gate:** `evidence/external/offsite-backup-verification.json` and
`evidence/external/offsite-independent-restore.json`. The receipts name the
exact independence boundary (`cloudflare_r2_operator_controlled`) and do
not claim a third-party-held account or a live-droplet snapshot.

The older rollback rehearsal (`scripts/go-closure-rollback-rehearsal.sh`)
still exists for staging image recovery; it is not this alpha's offsite
receipt path.

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

**Gate (required before proceed):** all fourteen scenarios PASS, restart storm
PASS, and the required 24-hour soak has completed with RSS growth inside
`MERC_SOAK_MAX_RSS_GROWTH_BYTES`, zero firing page alerts at the end, and a
post-run database with no open terminal tasks and a zero ledger sum.

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


<!-- source: docs/RUNPOD_ESCALATION.md -->

# RunPod support escalation — pods allocate and bill, never get runtime

## Request

Why do allocated pods never receive runtime placement on this account, and what must change on our side to make them schedule?

## Account (as observed via API)

| Field | Value |
| --- | --- |
| Client balance (before series) | $17.1154584262 |
| Client balance (after series) | $17.0869980669 |
| Spend limit | $80 |
| Minimum balance | $0 |
| Current spend per hour | $0 |
| `machineQuota` | **0** |

Balance, spend limit, and minimum balance were all satisfied during the attempts. Diagnosis recorded at `2026-07-29T01:53:04.710129+00:00` (UTC).

**Question:** `machineQuota` reads `0` on this account. Does that field gate on-demand pod placement for renters, or does it apply only to hosting/supply-side machines? If it is relevant to renting, what action is required on our side to set a non-zero quota?

## Problem

Pods are created, accepted, and billed, but never receive runtime placement.

- `desiredStatus` reports `RUNNING` (request intent).
- `runtime` stays `null` indefinitely (no placement / no agent-reported runtime).
- Observed waits without runtime: up to **585 seconds** (pod `56bbql1zb391wc`) and **180 seconds** (pod `oljnclfjewy5m6`).

All pods were torn down afterward. The account currently shows **zero pods** and **$0/hr** spend rate.

## Attempt log

### Prior series (`startup-diagnosis`)

| Pod ID | GPU | Cloud | API | Image | Runtime | Outcome | Amount recorded |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `pww27g05kgd6zy` | A100 80GB PCIe | ALL | GraphQL | `vllm/vllm-openai:v0.6.3.post1` | `null` | never started | **$0.26** charged |
| `bwwg34zwuq9idd` | A100 80GB PCIe | ALL | GraphQL | `vllm/vllm-openai:v0.26.0-cu129-ubuntu2404` | `null` | never started | **$0.20** charged |
| `c4oqcp8b14tirf` | RTX A5000 | SECURE | REST | `nvidia/cuda:12.4.1-base-ubuntu22.04` | `null` | machine allocated, container never reported | $0.27/hr (rate only; no total recorded) |

Prior series total charged (as recorded): **$0.46**.

### Later series (`pod-scheduling-diagnosis`)

| Pod ID | GPU | Cloud | API | Image | `desiredStatus` | Runtime | Waited | Outcome | Rate |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `56bbql1zb391wc` | NVIDIA RTX A4000 | COMMUNITY | REST | `nvidia/cuda:12.4.1-base-ubuntu22.04` | `RUNNING` | `null` | 585 s | never started; torn down | $0.17/hr |
| `oljnclfjewy5m6` | NVIDIA RTX A4000 \| A5000 \| A40 | SECURE | REST | `nvidia/cuda:12.4.1-base-ubuntu22.04` | `RUNNING` | `null` | 180 s | never started; torn down | $0.27/hr |

Account spend delta for the later series (balance before − after): **$0.0285**.

## Teardown confirmation

After teardown:

- Pods listed: **0**
- Current spend per hour: **$0**

## Already ruled out

Please do not suggest re-trying these; each was already exercised without a non-null `runtime`:

| Variable | Values tried |
| --- | --- |
| GPU class | A100 80GB PCIe; RTX A4000; RTX A5000; A40 (including multi-class request A4000\|A5000\|A40) |
| Cloud type | ALL; COMMUNITY; SECURE |
| API | GraphQL (`podFindAndDeployOnDemand`); REST (`rest.runpod.io`) |
| Image | `vllm/vllm-openai:v0.6.3.post1`; `vllm/vllm-openai:v0.26.0-cu129-ubuntu2404`; plain `nvidia/cuda:12.4.1-base-ubuntu22.04` (no custom entrypoint override on the plain CUDA attempts) |
| Account funds | Balance above min; spend limit $80; min balance $0 |

## What we need from you

1. Why pods on this account reach accepted/`desiredStatus: RUNNING` and incur charges while `runtime` remains `null` indefinitely.
2. What must change on **our** side (account setting, verification, quota, region, product flag, or other) so a newly created pod receives runtime placement.
3. Clarification of whether `machineQuota: 0` is expected for a renter account and whether it blocks scheduling.
4. Refund or credit guidance for the amounts charged on pods that never started (`$0.46` prior series + `$0.0285` later series, as recorded).


<!-- source: ops/staging/README.md -->

# GO-closure staging harness

This directory is an operational harness, not deployment evidence. It emits a
PASS receipt only after the requested command and its assertions succeed. A
manifest, a dry run, or a short soak cannot satisfy the Level-B canary gate.

The stack is standalone and intentionally does not merge with
`docker-compose.prod.yml`. The control image has no `build` key, and every
runtime image is either fixed to a reviewed digest in the manifest or supplied
as an exact `registry/repository@sha256:<64 hex>` reference. The deployment and
rollback scripts pull and inspect those exact references before activation.
The Level B control runtime is intentionally fixed to test-only Stripe payment
authority (`MERC_PAYMENT_MODE=test`, `MERC_PAYMENT_PROVIDER=stripe`,
`MERC_SETTLEMENT_CURRENCY=cad`); the Stripe Sandbox preflight and matrix bind
every provider object and Canadian payout fixture to that same reviewed
authority. Because the governed catalogue board is USD-denominated, startup
also requires an operator-approved CAD-per-USD rate and immutable FX revision;
the resulting schedule binds the board digest, reference prices, converted CAD
prices, and FX authority atomically. These settings do not authorize live value.

## Operator preparation

1. Provision the names and credentials listed in
   `ops/go-closure-inputs.json`. Copy `ops/staging/env.go-closure.example` to
   `.env.go-closure`, replace every placeholder through the approved secret
   channel, and run `chmod 600 .env.go-closure` on both the operator machine and
   staging host. Never commit or paste the file into task output.
2. Publish candidate and prior control images, retain both by digest, and fill
   the image/commit pairs. Pin the four monitoring images by digest as well.
3. Place `.env.go-closure` at `STAGING_DEPLOYMENT_ROOT` using a separate secure
   channel. `go-closure-deploy.sh` syncs only non-secret manifests and scripts;
   it deliberately never transfers the environment file.
   Bind worker admission to reviewed artifacts with the exact agent semver(s)
   and 16-hex runtime build hash(es). Unknown agent builds must remain a hard
   admission failure.
4. Point both staging DNS names at the host. Set `STAGING_BIND_ADDRESS` to one
   specific interface IPv4 address; wildcard binds are rejected. Restrict that
   interface to approved participants with the project firewall, VPN, or
   identity-aware proxy. The compose file exposes only Caddy on that address;
   metrics UIs bind to loopback. The bind restriction is defense in depth, not
   a substitute for verifying the external access-control policy.

Validate the non-secret scaffold before use:

```sh
scripts/validate-go-closure-scaffold.sh
scripts/go-closure-deploy.sh --target ssh --activate candidate --check
scripts/go-closure-restart-storm.sh --target ssh --check
scripts/go-closure-canary-rehearsal.sh --target ssh --check
```

The guarded `merc release launch` flow first runs
`go-closure-release-identity.sh --target ssh`. It synchronizes only the
reviewed non-secret harness, then compares a SHA-256 profile of every declared
operator input on the staging host with the sealed local plan. Neither the
profile nor the release state records secret values, and the command never
copies `.env.go-closure` to staging. A mismatch is a fail-closed remote-drift
refusal; reconcile the two approved operator files, reseal the plan, and obtain
a new approval before retrying.

Mutating operations require the literal `--execute` flag:

```sh
scripts/go-closure-deploy.sh --target ssh --activate candidate --execute
scripts/go-closure-rollback-rehearsal.sh --target ssh --execute
scripts/go-closure-restart-storm.sh --target ssh --execute
scripts/go-closure-canary-rehearsal.sh --target ssh --execute
scripts/go-closure-soak.sh --target ssh --duration 86400 --execute
```

For the production-shaped local topology, `make soak-15m` and `make soak-2h`
are short non-qualifying iterations. `make soak-24h-persistent` starts a
terminal-independent continuous 24-hour process in a retained `tmux` session,
and `make soak-24h-status` reports its state and process exit status. Any process
interruption invalidates the run; only the terminal PASS receipt, bound to the
state file's source fingerprint and start window, can close the 24-hour gate.

Use `--target local` only for an isolated host rehearsal. It does not close the
persistent external staging blocker.

## Driver receipts

The canary scenario driver is invoked as:

```text
$MERC_CANARY_SCENARIO_DRIVER run <scenario> <minimum-count>
```

It must write exactly one JSON document to stdout. The document must contain
the requested scenario, the requested count, an observed count exactly equal
to that request and the evidence-array length, `status: "PASS"`, and one
unique observation/subject pair per item. The rehearsal exports
`MERC_CANARY_RUN_ID`, `MERC_CANARY_CANDIDATE_COMMIT`,
`MERC_CANARY_CONTROL_IMAGE`, `MERC_CANARY_DRIVER_SHA256`,
`MERC_CANARY_RUN_STARTED_AT`, `MERC_CANARY_SCENARIO`, and
`MERC_CANARY_SCENARIO_STARTED_AT`; the receipt must bind those exact values.
The driver must be a canonical, non-symlink, non-group/world-writable
executable inside a non-group/world-writable directory. Its SHA-256 is checked
against the operator-reviewed `MERC_CANARY_APPROVED_DRIVER_SHA256`, before and
after every scenario, and must remain unchanged for the whole run.

```json
{
  "schema_version": 2,
  "scenario": "embed_success",
  "requested": 20,
  "observed": 20,
  "status": "PASS",
  "binding": {
    "run_id": "32 lowercase hex",
    "candidate_commit": "40 lowercase hex",
    "control_image": "registry/repository@sha256:...",
    "driver_sha256": "64 lowercase hex"
  },
  "started_at": "RFC3339 UTC within this run",
  "finished_at": "RFC3339 UTC after started_at",
  "safety": {
    "stripe_test_mode": true,
    "stripe_live_mode": false,
    "real_value": false,
    "approved_participants_only": true,
    "secret_values_recorded": false
  },
  "evidence": [
    {
      "id": "unique-observation-id",
      "subject_id": "job UUID",
      "occurred_at": "RFC3339 UTC inside the scenario window",
      "source": "merc_postgres.jobs"
    }
  ]
}
```

Sources are closed, not free-form: buyers use `merc_postgres.buyers`, Metal
agents `merc_postgres.workers`, completed/cancelled workloads
`merc_postgres.jobs`, retries/recovery `merc_postgres.job_events`, buyer
webhooks `merc_postgres.webhooks`, stale-commit HTTP observations
`merc_control.http`, and external exercises their named provider source in
`scripts/validate-canary-scenario-receipt.py`. Merc independently queries
PostgreSQL for the buyer, worker, workload, retry, recovery, and webhook
subjects after structural validation. Completed workloads must belong to the
approved buyers, and every task must run on an approved worker plus reviewed
agent version/build, show real runtime and verification authority, and carry
the exact frozen buyer/supplier/platform ledger split with zero-sum money. The
final receipt also requires a clean global ledger, no open tasks under terminal
jobs, two active workers, no firing page alert, and unchanged reviewed driver
bytes. A driver cannot promote itself by printing booleans.
Each stale-attempt observation additionally binds the submitted/current attempt
numbers, HTTP 409, response digest, and identical before/after state digests;
Merc verifies the task is durably stored at that newer current attempt.

The agent restart driver is invoked as:

```text
$MERC_AGENT_RESTART_DRIVER restart-all 2
```

The harness exports `MERC_RESTART_RUN_ID`, `MERC_RESTART_CANDIDATE_COMMIT`,
`MERC_RESTART_CONTROL_IMAGE`, `MERC_RESTART_DRIVER_SHA256`,
`MERC_RESTART_RUN_STARTED_AT`, `MERC_RESTART_RUN_STARTED_EPOCH`, and the
approved worker IDs. The schema-v2 driver receipt is only an action receipt: it
must bind those exact run/candidate/driver values and contain exactly two
unique `approved_agent_supervisor` actions targeting the approved worker UUIDs
inside the invocation window. The driver must be a canonical, non-symlink,
non-group/world-writable executable in a non-group/world-writable directory,
and its bytes must match `MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256` before and
after execution.

Each merc-agent process generates a fresh `agent_session_id` at startup. MERC
stores that UUID and its first registration time without resetting the time
when the same process registers again. Before invoking the supervisor adapter,
the restart storm reads two current reviewed sessions from PostgreSQL. It emits
restart authority only after both approved worker rows independently change to
new session UUIDs created during the run, heartbeat during the run, and remain
current without a second unexpected restart through the remaining fault storm.
The adapter cannot prove a restart by printing a count or boolean.

Receipts are structural inputs, not automatic proof. Preserve the underlying
provider, database, receiver, agent, and object identifiers. An independent
reviewer must still correlate them before GO.

## Evidence and recovery rules

Successful operations write atomic JSON receipts and raw, non-secret samples
under `evidence/go-closure/` on the staging host. Failures exit nonzero and do
not manufacture a PASS file. The rollback rehearsal takes and independently
verifies an encrypted offsite backup before switching images. `backup.sh`
downloads both ciphertext and manifest through the offsite API, compares both
SHA-256 values, uploads a closed verification receipt, and validates that
receipt against the retained encrypted bytes. Only after the entire backup
command succeeds does it atomically emit an invocation result naming that exact
backup and all three hashes. The rollback rehearsal refuses a stale, concurrent,
or mixed backup: it resolves artifacts only through that invocation result,
creation must fall inside the current rehearsal window, the backup ID and exact
`s3://` URI must agree, and both invocation and verification receipts are
embedded rather than replaced by a boolean. It then checks the
public commit at candidate, prior, and forward-recovered states, measures both
RTOs, and compares database integrity snapshots before and after.

Runtime receiver material is written only under the ignored `.secrets/` tree;
the backup-age signal is written under ignored `.artifacts/`. Neither path is
synced from the operator workstation or included in evidence.

The restart storm performs two verified control restarts, one database restart,
one object-store restart, one alerting restart, two bounded control-network
interruptions, and delegates the two Metal-agent supervisor actions to the
reviewed adapter above while deriving actual restart proof from durable process
session transitions. The final 24-hour soak refuses a shorter duration unless
`--iteration` is supplied; iteration receipts are marked non-qualifying. Every
sample binds the full control-container ID, configured immutable candidate
image, and content-addressed image ID. A restart, recreation, or image
substitution invalidates the run. The schema-v2 PASS receipt is accepted only
after `validate-go-closure-soak-receipt.py` verifies the retained JSONL
SHA-256, re-derives every health assertion and resource bound, and confirms at
least 95% sample coverage. The runner deletes the PASS receipt if that
independent validation fails.

After every operation has completed, place the exact signed governance bundle
under the staging root's restricted evidence directory (mode `0600`; the
bundle must contain no secrets). Select every receipt by its exact filename,
never by `latest` or a glob, and run the final chain validator:

```sh
python3 scripts/validate-go-closure-evidence-chain.py \
  --root "$STAGING_DEPLOYMENT_ROOT" \
  --commit "$MERC_CANDIDATE_COMMIT" \
  --image "$MERC_CANDIDATE_CONTROL_IMAGE" \
  --checked-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --deploy evidence/go-closure/<exact-candidate-deploy-receipt>.json \
  --rollback evidence/go-closure/<exact-rollback-forward-receipt>.json \
  --restart evidence/go-closure/<exact-restart-storm-receipt>.json \
  --canary evidence/go-closure/<exact-canary-rehearsal-receipt>.json \
  --soak evidence/go-closure/<exact-qualifying-soak-receipt>.json \
  --governance evidence/go-closure/<exact-signed-governance-bundle>.json
```

The validator rejects evidence older than seven days; requires deploy,
rollback/forward, restart, canary, and the uninterrupted 24-hour soak in that
order; revalidates the retained backup ciphertext and raw soak samples; binds
the deploy and soak content image IDs; revalidates every nested restart and
canary driver receipt; and requires final release approval after the soak and
all other approvals/exercises. Its PASS means only
`ELIGIBLE_FOR_SUPERVISED_LEVEL_B_PRIVATE_CANARY_REVIEW`. It is not Level-C GO,
live-payment activation, or public-launch authority.

These scripts never authorize Stripe live mode, real-value settlement, or
unrestricted public access.

For the authoritative CLI flow, put the safe-relative governance receipt path
(for example `evidence/go-closure/governance-<candidate>.json`) in
`ops/launch/level-b.yaml` under `evidence.governance_receipt` before sealing
the plan. `merc release launch` captures the five other exact paths from the
single PASS line of each audited adapter, then invokes
`go-closure-evidence-proof.sh` with all six paths. It refuses an absent,
duplicated, outside-root, or non-JSON receipt path; it never chooses a newest
file or glob. A PASS root receipt is only evidence-chain eligibility for the
supervised private canary. `merc release go-no-go` separately applies the
current readiness ledger, so unresolved P1 entries still produce `NO_GO`.

For an operator-only release status view, run `merc release ui`. It binds to a
random `127.0.0.1` port by default and prints the loopback URL. The handler is
read-only, emits `Cache-Control: no-store`, and exposes only typed release
state, typed root evidence, the readiness score, decision labels, and P1 IDs/
owners. It rejects wildcard and non-loopback bind addresses.

`merc release destroy` is a controlled service teardown, not a data eraser. It
requires `--apply`, `--approve-plan <PLAN_SHA256>`, and
`--confirm-destroy <PLAN_SHA256>`, then stops only the audited Compose services
and writes an exact teardown receipt. It preserves persistent volumes, staged
evidence, offsite backups, the operator environment file, and images. Any
destruction of those durable materials must be separately scoped and approved.


<!-- source: ops/monitoring/README.md -->

# Canary monitoring bundle

This directory is a provisionable Prometheus, Alertmanager, and Grafana contract.
Deploy it with the root `docker-compose.observability.yml` (standalone for local
proof, or layered onto `docker-compose.prod.yml` on a canary host).

## Deploy

The production deploy path composes two manifests:

```text
docker compose \
  -f docker-compose.prod.yml \
  -f docker-compose.observability.yml \
  config
```

`scripts/deploy.sh` and `scripts/bootstrap-prod.sh` use that pair by default.
The GO-closure staging path continues to use `ops/staging/compose.go-closure.yml`,
which already embeds the same four observability services.

Mount:

* `prometheus.yml` and `alerts.yml` at `/etc/prometheus/`
* `alertmanager.yml` at `/etc/alertmanager/alertmanager.yml`
* `grafana/provisioning` at `/etc/grafana/provisioning`
* `grafana/dashboards` at `/var/lib/grafana/dashboards`

## Required: alert receiver (fail closed)

An alerting stack with no reachable receiver is worse than no stack — it looks
configured while pages go nowhere. The receiver is a **required operator input**:

| Input | Where | Failure mode |
|---|---|---|
| `MERC_ALERT_RECEIVER_URL_FILE` | absolute path to a file containing only the HTTPS webhook URL | compose config fails if unset; deploy refuses if file missing/empty/non-HTTPS |
| Docker secret `cx_alert_receiver_url` | mounted at `/run/secrets/cx_alert_receiver_url` | Alertmanager reads it via `url_file` |

Write the real test receiver URL, including any secret path, to the file named by
`MERC_ALERT_RECEIVER_URL_FILE` (mode `600`). Do not commit it. Example:

```bash
umask 077
printf '%s' 'https://hooks.example.com/services/…' > /run/secrets/cx_alert_receiver_url
export MERC_ALERT_RECEIVER_URL_FILE=/run/secrets/cx_alert_receiver_url
```

The receiver must accept both firing and resolved webhook events.

Also required for Grafana: `GF_SECURITY_ADMIN_PASSWORD`.

GO-closure staging materializes the same secret from
`ALERT_RECEIVER_WEBHOOK_URL` into `.secrets/go-closure/cx_alert_receiver_url`
via `gc_materialize_alert_secret`.

## Scrapes and signals

Local fire → receive → resolve proof (derives status from the sink, never
asserts delivery):

```text
bash scripts/test-alert-delivery.sh
```

Prometheus scrapes the control service over the private service network because
Caddy deliberately returns `404` for public `/metrics`. `node-exporter` supplies
host disk telemetry.

`cx_model_cache_corruption_total` is intentionally **not** emitted by any current
Go or Rust path, and Prometheus does not scrape supplier agents. The page-severity
"corruption detected" rule was removed for that reason. The companion
`absent(cx_model_cache_corruption_total)` ticket rule remains: its absence
deliberately opens a staging ticket until a real agent telemetry collector lands.

Set `MERC_BACKUP_STATUS_FILE` for `scripts/backup.sh` and the control process to
the same mounted path. The backup script atomically writes a Unix timestamp only
after encrypted offsite upload, independent download, and checksum verification.
Mount that file read-only into the control container. This signal is backup-age
telemetry, not proof of a successful restore drill. Production compose defaults
the mount to `${MERC_BACKUP_HEALTH_DIR:-./.artifacts/backup-health}` →
`/run/cx-health`. Schedule backups with the systemd units under `ops/systemd/`.

Validate locally:

```text
promtool check rules ops/monitoring/alerts.yml
promtool check config --syntax-only ops/monitoring/prometheus.yml
node scripts/validate-observability.mjs
docker compose -f docker-compose.prod.yml -f docker-compose.observability.yml config
```

Before GO, provision the stack, fire and resolve representative alerts, silence
one test alert, and preserve receiver event IDs and delivery timestamps.

Use a narrow, expiring silence with an operator comment, for example:

```text
amtool --alertmanager.url http://alertmanager:9093 silence add \
  alertname=MercQueueAgeHigh --duration=15m \
  --comment='staging synthetic owned by <operator>'
```

Never silence by `severity` alone; that would suppress unrelated canary failures.

