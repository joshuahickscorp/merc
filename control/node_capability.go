package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Canonical node Capability (Network V2 Step 6)
//
// Capability answers "what is true of this node". Activation / admission policy
// answers "what may this node be given". Those authorities must never fuse.
//
// This type is the single versioned, digest-bound snapshot of node facts for
// one coherent epoch. It deliberately carries no routability, lifecycle,
// canary, directed-eligibility, or reservation-price field (D1).
//
// WorkerCapability remains the registration ingress DTO. worker rows and
// worker_authorized_capabilities.routable are compatibility projections of
// this snapshot + the separate activation authority, with recorded deletion
// milestones in docs/NETWORK_V2_AUTHORITY_MIGRATION_REGISTER.md §2.
// ---------------------------------------------------------------------------

const (
	nodeCapabilityVersion = 1

	// Knowledge labels for one fact. UNKNOWN can never satisfy a hard contract (D3).
	capabilityKnowledgeMeasured        = "measured"
	capabilityKnowledgeDeclared        = "declared"
	capabilityKnowledgeSupplierDerived = "supplier_derived"
	capabilityKnowledgeAssumed         = "assumed"
	capabilityKnowledgeUnknown         = "unknown"
	capabilityKnowledgeNotApplicable   = "not_applicable"

	// Sentinel values for open-ended node facts that have no measured/declared
	// source yet. Callers compare against these strings; they are never treated
	// as a match for a required concrete value.
	capabilityUnknownValue = "unknown"
)

// CapabilityFactFamily names a freshness domain inside one snapshot.
// Each family declares its own TTL; a fact past its TTL cannot satisfy a hard
// contract for that family (D5).
type CapabilityFactFamily string

const (
	capabilityFamilyIdentity         CapabilityFactFamily = "identity"
	capabilityFamilyHardware         CapabilityFactFamily = "hardware"
	capabilityFamilyTopology         CapabilityFactFamily = "topology"
	capabilityFamilyMemory           CapabilityFactFamily = "memory"
	capabilityFamilyStorage          CapabilityFactFamily = "storage"
	capabilityFamilyNetwork          CapabilityFactFamily = "network"
	capabilityFamilyRegion           CapabilityFactFamily = "region"
	capabilityFamilyFailureDomain    CapabilityFactFamily = "failure_domain"
	capabilityFamilyRuntimeCells     CapabilityFactFamily = "runtime_cells"
	capabilityFamilyVersions         CapabilityFactFamily = "versions"
	capabilityFamilyModelResidency   CapabilityFactFamily = "model_residency"
	capabilityFamilyPrefixCache      CapabilityFactFamily = "prefix_cache"
	capabilityFamilyAvailability     CapabilityFactFamily = "availability"
	capabilityFamilyLimits           CapabilityFactFamily = "limits"
	capabilityFamilyInterruption     CapabilityFactFamily = "interruption"
	capabilityFamilyThermalPower     CapabilityFactFamily = "thermal_power"
	capabilityFamilyBenchmark        CapabilityFactFamily = "benchmark"
	capabilityFamilyTrustReliability CapabilityFactFamily = "trust_reliability"
	capabilityFamilyContainment      CapabilityFactFamily = "containment"
)

// capabilityFamilyTTL is the hard-contract freshness window per family.
// Values mirror production gates so wiring this vocabulary cannot change who
// is currently eligible (D6): liveness 60s, authorization/benchmark 7d.
// Families with no production hard gate use 0 (always fresh once observed, or
// still refuse UNKNOWN via knowledge checks).
var capabilityFamilyTTL = map[CapabilityFactFamily]time.Duration{
	capabilityFamilyIdentity:         0, // enrollment identity; no rolling TTL
	capabilityFamilyHardware:         0,
	capabilityFamilyTopology:         0,
	capabilityFamilyMemory:           0, // total memory is enrollment; live mem is availability
	capabilityFamilyStorage:          0,
	capabilityFamilyNetwork:          0,
	capabilityFamilyRegion:           0,
	capabilityFamilyFailureDomain:    0,
	capabilityFamilyRuntimeCells:     7 * 24 * time.Hour, // matches wac.authorized_at window
	capabilityFamilyVersions:         0,
	capabilityFamilyModelResidency:   60 * time.Second, // warm model window (soft today; hard when required)
	capabilityFamilyPrefixCache:      90 * time.Second, // prefix warm preference window
	capabilityFamilyAvailability:     60 * time.Second, // last_seen claim gate
	capabilityFamilyLimits:           0,
	capabilityFamilyInterruption:     0,
	capabilityFamilyThermalPower:     0, // thermal_ok is soft after register
	capabilityFamilyBenchmark:        7 * 24 * time.Hour,
	capabilityFamilyTrustReliability: 0,
	capabilityFamilyContainment:      0,
}

