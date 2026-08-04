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

	"github.com/google/uuid"
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
// requests (proven — never defaulted), one pooled client, a correct bounded
// concurrency pool (engine in-flight never exceeds claimed c), sample-count
// floors, alternating-batch arm interleaving, multi-level interval gating
// (PASS/FAIL/INCONCLUSIVE from bootstrap CIs), raw samples, and first-class
// refusal. The live CLI (control gateway-parity / scripts/gateway-parity-v2.go)
// calls the same EvaluateGatewayParityGate and BuildGatewayParityReceipt as
// the tests — there is no forked gate.

const (
	gatewayParityHarnessID     = "merc-gateway-parity-v2"
	gatewayParitySchemaVersion = 2
	// v4: wave-block bootstrap of TTFT shift, no degenerate CI collapse,
	// wave floor (not request floor), renamed ttft_shift_q95_ms.
	gatewayParityGateVersion = "gateway-parity-gate-v4"

	// steady-state ratio: at least this many full waves (n >= multiple * c).
	// Same rule as serving_matrix; the wave is the independent block.
	gatewayParitySteadyMultiple = 5

	// Mean in-flight must reach this fraction of nominal concurrency or the
	// level is refused as not having reached steady state.
	gatewayParityMinInFlightFraction = 0.70

	// Maximum per-level error rate. The CLI oversamples above the floor so a
	// stray transport error does not refuse a good run; without a bound that
	// same slack lets a run with a real error problem still hit the floor on
	// survivors (survival bias). 5% is small enough that a systemic failure
	// mode cannot launder survivors into PASS, and loose enough that a single
	// transient at low n still clears. Independent of oversample size.
	gatewayParityMaxErrorRate = 0.05

	// gatewayParityIdentifyingWaves = ceil(ln(0.025)/ln(0.95)).
	// A percentile-bootstrap 97.5% upper bound is only identified when
	// P(true q95 > sample max) = 0.95^W ≤ 0.025. Below that the bootstrap UB
	// is capped at the sample maximum and nominal coverage fails.
	gatewayParityIdentifyingWaves = 72

	// Cap on requests per arm so a full ladder stays within roughly an order
	// of the historical c=128 cost (~706) rather than 72×128 = 9216. When
	// identifyingWaves * c exceeds this, the wave floor drops to budget/c and
	// the receipt must state that the upper bound is not identified.
	gatewayParityRequestBudgetPerArm = 768

	// Bootstrap replicates for wave-block and per-arm percentile CIs.
	gatewayParityBootstrapRounds = 1000
)

// gatewayParityRequiredLadder is the concurrency set a PARITY_EVIDENCE claim
// must measure. A single level (e.g. -concurrency 1) is not a parity claim.
var gatewayParityRequiredLadder = []int{1, 8, 32}

// GatewayParityWaveFloor is the minimum number of complete scheduler waves
// required at a concurrency level. The harness launches waves of size c
// (semaphore capacity), so the independent observation is the wave, not the
// request. n_samples = wave_floor * c when every wave is full.
//
// Schedule (request budget 768/arm):
//
//	c≤10:  72 waves — full identification of the 97.5% bootstrap upper bound
//	c=32:  24 waves — cost-capped; upper bound not identified
//	c=64:  12 waves — cost-capped; upper bound not identified
//	c=128:  6 waves — cost-capped; upper bound not identified
//
// Never fewer than gatewayParitySteadyMultiple waves (steady-state).
func GatewayParityWaveFloor(concurrency int) int {
	if concurrency < 1 {
		concurrency = 1
	}
	if gatewayParityIdentifyingWaves*concurrency <= gatewayParityRequestBudgetPerArm {
		return gatewayParityIdentifyingWaves
	}
	w := gatewayParityRequestBudgetPerArm / concurrency
	if w < gatewayParitySteadyMultiple {
		w = gatewayParitySteadyMultiple
	}
	return w
}

// GatewayParitySampleFloor is wave_floor × concurrency. Kept as a sample count
// for call sites that size n; the statistical unit is the wave.
func GatewayParitySampleFloor(concurrency int) int {
	if concurrency < 1 {
		concurrency = 1
	}
	return GatewayParityWaveFloor(concurrency) * concurrency
}

// GatewayParityUpperBoundIdentified reports whether a percentile-bootstrap
// 97.5% upper bound is statistically identified at the given wave count.
func GatewayParityUpperBoundIdentified(nWaves int) bool {
	return nWaves >= gatewayParityIdentifyingWaves
}

// RefuseGatewayParitySampleCount returns a non-empty reason when n is below the
// floor for concurrency. Callers must treat a non-empty return as a hard
// refusal — do not report the level with a caveat.
func RefuseGatewayParitySampleCount(concurrency, n int) string {
	need := GatewayParitySampleFloor(concurrency)
	waves := GatewayParityWaveFloor(concurrency)
	if n < need {
		return fmt.Sprintf(
			"refused: sample count %d at concurrency %d is below floor %d (%d waves × c); level not reported",
			n, concurrency, need, waves)
	}
	return ""
}

// RefuseGatewayParityErrorRate returns a non-empty reason when the per-level
// error rate exceeds gatewayParityMaxErrorRate. This is independent of the
// sample floor and of oversample size: reaching RequestsOK ≥ floor on survivors
// is not enough when the underlying error problem is large.
func RefuseGatewayParityErrorRate(attempted, errors int) string {
	if attempted <= 0 {
		return ""
	}
	rate := float64(errors) / float64(attempted)
	if rate > gatewayParityMaxErrorRate {
		return fmt.Sprintf(
			"refused: error rate %.1f%% (%d/%d attempted) exceeds budget %.0f%%; survival-bias cap is independent of oversample",
			rate*100, errors, attempted, gatewayParityMaxErrorRate*100)
	}
	return ""
}

