# Release readiness: NO-GO

As of 2026-07-19, this working tree is not approved for a private pilot that
moves real buyer or supplier money. There are no known open P0 code defects, and
the complete local customer path and exact-head remote CI pass, but five external
operational/payment proofs cannot be manufactured from this workstation. The exact machine decision is in
`ops/go-no-go.json`; `ops/readiness.json` is the evidence ledger.

## What was fixed

- Job submission now requires a durable buyer-scoped idempotency key. Identical
  replay returns the original job and secret response; conflicting reuse is a
  `409`, and concurrent submission cannot create duplicate economic authority.
- Task start, failure, commit, recovery, and heartbeat lease renewal use an
  explicit retry-attempt epoch. A delayed process from a prior assignment cannot
  renew or settle a requeued task. Recovery increments the epoch.
- Active agents now renew every running task they report, not a permanently empty
  `current_task`; worker id, lease count, duplicate ids, and attempt values are
  fail-closed and strictly decoded.
- Cancellation is buyer-scoped before task locking and naturally idempotent for
  the owning buyer. Webhook registration was confirmed owner-scoped and
  idempotent for the same job/URL.
- The local proof is hermetic against inherited production/Stripe environment
  variables. It proves idempotency and still refuses to contact Stripe.
- Container bases and production services are digest-pinned. GitHub Actions are
  commit-pinned. The control container is read-only, capability-dropped,
  no-new-privileges, PID/CPU/memory constrained; Caddy admin is loopback-only.
- A real database plus object-store restore drill, fail-closed rollback script,
  backup configuration contract, alert rules, auth matrix, frontend contract,
  and incident runbooks were added. The CLI archive's missing README was found
  and fixed by a clean-install release test.

## Proven locally

The final native proof launched isolated PostgreSQL and MinIO plus two distinct
optimized `cx-agent` processes on this Apple host. Both `embed` and
`batch_infer` completed through Candle. Its receipt records a zero-sum ledger, no
duplicate task effects, idempotent submission, schema apply-twice, lifecycle
regression rejection, and an unchanged source fingerprint. Measured results:

| Measure | Result |
|---|---:|
| Control RSS | 567,968 KB |
| Two-agent RSS | 2,434,416 KB |
| `embed` | 3,200 ms final warm run (34,108 ms cold outlier observed) |
| `batch_infer` | 3,196 ms |
| stripped `cx` | 13,631,954 bytes |
| optimized `cx-agent` | 10,538,864 bytes |

`make ci`, `go test -race -count=10 ./...`, the clean CLI archive install,
YAML/shell/JSON validation, the clean CLI release archive, `govulncheck`, and `cargo audit` pass. RustSec reports
one non-vulnerability warning: Candle's graph still uses the unmaintained `paste`
macro. Gitleaks scanned 870 commits (40.37 MB); its 24 detections were triaged as
shell-variable auth headers, explicit development fixtures, and content hashes.
The real ignored `.env` and `.secrets` files have no repository history. A
candidate-only Gitleaks scan has zero findings. The pre-commit full-proof ledger
has all 14 required gates PASS, no skips, source fingerprint
`4c8c5bd4228bbde5155a4e850549dd80241f8f4109f9f1abe778ee7ebe4210db`, and
SHA-256 `da0c9da90c3a421d2840a6042fef33a424b08c605ecabba92db27e073543ac8d`.

The authoritative census includes cached and non-ignored untracked candidate
files, so new hardening files are not omitted merely because they are unstaged.
It reports approximately 38.7k LOC of maintained non-design production core; the
exact totals are in `census/CODEBASE_CENSUS.json`.

## Proven remotely

PR #5 exact-head run `29707723987` passed all five jobs at
`1d026d5a0750825d62561aa973e0948784df5f2d`. Every job asserted the checked-out
candidate SHA. The four `candidate-*` artifacts were downloaded; all embedded
checksums passed, including the previously omitted hidden configuration template.
The receipt includes multi-platform CLI archives, Linux `cx`, optimized Metal
`cx-agent`, website/contracts, both candidate and `0387766` control image
archives, and two SPDX SBOMs. The candidate/prior image IDs were
`sha256:05e23f42a6e52219e4057dbb134fa716b5e1e68ca3508bced3c604e7c229dfe1`
and `sha256:d74ff5dd81a9fb5194d48a2fbc3c51fca11b01e67c54066ae6a2fb672f996023`.
Because this document changes the commit, PR #5's current-head check rollup and
the four `candidate-*-${HEAD}` artifacts are the normative final receipt.

## Blocking P1s

1. No independent offsite backup was uploaded and restored. The local
   transactional database/object mirror drill passed, but the AWS-compatible
   destination and credentials are absent.
2. No persistent TLS staging environment is available. This host lacks Docker
   Compose v2, a Docker daemon, representative DNS/TLS, and a staging endpoint.
   The isolated native proof is strong integration evidence, not a deployment.
3. Rollback could not be rehearsed because no prior content-addressed image and
   staging host exist. The script is present and syntax-checked only.
4. The full Stripe test-mode cash/payout matrix was not run. Local processor
   boundary tests do not substitute for authorize/capture/refund/dispute,
   payout failure/outcome-unknown, duplicate/out-of-order webhooks, and provider
   reconciliation using real test-mode ids.
5. Alert rules parse and map to runbooks, but no real receiver delivered,
   acknowledged, and resolved a synthetic page.
## GO procedure

Configure independent offsite backup storage and a representative staging host.
Deploy, run the complete buyer/supplier/admin and both-workload matrix, execute
the Stripe test-mode cash matrix and scoped reconciliation, restore the uploaded
backup, rehearse rollback to the prior image and forward again, and prove a real
synthetic page. Re-run `make prove-local` on the unchanged source and attach all
receipts. Only when `ops/go-no-go.json` has zero P0 and zero P1 may this change to
GO.
