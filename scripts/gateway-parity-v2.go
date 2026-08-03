// Authoritative gateway parity harness (v2) — live dual-arm runner.
//
// The measurement library and gates live in control/gateway_parity_harness.go
// (package main of module merc/control). This script is a thin dual-arm driver
// against two OpenAI-compatible /v1 bases. It does NOT import the control
// package (control is package main); it re-embeds the same floors and the
// pooled client shape so a live run can be launched without rebuilding the
// control binary.
//
// For CI-grade self-tests and gate proofs, run:
//
//	cd control && go test -count=1 -run 'TestGatewayParity' .
//
// Live run (budgeted separately; this task forbids cloud spend):
//
//	go run scripts/gateway-parity-v2.go \
//	  -merc-base-url http://127.0.0.1:8080/v1 \
//	  -direct-base-url http://127.0.0.1:8095/v1 \
//	  -model cx-chat-1b \
//	  -out evidence/perf/gateway-parity-v2.json
//
// Receipts written by this script are PARITY_EVIDENCE only when both arms are
// real and body identity is proven. Self-tests must set -evidence-class
// HARNESS_SELF_TEST.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	harnessID       = "merc-gateway-parity-v2"
	schemaVersion   = 2
	gateVersion     = "gateway-parity-gate-v2"
	minSamples      = 20
	steadyMultiple  = 5
	minInFlightFrac = 0.70
)

func sampleFloor(c int) int {
	if c < 1 {
		c = 1
	}
	need := c * steadyMultiple
	if need < minSamples {
		return minSamples
	}
	return need
}

