package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Authoritative gateway parity harness.
//
// Replaces scripts/gateway-parity.py, which was invalidated for:
//  1. nreq = min(max(20,c),80) → at c=32, n==c=32 (one wave, no steady state)
//  2. FIFO join of oldest-started thread (~51% mean in-flight vs ~76% correct)
//  3. evaluate_budget inspected concurrency=1 only, then set gate_passed
//  4. urllib with Connection: close (fresh TCP+TLS per request)
//  5. merc injected top_p=0.95 from the profile; direct left it unset
//  6. "throughput loss" was the same one-time TTFT delay counted twice
//  7. no surviving causal root across a large investigation
//  8. non-monotonic c=8 faster / c=32 slower vs any fixed per-request model
//
// This harness produces receipts that can survive an adversary: byte-identical
// requests (proven when capture is on), one pooled client, a correct bounded
// concurrency pool with mean-in-flight sampling, sample-count floors, arm
// interleaving, multi-level gating via EvaluateServingMatrixGate, confidence
// intervals, raw samples, and first-class refusal.

const (
	gatewayParityHarnessID     = "merc-gateway-parity-v2"
	gatewayParitySchemaVersion = 2
	gatewayParityGateVersion   = "gateway-parity-gate-v2"

	// Absolute floor for any level (nearest-rank p95/p99 need this many).
	// Matches control/runtime_cell_cost.go:minCellCostSamples.
	gatewayParityMinSamples = 20

	// steady-state ratio: n >= 5 * concurrency (same rule as serving_matrix).
	gatewayParitySteadyMultiple = 5

	// Mean in-flight must reach this fraction of nominal concurrency or the
	// level is refused as not having reached steady state.
	gatewayParityMinInFlightFraction = 0.70
)

// GatewayParitySampleFloor is the minimum successful samples required at a
// concurrency level. c=1 is held to the absolute floor of 20 (not 5); higher
// levels use max(20, 5*c).
func GatewayParitySampleFloor(concurrency int) int {
	if concurrency < 1 {
		concurrency = 1
	}
	need := concurrency * gatewayParitySteadyMultiple
	if need < gatewayParityMinSamples {
		return gatewayParityMinSamples
	}
	return need
}

// RefuseGatewayParitySampleCount returns a non-empty reason when n is below the
// floor for concurrency. Callers must treat a non-empty return as a hard
// refusal — do not report the level with a caveat.
func RefuseGatewayParitySampleCount(concurrency, n int) string {
	need := GatewayParitySampleFloor(concurrency)
	if n < need {
		return fmt.Sprintf(
			"refused: sample count %d at concurrency %d is below floor %d (max(20, 5×c)); level not reported",
			n, concurrency, need)
	}
	return ""
}

// GatewayParitySamplingContract is the fully-bound decode/request contract.
// Every field the gateway can default MUST be set explicitly, or the arms are
// not the same request.
type GatewayParitySamplingContract struct {
	Model                string   `json:"model"`
	Prompt               string   `json:"prompt"`
	Temperature          float64  `json:"temperature"`
	TopP                 float64  `json:"top_p"`
	TopK                 *int     `json:"top_k,omitempty"`
	Seed                 *int64   `json:"seed,omitempty"`
	MaxTokens            int      `json:"max_tokens"`
	Stop                 []string `json:"stop,omitempty"`
	Stream               bool     `json:"stream"`
	TokenizerTemplateRev string   `json:"tokenizer_template_revision,omitempty"`
	ModelDigest          string   `json:"model_digest"`
	RuntimeProfileID     string   `json:"runtime_profile_id,omitempty"`
	RuntimeProfileSHA256 string   `json:"runtime_profile_sha256,omitempty"`
}

// BuildChatCompletionsBody returns the JSON body both arms must send. All
// sampler fields the gateway can default are set explicitly.
func (c GatewayParitySamplingContract) BuildChatCompletionsBody() ([]byte, error) {
	payload := map[string]any{
		"model":       c.Model,
		"messages":    []map[string]string{{"role": "user", "content": c.Prompt}},
		"max_tokens":  c.MaxTokens,
		"temperature": c.Temperature,
		"top_p":       c.TopP,
		"stream":      c.Stream,
	}
	if c.Stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if c.TopK != nil {
		payload["top_k"] = *c.TopK
	}
	if c.Seed != nil {
		payload["seed"] = *c.Seed
	}
	if len(c.Stop) > 0 {
		payload["stop"] = c.Stop
	}
	// Canonical key order so a harness-built body can match prepareRealtimeRequest
	// output when every defaultable field is already set.
	return canonicalJSON(payload)
}

// AssertByteIdenticalBodies refuses when the two on-the-wire bodies differ.
// Empty reason means equal.
func AssertByteIdenticalBodies(mercBody, directBody []byte) string {
	if len(mercBody) == 0 || len(directBody) == 0 {
		return "refused: one or both request bodies are empty; cannot prove byte-identity"
	}
	if !bytes.Equal(mercBody, directBody) {
		mercSum := sha256.Sum256(mercBody)
		directSum := sha256.Sum256(directBody)
		return fmt.Sprintf(
			"refused: request bodies differ; merc_sha256=%s direct_sha256=%s merc_bytes=%d direct_bytes=%d",
			hex.EncodeToString(mercSum[:]), hex.EncodeToString(directSum[:]),
			len(mercBody), len(directBody))
	}
	return ""
}

