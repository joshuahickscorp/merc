package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	executionEnvelopeAuthorityVersion = 1
	executionEnvelopeRoundingPolicy   = "execution-envelope-nanos-v1"
	// Hard bounds so a single grant cannot pin buyer funds forever or invent
	// unbounded request fan-out.
	executionEnvelopeMaxTTL       = 24 * time.Hour
	executionEnvelopeMinTTL       = 30 * time.Second
	executionEnvelopeMaxRequests  = 100_000
	executionEnvelopeMaxTokensCap = 1_000_000_000
)

var (
	errEnvelopeNotFound            = errors.New("execution envelope not found")
	errEnvelopeExpired             = errors.New("execution envelope is expired or closed")
	errEnvelopeInsufficient        = errors.New("execution envelope lacks remaining capacity")
	errEnvelopeCeilingExceeded     = errors.New("request exceeds envelope per-request ceiling")
	errEnvelopeScopeMismatch       = errors.New("request is outside the envelope runtime scope")
	errEnvelopeIdempotencyConflict = errors.New("envelope spend idempotency key was already used for a different request")
	errEnvelopeSpendNotFound       = errors.New("execution envelope spend not found")
)

// ExecutionEnvelopeAuthority is the immutable grant document bound at create.
// It is an authority object in the same sense as RealtimePricingAuthority:
// versioned, currency-bound, validated closed, and content-addressed. Per-request
// settlement still freezes a realtime PricingDecision; the envelope only
// amortizes buyer-funding reservation, never redefines supplier entitlement.
type ExecutionEnvelopeAuthority struct {
	Version                    int    `json:"version"`
	Currency                   string `json:"currency"`
	BuyerID                    string `json:"buyer_id"`
	CapNanos                   int64  `json:"cap_nanos"`
	MaxRequests                int64  `json:"max_requests"`
	MaxTokens                  int64  `json:"max_tokens"`
	PerRequestCeilingNanos     int64  `json:"per_request_ceiling_nanos"`
	RuntimeProfileID           string `json:"runtime_profile_id"`
	RuntimeProfileSHA256       string `json:"runtime_profile_sha256"`
	ModelAlias                 string `json:"model_alias"`
	BuyerInputNanosPerMillion  int64  `json:"buyer_input_nanos_per_million"`
	BuyerOutputNanosPerMillion int64  `json:"buyer_output_nanos_per_million"`
	ExpiresAtRFC3339           string `json:"expires_at"`
	RoundingPolicy             string `json:"rounding_policy"`
}

// ExecutionEnvelopeCreateRequest is the buyer-facing create body.
type ExecutionEnvelopeCreateRequest struct {
	RuntimeProfileID       string `json:"runtime_profile_id"`
	CapNanos               int64  `json:"cap_nanos"`
	MaxRequests            int64  `json:"max_requests"`
	MaxTokens              int64  `json:"max_tokens"`
	PerRequestCeilingNanos int64  `json:"per_request_ceiling_nanos"`
	TTLSeconds             int64  `json:"ttl_seconds"`
}

// ExecutionEnvelope is the durable grant row returned to the buyer.
type ExecutionEnvelope struct {
	ID                     uuid.UUID                  `json:"id"`
	BuyerID                uuid.UUID                  `json:"buyer_id"`
	Currency               string                     `json:"currency"`
	CapNanos               int64                      `json:"cap_nanos"`
	SpentNanos             int64                      `json:"spent_nanos"`
	ReservedNanos          int64                      `json:"reserved_nanos"`
	MaxRequests            int64                      `json:"max_requests"`
	RequestCount           int64                      `json:"request_count"`
	MaxTokens              int64                      `json:"max_tokens"`
	TokenCount             int64                      `json:"token_count"`
	PerRequestCeilingNanos int64                      `json:"per_request_ceiling_nanos"`
	RuntimeProfileID       string                     `json:"runtime_profile_id"`
	RuntimeProfileSHA256   string                     `json:"runtime_profile_sha256"`
	ModelAlias             string                     `json:"model_alias"`
	ExpiresAt              time.Time                  `json:"expires_at"`
	State                  string                     `json:"state"`
	Version                int64                      `json:"version"`
	Authority              ExecutionEnvelopeAuthority `json:"authority"`
	AuthoritySHA256        string                     `json:"authority_sha256"`
	CreatedAt              time.Time                  `json:"created_at"`
	ClosedAt               *time.Time                 `json:"closed_at,omitempty"`
}

