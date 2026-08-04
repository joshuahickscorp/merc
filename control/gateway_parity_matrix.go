package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Multi-dimensional gateway parity matrix.
//
// The legacy harness varied concurrency only — one prompt, one output length,
// always labelled warm. Gateway overhead is close to constant per request, so a
// parity claim at one shape says nothing about the others. Merc's prefix-locality
// story lives entirely on the state axis the harness did not vary.
//
// This file adds prompt size, output size, and state (cold / warm / prefix-hit)
// as first-class, independently gated cells. Traffic class is deliberately not
// an axis: it is currently a label nothing in the gateway acts on, so varying it
// would measure nothing (see TrafficClassNonActingNote).
//
// Rules that survive from the single-shape harness:
//   - proven body identity (per cell)
//   - peak in-flight cap
//   - error-rate budget
//   - minimum-detectable-effect gate
//   - full concurrency ladder requirement for PARITY_EVIDENCE
//   - PASS / FAIL / INCONCLUSIVE interval verdicts
//   - adding a dimension must not make PASS easier anywhere
//
// A cell that fails or is under-powered is refused on its own. Quoting one cell
// as another is the same defect as letting c=1 stand in for c=32.

const (
	// gatewayParityPrefixHitMinFraction is the minimum share of OK merc samples
	// that must report cached_tokens > 0 for a prefix-hit cell to be verified.
	// Below this the cell is REFUSED — an assumed hit is not a measurement.
	gatewayParityPrefixHitMinFraction = 0.80

	// Shared-prefix marker embedded so a stand-in (and a human reading the
	// receipt) can see which requests were arranged for a prefix hit.
	gatewayParitySharedPrefixMarker = "[merc-parity-shared-prefix-v1]"
)

// TrafficClassNonActingNote is emitted on every matrix receipt. Traffic class
// is listed in the programme directive (interactive / throughput / background)
// but nothing in the gateway currently branches on it for the parity path.
// Varying a no-op label would invent a dimension that measures nothing.
const TrafficClassNonActingNote = "traffic class (interactive|throughput|background) is a non-acting label on the parity path today; the harness does not vary it because nothing would change. When a class becomes an admission/scheduling input, re-add it as a refused-until-wired axis rather than a free label."

// Full documented axes. Cartesian product without traffic class:
// 8 × 5 × 4 × 3 = 480. With traffic class (non-acting): 1440.
var (
	gatewayParityFullConcurrency = []int{1, 2, 4, 8, 16, 32, 64, 128}
	gatewayParityFullPromptTok   = []int{32, 256, 1024, 8192, 32768}
	gatewayParityFullOutputTok   = []int{16, 128, 512, 2048}
	gatewayParityFullStates      = []string{"cold", "warm", "prefix-hit"}
	// Documented only — not selected; see TrafficClassNonActingNote.
	gatewayParityFullTrafficClasses = []string{"interactive", "throughput", "background"}
)

// GatewayParityAxes is the documented full multi-dimensional space.
type GatewayParityAxes struct {
	Concurrency   []int    `json:"concurrency"`
	PromptTokens  []int    `json:"prompt_tokens"`
	OutputTokens  []int    `json:"output_tokens"`
	State         []string `json:"state"`
	TrafficClass  []string `json:"traffic_class_documented_not_selected"`
	FullCellCount int      `json:"full_cell_count_including_traffic_class"`
	// WithoutTrafficClass is 8×5×4×3 = 480 — the product of acting axes.
	ActingCellCount int `json:"acting_cell_count_without_traffic_class"`
}

// FullGatewayParityAxes returns the complete documented axis set.
func FullGatewayParityAxes() GatewayParityAxes {
	return GatewayParityAxes{
		Concurrency:     append([]int(nil), gatewayParityFullConcurrency...),
		PromptTokens:    append([]int(nil), gatewayParityFullPromptTok...),
		OutputTokens:    append([]int(nil), gatewayParityFullOutputTok...),
		State:           append([]string(nil), gatewayParityFullStates...),
		TrafficClass:    append([]string(nil), gatewayParityFullTrafficClasses...),
		FullCellCount:   8 * 5 * 4 * 3 * 3, // 1440
		ActingCellCount: 8 * 5 * 4 * 3,     // 480
	}
}

// GatewayParityCellSpec is one independently gated matrix cell.
type GatewayParityCellSpec struct {
	Concurrency  int    `json:"concurrency"`
	PromptTokens int    `json:"prompt_tokens"`
	OutputTokens int    `json:"output_tokens"`
	State        string `json:"state"` // cold | warm | prefix-hit
}

// Key is a stable identity for maps and refusal records.
func (p GatewayParityCellSpec) Key() string {
	return fmt.Sprintf("c=%d/p=%d/o=%d/state=%s",
		p.Concurrency, p.PromptTokens, p.OutputTokens, p.State)
}

// GatewayParityDroppedAxis records a wholesale axis-value omission.
type GatewayParityDroppedAxis struct {
	Axis   string   `json:"axis"`
	Values []string `json:"values"`
	Reason string   `json:"reason"`
}

// GatewayParityMatrixSelection is the chosen subset plus machine-readable drops.
type GatewayParityMatrixSelection struct {
	Selected       []GatewayParityCellSpec    `json:"selected"`
	DroppedAxes    []GatewayParityDroppedAxis `json:"dropped_axes"`
	DroppedSummary []string                   `json:"dropped_summary"`
	Rationale      string                     `json:"rationale"`
}