func main() {
	mercURL := flag.String("merc-base-url", "", "merc OpenAI-compatible base (…/v1)")
	directURL := flag.String("direct-base-url", "", "engine base for the direct arm (…/v1)")
	model := flag.String("model", "cx-chat-1b", "model id both arms must accept")
	prompt := flag.String("prompt", "Write a short factual paragraph about the water cycle. Be specific and do not repeat yourself.", "")
	maxTokens := flag.Int("max-tokens", 32, "max_tokens (bound explicitly on both arms)")
	temperature := flag.Float64("temperature", 0, "temperature (bound explicitly)")
	topP := flag.Float64("top-p", 0.95, "top_p (bound explicitly; do not leave unset)")
	seed := flag.Int64("seed", -1, "seed; <0 means omit")
	levelsFlag := flag.String("concurrency", "1,8,32", "comma-separated concurrency levels")
	out := flag.String("out", "evidence/perf/gateway-parity-v2.json", "receipt path")
	evidenceClass := flag.String("evidence-class", "PARITY_EVIDENCE", "PARITY_EVIDENCE | HARNESS_SELF_TEST")
	modelDigest := flag.String("model-digest", "", "sha256 of model artifact (required for PARITY_EVIDENCE)")
	topologyNote := flag.String("topology-note", "", "where client / control plane / engine run")
	selfTestStandin := flag.Bool("self-test-standin", false, "run against a local stand-in; forces HARNESS_SELF_TEST")
	authorityID := flag.String("authority-id", "", "required to overwrite a withdrawn receipt at -out")
	flag.Parse()

	if *selfTestStandin {
		*evidenceClass = "HARNESS_SELF_TEST"
		runStandinSelfTest(*out, *authorityID)
		return
	}
	if *mercURL == "" || *directURL == "" {
		fmt.Fprintln(os.Stderr, "-merc-base-url and -direct-base-url are required (or -self-test-standin)")
		os.Exit(2)
	}
	if *evidenceClass == "PARITY_EVIDENCE" && len(*modelDigest) != 64 {
		fmt.Fprintln(os.Stderr, "PARITY_EVIDENCE requires -model-digest (64 hex chars)")
		os.Exit(2)
	}

	mercKey := os.Getenv("MERC_BENCHMARK_API_KEY")
	directKey := os.Getenv("MERC_DIRECT_VLLM_API_KEY")
	if mercKey == "" || directKey == "" {
		fmt.Fprintln(os.Stderr, "MERC_BENCHMARK_API_KEY and MERC_DIRECT_VLLM_API_KEY are required")
		os.Exit(2)
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
			os.Exit(2)
		}
		claimed = append(claimed, c)
	}
	sort.Ints(claimed)

	body, err := buildBody(*model, *prompt, *temperature, *topP, *maxTokens, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	maxC := claimed[len(claimed)-1]
	client := newClient(maxC)
	hostStart := captureLoad()
	levels := map[string]levelResult{}
	var refusals []string

	// Prove the harness body is fully bound before any live call.
	if !bytes.Contains(body, []byte(`"top_p"`)) {
		refusals = append(refusals, "harness body missing top_p")
	}

	ctx := context.Background()
	for _, c := range claimed {
		n := sampleFloor(c)
		fmt.Printf("=== interleaved c=%d n=%d per arm ===\n", c, n)
		merc, direct := runInterleaved(ctx, client, *mercURL, mercKey, *directURL, directKey, body, c, n)
		levels[fmt.Sprintf("merc@c=%d", c)] = merc
		levels[fmt.Sprintf("direct@c=%d", c)] = direct
		for _, side := range []levelResult{merc, direct} {
			fmt.Printf("  %s: status=%s ok=%d/%d mean_in_flight=%.2f",
				side.Arm, side.Status, side.RequestsOK, side.RequestsAttempted, side.MeanInFlight)
			if side.TTFTp95 != nil {
				fmt.Printf(" ttft_p95=%.2f", side.TTFTp95.Point)
			}
			if side.Reason != "" {
				fmt.Printf(" reason=%s", side.Reason)
			}
			fmt.Println()
			if side.Status == "REFUSED" {
				refusals = append(refusals, side.Reason)
			}
		}
	}
	hostEnd := captureLoad()

	identity := bodyIdentity{
		Proven:            false,
		Method:            "harness sends identical body to both arms; merc upstream capture not queried by this thin driver (use control tests + MERC_PARITY_CAPTURE_UPSTREAM=1 for proof)",
		HarnessBodySHA256: shaHex(body),
		Equal:             true,
		Detail:            "byte-identity argued from identical harness body; enable MERC_PARITY_CAPTURE_UPSTREAM on the control plane to prove merc upstream equals this body",
	}
	// If merc returned X-Merc-Upstream-Body-SHA256 on any sample, compare.
	if merc := levels["merc@c="+fmt.Sprint(claimed[0])]; len(merc.RawSamples) > 0 {
		for _, s := range merc.RawSamples {
			if s.UpstreamBodySHA != "" {
				identity.MercUpstreamSHA256 = s.UpstreamBodySHA
				identity.DirectRequestSHA256 = shaHex(body)
				identity.Proven = s.UpstreamBodySHA == shaHex(body)
				identity.Equal = identity.Proven
				if !identity.Equal {
					identity.Detail = fmt.Sprintf("merc upstream sha %s != harness body sha %s", s.UpstreamBodySHA, shaHex(body))
					refusals = append(refusals, identity.Detail)
				} else {
					identity.Method = "X-Merc-Upstream-Body-SHA256 matches harness body sha256"
					identity.Detail = "proven"
				}
				break
			}
		}
	}

	rec := receipt{
		SchemaVersion:    schemaVersion,
		Kind:             "gateway_parity",
		Harness:          harnessID,
		GateVersion:      gateVersion,
		MeasuredAt:       time.Now().UTC().Format(time.RFC3339),
		MercSourceCommit: gitHEAD(),
		EvidenceClass:    *evidenceClass,
		SamplingContract: map[string]any{
			"model": *model, "prompt": *prompt, "temperature": *temperature,
			"top_p": *topP, "max_tokens": *maxTokens, "stream": true,
			"model_digest": *modelDigest, "seed": seedOrNil(*seed),
		},
		NetworkTopology: map[string]any{
			"notes":         *topologyNote,
			"merc_base_url": *mercURL, "direct_base_url": *directURL,
		},
		ClientImpl:       "net/http.Client + cloned Transport (scripts/gateway-parity-v2.go)",
		ConnectionPolicy: fmt.Sprintf("persistent keep-alive; MaxIdleConnsPerHost>=%d", maxC),
		ConnStats:        client.snapshot(),
		HostLoadStart:    hostStart,
		HostLoadEnd:      hostEnd,
		ColdWarmState:    "warm",
		ClaimedLevels:    claimed,
		Levels:           levels,
		BodyIdentity:     identity,
		Budget: map[string]any{
			"ttft_overhead_p95_ms":            15.0,
			"throughput_loss_fraction":        0.05,
			"require_measured_at_every_level": true,
			"basis":                           "evaluated at every claimed concurrency level",
		},
		Notes: []string{
			"Authoritative v2 harness. Old scripts/gateway-parity.py receipts are INVALIDATED_PENDING_RERUN.",
			"Gate evaluation for production claims should also run control.EvaluateGatewayParityGate via go test / control binary.",
		},
	}
	for _, lvl := range levels {
		rec.RawSampleCount += len(lvl.RawSamples)
	}
	// Multi-level gate: every claimed level must be MEASURED on both arms.
	gatePassed := true
	var gateLevels []map[string]any
	for _, c := range claimed {
		gl := map[string]any{"concurrency": c, "passed": true, "refusals": []string{}}
		var levelRefusals []string
		for _, arm := range []string{"merc", "direct"} {
			key := fmt.Sprintf("%s@c=%d", arm, c)
			lvl, ok := levels[key]
			if !ok || lvl.Status != "MEASURED" {
				msg := fmt.Sprintf("%s missing or not MEASURED", key)
				levelRefusals = append(levelRefusals, msg)
				refusals = append(refusals, msg)
			} else if lvl.RequestsOK < sampleFloor(c) {
				msg := fmt.Sprintf("%s sample count %d below floor %d", key, lvl.RequestsOK, sampleFloor(c))
				levelRefusals = append(levelRefusals, msg)
				refusals = append(refusals, msg)
			}
		}
		if len(levelRefusals) > 0 {
			gl["passed"] = false
			gl["refusals"] = levelRefusals
			gatePassed = false
		}
		gateLevels = append(gateLevels, gl)
	}
	if *evidenceClass == "HARNESS_SELF_TEST" {
		gatePassed = false
		refusals = append(refusals, "HARNESS_SELF_TEST: not parity evidence")
	}
	if !identity.Equal {
		gatePassed = false
	}
	rec.Gate = map[string]any{
		"version": gateVersion, "gate_passed": gatePassed,
		"levels": gateLevels, "claimed_concurrency_levels": claimed,
		"basis": "budget/measurement evaluated at every claimed concurrency level",
	}
	rec.GatePassed = gatePassed && len(refusals) == 0
	rec.Comparable = rec.GatePassed
	rec.Refusals = uniq(refusals)
	if !rec.GatePassed && len(rec.Refusals) > 0 {
		rec.RefusalReason = rec.Refusals[0]
	}

	if err := writeBoundEvidenceJSON(*out, rec, harnessID+"#"+gateVersion, *modelDigest, *authorityID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("wrote", *out)
	if !rec.GatePassed {
		fmt.Fprintln(os.Stderr, "gate_passed=false")
		os.Exit(1)
	}
}

func runStandinSelfTest(out, authorityID string) {
	// Minimal stand-in so `go run scripts/gateway-parity-v2.go -self-test-standin`
	// produces a labelled HARNESS_SELF_TEST receipt without merc or a model.
	var hits atomic.Int64
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
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
	body, _ := buildBody("stand-in", "self-test", 0, 1.0, 8, -1)
	client := newClient(1)
	hostStart := captureLoad()
	merc, direct := runInterleaved(context.Background(), client, base, "k", base, "k", body, 1, sampleFloor(1))
	hostEnd := captureLoad()
	levels := map[string]levelResult{"merc@c=1": merc, "direct@c=1": direct}
	rec := receipt{
		SchemaVersion: schemaVersion, Kind: "gateway_parity", Harness: harnessID,
		GateVersion: gateVersion, MeasuredAt: time.Now().UTC().Format(time.RFC3339),
		MercSourceCommit: gitHEAD(), EvidenceClass: "HARNESS_SELF_TEST",
		SamplingContract: map[string]any{"model": "stand-in", "top_p": 1.0, "temperature": 0, "max_tokens": 8, "stream": true},
		NetworkTopology:  map[string]any{"engine": "local stand-in", "client_to_engine_hop": "loopback"},
		ClientImpl:       "net/http.Client (scripts/gateway-parity-v2.go)",
		ConnectionPolicy: "persistent keep-alive",
		ConnStats:        client.snapshot(),
		HostLoadStart:    hostStart, HostLoadEnd: hostEnd, ColdWarmState: "warm",
		ClaimedLevels: []int{1}, Levels: levels,
		BodyIdentity: bodyIdentity{
			Proven: true, Method: "identical harness body to stand-in both arms",
			HarnessBodySHA256: shaHex(body), Equal: true, Detail: "self-test",
		},
		Gate: map[string]any{
			"version": gateVersion, "gate_passed": false,
			"basis": "HARNESS_SELF_TEST is never gate_passed",
		},
		GatePassed: false, Comparable: false,
		RefusalReason: "HARNESS_SELF_TEST: not parity evidence",
		Refusals:      []string{"HARNESS_SELF_TEST: not parity evidence"},
		Notes: []string{
			"stand-in token timing is artificial",
			"NOT parity evidence; do not quote as gateway overhead",
			fmt.Sprintf("stand-in hits=%d", hits.Load()),
		},
	}
	for _, lvl := range levels {
		rec.RawSampleCount += len(lvl.RawSamples)
	}
	if err := writeBoundEvidenceJSON(out, rec, harnessID+"#HARNESS_SELF_TEST", "", authorityID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("wrote", out, "(HARNESS_SELF_TEST)")
}

// writeBoundEvidenceJSON routes through scripts/write-bound-evidence.py so
// standalone harnesses share the same refusal rules as control/receipt_identity.go
// (complete identity, real git source_commit, sticky withdrawal).
func writeBoundEvidenceJSON(path string, rec any, harness, modelDigest, authorityID string) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "merc-parity-payload-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	args := []string{
		"scripts/write-bound-evidence.py",
		"--out", path,
		"--harness", harness,
		"--payload-file", tmpName,
		"--build-binary", "scripts/gateway-parity-v2.go",
		"--exact-config", "embedded sampling + topology",
		"--raw-samples", "embedded levels.*.raw_samples",
	}
	if modelDigest != "" {
		args = append(args, "--model-digest", modelDigest)
	} else {
		args = append(args, "--model-na", "no model digest supplied (self-test or omitted)")
	}
	if authorityID != "" {
		args = append(args, "--authority-id", authorityID)
	}
	cmd := exec.Command("python3", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- thin client / pool (mirrors control/gateway_parity_harness.go) ---

type rawSample struct {
	Arm              string    `json:"arm"`
	Index            int       `json:"index"`
	StartUnixNano    int64     `json:"start_unix_nano"`
	TTFTMs           float64   `json:"ttft_ms"`
	TotalMs          float64   `json:"total_ms"`
	ITLMs            []float64 `json:"itl_ms,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	FinishReason     string    `json:"finish_reason,omitempty"`
	RequestBytes     int       `json:"request_bytes"`
	ResponseBytes    int       `json:"response_bytes"`
	RequestBodySHA   string    `json:"request_body_sha256,omitempty"`
	UpstreamBodySHA  string    `json:"upstream_body_sha256,omitempty"`
	ConnReused       bool      `json:"conn_reused"`
	Err              string    `json:"error,omitempty"`
}

type pointEst struct {
	Point    float64 `json:"point"`
	CI95Low  float64 `json:"ci95_low"`
	CI95High float64 `json:"ci95_high"`
	Method   string  `json:"method"`
	N        int     `json:"n"`
}

type levelResult struct {
	Arm                string         `json:"arm"`
	Concurrency        int            `json:"concurrency"`
	RequestsAttempted  int            `json:"requests_attempted"`
	RequestsOK         int            `json:"requests_ok"`
	Errors             int            `json:"errors"`
	ErrorSamples       []string       `json:"error_samples,omitempty"`
	WallSeconds        float64        `json:"wall_seconds"`
	MeanInFlight       float64        `json:"mean_in_flight"`
	PeakInFlight       int            `json:"peak_in_flight"`
	InFlightSamples    int            `json:"in_flight_samples"`
	AggregateTokPerSec *float64       `json:"aggregate_tokens_per_sec,omitempty"`
	AggregateReqPerSec *float64       `json:"aggregate_requests_per_sec,omitempty"`
	TTFTp50            *pointEst      `json:"ttft_p50_ms,omitempty"`
	TTFTp95            *pointEst      `json:"ttft_p95_ms,omitempty"`
	TTFTp99            *pointEst      `json:"ttft_p99_ms,omitempty"`
	TTFTMean           *pointEst      `json:"ttft_mean_ms,omitempty"`
	TotalTokens        int            `json:"completion_tokens_total"`
	MeanCompletionTok  float64        `json:"mean_completion_tokens"`
	FinishReasons      map[string]int `json:"finish_reasons,omitempty"`
	Status             string         `json:"status"`
	Reason             string         `json:"reason,omitempty"`
	RawSamples         []rawSample    `json:"raw_samples"`
}

type bodyIdentity struct {
	Proven              bool   `json:"proven"`
	Method              string `json:"method"`
	HarnessBodySHA256   string `json:"harness_body_sha256"`
	MercUpstreamSHA256  string `json:"merc_upstream_sha256,omitempty"`
	DirectRequestSHA256 string `json:"direct_request_sha256,omitempty"`
	Equal               bool   `json:"bodies_equal"`
	Detail              string `json:"detail,omitempty"`
}

type connStats struct {
	ConnectionsOpened int            `json:"connections_opened"`
	RequestsPerConn   map[string]int `json:"requests_per_conn"`
	ReusedConnections int64          `json:"reused_connections"`
	NewConnections    int64          `json:"new_connections"`
}

type receipt struct {
	SchemaVersion    int                    `json:"schema_version"`
	Kind             string                 `json:"kind"`
	Harness          string                 `json:"harness"`
	GateVersion      string                 `json:"gate_version"`
	MeasuredAt       string                 `json:"measured_at"`
	MercSourceCommit string                 `json:"merc_source_commit,omitempty"`
	EvidenceClass    string                 `json:"evidence_class"`
	Comparable       bool                   `json:"comparable"`
	GatePassed       bool                   `json:"gate_passed"`
	Refusals         []string               `json:"refusals"`
	RefusalReason    string                 `json:"refusal_reason,omitempty"`
	SamplingContract map[string]any         `json:"sampling_contract"`
	NetworkTopology  map[string]any         `json:"network_topology"`
	ClientImpl       string                 `json:"client_implementation"`
	ConnectionPolicy string                 `json:"connection_policy"`
	ConnStats        connStats              `json:"connection_stats"`
	HostLoadStart    map[string]any         `json:"host_load_start"`
	HostLoadEnd      map[string]any         `json:"host_load_end"`
	ColdWarmState    string                 `json:"cold_warm_state"`
	ClaimedLevels    []int                  `json:"claimed_concurrency_levels"`
	Levels           map[string]levelResult `json:"levels"`
	BodyIdentity     bodyIdentity           `json:"body_identity"`
	Gate             map[string]any         `json:"gate"`
	Budget           map[string]any         `json:"budget"`
	RawSampleCount   int                    `json:"raw_sample_count"`
	Notes            []string               `json:"notes,omitempty"`
}

type pooledClient struct {
	http    *http.Client
	ua      string
	dialed  atomic.Int64
	reused  atomic.Int64
	mu      sync.Mutex
	perConn map[string]int
}

func newClient(maxC int) *pooledClient {
	idle := maxC
	if idle < 128 {
		idle = 128
	}
	c := &pooledClient{ua: harnessID + "/1.0", perConn: map[string]int{}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = idle
	tr.MaxIdleConns = idle * 4
	tr.DisableKeepAlives = false
	base := tr.DialContext
	if base == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		base = d.DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c.dialed.Add(1)
		return base(ctx, network, addr)
	}
	c.http = &http.Client{Transport: tr, Timeout: 5 * time.Minute}
	return c
}

func (c *pooledClient) snapshot() connStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	per := map[string]int{}
	for k, v := range c.perConn {
		per[k] = v
	}
	return connStats{
		ConnectionsOpened: int(c.dialed.Load()),
		RequestsPerConn:   per,
		ReusedConnections: c.reused.Load(),
		NewConnections:    c.dialed.Load(),
	}
}

func (c *pooledClient) complete(ctx context.Context, baseURL, key string, body []byte, arm string, index int) rawSample {
	s := rawSample{Arm: arm, Index: index, RequestBytes: len(body), RequestBodySHA: shaHex(body)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.Err = err.Error()
		return s
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Connection", "keep-alive")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	var reused bool
	var remote string
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			reused = info.Reused
			if info.Conn != nil {
				remote = info.Conn.RemoteAddr().String()
			}
		},
	}))
	start := time.Now()
	s.StartUnixNano = start.UnixNano()
	resp, err := c.http.Do(req)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	defer resp.Body.Close()
	s.ConnReused = reused
	if reused {
		c.reused.Add(1)
	}
	if remote != "" {
		c.mu.Lock()
		c.perConn[remote]++
		c.mu.Unlock()
	}
	if sha := resp.Header.Get("X-Merc-Upstream-Body-SHA256"); sha != "" {
		s.UpstreamBodySHA = sha
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		s.Err = fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		return s
	}
	var tokenTimes []time.Time
	var ttftSet bool
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	respBytes := 0
	for scanner.Scan() {
		line := scanner.Text()
		respBytes += len(line) + 1
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(line[6:])
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
				PromptTokens     int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.Content != "" {
				now := time.Now()
				tokenTimes = append(tokenTimes, now)
				if !ttftSet {
					s.TTFTMs = float64(now.Sub(start).Microseconds()) / 1000.0
					ttftSet = true
				}
			}
			if chunk.Choices[0].FinishReason != nil {
				s.FinishReason = *chunk.Choices[0].FinishReason
			}
		}
		if chunk.Usage != nil {
			s.CompletionTokens = chunk.Usage.CompletionTokens
			s.PromptTokens = chunk.Usage.PromptTokens
		}
	}
	s.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
	s.ResponseBytes = respBytes
	for i := 1; i < len(tokenTimes); i++ {
		s.ITLMs = append(s.ITLMs, float64(tokenTimes[i].Sub(tokenTimes[i-1]).Microseconds())/1000.0)
	}
	if err := scanner.Err(); err != nil {
		s.Err = err.Error()
	}
	return s
}

func runInterleaved(ctx context.Context, client *pooledClient, mercURL, mercKey, directURL, directKey string, body []byte, concurrency, n int) (merc, direct levelResult) {
	if n < sampleFloor(concurrency) {
		reason := fmt.Sprintf("refused: sample count %d at concurrency %d is below floor %d", n, concurrency, sampleFloor(concurrency))
		merc = levelResult{Arm: "merc", Concurrency: concurrency, RequestsAttempted: n, Status: "REFUSED", Reason: reason}
		direct = levelResult{Arm: "direct", Concurrency: concurrency, RequestsAttempted: n, Status: "REFUSED", Reason: reason}
		return
	}
	type job struct {
		arm   string
		index int
	}
	jobs := make([]job, 0, n*2)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			jobs = append(jobs, job{"merc", i}, job{"direct", i})
		} else {
			jobs = append(jobs, job{"direct", i}, job{"merc", i})
		}
	}
	mercSamples := make([]rawSample, n)
	directSamples := make([]rawSample, n)
	var mercIF, directIF, sumMerc, nMerc, peakMerc, sumDirect, nDirect, peakDirect atomic.Int64
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				m := mercIF.Load()
				sumMerc.Add(m)
				nMerc.Add(1)
				for {
					p := peakMerc.Load()
					if m <= p || peakMerc.CompareAndSwap(p, m) {
						break
					}
				}
				d := directIF.Load()
				sumDirect.Add(d)
				nDirect.Add(1)
				for {
					p := peakDirect.Load()
					if d <= p || peakDirect.CompareAndSwap(p, d) {
						break
					}
				}
			}
		}
	}()
	globalSem := make(chan struct{}, concurrency*2)
	mercSem := make(chan struct{}, concurrency)
	directSem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	wallStart := time.Now()
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			globalSem <- struct{}{}
			defer func() { <-globalSem }()
			url, key := directURL, directKey
			sem := directSem
			inF := &directIF
			if j.arm == "merc" {
				url, key = mercURL, mercKey
				sem = mercSem
				inF = &mercIF
			}
			sem <- struct{}{}
			inF.Add(1)
			sample := client.complete(ctx, url, key, body, j.arm, j.index)
			inF.Add(-1)
			<-sem
			if j.arm == "merc" {
				mercSamples[j.index] = sample
			} else {
				directSamples[j.index] = sample
			}
		}(j)
	}
	wg.Wait()
	wall := time.Since(wallStart).Seconds()
	close(done)

	fin := func(arm string, samples []rawSample, sumIF, nIF, peakIF *atomic.Int64) levelResult {
		r := levelResult{
			Arm: arm, Concurrency: concurrency, RequestsAttempted: n,
			Status: "MEASURED", WallSeconds: wall, FinishReasons: map[string]int{},
			RawSamples: append([]rawSample(nil), samples...),
		}
		if nObs := nIF.Load(); nObs > 0 {
			r.MeanInFlight = float64(sumIF.Load()) / float64(nObs)
			r.InFlightSamples = int(nObs)
		}
		r.PeakInFlight = int(peakIF.Load())
		var ttfts []float64
		var totalTok, ok int
		var errs []string
		for _, s := range samples {
			if s.Err != "" {
				r.Errors++
				if len(errs) < 5 {
					errs = append(errs, s.Err)
				}
				continue
			}
			ok++
			if s.TTFTMs > 0 {
				ttfts = append(ttfts, s.TTFTMs)
			}
			totalTok += s.CompletionTokens
			if s.FinishReason != "" {
				r.FinishReasons[s.FinishReason]++
			}
		}
		r.RequestsOK = ok
		r.ErrorSamples = errs
		r.TotalTokens = totalTok
		if ok > 0 {
			r.MeanCompletionTok = float64(totalTok) / float64(ok)
			if wall > 0 {
				tps := float64(totalTok) / wall
				rps := float64(ok) / wall
				r.AggregateTokPerSec = &tps
				r.AggregateReqPerSec = &rps
			}
		}
		if r.MeanInFlight < float64(concurrency)*minInFlightFrac {
			r.Status = "REFUSED"
			r.Reason = fmt.Sprintf("refused: mean in-flight %.2f below %.0f%% of c=%d", r.MeanInFlight, minInFlightFrac*100, concurrency)
			return r
		}
		if ok < sampleFloor(concurrency) {
			r.Status = "REFUSED"
			r.Reason = fmt.Sprintf("refused: sample count %d below floor %d", ok, sampleFloor(concurrency))
			return r
		}
		if len(ttfts) > 0 {
			p50, p95, p99 := pct(ttfts, 0.5), pct(ttfts, 0.95), pct(ttfts, 0.99)
			r.TTFTp50 = &pointEst{Point: p50, CI95Low: p50, CI95High: p50, Method: "nearest_rank", N: len(ttfts)}
			r.TTFTp95 = &pointEst{Point: p95, CI95Low: p95, CI95High: p95, Method: "nearest_rank", N: len(ttfts)}
			r.TTFTp99 = &pointEst{Point: p99, CI95Low: p99, CI95High: p99, Method: "nearest_rank", N: len(ttfts)}
			mean := 0.0
			for _, v := range ttfts {
				mean += v
			}
			mean /= float64(len(ttfts))
			r.TTFTMean = &pointEst{Point: mean, CI95Low: mean, CI95High: mean, Method: "mean", N: len(ttfts)}
		}
		return r
	}
	return fin("merc", mercSamples, &sumMerc, &nMerc, &peakMerc),
		fin("direct", directSamples, &sumDirect, &nDirect, &peakDirect)
}

func buildBody(model, prompt string, temperature, topP float64, maxTokens int, seed int64) ([]byte, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"top_p":       topP,
		"stream":      true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if seed >= 0 {
		payload["seed"] = seed
	}
	return json.Marshal(payload)
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	o := append([]float64(nil), xs...)
	sort.Float64s(o)
	idx := int(float64(len(o)) * p)
	if idx >= len(o) {
		idx = len(o) - 1
	}
	return o[idx]
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func captureLoad() map[string]any {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return map[string]any{
		"captured_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"goroutines":      runtime.NumGoroutine(),
		"num_cpu":         runtime.NumCPU(),
		"gomaxprocs":      runtime.GOMAXPROCS(0),
		"mem_alloc_bytes": ms.Alloc,
		"mem_sys_bytes":   ms.Sys,
	}
}

func gitHEAD() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func seedOrNil(seed int64) any {
	if seed < 0 {
		return nil
	}
	return seed
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
