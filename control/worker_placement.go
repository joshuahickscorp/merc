package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WorkerPlacement is the fourth meaning of "placement": the actual worker
// choice for one accepted assignment (or an explicit record of why a worker
// cannot be bound yet).
//
// It is deliberately not PlacementDecision (supply mode), PlacementRequirement
// (eligibility), or RealtimePlacementPlan (host topology). Those three remain
// what they are. This type binds the worker the control plane actually chose
// at authorize / claim / lease — or records that batch accept cannot choose one.
//
// Build order and shapes (Step 9 shape note, 2026-08-09):
//
//   - batch accept → PENDING_CLAIM (pull market; no worker at accept)
//   - batch claim  → BOUND with ClaimEligibility (no frozen candidate epoch)
//   - realtime     → BOUND citing MarketDecision PUSH_ORDER_BOOK
//   - service lease → BOUND citing the offer book selection
//
// Locality is soft belief: a locality-driven winner must record the freshness
// of worker_prefix_state / model state / offer warmth it trusted. Fallback is
// lane-discriminated — one canonical fallback field would flatten three
// semantics (requeue/hedge, immutable worker, keep-lease-new-worker).

const workerPlacementVersion = 1

// Status values.
const (
	workerPlacementBound        = "BOUND"
	workerPlacementPendingClaim = "PENDING_CLAIM"
	workerPlacementRefused      = "REFUSED"
)

// Accept / claim lanes.
const (
	workerPlacementLaneBatch        = "batch"
	workerPlacementLaneRealtime     = "realtime"
	workerPlacementLaneServiceLease = "service_lease"
)

// How the worker was selected.
const (
	workerSelectionBatchClaimPull    = "BATCH_CLAIM_PULL"
	workerSelectionRealtimePushBook  = "REALTIME_PUSH_BOOK"
	workerSelectionServiceLeaseOffer = "SERVICE_LEASE_OFFER"
	workerSelectionNonePendingPull   = "NONE_PENDING_PULL"
)

// Fallback shapes — one per lane, not interchangeable.
const (
	workerFallbackBatchRequeue            = "BATCH_REQUEUE"
	workerFallbackBatchHedge              = "BATCH_HEDGE"
	workerFallbackRealtimeImmutableWorker = "REALTIME_IMMUTABLE_WORKER"
	workerFallbackLeaseKeepLeaseNewWorker = "LEASE_KEEP_LEASE_NEW_WORKER"
)

// Locality belief sources (soft state, never admission authority).
const (
	localitySourcePrefixState = "worker_prefix_state"
	localitySourceModelState  = "worker_model_state"
	localitySourceOfferWarmth = "realtime_offer_warmth"
)

// Freshness TTL constants mirrored from production claim/authorize SQL.
// Unmeasured wall-clock latency is not invented here — only the declared TTL
// windows the selection predicates already use.
const (
	// scheduler.go warm_prefix_depth: last_seen_warm > now() - interval '90 seconds'
	workerPrefixStateTTLSecs = 90
	// scheduler.go warm_for_task: last_seen_warm > now() - interval '60 seconds'
	workerModelStateTTLSecs = 60
	// realtime_store.go offer eligibility: last_seen_at > now()-interval '45 seconds'
	realtimeOfferWarmthTTLSecs = 45
)

// Batch pull markets have no candidate epoch object. The only legal values on
// ClaimEligibility.CandidateEpoch are empty or this explicit refusal token.
const batchCandidateEpochNone = "NONE_PULL_MARKET"

// workerPlacementRequireLocalityFreshness refuses a locality-driven placement
// that omits the freshness of the soft state it believed. Tests may neutralise
// it to show failing-before behaviour; production always leaves it true.
var workerPlacementRequireLocalityFreshness = true

