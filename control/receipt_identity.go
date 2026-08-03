package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Binding status values carried on every evidence artifact.
const (
	BindingBound      = "BOUND"
	BindingUnbound    = "UNBOUND"
	BindingSuperseded = "SUPERSEDED"
	BindingWithdrawn  = "WITHDRAWN"
)

// The eight programme identity fields. Every evidence write must populate each
// slot with either a concrete value or an explicit N/A reason. An empty string
// is not a reason and is refused.
const (
	IdentitySourceCommit        = "source_commit"
	IdentityBuildDigest         = "build_digest"
	IdentityModelArtifactDigest = "model_artifact_digest"
	IdentityImageDigest         = "image_digest"
	IdentityHarnessRevision     = "harness_revision"
	IdentityCorpusDigest        = "corpus_digest"
	IdentityExactConfig         = "exact_config"
	IdentityRawSamples          = "raw_samples"
)

// IdentitySlot is either a concrete value or an explicit not-applicable reason.
// Both empty is incomplete. Value and NA both set is also incomplete.
type IdentitySlot struct {
	Value string `json:"value,omitempty"`
	NA    string `json:"na,omitempty"`
}

// Present reports whether the slot carries a concrete value.
func (s IdentitySlot) Present() bool {
	return strings.TrimSpace(s.Value) != ""
}

// MarkedNA reports whether the slot is explicitly not applicable.
func (s IdentitySlot) MarkedNA() bool {
	return strings.TrimSpace(s.NA) != ""
}

// Complete reports whether the slot is either present or explicitly N/A.
func (s IdentitySlot) Complete() bool {
	v, n := strings.TrimSpace(s.Value), strings.TrimSpace(s.NA)
	if v != "" && n != "" {
		return false
	}
	return v != "" || n != ""
}

// IncompleteReason explains why a slot fails Complete.
func (s IdentitySlot) IncompleteReason(field string) string {
	v, n := strings.TrimSpace(s.Value), strings.TrimSpace(s.NA)
	switch {
	case v != "" && n != "":
		return field + ": both value and na set"
	case v == "" && n == "":
		return field + ": empty (need value or na reason)"
	default:
		return ""
	}
}

// ReceiptIdentity is the single producer-identity block for evidence JSON.
// New writes must go through ValidateReceiptIdentity + WriteBoundEvidenceJSON.
type ReceiptIdentity struct {
	SourceCommit        IdentitySlot `json:"source_commit"`
	BuildDigest         IdentitySlot `json:"build_digest"`
	ModelArtifactDigest IdentitySlot `json:"model_artifact_digest"`
	ImageDigest         IdentitySlot `json:"image_digest"`
	HarnessRevision     IdentitySlot `json:"harness_revision"`
	CorpusDigest        IdentitySlot `json:"corpus_digest"`
	ExactConfig         IdentitySlot `json:"exact_config"`
	RawSamples          IdentitySlot `json:"raw_samples"`
}

// IdentitySlotValue builds a present slot.
func IdentitySlotValue(v string) IdentitySlot {
	return IdentitySlot{Value: strings.TrimSpace(v)}
}

// IdentitySlotNA builds an explicit not-applicable slot. reason must be non-empty.
func IdentitySlotNA(reason string) IdentitySlot {
	return IdentitySlot{NA: strings.TrimSpace(reason)}
}

// slots returns field name → slot for validation and missing-field lists.
func (id ReceiptIdentity) slots() []struct {
	name string
	slot IdentitySlot
} {
	return []struct {
		name string
		slot IdentitySlot
	}{
		{IdentitySourceCommit, id.SourceCommit},
		{IdentityBuildDigest, id.BuildDigest},
		{IdentityModelArtifactDigest, id.ModelArtifactDigest},
		{IdentityImageDigest, id.ImageDigest},
		{IdentityHarnessRevision, id.HarnessRevision},
		{IdentityCorpusDigest, id.CorpusDigest},
		{IdentityExactConfig, id.ExactConfig},
		{IdentityRawSamples, id.RawSamples},
	}
}

// IncompleteFields lists identity fields that are neither valued nor N/A.
func (id ReceiptIdentity) IncompleteFields() []string {
	var out []string
	for _, s := range id.slots() {
		if !s.slot.Complete() {
			out = append(out, s.name)
		}
	}
	return out
}