// ExecutionEnvelopeSpend is one idempotent hold/capture against an envelope.
type ExecutionEnvelopeSpend struct {
	ID             uuid.UUID  `json:"id"`
	EnvelopeID     uuid.UUID  `json:"envelope_id"`
	BuyerID        uuid.UUID  `json:"buyer_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	RequestID      string     `json:"request_id"`
	ContractID     *uuid.UUID `json:"contract_id,omitempty"`
	State          string     `json:"state"`
	ReservedNanos  int64      `json:"reserved_nanos"`
	CapturedNanos  int64      `json:"captured_nanos"`
	ReservedTokens int64      `json:"reserved_tokens"`
	CapturedTokens int64      `json:"captured_tokens"`
	CreatedAt      time.Time  `json:"created_at"`
	FinalizedAt    *time.Time `json:"finalized_at,omitempty"`
}

func validateExecutionEnvelopeAuthority(a ExecutionEnvelopeAuthority) error {
	if a.Version != executionEnvelopeAuthorityVersion ||
		a.RoundingPolicy != executionEnvelopeRoundingPolicy {
		return errors.New("execution envelope authority has unsupported version or rounding policy")
	}
	if err := RequireSettlementCurrency(a.Currency); err != nil {
		return err
	}
	if _, err := uuid.Parse(a.BuyerID); err != nil {
		return errors.New("execution envelope authority lacks a valid buyer id")
	}
	if a.CapNanos <= 0 || a.PerRequestCeilingNanos <= 0 ||
		a.PerRequestCeilingNanos > a.CapNanos ||
		a.MaxRequests < 1 || a.MaxRequests > executionEnvelopeMaxRequests ||
		a.MaxTokens < 0 || a.MaxTokens > executionEnvelopeMaxTokensCap {
		return errors.New("execution envelope authority has invalid spend or request bounds")
	}
	if a.RuntimeProfileID == "" || !validSHA256(a.RuntimeProfileSHA256) ||
		strings.TrimSpace(a.ModelAlias) == "" {
		return errors.New("execution envelope authority lacks runtime scope")
	}
	if a.BuyerInputNanosPerMillion <= 0 || a.BuyerOutputNanosPerMillion <= 0 {
		return errors.New("execution envelope authority lacks positive buyer token rates")
	}
	if _, err := time.Parse(time.RFC3339Nano, a.ExpiresAtRFC3339); err != nil {
		if _, err2 := time.Parse(time.RFC3339, a.ExpiresAtRFC3339); err2 != nil {
			return errors.New("execution envelope authority has invalid expiry")
		}
	}
	return nil
}

func executionEnvelopeAuthorityDigest(a ExecutionEnvelopeAuthority) (string, error) {
	if err := validateExecutionEnvelopeAuthority(a); err != nil {
		return "", err
	}
	// Canonical JSON via encoding/json with struct field order is stable for this
	// type; the digest is bound at insert and re-checked on load.
	raw, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ceilNanosToMicros converts an exact nano liability to the integer micros that
// must be *held* against micros-granular available funds (balance_micros, free
// credit). Direction is UP so a sub-micro remainder cannot erase a positive
// hold — the platform absorbs one extra micro; the buyer must not under-fund.
//
// This is deliberately not LedgerMicrosFromNanos: that helper projects a
// completed amount to the legacy ledger with to-nearest (half-up) rounding and
// is correct for display/settlement presentation. Using it for a funding hold
// under-holds remainders below half a micro (e.g. 1_000_050 nanos → 1000 micros
// projected, but 1001 micros held).
func ceilNanosToMicros(nanos int64) int64 {
	if nanos <= 0 {
		return 0
	}
	return (nanos + NanosPerMicro - 1) / NanosPerMicro
}

// CreateExecutionEnvelope reserves cap_nanos against the buyer's available
// funding once, freezes scope + ceilings, and returns an ACTIVE grant. Two
// envelopes cannot jointly exceed the buyer's funds because create reuses the
// same evaluateRealtimeBuyerFunding critical section that serialises concurrent
// realtime admissions (including other envelope creates).
func (s *Store) CreateExecutionEnvelope(ctx context.Context, buyerID uuid.UUID, req ExecutionEnvelopeCreateRequest) (ExecutionEnvelope, error) {
	if buyerID == uuid.Nil {
		return ExecutionEnvelope{}, errors.New("execution envelope requires a buyer")
	}
	profile, ok := vllmProfileByID(strings.TrimSpace(req.RuntimeProfileID))
	if !ok {
		return ExecutionEnvelope{}, errors.New("unknown runtime profile for execution envelope")
	}
	if err := validateVLLMRuntimeProfile(profile); err != nil {
		return ExecutionEnvelope{}, err
	}
	currency, err := SettlementCurrency()
	if err != nil {
		return ExecutionEnvelope{}, err
	}
	if req.CapNanos <= 0 || req.PerRequestCeilingNanos <= 0 ||
		req.PerRequestCeilingNanos > req.CapNanos ||
		req.MaxRequests < 1 || req.MaxRequests > executionEnvelopeMaxRequests ||
		req.MaxTokens < 0 || req.MaxTokens > executionEnvelopeMaxTokensCap {
		return ExecutionEnvelope{}, errors.New("execution envelope bounds are invalid")
	}
	if req.TTLSeconds < int64(executionEnvelopeMinTTL/time.Second) ||
		req.TTLSeconds > int64(executionEnvelopeMaxTTL/time.Second) {
		return ExecutionEnvelope{}, fmt.Errorf("execution envelope ttl must be between %s and %s",
			executionEnvelopeMinTTL, executionEnvelopeMaxTTL)
	}
	buyerIn, err := nanoRatePerMillionFromFloat(profile.BuyerInputUSDPerMillionTokens)
	if err != nil {
		return ExecutionEnvelope{}, err
	}
	buyerOut, err := nanoRatePerMillionFromFloat(profile.BuyerOutputUSDPerMillionTokens)
	if err != nil {
		return ExecutionEnvelope{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(req.TTLSeconds) * time.Second)
	authority := ExecutionEnvelopeAuthority{
		Version:                    executionEnvelopeAuthorityVersion,
		Currency:                   currency.Code(),
		BuyerID:                    buyerID.String(),
		CapNanos:                   req.CapNanos,
		MaxRequests:                req.MaxRequests,
		MaxTokens:                  req.MaxTokens,
		PerRequestCeilingNanos:     req.PerRequestCeilingNanos,
		RuntimeProfileID:           profile.RuntimeProfileID,
		RuntimeProfileSHA256:       profile.ProfileSHA256,
		ModelAlias:                 profile.ModelAlias,
		BuyerInputNanosPerMillion:  int64(buyerIn),
		BuyerOutputNanosPerMillion: int64(buyerOut),
		ExpiresAtRFC3339:           expiresAt.Format(time.RFC3339Nano),
		RoundingPolicy:             executionEnvelopeRoundingPolicy,
	}
	if err := validateExecutionEnvelopeAuthority(authority); err != nil {
		return ExecutionEnvelope{}, err
	}
	digest, err := executionEnvelopeAuthorityDigest(authority)
	if err != nil {
		return ExecutionEnvelope{}, err
	}
	authorityJSON, err := json.Marshal(authority)
	if err != nil {
		return ExecutionEnvelope{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ExecutionEnvelope{}, err
	}
	defer tx.Rollback(ctx)

	// Full durable funding check, once. Cap is exact nanos; evaluate ceilings
	// to whole micros internally so both envelope and non-envelope holds share
	// the same nano→micro direction.
	if err := evaluateRealtimeBuyerFunding(ctx, tx, buyerID, req.CapNanos); err != nil {
		return ExecutionEnvelope{}, err
	}

	id := uuid.New()
	_, err = tx.Exec(ctx, `
		INSERT INTO execution_envelopes
		 (id,buyer_id,currency,cap_nanos,spent_nanos,reserved_nanos,max_requests,request_count,
		  max_tokens,token_count,per_request_ceiling_nanos,runtime_profile_id,runtime_profile_sha256,
		  model_alias,expires_at,state,version,authority,authority_sha256,created_at,updated_at)
		VALUES ($1,$2,$3,$4,0,0,$5,0,$6,0,$7,$8,$9,$10,$11,'ACTIVE',0,$12::jsonb,$13,$14,$14)`,
		id, buyerID, currency.Code(), req.CapNanos, req.MaxRequests, req.MaxTokens,
		req.PerRequestCeilingNanos, profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.ModelAlias, expiresAt, authorityJSON, digest, now)
	if err != nil {
		return ExecutionEnvelope{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO execution_envelope_events (envelope_id,kind,detail)
		VALUES ($1,'CREATED',$2::jsonb)`, id, string(authorityJSON)); err != nil {
		return ExecutionEnvelope{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ExecutionEnvelope{}, err
	}
	return ExecutionEnvelope{
		ID: id, BuyerID: buyerID, Currency: currency.Code(),
		CapNanos: req.CapNanos, MaxRequests: req.MaxRequests, MaxTokens: req.MaxTokens,
		PerRequestCeilingNanos: req.PerRequestCeilingNanos,
		RuntimeProfileID:       profile.RuntimeProfileID,
		RuntimeProfileSHA256:   profile.ProfileSHA256,
		ModelAlias:             profile.ModelAlias,
		ExpiresAt:              expiresAt, State: "ACTIVE", Version: 0,
		Authority: authority, AuthoritySHA256: digest, CreatedAt: now,
	}, nil
}

