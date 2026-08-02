package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func materializationFixture(t *testing.T, receiptPricing string, resultsURL bool) (ProjectWorkloadIR, ProjectSubmission, *client, []byte) {
	t.Helper()
	jobID := uuid.New()
	payload := []byte("{\"scene\":\"receipt-bound artifact\"}\n")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/jobs/" + jobID.String() + "/receipt":
			writeJSON(w, http.StatusOK, ClearingReceipt{JobID: jobID, Status: "complete", AuthorityStatus: "verified",
				Authority: ReceiptAuthority{PricingDecisionSHA256: receiptPricing}})
		case "/v1/jobs/" + jobID.String() + "/results":
			result := JobResults{JobID: jobID, Status: "complete"}
			if resultsURL {
				result.ResultsURL = server.URL + "/artifact"
			}
			writeJSON(w, http.StatusOK, result)
		case "/artifact":
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	pricing := strings.Repeat("a", 64)
	ir := ProjectWorkloadIR{IRSHA256: strings.Repeat("b", 64), Steps: []ProjectIRStep{{
		ID: "extract", Outputs: []string{"project://generated/scene.json"},
	}}}
	submission := ProjectSubmission{IRSHA256: ir.IRSHA256, Status: "ACCEPTED", Steps: []ProjectStepSubmission{{
		StepID: "extract", JobID: jobID.String(), PricingDecisionSHA256: pricing, AuthorityQuoteSHA256: strings.Repeat("c", 64),
	}}}
	return ir, submission, &client{base: server.URL, hc: server.Client()}, payload
}

func TestMaterializeProjectStepBindsReceiptAndWritesDeclaredOutput(t *testing.T) {
	pricing := strings.Repeat("a", 64)
	ir, submission, c, payload := materializationFixture(t, pricing, true)
	root := t.TempDir()
	now := time.Date(2026, 8, 1, 22, 0, 0, 0, time.UTC)
	result, err := materializeProjectStep(c, root, ir, submission, "extract", now)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(root, "generated", "scene.json"))
	if err != nil || string(stored) != string(payload) {
		t.Fatalf("materialized output = %q err=%v", stored, err)
	}
	sum := sha256.Sum256(payload)
	if result.Version != 1 || result.Output != "project://generated/scene.json" || result.Bytes != int64(len(payload)) ||
		result.SHA256 != hex.EncodeToString(sum[:]) || result.PricingDecisionSHA256 != pricing ||
		result.MaterializedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("materialization receipt lost authority: %+v", result)
	}
	if _, err := materializeProjectStep(c, root, ir, submission, "extract", now); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing output was overwritten: %v", err)
	}
}

func TestMaterializeProjectStepFailsClosedBeforeWritingOutput(t *testing.T) {
	root := t.TempDir()
	pricing := strings.Repeat("a", 64)
	tests := []struct {
		name       string
		receiptSHA string
		resultsURL bool
		want       string
	}{
		{name: "receipt authority mismatch", receiptSHA: strings.Repeat("d", 64), resultsURL: true, want: "pricing authority"},
		{name: "missing merged result", receiptSHA: pricing, resultsURL: false, want: "no retained merged result"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ir, submission, c, _ := materializationFixture(t, tc.receiptSHA, tc.resultsURL)
			stepRoot := filepath.Join(root, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.Mkdir(stepRoot, 0700); err != nil {
				t.Fatal(err)
			}
			_, err := materializeProjectStep(c, stepRoot, ir, submission, "extract", time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(stepRoot, "generated", "scene.json")); !os.IsNotExist(statErr) {
				t.Fatalf("failed materialization wrote output: %v", statErr)
			}
		})
	}
	t.Run("symlinked output parent", func(t *testing.T) {
		pricing := strings.Repeat("a", 64)
		ir, submission, c, _ := materializationFixture(t, pricing, true)
		stepRoot := filepath.Join(root, "symlink-parent")
		outside := t.TempDir()
		if err := os.Mkdir(stepRoot, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(stepRoot, "generated")); err != nil {
			t.Skipf("cannot create symlink fixture: %v", err)
		}
		_, err := materializeProjectStep(c, stepRoot, ir, submission, "extract", time.Now())
		if err == nil || !strings.Contains(err.Error(), "parent is not a real directory") {
			t.Fatalf("symlinked parent error = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(outside, "scene.json")); !os.IsNotExist(statErr) {
			t.Fatalf("symlinked parent wrote outside project root: %v", statErr)
		}
	})
}
