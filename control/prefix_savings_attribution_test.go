package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrefixReuseClaimsNoSavingsUntilItIsAttributed is a tripwire on a claim, not
// on behaviour.
//
// Prefix locality is live production ROUTING: the claim ORDER BY ranks
// warm_prefix_depth and warm_for_task below the cost-class terms
// (control/scheduler.go:1093-1108), so a warm worker can win a cost tie. That part
// is real and measured — evidence/perf/prefix-two-worker-latest.json has physical
// prompt_ms and wall_ms.
//
// The SAVINGS are a different claim, and they are not attributed. ClassPrefixReusedInput
// exists with the right semantics — physicalClasses marks it false, meaning those
// tokens did not consume supplier compute — but nothing in production ever writes
// it. The engine's usage.prompt_tokens_details.cached_tokens signal is parsed only
// by the gateway parity harness and CLI, which are bench tooling; the production
// realtime path never reads it. So no job records a prefix-reuse split and no
// receipt is backed by one.
//
// Network V2 Step 25 requires every ENABLED work-elimination class to have
// production callers AND receipt-backed savings. Prefix currently satisfies the
// first through routing and not the second. The honest position, recorded in the
// Step 25 shape note, is that prefix routing claims NO savings.
//
// This test fails the moment someone attributes the class, because at that point
// the claim becomes available and the plan and the savings language must be
// updated together rather than one drifting ahead of the other.
func TestPrefixReuseClaimsNoSavingsUntilItIsAttributed(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var attributors []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// billing_classes.go declares the class and its physical-ness; that is the
		// definition, not an attribution of savings to a job.
		if name == "billing_classes.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(src), "ClassPrefixReusedInput") {
			attributors = append(attributors, name)
		}
	}
	if len(attributors) > 0 {
		t.Fatalf("ClassPrefixReusedInput is now attributed in production %v: prefix savings are "+
			"claimable, so update the Step 25 shape note in docs/archive/engineering/NETWORK_V2_EXECUTION_PLAN.md, "+
			"which currently records prefix as routing-only with no claimed savings", attributors)
	}
}