// WorkerPlacement is the immutable worker-choice binding for one placement act.
type WorkerPlacement struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
	// Lane is the accept/claim path that produced this binding.
	Lane string `json:"lane"`

	// Selected worker when Status is BOUND.
	WorkerID   uuid.UUID `json:"worker_id,omitempty"`
	SupplierID uuid.UUID `json:"supplier_id,omitempty"`

	// SelectionShape names how the worker was chosen (or that none was).
	SelectionShape string `json:"selection_shape"`

	// ClaimEligibility is set only for batch BOUND claims. It records what was
	// true of the claiming worker at pull time. It is not a frozen candidate set
	// or epoch — batch is SKIP LOCKED over the live queue × fleet.
	ClaimEligibility *BatchClaimEligibility `json:"claim_eligibility,omitempty"`

	// CandidateAuthority cites an existing frozen book when one exists
	// (realtime MarketDecision, service-lease offer selection). Empty for batch.
	CandidateAuthority       string `json:"candidate_authority,omitempty"`
	CandidateAuthorityDigest string `json:"candidate_authority_digest,omitempty"`
	CandidateCount           int    `json:"candidate_count,omitempty"`
	SelectedRank             int    `json:"selected_rank,omitempty"`

	// Locality is the soft belief that participated in selection. Required when
	// LocalityDroveSelection is true (freshness tripwire).
	Locality               *LocalityBelief `json:"locality,omitempty"`
	LocalityDroveSelection bool            `json:"locality_drove_selection"`

	// Fallback is lane-discriminated. A batch placement cannot record a
	// realtime-immutable-worker fallback, and vice versa.
	Fallback WorkerPlacementFallback `json:"fallback"`

	Reason string `json:"reason"`
}

// BatchClaimEligibility is the claim-time snapshot for a batch pull.
// CandidateEpoch must not pretend a frozen book existed.
type BatchClaimEligibility struct {
	ClaimingWorkerID   uuid.UUID `json:"claiming_worker_id"`
	ClaimingSupplierID uuid.UUID `json:"claiming_supplier_id"`
	ClaimedTaskID      uuid.UUID `json:"claimed_task_id"`
	ClaimedJobID       uuid.UUID `json:"claimed_job_id"`
	// ClaimedAt is RFC3339 UTC of the claim instant.
	ClaimedAt string `json:"claimed_at"`
	// Runtime / hardware facts frozen onto the task at claim.
	RuntimeCellID string `json:"runtime_cell_id,omitempty"`
	HWClass       string `json:"hw_class,omitempty"`
	// AvailabilityMode is always SKIP_LOCKED for batch claim SQL.
	AvailabilityMode string `json:"availability_mode"`
	// CandidateEpoch is struck for batch. Empty or NONE_PULL_MARKET only.
	CandidateEpoch string `json:"candidate_epoch"`
	Note           string `json:"note,omitempty"`
}

// LocalityBelief records a soft residency/warmth signal and the freshness of
// the state row the selector trusted. Stale worker_prefix_state can flip a
// winner inside a cost class; without this record the flip is invisible.
type LocalityBelief struct {
	Source string `json:"source"`
	// LastSeenWarm is RFC3339 of the state row's last_seen_warm / last_seen_at
	// when known. Empty when FreshnessKnown is false.
	LastSeenWarm string `json:"last_seen_warm,omitempty"`
	// FreshUntil is the exclusive upper bound of the TTL window applied at
	// decide time (last_seen + TTL), RFC3339. Required when FreshnessKnown.
	FreshUntil string `json:"fresh_until,omitempty"`
	// FreshnessTTLSecs is the declared TTL window from claim/authorize SQL.
	FreshnessTTLSecs int `json:"freshness_ttl_secs,omitempty"`
	// Signal values (optional; zero means cold / absent, not unknown).
	WarmPrefixDepth int    `json:"warm_prefix_depth,omitempty"`
	WarmModel       bool   `json:"warm_model,omitempty"`
	OfferWarmth     string `json:"offer_warmth,omitempty"`
	// FreshnessKnown is false when the selector cannot name the state row's age.
	// A zero LastSeenWarm must not be read as a measurement.
	FreshnessKnown bool `json:"freshness_known"`
}

// WorkerPlacementFallback is the recovery shape for this lane.
type WorkerPlacementFallback struct {
	Shape string `json:"shape"`
	Note  string `json:"note,omitempty"`
}

