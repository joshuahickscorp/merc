package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// quote.go — Plane C / Compute Autopilot (docs/PLANE_C.md): the buyer-facing
// quote. POST /v1/quote takes the same submission shape as /v1/jobs but spends no
// money and creates no job — it SCANS the input, assembles a conservative
// cost/ETA/supply/risk band from machinery that already exists (price estimate,
// adaptive split, queue/worker counts, the model catalogue), persists what it
// believed (quotes table), and returns a structured quote.
//
// Honesty rules (PLANE_C §26, BLACKHOLE): the quote never hides uncertainty,
// never promises an exact cost when the token count is sampled/heuristic, and
// never fakes supply — `eligible_now` is a real count of workers that would pass
// the claim hard filter for THIS job. Where a signal does not exist yet (warm
// model state, per-worker historical throughput), the quote says so in
// `confidence.reasons` and leans conservative, rather than inventing a number.

// --- input scanner (PLANE_C §8) -------------------------------------------------

// fieldSampleN is how many leading valid records we inspect to infer field names.
const fieldSampleN = 512

// bytesPerTokenHeuristic is the documented rune→token divisor for ASCII-ish
// input (Project Detection & Quotation 6->6.5, docs/internal/CREED_AND_PATH_TO_TEN.md).
// It is intentionally crude (≈4 chars/token, the standard rule-of-thumb for
// English-ish BPE tokenization) and surfaced as an estimate, never as an exact
// count — porting the real per-model BPE tokenizer into the control plane (Go)
// would mean vendoring a full tokenizer implementation for a pricing ESTIMATE,
// a heavier dependency than this codebase takes on elsewhere (BLACKHOLE: own
// the trivial, never the treacherous) — deliberately not done here.
const bytesPerTokenHeuristic = 4.0

// nonASCIITokensPerRune is the divisor used instead of bytesPerTokenHeuristic
// when the input is mostly non-ASCII (CJK/Cyrillic/etc.). Real BPE tokenizers
// allocate close to one token per character for these scripts, NOT ~4 — the
// English-text rule of thumb badly undercounts otherwise (a genuine, real bug
// this rung fixes, not a full tokenizer port).
const nonASCIITokensPerRune = 0.9

// estimateTokens applies the rune-based (not byte-based) token heuristic. Using
// RUNES instead of raw BYTES fixes a real undercounting bug for multi-byte UTF-8
// text: a CJK character is 3 bytes but ~1 rune, so a byte-based divisor treats
// it as ~0.75 "tokens" when a real tokenizer is much closer to 1 token per
// character. Mostly-non-ASCII input additionally switches to
// nonASCIITokensPerRune instead of the /4 English-text ratio, since that ratio
// assumes short average token spans that hold for Latin-script text but not for
// CJK/Cyrillic/etc. Pure, no I/O — unit-tested directly.
func estimateTokens(text []byte) int64 {
	asciiCount := 0
	for _, b := range text {
		if b < 128 {
			asciiCount++
		}
	}
	return estimateTokensFromCounts(utf8.RuneCount(text), asciiCount, len(text))
}

// estimateTokensFromCounts is the pure math behind estimateTokens, split out so
// scanJSONL can accumulate rune/ASCII/byte counts incrementally across many
// JSONL lines (avoiding a second full-input buffer copy) and apply the exact
// same heuristic to the aggregate.
func estimateTokensFromCounts(runeCount, asciiCount, byteLen int) int64 {
	if runeCount == 0 || byteLen == 0 {
		return 0
	}
	if float64(asciiCount)/float64(byteLen) < 0.5 {
		return int64(math.Ceil(float64(runeCount) * nonASCIITokensPerRune))
	}
	return int64(math.Ceil(float64(runeCount) / bytesPerTokenHeuristic))
}

// FieldStat is the content-length evidence for one top-level field, measured
// across the sampled records (Project Detection & Quotation 8->9: content-based
// detection). AvgStringLen is the mean length, in runes, of this field's STRING
// values in the sample (a non-string value contributes length 0 — it is not text
// to embed/classify/extract); Occurrences is how many sampled records carried the
// field at all. These are the real numbers behind RecommendedField, surfaced so a
// buyer can see WHY a field was suggested, not just take the suggestion on faith.
type FieldStat struct {
	Field        string  `json:"field"`
	AvgStringLen float64 `json:"avg_string_len"` // mean rune length of this field's string values in the sample
	Occurrences  int     `json:"occurrences"`    // sampled records that carried this field
}

// QuoteInputScan is the preflight scan of a JSONL input (PLANE_C §8). Every figure
// is real; `estimated_tokens` is explicitly a heuristic.
type QuoteInputScan struct {
	Records          int      `json:"records"`          // non-blank JSONL lines
	Bytes            int      `json:"bytes"`            // total input bytes
	EstimatedTokens  int64    `json:"estimated_tokens"` // byte/token heuristic, not exact
	MalformedRecords int      `json:"malformed_records"`
	BlankRecords     int      `json:"blank_records"`   // blank/whitespace lines (skipped, never records)
	SkippedRecords   int      `json:"skipped_records"` // blank + malformed: lines NOT usable as input (item 23)
	FirstBadLine     int      `json:"first_bad_line"`  // 1-based line of the first malformed record; 0 = none
	MaxLineBytes     int      `json:"max_line_bytes"`
	SampledRecords   int      `json:"sampled_records"` // records inspected for field names
	DetectedFields   []string `json:"detected_fields"` // sorted union of top-level keys in the sample
	// RecommendedField is the content-based field-detection SUGGESTION (Project
	// Detection & Quotation 8->9): the top-level string field with the longest
	// average content across the sample — the column most likely to be the text a
	// buyer wants embedded/classified/extracted (a `text`/`body`/`content` column,
	// not an `id`/`label`). Empty when the sample has no string-valued field to
	// recommend (e.g. every record is all-numeric, or there are no valid records).
	// It is a CONFIRMABLE suggestion, never an imposed choice: the quote surfaces it
	// plus the per-field evidence (FieldStats) so a buyer confirms or overrides.
	RecommendedField string `json:"recommended_field,omitempty"`
	// FieldStats is the per-field content-length evidence behind RecommendedField,
	// sorted by AvgStringLen descending (the recommendation is FieldStats[0] when
	// non-empty). Surfaced so the suggestion is transparent and auditable.
	FieldStats []FieldStat `json:"field_stats,omitempty"`
}

// scanJSONL walks JSONL bytes once and reports a real preflight scan: record
// count, byte count, malformed count + first bad line, max line size, a token
// heuristic, and the union of top-level field names across the first `fieldSampleN`
// valid records. Pure — no I/O, fully unit-tested.
func scanJSONL(data []byte) QuoteInputScan {
	scan := QuoteInputScan{}
	fields := map[string]bool{}
	// Per-field content-length accumulation (Project Detection & Quotation 8->9):
	// summed rune length of each field's STRING values + how many sampled records
	// carried it, so a longest-average-string heuristic can recommend the text
	// field a buyer most likely wants embedded/classified/extracted.
	fieldStrLen := map[string]int{}
	fieldOccur := map[string]int{}
	lineNo := 0
	// Accumulated incrementally across every non-blank line — avoids copying the
	// whole input into a second buffer just to re-scan it for the token estimate.
	totalRunes, totalASCII := 0, 0
	for _, raw := range bytes.Split(data, []byte("\n")) {
		lineNo++
		ln := bytes.TrimRight(raw, "\r")
		if len(bytes.TrimSpace(ln)) == 0 {
			scan.BlankRecords++
			continue // blank line carries no record
		}
		scan.Records++
		scan.Bytes += len(ln)
		totalRunes += utf8.RuneCount(ln)
		for _, b := range ln {
			if b < 128 {
				totalASCII++
			}
		}
		if len(ln) > scan.MaxLineBytes {
			scan.MaxLineBytes = len(ln)
		}
		if !json.Valid(ln) {
			scan.MalformedRecords++
			if scan.FirstBadLine == 0 {
				scan.FirstBadLine = lineNo
			}
			continue
		}
		if scan.SampledRecords < fieldSampleN {
			var obj map[string]json.RawMessage
			if json.Unmarshal(ln, &obj) == nil {
				for k, v := range obj {
					fields[k] = true
					fieldOccur[k]++
					// Only STRING values are candidate text to embed/classify/extract;
					// a numeric/bool/object/array value contributes 0 to the string
					// length (it is not the text column). strconv.Unquote decodes the
					// JSON string to its real content so escapes/unicode count as their
					// actual rune length, not the escaped byte length.
					if s, ok := jsonStringValue(v); ok {
						fieldStrLen[k] += utf8.RuneCountInString(s)
					}
				}
			}
			scan.SampledRecords++
		}
	}
	// Records used vs skipped (item 23): a blank line is no record, a malformed line is a
	// record that cannot be processed; both are surfaced so the quote is honest about how
	// much of the input is actually usable.
	scan.SkippedRecords = scan.BlankRecords + scan.MalformedRecords
	scan.EstimatedTokens = estimateTokensFromCounts(totalRunes, totalASCII, scan.Bytes)
	scan.DetectedFields = sortedKeys(fields)
	scan.FieldStats, scan.RecommendedField = recommendField(fields, fieldStrLen, fieldOccur)
	return scan
}

// jsonStringValue decodes a top-level JSON field value to its string content,
// reporting ok=false for any non-string value (number/bool/null/object/array) so
// only real text columns contribute to the field-length heuristic. Trims leading
// whitespace before the type check because encoding/json leaves raw messages
// verbatim.
func jsonStringValue(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", false
	}
	return s, true
}

