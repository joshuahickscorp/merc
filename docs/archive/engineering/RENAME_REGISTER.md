# merc rename register

The rebrand from ComputeXchange to **merc** is not one operation. This file records
what has landed, what is blocked on a change outside this repository, and what must
never be renamed at all.

Classification came from a 14-agent pass (7 identifier families, each paired with an
adversarial verifier whose brief was to find a RENAME that is really a receipt or a
hash input). Every FREEZE below was confirmed against the file it names.

---

## 1. Landed — repo-internal, gates green

| Change | Where |
|---|---|
| Brand prose | `README.md`, `NOTICE` title, `cli/README.md`, `sdk/python/README.md`, `Dockerfile.control` label, `agent/Cargo.toml` description, `agent/src/main.rs` `about`, `control/buyer.go` CLI header, 6 `docs/*.md`, 3 `ops/*.json` titles, `proto/manifest.schema.json` title, 4 `scripts/*.sh` |
| Python SDK | `sdk/python/computeexchange/` → `sdk/python/merc/`, distribution name, `packages`, version attr, tests, `scripts/verify-python-sdk-package.sh`. Clean-room install reports `installed merc 0.1.0 from merc` |
| Go module | `computeexchange/control` → `merc/control` |
| Website copy | titles, og:title, wordmark, footer, claims-ledger label, `site.webmanifest`, and the `site-build.mjs` brand assertion |
| Model owner | `control/api.go` `OwnedBy` |
| GitHub repository | Renamed to `joshuahickscorp/merc`. Old URLs redirect. Local `origin` re-pointed, and the repo URL updated in `.github/ISSUE_TEMPLATE/config.yml`, `Dockerfile.control` `image.source`, `scripts/install.sh` `MERC_AGENT_REPO` default, `scripts/release-doctor.sh`, `sdk/typescript/package.json`, `web/.well-known/security.txt`, `web/buyer.html`, `web/index.html`. `publish-candidate.yml` derives the cosign `--certificate-identity` from `$GITHUB_REPOSITORY`, so signing follows the rename with no edit; the recorded `certificate_identity` values under `evidence/` keep the old URL because they record what was actually verified. |
| Supplier agent binary | `cx-agent` → `merc-agent` (Cargo package/bin, `scripts/install.sh` / `package-agent-binary.sh`, agent-release and CI evidence artifacts, canary/rehearsal drivers, sandbox profile `merc-agent.sb`) |
| Control container binary | `/cx` → `/merc`, `cx-healthcheck` → `merc-healthcheck` in `Dockerfile.control`, compose healthchecks, and CI control evidence packaging |
| launchd label | `dev.computeexchange.agent` → `dev.merc.agent` |
| systemd backup units | `cx-backup.service` / `cx-backup.timer` → `merc-backup.*` under `ops/systemd/` |
| Agent state directory | `~/.compute-exchange` → `~/.merc`, with migrate-on-read in the agent and migrate-on-install in `scripts/install.sh` |

`make ci` exits 0 after each step.

### Compatibility retained after the process rename

- **`scripts/uninstall.sh`** still removes the pre-rebrand binary (`$PREFIX/cx-agent`),
  launchd label (`dev.computeexchange.agent`), and state dir (`~/.compute-exchange`),
  in addition to the modern names. `scripts/test-agent-uninstall-legacy.sh` fails if
  that dual handling disappears.
- **Retained rollback image entrypoint `/cx`.** Local rehearsal and go-closure
  rollback paths select the healthcheck/entrypoint per image via
  `MERC_LOCAL_CONTROL_HEALTHCHECK` and `MERC_LOCAL_CONTROL_ENTRYPOINT` (legacy
  registry digests keep `/cx`; newly built images use `/merc` and
  `/merc-healthcheck`). Do not hard-code the new entrypoint for prior images.
- **Hosts with `cx-backup.*` installed** must
  `systemctl disable --now cx-backup.timer` before enabling `merc-backup.timer`,
  or the old timer keeps running the old script path. Documented in
  `docs/RUNBOOKS.md` and the unit file comments.

