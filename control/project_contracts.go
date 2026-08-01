package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type ProjectRuntimeContract struct {
	WorkloadKind          string `json:"workload_kind"`
	RuntimeID             string `json:"runtime_id"`
	RuntimeRevision       string `json:"runtime_revision"`
	RuntimeCellID         string `json:"runtime_cell_id"`
	RuntimeContractSHA256 string `json:"runtime_contract_sha256"`
	ModelID               string `json:"model_id"`
	ModelContractSHA256   string `json:"model_contract_sha256"`
	Verification          string `json:"verification"`
	Lifecycle             string `json:"lifecycle"`
}

func projectKindForRuntimeJob(job string) string {
	switch job {
	case "embed":
		return "embeddings"
	case "batch_infer":
		return "batch_inference"
	default:
		return ""
	}
}

func projectModelContractDigest(model authorityModel) (string, error) {
	blob, err := json.Marshal(model)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func advertisedProjectRuntimeContracts() ([]ProjectRuntimeContract, error) {
	activation := currentActivation()
	var contracts []ProjectRuntimeContract
	for _, profile := range activation.profiles() {
		for _, cell := range profile.Cells {
			if !cell.Routable(profile) {
				continue
			}
			kind := projectKindForRuntimeJob(cell.Job)
			if kind == "" {
				continue
			}
			model, ok := runtimeAuthorityModels[cell.Model]
			if !ok {
				return nil, fmt.Errorf("runtime cell %s has unknown model %s", cell.ID, cell.Model)
			}
			runtimeDigest, err := profile.CellCapabilityDigest(cell, runtimeAuthorityModels)
			if err != nil {
				return nil, err
			}
			modelDigest, err := projectModelContractDigest(model)
			if err != nil {
				return nil, err
			}
			contracts = append(contracts, ProjectRuntimeContract{
				WorkloadKind: kind, RuntimeID: profile.RuntimeID, RuntimeRevision: profile.Revision,
				RuntimeCellID: cell.ID, RuntimeContractSHA256: runtimeDigest,
				ModelID: cell.Model, ModelContractSHA256: modelDigest,
				Verification: cell.Verification, Lifecycle: cell.EffectiveLifecycle(profile),
			})
		}
	}
	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].WorkloadKind != contracts[j].WorkloadKind {
			return contracts[i].WorkloadKind < contracts[j].WorkloadKind
		}
		if contracts[i].ModelID != contracts[j].ModelID {
			return contracts[i].ModelID < contracts[j].ModelID
		}
		return contracts[i].RuntimeID < contracts[j].RuntimeID
	})
	return contracts, nil
}

func resolveDeclaredProjectContracts(ir *ProjectWorkloadIR) {
	contracts, err := advertisedProjectRuntimeContracts()
	if err != nil {
		ir.RefusalReasons = append(ir.RefusalReasons, "runtime contract authority unavailable: "+err.Error())
		ir.Unknowns = append(ir.Unknowns, "declared runtime/model contract resolution against server authority")
		return
	}
	for i := range ir.Steps {
		step := &ir.Steps[i]
		var matches []ProjectRuntimeContract
		for _, contract := range contracts {
			if contract.WorkloadKind == step.Kind &&
				contract.RuntimeContractSHA256 == step.RuntimeContract &&
				contract.ModelContractSHA256 == step.ModelContract {
				matches = append(matches, contract)
			}
		}
		if len(matches) != 1 {
			ir.RefusalReasons = append(ir.RefusalReasons, fmt.Sprintf(
				"step %s runtime/model contract pair resolved to %d routable cells", step.ID, len(matches)))
			continue
		}
		step.RuntimeID = matches[0].RuntimeID
		step.ModelID = matches[0].ModelID
		if step.Verification != matches[0].Verification {
			ir.RefusalReasons = append(ir.RefusalReasons, fmt.Sprintf(
				"step %s verification %q does not match runtime contract %q",
				step.ID, step.Verification, matches[0].Verification))
			step.RuntimeID, step.ModelID = "", ""
		}
	}
	for _, step := range ir.Steps {
		if step.RuntimeID == "" || step.ModelID == "" {
			ir.Unknowns = append(ir.Unknowns, "declared runtime/model contract resolution against server authority")
			break
		}
	}
	ir.Unknowns = normalizeUniqueStrings(ir.Unknowns, func(value string) string { return value })
}