// DefaultGatewayParityMatrixSelection returns a defensible ~16–20 cell subset.
//
// Argument (also emitted into DroppedSummary / Rationale):
//
// The full 1440-cell product is unreadable and mostly uninformative. Gateway
// overhead is near-constant per request, so the answer changes at the corners:
//
//  1. Overhead-dominated — short prompt + short output (p=32, o=16): fixed
//     gateway cost is the largest fraction of wall time.
//  2. Prefill-dominated — long prompt + short output (p=8192, o=16): engine
//     prefill dominates; absolute overhead is the same but relative is small.
//  3. Decode-dominated — short prompt + long output (p=32, o=512): long decode
//     dilutes per-request overhead across many tokens.
//  4. State contrast — cold vs warm vs prefix-hit on the overhead shape, and
//     cold vs prefix-hit on the prefill shape (where prefix reuse actually
//     matters). A prefix-hit cell that cannot verify a cache hit is REFUSED.
//  5. Concurrency ladder {1,8,32} is required for PARITY_EVIDENCE and is run on
//     the overhead shape for cold and warm; higher concurrencies and intermediate
//     steps are wall-time / single-host limited.
//
// Traffic class is documented and dropped entirely (non-acting label).
func DefaultGatewayParityMatrixSelection() GatewayParityMatrixSelection {
	type shape struct{ p, o int }
	overhead := shape{32, 16}
	prefill := shape{8192, 16}
	decode := shape{32, 512}
	legacy := shape{256, 128} // historical single-shape default

	var selected []GatewayParityCellSpec
	add := func(s shape, state string, conc ...int) {
		for _, c := range conc {
			selected = append(selected, GatewayParityCellSpec{
				Concurrency: c, PromptTokens: s.p, OutputTokens: s.o, State: state,
			})
		}
	}
	// Overhead shape: full state × ladder (prefix-hit only to c=8 — at c=32 a
	// prefix cache often thrash-evicts and the cell would refuse on signal).
	add(overhead, "warm", 1, 8, 32)
	add(overhead, "cold", 1, 8, 32)
	add(overhead, "prefix-hit", 1, 8)
	// Prefill shape: warm at {1,8}; cold + prefix-hit at c=1 for contrast.
	add(prefill, "warm", 1, 8)
	add(prefill, "cold", 1)
	add(prefill, "prefix-hit", 1)
	// Decode shape: warm at {1,8}; cold at c=1.
	add(decode, "warm", 1, 8)
	add(decode, "cold", 1)
	// Legacy mid shape (warm@c=1) so the historical single-shape remains a cell.
	add(legacy, "warm", 1)

	return GatewayParityMatrixSelection{
		Selected: selected,
		Rationale: "corners that change the overhead answer: (32,16) overhead-dominated " +
			"with full state×ladder; (8192,16) prefill-dominated; (32,512) decode-dominated; " +
			"prefix-hit vs cold contrast where reuse can matter; traffic class omitted (non-acting). " +
			fmt.Sprintf("selected=%d of acting=%d (full with traffic class=%d)",
				len(selected), 8*5*4*3, 8*5*4*3*3),
		DroppedSummary: []string{
			fmt.Sprintf("selected %d cells (not 1440); every omission is in dropped_axes", len(selected)),
			"concurrency kept {1,8,32} on overhead cold/warm; prefix-hit and non-overhead shapes use a thinner ladder; dropped {2,4,16,64,128}",
			"prompt_tokens kept {32,256,8192}; dropped {1024,32768} (32K reserved for explicit long-context profile)",
			"output_tokens kept {16,128,512}; dropped {2048} (2K reserved for explicit long-decode profile)",
			"state kept {cold,warm,prefix-hit}; prefix-hit refused per-cell without observed cached_tokens signal",
			"traffic_class dropped entirely: non-acting label on the parity path (see traffic_class_note)",
			"silent truncation is forbidden: quoting one cell as another is refused by per-cell gating",
		},
		DroppedAxes: []GatewayParityDroppedAxis{
			{Axis: "concurrency", Values: strInts([]int{2, 4, 16, 64, 128}), Reason: "outside selected ladders; wall-time / single-host scale"},
			{Axis: "prompt_tokens", Values: strInts([]int{1024, 32768}), Reason: "outside selected {32,256,8192}; 32K long-context profile only"},
			{Axis: "output_tokens", Values: strInts([]int{2048}), Reason: "outside selected {16,128,512}; 2K long-decode profile only"},
			{Axis: "traffic_class", Values: append([]string(nil), gatewayParityFullTrafficClasses...), Reason: TrafficClassNonActingNote},
		},
	}
}

// CompetitiveCUDAParityMatrixSelection is the revision-1 competitive matrix.
//
// Extends the defensible subset so the CUDA dual-arm run answers the programme
// question on the full concurrency ladder {1,8,32,64,128} where the pod
// sustains them, with short/medium/long prompts, short/long outputs, and
// cold/warm (plus prefix-hit contrast on the overhead shape). Every cell is
// independently gated — no level may be claimed from another level's data.
//
// Selected deliberately (not the 1440-cell product): corners that change the
// overhead answer, plus the full ladder on the overhead-dominated shape where
// gateway cost is the largest fraction of wall time.
func CompetitiveCUDAParityMatrixSelection() GatewayParityMatrixSelection {
	type shape struct{ p, o int }
	overhead := shape{32, 16}  // short prompt, short output
	medium := shape{256, 128}  // medium / legacy mid
	prefill := shape{8192, 16} // long prompt, short output
	decode := shape{32, 512}   // short prompt, long output
	// Full competitive ladder required by revision-1. A cell that the pod
	// cannot sustain will REFUSE on its own (error rate / peak in-flight).
	ladder := []int{1, 8, 32, 64, 128}

	var selected []GatewayParityCellSpec
	add := func(s shape, state string, conc ...int) {
		for _, c := range conc {
			selected = append(selected, GatewayParityCellSpec{
				Concurrency: c, PromptTokens: s.p, OutputTokens: s.o, State: state,
			})
		}
	}
	// Overhead shape: cold + warm × full ladder. Prefix-hit only to c=8
	// (higher concurrencies thrash-evict the prefix cache and refuse on signal).
	add(overhead, "warm", ladder...)
	add(overhead, "cold", ladder...)
	add(overhead, "prefix-hit", 1, 8)
	// Medium shape: warm × {1,8,32}; cold at c=1 for contrast.
	add(medium, "warm", 1, 8, 32)
	add(medium, "cold", 1)
	// Prefill shape: warm × {1,8}; cold + prefix-hit at c=1.
	add(prefill, "warm", 1, 8)
	add(prefill, "cold", 1)
	add(prefill, "prefix-hit", 1)
	// Decode shape: warm × {1,8}; cold at c=1.
	add(decode, "warm", 1, 8)
	add(decode, "cold", 1)

	return GatewayParityMatrixSelection{
		Selected: selected,
		Rationale: "competitive CUDA matrix (revision-1): overhead (32,16) cold+warm × " +
			"{1,8,32,64,128}; medium (256,128); prefill (8192,16); decode (32,512); " +
			"prefix-hit contrast where reuse can matter; every cell independently gated. " +
			fmt.Sprintf("selected=%d", len(selected)),
		DroppedSummary: []string{
			fmt.Sprintf("selected %d cells; every omission is in dropped_axes", len(selected)),
			"concurrency full competitive ladder {1,8,32,64,128} on overhead cold/warm; thinner ladders on non-overhead shapes; dropped {2,4,16}",
			"prompt_tokens kept {32,256,8192}; dropped {1024,32768}",
			"output_tokens kept {16,128,512}; dropped {2048}",
			"state kept {cold,warm,prefix-hit}; prefix-hit refused per-cell without cached_tokens signal",
			"traffic_class dropped entirely: non-acting label on the parity path",
			"silent truncation forbidden: quoting one cell as another refused by per-cell gating",
		},
		DroppedAxes: []GatewayParityDroppedAxis{
			{Axis: "concurrency", Values: strInts([]int{2, 4, 16}), Reason: "outside competitive ladder {1,8,32,64,128}"},
			{Axis: "prompt_tokens", Values: strInts([]int{1024, 32768}), Reason: "outside selected {32,256,8192}"},
			{Axis: "output_tokens", Values: strInts([]int{2048}), Reason: "outside selected {16,128,512}"},
			{Axis: "traffic_class", Values: append([]string(nil), gatewayParityFullTrafficClasses...), Reason: TrafficClassNonActingNote},
		},
	}
}