func (s *Store) GetExecutionEnvelope(ctx context.Context, buyerID, envelopeID uuid.UUID) (ExecutionEnvelope, error) {
	env, err := scanExecutionEnvelope(s.pool.QueryRow(ctx, `
		SELECT `+executionEnvelopeColumns+`
		  FROM execution_envelopes WHERE id=$1 AND buyer_id=$2`, envelopeID, buyerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ExecutionEnvelope{}, errEnvelopeNotFound
	}
	return env, err
}

const executionEnvelopeColumns = `
	id,buyer_id,currency,cap_nanos,spent_nanos,reserved_nanos,max_requests,request_count,
	max_tokens,token_count,per_request_ceiling_nanos,runtime_profile_id,runtime_profile_sha256,
	model_alias,expires_at,state,version,authority,authority_sha256,created_at,closed_at`

func scanExecutionEnvelope(row pgx.Row) (ExecutionEnvelope, error) {
	var (
		env          ExecutionEnvelope
		authorityRaw []byte
		closedAt     *time.Time
	)
	err := row.Scan(
		&env.ID, &env.BuyerID, &env.Currency, &env.CapNanos, &env.SpentNanos, &env.ReservedNanos,
		&env.MaxRequests, &env.RequestCount, &env.MaxTokens, &env.TokenCount,
		&env.PerRequestCeilingNanos, &env.RuntimeProfileID, &env.RuntimeProfileSHA256,
		&env.ModelAlias, &env.ExpiresAt, &env.State, &env.Version,
		&authorityRaw, &env.AuthoritySHA256, &env.CreatedAt, &closedAt,
	)
	if err != nil {
		return ExecutionEnvelope{}, err
	}
	if err := json.Unmarshal(authorityRaw, &env.Authority); err != nil {
		return ExecutionEnvelope{}, err
	}
	if err := validateExecutionEnvelopeAuthority(env.Authority); err != nil {
		return ExecutionEnvelope{}, err
	}
	digest, err := executionEnvelopeAuthorityDigest(env.Authority)
	if err != nil || digest != env.AuthoritySHA256 {
		return ExecutionEnvelope{}, errors.New("execution envelope authority digest mismatch")
	}
	env.ClosedAt = closedAt
	return env, nil
}

// reserveEnvelopeSpendTx holds needNanos against the envelope in one atomic
// UPDATE. No buyer advisory lock; no transaction-scoped lock is held across an
// upstream call — the row lock lives only for this statement. The spend row
// makes the hold durable and idempotent under retries.
func reserveEnvelopeSpendTx(
	ctx context.Context, tx pgx.Tx,
	envelopeID, buyerID uuid.UUID,
	needNanos, reserveTokens int64,
	idempotencyKey, requestID string,
	profile VLLMRuntimeProfile,
) (ExecutionEnvelopeSpend, error) {
	if envelopeID == uuid.Nil || buyerID == uuid.Nil || needNanos <= 0 {
		return ExecutionEnvelopeSpend{}, errors.New("envelope spend requires envelope, buyer, and positive nanos")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return ExecutionEnvelopeSpend{}, errors.New("envelope spend requires an idempotency key")
	}
	if reserveTokens < 0 {
		return ExecutionEnvelopeSpend{}, errors.New("envelope reserved tokens cannot be negative")
	}

	// Idempotent replay: same key returns the prior spend without re-debiting.
	var existing ExecutionEnvelopeSpend
	err := scanEnvelopeSpend(tx.QueryRow(ctx, `
		SELECT `+executionEnvelopeSpendColumns+`
		  FROM execution_envelope_spends
		 WHERE envelope_id=$1 AND idempotency_key=$2`, envelopeID, idempotencyKey), &existing)
	if err == nil {
		if existing.BuyerID != buyerID || existing.ReservedNanos != needNanos {
			return ExecutionEnvelopeSpend{}, errEnvelopeIdempotencyConflict
		}
		if requestID != "" && existing.RequestID != "" && existing.RequestID != requestID {
			return ExecutionEnvelopeSpend{}, errEnvelopeIdempotencyConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ExecutionEnvelopeSpend{}, err
	}

	// Single-statement capacity claim. Contending spends serialize on this row
	// for the duration of the UPDATE only; a failed WHERE means the cap held.
	var (
		versionAfter  int64
		reservedAfter int64
		spentAfter    int64
		capNanos      int64
		state         string
		expiresAt     time.Time
		profileID     string
		profileSHA    string
		ceiling       int64
	)
	err = tx.QueryRow(ctx, `
		UPDATE execution_envelopes
		   SET reserved_nanos = reserved_nanos + $2,
		       request_count = request_count + 1,
		       token_count = token_count + $3,
		       version = version + 1,
		       updated_at = now()
		 WHERE id = $1
		   AND buyer_id = $4
		   AND state = 'ACTIVE'
		   AND expires_at > now()
		   AND $2 > 0
		   AND $2 <= per_request_ceiling_nanos
		   AND spent_nanos + reserved_nanos + $2 <= cap_nanos
		   AND request_count + 1 <= max_requests
		   AND (max_tokens = 0 OR token_count + $3 <= max_tokens)
		   AND runtime_profile_id = $5
		   AND runtime_profile_sha256 = $6
		 RETURNING version, reserved_nanos, spent_nanos, cap_nanos, state, expires_at,
		           runtime_profile_id, runtime_profile_sha256, per_request_ceiling_nanos`,
		envelopeID, needNanos, reserveTokens, buyerID,
		profile.RuntimeProfileID, profile.ProfileSHA256,
	).Scan(&versionAfter, &reservedAfter, &spentAfter, &capNanos, &state, &expiresAt,
		&profileID, &profileSHA, &ceiling)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish scope / ceiling / expiry / capacity for the caller.
		var (
			gotBuyer                                   uuid.UUID
			gotState, gotProfileID, gotProfileSHA      string
			gotCeiling, gotCap, gotSpent, gotReserved  int64
			gotMaxReq, gotReqCount, gotMaxTok, gotToks int64
			gotExpires                                 time.Time
		)
		probe := tx.QueryRow(ctx, `
			SELECT buyer_id,state,expires_at,per_request_ceiling_nanos,cap_nanos,spent_nanos,reserved_nanos,
			       max_requests,request_count,max_tokens,token_count,runtime_profile_id,runtime_profile_sha256
			  FROM execution_envelopes WHERE id=$1`, envelopeID)
		if perr := probe.Scan(&gotBuyer, &gotState, &gotExpires, &gotCeiling, &gotCap, &gotSpent, &gotReserved,
			&gotMaxReq, &gotReqCount, &gotMaxTok, &gotToks, &gotProfileID, &gotProfileSHA); perr != nil {
			if errors.Is(perr, pgx.ErrNoRows) {
				return ExecutionEnvelopeSpend{}, errEnvelopeNotFound
			}
			return ExecutionEnvelopeSpend{}, perr
		}
		if gotBuyer != buyerID {
			return ExecutionEnvelopeSpend{}, errEnvelopeNotFound
		}
		if gotState != "ACTIVE" || !gotExpires.After(time.Now().UTC()) {
			return ExecutionEnvelopeSpend{}, errEnvelopeExpired
		}
		if gotProfileID != profile.RuntimeProfileID || gotProfileSHA != profile.ProfileSHA256 {
			return ExecutionEnvelopeSpend{}, errEnvelopeScopeMismatch
		}
		if needNanos > gotCeiling {
			return ExecutionEnvelopeSpend{}, errEnvelopeCeilingExceeded
		}
		return ExecutionEnvelopeSpend{}, errEnvelopeInsufficient
	}
	if err != nil {
		return ExecutionEnvelopeSpend{}, err
	}
	_ = versionAfter
	_ = reservedAfter
	_ = spentAfter
	_ = capNanos
	_ = state
	_ = expiresAt
	_ = profileID
	_ = profileSHA
	_ = ceiling

	spendID := uuid.New()
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		INSERT INTO execution_envelope_spends
		 (id,envelope_id,buyer_id,idempotency_key,request_id,contract_id,state,
		  reserved_nanos,captured_nanos,reserved_tokens,captured_tokens,created_at)
		VALUES ($1,$2,$3,$4,$5,NULL,'RESERVED',$6,0,$7,0,$8)`,
		spendID, envelopeID, buyerID, idempotencyKey, requestID, needNanos, reserveTokens, now)
	if err != nil {
		// Unique race: another writer inserted the same key between our SELECT
		// and INSERT. Re-read and treat as idempotent replay; reverse our
		// counter bump so the envelope is not double-charged.
		if isUniqueViolation(err) {
			if _, relErr := tx.Exec(ctx, `
				UPDATE execution_envelopes
				   SET reserved_nanos = reserved_nanos - $2,
				       request_count = request_count - 1,
				       token_count = token_count - $3,
				       version = version + 1,
				       updated_at = now()
				 WHERE id=$1 AND reserved_nanos >= $2 AND request_count >= 1
				   AND token_count >= $3`, envelopeID, needNanos, reserveTokens); relErr != nil {
				return ExecutionEnvelopeSpend{}, relErr
			}
			if err := scanEnvelopeSpend(tx.QueryRow(ctx, `
				SELECT `+executionEnvelopeSpendColumns+`
				  FROM execution_envelope_spends
				 WHERE envelope_id=$1 AND idempotency_key=$2`, envelopeID, idempotencyKey), &existing); err != nil {
				return ExecutionEnvelopeSpend{}, err
			}
			if existing.BuyerID != buyerID || existing.ReservedNanos != needNanos {
				return ExecutionEnvelopeSpend{}, errEnvelopeIdempotencyConflict
			}
			return existing, nil
		}
		return ExecutionEnvelopeSpend{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO execution_envelope_events (envelope_id,kind,detail)
		VALUES ($1,'SPEND_RESERVED',$2::jsonb)`, envelopeID, fmt.Sprintf(
		`{"spend_id":%q,"reserved_nanos":%d,"idempotency_key":%q}`, spendID.String(), needNanos, idempotencyKey)); err != nil {
		return ExecutionEnvelopeSpend{}, err
	}
	return ExecutionEnvelopeSpend{
		ID: spendID, EnvelopeID: envelopeID, BuyerID: buyerID,
		IdempotencyKey: idempotencyKey, RequestID: requestID, State: "RESERVED",
		ReservedNanos: needNanos, ReservedTokens: reserveTokens, CreatedAt: now,
	}, nil
}

