package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The immutable capability manifest, split out from the mutable activation
// policy.
//
// Promoting llama_cpp_metal's embed cell from VALIDATED to REAL_RUNTIME_PROVEN
// broke every agent at once. The agent embeds runtime-authority.json at COMPILE
// time and hashes the raw file bytes; a lifecycle edit moves those bytes, so the
// control plane and the agent started describing different matrices and every
// enrolment and dispatch was refused until the binary was rebuilt. A lifecycle
// promotion had become a coordinated binary deploy, which is precisely the thing
// a lifecycle is supposed to let you avoid.
//
// The digest an agent and the control plane must agree on is the CAPABILITY set:
// which engine, which adapter, which weights, which tokenizer, which hardware,
// which cells, which formats. None of that changes when a cell is promoted. What
// does change — lifecycle, routability, quality tier, the receipt that justified
// the promotion, the rejection reason — is activation policy, which the control
// plane owns at runtime and which an agent must never need to know at build time.
//
// So the digest is computed over an EXPLICIT projection rather than over the
// struct with a few fields blanked. That direction matters: with the blanking
// approach every field added later is capability by default and silently welds
// itself to the agent binary, which is exactly how cell lifecycle got into the
// old digest without anyone deciding it should be there. Here a new field is
// excluded until someone writes a line for it.

// capabilityManifestVersion identifies the DEFINITION of the digest, not the
// content it covers. It exists so a database written under the old whole-struct
// digest can be recognised and upgraded instead of being reported as content
// drift — the digest changed meaning, the profiles did not change content.
//
// v1: the whole decoded profile struct with lifecycle and superseded_by blanked.
// v2: this explicit capability projection.
const capabilityManifestVersion = 2

// capabilityFieldSep separates fields within one canonical line. ASCII unit
// separator, which cannot occur in an identifier, path, digest or platform name,
// so no value can forge a field boundary.
const capabilityFieldSep = "\x1f"

// capabilityLine joins one canonical record. Every value is already a string by
// the time it gets here: formatting rules live at the call sites so that the Rust
// implementation can be read against the same list.
func capabilityLine(fields ...string) string {
	return strings.Join(fields, capabilityFieldSep)
}

// capabilitySourceIdentity keeps a repository layout move from changing the
// identity of an otherwise identical runtime profile. The source identity is
// provenance, but these two values name the same authority document across the
// pre-canonical and canonical trees. Arbitrary provenance values remain fully
// digest-bound.
func capabilitySourceIdentity(value string) string {
	switch value {
	case "control/runtime-authority.json", "src/control/runtime-authority.json":
		return "control/runtime-authority.json"
	default:
		return value
	}
}

