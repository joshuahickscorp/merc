package main

// Realtime selection cost decomposition harness.
//
// Measures five statements (S0..S4) against a live synthetic offer book so the
// linear term inside AuthorizeRealtimeContract's SQL can be named with numbers
// rather than guessed from the plan shape. Opt-in only:
//
//	MERC_REALTIME_DECOMP=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	bash ops/scripts/with-isolated-test-db.sh \
//	  bash -c 'cd src/control && go test -count=1 -run '^TestRealtimeSelectionCostDecomposition$' -timeout 3h .'
//
// Writes evidence/perf/realtime-selection-decomposition-<shortsha>.json.
//
// S2 and S3 are COST PROBES, not correctness probes. Under rollback and
// concurrent state they will not select the same worker as S1; rank identity
// is not proven here. The production considered book is not truncated.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	realtimeDecompEnv              = "MERC_REALTIME_DECOMP"
	realtimeDecompLadderEnv        = "MERC_REALTIME_DECOMP_LADDER"
	realtimeDecompSamplesEnv       = "MERC_REALTIME_DECOMP_SAMPLES"
	realtimeDecompSeed       int64 = 20260810
	realtimeDecompDefaultN         = 20
	realtimeDecompWarmupN          = 4
	// Honesty gate: 1-minute load average above this marks the whole artifact
	// load_contaminated and absolute_quotable=false. Ratios stay reportable.
	realtimeDecompLoadCeiling = 8.0
	// Honesty gate: p99 > 25× p50 → unstable_sample_population.
	realtimeDecompUnstableP99OverP50 = 25.0
)

var realtimeDecompDefaultLadder = []int{1_000, 10_000, 100_000}

// Ranking keys identical to production row_number() OVER (ORDER BY ...) in
// realtimeAuthorizeCandidatesCTE. Kept as a single constant so S3 cannot drift
// a tie-break relative to the window sort it is isolating.
const realtimeDecompRankingOrderBy = `
			         e.verified_outcome_cost ASC,
			         CASE e.warmth WHEN 'HOT' THEN 0 WHEN 'WARM' THEN 1 WHEN 'CACHED' THEN 2 ELSE 3 END,
			         e.available_sequences DESC, e.last_seen_at DESC, e.worker_id ASC`

// decompPickedUpdatedSkip is the production picked+updated claim body from
// realtimeAuthorizeSelectOfferSQLSkip, shared by S1 and S2 so only the book
// aggregation (or its absence) differs.
const decompPickedUpdatedSkip = `
		, picked AS (
			SELECT c.worker_id,c.runtime_profile_id,
			       c.terminal_attempts,c.terminal_fails,c.verified_settlements,c.refund_count,
			       c.candidate_count,c.selected_rank
			  FROM candidates c
			  JOIN realtime_worker_offers o
			    ON o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			 ORDER BY c.selected_rank
			 FOR UPDATE OF o SKIP LOCKED
			 LIMIT 1
		), updated AS (
			UPDATE realtime_worker_offers o
			   SET available_sequences = o.available_sequences - 1, updated_at = now()
			  FROM picked c
			 WHERE o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			   AND o.available_sequences > 0
			 RETURNING o.worker_id,o.supplier_id,o.upstream_base_url,o.upstream_token_sealed,
			           o.supplier_input_usd_per_million_tokens::float8,
			           o.supplier_output_usd_per_million_tokens::float8,
			           o.placement_plan,o.placement_plan_sha256,o.warmth,
			           c.candidate_count,c.selected_rank,
			           c.terminal_attempts,c.terminal_fails,
			           c.verified_settlements,c.refund_count
		)`

// S1 production baseline (with considered book).
func decompS1SQL() string {
	return realtimeAuthorizeSelectOfferSQLSkip
}

// S2: S1 without the book CTE and without b.considered — isolates jsonb_agg
// build + wire transfer.
func decompS2SQL() string {
	return realtimeAuthorizeCandidatesCTE + decompPickedUpdatedSkip + `
		SELECT u.worker_id,u.supplier_id,u.upstream_base_url,u.upstream_token_sealed,
		       u.supplier_input_usd_per_million_tokens::float8,
		       u.supplier_output_usd_per_million_tokens::float8,
		       u.placement_plan,u.placement_plan_sha256,u.warmth,
		       u.candidate_count,u.selected_rank,
		       u.terminal_attempts,u.terminal_fails,u.verified_settlements,u.refund_count
		  FROM updated u`
}

