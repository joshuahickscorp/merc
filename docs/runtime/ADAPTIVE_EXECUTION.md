# Adaptive execution — capability, activation, verification classes, checkpoint gate

Branch `perf/execution-frontier`, continuing from `579edc0e`.
`release/rc1-go-closure` untouched.

This records what is done and what is not. Every "done" row has a test or a
receipt behind it; every "not done" row says so plainly rather than being
described as partial.

## 0. Capability is now separate from activation policy

**The defect.** Promoting llama.cpp's embed cell from `VALIDATED` to
`REAL_RUNTIME_PROVEN` broke every agent at once. `merc-agent` embeds
`control/runtime-authority.json` at compile time and hashed its **raw bytes**;
the control plane did the same. A lifecycle edit moves those bytes, so the two
started describing different matrices and every enrolment and dispatch was
refused until the whole fleet was rebuilt. A lifecycle promotion had become a
coordinated binary deploy — the exact thing a lifecycle is supposed to avoid.

**The split.**

| Immutable capability manifest | Mutable activation policy |
|---|---|
| profile id, revision, engine + revision, adapter | lifecycle |
| tokenizer revision, chat template, source identity | ordinary routability |
| device, hardware platforms, device count | directed-test eligibility |
| parallelism, runtime features | canary allowlist, traffic percentage |
| per cell: workload, model, runner, wire format, memory floor, verification contract, measured parallelism limits | promotion receipt |
| the exact artifact bytes each cell resolves to | rollback target, policy revision, effective time |

`control/capability_manifest.go` computes the digest over an **explicit**
projection rather than over the struct with a few fields blanked. That direction
is the point: with blanking, every field added later is capability by default and
silently welds itself to the agent binary. That is precisely how the per-cell
lifecycle got into the old digest without anyone deciding it should be there.

The canonical form is line-oriented, not JSON, because the two implementations
are in different languages: Go marshals `float64(2)` as `2` and Rust marshals
`2.0f64` as `2.0`, so a JSON canonicalisation would produce two different digests
for one document and the disagreement would surface as **every agent being
refused**. Numbers are formatted explicitly on both sides and the digest is
pinned in both test suites.

```
capability matrix digest  ea158343b64ea0967b832757c97777b8b2c6f00e8ee27549888e1f25c7bbb2a4
document byte digest      92e6343c16346172ecc6ff32a841ba5607e2d36d8f8c9f452ea780fffff406a6
```

**The last compile-time coupling.** Enrolment used to validate a worker's
declaration against the DIRECTED set, so an agent whose embedded document called
a cell `DRAFT` could not declare it — and promoting that cell by policy therefore
*still* needed the fleet rebuilt first. A worker now declares **capability** and
the control plane authorizes against **policy**. `TestPromotingOutOfDraftNeedsNoNewAgentBuild`
drives exactly that: the same worker is refused before the policy write and
authorized after it, with the capability digest unchanged.

The agent's own filter narrowed to the terminal states only. A
`REJECTED_FOR_CONTRACT` cell stays undeclarable, because measurement decided that
the runtime cannot honour the contract and no policy write reverses a
measurement.

**Activation policy** lives in `runtime_activation_policies`: append-only, one
revision per decision, keyed by capability identity. A rollback **writes
forward** — an undo that erased the intervening revision would leave no record
that the decision was ever taken. The document still declares the *default*
policy for a capability revision, and the sync **seeds** rather than overwrites,
so an operator promotion survives a redeploy.

Three rules are in the database, not only in Go:

- routability is derived from lifecycle, never independently asserted;
- nothing becomes routable without naming the promotion receipt that authorised it;
- policy rows cannot be updated or deleted.

A statement whose capability digest or profile revision no longer matches the
document is **refused at write** (the operator is still there to be told) and
**dropped at read** with the drop recorded — the promotion was granted to a
runtime that no longer exists in that form.

**Migration.** The digest changed meaning, not content, so it must not be
reported as drift. `runtime_profiles.capability_manifest_version` recognises a
row written under v1 and rewrites it; `runtime_profile_digest_history` retains
the old value so anything that recorded it still resolves to the revision it
named. No profile took a new revision for this.

### Found while doing it

- **A cell cannot outrank its profile**, so a cell-only `CANARY` statement is
  floored straight back to `REAL_RUNTIME_PROVEN` and changes nothing. A promotion
  addresses both. The test asserts it rather than working around it.
- **`effective_at <= now()` versus a Go timestamp.** PostgreSQL's `now()` is the
  *transaction* start, so a wall-clock time taken in Go is always fractionally
  later — and policy written and projected in one transaction projected nothing,
  silently. The insert uses `COALESCE($13, now())`.
