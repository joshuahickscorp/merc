package main

import (
	"math"
	"strings"
	"testing"
)

func productionMetalCapability() WorkerCapability {
	return WorkerCapability{
		HWClass:  "apple_silicon_pro",
		Engine:   "candle",
		MemoryGB: 36,
		SupportedJobs: []string{
			"embed", "batch_infer",
		},
		SupportedModels: []string{
			"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4",
		},
		Benchmarks: []BenchResult{
			{JobType: "embed", ModelID: "all-minilm-l6-v2", EPS: 10, ThermalOK: true},
			{JobType: "batch_infer", ModelID: "llama-3.2-1b-instruct-q4", TPS: 10, ThermalOK: true},
		},
	}
}

func TestWorkerRuntimeProjectionRejectsHostileTelemetryAndIdentity(t *testing.T) {
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
	for _, tc := range []struct {
		job, model, kind string
	}{
		{"embed", "all-minilm-l6-v2", "hf"},
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
	t.Run("omitted kind is canonicalized", func(t *testing.T) {
		got, err := normalizeAdvertisedRuntimeModelRef("embed", ModelRef{Ref: "all-minilm-l6-v2"})
		if err != nil {
			t.Fatalf("omitted kind rejected: %v", err)
		}
		if got != (ModelRef{Kind: "hf", Ref: "all-minilm-l6-v2"}) {
			t.Fatalf("normalized ref=%+v, want generated hf kind", got)
		}
	})

	t.Run("matching explicit kind remains canonical", func(t *testing.T) {
		got, err := normalizeAdvertisedRuntimeModelRef("batch_infer", ModelRef{
			Kind: "gguf",
			Ref:  "llama-3.2-1b-instruct-q4",
		})
		if err != nil {
			t.Fatalf("matching kind rejected: %v", err)
		}
		if got.Kind != "gguf" {
			t.Fatalf("normalized kind=%q, want gguf", got.Kind)
		}
	})

	t.Run("explicit mismatch is rejected", func(t *testing.T) {
		_, err := normalizeAdvertisedRuntimeModelRef("embed", ModelRef{
			Kind: "gguf",
			Ref:  "all-minilm-l6-v2",
		})
		if err == nil || !strings.Contains(err.Error(), `requires model.kind="hf"`) {
			t.Fatalf("mismatch error=%v, want generated-kind rejection", err)
		}
	})
}

func TestWorkerRegistrationConsumesProductionRuntimeProjection(t *testing.T) {
	valid := productionMetalCapability()
	if err := validateWorkerRuntimeProjection(valid); err != nil {
		t.Fatalf("valid production Metal worker rejected: %v", err)
	}
	projected, err := projectWorkerRuntimeCapabilities(valid)
	if err != nil {
		t.Fatalf("project valid production Metal worker: %v", err)
	}
	if len(projected) != 2 {
		t.Fatalf("focused worker must project to 2 exact production cells, got %d", len(projected))
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

	cases := []struct {
		name    string
		mutate  func(*WorkerCapability)
		pattern string
	}{
		{name: "unknown engine", mutate: func(c *WorkerCapability) { c.Engine = "other" }, pattern: "no advertised production cell"},
		{name: "unknown hardware", mutate: func(c *WorkerCapability) { c.HWClass = "other" }, pattern: "no advertised production cell"},
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
				c.Benchmarks[1] = BenchResult{JobType: "embed", ModelID: "llama-3.2-1b-instruct-q4", EPS: 1}
			},
			pattern: "not an advertised production cell",
		},
		{
			name: "duplicate model",
			mutate: func(c *WorkerCapability) {
				c.SupportedModels = append(c.SupportedModels, "all-minilm-l6-v2")
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
			cap := productionMetalCapability()
			tc.mutate(&cap)
			err := validateWorkerRuntimeProjection(cap)
			if err == nil || !strings.Contains(err.Error(), tc.pattern) {
				t.Fatalf("error=%v, want substring %q", err, tc.pattern)
			}
		})
	}
}

func TestHeartbeatLoadedModelsStayInsideProductionProjection(t *testing.T) {
	if err := validateHeartbeatRuntimeModels([]string{"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4"}); err != nil {
		t.Fatalf("production warm models rejected: %v", err)
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

func productionCatalogRows() []ModelRow {
	return []ModelRow{
		{ID: "all-minilm-l6-v2", Kind: "embed", PricePer1K: .001, MinMemoryGB: 2, HFRepo: "sentence-transformers/all-MiniLM-L6-v2"},
		{ID: "llama-3.2-1b-instruct-q4", Kind: "gguf", PricePer1K: .002, MinMemoryGB: 4, HFRepo: "unsloth/Llama-3.2-1B-Instruct-GGUF"},
	}
}

func TestAdvertisedRuntimeCatalogFailsClosedOnDrift(t *testing.T) {
	if err := validateAdvertisedRuntimeCatalogRows(productionCatalogRows()); err != nil {
		t.Fatalf("valid production catalog rejected: %v", err)
	}

	t.Run("missing row", func(t *testing.T) {
		rows := productionCatalogRows()[:1]
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
		rows[1].MinMemoryGB = 3
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("missing resolver metadata", func(t *testing.T) {
		rows := productionCatalogRows()
		rows[1].HFRepo = ""
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
	t.Run("supported but wrong wire kind", func(t *testing.T) {
		rows := productionCatalogRows()
		rows[0].Kind = "gguf"
		if err := validateAdvertisedRuntimeCatalogRows(rows); err == nil || !strings.Contains(err.Error(), "requires wire kind") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestGeneratedRuntimeCapabilitiesBindCanonicalWireKind(t *testing.T) {
	want := map[string]string{
		"all-minilm-l6-v2":         "hf",
		"llama-3.2-1b-instruct-q4": "gguf",
	}
	for _, cap := range generatedAdvertisedRuntimeCapabilities {
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
		"gguf": "gguf", "hf": "hf", "embed": "hf",
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
