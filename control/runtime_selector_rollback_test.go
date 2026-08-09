package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminSelectorRollbackReachesAppendOnlyPolicy(t *testing.T) {
	ctx, store, pool := openActivationStore(t)
	// Use a non-routable containment write as the state to undo. Operator-global
	// promotion is intentionally unavailable until global coverage and durable
	// matched-pair evidence exist; rollback must remain functional independently
	// of that fail-closed promotion boundary.
	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
		Lifecycle: runtimeLifecycleQuarantined,
	}}, "selector rollback endpoint fixture containment"); err != nil {
		t.Fatalf("apply containment fixture: %v", err)
	}
	var containedRevision int64
	must(t, pool.QueryRow(ctx, `SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&containedRevision))
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleQuarantined {
		t.Fatalf("fixture cell lifecycle=%s, want QUARANTINED", got)
	}

	body, err := json.Marshal(selectorRollbackRequest{
		TargetPolicyRevision: containedRevision - 1,
		Note:                 "selector challenger failed the bounded canary; restore incumbent",
	})
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/admin/runtime/selector/rollback", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	(&Server{store: store}).handleAdminSelectorRollback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ActivationApplied      bool  `json:"activation_applied"`
		PolicyRevision         int64 `json:"policy_revision"`
		RollbackTargetRevision int64 `json:"rollback_target_revision"`
	}
	must(t, json.Unmarshal(rec.Body.Bytes(), &response))
	if !response.ActivationApplied || response.PolicyRevision <= containedRevision ||
		response.RollbackTargetRevision != containedRevision-1 {
		t.Fatalf("rollback response lost forward policy authority: %+v", response)
	}
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleRealRuntimeProven {
		t.Fatalf("rollback left challenger lifecycle=%s, want REAL_RUNTIME_PROVEN", got)
	}
	var source string
	var target *int64
	if err := pool.QueryRow(ctx, `SELECT source,rollback_target FROM runtime_activation_policies
		WHERE policy_revision=$1 AND runtime_profile_id='llama_cpp_metal' AND cell_id=''`, response.PolicyRevision).
		Scan(&source, &target); err != nil {
		t.Fatal(err)
	}
	if source != activationSourceRollback || target == nil || *target != containedRevision-1 {
		t.Fatalf("rollback revision lost append-only provenance: source=%q target=%v", source, target)
	}
}

func TestAdminSelectorRollbackRefusesMalformedRequest(t *testing.T) {
	server := &Server{store: &Store{}}
	for _, raw := range []string{
		`{"target_policy_revision":1}`,
		`{"target_policy_revision":0,"note":"x"}`,
		`{"target_policy_revision":1,"note":"x","extra":true}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/admin/runtime/selector/rollback", bytes.NewBufferString(raw))
		rec := httptest.NewRecorder()
		server.handleAdminSelectorRollback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("raw=%s status=%d body=%s", raw, rec.Code, rec.Body.String())
		}
	}
}
