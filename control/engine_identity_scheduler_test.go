package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDynamicPeerMatchRequiresExactPolicyDeviceAndRuntimeCell(t *testing.T) {
	now := time.Now()
	key := runtimeCapabilityKey("cell-a", "runtime-a", "gguf")
	task := MatchTask{
		JobType: "batch_infer", MinMemoryGB: 64, HWClasses: []string{"apple_silicon_ultra"},
		PinEngine: "candle", PinBuildHash: "deadbeefdeadbeef",
		PinBuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		PinHardwareIdentity:    testOnlyHardwareIdentity,
		RequiredRuntimeKeys:    []string{key}, Tier: "batch",
	}
	worker := MatchWorker{
		ID: uuid.New(), HWClass: "apple_silicon_ultra", Engine: "candle",
		BuildHash: "deadbeefdeadbeef", BuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		HardwareIdentity: testOnlyHardwareIdentity, AuthorizedRuntimeKeys: []string{key},
		MemoryGB: 128, Reputation: 1, TPS: map[string]float32{"batch_infer": 100},
		LastSeen: now,
	}
	wrongPolicy := worker
	wrongPolicy.ID = uuid.New()
	wrongPolicy.BuildIdentityPolicy = externalRunnerBuildIdentityPolicy
	wrongDevice := worker
	wrongDevice.ID = uuid.New()
	wrongDevice.HardwareIdentity = "apple_silicon_v1|brand=Apple M1 Ultra|model=Mac13,2|memory_bytes=137438953472|cpu_cores=20|gpu_cores=64"
	wrongCell := worker
	wrongCell.ID = uuid.New()
	wrongCell.AuthorizedRuntimeKeys = []string{runtimeCapabilityKey("cell-b", "runtime-b", "gguf")}

	got, err := Match(task, []MatchWorker{wrongPolicy, wrongDevice, wrongCell, worker})
	mustf(t, err, "match exact dynamic peer: %v")
	if len(got) != 1 || got[0].ID != worker.ID {
		t.Fatalf("dynamic peer match=%+v, want only exact worker %s", got, worker.ID)
	}
}

func TestEveryCurrentSchedulerBranchBindsExactBuildAndDevice(t *testing.T) {
	raw, err := os.ReadFile("scheduler.go")
	must(t, err)
	sql := string(raw)
	for _, predicate := range []string{
		"j.placement_requirement->>'engine_build_hash' = me.build_hash",
		"j.placement_requirement->>'hardware_identity' = me.hardware_identity",
		"j.placement_requirement->>'engine_build_hash' = COALESCE(w2.build_hash,'')",
		"j.placement_requirement->>'hardware_identity' = COALESCE(w2.hardware_identity,'')",
		"j.placement_requirement->>'engine_build_hash' = COALESCE(w3.build_hash,'')",
		"j.placement_requirement->>'hardware_identity' = COALESCE(w3.hardware_identity,'')",
	} {
		if !strings.Contains(sql, predicate) {
			t.Errorf("scheduler is missing exact current claim predicate %q", predicate)
		}
	}
}

func TestEveryCurrentSchedulerCapabilityExpiresFromSourceBenchmarkTime(t *testing.T) {
	raw, err := os.ReadFile("scheduler.go")
	must(t, err)
	const predicate = "authorized_at >= now() - interval '7 days'"
	if got := strings.Count(string(raw), predicate); got != 3 {
		t.Fatalf("scheduler source-benchmark freshness predicates=%d, want main+w2+w3", got)
	}

	installTestOnlyCombinedTokenAuthority(t)
	registeredAt := runtimeCellPerformanceNow().UTC()
	measuredAt := registeredAt.Add(-6*24*time.Hour - 23*time.Hour).Truncate(time.Second)
	cap := productionMetalCapability()
	cap.Benchmarks[1].MeasuredUnix = uint64(measuredAt.Unix())
	authorized := generatedRuntimeCapability{
		ID: "candle-metal-llama1-infer", Job: "batch_infer",
		Model: "llama-3.2-1b-instruct-q4",
	}
	got := workerCapabilityAuthorizedAt(cap, authorized, true, registeredAt)
	if !got.Equal(measuredAt) {
		t.Fatalf("re-registration refreshed source benchmark time: got %s want %s", got, measuredAt)
	}
	claimAt := measuredAt.Add(workerBenchmarkMaxAge + time.Second)
	if !got.Before(claimAt.Add(-workerBenchmarkMaxAge)) {
		t.Fatalf("source-aged authorization remained claimable after source+7d: source=%s claim=%s",
			got, claimAt)
	}
}

func TestPinnedTiebreakEligibilityBindsCurrentPlacementBuildAndDevice(t *testing.T) {
	raw, err := os.ReadFile("verification_lifecycle.go")
	must(t, err)
	sql := string(raw)
	for _, predicate := range []string{
		"COALESCE(nw.build_hash,'')=COALESCE(j.placement_requirement->>'engine_build_hash','')",
		"COALESCE(nw.hardware_identity,'')=COALESCE(j.placement_requirement->>'hardware_identity','')",
	} {
		if !strings.Contains(sql, predicate) {
			t.Errorf("pinned tiebreak eligibility is missing %q", predicate)
		}
	}
}

func TestDynamicPeerWritersLockWorkerBeforeReserveTaskAndJob(t *testing.T) {
	t.Helper()
	assertOrder := func(file string, ordered ...string) {
		t.Helper()
		raw, err := os.ReadFile(file)
		must(t, err)
		remaining := string(raw)
		for _, marker := range ordered {
			at := strings.Index(remaining, marker)
			if at < 0 {
				t.Fatalf("%s lacks lock-order marker %q", file, marker)
			}
			remaining = remaining[at+len(marker):]
		}
	}
	assertOrder("store_tasks.go",
		"func (s *Store) InsertTiebreakTask", "lockDynamicPeerWorkerTx", "lockEconomicReserveTx",
		"SELECT id FROM tasks WHERE id=$1 AND job_id=$2 FOR UPDATE", "lockLiveJobForDynamicTaskTx")
	assertOrder("store_tasks.go",
		"func (s *Store) InsertHedgeTask", "lockDynamicPeerWorkerTx", "lockEconomicReserveTx",
		"SELECT status FROM tasks", "lockLiveJobForDynamicTaskTx")
	assertOrder("verification_lifecycle.go",
		"func (s *Store) ReassignPinnedTiebreak", "lockDynamicPeerWorkerTx", "SELECT id FROM tasks",
		"SELECT status FROM jobs")
	assertOrder("verification_apply.go",
		"func (s *Store) applyVerificationDecision", "lockDynamicPeerWorkerTx",
		"lockVerificationWorkFenceTx", "lockEconomicReserveTx")
}

func TestDynamicPeerTransactionalPredicatesMatchCurrentClaimContainment(t *testing.T) {
	for _, file := range []string{"store_tasks.go", "verification_lifecycle.go"} {
		raw, err := os.ReadFile(file)
		must(t, err)
		sql := string(raw)
		for _, predicate := range []string{
			// Shared helper expands to sandboxed OR directed_cell_id + opt-in ban.
			`workerJobContainmentSQL("nw", "j")`,
			"supplierNotLinkedToBuyerSQL(\"ns\")",
			"wac.authorized_at>=now()-interval '7 days'",
			"j.placement_requirement->>'engine_build_identity_policy'",
			"j.placement_requirement->>'hardware_identity'",
		} {
			if !strings.Contains(sql, predicate) {
				t.Errorf("%s dynamic peer predicate lacks %q", file, predicate)
			}
		}
	}
}
