package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The 13 required fields on BenchmarkComparisonContract, in contract order.
// A comparison missing any one is refused; this list is the table, not an example.
var benchmarkComparisonRequiredFieldNames = []string{
	"model_identity",
	"model_revision",
	"tokenizer",
	"precision_quantization",
	"accepted_quality_contract",
	"prompt_distribution",
	"prompt_and_output_lengths",
	"sampling_settings",
	"batch_concurrency",
	"hardware",
	"runtime_build",
	"driver",
	"warm_cold_state",
}

const (
	benchCmpWorkload     = "batch_infer"
	benchCmpModel        = "llama-3.2-1b-instruct-q4"
	benchCmpMetalCell    = "candle-metal-llama1-infer"
	benchCmpCUDACell     = "vllm-cuda-llama1-infer"
	benchCmpCoveringID   = "TEST_ONLY-batch-infer-q4-metal-bf16-cuda"
	benchCmpRefusedPair  = "batch-infer-metal-q4-vs-cuda-bf16-REFUSED"
	benchCmpMatchedExact = "batch-infer-byte-exact-matched-precision-llama32-1b"
)

func completeQ4MetalContract() BenchmarkComparisonContract {
	return BenchmarkComparisonContract{
		ModelIdentity:           benchCmpModel,
		ModelRevision:           "gguf-q4-k-m-3f5a2242",
		Tokenizer:               "llama-3.2-instruct-tokenizer-r1",
		PrecisionQuantization:   "q4_k_m",
		AcceptedQualityContract: benchCmpMatchedExact,
		PromptDistribution:      "quality-suite-v1",
		PromptAndOutputLengths:  "prompt_tokens=128 output_tokens=128",
		SamplingSettings:        "temperature=0 top_p=1 seed=1 greedy",
		BatchConcurrency:        "batch=1 concurrency=1",
		Hardware:                "metal",
		RuntimeBuild:            "candle-metal-build-r7",
		Driver:                  "metal-4.0",
		WarmColdState:           benchmarkColdState,
	}
}

func q4MetalMeasurement() BenchmarkMeasurement {
	return BenchmarkMeasurement{
		Workload: benchCmpWorkload,
		CellID:   benchCmpMetalCell,
		Contract: completeQ4MetalContract(),
	}
}

func bf16CUDAMeasurement() BenchmarkMeasurement {
	c := completeQ4MetalContract()
	c.PrecisionQuantization = "bf16"
	c.Hardware = "cuda"
	c.RuntimeBuild = "vllm-cuda-build-r1"
	c.Driver = "cuda-12.4"
	return BenchmarkMeasurement{
		Workload: benchCmpWorkload,
		CellID:   benchCmpCUDACell,
		Contract: c,
	}
}

func installCoveringQ4VsBF16Contract(t *testing.T) {
	t.Helper()
	acceptableQualityContracts[benchCmpCoveringID] = AcceptableQualityContract{
		ID:                       benchCmpCoveringID,
		JobType:                  benchCmpWorkload,
		ModelRef:                 benchCmpModel,
		Status:                   "ACTIVE",
		MultiFamilySubstitutable: true,
		EligibleCellIDs:          []string{benchCmpMetalCell, benchCmpCUDACell},
		AllowedPrecisions:        []string{"q4_k_m", "bf16"},
		AllowedDevices:           []string{"metal", "cuda"},
	}
	t.Cleanup(func() { delete(acceptableQualityContracts, benchCmpCoveringID) })
}

func contractFieldPointer(c *BenchmarkComparisonContract, field string) *string {
	switch field {
	case "model_identity":
		return &c.ModelIdentity
	case "model_revision":
		return &c.ModelRevision
	case "tokenizer":
		return &c.Tokenizer
	case "precision_quantization":
		return &c.PrecisionQuantization
	case "accepted_quality_contract":
		return &c.AcceptedQualityContract
	case "prompt_distribution":
		return &c.PromptDistribution
	case "prompt_and_output_lengths":
		return &c.PromptAndOutputLengths
	case "sampling_settings":
		return &c.SamplingSettings
	case "batch_concurrency":
		return &c.BatchConcurrency
	case "hardware":
		return &c.Hardware
	case "runtime_build":
		return &c.RuntimeBuild
	case "driver":
		return &c.Driver
	case "warm_cold_state":
		return &c.WarmColdState
	default:
		return nil
	}
}

