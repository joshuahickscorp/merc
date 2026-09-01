package main

import (
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

type Store struct {
	pool                  *pgxpool.Pool
	verificationResources *verificationResourceBudget
	// Process-local caches for hot first-token path reads. See api_key_cache.go
	// and operational_control_cache.go for the revocation / kill-switch bounds.
	apiKeys             apiKeyCache
	operationalControls operationalControlCache
	// apiKeyLookups collapses concurrent cold LookupAPIKey misses for the same
	// key_hash so a TTL-expiry stampede is one DB round-trip, not N. Negatives
	// are still never cached (singleflight only shares the in-flight result).
	apiKeyLookups singleflight.Group
	// Coalesced liveness ingest: many authenticated heartbeats fold into few
	// multi-row statements. See liveness_ingest.go. Lazily initialised so
	// tests can override config before the first heartbeat.
	livenessOnce        sync.Once
	livenessCoalescer   *livenessCoalescer
	livenessCfg         livenessBatchConfig
	livenessCfgOverride *livenessBatchConfig
	// Process-wide live-device index. Shadow only: populated from the
	// heartbeat path, never consulted for authorize/claim/lease/pricing.
	// Starts empty (fail-closed) and is sized to the worker count plus
	// headroom. See liveness_shadow.go.
	liveIndexOnce sync.Once
	liveIndex     *LiveDeviceIndex
	// slotOwner is a dense fingerprint table indexed by offer_slot (8 B/slot),
	// allocated to the same capacity as liveIndex under liveIndexOnce. 0 means
	// unclaimed. Elements are accessed with atomic.Load/StoreUint64 — same
	// concurrency model as LiveDeviceIndex.epochs. See liveness_shadow.go.
	slotOwner []uint64
	// offerSlotCache keys the live index by OFFER, not worker: the money-selection
	// liveness plane is per-(worker_id, runtime_profile_id).
	// offerSlotKey → offerBinding (slot + the supplier and capacity the durable
	// UPDATE's WHERE clause would have checked). Invalidated on re-registration.
	offerSlotCache sync.Map
	// offerPersistCache records what the last SUCCESSFUL durable heartbeat write
	// actually persisted per offer (offerSlotKey → offerPersistState). It exists
	// so the flag-ON path can skip a durable transaction for a heartbeat that
	// would change nothing. Never consulted while the flag is off.
	offerPersistCache sync.Map
}

func NewStore(pool *pgxpool.Pool) *Store {
	return newStoreWithVerificationLeaderConnections(pool, 0)
}

func NewStoreWithWorkerLeader(pool *pgxpool.Pool) *Store {
	return newStoreWithVerificationLeaderConnections(pool, verificationWorkerLeaderConnections)
}

func newStoreWithVerificationLeaderConnections(pool *pgxpool.Pool, leaderConns int32) *Store {
	maxConns := int32(0)
	if pool != nil {
		maxConns = pool.Config().MaxConns
	}
	return &Store{
		pool:                  pool,
		verificationResources: newVerificationResourceBudget(maxConns, leaderConns, verificationArtifactMemoryCeiling),
	}
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const schemaMigrationAdvisoryLock = "computeexchange-control-schema-v1"

//go:embed schema.sql

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
	return s.migrate(ctx, true)
}

// adoptMigratedSchema runs the post-DDL half of Migrate against a database
// that already has src/control/schema.sql (a template clone). Catalog/profile
// sync and activation load still run so a test that installed in-memory
// authority before opening the store gets the same seed a fresh apply would.
func (s *Store) adoptMigratedSchema(ctx context.Context) error {
	return s.migrate(ctx, false)
}

func (s *Store) migrate(ctx context.Context, applyCanonicalSchema bool) error {
	conn, release, err := s.acquireSchemaMigrationLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	// Optional operator escape hatch for unlabeled prepaid cash whose operations
	// history cannot determine a single currency. The schema reconstruction
	// reads merc.prepaid_legacy_currency; silence is not a default to usd.
	if override := strings.TrimSpace(os.Getenv("MERC_PREPAID_LEGACY_CURRENCY")); override != "" {
		parsed, perr := ParseCurrency(override)
		if perr != nil {
			return fmt.Errorf("MERC_PREPAID_LEGACY_CURRENCY: %w", perr)
		}
		if _, err := conn.Exec(ctx,
			`SELECT set_config('merc.prepaid_legacy_currency', $1, false)`,
			parsed.Code()); err != nil {
			return fmt.Errorf("set prepaid legacy currency override: %w", err)
		}
	}
	if applyCanonicalSchema {
		_, err = conn.Conn().PgConn().Exec(ctx, canonicalSchema).ReadAll()
		if err != nil {
			return fmt.Errorf("apply canonical schema: %w", err)
		}
	}
	if err := syncRuntimeCatalog(ctx, conn); err != nil {
		return err
	}
	// Governed runtime identity. This refuses to start when a registered
	// profile's content changed without a revision bump, which is the point:
	// continuing would leave the database describing one runtime and the process
	// describing another.
	if err := syncRuntimeProfiles(ctx, conn); err != nil {
		return err
	}
	release()
	// Install the activation policy in force before anything is served. Until
	// this runs the process operates on what the embedded document declares,
	// which is the right fallback and the wrong answer once an operator has
	// promoted or rolled back a cell.
	if err := loadActivationAtStartup(ctx, s.pool); err != nil {
		return fmt.Errorf("load runtime activation policy: %w", err)
	}
	if _, err := s.ReconcileLegacyVerifyingTasks(ctx); err != nil {
		return fmt.Errorf("reconcile legacy verification: %w", err)
	}
	// Empty fail-closed index, sized to current workers. Devices reconstruct
	// by heartbeating inside 45s. Not a selection input.
	s.ensureLiveDeviceIndex()
	return nil
}

var (
	errNotFound              = errors.New("not found")
	errJobNotCancellable     = errors.New("job is no longer cancellable")
	errDisputeReasonRequired = errors.New("dispute reason is required")
	errDisputeReasonTooLong  = errors.New("dispute reason exceeds 1000 characters")
	errDisputeJobNotTerminal = errors.New("only terminal jobs may be disputed")
	errDisputeWindowClosed   = errors.New("the seven-day dispute filing window has closed")
	errDisputeAlreadyActive  = errors.New("an active dispute already exists for this job")
	// Operator resolution is only for the terminal operator queue. Automatic
	// re-verification still owns open/no_peer/reverifying; unresolvable is the
	// human queue (no silent auto-award to either party).
	errDisputeNotOperatorQueue       = errors.New("dispute_not_operator_queue: only unresolvable disputes may be operator-resolved")
	errDisputeResolutionInvalid      = errors.New("dispute_resolution_invalid: resolution must be upheld or rejected")
	errDisputeAlreadyResolved        = errors.New("dispute_already_resolved: dispute is already terminal")
	errDisputePayoutRecoveryInFlight = errors.New("dispute_payout_recovery_in_flight: provider payout recovery must settle before rejection")
)

var errOAuthLinkStateInvalid = errors.New("invalid or expired OAuth link state")

const (
	maxOAuthLinkCapabilityBytes = 256
	maxOAuthLinkStateLifetime   = 15 * time.Minute
)

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

func nullInputDepthBand(band string) any {
	if band == "" {
		return nil
	}
	return band
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
	digest := apiKeyCacheKeyFor(raw)
	return hex.EncodeToString(digest[:])
}

type AuthResult struct {
	BuyerID     uuid.UUID
	IsAdmin     bool
	APIKeyID    uuid.UUID
	APIKeyLabel string
}

func (s *Store) LookupAPIKey(ctx context.Context, rawKey string) (AuthResult, error) {
	keyDigest := apiKeyCacheKeyFor(rawKey)
	if cached, ok := s.apiKeys.get(keyDigest); ok {
		return cached, nil
	}
	keyHash := hex.EncodeToString(keyDigest[:])
	// Coalesce concurrent cold misses for this hash. Double-check the cache
	// inside the group so a leader that just filled it satisfies waiters
	// without a second query.
	v, err, _ := s.apiKeyLookups.Do(keyHash, func() (any, error) {
		if cached, ok := s.apiKeys.get(keyDigest); ok {
			return cached, nil
		}
		var r AuthResult
		qerr := s.pool.QueryRow(ctx,
			`SELECT id, buyer_id, is_admin,
			        COALESCE(NULLIF(name,''), CASE WHEN is_admin THEN 'break-glass API key' ELSE 'API key' END)
			   FROM api_keys
			 WHERE key_hash = $1 AND revoked = false
			   AND (is_admin OR EXISTS (
			     SELECT 1 FROM buyers b WHERE b.id=api_keys.buyer_id AND b.deleted_at IS NULL))`,
			keyHash,
		).Scan(&r.APIKeyID, &r.BuyerID, &r.IsAdmin, &r.APIKeyLabel)
		if errors.Is(qerr, pgx.ErrNoRows) {
			// Never cache negatives: a re-minted or un-revoked key must not be
			// blocked by a stale miss, and an attacker must not pin "denied".
			return AuthResult{}, errNotFound
		}
		if qerr != nil {
			return AuthResult{}, qerr
		}
		s.apiKeys.put(keyDigest, r)
		return r, nil
	})
	if err != nil {
		return AuthResult{}, err
	}
	return v.(AuthResult), nil
}

type APIKeyRow struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Masked    string    `json:"masked"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

// createAPIKeySQL hardcodes is_admin = false so ordinary buyer signup and key
// minting can never elevate to the admin credential class.

// createAPIKeySQL hardcodes is_admin = false so ordinary buyer signup and key
// minting can never elevate to the admin credential class.
const createAPIKeySQL = `INSERT INTO api_keys (buyer_id, key_hash, name, masked, is_admin, revoked)
		 SELECT id, $2, $3, $4, false, false
		   FROM buyers WHERE id=$1 AND deleted_at IS NULL
		 RETURNING id`

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
	err = s.pool.QueryRow(ctx, createAPIKeySQL, buyerID, hashKey(raw), name, masked).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", "", errNotFound
		}
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
	// Invalidate before returning success so same-process revoke is immediate.
	// Multi-instance residual is bounded by apiKeyCacheTTL (see api_key_cache.go).
	s.apiKeys.invalidateID(id)
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

const memSampleWindow = 20

// DeleteOldAlphaRequests removes non-accepted alpha leads created before the
// cutoff. Accepted rows are retained (they have no buyer account to tombstone).
func (s *Store) DeleteOldAlphaRequests(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM alpha_requests
		 WHERE status <> 'accepted' AND created_at < $1`, before)
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

