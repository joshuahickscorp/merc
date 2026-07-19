package main

import "testing"

func TestFailureTaxonomyClassification(t *testing.T) {
	for _, c := range []string{"bad_input", "bad_jsonl", "unsupported_model", "unsupported_job_type"} {
		p, known := classifyFailure(c)
		if !known {
			t.Fatalf("%q must be a known class", c)
		}
		if p.retryable {
			t.Fatalf("%q is buyer fault and must be terminal (not retryable)", c)
		}
		if !p.buyerFault {
			t.Fatalf("%q must be marked buyer_fault", c)
		}
	}
	for _, c := range []string{"oom", "model_load_failed", "timeout", "worker_shutdown", "transient_io", "object_store_error", "internal_error"} {
		p, known := classifyFailure(c)
		if !known || !p.retryable || p.buyerFault {
			t.Fatalf("%q must be a known retryable system failure, got %+v known=%v", c, p, known)
		}
	}
	if _, known := classifyFailure("totally_made_up"); known {
		t.Fatal("unknown class must report known=false")
	}
}
