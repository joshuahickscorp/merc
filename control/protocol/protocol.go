// Package protocol contains additive V1 protocol DTOs.  It is a leaf package:
// there is no scheduler, database, payment, provider client, or service here.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

const SchemaVersionV1 = 1

const (
	KindWorkloadIR        = "workload_ir"
	KindCapabilityIR      = "capability_ir"
	KindPricingDecision   = "pricing_decision"
	KindPlacementDecision = "placement_decision"
	KindRuntimeDecision   = "runtime_decision"
	KindTopologyDecision  = "topology_decision"
	KindEvidenceEnvelope  = "evidence_envelope"
	KindEvidenceBinding   = "evidence_binding_artifact"
	KindServiceLease      = "service_lease"
	KindProviderSnapshot  = "provider_adapter_snapshot"
	KindLocalityEvent     = "locality_event"
	KindShadowReplay      = "shadow_replay"
	KindSplitThreshold    = "split_threshold"
)

var (
	idPattern     = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._:-]{2,127}$")
	shaPattern    = regexp.MustCompile("^[0-9a-f]{64}$")
	commitPattern = regexp.MustCompile("^[0-9a-f]{40,64}$")
	enginePattern = regexp.MustCompile("^[a-z][a-z0-9._-]{1,63}$")
)

type BindingStatus string

const (
	BindingBound      BindingStatus = "BOUND"
	BindingUnbound    BindingStatus = "UNBOUND"
	BindingSuperseded BindingStatus = "SUPERSEDED"
	BindingWithdrawn  BindingStatus = "WITHDRAWN"
)

type ProviderState string

const (
	ProviderNotRoutable ProviderState = "NOT_ROUTABLE"
	ProviderRoutable    ProviderState = "ROUTABLE"
)

type RuntimeEngine string

const (
	EngineVLLM        RuntimeEngine = "vllm"
	EngineSGLang      RuntimeEngine = "sglang"
	EngineTensorRTLLM RuntimeEngine = "tensorrt_llm"
	EngineLlamaCPP    RuntimeEngine = "llama_cpp"
	EngineMLX         RuntimeEngine = "mlx"
	EngineCandle      RuntimeEngine = "candle"
)

type ShadowReplayStatus string

const ShadowNotExecuted ShadowReplayStatus = "NOT_EXECUTED"

type ServiceLeaseState string

const (
	LeasePending  ServiceLeaseState = "PENDING"
	LeaseActive   ServiceLeaseState = "ACTIVE"
	LeaseReleased ServiceLeaseState = "RELEASED"
	LeaseExpired  ServiceLeaseState = "EXPIRED"
	LeaseRefused  ServiceLeaseState = "REFUSED"
)

type SplitDisposition string

const (
	StayMonolith        SplitDisposition = "STAY_MONOLITH"
	SplitReviewRequired SplitDisposition = "SPLIT_REVIEW_REQUIRED"
)

// ContractHeader is included in every V1 contract. CanonicalSHA256 hashes the
// JSON body with this field blank. Types use structs and sorted arrays, never
// maps, so a V1 value has one canonical representation.
type ContractHeader struct {
	SchemaVersion   int       `json:"schema_version"`
	Kind            string    `json:"kind"`
	ID              string    `json:"id"`
	CanonicalSHA256 string    `json:"canonical_sha256"`
	PolicyRevision  string    `json:"policy_revision"`
	CreatedAt       time.Time `json:"created_at"`
}

type ContractRef struct {
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	CanonicalSHA256 string `json:"canonical_sha256"`
}

func Ref(header ContractHeader) ContractRef {
	return ContractRef{Kind: header.Kind, ID: header.ID, CanonicalSHA256: header.CanonicalSHA256}
}

func (r ContractRef) Validate(kind string) error {
	if kind != "" && r.Kind != kind {
		return fmt.Errorf("reference kind %q is not %q", r.Kind, kind)
	}
	if !idPattern.MatchString(r.ID) || !shaPattern.MatchString(r.CanonicalSHA256) {
		return errors.New("invalid contract reference")
	}
	return nil
}

type headerCarrier interface{ contractHeader() *ContractHeader }

func seal(value headerCarrier) error {
	header := value.contractHeader()
	old := header.CanonicalSHA256
	header.CanonicalSHA256 = ""
	body, err := json.Marshal(value)
	if err != nil {
		header.CanonicalSHA256 = old
		return err
	}
	sum := sha256.Sum256(body)
	header.CanonicalSHA256 = hex.EncodeToString(sum[:])
	return nil
}

func validateHeader(value headerCarrier, kind string) error {
	header := value.contractHeader()
	if header.SchemaVersion != SchemaVersionV1 || header.Kind != kind {
		return fmt.Errorf("expected %s schema version 1", kind)
	}
	if !idPattern.MatchString(header.ID) || !shaPattern.MatchString(header.CanonicalSHA256) {
		return fmt.Errorf("%s has invalid identity or digest", kind)
	}
	if strings.TrimSpace(header.PolicyRevision) == "" || header.CreatedAt.IsZero() {
		return fmt.Errorf("%s requires policy_revision and created_at", kind)
	}
	if err := canonicalTimestamp(header.CreatedAt, "created_at"); err != nil {
		return fmt.Errorf("%s %w", kind, err)
	}
	claimed := header.CanonicalSHA256
	header.CanonicalSHA256 = ""
	body, err := json.Marshal(value)
	header.CanonicalSHA256 = claimed
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if claimed != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%s canonical digest does not match body", kind)
	}
	return nil
}

func required(value, field string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}

func digest(value, field string) error {
	if !shaPattern.MatchString(value) {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", field)
	}
	return nil
}

