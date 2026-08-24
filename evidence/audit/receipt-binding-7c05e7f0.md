# Lane D — Receipt binding audit

Candidate HEAD (this worktree): `7c05e7f01fc29db497bee78220f608d1aa4f7746`  
`git log -1 --format='%H %ci %s' HEAD`:

```
7c05e7f01fc29db497bee78220f608d1aa4f7746 2026-08-23 22:43:49 -0400 alpha: Accounts v1 support enabled; connected-account creation PASSES
```

Write scope of this lane is this file only. No receipt and no checker was edited.  
`ops/authorization-matrix.json` is not materialized in this sparse worktree; it was read with `git show HEAD:ops/authorization-matrix.json`. A path missing from disk is not evidence it is missing from git.

## Method (applied, not restated as policy)

1. Enumerate every `DOMAIN_RECEIPTS` row in `scripts/validate-readiness.py` (domain + points), plus the two extra paths the same checker fail-closes on: `evidence/external/qualifying-soak-alpha.json` (`soak:alpha-derived`) and `evidence/external/external-alpha-participants.json` (`claim:EXTERNAL_ALPHA_PROVEN`). `NAMED_REVIEWER_RECEIPT` is the same file as the security-domain staging-attack row.
2. For each existing JSON, read the binding-shaped fields that are actually present (commit / HEAD / candidate / producer / run_id / generated_at / provider account). Values below are copied from the files; nothing is inferred from a filename.
3. Classify by **git commit identity of the run**, not by the receipt's own `binding_status` stamp and not by "the file is in the HEAD tree":
   - **BOUND-TO-HEAD** — a field holds a 40-char SHA whose `git cat-file -t` is `commit` and that SHA equals HEAD.
   - **BOUND-TO-OLDER-COMMIT \<sha\>** — such a field exists, `git cat-file -t` is `commit`, SHA ≠ HEAD.
   - **NO-BINDING-FIELD** — no git-commit field. 40-char hex that is **not** a git object (failed `git cat-file -t`) is not a commit binding.
   - **MISSING-FILE** — absent on disk **and** `git cat-file -e HEAD:<path>` fails.
4. For BOUND-TO-OLDER-COMMIT, `git log --oneline <sha>..HEAD -- <paths>` over paths the receipt itself names (harness_revision, source, expected_commit subject, compose_file, workflow identity). Empty log = subject untouched (weaker-but-arguable). Non-empty = subject changed underneath the binding (not arguable).
5. Honest 100/100: operator rule is that no historical unbound receipt earns readiness. Every scoring receipt must name HEAD `7c05e7f01fc29db497bee78220f608d1aa4f7746`.

The receipt-local stamp `binding_status: BOUND | UNBOUND` is reported as a field that was read. It is **not** the Lane D class. Several files stamp `UNBOUND` while still carrying an older `source_commit`; those are BOUND-TO-OLDER-COMMIT.

**No required receipt binds to HEAD.** `rg` / `git grep` for `7c05e7f01fc29db497bee78220f608d1aa4f7746` and `7c05e7f0` under `evidence/` and `ops/authorization-matrix.json` returned no hits. The only required path whose **file** was last committed at HEAD is `evidence/external/stripe-sandbox-matrix.json`, and that object still has `binding_status: "UNBOUND"` and no commit SHA value.

## Counts (unique paths the checker requires)

| class | n | paths |
|---|---|---|
| required unique paths | **26** | 24 `DOMAIN_RECEIPTS` paths + alpha soak + external-participants |
| DOMAIN_RECEIPTS rows (path×domain×points) | 33 | duplicates share one binding |
| BOUND-TO-HEAD | **0** | — |
| BOUND-TO-OLDER-COMMIT | **14** | see table |
| NO-BINDING-FIELD | **8** | see table |
| MISSING-FILE | **4** | privacy / licensing / staffed-abuse / external-participants |

14 + 8 + 4 + 0 = 26.

If counted as DOMAIN_RECEIPTS unique paths only (24): older 13, no-binding 8, missing 3, HEAD 0.

## Classification table

Columns: receipt path | domain | points | binding class | subject code changed since binding? | needs regeneration for honest 100/100? | exact field names read.

Duplicate paths appear once per `DOMAIN_RECEIPTS` row (same class, different domain/points).