// recommendField picks the top-level field with the longest AVERAGE string
// content across the sample (the longest-string heuristic, Project Detection &
// Quotation 8->9) as the confirmable "which column is the text" suggestion, and
// returns the full per-field evidence sorted by that average descending. A field
// with no string content in the sample (avg 0) is never recommended — but it is
// still listed in FieldStats (avg 0) so the buyer sees every candidate. Ties on
// average length break deterministically by field name so the output is stable.
// Returns ("", nil-ish) cleanly when there are no fields at all. Pure —
// unit-tested without a DB.
func recommendField(fields map[string]bool, strLen, occur map[string]int) ([]FieldStat, string) {
	if len(fields) == 0 {
		return nil, ""
	}
	stats := make([]FieldStat, 0, len(fields))
	for f := range fields {
		n := occur[f]
		avg := 0.0
		if n > 0 {
			avg = float64(strLen[f]) / float64(n)
		}
		stats = append(stats, FieldStat{Field: f, AvgStringLen: avg, Occurrences: n})
	}
	// Sort by avg string length DESC, then field name ASC for a stable tiebreak.
	// Small N (top-level field count) — insertion sort keeps this dependency-free
	// and deterministic, matching sortedKeys' own convention above (no "sort"
	// import for a handful of keys).
	less := func(a, b FieldStat) bool {
		if a.AvgStringLen != b.AvgStringLen {
			return a.AvgStringLen > b.AvgStringLen
		}
		return a.Field < b.Field
	}
	for i := 1; i < len(stats); i++ {
		for j := i; j > 0 && less(stats[j], stats[j-1]); j-- {
			stats[j-1], stats[j] = stats[j], stats[j-1]
		}
	}
	recommended := ""
	if stats[0].AvgStringLen > 0 {
		recommended = stats[0].Field
	}
	return stats, recommended
}

// sortedKeys(map[string]bool) is now served by the generic sortedKeys[V any] in
// audit.go (folded in from the buyer CLI); the map[string]bool caller above binds
// it directly. One deterministic sorted-keys helper for the whole cx binary.

// --- quote object (PLANE_C §7) --------------------------------------------------

// Quote is the central Plane C object returned by POST /v1/quote.
//
// QuoteID is the buyer-facing handle, shaped "q_<uuid>". The bare <uuid> is the
// quotes.id primary key (id is a real UUID column), so a buyer can bind a later
// submission by passing back either the "q_" handle or the bare uuid (the bind
// path strips the prefix — see quoteIDToUUID). ExpiresAt bounds how long the quote
// stays bindable (Plane D D7); InputSHA256 lets the submit path best-effort confirm
// the bytes match what was quoted. bareID carries the parsed uuid to InsertQuote
// without putting a redundant field on the wire.
type Quote struct {
	QuoteID string `json:"quote_id"`
	JobType string `json:"job_type"`
	Model   string `json:"model"`
	Tier    string `json:"tier"`
	// TierSemantics prevents a price multiplier from being mistaken for capacity
	// reservation, wider fan-out, or an SLA. A separately populated SLA block is
	// the only completion guarantee.
	TierSemantics string               `json:"tier_semantics"`
	Input         QuoteInputScan       `json:"input"`
	AudioInput    *AudioUploadMetadata `json:"audio_input,omitempty"`
	Execution     QuoteExecution       `json:"execution"`
	Cost          QuoteCost            `json:"cost"`
	Time          QuoteTime            `json:"time"`
	Confidence    QuoteConfidence      `json:"confidence"`
	Budget        QuoteBudget          `json:"budget"`
	Warnings      []string             `json:"warnings"`
	ExpiresAt     time.Time            `json:"expires_at"`   // quote stops being bindable after this (Plane D D7)
	InputSHA256   string               `json:"input_sha256"` // sha256 of the scanned input bytes (best-effort submit match)
	// PrivatePoolAttestation is the written guarantee of what "private" actually
	// means (Buyer advantage & pricing edge 6->7: "Productize the privacy premium
	// instead of leaving it a sentence") — populated only when sub.PrivatePool is
	// set, empty ("") otherwise. See privatePoolAttestation for the exact text.
	PrivatePoolAttestation string `json:"private_pool_attestation,omitempty"`
	// SLA is the wall-clock speed-SLA OFFER (Speed Lane wave 2A): a guaranteed
	// completion time derived from the fan-out planner's CONSERVATIVE band, priced
	// at a documented premium that is auto-refunded on a miss. nil whenever any
	// honesty precondition fails (thin supply, planner disabled, no measured
	// rates, un-priceable premium) — see deriveQuoteSLA. It binds only when the
	// submission binds this quote with firm_quote.
	SLA *QuoteSLA `json:"sla,omitempty"`
	// Routing is the substrate-routing decision (Speed Lane road-to-ten rubric
	// dimension 4, routing.go): which substrate this job's SHAPE favors —
	// fleet, a lit GPU lane, or an honest GPU recommendation — with the
	// measured basis and the compared numbers stated plainly. Present only for
	// GENERATIVE jobs with records > 0: the 2026-07-06 A100 vLLM sweep the
	// decision is grounded in measured generative decode only, so any other
	// job shape gets NO routing block rather than an unmeasured guess.
	Routing *QuoteRouting `json:"routing,omitempty"`
	// Economics is the complete versioned margin-guard decision. It exposes the
	// schedule, frozen per-task amounts, reserve, scenarios, and minimum headroom
	// needed to reproduce why this quote is or is not executable.
	Economics EconomicPlan `json:"economics"`

	bareID uuid.UUID // quotes.id primary key (the <uuid> inside QuoteID); not on the wire
}

// QuoteRouting is the wire form of routing.go's SubstrateDecision: the
// substrate the job's shape favors, the plain-english why, the two numbers
// that were compared, and the basis naming the sweep artifact the GPU figure
// was modeled from (gpu_modeled_secs is ALWAYS [MODELED] — the measured
// sweep's aggregate tok/s interpolated at this job's shape, excluding
// rental/provisioning time — never a measurement of this job).
type QuoteRouting struct {
	Substrate      string  `json:"substrate"` // fleet | gpu_lane | gpu_recommend
	Reason         string  `json:"reason"`
	FleetETASecs   int     `json:"fleet_eta_secs"`
	GPUModeledSecs float64 `json:"gpu_modeled_secs"`
	Basis          string  `json:"basis"`
}

func serviceTierSemantics(tier string) string {
	switch tier {
	case "priority":
		return "bounded queue preference per eligible worker; after three consecutive ordinary priority claims, one eligible batch opportunity is served; no device reservation, wider-fanout guarantee, or SLA"
	case "trusted":
		return "restricts execution to the trusted supplier tier; it is not queue priority, device reservation, wider fan-out, or an SLA"
	default:
		return "standard queue service with a bounded batch opportunity under priority contention; no device reservation, wider-fanout guarantee, or SLA"
	}
}

// quoteRoutingBasis names the measured artifact behind every routing block's
// GPU figure, with the honesty label attached to the number itself.
const quoteRoutingBasis = "gpu_modeled_secs [MODELED] from the measured 2026-07-06 A100-SXM4-80GB vLLM sweep: docs/speed-lane-reports/A100_CAPABILITY_SWEEP.md (raw: artifacts/a100-sxm-capability-sweep-2026-07-06.jsonl)"

// quoteTTL is how long an advisory quote stays bindable to a submission (Plane D
// D7). A submission carrying an expired quote_id is rejected 409.
const quoteTTL = 15 * time.Minute

type QuoteExecution struct {
	RecommendedSplitSize int     `json:"recommended_split_size"`
	EstimatedTasks       int     `json:"estimated_tasks"`
	EligibleWorkersNow   int     `json:"eligible_workers_now"`
	WarmEligibleWorkers  int     `json:"warm_eligible_workers"` // eligible workers that ALSO have the model warm (warm-routing, D3)
	ModelMinMemoryGB     float32 `json:"model_min_memory_gb"`   // catalogue floor; the per-task memory requirement
	OOMRisk              string  `json:"oom_risk"`              // low|medium|high
	ColdStartRisk        string  `json:"cold_start_risk"`       // low|medium|high
	SLAEligible          bool    `json:"sla_eligible"`          // supply >= threshold → a project-SLA ETA is offerable (research §6.2 launch gate)
	PoolReputation       float64 `json:"pool_reputation"`       // avg reputation (0..1) of the eligible supplier pool (routing transparency, research §4)
	// PrivatePool/PrivatePoolMemberCount (Buyer advantage & pricing edge 6->7):
	// whether this quote priced a private-pool submission, and how many suppliers
	// are actually bound to this buyer's pool right now — routing transparency for
	// the premium the buyer is being asked to pay (0 members means the job could
	// never be claimed; see createJob's guard).
	PrivatePool            bool `json:"private_pool"`
	PrivatePoolMemberCount int  `json:"private_pool_member_count,omitempty"`
}

// privatePoolPremiumRate is the real price of the privacy premium (Buyer advantage
// & pricing edge 6->7, docs/internal/CREED_AND_PATH_TO_TEN.md: "Productize the
// privacy premium instead of leaving it a sentence"): a private_pool job routes
// ONLY to the buyer's own bound suppliers instead of the full shared pool, which
// structurally shrinks the eligible supply and removes the scheduler's normal
// freedom to pick the cheapest/fastest/warmest candidate — a real cost the buyer
// should see priced, not a free flag. 25% is a deliberately explainable round
// number (not reverse-engineered from a hidden formula), applied to the same
// `expected` estimator every other cost line uses, so it moves with real pricing
// (Buyer advantage & pricing edge 4.5->5's repriced catalogue) instead of being a
// second, independent hand-typed constant.
const privatePoolPremiumRate = 0.25

