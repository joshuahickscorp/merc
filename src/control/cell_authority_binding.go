package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Cell → authority artifact → validity.
//
// A lifecycle of CANARY or ACTIVE is not enough to take ordinary buyer work.
// The cell's benchmark authority must resolve, carry BOUND producer identity
// (the eight-field evidence programme bar), and must not have been withdrawn.
// Routability is this predicate, not a free-standing lifecycle field: a dead
// or merely UNBOUND receipt keeps its lifecycle but leaves the routable set
// automatically.
//
// The weaker historical bar (merc_source_commit is a real git object) is
// strictly weaker than BOUND. A receipt can clear that check and still miss
// build_digest, model_artifact_digest, corpus_digest, and the rest. Ordinary
// buyer traffic requires BOUND.
//
// The embedded evidence-manifest is the control plane's view of each receipt.
// Production containers ship the binary, not evidence/, so identity is checked
// against the manifest. TestBenchmarkManifestIdentityMatchesTheReceipts keeps
// the embedded copy honest against the files and against real git objects.

// Authority validity values that refuse any dependent cell for ordinary routing.
// Empty or "VALID" means the receipt still stands.
const (
	authorityValidityValid       = "VALID"
	authorityValidityInvalidated = "INVALIDATED"
	authorityValidityWithdrawn   = "WITHDRAWN"
	authorityValiditySuperseded  = "SUPERSEDED"
)

// hexObjectName is the shape of a git object name we will even try to resolve.
// Free strings such as "working-tree-before-media-authority" fail here without
// consulting git, so a non-object can never become a bindable commit by shape.
var hexObjectName = regexp.MustCompile(`(?i)^[0-9a-f]{7,64}$`)

// engineBuildHashPattern is the source-bound execution identity emitted by the
// agent benchmark harness and advertised by a worker. It is deliberately not a
// generic digest: the agent contract is exactly 16 lowercase hexadecimal
// characters, and accepting a free string here would let an unmeasured engine
// build inherit another build's active-hour throughput floor.
var engineBuildHashPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

const (
	currentEngineBuildIdentityPolicy  = "merc_agent_running_executable_sha256_v1"
	externalRunnerBuildIdentityPolicy = "merc_external_runner_artifact_config_sha256_v1"
)

func validCurrentEngineBuildIdentityPolicy(policy string) bool {
	return policy == currentEngineBuildIdentityPolicy ||
		policy == externalRunnerBuildIdentityPolicy
}

// historicalEngineBuildIdentityPolicyMatches lets snapshots written before
// the policy tag was introduced remain self-contained and readable. Empty is
// only accepted when every frozen copy is empty; all current admission paths
// separately require validCurrentEngineBuildIdentityPolicy, so an unversioned
// short hash can never become current authority again.
func historicalEngineBuildIdentityPolicyMatches(
	policy string,
	copies ...string,
) bool {
	if policy != "" && !validCurrentEngineBuildIdentityPolicy(policy) {
		return false
	}
	for _, copy := range copies {
		if copy != policy {
			return false
		}
	}
	return true
}

func requiredEngineBuildIdentityPolicy(
	profile authorityRuntimeProfile,
	cell authorityCell,
) string {
	if profile.Engine == "candle" && cell.Job != "media_transcode" {
		return currentEngineBuildIdentityPolicy
	}
	return externalRunnerBuildIdentityPolicy
}

// Current Apple benchmark authority binds the performance-relevant machine
// configuration, not merely a marketing generation. A same-brand lower-core
// or different-memory SKU must not inherit another device's floor.
var currentAppleHardwareIdentityPattern = regexp.MustCompile(
	`^apple_silicon_v1\|brand=[A-Za-z0-9 ._-]+\|model=[A-Za-z0-9,._-]+\|memory_bytes=[1-9][0-9]*\|cpu_cores=[1-9][0-9]*\|gpu_cores=[1-9][0-9]*$`,
)

const maxHardwareIdentityBytes = 128

// canonicalHardwareIdentity preserves the receipt/worker display identity but
// removes representation ambiguity. Exact generation remains visible (for
// example Apple M1 Ultra != Apple M3 Ultra); only repeated/edge whitespace is
// normalized. Producers must send the canonical form so comparison never
// silently repairs an authority-bearing value.
func canonicalHardwareIdentity(raw string) string {
	return strings.Join(strings.Fields(raw), " ")
}

func validCanonicalHardwareIdentity(raw string) bool {
	return raw != "" && len(raw) <= maxHardwareIdentityBytes &&
		raw == canonicalHardwareIdentity(raw)
}

func validCurrentHardwareIdentity(raw string) bool {
	return validCanonicalHardwareIdentity(raw) &&
		currentAppleHardwareIdentityPattern.MatchString(raw)
}