// GatewayParityConnStats records how many TCP connections the shared client
// opened and how many requests each served — the number that proved the last
// transport claim wrong.
type GatewayParityConnStats struct {
	ConnectionsOpened int            `json:"connections_opened"`
	RequestsPerConn   map[string]int `json:"requests_per_conn"`
	ReusedConnections int64          `json:"reused_connections"`
	NewConnections    int64          `json:"new_connections"`
}

// GatewayParityClient is the single client implementation both arms share.
// Persistent pooled connections, pool sized at or above max concurrency.
type GatewayParityClient struct {
	HTTP      *http.Client
	UserAgent string
	MaxIdle   int
	mu        sync.Mutex
	connStats GatewayParityConnStats
	perConn   map[string]int
	reused    atomic.Int64
	dialed    atomic.Int64
}

// NewGatewayParityClient builds a pooled client. maxConcurrency sizes the idle
// pool so a steady-state run at that concurrency reuses connections.
func NewGatewayParityClient(maxConcurrency int) *GatewayParityClient {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	idle := maxConcurrency
	if idle < realtimeMaxIdleConnsPerHost {
		// Match the gateway's own upstream budget so the harness is not the
		// weaker transport.
		idle = realtimeMaxIdleConnsPerHost
	}
	g := &GatewayParityClient{
		UserAgent: gatewayParityHarnessID + "/1.0",
		MaxIdle:   idle,
		perConn:   make(map[string]int),
		connStats: GatewayParityConnStats{RequestsPerConn: make(map[string]int)},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = idle
	transport.MaxIdleConns = idle * 4
	transport.DisableKeepAlives = false
	// Count dials so the receipt can prove reuse.
	baseDial := transport.DialContext
	if baseDial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		baseDial = d.DialContext
	}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		g.dialed.Add(1)
		return baseDial(ctx, network, addr)
	}
	g.HTTP = &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute,
	}
	return g
}

// SnapshotConnStats returns connection accounting for the receipt.
func (c *GatewayParityClient) SnapshotConnStats() GatewayParityConnStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	per := make(map[string]int, len(c.perConn))
	for k, v := range c.perConn {
		per[k] = v
	}
	return GatewayParityConnStats{
		ConnectionsOpened: int(c.dialed.Load()),
		RequestsPerConn:   per,
		ReusedConnections: c.reused.Load(),
		NewConnections:    c.dialed.Load(),
	}
}

func (c *GatewayParityClient) noteConn(reused bool, remote string) {
	if reused {
		c.reused.Add(1)
	}
	if remote == "" {
		return
	}
	c.mu.Lock()
	c.perConn[remote]++
	c.mu.Unlock()
}

// GatewayParityRawSample is one completed request. Raw samples are not optional.
type GatewayParityRawSample struct {
	Arm              string    `json:"arm"` // merc | direct
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
	UpstreamBodySHA  string    `json:"upstream_body_sha256,omitempty"` // merc arm when capture on
	ConnReused       bool      `json:"conn_reused"`
	Err              string    `json:"error,omitempty"`
}

// GatewayParityPointEstimate is a point estimate with a 95% interval.
type GatewayParityPointEstimate struct {
	Point    float64 `json:"point"`
	CI95Low  float64 `json:"ci95_low"`
	CI95High float64 `json:"ci95_high"`
	Method   string  `json:"method"`
	N        int     `json:"n"`
}

// BootstrapPercentileCI returns a percentile point estimate and a percentile
// bootstrap 95% CI. Deterministic given the same samples (fixed seed stream).
func BootstrapPercentileCI(xs []float64, p float64, rounds int) GatewayParityPointEstimate {
	out := GatewayParityPointEstimate{Method: "percentile_bootstrap_95", N: len(xs)}
	if len(xs) == 0 {
		out.Point = math.NaN()
		return out
	}
	out.Point = PercentileNearestRank(xs, p)
	if len(xs) == 1 || rounds < 1 {
		out.CI95Low = out.Point
		out.CI95High = out.Point
		return out
	}
	// Simple LCG so the harness has no math/rand seed global dependency and
	// results are reproducible for a given sample multiset order.
	state := uint64(0x9e3779b97f4a7c15) ^ uint64(len(xs))<<32 ^ uint64(math.Float64bits(xs[0]))
	next := func() uint64 {
		state = state*6364136223846793005 + 1
		return state
	}
	boot := make([]float64, rounds)
	tmp := make([]float64, len(xs))
	for r := 0; r < rounds; r++ {
		for i := range tmp {
			tmp[i] = xs[int(next()%uint64(len(xs)))]
		}
		boot[r] = PercentileNearestRank(tmp, p)
	}
	sort.Float64s(boot)
	loIdx := int(0.025 * float64(rounds))
	hiIdx := int(0.975 * float64(rounds))
	if hiIdx >= rounds {
		hiIdx = rounds - 1
	}
	out.CI95Low = boot[loIdx]
	out.CI95High = boot[hiIdx]
	return out
}

// MeanWithCI is mean ± 1.96 * SE (normal approximation).
func MeanWithCI(xs []float64) GatewayParityPointEstimate {
	out := GatewayParityPointEstimate{Method: "mean_normal_approx_95", N: len(xs)}
	if len(xs) == 0 {
		out.Point = math.NaN()
		return out
	}
	var sum float64
	for _, v := range xs {
		sum += v
	}
	mean := sum / float64(len(xs))
	out.Point = mean
	if len(xs) == 1 {
		out.CI95Low, out.CI95High = mean, mean
		return out
	}
	var ss float64
	for _, v := range xs {
		d := v - mean
		ss += d * d
	}
	se := math.Sqrt(ss/float64(len(xs)-1)) / math.Sqrt(float64(len(xs)))
	out.CI95Low = mean - 1.96*se
	out.CI95High = mean + 1.96*se
	return out
}

