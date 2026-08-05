# Architecture

Merged architecture/contracts surface (PLAN_300K L6).


<!-- source: EXECUTION_NETWORK_BIBLE.md -->

# ComputExchange execution network bible

This file is the repository control record for the authoritative expansion
specification supplied on 2026-07-21. The received 3,416-line source has SHA-256
`60962a66291553cdc6a88d4e2cbf0979e80f4fd731ff8b42c48740e11d0368e1`.

The governing milestone is a production-grade OpenAI-compatible streaming
endpoint backed by pinned upstream vLLM at near-native performance, with
ComputExchange admission, routing, verification, transparent pricing,
settlement, automatic failure remedies, supplier payouts, and inspectable
receipts. Commodity runtime work belongs upstream; ComputExchange owns the
execution contract and execution-to-money authority.

Every cycle must distinguish implementation, automated tests, real-runtime
proof, private-canary proof, production proof, and external blockers. A route,
mock, unit test, dashboard, or unmeasured claim is never promoted to a higher
proof level. The current detailed state is maintained in
`docs/PROGRAMME.md § "Merc shippability status"` (per-lane rung) and `RELEASE_READINESS.md` with
`ops/go-no-go.json` (release decision). `REQUIREMENT_PROOF_MATRIX.md` is a
superseded 2026-07-21 snapshot kept only as an audit trail. `VLLM_LANE_STATUS.md`
was named here but has never existed in this repository; the vLLM lane's current
rung lives in `docs/PROGRAMME.md § "Merc shippability status"` alongside every other lane.

Until the complete source text is imported through the normal documentation
review path, the source with the digest above remains authoritative if this
control record is incomplete. This record may not be used to weaken or replace
any requirement in that source.


<!-- source: docs/WORKLOAD_IR_V1.md -->

# Merc Workload IR v1

Workload IR is the local, pre-upload description of a project. It is a proposal,
not a quote, order, job, or permission to execute. Version 1 deliberately refuses
to manufacture runtime, resource, economics, privacy, or quality authority when
the project does not contain it.

The authenticated production route `POST /v1/projects/compile` accepts the same
proposal as a bounded tar (or gzip-tar) upload. It returns only the digest-bound
IR. `X-Merc-Bounded-Probe: true` requests the non-executing bounded probe and
must carry the exact prior `X-Merc-Approved-IR-SHA256`; changed archives,
traversal, links, sensitive paths, and oversized entries fail closed. The route
creates no quote, capacity reservation, pricing decision, or execution authority.

## Safe workflow

1. Run `merc project compile --root PROJECT`.
2. Review the complete JSON proposal and its `ir_sha256`.
3. Approve that exact digest.
4. Run `merc project compile --root PROJECT --probe
   --buyer-approved-ir-sha256 SHA256`.

The second command rescans the project and independently reconstructs the
unprobed IR. It refuses before probing unless that digest equals the buyer's
approved digest. Changing a file, detector result, graph edge, unknown, or
refusal invalidates the approval.

The v1 probe is `NON_EXECUTING_FILE_SHAPE_V1`. It executes no project code,
starts no container, opens no network connection, and reads at most 1 MiB from
the already bounded static-inspection sample. JSONL/NDJSON observation is capped
at 256 records per artifact.

After approval, each step receives a structured resource observation scoped to
its declared `project://PATH` inputs (`project://input` deliberately selects all
non-declaration artifacts). The observation records exact artifact bytes,
sampled bytes, bounded sample records, a text-token estimate from sampled bytes,
and the probe kind. Complete samples become
`SHAPE_MEASURED_CALIBRATION_REQUIRED`; an absent, unreferenced, or truncated
sample becomes `PROBE_INCOMPLETE_REFUSE`. These are workload-shape facts, not a
GPU-memory, duration, or cost claim.

## Canonical identity

`project_sha256` commits to every included relative path, byte length, and full
artifact SHA-256. `ir_sha256` commits to the canonical JSON IR with only its own
`ir_sha256` field cleared. The compiler sorts file paths, detector kinds,
evidence, observations, and refusals before hashing.

Ignored build/cache directories are `.git`, `.hg`, `.svn`, `node_modules`,
`vendor`, `target`, `dist`, and `build`. Their omission is deterministic and is
part of the v1 boundary.

## Resource bounds and refusals

Static inspection admits at most 10,000 regular files, 4 GiB per artifact, and
64 GiB total. Detector content inspection is capped at 256 KiB per file and
16 MiB total. Full bytes are streamed only to bind artifact hashes.

The compiler refuses symlinks and non-regular files because their target may
escape or change the reviewed project boundary. It refuses common credential
files (`.env*`, private-key files, package registry credentials, SSH keys) and
asks for a redacted inventory. Privileged or host-network container authority is
recorded as an unsafe refusal.

An unknown detector or a detector below the 0.70 confidence floor never becomes
execution authority. Detected steps remain `UNRESOLVED_REFUSE` or
`UNCALIBRATED_REFUSE` until a later compiler revision can bind them to governed
runtime/model contracts and measured outcome cohorts.

## Graph fields

Each IR contains:

- steps and dependencies;
- input and output artifact references;
- runtime and model contracts;
- resource and parallelism estimates;
- checkpoint and verification policy;
- privacy, egress, data location, quality, and deadline;
- probe authorization and observations;
- cost/duration estimate state and calibration basis;
- unknowns and refusal reasons.

Each step also carries compiler-derived topology shape evidence. Independent
steps are labelled `INDEPENDENT_CHUNK_SPLIT`, service/realtime steps
`REPLICA_SERVICE`, rendering/media steps `FRAME_TILE_SAMPLE`, and explicit
single-device steps `SINGLE_DEVICE`. A `TIGHT` declaration is retained as
`GANG_UNMEASURED_REFUSE` until a later authority supplies measured collective
fabric, capacity, and topology-specific economics. These fields describe the
graph; they never select a worker, quote a price, or authorize execution.

Version 1 produces a stable graph for supported detector signals but keeps the
economics state at `UNCALIBRATED_REFUSE`. Detector confidence is not cost or
duration confidence, and neither can affect pricing, reserve, settlement, or
admission.

## Calibration gate

`merc project calibration-check --cohort COHORT.json` evaluates outcome-linked
evidence for one exact `ir_sha256`. It exits nonzero and emits every refusal when
the cohort does not clear the gate. A promotable cohort requires:

- at least 100 `PRIMARY_EXECUTION` observations;
- one explicit supported currency and a source-receipt SHA-256;
- `TRUE_NET_COMPLETE` cost observations, not supplier gross, platform ledger
  rows, or known-cost contribution presented as net cost;
- median absolute cost error no greater than 10%;
- p90 absolute cost and duration error no greater than 20%;
- at least 95% of actual complete cost within the frozen buyer ceiling.

The result binds the cohort bytes to `calibration_cohort_sha256` and reports
cost and duration confidence as `1 - p90_error`. Passing this evidence permits a
later compiler revision to use it for estimates only. The gate has no caller
from pricing, reserves, settlement, or admission.

## Production project quote

`merc project quote --root PROJECT --buyer-approved-ir-sha256 SHA256` repeats
the exact approval-bound probe and quotes every resolved finite step through the
authenticated production `/v1/quote` caller. It does not maintain a project
price table or locally derive supplier share, floors, payment allocation, or
Merc contribution.

For every step, the command:

- requires one explicit, bounded, non-symlink `project://PATH` input;
- derives job type, model kind and model ID from resolved server authority;
- validates the returned composite `PricingDecision` by rebuilding it from its
  workload, compute, placement, economic and catalogue authorities;
- requires the project, quote, pricing decision and fixed-point decision to use
  the same currency;
- records the exact pricing-decision SHA-256 and converts the quote boundary to
  nano-major-units once;
- sums expected and maximum costs with overflow checks;
- refuses the fixed-point project buyer ceiling;
- computes p50 and p90 critical paths from declared DAG dependencies rather
  than summing parallel branches.

The aggregate remains `STEP_QUOTES_NOT_PROJECT_OUTCOME_CALIBRATED`. Step quotes
are real pricing and time authorities, but their sum has not demonstrated the
project-level median/p90 targets until an outcome cohort passes the calibration
gate.

The quote artifact is version 2 and embeds each complete server quote plus its
canonical SHA-256. This is intentional: a later submit must be able to rebuild
the composite pricing authority, not trust copied display totals or a bare quote
ID.

## Reviewed project submit

`merc project submit --root PROJECT --buyer-approved-ir-sha256 SHA256 --quote
PROJECT_QUOTE.json` accepts a quote only after rescanning and repeating the
bounded probe over the exact approved project. Before the first mutation it revalidates every embedded
workload, compute, placement, economic, catalogue and fixed-point pricing
snapshot; its quote and pricing digests; currency; runtime/model identity;
expiry; exact input bytes; step and aggregate nano-unit costs; buyer ceiling;
and critical-path/confidence fields.

Accepted jobs bind `firm_quote=true`, the reviewed quote ID, and the exact step
maximum converted across the legacy float wire only after an exact nano-unit
round trip check. Each POST carries a deterministic IR-and-quote-bound
idempotency key, and the output records both normal acceptance and replay.

`merc project submit` remains the reviewed all-at-once path for finite
independent steps and labels that mode `INDEPENDENT_FINITE_STEPS`. A network
failure can still occur between independent POSTs; in that case the command
emits the already accepted job IDs with `PARTIAL` (or `INDETERMINATE` when the
first response is unknown), records the attempted idempotency key, exits
nonzero, and a retry of the same reviewed artifact safely replays those
submissions.

Declared dataflow uses a separate, receipt-bound progression rather than
pretending that future output is available at initial quote time:

1. `merc project quote-roots` quotes only dependency-free inputs already in the
   reviewed project directory.
2. `merc project submit-roots` creates one server-side project order and submits
   those roots as `INITIAL_MATERIALIZED_ROOTS`.
3. After a root has a completed, verified job receipt, `merc project
   materialize` writes the exact result as its declared project artifact and
   emits a hash- and receipt-bound materialization record.
4. `merc project quote-step` rechecks both the local artifact bytes and the
   server-side accepted root slot, then quotes one ready downstream step against
   the project order's remaining exact-nano ceiling.
5. `merc project submit-step` repeats those checks immediately before posting
   the firm job, labels the submission `DEPENDENT_MATERIALIZED_STEP`, and uses a
   project-, step-, and quote-bound idempotency key.

The materialization record is insufficient by itself: the next step also
requires the upstream job's completed verified receipt and its frozen
PricingDecision digest to appear in the buyer-scoped server order. Conversely,
the order is insufficient by itself: changing the local materialized bytes
after receipt refuses before the next quote or submit. This supports finite
declared artifact dataflow only; it does not yet make image/video generation,
LoRA, service, or tightly coupled declarations executable when no governed
runtime cell exists. The bounded deterministic `media_rendering` cell is now
resolvable for closed scene documents; that does not make prompt-to-image or
video generation executable. `ACCEPTED` is not completion, outcome verification,
settlement, or project calibration.

## Detectors

The v1 static detector taxonomy covers realtime inference, batch inference and
compute, embeddings, structured extraction, media rendering, bounded
`media_transcode`, image/video, LoRA, model evaluation, bounded containers, and
service deployment. An explicit `ffmpeg` signal proposes the bounded
`media_transcode` contract; it still requires the exact routable runtime/model
digests before it can be quoted. Evidence is
the set of project-relative files carrying each signal. Two independent files
raise a detector from 0.55 to 0.72; this is only a proposal confidence and does
not assert that the inferred graph is runnable.

Detector ordering never creates graph edges. Without an explicit, validated
dataflow declaration, detected steps are independent and the IR records step
dependencies as unresolved. A sorted filename or detector name is not evidence
that one workload consumes another's result.

## Explicit project declaration

A project may include a root `merc.project.json`. This is the buyer's proposed
dataflow and constraints, not server authority. It contains version 1, 1–256
steps, privacy, quality, optional RFC3339 deadline, result policy, and economics.
Steps name explicit inputs, outputs and dependencies; the compiler rejects
missing dependencies, self-dependencies and cycles, then sorts the DAG by step
ID for canonical identity.

Each declared step must use a supported detector kind, bind proposed runtime and
model contracts by SHA-256, require a bounded resource probe, declare
`INDEPENDENT`, `TIGHT`, or `SINGLE_DEVICE` parallelism, and name checkpoint and
verification policy. A declared kind with no independent static detector signal
is retained in the graph but produces a refusal.

`merc project contracts` prints the exact contract pairs the running binary can
resolve from its embedded activation authority. Each row includes workload kind,
runtime/profile/cell identity, model identity, lifecycle, verification contract,
and the two SHA-256 values used in `merc.project.json`. Compilation resolves a
step only when exactly one currently routable cell matches both digests and the
workload kind, and when the declared verification contract equals that cell's
contract. A well-formed but unknown hash remains refused. The current catalog
contains the genuinely advertised batch-inference and embeddings cells plus the
bounded private-canary `media_transcode` and `media_rendering` cells. LoRA and
service declarations cannot resolve until their trainer/evaluator and
lease-backed runtime cells exist.

Economics contains an ISO currency and a positive
`maximum_buyer_price_nanos`. Fixed-point nanos avoid a float or integer-unit
rounding change before a price exists. The buyer must leave `supplier_floor` and
`merc_contribution` as `UNRESOLVED_REFUSE` and may not provide a
`pricing_decision_sha256`; only server pricing authority may resolve those.

Example skeleton:

```json
{
  "version": 1,
  "steps": [{
    "id": "extract",
    "kind": "structured_extraction",
    "depends_on": [],
    "inputs": ["project://input"],
    "outputs": ["project://records"],
    "runtime_contract": "<sha256>",
    "model_contract": "<sha256>",
    "resource_estimate": {"state": "BOUNDED_PROBE_REQUIRED"},
    "parallelism": "SINGLE_DEVICE",
    "checkpoint_policy": "NOT_APPLICABLE",
    "verification": "schema-v1"
  }],
  "privacy": {"egress": "DENY", "data_location": "CA"},
  "quality": {"requirement": "buyer-fixture-v1", "verification": "independent"},
  "result": {"contract": "artifact-set-v1", "retention": "30d", "delivery": "object-store"},
  "economics": {
    "currency": "cad",
    "maximum_buyer_price_nanos": 50000000,
    "supplier_floor": "UNRESOLVED_REFUSE",
    "merc_contribution": "UNRESOLVED_REFUSE"
  }
}
```


<!-- source: docs/CAPACITY_LEASES.md -->

# Capacity leases (design only)

Status: design, not implemented. Implement only after execution envelopes are
complete and green. This document matches the envelope rigour: where authority
lives, what is safe to over-issue, crash behaviour, and interaction with the
cost-rank ordering in `control/scheduler.go` / realtime offer selection.

## Problem

The supply-side mirror of per-request market clearing: every realtime
authorization re-ranks the offer book, decrements `available_sequences`, and
binds worker + rates into an `execution_contract`. At high concurrency that is
correct but expensive, and it couples admission latency to book depth.

A **capacity lease** is a bounded grant a *verified worker* holds so the hot
scheduler can assign work *within* the lease without re-clearing the full market
per request.

## Lease contents (minimum)

| Field | Purpose |
| --- | --- |
| Worker id + supplier id | Who is bound |
| Runtime profile id + sha256 | Model / runtime scope (must match envelope scope when both exist) |
| Sequence slots | Max concurrent sequences covered by the lease |
| Interval / expires_at | Wall-clock bound |
| Supplier floor rates | Input/output nanos per million (frozen at lease issue) |
| Buyer price ceiling class | Optional: max buyer rates this lease may serve |
| Cost class key | The verified-outcome cost class at issue time |
| Monotonic version | Optimistic concurrency on remaining slots |

## Authoritative state

**Postgres is authority**, same argument as envelopes:

- In-memory slot counters without durable issue are a free credit line for
  capacity: restart, second replica, or clock skew over-admits sequences.
- Safe shape: one row per lease; remaining slots updated by a **single**
  `UPDATE ... WHERE remaining_slots >= $n AND expires_at > now() RETURNING ...`.