// weightArtifactSuffixes mark model weight bytes rather than documentation.
// A docs-only builtin cell (media contracts) does not need a weight digest on
// its receipt; a generation or embedding cell does.
var weightArtifactSuffixes = []string{
	".gguf", ".safetensors", ".bin", ".pt", ".pth", ".onnx", ".npz",
}

// cellAuthorityBindable reports whether a cell may stand on its declared
// benchmark authority for ordinary buyer work.
//
// The bar is BOUND: the receipt must clear every historical bindable check
// (resolves, measures the right model, merc_source_commit is a real git object,
// weight digests match pins, not withdrawn) AND carry binding_status=BOUND in
// the embedded manifest. BOUND means the eight programme identity fields are
// each a value or an explicit N/A with reason — see src/control/receipt_identity.go.
//
// Returns ok=false with a machine-readable reason when it may not. The reason
// is for tests and operator diagnostics; callers that only need a bool use
// cell.Routable / activation.cellRoutable.
func cellAuthorityBindable(profile authorityRuntimeProfile, cell authorityCell) (bool, string) {
	path := cell.benchmarkAuthorityFor(profile)
	if path == "" {
		return false, fmt.Sprintf("cell %q names no benchmark authority", cell.ID)
	}
	receipt, known := benchmarkAuthorityManifest[path]
	if !known {
		return false, fmt.Sprintf("benchmark authority %q is not a known receipt", path)
	}
	if !receipt.isEvidenceFor(profile.RuntimeID) {
		return false, fmt.Sprintf(
			"receipt %q is evidence for %v, not %q",
			path, receipt.RuntimeProfileIDs, profile.RuntimeID)
	}
	if len(receipt.ModelIDs) > 0 && !receipt.measures(cell.Model) {
		return false, fmt.Sprintf(
			"receipt %q measures %v, not cell model %q",
			path, receipt.ModelIDs, cell.Model)
	}
	if reason := authorityValidityRefusal(receipt.Validity); reason != "" {
		return false, fmt.Sprintf("authority %q is %s", path, reason)
	}
	if err := validateMercSourceCommit(receipt.MercSourceCommit); err != nil {
		return false, fmt.Sprintf("authority %q: %v", path, err)
	}
	if strings.TrimSpace(receipt.Harness) == "" {
		return false, fmt.Sprintf("authority %q names no harness", path)
	}
	if rev := strings.TrimSpace(receipt.ProfileRevision); rev != "" && rev != profile.Revision {
		return false, fmt.Sprintf(
			"authority %q cites profile_revision %q but profile %q is at %q",
			path, rev, profile.RuntimeID, profile.Revision)
	}
	exactPins, err := exactWeightDigestsForCell(cell, runtimeAuthorityModels)
	if err != nil {
		return false, fmt.Sprintf(
			"authority %q cannot resolve exact weight pins for cell %q: %v",
			path, cell.ID, err)
	}
	if len(exactPins) > 0 {
		if len(receipt.ModelArtifactSHA256s) == 0 {
			return false, fmt.Sprintf(
				"authority %q omits the exact %q model artifact digest that cell %q pins for %q",
				path, wireKindFor(cell, runtimeAuthorityModels[cell.Model].WireKind), cell.ID, cell.Model)
		}
		if missing := missingArtifactDigests(exactPins, receipt.ModelArtifactSHA256s); len(missing) > 0 {
			return false, fmt.Sprintf(
				"authority %q model artifact identity omits exact %q weight pin(s) %v for cell %q; a sibling format pinned for model %q is not authority for this cell",
				path, wireKindFor(cell, runtimeAuthorityModels[cell.Model].WireKind),
				missing, cell.ID, cell.Model)
		}
	}
	// BOUND is the programme bar. A receipt that only has a real merc_source_commit
	// (the historical "bindable" check) is not enough for ordinary buyer work.
	if !strings.EqualFold(strings.TrimSpace(receipt.BindingStatus), BindingBound) {
		status := strings.TrimSpace(receipt.BindingStatus)
		if status == "" {
			status = "missing"
		}
		return false, fmt.Sprintf(
			"authority %q is not BOUND (binding_status=%s); ordinary routing requires BOUND producer identity",
			path, status)
	}
	if !engineBuildHashPattern.MatchString(receipt.EngineBuildHash) {
		return false, fmt.Sprintf(
			"authority %q has engine_build_hash %q; current benchmark authority requires the exact 16-character lowercase hexadecimal execution build identity",
			path, receipt.EngineBuildHash)
	}
	requiredBuildPolicy := requiredEngineBuildIdentityPolicy(profile, cell)
	if receipt.EngineBuildIdentityPolicy != requiredBuildPolicy {
		return false, fmt.Sprintf(
			"authority %q has engine_build_identity_policy %q; current benchmark authority requires %q",
			path, receipt.EngineBuildIdentityPolicy, requiredBuildPolicy)
	}
	if !validCurrentHardwareIdentity(receipt.HardwareIdentity) {
		return false, fmt.Sprintf(
			"authority %q has hardware_identity %q; current benchmark authority requires an exact canonical Apple configuration fingerprint",
			path, receipt.HardwareIdentity)
	}
	return true, ""
}