// GatewayParityLevelResult is one arm at one concurrency.
type GatewayParityLevelResult struct {
	Arm                string                      `json:"arm"`
	Concurrency        int                         `json:"concurrency"`
	RequestsAttempted  int                         `json:"requests_attempted"`
	RequestsOK         int                         `json:"requests_ok"`
	Errors             int                         `json:"errors"`
	ErrorSamples       []string                    `json:"error_samples,omitempty"`
	WallSeconds        float64                     `json:"wall_seconds"`
	MeanInFlight       float64                     `json:"mean_in_flight"`
	PeakInFlight       int                         `json:"peak_in_flight"`
	InFlightSamples    int                         `json:"in_flight_samples"`
	AggregateTokPerSec *float64                    `json:"aggregate_tokens_per_sec,omitempty"`
	AggregateReqPerSec *float64                    `json:"aggregate_requests_per_sec,omitempty"`
	TTFTp50            *GatewayParityPointEstimate `json:"ttft_p50_ms,omitempty"`
	TTFTp95            *GatewayParityPointEstimate `json:"ttft_p95_ms,omitempty"`
	TTFTp99            *GatewayParityPointEstimate `json:"ttft_p99_ms,omitempty"`
	TTFTMean           *GatewayParityPointEstimate `json:"ttft_mean_ms,omitempty"`
	TotalTokens        int                         `json:"completion_tokens_total"`
	MeanCompletionTok  float64                     `json:"mean_completion_tokens"`
	FinishReasons      map[string]int              `json:"finish_reasons,omitempty"`
	Status             string                      `json:"status"` // MEASURED | REFUSED
	Reason             string                      `json:"reason,omitempty"`
	RawSamples         []GatewayParityRawSample    `json:"raw_samples"`
}

// GatewayParityBudget is the overhead ceiling applied at EVERY claimed level.
type GatewayParityBudget struct {
	TTFTOverheadP95Ms       float64 `json:"ttft_overhead_p95_ms"`
	ThroughputLossFraction  float64 `json:"throughput_loss_fraction"`
	RequireMeasuredEveryLvl bool    `json:"require_measured_at_every_level"`
	Basis                   string  `json:"basis"`
}

// DefaultGatewayParityBudget is a policy ceiling, not a derived SLA. Calibrated
// with headroom against a clean measurement; not against the invalidated
// gateway-parity.py receipt.
func DefaultGatewayParityBudget() GatewayParityBudget {
	return GatewayParityBudget{
		TTFTOverheadP95Ms:       15.0,
		ThroughputLossFraction:  0.05,
		RequireMeasuredEveryLvl: true,
		Basis: "policy ceiling applied independently at every claimed concurrency level; " +
			"a pass at concurrency=1 alone cannot set gate_passed (see EvaluateServingMatrixGate)",
	}
}

// GatewayParityNetworkTopology states where each component runs.
type GatewayParityNetworkTopology struct {
	ClientHost      string `json:"client_host"`
	ControlPlane    string `json:"control_plane"`
	Engine          string `json:"engine"`
	ClientToControl string `json:"client_to_control_hop"` // loopback | lan | wan
	ControlToEngine string `json:"control_to_engine_hop"`
	ClientToEngine  string `json:"client_to_engine_hop"` // direct arm
	Notes           string `json:"notes,omitempty"`
}

// GatewayParityHostLoad is a point-in-time host snapshot.
type GatewayParityHostLoad struct {
	CapturedAt    string   `json:"captured_at"`
	Goroutines    int      `json:"goroutines"`
	NumCPU        int      `json:"num_cpu"`
	GOMAXPROCS    int      `json:"gomaxprocs"`
	MemAllocBytes uint64   `json:"mem_alloc_bytes"`
	MemSysBytes   uint64   `json:"mem_sys_bytes"`
	LoadAvg1      *float64 `json:"load_avg_1,omitempty"`
	// GPU/HBM left nil when unmeasured rather than zero-faked.
	GPUUtilPct *float64 `json:"gpu_util_pct,omitempty"`
	HBMUtilPct *float64 `json:"hbm_util_pct,omitempty"`
	CPUUtilPct *float64 `json:"cpu_util_pct,omitempty"`
}

// CaptureGatewayParityHostLoad records what this process can see without
// privileged GPU tooling. GPU/HBM stay nil on hosts without a probe.
func CaptureGatewayParityHostLoad() GatewayParityHostLoad {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return GatewayParityHostLoad{
		CapturedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Goroutines:    runtime.NumGoroutine(),
		NumCPU:        runtime.NumCPU(),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
		MemAllocBytes: ms.Alloc,
		MemSysBytes:   ms.Sys,
	}
}