// canonicalTimestamp keeps logically identical contracts from acquiring
// different digests through a time-zone or sub-millisecond representation.
// V1 records UTC at millisecond precision. This is a transport rule only; it
// does not constrain the precision used by the live scheduler or ledger.
func canonicalTimestamp(value time.Time, field string) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", field)
	}
	if value.Location() != time.UTC {
		return fmt.Errorf("%s must be UTC", field)
	}
	if value.Nanosecond()%int(time.Millisecond) != 0 {
		return fmt.Errorf("%s must use millisecond precision", field)
	}
	return nil
}

func sortedUnique(values []string, field string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s is required", field)
	}
	for index, value := range values {
		if err := required(value, field); err != nil {
			return err
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("%s must be strictly sorted and unique", field)
		}
	}
	return nil
}

func sortedEvidence(values []EvidenceEnvelope, field string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s is required", field)
	}
	for index, value := range values {
		if index > 0 && values[index-1].Header.ID >= value.Header.ID {
			return fmt.Errorf("%s must be strictly sorted and unique by evidence id", field)
		}
	}
	return nil
}

func sortedRegions(values []RegionIdentity) error {
	if len(values) == 0 {
		return errors.New("provider regions are required")
	}
	for index, value := range values {
		if err := value.Validate(); err != nil {
			return err
		}
		if index > 0 && values[index-1].ID >= value.ID {
			return errors.New("provider regions must be strictly sorted and unique")
		}
	}
	return nil
}

func sortedFailureDomains(values []FailureDomainIdentity) error {
	if len(values) == 0 {
		return errors.New("provider failure domains are required")
	}
	for index, value := range values {
		if err := value.Validate(); err != nil {
			return err
		}
		if index > 0 && values[index-1].ID >= value.ID {
			return errors.New("provider failure domains must be strictly sorted and unique")
		}
	}
	return nil
}

// RegionIdentity and FailureDomainIdentity turn locality into a verifiable
// fact. Unknown identities are never eligible for a locality benefit.
type RegionIdentity struct {
	ProviderID string `json:"provider_id"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Known      bool   `json:"known"`
}

func (r RegionIdentity) Validate() error {
	if !r.Known || !idPattern.MatchString(r.ID) {
		return errors.New("region is unknown or invalid")
	}
	if err := required(r.ProviderID, "region.provider_id"); err != nil {
		return err
	}
	return required(r.Name, "region.name")
}

type FailureDomainIdentity struct {
	ProviderID string `json:"provider_id"`
	RegionID   string `json:"region_id"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Known      bool   `json:"known"`
}

func (f FailureDomainIdentity) Validate() error {
	if !f.Known || !idPattern.MatchString(f.ID) || !idPattern.MatchString(f.RegionID) {
		return errors.New("failure domain is unknown or invalid")
	}
	if err := required(f.ProviderID, "failure_domain.provider_id"); err != nil {
		return err
	}
	return required(f.Name, "failure_domain.name")
}

func validateLocality(region RegionIdentity, domain FailureDomainIdentity) error {
	if err := region.Validate(); err != nil {
		return err
	}
	if err := domain.Validate(); err != nil {
		return err
	}
	if region.ProviderID != domain.ProviderID || region.ID != domain.RegionID {
		return errors.New("failure domain does not belong to region")
	}
	return nil
}

// EvidenceSupersession is an append-only forward link: the new envelope names
// the immutable envelope it replaces, rather than changing historic evidence.
type EvidenceSupersession struct {
	Target ContractRef `json:"target"`
	Reason string      `json:"reason"`
}

// EvidenceBindingArtifact is the immutable binding record an EvidenceEnvelope
// names. It intentionally contains only identity slots; a resolver supplies
// current/supersession state without rewriting either the payload or this
// historic binding record.
type EvidenceBindingArtifact struct {
	Header        ContractHeader `json:"header"`
	EvidenceID    string         `json:"evidence_id"`
	BindingStatus BindingStatus  `json:"binding_status"`
	SourceCommit  string         `json:"source_commit"`
	PayloadSHA256 string         `json:"payload_sha256"`
	HarnessSHA256 string         `json:"harness_sha256"`
}

func (a *EvidenceBindingArtifact) contractHeader() *ContractHeader { return &a.Header }
func (a *EvidenceBindingArtifact) Seal() error                     { return seal(a) }

func (a EvidenceBindingArtifact) Validate() error {
	if err := validateHeader(&a, KindEvidenceBinding); err != nil {
		return err
	}
	if !idPattern.MatchString(a.EvidenceID) || !commitPattern.MatchString(a.SourceCommit) {
		return errors.New("binding artifact has invalid evidence identity or source commit")
	}
	switch a.BindingStatus {
	case BindingBound, BindingUnbound, BindingSuperseded, BindingWithdrawn:
	default:
		return errors.New("binding artifact has invalid status")
	}
	if err := digest(a.PayloadSHA256, "binding_artifact.payload_sha256"); err != nil {
		return err
	}
	return digest(a.HarnessSHA256, "binding_artifact.harness_sha256")
}

// EvidenceBindingResolution is supplied by the monolith's append-only binding
// index. Current=false is the explicit, immutable answer for a historic
// artifact that a newer binding has superseded or withdrawn.
type EvidenceBindingResolution struct {
	Artifact EvidenceBindingArtifact
	Current  bool
}

// EvidenceBindingResolver is a read-only seam. It cannot write evidence,
// execute a workload, schedule, or move money.
type EvidenceBindingResolver interface {
	ResolveEvidenceBinding(ContractRef) (EvidenceBindingResolution, error)
}

// EvidenceEnvelope carries enough identity for a selector to reject evidence
// that is UNBOUND, SUPERSEDED, or WITHDRAWN without altering historic payloads.
// BindingArtifact names the immutable binding receipt/registry entry that
// attests this envelope's lifecycle state. Resolution is deliberately left to
// the monolith adapter; this leaf package never reads a registry or network.
type EvidenceEnvelope struct {
	Header          ContractHeader         `json:"header"`
	BindingStatus   BindingStatus          `json:"binding_status"`
	BindingArtifact ContractRef            `json:"binding_artifact"`
	SourceCommit    string                 `json:"source_commit"`
	ProducerID      string                 `json:"producer_id"`
	PayloadSHA256   string                 `json:"payload_sha256"`
	HarnessSHA256   string                 `json:"harness_sha256"`
	Supersedes      []EvidenceSupersession `json:"supersedes,omitempty"`
}

