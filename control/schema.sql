
SELECT pg_advisory_xact_lock(
    hashtextextended('computeexchange-control-schema-v1', 0)
);

CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid()


CREATE TABLE IF NOT EXISTS suppliers (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at     TIMESTAMPTZ DEFAULT now(),
    email          TEXT NOT NULL UNIQUE,
    owner_buyer_id UUID,
    stripe_acct    TEXT,               -- Stripe Connect account ID; Stripe hosts KYC/tax collection
    reputation     REAL DEFAULT 0.5,   -- 0.0-1.0
    tier           SMALLINT DEFAULT 0, -- 0-3
    status         TEXT DEFAULT 'pending',  -- pending|active|suspended|banned
    completed_tasks BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS workers (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    supplier_id    UUID REFERENCES suppliers,
    hw_class       TEXT NOT NULL,   -- 'apple_silicon_max', 'apple_silicon_pro', etc.
    memory_gb      REAL,
    bw_gbps        REAL,            -- measured memory bandwidth
    created_at     TIMESTAMPTZ DEFAULT now(),
    last_seen_at   TIMESTAMPTZ,
    version        TEXT,            -- agent binary version
    priority_claim_streak INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS benchmark_results (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    worker_id      UUID REFERENCES workers,
    measured_at    TIMESTAMPTZ DEFAULT now(),
    model_id       TEXT,            -- e.g. 'llama3.1-70b-q4_k_m'
    job_type       TEXT,            -- 'embed', 'infer', 'transcribe'
    tps            REAL,            -- tokens/sec
    eps            REAL,            -- embeddings/sec
    thermal_ok     BOOLEAN,
    p99_latency_ms REAL,
    load_ms        BIGINT DEFAULT 0 -- cold-load wall-clock ms the agent measured (0 = pre-load_ms agent build)
);

CREATE TABLE IF NOT EXISTS jobs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id            UUID NOT NULL,
    created_at          TIMESTAMPTZ DEFAULT now(),
    status              TEXT DEFAULT 'queued',  -- queued|running|verifying|complete|failed|cancelled
    job_type            TEXT NOT NULL,
    model_ref           TEXT,
    input_ref           TEXT NOT NULL,          -- object storage key
    output_ref          TEXT,                   -- object storage key
    tier                TEXT DEFAULT 'batch',   -- batch|priority|trusted
    verification_policy JSONB,
    estimated_usd       NUMERIC(10,6),
    actual_usd          NUMERIC(10,6),
    task_count          INT,
    tasks_done          INT DEFAULT 0,
    max_duration_secs   BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS tasks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id        UUID REFERENCES jobs,
    worker_id     UUID REFERENCES workers,
    created_at    TIMESTAMPTZ DEFAULT now(),
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    status        TEXT DEFAULT 'queued',  -- queued|running|verifying|complete|failed|retrying
    result_ref    TEXT,
    result_key    TEXT,
    input_ref     TEXT,                   -- per-task input chunk object key (null = inherit job.input_ref)
    expected_output_records BIGINT,
    is_honeypot   BOOLEAN DEFAULT false,
    is_redundancy BOOLEAN DEFAULT false,
    retry_count   SMALLINT DEFAULT 0,
    claimed_by      UUID,                     -- worker currently holding the task
    claimed_at      TIMESTAMPTZ,              -- when the claim was taken
    visible_at      TIMESTAMPTZ DEFAULT now(),-- task is claimable only once now() >= visible_at
    excluded_worker UUID,                     -- worker to skip on the next claim (the one that just failed it)
    excluded_until  TIMESTAMPTZ               -- exclusion is only in force while now() < excluded_until (NULL = no exclusion)
);
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS input_ref TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS result_key TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS expected_output_records BIGINT;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_expected_output_records_positive;
ALTER TABLE tasks ADD CONSTRAINT tasks_expected_output_records_positive
    CHECK (expected_output_records IS NULL OR expected_output_records > 0);
CREATE OR REPLACE FUNCTION cx_reject_expected_output_records_update() RETURNS trigger AS $$
BEGIN
    IF OLD.expected_output_records IS DISTINCT FROM NEW.expected_output_records THEN
        RAISE EXCEPTION 'task expected output records for % are immutable', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS tasks_expected_output_records_immutable ON tasks;
CREATE TRIGGER tasks_expected_output_records_immutable
    BEFORE UPDATE OF expected_output_records ON tasks
    FOR EACH ROW EXECUTE FUNCTION cx_reject_expected_output_records_update();
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS excluded_worker UUID;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS excluded_until  TIMESTAMPTZ;

ALTER TABLE tasks SET (
    autovacuum_vacuum_scale_factor  = 0.02,
    autovacuum_vacuum_threshold     = 50,
    autovacuum_analyze_scale_factor = 0.02,
    autovacuum_analyze_threshold    = 50,
    autovacuum_vacuum_cost_limit    = 1000
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    TIMESTAMPTZ DEFAULT now(),
    kind          TEXT NOT NULL,  -- 'buyer_charge'|'supplier_credit'|'platform_take'|'clawback'|'stripe_fee'
    supplier_id   UUID REFERENCES suppliers,
    buyer_id      UUID,
    task_id       UUID REFERENCES tasks,
    amount_usd    NUMERIC(10,6) NOT NULL,  -- positive = credit, negative = debit
    payout_status TEXT DEFAULT 'pending',  -- pending|held|awaiting_funding|ready|sending|outcome_unknown|carried|released|exported|clawed_back|reversal_required
    release_at    TIMESTAMPTZ,             -- when payout hold expires
    payout_ref    TEXT                     -- Stripe/Trolley transfer ID
);

ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_released_requires_ref;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_released_requires_ref
    CHECK (kind <> 'supplier_credit' OR payout_status <> 'released' OR payout_ref IS NOT NULL);
CREATE INDEX IF NOT EXISTS ledger_due_payout_idx
    ON ledger_entries (release_at,id)
    WHERE kind='supplier_credit' AND payout_status='held' AND release_at IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS ledger_task_kind_uniq ON ledger_entries (task_id, kind);


CREATE TABLE IF NOT EXISTS buyers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           TEXT UNIQUE NOT NULL,
    password_hash   TEXT,                      -- bcrypt; NULL = no password set (seed / API-key-only)
    free_credit_usd NUMERIC(12,6) NOT NULL DEFAULT 0,  -- sandbox grant; 402 gate exempts spend up to this
    created_at      TIMESTAMPTZ DEFAULT now()
);

ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS owner_buyer_id UUID;
WITH ownership_matches AS (
    SELECT s.id AS supplier_id,
           b.id AS buyer_id,
           count(*) OVER (PARTITION BY s.id) AS buyer_matches,
           count(*) OVER (PARTITION BY b.id) AS supplier_matches
      FROM suppliers s
      JOIN buyers b ON lower(b.email) = lower(s.email)
)
UPDATE suppliers s
   SET owner_buyer_id = m.buyer_id
  FROM ownership_matches m
 WHERE s.id = m.supplier_id
   AND s.owner_buyer_id IS NULL
   AND m.buyer_matches = 1
   AND m.supplier_matches = 1
   AND NOT EXISTS (
       SELECT 1 FROM suppliers owned WHERE owned.owner_buyer_id = m.buyer_id
   );

DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'suppliers_owner_buyer_fk'
           AND conrelid = 'suppliers'::regclass
    ) THEN
        ALTER TABLE suppliers
            ADD CONSTRAINT suppliers_owner_buyer_fk
            FOREIGN KEY (owner_buyer_id) REFERENCES buyers(id) ON DELETE RESTRICT;
    END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS suppliers_owner_buyer_uniq
    ON suppliers (owner_buyer_id) WHERE owner_buyer_id IS NOT NULL;

ALTER TABLE suppliers DROP COLUMN IF EXISTS tax_id;
ALTER TABLE suppliers DROP COLUMN IF EXISTS tax_country;

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,               -- SHA-256 of the raw cx_sess_ token
    buyer_id   UUID NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked    BOOLEAN DEFAULT false
);
CREATE INDEX IF NOT EXISTS sessions_buyer_idx ON sessions (buyer_id);


CREATE TABLE IF NOT EXISTS api_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id   UUID,
    key_hash   TEXT UNIQUE NOT NULL,      -- store a hash, never the raw key
    is_admin   BOOLEAN DEFAULT false,
    created_at TIMESTAMPTZ DEFAULT now(),
    revoked    BOOLEAN DEFAULT false
);
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS name   TEXT;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS masked TEXT;

CREATE TABLE IF NOT EXISTS admin_actions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind                    TEXT NOT NULL,
    task_id                 UUID,
    supplier_id             UUID,
    ledger_entry_id         UUID,
    reason                  TEXT,
    detail                  JSONB,
    actor_mode              TEXT,
    actor_principal_id      UUID,
    actor_session_id        UUID,
    actor_label             TEXT,
    attribution_scope       TEXT,
    intent_version          INTEGER,
    request_sha256          TEXT,
    correlation_ref         TEXT,
    target_kind             TEXT,
    target_id               UUID,
    fund_id                 UUID,
    fund_ref                TEXT,
    authorization_ref       TEXT,
    amount_cents            BIGINT,
    currency                TEXT
);
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_mode TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_principal_id UUID;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_session_id UUID;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_label TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS attribution_scope TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS intent_version INTEGER;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS request_sha256 TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS correlation_ref TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS target_kind TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS target_id UUID;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS fund_id UUID;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS fund_ref TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS authorization_ref TEXT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS amount_cents BIGINT;
ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS currency TEXT;
ALTER TABLE admin_actions DROP CONSTRAINT IF EXISTS admin_actions_actor_shape;
ALTER TABLE admin_actions ADD CONSTRAINT admin_actions_actor_shape CHECK (
    (actor_mode IS NULL AND actor_principal_id IS NULL AND actor_session_id IS NULL
     AND attribution_scope IS NULL)
 OR (actor_mode = 'break_glass_api_key' AND actor_principal_id IS NOT NULL
     AND actor_session_id IS NULL AND attribution_scope = 'shared_credential_only')
) NOT VALID;
ALTER TABLE admin_actions DROP CONSTRAINT IF EXISTS admin_actions_money_shape;
ALTER TABLE admin_actions ADD CONSTRAINT admin_actions_money_shape CHECK (
    kind NOT IN ('subsidy_fund_authorized','payout_subsidy_authorized')
 OR (actor_mode IS NOT NULL AND intent_version = 1
     AND request_sha256 ~ '^[0-9a-f]{64}$'
     AND correlation_ref IS NOT NULL AND btrim(correlation_ref) <> ''
     AND target_kind IS NOT NULL AND target_id IS NOT NULL
     AND fund_id IS NOT NULL AND fund_ref IS NOT NULL AND btrim(fund_ref) <> ''
     AND amount_cents > 0 AND currency = 'usd'
     AND reason IS NOT NULL AND btrim(reason) <> '')
) NOT VALID;
CREATE UNIQUE INDEX IF NOT EXISTS admin_actions_money_correlation_uniq
    ON admin_actions (kind, correlation_ref)
    WHERE kind IN ('subsidy_fund_authorized','payout_subsidy_authorized');
CREATE OR REPLACE FUNCTION reject_admin_action_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'admin actions are append-only';
END;
$$;
DROP TRIGGER IF EXISTS admin_actions_append_only ON admin_actions;
CREATE TRIGGER admin_actions_append_only
BEFORE UPDATE OR DELETE ON admin_actions
FOR EACH ROW EXECUTE FUNCTION reject_admin_action_mutation();

CREATE TABLE IF NOT EXISTS worker_tokens (
    token_hash  TEXT PRIMARY KEY,         -- SHA-256 hash of the raw token; raw is shown once at mint, never stored
    worker_id   UUID REFERENCES workers,
    supplier_id UUID REFERENCES suppliers,
    created_at  TIMESTAMPTZ DEFAULT now(),
    revoked     BOOLEAN DEFAULT false
);
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_name = 'worker_tokens' AND column_name = 'token') THEN
    ALTER TABLE worker_tokens RENAME COLUMN token TO token_hash;
  END IF;
END $$;

ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS credential_id UUID DEFAULT gen_random_uuid();
UPDATE worker_tokens SET credential_id = gen_random_uuid() WHERE credential_id IS NULL;
ALTER TABLE worker_tokens ALTER COLUMN credential_id SET NOT NULL;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS device_key_algorithm TEXT;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS device_public_key BYTEA;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS device_fingerprint TEXT;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS credential_version INT NOT NULL DEFAULT 1;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS rotated_from_credential_id UUID;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS revocation_reason TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS worker_tokens_credential_id_uniq ON worker_tokens (credential_id);
CREATE UNIQUE INDEX IF NOT EXISTS worker_tokens_active_device_fingerprint_uniq
    ON worker_tokens (device_fingerprint)
    WHERE device_fingerprint IS NOT NULL AND revoked = false;
ALTER TABLE worker_tokens DROP CONSTRAINT IF EXISTS worker_tokens_device_binding_valid;
ALTER TABLE worker_tokens ADD CONSTRAINT worker_tokens_device_binding_valid CHECK (
    num_nonnulls(device_key_algorithm, device_public_key, device_fingerprint) = 0
    OR (num_nonnulls(device_key_algorithm, device_public_key, device_fingerprint) = 3
        AND device_key_algorithm = 'p256' AND octet_length(device_public_key) = 65)
);
ALTER TABLE worker_tokens DROP CONSTRAINT IF EXISTS worker_tokens_credential_version_positive;
ALTER TABLE worker_tokens ADD CONSTRAINT worker_tokens_credential_version_positive
    CHECK (credential_version > 0);
