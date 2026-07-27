# Which lane can actually pay a supplier

Three independent web-enabled studies, commissioned 2026-07-26, on pricing
conventions, the throughput/watt frontier, and demand for verified compute. All
three converged, which is the reason to take the conclusion seriously.

## The arithmetic that forces the answer

Moving the catalogue from cost-plus to a market board fixed a price that was
~460× above market — and cut supplier gross by the same factor. On the hardware
the control plane admits:

| | Cost-plus | Market board |
|---|---:|---:|
| Price, Llama-3.2-1B | $4.1386 / 1M | $0.0090 / 1M |
| Supplier gross @138.7 tok/s | $2.004 / hr | $0.00436 / hr |
| Electricity (30 W @ $0.15/kWh) | $0.0045 / hr | $0.0045 / hr |
| **Supplier net** | **+$2.00 / hr** | **−$0.00014 / hr** |

Required throughput at the market price, 97% supplier share:

| Supplier net | Aggregate tok/s needed | Reality |
|---|---:|---|
| electricity only | 143 | M3 Pro measures **138.7** — underwater |
| $0.25 / hr | 8,098 | one fully-loaded H100 at SOTA serving |
| $1.00 / hr | 31,962 | multi-GPU continuous-batching cluster |
| $2.00 / hr | 63,781 | multi-node datacenter |

TensorRT-LLM on an H100 at FP8 peaks near ~11k aggregate output tok/s for a
6B-class model — about **$0.25–0.35/hr** of supplier gross, and only if the card
is sold 100% of the time. That is not consumer-fleet economics; it is selling a
whole H100 at near-theoretical decode density.

## What the studies concluded

**Pricing.** There is no stable price point at which a heterogeneous consumer
fleet, priced per token, both pays owners above electricity and undercuts
RunPod/Vast — because market token prices already sit near datacenter marginal
cost with continuous batching. GPU-*hour* rental (the Salad/Vast shape) does work
for consumer NVIDIA hosts. **That is not a token market.**

**Speed.** Closing the engine gap (Candle static batching → vLLM/SGLang/TRT
continuous batching) is real and large — roughly **5–50×** — but it does not close
the *economic* gap at $0.009/1M for small models. It makes you a better commodity
seller of already-raced tokens.

**Speculative decoding is a clear no.** It helps latency at low batch and
generally *hurts* throughput at high batch, which is the wrong direction
entirely: payout economics are driven by aggregate throughput, not per-request
speed. It cannot deliver 8k–64k tok/s.

**A generic "we are faster" story is not a moat.** Cerebras (~1,800 tok/s on
Llama 3.1 8B) and Groq (~600 tok/s on 8B-class) already sell per-request speed
that a marketplace of spare machines will not match.

**Demand.** "Verified compute" as defined here — receipts, hash-chained stream
evidence, honeypots, an immutable ledger — is **not a standalone market**. No
public buyer RFPs or procurement language name marketplace-style cryptographic
receipts as the purchased good. It is a *feature* that lets strangers settle,
analogous to escrow. What buyers do pay for is **confidential computing / TEE
isolation** (data-in-use privacy, ~20–30% instance premium in regulated settings,
figure still being traced to SKU pricing) and **compliance posture** as a
procurement gate. Those are different products.

## The lane that does work

**Deadline-tolerant GPU rendering.**

| | Value |
|---|---|
| RTX 4090 OctaneBench | ~1,300 OB |
| Farm clearing rate (GarageFarm low GPU tier) | $0.004 / OB·hr |
| **Gross capacity value** | **~$5.20 / hr** |
| Electricity @ 400–450 W | ~$0.06–0.07 / hr |
| After a 20–40% take and partial utilisation | still **dollars per hour** |

Against **$0.0044/hr, underwater** for AI inference. Roughly three orders of
magnitude better per supplier-hour.

And the shape matches what this codebase already is: embarrassingly parallel by
frame, deadline-tolerant, output-verifiable by pixel hash, retries cheap. The
scheduler was built for exactly this — 45-second task sizing, `FOR UPDATE SKIP
LOCKED` claiming, hedge tasks, straggler requeue, per-chunk settlement. The
verification machinery that has no buyer in AI has an obvious use here: frames
are the artifact and a reference frame is a natural honeypot.

Secondary lane: **Mac/iOS CI**, where Apple hardware is a licensing requirement
rather than a cost disadvantage — the one place this project's Apple-only
supply constraint is an asset instead of a liability.

Risks the research names honestly: DCC licences and plugins, VRAM floors, NDA and
content security (TPN), colour management, and support burden. **Render farms
sell operations, not raw FLOPs.**

## What this does not say

It does not say to pivot today. It says the AI-inference token lane cannot pay
suppliers at market prices on any hardware this project can realistically
aggregate, and that a lane exists which can. The batch machinery, the settlement
ledger and the verification apparatus all carry over — the workload changes, not
the platform.

Sources, fetch dates and the full arithmetic are in the three commissioned
reports under `~/.claude-grok/tasks/consult-20260726-1551*/`.