func (e *EvidenceEnvelope) contractHeader() *ContractHeader { return &e.Header }
func (e *EvidenceEnvelope) Seal() error                     { return seal(e) }

func (e EvidenceEnvelope) Validate() error {
	if err := validateHeader(&e, KindEvidenceEnvelope); err != nil {
		return err
	}
	switch e.BindingStatus {
	case BindingBound, BindingUnbound, BindingSuperseded, BindingWithdrawn:
	default:
		return errors.New("invalid binding status")
	}
	if err := e.BindingArtifact.Validate(KindEvidenceBinding); err != nil {
		return fmt.Errorf("invalid evidence binding artifact: %w", err)
	}
	if !commitPattern.MatchString(e.SourceCommit) {
		return errors.New("evidence source_commit must be an exact commit")
	}
	if err := required(e.ProducerID, "evidence.producer_id"); err != nil {
		return err
	}
	if err := digest(e.PayloadSHA256, "evidence.payload_sha256"); err != nil {
		return err
	}
	if err := digest(e.HarnessSHA256, "evidence.harness_sha256"); err != nil {
		return err
	}
	for index, item := range e.Supersedes {
		if err := item.Target.Validate(KindEvidenceEnvelope); err != nil {
			return fmt.Errorf("invalid evidence supersession target: %w", err)
		}
		if err := required(item.Reason, "evidence.supersedes.reason"); err != nil {
			return err
		}
		if item.Target.Kind == e.Header.Kind && item.Target.ID == e.Header.ID {
			return errors.New("evidence cannot supersede itself")
		}
		if index > 0 && e.Supersedes[index-1].Target.ID >= item.Target.ID {
			return errors.New("evidence supersedes must be strictly sorted and unique by target id")
		}
	}
	return nil
}

func (e EvidenceEnvelope) ValidateAgainstBindingResolver(resolver EvidenceBindingResolver) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if resolver == nil {
		return errors.New("evidence binding resolver is required")
	}
	resolution, err := resolver.ResolveEvidenceBinding(e.BindingArtifact)
	if err != nil {
		return fmt.Errorf("resolve evidence binding artifact: %w", err)
	}
	if err := resolution.Artifact.Validate(); err != nil {
		return err
	}
	if Ref(resolution.Artifact.Header) != e.BindingArtifact {
		return errors.New("resolver returned a different binding artifact")
	}
	if !resolution.Current || resolution.Artifact.BindingStatus != BindingBound ||
		e.BindingStatus != BindingBound {
		return errors.New("evidence binding is not current and BOUND")
	}
	if resolution.Artifact.EvidenceID != e.Header.ID ||
		resolution.Artifact.SourceCommit != e.SourceCommit ||
		resolution.Artifact.PayloadSHA256 != e.PayloadSHA256 ||
		resolution.Artifact.HarnessSHA256 != e.HarnessSHA256 {
		return errors.New("binding artifact does not match evidence identity")
	}
	return nil
}

func boundEvidence(evidence []EvidenceEnvelope) error {
	if err := sortedEvidence(evidence, "evidence"); err != nil {
		return err
	}
	for _, item := range evidence {
		if err := item.Validate(); err != nil {
			return err
		}
		if item.BindingStatus != BindingBound {
			return fmt.Errorf("evidence %q is %s, not BOUND", item.Header.ID, item.BindingStatus)
		}
	}
	return nil
}

func boundEvidenceResolved(evidence []EvidenceEnvelope, resolver EvidenceBindingResolver) error {
	if err := boundEvidence(evidence); err != nil {
		return err
	}
	for _, item := range evidence {
		if err := item.ValidateAgainstBindingResolver(resolver); err != nil {
			return err
		}
	}
	return nil
}

// WorkloadIR describes work without selecting an engine or provider.
type WorkloadIR struct {
	Header                  ContractHeader `json:"header"`
	WorkloadClass           string         `json:"workload_class"`
	InputContractSHA256     string         `json:"input_contract_sha256"`
	RequiredCapabilities    []string       `json:"required_capabilities"`
	RequiredRegionID        string         `json:"required_region_id,omitempty"`
	RequiredFailureDomainID string         `json:"required_failure_domain_id,omitempty"`
}

func (w *WorkloadIR) contractHeader() *ContractHeader { return &w.Header }
func (w *WorkloadIR) Seal() error                     { return seal(w) }

func (w WorkloadIR) Validate() error {
	if err := validateHeader(&w, KindWorkloadIR); err != nil {
		return err
	}
	if err := required(w.WorkloadClass, "workload_class"); err != nil {
		return err
	}
	if err := digest(w.InputContractSHA256, "input_contract_sha256"); err != nil {
		return err
	}
	if err := sortedUnique(w.RequiredCapabilities, "required_capabilities"); err != nil {
		return err
	}
	if w.RequiredRegionID != "" && !idPattern.MatchString(w.RequiredRegionID) {
		return errors.New("invalid required_region_id")
	}
	if w.RequiredFailureDomainID != "" && !idPattern.MatchString(w.RequiredFailureDomainID) {
		return errors.New("invalid required_failure_domain_id")
	}
	return nil
}

func (e RuntimeEngine) Validate() error {
	switch e {
	case EngineVLLM, EngineSGLang, EngineTensorRTLLM, EngineLlamaCPP, EngineMLX, EngineCandle:
		return nil
	}
	// An engine identifier is a cell attribute, not a product boundary. A
	// future engine is admissible to this transport schema if it has a stable
	// canonical identifier; capability/evidence validation still decides
	// whether a particular cell may be selected.
	if enginePattern.MatchString(string(e)) {
		return nil
	}
	return fmt.Errorf("invalid runtime engine %q", e)
}

