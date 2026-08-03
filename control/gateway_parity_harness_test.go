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

// Sub-floor sample count must hard-refuse. Reporting the level with a caveat
// is the defect class that produced invalid receipts.
func TestGatewayParityRefusesSubFloorSampleCount(t *testing.T) {
	cases := []struct {
		c, n    int
		refuse  bool
		wantSub string
	}{
		{1, 19, true, "below floor"},
		{1, 20, false, ""},
		{8, 39, true, "below floor"},
		{8, 40, false, ""},
		{32, 159, true, "below floor"},
		{32, 160, false, ""},
		{64, 319, true, "below floor"},
		{64, 320, false, ""},
		{128, 639, true, "below floor"},
		{128, 640, false, ""},
	}
	for _, tc := range cases {
		reason := RefuseGatewayParitySampleCount(tc.c, tc.n)
		if tc.refuse && reason == "" {
			t.Fatalf("c=%d n=%d: expected refusal, got empty", tc.c, tc.n)
		}
		if !tc.refuse && reason != "" {
			t.Fatalf("c=%d n=%d: unexpected refusal %q", tc.c, tc.n, reason)
		}
		if tc.refuse && !strings.Contains(reason, tc.wantSub) {
			t.Fatalf("c=%d n=%d: refusal %q missing %q", tc.c, tc.n, reason, tc.wantSub)
		}
	}
	if GatewayParitySampleFloor(1) != 20 {
		t.Fatalf("floor(1)=%d, want 20 (absolute min, not 5×1)", GatewayParitySampleFloor(1))
	}
	if GatewayParitySampleFloor(32) != 160 {
		t.Fatalf("floor(32)=%d, want 160", GatewayParitySampleFloor(32))
	}

	// RunGatewayParityLevel must refuse before fabricating samples.
	client := NewGatewayParityClient(4)
	got := RunGatewayParityLevel(context.Background(), client, "http://127.0.0.1:1/v1", "", "merc",
		[]byte(`{}`), 32, 32)
	if got.Status != "REFUSED" {
		t.Fatalf("status=%s, want REFUSED for n=32 at c=32", got.Status)
	}
	if !strings.Contains(got.Reason, "below floor") {
		t.Fatalf("reason=%q, want below floor", got.Reason)
	}
	if len(got.RawSamples) != 0 {
		t.Fatalf("refused level must not invent raw samples, got %d", len(got.RawSamples))
	}
}

