# HYPER-G — what is actually stale in Merc's performance record

Read-only archaeology at HEAD `4b10b56e0c491c2e9fdb652344d07020c78cd43f`
(`grok/HYPER-G-atlas-20260823-234256`). No other tracked file was edited.
`git lfs prune` was not run.

Governing rule this report exists to enforce: **never optimize against old
numbers.** Every figure below is written with its provenance. None of them is
repeated as a present-tense fact.

---

## 0. How this checkout reads, and how the numbers were recovered

This worktree is a sparse checkout. `evidence/perf/**` is Git LFS
(`.gitattributes`: `evidence/perf/** filter=lfs`). Every file under
`evidence/perf/` in the working tree begins:

```
version https://git-lfs.github.com/spec/v1
```

That is not a JSON parse failure. On disk here those files are **LFS pointers**.
Zero objects lived in this worktree's LFS store. The parent repo cache
(`LocalMediaDir=/Users/scammermike/Downloads/merc/.git/lfs/objects`) held all
**96/96** pointer OIDs. Claims below were read from those objects, without
`git lfs pull` and without `git lfs prune`. A clone that lacks that cache must
treat the same files as **UNREADABLE HERE**.

Non-LFS sources read as ordinary files / `git show HEAD:<path>`:

- `evidence/bench/*.jsonl` (+ binding sidecars)
- `evidence/benchmarks/*.json`
- `evidence/competitive-position.json`
- `evidence/autonomous/hardware-characterization.json`
- `docs/`, `ops/`, `control/`, `scripts/` via `git show` (not materialized)

Classification uses the artifact's own words when it has them
(`MEASURED` / `DERIVED` / `MODELED` / `PROJECTED` / `UNMEASURED`, plus the
repo's `BOUND` / `UNBOUND` / `WITHDRAWN` / `SUPERSEDED` / `INVALIDATED`
binding vocabulary). When a number is asserted with none of those words, that
ambiguity is the finding.

**Still true at HEAD** means: the artifact names a real `source_commit` /
`merc_source_commit` / `head`, and
`git log --oneline <sha>..HEAD -- <subject paths>` is empty. Bound-to-HEAD
(`4b10b56e`) receipts: **none**.

---

## 1. The failure mode, named

`evidence/perf/latency-atlas.json` is kind `merc_latency_atlas`,
`binding_status=UNBOUND`, `head=74dea5e17dcc44c7790c4e6b41c48bbd59dd0f0d`.
That SHA **is an ancestor of HEAD**. `git rev-list --count 74dea5e1..HEAD` =
**164 commits**. The atlas itself says it must not be cited as bound authority:

> UNBOUND on purpose. This atlas is a structural census assembled from two
> read-only reconnaissance reports plus percentiles copied out of other
> evidence files; it has no producer identity of its own — no harness
> revision, no exact config, no raw samples — so it cannot claim BOUND.

It self-flags **19 MEASURED** stages (copied from other `evidence/perf/*.json`)
and **38 UNMEASURED** stages. Several of the 19 copy `binding_status=UNBOUND`
sources. `missing_identity_fields` includes `source_commit`, `build_digest`,
`harness_revision`, `corpus_digest`, `exact_config`, `raw_samples`.

`docs/REMAINING_WORK.md` still quotes that atlas as if it measured today's
bottleneck (see §6). That is the failure mode this lane was asked to map
completely.

---

## 2. Table A — latency atlas stages (57)

Bound commit of the atlas file: **`74dea5e17dcc44c7790c4e6b41c48bbd59dd0f0d`**
(census HEAD). File last touched at `0f3202d0` (deferral: the buyer paid for
speed…). Subject-code verdict for **every row**: **CHANGED**. Git output is in
§5.1.

Copied MEASURED values are laboratory c=1 (or the noted concurrency), tens to
low hundreds of samples, single-host. A p99 from n≈40–80 at c=1 is not a
production tail. The atlas says this.