// S3: S2 with window functions removed; pick is top-N ORDER BY ranking keys
// LIMIT 1. Isolates full-sort-and-materialise versus top-N heapsort.
//
// Cost probe only — does not claim the same worker as S1 under concurrent
// state or after rollback.
func decompS3SQL(eligibleBody string) string {
	// Close eligible with ") ," immediately so extractEligibleCTEBody on the
	// result returns a body byte-identical to production (no trailing pad).
	return `WITH eligible AS (` + eligibleBody + `), picked AS (
			SELECT e.worker_id,e.runtime_profile_id,
			       e.terminal_attempts,e.terminal_fails,e.verified_settlements,e.refund_count,
			       0::int AS candidate_count,
			       1::int AS selected_rank
			  FROM eligible e
			  JOIN realtime_worker_offers o
			    ON o.worker_id = e.worker_id AND o.runtime_profile_id = e.runtime_profile_id
			 ORDER BY` + realtimeDecompRankingOrderBy + `
			 FOR UPDATE OF o SKIP LOCKED
			 LIMIT 1
		), updated AS (
			UPDATE realtime_worker_offers o
			   SET available_sequences = o.available_sequences - 1, updated_at = now()
			  FROM picked c
			 WHERE o.worker_id = c.worker_id AND o.runtime_profile_id = c.runtime_profile_id
			   AND o.available_sequences > 0
			 RETURNING o.worker_id,o.supplier_id,o.upstream_base_url,o.upstream_token_sealed,
			           o.supplier_input_usd_per_million_tokens::float8,
			           o.supplier_output_usd_per_million_tokens::float8,
			           o.placement_plan,o.placement_plan_sha256,o.warmth,
			           c.candidate_count,c.selected_rank,
			           c.terminal_attempts,c.terminal_fails,
			           c.verified_settlements,c.refund_count
		)
		SELECT u.worker_id,u.supplier_id,u.upstream_base_url,u.upstream_token_sealed,
		       u.supplier_input_usd_per_million_tokens::float8,
		       u.supplier_output_usd_per_million_tokens::float8,
		       u.placement_plan,u.placement_plan_sha256,u.warmth,
		       u.candidate_count,u.selected_rank,
		       u.terminal_attempts,u.terminal_fails,u.verified_settlements,u.refund_count
		  FROM updated u`
}

// S4: count over eligible alone — irreducible eligibility filter floor.
func decompS4SQL(eligibleBody string) string {
	return `WITH eligible AS (` + eligibleBody + `) SELECT count(*)::int FROM eligible`
}

// extractEligibleCTEBody returns the body inside `WITH eligible AS ( ... )`
// from realtimeAuthorizeCandidatesCTE, so S3/S4 cannot drift predicates.
func extractEligibleCTEBody(sql string) (string, error) {
	const open = "WITH eligible AS ("
	i := strings.Index(sql, open)
	if i < 0 {
		return "", errors.New("eligible CTE open not found")
	}
	start := i + len(open)
	// Balance parentheses from the first char of the body.
	depth := 1
	for j := start; j < len(sql); j++ {
		switch sql[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return sql[start:j], nil
			}
		}
	}
	return "", errors.New("eligible CTE close not found")
}

// ---------------------------------------------------------------------------
// Artifact types
// ---------------------------------------------------------------------------

type decompReport struct {
	SchemaVersion    int                       `json:"schema_version"`
	Kind             string                    `json:"kind"`
	SourceCommit     string                    `json:"source_commit"`
	GeneratedAt      string                    `json:"generated_at"`
	FinishedAt       string                    `json:"finished_at"`
	WallClockSeconds float64                   `json:"wall_clock_seconds"`
	NumCPU           int                       `json:"num_cpu"`
	LoadAverageStart []float64                 `json:"load_average_start"`
	LoadAverageEnd   []float64                 `json:"load_average_end"`
	LoadContaminated bool                      `json:"load_contaminated"`
	AbsoluteQuotable bool                      `json:"absolute_quotable"`
	SamplesPerCell   int                       `json:"samples_per_cell"`
	WarmupPerCell    int                       `json:"warmup_per_cell"`
	Ladder           []int                     `json:"ladder"`
	Honesty          decompHonesty             `json:"honesty"`
	Notes            []string                  `json:"notes"`
	Sizes            map[string]decompSizeCell `json:"sizes"`
	DerivedRatios    map[string]decompRatios   `json:"derived_ratios"`
	ProjectionTopK   *decompTopKProjection     `json:"topk_projection_at_100k,omitempty"`
}

type decompHonesty struct {
	WhatThisProves       string `json:"what_this_proves"`
	WhatThisDoesNotProve string `json:"what_this_does_not_prove"`
	S2S3AreCostProbes    string `json:"s2_s3_are_cost_probes"`
	LoadNote             string `json:"load_note"`
}

type decompSizeCell struct {
	BookSize                      int                         `json:"book_size"`
	LiveEligibleAtMeasurement     int                         `json:"live_eligible_at_measurement"`
	NoLiveCandidatesAtMeasurement bool                        `json:"no_live_candidates_at_measurement"`
	HeartbeatSweeps               int                         `json:"heartbeat_sweeps"`
	HeartbeatMeanMS               float64                     `json:"heartbeat_mean_ms,omitempty"`
	Statements                    map[string]decompStmtResult `json:"statements"`
}