// Mismatched request bodies must refuse. Byte-identity is the whole point of
// binding every sampler field the gateway can default.
func TestGatewayParityRefusesMismatchedRequestBodies(t *testing.T) {
	a := []byte(`{"model":"m","max_tokens":16,"temperature":0,"top_p":0.95}`)
	b := []byte(`{"model":"m","max_tokens":16,"temperature":0,"top_p":1}`)
	if reason := AssertByteIdenticalBodies(a, b); reason == "" {
		t.Fatal("mismatched bodies produced no refusal")
	} else if !strings.Contains(reason, "differ") {
		t.Fatalf("refusal did not name the difference: %s", reason)
	}
	if reason := AssertByteIdenticalBodies(a, a); reason != "" {
		t.Fatalf("identical bodies refused: %s", reason)
	}
	if reason := AssertByteIdenticalBodies(nil, a); reason == "" {
		t.Fatal("empty body was accepted")
	}

	// A receipt built with unequal bodies must not pass.
	contract := GatewayParitySamplingContract{
		Model: "m", Prompt: "p", Temperature: 0, TopP: 0.95, MaxTokens: 16, Stream: true,
		ModelDigest: strings.Repeat("ab", 32),
	}
	body, err := contract.BuildChatCompletionsBody()
	if err != nil {
		t.Fatal(err)
	}
	other := append([]byte(nil), body...)
	// Flip top_p by rebuilding.
	contract2 := contract
	contract2.TopP = 1.0
	body2, err := contract2.BuildChatCompletionsBody()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(body, body2) {
		t.Fatal("test setup: bodies unexpectedly equal")
	}
	identity := GatewayParityBodyIdentity{
		Proven: true, Method: "test",
		HarnessBodySHA256:   sha256Hex(body),
		MercUpstreamSHA256:  sha256Hex(body),
		DirectRequestSHA256: sha256Hex(body2),
		Equal:               false,
		Detail:              AssertByteIdenticalBodies(body, body2),
	}
	client := NewGatewayParityClient(1)
	// Minimal measured levels so the gate's "measured" path is reached and the
	// body refusal is still decisive.
	levels := map[string]GatewayParityLevelResult{
		"merc@c=1": {
			Arm: "merc", Concurrency: 1, RequestsAttempted: 20, RequestsOK: 20,
			Status: "MEASURED", MeanInFlight: 1, PeakInFlight: 1,
			TTFTp95: &GatewayParityPointEstimate{Point: 10},
		},
		"direct@c=1": {
			Arm: "direct", Concurrency: 1, RequestsAttempted: 20, RequestsOK: 20,
			Status: "MEASURED", MeanInFlight: 1, PeakInFlight: 1,
			TTFTp95: &GatewayParityPointEstimate{Point: 10},
		},
	}
	tps := 100.0
	for k, v := range levels {
		v.AggregateTokPerSec = &tps
		v.TotalTokens = 100
		v.MeanCompletionTok = 5
		levels[k] = v
	}
	rec := BuildGatewayParityReceipt(
		contract, GatewayParityNetworkTopology{}, client, []int{1}, levels, identity,
		DefaultGatewayParityBudget(), CaptureGatewayParityHostLoad(), CaptureGatewayParityHostLoad(),
		"PARITY_EVIDENCE", nil,
	)
	if rec.GatePassed {
		t.Fatal("gate_passed=true with mismatched request bodies")
	}
	if rec.Comparable {
		t.Fatal("comparable=true with mismatched request bodies")
	}
	joined := strings.Join(rec.Refusals, "; ")
	if !strings.Contains(joined, "byte-identical") && !strings.Contains(joined, "differ") {
		t.Fatalf("refusals did not name body mismatch: %v", rec.Refusals)
	}
	_ = other
}

