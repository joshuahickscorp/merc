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
	// ExpiresAt is the current token lifetime end. Zero means the row predated
	// expiry and is still inside the migration grace window (see
	// workerTokenGraceUntil). Callers must not treat zero as "never expires"
	// past the grace window — LookupWorkerToken rejects those.
	ExpiresAt time.Time
}

// Worker token lifetime policy.
//
// Short-lived tokens force a worker that has been offline past the window to
// re-enroll rather than resume with an indefinitely valid secret. Renewal is
// attached to the existing heartbeat so a live worker never needs a separate
// round-trip.
//
// workerTokenTTL is the lifetime of a newly issued or renewed token.
// workerTokenGrace is how long pre-expiry rows (expires_at IS NULL) remain
// valid after this code ships, so existing enrolled workers are not silently
// locked out at migration time.
const (
	workerTokenTTL   = 2 * time.Hour
	workerTokenGrace = 7 * 24 * time.Hour
)

// workerTokenGraceUntil is computed once at process start so grace is stable
// for the lifetime of a control-plane binary, not a moving window that never
// closes.
var workerTokenGraceUntil = time.Now().UTC().Add(workerTokenGrace)

// errWorkerTokenExpired is the greppable refusal for an expired credential.
var errWorkerTokenExpired = errors.New("WORKER_TOKEN_EXPIRED: worker token has expired; re-enroll")