| receipt path | domain | points | binding class | subject code changed since binding? | needs regeneration? | exact field names read |
|---|---|---|---|---|---|---|
| evidence/autonomous/registry-verification.json | source_and_ci | 4 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (501 commits on named publish workflow + Dockerfile.control + control/) | YES | binding_status, missing_identity_fields, candidate.source_commit, candidate.image, identity.workflow_source_commit, identity.certificate_identity, honesty.describes_source_commit, honesty.describes_candidate_image, verified_at, updated_at, status |
| evidence/autonomous/supply-chain.json | source_and_ci | 3 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (501 commits on named publish workflow + Dockerfile.control + control/) | YES | binding_status, missing_identity_fields, verification.workflow_commit, verification.certificate_identity, candidate.image, completed_at, kind, status |
| ops/authorization-matrix.json | source_and_ci | 3 | NO-BINDING-FIELD | N/A (no binding SHA). Supplementary: `control/api.go` named in `source` changed after the matrix file's last commit. | YES (re-derive from HEAD `control/api.go` and stamp HEAD; file has no commit field) | source, schema_version, policy.default (no commit / producer / run_id / generated_at) |
| ops/authorization-matrix.json | security | 8 | NO-BINDING-FIELD | N/A (no binding SHA). Same file as the source_and_ci row. | YES | source, schema_version, policy.default |
| evidence/autonomous/technical-exercises.json | security | 6 | BOUND-TO-OLDER-COMMIT `87a818578ed151d87df0139d1e7d31f98f9840b5` | YES (499 commits on scripts/technical-exercises.sh + control/ + ops/authorization-matrix.json, which exact_config names) | YES | binding_status, producer_identity.source_commit.value, producer_identity.build_digest.value, producer_identity.harness_revision.value, producer_identity.exact_config.value, completed_at, status |
| evidence/external/staging-attack-rehearsal.json | security | 1 | BOUND-TO-OLDER-COMMIT `7fe489606edd76176109ec65cad6154a4906d54c` | YES (2 commits on named scripts/alpha-security-suite.py + control/api.go + control/billing.go + control/suppliers.go + ops/authorization-matrix.json) | YES | binding_status, source_commit_recorded, producer_identity.source_commit.value, producer_identity.harness_revision.value, producer_identity.build_digest.value, observations.started_at, observations.finished_at, target.hostname, reviewer.name, reviewer.organization, status |
| evidence/autonomous/payment-simulator.json | money_and_reconciliation | 9 | NO-BINDING-FIELD | N/A (no binding SHA) | YES | binding_status, missing_identity_fields, status, evidence_label, kind, seed |
| evidence/external/stripe-sandbox-matrix.json | money_and_reconciliation | 6 | NO-BINDING-FIELD | N/A (no binding SHA). File last committed at HEAD, but no commit field. Harness scripts named in `harness.*` changed in parent commits. | YES (also currently status BLOCKED / checker CHECK_FAILED; Connect-complete PASS required) | binding_status, missing_identity_fields, run_id, connect_remainder.run_id, platform_account, fixtures.platform_account, fixtures.connected_account, status, provider_mode, live_mode, harness.nonconnect_driver, harness.connect_remainder_command, kind |
| evidence/autonomous/technical-exercises.json | lifecycle_and_concurrency | 5 | BOUND-TO-OLDER-COMMIT `87a818578ed151d87df0139d1e7d31f98f9840b5` | YES (same file as security row) | YES | binding_status, producer_identity.source_commit.value, producer_identity.harness_revision.value, completed_at, status |
| evidence/autonomous/local-restart-storm.json | lifecycle_and_concurrency | 5 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (501 commits on Dockerfile.control + control/; receipt names source_commit of that local control image) | YES | binding_status, missing_identity_fields, source_commit, immutable_local_image_id, dirty_state_sha256, started_at, finished_at, kind, status |
| evidence/autonomous/logical-independent-restore.json | artifacts_and_storage | 6 | BOUND-TO-OLDER-COMMIT `73bc49da1d92b4189d62f9cfbb4b65de18753208` | NO — weaker-but-arguable. `git log` on named `scripts/local-independent-restore.sh` is empty. | YES (operator rule still requires HEAD stamp) | binding_status, producer_identity.source_commit.value, producer_identity.build_digest.value, producer_identity.harness_revision.value, completed_at, kind, status, external_offsite_restore |
| evidence/external/offsite-backup-verification.json | artifacts_and_storage | 2 | BOUND-TO-OLDER-COMMIT `d8d2e65ad336a87407a9446fafd4e94c7a663554` | YES (1 commit, 380 insertions on named `scripts/offsite-independent-restore.sh`) | YES | binding_status, producer_identity.source_commit.value, producer_identity.build_digest.value, producer_identity.harness_revision.value, verified_at, backup_id, offsite_uri, independence.provider, independence.endpoint_host, kind, status |
| evidence/autonomous/hardware-characterization.json | agent_and_sandbox | 8 | NO-BINDING-FIELD | N/A (no git-commit field. `model_revisions` 40-char hex values are not git objects.) | YES | binding_status, missing_identity_fields, source_identity, runtime_authority_sha256, model_revisions, device, device_model, hardware_class, kind, status |
| evidence/autonomous/logical-independent-restore.json | database_and_recovery | 4 | BOUND-TO-OLDER-COMMIT `73bc49da1d92b4189d62f9cfbb4b65de18753208` | NO — weaker-but-arguable (same file as artifacts row) | YES | binding_status, producer_identity.source_commit.value, producer_identity.harness_revision.value, completed_at, status, external_offsite_restore |
| evidence/autonomous/local-rollback.json | database_and_recovery | 3 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (501 commits on Dockerfile.control + control/; receipt names candidate.source_commit of that image) | YES | binding_status, missing_identity_fields, candidate.source_commit, candidate.image_id, candidate.dirty_state_sha256, started_at, finished_at, kind, status |
| evidence/external/offsite-independent-restore.json | database_and_recovery | 1 | BOUND-TO-OLDER-COMMIT `d8d2e65ad336a87407a9446fafd4e94c7a663554` | YES (same named harness change as offsite-backup) | YES | binding_status, producer_identity.source_commit.value, producer_identity.harness_revision.value, completed_at, backup_id, offsite_uri, independence.provider, kind, status |
| evidence/autonomous/staging-validation.json | deployment_and_rollback | 2 | NO-BINDING-FIELD | N/A (no binding SHA) | YES | binding_status, missing_identity_fields, status, supported_deployment_system, deployment_evidence |
| evidence/autonomous/local-rollback.json | deployment_and_rollback | 2 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (same file as database row) | YES | binding_status, candidate.source_commit, candidate.image_id, started_at, finished_at, status |
| evidence/autonomous/local-restart-storm.json | deployment_and_rollback | 1 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (same file as lifecycle row) | YES | binding_status, source_commit, immutable_local_image_id, started_at, finished_at, status |
| evidence/autonomous/local-soak-60s.json | deployment_and_rollback | 0 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (501 commits on Dockerfile.control + control/; receipt names source_commit of that image) | no (0-pt row; does not move 100/100). Still historical. | binding_status, missing_identity_fields, source_commit, immutable_local_image_id, dirty_state_sha256, started_at, finished_at, kind, status |
| evidence/autonomous/soak-requirement-derivation.json | deployment_and_rollback | 0 | BOUND-TO-OLDER-COMMIT `73bc49da1d92b4189d62f9cfbb4b65de18753208` | YES (14 commits on control/, which exact_config names as `named control/ periods`; harness scripts themselves unchanged) | no (0-pt row; does not move 100/100). Still historical. | binding_status, producer_identity.source_commit.value, producer_identity.harness_revision.value, completed_at, kind, status, conclusion, qualifies_for_24h_gate |
| evidence/external/qualifying-soak-24h.json | deployment_and_rollback | 3 | BOUND-TO-OLDER-COMMIT `9e31c65b27860d659d7ce972e2de7052691c0642` | YES (6 commits on scripts/soak/soak24.py + control/ + Dockerfile.control) | YES (also status IN_PROGRESS, kind/schema do not match the checker) | binding_status, expected_commit, candidate.expected_commit, last_version.commit, producer_identity.source_commit.value, producer_identity.harness_revision.value, started_at, updated_at, host, status, kind, mode |
| evidence/autonomous/alert-pipeline-simulation.json | observability_and_alerting | 3 | NO-BINDING-FIELD | N/A (no binding SHA) | YES | binding_status, missing_identity_fields, status, label, profile, receiver |
| evidence/autonomous/alert-page-simulation.json | observability_and_alerting | 2 | NO-BINDING-FIELD | N/A (no binding SHA) | YES | binding_status, missing_identity_fields, status, label, profile, receiver |
| evidence/autonomous/alert-delivery-r1.json | observability_and_alerting | 1 | BOUND-TO-OLDER-COMMIT `9e31c65b27860d659d7ce972e2de7052691c0642` | NO — weaker-but-arguable. `git log` on named scripts/test-alert-delivery.sh + docker-compose.observability.yml + docs/RUNBOOKS.md is empty. | YES (operator rule still requires HEAD stamp) | binding_status, producer_identity.source_commit.value, producer_identity.harness_revision.value, completed_at, compose_file, receiver.url_host, delivery.firing_received_at, delivery.resolved_received_at, kind, status |
| evidence/autonomous/technical-exercises.json | privacy_and_data_governance | 3 | BOUND-TO-OLDER-COMMIT `87a818578ed151d87df0139d1e7d31f98f9840b5` | YES (same file as security row) | YES | binding_status, producer_identity.source_commit.value, completed_at, status |
| evidence/external/privacy-qualified-approval.json | privacy_and_data_governance | 1 | MISSING-FILE | N/A | YES (create, bound to HEAD) | (file absent on disk and in HEAD) |
| evidence/autonomous/supply-chain.json | licensing_and_supply_chain | 2 | BOUND-TO-OLDER-COMMIT `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | YES (same file as source_and_ci row) | YES | binding_status, verification.workflow_commit, completed_at, status |
| evidence/external/licensing-provenance-approval.json | licensing_and_supply_chain | 1 | MISSING-FILE | N/A | YES (create, bound to HEAD) | (file absent on disk and in HEAD) |
| evidence/autonomous/technical-exercises.json | abuse_and_trust | 1 | BOUND-TO-OLDER-COMMIT `87a818578ed151d87df0139d1e7d31f98f9840b5` | YES (same file as security row) | YES | binding_status, producer_identity.source_commit.value, completed_at, status |
| evidence/external/staffed-abuse-route-or-tabletop.json | abuse_and_trust | 1 | MISSING-FILE | N/A | YES (create, bound to HEAD) | (file absent on disk and in HEAD) |
| evidence/autonomous/technical-exercises.json | support_and_incident_response | 1 | BOUND-TO-OLDER-COMMIT `87a818578ed151d87df0139d1e7d31f98f9840b5` | YES (same file as security row) | YES | binding_status, producer_identity.source_commit.value, completed_at, status |
| evidence/autonomous/website-validation.json | website_and_buyer_usability | 2 | NO-BINDING-FIELD | N/A (no binding SHA). honesty.does_not_describe names HEAD explicitly as out of scope. | YES | binding_status, missing_identity_fields, completed_at, honesty.record_class, honesty.event_date, honesty.does_not_describe, verification_commands, status, kind, target.kind |
| evidence/external/qualifying-soak-alpha.json | backend_alpha soak:alpha-derived (not on the 100-point bar) | n/a | BOUND-TO-OLDER-COMMIT `a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05` | YES (17 commits on named scripts/alpha/derived-soak.py + control/main.go + control/; harness+main.go alone: 1 commit) | no (not a 100-point row). Still historical for ALPHA_ENGINEERING_READY. | binding_status, expected_commit, last_version.commit, producer_identity.source_commit.value, producer_identity.image_digest.value, producer_identity.harness_revision.value, started_at, finished_at, host, kind, status |
| evidence/external/external-alpha-participants.json | backend_alpha claim:EXTERNAL_ALPHA_PROVEN (not on the 100-point bar) | n/a | MISSING-FILE | N/A | no (not a 100-point row). Required to flip EXTERNAL_ALPHA_PROVEN. | (file absent on disk and in HEAD) |


## Binding SHA inventory

Every 40-char value used as a **git commit binding** was checked with `git cat-file -t` and `git merge-base --is-ancestor <sha> HEAD`.

| SHA | `git cat-file -t` | `git log -1` | ancestor of HEAD? | distance `rev-list --count sha..HEAD` | used as binding by |
|---|---|---|---|---|---|
| `a4d50d93d0d8e44742fabe9d6b06380e3191b2e5` | commit | 2026-07-19 21:50:04 -0400 release: record exact-head publication receipts | yes | 904 | registry-verification, supply-chain, local-restart-storm, local-rollback, local-soak-60s |
| `87a818578ed151d87df0139d1e7d31f98f9840b5` | commit | 2026-07-20 01:51:27 -0400 release: complete autonomous nonexternal closure | yes | 903 | technical-exercises |
| `7fe489606edd76176109ec65cad6154a4906d54c` | commit | 2026-08-17 11:54:52 -0400 docs: self-describe the two receipts the rewritten closeout newly cites | yes | 16 | staging-attack-rehearsal |
| `73bc49da1d92b4189d62f9cfbb4b65de18753208` | commit | 2026-08-16 20:21:37 -0400 alpha: carry the 1.26.6 toolchain into the image and CI | yes | 46 | logical-independent-restore, soak-requirement-derivation |
| `d8d2e65ad336a87407a9446fafd4e94c7a663554` | commit | 2026-08-16 21:01:37 -0400 alpha: staging satisfied and a real 3600-second soak, both bound to the deployed candidate | yes | 44 | offsite-backup-verification, offsite-independent-restore |
| `9e31c65b27860d659d7ce972e2de7052691c0642` | commit | 2026-08-17 11:28:59 -0400 chore: create scripts/soak so the 24h soak lane has a write root in HEAD | yes | 20 | qualifying-soak-24h, alert-delivery-r1 |
| `a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05` | commit | 2026-08-16 19:40:10 -0400 alpha: governance drafts, generated license inventory, and the honest external residue | yes | 50 | qualifying-soak-alpha |
| `7c05e7f01fc29db497bee78220f608d1aa4f7746` | commit | HEAD | (is HEAD) | 0 | **no required receipt** |

Not git commits (so they do **not** create BOUND-TO-OLDER-COMMIT):

| value | where | `git cat-file -t` |
|---|---|---|
| `1110a243fdf4706b3f48f1d95db1a4f5529b4d41` | hardware-characterization `model_revisions[0][1]` (model id `all-minilm-l6-v2`) | NOT_IN_GIT |
| `b69aef112e9f895e6f98d7ae0949f72ff09aa401` | hardware-characterization `model_revisions[1][1]` (model id `llama-3.2-1b-instruct-q4`) | NOT_IN_GIT |
| `4a4807d69d8747a5` | hardware-characterization `source_identity` | not 40-char |
| `ecbf2fc6cc2b749860cd769d29a6b39bcf76f8c96ad0b785c0cdb29851b1f5c8` | hardware-characterization `runtime_authority_sha256` | 64-char content hash, not a commit |

`19fe0b23940c7e3d4da9b45d9cc5689c2c515d07` and `9e31c65b27860d659d7ce972e2de7052691c0642` also appear inside registry-verification `honesty.later_plane_observations_that_are_not_this_candidate`. Those strings are observations the receipt **disclaims**; the binding of that receipt is `a4d50d93…`.

## Real JSON values (binding fields)

### evidence/autonomous/registry-verification.json

```json
{
  "binding_status": "UNBOUND",
  "missing_identity_fields": ["source_commit", "build_digest", "model_artifact_digest", "image_digest", "harness_revision", "corpus_digest", "exact_config", "raw_samples"],
  "status": "PASS",
  "verified_at": "2026-07-20T04:18:24Z",
  "updated_at": "2026-07-20T04:21:25Z",
  "candidate": {
    "image": "ghcr.io/joshuahickscorp/computexchange-control@sha256:f848a8048af250f7135f54b15d8bf4455bd24af6d42fd4d380dd99e0c1b91563",
    "source_commit": "a4d50d93d0d8e44742fabe9d6b06380e3191b2e5",
    "version": "v0.1.0-rc.1"
  },
  "identity": {
    "certificate_identity": "https://github.com/joshuahickscorp/computexchange/.github/workflows/publish-candidate.yml@refs/heads/release/rc1-go-closure",
    "oidc_issuer": "https://token.actions.githubusercontent.com",
    "workflow_source_commit": "a4d50d93d0d8e44742fabe9d6b06380e3191b2e5"
  },
  "honesty": {
    "record_class": "historical_registry_sbom_verification",
    "event_date": "2026-07-20",
    "describes_source_commit": "a4d50d93d0d8e44742fabe9d6b06380e3191b2e5",
    "describes_candidate_image": "ghcr.io/joshuahickscorp/computexchange-control@sha256:f848a8048af250f7135f54b15d8bf4455bd24af6d42fd4d380dd99e0c1b91563",
    "does_not_describe_running_plane": true
  }
}
```

Producer identity recovered from fields: GitHub Actions OIDC `publish-candidate.yml` at workflow_source_commit `a4d50d93…`, image `ghcr.io/joshuahickscorp/computexchange-control@sha256:f848a8048af250f7135f54b15d8bf4455bd24af6d42fd4d380dd99e0c1b91563`. No `run_id`, no `generated_at` (timestamps are `verified_at` / `updated_at`).

### evidence/autonomous/supply-chain.json

```json
{
  "binding_status": "UNBOUND",
  "kind": "registry_supply_chain_verification",
  "status": "PASS",
  "completed_at": "2026-07-20T05:46:21Z",
  "candidate": {
    "image": "ghcr.io/joshuahickscorp/computexchange-control@sha256:f848a8048af250f7135f54b15d8bf4455bd24af6d42fd4d380dd99e0c1b91563"
  },
  "verification": {
    "certificate_identity": "https://github.com/joshuahickscorp/computexchange/.github/workflows/publish-candidate.yml@refs/heads/release/rc1-go-closure",
    "certificate_issuer": "https://token.actions.githubusercontent.com",
    "workflow_commit": "a4d50d93d0d8e44742fabe9d6b06380e3191b2e5",
    "cosign_version": "3.0.6",
    "trivy_version": "0.66.0"
  }
}
```

### ops/authorization-matrix.json (via `git show HEAD:ops/authorization-matrix.json`)

```json
{
  "schema_version": 1,
  "source": "control/api.go:Server.Routes",
  "policy": { "default": "deny" },
  "roles": ["anonymous", "buyer_owner", "different_buyer", "active_worker", "different_worker", "operator", "revoked_identity", "provider_hmac"]
}
```

Route count from the file: 126. No `commit`, `producer`, `run_id`, or `generated_at`. Last commit that touched the path: `a1c0a55f0f35d60787aeb8bccaaf5d90c00d6870` 2026-08-15 22:32:22 -0400 `ui: versioned composition surface for BUY/EARN/HEALTH, explicitly not a TUI (P8)` — that is the file's git history, not a binding field.

Supplementary (not a binding SHA, so not used for class):

```
$ git log --oneline a1c0a55f0f35d60787aeb8bccaaf5d90c00d6870..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- control/api.go
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
64da4293 alpha: backend-alpha release level, recovery and security suites, live staging money ingress
```

The matrix's named source `control/api.go` changed after the matrix file was last written.

### evidence/autonomous/technical-exercises.json

```json
{
  "binding_status": "BOUND",
  "status": "PASS",
  "completed_at": "2026-07-20T05:49:23Z",
  "producer_identity": {
    "source_commit": { "value": "87a818578ed151d87df0139d1e7d31f98f9840b5" },
    "build_digest": { "value": "fdc7eb3a4977f7c46bf43071ccbc0da3ff6c1231907aaff008bcaf0ce8e54097" },
    "harness_revision": { "value": "scripts/technical-exercises.sh" },
    "exact_config": { "value": "NAME=cx-technical-exercises-20260719 PG_IMAGE=postgres:17@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d port=55441; go test DSAR/tabletop/break-glass; qualification.qualified_human_tabletop=NOT EXECUTED qualification.external_subprocessor_deletion=NOT EXECUTED" }
  }
}
```

### evidence/external/staging-attack-rehearsal.json

```json
{
  "binding_status": "BOUND",
  "kind": "external_staging_attack_rehearsal",
  "status": "PASS",
  "source_commit_recorded": "7fe489606edd76176109ec65cad6154a4906d54c",
  "target": { "scheme": "https", "hostname": "mercmerc.net", "url": "https://mercmerc.net" },
  "observations": { "started_at": "2026-08-17T16:12:27Z", "finished_at": "2026-08-17T16:12:57Z" },
  "reviewer": { "name": "", "organization": "", "named_human_reviewer": "unmet" },
  "producer_identity": {
    "source_commit": { "value": "7fe489606edd76176109ec65cad6154a4906d54c" },
    "build_digest": { "value": "172dedb3bc63a5c7533d064d52c765135742901cc137fa2438a6dd4d16f2d02d" },
    "harness_revision": { "value": "scripts/alpha-security-suite.py" }
  }
}
```

### evidence/autonomous/payment-simulator.json

```json
{
  "binding_status": "UNBOUND",
  "missing_identity_fields": ["source_commit", "build_digest", "model_artifact_digest", "image_digest", "harness_revision", "corpus_digest", "exact_config", "raw_samples"],
  "kind": "stripe_provider_simulation",
  "status": "SIMULATED PASS",
  "evidence_label": "SIMULATED",
  "seed": 20260719
}
```

No commit field, no producer, no run_id, no generated_at, no provider account.

### evidence/external/stripe-sandbox-matrix.json

This is the only required receipt whose **blob** was last committed at HEAD (`git log -1 --format='%H %s' -- evidence/external/stripe-sandbox-matrix.json` = `7c05e7f0 alpha: Accounts v1 support enabled; connected-account creation PASSES`). It still does not name HEAD.

```json
{
  "binding_status": "UNBOUND",
  "missing_identity_fields": ["source_commit", "build_digest", "model_artifact_digest", "image_digest", "corpus_digest", "exact_config", "raw_samples"],
  "kind": "stripe_sandbox_matrix",
  "status": "BLOCKED",
  "provider_mode": "test",
  "live_mode": "PROHIBITED",
  "run_id": "20260816T235949Z-l9nc",
  "platform_account": "acct_1TxbzMCwPLrR4vaY",
  "platform_country": "CA",
  "platform_default_currency": "cad",
  "settlement_currency": "cad",
  "harness": {
    "nonconnect_driver": "scripts/stripe-sandbox-nonconnect.sh",
    "connect_remainder_command": "scripts/stripe-sandbox-connect.sh",
    "connect_remainder_alias": "scripts/stripe-sandbox.sh connect",
    "connect_remainder_status": "BLOCKED-ON-CONNECT"
  },
  "connect_remainder": {
    "run_id": "20260824T024126Z-l9cn",
    "status": "BLOCKED",
    "exit_reason": "blocked: external gate"
  },
  "fixtures": {
    "platform_account": "acct_1TxbzMCwPLrR4vaY",
    "connected_account": "acct_1U7npECeWJZCwOUN",
    "billing_webhook_endpoint": "we_1U5Cz2CwPLrR4vaYKXZzRjmn",
    "connect_webhook_endpoint": "we_1U5Cz3CwPLrR4vaYVjElBvu8"
  }
}
```

`connect_gated_remainder` row `connected_account_creation` has `"status": "PASS"`, `"fixture_id": "acct_1U7npECeWJZCwOUN"`. No 40-char git SHA anywhere in the object (`re.findall` of `[0-9a-f]{40}` = `[]`). `source_commit` appears only as a **name** inside `missing_identity_fields`.

Provider identity present: Stripe test platform `acct_1TxbzMCwPLrR4vaY`, connected account `acct_1U7npECeWJZCwOUN`. Producer scripts named; no producer commit.

### evidence/autonomous/local-restart-storm.json

```json
{
  "binding_status": "UNBOUND",
  "source_commit": "a4d50d93d0d8e44742fabe9d6b06380e3191b2e5",
  "immutable_local_image_id": "sha256:bb6cb05263f2e2f9b5d88ea329fc4437d32e56683b9ba3d3aefe99a2383a15c0",
  "dirty_state_sha256": "b5318a071de443496bf39aa7aa8fd3ed699ef69aeea022042dbc730f28b0dad7",
  "started_at": "2026-07-20T05:26:01Z",
  "finished_at": "2026-07-20T05:28:28Z",
  "kind": "local_restart_storm",
  "status": "PASS"
}
```

### evidence/autonomous/logical-independent-restore.json

```json
{
  "binding_status": "BOUND",
  "kind": "logical_independent_restore",
  "status": "PASS",
  "completed_at": "2026-08-17T00:44:07Z",
  "external_offsite_restore": "PASS",
  "producer_identity": {
    "source_commit": { "value": "73bc49da1d92b4189d62f9cfbb4b65de18753208" },
    "build_digest": { "value": "8bda8d2f5a54aa78dfebda85e495e07f0cbec0c7d8460fbf0cad69a911b0f5f6" },
    "harness_revision": { "value": "scripts/local-independent-restore.sh" }
  }
}
```

### evidence/external/offsite-backup-verification.json

```json
{
  "binding_status": "BOUND",
  "kind": "merc_offsite_backup_verification",
  "status": "PASS",
  "verified_at": "2026-08-17T01:18:35Z",
  "backup_id": "20260817T011802Z",
  "offsite_uri": "s3://merc/offsite-alpha/20260817T011802Z",
  "independence": {
    "provider": "cloudflare_r2",
    "endpoint_host": "b0b28181939a4f2a9169fb4a0df36cae.r2.cloudflarestorage.com",
    "boundary": "cloudflare_r2_operator_controlled"
  },
  "producer_identity": {
    "source_commit": { "value": "d8d2e65ad336a87407a9446fafd4e94c7a663554" },
    "build_digest": { "value": "93bb875336f12a7d2819bf1adf3b80b7953ed91fd16979f5bde8b0b92103fa60" },
    "harness_revision": { "value": "scripts/offsite-independent-restore.sh" }
  }
}
```

### evidence/autonomous/hardware-characterization.json

```json
{
  "binding_status": "UNBOUND",
  "missing_identity_fields": ["source_commit", "build_digest", "model_artifact_digest", "image_digest", "harness_revision", "corpus_digest", "exact_config", "raw_samples"],
  "kind": "cx_agent_device_characterization",
  "status": "PASS",
  "physical_devices_observed": 1,
  "device": "metal",
  "device_model": "Mac15,14",
  "hardware_class": "apple_silicon_ultra",
  "source_identity": "4a4807d69d8747a5",
  "runtime_authority_sha256": "ecbf2fc6cc2b749860cd769d29a6b39bcf76f8c96ad0b785c0cdb29851b1f5c8",
  "model_revisions": [
    ["all-minilm-l6-v2", "1110a243fdf4706b3f48f1d95db1a4f5529b4d41"],
    ["llama-3.2-1b-instruct-q4", "b69aef112e9f895e6f98d7ae0949f72ff09aa401"]
  ]
}
```

No git commit. No run_id. No generated_at.

### evidence/autonomous/local-rollback.json

```json
{
  "binding_status": "UNBOUND",
  "kind": "local_immutable_rollback",
  "status": "PASS",
  "started_at": "2026-07-20T05:22:48Z",
  "finished_at": "2026-07-20T05:24:37Z",
  "candidate": {
    "source_commit": "a4d50d93d0d8e44742fabe9d6b06380e3191b2e5",
    "dirty_state_sha256": "b5318a071de443496bf39aa7aa8fd3ed699ef69aeea022042dbc730f28b0dad7",
    "image_id": "sha256:bb6cb05263f2e2f9b5d88ea329fc4437d32e56683b9ba3d3aefe99a2383a15c0"
  }
}
```

### evidence/external/offsite-independent-restore.json

```json
{
  "binding_status": "BOUND",
  "kind": "external_offsite_restore",
  "status": "PASS",
  "completed_at": "2026-08-17T01:18:42Z",
  "backup_id": "20260817T011802Z",
  "offsite_uri": "s3://merc/offsite-alpha/20260817T011802Z",
  "independence": {
    "provider": "cloudflare_r2",
    "endpoint_host": "b0b28181939a4f2a9169fb4a0df36cae.r2.cloudflarestorage.com"
  },
  "producer_identity": {
    "source_commit": { "value": "d8d2e65ad336a87407a9446fafd4e94c7a663554" },
    "harness_revision": { "value": "scripts/offsite-independent-restore.sh" }
  }
}
```

### evidence/autonomous/staging-validation.json

```json
{
  "binding_status": "UNBOUND",
  "missing_identity_fields": ["source_commit", "build_digest", "model_artifact_digest", "image_digest", "harness_revision", "corpus_digest", "exact_config", "raw_samples"],
  "status": "PASS",
  "supported_deployment_system": "Docker Compose v2",
  "deployment_evidence": "NOT EXECUTED"
}
```

No commit, no producer, no run_id, no generated_at.

### evidence/autonomous/local-soak-60s.json

```json
{
  "binding_status": "UNBOUND",
  "source_commit": "a4d50d93d0d8e44742fabe9d6b06380e3191b2e5",
  "immutable_local_image_id": "sha256:634f73849e0384932b094f99d872a0a12064a071169f64fdb588da7c05bb1c83",
  "dirty_state_sha256": "85f1028fb84db3758eb0413f902381064ae3149123a75f6b1f3ecd5ddfd50843",
  "started_at": "2026-07-20T04:53:45Z",
  "finished_at": "2026-07-20T04:55:48Z",
  "kind": "local_resilience_soak",
  "status": "PASS"
}
```

### evidence/autonomous/soak-requirement-derivation.json

```json
{
  "binding_status": "BOUND",
  "kind": "soak_requirement_derivation",
  "status": "PASS",
  "completed_at": "2026-08-17T00:44:10Z",
  "qualifies_for_24h_gate": false,
  "conclusion": "deterministic_coverage_supersedes_arbitrary_24h",
  "producer_identity": {
    "source_commit": { "value": "73bc49da1d92b4189d62f9cfbb4b65de18753208" },
    "harness_revision": { "value": "scripts/derive-recovery-receipts.py" },
    "exact_config": { "value": "soak requirement derivation from named control/ periods" }
  }
}
```

### evidence/external/qualifying-soak-24h.json

```json
{
  "binding_status": "BOUND",
  "schema_version": 1,
  "kind": "qualifying_24h_https_observer",
  "status": "IN_PROGRESS",
  "mode": "qualifying",
  "expected_commit": "9e31c65b27860d659d7ce972e2de7052691c0642",
  "host": "mercmerc.net",
  "started_at": "2026-08-17T15:39:03Z",
  "updated_at": "2026-08-17T15:42:05Z",
  "candidate": {
    "expected_commit": "9e31c65b27860d659d7ce972e2de7052691c0642",
    "observed_commits": ["9e31c65b27860d659d7ce972e2de7052691c0642"]
  },
  "last_version": { "commit": "9e31c65b27860d659d7ce972e2de7052691c0642" },
  "producer_identity": {
    "source_commit": { "value": "9e31c65b27860d659d7ce972e2de7052691c0642" },
    "harness_revision": { "value": "scripts/soak/soak24.py" }
  }
}
```

No `control_image` field (the checker for the 3-point row requires one). Duration in the file: `requested_seconds: 86400`, `elapsed_seconds: 182`.

### evidence/autonomous/alert-pipeline-simulation.json

```json
{
  "binding_status": "UNBOUND",
  "status": "PASS",
  "label": "ALERT PIPELINE SIMULATION",
  "profile": "check",
  "receiver": "harness-controlled loopback receiver"
}
```

### evidence/autonomous/alert-page-simulation.json

```json
{
  "binding_status": "UNBOUND",
  "status": "PASS",
  "label": "ALERT PIPELINE SIMULATION",
  "profile": "page",
  "receiver": "harness-controlled loopback receiver"
}
```

### evidence/autonomous/alert-delivery-r1.json

```json
{
  "binding_status": "BOUND",
  "kind": "alert_delivery",
  "status": "PASS",
  "completed_at": "2026-08-17T15:33:38Z",
  "compose_file": "docker-compose.observability.yml",
  "compose_project": "merc-alert-delivery-60224",
  "receiver": {
    "transport": "alertmanager_webhook",
    "url_host": "http://host.docker.internal",
    "secret_values_recorded": false
  },
  "delivery": {
    "firing_received_at": "2026-08-17T15:33:32Z",
    "resolved_received_at": "2026-08-17T15:33:37Z"
  },
  "producer_identity": {
    "source_commit": { "value": "9e31c65b27860d659d7ce972e2de7052691c0642" },
    "harness_revision": { "value": "scripts/test-alert-delivery.sh" }
  }
}
```

### evidence/autonomous/website-validation.json

```json
{
  "binding_status": "UNBOUND",
  "kind": "website_contract_and_accessibility_validation",
  "status": "PASS_AUTOMATED_BROWSER",
  "completed_at": "2026-07-20T05:41:18Z",
  "target": { "kind": "loopback_static_server", "url_not_tested": "https://mercmerc.net/buyer" },
  "honesty": {
    "record_class": "historical_loopback_run",
    "event_date": "2026-07-20",
    "does_not_describe": "the public /buyer workspace on mercmerc.net, HEAD, or any later site build",
    "binding_status_means": "UNBOUND — producer identity was never recovered for this run"
  },
  "verification_commands": [
    "node scripts/site-build.mjs",
    "Playwright against a loopback static server using the installed Google Chrome executable"
  ]
}
```

### evidence/external/qualifying-soak-alpha.json

```json
{
  "binding_status": "BOUND",
  "kind": "backend_alpha_soak",
  "status": "PASS",
  "expected_commit": "a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05",
  "host": "mercmerc.net",
  "started_at": "2026-08-16T23:58:47Z",
  "finished_at": "2026-08-17T00:58:47Z",
  "last_version": { "commit": "a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05" },
  "producer_identity": {
    "source_commit": { "value": "a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05" },
    "image_digest": { "value": "2b2f85c969176dd5cc84f66e402e0853519f7784bc9f60fcf86841372e9fb28c" },
    "harness_revision": { "value": "scripts/alpha/derived-soak.py" },
    "exact_config": { "value": "backend_alpha_soak 3600s interval=30 host=mercmerc.net commit=a5bca8c0" }
  },
  "qualification": {
    "derived_from": "2x pgxpool MaxConnLifetime=30m (control/main.go)"
  }
}
```

### MISSING-FILE (absent on disk and in HEAD)

Confirmed with `git cat-file -e HEAD:<path>` → `path does not exist in 'HEAD'`:

- `evidence/external/privacy-qualified-approval.json`
- `evidence/external/licensing-provenance-approval.json`
- `evidence/external/staffed-abuse-route-or-tabletop.json`
- `evidence/external/external-alpha-participants.json`

## git log for BOUND-TO-OLDER-COMMIT

Paths are those the receipt names. HEAD is `7c05e7f01fc29db497bee78220f608d1aa4f7746`.

### registry-verification + supply-chain + local-restart-storm + local-rollback + local-soak-60s

Named subject: `.github/workflows/publish-candidate.yml` (certificate_identity / verification), `Dockerfile.control` + `control/` (candidate control image at `source_commit` / `candidate.source_commit`).

```
$ git log --oneline a4d50d93d0d8e44742fabe9d6b06380e3191b2e5..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- .github/workflows/publish-candidate.yml Dockerfile.control control/
```

COUNT: **501** (full oneline list in Appendix A).

```
$ git diff --stat a4d50d93d0d8e44742fabe9d6b06380e3191b2e5..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- Dockerfile.control .github/workflows/publish-candidate.yml
 .github/workflows/publish-candidate.yml | 62 +++++++++++++++++++++++++++---
 Dockerfile.control                      | 68 ++++++++++++++++++++++++---------
 2 files changed, 106 insertions(+), 24 deletions(-)