- **The worker/profile trigger read an arbitrary revision.** `SELECT * INTO p
  FROM runtime_profiles WHERE runtime_profile_id = …` was written when one row
  existed per profile; with history retained, PL/pgSQL's non-STRICT `SELECT INTO`
  takes the first row it finds, so the lifecycle and engine checks were made
  against whichever revision the planner returned. Now scoped to the revision the
  worker binds.
- **A cell-id-keyed projection panics at process start.** The document validator
  deliberately permits two non-routable profiles to describe the same cell id —
  that is how a challenger is registered against an incumbent's cell — and the
  lifecycle-blind capability projection is the first filter that admits both.
  Keyed by `(runtime, cell)` now.

## 1. Verification classes are governed, not sampled

Ordinary buyer traffic is unchanged: reputation-weighted, HMAC-selected per task
id. What did not exist was a way to say *this execution will be verified* without
touching the sampling secret.

```
SAMPLED    ordinary buyer traffic; the only class a buyer may have
REQUIRED   always checked; system and operator work
HONEYPOT   known-answer probe; always checked
REDUNDANT  second execution of a primary chunk; always checked
REPLAY     recorded input against recorded output; always checked
```

The class is bound to the **task**, the **compute plan**, the **verification work
row** and the **receipt**. It is expressed as a probability of `1.0` rather than
as a branch around the sampling machinery, so the pinned sampling row still
records what actually decided the task; the class is stored beside it, so a
reader can tell *certain because governed* from *certain because the supplier is
new*.

A buyer cannot select one. The field does not exist on the wire, and submit
decodes with `DisallowUnknownFields` — a stronger guarantee than a validator that
has to remember to refuse it. `MERC_VERIFICATION_SAMPLE_SECRET` is untouched.

Two rules are in the database: the class may not contradict the probe/redundancy
flags on the task it labels, and a non-ordinary class may not be pinned
`selected = false`.

## 2. `merc dev checkpoint`

This exists because a knowingly unverified checkpoint was pushed: the commit was
chained after a test run with `&&` in a way that did not gate on the result, the
suite was red, and the push went out. That is not a discipline problem — a shell
pipeline whose failure mode is "push anyway" will be typed again.

Sequential, as a program rather than a habit:

1. worktree validation (clean tree, not a frozen branch, HEAD recorded);
2. targeted authority tests, first, so a regression is reported in seconds;
3. mutation suite — which modifies the tree;
4. **proof** that the mutation runner restored it, by content digest over every
   tracked file. `git status --porcelain` would usually catch it; hashing the
   bytes answers the question actually being asked — *is this the source that was
   tested*;
5. full CI, only now that the tree is proved restored;
6. race suite over the concurrency the money path has;
7. HEAD and worktree digest unchanged;
8. receipt at `evidence/checkpoint/<sha>.json`.

A receipt with a skipped or failed step **authorizes nothing**. Recording the
skip and treating the receipt as complete would be the same mistake with better
paperwork. `.githooks/pre-push` calls `merc dev checkpoint-verify`; the logic is
in the CLI so the same rule can be run by hand or by CI without depending on
anyone's local hook configuration. Remote CI remains authoritative.

Receipts are **not** committed. A receipt is bound to a commit hash, so
committing one would create a commit that itself has no receipt, and the hook
would refuse the very commit carrying the proof.

### What the gate found on its first run

`make ci` was already red at `579edc0e`, in five places, none of them noticed
because nothing ran the whole target:

- **the route-count tripwire was stale.** `validate-authorization-matrix.py`
  asserts exactly 81 reviewed routes; 82 have been registered since
  `GET /admin/plan-accuracy` landed in `b0004f00`. The route WAS added to the
  matrix — only the constant was missed. Because `validate-readiness.py` scores
  `auth_matrix_complete` for 3 points under source-and-CI and 8 under security,
  the stale constant silently cost **11 readiness points**, and the declared score
  of 83 had been reading as a receipt-derived 72 ever since.
- **the MiniLM GGUF was never recorded in `ops/model-provenance.json`.** It was
  added to the runtime authority and pinned in `agent/src/models.rs`, and the
  governance validator wants all three to agree.
- **`cargo clippy -- -D warnings` was red** on five dead-code items and a
  `mod tests` with public items after it. The dead code is the `RuntimeDriver`
  boundary's unused half (`validate`, `cancel`, `drain`), the GGUF embed spec, and
  the supervised-launch arm of `LlamaServerSupervision` — all deliberate shape
  rather than leftovers, so each now carries an explicit allow and a reason
  instead of a warning nobody reads.
