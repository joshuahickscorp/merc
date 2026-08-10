package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EvidenceEnvelope is the immutable hash-linked chain root for one accepted
// transaction outcome. Links cite existing authority digests; they never
// recompute alternate digests of the same facts, and they never invent a
// digest for an authority that does not exist.
//
// Funding prepaid holds live in execution_envelopes (accounts/store_prepaid).
// That table is unrelated money authority. This type is the Network V2
// transaction-chain root (Step 14). File evidence binding
// (ReceiptIdentity / *.binding.json) remains a separate producer-identity
// layer and is not reimplemented here.

const evidenceEnvelopeVersion = 1

// Link kinds in Bible chain order. Pricing is included because batch accept
// already freezes pricing_decision_sha256 in the same INSERT as the other
// decision digests; the envelope binds that existing column, not a copy.
const (
	EnvelopeLinkRequest      = "request"
	EnvelopeLinkWorkload     = "workload"
	EnvelopeLinkMarket       = "market"
	EnvelopeLinkPricing      = "pricing"
	EnvelopeLinkRuntime      = "runtime"
	EnvelopeLinkPlacement    = "placement"
	EnvelopeLinkTopology     = "topology"
	EnvelopeLinkExecution    = "execution"
	EnvelopeLinkVerification = "verification"
	EnvelopeLinkSettlement   = "settlement"
	EnvelopeLinkReceipt      = "receipt"
)

// Link statuses. BOUND cites a real digest. ABSENT means the programme has no
// such authority type yet (expected partial). PENDING means the authority will
// be written later in this outcome's lifecycle. A validation failure is a
// break — never stored as a link status.
const (
	EnvelopeLinkBound   = "BOUND"
	EnvelopeLinkAbsent  = "ABSENT"
	EnvelopeLinkPending = "PENDING"
)

// Lanes that may root an envelope. Step 14 writes the batch accept root.
const (
	EnvelopeLaneBatch = "batch"
)

// evidenceEnvelopeChainOrder is the fixed, complete set of links every
// envelope must carry so partial chains stay legible.
var evidenceEnvelopeChainOrder = []string{
	EnvelopeLinkRequest,
	EnvelopeLinkWorkload,
	EnvelopeLinkMarket,
	EnvelopeLinkPricing,
	EnvelopeLinkRuntime,
	EnvelopeLinkPlacement,
	EnvelopeLinkTopology,
	EnvelopeLinkExecution,
	EnvelopeLinkVerification,
	EnvelopeLinkSettlement,
	EnvelopeLinkReceipt,
}

// evidenceEnvelopeTamperCheck enables root-digest verification. Tests may
// neutralise it to show failing-before behaviour; production always leaves it
// true. Never set false outside tests.
var evidenceEnvelopeTamperCheck = true

var (
	errEvidenceEnvelopeInvalid  = errors.New("evidence envelope invalid")
	errEvidenceEnvelopeTampered = errors.New("evidence envelope digest mismatch")
	errEvidenceEnvelopeBroken   = errors.New("evidence envelope broken link")
)

// EvidenceEnvelopeLink is one stage of the request-to-receipt chain.
type EvidenceEnvelopeLink struct {
	Kind string `json:"kind"`
	// Status is BOUND, ABSENT, or PENDING. Never a silent empty.
	Status string `json:"status"`
	// Authority names the existing type that owns Digest when BOUND
	// (e.g. WorkloadDecision, PlacementRequirement). Empty when ABSENT/PENDING.
	Authority string `json:"authority,omitempty"`
	// Digest is the existing authority digest when BOUND. Empty otherwise.
	// Never a fabricated placeholder.
	Digest string `json:"digest,omitempty"`
	// Reason is required for ABSENT and PENDING so a reader can tell "type
	// does not exist yet" from "stage not written yet" from a break.
	Reason string `json:"reason,omitempty"`
}

