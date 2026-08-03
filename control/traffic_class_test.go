package main

import (
	"testing"
	"time"
)

// The promoted table has to be internally consistent, and this is where that is
// checked: the values are compile-time constants, so a boot-time check would
// never fire.
func TestTrafficClassPoliciesAreConsistent(t *testing.T) {
	if err := ValidateTrafficClassPolicies(); err != nil {
		t.Fatalf("promoted traffic-class table is inconsistent: %v", err)
	}
	if got := len(DeclaredTrafficClasses()); got != 4 {
		t.Fatalf("declared traffic classes = %d, want the four merc.md names", got)
	}
	if first := DeclaredTrafficClasses()[0]; first != TrafficInteractive {
		t.Fatalf("highest-priority class = %s, want INTERACTIVE", first)
	}
}

// The exit gate: interactive latency is never traded for aggregate throughput.
func TestInteractiveIsNeverBatchedAtTheThroughputKnee(t *testing.T) {
	interactive, err := PolicyForTrafficClass(TrafficInteractive)
	if err != nil {
		t.Fatal(err)
	}
	if interactive.TokenBudget >= maxBatchTokens {
		t.Fatalf("interactive budget %d is at the throughput knee %d",
			interactive.TokenBudget, maxBatchTokens)
	}
	// And that budget must actually hold a latency contract a human notices.
	if est := EstimatedTTFT(interactive.TokenBudget); est > time.Second {
		t.Fatalf("interactive budget implies TTFT ~%v", est.Round(time.Millisecond))
	}
	if err := ValidateBatchBudget(
		LatencyInteractive, interactive.TokenBudget, interactive.MaxQueueWait,
	); err != nil {
		t.Fatalf("interactive budget violates its own queue wait: %v", err)
	}
	for _, class := range []TrafficClass{
		TrafficBatchPriority, TrafficBatchStandard, TrafficBackground,
	} {
		policy, err := PolicyForTrafficClass(class)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Priority >= interactive.Priority {
			t.Fatalf("%s outranks or ties INTERACTIVE", class)
		}
		var declares bool
		for _, higher := range policy.NeverDelays {
			declares = declares || higher == TrafficInteractive
		}
		if !declares {
			t.Fatalf("%s does not declare that it never delays INTERACTIVE", class)
		}
	}
}

// A class nobody declared must not fall through to the widest budget.
func TestUnknownTrafficClassFailsClosed(t *testing.T) {
	if _, err := PolicyForTrafficClass(TrafficClass("PLATINUM")); err == nil {
		t.Fatal("an undeclared class received a policy")
	}
	if _, err := TokenBudgetForTrafficClass(TrafficClass("")); err == nil {
		t.Fatal("the empty class received a token budget")
	}
}

// Resolution comes off the frozen decision, and an unrecognised spelling is never
// an upgrade.
func TestTrafficClassResolvesFromTheFrozenDecision(t *testing.T) {
	for _, tc := range []struct {
		latency string
		sla     float64
		want    TrafficClass
	}{
		{"priority_queue", 0, TrafficBatchPriority},
		{"trusted_supply", 0, TrafficBatchPriority},
		{"standard_batch", 0, TrafficBatchStandard},
		// A paid deadline buys the class that protects a deadline.
		{"standard_batch", 0.01, TrafficBatchPriority},
		{"background", 0, TrafficBackground},
		{"", 0, TrafficBatchStandard},
		{"gold_tier_please", 0, TrafficBatchStandard},
	} {
		if got := TrafficClassForWorkload(tc.latency, tc.sla); got != tc.want {
			t.Fatalf("TrafficClassForWorkload(%q, %v) = %s, want %s",
				tc.latency, tc.sla, got, tc.want)
		}
	}
	if got := TrafficClassForRealtime(); got != TrafficInteractive {
		t.Fatalf("realtime class = %s", got)
	}
}

