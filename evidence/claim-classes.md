# Merc claim classes at HEAD `83806ccf`

Four classes, stated separately, never mixed into one number. A class Merc
cannot currently support is written `NOT CLAIMED` rather than omitted. This
file is a claim ledger, not a licence to quote any of these figures as
present-tense production capacity except where a class explicitly says so.

Read at HEAD `83806ccf805b1bc40eddd35383c2230d63d1e1e6` (2026-08-24).
`evidence/perf/**` is Git LFS. Working-tree files are pointers; numbers below
were read from `LocalMediaDir` objects whose `oid sha256` matches the pointer.
A clone without that cache must treat the same files as unreadable.

Governing archaeology (read first, before any `evidence/perf/` citation):
`evidence/hyper-atlas-archaeology.md` at
`fd0546829d06d92b99e1119953fdf9aa25ff8474`
("perf: three read-only audits of what Merc actually knows about itself").
That audit's body names a worktree HEAD `4b10b56e` that is **not in this
object store**. Its artifact classifications (UNBOUND / WITHDRAWN /
SUPERSEDED / subject-code CHANGED) still apply to every `evidence/perf/`
receipt it named, because those receipts have not been re-measured at this
HEAD. Two post-audit facts this ledger uses instead of the audit's then-HEAD
numbers:

- the sealed llama infer cell is now r7, not r6;
- `evidence/autonomous/hardware-characterization.json` was regenerated at
  the freeze candidate `c0c9e3fc` (the audit's `fa47cafe` SHA is not a git
  object here).

**Rule used throughout:** a number without its measurement contract does not
support a claim. An artifact the archaeology marks stale is not used as
present-tense dominance. Where SCALE cites an UNBOUND/CHANGED demonstration
anyway, the justification is in that section.

---

## Known-stale case: `evidence/perf/latency-atlas.json`

Verified before any other `evidence/perf/` citation.

```
git log --oneline -1 -- evidence/perf/latency-atlas.json
435d013e deferral: the buyer paid for speed, and the claim path never read the promise
```

Full file commit: `435d013e644c544db1123f09ccea19e2f69e2c9b` (2026-08-09).
`git rev-list --count 435d013e..HEAD` = **204**. The atlas body's own
`head` field is `74dea5e17dcc44c7790c4e6b41c48bbd59dd0f0d`.
`git rev-list --count 74dea5e1..HEAD` = **1001**. LFS oid
`acbd7980b00d7c3123c19224b05515e844da9b616c8d1d99ae99f776c863b2cf`.

Quoted from the artifact:

- `kind`: `merc_latency_atlas`
- `binding_status`: `UNBOUND`
- `missing_identity_fields`: `source_commit`, `build_digest`,
  `harness_revision`, `corpus_digest`, `exact_config`, `raw_samples`
- `honesty.binding_note`: "UNBOUND on purpose. This atlas is a structural
  census assembled from two read-only reconnaissance reports plus percentiles
  copied out of other evidence files; it has no producer identity of its own
  — no harness revision, no exact config, no raw samples — so it cannot claim
  BOUND. […] Nothing here may be cited as bound authority."

Archaeology Table A: every copied MEASURED stage has subject-code CHANGED;
38 stages remain UNMEASURED. **This ledger does not cite any atlas
percentile as a claim.** It is the known-stale case the instructions named.

---

## Trap: 138.7 tok/s is not the sealed cell

Several docs still quote **138.7 tok/s**. That number is
`batch_32_tokens_per_second = 138.71389521174524` in
`evidence/benchmarks/2026-07-01-m3-pro.json`
(`git log --oneline -1` → `1e006669 feat: evidence that cannot name its
producer is refused at write time and in CI`; full
`1e006669d8d6d86dde678a293292589a026e99cd`). Contract of that receipt:
Apple M3 Pro, candle, llama-3.2-1b-instruct-q4, `binding_status=UNBOUND`,
`missing_identity_fields` includes `source_commit`. Archaeology: this **is**
the 138.7 figure; not what `repricingBenchmarks` uses; subject code CHANGED.

The live catalogue constant is not 138.7 and is not r6's 304.2661.

Sealed llama infer cell, quoted from
`evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r7.json` and from
`control/pricing.go` `repricingBenchmarks[0]` (both last touched
`697713f9 alpha: the agent enrols, and the embed cell measures what Merc
settles`; full `697713f91357176b184de4942658ed7b2a0b7a12`):

