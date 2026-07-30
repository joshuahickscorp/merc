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
	License             string `json:"license"`
	LicenseURL          string `json:"license_url"`
	CommercialUse       bool   `json:"commercial_use"`
	AttributionRequired bool   `json:"attribution_required"`
	AttributionText     string `json:"attribution_text"`
	RemoteCode          bool   `json:"remote_code"`
	Artifacts           []struct {
		Repo     string `json:"repo,omitempty"`
		Revision string `json:"revision,omitempty"`
		Path     string `json:"path"`
		SHA256   string `json:"sha256"`
		Bytes    int64  `json:"bytes"`
	} `json:"artifacts"`
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

// ContentDigest binds everything a profile MEANS: engine and its revision,
// tokenizer revision, chat template, adapter, source identity, device,
// hardware, device count, per-cell memory, parallelism, capabilities,
// benchmark authority, quality tier and cells. Lifecycle and SupersededBy are excluded because both are expected to
// change without the profile becoming a different profile.
//
// The digest is computed over the decoded struct, not the file bytes, so
// reformatting runtime-authority.json or reordering its keys cannot change it.
// That is the opposite of generatedRuntimeMatrixSHA256, which digests the raw
// bytes and therefore moves on whitespace: a document-level digest answers
// "is this the same file", a profile digest answers "is this the same runtime".
func (p authorityRuntimeProfile) ContentDigest() (string, error) {
	content := p
	content.Lifecycle = ""
	content.SupersededBy = ""
	blob, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("marshal runtime profile %q: %w", p.RuntimeID, err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
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
	runtimeAuthority                       = loadRuntimeAuthority()
	generatedRuntimeMatrixVersion          = runtimeAuthority.MatrixVersion
	generatedRuntimeMatrixSHA256           = runtimeAuthoritySHA256()
	generatedAdvertisedRuntimeCapabilities = projectRuntimeCapabilities(runtimeAuthority)
)

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

	modelIDs := make(map[string]bool, len(authority.Models))
	for _, model := range authority.Models {
		modelIDs[model.ID] = true
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
		if _, err := profile.ContentDigest(); err != nil {
			return err
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
		routable := runtimeLifecycleRoutable(profile.Lifecycle)
		if routable && (profile.BenchmarkAuthority == "" || profile.QualityTier == "") {
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
			if !modelIDs[cell.Model] {
				return fmt.Errorf("runtime %q cell %q references undefined model %q",
					profile.RuntimeID, cell.ID, cell.Model)
			}
			if cell.WireKind != "" && !knownWireKind(cell.WireKind) {
				return fmt.Errorf("runtime %q cell %q declares unknown wire kind %q",
					profile.RuntimeID, cell.ID, cell.WireKind)
			}
			if !routable {
				continue
			}
			if owner, taken := seenRoutableCell[cell.ID]; taken {
				return fmt.Errorf("routable cell %q is claimed by both %q and %q",
					cell.ID, owner, profile.RuntimeID)
			}
			seenRoutableCell[cell.ID] = profile.RuntimeID
			servedModels[cell.Model] = true
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

func runtimeAuthoritySHA256() string {
	sum := sha256.Sum256(runtimeAuthorityJSON)
	return hex.EncodeToString(sum[:])
}

func projectRuntimeCapabilities(authority runtimeAuthorityDocument) []generatedRuntimeCapability {
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
	// ONLY routable profiles are projected. This is the property that makes
	// registering a runtime safe: a VALIDATED or DRAFT profile is fully
	// described, addressable and comparable, and still cannot be advertised to a
	// worker, quoted to a buyer, or matched by admission. Registering MLX did not
	// widen what Merc sells by one cell.
	var capabilities []generatedRuntimeCapability
	seen := make(map[string]bool)
	for _, profile := range authority.RoutableRuntimes() {
		for _, cell := range profile.Cells {
			model, ok := models[cell.Model]
			if seen[cell.ID] || !ok || cell.ID == "" || cell.Job != model.job ||
				cell.Runner != cell.Job || cell.MinMemoryGB < model.min || cell.Verification == "" {
				panic("embedded runtime authority contains an invalid cell")
			}
			seen[cell.ID] = true
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
// path to the runtime_profile_id that receipt claims.
//
// The manifest is generated (see scripts/gen-benchmark-manifest.py) and embedded
// because the control plane must validate this at process start, where the
// repository working tree is not guaranteed to be present — a container ships
// the binary, not evidence/.
var benchmarkAuthorityManifest = loadBenchmarkAuthorityManifest()

// benchmarkReceiptSummary is what the control plane needs to know about a
// receipt without shipping evidence/ into the container.
type benchmarkReceiptSummary struct {
	RuntimeProfileID string `json:"runtime_profile_id"`
	// ThroughputMeasured separates "this profile has a receipt" from "this
	// profile has a comparable throughput number".
	ThroughputMeasured bool `json:"throughput_measured"`
	// ByteDeterministic records whether the measured engine reproduced its own
	// serial output under batching. A cell whose verification is byte_exact
	// cannot be served by an engine that does not.
	ByteDeterministic bool `json:"byte_deterministic"`
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
	if receipt.RuntimeProfileID != profile.RuntimeID {
		return fmt.Errorf(
			"runtime %q names benchmark authority %q, but that receipt is evidence for %q",
			profile.RuntimeID, profile.BenchmarkAuthority, receipt.RuntimeProfileID)
	}
	// The measurement that produced this rule: llama_cpp_metal came back 4.31x
	// faster than the incumbent at peak and diverged from its own serial output
	// at every batch size tested. The batch_infer cell declares byte_exact
	// verification. Promoting on throughput alone would have routed buyer work to
	// an engine that cannot satisfy the verification contract the cell sells.
	if runtimeLifecycleRoutable(profile.Lifecycle) && !receipt.ByteDeterministic {
		for _, cell := range profile.Cells {
			if cell.Verification == "byte_exact" {
				return fmt.Errorf(
					"runtime %q is routable and serves byte_exact cell %q, but its "+
						"benchmark authority records that it is not byte-deterministic",
					profile.RuntimeID, cell.ID)
			}
		}
	}
	return nil
}

// profileThroughputIsMeasured reports whether this profile's benchmark authority
// actually contains a throughput measurement. Having a receipt and having a
// comparable number are different facts, and conflating them is how a runtime
// tournament compares a measured challenger against an unmeasured incumbent and
// calls the result evidence.
func profileThroughputIsMeasured(profile authorityRuntimeProfile) bool {
	receipt, known := benchmarkAuthorityManifest[profile.BenchmarkAuthority]
	return known && receipt.RuntimeProfileID == profile.RuntimeID && receipt.ThroughputMeasured
}