// EvidenceEnvelope is the sealed chain root. EnvelopeSHA256 is the digest of
// the envelope with EnvelopeSHA256 cleared; a mutation of any bound digest
// fails ValidateEvidenceEnvelope.
type EvidenceEnvelope struct {
	Version        int                    `json:"version"`
	Lane           string                 `json:"lane"`
	SubjectID      string                 `json:"subject_id"`
	Links          []EvidenceEnvelopeLink `json:"links"`
	EnvelopeSHA256 string                 `json:"envelope_sha256,omitempty"`
}

// sealEvidenceEnvelope validates structural rules, computes the root digest,
// and returns a sealed copy.
func sealEvidenceEnvelope(env EvidenceEnvelope) (EvidenceEnvelope, error) {
	env.EnvelopeSHA256 = ""
	if err := validateEvidenceEnvelopeStructure(env); err != nil {
		return EvidenceEnvelope{}, err
	}
	digest, err := evidenceEnvelopeDigest(env)
	if err != nil {
		return EvidenceEnvelope{}, err
	}
	env.EnvelopeSHA256 = digest
	return env, nil
}

// evidenceEnvelopeDigest hashes the envelope without the root field.
func evidenceEnvelopeDigest(env EvidenceEnvelope) (string, error) {
	body := env
	body.EnvelopeSHA256 = ""
	return canonicalDigest("evidence envelope", body)
}

// ValidateEvidenceEnvelope checks structure and, when tamper checking is on,
// that EnvelopeSHA256 matches a fresh digest of the links.
func ValidateEvidenceEnvelope(env EvidenceEnvelope) error {
	if err := validateEvidenceEnvelopeStructure(env); err != nil {
		return err
	}
	if !validSHA256(env.EnvelopeSHA256) {
		return fmt.Errorf("%w: missing envelope root digest", errEvidenceEnvelopeInvalid)
	}
	if !evidenceEnvelopeTamperCheck {
		return nil
	}
	got, err := evidenceEnvelopeDigest(env)
	if err != nil {
		return err
	}
	if got != env.EnvelopeSHA256 {
		return fmt.Errorf("%w: sealed %s recomputed %s",
			errEvidenceEnvelopeTampered, env.EnvelopeSHA256, got)
	}
	return nil
}

func validateEvidenceEnvelopeStructure(env EvidenceEnvelope) error {
	if env.Version != evidenceEnvelopeVersion {
		return fmt.Errorf("%w: unsupported version %d", errEvidenceEnvelopeInvalid, env.Version)
	}
	if env.Lane == "" {
		return fmt.Errorf("%w: missing lane", errEvidenceEnvelopeInvalid)
	}
	if strings.TrimSpace(env.SubjectID) == "" {
		return fmt.Errorf("%w: missing subject_id", errEvidenceEnvelopeInvalid)
	}
	if len(env.Links) != len(evidenceEnvelopeChainOrder) {
		return fmt.Errorf("%w: want %d links, got %d",
			errEvidenceEnvelopeInvalid, len(evidenceEnvelopeChainOrder), len(env.Links))
	}
	for i, wantKind := range evidenceEnvelopeChainOrder {
		link := env.Links[i]
		if link.Kind != wantKind {
			return fmt.Errorf("%w: link %d kind %q want %q",
				errEvidenceEnvelopeInvalid, i, link.Kind, wantKind)
		}
		if err := validateEvidenceEnvelopeLink(link); err != nil {
			return fmt.Errorf("%w: link %s: %v", errEvidenceEnvelopeBroken, wantKind, err)
		}
	}
	return nil
}

func validateEvidenceEnvelopeLink(link EvidenceEnvelopeLink) error {
	switch link.Status {
	case EnvelopeLinkBound:
		if strings.TrimSpace(link.Authority) == "" {
			return errors.New("BOUND link missing authority name")
		}
		if !validSHA256(link.Digest) {
			return errors.New("BOUND link missing valid digest")
		}
		if strings.TrimSpace(link.Reason) != "" {
			return errors.New("BOUND link must not carry a reason")
		}
	case EnvelopeLinkAbsent, EnvelopeLinkPending:
		if strings.TrimSpace(link.Reason) == "" {
			return fmt.Errorf("%s link missing reason", link.Status)
		}
		if link.Digest != "" {
			// Fabricated digests for missing authorities are refused.
			return fmt.Errorf("%s link must not carry a digest", link.Status)
		}
		if link.Authority != "" {
			return fmt.Errorf("%s link must not name an authority", link.Status)
		}
	default:
		return fmt.Errorf("unknown status %q", link.Status)
	}
	return nil
}