| field | value |
|---|---|
| `engine_build_hash` | `0b6a7a9cd7578343` |
| `batch_infer.throughput_units_per_second` | **302.3194** |
| `operating_batch` | 1 |
| `unit` / `unit_scope` | `tokens` / `token_like_input_plus_max_output_tokens` |
| model | `llama-3.2-1b-instruct-q4` |
| `model_artifact_sha256` | `3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1` |
| precision | Q4_K_M GGUF (manifest: `Q4_K_M GGUF`) |
| hardware | `apple_silicon_v1\|brand=Apple M3 Ultra\|model=Mac15,14\|memory_bytes=103079215104\|cpu_cores=28\|gpu_cores=60` |
| engine | candle in-process Metal |
| `thermal_ok` | true |
| harness | `merc-agent bench-batch` (r7 settlement-geometry re-measure) |
| `reps` / `max_tokens` / `prompt_bytes` | 5 / 48 / 59 |
| batch sizes | 1, 8, 32, 64 |
| warm/cold | in-process bench; `warmup_ok=true`; not a serving cold-start |
| `binding_status` | `BOUND` |
| `merc_source_commit` / producer `source_commit` | `c0c9e3fce0fd8d376575d4770de517aba9c42816` |
| `git log 697713f9..HEAD -- agent/src/` | empty (0) |

r6 (`304.2661`, hash `7cc01c442c7f6dbe`) is `validity=SUPERSEDED` in
`control/evidence-manifest.json`. r7 names r2–r6 as superseded. **302.3194
is a single-arm sealed rate, not an engine-dominance ratio.** It is recorded
here so it cannot be silently mixed into any class below. Decode-only
diagnostic at operating batch 1 is `231.2563` tok/s and is not catalogue
authority.

A q4 Metal number and a bf16 CUDA number remain different physical products
unless an `AcceptableQualityContract` makes them substitutable.
`ops/acceptable-quality-contracts.json` (file commit `4a703945`) **refuses**
Metal q4 vs CUDA bf16 for `batch_infer` and authorises embed cosine
substitutability **without** cross-hardware USD ranking.

---

## 1. ENGINE DOMINANCE — `NOT CLAIMED`

**Class:** same hardware, same model, same artifact/precision, same
workload: a Merc runtime beats the alternative.

**Claim Merc can make today:** `NOT CLAIMED`.

No still-true-at-HEAD two-engine ranking exists on one digest, one
precision, one hardware identity, and the verification contract the cell
actually sells. The sealed r7 receipt is one engine. The historical
same-digest tournament is stale, and its speed winner is rejected for the
production infer contract.

### Evidence considered and refused

**A. Host-scope tournament (same GGUF, same Metal host) — stale, and the
winner fails `byte_exact`.**

- File: `evidence/perf/runtime-benchmarks/engine-tournament-metal-host-scope-r2.json`
- File last git commit: `eed5b550 L4: migrate 84 evidence/perf files to git-lfs at tip`
  (`eed5b55085b6bdd96cf1f56fe69b3890dde253f7`). Prefer the receipt's own
  producer SHA, not the LFS-migration touch.
- Producer `source_commit`: `2c8518dc908bda0651706d24e26e6f49acf0659e`
- `binding_status`: `BOUND`; `benchmark_status`: `TWO_ENGINE_RANKED`
- Archaeology Table B: MEASURED, subject code **CHANGED**.
- `git rev-list --count 2c8518dc..HEAD -- agent/src/` = **79**.

Quoted ranking (primary metric `mean_output_tok_per_sec_across_measured_points`):

- winner `llama_cpp` / loser `candle`
- `primary_ratio_winner_over_loser` = **2.074269306868982**
- `primary_ratio_lower_bound` = **1.4584890925709417**
- `all_paired_points_agree` = true
- aggregate mean output tok/s: candle **180.23781004755523**, llama.cpp
  **373.8617573189256**

Physical cells (same ranking, same SHA) live in
`evidence/perf/runtime-benchmarks/serving-matrix-candle-vs-llama-cpp-metal-r1.json`
(file commit also `eed5b550`; producer `2c8518dc`).

**Contract of that measurement:**

| axis | value |
|---|---|
| hardware | Apple M3 Ultra Metal (`hw_class=apple_silicon_ultra`; `hw_attested=false`) |
| model | `llama-3.2-1b-instruct-q4` |
| artifact | sha256 `3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1` |
| precision | `Q4_K_M` on both arms (`RefuseMismatchedModelDigests`) |
| workload | serving-matrix `local_evidence` subset: concurrency {1,8}, prompt_tokens 32, output_tokens 16, state {cold,warm}, lane `interactive` |
| transport | candle `merc-agent serve-openai` vs `llama-server -ngl 99 -np 8 -c 4096` |
| samples | 5 ok / 5 attempted per engine at the warm c=1 point |
| measured_at | 2026-08-03T17:54:11Z |
| unit | output tokens/s over OpenAI SSE samples — **not** settlement geometry `token_like_input_plus_max_output_tokens` |