// GatewayParityStateDefinitions are the precise operational meanings of state.
// Embedded in every matrix receipt so a reader never invents a softer meaning.
type GatewayParityStateDefinitions struct {
	Cold string `json:"cold"`
	Warm string `json:"warm"`
	// PrefixHit covers arrangement AND verification. Unverified = refused.
	PrefixHit string `json:"prefix_hit"`
}

// GatewayParityStateDefinitionsText returns the canonical definitions.
func GatewayParityStateDefinitionsText() GatewayParityStateDefinitions {
	return GatewayParityStateDefinitions{
		Cold: "No discarded warm-up is issued. A fresh GatewayParityClient (new TCP pool) is built for the cell so first-sample connection setup is included. Cold is a harness path, not a claim that the remote engine's model weights were unloaded — engines rarely expose a force-evict API. The receipt records cold_no_warmup=true and cold_fresh_client=true; that is the definition, not an assumption about engine KV emptiness.",
		Warm: "At least one discarded warm-up completion is issued on BOTH arms (same model, short max_tokens) and must finish with ttft_ms>0 and a finish_reason before measurement samples start. Warmth is verified by warmup_requests_ok>=2 (one per arm) and warmup_verified=true. Conn_reused on later samples is reported but not required (a load balancer may still open a new socket).",
		PrefixHit: "Arrangement: (1) a shared leading prefix of target prompt size containing " +
			gatewayParitySharedPrefixMarker + "; (2) a prime request with that prefix on both arms; " +
			"(3) measurement requests reuse the same leading prefix with a unique per-sample tail. " +
			"Verification (hard): at least 80% of OK merc samples must report usage.prompt_tokens_details.cached_tokens > 0. " +
			"Missing signal is NOT treated as a hit — the cell is REFUSED with an explicit reason. " +
			"llama.cpp/MLX/Candle typically lack the OpenAI cached_tokens field; those engines refuse prefix-hit cells rather than invent a hit.",
	}
}

// GatewayParityStateProof is the per-cell evidence that the claimed state was
// established and, for prefix-hit, verified rather than assumed.
type GatewayParityStateProof struct {
	ClaimedState string `json:"claimed_state"`

	// Cold path markers.
	ColdNoWarmup    bool `json:"cold_no_warmup,omitempty"`
	ColdFreshClient bool `json:"cold_fresh_client,omitempty"`

	// Warm path markers.
	WarmupRequestsOK int  `json:"warmup_requests_ok,omitempty"`
	WarmupVerified   bool `json:"warmup_verified,omitempty"`

	// Prefix-hit arrangement + observed signal.
	SharedPrefixChars      int  `json:"shared_prefix_chars,omitempty"`
	PrefixPrimeOK          bool `json:"prefix_prime_ok,omitempty"`
	CachedTokensMin        int  `json:"cached_tokens_min,omitempty"`
	CachedTokensMax        int  `json:"cached_tokens_max,omitempty"`
	SamplesWithCacheSignal int  `json:"samples_with_cache_signal,omitempty"`
	SamplesWithCacheHit    int  `json:"samples_with_cache_hit,omitempty"`
	SamplesOK              int  `json:"samples_ok,omitempty"`
	// HitFraction is SamplesWithCacheHit / SamplesOK when SamplesOK > 0.
	HitFraction float64 `json:"hit_fraction,omitempty"`

	// Verified is true only when the claimed state meets its proof rule.
	// A prefix-hit cell with Verified=false is REFUSED.
	Verified bool   `json:"verified"`
	Detail   string `json:"detail"`
}

// GatewayParityCellResult is one independently gated matrix cell.
type GatewayParityCellResult struct {
	Spec   GatewayParityCellSpec `json:"spec"`
	Status string                `json:"status"` // MEASURED | REFUSED
	Reason string                `json:"reason,omitempty"`
	// Gate is the interval verdict for THIS cell alone. A PASS here is not a
	// PASS for any other cell.
	Gate             GatewayParityGateLevel        `json:"gate"`
	StateProof       GatewayParityStateProof       `json:"state_proof"`
	SamplingContract GatewayParitySamplingContract `json:"sampling_contract"`
	BodyIdentity     GatewayParityBodyIdentity     `json:"body_identity"`
	Merc             GatewayParityLevelResult      `json:"merc"`
	Direct           GatewayParityLevelResult      `json:"direct"`
	// RelativeOverhead is merc_ttft_p95 / direct_ttft_p95 when both defined.
	// Used only for shape insight, never as a pass criterion.
	RelativeOverhead *float64 `json:"relative_overhead_ttft_p95,omitempty"`
	// AbsoluteOverheadMs is merc − direct TTFT p95 point estimate.
	AbsoluteOverheadMs *float64 `json:"absolute_overhead_ttft_p95_ms,omitempty"`
}

// GatewayParityShapeInsight summarises where fixed overhead matters most.
// Computed from measured cells; never assumed.
type GatewayParityShapeInsight struct {
	Method string `json:"method"`
	// HighestRelativeCell is the measured cell where merc/direct TTFT p95 ratio
	// is largest (gateway cost as a fraction of request cost).
	HighestRelativeCell string   `json:"highest_relative_cell,omitempty"`
	HighestRelative     *float64 `json:"highest_relative_overhead,omitempty"`
	// LowestRelativeCell is the measured cell where the ratio is smallest.
	LowestRelativeCell string   `json:"lowest_relative_cell,omitempty"`
	LowestRelative     *float64 `json:"lowest_relative_overhead,omitempty"`
	// Finding is a one-line measured conclusion when both ends exist.
	Finding string `json:"finding,omitempty"`
	// PerCell lists every measured cell's relative overhead for the skeptic.
	PerCell []GatewayParityShapeCellNote `json:"per_cell,omitempty"`
}

// GatewayParityShapeCellNote is one row of the shape insight table.
type GatewayParityShapeCellNote struct {
	Cell               string   `json:"cell"`
	AbsoluteOverheadMs *float64 `json:"absolute_overhead_ttft_p95_ms,omitempty"`
	RelativeOverhead   *float64 `json:"relative_overhead_ttft_p95,omitempty"`
	MercTTFTp95Ms      *float64 `json:"merc_ttft_p95_ms,omitempty"`
	DirectTTFTp95Ms    *float64 `json:"direct_ttft_p95_ms,omitempty"`
	Status             string   `json:"status"`
	GateVerdict        string   `json:"gate_verdict,omitempty"`
}

