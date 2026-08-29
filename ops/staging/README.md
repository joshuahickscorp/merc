# GO-closure staging harness

This directory is an operational harness, not deployment evidence. It emits a
PASS receipt only after the requested command and its assertions succeed. A
manifest, a dry run, or a short soak cannot satisfy the Level-B canary gate.

The stack is standalone and intentionally does not merge with
`ops/deploy/docker-compose.prod.yml`. The control image has no `build` key, and every
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
ops/scripts/validate-go-closure-scaffold.sh
ops/scripts/go-closure-deploy.sh --target ssh --activate candidate --check
ops/scripts/go-closure-restart-storm.sh --target ssh --check
ops/scripts/go-closure-canary-rehearsal.sh --target ssh --check
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
ops/scripts/go-closure-deploy.sh --target ssh --activate candidate --execute
ops/scripts/go-closure-rollback-rehearsal.sh --target ssh --execute
ops/scripts/go-closure-restart-storm.sh --target ssh --execute
ops/scripts/go-closure-canary-rehearsal.sh --target ssh --execute
ops/scripts/go-closure-soak.sh --target ssh --duration 86400 --execute
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
`ops/scripts/validate-canary-scenario-receipt.py`. Merc independently queries
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
python3 ops/scripts/validate-go-closure-evidence-chain.py \
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
