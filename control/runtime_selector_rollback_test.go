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
	receipt := seedPassedPromotionGate(t, ctx, store, llamaEmbedCell)
	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{
		{RuntimeProfileID: "llama_cpp_metal", Lifecycle: runtimeLifecycleCanary, PromotionReceipt: "profile-canary"},
		{RuntimeProfileID: "llama_cpp_metal", CellID: llamaEmbedCell,
			Lifecycle: runtimeLifecycleCanary, PromotionReceipt: receipt,
			CanaryAllowlist: []string{"selector-rollback-test"}, CanaryTrafficPct: 5},
	}, "selector rollback endpoint fixture promotion"); err != nil {
		t.Fatalf("apply promotion fixture: %v", err)
	}
	var promotedRevision int64
	if err := pool.QueryRow(ctx, `SELECT MAX(policy_revision) FROM runtime_activation_policies`).Scan(&promotedRevision); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleCanary {
		t.Fatalf("fixture cell lifecycle=%s, want CANARY", got)
	}

	body, err := json.Marshal(selectorRollbackRequest{
		TargetPolicyRevision: promotedRevision - 1,
		Note:                 "selector challenger failed the bounded canary; restore incumbent",
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ActivationApplied || response.PolicyRevision <= promotedRevision ||
		response.RollbackTargetRevision != promotedRevision-1 {
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
	if source != activationSourceRollback || target == nil || *target != promotedRevision-1 {
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
