package main

import (
	"fmt"
	"sort"
	"time"
)

// Traffic classes: the four service levels batching and queueing are governed by.
//
// Two vocabularies already existed and neither covered the question. batch_policy.go
// knew INTERACTIVE and BATCH, which are token budgets read off one measured curve.
// workload_classification.go knew standard_batch, priority_queue and trusted_supply,
// which are admission tiers. Neither says what happens when a priority batch and a
// standard batch both want the same worker, and that is the decision that makes an
// interactive request wait behind bulk work.
//
// So this file is the mapping, not a third vocabulary: both existing spellings
// resolve into one of four classes, and each class carries the policy that governs
// it. Nothing else in the tree may invent a fifth.
//
// What is MEASURED and what is POLICY is kept apart on purpose. The token budgets
// come from the curve in batch_policy.go — 4,096 tokens holds TTFT near 570ms with
// 93% of peak throughput, and 8,192 is the knee past which throughput is flat.
// Those are two measured points, so there are two budgets, and the classes that
// share one say so rather than interpolating a third number that nothing measured.
// The queue policy — how long a class may wait, whether it may be preempted, and
// what it may never delay — is a product decision and is labelled as one.

// TrafficClass is the governed service level of one unit of work.
type TrafficClass string

const (
	// TrafficInteractive is work with a human or a caller's socket waiting on it:
	// the realtime lane. Latency is the product.
	TrafficInteractive TrafficClass = "INTERACTIVE"
	// TrafficBatchPriority is batch work that paid for a deadline — the priority
	// tier and anything carrying an SLA premium.
	TrafficBatchPriority TrafficClass = "BATCH_PRIORITY"
	// TrafficBatchStandard is ordinary batch work: a deadline exists but no
	// premium was paid for it.
	TrafficBatchStandard TrafficClass = "BATCH_STANDARD"
	// TrafficBackground is interruptible work with no deadline the buyer relies
	// on. It exists to absorb idle capacity, and it is the class that yields.
	TrafficBackground TrafficClass = "BACKGROUND"
)

// TrafficClassPolicy is one class's promoted policy.
type TrafficClassPolicy struct {
	Class TrafficClass `json:"class"`

	// TokenBudget is the in-flight prefill budget for a batch in this class.
	// MEASURED: both values are points on the curve recorded in batch_policy.go.
	TokenBudget int `json:"token_budget_tokens"`

	// MaxQueueWait is how long work in this class may sit unscheduled before the
	// scheduler is considered to be failing it. POLICY.
	MaxQueueWait time.Duration `json:"max_queue_wait"`

	// Preemptible reports whether in-flight work may be interrupted to admit a
	// higher class. POLICY. Only BACKGROUND is: preempting paid batch work would
	// charge a buyer for compute that was thrown away.
	Preemptible bool `json:"preemptible"`

	// Priority orders admission when several classes want one worker. Higher wins.
	// It is not a weight and not a share — it is a strict order, because a share
	// lets bulk work take a fraction of every interactive slot.
	Priority int `json:"priority"`

	// NeverDelays names the classes this class must not push behind itself. It is
	// stated per class rather than derived from Priority so the invariant is
	// checkable directly: merc.md's exit gate is that interactive performance is
	// never sacrificed to inflate aggregate throughput, and a priority integer
	// alone does not say that.
	NeverDelays []TrafficClass `json:"never_delays"`
}

// trafficClassPolicies is the promoted table. One entry per class, no runtime
// override: a batching policy that could be widened by an environment variable is
// not a promoted policy.
var trafficClassPolicies = map[TrafficClass]TrafficClassPolicy{
	TrafficInteractive: {
		Class: TrafficInteractive, TokenBudget: interactiveBatchTokens,
		MaxQueueWait: 2 * time.Second, Preemptible: false, Priority: 40,
	},
	TrafficBatchPriority: {
		Class: TrafficBatchPriority, TokenBudget: maxBatchTokens,
		MaxQueueWait: 30 * time.Second, Preemptible: false, Priority: 30,
		NeverDelays: []TrafficClass{TrafficInteractive},
	},
	TrafficBatchStandard: {
		Class: TrafficBatchStandard, TokenBudget: maxBatchTokens,
		MaxQueueWait: 5 * time.Minute, Preemptible: false, Priority: 20,
		NeverDelays: []TrafficClass{TrafficInteractive, TrafficBatchPriority},
	},
	TrafficBackground: {
		// The same knee, not a larger budget. Throughput is flat above 8,192
		// tokens in flight, so a bigger budget for background work would buy
		// nothing and would hold memory that an interactive batch then waits for.
		Class: TrafficBackground, TokenBudget: maxBatchTokens,
		MaxQueueWait: 6 * time.Hour, Preemptible: true, Priority: 10,
		NeverDelays: []TrafficClass{
			TrafficInteractive, TrafficBatchPriority, TrafficBatchStandard,
		},
	},
}