// privatePoolAttestation is the WRITTEN guarantee of what "private" actually
// means, shown on every private-pool quote (the rung's explicit requirement: "a
// clear price premium AND a written attestation of what 'private' actually
// guarantees"). Kept honest and narrow — it states exactly what the dispatch
// filter enforces (control/scheduler.go's private_pool_members EXISTS clause),
// nothing more: only bound suppliers can claim the job; it does not claim
// encryption, geographic isolation, or any property the platform does not
// actually implement.
const privatePoolAttestation = "Private pool: this job's tasks are claimable ONLY by the suppliers you have explicitly bound via POST /v1/private-pool (enforced by the control plane's dispatch filter, not merely a stated policy) — no other supplier on the exchange, however otherwise eligible, can ever claim a task from this job. This guarantees WHO runs your work; it does not by itself add encryption-at-rest, network isolation, or any other property beyond supplier selection."

// slaMinEligibleWorkers is the minimum eligible supply before a project-priced SLA ETA
// is offered (DEEP_RESEARCH_V2 §6.2 launch gate; "probably 5-10"). Below it the quote is
// advisory only — the failure mode (SLA miss, no revenue, lost buyer) is worse than
// declining to promise.
const slaMinEligibleWorkers = 5

// --- wall-clock speed-SLA quote (Speed Lane wave 2A,
// --- docs/speed-lane-reports/SLA_QUOTE_WAVE2A.md) --------------------------------
//
// The fan-out planner (wave 1B, planner.go) turned the ETA from a blunt
// queue/workers aggregate into a modeled makespan over the fleet's REAL measured
// per-worker rates, with a conservative band (every rate degraded to 75% of
// measured — plannerConservativeRateFactor's own documented grounding). Wave 2A
// turns that conservative band into a product no GPU rental offers: a GUARANTEED
// completion time with an automatic, deterministic remedy on a miss. The
// guarantee is offered ONLY when every honesty precondition holds (see
// deriveQuoteSLA); a fleet the planner cannot model never gets a promise.

const (
	// slaPremiumRate is the documented surcharge an SLA quote carries: 15% of the
	// quote's expected cost. Deliberately an explainable round number (the same
	// discipline as privatePoolPremiumRate's 25%), not reverse-engineered from a
	// hidden risk model: it is the price of the platform underwriting the
	// guarantee, and it is EXACTLY what the buyer gets back on a miss — the
	// remedy is the premium, so the surcharge and the remedy can never drift
	// apart. Applied to the same `expected` estimator every other cost line uses.
	slaPremiumRate = 0.15

	// slaSafetyMarginFactor is the explicit margin applied ON TOP of the
	// planner's conservative band. The band already covers measured rate spread +
	// thermal degradation (rates at 75% of measured); this margin covers what the
	// band structurally cannot see at quote time: claim-poll latency between
	// dispatch and pickup, integer task granularity (a chunk is indivisible — the
	// last chunk's remainder rounds the makespan up), and queue arrivals between
	// quote time and submit time (the quote's queue-depth snapshot is not a
	// reservation; the quote stays bindable for quoteTTL = 15 min). Combined with
	// the 75% rate degradation this plans at 1/0.75 × 1.25 ≈ 1.67× of the
	// measured-rate p50 — above the measured 1.58× sustained-vs-peak derating
	// (sustainedDeratingFactor), so a long batch job that throttles is still
	// inside the guarantee by construction.
	slaSafetyMarginFactor = 1.25

	// slaMergeAllowanceSecs covers the tail the planner never models: the
	// guarantee clock stops at results_merged_at (the buyer-visible artifact),
	// which lands AFTER the last commit — the merge itself (mergeJobResults
	// buffers + writes the artifact) plus, when the last commit's synchronous
	// finalize does not fire (e.g. the final task resolved through a background
	// path), one full job-finalize cadence (workers.go finalizationInterval = 20s)
	// before finalizeJobs merges instead. 60s = 3× that sweep cadence — a
	// deliberate allowance, not a measured number, and labeled as such.
	slaMergeAllowanceSecs = 60
)

// slaRemedyText is the written remedy shown on every SLA quote: exactly what
// the enforcement path (collect.go settleSLAOutcome) actually does, nothing more.
const slaRemedyText = "If your job's results are not merged within guaranteed_secs of submission, the SLA premium is refunded automatically as a ledger credit (netted off the amount collected) and the miss is recorded on the job timeline. The job always runs to completion — a miss triggers the refund, never a kill. Your existing partial-settle rights (pay only for completed tasks) are unchanged."

// QuoteSLA is the wall-clock guarantee OFFER attached to a quote when the fleet
// supports one (Speed Lane wave 2A). Every term of the formula is surfaced so
// the buyer can audit the guarantee's construction:
//
//	guaranteed_secs = ceil(conservative_model_secs × safety_margin_factor)
//	                  + merge_allowance_secs
//
// conservative_model_secs is the planner's conservative makespan band (rates
// degraded to 75% of measured — a MODELED figure from measured inputs, labeled
// as such). The guarantee binds only when the submission binds this quote with
// firm_quote (see createJob); the clock runs submit → results merged.
type QuoteSLA struct {
	GuaranteedSecs        int     `json:"guaranteed_secs"`
	PremiumUSD            float64 `json:"premium_usd"`
	ConservativeModelSecs int     `json:"conservative_model_secs"` // planner conservative band [MODELED from measured rates]
	SafetyMarginFactor    float64 `json:"safety_margin_factor"`
	MergeAllowanceSecs    int     `json:"merge_allowance_secs"`
	Remedy                string  `json:"remedy"`
}

// slaGuaranteedSecs is the guarantee formula, pure: the planner's conservative
// band inflated by the explicit safety margin, plus the merge/collect
// allowance. 0 in → 0 out (no band, no guarantee). Unit-tested directly.
func slaGuaranteedSecs(conservativeSecs int) int {
	if conservativeSecs <= 0 {
		return 0
	}
	return int(math.Ceil(float64(conservativeSecs)*slaSafetyMarginFactor)) + slaMergeAllowanceSecs
}

// deriveQuoteSLA decides whether a quote may carry a time guarantee, pure and
// conservative: every precondition is an HONESTY gate, and failing any one of
// them returns nil — the quote stays advisory, exactly as before this wave.
//
//   - slaEligible: the live eligible supply cleared slaMinEligibleWorkers. A
//     thin pool cannot absorb a straggler; no guarantee.
//   - plannerBacked: the ETA actually came from the planner over ≥
//     plannerMinFleetSamples REAL measured rates (etaBandSecs, api.go). When the
//     planner is disabled (CX_DISABLE_FANOUT_PLANNER) or the rate cache is thin,
//     there is no measured basis for a promise — no guarantee, never a guess.
//   - conservativeSecs > 0: a degenerate plan (empty job) guarantees nothing.
//   - premium > 0: a guarantee whose remedy rounds to $0.00 is not a real
//     commitment; a quote too small to price a premium on gets no guarantee.
func deriveQuoteSLA(slaEligible, plannerBacked bool, conservativeSecs int, expectedUSD float64) *QuoteSLA {
	if !slaEligible || !plannerBacked || conservativeSecs <= 0 {
		return nil
	}
	premium := roundUSD(expectedUSD * slaPremiumRate)
	if premium <= 0 {
		return nil
	}
	return &QuoteSLA{
		GuaranteedSecs:        slaGuaranteedSecs(conservativeSecs),
		PremiumUSD:            premium,
		ConservativeModelSecs: conservativeSecs,
		SafetyMarginFactor:    slaSafetyMarginFactor,
		MergeAllowanceSecs:    slaMergeAllowanceSecs,
		Remedy:                slaRemedyText,
	}
}

// sustainedThroughputGap is the REAL, MEASURED steady-state-vs-peak throughput
// gap on fanless/thermally-constrained Apple Silicon (Thermal 6->7,
// docs/internal/CREED_AND_PATH_TO_TEN.md; docs/GPU_CAPABILITY.md's
// "Sustained vs. peak" section, Implementation Log entry 52): a single real M3
// Pro run over 8 minutes measured peak 173.3 tok/s but a sustained mean (last 25%
// of windows, the steady-state regime) of 109.8 tok/s — a 36.6% drop. The static
// per-task ETA target (targetTaskSecs, api.go's jobTypeThroughput) is derived from
// the PEAK figure the business quotes, so for a long batch job that actually runs
// for minutes the peak-derived ETA is optimistic by exactly this gap. This is the
// published number, transcribed with its source, not an invented one.
const sustainedThroughputGap = 0.366

// sustainedDeratingFactor converts a PEAK-derived duration into a SUSTAINED one:
// if the machine actually runs at (1-gap) of peak, the same work takes 1/(1-gap)
// longer. At a 36.6% gap that is ~1.577×. Applied only to the peak-derived static
// ETA target for long batch jobs (never to an ETA already driven by real observed
// durations — that history already reflects whatever sustained pace it ran at).
const sustainedDeratingFactor = 1.0 / (1.0 - sustainedThroughputGap)

// sustainedETAThresholdSecs is how long a peak-derived batch-job ETA must be
// before the sustained derating applies. Short jobs (seconds) finish inside the
// peak regime — the throttle-induced drop only shows up after minutes of
// sustained load (docs/GPU_CAPABILITY.md: the drop began ~5.7 min in), so a job
// the peak estimate already puts under a few minutes is honestly quoted at peak.
// 120s is deliberately conservative (well under the observed ~5.7-min onset) so
// the derating engages before, not after, a job is long enough to actually
// throttle — an ETA that is a little long is a far better miss than one that is
// short.
const sustainedETAThresholdSecs = 120