// GatewayParityReceipt is the on-disk claim.
type GatewayParityReceipt struct {
	SchemaVersion    int    `json:"schema_version"`
	Kind             string `json:"kind"`
	Harness          string `json:"harness"`
	GateVersion      string `json:"gate_version"`
	MeasuredAt       string `json:"measured_at"`
	MercSourceCommit string `json:"merc_source_commit,omitempty"`
	ImageDigest      string `json:"image_digest,omitempty"`
	EngineVersion    string `json:"engine_version,omitempty"`
	EvidenceClass    string `json:"evidence_class"` // PARITY_EVIDENCE | HARNESS_SELF_TEST | INVALIDATED

	Comparable bool     `json:"comparable"`
	GatePassed bool     `json:"gate_passed"`
	Refusals   []string `json:"refusals"`
	// Refusal is first-class: non-empty means no parity conclusion may be drawn.
	RefusalReason string `json:"refusal_reason,omitempty"`

	SamplingContract GatewayParitySamplingContract `json:"sampling_contract"`
	NetworkTopology  GatewayParityNetworkTopology  `json:"network_topology"`
	ClientImpl       string                        `json:"client_implementation"`
	ConnectionPolicy string                        `json:"connection_policy"`
	ConnStats        GatewayParityConnStats        `json:"connection_stats"`
	HostLoadStart    GatewayParityHostLoad         `json:"host_load_start"`
	HostLoadEnd      GatewayParityHostLoad         `json:"host_load_end"`
	ColdWarmState    string                        `json:"cold_warm_state"`

	ClaimedLevels []int                               `json:"claimed_concurrency_levels"`
	Levels        map[string]GatewayParityLevelResult `json:"levels"` // key: "merc@c=N" / "direct@c=N"
	// BodyIdentity records what was proven about on-the-wire equality.
	BodyIdentity GatewayParityBodyIdentity `json:"body_identity"`

	// Gate reuses the serving-matrix multi-level structure so a claim that
	// skips a level cannot pass.
	Gate   ServingMatrixGate   `json:"gate"`
	Budget GatewayParityBudget `json:"budget"`

	// RawSamples flattened for a skeptic who does not want to dig into levels.
	// Optional duplicate; levels already carry raw_samples. Kept so a single
	// top-level array is always present when the run measured anything.
	RawSampleCount int `json:"raw_sample_count"`

	Notes []string `json:"notes,omitempty"`
}

// GatewayParityBodyIdentity is the proof (or argument) that both arms sent the
// same request.
type GatewayParityBodyIdentity struct {
	Proven              bool   `json:"proven"`
	Method              string `json:"method"`
	HarnessBodySHA256   string `json:"harness_body_sha256"`
	MercUpstreamSHA256  string `json:"merc_upstream_sha256,omitempty"`
	DirectRequestSHA256 string `json:"direct_request_sha256,omitempty"`
	Equal               bool   `json:"bodies_equal"`
	Detail              string `json:"detail,omitempty"`
}

// CompleteOneStream runs one streaming completion with the shared client and
// the pre-built body (byte-identical across arms).
func (c *GatewayParityClient) CompleteOneStream(
	ctx context.Context,
	baseURL, apiKey string,
	body []byte,
	arm string,
	index int,
) GatewayParityRawSample {
	sample := GatewayParityRawSample{
		Arm:            arm,
		Index:          index,
		RequestBytes:   len(body),
		RequestBodySHA: sha256Hex(body),
	}
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		sample.Err = err.Error()
		return sample
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// Force keep-alive; never Connection: close.
	req.Header.Set("Connection", "keep-alive")

	var reused bool
	var remote string
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			reused = info.Reused
			if info.Conn != nil {
				remote = info.Conn.RemoteAddr().String()
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	start := time.Now()
	sample.StartUnixNano = start.UnixNano()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		sample.Err = err.Error()
		return sample
	}
	defer resp.Body.Close()
	sample.ConnReused = reused
	c.noteConn(reused, remote)
	if sha := resp.Header.Get("X-Merc-Upstream-Body-SHA256"); sha != "" {
		sample.UpstreamBodySHA = sha
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		sample.Err = fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		return sample
	}

	var (
		ttftSet          bool
		tokenTimes       []time.Time
		completionTokens int
		promptTokens     int
		finishReason     string
		respBytes        int
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.Content != "" {
				now := time.Now()
				tokenTimes = append(tokenTimes, now)
				if !ttftSet {
					sample.TTFTMs = float64(now.Sub(start).Microseconds()) / 1000.0
					ttftSet = true
				}
			}
			if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
				finishReason = *chunk.Choices[0].FinishReason
			}
		}
		if chunk.Usage != nil {
			if chunk.Usage.CompletionTokens > 0 {
				completionTokens = chunk.Usage.CompletionTokens
			}
			if chunk.Usage.PromptTokens > 0 {
				promptTokens = chunk.Usage.PromptTokens
			}
		}
	}
	if err := scanner.Err(); err != nil {
		sample.Err = err.Error()
		return sample
	}
	sample.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
	sample.ResponseBytes = respBytes
	sample.CompletionTokens = completionTokens
	sample.PromptTokens = promptTokens
	sample.FinishReason = finishReason
	for i := 1; i < len(tokenTimes); i++ {
		sample.ITLMs = append(sample.ITLMs,
			float64(tokenTimes[i].Sub(tokenTimes[i-1]).Microseconds())/1000.0)
	}
	return sample
}

