package main

import "testing"

func installRealtimeCADFXForTest(t *testing.T) {
	t.Helper()
	t.Setenv(priceFXRateEnv, "1.37")
	t.Setenv(priceFXRevisionEnv, "realtime-test-cad-usd-2026-08-09")
}
