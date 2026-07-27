package main

import (
	"os"
	"strings"
	"testing"
)

// allowSkippingDBTestsEnv opts a run out of the integration suite deliberately.
const allowSkippingDBTestsEnv = "MERC_ALLOW_SKIPPING_DB_TESTS"

// requireTestDatabase returns the integration-test DSN, failing the test when it
// is absent.
//
// The tests that need a database are the ones that carry every assertion this
// project makes about money and scheduling: dispute freezes, payout claiming,
// ledger writes, admin mutation audit atomicity, realtime settlement. Guarding
// them with t.Skip meant `make ci` printed `ok` in under a second while
// executing zero lines of ledger SQL, and a green local run said nothing about
// the paths most likely to lose money.
//
// Absence of the DSN is therefore a failure, not a skip. A developer who wants
// the fast unit-only loop opts out explicitly with MERC_ALLOW_SKIPPING_DB_TESTS=1
// (see `make test-unit`); CI never sets it, so the suite cannot quietly stop
// running there.
func requireTestDatabase(t *testing.T) string {
	t.Helper()
	if url := strings.TrimSpace(os.Getenv("MERC_TEST_DATABASE_URL")); url != "" {
		return url
	}
	if os.Getenv(allowSkippingDBTestsEnv) == "1" {
		t.Skipf("MERC_TEST_DATABASE_URL is unset and %s=1", allowSkippingDBTestsEnv)
	}
	t.Fatalf("MERC_TEST_DATABASE_URL is not set.\n"+
		"This test asserts money or scheduling behaviour and must not be skipped silently.\n"+
		"Start a database with `make dev-up` and export MERC_TEST_DATABASE_URL, or run\n"+
		"`make test-unit` to opt out deliberately (%s=1).", allowSkippingDBTestsEnv)
	return ""
}