func (s *Store) LookupWorkerToken(ctx context.Context, token string) (WorkerAuth, error) {
	var w WorkerAuth
	var expiresAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT worker_id,supplier_id,credential_id,COALESCE(device_fingerprint,''),
		        credential_version,device_fingerprint IS NOT NULL, expires_at
		   FROM worker_tokens
		 WHERE token_hash = $1 AND revoked = false`,
		hashKey(token),
	).Scan(&w.WorkerID, &w.SupplierID, &w.CredentialID, &w.DeviceFingerprint,
		&w.CredentialVersion, &w.EnrollmentDeviceBound, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, errNotFound
	}
	if err != nil {
		return w, err
	}
	now := time.Now().UTC()
	if expiresAt == nil {
		// Pre-migration token: honour the grace window, then force re-enroll.
		if now.After(workerTokenGraceUntil) {
			return w, errWorkerTokenExpired
		}
		w.ExpiresAt = workerTokenGraceUntil
		return w, nil
	}
	w.ExpiresAt = expiresAt.UTC()
	if !w.ExpiresAt.After(now) {
		return w, errWorkerTokenExpired
	}
	return w, nil
}

func (s *Store) CreateWorkerToken(ctx context.Context, workerID, supplierID uuid.UUID) (string, error) {
	raw := newSecret("cxw_")
	if raw == "" {
		return "", errors.New("worker token: entropy failure")
	}
	expires := time.Now().UTC().Add(workerTokenTTL)
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
		`INSERT INTO worker_tokens (token_hash, worker_id, supplier_id, revoked, expires_at, last_renewed_at)
		 VALUES ($1, $2, $3, false, $4, now())`,
		hashKey(raw), workerID, supplierID, expires,
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

// CreateWorkerTokenWithExpiry issues a token with an explicit lifetime. Used by
// seed/dev so demo tokens have a visible (long) expiry rather than infinite.
func (s *Store) CreateWorkerTokenWithExpiry(ctx context.Context, workerID, supplierID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = workerTokenTTL
	}
	raw := newSecret("cxw_")
	if raw == "" {
		return "", time.Time{}, errors.New("worker token: entropy failure")
	}
	expires := time.Now().UTC().Add(ttl)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class) VALUES ($1, $2, 'cpu')
		 ON CONFLICT (id) DO NOTHING`,
		workerID, supplierID,
	); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_tokens (token_hash, worker_id, supplier_id, revoked, expires_at, last_renewed_at)
		 VALUES ($1, $2, $3, false, $4, now())`,
		hashKey(raw), workerID, supplierID, expires,
	); err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, err
	}
	return raw, expires, nil
}

// RenewWorkerToken extends the lifetime of the credential currently presented
// by the worker. Called from the heartbeat path so a live worker never needs a
// separate renewal round-trip. Returns the new expiry.
func (s *Store) RenewWorkerToken(ctx context.Context, credentialID uuid.UUID) (time.Time, error) {
	expires := time.Now().UTC().Add(workerTokenTTL)
	tag, err := s.pool.Exec(ctx,
		`UPDATE worker_tokens
		    SET expires_at = $2, last_renewed_at = now()
		  WHERE credential_id = $1 AND revoked = false
		    AND (expires_at IS NULL OR expires_at > now())`,
		credentialID, expires,
	)
	if err != nil {
		return time.Time{}, err
	}
	if tag.RowsAffected() == 0 {
		return time.Time{}, errWorkerTokenExpired
	}
	return expires, nil
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
	profileDigest, err := profile.CapabilityDigest(runtimeAuthorityModels)
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
		    runtime_profile_revision, runtime_profile_digest,
		    sandboxed, unsandboxed_opt_in)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now(), $8,$9,$10,$11,$12,$13,
		         CASE WHEN $13::uuid IS NULL THEN NULL ELSE now() END, $14,$15,$16,$17,$18)
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
		   sandboxed = EXCLUDED.sandboxed,
		   unsandboxed_opt_in = EXCLUDED.unsandboxed_opt_in,
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
		cap.Sandboxed, cap.UnsandboxedOptIn,
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
		claimedRate := b.TPS
		if b.JobType == "embed" {
			claimedRate = b.EPS
		}
		// Self-reported rates are provisional until a peer of the same cell
		// class can corroborate them. Unpeered cells (no peer measurement)
		// keep the claimed rate so a single-supplier fleet stays routable;
		// only a disputed claim — peers exist and none agree — falls to the
		// floor (see RoutableBenchmarkRate / uncorroboratedBenchmarkFloorTPS).
		_, err = tx.Exec(ctx,
			`INSERT INTO benchmark_results
			   (worker_id, model_id, job_type, tps, eps, thermal_ok, p99_latency_ms, load_ms,
			    corroborated, claimed_rate)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8, false, $9)`,
			cap.WorkerID, b.ModelID, b.JobType, b.TPS, b.EPS, b.ThermalOK, float32(b.P99MS), loadMS,
			claimedRate,
		)
		if err != nil {
			return err
		}
		corroborated, peerAvailable, source, err := tryCorroborateBenchmarkTx(ctx, tx, cap.WorkerID, b.JobType, b.ModelID, claimedRate)
		if err != nil {
			return err
		}
		schedulerRate := RoutableBenchmarkRate(claimedRate, corroborated, peerAvailable)
		if corroborated {
			if _, err := tx.Exec(ctx,
				`UPDATE benchmark_results
				    SET corroborated = true, corroboration_source = $2, corroborated_at = now()
				  WHERE id = (
				    SELECT id FROM benchmark_results
				     WHERE worker_id = $1 AND job_type = $3 AND model_id = $4
				     ORDER BY measured_at DESC LIMIT 1
				  )`,
				cap.WorkerID, source, b.JobType, b.ModelID,
			); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO worker_tps_cache (worker_id, job_type, tps, claimed_tps, corroborated, updated_at)
			 VALUES ($1,$2,$3,$4,$5, now())
			 ON CONFLICT (worker_id, job_type) DO UPDATE SET
			   tps = EXCLUDED.tps, claimed_tps = EXCLUDED.claimed_tps,
			   corroborated = EXCLUDED.corroborated, updated_at = now()`,
			cap.WorkerID, b.JobType, schedulerRate, claimedRate, corroborated,
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
	ResidentModels     []ResidentModel
	EvictedModels      []string
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
	// Evictions first: a model that was dropped and then re-loaded in the same
	// window reappears below with a fresh measurement. DELETE mirrors the
	// prefix-routing budget eviction shape — remove the row, do not soft-flag.
	if len(r.EvictedModels) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM worker_model_state
			  WHERE worker_id = $1 AND model_id = ANY($2::text[])`,
			workerID, r.EvictedModels); err != nil {
			return err
		}
	}
	// Prefer measured resident_models. Legacy agents that only send loaded_models
	// still refresh last_seen_warm so warm-routing keeps working; those rows
	// remain unmeasured (NULL rss/load) and cannot authorize service-lease warmth.
	if len(r.ResidentModels) > 0 {
		for _, m := range r.ResidentModels {
			rss, err := residencyRSSDeltaForStore(m.RSSDeltaBytes)
			if err != nil {
				return err
			}
			loadMS, err := residencyLoadMSForStore(m.LoadMS)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO worker_model_state
				   (worker_id, model_id, last_seen_warm, rss_delta_bytes, load_ms)
				 VALUES ($1, $2, now(), $3, $4)
				 ON CONFLICT (worker_id, model_id)
				 DO UPDATE SET last_seen_warm = now(),
				               rss_delta_bytes = EXCLUDED.rss_delta_bytes,
				               load_ms = EXCLUDED.load_ms`,
				workerID, m.ModelID, rss, loadMS); err != nil {
				return err
			}
		}
	} else if len(r.LoadedModels) > 0 {
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

// WorkerAdmissionTerms is the pair of facts that decide whether this worker can
// ever be offered a task: the hardware class the runtime document must serve,
// and the reservation price the scheduler filters on.
func (s *Store) WorkerAdmissionTerms(ctx context.Context, workerID uuid.UUID) (hwClass string, minPayoutUSDHr float64, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(hw_class,''), COALESCE(min_payout_usd_hr, 0)
		   FROM workers WHERE id = $1`, workerID,
	).Scan(&hwClass, &minPayoutUSDHr)
	return hwClass, minPayoutUSDHr, err
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

// WorkerEarnings reports what a supplier has been paid, earned in total, still
// carries, and — crucially — what is held and why.
//
// CarriedUSD reads the account-level accrual, NOT a sum over per-entry
// settlement rows. Under account accrual each settlement records the carry
// AFTER that entry -- a running balance -- so summing them counts the same
// money once per entry. That bug reported five 0.4-cent credits, all of which
// had been paid, as $0.02 permanently carried: a supplier being told their
// entire earnings were stuck when the cash had already been sent.
//
// HeldByReason is exclusive per credit so the three freeze reasons (24h hold,
// dispute, canary manual gate) and verification are not collapsed into one
// opaque "owed" number the supplier must reverse-engineer.
func (s *Store) WorkerEarnings(ctx context.Context, supplierID uuid.UUID) (Earnings, error) {
	var e Earnings
	settlement, err := SettlementCurrency()
	if err != nil {
		return e, err
	}
	e.Currency = settlement.Code()
	var paidMinorUnits, carriedMicros int64
	err = s.pool.QueryRow(ctx,
		`SELECT
		   COALESCE(SUM(op.sent_cents) FILTER (
		     WHERE le.payout_status = 'released' AND op.cash_moved = true
		       AND op.sent_cents > 0), 0)::bigint,
		   COALESCE(SUM(le.amount_usd) FILTER (WHERE le.amount_usd > 0), 0),
		   COALESCE((SELECT a.accrued_microusd FROM supplier_payout_accruals a
		              WHERE a.supplier_id = $1), 0)::bigint
		 FROM ledger_entries le
		 LEFT JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.supplier_id = $1 AND le.kind = 'supplier_credit'`,
		supplierID,
	).Scan(&paidMinorUnits, &e.LifetimeUSD, &carriedMicros)
	if err != nil {
		return e, err
	}
	paidMicros, err := settlement.MinorToMicros(paidMinorUnits)
	if err != nil {
		return e, err
	}
	e.BalanceUSD = microsToUSD(paidMicros)
	e.CarriedUSD = microsToUSD(carriedMicros)

	manualGate, err := canaryManualPayoutGate()
	if err != nil {
		return e, err
	}
	e.ManualPayoutGate = manualGate
	if manualGate {
		e.ManualPayoutGateNote = "canary manual payout gate is in force: no cash leaves until an operator POSTs " +
			"/admin/payouts/{ledger_entry_id}/release naming the held credit. " +
			"next_payout_at is eligibility only, not a promise of automatic payout."
	}

	var lastMinorUnits int64
	var lastAt time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT op.sent_cents,op.updated_at
		   FROM ledger_entries le
		   JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		  WHERE le.supplier_id=$1 AND le.kind='supplier_credit'
		    AND le.payout_status='released' AND op.cash_moved=true
		  ORDER BY op.updated_at DESC,le.id DESC LIMIT 1`,
		supplierID,
	).Scan(&lastMinorUnits, &lastAt)
	switch {
	case err == nil:
		lastMicros, err := settlement.MinorToMicros(lastMinorUnits)
		if err != nil {
			return e, err
		}
		lastAmt := microsToUSD(lastMicros)
		e.LastPayoutUSD = &lastAmt
		t := lastAt.Unix()
		e.LastPayoutAt = &t
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return e, err
	}

	// Per-credit hold classification. One credit contributes to exactly one
	// reason bucket so the sums reconcile to HeldUSD.
	type holdRow struct {
		amountUSD float64
		status    string
		releaseAt *time.Time
		verdict   string
		disputed  bool
		approved  bool
	}
	rows, err := s.pool.Query(ctx, `
		SELECT le.amount_usd::float8, le.payout_status, le.release_at,
		       COALESCE(t.verification_outcome,''),
		       EXISTS(
		         SELECT 1 FROM disputes d
		          WHERE d.job_id=t.job_id
		            AND d.status IN ('open','no_peer','reverifying','unresolvable')
		       ),
		       EXISTS(
		         SELECT 1 FROM admin_actions aa
		          WHERE aa.kind='payout_released' AND aa.ledger_entry_id=le.id
		       )
		  FROM ledger_entries le
		  LEFT JOIN tasks t ON t.id=le.task_id
		 WHERE le.supplier_id=$1 AND le.kind='supplier_credit'
		   AND le.payout_status IN ('held','ready','awaiting_funding','sending','outcome_unknown')
		   AND le.amount_usd > 0
		 ORDER BY le.release_at NULLS LAST, le.id`, supplierID)
	if err != nil {
		return e, err
	}
	defer rows.Close()

	type bucket struct {
		amount   float64
		count    int
		earliest *time.Time
	}
	buckets := map[string]*bucket{}
	add := func(reason string, amount float64, releaseAt *time.Time) {
		b := buckets[reason]
		if b == nil {
			b = &bucket{}
			buckets[reason] = b
		}
		b.amount += amount
		b.count++
		if releaseAt != nil && (b.earliest == nil || releaseAt.Before(*b.earliest)) {
			t := *releaseAt
			b.earliest = &t
		}
	}

	var nextEligible *time.Time
	for rows.Next() {
		var row holdRow
		if err := rows.Scan(&row.amountUSD, &row.status, &row.releaseAt, &row.verdict, &row.disputed, &row.approved); err != nil {
			return e, err
		}
		e.HeldUSD += row.amountUSD
		switch {
		case row.disputed:
			add("dispute_freeze", row.amountUSD, row.releaseAt)
		case row.status == PayoutSending || row.status == PayoutOutcomeUnknown:
			add("in_flight", row.amountUSD, row.releaseAt)
		case row.status == PayoutAwaitingFunding:
			add("awaiting_funding", row.amountUSD, row.releaseAt)
		case row.verdict != "" && row.verdict != string(OutcomePass):
			add("verification", row.amountUSD, row.releaseAt)
		case manualGate && !row.approved && (row.status == PayoutHeld || row.status == PayoutReady):
			add("manual_gate", row.amountUSD, row.releaseAt)
			// Still surface eligibility for the hold window under manual gate.
			if row.releaseAt != nil && (nextEligible == nil || row.releaseAt.Before(*nextEligible)) {
				t := *row.releaseAt
				nextEligible = &t
			}
		case row.releaseAt != nil && row.releaseAt.After(time.Now()):
			add("hold_window", row.amountUSD, row.releaseAt)
			if nextEligible == nil || row.releaseAt.Before(*nextEligible) {
				t := *row.releaseAt
				nextEligible = &t
			}
		default:
			// Due / ready under the automatic path, or held with release_at past.
			add("hold_window", row.amountUSD, row.releaseAt)
			if row.releaseAt != nil && (nextEligible == nil || row.releaseAt.Before(*nextEligible)) {
				t := *row.releaseAt
				nextEligible = &t
			}
		}
	}
	if err := rows.Err(); err != nil {
		return e, err
	}

	details := map[string]string{
		"dispute_freeze":   "held while an active buyer dispute (open/no_peer/reverifying/unresolvable) freezes the job",
		"verification":     "held until the task has a durable unqualified verification pass",
		"manual_gate":      "canary manual payout gate: operator must release each ledger-entry UUID before cash moves",
		"hold_window":      "inside the post-completion payout hold window, or eligible and waiting for the payout sweep",
		"awaiting_funding": "held until buyer-collection or subsidy funding is reserved for this credit",
		"in_flight":        "payout has been claimed and is at the provider boundary (sending/outcome_unknown)",
	}
	// Stable order for clients that render the list as-is.
	order := []string{"dispute_freeze", "verification", "manual_gate", "hold_window", "awaiting_funding", "in_flight"}
	e.HeldByReason = make([]EarningsHoldReason, 0, len(buckets))
	for _, reason := range order {
		b := buckets[reason]
		if b == nil {
			continue
		}
		hr := EarningsHoldReason{
			Reason: reason, AmountUSD: b.amount, EntryCount: b.count, Detail: details[reason],
		}
		if b.earliest != nil && (reason == "hold_window" || reason == "manual_gate") {
			t := b.earliest.Unix()
			hr.EarliestReleaseAt = &t
		}
		e.HeldByReason = append(e.HeldByReason, hr)
	}
	if nextEligible != nil {
		t := nextEligible.Unix()
		e.NextPayoutAt = &t
	}
	return e, nil
}

