package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func l12EnsurePriceBoard(t *testing.T) {
	t.Helper()
	if p := strings.TrimSpace(os.Getenv(priceBoardPathEnv)); p != "" {
		if _, err := os.Stat(p); err == nil {
			return
		}
	}
	for _, candidate := range []string{
		filepath.Join("..", "pricing", "board.json"),
		"/tmp/merc-l12/board.json",
	} {
		if _, err := os.Stat(candidate); err == nil {
			abs, aerr := filepath.Abs(candidate)
			mustf(t, aerr, "price board: %v")
			t.Setenv(priceBoardPathEnv, abs)
			return
		}
	}
	t.Fatal("pricing/board.json is not on disk; set MERC_PRICE_BOARD to the governed board")
}

func l12EnsureIsolatedTemplate(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(isolatedTestDBTemplateEnv)) != "" {
		return
	}
	t.Setenv(isolatedTestDBTemplateEnv, schemaTemplateDatabaseName(canonicalSchemaSHA256()))
}

func l12InferJobBody(maxUSD float64) map[string]any {
	return map[string]any{
		"job_type": map[string]any{"type": "batch_infer", "max_tokens": 16},
		"model":    map[string]any{"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
		"tier":     "batch",
		"input":    `{"id":"l12-0","prompt":"operator-controlled l12 infer rehearsal"}` + "\n",
		"max_usd":  maxUSD,
		"verification": map[string]any{
			// Current uniform-task economics admit one primary plus exact
			// redundancy clones and refuse honeypots (heterogeneous, no per-task
			// allocation). 0.1 would also round to zero honeypots on one record.
			"redundancy_frac": 1.0,
			"honeypot_frac":   0.0,
		},
	}
}

func l12EmbedJobBody(maxUSD float64) map[string]any {
	return map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":     "batch",
		"input":    `{"id":"l12-embed","text":"operator-controlled l12 embed probe"}` + "\n",
		"max_usd":  maxUSD,
	}
}

func l12WriteReceipt(t *testing.T, name string, doc map[string]any) {
	t.Helper()
	root, err := filepath.Abs("..")
	mustf(t, err, "repo root: %v")
	path := filepath.Join(root, "evidence", "canary", "l12-p1-canary-rehearsal-"+name+".json")
	stamped := map[string]any{
		"schema_version":         1,
		"kind":                   "p1_canary_rehearsal_" + name,
		"gate":                   "P1-CANARY-REHEARSAL",
		"classification":         "ALPHA_CONTROL",
		"does_not_satisfy":       "EXTERNAL_ALPHA_PROVEN",
		"participant_class":      "operator_controlled",
		"synthetic":              true,
		"controlled_by_operator": true,
		"operator_owned":         true,
		"external_alpha_proven":  false,
		"observed_at":            time.Now().UTC().Format(time.RFC3339),
		"rehearsal":              true,
	}
	for k, v := range doc {
		stamped[k] = v
	}
	body, err := json.MarshalIndent(stamped, "", "  ")
	mustf(t, err, "render receipt: %v")
	mustf(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir: %v")
	mustf(t, os.WriteFile(path, append(body, '\n'), 0o644), "write %s: %v", path)
	t.Logf("wrote %s", path)
}

func l12SealedInferCapability(t *testing.T) (WorkerCapability, benchmarkReceiptSummary) {
	t.Helper()
	profile, _, bench, err := currentRuntimeCellBenchmarkIdentity("candle-metal-llama1-infer")
	mustf(t, err, "infer identity: %v")
	rate := bench.Throughput[profile.RuntimeID].UnitsPerSecAtOperatingBatch
	session := uuid.New()
	return WorkerCapability{
		HWClass:             bench.HWClass,
		Engine:              "candle",
		BuildHash:           bench.EngineBuildHash,
		BuildIdentityPolicy: bench.EngineBuildIdentityPolicy,
		HardwareIdentity:    bench.HardwareIdentity,
		MemoryGB:            96,
		MemoryBwGbps:        800,
		SupportedJobs:       []string{"batch_infer"},
		SupportedModels:     []string{"llama-3.2-1b-instruct-q4"},
		AgentVersion:        "0.1.0",
		OSVersion:           "macos",
		Sandboxed:           true,
		AgentSessionID:      &session,
		Benchmarks: []BenchResult{{
			ModelID:      "llama-3.2-1b-instruct-q4",
			JobType:      "batch_infer",
			TPS:          float32(rate),
			ThermalOK:    true,
			Unit:         "tokens",
			UnitScope:    performanceUnitScopeTokenLikeInputPlusOutputTokens,
			MeasuredUnix: uint64(time.Now().Unix()),
		}},
	}, bench
}

func l12AgentBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	mustf(t, err, "repo root: %v")
	for _, rel := range []string{
		filepath.Join("agent", "target", "debug", "merc-agent"),
		filepath.Join("agent", "target", "release", "merc-agent"),
	} {
		path := filepath.Join(root, rel)
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			continue
		}
		cmd := exec.Command(path, "emit-infer-artifact", "--help")
		out, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(string(out), "Emit the exact bytes") {
			return path
		}
		t.Logf("%s lacks emit-infer-artifact: %v\n%s", path, err, out)
	}
	t.Fatal("no merc-agent with emit-infer-artifact; build agent/ after adding the command")
	return ""
}