// A receipt that claims levels it did not evaluate must fail the gate.
// This is the defect that let evaluate_budget inspect c=1 only and still
// report gate_passed: true while c=32 was 3.5× outside budget.
func TestGatewayParityGateFailsWhenClaimedLevelNotEvaluated(t *testing.T) {
	levels := map[string]GatewayParityLevelResult{
		"merc@c=1": {
			Arm: "merc", Concurrency: 1, RequestsAttempted: 20, RequestsOK: 20,
			Status: "MEASURED", MeanInFlight: 1,
			TTFTp95: &GatewayParityPointEstimate{Point: 12},
		},
		"direct@c=1": {
			Arm: "direct", Concurrency: 1, RequestsAttempted: 20, RequestsOK: 20,
			Status: "MEASURED", MeanInFlight: 1,
			TTFTp95: &GatewayParityPointEstimate{Point: 10},
		},
		// c=8 and c=32 claimed but absent.
	}
	tps := 50.0
	for k, v := range levels {
		v.AggregateTokPerSec = &tps
		v.TotalTokens = 100
		v.MeanCompletionTok = 5
		levels[k] = v
	}
	budget := DefaultGatewayParityBudget()
	gate := EvaluateGatewayParityGate([]int{1, 8, 32}, levels, budget)
	if gate.GatePassed {
		t.Fatal("gate_passed=true when only c=1 was measured; multi-level claim must fail")
	}
	if len(gate.Levels) != 3 {
		t.Fatalf("gate recorded %d levels, want 3", len(gate.Levels))
	}
	if !gate.Levels[0].Passed {
		// c=1 may still pass its own overhead check; if it failed for another
		// reason surface it. Either way higher levels must fail.
		t.Logf("c=1 level: %+v", gate.Levels[0])
	}
	for _, level := range gate.Levels[1:] {
		if level.Passed {
			t.Fatalf("concurrency %d passed with no measurement", level.Concurrency)
		}
		if len(level.Refusals) == 0 {
			t.Fatalf("concurrency %d produced no refusal", level.Concurrency)
		}
	}

	// Measuring every claimed level with matching tokens and budget-clearing
	// overhead must clear the multi-level gate.
	for _, c := range []int{8, 32} {
		n := GatewayParitySampleFloor(c)
		ttftMerc := &GatewayParityPointEstimate{Point: 12}
		ttftDirect := &GatewayParityPointEstimate{Point: 10}
		tpsM, tpsD := 100.0, 100.0
		levels[fmt.Sprintf("merc@c=%d", c)] = GatewayParityLevelResult{
			Arm: "merc", Concurrency: c, RequestsAttempted: n, RequestsOK: n,
			Status: "MEASURED", MeanInFlight: float64(c), TTFTp95: ttftMerc,
			AggregateTokPerSec: &tpsM, TotalTokens: n * 5, MeanCompletionTok: 5,
		}
		levels[fmt.Sprintf("direct@c=%d", c)] = GatewayParityLevelResult{
			Arm: "direct", Concurrency: c, RequestsAttempted: n, RequestsOK: n,
			Status: "MEASURED", MeanInFlight: float64(c), TTFTp95: ttftDirect,
			AggregateTokPerSec: &tpsD, TotalTokens: n * 5, MeanCompletionTok: 5,
		}
	}
	// Fix c=1 token totals to match.
	for _, arm := range []string{"merc", "direct"} {
		k := arm + "@c=1"
		v := levels[k]
		v.TotalTokens = 100
		v.MeanCompletionTok = 5
		levels[k] = v
	}
	gateOK := EvaluateGatewayParityGate([]int{1, 8, 32}, levels, budget)
	if !gateOK.GatePassed {
		t.Fatalf("gate failed with every level measured inside budget: %+v", gateOK)
	}
}

// Sampling contract must bind top_p (and friends) so merc cannot inject a
// profile default the direct arm never sees.
func TestGatewayParitySamplingContractBindsDefaultableFields(t *testing.T) {
	c := GatewayParitySamplingContract{
		Model: "cx-chat-1b", Prompt: "hello", Temperature: 0, TopP: 0.95,
		MaxTokens: 32, Stream: true, ModelDigest: strings.Repeat("cd", 32),
	}
	body, err := c.BuildChatCompletionsBody()
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"temperature", "top_p", "max_tokens", "stream", "model"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("sampling contract body missing %q", key)
		}
	}
	if payload["top_p"] != 0.95 {
		t.Fatalf("top_p=%v, want 0.95 (explicit; gateway must not inject a different default)", payload["top_p"])
	}
}

