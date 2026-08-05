package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedPassedPromotionGate inserts a passed gate verdict and returns the receipt
// ref ApplyActivationPolicy will accept for an operator promotion of cellID.
func seedPassedPromotionGate(t *testing.T, ctx context.Context, store *Store, cellID string) string {
	t.Helper()
	// Unique digest per call so ON CONFLICT DO NOTHING does not collide across tests.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", cellID, time.Now().UnixNano())))
	digest := hex.EncodeToString(sum[:])
	ref := fmt.Sprintf("%s:%s:%s", promotionGateVersion, cellID, digest)
	evidence := CellPromotionEvidence{
		GateVersion:            promotionGateVersion,
		EvaluatedAt:            time.Now().UTC(),
		Scope:                  CellPromotionScope{CellID: cellID, JobType: "embed", ModelRef: "all-minilm-l6-v2"},
		IncumbentCell:          "candle-metal-minilm-embed",
		Basis:                  promotionBasisCost,
		RequiredMarginFraction: promotionCostMarginFraction,
		RuntimeMatrixSHA256:    generatedRuntimeMatrixSHA256,
		// Empty refusals ⇒ Passed().
	}
	// Stamp the digest that ReceiptRef would compute only if we used Digest();
	// here we write the row directly so the ref is under our control.
	raw, err := json.Marshal(evidence)
	must(t, err)
	_, err = store.pool.Exec(ctx, `
		INSERT INTO runtime_cell_promotion_evaluations
		  (evidence_sha256, promotion_receipt_ref, gate_version, scope_json,
		   incumbent_cell, challenger_cell, passed, policy_revision,
		   runtime_matrix_sha256, evaluated_at, evidence_json)
		VALUES ($1,$2,$3,$4::jsonb,$5,$6,true,1,$7,$8,$9::jsonb)`,
		digest, ref, promotionGateVersion, `{"cell_id":"`+cellID+`"}`,
		evidence.IncumbentCell, cellID, generatedRuntimeMatrixSHA256,
		evidence.EvaluatedAt, string(raw))
	mustf(t, err, "seed promotion gate verdict: %v")
	return ref
}

// withActivationRestored makes the process-wide policy snapshot safe to mutate.
//
// The snapshot is global on purpose — it is what every projection reads — so a
// test that installs one has to put the previous one back or it leaks into every
// test that runs after it in the same binary.
func withActivationRestored(t *testing.T) {
	t.Helper()
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
}

func openActivationStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	withActivationRestored(t)
	return openIsolatedTestStore(t)
}

// The seed must reproduce the document exactly, or the split has changed what the
// product does on day one rather than only how it is governed.
func TestActivationPolicySeedsFromTheDocumentAndProjectsBack(t *testing.T) {
	ctx, store, pool := openActivationStore(t)

	entries, err := store.CurrentActivationPolicy(ctx)
	must(t, err)
	want, err := documentActivationEntries()
	must(t, err)
	if len(entries) != len(want) {
		t.Fatalf("policy holds %d statements, document declares %d", len(entries), len(want))
	}
	byKey := map[string]ActivationPolicyEntry{}
	for _, entry := range entries {
		byKey[activationKey(entry.RuntimeProfileID, entry.CellID)] = entry
	}
	for _, expected := range want {
		key := activationKey(expected.RuntimeProfileID, expected.CellID)
		got, ok := byKey[key]
		if !ok {
			t.Fatalf("%s has no activation policy", key)
		}
		if got.Lifecycle != expected.Lifecycle {
			t.Errorf("%s is %s in policy and %s in the document",
				key, got.Lifecycle, expected.Lifecycle)
		}
		if got.Source != activationSourceDocument {
			t.Errorf("%s was seeded from %q", key, got.Source)
		}
	}

	// And the registry's denormalized columns agree with policy, because the
	// scheduler reads those and a cache that disagrees with its source is worse
	// than no cache.
	assertRegistryMatchesPolicy(t, ctx, pool)
}

