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
- Job cancellation, task lease recovery, retry exhaustion, and result commit are
  transactional. Duplicate commits and duplicate money effects are rejected.
- On macOS the shipped supplier profile denies inbound networking, listening
  sockets, arbitrary outbound ports, writes outside agent/cache/temp paths, and
  reads of common credential and personal-data locations. The agent can be
  configured to fail closed if sandbox re-exec is unavailable.

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
skipped physical execution as a skip, never as a pass.
