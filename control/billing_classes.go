package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Billing classes separate what the buyer receives from what merc computed.
//
// Prefix reuse, exact caching and coalescing all deliver tokens for less
// physical work. Charging one undifferentiated per-token price either
// overcharges the buyer for work nobody did, or underpays the supplier for work
// they actually performed. It is the same conflation that inflated the retired
// 145x claim, expressed as money instead of as a benchmark.

const (
	// Physical: merc executed the model for these.
	ClassUncachedInput   = "uncached_input"
	ClassGeneratedOutput = "generated_output"
	// Logical: delivered to the buyer without new model execution.
	ClassPrefixReusedInput = "prefix_reused_input"
	ClassExactCachedInput  = "exact_cached_input"
	ClassExactResultReuse  = "exact_result_reuse"
)

// physicalClasses is the authority for which classes consumed supplier compute.
var physicalClasses = map[string]bool{
	ClassUncachedInput:     true,
	ClassGeneratedOutput:   true,
	ClassPrefixReusedInput: false,
	ClassExactCachedInput:  false,
	ClassExactResultReuse:  false,
}

// TokenAccounting is one job's tokens split by class.
type TokenAccounting map[string]int64

// PhysicalTokens is the work the supplier actually performed, and therefore the
// only quantity that may be described as inference throughput.
func (t TokenAccounting) PhysicalTokens() int64 {
	var n int64
	for class, tokens := range t {
		if physicalClasses[class] {
			n += tokens
		}
	}
	return n
}

// DeliveredTokens is everything the buyer received, physical or reused.
func (t TokenAccounting) DeliveredTokens() int64 {
	var n int64
	for _, tokens := range t {
		n += tokens
	}
	return n
}

// ReuseRatio is delivered/physical: 1.0 means every delivered token was
// computed, 3.0 means two thirds were served from reuse. It is a cost ratio,
// never a speed multiple.
func (t TokenAccounting) ReuseRatio() float64 {
	p := t.PhysicalTokens()
	if p == 0 {
		return 0
	}
	return float64(t.DeliveredTokens()) / float64(p)
}

// reuseDiscountShare is the fraction of an avoided-work saving passed to the
// buyer. The rest splits between supplier earnings and merc contribution, so an
// efficiency gain is shared rather than silently retained or wholly given away.
const reuseDiscountShare = 0.60

// PriceReusedTokens returns what to charge for tokens that were delivered
// without new model execution.
//
// Not free: merc still stores, indexes, looks up, delivers and verifies the
// reused content, and the price must stay above that cost or reuse becomes a
// way to lose money faster.
func PriceReusedTokens(fullPricePer1K float64, tokens int64) float64 {
	if tokens <= 0 || fullPricePer1K <= 0 {
		return 0
	}
	retained := 1.0 - reuseDiscountShare
	return float64(tokens) / 1000.0 * fullPricePer1K * retained
}

// PriceAccounting prices a job across all classes: physical work at the full
// catalogue rate, reused work at the discounted rate.
//
// The result is clamped to the full-rate ceiling. Found by fuzzing: the ledger
// rounds to micro-USD, and at catalogue prices a job's charge is frequently
// BELOW one micro-USD, so rounding to nearest can land above what charging
// every delivered token at the full rate would have cost. Reuse must never make
// a job more expensive than no reuse -- that would invert the whole point and
// would be invisible at these magnitudes.
func PriceAccounting(t TokenAccounting, fullPricePer1K float64) float64 {
	var total float64
	for class, tokens := range t {
		if tokens <= 0 {
			continue
		}
		if physicalClasses[class] {
			total += float64(tokens) / 1000.0 * fullPricePer1K
			continue
		}
		total += PriceReusedTokens(fullPricePer1K, tokens)
	}
	rounded := roundUSD(total)
	if ceiling := roundUSD(float64(t.DeliveredTokens()) / 1000.0 * fullPricePer1K); rounded > ceiling {
		return ceiling
	}
	return rounded
}

// RecordTokenAccounting persists the split for a job.
func (s *Store) RecordTokenAccounting(ctx context.Context, jobID uuid.UUID, t TokenAccounting) error {
	for class, tokens := range t {
		if _, ok := physicalClasses[class]; !ok {
			return fmt.Errorf("unknown billing token class %q", class)
		}
		if tokens < 0 {
			return fmt.Errorf("negative token count %d for class %q", tokens, class)
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO job_token_accounting (job_id, class, tokens)
			VALUES ($1, $2, $3)
			ON CONFLICT (job_id, class) DO UPDATE SET tokens = EXCLUDED.tokens`,
			jobID, class, tokens); err != nil {
			return fmt.Errorf("recording %s tokens for job %s: %w", class, jobID, err)
		}
	}
	return nil
}

// JobTokenAccounting reads a job's split back.
func (s *Store) JobTokenAccounting(ctx context.Context, jobID uuid.UUID) (TokenAccounting, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT class, tokens FROM job_token_accounting WHERE job_id=$1`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := TokenAccounting{}
	for rows.Next() {
		var class string
		var tokens int64
		if err := rows.Scan(&class, &tokens); err != nil {
			return nil, err
		}
		out[class] = tokens
	}
	return out, rows.Err()
}