// The central claim of section 0: a promotion is a policy write. No document
// edit, no new profile revision, no new capability digest — therefore no agent
// rebuild.
func TestPromotionIsAPolicyWriteAndLeavesCapabilityIdentityUntouched(t *testing.T) {
	ctx, store, pool := openActivationStore(t)

	profile, ok := runtimeProfileByID("llama_cpp_metal")
	if !ok {
		t.Fatal("llama_cpp_metal is not registered")
	}
	before, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleRealRuntimeProven {
		t.Fatalf("the embed cell starts at %s, not REAL_RUNTIME_PROVEN", got)
	}
	if cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("the llama.cpp embed cell is advertised before any promotion")
	}

	// BOTH the profile and the cell. A cell cannot outrank its profile, so a
	// cell-only CANARY statement is floored back to REAL_RUNTIME_PROVEN and
	// changes nothing — which is the rule working, not the promotion failing.
	// Operator promotion requires the cell-promotion-gate verdict, not a path.
	receipt := seedPassedPromotionGate(t, ctx, store, llamaEmbedCell)
	revision, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{
		{
			RuntimeProfileID: "llama_cpp_metal",
			Lifecycle:        runtimeLifecycleCanary,
			// Profile-level entries are not cell promotions; no gate required.
			PromotionReceipt: "profile-canary-note",
		},
		{
			RuntimeProfileID: "llama_cpp_metal",
			CellID:           llamaEmbedCell,
			Lifecycle:        runtimeLifecycleCanary,
			PromotionReceipt: receipt,
			CanaryAllowlist:  []string{"proof-cohort"},
			CanaryTrafficPct: 5,
		},
	}, "bounded canary for the proven embed cell")
	mustf(t, err, "apply promotion: %v")
	if revision < 2 {
		t.Fatalf("promotion took revision %d, want a revision after the document seed", revision)
	}

	// The projection moved.
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleCanary {
		t.Fatalf("after promotion the cell is %s in the registry", got)
	}
	if !cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("a CANARY cell did not reach the advertised projection")
	}
	assertRegistryMatchesPolicy(t, ctx, pool)

	// The identity every agent compares did not.
	after, err := profile.CapabilityDigest(runtimeAuthorityModels)
	must(t, err)
	if after != before {
		t.Fatal("a promotion moved the profile capability digest")
	}
	if generatedRuntimeMatrixSHA256 != pinnedCapabilityMatrixDigest {
		t.Fatal("a promotion moved the capability matrix digest every agent binds")
	}

	// The rejected generation cell is untouched. A promotion addresses one cell.
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", "llama-cpp-metal-llama1-infer"); got != runtimeLifecycleRejectedForContract {
		t.Fatalf("promoting the embed cell moved the generation cell to %s", got)
	}

	// Rollback restores the earlier state by writing forward.
	rolled, err := store.RollbackActivationPolicy(ctx, revision-1, "canary withdrawn")
	mustf(t, err, "rollback: %v")
	if rolled <= revision {
		t.Fatalf("rollback took revision %d, which is not after %d", rolled, revision)
	}
	if got := lifecycleOfCell(t, ctx, pool, "llama_cpp_metal", llamaEmbedCell); got != runtimeLifecycleRealRuntimeProven {
		t.Fatalf("after rollback the cell is %s", got)
	}
	if cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("a rolled-back cell is still advertised")
	}

	// The promotion is still in the history. A rollback that erased it would
	// leave no record that the decision was ever taken.
	var promotions int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM runtime_activation_policies
		 WHERE policy_revision=$1 AND lifecycle='CANARY'`, revision).Scan(&promotions); err != nil {
		t.Fatal(err)
	}
	if promotions != 2 {
		t.Fatalf("the promotion revision holds %d CANARY statements after rollback, want the "+
			"profile and the cell", promotions)
	}
	var target *int64
	if err := pool.QueryRow(ctx, `
		SELECT DISTINCT rollback_target FROM runtime_activation_policies
		 WHERE policy_revision=$1`, rolled).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target == nil || *target != revision-1 {
		t.Fatalf("the rollback revision names target %v, want %d", target, revision-1)
	}
}

// The last compile-time lifecycle coupling: a cell the agent's embedded document
// calls DRAFT.
//
// Enrolment used to validate a worker's declaration against the DIRECTED set, so
// a DRAFT cell could not be declared by any agent — and promoting it out of DRAFT
// by policy therefore still needed the fleet rebuilt and redeployed first. A
// worker now declares CAPABILITY and the control plane authorizes against policy,
// so the promotion is the only thing that has to happen.
func TestPromotingOutOfDraftNeedsNoNewAgentBuild(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	cuda := WorkerCapability{
		HWClass: "nvidia_24gb", Engine: "vllm", MemoryGB: 24,
		SupportedJobs:   []string{"batch_infer"},
		SupportedModels: []string{"llama-3.2-1b-instruct-q4"},
	}
	// Declaring a DRAFT cell is legitimate — the host really can serve it. What
	// it cannot do yet is be authorized for anything.
	if _, err := projectWorkerRuntimeCapabilities(cuda); err == nil {
		t.Fatal("a DRAFT cell was authorized before any policy promoted it")
	} else if !strings.Contains(err.Error(), "is activated") {
		t.Fatalf("the refusal was about the declaration rather than activation: %v", err)
	}

	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{
		{RuntimeProfileID: "vllm_cuda", Lifecycle: runtimeLifecycleValidated},
		{RuntimeProfileID: "vllm_cuda", CellID: "vllm-cuda-llama1-infer",
			Lifecycle: runtimeLifecycleValidated},
	}, "registered for directed proof runs"); err != nil {
		t.Fatalf("promote out of DRAFT: %v", err)
	}

	projected, err := projectWorkerRuntimeCapabilities(cuda)
	mustf(t, err, "after promotion the same worker still cannot enrol: %v")
	if len(projected) != 1 || projected[0].ID != "vllm-cuda-llama1-infer" {
		t.Fatalf("projected %+v", projected)
	}
	// Directed, never advertised. VALIDATED is not routable.
	if cellIsAdvertised("vllm-cuda-llama1-infer") {
		t.Fatal("a VALIDATED cell reached the buyer-visible catalogue")
	}
	if generatedRuntimeMatrixSHA256 != pinnedCapabilityMatrixDigest {
		t.Fatal("promoting out of DRAFT moved the capability matrix digest")
	}
}

// Policy is append-only in the database, not merely by convention in Go.
func TestActivationPolicyRowsCannotBeRewrittenOrDeleted(t *testing.T) {
	ctx, _, pool := openActivationStore(t)

	for name, statement := range map[string]string{
		"update": `UPDATE runtime_activation_policies SET lifecycle='ACTIVE'
		            WHERE runtime_profile_id='llama_cpp_metal'`,
		"delete": `DELETE FROM runtime_activation_policies
		            WHERE runtime_profile_id='llama_cpp_metal'`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Errorf("a policy %s was accepted", name)
		}
	}
}

// A capability change must not inherit the activation decisions taken about the
// runtime it replaced.
func TestPolicyWrittenAgainstAnotherCapabilityIsRefusedAtWriteAndAtRead(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	// At write: the operator is still there to be told.
	_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		CapabilityDigest: "00000000000000000000000000000000000000000000000000000000000000ff",
		Lifecycle:        runtimeLifecycleCanary,
		PromotionReceipt: "evidence/chain/two-agent-product-chain.json",
	}}, "promotion against a capability that is not current")
	if err == nil {
		t.Fatal("a promotion naming a stale capability digest was accepted")
	}

	// At read: a row that was valid when written and is not any more falls back
	// to the document rather than continuing to govern.
	stale, err := activationSnapshotFrom([]ActivationPolicyEntry{{
		PolicyRevision:   7,
		RuntimeProfileID: "llama_cpp_metal",
		ProfileRevision:  "r8",
		CellID:           llamaEmbedCell,
		CapabilityDigest: "00000000000000000000000000000000000000000000000000000000000000ff",
		Lifecycle:        runtimeLifecycleActive,
	}})
	must(t, err)
	if len(stale.Stale) != 1 {
		t.Fatalf("a stale statement produced %d warnings: %v", len(stale.Stale), stale.Stale)
	}
	for _, cap := range stale.advertised {
		if cap.ID == llamaEmbedCell {
			t.Fatal("a policy written against a different capability made a cell routable")
		}
	}

	// And a statement for a profile revision the document has moved past.
	superseded, err := activationSnapshotFrom([]ActivationPolicyEntry{{
		PolicyRevision:   7,
		RuntimeProfileID: "llama_cpp_metal",
		ProfileRevision:  "r1",
		CellID:           llamaEmbedCell,
		CapabilityDigest: "00000000000000000000000000000000000000000000000000000000000000ff",
		Lifecycle:        runtimeLifecycleActive,
	}})
	must(t, err)
	if len(superseded.Stale) != 1 {
		t.Fatalf("a superseded-revision statement produced %v", superseded.Stale)
	}
}

// Quarantining a cell must remove it from every route, not only from the
// advertised catalogue: directed routing sends real buyer work.
func TestQuarantinePolicyRemovesACellFromEveryRoute(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	if !cellIsDirected(llamaEmbedCell) {
		t.Fatal("the proven embed cell is not directed-reachable to begin with")
	}
	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleQuarantined,
	}}, "quarantine"); err != nil {
		t.Fatal(err)
	}
	if cellIsDirected(llamaEmbedCell) {
		t.Fatal("a QUARANTINED cell is still reachable by directed routing")
	}
	if cellIsAdvertised(llamaEmbedCell) {
		t.Fatal("a QUARANTINED cell is still advertised")
	}
	// And a worker of that engine can no longer bind the profile at all, because
	// nothing on it is reachable.
	_, err := ResolveWorkerRuntimeProfile(WorkerCapability{
		HWClass: "apple_silicon_max", Engine: "llama_cpp", MemoryGB: 64,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"},
	})
	if err == nil {
		t.Fatal("a worker enrolled against a profile with every cell quarantined")
	}
}

// Nothing becomes routable without naming the receipt that authorised it. The
// rule is in the database, so a caller that forgets cannot make it optional.
// Operator promotions further require the gate's verdict, not a free string.
func TestRoutablePolicyRequiresAPromotionReceipt(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleCanary,
	}}, "promotion with no evidence")
	if err == nil {
		t.Fatal("a cell was promoted to CANARY with no promotion receipt")
	}

	// A non-empty path that is not a gate verdict is also refused.
	_, err = store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           llamaEmbedCell,
		Lifecycle:        runtimeLifecycleCanary,
		PromotionReceipt: "evidence/chain/two-agent-product-chain.json",
	}}, "promotion with a loose path")
	if err == nil {
		t.Fatal("a cell was promoted with a loose receipt path, not a gate verdict")
	}
	if !strings.Contains(err.Error(), promotionGateVersion) {
		t.Fatalf("refusal did not name the gate: %v", err)
	}
}

// The digest changed DEFINITION, so anything that recorded the old value must
// still be able to resolve the revision it named.
func TestPriorCapabilityDigestsAreRetained(t *testing.T) {
	ctx, _, pool := openActivationStore(t)

	// Simulate a database written under manifest v1: an older digest under the
	// same revision, at the older version.
	const legacy = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_profiles SET profile_digest=$1, capability_manifest_version=1
		 WHERE runtime_profile_id='llama_cpp_metal' AND is_current`, legacy); err != nil {
		t.Fatal(err)
	}
	mustf(t, NewStore(pool).Migrate(ctx), "re-migration over a v1 registry: %v")

	var current string
	var version int
	if err := pool.QueryRow(ctx, `
		SELECT profile_digest, capability_manifest_version FROM runtime_profiles
		 WHERE runtime_profile_id='llama_cpp_metal' AND is_current`).
		Scan(&current, &version); err != nil {
		t.Fatal(err)
	}
	if version != capabilityManifestVersion {
		t.Fatalf("the row is still at manifest version %d", version)
	}
	if current == legacy {
		t.Fatal("the v1 digest was not upgraded")
	}
	var retained bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM runtime_profile_digest_history
		                WHERE runtime_profile_id='llama_cpp_metal' AND profile_digest=$1)`,
		legacy).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("the prior digest was overwritten without being retained; " +
			"anything that recorded it can no longer resolve the revision it named")
	}
}

func assertRegistryMatchesPolicy(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT m.runtime_profile_id, m.cell_id, m.lifecycle, m.routable
		  FROM runtime_profile_models m
		  JOIN runtime_profiles p
		    ON p.runtime_profile_id = m.runtime_profile_id AND p.revision = m.revision
		 WHERE p.is_current`)
	must(t, err)
	defer rows.Close()
	snapshot := currentActivation()
	seen := 0
	for rows.Next() {
		var runtimeID, cellID, lifecycle string
		var routable bool
		must(t, rows.Scan(&runtimeID, &cellID, &lifecycle, &routable))
		seen++
		profile, ok := runtimeProfileByID(runtimeID)
		if !ok {
			t.Fatalf("registry holds unregistered profile %q", runtimeID)
		}
		for _, cell := range profile.Cells {
			if cell.ID != cellID {
				continue
			}
			if want := snapshot.cellLifecycle(profile, cell); want != lifecycle {
				t.Errorf("%s/%s: registry says %s, policy says %s",
					runtimeID, cellID, lifecycle, want)
			}
			// Registry routable tracks lifecycle AND bindable authority.
			wantRoutable := snapshot.cellRoutable(profile, cell)
			if routable != wantRoutable {
				t.Errorf("%s/%s: routable=%v, want %v at lifecycle %s",
					runtimeID, cellID, routable, wantRoutable, lifecycle)
			}
		}
	}
	must(t, rows.Err())
	if seen == 0 {
		t.Fatal("the registry holds no current cells")
	}
}

