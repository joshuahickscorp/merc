package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func productionMetalCapability() WorkerCapability {
	return WorkerCapability{
		HWClass:             "apple_silicon_ultra",
		Engine:              "candle",
		BuildHash:           "97acc6fe17daca56",
		BuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		HardwareIdentity:    testOnlyHardwareIdentity,
		MemoryGB:            36,
		SupportedJobs: []string{
			"embed", "batch_infer",
		},
		SupportedModels: []string{
			"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4",
		},
		Benchmarks: []BenchResult{
			{JobType: "embed", ModelID: "all-minilm-l6-v2", EPS: 3000, ThermalOK: true,
				Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
				MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix())},
			{JobType: "batch_infer", ModelID: "llama-3.2-1b-instruct-q4", TPS: 200, ThermalOK: true,
				Unit: "tokens", UnitScope: performanceUnitScopeTokenLikeInputPlusOutputTokens,
				MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix())},
		},
	}
}

func TestAdvertisedWorkerRequiresExactBenchmarkedBuildAndDevice(t *testing.T) {
	installTestOnlyCombinedTokenAuthority(t)
	base := productionMetalCapability()
	_, _, benchmark, err := currentRuntimeCellBenchmarkIdentity("candle-metal-llama1-infer")
	mustf(t, err, "resolve TEST_ONLY exact worker identity: %v")
	base.HWClass = benchmark.HWClass
	base.BuildHash = benchmark.EngineBuildHash
	base.HardwareIdentity = benchmark.HardwareIdentity
	if _, err := projectWorkerRuntimeCapabilities(base); err != nil {
		t.Fatalf("exact benchmarked worker was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*WorkerCapability)
		want   string
	}{
		{"missing build", func(c *WorkerCapability) { c.BuildHash = "" }, "build_hash"},
		{"wrong build", func(c *WorkerCapability) { c.BuildHash = "0000000000000000" }, "does not exactly match"},
		{"missing build policy", func(c *WorkerCapability) { c.BuildIdentityPolicy = "" }, "build_identity_policy"},
		{"retired build policy", func(c *WorkerCapability) { c.BuildIdentityPolicy = "source_only_v0" }, "build_identity_policy"},
		{"missing device", func(c *WorkerCapability) { c.HardwareIdentity = "" }, "hardware_identity"},
		{"wrong device", func(c *WorkerCapability) { c.HardwareIdentity = "Apple M1 Ultra" }, "does not exactly match"},
		{"missing cell benchmark", func(c *WorkerCapability) { c.Benchmarks = nil }, "requires a fresh matching worker benchmark"},
		{"wrong benchmark unit", func(c *WorkerCapability) { c.Benchmarks[1].UnitScope = performanceUnitScopeDecodeOutputTokens }, "unit/scope"},
		{"thermal failure", func(c *WorkerCapability) { c.Benchmarks[1].ThermalOK = false }, "not thermally valid"},
		{"stale benchmark", func(c *WorkerCapability) {
			c.Benchmarks[1].MeasuredUnix = uint64(runtimeCellPerformanceNow().Add(-8 * 24 * time.Hour).Unix())
		}, "not fresh"},
		{"future benchmark", func(c *WorkerCapability) {
			c.Benchmarks[1].MeasuredUnix = uint64(runtimeCellPerformanceNow().Add(time.Hour).Unix())
		}, "not fresh"},
		{"below governed floor", func(c *WorkerCapability) { c.Benchmarks[1].TPS = 1 }, "below governed conservative floor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutant := base
			mutant.Benchmarks = append([]BenchResult(nil), base.Benchmarks...)
			tc.mutate(&mutant)
			if _, err := projectWorkerRuntimeCapabilities(mutant); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mutant worker error=%v, want %q refusal", err, tc.want)
			}
		})
	}
}