- **`assert-no-test-skips.sh` was red** because every test that needs object
  storage or a local engine skips, and the allowlist had two entries. Those
  skips are now named — 36 of them — which is the honest cost of a gate that
  lists every skip rather than accepting a category. `make ci` also now passes
  `MERC_TEST_S3_*` and `MERC_LLAMA_EMBED_URL` through when the environment has
  them, so a machine with MinIO and a llama-server actually runs those tests.

All five are fixed. The point is not the fixes; it is that "full suite green" at
the last checkpoint meant `go test ./...`, and the gate is what makes the
difference visible.

## 3. The llama.cpp failure matrix

`REAL_RUNTIME_PROVEN` was recorded with an explicitly incomplete failure matrix
and the promotion receipt said so. A happy path proves a runtime can earn money;
it says nothing about what happens when the agent dies holding a claim, when an
upload never lands, when the verifier restarts mid-decision, or when settlement
is retried. Every one of those is a place money can be created or destroyed.

Every case asserts the same nine properties through one helper, so a case cannot
quietly check less than its neighbours: one authoritative state from a closed set,
no duplicate buyer debit, no duplicate supplier payable, **nothing standing as
payment** for undelivered or rejected work, no leaked task lease, no leaked
verification lease, no artifact authority without a digest, bounded retry, and an
actionable diagnostic.

| Case | Driven by | Result |
|---|---|---|
| agent death before claim | real process, killed | queued, claimable, 0 USD |
| agent death after claim | real process, killed mid-claim | requeued once, 0 USD |
| runtime unreachable | real agent against a dead port | no commit, 0 USD |
| duplicate commit | second identical commit | refused, one payable |
| output upload interrupted | commit naming an absent object | no verification, 0 USD |
| result digest mismatch | well-formed digest of other bytes | work outcome `fail`, 0 USD |
| verifier restart | two passes over one attempt | one terminal outcome |
| finalizer + settlement retry | three finalizations | one debit, one payable |
| expired lease | seven expiries | requeued, 0 USD |
| input download failure | input object removed | requeued, 0 USD |
| cancellation before execution | queued job cancelled | cancelled, 0 USD |
| cancellation during execution | running job | refused, 0 USD |
| database restart | every backend terminated | recovers, no double count |
| receipt-generation failure | plan removal attempted | refused by the schema |

The **supplier invariant is the net, not the row count**. The supplier is credited
when verification settles and the grade can arrive afterwards, so a clawback
legitimately leaves two rows summing to nothing; asserting "exactly one row" would
call correct behaviour wrong in one direction and miss a real double-payment in
the other.

Three things the matrix found about the harness rather than the product, all of
which are the schema defending itself and were adopted rather than worked around:

- writing the execution identity columns directly is refused — *"task execution
  identity is immutable outside claim transition"* — so a claim has to come from
  the claim path;
- a job cannot go `running -> queued`, so a cancellation-before-execution case has
  to submit a job rather than rewind one;
- a frozen compute plan cannot be removed at all, which is a stronger answer to
  "what if the receipt authority is missing" than any handling would be.

Not driven: runtime **crash** mid-execution (as opposed to unreachable), model
artifact missing and model digest mismatch at the agent's download step, and
stale-attempt/tiebreak interactions beyond the existing
`TestStaleAttemptOutputIsNotVerified`. Those need the agent's download path
instrumented, not the control plane.

## Not done

| Section | State |
|---|---|
| 4. RuntimeSelector in shadow mode | **not done** |
| 5. Verified-outcome scoring | **not done** |
| 6. Paired selector evidence | **not done** |
| 7. Selector promotion gate | **not done** |
| 8. Coalescing proved through real money | **not done** — coalescing works and is concurrency-tested; the 128-request money proof through real runtime execution is not taken |
| 9. Tokenization / tool-schema caches | **not done** |
| 10. Token-budget batching sweep | **not done** |
| 11. vLLM CUDA as a governed cell | **not done** |

`llama_cpp_metal`'s embed cell remains `REAL_RUNTIME_PROVEN` and directed-only.
Candle remains `ACTIVE`. llama.cpp byte-exact generation remains
`REJECTED_FOR_CONTRACT`. Ordinary routing has not changed.

## Grok

`DEFERRED_USAGE_LIMIT`. Not auth-blocked and not awaiting an external action. The
five queued audits keep their contracts and are re-runnable when usage returns.
The direct adversarial work here is marked `NOT_GROK_INDEPENDENT`: the
capability/activation split was reviewed by writing the mutation tests that fail
if either half leaks into the other, and by pinning the cross-language digest on
both sides so a divergence is a test failure rather than a production refusal.