func lifecycleOfCell(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runtimeID, cellID string) string {
	t.Helper()
	var lifecycle string
	if err := pool.QueryRow(ctx, `
		SELECT m.lifecycle FROM runtime_profile_models m
		  JOIN runtime_profiles p
		    ON p.runtime_profile_id = m.runtime_profile_id AND p.revision = m.revision
		 WHERE p.is_current AND m.runtime_profile_id=$1 AND m.cell_id=$2`,
		runtimeID, cellID).Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle of %s/%s: %v", runtimeID, cellID, err)
	}
	return lifecycle
}

func cellIsAdvertised(cellID string) bool {
	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.ID == cellID {
			return true
		}
	}
	return false
}

func cellIsDirected(cellID string) bool {
	for _, cap := range directedRuntimeCapabilities() {
		if cap.ID == cellID {
			return true
		}
	}
	return false
}

// A rejection is a measurement, and policy does not reverse measurements.
//
// This is the hole the capability/activation split opens if nothing closes it:
// the determinism rule that refuses a byte_exact cell on a non-deterministic
// engine runs when the DOCUMENT loads, and activation policy is a second door
// into the same decision. Without this guard an operator could promote
// llama.cpp's byte_exact generation cell straight to CANARY by writing a row —
// the cell whose sweep found divergence from its own serial output in every
// batched configuration tested.
func TestPolicyCannotReverseAMeasuredRejection(t *testing.T) {
	ctx, store, _ := openActivationStore(t)

	for _, lifecycle := range []string{
		runtimeLifecycleValidated,
		runtimeLifecycleRealRuntimeProven,
		runtimeLifecycleCanary,
		runtimeLifecycleActive,
	} {
		_, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
			RuntimeProfileID: "llama_cpp_metal",
			CellID:           "llama-cpp-metal-llama1-infer",
			Lifecycle:        lifecycle,
			PromotionReceipt: "evidence/perf/runtime-benchmarks/llama-cpp-metal-llama1-q4-r3.json",
		}}, "reversing a measured rejection")
		if err == nil {
			t.Fatalf("policy promoted a REJECTED_FOR_CONTRACT cell to %s", lifecycle)
		}
		if !strings.Contains(err.Error(), "REJECTED_FOR_CONTRACT") {
			t.Errorf("refused for the wrong reason at %s: %v", lifecycle, err)
		}
	}

	// Restating the rejection is fine — that is not a reversal.
	if _, err := store.ApplyActivationPolicy(ctx, []ActivationPolicyEntry{{
		RuntimeProfileID: "llama_cpp_metal",
		CellID:           "llama-cpp-metal-llama1-infer",
		Lifecycle:        runtimeLifecycleRejectedForContract,
	}}, "restating the rejection"); err != nil {
		t.Fatalf("policy could not restate an existing rejection: %v", err)
	}
}