type RuntimeCellCapability struct {
	CellID           string        `json:"cell_id"`
	Engine           RuntimeEngine `json:"engine"`
	ModelSHA256      string        `json:"model_sha256"`
	CapabilitySHA256 string        `json:"capability_sha256"`
	CapabilityIDs    []string      `json:"capability_ids"`
	RegionID         string        `json:"region_id"`
	FailureDomainID  string        `json:"failure_domain_id"`
	Available        bool          `json:"available"`
}

func (c RuntimeCellCapability) Validate() error {
	if !idPattern.MatchString(c.CellID) {
		return errors.New("invalid runtime cell id")
	}
	if err := c.Engine.Validate(); err != nil {
		return err
	}
	if err := digest(c.ModelSHA256, "runtime_cell.model_sha256"); err != nil {
		return err
	}
	if err := digest(c.CapabilitySHA256, "runtime_cell.capability_sha256"); err != nil {
		return err
	}
	if err := sortedUnique(c.CapabilityIDs, "runtime_cell.capability_ids"); err != nil {
		return err
	}
	if !idPattern.MatchString(c.RegionID) || !idPattern.MatchString(c.FailureDomainID) {
		return errors.New("runtime cell locality is invalid")
	}
	return nil
}

func validateCells(cells []RuntimeCellCapability) error {
	if len(cells) == 0 {
		return errors.New("runtime cells are required")
	}
	for index, cell := range cells {
		if err := cell.Validate(); err != nil {
			return err
		}
		if index > 0 && cells[index-1].CellID >= cell.CellID {
			return errors.New("runtime cells must be sorted and unique")
		}
	}
	return nil
}

func cellSupports(cell RuntimeCellCapability, required []string) bool {
	cellIndex := 0
	for _, want := range required {
		for cellIndex < len(cell.CapabilityIDs) && cell.CapabilityIDs[cellIndex] < want {
			cellIndex++
		}
		if cellIndex == len(cell.CapabilityIDs) || cell.CapabilityIDs[cellIndex] != want {
			return false
		}
	}
	return true
}

func sameRuntimeCell(left, right RuntimeCellCapability) bool {
	if left.CellID != right.CellID || left.Engine != right.Engine ||
		left.ModelSHA256 != right.ModelSHA256 || left.CapabilitySHA256 != right.CapabilitySHA256 ||
		left.RegionID != right.RegionID || left.FailureDomainID != right.FailureDomainID ||
		left.Available != right.Available || len(left.CapabilityIDs) != len(right.CapabilityIDs) {
		return false
	}
	for index := range left.CapabilityIDs {
		if left.CapabilityIDs[index] != right.CapabilityIDs[index] {
			return false
		}
	}
	return true
}

// ProviderAdapter is an observation seam only. It returns a snapshot but cannot
// buy capacity, route a request, or debit money.
type ProviderAdapter interface {
	Snapshot() ProviderAdapterSnapshot
}

type ProviderAdapterSnapshot struct {
	Header         ContractHeader          `json:"header"`
	ProviderID     string                  `json:"provider_id"`
	State          ProviderState           `json:"state"`
	Regions        []RegionIdentity        `json:"regions,omitempty"`
	FailureDomains []FailureDomainIdentity `json:"failure_domains,omitempty"`
	RuntimeCells   []RuntimeCellCapability `json:"runtime_cells,omitempty"`
	Evidence       []EvidenceEnvelope      `json:"evidence,omitempty"`
}

func (p *ProviderAdapterSnapshot) contractHeader() *ContractHeader { return &p.Header }
func (p *ProviderAdapterSnapshot) Seal() error                     { return seal(p) }

func NewUnconfiguredProvider(id, providerID, policyRevision string, createdAt time.Time) (ProviderAdapterSnapshot, error) {
	p := ProviderAdapterSnapshot{
		Header:     ContractHeader{SchemaVersion: SchemaVersionV1, Kind: KindProviderSnapshot, ID: id, PolicyRevision: policyRevision, CreatedAt: createdAt},
		ProviderID: providerID,
		State:      ProviderNotRoutable,
	}
	if err := p.Seal(); err != nil {
		return ProviderAdapterSnapshot{}, err
	}
	return p, p.Validate()
}

func (p ProviderAdapterSnapshot) Validate() error {
	if err := validateHeader(&p, KindProviderSnapshot); err != nil {
		return err
	}
	if err := required(p.ProviderID, "provider_id"); err != nil {
		return err
	}
	switch p.State {
	case ProviderNotRoutable:
		if len(p.RuntimeCells) != 0 || len(p.Evidence) != 0 {
			return errors.New("NOT_ROUTABLE provider cannot advertise supply")
		}
		return nil
	case ProviderRoutable:
		if err := sortedRegions(p.Regions); err != nil {
			return fmt.Errorf("invalid provider regions: %w", err)
		}
		if err := sortedFailureDomains(p.FailureDomains); err != nil {
			return fmt.Errorf("invalid provider failure domains: %w", err)
		}
		for _, region := range p.Regions {
			if region.ProviderID != p.ProviderID {
				return errors.New("invalid provider region")
			}
		}
		for _, domain := range p.FailureDomains {
			if domain.ProviderID != p.ProviderID {
				return errors.New("invalid provider failure domain")
			}
		}
		if err := validateCells(p.RuntimeCells); err != nil {
			return err
		}
		for _, cell := range p.RuntimeCells {
			regionFound := false
			domainFound := false
			for _, region := range p.Regions {
				regionFound = regionFound || region.ID == cell.RegionID
			}
			for _, domain := range p.FailureDomains {
				domainFound = domainFound || (domain.ID == cell.FailureDomainID && domain.RegionID == cell.RegionID)
			}
			if !regionFound || !domainFound {
				return fmt.Errorf("runtime cell %q locality is absent from provider", cell.CellID)
			}
		}
		return boundEvidence(p.Evidence)
	default:
		return errors.New("invalid provider state")
	}
}

