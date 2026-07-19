package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// store.go — the one concrete data layer. A *Store wraps a *pgxpool.Pool and
// owns every SQL query in the control plane. Deliberately NOT an interface:
// there is exactly one implementation, so an interface would be ceremony
// (BLACKHOLE: collapse the indirection). Column and table names MUST match
// db/schema.sql exactly — that schema is the contract.

// Store is the database access layer.
type Store struct {
	pool                  *pgxpool.Pool
	verificationResources *verificationResourceBudget
}

// NewStore wraps an already-opened pool.
func NewStore(pool *pgxpool.Pool) *Store {
	maxConns := int32(0)
	if pool != nil {
		maxConns = pool.Config().MaxConns
	}
	return &Store{
		pool:                  pool,
		verificationResources: newVerificationResourceBudget(maxConns, verificationArtifactMemoryCeiling),
	}
}

// Ping verifies DB connectivity for /healthz.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const schemaMigrationAdvisoryLock = "computeexchange-control-schema-v1"

// acquireSchemaMigrationLock serializes startup migration across every control
// replica and the one-shot schema job. PostgreSQL DDL is transactional, but two
// independently starting replicas can still interleave idempotent-looking ALTER
// and trigger replacement statements. A session lock spans the smaller
// per-table telemetry transactions as well. If unlock cannot be confirmed, the
// connection is removed from the pool and closed so a session lock can never
// leak into normal request traffic.
func (s *Store) acquireSchemaMigrationLock(ctx context.Context) (*pgxpool.Conn, func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	if _, err := conn.Exec(ctx,
		`SELECT pg_advisory_lock(hashtextextended($1, 0))`, schemaMigrationAdvisoryLock); err != nil {
		conn.Release()
		return nil, nil, fmt.Errorf("acquire migration lock: %w", err)
	}

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := conn.QueryRow(cleanupCtx,
			`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, schemaMigrationAdvisoryLock).Scan(&unlocked)
		if err == nil && unlocked {
			conn.Release()
			return
		}
		// Release would return a possibly still-locked session to the pool. Hijack
		// and close it instead; PostgreSQL releases every advisory lock on close.
		raw := conn.Hijack()
		if closeErr := raw.Close(cleanupCtx); closeErr != nil {
			log.Printf("migration lock connection close failed: %v", closeErr)
		}
		if err != nil {
			log.Printf("migration advisory unlock failed; connection discarded: %v", err)
		} else {
			log.Print("migration advisory unlock was not held; connection discarded")
		}
	}
	return conn, release, nil
}

// Migrate applies the control-plane schema changes idempotently. db/schema.sql is
// the base contract; these statements mirror changes the running binary requires
// so a deployment cannot start against a partially upgraded schema. Most changes
// are additive; explicitly marked security migrations may remove legacy sensitive
// columns. Surfacing a failure here is fatal at startup, never silent.
func (s *Store) Migrate(ctx context.Context) error {
	migrationConn, releaseMigrationLock, err := s.acquireSchemaMigrationLock(ctx)
	if err != nil {
		return err
	}
	defer releaseMigrationLock()

	stmts := []string{
		// Per-task input/result object keys. A task is a split of the job's input,
		// so it carries its own chunk key (input_ref) and result target key.
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS input_ref TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS result_key TEXT`,
		// Exact per-task output cardinality. NULL remains an explicit legacy/opaque
		// unknown; it is never backfilled from jobs.split_size because that value is
		// only a ceiling and would overstate every short final chunk.
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS expected_output_records BIGINT`,
		`ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_expected_output_records_positive`,
		`ALTER TABLE tasks ADD CONSTRAINT tasks_expected_output_records_positive
		   CHECK (expected_output_records IS NULL OR expected_output_records > 0)`,
		`CREATE OR REPLACE FUNCTION cx_reject_expected_output_records_update() RETURNS trigger AS $$
		 BEGIN
		   IF OLD.expected_output_records IS DISTINCT FROM NEW.expected_output_records THEN
		     RAISE EXCEPTION 'task expected output records for % are immutable', OLD.id;
		   END IF;
		   RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS tasks_expected_output_records_immutable ON tasks`,
		`CREATE TRIGGER tasks_expected_output_records_immutable
		   BEFORE UPDATE OF expected_output_records ON tasks
		   FOR EACH ROW EXECUTE FUNCTION cx_reject_expected_output_records_update()`,
		// Verification-requeue worker exclusion (Scheduling & Matching Engine 8->9,
		// docs/internal/CREED_AND_PATH_TO_TEN.md): RequeueTask records the worker that
		// just failed a task here (until excluded_until) so the claim skips it for a
		// window, then the exclusion expires — a thin fleet is never permanently
		// starved of the retry. See RequeueTask (store.go) and ClaimTaskSQL (scheduler.go).
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS excluded_worker UUID`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS excluded_until  TIMESTAMPTZ`,
		// Webhook registrations are a leased, backoff-aware outbox. A stable
		// (job_id,buyer_id) FK prevents a job-scoped registration from ever crossing
		// buyer ownership, even if a future caller bypasses the HTTP handler.
		`CREATE TABLE IF NOT EXISTS webhooks (
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
		 )`,
		// Real model + pricing catalogue (replaces the old static Go list).
		`CREATE TABLE IF NOT EXISTS models (
		   id            TEXT PRIMARY KEY,
		   family        TEXT,
		   quant         TEXT,
		   kind          TEXT,
		   dim           INT,
		   job_type      TEXT,
		   price_per_1k  NUMERIC(12,8),
		   price_per_unit NUMERIC(12,8),
		   min_memory_gb REAL,
		   hf_repo       TEXT
		 )`,
		`CREATE INDEX IF NOT EXISTS webhooks_job_idx ON webhooks (job_id)`,
		// Additive outbox upgrade for databases created before durable retry state.
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS delivered_at TIMESTAMPTZ`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS lease_token UUID`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS dead_lettered_at TIMESTAMPTZ`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS last_error TEXT`,
		`ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS signing_secret_sealed TEXT`,
		// Never preserve a plaintext/raw partial-deployment value. Converting it to
		// the same NULL legacy state below guarantees it is dead-lettered before I/O.
		`UPDATE webhooks
		    SET signing_secret_sealed=NULL
		  WHERE signing_secret_sealed IS NOT NULL
		    AND NOT (
		      signing_secret_sealed LIKE 'enc:%' AND length(signing_secret_sealed)>4
		    )`,
		`DO $$
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
		 END
		 $$`,
		`ALTER TABLE webhooks VALIDATE CONSTRAINT webhooks_signing_secret_sealed_check`,
		// A pre-signing registration has no secret that its receiver could know.
		// Dead-letter it without an outbound request; an authenticated exact
		// re-registration upgrades the row with a new recoverable secret.
		`UPDATE webhooks
		    SET dead_lettered_at=now(),
		        lease_token=NULL,
		        lease_expires_at=NULL,
		        last_error='legacy webhook has no per-registration signing secret; re-register it'
		  WHERE delivered_at IS NULL
		    AND dead_lettered_at IS NULL
		    AND signing_secret_sealed IS NULL`,
		// NULL-job "catch-all" rows were historically accepted but never had a
		// fanout/delivery model, so they could never fire. Ownership-invalid legacy
		// rows are equally undeliverable. Remove both before installing the honest
		// one-row/one-job invariant.
		`DELETE FROM webhooks wh
		  WHERE wh.job_id IS NULL OR wh.buyer_id IS NULL
		     OR NOT EXISTS (
		       SELECT 1 FROM jobs j WHERE j.id=wh.job_id AND j.buyer_id=wh.buyer_id
		     )`,
		`ALTER TABLE webhooks ALTER COLUMN buyer_id SET NOT NULL`,
		`ALTER TABLE webhooks ALTER COLUMN job_id SET NOT NULL`,
		// Collapse duplicate legacy deliveries before making registration idempotent.
		// Prefer an already-delivered row so migration never resurrects an event.
		`DELETE FROM webhooks
		  WHERE id IN (
		    SELECT id FROM (
		      SELECT id,row_number() OVER (
		        PARTITION BY buyer_id,job_id,url
		        ORDER BY (delivered_at IS NULL),(dead_lettered_at IS NOT NULL),created_at NULLS LAST,id
		      ) AS duplicate_rank
		      FROM webhooks
		    ) ranked
		    WHERE duplicate_rank>1
		  )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS webhooks_job_url_uniq ON webhooks (buyer_id,job_id,url)`,
		`CREATE INDEX IF NOT EXISTS webhooks_delivery_due_idx ON webhooks (next_attempt_at,created_at,id)
		   WHERE delivered_at IS NULL AND dead_lettered_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS jobs_id_buyer_id_uniq ON jobs (id,buyer_id)`,
		`DO $$
		 BEGIN
		   IF NOT EXISTS (
		     SELECT 1 FROM pg_constraint
		      WHERE conrelid='webhooks'::regclass AND conname='webhooks_job_owner_fkey'
		   ) THEN
		     ALTER TABLE webhooks ADD CONSTRAINT webhooks_job_owner_fkey
		       FOREIGN KEY (job_id,buyer_id) REFERENCES jobs(id,buyer_id)
		       ON DELETE CASCADE NOT VALID;
		   END IF;
		 END
		 $$`,
		`ALTER TABLE webhooks VALIDATE CONSTRAINT webhooks_job_owner_fkey`,
		// Scheduler V2 / Turbo columns. These MIRROR db/schema.sql (owned by infra)
		// so a control plane that only ran Migrate (not the full schema.sql) still
		// self-migrates to the columns the hard-filter claim + result merge need.
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS min_memory_gb REAL DEFAULT 0`,
		// Buyer-authored execution ceiling. Keep the full uint32 wire range and make
		// zero the explicit "runner default" value for legacy jobs.
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS max_duration_secs BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_max_duration_secs_range`,
		`ALTER TABLE jobs ADD CONSTRAINT jobs_max_duration_secs_range
		   CHECK (max_duration_secs BETWEEN 0 AND 4294967295)`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS min_reputation REAL DEFAULT 0`,
		// Private Deployment tier (research §3): a private_pool job routes ONLY to the
		// buyer's own bound suppliers (their dedicated Mac/GPU fleet), so "data never
		// leaves our boxes" is contractual, not marketing. private_pool_members binds
		// a buyer to the suppliers allowed to run their private work.
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS private_pool BOOLEAN DEFAULT false`,
		`CREATE TABLE IF NOT EXISTS private_pool_members (
		   buyer_id    UUID NOT NULL,
		   supplier_id UUID NOT NULL,
		   created_at  TIMESTAMPTZ DEFAULT now(),
		   PRIMARY KEY (buyer_id, supplier_id)
		 )`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS hw_classes TEXT[]`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS data_residency TEXT[]`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS split_size INT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS offered_rate_usd_hr REAL`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS eta_secs INT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS job_type_spec JSONB`,
		// Budget Governor (Plane C §12 / Plane D §14 D8): buyer hard spend cap +
		// the governor state machine. NULL max_usd = no cap (unchanged behavior).
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS max_usd NUMERIC(12,6)`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS budget_state TEXT DEFAULT 'tracking'`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS chunk_index INT DEFAULT 0`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS hedged_from UUID`,
		// Third-opinion tasks carry the immutable verification class of the
		// disagreement attempt that created them. Worker profiles are mutable, so a
		// later retry must never infer its comparison class from today's profile.
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_hw_class TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_engine TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_build_hash TEXT`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS engine TEXT NOT NULL DEFAULT 'candle'`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS build_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS supported_jobs TEXT[]`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS supported_models TEXT[]`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS min_payout_usd_hr REAL DEFAULT 0`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS thermal_ok BOOLEAN DEFAULT true`,
		// Durable per-worker service-lane debt. Three consecutive ordinary priority
		// claims force the next eligible ordinary batch task to the front; pinned
		// verification claims bypass and do not mutate this counter.
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS priority_claim_streak INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE workers DROP CONSTRAINT IF EXISTS workers_priority_claim_streak_range`,
		`ALTER TABLE workers ADD CONSTRAINT workers_priority_claim_streak_range
		   CHECK (priority_claim_streak BETWEEN 0 AND 3)`,
		// Exact worker/runtime authority. Deliberately no INSERT...SELECT backfill from
		// supported_jobs/supported_models: legacy array-only workers remain inert until
		// a real registration atomically projects the current generated matrix.
		`CREATE TABLE IF NOT EXISTS worker_authorized_capabilities (
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
		 )`,
		// Rows created by the pre-model-kind projection cannot be upgraded from the
		// mutable model catalog without inventing authority. Delete them and require a
		// real worker re-registration, matching the existing no-array-backfill rule.
		`ALTER TABLE worker_authorized_capabilities ADD COLUMN IF NOT EXISTS model_kind TEXT`,
		`DELETE FROM worker_authorized_capabilities
		   WHERE COALESCE(model_kind,'') NOT IN ('gguf','hf','mlx')`,
		`ALTER TABLE worker_authorized_capabilities ALTER COLUMN model_kind SET NOT NULL`,
		`ALTER TABLE worker_authorized_capabilities
		   DROP CONSTRAINT IF EXISTS worker_authorized_capabilities_model_kind_valid`,
		`ALTER TABLE worker_authorized_capabilities
		   ADD CONSTRAINT worker_authorized_capabilities_model_kind_valid
		   CHECK (model_kind IN ('gguf','hf','mlx'))`,
		`CREATE INDEX IF NOT EXISTS worker_authorized_capabilities_exact_idx
		   ON worker_authorized_capabilities (worker_id, job_type, model_ref, matrix_sha256)`,
		`CREATE INDEX IF NOT EXISTS worker_authorized_capabilities_supply_idx
		   ON worker_authorized_capabilities (job_type, model_ref, matrix_sha256, worker_id)`,
		// Immutable execution provenance for the CURRENT task attempt. ClaimTask writes
		// these fields from the server-authorized exact capability row in the same
		// transaction that hands the task to a worker. A retry may replace them with
		// the newly selected attempt's authority, but workers can never supply or edit
		// them. Clearing receipts therefore bind the accepted task to the exact runtime
		// cell and matrix revision that authorized execution instead of inferring it
		// later from a mutable worker registration.
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_cell_id TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_id TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_matrix_sha256 TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS model_kind TEXT`,
		// Immutable identity of the worker attempt itself. workers is live routing
		// state and UpsertWorker intentionally mutates its class/build; accepted work
		// must remain attributable to the exact identity observed by ClaimTask.
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_worker_id UUID`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_supplier_id UUID`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_hw_class TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_engine TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_build_hash TEXT`,
		`ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_execution_identity_complete`,
		`ALTER TABLE tasks ADD CONSTRAINT tasks_execution_identity_complete CHECK (
		   (execution_worker_id IS NULL AND execution_supplier_id IS NULL
		    AND execution_hw_class IS NULL AND execution_engine IS NULL
		    AND execution_build_hash IS NULL)
		   OR
		   (execution_worker_id IS NOT NULL AND execution_supplier_id IS NOT NULL
		    AND COALESCE(btrim(execution_hw_class),'') <> ''
		    AND COALESCE(btrim(execution_engine),'') <> ''
		    AND execution_build_hash IS NOT NULL)
		 )`,
		`ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_runtime_provenance_complete`,
		`ALTER TABLE tasks ADD CONSTRAINT tasks_runtime_provenance_complete CHECK (
		   (runtime_cell_id IS NULL AND runtime_id IS NULL AND runtime_matrix_sha256 IS NULL AND model_kind IS NULL)
		   OR
		   (COALESCE(runtime_cell_id,'') <> '' AND COALESCE(runtime_id,'') <> ''
		    AND runtime_matrix_sha256 ~ '^[0-9a-f]{64}$' AND COALESCE(model_kind,'') <> '')
		 )`,
		// Durable third-opinion claim history keeps every worker/supplier that started
		// a tiebreak visible after FailTask/the stale reaper clears tasks.worker_id.
		// No prior peer (or another machine owned by that supplier) may retry the
		// third vote; ordinary claims deliberately do not write this sparse table.
		`CREATE TABLE IF NOT EXISTS task_execution_history (
		   task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		   attempt SMALLINT NOT NULL CHECK (attempt >= 0),
		   worker_id UUID NOT NULL REFERENCES workers(id) ON DELETE RESTRICT,
		   supplier_id UUID NOT NULL REFERENCES suppliers(id) ON DELETE RESTRICT,
		   started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   PRIMARY KEY (task_id,attempt,worker_id)
		 )`,
		`CREATE INDEX IF NOT EXISTS task_execution_history_task_supplier_idx
		   ON task_execution_history (task_id,supplier_id,worker_id)`,
		`ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS data_country TEXT`,
		`ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS quarantined_at TIMESTAMPTZ`,
		// Stripe Connect payout readiness (suppliers.go): flipped by the account.updated
		// webhook once Stripe says the connected account can receive transfers.
		`ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS payouts_enabled BOOLEAN DEFAULT false`,
		`CREATE INDEX IF NOT EXISTS tasks_job_chunk_idx ON tasks (job_id, chunk_index)`,
		`CREATE INDEX IF NOT EXISTS workers_supplier_idx ON workers (supplier_id)`,
		// Quote-to-actual drift feedback (Plane D D6 / errata C-Errata-6). MIRRORS
		// db/schema.sql: real per-task durations of COMMITTED tasks only, so the
		// Exchange Brain learns an observed p90 the next quote's ETA can lean on.
		// Malformed/failed tasks never write a row, so they cannot poison the estimate.
		`CREATE TABLE IF NOT EXISTS task_durations (
		   id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   created_at  TIMESTAMPTZ DEFAULT now(),
		   job_id      UUID,
		   job_type    TEXT,
		   model_ref   TEXT,
		   split_size  INT,
		   duration_ms BIGINT
		 )`,
		`CREATE INDEX IF NOT EXISTS task_durations_type_model_idx ON task_durations (job_type, model_ref)`,
		// Performance Observability 6.5->7 (docs/internal/CREED_AND_PATH_TO_TEN.md):
		// the committing worker's identity + verification class, so a version bump
		// or a heterogeneous fleet can be sliced out of the same duration history
		// instead of one blended average hiding a regression on just one build.
		// build_hash already IS the version-sliced identity this repo tracks
		// (hardware::engine_build_hash folds in agent version + device backend +
		// kernel identity — see docs/DETERMINISM_CLASS.md), so it stands in for a
		// separate raw "agent_version" column.
		`ALTER TABLE task_durations ADD COLUMN IF NOT EXISTS worker_id UUID`,
		`ALTER TABLE task_durations ADD COLUMN IF NOT EXISTS engine TEXT`,
		`ALTER TABLE task_durations ADD COLUMN IF NOT EXISTS build_hash TEXT`,
		`ALTER TABLE task_durations ADD COLUMN IF NOT EXISTS task_id UUID`,
		`CREATE TABLE IF NOT EXISTS billing_customers (
		   buyer_id               UUID PRIMARY KEY,
		   stripe_customer_id     TEXT,
		   default_payment_method TEXT,
		   created_at             TIMESTAMPTZ DEFAULT now()
		 )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS billing_customers_stripe_customer_uidx
		   ON billing_customers (stripe_customer_id)
		   WHERE stripe_customer_id IS NOT NULL AND stripe_customer_id <> ''`,
		// Supplier Connect account (the payout transfers' destination) + the
		// intake→job links that drive multi-stage pipeline execution.
		`ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS stripe_acct TEXT`,
		// Self-serve buyer accounts (accounts.go) + opaque session tokens. MIRRORS
		// db/schema.sql so a control plane that only ran Migrate self-migrates. buyers
		// is the account of record: UNIQUE email + bcrypt password_hash + a sandbox
		// free_credit_usd grant. sessions are hashed-at-rest opaque tokens (like
		// api_keys/worker_tokens) the login flow issues and authBuyer accepts.
		`CREATE TABLE IF NOT EXISTS buyers (
		   id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   email           TEXT UNIQUE NOT NULL,
		   password_hash   TEXT,
		   free_credit_usd NUMERIC(12,6) NOT NULL DEFAULT 0,
		   created_at      TIMESTAMPTZ DEFAULT now()
		 )`,
		// Supplier identity is owned by an authenticated buyer account. Legacy rows
		// are backfilled only when the case-insensitive email relationship is exactly
		// one-to-one; ambiguous/unmatched suppliers stay unowned and therefore inert
		// to self-serve routes. Stripe Connect, not CX, stores KYC/tax identifiers.
		`ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS owner_buyer_id UUID`,
		`WITH ownership_matches AS (
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
		    )`,
		`DO $$ BEGIN
		   IF NOT EXISTS (
		       SELECT 1 FROM pg_constraint
		        WHERE conname = 'suppliers_owner_buyer_fk'
		          AND conrelid = 'suppliers'::regclass
		   ) THEN
		     ALTER TABLE suppliers
		       ADD CONSTRAINT suppliers_owner_buyer_fk
		       FOREIGN KEY (owner_buyer_id) REFERENCES buyers(id) ON DELETE RESTRICT;
		   END IF;
		 END $$`,
		`CREATE UNIQUE INDEX IF NOT EXISTS suppliers_owner_buyer_uniq
		   ON suppliers (owner_buyer_id) WHERE owner_buyer_id IS NOT NULL`,
		`ALTER TABLE suppliers DROP COLUMN IF EXISTS tax_id`,
		`ALTER TABLE suppliers DROP COLUMN IF EXISTS tax_country`,
		`CREATE TABLE IF NOT EXISTS sessions (
		   token_hash TEXT PRIMARY KEY,
		   buyer_id   UUID NOT NULL,
		   created_at TIMESTAMPTZ DEFAULT now(),
		   expires_at TIMESTAMPTZ NOT NULL,
		   revoked    BOOLEAN DEFAULT false
		 )`,
		`CREATE INDEX IF NOT EXISTS sessions_buyer_idx ON sessions (buyer_id)`,
		// OAuth account-link attempts are server-side capabilities, not signed buyer
		// identifiers. Only hashes of the two independent browser secrets are stored;
		// the callback atomically consumes a live row and recovers its bound buyer.
		// buyer_id historically also identifies API-key-only buyers that predate the
		// buyers account table, so do not make OAuth linking silently unavailable to
		// those still-valid identities.
		// Device-proofed, one-time supplier enrollment. Existing seed/manual tokens
		// remain valid but visibly unbound; exchanged credentials carry the P-256
		// public-key binding, stable lifecycle id, rotation chain, and audit trail.
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS credential_id UUID DEFAULT gen_random_uuid()`,
		`UPDATE worker_tokens SET credential_id = gen_random_uuid() WHERE credential_id IS NULL`,
		`ALTER TABLE worker_tokens ALTER COLUMN credential_id SET NOT NULL`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS device_key_algorithm TEXT`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS device_public_key BYTEA`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS device_fingerprint TEXT`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS credential_version INT NOT NULL DEFAULT 1`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS rotated_from_credential_id UUID`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS label TEXT`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ`,
		`ALTER TABLE worker_tokens ADD COLUMN IF NOT EXISTS revocation_reason TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS worker_tokens_credential_id_uniq ON worker_tokens (credential_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS worker_tokens_active_device_fingerprint_uniq
		   ON worker_tokens (device_fingerprint)
		   WHERE device_fingerprint IS NOT NULL AND revoked = false`,
		`ALTER TABLE worker_tokens DROP CONSTRAINT IF EXISTS worker_tokens_device_binding_valid`,
		`ALTER TABLE worker_tokens ADD CONSTRAINT worker_tokens_device_binding_valid CHECK (
		   num_nonnulls(device_key_algorithm,device_public_key,device_fingerprint) = 0
		   OR (num_nonnulls(device_key_algorithm,device_public_key,device_fingerprint) = 3
		       AND device_key_algorithm = 'p256' AND octet_length(device_public_key) = 65)
		 )`,
		`ALTER TABLE worker_tokens DROP CONSTRAINT IF EXISTS worker_tokens_credential_version_positive`,
		`ALTER TABLE worker_tokens ADD CONSTRAINT worker_tokens_credential_version_positive CHECK (credential_version > 0)`,
		`ALTER TABLE worker_tokens DROP CONSTRAINT IF EXISTS worker_tokens_rotated_from_fk`,
		`CREATE TABLE IF NOT EXISTS worker_enrollment_codes (
		   id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   code_hash TEXT NOT NULL UNIQUE,
		   buyer_id UUID NOT NULL REFERENCES buyers(id) ON DELETE CASCADE,
		   supplier_id UUID NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
		   audience TEXT NOT NULL,
		   device_key_algorithm TEXT NOT NULL CHECK (device_key_algorithm = 'p256'),
		   device_public_key BYTEA NOT NULL CHECK (octet_length(device_public_key) = 65),
		   device_fingerprint TEXT NOT NULL,
		   label TEXT,
		   rotate_from_credential_id UUID,
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   expires_at TIMESTAMPTZ NOT NULL,
		   consumed_at TIMESTAMPTZ,
		   consumed_credential_id UUID REFERENCES worker_tokens(credential_id) ON DELETE SET NULL,
		   revoked_at TIMESTAMPTZ,
		   failed_attempts INT NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
		   last_attempt_at TIMESTAMPTZ,
		   CHECK (expires_at > created_at)
		 )`,
		// Enrollment wire v2 cannot safely infer a trusted origin for legacy rows.
		// Preserve them as protocol 1 and require every newly issued v2 row to carry
		// the exact origin/request binding that the device signs at exchange time.
		`ALTER TABLE worker_enrollment_codes ADD COLUMN IF NOT EXISTS protocol_version INT NOT NULL DEFAULT 1`,
		`ALTER TABLE worker_enrollment_codes ADD COLUMN IF NOT EXISTS control_origin TEXT`,
		`ALTER TABLE worker_enrollment_codes ADD COLUMN IF NOT EXISTS request_id TEXT`,
		`ALTER TABLE worker_enrollment_codes DROP CONSTRAINT IF EXISTS worker_enrollment_codes_protocol_binding_valid`,
		`ALTER TABLE worker_enrollment_codes ADD CONSTRAINT worker_enrollment_codes_protocol_binding_valid CHECK (
		   (protocol_version = 1 AND control_origin IS NULL AND request_id IS NULL)
		   OR
		   (protocol_version = 2 AND control_origin IS NOT NULL AND control_origin <> ''
		       AND request_id IS NOT NULL AND char_length(request_id) = 22)
		 )`,
		`ALTER TABLE worker_enrollment_codes
		   DROP CONSTRAINT IF EXISTS worker_enrollment_codes_rotate_from_credential_id_fkey`,
		`CREATE INDEX IF NOT EXISTS worker_enrollment_codes_owner_idx
		   ON worker_enrollment_codes (buyer_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS worker_enrollment_codes_expiry_idx
		   ON worker_enrollment_codes (expires_at)
		   WHERE consumed_at IS NULL AND revoked_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS worker_credential_audit (
		   id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   event_type TEXT NOT NULL CHECK (event_type IN (
		     'code_issued','code_revoked','exchange_succeeded','exchange_rejected',
		     'credential_revoked','credential_rotated')),
		   buyer_id UUID,
		   supplier_id UUID,
		   worker_id UUID,
		   enrollment_code_id UUID,
		   credential_id UUID,
		   reason TEXT,
		   detail JSONB NOT NULL DEFAULT '{}'::jsonb,
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE INDEX IF NOT EXISTS worker_credential_audit_owner_idx
		   ON worker_credential_audit (buyer_id, created_at DESC)`,
		`CREATE OR REPLACE FUNCTION cx_reject_worker_credential_audit_mutation() RETURNS trigger AS $$
		 BEGIN
		   RAISE EXCEPTION 'worker credential audit is append-only';
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS worker_credential_audit_append_only ON worker_credential_audit`,
		`CREATE TRIGGER worker_credential_audit_append_only
		   BEFORE UPDATE OR DELETE ON worker_credential_audit
		   FOR EACH ROW EXECUTE FUNCTION cx_reject_worker_credential_audit_mutation()`,
		// Stuck-run watchdog V2 (MIRRORS db/schema.sql): escalation strikes, the
		// buyer deadline knob, and the ETA calibration loop.
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS watchdog_strikes INT NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS deadline_secs INT`,
		`CREATE TABLE IF NOT EXISTS eta_calibration (
		   id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   job_id         UUID,
		   job_type       TEXT,
		   tier           TEXT,
		   predicted_secs INT,
		   realized_secs  INT,
		   created_at     TIMESTAMPTZ DEFAULT now()
		 )`,
		`CREATE INDEX IF NOT EXISTS eta_calibration_type_idx ON eta_calibration (job_type, tier, created_at DESC)`,
		// Charge batching + Stripe fee truth (MIRRORS db/schema.sql; see collect.go).
		// A charge batch is ONE PaymentIntent covering many small deferred jobs of one
		// buyer; amount_usd is FROZEN at formation. A durable request operation now
		// prevents any blind retry after an ambiguous external attempt.
		`CREATE TABLE IF NOT EXISTS charge_batches (
		   id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   buyer_id   UUID NOT NULL,
		   amount_usd NUMERIC(10,6) NOT NULL,
		   status     TEXT NOT NULL DEFAULT 'attempting',
		   stripe_pi  TEXT,
		   created_at TIMESTAMPTZ DEFAULT now(),
		   charged_at TIMESTAMPTZ
		 )`,
		`CREATE INDEX IF NOT EXISTS charge_batches_status_idx ON charge_batches (status, created_at)`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_status TEXT NOT NULL DEFAULT 'not_attempted'`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_batch_id UUID`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_attempts INT NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_next_at TIMESTAMPTZ`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS deferred_at TIMESTAMPTZ`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_attempt_usd NUMERIC(10,6)`,
		`ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0`,
		`ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS next_at TIMESTAMPTZ`,
		`ALTER TABLE charge_batches ALTER COLUMN amount_usd TYPE NUMERIC(12,6)`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS stripe_pi TEXT`,
		`CREATE INDEX IF NOT EXISTS jobs_charge_status_idx ON jobs (charge_status)`,
		`CREATE TABLE IF NOT EXISTS buyer_charge_operations (
		   operation_key TEXT PRIMARY KEY CHECK (btrim(operation_key) <> ''),
		   source_kind TEXT NOT NULL CHECK (source_kind IN ('job','batch')),
		   job_id UUID UNIQUE REFERENCES jobs(id) ON DELETE RESTRICT,
		   charge_batch_id UUID UNIQUE REFERENCES charge_batches(id) ON DELETE RESTRICT,
		   buyer_id UUID NOT NULL,
		   stripe_customer TEXT NOT NULL CHECK (btrim(stripe_customer) <> ''),
		   stripe_payment_method TEXT NOT NULL CHECK (btrim(stripe_payment_method) <> ''),
		   amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
		   currency TEXT NOT NULL CHECK (currency='usd'),
		   status TEXT NOT NULL CHECK (status IN ('outcome_unknown','succeeded')),
		   payment_intent TEXT UNIQUE,
		   charge_id TEXT UNIQUE,
		   last_error TEXT,
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   CHECK ((source_kind='job' AND job_id IS NOT NULL AND charge_batch_id IS NULL)
		       OR (source_kind='batch' AND charge_batch_id IS NOT NULL AND job_id IS NULL)),
		   CHECK (status<>'succeeded' OR (payment_intent IS NOT NULL AND charge_id IS NOT NULL))
		 )`,
		`CREATE INDEX IF NOT EXISTS buyer_charge_operations_status_idx
		   ON buyer_charge_operations (status,created_at)`,
		// One stripe_fee ledger row per PaymentIntent, structurally (the fee recorder
		// is INSERT-if-absent by payout_ref; the partial unique index closes the race).
		`CREATE UNIQUE INDEX IF NOT EXISTS ledger_stripe_fee_ref_uniq ON ledger_entries (payout_ref) WHERE kind = 'stripe_fee'`,
		// Independent economic facts (economic_facts.go). Exact input/output units
		// are nullable ingestion seams: current historical rows stay NULL rather than
		// being reverse-engineered from quote estimates. The projection table is one
		// idempotently recomputed row per job; unknown fee attribution stays NULL.
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_records BIGINT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_bytes BIGINT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_input_source TEXT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_records BIGINT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_bytes BIGINT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS economic_output_source TEXT`,
		`CREATE TABLE IF NOT EXISTS job_economic_facts (
		   job_id UUID PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
		   buyer_id UUID NOT NULL,
		   job_status TEXT NOT NULL,
		   charge_status TEXT NOT NULL,
		   schema_version SMALLINT NOT NULL DEFAULT 1,
		   reconciliation_state TEXT NOT NULL DEFAULT 'pending'
		     CHECK (reconciliation_state IN ('pending','awaiting_collection','awaiting_processor_fee','unresolved_batch_fee','incomplete','complete')),
		   missing_data_reasons JSONB NOT NULL DEFAULT '[]'::jsonb,
		   input_records BIGINT,
		   input_bytes BIGINT,
		   input_units_source TEXT,
		   output_records BIGINT,
		   output_bytes BIGINT,
		   output_units_source TEXT,
		   control_plane_elapsed_ms BIGINT,
		   control_plane_elapsed_source TEXT,
		   primary_tasks_run INT NOT NULL DEFAULT 0,
		   verification_tasks_run INT NOT NULL DEFAULT 0,
		   retry_attempts INT NOT NULL DEFAULT 0,
		   verdict_attempts INT NOT NULL DEFAULT 0,
		   verification_task_server_ms BIGINT,
		   verification_tasks_with_server_ms INT NOT NULL DEFAULT 0,
		   verification_work_source TEXT NOT NULL,
		   worker_reported_tokens BIGINT,
		   worker_reported_tokens_tasks INT NOT NULL DEFAULT 0,
		   worker_reported_tokens_source TEXT,
		   settlement_usd NUMERIC(12,6),
		   settlement_usd_basis TEXT NOT NULL,
		   supplier_liability_usd NUMERIC(12,6),
		   supplier_liability_basis TEXT NOT NULL,
		   refunds_usd NUMERIC(12,6),
		   refunds_basis TEXT NOT NULL,
		   billed_usd NUMERIC(12,6),
		   billed_usd_basis TEXT NOT NULL,
		   processor_fee_payment_intent TEXT,
		   processor_fee_payment_intent_total_usd NUMERIC(12,6),
		   processor_fee_usd NUMERIC(12,6),
		   processor_fee_basis TEXT,
		   contribution_margin_usd NUMERIC(12,6),
		   recomputed_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE INDEX IF NOT EXISTS job_economic_facts_state_idx
		   ON job_economic_facts (reconciliation_state, recomputed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS charge_batch_fee_allocations (
		   charge_batch_id UUID NOT NULL REFERENCES charge_batches(id) ON DELETE CASCADE,
		   job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		   stripe_pi TEXT NOT NULL,
		   allocation_ordinal INT NOT NULL,
		   billed_weight_usd NUMERIC(12,6) NOT NULL CHECK (billed_weight_usd > 0),
		   allocated_fee_usd NUMERIC(12,6) NOT NULL CHECK (allocated_fee_usd >= 0),
		   allocated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   PRIMARY KEY (charge_batch_id, job_id),
		   UNIQUE (job_id),
		   UNIQUE (charge_batch_id, allocation_ordinal)
		 )`,
		`CREATE INDEX IF NOT EXISTS charge_batch_fee_allocations_pi_idx
		   ON charge_batch_fee_allocations (stripe_pi)`,
		// Fail-closed quote/submit plans, immutable per-job snapshot, frozen task
		// amounts, and the separately mutable dynamic-work reserve.
		`ALTER TABLE quotes ADD COLUMN IF NOT EXISTS economic_schedule_version TEXT`,
		`ALTER TABLE quotes ADD COLUMN IF NOT EXISTS economic_plan JSONB`,
		`ALTER TABLE quotes ADD COLUMN IF NOT EXISTS economic_executable BOOLEAN NOT NULL DEFAULT false`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS economic_buyer_charge_usd NUMERIC(12,6)`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS economic_supplier_payout_usd NUMERIC(12,6)`,
		`ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_frozen_economic_amounts_valid`,
		`ALTER TABLE tasks ADD CONSTRAINT tasks_frozen_economic_amounts_valid CHECK (
		   (economic_buyer_charge_usd IS NULL AND economic_supplier_payout_usd IS NULL)
		   OR (economic_buyer_charge_usd > 0 AND economic_supplier_payout_usd >= 0
		       AND economic_supplier_payout_usd <= economic_buyer_charge_usd)
		 )`,
		`CREATE OR REPLACE FUNCTION cx_reject_frozen_task_economics_update() RETURNS trigger AS $$
		 BEGIN
		   IF OLD.economic_buyer_charge_usd IS DISTINCT FROM NEW.economic_buyer_charge_usd
		      OR OLD.economic_supplier_payout_usd IS DISTINCT FROM NEW.economic_supplier_payout_usd THEN
		     RAISE EXCEPTION 'task economic amounts for % are immutable', OLD.id;
		   END IF;
		   RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS tasks_frozen_economics_immutable ON tasks`,
		`CREATE TRIGGER tasks_frozen_economics_immutable
		   BEFORE UPDATE OF economic_buyer_charge_usd, economic_supplier_payout_usd ON tasks
		   FOR EACH ROW EXECUTE FUNCTION cx_reject_frozen_task_economics_update()`,
		`CREATE TABLE IF NOT EXISTS job_economic_plans (
		   job_id UUID PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
		   plan_version SMALLINT NOT NULL,
		   schedule_version TEXT NOT NULL,
		   plan_json JSONB NOT NULL,
		   initial_task_count INT NOT NULL CHECK (initial_task_count > 0),
		   buyer_charge_per_task_usd NUMERIC(12,6) NOT NULL CHECK (buyer_charge_per_task_usd > 0),
		   supplier_payout_per_task_usd NUMERIC(12,6) NOT NULL CHECK (supplier_payout_per_task_usd >= 0),
		   initial_buyer_charge_usd NUMERIC(12,6) NOT NULL CHECK (initial_buyer_charge_usd > 0),
		   reserved_buyer_charge_usd NUMERIC(12,6) NOT NULL CHECK (reserved_buyer_charge_usd >= initial_buyer_charge_usd),
		   sla_premium_usd NUMERIC(12,6) NOT NULL DEFAULT 0 CHECK (sla_premium_usd >= 0),
		   firm_quote_max_usd NUMERIC(12,6),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`ALTER TABLE job_economic_plans DROP CONSTRAINT IF EXISTS job_economic_plans_firm_quote_max_positive`,
		`ALTER TABLE job_economic_plans ADD CONSTRAINT job_economic_plans_firm_quote_max_positive
		   CHECK (firm_quote_max_usd IS NULL OR firm_quote_max_usd > 0)`,
		`CREATE OR REPLACE FUNCTION cx_reject_job_economic_plan_update() RETURNS trigger AS $$
		 BEGIN
		   RAISE EXCEPTION 'job economic plan % is immutable', OLD.job_id;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS job_economic_plans_immutable ON job_economic_plans`,
		`CREATE TRIGGER job_economic_plans_immutable
		   BEFORE UPDATE ON job_economic_plans
		   FOR EACH ROW EXECUTE FUNCTION cx_reject_job_economic_plan_update()`,
		`CREATE TABLE IF NOT EXISTS job_economic_reserves (
		   job_id UUID PRIMARY KEY REFERENCES job_economic_plans(job_id) ON DELETE CASCADE,
		   reserved_tasks INT NOT NULL CHECK (reserved_tasks >= 0),
		   consumed_tasks INT NOT NULL DEFAULT 0 CHECK (consumed_tasks >= 0 AND consumed_tasks <= reserved_tasks),
		   updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ledger_job_sla_premium_ref_uniq
		   ON ledger_entries (payout_ref)
		   WHERE kind = 'buyer_charge' AND task_id IS NULL AND payout_ref IS NOT NULL`,
		// Wake-on-work (docs/CREED_AND_PATH_TO_TEN.md, "Control plane hot path" 6→7 /
		// "Scalability headroom" 5→6 / "End-to-end job latency" 7.5→8 — the same fix
		// serves all three). Before this, every idle long-polling worker re-attempted
		// a full ClaimTask transaction every 250ms regardless of whether any work
		// existed. A STATEMENT-level trigger (fires once per statement, not once per
		// row, so a 5,500-chunk submit sends one notify, not 5,500) calls pg_notify
		// whenever a task is inserted (a new job) or its status/visible_at changes (a
		// requeue, a hedge, a rescue) — notify.go's listener wakes every waiting
		// long-poll goroutine on receipt, which then re-attempts its own ClaimTask
		// immediately instead of waiting out the rest of its poll tick. The 250ms
		// ticker in api.go's claimWithWait becomes a rare-case safety net (a missed
		// notification, e.g. across a brief connection drop), not the primary
		// wake mechanism — see notify.go for the listener + broadcast implementation.
		`CREATE OR REPLACE FUNCTION notify_task_available() RETURNS trigger AS $$
		 BEGIN
		   PERFORM pg_notify('cx_task_available', '');
		   RETURN NULL;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS tasks_notify_available ON tasks`,
		`CREATE TRIGGER tasks_notify_available
		   AFTER INSERT OR UPDATE OF status, visible_at ON tasks
		   FOR EACH STATEMENT EXECUTE FUNCTION notify_task_available()`,
		// Cold-load timing (docs/CREED_AND_PATH_TO_TEN.md, "Warm model pool" 6.5→7).
		`ALTER TABLE benchmark_results ADD COLUMN IF NOT EXISTS load_ms BIGINT DEFAULT 0`,
		// Maintained completed-task counter (Control Plane Hot Path 7->8,
		// docs/internal/CREED_AND_PATH_TO_TEN.md): ClaimTask used to re-derive a
		// supplier's lifetime completed-task count with a `count(*)` scan over
		// `tasks` on EVERY single claim (the trusted-tier gate). Now maintained as
		// a running column, incremented once per real commit (CommitTask) instead.
		`ALTER TABLE suppliers ADD COLUMN IF NOT EXISTS completed_tasks BIGINT NOT NULL DEFAULT 0`,
		// One-time backfill for suppliers that already had completed tasks before
		// this column existed. Safe to run on every startup: once backfilled, a
		// real supplier's count is > 0 and this WHERE clause matches nothing for
		// them; it only ever does real work for a genuinely still-zero supplier,
		// where the inner count is cheap (no rows to scan for a fresh worker).
		`UPDATE suppliers s SET completed_tasks = (
		   SELECT count(*) FROM tasks t
		    WHERE t.worker_id IN (SELECT id FROM workers WHERE supplier_id = s.id)
		      AND t.status = 'complete'
		 ) WHERE completed_tasks = 0`,
		// Results-merge watermark (Data Transfer & Artifact I/O 4.5->5,
		// docs/internal/CREED_AND_PATH_TO_TEN.md, "Stop paying for every poll
		// twice"): set once a job's buyer-ready artifact has actually been merged,
		// so GET /v1/jobs/{id}/results only re-merges when no successful merge has
		// happened since completion instead of on every single poll.
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS results_merged_at TIMESTAMPTZ`,
		// Postgres Data Lifecycle 5->6 (docs/internal/CREED_AND_PATH_TO_TEN.md):
		// per-table autovacuum tuning for the telemetry tables sweepTelemetryRetention
		// now DELETEs from hourly (control/workers.go telemetryTables) — mirrors
		// db/schema.sql, here so a deployment that migrated the tables before this rung
		// existed still picks up the tuning on next startup.
		//
		// Since Postgres Data Lifecycle 6->7 these tables are PARTITIONED, and a
		// partitioned PARENT rejects storage parameters (they must sit on the leaves —
		// MigrateTelemetryPartitions below applies them per-leaf). So each ALTER is
		// guarded to run ONLY while the table is still a PLAIN table (relkind 'r'): on
		// an already-partitioned DB it is a no-op (relkind 'p'); on an older DB whose
		// tables are still plain, it tunes them exactly as before, right up until the
		// conversion below carries the same params onto every leaf. Without the guard,
		// a second startup after conversion would error ("cannot specify storage
		// parameters for a partitioned table"). The relkind check makes it idempotent.
		`DO $$ BEGIN
		   IF EXISTS (SELECT 1 FROM pg_class WHERE relname='worker_memory_samples' AND relnamespace='public'::regnamespace AND relkind='r') THEN
		     ALTER TABLE worker_memory_samples SET (
		       autovacuum_vacuum_scale_factor=0.02, autovacuum_vacuum_threshold=200,
		       autovacuum_analyze_scale_factor=0.02, autovacuum_analyze_threshold=200,
		       autovacuum_vacuum_cost_limit=1000);
		   END IF;
		 END $$`,
		`DO $$ BEGIN
		   IF EXISTS (SELECT 1 FROM pg_class WHERE relname='task_durations' AND relnamespace='public'::regnamespace AND relkind='r') THEN
		     ALTER TABLE task_durations SET (
		       autovacuum_vacuum_scale_factor=0.05, autovacuum_vacuum_threshold=100,
		       autovacuum_analyze_scale_factor=0.05, autovacuum_analyze_threshold=100,
		       autovacuum_vacuum_cost_limit=500);
		   END IF;
		 END $$`,
		`DO $$ BEGIN
		   IF EXISTS (SELECT 1 FROM pg_class WHERE relname='job_events' AND relnamespace='public'::regnamespace AND relkind='r') THEN
		     ALTER TABLE job_events SET (
		       autovacuum_vacuum_scale_factor=0.1, autovacuum_vacuum_threshold=100,
		       autovacuum_analyze_scale_factor=0.1, autovacuum_analyze_threshold=100,
		       autovacuum_vacuum_cost_limit=500);
		   END IF;
		 END $$`,
		// Buyer Advantage & Pricing Edge 4.5->5 (docs/internal/CREED_AND_PATH_TO_TEN.md,
		// "Reprice from real supplier economics, not hand-seeded constants"): price
		// provenance columns so a catalogue price is traceable to either the original
		// hand-typed launch constant or a real measured-throughput formula (see
		// control/pricing.go). Mirrors db/schema.sql for a DB that only ran Migrate.
		`ALTER TABLE models ADD COLUMN IF NOT EXISTS price_source TEXT DEFAULT 'seed'`,
		`ALTER TABLE models ADD COLUMN IF NOT EXISTS price_formula TEXT`,
		// Project Detection & Quotation 7->8 ("Ship a firm-quote tier: a real
		// commitment, not just an estimate"): an opt-in per-job flag that caps the
		// buyer's charge at the quote's stated maximum, with overage absorbed by the
		// platform rather than passed through (control/firmquote.go).
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS firm_quote BOOLEAN DEFAULT false`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS firm_quote_max_usd NUMERIC(12,6)`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS billed_usd NUMERIC(12,6)`,
		// Speed Lane wave 2A (docs/speed-lane-reports/SLA_QUOTE_WAVE2A.md): the
		// wall-clock speed-SLA binding + outcome, MIRRORS db/schema.sql (the quotes
		// table's own sla columns live only in schema.sql — quotes itself is
		// schema.sql-owned). sla_guarantee_secs/sla_premium_usd are stamped at
		// submit when firm_quote binds an SLA-bearing quote; sla_met is NULL until
		// the outcome is decided at finalize (true = met, false = missed → an
		// sla_refund ledger row, made once-only by the partial unique index).
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS sla_guarantee_secs INT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS sla_premium_usd NUMERIC(12,6)`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS sla_met BOOLEAN`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ledger_sla_refund_ref_uniq ON ledger_entries (payout_ref) WHERE kind = 'sla_refund'`,
		// Control Plane Hot Path 7->8 ("hoist worker_tps into something computed
		// once per worker state change rather than recomputed per candidate row
		// per claim"): maintained cache, mirrors db/schema.sql for a DB that only
		// ran Migrate. See UpsertWorker (maintains it) and ClaimTaskSQL (reads it).
		`CREATE TABLE IF NOT EXISTS worker_tps_cache (
		   worker_id  UUID NOT NULL,
		   job_type   TEXT NOT NULL,
		   tps        REAL NOT NULL DEFAULT 0,
		   updated_at TIMESTAMPTZ DEFAULT now(),
		   PRIMARY KEY (worker_id, job_type)
		 )`,
		// Control Plane Hot Path 8->9 ("trust a buyer/worker-supplied SHA-256 for
		// redundancy/honeypot comparison where safe, instead of re-downloading
		// bytes the worker just uploaded synchronously inside the commit
		// transaction"): the worker-reported SHA-256 of its own committed result
		// bytes, persisted at CommitTask so a later commit's redundancy compare
		// can trust a hash-to-hash match for byte-exact job types without a
		// second S3 GetObject. Empty/NULL for an older agent that does not send
		// one (or a pre-migration row) — the commit handler's fallback is a real
		// GetObject, so correctness never depends on this column being populated.
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS result_sha256 TEXT`,
		// Verification-before-settlement state machine. CommitTask persists an
		// upload as `verifying`; FinalizeTaskVerification atomically records the
		// verdict, completion counters/telemetry, and ledger rows.
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verification_outcome TEXT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS reported_duration_ms BIGINT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS reported_tokens_used BIGINT`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS reported_hardware_temp_c REAL`,
		`CREATE TABLE IF NOT EXISTS task_verdicts (
		   task_id       UUID NOT NULL REFERENCES tasks ON DELETE CASCADE,
		   attempt       SMALLINT NOT NULL,
		   job_id        UUID NOT NULL REFERENCES jobs ON DELETE CASCADE,
		   supplier_id   UUID REFERENCES suppliers,
		   outcome       TEXT NOT NULL CHECK (outcome IN ('pass','pass_with_penalty','fail','loss_no_payout','clawed_back')),
		   result_sha256 TEXT,
		   created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
		   PRIMARY KEY (task_id, attempt)
		 )`,
		`CREATE INDEX IF NOT EXISTS task_verdicts_job_idx ON task_verdicts (job_id, created_at)`,
		`ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS decision_version INTEGER`,
		`ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS decision_sha256 TEXT`,
		`ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS artifact_key TEXT`,
		`ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS artifact_sha256 TEXT`,
		`CREATE OR REPLACE FUNCTION reject_task_verdict_update() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'task verdict history is immutable'; END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS task_verdicts_no_update ON task_verdicts`,
		`CREATE TRIGGER task_verdicts_no_update BEFORE UPDATE OR DELETE ON task_verdicts
		 FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update()`,
		`DROP TRIGGER IF EXISTS task_execution_history_append_only ON task_execution_history`,
		`CREATE TRIGGER task_execution_history_append_only BEFORE UPDATE OR DELETE ON task_execution_history
		 FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update()`,
		`CREATE TABLE IF NOT EXISTS task_verdict_resolutions (
		   effect_id UUID PRIMARY KEY,
		   task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		   source_task_id UUID REFERENCES tasks(id) ON DELETE SET NULL,
		   kind TEXT NOT NULL CHECK (kind IN ('promoted_pass','clawed_back')),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE INDEX IF NOT EXISTS task_verdict_resolutions_task_idx
		   ON task_verdict_resolutions (task_id,created_at,effect_id)`,
		`DROP TRIGGER IF EXISTS task_verdict_resolutions_append_only ON task_verdict_resolutions`,
		`CREATE TRIGGER task_verdict_resolutions_append_only BEFORE UPDATE OR DELETE ON task_verdict_resolutions
		 FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update()`,
		// Durable verification attempt/work foundation. Staging metadata is immutable
		// but not authoritative; a live fenced lease pins one server-observed artifact
		// tuple before a terminal decision can be recorded.
		`CREATE TABLE IF NOT EXISTS verification_work (
		   id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
		   attempt BIGINT NOT NULL CHECK (attempt >= 0),
		   job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
		   worker_id UUID NOT NULL REFERENCES workers(id) ON DELETE RESTRICT,
		   supplier_id UUID NOT NULL REFERENCES suppliers(id) ON DELETE RESTRICT,
		   snapshot_version SMALLINT NOT NULL CHECK (snapshot_version > 0),
		   input_snapshot JSONB NOT NULL CHECK (jsonb_typeof(input_snapshot)='object'),
		   snapshot_sha256 TEXT NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
		   staged_result_key TEXT NOT NULL CHECK (btrim(staged_result_key) <> ''),
		   reported_result_sha256 TEXT CHECK (reported_result_sha256 ~ '^[0-9a-f]{64}$'),
		   duration_ms BIGINT NOT NULL CHECK (duration_ms >= 0),
		   tokens_used BIGINT NOT NULL CHECK (tokens_used >= 0),
		   hardware_temp_c REAL,
		   sampling_policy TEXT,sampling_probability TEXT,sampling_selected BOOLEAN,
		   status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','leased','terminal')),
		   artifact_key TEXT, artifact_sha256 TEXT, artifact_bytes BIGINT,
		   lease_owner TEXT, lease_token UUID, lease_expires_at TIMESTAMPTZ,
		   lease_attempts INT NOT NULL DEFAULT 0 CHECK (lease_attempts >= 0),
		   next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_error TEXT,
		   terminal_outcome TEXT, decision_sha256 TEXT, terminal_at TIMESTAMPTZ,
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   UNIQUE (task_id,attempt),
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
		 )`,
		`ALTER TABLE verification_work ADD COLUMN IF NOT EXISTS sampling_policy TEXT`,
		`ALTER TABLE verification_work ADD COLUMN IF NOT EXISTS sampling_probability TEXT`,
		`ALTER TABLE verification_work ADD COLUMN IF NOT EXISTS sampling_selected BOOLEAN`,
		`ALTER TABLE verification_work DROP CONSTRAINT IF EXISTS verification_work_sampling_complete`,
		`ALTER TABLE verification_work ADD CONSTRAINT verification_work_sampling_complete CHECK (
		   (sampling_policy IS NULL AND sampling_probability IS NULL AND sampling_selected IS NULL)
		   OR (btrim(sampling_policy) <> '' AND sampling_probability IS NOT NULL AND sampling_selected IS NOT NULL)
		 )`,
		`ALTER TABLE verification_work DROP CONSTRAINT IF EXISTS verification_work_terminal_requires_sampling`,
		`ALTER TABLE verification_work ADD CONSTRAINT verification_work_terminal_requires_sampling
		   CHECK (status<>'terminal' OR sampling_policy IS NOT NULL)`,
		// A replay may be upgrading a legacy in-flight row after this trigger was
		// installed by an earlier binary/schema apply. Startup is not serving work;
		// remove the guard for the narrowly-scoped backfill and recreate it below.
		`DROP TRIGGER IF EXISTS tasks_execution_identity_immutable ON tasks`,
		// Recover exact provenance only from an already-immutable attempt snapshot.
		// For in-flight pre-deploy claims with no snapshot yet, freeze the best
		// available live row once during migration. Completed legacy rows without
		// verification_work deliberately remain unknown instead of being rewritten
		// as if today's mutable worker profile had executed them.
		`UPDATE tasks t
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
		    AND vw.input_snapshot ? 'build_hash'`,
		`UPDATE tasks t
		    SET execution_worker_id=w.id,execution_supplier_id=w.supplier_id,
		        execution_hw_class=w.hw_class,execution_engine=w.engine,
		        execution_build_hash=w.build_hash
		   FROM workers w
		  WHERE t.execution_worker_id IS NULL
		    AND t.status IN ('running','verifying')
		    AND t.worker_id=w.id AND t.claimed_by=w.id
		    AND COALESCE(btrim(w.hw_class),'')<>''
		    AND COALESCE(btrim(w.engine),'')<>''`,
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='tasks_execution_worker_fk') THEN
		     ALTER TABLE tasks ADD CONSTRAINT tasks_execution_worker_fk
		       FOREIGN KEY (execution_worker_id) REFERENCES workers(id) ON DELETE RESTRICT;
		   END IF;
		   IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='tasks_execution_supplier_fk') THEN
		     ALTER TABLE tasks ADD CONSTRAINT tasks_execution_supplier_fk
		       FOREIGN KEY (execution_supplier_id) REFERENCES suppliers(id) ON DELETE RESTRICT;
		   END IF;
		 END $$`,
		`CREATE OR REPLACE FUNCTION cx_protect_task_execution_identity() RETURNS trigger AS $$
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
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS tasks_execution_identity_immutable ON tasks`,
		`CREATE TRIGGER tasks_execution_identity_immutable
		   BEFORE UPDATE OF execution_worker_id,execution_supplier_id,execution_hw_class,
		                    execution_engine,execution_build_hash ON tasks
		   FOR EACH ROW EXECUTE FUNCTION cx_protect_task_execution_identity()`,
		// Freeze the best available class for pre-deploy queued tiebreaks. New rows
		// always write these columns at creation; this one-time/idempotent projection
		// uses only durable work or the claim-frozen execution tuple.
		`UPDATE tasks t
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
		        )) IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS verification_work_pending_idx
		   ON verification_work (status,next_attempt_at,created_at,id)`,
		`CREATE INDEX IF NOT EXISTS verification_work_expired_lease_idx
		   ON verification_work (status,lease_expires_at,id) WHERE status='leased'`,
		`ALTER TABLE task_verdicts ADD COLUMN IF NOT EXISTS verification_work_id UUID REFERENCES verification_work(id)`,
		`CREATE TABLE IF NOT EXISTS chunk_artifact_resolutions (
		   effect_id UUID PRIMARY KEY,
		   job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		   chunk_index INT NOT NULL,
		   winner_task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
		   verification_work_id UUID NOT NULL REFERENCES verification_work(id) ON DELETE RESTRICT,
		   artifact_key TEXT NOT NULL CHECK (btrim(artifact_key) <> ''),
		   artifact_sha256 TEXT NOT NULL CHECK (artifact_sha256 ~ '^[0-9a-f]{64}$'),
		   artifact_bytes BIGINT NOT NULL CHECK (artifact_bytes >= 0),
		   basis TEXT NOT NULL CHECK (basis IN ('provisional','majority')),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`ALTER TABLE chunk_artifact_resolutions DROP CONSTRAINT IF EXISTS chunk_artifact_resolutions_one_basis`,
		`ALTER TABLE chunk_artifact_resolutions ADD CONSTRAINT chunk_artifact_resolutions_one_basis
		   UNIQUE (job_id,chunk_index,basis)`,
		`ALTER TABLE chunk_artifact_resolutions DROP CONSTRAINT IF EXISTS chunk_artifact_resolutions_chunk_nonnegative`,
		`ALTER TABLE chunk_artifact_resolutions ADD CONSTRAINT chunk_artifact_resolutions_chunk_nonnegative
		   CHECK (chunk_index >= 0)`,
		`CREATE INDEX IF NOT EXISTS chunk_artifact_resolutions_lookup_idx
		   ON chunk_artifact_resolutions (job_id,chunk_index,basis,created_at,effect_id)`,
		`CREATE OR REPLACE FUNCTION validate_chunk_artifact_resolution_insert() RETURNS trigger AS $$
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
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS chunk_artifact_resolutions_validate_insert ON chunk_artifact_resolutions`,
		`CREATE TRIGGER chunk_artifact_resolutions_validate_insert BEFORE INSERT ON chunk_artifact_resolutions
		 FOR EACH ROW EXECUTE FUNCTION validate_chunk_artifact_resolution_insert()`,
		`DROP TRIGGER IF EXISTS chunk_artifact_resolutions_append_only ON chunk_artifact_resolutions`,
		`CREATE TRIGGER chunk_artifact_resolutions_append_only BEFORE UPDATE OR DELETE ON chunk_artifact_resolutions
		 FOR EACH ROW EXECUTE FUNCTION reject_task_verdict_update()`,
		`CREATE TABLE IF NOT EXISTS verification_work_plans (
		   work_id UUID PRIMARY KEY REFERENCES verification_work(id) ON DELETE RESTRICT,
		   plan_version SMALLINT NOT NULL CHECK (plan_version > 0),
		   snapshot_sha256 TEXT NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
		   artifact_key TEXT NOT NULL CHECK (btrim(artifact_key) <> ''),
		   artifact_sha256 TEXT NOT NULL CHECK (artifact_sha256 ~ '^[0-9a-f]{64}$'),
		   artifact_bytes BIGINT NOT NULL CHECK (artifact_bytes >= 0),
		   sampling_policy TEXT NOT NULL CHECK (btrim(sampling_policy) <> ''),
		   sampling_probability TEXT NOT NULL,sampling_selected BOOLEAN NOT NULL,
		   decision_json JSONB NOT NULL CHECK (jsonb_typeof(decision_json)='object'),
		   settlement_json JSONB NOT NULL CHECK (jsonb_typeof(settlement_json)='array'),
		   decision_sha256 TEXT NOT NULL CHECK (decision_sha256 ~ '^[0-9a-f]{64}$'),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE OR REPLACE FUNCTION reject_verification_work_plan_update() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'verification work plan is immutable'; END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS verification_work_plans_no_update ON verification_work_plans`,
		`CREATE TRIGGER verification_work_plans_no_update BEFORE UPDATE OR DELETE ON verification_work_plans
		 FOR EACH ROW EXECUTE FUNCTION reject_verification_work_plan_update()`,
		`CREATE OR REPLACE FUNCTION protect_verification_work_identity() RETURNS trigger AS $$
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
		      THEN RAISE EXCEPTION 'terminal verification work is immutable'; END IF;
		   IF OLD.status='pending' AND NEW.status NOT IN ('pending','leased')
		      THEN RAISE EXCEPTION 'verification work must be leased before terminal transition'; END IF;
		   IF OLD.status='leased' AND NEW.status='leased'
		      AND OLD.lease_token IS DISTINCT FROM NEW.lease_token AND OLD.lease_expires_at>now()
		      THEN RAISE EXCEPTION 'live verification lease cannot be stolen'; END IF;
		   RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS verification_work_immutable ON verification_work`,
		`CREATE TRIGGER verification_work_immutable BEFORE UPDATE OR DELETE ON verification_work
		 FOR EACH ROW EXECUTE FUNCTION protect_verification_work_identity()`,
		`CREATE OR REPLACE FUNCTION validate_verification_work_binding() RETURNS trigger AS $$
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
		   ) THEN RAISE EXCEPTION 'verification work does not match the claim-frozen task attempt snapshot'; END IF;
		   RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS verification_work_binding ON verification_work`,
		`CREATE TRIGGER verification_work_binding BEFORE INSERT ON verification_work
		 FOR EACH ROW EXECUTE FUNCTION validate_verification_work_binding()`,
		`ALTER TABLE verification_events ADD COLUMN IF NOT EXISTS effect_id UUID`,
		`ALTER TABLE verification_events ADD COLUMN IF NOT EXISTS attempt SMALLINT`,
		`DROP INDEX IF EXISTS verification_events_effect_uniq`,
		`CREATE UNIQUE INDEX IF NOT EXISTS verification_events_effect_uniq
		   ON verification_events (effect_id)`,
		`DELETE FROM verification_events newer
		 USING verification_events older
		 WHERE newer.effect_id IS NULL AND older.effect_id IS NULL
		   AND newer.task_id IS NOT NULL
		   AND newer.task_id = older.task_id AND newer.kind = older.kind
		   AND (newer.created_at > older.created_at
		        OR (newer.created_at = older.created_at AND newer.id::text > older.id::text))`,
		`DROP INDEX IF EXISTS verification_events_task_kind_uniq`,
		`CREATE UNIQUE INDEX IF NOT EXISTS verification_events_legacy_task_kind_uniq
		   ON verification_events (task_id, kind)
		   WHERE task_id IS NOT NULL AND effect_id IS NULL`,
		// Admin authority provenance. Sessions minted before they were linked to a
		// passkey cannot be attributed honestly, so this migration expires them and
		// requires one fresh login rather than inventing an origin credential.
		`ALTER TABLE admin_credentials ADD COLUMN IF NOT EXISTS revoked BOOLEAN NOT NULL DEFAULT false`,
		`ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS id UUID DEFAULT gen_random_uuid()`,
		`UPDATE admin_sessions SET id=gen_random_uuid() WHERE id IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS admin_sessions_id_uniq ON admin_sessions (id)`,
		`ALTER TABLE admin_sessions ALTER COLUMN id SET NOT NULL`,
		`ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS admin_credential_id UUID
		   REFERENCES admin_credentials(id) ON DELETE RESTRICT`,
		`DELETE FROM admin_sessions WHERE admin_credential_id IS NULL`,
		`ALTER TABLE admin_sessions ALTER COLUMN admin_credential_id SET NOT NULL`,
		`CREATE INDEX IF NOT EXISTS admin_sessions_credential_idx
		   ON admin_sessions (admin_credential_id, expires_at)`,
		// Operator Tooling 7->8 (docs/internal/CREED_AND_PATH_TO_TEN.md, "Add write
		// actions the operator currently has to reach into the database for"): an
		// append-only audit log for every admin write action that used to be a raw
		// psql UPDATE per RUNBOOKS.md (force-requeue a stuck task, adjust a
		// supplier's reputation, release a payout hold). Mirrors the job_events /
		// verification_events append-only pattern already established in this
		// schema: kind is the closed action set, detail carries the operator's
		// free-text reason plus whatever before/after values matter for that action
		// (e.g. old/new reputation), so a later operator can see WHO did WHAT and
		// WHY without re-deriving it from a diff of table state over time.
		`CREATE TABLE IF NOT EXISTS admin_actions (
		   id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		   kind        TEXT NOT NULL,  -- task_requeued|reputation_adjusted|payout_released
		   task_id     UUID,
		   supplier_id UUID,
		   ledger_entry_id UUID,
		   reason      TEXT,
		   detail      JSONB
		 )`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_mode TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_principal_id UUID`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_session_id UUID`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS actor_label TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS attribution_scope TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS intent_version INTEGER`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS request_sha256 TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS correlation_ref TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS target_kind TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS target_id UUID`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS fund_id UUID`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS fund_ref TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS authorization_ref TEXT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS amount_cents BIGINT`,
		`ALTER TABLE admin_actions ADD COLUMN IF NOT EXISTS currency TEXT`,
		`ALTER TABLE admin_actions DROP CONSTRAINT IF EXISTS admin_actions_actor_shape`,
		`ALTER TABLE admin_actions ADD CONSTRAINT admin_actions_actor_shape CHECK (
		   (actor_mode IS NULL AND actor_principal_id IS NULL AND actor_session_id IS NULL
		    AND attribution_scope IS NULL)
		   OR (actor_mode='passkey_session' AND actor_principal_id IS NOT NULL
		       AND actor_session_id IS NOT NULL AND attribution_scope='credential_only')
		   OR (actor_mode='break_glass_api_key' AND actor_principal_id IS NOT NULL
		       AND actor_session_id IS NULL AND attribution_scope='shared_credential_only')
		 ) NOT VALID`,
		`ALTER TABLE admin_actions DROP CONSTRAINT IF EXISTS admin_actions_money_shape`,
		`ALTER TABLE admin_actions ADD CONSTRAINT admin_actions_money_shape CHECK (
		   kind NOT IN ('subsidy_fund_authorized','payout_subsidy_authorized')
		   OR (actor_mode IS NOT NULL AND intent_version=1
		       AND request_sha256 ~ '^[0-9a-f]{64}$'
		       AND correlation_ref IS NOT NULL AND btrim(correlation_ref) <> ''
		       AND target_kind IS NOT NULL AND target_id IS NOT NULL
		       AND fund_id IS NOT NULL AND fund_ref IS NOT NULL AND btrim(fund_ref) <> ''
		       AND amount_cents > 0 AND currency='usd'
		       AND reason IS NOT NULL AND btrim(reason) <> '')
		 ) NOT VALID`,
		`CREATE UNIQUE INDEX IF NOT EXISTS admin_actions_money_correlation_uniq
		   ON admin_actions (kind,correlation_ref)
		   WHERE kind IN ('subsidy_fund_authorized','payout_subsidy_authorized')`,
		`CREATE OR REPLACE FUNCTION reject_admin_action_mutation()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   RAISE EXCEPTION 'admin actions are append-only';
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS admin_actions_append_only ON admin_actions`,
		`CREATE TRIGGER admin_actions_append_only
		 BEFORE UPDATE OR DELETE ON admin_actions
		 FOR EACH ROW EXECUTE FUNCTION reject_admin_action_mutation()`,
		// Pricing/economics cash-safety tranche (kept as one distinct additive
		// section): exact card minor units and a durable supplier-payout operation
		// whose sending/reversal state cannot be erased by a racing status write.
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_requested_cents BIGINT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_received_cents BIGINT`,
		`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS charge_currency TEXT`,
		`ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_requested_cents BIGINT`,
		`ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_received_cents BIGINT`,
		`ALTER TABLE charge_batches ADD COLUMN IF NOT EXISTS charge_currency TEXT`,
		`CREATE TABLE IF NOT EXISTS buyer_cash_collections (
		   payment_intent TEXT PRIMARY KEY CHECK (btrim(payment_intent) <> ''),
		   charge_id TEXT UNIQUE CHECK (charge_id IS NULL OR btrim(charge_id) <> ''),
		   buyer_id UUID NOT NULL,
		   source_kind TEXT NOT NULL CHECK (source_kind IN ('job','batch')),
		   job_id UUID UNIQUE REFERENCES jobs(id),
		   charge_batch_id UUID UNIQUE REFERENCES charge_batches(id),
		   requested_cents BIGINT NOT NULL CHECK (requested_cents > 0),
		   received_cents BIGINT NOT NULL CHECK (received_cents > 0),
		   currency TEXT NOT NULL CHECK (currency = 'usd'),
		   recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   CHECK (requested_cents = received_cents),
		   CHECK ((source_kind='job' AND job_id IS NOT NULL AND charge_batch_id IS NULL)
		       OR (source_kind='batch' AND charge_batch_id IS NOT NULL AND job_id IS NULL))
		 )`,
		`ALTER TABLE buyer_cash_collections ADD COLUMN IF NOT EXISTS charge_id TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS buyer_cash_collections_charge_id_uniq
		   ON buyer_cash_collections (charge_id) WHERE charge_id IS NOT NULL`,
		// Only exact historical confirmations are canonicalized. A legacy charged
		// row without PI + matching positive minor-unit fields remains unfunded.
		`INSERT INTO buyer_cash_collections
		   (payment_intent,buyer_id,source_kind,job_id,requested_cents,received_cents,currency)
		 SELECT stripe_pi,buyer_id,'job',id,charge_requested_cents,charge_received_cents,charge_currency
		   FROM jobs
		  WHERE charge_status='charged' AND COALESCE(stripe_pi,'') <> ''
		    AND charge_requested_cents > 0
		    AND charge_received_cents=charge_requested_cents AND charge_currency='usd'
		 ON CONFLICT DO NOTHING`,
		`INSERT INTO buyer_cash_collections
		   (payment_intent,buyer_id,source_kind,charge_batch_id,requested_cents,received_cents,currency)
		 SELECT stripe_pi,buyer_id,'batch',id,charge_requested_cents,charge_received_cents,charge_currency
		   FROM charge_batches
		  WHERE status='charged' AND COALESCE(stripe_pi,'') <> ''
		    AND charge_requested_cents > 0
		    AND charge_received_cents=charge_requested_cents AND charge_currency='usd'
		 ON CONFLICT DO NOTHING`,
		// Signed Stripe cash-event inbox and object state. Event ids plus payload
		// digests make delivery replay idempotent and conflicting reuse fatal. Charge
		// refunds are cumulative; dispute cash availability is ordered by Stripe's
		// event creation time rather than webhook arrival order.
		`CREATE TABLE IF NOT EXISTS stripe_webhook_events (
		   event_id TEXT PRIMARY KEY CHECK (btrim(event_id) <> ''),
		   event_type TEXT NOT NULL CHECK (event_type IN (
		     'charge.refunded','charge.dispute.created','charge.dispute.funds_withdrawn',
		     'charge.dispute.funds_reinstated','charge.dispute.closed')),
		   object_id TEXT NOT NULL CHECK (btrim(object_id) <> ''),
		   charge_id TEXT NOT NULL CHECK (btrim(charge_id) <> ''),
		   payment_intent TEXT CHECK (payment_intent IS NULL OR btrim(payment_intent) <> ''),
		   event_created BIGINT NOT NULL CHECK (event_created > 0),
		   payload_sha256 TEXT NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
		   recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE INDEX IF NOT EXISTS stripe_webhook_events_object_idx
		   ON stripe_webhook_events (event_type,object_id,event_created)`,
		`CREATE TABLE IF NOT EXISTS stripe_charge_cash_state (
		   charge_id TEXT PRIMARY KEY CHECK (btrim(charge_id) <> ''),
		   payment_intent TEXT CHECK (payment_intent IS NULL OR btrim(payment_intent) <> ''),
		   amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
		   refunded_cents BIGINT NOT NULL CHECK (refunded_cents > 0 AND refunded_cents <= amount_cents),
		   currency TEXT NOT NULL CHECK (btrim(currency) <> ''),
		   last_event_id TEXT NOT NULL REFERENCES stripe_webhook_events(event_id) ON DELETE RESTRICT,
		   last_event_created BIGINT NOT NULL CHECK (last_event_created > 0),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE INDEX IF NOT EXISTS stripe_charge_cash_state_pi_idx
		   ON stripe_charge_cash_state (payment_intent) WHERE payment_intent IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS stripe_dispute_cash_state (
		   dispute_id TEXT PRIMARY KEY CHECK (btrim(dispute_id) <> ''),
		   charge_id TEXT NOT NULL CHECK (btrim(charge_id) <> ''),
		   payment_intent TEXT CHECK (payment_intent IS NULL OR btrim(payment_intent) <> ''),
		   amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
		   currency TEXT NOT NULL CHECK (btrim(currency) <> ''),
		   status TEXT NOT NULL CHECK (btrim(status) <> ''),
		   cash_unavailable BOOLEAN NOT NULL DEFAULT false,
		   cash_effect_created BIGINT NOT NULL DEFAULT 0 CHECK (cash_effect_created >= 0),
		   cash_effect_rank INTEGER NOT NULL DEFAULT 0 CHECK (cash_effect_rank >= 0),
		   last_event_id TEXT NOT NULL REFERENCES stripe_webhook_events(event_id) ON DELETE RESTRICT,
		   last_event_type TEXT NOT NULL,
		   last_event_created BIGINT NOT NULL CHECK (last_event_created > 0),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`CREATE INDEX IF NOT EXISTS stripe_dispute_cash_state_pi_unavailable_idx
		   ON stripe_dispute_cash_state (payment_intent)
		   WHERE payment_intent IS NOT NULL AND cash_unavailable`,
		`CREATE TABLE IF NOT EXISTS platform_subsidy_funds (
		   id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   authorization_action_id UUID NOT NULL UNIQUE REFERENCES admin_actions(id) ON DELETE RESTRICT,
		   fund_ref TEXT NOT NULL UNIQUE CHECK (btrim(fund_ref) <> ''),
		   external_treasury_ref TEXT NOT NULL UNIQUE CHECK (btrim(external_treasury_ref) <> ''),
		   authorized_cents BIGINT NOT NULL CHECK (authorized_cents > 0),
		   currency TEXT NOT NULL CHECK (currency = 'usd'),
		   reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
		   status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','closed')),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`,
		`ALTER TABLE platform_subsidy_funds ADD COLUMN IF NOT EXISTS authorization_action_id UUID`,
		`CREATE UNIQUE INDEX IF NOT EXISTS platform_subsidy_funds_authorization_action_uniq
		   ON platform_subsidy_funds (authorization_action_id)
		   WHERE authorization_action_id IS NOT NULL`,
		`ALTER TABLE platform_subsidy_funds DROP CONSTRAINT IF EXISTS platform_subsidy_funds_authorization_action_fkey`,
		`ALTER TABLE platform_subsidy_funds ADD CONSTRAINT platform_subsidy_funds_authorization_action_fkey
		   FOREIGN KEY (authorization_action_id) REFERENCES admin_actions(id) ON DELETE RESTRICT NOT VALID`,
		`ALTER TABLE platform_subsidy_funds DROP CONSTRAINT IF EXISTS platform_subsidy_funds_authorization_required`,
		`ALTER TABLE platform_subsidy_funds ADD CONSTRAINT platform_subsidy_funds_authorization_required
		   CHECK (authorization_action_id IS NOT NULL) NOT VALID`,
		`CREATE TABLE IF NOT EXISTS supplier_payout_funding (
		   id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   authorization_action_id UUID UNIQUE REFERENCES admin_actions(id) ON DELETE RESTRICT,
		   ledger_entry_id UUID NOT NULL UNIQUE REFERENCES ledger_entries(id),
		   source_kind TEXT NOT NULL CHECK (source_kind IN ('buyer_collection','platform_subsidy')),
		   liability_job_id UUID REFERENCES jobs(id),
		   collection_payment_intent TEXT REFERENCES buyer_cash_collections(payment_intent),
		   subsidy_fund_id UUID REFERENCES platform_subsidy_funds(id),
		   subsidy_authorization_ref TEXT UNIQUE,
		   subsidy_reason TEXT,
		   amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
		   currency TEXT NOT NULL CHECK (currency = 'usd'),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   CONSTRAINT supplier_payout_funding_source_valid CHECK (
		     (source_kind='buyer_collection' AND liability_job_id IS NOT NULL
		      AND collection_payment_intent IS NOT NULL
		      AND subsidy_fund_id IS NULL
		      AND subsidy_authorization_ref IS NULL AND subsidy_reason IS NULL)
		     OR
		     (source_kind='platform_subsidy' AND collection_payment_intent IS NULL
		      AND subsidy_fund_id IS NOT NULL
		      AND subsidy_authorization_ref IS NOT NULL AND btrim(subsidy_authorization_ref) <> ''
		      AND subsidy_reason IS NOT NULL AND btrim(subsidy_reason) <> '')
		   )
		 )`,
		`CREATE INDEX IF NOT EXISTS supplier_payout_funding_collection_idx
		   ON supplier_payout_funding (collection_payment_intent)
		   WHERE source_kind='buyer_collection'`,
		`CREATE INDEX IF NOT EXISTS supplier_payout_funding_subsidy_fund_idx
		   ON supplier_payout_funding (subsidy_fund_id)
		   WHERE source_kind='platform_subsidy'`,
		`ALTER TABLE supplier_payout_funding ADD COLUMN IF NOT EXISTS subsidy_fund_id UUID
		   REFERENCES platform_subsidy_funds(id)`,
		`ALTER TABLE supplier_payout_funding ADD COLUMN IF NOT EXISTS authorization_action_id UUID`,
		`CREATE UNIQUE INDEX IF NOT EXISTS supplier_payout_funding_authorization_action_uniq
		   ON supplier_payout_funding (authorization_action_id)
		   WHERE authorization_action_id IS NOT NULL`,
		`ALTER TABLE supplier_payout_funding DROP CONSTRAINT IF EXISTS supplier_payout_funding_authorization_action_fkey`,
		`ALTER TABLE supplier_payout_funding ADD CONSTRAINT supplier_payout_funding_authorization_action_fkey
		   FOREIGN KEY (authorization_action_id) REFERENCES admin_actions(id) ON DELETE RESTRICT NOT VALID`,
		`ALTER TABLE supplier_payout_funding DROP CONSTRAINT IF EXISTS supplier_payout_funding_source_valid`,
		`ALTER TABLE supplier_payout_funding ADD CONSTRAINT supplier_payout_funding_source_valid CHECK (
		   (source_kind='buyer_collection' AND liability_job_id IS NOT NULL
		    AND collection_payment_intent IS NOT NULL AND subsidy_fund_id IS NULL
		    AND authorization_action_id IS NULL
		    AND subsidy_authorization_ref IS NULL AND subsidy_reason IS NULL)
		   OR
		   (source_kind='platform_subsidy' AND collection_payment_intent IS NULL
		    AND subsidy_fund_id IS NOT NULL
		    AND authorization_action_id IS NOT NULL
		    AND subsidy_authorization_ref IS NOT NULL AND btrim(subsidy_authorization_ref) <> ''
		    AND subsidy_reason IS NOT NULL AND btrim(subsidy_reason) <> '')
		 ) NOT VALID`,
		// Current impairment of an immutable funding reservation. The reservation row
		// remains append-only; this event-derived state says whether Stripe cash still
		// covers it and which signed event last changed that conclusion.
		`CREATE TABLE IF NOT EXISTS supplier_payout_funding_state (
		   funding_id UUID PRIMARY KEY REFERENCES supplier_payout_funding(id) ON DELETE RESTRICT,
		   status TEXT NOT NULL CHECK (status IN ('available','compromised')),
		   compromised_cents BIGINT NOT NULL CHECK (compromised_cents >= 0),
		   last_event_id TEXT NOT NULL REFERENCES stripe_webhook_events(event_id) ON DELETE RESTRICT,
		   reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   CHECK ((status='available' AND compromised_cents=0)
		       OR (status='compromised' AND compromised_cents>0))
		 )`,
		`CREATE INDEX IF NOT EXISTS supplier_payout_funding_state_compromised_idx
		   ON supplier_payout_funding_state (updated_at,funding_id) WHERE status='compromised'`,
		`CREATE TABLE IF NOT EXISTS supplier_minor_unit_settlements (
		   ledger_entry_id UUID PRIMARY KEY REFERENCES ledger_entries(id) ON DELETE RESTRICT,
		   policy TEXT NOT NULL CHECK (policy='floor_cent_carry_v1'),
		   liability_microusd BIGINT NOT NULL CHECK (liability_microusd >= 0),
		   cash_cents BIGINT NOT NULL CHECK (cash_cents >= 0),
		   remainder_microusd BIGINT NOT NULL CHECK (
		     remainder_microusd >= 0 AND remainder_microusd < 10000),
		   currency TEXT NOT NULL DEFAULT 'usd' CHECK (currency='usd'),
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   CHECK (liability_microusd = cash_cents * 10000 + remainder_microusd)
		 )`,
		`CREATE OR REPLACE FUNCTION validate_minor_unit_settlement_binding()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 DECLARE ledger_kind TEXT; ledger_microusd BIGINT;
		 BEGIN
		   SELECT kind,(amount_usd*1000000)::bigint INTO ledger_kind,ledger_microusd
		     FROM ledger_entries WHERE id=NEW.ledger_entry_id;
		   IF ledger_kind IS DISTINCT FROM 'supplier_credit'
		      OR ledger_microusd IS DISTINCT FROM NEW.liability_microusd THEN
		     RAISE EXCEPTION 'minor-unit settlement does not match supplier-credit liability';
		   END IF;
		   RETURN NEW;
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS supplier_minor_unit_settlement_binding
		   ON supplier_minor_unit_settlements`,
		`CREATE TRIGGER supplier_minor_unit_settlement_binding
		 BEFORE INSERT ON supplier_minor_unit_settlements
		 FOR EACH ROW EXECUTE FUNCTION validate_minor_unit_settlement_binding()`,
		`CREATE OR REPLACE FUNCTION reject_settled_ledger_money_mutation()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF (OLD.kind,OLD.amount_usd) IS DISTINCT FROM (NEW.kind,NEW.amount_usd)
		      AND EXISTS (SELECT 1 FROM supplier_minor_unit_settlements
		                   WHERE ledger_entry_id=OLD.id) THEN
		     RAISE EXCEPTION 'settled supplier liability identity is immutable';
		   END IF;
		   RETURN NEW;
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS settled_ledger_money_immutable ON ledger_entries`,
		`CREATE TRIGGER settled_ledger_money_immutable
		 BEFORE UPDATE OF kind,amount_usd ON ledger_entries
		 FOR EACH ROW EXECUTE FUNCTION reject_settled_ledger_money_mutation()`,
		`CREATE OR REPLACE FUNCTION reject_minor_unit_settlement_mutation()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   RAISE EXCEPTION 'supplier minor-unit settlements are append-only';
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS supplier_minor_unit_settlement_append_only
		   ON supplier_minor_unit_settlements`,
		`CREATE TRIGGER supplier_minor_unit_settlement_append_only
		 BEFORE UPDATE OR DELETE ON supplier_minor_unit_settlements
		 FOR EACH ROW EXECUTE FUNCTION reject_minor_unit_settlement_mutation()`,
		`CREATE TABLE IF NOT EXISTS supplier_payout_operations (
		   id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		   ledger_entry_id UUID NOT NULL UNIQUE REFERENCES ledger_entries(id) ON DELETE RESTRICT,
		   funding_id UUID REFERENCES supplier_payout_funding(id),
		   supplier_id UUID NOT NULL REFERENCES suppliers(id),
		   requested_cents BIGINT NOT NULL CHECK (requested_cents > 0),
		   sent_cents BIGINT CHECK (sent_cents > 0),
		   currency TEXT NOT NULL DEFAULT 'usd' CHECK (currency = 'usd'),
		   status TEXT NOT NULL CHECK (status IN (
		     'sending','ready','outcome_unknown','released','exported','clawed_back','reversal_required','reversed')),
		   cash_moved BOOLEAN NOT NULL DEFAULT false,
		   outcome_unknown BOOLEAN NOT NULL DEFAULT false,
		   transfer_ref TEXT,
		   last_error TEXT,
		   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   CHECK (NOT cash_moved OR (sent_cents IS NOT NULL AND transfer_ref IS NOT NULL)),
		   CHECK (NOT outcome_unknown OR NOT cash_moved)
		 )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS supplier_payout_operations_transfer_ref_uniq
		   ON supplier_payout_operations (transfer_ref) WHERE transfer_ref IS NOT NULL AND cash_moved`,
		`CREATE INDEX IF NOT EXISTS supplier_payout_operations_status_idx
		   ON supplier_payout_operations (status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS ledger_due_payout_idx
		   ON ledger_entries (release_at,id)
		   WHERE kind='supplier_credit' AND payout_status='held' AND release_at IS NOT NULL`,
		`ALTER TABLE supplier_payout_operations ADD COLUMN IF NOT EXISTS funding_id UUID
		   REFERENCES supplier_payout_funding(id)`,
		`ALTER TABLE supplier_payout_operations ADD COLUMN IF NOT EXISTS outcome_unknown BOOLEAN
		   NOT NULL DEFAULT false`,
		`ALTER TABLE supplier_payout_operations DROP CONSTRAINT IF EXISTS supplier_payout_operations_status_check`,
		`ALTER TABLE supplier_payout_operations ADD CONSTRAINT supplier_payout_operations_status_check
		   CHECK (status IN ('sending','ready','outcome_unknown','released','exported',
		                     'clawed_back','reversal_required','reversed'))`,
		`ALTER TABLE supplier_payout_operations DROP CONSTRAINT IF EXISTS supplier_payout_operations_outcome_unknown_check`,
		`ALTER TABLE supplier_payout_operations ADD CONSTRAINT supplier_payout_operations_outcome_unknown_check
		   CHECK (NOT outcome_unknown OR NOT cash_moved)`,
		`ALTER TABLE supplier_payout_operations DROP CONSTRAINT IF EXISTS supplier_payout_operations_ledger_entry_id_fkey`,
		`ALTER TABLE supplier_payout_operations ADD CONSTRAINT supplier_payout_operations_ledger_entry_id_fkey
		   FOREIGN KEY (ledger_entry_id) REFERENCES ledger_entries(id) ON DELETE RESTRICT`,
		`CREATE OR REPLACE FUNCTION reject_immutable_money_fact_mutation()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF TG_OP = 'DELETE' OR OLD IS DISTINCT FROM NEW THEN
		     RAISE EXCEPTION '% rows are append-only', TG_TABLE_NAME;
		   END IF;
		   RETURN NEW;
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS buyer_cash_collections_append_only ON buyer_cash_collections`,
		`CREATE TRIGGER buyer_cash_collections_append_only
		 BEFORE UPDATE OR DELETE ON buyer_cash_collections
		 FOR EACH ROW EXECUTE FUNCTION reject_immutable_money_fact_mutation()`,
		`DROP TRIGGER IF EXISTS stripe_webhook_events_append_only ON stripe_webhook_events`,
		`CREATE TRIGGER stripe_webhook_events_append_only
		 BEFORE UPDATE OR DELETE ON stripe_webhook_events
		 FOR EACH ROW EXECUTE FUNCTION reject_immutable_money_fact_mutation()`,
		`CREATE OR REPLACE FUNCTION protect_buyer_charge_operation()
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
		 $$`,
		`DROP TRIGGER IF EXISTS buyer_charge_operation_identity_immutable ON buyer_charge_operations`,
		`CREATE TRIGGER buyer_charge_operation_identity_immutable
		 BEFORE UPDATE OR DELETE ON buyer_charge_operations
		 FOR EACH ROW EXECUTE FUNCTION protect_buyer_charge_operation()`,
		`DROP TRIGGER IF EXISTS supplier_payout_funding_append_only ON supplier_payout_funding`,
		`CREATE TRIGGER supplier_payout_funding_append_only
		 BEFORE UPDATE OR DELETE ON supplier_payout_funding
		 FOR EACH ROW EXECUTE FUNCTION reject_immutable_money_fact_mutation()`,
		`CREATE OR REPLACE FUNCTION protect_subsidy_fund_identity()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF TG_OP='DELETE' THEN
		     RAISE EXCEPTION 'platform subsidy funds cannot be deleted';
		   END IF;
		   IF (OLD.id,OLD.authorization_action_id,OLD.fund_ref,OLD.external_treasury_ref,
		       OLD.authorized_cents,OLD.currency,OLD.reason,OLD.created_at)
		      IS DISTINCT FROM
		      (NEW.id,NEW.authorization_action_id,NEW.fund_ref,NEW.external_treasury_ref,
		       NEW.authorized_cents,NEW.currency,NEW.reason,NEW.created_at) THEN
		     RAISE EXCEPTION 'platform subsidy fund authorization identity is immutable';
		   END IF;
		   IF OLD.status='closed' AND NEW.status<>'closed' THEN
		     RAISE EXCEPTION 'closed platform subsidy funds cannot be reopened';
		   END IF;
		   RETURN NEW;
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS platform_subsidy_funds_identity_immutable ON platform_subsidy_funds`,
		`CREATE TRIGGER platform_subsidy_funds_identity_immutable
		 BEFORE UPDATE OR DELETE ON platform_subsidy_funds
		 FOR EACH ROW EXECUTE FUNCTION protect_subsidy_fund_identity()`,
		`CREATE OR REPLACE FUNCTION validate_money_authority_action_binding()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 DECLARE binding_ok BOOLEAN;
		 BEGIN
		   IF NEW.kind IN ('subsidy_fund_authorized','payout_subsidy_authorized') THEN
		     IF NEW.actor_mode='passkey_session' THEN
		       SELECT EXISTS (
		         SELECT 1 FROM admin_sessions s
		         JOIN admin_credentials c ON c.id=s.admin_credential_id
		          WHERE s.id=NEW.actor_session_id AND c.id=NEW.actor_principal_id
		            AND s.revoked=false AND s.expires_at>clock_timestamp()
		            AND c.revoked=false
		       ) INTO binding_ok;
		     ELSIF NEW.actor_mode='break_glass_api_key' THEN
		       SELECT EXISTS (
		         SELECT 1 FROM api_keys k
		          WHERE k.id=NEW.actor_principal_id
		            AND k.is_admin=true AND k.revoked=false
		       ) INTO binding_ok;
		     ELSE
		       binding_ok:=false;
		     END IF;
		     IF NOT COALESCE(binding_ok,false) THEN
		       RAISE EXCEPTION 'money authority action % has no live authenticated credential binding',NEW.id;
		     END IF;
		   END IF;
		   IF NEW.kind='subsidy_fund_authorized' THEN
		     SELECT EXISTS (
		       SELECT 1 FROM platform_subsidy_funds f
		        WHERE f.authorization_action_id=NEW.id
		          AND NEW.target_kind='subsidy_fund'
		          AND NEW.target_id=f.id AND NEW.fund_id=f.id
		          AND NEW.fund_ref=f.fund_ref AND NEW.authorization_ref IS NULL
		          AND NEW.amount_cents=f.authorized_cents
		          AND NEW.currency=f.currency AND NEW.reason=f.reason
		     ) INTO binding_ok;
		   ELSIF NEW.kind='payout_subsidy_authorized' THEN
		     SELECT EXISTS (
		       SELECT 1 FROM supplier_payout_funding p
		       JOIN platform_subsidy_funds f ON f.id=p.subsidy_fund_id
		        WHERE p.authorization_action_id=NEW.id
		          AND p.source_kind='platform_subsidy'
		          AND NEW.target_kind='supplier_liability'
		          AND NEW.target_id=p.ledger_entry_id
		          AND NEW.ledger_entry_id=p.ledger_entry_id
		          AND NEW.fund_id=f.id AND NEW.fund_ref=f.fund_ref
		          AND NEW.authorization_ref=p.subsidy_authorization_ref
		          AND NEW.amount_cents=p.amount_cents
		          AND NEW.currency=p.currency AND NEW.reason=p.subsidy_reason
		     ) INTO binding_ok;
		   ELSE
		     RETURN NEW;
		   END IF;
		   IF NOT COALESCE(binding_ok,false) THEN
		     RAISE EXCEPTION 'money authority action % has no exact resource binding',NEW.id;
		   END IF;
		   RETURN NEW;
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS admin_actions_money_binding ON admin_actions`,
		`CREATE CONSTRAINT TRIGGER admin_actions_money_binding
		 AFTER INSERT ON admin_actions DEFERRABLE INITIALLY DEFERRED
		 FOR EACH ROW EXECUTE FUNCTION validate_money_authority_action_binding()`,
		`CREATE OR REPLACE FUNCTION validate_money_authority_resource_binding()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 DECLARE binding_ok BOOLEAN;
		 BEGIN
		   IF TG_TABLE_NAME='platform_subsidy_funds' THEN
		     SELECT EXISTS (
		       SELECT 1 FROM admin_actions a
		        WHERE a.id=NEW.authorization_action_id
		          AND a.kind='subsidy_fund_authorized'
		          AND a.target_kind='subsidy_fund'
		          AND a.target_id=NEW.id AND a.fund_id=NEW.id
		          AND a.fund_ref=NEW.fund_ref AND a.authorization_ref IS NULL
		          AND a.amount_cents=NEW.authorized_cents
		          AND a.currency=NEW.currency AND a.reason=NEW.reason
		     ) INTO binding_ok;
		   ELSE
		     IF NEW.source_kind<>'platform_subsidy' THEN
		       RETURN NEW;
		     END IF;
		     SELECT EXISTS (
		       SELECT 1 FROM admin_actions a
		       JOIN platform_subsidy_funds f ON f.id=NEW.subsidy_fund_id
		        WHERE a.id=NEW.authorization_action_id
		          AND a.kind='payout_subsidy_authorized'
		          AND a.target_kind='supplier_liability'
		          AND a.target_id=NEW.ledger_entry_id
		          AND a.ledger_entry_id=NEW.ledger_entry_id
		          AND a.fund_id=f.id AND a.fund_ref=f.fund_ref
		          AND a.authorization_ref=NEW.subsidy_authorization_ref
		          AND a.amount_cents=NEW.amount_cents
		          AND a.currency=NEW.currency AND a.reason=NEW.subsidy_reason
		     ) INTO binding_ok;
		   END IF;
		   IF NOT COALESCE(binding_ok,false) THEN
		     RAISE EXCEPTION '% row has no exact money authority action binding',TG_TABLE_NAME;
		   END IF;
		   RETURN NEW;
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS platform_subsidy_funds_money_binding ON platform_subsidy_funds`,
		`CREATE CONSTRAINT TRIGGER platform_subsidy_funds_money_binding
		 AFTER INSERT ON platform_subsidy_funds DEFERRABLE INITIALLY DEFERRED
		 FOR EACH ROW EXECUTE FUNCTION validate_money_authority_resource_binding()`,
		`DROP TRIGGER IF EXISTS supplier_payout_funding_money_binding ON supplier_payout_funding`,
		`CREATE CONSTRAINT TRIGGER supplier_payout_funding_money_binding
		 AFTER INSERT ON supplier_payout_funding DEFERRABLE INITIALLY DEFERRED
		 FOR EACH ROW EXECUTE FUNCTION validate_money_authority_resource_binding()`,
		`CREATE OR REPLACE FUNCTION reject_payout_operation_identity_mutation()
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
		 $$`,
		`DROP TRIGGER IF EXISTS supplier_payout_operation_identity_immutable
		   ON supplier_payout_operations`,
		`CREATE TRIGGER supplier_payout_operation_identity_immutable
		 BEFORE UPDATE ON supplier_payout_operations
		 FOR EACH ROW EXECUTE FUNCTION reject_payout_operation_identity_mutation()`,
		`CREATE OR REPLACE FUNCTION reject_payout_operation_delete()
		 RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   RAISE EXCEPTION 'supplier payout operations are append-only state records';
		 END;
		 $$`,
		`DROP TRIGGER IF EXISTS supplier_payout_operation_no_delete
		   ON supplier_payout_operations`,
		`CREATE TRIGGER supplier_payout_operation_no_delete
		 BEFORE DELETE ON supplier_payout_operations
		 FOR EACH ROW EXECUTE FUNCTION reject_payout_operation_delete()`,
	}
	tx, err := migrationConn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin: %w", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("migrate: %q: %w", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit schema changes: %w", err)
	}
	// Postgres Data Lifecycle 6->7 (docs/internal/CREED_AND_PATH_TO_TEN.md): convert the
	// three high-churn telemetry tables to declarative RANGE partitioning by created_at,
	// in place, preserving every existing row (control/partition.go). Runs LAST — after
	// the base tables above exist (or were just created) — and is idempotent: a table
	// already partitioned is a no-op, so this is safe on every startup. A failure here
	// is fatal at startup exactly like every statement above; a half-migrated table is
	// impossible because each table's conversion is its own transaction.
	if err := migrateTelemetryPartitions(ctx, migrationConn); err != nil {
		return fmt.Errorf("migrate: telemetry partitions: %w", err)
	}
	// Reconciliation uses the pool and must also work with a one-connection test
	// or maintenance pool, so return the locked connection before running it.
	releaseMigrationLock()
	// Deployments predating durable verification_work may have crashed after
	// flipping a task to verifying but before any recoverable attempt snapshot
	// existed. Retry those uploads safely before the server accepts traffic; never
	// synthesize a verdict or settlement from the incomplete legacy projection.
	if _, err := s.ReconcileLegacyVerifyingTasks(ctx); err != nil {
		return fmt.Errorf("migrate: reconcile legacy verification: %w", err)
	}
	return nil
}

// errNotFound is returned when a lookup matches no row.
var errNotFound = errors.New("not found")

// --- buyer billing data access (billing_customers) ---

// errOAuthLinkStateInvalid deliberately collapses missing, expired, wrong-browser,
// and already-consumed states into one result. Callers must not expose which part
// of an OAuth capability was valid.
var errOAuthLinkStateInvalid = errors.New("invalid or expired OAuth link state")

const (
	maxOAuthLinkCapabilityBytes = 256
	maxOAuthLinkStateLifetime   = 15 * time.Minute
)

// GetBillingCustomer returns the buyer's Stripe customer id + default payment
// method (both "" if none); errNotFound when the buyer has no billing row yet.
func (s *Store) GetBillingCustomer(ctx context.Context, buyerID uuid.UUID) (custID, pm string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(stripe_customer_id,''), COALESCE(default_payment_method,'')
		   FROM billing_customers WHERE buyer_id=$1`, buyerID).Scan(&custID, &pm)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errNotFound
	}
	return custID, pm, err
}

// UpsertBillingCustomer maps a buyer to a Stripe customer id (idempotent).
func (s *Store) UpsertBillingCustomer(ctx context.Context, buyerID uuid.UUID, custID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO billing_customers (buyer_id, stripe_customer_id) VALUES ($1, $2)
		   ON CONFLICT (buyer_id) DO UPDATE SET stripe_customer_id = EXCLUDED.stripe_customer_id`,
		buyerID, custID)
	return err
}

// SetBillingPMByCustomer records a buyer's default payment method, keyed by their
// Stripe customer id (the webhook's view of who they are).
func (s *Store) SetBillingPMByCustomer(ctx context.Context, custID, pm string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE billing_customers SET default_payment_method=$2 WHERE stripe_customer_id=$1`, custID, pm)
	if err != nil {
		return err
	}
	return validateBillingPMUpdateCount(tag.RowsAffected())
}

func validateBillingPMUpdateCount(rows int64) error {
	switch rows {
	case 1:
		return nil
	case 0:
		return errNotFound
	default:
		// The unique partial index makes this impossible after migration; retain the
		// check as defense-in-depth for a partially migrated or externally altered DB.
		return fmt.Errorf("billing customer mapping matched %d rows, want exactly one", rows)
	}
}

// JobChargeInfo returns a job's buyer + the amount to actually CHARGE it (for the
// auto-charge). This is normally the settled actual_usd unchanged — but for a
// firm-quote job (Project Detection & Quotation 7->8,
// docs/internal/CREED_AND_PATH_TO_TEN.md, "a real commitment, not just an
// estimate") the charge is capped at firm_quote_max_usd: the buyer is NEVER
// charged more than the quoted maximum they committed budget against, even when
// the real actual_usd (what suppliers actually earned for the real work they
// did — untouched, see billing.go/CommitTask) exceeds it. The platform absorbs
// that difference; nothing here reduces what a supplier is owed.
func (s *Store) JobChargeInfo(ctx context.Context, jobID uuid.UUID) (buyerID uuid.UUID, chargeUSD float64, err error) {
	var actualUSD float64
	var firmQuote bool
	var firmMax float64
	var slaRefund float64
	// The sla_refund subquery is the Go-side twin of collect.go's
	// firmChargeAmountSQL netting (Speed Lane wave 2A): a missed speed-SLA's
	// premium refund (an sla_refund ledger credit keyed 'sla-<job_id>') comes off
	// the amount actually collected, on BOTH charge paths, so a refund can never
	// be bypassed by which path happens to collect the job.
	err = s.pool.QueryRow(ctx,
		`SELECT buyer_id, COALESCE(actual_usd,0), firm_quote, COALESCE(firm_quote_max_usd,0),
		        COALESCE((SELECT SUM(le.amount_usd) FROM ledger_entries le
		                  WHERE le.kind = 'sla_refund'
		                    AND le.payout_ref = 'sla-' || jobs.id::text), 0)::float8
		   FROM jobs WHERE id=$1`,
		jobID).Scan(&buyerID, &actualUSD, &firmQuote, &firmMax, &slaRefund)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
		return
	}
	if err != nil {
		return
	}
	chargeUSD = actualUSD
	if firmQuote && firmMax > 0 && actualUSD > firmMax {
		chargeUSD = firmMax
	}
	if slaRefund > 0 {
		chargeUSD -= slaRefund
		if chargeUSD < 0 {
			chargeUSD = 0 // the remedy nets the bill down, never into a negative charge
		}
	}
	return
}

// SetSupplierStripeAcct records a supplier's Connect account id (the payout target).
func (s *Store) SetSupplierStripeAcct(ctx context.Context, supplierID uuid.UUID, acct string) error {
	_, err := s.pool.Exec(ctx, `UPDATE suppliers SET stripe_acct=$2 WHERE id=$1`, supplierID, acct)
	return err
}

const (
	intakeStageLockNamespace   uint32 = 0x4358494e // "CXIN"
	pipelineStageLockNamespace uint32 = 0x4358504c // "CXPL"
)

// workflowStageLockKeys deterministically maps a workflow/stage into PostgreSQL's
// two-int advisory-lock domain. Hash collisions only serialize unrelated stages;
// they can never permit duplicate execution, which is the safe failure mode.
func workflowStageLockKeys(namespace uint32, workflowID uuid.UUID, stageIndex int) (int32, int32) {
	left := binary.BigEndian.Uint32(workflowID[0:4]) ^
		binary.BigEndian.Uint32(workflowID[4:8]) ^ namespace
	right := binary.BigEndian.Uint32(workflowID[8:12]) ^
		binary.BigEndian.Uint32(workflowID[12:16]) ^ uint32(stageIndex)
	return int32(left), int32(right)
}

// LockWorkflowStage holds a session advisory lock on a dedicated pool connection
// across the check -> createJob -> link sequence. This closes the HA race where two
// completion workers could both observe "not submitted" and create two chargeable
// downstream jobs. Waiters serialize, then re-check the durable link after acquiring.
func (s *Store) LockWorkflowStage(ctx context.Context, namespace uint32, workflowID uuid.UUID, stageIndex int) (func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	key1, key2 := workflowStageLockKeys(namespace, workflowID, stageIndex)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, key1, key2); err != nil {
		conn.Release()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(releaseCtx, `SELECT pg_advisory_unlock($1, $2)`, key1, key2).Scan(&unlocked); err != nil || !unlocked {
				// Never return a connection with a possibly-held session lock to the
				// pool. Closing the hijacked connection lets Postgres release it.
				raw := conn.Hijack()
				_ = raw.Close(releaseCtx)
				return
			}
			conn.Release()
		})
	}, nil
}

// JobOutputRef returns a completed job's merged-output object key (for chaining the
// next pipeline stage onto it).
func (s *Store) JobOutputRef(ctx context.Context, jobID uuid.UUID) (string, error) {
	var ref string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(output_ref,'') FROM jobs WHERE id=$1`, jobID).Scan(&ref)
	return ref, err
}

// JobBuyerID returns the buyer who submitted jobID (Security Posture 6.5->7:
// resolveInput's s3_key ownership check — a buyer may only reference an object
// key under a job THEY submitted, never another buyer's).
func (s *Store) JobBuyerID(ctx context.Context, jobID uuid.UUID) (uuid.UUID, error) {
	var buyerID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT buyer_id FROM jobs WHERE id=$1`, jobID).Scan(&buyerID)
	return buyerID, err
}

// nullStrSlice maps an empty/nil slice to nil so it encodes as a SQL NULL (not an
// empty array). The claim's filters treat NULL hw_classes/data_residency as "any",
// which is the wrong semantics for an empty {} array — so empty must become NULL.
func nullStrSlice(xs []string) any {
	if len(xs) == 0 {
		return nil
	}
	return xs
}

// nullJSON maps empty bytes to nil so an absent JSONB column stays NULL rather
// than an invalid empty document.
func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullPosFloat maps a non-positive value to nil so the column stays SQL NULL
// (used for jobs.max_usd: 0 = "no cap" must be NULL, so the claim's budget gate
// distinguishes an unset cap from a real $0 cap via `IS NOT NULL`).
func nullPosFloat(v float64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// nullPosInt mirrors nullPosFloat for integer columns where 0 means "unset →
// persisted NULL" (jobs.sla_guarantee_secs, wave 2A): NULL cleanly means "no
// SLA" instead of a fake 0-second guarantee.
func nullPosInt(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullPosInt64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// nullStr maps the empty string to nil so a text column stays SQL NULL rather
// than an empty string (jobs.routing_substrate/routing_reason: "" means "no
// routing block", persisted NULL, so `routing_substrate IS NULL` cleanly
// distinguishes a job that carried no substrate decision — a non-generative or
// empty-input job — from one that did).
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullSHA256Hex validates a worker-reported SHA-256 hex string (Control Plane
// Hot Path 8->9, docs/internal/CREED_AND_PATH_TO_TEN.md "trust a
// buyer/worker-supplied SHA-256 ... where safe") before it is ever persisted or
// trusted for a hash-to-hash comparison: exactly 64 lowercase hex characters (a
// real SHA-256 digest), else nil (SQL NULL — the commit/verify path always falls
// back to a real GetObject when this is NULL, so a malformed or absent hash can
// never cause a wrong trust decision, only a missed speed optimization).
func nullSHA256Hex(h string) any {
	if len(h) != 64 {
		return nil
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return nil
		}
	}
	return h
}

// nullUUID maps the zero UUID to nil so an unset reference stays SQL NULL (used for
// jobs.quote_id: no binding must be NULL so the partial index and the quote→invoice
// join cleanly skip unbound jobs).
func nullUUID(id uuid.UUID) any {
	if id == (uuid.UUID{}) {
		return nil
	}
	return id
}

// hashKey hashes a credential for comparison against the stored key_hash. We
// store and compare only the hash, never the raw key.
func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// --- auth ---

// AuthResult identifies the caller behind a credential.
type AuthResult struct {
	BuyerID     uuid.UUID
	IsAdmin     bool
	APIKeyID    uuid.UUID
	APIKeyLabel string
}

// LookupAPIKey resolves a bearer API key to its buyer + admin flag. Missing or
// revoked → errNotFound (the handler turns that into 401). Never fake-accepts.
func (s *Store) LookupAPIKey(ctx context.Context, rawKey string) (AuthResult, error) {
	var r AuthResult
	err := s.pool.QueryRow(ctx,
		`SELECT id, buyer_id, is_admin,
		        COALESCE(NULLIF(name,''), CASE WHEN is_admin THEN 'break-glass API key' ELSE 'API key' END)
		   FROM api_keys
		 WHERE key_hash = $1 AND revoked = false`,
		hashKey(rawKey),
	).Scan(&r.APIKeyID, &r.BuyerID, &r.IsAdmin, &r.APIKeyLabel)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errNotFound
	}
	return r, err
}

// WorkerAuth identifies the worker/supplier behind a worker token.
type WorkerAuth struct {
	WorkerID              uuid.UUID
	SupplierID            uuid.UUID
	CredentialID          uuid.UUID
	DeviceFingerprint     string
	CredentialVersion     int
	EnrollmentDeviceBound bool
}

// LookupWorkerToken resolves an X-Worker-Token to its worker + supplier. Like
// api_keys, only the SHA-256 hash is stored and compared — the raw token never
// touches the DB, so a DB read can never leak a live supplier credential.
func (s *Store) LookupWorkerToken(ctx context.Context, token string) (WorkerAuth, error) {
	var w WorkerAuth
	err := s.pool.QueryRow(ctx,
		`SELECT worker_id,supplier_id,credential_id,COALESCE(device_fingerprint,''),
		        credential_version,device_fingerprint IS NOT NULL
		   FROM worker_tokens
		 WHERE token_hash = $1 AND revoked = false`,
		hashKey(token),
	).Scan(&w.WorkerID, &w.SupplierID, &w.CredentialID, &w.DeviceFingerprint,
		&w.CredentialVersion, &w.EnrollmentDeviceBound)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, errNotFound
	}
	return w, err
}

// CreateWorkerToken mints a worker token for a supplier's NEW worker: it generates
// a random raw token, stores ONLY its hash, and returns the raw token once. The raw
// value can never be recovered (it is not stored) — the caller hands it to the
// supplier. This is the onboarding path real suppliers use instead of seeded tokens.
//
// worker_tokens.worker_id has a foreign key into workers, but the real workers row
// (hw_class, benchmarks, ...) does not exist yet at mint time — the agent only
// creates/fills it via UpsertWorker's `ON CONFLICT (id) DO UPDATE` the first time it
// actually registers with this token. So this inserts a minimal PLACEHOLDER workers
// row first, in the same transaction: hw_class='cpu' (a valid enum member, used only
// to satisfy the NOT NULL column) with supported_jobs/supported_models left NULL.
// More importantly it has no worker_authorized_capabilities rows. Every eligibility
// path requires an exact current-matrix row, so this placeholder is structurally
// inert until the agent's first real registration atomically creates those rows.
//
// It also activates the supplier (status 'pending' -> 'active') if this is their
// FIRST token. EnsureSupplierForBuyer leaves a brand-new supplier at
// status='pending', the claim query hard-requires
// s.status='active' (scheduler.go's quarantine gate), and — before this fix —
// nothing in production code ever performed that transition; only test fixtures
// and the demo seed set status='active' directly. Without this, a self-served
// supplier could authenticate and poll forever but their tasks could NEVER be
// claimed. The guard is `AND status = 'pending'`, deliberately: minting an
// ADDITIONAL token for a supplier who was later suspended/quarantined for fraud
// must never silently reactivate them — activation only ever happens once, on the
// way up from the initial pending state.
func (s *Store) CreateWorkerToken(ctx context.Context, workerID, supplierID uuid.UUID) (string, error) {
	raw := newSecret("cxw_")
	if raw == "" {
		return "", errors.New("worker token: entropy failure")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class) VALUES ($1, $2, 'cpu')
		 ON CONFLICT (id) DO NOTHING`,
		workerID, supplierID,
	); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_tokens (token_hash, worker_id, supplier_id, revoked)
		 VALUES ($1, $2, $3, false)`,
		hashKey(raw), workerID, supplierID,
	); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE suppliers SET status = 'active' WHERE id = $1 AND status = 'pending'`,
		supplierID,
	); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return raw, nil
}

// --- buyer API-key lifecycle (POST/GET/DELETE /v1/keys) ---

// APIKeyRow is one masked api_key as shown in the list view. `Masked` is the
// non-secret display hint (prefix + last4) captured at mint; the raw secret is
// NEVER reconstructable (only key_hash is stored).
type APIKeyRow struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Masked    string    `json:"masked"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

// CreateAPIKey mints a buyer API key the same way CreateWorkerToken mints a worker
// token: it generates a random raw secret, stores ONLY its SHA-256 hash plus a
// non-secret display hint (prefix + last4), and returns the raw secret ONCE. The
// raw value is unrecoverable afterwards (it is never stored). `test` selects the
// cx_test_ prefix (vs. cx_live_) so callers can tell environments apart; it does
// not change auth · both authenticate identically (no scopes/spend here by design).
// is_admin is left at its default (false): minting a key never grants admin.
func (s *Store) CreateAPIKey(ctx context.Context, buyerID uuid.UUID, name string, test bool) (id uuid.UUID, raw, masked string, err error) {
	prefix := "cx_live_"
	if test {
		prefix = "cx_test_"
	}
	raw = newSecret(prefix)
	if raw == "" {
		return uuid.Nil, "", "", errors.New("api key: entropy failure")
	}
	masked = maskKey(raw)
	err = s.pool.QueryRow(ctx,
		`INSERT INTO api_keys (buyer_id, key_hash, name, masked, is_admin, revoked)
		 VALUES ($1, $2, $3, $4, false, false)
		 RETURNING id`,
		buyerID, hashKey(raw), name, masked,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, "", "", err
	}
	return id, raw, masked, nil
}

// ListAPIKeys returns the caller's keys as masked rows (never the raw secret ·
// only key_hash is stored, so there is nothing to leak even on a full DB read).
// Scoped to buyerID; revoked keys are included so the UI can show their state.
func (s *Store) ListAPIKeys(ctx context.Context, buyerID uuid.UUID) ([]APIKeyRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, COALESCE(name,''), COALESCE(masked,''), created_at, revoked
		   FROM api_keys
		  WHERE buyer_id = $1
		  ORDER BY created_at DESC`,
		buyerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKeyRow{}
	for rows.Next() {
		var r APIKeyRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Masked, &r.CreatedAt, &r.Revoked); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeAPIKey revokes one key, scoped to the owning buyer so a caller can never
// revoke another buyer's key. Idempotent: revoking an already-revoked (or
// never-existed-for-this-buyer) key is a no-op success · the endpoint is 204
// either way, matching the DELETE contract. Returns whether a row was found so
// the handler can distinguish "revoked" from "not yours / not found" if needed.
func (s *Store) RevokeAPIKey(ctx context.Context, buyerID, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked = true
		  WHERE id = $1 AND buyer_id = $2`,
		id, buyerID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// maskKey builds the non-secret display hint for an api_key: the cx_live_/cx_test_
// prefix plus the last 4 chars of the secret, joined by an ellipsis. It is derived
// from the raw secret ONLY at mint time and persisted; it is never enough to
// reconstruct the key (4 trailing chars of 32 bytes of entropy).
func maskKey(raw string) string {
	// CX key tags are "cx_live_" / "cx_test_" (two underscores); the random tail is
	// base64url and may itself contain "_", so take up to the SECOND underscore. Never
	// LastIndex · that would leak almost the whole secret into the masked hint.
	prefix := raw
	if i1 := strings.IndexByte(raw, '_'); i1 >= 0 {
		if i2 := strings.IndexByte(raw[i1+1:], '_'); i2 >= 0 {
			prefix = raw[:i1+1+i2+1]
		}
	}
	last4 := raw
	if len(raw) >= 4 {
		last4 = raw[len(raw)-4:]
	}
	return prefix + "..." + last4
}

// --- workers + benchmarks ---

// UpsertWorker inserts or refreshes a worker row and persists its benchmark
// results plus its exact generated production capabilities, all in one
// transaction. Called on POST /v1/worker/register. The independent capability
// arrays remain stored for wire/debug compatibility but are never dispatch
// authority; only worker_authorized_capabilities is.
func (s *Store) UpsertWorker(ctx context.Context, cap WorkerCapability) error {
	projected, err := projectWorkerRuntimeCapabilities(cap)
	if err != nil {
		return fmt.Errorf("projecting worker runtime capabilities: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// thermal_ok is the AND of every benchmark's thermal_ok, defaulting to true
	// when the worker reported no benchmarks (no evidence of throttling).
	thermalOK := true
	for _, b := range cap.Benchmarks {
		thermalOK = thermalOK && b.ThermalOK
	}

	// engine + build_hash are the verification-class axes (handler-normalized:
	// engine is blank→candle + validated, build_hash is the opaque agent build tag).
	// They are persisted alongside hw_class so the redundancy matcher and the verifier
	// can pin byte-exact peers/honeypots to the same (hw_class, engine, build_hash).
	_, err = tx.Exec(ctx,
		`INSERT INTO workers
		   (id, supplier_id, hw_class, engine, build_hash, memory_gb, bw_gbps, last_seen_at, version,
		    supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now(), $8,$9,$10,$11,$12)
		 ON CONFLICT (id) DO UPDATE SET
		   hw_class = EXCLUDED.hw_class,
		   engine = EXCLUDED.engine,
		   build_hash = EXCLUDED.build_hash,
		   memory_gb = EXCLUDED.memory_gb,
		   bw_gbps = EXCLUDED.bw_gbps,
		   last_seen_at = now(),
		   version = EXCLUDED.version,
		   supported_jobs = EXCLUDED.supported_jobs,
		   supported_models = EXCLUDED.supported_models,
		   min_payout_usd_hr = EXCLUDED.min_payout_usd_hr,
		   thermal_ok = EXCLUDED.thermal_ok`,
		cap.WorkerID, cap.SupplierID, cap.HWClass, cap.Engine, cap.BuildHash, cap.MemoryGB, cap.MemoryBwGbps, cap.AgentVersion,
		cap.SupportedJobs, cap.SupportedModels, cap.MinPayoutUsdHr, thermalOK,
	)
	if err != nil {
		return err
	}

	// Replace, never merge: a worker that stops advertising a cell loses that
	// authority in the same transaction that updates its worker row. Rows carry the
	// generated matrix hash, and every reader requires the current hash, so a deploy
	// also makes stale registrations inert until the agent re-registers. There is no
	// legacy array backfill anywhere in Migrate or seedDemo.
	if _, err := tx.Exec(ctx,
		`DELETE FROM worker_authorized_capabilities WHERE worker_id = $1`,
		cap.WorkerID); err != nil {
		return err
	}
	for _, authorized := range projected {
		if _, err := tx.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
				   (worker_id, cell_id, runtime_id, job_type, model_ref, model_kind, matrix_sha256)
				 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			cap.WorkerID, authorized.ID, authorized.Runtime, authorized.Job,
			authorized.Model, authorized.ModelKind, generatedRuntimeMatrixSHA256); err != nil {
			return err
		}
	}

	for _, b := range cap.Benchmarks {
		loadMS, err := benchmarkLoadMSForStore(b.LoadMS)
		if err != nil {
			return err // projectWorkerRuntimeCapabilities already validates this; keep the cast local and checked.
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO benchmark_results
			   (worker_id, model_id, job_type, tps, eps, thermal_ok, p99_latency_ms, load_ms)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			cap.WorkerID, b.ModelID, b.JobType, b.TPS, b.EPS, b.ThermalOK, float32(b.P99MS), loadMS,
		)
		if err != nil {
			return err
		}
		// Maintain worker_tps_cache HERE, at the one real worker-state change
		// (Control Plane Hot Path 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md
		// "hoist worker_tps into something computed once per worker state change
		// rather than recomputed per candidate row per claim"), instead of
		// ClaimTaskSQL re-deriving "this worker's most recent tps for this
		// job_type" with a correlated subquery over benchmark_results for EVERY
		// eligible candidate row on EVERY claim. UpsertWorker only ever appends
		// benchmark_results newest-first (ORDER BY measured_at DESC in the old
		// subquery), so a plain last-write-wins UPSERT here is equivalent to that
		// "most recent" semantics — the maintained cache and the historical
		// benchmark_results ledger (kept, unchanged, for HistoricalP90-style
		// analysis elsewhere) never disagree about which measurement is newest.
		schedulerRate := b.TPS
		if b.JobType == "embed" {
			schedulerRate = b.EPS
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO worker_tps_cache (worker_id, job_type, tps, updated_at)
			 VALUES ($1,$2,$3, now())
			 ON CONFLICT (worker_id, job_type) DO UPDATE SET
			   tps = EXCLUDED.tps, updated_at = now()`,
			cap.WorkerID, b.JobType, schedulerRate,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// WorkerResources is the live resource state a heartbeat carries (the supplier
// throttling signal). All GB; Throttled means the agent is currently pausing new
// claims for memory pressure. LoadedModels is the warm-routing delta (D3): the ids
// of models warm in the agent's pool right now, upserted into worker_model_state so
// the scheduler can prefer a warm worker (it only re-ranks; never a hard filter).
type WorkerResources struct {
	AvailableMemoryGB  float32
	EffectiveMemoryGB  float32
	ReservedHeadroomGB float32
	Throttled          bool
	LoadedModels       []string
}

// HeartbeatWorker refreshes last_seen_at AND the live resource state the
// safe-dispatch claim filter reads (effective memory + throttle). Called from
// POST /v1/worker/heartbeat. A reported memory value of 0 is treated as "not
// sent" (a pre-throttling agent omits these fields) and leaves the stored value
// untouched, so the claim falls back to total memory rather than wrongly
// excluding the worker. `throttled` is always written (false for older agents,
// which keeps them claimable).
func (s *Store) HeartbeatWorker(ctx context.Context, workerID uuid.UUID, r WorkerResources) error {
	// nil → COALESCE keeps the existing column value (NULL until first real beat).
	var effective, available, headroom *float32
	if r.EffectiveMemoryGB > 0 {
		effective = &r.EffectiveMemoryGB
	}
	if r.AvailableMemoryGB > 0 {
		available = &r.AvailableMemoryGB
	}
	if r.ReservedHeadroomGB > 0 {
		headroom = &r.ReservedHeadroomGB
	}
	// One transaction so a heartbeat is all-or-nothing: liveness + memory + warm
	// state never partially apply (a mid-beat failure would otherwise leave, say,
	// last_seen_at fresh but warm state stale). Matches the atomicity the claim path
	// already uses.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE workers SET last_seen_at = now(),
		   effective_memory_gb  = COALESCE($2, effective_memory_gb),
		   available_memory_gb  = COALESCE($3, available_memory_gb),
		   reserved_headroom_gb = COALESCE($4, reserved_headroom_gb),
		   throttled            = $5
		 WHERE id = $1`,
		workerID, effective, available, headroom, r.Throttled); err != nil {
		return err
	}
	// Append a rolling memory sample (Plane D D4): the UPDATE above keeps only the
	// LATEST beat; this row preserves the history /admin/capacity + quote risk read.
	// Only beats that actually reported memory write a sample — a pre-throttling
	// agent sends neither value, and an all-NULL row would just dilute the median.
	if effective != nil || available != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO worker_memory_samples (worker_id, available_gb, effective_gb, throttled)
			 VALUES ($1, $2, $3, $4)`,
			workerID, available, effective, r.Throttled); err != nil {
			return err // a failed sample is a real failure, not silently swallowed (BLACKHOLE)
		}
	}
	// Warm-model state (D3): upsert one row per model the agent reported warm,
	// refreshing last_seen_warm so the scheduler/quote see which (worker, model)
	// pairs avoid a cold load right now. The unnest+ON CONFLICT writes all ids in a
	// single round-trip and is a no-op for a pre-warm agent (LoadedModels nil → the
	// array is empty). Staleness is read against last_seen_warm (a worker that stops
	// reporting a model ages out), so we never need to delete stale rows here. A
	// failed upsert is a real failure (BLACKHOLE), not silently swallowed.
	if len(r.LoadedModels) > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO worker_model_state (worker_id, model_id, last_seen_warm)
			 SELECT $1, m, now() FROM unnest($2::text[]) AS m
			 ON CONFLICT (worker_id, model_id)
			 DO UPDATE SET last_seen_warm = now()`,
			workerID, r.LoadedModels); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// MedianEffectiveMemoryGB returns the median effective memory (GB) across the most
// recent memory sample of each LIVE eligible worker for (jobType, modelRef): one
// row per worker (its latest sample), filtered to the same exact capability +
// active-supplier + not-throttled + seen-<60s predicate the claim uses, so the
// median reflects supply a job could actually land on. ok is false when no eligible
// worker has reported a sample yet (the caller then leaves quote risk unchanged
// rather than inventing a floor). Plane D D4 quote-risk feedback.
func (s *Store) MedianEffectiveMemoryGB(ctx context.Context, jobType, modelRef string) (median float32, ok bool, err error) {
	var med *float64
	err = s.pool.QueryRow(ctx,
		`WITH latest AS (
		     SELECT DISTINCT ON (m.worker_id) m.worker_id, m.effective_gb
		       FROM worker_memory_samples m
		       JOIN workers w   ON w.id = m.worker_id
		       JOIN suppliers s ON s.id = w.supplier_id
		      WHERE m.effective_gb IS NOT NULL
		        AND w.last_seen_at IS NOT NULL
		        AND w.last_seen_at > now() - interval '60 seconds'
		        AND s.status = 'active'
		        AND NOT COALESCE(w.throttled, false)
		        AND EXISTS (
		          SELECT 1 FROM worker_authorized_capabilities wac
		           WHERE wac.worker_id = w.id
		             AND wac.job_type = $1
		             AND wac.model_ref = $2
		             AND wac.matrix_sha256 = $3
		        )
		      ORDER BY m.worker_id, m.created_at DESC
		 )
		 SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY effective_gb) FROM latest`,
		jobType, modelRef, generatedRuntimeMatrixSHA256,
	).Scan(&med)
	if err != nil {
		return 0, false, err
	}
	if med == nil {
		return 0, false, nil // no eligible worker has a sample yet
	}
	return float32(*med), true, nil
}

// AdminWorker is one row of GET /admin/workers. EffectiveMemoryGB + Throttled
// are the live throttling state (operator visibility): effective is the
// allocatable pool after the supplier's headroom (falls back to total memory
// before the first heartbeat), and Throttled is the worker pausing for memory
// pressure — the same signals the safe-dispatch claim filter enforces.
type AdminWorker struct {
	ID                uuid.UUID  `json:"id"`
	SupplierID        uuid.UUID  `json:"supplier_id"`
	HWClass           string     `json:"hw_class"`
	MemoryGB          float32    `json:"memory_gb"`
	EffectiveMemoryGB float32    `json:"effective_memory_gb"`
	Throttled         bool       `json:"throttled"`
	AvgAvailableGB    float32    `json:"avg_available_gb"` // mean available_gb over the last memSampleWindow samples (0 = no samples yet)
	MemorySamples     int        `json:"memory_samples"`   // how many recent samples backed AvgAvailableGB (operator can judge the average's weight)
	LastSeenAt        *time.Time `json:"last_seen_at"`
	Version           string     `json:"version"`
	Reputation        float32    `json:"reputation"`
	Tier              int16      `json:"tier"`
	Status            string     `json:"status"`
}

// memSampleWindow is how many of a worker's most-recent memory samples the admin
// rolling average + capacity view summarise. Small so the figure tracks RECENT
// pressure (a worker that just freed memory is not penalised for old beats).
const memSampleWindow = 20

// DeleteOldWorkerMemorySamples prunes rows older than `before` (docs/
// CREED_AND_PATH_TO_TEN.md, "Postgres data lifecycle" 3→4). worker_memory_samples
// appends one row per worker per heartbeat-with-memory-reporting — at 1k workers on
// a 30s heartbeat that is roughly 2.9M rows/day with no bound before this existed.
// Every real read of this table (ListWorkers above, memSampleWindow's admin/capacity
// view, and the quote-risk memory-floor check) only ever looks at the most recent
// memSampleWindow rows per worker, so deleting anything older than a modest
// retention window changes no query's result — this is pure bloat removal, not a
// behavior change. Returns the number of rows actually deleted so the caller (the
// retention ticker) can log real progress, not just "ran".
func (s *Store) DeleteOldWorkerMemorySamples(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM worker_memory_samples WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteOldTaskDurations prunes rows older than `before` (docs/
// CREED_AND_PATH_TO_TEN.md, "Postgres data lifecycle" 4→5, extending the same fix
// to the other unbounded telemetry table the audit named). task_durations is purely
// internal: HistoricalP90DurationMs and the admin drift rollup are its only readers,
// both aggregate queries with driftMinSamples gating thin history, neither pins to
// specific old rows by id. Both readers ALSO now bound themselves to driftWindow
// (24h, Performance Observability 7→8) independent of this retention window — this
// 30-day window is deliberately wider than driftWindow so retention is never the
// thing silently narrowing what the drift calculation can see; it just bounds how
// long the raw rows survive at all, and can be widened later without any code
// change if the drift/ETA calculation's own window is ever widened past it.
func (s *Store) DeleteOldTaskDurations(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM task_durations WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteOldJobEvents prunes rows older than `before`. UNLIKE the two telemetry
// tables above, job_events is buyer-VISIBLE history (GET /v1/jobs/{id}/events
// reads it directly, scoped to one job at a time) — a buyer auditing an old
// invoice or dispute may reasonably want to see it months later, so this uses a
// much longer window than task_durations/worker_memory_samples by design; the
// caller (sweepTelemetryRetention) passes a 180-day cutoff, not the 30-day one
// used for pure telemetry.
func (s *Store) DeleteOldJobEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM job_events WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// TelemetryTableCounts returns the live row count of every table
// sweepTelemetryRetention prunes (Postgres Data Lifecycle 8->9), keyed by table
// name — the bloat-ratio half of the retention-health metric: a real count an
// operator can watch for "still climbing despite the sweep running", not just
// inferred from the sweep's own self-reported prune count. Table names here are
// this codebase's own fixed constants (telemetryTables), never request input, so
// the direct interpolation below carries no injection risk.
func (s *Store) TelemetryTableCounts(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(telemetryTables))
	for _, table := range telemetryTables {
		var n int64
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("counting %s: %w", table, err)
		}
		out[table] = n
	}
	return out, nil
}

// ListWorkers returns the worker fleet joined to supplier reputation/status, each
// annotated with its recent memory-pressure average (avg_available_gb over the last
// memSampleWindow worker_memory_samples — Plane D D4). The LATERAL subquery is a
// per-worker recent-N average; workers without samples yet report 0/0, never a faked
// number. The index (worker_id, created_at DESC) serves the ordered limit.
func (s *Store) ListWorkers(ctx context.Context) ([]AdminWorker, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT w.id, w.supplier_id, w.hw_class, w.memory_gb,
		        COALESCE(w.effective_memory_gb, w.memory_gb, 0),
		        COALESCE(w.throttled, false),
		        COALESCE(s2.avg_available_gb, 0), COALESCE(s2.n, 0),
		        w.last_seen_at,
		        COALESCE(w.version,''), s.reputation, s.tier, s.status
		 FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		 LEFT JOIN LATERAL (
		     SELECT avg(recent.available_gb)::real AS avg_available_gb, count(*) AS n
		       FROM (SELECT available_gb FROM worker_memory_samples
		              WHERE worker_id = w.id AND available_gb IS NOT NULL
		              ORDER BY created_at DESC LIMIT $1) recent
		 ) s2 ON true
		 ORDER BY w.last_seen_at DESC NULLS LAST`,
		memSampleWindow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminWorker
	for rows.Next() {
		var a AdminWorker
		if err := rows.Scan(&a.ID, &a.SupplierID, &a.HWClass, &a.MemoryGB,
			&a.EffectiveMemoryGB, &a.Throttled, &a.AvgAvailableGB, &a.MemorySamples,
			&a.LastSeenAt, &a.Version,
			&a.Reputation, &a.Tier, &a.Status); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AdminJob is one row of the admin jobs view (across ALL buyers).
type AdminJob struct {
	ID           uuid.UUID `json:"id"`
	BuyerID      uuid.UUID `json:"buyer_id"`
	Status       string    `json:"status"`
	JobType      string    `json:"job_type"`
	ModelRef     string    `json:"model_ref"`
	Tier         string    `json:"tier"`
	TaskCount    int       `json:"task_count"`
	TasksDone    int       `json:"tasks_done"`
	EstimatedUSD float64   `json:"estimated_usd"`
	ActualUSD    float64   `json:"actual_usd"`
	CreatedAt    time.Time `json:"created_at"`
}

// ListJobsAdmin returns the most recent jobs across all buyers for the admin panel
// (newest first, capped at 200). Unlike GET /v1/jobs/{id} this is NOT buyer-scoped
// — it is gated by authAdmin.
func (s *Store) ListJobsAdmin(ctx context.Context) ([]AdminJob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, buyer_id, status, job_type, COALESCE(model_ref,''), tier,
		        COALESCE(task_count,0), COALESCE(tasks_done,0),
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0), created_at
		 FROM jobs ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminJob
	for rows.Next() {
		var a AdminJob
		if err := rows.Scan(&a.ID, &a.BuyerID, &a.Status, &a.JobType, &a.ModelRef,
			&a.Tier, &a.TaskCount, &a.TasksDone, &a.EstimatedUSD, &a.ActualUSD, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AdminPayout is one per-supplier, per-status rollup of supplier credits.
type AdminPayout struct {
	SupplierID               uuid.UUID `json:"supplier_id"`
	PayoutStatus             string    `json:"payout_status"`
	Count                    int       `json:"count"`
	AmountUSD                float64   `json:"amount_usd"`
	CashSentUSD              float64   `json:"cash_sent_usd"`
	CarriedRemainderUSD      float64   `json:"carried_remainder_usd"`
	OutcomeUnknownCount      int       `json:"outcome_unknown_count"`
	ReleasedWithoutCashCount int       `json:"released_without_cash_count"`
}

// ListPayoutsAdmin rolls up supplier_credit ledger entries by (supplier, payout
// status) so the admin panel can see the complete payout state, confirmed cash,
// carried debt, ambiguous outcomes, and structurally suspicious released rows.
func (s *Store) ListPayoutsAdmin(ctx context.Context) ([]AdminPayout, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(le.supplier_id,'00000000-0000-0000-0000-000000000000'::uuid),
		        le.payout_status, COUNT(*), COALESCE(SUM(le.amount_usd),0),
		        COALESCE(SUM(op.sent_cents) FILTER (WHERE op.cash_moved),0)::float8 / 100.0,
		        COALESCE(SUM(mu.remainder_microusd),0)::float8 / 1000000.0,
		        COUNT(*) FILTER (WHERE COALESCE(op.outcome_unknown,false)),
		        COUNT(*) FILTER (
		          WHERE le.payout_status='released' AND NOT COALESCE(op.cash_moved,false))
		 FROM ledger_entries le
		 LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 LEFT JOIN supplier_minor_unit_settlements mu ON mu.ledger_entry_id=le.id
		 WHERE le.kind = 'supplier_credit'
		 GROUP BY le.supplier_id, le.payout_status
		 ORDER BY le.supplier_id, le.payout_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminPayout
	for rows.Next() {
		var a AdminPayout
		if err := rows.Scan(&a.SupplierID, &a.PayoutStatus, &a.Count, &a.AmountUSD,
			&a.CashSentUSD, &a.CarriedRemainderUSD, &a.OutcomeUnknownCount,
			&a.ReleasedWithoutCashCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SuspendWorker flags a worker's supplier as suspended (manual admin action).
func (s *Store) SuspendWorker(ctx context.Context, workerID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE suppliers SET status = 'suspended'
		 WHERE id = (SELECT supplier_id FROM workers WHERE id = $1)`,
		workerID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// ReinstateWorker closes the "reinstate after review" half of RUNBOOKS.md's Bad /
// fraudulent worker procedure (Operator Tooling 7->8): until now this was a raw
// `psql -c "UPDATE suppliers SET status='active', quarantined_at=NULL ..."`. Only
// clears quarantine on a supplier that IS quarantined/suspended (never touches an
// already-active supplier, and never reinstates a 'banned' supplier — a ban is a
// harder stop than a quarantine and must stay a deliberate, separate action, not a
// side effect of this endpoint).
func (s *Store) ReinstateWorker(ctx context.Context, workerID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE suppliers SET status = 'active', quarantined_at = NULL
		 WHERE id = (SELECT supplier_id FROM workers WHERE id = $1) AND status = 'suspended'`,
		workerID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		// Distinguish "no such worker" from "worker exists but isn't suspended" so
		// the handler can 404 vs 409 instead of collapsing both into one error.
		var exists bool
		if qerr := s.pool.QueryRow(ctx, `SELECT true FROM workers WHERE id = $1`, workerID).Scan(&exists); errors.Is(qerr, pgx.ErrNoRows) {
			return errNotFound
		} else if qerr != nil {
			return qerr
		}
		return errNotSuspended
	}
	return nil
}

// errNotSuspended distinguishes "worker exists but its supplier is not currently
// suspended" from errNotFound, so ReinstateWorker's caller can 409 instead of 404.
var errNotSuspended = errors.New("supplier is not suspended")

// AdminAction is one row of the append-only operator-write audit log (Operator
// Tooling 7->8): every admin write endpoint that mutates real operational state
// records one of these in the SAME transaction as the mutation, so "who did what,
// when, and why" never depends on re-deriving it from a diff of table state.
type AdminAction struct {
	ID            uuid.UUID       `json:"id"`
	CreatedAt     time.Time       `json:"created_at"`
	Kind          string          `json:"kind"`
	TaskID        *uuid.UUID      `json:"task_id,omitempty"`
	SupplierID    *uuid.UUID      `json:"supplier_id,omitempty"`
	LedgerEntryID *uuid.UUID      `json:"ledger_entry_id,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Detail        json.RawMessage `json:"detail,omitempty"`
}

// recordAdminAction inserts one audit row. Called INSIDE the same transaction as
// the mutation it documents (always a *pgxpool.Tx here) so the audit trail can
// never exist for a mutation that rolled back, or vice versa.
func recordAdminAction(ctx context.Context, tx pgx.Tx, kind string, taskID, supplierID, ledgerEntryID *uuid.UUID, reason string, detail any) error {
	var detailJSON []byte
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		detailJSON = b
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO admin_actions (kind, task_id, supplier_id, ledger_entry_id, reason, detail)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		kind, taskID, supplierID, ledgerEntryID, nullIfEmpty(reason), detailJSON)
	return err
}

// ListAdminActions returns the audit log, newest first, capped at 200 — the
// operator's "what happened and why" review surface for every write action this
// console now performs instead of raw SQL.
func (s *Store) ListAdminActions(ctx context.Context) ([]AdminAction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, created_at, kind, task_id, supplier_id, ledger_entry_id, COALESCE(reason,''), detail
		 FROM admin_actions ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAction
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Kind, &a.TaskID, &a.SupplierID, &a.LedgerEntryID, &a.Reason, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AdminForceRequeueTask closes the "Stuck job" runbook's manual fix (Operator
// Tooling 7->8): until now this was a raw
// `psql -c "UPDATE tasks SET status='queued', claimed_by=NULL, visible_at=now() ..."`.
// Scoped to the SAME status set the runbook names (running/retrying) — a queued
// task is already claimable and a complete/failed/cancelled task must never be
// silently resurrected by an operator fat-fingering an id. Records an audit row
// in the SAME transaction, so a requeue can never happen without a trace of who
// forced it and why.
func (s *Store) AdminForceRequeueTask(ctx context.Context, taskID uuid.UUID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var jobID uuid.UUID
	var prevStatus string
	if err := tx.QueryRow(ctx,
		`UPDATE tasks SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		                  worker_id = NULL, visible_at = now()
		 WHERE id = $1 AND status IN ('running','retrying')
		 RETURNING job_id, status`,
		taskID,
	).Scan(&jobID, &prevStatus); errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "no such task" from "task exists but isn't in a requeueable
		// state" so the handler can 404 vs 409 instead of collapsing both.
		var exists bool
		if qerr := tx.QueryRow(ctx, `SELECT true FROM tasks WHERE id = $1`, taskID).Scan(&exists); errors.Is(qerr, pgx.ErrNoRows) {
			return errNotFound
		} else if qerr != nil {
			return qerr
		}
		return errNotRequeueable
	} else if err != nil {
		return err
	}

	if err := recordAdminAction(ctx, tx, "task_requeued", &taskID, nil, nil, reason,
		map[string]any{"job_id": jobID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// errNotRequeueable distinguishes "task exists but is not in a requeueable state
// (running/retrying)" from errNotFound.
var errNotRequeueable = errors.New("task is not in a requeueable state")

// AdminAdjustReputation closes the "manually adjust a supplier's reputation with
// an audit trail" gap named directly in the backlog rung (Operator Tooling 7->8).
// Unlike DockReputation/DockReputationMild (which apply a FIXED delta for a named
// verification event), this is an operator-driven, arbitrary, auditable
// adjustment — e.g. correcting a reputation score after a manual fraud review
// overturns or confirms an automated call. Clamped to [0,1] like every other
// reputation write in this codebase; the audit row records the exact before/after
// values and the operator's stated reason, so "why is this supplier's reputation
// what it is" is always answerable without asking anyone.
func (s *Store) AdminAdjustReputation(ctx context.Context, supplierID uuid.UUID, delta float32, reason string) (before, after float32, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx,
		`SELECT reputation FROM suppliers WHERE id = $1 FOR UPDATE`, supplierID,
	).Scan(&before); errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, errNotFound
	} else if err != nil {
		return 0, 0, err
	}

	after = before + delta
	if after < 0 {
		after = 0
	} else if after > 1 {
		after = 1
	}
	if _, err := tx.Exec(ctx, `UPDATE suppliers SET reputation = $2 WHERE id = $1`, supplierID, after); err != nil {
		return 0, 0, err
	}

	if err := recordAdminAction(ctx, tx, "reputation_adjusted", nil, &supplierID, nil, reason,
		map[string]any{"delta": delta, "before": before, "after": after}); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return before, after, nil
}

var (
	errPayoutFundingAlreadyBound = errors.New("payout already has a different funding source")
	errSubsidyFundConflict       = errors.New("subsidy fund reference conflicts with existing authorization")
	errSubsidyFundUnavailable    = errors.New("subsidy fund is missing, closed, or has insufficient unreserved capacity")
)

func isPayoutFundingUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateSubsidyFund records a finite, immutable operator-declared treasury pool.
// externalTreasuryRef is an operator assertion rather than independent bank
// reconciliation; the enforced theorem is exact and narrower: later reservations
// are row-locked and cannot sum past authorizedCents. Identical creation retries
// are idempotent, while any attempted rebinding or capacity change is rejected.
func (s *Store) CreateSubsidyFund(
	ctx context.Context,
	actor AdminActor,
	fundRef string,
	externalTreasuryRef string,
	authorizedCents int64,
	reason string,
) (bool, error) {
	fundRef = strings.TrimSpace(fundRef)
	externalTreasuryRef = strings.TrimSpace(externalTreasuryRef)
	reason = strings.TrimSpace(reason)
	if fundRef == "" || externalTreasuryRef == "" || reason == "" {
		return false, errors.New("fund_ref, external_treasury_ref, and reason are required")
	}
	if len(fundRef) > 200 || len(externalTreasuryRef) > 300 || len(reason) > 1000 {
		return false, errors.New("subsidy fund field is too long")
	}
	if authorizedCents <= 0 {
		return false, errors.New("authorized_cents must be positive")
	}
	if err := validateAdminActorShape(actor); err != nil {
		return false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return false, err
	}
	// Serialize one natural fund reference before checking/inserting both sides of
	// the action/resource binding. Hash collisions only serialize unrelated funds;
	// they cannot merge authority or relax a constraint.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fundRef); err != nil {
		return false, err
	}

	var (
		existingFundID, existingActionID                      uuid.UUID
		existingTreasuryRef, existingCurrency, existingReason string
		existingStatus                                        string
		existingAuthorizedCents                               int64
	)
	err = tx.QueryRow(ctx, `
		SELECT id,COALESCE(authorization_action_id,'00000000-0000-0000-0000-000000000000'::uuid),
		       external_treasury_ref,authorized_cents,currency,reason,status
		  FROM platform_subsidy_funds WHERE fund_ref=$1 FOR UPDATE`, fundRef).Scan(
		&existingFundID, &existingActionID, &existingTreasuryRef, &existingAuthorizedCents,
		&existingCurrency, &existingReason, &existingStatus)
	if err == nil {
		intent := moneyAuthorityIntent{
			Kind: "subsidy_fund_authorized", TargetKind: "subsidy_fund",
			TargetID: existingFundID, FundID: existingFundID, FundRef: fundRef,
			ExternalTreasuryRef: externalTreasuryRef, AmountCents: authorizedCents,
			Currency: "usd", Reason: reason, CorrelationRef: fundRef,
		}
		if existingTreasuryRef != externalTreasuryRef || existingAuthorizedCents != authorizedCents ||
			existingCurrency != "usd" || existingReason != reason || existingStatus != "active" {
			return false, errSubsidyFundConflict
		}
		if err := assertMoneyAuthorityAction(ctx, tx, actor, existingActionID, intent); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	fundID, actionID := uuid.New(), uuid.New()
	intent := moneyAuthorityIntent{
		Kind: "subsidy_fund_authorized", TargetKind: "subsidy_fund",
		TargetID: fundID, FundID: fundID, FundRef: fundRef,
		ExternalTreasuryRef: externalTreasuryRef, AmountCents: authorizedCents,
		Currency: "usd", Reason: reason, CorrelationRef: fundRef,
	}
	if _, err := insertMoneyAuthorityAction(ctx, tx, actor, actionID, intent, nil); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errSubsidyFundConflict
		}
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_subsidy_funds
		  (id,authorization_action_id,fund_ref,external_treasury_ref,
		   authorized_cents,currency,reason,status)
		VALUES ($1,$2,$3,$4,$5,'usd',$6,'active')`,
		fundID, actionID, fundRef, externalTreasuryRef, authorizedCents, reason); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errSubsidyFundConflict
		}
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// AuthorizePayoutSubsidy is the explicit capped platform alternative to buyer
// collection. It does not claim that cash moved or waive the payout hold: it
// reserves exact cents from an existing operator-declared treasury pool. The pool
// row is locked while aggregate reservations are checked, and the globally unique
// per-liability authorization reference prevents approval reuse. Identical retries
// are idempotent and do not append duplicate admin actions.
func (s *Store) AuthorizePayoutSubsidy(
	ctx context.Context,
	actor AdminActor,
	entryID uuid.UUID,
	fundRef string,
	authorizationRef string,
	reason string,
) (bool, error) {
	fundRef = strings.TrimSpace(fundRef)
	authorizationRef = strings.TrimSpace(authorizationRef)
	reason = strings.TrimSpace(reason)
	if fundRef == "" {
		return false, errors.New("subsidy fund_ref is required")
	}
	if authorizationRef == "" {
		return false, errors.New("subsidy authorization_ref is required")
	}
	if reason == "" {
		return false, errors.New("subsidy reason is required")
	}
	if len(fundRef) > 200 || len(authorizationRef) > 200 {
		return false, errors.New("subsidy authorization_ref is too long")
	}
	if len(reason) > 1000 {
		return false, errors.New("subsidy reason is too long")
	}
	if err := validateAdminActorShape(actor); err != nil {
		return false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return false, err
	}

	var (
		supplierID      uuid.UUID
		taskID          *uuid.UUID
		status          string
		liabilityMicros int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT supplier_id,task_id,payout_status,(amount_usd*1000000)::bigint
		  FROM ledger_entries
		 WHERE id=$1 AND kind='supplier_credit'
		 FOR UPDATE`, entryID,
	).Scan(&supplierID, &taskID, &status, &liabilityMicros); errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	} else if err != nil {
		return false, err
	}
	if status != PayoutHeld && status != PayoutReady && status != PayoutAwaitingFunding {
		return false, errNotHeld
	}
	amountCents, _, err := splitSupplierLiabilityMicros(liabilityMicros)
	if err != nil {
		return false, err
	}
	if amountCents <= 0 {
		return false, fmt.Errorf("supplier credit %s is carried below one cent and needs no subsidy cash authorization", entryID)
	}

	var (
		existingSource, existingFundRef, existingRef, existingReason, existingCurrency string
		existingAmount                                                                 int64
		existingFundID, existingActionID                                               uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT funding.source_kind,COALESCE(fund.id,'00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(funding.authorization_action_id,'00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(fund.fund_ref,''),
		       COALESCE(funding.subsidy_authorization_ref,''),
		       COALESCE(funding.subsidy_reason,''),funding.amount_cents,funding.currency
		  FROM supplier_payout_funding funding
		  LEFT JOIN platform_subsidy_funds fund ON fund.id=funding.subsidy_fund_id
		 WHERE funding.ledger_entry_id=$1
		 FOR UPDATE OF funding`, entryID,
	).Scan(&existingSource, &existingFundID, &existingActionID, &existingFundRef,
		&existingRef, &existingReason, &existingAmount, &existingCurrency)
	if err == nil {
		if existingSource == payoutFundingPlatformSubsidy && existingFundRef == fundRef &&
			existingRef == authorizationRef && existingReason == reason &&
			existingAmount == amountCents && existingCurrency == "usd" {
			intent := moneyAuthorityIntent{
				Kind: "payout_subsidy_authorized", TargetKind: "supplier_liability",
				TargetID: entryID, FundID: existingFundID, FundRef: fundRef,
				AuthorizationRef: authorizationRef, AmountCents: amountCents,
				Currency: "usd", Reason: reason, CorrelationRef: authorizationRef,
			}
			if err := assertMoneyAuthorityAction(ctx, tx, actor, existingActionID, intent); err != nil {
				return false, err
			}
			if status == PayoutAwaitingFunding {
				if _, err := tx.Exec(ctx,
					`UPDATE ledger_entries SET payout_status=$2
					  WHERE id=$1 AND payout_status=$3`,
					entryID, PayoutHeld, PayoutAwaitingFunding); err != nil {
					return false, err
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return false, err
			}
			return false, nil
		}
		return false, errPayoutFundingAlreadyBound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	var fundID, fundActionID uuid.UUID
	var capacity int64
	var fundTreasuryRef, fundCurrency, fundReason string
	if err := tx.QueryRow(ctx, `
		SELECT id,COALESCE(authorization_action_id,'00000000-0000-0000-0000-000000000000'::uuid),
		       external_treasury_ref,authorized_cents,currency,reason
		  FROM platform_subsidy_funds
		 WHERE fund_ref=$1 AND status='active' AND currency='usd'
		 FOR UPDATE`, fundRef,
	).Scan(&fundID, &fundActionID, &fundTreasuryRef, &capacity, &fundCurrency, &fundReason); errors.Is(err, pgx.ErrNoRows) {
		return false, errSubsidyFundUnavailable
	} else if err != nil {
		return false, err
	}
	fundIntent := moneyAuthorityIntent{
		Kind: "subsidy_fund_authorized", TargetKind: "subsidy_fund",
		TargetID: fundID, FundID: fundID, FundRef: fundRef,
		ExternalTreasuryRef: fundTreasuryRef, AmountCents: capacity,
		Currency: fundCurrency, Reason: fundReason, CorrelationRef: fundRef,
	}
	if err := assertMoneyAuthorityAction(ctx, tx, actor, fundActionID, fundIntent); err != nil {
		return false, err
	}
	var reserved int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_cents),0)::bigint
		  FROM supplier_payout_funding
		 WHERE source_kind='platform_subsidy' AND subsidy_fund_id=$1`, fundID,
	).Scan(&reserved); err != nil {
		return false, err
	}
	if reserved < 0 || reserved > capacity || amountCents > capacity-reserved {
		return false, errSubsidyFundUnavailable
	}

	var liabilityJobID *uuid.UUID
	if taskID != nil {
		var jobID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT job_id FROM tasks WHERE id=$1`, *taskID).Scan(&jobID); err != nil {
			return false, err
		}
		liabilityJobID = &jobID
	}
	actionID := uuid.New()
	intent := moneyAuthorityIntent{
		Kind: "payout_subsidy_authorized", TargetKind: "supplier_liability",
		TargetID: entryID, FundID: fundID, FundRef: fundRef,
		AuthorizationRef: authorizationRef, AmountCents: amountCents,
		Currency: "usd", Reason: reason, CorrelationRef: authorizationRef,
	}
	if _, err := insertMoneyAuthorityAction(ctx, tx, actor, actionID, intent, &supplierID); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errPayoutFundingAlreadyBound
		}
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO supplier_payout_funding
		  (authorization_action_id,ledger_entry_id,source_kind,liability_job_id,
		   subsidy_fund_id,subsidy_authorization_ref,subsidy_reason,amount_cents,currency)
		VALUES ($1,$2,'platform_subsidy',$3,$4,$5,$6,$7,'usd')`,
		actionID, entryID, liabilityJobID, fundID, authorizationRef, reason, amountCents); err != nil {
		if isPayoutFundingUniqueViolation(err) {
			return false, errPayoutFundingAlreadyBound
		}
		return false, err
	}
	if status == PayoutAwaitingFunding {
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2
			  WHERE id=$1 AND payout_status=$3`,
			entryID, PayoutHeld, PayoutAwaitingFunding); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// AdminReleasePayoutHold closes the "manually trigger a payout-hold release" gap
// named directly in the backlog rung (Operator Tooling 7->8). The endpoint accepts
// only 'held' and definitely-not-sent 'ready'. It never accepts awaiting_funding,
// sending, outcome_unknown, or reversal states. Either accepted state becomes a
// genuine 'held' row with release_at=now(), so the next sweep can claim it; this
// action never marks cash released and cannot bypass the durable provider result.
func (s *Store) AdminReleasePayoutHold(ctx context.Context, entryID uuid.UUID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var supplierID uuid.UUID
	if err := tx.QueryRow(ctx,
		`UPDATE ledger_entries SET payout_status = 'held', release_at = now()
		 WHERE id = $1 AND kind = 'supplier_credit' AND payout_status IN ('held','ready')
		 RETURNING supplier_id`,
		entryID,
	).Scan(&supplierID); errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if qerr := tx.QueryRow(ctx, `SELECT true FROM ledger_entries WHERE id = $1`, entryID).Scan(&exists); errors.Is(qerr, pgx.ErrNoRows) {
			return errNotFound
		} else if qerr != nil {
			return qerr
		}
		return errNotHeld
	} else if err != nil {
		return err
	}

	if err := recordAdminAction(ctx, tx, "payout_released", nil, &supplierID, &entryID, reason, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// errNotHeld distinguishes "ledger entry exists but is not currently held/ready"
// (e.g. already released, pending, or clawed back) from errNotFound.
var errNotHeld = errors.New("ledger entry is not held or ready")

// --- jobs + tasks ---

// CreateJobWithTasks inserts the job, immutable economics, optional webhook, and
// task rows in one transaction. Each task starts queued and immediately claimable
// (visible_at defaults to now() in the schema).
func (s *Store) CreateJobWithTasks(ctx context.Context, j *jobRow, tasks []taskRow) error {
	hasWebhook := j.WebhookID != uuid.Nil || j.WebhookURL != "" || j.WebhookSigningSecretSealed != ""
	if hasWebhook && (j.WebhookID == uuid.Nil || j.WebhookURL == "" ||
		!strings.HasPrefix(j.WebhookSigningSecretSealed, "enc:")) {
		return errors.New("job webhook requires id, url, and encrypted signing secret together")
	}
	if err := ValidateEconomicPlanSnapshot(j.EconomicPlan); err != nil {
		return fmt.Errorf("refusing job without valid economic plan: %w", err)
	}
	if j.EconomicPlan.Input.InitialTaskCount != len(tasks) {
		return fmt.Errorf("economic plan initial_task_count=%d does not match %d initial tasks",
			j.EconomicPlan.Input.InitialTaskCount, len(tasks))
	}
	if j.TaskCount != len(tasks) {
		return fmt.Errorf("job task_count=%d does not match %d initial tasks", j.TaskCount, len(tasks))
	}
	if j.EconomicInputSource == economicInputSourceSubmitStream {
		var primaryRecords int64
		for _, task := range tasks {
			if task.ExpectedOutputRecords <= 0 {
				return fmt.Errorf("exact streamed job task %s lacks expected output record authority", task.ID)
			}
			if !task.IsHoneypot && !task.IsRedundancy {
				primaryRecords += task.ExpectedOutputRecords
			}
		}
		if primaryRecords != j.EconomicInputRecords {
			return fmt.Errorf("primary task output records %d do not match exact streamed input records %d",
				primaryRecords, j.EconomicInputRecords)
		}
	}
	if math.Abs(j.EstimatedUSD-j.EconomicPlan.InitialBuyerChargeUSD) > 0.000001 {
		return fmt.Errorf("job estimate %.6f does not match frozen economic charge %.6f",
			j.EstimatedUSD, j.EconomicPlan.InitialBuyerChargeUSD)
	}
	if math.Abs(j.SLAPremiumUSD-j.EconomicPlan.Input.SLAPremiumUSD) > 0.000001 {
		return fmt.Errorf("job SLA premium %.6f does not match frozen economic premium %.6f",
			j.SLAPremiumUSD, j.EconomicPlan.Input.SLAPremiumUSD)
	}
	if j.FirmQuote {
		if j.FirmQuoteMaxUSD <= 0 || math.Abs(j.FirmQuoteMaxUSD-j.EconomicPlan.Input.FirmQuoteMaxUSD) > 0.000001 {
			return fmt.Errorf("firm quote max %.6f does not match frozen economic cap %.6f",
				j.FirmQuoteMaxUSD, j.EconomicPlan.Input.FirmQuoteMaxUSD)
		}
	} else if j.EconomicPlan.Input.FirmQuoteMaxUSD > 0 || j.FirmQuoteMaxUSD > 0 {
		return errors.New("non-firm job cannot carry a firm economic cap")
	}
	planJSON, err := json.Marshal(j.EconomicPlan)
	if err != nil {
		return fmt.Errorf("marshal economic plan: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// max_usd is NULL when no cap was set (0), so the claim's budget gate can use
	// `j.max_usd IS NOT NULL` to cleanly tell "no cap" from a real $0 cap. Same
	// pattern for firm_quote_max_usd (Project Detection & Quotation 7->8): NULL
	// means "not a firm quote", never a fake $0 ceiling.
	// Routing decision (rubric dimension 5): all four columns persist together,
	// keyed by RoutingSubstrate. "" → the job carried NO routing block (a
	// non-generative or empty-input job — the honesty boundary), so every routing
	// column stays NULL rather than a fake fleet/0s row the receipt would then
	// falsely project. When present, the numbers are bound verbatim (the fleet ETA
	// == the job's eta_secs; the GPU figure is [MODELED]).
	var rSubstrate, rReason, rFleetETA, rGPUModeled any
	if j.RoutingSubstrate != "" {
		rSubstrate = j.RoutingSubstrate
		rReason = nullStr(j.RoutingReason)
		rFleetETA = j.RoutingFleetETASecs
		rGPUModeled = j.RoutingGPUModeledSecs
	}
	// Exact input units are written only when the authoritative streaming submit
	// path supplied its source marker. Older/direct jobRow callers leave all three
	// NULL; a zero value without provenance must never become a fake measured zero.
	var economicInputRecords, economicInputBytes, economicInputSource any
	if j.EconomicInputSource != "" {
		economicInputRecords = j.EconomicInputRecords
		economicInputBytes = j.EconomicInputBytes
		economicInputSource = j.EconomicInputSource
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO jobs
		   (id, buyer_id, status, job_type, model_ref, input_ref, output_ref,
		    tier, verification_policy, estimated_usd, actual_usd, task_count, tasks_done,
		    min_memory_gb, max_duration_secs, hw_classes, data_residency, job_type_spec, split_size,
		    offered_rate_usd_hr, eta_secs, max_usd, budget_state, quote_id, min_reputation, private_pool,
		    deadline_secs, firm_quote, firm_quote_max_usd, sla_guarantee_secs, sla_premium_usd,
		    routing_substrate, routing_reason, routing_fleet_eta_secs, routing_gpu_modeled_secs,
		    economic_input_records, economic_input_bytes, economic_input_source)
		 VALUES ($1,$2,'queued',$3,$4,$5,$6,$7,$8,$9,0,$10,0,
		         $11,$12,$13,$14,$15,$16,$17,$18,$19,'tracking',$20,$21,$22,$23,$24,$25,$26,$27,
		         $28,$29,$30,$31,$32,$33,$34)`,
		j.ID, j.BuyerID, j.JobType, j.ModelRef, j.InputRef, j.OutputRef,
		j.Tier, j.VerificationPolicy, j.EstimatedUSD, j.TaskCount,
		j.MinMemoryGB, j.MaxDurationSecs, nullStrSlice(j.HWClasses), nullStrSlice(j.DataResidency),
		nullJSON(j.JobTypeSpec), j.SplitSize, j.OfferedRateUsdHr, j.ETASecs,
		nullPosFloat(j.MaxUSD), nullUUID(j.QuoteID), j.MinReputation, j.PrivatePool,
		j.DeadlineSecs, j.FirmQuote, nullPosFloat(j.FirmQuoteMaxUSD),
		nullPosInt(j.SLAGuaranteeSecs), nullPosFloat(j.SLAPremiumUSD),
		rSubstrate, rReason, rFleetETA, rGPUModeled,
		economicInputRecords, economicInputBytes, economicInputSource,
	)
	if err != nil {
		return err
	}
	if hasWebhook {
		if _, err := tx.Exec(ctx, `
			INSERT INTO webhooks (id,buyer_id,job_id,url,signing_secret_sealed)
			VALUES ($1,$2,$3,$4,$5)`,
			j.WebhookID, j.BuyerID, j.ID, j.WebhookURL, j.WebhookSigningSecretSealed); err != nil {
			return fmt.Errorf("insert job webhook: %w", err)
		}
	}
	// The immutable plan references jobs(job_id), so insert its parent first
	// inside this same transaction. Job, plan, reserve, and tasks still become
	// visible atomically only after the final commit.
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_economic_plans (
		  job_id,plan_version,schedule_version,plan_json,initial_task_count,
		  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd,firm_quote_max_usd
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		j.ID, j.EconomicPlan.Version, j.EconomicPlan.Schedule.Version, planJSON,
		j.EconomicPlan.Input.InitialTaskCount, j.EconomicPlan.BuyerChargePerTaskUSD,
		j.EconomicPlan.SupplierPayoutPerTaskUSD, j.EconomicPlan.InitialBuyerChargeUSD,
		j.EconomicPlan.ReservedBuyerChargeUSD, j.EconomicPlan.Input.SLAPremiumUSD,
		nullPosFloat(j.EconomicPlan.Input.FirmQuoteMaxUSD)); err != nil {
		return fmt.Errorf("insert economic plan: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_economic_reserves (job_id,reserved_tasks,consumed_tasks)
		VALUES ($1,$2,0)`, j.ID, j.EconomicPlan.Input.ExtraTaskReserve); err != nil {
		return fmt.Errorf("insert economic reserve: %w", err)
	}

	// PATCH (Control plane hot path 8->9, docs/internal/CREED_AND_PATH_TO_TEN.md
	// "batch large-job inserts via pgx CopyFrom instead of row-by-row insert"):
	// this used to be one round-trip INSERT per task, inside the SAME transaction
	// a large job's buyer-facing submit request waits on — O(task count)
	// sequential round-trips synchronously in the request path, so a
	// multi-thousand-task job paid a multi-thousand-statement transaction before
	// the buyer's POST /v1/jobs ever returned. pgx's CopyFrom streams every row
	// over the wire in the Postgres COPY binary protocol in ONE operation —
	// still inside this same transaction (so a mid-copy failure rolls back the
	// whole job exactly as before; nothing partially lands), but without a
	// round-trip per row. visible_at/status/retry_count were server-side
	// DEFAULTs the old per-row INSERT relied on implicitly; CopyFrom has no
	// DEFAULT-substitution, so they are bound explicitly per row below —
	// status='queued', retry_count=0, and visible_at pinned to ONE now() read up
	// front so every row in the copy shares the identical timestamp a single
	// INSERT statement's now() would have produced.
	if len(tasks) > 0 {
		now := time.Now()
		_, err = tx.CopyFrom(ctx,
			pgx.Identifier{"tasks"},
			[]string{"id", "job_id", "status", "is_honeypot", "is_redundancy", "retry_count",
				"input_ref", "result_key", "chunk_index", "expected_output_records", "visible_at",
				"economic_buyer_charge_usd", "economic_supplier_payout_usd"},
			pgx.CopyFromSlice(len(tasks), func(i int) ([]any, error) {
				t := tasks[i]
				return []any{t.ID, t.JobID, "queued", t.IsHoneypot, t.IsRedundancy, int16(0),
					t.InputRef, t.ResultKey, t.ChunkIndex, nullPosInt64(t.ExpectedOutputRecords), now,
					j.EconomicPlan.BuyerChargePerTaskUSD, j.EconomicPlan.SupplierPayoutPerTaskUSD}, nil
			}),
		)
		if err != nil {
			return fmt.Errorf("copy tasks: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// jobRow mirrors the jobs table columns we write. The Turbo fields (MinMemoryGB,
// HWClasses, DataResidency, JobTypeSpec, SplitSize, OfferedRateUsdHr, ETASecs)
// lift per-job constraints out of the manifest JSON into queryable columns so the
// SKIP-LOCKED claim can hard-filter on them.
type jobRow struct {
	ID                 uuid.UUID
	BuyerID            uuid.UUID
	JobType            string
	ModelRef           string
	InputRef           string
	OutputRef          string
	Tier               string
	VerificationPolicy []byte // jsonb
	EstimatedUSD       float64
	TaskCount          int
	MinMemoryGB        float32
	MaxDurationSecs    uint32
	HWClasses          []string // nil = any class
	DataResidency      []string // nil = unrestricted
	JobTypeSpec        []byte   // jsonb: the full submitted JobType (tag + fields)
	SplitSize          int
	OfferedRateUsdHr   float32
	ETASecs            int
	MaxUSD             float64   // buyer hard spend cap (Budget Governor); 0 = no cap
	QuoteID            uuid.UUID // advisory quote bound to this job (Plane D D7); zero = none → persisted NULL
	MinReputation      float32   // Elite-supplier gate: claim only by suppliers with reputation >= this (0 = any)
	PrivatePool        bool      // Private Deployment: route ONLY to the buyer's bound suppliers (private_pool_members)
	DeadlineSecs       int       // watchdog policy: -1 opt out, 0 default, 60..604800 explicit wall-clock deadline
	// FirmQuote / FirmQuoteMaxUSD: the firm-quote tier (Project Detection &
	// Quotation 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md). When FirmQuote is
	// true, FirmQuoteMaxUSD is the real ceiling the buyer's eventual charge is
	// capped at (JobChargeInfo), with any overage absorbed by the platform.
	FirmQuote       bool
	FirmQuoteMaxUSD float64
	// SLAGuaranteeSecs / SLAPremiumUSD: the wall-clock speed-SLA binding (Speed
	// Lane wave 2A). When > 0 the job's results are guaranteed merged within
	// SLAGuaranteeSecs of created_at; a miss auto-refunds SLAPremiumUSD via an
	// sla_refund ledger row (collect.go settleSLAOutcome). 0 = no SLA → NULL.
	SLAGuaranteeSecs int
	SLAPremiumUSD    float64
	// EconomicInput*: exact full-stream submit facts. Source="" keeps all three
	// columns NULL for legacy/direct callers rather than inventing zero units.
	EconomicInputRecords int64
	EconomicInputBytes   int64
	EconomicInputSource  string
	EconomicPlan         EconomicPlan
	// Webhook* is an all-or-none optional registration inserted in the same
	// transaction as the job. Only the sealed secret crosses this data boundary.
	WebhookID                  uuid.UUID
	WebhookURL                 string
	WebhookSigningSecretSealed string
	// Routing*: the SUBSTRATE-ROUTING decision stamped at submit (Speed Lane
	// road-to-ten rubric dimension 5, control/routing.go + quote.go). Present
	// only for GENERATIVE jobs with records > 0 — the honesty boundary the quote
	// path enforces (the A100 sweep measured generative decode only). Persisted so
	// the clearing receipt can project the "we ran it on X because Y" row
	// deterministically. RoutingSubstrate "" → all four persist NULL (no block).
	// RoutingGPUModeledSecs is ALWAYS [MODELED] (the sweep's aggregate tok/s at
	// this job's shape, excluding rental/provisioning) — never a measurement.
	RoutingSubstrate      string
	RoutingReason         string
	RoutingFleetETASecs   int
	RoutingGPUModeledSecs float64
}

// taskRow mirrors the tasks columns we write at creation. Each task is one chunk
// of the job's split input, so it carries its own object keys: input_ref is the
// chunk's input.jsonl key, result_key is where the worker writes its result.json.
// ChunkIndex is the 0-based input position used to merge results back in order;
// redundancy/honeypot clones reuse their primary's chunk_index.
type taskRow struct {
	ID                    uuid.UUID
	JobID                 uuid.UUID
	IsHoneypot            bool
	IsRedundancy          bool
	InputRef              string
	ResultKey             string
	ChunkIndex            int
	ExpectedOutputRecords int64 // 0 = explicit legacy/opaque unknown, persisted NULL
}

// JobView is the GET /v1/jobs/{id} projection. MaxUSD/BudgetState expose the
// Budget Governor (Plane C §12 / Plane D §14 D8): MaxUSD is the buyer's hard cap
// (0 when unset) and BudgetState is the governor state machine.
type JobView struct {
	ID           uuid.UUID
	BuyerID      uuid.UUID
	Status       string
	JobType      string
	Tier         string
	OutputRef    string
	TaskCount    int
	TasksDone    int
	EstimatedUSD float64
	ActualUSD    float64
	ETASecs      int
	CreatedAt    time.Time
	MaxUSD       float64
	BudgetState  string
	ChargeStatus string
	Verification Verification
	// ResultsMergedAt is the results-merge watermark: nil until the buyer-ready
	// artifact has actually been merged at least once (Data Transfer & Artifact
	// I/O 4.5->5, "Stop paying for every poll twice"). handleJobResults skips
	// re-merging on read once this is set.
	ResultsMergedAt *time.Time
	// SLAGuaranteeSecs / SLAPremiumUSD / SLAMet: the wall-clock speed-SLA (wave
	// 2A). Guarantee 0 = no SLA bound. SLAMet is nil until the outcome is decided
	// at finalize (or forever, for a job with no SLA); true = met, false = missed
	// (an sla_refund ledger credit for the premium was recorded).
	SLAGuaranteeSecs int
	SLAPremiumUSD    float64
	SLAMet           *bool
}

// GetJob loads a job scoped to a buyer (buyers see only their own jobs).
func (s *Store) GetJob(ctx context.Context, jobID, buyerID uuid.UUID) (*JobView, error) {
	var j JobView
	err := s.pool.QueryRow(ctx,
		`SELECT id, buyer_id, status, job_type, tier, COALESCE(output_ref,''),
		        COALESCE(task_count,0), COALESCE(tasks_done,0),
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0),
		        COALESCE(eta_secs,0), created_at,
		        COALESCE(max_usd,0), COALESCE(budget_state,'tracking'),
		        COALESCE(charge_status,'not_attempted'), results_merged_at,
		        COALESCE(sla_guarantee_secs,0), COALESCE(sla_premium_usd,0)::float8, sla_met
		 FROM jobs WHERE id = $1 AND buyer_id = $2`,
		jobID, buyerID,
	).Scan(&j.ID, &j.BuyerID, &j.Status, &j.JobType, &j.Tier, &j.OutputRef,
		&j.TaskCount, &j.TasksDone, &j.EstimatedUSD, &j.ActualUSD, &j.ETASecs, &j.CreatedAt,
		&j.MaxUSD, &j.BudgetState, &j.ChargeStatus, &j.ResultsMergedAt,
		&j.SLAGuaranteeSecs, &j.SLAPremiumUSD, &j.SLAMet)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	// Assemble the verification receipt (counts from the append-only log + latest
	// dispute). A read failure here must not hide the job: log and leave the
	// zero-value aggregate (label "unverified"), never fabricate counts.
	vr, verr := s.JobVerification(ctx, j.ID)
	if verr != nil {
		log.Printf("job verification aggregate (job %s): %v", j.ID, verr)
		vr.Label = deriveVerificationLabel(vr)
	}
	j.Verification = vr
	return &j, nil
}

// QueuedTaskCount is the number of claimable (queued/retrying, visible-now,
// unclaimed) tasks across all non-terminal jobs — the backlog the ETA estimate
// and the /metrics queue-depth gauge read.
func (s *Store) QueuedTaskCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks t JOIN jobs j ON j.id = t.job_id
		 WHERE t.status IN ('queued','retrying')
		   AND t.claimed_by IS NULL
		   AND COALESCE(t.visible_at, t.created_at) <= now()
		   AND j.status NOT IN ('cancelled','failed','complete')`,
	).Scan(&n)
	return n, err
}

// jobInternal carries the fields payout scheduling needs (not buyer-scoped).
type jobInternal struct {
	BuyerID            uuid.UUID
	TaskCount          int
	EstimatedUSD       float64
	VerificationPolicy []byte
}

// getJobInternal loads the payout-relevant job fields by id (no buyer scope;
// called from the worker commit path).
func (s *Store) getJobInternal(ctx context.Context, jobID uuid.UUID) (*jobInternal, error) {
	var j jobInternal
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id, COALESCE(task_count,0), COALESCE(estimated_usd,0),
		        COALESCE(verification_policy,'{}'::jsonb)
		 FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&j.BuyerID, &j.TaskCount, &j.EstimatedUSD, &j.VerificationPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// TaskEconomicAmounts returns the immutable buyer/supplier amounts stamped on
// this exact task at admission (or dynamic reserve authorization). Legacy NULL
// rows are an error: settlement must fail closed rather than revive the mutable
// estimated_usd/task_count formula.
func (s *Store) TaskEconomicAmounts(ctx context.Context, taskID uuid.UUID) (buyerCharge, supplierPayout float64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT economic_buyer_charge_usd::float8,economic_supplier_payout_usd::float8
		  FROM tasks WHERE id=$1`, taskID).Scan(&buyerCharge, &supplierPayout)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, errNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("task %s has no frozen economic amounts: %w", taskID, err)
	}
	return buyerCharge, supplierPayout, nil
}

// CancelJob cancels a job only if it has not started (still queued). Returns
// errNotFound if no such cancellable job exists for the buyer.
func (s *Store) CancelJob(ctx context.Context, jobID, buyerID uuid.UUID) error {
	status, pending, err := s.jobTerminalTransitionState(ctx, jobID)
	if err != nil {
		return err
	}
	if status != "queued" || pending {
		return errNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockUnfinishedJobTasksTx(ctx, tx, jobID); err != nil {
		return err
	}
	status, pending, err = jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return err
	}
	var owns bool
	if err := tx.QueryRow(ctx, `SELECT buyer_id=$2 FROM jobs WHERE id=$1`, jobID, buyerID).Scan(&owns); err != nil {
		return err
	}
	if status != "queued" || !owns || pending {
		return errNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, jobID); err != nil {
		return err
	}
	// Drop the still-queued tasks in the same parent-fenced transaction so a
	// concurrent claim/commit cannot observe a cancelled job with live work.
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'failed'
		 WHERE job_id = $1 AND status = 'queued'`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// StartTask marks a claimed task running. Scoped to the worker that owns the
// claim so a worker cannot start another worker's task.
func (s *Store) StartTask(ctx context.Context, taskID, workerID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Freeze mutable registration axes under the same worker-row lock ClaimTask
	// uses. Taking this before the task lock preserves a single worker -> task ->
	// job lock order for the legacy explicit-start path.
	var claimSupplierID uuid.UUID
	var claimHWClass, claimEngine, claimBuildHash string
	err = tx.QueryRow(ctx, `
		SELECT supplier_id,COALESCE(hw_class,''),COALESCE(engine,''),COALESCE(build_hash,'')
		  FROM workers WHERE id=$1 FOR UPDATE`, workerID).
		Scan(&claimSupplierID, &claimHWClass, &claimEngine, &claimBuildHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(claimHWClass) == "" || strings.TrimSpace(claimEngine) == "" {
		return errNotFound
	}

	var (
		jobID               uuid.UUID
		status              string
		claimedBy           *uuid.UUID
		taskWorkerID        *uuid.UUID
		executionWorkerID   *uuid.UUID
		executionSupplierID *uuid.UUID
		startedAt           *time.Time
		isRedundancy        bool
		hedgedFrom          *uuid.UUID
		attempt             int16
	)
	err = tx.QueryRow(ctx, `
		SELECT job_id,status,claimed_by,worker_id,execution_worker_id,execution_supplier_id,started_at,
		       COALESCE(is_redundancy,false),hedged_from,COALESCE(retry_count,0)
		  FROM tasks WHERE id=$1 FOR UPDATE`, taskID).
		Scan(&jobID, &status, &claimedBy, &taskWorkerID, &executionWorkerID, &executionSupplierID, &startedAt,
			&isRedundancy, &hedgedFrom, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if claimedBy == nil || *claimedBy != workerID || (status != "queued" && status != "running") {
		return errNotFound
	}

	// Start, commit, apply, and terminal parent transitions share task -> parent
	// ordering (this compatibility path additionally owns its worker lock first).
	// Keeping the parent transition in this transaction closes the old
	// gap where a task became running before its queued parent became running.
	var parentStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&parentStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	if parentStatus != "queued" && parentStatus != "running" && parentStatus != "verifying" {
		return errNotFound
	}

	dynamicTiebreak := isRedundancy && hedgedFrom != nil
	if status == "running" {
		// ClaimTask already performs the eligibility check, starts the task, and
		// inserts tiebreak execution history atomically. The explicit /start call
		// made by an agent afterward is an exact idempotent acknowledgement only.
		if taskWorkerID == nil || *taskWorkerID != workerID ||
			executionWorkerID == nil || *executionWorkerID != workerID || executionSupplierID == nil {
			return errNotFound
		}
		if dynamicTiebreak {
			var historyExact bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
				 SELECT 1 FROM task_execution_history h
				  WHERE h.task_id=$1 AND h.attempt=$2 AND h.worker_id=$3
				    AND h.supplier_id=$4
				)`, taskID, attempt, workerID, *executionSupplierID).Scan(&historyExact); err != nil {
				return err
			}
			if !historyExact {
				return errNotFound
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running' WHERE id=$1 AND status='queued'`, jobID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if taskWorkerID != nil && *taskWorkerID != workerID {
		return errNotFound
	}
	if dynamicTiebreak {
		if startedAt != nil {
			return errNotFound
		}
		eligible, err := tiebreakPeerClaimEligibleTx(ctx, tx, taskID, jobID, workerID)
		if err != nil {
			return err
		}
		if !eligible {
			return errNotFound
		}
		ct, err := tx.Exec(ctx, `
			INSERT INTO task_execution_history (task_id,attempt,worker_id,supplier_id)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (task_id,attempt,worker_id) DO NOTHING`,
			taskID, attempt, workerID, claimSupplierID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return errNotFound
		}
	}

	ct, err := tx.Exec(ctx, `
		UPDATE tasks SET status='running',started_at=now(),worker_id=$2,
		       execution_worker_id=$2,execution_supplier_id=$3,
		       execution_hw_class=$4,execution_engine=$5,execution_build_hash=$6
		 WHERE id=$1 AND claimed_by=$2 AND status='queued'`,
		taskID, workerID, claimSupplierID, claimHWClass, claimEngine, claimBuildHash)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='running' WHERE id=$1 AND status='queued'`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CommitTaskInfo is what CommitTask returns so verification can run.
type CommitTaskInfo struct {
	TaskID       uuid.UUID
	JobID        uuid.UUID
	WorkerID     uuid.UUID
	SupplierID   uuid.UUID
	IsHoneypot   bool
	IsRedundancy bool
	HWClass      string
	// engine + buildHash are the finer verification-class axes of the COMMITTING
	// worker (alongside HWClass). The verifier uses the full (hw_class, engine,
	// build_hash) class to decide whether a byte-exact redundancy/honeypot
	// comparison is even meaningful: a pure byte mismatch ACROSS the class boundary
	// is not an auto-dock (two engines / two builds legitimately differ in bytes on
	// identical hardware) — it falls back to provisional trust, mirroring the
	// missing-third-worker pattern. Unexported: internal to verification.
	engine       string
	buildHash    string
	jobType      string // parent job's job_type, for honeypot answer lookup
	jobMaxTokens uint32 // bounded projection of jobs.job_type_spec.max_tokens
	// resultMaxBytes is frozen into the attempt snapshot. A restart therefore
	// applies the same artifact policy even after a control-plane upgrade.
	resultMaxBytes int64
	InputRef       string // this task's input chunk key (honeypot answer lookup)
	ResultKey      string // canonical server-side result key (verification fetch)
	ModelRef       string // parent job's model_ref (tiebreak peer selection)
	MinMemoryGB    float32
	ChunkIndex     int // this task's chunk position (tiebreak pairing + N-way vote)
	SplitSize      int
	// ExpectedOutputRecords is the immutable exact cardinality captured while the
	// input chunk was streamed. Zero is explicit legacy/opaque unknown and keeps
	// the conservative <= split-size compatibility contract.
	ExpectedOutputRecords int64
	Attempt               int16
	DurationMS            uint64
	TokensUsed            uint64
	ResultSHA256          string
	hardwareTempC         *float32
	// verificationCheckSampled is the once-persisted audit choice from the
	// immutable verification work plan. Nil preserves direct/unit legacy behavior;
	// recovery processors always set it so reputation changes cannot change a
	// retried attempt's audit branch.
	verificationCheckSampled *bool
	// peerSupplierID is the redundancy peer's supplier, set by the commit handler
	// when a sibling result exists. Used only by the no-object-store verification
	// fallback so a 2-blob disagreement docks the RIGHT supplier (uuid.Nil = unknown).
	peerSupplierID uuid.UUID
	// peerEngine + peerBuildHash are the redundancy peer's finer verification class,
	// also set by the commit handler, so the no-object-store fallback can tell a
	// same-class byte mismatch (a real defect) from a cross-class one (provisional
	// trust). Blank = unknown peer class → a byte-exact pair is non-comparable.
	peerEngine    string
	peerBuildHash string
}

// CommitTask stores the result ref and flips the task to verifying. It does NOT
// mark work complete, increment counters, write duration telemetry, or create
// money rows; FinalizeTaskVerification does all of those atomically only after a
// durable verdict. Returns the context verification needs. Scoped to the claiming
// worker. The stored
// result_ref is the canonical server-side result_key (the key the control plane
// presigned at dispatch), NOT whatever the worker echoes in the commit body — the
// worker's TaskCommit.result_key is a presigned URL in V1, so trusting it would
// store a URL where a key belongs. We trust the path + our own dispatch record.
func (s *Store) CommitTask(ctx context.Context, taskID, workerID uuid.UUID, c TaskCommit) (*CommitTaskInfo, error) {
	return s.commitTask(ctx, taskID, workerID, c, nil)
}

func (s *Store) commitTask(ctx context.Context, taskID, workerID uuid.UUID, c TaskCommit, probe recoveryBoundaryProbe) (*CommitTaskInfo, error) {
	if c.DurationMS > math.MaxInt64 || c.TokensUsed > math.MaxInt64 {
		return nil, fmt.Errorf("reported duration/tokens exceed durable range")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Control Plane Hot Path 8->9 (docs/internal/CREED_AND_PATH_TO_TEN.md, "Get
	// result-commit off the S3 critical path"): validate the worker-supplied
	// SHA-256 shape before persisting it — a malformed value (wrong length,
	// non-hex) is stored as SQL NULL (never trusted downstream) rather than an
	// unusable garbage string a later hash-compare would silently never match.
	resultSHA256 := nullSHA256Hex(c.ResultSHA256)

	ct, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'verifying',
		       completed_at = CASE WHEN status='verifying' THEN completed_at ELSE NULL END,
		       result_ref = CASE WHEN status='verifying' THEN result_ref ELSE COALESCE(NULLIF(result_key,''), $3) END,
		       worker_id = CASE WHEN status='verifying' THEN worker_id ELSE $2 END,
		       result_sha256 = CASE WHEN status='verifying' THEN result_sha256 ELSE $4 END,
		       reported_duration_ms = CASE WHEN status='verifying' THEN reported_duration_ms ELSE $5 END,
		       reported_tokens_used = CASE WHEN status='verifying' THEN reported_tokens_used ELSE $6 END,
		       reported_hardware_temp_c = CASE WHEN status='verifying' THEN reported_hardware_temp_c ELSE $7 END,
		       verification_outcome = CASE WHEN status='verifying' THEN verification_outcome ELSE NULL END,
		       verified_at = CASE WHEN status='verifying' THEN verified_at ELSE NULL END
		 WHERE id = $1 AND claimed_by = $2 AND execution_worker_id = $2
		   AND (status IN ('running','queued') OR (status = 'verifying' AND worker_id = $2))`,
		taskID, workerID, c.ResultKey, resultSHA256, int64(c.DurationMS), int64(c.TokensUsed), c.HardwareTempC)
	if err != nil {
		return nil, err
	}
	if ct.RowsAffected() == 0 {
		return nil, errNotFound
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterTaskProjection)

	var info CommitTaskInfo
	var jobMaxTokens int64
	info.TaskID = taskID
	err = tx.QueryRow(ctx,
		`SELECT t.job_id, t.is_honeypot, t.is_redundancy,
		        COALESCE(t.input_ref,''),
		        COALESCE(NULLIF(t.result_key,''), $3),
		        t.execution_worker_id,t.execution_supplier_id,t.execution_hw_class,
		        t.execution_engine,t.execution_build_hash,j.job_type,
		        COALESCE((j.job_type_spec->>'max_tokens')::bigint,0),
		        COALESCE(j.model_ref,''), COALESCE(j.min_memory_gb,0),
		        COALESCE(t.chunk_index,0), COALESCE(j.split_size,0),
		        COALESCE(t.expected_output_records,0),
		        COALESCE(t.retry_count,0), COALESCE(t.result_sha256,'')
	 FROM tasks t JOIN jobs j ON j.id = t.job_id
	 WHERE t.id = $1 AND t.execution_worker_id=$2`,
		taskID, workerID, c.ResultKey,
	).Scan(&info.JobID, &info.IsHoneypot, &info.IsRedundancy, &info.InputRef,
		&info.ResultKey, &info.WorkerID, &info.SupplierID, &info.HWClass, &info.engine, &info.buildHash, &info.jobType, &jobMaxTokens,
		&info.ModelRef, &info.MinMemoryGB, &info.ChunkIndex, &info.SplitSize,
		&info.ExpectedOutputRecords, &info.Attempt, &info.ResultSHA256)
	if err != nil {
		return nil, err
	}
	if jobMaxTokens < 0 || jobMaxTokens > int64(^uint32(0)) {
		return nil, fmt.Errorf("job max_tokens %d exceeds artifact-policy range", jobMaxTokens)
	}
	info.jobMaxTokens = uint32(jobMaxTokens)
	info.resultMaxBytes = verificationArtifactMaxBytesForRecords(
		info.jobType, info.ExpectedOutputRecords, info.SplitSize, info.jobMaxTokens,
	)

	// Commit, apply, and every terminal job transition share task -> parent lock
	// order. Without this parent fence a sibling could fail/cancel the job after
	// this transaction changed the task to verifying but before that uncommitted
	// row/work was visible; the commit would then strand durable verification under
	// a terminal parent. Whichever transaction gets the parent lock first now wins
	// coherently: a terminal parent rolls this upload projection back, while a
	// committed upload makes terminal paths observe unresolved verification work.
	var parentStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, info.JobID).Scan(&parentStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	if parentStatus != "queued" && parentStatus != "running" && parentStatus != "verifying" {
		return nil, errNotFound
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterParentFence)

	// Once every remaining task has uploaded and is awaiting a verdict, expose the
	// parent as verifying. A later rejection/requeue moves it back to running.
	if _, err := tx.Exec(ctx,
		`UPDATE jobs j SET status = 'verifying'
		 WHERE j.id = $1 AND j.status IN ('queued','running')
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks t
		     WHERE t.job_id = j.id AND t.status IN ('queued','retrying','running')
		   )`, info.JobID); err != nil {
		return nil, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterJobProjection)

	info.DurationMS = c.DurationMS
	info.TokensUsed = c.TokensUsed
	info.hardwareTempC = c.HardwareTempC
	snapshot, err := verificationWorkSnapshotFromCommit(&info, c)
	if err != nil {
		return nil, err
	}
	if err := createVerificationWorkTx(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterWorkInsert)

	reachRecoveryBoundary(ctx, probe, BoundaryCommitBeforeDBCommit)
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCommitAfterDBCommit)
	return &info, nil
}

// driftMinSamples is how many committed durations a (job_type, model_ref) needs
// before an observed p90 is trusted enough to replace the static throughput target
// in the quote's ETA. Below it the history is too thin to be representative, and
// the estimate cleanly falls back to the target (HistoricalP90DurationMs reports 0).
const driftMinSamples = 5

// driftWindow bounds both HistoricalP90DurationMs and DriftRollup to recent history
// (Performance Observability 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md: "the
// historical p90 duration calculation... aggregates all-time history with no time
// window, so detection latency actually grows worse as the table grows"). Before
// this, a build shipped a month ago and a build shipped an hour ago were blended
// into the SAME p90 for as long as task_durations kept both rows — a fresh
// regression got diluted in direct proportion to how much older, healthy history
// sat in the same table, and that dilution could only get worse over time as more
// old rows accumulated. 24h matches the rung's own stated example window and is
// wide enough to still clear driftMinSamples on a realistically busy job_type
// while being narrow enough that a regression introduced this hour dominates the
// window instead of being averaged away by last month.
const driftWindow = 24 * time.Hour

// HistoricalP90DurationMs returns the observed 90th-percentile committed-task
// duration (ms) for a (job_type, model_ref) over the last driftWindow, and how
// many samples backed it. It uses percentile_disc(0.9) (a real recorded value, not
// an interpolation) over task_durations, filtered to created_at > now() -
// driftWindow so old, no-longer-representative history cannot dilute a recent
// regression. When fewer than driftMinSamples rows exist IN THE WINDOW the history
// is too thin to trust, so it returns (0, n) and the caller falls back to the
// static target — the quote NEVER invents an ETA from one lucky sample, and it
// never reaches past the window to manufacture samples either. A model_ref of ""
// matches rows regardless of model (job-type-only history).
func (s *Store) HistoricalP90DurationMs(ctx context.Context, jobType, modelRef string) (p90ms int64, samples int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*),
		        COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY duration_ms), 0)
		   FROM task_durations
		  WHERE job_type = $1
		    AND ($2 = '' OR model_ref = $2)
		    AND created_at > now() - make_interval(secs => $3)`,
		jobType, modelRef, int(driftWindow.Seconds()),
	).Scan(&samples, &p90ms)
	if err != nil {
		return 0, 0, err
	}
	if samples < driftMinSamples {
		return 0, samples, nil // too thin — caller falls back to the static target
	}
	return p90ms, samples, nil
}

// DriftRow is one per-(job_type, model) quoted-vs-actual rollup for GET /admin/drift
// (Plane D D6). Actuals come from recorded committed durations; AvgQuotedETASecs is
// the average of jobs.eta_secs (the quoted ETA, REUSED — not a duplicate column) over
// the jobs that produced those durations, so an operator can see how the static quote
// compares to reality and whether the observed p90 is now driving the estimate.
type DriftRow struct {
	JobType          string  `json:"job_type"`
	ModelRef         string  `json:"model_ref"`
	Samples          int     `json:"samples"`             // committed-task durations recorded IN THE WINDOW (see WindowHours)
	AvgDurationMs    float64 `json:"avg_duration_ms"`     // mean actual per-task wall-time, windowed
	P90DurationMs    int64   `json:"p90_duration_ms"`     // observed p90 (what the ETA learns from), windowed
	AvgQuotedETASecs float64 `json:"avg_quoted_eta_secs"` // mean quoted whole-job eta_secs (reused jobs.eta_secs), windowed
	UsingObservedP90 bool    `json:"using_observed_p90"`  // true once samples >= the trust floor
	WindowHours      float64 `json:"window_hours"`        // the rolling window every figure above is bounded to (driftWindow) — named explicitly so a reader never mistakes this for all-time history
}

// DriftRollup returns the quoted-vs-actual rollup per (job_type, model_ref) for the
// admin drift surface, over the last driftWindow (Performance Observability 7->8:
// "the historical p90 duration calculation... aggregates all-time history with no
// time window, so detection latency actually grows worse as the table grows").
// Actuals (count, avg, p90) come from task_durations, filtered to created_at > now()
// - driftWindow; the quoted side is the average jobs.eta_secs over the SAME windowed
// set of jobs (LEFT JOIN so a duration whose job row was pruned still counts its
// actual side honestly rather than vanishing). Ordered by sample volume so the
// thickest recent history — the rows whose observed p90 is actually steering
// quotes right now — sorts first. A (job_type, model_ref) with zero rows in the
// window simply does not appear (it never did pre-window either — COUNT(*) was
// always >= 1 to produce a GROUP BY row), so an operator reading a shrunken table
// after a quiet period is expected behavior, not a bug.
func (s *Store) DriftRollup(ctx context.Context) ([]DriftRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT td.job_type,
		        COALESCE(td.model_ref,''),
		        COUNT(*),
		        COALESCE(AVG(td.duration_ms),0),
		        COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY td.duration_ms),0),
		        COALESCE(AVG(j.eta_secs),0)
		   FROM task_durations td
		   LEFT JOIN jobs j ON j.id = td.job_id
		  WHERE td.created_at > now() - make_interval(secs => $1)
		  GROUP BY td.job_type, td.model_ref
		  ORDER BY COUNT(*) DESC, td.job_type, td.model_ref`,
		int(driftWindow.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriftRow
	for rows.Next() {
		var d DriftRow
		if err := rows.Scan(&d.JobType, &d.ModelRef, &d.Samples,
			&d.AvgDurationMs, &d.P90DurationMs, &d.AvgQuotedETASecs); err != nil {
			return nil, err
		}
		d.UsingObservedP90 = d.Samples >= driftMinSamples
		d.WindowHours = driftWindow.Hours()
		out = append(out, d)
	}
	return out, rows.Err()
}

// JobAllTasksDone reports whether every task of a job has finished
// (complete/failed) with at least one complete, i.e. the job is ready to finalize.
// Used by the commit path to merge-then-mark synchronously on the last commit.
func (s *Store) JobAllTasksDone(ctx context.Context, jobID uuid.UUID) (bool, error) {
	var done bool
	err := s.pool.QueryRow(ctx,
		`SELECT j.task_count > 0
		        AND NOT EXISTS (
		          SELECT 1 FROM tasks t
		          WHERE t.job_id = j.id AND t.status NOT IN ('complete','failed')
		        )
		        AND EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.id AND t.status = 'complete')
		 FROM jobs j WHERE j.id = $1`,
		jobID,
	).Scan(&done)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errNotFound
	}
	return done, err
}

// quarantineRepFloor is the reputation below which a supplier is auto-quarantined
// (suspended) so the claim's s.status='active' gate stops handing it work. Above
// the instant-ban threshold (0.0); a quarantined supplier can be reinstated.
const quarantineRepFloor = 0.2

// QuarantineSupplier suspends a supplier and stamps quarantined_at = now(),
// unconditionally (the honeypot-fail path quarantines on a single confirmed bad
// known-answer result, independent of the resulting reputation). A no-op on an
// already-banned supplier (don't downgrade a ban to a suspension). Idempotent for
// an already-suspended one (quarantined_at is only set if not already set).
func (s *Store) QuarantineSupplier(ctx context.Context, supplierID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE suppliers
		   SET status = 'suspended',
		       quarantined_at = COALESCE(quarantined_at, now())
		 WHERE id = $1 AND status <> 'banned'`,
		supplierID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() > 0 {
		metrics.quarantines.Add(1)
	}
	return nil
}

// DockReputation applies a reputation event to a supplier, computing the new
// score with the pure updateReputation (the single source of the delta + clamp
// rules) in a read-modify-write transaction. A score that collapses to 0.0
// (e.g. a spoofing event, delta -1.0) also flips the supplier to banned, since
// that is the action plan's instant-ban threshold; a score below
// quarantineRepFloor auto-suspends (quarantines) the supplier. Used by
// verification on honeypot/redundancy outcomes.
func (s *Store) DockReputation(ctx context.Context, supplierID uuid.UUID, event ReputationEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var current float32
	err = tx.QueryRow(ctx,
		`SELECT reputation FROM suppliers WHERE id = $1 FOR UPDATE`, supplierID,
	).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}

	next := updateReputation(current, event)
	switch {
	case next <= 0.0:
		// Instant-ban threshold reached.
		_, err = tx.Exec(ctx,
			`UPDATE suppliers SET reputation = 0.0, status = 'banned' WHERE id = $1`,
			supplierID)
	case next < quarantineRepFloor:
		// Auto-quarantine: reputation collapsed below the trust floor. Suspend the
		// supplier and stamp quarantined_at so the claim's s.status='active' gate
		// excludes it (it can be reinstated by an admin). Only flip a still-active
		// supplier (don't clobber an existing ban).
		_, err = tx.Exec(ctx,
			`UPDATE suppliers
			   SET reputation = $2,
			       status = CASE WHEN status = 'active' THEN 'suspended' ELSE status END,
			       quarantined_at = CASE WHEN status = 'active' THEN now() ELSE quarantined_at END
			 WHERE id = $1`,
			supplierID, next)
	default:
		_, err = tx.Exec(ctx,
			`UPDATE suppliers SET reputation = $2 WHERE id = $1`,
			supplierID, next)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DockReputationMild applies an event's delta (the same pure reputationDelta the
// full DockReputation uses) clamped to [0, 1], WITHOUT the quarantine/ban side
// effects. Used by the dead-claim rescue: a machine that went silent mid-claim is
// a soft reliability signal (mildest dock), not fraud — a repeat offender's score
// still erodes toward the claim filter's reputation gates, but this path NEVER
// suspends or bans on its own.
func (s *Store) DockReputationMild(ctx context.Context, supplierID uuid.UUID, event ReputationEvent) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE suppliers
		   SET reputation = GREATEST(0.0, LEAST(1.0, reputation + $2))
		 WHERE id = $1`,
		supplierID, reputationDelta(event))
	return err
}

// Verification-requeue backoff + worker-exclusion tuning (Scheduling & Matching
// Engine 8->9, docs/internal/CREED_AND_PATH_TO_TEN.md). A task that just FAILED
// verification (a bad honeypot answer) should not immediately return to the same
// worker with no delay: it costs the worker a re-dispatch it will likely fail
// again, and it costs the job wall-clock. So RequeueTask now (a) pushes visible_at
// out by an exponential-by-retry backoff, and (b) records the worker that just
// failed it in excluded_worker/excluded_until so the claim query prefers a
// DIFFERENT worker for the exclusion window. The exclusion deliberately EXPIRES
// (excluded_until = the backoff instant + a grace window): on a thin or
// single-worker fleet the retry is delayed and de-prioritized-away-from-the-failer,
// never permanently starved.
const (
	requeueBackoffBase = 30 * time.Second // first requeue delay; doubles per prior retry
	requeueBackoffCap  = 10 * time.Minute // ceiling so a high retry_count can't push a task far out
	// Grace window the failed worker stays excluded AFTER the task becomes visible,
	// so another worker gets first crack once the backoff elapses instead of the
	// failer racing back in the same instant. Bounded — see the comment above.
	requeueExclusionGrace = 2 * time.Minute
)

// requeueBackoff returns the visibility delay for a task being requeued after a
// failed verification, given how many times it has already been retried. Exponential
// (base << priorRetries) with a hard cap, mirroring the stale-task requeue ladder's
// shape (workers.go) so the two retry paths behave consistently.
func requeueBackoff(priorRetries int) time.Duration {
	if priorRetries < 0 {
		priorRetries = 0
	}
	// Cap the shift so the << can't overflow the duration on a pathological retry_count.
	shift := priorRetries
	if shift > 20 {
		shift = 20
	}
	d := requeueBackoffBase << uint(shift)
	if d <= 0 || d > requeueBackoffCap {
		return requeueBackoffCap
	}
	return d
}

// RequeueTask resets a failed-verification task to retrying and bumps the retry
// counter so the scheduler hands it to a DIFFERENT worker after a delay.
//
// PATCH (Scheduling & Matching Engine 8->9, docs/internal/CREED_AND_PATH_TO_TEN.md
// "add backoff plus worker-exclusion to verification-requeue"): this used to reset
// visible_at to now() (immediately reclaimable) and clear worker_id, so the exact
// worker that just failed the task's honeypot could reclaim it on its very next poll
// with zero delay — burning another dispatch on a machine that just proved it gets
// this task wrong. Now the task is pushed out by an exponential-per-retry backoff
// AND the just-failed worker is recorded in excluded_worker/excluded_until (read the
// worker off the row itself — CommitTask leaves worker_id/claimed_by set to the
// committing worker, so no caller change is needed), so the claim query prefers a
// different worker for the window. The exclusion expires (bounded), never starving a
// thin fleet's retry. Selection-affecting only — nothing about a task's bytes or the
// job's eventual result changes.
func (s *Store) RequeueTask(ctx context.Context, taskID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Read the worker that just held this task (still set post-commit) and its retry
	// count, so the backoff scales and the failer is the one we exclude. FOR UPDATE
	// pins the row against a concurrent requeue/claim while we compute the delay.
	var failedWorker *uuid.UUID
	var jobID uuid.UUID
	var priorRetries int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(worker_id, claimed_by), job_id, retry_count FROM tasks WHERE id = $1 FOR UPDATE`,
		taskID,
	).Scan(&failedWorker, &jobID, &priorRetries); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	backoff := requeueBackoff(priorRetries)

	if _, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'retrying', claimed_by = NULL, claimed_at = NULL,
		       worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2),
		       excluded_worker = $3,
		       excluded_until  = now() + make_interval(secs => $4)
		 WHERE id = $1`,
		taskID, backoff.Seconds(), failedWorker,
		(backoff + requeueExclusionGrace).Seconds(),
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET status = 'running' WHERE id = $1 AND status = 'verifying'`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- honeypots ---

// AvailableHoneypots returns up to limit honeypot input_refs for a job type, to
// inject as known-answer tasks at job submission. Fewer than limit (or none) is
// fine — honeypot coverage is best-effort, never fabricated.
func (s *Store) AvailableHoneypots(ctx context.Context, jobType string, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT input_ref FROM honeypots WHERE job_type = $1 ORDER BY created_at ASC LIMIT $2`,
		jobType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// GetHoneypotAnswer returns the known answer bytes for a honeypot input ref (if one
// exists for this job type) plus the verification class the answer was produced under
// ("engine|build_hash", or "" = unknown/class-blind). The verifier uses answerClass to
// decide whether a BYTE-EXACT honeypot mismatch is grounds to auto-quarantine: only
// when the committing worker shares the answer's class. A "" class means the answer is
// class-blind and never auto-quarantines a byte-exact job (the safe default).
func (s *Store) GetHoneypotAnswer(ctx context.Context, jobType, inputRef string) (answer []byte, answerClass string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT known_answer, COALESCE(answer_class,'') FROM honeypots
		 WHERE job_type = $1 AND input_ref = $2 LIMIT 1`,
		jobType, inputRef,
	).Scan(&answer, &answerClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", errNotFound
	}
	return answer, answerClass, err
}

// InsertHoneypot seeds a honeypot probe idempotently, REFUSING a blank-class byte-exact
// write (validateHoneypotSeed / item 11) so the seed/admin path fails safely rather than
// writing dead or dangerous coverage. answerClass is the "engine|build_hash" of the worker
// that produced knownAnswer; tolerant job types may pass "" (class-blind is safe for them).
func (s *Store) InsertHoneypot(ctx context.Context, jobType, inputRef string, knownAnswer []byte, answerClass string) error {
	if err := validateHoneypotSeed(jobType, answerClass); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO honeypots (job_type, input_ref, known_answer, answer_class)
		 SELECT $1, $2, $3, $4
		 WHERE NOT EXISTS (SELECT 1 FROM honeypots WHERE job_type=$1 AND input_ref=$2)`,
		jobType, inputRef, knownAnswer, answerClass)
	return err
}

// PeerResultKey finds a completed sibling task that ran the SAME input chunk for
// the same job as the given task but on a DIFFERENT worker, returning its result
// key for a within-class redundancy comparison plus the peer worker's finer
// verification class (engine + build_hash). The pairing is by shared input_ref
// (primary + its redundancy clone carry the same chunk). The class lets the
// verifier's no-object-store fallback decide whether a byte-exact disagreement is
// same-class (a real mismatch) or cross-class (provisional trust, not a dock).
// Returns errNotFound when no committed peer exists yet (the common case — the peer
// may not have finished, so verification simply has nothing to compare against).
// peerSHA256 is the peer's stored tasks.result_sha256 (Control Plane Hot Path
// 8->9): "" when the peer committed before this column existed, or with an
// agent build that never reported one — the caller always falls back to a real
// GetObject in that case, so an absent hash never changes correctness.
func (s *Store) PeerResultKey(ctx context.Context, taskID uuid.UUID) (peerResult string, peerSupplier uuid.UUID, peerEngine, peerBuildHash, peerSHA256 string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(p.result_ref,''),COALESCE(vw.supplier_id,p.execution_supplier_id),
		        COALESCE(vw.input_snapshot->>'engine',p.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',p.execution_build_hash,''),
		        COALESCE(p.result_sha256,'')
		 FROM tasks t
		   JOIN tasks p ON p.job_id = t.job_id AND p.input_ref = t.input_ref
		                AND p.id <> t.id AND p.status = 'complete'
		                AND p.result_ref IS NOT NULL AND p.result_ref <> ''
		   LEFT JOIN verification_work vw ON vw.task_id=p.id AND vw.attempt=p.retry_count
		 WHERE t.id = $1
		   AND COALESCE(vw.supplier_id,p.execution_supplier_id) IS NOT NULL
		 ORDER BY p.completed_at ASC
		 LIMIT 1`,
		taskID,
	).Scan(&peerResult, &peerSupplier, &peerEngine, &peerBuildHash, &peerSHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, "", "", "", errNotFound
	}
	return peerResult, peerSupplier, peerEngine, peerBuildHash, peerSHA256, err
}

// PeerSealedResult returns the same earliest completed peer only when its result
// is backed by terminal verification work and the task projection exactly matches
// that server-observed artifact. This is the authority required for a safe hash
// equality fast path; worker-reported hashes alone never qualify.
func (s *Store) PeerSealedResult(ctx context.Context, taskID uuid.UUID) (VerificationArtifact, uuid.UUID, string, string, error) {
	var artifact VerificationArtifact
	var supplier uuid.UUID
	var engine, build string
	err := s.pool.QueryRow(ctx, `
		SELECT vw.artifact_key,vw.artifact_sha256,vw.artifact_bytes,
		       vw.supplier_id,COALESCE(vw.input_snapshot->>'engine',''),
		       COALESCE(vw.input_snapshot->>'build_hash','')
		  FROM tasks t
		  JOIN tasks p ON p.job_id=t.job_id AND p.input_ref=t.input_ref
		              AND p.id<>t.id AND p.status='complete'
		  JOIN verification_work vw ON vw.task_id=p.id AND vw.attempt=p.retry_count
		 WHERE t.id=$1 AND vw.status='terminal'
		   AND p.result_ref=vw.artifact_key AND p.result_sha256=vw.artifact_sha256
		 ORDER BY p.completed_at ASC,p.id LIMIT 1`, taskID).
		Scan(&artifact.Key, &artifact.SHA256, &artifact.Bytes, &supplier, &engine, &build)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact, uuid.Nil, "", "", errNotFound
	}
	return artifact, supplier, engine, build, err
}

// ChunkResult is one committed result for a chunk: its result key plus the
// worker + supplier that produced it (so a majority vote can credit the winner
// and dock the losers by supplier).
type ChunkResult struct {
	TaskID     uuid.UUID
	WorkerID   uuid.UUID
	SupplierID uuid.UUID
	ResultRef  string
	// Artifact is the terminal verification_work tuple when this result went
	// through the durable verifier. Nil is an explicit pre-migration/legacy
	// fallback: callers may read ResultRef, but must never hash-trust it.
	Artifact *VerificationArtifact
	// Engine + BuildHash are the finer verification-class axes of the worker behind
	// this result, so the N-way vote can tell whether two byte-exact results are even
	// in the same (hw_class, engine, build_hash) class before docking on a mismatch.
	Engine    string
	BuildHash string
}

// ChunkResults returns every committed result for a job's chunk (the primary and
// all its redundancy/tiebreak clones share input_ref + chunk_index), each with
// its worker + supplier. The 3-way tiebreak vote (Verification V2) gathers these
// once a tiebreak commits and does a real majorityVote over them.
func (s *Store) ChunkResults(ctx context.Context, jobID uuid.UUID, chunkIndex int) ([]ChunkResult, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id,COALESCE(vw.worker_id,t.execution_worker_id),
		        COALESCE(vw.supplier_id,t.execution_supplier_id),
		        t.result_ref,
		        COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,''),
		        vw.artifact_key,vw.artifact_sha256,vw.artifact_bytes
		 FROM tasks t
		 LEFT JOIN verification_work vw
		   ON vw.task_id=t.id AND vw.attempt=t.retry_count AND vw.status='terminal'
		 WHERE t.job_id = $1 AND COALESCE(t.chunk_index,0) = $2
		   AND t.status = 'complete' AND t.is_honeypot = false
		   AND t.result_ref IS NOT NULL AND t.result_ref <> ''
		   AND COALESCE(vw.worker_id,t.execution_worker_id) IS NOT NULL
		   AND COALESCE(vw.supplier_id,t.execution_supplier_id) IS NOT NULL
		 ORDER BY t.completed_at ASC NULLS LAST, t.id ASC`,
		jobID, chunkIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkResult
	for rows.Next() {
		var cr ChunkResult
		var artifactKey, artifactSHA *string
		var artifactBytes *int64
		if err := rows.Scan(&cr.TaskID, &cr.WorkerID, &cr.SupplierID, &cr.ResultRef,
			&cr.Engine, &cr.BuildHash, &artifactKey, &artifactSHA, &artifactBytes); err != nil {
			return nil, err
		}
		if artifactKey != nil || artifactSHA != nil || artifactBytes != nil {
			if artifactKey == nil || artifactSHA == nil || artifactBytes == nil {
				return nil, fmt.Errorf("task %s has an incomplete terminal verification artifact tuple", cr.TaskID)
			}
			cr.Artifact = &VerificationArtifact{Key: *artifactKey, SHA256: *artifactSHA, Bytes: *artifactBytes}
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

// TiebreakExists reports whether a tiebreak (a redundancy task carrying
// hedged_from) already exists for a chunk, so a mismatch never spawns more than
// one third opinion.
func (s *Store) TiebreakExists(ctx context.Context, jobID uuid.UUID, chunkIndex int) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks
		 WHERE job_id = $1 AND COALESCE(chunk_index,0) = $2
		   AND is_redundancy = true AND hedged_from IS NOT NULL`,
		jobID, chunkIndex).Scan(&n)
	return n > 0, err
}

var ErrEconomicReserveExhausted = errors.New("economic extra-task reserve exhausted or work is no longer pre-charge")

func consumeEconomicReserveTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (buyerCharge, supplierPayout float64, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE job_economic_reserves r
		   SET consumed_tasks=consumed_tasks+1,updated_at=now()
		  FROM job_economic_plans p,jobs j
		 WHERE r.job_id=$1 AND p.job_id=r.job_id AND j.id=r.job_id
		   AND r.consumed_tasks < r.reserved_tasks
		   AND j.status IN ('queued','running','verifying')
		   AND j.charge_status='not_attempted'
		   AND (j.max_usd IS NULL
		        OR p.buyer_charge_per_task_usd * (p.initial_task_count+r.consumed_tasks+1)
		             + p.sla_premium_usd <= j.max_usd)
		   AND (p.firm_quote_max_usd IS NULL
		        OR p.buyer_charge_per_task_usd * (p.initial_task_count+r.consumed_tasks+1)
		             + p.sla_premium_usd <= p.firm_quote_max_usd)
		RETURNING p.buyer_charge_per_task_usd::float8,p.supplier_payout_per_task_usd::float8`, jobID).
		Scan(&buyerCharge, &supplierPayout)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrEconomicReserveExhausted
	}
	return buyerCharge, supplierPayout, err
}

// InsertTiebreakTask inserts a third-opinion redundancy task for a chunk, pinned
// (pre-claimed, not yet started) to a chosen same-class peer so only that worker
// runs it, carrying hedged_from = the primary task and the chunk's input_ref +
// chunk_index. It also bumps the parent job's task_count so the completion sweep
// waits for this extra opinion before finalizing. Returns the new task id.
func (s *Store) InsertTiebreakTask(ctx context.Context, jobID, primaryTaskID, peerWorker uuid.UUID, inputRef string, chunkIndex int) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	buyerCharge, supplierPayout, err := consumeEconomicReserveTx(ctx, tx, jobID)
	if err != nil {
		if errors.Is(err, ErrEconomicReserveExhausted) {
			// A retry may arrive after the first creator consumed the final slot.
			// Return that durable task idempotently; do not turn a successful prior
			// authorization into a spurious reserve error.
			var existing uuid.UUID
			qerr := tx.QueryRow(ctx, `
				SELECT id FROM tasks
				 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2
				   AND hedged_from IS NOT NULL AND is_redundancy=true
				 ORDER BY created_at LIMIT 1`, jobID, chunkIndex).Scan(&existing)
			if qerr == nil {
				return existing, nil
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return uuid.Nil, qerr
			}
		}
		return uuid.Nil, err
	}
	// The reserve-row UPDATE above serializes dynamic creators for this job. A
	// concurrent identical creator is now visible; return its id and roll back our
	// tentative reserve consumption instead of duplicating work.
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND COALESCE(chunk_index,0)=$2
		   AND hedged_from IS NOT NULL AND is_redundancy=true
		 ORDER BY created_at LIMIT 1`, jobID, chunkIndex).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	frozenClass, err := frozenTiebreakClassForAnchorTx(ctx, tx, primaryTaskID)
	if err != nil {
		return uuid.Nil, err
	}

	id := uuid.New()
	resultKey := fmt.Sprintf("jobs/%s/tiebreak/%s/result.json", jobID, id)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tasks
		   (id, job_id, status, is_honeypot, is_redundancy, retry_count,
		    input_ref, result_key, chunk_index, hedged_from,expected_output_records,
		    verification_hw_class,verification_engine,verification_build_hash,
		    claimed_by, claimed_at, visible_at,
		    economic_buyer_charge_usd,economic_supplier_payout_usd)
		 VALUES ($1,$2,'queued',false,true,0,$3,$4,$5,$6,
		         (SELECT expected_output_records FROM tasks WHERE id=$6),
		         $7,$8,$9,$10,now(),now(),$11,$12)`,
		id, jobID, inputRef, resultKey, chunkIndex, primaryTaskID,
		frozenClass.HWClass, frozenClass.Engine, frozenClass.BuildHash,
		peerWorker, buyerCharge, supplierPayout,
	); err != nil {
		return uuid.Nil, err
	}
	// One more opinion to wait for before the job is "all tasks done".
	if _, err := tx.Exec(ctx,
		`UPDATE jobs
		    SET task_count = task_count + 1,
		        status = CASE WHEN status='verifying' THEN 'running' ELSE status END
		  WHERE id = $1`, jobID); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// --- ledger ---

// InsertLedgerEntries writes a batch of ledger rows in one transaction.
func (s *Store) InsertLedgerEntries(ctx context.Context, entries []LedgerEntry) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, e := range entries {
		_, err = tx.Exec(ctx,
			`INSERT INTO ledger_entries
			   (kind, supplier_id, buyer_id, task_id, amount_usd, payout_status, release_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 ON CONFLICT (task_id, kind) DO NOTHING`,
			e.Kind, e.SupplierID, e.BuyerID, e.TaskID, e.AmountUSD, e.PayoutStatus, e.ReleaseAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ClawbackTaskCredit reverses the supplier credit already written for a task on
// confirmed fraud. Liability reversal and cash recovery are deliberately
// different states: a held/ready credit becomes clawed_back, while a transfer
// already released, exported, or in flight becomes reversal_required. Nothing in
// this function pretends a provider reversal exists.
func (s *Store) ClawbackTaskCredit(ctx context.Context, supplierID, taskID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var (
		creditID       uuid.UUID
		credited       float64
		creditState    string
		payoutRef      string
		outcomeUnknown bool
	)
	err = tx.QueryRow(ctx, `
		SELECT le.id,le.amount_usd::float8,le.payout_status,
		       COALESCE(le.payout_ref,''),COALESCE(op.outcome_unknown,false)
		  FROM ledger_entries le
		  LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.supplier_id=$1 AND le.task_id=$2
		   AND le.kind='supplier_credit' AND le.amount_usd>0
		 ORDER BY le.created_at,le.id LIMIT 1
		 FOR UPDATE OF le`, supplierID, taskID).
		Scan(&creditID, &credited, &creditState, &payoutRef, &outcomeUnknown)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && credited > 0 {
		cb := clawbackEntry(supplierID, taskID, credited)
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries
		   (kind, supplier_id, task_id, amount_usd, payout_status)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (task_id, kind) DO NOTHING`,
			cb.Kind, cb.SupplierID, cb.TaskID, cb.AmountUSD, cb.PayoutStatus,
		); err != nil {
			return err
		}
		reversalRequired := payoutRef != "" || outcomeUnknown ||
			creditState == PayoutSending || creditState == PayoutOutcomeUnknown ||
			creditState == PayoutReleased || creditState == PayoutExported ||
			creditState == PayoutReversalRequired
		nextState := PayoutClawedBack
		if reversalRequired {
			nextState = PayoutReversalRequired
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1`, creditID, nextState); err != nil {
			return err
		}
		opState := nextState
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status=$2,updated_at=now(),
			       last_error=CASE WHEN $2='reversal_required'
			                       THEN 'confirmed clawback requires external recovery'
			                       ELSE last_error END
			 WHERE ledger_entry_id=$1`, creditID, opState); err != nil {
			return err
		}
	}
	// The money reversal and the buyer-facing durable verdict move together. A
	// task with no previously scheduled credit still records the corrected verdict.
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET verification_outcome='clawed_back', verified_at=now() WHERE id=$1`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO task_verdict_resolutions (effect_id,task_id,source_task_id,kind)
		 VALUES ($1,$2,$2,'clawed_back') ON CONFLICT (effect_id) DO NOTHING`,
		verificationResolutionID(taskID, "clawed_back", taskID), taskID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WorkerEarnings separates accrued credit from proved supplier cash for
// GET /v1/worker/earnings. Lifetime = all positive supplier credits ever.
// Balance and LastPayout* require a durable payout operation with cash_moved=true;
// `awaiting_funding`/`ready` are debt, `outcome_unknown` is possible cash,
// `exported` is coordination, and `reversal_required` is unresolved recovery, so
// none may render as a paid balance or "last payout" in the app.
// NextPayoutAt remains the soonest held credit's scheduled attempt time.
func (s *Store) WorkerEarnings(ctx context.Context, supplierID uuid.UUID) (Earnings, error) {
	var e Earnings
	err := s.pool.QueryRow(ctx,
		`SELECT
		   COALESCE(SUM(op.sent_cents) FILTER (
		     WHERE le.payout_status = 'released' AND op.cash_moved = true
		       AND op.sent_cents > 0), 0)::float8 / 100.0,
		   COALESCE(SUM(le.amount_usd) FILTER (WHERE le.amount_usd > 0), 0),
		   COALESCE(SUM(mu.remainder_microusd) FILTER (
		     WHERE le.payout_status NOT IN ('clawed_back','reversal_required')),0)::float8 / 1000000.0
		 FROM ledger_entries le
		 LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 LEFT JOIN supplier_minor_unit_settlements mu ON mu.ledger_entry_id=le.id
		 WHERE le.supplier_id = $1 AND le.kind = 'supplier_credit'`,
		supplierID,
	).Scan(&e.BalanceUSD, &e.LifetimeUSD, &e.CarriedUSD)
	if err != nil {
		return e, err
	}

	var lastAmt float64
	var lastAt time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT op.sent_cents::float8 / 100.0,op.updated_at
		   FROM ledger_entries le
		   JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		  WHERE le.supplier_id=$1 AND le.kind='supplier_credit'
		    AND le.payout_status='released' AND op.cash_moved=true
		  ORDER BY op.updated_at DESC,le.id DESC LIMIT 1`,
		supplierID,
	).Scan(&lastAmt, &lastAt)
	switch {
	case err == nil:
		e.LastPayoutUSD = &lastAmt
		t := lastAt.Unix()
		e.LastPayoutAt = &t
	case errors.Is(err, pgx.ErrNoRows):
		// No durable cash payout yet — leave both nil, not zero.
	default:
		return e, err
	}

	var nextAt time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT release_at FROM ledger_entries
		  WHERE supplier_id = $1 AND kind = 'supplier_credit'
		    AND payout_status = 'held' AND release_at IS NOT NULL
		  ORDER BY release_at ASC LIMIT 1`,
		supplierID,
	).Scan(&nextAt)
	switch {
	case err == nil:
		t := nextAt.Unix()
		e.NextPayoutAt = &t
	case errors.Is(err, pgx.ErrNoRows):
		// Nothing currently held — no scheduled next payout, not a fabricated one.
	default:
		return e, err
	}
	return e, nil
}

// FraudFlag is one row of GET /admin/fraud-flags: suppliers below the trust
// floor or already suspended/banned.
type FraudFlag struct {
	SupplierID uuid.UUID `json:"supplier_id"`
	Reputation float32   `json:"reputation"`
	Tier       int16     `json:"tier"`
	Status     string    `json:"status"`
}

// ListFraudFlags returns suppliers whose reputation dropped below 0.5 or whose
// status is non-active — the set an admin should review.
func (s *Store) ListFraudFlags(ctx context.Context) ([]FraudFlag, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, reputation, tier, status FROM suppliers
		 WHERE reputation < 0.5 OR status IN ('suspended','banned')
		 ORDER BY reputation ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FraudFlag
	for rows.Next() {
		var f FraudFlag
		if err := rows.Scan(&f.SupplierID, &f.Reputation, &f.Tier, &f.Status); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FraudReport is one row of GET /admin/fraud: the full trust picture for a
// flagged supplier — reputation, tier, status, when (if) it was quarantined, and
// its confirmed-fraud signal (clawback count = reversed credits on bad results).
type FraudReport struct {
	SupplierID    uuid.UUID  `json:"supplier_id"`
	Reputation    float32    `json:"reputation"`
	Tier          int16      `json:"tier"`
	Status        string     `json:"status"`
	QuarantinedAt *time.Time `json:"quarantined_at"`
	Clawbacks     int        `json:"clawbacks"`      // confirmed-fraud clawback rows
	MismatchTasks int        `json:"mismatch_tasks"` // tasks this supplier's clawbacks span
}

// ListFraud returns the fraud report for every supplier that is quarantined,
// banned, below the trust floor, or has any clawback on record — the admin's
// review queue. Ordered worst-first (lowest reputation). The clawback/mismatch
// counts come from the ledger so they reflect real reversed credit, not a guess.
func (s *Store) ListFraud(ctx context.Context) ([]FraudReport, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.id, s.reputation, s.tier, s.status, s.quarantined_at,
		        COALESCE(cb.n,0), COALESCE(cb.tasks,0)
		 FROM suppliers s
		 LEFT JOIN (
		   SELECT supplier_id, count(*) AS n, count(DISTINCT task_id) AS tasks
		   FROM ledger_entries WHERE kind = 'clawback' GROUP BY supplier_id
		 ) cb ON cb.supplier_id = s.id
		 WHERE s.reputation < 0.5
		    OR s.status IN ('suspended','banned')
		    OR s.quarantined_at IS NOT NULL
		    OR COALESCE(cb.n,0) > 0
		 ORDER BY s.reputation ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FraudReport
	for rows.Next() {
		var f FraudReport
		if err := rows.Scan(&f.SupplierID, &f.Reputation, &f.Tier, &f.Status,
			&f.QuarantinedAt, &f.Clawbacks, &f.MismatchTasks); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// --- models catalogue (real pricing) ---

// ModelRow is one row of the models table: the real pricing catalogue.
type ModelRow struct {
	ID           string
	Family       string
	Quant        string
	Kind         string
	Dim          int
	JobType      string
	PricePer1K   float64
	PricePerUnit float64
	MinMemoryGB  float32
	HFRepo       string
}

// ListModels returns the full models catalogue ordered by price.
func (s *Store) ListModels(ctx context.Context) ([]ModelRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, COALESCE(family,''), COALESCE(quant,''), COALESCE(kind,''),
		        COALESCE(dim,0), COALESCE(job_type,''),
		        COALESCE(price_per_1k,0), COALESCE(price_per_unit,0),
		        COALESCE(min_memory_gb,0), COALESCE(hf_repo,'')
		 FROM models ORDER BY price_per_1k ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelRow
	for rows.Next() {
		var m ModelRow
		if err := rows.Scan(&m.ID, &m.Family, &m.Quant, &m.Kind, &m.Dim, &m.JobType,
			&m.PricePer1K, &m.PricePerUnit, &m.MinMemoryGB, &m.HFRepo); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModel loads one model by id. errNotFound when the id is unknown.
func (s *Store) GetModel(ctx context.Context, id string) (*ModelRow, error) {
	var m ModelRow
	err := s.pool.QueryRow(ctx,
		`SELECT id, COALESCE(family,''), COALESCE(quant,''), COALESCE(kind,''),
		        COALESCE(dim,0), COALESCE(job_type,''),
		        COALESCE(price_per_1k,0), COALESCE(price_per_unit,0),
		        COALESCE(min_memory_gb,0), COALESCE(hf_repo,'')
		 FROM models WHERE id = $1`, id,
	).Scan(&m.ID, &m.Family, &m.Quant, &m.Kind, &m.Dim, &m.JobType,
		&m.PricePer1K, &m.PricePerUnit, &m.MinMemoryGB, &m.HFRepo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// --- webhooks ---

var (
	errWebhookLeaseLost   = errors.New("webhook delivery lease lost")
	errWebhookJobRequired = errors.New("webhook job id is required")
	errWebhookLimit       = errors.New("webhook registration limit reached for job")
)

const webhookRegistrationLimitPerJob = 32

// InsertWebhook persists a completion-webhook registration. Job-scoped hooks are
// ownership-checked and inserted in the same transaction, so a leaked job UUID can
// never register a callback for a different buyer. The composite FK installed by
// Migrate is a second, database-level enforcement of the same invariant.
func (s *Store) InsertWebhook(ctx context.Context, buyerID uuid.UUID, jobID *uuid.UUID, url string) (WebhookRegistration, error) {
	if buyerID == uuid.Nil {
		return WebhookRegistration{}, errors.New("webhook buyer id is required")
	}
	if jobID == nil || *jobID == uuid.Nil {
		return WebhookRegistration{}, errWebhookJobRequired
	}
	url = strings.TrimSpace(url)
	if len(url) == 0 || len(url) > webhookURLMaxBytes {
		return WebhookRegistration{}, fmt.Errorf("webhook url must be between 1 and %d bytes", webhookURLMaxBytes)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebhookRegistration{}, err
	}
	defer tx.Rollback(ctx)
	var owned bool
	err = tx.QueryRow(ctx,
		`SELECT true FROM jobs WHERE id=$1 AND buyer_id=$2 FOR UPDATE`,
		*jobID, buyerID).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookRegistration{}, errNotFound
	}
	if err != nil {
		return WebhookRegistration{}, err
	}
	var existing uuid.UUID
	var existingSealed string
	err = tx.QueryRow(ctx,
		`SELECT id,COALESCE(signing_secret_sealed,'')
		   FROM webhooks
		  WHERE buyer_id=$1 AND job_id=$2 AND url=$3
		  FOR UPDATE`,
		buyerID, *jobID, url).Scan(&existing, &existingSealed)
	if err == nil {
		secret, openErr := openWebhookSigningSecret(existingSealed)
		if openErr != nil {
			// Exact re-registration is the explicit migration path for a legacy or
			// unreadable row. The receiver learns the new key in this response and
			// a pending/dead row is re-armed; no unsigned delivery ever occurs.
			var sealed string
			secret, sealed, err = newWebhookSigningSecret()
			if err != nil {
				return WebhookRegistration{}, err
			}
			if _, err = tx.Exec(ctx, `
				UPDATE webhooks
				   SET signing_secret_sealed=$2,
				       attempts=0,next_attempt_at=now(),
				       lease_token=NULL,lease_expires_at=NULL,
				       dead_lettered_at=NULL,last_attempt_at=NULL,last_error=NULL
				 WHERE id=$1`, existing, sealed); err != nil {
				return WebhookRegistration{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return WebhookRegistration{}, err
		}
		return WebhookRegistration{ID: existing, Secret: secret}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return WebhookRegistration{}, err
	}
	var registrations int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM webhooks WHERE buyer_id=$1 AND job_id=$2`,
		buyerID, *jobID).Scan(&registrations); err != nil {
		return WebhookRegistration{}, err
	}
	if registrations >= webhookRegistrationLimitPerJob {
		return WebhookRegistration{}, errWebhookLimit
	}
	secret, sealed, err := newWebhookSigningSecret()
	if err != nil {
		return WebhookRegistration{}, err
	}
	id := uuid.New()
	if _, err = tx.Exec(ctx,
		`INSERT INTO webhooks (id,buyer_id,job_id,url,signing_secret_sealed)
		 VALUES ($1,$2,$3,$4,$5)`,
		id, buyerID, jobID, url, sealed); err != nil {
		return WebhookRegistration{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WebhookRegistration{}, err
	}
	return WebhookRegistration{ID: id, Secret: secret}, nil
}

// JobWebhooks returns the buyer-owned webhook URLs registered for one job.
func (s *Store) JobWebhooks(ctx context.Context, jobID, buyerID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT url FROM webhooks
		 WHERE job_id = $1 AND buyer_id = $2`,
		jobID, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// PendingWebhook is one undelivered completion webhook for a complete job.
type PendingWebhook struct {
	ID                  uuid.UUID
	JobID               uuid.UUID
	URL                 string
	Status              string
	SigningSecretSealed string
	LeaseToken          uuid.UUID
	Attempts            int
}

// ClaimPendingWebhooks leases due, terminal job-scoped deliveries. The ownership
// predicate is repeated in both the candidate and returned-row joins: even a
// malformed legacy row predating the composite FK can never receive another
// buyer's result URL. SKIP LOCKED makes this safe when more than one sweep process
// is enabled, while next_attempt_at and dead_lettered_at keep poison rows out of
// subsequent pages.
func (s *Store) ClaimPendingWebhooks(ctx context.Context, limit int, lease time.Duration) ([]PendingWebhook, error) {
	if limit <= 0 || lease <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
		  SELECT wh.id
		    FROM webhooks wh
		    JOIN jobs j ON j.id=wh.job_id AND j.buyer_id=wh.buyer_id
		   WHERE wh.delivered_at IS NULL
		     AND wh.dead_lettered_at IS NULL
		     AND wh.job_id IS NOT NULL
		     AND wh.next_attempt_at <= now()
		     AND (wh.lease_expires_at IS NULL OR wh.lease_expires_at <= now())
		     AND j.status IN ('complete','failed','cancelled')
		   ORDER BY wh.next_attempt_at,j.created_at,wh.created_at,wh.id
		   FOR UPDATE OF wh SKIP LOCKED
		   LIMIT $1
		), claimed AS (
		  UPDATE webhooks wh
		     SET lease_token=gen_random_uuid(),
		         lease_expires_at=now() + make_interval(secs => $2),
		         last_attempt_at=now()
		    FROM candidates c
		   WHERE wh.id=c.id
		   RETURNING wh.id,wh.job_id,wh.buyer_id,wh.url,
		             COALESCE(wh.signing_secret_sealed,'') AS signing_secret_sealed,
		             wh.lease_token,wh.attempts,
		             wh.next_attempt_at,wh.created_at
		)
		SELECT c.id,c.job_id,c.url,j.status,c.signing_secret_sealed,c.lease_token,c.attempts
		  FROM claimed c
		  JOIN jobs j ON j.id=c.job_id AND j.buyer_id=c.buyer_id
		 ORDER BY c.next_attempt_at,c.created_at,c.id`,
		limit, int64(lease/time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingWebhook
	for rows.Next() {
		var p PendingWebhook
		if err := rows.Scan(
			&p.ID, &p.JobID, &p.URL, &p.Status, &p.SigningSecretSealed, &p.LeaseToken, &p.Attempts,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkWebhookDelivered completes only the exact live lease. A provider may have
// accepted the POST before this write fails; leaving the lease to expire causes a
// safe at-least-once replay with the same stable delivery_id, never a false success.
func (s *Store) MarkWebhookDelivered(ctx context.Context, id, leaseToken uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE webhooks
		   SET delivered_at=now(),lease_token=NULL,lease_expires_at=NULL,last_error=NULL
		 WHERE id=$1 AND lease_token=$2 AND delivered_at IS NULL AND dead_lettered_at IS NULL`,
		id, leaseToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errWebhookLeaseLost
	}
	return nil
}

// MarkWebhookFailed advances durable backoff or dead-letters a permanent/exhausted
// delivery. The lease token fences a late HTTP response from changing a row already
// reclaimed by another process.
func (s *Store) MarkWebhookFailed(
	ctx context.Context,
	id, leaseToken uuid.UUID,
	failure error,
	permanent bool,
	retryAfter time.Duration,
	maxAttempts int,
) (attempts int, deadLettered bool, err error) {
	if retryAfter < 0 {
		retryAfter = 0
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	message := "webhook delivery failed"
	if failure != nil {
		message = failure.Error()
	}
	runes := []rune(message)
	if len(runes) > 2048 {
		message = string(runes[:2048])
	}
	err = s.pool.QueryRow(ctx, `
		UPDATE webhooks
		   SET attempts=attempts+1,
		       last_error=$3,
		       dead_lettered_at=CASE WHEN $4 OR attempts+1 >= $6 THEN now() ELSE NULL END,
		       next_attempt_at=CASE
		         WHEN $4 OR attempts+1 >= $6 THEN next_attempt_at
		         ELSE now() + make_interval(secs => $5)
		       END,
		       lease_token=NULL,lease_expires_at=NULL
		 WHERE id=$1 AND lease_token=$2 AND delivered_at IS NULL AND dead_lettered_at IS NULL
		 RETURNING attempts,dead_lettered_at IS NOT NULL`,
		id, leaseToken, message, permanent, int64(retryAfter/time.Second), maxAttempts,
	).Scan(&attempts, &deadLettered)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, errWebhookLeaseLost
	}
	return attempts, deadLettered, err
}

// --- background-worker support ---

// DueHeldEntry is a supplier credit whose hold has expired and is due for payout.
type DueHeldEntry struct {
	ID               uuid.UUID
	SupplierID       uuid.UUID
	AmountUSD        float64 // exact provider amount after ClaimPayout
	LiabilityMicros  int64   // immutable six-decimal internal liability
	RequestedCents   int64
	RemainderMicros  int64
	SettlementPolicy string
	Currency         string
}

const (
	payoutFundingBuyerCollection = "buyer_collection"
	payoutFundingPlatformSubsidy = "platform_subsidy"
)

// persistMinorUnitSettlement freezes the exact relationship between one internal
// six-decimal liability and its provider-minor-unit cash request. The row is
// append-only in the schema. Identical retries reuse it; any changed amount or
// policy fails before funding or a provider call.
func persistMinorUnitSettlement(
	ctx context.Context,
	tx pgx.Tx,
	entryID uuid.UUID,
	liabilityMicros int64,
) (cashCents, remainderMicros int64, err error) {
	cashCents, remainderMicros, err = splitSupplierLiabilityMicros(liabilityMicros)
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO supplier_minor_unit_settlements
		  (ledger_entry_id,policy,liability_microusd,cash_cents,remainder_microusd,currency)
		VALUES ($1,$2,$3,$4,$5,'usd')
		ON CONFLICT (ledger_entry_id) DO NOTHING`,
		entryID, supplierSettlementPolicyFloorCentCarryV1, liabilityMicros, cashCents, remainderMicros,
	); err != nil {
		return 0, 0, err
	}
	var existingPolicy, currency string
	var existingLiability, existingCash, existingRemainder int64
	if err := tx.QueryRow(ctx, `
		SELECT policy,liability_microusd,cash_cents,remainder_microusd,currency
		  FROM supplier_minor_unit_settlements WHERE ledger_entry_id=$1`, entryID,
	).Scan(&existingPolicy, &existingLiability, &existingCash, &existingRemainder, &currency); err != nil {
		return 0, 0, err
	}
	if existingPolicy != supplierSettlementPolicyFloorCentCarryV1 ||
		existingLiability != liabilityMicros || existingCash != cashCents ||
		existingRemainder != remainderMicros || currency != "usd" {
		return 0, 0, fmt.Errorf(
			"minor-unit settlement for ledger entry %s changed: policy=%s liability=%d cash=%d remainder=%d currency=%s",
			entryID, existingPolicy, existingLiability, existingCash, existingRemainder, currency)
	}
	return cashCents, remainderMicros, nil
}

// reservePayoutFunding binds one supplier liability to exact incoming card cash.
// The caller already owns the supplier-credit row lock. This function additionally
// locks the standalone job or shared charge-batch cash pool before checking its
// remaining integer cents, so concurrent payout workers cannot over-allocate one
// PaymentIntent. An existing reservation is reused unchanged across rail retries.
// A pre-authorized platform subsidy is already a reservation and is accepted here;
// this function never manufactures one implicitly.
func reservePayoutFunding(
	ctx context.Context,
	tx pgx.Tx,
	entryID uuid.UUID,
	taskID *uuid.UUID,
	requestedCents int64,
	currency string,
) (uuid.UUID, bool, error) {
	var (
		existingID       uuid.UUID
		existingSource   string
		existingAmount   int64
		existingCurrency string
		existingState    string
	)
	err := tx.QueryRow(ctx, `
		SELECT f.id,f.source_kind,f.amount_cents,f.currency,
		       COALESCE(fs.status,'available')
		  FROM supplier_payout_funding f
		  LEFT JOIN supplier_payout_funding_state fs ON fs.funding_id=f.id
		 WHERE f.ledger_entry_id=$1
		 FOR UPDATE OF f`, entryID,
	).Scan(&existingID, &existingSource, &existingAmount, &existingCurrency, &existingState)
	if err == nil {
		if existingAmount != requestedCents || existingCurrency != currency ||
			(existingSource != payoutFundingBuyerCollection && existingSource != payoutFundingPlatformSubsidy) {
			return uuid.Nil, false, fmt.Errorf(
				"payout funding for ledger entry %s does not match liability: source=%s amount=%d %s liability=%d %s",
				entryID, existingSource, existingAmount, existingCurrency, requestedCents, currency)
		}
		// A refund/dispute can invalidate an immutable reservation while an
		// earlier provider attempt is in flight. Never reuse that reservation for
		// a new send; reconciliation owns any cash that may already have moved.
		if existingState == "compromised" {
			return existingID, false, nil
		}
		return existingID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	if taskID == nil {
		// A job-level supplier credit has no causal buyer collection to follow.
		// It can move only after AuthorizePayoutSubsidy creates an explicit row.
		return uuid.Nil, false, nil
	}

	var (
		jobID                                     uuid.UUID
		buyerID                                   uuid.UUID
		chargeStatus, paymentIntent, cashCurrency string
		batchID                                   *uuid.UUID
		cashRequested, cashReceived               int64
	)
	if err := tx.QueryRow(ctx, `
		SELECT j.id,j.buyer_id,j.charge_status,j.charge_batch_id,
		       COALESCE(j.stripe_pi,''),COALESCE(j.charge_requested_cents,0),
		       COALESCE(j.charge_received_cents,0),COALESCE(j.charge_currency,'')
		  FROM tasks t JOIN jobs j ON j.id=t.job_id
		 WHERE t.id=$1
		 FOR UPDATE OF j`, *taskID,
	).Scan(&jobID, &buyerID, &chargeStatus, &batchID, &paymentIntent,
		&cashRequested, &cashReceived, &cashCurrency); err != nil {
		return uuid.Nil, false, err
	}
	if chargeStatus != "charged" {
		return uuid.Nil, false, nil
	}

	sourceKind := "job"
	if batchID != nil {
		sourceKind = "batch"
		var batchBuyer uuid.UUID
		var batchStatus string
		if err := tx.QueryRow(ctx, `
			SELECT buyer_id,status,COALESCE(stripe_pi,''),
			       COALESCE(charge_requested_cents,0),COALESCE(charge_received_cents,0),
			       COALESCE(charge_currency,'')
			  FROM charge_batches WHERE id=$1 FOR UPDATE`, *batchID,
		).Scan(&batchBuyer, &batchStatus, &paymentIntent, &cashRequested, &cashReceived, &cashCurrency); err != nil {
			return uuid.Nil, false, err
		}
		if batchBuyer != buyerID || batchStatus != "charged" {
			return uuid.Nil, false, nil
		}
	}
	if strings.TrimSpace(paymentIntent) == "" || cashRequested <= 0 ||
		cashReceived != cashRequested || cashCurrency != currency {
		return uuid.Nil, false, nil
	}

	// Lock and revalidate the canonical cross-source record. This is the shared
	// allocation mutex: all task liabilities funded by one batch serialize here.
	var canonicalBuyer uuid.UUID
	var canonicalRequested, canonicalReceived int64
	var canonicalCurrency, canonicalChargeID string
	if sourceKind == "job" {
		err = tx.QueryRow(ctx, `
			SELECT buyer_id,requested_cents,received_cents,currency,COALESCE(charge_id,'')
			  FROM buyer_cash_collections
			 WHERE payment_intent=$1 AND source_kind='job' AND job_id=$2
			 FOR UPDATE`, paymentIntent, jobID,
		).Scan(&canonicalBuyer, &canonicalRequested, &canonicalReceived, &canonicalCurrency, &canonicalChargeID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT buyer_id,requested_cents,received_cents,currency,COALESCE(charge_id,'')
			  FROM buyer_cash_collections
			 WHERE payment_intent=$1 AND source_kind='batch' AND charge_batch_id=$2
			 FOR UPDATE`, paymentIntent, *batchID,
		).Scan(&canonicalBuyer, &canonicalRequested, &canonicalReceived, &canonicalCurrency, &canonicalChargeID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	if canonicalBuyer != buyerID || canonicalRequested != cashRequested ||
		canonicalReceived != cashReceived || canonicalCurrency != cashCurrency {
		return uuid.Nil, false, fmt.Errorf("canonical buyer cash %s disagrees with its %s source", paymentIntent, sourceKind)
	}
	// Legacy collections without the Stripe Charge id cannot be safely matched to
	// a dispute whose payment_intent is null. They remain owed but unfundable until
	// an operator reconciles the real charge binding.
	if strings.TrimSpace(canonicalChargeID) == "" {
		return uuid.Nil, false, nil
	}

	unavailable, err := stripeCollectionUnavailableCents(ctx, tx, paymentIntent, cashReceived)
	if err != nil {
		return uuid.Nil, false, err
	}
	available := cashReceived - unavailable
	if available < 0 {
		return uuid.Nil, false, fmt.Errorf("stripe cash state for %s exceeds collected cash", paymentIntent)
	}

	var reserved int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_cents),0)::bigint
		  FROM supplier_payout_funding
		 WHERE source_kind='buyer_collection' AND collection_payment_intent=$1`, paymentIntent,
	).Scan(&reserved); err != nil {
		return uuid.Nil, false, err
	}
	if reserved < 0 || reserved > available || requestedCents > available-reserved {
		return uuid.Nil, false, nil
	}

	var fundingID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO supplier_payout_funding
		  (ledger_entry_id,source_kind,liability_job_id,collection_payment_intent,
		   amount_cents,currency)
		VALUES ($1,'buyer_collection',$2,$3,$4,$5)
		RETURNING id`, entryID, jobID, paymentIntent, requestedCents, currency,
	).Scan(&fundingID); err != nil {
		return uuid.Nil, false, err
	}
	return fundingID, true, nil
}

// DuePayouts returns held supplier credits with release_at <= now(): the set the
// payout-release loop should attempt to send. ClaimPayout moves a row with no
// exact cash reservation to awaiting_funding, so a bounded oldest-first page
// always makes progress and cannot be permanently occupied by the same debt.
func (s *Store) DuePayouts(ctx context.Context, limit int) ([]DueHeldEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT le.id, le.supplier_id, le.amount_usd
		 FROM ledger_entries le LEFT JOIN tasks t ON t.id=le.task_id
		 WHERE le.kind = 'supplier_credit' AND le.payout_status = 'held'
		   AND le.release_at IS NOT NULL AND le.release_at <= now()
		   -- A provisional pass_with_penalty is visible to the buyer but cannot
		   -- leave the platform as supplier money until an unqualified durable pass.
		   AND (le.task_id IS NULL OR t.verification_outcome = 'pass')
		 ORDER BY le.release_at ASC,le.id ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueHeldEntry
	for rows.Next() {
		var e DueHeldEntry
		if err := rows.Scan(&e.ID, &e.SupplierID, &e.AmountUSD); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClaimPayout is the local half of the payout outbox. It locks one due credit,
// creates or resumes its durable operation, and CASes held -> sending in the
// same transaction. A clawback that wins the row lock first makes claimed=false;
// no provider call occurs. A concurrent release worker likewise cannot claim it
// twice. The operation id is stable through retries, as is the rail idempotency
// key (the ledger entry id).
func (s *Store) ClaimPayout(ctx context.Context, entryID uuid.UUID) (DueHeldEntry, bool, error) {
	var out DueHeldEntry
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx)

	var (
		status          string
		releaseAt       *time.Time
		verdict         string
		taskID          *uuid.UUID
		liabilityMicros int64
	)
	err = tx.QueryRow(ctx, `
		SELECT le.id,le.supplier_id,le.amount_usd::float8,
		       (le.amount_usd*1000000)::bigint,le.payout_status,
		       le.release_at,COALESCE(t.verification_outcome,''),le.task_id
		  FROM ledger_entries le LEFT JOIN tasks t ON t.id=le.task_id
		 WHERE le.id=$1 AND le.kind='supplier_credit'
		 FOR UPDATE OF le`, entryID).
		Scan(&out.ID, &out.SupplierID, &out.AmountUSD, &liabilityMicros, &status, &releaseAt, &verdict, &taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, errNotFound
	}
	if err != nil {
		return out, false, err
	}
	if status != PayoutHeld || releaseAt == nil || releaseAt.After(time.Now()) ||
		(taskID != nil && verdict != string(OutcomePass)) {
		return out, false, nil
	}
	out.LiabilityMicros = liabilityMicros
	out.SettlementPolicy = supplierSettlementPolicyFloorCentCarryV1
	out.Currency = "usd"
	out.RequestedCents, out.RemainderMicros, err = persistMinorUnitSettlement(
		ctx, tx, entryID, liabilityMicros)
	if err != nil {
		return out, false, err
	}
	if out.RequestedCents == 0 {
		ct, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1 AND payout_status=$3`,
			entryID, PayoutCarried, PayoutHeld)
		if err != nil {
			return out, false, err
		}
		if ct.RowsAffected() != 1 {
			return out, false, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return out, false, err
		}
		return out, false, nil
	}
	// The provider receives exactly the frozen integer-cent amount. The original
	// six-decimal liability stays in LiabilityMicros and reconciles through the
	// append-only settlement row; passing it to a rail would round a second time.
	out.AmountUSD = float64(out.RequestedCents) / 100
	fundingID, funded, err := reservePayoutFunding(
		ctx, tx, entryID, taskID, out.RequestedCents, out.Currency)
	if err != nil {
		return out, false, err
	}
	if !funded {
		// Persist the deterministic split even while cash remains unavailable. This
		// does not create an operation or cross the provider boundary. Move the row
		// out of the bounded due page so old unfunded debt cannot starve later
		// payable work; an exact collection fact or explicit subsidy re-arms it.
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2
			  WHERE id=$1 AND payout_status=$3`,
			entryID, PayoutAwaitingFunding, PayoutHeld); err != nil {
			return out, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return out, false, err
		}
		return out, false, nil
	}

	var opID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO supplier_payout_operations
		  (ledger_entry_id,funding_id,supplier_id,requested_cents,currency,status,last_error)
		VALUES ($1,$2,$3,$4,$5,'sending',NULL)
		ON CONFLICT (ledger_entry_id) DO UPDATE SET
		  funding_id=EXCLUDED.funding_id,status='sending',last_error=NULL,updated_at=now()
		WHERE supplier_payout_operations.status='ready'
		  AND (supplier_payout_operations.funding_id IS NULL
		       OR supplier_payout_operations.funding_id=EXCLUDED.funding_id)
		  AND supplier_payout_operations.supplier_id=EXCLUDED.supplier_id
		  AND supplier_payout_operations.requested_cents=EXCLUDED.requested_cents
		  AND supplier_payout_operations.currency=EXCLUDED.currency
		RETURNING id`, out.ID, fundingID, out.SupplierID, out.RequestedCents, out.Currency).Scan(&opID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, fmt.Errorf("payout operation for ledger entry %s is not retryable", entryID)
	}
	if err != nil {
		return out, false, err
	}
	ct, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1 AND payout_status=$3`,
		entryID, PayoutSending, PayoutHeld)
	if err != nil {
		return out, false, err
	}
	if ct.RowsAffected() != 1 {
		return out, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return out, false, err
	}
	return out, true, nil
}

// RecoverStalePayoutOperations conservatively marks durable sending rows whose
// worker lease expired without a terminal provider result as outcome_unknown.
// A crash could have occurred before OR after the provider crossed its cash
// boundary, so stale work must never become the definitely-not-sent ready state.
// The original updated_at is preserved so the unknown resolver can immediately
// lease an already-expired row with the same payout key and funding reservation.
func (s *Store) RecoverStalePayoutOperations(ctx context.Context, lease time.Duration, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT op.ledger_entry_id
		  FROM supplier_payout_operations op
		  JOIN ledger_entries le ON le.id=op.ledger_entry_id
		 WHERE op.status='sending' AND le.payout_status='sending'
		   AND NOT op.cash_moved AND op.transfer_ref IS NULL
		   AND op.updated_at <= $1
		 ORDER BY op.updated_at,op.ledger_entry_id
		 FOR UPDATE OF op,le SKIP LOCKED
		 LIMIT $2`, time.Now().Add(-lease), limit)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status='outcome_unknown',outcome_unknown=true,
			       last_error='sending lease expired; provider outcome unknown'
			 WHERE ledger_entry_id=$1 AND status='sending'
			   AND NOT cash_moved AND transfer_ref IS NULL`, id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ledger_entries SET payout_status='outcome_unknown'
			 WHERE id=$1 AND payout_status='sending'`, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// ClaimOutcomeUnknownPayouts leases unresolved provider attempts for an exact-key
// replay. The ledger and operation remain outcome_unknown (or reversal_required
// after a clawback) during the network call, so every concurrent reader stays
// conservative. Rows older than retryWindow are never POSTed automatically: the
// provider may have pruned its idempotency key, and a read-only/operator resolver
// is required instead.
func (s *Store) ClaimOutcomeUnknownPayouts(
	ctx context.Context,
	lease time.Duration,
	retryWindow time.Duration,
	limit int,
) ([]DueHeldEntry, error) {
	if limit <= 0 || lease < 0 || retryWindow <= 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	now := time.Now()
	rows, err := tx.Query(ctx, `
		SELECT op.ledger_entry_id,op.supplier_id,
		       op.requested_cents::float8 / 100.0,
		       settlement.liability_microusd,op.requested_cents,
		       settlement.remainder_microusd,settlement.policy,op.currency
		  FROM supplier_payout_operations op
		  JOIN ledger_entries le ON le.id=op.ledger_entry_id
		  JOIN supplier_minor_unit_settlements settlement
		    ON settlement.ledger_entry_id=le.id
		 WHERE op.outcome_unknown=true AND NOT op.cash_moved
		   AND op.transfer_ref IS NULL
		   AND op.status IN ('outcome_unknown','reversal_required')
		   AND le.payout_status IN ('outcome_unknown','reversal_required')
		   AND op.updated_at <= $1 AND op.created_at >= $2
		 ORDER BY op.updated_at,op.ledger_entry_id
		 FOR UPDATE OF op SKIP LOCKED
		 LIMIT $3`, now.Add(-lease), now.Add(-retryWindow), limit)
	if err != nil {
		return nil, err
	}
	var out []DueHeldEntry
	for rows.Next() {
		var e DueHeldEntry
		if err := rows.Scan(&e.ID, &e.SupplierID, &e.AmountUSD,
			&e.LiabilityMicros, &e.RequestedCents, &e.RemainderMicros,
			&e.SettlementPolicy, &e.Currency); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, e := range out {
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations SET updated_at=$2
			 WHERE ledger_entry_id=$1 AND outcome_unknown=true
			   AND NOT cash_moved AND transfer_ref IS NULL`, e.ID, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkPayoutOutcomeUnknown preserves an ambiguous provider result. The flag is
// sticky across retries and clawback: only an exact successful provider result
// may clear it. If a clawback already won, both rows remain reversal_required.
func (s *Store) MarkPayoutOutcomeUnknown(ctx context.Context, entryID uuid.UUID, cause error) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var ledgerStatus, opStatus string
	var cashMoved bool
	var transferRef *string
	if err := tx.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.cash_moved,op.transfer_ref
		  FROM ledger_entries le
		  JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1 FOR UPDATE OF le,op`, entryID,
	).Scan(&ledgerStatus, &opStatus, &cashMoved, &transferRef); err != nil {
		return "", err
	}
	if cashMoved || transferRef != nil {
		return "", fmt.Errorf("payout %s already has provider cash evidence", entryID)
	}
	if ledgerStatus != PayoutSending && ledgerStatus != PayoutOutcomeUnknown &&
		ledgerStatus != PayoutReversalRequired {
		return ledgerStatus, fmt.Errorf("payout %s cannot become outcome_unknown from ledger=%s operation=%s",
			entryID, ledgerStatus, opStatus)
	}
	errText := "provider outcome unknown"
	if cause != nil {
		errText = truncate(cause.Error(), 500)
	}
	next := PayoutOutcomeUnknown
	if ledgerStatus == PayoutReversalRequired || opStatus == PayoutReversalRequired {
		next = PayoutReversalRequired
	}
	if _, err := tx.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET status=$2,outcome_unknown=true,last_error=$3,updated_at=now()
		 WHERE ledger_entry_id=$1 AND NOT cash_moved AND transfer_ref IS NULL`,
		entryID, next, errText); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1`, entryID, next); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return next, nil
}

// DeferPayout moves only an in-flight sending operation to ready. If a clawback
// raced and changed the row to reversal_required, both records remain there; this
// function deliberately cannot overwrite that state.
func (s *Store) DeferPayout(ctx context.Context, entryID uuid.UUID, cause error) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var status string
	var outcomeUnknown bool
	if err := tx.QueryRow(ctx, `
		SELECT le.payout_status,COALESCE(op.outcome_unknown,false)
		  FROM ledger_entries le
		  LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1 FOR UPDATE OF le`, entryID).Scan(&status, &outcomeUnknown); err != nil {
		return "", err
	}
	if outcomeUnknown {
		return status, fmt.Errorf("payout %s has an unresolved provider outcome and cannot be deferred to ready", entryID)
	}
	if status != PayoutSending {
		return status, tx.Commit(ctx)
	}
	errText := ""
	if cause != nil {
		errText = truncate(cause.Error(), 500)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE supplier_payout_operations
		    SET status='ready',last_error=NULLIF($2,''),updated_at=now()
		  WHERE ledger_entry_id=$1 AND status='sending'`, entryID, errText); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2 WHERE id=$1 AND payout_status=$3`,
		entryID, PayoutReady, PayoutSending); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return PayoutReady, nil
}

// FinalizePayout records what crossed the provider boundary. A cash result must
// exactly match the operation's frozen cents/currency. If a clawback changed the
// state while the provider request was in flight, the transfer reference is still
// persisted but BOTH records remain reversal_required; no completion write can
// relabel the cash as recovered or safely released.
func (s *Store) FinalizePayout(ctx context.Context, entryID uuid.UUID, result PayoutResult) (string, error) {
	if strings.TrimSpace(result.Ref) == "" {
		return "", errors.New("payout result has no durable reference")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var ledgerStatus, opStatus, currency, fundingCurrency, settlementPolicy string
	var requested, fundingAmount, liabilityMicros, settlementCash, remainderMicros int64
	err = tx.QueryRow(ctx, `
		SELECT le.payout_status,op.status,op.requested_cents,op.currency,
		       funding.amount_cents,funding.currency,
		       (le.amount_usd*1000000)::bigint,settlement.policy,
		       settlement.cash_cents,settlement.remainder_microusd
		  FROM ledger_entries le
		  JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		  JOIN supplier_payout_funding funding
		    ON funding.id=op.funding_id AND funding.ledger_entry_id=le.id
		  JOIN supplier_minor_unit_settlements settlement
		    ON settlement.ledger_entry_id=le.id
		 WHERE le.id=$1
		 FOR UPDATE OF le,op,funding`, entryID).
		Scan(&ledgerStatus, &opStatus, &requested, &currency, &fundingAmount, &fundingCurrency,
			&liabilityMicros, &settlementPolicy, &settlementCash, &remainderMicros)
	if err != nil {
		return "", err
	}
	if requested != fundingAmount || currency != fundingCurrency {
		return "", fmt.Errorf(
			"payout %s operation/funding mismatch: operation=%d %s funding=%d %s",
			entryID, requested, currency, fundingAmount, fundingCurrency)
	}
	if settlementPolicy != supplierSettlementPolicyFloorCentCarryV1 ||
		requested != settlementCash || liabilityMicros != settlementCash*microUSDPerCent+remainderMicros {
		return "", fmt.Errorf(
			"payout %s minor-unit reconciliation mismatch: policy=%s liability=%d requested=%d settlement=%d remainder=%d",
			entryID, settlementPolicy, liabilityMicros, requested, settlementCash, remainderMicros)
	}
	if result.CashMoved {
		if result.SentCents != requested || result.Currency != currency {
			return "", fmt.Errorf(
				"payout %s cash mismatch: requested=%d %s sent=%d %s",
				entryID, requested, currency, result.SentCents, result.Currency)
		}
		finalStatus := PayoutReleased
		if ledgerStatus == PayoutReversalRequired || ledgerStatus == PayoutClawedBack ||
			opStatus == PayoutReversalRequired || opStatus == PayoutClawedBack {
			finalStatus = PayoutReversalRequired
		} else if !((ledgerStatus == PayoutSending && opStatus == PayoutSending) ||
			(ledgerStatus == PayoutOutcomeUnknown && opStatus == PayoutOutcomeUnknown)) {
			return "", fmt.Errorf("payout %s cannot complete from ledger=%s operation=%s", entryID, ledgerStatus, opStatus)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status=$2,sent_cents=$3,currency=$4,cash_moved=true,
			       outcome_unknown=false,transfer_ref=$5,last_error=NULL,updated_at=now()
			 WHERE ledger_entry_id=$1`,
			entryID, finalStatus, result.SentCents, result.Currency, result.Ref); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status=$2,payout_ref=$3 WHERE id=$1`,
			entryID, finalStatus, result.Ref); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return finalStatus, nil
	}

	// A manual export is coordination only. It may resolve an ambiguous synced-file
	// response with the same payout key, but it never satisfies supplier cash. A
	// clawback that already won remains reversal_required because the out-of-band
	// instruction may still need cancellation.
	if ledgerStatus == PayoutReversalRequired || opStatus == PayoutReversalRequired {
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status='reversal_required',outcome_unknown=false,
			       transfer_ref=$2,last_error='manual export requires external cancellation',updated_at=now()
			 WHERE ledger_entry_id=$1`, entryID, result.Ref); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET payout_status='reversal_required',payout_ref=$2 WHERE id=$1`,
			entryID, result.Ref); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return PayoutReversalRequired, nil
	}
	if !((ledgerStatus == PayoutSending && opStatus == PayoutSending) ||
		(ledgerStatus == PayoutOutcomeUnknown && opStatus == PayoutOutcomeUnknown)) {
		return "", fmt.Errorf("non-cash payout %s cannot complete from ledger=%s operation=%s", entryID, ledgerStatus, opStatus)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET status='exported',cash_moved=false,outcome_unknown=false,
		       transfer_ref=$2,last_error=NULL,updated_at=now()
		 WHERE ledger_entry_id=$1`, entryID, result.Ref); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE ledger_entries SET payout_status=$2,payout_ref=$3 WHERE id=$1`,
		entryID, PayoutExported, result.Ref); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return PayoutExported, nil
}

// SetChargeStatus records the outcome of a job's off-session buyer charge
// (not_attempted|charged|failed|no_payment_method) so charging state is queryable
// rather than log-only. Best-effort: a failure here never blocks the lifecycle.
func (s *Store) SetChargeStatus(ctx context.Context, jobID uuid.UUID, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE jobs SET charge_status = $2 WHERE id = $1`, jobID, status)
	return err
}

// RecordVerificationEvent appends one row to the append-only verification_events
// receipt log. It is DURABLE and fail-closed: verification callers propagate a
// write error, so settlement cannot proceed without the fact its buyer receipt
// depends on. The task/kind unique index makes retries idempotent. kind is one of the closed set
// {honeypot_pass|honeypot_fail|redundancy_match|redundancy_mismatch|tiebreak_win|
// tiebreak_loss}; taskID/supplierID may be uuid.Nil when not known, stored as NULL.
func (s *Store) RecordVerificationEvent(ctx context.Context, jobID, taskID, supplierID uuid.UUID, kind string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO verification_events (job_id, task_id, supplier_id, kind)
		 VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		jobID, nullUUID(taskID), nullUUID(supplierID), kind)
	return err
}

// JobVerification aggregates a job's verification_events log into the buyer-facing
// receipt block, plus the latest dispute's status (disputes table; ” when none).
// Counts come from a single grouped query; only outcomes that actually occurred are
// present, so the aggregate never overstates what was checked. `checked` is every
// task that underwent ANY verification (the sum of all event kinds). The honest label
// is derived from the counts by deriveVerificationLabel.
func (s *Store) JobVerification(ctx context.Context, jobID uuid.UUID) (Verification, error) {
	var v Verification
	rows, err := s.pool.Query(ctx,
		`SELECT kind, count(*) FROM verification_events WHERE job_id = $1 GROUP BY kind`, jobID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return v, err
		}
		switch kind {
		case "honeypot_pass":
			v.HoneypotsPassed += n
			v.Checked += n
		case "honeypot_fail":
			v.HoneypotsFailed += n
			v.Checked += n
		case "redundancy_match":
			v.RedundancyMatched += n
			v.Checked += n
		case "redundancy_mismatch":
			v.RedundancyMismatched += n
			v.Checked += n
		case "tiebreak_win", "tiebreak_loss":
			v.Tiebreaks += n
			v.Checked += n
		case "redundancy_same_supplier":
			// A same-supplier "peer" — NOT an independent cross-check (items 7, 9).
			// Surfaced as its own count and deliberately NOT added to Checked, so the
			// receipt can say "no-independent-peer" rather than "verified".
			v.SameSupplier += n
		case "redundancy_cross_class", "tiebreak_cross_class":
			// Cross-class coverage gap: the chunk had a redundant peer but in a
			// DIFFERENT verification class, so a byte-exact comparison could not be
			// performed. Surfaced as CrossClassSkipped for the receipt (item 9) but NOT
			// counted as "checked" — counting an uncheckable comparison as verified
			// would overstate the receipt (BLACKHOLE: surface the gap, never inflate).
			v.CrossClassSkipped += n
		}
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
	// Per-deliverable coverage. Independent evidence may be recorded on the
	// redundancy/tiebreak task rather than the primary, so coverage joins events
	// back to the shared chunk_index and counts distinct chunks. Honeypots are
	// supplier audits, not buyer-output verification, and never enter either side.
	if err := s.pool.QueryRow(ctx,
		`SELECT
		   (SELECT COUNT(DISTINCT COALESCE(t.chunk_index,0))
		      FROM tasks t
		     WHERE t.job_id=$1 AND t.status='complete'
		       AND t.is_honeypot=false AND t.is_redundancy=false),
		   (SELECT COUNT(DISTINCT COALESCE(et.chunk_index,0))
		      FROM verification_events ve
		      JOIN tasks et ON et.id=ve.task_id
		     WHERE ve.job_id=$1 AND et.job_id=$1
		       AND ve.kind IN ('redundancy_match','tiebreak_win'))`, jobID,
	).Scan(&v.DeliveredChunks, &v.VerifiedChunks); err != nil {
		return v, err
	}
	if v.VerifiedChunks > v.DeliveredChunks {
		v.VerifiedChunks = v.DeliveredChunks
	}
	v.UnverifiedChunks = v.DeliveredChunks - v.VerifiedChunks
	// Latest dispute for the job ('' when none). A no-row scan is the normal "no
	// dispute" case, not an error.
	var disputeStatus string
	err = s.pool.QueryRow(ctx,
		`SELECT status FROM disputes WHERE job_id = $1 ORDER BY created_at DESC LIMIT 1`, jobID,
	).Scan(&disputeStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	v.DisputeStatus = disputeStatus
	v.Label = deriveVerificationLabel(v)
	return v, nil
}

// SupplierVerification aggregates a SUPPLIER's own verification_events log — across
// every job they have ever worked, not one job — into the trust-panel receipt
// (Supplier onboarding & safety 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md:
// "Populate the trust panel with real data"). Only honeypot outcomes are counted
// (the trust panel's own vocabulary — payouts_configured/connected/enabled and
// honeypots_passed/failed/verification_label); redundancy/tiebreak counts stay
// job-scoped via JobVerification. Reuses deriveVerificationLabel so the derived
// label means exactly the same thing here as it does on a job receipt.
func (s *Store) SupplierVerification(ctx context.Context, supplierID uuid.UUID) (SupplierVerification, error) {
	var sv SupplierVerification
	rows, err := s.pool.Query(ctx,
		`SELECT kind, count(*) FROM verification_events
		  WHERE supplier_id = $1 AND kind IN ('honeypot_pass','honeypot_fail')
		  GROUP BY kind`, supplierID)
	if err != nil {
		return sv, err
	}
	defer rows.Close()
	var v Verification
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return sv, err
		}
		switch kind {
		case "honeypot_pass":
			sv.HoneypotsPassed = n
			v.HoneypotsPassed = n
			v.Checked += n
		case "honeypot_fail":
			sv.HoneypotsFailed = n
			v.HoneypotsFailed = n
			v.Checked += n
		}
	}
	if err := rows.Err(); err != nil {
		return sv, err
	}
	sv.Label = deriveVerificationLabel(v)
	return sv, nil
}

// JobVerificationClasses returns the DISTINCT verification classes ("engine|build_hash")
// of the workers that produced this job's completed (non-honeypot) results — the
// "cleared under" provenance for the ClearingReceipt (items 13, 15). A blank class
// (unknown build) maps to "" via classKey. Read-only.
func (s *Store) JobVerificationClasses(ctx context.Context, jobID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		                 COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,'')
		 FROM tasks t
		 LEFT JOIN verification_work vw ON vw.task_id=t.id AND vw.attempt=t.retry_count
		 WHERE t.job_id = $1 AND t.status = 'complete' AND t.is_honeypot = false`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var engine, build string
		if err := rows.Scan(&engine, &build); err != nil {
			return nil, err
		}
		out = append(out, classKey(engine, build))
	}
	return out, rows.Err()
}

// JobTaskReceipts returns the per-task verification drilldown for a job (item 15): each
// task's chunk, status, honeypot flag, worker verification class, and its latest
// comparison event kind. It NEVER selects the honeypot known_answer, so the drilldown
// cannot leak the hidden probe answer. Read-only.
func (s *Store) JobTaskReceipts(ctx context.Context, jobID uuid.UUID) ([]TaskReceipt, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(t.chunk_index,0), t.status, t.is_honeypot,
		        COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,''),
		        COALESCE((SELECT ve.kind FROM verification_events ve
		                  WHERE ve.task_id = t.id ORDER BY ve.created_at DESC LIMIT 1), ''),
		        COALESCE(t.verification_outcome,''),
		        COALESCE(t.runtime_cell_id,''), COALESCE(t.runtime_id,''),
		        COALESCE(t.runtime_matrix_sha256,''), COALESCE(t.model_kind,'')
		 FROM tasks t
		 LEFT JOIN verification_work vw ON vw.task_id=t.id AND vw.attempt=t.retry_count
		 WHERE t.job_id = $1
		 ORDER BY COALESCE(t.chunk_index,0), t.id`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskReceipt
	for rows.Next() {
		var (
			chunk                        int
			status                       string
			isHoneypot                   bool
			engine, build                string
			kind, verdict                string
			cellID, runtimeID, matrixSHA string
			modelKind                    string
		)
		if err := rows.Scan(&chunk, &status, &isHoneypot, &engine, &build, &kind, &verdict,
			&cellID, &runtimeID, &matrixSHA, &modelKind); err != nil {
			return nil, err
		}
		out = append(out, taskReceiptRowWithRuntime(chunk, status, isHoneypot, engine, build,
			kind, verdict, cellID, runtimeID, matrixSHA, modelKind))
	}
	return out, rows.Err()
}

// RecordDispute records a buyer's dispute against a job's result, atomically verifying
// the job belongs to that buyer (the INSERT...SELECT yields no row — errNotFound — when
// it does not, so a buyer can never dispute another's job). Returns the new dispute id.
// This is the foundation primitive for optimistic-verification recompute + the payout
// guarantee; resolution (operator bisection / tolerance-aware FP) is the frontier seam.
func (s *Store) RecordDispute(ctx context.Context, jobID, buyerID uuid.UUID, reason string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO disputes (job_id, buyer_id, reason)
		 SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM jobs WHERE id = $1 AND buyer_id = $2)
		 RETURNING id`,
		jobID, buyerID, reason).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errNotFound
	}
	return id, err
}

// DisputeRow is an unresolved dispute the resolver works on.
type DisputeRow struct {
	ID, JobID uuid.UUID
	Status    string
}

// ActiveDisputes returns disputes still needing resolution: 'open'/'no_peer' need a
// re-verify dispatched; 'reverifying' awaits the independent re-run's objective verdict.
func (s *Store) ActiveDisputes(ctx context.Context, limit int) ([]DisputeRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, job_id, status FROM disputes
		  WHERE status IN ('open','no_peer','reverifying') ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DisputeRow
	for rows.Next() {
		var d DisputeRow
		if err := rows.Scan(&d.ID, &d.JobID, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReverifyTargetRow is the disputed job's primary completed task + the routing facts a
// re-verification peer needs.
type ReverifyTargetRow struct {
	TaskID, AnchorWorker uuid.UUID
	JobType, ModelRef    string
	InputRef             string
	MinMemGB             float32
	ChunkIndex           int
}

// ReverifyTarget returns the disputed job's primary completed task (chunk 0, not a
// redundancy/honeypot) to independently re-run. ok=false when the job has no such
// completed task to re-verify.
func (s *Store) ReverifyTarget(ctx context.Context, jobID uuid.UUID) (ReverifyTargetRow, bool, error) {
	var t ReverifyTargetRow
	err := s.pool.QueryRow(ctx,
		`SELECT tk.id, tk.worker_id, j.job_type, COALESCE(j.model_ref,''),
		        tk.input_ref, COALESCE(j.min_memory_gb,0), tk.chunk_index
		   FROM tasks tk JOIN jobs j ON j.id = tk.job_id
		  WHERE tk.job_id = $1 AND tk.status = 'complete'
		        AND COALESCE(tk.is_redundancy,false) = false
		        AND COALESCE(tk.is_honeypot,false) = false
		        AND tk.worker_id IS NOT NULL
		  ORDER BY tk.chunk_index LIMIT 1`, jobID).
		Scan(&t.TaskID, &t.AnchorWorker, &t.JobType, &t.ModelRef, &t.InputRef, &t.MinMemGB, &t.ChunkIndex)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	return t, true, nil
}

// SetDisputeReverifying records the dispatched re-verify task and flips to 'reverifying'.
func (s *Store) SetDisputeReverifying(ctx context.Context, id, reverifyTaskID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE disputes SET status = 'reverifying', reverify_task_id = $2 WHERE id = $1`,
		id, reverifyTaskID)
	return err
}

// SetDisputeStatus updates a dispute's status (stamping resolved_at on a terminal one).
func (s *Store) SetDisputeStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE disputes SET status = $2,
		        resolved_at = CASE WHEN $2 IN ('resolved','rejected') THEN now() ELSE resolved_at END
		  WHERE id = $1`, id, status)
	return err
}

// JobHasPendingTasks reports whether a job still has queued/running tasks — the resolver
// waits on this so a re-verify (and any cascaded tiebreak) fully settles before verdict.
func (s *Store) JobHasPendingTasks(ctx context.Context, jobID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE job_id = $1 AND status IN ('queued','retrying','running')`,
		jobID).Scan(&n)
	return n > 0, err
}

// TaskHasClawback reports whether a confirmed-bad clawback was recorded against a task —
// the OBJECTIVE verdict signal for a dispute (the original result was wrong).
func (s *Store) TaskHasClawback(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE kind = 'clawback' AND task_id = $1)`,
		taskID).Scan(&exists)
	return exists, err
}

// MarkPayout records the terminal provider status and reference for a ledger entry.
func (s *Store) MarkPayout(ctx context.Context, entryID uuid.UUID, status, ref string) error {
	// Invariant (BLACKHOLE: never fake a transfer): a credit may only be marked
	// 'released' WITH a real rail reference. Enforced here and, structurally, by the
	// ledger_released_requires_ref CHECK constraint in db/schema.sql.
	if status == PayoutReleased && ref == "" {
		return fmt.Errorf("refusing to mark ledger entry %s 'released' without a payout reference", entryID)
	}
	switch status {
	case PayoutReady:
		_, err := s.DeferPayout(ctx, entryID, nil)
		return err
	case PayoutReleased:
		var cents int64
		var currency string
		if err := s.pool.QueryRow(ctx,
			`SELECT requested_cents,currency FROM supplier_payout_operations WHERE ledger_entry_id=$1`,
			entryID).Scan(&cents, &currency); err != nil {
			return err
		}
		_, err := s.FinalizePayout(ctx, entryID, PayoutResult{
			Ref: ref, SentCents: cents, Currency: currency, CashMoved: true,
		})
		return err
	default:
		return fmt.Errorf("unsupported payout transition to %q; use the durable payout operation", status)
	}
}

// StaleTask is a running task whose claim has outlived its timeout.
type StaleTask struct {
	ID         uuid.UUID
	JobID      uuid.UUID
	RetryCount int16
}

// StaleRunningTasks finds tasks stuck in running before any durable upload.
// Verifying tasks are owned by verification_work and its expiring fenced lease;
// blindly requeueing one by claimed_at would discard a sealed upload while its
// recovery processor was working or after the request process died.
func (s *Store) StaleRunningTasks(ctx context.Context, timeout time.Duration, limit int) ([]StaleTask, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, job_id, retry_count FROM tasks
		 WHERE status = 'running' AND claimed_at IS NOT NULL
		   AND claimed_at < now() - make_interval(secs => $1)
		 ORDER BY claimed_at ASC LIMIT $2`,
		timeout.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleTask
	for rows.Next() {
		var t StaleTask
		if err := rows.Scan(&t.ID, &t.JobID, &t.RetryCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Straggler is a running task that has run long enough to warrant a hedge: a
// duplicate copy dispatched to a second worker so the buyer is not held hostage
// by one slow node. Carries the chunk identity the hedge clones.
type Straggler struct {
	TaskID     uuid.UUID
	JobID      uuid.UUID
	WorkerID   uuid.UUID
	JobType    string
	ModelRef   string
	InputRef   string
	ChunkIndex int
	MinMemGB   float32
	// ThrottledHedge is true when this straggler was selected via the SHORT
	// throttled-worker path (docs/internal/CREED_AND_PATH_TO_TEN.md, "Thermal
	// sustained-vs-peak throughput on fanless Apple Silicon" 7→8: "detect
	// throttling live... the same way a stalled worker triggers a hedge
	// today") rather than the normal elapsed-time `after` path — purely
	// informational (logging/metrics), never changes hedge mechanics.
	ThrottledHedge bool
}

// StragglerTasks finds running PRIMARY tasks (not honeypot, not redundancy, not
// themselves a hedge) that warrant a hedge via EITHER of two independent paths:
//
//  1. elapsed time exceeds `after` (the original, pre-existing path — a slow
//     worker, regardless of why).
//  2. the CLAIMING WORKER's own most recent heartbeat currently reports
//     `throttled = true` (memory pressure OR a live sustained-throughput drop —
//     see agent/src/runners.rs's LiveThroughputMonitor / main.rs's `throttled:
//     throttle.throttled || live_throttling`) AND the task has run at least
//     `throttledAfter` (a short floor — never zero — so a task that started a
//     heartbeat-tick ago isn't hedged before it could possibly have produced a
//     result either way). This is deliberately MUCH shorter than `after`: a
//     worker that is DEMONSTRABLY throttling right now, live, is a stronger and
//     earlier signal than "this task has simply been running a while", which is
//     exactly the facet's own proof artifact ("triggers a hedge before the
//     stale-worker watchdog would have caught it" — here, before even the
//     normal elapsed-time hedge would have caught it).
//
// Excludes any chunk that already has a hedge in flight (so a straggler is
// hedged at most once) and any whose job already has >= maxInFlight hedges
// running (hedge sparingly). Ordered oldest-start first, capped at limit.
func (s *Store) StragglerTasks(ctx context.Context, after, throttledAfter time.Duration, maxInFlight, limit int) ([]Straggler, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.job_id, t.worker_id, j.job_type, COALESCE(j.model_ref,''),
		        COALESCE(t.input_ref,''), COALESCE(t.chunk_index,0), COALESCE(j.min_memory_gb,0),
		        COALESCE(w.throttled, false)
		 FROM tasks t JOIN jobs j ON j.id = t.job_id
		 LEFT JOIN workers w ON w.id = t.worker_id
		 WHERE t.status = 'running'
		   AND t.is_honeypot = false AND t.is_redundancy = false
		   AND t.hedged_from IS NULL
		   AND t.started_at IS NOT NULL
		   AND (
		     t.started_at < now() - make_interval(secs => $1)
		     OR (
		       COALESCE(w.throttled, false)
		       AND t.started_at < now() - make_interval(secs => $2)
		     )
		   )
		   AND j.status = 'running'
		   -- this chunk is not already hedged:
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks h
		     WHERE h.job_id = t.job_id AND COALESCE(h.chunk_index,0) = COALESCE(t.chunk_index,0)
		       AND h.hedged_from IS NOT NULL AND h.is_redundancy = false
		       AND h.status NOT IN ('failed','cancelled')
		   )
		   -- and the job is under its in-flight hedge cap:
		   AND (
		     SELECT count(*) FROM tasks h2
		     WHERE h2.job_id = t.job_id AND h2.hedged_from IS NOT NULL
		       AND h2.is_redundancy = false AND h2.status IN ('queued','running')
		   ) < $3
		 ORDER BY t.started_at ASC
		 LIMIT $4`,
		after.Seconds(), throttledAfter.Seconds(), maxInFlight, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Straggler
	for rows.Next() {
		var s Straggler
		var throttled bool
		if err := rows.Scan(&s.TaskID, &s.JobID, &s.WorkerID, &s.JobType, &s.ModelRef,
			&s.InputRef, &s.ChunkIndex, &s.MinMemGB, &throttled); err != nil {
			return nil, err
		}
		// ThrottledHedge is set whenever the worker is currently throttled AND
		// this task hasn't yet crossed the normal `after` threshold — i.e. it is
		// a candidate ONLY because of the throttled-worker path, not the
		// elapsed-time path too (both can independently be true for an
		// old-enough task; that's still "elapsed" for attribution purposes).
		s.ThrottledHedge = throttled
		out = append(out, s)
	}
	return out, rows.Err()
}

// EndgameTailTasks finds the ENDGAME RACE candidates (Speed Lane wave 1B,
// workers.go raceEndgameTails): running PRIMARY tasks (not honeypot, not
// redundancy, not themselves a hedge) of a running job that has ZERO unclaimed
// work left — no queued/retrying task with claimed_by IS NULL, visible or not
// (a backoff-hidden retry still counts as work coming back, so its job is
// conservatively NOT in endgame). At that point the job's wall-clock IS the
// slowest running chunk, so each candidate is worth duplicating onto idle
// spare capacity IMMEDIATELY instead of waiting out the 90s elapsed-time
// hedge. minRun is a small floor (a chunk that just started is about to finish
// anyway — duplicating it is pure waste); the not-already-hedged-chunk and
// per-job in-flight-hedge-cap guards are byte-identical to StragglerTasks so
// the race and the hedge can never double-duplicate one chunk or blow the same
// cap. Ordered oldest-start first — the longest-running chunk is the tail.
func (s *Store) EndgameTailTasks(ctx context.Context, minRun time.Duration, maxInFlight, limit int) ([]Straggler, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.job_id, t.worker_id, j.job_type, COALESCE(j.model_ref,''),
		        COALESCE(t.input_ref,''), COALESCE(t.chunk_index,0), COALESCE(j.min_memory_gb,0)
		 FROM tasks t JOIN jobs j ON j.id = t.job_id
		 WHERE t.status = 'running'
		   AND t.is_honeypot = false AND t.is_redundancy = false
		   AND t.hedged_from IS NULL
		   AND t.started_at IS NOT NULL
		   AND t.started_at < now() - make_interval(secs => $1)
		   AND j.status = 'running'
		   -- ENDGAME: no unclaimed queued/retrying work remains on this job.
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks q
		     WHERE q.job_id = t.job_id AND q.status IN ('queued','retrying')
		       AND q.claimed_by IS NULL
		   )
		   -- this chunk is not already hedged (same guard as StragglerTasks):
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks h
		     WHERE h.job_id = t.job_id AND COALESCE(h.chunk_index,0) = COALESCE(t.chunk_index,0)
		       AND h.hedged_from IS NOT NULL AND h.is_redundancy = false
		       AND h.status NOT IN ('failed','cancelled')
		   )
		   -- and the job is under its in-flight hedge cap (same as StragglerTasks):
		   AND (
		     SELECT count(*) FROM tasks h2
		     WHERE h2.job_id = t.job_id AND h2.hedged_from IS NOT NULL
		       AND h2.is_redundancy = false AND h2.status IN ('queued','running')
		   ) < $2
		 ORDER BY t.started_at ASC
		 LIMIT $3`,
		minRun.Seconds(), maxInFlight, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Straggler
	for rows.Next() {
		var s Straggler
		if err := rows.Scan(&s.TaskID, &s.JobID, &s.WorkerID, &s.JobType, &s.ModelRef,
			&s.InputRef, &s.ChunkIndex, &s.MinMemGB); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BusyWorkerIDs reports which of the given workers currently hold work: a
// RUNNING task, or a queued/retrying task pinned to them (claimed_by — a
// tiebreak/hedge/race dispatch they are about to pick up). Used by
// SelectEndgameRacePeer to restrict the race to genuinely IDLE spare capacity
// in one query instead of a per-candidate probe. Workers absent from the map
// are idle.
func (s *Store) BusyWorkerIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	busy := make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return busy, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.worker_id FROM tasks t
		 WHERE t.status = 'running' AND t.worker_id = ANY($1)
		 UNION
		 SELECT t.claimed_by FROM tasks t
		 WHERE t.status IN ('queued','retrying') AND t.claimed_by = ANY($1)`,
		ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		busy[id] = true
	}
	return busy, rows.Err()
}

// InsertHedgeTask inserts a straggler hedge: a DUPLICATE primary (is_redundancy =
// false so the merge will accept its result, hedged_from = the slow task) for the
// same chunk, pinned (pre-claimed, not started) to a chosen distinct same-class
// peer. It does NOT bump task_count — a hedge is a duplicate of work already
// counted, and "first commit wins" (the merge dedupes per chunk; the loser is
// cancelled on the winner's commit). Returns the new task id.
func (s *Store) InsertHedgeTask(ctx context.Context, jobID, primaryTaskID, peerWorker uuid.UUID, inputRef string, chunkIndex int) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	buyerCharge, supplierPayout, err := consumeEconomicReserveTx(ctx, tx, jobID)
	if err != nil {
		if errors.Is(err, ErrEconomicReserveExhausted) {
			var existing uuid.UUID
			qerr := tx.QueryRow(ctx, `
				SELECT id FROM tasks
				 WHERE job_id=$1 AND hedged_from=$2 AND is_redundancy=false
				 ORDER BY created_at LIMIT 1`, jobID, primaryTaskID).Scan(&existing)
			if qerr == nil {
				return existing, nil
			}
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return uuid.Nil, qerr
			}
		}
		return uuid.Nil, err
	}

	// Serialize against FinalizeTaskVerification on the original. A hedge is only
	// admissible while that original is still live; otherwise a creator racing the
	// winning commit could insert fresh work after the winner had already settled.
	var primaryStatus string
	err = tx.QueryRow(ctx, `
		SELECT status FROM tasks
		 WHERE id=$1 AND job_id=$2 AND is_redundancy=false
		 FOR UPDATE`, primaryTaskID, jobID).Scan(&primaryStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrEconomicReserveExhausted
	}
	if err != nil {
		return uuid.Nil, err
	}
	if primaryStatus != "running" && primaryStatus != "verifying" {
		return uuid.Nil, ErrEconomicReserveExhausted
	}

	// The reserve update serialized creators for the job and the original-row
	// lock serialized them with settlement. Recheck now, inside both locks, so a
	// concurrent endgame sweep returns the one existing hedge without consuming
	// a second reserve slot.
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM tasks
		 WHERE job_id=$1 AND hedged_from=$2 AND is_redundancy=false
		 ORDER BY created_at LIMIT 1`, jobID, primaryTaskID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	id := uuid.New()
	resultKey := fmt.Sprintf("jobs/%s/hedge/%s/result.json", jobID, id)
	_, err = tx.Exec(ctx,
		`INSERT INTO tasks
		   (id, job_id, status, is_honeypot, is_redundancy, retry_count,
		    input_ref, result_key, chunk_index, hedged_from,expected_output_records,
		    claimed_by, claimed_at, visible_at,
		    economic_buyer_charge_usd,economic_supplier_payout_usd)
		 VALUES ($1,$2,'queued',false,false,0,$3,$4,$5,$6,
		         (SELECT expected_output_records FROM tasks WHERE id=$6),
		         $7, now(), now(),$8,$9)`,
		id, jobID, inputRef, resultKey, chunkIndex, primaryTaskID, peerWorker,
		buyerCharge, supplierPayout)
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// CancelStragglerSiblings implements "first commit wins": once a task for a chunk
// commits, any OTHER not-complete HEDGE (or hedged primary) for that same chunk is
// marked failed so it stops blocking job completion and frees its worker. It never
// touches the just-committed task, completed tasks, or verification-redundancy
// tasks (is_redundancy=true with no hedged_from). Idempotent.
//
// PATCH (Speed Lane wave 1B, planner.go / raceEndgameTails): "first commit
// wins" now works in BOTH directions. The original predicate matched ONLY
// hedge copies (hedged_from IS NOT NULL), so when the HEDGE/RACE duplicate
// committed FIRST, the hedged ORIGINAL kept running — and since job
// completion (JobAllTasksDone) requires every task terminal, the job STILL
// waited out the slow original, which nullified the entire wall-clock point
// of duplicating the tail. The predicate now also cancels the hedged ORIGINAL
// — but ONLY when the just-committed keep task ($3) is that original's OWN
// winning duplicate (h.id = $3 AND h.hedged_from = tasks.id AND
// h.is_redundancy = false). Deliberately NOT any broader trigger: a
// verification-redundancy clone or tiebreak commit for the same chunk also
// flows through this function, and neither may ever cancel a still-running
// primary (their results are never the deliverable — the chunk would be left
// with no primary result at all). The cancelled original was never committed,
// so no payout was ever scheduled for it — money is untouched. A worker that
// later tries to commit the cancelled task gets the pre-existing 409 conflict
// (CommitTask requires status running/queued), the same contract a losing
// hedge has always had.
func (s *Store) CancelStragglerSiblings(ctx context.Context, jobID uuid.UUID, chunkIndex int, keepTaskID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tasks
		   SET status = 'failed', claimed_by = NULL
		 WHERE job_id = $1 AND COALESCE(chunk_index,0) = $2
		   AND id <> $3
		   AND status IN ('queued','running','retrying')
		   AND is_redundancy = false AND is_honeypot = false
		   AND (hedged_from IS NOT NULL
		        OR EXISTS (
		          SELECT 1 FROM tasks h
		          WHERE h.id = $3 AND h.hedged_from = tasks.id
		            AND h.is_redundancy = false
		        ))`,
		jobID, chunkIndex, keepTaskID)
	return err
}

// RequeueStaleTask pushes a stale pre-upload running task back to the queue with a backoff:
// clears the claim, increments retry_count, and sets visible_at = now()+backoff.
func (s *Store) RequeueStaleTask(ctx context.Context, taskID uuid.UUID, backoff time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`WITH requeued AS (
		 UPDATE tasks
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'running'
		 RETURNING job_id
		)
		UPDATE jobs SET status = 'running'
		 WHERE id IN (SELECT job_id FROM requeued) AND status = 'verifying'`,
		taskID, backoff.Seconds())
	return err
}

// FailTaskAndSettleJob marks a task permanently failed (retries exhausted) and
// fails its parent job, settling the job at the work that actually completed
// (partial-settle everywhere — see failJobAndSettleOnce). The caller checkpoints
// completed chunks BEFORE calling this (merge-before-mark), so delivered work is
// downloadable even off the failure path.
func (s *Store) FailTaskAndSettleJob(ctx context.Context, taskID, jobID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'failed'
		  WHERE id = $1 AND job_id=$2 AND status='running'`, taskID, jobID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return nil // the upload/verdict path or another reaper won the task race
	}
	if _, err := failJobAndSettleOnce(ctx, tx, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// JobCheckpointInfo returns what the merge-before-fail discipline needs to decide
// whether a checkpoint merge is worth attempting: the job's output_ref (empty =
// nowhere to write) and its completed-task count (0 = nothing to checkpoint).
func (s *Store) JobCheckpointInfo(ctx context.Context, jobID uuid.UUID) (outputRef string, tasksDone int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(output_ref,''), COALESCE(tasks_done,0) FROM jobs WHERE id = $1`,
		jobID).Scan(&outputRef, &tasksDone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, errNotFound
	}
	return outputRef, tasksDone, err
}

// TaskJobID resolves a task's parent job. Used by the fail endpoint to checkpoint
// delivered chunks AFTER a validated terminal failure (the checkpoint needs the job,
// FailTask returns only the outcome). errNotFound when the task does not exist.
func (s *Store) TaskJobID(ctx context.Context, taskID uuid.UUID) (uuid.UUID, error) {
	var jobID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT job_id FROM tasks WHERE id = $1`, taskID).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errNotFound
	}
	return jobID, err
}

// failJobAndSettleOnce flips a job to 'failed' EXACTLY once, even when several of
// the job's tasks fail terminally (e.g. multiple workers each report bad input, or
// the stale reaper and the fail endpoint both fire). It is a no-op when the job is
// already terminal. `flipped` is true only on the call that actually transitioned
// the job, so the caller emits the job_failed event exactly once.
//
// MONEY (partial-settle everywhere, same rule as the stuck-run watchdog): nothing
// is refunded via ledger rows, because per-task charges settle only at a verified
// commit — completed chunks were DELIVERED and stay charged (the supplier earned
// them), and the un-run remainder was never charged in the first place. The job's
// actual_usd is settled here to the sum of those completed-task charges, so a job
// with ZERO delivered chunks honestly settles at $0 (nothing charged, nothing owed)
// with no refund row needed.
func failJobAndSettleOnce(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (flipped bool, err error) {
	status, pending, err := jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if status == "complete" || status == "cancelled" || status == "failed" {
		return false, nil // already terminal — nothing to settle again
	}
	if pending {
		return false, ErrJobVerificationPending
	}
	ct, err := tx.Exec(ctx,
		`UPDATE jobs SET status = 'failed'
		  WHERE id = $1 AND status NOT IN ('complete','cancelled','failed')`, jobID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() != 1 {
		return false, nil
	}
	// Settle at completed work (the tx-scoped twin of SetJobActualUSD).
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET actual_usd = COALESCE((
		   SELECT SUM(-amount_usd) FROM ledger_entries
		   WHERE kind = 'buyer_charge'
		     AND task_id IN (SELECT id FROM tasks WHERE job_id = $1)
		 ),0)
		 WHERE id = $1`, jobID); err != nil {
		return false, err
	}
	return true, nil
}

// StuckJob is a running job the watchdog judged stuck: past its deadline with no
// task progress. Carries what the reaper needs to escalate (rescue or kill),
// checkpoint, cancel + settle, and attribute the stall.
type StuckJob struct {
	ID        uuid.UUID
	BuyerID   uuid.UUID
	OutputRef string
	EtaSecs   int
	TasksDone int
	TaskCount int
	Strikes   int  // watchdog_strikes: 0 → rescue next, >=1 → kill next
	DeadClaim bool // an unfinished task is claimed by a DEAD worker (machine-stuck, not workload-stuck)
}

// StuckRunningJobs returns running jobs past their deadline with NO task progress
// (no commit, no fresh claim, and no recently-scheduled retry visibility) within
// grace. Progress within grace — even slow progress — exempts a job: the watchdog
// regulates stuck runs, never merely slow ones (hedging already covers slow).
//
// The deadline is, in precedence order (each case OWNS its jobs — a later clause
// never overrides an earlier one, so an explicit 3-day deadline is never cut short
// by the fallback cap):
//   - an explicit buyer deadline_secs (> 0): a hard wall-clock deadline;
//   - otherwise the ETA-derived deadline: factor × eta_secs, FLOORED at
//     eta_secs + 120s so a tiny prediction (eta 10s → 15s at 1.5×) cannot judge a
//     job faster than a human could blink;
//   - otherwise (no explicit deadline AND no ETA) a 24-hour wall-clock cap — the
//     only deadline a no-prediction job can honestly be held to.
//
// deadline_secs = -1 is the buyer's opt-out: the job is NEVER judged, not even by
// the 24h cap (they asked for run-to-completion and get exactly that).
//
// The visible_at term in the progress check makes a just-rescued/requeued task
// count as progress: its visibility backoff sits in the near future, which proves
// the queue is actively re-placing the work — without it, a rescue would be judged
// "still no progress" on the very next sweep and killed before any worker could
// claim. deadAfter bounds the worker-liveness attribution: DeadClaim is true when
// an unfinished task's claiming worker has not heartbeated within it (or ever).
func (s *Store) StuckRunningJobs(ctx context.Context, factor float64, grace, deadAfter time.Duration, limit int) ([]StuckJob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.buyer_id, COALESCE(j.output_ref,''), COALESCE(j.eta_secs,0),
		        j.tasks_done, j.task_count, COALESCE(j.watchdog_strikes,0),
		        EXISTS (
		          SELECT 1 FROM tasks t JOIN workers w ON w.id = t.claimed_by
		          WHERE t.job_id = j.id AND t.status = 'running'
		            AND (w.last_seen_at IS NULL OR w.last_seen_at < now() - make_interval(secs => $4::float8))
		        ) AS dead_claim
		 FROM jobs j
		 WHERE j.status = 'running'
		   AND COALESCE(j.deadline_secs, 0) <> -1
		   AND (
		     (j.deadline_secs IS NOT NULL AND j.deadline_secs > 0
		       AND now() > j.created_at + make_interval(secs => j.deadline_secs::float8))
		     OR (COALESCE(j.deadline_secs, 0) = 0 AND COALESCE(j.eta_secs, 0) > 0
		       AND now() > j.created_at + GREATEST(
		             make_interval(secs => j.eta_secs::float8 * $1),
		             make_interval(secs => j.eta_secs::float8 + 120)))
		     OR (COALESCE(j.deadline_secs, 0) = 0 AND COALESCE(j.eta_secs, 0) = 0
		       AND now() > j.created_at + interval '24 hours')
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks t
		     WHERE t.job_id = j.id
		       AND (t.completed_at > now() - make_interval(secs => $2::float8)
		         OR t.claimed_at   > now() - make_interval(secs => $2::float8)
		         OR t.visible_at   > now() - make_interval(secs => $2::float8))
		   )
		 ORDER BY j.created_at ASC
		 LIMIT $3`,
		factor, grace.Seconds(), limit, deadAfter.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StuckJob
	for rows.Next() {
		var j StuckJob
		if err := rows.Scan(&j.ID, &j.BuyerID, &j.OutputRef, &j.EtaSecs, &j.TasksDone, &j.TaskCount,
			&j.Strikes, &j.DeadClaim); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RescueStuckJob is the watchdog's FIRST strike: instead of killing a stuck job it
// moves every unfinished task back to the queue for a different machine — the claim
// is cleared, a small visibility backoff applied (same mechanics as the stale
// requeue), and retry_count is deliberately NOT incremented (the stall is not the
// task's fault; burning its retries here would fast-track it to a terminal fail).
// The strike transition is guarded (status='running' AND watchdog_strikes=0), so
// concurrent sweeps rescue at most once: flipped=false means another sweep won the
// race (or the job progressed/went terminal) and NOTHING was touched.
func (s *Store) RescueStuckJob(ctx context.Context, jobID uuid.UUID, backoff time.Duration) (flipped bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx,
		`UPDATE jobs SET watchdog_strikes = 1
		 WHERE id = $1 AND status = 'running' AND COALESCE(watchdog_strikes, 0) = 0`, jobID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       started_at = NULL, worker_id = NULL,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE job_id = $1 AND status IN ('running','retrying')`,
		jobID, backoff.Seconds()); err != nil {
		return false, err
	}
	// Already-queued tasks with a STALE visible_at get it refreshed too. Without
	// this, a job whose unfinished work is all sitting unclaimed in the queue (e.g.
	// no capacity for its hw_class) gets a "rescue" that touches zero rows and no
	// fresh progress term — so the very next sweep would judge it stuck again and
	// kill it 30s after promising a second chance. The refresh makes the second
	// window real; if capacity never appears, the deadline clause catches it again
	// honestly at strike 1.
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET visible_at = now() + make_interval(secs => $2)
		 WHERE job_id = $1 AND status = 'queued'
		   AND visible_at < now() + make_interval(secs => $2)`,
		jobID, backoff.Seconds()); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// DeadClaim is a running task held by a worker that stopped heartbeating: the
// machine is gone (crash, sleep, network loss), so waiting for a commit is
// hopeless. Carries who to dock and where to requeue.
type DeadClaim struct {
	TaskID     uuid.UUID
	JobID      uuid.UUID
	WorkerID   uuid.UUID
	SupplierID uuid.UUID // zero when the worker row has no supplier (never docked)
	JobType    string
}

// DeadClaimedTasks finds running tasks whose claiming worker has been silent past
// olderThan (last_seen_at older than it, or never seen) AND whose claim itself is
// older than olderThan — the double condition so a task claimed a moment before a
// heartbeat lull is not misread as dead. These are the fast-rescue set: a dead
// machine is a certainty, so its tasks requeue immediately instead of waiting for
// the 30-min stale reaper or the job-level watchdog.
func (s *Store) DeadClaimedTasks(ctx context.Context, olderThan time.Duration, limit int) ([]DeadClaim, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id,t.job_id,t.execution_worker_id,t.execution_supplier_id,j.job_type
		 FROM tasks t
		 JOIN workers w ON w.id = t.claimed_by
		 JOIN jobs j ON j.id = t.job_id
		 WHERE t.status = 'running'
		   AND t.claimed_at IS NOT NULL
		   AND t.claimed_at < now() - make_interval(secs => $1)
		   AND (w.last_seen_at IS NULL OR w.last_seen_at < now() - make_interval(secs => $1))
		   AND t.execution_worker_id IS NOT NULL
		   AND t.execution_supplier_id IS NOT NULL
		   AND j.status = 'running'
		 ORDER BY t.claimed_at ASC
		 LIMIT $2`,
		olderThan.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeadClaim
	for rows.Next() {
		var d DeadClaim
		var sup *uuid.UUID
		if err := rows.Scan(&d.TaskID, &d.JobID, &d.WorkerID, &sup, &d.JobType); err != nil {
			return nil, err
		}
		if sup != nil {
			d.SupplierID = *sup
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RescueRunningTask requeues ONE running task with the rescue mechanics (claim
// cleared, small visibility backoff, retry_count NOT incremented — the worker died
// or wedged; the task did nothing wrong). Guarded by status='running' so a task
// that committed/failed between selection and here is untouched; rescued=false
// reports exactly that, so the caller only events/docks on a real rescue.
func (s *Store) RescueRunningTask(ctx context.Context, taskID uuid.UUID, backoff time.Duration) (rescued bool, err error) {
	ct, err := s.pool.Exec(ctx,
		`UPDATE tasks
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       started_at = NULL, worker_id = NULL,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'running'`,
		taskID, backoff.Seconds())
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// CancelledTaskResultKeys returns the result_key of every cancelled PRIMARY task
// of a job — the keys whose "<result_key>.partial" objects the watchdog's kill
// path checks for mid-chunk checkpoint documents. Honeypot/redundancy clones are
// excluded (verification probes, never buyer output).
func (s *Store) CancelledTaskResultKeys(ctx context.Context, jobID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT result_key FROM tasks
		 WHERE job_id = $1 AND status = 'cancelled'
		   AND is_honeypot = false AND is_redundancy = false
		   AND result_key IS NOT NULL AND result_key <> ''
		 ORDER BY chunk_index ASC, id ASC`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RecordEtaCalibration inserts one eta_calibration row for a finalized job —
// predicted_secs = the eta_secs persisted at submit, realized_secs = wall-clock
// seconds from created_at to now (the finalize moment). A job with no prediction
// (eta_secs NULL/0) inserts NOTHING (there is no predicted value to calibrate,
// and fabricating one would poison the loop), and a job already recorded is a
// no-op (both finalize sites can fire once each in a race). Returns the recorded
// pair — (0, 0) when nothing was inserted — so the caller can count near-misses.
func (s *Store) RecordEtaCalibration(ctx context.Context, jobID uuid.UUID) (predicted, realized int, err error) {
	// ON CONFLICT (job_id) DO NOTHING makes the once-only guarantee ATOMIC (the
	// eta_calibration_job_uniq index): when the two finalize sites race, exactly one
	// insert wins and returns the row; the loser scans ErrNoRows and records nothing
	// — so the near-miss counter can never double-count a job.
	err = s.pool.QueryRow(ctx,
		`INSERT INTO eta_calibration (job_id, job_type, tier, predicted_secs, realized_secs)
		 SELECT id, job_type, tier, eta_secs,
		        GREATEST(0, EXTRACT(EPOCH FROM (now() - created_at)))::int
		 FROM jobs
		 WHERE id = $1 AND COALESCE(eta_secs, 0) > 0
		 ON CONFLICT (job_id) DO NOTHING
		 RETURNING predicted_secs, realized_secs`,
		jobID).Scan(&predicted, &realized)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil // no ETA prediction (or already recorded) — nothing to calibrate
	}
	return predicted, realized, err
}

// CancelStuckJob flips a stuck job to 'cancelled' and cancels its unfinished tasks.
// Deliberately NOT the full-refund fail path: buyer charges settle per task at
// commit, so completed work stays charged (the supplier earned it — users owe each
// other) and the un-run remainder was never charged. flipped=false when the job
// went terminal (or progressed) between selection and here, in which case nothing
// is touched.
func (s *Store) CancelStuckJob(ctx context.Context, jobID uuid.UUID) (flipped bool, err error) {
	status, pending, err := s.jobTerminalTransitionState(ctx, jobID)
	if err != nil {
		return false, err
	}
	if status != "running" || pending {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockUnfinishedJobTasksTx(ctx, tx, jobID); err != nil {
		return false, err
	}
	status, pending, err = jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if status != "running" || pending {
		return false, nil
	}
	ct, err := tx.Exec(ctx,
		`UPDATE jobs SET status = 'cancelled' WHERE id = $1 AND status = 'running'`, jobID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'cancelled'
		 WHERE job_id = $1 AND status NOT IN ('complete','failed','cancelled')`, jobID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// CompletableJob is a job ready to finalize: all tasks done, status not yet
// terminal. Carries its buyer + output ref for the merge + webhook payload.
type CompletableJob struct {
	ID        uuid.UUID
	BuyerID   uuid.UUID
	OutputRef string
}

// FinalizableJobs returns jobs whose every task has finished (complete/failed,
// with at least one complete) but whose status is still running/verifying — the
// set the completion sweep should MERGE then finalize. Read-only: it does NOT
// flip status, so the sweep can write the merged artifact BEFORE marking the job
// complete (the buyer must never see status=complete with no merged output).
func (s *Store) FinalizableJobs(ctx context.Context, limit int) ([]CompletableJob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.buyer_id, COALESCE(j.output_ref,'')
		 FROM jobs j
		 WHERE j.status IN ('running','verifying')
		   AND j.task_count > 0
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks t
		     WHERE t.job_id = j.id AND t.status NOT IN ('complete','failed')
		   )
		   AND EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.id AND t.status = 'complete')
		 ORDER BY j.created_at ASC LIMIT $1`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompletableJob
	for rows.Next() {
		var c CompletableJob
		if err := rows.Scan(&c.ID, &c.BuyerID, &c.OutputRef); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkJobComplete flips one job to 'complete', only from a non-terminal state.
// Idempotent (a no-op once already complete). Called by the sweep AFTER the
// merged artifact is written.
func (s *Store) MarkJobComplete(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = 'complete'
		 WHERE id = $1 AND status IN ('running','verifying')`,
		jobID)
	return err
}

// CompleteJobEconomics publishes completion, records the once-only SLA premium,
// and projects actual_usd in one transaction. Keeping these together prevents a
// transient premium insert failure from leaving an already-complete job outside
// FinalizableJobs forever with revenue missing from its charge basis.
func (s *Store) CompleteJobEconomics(ctx context.Context, jobID uuid.UUID) error {
	return s.completeJobEconomics(ctx, jobID, nil)
}

// completeJobEconomics is the crash-testable production implementation. The
// public wrapper keeps ordinary callers probe-free; a subprocess integration
// test injects a probe to prove rollback before commit and idempotent replay
// after commit at every status/SLA/actual-cost edge.
func (s *Store) completeJobEconomics(
	ctx context.Context,
	jobID uuid.UUID,
	probe recoveryBoundaryProbe,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET status='complete'
		  WHERE id=$1 AND status IN ('running','verifying','complete')`, jobID); err != nil {
		return err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterJobProjection)
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (kind,buyer_id,amount_usd,payout_status,payout_ref)
		SELECT 'buyer_charge',j.buyer_id,-p.sla_premium_usd,'released',$2
		  FROM jobs j JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.id=$1 AND j.status='complete' AND p.sla_premium_usd > 0
		ON CONFLICT DO NOTHING`, jobID, slaPremiumChargeRef(jobID)); err != nil {
		return err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterSLAPremium)
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET actual_usd = COALESCE((
		   SELECT SUM(-amount_usd) FROM ledger_entries
		   WHERE kind='buyer_charge'
		     AND (task_id IN (SELECT id FROM tasks WHERE job_id=$1)
		          OR payout_ref=$2)
		 ),0)
		 WHERE id=$1`, jobID, slaPremiumChargeRef(jobID)); err != nil {
		return err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterActualUSD)
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteBeforeDBCommit)
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterDBCommit)
	return nil
}

// MarkResultsMerged stamps the results-merge watermark and the exact units of
// the buyer-ready artifact right after a real successful merge writes it.
// handleJobResults reads this to skip the re-merge on every poll once it is
// set (Data Transfer & Artifact I/O 4.5->5, "Stop paying for every poll
// twice") — it is set unconditionally (not COALESCE-guarded) so a later real
// merge (e.g. by the completion-sweep fallback) always advances it and refreshes
// the exact counters from the bytes that were actually written.
func (s *Store) MarkResultsMerged(ctx context.Context, jobID uuid.UUID, outputRecords, outputBytes int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs
		    SET results_merged_at = now(),
		        economic_output_records = $2,
		        economic_output_bytes = $3,
		        economic_output_source = $4
		  WHERE id = $1`,
		jobID, outputRecords, outputBytes, economicOutputSourceMergedArtifact)
	return err
}

func slaPremiumChargeRef(jobID uuid.UUID) string { return "sla-premium-" + jobID.String() }

// EnsureJobSLAPremiumCharge records the bound SLA premium exactly once, only for
// a successfully completed job. It is buyer revenue only: no task_id means it can
// never create supplier liability. A failed/partial job never receives the charge.
func (s *Store) EnsureJobSLAPremiumCharge(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ledger_entries (kind,buyer_id,amount_usd,payout_status,payout_ref)
		SELECT 'buyer_charge',j.buyer_id,-p.sla_premium_usd,'released',$2
		  FROM jobs j JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.id=$1 AND j.status='complete' AND p.sla_premium_usd > 0
		ON CONFLICT DO NOTHING`, jobID, slaPremiumChargeRef(jobID))
	return err
}

// SetJobActualUSD recomputes a job's actual_usd from the ledger (sum of buyer
// charges on its tasks) — the real settled cost, set when the job finalizes.
func (s *Store) SetJobActualUSD(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET actual_usd = COALESCE((
		   SELECT SUM(-amount_usd) FROM ledger_entries
		   WHERE kind = 'buyer_charge'
		     AND (task_id IN (SELECT id FROM tasks WHERE job_id = $1)
		          OR payout_ref = $2)
		 ),0)
		 WHERE id = $1`, jobID, slaPremiumChargeRef(jobID))
	return err
}

// JobResultKeys returns the exact per-chunk object keys selected for the buyer.
// A majority resolution overrides the provisional primary/hedge winner; a
// provisional resolution overrides the legacy task projection. Jobs predating
// durable verification resolution fall back explicitly to the first completed
// primary for that chunk.
func (s *Store) JobResultKeys(ctx context.Context, jobID uuid.UUID) ([]string, error) {
	info, err := s.JobMergeInputs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(info.Results))
	for i := range info.Results {
		out[i] = info.Results[i].ResultRef
	}
	return out, nil
}

// PrimaryResult is one completed primary task's result location, in input order.
type PrimaryResult struct {
	ChunkIndex int
	ResultRef  string
	// Artifact is non-nil only when an append-only chunk resolution selected a
	// terminal server-sealed verification artifact. Nil is an explicit legacy
	// result_ref fallback used for jobs created before this authority existed.
	Artifact *VerificationArtifact
}

// JobMergeInfo carries what MergeJobResults needs: the job's type + output key
// and the ordered list of its completed PRIMARY task result keys (honeypot and
// redundancy clones excluded — they verify, they are not the deliverable).
type JobMergeInfo struct {
	JobType   string
	OutputRef string
	Results   []PrimaryResult
}

// JobMergeInputs loads the job type, output ref, and the ordered completed
// primary-task results for the buyer-ready merge (MergeJobResults). Ordered by
// chunk_index so the merged artifact reads in the buyer's original input order.
func (s *Store) JobMergeInputs(ctx context.Context, jobID uuid.UUID) (*JobMergeInfo, error) {
	var info JobMergeInfo
	err := s.pool.QueryRow(ctx,
		`SELECT job_type, COALESCE(output_ref,'') FROM jobs WHERE id = $1`, jobID,
	).Scan(&info.JobType, &info.OutputRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	// One base result per chunk_index is the first completed primary/economic
	// hedge winner. Overlay the append-only authority for that chunk, preferring a
	// later majority fact over its provisional winner. The base task projection is
	// retained only as an explicit legacy fallback when no resolution exists.
	rows, err := s.pool.Query(ctx,
		`WITH primary_results AS (
		   SELECT DISTINCT ON (COALESCE(chunk_index,0))
		          COALESCE(chunk_index,0) AS chunk_index,result_ref
		     FROM tasks
		    WHERE job_id=$1 AND status='complete'
		      AND is_honeypot=false AND is_redundancy=false
		      AND result_ref IS NOT NULL AND result_ref<>''
		    ORDER BY COALESCE(chunk_index,0),completed_at ASC NULLS LAST,id ASC
		 ), selected_resolution AS (
		   SELECT DISTINCT ON (chunk_index)
		          chunk_index,artifact_key,artifact_sha256,artifact_bytes
		     FROM chunk_artifact_resolutions
		    WHERE job_id=$1
		    ORDER BY chunk_index,
		             CASE basis WHEN 'majority' THEN 0 ELSE 1 END,
		             created_at DESC,effect_id DESC
		 )
		 SELECT p.chunk_index,COALESCE(r.artifact_key,p.result_ref),
		        r.artifact_key,r.artifact_sha256,r.artifact_bytes
		   FROM primary_results p
		   LEFT JOIN selected_resolution r USING (chunk_index)
		  ORDER BY p.chunk_index`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pr PrimaryResult
		var artifactKey, artifactSHA *string
		var artifactBytes *int64
		if err := rows.Scan(&pr.ChunkIndex, &pr.ResultRef, &artifactKey, &artifactSHA, &artifactBytes); err != nil {
			return nil, err
		}
		if artifactKey != nil || artifactSHA != nil || artifactBytes != nil {
			if artifactKey == nil || artifactSHA == nil || artifactBytes == nil {
				return nil, fmt.Errorf("chunk %d has an incomplete resolved artifact tuple", pr.ChunkIndex)
			}
			pr.Artifact = &VerificationArtifact{Key: *artifactKey, SHA256: *artifactSHA, Bytes: *artifactBytes}
		}
		info.Results = append(info.Results, pr)
	}
	return &info, rows.Err()
}

// QueueDepthRow is one (tier, job_type) bucket of the claimable-task backlog.
type QueueDepthRow struct {
	Tier    string
	JobType string
	Count   int
}

// QueueDepth returns the claimable-task backlog grouped by job tier and job type,
// for the /metrics cx_queue_depth gauge. "Claimable" matches the claim's own
// predicate (queued/retrying, visible-now, unclaimed, non-terminal job).
func (s *Store) QueueDepth(ctx context.Context) ([]QueueDepthRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.tier, j.job_type, count(*)
		 FROM tasks t JOIN jobs j ON j.id = t.job_id
		 WHERE t.status IN ('queued','retrying')
		   AND t.claimed_by IS NULL
		   AND COALESCE(t.visible_at, t.created_at) <= now()
		   AND j.status NOT IN ('cancelled','failed','complete')
		 GROUP BY j.tier, j.job_type
		 ORDER BY j.tier, j.job_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueDepthRow
	for rows.Next() {
		var qd QueueDepthRow
		if err := rows.Scan(&qd.Tier, &qd.JobType, &qd.Count); err != nil {
			return nil, err
		}
		out = append(out, qd)
	}
	return out, rows.Err()
}

// taskDurationBucketsMs are the fixed histogram bucket upper bounds (milliseconds)
// for cx_task_duration_ms (docs/CREED_AND_PATH_TO_TEN.md, "Performance
// observability" 6→6.5). Chosen to span a real batch task's plausible range: a
// fast embed chunk (low hundreds of ms) through a slow generative chunk nearing
// the straggler-hedge threshold (hedgeAfter = 90s) and beyond.
var taskDurationBucketsMs = []float64{100, 500, 1000, 2500, 5000, 15000, 30000, 60000, 120000}

// TaskDurationHistogramRow is one job_type's histogram: Buckets holds the
// CUMULATIVE count of observations with duration_ms <= taskDurationBucketsMs[i]
// (the Prometheus histogram convention — le is "less than or equal", cumulative,
// not per-bin), alongside the total Count and SumMs Prometheus's _sum/_count lines
// need.
type TaskDurationHistogramRow struct {
	JobType string
	Buckets []int64 // cumulative, same order/length as taskDurationBucketsMs
	Count   int64
	SumMs   int64
}

// TaskDurationHistogram computes a real cx_task_duration_ms histogram per
// job_type straight from task_durations — the same table the drift/ETA rollup
// already reads, so this adds zero new instrumentation, only a new query over
// data that was already being recorded (docs/CREED_AND_PATH_TO_TEN.md,
// "Performance observability" 6→6.5: "zero latency histograms anywhere"). The
// per-bucket FILTER counts are computed in one aggregate query, not fetched row by
// row, so this scales with job_type cardinality, not with row count.
func (s *Store) TaskDurationHistogram(ctx context.Context) ([]TaskDurationHistogramRow, error) {
	// Build "count(*) FILTER (WHERE duration_ms <= $N)" once per bucket boundary,
	// parameterized (never string-interpolated) even though the boundaries are a
	// fixed Go slice, not user input — consistent with how every other query in
	// this file binds values.
	selectCols := make([]string, 0, len(taskDurationBucketsMs))
	args := make([]any, 0, len(taskDurationBucketsMs))
	for i, b := range taskDurationBucketsMs {
		args = append(args, b)
		selectCols = append(selectCols, fmt.Sprintf("count(*) FILTER (WHERE duration_ms <= $%d)", i+1))
	}
	query := fmt.Sprintf(
		`SELECT job_type, count(*), COALESCE(sum(duration_ms),0), %s
		 FROM task_durations
		 GROUP BY job_type
		 ORDER BY job_type`,
		strings.Join(selectCols, ", "),
	)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskDurationHistogramRow
	for rows.Next() {
		var r TaskDurationHistogramRow
		r.Buckets = make([]int64, len(taskDurationBucketsMs))
		scanArgs := make([]any, 0, 3+len(r.Buckets))
		scanArgs = append(scanArgs, &r.JobType, &r.Count, &r.SumMs)
		for i := range r.Buckets {
			scanArgs = append(scanArgs, &r.Buckets[i])
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatencyPhaseRow is one job_type's p50/p90 (milliseconds) for each of the three
// phases end-to-end task latency decomposes into. Backs cx_latency_phase_ms
// (End-to-End Job Latency Decomposition 7->7.5, docs/internal/CREED_AND_PATH_TO_TEN.md).
type LatencyPhaseRow struct {
	JobType               string
	QueueWaitP50Ms        float64
	QueueWaitP90Ms        float64
	DispatchOverheadP50Ms float64
	DispatchOverheadP90Ms float64
	RunP50Ms              float64
	RunP90Ms              float64
	Count                 int64
}

// LatencyPhaseDecomposition computes, per job_type, real p50/p90 millisecond
// figures for the three phases a completed task's end-to-end latency decomposes
// into: QUEUE-WAIT (submitted/eligible -> claimed — idle-fleet pickup cost),
// DISPATCH OVERHEAD (claimed -> started — time the worker spent between taking
// the claim and actually beginning work, e.g. a cold model load), and RUN
// (started -> completed — the actual work, including verification + result
// commit). Turns the existing created_at/visible_at/claimed_at/started_at/
// completed_at timestamps — already recorded on every task, no new
// instrumentation — into the first real decomposition of WHERE end-to-end
// latency goes, rather than just the single total task_duration_ms histogram.
// Only 'complete' tasks are included (a failed/retrying task's timestamps don't
// represent a real finished trip through all three phases).
func (s *Store) LatencyPhaseDecomposition(ctx context.Context) ([]LatencyPhaseRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.job_type, count(*),
		       COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY queue_wait_ms), 0),
		       COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY queue_wait_ms), 0),
		       COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY dispatch_overhead_ms), 0),
		       COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY dispatch_overhead_ms), 0),
		       COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY run_ms), 0),
		       COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY run_ms), 0)
		  FROM (
		    SELECT t.job_id,
		           EXTRACT(EPOCH FROM (t.claimed_at - GREATEST(t.created_at, t.visible_at))) * 1000 AS queue_wait_ms,
		           EXTRACT(EPOCH FROM (t.started_at - t.claimed_at)) * 1000 AS dispatch_overhead_ms,
		           EXTRACT(EPOCH FROM (t.completed_at - t.started_at)) * 1000 AS run_ms
		      FROM tasks t
		     WHERE t.status = 'complete'
		       AND t.claimed_at IS NOT NULL AND t.started_at IS NOT NULL AND t.completed_at IS NOT NULL
		  ) phases
		  JOIN jobs j ON j.id = phases.job_id
		 GROUP BY j.job_type
		 ORDER BY j.job_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LatencyPhaseRow
	for rows.Next() {
		var r LatencyPhaseRow
		if err := rows.Scan(&r.JobType, &r.Count,
			&r.QueueWaitP50Ms, &r.QueueWaitP90Ms,
			&r.DispatchOverheadP50Ms, &r.DispatchOverheadP90Ms,
			&r.RunP50Ms, &r.RunP90Ms); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveWorkerCount is the number of workers seen within the last 60s, for the
// /metrics active_workers gauge.
func (s *Store) ActiveWorkerCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM workers
		 WHERE last_seen_at IS NOT NULL AND last_seen_at > now() - interval '60 seconds'`,
	).Scan(&n)
	return n, err
}

// InvoiceView is the buyer-facing invoice for one job: the job header plus the
// realized ledger breakdown for its tasks. All money is computed from real rows;
// nothing is fabricated.
type InvoiceView struct {
	JobID           uuid.UUID `json:"job_id"`
	BuyerID         uuid.UUID `json:"buyer_id"`
	Status          string    `json:"status"`
	JobType         string    `json:"job_type"`
	CreatedAt       time.Time `json:"created_at"`
	EstimatedUSD    float64   `json:"estimated_usd"`
	ActualUSD       float64   `json:"actual_usd"`
	ChargedUSD      float64   `json:"charged_usd"`
	SupplierPaidUSD float64   `json:"supplier_credit_usd"`
	PlatformTakeUSD float64   `json:"platform_take_usd"`
	// QuotedUSD is the cost_expected_usd of the advisory quote this job was bound to
	// (Plane D D7), so the invoice shows quoted-vs-actual. omitempty + a pointer keeps
	// it off the wire for unbound jobs (a literal 0.0 would falsely read as "quoted $0").
	QuotedUSD *float64 `json:"quoted_usd,omitempty"`
	// FirmQuote / FirmQuoteMaxUSD / BilledUSD (Project Detection & Quotation 7->8,
	// docs/internal/CREED_AND_PATH_TO_TEN.md, "Ship a firm-quote tier: a real
	// commitment, not just an estimate"): when FirmQuote is true, BilledUSD is what
	// the buyer's Stripe charge was ACTUALLY capped at — the real proof artifact
	// that "actual cost exceeds its firm quote, still charges the buyer only the
	// quoted maximum". BilledUSD is nil until a charge is actually attempted
	// (FreezeChargeAmount/FormChargeBatch stamp it), same never-fabricate discipline
	// as QuotedUSD. ChargedUSD above stays the pre-existing per-task ledger sum
	// (the real value of work delivered) — BilledUSD is deliberately the SEPARATE,
	// possibly-lower number Stripe actually collected.
	FirmQuote       bool     `json:"firm_quote,omitempty"`
	FirmQuoteMaxUSD *float64 `json:"firm_quote_max_usd,omitempty"`
	BilledUSD       *float64 `json:"billed_usd,omitempty"`
	// Speed-SLA facts (wave 2A), surfaced on the invoice — and therefore on the
	// ClearingReceipt, which embeds this view. SLAPremiumUSD is the surcharge the
	// bound guarantee carried (folded into the job's estimate/actual);
	// SLARefundUSD is the real sla_refund ledger credit recorded on a miss (nil
	// until one exists — never fabricated); SLAMet is the recorded outcome (nil
	// = no SLA, or not yet decided). Same never-fabricate discipline as
	// QuotedUSD/BilledUSD above.
	SLAGuaranteeSecs int      `json:"sla_guarantee_secs,omitempty"`
	SLAPremiumUSD    *float64 `json:"sla_premium_usd,omitempty"`
	SLARefundUSD     *float64 `json:"sla_refund_usd,omitempty"`
	SLAMet           *bool    `json:"sla_met,omitempty"`
	// Routing* are the persisted SUBSTRATE-ROUTING decision (rubric dimension 5,
	// jobs.routing_*). Not on the invoice wire — they are read here only so the
	// clearing receipt handler (which already reads this view) can project the
	// "we ran it on X because Y" routing block without a second job read.
	// RoutingSubstrate == "" means the job carried no routing block (all four
	// columns NULL — a non-generative or empty-input job).
	RoutingSubstrate      string  `json:"-"`
	RoutingReason         string  `json:"-"`
	RoutingFleetETASecs   int     `json:"-"`
	RoutingGPUModeledSecs float64 `json:"-"`
}

// JobInvoice builds an invoice for a job scoped to its buyer (buyers see only
// their own jobs). It reads the job header and aggregates the realized ledger
// entries for the job's tasks by kind. Returns errNotFound when the job is not
// the buyer's.
func (s *Store) JobInvoice(ctx context.Context, jobID, buyerID uuid.UUID) (*InvoiceView, error) {
	iv := InvoiceView{JobID: jobID}
	var firmMax, billed *float64
	var slaGuarantee int
	var slaPremium *float64
	// Routing decision (rubric dimension 5): NULL columns (a job with no routing
	// block) scan into these nullables, then default to the zero substrate ""
	// below so the receipt cleanly omits the block — never a fabricated fleet/0s.
	var rSubstrate, rReason *string
	var rFleetETA *int
	var rGPUModeled *float64
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id, status, job_type, created_at,
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0),
		        firm_quote, firm_quote_max_usd, billed_usd,
		        COALESCE(sla_guarantee_secs,0), sla_premium_usd, sla_met,
		        routing_substrate, routing_reason, routing_fleet_eta_secs, routing_gpu_modeled_secs
		 FROM jobs WHERE id = $1 AND buyer_id = $2`,
		jobID, buyerID,
	).Scan(&iv.BuyerID, &iv.Status, &iv.JobType, &iv.CreatedAt, &iv.EstimatedUSD, &iv.ActualUSD,
		&iv.FirmQuote, &firmMax, &billed, &slaGuarantee, &slaPremium, &iv.SLAMet,
		&rSubstrate, &rReason, &rFleetETA, &rGPUModeled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	iv.FirmQuoteMaxUSD = firmMax
	iv.BilledUSD = billed
	iv.SLAGuaranteeSecs = slaGuarantee
	iv.SLAPremiumUSD = slaPremium
	if rSubstrate != nil {
		iv.RoutingSubstrate = *rSubstrate
		if rReason != nil {
			iv.RoutingReason = *rReason
		}
		if rFleetETA != nil {
			iv.RoutingFleetETASecs = *rFleetETA
		}
		if rGPUModeled != nil {
			iv.RoutingGPUModeledSecs = *rGPUModeled
		}
	}
	// Surface the REAL recorded refund (the sla_refund ledger credit keyed
	// 'sla-<job_id>'), only when one exists — the invoice shows what actually
	// happened, never a predicted remedy.
	if slaGuarantee > 0 {
		var refund float64
		if rerr := s.pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount_usd),0)::float8 FROM ledger_entries
			  WHERE kind = 'sla_refund' AND payout_ref = $1`,
			"sla-"+jobID.String()).Scan(&refund); rerr != nil {
			return nil, rerr
		}
		if refund > 0 {
			iv.SLARefundUSD = &refund
		}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT le.kind, COALESCE(SUM(le.amount_usd),0)
		 FROM ledger_entries le JOIN tasks t ON t.id = le.task_id
		 WHERE t.job_id = $1 GROUP BY le.kind`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var amt float64
		if err := rows.Scan(&kind, &amt); err != nil {
			return nil, err
		}
		switch kind {
		case "supplier_credit", "clawback":
			iv.SupplierPaidUSD += amt // clawback is negative → reduces net paid
		case "platform_take":
			iv.PlatformTakeUSD += amt
		case "buyer_charge":
			iv.ChargedUSD += amt
		}
	}
	// No explicit buyer_charge rows → the charge is the job's actual (else
	// estimated) cost. Never fabricate; fall back to what the job already knows.
	if iv.ChargedUSD == 0 {
		if iv.ActualUSD > 0 {
			iv.ChargedUSD = iv.ActualUSD
		} else {
			iv.ChargedUSD = iv.EstimatedUSD
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// If the job was bound to a quote (Plane D D7), surface what the buyer was quoted
	// alongside what they were charged. Absent binding leaves QuotedUSD nil (omitted).
	if quoted, ok, qerr := s.QuotedUSDForJob(ctx, jobID); qerr != nil {
		return nil, qerr
	} else if ok {
		iv.QuotedUSD = &quoted
	}
	return &iv, nil
}

// SupplierStripeAcct returns a supplier's connected Stripe account id (empty when
// unset). Used by StripePayout to target a real transfer. errNotFound when the
// supplier row is missing.
func (s *Store) SupplierStripeAcct(ctx context.Context, supplierID uuid.UUID) (string, error) {
	var acct *string
	err := s.pool.QueryRow(ctx, `SELECT stripe_acct FROM suppliers WHERE id = $1`, supplierID).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	if acct == nil {
		return "", nil
	}
	return *acct, nil
}