var errNotSuspended = errors.New("supplier is not suspended")

type AdminAction struct {
	ID               uuid.UUID       `json:"id"`
	CreatedAt        time.Time       `json:"created_at"`
	Kind             string          `json:"kind"`
	TaskID           *uuid.UUID      `json:"task_id,omitempty"`
	SupplierID       *uuid.UUID      `json:"supplier_id,omitempty"`
	LedgerEntryID    *uuid.UUID      `json:"ledger_entry_id,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	Detail           json.RawMessage `json:"detail,omitempty"`
	ActorMode        string          `json:"actor_mode,omitempty"`
	ActorPrincipalID *uuid.UUID      `json:"actor_principal_id,omitempty"`
	ActorSessionID   *uuid.UUID      `json:"actor_session_id,omitempty"`
	ActorLabel       string          `json:"actor_label,omitempty"`
	AttributionScope string          `json:"attribution_scope,omitempty"`
	IntentVersion    *int            `json:"intent_version,omitempty"`
	RequestSHA256    string          `json:"request_sha256,omitempty"`
	CorrelationRef   string          `json:"correlation_ref,omitempty"`
	TargetKind       string          `json:"target_kind,omitempty"`
	TargetID         *uuid.UUID      `json:"target_id,omitempty"`
}

func (s *Store) ListAdminActions(ctx context.Context) ([]AdminAction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id,created_at,kind,task_id,supplier_id,ledger_entry_id,COALESCE(reason,''),detail,
		        COALESCE(actor_mode,''),actor_principal_id,actor_session_id,COALESCE(actor_label,''),
		        COALESCE(attribution_scope,''),intent_version,COALESCE(request_sha256,''),
		        COALESCE(correlation_ref,''),COALESCE(target_kind,''),target_id
		 FROM admin_actions ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAction
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Kind, &a.TaskID, &a.SupplierID, &a.LedgerEntryID,
			&a.Reason, &a.Detail, &a.ActorMode, &a.ActorPrincipalID, &a.ActorSessionID, &a.ActorLabel,
			&a.AttributionScope, &a.IntentVersion, &a.RequestSHA256, &a.CorrelationRef,
			&a.TargetKind, &a.TargetID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

var errNotRequeueable = errors.New("task is not in a requeueable state")

func (s *Store) AdminAdjustReputation(ctx context.Context, actor AdminActor, supplierID uuid.UUID, delta float32, reason, correlationRef string) (before, after float32, err error) {
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionReputationChanged, TargetKind: adminTargetSupplier,
		TargetID: supplierID, Reason: reason, CorrelationRef: correlationRef, Delta: &delta,
	})
	if err != nil {
		return 0, 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return 0, 0, err
	}
	if replay, err := acquireAdminMutationReplay(ctx, tx, actor, intent); err != nil {
		return 0, 0, err
	} else if replay.Found {
		before, after, err := replayedReputation(replay.Detail)
		if err != nil {
			return 0, 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, 0, err
		}
		return before, after, nil
	}

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

	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, &supplierID, nil,
		map[string]any{"reputation": before}, map[string]any{"reputation": after}); err != nil {
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

var errNotHeld = errors.New("ledger entry is not held or ready")

var errIdempotencyConflict = errors.New("idempotency key was already used with a different request")

type CanaryAdmissionCounts struct {
	QueuedJobs          int
	JobsToday           int
	HeldShadowPayoutUSD float64
}

func (s *Store) CanaryAdmissionCounts(ctx context.Context) (CanaryAdmissionCounts, error) {
	var out CanaryAdmissionCounts
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM jobs WHERE status='queued'),
		  (SELECT count(*) FROM jobs WHERE created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
		  (SELECT COALESCE(sum(amount_usd),0)::float8 FROM ledger_entries
		    WHERE kind='supplier_credit' AND payout_status IN ('held','ready','sending','outcome_unknown'))`,
	).Scan(&out.QueuedJobs, &out.JobsToday, &out.HeldShadowPayoutUSD)
	return out, err
}

