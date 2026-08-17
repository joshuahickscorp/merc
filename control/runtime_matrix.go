package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
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
	for _, cap := range advertisedRuntimeCapabilities() {
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
	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.Model == modelRef {
			return true
		}
	}
	return false
}

func directedRuntimeModel(modelRef string) bool {
	if modelRef == "" {
		return false
	}
	for _, cap := range directedRuntimeCapabilities() {
		if cap.Model == modelRef {
			return true
		}
	}
	return false
}

func documentRuntimeModel(modelRef string) bool {
	if modelRef == "" {
		return false
	}
	for _, cap := range generatedCapabilityRuntimeCells {
		if cap.Model == modelRef {
			return true
		}
	}
	return false
}

// serviceLeaseProfileModel is true when modelRef is a governed vLLM profile's
// model_alias. Service-lease offers join warm capacity to measured
// worker_model_state rows keyed by that alias, so heartbeats may report it.
func serviceLeaseProfileModel(modelRef string) bool {
	if modelRef == "" {
		return false
	}
	for _, profile := range vllmRuntimeProfiles {
		if profile.ModelAlias == modelRef {
			return true
		}
	}
	return false
}

func measurableResidencyModel(modelRef string) bool {
	return advertisedRuntimeModel(modelRef) || serviceLeaseProfileModel(modelRef)
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
	// Prefer a deterministic kind when exactly one is advertised. When several
	// cells serve the same model under different wire kinds, leave Kind empty so
	// callers do not treat the first advertised cell as the only addressable one.
	kinds := advertisedRuntimeModelKinds(jobType, modelRef)
	if len(kinds) == 1 {
		ref.Kind = kinds[0]
	}
	return ref
}

// advertisedRuntimeModelKinds is the set of artifact formats advertised cells
// actually load for a (job, model). Format belongs to the (runtime, model) pair,
// not to the model alone — candle may serve MiniLM as hf while llama.cpp serves
// the same logical model as gguf.
func advertisedRuntimeModelKinds(jobType, modelRef string) []string {
	if jobType == "" || modelRef == "" {
		return nil
	}
	seen := map[string]bool{}
	var kinds []string
	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.Job != jobType || cap.Model != modelRef || cap.ModelKind == "" {
			continue
		}
		if seen[cap.ModelKind] {
			continue
		}
		seen[cap.ModelKind] = true
		kinds = append(kinds, cap.ModelKind)
	}
	sort.Strings(kinds)
	return kinds
}

func normalizeAdvertisedRuntimeModelRef(jobType string, submitted ModelRef) (ModelRef, error) {
	if err := validateAdvertisedRuntimeJobModel(jobType, submitted.Ref); err != nil {
		return ModelRef{}, err
	}
	allowed := advertisedRuntimeModelKinds(jobType, submitted.Ref)
	if len(allowed) == 0 {
		// The job/model pair is advertised but no cell declared a wire kind —
		// refuse rather than invent one.
		return ModelRef{}, fmt.Errorf(
			"runtime matrix advertises job_type=%q model=%q with no wire kind",
			jobType, submitted.Ref,
		)
	}
	if submitted.Kind == "" {
		// Single-kind catalogue: fill the kind so existing callers keep a
		// concrete binding. Multi-kind: leave empty so admission can rank across
		// cells rather than silently collapsing to the first advertised kind.
		if len(allowed) == 1 {
			return ModelRef{Kind: allowed[0], Ref: submitted.Ref}, nil
		}
		return ModelRef{Ref: submitted.Ref}, nil
	}
	for _, kind := range allowed {
		if kind == submitted.Kind {
			return ModelRef{Kind: submitted.Kind, Ref: submitted.Ref}, nil
		}
	}
	return ModelRef{}, fmt.Errorf(
		"runtime matrix has no advertised cell serving model.kind=%q for job_type=%q model=%q (allowed: %s)",
		submitted.Kind, jobType, submitted.Ref, strings.Join(allowed, ", "),
	)
}