func TestBenchmarkComparisonRequiredFieldListIsTheFullContract(t *testing.T) {
	got := make([]string, 0, len(benchmarkComparisonRequiredFieldNames))
	for _, field := range benchmarkComparisonContractFields(BenchmarkComparisonContract{}) {
		got = append(got, field.name)
	}
	if len(got) != len(benchmarkComparisonRequiredFieldNames) {
		t.Fatalf("required field count %d, want %d (%v)", len(got), len(benchmarkComparisonRequiredFieldNames), got)
	}
	for i, name := range benchmarkComparisonRequiredFieldNames {
		if got[i] != name {
			t.Fatalf("required field %d = %q, want %q (full list %v)", i, got[i], name, got)
		}
	}
}

func TestBenchmarkComparisonIdenticalMeasurementsAccepted(t *testing.T) {
	a := q4MetalMeasurement()
	b := q4MetalMeasurement()
	if err := CompareBenchmarkMeasurements(a, b); err != nil {
		t.Fatalf("identical q4 Metal arms must compare: %v", err)
	}
}

func TestBenchmarkComparisonQ4MetalVsBF16CUDARefusedWithoutQualityContract(t *testing.T) {
	metal := q4MetalMeasurement()
	cuda := bf16CUDAMeasurement()
	// The catalogue already names this pairing and REFUSES it. Citing that
	// row is not coverage: it is the record that substitutability does not
	// hold. No other ACTIVE contract covers q4 Metal vs bf16 CUDA generation.
	metal.Contract.AcceptedQualityContract = benchCmpRefusedPair
	cuda.Contract.AcceptedQualityContract = benchCmpRefusedPair

	err := CompareBenchmarkMeasurements(metal, cuda)
	if err == nil {
		t.Fatal("q4 Metal vs bf16 CUDA must be REFUSED with no covering quality contract")
	}
	if !errors.Is(err, errBenchmarkComparisonRefused) {
		t.Fatalf("want errBenchmarkComparisonRefused, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "precision_quantization") {
		t.Fatalf("refusal must name the differing field precision_quantization, got %v", err)
	}
	if !strings.Contains(msg, "no AcceptableQualityContract covers this pairing") {
		t.Fatalf("refusal must say no quality contract covers the pairing, got %v", err)
	}
	if !strings.Contains(msg, `q4_k_m`) || !strings.Contains(msg, `bf16`) {
		t.Fatalf("refusal must quote the differing precision values, got %v", err)
	}
}

func TestBenchmarkComparisonQ4MetalVsBF16CUDAAcceptedUnderQualityContract(t *testing.T) {
	installCoveringQ4VsBF16Contract(t)
	metal := q4MetalMeasurement()
	cuda := bf16CUDAMeasurement()
	metal.Contract.AcceptedQualityContract = benchCmpCoveringID
	cuda.Contract.AcceptedQualityContract = benchCmpCoveringID

	if err := CompareBenchmarkMeasurements(metal, cuda); err != nil {
		t.Fatalf("q4 Metal vs bf16 CUDA must be ACCEPTED under an explicit AcceptableQualityContract: %v", err)
	}
}

func TestBenchmarkComparisonContractMissingAnyRequiredFieldIsRefused(t *testing.T) {
	complete := completeQ4MetalContract()
	for _, field := range benchmarkComparisonRequiredFieldNames {
		t.Run(field, func(t *testing.T) {
			c := complete
			ptr := contractFieldPointer(&c, field)
			if ptr == nil {
				t.Fatalf("test helper has no pointer for required field %s", field)
			}
			*ptr = ""
			a := BenchmarkMeasurement{Workload: benchCmpWorkload, CellID: benchCmpMetalCell, Contract: c}
			b := a
			err := CompareBenchmarkMeasurements(a, b)
			if err == nil {
				t.Fatalf("missing %s must refuse the comparison", field)
			}
			if !errors.Is(err, errBenchmarkComparisonRefused) {
				t.Fatalf("want errBenchmarkComparisonRefused, got %v", err)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("refusal must name missing field %s, got %v", field, err)
			}
			if !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("missing field must mean the contract does not exist, got %v", err)
			}
		})
	}
}