$ git diff --stat a4d50d93d0d8e44742fabe9d6b06380e3191b2e5..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- control/ | tail -1
 587 files changed, 215063 insertions(+), 7040 deletions(-)
```

First 8 and last 8 of that log:

```
a402acb7 alpha: the receipt names the wall that exists, and redundancy has someone to be redundant with
77b36ea9 alpha: redundancy may only replace a honeypot when redundancy can actually be independent
0ffbd52d alpha: the price board was serving a seed that pointed at the superseded r4 receipt
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
9ba9884e alpha: restore the money contract as a real requirement, and meet it
8142e6e5 alpha: the control suite is green, and the execution loop closes through the public API
86a010cb alpha: the currency fix, at the radius it should have had — 25 failures to 9
01de03ea Revert the currency fix: it cost 55 net tests and my one-line hypothesis was wrong
…
2de6b40e object retention: job payload objects expire 30 days after terminal
8156ce1a supplier console: earnings, payout rail and verification standing
fbe02ce7 release: harden soak observability and continuity
87a81857 release: complete autonomous nonexternal closure
```

Subject code **changed**. Not weaker-arguable.

### technical-exercises (`87a81857`)

Named: `scripts/technical-exercises.sh`; exact_config names `go test DSAR/tabletop/break-glass` and `authorization-matrix check`.

```
$ git log --oneline 87a818578ed151d87df0139d1e7d31f98f9840b5..HEAD -- scripts/technical-exercises.sh control/ ops/authorization-matrix.json
```

COUNT: **499**. Overlaps the control/ log above. Harness + matrix only:

```
$ git log --oneline 87a818578ed151d87df0139d1e7d31f98f9840b5..HEAD -- scripts/technical-exercises.sh
ded18b54 rename: zero-residue audit, RESIDUE 0, gated in CI
8156ce1a supplier console: earnings, payout rail and verification standing
```

Subject code **changed**.

### staging-attack-rehearsal (`7fe48960`)

Named: `scripts/alpha-security-suite.py`, `control/api.go`, `control/billing.go`, `control/suppliers.go`, `ops/authorization-matrix.json`.

```
$ git log --oneline 7fe489606edd76176109ec65cad6154a4906d54c..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- scripts/alpha-security-suite.py control/api.go control/billing.go control/suppliers.go ops/authorization-matrix.json
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
9ba9884e alpha: restore the money contract as a real requirement, and meet it
```

COUNT: **2**. Subject code **changed**.

Harness-only:

```
$ git log --oneline 7fe489606edd76176109ec65cad6154a4906d54c..HEAD -- scripts/alpha-security-suite.py
9ba9884e alpha: restore the money contract as a real requirement, and meet it
```

### logical-independent-restore (`73bc49da`) — weaker-but-arguable

Named: `scripts/local-independent-restore.sh`. Last commit on that path is `b135257796f3c87ad3829db0d99b9ee01deb4052`, an ancestor of `73bc49da`.

```
$ git log --oneline 73bc49da1d92b4189d62f9cfbb4b65de18753208..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- scripts/local-independent-restore.sh
```

COUNT: **0** (empty). Subject code named by the receipt is **untouched**. Weaker-but-arguable. Operator rule still requires a HEAD-stamped re-run for the points to be honest.

### soak-requirement-derivation (`73bc49da`)

Named: `scripts/derive-recovery-receipts.py`, `scripts/test-backup-age-metric.sh`, and exact_config `named control/ periods`.

```
$ git log --oneline 73bc49da1d92b4189d62f9cfbb4b65de18753208..HEAD -- scripts/derive-recovery-receipts.py scripts/test-backup-age-metric.sh
```

COUNT: **0** (harness untouched).

```
$ git log --oneline 73bc49da1d92b4189d62f9cfbb4b65de18753208..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- scripts/derive-recovery-receipts.py scripts/test-backup-age-metric.sh control/
a402acb7 alpha: the receipt names the wall that exists, and redundancy has someone to be redundant with
77b36ea9 alpha: redundancy may only replace a honeypot when redundancy can actually be independent
0ffbd52d alpha: the price board was serving a seed that pointed at the superseded r4 receipt
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
9ba9884e alpha: restore the money contract as a real requirement, and meet it
8142e6e5 alpha: the control suite is green, and the execution loop closes through the public API
86a010cb alpha: the currency fix, at the radius it should have had — 25 failures to 9
01de03ea Revert the currency fix: it cost 55 net tests and my one-line hypothesis was wrong
e3af8bce alpha: the two correctness defects were one, and it was the fixtures lying about currency
066f9189 alpha: finish the cell authority migration and make make-ci complete honestly
33e220d3 alpha: closeout condition 13 closes — evidence binding exits clean
6ce6a3a9 alpha: land three dead lanes' preserved work and make the cell diagnostic compile
fcf7bdf6 A-execution-loop-20260816-223450: preserve work from a lane that died without reporting
19fe0b23 alpha-cells-20260816-202101: preserve work from a lane that died without reporting
```

COUNT: **14**. Because exact_config names `control/` periods, subject code **changed**. (0-pt row.)

### offsite-backup-verification + offsite-independent-restore (`d8d2e65a`)

Named: `scripts/offsite-independent-restore.sh`.

```
$ git log --oneline d8d2e65ad336a87407a9446fafd4e94c7a663554..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- scripts/offsite-independent-restore.sh
d5ec4617 alpha: the live droplet's own data now goes offsite and comes back
```

COUNT: **1**. `git show --stat d5ec4617 -- scripts/offsite-independent-restore.sh`:

```
scripts/offsite-independent-restore.sh | 430 +++++++++++++++++++++++++++++----
 1 file changed, 380 insertions(+), 50 deletions(-)