| stage_id | claim | source artifact (json_path) | bound commit of *source* | class | subject code |
|---|---|---|---|---|---|
| `read_body` | p50=0.004 / p95=0.006 / p99=0.006 ms | `merc-latency-gap-accounting-latest.json` `accounting_table['c=1'].named_stages.read_body` | `2d31f092` BOUND n=40 c=1 | MEASURED (copy) | CHANGED §5.2 |
| `prepare_json` | p50=0.022 / p95=0.034 / p99=0.035 ms | same, `.prepare_json` | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `intake_control` | p50=0.167 / p95=0.276 / p99=0.417 ms | same, `.intake_control` | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `exact_reuse` | p50=p95=p99=`UNMEASURED` | none | — | UNMEASURED | CHANGED (path exists: `realtime.go:931`) |
| `coalesce` | `UNMEASURED` | none | — | UNMEASURED | CHANGED (`realtime.go:969`) |
| `authorize_contract` | p50=1.133333 / p95=1.445625 / p99=1.582 ms | `hot-path-free-admit-latest.json` `cells[0].authorize_ms` (legacy c=1) | **no `source_commit`** UNBOUND n=80 c=1 | MEASURED (copy of UNBOUND) | CHANGED §5.3 |
| `authorize_tx_begin_and_idempotency` | p50=0.034292 / p95=0.06375 / p99=0.06675 ms | `merc-segment-latency-latest.json` `cells[0].authorize_decomposition_ms.begin` | `0487c043` BOUND n=60 c=1 | MEASURED (copy; covers tx.Begin only, not idempotency hit) | CHANGED §5.4 |
| `price_bound_revalidation` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `funding_legacy_buyer` | p50=0.144375 / p95=0.231667 / p99=0.250958 ms | segment `authorize_decomposition_ms.funding` | `0487c043` | MEASURED (copy) | CHANGED §5.4 |
| `funding_envelope_spend` | p50=0.407917 / p95=0.56375 / p99=1.308708 ms | hot-path `direct_claim_decomp_c1.envelope_spend_ms` | no SHA UNBOUND | MEASURED (copy of UNBOUND) | CHANGED |
| `offer_book_count` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `offer_capacity_claim_and_rank` | p50=0.150084 / p95=0.20975 / p99=0.219042 ms | segment `offer_claim` | `0487c043` | MEASURED (copy; offer_count=1 fixture) | CHANGED §5.4 |
| `placement_bind` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `pricing_decision` | p50=0.0065 / p95=0.012916 / p99=0.080708 ms | hot-path `direct_claim_decomp_c1.pricing_ms` | no SHA UNBOUND | MEASURED (copy of UNBOUND stand-in) | CHANGED |
| `market_clearing_receipt` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `durable_contract_reservation` | p50=0.391333 / p95=0.8015 / p99=1.790083 ms | hot-path `contract_event_batch_ms` | no SHA UNBOUND | MEASURED (copy of UNBOUND) | CHANGED |
| `open_upstream_credential` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `admission_event` | p50=0.002 / p95=0.005 ms | gap-accounting c=1 | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `arrival_batch` | p50=0.009 / p95=0.014 ms | gap-accounting c=1 | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `pre_upstream` | p50=0.005 / p95=0.007 ms | gap-accounting c=1 | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `upstream_ttfb` | p50=2.863 / p95=3.016 ms | gap-accounting c=1 | `2d31f092` | MEASURED (copy; loopback Metal llama-server) | CHANGED §5.2 |
| `settlement_intent` | p50=0 / p95=0.001 ms | gap-accounting c=1 | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `post_upstream` | p50=0.003 / p95=0.005 ms | gap-accounting c=1 | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `upstream_first_sse` | p50=3.03 / p95=3.647 ms | gap-accounting c=1 | `2d31f092` | MEASURED (copy) | CHANGED §5.2 |
| `sl_offer_upsert` | `UNMEASURED` | none | — | UNMEASURED | CHANGED (lease code landed after atlas: `efac9aa2`) |
| `sl_create_order_book_lock` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_pricing_and_select` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_prepaid_reserve` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_durable_activation` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_heartbeat_meter` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_recover_worker_loss` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_failover_replace` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_failover_terminate_no_replacement` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_finalize_expired` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_buyer_cancel` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `sl_receipt_read` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `buyer_auth` | p50=0.070125 / p95=0.118625 ms | `authorize-auth-tails-latest.json` lookup_api_key_cold c=1 max_conns=20 | `2c8518dc` BOUND n=80 | MEASURED (copy) | CHANGED §5.3 |
| `quote_build_and_persist` | `UNMEASURED` | none | — | UNMEASURED | CHANGED (`e37a25c1` canary quotes) |
| `job_submit_normalize` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `job_input_stream_upload` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `job_pricing_acceptance` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `exact_reuse_batch` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `job_funding_admission` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `job_durable_persist` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `worker_register_capabilities` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `worker_heartbeat` | `UNMEASURED` | none | — | UNMEASURED | CHANGED (P0A took durable HB off the authenticated fast path: `375ab9be`) |
| `task_claim` | p50=2.406916 / p95=8.032542 ms | `prefix-kv-hitrate-latest.json` `claim_latency_ns_p50=2406916` / `p95=8032542` | producer `3f04760f` era; file LFS-migrated `0c721952` | MEASURED (copy of **routing-belief** claim latency, not a claim-SLA harness) | CHANGED (`0f3202d0` deferral never read the promise) |
| `task_dispatch_presign` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `task_start_ack` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `agent_execute_and_upload` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `task_commit_ingest` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `verification_process` | `UNMEASURED` | none | — | UNMEASURED | CHANGED (`db94746a` evaluator-never-ran) |
| `verification_settlement_apply` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `job_merge_results` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `job_finalize_economics` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `job_sla_and_card_charge` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |
| `receipt_assembly` | `UNMEASURED` | none | — | UNMEASURED | CHANGED |

Atlas structural risk #1 is **not a measurement**: `askDeferralWindow = 20s`
(`control/scheduler.go:193`) — a code constant, scale `s`. Rank #2 is
long-poll up to 25s. Those are the "20-second queue deferral" figures
`docs/REMAINING_WORK.md` attributes to the atlas.

Atlas prose also copies hot-path **legacy c=32** authorize
**p50=59.137084 ms / p99=132.921708 ms** (UNBOUND, no `source_commit`). That
is the "59 ms p50 / 133 ms p99 at concurrency 32" quote. The stage table
itself stores only the c=1 1.133 ms cell for `authorize_contract`.

---

## 3. Table B — other `evidence/perf` artifacts (headline claims)

Working-tree status for every row: **LFS pointer**. Content from parent LFS
cache. "File last git commit" is often the L4 LFS migration `0c721952` or the
alpha stamp `c538cd92`, **not** the measurement. Prefer the artifact's own
`source_commit` / `merc_source_commit` / `producer_identity.source_commit`.

| artifact | claim | bound commit | class | subject code |
|---|---|---|---|---|
| `hot-path-free-admit-latest.json` | authorize p50 ms: legacy 1.133 / 13.438 / **59.137** at c=1/8/32; envelope 1.252 / 3.190 / 3.211; envelope+direct 0.959 / 2.506 / 2.882. c=32 legacy p95=117.260 p99=**132.922**. Target ≤1 ms merc-added: not shown. | **none** (`missing_identity_fields` includes `source_commit`). `binding_status=UNBOUND`. Note: a prior `UNBOUND_PROBE` value was rewritten because it was "invisible to the gate and reads as bound to a human skimming". measured_at 2026-08-03. | MEASURED (lab microbench) | CHANGED (`realtime_store.go` §5.3, 24 commits since `2c8518dc`) |
| `merc-latency-gap-accounting-latest.json` | c=1: client TTFT p50=9.101 ms; merc_owned p50=2.994; direct engine TTFT p50=5.67; parity overhead p50=3.431. Metal loopback llama-server, n=40. | `2d31f092` BOUND. Harness: `control/merc_latency_gap_accounting_test.go` | MEASURED | CHANGED §5.2 (8 commits on `realtime.go`) |
| `merc-segment-latency-latest.json` | c=1 run=1: client TTFT p50=1.675 ms; merc_added TTFT p50=1.354; authorize_contract p50=0.903; settlement p50=1.638. Stub upstream, n=60. | `0487c043` BOUND. Harness: `control/merc_segment_latency_measure_test.go` | MEASURED | CHANGED §5.4 (26 commits) |
| `authorize-auth-tails-latest.json` | max_conns=64 c=32: multi-buyer 1-offer p50=22.088 p95=75.556; N-offer p95=9.988; same-buyer p95=197.040. max_conns=20 c=32: 1-offer p95=55.881, same-buyer p95=119.239. Cold API-key lookup p50=0.070 ms (c=1). | `2c8518dc` BOUND. Harness: `control/authorize_tail_characterize_test.go` | MEASURED | CHANGED §5.3 (24 commits). `docs/RUNTIME_AND_PERF.md` still quotes ~50–75 ms p95 as the 2026-08-03 verdict. |
| `authorize-auth-tails-baseline.json` / `…174152Z` | same producer SHA `2c8518dc`; earlier cells | `2c8518dc` | MEASURED (superseded by `-latest`) | CHANGED |
| `authorize-auth-tails-20260803T174544Z.json` | intermediate | `2c8518dc` | MEASURED (superseded) | CHANGED |
| `arrival-batching.json` | stand-in CB: peak agg tok/s 14003.8 off vs 13041.2 on (−6.9%); TTFT p50 48→28 ms at c=64. Verdict `INCONCLUSIVE_NULL`. | none UNBOUND | MEASURED stand-in (not a real CB engine) | CHANGED |
| `control-plane-hot-path-profile.json` | heartbeat_ingest_flag_off_batched_success p50=3.205 ms, 2440 ops/s; flag_on_retired p50=0.0005 ms, 4.77e6 ops/s; authorize_success_book_16_c8 p50=19.688 p95=27.054 p99=65.083 ms, 366.6 op/s. Ranking mixes MEASURED p50 with **PROJECTED** "100 admits/s" payoff. Unmeasured: ClaimTasksTx, HTTP+authWorker, live QPS, droplet-class, flag-ON liveness in Authorize. | `1b016ec4` classification=MEASURED, binding UNBOUND. Harness: `control/control_plane_hot_path_profile_test.go` | MEASURED + DERIVED frequency + PROJECTED payoff | CHANGED §5.5 (5 commits; `b89152ee` landed the profile then later liveness/quote work) |
| `droplet-device-ceiling.json` | PROXY upper bound (docker `--cpus=1 --memory=961m` postgres on M3 ARM, PG-only cgroup). Baseline ~1980 hb/s → ~89,107 devices alive; coalesced 2 ms ~4089.6 hb/s → ~184,032; 50 ms flush ~263 hb/s (worse). bytes/device ~2185. 10M host lower bound ≥55. Restart empty 62 ms / loaded checkpoint-kill 241 ms. Selector eligibility p95 ~116 ms (doc) / curve cells up to p95=134.9 ms. | `source_commit_base=90ff8869` UNBOUND | MEASURED (proxy) + PROJECTED (10M host count) | CHANGED §5.6 (3 commits on heartbeat/store_workers) |
| `droplet-device-ceiling-reading.md` | prose twin of the JSON | same | MEASURED (proxy) | CHANGED |
| `heartbeat-ingest-ceiling.json` | same OID as droplet-device-ceiling.json (pointer `655ecf0ada`) | `90ff8869` | MEASURED (duplicate pointer) | CHANGED |
| `heartbeat-ingest-ceiling-batched-tight.json` / `-default-knobs.json` / `heartbeat-ingest-restart.json` | related ingest/restart cells | `90ff8869` or stamp `c538cd92` UNBOUND | MEASURED | CHANGED |
| `liveness-index-bench.json` | ~12.4 B/device; in-process ~3.5M authenticated hb/s; cells e.g. liveslots_ms=0.143, tick_expire_ms=0.132 | `9c5a9f25` UNBOUND. Harness: `control/liveness_index_bench_test.go` | MEASURED (in-process index, not SQL) | CHANGED §5.7 (1 commit: `b1f593b4` compact index) |
| `liveness-write-amplification.json` | coalescer items 1100 off / 300 on; preload_ms 15.4 / 20.4 | `ff5a6531-dirty` MEASURED UNBOUND | MEASURED | CHANGED (dirty tree; offer→slot mapping `ff5a6531`) |
| `prefix-kv-hitrate-latest.json` | belief hit_rate=0.125 (family 0.167); claim_latency_ns_p50=2406916 (2.407 ms) and 1830250 (1.830 ms) on another arm; exact-body replay 0.995; identity cache hit 721 ns / miss 14533 ns. Classification: ROUTING_AFFINITY_ONLY — not physical KV. | LFS `0c721952`; physical KV superseded by `prefix-kv-physical-metal-latest.json` | MEASURED (routing) | CHANGED |
| `prefix-kv-physical-metal-latest.json` | physical KV reuse on llama.cpp Metal | LFS `0c721952` | MEASURED | CHANGED |
| `prefix-two-worker-latest.json` | two workers **same host**; `not_exercised`: two machines, cross-supplier, multi-region, paid cloud | `644ccee6` BOUND. Harness: `scripts/prefix-two-worker-measure.py` | MEASURED (same-host) | CHANGED. `docs/NETWORK_V2_EXECUTION_PLAN.md` quotes this honestly as not network supremacy. |
| `prefix-affinity-routing.json` | stand-in ranker; superseded by hitrate receipt | LFS `0c721952` | MEASURED stand-in | CHANGED |
| `five-cache-architecture-audit.json` | two of five directive caches **DOES_NOT_APPLY**; identity-cache microbench hit ~0.4 µs miss ~12 µs | `a16e9162` BOUND | MEASURED (microbench) + architecture census | CHANGED |
| `selector-scale-curve.json` | SQL scaling shape for batch/realtime/service_lease; bible_targets_ms 1/3/10/25 at 1k/10k/100k/1M; host loadavg 17.8→23.6 — **absolute ms PROVISIONAL** | `source_commit=unknown` UNBOUND generated 2026-08-10 | MEASURED shape / UNMEASURED absolute | **cannot bind**; treat as not still-true |
| `selector-scale-diagnostic.json` | partial diagnostic twin | `source_commit=unknown` | MEASURED (partial) | cannot bind |
| `realtime-selection-decomposition-5892b3300084.json` | S0–S4 SQL cost probes vs synthetic offer book | SHA in filename; harness `control/realtime_selection_decomposition_test.go` | MEASURED (opt-in, 3h) | CHANGED |
| `realtime-auth-reputation-latest.json` | auth/reputation probe | LFS `0c721952` | MEASURED | CHANGED |
| `realtime-candidate-projection-ab.json` | candidate projection A/B | stamp `c538cd92` | MEASURED | CHANGED |
| `g021-g053-per-phase-prediction-regret.json` | status=PARTIAL; latency/cost/SLA/locality regret; Mode B refused | `bff5fd33` UNBOUND | MEASURED (offline replay) | CHANGED (2 commits on replay/plan_calibration) |
| `gateway-parity.json` | `validity=INVALIDATED_PENDING_RERUN` WITHDRAWN | `44588086` | WITHDRAWN (was MEASURED) | do not quote |
| `gateway-parity-v2-local-metal.json` + `.bound.json` | `SUPERSEDED_GATE_STATISTICS` | `4e926f22` | SUPERSEDED | do not quote |
| `gateway-parity-v2-local-metal-quiet.json` + `.bound.json` | quiet Metal parity; `.bound` BOUND | `3aed4370` | MEASURED | CHANGED |
| `gateway-parity-v2-matrix-selftest.json` | cells[0] absolute_overhead_ttft_p95_ms=5.058; relative 3.08 | `a16e9162` BOUND | MEASURED (selftest) | CHANGED |
| `gateway-parity-v2-runpod-vllm-latest.json` | RunPod vLLM parity | `e66c6cc5` UNBOUND | MEASURED | CHANGED |
| `gateway-parity-v2-selftest.json` / `-cli.json` | selftest | `ce6422be` / `c43d22b4` UNBOUND | MEASURED | CHANGED |
| `gateway-concurrency-sweep.json` | concurrency sweep | `695fa1e9` UNBOUND | MEASURED | CHANGED |
| `cuda-a5000-qwen1.5b.json` | peak_aggregate_tokens_per_sec=**1617.43**, $/MTok=0.0464 | none UNBOUND | MEASURED (historical CUDA) | **must not quote as today** (`docs/PROGRAMME.md` already says so) |
| `cuda-a5000-tuned.json` | peak 2841.82 tok/s, $/MTok=0.0264 | none UNBOUND | MEASURED (historical) | must not quote |
| `cuda-a5000-ceiling.json` | peak **7081.56** tok/s, $/MTok=0.0106 | none UNBOUND | MEASURED (historical) | must not quote |
| `cuda-throughput-correction.json` | best.aggregate=7081.56; whose_cost / caveat | none UNBOUND | DERIVED from the three CUDA sweeps | must not quote |
| `metal-m3ultra-qwen3b.json` | Metal M3 Ultra Qwen3B sweep | LFS `0c721952` UNBOUND | MEASURED (historical) | CHANGED |
| `board-power-a40-latest.json` | A40 board power | none UNBOUND | MEASURED | CHANGED |
| `catalogue-profile-match-1ylse7hwwee68x.json` | catalogue vs pod match | none UNBOUND | MEASURED (assessment) | CHANGED |
| `vllm-profile-probe-20260803T175830Z.json` | vLLM catalogue probe | none UNBOUND | MEASURED | CHANGED |
| `ioreport-energy-deltas-authority.json` | energy deltas | `75ccb15a` BOUND. Harness: `scripts/energy-deltas-measure.py` | MEASURED | CHANGED |
| `ioreport-gpu-energy-authority.json` | GPU energy | `04295cfa` BOUND. Harness: `scripts/bench-harness.py` | MEASURED | CHANGED |
| `phase6-directive-economics.json` | phase-6 economics receipt | LFS `0c721952` | MODELED / census | CHANGED |
| `routing-crossover.json` | Metal vs CUDA TTFT/throughput shape | LFS `0c721952` UNBOUND | MEASURED shape; absolute tok/s not comparable (competitive-position agrees) | CHANGED |
| `blender-render-baseline.json` | Cycles/Blender baseline cells | `017c7d10-dirty` MEASURED UNBOUND | MEASURED | **dirty SHA — not a real commit**; CHANGED |
| `cycles-device-placement.json` | device placement grid | `44d245d9-dirty` MEASURED | MEASURED | dirty SHA; CHANGED |
| `cycles-standalone-parity.json` | standalone vs blender-integrated | `1dd71979` MEASURED UNBOUND | MEASURED | CHANGED |
| `render-verify-pipeline.json` | L1 PNG decode+hash vs hash-only; Amdahl **CEILING** labelled never-achieved; PIXEL_EXACT catches 1-pixel mutation | `44d245d9-dirty` MEASURED UNBOUND. Harness: `control/render_verify_pipeline_bench_test.go` | MEASURED + MODELED (Amdahl ceiling) | dirty SHA; CHANGED |
| `heterogeneous-placement-principal-latest.json` | status=`REFUSED_STRUCTURAL` | `629f8768` BOUND | not a throughput claim | — |
| `heterogeneous-placement-power-analysis-latest.json` | status=`DESIGN_AND_PRIOR_MEASUREMENT` | none UNBOUND | MODELED budget of GPU time/USD to authorize a Metal-vs-CUDA measure | — |

### Runtime-benchmarks (engine cells)

| artifact | claim | bound commit | class | subject code |
|---|---|---|---|---|
| `candle-metal-llama1-q4-r6.json` | **catalogue authority**. batch_infer `throughput_units_per_second=304.2661` (batch1); batch32=670.217; serial 304.4813; peak 678.7067; decode-only diagnostic serial 232.91. unit_scope `token_like_input_plus_max_output_tokens`. engine_build_hash `7cc01c442c7f6dbe`. thermal_ok=true. | `926edfe3` BOUND. Harness: `merc-agent bench-batch` | MEASURED | CHANGED §5.8 (`agent/src/` 3 commits) |
| `candle-metal-llama1-q4-r5.json` | 288.971 units/s; hash `f4303a751ca2b2af` **superseded**. `docs/ALPHA_LAUNCH_READINESS.md` still says the image copies the r5 receipt the catalogue cites — catalogue now cites **r6**. | `e351341f` BOUND | MEASURED (superseded) | CHANGED |
| `candle-metal-llama1-q4-r2`…`r4` | earlier candle infer cells | various / LFS migrate | MEASURED (superseded) | CHANGED |
| `mlx-metal-llama1-4bit-r1.json` | MLX 4-bit physical throughput | `c212dcb1` UNBOUND | MEASURED | CHANGED; contradicts speed-lane 6,828 vs cross-test 310.7 in `docs/RUNTIME_AND_PERF.md` contradiction ledger |
| `llama-cpp-metal-llama1-q4-r1.json` / `r3.json` | llama.cpp Metal infer | `a0abf0a2` / `c212dcb1` UNBOUND | MEASURED | CHANGED |
| `llama-cpp-metal-determinism-sweep.json` | batched Metal **diverges** byte-exact; serialised only at ~1.02× | `f89d1aee` UNBOUND | MEASURED | CHANGED |
| `llama-cpp-metal-embed-cosine-gate.json` | cosine 0.999999 vs 0.999 gate; `validity=SUPERSEDED` | `0c356bf3` SUPERSEDED | MEASURED then SUPERSEDED | — |
| `vllm-cuda-llama1-r1.json` | vLLM CUDA llama-1B | `c212dcb1` UNBOUND | MEASURED | CHANGED; PROGRAMME: no bound CUDA tok/s at this commit |
| `vllm-cuda-raw.txt` | raw log (device=Metal in the first line — mislabeled twin) | LFS | MEASURED log | — |
| `serving-matrix-llama-cpp-metal-local-r2.json` | serving matrix BOUND | `a8159ac7` | MEASURED | CHANGED |
| `serving-matrix-llama-cpp-metal-local-r1.json` | SUPERSEDED | — | SUPERSEDED | — |
| `serving-matrix-candle-vs-llama-cpp-metal-r1.json` | two-arm same digest | `2c8518dc` BOUND | MEASURED | CHANGED |
| `embed-cell-candle-vs-llama-cpp-r3.json` | embed cell two-engine | later stamp `9ba9884e` | MEASURED | CHANGED |
| `engine-tournament-metal-host-scope-r2.json` | TWO_ENGINE_RANKED | `2c8518dc` BOUND | MEASURED | CHANGED |
| `engine-tournament-metal-host-scope-r1.json` | INCOMPARABLE_ARMS | `a8159ac7` | MEASURED (incomparable) | CHANGED |
| `candle-metal-ffmpeg-media-r1.json` / `candle-metal-rendering-r1.json` | media/render cells; media `merc_source_commit` is **not a git object** (PROGRAMME: CANARY, not ordinary-routable) | stamp `19fe0b23` | MEASURED (unbindable for catalogue) | CHANGED |

### Selector family (several WITHDRAWN as economic authority)

| artifact | claim | bound commit | class | subject code |
|---|---|---|---|---|
| `selector/engine-parity-metal-embed-latest.json` | candle vs llama.cpp Metal embed p50/p95/p99 ms/unit | `585fb2f4` BOUND. Harness: `scripts/engine-parity-metal-embed-measure.py` | MEASURED | CHANGED. Cited as live evidence by `ops/placement-readiness-contract.json` and `ops/acceptable-quality-contracts.json`. |
| `selector/engine-parity-metal-embed-20260804T234903Z.json` | later bound twin | `644ccee6` | MEASURED | CHANGED |
| `selector/embed-throughput-metal-llamacpp-latest.json` | peak_units_per_sec=**4411.82** | none UNBOUND | MEASURED | CHANGED |
| `selector/embed-throughput-cuda-a40-latest.json` | peak_units_per_sec=**223.57** | none UNBOUND | MEASURED | CHANGED |
| `selector/cuda-embed-arm-latest.json` | CUDA embed arm vs matched MiniLM contract | `0174279f` BOUND. Harness: `scripts/cuda-embed-arm-measure.py` | MEASURED | CHANGED |
| `selector/paired-cohort-embed.json` | `validity=WITHDRAWN` (wrong price unit) | none | WITHDRAWN (was MEASURED) | do not quote as economics |
| `selector/cell-economics-census.json` | WITHDRAWN | `9092fbd4` | WITHDRAWN | do not quote |
| `selector/economic-selector-candle-vs-llama-proof.json` | WITHDRAWN | `644ccee6` | WITHDRAWN | do not quote |
| `selector/governed-candle-vs-llama-shadow-decision.json` | WITHDRAWN | `a1a3c257` | WITHDRAWN | do not quote |
| `selector/metal-vs-cuda-embed-verdict-latest.json` | WITHDRAWN: derived from two UNBOUND sweeps | `162cecee` | WITHDRAWN | do not quote |

---

## 4. Table C — non-`evidence/perf` Merc performance numbers

| artifact | claim | bound commit | class | subject code |
|---|---|---|---|---|
| `evidence/benchmarks/2026-07-01-m3-pro.json` | M3 Pro candle: embed **1967.3141** eps p99=7 ms; batch_infer batch32 **138.71389521** tok/s, serial 91.19. `binding_status=UNBOUND`, no `source_commit`. **This is the 138.7 figure.** | none UNBOUND. File last commit `1de03bf0` (producer-identity stamp, 2026-08-02) | MEASURED (historical M3 Pro) | CHANGED §5.9 (`agent/src/` 16 commits since the stamp). **Not** what `repricingBenchmarks` uses anymore. |
| `evidence/benchmarks/2026-07-26-m3-pro-backend-compare.json` | candle CPU session peak 20.41 tok/s vs llama-server Metal; candle Device::new_metal panicked to CPU this session | none UNBOUND | MEASURED (confounded) | CHANGED |
| `evidence/bench/runs.jsonl` | MLX 4-bit M3 Ultra 2026-07-27: INTERACTIVE decode 336.1 t/s TTFT 62 ms; BATCH 4558.7 decode / 6171 goodput; SHARED_PREFIX 4487.8 decode / 13293 delivered / 5816 physical. `power_source=UNAVAILABLE` | none. File last commit `8156ce1a` 2026-07-27 | MEASURED (unbound MLX logs). Energy: **UNMEASURED** | CHANGED. `docs/RUNTIME_AND_PERF.md` reprints 6512 tok/s from this family and then disclaims it. |
| `evidence/bench/runs-8bit.jsonl` | MLX 8-bit INTERACTIVE decode 309.4 t/s; BATCH 4644.9 / 6476 goodput | same | MEASURED (unbound) | CHANGED |
| `evidence/bench/quant-spec.jsonl` | 4-bit decode_tokens_per_s 392.9 / 4738.4 / 5581.9 at batch 1/64/256 | same | MEASURED (unbound) | CHANGED |
| `evidence/bench/quality-suite.jsonl` | token agreement / outcome_correct — quality, not throughput | same | MEASURED (quality) | CHANGED |
| `evidence/autonomous/hardware-characterization.json` | **BOUND to `fa47cafe`**. embed all-minilm **1634.4673** eps p99=7 ms load_ms=4544; llama-3.2-1b-q4 **262.42694** tps p99=5 ms load_ms=38606; ffmpeg-transcode **4277.5845** media_work_units/s p99=101; svg-scene-render **3.587947e8** px/s p99=1. peak_rss=3.72e9. thermal nominal. Single Mac15,14. | `fa47cafe` BOUND. Harness: `scripts/produce-hardware-characterization.py` (stamped `9756609e`) | MEASURED | **STILL TRUE for `agent/src/`** (`git log fa47cafe..HEAD -- agent/src/` empty). 9 other commits after `fa47cafe` (docs/readiness), none in `agent/src/`. Not the catalogue constant (304.2661 from r6). |
| `evidence/competitive-position.json` | "Metal 14.4ms TTFT at c=1 vs CUDA 181ms; CUDA 1617 tok/s at c=16 vs Metal 211 with 5.0s TTFT collapse" | none UNBOUND 2026-07-27 | MEASURED SHAPE (quotes CUDA 1617 from unbound A5000 sweep) | CHANGED; CUDA half must not be quoted as today |
| `control/pricing.go` `repricingBenchmarks[0].UnitsPerSec` | **304.2661** tokens/s, cites `candle-metal-llama1-q4-r6.json#batch_infer`, hardware `apple_silicon_ultra` | compile-time literal; citation gate `control/repricing_benchmark_authority_test.go` | DERIVED (conservative bound on r6 MEASURED) | CHANGED with r6 subject (`agent/src/` 3 commits). Comment 20 lines later still says **"Apple Silicon, 138.7 tok/s measured"** — stale comment vs live constant. |
| `control/pricing.go` `unpricedThroughputUntilBound` MiniLM | 1967.3141 embeddings/s, cites M3 Pro `#embed` | UNBOUND diagnostic, deliberately not priced | MEASURED (parked) | CHANGED |
| `pricing/board.json` | vendor USD/1k observations (OpenAI 0.02/1M embeddings, etc.), `fetched_at=2026-08-02`, positioning_multiplier=0.9 | market board, not a Merc bench | MODELED reference | n/a (not Merc runtime) |
| `ops/economics-readiness.json` | `latency_p50_ms: null` | review_basis_commit in file | UNMEASURED (explicit null) | — |

