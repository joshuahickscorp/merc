package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"merc/control/protocol"
)

// The functions in this file are one-way, additive projections from the
// monolith's existing frozen authorities into protocol V1. They deliberately
// do not replace database rows, scheduler choices, runtime profiles, or money
// authorities. Callers may mint a protocol record for review/shadow use, then
// validate it against the supplied snapshots before ever considering it.

func protocolV1Header(header protocol.ContractHeader, kind string) protocol.ContractHeader {
	header.SchemaVersion = protocol.SchemaVersionV1
	header.Kind = kind
	header.CanonicalSHA256 = ""
	return header
}

func sortedProtocolCapabilities(values []string) []string {
	copyValues := append([]string(nil), values...)
	sort.Strings(copyValues)
	return copyValues
}

// ProtocolWorkloadFromDecisionV1 projects the already-frozen request binding;
// it never reclassifies a buyer request or chooses a runtime cell.
func ProtocolWorkloadFromDecisionV1(
	header protocol.ContractHeader,
	decision WorkloadDecision,
) (protocol.WorkloadIR, error) {
	if err := ValidateFrozenWorkloadDecisionSnapshot(decision); err != nil {
		return protocol.WorkloadIR{}, fmt.Errorf("cannot project unverified frozen workload decision: %w", err)
	}
	// RuntimeJobType is the frozen server-classified capability requirement.
	// The existing WorkloadDecision does not prove a provider region/failure
	// domain, so this review projection refuses to invent either.
	workloadHeader := protocolV1Header(header, protocol.KindWorkloadIR)
	workloadHeader.ID = "workload:" + decision.BindingSHA256
	value := protocol.WorkloadIR{
		Header:               workloadHeader,
		WorkloadClass:        decision.WorkloadClass,
		InputContractSHA256:  decision.BindingSHA256,
		RequiredCapabilities: sortedProtocolCapabilities([]string{decision.RuntimeJobType}),
	}
	if err := value.Seal(); err != nil {
		return protocol.WorkloadIR{}, err
	}
	return value, value.Validate()
}

// VerifiedPricingDecisionForProtocol is opaque outside this package. It can be
// created only by one of the execution-mode-specific constructors below; a raw
// PricingDecision is not evidence of authority.
type VerifiedPricingDecisionForProtocol struct {
	decision            PricingDecision
	digest              string
	workloadInputSHA256 string
}

// VerifiedCanonicalPricingDecisionForProtocol prevents a caller from passing
// an arbitrary protocol DTO to a lease projection. It is constructed only from
// a verified legacy pricing snapshot below.
type VerifiedCanonicalPricingDecisionForProtocol struct {
	value protocol.PricingDecision
}

func (v VerifiedCanonicalPricingDecisionForProtocol) Decision() protocol.PricingDecision {
	return v.value
}

func (v VerifiedCanonicalPricingDecisionForProtocol) Ref() protocol.ContractRef {
	return protocol.Ref(v.value.Header)
}

func verifiedPricingDecisionForProtocol(
	decision PricingDecision,
	workloadInputSHA256 string,
	err error,
) (VerifiedPricingDecisionForProtocol, error) {
	if err != nil {
		return VerifiedPricingDecisionForProtocol{}, fmt.Errorf("cannot project unverified pricing decision: %w", err)
	}
	if !validSHA256(workloadInputSHA256) {
		return VerifiedPricingDecisionForProtocol{}, errors.New("verified pricing decision lacks a workload input commitment")
	}
	frozen, err := pricingDecisionDigest(decision)
	if err != nil {
		return VerifiedPricingDecisionForProtocol{}, err
	}
	return VerifiedPricingDecisionForProtocol{
		decision: decision, digest: frozen, workloadInputSHA256: workloadInputSHA256,
	}, nil
}

// VerifyDistributedPricingDecisionForProtocol validates the exact distributed
// pricing snapshot before it can cross the protocol boundary.
func VerifyStoreAnchoredDistributedPricingDecisionForProtocol(
	ctx context.Context,
	store *Store,
	decision PricingDecision,
	workload WorkloadDecision,
	compute ComputePlan,
	placement PlacementRequirement,
	economic EconomicPlan,
) (VerifiedPricingDecisionForProtocol, error) {
	return verifiedPricingDecisionForProtocol(
		decision,
		workload.BindingSHA256,
		ValidateDistributedPricingDecisionSnapshotWithStore(ctx, store, decision, workload, compute, placement, economic),
	)
}

