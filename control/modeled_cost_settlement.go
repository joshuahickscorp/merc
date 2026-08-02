package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CostSettlementActuals records accepted bounds beside settled actuals for
// storage and egress. It is NOT part of the frozen PricingDecision digest:
// acceptance freezes the bound; settlement writes the actual beside it so a
// historical decision is never re-derived under later geometry.
type CostSettlementActuals struct {
	// CostScheduleSHA256 must match the decision's bound schedule; a mismatch
	// fails closed so settlement cannot apply a different rate card.
	CostScheduleSHA256 string `json:"cost_schedule_sha256"`

	StorageAcceptedBytes int64 `json:"storage_accepted_bytes"`
	StorageSettledBytes  int64 `json:"storage_settled_bytes"`
	StorageAcceptedNanos int64 `json:"storage_accepted_nanos"`
	StorageSettledNanos  int64 `json:"storage_settled_nanos"`

	EgressAcceptedBytes int64 `json:"egress_accepted_bytes"`
	EgressSettledBytes  int64 `json:"egress_settled_bytes"`
	EgressAcceptedNanos int64 `json:"egress_accepted_nanos"`
	EgressSettledNanos  int64 `json:"egress_settled_nanos"`

	// RetentionSecs is the retention period used for both bound and actual
	// storage modeling, frozen at settlement from jobObjectRetentionPeriod().
	RetentionSecs int64 `json:"retention_secs"`

	SettledAt time.Time `json:"settled_at"`
}

// settleStorageEgressFromBytes recomputes storage and egress from actual
// artifact bytes under the same CostSchedule the decision froze. Returns the
// accepted bound (from the decision) beside the settled actual.
func settleStorageEgressFromBytes(
	schedule CostSchedule,
	pricing PricingDecision,
	storageSettledBytes, egressSettledBytes int64,
	retention time.Duration,
	now time.Time,
) (CostSettlementActuals, error) {
	if pricing.CostScheduleSHA256 == "" {
		return CostSettlementActuals{}, errors.New(
			"cost settlement refuses a pricing decision with no cost schedule digest")
	}
	digest, err := costScheduleDigest(schedule)
	if err != nil {
		return CostSettlementActuals{}, err
	}
	if digest != pricing.CostScheduleSHA256 {
		return CostSettlementActuals{}, fmt.Errorf(
			"cost settlement schedule digest %s does not match decision %s",
			digest, pricing.CostScheduleSHA256)
	}
	if storageSettledBytes < 0 || egressSettledBytes < 0 {
		return CostSettlementActuals{}, errors.New(
			"cost settlement refuses negative settled byte counts")
	}
	storageSettled, err := storageNanosForBytes(schedule, storageSettledBytes, retention)
	if err != nil {
		return CostSettlementActuals{}, err
	}
	egressSettled, err := egressNanosForBytes(schedule, egressSettledBytes)
	if err != nil {
		return CostSettlementActuals{}, err
	}
	// Accepted nanos: recompute from the decision's frozen accepted bytes when
	// present; otherwise from the modeled component amount projected to nanos.
	storageAcceptedNanos := usdToMicros(pricing.StorageCost.Amount) * NanosPerMicro
	egressAcceptedNanos := usdToMicros(pricing.EgressCost.Amount) * NanosPerMicro
	return CostSettlementActuals{
		CostScheduleSHA256:   digest,
		StorageAcceptedBytes: pricing.StorageAcceptedBytes,
		StorageSettledBytes:  storageSettledBytes,
		StorageAcceptedNanos: storageAcceptedNanos,
		StorageSettledNanos:  storageSettled,
		EgressAcceptedBytes:  pricing.EgressAcceptedBytes,
		EgressSettledBytes:   egressSettledBytes,
		EgressAcceptedNanos:  egressAcceptedNanos,
		EgressSettledNanos:   egressSettled,
		RetentionSecs:        int64(retention / time.Second),
		SettledAt:            now.UTC(),
	}, nil
}

// PersistCostSettlementActuals writes the settlement evidence for a job.
// Idempotent on the job_id primary key: a second write with different numbers
// is refused so an accidental re-settlement cannot rewrite history.
func (s *Store) PersistCostSettlementActuals(
	ctx context.Context, jobID uuid.UUID, actuals CostSettlementActuals,
) error {
	blob, err := json.Marshal(actuals)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO job_cost_settlements (job_id, settlement)
		VALUES ($1, $2::jsonb)
		ON CONFLICT (job_id) DO NOTHING`, jobID, blob)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Conflict: require exact match.
	var existing []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT settlement FROM job_cost_settlements WHERE job_id=$1`, jobID,
	).Scan(&existing); err != nil {
		return err
	}
	var prev CostSettlementActuals
	if err := json.Unmarshal(existing, &prev); err != nil {
		return err
	}
	// Compare durable fields; SettledAt may differ by clock if retried.
	prev.SettledAt = actuals.SettledAt
	prevBlob, _ := json.Marshal(prev)
	if string(prevBlob) != string(blob) {
		// Re-compare without SettledAt by zeroing both.
		a, b := actuals, prev
		a.SettledAt, b.SettledAt = time.Time{}, time.Time{}
		ab, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		if string(ab) != string(bb) {
			return fmt.Errorf(
				"job %s cost settlement already recorded with different actuals", jobID)
		}
	}
	return nil
}

// LoadCostSettlementActuals returns the settlement evidence for a job, or
// (nil, nil) when none has been written.
func (s *Store) LoadCostSettlementActuals(
	ctx context.Context, jobID uuid.UUID,
) (*CostSettlementActuals, error) {
	var blob []byte
	err := s.pool.QueryRow(ctx,
		`SELECT settlement FROM job_cost_settlements WHERE job_id=$1`, jobID,
	).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out CostSettlementActuals
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
