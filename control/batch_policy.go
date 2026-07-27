package main

import (
	"fmt"
	"time"
)

// Token-budget batching.
//
// Batching was previously bounded by request count, which is the wrong unit: a
// batch of 64 short prompts and a batch of 64 long ones do wildly different
// amounts of prefill, and prefill is 76% of wall time at the operating point.
//
// Measured on the 8-bit authority (128-token prompts, 32 output, M3 Ultra):
//
//	batch   physical t/s   TTFT ms   tokens
//	   32          6,047       571    4,096
//	   64          6,495     1,138    8,192
//	  128          6,552     2,337   16,384
//	  256          6,646     4,666   32,768
//
// Past 8,192 tokens the curve is flat and the latency is not: batch 256 buys
// 2.3% more throughput for 310% more TTFT, i.e. ~134% latency per 1%
// throughput. The knee is 8,192 tokens in flight, and it is a TOKEN budget, not
// a request budget.
const (
	// maxBatchTokens is the measured knee. Above it, throughput is flat.
	maxBatchTokens = 8192

	// interactiveBatchTokens bounds a latency-class batch. From the same
	// measurement, 4,096 tokens holds TTFT near 570ms while retaining 93% of
	// peak throughput -- the right trade when a human is waiting.
	interactiveBatchTokens = 4096
)

// LatencyClass selects which budget applies.
type LatencyClass string

const (
	LatencyInteractive LatencyClass = "INTERACTIVE"
	LatencyBatch       LatencyClass = "BATCH"
)

// TokenBudgetFor returns the in-flight token budget for a latency class.
func TokenBudgetFor(class LatencyClass) int {
	if class == LatencyInteractive {
		return interactiveBatchTokens
	}
	return maxBatchTokens
}

// BatchCandidate is one unit of pending work.
type BatchCandidate struct {
	ID           string
	PromptTokens int
	OutputTokens int
	// ReusablePrefixTokens is what a target worker already holds warm, from
	// DeepestWarmPrefix. Reused tokens cost no prefill, so they do not consume
	// the budget -- which is the whole point of measuring the budget in tokens.
	ReusablePrefixTokens int
}

// prefillTokens is the work this candidate actually adds to a batch.
func (c BatchCandidate) prefillTokens() int {
	n := c.PromptTokens - c.ReusablePrefixTokens
	if n < 0 {
		return 0
	}
	return n
}

// SelectBatch chooses the largest set of candidates whose combined new prefill
// fits the budget.
//
// Candidates whose prefix is already warm cost little and pack densely; a cold
// 2,000-token prompt consumes a quarter of the budget alone. Ordering by
// ascending prefill cost fills the batch with the cheapest available work,
// which both maximises the number of requests served per batch and keeps TTFT
// bounded.
//
// Returns the selected candidates and the prefill tokens they consume.
func SelectBatch(candidates []BatchCandidate, budget int) ([]BatchCandidate, int) {
	if budget <= 0 {
		return nil, 0
	}
	// Stable copy ordered by cost; the caller's slice is not mutated.
	ordered := make([]BatchCandidate, len(candidates))
	copy(ordered, candidates)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j].prefillTokens() < ordered[j-1].prefillTokens(); j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}

	var chosen []BatchCandidate
	used := 0
	for _, c := range ordered {
		cost := c.prefillTokens()
		// A single candidate larger than the whole budget must still be
		// servable, or long prompts starve forever. Admit it alone.
		if cost > budget {
			if len(chosen) == 0 {
				return []BatchCandidate{c}, cost
			}
			continue
		}
		if used+cost > budget {
			continue
		}
		chosen = append(chosen, c)
		used += cost
	}
	return chosen, used
}

// EstimatedTTFT projects time-to-first-token for a batch of the given prefill
// size, from the measured linear prefill curve.
//
// Prefill measured at O(n^1.01) across 64-1024 token prompts, ~147us per token
// per stream at batch 64. Linear is a good enough model over the operating
// range; it exists to bound admission, not to be a simulator.
func EstimatedTTFT(prefillTokens int) time.Duration {
	const microsPerToken = 0.1389 // 1138ms / 8192 tokens, measured at the knee
	return time.Duration(float64(prefillTokens) * microsPerToken * float64(time.Millisecond))
}

// ValidateBatchBudget rejects a budget that would violate a latency contract.
func ValidateBatchBudget(class LatencyClass, budget int, deadline time.Duration) error {
	if budget <= 0 {
		return fmt.Errorf("token budget must be positive, got %d", budget)
	}
	if deadline <= 0 {
		return nil
	}
	if est := EstimatedTTFT(budget); est > deadline {
		return fmt.Errorf(
			"token budget %d implies TTFT ~%v which exceeds the %v deadline for class %s",
			budget, est.Round(time.Millisecond), deadline, class)
	}
	return nil
}