func TestWorkerRuntimeProjectionRejectsHostileTelemetryAndIdentity(t *testing.T) {
	// Pin document activation so suite-order quarantine overlays cannot empty the
	// directed set and turn every shape refusal into a zero-cell refusal.
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
	activeRuntimeActivation.Store(documentActivation())
	cases := []struct {
		name    string
		mutate  func(*WorkerCapability)
		pattern string
	}{
		{
			name: "duplicate benchmark tuple",
			mutate: func(c *WorkerCapability) {
				c.Benchmarks[1] = c.Benchmarks[0]
			},
			pattern: "duplicate tuple",
		},
		{
			name: "negative throughput",
			mutate: func(c *WorkerCapability) {
				c.Benchmarks[0].EPS = -1
			},
			pattern: "non-finite or negative",
		},
		{
			name: "nan throughput",
			mutate: func(c *WorkerCapability) {
				c.Benchmarks[0].EPS = float32(math.NaN())
			},
			pattern: "non-finite or negative",
		},
		{
			name: "zero native throughput",
			mutate: func(c *WorkerCapability) {
				c.Benchmarks[0].EPS = 0
				c.Benchmarks[0].TPS = 100
			},
			pattern: "no positive measured throughput",
		},
		{
			name: "implausibly high throughput",
			mutate: func(c *WorkerCapability) {
				c.Benchmarks[0].EPS = maxBenchmarkRate * 2
			},
			pattern: "plausible throughput maximum",
		},
		{
			name: "load uint64 overflow",
			mutate: func(c *WorkerCapability) {
				c.Benchmarks[0].LoadMS = math.MaxUint64
			},
			pattern: "load_ms",
		},
		{
			name: "p99 operational overflow",
			mutate: func(c *WorkerCapability) {
				c.Benchmarks[0].P99MS = maxBenchmarkP99MS + 1
			},
			pattern: "p99_ms",
		},
		{
			name: "oversized build hash",
			mutate: func(c *WorkerCapability) {
				c.BuildHash = strings.Repeat("a", maxWorkerBuildHashBytes+1)
			},
			pattern: "build_hash exceeds",
		},
		{
			name: "version control character",
			mutate: func(c *WorkerCapability) {
				c.AgentVersion = "v1\nforged"
			},
			pattern: "control character",
		},
		{
			name: "nonfinite memory",
			mutate: func(c *WorkerCapability) {
				c.MemoryGB = float32(math.Inf(1))
			},
			pattern: "memory_gb",
		},
		{
			name: "absurd bandwidth",
			mutate: func(c *WorkerCapability) {
				c.MemoryBwGbps = maxWorkerMemoryBwGbps + 1
			},
			pattern: "memory_bw_gbps",
		},
		{
			name: "negative payout floor",
			mutate: func(c *WorkerCapability) {
				c.MinPayoutUsdHr = -1
			},
			pattern: "min_payout_usd_hr",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := productionMetalCapability()
			tc.mutate(&cap)
			err := validateWorkerRuntimeProjection(cap)
			if err == nil || !strings.Contains(err.Error(), tc.pattern) {
				t.Fatalf("error=%v, want substring %q", err, tc.pattern)
			}
		})
	}
}

func TestAdvertisedRuntimeJobModelIsExactNotCartesian(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	// Exercise the catalogue shape under explicit TEST_ONLY exact authorities;
	// checked-in production currently advertises no cell.
	allowed := [][2]string{
		{"embed", "all-minilm-l6-v2"},
		{"batch_infer", "llama-3.2-1b-instruct-q4"},
	}
	for _, pair := range allowed {
		if err := validateAdvertisedRuntimeJobModel(pair[0], pair[1]); err != nil {
			t.Errorf("production tuple %q/%q rejected: %v", pair[0], pair[1], err)
		}
	}

	rejected := [][2]string{
		{"embed", "llama-3.2-1b-instruct-q4"},
		{"unsupported", "all-minilm-l6-v2"},
		{"batch_infer", "unsupported-model"},
		{"media_transcode", "ffmpeg-transcode-v1"},
		{"media_rendering", "svg-scene-render-v1"},
		{"embed", "unsupported-model"},
		{"unsupported", ""},
	}
	for _, pair := range rejected {
		if err := validateAdvertisedRuntimeJobModel(pair[0], pair[1]); err == nil {
			t.Errorf("non-production tuple %q/%q was admitted", pair[0], pair[1])
		}
	}
}

