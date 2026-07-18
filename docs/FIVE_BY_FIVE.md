# Computexchange 5/5 execution contract

The canonical tracker is [`proof/5x5-gates.json`](../proof/5x5-gates.json). It
turns each score into a falsifiable definition and separates work we can finish in
the repository from evidence that requires real hardware, payment rails, or users.

No facet reaches 5/5 because code exists, a demo passed, or a document says it is
ready. A facet reaches 5/5 only when every required gate is proven with its stated
scope. A mock can validate orchestration but cannot satisfy a physical-machine gate;
Stripe test mode can prove idempotency but cannot satisfy a real-payout gate; internal
jobs cannot satisfy an external-market gate.

The report separates `prerequisite` gates (schemas, generators, validators, mock
orchestration, and packaging) from product `outcome` gates. A proven prerequisite
is useful evidence but never adds an outcome point, and the report prints only
`5/5 YES` when every prerequisite, local outcome, hardware outcome, and external
outcome is proven. Intermediate numerators are gate inventories, not facet scores.

The registry also enforces two anti-theater rules at load time:

- every `proven` gate must name a repeatable `command` or `evidence_validator`;
- every unproven gate must name one concrete `next_action`.

That makes the file both a falsifiable scorecard and an execution queue. A static
`state: proven` label with no validator is invalid, and a criticism with no next
engineering or evidence step is not accepted as “direction.”

Run the current report:

```bash
cx prove
```

Run every proof command currently attached to a gate and write a JSONL ledger:

```bash
cx prove --run
```

Each attached-gate run now writes an atomic terminal `source.json` beside the ledger.
It binds the
registry hash, commit, dirty state, and a deterministic hash of every tracked or
non-ignored untracked source file. Reusing an artifact directory replaces the old
ledger rather than mixing runs, concurrent reuse is locked, zero-command runs fail,
and the envelope records selected versus executed gates, overall status, and ledger
hash. `scripts/prove-local.sh` records the same start/end source identity plus an
explicit `contract_only` or `full_local` mode and a terminal status. The ledger
validator rejects stale, partial, failed, wrong-mode, or missing-required-row evidence.

Start/end hashes detect persistent source drift, not a malicious proof command that
mutates and restores a file between samples. Ignored build outputs are therefore
freshly rebuilt in per-run caches and hashed, but a dirty local run remains development
evidence. Release evidence requires the same commands in a clean CI checkout and then
an immutable released commit/artifact; the local harness does not claim to be a
tamper-proof build service.

Limit either operation to one or more facets with `--facet verification`,
`--facet platform_runtime`, and so on. Commands are deliberately sparse: a gate
does not get a command until the command proves the actual acceptance statement.

## Execution order

1. **Stop-ship truth and safety:** supplier ownership, KYC data minimization,
   unpredictable audits, atomic verification-before-settlement, honest per-chunk
   receipts, non-circular facts, and a frozen margin-guard contract. Attempt-level
   retry/loser/dispute/reversal cost coverage remains a separate outcome gate.
2. **Reproducible lanes:** one inventory-driven proof path for two Apple machines
   and any number of explicitly provisioned CUDA workers. Multi-device clustering is
   not required; distinct physical workers completing distinct work is the first bar.
3. **Stranger-ready local product:** installable SDK/CLI, terminal-free supplier
   enrollment, an explicit API contract, signed release inputs, and self-authored
   project policies that do not pretend to be professional legal opinions.
4. **Productize one non-token artifact lane:** transcode first because the final
   artifact can be deterministically re-verified; render follows behind stronger
   anti-quality-shaving evidence.
5. **External proof:** real charge and payout, then a 30-day cohort. The public site
   is intentionally later, matching the current priority; it becomes useful when it
   can point to a working install, policies, receipts, and immutable release.

The registry additionally gives explicit work to the audit gaps that are easy to
hand-wave: colluding smaller-model/precision substitution, long-con detection
bounds, current competitive price and free-local substitutes, supplier net value,
buyer account lifecycle, truthful central-topology language, buyer-demanded CUDA or
container runtime, lane promotion/rollback, held-out artifact generalization,
JavaScript distribution and alias honesty, confidential-compute non-claims, name
collision screening, incident drills, and wedge/repeat-use evidence.

## Hardware plan

- The incoming Mac Studio plus the existing Mac are enough for the first Apple
  distinct-machine gate. They prove independent scheduling, receipts, reconnect,
  and heterogeneity; they do not prove distributed tensor/model clustering unless a
  separate clustered runner is implemented and measured.
- CUDA breadth uses multiple short-lived RunPod workers only after the local/mock
  orchestration proof passes. Inventory must pin pod/host/GPU identity, cap spend,
  collect artifacts before teardown, and fail closed if teardown cannot be verified.
- A 24-hour soak is a later hardware gate. It is never inferred from a short
  benchmark.

## Policy boundary

Policy, terms, and professional-review work are deliberately outside this engineering
release. The proof registry keeps those gates unproven and non-runnable, so an
engineering build neither depends on unstaged policy files nor turns their absence
into a false product-readiness claim. They require a separately authorized pass with
business-owner decisions.
