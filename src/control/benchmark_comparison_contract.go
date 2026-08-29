package main

import (
	"errors"
	"fmt"
	"strings"
)

// A published engine comparison is only a result when both arms name what
// actually ran. A number with a story attached — q4 Metal timed against bf16
// CUDA, then labelled an engine delta — is the failure this type exists to
// refuse. AcceptableQualityContract remains the sole substitutability
// authority (src/control/acceptable-quality-contracts.json, embedded in
// src/control/acceptable-quality-contracts.json). This file does not author a
// second one.
//
// Refusal is the default. Missing or empty required fields mean the contract
// does not exist. An unknown quality-contract id, an unparseable vocabulary
// value, or an unknown JSON field is a refusal, never a pass.

var errBenchmarkComparisonRefused = errors.New("benchmark comparison refused")

const (
	benchmarkWarmState = "warm"
	benchmarkColdState = "cold"
)

// BenchmarkComparisonContract pins the identity of one measured arm.
// Every field below is required. An empty value means there is no contract.
type BenchmarkComparisonContract struct {
	ModelIdentity           string `json:"model_identity"`
	ModelRevision           string `json:"model_revision"`
	Tokenizer               string `json:"tokenizer"`
	PrecisionQuantization   string `json:"precision_quantization"`
	AcceptedQualityContract string `json:"accepted_quality_contract"`
	PromptDistribution      string `json:"prompt_distribution"`
	PromptAndOutputLengths  string `json:"prompt_and_output_lengths"`
	SamplingSettings        string `json:"sampling_settings"`
	BatchConcurrency        string `json:"batch_concurrency"`
	Hardware                string `json:"hardware"`
	RuntimeBuild            string `json:"runtime_build"`
	Driver                  string `json:"driver"`
	WarmColdState           string `json:"warm_cold_state"`
}

// BenchmarkMeasurement is one arm of a comparison: a named workload, the cell
// that produced it, and the contract that says what ran.
type BenchmarkMeasurement struct {
	Workload string
	CellID   string
	Contract BenchmarkComparisonContract
}

type benchmarkComparisonField struct {
	name  string
	value string
	// overridable fields may differ when an AcceptableQualityContract
	// explicitly declares the two artifacts substitutable. Everything else
	// is the workload, and a quality contract does not rewrite it.
	overridable bool
}

func benchmarkComparisonContractFields(c BenchmarkComparisonContract) []benchmarkComparisonField {
	return []benchmarkComparisonField{
		{name: "model_identity", value: c.ModelIdentity},
		{name: "model_revision", value: c.ModelRevision},
		{name: "tokenizer", value: c.Tokenizer},
		{name: "precision_quantization", value: c.PrecisionQuantization, overridable: true},
		{name: "accepted_quality_contract", value: c.AcceptedQualityContract},
		{name: "prompt_distribution", value: c.PromptDistribution},
		{name: "prompt_and_output_lengths", value: c.PromptAndOutputLengths},
		{name: "sampling_settings", value: c.SamplingSettings},
		{name: "batch_concurrency", value: c.BatchConcurrency},
		{name: "hardware", value: c.Hardware, overridable: true},
		{name: "runtime_build", value: c.RuntimeBuild, overridable: true},
		{name: "driver", value: c.Driver, overridable: true},
		{name: "warm_cold_state", value: c.WarmColdState},
	}
}

func refusedBenchmarkComparison(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errBenchmarkComparisonRefused}, args...)...)
}

// ParseBenchmarkComparisonContract decodes a closed JSON object. Unknown keys,
// duplicate keys, trailing values, and type mismatches are refusals.
func ParseBenchmarkComparisonContract(raw []byte) (BenchmarkComparisonContract, error) {
	var c BenchmarkComparisonContract
	if err := decodeStrictJSONObject(raw, &c); err != nil {
		return BenchmarkComparisonContract{}, refusedBenchmarkComparison(
			"unknown or unparseable field: %v", err)
	}
	if err := validateBenchmarkComparisonContract(c); err != nil {
		return BenchmarkComparisonContract{}, err
	}
	return c, nil
}

func validateBenchmarkComparisonContract(c BenchmarkComparisonContract) error {
	for _, field := range benchmarkComparisonContractFields(c) {
		if strings.TrimSpace(field.value) == "" {
			return refusedBenchmarkComparison(
				"missing required field %s (comparison contract does not exist)",
				field.name)
		}
	}
	state := strings.TrimSpace(c.WarmColdState)
	if state != benchmarkWarmState && state != benchmarkColdState {
		return refusedBenchmarkComparison(
			"unparseable field warm_cold_state (%q)", c.WarmColdState)
	}
	id := strings.TrimSpace(c.AcceptedQualityContract)
	if _, ok := qualityContractByID(id); !ok {
		return refusedBenchmarkComparison(
			"unknown accepted_quality_contract %q", id)
	}
	return nil
}