Warm c=1 point, quoted: candle `output_tok_per_sec=182.77174521355528`,
TTFT p50=24.411 ms; llama.cpp `272.5444660043772`, TTFT p50=12.587 ms.

The same receipts **forbid** reading this as engine dominance under the
product contract:

- `does_not_prove` includes `byte_exact batch_infer for llama.cpp on Metal`
- serving-matrix notes: "llama.cpp batch_infer cell stays
  `REJECTED_FOR_CONTRACT` for byte_exact; this receipt ranks physical
  serving metrics only"
- ranking basis: "Does not use byte_exact or catalogue parity"
- AQC `batch-infer-byte-exact-matched-precision-llama32-1b` eligible cells:
  **only** `candle-metal-llama1-infer`

So the faster arm cannot be sold as the batch_infer product. A speed win
that breaks verification is not ENGINE DOMINANCE on the contract Merc
settles.

**B. Unbound bench-batch comparison — UNBOUND, missing identity, same
verification veto.**

`evidence/perf/runtime-benchmarks/candle-vs-llama-cpp-metal-r3.json`
(file commit `eed5b550`; producer `merc_source_commit=470aa9f81a13d508fd91f4cf0d4c55e57560a337`).
`binding_status=UNBOUND`. Peak ratio llama.cpp/candle **4.314** at batch 64
(candle 495.6 vs llama.cpp 2161.5 tok/s). Verdict, quoted: "llama_cpp_metal
is 4.31x faster at peak and is NOT byte-deterministic under batching. The
batch_infer cell requires byte_exact verification, so throughput alone
cannot promote it." Archaeology: do not quote as today. Serving-matrix r1
explicitly supersedes this file.

**C. Embed two-engine cell — different artifacts, so not this class.**

`evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r3.json`
(file commit `a382fb65`; producer `9e31c65b27860d659d7ce972e2de7052691c0642`;
`binding_status=BOUND`; measured_at 2026-08-17T15:38:58Z; Apple M3 Ultra).
Slowest-of-five at batch 128: candle **3344.095748149461**
embeddings/s (fp32 safetensors
`53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db`) vs
llama.cpp **1443.8485385737333** embeddings/s (F16 GGUF
`797b70c4edf85907fe0a49eb85811256f65fa0f7bf52166b147fd16be2be4662`).
`engine_configuration.llama_cpp_metal.total_slots=4`, and
`not_established.engine_tuning` warns that a candle win at large batch is a
statement about **this** server configuration. Archaeology: CHANGED.
Different precision/artifact ⇒ not ENGINE DOMINANCE even if it were still
true.

**D. Sealed r7 — one arm.** 302.3194 tok/s under the contract in the trap
section. No alternative was measured on that binary.

**E. CUDA / MLX arms.** Tournament `does_not_prove`: "CUDA engine ranking",
"MLX ranking on this GGUF". CUDA historical sweeps
(`cuda-a5000-*.json`, peak 1617.43 / 2841.82 / 7081.56 tok/s) are UNBOUND;
archaeology: **must not quote as today**. They are also a different
precision family than the sealed q4 GGUF.

### What would upgrade `NOT CLAIMED` to claimed

A new BOUND two-engine receipt at this HEAD (or a named freeze candidate)
that holds **all** of:

1. identical model artifact digest (not merely the same model name);
2. identical precision;
3. identical hardware identity string;
4. identical workload (prompt, max tokens, batch/concurrency, warm vs cold);
5. the verification the cell sells (`byte_exact` for llama-3.2-1b-instruct-q4
   batch_infer; `embed-cosine-v2` mean/row ≥ 0.999 for MiniLM embed);
6. producer identity complete (`source_commit`, `build_digest`,
   `engine_build_hash`, harness revision, raw samples);
7. a Merc runtime beats the alternative on the **primary metric the product
   contract names**, with the loser still on that contract.

Until that receipt exists, Merc may not say a runtime is dominant. It may
say a runtime is the only eligible cell on a contract (that is eligibility,
not dominance).

---

## 2. PLATFORM DOMINANCE — `NOT CLAIMED`

**Class:** Merc **chooses** a runtime/hardware combination that beats a
fixed deployment for the accepted product contract.

**Claim Merc can make today:** `NOT CLAIMED`.

Ordinary admission still freezes one advertised cell per (job type, model).
There is no measured win of a Merc-chosen mix over a frozen Metal-only or
CUDA-only deployment on one Acceptable Quality Contract. The software
prereq for honest substitutability exists; the measurement does not.

### Evidence considered and refused

**A. Heterogeneous placement principal — structural refusal, experiment
not run.**

