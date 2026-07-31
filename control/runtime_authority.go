package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed runtime-authority.json
var runtimeAuthorityJSON []byte

type authorityModel struct {
	ID          string  `json:"id"`
	WireKind    string  `json:"wire_kind"`
	Job         string  `json:"job_type"`
	MinMemoryGB float64 `json:"min_memory_gb"`
	HFRepo      string  `json:"hf_repo"`
	HFRevision  string  `json:"hf_revision"`
	// Onboarding posture. Enforced by validateModelOnboarding; see
	// control/model_onboarding.go for why each one is a hard gate.
	License             string              `json:"license"`
	LicenseURL          string              `json:"license_url"`
	CommercialUse       bool                `json:"commercial_use"`
	AttributionRequired bool                `json:"attribution_required"`
	AttributionText     string              `json:"attribution_text"`
	RemoteCode          bool                `json:"remote_code"`
	Artifacts           []authorityArtifact `json:"artifacts"`
}

// authorityArtifact is one downloadable file backing a model.
//
// WireKind exists for the same reason authorityCell.WireKind does. Format is a
// property of the (runtime, model) pair, so a model serving two runtimes carries
// the artifacts for both: all-minilm-l6-v2 ships safetensors for candle and a
// GGUF of the same logical weights for llama.cpp. An empty value inherits the
// model's declared kind, which is what every artifact meant when one format
// existed.
//
// Without this an agent asked to serve a GGUF cell would fetch — and verify the
// digest of — the safetensors it has no loader for, and a cell could declare a
// format the document has no bytes for at all.
type authorityArtifact struct {
	Repo     string `json:"repo,omitempty"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
	WireKind string `json:"wire_kind,omitempty"`
}

// artifactsFor returns the artifacts a runtime loading `kind` must fetch.
func (m authorityModel) artifactsFor(kind string) []authorityArtifact {
	out := make([]authorityArtifact, 0, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		if wireKindForArtifact(artifact, m.WireKind) == kind {
			out = append(out, artifact)
		}
	}
	return out
}

// Runtime lifecycle. The whole point of the state machine is that only the last
// two may receive buyer work, so a profile can be registered, described and
// benchmarked long before it is allowed to earn money.
//
// A benchmark script that ran outside the product moves a profile to VALIDATED
// at most. REAL_RUNTIME_PROVEN means a real engine executed real work.
// CANARY means it did so through a complete Merc contract-to-settlement chain.
const (
	runtimeLifecycleDraft             = "DRAFT"
	runtimeLifecycleValidated         = "VALIDATED"
	runtimeLifecycleRealRuntimeProven = "REAL_RUNTIME_PROVEN"
	runtimeLifecycleCanary            = "CANARY"
	runtimeLifecycleActive            = "ACTIVE"
	runtimeLifecycleQuarantined       = "QUARANTINED"
	runtimeLifecycleRetired           = "RETIRED"
)

// runtimeLifecycleRank orders the states for comparison. Terminal states
// (QUARANTINED, RETIRED) rank below DRAFT deliberately: they are not partial
// progress toward routability, they are exclusions from it.
func runtimeLifecycleRank(state string) (int, bool) {
	switch state {
	case runtimeLifecycleQuarantined, runtimeLifecycleRetired:
		return 0, true
	case runtimeLifecycleDraft:
		return 1, true
	case runtimeLifecycleValidated:
		return 2, true
	case runtimeLifecycleRealRuntimeProven:
		return 3, true
	case runtimeLifecycleCanary:
		return 4, true
	case runtimeLifecycleActive:
		return 5, true
	}
	return 0, false
}

// runtimeLifecycleRoutable reports whether a profile may receive buyer work.
func runtimeLifecycleRoutable(state string) bool {
	return state == runtimeLifecycleCanary || state == runtimeLifecycleActive
}

type authorityEngine struct {
	Engine  string `json:"engine"`
	Adapter string `json:"adapter"`
}

type authorityCell struct {
	ID     string `json:"id"`
	Job    string `json:"job"`
	Model  string `json:"model"`
	Runner string `json:"runner"`
	// WireKind is the artifact format THIS runtime loads the model from. Empty
	// inherits the model's declared kind, which is what every cell did when one
	// runtime existed.
	//
	// Format belongs to the (runtime, model) pair, not to the model. candle
	// serves all-minilm-l6-v2 from safetensors and llama.cpp serves the same
	// logical model from a GGUF; a global wire_kind cannot express that, and
	// measurement showed the two agree at 0.999999 cosine against a 0.999 gate,
	// so the difference is real and admissible rather than hypothetical.
	WireKind     string  `json:"wire_kind,omitempty"`
	MinMemoryGB  float64 `json:"min_memory_gb"`
	Verification string  `json:"verification"`

	// Lifecycle, benchmark authority and quality tier are per CELL, because the
	// evidence is per cell.
	//
	// llama_cpp_metal is the case that forced this. Its embed cell is measured
	// faster than candle at every batch size and clears the governed cosine gate
	// at 0.999998; its batch_infer cell diverges from its own serial output in
	// every batched configuration on Metal and is unusable for the byte_exact
	// contract it declares. One lifecycle on the profile would let the embedding
	// proof promote the generation cell, which is precisely the thing the
	// evidence says must not happen. The proof boundary is finer than the
	// profile, so the lifecycle has to be too.
	//
	// Empty inherits the profile's, which is what every cell meant before the
	// field existed.
	Lifecycle string `json:"lifecycle,omitempty"`
	// BenchmarkAuthority is the receipt for THIS cell. A receipt measuring
	// MiniLM embeddings is not evidence about Llama generation, and letting one
	// pointer cover both is how a runtime gets promoted on the wrong number.
	BenchmarkAuthority string `json:"benchmark_authority,omitempty"`
	QualityTier        string `json:"quality_tier,omitempty"`
	// RejectionReason is required by, and only by, REJECTED_FOR_CONTRACT. A
	// rejection without a stated reason is indistinguishable from an oversight,
	// and the next person to read it will assume it was one.
	RejectionReason string `json:"rejection_reason,omitempty"`
	// Parallelism limits this cell was actually measured under. Zero means
	// unstated rather than unlimited; the selector treats unstated as "cannot
	// promise", never as "no ceiling".
	MaxBatch       int `json:"max_batch,omitempty"`
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

// Cell lifecycle. The profile states plus one that only makes sense per cell.
//
// REJECTED_FOR_CONTRACT means measurement proved this runtime cannot satisfy the
// contract this cell sells — not that it is unfinished, and not that it might be
// promoted later once someone gets round to it. llama.cpp on Metal cannot be
// both batched and byte-deterministic, so its byte_exact cell is rejected rather
// than left at VALIDATED where it reads as work in progress.
const runtimeLifecycleRejectedForContract = "REJECTED_FOR_CONTRACT"

// cellLifecycleRank orders the cell states. REJECTED_FOR_CONTRACT ranks with the
// other terminal exclusions: it is not partial progress toward routability.
func cellLifecycleRank(state string) (int, bool) {
	if state == runtimeLifecycleRejectedForContract {
		return 0, true
	}
	return runtimeLifecycleRank(state)
}

// EffectiveLifecycle is the cell's own state, floored by its profile's.
//
// A profile cannot inflate a cell: promoting llama_cpp_metal to ACTIVE must not
// drag a REJECTED_FOR_CONTRACT cell up with it. A cell cannot outrank its
// profile either — a DRAFT profile has no proven anything, so a cell inside it
// claiming ACTIVE is claiming something the profile has not established.
func (c authorityCell) EffectiveLifecycle(profile authorityRuntimeProfile) string {
	own := c.Lifecycle
	if own == "" {
		return profile.Lifecycle
	}
	ownRank, ownKnown := cellLifecycleRank(own)
	profileRank, profileKnown := cellLifecycleRank(profile.Lifecycle)
	if !ownKnown || !profileKnown {
		return own
	}
	if ownRank <= profileRank {
		return own
	}
	return profile.Lifecycle
}

// Routable reports whether this cell may take ORDINARY buyer work.
//
// REAL_RUNTIME_PROVEN is deliberately not routable. It means a real engine
// executed real work through the complete Merc chain, which is the evidence a
// promotion argument is built FROM, not the promotion itself. Reaching it
// through explicit operator or test routing is how the next evidence gets
// collected; reaching it through ordinary buyer traffic would be the promotion
// happening by accident.
func (c authorityCell) Routable(profile authorityRuntimeProfile) bool {
	return runtimeLifecycleRoutable(c.EffectiveLifecycle(profile))
}

// ReachableByDirectedRouting reports whether an operator or a test may force
// work onto this cell by naming it explicitly.
//
// VALIDATED and above, terminal states excluded. The floor is VALIDATED rather
// than REAL_RUNTIME_PROVEN because of the order evidence is actually collected
// in: a cell reaches REAL_RUNTIME_PROVEN BY being driven through the complete
// Merc chain, so requiring it beforehand would make the state unreachable and
// the whole lifecycle a dead end. DRAFT is excluded because a draft cell has not
// been measured at all, and terminal states because they are exclusions.
//
// This is not routability. Nothing here reaches ordinary buyer traffic: the
// advertised catalogue stays routable-only, and a directed cell is reached only
// when an operator or a test freezes it into a workload decision.
func (c authorityCell) ReachableByDirectedRouting(profile authorityRuntimeProfile) bool {
	return directedReachableLifecycle(c.EffectiveLifecycle(profile))
}

// directedReachableLifecycle is the predicate itself, so activation policy can
// apply it to a stored lifecycle without reconstructing a document cell.
func directedReachableLifecycle(state string) bool {
	validated, _ := cellLifecycleRank(runtimeLifecycleValidated)
	rank, known := cellLifecycleRank(state)
	if !known {
		return false
	}
	switch state {
	case runtimeLifecycleQuarantined, runtimeLifecycleRetired,
		runtimeLifecycleRejectedForContract:
		return false
	}
	return rank >= validated
}

// benchmarkAuthorityFor resolves the cell's own receipt, falling back to the
// profile's.
func (c authorityCell) benchmarkAuthorityFor(profile authorityRuntimeProfile) string {
	if c.BenchmarkAuthority != "" {
		return c.BenchmarkAuthority
	}
	return profile.BenchmarkAuthority
}

// qualityTierFor resolves the cell's own tier, falling back to the profile's.
func (c authorityCell) qualityTierFor(profile authorityRuntimeProfile) string {
	if c.QualityTier != "" {
		return c.QualityTier
	}
	return profile.QualityTier
}

// knownWireKind is the closed set an agent can actually load.
func knownWireKind(kind string) bool { return kind == "gguf" || kind == "hf" }

// wireKindFor resolves a cell's artifact format, falling back to the model's.
func wireKindFor(cell authorityCell, modelKind string) string {
	if cell.WireKind != "" {
		return cell.WireKind
	}
	return modelKind
}

// wireKindForArtifact resolves an artifact's format, falling back to the model's.
func wireKindForArtifact(artifact authorityArtifact, modelKind string) string {
	if artifact.WireKind != "" {
		return artifact.WireKind
	}
	return modelKind
}

type authorityRuntimeProfile struct {
	RuntimeID string `json:"runtime_id"`
	// Revision makes profile CONTENT immutable under a stable identity. A
	// profile that changes which model, quantization, hardware or capability it
	// means must take a new revision; keeping r1 while its meaning changes would
	// make every receipt, benchmark and calibration bucket that named r1
	// ambiguous after the fact.
	//
	// The identity is (runtime_id, revision) and the content is bound by
	// ContentDigest. runtime_id alone stays stable so task provenance rows and
	// frozen workload decisions that carry it keep meaning what they said.
	Revision string `json:"revision"`
	// SupersededBy names the runtime_id that replaced this one, or is empty.
	// It is excluded from the content digest: superseding is something that
	// happens TO a profile, not part of what the profile means.
	SupersededBy   string `json:"superseded_by"`
	Engine         string `json:"engine"`
	EngineRevision string `json:"engine_revision"`
	// Tokenizer and chat template are part of what a profile MEANS: identical
	// weights under a different template are a different product, and a
	// benchmark taken under one does not transfer to the other.
	TokenizerRevision string `json:"tokenizer_revision"`
	ChatTemplateID    string `json:"chat_template_id"`
	// SourceIdentity records where this profile came from. Two documents could
	// otherwise define byte-identical content and be indistinguishable in
	// provenance.
	SourceIdentity string `json:"source_identity"`
	Adapter        string `json:"adapter"`
	// Lifecycle is excluded from the content digest. A profile is expected to
	// move VALIDATED to REAL_RUNTIME_PROVEN to CANARY to ACTIVE without becoming
	// a different profile; that progression is the whole point of the state.
	Lifecycle string `json:"lifecycle"`
	Device    string `json:"device"`
	Hardware  struct {
		Platforms   []string `json:"platforms"`
		DeviceCount struct {
			Minimum int `json:"minimum"`
			Maximum int `json:"maximum"`
		} `json:"device_count"`
	} `json:"hardware"`
	Parallelism struct {
		ContinuousBatching bool `json:"continuous_batching"`
		TensorParallel     bool `json:"tensor_parallel"`
		PipelineParallel   bool `json:"pipeline_parallel"`
		DataParallel       bool `json:"data_parallel"`
	} `json:"parallelism"`
	Capabilities struct {
		Streaming   bool `json:"streaming"`
		PrefixCache bool `json:"prefix_cache"`
		Speculation bool `json:"speculation"`
	} `json:"capabilities"`
	BenchmarkAuthority string          `json:"benchmark_authority"`
	QualityTier        string          `json:"quality_tier"`
	Evidence           []string        `json:"evidence"`
	Cells              []authorityCell `json:"cells"`
}

// runtimeProfileRevisionPattern is deliberately narrow. A revision is a counter,
// not a description: "r2" sorts and compares, "v2-mixed-bit-retune" invites
// someone to edit the meaning without changing the string.
var runtimeProfileRevisionPattern = regexp.MustCompile(`^r[1-9][0-9]*$`)

// The profile content digest lives in capability_manifest.go as CapabilityDigest.
//
// It used to be computed here, over the decoded struct with Lifecycle and
// SupersededBy blanked. That was correct for those two fields and wrong by
// construction for everything added afterwards: the per-cell lifecycle landed
// later and went straight into the digest, so promoting llama.cpp's embed cell
// forced a revision bump and, because the agent binds the same document at
// compile time, a rebuild of every agent in the fleet.
//
// authorityModels indexes the document's model catalogue for digest resolution.
func (d runtimeAuthorityDocument) authorityModels() map[string]authorityModel {
	out := make(map[string]authorityModel, len(d.Models))
	for _, model := range d.Models {
		out[model.ID] = model
	}
	return out
}

// declaredCapabilities lists the capability and parallelism flags this profile
// claims. A claim is a promise about behaviour, so an unproven profile may not
// make one.
func (p authorityRuntimeProfile) declaredCapabilities() []string {
	var claimed []string
	for name, on := range map[string]bool{
		"parallelism.continuous_batching": p.Parallelism.ContinuousBatching,
		"parallelism.tensor_parallel":     p.Parallelism.TensorParallel,
		"parallelism.pipeline_parallel":   p.Parallelism.PipelineParallel,
		"parallelism.data_parallel":       p.Parallelism.DataParallel,
		"capabilities.streaming":          p.Capabilities.Streaming,
		"capabilities.prefix_cache":       p.Capabilities.PrefixCache,
		"capabilities.speculation":        p.Capabilities.Speculation,
	} {
		if on {
			claimed = append(claimed, name)
		}
	}
	sort.Strings(claimed)
	return claimed
}

type runtimeAuthorityDocument struct {
	SchemaVersion int                       `json:"schema_version"`
	MatrixVersion string                    `json:"matrix_version"`
	Engines       []authorityEngine         `json:"engines"`
	Models        []authorityModel          `json:"models"`
	Runtimes      []authorityRuntimeProfile `json:"runtimes"`
}

// RoutableRuntimes returns the profiles allowed to receive buyer work.
func (d runtimeAuthorityDocument) RoutableRuntimes() []authorityRuntimeProfile {
	out := make([]authorityRuntimeProfile, 0, len(d.Runtimes))
	for _, profile := range d.Runtimes {
		if runtimeLifecycleRoutable(profile.Lifecycle) {
			out = append(out, profile)
		}
	}
	return out
}

type generatedRuntimeCapability struct {
	ID              string
	Runtime         string
	Engine          string
	Device          string
	HardwareClasses []string
	Job             string
	Model           string
	ModelKind       string
	Runner          string
	MinMemoryGB     float64
	Verification    string
}

var (
	runtimeAuthority              = loadRuntimeAuthority()
	generatedRuntimeMatrixVersion = runtimeAuthority.MatrixVersion
	// The CAPABILITY digest, not the file digest. Everything that consumes this
	// — worker_authorized_capabilities.matrix_sha256, tasks.runtime_matrix_sha256,
	// the agent's own dispatch check — is asking "are we describing the same
	// runtimes", and only capability answers that. Promoting a cell leaves it
	// unchanged, which is the whole point.
	generatedRuntimeMatrixSHA256 = runtimeCapabilityMatrixDigest()
	// The document's byte identity, kept for provenance in receipts and for the
	// checkpoint gate. Never used as dispatch identity.
	generatedRuntimeAuthorityFileSHA256 = runtimeAuthorityFileSHA256()
	// The model catalogue every CapabilityDigest resolves its artifacts against.
	runtimeAuthorityModels = runtimeAuthority.authorityModels()
	// Every cell the document declares, lifecycle-blind. This is CAPABILITY: what
	// the fleet is physically able to do. It is a compile-time constant on
	// purpose, because capability is exactly the half that does not move without a
	// new revision. It is never a catalogue and never an authorization — enrolment
	// validates a worker's declaration against it and then authorizes only what
	// activation policy allows.
	generatedCapabilityRuntimeCells = projectCells(runtimeAuthority,
		func(authorityRuntimeProfile, authorityCell) bool { return true })
)

// The advertised and directed projections used to be package variables computed
// once from the document. They are now derived from the ACTIVATION POLICY the
// process is operating under — see advertisedRuntimeCapabilities() and
// directedRuntimeCapabilities() in activation_policy.go — because a promotion is
// a policy write, and a projection frozen at process start would keep serving the
// state the document happened to declare when the binary was built.

func loadRuntimeAuthority() runtimeAuthorityDocument {
	var authority runtimeAuthorityDocument
	if err := json.Unmarshal(runtimeAuthorityJSON, &authority); err != nil {
		panic(fmt.Sprintf("decode embedded runtime authority: %v", err))
	}
	if err := validateRuntimeAuthorityDocument(authority); err != nil {
		panic(fmt.Sprintf("embedded runtime authority: %v", err))
	}
	if err := validateModelOnboarding(authority); err != nil {
		// Fail closed at process start. A model whose licence merc cannot
		// resell under, or which wants to run repo-supplied code on a
		// supplier's machine, must not reach a catalogue that suppliers serve
		// and buyers are charged for.
		panic(fmt.Sprintf("runtime authority onboarding policy: %v", err))
	}
	return authority
}

// validateRuntimeAuthorityDocument is the governed replacement for the old
// "exactly two models and two cells" check.
//
// That check was fail-closed and correct for a single-runtime product, but it
// had hardened into a singleton: it made registering a second runtime a panic
// rather than a decision, so Merc could not represent a runtime choice at all.
// The replacement keeps every guarantee the count check was standing in for
// without pinning the shape of the fleet:
//
//   - every publicly admitted model reaches at least one ROUTABLE profile, so a
//     model can never be sellable with no way to serve it;
//   - profile identity and cell identity are unique among routable profiles, so
//     two profiles cannot both claim to be the authority for one cell;
//   - every profile names an engine in the registry and an adapter that engine
//     declares, so an unknown runtime cannot be admitted by spelling;
//   - a profile may not claim a capability its lifecycle has not proven;
//   - a routable profile must name its benchmark authority and declare a quality
//     tier, because routing work to an unmeasured runtime is the failure mode
//     this whole structure exists to prevent;
//   - no cell references a model the document does not define.
func validateRuntimeAuthorityDocument(authority runtimeAuthorityDocument) error {
	if authority.SchemaVersion != 2 {
		return fmt.Errorf("unsupported schema_version %d (want 2)", authority.SchemaVersion)
	}
	if authority.MatrixVersion == "" {
		return errors.New("matrix_version is empty")
	}
	if len(authority.Models) == 0 {
		return errors.New("document defines no models")
	}
	if len(authority.Runtimes) == 0 {
		return errors.New("document defines no runtimes")
	}

	adapters := make(map[string]string, len(authority.Engines))
	for _, engine := range authority.Engines {
		if engine.Engine == "" || engine.Adapter == "" {
			return errors.New("engine registry contains an incomplete entry")
		}
		if _, dup := adapters[engine.Engine]; dup {
			return fmt.Errorf("engine %q is registered twice", engine.Engine)
		}
		adapters[engine.Engine] = engine.Adapter
	}

	modelsByID := make(map[string]authorityModel, len(authority.Models))
	for _, model := range authority.Models {
		modelsByID[model.ID] = model
		for _, artifact := range model.Artifacts {
			if !knownWireKind(wireKindForArtifact(artifact, model.WireKind)) {
				return fmt.Errorf("model %q artifact %q declares unknown wire kind %q",
					model.ID, artifact.Path, artifact.WireKind)
			}
		}
	}

	seenRuntime := make(map[string]bool, len(authority.Runtimes))
	seenRoutableCell := make(map[string]string)
	servedModels := make(map[string]bool, len(authority.Models))

	for _, profile := range authority.Runtimes {
		if profile.RuntimeID == "" {
			return errors.New("a runtime profile has no runtime_id")
		}
		if seenRuntime[profile.RuntimeID] {
			return fmt.Errorf("runtime profile %q is declared twice", profile.RuntimeID)
		}
		seenRuntime[profile.RuntimeID] = true
		if !runtimeProfileRevisionPattern.MatchString(profile.Revision) {
			return fmt.Errorf("runtime %q has revision %q, want r1, r2, …",
				profile.RuntimeID, profile.Revision)
		}
		if profile.SupersededBy != "" {
			if profile.SupersededBy == profile.RuntimeID {
				return fmt.Errorf("runtime %q supersedes itself", profile.RuntimeID)
			}
			// A superseded profile has been replaced. Continuing to route buyer
			// work to it means the replacement was never actually adopted.
			if runtimeLifecycleRoutable(profile.Lifecycle) {
				return fmt.Errorf("runtime %q is superseded by %q but still routable",
					profile.RuntimeID, profile.SupersededBy)
			}
		}

		rank, known := runtimeLifecycleRank(profile.Lifecycle)
		if !known {
			return fmt.Errorf("runtime %q has unknown lifecycle %q",
				profile.RuntimeID, profile.Lifecycle)
		}
		adapter, engineKnown := adapters[profile.Engine]
		if !engineKnown {
			return fmt.Errorf("runtime %q names engine %q which is not in the engine registry",
				profile.RuntimeID, profile.Engine)
		}
		if profile.Adapter != adapter {
			return fmt.Errorf("runtime %q declares adapter %q but engine %q registers %q",
				profile.RuntimeID, profile.Adapter, profile.Engine, adapter)
		}
		if profile.Device == "" || len(profile.Hardware.Platforms) == 0 {
			return fmt.Errorf("runtime %q has no device or hardware platform", profile.RuntimeID)
		}
		if profile.Hardware.DeviceCount.Minimum < 1 ||
			profile.Hardware.DeviceCount.Maximum < profile.Hardware.DeviceCount.Minimum {
			return fmt.Errorf("runtime %q has an invalid device_count range", profile.RuntimeID)
		}
		// A capability claim is a promise about behaviour, so a profile that has
		// not been observed executing real work may not make one.
		//
		// Terminal states are exempt, not because they are trusted but because
		// they are already excluded from routing. Quarantine is a statement about
		// whether a runtime may take work, not a retroactive claim that what was
		// previously observed of it never happened.
		terminal := profile.Lifecycle == runtimeLifecycleQuarantined ||
			profile.Lifecycle == runtimeLifecycleRetired
		provenRank, _ := runtimeLifecycleRank(runtimeLifecycleRealRuntimeProven)
		if !terminal && rank < provenRank {
			if claimed := profile.declaredCapabilities(); len(claimed) > 0 {
				return fmt.Errorf(
					"runtime %q is %s but claims unproven capabilities %v",
					profile.RuntimeID, profile.Lifecycle, claimed)
			}
		}
		if len(profile.Cells) == 0 {
			return fmt.Errorf("runtime %q declares no cells", profile.RuntimeID)
		}
		profileRoutable := runtimeLifecycleRoutable(profile.Lifecycle)
		if profileRoutable && (profile.BenchmarkAuthority == "" || profile.QualityTier == "") {
			return fmt.Errorf(
				"runtime %q is routable without a benchmark authority and quality tier",
				profile.RuntimeID)
		}
		if err := validateBenchmarkAuthorityBinding(profile); err != nil {
			return err
		}

		seenCellInProfile := make(map[string]bool, len(profile.Cells))
		for _, cell := range profile.Cells {
			if cell.ID == "" || cell.Job == "" || cell.Model == "" ||
				cell.Runner == "" || cell.Verification == "" || cell.MinMemoryGB <= 0 {
				return fmt.Errorf("runtime %q has an incomplete cell %q",
					profile.RuntimeID, cell.ID)
			}
			if seenCellInProfile[cell.ID] {
				return fmt.Errorf("runtime %q declares cell %q twice", profile.RuntimeID, cell.ID)
			}
			seenCellInProfile[cell.ID] = true
			model, modelKnown := modelsByID[cell.Model]
			if !modelKnown {
				return fmt.Errorf("runtime %q cell %q references undefined model %q",
					profile.RuntimeID, cell.ID, cell.Model)
			}
			if cell.WireKind != "" && !knownWireKind(cell.WireKind) {
				return fmt.Errorf("runtime %q cell %q declares unknown wire kind %q",
					profile.RuntimeID, cell.ID, cell.WireKind)
			}
			// A declared format with no artifacts behind it is a cell nothing can
			// serve. Before per-cell wire kinds existed this was unreachable — one
			// format meant every artifact matched it — and declaring the llama.cpp
			// GGUF embed cell made it reachable for the first time.
			cellKind := wireKindFor(cell, model.WireKind)
			if len(model.artifactsFor(cellKind)) == 0 {
				return fmt.Errorf(
					"runtime %q cell %q serves model %q as %q, which declares no %q artifact",
					profile.RuntimeID, cell.ID, cell.Model, cellKind, cellKind)
			}
			if err := validateCellAuthority(profile, cell); err != nil {
				return err
			}
			if !cell.Routable(profile) {
				continue
			}
			if owner, taken := seenRoutableCell[cell.ID]; taken {
				return fmt.Errorf("routable cell %q is claimed by both %q and %q",
					cell.ID, owner, profile.RuntimeID)
			}
			seenRoutableCell[cell.ID] = profile.RuntimeID
			servedModels[cell.Model] = true
		}
		// After the cell loop, not before it. The digest resolves every cell's
		// artifacts and so fails on an unknown wire kind or a missing artifact
		// too — with a message about hashing rather than about the cell that is
		// actually wrong. Running it last leaves the specific refusals in front.
		if _, err := profile.CapabilityDigest(modelsByID); err != nil {
			return err
		}
	}

	for _, model := range authority.Models {
		if !servedModels[model.ID] {
			return fmt.Errorf(
				"model %q is admitted but no routable runtime profile serves it", model.ID)
		}
	}
	for _, profile := range authority.Runtimes {
		if profile.SupersededBy != "" && !seenRuntime[profile.SupersededBy] {
			return fmt.Errorf("runtime %q is superseded by %q, which is not registered",
				profile.RuntimeID, profile.SupersededBy)
		}
	}
	return nil
}

// validateCellAuthority enforces the rules that only make sense per cell.
func validateCellAuthority(profile authorityRuntimeProfile, cell authorityCell) error {
	where := fmt.Sprintf("runtime %q cell %q", profile.RuntimeID, cell.ID)

	if cell.Lifecycle != "" {
		if _, known := cellLifecycleRank(cell.Lifecycle); !known {
			return fmt.Errorf("%s has unknown lifecycle %q", where, cell.Lifecycle)
		}
	}
	effective := cell.EffectiveLifecycle(profile)

	// A rejection must say what it was rejected FOR. Without a reason it reads
	// as an oversight, and the next person to look will treat it as one.
	if cell.Lifecycle == runtimeLifecycleRejectedForContract && cell.RejectionReason == "" {
		return fmt.Errorf("%s is REJECTED_FOR_CONTRACT with no rejection_reason", where)
	}
	if cell.Lifecycle != runtimeLifecycleRejectedForContract && cell.RejectionReason != "" {
		return fmt.Errorf("%s states a rejection_reason but is %q", where, cell.Lifecycle)
	}

	// Evidence is required at the point work can actually reach the cell, and
	// REAL_RUNTIME_PROVEN is such a point: directed routing sends real buyer work
	// there even though ordinary traffic does not.
	provenRank, _ := cellLifecycleRank(runtimeLifecycleRealRuntimeProven)
	rank, _ := cellLifecycleRank(effective)
	if rank >= provenRank {
		if cell.benchmarkAuthorityFor(profile) == "" {
			return fmt.Errorf("%s is %s with no benchmark authority", where, effective)
		}
		if cell.qualityTierFor(profile) == "" {
			return fmt.Errorf("%s is %s with no quality tier", where, effective)
		}
	}

	// The receipt must be evidence for THIS profile and THIS cell's model. A
	// MiniLM embedding measurement is not evidence about Llama generation, and
	// the whole reason cells have their own authority is to stop one standing in
	// for the other.
	if authority := cell.benchmarkAuthorityFor(profile); authority != "" {
		receipt, known := benchmarkAuthorityManifest[authority]
		if !known {
			return fmt.Errorf("%s names benchmark authority %q, which is not a known receipt",
				where, authority)
		}
		if !receipt.isEvidenceFor(profile.RuntimeID) {
			return fmt.Errorf("%s names benchmark authority %q, which is evidence for %v",
				where, authority, receipt.RuntimeProfileIDs)
		}
		if len(receipt.ModelIDs) > 0 && !receipt.measures(cell.Model) {
			return fmt.Errorf(
				"%s serves model %q but its benchmark authority %q measures %v",
				where, cell.Model, authority, receipt.ModelIDs)
		}
		// The measured determinism decides which verification contracts this
		// cell may sell, at every level work can reach it — not only when it is
		// routable. Directed routing is real buyer work.
		if rank >= provenRank && cell.Verification == "byte_exact" && !receipt.ByteDeterministic {
			return fmt.Errorf(
				"%s is verified byte_exact and %s, but its benchmark authority records "+
					"that it is not byte-deterministic", where, effective)
		}
	}

	if cell.MaxBatch < 0 || cell.MaxConcurrency < 0 {
		return fmt.Errorf("%s declares a negative parallelism limit", where)
	}
	return nil
}

// runtimeAuthorityFileSHA256 digests the raw document bytes. It answers "is this
// the same file", which is a provenance question, and it is NOT what agents and
// the control plane agree on: it moves on whitespace and on any activation-policy
// edit, so using it for dispatch identity made a lifecycle promotion a fleet-wide
// binary deploy.
func runtimeAuthorityFileSHA256() string {
	sum := sha256.Sum256(runtimeAuthorityJSON)
	return hex.EncodeToString(sum[:])
}

// runtimeCapabilityMatrixDigest is the dispatch identity. Fails closed at process
// start, alongside the rest of the document validation.
func runtimeCapabilityMatrixDigest() string {
	digest, err := capabilityMatrixDigest(runtimeAuthority)
	if err != nil {
		panic(fmt.Sprintf("embedded runtime authority capability digest: %v", err))
	}
	return digest
}

// documentRoutableCells and documentDirectedCells project straight from the
// document, ignoring stored policy. They are what the process runs under before
// any policy is loaded and what the document-level tests assert against; live
// serving reads the activation snapshot instead.
func documentDirectedCells() []generatedRuntimeCapability {
	return projectCells(runtimeAuthority, func(p authorityRuntimeProfile, c authorityCell) bool {
		return c.ReachableByDirectedRouting(p)
	})
}

func documentRoutableCells() []generatedRuntimeCapability {
	return projectCells(runtimeAuthority, func(p authorityRuntimeProfile, c authorityCell) bool {
		return c.Routable(p)
	})
}

func projectCells(
	authority runtimeAuthorityDocument,
	include func(authorityRuntimeProfile, authorityCell) bool,
) []generatedRuntimeCapability {
	models := make(map[string]struct {
		kind string
		job  string
		min  float64
	}, len(authority.Models))
	for _, model := range authority.Models {
		if model.ID == "" || model.WireKind == "" || model.Job == "" || model.MinMemoryGB <= 0 ||
			model.HFRepo == "" || len(model.HFRevision) != 40 || len(model.Artifacts) == 0 {
			panic("embedded runtime authority contains an invalid model")
		}
		for _, artifact := range model.Artifacts {
			if artifact.Path == "" || artifact.Bytes <= 0 || len(artifact.SHA256) != 64 {
				panic("embedded runtime authority contains an invalid model artifact")
			}
			if _, err := hex.DecodeString(artifact.SHA256); err != nil {
				panic("embedded runtime authority contains a non-hex model digest")
			}
			if (artifact.Repo == "") != (artifact.Revision == "") ||
				(artifact.Revision != "" && len(artifact.Revision) != 40) {
				panic("embedded runtime authority contains an invalid alternate artifact revision")
			}
		}
		if _, exists := models[model.ID]; exists {
			panic("embedded runtime authority contains a duplicate model")
		}
		models[model.ID] = struct {
			kind string
			job  string
			min  float64
		}{model.WireKind, model.Job, model.MinMemoryGB}
	}
	// ONLY routable CELLS are projected, not routable profiles.
	//
	// This is the property that makes registering a runtime safe: a VALIDATED or
	// DRAFT cell is fully described, addressable and comparable, and still cannot
	// be advertised to a worker, quoted to a buyer, or matched by admission.
	// Making it per cell is what stops llama.cpp's proven embedding cell from
	// dragging its rejected generation cell into the advertised set with it.
	var capabilities []generatedRuntimeCapability
	// Keyed by (runtime, cell), not by cell id alone.
	//
	// The document validator deliberately permits two NON-ROUTABLE profiles to
	// describe the same cell id — that is how a challenger runtime is registered
	// against an incumbent's cell at all — so a set keyed on the id alone panics
	// at process start the first time a filter admits both. Routable collisions
	// are still refused, by the validator, where the rule belongs.
	seen := make(map[[2]string]bool)
	for _, profile := range authority.Runtimes {
		for _, cell := range profile.Cells {
			if !include(profile, cell) {
				continue
			}
			model, ok := models[cell.Model]
			key := [2]string{profile.RuntimeID, cell.ID}
			if seen[key] || !ok || cell.ID == "" || cell.Job != model.job ||
				cell.Runner != cell.Job || cell.MinMemoryGB < model.min || cell.Verification == "" {
				panic("embedded runtime authority contains an invalid cell")
			}
			seen[key] = true
			capabilities = append(capabilities, generatedRuntimeCapability{
				ID: cell.ID, Runtime: profile.RuntimeID, Engine: profile.Engine,
				Device: profile.Device, HardwareClasses: profile.Hardware.Platforms,
				Job: cell.Job, Model: cell.Model,
				ModelKind:   wireKindFor(cell, model.kind),
				Runner:      cell.Runner,
				MinMemoryGB: cell.MinMemoryGB, Verification: cell.Verification,
			})
		}
	}
	return capabilities
}

func syncRuntimeCatalog(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
WITH desired AS (
    SELECT * FROM jsonb_to_recordset(($1::jsonb)->'models') AS model(
        id text, family text, quant text, kind text, dim int, job_type text,
        price_per_1k numeric, min_memory_gb real, hf_repo text
    )
), upserted AS (
    INSERT INTO models (id, family, quant, kind, dim, job_type, price_per_1k, min_memory_gb, hf_repo)
    SELECT id, family, quant, kind, dim, job_type, price_per_1k, min_memory_gb, hf_repo
      FROM desired
    ON CONFLICT (id) DO UPDATE SET
        family=EXCLUDED.family, quant=EXCLUDED.quant, kind=EXCLUDED.kind,
        dim=EXCLUDED.dim, job_type=EXCLUDED.job_type,
        -- Runtime synchronization must not silently overwrite a versioned
        -- measured price or leave its provenance attached to seed bytes.
        min_memory_gb=EXCLUDED.min_memory_gb,
        hf_repo=EXCLUDED.hf_repo
    RETURNING id
)
DELETE FROM models WHERE id NOT IN (SELECT id FROM desired)`, runtimeAuthorityJSON)
	if err != nil {
		return fmt.Errorf("synchronize runtime model catalog: %w", err)
	}
	return nil
}