```

The receipt **files** were also updated in `d5ec4617`, but `producer_identity.source_commit.value` remains `d8d2e65a`. Subject code **changed** since the named binding. Not weaker-arguable.

### qualifying-soak-24h (`9e31c65b`)

Named: `scripts/soak/soak24.py`; `expected_commit` / `last_version.commit` is the deployed control.

```
$ git log --oneline 9e31c65b27860d659d7ce972e2de7052691c0642..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- scripts/soak/soak24.py control/ Dockerfile.control
a402acb7 alpha: the receipt names the wall that exists, and redundancy has someone to be redundant with
77b36ea9 alpha: redundancy may only replace a honeypot when redundancy can actually be independent
0ffbd52d alpha: the price board was serving a seed that pointed at the superseded r4 receipt
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
9ba9884e alpha: restore the money contract as a real requirement, and meet it
3012f8a3 alpha: land eight wave lanes, including an adversarial audit that found twelve things
```

COUNT: **6**. Subject code **changed**.

Harness-only:

```
$ git log --oneline 9e31c65b27860d659d7ce972e2de7052691c0642..HEAD -- scripts/soak/soak24.py
3012f8a3 alpha: land eight wave lanes, including an adversarial audit that found twelve things
```

### alert-delivery-r1 (`9e31c65b`) — weaker-but-arguable

Named: `scripts/test-alert-delivery.sh`, `docker-compose.observability.yml`, `docs/RUNBOOKS.md`.

```
$ git log --oneline 9e31c65b27860d659d7ce972e2de7052691c0642..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- scripts/test-alert-delivery.sh docker-compose.observability.yml docs/RUNBOOKS.md
```

COUNT: **0** (empty). Subject code named by the receipt is **untouched**. Weaker-but-arguable. Operator rule still requires a HEAD-stamped re-run for the 1 point to be honest.

### qualifying-soak-alpha (`a5bca8c0`) (not on the 100-point bar)

Named: `scripts/alpha/derived-soak.py`, `control/main.go` (`qualification.derived_from`).

```
$ git log --oneline a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- scripts/alpha/derived-soak.py control/main.go control/
a402acb7 alpha: the receipt names the wall that exists, and redundancy has someone to be redundant with
77b36ea9 alpha: redundancy may only replace a honeypot when redundancy can actually be independent
0ffbd52d alpha: the price board was serving a seed that pointed at the superseded r4 receipt
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
9ba9884e alpha: restore the money contract as a real requirement, and meet it
8142e6e5 alpha: the control suite is green, and the execution loop closes through the public API
86a010cb alpha: the currency fix, at the radius it should have had — 25 failures to 9
01de03ea Revert the currency fix: it cost 55 net tests and my one-line hypothesis was wrong
e3af8bce alpha: the two correctness defects were one, and it was the fixtures lying about currency
066f9189 alpha: finish the cell authority migration and make make-ci complete honestly
33e220d3 alpha: closeout condition 13 closes — evidence binding exits clean
6ce6a3a9 alpha: land three dead lanes' preserved work and make the cell diagnostic compile
fcf7bdf6 A-execution-loop-20260816-223450: preserve work from a lane that died without reporting
19fe0b23 alpha-cells-20260816-202101: preserve work from a lane that died without reporting
d8d2e65a alpha: staging satisfied and a real 3600-second soak, both bound to the deployed candidate
926edfe3 alpha: close the govulncheck finding, correct the mutation catalogue, and record what the rehearsal could not reach
b1352577 alpha: offsite backup proven across a real provider boundary, Stripe test matrix driven to the Connect wall
```

COUNT: **17**. Subject code **changed**.

```
$ git log --oneline a5bca8c0..HEAD -- scripts/alpha/derived-soak.py control/main.go
d8d2e65a alpha: staging satisfied and a real 3600-second soak, both bound to the deployed candidate
```

## What must be regenerated for an honest 100/100

Operator rule: no historical unbound receipt earns readiness. **Zero** scoring receipts bind to `7c05e7f01fc29db497bee78220f608d1aa4f7746`. Every path below currently either (a) earns points from an older/unbound document or (b) must be created/re-run to close the remaining 13 external points.

### Must re-run / re-stamp at HEAD (currently exist; would otherwise keep earning historical points)

| path | points at stake | note |
|---|---|---|
| evidence/autonomous/registry-verification.json | 4 | image/workflow at a4d50d93, 904 commits back |
| evidence/autonomous/supply-chain.json | 3+2 | same image/workflow |
| ops/authorization-matrix.json | 3+8 | no commit field; named source `control/api.go` moved after last matrix commit |
| evidence/autonomous/technical-exercises.json | 6+5+3+1+1 | bound to 87a81857; control/ and matrix moved |
| evidence/external/staging-attack-rehearsal.json | 1 | bound to 7fe48960; api.go/security suite moved; named reviewer still empty |
| evidence/autonomous/payment-simulator.json | 9 | no commit field |
| evidence/autonomous/local-restart-storm.json | 5+1 | source_commit a4d50d93 |
| evidence/autonomous/logical-independent-restore.json | 6+4 | weaker-but-arguable harness; still not HEAD |
| evidence/external/offsite-backup-verification.json | 2 | bound to d8d2e65a; harness rewritten in d5ec4617 |
| evidence/autonomous/hardware-characterization.json | 8 | no git-commit field |
| evidence/autonomous/local-rollback.json | 3+2 | candidate.source_commit a4d50d93 |
| evidence/external/offsite-independent-restore.json | 1 | same as offsite-backup |
| evidence/autonomous/staging-validation.json | 2 | no commit field; deployment_evidence NOT EXECUTED |
| evidence/autonomous/alert-pipeline-simulation.json | 3 | no commit field |
| evidence/autonomous/alert-page-simulation.json | 2 | no commit field |
| evidence/autonomous/alert-delivery-r1.json | 1 | weaker-but-arguable harness; still not HEAD |
| evidence/autonomous/website-validation.json | 2 | honesty.does_not_describe names HEAD as out of scope |

### Must produce at HEAD (currently fail or missing; needed to leave 87 and reach 100)

The checker's own ceiling comment: local receipts 84, offsite pair +3 = 87, remaining 13 on other `evidence/external/` rows.

| path | points | why it does not currently pay |
|---|---|---|
| evidence/external/stripe-sandbox-matrix.json | 6 | `status: BLOCKED`; no commit SHA; Connect remainder incomplete (no `tr_`, payout hold/release/failure/reversal false). File was edited at HEAD but is still UNBOUND. |
| evidence/external/qualifying-soak-24h.json | 3 | `status: IN_PROGRESS`, `kind: qualifying_24h_https_observer`, `schema_version: 1`; checker wants `go_closure_soak` schema 2, PASS, ≥86400s, `control_image`. Bound to 9e31c65b not HEAD. |
| evidence/external/privacy-qualified-approval.json | 1 | MISSING-FILE |
| evidence/external/licensing-provenance-approval.json | 1 | MISSING-FILE |
| evidence/external/staffed-abuse-route-or-tabletop.json | 1 | MISSING-FILE |
| evidence/external/staging-attack-rehearsal.json | 1 | exists and is PASS on content, but bound to 7fe48960 not HEAD (also listed above) |

### Do not move the 100-point bar (still historical)

- `evidence/autonomous/local-soak-60s.json` (0 pt) — source_commit a4d50d93
- `evidence/autonomous/soak-requirement-derivation.json` (0 pt) — 73bc49da; control/ periods moved
- `evidence/external/qualifying-soak-alpha.json` — backend-alpha soak, bound to a5bca8c0
- `evidence/external/external-alpha-participants.json` — MISSING; EXTERNAL_ALPHA_PROVEN claim, not a domain point

Weaker-but-arguable (subject paths named by the receipt are untouched since the binding SHA; operator rule still says regenerate):

1. `evidence/autonomous/logical-independent-restore.json`
2. `evidence/autonomous/alert-delivery-r1.json`

## Fail-closed extras (not scoring receipts)

`refuse_canary_rehearsal_as_external()` globs `evidence/canary/l11-p1-canary-rehearsal-*.json` if present. Six files exist. They are not DOMAIN_RECEIPTS. Each has `binding_status: "UNBOUND"`, `missing_identity_fields` including `source_commit`, and both `deployed_commit` and `local_head` equal to `a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05` (older than HEAD). Not counted in N.

## VERIFY

Command (this worktree, sparse checkout):

```
python3 scripts/validate-readiness.py
```

Real output:

```
readiness: FAIL: cannot load readiness ledgers: [Errno 2] No such file or directory: '/Users/scammermike/.claude-grok/worktrees/D-receipt-binding-20260823-225709/ops/readiness.json'
```

Exit code 1. `ops/readiness.json` and `ops/go-no-go.json` are not in the sparse checkout roots, so the checker cannot compute the derived score here. `DOMAIN_RECEIPTS` and the content checkers were read from `scripts/validate-readiness.py` on disk. Receipt JSON was read from disk where present, else `git show HEAD:<path>`.

## Appendix A — full `git log --oneline` for a4d50d93..HEAD on publish workflow + Dockerfile.control + control/

Command:

```
git log --oneline a4d50d93d0d8e44742fabe9d6b06380e3191b2e5..7c05e7f01fc29db497bee78220f608d1aa4f7746 -- .github/workflows/publish-candidate.yml Dockerfile.control control/
```

COUNT: 501

```
a402acb7 alpha: the receipt names the wall that exists, and redundancy has someone to be redundant with
77b36ea9 alpha: redundancy may only replace a honeypot when redundancy can actually be independent
0ffbd52d alpha: the price board was serving a seed that pointed at the superseded r4 receipt
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
9ba9884e alpha: restore the money contract as a real requirement, and meet it
8142e6e5 alpha: the control suite is green, and the execution loop closes through the public API
86a010cb alpha: the currency fix, at the radius it should have had — 25 failures to 9
01de03ea Revert the currency fix: it cost 55 net tests and my one-line hypothesis was wrong
e3af8bce alpha: the two correctness defects were one, and it was the fixtures lying about currency
066f9189 alpha: finish the cell authority migration and make make-ci complete honestly
33e220d3 alpha: closeout condition 13 closes — evidence binding exits clean
6ce6a3a9 alpha: land three dead lanes' preserved work and make the cell diagnostic compile
fcf7bdf6 A-execution-loop-20260816-223450: preserve work from a lane that died without reporting
19fe0b23 alpha-cells-20260816-202101: preserve work from a lane that died without reporting
73bc49da alpha: carry the 1.26.6 toolchain into the image and CI
926edfe3 alpha: close the govulncheck finding, correct the mutation catalogue, and record what the rehearsal could not reach
b1352577 alpha: offsite backup proven across a real provider boundary, Stripe test matrix driven to the Connect wall
14db5398 gofmt recovery lane test
64da4293 alpha: backend-alpha release level, recovery and security suites, live staging money ingress
11813c8f alpha: honest historical-evidence citations and a route tripwire that matches the matrix
41c2ead6 render: L1 digest fast path, verify pipelining, and the CPU/Metal placement rule
44d245d9 render: MEASURED headless Cycles baseline and standalone-Cycles archaeology (P4)
017c7d10 alpha: make the candidate honest to itself (P9, local only)
a1c0a55f ui: versioned composition surface for BUY/EARN/HEALTH, explicitly not a TUI (P8)
a8fdecf5 placement: let the money seam accept multi-family placement v4 (P3)
96ed6deb workload: accept and persist buyer objective as a binding field (P6)
ed3a157d wm: make World Model provenance laundering fail closed (P7)
b89152ee perf: index the heartbeat capacity subquery, and land the P1 hot-path profile
382df92c compactness: drop the write-only device_slot cache (P2)
dc021eb8 liveness: close the last three partially-covered P0 invariants
5e55029a liveness: settle from offer state written by the detached path, not just the blocking one
61db3cd3 liveness: measure the write amplification the flag actually retires, and enumerate the divergence
ff5a6531 liveness: make the offer→slot mapping self-verifying and detached writes ordered
375ab9be liveness: take the durable heartbeat write off the authenticated fast path (P0A)
42c9130d liveness: flag-gated selection flip — offer-grain index authoritative for realtime routing (G082)
da97ff4e liveness: re-key the live index per-offer so it matches the money-selection plane (G082)
94b7abd4 test: clone isolated test DBs with STRATEGY file_copy (halves the per-clone tax)
deac3e9b docker: ship the runtime-cell binding receipts the catalogue cites at boot
1d85dbf1 power: split the economic-envelope boot gate from the joules-measurement gate
65c004db liveness: shadow-wire the live-index in production + prove SQL-liveness parity (G082, inert)
b1f593b4 liveness: compact in-process live-device index — 12.4 B/device, 3.5M hb/s, fail-closed (G082 core)
6d9dcb55 hetero: acceptable-quality-contract gate for honest Metal/CUDA substitutability (G024, software prereq)
9c5a9f25 liveness: batched coalesced heartbeats ~2x a droplet's device ceiling; measured curve (G073/G081)
90ff8869 test: schema-template + safe parallelism + integration tiering (G080, partial)
f4c2cc68 g070: bind llama batch_infer cell so boot is one power-measurement away
99a32fc9 wm/replay: per-phase predicted-vs-actual decomposition and shadow regret (G021/G053)
9fa74da0 wm: World Model V2.2 foundation — schema, roles, observation root, boundary proof
883af0a8 containment: the sandbox flag was a field the supplier set, and nothing checked who set it
b5e2c9ba Merge branch 'grok/money-pool-p1-20260810-122752'
1c754fcb money: admission counted free credit and prepaid as one pool, settlement counted them as two
ddff433e Merge branch 'grok/lease-contract-20260810-121911'
efac9aa2 lease: merc was selling elasticity it had no code path to deliver, and an SLO with no tail
14fba52f realtime: the branch only needed to know there were two offers, and it counted the whole book
5892b330 scale: the instability detector missed the artifact that motivated it
70c37bf9 scale: the full curve, and the two ways it was still able to publish a number that was not a measurement
33874270 scale: a p50 only describes a cell if the cell has one mode
9da3911c reuse: idempotency was tested on the wrong axis, and the right one is what the caller is told
da50cd9c scale: seeding had become most of the run, and it is setup rather than measurement
4b0db027 replay: the retry backoff vanished from the accounting, on exactly the slowest tasks
a9430880 pricing: the reuse arm of the same ceiling gap
a1ca2df3 pricing: the buyer's ceiling was proven in USD and never in the currency they pay
863ef63b scale: the harness was measuring its own lock wait and calling it selection cost
a5ece045 replay: the capture that "was never taken" was already on disk, unread
2a741d4d step26: promoting a workload class is three lines, and nothing noticed the set growing
02686dd5 lease: a retried heartbeat must not re-charge an interval, and "not twice" needed defining first
267fdbc6 step25: the absent elimination classes were named only in plan prose
05b77c44 step27: two of the four fabrics close as refusals, and one of those refusals lived only in a comment
ebdfe205 step22: a fault catalogue that omits what it cannot attack reads exactly like one that covers it
353840cb scale: a cell that says every sample failed, without saying what refused, costs a whole second run
7960f143 realtime: the ranking was hauling 132MB of payload to disk to order rows by four scalars
a984b8f7 mutation: three mutants had stopped pointing at anything, and one of them was hiding a real gap
afa232bb money: the buyer rails had no ordering test, and that is where all five defects lived
d718c085 merge: network mutants 110-124 and the scale harness that stopped timing refusals
0b9e9086 checkpoint: re-proving unchanged bytes was the tax that got paid with --skip-mutation
699e1fe0 mutants+harness: attack the honesty properties, and stop timing refusals
c3b7a5f4 money: the netting rule was a string a future writer had to remember
9ad5bafc runtime+narrowing+money: name the basis, bound what can be bounded, restore what was debited
ca6987a3 money: admission held the ceiling, and refund handed the same cash back
6c145431 finality: a lane that cannot say it is final was saying nothing at all
db94746a verification: a buyer-visible claim could name an evaluator that never ran
8e6b1024 market+topology: record what actually cleared, and refuse what physics refuses
9a5ae9ba evidence: the digests were all there and nothing tied them into one chain
dd59aa62 money: prepaid admission never joined the lock the other two rails share
0f3202d0 deferral: the buyer paid for speed, and the claim path never read the promise
a7bf17a6 money: three rails disagreed about what a buyer already owes, and two under-held
e138a67f prefix: the routing is real, the savings are not attributed, so claim neither
c9d35603 step6: capability is what is true of a node, routability is what we allow it
62208239 nanos: the remainder carry is a property of the type, not of the system
0bae3b07 honeypot: the buyer surface names the probe, and only admission makes that safe
c9c69668 mutation: the step 5 money authority had no mutants at all
58e410e6 security: containment was recorded and never required
11764383 step5: accepted money must finish in the currency it was accepted in
53ea0697 step4: the fixtures assumed a routability that step 4 deliberately removed
7f51e9dc step5: the envelope held exact nanos and the ordinary path held rounded micros
5ab2f8f1 step5: a refusal should send the reader to the actual fault
36ad3741 step5: the fixtures were half-converted, not the money path
97ed15dd step5: freeze the cost policy and make currency a first-class denominator
c27f4451 test: refresh runtime economics mutants
7c542b50 test: align mutation preflight with current authority
496aeac4 test: isolate synthetic runtime activation
5c564f5c test: bind frozen candidate fixture to current authority
0450da06 network-v2: close runtime-cell economics authority
c4c254fd Optimize mutation validation gates
5f6144cb Hydrate LFS for isolated mutation workers
ae9b192c Speed up mutation checkpoint contracts
d510284b Isolate parallel mutation databases
b7ea98f6 Parallelize candidate mutation checks
26d91790 reduce: the audited true reductions, executed -- 5,482 authored lines, nothing else
03f07d36 supplier: the agent could execute work but never enrol itself, and never show its trail
05652bbf buyer: a stranger can now finish the loop without dropping to curl
9528078a money: guard the structure, because a filename cannot follow a function to another file
3a920c62 economics: price the cell, not the model, because duration cancels and provider cost does not
b1c5ce8e evidence: hash the bytes ourselves, because fsck passed while two objects were corrupt
80e43cc8 docs: the 68-to-19 merge preserved every atom and broke every inbound link
644ccee6 P0: CI hydrates tier-2 evidence, and the fingerprint binds the payload not the disk
5ffce714 L3: delete uncited control/evidence perf orphan
f1f518f0 L1: install git-lfs substrate without converting files
aa9ab10b chore(layout): move proof, census, and artifacts under evidence/
e299c0b7 chore(layout): move monitoring/ under ops/monitoring/
d6ac3e18 chore(layout): assemble clients/ from sdk, cli, macapp, proto
a7d4d69d feat: say which authority forces the cost tie, and by what margin it holds
b8a4ccbf fix: the prefix belief could never be corrected, because nothing sent the signal
a31f3a35 feat: freeze the cell's economics beside the money, not beside the benchmark
072ded5e placement: make heterogeneous readiness a self-answering gate
e78c4cc1 test: mutate the selector invariants, and close the two holes that found
e918189e feat: the engine question, measured by something that can name what it ran
095f69d3 feat: a cost tie must not be sold as a cost win, and an unbound number may not rule
585fb2f4 merge: cell economics exist, and they prove the tie is arithmetic
a2d6c078 feat: cell economics exist, and they prove the tie is arithmetic
2d4f6a75 measure: prefix affinity is physically confirmed on Metal, not belief
9092fbd4 style: gofmt pricing.go after the citation-honesty comments landed
64d41d64 docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
8cff3335 fix: two operator selector routes were serving without a reviewed authorization decision
e571dc30 fix: the pricing citation gate made the release image unbootable
2f0c6e96 style: gofmt the files this wave touched
8f711f65 fix: the free-credit rail handed out envelope money a second time
4bb252b5 merge: can admission be durable without being synchronous, and the third rail it exposed
096dd981 design: can admission be durable without being synchronous, and the third rail it exposed
3ec1d547 merge: unblock the selector, and find that pricing is what actually blocks selection
0e75ff18 feat: unblock the selector, and find that pricing is what actually blocks selection
49b71c0e merge: prefix affinity is a real lever, at 71% not the 80-95% the target asks for
c56b6d31 measure: prefix affinity is a real lever, at 71% not the 80-95% the target asks for
f76b5559 fix: five data races in the engine-tournament harness
56053148 merge: the single-hot-offer tail is a fixture artefact
84795c25 docs: the single-hot-offer tail is a fixture artefact, not a production defect
7c256f1a merge: the gate that stops under-powered passes was computed wrong
c8f9d531 fix: the gate that stops under-powered passes was computed under an assumption its own scheduler refutes
b7d1ec75 merge: the product was down to selling one thing
d9786fe3 fix: the product was down to selling one thing
e24a1989 merge: Merc measured against real vLLM on CUDA, and it does not match
0582fc3c evidence: Merc measured against real vLLM on CUDA, and it does not match
091aef3c merge: a real two-engine choice, on identical weights
cab7574c feat: a real two-engine choice, on identical weights
86bfeee0 merge: the authorize tail was one offer row, not the connection pool
6e23d3f9 perf: the authorize tail was one offer row, not the connection pool
f21fc9c6 measure: close the latency accounting, and correct what this wave was aiming at
3aed4370 test: two fixtures encoded a moment rather than a property
c205e43d merge: the harness measured one shape of request, so every claim was about one shape
e097601f merge: make true net contribution unavailable rather than wrong
a092d1a7 feat: the harness measured one shape of request, so every claim was about one shape
338a5c9c feat: make true net contribution unavailable rather than wrong
72ae4f01 merge: the last routable cell rode a receipt the evidence programme calls unbound
5951d3b6 fix: the last routable cell rode a receipt the evidence programme calls unbound
490301e5 evidence: the first honest parity measurement, and it fails
4e926f22 merge: give back the speedup, because the reorder that bought it deadlocked admission
59f90389 fix: give back the speedup, because the reorder that bought it deadlocked admission
a16e9162 merge: a repeatable per-segment latency harness, and the receipt that binds it
76257843 test: make the latency numbers this wave runs on reproducible, and bind them
a255784a merge: take the work that is not money-critical off the first-token path
6c2310f0 perf: take the work that is not money-critical off the first-token path
d6390917 fix: a buyer who hung up was recorded as a request that failed for another reason
387cc90d test: give the spawned agent the seatbelt profile it correctly demands
cca80eb9 merge: a sub-micro job rounded the supplier's share into meaninglessness
53f90a96 fix: a sub-micro job rounded the supplier's share into meaninglessness
0487c043 merge: say what the evidence supports, and stop quoting numbers nobody can reproduce
ab4d2877 docs: say what the evidence supports, and stop quoting numbers nobody can reproduce
bbe81c61 merge: measure the one engine this machine can actually serve, and name the six it cannot
5a329e1d feat: measure the one engine this machine can actually serve, and name the six it cannot
c121efcb fix: three more ways to get a comfortable parity number
a8159ac7 merge: take the buyer funding lock late, and prove the rest of the c=1 path is a floor
e00def06 perf: take the buyer funding lock late, and prove the rest of the c=1 path is a floor
34f672da test: put the two execution-envelope routes under the exhaustive credential check
ca4ab653 merge: two buyer prices cited artifacts that cannot be reproduced
f20c7858 fix: two buyer prices cited artifacts that cannot be reproduced, and the power figures behind the viability gate were invented
f0711cc2 merge: seven tests described the world before the quarantine
3694c66a test: seven tests described the world before the quarantine
0d153af4 merge: the two mechanisms that actually eliminate work now leave receipts
df269e31 feat: the two mechanisms that actually eliminate work now leave receipts
30822234 fix: binding metadata was stamped into files that are payloads, not receipts
4ef1922a merge: evidence that cannot name its producer is refused at write time and in CI
cd64be25 merge: a cell stayed routable on a receipt nobody could reproduce
cd48c9ab merge: a parity harness that could not measure, and a gate that could not refuse
1de03bf0 feat: evidence that cannot name its producer is refused at write time and in CI
fdc8eec1 fix: a cell stayed routable on a receipt nobody could reproduce
1973901f fix: an expired envelope freed money on the refund rail that the funding rail still held
c130345b fix: a parity harness that could not measure, and a gate that could not refuse
ce6422be fix: an expiring envelope released money that was still being spent
09a0cc03 feat: a parity harness whose result could survive an adversary
b1da1f44 Merge branch 'grok/sw-envelope-20260802-202158' into program/network-10of10
0f2a7d18 Merge branch 'grok/sw-reputation-20260802-204128' into program/network-10of10
64488040 Merge branch 'grok/sw-lanes-20260802-202229' into program/network-10of10
9eba1069 Merge branch 'grok/sw-prefix-20260802-204148' into program/network-10of10
83cb1668 Merge branch 'grok/sw-tournament-20260802-202213' into program/network-10of10
57d19e4b Merge branch 'grok/sw-transport-20260802-202128' into program/network-10of10
365e4dbf feat: route to the worker that already holds the prefix, without outranking cost
bc4baab6 perf: stop recomputing every supplier's whole history to admit one request
541382bf fix: do not spend measured latency on an unmeasured gain
9a77cb8f feat: give the engine coherent arrivals instead of an unstructured trickle
c06963a0 feat: a selector with one candidate is a hardcode, so let engines compete
00364e20 feat: a bounded prepaid envelope, so admission stops re-proving funding per request
6d764a5e perf: the gateway kept two idle upstream connections, not one per stream
5bceac40 fix: a delivery that ran no model is not a verified execution
695fa1e9 merge: real containment, rebased onto the claim predicate (D6)
f45c4c96 revert: unland D19 video pending rebase
373a7124 tests: age containment workers offline; cancel residual claimable tasks
277d3fa0 video: gofmt the lane
b22b18f1 merge: d19-video
aefc89db d19-video
6e2a0cdf merge: clear the market on verified-outcome cost (D16)
d9762925 merge: one cost-rank table and executable gates (D10)
76cfd1ed scheduler: one cost-rank table, and gates that execute
e1939f63 merge: wire the complete code that had no caller (D13)
be1a6285 merge: true net contribution, rebased onto the exact settlement authority (D4)
4487c432 claim: floor only disputed cells; unpeered keep claimed rate
e2177165 d6-containment
472ea8fe revert: unland D6 containment pending rebase
6f1f0d05 merge: d8-parity-receipt
0ea7fc4d merge: d6-containment
93209f49 d6-containment
48e01c9f merge: d9-canary-integrity
55849028 merge: d12-keys-bounds
7c9da6c1 d9-canary-integrity
dc0d4cec d12-keys-bounds
49c4d3b0 d16-clearing: verified-outcome rank, measured warm fixtures
e2b24fc0 tests: the residency fixture must stop being eligible when its test ends
84ec9068 tests: the residency seed must clean up after itself
6c9dc98a d13-wire-dead-code
cbbbcc6c revert: unland D16 clearing pending rebase
23862fdb economics: a true net contribution Merc can actually state
18c23cb6 merge: d16-clearing
56dbcea2 d16-clearing
4073e0e0 merge: d15-residency
3d4dbb09 merge: d17-segments
f0ca4c76 d15-residency
b52021e9 d17-segments
6edf334d auth matrix: admit the operator dispute-resolution route
71f77095 merge: disputes can exit and earnings show what is held (D11)
deacdf7b merge: d3-realtime-funding
4a92deff wip: d11-stuck-money
9ddcbcc6 wip: d3-realtime-funding
69870ca7 merge: reuse identity covers every output-affecting field (D5)
6402945a reuse: the cache key must cover everything that changes the answer
a9d0cd2a merge: catalogue re-anchored on append-only schedules (D2)
b5204ce4 settlement: the exact entitlement is what the ledger pays
fc80cd22 parity: give the gateway measurement provenance, a budget and a gate
acd4a081 pricing: re-anchor the catalogue on append-only schedules at validate
a0b15db5 Advance network programme rendering and CAD economics proof
424aae37 Record independent LoRA evaluation evidence
da4198cf Record bounded render assembly evidence
2a4720be Replay immutable fabric topology evaluations
ecf149ea Compose bounded network liquidity receipt
7d10c1b9 Expose buyer-scoped render work units
b6dc1986 Persist buyer-scoped project compile receipts
3b17dc66 Record realtime offer book clearing receipts
a895dce7 Cache bounded realtime tools and schema identity
d018ba73 Fund terminal service lease payouts from prepaid cash
a2dbb86a Expose bounded project compiler route
a1132361 Expose reserved service lease data plane
e33e93a9 Expose selector rollback authority
c9017040 Bind fabric endpoint to topology refusal
48891f90 Align authorization evidence with registered routes
9a46c1dc Bind service lease heartbeats to probe receipts
ce593eff Persist selector promotion evidence receipts
ca1c7391 Bind fabric evidence to topology planning
d322a6b8 Expose selector promotion evidence gate
fc823c5e Expose measured selector regret for operators
f6abda7d Carry compiler-derived topology in workload IR
e9b3d95f Record service offer-book clearing receipts
c4f27044 Plan bounded topology and harden service liquidity evidence
b07e088c Record service lease pricing authority at activation
5a784269 Record terminal service liquidity transitions
c119cf1c Record bounded service market liquidity
b4c947d0 Allow buyers to cancel service leases safely
23443571 Meter exact batch reuse by completion tokens
ce283e27 Settle service leases against prepaid funds
9df7de1a Reserve prepaid funds for service leases
004406c0 Settle supplier payouts in currency minor units
2787dcaa Bind Stripe fee and payout units to settlement currency
86abcc27 Name prepaid billing status in settlement units
9f1d2c14 Bind prepaid topups to exact settlement currency
47c41691 Fail closed on malformed LoRA evaluation commitments
9670b817 Race test service lease capacity refresh
ec9f1a8b Make render work units deterministic
1be9c12c Normalize LoRA dataset record identities
c818ef48 Prevent service lease offer overbooking
d27ed989 Bind public CSP to shipped inline assets
74d46232 Expose governed image capacity in buyer workspace
72bc326e Complete service and project browser surfaces
140a02b5 Surface Fabric collective topology evidence
9b644ca7 Retain peer-bound Fabric collective evidence
981b3bf5 Refuse duplicate records in LoRA dataset probes
78af05f3 Measure fabric meshes and harden Linux agent setup
437264b1 Derive bounded render work plans from project IR
c200816d Validate bounded LoRA datasets before training
1f28fd65 Bind fabric probes to mutual TLS worker identities
f1174ab1 Measure and enforce service lease data plane SLOs
6d300dc9 Bind LoRA project IR to outcome inputs
3be5a4e0 Bind rendering project IR to asset authority
757438de Use fixed point LoRA outcome settlement
92d07fb1 Add metered warm service lease control path
3fee7a42 Bind fabric probes to mutual worker observations
8124e701 Record fabric probes as non-admissible evidence
7dbb1333 Freeze realtime replica service placement
574495c4 Measure realtime market liquidity from live paths
2b618144 Bound advisory prefix routing state per worker
b566237b Stage project roots before dependent artifacts
3f3a3b10 Complete receipt-bound dependent project steps
ba397915 Reserve project ceilings server-side
d9b89a01 Materialize receipt-bound Project IR outputs
c661f3f0 Add Project IR CAD execution proof
726b4859 Bind Project IR dependencies to artifact dataflow
92ef8679 Clarify catalogue pricing authority
e122101f Remove alternate catalogue price derivation
c6e6edff Bind firm quote jobs to exact pricing authority
91d76998 Bind coalesced receipts to physical leaders
faa3016c Name coalescing gross contribution honestly
ecf40364 Prove realtime coalescing money through production handler
e3381d0a Bind supplier shares to physical workload authority
2a1f7195 Review public config and job listing authorization
89700869 Isolate CI and mutation databases
11b46065 Bind RunPod vLLM canary to runtime profile
84aab61b Replay completed prepaid Stripe webhooks
64bf752a Fail closed before unavailable quote pricing
cd6f8e97 Bind release CLI identity
cb87f201 Make realtime reuse idempotent
84ab0f78 Persist realtime reuse pricing authority
a582c9ec Define realtime reuse PricingDecision authority
dafe1270 Make realtime reuse pricing exact
1a92a72f Persist realtime PricingDecision authority
82f2176b Define realtime PricingDecision authority
e6f27695 Make realtime token arithmetic exact
b9917870 Add authority-bound project submission
f98b42a6 Quote project graphs through pricing authority
ca54634f Measure bounded project resource shapes
d71bdffb Resolve project graphs against runtime authority
e2f61668 Bind Workload IR to explicit project dataflow
273da127 Gate project estimates on outcome calibration
28eac9ab Add bounded project Workload IR compiler
d184ed29 Complete buyer and supplier canary surfaces
0a4b66cb feat(pricing): freeze fixed-point conservation authority
4223abf4 fix(economics): allocate fixed costs across charge batches
f0b7df88 test(pricing): kill fixed-point authority mutations
4eb98696 fix(checkpoint): keep readiness and loop receipts reproducible
ca1b63a0 feat: one derivation for quote and floor, and the loop closes
1ae406a7 feat: admission compares exact per-task entitlement, and one authority derives the floor
f6e66043 feat: an exact economic unit, because micro-USD is too coarse to plan in
5fb21c2c feat: the first-complete-loop driver, and the defect it found at admission
d65f65c0 fix: the two-agent receipt test read the invoice one step before it was written
9cf7e784 feat: the cohort receipt lands, and it contradicts the benchmark it was meant to confirm
361cf5f9 fix: the first real cohort found that two cells serving one model cannot differ on price
bab3b244 fix: the shadow-selection insert bound seventeen arguments to fifteen columns
291f4544 feat: every job now records which execution mode it was placed in, and why
c69b0634 feat: a paid RunPod experiment now has a money bound, and the cohort has a harness
eca148d8 test: the reachability census caught the edge the latency rule created
759e8e08 test: the protected-route count follows the reviewed matrix to 66
948e1e21 fix: the cost model could not see a cell that crashed
0c56a2cc fix: a promotion gate that only knew price would have bought latency with it
aa5ecf69 feat: the selector learned what a verified unit costs
58384221 fix: whoever priced first decided the prices for the whole process
9c722fe3 Merge branch 'worktree-wf_63b8f2fa-32c-3' into perf/execution-frontier
cf5d929a Merge branch 'worktree-wf_63b8f2fa-32c-1' into perf/execution-frontier
c5164bb3 perf: a stored pricing decision certified its own supplier rate
c3285f86 fix: a refund replay could return another refund's payment intents
a5046b76 fix: the probe stayed green while the money path closed
78a2f5d5 perf: a default install could never claim any work, and never said why
9c781dd0 fix: no buyer could put money into an account
664b3648 fix: "disable the canary" was one non-empty string away
0abb578a fix: the production image could not start, and nothing noticed
7d6de054 perf: record what the selector would have chosen, over the set admission cannot route
cf7b0f6b fix: renew the coalescing leader's lease, and run the sweep that reclaims it
ee915a7d docs: reconcile the actual HEAD, and withdraw seven claims that outran their wiring
e4fd8993 fix: name the files when the checkpoint's tree moves under it
98f2257d test: the batch reuse key binds runtime authority, not just request shape
c4127cfa fix: the checkpoint refuses to run beside a live mutation runner
391629d4 fix: the mutation step drops object storage and the engine from its environment
af7804f4 fix: a processor with no verifier refuses instead of panicking
a0b52cb8 perf: capability and activation are different things
579edc0e perf: llama.cpp embedding is REAL_RUNTIME_PROVEN
6b45bbca perf: a rejected execution leaves the supplier nothing
f763012a fix: withdraw a governed-reference claim that was never true
6628f57c perf: buyer debit, Merc contribution and receipt authority on both cells
001f1af1 perf: both agents settle through the production path, verified and paid
45e8ab0d perf: both agents execute a directed job end to end, autonomously
cb5b0b58 perf: a directed job reaches the intended agent and no other
5f59452e perf: two real agents enrol autonomously against the production control plane
1deb51e2 perf: let a worker advertise the cell it is being asked to prove
0ab1d535 perf: freeze comparator v2 before it authorizes settlement
41d7c768 perf: version the embedding comparator, and stop a mean hiding a wrong row
2d62ee06 perf: grade llama.cpp's real output through the real verification processor
0bf2ee20 perf: build the artifact harness, and run real engine output through the chain
f9713924 perf: let an operator name a runtime cell, and stop the buyer selecting one
b300d6d6 perf: make runtime authority cell-specific, because the evidence is
c7f86f87 perf: wire in-flight coalescing, and scope reuse to a tenant
bdcb494b test: follow calls into calibration instead of scanning for its name
62603125 perf: give the agent a runtime driver, and measure the embed cell on both
c0cf0936 perf: bind resolved artifacts into the profile digest, and survive the bump
509f9f5a perf: make (runtime_profile_id, revision) the key and retain history
025b54d3 perf: move artifact format from the model to the (runtime, model) pair
f89d1aee perf: Lane A — bind workers to full profile identity, enforce at dispatch
04d447d7 perf: Phase 0 — harden the overhead and profile-digest authorities
e9919166 fix: repair the agent's schema-v2 break, then re-run all four runtimes
c212dcb1 perf: cross-test four runtimes on one harness across Metal and CUDA
c1d52974 perf: cross-test candle against llama.cpp on one harness, on Metal
470aa9f8 perf: measure llama.cpp on the pinned artifact, and bind benchmark evidence
a0abf0a2 perf: add the control-plane runtime adapter boundary
80a8e3d7 perf: govern runtime identity in PostgreSQL, additively
bdcb968f perf: separate overhead cost from base calibration, and pin profile content
9c5b6b00 test: cross-check the Go percentile against PostgreSQL on discriminating windows
b9ed13e4 perf: replace the singleton runtime assumption with a governed registry
24aa6ea5 perf: harden the plan_actuals truth boundary and add governed fallback
b0004f00 perf: measure compute-plan error before calibrating it
4a8f8a74 release: add Level-B operator handoff
9fe7e55b add confirmed data-preserving release teardown
f8c2d0bd add loopback release status ui
aa79a2aa prove release evidence from exact staging receipts
843cff11 bind release plans to remote staging identity
2b0caa82 harden guarded release adapter execution
b72ee82b validate level-b launch configuration authority
3581f490 return missing input queue on launch refusal
d8202e79 fail closed on incomplete release launches
cb295a7c bind release plans to secret continuity state
2e4fa73c add guarded level-b release planning cli
0a538942 harden task depth calibration authority
9d37ee75 harden frozen ETA-band authority
8098a271 harden atomic prepaid batch funding
9057419c fix settlement authority and disable unsafe batch reuse
8c4442a8 freeze input depth authority for eta calibration
9c30388d recheck task obligations before finalizing jobs
f30730b8 keep verification-pending failures retryable
74061cde fence dynamic obligations against terminal jobs
292bde84 detach unfinished tasks from terminal jobs
7ce470f1 release terminal task leases and fence stale requeue
f7214409 prove dynamic tiebreak start identity
036d52ab release ownership after commit acknowledgement exhaustion
0a3de6e2 isolate historical ETA duration by model
5285b6fe release task ownership after start exhaustion
2accbc93 retry ambiguous task start acknowledgements
1c06deb3 scope ETA calibration by model
51d9f691 make ETA calibration converge on raw estimates
1cb43e7e prove observed-output settlement recovery
af96e93d bill batch generative work for tokens the worker actually produced
47c369a3 read the eta calibration table back into the next quote
fc9f9d7f refuse to seed a honeypot no honest worker can pass
edb3b7bb correct the pre-push overclaims and close a secret-ignore hole
8a9a6a92 rename every process, binary and service off cx
a14c43c3 merge the canary scenario driver
9445f801 treat the min-billable floor as settlement authority, not estimation
dfcf1f3c refund the buyer when a dispute is upheld
adb1a334 let the control plane start after model price state is reset
11712474 stop the canary driver certifying work it never observed
aed9b393 bound billed completion tokens by output the control plane observed
0825dd7a refuse to price the catalogue from stale market evidence
822025ec pin the agent publisher identity and finish the GitHub rename
8488f3ef bind pricing authority end to end
0a1cb888 harden verification recovery concurrency
bd7a44f6 bind catalogue pricing to atomic FX authority
38197361 bind planning capacity to claim eligibility
0c6db9c1 bind accepted jobs to settlement currency
ab026140 make processor fee allocation deterministic and durable
2ec16884 bind release authority to exact evidence chain
983b65b2 derive agent restart proof from process sessions
677166d9 prove Stripe webhook application outcomes
789db19f bind Stripe sandbox to staging authority
3e41cf7c freeze Stripe API and webhook contracts
deee9c1d bind realtime placement authority
600d94e5 freeze quote-bound compute authority
ec736e0e seal live payment activation authority
20b9a0b5 harden canonical workload authority
44588086 measure merc's gateway overhead: +3.5ms TTFT p50, throughput within 2%
b0d85ea7 clear dead code and a receipt that documents a fixed bug as current
dbf19f19 schema: two upgrade-only migration bugs, found by deploying
41db85b5 settlement currency is configuration, not a hardcoded literal
68d236ba shape-aware routing, gated on evidence it does not yet have
cee8ec79 warm-prefix routing on the live claim path
7f761c79 reuse: floor the hit charge, or the whole feature silently never fires
34822d28 reuse economy on the hot path, supplier floor, and CUDA-aware cost ranking
ec5a3782 money defect: supplier paid zero while the buyer is charged
37ea6c22 multi-GPU: workers declare topology, merc does not trust it
039c54cb multi-GPU: tensor-parallel admission that cannot admit an OOM
6150b4ae LoRA: outcome-aware settlement with independent evaluation
c59de433 image generation: governance, surface, and an honest 503
640ba9a8 price board: gate the page against the server's own pricing
ded18b54 rename: zero-residue audit, RESIDUE 0, gated in CI
048d8c8d supplier earnings: carry reads the authoritative accrual
cea3a764 realtime: official OpenAI SDK conformance restored and passing
c50ca1e6 onboarding policy: refuse models merc cannot sell or safely run
f0daf327 tenant isolation: gate the object boundary instead of assuming it
2de6b40e object retention: job payload objects expire 30 days after terminal
8156ce1a supplier console: earnings, payout rail and verification standing
fbe02ce7 release: harden soak observability and continuity
87a81857 release: complete autonomous nonexternal closure
```