- File: `evidence/perf/heterogeneous-placement-principal-latest.json`
- File last git commit: `eed5b550` (LFS migration). Producer
  `source_commit`: `629f87682eaf4bf7026420883a34a4a62c909892`
- `binding_status`: `BOUND`; `status`: `REFUSED_STRUCTURAL`
- Archaeology: "not a throughput claim"
- `money.spent_usd_this_receipt`: 0.0; no pod created

Quoted verdict: "The heterogeneous placement thesis cannot be measured on
this tree. Ordinary admission freezes a Metal-only singleton; CUDA
generation is DRAFT; no CUDA embed exists; media is not routable; realtime
clearing is not shape-aware; and Metal Q4 vs CUDA bf16 is not the same
quality contract. This is a structural negative for shippability of the
thesis, not a measured win or loss on the mix."

`does_not_prove` includes "that Merc heterogeneous placement beats or loses
to a fixed CUDA or fixed Metal deployment on a mixed workload".

**B. Acceptable Quality Contracts — quality gate, not a beat.**

`ops/acceptable-quality-contracts.json` at `4a703945182bf0b9f60a529b881693dbcbf0e162`
("hetero: acceptable-quality-contract gate for honest Metal/CUDA
substitutability (G024, software prereq)").

- `batch-infer-byte-exact-matched-precision-llama32-1b`: ACTIVE; eligible
  cells **only** `candle-metal-llama1-infer`; Metal q4 only. No hardware
  choice to make.
- `batch-infer-metal-q4-vs-cuda-bf16-REFUSED`: "Metal q4 versus CUDA bf16
  is not one matched quality contract. merc must not rank them on price."
- `embed-cosine-v2-all-minilm-l6-v2`: ACTIVE; Metal and CUDA listed as
  allowed devices; `honest_scope`: "Cross-hardware COST comparison remains
  refused (`control/runtime_cell_cost.go:comparableHardwareFor`). This
  contract authorizes quality substitutability, not USD ranking across
  machines."

AQC is the contract that would make a platform claim *possible*. It does
not itself measure a chosen combination beating a fixed deployment.
Cited measured_evidence paths
(`engine-parity-metal-embed-latest.json`, `cuda-embed-arm-latest.json`,
and the infer contract's `candle-metal-llama1-q4-r4.json`) are archaeology
CHANGED / superseded (r4 superseded by r5 then r6 then **r7**). They are
not used here as a dominance number.

**C. Metal-vs-CUDA embed verdict — WITHDRAWN.**

`evidence/perf/selector/metal-vs-cuda-embed-verdict-latest.json`
(file commit `c9831ed4`; producer `162cecee87d2a67c338307ce974d3feb96ee4312`).
`validity=WITHDRAWN`, `binding_status=WITHDRAWN`. Archaeology: do not quote.
Withdrawal reason, quoted: "The verdict derives measured latency and
saturation claims from two UNBOUND throughput sweeps."

Even before withdrawal, `cost_verdict.status=REFUSED_CROSS_HARDWARE` and
`placement_thesis.status=STILL_REFUSED_STRUCTURAL`.
`throughput_verdict.status=REFUSED_ENGINE_NEVER_SATURATED`: the A40 had
1.23 ms/request queue vs 43.6 ms inference; 206 ms of a 250 ms request was
client/network. No A40 throughput ceiling was established.

**D. Competitive-position crossover — UNBOUND, different products.**

`evidence/competitive-position.json` at
`2a0abd39df5f233dc84f25a3e4c12b3ee1736717`. `binding_status=UNBOUND`.
Quoted shape: "Metal 14.4ms TTFT at concurrency 1 vs CUDA 181ms; CUDA 1617
tok/s aggregate at concurrency 16 vs Metal 211". Same file's correction:
"Different models and precision: 3B Q4_K_M on Metal, 1.5B fp16 on CUDA.
Absolute throughput is NOT comparable." Archaeology: CUDA half must not be
quoted as today; `not_yet_exploited` says nothing routes on workload SHAPE.
This is not a Merc-chosen beat of a fixed deployment.

**E. Power analysis**
(`evidence/perf/heterogeneous-placement-power-analysis-latest.json`):
`status=DESIGN_AND_PRIOR_MEASUREMENT`, UNBOUND. Budget of GPU time/USD to
*authorise* a future measure. Not a win.

### What would upgrade `NOT CLAIMED` to claimed

All of:

1. at least two ordinary-routable cells on **one** ACTIVE AQC (same job
   type, matched artifact/precision or an explicit multi-family metric with
   published threshold);
2. a frozen baseline deployment (single engine/hardware, named);
3. Merc production selection (not shadow-only) choosing among those cells
   per request/shape;
4. a BOUND receipt showing the Merc-chosen mix beats the frozen deployment
   on the contract's primary metric (latency inside SLA, USD per verified
   outcome, or energy per verified outcome — named in advance);
5. the same quality gate applied to both arms, so a cheaper/faster arm that
   fails AQC is not counted as a win.

`heterogeneous-placement-principal-latest.json` `what_would_unblock` is the
structural checklist; a powered CUDA re-run alone does not satisfy it.

---

## 3. NETWORK DOMINANCE — `NOT CLAIMED`

**Class:** Merc completes the buyer's **total verified computation** faster
or cheaper through placement, locality, decomposition, batching and reuse.

**Claim Merc can make today:** `NOT CLAIMED`.

Same-host prefix reuse was measured and is real as an engine cache effect.
The receipt itself refuses a network claim, buyer settlement does not shrink
with cached tokens, and archaeology marks the subject code CHANGED. Arrival
batching is an inconclusive stand-in. Exact reuse is UNMEASURED in the
atlas and is not given a number here.

### Evidence considered and refused

**A. Two-worker prefix — same host, explicit non-claim, CHANGED.**

- File: `evidence/perf/prefix-two-worker-latest.json`
- File last git commit: `2f5d8b3c prefix: two workers, engine-confirmed,
  and an explicit refusal to call it a network result`
  (`2f5d8b3cf523f8866938daa5bdce67660be5b4a9`)
- Producer `source_commit`: `644ccee648be143db58f78c83cf1b5e5a48d975c`
- `binding_status`: `BOUND`; measured_at 2026-08-04T23:54:47Z
- Archaeology: MEASURED (same-host); CHANGED.
  `docs/NETWORK_V2_EXECUTION_PLAN.md` (file commit `2b98aae4`) quotes it
  honestly: "The two-worker bound receipt is same-host process placement,
  not network supremacy."

Quoted deltas (definition: warm minus cold; n=12 interleaved rounds):

- `prompt_ms_p50` = **-409.1605** ms
- `wall_ms_p50` = **-431.8011039867997** ms
- `prefill_tokens_avoided_p50` = **2617.0**
- `gpu_joules_delta_p50` = **-42.5692305** (IOReport AGX GPU domain only)

**Contract:**

| axis | value |
|---|---|
| topology | two llama-server processes on **one Mac**, distinct ports |
| `not_exercised` | two suppliers on two machines; cross-supplier; multi-region; paid cloud |
| hardware | Apple M3 Ultra / 96GB; loadavg at start **31.70 / 26.82 / 23.10** ("quiet machine preferred") |
| model | same GGUF `3f5a2242…` Q4_K_M, ctx 4096, max_tokens 16, 45-paragraph prefixes, 12 rounds |
| quality | temperature 0; `identical_text_all_rounds=true` |
| concurrency | one in-flight request per worker |
| energy USD | `energy_usd_per_request_delta = -1.7737179375000002e-06` at policy default $0.15/kWh — **DEFAULTED-grade, not a metered invoice** |
| supplier entitlement delta | **0.0** (catalogue keyed by model, job_type; duration does not change payout) |

`does_not_prove` includes "a cross-supplier or cross-network advantage:
both workers are processes on this host" and "that production routing
changed". Selector order is CostRank then AskUSDHr **then** WarmPrefixDepth:
warmth cannot chase a cache hit onto a dearer class.

This is locality/reuse on one machine. It is not NETWORK DOMINANCE, and it
is not cheaper for the buyer under current settlement (token billing still
sees full prompt; supplier pay is unchanged; the dollar energy figure is a
policy default).

**B. Physical prefix KV — single local engine, buyer USD not reduced.**

`evidence/perf/prefix-kv-physical-metal-latest.json` (file commit
`eed5b550`; producer `9092fbd47d39a8cdff097dea28e9cc40d5bf81a2`;
`binding_status=BOUND`; measured_at 2026-08-04T07:12:06Z). Archaeology:
CHANGED. Quoted: `prefill_tokens_avoided_p50=2618.0`,
`wall_ms_warm_over_cold_ratio_p50=0.13389930741458997`,
`prompt_ms_warm_over_cold_ratio_p50=0.04526430551951792`.
`does_not_prove`: "buyer USD settlement reduction (token billing still sees
full prompt_tokens; money paths untouched)"; "cross-supplier fleet
behaviour; this is a single local Metal engine".