// GatewayParityPromptForCell builds a prompt of approximately PromptTokens
// (≈4 chars/token English average) with state-appropriate structure.
//
// For prefix-hit, the shared leading block is stable across the cell and only
// the trailing tag varies per sample index (see GatewayParityPromptVariant).
func GatewayParityPromptForCell(spec GatewayParityCellSpec) string {
	base := "Write a short factual paragraph about the water cycle. Be specific and do not repeat yourself. "
	targetChars := spec.PromptTokens * 4
	if targetChars < len(base) {
		targetChars = len(base)
	}
	if spec.State == "prefix-hit" {
		// Shared leading prefix: marker + pad. Tail is appended per sample.
		prefixBudget := targetChars
		if prefixBudget < len(gatewayParitySharedPrefixMarker)+len(base)+16 {
			prefixBudget = len(gatewayParitySharedPrefixMarker) + len(base) + 16
		}
		padLen := prefixBudget - len(gatewayParitySharedPrefixMarker) - len(base) - 1
		if padLen < 0 {
			padLen = 0
		}
		pad := strings.Repeat("detail ", padLen/7+1)
		body := gatewayParitySharedPrefixMarker + " " + base + pad
		if len(body) > prefixBudget {
			body = body[:prefixBudget]
		}
		return body
	}
	pad := strings.Repeat("detail ", (targetChars-len(base))/7+1)
	body := base + pad
	if len(body) > targetChars {
		body = body[:targetChars]
	}
	// Warm/cold: slight per-cell (not per-sample) tag so pure prefix caching
	// does not collapse every warm request into an accidental free hit.
	return fmt.Sprintf("%s [shape p=%d o=%d state=%s]", body, spec.PromptTokens, spec.OutputTokens, spec.State)
}

// GatewayParityPromptVariant returns the on-the-wire prompt for sample index i.
// prefix-hit cells share the leading prefix and only vary the tail.
func GatewayParityPromptVariant(shared string, spec GatewayParityCellSpec, index int) string {
	if spec.State == "prefix-hit" {
		return fmt.Sprintf("%s [tail=%d]", shared, index)
	}
	// Non-prefix states: identical body across samples (byte-identity proof).
	// Variation is only across cells, not within a cell, so both arms send the
	// same bytes for every sample of the cell.
	return shared
}

// VerifyGatewayParityStateProof applies the hard verification rules for a
// claimed state given the proof fields already filled by the runner.
func VerifyGatewayParityStateProof(proof *GatewayParityStateProof) {
	if proof == nil {
		return
	}
	switch proof.ClaimedState {
	case "cold":
		if proof.ColdNoWarmup && proof.ColdFreshClient {
			proof.Verified = true
			proof.Detail = "cold path: no warm-up issued; fresh client (new TCP pool) for this cell"
			return
		}
		proof.Verified = false
		proof.Detail = "cold path incomplete: require cold_no_warmup and cold_fresh_client"
	case "warm":
		if proof.WarmupVerified && proof.WarmupRequestsOK >= 2 {
			proof.Verified = true
			proof.Detail = fmt.Sprintf("warm path: %d discarded warm-up(s) completed OK on both arms", proof.WarmupRequestsOK)
			return
		}
		proof.Verified = false
		proof.Detail = fmt.Sprintf("warm path incomplete: warmup_verified=%v warmup_requests_ok=%d (need ≥2, both arms)",
			proof.WarmupVerified, proof.WarmupRequestsOK)
	case "prefix-hit":
		if !proof.PrefixPrimeOK {
			proof.Verified = false
			proof.Detail = "prefix-hit arrangement failed: prime request did not complete OK on both arms"
			return
		}
		if proof.SamplesOK == 0 {
			proof.Verified = false
			proof.Detail = "prefix-hit verification failed: no OK merc samples to examine for cached_tokens"
			return
		}
		if proof.SamplesWithCacheSignal == 0 {
			proof.Verified = false
			proof.Detail = "prefix-hit verification failed: engine reported no usage.prompt_tokens_details.cached_tokens " +
				"on any OK merc sample; missing signal is not a hit — cell refused (llama.cpp/MLX/Candle typically lack this field)"
			return
		}
		proof.HitFraction = float64(proof.SamplesWithCacheHit) / float64(proof.SamplesOK)
		if proof.HitFraction+1e-9 < gatewayParityPrefixHitMinFraction {
			proof.Verified = false
			proof.Detail = fmt.Sprintf(
				"prefix-hit verification failed: hit_fraction=%.3f (cached_tokens>0 on %d/%d OK merc samples) below floor %.2f; "+
					"an assumed hit is not a measurement",
				proof.HitFraction, proof.SamplesWithCacheHit, proof.SamplesOK, gatewayParityPrefixHitMinFraction)
			return
		}
		proof.Verified = true
		proof.Detail = fmt.Sprintf(
			"prefix-hit verified: prime OK; cached_tokens>0 on %d/%d OK merc samples (fraction=%.3f, min=%d max=%d); signal present on %d",
			proof.SamplesWithCacheHit, proof.SamplesOK, proof.HitFraction,
			proof.CachedTokensMin, proof.CachedTokensMax, proof.SamplesWithCacheSignal)
	default:
		proof.Verified = false
		proof.Detail = fmt.Sprintf("unknown state %q; refuse rather than invent a meaning", proof.ClaimedState)
	}
}

// CollectPrefixHitSignal fills cache-signal fields on proof from merc samples.
func CollectPrefixHitSignal(proof *GatewayParityStateProof, merc GatewayParityLevelResult) {
	if proof == nil {
		return
	}
	minHit := 0
	maxHit := 0
	first := true
	for _, s := range merc.RawSamples {
		if s.Err != "" || s.TTFTMs <= 0 || s.FinishReason == "" {
			continue
		}
		proof.SamplesOK++
		if !s.CachedTokensReported {
			continue
		}
		proof.SamplesWithCacheSignal++
		if first || s.CachedTokens < minHit {
			minHit = s.CachedTokens
		}
		if first || s.CachedTokens > maxHit {
			maxHit = s.CachedTokens
		}
		first = false
		if s.CachedTokens > 0 {
			proof.SamplesWithCacheHit++
		}
	}
	if proof.SamplesWithCacheSignal > 0 {
		proof.CachedTokensMin = minHit
		proof.CachedTokensMax = maxHit
	}
}