type CapabilityIR struct {
	Header        ContractHeader          `json:"header"`
	Provider      ContractRef             `json:"provider"`
	ProviderID    string                  `json:"provider_id"`
	Region        RegionIdentity          `json:"region"`
	FailureDomain FailureDomainIdentity   `json:"failure_domain"`
	RuntimeCells  []RuntimeCellCapability `json:"runtime_cells"`
	Evidence      []EvidenceEnvelope      `json:"evidence"`
}

func (c *CapabilityIR) contractHeader() *ContractHeader { return &c.Header }
func (c *CapabilityIR) Seal() error                     { return seal(c) }

func (c CapabilityIR) Validate() error {
	if err := validateHeader(&c, KindCapabilityIR); err != nil {
		return err
	}
	if err := c.Provider.Validate(KindProviderSnapshot); err != nil {
		return err
	}
	if err := required(c.ProviderID, "capability.provider_id"); err != nil {
		return err
	}
	if err := validateLocality(c.Region, c.FailureDomain); err != nil || c.Region.ProviderID != c.ProviderID {
		return errors.New("invalid capability locality")
	}
	if err := validateCells(c.RuntimeCells); err != nil {
		return err
	}
	for _, cell := range c.RuntimeCells {
		if cell.RegionID != c.Region.ID || cell.FailureDomainID != c.FailureDomain.ID {
			return fmt.Errorf("capability cell %q locality differs from capability locality", cell.CellID)
		}
	}
	return boundEvidence(c.Evidence)
}

func (c CapabilityIR) ValidateAgainstProvider(provider ProviderAdapterSnapshot) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := provider.Validate(); err != nil || provider.State != ProviderRoutable {
		return errors.New("provider is not routable")
	}
	if Ref(provider.Header) != c.Provider || provider.ProviderID != c.ProviderID {
		return errors.New("capability does not bind provider")
	}
	localityPresent := false
	for _, region := range provider.Regions {
		if region != c.Region {
			continue
		}
		for _, domain := range provider.FailureDomains {
			if domain == c.FailureDomain {
				localityPresent = true
				break
			}
		}
	}
	if !localityPresent {
		return errors.New("capability locality is absent from provider")
	}
	for _, capabilityCell := range c.RuntimeCells {
		matched := false
		for _, providerCell := range provider.RuntimeCells {
			if sameRuntimeCell(capabilityCell, providerCell) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("capability cell %q is absent or differs from provider snapshot", capabilityCell.CellID)
		}
	}
	return nil
}

// PricingDecision projects an existing frozen pricing authority; it does not
// calculate, reserve, charge, refund, settle, or move money.
type PricingDecision struct {
	Header              ContractHeader     `json:"header"`
	Workload            ContractRef        `json:"workload"`
	FrozenPricingSHA256 string             `json:"frozen_pricing_sha256"`
	Currency            string             `json:"currency"`
	Evidence            []EvidenceEnvelope `json:"evidence"`
}

func (p *PricingDecision) contractHeader() *ContractHeader { return &p.Header }
func (p *PricingDecision) Seal() error                     { return seal(p) }

func (p PricingDecision) Validate() error {
	if err := validateHeader(&p, KindPricingDecision); err != nil {
		return err
	}
	if err := p.Workload.Validate(KindWorkloadIR); err != nil {
		return err
	}
	if err := digest(p.FrozenPricingSHA256, "frozen_pricing_sha256"); err != nil {
		return err
	}
	if len(p.Currency) != 3 || strings.ToUpper(p.Currency) != p.Currency {
		return errors.New("currency must be uppercase ISO-4217")
	}
	return boundEvidence(p.Evidence)
}

func (p PricingDecision) ValidateAgainst(workload WorkloadIR) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := workload.Validate(); err != nil {
		return err
	}
	if p.Workload != Ref(workload.Header) {
		return errors.New("pricing decision does not bind workload")
	}
	return nil
}

func (p PricingDecision) ValidateAgainstResolved(workload WorkloadIR, resolver EvidenceBindingResolver) error {
	if err := p.ValidateAgainst(workload); err != nil {
		return err
	}
	return boundEvidenceResolved(p.Evidence, resolver)
}

type PlacementDecision struct {
	Header         ContractHeader        `json:"header"`
	Workload       ContractRef           `json:"workload"`
	Capability     ContractRef           `json:"capability"`
	Provider       ContractRef           `json:"provider"`
	SelectedCellID string                `json:"selected_cell_id"`
	Region         RegionIdentity        `json:"region"`
	FailureDomain  FailureDomainIdentity `json:"failure_domain"`
	Mode           string                `json:"mode"`
	Evidence       []EvidenceEnvelope    `json:"evidence"`
}

func (p *PlacementDecision) contractHeader() *ContractHeader { return &p.Header }
func (p *PlacementDecision) Seal() error                     { return seal(p) }

func (p PlacementDecision) Validate() error {
	if err := validateHeader(&p, KindPlacementDecision); err != nil {
		return err
	}
	if err := p.Workload.Validate(KindWorkloadIR); err != nil {
		return err
	}
	if err := p.Capability.Validate(KindCapabilityIR); err != nil {
		return err
	}
	if err := p.Provider.Validate(KindProviderSnapshot); err != nil || !idPattern.MatchString(p.SelectedCellID) || strings.TrimSpace(p.Mode) == "" {
		return errors.New("invalid placement references, cell, or mode")
	}
	if err := validateLocality(p.Region, p.FailureDomain); err != nil {
		return err
	}
	return boundEvidence(p.Evidence)
}