ALTER TABLE worker_tokens DROP CONSTRAINT IF EXISTS worker_tokens_rotated_from_fk;

CREATE TABLE IF NOT EXISTS worker_enrollment_codes (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code_hash                  TEXT NOT NULL UNIQUE,
    buyer_id                   UUID NOT NULL REFERENCES buyers(id) ON DELETE CASCADE,
    supplier_id                UUID NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
    audience                   TEXT NOT NULL,
    device_key_algorithm       TEXT NOT NULL CHECK (device_key_algorithm = 'p256'),
    device_public_key          BYTEA NOT NULL CHECK (octet_length(device_public_key) = 65),
    device_fingerprint         TEXT NOT NULL,
    label                      TEXT,
    rotate_from_credential_id  UUID,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at                 TIMESTAMPTZ NOT NULL,
    consumed_at                TIMESTAMPTZ,
    consumed_credential_id     UUID REFERENCES worker_tokens(credential_id) ON DELETE SET NULL,
    revoked_at                 TIMESTAMPTZ,
    failed_attempts            INT NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    last_attempt_at            TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);
ALTER TABLE worker_enrollment_codes ADD COLUMN IF NOT EXISTS protocol_version INT NOT NULL DEFAULT 1;
ALTER TABLE worker_enrollment_codes ADD COLUMN IF NOT EXISTS control_origin TEXT;
ALTER TABLE worker_enrollment_codes ADD COLUMN IF NOT EXISTS request_id TEXT;
ALTER TABLE worker_enrollment_codes DROP CONSTRAINT IF EXISTS worker_enrollment_codes_protocol_binding_valid;
ALTER TABLE worker_enrollment_codes ADD CONSTRAINT worker_enrollment_codes_protocol_binding_valid CHECK (
    (protocol_version = 1 AND control_origin IS NULL AND request_id IS NULL)
    OR
    (protocol_version = 2 AND control_origin IS NOT NULL AND control_origin <> ''
        AND request_id IS NOT NULL AND char_length(request_id) = 22)
);
ALTER TABLE worker_enrollment_codes
    DROP CONSTRAINT IF EXISTS worker_enrollment_codes_rotate_from_credential_id_fkey;
CREATE INDEX IF NOT EXISTS worker_enrollment_codes_owner_idx
    ON worker_enrollment_codes (buyer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS worker_enrollment_codes_expiry_idx
    ON worker_enrollment_codes (expires_at)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS worker_credential_audit (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type         TEXT NOT NULL CHECK (event_type IN (
                         'code_issued','code_revoked','exchange_succeeded','exchange_rejected',
                         'credential_revoked','credential_rotated')),
    buyer_id           UUID,
    supplier_id        UUID,
    worker_id          UUID,
    enrollment_code_id UUID,
    credential_id      UUID,
    reason             TEXT,
    detail             JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS worker_credential_audit_owner_idx
    ON worker_credential_audit (buyer_id, created_at DESC);
CREATE OR REPLACE FUNCTION cx_reject_worker_credential_audit_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'worker credential audit is append-only';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS worker_credential_audit_append_only ON worker_credential_audit;
CREATE TRIGGER worker_credential_audit_append_only
    BEFORE UPDATE OR DELETE ON worker_credential_audit
    FOR EACH ROW EXECUTE FUNCTION cx_reject_worker_credential_audit_mutation();

CREATE TABLE IF NOT EXISTS honeypots (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_type     TEXT NOT NULL,
    input_ref    TEXT NOT NULL,
    known_answer BYTEA,
    created_at   TIMESTAMPTZ DEFAULT now()
);
ALTER TABLE honeypots ADD COLUMN IF NOT EXISTS answer_class TEXT NOT NULL DEFAULT '';
ALTER TABLE honeypots ADD COLUMN IF NOT EXISTS answer_model          TEXT;  -- byte-exact seed's required model_ref (NULL = tolerant probe, no model bound)
ALTER TABLE honeypots ADD COLUMN IF NOT EXISTS answer_min_max_tokens INT;   -- byte-exact seed's minimum valid job max_tokens (NULL = no floor)


CREATE TABLE IF NOT EXISTS webhooks (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id         UUID NOT NULL,
    job_id           UUID NOT NULL,
    url              TEXT NOT NULL,
    created_at       TIMESTAMPTZ DEFAULT now(),
    delivered_at     TIMESTAMPTZ,
    attempts         INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token      UUID,
    lease_expires_at TIMESTAMPTZ,
    dead_lettered_at TIMESTAMPTZ,
    last_attempt_at  TIMESTAMPTZ,
    last_error       TEXT,
    signing_secret_sealed TEXT,
    CHECK ((lease_token IS NULL) = (lease_expires_at IS NULL)),
    CONSTRAINT webhooks_signing_secret_sealed_check CHECK (
      signing_secret_sealed IS NULL OR
      (signing_secret_sealed LIKE 'enc:%' AND length(signing_secret_sealed)>4)
    )
);
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS delivered_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS lease_token UUID;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS dead_lettered_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS signing_secret_sealed TEXT;
UPDATE webhooks
   SET signing_secret_sealed=NULL
 WHERE signing_secret_sealed IS NOT NULL
   AND NOT (
     signing_secret_sealed LIKE 'enc:%' AND length(signing_secret_sealed)>4
   );
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid='webhooks'::regclass
           AND conname='webhooks_signing_secret_sealed_check'
    ) THEN
        ALTER TABLE webhooks ADD CONSTRAINT webhooks_signing_secret_sealed_check
            CHECK (
              signing_secret_sealed IS NULL OR
              (signing_secret_sealed LIKE 'enc:%' AND length(signing_secret_sealed)>4)
            ) NOT VALID;
    END IF;
END $$;
ALTER TABLE webhooks VALIDATE CONSTRAINT webhooks_signing_secret_sealed_check;
UPDATE webhooks
   SET dead_lettered_at=now(),
       lease_token=NULL,
       lease_expires_at=NULL,
       last_error='legacy webhook has no per-registration signing secret; re-register it'
 WHERE delivered_at IS NULL
   AND dead_lettered_at IS NULL
   AND signing_secret_sealed IS NULL;
DELETE FROM webhooks wh
 WHERE wh.job_id IS NULL OR wh.buyer_id IS NULL
    OR NOT EXISTS (
      SELECT 1 FROM jobs j WHERE j.id=wh.job_id AND j.buyer_id=wh.buyer_id
    );
ALTER TABLE webhooks ALTER COLUMN buyer_id SET NOT NULL;
ALTER TABLE webhooks ALTER COLUMN job_id SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS jobs_id_buyer_id_uniq ON jobs (id,buyer_id);
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid='webhooks'::regclass AND conname='webhooks_job_owner_fkey'
    ) THEN
        ALTER TABLE webhooks ADD CONSTRAINT webhooks_job_owner_fkey
            FOREIGN KEY (job_id,buyer_id) REFERENCES jobs(id,buyer_id)
            ON DELETE CASCADE NOT VALID;
    END IF;
END $$;
ALTER TABLE webhooks VALIDATE CONSTRAINT webhooks_job_owner_fkey;


CREATE TABLE IF NOT EXISTS models (
    id             TEXT PRIMARY KEY,
    family         TEXT,
    quant          TEXT,
    kind           TEXT,
    dim            INT,                 -- embedding dimensionality (embed models)
    job_type       TEXT,
    price_per_1k   NUMERIC(12,8),       -- USD / 1,000 units
    min_memory_gb  REAL,
    hf_repo        TEXT                 -- HuggingFace repo to resolve weights from
);

ALTER TABLE models DROP COLUMN IF EXISTS price_per_unit;


ALTER TABLE jobs ADD COLUMN IF NOT EXISTS min_memory_gb      REAL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS max_duration_secs   BIGINT NOT NULL DEFAULT 0; -- 0 = runner default; otherwise buyer wall-clock ceiling
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_max_duration_secs_range;
ALTER TABLE jobs ADD CONSTRAINT jobs_max_duration_secs_range
    CHECK (max_duration_secs BETWEEN 0 AND 4294967295);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS min_reputation     REAL DEFAULT 0;    -- Elite-supplier gate (research §6.4): claim only by reputation >= this
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS hw_classes         TEXT[];           -- NULL = any class
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS data_residency     TEXT[];           -- NULL = unrestricted
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS split_size         INT;              -- adaptive chunk size chosen at submit
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS offered_rate_usd_hr REAL;            -- price-derived $/hr a worker earns running this (min-payout gate)
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS eta_secs           INT;              -- predicted completion seconds at submit
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_status      TEXT NOT NULL DEFAULT 'not_attempted'; -- not_attempted|charged|failed|no_payment_method|deferred (queryable charge state, not log-only).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS results_merged_at  TIMESTAMPTZ;      -- watermark: set when the buyer-ready artifact was last successfully merged, so GET /v1/jobs/{id}/results only re-merges once since completion instead of on every poll
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS job_type_spec      JSONB;

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS max_usd            NUMERIC(12,6);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS budget_state       TEXT DEFAULT 'tracking';

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS chunk_index INT DEFAULT 0;          -- position within the job's input
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS hedged_from UUID;                   -- original task this is a hedge/tiebreak of
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_hw_class TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_engine TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_build_hash TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_worker_id UUID;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_supplier_id UUID;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_hw_class TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_engine TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_build_hash TEXT;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_execution_identity_complete;
ALTER TABLE tasks ADD CONSTRAINT tasks_execution_identity_complete CHECK (
    (execution_worker_id IS NULL AND execution_supplier_id IS NULL
     AND execution_hw_class IS NULL AND execution_engine IS NULL
     AND execution_build_hash IS NULL)
    OR
    (execution_worker_id IS NOT NULL AND execution_supplier_id IS NOT NULL
     AND COALESCE(btrim(execution_hw_class),'') <> ''
     AND COALESCE(btrim(execution_engine),'') <> ''
     AND execution_build_hash IS NOT NULL)
);
ALTER TABLE workers ADD COLUMN IF NOT EXISTS engine TEXT NOT NULL DEFAULT 'candle';
ALTER TABLE workers ADD COLUMN IF NOT EXISTS build_hash TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS task_execution_history (
    task_id     UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt     SMALLINT NOT NULL CHECK (attempt >= 0),
    worker_id   UUID NOT NULL REFERENCES workers(id) ON DELETE RESTRICT,
    supplier_id UUID NOT NULL REFERENCES suppliers(id) ON DELETE RESTRICT,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, attempt, worker_id)
);
CREATE INDEX IF NOT EXISTS task_execution_history_task_supplier_idx
    ON task_execution_history (task_id, supplier_id, worker_id);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS result_sha256 TEXT;

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_outcome TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS reported_duration_ms BIGINT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS reported_tokens_used BIGINT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS reported_hardware_temp_c REAL;

CREATE TABLE IF NOT EXISTS task_verdicts (
    task_id       UUID NOT NULL REFERENCES tasks ON DELETE CASCADE,
    attempt       SMALLINT NOT NULL,
    job_id        UUID NOT NULL REFERENCES jobs ON DELETE CASCADE,
    supplier_id   UUID REFERENCES suppliers,
    outcome       TEXT NOT NULL CHECK (outcome IN ('pass','pass_with_penalty','fail','loss_no_payout','clawed_back')),
    result_sha256 TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, attempt)
);
CREATE INDEX IF NOT EXISTS task_verdicts_job_idx ON task_verdicts (job_id, created_at);
ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS decision_version INTEGER;
ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS decision_sha256 TEXT;
ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS artifact_key TEXT;
ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS artifact_sha256 TEXT;
CREATE OR REPLACE FUNCTION reject_task_verdict_update() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'task verdict history is immutable';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS task_verdicts_no_update ON task_verdicts;
CREATE TRIGGER task_verdicts_no_update BEFORE UPDATE OR DELETE ON task_verdicts
FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update();
DROP TRIGGER IF EXISTS task_execution_history_append_only ON task_execution_history;
CREATE TRIGGER task_execution_history_append_only BEFORE UPDATE OR DELETE ON task_execution_history
FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update();

