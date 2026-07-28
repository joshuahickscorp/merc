# Getting to 8.5 — a tutorial for the ten things only you can do

> **Historical planning snapshot — superseded on 2026-07-28.** Do not use this
> file as the current release checklist. The RunPod proof was completed and
> Decision Zero was resolved as `KEEP-RT`; see `DECISION_ZERO_REVERSAL.md`.
> Current external actions and release gates are authoritative in
> `EXTERNAL_OPERATOR_HANDOFF.md`, `SHIPPABILITY_STATUS.md`, and
> `ops/go-no-go.json`. This snapshot remains only to preserve decision history.

Everything else is being built for you. This document is the complete list of
what an agent cannot do on your behalf, why, and exactly how to do each one.

**Total hands-on time for items 1–6: about 25 minutes.** Items 7–10 involve
other people and run on their calendars, not yours.

Each item says what it unblocks, so you can stop wherever the return stops being
worth it.

---

## 1. Make the repository private — 5 seconds

**Why an agent won't:** changing the visibility of your GitHub account's
repository is an account setting on an outward-facing service. It is also the
single item where doing nothing has an ongoing cost.

**Why it matters:** the repo is public, has no `LICENSE`, and your own
`NOTICE:11-13` says *"Public source or binary distribution remains blocked
pending an owner-approved project license."* Anyone who has cloned it holds no
grant, and you hold no defensive terms. It also blocks item 4, which blocks PyPI,
which caps developer experience.

```bash
gh repo edit joshuahickscorp/merc --visibility private
```

Check first if you care about forks:

```bash
gh repo view joshuahickscorp/merc --json forkCount,stargazerCount
```

**Unblocks:** the only open legal exposure. Prerequisite for item 4.

---

## 2. A working GPU credential — 30 seconds

**Why an agent won't:** provisioning GPUs spends your money, and the key on disk
is dead so there is nothing to spend.

**What I found:** `.secrets/runpod.env` holds a 50-character `RUNPOD_API_KEY`. It
returns **HTTP 401** from `rest.runpod.io/v1/pods`. Stale or revoked.

**What to do:** create a fresh key at
`runpod.io → Settings → API Keys` (read+write), then:

```bash
printf 'RUNPOD_API_KEY=%s\n' 'YOUR_NEW_KEY' > .secrets/runpod.env
chmod 600 .secrets/runpod.env
```

Any provider works — Lambda, Vast, Modal — the harness is provider-agnostic HTTPS.
Make sure the account has a few dollars of credit; a parity run is roughly one
hour of one A100, about **$2–3**.

**Verify it took:**

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $(cut -d= -f2- < .secrets/runpod.env)" \
  https://rest.runpod.io/v1/pods
```

`200` means good. `401` means the key is wrong.

**Unblocks:** inference performance 6 → 8. This is the only route to a real
vLLM-on-CUDA parity number, and every performance and price claim in the repo
currently traces back to one hand-typed laptop benchmark.

**Note:** protocol conformance is already proven against a *real* engine without
any GPU — `make real-engine-conformance` runs llama.cpp on the exact model the
catalogue prices and produces
`evidence/realtime/real-engine-conformance.json`. What the GPU adds is the
competitive throughput number, not basic correctness.

---

## 3. Decision Zero — one call, no typing

**Why an agent won't:** it decides what the company sells. Two independent
analyses landed on opposite sides (78% vs 72%) — that overlap means the evidence
genuinely underdetermines it.

**Read:** `docs/DECISION_ZERO.md`. It costs both branches.

**The question that resolves it:** *within 90 days, can you get a Linux/CUDA
supplier to register and serve real traffic?*

- **Yes** → `[KEEP-RT]`. Realtime is the only path to that supply.
- **No** → `[KILL-RT]`. `control/types.go` admits only four Apple hardware
  classes, so an 8×H100 supplier gets HTTP 400 — you cannot sell a latency SLA
  on machines the control plane refuses.

Reply with the branch and I will execute it. The lane is snapshotted with
checksums outside the repo, so either direction is reversible.

**Unblocks:** roughly half the remaining roadmap, and ends paying to maintain
two products.

---

## 4. Choose a licence — 2 minutes

**Why an agent won't:** it grants or withholds rights to your work in perpetuity.

**Current state:** no `LICENSE` file. `agent/Cargo.toml:6` declares `license =
"MIT"` with no MIT text present, so the package metadata and the repository
disagree.

**If MIT is what you meant** (which `Cargo.toml` implies):

```bash
curl -sL https://raw.githubusercontent.com/licenses/license-templates/master/templates/mit.txt \
  | sed "s/{{ year }}/2026/; s/{{ organization }}/Joshua Hicks/" > LICENSE
