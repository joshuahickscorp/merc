# RunPod support escalation — pods allocate and bill, never get runtime

## Request

Why do allocated pods never receive runtime placement on this account, and what must change on our side to make them schedule?

## Account (as observed via API)

| Field | Value |
| --- | --- |
| Client balance (before series) | $17.1154584262 |
| Client balance (after series) | $17.0869980669 |
| Spend limit | $80 |
| Minimum balance | $0 |
| Current spend per hour | $0 |
| `machineQuota` | **0** |

Balance, spend limit, and minimum balance were all satisfied during the attempts. Diagnosis recorded at `2026-07-29T01:53:04.710129+00:00` (UTC).

**Question:** `machineQuota` reads `0` on this account. Does that field gate on-demand pod placement for renters, or does it apply only to hosting/supply-side machines? If it is relevant to renting, what action is required on our side to set a non-zero quota?

## Problem

Pods are created, accepted, and billed, but never receive runtime placement.

- `desiredStatus` reports `RUNNING` (request intent).
- `runtime` stays `null` indefinitely (no placement / no agent-reported runtime).
- Observed waits without runtime: up to **585 seconds** (pod `56bbql1zb391wc`) and **180 seconds** (pod `oljnclfjewy5m6`).

All pods were torn down afterward. The account currently shows **zero pods** and **$0/hr** spend rate.

## Attempt log

### Prior series (`startup-diagnosis`)

| Pod ID | GPU | Cloud | API | Image | Runtime | Outcome | Amount recorded |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `pww27g05kgd6zy` | A100 80GB PCIe | ALL | GraphQL | `vllm/vllm-openai:v0.6.3.post1` | `null` | never started | **$0.26** charged |
| `bwwg34zwuq9idd` | A100 80GB PCIe | ALL | GraphQL | `vllm/vllm-openai:v0.26.0-cu129-ubuntu2404` | `null` | never started | **$0.20** charged |
| `c4oqcp8b14tirf` | RTX A5000 | SECURE | REST | `nvidia/cuda:12.4.1-base-ubuntu22.04` | `null` | machine allocated, container never reported | $0.27/hr (rate only; no total recorded) |

Prior series total charged (as recorded): **$0.46**.

### Later series (`pod-scheduling-diagnosis`)

| Pod ID | GPU | Cloud | API | Image | `desiredStatus` | Runtime | Waited | Outcome | Rate |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `56bbql1zb391wc` | NVIDIA RTX A4000 | COMMUNITY | REST | `nvidia/cuda:12.4.1-base-ubuntu22.04` | `RUNNING` | `null` | 585 s | never started; torn down | $0.17/hr |
| `oljnclfjewy5m6` | NVIDIA RTX A4000 \| A5000 \| A40 | SECURE | REST | `nvidia/cuda:12.4.1-base-ubuntu22.04` | `RUNNING` | `null` | 180 s | never started; torn down | $0.27/hr |

Account spend delta for the later series (balance before − after): **$0.0285**.

## Teardown confirmation

After teardown:

- Pods listed: **0**
- Current spend per hour: **$0**

## Already ruled out

Please do not suggest re-trying these; each was already exercised without a non-null `runtime`:

| Variable | Values tried |
| --- | --- |
| GPU class | A100 80GB PCIe; RTX A4000; RTX A5000; A40 (including multi-class request A4000\|A5000\|A40) |
| Cloud type | ALL; COMMUNITY; SECURE |
| API | GraphQL (`podFindAndDeployOnDemand`); REST (`rest.runpod.io`) |
| Image | `vllm/vllm-openai:v0.6.3.post1`; `vllm/vllm-openai:v0.26.0-cu129-ubuntu2404`; plain `nvidia/cuda:12.4.1-base-ubuntu22.04` (no custom entrypoint override on the plain CUDA attempts) |
| Account funds | Balance above min; spend limit $80; min balance $0 |

## What we need from you

1. Why pods on this account reach accepted/`desiredStatus: RUNNING` and incur charges while `runtime` remains `null` indefinitely.
2. What must change on **our** side (account setting, verification, quota, region, product flag, or other) so a newly created pod receives runtime placement.
3. Clarification of whether `machineQuota: 0` is expected for a renter account and whether it blocks scheduling.
4. Refund or credit guidance for the amounts charged on pods that never started (`$0.46` prior series + `$0.0285` later series, as recorded).