**C. Arrival batching — stand-in, `INCONCLUSIVE_NULL`.**

`evidence/perf/arrival-batching.json` (file commit `eed5b550`;
`binding_status=UNBOUND`; no `source_commit`; measured_at 2026-08-03T00:38:01Z).
Archaeology: MEASURED stand-in (not a real CB engine); CHANGED; verdict
`INCONCLUSIVE_NULL`. Quoted comparison: peak aggregate tok/s 14003.8 off vs
13041.2 on (**−6.9%**); TTFT p50 48→28 ms at c=64. Stand-in
`cb_aware_prefill_share_v1` "does not model real vLLM / TensorRT-LLM /
llama.cpp continuous-batching schedulers". A null on a stand-in is not
evidence batching is worthless, and it is not a network win.

**D. Atlas `exact_reuse` / `exact_reuse_batch`:** UNMEASURED. Not given a
number. Competitive-position (UNBOUND, 2026-07-27) reported reuse machinery
unreferenced on the request path; that finding is not re-verified here and
is not turned into a performance claim.

### What would upgrade `NOT CLAIMED` to claimed

A BOUND receipt that measures the **buyer's total verified computation**
(wall clock to verified outcome, and USD the buyer actually pays) under:

1. at least two machines / two suppliers (the current `not_exercised` list);
2. Merc placement choosing the warm/local/batched arm rather than a harness
   pinning workers;