// ValidateWorkerPlacement checks structural rules and the locality-freshness
// tripwire. It does not consult the live fleet.
func ValidateWorkerPlacement(p WorkerPlacement) error {
	if p.Version != workerPlacementVersion {
		return fmt.Errorf("worker placement has unsupported version %d", p.Version)
	}
	switch p.Lane {
	case workerPlacementLaneBatch, workerPlacementLaneRealtime, workerPlacementLaneServiceLease:
	default:
		return fmt.Errorf("worker placement has unknown lane %q", p.Lane)
	}
	if strings.TrimSpace(p.Reason) == "" {
		return errors.New("worker placement requires a reason")
	}
	if strings.TrimSpace(p.SelectionShape) == "" {
		return errors.New("worker placement requires a selection_shape")
	}
	if err := validateWorkerPlacementFallback(p.Lane, p.Fallback); err != nil {
		return err
	}

	switch p.Status {
	case workerPlacementBound:
		if p.WorkerID == uuid.Nil || p.SupplierID == uuid.Nil {
			return errors.New("BOUND worker placement requires worker and supplier identity")
		}
		switch p.Lane {
		case workerPlacementLaneBatch:
			if p.SelectionShape != workerSelectionBatchClaimPull {
				return fmt.Errorf("batch BOUND placement requires selection_shape %s", workerSelectionBatchClaimPull)
			}
			if p.ClaimEligibility == nil {
				return errors.New("batch BOUND placement requires claim_eligibility snapshot")
			}
			if err := validateBatchClaimEligibility(*p.ClaimEligibility, p.WorkerID, p.SupplierID); err != nil {
				return err
			}
			// Batch must not cite a frozen push book as if it had one.
			if p.CandidateAuthority != "" || p.CandidateAuthorityDigest != "" {
				return errors.New("batch claim placement must not cite a frozen candidate authority")
			}
			if p.CandidateCount != 0 || p.SelectedRank != 0 {
				return errors.New("batch claim placement must not record push-book ranks")
			}
		case workerPlacementLaneRealtime:
			if p.SelectionShape != workerSelectionRealtimePushBook {
				return fmt.Errorf("realtime BOUND placement requires selection_shape %s", workerSelectionRealtimePushBook)
			}
			if p.ClaimEligibility != nil {
				return errors.New("realtime placement must not carry a batch claim_eligibility body")
			}
			if p.CandidateAuthority != "MarketDecision" {
				return errors.New("realtime BOUND placement must cite MarketDecision as candidate authority")
			}
			if !validSHA256(p.CandidateAuthorityDigest) {
				return errors.New("realtime BOUND placement requires MarketDecision digest")
			}
			if p.CandidateCount <= 0 || p.SelectedRank <= 0 || p.SelectedRank > p.CandidateCount {
				return errors.New("realtime BOUND placement has invalid candidate_count/selected_rank")
			}
		case workerPlacementLaneServiceLease:
			if p.SelectionShape != workerSelectionServiceLeaseOffer {
				return fmt.Errorf("service lease BOUND placement requires selection_shape %s", workerSelectionServiceLeaseOffer)
			}
			if p.ClaimEligibility != nil {
				return errors.New("service lease placement must not carry a batch claim_eligibility body")
			}
			if p.CandidateAuthority != "service_lease_offer_book" {
				return errors.New("service lease BOUND placement must cite service_lease_offer_book")
			}
			if p.CandidateCount <= 0 || p.SelectedRank <= 0 || p.SelectedRank > p.CandidateCount {
				return errors.New("service lease BOUND placement has invalid candidate_count/selected_rank")
			}
		}
	case workerPlacementPendingClaim:
		if p.Lane != workerPlacementLaneBatch {
			return errors.New("PENDING_CLAIM is only legal on the batch lane")
		}
		if p.SelectionShape != workerSelectionNonePendingPull {
			return fmt.Errorf("PENDING_CLAIM requires selection_shape %s", workerSelectionNonePendingPull)
		}
		if p.WorkerID != uuid.Nil || p.SupplierID != uuid.Nil {
			return errors.New("PENDING_CLAIM must not bind a worker")
		}
		if p.ClaimEligibility != nil {
			return errors.New("PENDING_CLAIM must not carry claim_eligibility (no claim yet)")
		}
		if p.CandidateAuthority != "" || p.CandidateCount != 0 {
			return errors.New("PENDING_CLAIM must not cite a candidate book")
		}
		if p.LocalityDroveSelection {
			return errors.New("PENDING_CLAIM cannot be locality-driven")
		}
	case workerPlacementRefused:
		if p.WorkerID != uuid.Nil {
			return errors.New("REFUSED worker placement must not bind a worker")
		}
	default:
		return fmt.Errorf("worker placement has unknown status %q", p.Status)
	}

	if p.LocalityDroveSelection {
		if p.Locality == nil {
			return errors.New("locality-driven worker placement requires a locality belief")
		}
		if workerPlacementRequireLocalityFreshness {
			if err := validateLocalityBeliefFreshness(*p.Locality); err != nil {
				return err
			}
		}
	} else if p.Locality != nil {
		// Soft belief may still be recorded when it did not move the winner;
		// if present, freshness rules still apply when known.
		if p.Locality.FreshnessKnown {
			if err := validateLocalityBeliefFreshness(*p.Locality); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateWorkerPlacementFallback(lane string, fb WorkerPlacementFallback) error {
	if strings.TrimSpace(fb.Shape) == "" {
		return errors.New("worker placement requires a fallback shape")
	}
	switch lane {
	case workerPlacementLaneBatch:
		switch fb.Shape {
		case workerFallbackBatchRequeue, workerFallbackBatchHedge:
			return nil
		default:
			return fmt.Errorf("batch worker placement cannot record fallback shape %q", fb.Shape)
		}
	case workerPlacementLaneRealtime:
		if fb.Shape != workerFallbackRealtimeImmutableWorker {
			return fmt.Errorf("realtime worker placement cannot record fallback shape %q", fb.Shape)
		}
		return nil
	case workerPlacementLaneServiceLease:
		if fb.Shape != workerFallbackLeaseKeepLeaseNewWorker {
			return fmt.Errorf("service lease worker placement cannot record fallback shape %q", fb.Shape)
		}
		return nil
	default:
		return fmt.Errorf("worker placement has unknown lane %q", lane)
	}
}

func validateBatchClaimEligibility(e BatchClaimEligibility, workerID, supplierID uuid.UUID) error {
	if e.ClaimingWorkerID == uuid.Nil || e.ClaimingSupplierID == uuid.Nil {
		return errors.New("batch claim eligibility lacks claiming worker/supplier")
	}
	if e.ClaimingWorkerID != workerID || e.ClaimingSupplierID != supplierID {
		return errors.New("batch claim eligibility identity disagrees with selected worker")
	}
	if e.ClaimedTaskID == uuid.Nil || e.ClaimedJobID == uuid.Nil {
		return errors.New("batch claim eligibility lacks task/job identity")
	}
	if strings.TrimSpace(e.ClaimedAt) == "" {
		return errors.New("batch claim eligibility requires claimed_at")
	}
	if e.AvailabilityMode != marketAvailabilitySkipLocked {
		return fmt.Errorf("batch claim eligibility availability_mode must be %s, got %q",
			marketAvailabilitySkipLocked, e.AvailabilityMode)
	}
	switch e.CandidateEpoch {
	case "", batchCandidateEpochNone:
		// legal — batch has no frozen epoch
	default:
		return fmt.Errorf("batch claim eligibility must not invent candidate epoch %q", e.CandidateEpoch)
	}
	return nil
}

func validateLocalityBeliefFreshness(b LocalityBelief) error {
	switch b.Source {
	case localitySourcePrefixState, localitySourceModelState, localitySourceOfferWarmth:
	default:
		return fmt.Errorf("locality belief has unknown source %q", b.Source)
	}
	if !b.FreshnessKnown {
		return errors.New("locality-driven placement requires freshness_known belief")
	}
	if strings.TrimSpace(b.FreshUntil) == "" {
		return errors.New("locality belief requires fresh_until when freshness is known")
	}
	if b.FreshnessTTLSecs <= 0 {
		return errors.New("locality belief requires positive freshness_ttl_secs")
	}
	return nil
}

func workerPlacementDigest(p WorkerPlacement) (string, error) {
	if err := ValidateWorkerPlacement(p); err != nil {
		return "", err
	}
	return canonicalDigest("worker placement", p)
}

// newBatchAcceptPendingWorkerPlacement records that batch accept cannot bind a
// worker: the market is pull + SKIP LOCKED and the candidate epoch is the live
// queue × fleet at claim time.
func newBatchAcceptPendingWorkerPlacement() (WorkerPlacement, error) {
	p := WorkerPlacement{
		Version:        workerPlacementVersion,
		Status:         workerPlacementPendingClaim,
		Lane:           workerPlacementLaneBatch,
		SelectionShape: workerSelectionNonePendingPull,
		Fallback: WorkerPlacementFallback{
			Shape: workerFallbackBatchRequeue,
			Note:  "batch recovery is requeue or hedge after claim failure; no immutable worker at accept",
		},
		Reason: "batch is a pull market over SKIP LOCKED; the worker is bound at claim " +
			"with a claim-time eligibility snapshot, not a frozen candidate epoch at accept",
	}
	if err := ValidateWorkerPlacement(p); err != nil {
		return WorkerPlacement{}, err
	}
	return p, nil
}

// BatchClaimPlacementInputs are the facts ClaimTasksTx already freezes onto a
// task, plus optional locality signals measured for this claiming worker.
type BatchClaimPlacementInputs struct {
	WorkerID      uuid.UUID
	SupplierID    uuid.UUID
	TaskID        uuid.UUID
	JobID         uuid.UUID
	ClaimedAt     time.Time
	RuntimeCellID string
	HWClass       string
	// Locality signals for THIS claiming worker at claim time.
	WarmPrefixDepth int
	// PrefixLastSeen is the newest last_seen_warm among matching prefix rows.
	// Zero time means no warm prefix row was observed.
	PrefixLastSeen time.Time
	WarmModel      bool
	// ModelLastSeen is last_seen_warm on worker_model_state when WarmModel.
	ModelLastSeen time.Time
	// LocalityDroveSelection is true when a non-zero warmth/prefix signal sat
	// in the claim ORDER BY for this worker (preference applied, not that a
	// multi-worker comparison proved affinity moved the pick).
	LocalityDroveSelection bool
	// Hedge is true when this claim is a hedge/tiebreak attempt.
	Hedge bool
}

// newBatchClaimWorkerPlacement binds the claiming worker with a claim-time
// eligibility snapshot. It refuses to invent a frozen candidate epoch.
func newBatchClaimWorkerPlacement(in BatchClaimPlacementInputs) (WorkerPlacement, error) {
	if in.WorkerID == uuid.Nil || in.SupplierID == uuid.Nil ||
		in.TaskID == uuid.Nil || in.JobID == uuid.Nil {
		return WorkerPlacement{}, errors.New("batch claim worker placement requires worker, supplier, task, and job")
	}
	claimedAt := in.ClaimedAt.UTC()
	if claimedAt.IsZero() {
		claimedAt = time.Now().UTC()
	}
	fallback := WorkerPlacementFallback{
		Shape: workerFallbackBatchRequeue,
		Note:  "failed or stuck batch tasks requeue to a different machine",
	}
	if in.Hedge {
		fallback = WorkerPlacementFallback{
			Shape: workerFallbackBatchHedge,
			Note:  "straggler hedge inserts a peer attempt without freezing the original worker",
		}
	}

	var locality *LocalityBelief
	drove := in.LocalityDroveSelection
	if in.WarmPrefixDepth > 0 || !in.PrefixLastSeen.IsZero() {
		b := LocalityBelief{
			Source:           localitySourcePrefixState,
			WarmPrefixDepth:  in.WarmPrefixDepth,
			FreshnessTTLSecs: workerPrefixStateTTLSecs,
		}
		if !in.PrefixLastSeen.IsZero() {
			b.FreshnessKnown = true
			b.LastSeenWarm = in.PrefixLastSeen.UTC().Format(time.RFC3339Nano)
			b.FreshUntil = in.PrefixLastSeen.UTC().Add(time.Duration(workerPrefixStateTTLSecs) * time.Second).Format(time.RFC3339Nano)
		}
		locality = &b
	} else if in.WarmModel {
		b := LocalityBelief{
			Source:           localitySourceModelState,
			WarmModel:        true,
			FreshnessTTLSecs: workerModelStateTTLSecs,
		}
		if !in.ModelLastSeen.IsZero() {
			b.FreshnessKnown = true
			b.LastSeenWarm = in.ModelLastSeen.UTC().Format(time.RFC3339Nano)
			b.FreshUntil = in.ModelLastSeen.UTC().Add(time.Duration(workerModelStateTTLSecs) * time.Second).Format(time.RFC3339Nano)
		}
		locality = &b
	}
	// If locality drove selection but we could not name freshness, leave
	// FreshnessKnown=false so ValidateWorkerPlacement refuses under the tripwire.
	if drove && locality == nil {
		locality = &LocalityBelief{
			Source:         localitySourcePrefixState,
			FreshnessKnown: false,
		}
	}

	p := WorkerPlacement{
		Version:        workerPlacementVersion,
		Status:         workerPlacementBound,
		Lane:           workerPlacementLaneBatch,
		WorkerID:       in.WorkerID,
		SupplierID:     in.SupplierID,
		SelectionShape: workerSelectionBatchClaimPull,
		ClaimEligibility: &BatchClaimEligibility{
			ClaimingWorkerID:   in.WorkerID,
			ClaimingSupplierID: in.SupplierID,
			ClaimedTaskID:      in.TaskID,
			ClaimedJobID:       in.JobID,
			ClaimedAt:          claimedAt.Format(time.RFC3339Nano),
			RuntimeCellID:      in.RuntimeCellID,
			HWClass:            in.HWClass,
			AvailabilityMode:   marketAvailabilitySkipLocked,
			CandidateEpoch:     batchCandidateEpochNone,
			Note: "claim-time eligibility snapshot over live queue × fleet; " +
				"no frozen candidate epoch exists for batch pull + SKIP LOCKED",
		},
		Locality:               locality,
		LocalityDroveSelection: drove,
		Fallback:               fallback,
		Reason: fmt.Sprintf(
			"worker %s claimed task %s under batch pull + SKIP LOCKED with claim-time eligibility snapshot",
			in.WorkerID, in.TaskID),
	}
	if err := ValidateWorkerPlacement(p); err != nil {
		return WorkerPlacement{}, err
	}
	return p, nil
}

// newRealtimeWorkerPlacement binds the worker selected by a PUSH_ORDER_BOOK
// MarketDecision. It does not re-rank; the market authority remains canonical.
func newRealtimeWorkerPlacement(
	md MarketDecision,
	marketDigest string,
	offerWarmth string,
	// offerLastSeen is the selected offer's last_seen_at when known. Zero means
	// freshness of the warmth belief is unknown (tripwire fires if warmth drove).
	offerLastSeen time.Time,
	localityDrove bool,
) (WorkerPlacement, error) {
	if err := ValidateMarketDecision(md); err != nil {
		return WorkerPlacement{}, fmt.Errorf("realtime worker placement requires valid MarketDecision: %w", err)
	}
	if md.MarketShape != marketShapePushOrderBook || md.PushOrderBook == nil {
		return WorkerPlacement{}, errors.New("realtime worker placement requires PUSH_ORDER_BOOK MarketDecision")
	}
	if !validSHA256(marketDigest) {
		return WorkerPlacement{}, errors.New("realtime worker placement requires MarketDecision digest")
	}
	book := md.PushOrderBook

	var locality *LocalityBelief
	warmth := strings.TrimSpace(offerWarmth)
	if warmth == "" && book.RankingInputs != nil {
		warmth = book.RankingInputs.Warmth
	}
	if warmth != "" {
		b := LocalityBelief{
			Source:           localitySourceOfferWarmth,
			OfferWarmth:      warmth,
			FreshnessTTLSecs: realtimeOfferWarmthTTLSecs,
		}
		if !offerLastSeen.IsZero() {
			b.FreshnessKnown = true
			b.LastSeenWarm = offerLastSeen.UTC().Format(time.RFC3339Nano)
			b.FreshUntil = offerLastSeen.UTC().Add(time.Duration(realtimeOfferWarmthTTLSecs) * time.Second).Format(time.RFC3339Nano)
		}
		// When the authorize SQL already filtered last_seen_at > now()-45s but
		// the exact stamp was not returned, record the policy bound as known at
		// decide time: fresh_until is "now + 0" is wrong; instead leave
		// FreshnessKnown only when we have last_seen. LocalityDrove without
		// stamp fails the tripwire — callers must pass offerLastSeen when
		// claiming warmth drove selection.
		locality = &b
	}
	if localityDrove && locality == nil {
		locality = &LocalityBelief{
			Source:         localitySourceOfferWarmth,
			FreshnessKnown: false,
		}
	}

	p := WorkerPlacement{
		Version:                  workerPlacementVersion,
		Status:                   workerPlacementBound,
		Lane:                     workerPlacementLaneRealtime,
		WorkerID:                 book.SelectedWorkerID,
		SupplierID:               book.SelectedSupplierID,
		SelectionShape:           workerSelectionRealtimePushBook,
		CandidateAuthority:       "MarketDecision",
		CandidateAuthorityDigest: marketDigest,
		CandidateCount:           book.CandidateCount,
		SelectedRank:             book.SelectedRank,
		Locality:                 locality,
		LocalityDroveSelection:   localityDrove,
		Fallback: WorkerPlacementFallback{
			Shape: workerFallbackRealtimeImmutableWorker,
			Note:  "realtime contracts keep the authorized worker; heartbeats cannot rewrite placement",
		},
		Reason: fmt.Sprintf(
			"realtime PUSH_ORDER_BOOK selected worker %s at rank %d of %d (MarketDecision %s)",
			book.SelectedWorkerID, book.SelectedRank, book.CandidateCount, marketDigest[:12]),
	}
	if err := ValidateWorkerPlacement(p); err != nil {
		return WorkerPlacement{}, err
	}
	return p, nil
}

// newServiceLeaseWorkerPlacement binds the worker chosen from the lease offer
// book. Fallback keeps the lease and takes a new worker on failover.
func newServiceLeaseWorkerPlacement(
	workerID, supplierID uuid.UUID,
	candidateCount, selectedRank int,
	// marketDigest is optional; service lease market detail is not yet a
	// MarketDecision. When empty, CandidateAuthorityDigest stays empty and
	// CandidateAuthority still names the offer book.
	marketDigest string,
) (WorkerPlacement, error) {
	if workerID == uuid.Nil || supplierID == uuid.Nil {
		return WorkerPlacement{}, errors.New("service lease worker placement requires worker and supplier")
	}
	if candidateCount <= 0 || selectedRank <= 0 || selectedRank > candidateCount {
		return WorkerPlacement{}, errors.New("service lease worker placement has invalid candidate_count/selected_rank")
	}
	p := WorkerPlacement{
		Version:            workerPlacementVersion,
		Status:             workerPlacementBound,
		Lane:               workerPlacementLaneServiceLease,
		WorkerID:           workerID,
		SupplierID:         supplierID,
		SelectionShape:     workerSelectionServiceLeaseOffer,
		CandidateAuthority: "service_lease_offer_book",
		CandidateCount:     candidateCount,
		SelectedRank:       selectedRank,
		Fallback: WorkerPlacementFallback{
			Shape: workerFallbackLeaseKeepLeaseNewWorker,
			Note:  "service lease keeps the lease identity and takes a replacement worker under frozen ceilings",
		},
		Reason: fmt.Sprintf(
			"service lease offer book selected worker %s at rank %d of %d",
			workerID, selectedRank, candidateCount),
	}
	if marketDigest != "" {
		if !validSHA256(marketDigest) {
			return WorkerPlacement{}, errors.New("service lease worker placement market digest is not sha256")
		}
		p.CandidateAuthorityDigest = marketDigest
	}
	if err := ValidateWorkerPlacement(p); err != nil {
		return WorkerPlacement{}, err
	}
	return p, nil
}

// projectPullMarketDecision builds the Step 7 batch market shape from a BOUND
// batch WorkerPlacement. MarketDecision remains the market authority plane;
// WorkerPlacement remains the worker-choice + locality + fallback plane.
func projectPullMarketDecision(p WorkerPlacement) (MarketDecision, error) {
	if err := ValidateWorkerPlacement(p); err != nil {
		return MarketDecision{}, err
	}
	if p.Lane != workerPlacementLaneBatch || p.Status != workerPlacementBound || p.ClaimEligibility == nil {
		return MarketDecision{}, errors.New("pull MarketDecision projects only from batch BOUND claim placement")
	}
	e := p.ClaimEligibility
	md := MarketDecision{
		Version:     marketDecisionVersion,
		MarketShape: marketShapePullEligibilitySnapshot,
		PullEligibilitySnapshot: &MarketPullEligibilitySnapshot{
			ClaimingWorkerID:   e.ClaimingWorkerID,
			ClaimingSupplierID: e.ClaimingSupplierID,
			ClaimedTaskID:      e.ClaimedTaskID,
			ClaimedJobID:       e.ClaimedJobID,
			AvailabilityMode:   e.AvailabilityMode,
			CandidateEpoch:     e.CandidateEpoch,
			Note:               e.Note,
		},
	}
	if err := ValidateMarketDecision(md); err != nil {
		return MarketDecision{}, err
	}
	return md, nil
}