Binding sidecars (`*.binding.json`) under bench/ and runtime-benchmarks do not
state performance numbers.

---

## 5. Subject-code git logs (verbatim)

HEAD = `4b10b56e`. Atlas census HEAD `74dea5e1` **is an ancestor** (164 commits later).

### 5.1 Atlas named control paths — `74dea5e1..HEAD`

```
git log --oneline 74dea5e17dcc44c7790c4e6b41c48bbd59dd0f0d..HEAD -- \
  control/realtime.go control/realtime_store.go control/api.go control/quote.go \
  control/scheduler.go control/store_jobs.go control/store_tasks.go \
  control/store_workers.go control/verification_processor.go \
  control/verification_apply.go control/service_lease_api.go \
  control/service_leases.go control/workers.go control/collect.go \
  control/billing.go control/receipt.go
```

```
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
64da4293 alpha: backend-alpha release level, recovery and security suites, live staging money ingress
a1c0a55f ui: versioned composition surface for BUY/EARN/HEALTH, explicitly not a TUI (P8)
96ed6deb workload: accept and persist buyer objective as a binding field (P6)
382df92c compactness: drop the write-only device_slot cache (P2)
ff5a6531 liveness: make the offer→slot mapping self-verifying and detached writes ordered
375ab9be liveness: take the durable heartbeat write off the authenticated fast path (P0A)
42c9130d liveness: flag-gated selection flip — offer-grain index authoritative for realtime routing (G082)
da97ff4e liveness: re-key the live index per-offer so it matches the money-selection plane (G082)
65c004db liveness: shadow-wire the live-index in production + prove SQL-liveness parity (G082, inert)
6d9dcb55 hetero: acceptable-quality-contract gate for honest Metal/CUDA substitutability (G024, software prereq)
9c5a9f25 liveness: batched coalesced heartbeats ~2x a droplet's device ceiling; measured curve (G073/G081)
99a32fc9 wm/replay: per-phase predicted-vs-actual decomposition and shadow regret (G021/G053)
883af0a8 containment: the sandbox flag was a field the supplier set, and nothing checked who set it
ddff433e Merge branch 'grok/lease-contract-20260810-121911'
efac9aa2 lease: merc was selling elasticity it had no code path to deliver, and an SLO with no tail
14fba52f realtime: the branch only needed to know there were two offers, and it counted the whole book
c3b7a5f4 money: the netting rule was a string a future writer had to remember
9ad5bafc runtime+narrowing+money: name the basis, bound what can be bounded, restore what was debited
ca6987a3 money: admission held the ceiling, and refund handed the same cash back
6c145431 finality: a lane that cannot say it is final was saying nothing at all
db94746a verification: a buyer-visible claim could name an evaluator that never ran
8e6b1024 market+topology: record what actually cleared, and refuse what physics refuses
9a5ae9ba evidence: the digests were all there and nothing tied them into one chain
dd59aa62 money: prepaid admission never joined the lock the other two rails share
0f3202d0 deferral: the buyer paid for speed, and the claim path never read the promise
a7bf17a6 money: three rails disagreed about what a buyer already owes, and two under-held
c9d35603 step6: capability is what is true of a node, routability is what we allow it
```