CREATE TABLE IF NOT EXISTS task_verdict_resolutions (
    effect_id      UUID PRIMARY KEY,
    task_id        UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    source_task_id UUID REFERENCES tasks(id) ON DELETE SET NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('promoted_pass','clawed_back')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS task_verdict_resolutions_task_idx
    ON task_verdict_resolutions (task_id,created_at,effect_id);
DROP TRIGGER IF EXISTS task_verdict_resolutions_append_only ON task_verdict_resolutions;
CREATE TRIGGER task_verdict_resolutions_append_only BEFORE UPDATE OR DELETE ON task_verdict_resolutions
FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update();

CREATE TABLE IF NOT EXISTS verification_work (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    attempt BIGINT NOT NULL CHECK (attempt >= 0),
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
    worker_id UUID NOT NULL REFERENCES workers(id) ON DELETE RESTRICT,
    supplier_id UUID NOT NULL REFERENCES suppliers(id) ON DELETE RESTRICT,
    snapshot_version SMALLINT NOT NULL CHECK (snapshot_version > 0),
    input_snapshot JSONB NOT NULL CHECK (jsonb_typeof(input_snapshot) = 'object'),
    snapshot_sha256 TEXT NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
    staged_result_key TEXT NOT NULL CHECK (btrim(staged_result_key) <> ''),
    reported_result_sha256 TEXT CHECK (reported_result_sha256 ~ '^[0-9a-f]{64}$'),
    duration_ms BIGINT NOT NULL CHECK (duration_ms >= 0),
    tokens_used BIGINT NOT NULL CHECK (tokens_used >= 0),
    hardware_temp_c REAL,
    sampling_policy TEXT,
    sampling_probability TEXT,
    sampling_selected BOOLEAN,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','leased','terminal')),
    artifact_key TEXT,
    artifact_sha256 TEXT,
    artifact_bytes BIGINT,
    lease_owner TEXT,
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    lease_attempts INT NOT NULL DEFAULT 0 CHECK (lease_attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT,
    terminal_outcome TEXT,
    decision_sha256 TEXT,
    terminal_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (task_id, attempt),
    CHECK ((artifact_key IS NULL AND artifact_sha256 IS NULL AND artifact_bytes IS NULL)
        OR (btrim(artifact_key) <> '' AND artifact_sha256 ~ '^[0-9a-f]{64}$' AND artifact_bytes >= 0)),
    CHECK ((sampling_policy IS NULL AND sampling_probability IS NULL AND sampling_selected IS NULL)
        OR (btrim(sampling_policy) <> '' AND sampling_probability IS NOT NULL AND sampling_selected IS NOT NULL)),
    CHECK ((status='leased' AND lease_owner IS NOT NULL AND btrim(lease_owner) <> ''
            AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status<>'leased' AND lease_owner IS NULL AND lease_token IS NULL AND lease_expires_at IS NULL)),
    CHECK ((status='terminal'
            AND terminal_outcome IN ('pass','pass_with_penalty','fail','loss_no_payout')
            AND decision_sha256 ~ '^[0-9a-f]{64}$' AND terminal_at IS NOT NULL
            AND artifact_key IS NOT NULL AND sampling_policy IS NOT NULL)
        OR (status<>'terminal' AND terminal_outcome IS NULL AND decision_sha256 IS NULL AND terminal_at IS NULL))
);
ALTER TABLE verification_work ADD COLUMN IF NOT EXISTS sampling_policy TEXT;
ALTER TABLE verification_work ADD COLUMN IF NOT EXISTS sampling_probability TEXT;
ALTER TABLE verification_work ADD COLUMN IF NOT EXISTS sampling_selected BOOLEAN;
ALTER TABLE verification_work DROP CONSTRAINT IF EXISTS verification_work_sampling_complete;
ALTER TABLE verification_work ADD CONSTRAINT verification_work_sampling_complete CHECK (
    (sampling_policy IS NULL AND sampling_probability IS NULL AND sampling_selected IS NULL)
    OR (btrim(sampling_policy) <> '' AND sampling_probability IS NOT NULL AND sampling_selected IS NOT NULL)
);
ALTER TABLE verification_work DROP CONSTRAINT IF EXISTS verification_work_terminal_requires_sampling;
ALTER TABLE verification_work ADD CONSTRAINT verification_work_terminal_requires_sampling
    CHECK (status<>'terminal' OR sampling_policy IS NOT NULL);
DROP TRIGGER IF EXISTS tasks_execution_identity_immutable ON tasks;
UPDATE tasks t
   SET execution_worker_id=vw.worker_id,
       execution_supplier_id=vw.supplier_id,
       execution_hw_class=vw.input_snapshot->>'hw_class',
       execution_engine=vw.input_snapshot->>'engine',
       execution_build_hash=vw.input_snapshot->>'build_hash'
  FROM verification_work vw
 WHERE vw.task_id=t.id AND vw.attempt=COALESCE(t.retry_count,0)
   AND t.execution_worker_id IS NULL
   AND COALESCE(btrim(vw.input_snapshot->>'hw_class'),'')<>''
   AND COALESCE(btrim(vw.input_snapshot->>'engine'),'')<>''
   AND vw.input_snapshot ? 'build_hash';
UPDATE tasks t
   SET execution_worker_id=w.id,execution_supplier_id=w.supplier_id,
       execution_hw_class=w.hw_class,execution_engine=w.engine,
       execution_build_hash=w.build_hash
  FROM workers w
 WHERE t.execution_worker_id IS NULL
   AND t.status IN ('running','verifying')
   AND t.worker_id=w.id AND t.claimed_by=w.id
   AND COALESCE(btrim(w.hw_class),'')<>''
   AND COALESCE(btrim(w.engine),'')<>'';
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='tasks_execution_worker_fk') THEN
        ALTER TABLE tasks ADD CONSTRAINT tasks_execution_worker_fk
            FOREIGN KEY (execution_worker_id) REFERENCES workers(id) ON DELETE RESTRICT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='tasks_execution_supplier_fk') THEN
        ALTER TABLE tasks ADD CONSTRAINT tasks_execution_supplier_fk
            FOREIGN KEY (execution_supplier_id) REFERENCES suppliers(id) ON DELETE RESTRICT;
    END IF;
END $$;
CREATE OR REPLACE FUNCTION cx_protect_task_execution_identity() RETURNS trigger AS $$
BEGIN
    IF (OLD.execution_worker_id,OLD.execution_supplier_id,OLD.execution_hw_class,
        OLD.execution_engine,OLD.execution_build_hash)
       IS DISTINCT FROM
       (NEW.execution_worker_id,NEW.execution_supplier_id,NEW.execution_hw_class,
        NEW.execution_engine,NEW.execution_build_hash) THEN
        IF NOT (
            OLD.status IN ('queued','retrying') AND NEW.status='running'
            AND NEW.execution_worker_id IS NOT NULL
            AND NEW.execution_supplier_id IS NOT NULL
            AND NEW.worker_id IS NOT DISTINCT FROM NEW.execution_worker_id
            AND NEW.claimed_by IS NOT DISTINCT FROM NEW.execution_worker_id
            AND COALESCE(btrim(NEW.execution_hw_class),'')<>''
            AND COALESCE(btrim(NEW.execution_engine),'')<>''
            AND NEW.execution_build_hash IS NOT NULL
            AND EXISTS (
                SELECT 1 FROM workers w
                 WHERE w.id=NEW.execution_worker_id
                   AND w.supplier_id=NEW.execution_supplier_id
                   AND w.hw_class=NEW.execution_hw_class
                   AND COALESCE(w.engine,'')=NEW.execution_engine
                   AND COALESCE(w.build_hash,'')=NEW.execution_build_hash
            )
        ) THEN
            RAISE EXCEPTION 'task execution identity for % is immutable outside claim transition', OLD.id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS tasks_execution_identity_immutable ON tasks;
CREATE TRIGGER tasks_execution_identity_immutable
    BEFORE UPDATE OF execution_worker_id,execution_supplier_id,execution_hw_class,
                     execution_engine,execution_build_hash ON tasks
    FOR EACH ROW EXECUTE FUNCTION cx_protect_task_execution_identity();
UPDATE tasks t
   SET verification_hw_class=COALESCE(NULLIF((
         SELECT vw.input_snapshot->>'hw_class' FROM verification_work vw
          WHERE vw.task_id=anchor.id ORDER BY vw.attempt DESC LIMIT 1
       ),''),NULLIF(COALESCE(anchor.execution_hw_class,''),'')),
       verification_engine=COALESCE((
         SELECT vw.input_snapshot->>'engine' FROM verification_work vw
          WHERE vw.task_id=anchor.id ORDER BY vw.attempt DESC LIMIT 1
       ),COALESCE(anchor.execution_engine,'')),
       verification_build_hash=COALESCE((
         SELECT vw.input_snapshot->>'build_hash' FROM verification_work vw
          WHERE vw.task_id=anchor.id ORDER BY vw.attempt DESC LIMIT 1
       ),COALESCE(anchor.execution_build_hash,''))
  FROM tasks anchor
 WHERE t.is_redundancy=true AND t.hedged_from=anchor.id
   AND NULLIF(COALESCE(t.verification_hw_class,''),'') IS NULL
   AND COALESCE(NULLIF(anchor.execution_hw_class,''),(
         SELECT NULLIF(vw.input_snapshot->>'hw_class','') FROM verification_work vw
          WHERE vw.task_id=anchor.id ORDER BY vw.attempt DESC LIMIT 1
       )) IS NOT NULL;
CREATE INDEX IF NOT EXISTS verification_work_pending_idx
    ON verification_work (status,next_attempt_at,created_at,id);
