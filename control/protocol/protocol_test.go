package protocol

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type fixtureBindingResolver map[string]EvidenceBindingResolution

func (r fixtureBindingResolver) ResolveEvidenceBinding(ref ContractRef) (EvidenceBindingResolution, error) {
	value, ok := r[ref.CanonicalSHA256]
	if !ok {
		return EvidenceBindingResolution{}, fmt.Errorf("binding artifact %q not found", ref.ID)
	}
	return value, nil
}

var fixtureTime = time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)

func fixtureHeader(kind, id string) ContractHeader {
	return ContractHeader{
		SchemaVersion:  SchemaVersionV1,
		Kind:           kind,
		ID:             id,
		PolicyRevision: "protocol-v1-test",
		CreatedAt:      fixtureTime,
	}
}

func fixtureDigest(seed string) string {
	return strings.Repeat(seed, 64)
}

func mustSeal(t *testing.T, value interface{ Seal() error }) {
	t.Helper()
	if err := value.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
}

func fixtureBindingArtifact(t *testing.T, evidenceID string, status BindingStatus) EvidenceBindingArtifact {
	t.Helper()
	value := EvidenceBindingArtifact{
		Header:        fixtureHeader(KindEvidenceBinding, "binding-artifact-"+evidenceID),
		EvidenceID:    evidenceID,
		BindingStatus: status,
		SourceCommit:  strings.Repeat("a", 40),
		PayloadSHA256: fixtureDigest("b"),
		HarnessSHA256: fixtureDigest("c"),
	}
	mustSeal(t, &value)
	if err := value.Validate(); err != nil {
		t.Fatalf("fixture binding artifact: %v", err)
	}
	return value
}

func fixtureEvidenceWithID(t *testing.T, id string) EvidenceEnvelope {
	t.Helper()
	artifact := fixtureBindingArtifact(t, id, BindingBound)
	value := EvidenceEnvelope{
		Header:          fixtureHeader(KindEvidenceEnvelope, id),
		BindingStatus:   BindingBound,
		BindingArtifact: Ref(artifact.Header),
		SourceCommit:    strings.Repeat("a", 40),
		ProducerID:      "protocol-test",
		PayloadSHA256:   fixtureDigest("b"),
		HarnessSHA256:   fixtureDigest("c"),
	}
	mustSeal(t, &value)
	if err := value.Validate(); err != nil {
		t.Fatalf("fixture evidence %q: %v", id, err)
	}
	return value
}

func fixtureEvidence(t *testing.T) EvidenceEnvelope {
	t.Helper()
	return fixtureEvidenceWithID(t, "evidence-v1")
}

func fixtureProvider(t *testing.T) (ProviderAdapterSnapshot, RegionIdentity, FailureDomainIdentity, []RuntimeCellCapability) {
	t.Helper()
	evidence := fixtureEvidence(t)
	region := RegionIdentity{ProviderID: "provider-test", ID: "region-ca1", Name: "Canada one", Known: true}
	domain := FailureDomainIdentity{ProviderID: "provider-test", RegionID: region.ID, ID: "fd-ca1", Name: "rack one", Known: true}
	cells := []RuntimeCellCapability{
		{CellID: "candle-cell", Engine: EngineCandle, ModelSHA256: fixtureDigest("d"), CapabilitySHA256: fixtureDigest("e"), CapabilityIDs: []string{"embedding"}, RegionID: region.ID, FailureDomainID: domain.ID, Available: true},
		{CellID: "vllm-cell", Engine: EngineVLLM, ModelSHA256: fixtureDigest("f"), CapabilitySHA256: fixtureDigest("0"), CapabilityIDs: []string{"embedding"}, RegionID: region.ID, FailureDomainID: domain.ID, Available: true},
	}
	provider := ProviderAdapterSnapshot{
		Header:         fixtureHeader(KindProviderSnapshot, "provider-snapshot-v1"),
		ProviderID:     "provider-test",
		State:          ProviderRoutable,
		Regions:        []RegionIdentity{region},
		FailureDomains: []FailureDomainIdentity{domain},
		RuntimeCells:   cells,
		Evidence:       []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &provider)
	if err := provider.Validate(); err != nil {
		t.Fatalf("fixture provider: %v", err)
	}
	return provider, region, domain, cells
}