// Every latency class the batch classifier can freeze must resolve to a declared
// traffic class. This is the test that fails if a new tier is added upstream and
// nobody gives it a queue policy.
func TestEveryFrozenLatencyClassHasAPolicy(t *testing.T) {
	for _, latency := range []string{"standard_batch", "priority_queue", "trusted_supply"} {
		class := TrafficClassForWorkload(latency, 0)
		if _, err := PolicyForTrafficClass(class); err != nil {
			t.Fatalf("latency class %q resolved to %s, which has no policy: %v",
				latency, class, err)
		}
	}
}

// Class comes from the contract, not from a free caller hint. Realtime is
// always INTERACTIVE; batch class tracks the paid tier and SLA premium.
func TestTrafficClassDerivedFromContractNotCallerHint(t *testing.T) {
	if got := TrafficClassForAdmittedWork(true, "background", 0); got != TrafficInteractive {
		t.Fatalf("realtime product must be INTERACTIVE, got %s (caller latency spelling ignored)", got)
	}
	if got := TrafficClassForAdmittedWork(false, "priority_queue", 0); got != TrafficBatchPriority {
		t.Fatalf("paid priority tier = %s, want BATCH_PRIORITY", got)
	}
	if got := TrafficClassForAdmittedWork(false, "standard_batch", 0); got != TrafficBatchStandard {
		t.Fatalf("standard batch = %s, want BATCH_STANDARD", got)
	}
	if got := TrafficClassForAdmittedWork(false, "standard_batch", 1.25); got != TrafficBatchPriority {
		t.Fatalf("SLA premium must upgrade standard_batch to BATCH_PRIORITY, got %s", got)
	}
	if got := TrafficClassForAdmittedWork(false, "background", 0); got != TrafficBackground {
		t.Fatalf("background = %s", got)
	}
	// A free self-label does not upgrade: unknown spelling is standard, not interactive.
	if got := TrafficClassForAdmittedWork(false, "INTERACTIVE", 0); got != TrafficBatchStandard {
		t.Fatalf("unknown latency spelling must not become INTERACTIVE, got %s", got)
	}
}

// WorkloadDecision.ResolvedTrafficClass combines the frozen latency class with
// the SLA premium charged on the same admission.
func TestWorkloadDecisionResolvesTrafficClassWithSLA(t *testing.T) {
	d := WorkloadDecision{LatencyClass: "standard_batch"}
	if got := d.ResolvedTrafficClass(0); got != TrafficBatchStandard {
		t.Fatalf("no premium = %s", got)
	}
	if got := d.ResolvedTrafficClass(0.5); got != TrafficBatchPriority {
		t.Fatalf("with premium = %s", got)
	}
}

// Join windows are class-derived and deadline-clamped. Proven by test.
func TestJoinWindowDerivedFromClassAndDeadline(t *testing.T) {
	now := time.Now()
	// Interactive default is near-zero (concurrent admits only).
	if w := defaultJoinWindowForClass(TrafficInteractive); w != interactiveArrivalJoinWindow {
		t.Fatalf("interactive default window = %v", w)
	}
	// Generous deadline keeps the class default.
	if w := JoinWindowForArrival(TrafficBatchStandard, now.Add(time.Minute), now, 128, 0); w != batchStandardArrivalJoinWindow {
		t.Fatalf("standard window = %v, want %v", w, batchStandardArrivalJoinWindow)
	}
	// Tight deadline collapses the window.
	if w := JoinWindowForArrival(TrafficInteractive, now.Add(time.Millisecond), now, 4096, 50*time.Millisecond); w != 0 {
		t.Fatalf("tight deadline must yield zero window, got %v", w)
	}
	// Configured override is still deadline-clamped.
	if w := JoinWindowForArrival(TrafficBackground, now.Add(20*time.Millisecond), now, 100, time.Second); w >= time.Second {
		t.Fatalf("configured window must be clamped by deadline, got %v", w)
	}
}