// RunGatewayParityMatrixCell measures one cell against merc and direct.
//
// State establishment and verification are part of the cell, not a free label.
// A prefix-hit cell that cannot verify a cache hit returns Status=REFUSED.
func RunGatewayParityMatrixCell(
	ctx context.Context,
	mercURL, mercKey, directURL, directKey string,
	baseContract GatewayParitySamplingContract,
	spec GatewayParityCellSpec,
	n int,
) GatewayParityCellResult {
	cell := GatewayParityCellResult{
		Spec:   spec,
		Status: "MEASURED",
	}
	proof := GatewayParityStateProof{ClaimedState: spec.State}

	// Per-cell client: cold always gets a fresh pool; warm/prefix-hit also use
	// a dedicated client so cell boundaries do not leak connection state.
	client := NewGatewayParityClient(spec.Concurrency)
	if spec.State == "cold" {
		proof.ColdNoWarmup = true
		proof.ColdFreshClient = true
	}

	sharedPrompt := GatewayParityPromptForCell(spec)
	// Measurement body uses the non-variant prompt for warm/cold (byte-identical
	// across samples). For prefix-hit the shared prefix is constant and the
	// harness still needs one body for identity proof — we use the prime body
	// (shared + tail=prime) only for warm-up; measurement uses per-index tails
	// and proves identity per sample via request_body_sha256 equality across arms
	// (both arms get the same variant for index i).
	measurePrompt := GatewayParityPromptVariant(sharedPrompt, spec, 0)
	contract := baseContract
	contract.Prompt = measurePrompt
	contract.MaxTokens = spec.OutputTokens
	body, err := contract.BuildChatCompletionsBody()
	if err != nil {
		cell.Status = "REFUSED"
		cell.Reason = "refused: cannot build request body: " + err.Error()
		cell.StateProof = proof
		return cell
	}
	cell.SamplingContract = contract

	// --- State establishment ---
	switch spec.State {
	case "cold":
		// No warm-up by definition.
	case "warm":
		warmTok := spec.OutputTokens
		if warmTok > 8 {
			warmTok = 8
		}
		warmContract := contract
		warmContract.MaxTokens = warmTok
		warmBody, werr := warmContract.BuildChatCompletionsBody()
		if werr != nil {
			cell.Status = "REFUSED"
			cell.Reason = "refused: warm-up body build failed: " + werr.Error()
			VerifyGatewayParityStateProof(&proof)
			cell.StateProof = proof
			return cell
		}
		ok := 0
		for _, arm := range []struct{ url, key, name string }{
			{mercURL, mercKey, "merc"},
			{directURL, directKey, "direct"},
		} {
			s := client.CompleteOneStream(ctx, arm.url, arm.key, warmBody, arm.name, -1)
			if s.Err == "" && s.TTFTMs > 0 && s.FinishReason != "" {
				ok++
			}
		}
		proof.WarmupRequestsOK = ok
		proof.WarmupVerified = ok >= 2
	case "prefix-hit":
		// Prime both arms with the shared prefix so a real engine can populate KV.
		primePrompt := GatewayParityPromptVariant(sharedPrompt, spec, -1)
		primeContract := contract
		primeContract.Prompt = primePrompt
		primeTok := spec.OutputTokens
		if primeTok > 8 {
			primeTok = 8
		}
		primeContract.MaxTokens = primeTok
		primeBody, perr := primeContract.BuildChatCompletionsBody()
		if perr != nil {
			cell.Status = "REFUSED"
			cell.Reason = "refused: prefix prime body build failed: " + perr.Error()
			VerifyGatewayParityStateProof(&proof)
			cell.StateProof = proof
			return cell
		}
		proof.SharedPrefixChars = len(sharedPrompt)
		ok := 0
		for _, arm := range []struct{ url, key, name string }{
			{mercURL, mercKey, "merc"},
			{directURL, directKey, "direct"},
		} {
			s := client.CompleteOneStream(ctx, arm.url, arm.key, primeBody, arm.name, -1)
			if s.Err == "" && s.TTFTMs > 0 && s.FinishReason != "" {
				ok++
			}
		}
		proof.PrefixPrimeOK = ok >= 2
		proof.WarmupRequestsOK = ok
	default:
		cell.Status = "REFUSED"
		cell.Reason = fmt.Sprintf("refused: unknown state %q", spec.State)
		VerifyGatewayParityStateProof(&proof)
		cell.StateProof = proof
		return cell
	}

	// --- Measurement ---
	// For prefix-hit, samples must use unique tails while sharing the prefix.
	// The interleaved runner takes a single body; for prefix-hit we run a
	// variant-aware path that still alternates arms and caps in-flight at c.
	var merc, direct GatewayParityLevelResult
	if spec.State == "prefix-hit" {
		merc, direct = runGatewayParityPrefixHitLevel(
			ctx, client, mercURL, mercKey, directURL, directKey,
			baseContract, sharedPrompt, spec, n,
		)
	} else {
		merc, direct = RunGatewayParityInterleavedLevel(
			ctx, client, mercURL, mercKey, directURL, directKey, body, spec.Concurrency, n,
		)
	}
	cell.Merc = merc
	cell.Direct = direct

	// Body identity for this cell (standard key shape for the prover).
	levels := map[string]GatewayParityLevelResult{
		fmt.Sprintf("merc@c=%d", spec.Concurrency):   merc,
		fmt.Sprintf("direct@c=%d", spec.Concurrency): direct,
	}
	// For prefix-hit, bodies differ per sample index but arms match pairwise.
	// Prove identity from the first sample's SHA equality across arms when the
	// harness body is the index-0 variant; still require Merc contract proof
	// for PARITY_EVIDENCE via the standard prover on the index-0 body.
	idBody := body
	if spec.State == "prefix-hit" && len(merc.RawSamples) > 0 && merc.RawSamples[0].RequestBodySHA != "" {
		// Rebuild index-0 body for the prover.
		p0 := GatewayParityPromptVariant(sharedPrompt, spec, 0)
		c0 := baseContract
		c0.Prompt = p0
		c0.MaxTokens = spec.OutputTokens
		if b0, err := c0.BuildChatCompletionsBody(); err == nil {
			idBody = b0
			cell.SamplingContract = c0
		}
	}
	cell.BodyIdentity = ProveGatewayParityBodyIdentity(idBody, []int{spec.Concurrency}, levels)

	// Prefix-hit signal collection + state verification.
	if spec.State == "prefix-hit" {
		CollectPrefixHitSignal(&proof, merc)
	}
	VerifyGatewayParityStateProof(&proof)
	cell.StateProof = proof

	if !proof.Verified {
		cell.Status = "REFUSED"
		cell.Reason = "refused: state not verified: " + proof.Detail
		// Still compute a gate level so the receipt shows what the numbers were,
		// but Passed will be false because of the structural refusal below.
	}
	if merc.Status == "REFUSED" || direct.Status == "REFUSED" {
		cell.Status = "REFUSED"
		if cell.Reason == "" {
			cell.Reason = fmt.Sprintf("refused: arm status merc=%s (%s) direct=%s (%s)",
				merc.Status, merc.Reason, direct.Status, direct.Reason)
		}
	}

	// Per-cell gate (same budget / MDE / interval rules as the single-shape path).
	gate := EvaluateGatewayParityGate([]int{spec.Concurrency}, levels, DefaultGatewayParityBudget())
	if len(gate.Levels) == 1 {
		cell.Gate = gate.Levels[0]
	} else {
		cell.Gate = GatewayParityGateLevel{
			Concurrency: spec.Concurrency,
			Verdict:     "FAIL",
			Refusals:    []string{"internal: gate returned no level for cell"},
		}
	}
	// State verification failure is a structural refusal on the cell, even if
	// the TTFT interval would have passed — never launder an unverified state.
	if !proof.Verified {
		cell.Gate.Verdict = "FAIL"
		cell.Gate.Passed = false
		cell.Gate.Refusals = append(cell.Gate.Refusals, cell.Reason)
	}
	if cell.Status == "REFUSED" && cell.Gate.Verdict == "PASS" {
		cell.Gate.Verdict = "FAIL"
		cell.Gate.Passed = false
		if cell.Reason != "" {
			cell.Gate.Refusals = append(cell.Gate.Refusals, cell.Reason)
		}
	}
	cell.Gate.Passed = cell.Gate.Verdict == "PASS" && len(cell.Gate.Refusals) == 0

	// Shape metrics (informational).
	if merc.TTFTp95 != nil && direct.TTFTp95 != nil &&
		!math.IsNaN(merc.TTFTp95.Point) && !math.IsNaN(direct.TTFTp95.Point) &&
		direct.TTFTp95.Point > 0 {
		abs := merc.TTFTp95.Point - direct.TTFTp95.Point
		rel := merc.TTFTp95.Point / direct.TTFTp95.Point
		cell.AbsoluteOverheadMs = &abs
		cell.RelativeOverhead = &rel
	}
	return cell
}

