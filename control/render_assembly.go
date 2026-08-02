package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	renderAssemblyManifestVersion = "MERC_RENDER_ASSEMBLY_MANIFEST_V1"
	renderAssemblyReceiptVersion  = "MERC_RENDER_ASSEMBLY_RECEIPT_V1"
	maxRenderAssemblyAttempts     = 64
)

// ProjectRenderAssemblyUnit is an observation for one deterministic render
// ordinal. A FAILED observation records why an attempted worker result was
// replaced; exactly one later SUCCEEDED observation must close that ordinal.
// It is evidence only: no worker identity, asset transfer, output bytes, or
// settlement authority is accepted on this boundary.
type ProjectRenderAssemblyUnit struct {
	Ordinal      int64  `json:"ordinal"`
	Attempt      int    `json:"attempt"`
	Status       string `json:"status"`
	OutputSHA256 string `json:"output_sha256,omitempty"`
	FailureCode  string `json:"failure_code,omitempty"`
}

type ProjectRenderAssemblyManifest struct {
	Units []ProjectRenderAssemblyUnit `json:"units"`
}

type ProjectRenderAssemblySummary struct {
	UnitCount        int64
	SucceededUnits   int64
	FailedAttempts   int64
	ReplacedOrdinals int64
}

// ProjectRenderAssemblyReceipt is a durable buyer-scoped manifest check. It
// proves complete deterministic ordinal coverage and replacement history, but
// deliberately remains non-executable until a governed renderer, asset
// locality, output verifier, and economic settlement path exist.
type ProjectRenderAssemblyReceipt struct {
	ID               string                      `json:"id"`
	Version          string                      `json:"version"`
	CompileReceiptID string                      `json:"compile_receipt_id"`
	ProjectSHA256    string                      `json:"project_sha256"`
	IRSHA256         string                      `json:"ir_sha256"`
	StepID           string                      `json:"step_id"`
	ManifestSHA256   string                      `json:"manifest_sha256"`
	Status           string                      `json:"status"`
	UnitCount        int64                       `json:"unit_count"`
	SucceededUnits   int64                       `json:"succeeded_units"`
	FailedAttempts   int64                       `json:"failed_attempts"`
	ReplacedOrdinals int64                       `json:"replaced_ordinals"`
	Units            []ProjectRenderAssemblyUnit `json:"units"`
	ExecutionRefusal string                      `json:"execution_refusal"`
	CreatedAt        time.Time                   `json:"created_at"`
}

// validateProjectRenderAssemblyManifest requires a complete, lexically
// ordered manifest. Failed attempts may precede the one successful result for
// an ordinal, which is the only replacement shape the receipt can honestly
// describe without selecting a worker or silently dropping a failure.
func validateProjectRenderAssemblyManifest(
	render ProjectIRRendering, manifest ProjectRenderAssemblyManifest,
) (ProjectRenderAssemblySummary, error) {
	plan, err := deriveProjectRenderWorkPlan(render)
	if err != nil {
		return ProjectRenderAssemblySummary{}, err
	}
	if len(manifest.Units) == 0 {
		return ProjectRenderAssemblySummary{}, errors.New("render assembly manifest has no units")
	}
	summary := ProjectRenderAssemblySummary{UnitCount: plan.UnitCount}
	index := 0
	for ordinal := int64(0); ordinal < plan.UnitCount; ordinal++ {
		if index >= len(manifest.Units) || manifest.Units[index].Ordinal != ordinal {
			return ProjectRenderAssemblySummary{}, fmt.Errorf("render assembly is missing ordinal %d", ordinal)
		}
		attempt := 0
		succeeded := false
		failedForOrdinal := false
		for index < len(manifest.Units) && manifest.Units[index].Ordinal == ordinal {
			unit := manifest.Units[index]
			if unit.Attempt != attempt || attempt >= maxRenderAssemblyAttempts {
				return ProjectRenderAssemblySummary{}, fmt.Errorf("render ordinal %d attempts are not contiguous and bounded", ordinal)
			}
			switch unit.Status {
			case "FAILED":
				if succeeded || strings.TrimSpace(unit.FailureCode) == "" || len(unit.FailureCode) > 128 || unit.OutputSHA256 != "" {
					return ProjectRenderAssemblySummary{}, fmt.Errorf("render ordinal %d has an invalid failed attempt", ordinal)
				}
				failedForOrdinal = true
				summary.FailedAttempts++
			case "SUCCEEDED":
				if succeeded || !validSHA256(unit.OutputSHA256) || unit.FailureCode != "" {
					return ProjectRenderAssemblySummary{}, fmt.Errorf("render ordinal %d has an invalid successful attempt", ordinal)
				}
				succeeded = true
				summary.SucceededUnits++
			default:
				return ProjectRenderAssemblySummary{}, fmt.Errorf("render ordinal %d has unsupported status %q", ordinal, unit.Status)
			}
			attempt++
			index++
		}
		if !succeeded {
			return ProjectRenderAssemblySummary{}, fmt.Errorf("render ordinal %d has no successful replacement result", ordinal)
		}
		if failedForOrdinal {
			summary.ReplacedOrdinals++
		}
	}
	if index != len(manifest.Units) {
		return ProjectRenderAssemblySummary{}, errors.New("render assembly manifest contains an out-of-range or duplicate ordinal")
	}
	if summary.SucceededUnits != plan.UnitCount {
		return ProjectRenderAssemblySummary{}, errors.New("render assembly manifest does not cover every planned unit")
	}
	return summary, nil
}