func (s *Store) CanaryBuyerAdmissionAllowed(ctx context.Context, buyerID uuid.UUID, max int) (bool, error) {
	var alreadyActive bool
	var active int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  EXISTS(SELECT 1 FROM jobs WHERE buyer_id=$1 AND created_at >= now()-interval '24 hours'),
		  count(DISTINCT buyer_id)
		FROM jobs WHERE created_at >= now()-interval '24 hours'`, buyerID,
	).Scan(&alreadyActive, &active)
	return alreadyActive || active < max, err
}

const driftMinSamples = 5

const driftWindow = 24 * time.Hour

// HistoricalP90DurationMs is scoped by (job_type, model_ref, input_depth_band).
// An empty inputDepthBand addresses only the legacy NULL/empty bucket so new
// band-specific callers never inherit undepth-scoped history.
func (s *Store) HistoricalP90DurationMs(ctx context.Context, jobType, modelRef, inputDepthBand string) (p90ms int64, samples int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*),
		        COALESCE(percentile_disc(0.9) WITHIN GROUP (ORDER BY duration_ms), 0)
		   FROM task_durations
		  WHERE job_type = $1
		    AND COALESCE(model_ref,'') = $2
		    AND COALESCE(input_depth_band,'') = $3
		    AND created_at > now() - make_interval(secs => $4)`,
		jobType, modelRef, inputDepthBand, int(driftWindow.Seconds()),
	).Scan(&samples, &p90ms)
	if err != nil {
		return 0, 0, err
	}
	if samples < driftMinSamples {
		return 0, samples, nil // too thin  -  caller falls back to the static target
	}
	return p90ms, samples, nil
}

