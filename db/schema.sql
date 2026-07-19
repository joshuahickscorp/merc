-- Computexchange — single authoritative schema (PostgreSQL).
--
-- One file, applied via `make migrate` (psql --single-transaction against
-- $DATABASE_URL). Transcribed
-- from the action plan's "Data model sketches" plus the control-plane additions
-- (queue/auth/honeypot tables). Re-runnable: every object uses IF NOT EXISTS, so
-- applying twice is a no-op rather than an error.
--
-- The job queue is Postgres, not NATS (BLACKHOLE compression): workers claim work
-- via `SELECT ... FOR UPDATE SKIP LOCKED` over `tasks`, gated on (status, visible_at).

-- Every supported schema runner wraps this file in one transaction. The lock
-- serializes it with control-plane startup migrations, so two replicas can
-- never interleave trigger/constraint replacement or expose a partial schema.
SELECT pg_advisory_xact_lock(
    hashtextextended('computeexchange-control-schema-v1', 0)
);

CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid()

-- ─────────────────────────────────────────────────────────────────────────────
-- Core domain
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS suppliers (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at     TIMESTAMPTZ DEFAULT now(),
    email          TEXT NOT NULL UNIQUE,
    -- The FK is added after buyers is created below. Nullable preserves seeded and
    -- legacy suppliers, but self-serve supplier APIs only operate on owned rows.
    owner_buyer_id UUID,
    stripe_acct    TEXT,               -- Stripe Connect account ID; Stripe hosts KYC/tax collection
    reputation     REAL DEFAULT 0.5,   -- 0.0–1.0
    tier           SMALLINT DEFAULT 0, -- 0–3
    status         TEXT DEFAULT 'pending',  -- pending|active|suspended|banned
    -- Maintained running count of this supplier's lifetime completed tasks
    -- (Control Plane Hot Path 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md):
    -- incremented once per real commit (CommitTask) instead of ClaimTask
    -- re-scanning `tasks` with a `count(*)` on every single claim.
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
    input_ref     TEXT,                   -- per-task input chunk object key (null = inherit job.input_ref)
    -- Exact non-blank input rows this task must return for record-shaped jobs.
    -- NULL is explicit legacy/opaque unknown; it is never backfilled from the
    -- job split ceiling because a short final chunk may contain fewer rows.
    expected_output_records BIGINT,
    is_honeypot   BOOLEAN DEFAULT false,
    is_redundancy BOOLEAN DEFAULT false,
    retry_count   SMALLINT DEFAULT 0,
    -- Postgres-queue columns (SKIP LOCKED claim + retry visibility):
    claimed_by      UUID,                     -- worker currently holding the task
    claimed_at      TIMESTAMPTZ,              -- when the claim was taken
    visible_at      TIMESTAMPTZ DEFAULT now(),-- task is claimable only once now() >= visible_at
    -- Verification-requeue worker exclusion (Scheduling & Matching Engine 8->9,
    -- docs/internal/CREED_AND_PATH_TO_TEN.md "add backoff plus worker-exclusion to
    -- verification-requeue so a chunk that just failed verification doesn't
    -- immediately return to the same worker with no delay"). When a task is requeued
    -- after a failed honeypot, RequeueTask records the worker that just failed it
    -- here and until excluded_until; the claim query skips that worker for the
    -- window (a DIFFERENT worker gets first crack), then the exclusion expires so a
    -- thin/single-worker fleet is never permanently starved of the retry.
    excluded_worker UUID,                     -- worker to skip on the next claim (the one that just failed it)
    excluded_until  TIMESTAMPTZ               -- exclusion is only in force while now() < excluded_until (NULL = no exclusion)
);
-- input_ref added after the fact for already-created tables (idempotent ALTER).
-- A task is a split of its job; when its own input_ref is null the dispatch uses
-- the parent job's input_ref. This column lets a job fan out into per-chunk
-- inputs without a second table.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS input_ref TEXT;
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
-- Verification-requeue worker exclusion (Scheduling & Matching Engine 8->9) —
-- idempotent ALTERs for already-created tables. See the column comments above.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS excluded_worker UUID;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS excluded_until  TIMESTAMPTZ;

-- E3 · autovacuum tuning for the hottest table. `tasks` churns constantly: every
-- claim/start/commit/fail/requeue is an UPDATE, so dead tuples (and the visibility
-- bloat that slows the FOR UPDATE SKIP LOCKED claim scan) accumulate fast. The DB
-- defaults (scale_factor 0.2 = vacuum/analyze only after 20% of the table turns
-- over) are tuned for cold tables and let bloat ride on a queue. Drop to small
-- scale factors with flat thresholds so autovacuum fires on absolute churn, and
-- raise the cost limit so a vacuum keeps pace instead of falling behind under load.
-- Idempotent (ALTER ... SET is a no-op replay) and table-scoped · no global GUC change.
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
                                  -- 'stripe_fee': the REAL Stripe processing fee of one successful PaymentIntent
                                  -- (latest_charge.balance_transaction.fee, fetched — never estimated), stored
                                  -- negative with payout_ref = the PaymentIntent id. One row per PI, enforced by
                                  -- ledger_stripe_fee_ref_uniq below, so a retried fee fetch can never double-count.
    supplier_id   UUID REFERENCES suppliers,
    buyer_id      UUID,
    task_id       UUID REFERENCES tasks,
    amount_usd    NUMERIC(10,6) NOT NULL,  -- positive = credit, negative = debit
    payout_status TEXT DEFAULT 'pending',  -- pending|held|awaiting_funding|ready|sending|outcome_unknown|carried|released|exported|clawed_back|reversal_required
    release_at    TIMESTAMPTZ,             -- when payout hold expires
    payout_ref    TEXT                     -- Stripe/Trolley transfer ID
);

-- A SUPPLIER payout may only be 'released' WITH a real rail reference — never a faked
-- transfer (BLACKHOLE). Enforced structurally so no code path or bug can record a payout
-- without proof of money movement. Scoped to supplier_credit: buyer_charge and
-- platform_take are internal bookkeeping rows marked 'released' (settled, no transfer ref).
-- Idempotent (drop+add) so re-applying the schema is safe.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_released_requires_ref;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_released_requires_ref
    CHECK (kind <> 'supplier_credit' OR payout_status <> 'released' OR payout_ref IS NOT NULL);
CREATE INDEX IF NOT EXISTS ledger_due_payout_idx
    ON ledger_entries (release_at,id)
    WHERE kind='supplier_credit' AND payout_status='held' AND release_at IS NOT NULL;

-- A task produces exactly one ledger entry per kind (buyer_charge / supplier_credit
-- / platform_take / clawback). This uniqueness makes InsertLedgerEntries idempotent
-- under retry (ON CONFLICT DO NOTHING) so a double-commit can never double-charge a
-- buyer or double-pay a supplier. Job-level entries (NULL task_id) stay unconstrained
-- (SQL NULLs are distinct) and are guarded by their own once-only logic.
CREATE UNIQUE INDEX IF NOT EXISTS ledger_task_kind_uniq ON ledger_entries (task_id, kind);

-- ─────────────────────────────────────────────────────────────────────────────
-- Auth + verification (control-plane additions; no silent auth bypass — rows
-- in api_keys/worker_tokens MUST be seeded for any request to authenticate).
-- ─────────────────────────────────────────────────────────────────────────────

-- Self-serve buyer accounts. Until now a buyer_id was a free-floating UUID minted
-- by the seed / referenced by api_keys; there was no way to sign UP. This is the
-- account of record: a UNIQUE email + a bcrypt password_hash (cost >= 12, set in
-- control/accounts.go · NEVER a plaintext or a fast hash). free_credit_usd is the
-- sandbox grant a new buyer gets so they can run jobs before adding a card; the 402
-- submit gate exempts spend up to this, then requires a card honestly. The id is the
-- buyer_id every existing buyer-scoped table already keys on, so accounts slot in
-- without a data migration. password_hash is nullable so a seeded / API-key-only
-- buyer (no password) is representable; login requires a non-null hash.
CREATE TABLE IF NOT EXISTS buyers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           TEXT UNIQUE NOT NULL,
    password_hash   TEXT,                      -- bcrypt; NULL = no password set (seed / API-key-only)
    free_credit_usd NUMERIC(12,6) NOT NULL DEFAULT 0,  -- sandbox grant; 402 gate exempts spend up to this
    created_at      TIMESTAMPTZ DEFAULT now()
);

-- Supplier account ownership. Older deployments keyed supplier self-service by
-- caller-supplied email, which allowed one buyer credential to operate on another
-- supplier. Backfill only genuinely one-to-one, case-insensitive email matches.
-- Ambiguous or unmatched legacy suppliers intentionally remain NULL: they are
-- inert to the self-serve routes until an operator resolves ownership explicitly.
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

-- KYC and tax identifiers belong at Stripe's hosted Connect boundary. Purge the
-- legacy plaintext columns rather than retaining sensitive copies in CX.
ALTER TABLE suppliers DROP COLUMN IF EXISTS tax_id;
ALTER TABLE suppliers DROP COLUMN IF EXISTS tax_country;

-- Opaque session tokens for the web/app login flow, hashed at rest exactly like
-- api_keys/worker_tokens (only the SHA-256 hash is stored; the raw token is shown
-- once at login and never recoverable). authBuyer accepts a cx_sess_ bearer as well
-- as an api key. expires_at bounds the session; revoked supports explicit logout.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,               -- SHA-256 of the raw cx_sess_ token
    buyer_id   UUID NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked    BOOLEAN DEFAULT false
);
CREATE INDEX IF NOT EXISTS sessions_buyer_idx ON sessions (buyer_id);

-- Account linking uses two independent, random browser capabilities: OAuth's
-- public state value and an HttpOnly initiation cookie. Store only their hashes,
-- bind them to the initiating buyer, and atomically stamp consumed_at at callback.
-- This makes state short-lived and single-use without exposing a buyer UUID in it.

