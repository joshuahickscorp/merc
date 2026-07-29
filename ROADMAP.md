# Roadmap

What Merc is working on next. Every item here comes from something already
recorded in the repository: the open blockers in `ops/go-no-go.json`, the
follow-ups in `ops/readiness.json`, and the known limitations in `docs/`.

No dates. Items marked *(uncertain)* are named as problems but have no committed
plan yet.

## Near term

These five are the only things standing between the current build and a private
pilot that moves real money. None of them can be done from the development
machine; they all need real infrastructure or a real payment account.

- Upload a backup to independent offsite storage and restore from that uploaded
  copy. Only the local drill has passed so far.
- Deploy to a real staging host with TLS and run the full buyer, supplier, and
  operator path there.
- Rehearse a rollback to the previous container image in staging. The rollback
  script has only been syntax-checked.
- Run the full Stripe test-mode money matrix end to end: authorize and capture,
  the actual fee, refund, dispute, payout success and failure, an unknown payout
  outcome, duplicate and out-of-order webhooks, and reconciliation.
- Wire the alert rules to a real on-call receiver and prove that one synthetic
  page is delivered, acknowledged, and resolved. A rule that has never paged is
  not a proven alert.

## Later

- Run sustained concurrent multi-job and restart soak tests. The current proof is
  two agents running a fixed script; cold-start time has been seen to vary from
  about 3.2 seconds to 34 seconds.
- Sign container images and add a container vulnerability scanner, once registry
  credentials exist.
- Drop the unmaintained `paste` macro once Candle's dependency graph allows it.
  It is a warning, not a known vulnerability.
- Build the buyer account features that are still missing: password recovery,
  data export, account deletion, and self-serve activation for new users.
- Pin the agent's outbound traffic to known destinations with a forced egress
  proxy. The macOS sandbox controls direction and ports but not which host the
  agent can reach. *(uncertain)*
- Remotely attest supplier hardware. Today a machine's identity is self-declared
  and only the advertised configuration is checked. *(uncertain)*
- Grow the minimal website into a real buyer dashboard, without inventing
  optimistic status: queued, running, verifying, complete, failed, and cancelled
  must stay distinct, and an unknown payment outcome must show as awaiting
  operator resolution rather than success or failure. *(uncertain)*
- Publish the Python SDK to PyPI. It currently installs from a checkout.
  *(uncertain)*