// etaBiasFactorMax bounds how far realized-versus-predicted history may stretch a
// quoted ETA. A window that is pathological (one job that sat queued for a day)
// must not be able to quote an absurd completion time.
const etaBiasFactorMax = 3.0

// ETABiasFactor closes the loop that recordEtaCalibration opens. Every finalized
// job writes its predicted and realized seconds to eta_calibration; this reads
// the (job_type, tier, model_ref, input_depth_band)-scoped median
// realized/predicted ratio back so the next quote can correct for a systematic
// bias without letting unlike input depths stretch each other.
//
// The correction is deliberately one-sided: the returned factor is never below
// 1. An ETA that is too optimistic misleads the buyer and burns SLA refunds, so
// stretching it is both honest and safe. An ETA that is too pessimistic is only
// unattractive, and shrinking it on this evidence would tighten the SLA bound
// that deriveQuoteSLA computes from the same number — paying out on a promise
// that a burst of fast jobs talked us into.
//
// Below driftMinSamples the factor is exactly 1 and the quote is unchanged.
// Calibration rows use jobs.eta_secs_raw as their denominator, so the learner
// always measures the uncalibrated estimator rather than feeding its own
// corrected buyer promise back into the next window. Legacy jobs with no raw
// authority use eta_secs once and naturally age out of driftWindow.
//
// An empty inputDepthBand addresses only the legacy NULL/empty bucket; new
// callers pass a validated non-empty band so legacy rows never bleed into it.
func (s *Store) ETABiasFactor(
	ctx context.Context, jobType, tier, modelRef, inputDepthBand string,
) (factor float64, samples int, err error) {
	var median float64
	// phase='total' only: per-phase rows (G053) are observations and must not
	// stretch a quoted ETA. Empty phase is treated as total for pre-migration
	// rows that somehow lack the DEFAULT (none should, after ADD COLUMN).
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*),
		        COALESCE(percentile_cont(0.5) WITHIN GROUP (
		          ORDER BY realized_secs::float8 / predicted_secs), 1)
		   FROM eta_calibration
		  WHERE job_type = $1
		    AND tier = $2
		    AND COALESCE(model_ref,'') = $3
		    AND COALESCE(input_depth_band,'') = $4
		    AND COALESCE(phase,'total') = 'total'
		    AND predicted_secs > 0
		    AND realized_secs >= 0
		    AND created_at > now() - make_interval(secs => $5)`,
		jobType, tier, modelRef, inputDepthBand, int(driftWindow.Seconds()),
	).Scan(&samples, &median)
	if err != nil {
		return 1, 0, err
	}
	if samples < driftMinSamples {
		return 1, samples, nil
	}
	return clampETABiasFactor(median), samples, nil
}

func clampETABiasFactor(median float64) float64 {
	if math.IsNaN(median) || math.IsInf(median, 0) || median <= 1 {
		return 1
	}
	if median > etaBiasFactorMax {
		return etaBiasFactorMax
	}
	return median
}

// applyETABias stretches a quoted ETA by a calibration factor from ETABiasFactor.
// A factor of exactly 1 (the no-evidence case) returns secs unchanged.
func applyETABias(secs int, factor float64) int {
	if secs <= 0 || factor <= 1 || math.IsNaN(factor) || math.IsInf(factor, 0) {
		return secs
	}
	stretched := int(math.Ceil(float64(secs) * factor))
	if stretched < secs {
		return secs // overflow guard: never shorten
	}
	return stretched
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
		  GROUP BY td.job_type, COALESCE(td.model_ref,'')
		  ORDER BY COUNT(*) DESC, td.job_type, COALESCE(td.model_ref,'')`,
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

const quarantineRepFloor = 0.2

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