CREATE TABLE IF NOT EXISTS api_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id   UUID,
    key_hash   TEXT UNIQUE NOT NULL,      -- store a hash, never the raw key
    is_admin   BOOLEAN DEFAULT false,
    created_at TIMESTAMPTZ DEFAULT now(),
    revoked    BOOLEAN DEFAULT false
);
-- Buyer-managed key lifecycle (POST/GET/DELETE /v1/keys). `name` is a human label;
-- `masked` is a NON-secret display hint captured at mint (prefix + last4) so the list
-- view can show which key is which WITHOUT ever reconstructing the raw secret (only
-- key_hash is stored · the raw value is revealed once and is unrecoverable). Idempotent
-- ALTERs so older DBs created before the lifecycle columns upgrade cleanly.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS name   TEXT;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS masked TEXT;

-- Admin passkey (WebAuthn) credentials — the operator's own device authenticators
-- (Touch ID / a security key) for the /admin panel. Single-operator: every row is the
-- one operator's. None of these fields is a secret: a credential id + COSE public key
-- gate nothing on their own — only a device holding the matching PRIVATE key can
-- produce a valid assertion — so they are stored plainly. sign_count is the standard
-- clone-detection counter (a decrease signals a cloned authenticator).
CREATE TABLE IF NOT EXISTS admin_credentials (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    credential_id BYTEA NOT NULL UNIQUE,      -- the WebAuthn credential id (lookup + exclusion)
    credential    JSONB NOT NULL,             -- the full go-webauthn Credential (public key, sign_count, aaguid, …)
    label         TEXT,                       -- human label, e.g. "Josh MacBook Touch ID"
    created_at    TIMESTAMPTZ DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    revoked       BOOLEAN NOT NULL DEFAULT false
);
ALTER TABLE admin_credentials ADD COLUMN IF NOT EXISTS revoked BOOLEAN NOT NULL DEFAULT false;

-- Admin sessions minted after a successful passkey login. Kept SEPARATE from the
-- buyer `sessions` table by design (a buyer session is never admin). Only the SHA-256
-- hash of the opaque cx_admin_ token is stored; the raw token lives in an httpOnly
-- Secure cookie in the operator's browser and is unrecoverable from the DB.
CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash TEXT PRIMARY KEY,
    id         UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    admin_credential_id UUID NOT NULL REFERENCES admin_credentials(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked    BOOLEAN DEFAULT false
);
-- Sessions created before credential provenance existed cannot be attributed
-- honestly. Force one passkey re-login instead of guessing which credential
-- minted them, then make provenance mandatory for every new session.
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS id UUID DEFAULT gen_random_uuid();
UPDATE admin_sessions SET id = gen_random_uuid() WHERE id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS admin_sessions_id_uniq ON admin_sessions (id);
ALTER TABLE admin_sessions ALTER COLUMN id SET NOT NULL;
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS admin_credential_id UUID
    REFERENCES admin_credentials(id) ON DELETE RESTRICT;
DELETE FROM admin_sessions WHERE admin_credential_id IS NULL;
ALTER TABLE admin_sessions ALTER COLUMN admin_credential_id SET NOT NULL;
CREATE INDEX IF NOT EXISTS admin_sessions_credential_idx
    ON admin_sessions (admin_credential_id, expires_at);

-- Immutable operator intent. Actor ids identify the authenticating credential,
-- not a human: a passkey row is credential-level attribution and a break-glass
-- API key is explicitly shared-credential-only. Money facts below carry a unique
-- FK to one of these actions; retries return the original action and never rewrite
-- its actor or semantic digest.
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
 OR (actor_mode = 'passkey_session' AND actor_principal_id IS NOT NULL
     AND actor_session_id IS NOT NULL AND attribution_scope = 'credential_only')
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
-- Migration for DBs created before tokens were hashed (column was `token`, held the
-- raw value). Rename to token_hash; pre-launch there are no real tokens to migrate,
-- so re-seed / re-mint after deploy. Idempotent — only fires if the old column exists.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_name = 'worker_tokens' AND column_name = 'token') THEN
    ALTER TABLE worker_tokens RENAME COLUMN token TO token_hash;
  END IF;
END $$;

-- One-time, device-proofed worker enrollment. Existing manually minted/seeded
-- worker tokens remain valid but are visibly unbound (NULL device fields). New
-- exchange credentials have a stable public id for account-side revocation and
-- rotation; raw tokens/codes are still shown once and only their SHA-256 hashes
-- are retained.
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
-- Preserve rotation lineage as an immutable identifier. A foreign key with
-- ON DELETE SET NULL can silently turn a pending rotation ceremony into a fresh
-- enrollment after retention deletes its source credential.
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
-- Enrollment wire v2 binds the approval's trusted public origin and the exact
-- pending request id into both the stored row and the device-signed exchange
-- transcript. Rows created before this migration remain protocol_version=1 and
-- are deliberately ineligible for the v2 exchange rather than being silently
-- upgraded without the missing authenticated context.
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

