# The CUDA chain: what is left, and exactly how to run it

Step 15's harness is proven with real money — a governed pod provisioned, served,
tore down verifiably and produced an admissible spend receipt for $0.01
(`evidence/runpod/spend-rr7b6uwmivaolh.json`). What has never run is the **Merc
chain** through CUDA: a buyer request that produces a quote, a routed execution on
an NVIDIA host, verification, a buyer charge, a supplier payable, a positive Merc
contribution and a receipt.

This file exists because working that out took a session's worth of reading, and
none of it should have to be worked out twice.

## The insight that makes it tractable

**No agent binary is needed on the pod.** The realtime lane does not poll; Merc
calls OUT to a registered worker offer's `upstream_base_url`. So the pod runs
nothing but vLLM, and a plain HTTP registration puts it in the fleet. That removes
the two hard dependencies an earlier plan assumed: cross-compiling `merc-agent` for
`x86_64-unknown-linux-gnu`, and a publicly reachable control plane for the pod to
call back into. Merc reaches RunPod's proxy URL; RunPod never reaches Merc.

## What the pod must serve

`control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json` pins all of it, and
the offer is refused if the pod serves anything else:

```text
image      vllm/vllm-openai@sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0
model      unsloth/Llama-3.2-1B-Instruct
revision   5a8abab4a5d6f164389b1079fb721cfab8d7126c
alias      cx-chat-1b
dtype      bfloat16, tensor_parallel_size 1, max_model_len 32768
profile id vllm-llama-3.2-1b-instruct-bf16-tp1
```

This digest corresponds to **v0.23.0**. The provisioner defaults to this admitted
profile; an override must also be an immutable OCI digest. The first paid run used
a mutable v0.26.0 tag and Qwen, which is why it proved the harness and not the
lane.

## The registration contract

`POST /v1/worker/realtime/register`, worker-token auth, strict JSON — an unknown
field is a 400, not a warning (`RealtimeOfferRegistration` in
`control/realtime_store.go:24`):

```json
{
  "runtime_profile_id": "vllm-llama-3.2-1b-instruct-bf16-tp1",
  "runtime_profile_sha256": "<the profile digest the control plane computed>",
  "hw_class": "nvidia_24gb",
  "gpu_count": 1,
  "memory_gb_per_gpu": 24.0,
  "memory_gb_in_use": 0.0,
  "upstream_base_url": "https://<pod>-8000.proxy.runpod.net/v1",
  "upstream_token": "<MERC_GPU_API_KEY>",
  "warmth": "HOT",
  "max_active_sequences": 128,
  "available_sequences": 128,
  "supplier_input_usd_per_million_tokens": 0.08,
  "supplier_output_usd_per_million_tokens": 0.30
}
```

Supplier rates must sit under the profile's buyer rates (0.12 / 0.45) or the offer
is underwater and admission should refuse it — which is itself worth asserting.

`interconnect` stays absent at `gpu_count: 1`. Merc refuses to guess an
interconnect and refuses a multi-GPU offer that does not declare one.

Heartbeat is `POST /v1/worker/realtime/heartbeat` with
`{runtime_profile_id, warmth, available_sequences, status}` — `status` one of
ACTIVE / DRAINING / FAILED / QUARANTINED, `warmth` one of HOT / WARM / CACHED /
COLD. An offer that stops heartbeating drains.

## The run

1. **Provision, governed.** The cap is the money bound and the only thing between a
   hung run and the balance:

   ```bash
   # First query the current secure-cloud rate; the script refuses unless this
   # value matches RunPod exactly before and immediately after creation.
   MERC_RUNPOD_GPU="NVIDIA RTX A5000" MERC_RUNPOD_CLOUD=SECURE \
   MERC_RUNPOD_COST_PER_HR=<current-provider-rate> MERC_RUNPOD_CAP_USD=2.00 \
   MERC_VLLM_IMAGE="vllm/vllm-openai@sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0" \
   MERC_VLLM_MODEL="unsloth/Llama-3.2-1B-Instruct" \
   MERC_RUNPOD_EXPERIMENT_CMD="bash scripts/cuda-chain-drive.sh" \
   bash scripts/runpod-vllm.sh experiment
   ```

   The driver runs INSIDE the experiment so the pod is torn down and the spend
   receipt written however the driver exits.

2. **Control plane.** Boot it against a scratch database, as
   `scratchpad/stageenv.sh` did: `DATABASE_URL` to a fresh database with
   `control/schema.sql` applied, `MERC_PAYMENT_MODE=test`,
   `MERC_SETTLEMENT_CURRENCY=cad`, `MERC_CANARY_MODE=false` with a decision ref,
   `MERC_VERIFICATION_SAMPLE_SECRET` and `MERC_TOKEN_KEY` set to 32+ bytes, and —
   because the board is a USD reference against CAD settlement —
   `MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE` with `MERC_PRICE_FX_REVISION`. Neither
   the application nor a script may invent that rate; it is an operator input and
   the revision string should say so.

3. **Identities through the real routes, not through the database.** A chain proof
   that seeds its own worker row proves nothing about enrolment. Buyer signup →
   API key → funding; supplier enrolment code → worker credential → worker token.

4. **Drive it.** `POST /v1/chat/completions` with `Idempotency-Key` and
   `X-Merc-Max-USD`, model `cx-chat-1b`.

5. **Assert the whole chain, from the ledger and the receipt** — not from the HTTP
   response:

   * one contract, `CAPTURED`, charge ≤ the accepted ceiling;
   * exactly one supplier credit, to the offer's supplier;
   * buyer debit − supplier credit = platform take, and that take is **positive**;
   * the receipt names the runtime profile, the model revision and the
     `stream_root_sha256` / `output_commitment`;
   * usage reconciles against what vLLM reported in the final chunk, and
     `realtime.go` forces `stream_options.include_usage` precisely so it can.

## What this will and will not prove

It proves the CUDA lane end to end for ONE profile on ONE host, which is what
promotes `vllm_cuda` off `DRAFT` and gives the RuntimeSelector a second hardware
class to compare on — currently every measurement in
`evidence/perf/selector/paired-cohort-embed.json` is `apple_silicon_ultra`, and the
cost model refuses to compare across hardware classes for good reason.

It does not prove TP>1, does not prove a fleet, and does not close `P1-STRIPE-TEST`
or any of the other seven external gates.

## Do not

* Do not run the driver outside `runpod-vllm.sh experiment`. The cap, the lifetime
  bound, the pre-flight refusal, the sweep-on-any-exit and the spend receipt all
  live there, and a pod nobody remembers to stop is the failure that costs money.
* Do not register an offer whose supplier rates exceed the profile's buyer rates
  and then read a negative contribution as a pricing defect.
* Do not promote the cell from this run alone. The promotion gate wants production
  decisions, twenty samples on one hardware class, zero verification failures and a
  margin — `control/runtime_cell_promotion.go` refuses anything less, and it should.
