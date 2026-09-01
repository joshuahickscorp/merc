package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Split out of store.go, which had grown to 5,727 lines across roughly two
// dozen unrelated responsibilities.  Same package, same behaviour: this is a
// file move so that a reviewer can hold one subject at a time and two people
// can edit payouts and job submission without conflicting.

// errStripeAcctTaken is the named enrolment refusal when two suppliers would
// share one Stripe Connect payout account. Mirrored by the partial UNIQUE on
// suppliers.stripe_acct so a bypassed check still fails at the constraint.
var errStripeAcctTaken = errors.New("STRIPE_ACCT_TAKEN: stripe connect account already linked to another supplier")

var errStripeAcctIdentityMismatch = errors.New(
	"STRIPE_ACCT_IDENTITY_MISMATCH: supplier is already bound to a different Stripe account",
)

// errWorkerTokenUnboundForbidden refuses CreateWorkerToken in production.
// Unbound credentials cannot satisfy ordinary-work containment; production
// suppliers must enrol through the device-bound path (EnrollWorkerTx).
var errWorkerTokenUnboundForbidden = errors.New("WORKER_TOKEN_UNBOUND_FORBIDDEN: unbound worker tokens cannot be minted in production; enrol with device binding")

func (s *Store) SetSupplierStripeAcct(ctx context.Context, supplierID uuid.UUID, acct string) error {
	acct = strings.TrimSpace(acct)
	if acct == "" {
		_, err := s.pool.Exec(ctx, `UPDATE suppliers SET stripe_acct=NULL WHERE id=$1`, supplierID)
		return err
	}
	if !validStripeObjectID(acct, "acct_") {
		return errors.New("stripe connected account id must be an acct_* identifier")
	}
	var other uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM suppliers
		  WHERE stripe_acct = $1 AND id <> $2
		  LIMIT 1`, acct, supplierID,
	).Scan(&other)
	if err == nil {
		return errStripeAcctTaken
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE suppliers SET stripe_acct=$2
		WHERE id=$1 AND (stripe_acct IS NULL OR btrim(stripe_acct)='' OR stripe_acct=$2)`, supplierID, acct)
	if isUniqueViolation(err) {
		return errStripeAcctTaken
	}
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var current *string
	if err := s.pool.QueryRow(ctx, `SELECT stripe_acct FROM suppliers WHERE id=$1`, supplierID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	if current != nil && strings.TrimSpace(*current) != "" && strings.TrimSpace(*current) != acct {
		return errStripeAcctIdentityMismatch
	}
	return nil
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

var errWorkerIdentityPinned = errors.New("worker execution identity is pinned by a live dynamic task")

// insertWorkerWithDeviceSlotTx inserts a worker row and assigns the next
// dense device_slot. The slot is internal bookkeeping for the live-device
// index — never a credential, never returned to the caller, never an
// eligibility input. ON CONFLICT leaves an existing slot untouched.
func insertWorkerWithDeviceSlotTx(ctx context.Context, tx pgx.Tx, s *Store, workerID, supplierID uuid.UUID) error {
	var slot int64
	err := tx.QueryRow(ctx, `
		INSERT INTO workers (id, supplier_id, hw_class, device_slot)
		VALUES ($1, $2, 'cpu', nextval('workers_device_slot_seq'))
		ON CONFLICT (id) DO NOTHING
		RETURNING device_slot`,
		workerID, supplierID,
	).Scan(&slot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return nil
}

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

// CreateWorkerToken mints an unbound worker credential. It is the legacy
// self-serve path (POST /v1/supplier/worker-tokens) and remains available only
// outside production for tests, local fleets, and directed-work fixtures.
//
// Unbound tokens never satisfy ordinary-buyer containment: eligibility requires
// an active device-bound credential alongside sandboxed=true. Production
// suppliers must use enrolment (EnrollWorkerTx), which binds device material.
// Activating a pending supplier here is intentional for directed/dev paths that
// still need status='active' without ordinary-work eligibility.
func (s *Store) CreateWorkerToken(ctx context.Context, workerID, supplierID uuid.UUID) (string, error) {
	if isProductionEnv(os.Getenv("MERC_ENV")) {
		return "", errWorkerTokenUnboundForbidden
	}
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
	if err := insertWorkerWithDeviceSlotTx(ctx, tx, s, workerID, supplierID); err != nil {
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

// CreateWorkerTokenWithExpiry issues an unbound token with an explicit lifetime.
// Same production gate and containment non-eligibility as CreateWorkerToken.
// Used by seed/dev so demo tokens have a visible (long) expiry rather than infinite.
func (s *Store) CreateWorkerTokenWithExpiry(ctx context.Context, workerID, supplierID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	if isProductionEnv(os.Getenv("MERC_ENV")) {
		return "", time.Time{}, errWorkerTokenUnboundForbidden
	}
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
	if err := insertWorkerWithDeviceSlotTx(ctx, tx, s, workerID, supplierID); err != nil {
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

// seedDevicePublicKey is a 65-byte uncompressed P-256 placeholder that satisfies
// worker_tokens_device_binding_valid. Seed and test device binding use this shape;
// production enrolment supplies a real device key via EnrollWorkerTx.
func seedDevicePublicKey() []byte {
	key := make([]byte, 65)
	key[0] = 0x04
	for i := 1; i < 65; i++ {
		key[i] = byte(i)
	}
	return key
}

// IssueDeviceBoundWorkerToken installs an active device-bound credential for a
// worker. Production suppliers receive this row shape only through enrolment;
// seed and tests call this (or equivalent SQL) so ordinary-work eligibility can
// honour sandboxed=true without driving the full enrolment ceremony.
func (s *Store) IssueDeviceBoundWorkerToken(ctx context.Context, workerID, supplierID uuid.UUID, fingerprint string) (string, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return "", errors.New("device-bound worker token requires device_fingerprint")
	}
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
	if err := insertWorkerWithDeviceSlotTx(ctx, tx, s, workerID, supplierID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO worker_tokens
		  (token_hash, worker_id, supplier_id, revoked, expires_at, last_renewed_at,
		   device_key_algorithm, device_public_key, device_fingerprint)
		 VALUES ($1, $2, $3, false, $4, now(), 'p256', $5, $6)`,
		hashKey(raw), workerID, supplierID, expires, seedDevicePublicKey(), fingerprint,
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
	activationRevision := activationAdmissionRevision(cap.activationPolicyRevision)
	registeredAt := time.Now().UTC()
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
		return fmt.Errorf("%w: begin worker admission: %v",
			errActivationAdmissionUnavailable, err)
	}
	defer tx.Rollback(ctx)
	if err := guardActivationAdmissionTx(ctx, tx, activationRevision); err != nil {
		return err
	}
	// A dynamic task is born pinned to one worker. Serialize registration with
	// that insertion boundary so the peer cannot change identity immediately
	// after reserve consumption and leave an unclaimable permanent hedge or
	// tiebreak row. Regular running tasks retain the existing fail-closed
	// StartTask/CompleteTask rechecks; this narrower guard protects the unique
	// dynamic reserve whose row itself prevents a replacement.
	var (
		currentHW, currentEngine, currentBuild, currentPolicy, currentHardware string
		currentProfile, currentProfileRevision, currentProfileDigest           string
		currentSupportedJobs, currentSupportedModels                           []string
		currentMemoryGB, currentMinPayout                                      float32
		currentUnsandboxed                                                     bool
	)
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(hw_class,''),COALESCE(engine,''),COALESCE(build_hash,''),
		       COALESCE(build_identity_policy,''),COALESCE(hardware_identity,''),
		       COALESCE(runtime_profile_id,''),COALESCE(runtime_profile_revision,''),
		       COALESCE(runtime_profile_digest,''),COALESCE(memory_gb,0),
		       COALESCE(min_payout_usd_hr,0),COALESCE(supported_jobs,ARRAY[]::text[]),
		       COALESCE(supported_models,ARRAY[]::text[]),COALESCE(unsandboxed_opt_in,false)
		  FROM workers WHERE id=$1 FOR UPDATE`, cap.WorkerID).Scan(
		&currentHW, &currentEngine, &currentBuild, &currentPolicy, &currentHardware,
		&currentProfile, &currentProfileRevision, &currentProfileDigest,
		&currentMemoryGB, &currentMinPayout, &currentSupportedJobs, &currentSupportedModels,
		&currentUnsandboxed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	eligibilityChanged := err == nil && (currentHW != cap.HWClass || currentEngine != cap.Engine ||
		currentBuild != cap.BuildHash || currentPolicy != cap.BuildIdentityPolicy ||
		currentHardware != cap.HardwareIdentity || currentProfile != profile.RuntimeID ||
		currentProfileRevision != profile.Revision || currentProfileDigest != profileDigest ||
		currentMemoryGB != cap.MemoryGB || currentMinPayout != cap.MinPayoutUsdHr ||
		!sameStrings(currentSupportedJobs, cap.SupportedJobs) ||
		!sameStrings(currentSupportedModels, cap.SupportedModels) ||
		currentUnsandboxed != cap.UnsandboxedOptIn)
	if eligibilityChanged {
		var livePinned bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			 SELECT 1 FROM tasks t JOIN jobs j ON j.id=t.job_id
			  WHERE t.claimed_by=$1 AND t.hedged_from IS NOT NULL
			    AND t.status IN ('queued','retrying') AND t.started_at IS NULL
			    AND COALESCE(NULLIF(j.placement_requirement->>'version','')::int,1)>=3
			)`, cap.WorkerID).Scan(&livePinned); err != nil {
			return err
		}
		if livePinned {
			return fmt.Errorf("%w: worker %s", errWorkerIdentityPinned, cap.WorkerID)
		}
	}

	thermalOK := true
	for _, b := range cap.Benchmarks {
		thermalOK = thermalOK && b.ThermalOK
	}

	// Supplier residency used as a derived region fact when the agent did not
	// declare region. Provenance stays supplier_derived — never declared (D3).
	var supplierDataCountry string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(data_country,'') FROM suppliers WHERE id=$1`,
		cap.SupplierID,
	).Scan(&supplierDataCountry); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load supplier data_country for capability: %w", err)
	}

	// Monotonic capability epoch: bump on every registration that rewrites
	// capability facts. First enrollment starts at 1.
	var priorEpoch int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(capability_epoch,0) FROM workers WHERE id=$1`,
		cap.WorkerID,
	).Scan(&priorEpoch); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load capability epoch: %w", err)
	}
	nextEpoch := priorEpoch + 1

	// Re-registration must never make a genuine newer source measurement older.
	// Keep the authorization instant only for the exact same capability tuple;
	// a removed cell or a matrix/runtime/model change still disappears below.
	// This is weight-bearing for live dynamic pins: replaying a near-expiry cache
	// cannot shorten their remaining claim window after reserve was consumed.
	//
	// Load existing stamps BEFORE sealing the snapshot so the epoch-consistent
	// Capability and the dual-written wac rows share one authorized_at view.
	type wacIdentity struct {
		cellID, runtimeID, jobType, modelRef, modelKind, matrixSHA256 string
	}
	existingAuthorizedAt := make(map[wacIdentity]time.Time)
	rows, err := tx.Query(ctx, `
		SELECT cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256,authorized_at
		  FROM worker_authorized_capabilities
		 WHERE worker_id=$1`, cap.WorkerID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var key wacIdentity
		var authorizedAt time.Time
		if err := rows.Scan(
			&key.cellID, &key.runtimeID, &key.jobType, &key.modelRef,
			&key.modelKind, &key.matrixSHA256, &authorizedAt,
		); err != nil {
			rows.Close()
			return err
		}
		existingAuthorizedAt[key] = authorizedAt.UTC()
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	registeredAt = time.Now().UTC()
	cellAuthorizedAt := make(map[string]time.Time, len(projected))
	type wacWrite struct {
		cell       generatedRuntimeCapability
		routable   bool
		authorized time.Time
	}
	wacWrites := make([]wacWrite, 0, len(projected))
	for _, authorized := range projected {
		// Dual-write: routable is a COMPATIBILITY PROJECTION of the activation
		// policy authority, not a NodeCapability fact (D1/D2). Claim SQL still
		// reads this column for the legacy workload_decision IS NULL branch so
		// directed-only workers cannot claim ordinary work.
		routable := activationRoutableProjection(authorized.ID)
		authorizedAt := workerCapabilityAuthorizedAt(cap, authorized, routable, registeredAt)
		key := wacIdentity{
			cellID: authorized.ID, runtimeID: authorized.Runtime,
			jobType: authorized.Job, modelRef: authorized.Model,
			modelKind: authorized.ModelKind, matrixSHA256: generatedRuntimeMatrixSHA256,
		}
		if prior, ok := existingAuthorizedAt[key]; ok && prior.After(authorizedAt) {
			authorizedAt = prior
		}
		cellAuthorizedAt[authorized.ID] = authorizedAt
		wacWrites = append(wacWrites, wacWrite{cell: authorized, routable: routable, authorized: authorizedAt})
	}

	nodeCap, err := BuildNodeCapability(NodeCapabilityBuildInput{
		Registration:        cap,
		ProjectedCells:      projected,
		CellAuthorizedAt:    cellAuthorizedAt,
		SupplierDataCountry: supplierDataCountry,
		ProfileID:           profile.RuntimeID,
		ProfileRev:          profile.Revision,
		ProfileDigest:       profileDigest,
		MatrixSHA256:        generatedRuntimeMatrixSHA256,
		Epoch:               nextEpoch,
		CapturedAt:          registeredAt,
		LastSeen:            registeredAt,
	})
	if err != nil {
		return fmt.Errorf("build node capability: %w", err)
	}
	snapshotBlob, err := capabilitySnapshotJSON(nodeCap)
	if err != nil {
		return fmt.Errorf("seal capability snapshot: %w", err)
	}

	// Projection columns derived from the sealed snapshot (compatibility
	// indexes). min_payout remains on workers as reservation-price policy —
	// it is intentionally absent from NodeCapability (D1 mixing-point split).
	gpuCount := nodeCap.GPUCount.Value
	memPerGPU := nodeCap.MemoryGBPerGPU.Value
	interconnect := nodeCap.Interconnect.Value
	osVersion := ""
	if nodeCap.OSVersion.Knowledge != capabilityKnowledgeUnknown {
		osVersion = nodeCap.OSVersion.Value
	}
	region := ""
	regionProvenance := capabilityKnowledgeUnknown
	if nodeCap.Region.Knowledge != capabilityKnowledgeUnknown {
		region = nodeCap.Region.Value
		regionProvenance = nodeCap.Region.Knowledge
	}
	failureDomain := nodeCap.FailureDomain.Value
	if failureDomain == "" {
		failureDomain = capabilityUnknownValue
	}
	interruption := nodeCap.InterruptionPolicy.Value
	if interruption == "" {
		interruption = capabilityUnknownValue
	}
	var diskGB *float64
	if nodeCap.DiskGB.Knowledge != capabilityKnowledgeUnknown {
		v := nodeCap.DiskGB.Value
		diskGB = &v
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO workers
		   (id, supplier_id, hw_class, engine, build_hash, build_identity_policy, hardware_identity, memory_gb, bw_gbps, last_seen_at, version,
		    supported_jobs, supported_models, min_payout_usd_hr, thermal_ok,
		    agent_session_id, agent_session_started_at, runtime_profile_id,
		    runtime_profile_revision, runtime_profile_digest,
		    sandboxed, unsandboxed_opt_in,
		    gpu_count, memory_gb_per_gpu, interconnect, os_version,
		    region, region_provenance, failure_domain, interruption_policy, disk_gb,
		    capability_epoch, capability_digest)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now(), $10,$11,$12,$13,$14,$15,
		         CASE WHEN $15::uuid IS NULL THEN NULL ELSE now() END, $16,$17,$18,$19,$20,
		         $21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31)
		 ON CONFLICT (id) DO UPDATE SET
		   hw_class = EXCLUDED.hw_class,
		   engine = EXCLUDED.engine,
		   runtime_profile_id = EXCLUDED.runtime_profile_id,
		   runtime_profile_revision = EXCLUDED.runtime_profile_revision,
		   runtime_profile_digest = EXCLUDED.runtime_profile_digest,
		   build_hash = EXCLUDED.build_hash,
		   build_identity_policy = EXCLUDED.build_identity_policy,
		   hardware_identity = EXCLUDED.hardware_identity,
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
		   gpu_count = EXCLUDED.gpu_count,
		   memory_gb_per_gpu = EXCLUDED.memory_gb_per_gpu,
		   interconnect = EXCLUDED.interconnect,
		   os_version = EXCLUDED.os_version,
		   region = EXCLUDED.region,
		   region_provenance = EXCLUDED.region_provenance,
		   failure_domain = EXCLUDED.failure_domain,
		   interruption_policy = EXCLUDED.interruption_policy,
		   disk_gb = EXCLUDED.disk_gb,
		   capability_epoch = EXCLUDED.capability_epoch,
		   capability_digest = EXCLUDED.capability_digest,
		   agent_session_started_at = CASE
		     WHEN EXCLUDED.agent_session_id IS NOT NULL
		      AND workers.agent_session_id IS DISTINCT FROM EXCLUDED.agent_session_id
		     THEN now()
		     ELSE workers.agent_session_started_at
		   END,
		   agent_session_id = COALESCE(EXCLUDED.agent_session_id, workers.agent_session_id)`,
		cap.WorkerID, cap.SupplierID, cap.HWClass, cap.Engine, cap.BuildHash, cap.BuildIdentityPolicy, cap.HardwareIdentity, cap.MemoryGB, cap.MemoryBwGbps, cap.AgentVersion,
		cap.SupportedJobs, cap.SupportedModels, cap.MinPayoutUsdHr, thermalOK, cap.AgentSessionID,
		profile.RuntimeID, profile.Revision, profileDigest,
		cap.Sandboxed, cap.UnsandboxedOptIn,
		gpuCount, memPerGPU, interconnect, osVersion,
		nullIfEmpty(region), regionProvenance, failureDomain, interruption, diskGB,
		nextEpoch, nodeCap.Digest,
	)
	if err != nil {
		return err
	}

	// Append-only sealed snapshot for this epoch. Canonical read path.
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_capability_snapshots
		   (worker_id, epoch, captured_at, digest, snapshot)
		 VALUES ($1,$2,$3,$4,$5)`,
		cap.WorkerID, nextEpoch, registeredAt, nodeCap.Digest, snapshotBlob,
	); err != nil {
		return fmt.Errorf("append capability snapshot: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM worker_authorized_capabilities WHERE worker_id = $1`,
		cap.WorkerID); err != nil {
		return err
	}
	for _, write := range wacWrites {
		if _, err := tx.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
				   (worker_id, cell_id, runtime_id, job_type, model_ref, model_kind,
				    matrix_sha256, routable, authorized_at)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			cap.WorkerID, write.cell.ID, write.cell.Runtime, write.cell.Job,
			write.cell.Model, write.cell.ModelKind, generatedRuntimeMatrixSHA256,
			// Routability comes from the ACTIVATION projection, never from the
			// worker and never from NodeCapability. A directed-only cell is
			// recorded as non-routable so the scheduler's legacy branch cannot
			// dispatch ordinary work to it.
			write.routable, write.authorized); err != nil {
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
			    corroborated, claimed_rate, measured_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8, false, $9, $10)`,
			cap.WorkerID, b.ModelID, b.JobType, b.TPS, b.EPS, b.ThermalOK, float32(b.P99MS), loadMS,
			claimedRate, time.Unix(int64(b.MeasuredUnix), 0).UTC(),
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

func workerCapabilityAuthorizedAt(
	cap WorkerCapability,
	authorized generatedRuntimeCapability,
	current bool,
	registeredAt time.Time,
) time.Time {
	if current {
		for _, benchmark := range cap.Benchmarks {
			if benchmark.JobType == authorized.Job && benchmark.ModelID == authorized.Model {
				return time.Unix(int64(benchmark.MeasuredUnix), 0).UTC()
			}
		}
	}
	return registeredAt.UTC()
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