// PolicyForTrafficClass returns the promoted policy, or an error for a class this
// authority does not declare. Fail closed: an unknown class must not silently
// receive the most generous budget in the table.
func PolicyForTrafficClass(class TrafficClass) (TrafficClassPolicy, error) {
	policy, ok := trafficClassPolicies[class]
	if !ok {
		return TrafficClassPolicy{}, fmt.Errorf("no promoted policy for traffic class %q", class)
	}
	return policy, nil
}

// DeclaredTrafficClasses lists the classes in strict admission order, highest
// priority first.
func DeclaredTrafficClasses() []TrafficClass {
	out := make([]TrafficClass, 0, len(trafficClassPolicies))
	for class := range trafficClassPolicies {
		out = append(out, class)
	}
	sort.Slice(out, func(i, j int) bool {
		return trafficClassPolicies[out[i]].Priority > trafficClassPolicies[out[j]].Priority
	})
	return out
}

// TrafficClassForWorkload resolves the class of an admitted batch workload from
// its frozen latency class and whether it bought an SLA.
//
// The frozen decision is the authority, not the request: a buyer naming a class
// would be pricing themselves into a better queue.
func TrafficClassForWorkload(latencyClass string, slaPremiumUSD float64) TrafficClass {
	switch latencyClass {
	case "priority_queue", "trusted_supply":
		return TrafficBatchPriority
	case "standard_batch":
		if slaPremiumUSD > 0 {
			// An SLA premium was charged, so the deadline was sold and the work
			// belongs in the class that protects one.
			return TrafficBatchPriority
		}
		return TrafficBatchStandard
	case "background":
		return TrafficBackground
	default:
		// Unknown spellings get the standard class, never a better one. A typo
		// must not be a free upgrade.
		return TrafficBatchStandard
	}
}

// TrafficClassForRealtime is the realtime lane's class. Stated as a function
// rather than a constant so the one caller reads as a decision, and so a future
// interruptible realtime tier has an obvious place to be added.
func TrafficClassForRealtime() TrafficClass { return TrafficInteractive }

// TokenBudgetForTrafficClass is the batching budget for a class.
func TokenBudgetForTrafficClass(class TrafficClass) (int, error) {
	policy, err := PolicyForTrafficClass(class)
	if err != nil {
		return 0, err
	}
	return policy.TokenBudget, nil
}

// ValidateTrafficClassPolicies checks the table's internal consistency.
//
// Run as a test rather than at boot because the table is a compile-time constant:
// a check that only fires in production for a value that cannot change at runtime
// is a check that never fires.
func ValidateTrafficClassPolicies() error {
	byPriority := DeclaredTrafficClasses()
	seen := map[int]TrafficClass{}
	for _, class := range byPriority {
		policy := trafficClassPolicies[class]
		if policy.Class != class {
			return fmt.Errorf("policy for %q is labelled %q", class, policy.Class)
		}
		if policy.TokenBudget <= 0 {
			return fmt.Errorf("%s has no token budget", class)
		}
		if policy.TokenBudget > maxBatchTokens {
			return fmt.Errorf(
				"%s budget %d exceeds the measured knee %d; nothing measured supports it",
				class, policy.TokenBudget, maxBatchTokens)
		}
		if policy.MaxQueueWait <= 0 {
			return fmt.Errorf("%s has no maximum queue wait", class)
		}
		if other, clash := seen[policy.Priority]; clash {
			return fmt.Errorf("%s and %s share priority %d; admission order must be strict",
				class, other, policy.Priority)
		}
		seen[policy.Priority] = class
	}
	// Every lower class must declare that it never delays every higher one, and a
	// higher class must never declare that it defers to a lower one.
	for i, class := range byPriority {
		policy := trafficClassPolicies[class]
		declared := map[TrafficClass]bool{}
		for _, higher := range policy.NeverDelays {
			declared[higher] = true
		}
		for j, other := range byPriority {
			switch {
			case j < i && !declared[other]:
				return fmt.Errorf("%s does not declare that it never delays the higher class %s",
					class, other)
			case j > i && declared[other]:
				return fmt.Errorf("%s defers to the lower class %s", class, other)
			}
		}
	}
	// Only the class with no deadline may be interrupted.
	for _, class := range byPriority {
		if trafficClassPolicies[class].Preemptible && class != TrafficBackground {
			return fmt.Errorf("%s is preemptible; interrupting work a buyer paid a deadline for "+
				"charges them for compute that was discarded", class)
		}
	}
	// The interactive budget must stay strictly below the knee, or the class that
	// exists to bound latency is batching at the throughput-optimal point.
	if trafficClassPolicies[TrafficInteractive].TokenBudget >= maxBatchTokens {
		return fmt.Errorf("interactive budget %d is at or above the throughput knee %d",
			trafficClassPolicies[TrafficInteractive].TokenBudget, maxBatchTokens)
	}
	return nil
}