// GatewayParityLadderComplete reports whether claimed concurrency levels cover
// the required PARITY_EVIDENCE ladder {1, 8, 32}.
func GatewayParityLadderComplete(claimed []int) bool {
	have := make(map[int]bool, len(claimed))
	for _, c := range claimed {
		have[c] = true
	}
	for _, need := range gatewayParityRequiredLadder {
		if !have[need] {
			return false
		}
	}
	return true
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
	// CachedTokens is usage.prompt_tokens_details.cached_tokens when the engine
	// reports it (vLLM OpenAI shape). Zero with CachedTokensReported=false means
	// no signal — never treat a missing field as a hit or a miss.
	CachedTokens         int    `json:"cached_tokens,omitempty"`
	CachedTokensReported bool   `json:"cached_tokens_reported,omitempty"`
	FinishReason         string `json:"finish_reason,omitempty"`
	RequestBytes         int    `json:"request_bytes"`
	ResponseBytes        int    `json:"response_bytes"`
	RequestBodySHA       string `json:"request_body_sha256,omitempty"`
	UpstreamBodySHA      string `json:"upstream_body_sha256,omitempty"` // merc arm when capture on
	// ContractID is X-Merc-Contract-ID from the merc control plane. A bare-SHA
	// stand-in that only echoes X-Merc-Upstream-Body-SHA256 does not set this.
	ContractID string `json:"merc_contract_id,omitempty"`
	ConnReused bool   `json:"conn_reused"`
	Err        string `json:"error,omitempty"`
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

// okTTFTByWave groups successful TTFT samples into scheduler waves.
// Wave index = sample.Index / concurrency (matching RunGatewayParityInterleavedLevel).
// When every Index is 0 (fixtures that omit it), slice position is used instead.
func okTTFTByWave(samples []GatewayParityRawSample, concurrency int) [][]float64 {
	if concurrency < 1 {
		concurrency = 1
	}
	if len(samples) == 0 {
		return nil
	}
	maxIdx := 0
	anyNonZero := false
	for _, s := range samples {
		if s.Index > maxIdx {
			maxIdx = s.Index
		}
		if s.Index != 0 {
			anyNonZero = true
		}
	}
	useIndex := anyNonZero || maxIdx > 0
	nWaves := (len(samples) + concurrency - 1) / concurrency
	if useIndex {
		nWaves = maxIdx/concurrency + 1
	}
	waves := make([][]float64, nWaves)
	for i, s := range samples {
		if s.Err != "" || s.TTFTMs <= 0 || s.FinishReason == "" {
			continue
		}
		w := i / concurrency
		if useIndex {
			w = s.Index / concurrency
		}
		if w < 0 || w >= nWaves {
			continue
		}
		waves[w] = append(waves[w], s.TTFTMs)
	}
	return waves
}

// WaveBlockTTFTShiftQ95 estimates q95(merc)−q95(direct) by jointly resampling
// wave pairs. The harness scheduler makes all c requests in a wave exchangeable
// and dependent; an independence assumption between individual requests is
// refuted by construction (every level is a handful of waves).
//
// PASS-side upper bound is max(blockDiffHi, blockMercHi−blockDirectLo) so the
// reported hi is never smaller than either the joint difference bootstrap or
// wave-block interval arithmetic — provably never PASS-loosening relative to
// either alone.
//
// Returns ok=false when either arm lacks usable waves.
func WaveBlockTTFTShiftQ95(
	mercSamples, directSamples []GatewayParityRawSample,
	concurrency, rounds int,
) (est GatewayParityPointEstimate, nWaves int, ok bool) {
	est = GatewayParityPointEstimate{Method: "wave_block_bootstrap_diff_of_q95"}
	if rounds < 1 {
		rounds = gatewayParityBootstrapRounds
	}
	mercWaves := okTTFTByWave(mercSamples, concurrency)
	dirWaves := okTTFTByWave(directSamples, concurrency)
	nAlign := len(mercWaves)
	if len(dirWaves) < nAlign {
		nAlign = len(dirWaves)
	}
	type pair struct{ m, d []float64 }
	pairs := make([]pair, 0, nAlign)
	var mercAll, dirAll []float64
	for i := 0; i < nAlign; i++ {
		if len(mercWaves[i]) == 0 || len(dirWaves[i]) == 0 {
			continue
		}
		pairs = append(pairs, pair{m: mercWaves[i], d: dirWaves[i]})
		mercAll = append(mercAll, mercWaves[i]...)
		dirAll = append(dirAll, dirWaves[i]...)
	}
	nWaves = len(pairs)
	est.N = nWaves
	if nWaves == 0 || len(mercAll) == 0 || len(dirAll) == 0 {
		return est, nWaves, false
	}
	est.Point = PercentileNearestRank(mercAll, 0.95) - PercentileNearestRank(dirAll, 0.95)
	if nWaves == 1 || rounds < 1 {
		// Single wave: no block resample is possible; degenerate at point and
		// leave identification to the caller (WaveCount < 72 → cannot PASS).
		est.CI95Low, est.CI95High = est.Point, est.Point
		return est, nWaves, true
	}

	// Deterministic LCG seeded from both arms (order-stable multiset).
	state := uint64(0x9e3779b97f4a7c15) ^ uint64(nWaves)<<40 ^ uint64(len(mercAll))<<20 ^ uint64(len(dirAll))
	if len(mercAll) > 0 {
		state ^= math.Float64bits(mercAll[0])
	}
	if len(dirAll) > 0 {
		state ^= math.Float64bits(dirAll[0]) << 1
	}
	next := func() uint64 {
		state = state*6364136223846793005 + 1
		return state
	}

	diffBoot := make([]float64, rounds)
	mercBoot := make([]float64, rounds)
	dirBoot := make([]float64, rounds)
	for r := 0; r < rounds; r++ {
		var mBoot, dBoot []float64
		for i := 0; i < nWaves; i++ {
			j := int(next() % uint64(nWaves))
			mBoot = append(mBoot, pairs[j].m...)
			dBoot = append(dBoot, pairs[j].d...)
		}
		mq := PercentileNearestRank(mBoot, 0.95)
		dq := PercentileNearestRank(dBoot, 0.95)
		mercBoot[r] = mq
		dirBoot[r] = dq
		diffBoot[r] = mq - dq
	}
	sort.Float64s(diffBoot)
	sort.Float64s(mercBoot)
	sort.Float64s(dirBoot)
	loIdx := int(0.025 * float64(rounds))
	hiIdx := int(0.975 * float64(rounds))
	if hiIdx >= rounds {
		hiIdx = rounds - 1
	}
	diffLo, diffHi := diffBoot[loIdx], diffBoot[hiIdx]
	mercLo, mercHi := mercBoot[loIdx], mercBoot[hiIdx]
	dirLo, dirHi := dirBoot[loIdx], dirBoot[hiIdx]
	// Interval-arithmetic bound on wave-block per-arm CIs.
	arithLo := mercLo - dirHi
	arithHi := mercHi - dirLo
	// PASS-side upper bound: never looser than either construction.
	hi := diffHi
	if arithHi > hi {
		hi = arithHi
	}
	// Lower bound: joint difference bootstrap (FAIL side). Taking min with
	// arithLo would only make FAIL harder / CI wider — also non-loosening for PASS.
	lo := diffLo
	if arithLo < lo {
		lo = arithLo
	}
	if hi < lo {
		lo, hi = hi, lo
	}
	est.CI95Low, est.CI95High = lo, hi
	est.Method = "wave_block_bootstrap_diff_of_q95; pass_hi=max(diff_hi, merc_hi-direct_lo)"
	return est, nWaves, true
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

// GatewayParityBudget is the TTFT-shift ceiling applied at EVERY claimed level.
type GatewayParityBudget struct {
	// TTFTShiftQ95BudgetMs is the maximum allowed q95(merc)−q95(direct) shift.
	// Name is "shift" not "overhead": the estimator is a difference of
	// percentiles (a shift function), not a percentile of paired differences.
	TTFTShiftQ95BudgetMs    float64 `json:"ttft_shift_q95_ms"`
	ThroughputLossFraction  float64 `json:"throughput_loss_fraction"`
	RequireMeasuredEveryLvl bool    `json:"require_measured_at_every_level"`
	// PrimaryMetric names the metric that alone can set the level verdict.
	// Throughput is secondary and never dual-refuses the same underlying event.
	PrimaryMetric string `json:"primary_metric"`
	Basis         string `json:"basis"`
}

// DefaultGatewayParityBudget is a policy ceiling, not a derived SLA. Calibrated
// with headroom against a clean measurement; not against the invalidated
// gateway-parity.py receipt.
func DefaultGatewayParityBudget() GatewayParityBudget {
	return GatewayParityBudget{
		TTFTShiftQ95BudgetMs:    15.0,
		ThroughputLossFraction:  0.05,
		RequireMeasuredEveryLvl: true,
		PrimaryMetric:           "ttft_shift_q95_ms",
		Basis: "primary=ttft_shift_q95_ms (q95(merc)−q95(direct)) wave-block bootstrap interval at every claimed level; " +
			"MDE (half-width of shift CI) must not exceed TTFT budget or the level is INCONCLUSIVE; " +
			"PASS requires an identified 97.5% upper bound (wave_count≥72) — non-identified UBs cannot PASS; " +
			"per-level error rate must not exceed 5% (survival-bias cap, independent of oversample); " +
			"throughput_loss_fraction is secondary and is not dual-refused when primary already FAIL; " +
			"verdict is PASS/FAIL/INCONCLUSIVE from bootstrap CIs (not point-estimate coin flips)",
	}
}

// GatewayParityGateLevel is the budget evaluation at one concurrency.
type GatewayParityGateLevel struct {
	Concurrency   int      `json:"concurrency"`
	Verdict       string   `json:"verdict"` // PASS | FAIL | INCONCLUSIVE
	Passed        bool     `json:"passed"`  // true only when Verdict == PASS
	Refusals      []string `json:"refusals"`
	PrimaryMetric string   `json:"primary_metric"`

	// TTFT shift q95 (q95(merc) − q95(direct)) with CI used for the interval decision.
	TTFTShiftQ95BudgetMs *GatewayParityPointEstimate `json:"ttft_shift_q95_ms,omitempty"`
	TTFTBudgetMs         float64                     `json:"ttft_shift_q95_budget_ms,omitempty"`
	// MinimumDetectableEffectMs is the half-width of the shift CI: the
	// smallest true shift this run can reliably distinguish from the noise
	// band. Compare to TTFTBudgetMs — if MDE > budget the gate is under-powered.
	MinimumDetectableEffectMs float64 `json:"minimum_detectable_effect_ms,omitempty"`
	// WaveCount is the number of scheduler waves (index/concurrency) that
	// contributed OK TTFT samples on both arms.
	WaveCount int `json:"wave_count,omitempty"`
	// UpperBoundIdentified is true when WaveCount ≥ 72 so the bootstrap 97.5%
	// upper bound is not catastrophically coverage-deficient.
	UpperBoundIdentified bool `json:"ttft_shift_q95_upper_bound_identified"`

	// Throughput is reported every time; Refused is true only when the primary
	// did not already FAIL and the secondary budget is breached.
	ThroughputLossFraction *float64 `json:"throughput_loss_fraction,omitempty"`
	ThroughputBudget       float64  `json:"throughput_loss_budget,omitempty"`
	ThroughputSecondary    string   `json:"throughput_secondary_note,omitempty"`
}

// GatewayParityGate is the multi-level gate with interval verdicts.
// GatePassed is true only when every claimed level is PASS (not INCONCLUSIVE).
type GatewayParityGate struct {
	Version       string                   `json:"version"`
	GatePassed    bool                     `json:"gate_passed"`
	Verdict       string                   `json:"verdict"` // PASS | FAIL | INCONCLUSIVE
	PrimaryMetric string                   `json:"primary_metric"`
	Levels        []GatewayParityGateLevel `json:"levels"`
	Budget        GatewayParityBudget      `json:"budget"`
	Basis         string                   `json:"basis"`
	ClaimedLevels []int                    `json:"claimed_concurrency_levels"`
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
	// EvidenceClass: PARITY_EVIDENCE (catalogue vLLM claim), LOCAL_METAL_PARITY
	// (gateway overhead vs llama.cpp/Metal only — not catalogue parity),
	// HARNESS_SELF_TEST, INCOMPLETE_LADDER, INVALIDATED.
	EvidenceClass string `json:"evidence_class"`

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
	// ColdWarmState is the legacy single-shape label. When Cells is non-empty,
	// per-cell state lives on each cell's state_proof; this field is "per-cell"
	// and must not be quoted as a blanket claim.
	ColdWarmState string `json:"cold_warm_state"`

	ClaimedLevels []int                               `json:"claimed_concurrency_levels"`
	Levels        map[string]GatewayParityLevelResult `json:"levels"` // key: "merc@c=N" / "direct@c=N" (single-shape)
	// BodyIdentity records what was proven about on-the-wire equality.
	BodyIdentity GatewayParityBodyIdentity `json:"body_identity"`

	// Matrix dimensions (prompt × output × state × concurrency). Empty Cells
	// means legacy single-shape mode. When non-empty, each cell is gated and
	// refused independently; a claim about one cell is not a claim about another.
	FullAxes       *GatewayParityAxes         `json:"full_axes,omitempty"`
	SelectedCells  []GatewayParityCellSpec    `json:"selected_cells,omitempty"`
	DroppedAxes    []GatewayParityDroppedAxis `json:"dropped_axes,omitempty"`
	DroppedSummary []string                   `json:"dropped_summary,omitempty"`
	Cells          []GatewayParityCellResult  `json:"cells,omitempty"`
	// TrafficClassNote records why traffic class is not an axis of this harness.
	TrafficClassNote string `json:"traffic_class_note,omitempty"`
	// StateDefinitions are the precise operational meanings of cold/warm/prefix-hit.
	StateDefinitions *GatewayParityStateDefinitions `json:"state_definitions,omitempty"`
	// ShapeInsight is a measured (not assumed) summary of where fixed overhead
	// is largest as a fraction of request cost across selected cells.
	ShapeInsight *GatewayParityShapeInsight `json:"shape_insight,omitempty"`

	// Gate evaluates every claimed level with an interval verdict so a claim
	// that skips a level, exceeds budget, or is under-powered cannot pass.
	// In matrix mode Gate aggregates per-cell verdicts (FAIL > INCONCLUSIVE > PASS).
	Gate   GatewayParityGate   `json:"gate"`
	Budget GatewayParityBudget `json:"budget"`

	// RawSamples flattened for a skeptic who does not want to dig into levels.
	// Optional duplicate; levels already carry raw_samples. Kept so a single
	// top-level array is always present when the run measured anything.
	RawSampleCount int `json:"raw_sample_count"`

	// DoesNotProve lists claims this receipt must never be quoted as. Required
	// for LOCAL_METAL_PARITY so a reader who quotes "Merc matches the engine"
	// (catalogue vLLM) is wrong on the face of the artifact.
	DoesNotProve []string `json:"does_not_prove,omitempty"`

	Notes []string `json:"notes,omitempty"`
}

// localMetalParityDoesNotProve is the fixed non-claim list for Metal/llama.cpp
// gateway-overhead receipts. Copied into every LOCAL_METAL_PARITY receipt.
var localMetalParityDoesNotProve = []string{
	"that Merc matches the advertised catalogue runtime profile (engine=vllm, dtype=bfloat16, container_platform=linux/amd64)",
	"vLLM parity, vLLM overhead, or any NVIDIA/CUDA/linux-amd64 serving measurement",
	"bf16 / full-precision weight parity with any published vLLM profile",
	"production multi-tenant capacity, pricing correctness, or catalogue SLA",
	"that a reader may re-label this receipt as evidence_class=PARITY_EVIDENCE",
}

// GatewayParityBodyIdentity is the proof (or argument) that both arms sent the
// same request through a real Merc control plane.
type GatewayParityBodyIdentity struct {
	Proven              bool   `json:"proven"`
	Method              string `json:"method"`
	HarnessBodySHA256   string `json:"harness_body_sha256"`
	MercUpstreamSHA256  string `json:"merc_upstream_sha256,omitempty"`
	DirectRequestSHA256 string `json:"direct_request_sha256,omitempty"`
	Equal               bool   `json:"bodies_equal"`
	Detail              string `json:"detail,omitempty"`
	// Contract proof counters. Proven requires every OK merc sample to carry a
	// valid X-Merc-Contract-ID as well as a matching upstream body SHA.
	OKSamplesExamined   int  `json:"ok_samples_examined,omitempty"`
	ContractIDsObserved int  `json:"contract_ids_observed,omitempty"`
	BareSHAStandIn      bool `json:"bare_sha_standin_refused,omitempty"`
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
	// net/http only replays a request onto a fresh connection when
	// Request.isReplayable() is true, which for a POST requires this header.
	// Without it, a keep-alive connection the server closed while idle surfaces
	// as a sample error instead of a retry — and because levels sample near
	// their floor, one such error drops the level below the floor and REFUSES
	// it. The body is already replayable: bytes.NewReader gives GetBody.
	// The key is per attempt, so a transport-level replay reuses the same key
	// while two distinct measurement samples never share one.
	req.Header.Set("Idempotency-Key", uuid.NewString())

	var reused bool
	var conn string
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			reused = info.Reused
			if info.Conn != nil {
				// Key per-connection counts on the LOCAL address. The remote
				// address is the same for every sample against one upstream, so
				// keying on it collapses every request into a single bucket and
				// makes connection reuse unmeasurable.
				conn = info.Conn.LocalAddr().String()
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
	c.noteConn(reused, conn)
	if sha := resp.Header.Get("X-Merc-Upstream-Body-SHA256"); sha != "" {
		sample.UpstreamBodySHA = sha
	}
	// Merc-only authorization header. Issued when the control plane creates an
	// execution contract; a bare body-SHA echo stand-in does not possess it.
	if cid := strings.TrimSpace(resp.Header.Get("X-Merc-Contract-ID")); cid != "" {
		sample.ContractID = cid
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
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokens        int `json:"prompt_tokens"`
				PromptTokensDetails *struct {
					CachedTokens *int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
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
			if chunk.Usage.PromptTokensDetails != nil && chunk.Usage.PromptTokensDetails.CachedTokens != nil {
				sample.CachedTokens = *chunk.Usage.PromptTokensDetails.CachedTokens
				sample.CachedTokensReported = true
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

// finalizeGatewayParitySamples turns raw samples + per-arm wall into a level
// result. An OK sample requires a defined TTFT (>0) and a finish_reason — a
// 2xx with no content delta is an error, not a silent TTFT exclusion.
func finalizeGatewayParitySamples(
	arm string,
	concurrency, n int,
	samples []GatewayParityRawSample,
	wallSeconds float64,
	meanInFlight float64,
	peakInFlight, inFlightSamples int,
) GatewayParityLevelResult {
	r := GatewayParityLevelResult{
		Arm: arm, Concurrency: concurrency, RequestsAttempted: n,
		Status: "MEASURED", WallSeconds: wallSeconds, FinishReasons: map[string]int{},
		MeanInFlight: meanInFlight, PeakInFlight: peakInFlight, InFlightSamples: inFlightSamples,
		RawSamples: append([]GatewayParityRawSample(nil), samples...),
	}
	var (
		ttfts      []float64
		totalTok   int
		ok         int
		errSamples []string
	)
	for _, s := range samples {
		if s.Err != "" {
			r.Errors++
			if len(errSamples) < 5 {
				errSamples = append(errSamples, s.Err)
			}
			continue
		}
		// Real completion: content TTFT and a terminal finish_reason required.
		if s.TTFTMs <= 0 || s.FinishReason == "" {
			r.Errors++
			msg := "incomplete response: require ttft_ms>0 and finish_reason on every OK sample"
			if s.TTFTMs <= 0 && s.FinishReason == "" {
				msg = "incomplete response: missing ttft_ms and finish_reason"
			} else if s.TTFTMs <= 0 {
				msg = "incomplete response: missing ttft_ms (no content token)"
			} else {
				msg = "incomplete response: missing finish_reason"
			}
			if len(errSamples) < 5 {
				errSamples = append(errSamples, msg)
			}
			continue
		}
		ok++
		ttfts = append(ttfts, s.TTFTMs)
		totalTok += s.CompletionTokens
		r.FinishReasons[s.FinishReason]++
	}
	r.RequestsOK = ok
	r.ErrorSamples = errSamples
	r.TotalTokens = totalTok
	if ok > 0 {
		r.MeanCompletionTok = float64(totalTok) / float64(ok)
		// Per-arm wall is the denominator — never a shared dual-arm wall.
		if wallSeconds > 0 {
			tps := float64(totalTok) / wallSeconds
			rps := float64(ok) / wallSeconds
			r.AggregateTokPerSec = &tps
			r.AggregateReqPerSec = &rps
		}
	}

	// Observed peak must not exceed the claimed concurrency level.
	if peakInFlight > concurrency {
		r.Status = "REFUSED"
		r.Reason = fmt.Sprintf(
			"refused: observed peak in-flight %d exceeds claimed concurrency %d",
			peakInFlight, concurrency)
		return r
	}
	// Steady-state in-flight check (FIFO-join defect class).
	if meanInFlight < float64(concurrency)*gatewayParityMinInFlightFraction {
		r.Status = "REFUSED"
		r.Reason = fmt.Sprintf(
			"refused: mean in-flight %.2f is below %.0f%% of nominal concurrency %d; level did not reach steady state",
			meanInFlight, gatewayParityMinInFlightFraction*100, concurrency)
		return r
	}
	if reason := RefuseGatewayParitySampleCount(concurrency, ok); reason != "" {
		r.Status = "REFUSED"
		r.Reason = reason
		return r
	}
	// Survival-bias cap: independent of the OK floor and of how far n sits
	// above it. A run that burns most attempts on errors and keeps only the
	// lucky survivors must not PASS.
	if reason := RefuseGatewayParityErrorRate(n, r.Errors); reason != "" {
		r.Status = "REFUSED"
		r.Reason = reason
		return r
	}
	// Nil TTFT percentile is a refusal, never a skipped budget check.
	if len(ttfts) == 0 {
		r.Status = "REFUSED"
		r.Reason = "refused: no OK samples with defined TTFT; cannot evaluate overhead budget"
		return r
	}
	p50 := BootstrapPercentileCI(ttfts, 0.50, 1000)
	p95 := BootstrapPercentileCI(ttfts, 0.95, 1000)
	p99 := BootstrapPercentileCI(ttfts, 0.99, 1000)
	mean := MeanWithCI(ttfts)
	r.TTFTp50, r.TTFTp95, r.TTFTp99, r.TTFTMean = &p50, &p95, &p99, &mean
	return r
}

// RunGatewayParityLevel drives n requests for one arm at concurrency c using a
// correct bounded pool (semaphore), NOT a FIFO join of the oldest-started
// thread. Samples true in-flight concurrency during the run. Engine in-flight
// is hard-capped at c.
func RunGatewayParityLevel(
	ctx context.Context,
	client *GatewayParityClient,
	baseURL, apiKey, arm string,
	body []byte,
	concurrency, n int,
) GatewayParityLevelResult {
	if reason := RefuseGatewayParitySampleCount(concurrency, n); reason != "" {
		return GatewayParityLevelResult{
			Arm: arm, Concurrency: concurrency, RequestsAttempted: n,
			Status: "REFUSED", Reason: reason,
		}
	}
	if concurrency < 1 {
		return GatewayParityLevelResult{
			Arm: arm, Concurrency: concurrency, RequestsAttempted: n,
			Status: "REFUSED", Reason: "refused: concurrency < 1",
		}
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
	// Mean is accumulated only while at least one request is in flight so
	// inter-wave idle does not dilute the steady-state fraction.
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
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				if cur > 0 {
					sumInFlight.Add(cur)
					inFlightN.Add(1)
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

	mean := 0.0
	nObs := int(inFlightN.Load())
	if nObs > 0 {
		mean = float64(sumInFlight.Load()) / float64(nObs)
	}
	return finalizeGatewayParitySamples(
		arm, concurrency, n, samples, wall, mean, int(peak.Load()), nObs,
	)
}

// RunGatewayParityInterleavedLevel runs merc and direct at one concurrency
// using alternating batches: each wave runs one arm to completion at cap c,
// then the other. The engine never sees more than c concurrent sequences —
// claimed concurrency c means c in flight at the engine, not 2c. Who leads
// flips every wave so thermal drift is shared. Each arm records its own wall
// clock (busy time) so aggregate tokens/sec is an independent measurement.
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

	mercSamples := make([]GatewayParityRawSample, n)
	directSamples := make([]GatewayParityRawSample, n)
	var (
		mercInFlight   atomic.Int64
		directInFlight atomic.Int64
		peakMerc       atomic.Int64
		peakDirect     atomic.Int64
		sumMerc        atomic.Int64
		nMerc          atomic.Int64
		sumDirect      atomic.Int64
		nDirect        atomic.Int64
		// Engine-global in-flight must never exceed c.
		engineInFlight atomic.Int64
		peakEngine     atomic.Int64
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
				g := engineInFlight.Load()
				for {
					p := peakEngine.Load()
					if g <= p || peakEngine.CompareAndSwap(p, g) {
						break
					}
				}
				m := mercInFlight.Load()
				for {
					p := peakMerc.Load()
					if m <= p || peakMerc.CompareAndSwap(p, m) {
						break
					}
				}
				if m > 0 {
					sumMerc.Add(m)
					nMerc.Add(1)
				}
				d := directInFlight.Load()
				for {
					p := peakDirect.Load()
					if d <= p || peakDirect.CompareAndSwap(p, d) {
						break
					}
				}
				if d > 0 {
					sumDirect.Add(d)
					nDirect.Add(1)
				}
			}
		}
	}()

	// Run one arm for indices [lo, hi) at concurrency cap. Returns wave wall seconds.
	runWave := func(arm string, lo, hi int) float64 {
		if lo >= hi {
			return 0
		}
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		url, key := directURL, directKey
		armIn := &directInFlight
		if arm == "merc" {
			url, key = mercURL, mercKey
			armIn = &mercInFlight
		}
		wallStart := time.Now()
		for i := lo; i < hi; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				armIn.Add(1)
				engineInFlight.Add(1)
				sample := client.CompleteOneStream(ctx, url, key, body, arm, i)
				engineInFlight.Add(-1)
				armIn.Add(-1)
				<-sem
				if arm == "merc" {
					mercSamples[i] = sample
				} else {
					directSamples[i] = sample
				}
			}(i)
		}
		wg.Wait()
		return time.Since(wallStart).Seconds()
	}

	var mercWall, directWall float64
	// Alternating batches of size `concurrency` (last batch may be smaller).
	// Global engine concurrency is at most c because only one arm runs at a time.
	for offset, waveIdx := 0, 0; offset < n; waveIdx++ {
		hi := offset + concurrency
		if hi > n {
			hi = n
		}
		// Flip who leads every wave.
		if waveIdx%2 == 0 {
			mercWall += runWave("merc", offset, hi)
			directWall += runWave("direct", offset, hi)
		} else {
			directWall += runWave("direct", offset, hi)
			mercWall += runWave("merc", offset, hi)
		}
		offset = hi
	}
	close(sampleDone)

	// Hard refuse if the engine saw more than claimed c (implementation bug).
	if int(peakEngine.Load()) > concurrency {
		reason := fmt.Sprintf(
			"refused: engine peak in-flight %d exceeds claimed concurrency %d (dual-load bug)",
			int(peakEngine.Load()), concurrency)
		merc = GatewayParityLevelResult{Arm: "merc", Concurrency: concurrency, RequestsAttempted: n, Status: "REFUSED", Reason: reason, PeakInFlight: int(peakMerc.Load()), RawSamples: mercSamples}
		direct = GatewayParityLevelResult{Arm: "direct", Concurrency: concurrency, RequestsAttempted: n, Status: "REFUSED", Reason: reason, PeakInFlight: int(peakDirect.Load()), RawSamples: directSamples}
		return merc, direct
	}

	meanMerc, nObsM := 0.0, int(nMerc.Load())
	if nObsM > 0 {
		meanMerc = float64(sumMerc.Load()) / float64(nObsM)
	}
	meanDirect, nObsD := 0.0, int(nDirect.Load())
	if nObsD > 0 {
		meanDirect = float64(sumDirect.Load()) / float64(nObsD)
	}
	merc = finalizeGatewayParitySamples("merc", concurrency, n, mercSamples, mercWall, meanMerc, int(peakMerc.Load()), nObsM)
	direct = finalizeGatewayParitySamples("direct", concurrency, n, directSamples, directWall, meanDirect, int(peakDirect.Load()), nObsD)
	return merc, direct
}

// pointEstimateBounds returns (lo, hi, point) for interval decisions.
// Unset CI bounds (both zero while point is non-zero) are a refusal, not a
// zero-width interval: collapsing them to the point would set MDE=0, disarm the
// under-powered check, and let ohHi<=budget PASS on a bare point estimate —
// exactly the coin flip the budget basis forbids.
func pointEstimateBounds(est *GatewayParityPointEstimate) (lo, hi, point float64, ok bool) {
	if est == nil || math.IsNaN(est.Point) {
		return 0, 0, 0, false
	}
	point = est.Point
	lo, hi = est.CI95Low, est.CI95High
	// Unset CI (both zero while point is non-zero) → not ok.
	if lo == 0 && hi == 0 && point != 0 {
		return 0, 0, point, false
	}
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo, hi, point, true
}

// EvaluateGatewayParityGate checks every claimed concurrency level.
// Primary metric is TTFT shift q95 (q95(merc)−q95(direct)) with an interval verdict:
//
//	PASS         — entire shift CI is at or below the budget, MDE ≤ budget,
//	               and the 97.5% upper bound is identified (wave_count ≥ 72)
//	FAIL         — entire shift CI is above the budget
//	INCONCLUSIVE — CI straddles the budget, under-powered, or UB not identified
//
// When both arms carry raw samples, the CI is a joint wave-block bootstrap of
// the difference (wave = index/concurrency). PASS-side hi is
// max(blockDiffHi, blockMercHi−blockDirectLo) so the change is never
// PASS-loosening relative to either construction. Fixtures without raw
// samples fall back to interval arithmetic on precomputed per-arm CIs.
//
// Throughput is secondary: reported always, refused only when the primary did
// not already FAIL (never dual-refuse one underlying latency event).
// A nil TTFT percentile is FAIL, never a skipped check.
func EvaluateGatewayParityGate(
	claimedLevels []int,
	levels map[string]GatewayParityLevelResult,
	budget GatewayParityBudget,
) GatewayParityGate {
	primary := budget.PrimaryMetric
	if primary == "" {
		primary = "ttft_shift_q95_ms"
	}
	gate := GatewayParityGate{
		Version:       gatewayParityGateVersion,
		PrimaryMetric: primary,
		Budget:        budget,
		Basis:         budget.Basis,
		ClaimedLevels: append([]int(nil), claimedLevels...),
		Levels:        make([]GatewayParityGateLevel, 0, len(claimedLevels)),
	}

	overall := "PASS"
	allPassed := true
	for _, c := range claimedLevels {
		level := GatewayParityGateLevel{
			Concurrency:      c,
			PrimaryMetric:    primary,
			TTFTBudgetMs:     budget.TTFTShiftQ95BudgetMs,
			ThroughputBudget: budget.ThroughputLossFraction,
			Verdict:          "PASS",
			Passed:           true,
		}
		merc := levels[fmt.Sprintf("merc@c=%d", c)]
		direct := levels[fmt.Sprintf("direct@c=%d", c)]
		var refusals []string

		if merc.Status != "MEASURED" {
			refusals = append(refusals, fmt.Sprintf("concurrency %d merc: %s", c, orDefault(merc.Reason, merc.Status)))
		}
		if direct.Status != "MEASURED" {
			refusals = append(refusals, fmt.Sprintf("concurrency %d direct: %s", c, orDefault(direct.Reason, direct.Status)))
		}
		// Peak-in-flight must not exceed claimed c (recorded even when MEASURED).
		if merc.Status == "MEASURED" && merc.PeakInFlight > c {
			refusals = append(refusals, fmt.Sprintf(
				"concurrency %d merc: peak in-flight %d exceeds claimed %d", c, merc.PeakInFlight, c))
		}
		if direct.Status == "MEASURED" && direct.PeakInFlight > c {
			refusals = append(refusals, fmt.Sprintf(
				"concurrency %d direct: peak in-flight %d exceeds claimed %d", c, direct.PeakInFlight, c))
		}

		primaryFailed := false
		if merc.Status == "MEASURED" && direct.Status == "MEASURED" && len(refusals) == 0 {
			// Output-contract equality.
			if math.Abs(merc.MeanCompletionTok-direct.MeanCompletionTok) > 0.5 {
				if merc.TotalTokens != direct.TotalTokens {
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: output contract differs; merc_completion_tokens=%d direct_completion_tokens=%d",
						c, merc.TotalTokens, direct.TotalTokens))
				}
			}

			// --- Primary: TTFT shift q95 interval decision ---
			var (
				ohLo, ohHi, ohPt float64
				oh               GatewayParityPointEstimate
				haveShift        bool
			)
			// Prefer joint wave-block bootstrap when both arms carry raw samples.
			if len(merc.RawSamples) > 0 && len(direct.RawSamples) > 0 {
				block, nWaves, blockOK := WaveBlockTTFTShiftQ95(
					merc.RawSamples, direct.RawSamples, c, gatewayParityBootstrapRounds)
				if blockOK {
					oh = block
					ohLo, ohHi, ohPt = block.CI95Low, block.CI95High, block.Point
					level.WaveCount = nWaves
					level.UpperBoundIdentified = GatewayParityUpperBoundIdentified(nWaves)
					haveShift = true
				}
			}
			if !haveShift {
				// Fallback: interval arithmetic on precomputed per-arm CIs
				// (unit-test fixtures without raw samples).
				mercLo, mercHi, mercPt, mercOK := pointEstimateBounds(merc.TTFTp95)
				dirLo, dirHi, dirPt, dirOK := pointEstimateBounds(direct.TTFTp95)
				if !mercOK || !dirOK {
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: ttft_p95 missing or CI unset on merc or direct; nil/unset percentile is a refusal, not a skipped check", c))
					primaryFailed = true
					level.Verdict = "FAIL"
				} else {
					ohLo = mercLo - dirHi
					ohHi = mercHi - dirLo
					ohPt = mercPt - dirPt
					if ohHi < ohLo {
						ohLo, ohHi = ohHi, ohLo
					}
					oh = GatewayParityPointEstimate{
						Point:    ohPt,
						CI95Low:  ohLo,
						CI95High: ohHi,
						Method:   "difference_of_bootstrap_percentile_ci",
						N:        merc.TTFTp95.N,
					}
					if direct.TTFTp95.N < oh.N {
						oh.N = direct.TTFTp95.N
					}
					// Fixtures without waves: do not claim identification.
					level.UpperBoundIdentified = false
					haveShift = true
				}
			}
			if haveShift && budget.TTFTShiftQ95BudgetMs > 0 {
				level.TTFTShiftQ95BudgetMs = &oh
				// MDE = half-width of the shift CI: the smallest true shift
				// this run can reliably distinguish from noise.
				level.MinimumDetectableEffectMs = (ohHi - ohLo) / 2
				// Statistical power is a hard gate, not decoration. If MDE
				// exceeds the budget the run cannot detect a violation at the
				// threshold it polices — so it must not PASS even when the
				// point estimate looks comfortable. A clear FAIL (CI entirely
				// above budget) still fails: under-power does not excuse a
				// measured breach.
				underpowered := level.MinimumDetectableEffectMs > budget.TTFTShiftQ95BudgetMs
				// Non-identified upper bound: bootstrap UB is capped at the
				// sample max with P(true q95 > max) = 0.95^W ≫ 0.025. Cannot
				// PASS; FAIL remains valid when even the lo exceeds budget.
				// Only enforced when waves were measured from raw samples
				// (WaveCount > 0). Fixture paths without raw samples leave
				// WaveCount=0 and exercise decision logic on injected CIs.
				ubNotIdentified := level.WaveCount > 0 && !level.UpperBoundIdentified

				switch {
				case ohLo > budget.TTFTShiftQ95BudgetMs:
					// Entire CI above budget → FAIL (safe even when UB not identified).
					level.Verdict = "FAIL"
					primaryFailed = true
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: ttft shift q95 CI [%.3f, %.3f] ms (point %.3f) entirely above budget %.3f ms (MDE=%.3f ms, waves=%d, ub_identified=%v)",
						c, ohLo, ohHi, ohPt, budget.TTFTShiftQ95BudgetMs, level.MinimumDetectableEffectMs, level.WaveCount, level.UpperBoundIdentified))
				case underpowered:
					// Cannot resolve the budget threshold → INCONCLUSIVE.
					level.Verdict = "INCONCLUSIVE"
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: under-powered: MDE=%.3f ms exceeds TTFT budget %.3f ms (shift CI [%.3f, %.3f] ms, point %.3f, waves=%d); gate cannot detect a violation at the policed threshold",
						c, level.MinimumDetectableEffectMs, budget.TTFTShiftQ95BudgetMs, ohLo, ohHi, ohPt, level.WaveCount))
				case ubNotIdentified && ohHi <= budget.TTFTShiftQ95BudgetMs:
					// Would be PASS on the numbers, but the 97.5% upper bound
					// is not identified — refusing to pretend it is.
					level.Verdict = "INCONCLUSIVE"
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: ttft shift q95 bootstrap upper bound not identified (need %d independent waves for 97.5%% UB, have %d; 0.95^W=%.4f); shift CI [%.3f, %.3f] ms point %.3f cannot support PASS",
						c, gatewayParityIdentifyingWaves, level.WaveCount, math.Pow(0.95, float64(level.WaveCount)), ohLo, ohHi, ohPt))
				case ohHi <= budget.TTFTShiftQ95BudgetMs:
					// Entire CI at or below budget AND MDE ≤ budget AND UB identified → PASS.
					level.Verdict = "PASS"
				default:
					// CI straddles budget → honest INCONCLUSIVE.
					level.Verdict = "INCONCLUSIVE"
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: ttft shift q95 CI [%.3f, %.3f] ms (point %.3f) straddles budget %.3f ms (MDE=%.3f ms, waves=%d); run cannot decide",
						c, ohLo, ohHi, ohPt, budget.TTFTShiftQ95BudgetMs, level.MinimumDetectableEffectMs, level.WaveCount))
				}
			}

			// --- Secondary: throughput (per-arm wall denominator) ---
			if merc.AggregateTokPerSec != nil && direct.AggregateTokPerSec != nil &&
				*direct.AggregateTokPerSec > 0 && budget.ThroughputLossFraction > 0 {
				loss := (*direct.AggregateTokPerSec - *merc.AggregateTokPerSec) / *direct.AggregateTokPerSec
				level.ThroughputLossFraction = &loss
				if primaryFailed {
					// Same underlying latency event already failed primary —
					// report but do not dual-refuse.
					level.ThroughputSecondary = fmt.Sprintf(
						"secondary: loss=%.4f budget=%.4f; not dual-refused because primary TTFT already FAIL",
						loss, budget.ThroughputLossFraction)
				} else if loss > budget.ThroughputLossFraction {
					level.ThroughputSecondary = fmt.Sprintf(
						"secondary FAIL: loss=%.4f exceeds budget=%.4f (merc=%.2f direct=%.2f tok/s, per-arm walls)",
						loss, budget.ThroughputLossFraction, *merc.AggregateTokPerSec, *direct.AggregateTokPerSec)
					// Secondary alone can fail the level only when primary did not.
					if level.Verdict == "PASS" {
						level.Verdict = "FAIL"
					}
					refusals = append(refusals, fmt.Sprintf(
						"concurrency %d: throughput loss fraction %.4f exceeds budget %.4f (merc=%.2f direct=%.2f tok/s; secondary metric, per-arm wall)",
						c, loss, budget.ThroughputLossFraction, *merc.AggregateTokPerSec, *direct.AggregateTokPerSec))
				} else {
					level.ThroughputSecondary = fmt.Sprintf(
						"secondary PASS: loss=%.4f within budget=%.4f (per-arm walls)",
						loss, budget.ThroughputLossFraction)
				}
			}
		}

		if len(refusals) > 0 && level.Verdict == "PASS" {
			// Structural refusals (not measured, peak exceeded, output drift).
			level.Verdict = "FAIL"
		}
		level.Refusals = refusals
		level.Passed = level.Verdict == "PASS" && len(refusals) == 0
		if !level.Passed {
			allPassed = false
		}
		// Overall verdict precedence: FAIL > INCONCLUSIVE > PASS.
		switch {
		case level.Verdict == "FAIL":
			overall = "FAIL"
		case level.Verdict == "INCONCLUSIVE" && overall == "PASS":
			overall = "INCONCLUSIVE"
		}
		gate.Levels = append(gate.Levels, level)
	}
	// RequireMeasuredEveryLvl: empty claimed set is not a pass.
	if budget.RequireMeasuredEveryLvl && len(claimedLevels) == 0 {
		allPassed = false
		overall = "FAIL"
	}
	gate.GatePassed = allPassed && overall == "PASS"
	gate.Verdict = overall
	if !gate.GatePassed && overall == "PASS" {
		gate.Verdict = "FAIL"
	}
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

