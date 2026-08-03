package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompetitiveCUDAParityMatrixSelection(t *testing.T) {
	sel := CompetitiveCUDAParityMatrixSelection()
	if len(sel.Selected) < 18 || len(sel.Selected) > 40 {
		t.Fatalf("competitive selected=%d; want a bounded matrix covering the competitive ladder", len(sel.Selected))
	}
	var hasC64, hasC128, hasMedium, hasCold, hasWarm bool
	for _, c := range sel.Selected {
		switch c.Concurrency {
		case 64:
			hasC64 = true
		case 128:
			hasC128 = true
		}
		if c.PromptTokens == 256 {
			hasMedium = true
		}
		if c.State == "cold" {
			hasCold = true
		}
		if c.State == "warm" {
			hasWarm = true
		}
	}
	if !hasC64 || !hasC128 {
		t.Fatalf("competitive matrix must include c=64 and c=128: c64=%v c128=%v", hasC64, hasC128)
	}
	if !hasMedium || !hasCold || !hasWarm {
		t.Fatalf("competitive matrix missing medium/cold/warm: medium=%v cold=%v warm=%v", hasMedium, hasCold, hasWarm)
	}
	// Overhead warm@c=128 must be present so the high concurrency claim is real.
	found := false
	for _, c := range sel.Selected {
		if c.Concurrency == 128 && c.PromptTokens == 32 && c.OutputTokens == 16 && c.State == "warm" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("missing overhead warm c=128 cell")
	}
}

func TestGatewayParityMatrixDefaultSubsetIsDefensible(t *testing.T) {
	sel := DefaultGatewayParityMatrixSelection()
	if len(sel.Selected) < 12 || len(sel.Selected) > 24 {
		t.Fatalf("selected=%d; want a defensible ~16–20 cell subset, not the full 1440", len(sel.Selected))
	}
	if sel.Rationale == "" || len(sel.DroppedSummary) == 0 || len(sel.DroppedAxes) == 0 {
		t.Fatal("subset must carry rationale, dropped_summary, and dropped_axes (silent truncation forbidden)")
	}
	// Must cover the corners that change the answer.
	var hasOverhead, hasPrefill, hasDecode bool
	var hasCold, hasWarm, hasPrefix bool
	var hasC1, hasC8, hasC32 bool
	for _, c := range sel.Selected {
		if c.PromptTokens == 32 && c.OutputTokens == 16 {
			hasOverhead = true
		}
		if c.PromptTokens == 8192 && c.OutputTokens == 16 {
			hasPrefill = true
		}
		if c.PromptTokens == 32 && c.OutputTokens == 512 {
			hasDecode = true
		}
		switch c.State {
		case "cold":
			hasCold = true
		case "warm":
			hasWarm = true
		case "prefix-hit":
			hasPrefix = true
		}
		switch c.Concurrency {
		case 1:
			hasC1 = true
		case 8:
			hasC8 = true
		case 32:
			hasC32 = true
		}
	}
	if !hasOverhead || !hasPrefill || !hasDecode {
		t.Fatalf("subset missing a shape corner: overhead=%v prefill=%v decode=%v", hasOverhead, hasPrefill, hasDecode)
	}
	if !hasCold || !hasWarm || !hasPrefix {
		t.Fatalf("subset missing a state: cold=%v warm=%v prefix-hit=%v", hasCold, hasWarm, hasPrefix)
	}
	if !hasC1 || !hasC8 || !hasC32 {
		t.Fatalf("subset must cover concurrency ladder {1,8,32}: c1=%v c8=%v c32=%v", hasC1, hasC8, hasC32)
	}
	// Traffic class must be explicitly dropped, not silently omitted.
	foundTC := false
	for _, d := range sel.DroppedAxes {
		if d.Axis == "traffic_class" {
			foundTC = true
			if !strings.Contains(d.Reason, "non-acting") {
				t.Fatalf("traffic_class drop reason must name non-acting: %s", d.Reason)
			}
		}
	}
	if !foundTC {
		t.Fatal("traffic_class must appear in dropped_axes")
	}
	// Full axes document 1440.
	axes := FullGatewayParityAxes()
	if axes.FullCellCount != 1440 {
		t.Fatalf("full_cell_count=%d want 1440", axes.FullCellCount)
	}
	if axes.ActingCellCount != 480 {
		t.Fatalf("acting_cell_count=%d want 480", axes.ActingCellCount)
	}
}