- No transaction-scoped lock held across the upstream vLLM call. Bind lease
  slot → run upstream → release/confirm slot on finalize, analogous to envelope
  RESERVED → CAPTURED/VOIDED.

## What makes a lease safe to over-issue (or not)

**Not safe to over-issue** relative to the worker's advertised
`available_sequences` *at issue time*: issuing leases whose total remaining
slots exceed the worker's true free sequences recreates the SKIP LOCKED
false-503 / silent overbook failures the atomic decrement path already fixed.

**Safe to over-issue relative to global demand**: leases are worker-local. Two
workers can each hold leases; the market is not a single global sequence pool.

**Not safe to issue across cost classes**: see ranking interaction below.

Issue-time rules:

1. Worker must be READY, heartbeat-fresh, profile hash match.
2. Lease slots ≤ current `available_sequences` (atomic decrement of both offer
   row and lease issue in one transaction).
3. Supplier floors frozen into the lease; a later cheaper offer does not rewrite
   in-flight leases (same immutability as contract rates today).
4. Expiry returns unused slots to the offer row (or to zero if the worker is
   gone — see death).

## Worker death mid-lease

| Situation | Action |
| --- | --- |
| Heartbeat lost, lease still ACTIVE | Mark lease `FAILOVER_REQUIRED` / `DEAD`; do not assign new work |
| In-flight contracts on that worker | Existing realtime recovery voids or settles by evidence; lease slots for those contracts stay reserved until contract terminal |
| Unused remaining slots | Release to zero on the dead worker's offer (offer is not READY); do **not** silently transfer to another worker — that would skip market clearing |
| Replacement capacity | New work clears the market (or a new lease is issued to a live worker). A lease is not a transferable bearer instrument |

Crash-safety direction (same as envelopes): **err toward holding the slot
reservation** until the bound contract is terminal, so a restart cannot
double-assign the same sequence. Orphan-slot recovery after grace voids holds
with no live contract (no phantom permanent capacity lock).

## Interaction with cost-rank ordering

Batch claim (`control/scheduler.go`) and realtime authorization share a
discipline:

> **Cost rank wins; warmth only breaks ties within a cost class.**

A capacity lease must not become a way to route to a more expensive class.

Concrete rules:

1. **Cost class is frozen at lease issue** from the same verified-outcome cost
   inputs used in `AuthorizeRealtimeContract` (base ask adjusted by measured
   fail/refund rates when sample thresholds are met).
2. The hot path may assign a request to a worker's lease **only if** that
   lease's cost class is still in the minimum cost class among *currently
   eligible* READY offers for the profile. If a cheaper class appears, either:
   - stop assigning from more expensive leases (preferred), or
   - require re-clearing for that request.
3. Warmth (HOT/WARM/COLD) may only choose among leases **inside** the same cost
   class. A HOT expensive lease must never beat a COLD cheaper class.
4. Self-declared warmth must not influence the cost class key.

Pseudo-policy for the assigner:

```
eligible = READY offers for profile with heartbeat freshness
min_class = min(cost_class(o) for o in eligible)
assignable_leases = leases where lease.cost_class == min_class
                    and lease.remaining_slots > 0
                    and lease.worker in eligible
pick = min_by (warmth_rank, worker_id) among assignable_leases
atomic_decrement(pick.remaining_slots)
```

