package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// merc dev authority — the runtime authority as the PRODUCT computes it.
//
// The state receipt used to restate digests that had been copied from a previous
// report, which is how a receipt turns into a transcript. Every value here comes
// out of the same functions admission, dispatch and enrolment use, so a receipt
// that disagrees with the product is reporting a real disagreement rather than a
// stale paste.
type authorityStateCell struct {
	CellID             string  `json:"cell_id"`
	CapabilityDigest   string  `json:"capability_digest"`
	Workload           string  `json:"workload"`
	Model              string  `json:"model"`
	WireKind           string  `json:"wire_kind"`
	Verification       string  `json:"verification"`
	DeclaredLifecycle  string  `json:"declared_lifecycle"`
	EffectiveLifecycle string  `json:"effective_lifecycle"`
	Routable           bool    `json:"routable"`
	DirectedEligible   bool    `json:"directed_eligible"`
	QualityTier        string  `json:"quality_tier"`
	BenchmarkAuthority string  `json:"benchmark_authority"`
	RejectionReason    string  `json:"rejection_reason,omitempty"`
	MinMemoryGB        float64 `json:"min_memory_gb"`
	MaxBatch           int     `json:"max_batch"`
	MaxConcurrency     int     `json:"max_concurrency"`
}

type authorityStateProfile struct {
	RuntimeProfileID string               `json:"runtime_profile_id"`
	Revision         string               `json:"revision"`
	Engine           string               `json:"engine"`
	Adapter          string               `json:"adapter"`
	Lifecycle        string               `json:"lifecycle"`
	CapabilityDigest string               `json:"capability_digest"`
	Cells            []authorityStateCell `json:"cells"`
}

type authorityState struct {
	MatrixVersion string `json:"matrix_version"`
	// CapabilityMatrixSHA256 is what agents and the control plane must agree on.
	// AuthorityDocumentSHA256 answers "is this the same file" and is provenance
	// only — conflating the two is what made a promotion a fleet rebuild.
	CapabilityMatrixSHA256    string                  `json:"capability_matrix_sha256"`
	AuthorityDocumentSHA256   string                  `json:"authority_document_sha256"`
	CapabilityManifestVersion int                     `json:"capability_manifest_version"`
	Profiles                  []authorityStateProfile `json:"profiles"`
	// Projections as the PROCESS computes them under the activation policy it is
	// operating under. With no database loaded this is the document default.
	ActivationPolicyRevision int64    `json:"activation_policy_revision"`
	AdvertisedCells          []string `json:"advertised_cells"`
	DirectedCells            []string `json:"directed_cells"`
	CapabilityCells          []string `json:"capability_cells"`
	StalePolicy              []string `json:"stale_policy,omitempty"`
}

func runDevAuthority() int {
	activation := currentActivation()
	state := authorityState{
		MatrixVersion:             generatedRuntimeMatrixVersion,
		CapabilityMatrixSHA256:    generatedRuntimeMatrixSHA256,
		AuthorityDocumentSHA256:   generatedRuntimeAuthorityFileSHA256,
		CapabilityManifestVersion: capabilityManifestVersion,
		ActivationPolicyRevision:  activation.PolicyRevision,
		StalePolicy:               activation.Stale,
	}
	for _, profile := range activation.profiles() {
		digest, err := profile.CapabilityDigest(runtimeAuthorityModels)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dev authority: %v\n", err)
			return 1
		}
		out := authorityStateProfile{
			RuntimeProfileID: profile.RuntimeID,
			Revision:         profile.Revision,
			Engine:           profile.Engine,
			Adapter:          profile.Adapter,
			Lifecycle:        profile.Lifecycle,
			CapabilityDigest: digest,
		}
		for _, cell := range profile.Cells {
			cellDigest, err := profile.CellCapabilityDigest(cell, runtimeAuthorityModels)
			if err != nil {
				fmt.Fprintf(os.Stderr, "dev authority: %v\n", err)
				return 1
			}
			model := runtimeAuthorityModels[cell.Model]
			declared := cell.Lifecycle
			if declared == "" {
				declared = "(inherits profile)"
			}
			out.Cells = append(out.Cells, authorityStateCell{
				CellID:             cell.ID,
				CapabilityDigest:   cellDigest,
				Workload:           cell.Job,
				Model:              cell.Model,
				WireKind:           wireKindFor(cell, model.WireKind),
				Verification:       cell.Verification,
				DeclaredLifecycle:  declared,
				EffectiveLifecycle: cell.EffectiveLifecycle(profile),
				Routable:           cell.Routable(profile),
				DirectedEligible:   cell.ReachableByDirectedRouting(profile),
				QualityTier:        cell.qualityTierFor(profile),
				BenchmarkAuthority: cell.benchmarkAuthorityFor(profile),
				RejectionReason:    cell.RejectionReason,
				MinMemoryGB:        cell.MinMemoryGB,
				MaxBatch:           cell.MaxBatch,
				MaxConcurrency:     cell.MaxConcurrency,
			})
		}
		state.Profiles = append(state.Profiles, out)
	}
	for _, cap := range advertisedRuntimeCapabilities() {
		state.AdvertisedCells = append(state.AdvertisedCells, cap.Runtime+"/"+cap.ID)
	}
	for _, cap := range directedRuntimeCapabilities() {
		state.DirectedCells = append(state.DirectedCells, cap.Runtime+"/"+cap.ID)
	}
	for _, cap := range generatedCapabilityRuntimeCells {
		state.CapabilityCells = append(state.CapabilityCells, cap.Runtime+"/"+cap.ID)
	}
	blob, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dev authority: %v\n", err)
		return 1
	}
	fmt.Println(string(blob))
	return 0
}