func TestBenchmarkComparisonRefusalNamesDifferingField(t *testing.T) {
	// Each substitutability-relevant field, flipped in isolation, must appear
	// in the refusal. Overridable fields still refuse here because the cited
	// matched-precision contract does not cover a Metal/CUDA or q4/bf16 split.
	cases := []struct {
		field string
		value string
	}{
		{"model_identity", "some-other-model"},
		{"model_revision", "other-revision"},
		{"tokenizer", "other-tokenizer"},
		{"precision_quantization", "bf16"},
		{"prompt_distribution", "other-corpus"},
		{"prompt_and_output_lengths", "prompt_tokens=256 output_tokens=256"},
		{"sampling_settings", "temperature=0.8 top_p=0.9"},
		{"batch_concurrency", "batch=8 concurrency=4"},
		{"hardware", "cuda"},
		{"runtime_build", "other-runtime-build"},
		{"driver", "other-driver"},
		{"warm_cold_state", "warm"},
	}
	base := q4MetalMeasurement()
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			other := q4MetalMeasurement()
			ptr := contractFieldPointer(&other.Contract, tc.field)
			if ptr == nil {
				t.Fatalf("test helper has no pointer for field %s", tc.field)
			}
			*ptr = tc.value
			err := CompareBenchmarkMeasurements(base, other)
			if err == nil {
				t.Fatalf("differing %s must refuse", tc.field)
			}
			if !strings.Contains(err.Error(), "field "+tc.field+" differed") {
				t.Fatalf("refusal must name field %s, got %v", tc.field, err)
			}
			if !strings.Contains(err.Error(), "no AcceptableQualityContract covers this pairing") {
				t.Fatalf("refusal must say no quality contract covers it, got %v", err)
			}
		})
	}
}

func TestBenchmarkComparisonUnknownQualityContractIsRefused(t *testing.T) {
	a := q4MetalMeasurement()
	a.Contract.AcceptedQualityContract = "no-such-quality-contract"
	err := CompareBenchmarkMeasurements(a, a)
	if err == nil {
		t.Fatal("unknown accepted_quality_contract must refuse, never pass")
	}
	if !strings.Contains(err.Error(), "unknown accepted_quality_contract") {
		t.Fatalf("unknown id must be named, got %v", err)
	}
}

func TestBenchmarkComparisonUnknownJSONFieldIsRefused(t *testing.T) {
	raw, err := json.Marshal(completeQ4MetalContract())
	must(t, err)
	var body map[string]string
	must(t, json.Unmarshal(raw, &body))
	body["not_a_contract_field"] = "invented"
	payload, err := json.Marshal(body)
	must(t, err)

	_, parseErr := ParseBenchmarkComparisonContract(payload)
	if parseErr == nil {
		t.Fatal("unknown JSON field must refuse, never pass")
	}
	if !errors.Is(parseErr, errBenchmarkComparisonRefused) {
		t.Fatalf("want errBenchmarkComparisonRefused, got %v", parseErr)
	}
	if !strings.Contains(parseErr.Error(), "unknown") {
		t.Fatalf("unknown field refusal must say so, got %v", parseErr)
	}
}

func TestBenchmarkComparisonUnparseableFieldIsRefused(t *testing.T) {
	c := completeQ4MetalContract()
	c.WarmColdState = "tepid"
	a := BenchmarkMeasurement{Workload: benchCmpWorkload, CellID: benchCmpMetalCell, Contract: c}
	err := CompareBenchmarkMeasurements(a, a)
	if err == nil {
		t.Fatal("unparseable warm_cold_state must refuse, never pass")
	}
	if !strings.Contains(err.Error(), "unparseable field warm_cold_state") {
		t.Fatalf("unparseable field must be named, got %v", err)
	}

	raw, err := json.Marshal(completeQ4MetalContract())
	must(t, err)
	var body map[string]any
	must(t, json.Unmarshal(raw, &body))
	body["model_identity"] = 12
	payload, jsonErr := json.Marshal(body)
	must(t, jsonErr)
	_, parseErr := ParseBenchmarkComparisonContract(payload)
	if parseErr == nil {
		t.Fatal("wrong-typed field must refuse as unparseable")
	}
	if !errors.Is(parseErr, errBenchmarkComparisonRefused) {
		t.Fatalf("want errBenchmarkComparisonRefused, got %v", parseErr)
	}
}

func TestBenchmarkComparisonCoveringContractDoesNotOverrideWorkloadFields(t *testing.T) {
	installCoveringQ4VsBF16Contract(t)
	metal := q4MetalMeasurement()
	cuda := bf16CUDAMeasurement()
	metal.Contract.AcceptedQualityContract = benchCmpCoveringID
	cuda.Contract.AcceptedQualityContract = benchCmpCoveringID
	cuda.Contract.PromptDistribution = "some-other-corpus"

	err := CompareBenchmarkMeasurements(metal, cuda)
	if err == nil {
		t.Fatal("a quality contract must not make a different prompt distribution substitutable")
	}
	if !strings.Contains(err.Error(), "field prompt_distribution differed") {
		t.Fatalf("refusal must name prompt_distribution, got %v", err)
	}
}