const executionEnvelopeSpendColumns = `
	id,envelope_id,buyer_id,idempotency_key,request_id,contract_id,state,
	reserved_nanos,captured_nanos,reserved_tokens,captured_tokens,created_at,finalized_at`

func scanEnvelopeSpend(row pgx.Row, out *ExecutionEnvelopeSpend) error {
	return row.Scan(
		&out.ID, &out.EnvelopeID, &out.BuyerID, &out.IdempotencyKey, &out.RequestID,
		&out.ContractID, &out.State, &out.ReservedNanos, &out.CapturedNanos,
		&out.ReservedTokens, &out.CapturedTokens, &out.CreatedAt, &out.FinalizedAt,
	)
}

func bindEnvelopeSpendContractTx(ctx context.Context, tx pgx.Tx, spendID, contractID uuid.UUID) error {
	ct, err := tx.Exec(ctx, `
		UPDATE execution_envelope_spends
		   SET contract_id=$2
		 WHERE id=$1 AND state='RESERVED' AND contract_id IS NULL`, spendID, contractID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errors.New("envelope spend could not bind contract")
	}
	return nil
}

// captureEnvelopeSpendTx moves a RESERVED hold into permanent spent amount.
// captured must be <= reserved; the difference returns to the envelope's free
// capacity. Idempotent on already-CAPTURED rows with the same amount.
//
// Crash-safety direction: a RESERVED hold that survives process death continues
// to count against the envelope (and therefore the buyer's funds) until capture
// or release. That errs toward no free work for the buyer — the safe direction
// for a marketplace — rather than phantom under-charge. Recovery of FAILED/
// CANCELLED contracts, expiry, and the orphan-spend sweeper release holds that
// never delivered work.
func captureEnvelopeSpendTx(ctx context.Context, tx pgx.Tx, contractID uuid.UUID, capturedNanos, capturedTokens int64) error {
	if contractID == uuid.Nil || capturedNanos <= 0 {
		return errors.New("envelope capture requires contract and positive captured nanos")
	}
	if capturedTokens < 0 {
		return errors.New("envelope captured tokens cannot be negative")
	}
	var (
		spendID, envelopeID uuid.UUID
		state               string
		reserved, priorCap  int64
		reservedTokens      int64
	)
	err := tx.QueryRow(ctx, `
		SELECT id,envelope_id,state,reserved_nanos,captured_nanos,reserved_tokens
		  FROM execution_envelope_spends
		 WHERE contract_id=$1
		 FOR UPDATE`, contractID).
		Scan(&spendID, &envelopeID, &state, &reserved, &priorCap, &reservedTokens)
	if errors.Is(err, pgx.ErrNoRows) {
		// Contract was not envelope-funded.
		return nil
	}
	if err != nil {
		return err
	}
	if state == "CAPTURED" {
		if priorCap != capturedNanos {
			return fmt.Errorf("envelope spend already captured at %d nanos, not %d", priorCap, capturedNanos)
		}
		return nil
	}
	if state != "RESERVED" {
		return fmt.Errorf("envelope spend in state %s cannot be captured", state)
	}
	if capturedNanos > reserved {
		return fmt.Errorf("captured %d nanos exceeds reserved %d", capturedNanos, reserved)
	}
	// Tokens: release any unused reserved token budget; never allow captured
	// above the reserved token hold.
	if capturedTokens > reservedTokens {
		capturedTokens = reservedTokens
	}
	tokenRelease := reservedTokens - capturedTokens

	ct, err := tx.Exec(ctx, `
		UPDATE execution_envelopes
		   SET reserved_nanos = reserved_nanos - $2,
		       spent_nanos = spent_nanos + $3,
		       token_count = token_count - $4,
		       version = version + 1,
		       updated_at = now()
		 WHERE id=$1
		   AND reserved_nanos >= $2
		   AND spent_nanos + $3 <= cap_nanos
		   AND token_count >= $4`,
		envelopeID, reserved, capturedNanos, tokenRelease)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errors.New("envelope capture failed capacity accounting")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE execution_envelope_spends
		   SET state='CAPTURED', captured_nanos=$2, captured_tokens=$3, finalized_at=now()
		 WHERE id=$1 AND state='RESERVED'`, spendID, capturedNanos, capturedTokens); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO execution_envelope_events (envelope_id,kind,detail)
		VALUES ($1,'SPEND_CAPTURED',$2::jsonb)`, envelopeID, fmt.Sprintf(
		`{"spend_id":%q,"contract_id":%q,"captured_nanos":%d,"reserved_nanos":%d}`,
		spendID.String(), contractID.String(), capturedNanos, reserved)); err != nil {
		return err
	}
	return nil
}