// ValidateReceiptIdentity refuses incomplete identity and free-string commits.
// repoRoot is used to check source_commit against real git objects.
// buildDigestPath, when non-empty, is the binary whose sha256 must equal
// BuildDigest.Value (derived, not typed).
func ValidateReceiptIdentity(repoRoot string, id ReceiptIdentity, buildDigestPath string) error {
	var errs []string
	for _, s := range id.slots() {
		if r := s.slot.IncompleteReason(s.name); r != "" {
			errs = append(errs, r)
		}
	}
	if id.SourceCommit.Present() {
		if err := validateGitObject(repoRoot, id.SourceCommit.Value); err != nil {
			errs = append(errs, "source_commit: "+err.Error())
		}
	}
	if id.BuildDigest.Present() {
		if buildDigestPath == "" {
			errs = append(errs, "build_digest: value present but no binary path was supplied for derivation check")
		} else {
			sum, err := sha256FileHex(buildDigestPath)
			if err != nil {
				errs = append(errs, "build_digest: hash binary: "+err.Error())
			} else if !strings.EqualFold(sum, strings.TrimSpace(id.BuildDigest.Value)) {
				errs = append(errs, fmt.Sprintf(
					"build_digest: value %s does not match sha256 of %s (%s)",
					id.BuildDigest.Value, buildDigestPath, sum))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("receipt identity incomplete: %s", strings.Join(errs, "; "))
	}
	return nil
}

// validateGitObject requires rev to resolve to an object in this repo.
// Free strings such as "working-tree-before-media-authority" fail.
func validateGitObject(repoRoot, rev string) error {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return fmt.Errorf("empty")
	}
	// Reject obvious non-hashes early (still allow annotated tags / short shas
	// that git can resolve — but refuse whitespace and path-like junk).
	if strings.ContainsAny(rev, " \t\n\r") {
		return fmt.Errorf("%q is not a git object", rev)
	}
	if repoRoot == "" {
		repoRoot = "."
	}
	// cat-file -e exits 0 iff the object exists.
	if _, err := gitBytes(repoRoot, "cat-file", "-e", rev+"^{object}"); err != nil {
		return fmt.Errorf("%q is not a git object in this repo", rev)
	}
	return nil
}

func sha256FileHex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DigestThisExecutable returns sha256 of the running binary (build_digest source).
func DigestThisExecutable() (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	// Resolve symlinks so go test / go run intermediate paths hash stably.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	sum, err := sha256FileHex(exe)
	if err != nil {
		return "", "", err
	}
	return sum, exe, nil
}

// ResolveRepoSourceCommit returns HEAD as a full object name, or error.
func ResolveRepoSourceCommit(repoRoot string) (string, error) {
	if repoRoot == "" {
		repoRoot = "."
	}
	out, err := gitBytes(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(out))
	if err := validateGitObject(repoRoot, head); err != nil {
		return "", err
	}
	return head, nil
}

// EvidenceWriteRequest is the single write path for bound evidence JSON.
type EvidenceWriteRequest struct {
	// RepoRoot is the git work tree used for source_commit validation.
	RepoRoot string
	// Path is the destination file (absolute or relative to process cwd).
	Path string
	// Payload is the evidence body. producer_identity and binding_status are
	// overwritten by the writer; callers must not rely on pre-set values.
	Payload map[string]any
	// Identity must be complete (value or na for every field).
	Identity ReceiptIdentity
	// BuildBinaryPath is the binary whose digest must match Identity.BuildDigest
	// when that slot is present. Empty is only legal when BuildDigest is N/A.
	BuildBinaryPath string
	// AuthorityID is required to replace a withdrawn artifact with a non-withdrawn
	// one. Empty refuses sticky-withdrawal overwrite.
	AuthorityID string
	// AllowWithdrawnWrite permits emitting binding_status=WITHDRAWN (or a payload
	// that remains withdrawn). Default false: new writes are BOUND.
	AllowWithdrawnWrite bool
}

// pathHoldsWithdrawn reports whether existing on-disk JSON is withdrawn.
func pathHoldsWithdrawn(data map[string]any) bool {
	if data == nil {
		return false
	}
	if status, _ := data["binding_status"].(string); strings.EqualFold(status, BindingWithdrawn) {
		return true
	}
	validity, _ := data["validity"].(string)
	validity = strings.ToUpper(strings.TrimSpace(validity))
	if strings.Contains(validity, "INVALIDATED") || strings.Contains(validity, "WITHDRAWN") {
		return true
	}
	return false
}

// payloadClaimsWithdrawn reports whether the new payload is itself a withdrawal.
func payloadClaimsWithdrawn(payload map[string]any, allowWithdrawn bool) bool {
	if !allowWithdrawn {
		return false
	}
	if status, _ := payload["binding_status"].(string); strings.EqualFold(status, BindingWithdrawn) {
		return true
	}
	return pathHoldsWithdrawn(payload)
}

// WriteBoundEvidenceJSON is the single write path for evidence under evidence/.
// It refuses incomplete identity, free-string source commits, undervived build
// digests, and silent replacement of withdrawn receipts.
func WriteBoundEvidenceJSON(req EvidenceWriteRequest) error {
	if req.Path == "" {
		return fmt.Errorf("evidence write: empty path")
	}
	if req.Payload == nil {
		return fmt.Errorf("evidence write: nil payload")
	}
	repoRoot := req.RepoRoot
	if repoRoot == "" {
		repoRoot = "."
	}

	if err := ValidateReceiptIdentity(repoRoot, req.Identity, req.BuildBinaryPath); err != nil {
		return fmt.Errorf("evidence write refused: %w", err)
	}

	// Sticky withdrawal: a path that currently holds a withdrawn receipt may not
	// receive a non-withdrawn replacement without a new authority id.
	if existing, err := readJSONObjectFile(req.Path); err == nil && pathHoldsWithdrawn(existing) {
		newIsWithdrawn := payloadClaimsWithdrawn(req.Payload, req.AllowWithdrawnWrite)
		if !newIsWithdrawn {
			auth := strings.TrimSpace(req.AuthorityID)
			if auth == "" {
				return fmt.Errorf(
					"evidence write refused: %s holds a withdrawn receipt; "+
						"refusing non-withdrawn overwrite without AuthorityID "+
						"(pass a new authority id to supersede deliberately)",
					req.Path)
			}
			// Record the authority that authorised the supersession.
			req.Payload["supersession_authority_id"] = auth
		}
	} else if err != nil && !os.IsNotExist(err) {
		// Unreadable existing file: fail closed rather than clobber.
		return fmt.Errorf("evidence write refused: cannot read existing %s: %w", req.Path, err)
	}

	// Assemble the on-disk object: caller payload + enforced identity + status.
	out := make(map[string]any, len(req.Payload)+4)
	for k, v := range req.Payload {
		out[k] = v
	}
	out["producer_identity"] = req.Identity
	if req.AllowWithdrawnWrite && pathHoldsWithdrawn(req.Payload) {
		out["binding_status"] = BindingWithdrawn
	} else {
		out["binding_status"] = BindingBound
	}
	// Clear any retrospective missing-field list on a bound write.
	delete(out, "missing_identity_fields")

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("evidence write: marshal: %w", err)
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
		return fmt.Errorf("evidence write: mkdir: %w", err)
	}
	if err := atomicWrite(req.Path, raw, 0o644); err != nil {
		return fmt.Errorf("evidence write: %w", err)
	}
	return nil
}