func TestGeneratedRuntimeModelRefOwnsInternalWireKind(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	for _, tc := range []struct {
		job, model, kind string
	}{
		{"embed", "all-minilm-l6-v2", "hf"},
		// batch_infer is BOUND and advertised; the generated kind is the GGUF pin.
		{"batch_infer", "llama-3.2-1b-instruct-q4", "gguf"},
		{"unknown", "unknown", ""},
	} {
		got := generatedRuntimeModelRef(tc.job, tc.model)
		if got.Ref != tc.model || got.Kind != tc.kind {
			t.Fatalf("generatedRuntimeModelRef(%q,%q)=%+v, want kind %q", tc.job, tc.model, got, tc.kind)
		}
	}
}

func TestNormalizeAdvertisedRuntimeModelRefOwnsBuyerIngressKind(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	t.Run("omitted kind is canonicalized", func(t *testing.T) {
		got, err := normalizeAdvertisedRuntimeModelRef("embed", ModelRef{Ref: "all-minilm-l6-v2"})
		mustf(t, err, "omitted kind rejected: %v")
		if got != (ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"}) {
			t.Fatalf("normalized ref=%+v, want generated hf kind", got)
		}
	})

	t.Run("bound batch_infer is advertised", func(t *testing.T) {
		got, err := normalizeAdvertisedRuntimeModelRef("batch_infer", ModelRef{
			Kind: "gguf",
			Ref:  "llama-3.2-1b-instruct-q4",
		})
		mustf(t, err, "BOUND batch_infer refused: %v")
		if got != (ModelRef{Kind: "gguf", Ref: "llama-3.2-1b-instruct-q4"}) {
			t.Fatalf("normalized ref=%+v, want generated gguf kind", got)
		}
	})

	t.Run("explicit mismatch is rejected", func(t *testing.T) {
		_, err := normalizeAdvertisedRuntimeModelRef("embed", ModelRef{
			Kind: "gguf",
			Ref:  "all-minilm-l6-v2",
		})
		if err == nil || !strings.Contains(err.Error(), `no advertised cell serving model.kind="gguf"`) {
			t.Fatalf("mismatch error=%v, want unadvertised-kind rejection", err)
		}
	})
}