//go:embed evidence-manifest.json
var benchmarkAuthorityManifestJSON []byte

// benchmarkAuthorityManifest maps each profile's declared benchmark authority
// path to the runtime profiles that receipt is evidence for.
//
// It is embedded because the control plane must validate this at process start,
// where the repository working tree is not guaranteed to be present — a
// container ships the binary, not evidence/. TestBenchmarkManifestMatchesTheReceipts
// keeps the embedded copy honest against the files themselves.
var benchmarkAuthorityManifest = loadBenchmarkAuthorityManifest()

// benchmarkReceiptSummary is what the control plane needs to know about a
// receipt without shipping evidence/ into the container.
type benchmarkReceiptSummary struct {
	// RuntimeProfileIDs is a list because the receipt a tournament actually
	// needs is a COMPARISON: one harness, one corpus, one output contract, both
	// profiles. Splitting that into one receipt per profile to satisfy a
	// singular field would destroy the only property that makes the two numbers
	// comparable, which is that they were taken together.
	RuntimeProfileIDs []string `json:"runtime_profile_ids"`
	// ModelIDs is what the receipt actually measured. A receipt naming a profile
	// is not evidence about every cell that profile declares: the embed
	// comparison measures MiniLM and says nothing whatsoever about Llama
	// generation on the same engine. Empty means the receipt predates the field
	// and is treated as unscoped rather than as covering nothing, so an old
	// receipt does not become a hard failure the day this landed.
	ModelIDs []string `json:"model_ids,omitempty"`
	// ThroughputMeasured separates "this profile has a receipt" from "this
	// profile has a comparable throughput number".
	ThroughputMeasured bool `json:"throughput_measured"`
	// ByteDeterministic records whether the measured engine reproduced its own
	// serial output under batching. A cell whose verification is byte_exact
	// cannot be served by an engine that does not.
	//
	// For a receipt that measures several profiles this is the conjunction: a
	// comparison may only vouch for byte-determinism if every profile in it was
	// byte-deterministic, because the field is read to decide whether a profile
	// may serve a byte_exact cell.
	ByteDeterministic bool `json:"byte_deterministic"`
}