// CapabilityFact is one labeled value with provenance. Knowledge=unknown means
// the fact is absent; such a fact can never satisfy a hard contract.
type CapabilityFact struct {
	Knowledge string `json:"knowledge"`
	Value     string `json:"value,omitempty"`
	Source    string `json:"source,omitempty"`
	// ObservedUnix is when this fact was last observed (UTC seconds). Zero means
	// "not timed" — only valid for UNKNOWN / not_applicable.
	ObservedUnix int64 `json:"observed_unix,omitempty"`
}

// CapabilityFloatFact is a numeric fact with the same knowledge labels.
type CapabilityFloatFact struct {
	Knowledge    string  `json:"knowledge"`
	Value        float64 `json:"value,omitempty"`
	Source       string  `json:"source,omitempty"`
	ObservedUnix int64   `json:"observed_unix,omitempty"`
}

// CapabilityIntFact is an integer fact with the same knowledge labels.
type CapabilityIntFact struct {
	Knowledge    string `json:"knowledge"`
	Value        int    `json:"value,omitempty"`
	Source       string `json:"source,omitempty"`
	ObservedUnix int64  `json:"observed_unix,omitempty"`
}

// CapabilityCellRef is a node-bound reference to an immutable catalogue cell.
// It intentionally omits lifecycle, routable, quality tier, and benchmark
// authority — those are activation / catalogue policy, not node facts (D1).
type CapabilityCellRef struct {
	CellID       string  `json:"cell_id"`
	RuntimeID    string  `json:"runtime_id"`
	Engine       string  `json:"engine"`
	Device       string  `json:"device"`
	Job          string  `json:"job"`
	Model        string  `json:"model"`
	ModelKind    string  `json:"model_kind"`
	Runner       string  `json:"runner"`
	MinMemoryGB  float64 `json:"min_memory_gb"`
	Verification string  `json:"verification"`
	// AuthorizedAtUnix is when this cell was authorized for the node. Freshness
	// for the runtime_cells family uses this stamp.
	AuthorizedAtUnix int64 `json:"authorized_at_unix"`
}

// CapabilityBenchmarkFact is one self-reported (or later corroborated) rate.
type CapabilityBenchmarkFact struct {
	ModelID      string  `json:"model_id"`
	JobType      string  `json:"job_type"`
	TPS          float64 `json:"tps"`
	EPS          float64 `json:"eps"`
	P99MS        uint32  `json:"p99_ms"`
	ThermalOK    bool    `json:"thermal_ok"`
	LoadMS       uint64  `json:"load_ms"`
	Unit         string  `json:"unit,omitempty"`
	UnitScope    string  `json:"unit_scope,omitempty"`
	MeasuredUnix uint64  `json:"measured_unix"`
	Knowledge    string  `json:"knowledge"` // measured | unknown
}

// NodeCapability is the canonical, versioned, digest-bound node snapshot.
//
// No field on this type may encode routability, lifecycle, canary membership,
// directed eligibility, min payout / reservation price, or activation policy
// revision. Those belong to other authorities.
type NodeCapability struct {
	Version      int       `json:"version"`
	Epoch        int64     `json:"epoch"`
	WorkerID     uuid.UUID `json:"worker_id"`
	SupplierID   uuid.UUID `json:"supplier_id"`
	CapturedAt   time.Time `json:"captured_at"`
	MatrixSHA256 string    `json:"matrix_sha256"`

	// Identity
	AgentSessionID *uuid.UUID `json:"agent_session_id,omitempty"`

	// Hardware
	HWClass          CapabilityFact `json:"hw_class"`
	HardwareIdentity CapabilityFact `json:"hardware_identity"`
	Engine           CapabilityFact `json:"engine"`

	// Topology (lossless capture of wire facts previously unpersisted)
	GPUCount       CapabilityIntFact   `json:"gpu_count"`
	MemoryGBPerGPU CapabilityFloatFact `json:"memory_gb_per_gpu"`
	Interconnect   CapabilityFact      `json:"interconnect"`

	// Memory / storage / network
	MemoryGB     CapabilityFloatFact `json:"memory_gb"`
	MemoryBwGbps CapabilityFloatFact `json:"memory_bw_gbps"`
	DiskGB       CapabilityFloatFact `json:"disk_gb"`
	// NetworkBW is absent on registration today — always UNKNOWN unless a
	// measured fabric path later fills it. Fabric measurements remain non-
	// admission evidence and are not copied here as hard capability.
	NetworkBWGbps CapabilityFloatFact `json:"network_bw_gbps"`

	// Region / failure domain. Region may be supplier-derived until agents
	// declare it; provenance must stay visible (D3).
	Region        CapabilityFact `json:"region"`
	FailureDomain CapabilityFact `json:"failure_domain"`

	// Runtime cells this node is authorized for (facts only — no routable).
	RuntimeCells []CapabilityCellRef `json:"runtime_cells"`

	// Versions
	AgentVersion        CapabilityFact `json:"agent_version"`
	OSVersion           CapabilityFact `json:"os_version"`
	BuildHash           CapabilityFact `json:"build_hash"`
	BuildIdentityPolicy CapabilityFact `json:"build_identity_policy"`
	RuntimeProfileID    CapabilityFact `json:"runtime_profile_id"`
	RuntimeProfileRev   CapabilityFact `json:"runtime_profile_revision"`
	RuntimeProfileDig   CapabilityFact `json:"runtime_profile_digest"`

	// Model declaration (enrollment). Live warmth is availability/residency
	// and is refreshed on heartbeat epochs.
	SupportedJobs   []string `json:"supported_jobs"`
	SupportedModels []string `json:"supported_models"`

	// Availability stamps at capture. Live heartbeats produce later epochs.
	LastSeenUnix CapabilityIntFact `json:"last_seen_unix"`
	Throttled    CapabilityFact    `json:"throttled"`

	// Interruption / preempt / spot — UNKNOWN until declared or measured (D3).
	InterruptionPolicy CapabilityFact `json:"interruption_policy"`

	// Thermal / power. thermal_ok is measured from benches; sustained watts
	// remain class-level ASSUMED and are referenced, never promoted (D3).
	ThermalOK        CapabilityFact `json:"thermal_ok"`
	PowerPolicyWatts CapabilityFact `json:"power_policy_watts"`
	PowerPolicyClass string         `json:"power_policy_class,omitempty"`

	// Benchmarks captured at this epoch.
	Benchmarks []CapabilityBenchmarkFact `json:"benchmarks"`

	// Containment as reported at registration (node fact; claim policy that
	// consumes it lives in the claim path, not here as a routable flag).
	Sandboxed        CapabilityFact `json:"sandboxed"`
	UnsandboxedOptIn CapabilityFact `json:"unsandboxed_opt_in"`

	// Digest is computed over the canonical encoding of every field above
	// except Digest itself. Empty until Seal() is called.
	Digest string `json:"digest,omitempty"`
}

