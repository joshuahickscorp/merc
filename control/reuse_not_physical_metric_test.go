package main

import (
	"os"
	"strings"
	"testing"
)

// TestZeroWorkDeliveriesDoNotCountAsVerifiedExecutions pins the separation
// between a realtime delivery that ran a model and one that did not.
//
// merc_realtime_contracts_verified_total is the only exported series for this
// lane and its help text claims V0 verification and settlement. An exact-reuse
// hit and a coalesced follower both return a body with no model run and no
// supplier credited. While all three shared one counter, that series could not
// distinguish work done from work avoided, and any throughput read off it
// counted reuse as physical work — the one thing this repository refuses to do.
//
// This is a source assertion rather than a behavioural one because reaching
// either branch needs a live store plus object storage; the branches are three
// lines long and the defect is which identifier they name.
func TestZeroWorkDeliveriesDoNotCountAsVerifiedExecutions(t *testing.T) {
	raw, err := os.ReadFile("realtime.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")

	for _, zeroWork := range []struct {
		header      string
		wantCounter string
	}{
		{`X-Merc-Exact-Reuse`, "metrics.realtimeReuseDeliveries.Add"},
		{`X-Merc-Coalesced`, "metrics.realtimeCoalescedDeliveries.Add"},
	} {
		start := -1
		for i, line := range lines {
			if strings.Contains(line, `w.Header().Set("`+zeroWork.header+`"`) {
				start = i
				break
			}
		}
		if start < 0 {
			t.Errorf("%s response header not found; if the zero-work delivery path "+
				"was removed, delete this assertion too rather than leaving a guard "+
				"that no longer guards anything", zeroWork.header)
			continue
		}

		// The branch runs from the header write to its return.
		found := ""
		for i := start; i < len(lines) && i < start+20; i++ {
			if strings.Contains(lines[i], "metrics.realtime") &&
				strings.Contains(lines[i], ".Add(") {
				found = strings.TrimSpace(lines[i])
			}
			if strings.TrimSpace(lines[i]) == "return" {
				break
			}
		}
		switch {
		case found == "":
			t.Errorf("%s branch increments no counter; a zero-work delivery that is "+
				"invisible in metrics is the opposite defect but still a defect",
				zeroWork.header)
		case strings.Contains(found, "metrics.realtimeVerified.Add"):
			t.Errorf("%s branch increments the physical-execution counter (%s). "+
				"No model ran and no supplier was credited on this path; it must "+
				"increment %s so reuse is never read as throughput.",
				zeroWork.header, found, zeroWork.wantCounter)
		case !strings.Contains(found, zeroWork.wantCounter):
			t.Errorf("%s branch increments %s, want %s",
				zeroWork.header, found, zeroWork.wantCounter)
		}
	}
}
