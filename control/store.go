package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
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

type Store struct {
	pool                  *pgxpool.Pool
	verificationResources *verificationResourceBudget
}

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

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const schemaMigrationAdvisoryLock = "computeexchange-control-schema-v1"

//go:embed schema.sql
var canonicalSchema string

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

func (s *Store) Migrate(ctx context.Context) error {
	conn, release, err := s.acquireSchemaMigrationLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = conn.Conn().PgConn().Exec(ctx, canonicalSchema).ReadAll()
	if err != nil {
		return fmt.Errorf("apply canonical schema: %w", err)
	}
	if err := syncRuntimeCatalog(ctx, conn); err != nil {
		return err
	}
	release()
	if _, err := s.ReconcileLegacyVerifyingTasks(ctx); err != nil {
		return fmt.Errorf("reconcile legacy verification: %w", err)
	}
	return nil
}

var (
	errNotFound          = errors.New("not found")
	errJobNotCancellable = errors.New("job is no longer cancellable")
)
var errOAuthLinkStateInvalid = errors.New("invalid or expired OAuth link state")

const (
	maxOAuthLinkCapabilityBytes = 256
	maxOAuthLinkStateLifetime   = 15 * time.Minute
)

func (s *Store) GetBillingCustomer(ctx context.Context, buyerID uuid.UUID) (custID, pm string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(stripe_customer_id,''), COALESCE(default_payment_method,'')
		   FROM billing_customers WHERE buyer_id=$1`, buyerID).Scan(&custID, &pm)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errNotFound
	}
	return custID, pm, err
}

func (s *Store) UpsertBillingCustomer(ctx context.Context, buyerID uuid.UUID, custID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO billing_customers (buyer_id, stripe_customer_id) VALUES ($1, $2)
		   ON CONFLICT (buyer_id) DO UPDATE SET stripe_customer_id = EXCLUDED.stripe_customer_id`,
		buyerID, custID)
	return err
}

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
		return fmt.Errorf("billing customer mapping matched %d rows, want exactly one", rows)
	}
}

func (s *Store) JobChargeInfo(ctx context.Context, jobID uuid.UUID) (buyerID uuid.UUID, chargeUSD float64, err error) {
	var actualUSD float64
	var firmQuote bool
	var firmMax float64
	var slaRefund float64
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

func (s *Store) SetSupplierStripeAcct(ctx context.Context, supplierID uuid.UUID, acct string) error {
	_, err := s.pool.Exec(ctx, `UPDATE suppliers SET stripe_acct=$2 WHERE id=$1`, supplierID, acct)
	return err
}

const (
	intakeStageLockNamespace   uint32 = 0x4358494e // "CXIN"
	pipelineStageLockNamespace uint32 = 0x4358504c // "CXPL"
)

func workflowStageLockKeys(namespace uint32, workflowID uuid.UUID, stageIndex int) (int32, int32) {
	left := binary.BigEndian.Uint32(workflowID[0:4]) ^
		binary.BigEndian.Uint32(workflowID[4:8]) ^ namespace
	right := binary.BigEndian.Uint32(workflowID[8:12]) ^
		binary.BigEndian.Uint32(workflowID[12:16]) ^ uint32(stageIndex)
	return int32(left), int32(right)
}

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
				raw := conn.Hijack()
				_ = raw.Close(releaseCtx)
				return
			}
			conn.Release()
		})
	}, nil
}

func (s *Store) JobOutputRef(ctx context.Context, jobID uuid.UUID) (string, error) {
	var ref string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(output_ref,'') FROM jobs WHERE id=$1`, jobID).Scan(&ref)
	return ref, err
}

func (s *Store) JobBuyerID(ctx context.Context, jobID uuid.UUID) (uuid.UUID, error) {
	var buyerID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT buyer_id FROM jobs WHERE id=$1`, jobID).Scan(&buyerID)
	return buyerID, err
}

