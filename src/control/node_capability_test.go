package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testNodeCapabilityInput builds a realistic BuildNodeCapability input from the
// production metal fixture without requiring a database.
func testNodeCapabilityInput(t *testing.T, mutate func(*WorkerCapability)) NodeCapabilityBuildInput {
	t.Helper()
	installTestOnlyCombinedTokenAuthority(t)
	cap := productionMetalCapability()
	_, _, benchmark, err := currentRuntimeCellBenchmarkIdentity("candle-metal-llama1-infer")
	if err != nil {
		t.Fatalf("resolve benchmark identity: %v", err)
	}
	cap.HWClass = benchmark.HWClass
	cap.BuildHash = benchmark.EngineBuildHash
	cap.HardwareIdentity = benchmark.HardwareIdentity
	cap.WorkerID = uuid.New()
	cap.SupplierID = uuid.New()
	cap.AgentVersion = "0.0.0-test"
	cap.OSVersion = "macOS-15.0"
	cap.GPUCount = 1
	cap.MemoryGBPerGPU = cap.MemoryGB
	cap.Sandboxed = true
	if mutate != nil {
		mutate(&cap)
	}
	projected, err := projectWorkerRuntimeCapabilities(cap)
	if err != nil {
		t.Fatalf("project capabilities: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	authAt := make(map[string]time.Time, len(projected))
	for _, c := range projected {
		authAt[c.ID] = now
	}
	profile, err := ResolveWorkerRuntimeProfile(cap)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}
	return NodeCapabilityBuildInput{
		Registration:        cap,
		ProjectedCells:      projected,
		CellAuthorizedAt:    authAt,
		SupplierDataCountry: "US",
		ProfileID:           profile.RuntimeID,
		ProfileRev:          profile.Revision,
		ProfileDigest:       digest,
		MatrixSHA256:        generatedRuntimeMatrixSHA256,
		Epoch:               1,
		CapturedAt:          now,
		LastSeen:            now,
	}
}

func TestNodeCapabilityLosslessLegacyConversion(t *testing.T) {
	in := testNodeCapabilityInput(t, nil)
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("BuildNodeCapability: %v", err)
	}
	cap := in.Registration

	// Every registration-path fact from the Step 6 inventory must survive.
	if nc.HWClass.Value != cap.HWClass {
		t.Errorf("hw_class: got %q want %q", nc.HWClass.Value, cap.HWClass)
	}
	if nc.HardwareIdentity.Value != cap.HardwareIdentity {
		t.Errorf("hardware_identity: got %q want %q", nc.HardwareIdentity.Value, cap.HardwareIdentity)
	}
	if nc.Engine.Value != normalizeEngine(cap.Engine) {
		t.Errorf("engine: got %q want %q", nc.Engine.Value, normalizeEngine(cap.Engine))
	}
	if nc.MemoryGB.Value != float64(cap.MemoryGB) {
		t.Errorf("memory_gb: got %v want %v", nc.MemoryGB.Value, cap.MemoryGB)
	}
	if nc.MemoryBwGbps.Value != float64(cap.MemoryBwGbps) {
		t.Errorf("memory_bw_gbps: got %v want %v", nc.MemoryBwGbps.Value, cap.MemoryBwGbps)
	}
	if nc.GPUCount.Value != 1 {
		t.Errorf("gpu_count: got %d want 1", nc.GPUCount.Value)
	}
	if nc.OSVersion.Value != cap.OSVersion {
		t.Errorf("os_version: got %q want %q", nc.OSVersion.Value, cap.OSVersion)
	}
	if nc.AgentVersion.Value != cap.AgentVersion {
		t.Errorf("agent_version: got %q want %q", nc.AgentVersion.Value, cap.AgentVersion)
	}
	if nc.BuildHash.Value != cap.BuildHash {
		t.Errorf("build_hash: got %q want %q", nc.BuildHash.Value, cap.BuildHash)
	}
	if nc.BuildIdentityPolicy.Value != cap.BuildIdentityPolicy {
		t.Errorf("build_identity_policy: got %q want %q", nc.BuildIdentityPolicy.Value, cap.BuildIdentityPolicy)
	}
	if nc.Sandboxed.Value != "true" {
		t.Errorf("sandboxed: got %q", nc.Sandboxed.Value)
	}
	if nc.UnsandboxedOptIn.Value != "false" {
		t.Errorf("unsandboxed_opt_in: got %q", nc.UnsandboxedOptIn.Value)
	}
	if nc.ThermalOK.Value != "true" {
		t.Errorf("thermal_ok: got %q", nc.ThermalOK.Value)
	}
	if len(nc.Benchmarks) != len(cap.Benchmarks) {
		t.Errorf("benchmarks: got %d want %d", len(nc.Benchmarks), len(cap.Benchmarks))
	}
	if len(nc.RuntimeCells) == 0 {
		t.Error("runtime cells empty — projection lost")
	}
	if len(nc.SupportedJobs) == 0 || len(nc.SupportedModels) == 0 {
		t.Error("supported jobs/models lost")
	}
	// min_payout must NOT appear on the capability type (policy separation).
	raw, _ := json.Marshal(nc)
	if strings.Contains(string(raw), "min_payout") {
		t.Error("NodeCapability JSON must not carry min_payout (reservation policy)")
	}
	if strings.Contains(string(raw), "routable") {
		t.Error("NodeCapability JSON must not carry routable (activation policy)")
	}
	if strings.Contains(string(raw), "lifecycle") {
		t.Error("NodeCapability JSON must not carry lifecycle (activation policy)")
	}
	// Inventory helper for the report table.
	inv := capabilityLegacyInventory(cap, nc)
	if inv["min_payout_usd_hr_excluded"] == "" {
		t.Error("inventory should record excluded min_payout")
	}
}