-- Append-only lifecycle evidence. IDs intentionally remain plain values rather
-- than foreign keys: deleting operational credentials/codes for retention must
-- not rewrite or erase the historical audit trail.
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
-- The verification class the known_answer was produced under, as "engine|build_hash"
-- (or '' = unknown). For a BYTE-EXACT job type the verifier auto-quarantines on a
-- honeypot byte mismatch ONLY when the committing worker shares this class — a
-- class-blind ('' ) or cross-class byte honeypot is NOT grounds to quarantine an
-- honest worker whose engine/build legitimately produces different bytes (the audit's
-- "Candle-seeded answer would byte-fail a correct vLLM result" hazard). Tolerant job
-- types (embed/classification/json/rerank) compare semantics and ignore this column.
-- DEFAULT '' so existing/seed honeypots (class-blind) keep working — they simply stop
-- auto-quarantining cross-class byte mismatches, which is the safe behavior. Full
-- hw_class-aware honeypot seeding is the Wave-2 prerequisite (docs/DETERMINISM_CLASS.md).
ALTER TABLE honeypots ADD COLUMN IF NOT EXISTS answer_class TEXT NOT NULL DEFAULT '';
-- INJECTION-TIME PARAM/MODEL GUARD (byte-exact honeypot safety, docs/DETERMINISM_CLASS.md;
-- named REQUIRED before production-scale byte-exact seeding in seed.go's
-- demoHoneypotHawkKnownAnswer doc). A byte-exact honeypot's known answer is only
-- valid evidence for a job that runs the EXACT model + at least the max_tokens the
-- answer was captured under (the hawking seed: llama-3.2-1b-instruct-q4, every row
-- EOS'd strictly below max_tokens=24). Keying AvailableSeedHoneypots on job_type
-- ALONE would draw this probe for a batch_infer job on a DIFFERENT model, or with a
-- SMALLER max_tokens, where an HONEST same-class worker legitimately produces
-- different bytes and would be wrongly quarantined. These columns record the bounds
-- so the injection query can refuse to draw the probe for a job it is not byte-valid
-- for. NULLABLE by design: a NULL answer_model marks a tolerant-class (embed/etc.)
-- probe with no model/param bound — those keep the old job_type-only behavior.
ALTER TABLE honeypots ADD COLUMN IF NOT EXISTS answer_model          TEXT;  -- byte-exact seed's required model_ref (NULL = tolerant probe, no model bound)
ALTER TABLE honeypots ADD COLUMN IF NOT EXISTS answer_min_max_tokens INT;   -- byte-exact seed's minimum valid job max_tokens (NULL = no floor)

-- ─────────────────────────────────────────────────────────────────────────────
-- Webhooks + model catalogue
-- ─────────────────────────────────────────────────────────────────────────────
-- Completion-webhook registrations. Delivery is an at-least-once, leased outbox:
-- the worker claims due rows with SKIP LOCKED, POSTs one attempt, then either
-- stamps delivered_at or advances durable exponential backoff. Permanent failures
-- and exhausted retries are dead-lettered so a poison endpoint cannot occupy the
-- queue forever. Every row is one buyer-owned job delivery; account-wide
-- subscriptions require a separate subscription/fanout schema and are not faked.

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
-- Additive upgrades for already-created tables (idempotent).
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS delivered_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS lease_token UUID;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS dead_lettered_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS signing_secret_sealed TEXT;
-- A partial/legacy plaintext value is not a usable per-registration authority.
-- Normalize it to legacy NULL so the dead-letter migration below catches it.
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
-- Pre-signing rows cannot be authenticated by their receiver. Never emit one
-- unsigned; exact authenticated re-registration upgrades and re-arms the row.
UPDATE webhooks
   SET dead_lettered_at=now(),
       lease_token=NULL,
       lease_expires_at=NULL,
       last_error='legacy webhook has no per-registration signing secret; re-register it'
 WHERE delivered_at IS NULL
   AND dead_lettered_at IS NULL
   AND signing_secret_sealed IS NULL;
-- Pre-outbox releases accepted NULL-job "catch-all" rows, but no fanout model
-- existed and those rows could never fire. Remove inert/ownership-invalid legacy
-- rows before installing the honest one-row/one-job invariant.
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

-- Model + pricing catalogue — the DB-backed source of truth GET /v1/models and
-- /v1/price-estimate read live (control/api.go ListModels/GetModel; no static Go
-- list). price_per_1k is USD per 1,000 units (tokens/embeddings);
-- price_per_unit is USD per discrete unit (e.g. audio-minute for transcription).
-- kind matches the wire model_kind/runner domain (embed|gguf|whisper|hf|mlx).

CREATE TABLE IF NOT EXISTS models (
    id             TEXT PRIMARY KEY,
    family         TEXT,
    quant          TEXT,
    kind           TEXT,
    dim            INT,                 -- embedding dimensionality (embed models)
    job_type       TEXT,
    price_per_1k   NUMERIC(12,8),       -- USD / 1,000 units
    price_per_unit NUMERIC(12,8),       -- USD / discrete unit (e.g. audio-minute)
    min_memory_gb  REAL,
    hf_repo        TEXT                 -- HuggingFace repo to resolve weights from
);

-- Seed the V1 catalogue. ON CONFLICT DO NOTHING keeps this idempotent and lets
-- operators edit rows without a re-seed clobbering them.
-- hf_repo / model id values are the ones the Rust agent actually resolves
-- (agent/src/models.rs): MiniLM embeddings, an unsloth Llama-3.2-1B GGUF, and
-- whisper-tiny|base. Keep these in lockstep with the agent's resolver.
INSERT INTO models (id, family, quant, kind, dim, job_type, price_per_1k, price_per_unit, min_memory_gb, hf_repo) VALUES
    ('all-minilm-l6-v2', 'minilm', NULL,   'embed',   384,  'embed',            0.00100000, NULL,        2, 'sentence-transformers/all-MiniLM-L6-v2'),
    ('bge-small-en-v1.5', 'bge',   NULL,   'embed',   384,  'embed',            0.00100000, NULL,        2, 'BAAI/bge-small-en-v1.5'),
    ('llama-3.2-1b-instruct-q4', 'llama', 'q4_k_m', 'gguf', NULL, 'batch_infer', 0.00200000, NULL,        4, 'unsloth/Llama-3.2-1B-Instruct-GGUF'),
    ('qwen2.5-7b-instruct-q4', 'qwen', 'q4_k_m', 'gguf', NULL, 'batch_infer',    0.00800000, NULL,       40, 'bartowski/Qwen2.5-7B-Instruct-GGUF'),
    ('whisper-tiny',     'whisper', NULL,  'whisper', NULL,  'audio_transcribe', NULL,       0.00400000,  1, 'openai/whisper-tiny'),
    ('whisper-base',     'whisper', NULL,  'whisper', NULL,  'audio_transcribe', NULL,       0.00500000,  2, 'openai/whisper-base')
ON CONFLICT (id) DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- Scheduler V2 / Turbo additions (idempotent ALTERs).
-- ─────────────────────────────────────────────────────────────────────────────
-- The V1 claim filtered only on status/visibility/tier; per-job hardware/model
-- constraints lived only in the manifest JSON, so a worker could claim a task it
-- could not run. Scheduler V2 lifts those constraints into queryable columns and
-- hard-filters them in the SKIP-LOCKED claim (store.go ClaimTask), so a worker
-- whose STORED DECLARATION is incompatible cannot claim the task. This is an
-- admission/scheduling invariant, not proof that an untrusted worker's hardware,
-- model, precision, or runtime declaration is true; execution identity remains a
-- separate verification gate. These mirror JobConstraints + registration state.

-- Job-level queryable constraints (lifted out of verification_policy/manifest):
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS min_memory_gb      REAL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS max_duration_secs   BIGINT NOT NULL DEFAULT 0; -- 0 = runner default; otherwise buyer wall-clock ceiling
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_max_duration_secs_range;
ALTER TABLE jobs ADD CONSTRAINT jobs_max_duration_secs_range
    CHECK (max_duration_secs BETWEEN 0 AND 4294967295);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS min_reputation     REAL DEFAULT 0;    -- Elite-supplier gate (research §6.4): claim only by reputation >= this
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS private_pool        BOOLEAN DEFAULT false; -- Private Deployment (research §3): route only to the buyer's bound suppliers
CREATE TABLE IF NOT EXISTS private_pool_members (
    buyer_id    UUID NOT NULL,
    supplier_id UUID NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (buyer_id, supplier_id)
);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS hw_classes         TEXT[];           -- NULL = any class
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS data_residency     TEXT[];           -- NULL = unrestricted
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS split_size         INT;              -- adaptive chunk size chosen at submit
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS offered_rate_usd_hr REAL;            -- price-derived $/hr a worker earns running this (min-payout gate)
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS eta_secs           INT;              -- predicted completion seconds at submit
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_status      TEXT NOT NULL DEFAULT 'not_attempted'; -- not_attempted|charged|failed|no_payment_method|deferred (queryable charge state, not log-only).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS results_merged_at  TIMESTAMPTZ;      -- watermark: set when the buyer-ready artifact was last successfully merged, so GET /v1/jobs/{id}/results only re-merges once since completion instead of on every poll
-- 'deferred': the job settled BELOW the CX_CHARGE_MIN_USD batching threshold, so it is
-- deliberately not charged alone (a ~30¢ Stripe fixed fee on a sub-$5 charge is fee
-- bleed); the charge-collect sweep groups deferred jobs per buyer into charge_batches
-- and bills once the buyer's deferred sum crosses the threshold or the oldest deferred
-- job turns 24h old. The money stays honestly owed in the ledger the whole time.
-- Full submitted JobType (tag + variant fields: labels/schema/max_tokens/...), so
-- the poll dispatch can carry buyer params to the agent, not just the bare tag.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS job_type_spec      JSONB;

-- Plane C §12 / Plane D §14 D8 — Budget Governor. max_usd is the buyer's hard
-- spend cap: the SKIP-LOCKED claim refuses to dispatch a NEW task once the job's
-- projected charge (already-charged tasks + one more task's estimate) would breach
-- it, so a runaway is STOPPED before money is spent (the cap prevents dispatch, it
-- never refunds). budget_state is the buyer-visible governor state machine
-- (tracking|near_limit|paused_for_budget|cancelled_by_budget); NULL max_usd = no
-- cap (unchanged behavior for every existing job).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS max_usd            NUMERIC(12,6);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS budget_state       TEXT DEFAULT 'tracking';

-- Task ordering (buyer-ready merge in input order) + straggler-hedge lineage.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS chunk_index INT DEFAULT 0;          -- position within the job's input
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS hedged_from UUID;                   -- original task this is a hedge/tiebreak of
-- A third-opinion task is pinned to the immutable verification class that
-- created it. Worker profiles may change between planning, dispatch, and retry;
-- these columns prevent a retry from silently changing comparison class.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_hw_class TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_engine TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_build_hash TEXT;
-- Immutable identity/class of the current (or most recently claimed) execution
-- attempt. workers is mutable fleet state; verification and receipts must never
-- reconstruct historical provenance from a later registration.
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
-- The legacy-tiebreak backfill below may fall back to the anchor worker profile.
-- These columns historically lived in a later worker-capability section; add them
-- before the backfill as well so a genuinely fresh schema is valid in file order.
ALTER TABLE workers ADD COLUMN IF NOT EXISTS engine TEXT NOT NULL DEFAULT 'candle';
ALTER TABLE workers ADD COLUMN IF NOT EXISTS build_hash TEXT NOT NULL DEFAULT '';

-- Sparse third-opinion execution identity. Requeue paths deliberately clear the
-- current tasks.worker_id/claimed_by projection; this history preserves every
-- worker and supplier that started a tiebreak attempt so it can never be retried
-- by either disputant, a same-supplier machine, or an earlier failed peer.
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

-- Control Plane Hot Path 8->9 (docs/internal/CREED_AND_PATH_TO_TEN.md, "Get
-- result-commit off the S3 critical path"): the worker-reported SHA-256 (hex) of
-- its own committed result bytes, persisted at CommitTask. A later commit's
-- redundancy/honeypot comparison trusts a hash-to-hash match for byte-exact job
-- types instead of a second synchronous S3 GetObject inside the commit
-- transaction. NULL for an older agent that omits it (or a pre-migration row) —
-- the commit handler always falls back to a real GetObject when a hash is
-- missing, so correctness never depends on this column being populated.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS result_sha256 TEXT;

-- Verification-before-settlement state. A worker commit first moves the task to
-- `verifying`; only FinalizeTaskVerification may atomically stamp a durable verdict,
-- mark it complete, increment counters/telemetry, and insert money rows. These
-- columns are the current-attempt projection; task_verdicts below is append-only
-- history across retries.
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

-- Verdict rows are immutable facts about what happened at one attempt. Later
-- tiebreaks may promote a provisional winner or claw back a loser, but that is a
-- new resolution fact rather than a rewrite of history.
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

-- Durable per-attempt verification work. Worker-reported staging metadata is
-- immutable but is not artifact authority; a fenced lease pins the authoritative
-- server-observed artifact tuple exactly once before recording a terminal decision.
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
-- A repeat schema apply may be upgrading a legacy in-flight row while the guard
-- from the previous apply is already installed. Schema application is offline;
-- drop it for the scoped backfill and recreate it immediately below.
DROP TRIGGER IF EXISTS tasks_execution_identity_immutable ON tasks;
-- Backfill provenance only from immutable verification work. The sole exception
-- is an in-flight pre-deploy claim that has not committed yet: freeze its current
-- worker row once at migration so its eventual commit has an attempt identity.
-- Completed legacy rows without work remain explicitly unknown.
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
-- Upgrade safety for tiebreaks created by a pre-frozen-class control plane.
-- Use only the immutable attempt snapshot or claim-frozen execution tuple.
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

-- Worker capability, queryable (the agent already advertises these on register).
ALTER TABLE workers ADD COLUMN IF NOT EXISTS supported_jobs     TEXT[];        -- job_type tags this worker can run
ALTER TABLE workers ADD COLUMN IF NOT EXISTS supported_models   TEXT[];        -- model ids resident/runnable locally
ALTER TABLE workers ADD COLUMN IF NOT EXISTS min_payout_usd_hr  REAL DEFAULT 0;-- operator reservation price ($/hr)
ALTER TABLE workers ADD COLUMN IF NOT EXISTS thermal_ok         BOOLEAN DEFAULT true;
-- Durable ordinary-lane fairness: after three consecutive priority claims, an
-- eligible batch task receives one opportunity. Pinned verification work is
-- handled ahead of this lane and does not mutate the streak.
ALTER TABLE workers ADD COLUMN IF NOT EXISTS priority_claim_streak INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workers DROP CONSTRAINT IF EXISTS workers_priority_claim_streak_range;
ALTER TABLE workers ADD CONSTRAINT workers_priority_claim_streak_range
    CHECK (priority_claim_streak BETWEEN 0 AND 3);

-- Exact production capability authority. The registration wire contract still
-- carries independent supported_jobs/supported_models arrays, but those arrays are
-- declarations only: registration intersects them with the generated production
-- runtime matrix and atomically replaces these rows. Every scheduler, quote, routing,
-- and planner eligibility path requires an exact (worker, job, model, matrix hash)
-- row. There is intentionally NO migration/backfill from legacy arrays; an existing
-- worker remains inert until it re-registers against the current matrix.
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
-- Existing normalized rows predate generated model-kind binding. Do not infer or
-- backfill authority from the mutable model catalog: invalidate them and require
-- the worker's next real registration to persist a current generated exact row.
ALTER TABLE worker_authorized_capabilities ADD COLUMN IF NOT EXISTS model_kind TEXT;
DELETE FROM worker_authorized_capabilities
 WHERE COALESCE(model_kind,'') NOT IN ('gguf','hf','mlx');
ALTER TABLE worker_authorized_capabilities ALTER COLUMN model_kind SET NOT NULL;
ALTER TABLE worker_authorized_capabilities
    DROP CONSTRAINT IF EXISTS worker_authorized_capabilities_model_kind_valid;
ALTER TABLE worker_authorized_capabilities
    ADD CONSTRAINT worker_authorized_capabilities_model_kind_valid
    CHECK (model_kind IN ('gguf','hf','mlx'));
CREATE INDEX IF NOT EXISTS worker_authorized_capabilities_exact_idx
    ON worker_authorized_capabilities (worker_id, job_type, model_ref, matrix_sha256);
CREATE INDEX IF NOT EXISTS worker_authorized_capabilities_supply_idx
    ON worker_authorized_capabilities (job_type, model_ref, matrix_sha256, worker_id);

-- Exact execution authority frozen on the current task attempt. ClaimTask copies
-- these values, including the generated wire model kind, from
-- worker_authorized_capabilities in the same transaction that claims the task; a
-- worker never authors them. A retry may
-- replace the tuple for its new attempt. Receipts can therefore prove the precise
-- runtime cell/matrix that authorized the accepted result even after a worker later
-- re-registers against a different matrix.
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

-- The on-device inference ENGINE this worker runs (candle|mlx|vllm|hawking). It is
-- the SECOND axis of the verification class alongside hw_class: byte-exact redundancy
-- peers and honeypots are drawn from the same (hw_class, engine), because two engines'
-- FP kernels differ even on identical hardware, so a future mlx/vllm/hawking worker is
-- never byte-compared against a Candle one. DEFAULT 'candle' so every existing worker
-- row (and an older agent that does not advertise the field) keeps today's behavior —
-- a single-engine Candle fleet's (hw_class, engine) class collapses back to hw_class.
ALTER TABLE workers ADD COLUMN IF NOT EXISTS engine             TEXT NOT NULL DEFAULT 'candle';

-- The FINER axis of the verification class BELOW (hw_class, engine): a stable hash of
-- the byte-output-determining BUILD inputs (engine + agent build + device backend +
-- catalogue quant — agent hardware::engine_build_hash). Two workers in the same
-- hw_class + engine but on different agent builds (a kernel/codegen change between
-- releases) can emit different bytes even on identical hardware, so BYTE-EXACT
-- redundancy peers + honeypots are pinned to the same (hw_class, engine, build_hash);
-- a cross-build byte mismatch is NOT an auto-dock — it falls back to provisional trust
-- (the missing-third-worker pattern). DEFAULT '' = "unknown build": an older agent that
-- does not advertise it is never drawn as a byte-exact peer and never auto-docked, so a
-- single-build fleet that all reports the same hash collapses the class to today's
-- behavior. See docs/DETERMINISM_CLASS.md.
ALTER TABLE workers ADD COLUMN IF NOT EXISTS build_hash         TEXT NOT NULL DEFAULT '';

-- Dynamic-throttling resource state, refreshed on every heartbeat (agent reads
-- REAL available memory each cycle). The SKIP-LOCKED claim filters on these so a
-- worker is never handed a task it cannot SAFELY run: effective_memory_gb is the
-- allocatable pool AFTER the supplier's reserved headroom, and `throttled` is the
-- agent pausing for memory pressure. effective_memory_gb stays NULL until the
-- first heartbeat, so the claim falls back to total memory_gb (no regression for
-- a just-registered worker); `throttled` defaults false (claimable).
ALTER TABLE workers ADD COLUMN IF NOT EXISTS effective_memory_gb REAL;          -- allocatable for jobs = available − headroom (NULL → fall back to memory_gb)
ALTER TABLE workers ADD COLUMN IF NOT EXISTS available_memory_gb REAL;          -- live free + reclaimable memory (GB)
ALTER TABLE workers ADD COLUMN IF NOT EXISTS reserved_headroom_gb REAL;         -- GB the operator reserves for their own use
ALTER TABLE workers ADD COLUMN IF NOT EXISTS throttled          BOOLEAN DEFAULT false; -- agent currently pausing new claims (memory pressure)

-- Supplier jurisdiction (data-residency match) + quarantine timestamp.
ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS data_country   TEXT;            -- ISO country the supplier operates in
ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS quarantined_at TIMESTAMPTZ;     -- set by auto-quarantine (Verification V2)

-- Stripe Connect payout readiness, flipped by the account.updated webhook
-- (control/suppliers.go). A supplier can only be PAID once Stripe says its
-- connected account can receive transfers; this column is the cached view the
-- status endpoint reads without a live Stripe call. Default false (not yet able).
ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS payouts_enabled BOOLEAN DEFAULT false;

-- ─────────────────────────────────────────────────────────────────────────────
-- Plane C / Compute Autopilot (docs/PLANE_C.md) — quote intelligence.
-- ─────────────────────────────────────────────────────────────────────────────
-- A quote is the central Plane C object: what the system BELIEVED about a job
-- before the buyer spent money (cost band, ETA band, eligible supply, OOM risk,
-- the input scan). Persisting the assumptions is the load-bearing rule (PLANE_C
-- §6): a later invoice can say what was believed at quote time, and quote-to-actual
-- drift can be measured. The full structured quote is kept in quote_json; the
-- scalar columns make assumptions queryable (admin drift views). The normalized
-- quote_inputs / quote_supply_snapshot tables (PLANE_C §6) are folded into
-- quote_json for the MVP and can be split out later without a data migration.

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
    confidence         REAL,             -- 0.0–1.0
    quote_json         JSONB             -- the full quote object returned to the buyer
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Plane C errata / Plane D D0 (docs/PLANE_C_ERRATA.md, docs/PLANE_D.md §6) —
-- immediate typed failure + buyer-visible event timeline.
-- ─────────────────────────────────────────────────────────────────────────────
-- task_failures is the structural fix for silent OOM + money drain: when a worker
-- KNOWS a task cannot complete it reports a typed failure (POST /v1/worker/task/
-- {id}/fail) instead of stranding it for the 30-min stale reaper. failure_class is
-- the shared taxonomy (control/failure.go + agent/src/failure.rs); retryable +
-- buyer_fault drive immediate-requeue vs terminal-refund. memory is the agent's
-- snapshot at failure (real, never faked) for OOM diagnosis + quote-risk feedback.

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

-- job_events is the append-only buyer-visible timeline (PLANE_C §16, errata C-3):
-- the buyer should not infer state from status fields alone. event is the shared
-- enum (job_created|task_failed|task_requeued|job_failed|...); buyer_text is a
-- safe-to-show summary, detail holds operator context (ids/class).
--
-- job_events is a declarative RANGE-partitioned table (Postgres Data Lifecycle 6->7) —
-- its parent + partitions are created together with the other two telemetry tables by
-- cx_partition_telemetry() further down (all three must be created after their column
-- shapes are declared, so the definition lives there, not here).

-- ─────────────────────────────────────────────────────────────────────────────
-- Indexes
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS tasks_job_status_idx     ON tasks (job_id, status);
CREATE INDEX IF NOT EXISTS tasks_worker_status_idx  ON tasks (worker_id, status);
CREATE INDEX IF NOT EXISTS tasks_status_visible_idx ON tasks (status, visible_at);  -- queue claim path
CREATE INDEX IF NOT EXISTS tasks_ready_unclaimed_idx ON tasks (status, (COALESCE(visible_at, created_at)), created_at)
    WHERE claimed_by IS NULL AND status IN ('queued','retrying');  -- hot SKIP-LOCKED claim path
CREATE INDEX IF NOT EXISTS ledger_supplier_payout_idx ON ledger_entries (supplier_id, payout_status);
CREATE INDEX IF NOT EXISTS ledger_kind_idx             ON ledger_entries (kind);  -- reconcile/audit sums by kind
CREATE INDEX IF NOT EXISTS workers_hwclass_seen_idx  ON workers (hw_class, last_seen_at);
-- latest-benchmark lookup for the claim's throughput tiebreak (worker × job_type, newest first)
CREATE INDEX IF NOT EXISTS benchmark_worker_type_time_idx ON benchmark_results (worker_id, job_type, measured_at DESC);
CREATE INDEX IF NOT EXISTS workers_class_engine_seen_idx ON workers (hw_class, engine, build_hash, last_seen_at);  -- (hw_class, engine, build_hash) redundancy-peer class lookups
CREATE INDEX IF NOT EXISTS webhooks_job_idx          ON webhooks (job_id);
CREATE INDEX IF NOT EXISTS webhooks_delivery_due_idx ON webhooks (next_attempt_at,created_at,id)
    WHERE delivered_at IS NULL AND dead_lettered_at IS NULL;
-- Duplicate registrations do not mean duplicate customer events. Prefer an
-- already-delivered row so applying this migration cannot resurrect an event.
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
-- job_events_job_idx is created by cx_partition_telemetry() alongside the partitioned
-- job_events parent (it cannot be created here — job_events does not exist yet at this
-- point in the file on a fresh DB, and the partitioned parent propagates the index to
-- every leaf).

-- disputes: the buyer-dispute record — the anti-defection / optimistic-verification
-- primitive ROADMAP_STATUS flags as missing ("a buyer-dispute mechanism — none exists
-- today"). A buyer files a dispute on a completed job's result; RESOLUTION by optimistic
-- recompute (Verde-style operator bisection / TAO tolerance-aware FP verification —
-- docs/PRODUCTION_AUDIT.md §2.3) is the frontier seam: recorded here, not yet auto-resolved.
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

-- verification_events is the append-only RECEIPT log: the verification machinery
-- (control/verification.go) already applies reputation deltas on every outcome, but
-- the OUTCOMES themselves were not persisted, so a buyer could not see what was
-- checked. Each row is one verification fact emitted co-located with its reputation
-- dock (honeypot pass/fail, redundancy match/mismatch, tiebreak win/loss). Writes are
-- best-effort and NEVER block the verify/money path; the aggregate is grouped by
-- job_id for the buyer-facing job-status `verification` block. kind is the closed set
-- {honeypot_pass|honeypot_fail|redundancy_match|redundancy_mismatch|tiebreak_win|tiebreak_loss}.
CREATE TABLE IF NOT EXISTS verification_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      UUID NOT NULL REFERENCES jobs,
    task_id     UUID,
    supplier_id UUID,
    kind        TEXT NOT NULL,  -- honeypot_pass|honeypot_fail|redundancy_match|redundancy_mismatch|tiebreak_win|tiebreak_loss|redundancy_cross_class|tiebreak_cross_class|redundancy_same_supplier
                                -- *_cross_class: a byte-exact comparison was skipped because the peer was in a DIFFERENT
                                -- verification class (engine/build_hash) — recorded for forensics, NOT counted as "checked".
                                -- redundancy_same_supplier: the only agreeing peer shared the committing supplier, so the
                                -- match was NOT counted as independent (no redundancy_match credit). The task still
                                -- succeeds; it is simply not independently verified (backlog P0 items 6-7).
    created_at  TIMESTAMPTZ DEFAULT now()
);
ALTER TABLE verification_events ADD COLUMN IF NOT EXISTS effect_id UUID;
ALTER TABLE verification_events ADD COLUMN IF NOT EXISTS attempt SMALLINT;
CREATE INDEX IF NOT EXISTS verification_events_job_idx ON verification_events (job_id, created_at);
DROP INDEX IF EXISTS verification_events_effect_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS verification_events_effect_uniq
    ON verification_events (effect_id);
-- Retry-safe durable facts: one task can emit a given verification outcome once.
-- Rows with NULL task_id (rare operator/job-level facts) remain unconstrained.
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
-- reverify_task_id links a dispute to the independent re-run dispatched to resolve it
-- (status flow: open|no_peer -> reverifying -> resolved|rejected|unresolvable).
ALTER TABLE disputes ADD COLUMN IF NOT EXISTS reverify_task_id UUID;
CREATE INDEX IF NOT EXISTS task_failures_job_idx      ON task_failures (job_id, created_at DESC); -- failure drill-down by job

-- ─────────────────────────────────────────────────────────────────────────────
-- Plane D D7 / Plane C errata C-Errata-4 (docs/PLANE_D.md §13) — quote-to-submit
-- binding. An advisory quote can be bound to the submission that acts on it, so a
-- later invoice can say "here is what you were told". expires_at bounds how long a
-- quote stays bindable (15 min); input_sha256 lets the submit path best-effort
-- confirm the bytes match what was quoted; jobs.quote_id records the binding.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS expires_at    TIMESTAMPTZ;             -- quote stops being bindable after this (now()+15m at insert)
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS input_sha256  TEXT;                    -- sha256 of the scanned input bytes, for best-effort submit match
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS quote_id      UUID;                    -- the advisory quote this job was bound to (NULL = none)
CREATE INDEX IF NOT EXISTS jobs_quote_idx ON jobs (quote_id) WHERE quote_id IS NOT NULL;  -- quote→job lookups (invoice/admin drift)

-- ─────────────────────────────────────────────────────────────────────────────
-- Plane D D6 / Plane C errata C-Errata-6 (docs/PLANE_D.md §12) — quote-to-actual
-- drift feedback. The quote's ETA is only as good as the static throughput target
-- until we measure reality; task_durations records the REAL per-task duration of
-- every COMMITTED task (malformed/failed tasks never write a row, so they cannot
-- poison the estimate) so the Exchange Brain can learn an observed p90 and the next
-- quote's ETA leans on it instead of the target. jobs.eta_secs (above) already holds
-- the quoted ETA — it is REUSED as the quoted side of the drift rollup, not duplicated.
-- ─────────────────────────────────────────────────────────────────────────────
-- Postgres Data Lifecycle 6->7 (docs/internal/CREED_AND_PATH_TO_TEN.md): task_durations,
-- worker_memory_samples and job_events are declarative RANGE-PARTITIONED tables (monthly
-- partitions on created_at) so expired history is dropped by an O(1) DROP PARTITION
-- instead of an O(rows) DELETE competing with autovacuum. All three are created together
-- by cx_partition_telemetry() below (after all three column shapes are declared), because
-- a partitioned parent needs its DEFAULT + month partitions created in the same breath to
-- be insertable. See that function for the full rationale (composite PK, NOT NULL
-- created_at, leaf-level autovacuum, fresh-vs-existing-DB convergence with the Go
-- migration control/partition.go).

-- ─────────────────────────────────────────────────────────────────────────────
-- Plane D D4 (docs/PLANE_D.md §10) — memory telemetry persistence + quote risk.
-- ─────────────────────────────────────────────────────────────────────────────
-- The heartbeat already carries the worker's live available/effective memory and
-- whether it is throttled (workers.* columns hold only the LATEST beat, which the
-- claim filter reads). worker_memory_samples appends a ROLLING sample each beat so
-- the system has a memory-pressure history, not just a snapshot: GET /admin/capacity
-- reports recent avg available/effective per worker, and quote risk (quote.go
-- assessRisk → applyMemoryFloorRisk) compares a model's memory floor against the
-- MEDIAN effective memory of eligible workers (from these samples) to bump oom_risk
-- when the floor is tight. All real telemetry — every value is the agent's reported
-- number, never faked. Retention is enforced by monthly partitioning (6->7 below) plus
-- the hourly DELETE sweep that trims the sub-month tail.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- Postgres Data Lifecycle 6->7 (docs/internal/CREED_AND_PATH_TO_TEN.md) — the three
-- telemetry tables, created as declarative RANGE-partitioned tables.
-- ─────────────────────────────────────────────────────────────────────────────
-- cx_partition_telemetry(table, columns, index_name, index_body, autovacuum) creates one
-- partitioned parent with a composite (created_at, id) PK, its secondary index, a DEFAULT
-- catch-all partition, and month partitions spanning [now()-1 month, now()+2 months] — all
-- with leaf-level autovacuum params (a partitioned PARENT cannot carry storage params, so
-- they go on every leaf). It is a COMPLETE NO-OP when a relation of that name already
-- exists in ANY form (plain or partitioned): a fresh DB is born partitioned here; an
-- existing plain-table DB is left untouched for the Go migration
-- (control/partition.go MigrateTelemetryPartitions) to convert IN PLACE, preserving every
-- row. Both paths converge to the identical shape, and the rotation job
-- (control/workers.go rotateTelemetryPartitions) keeps months current thereafter.
--
-- The composite PK (created_at, id) is mandatory (a partition-key column must be in every
-- unique constraint); no reader looks these rows up by id and no FK references them, so
-- gen_random_uuid()'s collision-freeness preserves effective global id-uniqueness.
-- created_at is NOT NULL because a RANGE partition key cannot be NULL — every existing row
-- already has a non-NULL created_at (it has defaulted to now() since creation).
CREATE OR REPLACE FUNCTION cx_partition_telemetry(
    p_table text, p_columns text, p_index_name text, p_index_body text, p_autovacuum text
) RETURNS void LANGUAGE plpgsql AS $cx$
DECLARE
    m date; lo date; hi date; part text; nextm date;
BEGIN
    -- No-op if the relation already exists in any form (idempotent + non-destructive).
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = p_table AND relnamespace = 'public'::regnamespace) THEN
        RETURN;
    END IF;
    EXECUTE format('CREATE TABLE %I (%s, PRIMARY KEY (created_at, id)) PARTITION BY RANGE (created_at)', p_table, p_columns);
    EXECUTE format('CREATE INDEX %I ON %I %s', p_index_name, p_table, p_index_body);
    EXECUTE format('CREATE TABLE %I PARTITION OF %I DEFAULT WITH (%s)', p_table || '_default', p_table, p_autovacuum);
    -- Pin both the calendar calculation and timestamptz bounds to UTC. A database
    -- session may use any TimeZone; session-local midnight would disagree with the
    -- Go rotation job's UTC month names/bounds and route boundary rows to DEFAULT.
    lo := date_trunc('month', (now() AT TIME ZONE 'UTC') - interval '1 month')::date;   -- one back so a just-inserted row near a month edge has a home
    hi := date_trunc('month', (now() AT TIME ZONE 'UTC') + interval '2 months')::date;  -- create-ahead headroom (matches partitionCreateAheadMonths)
    m := lo;
    WHILE m <= hi LOOP
        nextm := (m + interval '1 month')::date;
        part := p_table || '_p' || to_char(m, 'YYYY_MM');
        EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L) WITH (%s)',
                       part, p_table, m::timestamp AT TIME ZONE 'UTC',
                       nextm::timestamp AT TIME ZONE 'UTC', p_autovacuum);
        m := nextm;
    END LOOP;