type decompStmtResult struct {
	StatementID              string    `json:"statement_id"`
	Description              string    `json:"description"`
	SQLDigest                string    `json:"sql_digest"`
	ExecutesUpdate           bool      `json:"executes_update"`
	WarmupN                  int       `json:"warmup_n"`
	WarmupMS                 []float64 `json:"warmup_ms"`
	SampleN                  int       `json:"sample_n"`
	SampleMS                 []float64 `json:"sample_ms,omitempty"`
	TimedSuccessN            int       `json:"timed_success_n"`
	AllSamplesFailed         bool      `json:"all_samples_failed"`
	UnstableSamplePopulation bool      `json:"unstable_sample_population"`
	EmptyOrDarkBookSamples   int       `json:"empty_or_dark_book_samples"`
	OtherFailures            int       `json:"other_failures"`
	FirstError               string    `json:"first_error,omitempty"`
	// Timings are omitted when all_samples_failed — they are not cost.
	P50MS  *float64 `json:"p50_ms,omitempty"`
	P95MS  *float64 `json:"p95_ms,omitempty"`
	P99MS  *float64 `json:"p99_ms,omitempty"`
	MaxMS  *float64 `json:"max_ms,omitempty"`
	MeanMS *float64 `json:"mean_ms,omitempty"`
	MinMS  *float64 `json:"min_ms,omitempty"`
	// Five-number summary always present when any success exists (even unstable).
	FiveNumber  *decompFiveNumber `json:"five_number_summary,omitempty"`
	ExplainJSON json.RawMessage   `json:"explain_analyze_buffers_json,omitempty"`
	ExplainNote string            `json:"explain_note,omitempty"`
}

type decompFiveNumber struct {
	Min float64 `json:"min_ms"`
	Q1  float64 `json:"q1_ms"`
	Med float64 `json:"median_ms"`
	Q3  float64 `json:"q3_ms"`
	Max float64 `json:"max_ms"`
}

type decompRatios struct {
	BookSize                     int      `json:"book_size"`
	S1P50MS                      *float64 `json:"s1_p50_ms,omitempty"`
	JsonbAggFractionOfS1         *float64 `json:"jsonb_agg_fraction_of_s1,omitempty"`         // (S1-S2)/S1
	WindowSortFractionOfS1       *float64 `json:"window_sort_fraction_of_s1,omitempty"`       // (S2-S3)/S1
	EligibilityFloorFractionOfS1 *float64 `json:"eligibility_floor_fraction_of_s1,omitempty"` // S4/S1
	S0BeforeP50MS                *float64 `json:"s0_before_p50_ms,omitempty"`
	S0AfterP50MS                 *float64 `json:"s0_after_p50_ms,omitempty"`
	S0AfterOverBefore            *float64 `json:"s0_after_over_before,omitempty"`
	JsonbAggMS                   *float64 `json:"jsonb_agg_ms,omitempty"`   // S1-S2
	WindowSortMS                 *float64 `json:"window_sort_ms,omitempty"` // S2-S3
	S4P50MS                      *float64 `json:"s4_p50_ms,omitempty"`
	S3P50MS                      *float64 `json:"s3_p50_ms,omitempty"`
	S2P50MS                      *float64 `json:"s2_p50_ms,omitempty"`
	Notes                        []string `json:"notes,omitempty"`
}

type decompTopKProjection struct {
	AtBookSize       int     `json:"at_book_size"`
	AssumedTopK      int     `json:"assumed_top_k"`
	Working          string  `json:"working"`
	ProjectedS1P50MS float64 `json:"projected_s1_p50_ms"`
	Caveat           string  `json:"caveat"`
}

