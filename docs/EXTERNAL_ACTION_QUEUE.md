# External action queue

Everything below needs a human, an account, paid infrastructure, or a decision
that is not the code's to make. Nothing here can be closed by writing software.

For the **readiness facet** specifically (current machine-derived **84/100**,
which is the machine-reachable ceiling without staging/offsite/approvers), use
the ordered operator checklist in `docs/FACET_EXTERNAL_ACTION_PACK.md`. This
queue remains the broader external inventory for canary inputs and rename
cutovers.

Generated from measured probes, not from intent. Re-derive with:

```bash
set -a; . ./.env; set +a
bash scripts/stripe-sandbox.sh check
python3 -c "import json;d=json.load(open('evidence/canary/private-canary.json'));print(d['lanes_canary_proven'],'/',d['lanes_total'])"
```

## The headline

**A live Stripe key is not on this list, and cannot be.**
`scripts/lib/go-closure-common.sh:155` *refuses* anything but `sk_test_*` /
`rk_test_*`, and the payment authority reports `live_mode: PROHIBITED`. The
formal canary is test-mode by policy. Supplying a live key today would be
rejected by the very gate that mints release authority.

Measured Stripe state: `api_key: test`, `billing_webhook: webhook`,
`connect_webhook: webhook`, connect test account present, endpoint IDs distinct,
settlement currency `cad`. Every credential the sandbox contract asks for is
present.

Candidate-bound canary lanes: **0 of 21**.

## 1. Staging deployment — blocks the most

The go-closure canary rehearsal cannot run without a real staging host. This one
item gates the majority of the 21 lanes.

| Input | What it is |
|---|---|
| `STAGING_TLS_HOSTNAME` | A DNS name with TLS. The only failing field in the Stripe contract check (`staging_hostname_valid: false`), because Stripe webhook endpoints must reach a public HTTPS host. |
| `STAGING_DEPLOYMENT_ROOT` | Where the candidate is deployed and run from. |
| `MERC_CANDIDATE_CONTROL_IMAGE`, `MERC_PRIOR_CONTROL_IMAGE` | Immutable `@sha256:` image references. The rehearsal binds receipts to exact image identity, so a floating tag will not do. |
| `MERC_CANDIDATE_COMMIT`, `MERC_PRIOR_COMMIT` | The exact commits those images were built from. |
| `MERC_PROMETHEUS_IMAGE`, `MERC_ALERTMANAGER_IMAGE`, `MERC_GRAFANA_IMAGE`, `MERC_NODE_EXPORTER_IMAGE` | Pinned observability images. |
| `MERC_GO_CLOSURE_ENV_FILE` | The `.env.go-closure` holding the above. Deliberately gitignored. |

## 2. Off-host backup — blocks the restore lane

Backup and independent restore is a launch-critical lane, and it is the one the
driver refuses hardest, because a local-only restore is not evidence that the
data survived leaving the host.

| Input | What it is |
|---|---|
| `MERC_BACKUP_OFFSITE` | An S3-compatible bucket on different infrastructure from the deployment. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Restricted credentials for that bucket. Do not reuse the artifact-store keys. |
| `MERC_BACKUP_ENCRYPTION_RECIPIENT`, `MERC_BACKUP_DECRYPTION_IDENTITY_FILE` | An `age` keypair. The private identity must not live on the deployment host. |

## 3. Alerting — blocks the paging lane

| Input | What it is |
|---|---|
| `ALERT_RECEIVER_WEBHOOK_URL` | An HTTPS receiver that actually pages a human. |
| `ALERT_RECEIVER_NAME` | Which receiver, for the Alertmanager route. |

The canary fires a real alert and requires the receiver to record both firing and
resolution. A sink that swallows the alert proves nothing.

## 4. Approved canary participants — an operator decision

| Input | What it is |
|---|---|
| `MERC_CANARY_APPROVED_BUYER_EMAILS` | Exactly two buyers who consented to being canary participants. |
| `MERC_CANARY_APPROVED_WORKER_IDS` | Exactly two supplier workers. **These must be real v4 UUIDs.** The demo seed IDs (`00000000-…-b1`) are version-nibble `0`, and the receipt validator correctly refuses them, so `distinct_metal_agent` cannot pass on seeded workers. |
| `MERC_CANARY_APPROVED_AGENT_VERSIONS`, `MERC_CANARY_APPROVED_BUILD_HASHES` | The reviewed agent build the canary is allowed to attribute work to. |
| `MERC_CANARY_APPROVED_DRIVER_SHA256` | The operator-reviewed digest of the scenario driver. Review the driver, then pin its hash — the rehearsal re-checks the bytes before and after every scenario. |

## 5. Secrets that must be minted, not defaulted

| Input | Note |
|---|---|
| `MERC_TOKEN_KEY` | At least 32 unpredictable bytes. **Copy byte-identically at cutover**: `control/crypto.go` derives the AES key as `sha256(value)`, so regenerating it makes every sealed OAuth token and webhook secret in Postgres permanently undecryptable. |
| `MERC_VERIFICATION_SAMPLE_SECRET` | At least 32 unpredictable bytes. Determines which tasks get sampled; a predictable value lets a supplier know when it is unobserved. |

## 6. Rename cutovers that need the environment moved with the code

`scripts/rename-residue-audit.py` reports `RESIDUE=0` — nothing renameable is
left in the repository. What remains is 408 occurrences that each need an
external change first:

- the `ghcr.io/…/computexchange-control` registry package, and the
  `computexchange/control` image tags in CI;
- `CX_*` environment variables on the droplet and in GitHub Actions secrets —
  the code already reads `MERC_*`, so code and environment must move together
  (one such gap was found and closed locally: `CX_CONNECT_WEBHOOK_SECRET`);
- `/opt/computexchange`, `/etc/computexchange`, `/var/lib/computexchange` —
  real directories on the droplet (supplier agent state now uses `~/.merc`,
  with an in-repo migration from the pre-rebrand home directory);
- Prometheus `job="computexchange-control"` and the `ComputeExchange*` alert
  names — Alertmanager fingerprints the label set, so the receiver's filters
  must be updated in the same change or pages vanish silently.

Recorded receipts under `evidence/` keep the old names permanently. They record
what was actually verified, and rewriting them would make the repository claim
it verified an artifact that was never built.

## 7. Human and legal

- Independent penetration test, and the retest after remediation.
- Legal review of buyer and supplier terms, refund policy, and the Stripe
  account structure.
- A named on-call human for the incident runbook — the support contact is still
  a placeholder.
- Non-author review of the release, which the release doctor checks for.

## Not blocked on you

These are code items and are being worked in-repo, listed so the queue is not
mistaken for the whole picture:

- the `409` compute-versus-economic authority disagreement at job submit;
- a `batch_infer` honeypot, which needs a known answer generated on the exact
  engine build — seeding a generic one would disarm the probe;
- image generation, LoRA train/eval, adapter deploy and tensor-parallel
  execution, which are unimplemented rather than blocked.
