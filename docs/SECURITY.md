# Security posture

This is the current boundary and limitation register. It is not a certification.

## Trust boundaries

- Buyer endpoints require a buyer API key or revocable session. Object access is
  buyer-bound and unsupported job/model pairs fail before dispatch.
- Worker endpoints require a hashed-at-rest worker credential. Enrollment codes
  are single-use, expire, and can be revoked.
- Operator endpoints under `/admin/*` require a revocable admin API key. There is
  no browser-only or alternate operator authority.
- Stripe endpoints are unauthenticated transport endpoints whose payloads must
  pass timestamped HMAC verification.
- PostgreSQL is the queue, lifecycle, and money authority. S3-compatible storage
  is an artifact store, not a source of lifecycle truth.

## Authorization matrix

The normative route-by-route, eight-role decision table is
`ops/authorization-matrix.json`. CI parses `Server.Routes`, requires exact
coverage of all 110 method/path registrations, and verifies each protected route
uses its reviewed authentication wrapper. The summary below is explanatory.

| Surface | Credential / proof | Object scope | Mutation authority |
|---|---|---|---|
| health, readiness, version, static site, metrics | public behind the production proxy | no buyer objects | none |
| signup, login, alpha request | IP-rate-limited public request | newly created identity/lead only | identity bootstrap |
| Stripe billing and Connect webhooks | endpoint-specific timestamped HMAC | provider event id and bound account/job references | append-only provider facts; idempotent effects |
| buyer jobs, results, invoices, receipts, disputes, keys, webhooks | revocable buyer key or session | every lookup includes authenticated `buyer_id` | own jobs, credentials, callbacks, disputes |
| supplier enrollment management | buyer key/session plus supplier ownership | the buyer's single owned supplier | mint/revoke own device credentials |
| worker register, heartbeat, poll, start, fail, commit | hashed-at-rest worker token | authenticated worker plus exact task claim and attempt epoch | current execution only; no buyer or admin access |
| `/admin/*` | revocable admin API key with actor attribution | explicitly requested global object | audited suspend/requeue/reputation/payout/subsidy actions |

Buyer and worker credentials are different namespaces. A valid object UUID never
grants authority by itself. Cancellation performs an owner-scoped lookup before
locking task rows; task start, failure, commit, and heartbeat lease renewal are
fenced by the retry-attempt epoch.

## Enforced controls

- Buyer API keys and worker tokens are stored as hashes. Token-bearing responses
  use `Cache-Control: no-store`.
- Request and artifact reads are bounded. Job inputs are normalized before task
  creation; task output cardinality and workload-specific shape are checked before
  acceptance.
- Presigned URLs scope an agent to the current task input and result object.
  Storage credentials do not enter the agent.
- Completion callbacks are buyer/job-bound, signed with an independent secret,
  reject redirects, validate public destinations, and retry from a transactional
  outbox.
- Production startup requires strong token-encryption and verification-sampling
  secrets. Stripe signatures have a five-minute replay window.
- Redundancy and honeypot selection use keyed sampling. Their task addressing is
  indistinguishable from ordinary work.
- Settlement uses append-only ledger effects, stable idempotency keys, payout
  holds, disputes, refunds, and auditable operator actions.
- Every Stripe request pins the compiled API version immediately before network
  I/O; account-default version changes cannot alter charge, payout, refund,
  reversal, Connect, or reconciliation semantics outside a Merc release.
- Billing and Connect webhook endpoints must also pin that same API version for
  payload rendering. Endpoint setup refuses null/mismatched versions because
  request headers alone do not freeze the schema of later signed events.
- Valid cash-event webhooks return only non-secret outcome headers
  (`applied`, `stale_ignored`, `duplicate`, and the current effect rank). The
  supervised Sandbox matrix uses them to prove first-application ordering and
  replay against staging state without exposing ledger values or provider
  secrets.
- Connect `account.updated` facts are bound to the connected account and applied
  monotonically; `capability.updated` and Connect `payout.*` deliveries are
  append-only provider observations and cannot settle or reverse a Merc
  supplier-credit ledger row. A durable non-active `transfers` capability
  observation refuses new Connect transfer creation, while missing historical
  capability observations remain explicitly `unknown`.
- `account.external_account.created`, `.updated`, and `.deleted` deliveries
  are also retained as bound provider observations; they cannot alter supplier
  readiness or settle/reverse a Merc ledger row.
- `payment_intent.payment_failed` deliveries are retained as explicit provider
  failures but never mutate cash or retry state: a late failure can belong to an
  older PaymentIntent while a later attempt succeeds.
- Job cancellation, task lease recovery, retry exhaustion, and result commit are
  transactional. Duplicate commits and duplicate money effects are rejected.
