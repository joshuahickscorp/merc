package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	economicInputSourceSubmitStream    = "submit_stream_exact_raw_bytes_and_nonblank_jsonl_records"
	economicOutputSourceMergedArtifact = "merged_artifact_exact_bytes_and_records"
	batchFeeAllocationLegacyV0         = "legacy_order_residual_v0"
	batchFeeAllocationHamiltonV1       = "hamilton_largest_remainder_job_id_v1"
	processorFeeAllocationDirectV1     = "direct_single_charge_v1"
)

// roundEconomicUSD is an alias for the single micro-USD rounding helper.
func roundEconomicUSD(v float64) float64 { return roundUSD(v) }

type batchFeeWeight struct {
	JobID        uuid.UUID
	WeightMicros int64
}

type batchFeeAllocation struct {
	JobID           uuid.UUID
	WeightMicros    int64
	AllocatedMicros int64
}

func validateBatchFeeInputs(feeMicros int64, weights []batchFeeWeight) (int64, error) {
	if feeMicros < 0 || len(weights) == 0 {
		return 0, errors.New("invalid charge batch fee allocation")
	}
	var total int64
	seen := make(map[uuid.UUID]struct{}, len(weights))
	for _, weight := range weights {
		if weight.JobID == uuid.Nil || weight.WeightMicros <= 0 ||
			total > math.MaxInt64-weight.WeightMicros {
			return 0, errors.New("invalid charge batch weight")
		}
		if _, exists := seen[weight.JobID]; exists {
			return 0, errors.New("duplicate job in charge batch fee allocation")
		}
		seen[weight.JobID] = struct{}{}
		total += weight.WeightMicros
	}
	return total, nil
}

func allocateBatchFeeMicros(feeMicros int64, weights []batchFeeWeight) ([]batchFeeAllocation, error) {
	total, err := validateBatchFeeInputs(feeMicros, weights)
	if err != nil {
		return nil, err
	}

	type remainderRank struct {
		index     int
		jobID     uuid.UUID
		remainder *big.Int
	}
	result := make([]batchFeeAllocation, len(weights))
	ranks := make([]remainderRank, len(weights))
	var floorAllocated int64
	for i, weight := range weights {
		var numerator, quotient, remainder big.Int
		numerator.Mul(big.NewInt(feeMicros), big.NewInt(weight.WeightMicros))
		quotient.QuoRem(&numerator, big.NewInt(total), &remainder)
		if !quotient.IsInt64() {
			return nil, errors.New("charge batch allocation overflow")
		}
		share := quotient.Int64()
		if floorAllocated > math.MaxInt64-share {
			return nil, errors.New("charge batch allocation overflow")
		}
		floorAllocated += share
		result[i] = batchFeeAllocation{weight.JobID, weight.WeightMicros, share}
		ranks[i] = remainderRank{
			index: i, jobID: weight.JobID, remainder: new(big.Int).Set(&remainder),
		}
	}

	// Hamilton's largest-remainder method is deterministic and order
	// independent. The earlier implementation gave the whole residual to the
	// last job, so permuting identical economic facts changed which buyer job
	// absorbed processor-fee rounding.
	residual := feeMicros - floorAllocated
	if residual < 0 || residual >= int64(len(weights)) {
		return nil, errors.New("charge batch allocation residual is invalid")
	}
	sort.Slice(ranks, func(i, j int) bool {
		if compared := ranks[i].remainder.Cmp(ranks[j].remainder); compared != 0 {
			return compared > 0
		}
		return bytes.Compare(ranks[i].jobID[:], ranks[j].jobID[:]) < 0
	})
	for i := int64(0); i < residual; i++ {
		result[ranks[i].index].AllocatedMicros++
	}
	return result, nil
}

// allocateBatchFeeMicrosLegacy reconstructs the previously shipped
// order-residual rule. It exists only to verify already-durable rows during an
// upgrade; new allocations must use allocateBatchFeeMicros.
func allocateBatchFeeMicrosLegacy(
	feeMicros int64,
	weights []batchFeeWeight,
) ([]batchFeeAllocation, error) {
	total, err := validateBatchFeeInputs(feeMicros, weights)
	if err != nil {
		return nil, err
	}
	result := make([]batchFeeAllocation, len(weights))
	var allocated int64
	for i, weight := range weights {
		share := feeMicros - allocated
		if i < len(weights)-1 && feeMicros > 0 {
			var numerator, quotient big.Int
			numerator.Mul(big.NewInt(feeMicros), big.NewInt(weight.WeightMicros))
			quotient.Quo(&numerator, big.NewInt(total))
			if !quotient.IsInt64() {
				return nil, errors.New("legacy charge batch allocation overflow")
			}
			share = quotient.Int64()
			allocated += share
		}
		result[i] = batchFeeAllocation{weight.JobID, weight.WeightMicros, share}
	}
	return result, nil
}

