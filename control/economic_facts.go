package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	economicInputSourceSubmitStream    = "submit_stream_exact_raw_bytes_and_nonblank_jsonl_records"
	economicOutputSourceMergedArtifact = "merged_artifact_exact_bytes_and_records"
)

func roundEconomicUSD(v float64) float64 { return math.Round(v*1_000_000) / 1_000_000 }

type batchFeeWeight struct {
	JobID        uuid.UUID
	WeightMicros int64
}

type batchFeeAllocation struct {
	JobID           uuid.UUID
	WeightMicros    int64
	AllocatedMicros int64
}

func allocateBatchFeeMicros(feeMicros int64, weights []batchFeeWeight) ([]batchFeeAllocation, error) {
	if feeMicros < 0 || len(weights) == 0 {
		return nil, errors.New("invalid charge batch fee allocation")
	}
	var total int64
	for _, weight := range weights {
		if weight.WeightMicros <= 0 || total > math.MaxInt64-weight.WeightMicros {
			return nil, errors.New("invalid charge batch weight")
		}
		total += weight.WeightMicros
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
				return nil, errors.New("charge batch allocation overflow")
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
	if _, err := tx.Exec(ctx, `DELETE FROM charge_batch_fee_allocations WHERE charge_batch_id=$1`, batchID); err != nil {
		return false, err
	}
	for i, allocation := range allocations {
		_, err := tx.Exec(ctx, `INSERT INTO charge_batch_fee_allocations
			(charge_batch_id,job_id,stripe_pi,allocation_ordinal,billed_weight_usd,allocated_fee_usd)
			VALUES ($1,$2,$3,$4,$5::numeric/1000000,$6::numeric/1000000)`,
			batchID, allocation.JobID, pi, i, allocation.WeightMicros, allocation.AllocatedMicros)
		if err != nil {
			return false, err
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