// HardCapabilityRequirement is one hard-contract demand against a snapshot.
// Knowledge UNKNOWN on the node side, or a stale family, can never satisfy it.
type HardCapabilityRequirement struct {
	Family CapabilityFactFamily
	// Kind selects the check:
	//   "eq"      — fact.Value must equal Want (and knowledge must not be unknown)
	//   "known"   — knowledge must not be unknown
	//   "min_f64" — float fact >= WantF64
	//   "min_int" — int fact >= WantInt
	//   "true"    — fact.Value must be "true"
	Kind    string
	Want    string
	WantF64 float64
	WantInt int
	Field   string // diagnostic only
}

// NodeCapabilityBuildInput is everything required to convert registration +
// projection into one canonical snapshot without reading policy into it.
type NodeCapabilityBuildInput struct {
	Registration   WorkerCapability
	ProjectedCells []generatedRuntimeCapability
	// CellAuthorizedAt maps cell_id -> authorization instant (UTC).
	CellAuthorizedAt map[string]time.Time
	// SupplierDataCountry is the supplier residency fact used as a derived
	// region until the agent declares one. Empty → region UNKNOWN.
	SupplierDataCountry string
	// Profile identity resolved at enrollment (catalogue reference, not copy).
	ProfileID     string
	ProfileRev    string
	ProfileDigest string
	MatrixSHA256  string
	Epoch         int64
	CapturedAt    time.Time
	// LastSeen defaults to CapturedAt when zero (registration path).
	LastSeen time.Time
}

