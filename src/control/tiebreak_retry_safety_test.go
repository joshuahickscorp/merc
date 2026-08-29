package main

import (
	"strings"
	"testing"
)

func TestClaimTaskSQLFencesTiebreaksInPinnedAndGeneralBranches(t *testing.T) {
	for _, predicate := range []string{
		"t.claimed_by = $1 AND t.started_at IS NULL",
		"t.claimed_by IS NULL",
	} {
		query := ClaimTaskSQL(predicate)
		for _, required := range []string{
			predicate,
			"t.verification_hw_class",
			"ej.claim_engine=t.verification_engine",
			"ej.claim_build_hash=t.verification_build_hash",
			"FROM task_execution_history history",
			"executed.worker_id=ej.claim_worker_id",
			"executed.supplier_id=ej.claim_supplier_id",
		} {
			if !strings.Contains(query, required) {
				t.Fatalf("claim branch %q is missing tiebreak safety predicate %q", predicate, required)
			}
		}
	}
}