func fixtureContracts(t *testing.T) (WorkloadIR, CapabilityIR, ProviderAdapterSnapshot, PlacementDecision, RuntimeDecision) {
	t.Helper()
	provider, region, domain, cells := fixtureProvider(t)
	evidence := fixtureEvidence(t)
	workload := WorkloadIR{
		Header:               fixtureHeader(KindWorkloadIR, "workload-v1"),
		WorkloadClass:        "embed",
		InputContractSHA256:  fixtureDigest("1"),
		RequiredCapabilities: []string{"embedding"},
		RequiredRegionID:     region.ID,
	}
	mustSeal(t, &workload)
	capability := CapabilityIR{
		Header:        fixtureHeader(KindCapabilityIR, "capability-v1"),
		Provider:      Ref(provider.Header),
		ProviderID:    provider.ProviderID,
		Region:        region,
		FailureDomain: domain,
		RuntimeCells:  cells,
		Evidence:      []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &capability)
	placement := PlacementDecision{
		Header:         fixtureHeader(KindPlacementDecision, "placement-v1"),
		Workload:       Ref(workload.Header),
		Capability:     Ref(capability.Header),
		Provider:       Ref(provider.Header),
		SelectedCellID: "candle-cell",
		Region:         region,
		FailureDomain:  domain,
		Mode:           "POOL",
		Evidence:       []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &placement)
	runtime := RuntimeDecision{
		Header:         fixtureHeader(KindRuntimeDecision, "runtime-v1"),
		Workload:       Ref(workload.Header),
		Capability:     Ref(capability.Header),
		SelectedCellID: "candle-cell",
		Engine:         EngineCandle,
		Reason:         "bound benchmark is compatible with this cell",
		Evidence:       []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &runtime)
	if err := placement.ValidateAgainst(workload, capability, provider); err != nil {
		t.Fatalf("fixture placement: %v", err)
	}
	if err := runtime.ValidateAgainst(workload, capability, provider); err != nil {
		t.Fatalf("fixture runtime: %v", err)
	}
	return workload, capability, provider, placement, runtime
}

func TestCanonicalHeadersRejectUnknownSchemaAndMismatchedDigest(t *testing.T) {
	workload, _, _, _, _ := fixtureContracts(t)
	unknown := workload
	unknown.Header.SchemaVersion = 2
	mustSeal(t, &unknown)
	if err := unknown.Validate(); err == nil {
		t.Fatal("unknown schema was accepted")
	}
	tampered := workload
	tampered.Header.CanonicalSHA256 = fixtureDigest("9")
	if err := tampered.Validate(); err == nil {
		t.Fatal("mismatched canonical digest was accepted")
	}
}

func TestRuntimeSelectionRequiresBoundEvidenceAndKnownCapabilityCell(t *testing.T) {
	workload, capability, provider, _, runtime := fixtureContracts(t)
	superseded := runtime
	superseded.Evidence[0].BindingStatus = BindingSuperseded
	mustSeal(t, &superseded)
	if err := superseded.ValidateAgainst(workload, capability, provider); err == nil {
		t.Fatal("SUPERSEDED evidence selected a runtime")
	}
	unbound := runtime
	unbound.Evidence[0].BindingStatus = BindingUnbound
	mustSeal(t, &unbound)
	if err := unbound.ValidateAgainst(workload, capability, provider); err == nil {
		t.Fatal("UNBOUND evidence selected a runtime")
	}
	missingCell := runtime
	missingCell.SelectedCellID = "nonexistent-cell"
	mustSeal(t, &missingCell)
	if err := missingCell.ValidateAgainst(workload, capability, provider); err == nil {
		t.Fatal("runtime selected a cell outside CapabilityIR")
	}
}