COUNT=**28**. These include liveness flip, lease contract, offer-book counting
fix, verification evaluator, and quote-in-canary — all under stages the atlas
still describes at `74dea5e1` line numbers.

### 5.2 Gap-accounting — `2d31f092..HEAD -- control/realtime.go control/merc_latency_gap_accounting_test.go`

```
9c5a9f25 liveness: batched coalesced heartbeats ~2x a droplet's device ceiling; measured curve (G073/G081)
99a32fc9 wm/replay: per-phase predicted-vs-actual decomposition and shadow regret (G021/G053)
97ed15dd step5: freeze the cost policy and make currency a first-class denominator
26d91790 reduce: the audited true reductions, executed -- 5,482 authored lines, nothing else
64d41d64 docs: say which cited receipts are unbound, and flag a cost authority that rests on a withdrawn one
2f0c6e96 style: gofmt the files this wave touched
c8f9d531 fix: the gate that stops under-powered passes was computed under an assumption its own scheduler refutes
f21fc9c6 measure: close the latency accounting, and correct what this wave was aiming at
```

COUNT=**8**.

### 5.3 Authorize tails / hot-path subject — `2c8518dc..HEAD -- control/realtime_store.go control/authorize_tail_characterize_test.go`

COUNT=**24** (includes `14fba52f` whole-book count, G082 live-index, money-rail
locks, `6e23d3f9` "the authorize tail was one offer row", `84795c25` docs: the
single-hot-offer tail is a fixture artefact). First 15:

```
ff5a6531 liveness: make the offer→slot mapping self-verifying and detached writes ordered
375ab9be liveness: take the durable heartbeat write off the authenticated fast path (P0A)
42c9130d liveness: flag-gated selection flip — offer-grain index authoritative for realtime routing (G082)
da97ff4e liveness: re-key the live index per-offer so it matches the money-selection plane (G082)
65c004db liveness: shadow-wire the live-index in production + prove SQL-liveness parity (G082, inert)
9c5a9f25 liveness: batched coalesced heartbeats ~2x a droplet's device ceiling; measured curve (G073/G081)
90ff8869 test: schema-template + safe parallelism + integration tiering (G080, partial)
99a32fc9 wm/replay: per-phase predicted-vs-actual decomposition and shadow regret (G021/G053)
14fba52f realtime: the branch only needed to know there were two offers, and it counted the whole book
c3b7a5f4 money: the netting rule was a string a future writer had to remember
9ad5bafc runtime+narrowing+money: name the basis, bound what can be bounded, restore what was debited
ca6987a3 money: admission held the ceiling, and refund handed the same cash back
6c145431 finality: a lane that cannot say it is final was saying nothing at all
8e6b1024 market+topology: record what actually cleared, and refuse what physics refuses
dd59aa62 money: prepaid admission never joined the lock the other two rails share
```