3. settlement that reflects saved work if savings are claimed as cheaper
   (cached tokens reaching TaskCommit, or an equivalent verified-unit
   shrink) — physical prefill avoidance with unchanged buyer units is not
   cheaper;
4. the same AQC on both arms;
5. producer identity at HEAD (or a named freeze), after the subject paths
   that have moved since `644ccee6` / `9092fbd4`.

Same-host prefix reuse may be cited as an engine-cache demonstration. It
may not be cited as network dominance.

---

## 4. SCALE — claimed as two historical demonstrations; not claimed as production capacity

**Class:** what Merc has actually demonstrated about **representing many
device states**, stated as exactly what was proven.

**Claim Merc can make today (one sentence):** Merc has demonstrated (1) an
in-process presence index holding 10,000,000 live slots at ~12.41 bytes/device
and ~3.50 million heartbeats/s on one 28-core Mac Studio, and (2) a
droplet-class **PROXY** postgres ingest curve whose implied fleet is 89,107
(baseline) / 184,032 (2 ms coalesced) from `floor(hb/s × 45 s)`, after
seeding 50,000 device rows of which 47,778 were live. Merc has **not**
demonstrated 10 million devices in production postgres, a DigitalOcean
droplet ceiling, or a still-true-at-HEAD re-measure of either receipt.

These two demonstrations are **not** one number. They are not multiplied,
not added, and not quoted as current control-plane capacity. Both artifacts
are `UNBOUND` and archaeology marks subject code CHANGED. They are cited
anyway because this class is "what was proven", not "what is still true at
HEAD", and omitting them would hide the only many-device measurements in
the tree. Archaeology's own instruction is respected: the numbers are not
used as present-tense production capacity.

Current BOUND characterisation of a device is **one** machine
(`evidence/autonomous/hardware-characterization.json`,
`physical_devices_observed: 1`, source_commit `c0c9e3fc`, file commit
`2240d6ed`). That is not SCALE and is not mixed into the figures below.

### Demonstration 1 — in-process LiveDeviceIndex, 10,000,000 slots

- File: `evidence/perf/liveness-index-bench.json`
- File last git commit: `dde956ea alpha: stamp every unstamped receipt,
  which exposes nine citations that were hiding`
  (`dde956ea583707a5f283ced8d74d190702933c1c`) — a stamp, not the
  measurement.
- Artifact `source_commit`: `9c5a9f256c8079e9e8226b15b5d0b01907a14865`
- `binding_status`: `UNBOUND`
- `missing_identity_fields`: `build_digest`, `model_artifact_digest`,
  `image_digest`, `harness_revision`, `corpus_digest`, `exact_config`,
  `raw_samples`
- Archaeology Table B: MEASURED (in-process index, not SQL); CHANGED §5.7.
- Subject-code since `9c5a9f25` on
  `control/liveness_index.go` + `control/liveness_index_bench_test.go`
  (this HEAD): **2** commits —
  `b6e76b5a` compact index, `8ea119b6` shadow-wire (inert). The 12.4 B /
  3.5M figures therefore **predate** the compact-index implementation
  commit, as the archaeology already said.

Quoted 10,000,000-slot cell:

| field | value |
|---|---|
| `fleet` / `liveslots_count` | **10000000** |
| `hot_bytes_per_device` | **12.4073032** |
| `heartbeat_per_sec` | **3503242** |
| `heartbeat_latency_ns.p50` | 875 |
| `liveslots_ms` | 11.016 |
| `tick_expire_ms` | 15.265 |
| `duration_sec` | 2 |
| `workers` | 28 (`num_cpu=28`, darwin/arm64, host Mac-Studio) |
| generated_at | 2026-08-15T01:41:48Z |