// sustainedBatchETASecs returns the p50 ETA honestly adjusted for the sustained
// throughput gap. It derates ONLY when ALL of: the tier is "batch" (the rung's
// "long BATCH jobs" scope — priority/trusted are latency tiers, not the
// minutes-long sustained-throughput regime), the ETA came from the PEAK-derived
// static target rather than real observed history (usedObservedHistory==false —
// real durations already embody the machine's actual sustained pace, so derating
// them again would double-count), and the peak ETA is already long enough to run
// into the throttle regime (>= sustainedETAThresholdSecs). Otherwise the peak p50
// is returned unchanged. Pure — unit-tested without a DB.
func sustainedBatchETASecs(peakP50Secs int, tier string, usedObservedHistory bool) int {
	if tier != "batch" || usedObservedHistory || peakP50Secs < sustainedETAThresholdSecs {
		return peakP50Secs
	}
	adjusted := int(math.Ceil(float64(peakP50Secs) * sustainedDeratingFactor))
	if adjusted < peakP50Secs {
		adjusted = peakP50Secs // never shorten (defensive; the factor is >1)
	}
	return adjusted
}

type QuoteCost struct {
	MinUSD      float64 `json:"min_usd"`
	ExpectedUSD float64 `json:"expected_usd"`
	MaxUSD      float64 `json:"max_usd"`
	// VerificationOverheadUSD is the buyer-facing incremental guarded charge for
	// the initial redundancy/honeypot tasks. It is not merely their raw catalogue
	// compute input: processor, control-plane, and configured margin protection
	// apply to verification work just as they do to primary work. It is already
	// included in ExpectedUSD and MaxUSD; buyers must not add it a second time.
	VerificationOverheadUSD float64 `json:"verification_overhead_usd"`
	// PlatformTakeUSD is the gross amount retained from the initial buyer charge
	// after frozen supplier payout. Under the margin guard it includes modeled
	// processor/control costs plus required margin; it is neither a flat-rate cut
	// nor net profit. Actual realized margin is reconciled from ledger/Stripe facts.
	PlatformTakeUSD float64 `json:"platform_take_usd"`
	// PrivatePoolPremiumUSD is the real price of the privacy premium (Buyer
	// advantage & pricing edge 6->7): already folded into ExpectedUSD/MinUSD/MaxUSD
	// above, and broken out here so the buyer can see exactly what "private" costs
	// them, not just a sentence promising it exists. 0 for a non-private-pool quote.
	PrivatePoolPremiumUSD float64 `json:"private_pool_premium_usd,omitempty"`
}

type QuoteTime struct {
	P50Secs       int `json:"p50_secs"`
	P90Secs       int `json:"p90_secs"`
	WorstCaseSecs int `json:"worst_case_secs"`
}