END;
$cx$;

SELECT cx_partition_telemetry(
    'worker_memory_samples',
    $c$id           UUID NOT NULL DEFAULT gen_random_uuid(),
       worker_id    UUID,
       available_gb REAL,
       effective_gb REAL,
       throttled    BOOLEAN,
       created_at   TIMESTAMPTZ NOT NULL DEFAULT now()$c$,
    'worker_memory_samples_worker_time_idx', '(worker_id, created_at DESC)',
    'autovacuum_vacuum_scale_factor=0.02, autovacuum_vacuum_threshold=200, autovacuum_analyze_scale_factor=0.02, autovacuum_analyze_threshold=200, autovacuum_vacuum_cost_limit=1000'
);
SELECT cx_partition_telemetry(
    'task_durations',
    $c$id          UUID NOT NULL DEFAULT gen_random_uuid(),
       created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
       job_id      UUID,
       job_type    TEXT,
       model_ref   TEXT,
       split_size  INT,
       duration_ms BIGINT,
       worker_id   UUID,
       engine      TEXT,
       build_hash  TEXT,
       task_id     UUID$c$,
    'task_durations_type_model_idx', '(job_type, model_ref)',
    'autovacuum_vacuum_scale_factor=0.05, autovacuum_vacuum_threshold=100, autovacuum_analyze_scale_factor=0.05, autovacuum_analyze_threshold=100, autovacuum_vacuum_cost_limit=500'
);
ALTER TABLE task_durations ADD COLUMN IF NOT EXISTS task_id UUID;
SELECT cx_partition_telemetry(
    'job_events',
    $c$id          UUID NOT NULL DEFAULT gen_random_uuid(),
       created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
       job_id      UUID NOT NULL,
       task_id     UUID,
       event       TEXT NOT NULL,
       buyer_text  TEXT,
       detail      JSONB$c$,
    'job_events_job_idx', '(job_id, created_at)',
    'autovacuum_vacuum_scale_factor=0.1, autovacuum_vacuum_threshold=100, autovacuum_analyze_scale_factor=0.1, autovacuum_analyze_threshold=100, autovacuum_vacuum_cost_limit=500'
);

