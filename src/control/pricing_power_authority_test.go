package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestIncompleteGPUOnlyTelemetryStillRefuses(t *testing.T) {
	err := classifyLocalPackagePowerTelemetry(localPackagePowerTelemetry{
		CPUWatts: 0, ANEWatts: 0, GPUWatts: 131.69,
	})
	if err == nil {
		t.Fatal("GPU-only telemetry was accepted as package power")
	}
	if !strings.Contains(err.Error(), "refusing GPU-only") &&
		!strings.Contains(err.Error(), "incomplete local package telemetry") {
		t.Fatalf("GPU-only refusal=%v", err)
	}
	if !strings.Contains(err.Error(), localFailureCPUPowerSensorZero) {
		t.Fatalf("refusal must name cpu_power_sensor_zero: %v", err)
	}

	// A MEASURED constructor from GPU-only telemetry must not exist. The
	// classifier is the gate the seal script also enforces.
	reasons := localFailureReasonsFromTelemetry(localPackagePowerTelemetry{
		CPUWatts: 0, ANEWatts: 0, GPUWatts: 131.69,
	})
	if len(reasons) != 2 || reasons[0] != localFailureCPUPowerSensorZero ||
		reasons[1] != localFailureANEPowerSensorZero {
		t.Fatalf("local failure reasons=%v", reasons)
	}

	// Selecting GPU-only from the fallback hierarchy is invalid.
	_, err = selectEconomicPowerEnvelope([]economicPowerCandidate{{
		Name: "gpu-only", Rank: economicPowerRankGPUOnly, Watts: 131.69, Applicable: true,
	}})
	if err == nil || !strings.Contains(err.Error(), "GPU-only") {
		t.Fatalf("GPU-only candidate selected: %v", err)
	}
}

func TestVendorWallUpperBoundEnablesEconomicBoot(t *testing.T) {
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("VENDOR_WALL_UPPER_BOUND must enable BuildCataloguePriceSchedule: %v", err)
	}
	if len(schedule.Results) < 1 {
		t.Fatal("want >=1 published lane")
	}
	foundLlama := false
	for _, r := range schedule.Results {
		if r.ModelID == "llama-3.2-1b-instruct-q4" && r.JobType == "batch_infer" {
			foundLlama = true
			if r.PhysicalAuthority.Power.SourceClass != string(wattKindVendorWallUpperBound) {
				t.Fatalf("published llama source_class=%q", r.PhysicalAuthority.Power.SourceClass)
			}
			if r.PhysicalAuthority.Power.Watts != 270 {
				t.Fatalf("published llama watts=%v, want 270", r.PhysicalAuthority.Power.Watts)
			}
			if r.PhysicalAuthority.Power.SourceClass == string(wattKindMeasured) {
				t.Fatal("vendor wall stored as MEASURED")
			}
		}
	}
	if !foundLlama {
		t.Fatal("llama lane was not published")
	}
}

func TestVendorWallMovesSupplierEconomicsOnlyConservatively(t *testing.T) {
	entry := sustainedWattsByHWClass["apple_silicon_ultra"]
	if entry.Kind() != wattKindVendorWallUpperBound || entry.Watts() != 270 {
		t.Fatalf("ultra entry=%s %.0fW, want VENDOR_WALL_UPPER_BOUND 270W", entry.Kind(), entry.Watts())
	}
	if entry.Watts() <= 65 {
		t.Fatal("vendor bound must not be cheaper than the retired 65W understatement")
	}

	rows := SupplierViabilityReport()
	if len(rows) == 0 {
		t.Fatal("viability report is empty")
	}
	wantElec := roundUSD(270.0 / 1000.0 * defaultElectricityUSDPerKWh)
	cheapElec := roundUSD(65.0 / 1000.0 * defaultElectricityUSDPerKWh)
	sawUnderwater := false
	for _, r := range rows {
		if r.HWClass != "apple_silicon_ultra" {
			continue
		}
		if r.ElectricityUSDHr != wantElec {
			t.Fatalf("%s electricity=$%.6f/hr, want $%.6f/hr from 270W",
				r.ModelID, r.ElectricityUSDHr, wantElec)
		}
		if r.ElectricityUSDHr <= cheapElec {
			t.Fatalf("270W envelope is not conservative vs 65W: got $%.6f vs $%.6f",
				r.ElectricityUSDHr, cheapElec)
		}
		if r.Viable {
			t.Fatalf("%s looks viable under a conservative 270W bound; that would hide underwater economics",
				r.ModelID)
		}
		if r.NetUSDHr >= 0 {
			t.Fatalf("%s net=$%.6f; underwater warning requires negative net", r.ModelID, r.NetUSDHr)
		}
		warning := fmt.Sprintf(
			"WARNING: supplier economics underwater: model=%s job=%s hw=%s "+
				"gross=$%.6f/hr electricity=$%.6f/hr net=$%.6f/hr; "+
				"supplier floor uses %.0fW VENDOR_WALL_UPPER_BOUND",
			r.ModelID, r.JobType, r.HWClass, r.SupplierGrossUSDHr, r.ElectricityUSDHr, r.NetUSDHr,
			entry.Watts())
		if !strings.Contains(warning, "underwater") ||
			!strings.Contains(warning, "270W") ||
			!strings.Contains(warning, string(wattKindVendorWallUpperBound)) {
			t.Fatalf("warning not honest/visible: %s", warning)
		}
		t.Logf("%s", warning)
		sawUnderwater = true
	}
	if !sawUnderwater {
		t.Fatal("no apple_silicon_ultra viability row to show the honest underwater warning")
	}
}