**Contract:** `MERC_LIVENESS_INDEX_BENCH=1`; excluded from `make test` /
`make ci`; no PostgreSQL; "this index is not wired to eligibility; numbers
are presence-engine only"; does not prove production selector intersection,
authenticated HTTP ingest, multi-host presence, or a droplet-class 1vCPU
ceiling. Fleet ladder also includes 100,000 and 1,000,000 cells at
~12.9 / ~12.5 B/device and ~3.40M / ~3.47M hb/s; the 10M cell is the
largest representation actually allocated.

### Demonstration 2 — droplet-class PROXY ingest curve (postgres)

- File: `evidence/perf/droplet-device-ceiling.json`
  (prose twin: `evidence/perf/droplet-device-ceiling-reading.md`)
- File last git commit: `dde956ea` (stamp). Reading.md last git commit
  `3d794111`.
- Artifact `source_commit_base`: `90ff8869112608f6ae417d0913c263d19204551e`
- `binding_status`: `UNBOUND`; same `missing_identity_fields` family as
  the index bench; generated_at 2026-08-15T00:51:59Z
- Archaeology Table B: MEASURED (proxy) + PROJECTED (10M host count);
  CHANGED. `git log --oneline 90ff8869..HEAD -- control/store_workers.go
  control/heartbeat_ingest_bench_test.go` count at this HEAD = **25**,
  including taking the durable heartbeat write off the authenticated fast
  path.

Quoted verdict:

| path | sustained hb/s | implied fleet `floor(hb/s × 45)` |
|---|---:|---:|
| baseline, 50k book, conc=16 | **1980.1666666666667** | **89107** |
| coalesced 2 ms flush, 50k book, conc=64 | **4089.6** | **184032** |
| coalesced 50 ms flush | 262.666… | 11820 (an 11× **loss**; kept so it is not shipped) |

Footprint at seed: `seeded_devices=50000`, `live_devices=47778`,
`bytes_per_device=2185.46176`. Restart: empty clean 62.111 ms; loaded
checkpoint-then-SIGKILL 240.629 ms.

**Contract:**

| axis | value |
|---|---|
| host_class | `droplet-class PROXY` — `docker --cpus=1 --memory=961m --memory-swap=3009m` postgres:17 on Apple M3 ARM, **not** a DigitalOcean x86 vCPU |
| what was cgroup-limited | Postgres only; Go bench client unconstrained on 28 host cores |
| honesty | every throughput number is an **UPPER bound**; a real 1vCPU/961MB droplet shares the core with control/caddy/minio |
| liveness window | 45 s; implied fleet = `floor(sustained_heartbeats_per_sec × 45)` |
| `no_10m_on_one_droplet` | true. 10M × 2185 B ≈ 21.8 GB of offer+worker relations does not fit in 961 MB |
| 10M host-count | **PROJECTED** lower bound ≥ **55** such hosts *if* the working set fit — it does not |
| selector sidecar | unbounded eligibility COUNT p50=187.578 ms at 50k under ingest; production LIMIT-2 branch probe p50=0.831 ms. Ranking walk of eligible rows **not** measured as mutating Authorize |

`heartbeat-ingest-ceiling.json` shares LFS oid `655ecf0ada…` with this
file (archaeology). It is not a second measurement.

### What would upgrade the production-capacity half to claimed

1. Re-run `control/liveness_index_bench_test.go` at HEAD after the compact
   index and G082 wiring, with complete producer identity, and state
   whether the index is now on the eligibility path.
2. Re-run the droplet curve **on a real 1vCPU/961MB droplet** (or the
   production host class), with control/caddy/minio co-resident, at HEAD,
   BOUND.
3. If a 10M-device claim is desired: actually store 10M live offer+worker
   rows and measure Authorize/eligibility under that book — do not
   extrapolate from hb/s × 45 or from an in-process array.

Until then Merc may say what the two UNBOUND receipts measured, with their
contracts. Merc may not say it operates a 10 million device fleet.

---

## What this ledger is careful not to do

- No combined "Merc is 2× faster at 184k devices for 302 tok/s" sentence.
  Those figures belong to three different classes and three different
  contracts; two of the classes are `NOT CLAIMED`.
- No quote of `evidence/perf/latency-atlas.json` percentiles.
- No quote of 138.7 as current, of r6 304.2661 as current, of CUDA 1617 /
  7081 tok/s as today, of MLX 6512 / 6828 tok/s, of WITHDRAWN selector
  economics, of `gateway-parity.json` (`INVALIDATED_PENDING_RERUN`), or of
  `selector-scale-curve.json` (`source_commit=unknown`).
- No treatment of a q4 Metal cell and a bf16 CUDA cell as one product.

---

## Verification appendix

