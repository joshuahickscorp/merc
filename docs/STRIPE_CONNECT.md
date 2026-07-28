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