func TestVendorWallCannotSatisfyMeasuredEnergy(t *testing.T) {
	entry := sustainedWattsByHWClass["apple_silicon_ultra"]
	if err := acceptEnergyMeasurement(entry); err == nil {
		t.Fatal("VENDOR_WALL_UPPER_BOUND satisfied ENERGY_MEASUREMENT")
	} else if !strings.Contains(err.Error(), energyMeasurementAuthority) ||
		!strings.Contains(err.Error(), measuredEnergyEvidenceKind) ||
		!strings.Contains(err.Error(), string(wattKindVendorWallUpperBound)) {
		t.Fatalf("energy refusal=%v", err)
	}

	comp := frozenEnergyComponent("apple_silicon_ultra", 10, 1)
	if comp.Status != pricingCostUnknown {
		t.Fatalf("frozen energy from vendor wall status=%s, want unknown", comp.Status)
	}
	if !strings.Contains(comp.Basis, energyMeasurementAuthority) &&
		!strings.Contains(comp.Basis, measuredEnergyEvidenceKind) &&
		!strings.Contains(comp.Basis, "VENDOR_WALL") {
		t.Fatalf("frozen energy refusal basis=%s", comp.Basis)
	}

	// Catalogue-frozen energy must also refuse vendor wall as MEASURED joules.
	schedule, err := BuildCataloguePriceSchedule()
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	comp, joules, kind, _, err := frozenEnergyComponentFromCatalogue(
		schedule.Results[0].PhysicalAuthority, "apple_silicon_ultra", 1)
	if err != nil {
		t.Fatalf("catalogue energy freeze: %v", err)
	}
	if comp.Status != pricingCostUnknown || joules != 0 || kind == string(wattKindMeasured) {
		t.Fatalf("catalogue energy from vendor wall status=%s joules=%v kind=%s",
			comp.Status, joules, kind)
	}
	if !strings.Contains(comp.Basis, measuredEnergyEvidenceKind) {
		t.Fatalf("catalogue energy refusal basis=%s", comp.Basis)
	}
}

func TestVendorWallMismatchedHardwareFamilyRefuses(t *testing.T) {
	previous := sustainedWattsByHWClass["nvidia_80gb"]
	t.Cleanup(func() { sustainedWattsByHWClass["nvidia_80gb"] = previous })
	sustainedWattsByHWClass["nvidia_80gb"] = wattsVendorWallUpperBound(appleSiliconUltraVendorWallSpec())

	_, err := sustainedWattsEntryForPublication("nvidia_80gb")
	if err == nil || !strings.Contains(err.Error(), "cannot cover hardware class") {
		t.Fatalf("apple vendor wall covering cuda class error=%v", err)
	}

	b := measuredThroughput{
		ModelID: "test-model", JobType: "batch_infer",
		Unit: "tokens", UnitScope: performanceUnitScopeTokenLikeInputPlusOutputTokens,
		UnitsPerSec: 100, HWClass: "nvidia_80gb",
		HardwareIdentity: "cuda-a100",
	}
	err = validatePricingPowerCitation(b, sustainedWattsByHWClass["nvidia_80gb"], cataloguePowerNow())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot cover") {
		t.Fatalf("apple bound on cuda cell error=%v", err)
	}

	if err := vendorWallCoversHardware(
		sustainedWattsByHWClass["apple_silicon_ultra"].vendorWall,
		"apple_silicon_pro",
		"Apple M3 Pro",
	); err == nil {
		t.Fatal("m3_ultra bound covered apple_silicon_pro")
	}
}

func TestVendorWallCitationTamperingRefuses(t *testing.T) {
	previous := sustainedWattsByHWClass["apple_silicon_ultra"]
	t.Cleanup(func() { sustainedWattsByHWClass["apple_silicon_ultra"] = previous })

	tampered := previous
	if tampered.vendorWall == nil {
		t.Fatal("production ultra row has no vendor-wall provenance")
	}
	cp := *tampered.vendorWall
	cp.citationDigest = strings.Repeat("a", 64)
	tampered.vendorWall = &cp
	tampered.receiptSHA256 = cp.citationDigest
	sustainedWattsByHWClass["apple_silicon_ultra"] = tampered
	_, err := sustainedWattsEntryForPublication("apple_silicon_ultra")
	if err == nil || !strings.Contains(err.Error(), "citation_digest") {
		t.Fatalf("wrong digest error=%v", err)
	}

	missing := previous
	mcp := *previous.vendorWall
	mcp.vendor = ""
	missing.vendorWall = &mcp
	sustainedWattsByHWClass["apple_silicon_ultra"] = missing
	_, err = sustainedWattsEntryForPublication("apple_silicon_ultra")
	if err == nil || !strings.Contains(err.Error(), "vendor") {
		t.Fatalf("missing vendor field error=%v", err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("constructor with empty vendor must panic")
		}
	}()
	spec := appleSiliconUltraVendorWallSpec()
	spec.Vendor = ""
	_ = wattsVendorWallUpperBound(spec)
}

