package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Split out of store.go, which had grown to 5,727 lines across roughly two
// dozen unrelated responsibilities.  Same package, same behaviour: this is a
// file move so that a reviewer can hold one subject at a time and two people
// can edit payouts and job submission without conflicting.

func (s *Store) SetSupplierStripeAcct(ctx context.Context, supplierID uuid.UUID, acct string) error {
	_, err := s.pool.Exec(ctx, `UPDATE suppliers SET stripe_acct=$2 WHERE id=$1`, supplierID, acct)
	return err
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

func (s *Store) UpsertWorker(ctx context.Context, cap WorkerCapability) error {
	projected, err := projectWorkerRuntimeCapabilities(cap)
	if err != nil {
		return fmt.Errorf("projecting worker runtime capabilities: %w", err)
	}
	// Governed profile identity, resolved from the engine the agent reports
	// rather than from anything the worker gets to name. This is where the
	// profile's device-count range and per-cell memory floor become an actual
	// admission constraint instead of a declaration nothing compared against.
	profile, err := ResolveWorkerRuntimeProfile(cap)
	if err != nil {
		return fmt.Errorf("resolving worker runtime profile: %w", err)
	}
	// The full identity, not just the name. A worker enrolled against r1 is not
	// interchangeable with one enrolled against r4: it may be running a
	// different tokenizer, template or device budget.
	profileDigest, err := profile.ContentDigest(runtimeAuthorityModels)
	if err != nil {
		return fmt.Errorf("digesting worker runtime profile: %w", err)
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
		    supported_jobs, supported_models, min_payout_usd_hr, thermal_ok,
		    agent_session_id, agent_session_started_at, runtime_profile_id,
		    runtime_profile_revision, runtime_profile_digest)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now(), $8,$9,$10,$11,$12,$13,
		         CASE WHEN $13::uuid IS NULL THEN NULL ELSE now() END, $14,$15,$16)
		 ON CONFLICT (id) DO UPDATE SET
		   hw_class = EXCLUDED.hw_class,
		   engine = EXCLUDED.engine,
		   runtime_profile_id = EXCLUDED.runtime_profile_id,
		   runtime_profile_revision = EXCLUDED.runtime_profile_revision,
		   runtime_profile_digest = EXCLUDED.runtime_profile_digest,
		   build_hash = EXCLUDED.build_hash,
		   memory_gb = EXCLUDED.memory_gb,
		   bw_gbps = EXCLUDED.bw_gbps,
		   last_seen_at = now(),
		   version = EXCLUDED.version,
		   supported_jobs = EXCLUDED.supported_jobs,
		   supported_models = EXCLUDED.supported_models,
		   min_payout_usd_hr = EXCLUDED.min_payout_usd_hr,
		   thermal_ok = EXCLUDED.thermal_ok,
		   agent_session_started_at = CASE
		     WHEN EXCLUDED.agent_session_id IS NOT NULL
		      AND workers.agent_session_id IS DISTINCT FROM EXCLUDED.agent_session_id
		     THEN now()
		     ELSE workers.agent_session_started_at
		   END,
		   agent_session_id = COALESCE(EXCLUDED.agent_session_id, workers.agent_session_id)`,
		cap.WorkerID, cap.SupplierID, cap.HWClass, cap.Engine, cap.BuildHash, cap.MemoryGB, cap.MemoryBwGbps, cap.AgentVersion,
		cap.SupportedJobs, cap.SupportedModels, cap.MinPayoutUsdHr, thermalOK, cap.AgentSessionID,
		profile.RuntimeID, profile.Revision, profileDigest,
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
				   (worker_id, cell_id, runtime_id, job_type, model_ref, model_kind,
				    matrix_sha256, routable)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			cap.WorkerID, authorized.ID, authorized.Runtime, authorized.Job,
			authorized.Model, authorized.ModelKind, generatedRuntimeMatrixSHA256,
			// Routability comes from the ADVERTISED projection, never from the
			// worker. A directed-only capability is recorded as non-routable so
			// the scheduler's legacy branch cannot dispatch ordinary work to it.
			advertisedRuntimeCell(authorized.ID)); err != nil {
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

func (s *Store) DeleteOldWorkerMemorySamples(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM worker_memory_samples WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) ListWorkers(ctx context.Context) ([]AdminWorker, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT w.id, w.supplier_id, w.hw_class, COALESCE(w.memory_gb, 0),
		        COALESCE(w.effective_memory_gb, w.memory_gb, 0),
		        COALESCE(w.throttled, false),
		        COALESCE(s2.avg_available_gb, 0), COALESCE(s2.n, 0),
		        w.last_seen_at,
		        COALESCE(w.version,''), COALESCE(s.reputation, 0),
		        COALESCE(s.tier, 0), COALESCE(s.status, 'pending')
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

func (s *Store) SuspendWorker(ctx context.Context, actor AdminActor, workerID uuid.UUID, reason, correlationRef string) error {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionWorkerSuspended, TargetKind: adminTargetWorker,
		TargetID: workerID, Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return err
	}
	if replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent); err != nil {
		return err
	} else if replay.Found {
		return tx.Commit(ctx)
	}

	var supplierID uuid.UUID
	var beforeStatus string
	if err := tx.QueryRow(ctx, `
		SELECT s.id,s.status
		  FROM workers w JOIN suppliers s ON s.id=w.supplier_id
		 WHERE w.id=$1 FOR UPDATE OF s`, workerID).Scan(&supplierID, &beforeStatus); errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE suppliers SET status='suspended' WHERE id=$1`, supplierID); err != nil {
		return err
	}
	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, &supplierID, nil,
		map[string]any{"status": beforeStatus}, map[string]any{"status": "suspended"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReinstateWorker(ctx context.Context, actor AdminActor, workerID uuid.UUID, reason, correlationRef string) error {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionWorkerReinstated, TargetKind: adminTargetWorker,
		TargetID: workerID, Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return err
	}
	if replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent); err != nil {
		return err
	} else if replay.Found {
		return tx.Commit(ctx)
	}

	var supplierID uuid.UUID
	var beforeStatus string
	var beforeQuarantinedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT s.id,s.status,s.quarantined_at
		  FROM workers w JOIN suppliers s ON s.id=w.supplier_id
		 WHERE w.id=$1 FOR UPDATE OF s`, workerID).Scan(
		&supplierID, &beforeStatus, &beforeQuarantinedAt); errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	} else if err != nil {
		return err
	}
	if beforeStatus != "suspended" {
		return errNotSuspended
	}
	if _, err := tx.Exec(ctx,
		`UPDATE suppliers SET status='active',quarantined_at=NULL WHERE id=$1`, supplierID); err != nil {
		return err
	}
	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, &supplierID, nil,
		map[string]any{"status": beforeStatus, "quarantined_at": beforeQuarantinedAt},
		map[string]any{"status": "active", "quarantined_at": nil}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CanaryWorkerAdmissionAllowed(ctx context.Context, workerID uuid.UUID, max int) (bool, error) {
	var alreadyActive bool
	var active int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  EXISTS(SELECT 1 FROM workers WHERE id=$1 AND last_seen_at > now()-interval '60 seconds'),
		  count(*)
		FROM workers WHERE last_seen_at > now()-interval '60 seconds'`, workerID,
	).Scan(&alreadyActive, &active)
	return alreadyActive || active < max, err
}

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

