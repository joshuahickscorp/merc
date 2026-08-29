package main

import (
	"strings"
	"testing"
)

func TestBatchInferCompletionTokensCountsCommittedTokensNotRecords(t *testing.T) {
	got, metered, err := batchInferCompletionTokens([]byte(`{
"job_type":"batch_infer",
"completions":[{"index":0,"tokens":2},{"index":1,"tokens":97}]
}`), 2)
	must(t, err)
	if !metered {
		t.Fatal("complete batch_infer artifact was not metered")
	}
	if got != 99 {
		t.Fatalf("meter = %d, want 99 completion tokens (not 2 records)", got)
	}
}

func TestBatchInferCompletionTokensFailsClosedWithoutExactMeter(t *testing.T) {
	for name, body := range map[string]string{
		"missing completions": `{"job_type":"batch_infer"}`,
		"missing tokens":      `{"completions":[{"index":0}]}`,
		"non integer tokens":  `{"completions":[{"tokens":1.5}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, metered, err := batchInferCompletionTokens([]byte(body), 1)
			mustf(t, err, "incomplete meter should disable caching, not fail merge: %v")
			if metered || got != 0 {
				t.Fatalf("incomplete meter = (%d, %t), want (0, false)", got, metered)
			}
		})
	}
}

func TestBatchInferCompletionTokensRefusesOverflow(t *testing.T) {
	_, metered, err := batchInferCompletionTokens([]byte(`{
"completions":[{"tokens":9223372036854775808}]
}`), 1)
	if err == nil || metered || !strings.Contains(err.Error(), "signed 64-bit") {
		t.Fatalf("overflow = (%t, %v), want unmetered signed-range error", metered, err)
	}
}