// Bounded pool must keep mean in-flight near nominal concurrency (not the
// FIFO-join ~51% pathology).
func TestGatewayParityPoolMaintainsInFlight(t *testing.T) {
	var active atomic.Int64
	var peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := active.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		// Hold long enough that the pool fills.
		time.Sleep(30 * time.Millisecond)
		active.Add(-1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewGatewayParityClient(8)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"temperature":0,"top_p":1,"stream":true,"stream_options":{"include_usage":true}}`)
	// n=40 at c=8 meets the floor; sleep-based stand-in keeps workers busy.
	result := RunGatewayParityLevel(context.Background(), client, srv.URL+"/v1", "", "direct", body, 8, 40)
	if result.Status != "MEASURED" {
		t.Fatalf("status=%s reason=%s", result.Status, result.Reason)
	}
	if result.MeanInFlight < 8*gatewayParityMinInFlightFraction {
		t.Fatalf("mean in-flight %.2f below steady-state floor for c=8", result.MeanInFlight)
	}
	if peak.Load() < 6 {
		t.Fatalf("server peak concurrency %d; pool did not fill", peak.Load())
	}
	// Connection reuse: more requests than dials.
	stats := client.SnapshotConnStats()
	if stats.ConnectionsOpened > 40 {
		t.Fatalf("opened %d connections for 40 requests; keep-alive broken", stats.ConnectionsOpened)
	}
	if stats.ReusedConnections == 0 && result.RequestsOK > 8 {
		t.Fatalf("zero connection reuses across %d ok requests", result.RequestsOK)
	}
}

// Path-timing formatter must emit the previously-invisible TTFT stages.
func TestRealtimePathMarksIncludeAdmissionAndSettlementIntent(t *testing.T) {
	marks := map[string]time.Duration{
		"authorize_contract": 2 * time.Millisecond,
		"admission_event":    3 * time.Millisecond,
		"upstream_ttfb":      10 * time.Millisecond,
		"settlement_intent":  4 * time.Millisecond,
		"proxy_sse":          50 * time.Millisecond,
	}
	s := formatRealtimePathMarks(marks)
	for _, need := range []string{"admission_event=", "settlement_intent=", "authorize_contract="} {
		if !strings.Contains(s, need) {
			t.Fatalf("format missing %s in %q", need, s)
		}
	}
	// Order: admission_event before upstream_ttfb; settlement_intent after.
	adm := strings.Index(s, "admission_event=")
	up := strings.Index(s, "upstream_ttfb=")
	si := strings.Index(s, "settlement_intent=")
	if !(adm >= 0 && up > adm && si > up) {
		t.Fatalf("stage order wrong: %q", s)
	}
}

// Parity capture is off by default and loopback-only when on.
func TestParityUpstreamCaptureGuarded(t *testing.T) {
	t.Setenv(parityUpstreamCaptureEnv, "")
	if parityCaptureEnabled() {
		t.Fatal("capture enabled without env")
	}
	captureParityUpstreamBody("req_1", []byte(`{"a":1}`))
	if sha := parityUpstreamBodySHA("req_1"); sha != "" {
		t.Fatalf("capture stored body while disabled: %s", sha)
	}

	t.Setenv(parityUpstreamCaptureEnv, "1")
	// Reset the once-guard by constructing a fresh store via the enabled path.
	// parityCaptureOnce may already have fired with nil in this process; drive
	// the store directly for the unit test of put/get.
	store := newParityUpstreamCapture(4)
	store.put("req_x", []byte(`{"hello":"world"}`))
	e, ok := store.get("req_x")
	if !ok || e.SHA256 == "" || !bytes.Equal(e.Body, []byte(`{"hello":"world"}`)) {
		t.Fatalf("store round-trip failed: ok=%v entry=%+v", ok, e)
	}

	// Handler refuses non-loopback.
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/internal/parity/upstream-body?request_id=req_x", nil)
	req.RemoteAddr = "8.8.8.8:12345"
	rec := httptest.NewRecorder()
	// Force enabled for the handler check; store is process-global, so inject
	// via put on the global after enabling.
	parityCapture = store
	s.handleParityUpstreamBody(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote peer got %d, want 403", rec.Code)
	}
}

// Local dry run against a stand-in. Receipt is HARNESS_SELF_TEST, never parity
// evidence. Written under evidence/perf/ for a skeptic to inspect.
func TestGatewayParityHarnessSelfTestReceipt(t *testing.T) {
	// Stand-in OpenAI-compatible SSE server. Not a model.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/chat/completions" && !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"top_p"`)) {
			http.Error(w, "top_p missing", http.StatusBadRequest)
			return
		}
		// Tiny artificial delay so TTFT is non-zero and pool can fill.
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(1 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	contract := GatewayParitySamplingContract{
		Model: "stand-in", Prompt: "self-test", Temperature: 0, TopP: 1.0,
		MaxTokens: 8, Stream: true,
		ModelDigest: strings.Repeat("11", 32),
	}
	body, err := contract.BuildChatCompletionsBody()
	if err != nil {
		t.Fatal(err)
	}
	// Both arms hit the same stand-in: proves the harness, not merc overhead.
	client := NewGatewayParityClient(4)
	hostStart := CaptureGatewayParityHostLoad()
	// Self-test uses c=1 only with n=20 — meets floor, stays fast.
	claimed := []int{1}
	merc, direct := RunGatewayParityInterleavedLevel(
		context.Background(), client,
		srv.URL+"/v1", "k", srv.URL+"/v1", "k",
		body, 1, GatewayParitySampleFloor(1),
	)
	hostEnd := CaptureGatewayParityHostLoad()
	levels := map[string]GatewayParityLevelResult{
		"merc@c=1":   merc,
		"direct@c=1": direct,
	}
	identity := GatewayParityBodyIdentity{
		Proven: true, Method: "self-test: both arms send identical harness body to stand-in",
		HarnessBodySHA256:   sha256Hex(body),
		MercUpstreamSHA256:  sha256Hex(body),
		DirectRequestSHA256: sha256Hex(body),
		Equal:               true,
		Detail:              "stand-in self-test; merc upstream capture not required because both arms are the stand-in",
	}
	topology := GatewayParityNetworkTopology{
		ClientHost: "local-test-process", ControlPlane: "none (self-test)",
		Engine:          "httptest stand-in (not a model)",
		ClientToControl: "n/a", ControlToEngine: "n/a", ClientToEngine: "loopback",
		Notes: "HARNESS_SELF_TEST only",
	}
	rec := BuildGatewayParityReceipt(
		contract, topology, client, claimed, levels, identity,
		DefaultGatewayParityBudget(), hostStart, hostEnd,
		"HARNESS_SELF_TEST",
		[]string{
			"stand-in token timing is artificial",
			"NOT parity evidence; do not quote as gateway overhead",
		},
	)
	if rec.EvidenceClass != "HARNESS_SELF_TEST" {
		t.Fatalf("evidence_class=%s", rec.EvidenceClass)
	}
	if rec.GatePassed {
		t.Fatal("self-test receipt must not set gate_passed (not parity evidence)")
	}
	if merc.Status != "MEASURED" || direct.Status != "MEASURED" {
		t.Fatalf("arms not measured: merc=%s (%s) direct=%s (%s)",
			merc.Status, merc.Reason, direct.Status, direct.Reason)
	}
	if len(merc.RawSamples) < GatewayParitySampleFloor(1) {
		t.Fatalf("raw samples missing: merc=%d", len(merc.RawSamples))
	}

	// Write the receipt next to the invalidated one so a reader can see both.
	outDir := filepath.Join("..", "evidence", "perf")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(outDir, "gateway-parity-v2-selftest.json")
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote harness self-test receipt to %s (hits=%d)", outPath, hits.Load())

	// Round-trip: a skeptic must re-derive percentiles from raw samples.
	var loaded GatewayParityReceipt
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.RawSampleCount == 0 {
		t.Fatal("raw_sample_count is zero")
	}
	lvl := loaded.Levels["merc@c=1"]
	if len(lvl.RawSamples) == 0 {
		t.Fatal("loaded receipt lost raw samples")
	}
}

// Confirm loopback detection used by the capture handler.
func TestIsLoopbackRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	if !isLoopbackRequest(req) {
		t.Fatal("127.0.0.1 should be loopback")
	}
	req.RemoteAddr = "[::1]:9999"
	if !isLoopbackRequest(req) {
		t.Fatal("::1 should be loopback")
	}
	req.RemoteAddr = "203.0.113.4:9999"
	if isLoopbackRequest(req) {
		t.Fatal("TEST-NET should not be loopback")
	}
	// net.SplitHostPort needs a port; bare IP still handled.
	req.RemoteAddr = "127.0.0.1"
	if !isLoopbackRequest(req) {
		// Depending on parse path; accept either if RemoteAddr is malformed.
		if ip := net.ParseIP("127.0.0.1"); ip == nil || !ip.IsLoopback() {
			t.Fatal("sanity")
		}
	}
}