CREATE INDEX IF NOT EXISTS verification_work_expired_lease_idx
    ON verification_work (status,lease_expires_at,id) WHERE status='leased';
ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS verification_work_id UUID REFERENCES verification_work(id);
CREATE TABLE IF NOT EXISTS chunk_artifact_resolutions (
    effect_id            UUID PRIMARY KEY,
    job_id               UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    chunk_index          INT NOT NULL,
    winner_task_id       UUID NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    verification_work_id UUID NOT NULL REFERENCES verification_work(id) ON DELETE RESTRICT,
    artifact_key         TEXT NOT NULL CHECK (btrim(artifact_key) <> ''),
    artifact_sha256      TEXT NOT NULL CHECK (artifact_sha256 ~ '^[0-9a-f]{64}$'),
    artifact_bytes       BIGINT NOT NULL CHECK (artifact_bytes >= 0),
    basis                TEXT NOT NULL CHECK (basis IN ('provisional','majority')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE chunk_artifact_resolutions DROP CONSTRAINT IF EXISTS chunk_artifact_resolutions_one_basis;
ALTER TABLE chunk_artifact_resolutions ADD CONSTRAINT chunk_artifact_resolutions_one_basis
    UNIQUE (job_id,chunk_index,basis);
ALTER TABLE chunk_artifact_resolutions DROP CONSTRAINT IF EXISTS chunk_artifact_resolutions_chunk_nonnegative;
ALTER TABLE chunk_artifact_resolutions ADD CONSTRAINT chunk_artifact_resolutions_chunk_nonnegative
    CHECK (chunk_index >= 0);
CREATE INDEX IF NOT EXISTS chunk_artifact_resolutions_lookup_idx
    ON chunk_artifact_resolutions (job_id,chunk_index,basis,created_at,effect_id);
CREATE OR REPLACE FUNCTION validate_chunk_artifact_resolution_insert() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM tasks t JOIN verification_work vw
            ON vw.id=NEW.verification_work_id AND vw.task_id=t.id
         WHERE t.id=NEW.winner_task_id
           AND t.job_id=NEW.job_id AND COALESCE(t.chunk_index,0)=NEW.chunk_index
           AND t.status='complete'
           AND t.result_ref=NEW.artifact_key AND t.result_sha256=NEW.artifact_sha256
           AND vw.job_id=NEW.job_id AND vw.attempt=t.retry_count
           AND vw.status='terminal' AND vw.terminal_outcome IN ('pass','pass_with_penalty')
           AND vw.artifact_key=NEW.artifact_key
           AND vw.artifact_sha256=NEW.artifact_sha256
           AND vw.artifact_bytes=NEW.artifact_bytes
		   AND (NEW.basis<>'provisional' OR (
		       t.is_redundancy=false AND t.is_honeypot=false AND EXISTS (
		           SELECT 1 FROM ledger_entries le
		            WHERE le.task_id=t.id AND le.kind='buyer_charge'
		       )
		   ))
    ) THEN
        RAISE EXCEPTION 'chunk artifact resolution is not bound to terminal sealed winner work';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS chunk_artifact_resolutions_validate_insert ON chunk_artifact_resolutions;
CREATE TRIGGER chunk_artifact_resolutions_validate_insert BEFORE INSERT ON chunk_artifact_resolutions
FOR EACH ROW EXECUTE FUNCTION validate_chunk_artifact_resolution_insert();
DROP TRIGGER IF EXISTS chunk_artifact_resolutions_append_only ON chunk_artifact_resolutions;
CREATE TRIGGER chunk_artifact_resolutions_append_only BEFORE UPDATE OR DELETE ON chunk_artifact_resolutions
FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update();

CREATE TABLE IF NOT EXISTS verification_work_plans (
    work_id              UUID PRIMARY KEY REFERENCES verification_work(id) ON DELETE RESTRICT,
    plan_version         SMALLINT NOT NULL CHECK (plan_version > 0),
    snapshot_sha256      TEXT NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
    artifact_key         TEXT NOT NULL CHECK (btrim(artifact_key) <> ''),
    artifact_sha256      TEXT NOT NULL CHECK (artifact_sha256 ~ '^[0-9a-f]{64}$'),
    artifact_bytes       BIGINT NOT NULL CHECK (artifact_bytes >= 0),
    sampling_policy      TEXT NOT NULL CHECK (btrim(sampling_policy) <> ''),
    sampling_probability TEXT NOT NULL,
    sampling_selected    BOOLEAN NOT NULL,
    decision_json        JSONB NOT NULL CHECK (jsonb_typeof(decision_json)='object'),
    settlement_json      JSONB NOT NULL CHECK (jsonb_typeof(settlement_json)='array'),
    decision_sha256      TEXT NOT NULL CHECK (decision_sha256 ~ '^[0-9a-f]{64}$'),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE OR REPLACE FUNCTION reject_verification_work_plan_update() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'verification work plan is immutable'; END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS verification_work_plans_no_update ON verification_work_plans;
CREATE TRIGGER verification_work_plans_no_update BEFORE UPDATE OR DELETE ON verification_work_plans
FOR EACH ROW EXECUTE FUNCTION reject_verification_work_plan_update();

CREATE OR REPLACE FUNCTION protect_verification_work_identity() RETURNS trigger AS $$
BEGIN
    IF TG_OP='DELETE' THEN RAISE EXCEPTION 'verification work cannot be deleted'; END IF;
    IF (OLD.id,OLD.task_id,OLD.attempt,OLD.job_id,OLD.worker_id,OLD.supplier_id,
        OLD.snapshot_version,OLD.input_snapshot,OLD.snapshot_sha256,OLD.staged_result_key,
        OLD.reported_result_sha256,OLD.duration_ms,OLD.tokens_used,OLD.hardware_temp_c,OLD.created_at)
       IS DISTINCT FROM
       (NEW.id,NEW.task_id,NEW.attempt,NEW.job_id,NEW.worker_id,NEW.supplier_id,
        NEW.snapshot_version,NEW.input_snapshot,NEW.snapshot_sha256,NEW.staged_result_key,
        NEW.reported_result_sha256,NEW.duration_ms,NEW.tokens_used,NEW.hardware_temp_c,NEW.created_at)
       THEN RAISE EXCEPTION 'verification work attempt snapshot is immutable';
    END IF;
    IF OLD.artifact_key IS NOT NULL
       AND (OLD.artifact_key,OLD.artifact_sha256,OLD.artifact_bytes)
           IS DISTINCT FROM (NEW.artifact_key,NEW.artifact_sha256,NEW.artifact_bytes)
       THEN RAISE EXCEPTION 'verification artifact authority is immutable';
    END IF;
    IF OLD.sampling_policy IS NOT NULL
       AND (OLD.sampling_policy,OLD.sampling_probability,OLD.sampling_selected)
           IS DISTINCT FROM (NEW.sampling_policy,NEW.sampling_probability,NEW.sampling_selected)
       THEN RAISE EXCEPTION 'verification sampling decision is immutable';
    END IF;
    IF OLD.status='terminal' AND OLD IS DISTINCT FROM NEW
       THEN RAISE EXCEPTION 'terminal verification work is immutable';
    END IF;
    IF OLD.status='pending' AND NEW.status NOT IN ('pending','leased')
       THEN RAISE EXCEPTION 'verification work must be leased before terminal transition';
    END IF;
    IF OLD.status='leased' AND NEW.status='leased'
       AND OLD.lease_token IS DISTINCT FROM NEW.lease_token AND OLD.lease_expires_at>now()
       THEN RAISE EXCEPTION 'live verification lease cannot be stolen';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS verification_work_immutable ON verification_work;
CREATE TRIGGER verification_work_immutable BEFORE UPDATE OR DELETE ON verification_work
FOR EACH ROW EXECUTE FUNCTION protect_verification_work_identity();

CREATE OR REPLACE FUNCTION validate_verification_work_binding() RETURNS trigger AS $$
BEGIN
	IF EXISTS (SELECT 1 FROM verification_work existing
	           WHERE existing.task_id=NEW.task_id AND existing.attempt=NEW.attempt) THEN
	    RETURN NEW;
	END IF;
	IF NEW.status<>'pending' OR NEW.artifact_key IS NOT NULL
	   OR NEW.sampling_policy IS NOT NULL OR NEW.lease_owner IS NOT NULL
	   OR NEW.terminal_outcome IS NOT NULL OR NEW.decision_sha256 IS NOT NULL
	   OR NEW.terminal_at IS NOT NULL OR NEW.lease_attempts<>0 THEN
	    RAISE EXCEPTION 'verification work must begin as an unleased pending attempt';
	END IF;
    IF NOT EXISTS (
		SELECT 1 FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.id=NEW.task_id AND t.job_id=NEW.job_id AND t.status='verifying'
		   AND t.worker_id=NEW.worker_id AND t.claimed_by=NEW.worker_id
		   AND t.execution_worker_id=NEW.worker_id
		   AND t.execution_supplier_id=NEW.supplier_id
		   AND COALESCE(t.retry_count,0)::bigint=NEW.attempt
		   AND t.result_ref=NEW.staged_result_key
		   AND t.result_sha256 IS NOT DISTINCT FROM NEW.reported_result_sha256
		   AND t.reported_duration_ms=NEW.duration_ms AND t.reported_tokens_used=NEW.tokens_used
		   AND t.reported_hardware_temp_c IS NOT DISTINCT FROM NEW.hardware_temp_c
		   AND j.status IN ('queued','running','verifying')
		   AND NEW.snapshot_version=4
		   AND (NEW.input_snapshot->>'is_honeypot')::boolean=t.is_honeypot
		   AND (NEW.input_snapshot->>'is_redundancy')::boolean=t.is_redundancy
		   AND COALESCE(NEW.input_snapshot->>'hw_class','')=COALESCE(t.execution_hw_class,'')
		   AND COALESCE(NEW.input_snapshot->>'engine','')=COALESCE(t.execution_engine,'')
		   AND COALESCE(NEW.input_snapshot->>'build_hash','')=COALESCE(t.execution_build_hash,'')
		   AND COALESCE(NEW.input_snapshot->>'job_type','')=j.job_type
		   AND COALESCE(NEW.input_snapshot->>'input_ref','')=COALESCE(t.input_ref,'')
		   AND COALESCE(NEW.input_snapshot->>'model_ref','')=COALESCE(j.model_ref,'')
		   AND COALESCE((NEW.input_snapshot->>'min_memory_gb')::real,0)=COALESCE(j.min_memory_gb,0)
		   AND COALESCE((NEW.input_snapshot->>'chunk_index')::int,0)=COALESCE(t.chunk_index,0)
		   AND COALESCE((NEW.input_snapshot->>'split_size')::int,0)=COALESCE(j.split_size,0)
		   AND COALESCE((NEW.input_snapshot->>'result_max_bytes')::bigint,0) BETWEEN 1 AND 268435456
		   AND COALESCE((NEW.input_snapshot->>'expected_output_records')::bigint,0)=COALESCE(t.expected_output_records,0)
	) THEN RAISE EXCEPTION 'verification work does not match the claim-frozen task attempt snapshot';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS verification_work_binding ON verification_work;
CREATE TRIGGER verification_work_binding BEFORE INSERT ON verification_work
FOR EACH ROW EXECUTE FUNCTION validate_verification_work_binding();

ALTER TABLE workers ADD COLUMN IF NOT EXISTS supported_jobs     TEXT[];        -- job_type tags this worker can run
ALTER TABLE workers ADD COLUMN IF NOT EXISTS supported_models   TEXT[];        -- model ids resident/runnable locally
ALTER TABLE workers ADD COLUMN IF NOT EXISTS min_payout_usd_hr  REAL DEFAULT 0;-- operator reservation price ($/hr)
ALTER TABLE workers ADD COLUMN IF NOT EXISTS thermal_ok         BOOLEAN DEFAULT true;
ALTER TABLE workers ADD COLUMN IF NOT EXISTS priority_claim_streak INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workers DROP CONSTRAINT IF EXISTS workers_priority_claim_streak_range;
ALTER TABLE workers ADD CONSTRAINT workers_priority_claim_streak_range
    CHECK (priority_claim_streak BETWEEN 0 AND 3);

CREATE TABLE IF NOT EXISTS worker_authorized_capabilities (
    worker_id     UUID NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    cell_id       TEXT NOT NULL,
    runtime_id    TEXT NOT NULL,
    job_type      TEXT NOT NULL,
    model_ref     TEXT NOT NULL,
    model_kind    TEXT NOT NULL,
    matrix_sha256 TEXT NOT NULL,
    authorized_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (worker_id, cell_id, runtime_id, job_type, model_ref, matrix_sha256),
    CHECK (cell_id <> ''),
    CHECK (runtime_id <> ''),
    CHECK (job_type <> ''),
    CHECK (matrix_sha256 ~ '^[0-9a-f]{64}$')
);
ALTER TABLE worker_authorized_capabilities ADD COLUMN IF NOT EXISTS model_kind TEXT;
DELETE FROM worker_authorized_capabilities
 WHERE COALESCE(model_kind,'') NOT IN ('gguf','hf');
ALTER TABLE worker_authorized_capabilities ALTER COLUMN model_kind SET NOT NULL;
ALTER TABLE worker_authorized_capabilities
    DROP CONSTRAINT IF EXISTS worker_authorized_capabilities_model_kind_valid;
ALTER TABLE worker_authorized_capabilities
    ADD CONSTRAINT worker_authorized_capabilities_model_kind_valid
    CHECK (model_kind IN ('gguf','hf'));
CREATE INDEX IF NOT EXISTS worker_authorized_capabilities_exact_idx
    ON worker_authorized_capabilities (worker_id, job_type, model_ref, matrix_sha256);
CREATE INDEX IF NOT EXISTS worker_authorized_capabilities_supply_idx
    ON worker_authorized_capabilities (job_type, model_ref, matrix_sha256, worker_id);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_cell_id TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_id TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_matrix_sha256 TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS model_kind TEXT;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_runtime_provenance_complete;
ALTER TABLE tasks ADD CONSTRAINT tasks_runtime_provenance_complete CHECK (
    (runtime_cell_id IS NULL AND runtime_id IS NULL AND runtime_matrix_sha256 IS NULL AND model_kind IS NULL)
    OR
    (COALESCE(runtime_cell_id,'') <> '' AND COALESCE(runtime_id,'') <> ''
     AND runtime_matrix_sha256 ~ '^[0-9a-f]{64}$' AND COALESCE(model_kind,'') <> '')
);

ALTER TABLE workers ADD COLUMN IF NOT EXISTS engine             TEXT NOT NULL DEFAULT 'candle';
DELETE FROM worker_authorized_capabilities
 WHERE worker_id IN (SELECT id FROM workers WHERE engine <> 'candle');
ALTER TABLE workers DROP CONSTRAINT IF EXISTS workers_engine_valid;
ALTER TABLE workers ADD CONSTRAINT workers_engine_valid CHECK (engine = 'candle') NOT VALID;

ALTER TABLE workers ADD COLUMN IF NOT EXISTS build_hash         TEXT NOT NULL DEFAULT '';

ALTER TABLE workers ADD COLUMN IF NOT EXISTS effective_memory_gb REAL;          -- allocatable for jobs = available - headroom (NULL -> fall back to memory_gb)
ALTER TABLE workers ADD COLUMN IF NOT EXISTS available_memory_gb REAL;          -- live free + reclaimable memory (GB)
ALTER TABLE workers ADD COLUMN IF NOT EXISTS reserved_headroom_gb REAL;         -- GB the operator reserves for their own use
ALTER TABLE workers ADD COLUMN IF NOT EXISTS throttled          BOOLEAN DEFAULT false; -- agent currently pausing new claims (memory pressure)

ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS data_country   TEXT;            -- ISO country the supplier operates in
ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS quarantined_at TIMESTAMPTZ;     -- set by auto-quarantine (Verification V2)

ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS payouts_enabled BOOLEAN DEFAULT false;


CREATE TABLE IF NOT EXISTS quotes (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at         TIMESTAMPTZ DEFAULT now(),
    buyer_id           UUID,
    job_type           TEXT NOT NULL,
    model_ref          TEXT,
    tier               TEXT,
    records            BIGINT,            -- non-blank JSONL records scanned
    input_bytes        BIGINT,
    estimated_tokens   BIGINT,            -- byte/token heuristic (documented, not exact)
    malformed_records  INT,
    split_size         INT,              -- recommended lines/task
    task_count         INT,              -- estimated primary tasks
    eligible_now       INT,              -- workers passing the claim filter for this job, seen <60s
    cost_expected_usd  NUMERIC(12,6),
    cost_min_usd       NUMERIC(12,6),
    cost_max_usd       NUMERIC(12,6),
    eta_p50_secs       INT,
    eta_p90_secs       INT,
    oom_risk           TEXT,             -- low|medium|high (conservative, explainable)
    confidence         REAL,             -- 0.0-1.0
    quote_json         JSONB             -- the full quote object returned to the buyer
);


CREATE TABLE IF NOT EXISTS task_failures (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at    TIMESTAMPTZ DEFAULT now(),
    task_id       UUID REFERENCES tasks,
    job_id        UUID,
    worker_id     UUID,
    failure_class TEXT NOT NULL,    -- shared taxonomy: oom|bad_input|model_load_failed|timeout|...
    retryable     BOOLEAN NOT NULL,
    buyer_fault   BOOLEAN NOT NULL,
    message       TEXT,             -- short, buyer-safe summary (operator detail stays in logs)
    backend       TEXT,
    model_ref     TEXT,
    duration_ms   BIGINT,
    memory        JSONB             -- {total_gb, available_gb, effective_gb, reserved_headroom_gb} at failure
);



CREATE INDEX IF NOT EXISTS tasks_job_status_idx     ON tasks (job_id, status);
CREATE INDEX IF NOT EXISTS tasks_worker_status_idx  ON tasks (worker_id, status);
CREATE INDEX IF NOT EXISTS tasks_status_visible_idx ON tasks (status, visible_at);  -- queue claim path
CREATE INDEX IF NOT EXISTS tasks_ready_unclaimed_idx ON tasks (status, (COALESCE(visible_at, created_at)), created_at)
    WHERE claimed_by IS NULL AND status IN ('queued','retrying');  -- hot SKIP-LOCKED claim path
CREATE INDEX IF NOT EXISTS ledger_supplier_payout_idx ON ledger_entries (supplier_id, payout_status);
CREATE INDEX IF NOT EXISTS ledger_kind_idx             ON ledger_entries (kind);  -- reconcile/audit sums by kind
CREATE INDEX IF NOT EXISTS workers_hwclass_seen_idx  ON workers (hw_class, last_seen_at);
CREATE INDEX IF NOT EXISTS benchmark_worker_type_time_idx ON benchmark_results (worker_id, job_type, measured_at DESC);
CREATE INDEX IF NOT EXISTS workers_class_engine_seen_idx ON workers (hw_class, engine, build_hash, last_seen_at);  -- (hw_class, engine, build_hash) redundancy-peer class lookups
CREATE INDEX IF NOT EXISTS webhooks_job_idx          ON webhooks (job_id);
CREATE INDEX IF NOT EXISTS webhooks_delivery_due_idx ON webhooks (next_attempt_at,created_at,id)
    WHERE delivered_at IS NULL AND dead_lettered_at IS NULL;
DELETE FROM webhooks
 WHERE id IN (
   SELECT id FROM (
     SELECT id,row_number() OVER (
       PARTITION BY buyer_id,job_id,url
       ORDER BY (delivered_at IS NULL),(dead_lettered_at IS NOT NULL),created_at NULLS LAST,id
     ) AS duplicate_rank
     FROM webhooks
   ) ranked
   WHERE duplicate_rank>1
 );
CREATE UNIQUE INDEX IF NOT EXISTS webhooks_job_url_uniq ON webhooks (buyer_id,job_id,url);
CREATE INDEX IF NOT EXISTS models_job_type_idx       ON models (job_type);
CREATE INDEX IF NOT EXISTS tasks_job_chunk_idx        ON tasks (job_id, chunk_index);  -- ordered merge
CREATE INDEX IF NOT EXISTS workers_supplier_idx       ON workers (supplier_id);
CREATE INDEX IF NOT EXISTS quotes_buyer_created_idx   ON quotes (buyer_id, created_at DESC);  -- buyer quote history + admin drift

CREATE TABLE IF NOT EXISTS disputes (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      UUID NOT NULL REFERENCES jobs,
    buyer_id    UUID NOT NULL,
    reason      TEXT,
    status      TEXT NOT NULL DEFAULT 'open',  -- open|resolved|rejected
    created_at  TIMESTAMPTZ DEFAULT now(),
    resolved_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS disputes_job_idx ON disputes (job_id, created_at);

CREATE TABLE IF NOT EXISTS verification_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      UUID NOT NULL REFERENCES jobs,
    task_id     UUID,
    supplier_id UUID,
    kind        TEXT NOT NULL,  -- honeypot_pass|honeypot_fail|redundancy_match|redundancy_mismatch|tiebreak_win|tiebreak_loss|redundancy_cross_class|tiebreak_cross_class|redundancy_same_supplier
    created_at  TIMESTAMPTZ DEFAULT now()
);
ALTER TABLE verification_events ADD COLUMN IF NOT EXISTS effect_id UUID;
ALTER TABLE verification_events ADD COLUMN IF NOT EXISTS attempt SMALLINT;
CREATE INDEX IF NOT EXISTS verification_events_job_idx ON verification_events (job_id, created_at);
DROP INDEX IF EXISTS verification_events_effect_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS verification_events_effect_uniq
    ON verification_events (effect_id);
DELETE FROM verification_events newer
USING verification_events older
WHERE newer.effect_id IS NULL AND older.effect_id IS NULL
  AND newer.task_id IS NOT NULL
  AND newer.task_id = older.task_id AND newer.kind = older.kind
  AND (newer.created_at > older.created_at
       OR (newer.created_at = older.created_at AND newer.id::text > older.id::text));
DROP INDEX IF EXISTS verification_events_task_kind_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS verification_events_legacy_task_kind_uniq
    ON verification_events (task_id, kind)
    WHERE task_id IS NOT NULL AND effect_id IS NULL;
ALTER TABLE disputes ADD COLUMN IF NOT EXISTS reverify_task_id UUID;
CREATE INDEX IF NOT EXISTS task_failures_job_idx      ON task_failures (job_id, created_at DESC); -- failure drill-down by job

ALTER TABLE quotes ADD COLUMN IF NOT EXISTS expires_at    TIMESTAMPTZ;             -- quote stops being bindable after this (now()+15m at insert)
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS input_sha256  TEXT;                    -- sha256 of the scanned input bytes, for best-effort submit match
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS quote_id      UUID;                    -- the advisory quote this job was bound to (NULL = none)
CREATE INDEX IF NOT EXISTS jobs_quote_idx ON jobs (quote_id) WHERE quote_id IS NOT NULL;  -- quote-to-job lookups (invoice/admin drift)

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE oid=to_regclass('worker_memory_samples') AND relkind='p') THEN
        CREATE TABLE worker_memory_samples_compact (
            id UUID PRIMARY KEY DEFAULT gen_random_uuid(), worker_id UUID,
            available_gb REAL, effective_gb REAL, throttled BOOLEAN,
            created_at TIMESTAMPTZ NOT NULL DEFAULT now()
        );
        INSERT INTO worker_memory_samples_compact
        SELECT id,worker_id,available_gb,effective_gb,throttled,created_at FROM worker_memory_samples;
        DROP TABLE worker_memory_samples CASCADE;
        ALTER TABLE worker_memory_samples_compact RENAME TO worker_memory_samples;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_class WHERE oid=to_regclass('task_durations') AND relkind='p') THEN
        CREATE TABLE task_durations_compact (
            id UUID PRIMARY KEY DEFAULT gen_random_uuid(), created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            job_id UUID, job_type TEXT, model_ref TEXT, split_size INT, duration_ms BIGINT,
            worker_id UUID, engine TEXT, build_hash TEXT, task_id UUID
        );
        INSERT INTO task_durations_compact
        SELECT id,created_at,job_id,job_type,model_ref,split_size,duration_ms,worker_id,engine,build_hash,task_id FROM task_durations;
        DROP TABLE task_durations CASCADE;
        ALTER TABLE task_durations_compact RENAME TO task_durations;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_class WHERE oid=to_regclass('job_events') AND relkind='p') THEN
        CREATE TABLE job_events_compact (
            id UUID PRIMARY KEY DEFAULT gen_random_uuid(), created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            job_id UUID NOT NULL, task_id UUID, event TEXT NOT NULL, buyer_text TEXT, detail JSONB
        );
        INSERT INTO job_events_compact
        SELECT id,created_at,job_id,task_id,event,buyer_text,detail FROM job_events;
        DROP TABLE job_events CASCADE;
        ALTER TABLE job_events_compact RENAME TO job_events;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS worker_memory_samples (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), worker_id UUID,
    available_gb REAL, effective_gb REAL, throttled BOOLEAN,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS worker_memory_samples_worker_time_idx
    ON worker_memory_samples (worker_id, created_at DESC);