// batchAcceptBoundDigests are digests already frozen by SubmitJobTx. The
// envelope cites them; it does not re-hash decision bodies.
type batchAcceptBoundDigests struct {
	RequestSHA256     string
	WorkloadSHA256    string
	PlacementSHA256   string
	PricingSHA256     string
	RuntimeSHA256     string
	TopologySHA256    string
	ComputePlanSHA256 string // cited only when later steps need it; not a chain link
}

// buildBatchAcceptEvidenceEnvelope builds the batch-lane root written in the
// same transaction as the job's decision digests. Market remains ABSENT with
// reason until Step 7 batch MarketDecision exists.
// RuntimeDecision is BOUND when RuntimeSHA256 is supplied (Step 8).
// TopologyDecision is BOUND when TopologySHA256 is supplied (Step 10).
// VerificationContract / SettlementPlan stay ABSENT. Execution/receipt stages
// that belong later in the lifecycle are PENDING.
func buildBatchAcceptEvidenceEnvelope(jobID uuid.UUID, d batchAcceptBoundDigests) (EvidenceEnvelope, error) {
	if jobID == uuid.Nil {
		return EvidenceEnvelope{}, fmt.Errorf("%w: nil job id", errEvidenceEnvelopeInvalid)
	}
	links := make([]EvidenceEnvelopeLink, 0, len(evidenceEnvelopeChainOrder))
	for _, kind := range evidenceEnvelopeChainOrder {
		link, err := batchAcceptLink(kind, d)
		if err != nil {
			return EvidenceEnvelope{}, err
		}
		links = append(links, link)
	}
	return sealEvidenceEnvelope(EvidenceEnvelope{
		Version:   evidenceEnvelopeVersion,
		Lane:      EnvelopeLaneBatch,
		SubjectID: jobID.String(),
		Links:     links,
	})
}

