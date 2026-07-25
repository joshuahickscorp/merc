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