func TestCapabilityCannotInventOrAlterProviderCell(t *testing.T) {
	_, capability, provider, _, _ := fixtureContracts(t)
	invented := capability
	invented.RuntimeCells[0].CellID = "fake-cell"
	mustSeal(t, &invented)
	if err := invented.ValidateAgainstProvider(provider); err == nil {
		t.Fatal("capability invented a provider cell")
	}
	altered := capability
	altered.RuntimeCells[0].Available = false
	mustSeal(t, &altered)
	if err := altered.ValidateAgainstProvider(provider); err == nil {
		t.Fatal("capability changed provider cell availability")
	}
}

func TestSelectionRequiresCellCapabilitiesAtItsDeclaredLocality(t *testing.T) {
	workload, capability, provider, placement, runtime := fixtureContracts(t)
	missingCapability := workload
	missingCapability.RequiredCapabilities = []string{"missing-capability"}
	mustSeal(t, &missingCapability)
	if err := placement.ValidateAgainst(missingCapability, capability, provider); err == nil {
		t.Fatal("placement selected a cell without the workload capability")
	}
	if err := runtime.ValidateAgainst(missingCapability, capability, provider); err == nil {
		t.Fatal("runtime selected a cell without the workload capability")
	}
	wrongLocality := capability
	wrongLocality.RuntimeCells[0].RegionID = "other-region"
	mustSeal(t, &wrongLocality)
	if err := wrongLocality.ValidateAgainstProvider(provider); err == nil {
		t.Fatal("capability accepted a cell outside its declared locality")
	}
}

func TestCanonicalTransportRejectsTimezoneAndUnorderedEvidence(t *testing.T) {
	workload, _, provider, _, _ := fixtureContracts(t)
	nonUTC := workload
	nonUTC.Header.CreatedAt = fixtureTime.In(time.FixedZone("EDT", -4*60*60))
	mustSeal(t, &nonUTC)
	if err := nonUTC.Validate(); err == nil {
		t.Fatal("non-UTC timestamp was accepted into canonical transport")
	}

	unordered := provider
	newer := fixtureEvidenceWithID(t, "evidence-v2")
	older := fixtureEvidenceWithID(t, "evidence-v1")
	unordered.Evidence = []EvidenceEnvelope{newer, older}
	mustSeal(t, &unordered)
	if err := unordered.Validate(); err == nil {
		t.Fatal("unordered evidence was accepted into canonical transport")
	}
}

func TestEvidenceLifecycleIsAppendOnlyAndBoundToAnArtifact(t *testing.T) {
	old := fixtureEvidenceWithID(t, "evidence-old")
	next := fixtureEvidenceWithID(t, "evidence-next")
	next.Supersedes = []EvidenceSupersession{{
		Target: Ref(old.Header), Reason: "replacement receipt has a stronger harness binding",
	}}
	mustSeal(t, &next)
	if err := next.Validate(); err != nil {
		t.Fatalf("append-only evidence supersession: %v", err)
	}
	missingArtifact := old
	missingArtifact.BindingArtifact = ContractRef{}
	mustSeal(t, &missingArtifact)
	if err := missingArtifact.Validate(); err == nil {
		t.Fatal("evidence without a binding artifact was accepted")
	}
	self := old
	self.Supersedes = []EvidenceSupersession{{Target: Ref(old.Header), Reason: "not allowed"}}
	mustSeal(t, &self)
	if err := self.Validate(); err == nil {
		t.Fatal("evidence superseded itself")
	}
}