func readJSONObjectFile(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// DefaultBoundIdentity builds a complete identity for a control-plane writer.
// Non-applicable slots receive explicit N/A reasons; callers override as needed.
func DefaultBoundIdentity(repoRoot, harnessRevision, exactConfigRef, rawSamplesRef string) (ReceiptIdentity, string, error) {
	commit, err := ResolveRepoSourceCommit(repoRoot)
	if err != nil {
		return ReceiptIdentity{}, "", fmt.Errorf("source_commit: %w", err)
	}
	digest, exe, err := DigestThisExecutable()
	if err != nil {
		return ReceiptIdentity{}, "", fmt.Errorf("build_digest: %w", err)
	}
	if strings.TrimSpace(harnessRevision) == "" {
		return ReceiptIdentity{}, "", fmt.Errorf("harness_revision required")
	}
	if strings.TrimSpace(exactConfigRef) == "" {
		exactConfigRef = "embedded in receipt body"
	}
	if strings.TrimSpace(rawSamplesRef) == "" {
		rawSamplesRef = "embedded in receipt body"
	}
	id := ReceiptIdentity{
		SourceCommit:        IdentitySlotValue(commit),
		BuildDigest:         IdentitySlotValue(digest),
		ModelArtifactDigest: IdentitySlotNA("no model weights in this measurement"),
		ImageDigest:         IdentitySlotNA("no container image in this measurement"),
		HarnessRevision:     IdentitySlotValue(harnessRevision),
		CorpusDigest:        IdentitySlotNA("no external corpus in this measurement"),
		ExactConfig:         IdentitySlotValue(exactConfigRef),
		RawSamples:          IdentitySlotValue(rawSamplesRef),
	}
	return id, exe, nil
}