func projectWorkerRuntimeCapabilities(cap WorkerCapability) ([]generatedRuntimeCapability, error) {
	if err := validateWorkerCapabilityShape(cap); err != nil {
		return nil, err
	}
	// Two lanes, because a worker declares CAPABILITY and the control plane
	// decides ACTIVATION.
	//
	// What the worker says it can serve is validated against the capability set —
	// every cell the document declares for its engine and hardware, whatever the
	// lifecycle. What it is AUTHORIZED to serve is the directed set under the
	// activation policy in force. Validating the declaration against the directed
	// set instead put lifecycle on the agent's build: an agent whose embedded
	// document called a cell DRAFT could not declare it, so promoting that cell by
	// policy still required a rebuild of the fleet — precisely the coupling the
	// capability/activation split exists to remove.
	//
	// The directed set is the advertised set plus cells reachable only by operator
	// or test routing. Widening enrolment to it is what breaks the bootstrap
	// circle — a cell reaches REAL_RUNTIME_PROVEN by being driven through the
	// chain, so a worker that cannot offer an unproven cell can never be the one
	// driving it. What stops that widening the PRODUCT is that ordinary dispatch
	// is gated on the frozen runtime candidate, and directed candidates are only
	// frozen by an operator or a test.
	capable := make([]generatedRuntimeCapability, 0, len(generatedCapabilityRuntimeCells))
	for _, candidate := range generatedCapabilityRuntimeCells {
		if candidate.Engine == cap.Engine && generatedCapabilityHasHWClass(candidate, cap.HWClass) {
			capable = append(capable, candidate)
		}
	}
	if len(capable) == 0 {
		return nil, fmt.Errorf(
			"runtime engine=%q hw_class=%q has no reachable production cell (matrix %s)",
			cap.Engine, cap.HWClass, generatedRuntimeMatrixVersion,
		)
	}
	lane := make([]generatedRuntimeCapability, 0, len(capable))
	for _, candidate := range directedRuntimeCapabilities() {
		if candidate.Engine == cap.Engine && generatedCapabilityHasHWClass(candidate, cap.HWClass) {
			lane = append(lane, candidate)
		}
	}

	jobs, models := make(map[string]bool), make(map[string]bool)
	for _, candidate := range capable {
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
		// Capable and not activated. Said explicitly, because "projects to zero
		// tuples" reads as a malformed advertisement and this is a governance
		// decision about cells the worker really can serve.
		return nil, fmt.Errorf(
			"no cell this worker declares is activated for routing or directed use "+
				"(engine=%q hw_class=%q jobs=%v models=%v)",
			cap.Engine, cap.HWClass, cap.SupportedJobs, cap.SupportedModels)
	}

	seenBenchmarks := make(map[[2]string]BenchResult, len(cap.Benchmarks))
	for _, benchmark := range cap.Benchmarks {
		key := [2]string{benchmark.JobType, benchmark.ModelID}
		if _, exists := seenBenchmarks[key]; exists {
			return nil, fmt.Errorf("benchmarks contains duplicate tuple job_type=%q model=%q", benchmark.JobType, benchmark.ModelID)
		}
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
			capableCell := false
			for _, candidate := range capable {
				if candidate.Job == benchmark.JobType && candidate.Model == benchmark.ModelID {
					capableCell = true
					break
				}
			}
			if !capableCell {
				return nil, fmt.Errorf("benchmark tuple job_type=%q model=%q is not an advertised production cell for this runtime", benchmark.JobType, benchmark.ModelID)
			}
			// Capable but not directed/advertised (ACTIVE-unbound is
			// quarantined). Ignore the measurement; do not refuse the worker.
			continue
		}
		seenBenchmarks[key] = benchmark
	}

	for _, candidate := range projected {
		if float64(cap.MemoryGB) < candidate.MinMemoryGB {
			return nil, fmt.Errorf(
				"worker memory %.3f GB is below advertised cell %q minimum %.3f GB",
				cap.MemoryGB, candidate.ID, candidate.MinMemoryGB,
			)
		}
		if !advertisedRuntimeCell(candidate.ID) {
			continue
		}
		profile, cell, receipt, err := currentRuntimeCellBenchmarkIdentity(candidate.ID)
		if err != nil {
			return nil, err
		}
		if !validCurrentEngineBuildIdentityPolicy(cap.BuildIdentityPolicy) ||
			cap.BuildIdentityPolicy != receipt.EngineBuildIdentityPolicy {
			return nil, fmt.Errorf(
				"worker/current benchmark build_identity_policy %q/%q does not exactly match for advertised cell %q",
				cap.BuildIdentityPolicy, receipt.EngineBuildIdentityPolicy,
				candidate.ID)
		}
		if cap.HWClass != receipt.HWClass || cap.BuildHash != receipt.EngineBuildHash ||
			cap.HardwareIdentity != receipt.HardwareIdentity {
			return nil, fmt.Errorf(
				"worker identity hw_class/build_hash/hardware_identity %q/%q/%q does not exactly match advertised cell %q benchmark %q/%q/%q",
				cap.HWClass, cap.BuildHash, cap.HardwareIdentity, candidate.ID,
				receipt.HWClass, receipt.EngineBuildHash, receipt.HardwareIdentity)
		}
		benchmark, ok := seenBenchmarks[[2]string{candidate.Job, candidate.Model}]
		if !ok {
			return nil, fmt.Errorf(
				"advertised cell %q requires a fresh matching worker benchmark for job_type=%q model=%q",
				candidate.ID, candidate.Job, candidate.Model)
		}
		performance := resolveCellPerformance(profile, cell, runtimeCellPerformanceNow())
		if err := validateCurrentRuntimeCellPerformanceAuthorityAt(
			performance, runtimeCellPerformanceNow()); err != nil {
			return nil, fmt.Errorf("advertised cell %q current performance authority: %w", candidate.ID, err)
		}
		if benchmark.Unit != performance.Unit || benchmark.UnitScope != performance.UnitScope {
			return nil, fmt.Errorf(
				"advertised cell %q worker benchmark unit/scope %q/%q does not equal governed floor %q/%q",
				candidate.ID, benchmark.Unit, benchmark.UnitScope,
				performance.Unit, performance.UnitScope)
		}
		if !benchmark.ThermalOK {
			return nil, fmt.Errorf("advertised cell %q worker benchmark is not thermally valid", candidate.ID)
		}
		measuredAt := time.Unix(int64(benchmark.MeasuredUnix), 0).UTC()
		now := runtimeCellPerformanceNow().UTC()
		if benchmark.MeasuredUnix == 0 || measuredAt.After(now.Add(workerBenchmarkFutureSkew)) ||
			now.Sub(measuredAt) > workerBenchmarkMaxAge {
			return nil, fmt.Errorf(
				"advertised cell %q worker benchmark measured_unix=%d is not fresh under %s",
				candidate.ID, benchmark.MeasuredUnix, workerBenchmarkFreshnessPolicy)
		}
		rate := benchmark.TPS
		if candidate.Job == "embed" {
			rate = benchmark.EPS
		}
		if float64(rate)+1e-6 < performance.ConservativeUnitsPerSec {
			return nil, fmt.Errorf(
				"advertised cell %q worker benchmark %.6f %s/%s per second is below governed conservative floor %.6f",
				candidate.ID, rate, benchmark.Unit, benchmark.UnitScope,
				performance.ConservativeUnitsPerSec)
		}
	}
	return projected, nil
}

