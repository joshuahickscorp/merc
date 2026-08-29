package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAdminDirectedJobReachesBuildWorkloadDecisionDirected(t *testing.T) {
	graph := buildCallGraph(t)
	path := graph.reaches("Server.handleAdminDirectedJob", map[string]bool{
		"buildWorkloadDecisionDirected": true,
	})
	if path == nil {
		t.Fatal("handleAdminDirectedJob does not reach buildWorkloadDecisionDirected; " +
			"directed routing is still unwired from production")
	}
	t.Logf("production path: %s", strings.Join(path, " -> "))
}

func TestAdminDirectedJobFreezesTheNamedCell(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	// Unit-level freeze check: the same server-side field the admin route sets.
	// Full createJob needs prepaid/storage; the freeze is the mechanism that was
	// unwired, and buildWorkloadDecisionForSubmit is what createJob calls.
	sub := embedSubmit()
	sub.directedCellID = llamaEmbedCell
	decision, err := buildWorkloadDecisionForSubmit(sub, strings.Repeat("a", 64))
	mustf(t, err, "directed submit decision: %v")
	if decision.DirectedCellID != llamaEmbedCell {
		t.Fatalf("DirectedCellID=%q, want %q", decision.DirectedCellID, llamaEmbedCell)
	}
	if len(decision.RuntimeCandidates) != 1 ||
		decision.RuntimeCandidates[0].CellID != llamaEmbedCell {
		t.Fatalf("froze %+v, want cell %s alone", decision.RuntimeCandidates, llamaEmbedCell)
	}
	// Ordinary buyer submit still does not name a cell.
	ordinary, err := buildWorkloadDecisionForSubmit(embedSubmit(), strings.Repeat("b", 64))
	must(t, err)
	if ordinary.DirectedCellID != "" {
		t.Fatalf("ordinary submit recorded directed cell %q", ordinary.DirectedCellID)
	}
}

func TestAdminDirectedJobHandlerRefusesMissingCellAndBuyer(t *testing.T) {
	server := &Server{store: &Store{}}
	for _, raw := range []string{
		`{"buyer_id":"` + uuid.NewString() + `","idempotency_key":"directed-1","job":{"job_type":{"type":"embed"},"model":{"ref":"all-minilm-l6-v2"}}}`,
		`{"directed_cell_id":"llama-cpp-metal-minilm-embed","idempotency_key":"directed-1","job":{"job_type":{"type":"embed"},"model":{"ref":"all-minilm-l6-v2"}}}`,
		`{"buyer_id":"not-a-uuid","directed_cell_id":"llama-cpp-metal-minilm-embed","idempotency_key":"directed-1","job":{}}`,
		`{"buyer_id":"` + uuid.NewString() + `","directed_cell_id":"llama-cpp-metal-minilm-embed","job":{}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/admin/runtime/jobs/directed", bytes.NewBufferString(raw))
		rec := httptest.NewRecorder()
		server.handleAdminDirectedJob(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("raw=%s status=%d body=%s, want 400", raw, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminSelectorActivationReachesApplyActivationPolicy(t *testing.T) {
	graph := buildCallGraph(t)
	path := graph.reaches("Server.handleAdminSelectorActivation", map[string]bool{
		"Store.ApplyActivationPolicy": true,
	})
	if path == nil {
		t.Fatal("handleAdminSelectorActivation does not reach ApplyActivationPolicy")
	}
	t.Logf("production path: %s", strings.Join(path, " -> "))
}

func TestAdminSelectorActivationRefusesEmptyEntries(t *testing.T) {
	server := &Server{store: &Store{}}
	body, _ := json.Marshal(selectorActivationRequest{Note: "try empty"})
	req := httptest.NewRequest(http.MethodPost, "/admin/runtime/selector/activation", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleAdminSelectorActivation(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 for empty entries", rec.Code, rec.Body.String())
	}
}

func TestAdminSelectorActivationAppliesPolicy(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	// A non-routable lifecycle write needs no promotion receipt. Restating
	// REAL_RUNTIME_PROVEN for the llama embed cell exercises the production
	// apply path and appends a revision without promoting anything to routable.
	body, err := json.Marshal(selectorActivationRequest{
		Note: "operator activation apply path smoke",
		Entries: []ActivationPolicyEntry{{
			RuntimeProfileID: "llama_cpp_metal",
			CellID:           llamaEmbedCell,
			Lifecycle:        runtimeLifecycleRealRuntimeProven,
		}},
	})
	must(t, err)
	var before int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(policy_revision),0) FROM runtime_activation_policies`).
		Scan(&before); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/runtime/selector/activation", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	(&Server{store: store}).handleAdminSelectorActivation(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("activation status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ActivationApplied bool  `json:"activation_applied"`
		PolicyRevision    int64 `json:"policy_revision"`
	}
	must(t, json.Unmarshal(rec.Body.Bytes(), &response))
	if !response.ActivationApplied || response.PolicyRevision <= before {
		t.Fatalf("activation response lost forward authority: %+v (before=%d)", response, before)
	}
	// Still not routable — this lane wires apply, it does not promote.
	if cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("activation apply promoted the challenger into the advertised set; " +
			"this lane must not make any cell routable")
	}
}