type QuoteConfidence struct {
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

type QuoteBudget struct {
	SuggestedMaxUSD       float64 `json:"suggested_max_usd"`
	CancelBeforeExceeding bool    `json:"cancel_before_exceeding"`
}

// CompositeQuote is the quote for a detected MULTI-STAGE pipeline (item 4): the per-stage
// quotes plus an honest aggregate — total cost (the cap a buyer should set), an ETA BAND
// (best-case parallel to sequential worst case), the worst-stage confidence, and the worst
// risk across stages.
type CompositeQuote struct {
	Stages        []Quote         `json:"stages"`          // per-stage quote (per-stage cost)
	TotalCost     QuoteCost       `json:"total_cost"`      // summed across stages (the total cap)
	TimeBand      QuoteTime       `json:"time_band"`       // p50 = best-case parallel, worst_case = sequential
	Confidence    QuoteConfidence `json:"confidence"`      // worst-stage score + the union of reasons
	OOMRisk       string          `json:"oom_risk"`        // worst across stages
	ColdStartRisk string          `json:"cold_start_risk"` // worst across stages
	Warnings      []string        `json:"warnings"`
}

// composeQuotes aggregates per-stage quotes into a CompositeQuote (item 4). Cost is the
// SUM (the realistic total cap). The ETA band spans best-case parallel (the slowest
// stage's p50/p90, since fan-out stages run as concurrent jobs) to sequential worst case
// (the sum of worst cases). Confidence is the WORST stage's score; risk is the WORST
// across stages. Pure — never inflates and never hides a stage's uncertainty.
func composeQuotes(stages []Quote) CompositeQuote {
	var cost QuoteCost
	var band QuoteTime
	conf := QuoteConfidence{Score: 1.0}
	oom, cold := "low", "low"
	var warnings []string
	seenReason := map[string]bool{}
	for _, q := range stages {
		cost.MinUSD += q.Cost.MinUSD
		cost.ExpectedUSD += q.Cost.ExpectedUSD
		cost.MaxUSD += q.Cost.MaxUSD
		cost.VerificationOverheadUSD += q.Cost.VerificationOverheadUSD
		cost.PlatformTakeUSD += q.Cost.PlatformTakeUSD
		if q.Time.P50Secs > band.P50Secs {
			band.P50Secs = q.Time.P50Secs
		}
		if q.Time.P90Secs > band.P90Secs {
			band.P90Secs = q.Time.P90Secs
		}
		band.WorstCaseSecs += q.Time.WorstCaseSecs
		if q.Confidence.Score < conf.Score {
			conf.Score = q.Confidence.Score
		}
		for _, rs := range q.Confidence.Reasons {
			if !seenReason[rs] {
				seenReason[rs] = true
				conf.Reasons = append(conf.Reasons, rs)
			}
		}
		oom = worseRisk(oom, q.Execution.OOMRisk)
		cold = worseRisk(cold, q.Execution.ColdStartRisk)
		warnings = append(warnings, q.Warnings...)
	}
	if len(stages) == 0 {
		conf.Score = 0
	}
	return CompositeQuote{
		Stages: stages, TotalCost: cost, TimeBand: band, Confidence: conf,
		OOMRisk: oom, ColdStartRisk: cold, Warnings: warnings,
	}
}

// worseRisk returns the higher of two low|medium|high risk levels (unknown → low).
func worseRisk(a, b string) string {
	rank := map[string]int{"low": 0, "medium": 1, "high": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Executable quotes derive their gross retained amount from the same frozen
// buyer-charge and supplier-payout amounts settlement will use. The legacy flat
// platformTakeRate below is only a diagnostic fallback for a blocked plan; public
// handlers refuse such a quote, so it is never an executable price promise.

func (s *Server) quoteInitialEconomicTaskCount(ctx context.Context, sub jobSubmit, primaryTasks int) (int, error) {
	if primaryTasks <= 0 {
		return 0, nil
	}
	redundancy := fracCount(primaryTasks, sub.Verification.RedundancyFrac)
	honeypots := fracCount(primaryTasks, sub.Verification.HoneypotFrac)
	if !sub.Verification.SkipVerificationFloor &&
		sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0 && honeypots == 0 {
		honeypots = 1
	}
	if honeypots > 0 {
		available, err := s.store.AvailableSeedHoneypots(ctx, sub.JobType.Type, sub.Model.Ref, sub.JobType.MaxTokens, honeypots)
		if err != nil {
			return 0, err
		}
		honeypots = len(available)
	}
	return primaryTasks + redundancy + honeypots, nil
}

func (s *Server) buildQuoteWithSchedule(ctx context.Context, buyerID uuid.UUID, sub jobSubmit, inputBytes []byte, schedule EconomicSchedule) Quote {
	jobType := sub.JobType.Type
	tier := sub.Tier
	if err := validateAudioAdmissionInput(sub, inputBytes); err != nil {
		planInput := EconomicPlanInput{}
		return Quote{
			JobType:       jobType,
			Model:         sub.Model.Ref,
			Tier:          tier,
			TierSemantics: serviceTierSemantics(tier),
			Warnings:      []string{"audio admission blocked: " + err.Error()},
			Economics:     blockedEconomicPlan(planInput, schedule, "audio admission: "+err.Error()),
		}
	}
	scan := scanJSONL(inputBytes)

	avgLineBytes := 0.0
	if scan.Records > 0 {
		avgLineBytes = float64(len(inputBytes)) / float64(scan.Records)
	}
	split := adaptiveSplitSize(jobType, sub.Params, avgLineBytes)
	// Speed Lane wave 2A (closing wave 1B's §7 follow-up): the quote's split-size
	// estimate used to stay on the STATIC map while the submit path had already
	// moved to live-fleet sizing (adaptiveSplitSizeLive, api.go) — so a quote's
	// task-count could differ from the submit's reality for the same input. The
	// quote now runs the SAME live refinement, with the EXACT record count (the
	// quote scanned the whole input, so the planner width floor applies honestly).
	// An explicit params.split_size still wins, exactly like the submit path.
	if !hasExplicitSplitSize(sub.Params) {
		split = s.adaptiveSplitSizeLive(ctx, jobType, sub.Model.Ref,
			sub.Constraints.MinMemoryGB, sub.JobType.MaxTokens, avgLineBytes, split, scan.Records)
	}
	tasks := 0
	if scan.Records > 0 && split > 0 {
		tasks = (scan.Records + split - 1) / split
	}

	// Expected cost via the SAME estimator the submission uses; band it for honest
	// uncertainty (a sampled token estimate + variable verification/retry overhead).
	// jobType + max_tokens drive the generative output-token cost term (Project
	// Detection & Quotation 6->6.5): a batch_infer/json_extraction quote now moves
	// with max_tokens, exactly as the eventual submission's charge will.
	expected := s.estimateSubmissionUSD(ctx, sub, len(inputBytes), scan.Records)
	verifOverhead := roundUSD(expected * float64(sub.Verification.RedundancyFrac+sub.Verification.HoneypotFrac))
	// Verification floor (Verification & Result Trust 5->6,
	// docs/internal/CREED_AND_PATH_TO_TEN.md): createJob unconditionally floors the
	// honeypot count to 1 real extra task when the buyer submits with no explicit
	// verification fractions (and no opt-out) — see api.go's wantVerificationFloor.
	// A quote built from the SAME sub with both fractions still at their zero
	// default must not understate that: replicate the exact floor-detection logic
	// here so verifOverhead reflects at least one honeypot task's worth of cost,
	// the same floor the eventual submission will actually pay for.
	wantVerificationFloor := !sub.Verification.SkipVerificationFloor &&
		sub.Verification.RedundancyFrac <= 0 && sub.Verification.HoneypotFrac <= 0
	if wantVerificationFloor && tasks > 0 && fracCount(tasks, sub.Verification.HoneypotFrac) == 0 {
		// One extra honeypot task at the average per-task cost — the same real
		// floor createJob applies, priced rather than left at zero.
		verifOverhead = roundUSD(math.Max(verifOverhead, expected/float64(tasks)))
	}
	platformTake := roundUSD((expected + verifOverhead) * platformTakeRate)

	// Private-pool premium (Buyer advantage & pricing edge 6->7): a real, quoted
	// price for routing ONLY to the buyer's own bound suppliers, folded into every
	// downstream cost figure so MinUSD/MaxUSD/Budget all already reflect it (never
	// a separate number the buyer has to remember to add). privatePoolMemberCount
	// is looked up here — a buyer with zero bound suppliers still gets an honestly
	// priced quote (this is advisory pricing, not the hard guard; createJob is
	// where a zero-member private_pool submission is actually refused).
	var privatePoolPremium float64
	var privatePoolMemberCount int
	if sub.PrivatePool {
		privatePoolMemberCount, _ = s.store.PrivatePoolMemberCount(ctx, buyerID)
		privatePoolPremium = roundUSD(expected * privatePoolPremiumRate)
		expected = roundUSD(expected + privatePoolPremium)
	}
	initialEconomicTasks, economicCountErr := s.quoteInitialEconomicTaskCount(ctx, sub, tasks)
	baseComputeUSD := expected
	if tasks > 0 && initialEconomicTasks > 0 {
		baseComputeUSD = roundEconomicUSD(expected * float64(initialEconomicTasks) / float64(tasks))
		verifOverhead = roundEconomicUSD(math.Max(0, baseComputeUSD-expected))
	}

	costMin := roundUSD(expected * 0.85)
	costMax := roundUSD((expected + verifOverhead) * 1.5)

	// Real supply uses the buyer constraint or the catalogue model floor,
	// whichever is stricter. Compute it before ETA so both the planner and its
	// fallback count exactly the workers this job could claim.
	minMem := sub.Constraints.MinMemoryGB
	var modelMinMem float32
	if m, err := s.store.GetModel(ctx, sub.Model.Ref); err == nil {
		modelMinMem = m.MinMemoryGB
		if modelMinMem > minMem {
			minMem = modelMinMem
		}
	}

	// ETA: the existing queue-depth/throughput estimate is the p50; band it up for
	// p90/worst (cold starts, retries, contention) rather than promising a point.
	// Model-aware: once this (job_type, model) has enough committed history the p50
	// is driven by the OBSERVED p90 per-task duration, not the static target (Plane
	// D D6 drift feedback — the quote and reality converge as the Brain learns).
	// Speed Lane wave 2A: etaBandSecs additionally surfaces the planner's
	// CONSERVATIVE band (rates degraded to 75% of measured) — the guarantee basis
	// for the speed-SLA below. plannerBacked=false (disabled planner / thin rate
	// cache) keeps the p50 identical to the pre-wave estimateETASecs value and
	// forecloses any guarantee.
	p50, conservativeSecs, plannerBacked := s.etaBandSecs(ctx, jobType, sub.Model.Ref, minMem, tasks)
	// Sustained-throughput honesty (Thermal 6->7): estimateETASecs' static
	// fallback target is derived from the PEAK tok/s the business quotes, so a LONG
	// batch job — which really runs for minutes and hits the measured 36.6%
	// steady-state drop (docs/GPU_CAPABILITY.md) — is optimistically quoted at
	// peak. Detect whether the p50 used real observed history (which already
	// reflects the machine's actual sustained pace, so must NOT be re-derated) by
	// asking the same source estimateETASecs does; if it fell back to the peak
	// target, derate the p50 to the sustained pace for long batch jobs so the ETA
	// is honest for the jobs where the gap actually bites.
	observedP90ms, _, hErr := s.store.HistoricalP90DurationMs(ctx, jobType, sub.Model.Ref)
	usedObservedHistory := hErr == nil && observedP90ms > 0
	p50 = sustainedBatchETASecs(p50, tier, usedObservedHistory)
	eta := QuoteTime{P50Secs: p50, P90Secs: p50 * 2, WorstCaseSecs: p50 * 4}

	// Substrate routing (rubric dimension 4, routing.go): read the job's shape
	// and say which substrate runs it fastest, grounded in the measured A100
	// sweep. GENERATIVE + records > 0 only — the sweep measured generative
	// decode, so every other shape honestly gets no routing block. Uses the
	// SAME inputs the rest of this quote already computed: the sustained-
	// adjusted p50 (the ETA the buyer actually sees), the planner conservative
	// band + plannerBacked from etaBandSecs, the catalogue memory floor for the
	// model class, and the planner's own tokens-per-item estimator.
	var routing *QuoteRouting
	if generativeJobType(jobType) && scan.Records > 0 {
		// The LIVE lit-lane supply: real online vLLM-engine workers eligible for
		// this job (the same predicate as eligible_now, engine-filtered). A DB
		// error degrades to 0 — honestly "no lit lane" rather than a fabricated
		// gpu_lane. This is the honest switch: the router says gpu_lane only when
		// verified GPU supply actually exists, otherwise gpu_recommend.
		litGPU, _ := s.store.EligibleVLLMWorkerCount(ctx, jobType, sub.Model.Ref, minMem)
		dec := DecideSubstrate(scan.Records, tier,
			routingModelClass(sub.Model.Ref, modelMinMem),
			tokensPerItemEstimate(sub.JobType.MaxTokens, avgLineBytes),
			p50, conservativeSecs, plannerBacked, litGPU)
		routing = &QuoteRouting{
			Substrate:      dec.Substrate,
			Reason:         dec.Reason,
			FleetETASecs:   dec.FleetSecs,
			GPUModeledSecs: dec.GPUModeledSecs,
			Basis:          quoteRoutingBasis,
		}
	}

	eligibleNow, _ := s.store.EligibleWorkerCount(ctx, jobType, sub.Model.Ref, minMem)
	// Warm supply (warm-routing, D3): eligible workers that already have THIS model
	// loaded. Drives cold-start risk down + confidence up when present — these are the
	// workers the scheduler prefers, so the job likely starts without a cold load.
	warmEligible, _ := s.store.WarmEligibleWorkerCount(ctx, jobType, sub.Model.Ref, minMem)

	oomRisk, coldRisk, conf, warnings := assessRisk(scan, eligibleNow, warmEligible, modelMinMem)

	// Supply-density gate (DEEP_RESEARCH_V2 §6.2 launch gate) + routing transparency
	// (§4): only promise a project-SLA ETA when enough workers are eligible right now,
	// and surface the eligible pool's average reputation so the buyer sees the quality
	// of supply their job routes to.
	poolRep, _ := s.store.EligiblePoolReputation(ctx, jobType, sub.Model.Ref, minMem)
	slaEligible := eligibleNow >= slaMinEligibleWorkers
	if !slaEligible {
		warnings = append(warnings, fmt.Sprintf(
			"supply below the SLA threshold (%d eligible, need %d): ETA is advisory only, no project-SLA guarantee",
			eligibleNow, slaMinEligibleWorkers))
	}
	// Wall-clock speed-SLA offer (Speed Lane wave 2A): only when the supply gate
	// passed AND the ETA was genuinely planner-backed (real measured rates) does
	// the quote carry a time guarantee — priced at the documented premium, derived
	// from the planner's conservative band + explicit margins (deriveQuoteSLA).
	// Build once without the optional premium so the public Cost.Expected/Max contract
	// remains the price of compute itself. The premium is still 15% of that public
	// expected price, then a second deterministic plan freezes it as once-only buyer
	// revenue. This avoids both circular premium math and double-counting it in the
	// firm cap (which remains Cost.Max + SLA.Premium).
	basePlanInput := EconomicPlanInput{
		BaseComputeUSD:   baseComputeUSD,
		InitialTaskCount: initialEconomicTasks,
		ExtraTaskReserve: economicExtraTaskReserve(tasks),
		SupplierShare:    supplierShareRate,
	}
	baseEconomicPlan := BuildEconomicPlan(basePlanInput, schedule)
	if economicCountErr != nil {
		baseEconomicPlan = blockedEconomicPlan(basePlanInput, schedule, "counting initial verification work: "+economicCountErr.Error())
	}
	if baseEconomicPlan.Executable && initialEconomicTasks >= tasks {
		// QuoteCost is buyer-facing, so report the incremental GUARDED charge of
		// verification tasks. Reporting only their raw base compute would materially
		// understate what the buyer pays when the per-task processor fixed fee is the
		// binding margin constraint (especially for small jobs).
		verifOverhead = roundEconomicUSD(
			baseEconomicPlan.BuyerChargePerTaskUSD * float64(initialEconomicTasks-tasks),
		)
	}
	quoteSLA := deriveQuoteSLA(
		slaEligible && baseEconomicPlan.Executable,
		plannerBacked,
		conservativeSecs,
		baseEconomicPlan.InitialBuyerChargeUSD,
	)
	if quoteSLA != nil {
		warnings = append(warnings, fmt.Sprintf(
			"speed-SLA offer: guaranteed completion within %ds of submission for a $%.6f premium (auto-refunded on a miss); binds only when you submit with firm_quote=true and this quote_id",
			quoteSLA.GuaranteedSecs, quoteSLA.PremiumUSD))
	}
	if sub.PrivatePool && privatePoolMemberCount == 0 {
		warnings = append(warnings,
			"private_pool is set but you have zero bound suppliers (POST /v1/private-pool to add one): this job could never be claimed by anyone and will be refused at submit")
	}

	// Memory-floor feedback (Plane D D4): if the model's floor exceeds the MEDIAN
	// effective memory of eligible workers (from the rolling samples), the typical
	// eligible box is tight on this model even though the binary count passed —
	// bump oom_risk + lower confidence with an explainable reason. When no eligible
	// worker has reported a sample yet, ok is false and risk is left untouched (we
	// never invent a median).
	if modelMinMem > 0 {
		if median, ok, err := s.store.MedianEffectiveMemoryGB(ctx, jobType, sub.Model.Ref); err == nil && ok {
			oomRisk, conf = applyMemoryFloorRisk(oomRisk, conf, modelMinMem, median)
		}
	}

	// The quote id is a real UUID stored as quotes.id; the buyer-facing handle is
	// "q_<uuid>" (keeps the existing cx-quote display). input_sha256 fingerprints the
	// scanned bytes so a later submit can best-effort confirm it quoted THIS input.
	bareID := uuid.New()
	sum := sha256.Sum256(inputBytes)

	var attestation string
	if sub.PrivatePool {
		attestation = privatePoolAttestation
	}
	slaPremium := 0.0
	if quoteSLA != nil {
		slaPremium = quoteSLA.PremiumUSD
	}
	planInput := basePlanInput
	planInput.SLAPremiumUSD = slaPremium
	economicPlan := BuildEconomicPlan(planInput, schedule)
	if economicCountErr != nil {
		economicPlan = blockedEconomicPlan(planInput, schedule, "counting initial verification work: "+economicCountErr.Error())
	}
	if economicPlan.Executable {
		expected = baseEconomicPlan.InitialBuyerChargeUSD
		costMax = baseEconomicPlan.ReservedBuyerChargeUSD
		costMin = baseEconomicPlan.BuyerChargePerTaskUSD
		platformTake = roundEconomicUSD(
			baseEconomicPlan.InitialBuyerChargeUSD -
				baseEconomicPlan.SupplierPayoutPerTaskUSD*float64(initialEconomicTasks),
		)
	} else {
		warnings = append(warnings, "economics blocked: "+economicPlan.BlockReason)
	}

	return Quote{
		QuoteID:                "q_" + bareID.String(),
		bareID:                 bareID,
		ExpiresAt:              time.Now().Add(quoteTTL).UTC(),
		InputSHA256:            hex.EncodeToString(sum[:]),
		JobType:                jobType,
		Model:                  sub.Model.Ref,
		Tier:                   tier,
		TierSemantics:          serviceTierSemantics(tier),
		Input:                  scan,
		AudioInput:             audioUploadMetadata(sub.audioAdmission),
		PrivatePoolAttestation: attestation,
		SLA:                    quoteSLA,
		Routing:                routing,
		Economics:              economicPlan,
		Execution: QuoteExecution{
			RecommendedSplitSize:   split,
			EstimatedTasks:         tasks,
			EligibleWorkersNow:     eligibleNow,
			WarmEligibleWorkers:    warmEligible,
			ModelMinMemoryGB:       modelMinMem,
			OOMRisk:                oomRisk,
			ColdStartRisk:          coldRisk,
			SLAEligible:            slaEligible,
			PoolReputation:         poolRep,
			PrivatePool:            sub.PrivatePool,
			PrivatePoolMemberCount: privatePoolMemberCount,
		},
		Cost: QuoteCost{
			MinUSD: costMin, ExpectedUSD: expected, MaxUSD: costMax,
			VerificationOverheadUSD: verifOverhead, PlatformTakeUSD: platformTake,
			PrivatePoolPremiumUSD: privatePoolPremium,
		},
		Time:       eta,
		Confidence: conf,
		Budget: QuoteBudget{
			// Suggest a cap at the conservative top of the band so a normal run
			// completes but a runaway is stopped (PLANE_C §12 budget cap).
			SuggestedMaxUSD:       costMax,
			CancelBeforeExceeding: true,
		},
		Warnings: warnings,
	}
}

// assessRisk derives OOM/cold-start risk + a confidence score + warnings from the
// scan and live supply. Conservative and explainable: it leans toward higher risk
// and lower confidence when data is missing, and every downgrade carries a reason.
// warmEligible is the count of eligible workers that already have the model warm
// (warm-routing, D3): when > 0 the job likely starts without a cold model load, so
// cold-start risk drops to low and confidence rises; when 0 the cold-start estimate
// stays conservative (a load is possible) — never faked low.
func assessRisk(scan QuoteInputScan, eligibleNow, warmEligible int, modelMinMem float32) (oom, cold string, conf QuoteConfidence, warnings []string) {
	reasons := []string{}
	score := 0.8

	// Supply drives OOM + confidence: the eligible count already filters on
	// effective memory + not-throttled, so >0 eligible means real headroom exists.
	switch {
	case eligibleNow == 0:
		oom = "high"
		score -= 0.3
		reasons = append(reasons, "no workers currently pass the memory/model filter for this job")
		warnings = append(warnings, "no eligible workers online right now; the job may queue until supply appears")
	case eligibleNow < 3:
		oom = "medium"
		score -= 0.1
		reasons = append(reasons, fmt.Sprintf("%d eligible worker(s) with enough effective memory (thin supply)", eligibleNow))
	default:
		oom = "low"
		reasons = append(reasons, fmt.Sprintf("%d eligible workers have enough effective memory", eligibleNow))
	}
	if modelMinMem > 0 {
		reasons = append(reasons, fmt.Sprintf("model memory floor is %.0f GB; supply count is filtered against effective memory", modelMinMem))
	} else {
		reasons = append(reasons, "model not in the catalogue; using a conservative default price + no memory floor")
		score -= 0.1
	}

	// Cold-start risk from REAL warm-model state (warm-routing, D3). Warm eligible
	// workers already have the model loaded, so the job can start without a cold load:
	// risk drops to low and confidence rises. With no warm supply we stay conservative
	// (medium) and say so — never faked low.
	if warmEligible > 0 {
		cold = "low"
		score += 0.1
		reasons = append(reasons, fmt.Sprintf("%d eligible worker(s) already have this model warm; cold-start unlikely", warmEligible))
	} else {
		cold = "medium"
		reasons = append(reasons, "no eligible worker currently has this model warm; a cold model load is possible")
	}

	// Input quality.
	if scan.Records == 0 {
		score -= 0.4
		warnings = append(warnings, "input has no records")
	}
	if scan.MalformedRecords > 0 {
		score -= 0.2
		warnings = append(warnings, fmt.Sprintf("%d malformed JSONL record(s); first at line %d", scan.MalformedRecords, scan.FirstBadLine))
	}
	reasons = append(reasons, "token count is a byte heuristic, not an exact tokenizer count")

	if score < 0.05 {
		score = 0.05
	}
	if score > 0.95 {
		score = 0.95
	}
	return oom, cold, QuoteConfidence{Score: roundUSD(score), Reasons: reasons}, warnings
}

// applyMemoryFloorRisk bumps OOM risk + lowers confidence when the model's memory
// floor exceeds the MEDIAN effective memory of eligible workers (from the rolling
// worker_memory_samples — Plane D D4). The eligible COUNT can pass (some workers
// clear the floor) while the TYPICAL eligible box is still tight; this catches that
// honestly. Deterministic and explainable: a floor over the median escalates one
// step (low→medium→high) and shaves confidence with a stated reason; a floor that
// merely approaches the median (within memFloorTightMargin) adds a softer caution
// without escalating. Pure — no I/O, fully unit-tested. Called only when a real
// median exists; never fabricates supply.
func applyMemoryFloorRisk(oom string, conf QuoteConfidence, modelMinMem, medianEffectiveGB float32) (string, QuoteConfidence) {
	switch {
	case modelMinMem > medianEffectiveGB:
		oom = escalateRisk(oom)
		conf.Score = roundUSD(clampScore(conf.Score - 0.15))
		conf.Reasons = append(conf.Reasons, fmt.Sprintf(
			"model memory floor %.0f GB exceeds the median effective memory of eligible workers (%.1f GB); the typical eligible worker is tight on this model",
			modelMinMem, medianEffectiveGB))
	case medianEffectiveGB-modelMinMem <= memFloorTightMargin:
		conf.Score = roundUSD(clampScore(conf.Score - 0.05))
		conf.Reasons = append(conf.Reasons, fmt.Sprintf(
			"model memory floor %.0f GB is close to the median effective memory of eligible workers (%.1f GB); little headroom",
			modelMinMem, medianEffectiveGB))
	default:
		conf.Reasons = append(conf.Reasons, fmt.Sprintf(
			"median effective memory of eligible workers (%.1f GB) comfortably clears the model floor (%.0f GB)",
			medianEffectiveGB, modelMinMem))
	}
	return oom, conf
}

// memFloorTightMargin is the GB band above the model floor within which the median
// eligible worker counts as "tight" (a soft confidence shave, no risk escalation).
const memFloorTightMargin = 2.0

// escalateRisk bumps a low|medium|high risk one step toward high (high stays high).
func escalateRisk(r string) string {
	switch r {
	case "low":
		return "medium"
	case "medium":
		return "high"
	default:
		return "high"
	}
}

// clampScore keeps a confidence score inside the same [0.05, 0.95] band assessRisk
// uses, so memory-floor feedback can never push confidence to a fake 0 or 1.
func clampScore(s float64) float64 {
	if s < 0.05 {
		return 0.05
	}
	if s > 0.95 {
		return 0.95
	}
	return s
}

// --- handler --------------------------------------------------------------------

// handleQuote (POST /v1/quote, buyer-authed): scan + price a submission without
// spending. Validates the shape like createJob, resolves the input, builds the
// quote, persists the assumptions, and returns 200 with the Quote.
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var sub jobSubmit
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid quote request json: "+err.Error())
		return
	}
	if sub.JobType.Type == "" || !validJobTypes[sub.JobType.Type] {
		writeErr(w, http.StatusBadRequest, "valid job_type.type is required")
		return
	}
	if herr := rejectUntrustedAudioSubmission(sub); herr != nil {
		writeErr(w, herr.status, herr.msg)
		return
	}
	if sub.Tier == "" {
		sub.Tier = "batch"
	}
	if !validTiers[sub.Tier] {
		writeErr(w, http.StatusBadRequest, "invalid tier: "+sub.Tier)
		return
	}
	// Catalog membership alone is not runtime authority. Fail before resolving or
	// reading input unless this exact job/model pair is a generated production cell.
	canonicalModel, err := normalizeAdvertisedRuntimeModelRef(sub.JobType.Type, sub.Model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sub.Model = canonicalModel
	schedule, err := LoadEconomicScheduleFromEnv()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "economic schedule unavailable: "+err.Error())
		return
	}
	inputReader, _, err := s.resolveInput(r.Context(), auth.BuyerID, sub.Input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "resolving input: "+err.Error())
		return
	}
	// The quote-preview path (unlike the submission path this rung streams) needs the
	// whole input in memory anyway to scan/hash it for buildQuote — a quote is a
	// synchronous preview call, not the large async submission this rung targets, so a
	// whole-buffer read here is the same behavior this endpoint always had.
	inputBytes, err := readSynchronousInput(inputReader)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errSynchronousInputTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, "reading input: "+err.Error())
		return
	}
	q := s.buildQuoteWithSchedule(r.Context(), auth.BuyerID, sub, inputBytes, schedule)
	if !q.Economics.Executable {
		writeErr(w, http.StatusConflict, "quote is not executable: "+q.Economics.BlockReason)
		return
	}
	if err := s.store.InsertQuote(r.Context(), auth.BuyerID, q); err != nil {
		// Persisting the quote is the load-bearing rule; a failure is a real error,
		// not silently swallowed (the buyer still gets the quote, but we log loudly).
		writeErr(w, http.StatusInternalServerError, "persisting quote: "+err.Error())
		return
	}
	metrics.quotes.Add(1) // observability (Plane D D21): a quote was priced + persisted
	writeJSON(w, http.StatusOK, q)
}