func (s *Store) PeerResultKey(ctx context.Context, taskID uuid.UUID) (peerResult string, peerSupplier uuid.UUID, peerEngine, peerBuildHash, peerBuildIdentityPolicy, peerSHA256 string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(p.result_ref,''),COALESCE(vw.supplier_id,p.execution_supplier_id),
		        COALESCE(vw.input_snapshot->>'engine',p.execution_engine,''),
		        COALESCE(vw.input_snapshot->>'build_hash',p.execution_build_hash,''),
		        COALESCE(vw.input_snapshot->>'build_identity_policy',p.execution_build_identity_policy,''),
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
	).Scan(&peerResult, &peerSupplier, &peerEngine, &peerBuildHash, &peerBuildIdentityPolicy, &peerSHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, "", "", "", "", errNotFound
	}
	return peerResult, peerSupplier, peerEngine, peerBuildHash, peerBuildIdentityPolicy, peerSHA256, err
}

func (s *Store) PeerSealedResult(ctx context.Context, taskID uuid.UUID) (VerificationArtifact, uuid.UUID, string, string, string, error) {
	var artifact VerificationArtifact
	var supplier uuid.UUID
	var engine, build, buildIdentityPolicy string
	err := s.pool.QueryRow(ctx, `
		SELECT vw.artifact_key,vw.artifact_sha256,vw.artifact_bytes,
		       vw.supplier_id,COALESCE(vw.input_snapshot->>'engine',''),
		       COALESCE(vw.input_snapshot->>'build_hash',''),
		       COALESCE(vw.input_snapshot->>'build_identity_policy','')
		  FROM tasks t
		  JOIN tasks p ON p.job_id=t.job_id AND p.input_ref=t.input_ref
		              AND p.id<>t.id AND p.status='complete'
		  JOIN verification_work vw ON vw.task_id=p.id AND vw.attempt=p.retry_count
		 WHERE t.id=$1 AND vw.status='terminal'
		   AND p.result_ref=vw.artifact_key AND p.result_sha256=vw.artifact_sha256
		 ORDER BY p.completed_at ASC,p.id LIMIT 1`, taskID).
		Scan(&artifact.Key, &artifact.SHA256, &artifact.Bytes, &supplier, &engine, &build, &buildIdentityPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact, uuid.Nil, "", "", "", errNotFound
	}
	return artifact, supplier, engine, build, buildIdentityPolicy, err
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

// lockEconomicReserveTx is the first economic row lock in every dynamic
// obligation insertion. When a peer is pinned, the writer has already locked
// that worker; all paths then retain worker -> reserve -> task -> job so they do
// not deadlock Claim/Start/Upsert or one another.
func lockEconomicReserveTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) error {
	var locked uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT job_id FROM job_economic_reserves WHERE job_id=$1 FOR UPDATE`,
		jobID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEconomicReserveExhausted
	}
	return err
}

// frozenTaskEconomics is the per-task freeze returned by consumeEconomicReserveTx.
// Nanos are non-nil only for exact-policy plans; legacy rows keep floats alone.
type frozenTaskEconomics struct {
	BuyerChargeUSD      float64
	SupplierPayoutUSD   float64
	BuyerChargeNanos    *int64
	SupplierPayoutNanos *int64
}

func consumeEconomicReserveTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (frozen frozenTaskEconomics, err error) {
	// Denormalized columns must equal the digest-bound plan_json before any
	// money is returned from them. Fail closed on mismatch.
	if _, _, aerr := assertDenormalizedEconomicPlanMoney(ctx, tx, jobID); aerr != nil {
		return frozenTaskEconomics{}, aerr
	}
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
		   AND (NOT j.firm_quote
		        OR j.firm_quote_max_usd IS NULL
		        OR p.buyer_charge_per_task_usd * (p.initial_task_count+r.consumed_tasks+1)
		             + p.sla_premium_usd <= j.firm_quote_max_usd)
		RETURNING p.buyer_charge_per_task_usd::float8,p.supplier_payout_per_task_usd::float8,
		          p.buyer_charge_per_task_nanos,p.supplier_payout_per_task_nanos`, jobID).
		Scan(&frozen.BuyerChargeUSD, &frozen.SupplierPayoutUSD,
			&frozen.BuyerChargeNanos, &frozen.SupplierPayoutNanos)
	if errors.Is(err, pgx.ErrNoRows) {
		return frozenTaskEconomics{}, ErrEconomicReserveExhausted
	}
	return frozen, err
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
	ID                     string
	Family                 string
	Quant                  string
	Kind                   string
	Dim                    int
	JobType                string
	PricePer1K             float64
	ReferencePricePer1K    float64
	PriceReferenceCurrency string
	PriceCurrency          string
	MinMemoryGB            float32
	HFRepo                 string
}