Hot-path-free-admit has **no** `source_commit`, so this is the nearest named
subject path.

### 5.4 Segment latency — `0487c043..HEAD -- control/realtime_store.go control/merc_segment_latency_measure_test.go`

COUNT=**26** (same family as §5.3 plus envelope nanos vs micros `7f51e9dc`).

### 5.5 Hot-path profile — `1b016ec4..HEAD -- control/control_plane_hot_path_profile_test.go control/api.go control/realtime.go`

```
e37a25c1 alpha: canary mode no longer means no quotes, and staging runs current HEAD
64da4293 alpha: backend-alpha release level, recovery and security suites, live staging money ingress
a1c0a55f ui: versioned composition surface for BUY/EARN/HEALTH, explicitly not a TUI (P8)
96ed6deb workload: accept and persist buyer objective as a binding field (P6)
b89152ee perf: index the heartbeat capacity subquery, and land the P1 hot-path profile
```

COUNT=**5**.

### 5.6 Droplet / heartbeat ceiling — `90ff8869..HEAD -- control/heartbeat_ingest_bench_test.go control/store_workers.go`

```
382df92c compactness: drop the write-only device_slot cache (P2)
65c004db liveness: shadow-wire the live-index in production + prove SQL-liveness parity (G082, inert)
9c5a9f25 liveness: batched coalesced heartbeats ~2x a droplet's device ceiling; measured curve (G073/G081)
```

COUNT=**3**. `docs/DROPLET_SCALING.md` still presents the curve as the measured
control-plane ceiling (it does label PROXY / unbound snapshot).

### 5.7 Liveness index — `9c5a9f25..HEAD -- control/liveness_index_bench_test.go`

```
b1f593b4 liveness: compact in-process live-device index — 12.4 B/device, 3.5M hb/s, fail-closed (G082 core)
```

COUNT=**1**. The 12.4 B / 3.5M hb/s figures `docs/DROPLET_SCALING.md` quotes
predate the compact-index implementation commit.

### 5.8 Catalogue r6 candle — `926edfe3..HEAD -- agent/src/`

```
3012f8a3 alpha: land eight wave lanes, including an adversarial audit that found twelve things
8142e6e5 alpha: the control suite is green, and the execution loop closes through the public API
19fe0b23 alpha-cells-20260816-202101: preserve work from a lane that died without reporting
```

COUNT=**3**. Catalogue `304.2661` is therefore **not** still-true at HEAD.

### 5.9 M3 Pro 138.7 receipt stamp — `1de03bf0..HEAD -- agent/src/`

COUNT=**16**. The 138.7 number has no producer `source_commit` at all
(`missing_identity_fields` lists it).

### 5.10 Hardware characterization — `fa47cafe..HEAD -- agent/src/`

COUNT=**0**. This is the only MEASURED Merc runtime throughput receipt whose
named agent subject path has not moved.

---

## 6. Stale quotes (doc → artifact)

A quote is stale when a surface presents a number as current (or as the
authority the system "currently admits") and the source is bound to an older
commit, is UNBOUND, is superseded, or is self-flagged UNMEASURED.

| quote site | what it says as if current | artifact underneath | why stale |
|---|---|---|---|
| `docs/REMAINING_WORK.md` § Cloudflare move | "The latency atlas measures the dominant terms as a **20-second** queue deferral (`askDeferralWindow`) and same-buyer lock serialization at **59 ms p50 / 133 ms p99 at concurrency 32**" | `evidence/perf/latency-atlas.json` (prose) copying `hot-path-free-admit-latest.json` legacy c=32 (p50=59.137 p99=132.922) + scheduler constant 20s | **The atlas is UNBOUND at `74dea5e1` (164 commits behind HEAD).** 20s is a code constant, not a measurement. 59/133 is an UNBOUND c=32 lab cell with no `source_commit`. `realtime_store.go` has 24 commits since the nearest authorize SHA. This is the directive-quotes-old-atlas failure mode. |
| `docs/PROGRAMME.md` step 7 | "`batch_infer` constant at **138.7** against a measured 138.71389521 — conservative, so it **stands**" | `evidence/benchmarks/2026-07-01-m3-pro.json` | Live `repricingBenchmarks` is **304.2661** from r6, hardware ultra not pro. Step 7 text is a stale programme ledger. |
| `docs/PROGRAMME.md` ~3401 | "Supplier gross @**138.7** tok/s" / "M3 Pro measures **138.7** — underwater" | same M3 Pro receipt | Historical arithmetic; catalogue cell is M3 Ultra r6 304.2661. Hardware-characterization at HEAD-adjacent `fa47cafe` measures **262.43** tps on the same model — a third number, also not 138.7. |
| `docs/ARCHITECTURE.md` | "the M3 Pro measured **138.7**" and a boot WARNING block `measured 138.7` / `hw=apple_silicon_pro` | same + old `SupplierViabilityReport` print | Comment in `control/pricing.go:1830` still says the control plane "currently admits (Apple Silicon, **138.7 tok/s** measured)" while the live constant is 304.2661 ultra. |
| `control/pricing.go:1830` comment | "currently admits (Apple Silicon, 138.7 tok/s measured)" | M3 Pro UNBOUND receipt | Stale comment next to a different literal. |
| `docs/ALPHA_LAUNCH_READINESS.md` | Dockerfile must copy `evidence/perf/runtime-benchmarks/` "where the g070 llama **r5** receipt the catalogue cites at boot lives" | `candle-metal-llama1-q4-r5.json` (288.971, hash `f4303a75`) | Catalogue cites **r6** (`7cc01c44` / 304.2661). r5 is superseded. |
| `docs/RUNTIME_AND_PERF.md` § Speed lane | tables reprint **6512 tok/s**, 47×, 145× retired, 300× ladder (8300 / 12500 / 16100 / 18469 / 27700 / 41610) | `evidence/bench/runs.jsonl` unbound 2026-07-27 MLX | File itself later says "Current physical and delivered multiples are **unknown** under bound identity" and "must not be labelled as today's". Adjacent sections still read as current measured. Energy columns null (`UNMEASURED`). |
| `docs/RUNTIME_AND_PERF.md` contradiction ledger | MLX **6,828 t/s** vs **310.7 t/s** left side-by-side | speed-lane vs four-runtimes cross-test | Unresolved; neither is bound at HEAD. |
| `docs/RUNTIME_AND_PERF.md` § One capacity row | "multi-buyer single-offer authorize tail (**~50–75 ms p95** at c=32)" as 2026-08-03 **verdict** | `authorize-auth-tails-latest.json` (`2c8518dc`) | Bound, but 24 commits under `realtime_store.go` since, including G082 liveness and offer-count fix. Re-measure required before treating as current. |
| `docs/RUNTIME_AND_PERF.md` authorize floors | "from **59 ms → 3.2 ms p50**"; "Metal merc-owned was ~3.0 ms"; "Best authorize p50 is 0.96 ms" | `hot-path-free-admit-latest.json` UNBOUND no SHA + gap-accounting `2d31f092` | Quoted as "Measured floors (this worktree probe)". Source is UNBOUND / 164 commits behind atlas HEAD. |
| `docs/DROPLET_SCALING.md` | "~1,980 hb/s → **~89,107** devices"; coalesced 2 ms **~184,032**; live-index **~12.4 bytes/device**, **~3.5M hb/s** as "All numbers below are **measured**" | `droplet-device-ceiling.json` (`90ff8869` UNBOUND PROXY) + `liveness-index-bench.json` (`9c5a9f25`) | Doc does say unbound historical / PROXY. Index numbers predate `b1f593b4`. Heartbeat path has since left the authenticated fast path (`375ab9be`). |
| `docs/PROGRAMME.md` RunPod vLLM row | correctly says historical CUDA figures in `cuda-throughput-correction.json` / `cuda-a5000-ceiling.json` **must not be quoted as today's** | those UNBOUND files | Not stale (honest). Listed so CUDA 7081 / 1617 tok/s in other surfaces are judged against this. |
| `evidence/competitive-position.json` | CUDA **1617 tok/s** at c=16 vs Metal 211 | `cuda-a5000-qwen1.5b.json` UNBOUND | Same CUDA number PROGRAMME forbids quoting as today. |
| `ops/placement-readiness-contract.json` | `metal_pair_evidence` → `engine-parity-metal-embed-latest.json` | that file at `585fb2f4` | MEASURED but old candle SHA; later `644ccee6` twin exists. Placement contract treats it as current evidence. |
| `ops/acceptable-quality-contracts.json` | cites `engine-parity-metal-embed-latest.json`, `cuda-embed-arm-latest.json`, `candle-metal-llama1-q4-r4.json` | r4 superseded by r5 then **r6** | r4 is not the catalogue cell. |
| `ops/staging/alpha-participants.json` | sealed r6 hash `7cc01c44` (honest) | r6 | Not stale. |
| `docs/PROGRAMME.md` / `docs/RUNTIME_AND_PERF.md` five-cache | host microbench hit ~0.4 µs miss ~12 µs as current | `five-cache-architecture-audit.json` (`a16e9162`) | Old SHA; identity cache code has since been production-wired. Microbench not re-run at HEAD. |
| `docs/PROGRAMME.md` ~844 | "`merc-agent` binary registered **(1,980 embeddings/sec** measured on …" | M3 Pro embed 1967.3 and/or a rounded 1980 | Hardware-characterization now 1634.5 eps on ultra; three embed numbers in tree (1967 / 1634 / 4411 llama.cpp sweep UNBOUND). |