## 2. Blocked — needs a change outside this repository first

Renaming these in-repo before the external change produces a dangling reference that
still looks plausible.

| Item | External prerequisite |
|---|---|
| `ghcr.io/…/computexchange-control`, `computexchange/control` image tags | Rename/republish the registry package. **Recorded digests in `evidence/` must not follow** — see §3. |
| `CX_*` environment variables (~180 names) | The droplet `.env`, GitHub Actions secrets, and systemd units supply the values. Rename code and environment in the same cutover. **`CX_TOKEN_KEY` must be copied byte-identically** — `control/crypto.go` derives the AES key as `sha256(value)`, so regenerating it makes every sealed OAuth token and webhook secret in Postgres permanently undecryptable. |
| `CX_ENV` specifically | `control/main.go` gates the production hardening refusal on `EqualFold(cxEnv, "production")`. If the binary reads `MERC_ENV` while the droplet still sets `CX_ENV`, the value resolves empty, the refusal is skipped, and control boots with a warning while writing `plain:`-prefixed secrets. |
| Prometheus `job="computexchange-control"` and `external_labels.service` | These are part of the alert label set Alertmanager fingerprints and ships to the operator receiver. Update the receiver's filters first or pages vanish silently while both services look healthy. |
| `ComputeExchange*` alert names (24) + the 72 `cx_*` metric names | Must land in one commit with the receiver update. `validate-observability.mjs` only checks each name exists in its own file — it never checks an alert's expr references a metric that is actually emitted. Verify with `promtool check rules` and a live scrape. |
| `macapp/ComputeExchangeAgent/` | Real directory with 8 consumers including a live claim gate (`proof/claims/claim-policy.json`). Needs `git mv` plus all consumers in one commit. |
| `/opt/computexchange`, `/etc/computexchange`, `/var/lib/computexchange` | Real directories on the droplet. |
| Postgres role/database `cx` | `ALTER DATABASE` needs no active connections, or keep the name and treat it as configuration. |
| `scripts/cx` operator CLI wrapper | Pairs with host install path conventions; not part of the process/binary cutover above. |

## 3. Frozen — never rename

**Receipts.** Everything under `evidence/`, plus `ops/asset-provenance.json`,
`ops/readiness.json`, `ops/legal-review.json`, `census/`. These record which image
digest was actually pulled, which cosign certificate actually verified, and which
file had which sha256. Rewriting a name inside one makes the repo claim it verified
an image that was never built.

**Hash domain separators.** Renaming any of these silently changes every value
derived from it, with nothing failing:

- `control/evidence.go` — `computexchange-source-fingerprint-v2` (v1 retained as historical domain; see evidence/state/fingerprint-supersession.json)
- `control/store.go` — `computeexchange-control-schema-v1` (schema migration advisory lock)
- `control/worker_leader.go` — `computeexchange-background-workers-v1` (leader election)
- `control/schema.sql` — the matching `hashtextextended(...)` call

Note the two spellings differ: the module path was `computeexchange` (two e), the
fingerprint domain is `computexchange` (one e). A pattern written for one misses the other.

**Fixed-width binary magic.** `CXEM` is a 4-byte header shared by `control/api.go`,
`agent/src/executor.rs` and `sdk/python/merc/__init__.py`, and it prefixes embedding
blobs already written to object storage. `CXIN`/`CXPL` in `control/store.go` are
4-byte ASCII encoded as hex advisory-lock namespaces (`0x4358494e`).

**Live credential prefixes.** `cx_sess_` routes credential class in `control/api.go`;
`cx_live_`/`cx_test_` prefix keys already in the `api_keys` table; `cx_whsec_`,
`cx_admin_`. `maskKey` locates the displayed prefix by scanning to the *second*
underscore — a replacement with fewer underscores writes part of the raw secret into
the `masked` column.

**Stripe metadata keys.** `cx_operation_key`, `cx_payout_*` already exist on live
PaymentIntents and Transfers; renaming breaks idempotency matching against them.

