<!-- Preserved from a Codex attachment before that scratch copy was deleted.
     This is the governing Merc directive; it belongs in the repository, not
     in a transient attachments directory. -->

````text
MERC FINAL SHIPPABILITY, PIPELINE INTELLIGENCE, PERFORMANCE, PRICING, AND MARKET-DOMINANCE DIRECTIVE

ROLE

You are the primary implementation agent for Merc.

Grok is available as an independent analysis, adversarial review, benchmarking, architecture, and audit tool.

Use Grok continuously as a feedback loop, not as a substitute for implementation.

The objective is to bring Merc from its current partially canary-proven state to a complete, production-grade managed AI execution platform whose routing, pricing, execution, verification, settlement, developer experience, and operational reliability are all independently defensible.

Do not optimize for activity, commit count, route count, test count, or documentation volume.

Optimize for real product proof.

Do not stop after one audit or one implementation cycle.

Continue the constructive loop until:

1. every launch-critical lane is at least CANARY_PROVEN;
2. the complete pipeline has been independently graded and materially improved;
3. the pricing system is demonstrably sustainable and competitive;
4. Merc has repeatable workload-placement superiority;
5. no known high-leverage architectural, economic, security, or reliability issue remains unaddressed;
6. remaining blockers are genuinely external and returned as one consolidated action queue.

CURRENT VERIFIED BASELINE

Preserve and verify this current truth before changing it:

- Local Metal execution works.
- Realtime, batch inference, and embeddings have real local execution evidence.
- Gateway overhead is NOT currently measured. The artifact this line used to rest
  on, `evidence/perf/gateway-parity.json`, is `INVALIDATED_PENDING_RERUN`, and the
  only parity artifacts that exist at this commit are `HARNESS_SELF_TEST` with
  `comparable: false`. Until a bound parity receipt exists, treat gateway overhead
  as unproven rather than small.
- Supplier microunit accrual conserves money.
- Finite charge retries exist.
- Reuse billing classes distinguish physical and nonphysical work.
- Exact-result cache primitives exist.
- In-flight request coalescing has eliminated duplicate inference in synthetic concurrent traffic.
- Prefix-aware routing exists.
- Fuzzing, concurrency stress, mutation testing, and randomized money state-machine testing are present.
- Stripe credentials authenticate.
- Stripe sandbox currently functions in CAD and should remain CAD for test authority.
- RunPod credentials authenticate.
- RunPod worker startup and durable CUDA enrollment remain incomplete or only partially proven.
- Merc currently admits only the job classes proven by runtime authority.
- TensorParallelSize and PipelineParallelSize fields alone do not prove multi-device scheduling or execution.
- VisionMCP is outside the Merc product boundary and must not be modified or counted.
- Historical inflated throughput claims are retired.
- Physical inference, delivered-token efficiency, reuse, and outcome efficiency must remain separate authorities.

Revalidate the baseline from code, receipts, tests, runtime, and current infrastructure before relying on it.

MISSION

Build Merc into the strongest practical bounded AI execution platform.

The complete pipeline must be able to:

1. understand what the buyer is asking to execute;
2. classify the workload accurately;
3. estimate compute, memory, storage, network, latency, and verification requirements;
4. determine whether one device is sufficient;
5. determine whether tensor, pipeline, data, expert, frame, sample, or task parallelism is appropriate;
6. select a compatible runtime;
7. select an appropriate hardware topology;
8. produce a truthful quote and price ceiling;
9. schedule the workload;
10. acquire and cache inputs and models;
11. execute reliably;
12. verify the output or outcome;
13. charge the buyer;
14. create supplier payables;
15. preserve positive Merc contribution;
16. recover or refund failures;
17. expose an inspectable receipt;
18. improve from every benchmark and failure.

The user should not need to understand GPU rental, runtime configuration, model memory, tensor parallelism, container operations, batching, verification, supplier trust, or settlement.

Merc must convert a requested outcome into a correctly planned, priced, routed, executed, verified, and settled result.

PRIMARY CONSTRUCTIVE LOOP

Repeat this loop continuously:

1. INSPECT
   - Read the current repository, schemas, routes, SDKs, agents, runtime profiles, schedulers, pricing rules, receipts, operations, and benchmark evidence.
   - Distinguish:
     IMPLEMENTED
     WIRED
     TESTED
     REAL_RUNTIME_PROVEN
     CANARY_PROVEN
     PRODUCTION_PROVEN
     DOCUMENTED_ONLY
     DEAD
     EXTERNALLY_BLOCKED