// runGatewayParityPrefixHitLevel is the prefix-hit measurement path: each sample
// index i uses a shared leading prefix with a unique tail, both arms get the
// identical body for that index, and engine in-flight is still capped at c.
func runGatewayParityPrefixHitLevel(
	ctx context.Context,
	client *GatewayParityClient,
	mercURL, mercKey, directURL, directKey string,
	baseContract GatewayParitySamplingContract,
	sharedPrefix string,
	spec GatewayParityCellSpec,
	n int,
) (merc, direct GatewayParityLevelResult) {
	c := spec.Concurrency
	if reason := RefuseGatewayParitySampleCount(c, n); reason != "" {
		merc = GatewayParityLevelResult{Arm: "merc", Concurrency: c, RequestsAttempted: n, Status: "REFUSED", Reason: reason}
		direct = GatewayParityLevelResult{Arm: "direct", Concurrency: c, RequestsAttempted: n, Status: "REFUSED", Reason: reason}
		return merc, direct
	}
	bodies := make([][]byte, n)
	for i := 0; i < n; i++ {
		contract := baseContract
		contract.Prompt = GatewayParityPromptVariant(sharedPrefix, spec, i)
		contract.MaxTokens = spec.OutputTokens
		b, err := contract.BuildChatCompletionsBody()
		if err != nil {
			merc = GatewayParityLevelResult{Arm: "merc", Concurrency: c, RequestsAttempted: n, Status: "REFUSED", Reason: "body build: " + err.Error()}
			direct = GatewayParityLevelResult{Arm: "direct", Concurrency: c, RequestsAttempted: n, Status: "REFUSED", Reason: "body build: " + err.Error()}
			return merc, direct
		}
		bodies[i] = b
	}

	mercSamples := make([]GatewayParityRawSample, n)
	directSamples := make([]GatewayParityRawSample, n)
	var (
		mercInFlight   atomic.Int64
		directInFlight atomic.Int64
		peakMerc       atomic.Int64
		peakDirect     atomic.Int64
		sumMerc        atomic.Int64
		nMercObs       atomic.Int64
		sumDirect      atomic.Int64
		nDirectObs     atomic.Int64
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
					nMercObs.Add(1)
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
					nDirectObs.Add(1)
				}
			}
		}
	}()

	runWave := func(arm string, lo, hi int) float64 {
		if lo >= hi {
			return 0
		}
		sem := make(chan struct{}, c)
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
				sample := client.CompleteOneStream(ctx, url, key, bodies[i], arm, i)
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
	for offset, waveIdx := 0, 0; offset < n; waveIdx++ {
		hi := offset + c
		if hi > n {
			hi = n
		}
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

	if int(peakEngine.Load()) > c {
		reason := fmt.Sprintf(
			"refused: engine peak in-flight %d exceeds claimed concurrency %d (dual-load bug)",
			int(peakEngine.Load()), c)
		merc = GatewayParityLevelResult{Arm: "merc", Concurrency: c, RequestsAttempted: n, Status: "REFUSED", Reason: reason, PeakInFlight: int(peakMerc.Load()), RawSamples: mercSamples}
		direct = GatewayParityLevelResult{Arm: "direct", Concurrency: c, RequestsAttempted: n, Status: "REFUSED", Reason: reason, PeakInFlight: int(peakDirect.Load()), RawSamples: directSamples}
		return merc, direct
	}

	meanMerc, nObsM := 0.0, int(nMercObs.Load())
	if nObsM > 0 {
		meanMerc = float64(sumMerc.Load()) / float64(nObsM)
	}
	meanDirect, nObsD := 0.0, int(nDirectObs.Load())
	if nObsD > 0 {
		meanDirect = float64(sumDirect.Load()) / float64(nObsD)
	}
	merc = finalizeGatewayParitySamples("merc", c, n, mercSamples, mercWall, meanMerc, int(peakMerc.Load()), nObsM)
	direct = finalizeGatewayParitySamples("direct", c, n, directSamples, directWall, meanDirect, int(peakDirect.Load()), nObsD)
	return merc, direct
}

// RunGatewayParityMatrix runs every selected cell and returns results in order.
func RunGatewayParityMatrix(
	ctx context.Context,
	mercURL, mercKey, directURL, directKey string,
	baseContract GatewayParitySamplingContract,
	selection GatewayParityMatrixSelection,
) []GatewayParityCellResult {
	out := make([]GatewayParityCellResult, 0, len(selection.Selected))
	for _, spec := range selection.Selected {
		floor := GatewayParitySampleFloor(spec.Concurrency)
		n := floor + floor/10 + 2
		out = append(out, RunGatewayParityMatrixCell(
			ctx, mercURL, mercKey, directURL, directKey, baseContract, spec, n,
		))
	}
	return out
}

// BuildGatewayParityMatrixReceipt assembles a multi-cell receipt. Every guard
// from the single-shape path still applies; cells are additional structure.
// GatePassed is true only when every selected cell is PASS — a single failing
// or inconclusive cell fails the receipt.
func BuildGatewayParityMatrixReceipt(
	baseContract GatewayParitySamplingContract,
	topology GatewayParityNetworkTopology,
	selection GatewayParityMatrixSelection,
	cells []GatewayParityCellResult,
	budget GatewayParityBudget,
	hostStart, hostEnd GatewayParityHostLoad,
	evidenceClass string,
	notes []string,
) GatewayParityReceipt {
	if evidenceClass == "" {
		evidenceClass = "PARITY_EVIDENCE"
	}
	// Unique concurrencies across selected cells — full ladder still required.
	claimedSet := map[int]bool{}
	var claimed []int
	for _, s := range selection.Selected {
		if !claimedSet[s.Concurrency] {
			claimedSet[s.Concurrency] = true
			claimed = append(claimed, s.Concurrency)
		}
	}
	sort.Ints(claimed)

	// Placeholder client for connection policy text when no live client remains.
	client := NewGatewayParityClient(1)

	axes := FullGatewayParityAxes()
	defs := GatewayParityStateDefinitionsText()
	recNotes := append([]string(nil), notes...)
	recNotes = append(recNotes,
		"matrix mode: each cell is independently gated; a claim about one cell is not a claim about another",
		"state definitions: cold="+truncateForNote(defs.Cold, 160),
		"state definitions: warm="+truncateForNote(defs.Warm, 160),
		"state definitions: prefix-hit="+truncateForNote(defs.PrefixHit, 200),
		"selection rationale: "+selection.Rationale,
		TrafficClassNonActingNote,
	)

	// Build a synthetic levels map for ladder/identity aggregation: use the
	// overhead-shape warm cells when present so legacy fields remain populated
	// for readers that only know single-shape keys. Prefer exact warm p=32 o=16.
	levels := map[string]GatewayParityLevelResult{}
	var primaryIdentity GatewayParityBodyIdentity
	primaryIdentity.Equal = false
	primaryIdentity.Proven = false
	primaryIdentity.Detail = "matrix mode: see cells[].body_identity; top-level body_identity is the strictest cell"
	rawCount := 0
	for _, cell := range cells {
		rawCount += len(cell.Merc.RawSamples) + len(cell.Direct.RawSamples)
		// Populate legacy keys only when no collision (first writer wins) so a
		// multi-shape receipt does not silently overwrite merc@c=1 from a
		// different shape. Prefer overhead warm.
		mk := fmt.Sprintf("merc@c=%d", cell.Spec.Concurrency)
		dk := fmt.Sprintf("direct@c=%d", cell.Spec.Concurrency)
		prefer := cell.Spec.PromptTokens == 32 && cell.Spec.OutputTokens == 16 && cell.Spec.State == "warm"
		if _, exists := levels[mk]; !exists || prefer {
			levels[mk] = cell.Merc
			levels[dk] = cell.Direct
		}
		// Strictest identity: any unproven cell keeps top-level unproven.
		if cell.BodyIdentity.Proven && cell.BodyIdentity.Equal {
			if !primaryIdentity.Proven {
				primaryIdentity = cell.BodyIdentity
				primaryIdentity.Detail = "matrix: at least one cell proven; require ALL cells proven for PARITY_EVIDENCE comparable"
			}
		}
	}
	// For PARITY_EVIDENCE, every MEASURED cell must have proven identity.
	allProven := len(cells) > 0
	for _, cell := range cells {
		if cell.Status != "MEASURED" && cell.Status != "REFUSED" {
			allProven = false
			break
		}
		// REFUSED for state still may lack identity; only MEASURED cells need proof.
		if cell.Status == "MEASURED" && !(cell.BodyIdentity.Proven && cell.BodyIdentity.Equal) {
			allProven = false
			primaryIdentity.Proven = false
			primaryIdentity.Equal = false
			primaryIdentity.Detail = "matrix: not all MEASURED cells have proven Merc-bound body identity; see cells[].body_identity"
			break
		}
	}
	if allProven && evidenceClass == "PARITY_EVIDENCE" {
		// Re-check: every measured cell proven.
		ok := true
		for _, cell := range cells {
			if cell.Status == "MEASURED" && !(cell.BodyIdentity.Proven && cell.BodyIdentity.Equal) {
				ok = false
				break
			}
		}
		if ok {
			primaryIdentity.Proven = true
			primaryIdentity.Equal = true
			primaryIdentity.Detail = "matrix: every MEASURED cell has proven Merc-bound body identity"
		}
	}

	// Aggregate gate from per-cell verdicts.
	agg := GatewayParityGate{
		Version:       gatewayParityGateVersion,
		PrimaryMetric: budget.PrimaryMetric,
		Budget:        budget,
		Basis:         budget.Basis + "; matrix: every selected cell must PASS independently",
		ClaimedLevels: append([]int(nil), claimed...),
		Levels:        make([]GatewayParityGateLevel, 0, len(cells)),
	}
	overall := "PASS"
	allPassed := len(cells) > 0
	if len(cells) == 0 {
		allPassed = false
		overall = "FAIL"
	}
	var cellRefusals []string
	for _, cell := range cells {
		gl := cell.Gate
		// Annotate concurrency field remains; add cell key into refusals for traceability.
		if gl.Verdict != "PASS" || len(gl.Refusals) > 0 || cell.Status == "REFUSED" {
			tagged := make([]string, 0, len(gl.Refusals)+1)
			for _, r := range gl.Refusals {
				tagged = append(tagged, cell.Spec.Key()+": "+r)
			}
			if cell.Status == "REFUSED" && cell.Reason != "" {
				found := false
				for _, r := range tagged {
					if strings.Contains(r, cell.Reason) {
						found = true
						break
					}
				}
				if !found {
					tagged = append(tagged, cell.Spec.Key()+": "+cell.Reason)
				}
			}
			gl.Refusals = tagged
			if gl.Verdict == "PASS" {
				gl.Verdict = "FAIL"
			}
			gl.Passed = false
		}
		gl.Passed = gl.Verdict == "PASS" && len(gl.Refusals) == 0
		if !gl.Passed {
			allPassed = false
		}
		switch {
		case gl.Verdict == "FAIL":
			overall = "FAIL"
		case gl.Verdict == "INCONCLUSIVE" && overall == "PASS":
			overall = "INCONCLUSIVE"
		}
		cellRefusals = append(cellRefusals, gl.Refusals...)
		agg.Levels = append(agg.Levels, gl)
	}
	agg.GatePassed = allPassed && overall == "PASS"
	agg.Verdict = overall
	if !agg.GatePassed && overall == "PASS" {
		agg.Verdict = "FAIL"
	}

	// Also run the legacy multi-level evaluator on the synthetic levels map so
	// ladder incompleteness and shared budget rules still fire at receipt level.
	// Use BuildGatewayParityReceipt then overlay matrix fields — but that would
	// re-evaluate gate from levels only. Instead: call Build for structural
	// refusals with a single-shape projection, then force gate to aggregate.
	rec := BuildGatewayParityReceipt(
		baseContract, topology, client, claimed, levels, primaryIdentity,
		budget, hostStart, hostEnd, evidenceClass, recNotes,
	)
	// Overlay matrix fields. ColdWarmState must not be a blanket "warm".
	rec.ColdWarmState = "per-cell"
	rec.FullAxes = &axes
	rec.SelectedCells = append([]GatewayParityCellSpec(nil), selection.Selected...)
	rec.DroppedAxes = append([]GatewayParityDroppedAxis(nil), selection.DroppedAxes...)
	rec.DroppedSummary = append([]string(nil), selection.DroppedSummary...)
	rec.Cells = cells
	rec.TrafficClassNote = TrafficClassNonActingNote
	stateDefs := GatewayParityStateDefinitionsText()
	rec.StateDefinitions = &stateDefs
	rec.ShapeInsight = ComputeGatewayParityShapeInsight(cells)
	rec.RawSampleCount = rawCount
	rec.Gate = agg
	// Merge cell refusals into top-level refusals (dedup-preserving).
	seen := map[string]bool{}
	for _, r := range rec.Refusals {
		seen[r] = true
	}
	for _, r := range cellRefusals {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		rec.Refusals = append(rec.Refusals, r)
	}
	// Any non-PASS cell refuses the receipt.
	if !agg.GatePassed {
		rec.GatePassed = false
		rec.Comparable = false
		rec.Gate.GatePassed = false
		if rec.RefusalReason == "" {
			if agg.Verdict == "INCONCLUSIVE" {
				rec.RefusalReason = "matrix gate INCONCLUSIVE: at least one cell under-powered or straddling budget"
			} else if len(rec.Refusals) > 0 {
				rec.RefusalReason = rec.Refusals[0]
			} else {
				rec.RefusalReason = "matrix gate FAIL: at least one cell did not PASS"
			}
		}
	}
	// Self-test / incomplete ladder still force non-comparable (Build already did).
	if evidenceClass == "HARNESS_SELF_TEST" {
		rec.GatePassed = false
		rec.Comparable = false
		rec.Gate.GatePassed = false
	}
	if evidenceClass == "INCOMPLETE_LADDER" || rec.EvidenceClass == "INCOMPLETE_LADDER" {
		rec.GatePassed = false
		rec.Comparable = false
		rec.Gate.GatePassed = false
	}
	// PARITY_EVIDENCE still requires the concurrency ladder across selected cells.
	if evidenceClass == "PARITY_EVIDENCE" && !GatewayParityLadderComplete(claimed) {
		rec.EvidenceClass = "INCOMPLETE_LADDER"
		rec.GatePassed = false
		rec.Comparable = false
		rec.Gate.GatePassed = false
		msg := fmt.Sprintf(
			"incomplete concurrency ladder: PARITY_EVIDENCE requires levels %v across selected cells (got %v)",
			gatewayParityRequiredLadder, claimed)
		rec.Refusals = append(rec.Refusals, msg)
		if rec.RefusalReason == "" {
			rec.RefusalReason = msg
		}
	}
	return rec
}

// ComputeGatewayParityShapeInsight derives where relative overhead is largest
// from measured cells. Returns nil when fewer than two measured relatives exist.
func ComputeGatewayParityShapeInsight(cells []GatewayParityCellResult) *GatewayParityShapeInsight {
	insight := &GatewayParityShapeInsight{
		Method: "relative_overhead = merc_ttft_p95_point / direct_ttft_p95_point on MEASURED cells with both percentiles; absolute = merc − direct",
	}
	var bestKey, worstKey string
	var bestRel, worstRel float64
	haveBest, haveWorst := false, false
	for _, cell := range cells {
		note := GatewayParityShapeCellNote{
			Cell:               cell.Spec.Key(),
			Status:             cell.Status,
			GateVerdict:        cell.Gate.Verdict,
			AbsoluteOverheadMs: cell.AbsoluteOverheadMs,
			RelativeOverhead:   cell.RelativeOverhead,
		}
		if cell.Merc.TTFTp95 != nil {
			v := cell.Merc.TTFTp95.Point
			note.MercTTFTp95Ms = &v
		}
		if cell.Direct.TTFTp95 != nil {
			v := cell.Direct.TTFTp95.Point
			note.DirectTTFTp95Ms = &v
		}
		insight.PerCell = append(insight.PerCell, note)
		if cell.RelativeOverhead == nil {
			continue
		}
		rel := *cell.RelativeOverhead
		if !haveBest || rel > bestRel {
			bestRel, bestKey, haveBest = rel, cell.Spec.Key(), true
		}
		if !haveWorst || rel < worstRel {
			worstRel, worstKey, haveWorst = rel, cell.Spec.Key(), true
		}
	}
	if haveBest {
		insight.HighestRelativeCell = bestKey
		insight.HighestRelative = &bestRel
	}
	if haveWorst {
		insight.LowestRelativeCell = worstKey
		insight.LowestRelative = &worstRel
	}
	if haveBest && haveWorst && bestKey != worstKey {
		insight.Finding = fmt.Sprintf(
			"measured: highest relative overhead at %s (%.3f×); lowest at %s (%.3f×). "+
				"Where the ratio is highest, fixed per-request gateway cost is the largest fraction of request cost; "+
				"this ranking is measured from cell ratios, not assumed.",
			bestKey, bestRel, worstKey, worstRel)
	} else if haveBest {
		insight.Finding = fmt.Sprintf(
			"measured relative overhead at %s = %.3f× (insufficient contrast cells for a min/max pair)",
			bestKey, bestRel)
	} else {
		insight.Finding = "no MEASURED cells with both merc and direct TTFT p95; shape insight unavailable"
	}
	return insight
}

func truncateForNote(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SelfTestGatewayParityMatrixSelection is the subset the stand-in self-test runs.
// Covers every new dimension and the required concurrency ladder without the
// full default product (which is wall-time heavy under artificial sleeps).
func SelfTestGatewayParityMatrixSelection() GatewayParityMatrixSelection {
	sel := GatewayParityMatrixSelection{
		Rationale: "self-test subset: exercise prompt/output/state dimensions + concurrency ladder; not a parity claim",
		DroppedSummary: []string{
			"self-test subset (not default matrix): enough to prove the harness across dimensions",
			"full default selection is for live measurement; self-test stays fast and non-comparable",
		},
		DroppedAxes: []GatewayParityDroppedAxis{
			{Axis: "note", Values: []string{"self-test"}, Reason: "thinner than DefaultGatewayParityMatrixSelection; see rationale"},
		},
	}
	add := func(p, o int, state string, conc ...int) {
		for _, c := range conc {
			sel.Selected = append(sel.Selected, GatewayParityCellSpec{
				Concurrency: c, PromptTokens: p, OutputTokens: o, State: state,
			})
		}
	}
	add(32, 16, "warm", 1, 8, 32)
	add(32, 16, "cold", 1)
	add(32, 16, "prefix-hit", 1)
	add(8192, 16, "warm", 1)
	add(32, 512, "warm", 1)
	return sel
}