// BuildNodeCapability converts registration ingress + projected cells into the
// canonical snapshot. It never consults activation routability (D1).
func BuildNodeCapability(in NodeCapabilityBuildInput) (NodeCapability, error) {
	if in.Epoch <= 0 {
		return NodeCapability{}, fmt.Errorf("capability epoch must be positive, got %d", in.Epoch)
	}
	if in.CapturedAt.IsZero() {
		in.CapturedAt = time.Now().UTC()
	} else {
		in.CapturedAt = in.CapturedAt.UTC()
	}
	if in.LastSeen.IsZero() {
		in.LastSeen = in.CapturedAt
	} else {
		in.LastSeen = in.LastSeen.UTC()
	}
	if in.MatrixSHA256 == "" {
		in.MatrixSHA256 = generatedRuntimeMatrixSHA256
	}
	cap := in.Registration
	nowUnix := in.CapturedAt.Unix()

	thermalOK := true
	for _, b := range cap.Benchmarks {
		thermalOK = thermalOK && b.ThermalOK
	}

	nc := NodeCapability{
		Version:        nodeCapabilityVersion,
		Epoch:          in.Epoch,
		WorkerID:       cap.WorkerID,
		SupplierID:     cap.SupplierID,
		CapturedAt:     in.CapturedAt,
		MatrixSHA256:   in.MatrixSHA256,
		AgentSessionID: cap.AgentSessionID,

		HWClass:          declaredFact(cap.HWClass, "worker_capability.hw_class", nowUnix),
		HardwareIdentity: declaredFact(cap.HardwareIdentity, "worker_capability.hardware_identity", nowUnix),
		Engine:           declaredFact(normalizeEngine(cap.Engine), "worker_capability.engine", nowUnix),

		MemoryGB:     declaredFloat(float64(cap.MemoryGB), "worker_capability.memory_gb", nowUnix),
		MemoryBwGbps: declaredFloat(float64(cap.MemoryBwGbps), "worker_capability.memory_bw_gbps", nowUnix),
		DiskGB:       storageFactFromRegistration(cap, nowUnix),
		NetworkBWGbps: CapabilityFloatFact{
			Knowledge: capabilityKnowledgeUnknown,
			Source:    "absent_on_registration",
		},

		Region:        regionFactFromRegistration(cap, in.SupplierDataCountry, nowUnix),
		FailureDomain: optionalOrUnknown(cap.FailureDomain, "worker_capability.failure_domain", nowUnix),

		AgentVersion:        declaredFact(cap.AgentVersion, "worker_capability.agent_version", nowUnix),
		OSVersion:           osVersionFact(cap.OSVersion, nowUnix),
		BuildHash:           declaredFact(cap.BuildHash, "worker_capability.build_hash", nowUnix),
		BuildIdentityPolicy: declaredFact(cap.BuildIdentityPolicy, "worker_capability.build_identity_policy", nowUnix),
		RuntimeProfileID:    declaredFact(in.ProfileID, "resolved_runtime_profile", nowUnix),
		RuntimeProfileRev:   declaredFact(in.ProfileRev, "resolved_runtime_profile", nowUnix),
		RuntimeProfileDig:   declaredFact(in.ProfileDigest, "resolved_runtime_profile", nowUnix),

		SupportedJobs:   append([]string(nil), cap.SupportedJobs...),
		SupportedModels: append([]string(nil), cap.SupportedModels...),

		LastSeenUnix: CapabilityIntFact{
			Knowledge:    capabilityKnowledgeMeasured,
			Value:        int(in.LastSeen.Unix()),
			Source:       "registration_or_heartbeat",
			ObservedUnix: in.LastSeen.Unix(),
		},
		Throttled: CapabilityFact{
			Knowledge:    capabilityKnowledgeMeasured,
			Value:        boolString(false), // registration path is not throttled
			Source:       "registration_default",
			ObservedUnix: nowUnix,
		},

		InterruptionPolicy: optionalOrUnknown(cap.InterruptionPolicy, "worker_capability.interruption_policy", nowUnix),

		ThermalOK: CapabilityFact{
			Knowledge:    capabilityKnowledgeMeasured,
			Value:        boolString(thermalOK),
			Source:       "benchmark_results.thermal_ok_and",
			ObservedUnix: nowUnix,
		},
		PowerPolicyWatts: powerPolicyAssumed(cap.HWClass),
		PowerPolicyClass: cap.HWClass,

		Sandboxed: CapabilityFact{
			Knowledge:    capabilityKnowledgeDeclared,
			Value:        boolString(cap.Sandboxed),
			Source:       "worker_capability.sandboxed",
			ObservedUnix: nowUnix,
		},
		UnsandboxedOptIn: CapabilityFact{
			Knowledge:    capabilityKnowledgeDeclared,
			Value:        boolString(cap.UnsandboxedOptIn),
			Source:       "worker_capability.unsandboxed_opt_in",
			ObservedUnix: nowUnix,
		},
	}

	nc.GPUCount, nc.MemoryGBPerGPU, nc.Interconnect = topologyFactsFromRegistration(cap, nowUnix)

	// Runtime cells: catalogue facts the node is authorized for. Routable is
	// NOT recorded here — activation owns that (D1).
	cells := make([]CapabilityCellRef, 0, len(in.ProjectedCells))
	for _, c := range in.ProjectedCells {
		authAt := in.CapturedAt
		if in.CellAuthorizedAt != nil {
			if t, ok := in.CellAuthorizedAt[c.ID]; ok && !t.IsZero() {
				authAt = t.UTC()
			}
		}
		cells = append(cells, CapabilityCellRef{
			CellID:           c.ID,
			RuntimeID:        c.Runtime,
			Engine:           c.Engine,
			Device:           c.Device,
			Job:              c.Job,
			Model:            c.Model,
			ModelKind:        c.ModelKind,
			Runner:           c.Runner,
			MinMemoryGB:      c.MinMemoryGB,
			Verification:     c.Verification,
			AuthorizedAtUnix: authAt.Unix(),
		})
	}
	// Stable order for digest determinism.
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].CellID != cells[j].CellID {
			return cells[i].CellID < cells[j].CellID
		}
		return cells[i].RuntimeID < cells[j].RuntimeID
	})
	nc.RuntimeCells = cells

	benches := make([]CapabilityBenchmarkFact, 0, len(cap.Benchmarks))
	for _, b := range cap.Benchmarks {
		benches = append(benches, CapabilityBenchmarkFact{
			ModelID:      b.ModelID,
			JobType:      b.JobType,
			TPS:          float64(b.TPS),
			EPS:          float64(b.EPS),
			P99MS:        b.P99MS,
			ThermalOK:    b.ThermalOK,
			LoadMS:       b.LoadMS,
			Unit:         b.Unit,
			UnitScope:    b.UnitScope,
			MeasuredUnix: b.MeasuredUnix,
			Knowledge:    capabilityKnowledgeMeasured,
		})
	}
	sort.Slice(benches, func(i, j int) bool {
		if benches[i].JobType != benches[j].JobType {
			return benches[i].JobType < benches[j].JobType
		}
		return benches[i].ModelID < benches[j].ModelID
	})
	nc.Benchmarks = benches

	sort.Strings(nc.SupportedJobs)
	sort.Strings(nc.SupportedModels)

	if err := nc.Seal(); err != nil {
		return NodeCapability{}, err
	}
	return nc, nil
}