func TestRealtimeSelectionCostDecomposition(t *testing.T) {
	if os.Getenv(realtimeDecompEnv) != "1" {
		t.Skip("set MERC_REALTIME_DECOMP=1 to run the realtime selection cost decomposition")
	}
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "rt-selection-decomp-key-32bytes-min!!!")

	ladder := realtimeDecompLadder(t)
	samples := realtimeDecompSamples(t)
	eligibleBody, err := extractEligibleCTEBody(realtimeAuthorizeCandidatesCTE)
	if err != nil {
		t.Fatalf("extract eligible body: %v", err)
	}
	// Drift guards: S2/S3 eligible body must match production byte-for-byte.
	if got, err := extractEligibleCTEBody(realtimeAuthorizeCandidatesCTE); err != nil || got != eligibleBody {
		t.Fatalf("eligible body extract unstable: %v", err)
	}
	// S1 must be the production constant.
	if decompS1SQL() != realtimeAuthorizeSelectOfferSQLSkip {
		t.Fatal("S1 must be byte-identical to realtimeAuthorizeSelectOfferSQLSkip")
	}
	// S2 must compose production candidates CTE and must not aggregate the book.
	s2 := decompS2SQL()
	if !strings.HasPrefix(s2, realtimeAuthorizeCandidatesCTE) {
		t.Fatal("S2 must start with production candidates CTE")
	}
	if strings.Contains(s2, "jsonb_agg") || strings.Contains(s2, "book AS") {
		t.Fatal("S2 must not include the considered-book aggregation")
	}
	// S3 eligible body identical; no window functions.
	s3 := decompS3SQL(eligibleBody)
	s3Eligible, err := extractEligibleCTEBody(s3)
	if err != nil {
		t.Fatalf("S3 eligible extract: %v", err)
	}
	if s3Eligible != eligibleBody {
		t.Fatalf("S3 eligible body drifted from production\nprod=%q\ns3=%q", eligibleBody, s3Eligible)
	}
	if strings.Contains(s3, "count(*) OVER") || strings.Contains(s3, "row_number() OVER") {
		t.Fatal("S3 must not use window functions")
	}
	if !strings.Contains(s3, "FOR UPDATE OF o SKIP LOCKED") {
		t.Fatal("S3 must still claim with SKIP LOCKED")
	}
	// Ranking keys in S3 must match production window ORDER BY.
	prodRank := `e.verified_outcome_cost ASC,
			         CASE e.warmth WHEN 'HOT' THEN 0 WHEN 'WARM' THEN 1 WHEN 'CACHED' THEN 2 ELSE 3 END,
			         e.available_sequences DESC, e.last_seen_at DESC, e.worker_id ASC`
	if !strings.Contains(realtimeAuthorizeCandidatesCTE, prodRank) {
		t.Fatal("production ranking keys moved; update realtimeDecompRankingOrderBy")
	}
	if !strings.Contains(s3, strings.TrimSpace(realtimeDecompRankingOrderBy)) {
		t.Fatal("S3 ranking keys drifted from production")
	}
	s4 := decompS4SQL(eligibleBody)
	s4Eligible, err := extractEligibleCTEBody(s4)
	if err != nil || s4Eligible != eligibleBody {
		t.Fatalf("S4 eligible body drift: err=%v", err)
	}

	loadBefore, _ := readLoadAverage()
	startedAt := time.Now().UTC()
	commit := mercSourceCommitSHA()
	short := commit
	if i := strings.IndexByte(short, '-'); i > 0 {
		short = short[:i]
	}
	if len(short) > 12 {
		short = short[:12]
	}

	report := decompReport{
		SchemaVersion:    1,
		Kind:             "realtime_selection_cost_decomposition",
		SourceCommit:     commit,
		GeneratedAt:      startedAt.Format(time.RFC3339),
		NumCPU:           runtime.NumCPU(),
		LoadAverageStart: loadBefore,
		SamplesPerCell:   samples,
		WarmupPerCell:    realtimeDecompWarmupN,
		Ladder:           append([]int(nil), ladder...),
		Sizes:            map[string]decompSizeCell{},
		DerivedRatios:    map[string]decompRatios{},
		Honesty: decompHonesty{
			WhatThisProves:       "relative cost of precount / jsonb_agg / window-sort / eligibility filter inside the production authorize SQL on one host against a synthetic live book",
			WhatThisDoesNotProve: "fleet SLOs, absolute latency under quiet-host conditions when load_contaminated, or that S2/S3 select the same worker as S1",
			S2S3AreCostProbes:    "S2 and S3 are measurement variants that live only in this test file. They execute an UPDATE under ROLLBACK so the book is not drained. They will not select the same worker as S1 under concurrent state; rank identity was not proven.",
			LoadNote:             "If 1-minute load average exceeds 8 at any sample, absolute_quotable is false. RATIO decomposition between S0..S4 remains reportable.",
		},
		Notes: []string{
			"S0_before = historical unbounded COUNT(*); S0_after = production LIMIT-2 branch probe",
			"Heartbeat refreshes last_seen_at at the production 15s cadence so the 45s liveness window does not go dark mid-ladder",
			"Each S1/S2/S3 repetition runs inside its own transaction and ROLLs BACK",
		},
	}

	loadContaminated := loadExceedsCeiling(loadBefore)
	store, pool := openSelectorScaleStore(t, 16)
	ctx := context.Background()
	profile := sortedVLLMProfiles()[0]

	// Statement catalogue in measurement order.
	type stmtDef struct {
		id, desc string
		sql      string
		update   bool
		// argStyle: "precount" uses 4 args; "claim" uses 5 (includes min samples).
		argStyle string
		// scan: how many destinations (0 = Exec via QueryRow discarding).
		// For timing we only care that rows return / no error for claims.
	}
	stmts := []stmtDef{
		{id: "S0_before", desc: "unbounded precount (historical)", sql: realtimeOfferBookUnboundedCountSQL, update: false, argStyle: "precount"},
		{id: "S0_after", desc: "bounded LIMIT-2 branch probe (production)", sql: realtimeOfferBookBranchProbeSQL, update: false, argStyle: "precount"},
		{id: "S1", desc: "production realtimeAuthorizeSelectOfferSQLSkip", sql: decompS1SQL(), update: true, argStyle: "claim"},
		{id: "S2", desc: "S1 without book CTE / considered column", sql: s2, update: true, argStyle: "claim"},
		{id: "S3", desc: "S2 without window sort; ORDER BY rank keys LIMIT 1", sql: s3, update: true, argStyle: "claim"},
		{id: "S4", desc: "count(*) over eligible CTE alone", sql: s4, update: false, argStyle: "claim_count"},
	}

	for _, size := range ladder {
		t.Logf("=== book size %d ===", size)
		if err := wipeSelectorScaleTables(ctx, pool); err != nil {
			t.Logf("wipe (non-fatal): %v", err)
		}
		if err := seedSelectorRealtimeBook(ctx, pool, profile, size, realtimeDecompSeed); err != nil {
			t.Fatalf("seed %d: %v", size, err)
		}
		if _, err := pool.Exec(ctx, `ANALYZE realtime_worker_offers; ANALYZE suppliers`); err != nil {
			t.Fatalf("analyze: %v", err)
		}
		// Initial full liveness stamp so the first samples see a live book even
		// if staggered seeds sit near the edge of the window.
		if _, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers SET last_seen_at=now(), status='ACTIVE'`); err != nil {
			t.Fatalf("initial liveness: %v", err)
		}

		// Heartbeat: same mechanism as selector_scale_curve_test.go.
		var beats atomic.Int64
		var beatNanos atomic.Int64
		stopBeat := make(chan struct{})
		var beatWG sync.WaitGroup
		beatWG.Add(1)
		go func() {
			defer beatWG.Done()
			tick := time.NewTicker(selectorScaleHeartbeatInterval)
			defer tick.Stop()
			for {
				select {
				case <-stopBeat:
					return
				case <-tick.C:
					start := time.Now()
					if err := refreshSelectorScaleLiveness(ctx, pool, profile, ""); err != nil {
						continue
					}
					took := time.Since(start).Nanoseconds()
					beats.Add(1)
					beatNanos.Add(took)
				}
			}
		}()

		live, err := countLiveEligible(ctx, pool, profile)
		if err != nil {
			close(stopBeat)
			beatWG.Wait()
			t.Fatalf("live eligible count: %v", err)
		}
		cell := decompSizeCell{
			BookSize:                  size,
			LiveEligibleAtMeasurement: live,
			Statements:                map[string]decompStmtResult{},
		}
		if live == 0 {
			cell.NoLiveCandidatesAtMeasurement = true
			t.Logf("REFUSE size=%d: no_live_candidates_at_measurement (axis claims %d)", size, size)
			// Still record the empty cell; do not time statements as cost.
			for _, st := range stmts {
				cell.Statements[st.id] = decompStmtResult{
					StatementID:      st.id,
					Description:      st.desc,
					SQLDigest:        sqlTextDigest(st.sql),
					ExecutesUpdate:   st.update,
					AllSamplesFailed: true,
					FirstError:       "no_live_candidates_at_measurement",
				}
			}
			close(stopBeat)
			beatWG.Wait()
			cell.HeartbeatSweeps = int(beats.Load())
			report.Sizes[strconv.Itoa(size)] = cell
			continue
		}
		if live < size {
			t.Logf("WARN size=%d: live eligible=%d < axis (partial book still measured)", size, live)
		}

		for _, st := range stmts {
			// Refresh liveness before each statement series so dark-book
			// contamination cannot masquerade as a fast cell.
			if _, err := pool.Exec(ctx, `
				UPDATE realtime_worker_offers
				   SET last_seen_at=now(), status='ACTIVE',
				       available_sequences=GREATEST(available_sequences, max_active_sequences)`); err != nil {
				t.Fatalf("refresh before %s: %v", st.id, err)
			}
			// Re-check load mid-run.
			if la, _ := readLoadAverage(); loadExceedsCeiling(la) {
				loadContaminated = true
			}

			res := measureDecompStatement(t, ctx, pool, profile, st.id, st.desc, st.sql, st.argStyle, st.update, samples)
			cell.Statements[st.id] = res
			t.Logf("size=%d %s p50=%v failed=%v unstable=%v",
				size, st.id, fmtP50(res), res.AllSamplesFailed, res.UnstableSamplePopulation)
		}

		// One EXPLAIN per mutating statement already captured inside measure.
		close(stopBeat)
		beatWG.Wait()
		cell.HeartbeatSweeps = int(beats.Load())
		if n := beats.Load(); n > 0 {
			cell.HeartbeatMeanMS = float64(beatNanos.Load()) / float64(n) / float64(time.Millisecond)
		}
		// Final live check for honesty.
		if liveEnd, err := countLiveEligible(ctx, pool, profile); err == nil {
			cell.LiveEligibleAtMeasurement = liveEnd
			if liveEnd == 0 {
				cell.NoLiveCandidatesAtMeasurement = true
			}
		}
		report.Sizes[strconv.Itoa(size)] = cell
		report.DerivedRatios[strconv.Itoa(size)] = deriveDecompRatios(size, cell)
		_ = store
	}

	loadAfter, _ := readLoadAverage()
	if loadExceedsCeiling(loadAfter) {
		loadContaminated = true
	}
	report.LoadAverageEnd = loadAfter
	report.LoadContaminated = loadContaminated
	report.AbsoluteQuotable = !loadContaminated
	if loadContaminated {
		report.Notes = append(report.Notes,
			"load_contaminated=true: absolute milliseconds are NOT quotable; RATIO decomposition between S0..S4 remains reportable")
	}
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	report.WallClockSeconds = time.Since(startedAt).Seconds()

	// Top-K projection at 100k when S1/S2/S3/S4 all have p50s.
	if cell, ok := report.Sizes["100000"]; ok {
		report.ProjectionTopK = projectTopK(cell, report.DerivedRatios["100000"])
	}

	path, err := writeDecompEvidence(report, short)
	if err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s wall=%.1fs load_contaminated=%v absolute_quotable=%v",
		path, report.WallClockSeconds, report.LoadContaminated, report.AbsoluteQuotable)
}

func measureDecompStatement(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile,
	id, desc, sql, argStyle string, doesUpdate bool, samples int,
) decompStmtResult {
	t.Helper()
	out := decompStmtResult{
		StatementID:    id,
		Description:    desc,
		SQLDigest:      sqlTextDigest(sql),
		ExecutesUpdate: doesUpdate,
		WarmupN:        realtimeDecompWarmupN,
		SampleN:        samples,
	}
	args := decompArgs(profile, argStyle)

	runOnce := func() (time.Duration, error) {
		start := time.Now()
		var err error
		if doesUpdate {
			// Each repetition in its own transaction, then ROLL BACK so the
			// book is not drained and samples stay independent.
			tx, txErr := pool.Begin(ctx)
			if txErr != nil {
				return 0, txErr
			}
			// Intentionally always roll back — never commit a claim.
			defer tx.Rollback(ctx)
			err = execDecompSQL(ctx, tx, sql, argStyle, args)
			// Force statement completion before timing ends; rollback after.
			_ = time.Since(start)
			if err != nil {
				return time.Since(start), err
			}
			// Rollback is part of harness accounting, not the measured path;
			// time only the statement.
			return time.Since(start), nil
		}
		err = execDecompSQL(ctx, pool, sql, argStyle, args)
		return time.Since(start), err
	}

	var warmupMS []float64
	var sampleMS []float64
	var successMS []float64
	var emptyDark, otherFail int
	var firstErr string
	noteErr := func(e error) {
		if firstErr == "" && e != nil {
			firstErr = e.Error()
		}
	}

	total := realtimeDecompWarmupN + samples
	for i := 0; i < total; i++ {
		// Soft liveness bump every few samples so a long S1 series at 100k
		// cannot outrun the 45s window even if the heartbeat goroutine is
		// contending.
		if i > 0 && i%5 == 0 {
			_, _ = pool.Exec(ctx, `
				UPDATE realtime_worker_offers
				   SET last_seen_at=now()
				 WHERE last_seen_at < now() - interval '20 seconds'`)
		}
		d, err := runOnce()
		ms := float64(d) / float64(time.Millisecond)
		isWarm := i < realtimeDecompWarmupN
		if isWarm {
			warmupMS = append(warmupMS, ms)
		} else {
			sampleMS = append(sampleMS, ms)
		}
		if err != nil {
			noteErr(err)
			if errors.Is(err, pgx.ErrNoRows) || strings.Contains(err.Error(), "no rows") {
				emptyDark++
			} else {
				otherFail++
			}
			continue
		}
		if !isWarm {
			successMS = append(successMS, ms)
		}
	}
	out.WarmupMS = warmupMS
	out.SampleMS = sampleMS
	out.TimedSuccessN = len(successMS)
	out.EmptyOrDarkBookSamples = emptyDark
	out.OtherFailures = otherFail
	out.FirstError = firstErr

	// Honesty: S1 empty book → all_samples_failed, do not report as cost.
	// Apply the same rule to any claim statement with zero successes.
	if len(successMS) == 0 {
		out.AllSamplesFailed = true
		// Still capture EXPLAIN if possible for diagnosis.
		out.ExplainJSON, out.ExplainNote = decompExplain(ctx, pool, sql, argStyle, args, doesUpdate)
		return out
	}

	sorted := append([]float64(nil), successMS...)
	sort.Float64s(sorted)
	p50 := percentileSorted(sorted, 0.50)
	p95 := percentileSorted(sorted, 0.95)
	p99 := percentileSorted(sorted, 0.99)
	maxV := sorted[len(sorted)-1]
	minV := sorted[0]
	mean := meanFloats(successMS)
	out.P50MS = &p50
	out.P95MS = &p95
	out.P99MS = &p99
	out.MaxMS = &maxV
	out.MinMS = &minV
	out.MeanMS = &mean
	out.FiveNumber = &decompFiveNumber{
		Min: minV,
		Q1:  percentileSorted(sorted, 0.25),
		Med: p50,
		Q3:  percentileSorted(sorted, 0.75),
		Max: maxV,
	}
	if p50 > 0 && p99 > realtimeDecompUnstableP99OverP50*p50 {
		out.UnstableSamplePopulation = true
		// Keep five-number; clear quotable point estimates? Contract says:
		// "set unstable_sample_population and emit the five-number summary —
		// a bimodal cell has no quotable p50". We keep p50 in the file but
		// the flag marks it unusable for quoting.
	}

	out.ExplainJSON, out.ExplainNote = decompExplain(ctx, pool, sql, argStyle, args, doesUpdate)
	return out
}

func decompArgs(profile VLLMRuntimeProfile, argStyle string) []any {
	switch argStyle {
	case "precount":
		return []any{
			profile.RuntimeProfileID, profile.ProfileSHA256,
			profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
		}
	case "claim", "claim_count":
		return []any{
			profile.RuntimeProfileID, profile.ProfileSHA256,
			profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
			minRealtimeOutcomeSamples,
		}
	default:
		return nil
	}
}

// execDecompSQL runs the statement. For claim forms we QueryRow and scan what
// we can; ErrNoRows means empty/dark book.
func execDecompSQL(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql, argStyle string, args []any) error {
	switch argStyle {
	case "precount", "claim_count":
		var n int
		return q.QueryRow(ctx, sql, args...).Scan(&n)
	case "claim":
		// Production S1 returns considered JSON; S2/S3 do not. Scan into a
		// catch-all via raw values so both shapes work.
		rows := 0
		// Use a generic approach: select into a single dummy by wrapping? No —
		// run as Query and count rows / drain.
		// pgx.Row can't drain multi-col generically easily; use Query.
		return execDecompClaim(ctx, q, sql, args, &rows)
	default:
		return fmt.Errorf("unknown argStyle %q", argStyle)
	}
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func execDecompClaim(ctx context.Context, q queryRower, sql string, args []any, rows *int) error {
	// Prefer Query when available (pool and tx both support it).
	if qq, ok := q.(querier); ok {
		rs, err := qq.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		n := 0
		for rs.Next() {
			// Drain values without caring about types — cost probe only.
			vals, err := rs.Values()
			if err != nil {
				return err
			}
			_ = vals
			n++
		}
		if err := rs.Err(); err != nil {
			return err
		}
		*rows = n
		if n == 0 {
			return pgx.ErrNoRows
		}
		return nil
	}
	// Fallback: try scanning the first few columns used by S2/S3.
	var workerID, supplierID any
	var baseURL, sealed, placement, placementSHA, warmth any
	var inRate, outRate any
	var candCount, selRank, tA, tF, vS, rC any
	err := q.QueryRow(ctx, sql, args...).Scan(
		&workerID, &supplierID, &baseURL, &sealed, &inRate, &outRate,
		&placement, &placementSHA, &warmth, &candCount, &selRank, &tA, &tF, &vS, &rC)
	if err != nil {
		return err
	}
	*rows = 1
	return nil
}

func decompExplain(ctx context.Context, pool *pgxpool.Pool, sql, argStyle string, args []any, doesUpdate bool) (json.RawMessage, string) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, "begin failed: " + err.Error()
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '300s'`); err != nil {
		return nil, "statement_timeout: " + err.Error()
	}
	prefix := "EXPLAIN (ANALYZE, BUFFERS, TIMING ON, FORMAT JSON) "
	rows, err := tx.Query(ctx, prefix+sql, args...)
	if err != nil {
		return nil, "explain failed: " + err.Error()
	}
	defer rows.Close()
	var raw []byte
	if rows.Next() {
		// FORMAT JSON returns one row with a json/jsonb value.
		var plan any
		if err := rows.Scan(&plan); err != nil {
			return nil, "scan explain: " + err.Error()
		}
		switch v := plan.(type) {
		case []byte:
			raw = v
		case string:
			raw = []byte(v)
		default:
			b, mErr := json.Marshal(v)
			if mErr != nil {
				return nil, "marshal explain: " + mErr.Error()
			}
			raw = b
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "explain rows: " + err.Error()
	}
	// Always roll back so ANALYZE side effects (UPDATE) do not drain the book.
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return raw, "rollback after explain: " + err.Error()
	}
	if len(raw) == 0 {
		return nil, "empty explain"
	}
	_ = doesUpdate
	_ = argStyle
	return json.RawMessage(raw), "EXPLAIN (ANALYZE, BUFFERS, TIMING ON, FORMAT JSON) under ROLLBACK"
}

