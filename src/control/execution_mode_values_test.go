package main

import "testing"

// The JSON key "execution_mode" names two unrelated axes. A value added to one
// must never look like a value of the other, or a reader of a stored decision
// cannot tell which axis it is on.
func TestExecutionModeValueSetsAreDisjoint(t *testing.T) {
	network := []string{
		string(ModePool),
		string(ModeReplicaService),
		string(ModeLocalCluster),
		string(ModeCloudBackstop),
	}
	billing := []string{
		computeExecutionDistributed,
		computeExecutionExactReuse,
		pricingExecutionRealtime,
		pricingExecutionRealtimeReuse,
		pricingExecutionServiceLease,
	}

	if len(network) == 0 || len(billing) == 0 {
		t.Fatal("both axes must declare at least one value")
	}

	seen := make(map[string]string, len(network)+len(billing))
	for _, v := range network {
		if v == "" {
			t.Fatal("network execution_mode value must not be empty")
		}
		if other, ok := seen[v]; ok {
			t.Fatalf("duplicate value %q already registered on %s", v, other)
		}
		seen[v] = "network"
	}
	for _, v := range billing {
		if v == "" {
			t.Fatal("billing execution_mode value must not be empty")
		}
		if other, ok := seen[v]; ok {
			t.Fatalf("execution_mode value %q collides across axes: already on %s, also on billing — "+
				"a stored document could not tell network placement from billing path", v, other)
		}
		seen[v] = "billing"
	}
}