// Seal computes and stores Digest. Re-sealing after mutation is required;
// VerifyDigest fails on a tampered snapshot.
func (c *NodeCapability) Seal() error {
	if c.Version != nodeCapabilityVersion {
		return fmt.Errorf("unsupported node capability version %d", c.Version)
	}
	// Digest over a copy with Digest cleared so the field is not self-referential.
	copy := *c
	copy.Digest = ""
	// Normalize time to UTC second precision for stable JSON.
	copy.CapturedAt = copy.CapturedAt.UTC().Truncate(time.Second)
	blob, err := json.Marshal(copy)
	if err != nil {
		return fmt.Errorf("marshal node capability: %w", err)
	}
	sum := sha256.Sum256(blob)
	c.Digest = hex.EncodeToString(sum[:])
	c.CapturedAt = copy.CapturedAt
	return nil
}

// VerifyDigest recomputes the digest and rejects tampering.
func (c NodeCapability) VerifyDigest() error {
	if c.Digest == "" {
		return fmt.Errorf("node capability digest is empty")
	}
	if !validSHA256(c.Digest) {
		return fmt.Errorf("node capability digest is not a sha256")
	}
	copy := c
	if err := copy.Seal(); err != nil {
		return err
	}
	if copy.Digest != c.Digest {
		return fmt.Errorf("node capability digest mismatch: sealed=%s stored=%s", copy.Digest, c.Digest)
	}
	return nil
}

// FamilyObservedAt returns the observation instant used for a family's TTL.
func (c NodeCapability) FamilyObservedAt(family CapabilityFactFamily) time.Time {
	switch family {
	case capabilityFamilyIdentity:
		return time.Unix(c.HWClass.ObservedUnix, 0).UTC()
	case capabilityFamilyHardware:
		return time.Unix(c.HWClass.ObservedUnix, 0).UTC()
	case capabilityFamilyTopology:
		return time.Unix(c.GPUCount.ObservedUnix, 0).UTC()
	case capabilityFamilyMemory:
		return time.Unix(c.MemoryGB.ObservedUnix, 0).UTC()
	case capabilityFamilyStorage:
		return time.Unix(c.DiskGB.ObservedUnix, 0).UTC()
	case capabilityFamilyNetwork:
		return time.Unix(c.NetworkBWGbps.ObservedUnix, 0).UTC()
	case capabilityFamilyRegion:
		return time.Unix(c.Region.ObservedUnix, 0).UTC()
	case capabilityFamilyFailureDomain:
		return time.Unix(c.FailureDomain.ObservedUnix, 0).UTC()
	case capabilityFamilyRuntimeCells:
		var latest int64
		for _, cell := range c.RuntimeCells {
			if cell.AuthorizedAtUnix > latest {
				latest = cell.AuthorizedAtUnix
			}
		}
		if latest == 0 {
			return c.CapturedAt
		}
		return time.Unix(latest, 0).UTC()
	case capabilityFamilyVersions:
		return time.Unix(c.AgentVersion.ObservedUnix, 0).UTC()
	case capabilityFamilyModelResidency:
		// Enrollment declaration timestamp; live warmth is a later epoch.
		return c.CapturedAt
	case capabilityFamilyPrefixCache:
		return c.CapturedAt
	case capabilityFamilyAvailability:
		return time.Unix(int64(c.LastSeenUnix.Value), 0).UTC()
	case capabilityFamilyLimits:
		return c.CapturedAt
	case capabilityFamilyInterruption:
		return time.Unix(c.InterruptionPolicy.ObservedUnix, 0).UTC()
	case capabilityFamilyThermalPower:
		return time.Unix(c.ThermalOK.ObservedUnix, 0).UTC()
	case capabilityFamilyBenchmark:
		var latest uint64
		for _, b := range c.Benchmarks {
			if b.MeasuredUnix > latest {
				latest = b.MeasuredUnix
			}
		}
		if latest == 0 {
			return c.CapturedAt
		}
		return time.Unix(int64(latest), 0).UTC()
	case capabilityFamilyTrustReliability:
		return c.CapturedAt
	case capabilityFamilyContainment:
		return time.Unix(c.Sandboxed.ObservedUnix, 0).UTC()
	default:
		return time.Time{}
	}
}