```

Then delete the "no project-level LICENSE" paragraph from `NOTICE`.

If you want something else — Apache-2.0 for the patent grant, or a source-available
licence to stop a competitor reselling it — say which and I will write it.

**Unblocks:** PyPI publishing → DX 7 → 8.5. Resolves the `NOTICE` contradiction.

---

## 5. Real incident contacts — 5 minutes

**Why an agent won't:** inventing a phone number that pages nobody is worse than
an empty field, because it looks handled.

**Current state:** `make release-gates` fails on this by design:

```
runbook-contacts: FAIL: 6 placeholder contact line(s)
```

Edit `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md:9-14`. Six roles: incident commander,
security/privacy counsel, payments owner, supplier ops, support intake, status
channel. **At this stage they can all be you** — one email and one phone number
that actually reaches you beats six `[CONTACT REQUIRED]` markers.

```bash
make release-gates     # should stop reporting the contacts failure
```

**Unblocks:** the readiness GO gate; claims integrity 9 → 9.5.

---

## 6. A paging destination — 5 minutes

**Why an agent won't:** it is an account on a third-party service, and an on-call
routing policy is a commitment about who wakes up.

Simplest version — a Slack incoming webhook:

```bash
mkdir -p /run/secrets 2>/dev/null || true
printf '%s' 'https://hooks.slack.com/services/XXX/YYY/ZZZ' \
  | sudo tee /run/secrets/cx_alert_receiver_url > /dev/null
```

`monitoring/alertmanager.yml:21` already reads that path. PagerDuty or Opsgenie
work the same way.

**Unblocks:** operability 8 → 9. Turns "alerting configured" into "alerting
delivered", which is the difference between a dashboard and an on-call system.

---

## 7. Stripe sandbox — about 1 hour, deferred as you asked

Sequenced last deliberately. In the Stripe Dashboard, in **test mode**:

1. Create a webhook endpoint → copy `whsec_...` into `STRIPE_WEBHOOK_SECRET`.
2. Create a **second, distinct** endpoint for Connect → `STRIPE_CONNECT_WEBHOOK_SECRET`.
   The validator refuses if both secrets are the same, on purpose.
3. Enable Connect, note the `ca_...` client id.
4. Copy the `sk_test_...` key.

Put them in `.env`. **Never a `sk_live_` key** — the code refuses to start and
the scripts refuse before any network call.

```bash
make stripe-check && make stripe-matrix
```

**Unblocks:** money safety 9 → 9.5. The reversal path is implemented and
simulator-tested but has never met real Stripe.

---

## 8. Counsel on the model licences — external

`docs/THIRD_PARTY_LICENSES.md:31-32` marks **both** catalogue models BLOCKED —
Llama 3.2 1B and all-MiniLM-L6-v2 — while the binary prices and serves them.
Every model you sell is marked blocked in your own register.

`make license-register` fails on this deliberately. **Do not resolve it by
editing the register**; that is the one move that converts a real legal question
into a fake green check.

**Unblocks:** legal GO; the right to make public serving claims.

---

## 9. External penetration test — external

Security is at 7 and reaches 8 autonomously. The last point needs someone who
does not work for you attacking it. The codebase is unusually ready for this: the
webhook egress path has a DNS-rebinding-safe pinned dial, CI runs `govulncheck`,
`gitleaks` and `cargo-audit`, and admin authority is now a separate principal.

**Unblocks:** security 8 → 9.

---

## 10. The name — external, and the clock is running

`mercmerc.net` is a live, funded GPU-compute marketplace. Press, SEO and
enterprise legal review all reach them first. This costs more the longer you
build brand on the current name.

Get a trademark search. The answer may be "proceed anyway" — but make it a
decision rather than a default.

---

## What this buys you

| After | Score |
|---|---|
| Today | 5.5 |
| Autonomous work completing now | ≈7 |
| Items 1–6 (about 25 minutes) | ≈8 |
| Items 7–10 | ≈8.5 |

## What no list closes

**You have no users, and you do not own GPUs.** Competitive position stays around
3 until someone pays you, and inference performance stays capped while the only
hardware the control plane admits is Apple Silicon. Every item above is worth
doing. None of them substitutes for a first paying buyer.

The cheapest honest test of the whole thesis is item 3 plus three design
partners. If three people who are not you will run real jobs, the rest of this
matters. If none will, that is the most valuable thing you could learn, and it
costs a week rather than another quarter of engineering.