func nullStrSlice(xs []string) any {
	if len(xs) == 0 {
		return nil
	}
	return xs
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullPosFloat(v float64) any {
	if v <= 0 {
		return nil
	}
	return v
}

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

func nullUUID(id uuid.UUID) any {
	if id == (uuid.UUID{}) {
		return nil
	}
	return id
}

func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

type AuthResult struct {
	BuyerID     uuid.UUID
	IsAdmin     bool
	APIKeyID    uuid.UUID
	APIKeyLabel string
}

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

type WorkerAuth struct {
	WorkerID              uuid.UUID
	SupplierID            uuid.UUID
	CredentialID          uuid.UUID
	DeviceFingerprint     string
	CredentialVersion     int
	EnrollmentDeviceBound bool
}

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

type APIKeyRow struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Masked    string    `json:"masked"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

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

func maskKey(raw string) string {
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

	thermalOK := true
	for _, b := range cap.Benchmarks {
		thermalOK = thermalOK && b.ThermalOK
	}

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

type WorkerResources struct {
	AvailableMemoryGB  float32
	EffectiveMemoryGB  float32
	ReservedHeadroomGB float32
	Throttled          bool
	LoadedModels       []string
	ActiveTasks        []TaskLease
}

func (s *Store) HeartbeatTx(ctx context.Context, workerID uuid.UUID, r WorkerResources) error {
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
	if effective != nil || available != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO worker_memory_samples (worker_id, available_gb, effective_gb, throttled)
			 VALUES ($1, $2, $3, $4)`,
			workerID, available, effective, r.Throttled); err != nil {
			return err // a failed sample is a real failure, not silently swallowed (BLACKHOLE)
		}
	}
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
	for _, lease := range r.ActiveTasks {
		// Only the authenticated worker's current execution epoch can renew a
		// lease. A delayed heartbeat from an older attempt is therefore inert.
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET claimed_at = now()
			 WHERE id = $1 AND claimed_by = $2 AND worker_id = $2
			   AND status = 'running' AND retry_count = $3`,
			lease.TaskID, workerID, lease.Attempt); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

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

const memSampleWindow = 20

func (s *Store) DeleteOldWorkerMemorySamples(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM worker_memory_samples WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) DeleteOldTaskDurations(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM task_durations WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) DeleteOldJobEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM job_events WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

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

func (s *Store) ReinstateWorker(ctx context.Context, workerID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE suppliers SET status = 'active', quarantined_at = NULL
		 WHERE id = (SELECT supplier_id FROM workers WHERE id = $1) AND status = 'suspended'`,
		workerID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
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

var errNotSuspended = errors.New("supplier is not suspended")

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
		                  worker_id = NULL, retry_count = retry_count + 1, visible_at = now()
		 WHERE id = $1 AND status IN ('running','retrying')
		 RETURNING job_id, status`,
		taskID,
	).Scan(&jobID, &prevStatus); errors.Is(err, pgx.ErrNoRows) {
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

var errNotRequeueable = errors.New("task is not in a requeueable state")

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

func (s *Store) ReleasePayoutTx(ctx context.Context, entryID uuid.UUID, reason string) error {
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

var errNotHeld = errors.New("ledger entry is not held or ready")

func (s *Store) SubmitJobTx(ctx context.Context, j *jobRow, tasks []taskRow) error {
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
		    offered_rate_usd_hr, eta_secs, max_usd, budget_state, quote_id, min_reputation,
		    deadline_secs, firm_quote, firm_quote_max_usd, sla_guarantee_secs, sla_premium_usd,
		    economic_input_records, economic_input_bytes, economic_input_source,
		    submit_idempotency_key, submit_request_sha256)
		 VALUES ($1,$2,'queued',$3,$4,$5,$6,$7,$8,$9,0,$10,0,
		         $11,$12,$13,$14,$15,$16,$17,$18,$19,'tracking',$20,$21,$22,$23,$24,$25,$26,
		         $27,$28,$29,NULLIF($30,''),NULLIF($31,''))`,
		j.ID, j.BuyerID, j.JobType, j.ModelRef, j.InputRef, j.OutputRef,
		j.Tier, j.VerificationPolicy, j.EstimatedUSD, j.TaskCount,
		j.MinMemoryGB, j.MaxDurationSecs, nullStrSlice(j.HWClasses), nullStrSlice(j.DataResidency),
		nullJSON(j.JobTypeSpec), j.SplitSize, j.OfferedRateUsdHr, j.ETASecs,
		nullPosFloat(j.MaxUSD), nullUUID(j.QuoteID), j.MinReputation,
		j.DeadlineSecs, j.FirmQuote, nullPosFloat(j.FirmQuoteMaxUSD),
		nullPosInt(j.SLAGuaranteeSecs), nullPosFloat(j.SLAPremiumUSD),
		economicInputRecords, economicInputBytes, economicInputSource,
		j.SubmitIdempotencyKey, j.SubmitRequestSHA256,
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

type jobRow struct {
	ID                         uuid.UUID
	BuyerID                    uuid.UUID
	JobType                    string
	ModelRef                   string
	InputRef                   string
	OutputRef                  string
	Tier                       string
	VerificationPolicy         []byte // jsonb
	EstimatedUSD               float64
	TaskCount                  int
	MinMemoryGB                float32
	MaxDurationSecs            uint32
	HWClasses                  []string // nil = any class
	DataResidency              []string // nil = unrestricted
	JobTypeSpec                []byte   // jsonb: the full submitted JobType (tag + fields)
	SplitSize                  int
	OfferedRateUsdHr           float32
	ETASecs                    int
	MaxUSD                     float64   // buyer hard spend cap (Budget Governor); 0 = no cap
	QuoteID                    uuid.UUID // advisory quote bound to this job (Plane D D7); zero = none -> persisted NULL
	MinReputation              float32   // Elite-supplier gate: claim only by suppliers with reputation >= this (0 = any)
	DeadlineSecs               int       // watchdog policy: -1 opt out, 0 default, 60..604800 explicit wall-clock deadline
	FirmQuote                  bool
	FirmQuoteMaxUSD            float64
	SLAGuaranteeSecs           int
	SLAPremiumUSD              float64
	EconomicInputRecords       int64
	EconomicInputBytes         int64
	EconomicInputSource        string
	EconomicPlan               EconomicPlan
	WebhookID                  uuid.UUID
	WebhookURL                 string
	WebhookSigningSecretSealed string
	SubmitIdempotencyKey       string
	SubmitRequestSHA256        string
}

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

type JobView struct {
	ID               uuid.UUID
	BuyerID          uuid.UUID
	Status           string
	JobType          string
	Tier             string
	OutputRef        string
	TaskCount        int
	TasksDone        int
	EstimatedUSD     float64
	ActualUSD        float64
	ETASecs          int
	CreatedAt        time.Time
	MaxUSD           float64
	BudgetState      string
	ChargeStatus     string
	Verification     Verification
	ResultsMergedAt  *time.Time
	SLAGuaranteeSecs int
	SLAPremiumUSD    float64
	SLAMet           *bool
}

var errIdempotencyConflict = errors.New("idempotency key was already used with a different request")

func (s *Store) JobSubmissionReplay(
	ctx context.Context,
	buyerID uuid.UUID,
	idempotencyKey, requestSHA256 string,
) (JobSubmitResponse, bool, error) {
	var out JobSubmitResponse
	var createdAt time.Time
	var tier, storedSHA, webhookID, webhookSecretSealed string
	err := s.pool.QueryRow(ctx, `
		SELECT j.id,COALESCE(j.task_count,0),COALESCE(j.estimated_usd,0)::float8,
		       COALESCE(j.eta_secs,0),COALESCE(j.tier,'batch'),j.created_at,
		       COALESCE(w.id::text,''),COALESCE(w.signing_secret_sealed,''),
		       COALESCE(j.submit_request_sha256,'')
		  FROM jobs j
		  LEFT JOIN LATERAL (
		    SELECT id,signing_secret_sealed FROM webhooks
		     WHERE job_id=j.id ORDER BY created_at,id LIMIT 1
		  ) w ON true
		 WHERE j.buyer_id=$1 AND j.submit_idempotency_key=$2`,
		buyerID, idempotencyKey,
	).Scan(&out.JobID, &out.TaskCount, &out.EstimatedUSD, &out.ETASecs, &tier,
		&createdAt, &webhookID, &webhookSecretSealed, &storedSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobSubmitResponse{}, false, nil
	}
	if err != nil {
		return JobSubmitResponse{}, false, err
	}
	if storedSHA != requestSHA256 {
		return JobSubmitResponse{}, true, errIdempotencyConflict
	}
	dur := time.Duration(out.ETASecs) * time.Second
	if min := tierMinCompletion(tier); dur < min {
		dur = min
	}
	out.EstimatedCompletion = createdAt.Add(dur).UTC().Format(time.RFC3339)
	out.TierSemantics = serviceTierSemantics(tier)
	out.WebhookID = webhookID
	if webhookSecretSealed != "" {
		secret, err := openWebhookSigningSecret(webhookSecretSealed)
		if err != nil {
			return JobSubmitResponse{}, true, fmt.Errorf("open replay webhook secret: %w", err)
		}
		out.WebhookSecret = secret
	}
	return out, true, nil
}

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
	vr, verr := s.JobVerification(ctx, j.ID)
	if verr != nil {
		log.Printf("job verification aggregate (job %s): %v", j.ID, verr)
		vr.Label = deriveVerificationLabel(vr)
	}
	j.Verification = vr
	return &j, nil
}

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

