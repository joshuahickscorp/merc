package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Step 27 closes each of the four supply fabrics as either a real decision path
// with a receipt, or an explicit refusal. Two are refusals, and a refusal that
// lives only in a comment is one careless edit from silently becoming a live
// mode with no evidence behind it.
//
//	POOL             real. buildBatchTopologyDecision accepts independent
//	                 fan-out at degree 1; the decision is a receipt.
//	REPLICA_SERVICE  real. buildRealtimeTopologyDecision cites the frozen
//	                 RealtimePlacementPlan; GetServiceLeaseReceipt is the receipt.
//	LOCAL_CLUSTER    REFUSED BY CONSTRUCTION. Already pinned by
//	                 topology_decision_test.go and the integration test: an
//	                 accepted decision must carry the construction refusal.
//	CLOUD_BACKSTOP   REFUSED BY ABSENT AUTHORITY, and that is what this file
//	                 pins.
//
// CLOUD_BACKSTOP is the subtle one. Its decision logic is fully written in
// execution_mode.go — it can emit ModeCloudBackstop and it has real refusal
// reasons — so reading that file suggests a live mode. It is not live: every
// production caller passes CloudBackstopPermitted=false, so the branch is
// unreachable in production and no receipt can ever cite it.
//
// That distinction is the entire claim. "The code exists" and "the network can
// choose it" are different statements, and only the second one would let a
// provider bill a buyer. Flipping any of these to true without provider cost,
// privacy/region terms and deadline evidence is exactly the change this test
// exists to stop.
func TestCloudBackstopIsNeverPermittedByAProductionPath(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read control/: %v", err)
	}
	assignment := regexp.MustCompile(`CloudBackstopPermitted:\s*([A-Za-z0-9_.]+)`)

	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range assignment.FindAllStringSubmatch(string(raw), -1) {
			value := m[1]
			found++
			switch value {
			case "false":
				// The production shape: the mode is decided against.
			case "req.CloudBackstopPermitted":
				// Pass-through inside the planner; its callers are the sites
				// above, and they are the ones that must stay false.
			default:
				t.Fatalf("%s permits CLOUD_BACKSTOP (CloudBackstopPermitted: %s). "+
					"Step 27 closes that mode as refused because no production path permits it, "+
					"and a receipt citing it would be a provider charge with no measured provider "+
					"premium, no privacy/region terms and no deadline evidence behind it. "+
					"If the mode is genuinely being opened, close Step 27's refusal first.",
					name, value)
			}
		}
	}
	if found == 0 {
		t.Fatal("no CloudBackstopPermitted assignment found at all — this guard has stopped " +
			"watching anything, which is worse than it failing")
	}
	t.Logf("checked %d CloudBackstopPermitted assignments; none permits the mode", found)
}