func VerifyExactReusePricingDecisionForProtocol(
	decision PricingDecision,
	workload WorkloadDecision,
	compute ComputePlan,
) (VerifiedPricingDecisionForProtocol, error) {
	return verifiedPricingDecisionForProtocol(
		decision,
		workload.BindingSHA256,
		ValidateExactReusePricingDecisionSnapshot(decision, workload, compute),
	)
}

func VerifyRealtimePricingDecisionForProtocol(
	decision PricingDecision,
	input RealtimePricingInputs,
	workloadInputSHA256 string,
) (VerifiedPricingDecisionForProtocol, error) {
	return verifiedPricingDecisionForProtocol(decision, workloadInputSHA256, ValidateRealtimePricingDecisionSnapshot(decision, input))
}

func VerifyRealtimeReusePricingDecisionForProtocol(
	decision PricingDecision,
	input RealtimeReusePricingInputs,
	workloadInputSHA256 string,
) (VerifiedPricingDecisionForProtocol, error) {
	return verifiedPricingDecisionForProtocol(decision, workloadInputSHA256, ValidateRealtimeReusePricingDecisionSnapshot(decision, input))
}

func VerifyServiceLeasePricingDecisionForProtocol(
	decision PricingDecision,
	input ServiceLeasePricingInputs,
	workloadInputSHA256 string,
) (VerifiedPricingDecisionForProtocol, error) {
	return verifiedPricingDecisionForProtocol(decision, workloadInputSHA256, ValidateServiceLeasePricingDecisionSnapshot(decision, input))
}

// ProtocolPricingFromVerifiedDecisionV1 projects the precise verified legacy
// decision digest. It does not recalculate a quote or change money authority.
func ProtocolPricingFromVerifiedDecisionV1(
	header protocol.ContractHeader,
	workload protocol.WorkloadIR,
	verified VerifiedPricingDecisionForProtocol,
	evidence []protocol.EvidenceEnvelope,
) (VerifiedCanonicalPricingDecisionForProtocol, error) {
	if err := workload.Validate(); err != nil {
		return VerifiedCanonicalPricingDecisionForProtocol{}, fmt.Errorf("cannot project pricing against invalid workload: %w", err)
	}
	if verified.workloadInputSHA256 != workload.InputContractSHA256 {
		return VerifiedCanonicalPricingDecisionForProtocol{}, errors.New("verified pricing belongs to a different workload input commitment")
	}
	pricingHeader := protocolV1Header(header, protocol.KindPricingDecision)
	pricingHeader.ID = "pricing:" + verified.digest
	value := protocol.PricingDecision{
		Header:              pricingHeader,
		Workload:            protocol.Ref(workload.Header),
		FrozenPricingSHA256: verified.digest,
		// Legacy pricing stores ISO currency lowercase; V1 transport freezes the
		// same three-letter code in canonical uppercase beside the raw digest.
		Currency: strings.ToUpper(verified.decision.Currency),
		Evidence: append([]protocol.EvidenceEnvelope(nil), evidence...),
	}
	if err := value.Seal(); err != nil {
		return VerifiedCanonicalPricingDecisionForProtocol{}, err
	}
	if err := value.Validate(); err != nil {
		return VerifiedCanonicalPricingDecisionForProtocol{}, err
	}
	return VerifiedCanonicalPricingDecisionForProtocol{value: value}, nil
}

// ProtocolPlacementFromDecisionV1 retains the existing placement explanation
// while requiring the caller to supply the separately verified cell and
// locality identities. This is intentionally a projection, not routing.
func ProtocolPlacementFromDecisionV1(
	header protocol.ContractHeader,
	workload protocol.ContractRef,
	capability protocol.ContractRef,
	provider protocol.ContractRef,
	decision PlacementDecision,
	selectedCellID string,
	region protocol.RegionIdentity,
	failureDomain protocol.FailureDomainIdentity,
	evidence []protocol.EvidenceEnvelope,
) (protocol.PlacementDecision, error) {
	if strings.TrimSpace(decision.Reason) == "" {
		return protocol.PlacementDecision{}, errors.New("cannot project placement without an existing reason")
	}
	identity, err := canonicalDigest("protocol placement projection identity", struct {
		Workload       protocol.ContractRef
		Capability     protocol.ContractRef
		Provider       protocol.ContractRef
		Decision       PlacementDecision
		SelectedCellID string
		Region         protocol.RegionIdentity
		FailureDomain  protocol.FailureDomainIdentity
	}{workload, capability, provider, decision, selectedCellID, region, failureDomain})
	if err != nil {
		return protocol.PlacementDecision{}, err
	}
	placementHeader := protocolV1Header(header, protocol.KindPlacementDecision)
	placementHeader.ID = "placement:" + identity
	value := protocol.PlacementDecision{
		Header:         placementHeader,
		Workload:       workload,
		Capability:     capability,
		Provider:       provider,
		SelectedCellID: selectedCellID,
		Region:         region,
		FailureDomain:  failureDomain,
		Mode:           string(decision.Mode),
		Evidence:       append([]protocol.EvidenceEnvelope(nil), evidence...),
	}
	if err := value.Seal(); err != nil {
		return protocol.PlacementDecision{}, err
	}
	return value, value.Validate()
}