// FamilyFresh reports whether the family's observation is within its TTL at now.
// A zero TTL means "no rolling expiry" (still subject to UNKNOWN refusal).
func (c NodeCapability) FamilyFresh(family CapabilityFactFamily, now time.Time) bool {
	ttl, ok := capabilityFamilyTTL[family]
	if !ok {
		return false
	}
	if ttl == 0 {
		return true
	}
	observed := c.FamilyObservedAt(family)
	if observed.IsZero() {
		return false
	}
	return !now.UTC().After(observed.Add(ttl))
}

// SatisfiesHardContract checks every requirement against this snapshot at now.
// Failures are closed: UNKNOWN knowledge, mismatched value, or stale family.
func (c NodeCapability) SatisfiesHardContract(reqs []HardCapabilityRequirement, now time.Time) error {
	if err := c.VerifyDigest(); err != nil {
		return fmt.Errorf("capability digest: %w", err)
	}
	for _, req := range reqs {
		if !c.FamilyFresh(req.Family, now) {
			return fmt.Errorf("stale capability family %q (field %s) cannot satisfy a hard contract",
				req.Family, req.Field)
		}
		if err := c.checkRequirement(req); err != nil {
			return err
		}
	}
	return nil
}

func (c NodeCapability) checkRequirement(req HardCapabilityRequirement) error {
	switch req.Field {
	case "failure_domain":
		return checkStringFact(c.FailureDomain, req)
	case "interruption_policy":
		return checkStringFact(c.InterruptionPolicy, req)
	case "region":
		return checkStringFact(c.Region, req)
	case "hw_class":
		return checkStringFact(c.HWClass, req)
	case "hardware_identity":
		return checkStringFact(c.HardwareIdentity, req)
	case "engine":
		return checkStringFact(c.Engine, req)
	case "thermal_ok":
		return checkStringFact(c.ThermalOK, req)
	case "sandboxed":
		return checkStringFact(c.Sandboxed, req)
	case "unsandboxed_opt_in":
		return checkStringFact(c.UnsandboxedOptIn, req)
	case "disk_gb":
		return checkFloatFact(c.DiskGB, req)
	case "memory_gb":
		return checkFloatFact(c.MemoryGB, req)
	case "gpu_count":
		return checkIntFact(c.GPUCount, req)
	case "os_version":
		return checkStringFact(c.OSVersion, req)
	case "interconnect":
		return checkStringFact(c.Interconnect, req)
	default:
		return fmt.Errorf("unknown hard capability field %q", req.Field)
	}
}

func checkStringFact(f CapabilityFact, req HardCapabilityRequirement) error {
	if f.Knowledge == capabilityKnowledgeUnknown || f.Value == "" || f.Value == capabilityUnknownValue {
		return fmt.Errorf("UNKNOWN %s cannot satisfy hard contract (%s)", req.Field, req.Kind)
	}
	switch req.Kind {
	case "known":
		return nil
	case "eq":
		if f.Value != req.Want {
			return fmt.Errorf("%s=%q does not equal required %q", req.Field, f.Value, req.Want)
		}
		return nil
	case "true":
		if f.Value != "true" {
			return fmt.Errorf("%s=%q is not true", req.Field, f.Value)
		}
		return nil
	default:
		return fmt.Errorf("unsupported kind %q for string field %s", req.Kind, req.Field)
	}
}

func checkFloatFact(f CapabilityFloatFact, req HardCapabilityRequirement) error {
	if f.Knowledge == capabilityKnowledgeUnknown {
		return fmt.Errorf("UNKNOWN %s cannot satisfy hard contract (%s)", req.Field, req.Kind)
	}
	switch req.Kind {
	case "known":
		return nil
	case "min_f64":
		if f.Value < req.WantF64 {
			return fmt.Errorf("%s=%v is below required %v", req.Field, f.Value, req.WantF64)
		}
		return nil
	case "eq":
		if fmt.Sprintf("%v", f.Value) != req.Want {
			return fmt.Errorf("%s=%v does not equal required %s", req.Field, f.Value, req.Want)
		}
		return nil
	default:
		return fmt.Errorf("unsupported kind %q for float field %s", req.Kind, req.Field)
	}
}

func checkIntFact(f CapabilityIntFact, req HardCapabilityRequirement) error {
	if f.Knowledge == capabilityKnowledgeUnknown {
		return fmt.Errorf("UNKNOWN %s cannot satisfy hard contract (%s)", req.Field, req.Kind)
	}
	switch req.Kind {
	case "known":
		return nil
	case "min_int":
		if f.Value < req.WantInt {
			return fmt.Errorf("%s=%d is below required %d", req.Field, f.Value, req.WantInt)
		}
		return nil
	case "eq":
		if fmt.Sprintf("%d", f.Value) != req.Want {
			return fmt.Errorf("%s=%d does not equal required %s", req.Field, f.Value, req.Want)
		}
		return nil
	default:
		return fmt.Errorf("unsupported kind %q for int field %s", req.Kind, req.Field)
	}
}

// ---------------------------------------------------------------------------
// Activation routability projection (policy authority — not Capability)
// ---------------------------------------------------------------------------