// CompareBenchmarkMeasurements refuses unless the two arms are the same
// measured thing, or an AcceptableQualityContract makes the differing
// artifacts substitutable for the named workload.
func CompareBenchmarkMeasurements(a, b BenchmarkMeasurement) error {
	if err := validateBenchmarkComparisonContract(a.Contract); err != nil {
		return err
	}
	if err := validateBenchmarkComparisonContract(b.Contract); err != nil {
		return err
	}
	workloadA := strings.TrimSpace(a.Workload)
	workloadB := strings.TrimSpace(b.Workload)
	if workloadA == "" || workloadB == "" {
		return refusedBenchmarkComparison(
			"missing required field workload (comparison contract does not exist)")
	}
	if workloadA != workloadB {
		return refusedBenchmarkComparison(
			"field workload differed (%q vs %q); no AcceptableQualityContract covers this pairing for workload %q",
			workloadA, workloadB, workloadA)
	}

	left := benchmarkComparisonContractFields(a.Contract)
	right := benchmarkComparisonContractFields(b.Contract)
	var firstOverride, firstNonOverride *fieldDiff
	for i := range left {
		av := strings.TrimSpace(left[i].value)
		bv := strings.TrimSpace(right[i].value)
		if av == bv {
			continue
		}
		diff := &fieldDiff{name: left[i].name, a: av, b: bv}
		if left[i].overridable {
			if firstOverride == nil {
				firstOverride = diff
			}
			continue
		}
		if firstNonOverride == nil {
			firstNonOverride = diff
		}
	}
	if firstOverride == nil && firstNonOverride == nil {
		return nil
	}
	if firstNonOverride != nil {
		return differRefused(*firstNonOverride, workloadA)
	}

	qualityID := strings.TrimSpace(a.Contract.AcceptedQualityContract)
	contract, ok := qualityContractByID(qualityID)
	if !ok {
		return refusedBenchmarkComparison("unknown accepted_quality_contract %q", qualityID)
	}
	if !qualityContractCoversBenchmarkPair(contract, a, b) {
		return differRefused(*firstOverride, workloadA)
	}
	return nil
}

type fieldDiff struct {
	name, a, b string
}

func differRefused(d fieldDiff, workload string) error {
	return refusedBenchmarkComparison(
		"field %s differed (%q vs %q); no AcceptableQualityContract covers this pairing for workload %q",
		d.name, d.a, d.b, workload)
}

// qualityContractCoversBenchmarkPair asks the existing quality-contract
// authority whether these two artifacts are the same product for this
// workload. A REFUSED row, an unknown id, empty allowed lists, or a model
// / job mismatch is not coverage.
func qualityContractCoversBenchmarkPair(c AcceptableQualityContract, a, b BenchmarkMeasurement) bool {
	if c.Status != "ACTIVE" || !c.MultiFamilySubstitutable {
		return false
	}
	workload := strings.TrimSpace(a.Workload)
	if workload == "" || c.JobType != workload || c.JobType != strings.TrimSpace(b.Workload) {
		return false
	}
	modelA := strings.TrimSpace(a.Contract.ModelIdentity)
	modelB := strings.TrimSpace(b.Contract.ModelIdentity)
	if c.ModelRef != modelA || c.ModelRef != modelB {
		return false
	}
	if len(c.AllowedPrecisions) == 0 || len(c.AllowedDevices) == 0 {
		return false
	}
	if !stringInList(c.AllowedPrecisions, a.Contract.PrecisionQuantization) ||
		!stringInList(c.AllowedPrecisions, b.Contract.PrecisionQuantization) {
		return false
	}
	if !stringInList(c.AllowedDevices, a.Contract.Hardware) ||
		!stringInList(c.AllowedDevices, b.Contract.Hardware) {
		return false
	}
	cellA := strings.TrimSpace(a.CellID)
	cellB := strings.TrimSpace(b.CellID)
	if len(c.EligibleCellIDs) > 0 {
		if cellA == "" || cellB == "" {
			return false
		}
		if _, err := qualityContractAuthorizingMultiFamily(c.ID, []string{cellA, cellB}); err != nil {
			return false
		}
	}
	return true
}

func stringInList(list []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, item := range list {
		if strings.TrimSpace(item) == want {
			return true
		}
	}
	return false
}
