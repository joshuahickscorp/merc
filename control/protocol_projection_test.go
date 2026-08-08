package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"merc/control/protocol"
)

func protocolProjectionHeader(kind, id string) protocol.ContractHeader {
	return protocol.ContractHeader{
		SchemaVersion:  protocol.SchemaVersionV1,
		Kind:           kind,
		ID:             id,
		PolicyRevision: "projection-test-v1",
		CreatedAt:      time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC),
	}
}

func protocolProjectionDigest(seed string) string { return strings.Repeat(seed, 64) }

func protocolProjectionEvidence(t *testing.T) protocol.EvidenceEnvelope {
	t.Helper()
	value := protocol.EvidenceEnvelope{
		Header:        protocolProjectionHeader(protocol.KindEvidenceEnvelope, "projection-evidence-v1"),
		BindingStatus: protocol.BindingBound,
		BindingArtifact: protocol.ContractRef{
			Kind:            protocol.KindEvidenceBinding,
			ID:              "projection-binding-v1",
			CanonicalSHA256: protocolProjectionDigest("a"),
		},
		SourceCommit:  strings.Repeat("b", 40),
		ProducerID:    "projection-test",
		PayloadSHA256: protocolProjectionDigest("c"),
		HarnessSHA256: protocolProjectionDigest("d"),
	}
	if err := value.Seal(); err != nil {
		t.Fatalf("seal evidence: %v", err)
	}
	return value
}

func protocolProjectionBindingArtifact(t *testing.T) protocol.EvidenceBindingArtifact {
	t.Helper()
	value := protocol.EvidenceBindingArtifact{
		Header:        protocolProjectionHeader(protocol.KindEvidenceBinding, "projection-binding-artifact-v1"),
		EvidenceID:    "projection-evidence-v1",
		BindingStatus: protocol.BindingBound,
		SourceCommit:  strings.Repeat("b", 40),
		PayloadSHA256: protocolProjectionDigest("c"),
		HarnessSHA256: protocolProjectionDigest("d"),
	}
	if err := value.Seal(); err != nil {
		t.Fatalf("seal binding artifact: %v", err)
	}
	return value
}

