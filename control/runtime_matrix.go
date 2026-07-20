package main

import (
	"context"
	"fmt"
	"math"
	"unicode"
	"unicode/utf8"
)

func generatedCapabilityHasHWClass(cap generatedRuntimeCapability, hwClass string) bool {
	for _, candidate := range cap.HardwareClasses {
		if candidate == hwClass {
			return true
		}
	}
	return false
}

func advertisedRuntimeJobModel(jobType, modelRef string) bool {
	if jobType == "" || modelRef == "" {
		return false
	}
	for _, cap := range generatedAdvertisedRuntimeCapabilities {
		if cap.Job == jobType && cap.Model == modelRef {
			return true
		}
	}
	return false
}

func advertisedRuntimeModel(modelRef string) bool {
	if modelRef == "" {
		return false
	}
	for _, cap := range generatedAdvertisedRuntimeCapabilities {
		if cap.Model == modelRef {
			return true
		}
	}
	return false
}

func validateAdvertisedRuntimeJobModel(jobType, modelRef string) error {
	if advertisedRuntimeJobModel(jobType, modelRef) {
		return nil
	}
	return fmt.Errorf(
		"runtime capability is not advertised for job_type=%q model=%q (matrix %s)",
		jobType, modelRef, generatedRuntimeMatrixVersion,
	)
}

func generatedRuntimeModelRef(jobType, modelRef string) ModelRef {
	ref := ModelRef{Ref: modelRef}
	for _, cap := range generatedAdvertisedRuntimeCapabilities {
		if cap.Job == jobType && cap.Model == modelRef {
			ref.Kind = cap.ModelKind
			return ref
		}
	}
	return ref
}

func normalizeAdvertisedRuntimeModelRef(jobType string, submitted ModelRef) (ModelRef, error) {
	if err := validateAdvertisedRuntimeJobModel(jobType, submitted.Ref); err != nil {
		return ModelRef{}, err
	}
	canonical := generatedRuntimeModelRef(jobType, submitted.Ref)
	if submitted.Kind != "" && submitted.Kind != canonical.Kind {
		return ModelRef{}, fmt.Errorf(
			"runtime matrix requires model.kind=%q for job_type=%q model=%q; got %q",
			canonical.Kind, jobType, submitted.Ref, submitted.Kind,
		)
	}
	return canonical, nil
}