// activationRoutableProjection is the policy-authority answer for "may this
// cell take ordinary buyer work?". It is the sole writer of
// worker_authorized_capabilities.routable during migration (compatibility
// projection of activation, D1/D2). Capability snapshots never call this.
func activationRoutableProjection(cellID string) bool {
	return advertisedRuntimeCell(cellID)
}

// legacyClaimRequiresRoutable preserves the documented claim-SQL branch:
// legacy jobs with workload_decision IS NULL still require wac.routable so a
// directed-only worker cannot claim ordinary work (schema.sql:1236-1244).
func legacyClaimRequiresRoutable(workloadDecisionPresent bool, projectedRoutable bool) bool {
	if workloadDecisionPresent {
		// Frozen directed/ordinary candidates name cells explicitly; routable
		// is not the gate (claim SQL matches frozen cell identity instead).
		return true
	}
	return projectedRoutable
}

// ---------------------------------------------------------------------------
// Conversion helpers
// ---------------------------------------------------------------------------

func declaredFact(value, source string, observedUnix int64) CapabilityFact {
	if strings.TrimSpace(value) == "" {
		return CapabilityFact{
			Knowledge: capabilityKnowledgeUnknown,
			Value:     capabilityUnknownValue,
			Source:    source + ":empty",
		}
	}
	return CapabilityFact{
		Knowledge:    capabilityKnowledgeDeclared,
		Value:        value,
		Source:       source,
		ObservedUnix: observedUnix,
	}
}

func declaredFloat(value float64, source string, observedUnix int64) CapabilityFloatFact {
	return CapabilityFloatFact{
		Knowledge:    capabilityKnowledgeDeclared,
		Value:        value,
		Source:       source,
		ObservedUnix: observedUnix,
	}
}

func optionalOrUnknown(value, source string, observedUnix int64) CapabilityFact {
	if strings.TrimSpace(value) == "" {
		return CapabilityFact{
			Knowledge: capabilityKnowledgeUnknown,
			Value:     capabilityUnknownValue,
			Source:    source + ":absent",
		}
	}
	return CapabilityFact{
		Knowledge:    capabilityKnowledgeDeclared,
		Value:        value,
		Source:       source,
		ObservedUnix: observedUnix,
	}
}

func osVersionFact(osVersion string, observedUnix int64) CapabilityFact {
	if strings.TrimSpace(osVersion) == "" {
		return CapabilityFact{
			Knowledge: capabilityKnowledgeUnknown,
			Value:     capabilityUnknownValue,
			Source:    "worker_capability.os_version:absent",
		}
	}
	return CapabilityFact{
		Knowledge:    capabilityKnowledgeDeclared,
		Value:        osVersion,
		Source:       "worker_capability.os_version",
		ObservedUnix: observedUnix,
	}
}

func regionFactFromRegistration(cap WorkerCapability, supplierCountry string, observedUnix int64) CapabilityFact {
	if strings.TrimSpace(cap.Region) != "" {
		return CapabilityFact{
			Knowledge:    capabilityKnowledgeDeclared,
			Value:        cap.Region,
			Source:       "worker_capability.region",
			ObservedUnix: observedUnix,
		}
	}
	if strings.TrimSpace(supplierCountry) != "" {
		return CapabilityFact{
			Knowledge:    capabilityKnowledgeSupplierDerived,
			Value:        supplierCountry,
			Source:       "suppliers.data_country",
			ObservedUnix: observedUnix,
		}
	}
	return CapabilityFact{
		Knowledge: capabilityKnowledgeUnknown,
		Value:     capabilityUnknownValue,
		Source:    "region:absent",
	}
}

func storageFactFromRegistration(cap WorkerCapability, observedUnix int64) CapabilityFloatFact {
	// Persist only when the agent actually measured it (D3). A zero or absent
	// optional pointer means UNKNOWN — never invent a disk size.
	if cap.DiskGB == nil {
		return CapabilityFloatFact{
			Knowledge: capabilityKnowledgeUnknown,
			Source:    "worker_capability.disk_gb:absent",
		}
	}
	if *cap.DiskGB <= 0 {
		return CapabilityFloatFact{
			Knowledge: capabilityKnowledgeUnknown,
			Source:    "worker_capability.disk_gb:non_positive",
		}
	}
	return CapabilityFloatFact{
		Knowledge:    capabilityKnowledgeMeasured,
		Value:        float64(*cap.DiskGB),
		Source:       "worker_capability.disk_gb",
		ObservedUnix: observedUnix,
	}
}

