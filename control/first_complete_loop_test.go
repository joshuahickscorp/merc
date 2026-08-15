package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFirstCompleteLoopThroughThePublicAPI lives in
// first_complete_loop_integration_test.go (//go:build integration).
// `make test` excludes it; `make test-integration` and `make ci` run it.

func firstCompleteLoopReceiptPath() string {
	// A repeatable test must not rewrite a tracked historical receipt with fresh
	// random buyer, supplier, and job IDs. An operator may choose an explicit
	// diagnostic path, but writeFirstLoopReceipt refuses evidence/ while this loop
	// uses synthetic performance authority. Ordinary CI writes beside its other
	// ignored artifacts so a passing test leaves its source tree unchanged.
	path := strings.TrimSpace(os.Getenv("MERC_FIRST_COMPLETE_LOOP_RECEIPT_PATH"))
	if path == "" {
		path = filepath.Join("..", ".artifacts", "canary", "first-complete-loop.json")
	}
	return path
}

type firstLoopReceipt struct {
	JobID               string  `json:"job_id"`
	Email               string  `json:"buyer_email"`
	Currency            string  `json:"currency"`
	CeilingUSD          float64 `json:"accepted_ceiling_usd"`
	EstimateUSD         float64 `json:"quoted_estimate_usd"`
	ChargedUSD          float64 `json:"buyer_charged_usd"`
	BuyerMicros         int64   `json:"buyer_debit_micros"`
	SupplierMicros      int64   `json:"supplier_credit_micros"`
	PlatformMicros      int64   `json:"merc_contribution_micros"`
	SupplierID          string  `json:"paid_supplier_id"`
	RuntimeCell         string  `json:"runtime_cell_id"`
	RuntimeID           string  `json:"runtime_id"`
	Engine              string  `json:"engine"`
	HWClass             string  `json:"hw_class"`
	ExecutionMode       string  `json:"execution_mode"`
	ExecutionModeReason string  `json:"execution_mode_reason"`
	SelectionBasis      string  `json:"selection_basis"`
	RoutedCell          string  `json:"routed_cell_id"`
	VerificationOutcome string  `json:"verification_outcome"`
}

func writeFirstLoopReceipt(t *testing.T, loop firstLoopReceipt) {
	t.Helper()
	path := firstCompleteLoopReceiptPath()
	// A mechanics run backed by synthetic performance authority must never write
	// into evidence/, even when an operator supplied the old controlled-run path.
	// Refuse before creating a directory or writing any bytes.
	if strings.Contains(filepath.ToSlash(path), "/evidence/") || strings.HasPrefix(filepath.ToSlash(path), "evidence/") {
		t.Fatalf("TEST_ONLY combined-token mechanics receipt may not be written under evidence/: %s", path)
	}
	dir := filepath.Dir(path)
	mustf(t, os.MkdirAll(dir, 0o755), "create receipt directory: %v")
	payload := map[string]any{
		"schema_version":        1,
		"kind":                  "first_complete_loop",
		"harness":               "control/first_complete_loop_test.go",
		"runtime_matrix_sha256": generatedRuntimeMatrixSHA256,
		"loop":                  loop,
		"candidate_bound":       false,
		"authority_class":       "TEST_ONLY",
		"production_admission":  false,
		"limitations": []string{
			"Admission uses an in-memory TEST_ONLY combined-token benchmark authority; " +
				"this receipt cannot establish a sellable production lane.",
			"Runs against the working tree, not an immutable image at an exact " +
				"commit, so it cannot mint CANARY_PROVEN however complete the loop is.",
			"One buyer, one supplier, one job, one hardware class. Not a fleet claim.",
			"The buyer is funded by the sandbox credit grant, so the Stripe rails are " +
				"not exercised: no payment intent, no capture, no payout.",
		},
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	mustf(t, err, "render receipt: %v")
	mustf(t, os.WriteFile(path, append(body, '\n'), 0o644), "write receipt: %v")
	t.Logf("first-complete-loop receipt written to %s", path)
}

func TestFirstCompleteLoopReceiptPathDefaultsOutsideTrackedEvidence(t *testing.T) {
	t.Setenv("MERC_FIRST_COMPLETE_LOOP_RECEIPT_PATH", "")
	if got, want := firstCompleteLoopReceiptPath(), filepath.Join("..", ".artifacts", "canary", "first-complete-loop.json"); got != want {
		t.Fatalf("default first-loop receipt path = %q, want %q", got, want)
	}
	t.Setenv("MERC_FIRST_COMPLETE_LOOP_RECEIPT_PATH", "evidence/canary/operator-run.json")
	if got, want := firstCompleteLoopReceiptPath(), "evidence/canary/operator-run.json"; got != want {
		t.Fatalf("explicit first-loop receipt path = %q, want %q", got, want)
	}
}

type apiResponse struct {
	status int
	body   string
	json   map[string]any
}

func postJSON(t *testing.T, url, bearer string, payload any) apiResponse {
	t.Helper()
	return postJSONWithHeaders(t, url, bearer, nil, payload)
}

func postJSONWithHeaders(
	t *testing.T, url, bearer string, headers map[string]string, payload any,
) apiResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	must(t, err)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	mustf(t, err, "POST %s: %v", url)
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := apiResponse{status: resp.StatusCode, body: buf.String()}
	_ = json.Unmarshal(buf.Bytes(), &out.json)
	return out
}

func getJSON(t *testing.T, url, bearer string) apiResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := apiResponse{status: resp.StatusCode, body: buf.String()}
	_ = json.Unmarshal(buf.Bytes(), &out.json)
	return out
}

func deleteJSON(t *testing.T, url, bearer string) apiResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := apiResponse{status: resp.StatusCode, body: buf.String()}
	_ = json.Unmarshal(buf.Bytes(), &out.json)
	return out
}