// WorkerEarnings reports what a supplier has been paid, earned in total, and
// still carries.
//
// CarriedUSD reads the account-level accrual, NOT a sum over per-entry
// settlement rows. Under account accrual each settlement records the carry
// AFTER that entry -- a running balance -- so summing them counts the same
// money once per entry. That bug reported five 0.4-cent credits, all of which
// had been paid, as $0.02 permanently carried: a supplier being told their
// entire earnings were stuck when the cash had already been sent.
func (s *Store) WorkerEarnings(ctx context.Context, supplierID uuid.UUID) (Earnings, error) {
	var e Earnings
	err := s.pool.QueryRow(ctx,
		`SELECT
		   COALESCE(SUM(op.sent_cents) FILTER (
		     WHERE le.payout_status = 'released' AND op.cash_moved = true
		       AND op.sent_cents > 0), 0)::float8 / 100.0,
		   COALESCE(SUM(le.amount_usd) FILTER (WHERE le.amount_usd > 0), 0),
		   COALESCE((SELECT a.accrued_microusd FROM supplier_payout_accruals a
		              WHERE a.supplier_id = $1), 0)::float8 / 1000000.0
		 FROM ledger_entries le
		 LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
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

func (s *Store) ActiveWorkerCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM workers
		 WHERE last_seen_at IS NOT NULL AND last_seen_at > now() - interval '60 seconds'`,
	).Scan(&n)
	return n, err
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
