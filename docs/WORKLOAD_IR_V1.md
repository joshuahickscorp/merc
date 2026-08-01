# Merc Workload IR v1

Workload IR is the local, pre-upload description of a project. It is a proposal,
not a quote, order, job, or permission to execute. Version 1 deliberately refuses
to manufacture runtime, resource, economics, privacy, or quality authority when
the project does not contain it.

## Safe workflow

1. Run `merc project compile --root PROJECT`.
2. Review the complete JSON proposal and its `ir_sha256`.
3. Approve that exact digest.
4. Run `merc project compile --root PROJECT --probe
   --buyer-approved-ir-sha256 SHA256`.

The second command rescans the project and independently reconstructs the
unprobed IR. It refuses before probing unless that digest equals the buyer's
approved digest. Changing a file, detector result, graph edge, unknown, or
refusal invalidates the approval.

The v1 probe is `NON_EXECUTING_FILE_SHAPE_V1`. It executes no project code,
starts no container, opens no network connection, and reads at most 1 MiB from
the already bounded static-inspection sample. JSONL/NDJSON observation is capped
at 256 records per artifact.

## Canonical identity

`project_sha256` commits to every included relative path, byte length, and full
artifact SHA-256. `ir_sha256` commits to the canonical JSON IR with only its own
`ir_sha256` field cleared. The compiler sorts file paths, detector kinds,
evidence, observations, and refusals before hashing.

Ignored build/cache directories are `.git`, `.hg`, `.svn`, `node_modules`,
`vendor`, `target`, `dist`, and `build`. Their omission is deterministic and is
part of the v1 boundary.

## Resource bounds and refusals

Static inspection admits at most 10,000 regular files, 4 GiB per artifact, and
64 GiB total. Detector content inspection is capped at 256 KiB per file and
16 MiB total. Full bytes are streamed only to bind artifact hashes.

The compiler refuses symlinks and non-regular files because their target may
escape or change the reviewed project boundary. It refuses common credential
files (`.env*`, private-key files, package registry credentials, SSH keys) and
asks for a redacted inventory. Privileged or host-network container authority is
recorded as an unsafe refusal.

An unknown detector or a detector below the 0.70 confidence floor never becomes
execution authority. Detected steps remain `UNRESOLVED_REFUSE` or
`UNCALIBRATED_REFUSE` until a later compiler revision can bind them to governed
runtime/model contracts and measured outcome cohorts.

## Graph fields

Each IR contains:

- steps and dependencies;
- input and output artifact references;
- runtime and model contracts;
- resource and parallelism estimates;
- checkpoint and verification policy;
- privacy, egress, data location, quality, and deadline;
- probe authorization and observations;
- cost/duration estimate state and calibration basis;
- unknowns and refusal reasons.

Version 1 produces a stable graph for supported detector signals but keeps the
economics state at `UNCALIBRATED_REFUSE`. Detector confidence is not cost or
duration confidence, and neither can affect pricing, reserve, settlement, or
admission.

## Calibration gate

`merc project calibration-check --cohort COHORT.json` evaluates outcome-linked
evidence for one exact `ir_sha256`. It exits nonzero and emits every refusal when
the cohort does not clear the gate. A promotable cohort requires:

- at least 100 `PRIMARY_EXECUTION` observations;
- one explicit supported currency and a source-receipt SHA-256;
- `TRUE_NET_COMPLETE` cost observations, not supplier gross, platform ledger
  rows, or known-cost contribution presented as net cost;
- median absolute cost error no greater than 10%;
- p90 absolute cost and duration error no greater than 20%;
- at least 95% of actual complete cost within the frozen buyer ceiling.

The result binds the cohort bytes to `calibration_cohort_sha256` and reports
cost and duration confidence as `1 - p90_error`. Passing this evidence permits a
later compiler revision to use it for estimates only. The gate has no caller
from pricing, reserves, settlement, or admission.

## Detectors

The v1 static detector taxonomy covers realtime inference, batch inference and
compute, embeddings, structured extraction, media rendering, image/video,
LoRA, model evaluation, bounded containers, and service deployment. Evidence is
the set of project-relative files carrying each signal. Two independent files
raise a detector from 0.55 to 0.72; this is only a proposal confidence and does
not assert that the inferred graph is runnable.

Detector ordering never creates graph edges. Without an explicit, validated
dataflow declaration, detected steps are independent and the IR records step
dependencies as unresolved. A sorted filename or detector name is not evidence
that one workload consumes another's result.

## Explicit project declaration

A project may include a root `merc.project.json`. This is the buyer's proposed
dataflow and constraints, not server authority. It contains version 1, 1–256
steps, privacy, quality, optional RFC3339 deadline, result policy, and economics.
Steps name explicit inputs, outputs and dependencies; the compiler rejects
missing dependencies, self-dependencies and cycles, then sorts the DAG by step
ID for canonical identity.

Each declared step must use a supported detector kind, bind proposed runtime and
model contracts by SHA-256, require a bounded resource probe, declare
`INDEPENDENT`, `TIGHT`, or `SINGLE_DEVICE` parallelism, and name checkpoint and
verification policy. A declared kind with no independent static detector signal
is retained in the graph but produces a refusal.

`merc project contracts` prints the exact contract pairs the running binary can
resolve from its embedded activation authority. Each row includes workload kind,
runtime/profile/cell identity, model identity, lifecycle, verification contract,
and the two SHA-256 values used in `merc.project.json`. Compilation resolves a
step only when exactly one currently routable cell matches both digests and the
workload kind, and when the declared verification contract equals that cell's
contract. A well-formed but unknown hash remains refused. The current catalog
contains only the genuinely advertised batch-inference and embeddings cells;
rendering, LoRA, and service declarations cannot resolve until governed cells
for them exist.

Economics contains an ISO currency and a positive
`maximum_buyer_price_nanos`. Fixed-point nanos avoid a float or integer-unit
rounding change before a price exists. The buyer must leave `supplier_floor` and
`merc_contribution` as `UNRESOLVED_REFUSE` and may not provide a
`pricing_decision_sha256`; only server pricing authority may resolve those.

Example skeleton:

```json
{
  "version": 1,
  "steps": [{
    "id": "extract",
    "kind": "structured_extraction",
    "depends_on": [],
    "inputs": ["project://input"],
    "outputs": ["project://records"],
    "runtime_contract": "<sha256>",
    "model_contract": "<sha256>",
    "resource_estimate": "BOUNDED_PROBE_REQUIRED",
    "parallelism": "SINGLE_DEVICE",
    "checkpoint_policy": "NOT_APPLICABLE",
    "verification": "schema-v1"
  }],
  "privacy": {"egress": "DENY", "data_location": "CA"},
  "quality": {"requirement": "buyer-fixture-v1", "verification": "independent"},
  "result": {"contract": "artifact-set-v1", "retention": "30d", "delivery": "object-store"},
  "economics": {
    "currency": "cad",
    "maximum_buyer_price_nanos": 50000000,
    "supplier_floor": "UNRESOLVED_REFUSE",
    "merc_contribution": "UNRESOLVED_REFUSE"
  }
}
```
