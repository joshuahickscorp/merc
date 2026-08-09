package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestUpsertWorkerPersistsCapabilitySnapshotAndActivationRoutable proves the
// dual-write path: sealed NodeCapability snapshot + workers projection columns
// + wac.routable == activationRoutableProjection.
func TestUpsertWorkerPersistsCapabilitySnapshotAndActivationRoutable(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	store, pool, ctx := freshMigratedDatabase(t)
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,reputation,status,data_country)
		 VALUES ($1,$2,0.5,'active','US')`,
		supplierID, "cap+"+supplierID.String()+"@example.test"); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}

	disk := float32(1024)
	cap := WorkerCapability{
		WorkerID: uuid.New(), SupplierID: supplierID,
		Engine: "candle", HWClass: "apple_silicon_pro", MemoryGB: 96,
		BuildHash: testOnlyPublicationBuildHash, BuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		HardwareIdentity: testOnlyPublicationHardware,
		AgentVersion:     "1.2.3", OSVersion: "macOS-15.1",
		GPUCount: 1, MemoryGBPerGPU: 96, Interconnect: "",
		SupportedJobs:   []string{"embed"},
		SupportedModels: []string{"all-minilm-l6-v2"},
		Sandboxed:       true,
		DiskGB:          &disk,
		FailureDomain:   "rack-test-1",
		// Region omitted → supplier-derived US
		Benchmarks: []BenchResult{{
			JobType: "embed", ModelID: "all-minilm-l6-v2", EPS: 3000, ThermalOK: true,
			Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
			MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix()),
		}},
	}
	if err := store.UpsertWorker(ctx, cap); err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}

	var (
		epoch                                                        int64
		digest, osVersion, region, regionProv, failureDomain, interr string
		gpuCount                                                     int
		diskGB                                                       *float64
	)
	if err := pool.QueryRow(ctx, `
		SELECT capability_epoch, capability_digest, COALESCE(os_version,''),
		       COALESCE(region,''), region_provenance, failure_domain,
		       interruption_policy, COALESCE(gpu_count,0), disk_gb
		  FROM workers WHERE id=$1`, cap.WorkerID).Scan(
		&epoch, &digest, &osVersion, &region, &regionProv, &failureDomain,
		&interr, &gpuCount, &diskGB,
	); err != nil {
		t.Fatalf("read workers projection: %v", err)
	}
	if epoch != 1 {
		t.Fatalf("epoch=%d want 1", epoch)
	}
	if len(digest) != 64 {
		t.Fatalf("digest len=%d", len(digest))
	}
	if osVersion != "macOS-15.1" {
		t.Fatalf("os_version=%q", osVersion)
	}
	if region != "US" || regionProv != capabilityKnowledgeSupplierDerived {
		t.Fatalf("region=%q provenance=%q want US/supplier_derived", region, regionProv)
	}
	if failureDomain != "rack-test-1" {
		t.Fatalf("failure_domain=%q", failureDomain)
	}
	if interr != capabilityUnknownValue {
		t.Fatalf("interruption_policy=%q want unknown", interr)
	}
	if gpuCount != 1 {
		t.Fatalf("gpu_count=%d", gpuCount)
	}
	if diskGB == nil || *diskGB != 1024 {
		t.Fatalf("disk_gb=%v", diskGB)
	}

	// Snapshot row is append-only and digest-verifiable.
	var snapEpoch int64
	var snapDigest string
	var snapBody []byte
	if err := pool.QueryRow(ctx, `
		SELECT epoch, digest, snapshot
		  FROM worker_capability_snapshots WHERE worker_id=$1 AND epoch=$2`,
		cap.WorkerID, epoch).Scan(&snapEpoch, &snapDigest, &snapBody); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snapDigest != digest {
		t.Fatalf("snapshot digest %s != workers.capability_digest %s", snapDigest, digest)
	}
	parsed, err := ParseCapabilitySnapshot(snapBody)
	if err != nil {
		t.Fatalf("ParseCapabilitySnapshot: %v", err)
	}
	if parsed.HWClass.Value != cap.HWClass {
		t.Fatalf("snapshot hw_class=%q", parsed.HWClass.Value)
	}
	if parsed.Region.Knowledge != capabilityKnowledgeSupplierDerived {
		t.Fatalf("snapshot region knowledge=%s", parsed.Region.Knowledge)
	}

	// Dual-write agreement: every wac.routable equals activation projection.
	rows, err := pool.Query(ctx, `
		SELECT cell_id, routable FROM worker_authorized_capabilities WHERE worker_id=$1`,
		cap.WorkerID)
	if err != nil {
		t.Fatalf("query wac: %v", err)
	}
	defer rows.Close()
	var saw int
	for rows.Next() {
		var cellID string
		var routable bool
		if err := rows.Scan(&cellID, &routable); err != nil {
			t.Fatalf("scan wac: %v", err)
		}
		want := activationRoutableProjection(cellID)
		if routable != want {
			t.Errorf("cell %s routable=%v want activation projection %v", cellID, routable, want)
		}
		saw++
	}
	if saw == 0 {
		t.Fatal("no wac rows dual-written")
	}

	// Re-register bumps epoch and appends a second snapshot.
	if err := store.UpsertWorker(ctx, cap); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	var epoch2, snapCount int64
	if err := pool.QueryRow(ctx,
		`SELECT capability_epoch FROM workers WHERE id=$1`, cap.WorkerID).Scan(&epoch2); err != nil {
		t.Fatalf("epoch2: %v", err)
	}
	if epoch2 != 2 {
		t.Fatalf("epoch after re-register=%d want 2", epoch2)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM worker_capability_snapshots WHERE worker_id=$1`,
		cap.WorkerID).Scan(&snapCount); err != nil {
		t.Fatalf("snap count: %v", err)
	}
	if snapCount != 2 {
		t.Fatalf("snapshot count=%d want 2", snapCount)
	}

	// Append-only while the worker lives: UPDATE and standalone DELETE refuse.
	// CASCADE delete when the worker row is removed is allowed (fixture cleanup).
	_, err = pool.Exec(ctx,
		`UPDATE worker_capability_snapshots SET digest=$2 WHERE worker_id=$1 AND epoch=1`,
		cap.WorkerID, strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("snapshot UPDATE must be refused")
	}
	_, err = pool.Exec(ctx,
		`DELETE FROM worker_capability_snapshots WHERE worker_id=$1 AND epoch=1`,
		cap.WorkerID)
	if err == nil {
		t.Fatal("standalone snapshot DELETE must be refused while worker exists")
	}
}

// TestLegacyClaimRoutableGateUnchanged documents that the claim-path decision
// function still refuses directed-only cells for workload_decision IS NULL.
func TestLegacyClaimRoutableGateUnchanged(t *testing.T) {
	// Pure decision function — same inputs → same accept/reject as pre-Step-6.
	cases := []struct {
		name            string
		decisionPresent bool
		routable        bool
		want            bool
	}{
		{"legacy_routable", false, true, true},
		{"legacy_directed_only", false, false, false},
		{"frozen_routable", true, true, true},
		{"frozen_directed", true, false, true},
	}
	for _, tc := range cases {
		got := legacyClaimRequiresRoutable(tc.decisionPresent, tc.routable)
		if got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