func projectWorkerRuntimeCapabilities(cap WorkerCapability) ([]generatedRuntimeCapability, error) {
	if err := validateWorkerCapabilityShape(cap); err != nil {
		return nil, err
	}
	lane := make([]generatedRuntimeCapability, 0, len(generatedAdvertisedRuntimeCapabilities))
	for _, candidate := range generatedAdvertisedRuntimeCapabilities {
		if candidate.Engine == cap.Engine && generatedCapabilityHasHWClass(candidate, cap.HWClass) {
			lane = append(lane, candidate)
		}
	}
	if len(lane) == 0 {
		return nil, fmt.Errorf(
			"runtime engine=%q hw_class=%q has no advertised production cell (matrix %s)",
			cap.Engine, cap.HWClass, generatedRuntimeMatrixVersion,
		)
	}

	jobs, models := make(map[string]bool), make(map[string]bool)
	for _, candidate := range lane {
		jobs[candidate.Job] = true
		if candidate.Model != "" {
			models[candidate.Model] = true
		}
	}
	if err := validateUniqueRuntimeStrings("supported_jobs", cap.SupportedJobs); err != nil {
		return nil, err
	}
	if err := validateUniqueRuntimeStrings("supported_models", cap.SupportedModels); err != nil {
		return nil, err
	}
	for _, job := range cap.SupportedJobs {
		if !jobs[job] {
			return nil, fmt.Errorf("supported job %q is not advertised for engine=%q hw_class=%q", job, cap.Engine, cap.HWClass)
		}
	}
	for _, model := range cap.SupportedModels {
		if !models[model] {
			return nil, fmt.Errorf("supported model %q is not advertised for engine=%q hw_class=%q", model, cap.Engine, cap.HWClass)
		}
	}

	projected := make([]generatedRuntimeCapability, 0, len(lane))
	for _, candidate := range lane {
		if containsStr(cap.SupportedJobs, candidate.Job) && containsStr(cap.SupportedModels, candidate.Model) {
			projected = append(projected, candidate)
		}
	}
	if len(projected) == 0 {
		return nil, fmt.Errorf("worker advertisement projects to zero production capability tuples")
	}

	if len(cap.Benchmarks) > len(projected) {
		return nil, fmt.Errorf("benchmarks has %d tuples but this worker projects to only %d exact production cells", len(cap.Benchmarks), len(projected))
	}
	seenBenchmarks := make(map[[2]string]bool, len(cap.Benchmarks))
	for _, benchmark := range cap.Benchmarks {
		key := [2]string{benchmark.JobType, benchmark.ModelID}
		if seenBenchmarks[key] {
			return nil, fmt.Errorf("benchmarks contains duplicate tuple job_type=%q model=%q", benchmark.JobType, benchmark.ModelID)
		}
		seenBenchmarks[key] = true
		if !containsStr(cap.SupportedJobs, benchmark.JobType) || !containsStr(cap.SupportedModels, benchmark.ModelID) {
			return nil, fmt.Errorf("benchmark tuple job_type=%q model=%q is absent from the worker advertisement", benchmark.JobType, benchmark.ModelID)
		}
		matched := false
		for _, candidate := range projected {
			if candidate.Job == benchmark.JobType && candidate.Model == benchmark.ModelID {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("benchmark tuple job_type=%q model=%q is not an advertised production cell for this runtime", benchmark.JobType, benchmark.ModelID)
		}
	}

	for _, candidate := range projected {
		if float64(cap.MemoryGB) < candidate.MinMemoryGB {
			return nil, fmt.Errorf(
				"worker memory %.3f GB is below advertised cell %q minimum %.3f GB",
				cap.MemoryGB, candidate.ID, candidate.MinMemoryGB,
			)
		}
	}
	return projected, nil
}

const (
	maxWorkerBuildHashBytes = 256
	maxWorkerVersionBytes   = 128
	maxWorkerOSVersionBytes = 256
	maxWorkerMemoryGB       = 16 * 1024
	maxWorkerMemoryBwGbps   = 1_000_000
	maxWorkerMinPayoutUSDHr = 1_000_000
	maxBenchmarkRate        = 1_000_000_000
	maxBenchmarkLoadMS      = uint64(24 * 60 * 60 * 1000)
	maxBenchmarkP99MS       = uint32(24 * 60 * 60 * 1000)
)

func validateWorkerTextField(name, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func finiteFloat32(value float32) bool {
	v := float64(value)
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func benchmarkLoadMSForStore(loadMS uint64) (int64, error) {
	if loadMS > maxBenchmarkLoadMS {
		return 0, fmt.Errorf("benchmark load_ms=%d exceeds the 24-hour operational maximum", loadMS)
	}
	return int64(loadMS), nil
}

func validateWorkerCapabilityShape(cap WorkerCapability) error {
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"build_hash", cap.BuildHash, maxWorkerBuildHashBytes},
		{"agent_version", cap.AgentVersion, maxWorkerVersionBytes},
		{"os_version", cap.OSVersion, maxWorkerOSVersionBytes},
	} {
		if err := validateWorkerTextField(field.name, field.value, field.max); err != nil {
			return err
		}
	}
	if !finiteFloat32(cap.MemoryGB) || cap.MemoryGB <= 0 || cap.MemoryGB > maxWorkerMemoryGB {
		return fmt.Errorf("memory_gb must be finite and in (0,%d]", maxWorkerMemoryGB)
	}
	if !finiteFloat32(cap.MemoryBwGbps) || cap.MemoryBwGbps < 0 || cap.MemoryBwGbps > maxWorkerMemoryBwGbps {
		return fmt.Errorf("memory_bw_gbps must be finite and in [0,%d]", maxWorkerMemoryBwGbps)
	}
	if !finiteFloat32(cap.MinPayoutUsdHr) || cap.MinPayoutUsdHr < 0 || cap.MinPayoutUsdHr > maxWorkerMinPayoutUSDHr {
		return fmt.Errorf("min_payout_usd_hr must be finite and in [0,%d]", maxWorkerMinPayoutUSDHr)
	}
	for _, benchmark := range cap.Benchmarks {
		if !finiteFloat32(benchmark.TPS) || !finiteFloat32(benchmark.EPS) ||
			benchmark.TPS < 0 || benchmark.EPS < 0 {
			return fmt.Errorf("benchmark tuple job_type=%q model=%q has non-finite or negative throughput", benchmark.JobType, benchmark.ModelID)
		}
		if benchmark.TPS > maxBenchmarkRate || benchmark.EPS > maxBenchmarkRate {
			return fmt.Errorf("benchmark tuple job_type=%q model=%q exceeds the plausible throughput maximum", benchmark.JobType, benchmark.ModelID)
		}
		rate := benchmark.TPS
		if benchmark.JobType == "embed" {
			rate = benchmark.EPS
		}
		if rate <= 0 {
			return fmt.Errorf("benchmark tuple job_type=%q model=%q has no positive measured throughput", benchmark.JobType, benchmark.ModelID)
		}
		if benchmark.P99MS > maxBenchmarkP99MS {
			return fmt.Errorf("benchmark tuple job_type=%q model=%q p99_ms exceeds the 24-hour operational maximum", benchmark.JobType, benchmark.ModelID)
		}
		if _, err := benchmarkLoadMSForStore(benchmark.LoadMS); err != nil {
			return fmt.Errorf("benchmark tuple job_type=%q model=%q: %w", benchmark.JobType, benchmark.ModelID, err)
		}
	}
	return nil
}

func validateWorkerRuntimeProjection(cap WorkerCapability) error {
	_, err := projectWorkerRuntimeCapabilities(cap)
	return err
}

func validateUniqueRuntimeStrings(field string, values []string) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" {
			return fmt.Errorf("%s contains a blank value", field)
		}
		if seen[value] {
			return fmt.Errorf("%s contains duplicate value %q", field, value)
		}
		seen[value] = true
	}
	return nil
}

