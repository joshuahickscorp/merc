package main

import (
	"testing"
	"time"
)

// The measured frontier this policy encodes: past 8,192 tokens in flight,
// throughput is flat and latency is not. Batch 256 buys 2.3% throughput for
// 310% TTFT.

func TestBudgetIsTokensNotRequests(t *testing.T) {
	budget := TokenBudgetFor(LatencyBatch)

	// 64 short prompts fit comfortably.
	short := make([]BatchCandidate, 64)
	for i := range short {
		short[i] = BatchCandidate{ID: "s", PromptTokens: 128}
	}
	chosen, used := SelectBatch(short, budget)
	if len(chosen) != 64 {
		t.Fatalf("64x128 = 8192 tokens should all fit, got %d", len(chosen))
	}
	if used != 8192 {
		t.Fatalf("used %d tokens, want 8192", used)
	}

	// The same REQUEST count of long prompts must not, because the cost is
	// tokens. This is precisely what a request-count budget got wrong.
	long := make([]BatchCandidate, 64)
	for i := range long {
		long[i] = BatchCandidate{ID: "l", PromptTokens: 1024}
	}
	chosenLong, usedLong := SelectBatch(long, budget)
	if len(chosenLong) >= 64 {
		t.Fatalf("64x1024 = 65536 tokens must not all admit, got %d", len(chosenLong))
	}
	if usedLong > budget {
		t.Fatalf("admitted %d tokens over a %d budget", usedLong, budget)
	}
}

// Reused prefix tokens cost no prefill, so they must not consume the budget --
// otherwise cache hits would shrink the batch instead of enlarging it.
func TestWarmPrefixDoesNotConsumeBudget(t *testing.T) {
	budget := TokenBudgetFor(LatencyBatch)

	cold := make([]BatchCandidate, 16)
	warm := make([]BatchCandidate, 16)
	for i := range cold {
		cold[i] = BatchCandidate{ID: "c", PromptTokens: 1024}
		warm[i] = BatchCandidate{ID: "w", PromptTokens: 1024, ReusablePrefixTokens: 960}
	}

	coldChosen, coldUsed := SelectBatch(cold, budget)
	warmChosen, warmUsed := SelectBatch(warm, budget)

	if len(warmChosen) <= len(coldChosen) {
		t.Fatalf("warm prefixes should pack more work per batch: warm %d vs cold %d",
			len(warmChosen), len(coldChosen))
	}
	if warmUsed > budget || coldUsed > budget {
		t.Fatalf("budget exceeded: warm %d cold %d budget %d", warmUsed, coldUsed, budget)
	}
	if len(warmChosen) != 16 {
		t.Fatalf("16 prompts with only 64 new tokens each = 1024 tokens; all should fit, got %d",
			len(warmChosen))
	}
}

// A prompt larger than the entire budget must still be servable, or long
// requests starve forever behind cheaper ones.
func TestOversizedCandidateIsAdmittedAlone(t *testing.T) {
	budget := TokenBudgetFor(LatencyInteractive)
	huge := BatchCandidate{ID: "huge", PromptTokens: budget * 3}

	chosen, used := SelectBatch([]BatchCandidate{huge}, budget)
	if len(chosen) != 1 || chosen[0].ID != "huge" {
		t.Fatalf("an oversized prompt must be admitted alone, got %v", chosen)
	}
	if used != budget*3 {
		t.Fatalf("reported %d tokens, want %d", used, budget*3)
	}

	// Mixed with cheap work, the batch fills with what fits rather than
	// blocking on the giant one.
	mixed := []BatchCandidate{huge}
	for i := 0; i < 8; i++ {
		mixed = append(mixed, BatchCandidate{ID: "small", PromptTokens: 128})
	}
	chosen, used = SelectBatch(mixed, budget)
	if used > budget {
		t.Fatalf("admitted %d over budget %d", used, budget)
	}
	for _, c := range chosen {
		if c.ID == "huge" {
			t.Fatal("the oversized prompt displaced cheaper work that fit")
		}
	}
}

func TestInteractiveBudgetIsTighterThanBatch(t *testing.T) {
	i := TokenBudgetFor(LatencyInteractive)
	b := TokenBudgetFor(LatencyBatch)
	if i >= b {
		t.Fatalf("interactive budget %d must be below batch budget %d", i, b)
	}
	// The measured point: 4,096 tokens held TTFT near 570ms.
	if est := EstimatedTTFT(i); est > 700*time.Millisecond {
		t.Fatalf("interactive budget implies TTFT %v, too slow for a waiting human", est)
	}
	if est := EstimatedTTFT(b); est < 900*time.Millisecond {
		t.Fatalf("batch budget TTFT estimate %v is implausibly fast vs the 1138ms measurement", est)
	}
}

func TestBudgetValidationRefusesImpossibleDeadlines(t *testing.T) {
	// 8,192 tokens measured at ~1138ms cannot meet a 100ms deadline.
	if err := ValidateBatchBudget(LatencyInteractive, maxBatchTokens, 100*time.Millisecond); err == nil {
		t.Fatal("a budget that cannot meet the deadline must be refused")
	}
	// A generous deadline passes.
	if err := ValidateBatchBudget(LatencyBatch, maxBatchTokens, 10*time.Second); err != nil {
		t.Fatalf("a 10s deadline should accept the batch budget: %v", err)
	}
	// No deadline means no constraint.
	if err := ValidateBatchBudget(LatencyBatch, maxBatchTokens, 0); err != nil {
		t.Fatalf("absent deadline should not constrain: %v", err)
	}
	if err := ValidateBatchBudget(LatencyBatch, 0, time.Second); err == nil {
		t.Fatal("a non-positive budget must be refused")
	}
}

func TestSelectBatchDoesNotMutateCallerSlice(t *testing.T) {
	in := []BatchCandidate{
		{ID: "big", PromptTokens: 4000},
		{ID: "small", PromptTokens: 100},
	}
	SelectBatch(in, TokenBudgetFor(LatencyBatch))
	if in[0].ID != "big" || in[1].ID != "small" {
		t.Fatalf("caller slice was reordered: %v", in)
	}
}