func (p PlacementDecision) ValidateAgainst(workload WorkloadIR, capability CapabilityIR, provider ProviderAdapterSnapshot) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := workload.Validate(); err != nil {
		return err
	}
	if err := capability.ValidateAgainstProvider(provider); err != nil {
		return err
	}
	if p.Workload != Ref(workload.Header) || p.Capability != Ref(capability.Header) || p.Provider != Ref(provider.Header) {
		return errors.New("placement does not bind workload, capability, and provider")
	}
	if p.Region != capability.Region || p.FailureDomain != capability.FailureDomain {
		return errors.New("placement locality differs from capability locality")
	}
	if workload.RequiredRegionID != "" && workload.RequiredRegionID != p.Region.ID {
		return errors.New("placement violates workload region")
	}
	if workload.RequiredFailureDomainID != "" && workload.RequiredFailureDomainID != p.FailureDomain.ID {
		return errors.New("placement violates workload failure domain")
	}
	for _, cell := range capability.RuntimeCells {
		if cell.CellID == p.SelectedCellID && cell.Available && cellSupports(cell, workload.RequiredCapabilities) {
			return nil
		}
	}
	return errors.New("selected cell is absent, unavailable, or lacks workload capabilities")
}

// ValidateAgainstResolved is the selector-safe form. It validates the same
// pure decision graph as ValidateAgainst, then asks the append-only binding
// resolver whether every selection-bearing evidence envelope remains current.
func (p PlacementDecision) ValidateAgainstResolved(
	workload WorkloadIR,
	capability CapabilityIR,
	provider ProviderAdapterSnapshot,
	resolver EvidenceBindingResolver,
) error {
	if err := p.ValidateAgainst(workload, capability, provider); err != nil {
		return err
	}
	if err := boundEvidenceResolved(provider.Evidence, resolver); err != nil {
		return err
	}
	if err := boundEvidenceResolved(capability.Evidence, resolver); err != nil {
		return err
	}
	return boundEvidenceResolved(p.Evidence, resolver)
}

type RuntimeDecision struct {
	Header         ContractHeader     `json:"header"`
	Workload       ContractRef        `json:"workload"`
	Capability     ContractRef        `json:"capability"`
	SelectedCellID string             `json:"selected_cell_id"`
	Engine         RuntimeEngine      `json:"engine"`
	Reason         string             `json:"reason"`
	Evidence       []EvidenceEnvelope `json:"evidence"`
}

func (r *RuntimeDecision) contractHeader() *ContractHeader { return &r.Header }
func (r *RuntimeDecision) Seal() error                     { return seal(r) }

func (r RuntimeDecision) Validate() error {
	if err := validateHeader(&r, KindRuntimeDecision); err != nil {
		return err
	}
	if err := r.Workload.Validate(KindWorkloadIR); err != nil {
		return err
	}
	if err := r.Capability.Validate(KindCapabilityIR); err != nil {
		return err
	}
	if !idPattern.MatchString(r.SelectedCellID) {
		return errors.New("invalid selected runtime cell")
	}
	if err := r.Engine.Validate(); err != nil {
		return err
	}
	if err := required(r.Reason, "runtime.reason"); err != nil {
		return err
	}
	return boundEvidence(r.Evidence)
}

func (r RuntimeDecision) ValidateAgainst(workload WorkloadIR, capability CapabilityIR, provider ProviderAdapterSnapshot) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := workload.Validate(); err != nil {
		return err
	}
	if err := capability.ValidateAgainstProvider(provider); err != nil {
		return err
	}
	if r.Workload != Ref(workload.Header) || r.Capability != Ref(capability.Header) {
		return errors.New("runtime decision does not bind workload and capability")
	}
	for _, cell := range capability.RuntimeCells {
		if cell.CellID == r.SelectedCellID && cell.Available && cell.Engine == r.Engine && cellSupports(cell, workload.RequiredCapabilities) {
			return nil
		}
	}
	return errors.New("selected runtime engine/cell is absent or lacks workload capabilities")
}

func (r RuntimeDecision) ValidateAgainstResolved(
	workload WorkloadIR,
	capability CapabilityIR,
	provider ProviderAdapterSnapshot,
	resolver EvidenceBindingResolver,
) error {
	if err := r.ValidateAgainst(workload, capability, provider); err != nil {
		return err
	}
	if err := boundEvidenceResolved(provider.Evidence, resolver); err != nil {
		return err
	}
	if err := boundEvidenceResolved(capability.Evidence, resolver); err != nil {
		return err
	}
	return boundEvidenceResolved(r.Evidence, resolver)
}

type TopologyDecision struct {
	Header        ContractHeader        `json:"header"`
	Placement     ContractRef           `json:"placement"`
	Topology      string                `json:"topology"`
	Region        RegionIdentity        `json:"region"`
	FailureDomain FailureDomainIdentity `json:"failure_domain"`
	Evidence      []EvidenceEnvelope    `json:"evidence"`
}

func (t *TopologyDecision) contractHeader() *ContractHeader { return &t.Header }
func (t *TopologyDecision) Seal() error                     { return seal(t) }

func (t TopologyDecision) Validate() error {
	if err := validateHeader(&t, KindTopologyDecision); err != nil {
		return err
	}
	if err := t.Placement.Validate(KindPlacementDecision); err != nil || strings.TrimSpace(t.Topology) == "" {
		return errors.New("invalid topology decision")
	}
	if err := validateLocality(t.Region, t.FailureDomain); err != nil {
		return err
	}
	return boundEvidence(t.Evidence)
}

func (t TopologyDecision) ValidateAgainst(
	placement PlacementDecision,
	workload WorkloadIR,
	capability CapabilityIR,
	provider ProviderAdapterSnapshot,
) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := placement.ValidateAgainst(workload, capability, provider); err != nil {
		return err
	}
	if t.Placement != Ref(placement.Header) {
		return errors.New("topology decision does not bind placement")
	}
	if t.Region != placement.Region || t.FailureDomain != placement.FailureDomain {
		return errors.New("topology locality differs from placement locality")
	}
	return nil
}