// currentRuntimeCellBenchmarkIdentity resolves the one current benchmark
// summary for an exact cell. It returns only current bindable authority and is
// therefore never used by historical snapshot replay.
func currentRuntimeCellBenchmarkIdentity(
	cellID string,
) (authorityRuntimeProfile, authorityCell, benchmarkReceiptSummary, error) {
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			if cell.ID != cellID {
				continue
			}
			if ok, reason := cellAuthorityBindable(profile, cell); !ok {
				return authorityRuntimeProfile{}, authorityCell{}, benchmarkReceiptSummary{},
					fmt.Errorf("runtime cell %q benchmark authority is not current-bindable: %s", cellID, reason)
			}
			path := cell.benchmarkAuthorityFor(profile)
			return profile, cell, benchmarkAuthorityManifest[path], nil
		}
	}
	return authorityRuntimeProfile{}, authorityCell{}, benchmarkReceiptSummary{},
		fmt.Errorf("runtime cell %q is absent from current runtime authority", cellID)
}

// authorityValidityRefusal returns a short reason when validity forbids routing.
// Empty string means the receipt still stands (including absent validity, which
// is the historical shape of every cell benchmark receipt).
func authorityValidityRefusal(validity string) string {
	v := strings.ToUpper(strings.TrimSpace(validity))
	if v == "" || v == authorityValidityValid {
		return ""
	}
	// INVALIDATED_PENDING_RERUN and similar carry the same force as INVALIDATED.
	switch {
	case strings.Contains(v, authorityValidityInvalidated):
		return authorityValidityInvalidated
	case strings.Contains(v, authorityValidityWithdrawn):
		return authorityValidityWithdrawn
	case strings.Contains(v, authorityValiditySuperseded):
		return authorityValiditySuperseded
	default:
		// Unknown non-empty status is not a free pass: a new withdrawn spelling
		// must not keep a cell routable until someone teaches the predicate.
		return v
	}
}

// validateMercSourceCommit refuses free strings and non-objects.
//
// A missing commit is a binding failure: a receipt that does not name the source
// it was taken under cannot authorize production traffic.
func validateMercSourceCommit(commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("merc_source_commit is missing")
	}
	if strings.ContainsAny(commit, " \t\n\r/") || !hexObjectName.MatchString(commit) {
		return fmt.Errorf("merc_source_commit %q is not a git object", commit)
	}
	// Prefer a real object lookup when a work tree is available (tests, dev,
	// any host with the repo). Free strings never reach here.
	if gitObjectExists(".", commit) || gitObjectExists("..", commit) {
		return nil
	}
	// Container without .git: a well-formed object name was already shape-checked
	// and re-validated against real git by TestBenchmarkManifestIdentityMatchesTheReceipts
	// at build time. Short SHAs without a work tree are refused — they are
	// ambiguous without the object database.
	if len(commit) == 40 {
		return nil
	}
	return fmt.Errorf("merc_source_commit %q is not a git object in this repo", commit)
}

// gitObjectExists reports whether rev resolves to an object under root.
func gitObjectExists(root, rev string) bool {
	_, err := gitBytes(root, "cat-file", "-e", rev+"^{object}")
	return err == nil
}

// needsWeightArtifactDigest is true when the catalogue pins weight bytes for
// the model. Documentation-only builtin models (media contracts) do not.
func needsWeightArtifactDigest(modelID string) bool {
	model, ok := runtimeAuthorityModels[modelID]
	if !ok {
		return false
	}
	for _, artifact := range model.Artifacts {
		if isWeightArtifactPath(artifact.Path) {
			return true
		}
	}
	return false
}

