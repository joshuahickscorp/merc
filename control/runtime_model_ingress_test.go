package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeJobRejectsExplicitNoncanonicalModelKindBeforeSideEffects(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	// The TEST_ONLY current batch cell is gguf. Naming hf is still a 400: the
	// wire-kind fix accepts any advertised kind, not any kind in the document.
	_, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "batch_infer"},
		Model:   ModelRef{Kind: "hf", Ref: "llama-3.2-1b-instruct-q4"},
	})
	if herr == nil || herr.status != http.StatusBadRequest ||
		!strings.Contains(herr.msg, `no advertised cell serving model.kind="hf"`) {
		t.Fatalf("createJob mismatch result=%v, want unadvertised-kind 400", herr)
	}
}

func TestExplicitNoncanonicalModelKindNeverReachesPlacement(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	_, herr := normalizeAndValidateJobSubmit(jobSubmit{
		JobType: JobType{Type: "batch_infer", MaxTokens: 16},
		Model:   ModelRef{Kind: "hf", Ref: "llama-3.2-1b-instruct-q4"},
	})
	if herr == nil || herr.status != http.StatusBadRequest ||
		!strings.Contains(herr.msg, `no advertised cell serving model.kind="hf"`) {
		t.Fatalf("placement mismatch result=%v, want unadvertised-kind 400", herr)
	}
}