If `assignable_leases` is empty, fall back to full market clear (today's path).

## Interaction with execution envelopes

Envelopes amortize **buyer funding**. Leases amortize **supply selection**.
They compose:

1. Spend envelope (buyer cap) — one UPDATE.
2. Assign lease slot (supply) — one UPDATE.
3. Insert contract binding both.
4. Upstream call (no durable locks held).
5. Finalize: capture envelope spend + release lease slot accounting; supplier
   entitlement still from the per-request PricingDecision, unchanged.

Supplier liability remains the settlement path that exists today. Leases change
*when* capacity is reserved, not *how much* a supplier earns for delivered work.

## Over-issue summary

| Axis | Over-issue? |
| --- | --- |
| Worker's free sequences at issue | No |
| Buyer funding (use envelope) | No — separate authority |
| Cost class vs live book | No — lease unusable if not in min class |
| Warmth preference | Yes, only as tie-break within class |

## Implementation sketch (future)

Tables: `capacity_leases`, `capacity_lease_assignments` (idempotent per request),
events. Ticker: expire leases, recover orphan assignments, reconcile offer
`available_sequences` with sum of active lease remainders + unleased free.

Do not implement until envelope tests are green and the funding amortization is
measured as a real win under concurrent load.


<!-- source: docs/FRONTEND_CONTRACT.md -->

# Frontend and API contract

The private-pilot UI is a thin client of the control API. PostgreSQL remains the
lifecycle and money authority; browser state is never authoritative.

| UI flow | API | Success state | Required failure states |
|---|---|---|---|
| Sign up / sign in | `POST /v1/signup`, `POST /v1/login` | revocable session; `Cache-Control: no-store` | validation, throttled, invalid credential |
| Identify account | `GET /v1/me` | buyer id, email, remaining credit | expired/revoked session |
| Discover capacity | `GET /v1/models`, `POST /v1/quote` | supported model and bounded quote | unsupported tuple, no eligible capacity, malformed input |
| Submit | `POST /v1/jobs` with `Idempotency-Key` | one job id, estimate, webhook secret once | 402 funding, 409 key/body conflict, capacity, validation |
| Track | `GET /v1/jobs/{id}`, `/events`, `/failures` | explicit lifecycle plus typed failures | 404 for absent or other-buyer ids |
| Cancel | `DELETE /v1/jobs/{id}` | repeatable `cancelled` response while owned | 409 once work or verification makes cancellation unsafe |
| Download | `GET /v1/jobs/{id}/results` | buyer-scoped result references | incomplete, missing artifact, integrity failure |
| Invoice / receipt | `/invoice`, `/receipt` | accepted composite pricing, exact authority digests, catalogue/FX provenance, and modeled-versus-settled economics are labeled | incomplete settlement, provider outcome unknown, legacy authority explicitly unverifiable |
| Supplier onboarding | `/v1/supplier/*` | connected status and revocable device credential | KYC/provider pending, revoked enrollment, unsupported Mac |

Every buyer request must treat `401` as re-authentication, `403` as lack of
authority, `404` as an opaque object miss, `409` as a state/idempotency conflict,
`422` as a validation failure, `429` as backoff, and `5xx` as retryable only when
the operation is naturally idempotent or the same idempotency key is reused.

The current checked-in website is intentionally minimal. A richer dashboard must
not invent optimistic success: queued, running, verifying, complete, failed, and
cancelled are distinct, and payment/provider `outcome_unknown` must be shown as
pending operator resolution rather than success or failure.


<!-- source: docs/MEDIA_RENDERING_CONTRACT.md -->

# Bounded media rendering contract

The billable unit for this lane is one declared output pixel. The closed scene
JSON is an input commitment and validation boundary, not a proxy for physical
work; quotes and settlement therefore use `render_width × render_height` per
scene. This keeps the price authority aligned with the renderer benchmark's
pixels/second unit and prevents a tiny scene document from underpricing a large
canvas.

`media_rendering` is a deterministic vector-scene rasterisation lane, not image
generation. A buyer supplies a closed JSON scene containing a background colour
and at most 256 clipped rectangles. The governed builtin cell renders one P6
portable-pixmap artifact at the declared 16..1024 canvas size.

The control plane validates the closed scene, dimensions, pixel bound, one
primary plus one independent result, byte-exact agreement, and fixed-point
settlement. The agent performs the same scene validation before rasterising.
There is no model download, prompt-to-image claim, external asset fetch, or
unbounded renderer command surface. This lane does not activate the image
generation route.


<!-- source: docs/MEDIA_TRANSCODE_CONTRACT.md -->

# Merc media-transcode contract

`ffmpeg-transcode-v1` is a fixed built-in runtime contract, not a buyer command
surface. The agent invokes the pinned local FFmpeg/ffprobe executables with one
constant video-only template, clears the child environment, strips metadata,
and verifies the output before it can be committed.

The control plane accepts only MP4, MOV, WebM or Matroska inputs up to 64 MiB.
Width and height are even values from 64 through 4096, frame rate is 1–60 FPS,
and the requested H.264 bitrate is 200–50,000 kbps. The resulting MP4 is capped
at 64 MiB by the control-plane verification policy; the agent's stricter
runtime contract also bounds dimensions, duration, frame rate and process time.

This document is the identity contract for the built-in model row. It does not
grant rights to FFmpeg, libx264, or any other third-party codec. Those runtime
and distribution terms remain a separate legal review item until the release
image's exact binaries and notices are verified.


<!-- source: docs/STRIPE_CONNECT.md -->

# Connecting Stripe — what actually needs a domain, and what doesn't

Short version: **you are less blocked than you think.** A domain is needed for the
Stripe *account profile* and for production. It is **not** needed to start
exercising the payment code, and it is **not** needed even for the full scenario
matrix, because `cloudflared` (already installed here) can hand you a public
HTTPS URL without owning any DNS.

## What Stripe actually requires

| Thing | Needs a public domain? | Why |
|---|---|---|
| Test-mode API keys | No | Just an account |
| Receiving webhooks locally | **No** | `stripe listen` forwards to localhost |
| Dashboard webhook endpoints (`we_…`) | **A public HTTPS URL** — not necessarily *your* domain | Stripe POSTs to it from the internet |
| `make stripe-matrix` (full scenarios) | Yes, `we_…` ids | `scripts/stripe-sandbox-scenarios.sh:43-65` fetches the endpoints and resends events to them |
| Account profile / business website | Your domain, eventually | Stripe asks for it; not enforced in test mode |
| Live mode | Yes, properly | Out of scope — deliberately |

---

## Path A — no domain, ~5 minutes — PARTIAL, read the limit

**Corrected 2026-07-26 after testing it.** An earlier version of this document
said two `stripe listen` sessions would give you two different signing secrets.
They do not. `stripe listen --print-secret` returns the **same device-level
secret** every time:

    secret 1: whsec_29a39…
    secret 2: whsec_29a39…   ← identical

So the CLI path cannot satisfy this system's requirement that the billing and
Connect webhook secrets **differ**. That check is not pedantry — it stops a
leaked billing secret from being used to forge Connect events such as "a payout
succeeded", which is the one event class that moves supplier money.

**Do not relax the check to make the CLI path work.** Weakening a real control
for local convenience is exactly the pattern this project has spent weeks
removing.

What the CLI path is still good for: exercising the billing handler on its own,
signature verification, and `stripe trigger`. Not a full configuration.

```bash
stripe login
stripe listen \
  --events payment_intent.succeeded,payment_intent.payment_failed,charge.refunded,charge.dispute.created,charge.dispute.closed \
  --forward-to localhost:8080/v1/stripe/webhook
stripe trigger payment_intent.succeeded
```

## Path B — public HTTPS — REQUIRED, not optional

Because of the Path A limit above, two Dashboard endpoints are the only way to
get two distinct signing secrets. This is the real path.

`cloudflared` is installed. A **quick tunnel** gives you a throwaway
`https://<random>.trycloudflare.com` with no account, no DNS, no cost.

```bash
cloudflared tunnel --url http://localhost:8080
```

It prints a public HTTPS hostname. Use that as the base for both Dashboard
endpoints:

```
https://<random>.trycloudflare.com/v1/stripe/webhook
https://<random>.trycloudflare.com/v1/stripe/connect-webhook
```

Create both in the Dashboard (test mode), copy each signing secret **and** each
`we_…` id, then run `scripts/stripe-setup.sh`. Now `make stripe-matrix` works.

**Understand what you are doing before you run it:** a quick tunnel publishes your
local control plane to the public internet for as long as it runs. Do it with a
scratch database, keep it short-lived, and stop it when you are done. The
hostname is random but it is not secret.

Named tunnels on your own domain are the same command with a config file, once
DNS is yours.

---

## Path C — the droplet

`docker-compose.prod.yml` and `Caddyfile` already terminate TLS and reverse-proxy
`control:8080`. Point `SITE_HOST` at the new domain and Caddy will obtain a
certificate. This is the path that ends with a URL you can put on the Stripe
account profile.

---

## Which to use

- **Path A** exercises the billing handler but cannot finish the configuration.
- **Path B is required** for a working setup — two endpoints, two secrets. Still
  needs no domain of your own if you use a quick tunnel.
- **When the domain lands:** Path C, and update the account profile.

## The one thing to watch

Transfer reversal is implemented, simulator-tested, and **has never met real
Stripe**. It is the only money path in this system that can lose funds
irrecoverably — a chargeback after a supplier payout — and the only one whose
receipt has not been earned. When you get to the matrix, read that scenario's
output rather than the summary line.

## Currency mismatch found by the matrix, resolved in the candidate

The 2026-07-26 matrix proved that a USD-only ledger could not fund a transfer
from the Canadian platform's CAD settlement balance. The candidate no longer
has that split authority:

| | |
|---|---|
| Runtime authority | `MERC_SETTLEMENT_CURRENCY`, fixed to `cad` in the Level B staging manifest |
| Catalogue FX authority | Operator-supplied `MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE` and immutable `MERC_PRICE_FX_REVISION`; boot refuses a missing cross-currency authority |
| Stripe preflight | Requires a CAD platform balance bucket |
| Connected account | Requires a distinct, project-controlled Canadian Sandbox account with payouts enabled |
| Provider matrix | Creates and reconciles CAD PaymentIntents, refunds, transfers, bank fixtures, and payouts |

The shared sandbox contract rejects a USD override, a US connected account,
aliased webhook endpoint IDs, or endpoints outside the exact approved staging
host and paths. This closes the local configuration contradiction; the
provider-owned matrix receipt itself remains `NOT EXECUTED` until the external
Stripe Sandbox inputs exist.

## Current state (2026-07-26)

| | |
|---|---|
| Stripe account | `acct_1TxbzR…` — display name **"merc sandbox"** |
| Keys in `.env` | test-mode secret + publishable, pulled from the CLI config |
| Previous live `sk_live_` key | **removed** |
| API reachability | verified, `livemode: false` |
| Connect | **not enabled** — one click at dashboard.stripe.com/connect |
| Connected accounts | 0 — one is needed for `STRIPE_TEST_CONNECTED_ACCOUNT_ID` |
| Webhook secrets / endpoint ids | not set — needs Path B |

Note the CLI test key carries an expiry (`2026-10-25`). It works now, but for
anything long-lived take a key from the Dashboard instead of the CLI config, or
this breaks in three months with a confusing auth error.


<!-- source: docs/STRIPE_SANDBOX_SETUP.md -->

# Stripe Sandbox setup

Do not add real money.
Do not use a real card.
Do not use sk_live.
Do not use live connected accounts.

This procedure is only for merc’s supervised no-value private canary.
It does not authorize production Stripe mode, real charges, transfers, payouts,
or public enrollment.

## Least-privilege inputs

Create or select a Stripe Sandbox and place the following values in the
gitignored mode-0600 `.env.go-closure` or an approved secret manager. Never put
them in chat, Git, CI output, screenshots, or evidence:

- `STRIPE_SECRET_KEY`: `sk_test_*`, or a sufficiently scoped `rk_test_*` key.
- `STRIPE_WEBHOOK_SECRET`: the `whsec_*` secret for the Sandbox cash-event endpoint.
- `MERC_CONNECT_WEBHOOK_SECRET`: a distinct `whsec_*` secret for the Sandbox Connect endpoint.
- `MERC_CONNECT_CLIENT_ID`: the Sandbox Connect client identifier.
- `STRIPE_TEST_CONNECTED_ACCOUNT_ID`: a disposable `acct_*` Sandbox connected account.
  It must be a project-controlled Canadian test account with payouts enabled; the
  bundled matrix temporarily installs Stripe's documented success/failure test
  bank fixtures for Canada, switches the payout schedule to manual, and restores
  it. The reviewed candidate, staging runtime, PaymentIntents, transfers, refunds,
  connected-account balance, payouts, and receipt are all bound to CAD.
- `STRIPE_BILLING_WEBHOOK_ENDPOINT_ID`: the enabled `we_*` Sandbox endpoint for
  `/v1/stripe/webhook`.
- `STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID`: the enabled `we_*` Sandbox endpoint for
  `/v1/stripe/connect-webhook` and connected-account events.
- `STAGING_TLS_HOSTNAME`: the approved private-canary DNS name. The matrix
  requires the two endpoint URLs to equal
  `https://<host>/v1/stripe/webhook` and
  `https://<host>/v1/stripe/connect-webhook`; any other HTTPS host or path fails.
- Stripe CLI: installed locally; the command pins `STRIPE_API_KEY` to the already
  validated test-only key and never uses a saved live profile.

The API key needs only the Sandbox permissions used by customer, PaymentIntent,
refund, transfer, connected-account, event, and reconciliation checks. The
provider scenario driver is bundled at
`scripts/stripe-sandbox-scenarios.sh`; no operator-supplied code is required. Its
JSON receipt contains only identifier classes and no credential-shaped value.

## Run

```sh
chmod 600 .env.go-closure
make stripe-check
make stripe-matrix
```

`stripe-check` validates credential classes and confirms both the platform and
connected account are distinct Sandbox objects, the connected account is
Canadian and payout-enabled, the platform exposes a CAD settlement bucket, the
endpoint IDs and secrets are distinct, and both endpoint URLs/events/payload
versions exactly match the staging authority. Every live-key check occurs
before the first network request. `stripe-matrix` creates disposable CAD
objects, exercises the matrix, emits sanitized JSON, and attempts supported
cleanup. It forces an impossible client deadline and retries the exact
idempotency key, proving safe recovery when persistence at the provider is
unknown. The deterministic simulator separately covers both pre-persistence
timeout and provider success followed by a lost response. As part of the
matrix, each configured webhook endpoint receives one inert signed probe and
one invalid-signature probe; it must accept only the event signed by its
supplied Sandbox `whsec_*` secret. The billing endpoint then receives three
unique signed, no-value dispute envelopes: terminal state first must report
`applied`, the older opening state must report `stale_ignored` behind the same
cash-effect rank, and a byte-identical terminal replay must report `duplicate`.
These non-secret response headers turn replay/out-of-order safety into a
staging-database observation rather than a declared receipt field.

Until both commands pass with provider-owned receipts, readiness must remain:

```text
internal payment model: PASS
deterministic provider simulator: SIMULATED PASS
Stripe sandbox integration: NOT EXECUTED
Stripe live integration: PROHIBITED
```


<!-- source: docs/MONEY_AUDIT_2026-07-27.md -->

# Money audit, 2026-07-27

Ten agents across five dimensions (loss paths, fee efficiency, margin, ledger
integrity, supplier profitability), each finding attacked by an independent
verifier instructed to refute it. What survived is below, worst first.

I re-verified findings 1 and 2 against the code myself before writing this.

---

## 1. Suppliers are paid exactly $0.00, and always will be

**Confirmed independently by three of the five audit dimensions.**

`ClaimPayout` floors each ledger entry to whole cents:

```go
cashCents = liabilityMicros / microUSDPerCent   // control/payment.go:57, integer floor
```

When the entry is worth less than a cent, `RequestedCents == 0` and the entry is
set to `payout_status='carried'` (`control/store_payouts.go:693-707`). Then:

- `PayoutCarried` has exactly **one writer** in the whole tree — that line.
- **No code anywhere moves `carried` back to `held`.**
- `DuePayouts` selects `payout_status='held'` only (`store_payouts.go:577`).

So `carried` is terminal. And at catalogue prices essentially every credit is
sub-cent: a 1000-record job bills $0.00231, the supplier's ~97% share lands at
roughly 2,244 micro-USD — **0.22 of one cent**. It takes about four such jobs to
reach a single payable cent, and the entry is carried and abandoned long before.

This is the mechanical reason supplier net is −$0.00014/hr. It is not thin
margin. **The cash never leaves the platform.**

The design intended otherwise, which is what makes this a bug rather than a
policy: the settlement policy constant is literally named
`supplierSettlementPolicyFloorCentCarryV1`, the schema stores
`remainder_microusd` per settlement (`schema.sql:1822`), and
`('payout','carried','held')` is an explicitly whitelisted transition
(`schema.sql:2188`). The carry was specified and never implemented.

**Fix (not yet written — this is a money state machine and deserves its own
change):** accumulate the supplier's outstanding remainder and add it to the
next entry's liability before flooring, then settle the folded entries. The
column and the transition already exist to support exactly this.

Until it lands, `/v1/worker/earnings` reports money to suppliers that the payout
path is structurally incapable of sending.

## 2. The 24-hour age trigger charges at $0.50, where Stripe keeps 62.9%

`BuyersDueForBatch` fires when the balance reaches `chargeMinUSD` **or** the
oldest deferred job passes 24h, gated only by `SUM >= stripeMinChargeUSD`
($0.50, `collect.go:34`).

Stripe on a $0.50 charge: `0.029 × 0.50 + 0.30 = $0.3145` — **62.9%**.

But the buyer price floor was computed assuming the fixed fee is amortised over
a $5.00 batch: `processorRate = 0.029 + 0.30/5.00 = 8.9%`
(`economic_plan.go:39-44`). The plan reports Executable with positive headroom
while the charge loses about **$0.27**.

This is not an edge case, it is the only path that fires. At board prices a
buyer accrues ~$0.0045/GPU-hour, so reaching the $5.00 batch floor takes ~1,113
GPU-hours; the $0.50 age-out is what actually happens.

`economic_plan.go:261` asserts the fee "is amortised over a minimum-size charge
batch, matching how chargeOrDeferJob and FormChargeBatch actually settle". For
this path that assertion is false.

**Fix:** require `SUM >= chargeMinUSD` on the age branch too and let sub-floor
balances roll forward — nothing is lost, the receivable is already durable in the
ledger. Or make `processorFloorTerms` use the real worst-case batch, in which
case the rate becomes 62.9% and `BuildEconomicPlan` correctly refuses.

## 3. Failed charges retry forever with no attempt cap

`ReflipNoCardJobs` returns `no_payment_method` jobs to `deferred` so they rejoin
batching. There is **no equivalent for `failed`**. `retryFailedSingle` re-charges
each failed job alone — paying a solo $0.30 instead of a shared one — and
`chargeRetryBackoff` caps the *interval* at 6h but nothing caps the *attempts*.
`IncrementChargeAttempts` is recorded and logged, never compared to a limit.

A permanently dead card produces four Stripe PaymentIntent attempts per day, per
job, forever.

## 4. The advertised 3% platform take is really ~33.5%

`BuildEconomicPlan` derives the supplier's share from *compute cost*, then
derives the buyer's price from an unrelated floor, and books the difference as
`BuyerSafetyFeePerTaskUSD` — 100% to the platform.

With the shipped schedule the buyer pays $0.00068715/task while the supplier
receives $0.00045728. Effective take: **33.5%**, not the 3% `MERC_PLATFORM_TAKE_PCT`
advertises. The quote surfaces the spread to the *buyer*; the supplier has no
equivalent view.

Passing ~10% of that safety fee to the supplier would clear electricity plus a
50% margin and cost the platform ~$0.0000216/task.

## 5. One blog-sourced price sets the entire catalogue

`repriceFromMarketBoard` prices a class as **`min()`** of its observations
(`pricing.go:167-186`). For `infer_small` the three observations are $0.01, $0.04
and $0.06 per 1M — and the $0.01 row that sets the price for every supplier cites
a competitor's marketing blog, not a vendor pricing page.

`min()` is the most fragile possible selector: one stale, promotional or
mistyped row drags the whole supply side underwater, and the only validation is
that the positioning multiplier is finite and positive.

Median instead of min moves supplier net from −$0.00014/hr to **+$0.0129/hr** —
3.87× electricity. A one-line selector choice is worth 4–6× the supplier's
entire income.

## 6. Rounding at the Stripe boundary is never reconciled

Inside the ledger the money is clean — exact micro-USD, bound as
`($micros::numeric / 1000000)` so no float touches the money column. The drift is
at the rail: `chargeBuyer` converts with `int64(math.Round(usd*100))` and
`FormChargeBatch` accumulates in `float64`. Nothing ever compares
`SUM(kind='buyer_charge')` against `buyer_cash_collections.received_cents`.

Separately, `stripe_fee` is booked negative against the buyer and never
subtracted from `platform_take`, so ledger margin overstates reality by the full
processor fee: on a $5 batch `platform_take` reads $0.15 while Stripe took $0.445.

**Local resolution, 2026-07-28:** new recorded processor fees are now allocated
to batch jobs with Hamilton's largest-remainder method at micro-USD precision.
Immutable job IDs break equal-remainder ties, so permuting the same economic
facts cannot change which job receives a rounding micro-unit. Allocation is
serialized, append-only, idempotent, and rejected if the row set is partial or
does not conserve the fee exactly. Buyer invoices and clearing receipts expose
both `processor_fee_allocated_usd` and
`platform_net_after_processor_usd`, plus the versioned allocation method; a
batch invoice fails closed if a recorded fee has not been completely allocated.
Pre-upgrade rows retain and expose `legacy_order_residual_v0` rather than being
silently rewritten. Ten thousand randomized quota,
conservation, and permutation cases plus fresh-PostgreSQL concurrent mutation
tests cover the local boundary.

This does **not** close provider reconciliation. No Stripe test object, balance
transaction, refund, dispute, payout, or real cash evidence was created in this
change, and the formal Stripe test-mode matrix remains a release blocker.

## What cannot be fixed by configuration

`MERC_PLATFORM_TAKE_PCT` is clamped to [1%, 5%]. Running the whole range: at 5%
supplier net is −$0.00023/hr, at 1% it is −$0.00005/hr. **At a literal 0% take it
is still −$0.0000061/hr.** Break-even needs 143.2 tok/s at 97% share and 138.9
even at 100%; the M3 Pro measured 138.7.

The platform could take nothing at all and the supplier would still miss their
power bill by 0.13%. This knob closes 78% of the gap and cannot close the rest,
because the rest is a hardware fact. See `docs/RUNTIME_AND_PERF.md § "Speed lane, measured 2026-07-27"` — the
M3 Ultra measurement changes this arithmetic materially.

## Landed today

The one report that detects this condition, `SupplierViabilityReport`, existed
but was called **only from a test**. It now runs at boot (`control/main.go`) and
prints, against the real catalogue:

```
WARNING: supplier economics underwater: model=llama-3.2-1b-instruct-q4
job=batch_infer hw=apple_silicon_pro gross=$0.004359/hr electricity=$0.004500/hr
net=$-0.000141/hr; break-even needs 143.2 units/s, measured 138.7
```


<!-- source: docs/POSTGRES_TRUST_BOUNDARY.md -->

# Postgres trust boundary (production Compose)

## Decision

The shipped `docker-compose.prod.yml` keeps control→Postgres on
`sslmode=disable`. That is **not** a general claim that Postgres never needs
TLS. It is a deliberate choice under a narrow trust boundary that CI enforces.

## Why TLS is not required on this path

1. **Postgres is not published to the host network.** The `postgres` service in
   `docker-compose.prod.yml` has no `ports:` mapping. Only other Compose
   services on the project network can open a TCP connection to it.
2. **Control is the only application client.** The control service reaches
   Postgres via the Compose DNS name `postgres` on the private project bridge
   network. There is no remote DBA listener, no cloud proxy, and no host-port
   tunnel in the shipped file.
3. **The network segment is the confidentiality boundary.** On a single-host
   Docker/Compose deployment, traffic between containers on the internal bridge
   does not leave the host kernel's virtual Ethernet. An attacker who can sniff
   that path already has host-equivalent position and can read volume contents
   (`pgdata`) directly.
4. **Authentication remains required.** Connections still use a strong
   `POSTGRES_PASSWORD`. Disabling TLS does not disable password auth.

## When this document does **not** apply

Do **not** rely on this exception if any of the following become true:

- Postgres gains a host `ports:` publish, a public load balancer, or a
  cross-host overlay without an encrypting mesh.
- Control and Postgres run on different hosts or clusters without a private
  network + encrypting fabric.
- Compliance or threat model requires encrypting all data-in-transit regardless
  of network locality.

In those cases configure Postgres to serve TLS (server cert + key, and ideally
`hostssl` in `pg_hba.conf`) and set
`DATABASE_URL=...sslmode=verify-full&sslrootcert=...` (or `require` with a
reviewed risk acceptance). A DSN edit alone is insufficient: the server must actually present a certificate.

## CI contract

`scripts/validate-postgres-trust-boundary.py` fails the build unless:

1. This document still states the four conditions above.
2. `docker-compose.prod.yml` has no published `ports` on the `postgres` service.
3. The production control `DATABASE_URL` uses `sslmode=disable` only while those
   conditions hold (the script pins that the URL still matches the documented
   exception rather than silently drifting to a remote host).

If you enable real Postgres TLS, update the compose DSN **and** this document
**and** the validator in the same change.


<!-- source: docs/DECISION_ZERO.md -->

# Decision Zero — realtime lane: keep or kill

**Status: OPEN. Owner's call.** Everything in the remediation plan that does not
depend on this decision is done. This record exists so the call can be made from
evidence rather than from momentum, and so either branch is cheap to execute.

## What was tested

Two independent arguments were commissioned, each briefed to make the strongest
possible case for one branch and to engage the other side's steelman. Neither
was told which way anyone else leaned.

| Branch | Confidence | Driving fact |
|---|---:|---|
| `[KILL-RT]` | **78%** | Production identity is Apple Silicon + Candle only; realtime has never executed on a real GPU. |
| `[KEEP-RT]` | **72%** | Zero customers plus a batch registry that admits only Apple Silicon means realtime is the only designed path to general supply and default buyer integration. |

**Six points of confidence is not a verdict.** Two careful readings of the same
codebase landed on opposite conclusions with overlapping intervals. That is the
signal: this is not an engineering question with a hidden right answer, it is a
business-intent question, and the evidence genuinely underdetermines it.

## What both sides agree on

Worth separating from the contested part, because these are not in dispute:

1. **The lane must stop being a ghost.** 3,390 lines across
   `control/realtime*.go` and `agent/src/vllm.rs` are untracked — never in any
   CI run, never reviewed, and unrecoverable if deleted. The KEEP brief calls
   this "a process failure"; the KILL brief says archive before any purge. Both
   want it out of this state before anything else happens.
2. **Stop dual-tracking.** Whichever lane loses should stop receiving
   engineering attention immediately. The expensive failure mode is maintaining
   both.
3. **Deleting the bytes is a separate decision from stopping the lane.** In the
   KILL brief's own words: *"the case for stop-the-lane is stronger than the case
   for destroy-the-bytes-without-archive."*

A checksummed snapshot of all eight files is preserved outside the repository at
`realtime-lane-snapshot/` with a `MANIFEST.sha256`, so `[KILL-RT]` is now a
reversible operation rather than permanent loss.

## The question that actually resolves it

Not *"which code is better"* — both briefs agree the realtime money and protocol
work is substantial and the batch lane is the only thing that has run on
admissible hardware.

The question is: **within 90 days, can you get a Linux/CUDA supplier to register
and serve real traffic?**

- **Yes** → `[KEEP-RT]`. The realtime lane is the only path to that supply, the
  hard money and protocol work already exists, and OpenAI compatibility is the
  default integration buyers already have.
- **No** → `[KILL-RT]`. Selling a latency SLA on machines the control plane
  returns HTTP 400 for is not a product, and the 8–12 day Batch API port buys a
  buyer on-ramp for supply that actually exists.

Nobody but the owner can answer that, which is why the plan assigned this
decision here and why it stays open.

## Cost of each branch

| | `[KILL-RT]` | `[KEEP-RT]` |
|---|---|---|
| Immediate | delete 3,390 lines (snapshot preserved) | `git add` the lane, put it in CI |
| Next spend | Batch API port, **8–12 days** — a port, not a revert: `api.go` moved 2,111 lines and `store.go` 4,681 since `89267633` | one Linux CUDA host, real vLLM parity evidence, ~2 days + rental |
| Then | demote streaming in `EXECUTION_NETWORK_BIBLE.md` and the site | widen `validHWClasses` / `validEngines` beyond Apple + `candle` |
| Residual risk | Apple-only supply may never form; canary still prohibits independent suppliers | commodity chat competes on price with hyperscalers; cash rails unproven |

## What is already true regardless

The money holes that made `[KEEP-RT]` dangerous are closed in the working tree:
buyers are charged for realtime usage, the saved-card free-inference
short-circuit is gone, every ledger write goes through one integer-micro writer,
and sequence reservation is atomic and proven under concurrency. Whichever way
this goes, those were worth doing — they are not sunk cost in the losing branch.


<!-- source: docs/DECISION_ZERO_OUTCOME.md -->

# Decision Zero — resolved: `[KILL-RT]`

**Decided by the owner, 2026-07-26.** The question was: *within 90 days, can you
get a Linux/CUDA supplier to register and serve real traffic?* The answer was
"assume not", which selects `[KILL-RT]`.

## What that means

The realtime OpenAI-compatible streaming lane is removed from the product. It was
never tracked in git, never in any CI run, and never executed against a GPU.

`control/types.go` admits only four `apple_silicon_*` hardware classes and the
`candle` engine, and `proto/manifest.schema.json` pins `engine` to `"candle"`. An
NVIDIA worker is rejected at registration with HTTP 400. Selling a latency SLA on
hardware the control plane refuses to register is not a product, and maintaining
a second lane for supply that will not arrive is the expensive failure mode.

## Recoverability

The lane is preserved outside the repository as a checksummed snapshot:

    realtime-lane-snapshot/            9 files, 3,680 lines
    realtime-lane-snapshot/MANIFEST.sha256

Verified with `shasum -a 256 -c MANIFEST.sha256` immediately before removal. This
is a reversible decision, which is why the snapshot was taken before the delete
rather than after.

## What survives, and why it was still worth building

The money and correctness work done on the realtime lane was not wasted, because
it was mostly done to shared machinery that the batch lane uses too:

- one integer-micro ledger writer, enforced by test
- Stripe transfer reversal and the global payout pause
- the atomic capacity reservation pattern
- the buyer-scoped balance indexes
- prepaid balances

## What this does not decide

`[KILL-RT]` says the realtime lane is not the product. It does not say the batch
AI lane is. Market research commissioned the same day found that no AI inference
lane pays a supplier above electricity at market prices, and that the best
non-AI lane — deadline-tolerant GPU rendering — is roughly three orders of
magnitude better per supplier-hour. See `docs/PROGRAMME.md § "Which lane can actually pay a supplier"`. That is a
separate decision and it is still open.


<!-- source: docs/DECISION_ZERO_REVERSAL.md -->

# Decision Zero reversal — `[KEEP-RT]` supersedes `[KILL-RT]`

**Status: EXECUTED 2026-07-27.** The MERC COMPLETE-SHIPPABILITY goal is the
owner directive that supersedes `[KILL-RT]`; the lane is restored and green.

`docs/ARCHITECTURE.md § "Decision Zero — resolved: `[KILL-RT]`"` recorded `[KILL-RT]` on 2026-07-26: the
OpenAI-compatible realtime lane was removed from the product and preserved as a
checksummed snapshot. The MERC COMPLETE-SHIPPABILITY goal lists that lane first
and says "Do not narrow launch to batch only", which reverses it.

I have **not** restored the lane on that inference alone. This file records why
the reversal is probably correct, and exactly what it costs, so the decision is
made once and in writing rather than drifting.

## Why KILL-RT was decided

The question was: *within 90 days, can you get a Linux/CUDA supplier to register
and serve real traffic?* The answer was "assume not". The supporting facts:

- `control/types.go` admits only four `apple_silicon_*` hardware classes.
- `proto/manifest.schema.json` pins `engine` to `"candle"`.
- An NVIDIA worker is therefore **rejected at registration with HTTP 400**.

Selling a latency SLA on hardware the control plane refuses to register is not a
product.

## What changed

**The goal directs CUDA supply.** The shippability goal's second lane is
"RunPod-backed pinned vLLM workers through Merc routing, verification and
money", which is precisely the CUDA supply whose absence justified the kill.

**Correction, 2026-07-27.** An earlier version of this paragraph asserted
"RunPod is set up." That was inferred from the goal text and never checked. It
is false: there is no RunPod API key in the environment, no `~/.runpod`, no
`runpodctl`, and no credential in any `.env`. The reversal is still right — the
admission change is worth making and the realtime surface is worth having — but
it was justified partly on a premise I had not verified, and the lane cannot
reach `REAL_RUNTIME_PROVEN` until a funded RunPod credential exists.

## What the reversal actually costs

Restoring the lane is not just re-adding files. The hardware admission that made
it useless is still in force:

| work | where |
|---|---|
| Admit CUDA hardware classes | `control/types.go` |
| Admit a non-candle engine | `proto/manifest.schema.json` |
| Restore 5 Go files + runtime profile | `realtime-lane-snapshot/` (verified, 8 files + manifest) |
| Migrate the restored store to the sole ledger writer | the snapshot predates `control/ledger_write.go`; its raw `INSERT INTO ledger_entries` violates `TestNoRawLedgerInsertsOutsideWriter` |
| Restore schema DDL | ~297 lines removed when KILL-RT was completed |
| Re-add 5 routes to the authorization matrix | count returns 72 → 77 |
| Re-add 3 alerts + metrics + dashboard panel | removed as dead when the lane went |

**Realtime and RunPod are one lane, not two.** Neither is shippable without the
hardware-admission change, and the hardware-admission change is what makes both
worth doing.

## Recommendation

Confirm `[KEEP-RT]`, and treat it as a single lane: *CUDA admission + realtime
surface + RunPod worker*, proven end to end through routing, verification and
settlement before any public claim.

Do not restore the realtime files on their own. A realtime surface with no
registrable hardware is what KILL-RT correctly deleted, and re-adding it without
the admission change recreates exactly that.

## Note for whoever reads this next

The realtime files were never committed to git. Their absence looks identical to
data loss, and I restored them once by mistake earlier in this session before
finding the `[KILL-RT]` marker in `control/authorization_matrix_test.go`. Grep
for `KILL-RT` before concluding anything is missing.

## What was actually done

- CUDA hardware classes (`nvidia_24gb/48gb/80gb/180gb`) and the `vllm` engine
  admitted, paired so an engine cannot run on hardware that cannot serve it.
- 5 Go files + runtime profile restored from the verified snapshot, renamed
  `CX_` to `MERC_` to match the rest of the tree.
- 289 lines of realtime DDL restored; schema applies clean.
- 5 routes, the admin refund handler, the recovery sweep, 7 counters and the
  operational gauge block restored. Authorization surface 72 -> 77.
- `scripts/realtime-parity-benchmark.py` **rewritten** — the original was
  untracked and destroyed in the KILL-RT cleanup. The rewrite reconstructs the
  contract from its call site and keeps the attestation gate: a run against an
  unattested upstream reports `UNATTESTED_HARNESS_RUN` and refuses
  `public_claim_allowed`.

**Resolved 2026-07-27.** `realtime-openai-python-conformance.py` and
`realtime-openai-node-conformance.mjs` were rewritten from their call sites in
`control/realtime_integration_test.go`, which specifies the full contract: seven
capability flags, all required, plus `status == "PASS"`. Both now pass against
the real `openai` Python 2.48.0 and JavaScript 6.49.0 clients.

`realtime-sdk-conformance.sh` is NOT reconstructed. Unlike the other two it left
no call site carrying assertions - only a Makefile target name - so rebuilding
it would mean inventing a contract and calling the invention a recovery. The
`realtime-sdk-conformance` target now runs the two harnesses that do have a
specified contract, and fails loudly rather than skipping when the SDKs are not
configured.

## Bug this uncovered

Completing KILL-RT had removed the `execution_contracts` term from
`BuyerFreeCreditRemaining` **and the comma separating the final `GREATEST`
argument**, leaving `GREATEST(a - b - c 0)` — invalid SQL that shipped with
`make ci` green because nothing executed the query. The realtime integration
test caught it. Repaired, and `TestBuyerFreeCreditRemainingIsValidSQL` now
executes the query so it cannot recur.


<!-- source: docs/QUICKSTART.md -->

# Buyer quickstart

End-to-end path against a local control plane. Counted steps below are the
numbered buyer-facing actions after the stack is up.

## 0. Prerequisites (once per machine)

These are not buyer API steps; they stand up the local stack the rest of the
doc talks to.

```bash
# From the repository root.
cp -n .env.example .env

# Postgres must accept DATABASE_URL from .env (default
# postgres://cx:cx@localhost:5432/cx?sslmode=disable). Apply schema:
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql

# Object storage for job artifacts:
docker compose up -d minio createbuckets

# Export economic schedule (required for quotes and job intake):
set -a && source .env && set +a

# Seed demo buyer + worker credentials, then start control:
(cd control && go run . seed)
(cd control && go run .)
```

`cx seed` prints `api_key = dev-api-key-0001`. Keep that shell's control process
running for the steps below.

## Buyer steps

### 1. Export the control URL and buyer key

```bash
export MERC_URL=http://localhost:8080
export MERC_API_KEY=dev-api-key-0001
```

Every buyer request uses `Authorization: Bearer $MERC_API_KEY`.

### 2. Discover models

```bash
curl -fsS "$MERC_URL/v1/models" -H "Authorization: Bearer $MERC_API_KEY"
```

### 3. Quote an embeddings job

Quotes scan real input (JSONL string or object). They are source-bound, expire,
and do not reserve capacity or move money. With market-board catalogue prices,
use enough text that the microdollar estimate stays positive (two short lines
can round to $0 and return `409 conflict`).

The response includes `pricing_decision`, which binds the workload, compute,
placement and economic digests to the exact catalogue schedule, market-board
digest and FX revision. Cost components that are not independently metered are
reported as `unknown`; they are not silently presented as zero.

```bash
curl -fsS "$MERC_URL/v1/quote" \
  -H "Authorization: Bearer $MERC_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "job_type":{"type":"embed","batch_size":8},
    "model":{"ref":"all-minilm-l6-v2"},
    "tier":"batch",
    "input":"{\"text\":\"the quick brown fox jumps over the lazy dog with enough tokens to clear the microdollar floor\"}\n{\"text\":\"another reasonably long sentence so base_compute_usd stays positive after rounding\"}\n"
  }'
```

### 4. Submit embeddings

```bash
JOB_ID=$(curl -fsS "$MERC_URL/v1/jobs" \
  -H "Authorization: Bearer $MERC_API_KEY" \
  -H "Idempotency-Key: quickstart-embed-001" \
  -H 'Content-Type: application/json' \
  -d '{
    "job_type":{"type":"embed","batch_size":8},
    "model":{"ref":"all-minilm-l6-v2"},
    "params":{"split_size":1},
    "tier":"batch",
    "input":"{\"text\":\"the quick brown fox jumps over the lazy dog with enough tokens to clear the microdollar floor\"}\n{\"text\":\"another reasonably long sentence so base_compute_usd stays positive after rounding\"}\n"
  }' | jq -r .job_id)
echo "$JOB_ID"
```

For batched text generation, use
`"job_type":{"type":"batch_infer","max_tokens":32,"temperature":0}`,
model `llama-3.2-1b-instruct-q4`, and JSONL records with a `prompt` field.

### 5. Inspect the job

```bash
curl -fsS "$MERC_URL/v1/jobs/$JOB_ID" \
  -H "Authorization: Bearer $MERC_API_KEY"
```

Without a live agent the job stays queued; that is expected on a control-only
stack.

The clearing receipt exposes the same authority and its reconciliation:

```bash
curl -fsS "$MERC_URL/v1/jobs/$JOB_ID/receipt" \
  -H "Authorization: Bearer $MERC_API_KEY"
```

New jobs report `authority_status: "verified"`. Historical jobs that predate
composite pricing remain readable as `legacy_unverifiable`; the server never
reconstructs their accepted price from today's catalogue.

### 6. Fetch results (after an agent completes the job)

```bash
curl -fsS "$MERC_URL/v1/jobs/$JOB_ID/results" \
  -H "Authorization: Bearer $MERC_API_KEY"
```

Result records preserve input order. Before completion this returns a conflict.

### 7. Cancel (idempotent; unsettled work only)

```bash
curl -fsS -X DELETE "$MERC_URL/v1/jobs/$JOB_ID" \
  -H "Authorization: Bearer $MERC_API_KEY"
```

## Python SDK

### 8. Install the package from the checkout

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install ./clients/sdk/python
```

The package is **not** published to PyPI (this repository has no LICENSE, so
publishing is blocked). Install from the tree or a built wheel only.

### 9. Submit and wait with the SDK

Requires a worker agent online for `wait` to finish. Against control-only, use
`submit_job` + `get_job` instead of `wait`.

```python
from merc import Client

client = Client("http://localhost:8080", api_key="dev-api-key-0001")
job = client.submit_job(
    model="all-minilm-l6-v2",
    job_type="embed",
    input=(
        '{"text":"the quick brown fox jumps over the lazy dog with enough tokens to clear the microdollar floor"}\n'
        '{"text":"another reasonably long sentence so base_compute_usd stays positive after rounding"}\n'
    ),
)
print(client.get_job(job["job_id"])["status"])
# When an agent is running:
# done = client.wait(job["job_id"], timeout=300)
# rows = client.results_records(done["job_id"])
```

The SDK has no runtime dependency outside the Python standard library. Smoke
without a server:

```bash
python clients/sdk/python/example.py --smoke
```

## Step count

**9 buyer-facing steps** after prerequisites (export → models → quote → submit →
inspect → results → cancel → install SDK → SDK submit). Prerequisites (schema,
MinIO, seed, control process, economic env) are separate setup.


<!-- source: docs/DEPENDENCY_REVIEW.md -->

# Release-candidate dependency review

Status: source review complete for the supervised test-mode canary. This is
not a live-money dependency approval.

## Rust `paste` 1.0.15

`paste` is not a direct merc dependency and there is no `paste!`
use in the agent source. `cargo tree -i paste` shows it is required by the
locked numeric/runtime graph: Candle 0.10.2 through `gemm`/`pulp`, and both the
Candle and direct Tokenizers graphs through `macro_rules_attribute`.

Removing it would require replacing or forking the pinned model runtime, not a
small local macro rewrite. It is therefore retained for this RC. Locked Cargo
metadata reports `paste` 1.0.15 from crates.io, repository
`https://github.com/dtolnay/paste`, licensed `MIT OR Apache-2.0`. CI runs the
locked RustSec audit; the RC gate fails on an applicable advisory.

Re-evaluate this transitive edge whenever Candle or Tokenizers is upgraded.


<!-- source: docs/CANARY_DRIVER_FINDINGS.md -->

# Confirmed defects in the canary scenario driver

Independent adversarial lenses; entries found by more than one lens are marked.

## [CRITICAL] canary-scenario-driver.sh:363  (found by 2 lenses)
**safety.stripe_test_mode / real_value are hardcoded constants; the control plane's actual payment mode is never observed**

Defect: emit_receipt stamps `"safety": {"stripe_test_mode": True, "stripe_live_mode": False, "real_value": False, ...}` (scripts/canary-scenario-driver.sh:363-369) as Python literals for all 14 scenarios. The only thing behind those literals is `reject_live_stripe` (scripts/canary-scenario-driver.sh:46-57, called at :1344 and :1359), which inspects five environment variables *in the driver's own shell*: STRIPE_SECRET_KEY, STRIPE_LIVE_SECRET_KEY, STRIPE_RESTRICTED_KEY, STRIPE_PUBLISHABLE_KEY, NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY. None of those is the variable the deployed control plane reads. docker-compose.prod.yml:14 passes only `STRIPE_SECRET_KEY_FILE: ${STRIPE_SECRET_KEY_CONTAINER_FILE:-}` into the control container; the key itself is a mounted file (docker-compose.prod.yml:144). control/payment_authority.go:405-407 makes this explicit: LIVE payment mode *requires* STRIPE_SECRET_KEY_FILE and *forbids* an environment-inline STRIPE_SECRET_KEY. So a control plane in LIVE mode is exactly a control plane whose Stripe key the driver's refusal loop cannot see, by construction. The driver never reads STRIPE_SECRET_KEY_FILE, STRIPE_SECRET_KEY_SOURCE, or MERC_PAYMENT_MODE, and never asks the control plane. The information is one HTTP call away and already plumbed: GET /readyz returns `payment_mode`, `provider_enabled` and `live_value_movement` (control/api.go:393-399). Nothing upstream closes the gap either - gc_probe_release explicitly *accepts* live mode (`.payment_mode == "sealed" or ... or .payment_mode == "live"`, scripts/lib/go-closure-common.sh:287), and gc_validate_host_config:155 only checks the host-shell STRIPE_SECRET_KEY that the container does not receive. The validator then re-asserts the same constants (scripts/validate-canary-scenario-receipt.py:185-192), so a receipt cannot fail this check - it is a tautology from literal to literal.

Failure: Staging .env: STRIPE_SECRET_KEY=sk_test_51abc... (satisfies gc_validate_host_config:155 and reject_live_stripe), MERC_PAYMENT_MODE=live, MERC_ENV=production (docker-compose.prod.yml:6 default), STRIPE_SECRET_KEY_SOURCE=/etc/merc/stripe-live.key containing sk_live_51..., plus the live activation file/HMAC. The container starts in PaymentModeLive because compose never injects STRIPE_SECRET_KEY (payment_authority.go:405 is satisfied) and its credentialClass is sk_live. gc_probe_release accepts payment_mode=="live". The rehearsal then runs embed_success (minimum 20) and batch_infer_success (minimum 20): 40 real jobs settle with live_value_movement=true, charging real cards and issuing real Connect payouts to suppliers. All 14 receipts carry safety.stripe_test_mode=true, stripe_live_mode=false, real_value=false; validate-canary-scenario-receipt.py:185-192 passes every one; the rehearsal writes policy.real_value:false into the final PASS receipt (scripts/go-closure-canary-rehearsal.sh:436-437). CANARY_PROVEN authority is minted certifying that no real value moved, while real money moved.

Suggested fix: In setup_http (scripts/canary-scenario-driver.sh:120-129), fetch GET "$CONTROL_BASE/readyz" and die unless `.live_value_movement == false` and `.payment_mode` is "sealed" or "test"; carry the observed `payment_mode` and `live_value_movement` into the safety block instead of the literals at :364-366 so the receipt records what was measured. Tighten scripts/lib/go-closure-common.sh:287 to refuse payment_mode=="live" for canary runs. Separately, make reject_live_stripe compare a whitespace-trimmed value - `case "$value"` at :51-52 does not trim, while every Go consumer does (control/stripe_simulator.go:644 getenvTrim, control/payment_authority.go:405), so " sk_live_..." is classified live by the money code and not-live by the guard.

## [CRITICAL] canary-scenario-driver.sh:579  (found by 4 lenses)
**distinct_metal_agent emits occurred_at from a past heartbeat, which the validator rejects as outside the scenario window**

Defect: This is the only scenario whose evidence `occurred_at` is a database timestamp rather than wall-clock now: line 579 selects `to_char(w.last_seen_at AT TIME ZONE 'UTC', ...)` and line 593 binds it as `--arg at "$occurred"`. But `started_at` is captured at scenario entry (line 570, `started="$(utc_now)"`) and validate-canary-scenario-receipt.py:239-241 rejects any evidence with `occurred < started`. Since last_seen_at is the previous heartbeat (up to 30s old, agent/src/main.rs:1976), occurred_at is essentially always strictly before started_at. Every sibling scenario deliberately avoids this — scenario_job_success reads the DB row for durability then overwrites the timestamp with `utc_now()` and even comments why (lines 640-641: "Use finished-window occurrence (now), still inside scenario window"). distinct_metal_agent was missed.

Failure: Suppose finding #1 is fixed or a lucky run has both workers fresh: started_at = 2026-07-28T12:00:05Z, worker A last_seen_at = 12:00:02.4, worker B = 12:00:04.1. The driver emits evidence occurred_at values 2026-07-28T12:00:02Z and 2026-07-28T12:00:04Z and exits 0 with a receipt on stdout. validate_scenario_receipt (go-closure-canary-rehearsal.sh:387) then fails with "evidence[0] occurred outside the scenario window" and gc_die aborts the run. For the receipt to validate, both workers would have to have heartbeated within the same wall-clock second as `started` — roughly 0.03% of runs at a 30s cadence.

Suggested fix: Keep last_seen_at as the freshness predicate in the WHERE clause, but set occurred_at from `utc_now()` at append time, exactly as scenario_job_success does at line 641. Drop the to_char column from the SELECT and read only w.id.

## [CRITICAL] canary-scenario-driver.sh:866  (found by 1 lens)
**stale_attempt_commit_rejection's HTTP 409 is produced by the claim check, not the attempt fence — the named work never happens**

Defect: The scenario forces a requeue (lines 836-857) and then posts a commit with the stale attempt number, treating HTTP 409 as proof that the attempt-epoch fence rejected it (line 871). But `FailTaskTx` sets `claimed_by = NULL, worker_id = NULL` on requeue (control/failure.go:156), and `completeTaskTx` gates on the single conjunction `WHERE id=$1 AND claimed_by=$2 AND execution_worker_id=$2 AND retry_count=$3 AND status IN (...)` (control/store_tasks.go:319-320), all misses collapsing into one 409 `"task not claimed by this worker or not committable"` (control/api.go:2330). The commit is therefore already rejected by the claim predicate before `retry_count` is ever compared. Nothing in the receipt or in `corroborate_scenario` (go-closure-canary-rehearsal.sh:252-271, which only re-reads `t.retry_count=$current`) distinguishes the two causes.

Failure: Delete the `AND retry_count=$3` conjunct from `completeTaskTx` (control/store_tasks.go:319) — i.e. remove attempt fencing entirely, the exact regression this scenario exists to catch. Re-run `driver run stale_attempt_commit_rejection 3`: each iteration still gets 409 (task is unclaimed after the requeue), before/after state hashes still match, `current_attempt > submitted_attempt` still holds, and three PASS receipts are minted asserting `http_status: 409` stale-attempt rejection for a control plane that no longer fences stale attempts.

Suggested fix: Re-claim the task with the same worker at the new attempt before issuing the stale commit, so `claimed_by`/`execution_worker_id`/`status` all match and `retry_count` is the only failing predicate. Alternatively assert on a distinguishable response (a distinct error code/body for attempt-epoch rejection) rather than on the bare 409 status.

## [CRITICAL] canary-scenario-driver.sh:1006  (found by 4 lenses)
**backup_independent_restore asserts an independent download and isolated restore that the driver never performs**

Defect: The extras object hardcodes `independent_download:true, ciphertext_checksum_verified:true, isolated_restore:true, postgres_semantic_checks:true, object_checks:true` (lines 1006-1013). The driver runs only `scripts/backup.sh` (line 984); `scripts/restore.sh` is checked for the executable bit at line 981 and then never invoked, so no restore, no Postgres semantic check and no object check is ever executed. The evidence subject is `backup-${cipher:0:32}` where `cipher` falls back to `sha256_file "$drill_out"` — the SHA-256 of the driver's own captured stdout temp file (lines 991-996) — so the subject declared as `source:"offsite_backup_provider"` need not correspond to any provider object at all. `validate_special` merely re-requires the five booleans the driver invented (validate-canary-scenario-receipt.py:121-130) and `corroborate_scenario` has no contract for this scenario (go-closure-canary-rehearsal.sh:272-277).

Failure: Point `scripts/backup.sh` at any adapter whose stdout is `{"status":"PASS"}` (which is exactly the shape `MERC_BACKUP_RESULT_FILE` already writes at scripts/backup.sh:264) and delete the offsite bucket contents afterwards. The gate at line 1015 passes, and the driver emits a PASS receipt asserting `isolated_restore: true` and `postgres_semantic_checks: true` for a restore that was never attempted against a bundle that no longer exists.

Suggested fix: Actually invoke `scripts/restore.sh` against an independently downloaded ciphertext into a throwaway Postgres, and derive each of the six booleans from that run's observed output. Take the subject from the provider object (the offsite URI / manifest ciphertext_sha256) and fail closed instead of falling back to hashing the driver's own temp file.

## [CRITICAL] canary-scenario-driver.sh:1015  (found by 4 lenses)
**backup_independent_restore parses backup.sh's human log stdout as JSON; the PASS probe can never succeed**

Defect: The driver captures `scripts/backup.sh` stdout into `$drill_out` (driver:983-984) and then requires `jq -e '(.status == "PASS") or (.integrity.ciphertext_verified == true) or (.ok == true)' "$drill_out"` to succeed. backup.sh writes no JSON to stdout: `log() { echo "[backup] $*"; }` (backup.sh:39) is the only stdout writer, and all of its JSON receipts go to files — `$STAGE/manifest.json` (backup.sh:175), `$STAGE/verification.json` (backup.sh:228), and `$RESULT_FILE` (backup.sh:271, and the driver never sets MERC_BACKUP_RESULT_FILE). jq therefore hits a parse error on `[backup] ...`. The error is swallowed by `2>&1` on driver:1016 so the diagnostic blames the backup adapter. The same mismatch silently degrades the subject: `jq -r '.ciphertext_sha256 ...'` at driver:991 also fails, so driver:994 falls back to `sha256_file "$drill_out"` — the subject_id becomes a hash of log text with no tie to any offsite object.

Failure: Operator configures MERC_BACKUP_OFFSITE + AWS creds + age recipient; backup.sh runs the full path (encrypt, upload, independent re-download, ciphertext and manifest checksum match, verification receipt uploaded and validated at backup.sh:186-240) and exits 0. The driver still dies: `backup_independent_restore: backup adapter did not report a verified PASS/ciphertext proof`. Verified locally — the jq probe on backup.sh-shaped stdout exits 5 (parse error). This is scenario 10 of 14, so the rehearsal aborts even after a fully successful offsite backup.

Suggested fix: Invoke backup.sh with MERC_BACKUP_RESULT_FILE pointing at a mktemp path and probe that file (which is the schema'd `merc_backup_invocation_result` with `status:"PASS"`), rather than probing stdout.

## [CRITICAL] canary-scenario-driver.sh:1299  (found by 1 lens)
**bounded_retry_backoff_audit emits a fabricated Prometheus observation and a fabricated backoff verdict**

Defect: The receipt claims `backoff_schedule_within_policy:true` (line 1301) and `max_attempts_within_policy:true` (line 1300) as jq literals. The driver never observes a backoff schedule anywhere — it only reads `max(retry_count)` and `count(retry_count>3)` from PostgreSQL (lines 1272-1283). The evidence entry declares `source:"merc_prometheus"` (line 1296) while the only Prometheus interaction is a `/-/ready` liveness ping whose failure is tolerated and merely logged (lines 1287-1291) — zero PromQL is ever issued. The subject_id `retry-backoff-${MERC_CANARY_RUN_ID:0:16}` (line 1295) is derived purely from the driver's own run id, so no row in any store corresponds to it, and `corroborate_scenario` explicitly has no contract for this scenario (go-closure-canary-rehearsal.sh:272-277). `validate_special` only re-asserts the same three booleans the driver hardcoded (validate-canary-scenario-receipt.py:145-148), so nothing downstream can catch it.

Failure: Stop Prometheus entirely, then invoke `driver run bounded_retry_backoff_audit 1`. The `curl --fail .../-/ready` at line 1287 fails, the else branch logs and continues, and the driver emits a PASS receipt whose single evidence entry claims `source: merc_prometheus`, subject `retry-backoff-<runid>`, plus `backoff_schedule_within_policy: true`. Additionally, on any database where no task in the run window was ever retried, `max(retry_count)` is 0, so `max_attempts_within_policy:true` is proven by the absence of work rather than by bounded retries.

Suggested fix: Query Prometheus for the actual retry/backoff series (or drop the `merc_prometheus` source claim and use a `merc_postgres.*` source), derive `backoff_schedule_within_policy` from observed `visible_at` deltas between consecutive `task_requeued` events, require at least one retried task in the window, and give the evidence entry a subject the control plane can find (e.g. the task or event id the backoff was measured on).

## [CRITICAL] canary-scenario-driver.sh:1301  (found by 2 lenses)
**bounded_retry_backoff_audit claims backoff_schedule_within_policy and merc_prometheus provenance without querying Prometheus**

Defect: The scenario measures only `max(retry_count)` and `count(*) WHERE retry_count > 3` in PostgreSQL (driver:1272-1283). It never observes a single requeue delay, yet the extras assert `backoff_schedule_within_policy:true` (driver:1301) against the stated policy `requeueBackoffCap=10m` (driver:1271). The only Prometheus interaction is `curl .../-/ready` whose result merely selects between two log lines (driver:1287-1291) — failure is not fatal. Despite that, the single evidence entry is stamped `source:"merc_prometheus"` (driver:1296), and validate-canary-scenario-receipt.py:34 requires exactly that source, so the contract certifies a provenance the driver did not use. corroborate_scenario skips this scenario (go-closure-canary-rehearsal.sh:272-277).

Failure: Prometheus is stopped entirely before the rehearsal. The driver logs "Prometheus not reachable at http://127.0.0.1:9090; using PostgreSQL retry_count policy check only" and still emits a PASS receipt whose sole evidence entry declares `source: merc_prometheus` and whose extras declare the backoff schedule within policy. Separately, if no tasks were created since MERC_CANARY_RUN_STARTED_AT the query returns `coalesce(max(retry_count),0)=0`, so the audit passes over an empty set.

Suggested fix: Set `source` to `merc_postgres.*` (and update EXPECTED_SOURCES), drop `backoff_schedule_within_policy` unless an actual requeue-delay observation is made, and make the audit fail closed when the task set since run start is empty.

## [HIGH] canary-scenario-driver.sh:138  (found by 1 lens)
**Buyer API keys, worker tokens and admin keys are passed to curl as argv, exposing them in the process table for the whole run**

Defect: http_code_body execs curl with the caller's extra arguments spliced in verbatim: `code="$("${CURL_COMMON[@]}" -X "$method" -o "$tmp" -w '%{http_code}' "$url" "$@")"` (scripts/canary-scenario-driver.sh:138). Every credential-bearing call passes the secret as an `-H` argument: `-H "$auth"` where auth is `Authorization: Bearer <cx_test_… buyer API key>` (built at :217 primary_buyer_auth, :222 admin_auth, :538) at call sites :444-446, :463, :488, :539, :922-923; and `-H "X-Worker-Token: $token"` (a cxw_… worker token, control/enrollment.go:762, control/store_workers.go:50) at :705 and :837. Argument vectors are world-readable on Linux via /proc/<pid>/cmdline; the rehearsal's `umask 077` (scripts/go-closure-canary-rehearsal.sh:376) and the `chmod 600` on the receipt (:385) do nothing about this. The repository already establishes the correct pattern for exactly this problem one directory over: scripts/stripe-sandbox-scenarios.sh:70 feeds the Stripe secret through `printf 'user = "%s:"\n' "$STRIPE_SECRET_KEY" | curl --config -` specifically so the key never reaches argv. The driver did not follow it.

Failure: A full rehearsal runs embed_success with minimum=20: submit_job (:444) plus wait_job_status polling GET /v1/jobs/<id> every 2s for up to 600s per job (:462-480) forks a curl process carrying `-H "Authorization: Bearer cx_test_<primary buyer key>"` several thousand times, over roughly an hour across all scenarios; forced_retry and stale_attempt_commit_rejection additionally fork curl with `-H "X-Worker-Token: cxw_<token>"`. Any unprivileged local account on the staging droplet running `while :; do cat /proc/*/cmdline; done` - or the node_exporter/process-collector already in docker-compose.prod.yml, or any container sharing the host PID namespace - captures the live buyer API key and worker tokens. Those keys are not scoped to the canary: the buyer key can submit arbitrary jobs and spend the buyer's credit, and the worker token can fail and commit tasks (POST /v1/worker/task/<id>/fail and /commit) for that worker, which is precisely the authority the forced_retry and stale_attempt scenarios exercise.

Suggested fix: Have http_code_body accept the auth header out-of-band and hand it to curl via `--config`, mirroring scripts/stripe-sandbox-scenarios.sh:70 - e.g. `printf 'header = "%s"\n' "$auth" | curl --config - ...`, or write the header line to a mktemp 0600 file and pass `--config "$file"`. Keep only non-secret arguments (method, URL, Content-Type, Idempotency-Key) in argv.

## [HIGH] canary-scenario-driver.sh:157  (found by 1 lens)
**The PostgreSQL DSN, including POSTGRES_PASSWORD, is passed to psql as argv on every observation query**

Defect: db_scalar (scripts/canary-scenario-driver.sh:155-158) and db_tsv (:160-163) both invoke `psql "$DATABASE_URL_OBS" -X -qAt ...` with the full connection URI as argv[1]. DATABASE_URL_OBS comes from resolve_database_url (:92-106), which prefers MERC_CANARY_DATABASE_URL and falls back to DATABASE_URL - and DATABASE_URL for this deployment is `postgres://cx:${POSTGRES_PASSWORD}@postgres:5432/cx?sslmode=disable` (docker-compose.prod.yml:8). POSTGRES_PASSWORD is a first-class secret the project validates as at least 32 URL-safe characters (scripts/lib/go-closure-common.sh:181-182). As in the curl case, argv is world-readable via /proc/<pid>/cmdline on Linux. Note the contrast with the caller: the rehearsal's own corroboration deliberately avoids this, running `gc_compose exec -T postgres psql -U cx -d cx` with no password on the command line (scripts/go-closure-canary-rehearsal.sh:32-35). The same pattern is repeated in the test harness (scripts/test-canary-scenario-driver.sh:125, :134).

Failure: During scenario_job_success with minimum=20 the driver calls db_scalar once per completed job (:632-636), and wait_task_running busy-polls db_scalar every 50ms for up to 180s per task in forced_retry, stale_lease_recovery and stale_attempt_commit_rejection (:500-521) - tens of thousands of psql processes across a run, each with `postgres://cx:<32+ char POSTGRES_PASSWORD>@postgres:5432/cx?sslmode=disable` in its command line. An unprivileged local account (or any host-PID-namespace container, or a process-listing monitoring agent) sampling /proc/*/cmdline during the rehearsal recovers the database superuser password for the control plane, granting direct write access to jobs, tasks, ledger_entries and job_events - i.e. the exact tables the rehearsal's independent corroboration (scripts/go-closure-canary-rehearsal.sh:54-280) relies on to be trustworthy, so the leak also destroys the corroboration's value as evidence.

Suggested fix: Split the DSN once at resolve_database_url and pass the password out-of-band: export PGPASSWORD (or write a 0600 ~/.pgpass / PGSERVICEFILE entry) and call psql with only host/port/db/user flags, so no secret appears in argv. Same change in scripts/test-canary-scenario-driver.sh:125 and :134.

## [HIGH] go-closure-canary-rehearsal.sh:255  (found by 1 lens)
**corroborate_scenario's stale-attempt loop is truncated to one iteration because docker compose exec drains the loop's stdin**

Defect: The `while IFS=$'\t' read -r subject current; ... done < <(jq -r ... "$file")` loop (lines 254-269) calls `gc_canary_db_scalar`, which is `gc_compose exec -T postgres psql ...` (line 34). `docker compose exec` attaches stdin unconditionally — `-T` only disables TTY allocation, and Compose v2 offers no flag to detach stdin (verified against `docker compose exec --help`). The docker CLI therefore reads and discards the remaining lines of the process-substitution fd on the first iteration.

Failure: stale_attempt_commit_rejection runs with minimum=3 and the driver correctly produces three fenced task subjects. On the first loop iteration `docker compose exec -T` consumes the other two TSV lines; `read` then sees EOF and the loop ends with observed=1. require_observed_count gc_dies: 'stale_attempt_commit_rejection has 1 independently corroborated observation(s); require exactly 3'. The rehearsal aborts on scenario 8 of 14 even though all three rejections are durably fenced in PostgreSQL. I reproduced the mechanism with a stdin-reading stand-in: observed=1 instead of 3.

Suggested fix: Redirect stdin away from the subprocess inside the loop — `current_count="$(gc_canary_db_scalar "..." </dev/null)"` — or read the TSV on a dedicated descriptor (`done < <(...) 3<&0` / `while read -u 3`), or collect the jq output into an array first and iterate that.

## [HIGH] canary-scenario-driver.sh:315  (found by 1 lens)
**The secret-shaped-value guard recognizes no credential this system actually mints, and never runs on stderr**

Defect: The guard behind `secret_values_recorded: false` (scripts/canary-scenario-driver.sh:368, enforced at :390 and re-checked at scripts/validate-canary-scenario-receipt.py:257) is a six-prefix denylist: `sk_(test|live)_`, `rk_(test|live)_`, `pk_live_`, `whsec_`, `AGE-SECRET-KEY-`, `AKIA[0-9A-Z]{12,}` (scripts/canary-scenario-driver.sh:315-319, mirrored at scripts/validate-canary-scenario-receipt.py:58-62). Every one of those is a Stripe/age/AWS-ID shape. It matches none of the credentials this driver actually handles: buyer and admin API keys are `cx_test_…` / `cx_live_…` (control/store.go:273-276, control/api.go:3625-3627), worker tokens are `cxw_…` (control/enrollment.go:762, control/store_workers.go:50). It also misses the AWS *secret* access key (AKIA matches only the non-secret access key ID; the 40-char secret has no prefix), credential-bearing DSNs such as `postgres://cx:<POSTGRES_PASSWORD>@postgres:5432/cx`, MERC_TOKEN_KEY and MERC_VERIFICATION_SAMPLE_SECRET (opaque 32+ byte blobs, scripts/lib/go-closure-common.sh:176-180), MINIO_ROOT_PASSWORD, and Slack/PagerDuty receiver credentials. Separately, the guard runs only inside emit_receipt on the assembled receipt - it never touches the other output channel. die() writes straight to stderr with no filtering (:17-20), and five call sites interpolate text the driver did not author: 400 bytes of the Stripe adapter's stderr (:1056), 400 bytes of scripts/backup.sh's stderr (:985), and 200-300 bytes of raw control-plane response bodies (:451, :713, :845). The rehearsal does not capture or redact that stream - it inherits the rehearsal's stderr (scripts/go-closure-canary-rehearsal.sh:379 redirects stdout only), so it lands in the operator terminal and CI log verbatim.

Failure: ALERT_RECEIVER_NAME is copied verbatim into the receipt as `receiver_name` (scripts/canary-scenario-driver.sh:1184-1185) and the sink-file-derived `firing`/`resolved` values become `receiver_event_ids` and the evidence subject_id `alert-${firing_id}` (:1180). Alertmanager receivers are routinely named after their target, e.g. ALERT_RECEIVER_NAME="pagerduty-R02ABCDEF0123456789ABCDEF01234567" or a Slack receiver named after its webhook path. That string satisfies SAFE_ID (scripts/validate-canary-scenario-receipt.py:49, which permits `/`, `:`, `@`, `.`, `-`), is not matched by SECRET at :315 or :58, passes the receiver_name equality check at scripts/go-closure-canary-rehearsal.sh:27, and is then embedded into the archived PASS receipt via `--slurpfile receipts` (scripts/go-closure-canary-rehearsal.sh:420, 435) - a file the rehearsal itself only chmods 600 per-scenario and that is intended to be circulated as release evidence. The receipt simultaneously asserts secret_values_recorded:false. The same blindness means that if a `cx_test_…` key or `cxw_…` token ever reaches a receipt field or a die() string - e.g. via a control-plane 4xx body echoed at :451/:713/:845 - neither the driver nor the validator would stop it.

Suggested fix: Extend SECRET at scripts/canary-scenario-driver.sh:315 and scripts/validate-canary-scenario-receipt.py:58 to the credential classes this platform mints and consumes: `cx_(test|live)_`, `cxw_`, a DSN pattern `[a-z][a-z0-9+.-]*://[^\s:/@]+:[^\s/@]+@`, and a generic high-entropy AWS-secret shape - and prefer an allowlist for receipt fields that copy external strings (`receiver_name`, `receiver_event_ids`) rather than a denylist. Make die() and log() (:17-24) run the same filter over their argument before writing to stderr, so the adapter/response-body passthroughs at :451, :713, :845, :985 and :1056 cannot emit an unredacted credential into the rehearsal log.

## [HIGH] canary-scenario-driver.sh:583  (found by 1 lens)
**distinct_metal_agent counts heartbeats since run start against a 30s heartbeat, so fewer than <minimum> candidate rows exist**

Defect: The candidate-row filter is `w.last_seen_at >= '${MERC_CANARY_RUN_STARTED_AT}'::timestamptz` (line 583) evaluated by a single-shot query, and the result must equal `minimum` exactly (line 598-599). MERC_CANARY_RUN_STARTED_AT is stamped once for the whole run (go-closure-canary-rehearsal.sh:339), and distinct_metal_agent is scenario 2 of 14, running only a few seconds later. But `workers.last_seen_at` is advanced only by HeartbeatTx (control/store_workers.go:192-210), driven by the agent's 30-second heartbeat loop (agent/src/main.rs:1976). So a worker's last_seen_at is uniformly distributed up to 30s in the past relative to run start, and the query almost never sees `minimum` rows. The sibling liveness helper `first_live_approved_worker` gets this right with `now() - interval '5 minutes'` (line 268); this scenario does not. The same too-tight bar is repeated in the rehearsal's corroboration (go-closure-canary-rehearsal.sh:95), so simply widening the driver's window is not sufficient — the run binding has to be satisfied by an actually-observed heartbeat.

Failure: Run starts at T0. approved_buyer_identity (2 HTTP probes + 2 DB reads) finishes ~3s later, so distinct_metal_agent's query runs at ~T0+3s with minimum=2. Worker A last heartbeated at T0-5s, worker B at T0-20s; neither has heartbeated since T0. The SQL at lines 578-588 returns 0 rows, n=0, and line 598 dies with "distinct_metal_agent observed 0 live approved Metal agents ... require exactly 2". The rehearsal aborts at scenario 2 of 14 (go-closure-canary-rehearsal.sh:379-382) and no CANARY_PROVEN authority is ever minted. Probability both approved workers heartbeat inside the ~3s window is roughly (3/30)^2 ~= 1%, so this fails ~99 runs in 100.

Suggested fix: Poll instead of single-shot: loop for up to ~2x the heartbeat interval (say 90s) re-running the query until exactly `minimum` approved workers satisfy `last_seen_at >= MERC_CANARY_RUN_STARTED_AT`, then die if the deadline passes. That keeps the run binding (and keeps rehearsal:95 satisfiable) while tolerating the 30s cadence.

## [HIGH] canary-scenario-driver.sh:593  (found by 1 lens)
**distinct_metal_agent stamps occurred_at from the worker's past heartbeat, so its receipt is always rejected**

Defect: Every other scenario sets `occurred="$(utc_now)"` immediately before appending evidence (see the explicit comment at driver:640 "Use finished-window occurrence (now), still inside scenario window"). scenario_distinct_metal_agent instead carries the SQL value `to_char(w.last_seen_at ...)` (driver:579) straight into `--arg at "$occurred"` (driver:593). `started_at` is `utc_now()` taken at driver:570 and `finished_at` at driver:609 — the only work between them is one psql round trip, so the receipt window is ~0-1s wide. `last_seen_at` is by construction in the past. validate-canary-scenario-receipt.py:240-241 rejects any `occurred_at < started_at`. The driver exits 0 and emits a full receipt on stdout, so neither the driver nor the rehearsal's `rm -f -- "$receipt"` guard (go-closure-canary-rehearsal.sh:380) applies; the failure only surfaces at validate_scenario_receipt.

Failure: Agents heartbeat every 30s (agent/src/main.rs:1976). Rehearsal starts at 12:00:00Z; scenario 2 runs at 12:00:30Z; both approved workers last heartbeat at 12:00:18Z and 12:00:22Z. The driver emits a valid-looking PASS receipt with started_at=finished_at=12:00:30Z and occurred_at=12:00:18Z/12:00:22Z, exit 0. go-closure-canary-rehearsal.sh:387 then gc_dies with "distinct_metal_agent driver receipt failed exact-run validation". I reproduced this against the real validator: `canary-scenario-receipt: FAIL: evidence[0] occurred outside the scenario window`, exit 1. Only a worker whose heartbeat lands inside that ~1s window passes, i.e. ~(1/30)^2 of runs, so the rehearsal aborts at scenario 2 of 14 essentially always — CANARY_PROVEN can never be minted.

Suggested fix: Set `occurred="$(utc_now)"` after the row is read, as every other scenario does, and keep `last_seen_at` (if wanted) as a separate non-`occurred_at` field — but note the validator enforces a closed evidence key set, so it must simply use utc_now.

## [HIGH] canary-scenario-driver.sh:775  (found by 1 lens)
**stale_lease_recovery accepts job_stuck_rescued, which fires for stalled jobs with no dead worker**

Defect: The rescue-event query accepts `e.event IN ('task_rescued_dead_worker','job_stuck_rescued')` (line 775). `reapStuckJobs` emits `job_stuck_rescued` for any job past its stuck deadline, with `cause` set to "the workload made no progress" when `j.DeadClaim` is false (control/workers.go:770-783) — i.e. no lease ever went stale and no worker ever stopped heartbeating. The driver's own comment at lines 754-757 says the operator must stop the worker heartbeating, but the accepted event set makes that unnecessary. `corroborate_scenario` copies the same permissive event set (go-closure-canary-rehearsal.sh:223), so the rehearsal cannot catch it either.

Failure: Submit the 24-chunk embed job the scenario already generates (lines 420-431) against an agent that is slow but healthy and keeps heartbeating. Once the job exceeds `stuckEtaFactor`×ETA past `stuckProgressGrace`, `reapStuckJobs` inserts `job_stuck_rescued` with `DeadClaim=false`. The driver's poll at lines 771-780 picks up that event id, records it as `source: merc_postgres.job_events`, and emits a PASS receipt for stale_lease_recovery — proving nothing about dead-claim lease recovery, the deadWorkerAfter path the scenario exists to exercise.

Suggested fix: Restrict the query to `event='task_rescued_dead_worker'` (control/workers.go:868, the only dead-claim path), or additionally require the rescued task's prior `claimed_by` to match the worker whose `last_seen_at` fell more than deadWorkerAfter behind. Apply the same restriction in `corroborate_scenario`.

## [HIGH] canary-scenario-driver.sh:1234  (found by 4 lenses)
**post_rehearsal_invariant_audit reports two invariants it never evaluates**

Defect: `'unreconciled_state', false` (line 1234) is a hardcoded SQL literal — no reconciliation state is ever queried, yet the receipt publishes it in the `invariants` map (line 1260) and `validate_special` requires exactly `unreconciled_state: False` (validate-canary-scenario-receipt.py:131-144), so the fabricated value is what makes validation pass. `stuck_payouts` (lines 1228-1233) is unsatisfiable by construction: it requires `release_at < now() - interval '30 days'` AND `created_at >= MERC_CANARY_RUN_STARTED_AT`, so a row must have been created minutes ago yet have a release date over a month in the past. Both booleans are therefore constants. The evidence subject `invariant-audit-${MERC_CANARY_RUN_ID:0:16}` (line 1256) is minted from the run id and its declared `source: merc_postgres.invariant_audit` names a table that does not exist anywhere in the schema; `corroborate_scenario` skips this scenario (go-closure-canary-rehearsal.sh:272-277).

Failure: Introduce a real reconciliation drift (a ledger entry with no matching Stripe object, or a Stripe balance transaction with no ledger row) and any supplier_credit held past its 30-day release window. Run `driver run post_rehearsal_invariant_audit 1`: the audit returns `unreconciled_state:false` (never queried) and `stuck_payouts:false` (query cannot be true), all nine invariants read clean, and a PASS receipt is emitted attesting a clean post-rehearsal invariant sweep over a control plane that is not reconciled.

Suggested fix: Either implement a real reconciliation query for `unreconciled_state` and drop the `created_at >= run_start` predicate from `stuck_payouts` (window the check on `release_at` instead), or remove both keys from the published map and from `validate_special` so the receipt does not claim invariants that were not checked.

## [HIGH] canary-scenario-driver.sh:1294  (found by 1 lens)
**bounded_retry_backoff_audit claims source merc_prometheus and backoff_schedule_within_policy without reading Prometheus or measuring any backoff**

Defect: The scenario only queries PostgreSQL for max(retry_count) and a retry_count>3 count (lines 1272-1283). Prometheus is contacted solely as a /-/ready liveness probe whose result is logged and discarded (lines 1287-1291) - the else branch even logs 'Prometheus not reachable ... using PostgreSQL retry_count policy check only'. The evidence entry at lines 1294-1297 nevertheless declares source:"merc_prometheus", which is exactly what EXPECTED_SOURCES demands (validate-canary-scenario-receipt.py:34), and the extras at lines 1299-1302 hardcode backoff_schedule_within_policy:true, which validate_special requires to be true (lines 145-148). No code anywhere samples requeue intervals or the requeueBackoffCap=10m policy. corroborate_scenario skips this scenario (go-closure-canary-rehearsal.sh:272-277).

Failure: Prometheus is down (or MERC_PROMETHEUS_URL points at nothing) and a regression sets the requeue backoff to a fixed 1s, violating the documented exponential schedule. The driver logs 'Prometheus not reachable', finds max(retry_count)=1 in PostgreSQL, and emits a receipt whose single observation claims source merc_prometheus and backoff_schedule_within_policy:true. The validator PASSes it (verified) and the rehearsal does not corroborate it, so a broken backoff schedule ships with a receipt asserting a Prometheus-observed compliant one.

Suggested fix: Either query Prometheus for the retry/backoff series and derive the booleans from it (failing closed when unreachable), or change the evidence source and the validator's EXPECTED_SOURCES entry to merc_postgres.tasks and delete the unmeasured backoff_schedule_within_policy claim.

## [MEDIUM] canary-scenario-driver.sh:464  (found by 1 lens)
**pipefail plus SIGPIPE on `printf | head -n1` aborts the driver whenever an HTTP body exceeds the pipe buffer**

Defect: `set -euo pipefail` (line 13) is combined with `code="$(printf '%s\n' "$response" | head -n1)"`, used at lines 447, 464, 489, 540, 709, 841, 869, 924, 1103 and 1130. `head -n1` closes the pipe after the first line; if `$response` is larger than the pipe buffer (16 KiB on macOS, 64 KiB on Linux) the forked subshell running the `printf` builtin is killed by SIGPIPE, pipefail propagates 141, and because these are plain assignments `set -e` terminates the driver.

Failure: An embed job is submitted with `--argjson n 24` (line 424), so `GET /v1/jobs/$job_id` returns per-task detail. Once that body crosses ~16 KiB, wait_job_status line 464 kills the driver: exit 141, empty stdout, no diagnostic from `die`. The rehearsal reports only 'scenario driver failed for embed_success' at go-closure-canary-rehearsal.sh:381, pointing the operator at the control plane rather than at the parser. The same happens on any oversized error body — a reverse-proxy HTML 502 page — where the intended behaviour was `die "job submit failed HTTP 502: <first 300 bytes>"` (line 451). I reproduced it: a 200 KB response gives rc=141 and the following line never executes.

Suggested fix: Split without a pipe: `code="${response%%$'\n'*}"` and `body_out="${response#*$'\n'}"`. Alternatively have http_code_body return the code and body via two files or a delimiter that avoids re-parsing a large string through `head`.

## [MEDIUM] canary-scenario-driver.sh:561  (found by 1 lens)
**emit_receipt never checks subject-ID uniqueness, and approved_buyer_identity counts allowlist entries instead of distinct buyers**

Defect: emit_receipt is the single choke point through which all 14 scenarios mint receipts. Its python block (lines 377-391) enforces `len(evidence) == minimum` and per-entry UUID/SAFE_ID shape, but never checks that subject_id values are distinct — so threat-model property 8 is enforced nowhere inside the driver that claims to enforce it. That becomes reachable because scenario_approved_buyer_identity increments `n` once per entry of MERC_CANARY_APPROVED_BUYER_EMAILS (loop at lines 535-559, guard at line 561) rather than once per distinct buyer row, and approved_buyer_emails (lines 165-170) lowercases but does not deduplicate.

Failure: Operator sets MERC_CANARY_APPROVED_BUYER_EMAILS="canary-one@example.invalid,Canary-One@example.invalid" and MERC_CANARY_BUYER_API_KEYS="canary-one@example.invalid=K" (map mode, line 189-200, matches both after lowercasing). Both loop iterations probe /v1/me with the same key, both get the same buyer_id, both pass the me_email check (line 545) and the durability check (line 548-552), n reaches 2, line 561 passes, and emit_receipt prints a schema-v2 receipt with observed:2 and two evidence entries carrying the identical buyer UUID — one real subject presented as two. It is caught downstream by validate-canary-scenario-receipt.py:234 and by the rehearsal's own jq check (go-closure-canary-rehearsal.sh:70-75), so no CANARY_PROVEN is minted, but the driver's minting gate itself is blind to it.

Suggested fix: One line in emit_receipt's python, after the evidence loop: `if len({i.get("subject_id") for i in evidence}) != len(evidence): raise SystemExit("duplicate subject_id in evidence")`. Same for `id`.

## [MEDIUM] canary-scenario-driver.sh:859  (found by 2 lenses)
**stale_attempt before/after state hash straddles a window in which the requeued task is immediately re-claimable**

Defect: `before` is snapshotted at driver:859 and `after` at driver:873, with an HTTP commit POST in between; driver:874-875 dies if they differ, attributing any change to the rejected stale commit. But the preceding forced failure returns FailRequeued, documented in control/failure.go:63 as "retryable, under the retry cap -> claimable again now" — the 1-minute `staleBackoff` (control/workers.go:60) applies to the stale-reaper path, not the worker-fail path. So the task is claimable the moment retry_count advances, and the driver's own comment at driver:519-520 notes "sub-second agent completion is common on local Metal". The recorded `before_state_sha256`/`after_state_sha256` evidence therefore does not isolate the stale commit's effect either; it only proves nothing changed during one particular ~100ms window.

Failure: The requeue-detection loop (driver:851-855, `sleep 0.2`) observes retry_count advance; the live Metal agent has already re-claimed the task. `before` captures status='running'. During the stale commit POST the agent commits the current attempt: status becomes 'complete', result_sha256 populates, and three ledger_entries rows appear. `after` differs, and the driver dies with "stale_attempt: task/money state changed after rejected stale commit on <task>" even though the control plane correctly returned 409 and the stale commit changed nothing. Scenario 8 of 14 aborts the rehearsal on a race, with a diagnostic that points at a money-state violation that did not occur.

Suggested fix: Snapshot state keyed to the stale attempt only (e.g. filter ledger_entries and result fields by the submitted attempt), or hold the task out of the claim pool for the duration of the probe, rather than hashing whole-task state across a window the agent can mutate.

## [MEDIUM] canary-scenario-driver.sh:1061  (found by 1 lens)
**stripe_test_matrix mints its provider subject from the run id when the matrix script reports none**

Defect: `subject="stripe-matrix-${subject:-$MERC_CANARY_RUN_ID}"` (lines 1061-1062) falls back to the driver's own run id when `stripe-sandbox-scenarios.sh` emits no `.run_id`, so the evidence entry declared as `source:"stripe_test_api"` need not reference any Stripe object — it is guaranteed constructible with no provider interaction at all. Separately, `matrix_complete:true` (line 1071) is a jq literal: the gate at line 1058 checks only `.status`, `.provider_mode` and `.webhook.application_outcomes_verified`, never a completeness field. `validate_special` re-requires exactly the booleans the driver invented (validate-canary-scenario-receipt.py:108-113) and `corroborate_scenario` skips the scenario (go-closure-canary-rehearsal.sh:272-277).

Failure: Run the matrix script in a build where it emits `{"status":"PASS","provider_mode":"test","webhook":{"application_outcomes_verified":true}}` but has silently stopped exercising the refund and dispute legs (no `run_id`, no per-case output). The gate at line 1058 passes, the subject becomes `stripe-matrix-<MERC_CANARY_RUN_ID>` — a string the driver already held in memory — and a PASS receipt asserts `matrix_complete: true` for a partial matrix, with an evidence subject no Stripe API lookup can resolve.

Suggested fix: Fail closed when `.run_id` is absent, use a real test-mode provider object id (e.g. the `pi_`/`ch_`/`tr_` fixture the script exercised) as the subject, and derive `matrix_complete` from an explicit per-case completeness field in the matrix script's output.

## [MEDIUM] canary-scenario-driver.sh:1139  (found by 1 lens)
**real_alert_firing_resolution evidence is read verbatim from an unauthenticated operator-writable file**

Defect: After posting the fire/resolve pair to Alertmanager, the receipt's only evidence — subject `alert-${firing_id}` (line 1180) declared as `source:"alert_receiver_api"`, plus `receiver_event_ids.firing`/`.resolved` (lines 1184-1185) — is parsed out of `MERC_CANARY_ALERT_SINK_FILE`, a plain JSONL path supplied by the environment (lines 1111-1173). The driver never contacts `ALERT_RECEIVER_WEBHOOK_URL` to confirm the deliveries, and `corroborate_scenario` has no contract for this scenario (go-closure-canary-rehearsal.sh:272-277). The only structural gate is `validate_special`'s requirement that the two ids be SAFE_ID-shaped and distinct (validate-canary-scenario-receipt.py:114-120), and the rehearsal's extra check only compares `receiver_name` to `$ALERT_RECEIVER_NAME` (go-closure-canary-rehearsal.sh:26-29).

Failure: With the alert receiver disconnected (or its webhook silently dropping), write two lines into the sink file — `{"alertname":"MercCanaryDriverProbe_<runid12>","status":"firing","event_id":"evt-canary-000001"}` and the same with `"status":"resolved","event_id":"evt-canary-000002"`. Alertmanager accepts both POSTs (lines 1101, 1128), the parser at lines 1139-1167 returns the two ids, and a PASS receipt is minted attesting a real firing-and-resolution round trip through a receiver that delivered nothing.

Suggested fix: Query the receiver's own API for the two event ids (the `alert_receiver_api` the source field already claims) and require the returned records to carry the run-scoped alertname, rather than trusting a local file the driver cannot authenticate.

## [MEDIUM] canary-scenario-driver.sh:1175  (found by 1 lens)
**real_alert_firing_resolution never checks receiver event IDs against the validator's SAFE_ID length bound**

Defect: Line 1175 accepts any non-empty, distinct pair of firing/resolved IDs, and lines 1184-1185 put them into receiver_event_ids verbatim. emit_receipt only SAFE_ID-checks subject_id (driver lines 388-389), and the subject is the padded 'alert-'+firing_id, so a short ID slips through the driver. validate-canary-scenario-receipt.py:117-118 applies SAFE_ID (minimum 8 characters) to receiver_event_ids.firing and .resolved themselves.

Failure: The operator's JSONL sink at MERC_CANARY_ALERT_SINK_FILE records {"alertname":"MercCanaryDriverProbe_...","status":"firing","event_id":"evt-001"} and a matching resolved line with "evt-002". The driver fires and resolves the alert through Alertmanager, observes both IDs, passes its own distinctness check, builds subject_id 'alert-evt-001' (13 chars, SAFE_ID-clean) and exits 0 with a receipt. validate_scenario_receipt then fails with 'alert receipt requires safe firing and resolved receiver event IDs' (reproduced), and the rehearsal gc_dies at scenario 12 of 14 after all prior scenario work is spent.

Suggested fix: Apply the validator's SAFE_ID regex to firing_id and resolved_id at line 1175 and die with a clear diagnostic before doing the alert work, or normalise short receiver IDs into a schema-safe form used consistently in both the subject and the extras.

## [LOW] canary-scenario-driver.sh:65  (found by 1 lens)
**MERC_CANARY_RUN_STARTED_AT is never format-validated yet is interpolated raw into every freshness predicate**

Defect: require_binding_env validates the shape of RUN_ID, CANDIDATE_COMMIT, DRIVER_SHA256 and CONTROL_IMAGE (lines 70-77) but checks MERC_CANARY_RUN_STARTED_AT only for non-emptiness (line 65). It is then spliced unquoted into SQL string literals at lines 583, 725, 776, 1205, 1210, 1215, 1220, 1226, 1232, 1238, 1242, 1274 and 1281, where it is the sole guarantee that observations are fresh to this run.

Failure: Invoke the driver directly (as scripts/test-canary-scenario-driver.sh:186 does) with MERC_CANARY_RUN_STARTED_AT='-infinity'. That is a valid PostgreSQL timestamptz, so `w.last_seen_at >= '-infinity'::timestamptz` (line 583) is always true and distinct_metal_agent mints a receipt listing approved workers whose last heartbeat was weeks ago — the freshness gate the scenario exists to prove is silently disabled. A value such as `2020-01-01'::timestamptz OR true OR '` closes the literal outright and makes the predicate unconditionally true. The rehearsal path happens to be safe only because it generates the value itself at go-closure-canary-rehearsal.sh:339 and passes the same string to the validator, whose parse_utc would reject it.

Suggested fix: Add `[[ "$MERC_CANARY_RUN_STARTED_AT" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || die` alongside the other binding checks at lines 70-77, matching the strictness already applied to the other five bound values.

## [LOW] go-closure-canary-rehearsal.sh:268  (found by 1 lens)
**stale_attempt_commit_rejection corroboration counts evidence rows, not distinct database rows**

Defect: Every other corroboration branch computes `count(*) ... WHERE id=ANY('$ids'::uuid[])` against a primary key, which is inherently duplicate-immune: a repeated subject in the array still counts one row, so a padded receipt corroborates low and fails. The stale_attempt_commit_rejection branch (lines 253-270) instead iterates the evidence array and does `observed=$((observed + 1))` once per evidence entry, checking each subject independently. Duplicate subjects therefore each count. In the main rehearsal path validate_scenario_receipt runs first and rejects duplicates, but the corroboration self-test entry point at lines 282-295 calls corroborate_scenario directly and never invokes the validator.

Failure: With MERC_CANARY_CORROBORATION_SELF_TEST=1, feed a receipt whose three evidence entries all carry the same subject_id T and the same current_attempt 1, where task T genuinely has retry_count=1 and passes the buyer/worker predicates. The loop at line 254 runs three times, each query returns current_count=1, observed reaches 3, and require_observed_count passes — the self-test reports three independently corroborated stale-attempt rejections for a single task. Run the identical duplicated-subject receipt through any other scenario branch (e.g. cancelled_job) and count(*) returns 1 against minimum 3 and it correctly dies, which is the asymmetry.

Suggested fix: Deduplicate before counting, mirroring the other branches: derive the pairs with `jq -r '[.evidence[]|[.subject_id,.current_attempt]]|unique|.[]|@tsv'`, or assert `[ "$(jq '[.evidence[].subject_id]|unique|length' "$file")" = "$minimum" ]` before the loop.

## [LOW] canary-scenario-driver.sh:472  (found by 1 lens)
**wait_job_status's terminal-status guard omits 'complete', so cancel-race jobs burn the full timeout and report a misleading reason**

Defect: The short-circuit at lines 472-478 only treats `failed|cancelled` as terminal. `complete` is a terminal status too, but is not listed, so the poll loop keeps re-fetching a job that can never change status again until the deadline expires.

Failure: scenario_cancelled_job submits an embed job and immediately DELETEs it (lines 662-664). On a fast local Metal agent the job completes before the DELETE lands; the control plane returns 409, which cancel_job explicitly accepts (line 491). wait_job_status is then called with want=cancelled and timeout=120 against a job permanently at status='complete': it polls for the full 120 s and dies with 'job <id> did not reach status cancelled within 120s' instead of the true cause. With minimum=5 in the rehearsal (go-closure-canary-rehearsal.sh:361) that is up to 10 minutes of dead polling and a diagnostic that sends the operator hunting a cancellation bug that does not exist.

Suggested fix: Add `complete` to the terminal case at line 473 so a job that reached a different terminal state dies immediately with the observed status named.

## [LOW] canary-scenario-driver.sh:520  (found by 1 lens)
**wait_task_running opens two psql connections per 50ms poll for up to 180s**

Defect: The busy-poll loop (driver:500-521) calls db_scalar twice per iteration — the task query at driver:503 and the job-status query at driver:513 — and each db_scalar forks a fresh `psql` process and a new PostgreSQL connection (driver:155-158). With `sleep 0.05` and a 180s timeout this is thousands of connection attempts per invocation, and wait_task_running is called 11 times per rehearsal (forced_retry x5, stale_lease_recovery x3, stale_attempt_commit_rejection x3).

Failure: PostgreSQL runs with the default max_connections=100 while the control plane holds its own pool. The poll loop's ~20-40 new connections/sec saturates the remaining slots; psql exits non-zero with "FATAL: sorry, too many clients already". Because the result is captured by assignment (`task_id="$(db_scalar ...)"`), `set -e` aborts the driver mid-scenario with a bare psql error on stderr, and the rehearsal gc_dies on a self-inflicted infrastructure artifact rather than a real canary failure. The same storm can transiently starve the control plane it is observing.

Suggested fix: Fold both polls into one psql invocation (LEFT JOIN jobs) and back the interval off after the first second, or hold a single psql session open for the poll loop.

## [LOW] canary-scenario-driver.sh:933  (found by 1 lens)
**wait_job_status's `|| true` in buyer_webhook_retry_sequence is dead; a failed job aborts the driver instead of proceeding**

Defect: `wait_job_status "$job_id" complete 600 "$auth" >/dev/null || true` is written to tolerate a job that does not complete, but wait_job_status reports failures via `die` (driver:466, 474-476, 481), and `die` calls `exit 1` (driver:17-20). `exit` is a shell builtin that terminates the shell regardless of the `||` list, so the `|| true` can never run. Every non-complete outcome hard-exits the driver mid-scenario.

Failure: During buyer_webhook_retry_sequence the embed job reaches status `failed` (agent crash, model load failure — exactly what a canary exercises). wait_job_status hits the `failed|cancelled` case at driver:472-478 and dies with "job <id> reached terminal status failed while waiting for complete". The driver exits 1 without ever checking whether the registered webhook retried and delivered on the terminal event, which is the property the scenario exists to prove, and the rehearsal gc_dies at scenario 9 of 14.

Suggested fix: Either drop the misleading `|| true` and document that completion is required, or call wait_job_status in a subshell (`( wait_job_status ... ) || true`) so the tolerance actually takes effect and the scenario proceeds to the webhook-delivery observation.

## [LOW] canary-scenario-driver.sh:1159  (found by 1 lens)
**Alertmanager fingerprint fallback yields identical firing and resolved receiver event IDs**

Defect: When a sink line carries Alertmanager's native webhook payload, the parser falls back to a.get("fingerprint") as the event ID for both the firing and the resolved branch (lines 1159-1161). Alertmanager's fingerprint is a hash of the alert's label set, which is identical for the firing and the resolved notification of the same alert, so the fallback can only ever produce firing_id == resolved_id. Both the driver (line 1175) and the validator (validate-canary-scenario-receipt.py:119-120) require them to differ.

Failure: The operator points MERC_CANARY_ALERT_SINK_FILE at the natural sink - a JSONL dump of the raw Alertmanager webhook POSTs, which have no event_id field. The probe alert fires and resolves correctly and both deliveries land in the sink, but the parser extracts the same fingerprint for both, the 120s loop at lines 1138-1172 never satisfies firing_id != resolved_id, and the driver dies with 'no observed distinct firing/resolved receiver event IDs' despite the alert pipeline working exactly as required.

Suggested fix: Derive distinct IDs when falling back, e.g. hash fingerprint together with the delivery status/timestamp, or require an explicit per-delivery event_id and drop the fingerprint fallback entirely so the failure message points at the sink format.

---

## Resolved: `fail outcome was 'noop'` is not a product defect

Raised earlier as a candidate control-plane bug: `forced_retry` and
`stale_attempt_commit_rejection` post a worker failure and the control plane
answers `noop` with HTTP 200, leaving `retry_count` at 0.

Measured, on the two tasks from the run that reported it:

```
042839a6-00a8-4454-b0b8-b3fbb73fce3b | complete | retry_count 0
41e61772-fcee-40e2-8612-3f119687bb04 | complete | retry_count 0
```

Both were already `complete`. `FailTaskTx` (`control/failure.go:108`) returns
`FailNoop` with a nil error - hence HTTP 200 - when a task's status is not
`running`, `queued` or `retrying`.

That is correct. Failing an already-settled task must be idempotent; treating a
late failure report as a requeue would let it undo work that was verified and
paid for.

The defect is in the scenario, not the platform: it needs to observe a task in a
failable state, and an M3 Ultra finishes the chunk first. The driver already
tries to widen the window by submitting 24 rows to force multiple chunks, which
is not enough on fast local hardware.

Fix direction, not yet implemented: have the driver claim the task itself with an
approved worker token so the real agent cannot take it, making the attempt
lifecycle deterministic instead of racing. A bounded setup-retry on a fresh task
is the weaker alternative - acceptable only because failing to establish a
precondition is different from failing the assertion, and it must still fail
closed when no failable task can ever be observed.


<!-- source: docs/CANARY_SCENARIO_DRIVER.md -->

# Canary scenario driver

`scripts/canary-scenario-driver.sh` is the repository-bundled adapter that the
GO-closure canary rehearsal invokes to mint candidate-bound, schema-v2 scenario
receipts. It is **not** release authority. The rehearsal still:

1. SHA-pins the driver bytes to `MERC_CANARY_APPROVED_DRIVER_SHA256`
2. Validates every receipt with `scripts/validate-canary-scenario-receipt.py`
3. Independently re-queries PostgreSQL for database-backed subjects
4. Checks final Prometheus and ledger state

If the work did not happen, the driver exits non-zero and prints **no** receipt
on stdout.

## Invocation

```text
"$MERC_CANARY_SCENARIO_DRIVER" run <scenario> <minimum>
```

One JSON document on stdout, `status: "PASS"`, `observed == minimum == len(evidence)`.

### Environment the rehearsal provides

| Variable | Role |
| --- | --- |
| `MERC_CANARY_RUN_ID` | 32 lowercase hex run id |
| `MERC_CANARY_RUN_STARTED_AT` | UTC start of the whole canary run |
| `MERC_CANARY_CANDIDATE_COMMIT` | 40 lowercase hex candidate |
| `MERC_CANARY_CONTROL_IMAGE` | immutable `registry/repo@sha256:…` |
| `MERC_CANARY_DRIVER_SHA256` | SHA-256 of the driver bytes |
| `MERC_CANARY_SCENARIO` | scenario name (must match argv) |
| `MERC_CANARY_SCENARIO_MINIMUM` | exact observation count |
| `MERC_CANARY_SCENARIO_STARTED_AT` | UTC start of this scenario window |

### Operator / host environment

| Variable | Role |
| --- | --- |
| `MERC_CONTROL_BASE_URL` or `STAGING_TLS_HOSTNAME` | control plane base URL |
| `MERC_CANARY_DATABASE_URL` (or `DATABASE_URL` / `MERC_TEST_DATABASE_URL`) | **read-only** observation DB |
| `MERC_CANARY_APPROVED_BUYER_EMAILS` | exactly two distinct approved buyers |
| `MERC_CANARY_APPROVED_WORKER_IDS` | exactly two approved worker UUIDs |
| `MERC_CANARY_APPROVED_AGENT_VERSIONS` | reviewed agent semvers |
| `MERC_CANARY_APPROVED_BUILD_HASHES` | reviewed 16-hex build hashes |
| `MERC_CANARY_BUYER_API_KEYS` | `email=key,…` or ordered keys |
| `MERC_CANARY_WORKER_TOKENS` | `uuid=token,…` or ordered tokens |
| `MERC_CANARY_ADMIN_API_KEY` | admin routes when needed |
| Stripe test keys + webhook endpoint IDs | `stripe_test_matrix` |
| `MERC_BACKUP_OFFSITE`, age recipient/identity | `backup_independent_restore` |
| `ALERT_RECEIVER_*`, `MERC_CANARY_ALERT_SINK_FILE` | `real_alert_firing_resolution` |
| `MERC_CANARY_WEBHOOK_URL` | public HTTPS buyer webhook target |

Live Stripe credential classes (`sk_live_*`, `rk_live_*`, `pk_live_*`) are refused
before any network call.

## What each scenario proves

| Scenario | Real work | Observes | Needs running |
| --- | --- | --- | --- |
| `approved_buyer_identity` | `GET /v1/me` for each approved buyer | durable buyer UUIDs | control + DB + two approved buyers with API keys |
| `distinct_metal_agent` | read workers heartbeating in-run | approved Metal worker UUIDs | two reviewed Metal agents online with approved version/build |
| `embed_success` | submit embed jobs via public API; wait complete | job UUIDs with positive `actual_usd` | control + DB + Metal agents + buyer credit |
| `batch_infer_success` | submit batch_infer jobs; wait complete | job UUIDs | same + batch_infer model path |
| `cancelled_job` | submit then `DELETE /v1/jobs/{id}` | cancelled job UUIDs | control + DB + buyer key |
| `forced_retry` | claim task, `POST …/fail` with retryable class | `task_requeued` job_event UUIDs | control + live approved worker + worker token |
| `stale_lease_recovery` | submit, wait for dead-claim / stuck rescue | rescue job_event UUIDs | worker that stops heartbeating past `deadWorkerAfter` (~180s) |
| `stale_attempt_commit_rejection` | advance attempt, commit stale epoch | HTTP 409 + unchanged state digests | control + worker token + DB |
| `buyer_webhook_retry_sequence` | register webhook, complete job, wait retries | webhook UUIDs with `attempts>=2` + delivery | public HTTPS webhook URL (SSRF guard blocks localhost) |
| `backup_independent_restore` | offsite encrypted backup + independent restore | offsite provider subject | `MERC_BACKUP_OFFSITE`, age keys, backup/restore scripts |
| `stripe_test_matrix` | bundled Stripe sandbox matrix | Stripe test-mode subject | staging TLS hostname, test keys, endpoint IDs, fixture `pi_`/`ch_`/`tr_` |
| `real_alert_firing_resolution` | fire + resolve via Alertmanager | distinct receiver event IDs | Alertmanager + real receiver sink file |
| `post_rehearsal_invariant_audit` | read-only SQL invariant map | clean invariant map | control DB |
| `bounded_retry_backoff_audit` | max `retry_count` ≤ policy ceiling | policy flags | control DB (Prometheus optional) |

## Operator review and SHA pin

1. Read this document and the driver source.
2. Place a **physical, non-symlink** copy of the reviewed executable on the
   staging host in a non-group/world-writable directory.
3. `sha256sum` (or `shasum -a 256`) the bytes; set
   `MERC_CANARY_APPROVED_DRIVER_SHA256` to that 64 lowercase hex digest in
   `.env.go-closure`.
4. Point `MERC_CANARY_SCENARIO_DRIVER` at the absolute physical path.
5. `scripts/go-closure-canary-rehearsal.sh --target local|ssh --check` admits the
   pin without running scenarios; `--execute` runs all fourteen.

The rehearsal re-hashes the driver before every scenario and after the run. Any
byte change fails closed.

## Local test

```bash
# control plane + DB + at least one Metal agent required for workload scenarios
bash scripts/test-canary-scenario-driver.sh
```

The test:

- refuses live Stripe prefixes and missing control base (empty stdout)
- refuses `backup_independent_restore` without offsite config
- runs each scenario against the local control plane
- validates any PASS receipt with `validate-canary-scenario-receipt.py`
- requires identity + invariant audits to PASS; workload scenarios PASS when
  agents are live and fail closed (honestly) when not

## What this driver does **not** prove

- **Not release authority.** Only the full GO-closure canary rehearsal receipt
  (plus restart storm, rollback, and 24h soak) can support a GO decision.
- **Not live money.** Stripe stays test-mode; `real_value` is always false.
- **Not public canary or self-serve suppliers.** Only approved buyer emails and
  worker UUIDs participate.
- **Not a substitute for database corroboration.** Evidence that exists only in
  driver memory is a defect; Merc re-queries PostgreSQL.
- **Not coverage of RunPod / CUDA lanes.** Missing staging capabilities fail
  closed with a precise diagnostic; they are not papered over.
- **Not offsite backup** when only a local Docker restore is available. The
  observation source is `offsite_backup_provider`.
- **Not alert receiver proof** without observed firing/resolved event IDs from
  the real receiver sink.
- **Not secret safety if you paste secrets into env diagnostics.** The receipt
  path refuses secret-shaped strings; operator logs are still your responsibility.
- **Not multi-scenario subject uniqueness enforcement across process restarts.**
  Each scenario creates fresh subjects; do not hand-craft overlapping IDs.

## Threat model (summary)

1. A PASS is impossible without the work actually happening.
2. `observed == minimum == len(evidence)` exactly; no pad, truncate, or dedupe.
3. Every evidence entry must be re-findable by the control plane / provider.
4. No secret values in any receipt field.
5. Stripe test-mode only; live key prefixes refused first.
6. Only approved participants.
7. Timestamps bracket real work inside the invocation window.
8. Subject IDs unique within a receipt; keep them unique across the run too.


<!-- contradiction-ledger: route-counts -->

## Contradiction ledger (unresolved)

Authorization / route counts disagree across sources. Both sides retained.

| Claim | Source |
|---|---|
| **110** method/path registrations | `docs/SECURITY.md` (frozen; not merged into this file) |
| **76 / 77** route figures | `docs/ARCHITECTURE.md § "Decision Zero reversal — `[KEEP-RT]` supersedes `[KILL-RT]`"` (absorbed above) |

Compare `ops/authorization-matrix.json` at execution time. Do not treat merge as picking a winner.

<!-- source: clients/sdk/python/README.md -->

# merc  -  Python buyer SDK

A thin, **dependency-free** client for the merc buyer REST API. The
runtime uses only stdlib `urllib`; installing the package adds no third-party
runtime dependencies.

The package is not published on PyPI yet. Install it from a repository checkout:

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install ./clients/sdk/python
python -c "from merc import Client; print(Client.__module__)"
```

```python
from merc import Client

cx = Client("http://localhost:8080", api_key="<your buyer api key>")

# Low-level: submit, wait, fetch the merged JSONL artifact.
job = cx.submit_job(model="all-minilm-l6-v2", job_type="embed",
                    input='{"text":"hello"}\n{"text":"world"}\n')
cx.wait(job["job_id"])
print(cx.results_text(job["job_id"]))

# OpenAI-shaped convenience: submit -> wait -> reshape in one call.
out = cx.embeddings("all-minilm-l6-v2", ["hello", "world"])
print(out["data"][0]["embedding"][:3], out["model"])
```

**Methods:** `submit_job(...)`, `get_job(id)`, `results(id)`, `results_text(id)`,
`results_records(id)`, `cancel_job(id)`, `wait(id, timeout)`, `models()`,
`estimate(model, units, tier)`, and `embeddings(model, input)`.

Every non-2xx response raises `APIError` carrying the HTTP status and the
server's error body  -  failures are surfaced, never swallowed.

**Verify the package from a clean environment:**

```bash
bash scripts/verify-python-sdk-package.sh
python3 clients/sdk/python/example.py --smoke  # builds + prints a request; no server
```

The verification script installs into a throwaway virtual environment, changes
out of the checkout, imports the installed wheel, checks its metadata and public
surface, and removes the environment on exit. It does not use `PYTHONPATH` or an
editable install.

Supported job types are `embed` and `batch_infer`. Inference parameters
(`max_tokens=`, `temperature=`) are folded into the tagged job type only when
given. Any other workload identifier fails locally.
