-- World Model V2.2 foundation schema (directive X/XI).
--
-- SEPARATION LAW: world-model state is NOT authority. This schema and its roles
-- exist so the world model can observe, estimate, predict and replay WITHOUT
-- ever being able to accept, price, clear, route, place, verify or settle.
--
-- This bootstrap is an OWNER/admin operation, run separately from the control
-- plane's runtime Migrate() (which applies control/schema.sql as the authority
-- app user). It is idempotent: safe to apply repeatedly.
--
--   wm_owner   — owns the wm schema and its objects; runs wm migrations. Never
--                used by a runtime service.
--   wm_writer  — SELECT/INSERT/UPDATE/DELETE on wm objects ONLY. No authority
--                (public) access of any kind. No DDL. The runtime observation
--                ingester connects as this.
--   wm_reader  — SELECT on wm objects ONLY. Analysis/replay connects as this
--                (read-only authority access, if needed, is a separate grant).
--
-- The hard invariant, proven at the database by control/wm_boundary_test.go:
-- wm_writer and wm_reader CANNOT write or DDL any authority (public) table.

CREATE SCHEMA IF NOT EXISTS wm;

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'wm_owner')  THEN CREATE ROLE wm_owner  NOLOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'wm_writer') THEN CREATE ROLE wm_writer NOLOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'wm_reader') THEN CREATE ROLE wm_reader NOLOGIN; END IF;
END $$;

ALTER SCHEMA wm OWNER TO wm_owner;

-- Execution root: one row per material execution the world model observes. It
-- carries stable execution-science identity (directive: workload x runtime rev x
-- model rev x hardware x topology x input shape x concurrency x state), NOT a
-- foreign key into authority. authority_ref is a soft pointer (text), never a
-- coupling that could make acceptance depend on wm availability.
CREATE TABLE IF NOT EXISTS wm.execution (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    authority_ref      text        NOT NULL,        -- soft pointer to the authority object (job/task/contract/lease id)
    workload_kind      text        NOT NULL,        -- batch_infer | embed | media_transcode | media_rendering | service_lease | ...
    runtime_revision   text        NOT NULL DEFAULT '',
    model_revision     text        NOT NULL DEFAULT '',
    hardware_identity  text        NOT NULL DEFAULT '',
    input_shape        text        NOT NULL DEFAULT '',
    concurrency        integer,
    observed_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wm_execution_authority_ref_nonempty CHECK (btrim(authority_ref) <> ''),
    CONSTRAINT wm_execution_workload_kind_nonempty CHECK (btrim(workload_kind) <> '')
);
ALTER TABLE wm.execution OWNER TO wm_owner;
CREATE INDEX IF NOT EXISTS wm_execution_authority_ref_idx ON wm.execution (authority_ref);

-- Append-only observation of one phase/attempt/transfer/verification fact about
-- an execution. Provenance (source) is immutable once written; trust_state may
-- escalate a WORKER_CLAIM to CORROBORATED but NEVER to a CONTROL_PLANE_MEASURED
-- source. measurement_state is tri-state: UNKNOWN is never silently 0.
CREATE TABLE IF NOT EXISTS wm.phase_observation (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    execution_id       uuid        NOT NULL REFERENCES wm.execution(id),
    phase              text        NOT NULL,        -- queue | startup | prefill | decode | transfer | verify | total | ...
    source             text        NOT NULL,        -- provenance class (immutable)
    trust_state        text        NOT NULL DEFAULT 'UNCORROBORATED',
    measurement_state  text        NOT NULL,        -- MEASURED | UNKNOWN | NOT_APPLICABLE
    value_num          double precision,            -- NULL unless measurement_state = MEASURED
    unit               text        NOT NULL DEFAULT '',
    observed_at        timestamptz NOT NULL DEFAULT now(),
    valid_from         timestamptz NOT NULL DEFAULT now(),
    fresh_until        timestamptz,
    authority_ref      text        NOT NULL DEFAULT '',
    confidence         double precision,
    support_n          integer,
    invalidation_rule  text        NOT NULL DEFAULT '',
    evidence_digest    text        NOT NULL DEFAULT '',
    CONSTRAINT wm_phase_source_valid CHECK (source IN (
        'CONTROL_PLANE_MEASURED','EXTERNALLY_ATTESTED','WORKER_CLAIM_CORROBORATED',
        'WORKER_CLAIM_UNCORROBORATED','DERIVED','SYNTHETIC','COUNTERFACTUAL')),
    CONSTRAINT wm_phase_trust_valid CHECK (trust_state IN ('UNCORROBORATED','CORROBORATED','DISPUTED')),
    CONSTRAINT wm_phase_measurement_valid CHECK (measurement_state IN ('MEASURED','UNKNOWN','NOT_APPLICABLE')),
    -- UNKNOWN is never 0: a non-null numeric value is allowed only when MEASURED.
    CONSTRAINT wm_phase_value_only_when_measured CHECK (
        (measurement_state = 'MEASURED') OR (value_num IS NULL)),
    CONSTRAINT wm_phase_confidence_range CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),
    CONSTRAINT wm_phase_support_n_nonneg CHECK (support_n IS NULL OR support_n >= 0)
);
ALTER TABLE wm.phase_observation OWNER TO wm_owner;
CREATE INDEX IF NOT EXISTS wm_phase_execution_idx ON wm.phase_observation (execution_id);

-- Provenance immutability: a phase_observation is append-only, and its source
-- can never be rewritten (a WORKER_CLAIM must not launder itself into a
-- CONTROL_PLANE_MEASURED). trust_state and freshness may still evolve.
CREATE OR REPLACE FUNCTION wm.phase_observation_source_is_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source <> OLD.source THEN
        RAISE EXCEPTION 'wm.phase_observation.source is immutable provenance (% -> %)', OLD.source, NEW.source;
    END IF;
    RETURN NEW;
END $$;
ALTER FUNCTION wm.phase_observation_source_is_immutable() OWNER TO wm_owner;

DROP TRIGGER IF EXISTS wm_phase_observation_source_immutable ON wm.phase_observation;
CREATE TRIGGER wm_phase_observation_source_immutable
    BEFORE UPDATE ON wm.phase_observation
    FOR EACH ROW EXECUTE FUNCTION wm.phase_observation_source_is_immutable();

-- Grants. writer = DML in wm only; reader = SELECT in wm only. Neither gets any
-- privilege on public (authority). The hard boundary is the ABSENCE of any
-- public grant plus the explicit revokes below.
GRANT USAGE ON SCHEMA wm TO wm_writer, wm_reader;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA wm TO wm_writer;
GRANT SELECT ON ALL TABLES IN SCHEMA wm TO wm_reader;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA wm TO wm_writer;
ALTER DEFAULT PRIVILEGES FOR ROLE wm_owner IN SCHEMA wm
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO wm_writer;
ALTER DEFAULT PRIVILEGES FOR ROLE wm_owner IN SCHEMA wm
    GRANT SELECT ON TABLES TO wm_reader;

-- Belt-and-suspenders: wm runtime roles hold nothing on the authority schema.
-- (PG15+ already removes PUBLIC CREATE on public; this makes it explicit and
-- survives a cluster where PUBLIC was granted something.)
REVOKE ALL ON SCHEMA public FROM wm_writer, wm_reader;