func TestWorkerRegistrationConsumesProductionRuntimeProjection(t *testing.T) {
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
	activeRuntimeActivation.Store(documentActivation())

	// Local override only: productionMetalCapability still carries the shared
	// TEST_ONLY/legacy identity used by sibling tests. G070's r5-bound llama
	// cell requires the receipt's exact build/device; embed is parked so the
	// projection is exactly one cell. Do not mutate the shared fixture.
	valid := productionMetalCapability()
	_, _, receipt, err := currentRuntimeCellBenchmarkIdentity("candle-metal-llama1-infer")
	mustf(t, err, "resolve production r5 llama identity: %v")
	valid.HWClass = receipt.HWClass
	valid.BuildHash = receipt.EngineBuildHash
	valid.BuildIdentityPolicy = receipt.EngineBuildIdentityPolicy
	valid.HardwareIdentity = receipt.HardwareIdentity
	// Only the bound llama lane is activated/advertised; drop embed claims so
	// benchmark-count matches the single projected cell.
	valid.SupportedJobs = []string{"batch_infer"}
	valid.SupportedModels = []string{"llama-3.2-1b-instruct-q4"}
	valid.Benchmarks = []BenchResult{
		{JobType: "batch_infer", ModelID: "llama-3.2-1b-instruct-q4",
			TPS:       float32(receipt.Throughput["candle_metal"].UnitsPerSecAtOperatingBatch),
			ThermalOK: true,
			Unit:      "tokens", UnitScope: performanceUnitScopeTokenLikeInputPlusOutputTokens,
			MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix())},
	}

	mustf(t, validateWorkerRuntimeProjection(valid), "valid production Metal worker rejected: %v")
	projected, err := projectWorkerRuntimeCapabilities(valid)
	mustf(t, err, "project valid production Metal worker: %v")
	if len(projected) != 1 {
		t.Fatalf("focused worker must project to 1 exact production cell (llama), got %d: %+v",
			len(projected), projected)
	}
	if projected[0].ID != "candle-metal-llama1-infer" || projected[0].Job != "batch_infer" {
		t.Fatalf("projected cell=%+v, want candle-metal-llama1-infer/batch_infer", projected[0])
	}
	seen := map[[2]string]bool{}
	for _, cell := range projected {
		seen[[2]string{cell.Job, cell.Model}] = true
		if cell.Runtime != "candle_metal" || cell.Engine != "candle" {
			t.Errorf("wrong runtime lane entered projection: %+v", cell)
		}
	}
	for _, falseCartesian := range [][2]string{
		{"embed", "llama-3.2-1b-instruct-q4"},
		{"batch_infer", "all-minilm-l6-v2"},
		{"unsupported", "all-minilm-l6-v2"},
	} {
		if seen[falseCartesian] {
			t.Errorf("unsupported Cartesian pair entered exact projection: %v", falseCartesian)
		}
	}

	// Shape refusals use the same local r5-aligned base so identity match is
	// not the first failure.
	r5Base := valid
	cases := []struct {
		name    string
		mutate  func(*WorkerCapability)
		pattern string
	}{
		{name: "unknown engine", mutate: func(c *WorkerCapability) { c.Engine = "other" }, pattern: "no reachable production cell"},
		{name: "unknown hardware", mutate: func(c *WorkerCapability) { c.HWClass = "other" }, pattern: "no reachable production cell"},
		{
			name: "unknown model",
			mutate: func(c *WorkerCapability) {
				c.SupportedModels = append(c.SupportedModels, "other-model")
			},
			pattern: "not advertised",
		},
		{
			name: "unsupported job",
			mutate: func(c *WorkerCapability) {
				c.SupportedJobs = append(c.SupportedJobs, "unsupported")
			},
			pattern: "not advertised",
		},
		{
			name: "benchmark cross product",
			mutate: func(c *WorkerCapability) {
				// Declare both capable model lanes so the cross-product tuple is
				// advertisement-shaped, then prove the non-cell cartesian is
				// refused. Embed remains directed/parked; only llama is sold.
				c.SupportedJobs = []string{"embed", "batch_infer"}
				c.SupportedModels = []string{"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4"}
				c.Benchmarks = []BenchResult{
					c.Benchmarks[0],
					{JobType: "embed", ModelID: "llama-3.2-1b-instruct-q4", EPS: 1},
				}
			},
			pattern: "not an advertised production cell",
		},
		{
			name: "duplicate model",
			mutate: func(c *WorkerCapability) {
				c.SupportedModels = append(c.SupportedModels, "llama-3.2-1b-instruct-q4")
			},
			pattern: "duplicate",
		},
		{
			name: "impossible memory claim",
			mutate: func(c *WorkerCapability) {
				c.MemoryGB = 1
			},
			pattern: "below advertised cell",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := r5Base
			cap.SupportedJobs = append([]string(nil), r5Base.SupportedJobs...)
			cap.SupportedModels = append([]string(nil), r5Base.SupportedModels...)
			cap.Benchmarks = append([]BenchResult(nil), r5Base.Benchmarks...)
			tc.mutate(&cap)
			err := validateWorkerRuntimeProjection(cap)
			if err == nil || !strings.Contains(err.Error(), tc.pattern) {
				t.Fatalf("error=%v, want substring %q", err, tc.pattern)
			}
		})
	}
}