func TestNodeCapabilityHasNoRoutabilityFields(t *testing.T) {
	// Structural proof: the type surface cannot express routability.
	rt := reflect.TypeOf(NodeCapability{})
	forbidden := []string{"Routable", "Lifecycle", "Canary", "DirectedEligible", "MinPayout", "ActivationPolicy"}
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("NodeCapability field %s smuggles policy (%s)", name, bad)
			}
		}
	}
	// Cell refs likewise.
	crt := reflect.TypeOf(CapabilityCellRef{})
	for i := 0; i < crt.NumField(); i++ {
		name := crt.Field(i).Name
		if strings.Contains(name, "Routable") || strings.Contains(name, "Lifecycle") {
			t.Errorf("CapabilityCellRef field %s smuggles policy", name)
		}
	}
}

func TestNodeCapabilityDigestTamperRejection(t *testing.T) {
	in := testNodeCapabilityInput(t, nil)
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := nc.VerifyDigest(); err != nil {
		t.Fatalf("fresh digest should verify: %v", err)
	}
	// Mutate a fact without re-sealing.
	nc.HWClass.Value = "nvidia_80gb"
	if err := nc.VerifyDigest(); err == nil {
		t.Fatal("tampered snapshot must fail VerifyDigest")
	}
	// Mutate digest bytes.
	in2 := testNodeCapabilityInput(t, nil)
	nc2, err := BuildNodeCapability(in2)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	nc2.Digest = strings.Repeat("ab", 32) // valid hex shape, wrong content
	if err := nc2.VerifyDigest(); err == nil {
		t.Fatal("wrong digest must fail VerifyDigest")
	}
}