-- Postgres Data Lifecycle 5→6 (docs/internal/CREED_AND_PATH_TO_TEN.md): autovacuum
-- tuning for the telemetry tables sweepTelemetryRetention (control/workers.go
-- telemetryTables) now DELETEs from on an hourly ticker. Bounding the tables (4→5)
-- fixed unbounded growth but introduced a NEW churn shape the DB defaults were never
-- tuned for: instead of a slow trickle of dead tuples from scattered UPDATEs (what
-- the 0.2 default scale factor assumes), each table now takes one large DELETE burst
-- per hour, all at once. On a big table 20%-of-table-since-last-vacuum can be a huge
-- absolute number of dead tuples sitting unvacuumed between the hourly bursts,
-- bloating both the table and worker_memory_samples_worker_time_idx /
-- task_durations_type_model_idx / job_events_job_idx (which every real read of these
-- tables goes through — ListWorkers' recent-N-per-worker LATERAL, the p90/drift
-- rollup, and the buyer-facing GET /v1/jobs/{id}/events timeline). Same discipline as
-- the `tasks` table's own E3 tuning above: small scale factors with flat thresholds
-- so autovacuum fires on absolute churn instead of waiting for a fraction of the
-- table, sized per table to its actual insert-vs-delete pattern:
--
--   worker_memory_samples — the hottest of the three. One row per worker per
--   heartbeat-with-memory-reporting (control/store.go InsertWorkerMemorySample) is
--   the highest insert rate of any telemetry table (~2,880 rows/worker/day, per the
--   facet's own sizing note above), and the 14-day retention window is the
--   shortest, so the hourly sweep's delete burst is proportionally the largest
--   fraction of the table each cycle. Tuned as aggressively as `tasks` itself.
--
--   task_durations — one row per COMMITTED task (not per heartbeat), so a lower
--   insert rate than worker_memory_samples, but the 30-day window means the hourly
--   delete burst still removes a meaningful slice of the table each cycle at any
--   real task volume. Tuned between `tasks` and job_events: still low, not as low
--   as the heartbeat table.
--
--   job_events — a handful of rows per job/task (creation, failures, requeues), the
--   lowest insert rate of the three, AND the 180-day retention window means any
--   single hourly delete burst is a much smaller fraction of total table size than
--   the other two. Still tuned below the 0.2 default (it is still hourly-delete
--   churn, not cold storage), but the least aggressive of the three.
--
-- Idempotent and table-scoped · no global GUC change. Since 6->7 these tables are
-- PARTITIONED, and a partitioned PARENT rejects storage parameters (Postgres requires
-- them on the leaves), so the tuning is applied per-leaf by cx_partition_telemetry()
-- and the rotation job at partition-creation time — NOT on the parent here. This block
-- therefore applies the params ONLY when the table is still a PLAIN table (relkind 'r'):
-- on a fresh partitioned DB it is a no-op (relkind 'p'); on an older DB whose telemetry
-- tables are still plain (pre-6->7, awaiting the Go in-place conversion) it keeps them
-- tuned exactly as before, and the Go migration then carries the same params onto every
-- leaf when it converts. cx_apply_plain_autovacuum() encapsulates that relkind guard.
CREATE OR REPLACE FUNCTION cx_apply_plain_autovacuum(p_table text, p_params text)
RETURNS void LANGUAGE plpgsql AS $cx$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = p_table
                 AND relnamespace = 'public'::regnamespace AND relkind = 'r') THEN
        EXECUTE format('ALTER TABLE %I SET (%s)', p_table, p_params);
    END IF;