func (s *Store) AllocateBatchStripeFee(ctx context.Context, pi string) (bool, error) {
	if strings.TrimSpace(pi) == "" {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var batchID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM charge_batches
		WHERE stripe_pi=$1 AND status='charged' ORDER BY id LIMIT 1 FOR UPDATE`, pi).Scan(&batchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var feeMicros int64
	err = tx.QueryRow(ctx, `SELECT (-amount_usd*1000000)::bigint FROM ledger_entries
		WHERE kind='stripe_fee' AND payout_ref=$1`, pi).Scan(&feeMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT id,(billed_usd*1000000)::bigint,charge_status
		FROM jobs WHERE charge_batch_id=$1 ORDER BY created_at,id FOR UPDATE`, batchID)
	if err != nil {
		return false, err
	}
	var weights []batchFeeWeight
	for rows.Next() {
		var jobID uuid.UUID
		var weight *int64
		var status string
		if err := rows.Scan(&jobID, &weight, &status); err != nil {
			rows.Close()
			return false, err
		}
		if status != "charged" || weight == nil {
			rows.Close()
			return false, fmt.Errorf("charge batch %s has incomplete job economics", batchID)
		}
		weights = append(weights, batchFeeWeight{jobID, *weight})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	allocations, err := allocateBatchFeeMicros(feeMicros, weights)
	if err != nil {
		return false, err
	}
	existingRows, err := tx.Query(ctx, `SELECT job_id,stripe_pi,allocation_ordinal,
			allocation_method,
			(billed_weight_usd*1000000)::bigint,(allocated_fee_usd*1000000)::bigint
		FROM charge_batch_fee_allocations
		WHERE charge_batch_id=$1 ORDER BY allocation_ordinal FOR UPDATE`, batchID)
	if err != nil {
		return false, err
	}
	type storedAllocation struct {
		JobID           uuid.UUID
		StripePI        string
		Ordinal         int
		Method          string
		WeightMicros    int64
		AllocatedMicros int64
	}
	var existing []storedAllocation
	for existingRows.Next() {
		var row storedAllocation
		if err := existingRows.Scan(
			&row.JobID, &row.StripePI, &row.Ordinal,
			&row.Method, &row.WeightMicros, &row.AllocatedMicros,
		); err != nil {
			existingRows.Close()
			return false, err
		}
		existing = append(existing, row)
	}
	existingRows.Close()
	if err := existingRows.Err(); err != nil {
		return false, err
	}
	if len(existing) == 0 {
		for i, allocation := range allocations {
			_, err := tx.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
				(charge_batch_id,job_id,stripe_pi,allocation_ordinal,
				 billed_weight_usd,allocated_fee_usd,allocation_method)
				VALUES ($1,$2,$3,$4,$5::numeric/1000000,$6::numeric/1000000,$7)`,
				batchID, allocation.JobID, pi, i,
				allocation.WeightMicros, allocation.AllocatedMicros,
				batchFeeAllocationHamiltonV1)
			if err != nil {
				return false, err
			}
		}
	} else {
		if len(existing) != len(allocations) {
			return false, fmt.Errorf(
				"charge batch %s has a partial immutable fee allocation", batchID,
			)
		}
		expected := allocations
		expectedMethod := existing[0].Method
		switch expectedMethod {
		case batchFeeAllocationHamiltonV1:
		case batchFeeAllocationLegacyV0:
			expected, err = allocateBatchFeeMicrosLegacy(feeMicros, weights)
			if err != nil {
				return false, err
			}
		default:
			return false, fmt.Errorf(
				"charge batch %s has unknown immutable fee allocation method %q",
				batchID, expectedMethod,
			)
		}
		for i, allocation := range expected {
			stored := existing[i]
			if stored.JobID != allocation.JobID || stored.StripePI != pi ||
				stored.Ordinal != i || stored.Method != expectedMethod ||
				stored.WeightMicros != allocation.WeightMicros ||
				stored.AllocatedMicros != allocation.AllocatedMicros {
				return false, fmt.Errorf(
					"charge batch %s immutable fee allocation differs at ordinal %d",
					batchID, i,
				)
			}
		}
	}
	var allocated int64
	err = tx.QueryRow(ctx, `SELECT (COALESCE(SUM(allocated_fee_usd),0)*1000000)::bigint
		FROM charge_batch_fee_allocations WHERE charge_batch_id=$1`, batchID).Scan(&allocated)
	if err != nil {
		return false, err
	}
	if allocated != feeMicros {
		return false, fmt.Errorf("charge batch fee conservation failed: allocated=%d fee=%d", allocated, feeMicros)
	}
	return true, tx.Commit(ctx)
}

func (s *Store) BatchStripeFeesMissingAllocations(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT cb.stripe_pi FROM charge_batches cb
		JOIN ledger_entries fee ON fee.kind='stripe_fee' AND fee.payout_ref=cb.stripe_pi
		WHERE cb.status='charged' AND COALESCE(cb.stripe_pi,'')<>'' AND (
		(SELECT COUNT(*) FROM charge_batch_fee_allocations a WHERE a.charge_batch_id=cb.id)
		<>(SELECT COUNT(*) FROM jobs j WHERE j.charge_batch_id=cb.id) OR
		COALESCE((SELECT SUM(a.allocated_fee_usd) FROM charge_batch_fee_allocations a
		WHERE a.charge_batch_id=cb.id),-1)<>-fee.amount_usd)
		ORDER BY cb.charged_at,cb.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var pi string
		if err := rows.Scan(&pi); err != nil {
			return nil, err
		}
		result = append(result, pi)
	}
	return result, rows.Err()
}