func TestPSUCeilingIsNotSilentlySubstitutedForVendorWall(t *testing.T) {
	vendor := economicPowerCandidate{
		Name: "vendor-270", Rank: economicPowerRankVendorWallUpperBound,
		Watts: appleMacStudio2025M3UltraWallMaxWatts, Applicable: true,
		Entry: sustainedWattsByHWClass["apple_silicon_ultra"],
	}
	psu := economicPowerCandidate{
		Name: "psu-480", Rank: economicPowerRankPSUCeiling,
		Watts: appleMacStudio2025PSUCeilingWatts, Applicable: true,
	}
	got, err := selectEconomicPowerEnvelope([]economicPowerCandidate{psu, vendor})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got.Watts != 270 || got.Rank != economicPowerRankVendorWallUpperBound {
		t.Fatalf("selected %+v, want 270W vendor wall", got)
	}

	// If a caller tries to present 480W as the vendor-wall row, refuse.
	entry := sustainedWattsByHWClass["apple_silicon_ultra"]
	if entry.vendorWall != nil {
		bad := entry
		cp := *entry.vendorWall
		cp.wattsUpperBound = appleMacStudio2025PSUCeilingWatts
		bad.vendorWall = &cp
		bad.watts = appleMacStudio2025PSUCeilingWatts
		if err := acceptEconomicPowerEnvelope(bad, "apple_silicon_ultra",
			repricingBenchmarks[0].HardwareIdentity); err == nil {
			t.Fatal("480W silently accepted as the economic envelope while 270W is applicable")
		}
	}

	// 480W last resort only when 270W is inapplicable.
	got, err = selectEconomicPowerEnvelope([]economicPowerCandidate{psu})
	if err != nil {
		t.Fatalf("last-resort 480W: %v", err)
	}
	if got.Watts != 480 {
		t.Fatalf("last-resort selected %v, want 480", got.Watts)
	}
	_, err = selectEconomicPowerEnvelope([]economicPowerCandidate{
		{Name: "psu-as-best", Rank: economicPowerRankPSUCeiling, Watts: 480, Applicable: true},
		{Name: "vendor-270", Rank: economicPowerRankVendorWallUpperBound, Watts: 270, Applicable: true},
	})
	if err != nil {
		// selecting vendor wall is the success path; already covered above
		t.Fatalf("hierarchy with both present: %v", err)
	}
}

func TestVendorWallConstructorRequiresEveryProvenanceField(t *testing.T) {
	spec := appleSiliconUltraVendorWallSpec()
	if spec.WattsUpperBound != 270 || spec.Vendor != "apple" ||
		spec.ProductFamily != "mac_studio_2025" || spec.SOCFamily != "m3_ultra" ||
		spec.MeasurementScope != "AC_WALL" || !spec.IncludesPSULosses ||
		spec.WorkloadSpecific || spec.LocalMeasurementAvailable ||
		spec.MeasuredConfig != "32CPU/80GPU" || spec.LocalConfig != "28CPU/60GPU" {
		t.Fatalf("apple ultra spec not the conservative family/chassis bound: %+v", spec)
	}
	got := wattsVendorWallUpperBound(spec)
	if got.Kind() == wattKindMeasured {
		t.Fatal("constructor stored vendor wall as MEASURED")
	}
	if got.Kind() != wattKindVendorWallUpperBound || got.Watts() != 270 {
		t.Fatalf("constructed %+v", got)
	}
	if got.vendorWall == nil || got.vendorWall.wattsUpperBound != 270 {
		t.Fatal("missing typed watts_upper_bound provenance")
	}
}

func TestAppleCitationDigestMatchesPinnedAuthorityText(t *testing.T) {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(appleMacStudio2025WallPowerCitation)))
	if sum != appleMacStudio2025WallPowerCitationDigest {
		t.Fatalf("citation digest drifted: hashed=%s pinned=%s", sum, appleMacStudio2025WallPowerCitationDigest)
	}
	if !strings.Contains(appleMacStudio2025WallPowerCitation, "270 W") ||
		!strings.Contains(appleMacStudio2025WallPowerCitation, "9 W") ||
		!strings.Contains(appleMacStudio2025WallPowerCitation, "480W") ||
		!strings.Contains(appleMacStudio2025WallPowerCitation, appleMacStudio2025PowerSupportURL) ||
		!strings.Contains(appleMacStudio2025WallPowerCitation, appleMacStudio2025SpecsURL) {
		t.Fatal("citation text is missing Apple's wall-power figures or URLs")
	}
}