func TestGatewayParityStateProofRules(t *testing.T) {
	// Cold requires both markers.
	p := GatewayParityStateProof{ClaimedState: "cold", ColdNoWarmup: true, ColdFreshClient: true}
	VerifyGatewayParityStateProof(&p)
	if !p.Verified {
		t.Fatalf("cold should verify: %s", p.Detail)
	}
	p = GatewayParityStateProof{ClaimedState: "cold", ColdNoWarmup: true}
	VerifyGatewayParityStateProof(&p)
	if p.Verified {
		t.Fatal("cold without fresh client must not verify")
	}

	// Warm requires both arms.
	p = GatewayParityStateProof{ClaimedState: "warm", WarmupRequestsOK: 2, WarmupVerified: true}
	VerifyGatewayParityStateProof(&p)
	if !p.Verified {
		t.Fatalf("warm should verify: %s", p.Detail)
	}
	p = GatewayParityStateProof{ClaimedState: "warm", WarmupRequestsOK: 1, WarmupVerified: true}
	VerifyGatewayParityStateProof(&p)
	if p.Verified {
		t.Fatal("warm with one arm must not verify")
	}

	// Prefix-hit without signal refuses.
	p = GatewayParityStateProof{ClaimedState: "prefix-hit", PrefixPrimeOK: true, SamplesOK: 20}
	VerifyGatewayParityStateProof(&p)
	if p.Verified {
		t.Fatal("prefix-hit with no cache signal must not verify")
	}
	if !strings.Contains(p.Detail, "cached_tokens") {
		t.Fatalf("detail must name cached_tokens: %s", p.Detail)
	}

	// Prefix-hit with enough hits verifies.
	p = GatewayParityStateProof{
		ClaimedState: "prefix-hit", PrefixPrimeOK: true,
		SamplesOK: 20, SamplesWithCacheSignal: 20, SamplesWithCacheHit: 18,
		CachedTokensMin: 8, CachedTokensMax: 32,
	}
	VerifyGatewayParityStateProof(&p)
	if !p.Verified {
		t.Fatalf("prefix-hit should verify: %s", p.Detail)
	}

	// Below 80% refuses.
	p = GatewayParityStateProof{
		ClaimedState: "prefix-hit", PrefixPrimeOK: true,
		SamplesOK: 20, SamplesWithCacheSignal: 20, SamplesWithCacheHit: 10,
	}
	VerifyGatewayParityStateProof(&p)
	if p.Verified {
		t.Fatal("prefix-hit below 80% hit fraction must not verify")
	}
}

func TestGatewayParityPrefixHitCellRefusesWithoutCacheSignal(t *testing.T) {
	// Stand-in that NEVER reports cached_tokens — prefix-hit must refuse.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Merc-Upstream-Body-SHA256", sha256Hex(body))
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1,\"total_tokens\":9}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	base := srv.URL + "/v1"
	contract := GatewayParitySamplingContract{
		Model: "stand-in", Prompt: "p", Temperature: 0, TopP: 1, MaxTokens: 8, Stream: true,
		ModelDigest: strings.Repeat("22", 32),
	}
	spec := GatewayParityCellSpec{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "prefix-hit"}
	cell := RunGatewayParityMatrixCell(
		context.Background(), base, "k", base, "k", contract, spec, GatewayParitySampleFloor(1),
	)
	if cell.Status != "REFUSED" {
		t.Fatalf("status=%s want REFUSED when no cache signal; proof=%+v", cell.Status, cell.StateProof)
	}
	if cell.StateProof.Verified {
		t.Fatal("state_proof.verified must be false")
	}
	if !strings.Contains(cell.Reason, "cached_tokens") && !strings.Contains(cell.StateProof.Detail, "cached_tokens") {
		t.Fatalf("refusal must name cached_tokens: reason=%q detail=%q", cell.Reason, cell.StateProof.Detail)
	}
	if cell.Gate.Passed || cell.Gate.Verdict == "PASS" {
		t.Fatal("unverified prefix-hit must not PASS the cell gate")
	}
}