Surfaces that **correctly refuse** to treat old numbers as today (not stale):
PROGRAMME on CUDA tok/s; RUNTIME_AND_PERF "unknown under bound identity"
box; NETWORK_V2 on prefix-two-worker as same-host not network; PROGRAMME
paired-cohort WITHDRAWN.

---

## 7. Canonical atlas re-run list (12 stages) — harness exists or not

Rebuild covering: project analysis, quote, market selection, lease, runtime
startup, artifact transfer, model load, prefill, decode, verification,
settlement, control-plane overhead.

| stage | what would have to be re-run | harness | notes |
|---|---|---|---|
| **project analysis** | wall-clock of topology/compiler/plan at HEAD | **NO latency harness.** `control/topology_planner_test.go`, `control/plan_calibration.go` are correctness. Atlas has no `project_analysis` stage. | Never measured. |
| **quote** | `quote_build_and_persist` p50/p95/p99 at HEAD | **NO latency harness.** `control/quote.go` emits ETA `p50_secs` (planner / observed history) — that is a buyer ETA, not a control-plane wall clock. Atlas stage UNMEASURED. Instrumentation gap named at `quote.go:1062`. | Never measured. `e37a25c1` changed quote-in-canary after the atlas. |
| **market selection** | offer-claim + ranking + `market_clearing_receipt` at HEAD, multi-offer book | **Partial.** Exists: `control/merc_segment_latency_measure_test.go` (offer_claim, offer_count=1); `control/realtime_selection_decomposition_test.go` (opt-in SQL S0–S4); `control/selector_scale_curve_test.go` (shape, `source_commit=unknown`); `control/realtime_offer_book_branch_probe_test.go`; `control/hot_path_free_admit_probe_test.go`. | `market_clearing_receipt` UNMEASURED. G082 live-index is now on the selection path and was **not** in any of these receipts. Re-run all four plus a flag-ON Authorize cell. |
| **lease** | create / heartbeat / recover / failover / finalize / cancel / receipt_read | **NO e2e lease latency harness.** `control/service_lease_*_test.go` are correctness. `selector-scale-curve` has a `service_lease` SQL-shape lane (UNBOUND, `source_commit=unknown`). Atlas `sl_*` (11 stages) all UNMEASURED. Lease product code landed `efac9aa2` after the atlas. | Never measured as a path. |
| **runtime startup** | process start → serving, per engine/cell | **Partial.** `evidence/autonomous/hardware-characterization.json` records `load_ms` (embed 4544, llama 38606). `evidence/runpod/startup-diagnosis.json` is CUDA pod startup, not Merc agent load. | Characterization load_ms still-true for `agent/src/`. Not a control-plane stage clock. |
| **artifact transfer** | presign + GET input + PUT result | **NO latency harness.** `control/artifact_harness_test.go` is a real-storage **correctness** environment ("NO test-only artifact path"), not a timed transfer receipt. Atlas `task_dispatch_presign` / `agent_execute_and_upload` UNMEASURED. | Never measured. |
| **model load** | weights → ready, per cell | **Partial.** Same `load_ms` on hardware-characterization. Atlas `agent_execute_and_upload` UNMEASURED. No per-engine load receipt in `evidence/perf/runtime-benchmarks/` split out from throughput. | Re-run with r6+HEAD agent if used as atlas. |
| **prefill** | prompt-processing tok/s and TTFT split | **Yes (engine).** `scripts/bench-harness.py`, `merc-agent bench-batch`, `evidence/bench/runs.jsonl` `prefill_s`/`ttft_ms`. Catalogue r6 does **not** split prefill vs decode in the priced unit (settlement geometry is input+max_output). | MLX split is UNBOUND 2026-07-27. Re-run r6-class bench-batch at HEAD with prefill mark. |
| **decode** | decode tok/s / ITL | **Yes (engine).** Same harnesses. r6 decode-only diagnostic serial 232.91 t/s (not the priced 304.2661). | Re-run at HEAD (`agent/src/` 3 commits after r6). |
| **verification** | `verification_process` + L1/cosine/byte_exact | **Partial.** `control/render_verify_pipeline_bench_test.go` MEASURED Cycles-CPU L1 only (dirty SHA). Atlas `verification_process` UNMEASURED. Job verifier (`verification_processor.go`) has no percentile receipt. | Re-run render verify at a clean SHA; **write** a job-verification wall-clock harness — none exists. |
| **settlement** | `verification_settlement_apply` / realtime settlement / SLA+card | **Partial.** Segment harness has `settlement_finalize_ms` / `settlement_path_ms` (stub upstream, `0487c043`). Atlas `verification_settlement_apply`, `job_finalize_economics`, `job_sla_and_card_charge` UNMEASURED. Money tests exist; they do not emit p50. | Re-run segment at HEAD; no job-settlement latency harness. |
| **control-plane overhead** | TTFT residual, authorize, heartbeat ingest, auth lookup, selector | **Yes, stale.** `control/merc_latency_gap_accounting_test.go`, `control/merc_segment_latency_measure_test.go`, `control/hot_path_free_admit_probe_test.go`, `control/control_plane_hot_path_profile_test.go`, `control/heartbeat_ingest_bench_test.go`, `control/authorize_tail_characterize_test.go`, `control/liveness_index_bench_test.go`, `control/authorize_latency_remeasure_test.go` (opt-in, no committed receipt), `control/realtime_auth_latency_probe_test.go`. | Every committed receipt in this list is bound to a SHA behind HEAD (or has no SHA). G082 live-index and P0A heartbeat-off-fast-path **invalidate** using those numbers as current. Re-run the set at HEAD with producer identity. |