func TestNodeCapabilityUnknownNeverSatisfiesHardContract(t *testing.T) {
	in := testNodeCapabilityInput(t, nil)
	// Old agent: no failure domain, no interruption, no disk.
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	now := nc.CapturedAt.Add(time.Second)
	if nc.FailureDomain.Knowledge != capabilityKnowledgeUnknown {
		t.Fatalf("failure_domain knowledge=%s want unknown", nc.FailureDomain.Knowledge)
	}
	if nc.InterruptionPolicy.Knowledge != capabilityKnowledgeUnknown {
		t.Fatalf("interruption knowledge=%s want unknown", nc.InterruptionPolicy.Knowledge)
	}
	if nc.DiskGB.Knowledge != capabilityKnowledgeUnknown {
		t.Fatalf("disk knowledge=%s want unknown", nc.DiskGB.Knowledge)
	}

	err = nc.SatisfiesHardContract([]HardCapabilityRequirement{{
		Family: capabilityFamilyFailureDomain,
		Kind:   "eq",
		Want:   "rack-a1",
		Field:  "failure_domain",
	}}, now)
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("UNKNOWN failure_domain must refuse hard contract, got %v", err)
	}

	err = nc.SatisfiesHardContract([]HardCapabilityRequirement{{
		Family: capabilityFamilyInterruption,
		Kind:   "eq",
		Want:   "non_preemptible",
		Field:  "interruption_policy",
	}}, now)
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("UNKNOWN interruption must refuse hard contract, got %v", err)
	}

	err = nc.SatisfiesHardContract([]HardCapabilityRequirement{{
		Family:  capabilityFamilyStorage,
		Kind:    "min_f64",
		WantF64: 100,
		Field:   "disk_gb",
	}}, now)
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("UNKNOWN disk must refuse hard contract, got %v", err)
	}
}

func TestNodeCapabilityStaleFactCannotSatisfyHardContract(t *testing.T) {
	in := testNodeCapabilityInput(t, nil)
	// Stamp availability far in the past so the 60s TTL expires.
	old := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	in.LastSeen = old
	in.CapturedAt = old
	for k := range in.CellAuthorizedAt {
		in.CellAuthorizedAt[k] = old
	}
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	now := time.Now().UTC()
	if nc.FamilyFresh(capabilityFamilyAvailability, now) {
		t.Fatal("availability family should be stale after 2h")
	}
	// hw_class has TTL 0 (enrollment) so remains fresh — contrast.
	if !nc.FamilyFresh(capabilityFamilyHardware, now) {
		t.Fatal("hardware family has no rolling TTL and should stay fresh")
	}
	err = nc.SatisfiesHardContract([]HardCapabilityRequirement{{
		Family: capabilityFamilyAvailability,
		Kind:   "known",
		Field:  "hw_class", // field only used after freshness; use a known field check path
	}}, now)
	// Freshness fails first for availability family.
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale availability must refuse hard contract, got %v", err)
	}

	// Runtime cells family: 7d window. Age past 8 days.
	older := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	in2 := testNodeCapabilityInput(t, nil)
	in2.CapturedAt = older
	in2.LastSeen = older
	for k := range in2.CellAuthorizedAt {
		in2.CellAuthorizedAt[k] = older
	}
	// Benchmarks must also be aged or registration projection refuses — we
	// bypass projection by reusing already-projected cells from a fresh build
	// and only aging auth stamps (simulates claim-time staleness of wac).
	in2.ProjectedCells = in.ProjectedCells
	in2.CellAuthorizedAt = map[string]time.Time{}
	for _, c := range in2.ProjectedCells {
		in2.CellAuthorizedAt[c.ID] = older
	}
	// Keep registration benchmarks "fresh enough" for Build — Build doesn't
	// re-validate projection; it only converts.
	nc2, err := BuildNodeCapability(in2)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	if nc2.FamilyFresh(capabilityFamilyRuntimeCells, now) {
		t.Fatal("runtime_cells family should be stale after 8d")
	}
	err = nc2.SatisfiesHardContract([]HardCapabilityRequirement{{
		Family: capabilityFamilyRuntimeCells,
		Kind:   "eq",
		Want:   nc2.HWClass.Value,
		Field:  "hw_class",
	}}, now)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale runtime_cells must refuse, got %v", err)
	}
}

