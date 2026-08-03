package main

import (
	"fmt"
	"regexp"
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
	authorityValidityValid      = "VALID"
	authorityValidityInvalidated = "INVALIDATED"
	authorityValidityWithdrawn  = "WITHDRAWN"
	authorityValiditySuperseded = "SUPERSEDED"
)

// hexObjectName is the shape of a git object name we will even try to resolve.
// Free strings such as "working-tree-before-media-authority" fail here without
// consulting git, so a non-object can never become a bindable commit by shape.
var hexObjectName = regexp.MustCompile(`(?i)^[0-9a-f]{7,64}$`)

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
// each a value or an explicit N/A with reason — see control/receipt_identity.go.
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
	if needsWeightArtifactDigest(cell.Model) {
		if len(receipt.ModelArtifactSHA256s) == 0 {
			return false, fmt.Sprintf(
				"authority %q omits the model artifact digest that the authority pins for %q",
				path, cell.Model)
		}
		if !modelArtifactDigestsBound(cell.Model, receipt.ModelArtifactSHA256s) {
			return false, fmt.Sprintf(
				"authority %q model artifact digests do not match the pinned artifacts for %q",
				path, cell.Model)
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
	return true, ""
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