func TestWorkerRegistrationProjectsBuiltinMediaCell(t *testing.T) {
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
	activeRuntimeActivation.Store(documentActivation())
	cap := productionMetalCapability()
	cap.SupportedJobs = []string{"media_transcode"}
	cap.SupportedModels = []string{"ffmpeg-transcode-v1"}
	cap.Benchmarks = nil // media has its own physical throughput receipt, not model TPS

	projected, err := projectWorkerRuntimeCapabilities(cap)
	mustf(t, err, "builtin media worker rejected: %v")
	if len(projected) != 1 {
		t.Fatalf("projected %d cells, want one media cell: %+v", len(projected), projected)
	}
	cell := projected[0]
	if cell.ID != "candle-metal-ffmpeg-transcode" || cell.ModelKind != "builtin" {
		t.Fatalf("projected media cell=%+v, want candle-metal-ffmpeg-transcode/builtin", cell)
	}
}

func TestHeartbeatLoadedModelsStayInsideProductionProjection(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	// A heartbeat may name any runtime-authority model, including CANARY+BOUND
	// media that is document-routable but not buyer-advertised. Unknown names
	// and duplicate ids stay refused. Buyer catalogue advertisement is a
	// different predicate (TestRegistryAndDocumentAgreeOnEveryRoutableCell).
	for _, models := range [][]string{
		{"all-minilm-l6-v2"},
		{"llama-3.2-1b-instruct-q4"},
		{"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4"},
		{"ffmpeg-transcode-v1"},
		{"svg-scene-render-v1"},
	} {
		if err := validateHeartbeatRuntimeModels(models); err != nil {
			t.Fatalf("runtime-authority warm models rejected: %v (%v)", err, models)
		}
	}
	for _, models := range [][]string{
		{"unsupported-model"},
		{"unknown"},
		{"all-minilm-l6-v2", "all-minilm-l6-v2"},
	} {
		if err := validateHeartbeatRuntimeModels(models); err == nil {
			t.Fatalf("non-authoritative warm-model advertisement accepted: %v", models)
		}
	}
}

func TestHeartbeatResidentModelsRefuseOutOfRangeMeasurements(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	if err := validateHeartbeatResidentModels([]ResidentModel{{
		ModelID: "all-minilm-l6-v2", RSSDeltaBytes: 1 << 20, LoadMS: 100,
	}}); err != nil {
		t.Fatalf("in-range residency rejected: %v", err)
	}
	if err := validateHeartbeatResidentModels([]ResidentModel{{
		ModelID: "all-minilm-l6-v2", RSSDeltaBytes: maxResidencyRSSDeltaBytes + 1, LoadMS: 100,
	}}); err == nil {
		t.Fatal("over-max rss_delta_bytes accepted")
	}
	if err := validateHeartbeatResidentModels([]ResidentModel{{
		ModelID: "all-minilm-l6-v2", RSSDeltaBytes: 1, LoadMS: maxBenchmarkLoadMS + 1,
	}}); err == nil {
		t.Fatal("over-max load_ms accepted")
	}
	if err := validateHeartbeatResidentModels([]ResidentModel{{
		ModelID: "not-a-production-model", RSSDeltaBytes: 1, LoadMS: 1,
	}}); err == nil {
		t.Fatal("non-production resident model accepted")
	}
}