func (t TopologyDecision) ValidateAgainstResolved(
	placement PlacementDecision,
	workload WorkloadIR,
	capability CapabilityIR,
	provider ProviderAdapterSnapshot,
	resolver EvidenceBindingResolver,
) error {
	if err := t.ValidateAgainst(placement, workload, capability, provider); err != nil {
		return err
	}
	if err := placement.ValidateAgainstResolved(workload, capability, provider, resolver); err != nil {
		return err
	}
	return boundEvidenceResolved(t.Evidence, resolver)
}

type ServiceLease struct {
	Header            ContractHeader        `json:"header"`
	Placement         ContractRef           `json:"placement"`
	Runtime           ContractRef           `json:"runtime"`
	Topology          ContractRef           `json:"topology"`
	Pricing           ContractRef           `json:"pricing"`
	LegacyLeaseSHA256 string                `json:"legacy_lease_sha256"`
	Provider          ContractRef           `json:"provider"`
	Region            RegionIdentity        `json:"region"`
	FailureDomain     FailureDomainIdentity `json:"failure_domain"`
	State             ServiceLeaseState     `json:"state"`
	ExpiresAt         time.Time             `json:"expires_at"`
	FencingToken      uint64                `json:"fencing_token"`
	CapacityUnits     uint64                `json:"capacity_units"`
	Evidence          []EvidenceEnvelope    `json:"evidence"`
}

func (s *ServiceLease) contractHeader() *ContractHeader { return &s.Header }
func (s *ServiceLease) Seal() error                     { return seal(s) }

func (s ServiceLease) Validate() error {
	if err := validateHeader(&s, KindServiceLease); err != nil {
		return err
	}
	if err := s.Placement.Validate(KindPlacementDecision); err != nil {
		return err
	}
	if err := s.Runtime.Validate(KindRuntimeDecision); err != nil {
		return err
	}
	if err := s.Topology.Validate(KindTopologyDecision); err != nil {
		return err
	}
	if err := s.Pricing.Validate(KindPricingDecision); err != nil {
		return err
	}
	if err := s.Provider.Validate(KindProviderSnapshot); err != nil {
		return err
	}
	if err := digest(s.LegacyLeaseSHA256, "legacy_lease_sha256"); err != nil {
		return err
	}
	if err := validateLocality(s.Region, s.FailureDomain); err != nil {
		return err
	}
	switch s.State {
	case LeasePending, LeaseActive, LeaseReleased, LeaseExpired, LeaseRefused:
	default:
		return errors.New("invalid service_lease.state")
	}
	if err := canonicalTimestamp(s.ExpiresAt, "service_lease.expires_at"); err != nil {
		return err
	}
	if !s.ExpiresAt.After(s.Header.CreatedAt) || s.FencingToken == 0 || s.CapacityUnits == 0 {
		return errors.New("service lease requires future expiry, fencing token, and capacity")
	}
	return boundEvidence(s.Evidence)
}

func (s ServiceLease) ValidateAgainst(
	placement PlacementDecision,
	runtime RuntimeDecision,
	topology TopologyDecision,
	pricing PricingDecision,
	workload WorkloadIR,
	capability CapabilityIR,
	provider ProviderAdapterSnapshot,
) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := placement.ValidateAgainst(workload, capability, provider); err != nil {
		return err
	}
	if err := runtime.ValidateAgainst(workload, capability, provider); err != nil {
		return err
	}
	if err := topology.ValidateAgainst(placement, workload, capability, provider); err != nil {
		return err
	}
	if err := pricing.ValidateAgainst(workload); err != nil {
		return err
	}
	if s.Placement != Ref(placement.Header) || s.Runtime != Ref(runtime.Header) || s.Topology != Ref(topology.Header) || s.Pricing != Ref(pricing.Header) || s.Provider != Ref(provider.Header) {
		return errors.New("service lease does not bind placement, runtime, topology, pricing, and provider")
	}
	if runtime.SelectedCellID != placement.SelectedCellID {
		return errors.New("service lease runtime cell differs from placement cell")
	}
	if s.Region != placement.Region || s.FailureDomain != placement.FailureDomain {
		return errors.New("service lease locality differs from placement locality")
	}
	return nil
}

func (s ServiceLease) ValidateAgainstResolved(
	placement PlacementDecision,
	runtime RuntimeDecision,
	topology TopologyDecision,
	pricing PricingDecision,
	workload WorkloadIR,
	capability CapabilityIR,
	provider ProviderAdapterSnapshot,
	resolver EvidenceBindingResolver,
) error {
	if err := s.ValidateAgainst(placement, runtime, topology, pricing, workload, capability, provider); err != nil {
		return err
	}
	if err := topology.ValidateAgainstResolved(placement, workload, capability, provider, resolver); err != nil {
		return err
	}
	if err := runtime.ValidateAgainstResolved(workload, capability, provider, resolver); err != nil {
		return err
	}
	if err := pricing.ValidateAgainstResolved(workload, resolver); err != nil {
		return err
	}
	return boundEvidenceResolved(s.Evidence, resolver)
}

type LocalityEvent struct {
	Header         ContractHeader        `json:"header"`
	EventSequence  uint64                `json:"event_sequence"`
	PreviousSHA256 string                `json:"previous_sha256,omitempty"`
	EventType      string                `json:"event_type"`
	Provider       ContractRef           `json:"provider"`
	Region         RegionIdentity        `json:"region"`
	FailureDomain  FailureDomainIdentity `json:"failure_domain"`
	Evidence       []EvidenceEnvelope    `json:"evidence"`
}

func (l *LocalityEvent) contractHeader() *ContractHeader { return &l.Header }
func (l *LocalityEvent) Seal() error                     { return seal(l) }

