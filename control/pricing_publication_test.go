package main

import (
	"strings"
	"testing"
)

const supersededLlamaInferReceipt = "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r4.json"

func documentEntriesByKey(t *testing.T) []ActivationPolicyEntry {
	t.Helper()
	entries, err := documentActivationEntries()
	mustf(t, err, "document activation entries: %v")
	return entries
}

func advertisedIDs(snapshot *runtimeActivation) []string {
	ids := make([]string, 0, len(snapshot.advertised))
	for _, cap := range snapshot.advertised {
		ids = append(ids, cap.ID)
	}
	return ids
}

func TestDocumentSeedWithSupersededReceiptFallsBackToDocument(t *testing.T) {
	withActivationRestored(t)
	entries := documentEntriesByKey(t)
	found := false
	for i, entry := range entries {
		if entry.CellID != "candle-metal-llama1-infer" {
			continue
		}
		if entry.PromotionReceipt == supersededLlamaInferReceipt {
			t.Fatal("document already cites the superseded r4 receipt; cannot prove reseal fallback")
		}
		entries[i].PromotionReceipt = supersededLlamaInferReceipt
		entries[i].Routable = false
		found = true
	}
	if !found {
		t.Fatal("document has no candle-metal-llama1-infer statement")
	}

	snapshot, err := activationSnapshotFrom(entries)
	mustf(t, err, "snapshot drifted document seed: %v")
	if !strings.Contains(strings.Join(snapshot.Stale, "\n"), "falling back to document") {
		t.Fatalf("stale=%v, want document-seed fallback", snapshot.Stale)
	}
	ids := advertisedIDs(snapshot)
	if len(ids) != 1 || ids[0] != "candle-metal-llama1-infer" {
		t.Fatalf("advertised=%v, want exactly [candle-metal-llama1-infer] after r4→r6 reseal fallback", ids)
	}
}

func TestOperatorRoutableMismatchStillQuarantines(t *testing.T) {
	withActivationRestored(t)
	entries := documentEntriesByKey(t)
	found := false
	for i, entry := range entries {
		if entry.CellID != "candle-metal-llama1-infer" {
			continue
		}
		entries[i].Source = activationSourceOperator
		entries[i].PromotionReceipt = supersededLlamaInferReceipt
		found = true
	}
	if !found {
		t.Fatal("document has no candle-metal-llama1-infer statement")
	}

	snapshot, err := activationSnapshotFrom(entries)
	mustf(t, err, "snapshot operator mismatch: %v")
	if advertisedRuntimeCellFrom(snapshot, "candle-metal-llama1-infer") {
		t.Fatal("operator row with a superseded receipt reached the advertised set")
	}
	joined := strings.Join(snapshot.Stale, "\n")
	if !strings.Contains(joined, "QUARANTINED") || strings.Contains(joined, "falling back to document") {
		t.Fatalf("stale=%v, want operator quarantine and no document fallback", snapshot.Stale)
	}
}

func advertisedRuntimeCellFrom(snapshot *runtimeActivation, cellID string) bool {
	for _, cap := range snapshot.advertised {
		if cap.ID == cellID {
			return true
		}
	}
	return false
}

func TestPublicBoardSurvivesDocumentSeedReseal(t *testing.T) {
	ctx, server, store, pool, _ := currentPublicPricingFixture(t)
	docs, err := documentActivationEntries()
	mustf(t, err, "document entries under publication fixture: %v")
	var drifted ActivationPolicyEntry
	for _, entry := range docs {
		if entry.CellID == "candle-metal-llama1-infer" {
			drifted = entry
			break
		}
	}
	if drifted.CellID == "" {
		t.Fatal("publication fixture has no llama infer document statement")
	}
	drifted.PromotionReceipt = supersededLlamaInferReceipt
	drifted.Routable = false
	tx, err := pool.Begin(ctx)
	mustf(t, err, "begin drifted-seed tx: %v")
	_, err = insertActivationPolicy(ctx, tx, []ActivationPolicyEntry{drifted},
		activationSourceDocument, nil, "test: simulate r4 seed after r6 reseal")
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("write drifted document seed: %v", err)
	}
	mustf(t, tx.Commit(ctx), "commit drifted document seed: %v")

	if _, err := store.activationForNewAdmission(ctx); err != nil {
		t.Fatalf("refresh admission snapshot after reseal: %v", err)
	}
	board := callPublicPricingHandler(t, server, "/pricing/board.json")
	if board.Code != 200 {
		t.Fatalf("public board after document-seed reseal status=%d body=%s",
			board.Code, board.Body.String())
	}
}

func TestDocumentActivationSeedDrifted(t *testing.T) {
	want := ActivationPolicyEntry{
		Source: activationSourceDocument, ProfileRevision: "r9",
		CapabilityDigest: "aaa", Lifecycle: runtimeLifecycleActive,
		PromotionReceipt: "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r6.json",
	}
	stored := want
	if documentActivationSeedDrifted(stored, want) {
		t.Fatal("identical document seed reported as drifted")
	}
	stored.PromotionReceipt = supersededLlamaInferReceipt
	if !documentActivationSeedDrifted(stored, want) {
		t.Fatal("r4 vs r6 document seed was not drifted")
	}
	stored.Source = activationSourceOperator
	if documentActivationSeedDrifted(stored, want) {
		t.Fatal("operator row must not be treated as a drifted document seed")
	}
}
