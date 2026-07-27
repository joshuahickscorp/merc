# Postgres trust boundary (production Compose)

## Decision

The shipped `docker-compose.prod.yml` keeps control→Postgres on
`sslmode=disable`. That is **not** a general claim that Postgres never needs
TLS. It is a deliberate choice under a narrow trust boundary that CI enforces.

## Why TLS is not required on this path

1. **Postgres is not published to the host network.** The `postgres` service in
   `docker-compose.prod.yml` has no `ports:` mapping. Only other Compose
   services on the project network can open a TCP connection to it.
2. **Control is the only application client.** The control service reaches
   Postgres via the Compose DNS name `postgres` on the private project bridge
   network. There is no remote DBA listener, no cloud proxy, and no host-port
   tunnel in the shipped file.
3. **The network segment is the confidentiality boundary.** On a single-host
   Docker/Compose deployment, traffic between containers on the internal bridge
   does not leave the host kernel's virtual Ethernet. An attacker who can sniff
   that path already has host-equivalent position and can read volume contents
   (`pgdata`) directly.
4. **Authentication remains required.** Connections still use a strong
   `POSTGRES_PASSWORD`. Disabling TLS does not disable password auth.

## When this document does **not** apply

Do **not** rely on this exception if any of the following become true:

- Postgres gains a host `ports:` publish, a public load balancer, or a
  cross-host overlay without an encrypting mesh.
- Control and Postgres run on different hosts or clusters without a private
  network + encrypting fabric.
- Compliance or threat model requires encrypting all data-in-transit regardless
  of network locality.

In those cases configure Postgres to serve TLS (server cert + key, and ideally
`hostssl` in `pg_hba.conf`) and set
`DATABASE_URL=...sslmode=verify-full&sslrootcert=...` (or `require` with a
reviewed risk acceptance). A DSN edit alone is insufficient: the server must actually present a certificate.

## CI contract

`scripts/validate-postgres-trust-boundary.py` fails the build unless:

1. This document still states the four conditions above.
2. `docker-compose.prod.yml` has no published `ports` on the `postgres` service.
3. The production control `DATABASE_URL` uses `sslmode=disable` only while those
   conditions hold (the script pins that the URL still matches the documented
   exception rather than silently drifting to a remote host).

If you enable real Postgres TLS, update the compose DSN **and** this document
**and** the validator in the same change.