### Harness inventory (exists, by family)

Working (opt-in or CI) harnesses that **can** write a new atlas:

- Control-plane / admit: `control/merc_latency_gap_accounting_test.go`, `control/merc_segment_latency_measure_test.go`, `control/hot_path_free_admit_probe_test.go`, `control/authorize_tail_characterize_test.go`, `control/authorize_latency_remeasure_test.go`, `control/control_plane_hot_path_profile_test.go`, `control/realtime_auth_latency_probe_test.go`, `control/arrival_batch_perf_test.go`
- Selection / scale: `control/realtime_selection_decomposition_test.go`, `control/selector_scale_curve_test.go`, `control/realtime_offer_book_branch_probe_test.go`, `control/scheduler_sla_deferral_measure_test.go` (writes `/tmp`, **not** `evidence/perf/`)
- Liveness / heartbeat: `control/heartbeat_ingest_bench_test.go`, `control/liveness_index_bench_test.go`, `control/liveness_write_amp_bench_test.go`
- Engine prefill/decode: `scripts/bench-harness.py`, `merc-agent bench-batch`, `scripts/bench-report.py`, `scripts/realtime-parity-benchmark.py`, `scripts/runpod-cross-bench.sh`
- Embed: `scripts/engine-parity-metal-embed-measure.py`, `scripts/embed-throughput-sweep.py`, `scripts/cuda-embed-arm-measure.py`, `scripts/prefix-physical-kv-measure.py`, `scripts/prefix-two-worker-measure.py`
- Render: `control/render_verify_pipeline_bench_test.go`, `render/harness/device_placement_bench.py`, `control/blender_render_baseline_test.go`
- Energy: `scripts/energy-deltas-measure.py`
- Device characterization / load: `scripts/produce-hardware-characterization.py`
- Gateway: `control/gateway_parity_harness.go` + `control/gateway_parity_measure_test.go`

**No harness exists for:** quote wall-clock, project-analysis wall-clock, service-lease path clocks (11 atlas stages), artifact presign/transfer wall-clock, job `verification_process` / `verification_settlement_apply` / `job_finalize_economics` / `job_sla_and_card_charge` / `receipt_assembly`, `exact_reuse` / `coalesce` residual, `market_clearing_receipt`, `open_upstream_credential`, `placement_bind`, `price_bound_revalidation`, `offer_book_count`.

`control/scheduler_sla_deferral_measure_test.go` exists but does not land a
bound `evidence/perf/` receipt (default `/tmp/g054-claim-measure.txt`).

---

## 8. Ambiguity findings (asserted without class)

- Atlas `authorize_contract` stores the **c=1** 1.133 ms cell while atlas
  **prose** and `docs/REMAINING_WORK.md` discuss **c=32** 59/133 ms — same
  artifact, two numbers, only one in the stage table.
- Atlas `task_claim` copies prefix-KV **routing** claim latency, not
  `ClaimTasksTx` under the 20s SLA deferral the atlas ranks as structural
  risk #1.
- `hot-path-free-admit-latest.json` was rewritten from `UNBOUND_PROBE` to
  `UNBOUND` because a bespoke status "reads as bound to a human skimming".
- `selector-scale-curve.json` `source_commit=unknown`.
- `heartbeat-ingest-ceiling.json` and `droplet-device-ceiling.json` share one
  LFS OID.
- `vllm-cuda-raw.txt` first line says `compute device: Metal`.
- MLX 6,828 vs 310.7 left unresolved in RUNTIME_AND_PERF.
- Three embed throughputs in tree (1967 M3 Pro, 1634 characterization, 4411
  llama.cpp UNBOUND sweep) with no sentence saying which is current.
- Catalogue priced unit (304.2661 input+output) is not decode tok/s (232.91
  diagnostic) and not 138.7 and not characterization 262.43.

---

## 9. `python3 scripts/validate-readiness.py` — live output

Sparse checkout does not materialize `scripts/lib/` or `ops/`. The import was
satisfied from a **temp** copy of `scripts/lib/receipt_binding.py` via
`PYTHONPATH` (not written into the repo). The script then ran against this
tree:

```
readiness: candidate 4b10b56e0c491c2e9fdb652344d07020c78cd43f (ops/candidate.json absent; falling back to HEAD)
readiness: code-drift OK (no code changes since candidate)
readiness: FAIL: cannot load readiness ledgers: [Errno 2] No such file or directory: '/Users/scammermike/.claude-grok/worktrees/HYPER-G-atlas-20260823-234256/ops/readiness.json'
```

A FAIL on a missing ledger / declared-score mismatch is expected in this
sparse tree and is another lane's business. `ops/readiness.json` is the path
that would need widening to get a score. This lane did not materialize it
(`git sparse-checkout add` is forbidden here).

---

## 10. Counts (how the completion line is computed)

**104 performance artifacts** that state a Merc latency / throughput / memory /
token-rate / frame-rate / cost-per-unit number:

- 96 `evidence/perf/**` LFS objects (working-tree pointers; 96/96 readable from
  parent LFS cache)
- 4 `evidence/bench/*.jsonl`
- 2 `evidence/benchmarks/*.json`
- 1 `evidence/competitive-position.json`
- 1 `evidence/autonomous/hardware-characterization.json`

Docs, `control/pricing.go` literals, and `pricing/board.json` are quote or
DERIVED surfaces, not additional measurement artifacts.

**Still true at HEAD (subject paths empty):** 1 artifact —
`evidence/autonomous/hardware-characterization.json` (`fa47cafe` /
`agent/src/` COUNT=0). Six numbers: embed 1634.4673 eps, infer 262.42694 tps,
transcode 4277.5845 u/s, render 3.587947e8 px/s, load_ms 4544 / 38606.

**Subject code changed underneath:** 78 artifacts that (a) carry a resolvable
SHA or a named subject path and (b) are not the characterization receipt.
Includes the atlas, every copied MEASURED atlas source, r6 catalogue cell,
authorize tails, droplet curve, liveness index.

**Never measured:** 38 atlas stages listed UNMEASURED, plus energy in
`evidence/bench/runs.jsonl` (`power_source=UNAVAILABLE`), plus
`ops/economics-readiness.json` `latency_p50_ms=null`. The 38 are the
canonical "never measured" inventory for a rebuilt atlas.

**Unreadable (LFS pointer):** 0 unrecovered. 96 working-tree files **are**
pointers; content was recovered from the parent `LocalMediaDir` without pull
or prune. Without that cache this entire `evidence/perf/**` corpus is
UNREADABLE HERE.

Nothing in this report is a licence to optimize against the copied atlas
percentiles, the 138.7 viability story, the 59/133 ms c=32 cell, the 6512
tok/s MLX logs, or the CUDA 1617/7081 tok/s sweeps.
