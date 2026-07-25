# Security review and threat model

Review date: 2026-07-20. Scope: Blender Vision MCP 0.1.0, its CLI/MCP coordinator, portable project
store, review server, Blender worker, model registry, and distributed-worker protocol.

## Security properties

- Safe mode is the default. Blender is launched with `--factory-startup`, `--disable-autoexec`, and
  a fixed worker entry point; a `.blend` file cannot supply Python to the coordinator.
- The Blender worker accepts only a versioned, SHA-256-bound manifest and an explicit operation
  allowlist. Every scene, result, checkpoint, render, and export path is confined to one project.
- The coordinator has no automatic model-download path. A checkpoint is usable only after a named
  source/license approval, an expected SHA-256, and an explicit import from local storage.
- References are copied into an immutable artifact tree. Structured logs contain paths and hashes,
  not reference pixels. Acceptance rejects unknown rights state and incomplete provenance.
- MCP worker enrollment is off until a strong enrollment secret is configured. Worker and lease
  tokens are stored as hashes, compared without exposing the stored secret, and scoped to their
  protocol actions. Leases expire and have bounded retries.
- Artifact uploads are authenticated, sequential, path-confined, size-bounded, and SHA-256 checked
  before registration. Rejected or abandoned temporary uploads are removed.
- The review server binds only to loopback, generates an unpredictable per-process mutation token,
  caps request bodies, confines artifact reads, and sends a restrictive CSP plus no-store and
  anti-framing headers.
- The supplied container runs as an unprivileged user, uses a read-only root filesystem in Compose,
  and disables networking for the MCP vision service.

## Trust boundaries and threats

| Boundary | Principal threats | Controls | Residual risk |
| --- | --- | --- | --- |
| Reference or `.blend` import | Embedded scripts, path escape, missing linked assets, pixel leakage | Auto-exec disabled, project copy, safe audit, confined worker paths, sanitized logs | Blender parser vulnerabilities remain inside the local Blender process; isolate untrusted files further when risk warrants it. |
| MCP host to coordinator | Arbitrary commands or filesystem access | Typed tools, no shell tool, explicit project roots, safe filenames, operation allowlists | Stdio inherits the launching user's permissions; the host must be trusted. |
| Browser review UI | Cross-site mutation, directory traversal, oversized requests | Loopback only, mutation token, CSP, no CORS, 1 MiB cap, digest-only artifact route | A hostile process already running as the same local user can observe local traffic. |
| Remote worker | Token theft, false completion, poisoned artifacts, lease replay | Enrollment gate, hashed tokens, expiring lease token, capability filters, digest/size verification | Workers are execution principals and can return semantically bad but hash-valid evidence; named review and acceptance gates remain required. |
| Browser sensor | SSRF, ambient login/profile leakage, secret capture, service-worker persistence, cross-capture state | Exact origin allowlist, private-network deny by default, fresh empty context, no personal profile, service workers blocked, cache disabled, recursive metadata/console redaction, content-addressed artifacts | Page pixels and HTML can contain user-visible sensitive content; capture only targets whose evidence and rights were intentionally placed in scope. Cross-origin frames and closed shadow roots remain opaque. |
| Model/checkpoint import | Silent network, license violation, malicious pickle | No downloader, HTTPS source record, exact digest, named license review, data-only governance record | Loading a checkpoint in an external backend can execute backend-specific deserialization; use safe formats and isolated workers. |
| Receipt/export | Overclaimed fidelity, evidence replacement | Immutable hashes, complete evidence snapshot, separate integrity and fidelity results, named approvals, registered blend/GLB hashes | SHA-256 proves identity, not truth; deliberate reviewer fraud is outside the software trust boundary. |

## Network and deployment position

The default stdio server does not listen on a socket. The built-in HTTP transports do not provide
internet-grade TLS or user authorization and must stay on loopback or behind a separately operated
TLS/mTLS authorization gateway. The review UI must not be exposed remotely. `BVMCP_UNSAFE=1` and
non-loopback deployment are explicit operator risk decisions and are outside the supported public
default.

## Verification evidence

Release verification covers path traversal, manifest vocabulary parity, artifact corruption and
upload mismatch, review authentication and CSP, worker enrollment/lease recovery, receipt tamper
detection, model approval/import, cross-platform doctor discovery, wheel content, clean install,
real headless Blender operations, and the governed synthetic L3 calibration benchmark. See
`docs/RELEASE.md` for the exact commands.

## Known limitations

- This is an application security review, not a third-party penetration test or legal opinion.
- Blender, COLMAP, FFmpeg, GPU drivers, and optional model runtimes have their own supply-chain and
  parser attack surfaces and must be patched by the operator.
- The Mac Studio benchmark cannot earn L3 until raw rear-view references with rights provenance,
  recovered metric cameras, residual comparisons, and a real named human review are supplied.
- Research-only or license-review-required checkpoints remain non-authoritative for release even
  when their geometry looks better.
