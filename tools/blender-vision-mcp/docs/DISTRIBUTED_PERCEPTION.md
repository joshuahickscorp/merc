# Distributed perception fabric

Perception uses the existing authenticated pull-worker scheduler rather than a
parallel orchestration system. Workers advertise a class, hardware, memory,
models, render devices, and allowlisted capabilities. `vision.observe` accepts
`execution=distributed`; the coordinator then requires both
`perception.capture` and the exact `adapter.<name>` capability.

The allowlisted worker operations are:

- `perception.capture` for the governed sensor bus;
- `perception.workspace` for specialist analysis and routing;
- `perception.verify` for a worker-side verification followed by mandatory
  central verification.

Jobs use expiring authenticated leases, heartbeat-based availability, bounded
retry attempts, content-addressed input locality, and resumable digest-verified
artifact transfer. A worker completion cannot promote its own assertion: capture
completion is accepted only when the coordinator can reload the capture, match
the leased adapter, and independently verify its observation envelope.

The packaged worker path operates on a mounted portable project. Network workers
use the same MCP lease and chunk-transfer methods; artifact uploads are accepted
only after declared size and SHA-256 verification. Device loss requeues work
without rewriting observation authority. Identical governed inputs may have
different job/worker receipts, but they must resolve to the same capture and
manifest digest to be considered receipt-equivalent.

The test suite routes acquisition, workspace analysis, and central verification
through distinct advertised workers, then repeats acquisition on a second worker
and requires an identical capture and manifest digest. Actual Mac Studio/DGX
cross-device timing and hardware-specific raster equivalence remain an empirical
deployment benchmark, not a simulated claim.

## Real-process and physical-runtime benchmark

The fixed distributed runtime benchmark launches installed Chromium, managed
WebKit, and installed Blender, then starts two independent spawned OS worker
processes against one portable WAL-backed project:

```bash
BVMCP_RUN_PROCESS_TESTS=1 uv run bvmcp benchmark \
  bootstrap-distributed-runtime \
  --output artifacts/distributed-runtime
```

The first child claims a real 15-second authenticated lease and exits abruptly
with the manifest-fixed device-loss code. Its lease remains in the database. The
coordinator waits for the real lease deadline, reaps it, and requires the job to
return to `queued`. A second child with a different PID and worker credential
then claims attempt two, executes the allowlisted job, and binds the final result
and provenance to that worker. Worker tokens and lease tokens are never written
to the receipt.

The receipt distinguishes `functional_passed` from `complete`. Distinct PIDs
prove process isolation, not physical-host isolation. Unless a digest-bound
receipt from a different host is supplied, `second_physical_host` remains
`BLOCKED_EXTERNAL`. Likewise, a missing requestable WebGPU adapter remains
`BLOCKED_EXTERNAL`; the benchmark does not turn on unsafe emulation flags.

To resume the second-host gate, run the same source SHA and benchmark on another
physical machine, copy its receipt, then set both:

```bash
export BVMCP_SECOND_HOST_RECEIPT=/absolute/path/distributed-runtime.receipt.json
export BVMCP_SECOND_HOST_RECEIPT_SHA256=<sha256>
```

The receiving host rejects a stale source revision, substituted digest, symlink,
failed receipt, or its own host identity.