func TestProtocolProjectionsWrapFrozenMonolithAuthorities(t *testing.T) {
	workloadDecision, compute, _, _, originPricing := distributedPricingFixture(t)
	workload, err := ProtocolWorkloadFromDecisionV1(
		protocolProjectionHeader("ignored", "projection-workload-v1"),
		workloadDecision,
	)
	if err != nil {
		t.Fatalf("workload projection: %v", err)
	}
	if got, want := workload.RequiredCapabilities, []string{workloadDecision.RuntimeJobType}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("workload capabilities = %v, want frozen runtime capability %v", got, want)
	}
	if want := "workload:" + workloadDecision.BindingSHA256; workload.Header.ID != want {
		t.Fatalf("workload projection id = %q, want %q", workload.Header.ID, want)
	}

	evidence := protocolProjectionEvidence(t)
	originDigest, err := pricingDecisionDigest(originPricing)
	if err != nil {
		t.Fatalf("origin pricing digest: %v", err)
	}
	reuseCompute, err := newExactReuseComputePlan(
		workloadDecision, compute.InputRecords, compute.InputBytes,
		testInputDepthProfile(compute.InputRecords), 0.01, &compute,
	)
	if err != nil {
		t.Fatalf("exact reuse compute: %v", err)
	}
	reusePricing, err := newExactReusePricingDecision(
		workloadDecision, reuseCompute, originPricing.Catalogue,
		workloadDecision.Binding.Tier, 0.01, originDigest,
	)
	if err != nil {
		t.Fatalf("exact reuse pricing: %v", err)
	}
	verifiedPricing, err := VerifyExactReusePricingDecisionForProtocol(reusePricing, workloadDecision, reuseCompute)
	if err != nil {
		t.Fatalf("pricing verification: %v", err)
	}
	pricing, err := ProtocolPricingFromVerifiedDecisionV1(
		protocolProjectionHeader("ignored", "projection-pricing-v1"),
		workload, verifiedPricing, []protocol.EvidenceEnvelope{evidence},
	)
	if err != nil {
		t.Fatalf("pricing projection: %v", err)
	}
	canonicalPricing := pricing.Decision()
	if want := "pricing:" + canonicalPricing.FrozenPricingSHA256; canonicalPricing.Header.ID != want {
		t.Fatalf("pricing projection id = %q, want %q", canonicalPricing.Header.ID, want)
	}
	if err := canonicalPricing.ValidateAgainst(workload); err != nil {
		t.Fatalf("pricing projection binding: %v", err)
	}
	wrongWorkload := workload
	wrongWorkload.InputContractSHA256 = protocolProjectionDigest("9")
	if err := wrongWorkload.Seal(); err != nil {
		t.Fatalf("seal mismatched workload: %v", err)
	}
	if _, err := ProtocolPricingFromVerifiedDecisionV1(
		protocolProjectionHeader("ignored", "projection-pricing-wrong-workload-v1"),
		wrongWorkload, verifiedPricing, []protocol.EvidenceEnvelope{evidence},
	); err == nil {
		t.Fatal("verified pricing projected against a different workload")
	}

	providerRef := protocol.ContractRef{Kind: protocol.KindProviderSnapshot, ID: "projection-provider-v1", CanonicalSHA256: protocolProjectionDigest("2")}
	capabilityRef := protocol.ContractRef{Kind: protocol.KindCapabilityIR, ID: "projection-capability-v1", CanonicalSHA256: protocolProjectionDigest("3")}
	region := protocol.RegionIdentity{ProviderID: "provider-test", ID: "region-ca1", Name: "Canada one", Known: true}
	domain := protocol.FailureDomainIdentity{ProviderID: "provider-test", RegionID: region.ID, ID: "fd-ca1", Name: "rack one", Known: true}
	placement, err := ProtocolPlacementFromDecisionV1(
		protocolProjectionHeader("ignored", "projection-placement-v1"),
		protocol.Ref(workload.Header), capabilityRef, providerRef,
		PlacementDecision{Mode: ModePool, Reason: "existing admission chose independent work"},
		"candle-cell", region, domain, []protocol.EvidenceEnvelope{evidence},
	)
	if err != nil {
		t.Fatalf("placement projection: %v", err)
	}
	if err := placement.Validate(); err != nil {
		t.Fatalf("placement projection validation: %v", err)
	}
	if placement.Header.ID == "projection-placement-v1" {
		t.Fatal("placement projection retained caller-minted identity")
	}
	placementAgain, err := ProtocolPlacementFromDecisionV1(
		protocolProjectionHeader("ignored", "projection-placement-other-v1"),
		protocol.Ref(workload.Header), capabilityRef, providerRef,
		PlacementDecision{Mode: ModePool, Reason: "existing admission chose independent work"},
		"candle-cell", region, domain, []protocol.EvidenceEnvelope{evidence},
	)
	if err != nil || placementAgain.Header.ID != placement.Header.ID {
		t.Fatalf("placement projection identity is not stable: %+v, %v", placementAgain.Header, err)
	}
	legacyLease := ServiceLease{
		ID: uuid.New(), Region: region.ID, MaximumReplicas: 2, State: "ACTIVE",
		ExpiresAt: time.Date(2026, time.August, 8, 13, 0, 0, 0, time.UTC),
		Pricing:   reusePricing, PricingDecisionSHA256: canonicalPricing.FrozenPricingSHA256,
	}
	runtimeRef := protocol.ContractRef{Kind: protocol.KindRuntimeDecision, ID: "projection-runtime-v1", CanonicalSHA256: protocolProjectionDigest("4")}
	topologyRef := protocol.ContractRef{Kind: protocol.KindTopologyDecision, ID: "projection-topology-v1", CanonicalSHA256: protocolProjectionDigest("5")}
	lease, err := ProtocolServiceLeaseFromLegacyV1(
		protocolProjectionHeader("ignored", "projection-lease-v1"), legacyLease, protocol.Ref(placement.Header),
		runtimeRef, topologyRef, pricing, providerRef, region, domain, 1, 1,
		[]protocol.EvidenceEnvelope{evidence},
	)
	if err != nil || lease.State != protocol.LeaseActive {
		t.Fatalf("service lease projection = %+v, %v", lease, err)
	}
	if want := "service-lease:" + legacyLease.ID.String(); lease.Header.ID != want {
		t.Fatalf("lease projection id = %q, want %q", lease.Header.ID, want)
	}
	wrongRegion := legacyLease
	wrongRegion.Region = "not-ca1"
	if _, err := ProtocolServiceLeaseFromLegacyV1(
		protocolProjectionHeader("ignored", "projection-lease-wrong-region-v1"), wrongRegion, protocol.Ref(placement.Header),
		runtimeRef, topologyRef, pricing, providerRef, region, domain, 1, 1,
		[]protocol.EvidenceEnvelope{evidence},
	); err == nil {
		t.Fatal("service lease projection accepted unrelated legacy region")
	}
	mismatchedPricing := legacyLease
	mismatchedPricing.PricingDecisionSHA256 = protocolProjectionDigest("f")
	if _, err := ProtocolServiceLeaseFromLegacyV1(
		protocolProjectionHeader("ignored", "projection-lease-wrong-price-v1"), mismatchedPricing, protocol.Ref(placement.Header),
		runtimeRef, topologyRef, pricing, providerRef, region, domain, 1, 1,
		[]protocol.EvidenceEnvelope{evidence},
	); err == nil {
		t.Fatal("service lease projection accepted unrelated canonical pricing")
	}
}

func TestProtocolEvidenceBindingResolverIsImmutableSnapshot(t *testing.T) {
	artifact := protocolProjectionBindingArtifact(t)
	resolver, err := NewProtocolEvidenceBindingResolver([]ProtocolEvidenceBindingSnapshot{{Artifact: artifact, Current: true}})
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	got, err := resolver.ResolveEvidenceBinding(protocol.Ref(artifact.Header))
	if err != nil || !got.Current || got.Artifact.Header.CanonicalSHA256 != artifact.Header.CanonicalSHA256 {
		t.Fatalf("resolver result = %+v, %v", got, err)
	}
	if _, err := NewProtocolEvidenceBindingResolver([]ProtocolEvidenceBindingSnapshot{{Artifact: artifact, Current: true}, {Artifact: artifact, Current: false}}); err == nil {
		t.Fatal("duplicate binding artifact entered immutable resolver")
	}
}

func TestProtocolProjectionsRefuseUnverifiedLegacyAuthority(t *testing.T) {
	if _, err := ProtocolWorkloadFromDecisionV1(
		protocolProjectionHeader("ignored", "projection-invalid-workload-v1"),
		WorkloadDecision{Version: workloadDecisionVersion},
	); err == nil {
		t.Fatal("invalid frozen workload decision was projected")
	}
	if _, err := ProtocolPricingFromVerifiedDecisionV1(
		protocolProjectionHeader("ignored", "projection-invalid-pricing-v1"),
		protocol.WorkloadIR{},
		VerifiedPricingDecisionForProtocol{}, []protocol.EvidenceEnvelope{protocolProjectionEvidence(t)},
	); err == nil {
		t.Fatal("unverified pricing wrapper was projected")
	}
}
