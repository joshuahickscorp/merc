# GO-closure staging harness

This directory is an operational harness, not deployment evidence. It emits a
PASS receipt only after the requested command and its assertions succeed. A
manifest, a dry run, or a short soak cannot satisfy the Level-B canary gate.

The stack is standalone and intentionally does not merge with
`docker-compose.prod.yml`. The control image has no `build` key, and every
runtime image is either fixed to a reviewed digest in the manifest or supplied
as an exact `registry/repository@sha256:<64 hex>` reference. The deployment and
rollback scripts pull and inspect those exact references before activation.

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

Mutating operations require the literal `--execute` flag:

```sh
scripts/go-closure-deploy.sh --target ssh --activate candidate --execute
scripts/go-closure-rollback-rehearsal.sh --target ssh --execute
scripts/go-closure-restart-storm.sh --target ssh --execute
scripts/go-closure-canary-rehearsal.sh --target ssh --execute
scripts/go-closure-soak.sh --target ssh --duration 86400 --execute
```

Use `--target local` only for an isolated host rehearsal. It does not close the
persistent external staging blocker.

## Driver receipts

The canary scenario driver is invoked as:

```text
$CX_CANARY_SCENARIO_DRIVER run <scenario> <minimum-count>
```

It must write exactly one JSON document to stdout. The document must contain
the requested scenario, the requested minimum, an observed count at least that
large, `status: "PASS"`, an evidence array with one unique source record per
observation, and these safety booleans:

```json
{
  "schema_version": 1,
  "scenario": "embed_success",
  "requested": 20,
  "observed": 20,
  "status": "PASS",
  "safety": {
    "stripe_test_mode": true,
    "real_value": false,
    "approved_participants_only": true
  },
  "evidence": [
    {"id": "provider-or-database-event-id", "occurred_at": "RFC3339", "source": "staging-system"}
  ]
}
```

The agent restart driver is invoked as:

```text
$CX_AGENT_RESTART_DRIVER restart-all 2
```

Its JSON receipt must report `status: "PASS"`, `requested: 2`,
`restart_count >= 2`, `distinct_agents >= 2`, and an evidence array at least as
large as `restart_count`. The driver owns the project-controlled SSH/launchd
details; this repository never accepts an arbitrary shell command from the env.

Receipts are structural inputs, not automatic proof. Preserve the underlying
provider, database, receiver, agent, and object identifiers. An independent
reviewer must still correlate them before GO.

## Evidence and recovery rules

Successful operations write atomic JSON receipts and raw, non-secret samples
under `evidence/go-closure/` on the staging host. Failures exit nonzero and do
not manufacture a PASS file. The rollback rehearsal takes and independently
verifies an encrypted offsite backup before switching images, checks the public
commit at candidate, prior, and forward-recovered states, measures both RTOs,
and compares database integrity snapshots before and after.

Runtime receiver material is written only under the ignored `.secrets/` tree;
the backup-age signal is written under ignored `.artifacts/`. Neither path is
synced from the operator workstation or included in evidence.

The restart storm performs two verified control restarts, one database restart,
one object-store restart, one alerting restart, two bounded control-network
interruptions, and delegates two distinct Metal-agent restarts to the strict
adapter above. The final 24-hour soak refuses a shorter duration unless
`--iteration` is supplied; iteration receipts are marked non-qualifying.

These scripts never authorize Stripe live mode, real-value settlement, or
unrestricted public access.