END;
$cx$;
SELECT cx_apply_plain_autovacuum('worker_memory_samples',
    'autovacuum_vacuum_scale_factor=0.02, autovacuum_vacuum_threshold=200, autovacuum_analyze_scale_factor=0.02, autovacuum_analyze_threshold=200, autovacuum_vacuum_cost_limit=1000');
SELECT cx_apply_plain_autovacuum('task_durations',
    'autovacuum_vacuum_scale_factor=0.05, autovacuum_vacuum_threshold=100, autovacuum_analyze_scale_factor=0.05, autovacuum_analyze_threshold=100, autovacuum_vacuum_cost_limit=500');
SELECT cx_apply_plain_autovacuum('job_events',
    'autovacuum_vacuum_scale_factor=0.1, autovacuum_vacuum_threshold=100, autovacuum_analyze_scale_factor=0.1, autovacuum_analyze_threshold=100, autovacuum_vacuum_cost_limit=500');

-- ─────────────────────────────────────────────────────────────────────────────
-- Plane D D3 (docs/PLANE_D.md §9) — warm-model state + routing preference.
-- ─────────────────────────────────────────────────────────────────────────────
-- The heartbeat carries loaded_models: the ids of models currently WARM in the
-- agent's pool. HeartbeatWorker upserts one row here per warm id (last_seen_warm =
-- now()), so the control plane knows which (worker, model) pairs avoid a cold model
-- load right now. The scheduler reads this as a SMALL re-rank bonus: a worker with
-- the job's model already warm sorts ahead of an otherwise-equal cold worker (the
-- fastest task avoids a load). It NEVER overrides the claim's hard filter — warm
-- only re-ranks fit/throttle-eligible workers. Rows are cheap and self-refreshing;
-- staleness is read against last_seen_warm (a worker that stops reporting a model
-- ages out), so no separate eviction is required. All real telemetry — a row exists
-- only because the agent reported that id warm, never fabricated.
CREATE TABLE IF NOT EXISTS worker_model_state (
    worker_id      UUID NOT NULL,
    model_id       TEXT NOT NULL,
    last_seen_warm TIMESTAMPTZ DEFAULT now(),  -- last heartbeat that reported this model warm
    PRIMARY KEY (worker_id, model_id)
);
CREATE INDEX IF NOT EXISTS worker_model_state_model_idx
    ON worker_model_state (model_id, last_seen_warm DESC);  -- "which live workers have THIS model warm" (scheduler + quote)