func (s *Store) ListModels(ctx context.Context) ([]ModelRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, COALESCE(family,''), COALESCE(quant,''), COALESCE(kind,''),
		        COALESCE(dim,0), COALESCE(job_type,''),
		        COALESCE(price_per_1k,0),COALESCE(price_reference_per_1k,0),
		        COALESCE(price_reference_currency,''),COALESCE(price_currency,''),
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
			&m.PricePer1K, &m.ReferencePricePer1K, &m.PriceReferenceCurrency,
			&m.PriceCurrency, &m.MinMemoryGB, &m.HFRepo); err != nil {
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
		        COALESCE(price_per_1k,0),COALESCE(price_reference_per_1k,0),
		        COALESCE(price_reference_currency,''),COALESCE(price_currency,''),
		        COALESCE(min_memory_gb,0), COALESCE(hf_repo,'')
		 FROM models WHERE id = $1`, id,
	).Scan(&m.ID, &m.Family, &m.Quant, &m.Kind, &m.Dim, &m.JobType,
		&m.PricePer1K, &m.ReferencePricePer1K, &m.PriceReferenceCurrency,
		&m.PriceCurrency, &m.MinMemoryGB, &m.HFRepo)
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
	settleCode := SettlementCurrencyCode()
	if _, err := tx.Exec(ctx, `
		INSERT INTO supplier_minor_unit_settlements
		  (ledger_entry_id,policy,liability_microusd,cash_cents,remainder_microusd,currency)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (ledger_entry_id) DO NOTHING`,
		entryID, supplierSettlementPolicyFloorMinorUnitCarryV2, liabilityMicros, cashCents, remainderMicros, settleCode,
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
	if existingPolicy != supplierSettlementPolicyFloorMinorUnitCarryV2 ||
		existingLiability != liabilityMicros || existingCash != cashCents ||
		existingRemainder != remainderMicros || RequireSettlementCurrency(currency) != nil {
		return 0, 0, fmt.Errorf(
			"minor-unit settlement for ledger entry %s changed: policy=%s liability=%d cash=%d remainder=%d currency=%s",
			entryID, existingPolicy, existingLiability, existingCash, existingRemainder, currency)
	}
	return cashCents, remainderMicros, nil
}

// CountReversalRequired returns how many ledger rows still need external
// recovery (pending or in-flight). Non-zero means the platform must not send
// new supplier payouts.
func (s *Store) CountReversalRequired(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*)::bigint FROM ledger_entries
		 WHERE payout_status IN ('reversal_required', 'reversing')`).Scan(&n)
	return n, err
}

// DueReversal is a cash-moved liability that must be reversed at the provider.

// DueReversal is a cash-moved liability that must be reversed at the provider.
type DueReversal struct {
	ID             uuid.UUID
	SupplierID     uuid.UUID
	TransferRef    string
	PaymentIntent  string // collection PI when transfer reverse is unavailable
	RequestedCents int64
	Currency       string
	CashMoved      bool
}