Commands run in this worktree to establish HEAD, atlas staleness, and
file-touch SHAs. Producer `source_commit` inside a JSON body is the
measurement identity; `git log --oneline -1 -- <path>` is often the LFS
migration (`eed5b550`) or an alpha stamp (`dde956ea`) and is recorded so
that confusion is visible.

```
git rev-parse HEAD
83806ccf805b1bc40eddd35383c2230d63d1e1e6

git log --oneline -1 -- evidence/hyper-atlas-archaeology.md
fd054682 perf: three read-only audits of what Merc actually knows about itself

git log --oneline -1 -- evidence/perf/latency-atlas.json
435d013e deferral: the buyer paid for speed, and the claim path never read the promise

git rev-list --count 435d013e644c544db1123f09ccea19e2f69e2c9b..HEAD
204

git rev-list --count 74dea5e17dcc44c7790c4e6b41c48bbd59dd0f0d..HEAD
1001

git log --oneline -1 -- evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r7.json
697713f9 alpha: the agent enrols, and the embed cell measures what Merc settles

git log --oneline 697713f9..HEAD -- agent/src/
(empty)

git log --oneline -1 -- evidence/perf/runtime-benchmarks/engine-tournament-metal-host-scope-r2.json
eed5b550 L4: migrate 84 evidence/perf files to git-lfs at tip

git rev-list --count 2c8518dc908bda0651706d24e26e6f49acf0659e..HEAD -- agent/src/
79

git log --oneline -1 -- evidence/perf/runtime-benchmarks/serving-matrix-candle-vs-llama-cpp-metal-r1.json
eed5b550 L4: migrate 84 evidence/perf files to git-lfs at tip

git log --oneline -1 -- evidence/perf/runtime-benchmarks/candle-vs-llama-cpp-metal-r3.json
eed5b550 L4: migrate 84 evidence/perf files to git-lfs at tip

git log --oneline -1 -- evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r3.json
a382fb65 alpha: restore the money contract as a real requirement, and meet it

git log --oneline -1 -- evidence/perf/heterogeneous-placement-principal-latest.json
eed5b550 L4: migrate 84 evidence/perf files to git-lfs at tip

git log --oneline -1 -- evidence/perf/selector/metal-vs-cuda-embed-verdict-latest.json
c9831ed4 network-v2: close runtime-cell economics authority

git log --oneline -1 -- ops/acceptable-quality-contracts.json
4a703945 hetero: acceptable-quality-contract gate for honest Metal/CUDA substitutability (G024, software prereq)

git log --oneline -1 -- evidence/competitive-position.json
2a0abd39 honesty: the caveat was in the receipt and dropped at the surface that gets quoted

git log --oneline -1 -- evidence/perf/prefix-two-worker-latest.json
2f5d8b3c prefix: two workers, engine-confirmed, and an explicit refusal to call it a network result

git log --oneline -1 -- evidence/perf/prefix-kv-physical-metal-latest.json
eed5b550 L4: migrate 84 evidence/perf files to git-lfs at tip

git log --oneline -1 -- evidence/perf/arrival-batching.json
eed5b550 L4: migrate 84 evidence/perf files to git-lfs at tip

git log --oneline -1 -- evidence/perf/liveness-index-bench.json
dde956ea alpha: stamp every unstamped receipt, which exposes nine citations that were hiding

git log --oneline -1 -- evidence/perf/droplet-device-ceiling.json
dde956ea alpha: stamp every unstamped receipt, which exposes nine citations that were hiding

git log --oneline -1 -- evidence/benchmarks/2026-07-01-m3-pro.json
1e006669 feat: evidence that cannot name its producer is refused at write time and in CI

git log --oneline -1 -- evidence/autonomous/hardware-characterization.json
2240d6ed readiness: regenerate all receipts at the freeze candidate c0c9e3fc

git log --oneline -1 -- control/pricing.go
697713f9 alpha: the agent enrols, and the embed cell measures what Merc settles
```

LFS pointer for the atlas begins `oid sha256:acbd7980b00d7c3123c19224b05515e844da9b616c8d1d99ae99f776c863b2cf`
and parses as `kind=merc_latency_atlas`, `binding_status=UNBOUND`.

---

## Summary

| class | today |
|---|---|
| ENGINE DOMINANCE | `NOT CLAIMED` |
| PLATFORM DOMINANCE | `NOT CLAIMED` |
| NETWORK DOMINANCE | `NOT CLAIMED` |
| SCALE | two UNBOUND historical demonstrations (10M in-process slots; PROXY implied fleet 89,107 / 184,032); **not** production capacity |

The sealed catalogue infer rate **302.3194** tok/s at engine hash
`0b6a7a9cd7578343` is a single-arm BOUND measurement. It is not a
dominance claim and is not a scale claim.