func TestL12DiagnoseWorkerProjection(t *testing.T) {
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
	activeRuntimeActivation.Store(documentActivation())

	cap, bench := l12SealedInferCapability(t)
	t.Logf("profile=candle_metal cell=candle-metal-llama1-infer hash=%s hw=%s rate=%.4f",
		bench.EngineBuildHash, bench.HardwareIdentity,
		bench.Throughput["candle_metal"].UnitsPerSecAtOperatingBatch)
	if err := validateWorkerRuntimeProjection(cap); err != nil {
		t.Fatalf("projection refused: %v", err)
	}
	if !advertisedRuntimeCell("candle-metal-llama1-infer") {
		t.Fatal("candle-metal-llama1-infer is not in the advertised projection")
	}
}

func TestL12CurrentHostBinaryIsRefusedUnlessItCarriesTheSealedHash(t *testing.T) {
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
	activeRuntimeActivation.Store(documentActivation())

	cap, bench := l12SealedInferCapability(t)
	// Observed 2026-08-17 on this host: the binary that sealed r6 (7cc01c)
	// was overwritten; honeypot-answer now emits 2939a8e26ffe6fd2.
	cap.BuildHash = "2939a8e26ffe6fd2"
	err := validateWorkerRuntimeProjection(cap)
	if err == nil {
		t.Fatal("current-host hash was accepted; sealed r6 identity is no longer required")
	}
	if !strings.Contains(err.Error(), "does not exactly match") {
		t.Fatalf("refusal=%v, want exact-match identity refusal", err)
	}
	t.Logf("named refusal: worker hash 2939a8e26ffe6fd2 vs sealed %s: %v",
		bench.EngineBuildHash, err)
}

func TestL12QuoteRefusalChainFromSource(t *testing.T) {
	previous := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previous) })
	activeRuntimeActivation.Store(documentActivation())

	if err := validateAdvertisedRuntimeJobModel("embed", "all-minilm-l6-v2"); err == nil {
		t.Fatal("embed is advertised; CANARY+BOUND cells must stay off the sold surface")
	} else if !strings.Contains(err.Error(), "not advertised") {
		t.Fatalf("embed refusal=%v, want not-advertised", err)
	}
	if err := validateAdvertisedRuntimeJobModel("batch_infer", "llama-3.2-1b-instruct-q4"); err != nil {
		t.Fatalf("infer should be document-advertised under documentActivation: %v", err)
	}

	embedOK, embedReason := false, ""
	inferOK, inferReason := false, ""
	for _, profile := range runtimeAuthority.Runtimes {
		for _, cell := range profile.Cells {
			ok, reason := cellAuthorityBindable(profile, cell)
			switch cell.ID {
			case "candle-metal-minilm-embed":
				embedOK, embedReason = ok, reason
			case "candle-metal-llama1-infer":
				inferOK, inferReason = ok, reason
			}
		}
	}
	if !embedOK {
		t.Fatalf("embed does not bind under r3: %s", embedReason)
	}
	if !inferOK {
		t.Fatalf("infer does not bind: %s", inferReason)
	}

	l12WriteReceipt(t, "quote-refusal-chain", map[string]any{
		"status": "PASS",
		"plane":  "local-source",
		"chain": []string{
			`POST /v1/quote → Server.handleQuote (control/quote.go)`,
			`normalizeWorkloadRequest(sub, false) — structural only`,
			`store.activationForNewAdmission — refresh currentActivation() from PostgreSQL`,
			`normalizeWorkloadRequest(sub) → normalizeAndValidateJobSubmit (control/job_submit_validate.go)`,
			`normalizeAdvertisedRuntimeModelRef → validateAdvertisedRuntimeJobModel (control/runtime_matrix.go)`,
			`advertisedRuntimeJobModel iterates advertisedRuntimeCapabilities()`,
			`advertisedRuntimeCapabilities() = currentActivation().advertised (control/activation_policy.go)`,
			`cellRoutable requires EffectiveLifecycle == ACTIVE AND cell.Routable`,
			`authorityCell.Routable requires CANARY/ACTIVE plus cellAuthorityBindable (control/runtime_authority.go)`,
			`cellAuthorityBindable requires binding_status=BOUND, 16-hex engine_build_hash, exact Apple hardware_identity (control/cell_authority_binding.go)`,
			`HTTP 400 writeErr: "runtime capability is not advertised for job_type=%q model=%q (matrix %s)"`,
			`A registered worker is not consulted at this 400; EligibleWorkerCount is later and advisory`,
			`activationSnapshotFrom may QUARANTINE an ACTIVE stored row when storedRoutableEntryHasCurrentGlobalAuthority fails`,
			`projectWorkerRuntimeCapabilities then requires the sealed hw_class/build_hash/hardware_identity to offer an advertised cell`,
		},
		"embed_bindable":             embedOK,
		"embed_bind_refusal":         embedReason,
		"infer_bindable":             inferOK,
		"infer_bind_refusal":         inferReason,
		"infer_document_advertised":  advertisedRuntimeCell("candle-metal-llama1-infer"),
		"embed_document_advertised":  advertisedRuntimeCell("candle-metal-minilm-embed"),
		"sealed_infer_engine_hash":   func() string { _, b := l12SealedInferCapability(t); return b.EngineBuildHash }(),
		"current_host_observed_hash": "2939a8e26ffe6fd2",
		"external_alpha_proven":      false,
	})
}