- On macOS the shipped supplier profile denies inbound networking, listening
  sockets, arbitrary outbound ports, writes outside src/agent/cache/temp paths, and
  reads of common credential and personal-data locations. The agent can be
  configured to fail closed if sandbox re-exec is unavailable.

## Public hostname rehearsal

`ops/scripts/alpha-security-suite.py --surface external` drives the authorization
matrix as an internet client against `https://mercmerc.net`. It is a different
surface from the local `Server.Routes()` rehearsal (`--surface local`, the
`make alpha-security` default). The receipt at
`evidence/external/staging-attack-rehearsal.json` records the public-hostname
run.

The 2026-08-17 external run (`kind=external_staging_attack_rehearsal`,
`surface=persistent_staging_tls`, `qualification=EXTERNAL`) executed 265
attacks over 255 HTTPS requests on 112 distinct routes. No finding. Per class:

| Class | Attempted | Blocked | Finding | Notes |
|---|---|---|---|---|
| identity | 167 | 167 | 0 | anonymous + cross-role on every authenticated matrix route |
| identity_webhook | 8 | 8 | 0 | stripped, forged, and correct signature from the wrong authority in both directions |
| money | 15 | 15 | 0 | unauthenticated / wrong-role money routes; no matching-authority cash event sent |
| authority | 62 | 62 | 0 | operator surface plus `/readyz` payment_mode=test before and after |
| tls | 12 | 12 | 0 | TLS 1.3, SAN `mercmerc.net`, `ops/deploy/Caddyfile` headers, `:80` 308 to https |
| containment | 1 | 1 | 0 | public `GET /metrics` returned 404 as `ops/deploy/Caddyfile` claims |
| concurrency | 0 | 0 | 0 | not driven — would race live rows |
| resource | 0 | 0 | 0 | not driven — a flood would take the endpoint down |
| supply_chain | 0 | 0 | 0 | not driven — those probes read the checkout |

TLS: Let's Encrypt, protocol TLSv1.3, SAN `mercmerc.net`. Observed headers
matched `ops/deploy/Caddyfile` (`Strict-Transport-Security`, CSP, `Permissions-Policy`,
`Cross-Origin-Opener-Policy`, `X-Content-Type-Options`, `X-Frame-Options`,
`Referrer-Policy`; `Server` stripped). No error body leaked internals.
`payment_mode` was `test` and `/readyz` was 200 before and after.

Cross-role tokens were namespace-shaped stand-ins. Public signup is
canary-gated, so this process did not mint a live buyer or worker credential.
Wrong-authority webhooks used the live distinct endpoint secrets and were
rejected as invalid signatures. The matching-authority cash path was not sent.

`ops/scripts/validate-readiness.py:external_staging_attack_proven` accepts this
receipt on the executed public-surface evidence: 265 attacks, per-class
results, `qualification=EXTERNAL`, hostname `mercmerc.net`. It does not
require a named human reviewer. `reviewer.name` and `reviewer.organization`
stay empty because no named human reviewed this run; inventing one would
be a lie. Named review of the public-surface rehearsal is classified
`PUBLIC_LAUNCH` (`named_reviewer:staging-attack-rehearsal`). At backend
alpha the executed count is independently re-runnable and participants
are operator-controlled. At public launch strangers arrive, and "a script
said it was fine" is not an answer anyone can be held to.

## Residual limitations

- The macOS sandbox restricts outbound direction and ports, not destination host.
  HTTPS egress can reach an attacker-controlled host. A forced egress proxy or
  equivalent network policy is required for destination pinning.
- A checkout under `Downloads`, `Documents`, or `Desktop` conflicts with the
  shipped read denies. Install the agent outside those directories for sandboxed
  runs.
- Worker hardware and engine identity are self-declared. Scheduling checks the
  exact advertised tuple but does not remotely attest the physical machine.
- Bearer requests do not have per-request nonces; possession of a live bearer
  token authorizes the request until revocation.
- Application rate limits assume the reverse proxy appends the final forwarding
  hop correctly. Production must keep the control listener private behind that
  proxy.
- Local proofs establish code-path behavior, not external fleet scale, market
  liquidity, or production payment processing.

## Release checks

Before release, run `make ci`, `make prove-local`, and the macOS sandbox profile
test. Review dependency updates, census output, schema apply-twice evidence, live
two-agent receipts, money invariants, and the exact source fingerprint. Treat
skipped physical execution as a skip, never as a pass. Drive the public
hostname with `python3 ops/scripts/alpha-security-suite.py --surface external`
rather than relabeling a local `Server.Routes()` receipt.
