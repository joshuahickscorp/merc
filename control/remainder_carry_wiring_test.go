package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemainderCarryHasNoProductionCaller is a tripwire on a documentation claim,
// not on behaviour.
//
// docs/PROGRAMME.md describes RemainderCarry as the accrual ledger's fractional
// memory: 10,000 accruals of 17 nanos post 170 micros and lose nothing. That is
// true of the type and money_nanos_test.go proves it. It is NOT true of the
// running system, because production settlement projects each leg independently
// with projectNanosToMicros / LedgerMicrosFromNanos and rounds per post
// (payment.go, realtime_store.go). PROGRAMME.md now says so explicitly.
//
// The hazard is that someone wires the accrual into settlement and the document
// keeps reading as if it always described the system — or, worse, that the
// document is quoted as authority for "the ledger conserves exact nanos per task
// without residual loss" while nothing carries a remainder. This test fails the
// moment a production caller appears, so the claim and the code cannot drift
// apart silently in either direction.
func TestRemainderCarryHasNoProductionCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var callers []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(src)
		// The definition itself lives in money_nanos.go; a *use* is what matters.
		if name == "money_nanos.go" {
			continue
		}
		if strings.Contains(body, "NewRemainderCarry") {
			callers = append(callers, name)
		}
	}
	if len(callers) > 0 {
		t.Fatalf("RemainderCarry now has production callers %v: update docs/PROGRAMME.md, which "+
			"currently states that production settlement does NOT post through it and that no "+
			"exact-nanos-per-task conservation claim may be made until it does", callers)
	}
}