// releaseEnvelopeSpendForContractTx drops a RESERVED hold when the contract
// fails, is cancelled, or is recovered without delivery. Idempotent.
func releaseEnvelopeSpendForContractTx(ctx context.Context, tx pgx.Tx, contractID uuid.UUID) error {
	if contractID == uuid.Nil {
		return nil
	}
	var (
		spendID, envelopeID uuid.UUID
		state               string
		reserved            int64
		reservedTokens      int64
	)
	err := tx.QueryRow(ctx, `
		SELECT id,envelope_id,state,reserved_nanos,reserved_tokens
		  FROM execution_envelope_spends
		 WHERE contract_id=$1
		 FOR UPDATE`, contractID).
		Scan(&spendID, &envelopeID, &state, &reserved, &reservedTokens)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "RELEASED" || state == "VOIDED" {
		return nil
	}
	if state == "CAPTURED" {
		// Already billed; failure after capture is a refund path, not a release.
		return nil
	}
	if state != "RESERVED" {
		return fmt.Errorf("envelope spend in state %s cannot be released", state)
	}
	ct, err := tx.Exec(ctx, `
		UPDATE execution_envelopes
		   SET reserved_nanos = reserved_nanos - $2,
		       token_count = token_count - $3,
		       version = version + 1,
		       updated_at = now()
		 WHERE id=$1 AND reserved_nanos >= $2 AND token_count >= $3`,
		envelopeID, reserved, reservedTokens)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return errors.New("envelope release failed capacity accounting")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE execution_envelope_spends
		   SET state='VOIDED', finalized_at=now()
		 WHERE id=$1 AND state='RESERVED'`, spendID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO execution_envelope_events (envelope_id,kind,detail)
		VALUES ($1,'SPEND_VOIDED',$2::jsonb)`, envelopeID, fmt.Sprintf(
		`{"spend_id":%q,"contract_id":%q,"released_nanos":%d}`,
		spendID.String(), contractID.String(), reserved)); err != nil {
		return err
	}
	return nil
}

