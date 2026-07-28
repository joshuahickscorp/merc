# Security policy

Blender Vision treats references and Blender files as untrusted input.

- Safe mode is the default and cannot execute scripts embedded in `.blend` files.
- Headless Blender jobs accept schema-validated, hashed manifests and allowlisted operations.
- Worker reads and writes are confined to an explicit project root.
- Worker networking and arbitrary subprocess execution are not exposed by the operation API.
- Remote worker enrollment is disabled unless `BVMCP_WORKER_ENROLLMENT_TOKEN` is set. Enrollment,
  worker, and lease tokens are secrets and must not be logged or committed.
- Artifact transfer is authenticated, sequential, size-bounded, path-confined, and SHA-256
  verified before registration. A mismatched upload is rejected and its temporary file removed.
- A worker may abort only its own active transfer. Stale-transfer cleanup removes only unregistered
  partial files and records the terminal transfer state.
- Models and checkpoints are never downloaded silently.
- Material and visual-oracle records are appearance-only and cannot promote RGB/reflection evidence
  into geometry or dimensional authority.
- The local review server binds to loopback, serves only registered artifacts and bundled static
  assets, sets a restrictive content-security policy, caps request bodies, and requires a random
  in-page token for every mutation.
- Reference pixels are excluded from structured logs.

`BVMCP_UNSAFE=1` is reserved for local development. Do not use it for third-party assets.
The built-in HTTP transport does not terminate TLS or establish internet-safe authorization. Keep
it on loopback, or place it behind a separately managed TLS/mTLS and authorization gateway. Rotate
the enrollment token after provisioning and re-enroll a worker if its one-time token is exposed.
Report security issues privately to the repository maintainers rather than opening a public issue.