func countLiveEligible(ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile) (int, error) {
	var n int
	err := pool.QueryRow(ctx, realtimeOfferBookUnboundedCountSQL,
		profile.RuntimeProfileID, profile.ProfileSHA256,
		profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
	).Scan(&n)
	return n, err
}

func deriveDecompRatios(size int, cell decompSizeCell) decompRatios {
	r := decompRatios{BookSize: size}
	if cell.NoLiveCandidatesAtMeasurement {
		r.Notes = append(r.Notes, "no_live_candidates_at_measurement")
		return r
	}
	p50 := func(id string) *float64 {
		st, ok := cell.Statements[id]
		if !ok || st.AllSamplesFailed || st.P50MS == nil {
			return nil
		}
		if st.UnstableSamplePopulation {
			r.Notes = append(r.Notes, id+":unstable_sample_population")
		}
		return st.P50MS
	}
	s0b, s0a := p50("S0_before"), p50("S0_after")
	s1, s2, s3, s4 := p50("S1"), p50("S2"), p50("S3"), p50("S4")
	r.S0BeforeP50MS, r.S0AfterP50MS = s0b, s0a
	r.S1P50MS, r.S2P50MS, r.S3P50MS, r.S4P50MS = s1, s2, s3, s4
	if s0b != nil && s0a != nil && *s0b > 0 {
		v := *s0a / *s0b
		r.S0AfterOverBefore = &v
	}
	if s1 != nil && *s1 > 0 {
		if s2 != nil {
			d := *s1 - *s2
			r.JsonbAggMS = &d
			f := d / *s1
			r.JsonbAggFractionOfS1 = &f
		}
		if s2 != nil && s3 != nil {
			d := *s2 - *s3
			r.WindowSortMS = &d
			f := d / *s1
			r.WindowSortFractionOfS1 = &f
		}
		if s4 != nil {
			f := *s4 / *s1
			r.EligibilityFloorFractionOfS1 = &f
		}
	}
	return r
}