func productionCatalogRows() []ModelRow {
	return []ModelRow{
		{ID: "all-minilm-l6-v2", Kind: "embed", PricePer1K: .001, ReferencePricePer1K: .001, PriceReferenceCurrency: "usd", PriceCurrency: "usd", MinMemoryGB: 2, HFRepo: "sentence-transformers/all-MiniLM-L6-v2"},
		{ID: "llama-3.2-1b-instruct-q4", Kind: "gguf", PricePer1K: .002, ReferencePricePer1K: .002, PriceReferenceCurrency: "usd", PriceCurrency: "usd", MinMemoryGB: 4, HFRepo: "unsloth/Llama-3.2-1B-Instruct-GGUF"},
		{ID: "ffmpeg-transcode-v1", Kind: "builtin", PricePer1K: .003, ReferencePricePer1K: .003, PriceReferenceCurrency: "usd", PriceCurrency: "usd", MinMemoryGB: 1, HFRepo: "joshuahickscorp/merc"},
		{ID: "svg-scene-render-v1", Kind: "builtin", PricePer1K: .003, ReferencePricePer1K: .003, PriceReferenceCurrency: "usd", PriceCurrency: "usd", MinMemoryGB: 1, HFRepo: "joshuahickscorp/merc"},
	}
}

func TestAdvertisedRuntimeCatalogFailsClosedOnDrift(t *testing.T) {
	installBoundCataloguePublicationAuthorityForTest(t)
	mustf(t, validateAdvertisedRuntimeCatalogRows(productionCatalogRows()), "valid production catalog rejected: %v")

	t.Run("missing row", func(t *testing.T) {
		// Only advertised models are required; an empty catalogue misses embed.
		rows := []ModelRow{}
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "no row") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("zero price", func(t *testing.T) {
		rows := productionCatalogRows()
		rows[0].PricePer1K = 0
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "price") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("understated memory", func(t *testing.T) {
		rows := productionCatalogRows()
		// Index 0 is the advertised embed model.
		rows[0].MinMemoryGB = 0.5
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("missing resolver metadata", func(t *testing.T) {
		rows := productionCatalogRows()
		rows[0].HFRepo = ""
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("unmapped catalog kind", func(t *testing.T) {
		rows := productionCatalogRows()
		rows[0].Kind = "opaque"
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "wire mapping") {
			t.Fatalf("error=%v", err)
		}
	})
	// The catalog row must describe an artifact SOME advertised cell actually
	// serves. This used to read "the one wire kind this model requires"; wire
	// kind now belongs to the (runtime, model) pair, so a model may legitimately
	// have several — but not one that nothing serves.
	t.Run("supported but wrong wire kind", func(t *testing.T) {
		rows := productionCatalogRows()
		rows[0].Kind = "gguf"
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "no advertised runtime cell serves") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestGeneratedRuntimeCapabilitiesBindCanonicalWireKind(t *testing.T) {
	want := map[string]string{
		"all-minilm-l6-v2":         "hf",
		"llama-3.2-1b-instruct-q4": "gguf",
		"ffmpeg-transcode-v1":      "builtin",
		"svg-scene-render-v1":      "builtin",
	}
	for _, cap := range advertisedRuntimeCapabilities() {
		if cap.ModelKind == "" {
			t.Fatalf("advertised cell %q has no generated model kind", cap.ID)
		}
		if cap.ModelKind != want[cap.Model] {
			t.Fatalf("cell %q model %q kind=%q, want %q", cap.ID, cap.Model, cap.ModelKind, want[cap.Model])
		}
	}
}

func TestRuntimeWireModelKind(t *testing.T) {
	for catalog, want := range map[string]string{
		"gguf": "gguf", "hf": "hf", "embed": "hf", "builtin": "builtin",
	} {
		got, err := runtimeWireModelKind(catalog)
		if err != nil || got != want {
			t.Fatalf("runtimeWireModelKind(%q)=(%q,%v), want %q", catalog, got, err, want)
		}
	}
	for _, kind := range []string{"unsupported", "archive", "remote"} {
		if _, err := runtimeWireModelKind(kind); err == nil {
			t.Fatalf("unmapped catalog kind %q must fail closed", kind)
		}
	}
}