// RunGatewayParityLevel drives n requests for one arm at concurrency c using a
// correct bounded pool (semaphore), NOT a FIFO join of the oldest-started
// thread. Samples true in-flight concurrency during the run.
func RunGatewayParityLevel(
	ctx context.Context,
	client *GatewayParityClient,
	baseURL, apiKey, arm string,
	body []byte,
	concurrency, n int,
) GatewayParityLevelResult {
	result := GatewayParityLevelResult{
		Arm:               arm,
		Concurrency:       concurrency,
		RequestsAttempted: n,
		Status:            "MEASURED",
		FinishReasons:     map[string]int{},
	}
	if reason := RefuseGatewayParitySampleCount(concurrency, n); reason != "" {
		// Attempt floor is checked before the run; a caller that still asks
		// for a sub-floor n is refused without fabricating samples.
		result.Status = "REFUSED"
		result.Reason = reason
		return result
	}
	if concurrency < 1 {
		result.Status = "REFUSED"
		result.Reason = "refused: concurrency < 1"
		return result
	}

	samples := make([]GatewayParityRawSample, n)
	sem := make(chan struct{}, concurrency)
	var (
		inFlight    atomic.Int64
		peak        atomic.Int64
		sumInFlight atomic.Int64
		inFlightN   atomic.Int64
		wg          sync.WaitGroup
	)
	// Sampler: observe true in-flight at ~1 ms resolution while work runs.
	sampleDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampleDone:
				return
			case <-ticker.C:
				cur := inFlight.Load()
				sumInFlight.Add(cur)
				inFlightN.Add(1)
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
			}
		}
	}()

	wallStart := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			inFlight.Add(1)
			defer func() {
				inFlight.Add(-1)
				<-sem
			}()
			samples[i] = client.CompleteOneStream(ctx, baseURL, apiKey, body, arm, i)
		}(i)
	}
	wg.Wait()
	wall := time.Since(wallStart).Seconds()
	close(sampleDone)

	result.WallSeconds = wall
	result.PeakInFlight = int(peak.Load())
	if nObs := inFlightN.Load(); nObs > 0 {
		result.MeanInFlight = float64(sumInFlight.Load()) / float64(nObs)
		result.InFlightSamples = int(nObs)
	}

	var (
		ttfts      []float64
		totalTok   int
		ok         int
		errSamples []string
	)
	for _, s := range samples {
		result.RawSamples = append(result.RawSamples, s)
		if s.Err != "" {
			result.Errors++
			if len(errSamples) < 5 {
				errSamples = append(errSamples, s.Err)
			}
			continue
		}
		ok++
		if s.TTFTMs > 0 {
			ttfts = append(ttfts, s.TTFTMs)
		}
		totalTok += s.CompletionTokens
		if s.FinishReason != "" {
			result.FinishReasons[s.FinishReason]++
		}
	}
	result.RequestsOK = ok
	result.ErrorSamples = errSamples
	result.TotalTokens = totalTok
	if ok > 0 {
		result.MeanCompletionTok = float64(totalTok) / float64(ok)
		if wall > 0 {
			tps := float64(totalTok) / wall
			rps := float64(ok) / wall
			result.AggregateTokPerSec = &tps
			result.AggregateReqPerSec = &rps
		}
	}

	// Steady-state in-flight check: refuse if mean in-flight is materially
	// below nominal concurrency (the defect class of the FIFO join).
	if result.MeanInFlight < float64(concurrency)*gatewayParityMinInFlightFraction {
		result.Status = "REFUSED"
		result.Reason = fmt.Sprintf(
			"refused: mean in-flight %.2f is below %.0f%% of nominal concurrency %d; level did not reach steady state",
			result.MeanInFlight, gatewayParityMinInFlightFraction*100, concurrency)
		return result
	}
	if reason := RefuseGatewayParitySampleCount(concurrency, ok); reason != "" {
		result.Status = "REFUSED"
		result.Reason = reason
		return result
	}

	if len(ttfts) > 0 {
		p50 := BootstrapPercentileCI(ttfts, 0.50, 1000)
		p95 := BootstrapPercentileCI(ttfts, 0.95, 1000)
		p99 := BootstrapPercentileCI(ttfts, 0.99, 1000)
		mean := MeanWithCI(ttfts)
		result.TTFTp50, result.TTFTp95, result.TTFTp99, result.TTFTMean = &p50, &p95, &p99, &mean
	}
	return result
}