type pipelineQuoteStage struct {
	Op    string `json:"op"`
	Model string `json:"model"`
}

type pipelineQuoteRequest struct {
	Input  json.RawMessage      `json:"input"`
	Tier   string               `json:"tier"`
	Stages []pipelineQuoteStage `json:"stages"`
}

// handlePipelineQuote prices a detected MULTI-STAGE pipeline (item 4): it quotes each
// stage on the same input (the current patterns are fan-out) via the SAME buildQuote the
// single-quote path uses, then aggregates honestly with composeQuotes. Advisory only —
// it persists nothing and binds nothing (a composite preview); the buyer binds per stage.
func (s *Server) handlePipelineQuote(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	var req pipelineQuoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid pipeline quote json: "+err.Error())
		return
	}
	if len(req.Stages) == 0 {
		writeErr(w, http.StatusBadRequest, "a pipeline quote needs at least one stage")
		return
	}
	if req.Tier == "" {
		req.Tier = "batch"
	}
	if !validTiers[req.Tier] {
		writeErr(w, http.StatusBadRequest, "invalid tier: "+req.Tier)
		return
	}
	for i, st := range req.Stages {
		if st.Op == audioUploadJobType {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("stage %d: %s", i, audioUploadBoundaryError))
			return
		}
	}
	schedule, err := LoadEconomicScheduleFromEnv()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "economic schedule unavailable: "+err.Error())
		return
	}
	inputReader, _, err := s.resolveInput(r.Context(), auth.BuyerID, req.Input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "resolving input: "+err.Error())
		return
	}
	inputBytes, err := readSynchronousInput(inputReader)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errSynchronousInputTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, "reading input: "+err.Error())
		return
	}
	stages := make([]Quote, 0, len(req.Stages))
	for _, st := range req.Stages {
		if st.Op == "" || !validJobTypes[st.Op] {
			writeErr(w, http.StatusBadRequest, "invalid stage job_type: "+st.Op)
			return
		}
		if err := validateAdvertisedRuntimeJobModel(st.Op, st.Model); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		q := s.buildQuoteWithSchedule(r.Context(), auth.BuyerID, jobSubmit{
			JobType: JobType{Type: st.Op},
			Model:   generatedRuntimeModelRef(st.Op, st.Model),
			Tier:    req.Tier,
			Input:   req.Input,
		}, inputBytes, schedule)
		if !q.Economics.Executable {
			writeErr(w, http.StatusConflict, "pipeline stage quote is not executable: "+q.Economics.BlockReason)
			return
		}
		stages = append(stages, q)
	}
	writeJSON(w, http.StatusOK, composeQuotes(stages))
}

