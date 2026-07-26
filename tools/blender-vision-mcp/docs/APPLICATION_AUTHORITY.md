# Governed application authority

VisionMCP does not treat pixels as authority for invisible application behavior. A complete
application candidate is compiled from a versioned `ApplicationReferencePacket` containing visual
and textual evidence plus the structured contracts needed for backend behavior.

## Required graphs

- `ProductSpecIR`: product goals, non-goals, actors, routes, states, and feature flags
- `UserJourneyGraph`: success, denial, error, and recovery journeys
- `DataModelGraph`: entities, typed fields, relations, indexes, retention, migrations, and rollback
- `APIContractGraph`: operations, schemas, errors, authorization, idempotency, upload boundaries,
  rate limits, and timeouts
- `AuthPolicyGraph`: identity provider, roles, permissions, tenant boundary, denial default, and
  credential handling
- `BusinessRuleGraph`: preconditions, deterministic effects, invariants, retries, duplicates,
  failures, analytics, and edge cases
- `DeploymentGraph`: declared target, nodes, runtime, secrets, health checks, migration, rollback,
  fresh-clone, and release commands
- `ObservabilityGraph`: structured logs, metrics, traces, SLOs, dashboards, alerts, and redaction
- `AcceptanceTestGraph`: executable unit, integration, API, browser, accessibility, visual,
  performance, security, migration, and rollback acceptance

The packet also declares error cases, numeric performance budgets, accessibility requirements,
visual and motion references, design-system exports, sample payloads, and supplied source code.
Every source carries a content digest, authority class, rights state, and locator.

## Authority classes

- `OBSERVED`: directly captured from a governed runtime or supplied artifact
- `SPECIFIED`: explicitly supplied product or technical authority
- `DERIVED`: computed from authority but still requires review before production promotion
- `HYPOTHESIS`: a bounded draft assumption that can never be silently promoted

The `ReferenceCompletenessAnalyzer` checks cross-graph references, API/entity bindings,
journey/route/operation bindings, permissions, default roles, business-rule tests, error-case tests,
operational budgets, and reservation/purchase idempotency.

Its `ReferenceCompletenessReport` separates:

- whether a packet can be compiled as a draft;
- whether the resulting application can be promoted;
- missing authority;
- hypotheses;
- contradictions;
- exact resumption contracts;
- authority coverage.

A P0 contradiction or missing required graph prevents even draft compilation. A hypothesis can
produce an explicitly draft candidate, but prevents promotion. A production candidate requires no
P0/P1 missing authority, contradiction, or hypothesis.

## Schema reproduction

The JSON Schemas are generated deterministically from the strict Pydantic models:

```bash
uv run python scripts/export-app-spec-schemas.py
uv run pytest tests/test_application_specification.py
```

The application compiler consumes the packet digest and completeness report. It refuses to build
an unlabelled production candidate when business rules, auth policy, deployment target, or another
required authority is absent.

## Bounded compiler

`BoundedApplicationCompiler` v1 supports declared REST + SQLite applications on local process or
container targets. It generates:

- strict TypeScript backend and frontend source;
- relational schema, numbered up migration, and rollback;
- list/get/create/update/delete handlers;
- idempotent create with request hashes, replay, conflict, scope, and retention;
- file upload type, size, permission, and confined-storage boundaries;
- status/polling lookup with explicit missing-record recovery;
- deny-by-default role/permission checks using the declared local test identity provider;
- parameterized SQL and validated identifiers;
- OpenAPI;
- deterministic frontend shell with semantic routes, keyboard focus, and reduced motion;
- generated API/authorization/CRUD/idempotency/upload/status tests;
- structured request logs and health checks;
- pinned package lock;
- Dockerfile and Compose target;
- packet, completeness, source-file, and candidate receipts.

The compiler currently refuses unsupported database, protocol, tenant, remote-deployment, and auth
provider classes. It does not silently replace them with easier implementations.

```bash
bvmcp app check packet.json
bvmcp app compile packet.json \
  --workspace /tmp/application-candidates \
  --candidate-id candidate-v1 \
  --mode promotion_candidate
bvmcp app verify /tmp/application-candidates/candidate-v1
```

Generated candidates reproduce with:

```bash
npm ci
npm run verify
npm run db:migrate
npm run db:migrate
npm run db:rollback
docker compose build
```

Failed generation attempts are retained under the compiler workspace `failed/` directory with the
exact error rather than deleted.