func TestResolverAwareSelectionRefusesSupersededBinding(t *testing.T) {
	workload, capability, provider, placement, runtime := fixtureContracts(t)
	artifact := fixtureBindingArtifact(t, "evidence-v1", BindingBound)
	resolver := fixtureBindingResolver{
		artifact.Header.CanonicalSHA256: {Artifact: artifact, Current: true},
	}
	if err := placement.ValidateAgainstResolved(workload, capability, provider, resolver); err != nil {
		t.Fatalf("current binding selected placement: %v", err)
	}
	if err := runtime.ValidateAgainstResolved(workload, capability, provider, resolver); err != nil {
		t.Fatalf("current binding selected runtime: %v", err)
	}
	resolver[artifact.Header.CanonicalSHA256] = EvidenceBindingResolution{Artifact: artifact, Current: false}
	if err := placement.ValidateAgainstResolved(workload, capability, provider, resolver); err == nil {
		t.Fatal("superseded binding selected placement")
	}
	if err := runtime.ValidateAgainstResolved(workload, capability, provider, resolver); err == nil {
		t.Fatal("superseded binding selected runtime")
	}
}

func TestPlacementRefusesUnknownOrMismatchedLocality(t *testing.T) {
	workload, capability, provider, placement, _ := fixtureContracts(t)
	unknown := placement
	unknown.Region.Known = false
	mustSeal(t, &unknown)
	if err := unknown.ValidateAgainst(workload, capability, provider); err == nil {
		t.Fatal("unknown region gained locality")
	}
	mismatched := placement
	mismatched.FailureDomain.RegionID = "other-region"
	mustSeal(t, &mismatched)
	if err := mismatched.ValidateAgainst(workload, capability, provider); err == nil {
		t.Fatal("mismatched failure domain gained locality")
	}
}

func TestDefaultProviderIsNotRoutable(t *testing.T) {
	provider, err := NewUnconfiguredProvider("unconfigured-provider", "provider-test", "protocol-v1-test", fixtureTime)
	if err != nil {
		t.Fatalf("unconfigured provider: %v", err)
	}
	if provider.State != ProviderNotRoutable || provider.Validate() != nil {
		t.Fatal("unconfigured provider is not a valid fail-closed default")
	}
	_, capability, _, _, _ := fixtureContracts(t)
	capability.Provider = Ref(provider.Header)
	mustSeal(t, &capability)
	if err := capability.ValidateAgainstProvider(provider); err == nil {
		t.Fatal("NOT_ROUTABLE provider supplied a capability")
	}
}

func TestFutureEngineRemainsACellAttribute(t *testing.T) {
	cell := RuntimeCellCapability{
		CellID:           "future-cell",
		Engine:           RuntimeEngine("future_engine_v1"),
		ModelSHA256:      fixtureDigest("4"),
		CapabilitySHA256: fixtureDigest("5"),
		CapabilityIDs:    []string{"future_capability"},
		RegionID:         "region-ca1",
		FailureDomainID:  "fd-ca1",
		Available:        false,
	}
	if err := cell.Validate(); err != nil {
		t.Fatalf("future engine must remain a valid competing cell: %v", err)
	}
}