const (
	maxWorkerBuildHashBytes        = 256
	maxWorkerVersionBytes          = 128
	maxWorkerOSVersionBytes        = 256
	maxWorkerMemoryGB              = 16 * 1024
	maxWorkerMemoryBwGbps          = 1_000_000
	maxWorkerMinPayoutUSDHr        = 1_000_000
	maxBenchmarkRate               = 1_000_000_000
	maxBenchmarkLoadMS             = uint64(24 * 60 * 60 * 1000)
	maxBenchmarkP99MS              = uint32(24 * 60 * 60 * 1000)
	workerBenchmarkMaxAge          = 7 * 24 * time.Hour
	workerBenchmarkFutureSkew      = 5 * time.Minute
	workerBenchmarkFreshnessPolicy = "worker-startup-benchmark-v1/max-age-7d"
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

// Residency measurement bounds. load_ms reuses the benchmark ceiling (a load
// that took more than a day is not an operational measurement). rss_delta_bytes
// is capped at the same memory ceiling workers are allowed to declare so a
// malformed heartbeat cannot plant a multi-exabyte residency figure that later
// reaches a pricing path. Negative deltas are allowed (RSS noise around a small
// model) but only within the same absolute bound — refuse, never clamp.
const maxResidencyRSSDeltaBytes = int64(maxWorkerMemoryGB) * 1024 * 1024 * 1024

func residencyLoadMSForStore(loadMS uint64) (int64, error) {
	return benchmarkLoadMSForStore(loadMS)
}

func residencyRSSDeltaForStore(rssDeltaBytes int64) (int64, error) {
	if rssDeltaBytes > maxResidencyRSSDeltaBytes || rssDeltaBytes < -maxResidencyRSSDeltaBytes {
		return 0, fmt.Errorf(
			"residency rss_delta_bytes=%d outside the operational range [-%d,%d]",
			rssDeltaBytes, maxResidencyRSSDeltaBytes, maxResidencyRSSDeltaBytes,
		)
	}
	return rssDeltaBytes, nil
}

func validateHeartbeatResidentModels(models []ResidentModel) error {
	if len(models) == 0 {
		return nil
	}
	ids := make([]string, 0, len(models))
	for i, m := range models {
		if strings.TrimSpace(m.ModelID) == "" {
			return fmt.Errorf("resident_models[%d] has an empty model_id", i)
		}
		if _, err := residencyLoadMSForStore(m.LoadMS); err != nil {
			return fmt.Errorf("resident_models[%d] model %q: %w", i, m.ModelID, err)
		}
		if _, err := residencyRSSDeltaForStore(m.RSSDeltaBytes); err != nil {
			return fmt.Errorf("resident_models[%d] model %q: %w", i, m.ModelID, err)
		}
		ids = append(ids, m.ModelID)
	}
	// Resident models may include service-lease profile aliases so measured
	// warmth can authorize offer capacity under the same model_id key.
	return validateHeartbeatModelIDs("resident_models", ids, true)
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
	if !engineBuildHashPattern.MatchString(cap.BuildHash) {
		return fmt.Errorf("build_hash must be exactly 16 lowercase hexadecimal characters")
	}
	if !validCanonicalHardwareIdentity(cap.HardwareIdentity) {
		return fmt.Errorf(
			"hardware_identity must be a non-empty canonical exact device identity no longer than %d bytes",
			maxHardwareIdentityBytes)
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
	return validateHeartbeatModelIDs("loaded_models", models, false)
}

// validateHeartbeatModelIDs checks uniqueness and authority for model id lists
// on the heartbeat. When allowServiceAlias is true, governed vLLM profile
// model_alias values are accepted in addition to the advertised production
// projection — required so measured service-lease residency can land in
// worker_model_state under the same key the offer join uses.
func validateHeartbeatModelIDs(field string, models []string, allowServiceAlias bool) error {
	if err := validateUniqueRuntimeStrings(field, models); err != nil {
		return err
	}
	for _, model := range models {
		ok := advertisedRuntimeModel(model) || directedRuntimeModel(model) || documentRuntimeModel(model)
		if allowServiceAlias {
			ok = measurableResidencyModel(model) || documentRuntimeModel(model)
		}
		if !ok {
			return fmt.Errorf("%s model %q is not a known runtime-authority model", field, model)
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
	// Wire kinds are collected as a SET, not asserted unique.
	//
	// This check used to refuse two routable cells declaring different wire
	// kinds for one model. That was right while one runtime existed and is wrong
	// now: artifact format belongs to the (runtime, model) pair. candle loads
	// all-minilm-l6-v2 from safetensors and llama.cpp loads the same logical
	// model from a GGUF, and their embeddings agree at 0.999999 cosine against a
	// 0.999 gate — the divergence a uniqueness rule was guarding against does not
	// exist here.
	//
	// What is still enforced: every advertised wire kind must be one an agent can
	// load, and the catalogue's own kind must be one of them, so the DB row and
	// the runtimes cannot describe unrelated artifacts.
	requiredWireKinds := map[string]map[string]bool{}
	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.Model == "" {
			continue
		}
		if cap.MinMemoryGB > requiredMemory[cap.Model] {
			requiredMemory[cap.Model] = cap.MinMemoryGB
		}
		if !knownWireKind(cap.ModelKind) {
			return fmt.Errorf("runtime matrix advertises unknown wire kind %q for model %q",
				cap.ModelKind, cap.Model)
		}
		if requiredWireKinds[cap.Model] == nil {
			requiredWireKinds[cap.Model] = map[string]bool{}
		}
		requiredWireKinds[cap.Model][cap.ModelKind] = true
	}
	for modelID, minMemory := range requiredMemory {
		row, ok := byID[modelID]
		if !ok {
			return fmt.Errorf("runtime matrix advertises model %q but the DB catalog has no row", modelID)
		}
		price, err := modelPrice(row)
		if err != nil {
			return fmt.Errorf("runtime matrix advertises model %q without settlement-bound pricing: %w", modelID, err)
		}
		if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
			return fmt.Errorf("runtime matrix advertises model %q but its DB price is not positive", modelID)
		}
		if _, err := modelReferencePriceUSD(row); err != nil {
			return fmt.Errorf("runtime matrix advertises model %q without USD payout-floor pricing: %w", modelID, err)
		}
		if row.Kind == "" || row.HFRepo == "" {
			return fmt.Errorf("runtime matrix advertises model %q but its DB kind/repository metadata is incomplete", modelID)
		}
		wireKind, err := runtimeWireModelKind(row.Kind)
		if err != nil {
			return fmt.Errorf("runtime matrix advertises model %q with unusable catalog kind: %w", modelID, err)
		}
		if !requiredWireKinds[modelID][wireKind] {
			return fmt.Errorf(
				"catalog kind %q for model %q maps to wire kind %q, which no advertised runtime cell serves",
				row.Kind, modelID, wireKind,
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
	case "builtin":
		return "builtin", nil
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

// advertisedRuntimeCell reports whether a cell is in the buyer-facing projection.
//
// Enrolment projects the DIRECTED set, which is deliberately wider, so this is
// how a stored worker capability records which of the two it came from. A
// directed-only capability is real and claimable — but only by a job whose
// frozen decision names it.
func advertisedRuntimeCell(cellID string) bool {
	for _, candidate := range advertisedRuntimeCapabilities() {
		if candidate.ID == cellID {
			return true
		}
	}
	return false
}