// --- store (quote-specific *Store methods live with the quote, per benchmark.go) ---

// EligibleWorkerCount counts workers that would pass the claim hard filter for a
// job of (jobType, modelRef, minMemGB) AND are live (seen <60s): an exact
// current-matrix capability row, enough effective memory (falling back to total), not
// throttled, supplier active. This is the SAME predicate ClaimTask uses, so the
// quote's `eligible_now` is the honest current supply, not a raw worker count.
func (s *Store) EligibleWorkerCount(ctx context.Context, jobType, modelRef string, minMemGB float32) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		  WHERE w.last_seen_at IS NOT NULL
		    AND w.last_seen_at > now() - interval '60 seconds'
		    AND s.status = 'active'
		    AND NOT COALESCE(w.throttled, false)
		    AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id = w.id
		         AND wac.job_type = $1
		         AND wac.model_ref = $2
		         AND wac.matrix_sha256 = $4
		    )`,
		jobType, modelRef, minMemGB, generatedRuntimeMatrixSHA256,
	).Scan(&n)
	return n, err
}

// EligibleVLLMWorkerCount counts the live workers that would pass the SAME claim
// hard filter as EligibleWorkerCount AND run the vLLM engine (workers.engine =
// 'vllm') — i.e. the real, currently-online supply of the GPU serving lane for
// THIS job. It is what makes the substrate router's `gpu_lane` decision HONEST:
// routing.go reports a lit GPU lane only when this count is > 0, and falls back
// to an advisory `gpu_recommend` (count 0) otherwise — never claiming supply the
// exchange does not have. The within-nvidia_* byte-stability soak
// (docs/speed-lane-reports/VLLM_RESTART_SOAK_2026-07-06.md) is what makes such a
// worker's output trustworthy (tolerant (engine, build_hash) class + redundancy,
// not a byte-exact honeypot); this count is the supply half of lighting the lane.
func (s *Store) EligibleVLLMWorkerCount(ctx context.Context, jobType, modelRef string, minMemGB float32) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		  WHERE w.last_seen_at IS NOT NULL
		    AND w.last_seen_at > now() - interval '60 seconds'
		    AND s.status = 'active'
		    AND NOT COALESCE(w.throttled, false)
		    AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id = w.id
		         AND wac.job_type = $1
		         AND wac.model_ref = $2
		         AND wac.matrix_sha256 = $4
		    )
		    AND w.engine = 'vllm'`,
		jobType, modelRef, minMemGB, generatedRuntimeMatrixSHA256,
	).Scan(&n)
	return n, err
}

// EligiblePoolReputation is the AVERAGE supplier reputation across the workers that
// would pass the claim hard filter for this job right now (the same predicate as
// EligibleWorkerCount). Surfaced to the buyer as routing transparency (DEEP_RESEARCH_V2
// §4): the quality of the pool their project routes to. 0 when the pool is empty.
func (s *Store) EligiblePoolReputation(ctx context.Context, jobType, modelRef string, minMemGB float32) (float64, error) {
	var r float64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(AVG(s.reputation), 0)
		   FROM workers w JOIN suppliers s ON s.id = w.supplier_id
		  WHERE w.last_seen_at IS NOT NULL
		    AND w.last_seen_at > now() - interval '60 seconds'
		    AND s.status = 'active'
		    AND NOT COALESCE(w.throttled, false)
		    AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id = w.id
		         AND wac.job_type = $1
		         AND wac.model_ref = $2
		         AND wac.matrix_sha256 = $4
		    )`,
		jobType, modelRef, minMemGB, generatedRuntimeMatrixSHA256,
	).Scan(&r)
	return r, err
}

// AddPrivatePoolMember binds a supplier to a buyer's Private Deployment pool (research
// §3): only bound suppliers may claim that buyer's private_pool jobs. Idempotent.
func (s *Store) AddPrivatePoolMember(ctx context.Context, buyerID, supplierID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO private_pool_members (buyer_id, supplier_id) VALUES ($1,$2)
		 ON CONFLICT (buyer_id, supplier_id) DO NOTHING`,
		buyerID, supplierID)
	return err
}