func TestGatewayParityPerCellRefuseDoesNotBleed(t *testing.T) {
	// One cell fails (prefix-hit no signal), another passes structure on warm.
	// Aggregate must FAIL; the warm cell's local verdict must remain independent.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Merc-Upstream-Body-SHA256", sha256Hex(body))
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// No cached_tokens ever.
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	base := srv.URL + "/v1"
	contract := GatewayParitySamplingContract{
		Model: "stand-in", Prompt: "p", Temperature: 0, TopP: 1, MaxTokens: 8, Stream: true,
		ModelDigest: strings.Repeat("33", 32),
	}
	selection := GatewayParityMatrixSelection{
		Selected: []GatewayParityCellSpec{
			{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "warm"},
			{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "prefix-hit"},
		},
		Rationale: "unit test: per-cell refuse isolation",
	}
	// Thin n for speed — warm@c=1 floor is 20; use floor exactly.
	cells := make([]GatewayParityCellResult, 0, 2)
	for _, spec := range selection.Selected {
		cells = append(cells, RunGatewayParityMatrixCell(
			context.Background(), base, "k", base, "k", contract, spec, GatewayParitySampleFloor(1),
		))
	}
	rec := BuildGatewayParityMatrixReceipt(
		contract,
		GatewayParityNetworkTopology{Notes: "unit"},
		selection, cells, DefaultGatewayParityBudget(),
		CaptureGatewayParityHostLoad(), CaptureGatewayParityHostLoad(),
		"HARNESS_SELF_TEST",
		[]string{"unit test"},
	)
	if rec.GatePassed || rec.Comparable {
		t.Fatal("receipt with a refused cell must not pass/comparable")
	}
	var warm, prefix *GatewayParityCellResult
	for i := range rec.Cells {
		switch rec.Cells[i].Spec.State {
		case "warm":
			warm = &rec.Cells[i]
		case "prefix-hit":
			prefix = &rec.Cells[i]
		}
	}
	if warm == nil || prefix == nil {
		t.Fatal("both cells required")
	}
	if prefix.Status != "REFUSED" {
		t.Fatalf("prefix-hit status=%s want REFUSED", prefix.Status)
	}
	// Warm may MEASURED; its claim must not be re-labeled as the prefix cell.
	if warm.Spec.Key() == prefix.Spec.Key() {
		t.Fatal("cell keys collided")
	}
	// Top-level refusals must name the prefix cell specifically.
	joined := strings.Join(rec.Refusals, " | ")
	if !strings.Contains(joined, "prefix-hit") && !strings.Contains(joined, prefix.Spec.Key()) {
		t.Fatalf("top-level refusals must identify the failing cell: %v", rec.Refusals)
	}
}