// PendingReversalRefundRef returns the exact Stripe Refund already observed
// for a supplier recovery operation. It is separate from the claim query so a
// provider read can happen after the claim transaction commits, without ever
// falling back to a second POST for the same durable operation.
func (s *Store) PendingReversalRefundRef(ctx context.Context, entryID uuid.UUID) (string, error) {
	if entryID == uuid.Nil {
		return "", errors.New("reversal refund lookup requires a ledger entry")
	}
	var ref string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(reverse_ref,'')
		  FROM supplier_payout_operations
		 WHERE ledger_entry_id=$1`, entryID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	if !validStripeObjectID(ref, "re_") {
		return "", fmt.Errorf("reversal %s has a non-refund provider reference %q", entryID, ref)
	}
	return ref, nil
}

// RecordReversalRefundRef binds a Stripe Refund to the exact still-open
// recovery operation before local finalization. The reference is immutable
// once seen; a later provider response cannot replace it with a different
// refund.
func (s *Store) RecordReversalRefundRef(ctx context.Context, entryID uuid.UUID, refundID string) error {
	if entryID == uuid.Nil || !validStripeObjectID(refundID, "re_") {
		return errors.New("pending reversal refund binding has invalid identity")
	}
	refundID = strings.TrimSpace(refundID)
	tag, err := s.pool.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET reverse_ref=$2,updated_at=now()
		 WHERE ledger_entry_id=$1
		   AND status IN ('reversal_required','reversing')
		   AND (reverse_ref IS NULL OR reverse_ref=$2)`, entryID, refundID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var storedRef, status string
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(reverse_ref,''),status
		  FROM supplier_payout_operations
		 WHERE ledger_entry_id=$1`, entryID).Scan(&storedRef, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reversal operation %s is missing", entryID)
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(storedRef) == refundID &&
		(status == "reversal_required" || status == "reversing" || status == "reversed") {
		return nil
	}
	return fmt.Errorf("reversal %s has a conflicting provider refund binding", entryID)
}

// ClaimReversals claims reversal_required operations that already moved cash
// (or have a durable transfer_ref). CAS discipline matches ClaimPayout:
// SELECT … FOR UPDATE SKIP LOCKED, then status CAS reversal_required → reversing
// so a concurrent worker cannot double-drive the same row. Stale reversing rows
// older than lease are reclaimable (same pattern as RecoverStalePayoutOperations).

// ClaimReversals claims reversal_required operations that already moved cash
// (or have a durable transfer_ref). CAS discipline matches ClaimPayout:
// SELECT … FOR UPDATE SKIP LOCKED, then status CAS reversal_required → reversing
// so a concurrent worker cannot double-drive the same row. Stale reversing rows
// older than lease are reclaimable (same pattern as RecoverStalePayoutOperations).
func (s *Store) ClaimReversals(ctx context.Context, lease time.Duration, limit int) ([]DueReversal, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	now := time.Now()
	rows, err := tx.Query(ctx, `
		SELECT op.ledger_entry_id, op.supplier_id,
		       COALESCE(op.transfer_ref, le.payout_ref, ''),
		       COALESCE(f.collection_payment_intent, ''),
		       op.requested_cents, op.currency, op.cash_moved
		  FROM supplier_payout_operations op
		  JOIN ledger_entries le ON le.id = op.ledger_entry_id
		  LEFT JOIN tasks t ON t.id=le.task_id
		  LEFT JOIN supplier_payout_funding f ON f.id = op.funding_id
		 WHERE le.payout_status IN ('reversal_required', 'reversing')
		   AND op.status IN ('reversal_required', 'reversing')
		   AND (op.cash_moved OR COALESCE(op.transfer_ref, le.payout_ref, '') <> '')
		   AND (
		     (op.status = 'reversing' AND op.updated_at <= $1)
		     OR (
		       op.status = 'reversal_required'
		       AND NOT EXISTS (
		           SELECT 1 FROM disputes d
		            WHERE d.job_id=t.job_id
		              AND d.status IN ('open','no_peer','reverifying','unresolvable'))
		     )
		   )
		 ORDER BY op.updated_at, op.ledger_entry_id
		 FOR UPDATE OF op, le SKIP LOCKED
		 LIMIT $2`, now.Add(-lease), limit)
	if err != nil {
		return nil, err
	}
	var out []DueReversal
	for rows.Next() {
		var r DueReversal
		if err := rows.Scan(&r.ID, &r.SupplierID, &r.TransferRef, &r.PaymentIntent,
			&r.RequestedCents, &r.Currency, &r.CashMoved); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		ct, err := tx.Exec(ctx, `
			UPDATE supplier_payout_operations
			   SET status = 'reversing', updated_at = $2
			 WHERE ledger_entry_id = $1
			   AND status IN ('reversal_required', 'reversing')`, r.ID, now)
		if err != nil {
			return nil, err
		}
		if ct.RowsAffected() != 1 {
			return nil, fmt.Errorf("reversal claim CAS lost for operation %s", r.ID)
		}
		ct, err = tx.Exec(ctx, `
			UPDATE ledger_entries
			   SET payout_status = 'reversing'
			 WHERE id = $1
			   AND payout_status IN ('reversal_required', 'reversing')`, r.ID)
		if err != nil {
			return nil, err
		}
		if ct.RowsAffected() != 1 {
			return nil, fmt.Errorf("reversal claim CAS lost for ledger %s", r.ID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// FinalizeReversal records a successful provider recovery. Requires the row to
// still be reversing (CAS) so a retry after terminal success is inert under the
// same reverse_ref.

// FinalizeReversal records a successful provider recovery. Requires the row to
// still be reversing (CAS) so a retry after terminal success is inert under the
// same reverse_ref.
func (s *Store) FinalizeReversal(ctx context.Context, entryID uuid.UUID, result ReversalResult) (string, error) {
	result.Ref = strings.TrimSpace(result.Ref)
	if err := validateReversalResult(result); err != nil {
		return "", fmt.Errorf("reversal result refused: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var ledgerStatus, opStatus, currency string
	var requested int64
	var reverseRef *string
	err = tx.QueryRow(ctx, `
		SELECT le.payout_status, op.status, op.requested_cents, op.currency, op.reverse_ref
		  FROM ledger_entries le
		  JOIN supplier_payout_operations op ON op.ledger_entry_id = le.id
		 WHERE le.id = $1
		 FOR UPDATE OF le, op`, entryID).
		Scan(&ledgerStatus, &opStatus, &requested, &currency, &reverseRef)
	if err != nil {
		return "", err
	}
	if ledgerStatus == PayoutReversed && opStatus == PayoutReversed {
		if reverseRef != nil && *reverseRef == result.Ref {
			return PayoutReversed, tx.Commit(ctx)
		}
		if reverseRef != nil && *reverseRef != "" && *reverseRef != result.Ref {
			return "", fmt.Errorf("payout %s already reversed under ref %s", entryID, *reverseRef)
		}
		// Terminal without matching ref — still inert if we re-drive the same outcome.
		return PayoutReversed, tx.Commit(ctx)
	}
	if ledgerStatus != PayoutReversing || opStatus != PayoutReversing {
		return "", fmt.Errorf("payout %s cannot reverse from ledger=%s operation=%s",
			entryID, ledgerStatus, opStatus)
	}
	if reverseRef != nil && strings.TrimSpace(*reverseRef) != "" && strings.TrimSpace(*reverseRef) != result.Ref {
		return "", fmt.Errorf("payout %s already observed provider recovery ref %s, got %s",
			entryID, strings.TrimSpace(*reverseRef), result.Ref)
	}
	if result.Cents != requested || result.Currency != currency {
		return "", fmt.Errorf(
			"payout %s reversal mismatch: requested=%d %s recovered=%d %s",
			entryID, requested, currency, result.Cents, result.Currency)
	}
	ct, err := tx.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET status = 'reversed', reverse_ref = $2, last_error = NULL,
		       outcome_unknown = false, updated_at = now()
		 WHERE ledger_entry_id = $1 AND status = 'reversing'`,
		entryID, result.Ref)
	if err != nil {
		return "", err
	}
	if ct.RowsAffected() != 1 {
		return "", fmt.Errorf("payout %s reverse CAS lost on operation row", entryID)
	}
	ct, err = tx.Exec(ctx, `
		UPDATE ledger_entries
		   SET payout_status = 'reversed'
		 WHERE id = $1 AND payout_status = 'reversing'`, entryID)
	if err != nil {
		return "", err
	}
	if ct.RowsAffected() != 1 {
		return "", fmt.Errorf("payout %s reverse CAS lost on ledger row", entryID)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return PayoutReversed, nil
}