func TestNodeCapabilityRegionSupplierDerivedProvenance(t *testing.T) {
	// Agent omits region → supplier-derived.
	in := testNodeCapabilityInput(t, func(c *WorkerCapability) {
		c.Region = ""
	})
	in.SupplierDataCountry = "DE"
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if nc.Region.Knowledge != capabilityKnowledgeSupplierDerived {
		t.Fatalf("region knowledge=%s want supplier_derived", nc.Region.Knowledge)
	}
	if nc.Region.Value != "DE" {
		t.Fatalf("region value=%s want DE", nc.Region.Value)
	}
	// Agent declares region → declared, not supplier.
	in2 := testNodeCapabilityInput(t, func(c *WorkerCapability) {
		c.Region = "eu-central-1"
	})
	in2.SupplierDataCountry = "DE"
	nc2, err := BuildNodeCapability(in2)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	if nc2.Region.Knowledge != capabilityKnowledgeDeclared {
		t.Fatalf("declared region knowledge=%s", nc2.Region.Knowledge)
	}
	if nc2.Region.Value != "eu-central-1" {
		t.Fatalf("region=%s", nc2.Region.Value)
	}
}

func TestNodeCapabilityPowerPolicyStaysAssumed(t *testing.T) {
	in := testNodeCapabilityInput(t, nil)
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if nc.PowerPolicyWatts.Knowledge != capabilityKnowledgeAssumed &&
		nc.PowerPolicyWatts.Knowledge != capabilityKnowledgeMeasured {
		t.Fatalf("power knowledge=%s", nc.PowerPolicyWatts.Knowledge)
	}
	// Apple classes are ASSUMED today — never silently MEASURED.
	if nc.PowerPolicyWatts.Knowledge != capabilityKnowledgeAssumed {
		t.Fatalf("apple class power must stay ASSUMED, got %s", nc.PowerPolicyWatts.Knowledge)
	}
}

func TestNodeCapabilityMultiGPUTopologyRoundTrip(t *testing.T) {
	// Topology capture is independent of profile device-count admission. Build
	// from a valid single-device registration input, then set multi-GPU wire
	// fields on the registration before BuildNodeCapability (projection already
	// done for a valid host).
	in := testNodeCapabilityInput(t, nil)
	in.Registration.GPUCount = 4
	in.Registration.MemoryGBPerGPU = 9
	in.Registration.Interconnect = "pcie"
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if nc.GPUCount.Value != 4 {
		t.Fatalf("gpu_count=%d", nc.GPUCount.Value)
	}
	if nc.MemoryGBPerGPU.Value != 9 {
		t.Fatalf("mem/gpu=%v", nc.MemoryGBPerGPU.Value)
	}
	if nc.Interconnect.Value != "pcie" || nc.Interconnect.Knowledge != capabilityKnowledgeDeclared {
		t.Fatalf("interconnect=%+v", nc.Interconnect)
	}
}

func TestNodeCapabilityMeasuredDiskPersists(t *testing.T) {
	disk := float32(512)
	in := testNodeCapabilityInput(t, func(c *WorkerCapability) {
		c.DiskGB = &disk
	})
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if nc.DiskGB.Knowledge != capabilityKnowledgeMeasured || nc.DiskGB.Value != 512 {
		t.Fatalf("disk=%+v", nc.DiskGB)
	}
}

func TestActivationRoutableProjectionAgreesWithDualWriteSemantics(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	// Every advertised cell is routable; a directed-only cell is not.
	for _, cell := range advertisedRuntimeCapabilities() {
		if !activationRoutableProjection(cell.ID) {
			t.Errorf("advertised cell %s must be activation-routable", cell.ID)
		}
		// Dual-write column would store true; legacy claim with NULL decision
		// would accept.
		if !legacyClaimRequiresRoutable(false, activationRoutableProjection(cell.ID)) {
			t.Errorf("legacy claim must accept advertised cell %s", cell.ID)
		}
	}
	// Find a directed-only cell if any exist under current activation.
	advertised := map[string]bool{}
	for _, c := range advertisedRuntimeCapabilities() {
		advertised[c.ID] = true
	}
	for _, c := range directedRuntimeCapabilities() {
		if advertised[c.ID] {
			continue
		}
		if activationRoutableProjection(c.ID) {
			t.Errorf("directed-only cell %s must NOT be activation-routable", c.ID)
		}
		// Legacy job (no workload_decision) must refuse directed-only.
		if legacyClaimRequiresRoutable(false, activationRoutableProjection(c.ID)) {
			t.Errorf("legacy claim must refuse directed-only cell %s", c.ID)
		}
		// Frozen workload decision present: routable gate not applied (SQL
		// matches frozen cell identity instead).
		if !legacyClaimRequiresRoutable(true, activationRoutableProjection(c.ID)) {
			t.Errorf("frozen decision path must not hard-require routable for %s", c.ID)
		}
	}
}

