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
	evidenceClass := fs.String("evidence-class", "PARITY_EVIDENCE", "PARITY_EVIDENCE | HARNESS_SELF_TEST")
	modelDigest := fs.String("model-digest", "", "sha256 of model artifact (required for PARITY_EVIDENCE)")
	topologyNote := fs.String("topology-note", "", "where client / control plane / engine run")
	selfTestStandin := fs.Bool("self-test-standin", false, "run against a local stand-in; forces HARNESS_SELF_TEST")
	matrix := fs.Bool("matrix", false, "run the multi-dimensional matrix (prompt×output×state×concurrency); default defensible subset")
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
	if *evidenceClass == "PARITY_EVIDENCE" && len(*modelDigest) != 64 {
		fmt.Fprintln(os.Stderr, "PARITY_EVIDENCE requires -model-digest (64 hex chars)")
		return 2
	}

	mercKey := os.Getenv("MERC_BENCHMARK_API_KEY")
	directKey := os.Getenv("MERC_DIRECT_VLLM_API_KEY")
	if mercKey == "" || directKey == "" {
		fmt.Fprintln(os.Stderr, "MERC_BENCHMARK_API_KEY and MERC_DIRECT_VLLM_API_KEY are required")
		return 2
	}

	contract := GatewayParitySamplingContract{
		Model: *model, Prompt: *prompt, Temperature: *temperature, TopP: *topP,
		MaxTokens: *maxTokens, Stream: true, ModelDigest: *modelDigest,
	}
	if *seed >= 0 {
		s := *seed
		contract.Seed = &s
	}

	if *matrix {
		return runGatewayParityMatrixLive(
			*mercURL, mercKey, *directURL, directKey, contract,
			*evidenceClass, *topologyNote, *out,
		)
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
		"single-shape mode (legacy); pass -matrix for prompt×output×state dimensions",
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

// runGatewayParityMatrixLive drives the defensible multi-dimensional subset
// against real merc + direct endpoints.
func runGatewayParityMatrixLive(
	mercURL, mercKey, directURL, directKey string,
	contract GatewayParitySamplingContract,
	evidenceClass, topologyNote, out string,
) int {
	selection := DefaultGatewayParityMatrixSelection()
	fmt.Printf("=== matrix mode: %d cells (of 1440 full / 480 acting) ===\n", len(selection.Selected))
	fmt.Println("rationale:", selection.Rationale)
	hostStart := CaptureGatewayParityHostLoad()
	cells := RunGatewayParityMatrix(
		context.Background(), mercURL, mercKey, directURL, directKey, contract, selection,
	)
	hostEnd := CaptureGatewayParityHostLoad()
	topology := GatewayParityNetworkTopology{
		Notes:          topologyNote,
		ClientToEngine: "see merc_base_url / direct_base_url",
	}
	notes := []string{
		"Authoritative v2 matrix harness (gate v3 interval decisions per cell).",
		"scripts/gateway-parity.py is INVALIDATED.",
		fmt.Sprintf("merc_base_url=%s direct_base_url=%s", mercURL, directURL),
		selection.Rationale,
	}
	rec := BuildGatewayParityMatrixReceipt(
		contract, topology, selection, cells,
		DefaultGatewayParityBudget(), hostStart, hostEnd,
		evidenceClass, notes,
	)
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("wrote", out)
	fmt.Printf("cells=%d gate_passed=%v verdict=%s comparable=%v\n",
		len(rec.Cells), rec.GatePassed, rec.Gate.Verdict, rec.Comparable)
	if rec.ShapeInsight != nil && rec.ShapeInsight.Finding != "" {
		fmt.Println("shape_insight:", rec.ShapeInsight.Finding)
	}
	for _, cell := range rec.Cells {
		fmt.Printf("  cell %s status=%s gate=%s", cell.Spec.Key(), cell.Status, cell.Gate.Verdict)
		if cell.RelativeOverhead != nil {
			fmt.Printf(" rel=%.3f", *cell.RelativeOverhead)
		}
		if cell.Reason != "" {
			fmt.Printf(" reason=%s", cell.Reason)
		}
		fmt.Println()
	}
	if !rec.GatePassed {
		if rec.RefusalReason != "" {
			fmt.Fprintln(os.Stderr, "refusal:", rec.RefusalReason)
		}
		return 1
	}
	return 0
}

func runGatewayParityStandinSelfTest(out string) int {
	return runGatewayParityMatrixStandinSelfTest(out)
}

// runGatewayParityMatrixStandinSelfTest exercises prompt/output/state dimensions
// against two local stand-ins (merc slower by a fixed delay so shape insight is
// measurable). Always HARNESS_SELF_TEST — never comparable parity evidence.
func runGatewayParityMatrixStandinSelfTest(out string) int {
	var mercHits, directHits atomic.Int64

	// merc stand-in: fixed +5ms delay (simulates constant gateway overhead).
	// Injects cached_tokens when the body carries the shared-prefix marker so
	// prefix-hit verification can succeed without assuming a hit.
	mercLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	directLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	makeHandler := func(hits *atomic.Int64, extraDelay time.Duration) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			reqBody, _ := io.ReadAll(r.Body)
			// Artificial engine cost scales with request body size so short vs
			// long shapes are distinguishable. Fixed extraDelay on the merc arm
			// is the constant "gateway" component under test.
			engine := time.Duration(len(reqBody)/200) * time.Millisecond
			if engine < 2*time.Millisecond {
				engine = 2 * time.Millisecond
			}
			if engine > 40*time.Millisecond {
				engine = 40 * time.Millisecond
			}
			time.Sleep(engine + extraDelay)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Merc-Upstream-Body-SHA256", sha256Hex(reqBody))
			// Deliberately omit X-Merc-Contract-ID so self-test cannot satisfy
			// PARITY_EVIDENCE identity proof if re-labeled.
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			// cached_tokens only when shared-prefix marker is present.
			cached := 0
			if strings.Contains(string(reqBody), gatewayParitySharedPrefixMarker) {
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
	selection := SelfTestGatewayParityMatrixSelection()
	hostStart := CaptureGatewayParityHostLoad()
	cells := RunGatewayParityMatrix(
		context.Background(),
		mercBase, "k", directBase, "k",
		contract, selection,
	)
	hostEnd := CaptureGatewayParityHostLoad()

	topology := GatewayParityNetworkTopology{
		ClientHost: "local-cli-process", ControlPlane: "none (self-test)",
		Engine: "local dual stand-in (merc=+5ms fixed delay)", ClientToEngine: "loopback",
		Notes: "HARNESS_SELF_TEST matrix dimensions; NOT parity evidence",
	}
	rec := BuildGatewayParityMatrixReceipt(
		contract, topology, selection, cells,
		DefaultGatewayParityBudget(), hostStart, hostEnd,
		"HARNESS_SELF_TEST",
		[]string{
			"stand-in token timing is artificial; merc arm adds fixed +5ms",
			"NOT parity evidence; do not quote as gateway overhead",
			"stand-in omits X-Merc-Contract-ID; body_identity cannot satisfy PARITY_EVIDENCE",
			fmt.Sprintf("stand-in hits merc=%d direct=%d", mercHits.Load(), directHits.Load()),
			fmt.Sprintf("matrix cells=%d selection=%s", len(cells), selection.Rationale),
		},
	)
	if rec.Comparable || rec.GatePassed {
		fmt.Fprintln(os.Stderr, "bug: matrix self-test receipt is comparable or gate_passed")
		return 1
	}
	if len(rec.Cells) == 0 {
		fmt.Fprintln(os.Stderr, "bug: matrix self-test produced no cells")
		return 1
	}
	// Every state that was selected must appear.
	seenState := map[string]bool{}
	for _, c := range rec.Cells {
		seenState[c.Spec.State] = true
	}
	for _, need := range []string{"cold", "warm", "prefix-hit"} {
		if !seenState[need] {
			fmt.Fprintf(os.Stderr, "bug: matrix self-test missing state %s\n", need)
			return 1
		}
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
	fmt.Println("wrote", out, "(HARNESS_SELF_TEST matrix)")
	fmt.Printf("cells=%d gate_passed=%v comparable=%v verdict=%s\n",
		len(rec.Cells), rec.GatePassed, rec.Comparable, rec.Gate.Verdict)
	if rec.ShapeInsight != nil && rec.ShapeInsight.Finding != "" {
		fmt.Println("shape_insight:", rec.ShapeInsight.Finding)
	}
	for _, cell := range rec.Cells {
		fmt.Printf("  cell %s status=%s gate=%s", cell.Spec.Key(), cell.Status, cell.Gate.Verdict)
		if cell.Reason != "" {
			fmt.Printf(" reason=%s", cell.Reason)
		}
		if cell.RelativeOverhead != nil {
			fmt.Printf(" rel=%.3f", *cell.RelativeOverhead)
		}
		fmt.Println()
	}
	return 0
}