-- worker_tps_cache maintains ClaimTask's throughput tiebreak (Control Plane Hot
-- Path 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md "Get the correlated-subquery
-- cost out of the transactional hot path"): the claim CTE used to run a fresh
-- correlated subquery (SELECT br.tps FROM benchmark_results ... ORDER BY
-- measured_at DESC LIMIT 1) for EVERY eligible candidate row, on EVERY single
-- claim — an O(candidate rows) cost paid on the hottest transactional path for a
-- number that only actually changes once per real worker state change (a fresh
-- benchmark report). This table is upserted once, in UpsertWorker's own
-- transaction, exactly when a new benchmark_results row lands (mirrors
-- worker_model_state's "maintained on write, read as O(1) on the claim path"
-- shape). ClaimTaskSQL now LEFT JOINs this table per (worker, job_type) instead
-- of running the correlated subquery — a plain indexed lookup, not a per-row scan.
CREATE TABLE IF NOT EXISTS worker_tps_cache (
    worker_id  UUID NOT NULL,
    job_type   TEXT NOT NULL,
    tps        REAL NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (worker_id, job_type)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Stuck-run watchdog V2 (control/workers.go reapStuckJobs) — escalation ladder,
-- buyer deadline policy, and the ETA calibration loop.
-- ─────────────────────────────────────────────────────────────────────────────
-- watchdog_strikes is the escalation state: 0 = never judged stuck; the first
-- stuck verdict RESCUES (unfinished tasks requeued to a different machine) and
-- sets it to 1; a second verdict KILLS (checkpoint + cancel + settle). Guarded
-- transitions (WHERE status='running' AND watchdog_strikes=0) keep concurrent
-- sweeps idempotent.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS watchdog_strikes INT NOT NULL DEFAULT 0;
-- deadline_secs is the buyer's watchdog policy knob (POST /v1/jobs):
--   NULL / 0 → default behavior (ETA-derived deadline with floor + 24h cap),
--   -1       → opt OUT of the watchdog entirely (run to completion),
--   60..604800 → an explicit wall-clock deadline (1 minute to 7 days).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS deadline_secs INT;
-- eta_calibration is the feedback loop that tunes the watchdog's ETA factor:
-- one row per finalized job with an ETA prediction, pairing what was PREDICTED
-- at submit (jobs.eta_secs) with what was REALIZED (seconds from created_at to
-- finalize). Real observations only — a job with no prediction inserts nothing.
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
-- One calibration row per job, enforced structurally: the two finalize sites (the
-- commit-path finalize and the webhook sweep) can race, and INSERT..WHERE NOT EXISTS
-- is not atomic under READ COMMITTED — without this index both could insert,
-- duplicating rows and double-counting the near-miss metric.
CREATE UNIQUE INDEX IF NOT EXISTS eta_calibration_job_uniq ON eta_calibration (job_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- Charge batching + Stripe fee truth (control/collect.go, control/billing.go).
-- ─────────────────────────────────────────────────────────────────────────────
-- A charge batch is ONE PaymentIntent covering many small ('deferred') jobs of one
-- buyer, formed by the charge-collect sweep once the buyer's deferred sum crosses
-- CX_CHARGE_MIN_USD or the oldest deferred job turns 24h old. amount_usd is FROZEN
-- at formation. Before Stripe is called, buyer_charge_operations permanently
-- arms the request and moves the batch to outcome_unknown; ambiguous work is
-- reconciled from Stripe evidence rather than re-sent after an idempotency window.
-- status: attempting | outcome_unknown | charged.
CREATE TABLE IF NOT EXISTS charge_batches (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id   UUID NOT NULL,
    amount_usd NUMERIC(10,6) NOT NULL,       -- FROZEN sum of the member jobs at formation
    status     TEXT NOT NULL DEFAULT 'attempting',  -- attempting|charged
    stripe_pi  TEXT,                          -- the PaymentIntent id, once confirmed
    created_at TIMESTAMPTZ DEFAULT now(),
    charged_at TIMESTAMPTZ
);
-- Money-truth hardening (adversarial-review fixes):
--   deferred_at        · when the job entered 'deferred'; the 24h batching age counts
--                        from here, not from job creation (long-queued jobs keep their
--                        full accumulation window).
--   charge_attempt_usd · the FROZEN amount of the first single-job charge attempt;
--                        every retry replays the same (idempotency key, amount) pair,
--                        immune to a later actual_usd re-settle.
--   charge_batches.attempts/next_at · failed-batch backoff (30min x attempts <= 6h),
--                        so a hard-declined card is not retried once a minute forever.
--   charge_batches.amount_usd widened to NUMERIC(12,6): an unbounded re-armed debt
--                        must overflow at $999,999, not at $9,999 (formation is also
--                        capped at 500 member jobs per batch).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS deferred_at TIMESTAMPTZ;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_attempt_usd NUMERIC(10,6);
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS next_at TIMESTAMPTZ;
ALTER TABLE charge_batches ALTER COLUMN amount_usd TYPE NUMERIC(12,6);

CREATE INDEX IF NOT EXISTS charge_batches_status_idx ON charge_batches (status, created_at);
-- charge_batch_id stamps a deferred job into exactly one batch (stamped WHERE
-- charge_status='deferred' AND charge_batch_id IS NULL, so a concurrent sweep can
-- never double-batch a job). charge_attempts / charge_next_at back off the retry of
-- FAILED single-job charges (30min × attempts, capped at 6h) so a dead card is not
-- hammered every sweep; the amount stays owed in the ledger regardless.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_batch_id UUID;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_next_at  TIMESTAMPTZ;
-- The PaymentIntent id of a successfully charged SINGLE job (batches carry theirs on
-- charge_batches.stripe_pi). Needed by the stripe_fee backfill scan: a charge whose
-- fee fetch failed is found by "charged with a pi, no stripe_fee ledger row" and the
-- real fee is fetched again next sweep. NULL for jobs charged before this column
-- existed (their fee is honestly unknown, never estimated).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS stripe_pi TEXT;
CREATE INDEX IF NOT EXISTS jobs_charge_status_idx ON jobs (charge_status);

-- Exact outbound charge request boundary. The row is committed before contacting
-- Stripe and starts outcome_unknown: only the atomic canonical-cash commit can
-- advance it to succeeded. An old idempotency key is therefore never blindly
-- reused after a crash or confirmation-write failure.
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
-- One stripe_fee row per PaymentIntent, structurally: the fee recorder is
-- INSERT-if-absent by payout_ref, and this partial unique index makes a racing
-- double-insert impossible rather than merely unlikely.
CREATE UNIQUE INDEX IF NOT EXISTS ledger_stripe_fee_ref_uniq ON ledger_entries (payout_ref) WHERE kind = 'stripe_fee';

-- ─────────────────────────────────────────────────────────────────────────
-- Independent per-job economic facts (control/economic_facts.go).
-- ────────────────────────────────────────────────────────────────────────
-- The streaming submit and artifact merge paths persist these exact counters for
-- every new job. They remain nullable so legacy/pre-migration jobs honestly report
-- unknown rather than zero. A writer persists a source label alongside a counter;
-- the facts recomputer never reconstructs bytes/records from quote estimates or
-- object-key naming conventions.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_records  BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_bytes    BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_source   TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_records BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_bytes   BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_source  TEXT;

-- One current, reproducible projection per job. Monetary values which cannot be
-- attributed exactly (most importantly a multi-job Stripe batch fee) stay NULL.
-- processor_fee_payment_intent_total_usd still preserves the real Stripe total so
-- the report can say "known batch fee, unresolved per-job allocation" instead of
-- guessing pro-rata. jobs.actual_usd is retained only as explicitly-labelled
-- quote-derived settlement; it is never called execution cost.
CREATE TABLE IF NOT EXISTS job_economic_facts (
    job_id          UUID PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    buyer_id        UUID NOT NULL,
    job_status      TEXT NOT NULL,
    charge_status   TEXT NOT NULL,
    schema_version  SMALLINT NOT NULL DEFAULT 1,
    reconciliation_state TEXT NOT NULL DEFAULT 'pending'
        CHECK (reconciliation_state IN (
            'pending', 'awaiting_collection', 'awaiting_processor_fee',
            'unresolved_batch_fee', 'incomplete', 'complete'
        )),
    missing_data_reasons JSONB NOT NULL DEFAULT '[]'::jsonb,

    input_records       BIGINT,
    input_bytes         BIGINT,
    input_units_source  TEXT,
    output_records      BIGINT,
    output_bytes        BIGINT,
    output_units_source TEXT,
    control_plane_elapsed_ms     BIGINT,
    control_plane_elapsed_source TEXT,

    primary_tasks_run                   INT NOT NULL DEFAULT 0,
    verification_tasks_run              INT NOT NULL DEFAULT 0,
    retry_attempts                      INT NOT NULL DEFAULT 0,
    verdict_attempts                    INT NOT NULL DEFAULT 0,
    verification_task_server_ms         BIGINT,
    verification_tasks_with_server_ms   INT NOT NULL DEFAULT 0,
    verification_work_source            TEXT NOT NULL,
    worker_reported_tokens               BIGINT,
    worker_reported_tokens_tasks         INT NOT NULL DEFAULT 0,
    worker_reported_tokens_source        TEXT,

    settlement_usd       NUMERIC(12,6),
    settlement_usd_basis TEXT NOT NULL,
    supplier_liability_usd   NUMERIC(12,6),
    supplier_liability_basis TEXT NOT NULL,
    refunds_usd       NUMERIC(12,6),
    refunds_basis     TEXT NOT NULL,
    billed_usd        NUMERIC(12,6),
    billed_usd_basis  TEXT NOT NULL,
    processor_fee_payment_intent           TEXT,
    processor_fee_payment_intent_total_usd NUMERIC(12,6),
    processor_fee_usd                      NUMERIC(12,6),
    processor_fee_basis                    TEXT,
    contribution_margin_usd                NUMERIC(12,6),

    recomputed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS job_economic_facts_state_idx
    ON job_economic_facts (reconciliation_state, recomputed_at DESC);

-- Exact per-job allocation of a REAL Stripe fee for one multi-job charge batch.
-- The allocator works in integer micro-dollars over the batch's frozen billed_usd
-- weights, in stable (job.created_at, job.id) order; every row except the final
-- row receives its proportional floor and the final row receives the exact
-- remainder. Consequently SUM(allocated_fee_usd) equals the Stripe balance-
-- transaction fee exactly, without floating-point/pro-rata drift.
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

-- ─────────────────────────────────────────────────────────────────────────
-- Fail-closed quote/submit economic plans and bounded dynamic-work reserve.
-- ─────────────────────────────────────────────────────────────────────────
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

-- One immutable admission snapshot per job. No UPDATE is permitted: settlement
-- and dynamic dispatch read the frozen scalar columns, while the complete JSON
-- preserves every scenario/assumption the quote and submit evaluated. Deleting a
-- parent job may still cascade-delete its snapshot for normal data lifecycle.
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

-- Mutable consumption is isolated from the immutable plan. An UPDATE guarded by
-- consumed_tasks < reserved_tasks is the sole authorization for inserting a
-- tiebreak or hedge, so concurrent consumers can never exceed the bound.
CREATE TABLE IF NOT EXISTS job_economic_reserves (
    job_id         UUID PRIMARY KEY REFERENCES job_economic_plans(job_id) ON DELETE CASCADE,
    reserved_tasks INT NOT NULL CHECK (reserved_tasks >= 0),
    consumed_tasks INT NOT NULL DEFAULT 0 CHECK (consumed_tasks >= 0 AND consumed_tasks <= reserved_tasks),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_job_sla_premium_ref_uniq
    ON ledger_entries (payout_ref)
    WHERE kind = 'buyer_charge' AND task_id IS NULL AND payout_ref IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- Public-site alpha-access capture (docs/CREED_AND_PATH_TO_TEN.md, "Public site
-- & conversion" 4→5). The site's release beat previously said "ask for alpha
-- access" with no mechanism to ask through — this is that mechanism: a real,
-- unauthenticated, rate-limited capture endpoint (POST /v1/alpha-request) so a
-- prospective buyer or supplier can actually leave contact info instead of the
-- funnel dead-ending at the sentence.
CREATE TABLE IF NOT EXISTS alpha_requests (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ DEFAULT now(),
    email      TEXT NOT NULL,
    role       TEXT,   -- 'buyer' | 'supplier' | '' (unspecified) — whichever CTA was clicked
    note       TEXT,   -- optional free-text ("what would you run", etc.)
    source_ip  TEXT    -- for the same per-IP abuse-rate reasoning as signupLimiter
);
CREATE INDEX IF NOT EXISTS alpha_requests_created_at_idx ON alpha_requests (created_at);

-- ─────────────────────────────────────────────────────────────────────────────
-- Buyer Advantage & Pricing Edge 4.5→5 (docs/internal/CREED_AND_PATH_TO_TEN.md,
-- "Reprice from real supplier economics, not hand-seeded constants"). Price
-- provenance: a price is either 'seed' (the original hand-typed launch constant)
-- or 'measured_supplier_economics' (derived by control/pricing.go's
-- repriceFromSupplierEconomics from a real docs/GPU_CAPABILITY.md throughput
-- figure, the real control/payment.go supplier-share rate, and an electricity-cost
-- floor — the same inverse arithmetic scripts/supplier_earnings_calculator.py does
-- by hand for one supplier, now applied to reprice the catalogue itself).
-- price_formula records the exact figures so any repriced number is traceable back
-- to a real measurement, never a re-guessed constant.
ALTER TABLE models ADD COLUMN IF NOT EXISTS price_source  TEXT DEFAULT 'seed';
ALTER TABLE models ADD COLUMN IF NOT EXISTS price_formula TEXT;

-- Project Detection & Quotation 7→8 ("Ship a firm-quote tier: a real commitment,
-- not just an estimate"). An opt-in per-job flag: when set, the buyer's charge is
-- capped at firm_quote_max_usd (the quote's stated maximum) regardless of actual
-- cost — any overage is absorbed by the platform, never passed through. billed_usd
-- is what the buyer was actually charged (== actual_usd unless capped), so the
-- invoice/ledger can show the cap took effect.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS firm_quote          BOOLEAN DEFAULT false;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS firm_quote_max_usd  NUMERIC(12,6);

-- ─────────────────────────────────────────────────────────────────────────────
-- Speed Lane wave 2A (docs/speed-lane-reports/SLA_QUOTE_WAVE2A.md) — the
-- wall-clock speed-SLA quote. A quote whose fleet is SLA-eligible AND whose ETA
-- was planner-backed (real measured per-worker rates, control/planner.go) may
-- carry a TIME GUARANTEE derived from the planner's CONSERVATIVE band plus an
-- explicit safety margin and a merge/collect allowance (control/quote.go
-- slaGuaranteedSecs — every term documented there). The guarantee is priced: a
-- documented premium (sla_premium_usd) that is refunded automatically on a miss.
--
--   quotes.sla_guaranteed_secs / sla_premium_usd — the OFFER persisted with the
--     quote's other assumptions (NULL = no guarantee was offerable: thin fleet,
--     planner disabled, or no measured rates — honest degradation, never a guess).
--   jobs.sla_guarantee_secs / sla_premium_usd — the BINDING, stamped at submit
--     when firm_quote binds an SLA-bearing quote. The guarantee clock is the
--     buyer-visible span: jobs.created_at → jobs.results_merged_at.
--   jobs.sla_met — the OUTCOME: NULL until decided (or no SLA), true = met,
--     false = missed (an sla_refund ledger row + job event were recorded).
--     Deliberately NOT wired into deadline_secs: the deadline drives the stuck-run
--     watchdog's rescue/KILL ladder, while a missed SLA must complete-and-REFUND —
--     killing a late job would destroy the buyer's results to punish lateness.
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS sla_guaranteed_secs INT;
ALTER TABLE quotes ADD COLUMN IF NOT EXISTS sla_premium_usd     NUMERIC(12,6);
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS sla_guarantee_secs  INT;
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS sla_premium_usd     NUMERIC(12,6);
ALTER TABLE jobs   ADD COLUMN IF NOT EXISTS sla_met             BOOLEAN;
-- Exactly ONE sla_refund ledger row per job, structurally: the refund insert is
-- INSERT-if-absent by payout_ref ('sla-<job_id>'), and this partial unique index
-- makes a racing double-insert (two finalize sites + the collect sweep can all
-- observe the same miss) impossible rather than merely unlikely — the same
-- pattern as ledger_stripe_fee_ref_uniq above.
CREATE UNIQUE INDEX IF NOT EXISTS ledger_sla_refund_ref_uniq ON ledger_entries (payout_ref) WHERE kind = 'sla_refund';

-- ─────────────────────────────────────────────────────────────────────────────
-- Public Site & Conversion 6→7 (docs/internal/CREED_AND_PATH_TO_TEN.md, "Make the
-- funnel observable"). A minimal, self-hosted, cookie-free beacon: pageview,
-- scroll-depth per narrative beat, receipts-panel opens, and CTA clicks. No
-- tracking pixel, no third-party script, no cookie — see control/beacon.go for
-- the endpoint and web/index.html's inline beacon script for the client side.
--
-- Cookie-free by construction: page_id is generated client-side in memory only
-- (crypto.randomUUID(), never written to a cookie or localStorage) purely to
-- group the handful of events one single pageview emits — it dies with the tab
-- and is never sent back to the same visitor on a later visit, so it is not a
-- persistent client identifier. source_ip is retained only as long as every
-- other IP already stored in this database (alpha_requests, signup) for the
-- same abuse-rate reasoning, not to build a cross-session profile.
CREATE TABLE IF NOT EXISTS site_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at  TIMESTAMPTZ DEFAULT now(),
    page_id     UUID    NOT NULL, -- in-memory-only per-pageview id (see above) — groups events from one page load, nothing more
    event_type  TEXT    NOT NULL CHECK (event_type IN ('pageview', 'scroll_depth', 'receipts_open', 'cta_click')),
    beat        SMALLINT,         -- narrative beat index (0-6, see web/index.html data-beat) — NULL when not beat-scoped
    detail      TEXT,             -- e.g. which CTA ('alpha-request', 'demo') — free text, capped, never PII
    path        TEXT,             -- request path the beacon fired from
    referrer_host TEXT,           -- host-only referrer (no query string / path — never the full referring URL)
    source_ip   TEXT              -- same per-IP abuse-rate reasoning as alpha_requests.source_ip
);
CREATE INDEX IF NOT EXISTS site_events_created_at_idx ON site_events (created_at);
CREATE INDEX IF NOT EXISTS site_events_type_idx ON site_events (event_type, created_at);
CREATE INDEX IF NOT EXISTS site_events_page_id_idx ON site_events (page_id);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS billed_usd          NUMERIC(12,6);

-- ─────────────────────────────────────────────────────────────────────────────
-- Speed Lane road-to-ten (rubric dimension 5) — the SUBSTRATE-ROUTING receipt.
-- The quote path (control/quote.go) already reads the job's SHAPE and says which
-- substrate runs it fastest (fleet | gpu_lane | gpu_recommend), grounded in the
-- measured 2026-07-06 A100-SXM4-80GB vLLM sweep (control/routing.go). This wave
-- carries that same decision onto the SUBMIT path and PERSISTS it, so the
-- clearing receipt (GET /v1/jobs/{id}/receipt) can project the "we ran it on X
-- because Y" row deterministically from the job row — the same read the invoice
-- already makes, never a re-decision.
--
--   routing_substrate       — fleet | gpu_lane | gpu_recommend (NULL = no routing
--                             block: a non-generative job or an empty input, the
--                             SAME honesty boundary the quote enforces — the sweep
--                             measured generative decode only).
--   routing_reason          — the plain-english why, quote-warnings voice, naming
--                             the measured basis (control/routing.go's Reason).
--   routing_fleet_eta_secs  — the fleet ETA the decision compared (== eta_secs).
--   routing_gpu_modeled_secs— the [MODELED] one-A100 wall-clock the decision
--                             compared: the measured sweep's aggregate tok/s
--                             interpolated at this job's shape, EXCLUDING
--                             rental/provisioning — never a measurement of this job.
-- All four are NULL together (routing_substrate IS NULL) for a job with no
-- routing block, exactly as the submit response omits the block.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS routing_substrate        TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS routing_reason           TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS routing_fleet_eta_secs   INT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS routing_gpu_modeled_secs DOUBLE PRECISION;

-- ────────────────────────────────────────────────────────────────────────
-- Pricing/economics cash-safety tranche: exact card cash facts plus a durable
-- supplier-payout operation. Internal six-decimal settlement is not evidence of
-- the integer cents that crossed a rail, and a clawback racing an in-flight
-- transfer must remain visibly reversal_required until a real reversal exists.
-- ────────────────────────────────────────────────────────────────────────
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_requested_cents BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_received_cents  BIGINT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_currency        TEXT;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_requested_cents BIGINT;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_received_cents  BIGINT;
ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_currency        TEXT;

-- Canonical card-cash facts. A status flag on jobs/charge_batches is not enough
-- to fund supplier cash: one successful PaymentIntent is recorded exactly once,
-- in integer minor units, and is bound to exactly one standalone job OR one
-- frozen charge batch. The primary key prevents the same external cash fact from
-- being presented as two collections across those otherwise separate tables.
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

-- Backfill only exact legacy cash facts. Older "charged" rows without all exact
-- fields remain deliberately absent/unfunded. ON CONFLICT is needed for replaying
-- this idempotent schema; if a PaymentIntent was already (incorrectly) reused by
-- two sources, only its canonical first binding exists and the other source fails
-- closed at payout claim time.
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

-- Signature-verified Stripe refund/dispute deliveries are retained by event id
-- before their mutable object snapshot is applied. Stripe can retry and deliver
-- events out of order, so charge refunds use their cumulative maximum and dispute
-- availability uses event-created time plus a deterministic lifecycle rank.
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

-- A finite operator-declared treasury pool for exceptional make-goods. The
-- external_treasury_ref is an operator assertion, not independent bank
-- reconciliation; the hard invariant here is narrower and exact: reservations
-- against one immutable pool can never exceed authorized_cents.
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

-- One durable funding reservation per supplier liability. Buyer collection
-- reservations point to the exact PaymentIntent pool and the liability's job;
-- platform subsidies point to a finite pool and require an explicit globally
-- unique operator authorization reference plus a reason. Both reserve exact
-- integer cents and survive rail failure/retry so the same incoming cash or
-- authorized subsidy capacity cannot fund a second payout.
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

-- The reservation remains immutable even if its backing collection is later
-- refunded or disputed. This mutable projection surfaces the impairment and the
-- exact signed event that caused it; it never represents an automatic cash reversal.
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

-- Exact six-decimal liability -> provider-cent policy. Cash is always floored to
-- a whole cent (never overpaying one liability); every remaining microusd is kept
-- as a durable, non-negative carry. The arithmetic constraint makes a released
-- liability exactly reconcilable to provider cash plus remainder. A zero-cent row
-- moves to ledger payout_status='carried' and leaves the due sweep instead of
-- aborting or starving later payable rows.
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

-- The external cash fact and funding reservation are append-only identities.
-- Exact ON CONFLICT retries may perform a byte-identical no-op UPDATE; any changed
-- field or DELETE is rejected before payout state can be rewritten under history.
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

-- A subsidy pool's authorization identity and capacity are immutable. Closing a
-- pool is allowed; reopening it or changing the action/ref/treasury/cents/reason
-- would rewrite the meaning of every reservation already bound to the pool.
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

-- Both sides of a money authorization validate the same typed facts at commit.
-- The action is inserted before the resource so its FK is satisfiable; deferred
-- triggers then reject orphan actions, unaudited resources, or any mismatched
-- target/ref/cents/currency/reason before either transaction can commit.
CREATE OR REPLACE FUNCTION validate_money_authority_action_binding()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_ok BOOLEAN;
BEGIN
    IF NEW.kind IN ('subsidy_fund_authorized','payout_subsidy_authorized') THEN
        IF NEW.actor_mode = 'passkey_session' THEN
            SELECT EXISTS (
                SELECT 1
                  FROM admin_sessions s
                  JOIN admin_credentials c ON c.id = s.admin_credential_id
                 WHERE s.id = NEW.actor_session_id
                   AND c.id = NEW.actor_principal_id
                   AND s.revoked = false AND s.expires_at > clock_timestamp()
                   AND c.revoked = false
            ) INTO binding_ok;
        ELSIF NEW.actor_mode = 'break_glass_api_key' THEN
            SELECT EXISTS (
                SELECT 1 FROM api_keys k
                 WHERE k.id = NEW.actor_principal_id
                   AND k.is_admin = true AND k.revoked = false
            ) INTO binding_ok;
        ELSE
            binding_ok := false;
        END IF;
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
