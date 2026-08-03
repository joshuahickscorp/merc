package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// runGatewayParityCLI is the single live driver. scripts/gateway-parity-v2.go
// re-execs `go run . gateway-parity …` so the CLI and the tested path share
// EvaluateGatewayParityGate / BuildGatewayParityReceipt — no forked gate.
func runGatewayParityCLI(args []string) int {
	fs := flag.NewFlagSet("gateway-parity", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	mercURL := fs.String("merc-base-url", "", "merc OpenAI-compatible base (…/v1)")
	directURL := fs.String("direct-base-url", "", "engine base for the direct arm (…/v1)")
	model := fs.String("model", "cx-chat-1b", "model id both arms must accept")
	prompt := fs.String("prompt", "Write a short factual paragraph about the water cycle. Be specific and do not repeat yourself.", "")
	maxTokens := fs.Int("max-tokens", 32, "max_tokens (bound explicitly on both arms)")
	temperature := fs.Float64("temperature", 0, "temperature (bound explicitly)")
	topP := fs.Float64("top-p", 0.95, "top_p (bound explicitly; do not leave unset)")
	seed := fs.Int64("seed", -1, "seed; <0 means omit")
	levelsFlag := fs.String("concurrency", "1,8,32", "comma-separated concurrency levels")
	out := fs.String("out", "evidence/perf/gateway-parity-v2.json", "receipt path")
	evidenceClass := fs.String("evidence-class", "PARITY_EVIDENCE",
		"PARITY_EVIDENCE | LOCAL_METAL_PARITY | HARNESS_SELF_TEST")
	modelDigest := fs.String("model-digest", "", "sha256 of model artifact (required for PARITY_EVIDENCE and LOCAL_METAL_PARITY)")
	topologyNote := fs.String("topology-note", "", "where client / control plane / engine run")
	selfTestStandin := fs.Bool("self-test-standin", false, "run against a local stand-in; forces HARNESS_SELF_TEST")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *selfTestStandin {
		*evidenceClass = "HARNESS_SELF_TEST"
		return runGatewayParityStandinSelfTest(*out)
	}
	if *mercURL == "" || *directURL == "" {
		fmt.Fprintln(os.Stderr, "-merc-base-url and -direct-base-url are required (or -self-test-standin)")
		return 2
	}
	switch *evidenceClass {
	case "PARITY_EVIDENCE", "LOCAL_METAL_PARITY", "HARNESS_SELF_TEST":
	default:
		fmt.Fprintf(os.Stderr, "unknown -evidence-class %q (want PARITY_EVIDENCE | LOCAL_METAL_PARITY | HARNESS_SELF_TEST)\n", *evidenceClass)
		return 2
	}
	if (*evidenceClass == "PARITY_EVIDENCE" || *evidenceClass == "LOCAL_METAL_PARITY") && len(*modelDigest) != 64 {
		fmt.Fprintf(os.Stderr, "%s requires -model-digest (64 hex chars)\n", *evidenceClass)
		return 2
	}

	mercKey := os.Getenv("MERC_BENCHMARK_API_KEY")
	directKey := os.Getenv("MERC_DIRECT_VLLM_API_KEY")
	if mercKey == "" || directKey == "" {
		fmt.Fprintln(os.Stderr, "MERC_BENCHMARK_API_KEY and MERC_DIRECT_VLLM_API_KEY are required")
		return 2
	}

	var claimed []int
	for _, p := range strings.Split(*levelsFlag, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var c int
		if _, err := fmt.Sscanf(p, "%d", &c); err != nil || c < 1 {
			fmt.Fprintf(os.Stderr, "bad concurrency %q\n", p)
			return 2
		}
		claimed = append(claimed, c)
	}
	if len(claimed) == 0 {
		fmt.Fprintln(os.Stderr, "no concurrency levels")
		return 2
	}
	sort.Ints(claimed)

	contract := GatewayParitySamplingContract{
		Model: *model, Prompt: *prompt, Temperature: *temperature, TopP: *topP,
		MaxTokens: *maxTokens, Stream: true, ModelDigest: *modelDigest,
	}
	if *seed >= 0 {
		s := *seed
		contract.Seed = &s
	}
	body, err := contract.BuildChatCompletionsBody()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	maxC := claimed[len(claimed)-1]
	client := NewGatewayParityClient(maxC)
	hostStart := CaptureGatewayParityHostLoad()
	levels := map[string]GatewayParityLevelResult{}
	ctx := context.Background()
	for _, c := range claimed {
		// Attempt above the floor. The gate still requires RequestsOK >= floor,
		// so this buys an error budget rather than lowering the bar: sampling
		// exactly at the floor means a single transport hiccup drops the level
		// under it and REFUSES an otherwise good run.
		floor := GatewayParitySampleFloor(c)
		n := floor + floor/10 + 2
		fmt.Printf("=== alternating-batch interleaved c=%d n=%d (floor %d) per arm ===\n", c, n, floor)
		merc, direct := RunGatewayParityInterleavedLevel(
			ctx, client, *mercURL, mercKey, *directURL, directKey, body, c, n,
		)
		levels[fmt.Sprintf("merc@c=%d", c)] = merc
		levels[fmt.Sprintf("direct@c=%d", c)] = direct
		for _, side := range []GatewayParityLevelResult{merc, direct} {
			fmt.Printf("  %s: status=%s ok=%d/%d mean_in_flight=%.2f peak=%d wall=%.3fs",
				side.Arm, side.Status, side.RequestsOK, side.RequestsAttempted,
				side.MeanInFlight, side.PeakInFlight, side.WallSeconds)
			if side.TTFTp95 != nil {
				fmt.Printf(" ttft_p95=%.2f", side.TTFTp95.Point)
			}
			if side.Reason != "" {
				fmt.Printf(" reason=%s", side.Reason)
			}
			fmt.Println()
		}
	}
	hostEnd := CaptureGatewayParityHostLoad()

	// Identity starts unproven; Equal is false until evidence raises it.
	identity := ProveGatewayParityBodyIdentity(body, claimed, levels)

	topology := GatewayParityNetworkTopology{
		Notes:          *topologyNote,
		ClientToEngine: "see merc_base_url / direct_base_url",
	}
	// Stash URLs in notes for the receipt reader (topology fields are free-form).
	notes := []string{
		"Authoritative v2 harness (gate v3 interval decisions). scripts/gateway-parity.py is INVALIDATED.",
		fmt.Sprintf("merc_base_url=%s direct_base_url=%s", *mercURL, *directURL),
	}
	rec := BuildGatewayParityReceipt(
		contract, topology, client, claimed, levels, identity,
		DefaultGatewayParityBudget(), hostStart, hostEnd,
		*evidenceClass, notes,
	)

	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(*out, append(raw, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("wrote", *out)
	fmt.Printf("gate_passed=%v verdict=%s comparable=%v\n", rec.GatePassed, rec.Gate.Verdict, rec.Comparable)
	if !rec.GatePassed {
		if rec.RefusalReason != "" {
			fmt.Fprintln(os.Stderr, "refusal:", rec.RefusalReason)
		}
		return 1
	}
	return 0
}

func runGatewayParityStandinSelfTest(out string) int {
	var hits atomic.Int64
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Stand-in may echo X-Merc-Upstream-Body-SHA256 (exercises capture parsing)
	// but deliberately omits X-Merc-Contract-ID. A bare-SHA stand-in must not
	// be able to satisfy PARITY_EVIDENCE identity proof.
	var bodySHA string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		reqBody, _ := io.ReadAll(r.Body)
		if bodySHA == "" {
			bodySHA = sha256Hex(reqBody)
		}
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Merc-Upstream-Body-SHA256", sha256Hex(reqBody))
		// No X-Merc-Contract-ID: self-test must remain non-comparable and must
		// not satisfy Merc-bound identity proof if re-labeled as PARITY_EVIDENCE.
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})}
	go srv.Serve(l)
	defer srv.Close()

	base := "http://" + l.Addr().String() + "/v1"
	contract := GatewayParitySamplingContract{
		Model: "stand-in", Prompt: "self-test", Temperature: 0, TopP: 1.0,
		MaxTokens: 8, Stream: true, ModelDigest: strings.Repeat("11", 32),
	}
	body, err := contract.BuildChatCompletionsBody()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Exercise the concurrency machinery, not just c=1. A self-test that only
	// runs one request at a time never touches the alternating-batch pool, the
	// in-flight cap, or the peak-in-flight refusal — which is exactly where the
	// dual-load defect lived.
	claimed := []int{1, 8, 32}
	client := NewGatewayParityClient(claimed[len(claimed)-1])
	hostStart := CaptureGatewayParityHostLoad()
	levels := map[string]GatewayParityLevelResult{}
	for _, c := range claimed {
		floor := GatewayParitySampleFloor(c)
		merc, direct := RunGatewayParityInterleavedLevel(
			context.Background(), client, base, "k", base, "k", body, c, floor+floor/10+2,
		)
		levels[fmt.Sprintf("merc@c=%d", c)] = merc
		levels[fmt.Sprintf("direct@c=%d", c)] = direct
	}
	hostEnd := CaptureGatewayParityHostLoad()
	// Self-test: identity must stay unproven (bare-SHA, no Contract-ID).
	identity := ProveGatewayParityBodyIdentity(body, claimed, levels)
	if identity.Proven {
		fmt.Fprintln(os.Stderr, "bug: self-test stand-in satisfied Merc identity proof")
		return 1
	}
	topology := GatewayParityNetworkTopology{
		ClientHost: "local-cli-process", ControlPlane: "none (self-test)",
		Engine: "local stand-in", ClientToEngine: "loopback",
		Notes: "HARNESS_SELF_TEST only",
	}
	rec := BuildGatewayParityReceipt(
		contract, topology, client, claimed, levels, identity,
		DefaultGatewayParityBudget(), hostStart, hostEnd,
		"HARNESS_SELF_TEST",
		[]string{
			"stand-in token timing is artificial",
			"NOT parity evidence; do not quote as gateway overhead",
			"stand-in omits X-Merc-Contract-ID; body_identity cannot satisfy PARITY_EVIDENCE",
			fmt.Sprintf("stand-in hits=%d", hits.Load()),
		},
	)
	if rec.Comparable || rec.GatePassed {
		fmt.Fprintln(os.Stderr, "bug: self-test receipt is comparable or gate_passed")
		return 1
	}

	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("wrote", out, "(HARNESS_SELF_TEST)")
	return 0
}