// ProveGatewayParityBodyIdentity derives body identity from merc samples.
// Equal defaults to false and is raised only when evidence exists.
//
// For a proven claim, EVERY OK merc sample at every claimed level must carry:
//
//  1. X-Merc-Upstream-Body-SHA256 equal to the harness body SHA, and
//  2. X-Merc-Contract-ID — a valid UUID that Merc sets only after authorizing
//     an execution contract (buyer auth, funding, contract insert).
//
// A bare-SHA stand-in (echo server that hashes the request body and reflects
// it as X-Merc-Upstream-Body-SHA256) is explicitly refused: matching SHA
// without Contract-ID is not proof. Why Contract-ID is not free for an echo
// server to possess honestly: Merc emits it only on the realtime authorization
// path after a durable execution_contracts row exists. A pure body-echo
// stand-in has no contract table and no authorized UUID; it can forge a random
// UUID header only by impersonating Merc wholesale, which is a different
// residual threat than the bare-SHA attack this closes. The self-test stand-in
// deliberately omits Contract-ID so it cannot satisfy this proof.
func ProveGatewayParityBodyIdentity(
	body []byte,
	claimed []int,
	levels map[string]GatewayParityLevelResult,
) GatewayParityBodyIdentity {
	harness := sha256Hex(body)
	id := GatewayParityBodyIdentity{
		Proven: false,
		Equal:  false, // never default true
		Method: "requires X-Merc-Upstream-Body-SHA256 matching harness body sha256 " +
			"AND valid X-Merc-Contract-ID on every OK merc sample at every claimed level",
		HarnessBodySHA256:   harness,
		DirectRequestSHA256: harness,
		Detail:              "unproven: no matching upstream body sha + contract id observed",
	}
	if len(claimed) == 0 {
		id.Detail = "unproven: no claimed levels"
		return id
	}
	var (
		okExamined      int
		withSHA         int
		withContract    int
		withBoth        int
		sawBareSHA      bool
		missingAnyLevel bool
	)
	for _, c := range claimed {
		merc := levels[fmt.Sprintf("merc@c=%d", c)]
		levelOKCount := 0
		levelProvenCount := 0
		for _, s := range merc.RawSamples {
			if s.Err != "" || s.TTFTMs <= 0 || s.FinishReason == "" {
				continue
			}
			okExamined++
			levelOKCount++
			shaOK := s.UpstreamBodySHA != ""
			if shaOK {
				withSHA++
				id.MercUpstreamSHA256 = s.UpstreamBodySHA
				if s.UpstreamBodySHA != harness {
					id.Equal = false
					id.Proven = false
					id.OKSamplesExamined = okExamined
					id.Detail = fmt.Sprintf(
						"merc upstream sha %s != harness body sha %s at c=%d",
						s.UpstreamBodySHA, harness, c)
					return id
				}
			}
			cid := strings.TrimSpace(s.ContractID)
			contractOK := false
			if cid != "" {
				if _, err := uuid.Parse(cid); err == nil {
					contractOK = true
					withContract++
				}
			}
			if shaOK && !contractOK {
				// Matching (or present) SHA without a Merc contract id: the
				// bare-SHA stand-in path. Refuse explicitly.
				sawBareSHA = true
			}
			if shaOK && contractOK {
				withBoth++
				levelProvenCount++
			}
		}
		// Every OK sample at the level must be fully proven — not "at least one".
		if levelOKCount == 0 || levelProvenCount < levelOKCount {
			missingAnyLevel = true
		}
	}
	id.OKSamplesExamined = okExamined
	id.ContractIDsObserved = withContract
	if okExamined == 0 {
		id.Detail = "unproven: no OK merc samples to examine"
		return id
	}
	if sawBareSHA && withContract == 0 {
		// SHA may match (bodies equal) but a body-echo stand-in is not Merc.
		id.BareSHAStandIn = true
		id.Equal = withSHA == okExamined && withSHA > 0
		id.Proven = false
		id.Detail = "bare-SHA stand-in refused: X-Merc-Upstream-Body-SHA256 observed without X-Merc-Contract-ID; " +
			"PARITY_EVIDENCE requires a Merc-authorized contract id on every OK merc sample, not a body-echo SHA alone"
		return id
	}
	if sawBareSHA {
		id.BareSHAStandIn = true
		id.Equal = false // incomplete contract coverage cannot raise Equal
		id.Proven = false
		id.Detail = fmt.Sprintf(
			"bare-SHA stand-in refused: %d OK merc sample(s) carry upstream SHA without a valid X-Merc-Contract-ID "+
				"(examined=%d sha=%d contract=%d both=%d)",
			withSHA-withBoth, okExamined, withSHA, withContract, withBoth)
		return id
	}
	if !missingAnyLevel && withBoth == okExamined && okExamined > 0 {
		id.Proven = true
		id.Equal = true
		id.Method = "X-Merc-Upstream-Body-SHA256 matches harness body sha256 AND valid X-Merc-Contract-ID " +
			"on every OK merc sample at every claimed level"
		id.Detail = fmt.Sprintf("proven: %d/%d OK merc samples carry matching upstream SHA + contract id", withBoth, okExamined)
		return id
	}
	id.Detail = fmt.Sprintf(
		"unproven: missing matching upstream SHA and/or valid X-Merc-Contract-ID on OK merc samples "+
			"(examined=%d sha=%d contract=%d both=%d); require both on every OK sample",
		okExamined, withSHA, withContract, withBoth)
	return id
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
	rec.Notes = append(rec.Notes,
		"primary_metric="+orDefault(budget.PrimaryMetric, "ttft_shift_q95_ms")+
			"; throughput is secondary and never dual-refuses a primary TTFT FAIL",
		"interleaved schedule uses alternating batches at concurrency c (engine peak ≤ c); per-arm wall clocks for throughput",
	)

	var refusals []string
	// LOCAL_METAL_PARITY is a scoped measurement class: gateway overhead against
	// a same-host llama.cpp/Metal arm. It is never catalogue PARITY_EVIDENCE.
	if evidenceClass == "LOCAL_METAL_PARITY" {
		rec.DoesNotProve = append([]string(nil), localMetalParityDoesNotProve...)
		rec.Notes = append(rec.Notes,
			"evidence_class=LOCAL_METAL_PARITY: measures Merc gateway overhead vs direct llama.cpp/Metal "+
				"(same model digest, same host). NOT a measurement of the advertised vLLM/bf16/linux-amd64 profile.",
			"comparable=true under LOCAL_METAL_PARITY means only: Merc-vs-llama.cpp/Metal TTFT overhead is within "+
				"budget at every claimed level with proven body identity. It does NOT mean Merc matches vLLM.",
			"do not re-label as PARITY_EVIDENCE; does_not_prove travels with every number in this receipt",
		)
	}
	// Claim ladder: PARITY_EVIDENCE and LOCAL_METAL_PARITY require {1,8,32}.
	// A lone -concurrency 1 is an incomplete measurement class, not a claim.
	requiresLadder := evidenceClass == "PARITY_EVIDENCE" || evidenceClass == "LOCAL_METAL_PARITY"
	if requiresLadder && !GatewayParityLadderComplete(claimed) {
		rec.EvidenceClass = "INCOMPLETE_LADDER"
		evidenceClass = rec.EvidenceClass
		refusals = append(refusals, fmt.Sprintf(
			"incomplete concurrency ladder: claim class requires levels %v (got %v); "+
				"a single-level run is not a parity claim",
			gatewayParityRequiredLadder, claimed))
		rec.Notes = append(rec.Notes,
			"evidence_class=INCOMPLETE_LADDER: concurrency ladder incomplete; not a parity claim")
	}
	// Identity: Equal never defaults true; unproven is a hard refusal for
	// PARITY_EVIDENCE and LOCAL_METAL_PARITY. Equal=false without a detail still refuses.
	if !bodyIdentity.Equal {
		detail := bodyIdentity.Detail
		if detail == "" {
			detail = "bodies_equal is false (default is false; raised only on evidence)"
		}
		refusals = append(refusals, "request bodies are not byte-identical: "+detail)
	}
	requiresIdentity := evidenceClass == "PARITY_EVIDENCE" || evidenceClass == "LOCAL_METAL_PARITY"
	if !bodyIdentity.Proven && requiresIdentity {
		detail := bodyIdentity.Detail
		if detail == "" {
			detail = "enable MERC_PARITY_CAPTURE_UPSTREAM=1 so merc returns X-Merc-Upstream-Body-SHA256 " +
				"and ensure responses carry X-Merc-Contract-ID from a real control plane"
		}
		if bodyIdentity.BareSHAStandIn {
			refusals = append(refusals, "bare-SHA stand-in refused ("+evidenceClass+"): "+detail)
		} else {
			refusals = append(refusals, "body identity unproven ("+evidenceClass+" requires Merc contract proof): "+detail)
		}
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
			if ok && lvl.Status == "MEASURED" {
				if reason := RefuseGatewayParitySampleCount(c, lvl.RequestsOK); reason != "" {
					refusals = append(refusals, fmt.Sprintf("%s: %s", key, reason))
				}
				if reason := RefuseGatewayParityErrorRate(lvl.RequestsAttempted, lvl.Errors); reason != "" {
					refusals = append(refusals, fmt.Sprintf("%s: %s", key, reason))
				}
				if lvl.TTFTp95 == nil {
					refusals = append(refusals, fmt.Sprintf("%s: ttft_p95 is nil; nil percentile is a refusal", key))
				}
				if lvl.PeakInFlight > c {
					refusals = append(refusals, fmt.Sprintf(
						"%s: peak in-flight %d exceeds claimed concurrency %d", key, lvl.PeakInFlight, c))
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
	// INCONCLUSIVE is not a pass; comparable only when every level PASS.
	rec.Comparable = len(uniq) == 0 && rec.Gate.GatePassed && rec.Gate.Verdict == "PASS"
	rec.GatePassed = rec.Gate.GatePassed && len(uniq) == 0 && rec.Gate.Verdict == "PASS"
	if !rec.GatePassed && len(uniq) > 0 {
		rec.RefusalReason = uniq[0]
	} else if !rec.GatePassed && rec.Gate.Verdict == "INCONCLUSIVE" {
		rec.RefusalReason = "gate verdict INCONCLUSIVE: under-powered (MDE > budget) or overhead CI straddles budget"
	}
	// Self-tests and incomplete ladders must never be mistaken for parity evidence.
	if evidenceClass == "HARNESS_SELF_TEST" {
		rec.GatePassed = false
		rec.Comparable = false
		if rec.RefusalReason == "" {
			rec.RefusalReason = "HARNESS_SELF_TEST: not parity evidence"
		}
		// Ensure the self-test refusal is present even when the gate would pass.
		hasSelf := false
		for _, r := range rec.Refusals {
			if strings.Contains(r, "HARNESS_SELF_TEST") {
				hasSelf = true
				break
			}
		}
		if !hasSelf {
			rec.Refusals = append(rec.Refusals, "HARNESS_SELF_TEST: not parity evidence")
		}
		rec.Notes = append(rec.Notes,
			"evidence_class=HARNESS_SELF_TEST: this receipt proves the harness works; it is NOT a gateway-vs-engine parity claim",
			"self-test stand-in deliberately omits X-Merc-Contract-ID so body_identity cannot satisfy PARITY_EVIDENCE proof")
	}
	if evidenceClass == "INCOMPLETE_LADDER" {
		rec.GatePassed = false
		rec.Comparable = false
		if rec.RefusalReason == "" {
			rec.RefusalReason = "INCOMPLETE_LADDER: not a parity claim"
		}
	}
	// Collapse dual pass flags: nested gate.gate_passed cannot read true while
	// the receipt itself is refused, a self-test, or an incomplete ladder.
	if !rec.GatePassed {
		rec.Gate.GatePassed = false
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
