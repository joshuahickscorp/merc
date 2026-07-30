# Runtime GGUF closure — baseline before modification

Recorded before any edit in this pass, so every later claim has a fixed point to
be measured against.

```
branch      perf/execution-frontier
sha         160afc5f9ab0b78eef26f064ec3bb9ef2fb3876b
tree        clean
```

## Test baseline

| Suite | Command | Result |
| --- | --- | --- |
| Go control plane | `MERC_ALLOW_SKIPPING_DB_TESTS=1 go test ./...` | `ok merc/control` |
| Rust agent | `cargo test` | 106 passed, 0 failed, 0 ignored |

## Authority document

`control/runtime-authority.json`, `schema_version: 2`, matrix `2026-07-30.3`,
carries `runtimes[]`. Lifecycle at baseline:

| engine | lifecycle |
| --- | --- |
| candle | ACTIVE |
| mlx | VALIDATED |
| llama_cpp | VALIDATED |
| vllm | DRAFT |

## Claimed fixes — verified, not assumed

The instruction listed ten fixes to confirm are really present. Checked against
source rather than against the previous report:

| Claim | Status | Evidence |
| --- | --- | --- |
| Schema v2 uses `runtimes[]` | **PRESENT** | `schema_version: 2` and a `runtimes[]` array |
| `terminate()` retries and verifies absence | **PRESENT** | `scripts/runpod-vllm.sh` loops `1 2 3`, calls `pod_exists`, prints `(verified)` only when the pod is gone, and prints a loud `!! FAILED TO TERMINATE` otherwise |
| `down-all` iterates then re-lists | **PRESENT** | terminates every id, then `list_pods` |
| MLX listen backlog ≥ 256 | **NOT FOUND** | no `backlog` setting exists on any MLX path; the only matches in the tree are unrelated `webhook_backlog` metrics in `scripts/local-resilience-rehearsal.sh` |
| RunPod stale env files deleted before provisioning | **NOT FOUND** | no `rm`/`unlink` of stale pod-id, URL, port, status or env files in `scripts/runpod-vllm.sh` |

Two of the ten claimed fixes are not in the tree. They are recorded here as
absent rather than carried forward as done; the remaining five claims
(agent schema-v2 projection, Go-exercises-Rust-contract, harness measures
determinism, failed provision cannot report success, transport overhead
measured) are still to be checked in this pass.

## Provider state

```
bash scripts/runpod-vllm.sh list
  balance $16.87
  no pods running
```

Zero pods at baseline, verified against the provider rather than against a local
state file. Nothing is billing.

## Artifact confound carried in

This is the gap the closure exists to fix:

| engine | artifact at baseline |
| --- | --- |
| candle | pinned q4 GGUF |
| llama.cpp | pinned q4 GGUF |
| MLX | a different 4-bit conversion |
| vLLM | **HF bf16, not the pinned GGUF** |

The vLLM rows in the current report are therefore not exact-artifact results and
must not be cited as closing the CUDA gap.

## Claim boundary

Baseline only. No throughput figure in the previous report is re-measured in
this document; the reported history is carried forward as **unverified** until
it is re-run against the canonical manifest frozen later in this pass.