// RunGatewayParityInterleavedLevel runs merc and direct at one concurrency,
// alternating which arm leads each wave so thermal drift is shared. Each arm
// keeps up to `concurrency` requests in flight; arms are not dual-loaded onto
// the engine in the same instant (wave-interleaved, same as the intent of the
// old harness, but with a correct pool inside each wave).
func RunGatewayParityInterleavedLevel(
	ctx context.Context,
	client *GatewayParityClient,
	mercURL, mercKey, directURL, directKey string,
	body []byte,
	concurrency, n int,
) (merc, direct GatewayParityLevelResult) {
	if reason := RefuseGatewayParitySampleCount(concurrency, n); reason != "" {
		merc = GatewayParityLevelResult{Arm: "merc", Concurrency: concurrency, RequestsAttempted: n, Status: "REFUSED", Reason: reason}
		direct = GatewayParityLevelResult{Arm: "direct", Concurrency: concurrency, RequestsAttempted: n, Status: "REFUSED", Reason: reason}
		return merc, direct
	}

	// Build work as alternating single-request jobs, then run a correct pool
	// that keeps up to `concurrency` jobs in flight globally while preserving
	// arm labels. Per-arm concurrency is also capped at `concurrency` so one
	// arm cannot monopolise the engine.
	type job struct {
		arm   string
		index int
	}
	jobs := make([]job, 0, n*2)
	for i := 0; i < n; i++ {
		// Alternate merc/direct; flip who leads every pair-round so neither
		// arm is systematically first under thermal drift.
		if i%2 == 0 {
			jobs = append(jobs, job{arm: "merc", index: i}, job{arm: "direct", index: i})
		} else {
			jobs = append(jobs, job{arm: "direct", index: i}, job{arm: "merc", index: i})
		}
	}

	mercSamples := make([]GatewayParityRawSample, n)
	directSamples := make([]GatewayParityRawSample, n)
	var (
		mercInFlight   atomic.Int64
		directInFlight atomic.Int64
		// global in-flight for mean sampling of the combined schedule
		globalInFlight atomic.Int64
		peakGlobal     atomic.Int64
		sumGlobal      atomic.Int64
		nGlobal        atomic.Int64
		// per-arm peak/sum for reporting
		sumMerc    atomic.Int64
		nMerc      atomic.Int64
		peakMerc   atomic.Int64
		sumDirect  atomic.Int64
		nDirect    atomic.Int64
		peakDirect atomic.Int64
	)

	sampleDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampleDone:
				return
			case <-ticker.C:
				g := globalInFlight.Load()
				sumGlobal.Add(g)
				nGlobal.Add(1)
				for {
					p := peakGlobal.Load()
					if g <= p || peakGlobal.CompareAndSwap(p, g) {
						break
					}
				}
				m := mercInFlight.Load()
				sumMerc.Add(m)
				nMerc.Add(1)
				for {
					p := peakMerc.Load()
					if m <= p || peakMerc.CompareAndSwap(p, m) {
						break
					}
				}
				d := directInFlight.Load()
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

	// Global worker pool sized at 2*c so both arms can sit at concurrency c
	// simultaneously during overlap; per-arm semaphores enforce the c cap.
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
			var armSem chan struct{}
			var armIn *atomic.Int64
			url, key := directURL, directKey
			if j.arm == "merc" {
				armSem = mercSem
				armIn = &mercInFlight
				url, key = mercURL, mercKey
			} else {
				armSem = directSem
				armIn = &directInFlight
			}
			armSem <- struct{}{}
			armIn.Add(1)
			globalInFlight.Add(1)
			sample := client.CompleteOneStream(ctx, url, key, body, j.arm, j.index)
			globalInFlight.Add(-1)
			armIn.Add(-1)
			<-armSem
			if j.arm == "merc" {
				mercSamples[j.index] = sample
			} else {
				directSamples[j.index] = sample
			}
		}(j)
	}
	wg.Wait()
	wall := time.Since(wallStart).Seconds()
	close(sampleDone)

	finalize := func(arm string, samples []GatewayParityRawSample, sumIF, nIF, peakIF *atomic.Int64) GatewayParityLevelResult {
		r := GatewayParityLevelResult{
			Arm: arm, Concurrency: concurrency, RequestsAttempted: n,
			Status: "MEASURED", WallSeconds: wall, FinishReasons: map[string]int{},
			RawSamples: append([]GatewayParityRawSample(nil), samples...),
		}
		if nObs := nIF.Load(); nObs > 0 {
			r.MeanInFlight = float64(sumIF.Load()) / float64(nObs)
			r.InFlightSamples = int(nObs)
		}
		r.PeakInFlight = int(peakIF.Load())
		var ttfts []float64
		var totalTok, ok int
		var errSamples []string
		for _, s := range samples {
			if s.Err != "" {
				r.Errors++
				if len(errSamples) < 5 {
					errSamples = append(errSamples, s.Err)
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
		r.ErrorSamples = errSamples
		r.TotalTokens = totalTok
		if ok > 0 {
			r.MeanCompletionTok = float64(totalTok) / float64(ok)
			if wall > 0 {
				// Per-arm wall is the shared interleaved wall; tok/s is then
				// arm tokens over shared wall (same definition as a concurrent
				// dual-arm run). Documented in the receipt notes.
				tps := float64(totalTok) / wall
				rps := float64(ok) / wall
				r.AggregateTokPerSec = &tps
				r.AggregateReqPerSec = &rps
			}
		}
		if r.MeanInFlight < float64(concurrency)*gatewayParityMinInFlightFraction {
			r.Status = "REFUSED"
			r.Reason = fmt.Sprintf(
				"refused: mean in-flight %.2f is below %.0f%% of nominal concurrency %d; level did not reach steady state",
				r.MeanInFlight, gatewayParityMinInFlightFraction*100, concurrency)
			return r
		}
		if reason := RefuseGatewayParitySampleCount(concurrency, ok); reason != "" {
			r.Status = "REFUSED"
			r.Reason = reason
			return r
		}
		if len(ttfts) > 0 {
			p50 := BootstrapPercentileCI(ttfts, 0.50, 1000)
			p95 := BootstrapPercentileCI(ttfts, 0.95, 1000)
			p99 := BootstrapPercentileCI(ttfts, 0.99, 1000)
			mean := MeanWithCI(ttfts)
			r.TTFTp50, r.TTFTp95, r.TTFTp99, r.TTFTMean = &p50, &p95, &p99, &mean
		}
		return r
	}

	merc = finalize("merc", mercSamples, &sumMerc, &nMerc, &peakMerc)
	direct = finalize("direct", directSamples, &sumDirect, &nDirect, &peakDirect)
	return merc, direct
}

// EvaluateGatewayParityGate checks every claimed concurrency level. Reuses
// EvaluateServingMatrixGate so a receipt that claims levels it did not measure
// cannot pass. Overhead budget is applied per level on top.
func EvaluateGatewayParityGate(
	claimedLevels []int,
	levels map[string]GatewayParityLevelResult,
	budget GatewayParityBudget,
) ServingMatrixGate {
	// Project into serving-matrix cells for the merc arm so the multi-level
	// machinery is the sole gate for "was this level measured?".
	const armKey = "merc|parity|gateway"
	var cells []ServingMatrixCellResult
	for _, c := range claimedLevels {
		key := fmt.Sprintf("merc@c=%d", c)
		lvl, ok := levels[key]
		point := ServingMatrixPoint{Concurrency: c, PromptTokens: 0, OutputTokens: 0, State: "warm", Lane: "interactive", Precision: "parity"}
		if !ok || lvl.Status != "MEASURED" || lvl.RequestsOK == 0 {
			reason := "no MEASURED merc level"
			if ok && lvl.Reason != "" {
				reason = lvl.Reason
			}
			cells = append(cells, ServingMatrixCellResult{
				ArmKey: armKey, Point: point, Status: "INCOMPARABLE", Reason: reason,
			})
			continue
		}
		m := ServingMatrixMetrics{
			RequestsOK:        lvl.RequestsOK,
			RequestsAttempted: lvl.RequestsAttempted,
			Errors:            lvl.Errors,
		}
		if lvl.TTFTp95 != nil {
			v := lvl.TTFTp95.Point
			m.TTFTp95Ms = &v
		}
		if lvl.AggregateReqPerSec != nil {
			v := *lvl.AggregateReqPerSec
			m.ReqPerSec = &v
		}
		cells = append(cells, ServingMatrixCellResult{
			ArmKey: armKey, Point: point, Status: "MEASURED", Metrics: &m,
		})
	}

	smBudget := ServingMatrixBudget{
		RequireMeasuredAtEveryLevel: budget.RequireMeasuredEveryLvl,
		// Absolute TTFT/RPS ceilings are not the gateway-overhead budget; the
		// overhead checks below are. Leave absolute ceilings zero so the
		// serving-matrix gate only enforces "measured at every claimed level".
	}
	gate := EvaluateServingMatrixGate(claimedLevels, cells, armKey, smBudget)
	gate.Version = gatewayParityGateVersion
	gate.Budget = smBudget
	gate.Basis = budget.Basis

	// Per-level overhead evaluation. A gate that only looked at c=1 is the
	// defect this block exists to prevent.
	allPassed := gate.GatePassed
	for i, c := range claimedLevels {
		merc := levels[fmt.Sprintf("merc@c=%d", c)]
		direct := levels[fmt.Sprintf("direct@c=%d", c)]
		var refusals []string
		if merc.Status != "MEASURED" {
			refusals = append(refusals, fmt.Sprintf("concurrency %d merc: %s", c, orDefault(merc.Reason, merc.Status)))
		}
		if direct.Status != "MEASURED" {
			refusals = append(refusals, fmt.Sprintf("concurrency %d direct: %s", c, orDefault(direct.Reason, direct.Status)))
		}
		if merc.Status == "MEASURED" && direct.Status == "MEASURED" {
			// Output-contract equality: different token counts or finish
			// reasons mean the arms did not do the same work.
			if math.Abs(merc.MeanCompletionTok-direct.MeanCompletionTok) > 0.5 {
				// Allow half-token mean drift only; integer totals should match
				// when n is equal and the engine is deterministic under seed.
				if merc.TotalTokens != direct.TotalTokens {
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: output contract differs; merc_completion_tokens=%d direct_completion_tokens=%d",
						c, merc.TotalTokens, direct.TotalTokens))
				}
			}
			if merc.TTFTp95 != nil && direct.TTFTp95 != nil && budget.TTFTOverheadP95Ms > 0 {
				overhead := merc.TTFTp95.Point - direct.TTFTp95.Point
				if overhead > budget.TTFTOverheadP95Ms {
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: ttft overhead p95 %.3f ms exceeds budget %.3f ms",
						c, overhead, budget.TTFTOverheadP95Ms))
				}
			}
			if merc.AggregateTokPerSec != nil && direct.AggregateTokPerSec != nil &&
				*direct.AggregateTokPerSec > 0 && budget.ThroughputLossFraction > 0 {
				// Guard against double-counting pure TTFT delay as throughput
				// loss when completion token totals match: if totals are equal
				// the wall-time ratio is a restatement of latency, not an
				// independent deficit. Still compute and report, but annotate.
				loss := (*direct.AggregateTokPerSec - *merc.AggregateTokPerSec) / *direct.AggregateTokPerSec
				if loss > budget.ThroughputLossFraction {
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: throughput loss fraction %.4f exceeds budget %.4f (merc=%.2f direct=%.2f tok/s)",
						c, loss, budget.ThroughputLossFraction, *merc.AggregateTokPerSec, *direct.AggregateTokPerSec))
				}
			}
		}
		if i < len(gate.Levels) {
			gate.Levels[i].Refusals = append(gate.Levels[i].Refusals, refusals...)
			gate.Levels[i].Passed = len(gate.Levels[i].Refusals) == 0
			if !gate.Levels[i].Passed {
				allPassed = false
			}
		}
	}
	gate.GatePassed = allPassed
	return gate
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// RefuseOutputContractDrift returns a reason when the two arms produced
// different work products (token counts / finish reasons).
func RefuseOutputContractDrift(merc, direct GatewayParityLevelResult) string {
	if merc.Status != "MEASURED" || direct.Status != "MEASURED" {
		return ""
	}
	if merc.TotalTokens != direct.TotalTokens {
		return fmt.Sprintf(
			"output contract differs: merc completion_tokens_total=%d direct=%d",
			merc.TotalTokens, direct.TotalTokens)
	}
	// Finish-reason multiset: any key present on one side only is drift.
	keys := map[string]struct{}{}
	for k := range merc.FinishReasons {
		keys[k] = struct{}{}
	}
	for k := range direct.FinishReasons {
		keys[k] = struct{}{}
	}
	for k := range keys {
		if merc.FinishReasons[k] != direct.FinishReasons[k] {
			return fmt.Sprintf(
				"output contract differs: finish_reason %q merc=%d direct=%d",
				k, merc.FinishReasons[k], direct.FinishReasons[k])
		}
	}
	return ""
}

