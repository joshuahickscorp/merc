# Alpha launch-readiness review — staging deploy at the launch line

Produced 2026-08-15. Deploy driven to the go-live boundary and **held there deliberately**:
the operator's steer is "move toward alpha, do **not** launch, give a final review at the line."
This is that review. Nothing in here has opened buyer admission, moved real money, or exposed a
public endpoint. Every step that would cross the line is listed as an operator action, not a done
thing.

## What is deployed and verified

Control plane at HEAD `deac3e9b76595b03dc0fe489cfdf0efdb7df21bb`, running on the DigitalOcean droplet
(`192.241.134.31`, `/opt/merc`), overlaid on the existing data plane.

| Check | Result |
|---|---|
| `/version` | `commit deac3e9b…`, `modified: false`, `v0.1.0-merc-rc1`, `linux/amd64` — honest, clean tree |
| Boot | `catalogue price schedule …: 1 model price(s) updated atomically` — BuildCataloguePriceSchedule published a lane |
| `/healthz` | 200 |
| `/prices` | 200, 8.5 KB live catalogue |
| Payment authority | `mode=test provider_enabled=true live_value_movement=false` (Stripe **test** key, CAD, payouts unblocked) |
| Data plane | `merc-postgres-1` / `merc-minio-1` `StartedAt` = 2026-08-10T16:57:40, **unchanged** across every deploy step — the money DB was never recreated |
| Restarts | 0 |

Deploy invocation (data-plane-safe): `docker compose -f docker-compose.prod.yml -f
docker-compose.smallhost.yml -f docker-compose.canary.yml up -d --no-deps --no-recreate control`.

## Bugs found and fixed on the way (all committed / configured)

1. **Missing boot receipts in the image** — `Dockerfile.control` copied `evidence/benchmarks/` but not
   `evidence/perf/runtime-benchmarks/`, where the g070 llama `r5` receipt the catalogue cites at boot
   lives. The image crash-looped ("cited receipt … not found under any repo root"). Fixed + committed
   in `deac3e9b` (COPY the dir; 232 K, all LFS-smudged).
2. **`MERC_ENV=production` hardcoded** in the `prod.yml` `x-control-env` anchor blocks test payment
   mode (payment_authority.go:389). Alpha is Level B = staging (release_launch.go:27), so added
   `docker-compose.canary.yml` overriding `MERC_ENV=staging` for the control service.
3. **Stripe key wiring** — the anchor mounts `${STRIPE_SECRET_KEY_SOURCE}` → `/run/secrets/merc-stripe-secret-key`
   and reads `${STRIPE_SECRET_KEY_CONTAINER_FILE}`. Wrote the test key to `secrets/stripe-secret-key`
   (chown `65532` = distroless `nonroot`, mode 600; billing.go refuses group/other-accessible keys).
4. **Settlement currency** — droplet `.env` has `MERC_SETTLEMENT_CURRENCY=usd` while the Stripe test
   account is CA/CAD ("cannot settle USD"). A **P1-STRIPE-TEST** fix (set `cad`), not a boot blocker.

## The launch line — why the public endpoint is not up

`/readyz` returns 503 `canary_policy_unconfigured`. `caddy` (`depends_on control: {condition:
service_healthy}`, and `/merc-healthcheck` probes `/readyz`) therefore stays down, so there is no
public TLS. This is correct: a control plane that cannot govern who may transact is not "ready."

Crossing the line requires **one** of two operator artifacts — neither of which can or should be
fabricated by the agent:

- **A signed canary-disable decision** (`canary_decision.go`): an HMAC-digested, role-authorized,
  candidate-scoped, time-bounded JSON decision that opens self-serve signup. Set
  `MERC_CANARY_DISABLE_DECISION_REF` to its absolute path (mounted 600). This is a governance
  decision — it must be signed by the authority that holds the key, not forged.
- **Or a bounded canary** (`MERC_CANARY_MODE=true`): approved buyer emails, worker IDs, agent
  versions, build hashes, active-buyer/worker limits, **and two distinct `whsec_` webhook secrets**
  (`STRIPE_WEBHOOK_SECRET` + `MERC_CONNECT_WEBHOOK_SECRET`). The webhook secrets are not held on this
  machine; they come from the Stripe test dashboard once the endpoint URL exists.

## Go-live sequence (operator, when ready to launch — not done here)

1. Cloudflare grey-cloud A records: `mercmerc.net` + `storage.mercmerc.net` → `192.241.134.31`.
2. Provide the canary artifact above (signed disable decision, or bounded-canary config + webhook
   secrets). Fix `MERC_SETTLEMENT_CURRENCY=cad` to match the Stripe account.
3. `docker compose … up -d --no-deps caddy` — Caddy obtains the cert via HTTP-01 once DNS resolves.
4. `ops/scripts/alpha/probes.sh --execute` from a separate host; then `ops/scripts/alpha/deploy.sh
   --record-pass`.
5. Flip Stripe to live only as the final, separate launch action.

## Remaining alpha gates (not yet closed)

| Gate | State |
|---|---|
| boot | DONE only when `evidence/state/alpha-boot-green.json` is BOUND PASS at the candidate HEAD (`ops/scripts/alpha/lib.sh` refuses deploy otherwise) |
| P1-STAGING (deploy) | control serving in test mode at the launch line (this review). Public TLS pending the canary artifact. |
| P1-STRIPE-TEST | pending; fix settlement currency + webhook secrets first |
| P1-OFFSITE-RESTORE | pending (backup dump + cross-provider restore drill) |
| P1-ALERT-DELIVERY | pending (fire Alertmanager → external sink; pages a human) |
| P1-CANARY-REHEARSAL | pending; **this is the launch** (2 buyers + Mac Studio & MacBook workers) |
| P1-RECOVERY-SOAK | pending; 24 h, run last |
| Test-suite latency (G080) | 681 s, target < 60 s; round-3 lane measuring the real bottleneck (per-test `CREATE DATABASE` clone contention) |

## Honest risks / notes

- Droplet is 961 MB / 1 vCPU with ~271 MB free before control started. control (limit 300 m) + caddy
  (96 m) on top of PG (300 m) + MinIO (160 m) is tight; watch for OOM under load. 10 M-device scale is
  a separate hardening question (G081/G074b) — the droplet is a proxy, not the 10 M host.
- RunPod GPU supply is **held**: a billed pod would idle while admission is closed. Start it only when
  the canary is open and jobs can route to it.
- The economics warning on boot ("supplier economics underwater: llama … net=-$0.018/hr") is expected
  and known — the published lane is priced honestly below break-even on this hardware.