func TestRemainingCanonicalContractsAreRecordOnlyAndVersioned(t *testing.T) {
	workload, capability, provider, placement, runtime := fixtureContracts(t)
	evidence := fixtureEvidence(t)
	pricing := PricingDecision{
		Header: fixtureHeader(KindPricingDecision, "pricing-v1"), Workload: Ref(workload.Header),
		FrozenPricingSHA256: fixtureDigest("2"), Currency: "CAD", Evidence: []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &pricing)
	if err := pricing.ValidateAgainst(workload); err != nil {
		t.Fatalf("pricing decision: %v", err)
	}
	topology := TopologyDecision{
		Header: fixtureHeader(KindTopologyDecision, "topology-v1"), Placement: Ref(placement.Header),
		Topology: "INDEPENDENT_REPLICAS", Region: placement.Region, FailureDomain: placement.FailureDomain,
		Evidence: []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &topology)
	if err := topology.ValidateAgainst(placement, workload, capability, provider); err != nil {
		t.Fatalf("topology decision: %v", err)
	}
	lease := ServiceLease{
		Header: fixtureHeader(KindServiceLease, "service-lease-v1"), Placement: Ref(placement.Header),
		Runtime: Ref(runtime.Header), Topology: Ref(topology.Header), Pricing: Ref(pricing.Header), LegacyLeaseSHA256: fixtureDigest("3"),
		Provider: Ref(provider.Header), Region: placement.Region, FailureDomain: placement.FailureDomain,
		State: LeasePending, ExpiresAt: fixtureTime.Add(time.Hour), FencingToken: 1, CapacityUnits: 1,
		Evidence: []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &lease)
	if err := lease.ValidateAgainst(placement, runtime, topology, pricing, workload, capability, provider); err != nil {
		t.Fatalf("service lease: %v", err)
	}
	artifact := fixtureBindingArtifact(t, "evidence-v1", BindingBound)
	resolver := fixtureBindingResolver{artifact.Header.CanonicalSHA256: {Artifact: artifact, Current: true}}
	if err := lease.ValidateAgainstResolved(placement, runtime, topology, pricing, workload, capability, provider, resolver); err != nil {
		t.Fatalf("resolver-aware service lease: %v", err)
	}
	resolver[artifact.Header.CanonicalSHA256] = EvidenceBindingResolution{Artifact: artifact, Current: false}
	if err := lease.ValidateAgainstResolved(placement, runtime, topology, pricing, workload, capability, provider, resolver); err == nil {
		t.Fatal("stale evidence validated a service lease")
	}
	shadow := ShadowReplay{
		Header: fixtureHeader(KindShadowReplay, "shadow-v1"), InputDecision: Ref(runtime.Header),
		Status: ShadowNotExecuted, Reason: "comparison only", Evidence: []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &shadow)
	if err := shadow.Validate(); err != nil {
		t.Fatalf("record-only shadow replay: %v", err)
	}
	shadow.MoneyAuthorityInvoked = true
	mustSeal(t, &shadow)
	if err := shadow.Validate(); err == nil {
		t.Fatal("shadow replay invoked money authority")
	}
}

func TestLocalityAndSplitThresholdAreAppendOnlyAndReviewOnly(t *testing.T) {
	_, _, provider, placement, _ := fixtureContracts(t)
	evidence := fixtureEvidence(t)
	first := LocalityEvent{
		Header: fixtureHeader(KindLocalityEvent, "locality-v1"), EventSequence: 1, EventType: "OBSERVED",
		Provider: Ref(provider.Header), Region: placement.Region, FailureDomain: placement.FailureDomain,
		Evidence: []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &first)
	if err := first.ValidateAgainstProvider(provider); err != nil {
		t.Fatalf("locality event provider binding: %v", err)
	}
	second := LocalityEvent{
		Header: fixtureHeader(KindLocalityEvent, "locality-v2"), EventSequence: 2,
		PreviousSHA256: first.Header.CanonicalSHA256, EventType: "REVALIDATED",
		Provider: Ref(provider.Header), Region: placement.Region, FailureDomain: placement.FailureDomain,
		Evidence: []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &second)
	if err := second.ValidateAfter(first); err != nil {
		t.Fatalf("append-only locality chain: %v", err)
	}
	threshold := SplitThreshold{
		Header: fixtureHeader(KindSplitThreshold, "split-threshold-v1"), Metric: "scheduler_lock_p99_ms",
		Comparator: "GT", Threshold: 10, ObservedValue: 11, WindowStart: fixtureTime,
		WindowEnd: fixtureTime.Add(time.Hour), Evidence: []EvidenceEnvelope{evidence},
	}
	mustSeal(t, &threshold)
	result, err := threshold.Evaluate()
	if err != nil || result != SplitReviewRequired {
		t.Fatalf("split threshold result = %q, %v", result, err)
	}
	threshold.Evidence[0].BindingStatus = BindingUnbound
	mustSeal(t, &threshold)
	if _, err := threshold.Evaluate(); err == nil {
		t.Fatal("unbound threshold evidence requested a split review")
	}
}