// isEvidenceFor reports whether this receipt names the given profile.
func (r benchmarkReceiptSummary) isEvidenceFor(runtimeProfileID string) bool {
	for _, id := range r.RuntimeProfileIDs {
		if id == runtimeProfileID {
			return true
		}
	}
	return false
}

// measures reports whether this receipt actually measured the given model.
func (r benchmarkReceiptSummary) measures(modelID string) bool {
	for _, id := range r.ModelIDs {
		if id == modelID {
			return true
		}
	}
	return false
}

func loadBenchmarkAuthorityManifest() map[string]benchmarkReceiptSummary {
	out := map[string]benchmarkReceiptSummary{}
	if err := json.Unmarshal(benchmarkAuthorityManifestJSON, &out); err != nil {
		panic(fmt.Sprintf("decode embedded benchmark authority manifest: %v", err))
	}
	return out
}

// validateBenchmarkAuthorityBinding closes the hole that let candle_metal claim
// routability backed by a benchmark receipt for a DIFFERENT engine.
//
// The original check was that benchmark_authority is a non-empty string. That
// is satisfied by any path, and the first thing it let through was a real file
// describing vLLM at bf16 on a different model revision, whose own
// benchmark_status said UNPROVEN. A profile's evidence must at minimum NAME the
// profile it is evidence for.
//
// It is still not proof of a measurement — a receipt can name a profile and
// measure nothing, which is exactly what candle's honest receipt does. What this
// enforces is that the pointer is not simply wrong.
func validateBenchmarkAuthorityBinding(profile authorityRuntimeProfile) error {
	if profile.BenchmarkAuthority == "" {
		return nil // only routable profiles are required to have one
	}
	receipt, known := benchmarkAuthorityManifest[profile.BenchmarkAuthority]
	if !known {
		return fmt.Errorf(
			"runtime %q names benchmark authority %q, which is not a known receipt",
			profile.RuntimeID, profile.BenchmarkAuthority)
	}
	if !receipt.isEvidenceFor(profile.RuntimeID) {
		return fmt.Errorf(
			"runtime %q names benchmark authority %q, but that receipt is evidence for %v",
			profile.RuntimeID, profile.BenchmarkAuthority, receipt.RuntimeProfileIDs)
	}
	// The determinism rule moved to validateCellAuthority, where it belongs.
	//
	// It used to read: if the PROFILE is routable and ANY of its cells is
	// byte_exact, refuse on a non-byte-deterministic receipt. That was correct
	// while a profile was the unit of promotion and wrong once the evidence
	// turned out to be per cell — it would refuse to let llama_cpp_metal's proven
	// embedding cell become routable because a different, rejected cell on the
	// same profile declares byte_exact. Punishing the proven cell for the
	// rejected one's contract is the same conflation as promoting the rejected
	// one for the proven one's evidence, in the other direction.
	//
	// The per-cell version compares each cell's own verification contract against
	// its own benchmark authority, at every level work can reach that cell.
	return nil
}

// profileThroughputIsMeasured reports whether this profile's benchmark authority
// actually contains a throughput measurement. Having a receipt and having a
// comparable number are different facts, and conflating them is how a runtime
// tournament compares a measured challenger against an unmeasured incumbent and
// calls the result evidence.
func profileThroughputIsMeasured(profile authorityRuntimeProfile) bool {
	receipt, known := benchmarkAuthorityManifest[profile.BenchmarkAuthority]
	return known && receipt.isEvidenceFor(profile.RuntimeID) && receipt.ThroughputMeasured
}