// WorkerPayoutLedger returns recent supplier_credit and clawback rows for the
// supplier that owns this worker credential. Newest first. Bounded so a
// multi-year history cannot balloon a stranger-facing console response.
func (s *Store) WorkerPayoutLedger(ctx context.Context, supplierID uuid.UUID, limit int) (PayoutLedger, error) {
	var out PayoutLedger
	settlement, err := SettlementCurrency()
	if err != nil {
		return out, err
	}
	out.Currency = settlement.Code()
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT le.id, le.kind, le.amount_usd::float8, le.currency, le.payout_status,
		       le.task_id, t.job_id, le.release_at, le.created_at
		  FROM ledger_entries le
		  LEFT JOIN tasks t ON t.id = le.task_id
		 WHERE le.supplier_id = $1
		   AND le.kind IN ('supplier_credit','clawback')
		 ORDER BY le.created_at DESC, le.id DESC
		 LIMIT $2`, supplierID, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Entries = make([]PayoutLedgerEntry, 0, limit)
	for rows.Next() {
		var e PayoutLedgerEntry
		var releaseAt *time.Time
		var createdAt time.Time
		if err := rows.Scan(
			&e.ID, &e.Kind, &e.AmountUSD, &e.Currency, &e.PayoutStatus,
			&e.TaskID, &e.JobID, &releaseAt, &createdAt,
		); err != nil {
			return out, err
		}
		if releaseAt != nil {
			t := releaseAt.Unix()
			e.ReleaseAt = &t
		}
		e.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out.Entries = append(out.Entries, e)
	}
	return out, rows.Err()
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
