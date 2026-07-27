# Deploying merc to the droplet

Target: `192.241.134.31`, serving `mercmerc.net`. The droplet currently runs an
older build on `computexchange.net`; `/version` 404s there, so what is live
predates the economics fix, the honeypot fail-closed fix, and KILL-RT.

I have no SSH key to that host, so every step below is yours to run. Everything
that could be prepared without it has been.

---

## 0. Before anything: commit

The image records `MERC_BUILD_COMMIT` from a build argument and reports
`modified: false` regardless of whether the tree was dirty. An image built from
uncommitted work therefore claims a provenance it does not have. Commit first,
then build, so `/version` is true.

```bash
git status --porcelain | head
```

## 1. The environment the control plane demands

This list is not from the docs — it is what a `MERC_ENV=production` boot actually
refused to start without, in the order it refused. Each line below was a
separate `refusing to start`:

| Variable | Note |
|---|---|
| `MERC_ENV=production` | Turns on every check in this table. **Must be spelled `production` or `prod`.** |
| `MERC_ADMIN_CIDRS` | Comma-separated operator management CIDRs. |
| `MERC_TRUSTED_PROXY_CIDRS` | Caddy's network, so client IPs are attributed correctly. |
| `MERC_PUBLIC_CONTROL_ORIGIN` | `https://mercmerc.net` |
| `SITE_HOST` | `mercmerc.net` — also required to validate Stripe Connect return origins. |
| `STORAGE_HOST` | `storage.mercmerc.net` |
| `MERC_TOKEN_KEY` | **Copy the existing value byte-for-byte.** `control/crypto.go` derives the AES key as `sha256(value)`; regenerating it makes every sealed OAuth token and webhook secret already in Postgres permanently undecryptable. |
| `MERC_VERIFICATION_SAMPLE_SECRET` | Without it, `control/verification.go` silently substitutes a **published** default sampling secret. |
| `MERC_ECON_SCHEDULE_VERSION` | `2026-07-19` |
| `MERC_PROCESSOR_PERCENT_BPS` | `290` |
| `MERC_PROCESSOR_FIXED_USD` | `0.30` |
| `MERC_CONTROL_PLANE_PER_TASK_USD` | `0.0001` |
| `MERC_TARGET_MARGIN_BPS` | `1000` |
| `STRIPE_SECRET_KEY` | |
| `STRIPE_WEBHOOK_SECRET` | |
| `MERC_CONNECT_WEBHOOK_SECRET` | Must differ from the billing secret. |
| `MERC_CONNECT_RETURN_URL` | `https://mercmerc.net/supplier/connected` |
| `MERC_CONNECT_REFRESH_URL` | `https://mercmerc.net/supplier/reconnect` |
| `DATABASE_URL`, `S3_*` | As already configured on the host. |

**The rename has not touched these names.** Tier 1 of the rebrand was
repo-internal only, so the droplet's existing `CX_*` variables still apply
unchanged. See `docs/RENAME_REGISTER.md` before renaming any of them.

## 1a. Environment variable cutover — `CX_*` is now `MERC_*`

Every variable in this runbook was renamed from `CX_` to `MERC_`. The droplet
currently has the **old** names set, so the rename is a coordinated cutover, not
a drop-in deploy:

```bash
# On the droplet, rewrite the env file in place. Keeps every VALUE byte-identical.
cp /path/to/.env /path/to/.env.bak.$(date -u +%Y%m%dT%H%M%SZ)
sed -i -E 's/^CX_([A-Z0-9_]+)=/MERC_\1=/' /path/to/.env
grep -c '^MERC_' /path/to/.env   # sanity: should match the old CX_ count
```

**Copy the values, do not regenerate them.** `MERC_TOKEN_KEY` in particular
derives the AES key as `sha256(value)`; a new value makes every sealed OAuth
token and webhook secret already in Postgres permanently undecryptable.

A half-applied cutover is the dangerous state: the binary reads `MERC_ENV`,
finds nothing, and skips the production-hardening refusal — booting with a
warning while writing `plain:`-prefixed secrets. Rename the whole file at once.

GitHub Actions secrets need the same rename.

## 2. The one thing that will still refuse

Boot now runs a settlement preflight. Against the current Stripe account it
fails, and in production that is fatal rather than advisory:

```
control: stripe settlement preflight failed [account CA/CAD]: stripe platform
cannot settle USD (enabled settlement currencies: cad); every supplier payout
would fail with balance_insufficient.
```

This is correct — the ledger is USD-only (`control/payment.go` rejects any other
currency) and the platform is Canadian, so no supplier payout could ever
complete. `POST /v1/topups` for `CA`/`USD` is explicitly unsupported, so it
cannot be worked around from this side.

**Fix before deploying with a Stripe key**: add USD as a settlement currency on
the Stripe account (supported for Canadian platforms with a USD bank account),
or move to a platform whose country settles USD. See `docs/STRIPE_CONNECT.md`.

To deploy the site and control plane *without* the payout rail in the meantime,
leave `STRIPE_SECRET_KEY` unset — the preflight only runs when a key is present,
and the payout rail degrades to "none configured" (funded credits reach `owed`,
never `released`).