func TestGatewayParityMatrixSelfTestAcrossDimensions(t *testing.T) {
	// Dual stand-in: merc slower by fixed delay; prefix marker injects cached_tokens.
	var mercHits, directHits atomic.Int64
	makeHandler := func(hits *atomic.Int64, extra time.Duration) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			reqBody, _ := io.ReadAll(r.Body)
			engine := time.Duration(len(reqBody)/200) * time.Millisecond
			if engine < 2*time.Millisecond {
				engine = 2 * time.Millisecond
			}
			if engine > 40*time.Millisecond {
				engine = 40 * time.Millisecond
			}
			time.Sleep(engine + extra)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Merc-Upstream-Body-SHA256", sha256Hex(reqBody))
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			cached := 0
			if bytes.Contains(reqBody, []byte(gatewayParitySharedPrefixMarker)) {
				cached = 16
			}
			usage := fmt.Sprintf(
				`{"prompt_tokens":32,"completion_tokens":1,"total_tokens":33,"prompt_tokens_details":{"cached_tokens":%d}}`,
				cached,
			)
			if cached == 0 {
				usage = `{"prompt_tokens":32,"completion_tokens":1,"total_tokens":33}`
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":"+usage+"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		})
	}
	mercLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	directLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mercSrv := &http.Server{Handler: makeHandler(&mercHits, 5*time.Millisecond)}
	directSrv := &http.Server{Handler: makeHandler(&directHits, 0)}
	go mercSrv.Serve(mercLn)
	go directSrv.Serve(directLn)
	defer mercSrv.Close()
	defer directSrv.Close()

	mercBase := "http://" + mercLn.Addr().String() + "/v1"
	directBase := "http://" + directLn.Addr().String() + "/v1"
	contract := GatewayParitySamplingContract{
		Model: "stand-in", Prompt: "self-test", Temperature: 0, TopP: 1.0,
		MaxTokens: 8, Stream: true, ModelDigest: strings.Repeat("11", 32),
	}
	// Self-test selection includes c=32 which needs 160 samples — heavy.
	// Use a thinner but still multi-dimensional selection for the unit test;
	// the CLI self-test path uses SelfTestGatewayParityMatrixSelection.
	selection := GatewayParityMatrixSelection{
		Selected: []GatewayParityCellSpec{
			{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "warm"},
			{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "cold"},
			{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "prefix-hit"},
			{Concurrency: 1, PromptTokens: 256, OutputTokens: 16, State: "warm"},
			{Concurrency: 1, PromptTokens: 32, OutputTokens: 128, State: "warm"},
			{Concurrency: 8, PromptTokens: 32, OutputTokens: 16, State: "warm"},
		},
		Rationale: "unit self-test across dimensions (thinner ladder than live CLI)",
		DroppedSummary: []string{"unit test subset"},
		DroppedAxes: []GatewayParityDroppedAxis{
			{Axis: "traffic_class", Values: gatewayParityFullTrafficClasses, Reason: TrafficClassNonActingNote},
		},
	}
	hostStart := CaptureGatewayParityHostLoad()
	cells := RunGatewayParityMatrix(context.Background(), mercBase, "k", directBase, "k", contract, selection)
	hostEnd := CaptureGatewayParityHostLoad()
	rec := BuildGatewayParityMatrixReceipt(
		contract,
		GatewayParityNetworkTopology{ClientHost: "test", ControlPlane: "none (self-test)", Engine: "dual stand-in"},
		selection, cells, DefaultGatewayParityBudget(), hostStart, hostEnd,
		"HARNESS_SELF_TEST",
		[]string{"matrix self-test unit", "NOT parity evidence"},
	)

	if rec.Comparable || rec.GatePassed {
		t.Fatal("self-test must remain non-comparable and not gate_passed")
	}
	if rec.EvidenceClass != "HARNESS_SELF_TEST" {
		t.Fatalf("evidence_class=%s", rec.EvidenceClass)
	}
	if rec.ColdWarmState != "per-cell" {
		t.Fatalf("cold_warm_state=%q want per-cell", rec.ColdWarmState)
	}
	if rec.FullAxes == nil || rec.TrafficClassNote == "" {
		t.Fatal("matrix receipt must carry full_axes and traffic_class_note")
	}
	if len(rec.Cells) != len(selection.Selected) {
		t.Fatalf("cells=%d want %d", len(rec.Cells), len(selection.Selected))
	}

	seen := map[string]bool{}
	var prefixOK, coldOK, warmOK bool
	for _, cell := range rec.Cells {
		seen[cell.Spec.State] = true
		t.Logf("cell %s status=%s gate=%s verified=%v rel=%v",
			cell.Spec.Key(), cell.Status, cell.Gate.Verdict, cell.StateProof.Verified, cell.RelativeOverhead)
		switch cell.Spec.State {
		case "prefix-hit":
			// With injected cached_tokens the cell should MEASURE and verify.
			if cell.Status == "MEASURED" && cell.StateProof.Verified {
				prefixOK = true
			} else {
				t.Logf("prefix-hit detail: %s reason=%s", cell.StateProof.Detail, cell.Reason)
			}
		case "cold":
			if cell.StateProof.ColdNoWarmup && cell.StateProof.ColdFreshClient && cell.StateProof.Verified {
				coldOK = true
			}
		case "warm":
			if cell.StateProof.WarmupVerified && cell.StateProof.Verified {
				warmOK = true
			}
		}
	}
	if !seen["cold"] || !seen["warm"] || !seen["prefix-hit"] {
		t.Fatalf("missing states in cells: %v", seen)
	}
	if !prefixOK {
		t.Fatal("prefix-hit cell should verify when stand-in injects cached_tokens")
	}
	if !coldOK {
		t.Fatal("cold cell should verify operational cold path")
	}
	if !warmOK {
		t.Fatal("warm cell should verify warm-up on both arms")
	}
	if rec.ShapeInsight == nil || rec.ShapeInsight.Finding == "" {
		t.Fatal("shape_insight must be present")
	}
	t.Logf("shape_insight: %s", rec.ShapeInsight.Finding)

	// Relabel attack: PARITY_EVIDENCE on self-test stand-in must not pass.
	attack := BuildGatewayParityMatrixReceipt(
		contract,
		GatewayParityNetworkTopology{Notes: "attack"},
		selection, cells, DefaultGatewayParityBudget(), hostStart, hostEnd,
		"PARITY_EVIDENCE", nil,
	)
	if attack.GatePassed || attack.Comparable {
		t.Fatal("relabeling matrix self-test as PARITY_EVIDENCE must not pass")
	}

	// Env-gated bound receipt (same pattern as existing self-test). When sealing,
	// re-run the full SelfTestGatewayParityMatrixSelection so the artifact carries
	// the concurrency ladder and every corner dimension, not the thinner CI subset.
	if os.Getenv("MERC_GATEWAY_PARITY_MATRIX_SELFTEST") == "1" {
		sealSel := SelfTestGatewayParityMatrixSelection()
		sealCells := RunGatewayParityMatrix(context.Background(), mercBase, "k", directBase, "k", contract, sealSel)
		sealRec := BuildGatewayParityMatrixReceipt(
			contract,
			GatewayParityNetworkTopology{
				ClientHost: "local-test-process", ControlPlane: "none (self-test)",
				Engine: "dual stand-in (merc=+5ms fixed; engine cost scales with body)",
				ClientToEngine: "loopback", Notes: "HARNESS_SELF_TEST matrix dimensions",
			},
			sealSel, sealCells, DefaultGatewayParityBudget(), hostStart, CaptureGatewayParityHostLoad(),
			"HARNESS_SELF_TEST",
			[]string{
				"stand-in token timing is artificial; merc arm adds fixed +5ms over scaled engine cost",
				"NOT parity evidence; do not quote as gateway overhead",
				"stand-in omits X-Merc-Contract-ID; body_identity cannot satisfy PARITY_EVIDENCE",
				fmt.Sprintf("matrix cells=%d selection=%s", len(sealCells), sealSel.Rationale),
			},
		)
		if sealRec.Comparable || sealRec.GatePassed {
			t.Fatal("sealed matrix self-test must remain non-comparable")
		}
		outPath := filepath.Join("..", "evidence", "perf", "gateway-parity-v2-matrix-selftest.json")
		raw, err := json.Marshal(sealRec)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		id, bin, err := DefaultBoundIdentity("..", "control/gateway_parity_matrix.go#HARNESS_SELF_TEST",
			"embedded sampling_contract + cells", "embedded cells.*.merc/direct.raw_samples")
		if err != nil {
			t.Fatal(err)
		}
		id.ModelArtifactDigest = IdentitySlotNA("self-test stand-in; no model weights")
		id.ImageDigest = IdentitySlotNA("self-test; no container image")
		id.CorpusDigest = IdentitySlotNA("self-test; no external corpus")
		if err := WriteBoundEvidenceJSON(EvidenceWriteRequest{
			RepoRoot: "..", Path: outPath, Payload: payload,
			Identity: id, BuildBinaryPath: bin,
		}); err != nil {
			t.Fatalf("bound evidence write refused: %v", err)
		}
		t.Logf("wrote matrix self-test receipt to %s cells=%d shape=%s",
			outPath, len(sealRec.Cells), sealRec.ShapeInsight.Finding)
	} else {
		t.Logf("skipping bound write (set MERC_GATEWAY_PARITY_MATRIX_SELFTEST=1 to seal); merc=%d direct=%d",
			mercHits.Load(), directHits.Load())
	}
}

func TestGatewayParityCachedTokensParsedFromUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":1,\"total_tokens\":41,\"prompt_tokens_details\":{\"cached_tokens\":24}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	client := NewGatewayParityClient(1)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"temperature":0,"top_p":1,"stream":true,"stream_options":{"include_usage":true}}`)
	s := client.CompleteOneStream(context.Background(), srv.URL+"/v1", "k", body, "merc", 0)
	if s.Err != "" {
		t.Fatal(s.Err)
	}
	if !s.CachedTokensReported || s.CachedTokens != 24 {
		t.Fatalf("cached_tokens not parsed: reported=%v value=%d", s.CachedTokensReported, s.CachedTokens)
	}
}

func TestGatewayParityShapeInsightPrefersShortRequests(t *testing.T) {
	// Synthetic cells: short shape has higher relative overhead.
	shortRel, longRel := 2.0, 1.1
	shortAbs, longAbs := 5.0, 5.0
	cells := []GatewayParityCellResult{
		{
			Spec: GatewayParityCellSpec{Concurrency: 1, PromptTokens: 32, OutputTokens: 16, State: "warm"},
			Status: "MEASURED", Gate: GatewayParityGateLevel{Verdict: "PASS"},
			RelativeOverhead: &shortRel, AbsoluteOverheadMs: &shortAbs,
			Merc:   GatewayParityLevelResult{TTFTp95: &GatewayParityPointEstimate{Point: 10}},
			Direct: GatewayParityLevelResult{TTFTp95: &GatewayParityPointEstimate{Point: 5}},
		},
		{
			Spec: GatewayParityCellSpec{Concurrency: 1, PromptTokens: 8192, OutputTokens: 16, State: "warm"},
			Status: "MEASURED", Gate: GatewayParityGateLevel{Verdict: "PASS"},
			RelativeOverhead: &longRel, AbsoluteOverheadMs: &longAbs,
			Merc:   GatewayParityLevelResult{TTFTp95: &GatewayParityPointEstimate{Point: 55}},
			Direct: GatewayParityLevelResult{TTFTp95: &GatewayParityPointEstimate{Point: 50}},
		},
	}
	insight := ComputeGatewayParityShapeInsight(cells)
	if insight == nil || insight.HighestRelativeCell == "" {
		t.Fatal("expected insight")
	}
	if !strings.Contains(insight.HighestRelativeCell, "p=32") {
		t.Fatalf("highest relative should be short shape, got %s", insight.HighestRelativeCell)
	}
	if !strings.Contains(insight.LowestRelativeCell, "p=8192") {
		t.Fatalf("lowest relative should be long shape, got %s", insight.LowestRelativeCell)
	}
	if !strings.Contains(insight.Finding, "measured") {
		t.Fatalf("finding should be measured: %s", insight.Finding)
	}
}

// Ensure adding matrix dimensions does not relax the single-shape budget path.
func TestGatewayParityMatrixDoesNotRelaxSingleShapeGuards(t *testing.T) {
	// Re-check: error rate, sample floor, ladder still hard-refuse.
	if RefuseGatewayParitySampleCount(32, 32) == "" {
		t.Fatal("sample floor must still refuse n=32 at c=32")
	}
	if RefuseGatewayParityErrorRate(100, 6) == "" {
		t.Fatal("error-rate budget must still refuse 6%")
	}
	if GatewayParityLadderComplete([]int{1}) {
		t.Fatal("ladder must still require {1,8,32}")
	}
	if DefaultGatewayParityBudget().TTFTOverheadP95Ms != 15.0 {
		t.Fatal("TTFT budget must not be relaxed")
	}
}
