# Backend alpha staging plane (lane L1)

The `/readyz` 503 on the mercmerc.net droplet was `canary_policy_unconfigured`.

`MERC_CANARY_MODE=false` (compose default) without `MERC_CANARY_DISABLE_DECISION_REF`
fail-closes canary to `{Enabled: true, configError: decision missing}`. `/readyz`
reads that boot copy first. The repeating
`workers: stale-requeue: canary retry policy: canary disable decision reference is absent`
line is the same error via `canaryRetryLimit()`, not a separate readiness
condition. Worker leadership then cycles about every 100s because stale-requeue
never succeeds.

A controlled backend alpha does not open self-serve signup, so this lane does
**not** write a `DISABLE_CANARY_ADMISSION` decision. The fix is config supply:
`MERC_CANARY_MODE=true` with the allowlist in `ops/staging/alpha-participants.json`.

`validateCanaryMoneyMode` still demanded a Stripe Connect `ca_*` and a Connect
webhook whenever canary was on. This host is not a Connect platform (no `ca_*`,
no Connect return/refresh URLs). The narrow gate correction in
`control/main.go` is: when `MERC_ENV=staging` and Connect is not configured,
require a test-mode Stripe key (and a well-shaped billing webhook if one is
set) and do not demand Connect. Production and any staging stack that sets
Connect URLs or a `ca_*` still take the full pair.

Port 443 is bound to loopback only. The command that would publish it is
recorded in `ops/staging/compose.tls-loopback.yml`. Do not run it from this
lane.
