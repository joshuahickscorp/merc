# Review API

The review API exposes project resources, approval mutations, and job actions over loopback only.

- `GET /api/snapshot` returns governed project, evidence, comparison, queue, job, and receipt state.
- `GET /api/queue` returns pending named decisions.
- `GET /artifact/{sha256}` serves registered immutable artifacts only.
- `POST /api/action/{action}` performs allow-listed actions and requires `X-BVMCP-Review-Token`.

The server refuses non-loopback binds, confines static paths, caps request bodies at 1 MiB, applies
a restrictive content security policy, and emits no access log containing private project paths.