func (l LocalityEvent) Validate() error {
	if err := validateHeader(&l, KindLocalityEvent); err != nil {
		return err
	}
	if l.EventSequence == 0 || (l.EventSequence == 1 && l.PreviousSHA256 != "") {
		return errors.New("invalid locality event sequence")
	}
	if l.EventSequence > 1 {
		if err := digest(l.PreviousSHA256, "previous_sha256"); err != nil {
			return err
		}
	}
	if err := required(l.EventType, "locality.event_type"); err != nil {
		return err
	}
	if err := l.Provider.Validate(KindProviderSnapshot); err != nil {
		return err
	}
	if err := validateLocality(l.Region, l.FailureDomain); err != nil {
		return err
	}
	return boundEvidence(l.Evidence)
}

func (l LocalityEvent) ValidateAfter(previous LocalityEvent) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if err := previous.Validate(); err != nil {
		return err
	}
	if l.EventSequence != previous.EventSequence+1 || l.PreviousSHA256 != previous.Header.CanonicalSHA256 {
		return errors.New("locality event is not append-only")
	}
	return nil
}

func (l LocalityEvent) ValidateAgainstProvider(provider ProviderAdapterSnapshot) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if err := provider.Validate(); err != nil || provider.State != ProviderRoutable {
		return errors.New("locality event provider is not routable")
	}
	if l.Provider != Ref(provider.Header) {
		return errors.New("locality event does not bind provider")
	}
	for _, region := range provider.Regions {
		if region != l.Region {
			continue
		}
		for _, domain := range provider.FailureDomains {
			if domain == l.FailureDomain {
				return nil
			}
		}
	}
	return errors.New("locality event locality is absent from provider")
}

func (l LocalityEvent) ValidateAgainstResolvedProvider(provider ProviderAdapterSnapshot, resolver EvidenceBindingResolver) error {
	if err := l.ValidateAgainstProvider(provider); err != nil {
		return err
	}
	if err := boundEvidenceResolved(provider.Evidence, resolver); err != nil {
		return err
	}
	return boundEvidenceResolved(l.Evidence, resolver)
}

// ShadowReplay is record-only. Validation rejects any scheduler, money, or
// state-mutation use, so it is safe for shadow comparisons in the monolith.
type ShadowReplay struct {
	Header                ContractHeader     `json:"header"`
	InputDecision         ContractRef        `json:"input_decision"`
	Status                ShadowReplayStatus `json:"status"`
	Reason                string             `json:"reason"`
	SchedulerInvoked      bool               `json:"scheduler_invoked"`
	MoneyAuthorityInvoked bool               `json:"money_authority_invoked"`
	StateMutationInvoked  bool               `json:"state_mutation_invoked"`
	Evidence              []EvidenceEnvelope `json:"evidence"`
}

func (s *ShadowReplay) contractHeader() *ContractHeader { return &s.Header }
func (s *ShadowReplay) Seal() error                     { return seal(s) }

func (s ShadowReplay) Validate() error {
	if err := validateHeader(&s, KindShadowReplay); err != nil {
		return err
	}
	if err := s.InputDecision.Validate(""); err != nil {
		return err
	}
	if s.Status != ShadowNotExecuted || s.SchedulerInvoked || s.MoneyAuthorityInvoked || s.StateMutationInvoked {
		return errors.New("shadow replay must remain record-only")
	}
	if err := required(s.Reason, "shadow_replay.reason"); err != nil {
		return err
	}
	return boundEvidence(s.Evidence)
}

func (s ShadowReplay) ValidateResolved(resolver EvidenceBindingResolver) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return boundEvidenceResolved(s.Evidence, resolver)
}

// SplitThreshold may ask a reviewer to consider a split. It cannot initiate
// one, and it requires a bounded measured window with BOUND evidence.
type SplitThreshold struct {
	Header        ContractHeader     `json:"header"`
	Metric        string             `json:"metric"`
	Comparator    string             `json:"comparator"`
	Threshold     float64            `json:"threshold"`
	ObservedValue float64            `json:"observed_value"`
	WindowStart   time.Time          `json:"window_start"`
	WindowEnd     time.Time          `json:"window_end"`
	Evidence      []EvidenceEnvelope `json:"evidence"`
}

func (s *SplitThreshold) contractHeader() *ContractHeader { return &s.Header }
func (s *SplitThreshold) Seal() error                     { return seal(s) }

func (s SplitThreshold) Validate() error {
	if err := validateHeader(&s, KindSplitThreshold); err != nil {
		return err
	}
	if err := required(s.Metric, "split_threshold.metric"); err != nil {
		return err
	}
	if s.Comparator != "GT" && s.Comparator != "GTE" {
		return errors.New("split threshold comparator must be GT or GTE")
	}
	if math.IsNaN(s.Threshold) || math.IsInf(s.Threshold, 0) || s.Threshold <= 0 ||
		math.IsNaN(s.ObservedValue) || math.IsInf(s.ObservedValue, 0) || s.ObservedValue < 0 {
		return errors.New("split threshold values must be finite and declared")
	}
	if err := canonicalTimestamp(s.WindowStart, "split_threshold.window_start"); err != nil {
		return err
	}
	if err := canonicalTimestamp(s.WindowEnd, "split_threshold.window_end"); err != nil {
		return err
	}
	if !s.WindowEnd.After(s.WindowStart) {
		return errors.New("split threshold requires a bounded measured window")
	}
	return boundEvidence(s.Evidence)
}

func (s SplitThreshold) Evaluate() (SplitDisposition, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	if s.ObservedValue > s.Threshold || (s.Comparator == "GTE" && s.ObservedValue == s.Threshold) {
		return SplitReviewRequired, nil
	}
	return StayMonolith, nil
}

func (s SplitThreshold) EvaluateResolved(resolver EvidenceBindingResolver) (SplitDisposition, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	if err := boundEvidenceResolved(s.Evidence, resolver); err != nil {
		return "", err
	}
	if s.ObservedValue > s.Threshold || (s.Comparator == "GTE" && s.ObservedValue == s.Threshold) {
		return SplitReviewRequired, nil
	}
	return StayMonolith, nil
}