func isWeightArtifactPath(path string) bool {
	lower := strings.ToLower(path)
	for _, suffix := range weightArtifactSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// exactWeightDigestsForCell returns the complete, deterministic set of weight
// bytes the selected runtime cell actually loads.
//
// A model id is not a sufficient artifact identity. all-minilm-l6-v2 is the
// concrete counterexample: Candle loads its safetensors while llama.cpp loads a
// sibling GGUF. Both are canonical artifacts for the logical model, but only
// one wire-kind set is authority for either cell. Callers must therefore resolve
// the cell's wire kind first and require every selected weight pin, which also
// prevents one shard of a multi-shard model from standing in for the full set.
//
// The models argument keeps this helper usable by document validation and tests
// that operate on an immutable authority projection rather than package globals.
func exactWeightDigestsForCell(
	cell authorityCell, models map[string]authorityModel,
) ([]string, error) {
	model, ok := models[cell.Model]
	if !ok {
		return nil, fmt.Errorf("cell %q names undefined model %q", cell.ID, cell.Model)
	}
	kind := wireKindFor(cell, model.WireKind)
	artifacts := model.artifactsFor(kind)
	if len(artifacts) == 0 {
		return nil, fmt.Errorf(
			"cell %q serves model %q as %q, which resolves no artifacts",
			cell.ID, cell.Model, kind)
	}

	modelHasWeights := false
	for _, artifact := range model.Artifacts {
		if isWeightArtifactPath(artifact.Path) {
			modelHasWeights = true
			break
		}
	}

	seen := make(map[string]struct{}, len(artifacts))
	pins := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		if !isWeightArtifactPath(artifact.Path) {
			continue
		}
		digest := strings.ToLower(strings.TrimSpace(artifact.SHA256))
		if !validSHA256(digest) {
			return nil, fmt.Errorf(
				"cell %q selected %q weight artifact %q with invalid SHA-256 pin %q",
				cell.ID, kind, artifact.Path, artifact.SHA256)
		}
		if _, duplicate := seen[digest]; duplicate {
			continue
		}
		seen[digest] = struct{}{}
		pins = append(pins, digest)
	}
	if len(pins) == 0 && modelHasWeights {
		return nil, fmt.Errorf(
			"cell %q selects %q artifacts for weight-bearing model %q but that format has no weight pin",
			cell.ID, kind, cell.Model)
	}
	sort.Strings(pins)
	return pins, nil
}

// missingArtifactDigests returns every required digest absent from cited. The
// cited receipt may cover several arms of one comparison and therefore contain
// additional exact pins; extras do not let a sibling format replace the full
// selected set.
func missingArtifactDigests(required, cited []string) []string {
	citedSet := make(map[string]struct{}, len(cited))
	for _, digest := range cited {
		digest = strings.ToLower(strings.TrimSpace(digest))
		if digest != "" {
			citedSet[digest] = struct{}{}
		}
	}
	var missing []string
	for _, digest := range required {
		digest = strings.ToLower(strings.TrimSpace(digest))
		if _, ok := citedSet[digest]; !ok {
			missing = append(missing, digest)
		}
	}
	sort.Strings(missing)
	return missing
}

// modelArtifactDigestsBound reports whether the receipt cites at least one
// weight digest the catalogue pins for the model. Receipts often list tokenizer
// and config digests alongside weights; those are not pins and are ignored.
// A receipt that cites none of the pinned weights is unbound.
func modelArtifactDigestsBound(modelID string, cited []string) bool {
	pins := pinnedWeightDigests(modelID)
	if len(pins) == 0 {
		return false
	}
	pinSet := make(map[string]bool, len(pins))
	for _, p := range pins {
		pinSet[strings.ToLower(p)] = true
	}
	for _, c := range cited {
		c = strings.ToLower(strings.TrimSpace(c))
		if c != "" && pinSet[c] {
			return true
		}
	}
	return false
}

func pinnedWeightDigests(modelID string) []string {
	model, ok := runtimeAuthorityModels[modelID]
	if !ok {
		return nil
	}
	var out []string
	for _, artifact := range model.Artifacts {
		if isWeightArtifactPath(artifact.Path) && artifact.SHA256 != "" {
			out = append(out, artifact.SHA256)
		}
	}
	return out
}

// InvalidateBenchmarkAuthority marks a receipt unusable for ordinary routing.
// Used by tests to prove automatic demotion; production withdrawals edit the
// receipt (or its manifest summary) the same way.
func InvalidateBenchmarkAuthority(path, validity string) error {
	receipt, known := benchmarkAuthorityManifest[path]
	if !known {
		return fmt.Errorf("unknown benchmark authority %q", path)
	}
	if reason := authorityValidityRefusal(validity); reason == "" {
		return fmt.Errorf("validity %q does not withdraw authority", validity)
	}
	receipt.Validity = validity
	benchmarkAuthorityManifest[path] = receipt
	return nil
}

// RestoreBenchmarkAuthorityValidity clears a test invalidation.
func RestoreBenchmarkAuthorityValidity(path, previous string) {
	receipt, known := benchmarkAuthorityManifest[path]
	if !known {
		return
	}
	receipt.Validity = previous
	benchmarkAuthorityManifest[path] = receipt
}