// ReleaseExpiredExecutionEnvelopes marks expired ACTIVE envelopes terminal and
// returns unspent capacity to the buyer's available balance by leaving the
// open-reservation query. In-flight RESERVED spends are left alone: they still
// bind funds until the bound contract settles or is recovered. That is the
// crash-safe direction (no free work).
func (s *Store) ReleaseExpiredExecutionEnvelopes(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid envelope expiry limit")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT id FROM execution_envelopes
		 WHERE state='ACTIVE' AND expires_at <= now()
		 ORDER BY expires_at,id
		 FOR UPDATE SKIP LOCKED
		 LIMIT $1`, limit)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			UPDATE execution_envelopes
			   SET state='EXPIRED', closed_at=now(), version=version+1, updated_at=now()
			 WHERE id=$1 AND state='ACTIVE'`, id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO execution_envelope_events (envelope_id,kind,detail)
			VALUES ($1,'EXPIRED','{"reason":"wall_clock_expiry"}'::jsonb)`, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// RecoverOrphanEnvelopeSpends voids RESERVED spends whose contracts are already
// terminal without a capture, or that never bound a contract and are older than
// grace. Used after a control crash so reserved capacity converges to truth.
func (s *Store) RecoverOrphanEnvelopeSpends(ctx context.Context, grace time.Duration, limit int) (int, error) {
	if grace < 0 || limit < 1 || limit > 1000 {
		return 0, errors.New("invalid envelope orphan recovery bounds")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.envelope_id, s.reserved_nanos, s.reserved_tokens, s.contract_id
		  FROM execution_envelope_spends s
		  LEFT JOIN execution_contracts c ON c.id = s.contract_id
		 WHERE s.state = 'RESERVED'
		   AND (
		     (s.contract_id IS NULL AND s.created_at < now()-make_interval(secs=>$1::double precision))
		     OR (c.state IS NOT NULL AND c.state IN ('FAILED','CANCELLED','VERIFIED'))
		   )
		 ORDER BY s.created_at, s.id
		 FOR UPDATE OF s SKIP LOCKED
		 LIMIT $2`, grace.Seconds(), limit)
	if err != nil {
		return 0, err
	}
	type orphan struct {
		spendID, envelopeID uuid.UUID
		reserved, tokens    int64
		contractID          *uuid.UUID
	}
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.spendID, &o.envelopeID, &o.reserved, &o.tokens, &o.contractID); err != nil {
			rows.Close()
			return 0, err
		}
		orphans = append(orphans, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	released := 0
	for _, o := range orphans {
		// VERIFIED without CAPTURED is a bug-adjacent state; still do not
		// invent a capture amount — leave it for operator attention via event.
		if o.contractID != nil {
			var contractState string
			if err := tx.QueryRow(ctx, `SELECT state FROM execution_contracts WHERE id=$1`, *o.contractID).
				Scan(&contractState); err != nil {
				return 0, err
			}
			if contractState == "VERIFIED" {
				if _, err := tx.Exec(ctx, `
					INSERT INTO execution_envelope_events (envelope_id,kind,detail)
					VALUES ($1,'SPEND_VOIDED',$2::jsonb)`, o.envelopeID, fmt.Sprintf(
					`{"spend_id":%q,"contract_id":%q,"note":"verified_without_capture_left_reserved"}`,
					o.spendID.String(), o.contractID.String())); err != nil {
					return 0, err
				}
				continue
			}
		}
		ct, err := tx.Exec(ctx, `
			UPDATE execution_envelopes
			   SET reserved_nanos = reserved_nanos - $2,
			       token_count = token_count - $3,
			       version = version + 1,
			       updated_at = now()
			 WHERE id=$1 AND reserved_nanos >= $2 AND token_count >= $3`,
			o.envelopeID, o.reserved, o.tokens)
		if err != nil {
			return 0, err
		}
		if ct.RowsAffected() != 1 {
			return 0, errors.New("orphan envelope release failed capacity accounting")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE execution_envelope_spends
			   SET state='VOIDED', finalized_at=now()
			 WHERE id=$1 AND state='RESERVED'`, o.spendID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO execution_envelope_events (envelope_id,kind,detail)
			VALUES ($1,'SPEND_VOIDED',$2::jsonb)`, o.envelopeID, fmt.Sprintf(
			`{"spend_id":%q,"reason":"orphan_recovery","released_nanos":%d}`,
			o.spendID.String(), o.reserved)); err != nil {
			return 0, err
		}
		released++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return released, nil
}

// SpendExecutionEnvelope is the durable single-statement spend used by the
// hot path, exposed so tests can race the cap without full contract auth /
// capacity selection. Production authorization goes through
// AuthorizeRealtimeContract, which calls the same reserve helper inside its
// transaction and then binds a contract id.
func (s *Store) SpendExecutionEnvelope(
	ctx context.Context,
	envelopeID, buyerID uuid.UUID,
	needNanos, reserveTokens int64,
	idempotencyKey, requestID string,
	profile VLLMRuntimeProfile,
) (ExecutionEnvelopeSpend, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ExecutionEnvelopeSpend{}, err
	}
	defer tx.Rollback(ctx)
	spend, err := reserveEnvelopeSpendTx(ctx, tx, envelopeID, buyerID, needNanos, reserveTokens,
		idempotencyKey, requestID, profile)
	if err != nil {
		return ExecutionEnvelopeSpend{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ExecutionEnvelopeSpend{}, err
	}
	return spend, nil
}

// CaptureExecutionEnvelopeSpendForTest finalizes a spend bound to a synthetic
// contract id (tests only). Production capture runs inside FinalizeRealtimeSuccess.
func (s *Store) CaptureExecutionEnvelopeSpendForTest(ctx context.Context, contractID uuid.UUID, capturedNanos, capturedTokens int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := captureEnvelopeSpendTx(ctx, tx, contractID, capturedNanos, capturedTokens); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BindEnvelopeSpendContractForTest attaches a contract id to a reserved spend.
func (s *Store) BindEnvelopeSpendContractForTest(ctx context.Context, spendID, contractID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := bindEnvelopeSpendContractTx(ctx, tx, spendID, contractID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