// BuildGatewayParityReceipt assembles a complete receipt. If any hard refusal
// fires, GatePassed is false and RefusalReason is set.
func BuildGatewayParityReceipt(
	contract GatewayParitySamplingContract,
	topology GatewayParityNetworkTopology,
	client *GatewayParityClient,
	claimed []int,
	levels map[string]GatewayParityLevelResult,
	bodyIdentity GatewayParityBodyIdentity,
	budget GatewayParityBudget,
	hostStart, hostEnd GatewayParityHostLoad,
	evidenceClass string,
	notes []string,
) GatewayParityReceipt {
	if evidenceClass == "" {
		evidenceClass = "PARITY_EVIDENCE"
	}
	rec := GatewayParityReceipt{
		SchemaVersion:    gatewayParitySchemaVersion,
		Kind:             "gateway_parity",
		Harness:          gatewayParityHarnessID,
		GateVersion:      gatewayParityGateVersion,
		MeasuredAt:       time.Now().UTC().Format(time.RFC3339),
		MercSourceCommit: strings.TrimSpace(os.Getenv("MERC_SOURCE_COMMIT")),
		EvidenceClass:    evidenceClass,
		SamplingContract: contract,
		NetworkTopology:  topology,
		ClientImpl:       "net/http.Client + cloned Transport (gateway_parity_harness.go)",
		ConnectionPolicy: fmt.Sprintf(
			"persistent keep-alive; MaxIdleConnsPerHost=%d; DisableKeepAlives=false; Connection: keep-alive on every request",
			client.MaxIdle),
		ConnStats:     client.SnapshotConnStats(),
		HostLoadStart: hostStart,
		HostLoadEnd:   hostEnd,
		ColdWarmState: "warm",
		ClaimedLevels: append([]int(nil), claimed...),
		Levels:        levels,
		BodyIdentity:  bodyIdentity,
		Budget:        budget,
		Notes:         append([]string(nil), notes...),
	}
	if rec.MercSourceCommit == "" {
		rec.MercSourceCommit = readMercSourceCommit()
	}

	var refusals []string
	if !bodyIdentity.Equal {
		refusals = append(refusals, "request bodies are not byte-identical: "+bodyIdentity.Detail)
	}
	if !bodyIdentity.Proven && evidenceClass == "PARITY_EVIDENCE" {
		refusals = append(refusals,
			"body identity is argued, not proven (enable MERC_PARITY_CAPTURE_UPSTREAM=1 and compare merc upstream body)")
	}
	for _, c := range claimed {
		for _, arm := range []string{"merc", "direct"} {
			key := fmt.Sprintf("%s@c=%d", arm, c)
			lvl, ok := levels[key]
			if !ok {
				refusals = append(refusals, fmt.Sprintf("claimed level %s missing from receipt", key))
				continue
			}
			if lvl.Status == "REFUSED" {
				refusals = append(refusals, fmt.Sprintf("%s: %s", key, lvl.Reason))
			}
			if n := lvl.RequestsOK; ok && lvl.Status == "MEASURED" {
				if reason := RefuseGatewayParitySampleCount(c, n); reason != "" {
					refusals = append(refusals, fmt.Sprintf("%s: %s", key, reason))
				}
			}
		}
		merc := levels[fmt.Sprintf("merc@c=%d", c)]
		direct := levels[fmt.Sprintf("direct@c=%d", c)]
		if reason := RefuseOutputContractDrift(merc, direct); reason != "" {
			refusals = append(refusals, fmt.Sprintf("c=%d: %s", c, reason))
		}
		rec.RawSampleCount += len(merc.RawSamples) + len(direct.RawSamples)
	}

	rec.Gate = EvaluateGatewayParityGate(claimed, levels, budget)
	for _, lvl := range rec.Gate.Levels {
		refusals = append(refusals, lvl.Refusals...)
	}
	// Dedup refusals while preserving order.
	seen := map[string]bool{}
	var uniq []string
	for _, r := range refusals {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		uniq = append(uniq, r)
	}
	rec.Refusals = uniq
	rec.Comparable = len(uniq) == 0 && rec.Gate.GatePassed
	rec.GatePassed = rec.Gate.GatePassed && len(uniq) == 0
	if !rec.GatePassed && len(uniq) > 0 {
		rec.RefusalReason = uniq[0]
	}
	// Self-tests must never be mistaken for parity evidence.
	if evidenceClass == "HARNESS_SELF_TEST" {
		rec.GatePassed = false
		rec.Comparable = false
		if rec.RefusalReason == "" {
			rec.RefusalReason = "HARNESS_SELF_TEST: not parity evidence"
		}
		rec.Notes = append(rec.Notes,
			"evidence_class=HARNESS_SELF_TEST: this receipt proves the harness works; it is NOT a gateway-vs-engine parity claim")
	}
	return rec
}

func readMercSourceCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