func batchAcceptLink(kind string, d batchAcceptBoundDigests) (EvidenceEnvelopeLink, error) {
	bound := func(authority, digest string) (EvidenceEnvelopeLink, error) {
		if !validSHA256(digest) {
			return EvidenceEnvelopeLink{}, fmt.Errorf(
				"%w: %s requires existing %s digest", errEvidenceEnvelopeBroken, kind, authority)
		}
		return EvidenceEnvelopeLink{
			Kind: kind, Status: EnvelopeLinkBound,
			Authority: authority, Digest: digest,
		}, nil
	}
	absent := func(reason string) EvidenceEnvelopeLink {
		return EvidenceEnvelopeLink{
			Kind: kind, Status: EnvelopeLinkAbsent, Reason: reason,
		}
	}
	pending := func(reason string) EvidenceEnvelopeLink {
		return EvidenceEnvelopeLink{
			Kind: kind, Status: EnvelopeLinkPending, Reason: reason,
		}
	}

	switch kind {
	case EnvelopeLinkRequest:
		if validSHA256(d.RequestSHA256) {
			return bound("SubmitRequest", d.RequestSHA256)
		}
		// Some accept paths (test helpers, internal submits) do not carry a
		// request body digest. That is PENDING, not a break and not ABSENT
		// (the authority exists on the jobs column).
		return pending("submit_request_sha256 not supplied on this accept path"), nil
	case EnvelopeLinkWorkload:
		return bound("WorkloadDecision", d.WorkloadSHA256)
	case EnvelopeLinkMarket:
		return absent("MarketDecision authority type not introduced for batch " +
			"(Step 7: pull eligibility snapshot at claim, no batch MarketDecision object)"), nil
	case EnvelopeLinkPricing:
		return bound("PricingDecision", d.PricingSHA256)
	case EnvelopeLinkRuntime:
		// Step 8: RuntimeDecision is frozen on jobs.runtime_decision_sha256
		// inside the accept transaction. Shadow selection remains non-authority.
		return bound("RuntimeDecision", d.RuntimeSHA256)
	case EnvelopeLinkPlacement:
		// PlacementDecision as a unified type is partial; the frozen accept
		// authority today is PlacementRequirement (jobs.placement_requirement_sha256).
		return bound("PlacementRequirement", d.PlacementSHA256)
	case EnvelopeLinkTopology:
		// Step 10: TopologyDecision is frozen on jobs.topology_decision_sha256
		// inside the accept transaction. Shadow TopologyPlan remains non-authority.
		return bound("TopologyDecision", d.TopologySHA256)
	case EnvelopeLinkExecution:
		return pending("execution attempts not yet written at accept; " +
			"task execution identity is recorded per-task at claim/complete"), nil
	case EnvelopeLinkVerification:
		return absent("VerificationContract as a single type is ABSENT " +
			"(Step 12: policy/class/comparator/effects remain split)"), nil
	case EnvelopeLinkSettlement:
		return absent("SettlementPlan as a Go type is ABSENT " +
			"(Step 13: batch money authority exists but is not a SettlementPlan object)"), nil
	case EnvelopeLinkReceipt:
		return pending("immutable receipt root not yet projected; " +
			"ClearingReceipt remains live multi-query assembly until later Step 14 work"), nil
	default:
		return EvidenceEnvelopeLink{}, fmt.Errorf("%w: unknown link kind %q",
			errEvidenceEnvelopeInvalid, kind)
	}
}

// linkByKind returns the named link or false.
func (env EvidenceEnvelope) linkByKind(kind string) (EvidenceEnvelopeLink, bool) {
	for _, link := range env.Links {
		if link.Kind == kind {
			return link, true
		}
	}
	return EvidenceEnvelopeLink{}, false
}

// insertEvidenceEnvelopeTx persists a sealed envelope in the caller's
// transaction. Callers must already have validated/sealed the envelope.
func insertEvidenceEnvelopeTx(ctx context.Context, tx pgx.Tx, env EvidenceEnvelope, envelopeJSON []byte) error {
	if err := ValidateEvidenceEnvelope(env); err != nil {
		return fmt.Errorf("refuse unsealed evidence envelope: %w", err)
	}
	if len(envelopeJSON) == 0 {
		var err error
		envelopeJSON, err = json.Marshal(env)
		if err != nil {
			return fmt.Errorf("marshal evidence envelope: %w", err)
		}
	}
	subjectID, err := uuid.Parse(env.SubjectID)
	if err != nil {
		return fmt.Errorf("evidence envelope subject_id: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO evidence_envelopes
		  (envelope_sha256, lane, subject_id, version, envelope)
		VALUES ($1,$2,$3,$4,$5::jsonb)`,
		env.EnvelopeSHA256, env.Lane, subjectID, env.Version, envelopeJSON,
	); err != nil {
		return fmt.Errorf("insert evidence envelope: %w", err)
	}
	return nil
}

// loadEvidenceEnvelope loads the sealed root for a subject, if present.
func (s *Store) loadEvidenceEnvelope(ctx context.Context, lane string, subjectID uuid.UUID) (*EvidenceEnvelope, error) {
	var blob []byte
	err := s.pool.QueryRow(ctx, `
		SELECT envelope FROM evidence_envelopes
		 WHERE lane=$1 AND subject_id=$2`,
		lane, subjectID,
	).Scan(&blob)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	var env EvidenceEnvelope
	if err := json.Unmarshal(blob, &env); err != nil {
		return nil, fmt.Errorf("decode evidence envelope: %w", err)
	}
	if err := ValidateEvidenceEnvelope(env); err != nil {
		return nil, err
	}
	return &env, nil
}