// MarkReversalFailed returns the row to reversal_required with a durable error
// so the global payout pause remains engaged and operators can page on the metric.

// MarkReversalFailed returns the row to reversal_required with a durable error
// so the global payout pause remains engaged and operators can page on the metric.
func (s *Store) MarkReversalFailed(ctx context.Context, entryID uuid.UUID, cause error) error {
	errText := "provider reversal failed"
	if cause != nil {
		errText = truncate(cause.Error(), 500)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var ledgerStatus, opStatus string
	if err := tx.QueryRow(ctx, `
		SELECT le.payout_status, op.status
		  FROM ledger_entries le
		  JOIN supplier_payout_operations op ON op.ledger_entry_id = le.id
		 WHERE le.id = $1
		 FOR UPDATE OF le, op`, entryID).Scan(&ledgerStatus, &opStatus); err != nil {
		return err
	}
	if ledgerStatus == PayoutReversed || opStatus == PayoutReversed {
		return tx.Commit(ctx)
	}
	if ledgerStatus != PayoutReversing && ledgerStatus != PayoutReversalRequired {
		return fmt.Errorf("payout %s cannot record reverse failure from ledger=%s operation=%s",
			entryID, ledgerStatus, opStatus)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET status = 'reversal_required', last_error = $2, updated_at = now()
		 WHERE ledger_entry_id = $1
		   AND status IN ('reversing', 'reversal_required')`,
		entryID, errText); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ledger_entries
		   SET payout_status = 'reversal_required'
		 WHERE id = $1
		   AND payout_status IN ('reversing', 'reversal_required')`, entryID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RecordVerificationEvent(ctx context.Context, jobID, taskID, supplierID uuid.UUID, kind string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO verification_events (job_id, task_id, supplier_id, kind)
		 VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		jobID, nullUUID(taskID), nullUUID(supplierID), kind)
	return err
}

const (
	disputeFilingWindow   = 7 * 24 * time.Hour
	maxDisputeReasonRunes = 1000
)

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

type DeadClaim struct {
	TaskID     uuid.UUID
	JobID      uuid.UUID
	WorkerID   uuid.UUID
	SupplierID uuid.UUID // zero when the worker row has no supplier (never docked)
	JobType    string
}

func (s *Store) RecordEtaCalibration(
	ctx context.Context, jobID uuid.UUID,
) (rawPredicted, quotedPredicted, realized int, err error) {
	// input_depth_band is derived from the job's frozen compute-plan JSON so
	// calibration writes share the same authority as quotes and receipts.
	err = s.pool.QueryRow(ctx,
		`WITH source AS (
		   SELECT id, job_type, tier, COALESCE(model_ref,'') AS model_ref,
		          COALESCE(eta_secs_raw, eta_secs) AS raw_predicted_secs,
		          eta_secs AS quoted_predicted_secs,
		          GREATEST(0, EXTRACT(EPOCH FROM (now() - created_at)))::int AS realized_secs,
		          CASE
		            WHEN compute_plan->'input_depth_profile'->>'p90_depth_band'
		                 IN ('short','medium','long')
		            THEN compute_plan->'input_depth_profile'->>'p90_depth_band'
		            ELSE NULL
		          END AS input_depth_band
		     FROM jobs
		    WHERE id = $1 AND COALESCE(eta_secs_raw, eta_secs, 0) > 0
		 ), inserted AS (
		   INSERT INTO eta_calibration
		     (job_id, job_type, tier, model_ref, input_depth_band, phase,
		      predicted_secs, realized_secs)
		   SELECT id, job_type, tier, model_ref, input_depth_band, 'total',
		          raw_predicted_secs, realized_secs
		     FROM source
		   ON CONFLICT (job_id, phase) WHERE job_id IS NOT NULL DO NOTHING
		   RETURNING job_id, predicted_secs, realized_secs
		 )
		 SELECT i.predicted_secs, s.quoted_predicted_secs, i.realized_secs
		   FROM inserted i JOIN source s ON s.id = i.job_id`,
		jobID).Scan(&rawPredicted, &quotedPredicted, &realized)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, nil // no ETA prediction (or already recorded)  -  nothing to calibrate
	}
	return rawPredicted, quotedPredicted, realized, err
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

type PrimaryResult struct {
	ChunkIndex int
	ResultRef  string
	Artifact   *VerificationArtifact
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