func TestNodeCapabilityOldAgentWireCompatibility(t *testing.T) {
	// Old agent payload: no region, failure_domain, interruption, disk,
	// gpu_count, interconnect — still builds and stays eligible for today's
	// work (no hard contract on the new facts).
	in := testNodeCapabilityInput(t, func(c *WorkerCapability) {
		c.Region = ""
		c.FailureDomain = ""
		c.InterruptionPolicy = ""
		c.DiskGB = nil
		c.GPUCount = 0
		c.MemoryGBPerGPU = 0
		c.Interconnect = ""
		c.OSVersion = ""
	})
	in.SupplierDataCountry = ""
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("old agent build: %v", err)
	}
	if nc.GPUCount.Value != 1 {
		t.Fatalf("default gpu_count want 1 got %d", nc.GPUCount.Value)
	}
	if nc.FailureDomain.Knowledge != capabilityKnowledgeUnknown {
		t.Fatal("missing failure_domain must be UNKNOWN")
	}
	// Hard contracts on existing facts still work.
	now := nc.CapturedAt.Add(time.Second)
	err = nc.SatisfiesHardContract([]HardCapabilityRequirement{{
		Family: capabilityFamilyHardware,
		Kind:   "eq",
		Want:   nc.HWClass.Value,
		Field:  "hw_class",
	}}, now)
	if err != nil {
		t.Fatalf("old agent should still satisfy hw_class contract: %v", err)
	}
}

func TestNodeCapabilitySnapshotRoundTripJSON(t *testing.T) {
	in := testNodeCapabilityInput(t, nil)
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	blob, err := capabilitySnapshotJSON(nc)
	if err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	back, err := ParseCapabilitySnapshot(blob)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if back.Digest != nc.Digest {
		t.Fatalf("digest drift: %s vs %s", back.Digest, nc.Digest)
	}
	if back.Epoch != nc.Epoch || back.HWClass.Value != nc.HWClass.Value {
		t.Fatalf("round-trip drift: %+v vs %+v", back.HWClass, nc.HWClass)
	}
	// Tamper JSON body.
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	hw := m["hw_class"].(map[string]any)
	hw["value"] = "tampered"
	m["hw_class"] = hw
	evil, _ := json.Marshal(m)
	if _, err := ParseCapabilitySnapshot(evil); err == nil {
		t.Fatal("tampered snapshot JSON must be rejected")
	}
}

func TestNodeCapabilityDeclaredFailureDomainSatisfies(t *testing.T) {
	in := testNodeCapabilityInput(t, func(c *WorkerCapability) {
		c.FailureDomain = "rack-a1"
		c.InterruptionPolicy = "non_preemptible"
	})
	nc, err := BuildNodeCapability(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	now := nc.CapturedAt.Add(time.Second)
	err = nc.SatisfiesHardContract([]HardCapabilityRequirement{
		{Family: capabilityFamilyFailureDomain, Kind: "eq", Want: "rack-a1", Field: "failure_domain"},
		{Family: capabilityFamilyInterruption, Kind: "eq", Want: "non_preemptible", Field: "interruption_policy"},
	}, now)
	if err != nil {
		t.Fatalf("declared facts should satisfy: %v", err)
	}
}