func topologyFactsFromRegistration(cap WorkerCapability, observedUnix int64) (CapabilityIntFact, CapabilityFloatFact, CapabilityFact) {
	// Wire already carries these; default single-GPU when omitted matches
	// runtime_profile_admission.go (gpu_count <= 0 → 1 device).
	gpuCount := cap.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	memPerGPU := float64(cap.MemoryGBPerGPU)
	if memPerGPU <= 0 {
		memPerGPU = float64(cap.MemoryGB)
	}
	interconnect := strings.TrimSpace(cap.Interconnect)
	interFact := CapabilityFact{
		Knowledge:    capabilityKnowledgeDeclared,
		Value:        interconnect,
		Source:       "worker_capability.interconnect",
		ObservedUnix: observedUnix,
	}
	if interconnect == "" {
		if gpuCount <= 1 {
			// Single-GPU hosts have no multi-device fabric; empty is N/A, not a
			// mystery that should block single-GPU work.
			interFact = CapabilityFact{
				Knowledge:    capabilityKnowledgeNotApplicable,
				Value:        "",
				Source:       "single_gpu_host",
				ObservedUnix: observedUnix,
			}
		} else {
			interFact = CapabilityFact{
				Knowledge: capabilityKnowledgeUnknown,
				Value:     capabilityUnknownValue,
				Source:    "worker_capability.interconnect:absent_multi_gpu",
			}
		}
	}
	return CapabilityIntFact{
			Knowledge:    capabilityKnowledgeDeclared,
			Value:        gpuCount,
			Source:       "worker_capability.gpu_count",
			ObservedUnix: observedUnix,
		}, CapabilityFloatFact{
			Knowledge:    capabilityKnowledgeDeclared,
			Value:        memPerGPU,
			Source:       "worker_capability.memory_gb_per_gpu",
			ObservedUnix: observedUnix,
		}, interFact
}

func powerPolicyAssumed(hwClass string) CapabilityFact {
	// Class-level ASSUMED watts from pricing.go — never promote to MEASURED (D3).
	entry, ok := sustainedWattsByHWClass[hwClass]
	if !ok {
		return CapabilityFact{
			Knowledge: capabilityKnowledgeUnknown,
			Value:     capabilityUnknownValue,
			Source:    "sustainedWattsByHWClass:missing_class",
		}
	}
	knowledge := capabilityKnowledgeAssumed
	if entry.Kind() == wattKindMeasured {
		// Preserve MEASURED if a class ever earns it; do not invent MEASURED.
		// VENDOR_WALL_UPPER_BOUND is a conservative envelope, not a measurement.
		knowledge = capabilityKnowledgeMeasured
	}
	return CapabilityFact{
		Knowledge: knowledge,
		Value:     fmt.Sprintf("%g", entry.Watts()),
		Source:    entry.Provenance(),
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// capabilitySnapshotJSON is the durable body stored in
// worker_capability_snapshots.snapshot. Keeping it as the sealed NodeCapability
// JSON makes reload trivial and digest-verifiable.
func capabilitySnapshotJSON(c NodeCapability) ([]byte, error) {
	if err := c.VerifyDigest(); err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// ParseCapabilitySnapshot reloads and verifies an append-only row body.
func ParseCapabilitySnapshot(blob []byte) (NodeCapability, error) {
	var c NodeCapability
	if err := json.Unmarshal(blob, &c); err != nil {
		return NodeCapability{}, fmt.Errorf("decode capability snapshot: %w", err)
	}
	if err := c.VerifyDigest(); err != nil {
		return NodeCapability{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Lossless inventory helpers (used by tests + migration register)
// ---------------------------------------------------------------------------

// capabilityLegacyInventory lists every registration-path fact Step 6 must
// round-trip. Keys are stable names from the capability map inventory.
func capabilityLegacyInventory(cap WorkerCapability, nc NodeCapability) map[string]string {
	out := map[string]string{
		"hw_class":              nc.HWClass.Value,
		"hardware_identity":     nc.HardwareIdentity.Value,
		"engine":                nc.Engine.Value,
		"memory_gb":             fmt.Sprintf("%g", nc.MemoryGB.Value),
		"memory_bw_gbps":        fmt.Sprintf("%g", nc.MemoryBwGbps.Value),
		"gpu_count":             fmt.Sprintf("%d", nc.GPUCount.Value),
		"memory_gb_per_gpu":     fmt.Sprintf("%g", nc.MemoryGBPerGPU.Value),
		"interconnect":          nc.Interconnect.Value,
		"os_version":            nc.OSVersion.Value,
		"agent_version":         nc.AgentVersion.Value,
		"build_hash":            nc.BuildHash.Value,
		"build_identity_policy": nc.BuildIdentityPolicy.Value,
		"supported_jobs":        strings.Join(nc.SupportedJobs, ","),
		"supported_models":      strings.Join(nc.SupportedModels, ","),
		"sandboxed":             nc.Sandboxed.Value,
		"unsandboxed_opt_in":    nc.UnsandboxedOptIn.Value,
		"thermal_ok":            nc.ThermalOK.Value,
		"failure_domain":        nc.FailureDomain.Value,
		"interruption_policy":   nc.InterruptionPolicy.Value,
		"region_knowledge":      nc.Region.Knowledge,
		"disk_knowledge":        nc.DiskGB.Knowledge,
		"benchmark_count":       fmt.Sprintf("%d", len(nc.Benchmarks)),
		"runtime_cell_count":    fmt.Sprintf("%d", len(nc.RuntimeCells)),
		// Explicitly NOT in NodeCapability (policy / other authority):
		"min_payout_usd_hr_excluded": fmt.Sprintf("%g", cap.MinPayoutUsdHr),
	}
	return out
}