// RemovePrivatePoolMember unbinds a supplier from a buyer's Private Deployment pool
// (Buyer advantage & pricing edge 6->7, docs/internal/CREED_AND_PATH_TO_TEN.md:
// "Productize the privacy premium instead of leaving it a sentence" — a real
// buyer-facing flow needs add AND remove, not just the one-way bind the backend
// originally shipped). Idempotent: removing a non-member is a silent no-op, not
// an error, matching AddPrivatePoolMember's own idempotency.
func (s *Store) RemovePrivatePoolMember(ctx context.Context, buyerID, supplierID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM private_pool_members WHERE buyer_id = $1 AND supplier_id = $2`,
		buyerID, supplierID)
	return err
}

// PrivatePoolMember is one row of GET /v1/private-pool: a supplier this buyer has
// bound to their dedicated fleet, plus enough of the supplier's real state
// (reputation, active-ness) for the buyer to judge who they are actually paying
// the privacy premium to run on.
type PrivatePoolMember struct {
	SupplierID uuid.UUID `json:"supplier_id"`
	Reputation float32   `json:"reputation"`
	Status     string    `json:"status"`
	BoundAt    time.Time `json:"bound_at"`
}

// ListPrivatePoolMembers returns a buyer's own private-pool members (Buyer
// advantage & pricing edge 6->7), ordered by when they were bound — the
// buyer-facing counterpart to AddPrivatePoolMember/RemovePrivatePoolMember, so a
// buyer can see who is actually in their private pool via the API, not just a
// database row only an operator could query.
func (s *Store) ListPrivatePoolMembers(ctx context.Context, buyerID uuid.UUID) ([]PrivatePoolMember, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.supplier_id, s.reputation, s.status, m.created_at
		   FROM private_pool_members m JOIN suppliers s ON s.id = m.supplier_id
		  WHERE m.buyer_id = $1
		  ORDER BY m.created_at ASC`,
		buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PrivatePoolMember{}
	for rows.Next() {
		var m PrivatePoolMember
		if err := rows.Scan(&m.SupplierID, &m.Reputation, &m.Status, &m.BoundAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PrivatePoolMemberCount is the buyer's bound-supplier count (Buyer advantage &
// pricing edge 6->7): createJob uses it to refuse a private_pool submission that
// could never be claimed by anyone (zero bound suppliers), and buildQuote uses it
// to show the pool size the premium is actually paying for.
func (s *Store) PrivatePoolMemberCount(ctx context.Context, buyerID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM private_pool_members WHERE buyer_id = $1`, buyerID,
	).Scan(&n)
	return n, err
}

// WarmEligibleWorkerCount counts the eligible workers (same predicate as
// EligibleWorkerCount) that ALSO have modelRef WARM right now — a fresh
// worker_model_state row for the model (last_seen_warm within the 60s liveness
// window). This is the supply that would START this job without a cold model load
// (warm-routing, D3); the scheduler prefers exactly these workers. Returns 0 when
// modelRef is "" (no model → "warm" is undefined). Honest supply: a warm worker is
// counted only because it reported the model warm, never assumed.
func (s *Store) WarmEligibleWorkerCount(ctx context.Context, jobType, modelRef string, minMemGB float32) (int, error) {
	if modelRef == "" {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM workers w
		   JOIN suppliers s ON s.id = w.supplier_id
		   JOIN worker_model_state wms
		     ON wms.worker_id = w.id
		    AND wms.model_id = $2
		    AND wms.last_seen_warm > now() - interval '60 seconds'
		  WHERE w.last_seen_at IS NOT NULL
		    AND w.last_seen_at > now() - interval '60 seconds'
		    AND s.status = 'active'
		    AND NOT COALESCE(w.throttled, false)
		    AND COALESCE($3,0) <= COALESCE(w.effective_memory_gb, w.memory_gb, 0)
		    AND EXISTS (
		      SELECT 1 FROM worker_authorized_capabilities wac
		       WHERE wac.worker_id = w.id
		         AND wac.job_type = $1
		         AND wac.model_ref = $2
		         AND wac.matrix_sha256 = $4
		    )`,
		jobType, modelRef, minMemGB, generatedRuntimeMatrixSHA256,
	).Scan(&n)
	return n, err
}

// InsertQuote persists a quote's assumptions (PLANE_C §6: a later invoice must be
// able to say what was believed at quote time). Scalars are queryable; the full
// object is kept in quote_json.
func (s *Store) InsertQuote(ctx context.Context, buyerID uuid.UUID, q Quote) error {
	blob, err := json.Marshal(q)
	if err != nil {
		return err
	}
	planBlob, err := json.Marshal(q.Economics)
	if err != nil {
		return err
	}
	// id is the quote's bare UUID (the <uuid> in the "q_<uuid>" handle) so a buyer can
	// bind a later submission back to this exact row. expires_at/input_sha256 back the
	// D7 binding check (not-expired + bytes-match). sla_guaranteed_secs/
	// sla_premium_usd persist the speed-SLA OFFER (wave 2A) so the submit path binds
	// exactly what was offered — NULL when no guarantee was offerable (the honest
	// degradation path leaves the row exactly as pre-wave).
	var slaSecs *int
	var slaPremium *float64
	if q.SLA != nil {
		slaSecs, slaPremium = &q.SLA.GuaranteedSecs, &q.SLA.PremiumUSD
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO quotes
		   (id, buyer_id, job_type, model_ref, tier, records, input_bytes,
		    estimated_tokens, malformed_records, split_size, task_count, eligible_now,
		    cost_expected_usd, cost_min_usd, cost_max_usd, eta_p50_secs, eta_p90_secs,
		    oom_risk, confidence, quote_json, expires_at, input_sha256,
		    sla_guaranteed_secs, sla_premium_usd,
		    economic_schedule_version, economic_plan, economic_executable)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		q.bareID, buyerID, q.JobType, q.Model, q.Tier, q.Input.Records, q.Input.Bytes,
		q.Input.EstimatedTokens, q.Input.MalformedRecords, q.Execution.RecommendedSplitSize,
		q.Execution.EstimatedTasks, q.Execution.EligibleWorkersNow,
		q.Cost.ExpectedUSD, q.Cost.MinUSD, q.Cost.MaxUSD, q.Time.P50Secs, q.Time.P90Secs,
		q.Execution.OOMRisk, q.Confidence.Score, blob, q.ExpiresAt, q.InputSHA256,
		slaSecs, slaPremium,
		q.Economics.Schedule.Version, planBlob, q.Economics.Executable,
	)
	return err
}

// --- quote → submit binding (Plane D D7, errata C-Errata-4) ---------------------

// quoteIDToUUID parses a buyer-supplied quote handle into the bare quotes.id UUID.
// The handle is normally "q_<uuid>" (what POST /v1/quote returns), but a bare uuid
// is accepted too so a buyer can pass back either shape. An unparseable handle is a
// caller error, surfaced verbatim — never silently dropped.
func quoteIDToUUID(handle string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimPrefix(strings.TrimSpace(handle), "q_"))
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("invalid quote_id %q", handle)
	}
	return id, nil
}

// boundQuote is the slice of a persisted quote the submit path needs to validate a
// binding: the original (job_type, model, tier) the buyer was quoted on, the input
// fingerprint, the expected cost (echoed onto the invoice), the quoted MAXIMUM
// (the real commitment a firm-quote submission caps the charge at — Project
// Detection & Quotation 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md), and whether
// the quote has expired (computed in SQL against now() so it does not depend on
// clock skew).
type boundQuote struct {
	ID          uuid.UUID
	JobType     string
	ModelRef    string
	Tier        string
	InputSHA256 string
	CostExpUSD  float64
	CostMaxUSD  float64
	Expired     bool
	// SLAGuaranteedSecs / SLAPremiumUSD carry the quote's speed-SLA OFFER (wave
	// 2A) into the submit binding: a firm_quote submission against an SLA-bearing
	// quote binds the time guarantee alongside the price cap (createJob). Both 0
	// when the quote carried no offer — the binding then stays price-only.
	SLAGuaranteedSecs       int
	SLAPremiumUSD           float64
	EconomicScheduleVersion string
	EconomicPlan            EconomicPlan
	EconomicExecutable      bool
}

// GetBindableQuote loads a buyer's quote by id for binding a submission to it.
// Buyer-scoped (a buyer can only bind their OWN quotes); returns errNotFound when
// the id is not this buyer's quote. `expired` is derived from expires_at vs now()
// in the DB (a NULL expires_at — a pre-D7 quote — is treated as never-expiring so
// older rows still bind). The caller decides what a match requires.
func (s *Store) GetBindableQuote(ctx context.Context, quoteID, buyerID uuid.UUID) (*boundQuote, error) {
	var q boundQuote
	var planBlob []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, job_type, COALESCE(model_ref,''), COALESCE(tier,''),
		        COALESCE(input_sha256,''), COALESCE(cost_expected_usd,0),
		        COALESCE(cost_max_usd,0),
		        (expires_at IS NOT NULL AND expires_at <= now()) AS expired,
		        COALESCE(sla_guaranteed_secs,0), COALESCE(sla_premium_usd,0)::float8,
		        COALESCE(economic_schedule_version,''), economic_plan,
		        COALESCE(economic_executable,false)
		   FROM quotes
		  WHERE id = $1 AND buyer_id = $2`,
		quoteID, buyerID,
	).Scan(&q.ID, &q.JobType, &q.ModelRef, &q.Tier, &q.InputSHA256, &q.CostExpUSD, &q.CostMaxUSD, &q.Expired,
		&q.SLAGuaranteedSecs, &q.SLAPremiumUSD, &q.EconomicScheduleVersion, &planBlob, &q.EconomicExecutable)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(planBlob) > 0 {
		if err := json.Unmarshal(planBlob, &q.EconomicPlan); err != nil {
			return nil, fmt.Errorf("decoding quote economic plan: %w", err)
		}
	}
	return &q, nil
}

// QuotedUSDForJob returns the cost_expected_usd of the quote a job was bound to, if
// any. ok is false when the job has no quote_id (the invoice then omits the quoted
// figure rather than inventing one). Buyer-scoped via the jobs row the caller already
// owns; the join keeps it a single round-trip.
func (s *Store) QuotedUSDForJob(ctx context.Context, jobID uuid.UUID) (usd float64, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT q.cost_expected_usd
		   FROM jobs j JOIN quotes q ON q.id = j.quote_id
		  WHERE j.id = $1 AND j.quote_id IS NOT NULL`,
		jobID,
	).Scan(&usd)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return usd, true, nil
}