func protocolServiceLeaseState(state string) (protocol.ServiceLeaseState, error) {
	value := protocol.ServiceLeaseState(state)
	switch value {
	case protocol.LeasePending, protocol.LeaseActive, protocol.LeaseReleased, protocol.LeaseExpired, protocol.LeaseRefused:
		return value, nil
	default:
		return "", errors.New("cannot project unknown legacy service lease state")
	}
}

// ProtocolServiceLeaseFromLegacyV1 wraps rather than rewrites the legacy row.
// Region/failure-domain, fencing, capacity, and decision references are
// explicit arguments because the legacy row's free-form region string cannot
// honestly establish any of them on its own.
func ProtocolServiceLeaseFromLegacyV1(
	header protocol.ContractHeader,
	lease ServiceLease,
	placement protocol.ContractRef,
	runtime protocol.ContractRef,
	topology protocol.ContractRef,
	pricing VerifiedCanonicalPricingDecisionForProtocol,
	provider protocol.ContractRef,
	region protocol.RegionIdentity,
	failureDomain protocol.FailureDomainIdentity,
	fencingToken uint64,
	capacityUnits uint64,
	evidence []protocol.EvidenceEnvelope,
) (protocol.ServiceLease, error) {
	if lease.ID == uuid.Nil {
		return protocol.ServiceLease{}, errors.New("cannot project service lease without immutable legacy id")
	}
	canonicalPricing := pricing.Decision()
	if err := canonicalPricing.Validate(); err != nil {
		return protocol.ServiceLease{}, fmt.Errorf("cannot project service lease with invalid canonical pricing: %w", err)
	}
	legacyPricingDigest, err := pricingDecisionDigest(lease.Pricing)
	if err != nil {
		return protocol.ServiceLease{}, fmt.Errorf("cannot digest legacy service lease pricing: %w", err)
	}
	if lease.PricingDecisionSHA256 == "" || lease.PricingDecisionSHA256 != legacyPricingDigest ||
		canonicalPricing.FrozenPricingSHA256 != lease.PricingDecisionSHA256 {
		return protocol.ServiceLease{}, errors.New("canonical pricing does not bind legacy service lease pricing decision")
	}
	if strings.TrimSpace(lease.Region) == "" ||
		(!strings.EqualFold(strings.TrimSpace(lease.Region), region.ID) &&
			!strings.EqualFold(strings.TrimSpace(lease.Region), region.Name)) {
		return protocol.ServiceLease{}, errors.New("legacy service lease region does not bind canonical region identity")
	}
	if lease.MaximumReplicas <= 0 || capacityUnits == 0 || capacityUnits > uint64(lease.MaximumReplicas) {
		return protocol.ServiceLease{}, errors.New("legacy service lease capacity does not bind requested protocol capacity")
	}
	state, err := protocolServiceLeaseState(lease.State)
	if err != nil {
		return protocol.ServiceLease{}, err
	}
	legacyDigest, err := canonicalDigest("legacy service lease projection", lease)
	if err != nil {
		return protocol.ServiceLease{}, err
	}
	// One legacy row has one protocol identity. Callers supply policy/time but
	// cannot mint a second arbitrary ID for the same lease.
	leaseHeader := protocolV1Header(header, protocol.KindServiceLease)
	leaseHeader.ID = "service-lease:" + lease.ID.String()
	value := protocol.ServiceLease{
		Header:            leaseHeader,
		Placement:         placement,
		Runtime:           runtime,
		Topology:          topology,
		Pricing:           pricing.Ref(),
		LegacyLeaseSHA256: legacyDigest,
		Provider:          provider,
		Region:            region,
		FailureDomain:     failureDomain,
		State:             state,
		ExpiresAt:         lease.ExpiresAt,
		FencingToken:      fencingToken,
		CapacityUnits:     capacityUnits,
		Evidence:          append([]protocol.EvidenceEnvelope(nil), evidence...),
	}
	if err := value.Seal(); err != nil {
		return protocol.ServiceLease{}, err
	}
	return value, value.Validate()
}