2. AUDIT WITH GROK
   - Give Grok a bounded audit contract.
   - Require citations to exact code, schema, runtime evidence, receipts, or measurements.
   - Require a score from 0–10 for each category and subcategory.
   - Require explicit missing evidence.
   - Require a concrete solution set.
   - Require Grok to distinguish fact, inference, proposal, and unknown.
   - Require at least one adversarial challenge to every major conclusion.

3. VERIFY GROK
   - Independently verify Grok’s claims.
   - Reject false positives.
   - Correct weak framing.
   - Do not implement unverified recommendations.

4. PRIORITIZE
   Rank work by:

   expected buyer value
   × expected revenue or savings
   × probability of success
   × blocker reduction
   ÷ implementation cost
   ÷ operational risk

5. SPLIT IMPLEMENTATION
   - Assign Grok an independent, nonconflicting implementation or audit tranche where useful.
   - Keep the highest-risk money, authority, routing, and lifecycle work under direct review.
   - Use isolated worktrees and isolated databases.
   - Never allow parallel agents to mutate the same authoritative state or test database.

6. IMPLEMENT
   - Complete a coherent vertical slice.
   - Remove obsolete mocks and stubs when replaced.
   - Preserve migrations, receipt verification, and rollback.
   - Do not leave interface-only backends.

7. PROVE
   - Unit tests.
   - Integration tests.
   - Race tests.
   - Mutation tests where authority matters.
   - Fuzzing where parsers, billing, scheduling, identity, and state machines matter.
   - Real runtime.
   - Real hardware where applicable.
   - Failure injection.
   - Economic reconciliation.
   - Canary evidence.

8. BENCHMARK
   - Compare direct runtime versus Merc-wrapped runtime.
   - Record workload, hardware, runtime, model, revision, precision, batch, latency, throughput, quality, cost, supplier contribution, Merc contribution, and failure rate.
   - Never merge physical, delivered, cached, or outcome-equivalent measurements.

9. RE-GRADE
   - Ask Grok to independently re-grade only the affected categories.
   - Require evidence for every score increase.
   - Reject score inflation from documentation or test-only progress.

10. CONTINUE
   - Select the next highest-value blocker.
   - Do not pause for ordinary engineering decisions.
   - Pause only for genuinely external approvals, secrets, paid infrastructure authorization, legal decisions, or destructive production actions.

COMPLETE PIPELINE AUDIT

Create and maintain a scored pipeline matrix.

Use at least the following categories and subcategories.

CATEGORY 1 — REQUEST INTAKE AND WORKLOAD CLASSIFICATION

1. Natural-language or structured request interpretation.
2. API route and manifest validation.
3. Model-specific versus outcome-specific intent.
4. Workload classification:
   - realtime generation;
   - batch generation;
   - embeddings;
   - reranking;
   - classification;
   - structured extraction;
   - image generation;
   - video generation;
   - fine-tuning;
   - evaluation;
   - rendering;
   - multimodal preprocessing;
   - model onboarding.
5. Determinism detection.
6. Exact-result cache eligibility.
7. Prefix-reuse eligibility.
8. In-flight coalescing eligibility.
9. Verification-policy selection.
10. Latency-class selection.
11. Data-rights and privacy classification.
12. Invalid or ambiguous request handling.

Target: 10/10.

Merc must not route based only on a caller-supplied job_type string without independently validating workload shape and requirements.

CATEGORY 2 — COMPUTE-WEIGHT ESTIMATION

1. Input token estimation.
2. Output token estimation.
3. Context-length requirements.
4. Model-weight memory.
5. KV-cache memory.
6. Adapter memory.
7. Image/video tensor memory.
8. Training memory.
9. Optimizer state.
10. Activation memory.
11. Render memory.
12. Temporary storage.
13. Network transfer.
14. Expected duration.
15. Expected power or provider cost.
16. Verification cost.
17. Failure/retry reserve.
18. Confidence range.
19. Historical estimate calibration.
20. Estimate versus actual reconciliation.

Create a versioned Compute Plan:

```json
{
  "workload_class": "...",
  "model_revision": "...",
  "runtime_candidates": [],
  "minimum_vram_bytes": 0,
  "preferred_vram_bytes": 0,
  "host_ram_bytes": 0,
  "storage_bytes": 0,
  "network_bytes": 0,
  "expected_input_units": 0,
  "expected_output_units": 0,
  "expected_seconds": 0,
  "expected_cost": 0,
  "maximum_cost": 0,
  "parallelism": {},
  "verification_cost": 0,
  "confidence": 0
}
````

Target: estimate errors low enough that OOM, underquoting, and unnecessary expensive placement become exceptional rather than ordinary.

CATEGORY 3 — RUNTIME AND HARDWARE COMPATIBILITY

1. Model architecture support.
2. Runtime support.
3. CUDA version.
4. Driver.
5. Metal support.
6. GPU architecture.
7. GPU memory.
8. CPU architecture.
9. Host memory.
10. Storage.
11. Quantization.
12. Context.
13. LoRA compatibility.
14. Multimodal compatibility.
15. Tensor-parallel support.
16. Pipeline-parallel support.
17. Data-parallel support.
18. Expert-parallel support.
19. Network-topology requirement.
20. Runtime-version pinning.
21. Model and tokenizer revision pinning.
22. Health and readiness.
23. Startup and warm-state estimates.

No scheduler route may rely on unsupported profile fields alone.

Compatibility must be physically proven.

CATEGORY 4 — PLACEMENT AND DELEGATION

1. Single-device fit.
2. Single-host multi-GPU.
3. Multi-host eligibility.
4. Tensor-parallel selection.
5. Pipeline-parallel selection.
6. Data-parallel selection.
7. Expert-parallel selection.
8. Frame or sample parallelism.
9. Batch partitioning.
10. Artifact locality.
11. Prefix locality.
12. Adapter locality.
13. Region.
14. Buyer privacy.
15. Supplier trust.
16. Price ceiling.
17. Deadline.
18. Queue depth.
19. Runtime startup.
20. Fault domain.
21. Fallback.
22. Explainability.
23. Rebalancing.
24. Drain and maintenance.
25. Preemption.

Build a generic Placement Plan rather than hardcoding RunPod.

Example:

```json
{
  "topology": "SINGLE_HOST_TENSOR_PARALLEL",
  "runtime": "vllm",
  "device_count": 4,
  "required_vram_per_device": 80000000000,
  "interconnect": "NVLINK_PREFERRED",
  "model_sharding": "TENSOR_PARALLEL",
  "artifact_strategy": "LOCAL_CACHE",
  "verification_strategy": "V1",
  "fallbacks": []
}
```

Target: Merc chooses the least expensive topology that satisfies quality, latency, reliability, and verification requirements.

CATEGORY 5 — PRICING

Audit and revise every layer.

1. Supplier cost.
2. Provider cost.
3. Electricity where supplier-owned.
4. Hardware depreciation where applicable.
5. Runtime efficiency.
6. Startup cost.
7. Model-cache residency.
8. Storage.
9. Egress.
10. Verification.
11. Retry reserve.
12. Refund reserve.
13. Payment-processing allocation.
14. Support reserve.
15. Fraud reserve.
16. Merc contribution.
17. Supplier contribution.
18. Buyer savings.
19. Uncached input.
20. Reused input.
21. Exact cache.
22. Generated output.
23. Reused result.
24. Training attempt fee.
25. Training success fee.
26. SLA premium.
27. Spot discount.
28. Reserved capacity.
29. Multi-GPU premium.
30. Market reference quality.
31. Quote confidence.
32. Actual-versus-estimated reconciliation.

Pricing must satisfy:

```text
supplier_or_provider_cost covered
Merc contribution positive
buyer price competitive where sustainably possible
no hidden subsidy
currency explicit
```

Do not set prices from the minimum of unqualified market observations.

Use governed, normalized, confidence-weighted evidence.

Require a 10/10 pricing audit before launch.

A 10/10 pricing score means:

* no known conservation bug;
* no orphaned accrual;
* no unbounded retry;
* no cross-currency arithmetic;
* no negative unsponsored profile;
* no misleading take-rate claim;
* no stale unsupported competitor input;
* quote error measured;
* margins measured per workload;
* refund and transfer behavior tested;
* pricing decisions explainable and receipted.

CATEGORY 6 — EXECUTION

1. Admission.
2. Queueing.
3. Leasing.
4. Runtime launch.
5. Artifact acquisition.
6. Model loading.
7. Warm state.
8. Streaming.
9. Cancellation.
10. Timeout.
11. Retry.
12. Fallback.
13. Partial result.
14. Checkpoint.
15. Idempotency.
16. Worker failure.
17. Runtime failure.
18. OOM.
19. Multi-GPU failure.
20. Output finalization.
21. Artifact upload.
22. Resource teardown.
23. Orphan detection.
24. Cost control.

Every execution lane must use real runtime evidence.

CATEGORY 7 — PARALLEL EXECUTION

Create separate proof for:

1. Realtime request concurrency.
2. Continuous batching.
3. Independent batch splitting.
4. Shared-prefix batch.
5. Embedding sharding.
6. Image sample parallelism.
7. Video frame/segment parallelism.
8. Blender frame/tile parallelism.
9. LoRA data parallelism where supported.
10. Tensor parallelism.
11. Pipeline parallelism.
12. Expert parallelism.
13. Multi-host coordination.
14. Result aggregation.
15. Failure of one shard.
16. Partial retry.
17. Settlement across multiple suppliers.
18. Verification of aggregated output.

Do not treat tensor_parallel_size stored in a profile as implementation proof.

Prove at least single-host multi-GPU physically through RunPod.

Treat public-internet cross-owner model sharding as experimental unless latency, reliability, trust, and economics pass.

Prefer splitting independent tasks over splitting one tightly coupled model across unrelated suppliers.

CATEGORY 8 — VERIFICATION

1. Runtime identity.
2. Model identity.
3. Input commitment.
4. Stream commitment.
5. Output commitment.
6. Usage reconciliation.
7. Artifact validity.
8. Schema validation.
9. Deterministic replay.
10. Sample replay.
11. Redundant execution.
12. Buyer evaluator.
13. Fine-tune evaluation.
14. Render verification.
15. Multi-shard aggregation.
16. Supplier fraud.
17. Honeypots.
18. Verification cost.
19. Dispute authority.
20. Receipt verification.

Target: a supplier cannot create settlement authority by reporting success.

CATEGORY 9 — SETTLEMENT

1. Buyer authorization.
2. Buyer ledger debit.
3. CAD currency authority.
4. Supplier payable.
5. Provider cost.
6. Platform contribution.
7. Verification charge.
8. Storage/egress.
9. Refund.
10. Credit.
11. Transfer.
12. Payout.
13. Reversal.
14. Dispute reserve.
15. Chargeback.
16. Retry.
17. Manual review.
18. Idempotency.
19. Reconciliation.
20. Multi-supplier allocation.
21. Rounding.
22. Microunit conservation.

Use CAD for sandbox.

Do not block test authority on USD.

Keep currency explicit so live currency can change later.

CATEGORY 10 — DEVELOPER EXPERIENCE

1. OpenAI compatibility.
2. Python SDK.
3. TypeScript SDK.
4. Streaming.
5. Idempotency.
6. Typed errors.
7. Artifact upload.
8. Endpoint management.
9. Model onboarding.
10. Fine-tune workflow.
11. Receipts.
12. Webhooks.
13. Examples.
14. CLI.
15. API stability.
16. Migration path.
17. Documentation accuracy.

CATEGORY 11 — BUYER APPLICATION

1. Sign-up and organization.
2. Projects.
3. API keys.
4. Models.
5. Endpoints.
6. Jobs.
7. Artifacts.
8. Fine-tunes.
9. Media generation.
10. Spend.
11. Budgets.
12. Receipts.
13. Refunds.
14. Disputes.
15. Webhooks.
16. Usage and performance.

CATEGORY 12 — SUPPLIER APPLICATION

1. Enrollment.
2. Hardware.
3. Agent installation.
4. Benchmark.
5. Profiles.
6. Cache.
7. Jobs.
8. Utilization.
9. Earnings.
10. Payables.
11. Payouts.
12. Reputation.
13. Failures.
14. Drain.
15. Maintenance.
16. Economics calculator.

CATEGORY 13 — OPERABILITY

1. Production deployment.
2. DNS.
3. TLS.
4. Health.
5. Readiness.
6. Version.
7. Database.
8. Object storage.
9. Backups.
10. Restore.
11. Alerts.
12. Paging.
13. Runbooks.
14. Status.
15. Rollback.
16. Soak.
17. Incident exercise.
18. Cost monitoring.
19. Orphan cleanup.
20. Capacity control.

CATEGORY 14 — SECURITY

1. Authentication.
2. Authorization.
3. Default deny.
4. Tenant isolation.
5. Worker isolation.
6. Artifact isolation.
7. Secret handling.
8. Webhook security.
9. SSRF.
10. Egress.
11. Model safety.
12. Remote code.
13. Supply chain.
14. Dependency scanning.
15. Fuzzing.
16. Mutation tests.
17. Fraud defense.
18. AI-assisted review.
19. Independent penetration test.
20. Retest.

CATEGORY 15 — LEGAL AND GOVERNANCE

1. Merc naming.
2. Repository licensing.
3. SDK/agent licensing.
4. Model licensing.
5. Dataset rights.
6. Privacy.
7. Buyer terms.
8. Supplier terms.
9. Refund policy.
10. Stripe structure.
11. Tax.
12. Payouts.
13. Attribution.
14. Retention.
15. Incident notification.

SCORING

For every category and subcategory, Grok must give:

* score 0–10;
* evidence;
* status;
* defect;
* proposed solution;
* implementation cost;
* expected score after implementation;
* dependency;
* external blocker if any.

Score meanings:

0: absent
1–2: conceptual or broken
3–4: partial implementation
5: usable in controlled development
6: tested but incomplete
7: real-runtime proven
8: canary-worthy
9: production-grade
10: no known material change remains under current scope

A score of 10 requires independent adversarial review and evidence that further obvious changes are not available.

Do not assign 10 because the checklist is long.

COMPETITIVE PERFORMANCE PROGRAM

The objective is not to manufacture a universal “50× better” claim.

The objective is to discover and prove specific workload classes where Merc delivers order-of-magnitude advantages.

Create competitor-normalized benchmarks for:

1. open chat;
2. high-concurrency chat;
3. batch generation;
4. shared-prefix batch;
5. repeated eval sweeps;
6. retry storms;
7. structured extraction;
8. corpus embedding;
9. exact duplicate bursts;
10. LoRA outcome contracts;
11. image batches;
12. rendering.

Compare:

* buyer price;
* physical compute;
* delivered work;
* latency;
* success;
* verification;
* refunds;
* supplier economics;
* total operating burden.

Target ladders:

* 2× better where commodity execution dominates;
* 5× better through routing, batching, and reuse;
* 10× better on eligible repeated-work workloads;
* 50× better only where exact coalescing, cache reuse, outcome settlement, or task specialization genuinely supports it.

Every claim must state:

* workload;
* model;
* hardware;
* runtime;
* quality tier;
* cache/reuse;
* physical work;
* delivered work;
* competitor basis;
* benchmark date;
* receipt.

A verified 10× cost advantage on one valuable workload is better than a false 50× platform-wide claim.

RUNPOD EXPERIMENT PROGRAM

Use RunPod aggressively but with budget limits.

Experiments:

1. Minimal CUDA diagnostic pod.
2. Merc agent enrollment.
3. Single-GPU vLLM.
4. Direct-vLLM versus Merc parity.
5. Embeddings.
6. Batch.
7. Image generation.
8. LoRA training.
9. Tensor-parallel multi-GPU.
10. GPU cost/performance tournament.
11. Worker failure.
12. Pod startup latency.
13. Model-cache effect.
14. Artifact transfer.
15. Orphan cleanup.

For every pod:

* immutable image;
* exact configuration;
* maximum budget;
* startup timeout;
* idle timeout;
* automatic teardown;
* logs;
* cost;
* result.

Do not create unlimited pods.

RUNTIME TOURNAMENT

For each high-value profile, compare where available:

* MLX;
* llama.cpp;
* upstream vLLM;
* TensorRT-LLM;
* owned Merc runtime.

Select based on:

* quality;
* TTFT;
* ITL;
* throughput;
* energy;
* provider cost;
* startup;
* model breadth;
* reliability;
* maintenance.

Do not replace vLLM globally merely because another engine wins one profile.

PRICING ALGORITHM FINALIZATION

Build a Pricing Decision object that binds:

```json
{
  "compute_plan": "...",
  "placement_plan": "...",
  "supplier_cost": 0,
  "provider_cost": 0,
  "verification_cost": 0,
  "storage_cost": 0,
  "egress_cost": 0,
  "payment_cost": 0,
  "risk_reserve": 0,
  "Merc_contribution": 0,
  "buyer_price": 0,
  "market_reference": [],
  "currency": "CAD",
  "confidence": 0,
  "policy_revision": "..."
}
```

Require invariant tests:

* price never below sustainable floor without subsidy;
* reuse never costs more than uncached;
* exact cache is cheaper than fresh execution;
* supplier/provider cost covered;
* Merc contribution positive;
* currency never mixed;
* rounding cannot overcharge;
* quote and settlement reconcile;
* stale market observations cannot dominate;
* one low-quality source cannot set the market;
* refunds cannot exceed original charge;
* supplier payable cannot exceed verified authority.

Ask Grok to perform a final independent pricing audit and attempt to find any remaining lever or defect.

Do not declare 10/10 until it fails to find a material unaddressed change and the findings are independently checked.

FINAL SHIPPABILITY REQUIREMENTS

Merc is shippable only when the following are at least CANARY_PROVEN:

* realtime OpenAI-compatible inference;
* batch generation;
* embeddings;
* object storage;
* model cache;
* image generation;
* LoRA training and independent evaluation;
* adapter deployment;
* external-model validation;
* single-host multi-GPU;
* buyer dashboard;
* supplier console;
* Python SDK;
* TypeScript SDK;
* receipt-backed price board;
* CAD Stripe sandbox;
* supplier payable;
* refund;
* dispute;
* production deployment;
* backup restore;
* alert delivery;
* incident handling;
* security review.

Independent pentest and legal approvals may remain EXTERNALLY_BLOCKED but must have a complete package ready.

FINAL CANARY

Run one comprehensive canary:

1. OpenAI client sends realtime request.
2. Merc classifies workload.
3. Compute plan generated.
4. Price generated.
5. RunPod worker selected.
6. vLLM executes.
7. Stream delivered.
8. Usage reconciled.
9. CAD buyer debit recorded.
10. Supplier payable recorded.
11. Merc contribution positive.
12. Receipt verifies.
13. Batch runs.
14. Embeddings run.
15. Image generation runs.
16. LoRA trains and evaluates.
17. Adapter deploys.
18. Multi-GPU model runs.
19. Failure refunds.
20. Worker crashes and recovers.
21. Backup restores.
22. Alert reaches paging destination.
23. Dispute packet generates.
24. No orphan pod remains.
25. Every cost reconciles.

Run a sustained mixed-workload soak.

AGGRESSIVE RULES

* Prefer real execution over more documentation.
* Prefer fixing the current highest-value blocker over adding breadth.
* Use Grok for continuous independent review.
* Verify Grok.
* Split parallel work safely.
* Use isolated databases and worktrees.
* Do not let agents interfere with one another.
* Delete dead code.
* Replace mocks with real paths.
* Keep CI and race tests green.
* Preserve money conservation.
* Preserve receipt verification.
* Record failed experiments.
* Tear down paid infrastructure immediately after use.
* Do not ask permission for ordinary work.
* Do not inflate scores.
* Do not inflate benchmarks.
* Do not claim implementation from fields, schemas, or docs alone.
* Do not start the minimum-LOC refactor until shipping behavior is frozen.

REPORTING FORMAT

After every constructive cycle:

1. Category attacked.
2. Previous grade.
3. New grade.
4. Grok findings.
5. Independent verification.
6. Implementation.
7. Real resource used.
8. Product path completed.
9. Performance.
10. Quality.
11. Buyer price.
12. Supplier/provider cost.
13. Merc contribution.
14. Receipt.
15. Failures.
16. Remaining defects.
17. Next tranche.

FINAL STOP CONDITION

Stop only when:

* all launch-critical categories are 8 or higher;
* pricing, money correctness, verification integrity, and execution authority are 9 or higher;
* any category scored 10 has survived independent Grok challenge and direct verification;
* all product lanes are canary-proven;
* at least one real workload demonstrates a sustainable 10× advantage over a normalized alternative;
* any 50× claim is tied to a specific qualified workload and genuine work elimination or outcome efficiency;
* no known material high-leverage change remains;
* all remaining actions require external human, legal, account, or production approval;
* one consolidated external-action queue is produced;
* the complete behavioral truth is frozen for the later deep minimum-LOC refactor.

The objective is not to finish a checklist.

The objective is to make Merc a complete, aggressively verified, economically superior execution platform whose pipeline makes correct decisions from request intake through settlement and whose strongest claims survive independent attack.

```
```
