# Level-B operator handoff

Current committed candidate: `54ab790d5926d3eb671a754e8d07dbb48557fbde`.
The plan command binds the actual candidate to exact clean `HEAD`; reseal after
any commit. Readiness is **84/100**, P0 is **0**, P1 is **8**, Level B is
**NO-GO**, and Level C is **NO-GO / NOT ASSESSED**. **84/100 is the
machine-reachable ceiling** without staging, offsite storage, or human
approvers; the remaining 16 points are external-only (see
`docs/FACET_EXTERNAL_ACTION_PACK.md`). A Stripe live credential is prohibited
and is not an input to this procedure.

The eight P1s are: persistent TLS staging; independent encrypted offsite
backup and isolated restore; complete Stripe sandbox CAD matrix; external
staffed alert fire and resolution; approved buyer/worker canary; rollback/restart storm and
24-hour soak; independent accountable review/retest; and candidate-bound
governance approvals. No local or simulated receipt closes one of them.

## External bundles

| Bundle | Obtain from / required scope | Secure location | Non-secret verification | Gate |
| --- | --- | --- | --- | --- |
| Staging | Project staging host, SSH authority, base hostname and Cloudflare authority; isolated persistent test stack only | topology in `ops/launch/level-b.yaml`, credentials in `.merc-launch.env` mode 0600 | `merc release doctor --config ops/launch/level-b.yaml --secrets-file .merc-launch.env` | TLS staging, rollback, storm, soak |
| Artifact storage | Persistent staging object-store endpoint/bucket and least-privilege credentials | YAML and mode-0600 env file | same doctor command | durable artifacts |
| Offsite backup | Independent provider/bucket, restricted credentials, age recipient and separate age identity | mode-0600 env file; identity in separate mode-0600 file | `make release-doctor CHECK=backup` | backup and isolated restore |
| Stripe sandbox | `sk_test_`/approved `rk_test_`, both webhook secrets, test Connect account and endpoint IDs | mode-0600 env file | `make release-doctor CHECK=stripe` | CAD matrix |
| Alerting | Real on-call receiver credential/URL | mode-0600 env file | `make release-doctor CHECK=alert` | firing and resolution |
| Participants | Two approved synthetic buyers, two approved workers, enrollment authority and reviewed drivers | restricted completed participant record | `make release-doctor CHECK=canary` | approved canary |
| Independent review | Named non-author reviewer and completed report/retest | restricted review record | `make release-doctor CHECK=review` | review/retest |
| Governance | Exact-candidate named approvals and exercises | restricted governance evidence store | `merc release approvals-check --bundle <restricted-bundle>` | governance |

`merc release inputs --minimal --json` reports these eight bundles and does not
treat the 24-hour soak as an input. `merc release inputs --explain` classifies
the detailed adapter fields. Copy `.merc-launch.env.template` before adding any
secret; use the two JSON templates as non-secret record shapes.

Advisory Grok status: **NO_USABLE_VERDICT**. This workspace has no authenticated
Grok adapter; prior unavailable-adapter logs remain under `.artifacts/`. Grok is
not treated as an approval or as evidence for any gate.

## Resume

1. Fill `ops/launch/level-b.yaml` (the object hostname may be derived as
   `objects.<staging hostname>`).
2. Store secrets in mode-0600 `.merc-launch.env`.
3. Complete participant and approval records outside git.
4. Run:

```sh
merc release doctor --config ops/launch/level-b.yaml --secrets-file .merc-launch.env
merc release plan --config ops/launch/level-b.yaml --secrets-file .merc-launch.env --out .artifacts/level-b-plan.json
merc release launch --environment staging --config ops/launch/level-b.yaml --secrets-file .merc-launch.env --apply --approve-plan <PLAN_SHA256>
```

The last command remains fail-closed: it either produces candidate-bound
evidence and a decision, or reports the exact missing receipt. Expected direct
costs are staging compute/object storage, offsite storage/egress, and the
chosen alerting service; no paid resource is created by this repository without
the supplied authorized credentials.