func projectTopK(cell decompSizeCell, ratios decompRatios) *decompTopKProjection {
	// Arithmetic projection if the considered book were bounded to top K plus
	// a count. ONLY the jsonb_agg term (S1−S2) scales with considered size.
	// S3 already drops the window functions and the book CTE; EXPLAIN shows
	// S3 still does a full external-merge sort under ORDER BY … FOR UPDATE …
	// LIMIT 1, so bounding `considered` does NOT remove the pick-path sort.
	// Projected S1 ≈ S3 + jsonb_agg_full*(K/N).
	const k = 32
	const n = 100_000
	if ratios.S1P50MS == nil || ratios.S2P50MS == nil || ratios.S3P50MS == nil {
		return nil
	}
	jsonbFull := *ratios.S1P50MS - *ratios.S2P50MS
	if jsonbFull < 0 {
		jsonbFull = 0
	}
	jsonbK := jsonbFull * (float64(k) / float64(n))
	projected := *ratios.S3P50MS + jsonbK
	residual := *ratios.S3P50MS
	if ratios.S4P50MS != nil {
		residual = *ratios.S3P50MS - *ratios.S4P50MS
	}
	s4txt := "n/a"
	if ratios.S4P50MS != nil {
		s4txt = fmt.Sprintf("%.3f ms", *ratios.S4P50MS)
	}
	working := fmt.Sprintf(
		"At N=%d, S1_p50=%.3f ms, S2_p50=%.3f ms, S3_p50=%.3f ms, S4_p50=%s. "+
			"jsonb_agg_full = S1-S2 = %.3f ms (%.1f%% of S1). "+
			"window_funcs = S2-S3 = %.3f ms. "+
			"pick_path residual S3-S4 = %.3f ms (%.1f%% of S1) — dominant; EXPLAIN shows Sort Method: external merge under LockRows even for LIMIT 1 because FOR UPDATE prevents top-N heapsort. "+
			"If considered is top-K=%d only, jsonb_agg_K ≈ jsonb_agg_full*(K/N) = %.3f*(%d/%d) = %.3f ms. "+
			"Projected S1 ≈ S3 + jsonb_agg_K = %.3f + %.3f = %.3f ms. "+
			"Bounding considered alone cannot reach the 10 ms target while the FOR UPDATE pick re-joins and fully sorts the eligible book.",
		n, *ratios.S1P50MS, *ratios.S2P50MS, *ratios.S3P50MS, s4txt,
		jsonbFull, 100*jsonbFull / *ratios.S1P50MS,
		*ratios.S2P50MS-*ratios.S3P50MS,
		residual, 100*residual / *ratios.S1P50MS,
		k, jsonbFull, k, n, jsonbK,
		*ratios.S3P50MS, jsonbK, projected,
	)
	return &decompTopKProjection{
		AtBookSize:       n,
		AssumedTopK:      k,
		Working:          working,
		ProjectedS1P50MS: projected,
		Caveat:           "S2/S3 are cost probes only — rank identity not proven. jsonb_agg linearity in K is assumed, not measured. Absolute ms not quotable when load_contaminated. The 10ms target is NOT reachable by bounding considered alone on this evidence.",
	}
}

func writeDecompEvidence(report decompReport, shortSHA string) (string, error) {
	rel := fmt.Sprintf("evidence/perf/realtime-selection-decomposition-%s.json", shortSHA)
	path := filepath.Join("..", "..", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

func realtimeDecompLadder(t *testing.T) []int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(realtimeDecompLadderEnv))
	if raw == "" {
		return append([]int(nil), realtimeDecompDefaultLadder...)
	}
	var out []int
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q: %q is not a positive size", realtimeDecompLadderEnv, raw, field)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return append([]int(nil), realtimeDecompDefaultLadder...)
	}
	return out
}

func realtimeDecompSamples(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(realtimeDecompSamplesEnv))
	if raw == "" {
		return realtimeDecompDefaultN
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q is not a positive sample count", realtimeDecompSamplesEnv, raw)
	}
	return n
}

func loadExceedsCeiling(loads []float64) bool {
	if len(loads) == 0 {
		return false
	}
	return loads[0] > realtimeDecompLoadCeiling
}

func percentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func meanFloats(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func fmtP50(r decompStmtResult) string {
	if r.AllSamplesFailed {
		return "ALL_FAILED"
	}
	if r.P50MS == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3fms", *r.P50MS)
}