type jobInternal struct {
	BuyerID            uuid.UUID
	TaskCount          int
	EstimatedUSD       float64
	VerificationPolicy []byte
}

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

func (s *Store) CancelJob(ctx context.Context, jobID, buyerID uuid.UUID) error {
	// Scope the very first lookup to the authenticated buyer. Besides avoiding
	// object-existence disclosure, this prevents one buyer from forcing locks on
	// another buyer's task rows.
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id=$1 AND buyer_id=$2`, jobID, buyerID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if status == "cancelled" {
		return nil // DELETE is naturally idempotent for the owning buyer.
	}
	if status != "queued" {
		return errJobNotCancellable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockUnfinishedJobTasksTx(ctx, tx, jobID); err != nil {
		return err
	}
	status, pending, err := jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return err
	}
	var owns bool
	if err := tx.QueryRow(ctx, `SELECT buyer_id=$2 FROM jobs WHERE id=$1`, jobID, buyerID).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return errNotFound
	}
	if status == "cancelled" {
		return tx.Commit(ctx)
	}
	if status != "queued" || pending {
		return errJobNotCancellable
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'failed'
		 WHERE job_id = $1 AND status = 'queued'`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) StartTask(ctx context.Context, taskID, workerID uuid.UUID, claimAttempt int16) error {
	if claimAttempt < 0 {
		return errNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

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
		  FROM tasks WHERE id=$1 AND retry_count=$2 FOR UPDATE`, taskID, claimAttempt).
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
		 WHERE id=$1 AND claimed_by=$2 AND retry_count=$7 AND status='queued'`,
		taskID, workerID, claimSupplierID, claimHWClass, claimEngine, claimBuildHash, claimAttempt)
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

type CommitTaskInfo struct {
	TaskID                   uuid.UUID
	JobID                    uuid.UUID
	WorkerID                 uuid.UUID
	SupplierID               uuid.UUID
	IsHoneypot               bool
	IsRedundancy             bool
	HWClass                  string
	engine                   string
	buildHash                string
	jobType                  string // parent job's job_type, for honeypot answer lookup
	jobMaxTokens             uint32 // bounded projection of jobs.job_type_spec.max_tokens
	resultMaxBytes           int64
	InputRef                 string // this task's input chunk key (honeypot answer lookup)
	ResultKey                string // canonical server-side result key (verification fetch)
	ModelRef                 string // parent job's model_ref (tiebreak peer selection)
	MinMemoryGB              float32
	ChunkIndex               int // this task's chunk position (tiebreak pairing + N-way vote)
	SplitSize                int
	ExpectedOutputRecords    int64
	Attempt                  int16
	DurationMS               uint64
	TokensUsed               uint64
	ResultSHA256             string
	hardwareTempC            *float32
	verificationCheckSampled *bool
	peerSupplierID           uuid.UUID
	peerEngine               string
	peerBuildHash            string
}

func (s *Store) CompleteTaskTx(ctx context.Context, taskID, workerID uuid.UUID, c TaskCommit) (*CommitTaskInfo, error) {
	return s.completeTaskTx(ctx, taskID, workerID, c, nil)
}

func (s *Store) completeTaskTx(ctx context.Context, taskID, workerID uuid.UUID, c TaskCommit, probe recoveryBoundaryProbe) (*CommitTaskInfo, error) {
	if c.DurationMS > math.MaxInt64 || c.TokensUsed > math.MaxInt64 {
		return nil, fmt.Errorf("reported duration/tokens exceed durable range")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

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
		 WHERE id = $1 AND claimed_by = $2 AND execution_worker_id = $2 AND retry_count = $8
		   AND (status IN ('running','queued') OR (status = 'verifying' AND worker_id = $2))`,
		taskID, workerID, c.ResultKey, resultSHA256, int64(c.DurationMS), int64(c.TokensUsed), c.HardwareTempC, c.Attempt)
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

const driftMinSamples = 5

const driftWindow = 24 * time.Hour

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
		return 0, samples, nil // too thin  -  caller falls back to the static target
	}
	return p90ms, samples, nil
}

type DriftRow struct {
	JobType          string  `json:"job_type"`
	ModelRef         string  `json:"model_ref"`
	Samples          int     `json:"samples"`             // committed-task durations recorded IN THE WINDOW (see WindowHours)
	AvgDurationMs    float64 `json:"avg_duration_ms"`     // mean actual per-task wall-time, windowed
	P90DurationMs    int64   `json:"p90_duration_ms"`     // observed p90 (what the ETA learns from), windowed
	AvgQuotedETASecs float64 `json:"avg_quoted_eta_secs"` // mean quoted whole-job eta_secs (reused jobs.eta_secs), windowed
	UsingObservedP90 bool    `json:"using_observed_p90"`  // true once samples >= the trust floor
	WindowHours      float64 `json:"window_hours"`        // the rolling window every figure above is bounded to (driftWindow)  -  named explicitly so a reader never mistakes this for all-time history
}

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

const quarantineRepFloor = 0.2

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
		_, err = tx.Exec(ctx,
			`UPDATE suppliers SET reputation = 0.0, status = 'banned' WHERE id = $1`,
			supplierID)
	case next < quarantineRepFloor:
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

func (s *Store) DockReputationMild(ctx context.Context, supplierID uuid.UUID, event ReputationEvent) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE suppliers
		   SET reputation = GREATEST(0.0, LEAST(1.0, reputation + $2))
		 WHERE id = $1`,
		supplierID, reputationDelta(event))
	return err
}

const (
	requeueBackoffBase    = 30 * time.Second // first requeue delay; doubles per prior retry
	requeueBackoffCap     = 10 * time.Minute // ceiling so a high retry_count can't push a task far out
	requeueExclusionGrace = 2 * time.Minute
)

func requeueBackoff(priorRetries int) time.Duration {
	if priorRetries < 0 {
		priorRetries = 0
	}
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

func (s *Store) RequeueTask(ctx context.Context, taskID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

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

type ChunkResult struct {
	TaskID     uuid.UUID
	WorkerID   uuid.UUID
	SupplierID uuid.UUID
	ResultRef  string
	Artifact   *VerificationArtifact
	Engine     string
	BuildHash  string
}

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

func (s *Store) InsertTiebreakTask(ctx context.Context, jobID, primaryTaskID, peerWorker uuid.UUID, inputRef string, chunkIndex int) (uuid.UUID, error) {
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
	default:
		return e, err
	}
	return e, nil
}

type FraudFlag struct {
	SupplierID uuid.UUID `json:"supplier_id"`
	Reputation float32   `json:"reputation"`
	Tier       int16     `json:"tier"`
	Status     string    `json:"status"`
}

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

type FraudReport struct {
	SupplierID    uuid.UUID  `json:"supplier_id"`
	Reputation    float32    `json:"reputation"`
	Tier          int16      `json:"tier"`
	Status        string     `json:"status"`
	QuarantinedAt *time.Time `json:"quarantined_at"`
	Clawbacks     int        `json:"clawbacks"`      // confirmed-fraud clawback rows
	MismatchTasks int        `json:"mismatch_tasks"` // tasks this supplier's clawbacks span
}

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

type ModelRow struct {
	ID          string
	Family      string
	Quant       string
	Kind        string
	Dim         int
	JobType     string
	PricePer1K  float64
	MinMemoryGB float32
	HFRepo      string
}

func (s *Store) ListModels(ctx context.Context) ([]ModelRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, COALESCE(family,''), COALESCE(quant,''), COALESCE(kind,''),
		        COALESCE(dim,0), COALESCE(job_type,''),
		        COALESCE(price_per_1k,0), COALESCE(min_memory_gb,0), COALESCE(hf_repo,'')
		 FROM models ORDER BY price_per_1k ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelRow
	for rows.Next() {
		var m ModelRow
		if err := rows.Scan(&m.ID, &m.Family, &m.Quant, &m.Kind, &m.Dim, &m.JobType,
			&m.PricePer1K, &m.MinMemoryGB, &m.HFRepo); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetModel(ctx context.Context, id string) (*ModelRow, error) {
	var m ModelRow
	err := s.pool.QueryRow(ctx,
		`SELECT id, COALESCE(family,''), COALESCE(quant,''), COALESCE(kind,''),
		        COALESCE(dim,0), COALESCE(job_type,''),
		        COALESCE(price_per_1k,0), COALESCE(min_memory_gb,0), COALESCE(hf_repo,'')
		 FROM models WHERE id = $1`, id,
	).Scan(&m.ID, &m.Family, &m.Quant, &m.Kind, &m.Dim, &m.JobType,
		&m.PricePer1K, &m.MinMemoryGB, &m.HFRepo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

var (
	errWebhookLeaseLost   = errors.New("webhook delivery lease lost")
	errWebhookJobRequired = errors.New("webhook job id is required")
	errWebhookLimit       = errors.New("webhook registration limit reached for job")
)

const webhookRegistrationLimitPerJob = 32

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

type PendingWebhook struct {
	ID                  uuid.UUID
	JobID               uuid.UUID
	URL                 string
	Status              string
	SigningSecretSealed string
	LeaseToken          uuid.UUID
	Attempts            int
}

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

func (s *Store) DeliverWebhookTx(ctx context.Context, id, leaseToken uuid.UUID) error {
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
		if existingState == "compromised" {
			return existingID, false, nil
		}
		return existingID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	if taskID == nil {
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
	out.AmountUSD = float64(out.RequestedCents) / 100
	fundingID, funded, err := reservePayoutFunding(
		ctx, tx, entryID, taskID, out.RequestedCents, out.Currency)
	if err != nil {
		return out, false, err
	}
	if !funded {
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

func (s *Store) SetChargeStatus(ctx context.Context, jobID uuid.UUID, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE jobs SET charge_status = $2 WHERE id = $1`, jobID, status)
	return err
}

func (s *Store) RecordVerificationEvent(ctx context.Context, jobID, taskID, supplierID uuid.UUID, kind string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO verification_events (job_id, task_id, supplier_id, kind)
		 VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		jobID, nullUUID(taskID), nullUUID(supplierID), kind)
	return err
}

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
			v.SameSupplier += n
		case "redundancy_cross_class", "tiebreak_cross_class":
			v.CrossClassSkipped += n
		}
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
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

type DisputeRow struct {
	ID, JobID uuid.UUID
	Status    string
}

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

type ReverifyTargetRow struct {
	TaskID, AnchorWorker uuid.UUID
	JobType, ModelRef    string
	InputRef             string
	MinMemGB             float32
	ChunkIndex           int
}

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

func (s *Store) SetDisputeReverifying(ctx context.Context, id, reverifyTaskID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE disputes SET status = 'reverifying', reverify_task_id = $2 WHERE id = $1`,
		id, reverifyTaskID)
	return err
}

func (s *Store) SetDisputeStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE disputes SET status = $2,
		        resolved_at = CASE WHEN $2 IN ('resolved','rejected') THEN now() ELSE resolved_at END
		  WHERE id = $1`, id, status)
	return err
}

func (s *Store) JobHasPendingTasks(ctx context.Context, jobID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE job_id = $1 AND status IN ('queued','retrying','running')`,
		jobID).Scan(&n)
	return n > 0, err
}

func (s *Store) TaskHasClawback(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE kind = 'clawback' AND task_id = $1)`,
		taskID).Scan(&exists)
	return exists, err
}

func (s *Store) MarkPayout(ctx context.Context, entryID uuid.UUID, status, ref string) error {
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

type StaleTask struct {
	ID         uuid.UUID
	JobID      uuid.UUID
	RetryCount int16
}

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

type Straggler struct {
	TaskID         uuid.UUID
	JobID          uuid.UUID
	WorkerID       uuid.UUID
	JobType        string
	ModelRef       string
	InputRef       string
	ChunkIndex     int
	MinMemGB       float32
	ThrottledHedge bool
}

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
		s.ThrottledHedge = throttled
		out = append(out, s)
	}
	return out, rows.Err()
}

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

func (s *Store) JobCheckpointInfo(ctx context.Context, jobID uuid.UUID) (outputRef string, tasksDone int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(output_ref,''), COALESCE(tasks_done,0) FROM jobs WHERE id = $1`,
		jobID).Scan(&outputRef, &tasksDone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, errNotFound
	}
	return outputRef, tasksDone, err
}

func (s *Store) TaskJobID(ctx context.Context, taskID uuid.UUID) (uuid.UUID, error) {
	var jobID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT job_id FROM tasks WHERE id = $1`, taskID).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errNotFound
	}
	return jobID, err
}

func failJobAndSettleOnce(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (flipped bool, err error) {
	status, pending, err := jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if status == "complete" || status == "cancelled" || status == "failed" {
		return false, nil // already terminal  -  nothing to settle again
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

type StuckJob struct {
	ID        uuid.UUID
	BuyerID   uuid.UUID
	OutputRef string
	EtaSecs   int
	TasksDone int
	TaskCount int
	Strikes   int  // watchdog_strikes: 0 -> rescue next, >=1 -> kill next
	DeadClaim bool // an unfinished task is claimed by a DEAD worker (machine-stuck, not workload-stuck)
}

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
		       started_at = NULL, worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE job_id = $1 AND status IN ('running','retrying')`,
		jobID, backoff.Seconds()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET visible_at = now() + make_interval(secs => $2)
		 WHERE job_id = $1 AND status = 'queued'
		   AND visible_at < now() + make_interval(secs => $2)`,
		jobID, backoff.Seconds()); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

type DeadClaim struct {
	TaskID     uuid.UUID
	JobID      uuid.UUID
	WorkerID   uuid.UUID
	SupplierID uuid.UUID // zero when the worker row has no supplier (never docked)
	JobType    string
}

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

func (s *Store) RescueRunningTask(ctx context.Context, taskID uuid.UUID, backoff time.Duration) (rescued bool, err error) {
	ct, err := s.pool.Exec(ctx,
		`UPDATE tasks
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       started_at = NULL, worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'running'`,
		taskID, backoff.Seconds())
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

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

func (s *Store) RecordEtaCalibration(ctx context.Context, jobID uuid.UUID) (predicted, realized int, err error) {
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
		return 0, 0, nil // no ETA prediction (or already recorded)  -  nothing to calibrate
	}
	return predicted, realized, err
}

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

type CompletableJob struct {
	ID        uuid.UUID
	BuyerID   uuid.UUID
	OutputRef string
}

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

func (s *Store) MarkJobComplete(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = 'complete'
		 WHERE id = $1 AND status IN ('running','verifying')`,
		jobID)
	return err
}

func (s *Store) FinalizeJobTx(ctx context.Context, jobID uuid.UUID) error {
	return s.completeJobEconomics(ctx, jobID, nil)
}

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

func (s *Store) EnsureJobSLAPremiumCharge(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ledger_entries (kind,buyer_id,amount_usd,payout_status,payout_ref)
		SELECT 'buyer_charge',j.buyer_id,-p.sla_premium_usd,'released',$2
		  FROM jobs j JOIN job_economic_plans p ON p.job_id=j.id
		 WHERE j.id=$1 AND j.status='complete' AND p.sla_premium_usd > 0
		ON CONFLICT DO NOTHING`, jobID, slaPremiumChargeRef(jobID))
	return err
}

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

type PrimaryResult struct {
	ChunkIndex int
	ResultRef  string
	Artifact   *VerificationArtifact
}

type JobMergeInfo struct {
	JobType   string
	OutputRef string
	Results   []PrimaryResult
}

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

type QueueDepthRow struct {
	Tier    string
	JobType string
	Count   int
}

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

var taskDurationBucketsMs = []float64{100, 500, 1000, 2500, 5000, 15000, 30000, 60000, 120000}

type TaskDurationHistogramRow struct {
	JobType string
	Buckets []int64 // cumulative, same order/length as taskDurationBucketsMs
	Count   int64
	SumMs   int64
}

func (s *Store) TaskDurationHistogram(ctx context.Context) ([]TaskDurationHistogramRow, error) {
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

func (s *Store) ActiveWorkerCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM workers
		 WHERE last_seen_at IS NOT NULL AND last_seen_at > now() - interval '60 seconds'`,
	).Scan(&n)
	return n, err
}

type InvoiceView struct {
	JobID            uuid.UUID `json:"job_id"`
	BuyerID          uuid.UUID `json:"buyer_id"`
	Status           string    `json:"status"`
	JobType          string    `json:"job_type"`
	CreatedAt        time.Time `json:"created_at"`
	EstimatedUSD     float64   `json:"estimated_usd"`
	ActualUSD        float64   `json:"actual_usd"`
	ChargedUSD       float64   `json:"charged_usd"`
	SupplierPaidUSD  float64   `json:"supplier_credit_usd"`
	PlatformTakeUSD  float64   `json:"platform_take_usd"`
	QuotedUSD        *float64  `json:"quoted_usd,omitempty"`
	FirmQuote        bool      `json:"firm_quote,omitempty"`
	FirmQuoteMaxUSD  *float64  `json:"firm_quote_max_usd,omitempty"`
	BilledUSD        *float64  `json:"billed_usd,omitempty"`
	SLAGuaranteeSecs int       `json:"sla_guarantee_secs,omitempty"`
	SLAPremiumUSD    *float64  `json:"sla_premium_usd,omitempty"`
	SLARefundUSD     *float64  `json:"sla_refund_usd,omitempty"`
	SLAMet           *bool     `json:"sla_met,omitempty"`
}

func (s *Store) JobInvoice(ctx context.Context, jobID, buyerID uuid.UUID) (*InvoiceView, error) {
	iv := InvoiceView{JobID: jobID}
	var firmMax, billed *float64
	var slaGuarantee int
	var slaPremium *float64
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id, status, job_type, created_at,
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0),
		        firm_quote, firm_quote_max_usd, billed_usd,
		        COALESCE(sla_guarantee_secs,0), sla_premium_usd, sla_met
		 FROM jobs WHERE id = $1 AND buyer_id = $2`,
		jobID, buyerID,
	).Scan(&iv.BuyerID, &iv.Status, &iv.JobType, &iv.CreatedAt, &iv.EstimatedUSD, &iv.ActualUSD,
		&iv.FirmQuote, &firmMax, &billed, &slaGuarantee, &slaPremium, &iv.SLAMet)
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
			iv.SupplierPaidUSD += amt // clawback is negative -> reduces net paid
		case "platform_take":
			iv.PlatformTakeUSD += amt
		case "buyer_charge":
			iv.ChargedUSD += amt
		}
	}
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
	if quoted, ok, qerr := s.QuotedUSDForJob(ctx, jobID); qerr != nil {
		return nil, qerr
	} else if ok {
		iv.QuotedUSD = &quoted
	}
	return &iv, nil
}

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