**Third-party attribution.** `NOTICE` beyond line 1 — Llama 3.2 / Meta Platforms,
sentence-transformers, Geist / Vercel / basement.studio. Legally required verbatim.

**Dated findings.** `docs/WEBSITE_3D_BLENDER_STATUS_2026-07-19.md` observes that
`computexchange.net` was serving on that date. That was true. Append a note rather
than editing it.

**Signed workflow path.** The sigstore certificate identity recorded in evidence
contains `.github/workflows/publish-candidate.yml`. Moving or renaming that file
breaks `cosign verify --certificate-identity` for every image already signed.

## 4. Method

Any mechanical pass must be scoped to `git ls-files`. A `find`/`sed` sweep also hits
`.artifacts/` (gitignored, holds signed CI evidence whose own `SHA256SUMS` then stop
matching), `agent/target/`, `review/pass6/` (539 frozen occurrences, plus a 29 MB
binary archive and a base85 patch blob containing the literal `qCX_HL7Jp5i`), and the
`*.orig` patch backups.

Anchor `CX_` substitutions as `\bCX_[A-Z0-9_]+`. A bare `s/CX_/MERC_/g` corrupts that
patch blob — it is the only place in the repo where `CX_` is preceded by a word
character.

One expected consequence: `control/evidence.go` hashes every tracked path *and* its
content, so any rebrand commit changes the repo source fingerprint. The recorded
values in `ops/readiness.json` will not reproduce afterwards. That is honest only if
the domain constant stays at `v1` — changing both at once makes the divergence
unattributable.

## 5. Zero-residue audit — RESIDUE 0 as of 2026-07-27

`scripts/rename-residue-audit.py` classifies every remaining occurrence of the
old name against this register and fails on any that is neither frozen nor
externally blocked. It runs in `make ci`.

| category | count | meaning |
|---|---|---|
| FROZEN | 158 | must never be renamed |
| BLOCKED | 346 | needs an external change first |
| RESIDUE | 0 | nothing is stopping it — a defect |

Scoped to `git ls-files` per §4. Counts above are the 2026-07-27 baseline; the
process-rename cutover moves several former BLOCKED families into landed +
compatibility-frozen surfaces, so live counts from the script supersede the table.

### Frozen identifiers this register did not previously list

The audit found three cryptographic domain separators and several receipt-pinned
paths that §3 missed. Each was confirmed against the file that uses it:

- `cx-enrollment-request-v1` — `control/enrollment.go` computes
  `sha256("cx-enrollment-request-v1\x00" + publicKey)` as the enrollment request
  id. Renaming changes every id with nothing failing.
- `cx-worker-enrollment-exchange-v2` — first line of the enrollment signing
  transcript. Renaming invalidates the proof for every already-enrolled agent.
- `cx-macos-agent-v2` — the enrollment audience, which also appears inside that
  transcript.
- `logo/cx-capsule-target.svg` — pinned by **path and sha256** in
  `ops/asset-provenance.json`. Moving it orphans the receipt. This was found by
  moving it, noticing the receipt no longer matched, and moving it back.

### Newly recorded as blocked

- Everything inside a vLLM runtime profile (`cx-chat-1b`, `cx-llama32-instruct-v1`).
  `control/realtime_profiles.go` hashes the **whole raw file**, and `realtime.go`
  compares that digest against what a worker registered — so any string in a
  profile is a hash input. Renameable, but only in a cutover where workers
  re-attest, exactly like the runtime-authority matrix hash.
- `service: computexchange` in `monitoring/prometheus.yml` — `external_labels.service`
  is in the label set Alertmanager fingerprints.
- `s3://private-staging-backups/computexchange` — real prefix holding staging backups.
- `/srv/computexchange-go-closure` — real directory on the staging server.
- `description=computexchange` on Stripe webhook endpoints that already exist.

### One real bug the sweep found

`scripts/local-production-rehearsal.sh` asserted the served site body contains
`computexchange`. After the website rebrand that assertion was checking for the
old brand — it would pass only if the rename had failed. Now asserts `merc`.