// capabilityFloat formats a float identically in Go and Rust.
//
// This is the one place the two implementations could silently disagree.
// Go marshals float64(2) as "2" and Rust marshals f64 2.0 as "2.0", so a
// JSON-based canonical form would produce two different digests for one document
// and the disagreement would surface as every agent being refused. A fixed
// six-decimal format is "2.000000" in both languages.
func capabilityFloat(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

func capabilityBool(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// capabilityDigestOf hashes a set of canonical lines. The lines are sorted, so
// the order they were emitted in — which is the order the JSON document happens
// to list things in — cannot change the answer.
func capabilityDigestOf(lines []string) string {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, line := range sorted {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// capabilityVersionLine binds the digest DEFINITION into the digest itself, so
// two definitions cannot produce the same value for different meanings.
func capabilityVersionLine() string {
	return capabilityLine("capability-manifest-version", strconv.Itoa(capabilityManifestVersion))
}

// resolvedArtifactRef names the exact bytes, with the provenance that found them.
// Digest last: the identity is the content, repo/revision/path are how you get it.
func resolvedArtifactRef(model authorityModel, artifact authorityArtifact) string {
	repo, revision := artifact.Repo, artifact.Revision
	if repo == "" {
		repo, revision = model.HFRepo, model.HFRevision
	}
	return fmt.Sprintf("%s@%s/%s:%s", repo, revision, artifact.Path, artifact.SHA256)
}

// capabilityCellLines emits the canonical records for one cell of one profile.
//
// The cell's own lifecycle, quality tier, benchmark authority and rejection
// reason are all absent by construction: those are the activation policy for this
// cell, and moving any of them must not change what the cell IS.
func capabilityCellLines(
	profile authorityRuntimeProfile, cell authorityCell, models map[string]authorityModel,
) ([]string, error) {
	model, ok := models[cell.Model]
	if !ok {
		return nil, fmt.Errorf("runtime profile %q cell %q names undefined model %q",
			profile.RuntimeID, cell.ID, cell.Model)
	}
	kind := wireKindFor(cell, model.WireKind)
	lines := []string{capabilityLine(
		"cell", profile.RuntimeID, cell.ID, cell.Job, cell.Model, cell.Runner, kind,
		capabilityFloat(cell.MinMemoryGB), cell.Verification,
		strconv.Itoa(cell.MaxBatch), strconv.Itoa(cell.MaxConcurrency),
	)}
	// The bytes this cell actually loads. A cell names a model by id and a model
	// id is not weights: swapping which GGUF backs the `gguf` wire kind would
	// otherwise change every executed byte under an unchanged digest.
	for _, artifact := range model.artifactsFor(kind) {
		lines = append(lines, capabilityLine(
			"cell-artifact", profile.RuntimeID, cell.ID, resolvedArtifactRef(model, artifact)))
	}
	if len(lines) == 1 {
		return nil, fmt.Errorf("runtime profile %q cell %q serves model %q as %q, "+
			"which declares no %q artifact", profile.RuntimeID, cell.ID, cell.Model, kind, kind)
	}
	return lines, nil
}

// capabilityProfileLines emits every canonical record describing one profile.
func capabilityProfileLines(
	profile authorityRuntimeProfile, models map[string]authorityModel,
) ([]string, error) {
	lines := []string{capabilityLine(
		"profile", profile.RuntimeID, profile.Revision, profile.Engine, profile.EngineRevision,
		profile.Adapter, profile.TokenizerRevision, profile.ChatTemplateID,
		capabilitySourceIdentity(profile.SourceIdentity),
		profile.Device,
		strconv.Itoa(profile.Hardware.DeviceCount.Minimum),
		strconv.Itoa(profile.Hardware.DeviceCount.Maximum),
	)}
	for _, platform := range profile.Hardware.Platforms {
		lines = append(lines, capabilityLine("profile-platform", profile.RuntimeID, platform))
	}
	// Every flag, including the false ones. declaredCapabilities lists only what
	// is claimed, which is the right shape for "may this lifecycle promise that"
	// and the wrong shape for a digest: turning a flag off would otherwise be
	// invisible.
	for _, flag := range []struct {
		name string
		on   bool
	}{
		{"capabilities.prefix_cache", profile.Capabilities.PrefixCache},
		{"capabilities.speculation", profile.Capabilities.Speculation},
		{"capabilities.streaming", profile.Capabilities.Streaming},
		{"parallelism.continuous_batching", profile.Parallelism.ContinuousBatching},
		{"parallelism.data_parallel", profile.Parallelism.DataParallel},
		{"parallelism.pipeline_parallel", profile.Parallelism.PipelineParallel},
		{"parallelism.tensor_parallel", profile.Parallelism.TensorParallel},
	} {
		lines = append(lines,
			capabilityLine("profile-flag", profile.RuntimeID, flag.name, capabilityBool(flag.on)))
	}
	for _, cell := range profile.Cells {
		cellLines, err := capabilityCellLines(profile, cell, models)
		if err != nil {
			return nil, err
		}
		lines = append(lines, cellLines...)
	}
	return lines, nil
}

// CapabilityDigest binds everything a profile MEANS and nothing about where it is
// in its lifecycle: engine and revision, adapter, tokenizer revision, chat
// template, source identity, device, hardware platforms and device count,
// parallelism, runtime features, and per cell the workload, model, runner, wire
// format, memory floor, verification contract, measured parallelism limits and
// the exact artifact bytes it resolves to.
//
// Excluded, deliberately: lifecycle (profile and cell), superseded_by, quality
// tier, benchmark authority, evidence and rejection reason. Every one of those is
// something that happens TO a profile as evidence accumulates. A profile that has
// been promoted is the same profile.
func (p authorityRuntimeProfile) CapabilityDigest(models map[string]authorityModel) (string, error) {
	lines, err := capabilityProfileLines(p, models)
	if err != nil {
		return "", err
	}
	return capabilityDigestOf(append(lines, capabilityVersionLine())), nil
}

// CellCapabilityDigest is the per-cell identity the directive asks activation
// policy to be keyed against.
//
// It includes the profile's own identity line, because a cell is not portable:
// "embed MiniLM from a GGUF at a 2 GB floor" means something different on
// llama.cpp Metal than it would on vLLM CUDA, and two profiles declaring
// identical cell rows must not produce one identity.
func (p authorityRuntimeProfile) CellCapabilityDigest(
	cell authorityCell, models map[string]authorityModel,
) (string, error) {
	lines, err := capabilityCellLines(p, cell, models)
	if err != nil {
		return "", err
	}
	lines = append(lines, capabilityVersionLine(), capabilityLine(
		"profile", p.RuntimeID, p.Revision, p.Engine, p.EngineRevision, p.Adapter,
		p.TokenizerRevision, p.ChatTemplateID, p.SourceIdentity, p.Device,
		strconv.Itoa(p.Hardware.DeviceCount.Minimum),
		strconv.Itoa(p.Hardware.DeviceCount.Maximum),
	))
	return capabilityDigestOf(lines), nil
}

// capabilityMatrixDigest is the document-wide capability identity: the value an
// agent and the control plane must agree on before a task may be dispatched.
//
// It replaces the SHA-256 of the raw file. Every consumer wanted "are we
// describing the same runtimes", and the file digest answered "is this the same
// file" — so reformatting the JSON invalidated a fleet, and so did promoting a
// cell. matrix_version is excluded for the same reason: it is a label people bump
// when policy moves.
func capabilityMatrixDigest(d runtimeAuthorityDocument) (string, error) {
	models := d.authorityModels()
	lines := []string{capabilityVersionLine()}
	for _, engine := range d.Engines {
		lines = append(lines, capabilityLine("engine", engine.Engine, engine.Adapter))
	}
	for _, model := range d.Models {
		lines = append(lines, capabilityLine(
			"model", model.ID, model.WireKind, model.Job,
			capabilityFloat(model.MinMemoryGB), model.HFRepo, model.HFRevision))
		for _, artifact := range model.Artifacts {
			lines = append(lines, capabilityLine(
				"model-artifact", model.ID,
				wireKindForArtifact(artifact, model.WireKind),
				resolvedArtifactRef(model, artifact),
				strconv.FormatInt(artifact.Bytes, 10)))
		}
	}
	for _, profile := range d.Runtimes {
		profileLines, err := capabilityProfileLines(profile, models)
		if err != nil {
			return "", err
		}
		lines = append(lines, profileLines...)
	}
	return capabilityDigestOf(lines), nil
}
