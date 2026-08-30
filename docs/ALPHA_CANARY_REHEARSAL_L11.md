# L11 — buyer, supplier, verification, settlement rehearsal

Operator-controlled / synthetic rehearsal against the live staging
plane `https://mercmerc.net`. This is `P1-CANARY-REHEARSAL` work. It
does **not** satisfy `EXTERNAL_ALPHA_PROVEN`. The five closeout
receipts below are unbound rehearsal records of an attempt: the loop
did not close, and they are not authority for a completed matrix.

Driver: `python3 ops/scripts/alpha-e2e-rehearsal.py run`
Make: `make alpha-e2e-rehearsal`

## Deployed plane

The lane started against commit `8283ae583057d6265947a473023e4f05102704b4`.
A parallel deploy lane swapped control to
`a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05` while this rehearsal was in
flight. Probes were re-run against the new commit. `/readyz` stayed
`payment_mode=test`, `live_value_movement=false`. `MERC_CANARY_MODE=true`.
`MERC_PAYOUT_EXPORT` is unset.

## Participants actually exercised

| Role | Count wanted by the full matrix | Count exercised | Identity |
|---|---:|---:|---|
| Synthetic / operator buyer | 2 | **1** | `joshuahicksboba@gmail.com` (allowlisted `operator_buyer`) |
| Operator-controlled Metal worker | 2 | **1** reserved UUID minted; **0** registered/claimed | `7d2bb6c8-c45a-505e-ae39-6b9fc73989f5` |

The allowlist was not widened. `max_active_buyers=1` and
`max_active_workers=1`. A second buyer or worker would require adding
identities to `ops/staging/alpha-participants.json` **and** the live
`MERC_CANARY_*` env, then a control restart. That was not done.

No participant here is an independent external buyer or supplier.

## Closeout conditions 4–7

### 4. Buyer execution — PARTIAL

Receipt (unbound rehearsal record of an attempt; the loop did not close; not authority): `evidence/canary/l11-p1-canary-rehearsal-buyer-execution.json`

Proven on the live authenticated route:

- unlisted signup `canary-bot-unlisted@example.test` → **403** canary refuse
- approved email signup / `GET /v1/me` → **200**, `is_admin=false`
- anonymous `POST /v1/jobs` → **401**
- missing `Idempotency-Key` → **400** (key is required)
- Stripe test mode configured; buyer has **$0** free credit and no card

Not proven: a job accepted, priced, and left in a claimable state.
`POST /v1/quote` and `POST /v1/jobs` return **400**:

```
runtime capability is not advertised for job_type="embed"
model="all-minilm-l6-v2" (matrix 2026-08-04.1)
```

`GET /v1/models` is an empty list. In Postgres,
`candle-metal-minilm-embed` and `candle-metal-llama1-infer` are
`lifecycle=ACTIVE` but `routable=false`. Ordinary advertisement requires
`cellAuthorityBindable` → `binding_status=BOUND` on the embedded
benchmark receipt. Flipping `routable` or minting a fake BOUND receipt
would lower the bar. Directed `POST /admin/runtime/jobs/directed` still
goes through `normalizeAdvertisedRuntimeModelRef` and hits the same
gate.

Idempotent replay of one accepted job therefore could not be shown.
Repeating the refused submit with the same key created **zero** jobs
(`GET /v1/jobs` stayed empty; live `jobs` table count is 0).

### 5. Supplier execution — PARTIAL

Receipt (unbound rehearsal record of an attempt; the loop did not close; not authority): `evidence/canary/l11-p1-canary-rehearsal-supplier-execution.json`

Proven:

- `POST /v1/supplier/worker-tokens` with the reserved UUID → **201**
- same call with a foreign UUID → **403** `worker is not approved for
  this private canary` (identity binding at the canary gate)
- `GET /v1/worker/poll` with the reserved token before revoke → **204**
- `DELETE /v1/supplier/worker-credentials/{id}` → **204**
- poll after revoke → **401** `invalid worker token`

Not proven: register, claim, execute, return a result.
`POST /v1/worker/register` → **400** no cell activated for routing or
directed use. The process overlay quarantines ACTIVE cells whose
document `Routable` is false (unbound receipts). The real Metal agent
was not started against staging because register cannot succeed, and
its live build hash would also have to match `f4303a751ca2b2af`.

Unbound tokens are allowed because `MERC_ENV=staging` (not production).
They are not device-bound (`enrollment_device_bound=false`). Ordinary
claim still requires a directed or advertised cell.

### 6. Verification — BLOCKED

Receipt (unbound rehearsal record of an attempt; the loop did not close; not authority): `evidence/canary/l11-p1-canary-rehearsal-verification.json`

Accept and reject both require a committed task on the real
`POST /v1/worker/task/{id}/commit` path. No job was accepted and no
worker registered, so no result was committed. Inserting a task row
and marking it verified would not be the live path.

### 7. Settlement (ledger) — BLOCKED

Receipt (unbound rehearsal record of an attempt; the loop did not close; not authority): `evidence/canary/l11-p1-canary-rehearsal-settlement.json`

Live Postgres counts after the rehearsal: `jobs=0`, `tasks=0`,
`ledger_entries=0`. There is no settlement to replay. What is proven:
`/readyz` is test-mode with `live_value_movement=false`; payout export
is unset; no live money moved.

## Fail-closed controls (reachable depth)

Receipt (unbound rehearsal record of an attempt; the loop did not close; not authority): `evidence/canary/l11-p1-canary-rehearsal-fail-closed-controls.json`

| Control | Result |
|---|---|
| Canary unlisted buyer | 403 |
| Canary foreign worker | 403 |
| Admin from the operator Mac | 403 `admin source address not allowlisted` (`MERC_ADMIN_CIDRS=127.0.0.1/32,::1/128`) |
| Kill switch | pause+resume `dispatch` from control loopback, versions 2→3, then left unpaused |
| Intake pause | pause `intake`; `GET /v1/jobs` still 200; `GET /v1/jobs/{id}/results` still 404 (answers); resume |
| Revocation | three rehearsal worker credentials deleted; subsequent poll 401 |
| No payout export | `/readyz` test / no live value; `MERC_PAYOUT_EXPORT` unset |
| Reconciliation | nothing to reconcile; ledger empty |

A rehearsal admin credential labelled `l11-rehearsal-operator` was
minted in `admin_credentials` to drive loopback controls and **revoked**
afterwards.

## What the full counted `P1-CANARY-REHEARSAL` matrix still needs

The exit criterion still wants two synthetic buyers, two distinct
operator-controlled Metal workers, and the counted scenario driver
(20 embed / 20 batch_infer / …). This lane exercised **1 buyer** and
**1 reserved worker identity**, and could not run the counted matrix
because no cell is advertised.

To finish the matrix later, without lowering the bar:

1. A BOUND benchmark receipt for at least one candle Metal cell so
   ordinary advertisement (or a legitimate directed bootstrap that
   does not skip `normalizeAdvertisedRuntimeModelRef`) exists.
2. Add a second approved buyer email and a second worker UUID to the
   allowlist **and** the live env; raise `max_active_*` to 2.
3. Device-bound enrollment on two Metal machines; pin real
   `agent_version` + 16-hex `build_hash`.
4. Fund the buyer in Stripe **test** mode (or a sandbox credit grant)
   so a batch job can reserve.
5. Drive accept **and** a corrupted-result reject, then prove ledger
   single-settlement.

`P1-CANARY-REHEARSAL` stays open in `ops/go-no-go.json`.
`EXTERNAL_ALPHA_PROVEN` stays `NO_GO`.