## 3. Build the image — use CI, not your laptop

`.github/workflows/publish-candidate.yml` already builds on `ubuntu-latest`
(native amd64) and cosign-signs the result with an SPDX SBOM and SLSA
provenance, keyless via GitHub OIDC. It triggers on push to
`release/rc1-go-closure`, which is the branch this work is on. A hand-built
image carries none of that, and the receipts under `evidence/` are written
against signed images — so pushing is both easier and more correct:

```bash
git push origin release/rc1-go-closure
```

**Cross-building amd64 on the Mac works — but only under OrbStack.** Under
colima/qemu the Go compiler dies with `fatal error: found pointer to free
object` (the Go GC misbehaving under qemu-user, not a code fault). OrbStack uses
Rosetta instead and builds cleanly:

```bash
docker context use orbstack
docker build --platform linux/amd64 -f Dockerfile.control \
  --build-arg MERC_BUILD_VERSION=v0.1.0-merc-rc1 \
  --build-arg MERC_BUILD_COMMIT="$(git rev-parse HEAD)" \
  --build-arg MERC_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -t merc/control:amd64-rc1 .
```

Built and smoke-tested that exact image here — 6.7 MB, and it serves:

```
/healthz -> 200
/version {"version":"v0.1.0-merc-rc1","platform":"linux/amd64","modified":false}
```

Ship it directly if you prefer not to wait on CI:

```bash
docker save merc/control:amd64-rc1 | gzip | ssh root@192.241.134.31 'gunzip | docker load'
```

A locally built image carries **no cosign signature, SBOM or SLSA provenance**,
and the receipts under `evidence/` are written against signed images. Prefer the
CI path for anything you intend to keep.

If you would rather not use CI, build natively **on the droplet** — it is
x86_64, so no emulation is involved.

`docker-compose.prod.yml` references `computexchange/control`, and the published
image is `ghcr.io/<owner>/computexchange-control`. Both are **EXTERNAL** renames
per `docs/RENAME_REGISTER.md`: rename the registry package first, then the
compose reference. Never rewrite the digests recorded under `evidence/` — they
attest to builds that really happened under the old name.

## 4. Cutover

`mercmerc.net` and `storage.mercmerc.net` already resolve to the droplet as
proxied=false A records, which is what lets Caddy terminate TLS and obtain a
certificate over HTTP-01. Nothing else in DNS needs to change.

Deploying with `SITE_HOST=mercmerc.net` moves the live site: Caddy will obtain a
certificate for the new host and stop serving `computexchange.net` unless you
keep a block for it. Decide deliberately whether the old hostname should redirect
or 404 — I have not changed its DNS, and it is serving 200 right now.

## 5. Verify from off-box

```bash
curl -s https://mercmerc.net/version
curl -s -o /dev/null -w '%{http_code}\n' https://mercmerc.net/healthz
curl -s -o /dev/null -w '%{http_code}\n' https://mercmerc.net/readyz
```

`/version` should report `v0.1.0-merc-rc1` and the commit you built. If it 404s,
the old build is still serving.

## 6. Re-point the Stripe webhooks

The two endpoints currently point at a dead `trycloudflare.com` quick tunnel.
Once `mercmerc.net` serves, recreate them — the API returns the signing secret,
so no Dashboard visit is needed:

```bash
curl -s https://api.stripe.com/v1/webhook_endpoints -u "$STRIPE_SECRET_KEY:" \
  -d url=https://mercmerc.net/v1/stripe/webhook \
  -d 'enabled_events[]=payment_intent.succeeded' \
  -d 'enabled_events[]=setup_intent.succeeded' \
  -d 'enabled_events[]=payment_method.attached' \
  -d 'enabled_events[]=charge.refunded' \
  -d 'enabled_events[]=charge.dispute.created' \
  -d 'enabled_events[]=charge.dispute.closed' \
  -d 'enabled_events[]=payout.created' \
  -d 'enabled_events[]=payout.paid' \
  -d 'enabled_events[]=payout.failed'
```

```bash
curl -s https://api.stripe.com/v1/webhook_endpoints -u "$STRIPE_SECRET_KEY:" \
  -d url=https://mercmerc.net/v1/stripe/connect-webhook \
  -d connect=true \
  -d 'enabled_events[]=account.updated'
```

Put each response's `secret` into `STRIPE_WEBHOOK_SECRET` and
`MERC_CONNECT_WEBHOOK_SECRET`, and each `id` into
`STRIPE_BILLING_WEBHOOK_ENDPOINT_ID` and `STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID`.
The two secrets **must differ** — that check stops a leaked billing secret being
used to forge a Connect "payout succeeded" event. Then delete the two old
`we_…` endpoints pointing at the dead tunnel.

## 7. Then

```bash
make stripe-check && make stripe-matrix
```

`stripe-check` now fails closed if the platform cannot settle USD, so it will
stay red until step 2 is resolved. Once green, the matrix closes the 6-point
`money_and_reconciliation` gap and readiness moves from 83 toward 89.
