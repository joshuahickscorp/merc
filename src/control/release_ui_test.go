package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseUIRejectsNonLoopbackListener(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8080", "192.0.2.1:8080", "[::]:8080"} {
		if err := releaseUIAddressAllowed(address); err == nil {
			t.Fatalf("public release UI address accepted: %s", address)
		}
	}
	for _, address := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		mustf(t, releaseUIAddressAllowed(address), "loopback release UI address rejected %s: %v", address)
	}
}

func TestReleaseUIOnlyReflectsWhitelistedState(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, ".merc-release"), 0o700))
	state := `{"schema_version":2,"status":"planned","plan":{"plan_sha256":"plan"},"raw_secret":"super-secret-value"}`
	must(t, os.WriteFile(releaseStatePath(root), []byte(state), 0o600))
	evidence := `{"schema_version":1,"kind":"merc_level_b_release_root_evidence","status":"PASS","plan_sha256":"plan","candidate_commit":"commit","remote_profile_sha256":"profile","receipts":{},"evidence_chain_sha256":"chain","evidence_chain":{},"secret_values_recorded":false,"raw_secret":"super-secret-value"}`
	must(t, os.WriteFile(releaseEvidencePath(root), []byte(evidence), 0o600))
	must(t, os.MkdirAll(filepath.Join(root, "ops"), 0o700))
	must(t, os.WriteFile(filepath.Join(root, "ops", "go-no-go.json"), []byte(`{"readiness_score":83,"decisions":{"supervised_stripe_test_mode_private_canary":"NO_GO"},"open_p1":[{"id":"P1-STAGING","owner":"operations","blocker":"super-secret-value"}]}`), 0o600))
	request := httptest.NewRequest(http.MethodGet, "/release.json", nil)
	response := httptest.NewRecorder()
	releaseUIHandler(root).ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "super-secret-value") || strings.Contains(response.Body.String(), "raw_secret") {
		t.Fatalf("release UI leaked private data status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache policy=%q", got)
	}
}
