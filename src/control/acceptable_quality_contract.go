package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed acceptable-quality-contracts.json
var acceptableQualityContractsJSON []byte

// AcceptableQualityContract is the durable rule that makes two runtime cells
// the same product for a buyer. Without one, a Metal-vs-CUDA price choice is a
// product choice dressed as a placement choice — the one thing merc refuses.
//
// A RuntimeDecision that routes under multi-family admission must cite
// ContractID so a receipt can prove the quality gate that authorized
// substitutability, not merely that two cells shared a model id string.
type AcceptableQualityContract struct {
	ID                       string         `json:"id"`
	JobType                  string         `json:"job_type"`
	ModelRef                 string         `json:"model_ref"`
	Status                   string         `json:"status"`
	Metric                   string         `json:"metric"`
	MetricAuthority          string         `json:"metric_authority"`
	MetricRevision           string         `json:"metric_revision"`
	Thresholds               map[string]any `json:"thresholds"`
	ReferenceCellID          string         `json:"reference_cell_id"`
	EligibleCellIDs          []string       `json:"eligible_cell_ids"`
	AllowedPrecisions        []string       `json:"allowed_precisions"`
	AllowedDevices           []string       `json:"allowed_devices"`
	MultiFamilySubstitutable bool           `json:"multi_family_substitutable"`
	HowRoutingProvesMet      string         `json:"how_routing_proves_met"`
	MeasuredEvidence         []string       `json:"measured_evidence"`
	HonestScope              string         `json:"honest_scope"`
	RefusalReason            string         `json:"refusal_reason,omitempty"`
}

type acceptableQualityContractDocument struct {
	SchemaVersion int                         `json:"schema_version"`
	Kind          string                      `json:"kind"`
	Purpose       string                      `json:"purpose"`
	Contracts     []AcceptableQualityContract `json:"contracts"`
}

var acceptableQualityContracts = loadAcceptableQualityContracts()

func loadAcceptableQualityContracts() map[string]AcceptableQualityContract {
	var doc acceptableQualityContractDocument
	if err := json.Unmarshal(acceptableQualityContractsJSON, &doc); err != nil {
		panic(fmt.Sprintf("decode acceptable quality contracts: %v", err))
	}
	if doc.Kind != "acceptable_quality_contracts" || doc.SchemaVersion != 1 {
		panic(fmt.Sprintf("acceptable quality contracts: unexpected kind/version %q/%d",
			doc.Kind, doc.SchemaVersion))
	}
	out := make(map[string]AcceptableQualityContract, len(doc.Contracts))
	for _, c := range doc.Contracts {
		if strings.TrimSpace(c.ID) == "" {
			panic("acceptable quality contracts: empty contract id")
		}
		if _, dup := out[c.ID]; dup {
			panic(fmt.Sprintf("acceptable quality contracts: duplicate id %q", c.ID))
		}
		out[c.ID] = c
	}
	return out
}

// activeQualityContractFor returns the ACTIVE multi-family-capable contract that
// covers every cell in the set for this (job, model), or ok=false.
//
// A REFUSED contract is never returned as a covering contract: it exists to
// name the pairing that must not be treated as substitutable.
func activeQualityContractFor(jobType, modelRef string, cellIDs []string) (AcceptableQualityContract, bool) {
	if len(cellIDs) == 0 {
		return AcceptableQualityContract{}, false
	}
	want := make(map[string]struct{}, len(cellIDs))
	for _, id := range cellIDs {
		want[id] = struct{}{}
	}
	var matches []AcceptableQualityContract
	for _, c := range acceptableQualityContracts {
		if c.Status != "ACTIVE" || !c.MultiFamilySubstitutable {
			continue
		}
		if c.JobType != jobType || c.ModelRef != modelRef {
			continue
		}
		eligible := make(map[string]struct{}, len(c.EligibleCellIDs))
		for _, id := range c.EligibleCellIDs {
			eligible[id] = struct{}{}
		}
		covered := true
		for id := range want {
			if _, ok := eligible[id]; !ok {
				covered = false
				break
			}
		}
		if covered {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return AcceptableQualityContract{}, false
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	return matches[0], true
}

// qualityContractByID is the receipt lookup. Unknown ids return ok=false.
func qualityContractByID(id string) (AcceptableQualityContract, bool) {
	c, ok := acceptableQualityContracts[strings.TrimSpace(id)]
	return c, ok
}

// qualityContractAuthorizingMultiFamily is the accept/placement gate: a
// multi-family eligible set may be sealed only under an ACTIVE, multi-family
// substitutable contract that names every frozen cell. A REFUSED contract
// (Metal q4 vs CUDA bf16 generation) and an unknown id are both refusals —
// a price choice dressed as a placement choice must not seal.
func qualityContractAuthorizingMultiFamily(id string, cellIDs []string) (AcceptableQualityContract, error) {
	c, ok := qualityContractByID(id)
	if !ok {
		return AcceptableQualityContract{}, fmt.Errorf("unknown quality_contract_id %q", strings.TrimSpace(id))
	}
	if c.Status != "ACTIVE" || !c.MultiFamilySubstitutable {
		return AcceptableQualityContract{}, fmt.Errorf(
			"quality_contract_id %q is not an ACTIVE multi-family contract (status=%s multi_family=%v)",
			c.ID, c.Status, c.MultiFamilySubstitutable)
	}
	if len(cellIDs) < 2 {
		return AcceptableQualityContract{}, fmt.Errorf(
			"quality_contract_id %q cannot authorize a singleton freeze", c.ID)
	}
	eligible := make(map[string]struct{}, len(c.EligibleCellIDs))
	for _, cellID := range c.EligibleCellIDs {
		eligible[cellID] = struct{}{}
	}
	for _, cellID := range cellIDs {
		if _, ok := eligible[cellID]; !ok {
			return AcceptableQualityContract{}, fmt.Errorf(
				"quality_contract_id %q does not cover cell %q", c.ID, cellID)
		}
	}
	return c, nil
}

// generationQ4VsBF16Refused reports the explicit refusal contract for the
// Metal-q4 versus CUDA-bf16 generation pairing. Named so a receipt can cite
// why multi-family generation admission did not open.
func generationQ4VsBF16Refused() AcceptableQualityContract {
	c, ok := acceptableQualityContracts["batch-infer-metal-q4-vs-cuda-bf16-REFUSED"]
	if !ok {
		panic("missing batch-infer-metal-q4-vs-cuda-bf16-REFUSED quality contract")
	}
	return c
}

// devicesAmongCapabilities returns the sorted unique device families present.
func devicesAmongCapabilities(caps []generatedRuntimeCapability) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range caps {
		d := strings.TrimSpace(c.Device)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