func validateHeartbeatRuntimeModels(models []string) error {
	if err := validateUniqueRuntimeStrings("loaded_models", models); err != nil {
		return err
	}
	for _, model := range models {
		if !advertisedRuntimeModel(model) {
			return fmt.Errorf("loaded model %q is not in the advertised production runtime projection", model)
		}
	}
	return nil
}

func validateAdvertisedRuntimeCatalogRows(rows []ModelRow) error {
	byID := make(map[string]ModelRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	requiredMemory := map[string]float64{}
	requiredWireKind := map[string]string{}
	for _, cap := range generatedAdvertisedRuntimeCapabilities {
		if cap.Model != "" && cap.MinMemoryGB > requiredMemory[cap.Model] {
			requiredMemory[cap.Model] = cap.MinMemoryGB
		}
		if cap.Model != "" {
			if previous := requiredWireKind[cap.Model]; previous != "" && previous != cap.ModelKind {
				return fmt.Errorf("runtime matrix gives model %q conflicting wire kinds %q and %q", cap.Model, previous, cap.ModelKind)
			}
			requiredWireKind[cap.Model] = cap.ModelKind
		}
	}
	for modelID, minMemory := range requiredMemory {
		row, ok := byID[modelID]
		if !ok {
			return fmt.Errorf("runtime matrix advertises model %q but the DB catalog has no row", modelID)
		}
		price := modelPrice(row)
		if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
			return fmt.Errorf("runtime matrix advertises model %q but its DB price is not positive", modelID)
		}
		if row.Kind == "" || row.HFRepo == "" {
			return fmt.Errorf("runtime matrix advertises model %q but its DB kind/repository metadata is incomplete", modelID)
		}
		wireKind, err := runtimeWireModelKind(row.Kind)
		if err != nil {
			return fmt.Errorf("runtime matrix advertises model %q with unusable catalog kind: %w", modelID, err)
		}
		if expected := requiredWireKind[modelID]; wireKind != expected {
			return fmt.Errorf(
				"runtime matrix requires wire kind %q for model %q but catalog kind %q maps to %q",
				expected, modelID, row.Kind, wireKind,
			)
		}
		if float64(row.MinMemoryGB) < minMemory {
			return fmt.Errorf(
				"runtime matrix requires %.3f GB for model %q but the DB catalog advertises only %.3f GB",
				minMemory, modelID, row.MinMemoryGB,
			)
		}
	}
	return nil
}

func runtimeWireModelKind(catalogKind string) (string, error) {
	switch catalogKind {
	case "gguf":
		return "gguf", nil
	case "hf", "embed":
		return "hf", nil
	default:
		return "", fmt.Errorf("catalog kind %q has no agent wire mapping", catalogKind)
	}
}

func (s *Store) ValidateAdvertisedRuntimeCatalog(ctx context.Context) error {
	rows, err := s.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("reading model catalog for runtime-matrix validation: %w", err)
	}
	return validateAdvertisedRuntimeCatalogRows(rows)
}