CREATE TABLE IF NOT EXISTS task_durations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    job_id UUID, job_type TEXT, model_ref TEXT, split_size INT, duration_ms BIGINT,
    worker_id UUID, engine TEXT, build_hash TEXT, task_id UUID
);
CREATE INDEX IF NOT EXISTS task_durations_type_model_idx ON task_durations (job_type, model_ref);

CREATE TABLE IF NOT EXISTS job_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    job_id UUID NOT NULL, task_id UUID, event TEXT NOT NULL, buyer_text TEXT, detail JSONB
);
CREATE INDEX IF NOT EXISTS job_events_job_idx ON job_events (job_id, created_at);

CREATE TABLE IF NOT EXISTS worker_model_state (
    worker_id      UUID NOT NULL,
    model_id       TEXT NOT NULL,
    last_seen_warm TIMESTAMPTZ DEFAULT now(),  -- last heartbeat that reported this model warm
    PRIMARY KEY (worker_id, model_id)
);
CREATE INDEX IF NOT EXISTS worker_model_state_model_idx
    ON worker_model_state (model_id, last_seen_warm DESC);  -- "which live workers have THIS model warm" (scheduler + quote)

CREATE TABLE IF NOT EXISTS worker_tps_cache (
    worker_id  UUID NOT NULL,
    job_type   TEXT NOT NULL,
    tps        REAL NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (worker_id, job_type)
);

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS watchdog_strikes INT NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS deadline_secs INT;
CREATE TABLE IF NOT EXISTS eta_calibration (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id         UUID,
    job_type       TEXT,
    tier           TEXT,
    predicted_secs INT,
    realized_secs  INT,
    created_at     TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS eta_calibration_type_idx ON eta_calibration (job_type, tier, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS eta_calibration_job_uniq ON eta_calibration (job_id);

CREATE TABLE IF NOT EXISTS charge_batches (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id   UUID NOT NULL,
    amount_usd NUMERIC(10,6) NOT NULL,       -- FROZEN sum of the member jobs at formation
    status     TEXT NOT NULL DEFAULT 'attempting',  -- attempting|charged
    stripe_pi  TEXT,                          -- the PaymentIntent id, once confirmed
    created_at TIMESTAMPTZ DEFAULT now(),
    charged_at TIMESTAMPTZ
);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS deferred_at TIMESTAMPTZ;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_attempt_usd NUMERIC(10,6);
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS next_at TIMESTAMPTZ;
ALTER TABLE charge_batches ALTER COLUMN amount_usd TYPE NUMERIC(12,6);

CREATE INDEX IF NOT EXISTS charge_batches_status_idx ON charge_batches (status, created_at);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_batch_id UUID;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_next_at  TIMESTAMPTZ;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS stripe_pi TEXT;
CREATE INDEX IF NOT EXISTS jobs_charge_status_idx ON jobs (charge_status);

CREATE TABLE IF NOT EXISTS buyer_charge_operations (
    operation_key        TEXT PRIMARY KEY CHECK (btrim(operation_key) <> ''),
    source_kind          TEXT NOT NULL CHECK (source_kind IN ('job','batch')),
    job_id               UUID UNIQUE REFERENCES jobs(id) ON DELETE RESTRICT,
    charge_batch_id      UUID UNIQUE REFERENCES charge_batches(id) ON DELETE RESTRICT,
    buyer_id             UUID NOT NULL,
    stripe_customer      TEXT NOT NULL CHECK (btrim(stripe_customer) <> ''),
    stripe_payment_method TEXT NOT NULL CHECK (btrim(stripe_payment_method) <> ''),
    amount_cents         BIGINT NOT NULL CHECK (amount_cents > 0),
    currency             TEXT NOT NULL CHECK (currency='usd'),
    status               TEXT NOT NULL CHECK (status IN ('outcome_unknown','succeeded')),
    payment_intent       TEXT UNIQUE,
    charge_id            TEXT UNIQUE,
    last_error           TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((source_kind='job' AND job_id IS NOT NULL AND charge_batch_id IS NULL)
        OR (source_kind='batch' AND charge_batch_id IS NOT NULL AND job_id IS NULL)),
    CHECK (status<>'succeeded' OR (payment_intent IS NOT NULL AND charge_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS buyer_charge_operations_status_idx
    ON buyer_charge_operations (status,created_at);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_stripe_fee_ref_uniq ON ledger_entries (payout_ref) WHERE kind = 'stripe_fee';

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_records  BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_bytes    BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_source   TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_records BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_bytes   BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_source  TEXT;

CREATE TABLE IF NOT EXISTS charge_batch_fee_allocations (
    charge_batch_id   UUID NOT NULL REFERENCES charge_batches(id) ON DELETE CASCADE,
    job_id            UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    stripe_pi         TEXT NOT NULL,
    allocation_ordinal INT NOT NULL,
    billed_weight_usd NUMERIC(12,6) NOT NULL CHECK (billed_weight_usd > 0),
    allocated_fee_usd NUMERIC(12,6) NOT NULL CHECK (allocated_fee_usd >= 0),
    allocated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (charge_batch_id, job_id),
    UNIQUE (job_id),
    UNIQUE (charge_batch_id, allocation_ordinal)
);
CREATE INDEX IF NOT EXISTS charge_batch_fee_allocations_pi_idx
    ON charge_batch_fee_allocations (stripe_pi);

ALTER TABLE quotes ADD COLUMN IF NOT EXISTS economic_schedule_version TEXT;
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS economic_plan JSONB;
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS economic_executable BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS economic_buyer_charge_usd NUMERIC(12,6);
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS economic_supplier_payout_usd NUMERIC(12,6);
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_frozen_economic_amounts_valid;
ALTER TABLE tasks ADD CONSTRAINT tasks_frozen_economic_amounts_valid CHECK (
    (economic_buyer_charge_usd IS NULL AND economic_supplier_payout_usd IS NULL)
    OR (economic_buyer_charge_usd > 0 AND economic_supplier_payout_usd >= 0
        AND economic_supplier_payout_usd <= economic_buyer_charge_usd)
);
CREATE OR REPLACE FUNCTION cx_reject_frozen_task_economics_update() RETURNS trigger AS $$
BEGIN
    IF OLD.economic_buyer_charge_usd IS DISTINCT FROM NEW.economic_buyer_charge_usd
       OR OLD.economic_supplier_payout_usd IS DISTINCT FROM NEW.economic_supplier_payout_usd THEN
        RAISE EXCEPTION 'task economic amounts for % are immutable', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS tasks_frozen_economics_immutable ON tasks;
CREATE TRIGGER tasks_frozen_economics_immutable
    BEFORE UPDATE OF economic_buyer_charge_usd, economic_supplier_payout_usd ON tasks
    FOR EACH ROW EXECUTE FUNCTION cx_reject_frozen_task_economics_update();

CREATE TABLE IF NOT EXISTS job_economic_plans (
    job_id                       UUID PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    plan_version                 SMALLINT NOT NULL,
    schedule_version             TEXT NOT NULL,
    plan_json                    JSONB NOT NULL,
    initial_task_count           INT NOT NULL CHECK (initial_task_count > 0),
    buyer_charge_per_task_usd    NUMERIC(12,6) NOT NULL CHECK (buyer_charge_per_task_usd > 0),
    supplier_payout_per_task_usd NUMERIC(12,6) NOT NULL CHECK (supplier_payout_per_task_usd >= 0),
    initial_buyer_charge_usd     NUMERIC(12,6) NOT NULL CHECK (initial_buyer_charge_usd > 0),
    reserved_buyer_charge_usd    NUMERIC(12,6) NOT NULL CHECK (reserved_buyer_charge_usd >= initial_buyer_charge_usd),
    sla_premium_usd              NUMERIC(12,6) NOT NULL DEFAULT 0 CHECK (sla_premium_usd >= 0),
    firm_quote_max_usd           NUMERIC(12,6),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE job_economic_plans DROP CONSTRAINT IF EXISTS job_economic_plans_firm_quote_max_positive;
ALTER TABLE job_economic_plans ADD CONSTRAINT job_economic_plans_firm_quote_max_positive
    CHECK (firm_quote_max_usd IS NULL OR firm_quote_max_usd > 0);
CREATE OR REPLACE FUNCTION cx_reject_job_economic_plan_update() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'job economic plan % is immutable', OLD.job_id;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS job_economic_plans_immutable ON job_economic_plans;
CREATE TRIGGER job_economic_plans_immutable
    BEFORE UPDATE ON job_economic_plans
    FOR EACH ROW EXECUTE FUNCTION cx_reject_job_economic_plan_update();

CREATE TABLE IF NOT EXISTS job_economic_reserves (
    job_id         UUID PRIMARY KEY REFERENCES job_economic_plans(job_id) ON DELETE CASCADE,
    reserved_tasks INT NOT NULL CHECK (reserved_tasks >= 0),
    consumed_tasks INT NOT NULL DEFAULT 0 CHECK (consumed_tasks >= 0 AND consumed_tasks <= reserved_tasks),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_job_sla_premium_ref_uniq
    ON ledger_entries (payout_ref)
    WHERE kind = 'buyer_charge' AND task_id IS NULL AND payout_ref IS NOT NULL;

CREATE TABLE IF NOT EXISTS alpha_requests (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ DEFAULT now(),
    email      TEXT NOT NULL,
    role       TEXT,   -- 'buyer' | 'supplier' | '' (unspecified)  -  whichever CTA was clicked
    note       TEXT,   -- optional free-text ("what would you run", etc.)
    source_ip  TEXT    -- for the same per-IP abuse-rate reasoning as signupLimiter
);
CREATE INDEX IF NOT EXISTS alpha_requests_created_at_idx ON alpha_requests (created_at);

ALTER TABLE models ADD COLUMN IF NOT EXISTS price_source  TEXT DEFAULT 'seed';
ALTER TABLE models ADD COLUMN IF NOT EXISTS price_formula TEXT;

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS firm_quote          BOOLEAN DEFAULT false;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS firm_quote_max_usd  NUMERIC(12,6);

ALTER TABLE quotes ADD COLUMN IF NOT EXISTS sla_guaranteed_secs INT;
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS sla_premium_usd     NUMERIC(12,6);
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS sla_guarantee_secs  INT;
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS sla_premium_usd     NUMERIC(12,6);
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS sla_met             BOOLEAN;
CREATE UNIQUE INDEX IF NOT EXISTS ledger_sla_refund_ref_uniq ON ledger_entries (payout_ref) WHERE kind = 'sla_refund';

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS billed_usd          NUMERIC(12,6);

ALTER TABLE jobs DROP COLUMN IF EXISTS routing_substrate;
ALTER TABLE jobs DROP COLUMN IF EXISTS routing_reason;
ALTER TABLE jobs DROP COLUMN IF EXISTS routing_fleet_eta_secs;
ALTER TABLE jobs DROP COLUMN IF EXISTS routing_gpu_modeled_secs;

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_requested_cents BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_received_cents  BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_currency        TEXT;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_requested_cents BIGINT;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_received_cents  BIGINT;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_currency        TEXT;

CREATE TABLE IF NOT EXISTS buyer_cash_collections (
    payment_intent  TEXT PRIMARY KEY CHECK (btrim(payment_intent) <> ''),
    charge_id       TEXT UNIQUE CHECK (charge_id IS NULL OR btrim(charge_id) <> ''),
    buyer_id        UUID NOT NULL,
    source_kind     TEXT NOT NULL CHECK (source_kind IN ('job','batch')),
    job_id          UUID UNIQUE REFERENCES jobs(id),
    charge_batch_id UUID UNIQUE REFERENCES charge_batches(id),
    requested_cents BIGINT NOT NULL CHECK (requested_cents > 0),
    received_cents  BIGINT NOT NULL CHECK (received_cents > 0),
    currency        TEXT NOT NULL CHECK (currency = 'usd'),
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (requested_cents = received_cents),
    CHECK ((source_kind = 'job' AND job_id IS NOT NULL AND charge_batch_id IS NULL)
        OR (source_kind = 'batch' AND charge_batch_id IS NOT NULL AND job_id IS NULL))
);
ALTER TABLE buyer_cash_collections ADD COLUMN IF NOT EXISTS charge_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS buyer_cash_collections_charge_id_uniq
    ON buyer_cash_collections (charge_id) WHERE charge_id IS NOT NULL;

INSERT INTO buyer_cash_collections
  (payment_intent,buyer_id,source_kind,job_id,requested_cents,received_cents,currency,recorded_at)
SELECT stripe_pi,buyer_id,'job',id,charge_requested_cents,charge_received_cents,charge_currency,
       now()
  FROM jobs
 WHERE charge_status='charged' AND COALESCE(stripe_pi,'') <> ''
   AND charge_requested_cents > 0
   AND charge_received_cents = charge_requested_cents
   AND charge_currency='usd'
ON CONFLICT DO NOTHING;

INSERT INTO buyer_cash_collections
  (payment_intent,buyer_id,source_kind,charge_batch_id,requested_cents,received_cents,currency,recorded_at)
SELECT stripe_pi,buyer_id,'batch',id,charge_requested_cents,charge_received_cents,charge_currency,
       COALESCE(charged_at,created_at,now())
  FROM charge_batches
 WHERE status='charged' AND COALESCE(stripe_pi,'') <> ''
   AND charge_requested_cents > 0
   AND charge_received_cents = charge_requested_cents
   AND charge_currency='usd'
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS stripe_webhook_events (
    event_id       TEXT PRIMARY KEY CHECK (btrim(event_id) <> ''),
    event_type     TEXT NOT NULL CHECK (event_type IN (
                     'charge.refunded','charge.dispute.created',
                     'charge.dispute.funds_withdrawn',
                     'charge.dispute.funds_reinstated','charge.dispute.closed')),
    object_id      TEXT NOT NULL CHECK (btrim(object_id) <> ''),
    charge_id      TEXT NOT NULL CHECK (btrim(charge_id) <> ''),
    payment_intent TEXT CHECK (payment_intent IS NULL OR btrim(payment_intent) <> ''),
    event_created  BIGINT NOT NULL CHECK (event_created > 0),
    payload_sha256 TEXT NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS stripe_webhook_events_object_idx
    ON stripe_webhook_events (event_type,object_id,event_created);

CREATE TABLE IF NOT EXISTS stripe_charge_cash_state (
    charge_id          TEXT PRIMARY KEY CHECK (btrim(charge_id) <> ''),
    payment_intent     TEXT CHECK (payment_intent IS NULL OR btrim(payment_intent) <> ''),
    amount_cents       BIGINT NOT NULL CHECK (amount_cents > 0),
    refunded_cents     BIGINT NOT NULL CHECK (
                         refunded_cents > 0 AND refunded_cents <= amount_cents),
    currency           TEXT NOT NULL CHECK (btrim(currency) <> ''),
    last_event_id      TEXT NOT NULL REFERENCES stripe_webhook_events(event_id) ON DELETE RESTRICT,
    last_event_created BIGINT NOT NULL CHECK (last_event_created > 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS stripe_charge_cash_state_pi_idx
    ON stripe_charge_cash_state (payment_intent) WHERE payment_intent IS NOT NULL;

CREATE TABLE IF NOT EXISTS stripe_dispute_cash_state (
    dispute_id         TEXT PRIMARY KEY CHECK (btrim(dispute_id) <> ''),
    charge_id          TEXT NOT NULL CHECK (btrim(charge_id) <> ''),
    payment_intent     TEXT CHECK (payment_intent IS NULL OR btrim(payment_intent) <> ''),
    amount_cents       BIGINT NOT NULL CHECK (amount_cents > 0),
    currency           TEXT NOT NULL CHECK (btrim(currency) <> ''),
    status             TEXT NOT NULL CHECK (btrim(status) <> ''),
    cash_unavailable   BOOLEAN NOT NULL DEFAULT false,
    cash_effect_created BIGINT NOT NULL DEFAULT 0 CHECK (cash_effect_created >= 0),
    cash_effect_rank   INTEGER NOT NULL DEFAULT 0 CHECK (cash_effect_rank >= 0),
    last_event_id      TEXT NOT NULL REFERENCES stripe_webhook_events(event_id) ON DELETE RESTRICT,
    last_event_type    TEXT NOT NULL,
    last_event_created BIGINT NOT NULL CHECK (last_event_created > 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS stripe_dispute_cash_state_pi_unavailable_idx
    ON stripe_dispute_cash_state (payment_intent)
    WHERE payment_intent IS NOT NULL AND cash_unavailable;

CREATE TABLE IF NOT EXISTS platform_subsidy_funds (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    authorization_action_id UUID NOT NULL UNIQUE REFERENCES admin_actions(id) ON DELETE RESTRICT,
    fund_ref              TEXT NOT NULL UNIQUE CHECK (btrim(fund_ref) <> ''),
    external_treasury_ref TEXT NOT NULL UNIQUE CHECK (btrim(external_treasury_ref) <> ''),
    authorized_cents      BIGINT NOT NULL CHECK (authorized_cents > 0),
    currency              TEXT NOT NULL CHECK (currency = 'usd'),
    reason                TEXT NOT NULL CHECK (btrim(reason) <> ''),
    status                TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','closed')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE platform_subsidy_funds ADD COLUMN IF NOT EXISTS authorization_action_id UUID;
CREATE UNIQUE INDEX IF NOT EXISTS platform_subsidy_funds_authorization_action_uniq
    ON platform_subsidy_funds (authorization_action_id)
    WHERE authorization_action_id IS NOT NULL;
ALTER TABLE platform_subsidy_funds DROP CONSTRAINT IF EXISTS platform_subsidy_funds_authorization_action_fkey;
ALTER TABLE platform_subsidy_funds ADD CONSTRAINT platform_subsidy_funds_authorization_action_fkey
    FOREIGN KEY (authorization_action_id) REFERENCES admin_actions(id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE platform_subsidy_funds DROP CONSTRAINT IF EXISTS platform_subsidy_funds_authorization_required;
ALTER TABLE platform_subsidy_funds ADD CONSTRAINT platform_subsidy_funds_authorization_required
    CHECK (authorization_action_id IS NOT NULL) NOT VALID;

CREATE TABLE IF NOT EXISTS supplier_payout_funding (
    id                           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    authorization_action_id      UUID UNIQUE REFERENCES admin_actions(id) ON DELETE RESTRICT,
    ledger_entry_id              UUID NOT NULL UNIQUE REFERENCES ledger_entries(id),
    source_kind                  TEXT NOT NULL CHECK (source_kind IN ('buyer_collection','platform_subsidy')),
    liability_job_id             UUID REFERENCES jobs(id),
    collection_payment_intent    TEXT REFERENCES buyer_cash_collections(payment_intent),
    subsidy_fund_id              UUID REFERENCES platform_subsidy_funds(id),
    subsidy_authorization_ref    TEXT UNIQUE,
    subsidy_reason               TEXT,
    amount_cents                 BIGINT NOT NULL CHECK (amount_cents > 0),
    currency                     TEXT NOT NULL CHECK (currency = 'usd'),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT supplier_payout_funding_source_valid CHECK (
      (source_kind='buyer_collection'
       AND liability_job_id IS NOT NULL
       AND collection_payment_intent IS NOT NULL
       AND subsidy_fund_id IS NULL
       AND subsidy_authorization_ref IS NULL AND subsidy_reason IS NULL)
      OR
      (source_kind='platform_subsidy'
       AND collection_payment_intent IS NULL
       AND subsidy_fund_id IS NOT NULL
       AND subsidy_authorization_ref IS NOT NULL AND btrim(subsidy_authorization_ref) <> ''
       AND subsidy_reason IS NOT NULL AND btrim(subsidy_reason) <> '')
    )
);
CREATE INDEX IF NOT EXISTS supplier_payout_funding_collection_idx
    ON supplier_payout_funding (collection_payment_intent)
    WHERE source_kind='buyer_collection';
CREATE INDEX IF NOT EXISTS supplier_payout_funding_subsidy_fund_idx
    ON supplier_payout_funding (subsidy_fund_id)
    WHERE source_kind='platform_subsidy';
ALTER TABLE supplier_payout_funding ADD COLUMN IF NOT EXISTS subsidy_fund_id UUID
    REFERENCES platform_subsidy_funds(id);
ALTER TABLE supplier_payout_funding ADD COLUMN IF NOT EXISTS authorization_action_id UUID;
CREATE UNIQUE INDEX IF NOT EXISTS supplier_payout_funding_authorization_action_uniq
    ON supplier_payout_funding (authorization_action_id)
    WHERE authorization_action_id IS NOT NULL;
ALTER TABLE supplier_payout_funding DROP CONSTRAINT IF EXISTS supplier_payout_funding_authorization_action_fkey;
ALTER TABLE supplier_payout_funding ADD CONSTRAINT supplier_payout_funding_authorization_action_fkey
    FOREIGN KEY (authorization_action_id) REFERENCES admin_actions(id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE supplier_payout_funding DROP CONSTRAINT IF EXISTS supplier_payout_funding_source_valid;
ALTER TABLE supplier_payout_funding ADD CONSTRAINT supplier_payout_funding_source_valid CHECK (
  (source_kind='buyer_collection'
   AND liability_job_id IS NOT NULL AND collection_payment_intent IS NOT NULL
   AND subsidy_fund_id IS NULL
   AND authorization_action_id IS NULL
   AND subsidy_authorization_ref IS NULL AND subsidy_reason IS NULL)
  OR
  (source_kind='platform_subsidy'
   AND collection_payment_intent IS NULL AND subsidy_fund_id IS NOT NULL
   AND authorization_action_id IS NOT NULL
   AND subsidy_authorization_ref IS NOT NULL AND btrim(subsidy_authorization_ref) <> ''
   AND subsidy_reason IS NOT NULL AND btrim(subsidy_reason) <> '')
) NOT VALID;

CREATE TABLE IF NOT EXISTS supplier_payout_funding_state (
    funding_id        UUID PRIMARY KEY REFERENCES supplier_payout_funding(id) ON DELETE RESTRICT,
    status            TEXT NOT NULL CHECK (status IN ('available','compromised')),
    compromised_cents BIGINT NOT NULL CHECK (compromised_cents >= 0),
    last_event_id     TEXT NOT NULL REFERENCES stripe_webhook_events(event_id) ON DELETE RESTRICT,
    reason            TEXT NOT NULL CHECK (btrim(reason) <> ''),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status='available' AND compromised_cents=0)
        OR (status='compromised' AND compromised_cents>0))
);
CREATE INDEX IF NOT EXISTS supplier_payout_funding_state_compromised_idx
    ON supplier_payout_funding_state (updated_at,funding_id)
    WHERE status='compromised';

CREATE TABLE IF NOT EXISTS supplier_minor_unit_settlements (
    ledger_entry_id    UUID PRIMARY KEY REFERENCES ledger_entries(id) ON DELETE RESTRICT,
    policy             TEXT NOT NULL CHECK (policy = 'floor_cent_carry_v1'),
    liability_microusd BIGINT NOT NULL CHECK (liability_microusd >= 0),
    cash_cents         BIGINT NOT NULL CHECK (cash_cents >= 0),
    remainder_microusd BIGINT NOT NULL CHECK (
                          remainder_microusd >= 0 AND remainder_microusd < 10000),
    currency           TEXT NOT NULL DEFAULT 'usd' CHECK (currency = 'usd'),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (liability_microusd = cash_cents * 10000 + remainder_microusd)
);

CREATE OR REPLACE FUNCTION validate_minor_unit_settlement_binding()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    ledger_kind TEXT;
    ledger_microusd BIGINT;
BEGIN
    SELECT kind,(amount_usd*1000000)::bigint
      INTO ledger_kind,ledger_microusd
      FROM ledger_entries WHERE id=NEW.ledger_entry_id;
    IF ledger_kind IS DISTINCT FROM 'supplier_credit'
       OR ledger_microusd IS DISTINCT FROM NEW.liability_microusd THEN
        RAISE EXCEPTION 'minor-unit settlement does not match supplier-credit liability';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS supplier_minor_unit_settlement_binding
    ON supplier_minor_unit_settlements;
CREATE TRIGGER supplier_minor_unit_settlement_binding
BEFORE INSERT ON supplier_minor_unit_settlements
FOR EACH ROW EXECUTE FUNCTION validate_minor_unit_settlement_binding();

CREATE OR REPLACE FUNCTION reject_settled_ledger_money_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.kind,OLD.amount_usd) IS DISTINCT FROM (NEW.kind,NEW.amount_usd)
       AND EXISTS (SELECT 1 FROM supplier_minor_unit_settlements
                    WHERE ledger_entry_id=OLD.id) THEN
        RAISE EXCEPTION 'settled supplier liability identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS settled_ledger_money_immutable ON ledger_entries;
CREATE TRIGGER settled_ledger_money_immutable
BEFORE UPDATE OF kind,amount_usd ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION reject_settled_ledger_money_mutation();

CREATE OR REPLACE FUNCTION reject_minor_unit_settlement_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'supplier minor-unit settlements are append-only';
END;
$$;
DROP TRIGGER IF EXISTS supplier_minor_unit_settlement_append_only
    ON supplier_minor_unit_settlements;
CREATE TRIGGER supplier_minor_unit_settlement_append_only
BEFORE UPDATE OR DELETE ON supplier_minor_unit_settlements
FOR EACH ROW EXECUTE FUNCTION reject_minor_unit_settlement_mutation();

CREATE TABLE IF NOT EXISTS supplier_payout_operations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ledger_entry_id  UUID NOT NULL UNIQUE REFERENCES ledger_entries(id) ON DELETE RESTRICT,
    funding_id       UUID REFERENCES supplier_payout_funding(id),
    supplier_id      UUID NOT NULL REFERENCES suppliers(id),
    requested_cents  BIGINT NOT NULL CHECK (requested_cents > 0),
    sent_cents       BIGINT CHECK (sent_cents > 0),
    currency         TEXT NOT NULL DEFAULT 'usd' CHECK (currency = 'usd'),
    status           TEXT NOT NULL CHECK (status IN (
                       'sending','ready','outcome_unknown','released','exported',
                       'clawed_back','reversal_required','reversed')),
    cash_moved       BOOLEAN NOT NULL DEFAULT false,
    outcome_unknown  BOOLEAN NOT NULL DEFAULT false,
    transfer_ref     TEXT,
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (NOT cash_moved OR (sent_cents IS NOT NULL AND transfer_ref IS NOT NULL)),
    CHECK (NOT outcome_unknown OR NOT cash_moved)
);
CREATE UNIQUE INDEX IF NOT EXISTS supplier_payout_operations_transfer_ref_uniq
    ON supplier_payout_operations (transfer_ref) WHERE transfer_ref IS NOT NULL AND cash_moved;
CREATE INDEX IF NOT EXISTS supplier_payout_operations_status_idx
    ON supplier_payout_operations (status, updated_at);
ALTER TABLE supplier_payout_operations ADD COLUMN IF NOT EXISTS funding_id UUID
    REFERENCES supplier_payout_funding(id);
ALTER TABLE supplier_payout_operations ADD COLUMN IF NOT EXISTS outcome_unknown BOOLEAN
    NOT NULL DEFAULT false;
ALTER TABLE supplier_payout_operations DROP CONSTRAINT IF EXISTS supplier_payout_operations_status_check;
ALTER TABLE supplier_payout_operations ADD CONSTRAINT supplier_payout_operations_status_check
    CHECK (status IN ('sending','ready','outcome_unknown','released','exported',
                      'clawed_back','reversal_required','reversed'));
ALTER TABLE supplier_payout_operations DROP CONSTRAINT IF EXISTS supplier_payout_operations_outcome_unknown_check;
ALTER TABLE supplier_payout_operations ADD CONSTRAINT supplier_payout_operations_outcome_unknown_check
    CHECK (NOT outcome_unknown OR NOT cash_moved);
ALTER TABLE supplier_payout_operations DROP CONSTRAINT IF EXISTS supplier_payout_operations_ledger_entry_id_fkey;
ALTER TABLE supplier_payout_operations ADD CONSTRAINT supplier_payout_operations_ledger_entry_id_fkey
    FOREIGN KEY (ledger_entry_id) REFERENCES ledger_entries(id) ON DELETE RESTRICT;

CREATE OR REPLACE FUNCTION reject_immutable_money_fact_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR OLD IS DISTINCT FROM NEW THEN
        RAISE EXCEPTION '% rows are append-only', TG_TABLE_NAME;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS buyer_cash_collections_append_only ON buyer_cash_collections;
CREATE TRIGGER buyer_cash_collections_append_only
BEFORE UPDATE OR DELETE ON buyer_cash_collections
FOR EACH ROW EXECUTE FUNCTION reject_immutable_money_fact_mutation();
DROP TRIGGER IF EXISTS stripe_webhook_events_append_only ON stripe_webhook_events;
CREATE TRIGGER stripe_webhook_events_append_only
BEFORE UPDATE OR DELETE ON stripe_webhook_events
FOR EACH ROW EXECUTE FUNCTION reject_immutable_money_fact_mutation();
CREATE OR REPLACE FUNCTION protect_buyer_charge_operation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'buyer charge operations cannot be deleted';
    END IF;
    IF (OLD.operation_key,OLD.source_kind,OLD.job_id,OLD.charge_batch_id,
        OLD.buyer_id,OLD.stripe_customer,OLD.stripe_payment_method,
        OLD.amount_cents,OLD.currency,OLD.created_at)
       IS DISTINCT FROM
       (NEW.operation_key,NEW.source_kind,NEW.job_id,NEW.charge_batch_id,
        NEW.buyer_id,NEW.stripe_customer,NEW.stripe_payment_method,
        NEW.amount_cents,NEW.currency,NEW.created_at) THEN
        RAISE EXCEPTION 'buyer charge operation request identity is immutable';
    END IF;
    IF OLD.status='succeeded' AND
       (NEW.status,NEW.payment_intent,NEW.charge_id) IS DISTINCT FROM
       (OLD.status,OLD.payment_intent,OLD.charge_id) THEN
        RAISE EXCEPTION 'succeeded buyer charge evidence is immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS buyer_charge_operation_identity_immutable ON buyer_charge_operations;
CREATE TRIGGER buyer_charge_operation_identity_immutable
BEFORE UPDATE OR DELETE ON buyer_charge_operations
FOR EACH ROW EXECUTE FUNCTION protect_buyer_charge_operation();
DROP TRIGGER IF EXISTS supplier_payout_funding_append_only ON supplier_payout_funding;
CREATE TRIGGER supplier_payout_funding_append_only
BEFORE UPDATE OR DELETE ON supplier_payout_funding
FOR EACH ROW EXECUTE FUNCTION reject_immutable_money_fact_mutation();

CREATE OR REPLACE FUNCTION protect_subsidy_fund_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'platform subsidy funds cannot be deleted';
    END IF;
    IF (OLD.id, OLD.authorization_action_id, OLD.fund_ref,
        OLD.external_treasury_ref, OLD.authorized_cents, OLD.currency,
        OLD.reason, OLD.created_at)
       IS DISTINCT FROM
       (NEW.id, NEW.authorization_action_id, NEW.fund_ref,
        NEW.external_treasury_ref, NEW.authorized_cents, NEW.currency,
        NEW.reason, NEW.created_at) THEN
        RAISE EXCEPTION 'platform subsidy fund authorization identity is immutable';
    END IF;
    IF OLD.status = 'closed' AND NEW.status <> 'closed' THEN
        RAISE EXCEPTION 'closed platform subsidy funds cannot be reopened';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS platform_subsidy_funds_identity_immutable ON platform_subsidy_funds;
CREATE TRIGGER platform_subsidy_funds_identity_immutable
BEFORE UPDATE OR DELETE ON platform_subsidy_funds
FOR EACH ROW EXECUTE FUNCTION protect_subsidy_fund_identity();

CREATE OR REPLACE FUNCTION validate_money_authority_action_binding()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_ok BOOLEAN;
BEGIN
    IF NEW.kind IN ('subsidy_fund_authorized','payout_subsidy_authorized') THEN
        SELECT EXISTS (
            SELECT 1 FROM api_keys k
             WHERE NEW.actor_mode = 'break_glass_api_key'
               AND k.id = NEW.actor_principal_id
               AND k.is_admin = true AND k.revoked = false
        ) INTO binding_ok;
        IF NOT COALESCE(binding_ok, false) THEN
            RAISE EXCEPTION 'money authority action % has no live authenticated credential binding', NEW.id;
        END IF;
    END IF;
    IF NEW.kind = 'subsidy_fund_authorized' THEN
        SELECT EXISTS (
            SELECT 1
              FROM platform_subsidy_funds f
             WHERE f.authorization_action_id = NEW.id
               AND NEW.target_kind = 'subsidy_fund'
               AND NEW.target_id = f.id AND NEW.fund_id = f.id
               AND NEW.fund_ref = f.fund_ref
               AND NEW.authorization_ref IS NULL
               AND NEW.amount_cents = f.authorized_cents
               AND NEW.currency = f.currency AND NEW.reason = f.reason
        ) INTO binding_ok;
    ELSIF NEW.kind = 'payout_subsidy_authorized' THEN
        SELECT EXISTS (
            SELECT 1
              FROM supplier_payout_funding p
              JOIN platform_subsidy_funds f ON f.id = p.subsidy_fund_id
             WHERE p.authorization_action_id = NEW.id
               AND p.source_kind = 'platform_subsidy'
               AND NEW.target_kind = 'supplier_liability'
               AND NEW.target_id = p.ledger_entry_id
               AND NEW.ledger_entry_id = p.ledger_entry_id
               AND NEW.fund_id = f.id AND NEW.fund_ref = f.fund_ref
               AND NEW.authorization_ref = p.subsidy_authorization_ref
               AND NEW.amount_cents = p.amount_cents
               AND NEW.currency = p.currency AND NEW.reason = p.subsidy_reason
        ) INTO binding_ok;
    ELSE
        RETURN NEW;
    END IF;
    IF NOT COALESCE(binding_ok, false) THEN
        RAISE EXCEPTION 'money authority action % has no exact resource binding', NEW.id;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS admin_actions_money_binding ON admin_actions;
CREATE CONSTRAINT TRIGGER admin_actions_money_binding
AFTER INSERT ON admin_actions DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_money_authority_action_binding();

CREATE OR REPLACE FUNCTION validate_money_authority_resource_binding()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_ok BOOLEAN;
BEGIN
    IF TG_TABLE_NAME = 'platform_subsidy_funds' THEN
        SELECT EXISTS (
            SELECT 1 FROM admin_actions a
             WHERE a.id = NEW.authorization_action_id
               AND a.kind = 'subsidy_fund_authorized'
               AND a.target_kind = 'subsidy_fund'
               AND a.target_id = NEW.id AND a.fund_id = NEW.id
               AND a.fund_ref = NEW.fund_ref AND a.authorization_ref IS NULL
               AND a.amount_cents = NEW.authorized_cents
               AND a.currency = NEW.currency AND a.reason = NEW.reason
        ) INTO binding_ok;
    ELSE
        IF NEW.source_kind <> 'platform_subsidy' THEN
            RETURN NEW;
        END IF;
        SELECT EXISTS (
            SELECT 1
              FROM admin_actions a
              JOIN platform_subsidy_funds f ON f.id = NEW.subsidy_fund_id
             WHERE a.id = NEW.authorization_action_id
               AND a.kind = 'payout_subsidy_authorized'
               AND a.target_kind = 'supplier_liability'
               AND a.target_id = NEW.ledger_entry_id
               AND a.ledger_entry_id = NEW.ledger_entry_id
               AND a.fund_id = f.id AND a.fund_ref = f.fund_ref
               AND a.authorization_ref = NEW.subsidy_authorization_ref
               AND a.amount_cents = NEW.amount_cents
               AND a.currency = NEW.currency AND a.reason = NEW.subsidy_reason
        ) INTO binding_ok;
    END IF;
    IF NOT COALESCE(binding_ok, false) THEN
        RAISE EXCEPTION '% row has no exact money authority action binding', TG_TABLE_NAME;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS platform_subsidy_funds_money_binding ON platform_subsidy_funds;
CREATE CONSTRAINT TRIGGER platform_subsidy_funds_money_binding
AFTER INSERT ON platform_subsidy_funds DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_money_authority_resource_binding();
DROP TRIGGER IF EXISTS supplier_payout_funding_money_binding ON supplier_payout_funding;
CREATE CONSTRAINT TRIGGER supplier_payout_funding_money_binding
AFTER INSERT ON supplier_payout_funding DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_money_authority_resource_binding();

CREATE OR REPLACE FUNCTION reject_payout_operation_identity_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.ledger_entry_id,OLD.funding_id,OLD.supplier_id,OLD.requested_cents,
        OLD.currency,OLD.created_at)
       IS DISTINCT FROM
       (NEW.ledger_entry_id,NEW.funding_id,NEW.supplier_id,NEW.requested_cents,
        NEW.currency,NEW.created_at) THEN
        RAISE EXCEPTION 'supplier payout operation identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS supplier_payout_operation_identity_immutable
    ON supplier_payout_operations;
CREATE TRIGGER supplier_payout_operation_identity_immutable
BEFORE UPDATE ON supplier_payout_operations
FOR EACH ROW EXECUTE FUNCTION reject_payout_operation_identity_mutation();
CREATE OR REPLACE FUNCTION reject_payout_operation_delete()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'supplier payout operations are append-only state records';
END;
$$;
DROP TRIGGER IF EXISTS supplier_payout_operation_no_delete
    ON supplier_payout_operations;
CREATE TRIGGER supplier_payout_operation_no_delete
BEFORE DELETE ON supplier_payout_operations
FOR EACH ROW EXECUTE FUNCTION reject_payout_operation_delete();

CREATE TABLE IF NOT EXISTS lifecycle_transitions (
    entity TEXT NOT NULL,
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    PRIMARY KEY (entity,from_state,to_state)
);
DELETE FROM lifecycle_transitions;
INSERT INTO lifecycle_transitions (entity,from_state,to_state) VALUES
('job','queued','running'),('job','queued','cancelled'),('job','queued','failed'),
('job','running','verifying'),('job','running','complete'),('job','running','failed'),('job','running','cancelled'),
('job','verifying','running'),('job','verifying','complete'),('job','verifying','failed'),
('task','queued','running'),('task','queued','retrying'),('task','queued','failed'),
('task','queued','cancelled'),
('task','running','queued'),('task','running','verifying'),('task','running','complete'),('task','running','retrying'),('task','running','failed'),('task','running','cancelled'),
('task','verifying','running'),('task','verifying','complete'),('task','verifying','retrying'),('task','verifying','failed'),
('task','verifying','cancelled'),
('task','retrying','queued'),('task','retrying','running'),('task','retrying','failed'),('task','retrying','cancelled'),
('task','complete','retrying'),('task','complete','failed'),('task','failed','retrying'),
('verification','pending','leased'),('verification','pending','terminal'),
('verification','leased','pending'),('verification','leased','terminal'),
('job_charge','not_attempted','deferred'),('job_charge','not_attempted','charged'),
('job_charge','not_attempted','failed'),('job_charge','not_attempted','no_payment_method'),('job_charge','not_attempted','outcome_unknown'),
('job_charge','deferred','not_attempted'),('job_charge','deferred','charged'),
('job_charge','deferred','failed'),('job_charge','deferred','no_payment_method'),('job_charge','deferred','outcome_unknown'),
('job_charge','no_payment_method','deferred'),('job_charge','no_payment_method','outcome_unknown'),
('job_charge','failed','not_attempted'),('job_charge','failed','charged'),('job_charge','failed','outcome_unknown'),
('job_charge','outcome_unknown','charged'),('job_charge','outcome_unknown','failed'),('job_charge','outcome_unknown','deferred'),
('charge_batch','attempting','charged'),('charge_batch','attempting','outcome_unknown'),('charge_batch','outcome_unknown','charged'),
('charge_operation','outcome_unknown','succeeded'),
('payout','pending','held'),('payout','pending','awaiting_funding'),('payout','pending','ready'),
('payout','held','awaiting_funding'),('payout','held','ready'),('payout','held','sending'),
('payout','held','released'),('payout','held','exported'),('payout','held','carried'),('payout','held','clawed_back'),('payout','held','reversal_required'),
('payout','awaiting_funding','held'),('payout','awaiting_funding','ready'),('payout','awaiting_funding','sending'),
('payout','awaiting_funding','clawed_back'),('payout','awaiting_funding','reversal_required'),
('payout','ready','held'),('payout','ready','sending'),('payout','ready','released'),('payout','ready','exported'),
('payout','ready','clawed_back'),('payout','ready','reversal_required'),
('payout','sending','ready'),('payout','sending','released'),('payout','sending','outcome_unknown'),
('payout','sending','reversal_required'),
('payout','outcome_unknown','ready'),('payout','outcome_unknown','released'),('payout','outcome_unknown','reversal_required'),
('payout','released','clawed_back'),('payout','released','reversal_required'),('payout','exported','reversal_required'),
('payout','carried','held'),('payout','carried','awaiting_funding'),
('webhook','pending','leased'),('webhook','pending','delivered'),('webhook','pending','dead'),
('webhook','leased','pending'),('webhook','leased','delivered'),('webhook','leased','dead');

CREATE OR REPLACE FUNCTION cx_guard_lifecycle_transition() RETURNS trigger AS $$
DECLARE old_state TEXT; new_state TEXT;
BEGIN
    old_state := to_jsonb(OLD)->>TG_ARGV[1];
    new_state := to_jsonb(NEW)->>TG_ARGV[1];
    IF old_state IS NOT DISTINCT FROM new_state THEN RETURN NEW; END IF;
    IF NOT EXISTS (
        SELECT 1 FROM lifecycle_transitions
         WHERE entity=TG_ARGV[0] AND from_state=old_state AND to_state=new_state
    ) THEN
        RAISE EXCEPTION 'illegal % lifecycle transition: % -> %',TG_ARGV[0],old_state,new_state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS jobs_lifecycle_guard ON jobs;
CREATE TRIGGER jobs_lifecycle_guard BEFORE UPDATE OF status ON jobs
FOR EACH ROW EXECUTE FUNCTION cx_guard_lifecycle_transition('job','status');
DROP TRIGGER IF EXISTS tasks_lifecycle_guard ON tasks;
CREATE TRIGGER tasks_lifecycle_guard BEFORE UPDATE OF status ON tasks
FOR EACH ROW EXECUTE FUNCTION cx_guard_lifecycle_transition('task','status');
DROP TRIGGER IF EXISTS verification_work_lifecycle_guard ON verification_work;
CREATE TRIGGER verification_work_lifecycle_guard BEFORE UPDATE OF status ON verification_work
FOR EACH ROW EXECUTE FUNCTION cx_guard_lifecycle_transition('verification','status');
DROP TRIGGER IF EXISTS jobs_charge_lifecycle_guard ON jobs;
CREATE TRIGGER jobs_charge_lifecycle_guard BEFORE UPDATE OF charge_status ON jobs
FOR EACH ROW EXECUTE FUNCTION cx_guard_lifecycle_transition('job_charge','charge_status');
DROP TRIGGER IF EXISTS charge_batches_lifecycle_guard ON charge_batches;
CREATE TRIGGER charge_batches_lifecycle_guard BEFORE UPDATE OF status ON charge_batches
FOR EACH ROW EXECUTE FUNCTION cx_guard_lifecycle_transition('charge_batch','status');
DROP TRIGGER IF EXISTS charge_operations_lifecycle_guard ON buyer_charge_operations;
CREATE TRIGGER charge_operations_lifecycle_guard BEFORE UPDATE OF status ON buyer_charge_operations
FOR EACH ROW EXECUTE FUNCTION cx_guard_lifecycle_transition('charge_operation','status');
DROP TRIGGER IF EXISTS payouts_lifecycle_guard ON ledger_entries;
CREATE TRIGGER payouts_lifecycle_guard BEFORE UPDATE OF payout_status ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION cx_guard_lifecycle_transition('payout','payout_status');

CREATE OR REPLACE FUNCTION cx_webhook_lifecycle_guard() RETURNS trigger AS $$
DECLARE old_state TEXT; new_state TEXT;
BEGIN
    old_state := CASE WHEN OLD.delivered_at IS NOT NULL THEN 'delivered'
                      WHEN OLD.dead_lettered_at IS NOT NULL THEN 'dead'
                      WHEN OLD.lease_token IS NOT NULL THEN 'leased' ELSE 'pending' END;
    new_state := CASE WHEN NEW.delivered_at IS NOT NULL THEN 'delivered'
                      WHEN NEW.dead_lettered_at IS NOT NULL THEN 'dead'
                      WHEN NEW.lease_token IS NOT NULL THEN 'leased' ELSE 'pending' END;
    IF old_state IS NOT DISTINCT FROM new_state THEN RETURN NEW; END IF;
    IF NOT EXISTS (
        SELECT 1 FROM lifecycle_transitions
         WHERE entity='webhook' AND from_state=old_state AND to_state=new_state
    ) THEN RAISE EXCEPTION 'illegal webhook lifecycle transition: % -> %',old_state,new_state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS webhooks_lifecycle_guard ON webhooks;
CREATE TRIGGER webhooks_lifecycle_guard
BEFORE UPDATE OF delivered_at,dead_lettered_at,lease_token,lease_expires_at ON webhooks
FOR EACH ROW EXECUTE FUNCTION cx_webhook_lifecycle_guard();

DROP TABLE IF EXISTS admin_sessions;
DROP TABLE IF EXISTS admin_credentials;
DROP TABLE IF EXISTS private_pool_members;
DROP TABLE IF EXISTS job_economic_facts;
DROP TABLE IF EXISTS site_events;
